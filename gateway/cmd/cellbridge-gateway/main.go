package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/api"
	"github.com/cellbridge/cellbridge/gateway/internal/auth"
	"github.com/cellbridge/cellbridge/gateway/internal/call"
	"github.com/cellbridge/cellbridge/gateway/internal/config"
	"github.com/cellbridge/cellbridge/gateway/internal/db"
	pocketdiscovery "github.com/cellbridge/cellbridge/gateway/internal/discovery"
	"github.com/cellbridge/cellbridge/gateway/internal/eventbus"
	"github.com/cellbridge/cellbridge/gateway/internal/modem"
	"github.com/cellbridge/cellbridge/gateway/internal/modem/at"
	"github.com/cellbridge/cellbridge/gateway/internal/modem/discovery"
	"github.com/cellbridge/cellbridge/gateway/internal/push"
	"github.com/cellbridge/cellbridge/gateway/internal/recording"
	"github.com/cellbridge/cellbridge/gateway/internal/sms"
	"github.com/cellbridge/cellbridge/gateway/internal/sip"
	"github.com/cellbridge/cellbridge/gateway/internal/voice"
	qdc507voice "github.com/cellbridge/cellbridge/gateway/internal/voice/qdc507"
	"github.com/cellbridge/cellbridge/gateway/internal/webrtc"
)

func main() {
	configPath := flag.String("config", "", "YAML configuration path")
	listen := flag.String("listen", "", "control-plane listen address override")
	id := flag.String("gateway-id", "gw-dev", "gateway identifier")
	name := flag.String("name", "CellBridge Gateway", "gateway display name")
	version := flag.String("version", "dev", "reported gateway version")
	dataDir := flag.String("data-dir", "", "gateway data directory override")
	flag.Parse()
	settings := config.Default()
	if *configPath != "" {
		loaded, err := config.Load(*configPath)
		if err != nil {
			slog.Error("load configuration", "error", err)
			os.Exit(1)
		}
		settings = loaded
	}
	if *listen != "" {
		settings.Server.Listen = *listen
	}
	if *dataDir != "" {
		settings.Data.Dir = *dataDir
	}
	if err := settings.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	turnTTL, err := time.ParseDuration(settings.WebRTC.TurnCredentialTTL)
	if err != nil || turnTTL <= 0 {
		slog.Error("invalid TURN credential TTL", "value", settings.WebRTC.TurnCredentialTTL)
		os.Exit(1)
	}
	if err := os.MkdirAll(settings.Data.Dir, 0o750); err != nil {
		slog.Error("create data directory", "error", err)
		os.Exit(1)
	}
	database, err := db.Open(filepath.Join(settings.Data.Dir, "cellbridge.sqlite"))
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		slog.Error("migrate database", "error", err)
		os.Exit(1)
	}
	if err := database.EnsureRecordingSettings(context.Background(), db.RecordingSettings{
		Enabled:          settings.Recording.Enabled,
		AutoMode:         settings.Recording.AutoMode,
		RetentionDays:    settings.Recording.RetentionDays,
		MinimumFreeBytes: settings.Recording.MinimumFreeBytes,
	}); err != nil {
		slog.Error("initialize recording settings", "error", err)
		os.Exit(1)
	}
	authentication := auth.NewService(*id, database)
	application := api.NewServer(*id, *name, *version)
	turnSecret := os.Getenv("CELLBRIDGE_TURN_AUTH_SECRET")
	if turnSecret == "" {
		turnSecret = os.Getenv("TURN_AUTH_SECRET")
	}
	application.NetworkMode = settings.Network.Mode
	application.NetworkTransport = settings.Network.Transport
	application.TailnetHostname = settings.Network.TailnetHostname
	application.PrivateTURN = api.PrivateTURNConfig{
		Enabled:       settings.WebRTC.PrivateTURN.Enabled,
		Host:          settings.WebRTC.PrivateTURN.Host,
		Port:          settings.WebRTC.PrivateTURN.Port,
		CredentialTTL: turnTTL,
		Secret:        []byte(turnSecret),
	}
	application.Database = database
	application.Auth = authentication
	application.Fingerprint = "sha256:pending-gateway-certificate"
	application.Events = eventbus.New()
	application.CallControl = call.NewController()
	application.SetCapabilities(modem.Capabilities{Tier: modem.CapabilityUnknown})
	if settings.Push.Mode == "broker" && strings.TrimSpace(settings.Push.BrokerURL) != "" {
		brokerURL := strings.TrimRight(strings.TrimSpace(settings.Push.BrokerURL), "/")
		if strings.EqualFold(brokerURL, "https://push.example.com") {
			slog.Warn("push sender unavailable", "error_code", "BLOCKED_EXTERNAL_APNS", "reason", "push broker URL is a placeholder")
		} else {
			application.PushSender = push.NewBroker(brokerURL, os.Getenv("CELLBRIDGE_PUSH_BROKER_AUTHORIZATION"))
			slog.Info("push sender configured", "mode", "broker", "endpoint", brokerURL)
		}
	} else {
		slog.Warn("push sender unavailable", "error_code", "BLOCKED_EXTERNAL_APNS", "reason", "APNs provider or push broker is not configured")
	}
	if rtc, rtcErr := webrtc.NewEngine(); rtcErr != nil {
		slog.Error("initialize WebRTC", "error", rtcErr)
	} else {
		application.WebRTC = rtc
	}
	shutdownContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if settings.Recording.Enabled {
		recorder, recordingErr := recording.NewManager(database, filepath.Join(settings.Data.Dir, "recordings"), settings.Recording.RetentionDays)
		if recordingErr != nil {
			slog.Error("initialize recording service", "error", recordingErr)
			os.Exit(1)
		}
		recorder.SetMinimumFreeBytes(settings.Recording.MinimumFreeBytes)
		recorder.SetEventHandler(func(kind string, item db.Recording) {
			if application.Events != nil {
				application.Events.Publish(item.SyncSeq, kind, recordingEventPayload(item))
			}
		})
		application.Recordings = recorder
		application.SetCapabilities(modem.Capabilities{Tier: modem.CapabilityUnknown, Recording: &modem.RecordingCapabilities{Supported: true, Manual: true, Auto: false, Format: "m4a-aac"}})
		if recoveryErr := recorder.Recover(context.Background()); recoveryErr != nil {
			slog.Warn("recover recording work directories", "error", recoveryErr)
		}
		go recorder.RunRetention(shutdownContext)
	}

	serialAdapter, serialErr := openConfiguredModem(settings.Modem.TTY, settings.Modem.Baud)
	if serialErr != nil {
		if settings.Modem.TTY == "auto" {
			slog.Warn("no usable modem found; gateway will run without cellular I/O", "error", serialErr)
		} else {
			slog.Error("open configured modem", "error", serialErr)
			os.Exit(1)
		}
	}
	if serialAdapter != nil {
		defer serialAdapter.Close()
		capabilities := modem.Capabilities{Tier: modem.CapabilitySMSOnly, SMS: true}
		if application.Recordings != nil {
			capabilities.Recording = &modem.RecordingCapabilities{Supported: true, Manual: true, Auto: false, Format: "m4a-aac"}
		}
		probeContext, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if probed, probeErr := serialAdapter.Probe(probeContext); probeErr != nil {
			slog.Warn("probe modem capabilities", "error", probeErr)
		} else {
			capabilities = probed
		}
		probeCancel()
		if application.Recordings != nil {
			capabilities.Recording = &modem.RecordingCapabilities{Supported: true, Manual: true, Auto: false, Format: "m4a-aac"}
		}
		var voiceAudio modem.VoiceAudio
		var voiceAudioCapabilities modem.AudioCapabilities
		if settings.Voice.Enabled && (settings.Voice.Backend == "raw-pcm" || settings.Voice.Backend == "alsa") {
			if settings.Voice.Backend == "alsa" {
				voiceAudio, err = voice.OpenALSA(settings.Voice.RXPath, settings.Voice.TXPath)
			} else {
				voiceAudio, err = voice.OpenRawPCM(settings.Voice.RXPath, settings.Voice.TXPath)
			}
			if err != nil {
				slog.Warn("open voice audio backend; voice remains unavailable", "backend", settings.Voice.Backend, "error", err)
			} else {
				probeContext, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
				if probedAudio, probeErr := voiceAudio.Probe(probeContext); probeErr != nil {
					err = probeErr
					_ = voiceAudio.Close()
					slog.Warn("probe voice audio backend; voice remains unavailable", "backend", settings.Voice.Backend, "error", probeErr)
				} else {
					voiceAudioCapabilities = probedAudio
					application.VoiceAudio = voiceAudio
					defer voiceAudio.Close()
				}
				probeCancel()
			}
		}
		if settings.Voice.Enabled && settings.Voice.Backend == "qdc507" {
			hostAudio, audioErr := voice.OpenALSA(settings.Voice.RXPath, settings.Voice.TXPath)
			if audioErr != nil {
				slog.Warn("open QDC507 host ALSA backend; voice remains unavailable", "error", audioErr)
			} else {
				voiceAudio, audioErr = qdc507voice.Open(hostAudio, qdc507voice.Config{
					TTY:        settings.Modem.TTY,
					RuntimeDir: settings.Voice.RuntimeDir,
					Bootstrap:  settings.Voice.Bootstrap,
				})
				if audioErr == nil {
					// Cold start of the private adb daemon + module USB
					// re-enumeration can take >10s after a NAS reboot;
					// give the probe enough room (transportID retries
					// internally). Observed 2026-09-06: 8s was too short
					// and voice degraded to control-only at boot.
					probeContext, probeCancel := context.WithTimeout(context.Background(), 30*time.Second)
					voiceAudioCapabilities, audioErr = voiceAudio.Probe(probeContext)
					probeCancel()
				}
				if audioErr != nil {
					slog.Warn("probe QDC507 voice backend; voice remains control-only", "error", audioErr)
					if voiceAudio != nil {
						_ = voiceAudio.Close()
					}
				} else {
					application.VoiceAudio = voiceAudio
					defer voiceAudio.Close()
				}
			}
		}
		if application.VoiceAudio != nil {
			capabilities.Tier = modem.CapabilityFullVoice
			capabilities.Voice = true
			capabilities.DTMF = true
			if voiceAudioCapabilities.Backend == "" {
				voiceAudioCapabilities = modem.AudioCapabilities{Backend: settings.Voice.Backend, SampleRate: settings.Voice.SampleRate, Channels: 1}
			}
			capabilities.Audio = &voiceAudioCapabilities
		} else if capabilities.Voice {
			capabilities.Tier = modem.CapabilityVoiceControlOnly
		} else if capabilities.SMS {
			capabilities.Tier = modem.CapabilitySMSOnly
		}
		application.SetCapabilities(capabilities)
		application.SMSEngine = sms.NewEngine(database, serialAdapter)
		go func() {
			if runErr := application.SMSEngine.Run(shutdownContext, 5*time.Second, application.HandleMessage); runErr != nil && runErr != context.Canceled {
				slog.Warn("SMS inbox reader stopped", "error", runErr)
			}
		}()
		callModem := modem.NewActiveCallAdapter(serialAdapter)
		defer callModem.Close()
		application.CallModem = callModem
		go func() {
			for event := range callModem.Events() {
				application.HandleModemEvent(event)
			}
		}()
		statusContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if status, statusErr := serialAdapter.Status(statusContext); statusErr != nil {
			slog.Warn("read modem status", "error", statusErr)
		} else {
			if capabilities.Tier == modem.CapabilitySMSOnly {
				status.Voice = "unavailable"
			} else if application.VoiceAudio != nil {
				status.Voice = "ready"
			} else {
				// A serial AT adapter alone is voice-control-only. A certified
				// PCM/UAC backend must promote this to ready before WebRTC audio.
				status.Voice = "control_only"
			}
			status.CapabilityTier = capabilities.Tier
			application.SetLineStatus(status)
		}
		cancel()
		go func() {
			if runErr := serialAdapter.Run(shutdownContext); runErr != nil && runErr != context.Canceled {
				slog.Warn("modem URC reader stopped", "error", runErr)
			}
		}()
		if settings.SIP.Enabled {
			registrar := sip.NewRegistrar()
			sipAuth := sip.NewAuth(settings.SIP.Realm)
			for _, u := range settings.SIP.Users {
				sipAuth.AddUser(u.Username, u.Password)
			}
			sipServer := sip.NewServer(settings.SIP.Listen, registrar, sipAuth, callModem, application.VoiceAudio)
			application.YakPushToken = settings.SIP.PushToken
			sipServer.AttachEvents(callModem.Events(), settings.SIP.PushToken)
			if application.SMSEngine != nil {
				sipServer.AttachSMS(func(ctx context.Context, to, body string) error {
					_, err := application.SMSEngine.Send(ctx, to, body)
					return err
				})
			}
			if err := sipServer.Start(shutdownContext); err != nil {
				slog.Error("start SIP server", "error", err)
			} else {
				defer sipServer.Stop(context.Background())
				slog.Info("SIP gateway listening", "addr", settings.SIP.Listen)
			}
		}
	}

server := &http.Server{
		Addr:              settings.Server.Listen,
		Handler:           application.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	var pocketAdvertiser *pocketdiscovery.Advertiser
	if settings.Network.Mode == "pocket" {
		pocketAdvertiser, err = pocketdiscovery.RegisterPocket(*name, *id, settings.Network.Transport, settings.Server.Listen)
		if err != nil {
			slog.Warn("PocketBridge discovery unavailable", "error", err, "error_code", "POCKET_DISCOVERY_UNAVAILABLE")
		} else {
			defer pocketAdvertiser.Shutdown()
			slog.Info("PocketBridge discovery ready", "service", "_cellbridge._tcp", "transport", settings.Network.Transport)
		}
	}
	go func() {
		<-shutdownContext.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	slog.Info("cellbridge gateway listening", "address", settings.Server.Listen, "gateway_id", *id)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func recordingEventPayload(item db.Recording) map[string]any {
	value := map[string]any{"id": item.ID, "callId": item.CallID, "state": item.State}
	if item.DurationMs != nil {
		value["durationMs"] = *item.DurationMs
	}
	if item.SizeBytes != nil {
		value["sizeBytes"] = *item.SizeBytes
	}
	return value
}

func openConfiguredModem(configuredPath string, baud int) (*at.Adapter, error) {
	paths := []string{configuredPath}
	if configuredPath == "" || configuredPath == "auto" {
		candidates, err := discovery.Enumerate("/dev")
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("no serial modem candidates under /dev")
		}
		paths = make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			paths = append(paths, candidate.Path)
		}
	}
	var failures []string
	for _, path := range paths {
		openContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		client, err := at.OpenSerial(openContext, path, baud)
		cancel()
		if err == nil {
			adapter := at.NewAdapter(client)
			// SMS dry-run by default: submissions use AT+CMGW (store
			// only, never delivered, costs nothing). Set
			// CELLBRIDGE_SMS_DRY_RUN=false to actually send.
			dryRun := !strings.EqualFold(strings.TrimSpace(os.Getenv("CELLBRIDGE_SMS_DRY_RUN")), "false")
			client.SetDryRun(dryRun)
			slog.Info("modem SMS dry-run", "enabled", dryRun)
			return adapter, nil
		}
		failures = append(failures, fmt.Sprintf("%s: %v", path, err))
	}
	return nil, fmt.Errorf("no usable modem found: %s", strings.Join(failures, "; "))
}
