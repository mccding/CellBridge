package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/auth"
	"github.com/cellbridge/cellbridge/gateway/internal/call"
	"github.com/cellbridge/cellbridge/gateway/internal/db"
	"github.com/cellbridge/cellbridge/gateway/internal/eventbus"
	"github.com/cellbridge/cellbridge/gateway/internal/modem"
	"github.com/cellbridge/cellbridge/gateway/internal/push"
	"github.com/cellbridge/cellbridge/gateway/internal/recording"
	"github.com/cellbridge/cellbridge/gateway/internal/sms"
	"github.com/cellbridge/cellbridge/gateway/internal/voice"
	"github.com/cellbridge/cellbridge/gateway/internal/webrtc"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	pion "github.com/pion/webrtc/v4"
)

type Server struct {
	GatewayID         string
	Name              string
	Version           string
	Capabilities      modem.Capabilities
	Line              modem.LineStatus
	Database          *db.DB
	Auth              *auth.Service
	Fingerprint       string
	Events            *eventbus.Bus
	SMSEngine         *sms.Engine
	CallControl       *call.Controller
	CallModem         CallModem
	WebRTC            *webrtc.Engine
	WebRTCTurnServers []pion.ICEServer
	NetworkMode       string
	NetworkTransport  string
	TailnetHostname   string
	PrivateTURN       PrivateTURNConfig
	VoiceAudio        modem.VoiceAudio
	Recordings        *recording.Manager
	PushSender        push.Sender
	YakPushToken      string // official YakPhone push token (push.yakteam.com)
	lineMu            sync.RWMutex
	webrtcMu          sync.Mutex
	webrtcCalls       map[string]*webrtc.Session
	voiceBridges      map[string]*voice.Bridge
	rateMu            sync.Mutex
	lastSMSSend       map[string]time.Time
}

type PrivateTURNConfig struct {
	Enabled       bool
	Host          string
	Port          int
	CredentialTTL time.Duration
	Secret        []byte
}

// CallModem is the narrow control surface exposed by the HTTP layer. The
// serial adapter owns AT details; API handlers only coordinate state and
// enforce authentication/idempotency.
type CallModem interface {
	Dial(context.Context, string) error
	Answer(context.Context) error
	Hangup(context.Context) error
	DTMF(context.Context, rune) error
}

func NewServer(gatewayID, name, version string) *Server {
	return &Server{
		GatewayID: gatewayID,
		Name:      name,
		Version:   version,
		Line: modem.LineStatus{
			SIM:            "unknown",
			Registration:   "unknown",
			CapabilityTier: modem.CapabilityUnknown,
			Voice:          "unavailable",
			SMS:            "unavailable",
		},
		webrtcCalls:  make(map[string]*webrtc.Session),
		voiceBridges: make(map[string]*voice.Bridge),
		lastSMSSend:  make(map[string]time.Time),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", method(http.MethodGet, s.health))
	mux.HandleFunc("/api/v1/identity", method(http.MethodGet, s.identity))
	mux.HandleFunc("/api/v1/gateway", method(http.MethodGet, s.gateway))
	mux.HandleFunc("/api/v1/line", method(http.MethodGet, s.line))
	mux.HandleFunc("/api/v1/pairing/start", method(http.MethodPost, s.pairingStart))
	mux.HandleFunc("/api/v1/pairing/complete", method(http.MethodPost, s.pairingComplete))
	mux.HandleFunc("/api/v1/auth/refresh", method(http.MethodPost, s.refresh))
	mux.HandleFunc("/api/v1/sync", method(http.MethodGet, s.sync))
	mux.HandleFunc("/api/v1/messages", s.messages)
	mux.HandleFunc("/api/v1/messages/", s.messageAction)
	mux.HandleFunc("/api/v1/threads", method(http.MethodGet, s.threads))
	mux.HandleFunc("/api/v1/calls", s.calls)
	mux.HandleFunc("/api/v1/calls/", s.callAction)
	mux.HandleFunc("/api/v1/settings/recording", s.recordingSettings)
	mux.HandleFunc("/api/v1/recordings", s.recordings)
	mux.HandleFunc("/api/v1/recordings/", s.recordingAction)
	mux.HandleFunc("/api/v1/devices/", s.deviceAction)
	mux.HandleFunc("/api/v1/events", s.events)
	return mux
}

type endpoint func(http.ResponseWriter, *http.Request)

func method(expected string, handler endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != expected {
			writeError(w, http.StatusMethodNotAllowed, "CB-API-001", "method not allowed")
			return
		}
		handler(w, r)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	s.lineMu.RLock()
	line := s.Line
	s.lineMu.RUnlock()
	status := "ok"
	if line.SMS == "unavailable" && line.Voice == "unavailable" {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status, "version": s.Version})
}

// identity is intentionally unauthenticated: it is the discovery handshake
// used to decide which Gateway credentials, if any, should be tried. It never
// exposes a token, modem identifier, phone number, or APNs credential.
func (s *Server) identity(w http.ResponseWriter, r *http.Request) {
	mode := s.NetworkMode
	if mode == "" {
		mode = "tailnet"
	}
	transport := s.NetworkTransport
	if transport == "" {
		if mode == "pocket" {
			transport = "pocketWiFi"
		} else {
			transport = "tailnet"
		}
	}
	slog.Info("gateway identity served", "remote_addr", r.RemoteAddr, "gateway_id", s.GatewayID, "mode", mode, "transport", transport)
	writeJSON(w, http.StatusOK, map[string]string{
		"gatewayId":   s.GatewayID,
		"gatewayName": s.Name,
		"mode":        mode,
		"transport":   transport,
		"apiVersion":  "v1",
		"publicKey":   s.Fingerprint,
		"fingerprint": s.Fingerprint,
	})
}

func (s *Server) gateway(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	s.lineMu.RLock()
	capabilities := s.Capabilities
	s.lineMu.RUnlock()
	slog.Info("gateway status served", "remote_addr", r.RemoteAddr, "forwarded_for", forwardedFor(r), "tailscale_user", tailscaleUser(r), "gateway_id", s.GatewayID)
	transport := s.NetworkTransport
	if transport == "" {
		transport = s.NetworkMode
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": s.GatewayID, "name": s.Name, "capabilities": capabilities, "lineID": s.GatewayID + ":line", "transport": transport})
}

func (s *Server) line(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	s.lineMu.RLock()
	line := s.Line
	s.lineMu.RUnlock()
	slog.Info("line status served", "remote_addr", r.RemoteAddr, "forwarded_for", forwardedFor(r), "tailscale_user", tailscaleUser(r), "gateway_id", s.GatewayID)
	writeJSON(w, http.StatusOK, line)
}

func (s *Server) SetLineStatus(line modem.LineStatus) {
	s.lineMu.Lock()
	s.Line = line
	s.lineMu.Unlock()
}

func (s *Server) SetCapabilities(capabilities modem.Capabilities) {
	s.lineMu.Lock()
	s.Capabilities = capabilities
	s.lineMu.Unlock()
}

func (s *Server) pairingStart(w http.ResponseWriter, r *http.Request) {
	slog.Info("pairing start request",
		"remote_addr", r.RemoteAddr,
		"forwarded_for", strings.TrimSpace(r.Header.Get("X-Forwarded-For")),
		"tailscale_user", strings.TrimSpace(r.Header.Get("Tailscale-User-Login")),
		"network_mode", s.NetworkMode,
	)
	if s.Auth == nil {
		slog.Warn("pairing start unavailable", "remote_addr", r.RemoteAddr)
		writeError(w, http.StatusServiceUnavailable, "CB-PAIR-000", "pairing service unavailable")
		return
	}
	var started auth.PairingStart
	var err error
	if s.NetworkMode == "tailnet" {
		// In Tailnet mode the Serve listener is the only remote entry point.
		// Serve authenticates the Tailnet peer before proxying to this loopback
		// backend, and the backend may therefore see either the peer address or
		// the loopback proxy address. Public fallback remains disabled.
		started, err = s.Auth.StartPairingFromTailnet(r.RemoteAddr, s.Fingerprint)
	} else {
		started, err = s.Auth.StartPairing(r.RemoteAddr, s.Fingerprint)
	}
	if err != nil {
		slog.Warn("pairing start rejected", "remote_addr", r.RemoteAddr, "network_mode", s.NetworkMode, "error", err)
		writeError(w, http.StatusForbidden, "CB-PAIR-001", err.Error())
		return
	}
	slog.Info("pairing start issued", "remote_addr", r.RemoteAddr, "pairing_id", started.PairingID, "gateway_id", started.GatewayID)
	response := map[string]any{
		"gatewayId":     started.GatewayID,
		"pairingId":     started.PairingID,
		"oneTimeSecret": started.OneTimeSecret,
		"expiresAt":     started.ExpiresAt,
		"fingerprint":   started.Fingerprint,
	}
	if s.NetworkMode == "tailnet" && strings.TrimSpace(s.TailnetHostname) != "" {
		response["baseURL"] = "https://" + strings.TrimSpace(s.TailnetHostname)
		response["mode"] = "tailnet"
	}
	writeJSON(w, http.StatusOK, response)
}

type pairingCompleteRequest struct {
	PairingID       string `json:"pairingId"`
	DeviceName      string `json:"deviceName"`
	DevicePublicKey string `json:"devicePublicKey"`
	Proof           string `json:"proof"`
}

func (s *Server) pairingComplete(w http.ResponseWriter, r *http.Request) {
	slog.Info("pairing complete request",
		"remote_addr", r.RemoteAddr,
		"forwarded_for", strings.TrimSpace(r.Header.Get("X-Forwarded-For")),
		"tailscale_user", strings.TrimSpace(r.Header.Get("Tailscale-User-Login")),
	)
	if s.Auth == nil {
		slog.Warn("pairing complete unavailable", "remote_addr", r.RemoteAddr)
		writeError(w, http.StatusServiceUnavailable, "CB-PAIR-000", "pairing service unavailable")
		return
	}
	var request pairingCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		slog.Warn("pairing complete invalid json", "remote_addr", r.RemoteAddr, "error", err)
		writeError(w, http.StatusBadRequest, "CB-PAIR-002", "invalid JSON body")
		return
	}
	slog.Info("pairing complete payload received", "remote_addr", r.RemoteAddr, "pairing_id", request.PairingID, "device_name", request.DeviceName)
	publicKeyBytes, err := decodeBase64(request.DevicePublicKey)
	if err != nil || len(publicKeyBytes) != ed25519.PublicKeySize {
		slog.Warn("pairing complete public key rejected", "remote_addr", r.RemoteAddr, "pairing_id", request.PairingID, "error", err)
		writeError(w, http.StatusBadRequest, "CB-PAIR-003", "invalid device public key")
		return
	}
	proof, err := decodeBase64(request.Proof)
	if err != nil {
		slog.Warn("pairing complete proof encoding rejected", "remote_addr", r.RemoteAddr, "pairing_id", request.PairingID, "error", err)
		writeError(w, http.StatusBadRequest, "CB-PAIR-004", "invalid pairing proof")
		return
	}
	deviceID, access, refresh, err := s.Auth.Complete(r.Context(), request.PairingID, request.DeviceName, ed25519.PublicKey(publicKeyBytes), proof)
	if err != nil {
		slog.Warn("pairing complete rejected", "remote_addr", r.RemoteAddr, "pairing_id", request.PairingID, "error", err)
		writeError(w, http.StatusUnauthorized, "CB-PAIR-005", err.Error())
		return
	}
	slog.Info("pairing complete accepted", "remote_addr", r.RemoteAddr, "pairing_id", request.PairingID, "device_id", deviceID)
	writeJSON(w, http.StatusOK, map[string]string{"deviceId": deviceID, "accessToken": access, "refreshToken": refresh})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-AUTH-000", "authentication service unavailable")
		return
	}
	var request struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || strings.TrimSpace(request.RefreshToken) == "" {
		writeError(w, http.StatusBadRequest, "CB-AUTH-002", "refreshToken is required")
		return
	}
	access, refresh, err := s.Auth.Refresh(r.Context(), request.RefreshToken)
	if err != nil {
		slog.Warn("auth refresh rejected", "remote_addr", r.RemoteAddr, "error", err)
		writeError(w, http.StatusUnauthorized, "CB-AUTH-003", "invalid refresh token")
		return
	}
	slog.Info("auth refresh accepted", "remote_addr", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]string{"accessToken": access, "refreshToken": refresh})
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-SYNC-000", "database unavailable")
		return
	}
	after, limit, err := pagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CB-SYNC-002", err.Error())
		return
	}
	changes, err := s.Database.Sync(r.Context(), after, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-SYNC-003", err.Error())
		return
	}
	// Keep the wire contract stable for clients that decode changes as an
	// array. A nil Go slice would otherwise be encoded as JSON null when there
	// are no new changes, which makes a successful empty sync look like a
	// decoding failure on iOS.
	if changes == nil {
		changes = make([]db.Change, 0)
	}
	slog.Info("sync response served", "remote_addr", r.RemoteAddr, "forwarded_for", forwardedFor(r), "tailscale_user", tailscaleUser(r), "after", after, "limit", limit, "changes", len(changes))
	to := after
	if len(changes) > 0 {
		to = changes[len(changes)-1].Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": after, "to": to, "hasMore": len(changes) == limit, "changes": changes})
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.sendMessage(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "CB-API-001", "method not allowed")
		return
	}
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-SMS-000", "database unavailable")
		return
	}
	after, limit, err := pagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CB-SMS-003", err.Error())
		return
	}
	writeMessages := func(messages []db.Message) {
		slog.Info("messages response served", "remote_addr", r.RemoteAddr, "forwarded_for", forwardedFor(r), "tailscale_user", tailscaleUser(r), "after", after, "limit", limit, "thread_query", strings.TrimSpace(r.URL.Query().Get("threadKey")) != "", "messages", len(messages))
		response := make([]messageResponse, 0, len(messages))
		for _, message := range messages {
			response = append(response, s.messageResponseFromDB(message))
		}
		writeJSON(w, http.StatusOK, response)
	}
	if threadKey := strings.TrimSpace(r.URL.Query().Get("threadKey")); threadKey != "" {
		beforeCreatedAt, beforeID, cursorErr := threadMessageCursor(r)
		if cursorErr != nil {
			writeError(w, http.StatusBadRequest, "CB-SMS-005", cursorErr.Error())
			return
		}
		// Fetch one sentinel row so the client can stop without an extra empty
		// request when the oldest page has been reached.
		messages, listErr := s.Database.ListThreadMessages(r.Context(), threadKey, beforeCreatedAt, beforeID, limit+1)
		if listErr != nil {
			writeError(w, http.StatusInternalServerError, "CB-SMS-006", listErr.Error())
			return
		}
		hasMore := len(messages) > limit
		if hasMore {
			messages = messages[len(messages)-limit:]
		}
		w.Header().Set("X-CellBridge-Has-More", strconv.FormatBool(hasMore))
		writeMessages(messages)
		return
	}
	messages, err := s.Database.ListMessages(r.Context(), after, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-SMS-004", err.Error())
		return
	}
	writeMessages(messages)
}

type sendMessageRequest struct {
	To   string `json:"to"`
	Body string `json:"body"`
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	deviceID, ok := s.authorize(w, r)
	if !ok {
		return
	}
	requestID, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	operationContext := context.WithoutCancel(r.Context())
	if s.SMSEngine == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-SMS-000", "SMS modem is unavailable")
		return
	}
	if s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-SMS-000", "database unavailable")
		return
	}
	var request sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "CB-SMS-003", "invalid JSON body")
		return
	}
	request.To = strings.TrimSpace(request.To)
	request.Body = strings.TrimSpace(request.Body)
	if request.To == "" || len(request.To) > 32 || request.Body == "" || len([]rune(request.Body)) > 10000 {
		writeError(w, http.StatusBadRequest, "CB-SMS-003", "to or body is invalid")
		return
	}
	claimed, existing, err := s.claimIdempotency(operationContext, deviceID, "sms.send", requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-SMS-004", err.Error())
		return
	}
	if !claimed {
		if replayIdempotency(w, existing) {
			return
		}
	}
	if !s.allowSMSSend(deviceID) {
		s.writeIdempotentError(w, operationContext, deviceID, "sms.send", requestID, http.StatusTooManyRequests, "CB-SMS-008", "SMS send rate limit exceeded")
		return
	}
	// SMS submission is a modem operation, not a request-scoped read. If the
	// Tailnet path drops after AT+CMGS has accepted the PDU, cancelling here
	// would leave the message delivered but the database at queued/failed.
	// Finish the bounded operation and persist its truthful status so a client
	// can reconcile after a transport timeout instead of sending a duplicate.
	sendContext, cancelSend := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancelSend()
	message, err := s.SMSEngine.Send(sendContext, request.To, request.Body)
	if err != nil {
		if message != nil {
			s.audit(operationContext, deviceID, "sms.send", message.ID, "failed")
			slog.Warn("SMS submission failed after message creation", "message_id", message.ID, "status", message.Status, "duration_ms", time.Since(startedAt).Milliseconds(), "error", err)
			response := s.messageResponseFromDB(*message)
			body, marshalErr := json.Marshal(response)
			if marshalErr == nil {
				_ = s.Database.CompleteIdempotency(operationContext, deviceID, "sms.send", requestID, http.StatusBadGateway, body)
			}
			writeJSON(w, http.StatusBadGateway, response)
			return
		}
		// The request key is enough to correlate a failed submission. Do not
		// put the destination number into the audit log.
		s.audit(operationContext, deviceID, "sms.send", "request:"+requestID, "failed")
		slog.Warn("SMS submission failed before message creation", "request_id", requestID, "duration_ms", time.Since(startedAt).Milliseconds(), "error", err)
		body, marshalErr := json.Marshal(map[string]string{"code": "CB-SMS-002", "message": err.Error()})
		if marshalErr == nil {
			_ = s.Database.CompleteIdempotency(operationContext, deviceID, "sms.send", requestID, http.StatusBadGateway, body)
			writeRawJSON(w, http.StatusBadGateway, body)
		} else {
			writeError(w, http.StatusInternalServerError, "CB-SMS-004", marshalErr.Error())
		}
		return
	}
	response := s.messageResponseFromDB(*message)
	body, err := json.Marshal(response)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-SMS-004", err.Error())
		return
	}
	if err := s.Database.CompleteIdempotency(operationContext, deviceID, "sms.send", requestID, http.StatusCreated, body); err != nil {
		writeError(w, http.StatusInternalServerError, "CB-SMS-004", err.Error())
		return
	}
	s.audit(operationContext, deviceID, "sms.send", message.ID, "accepted")
	s.HandleMessage(message)
	slog.Info("SMS submission accepted", "message_id", message.ID, "duration_ms", time.Since(startedAt).Milliseconds())
	writeRawJSON(w, http.StatusCreated, body)
}

func (s *Server) allowSMSSend(deviceID string) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := time.Now()
	previous, ok := s.lastSMSSend[deviceID]
	if ok && now.Sub(previous) < time.Second {
		return false
	}
	s.lastSMSSend[deviceID] = now
	return true
}

func (s *Server) threads(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-SMS-000", "database unavailable")
		return
	}
	threads, err := s.Database.ListThreads(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-SMS-005", err.Error())
		return
	}
	slog.Info("threads response served", "remote_addr", r.RemoteAddr, "forwarded_for", forwardedFor(r), "tailscale_user", tailscaleUser(r), "threads", len(threads))
	response := make([]threadResponse, 0, len(threads))
	for _, thread := range threads {
		response = append(response, threadResponse{Key: thread.Key, Peer: thread.Peer, UnreadCount: thread.UnreadCount, LastMessage: s.messageResponseFromDB(thread.LastMessage)})
	}
	writeJSON(w, http.StatusOK, response)
}

type dialRequest struct {
	To           string `json:"to"`
	ClientCallID string `json:"clientCallId"`
}

type dtmfRequest struct {
	Digit string `json:"digit"`
}

type webRTCOfferRequest struct {
	SDP       string `json:"sdp"`
	Type      string `json:"type"`
	Transport string `json:"transport"`
}

type callResponse struct {
	ID                  string `json:"id"`
	GatewayID           string `json:"gatewayID"`
	LineID              string `json:"lineID"`
	Direction           string `json:"direction"`
	Peer                string `json:"peer"`
	State               string `json:"state"`
	StartedAt           int64  `json:"startedAt"`
	ConnectedAt         *int64 `json:"connectedAt"`
	EndedAt             *int64 `json:"endedAt"`
	EndReason           string `json:"endReason,omitempty"`
	RecordingID         string `json:"recordingId,omitempty"`
	RecordingState      string `json:"recordingState,omitempty"`
	RecordingDurationMs *int64 `json:"recordingDurationMs,omitempty"`
}

type recordingResponse struct {
	ID                 string    `json:"id"`
	GatewayID          string    `json:"gatewayID"`
	LineID             string    `json:"lineID"`
	CallID             string    `json:"callId"`
	State              string    `json:"state"`
	TriggerMode        string    `json:"triggerMode"`
	StartedAt          *int64    `json:"startedAt,omitempty"`
	StoppedAt          *int64    `json:"stoppedAt,omitempty"`
	DurationMs         *int64    `json:"durationMs,omitempty"`
	Container          string    `json:"container,omitempty"`
	Codec              string    `json:"codec,omitempty"`
	SampleRate         int       `json:"sampleRate,omitempty"`
	Channels           int       `json:"channels,omitempty"`
	Bitrate            int       `json:"bitrate,omitempty"`
	SizeBytes          *int64    `json:"sizeBytes,omitempty"`
	SHA256             string    `json:"sha256,omitempty"`
	WaveformVersion    string    `json:"waveformVersion,omitempty"`
	FailureCode        string    `json:"failureCode,omitempty"`
	FailureDetail      string    `json:"failureDetail,omitempty"`
	RetentionExpiresAt *int64    `json:"retentionExpiresAt,omitempty"`
	WaveformPeaks      []float32 `json:"waveformPeaks,omitempty"`
}

type recordingSettingsResponse struct {
	Enabled          bool   `json:"enabled"`
	AutoMode         string `json:"autoMode"`
	RetentionDays    int    `json:"retentionDays"`
	MinimumFreeBytes int64  `json:"minimumFreeBytes"`
	UsedBytes        int64  `json:"usedBytes"`
}

type recordingSettingsRequest struct {
	Enabled          *bool   `json:"enabled"`
	AutoMode         *string `json:"autoMode"`
	RetentionDays    *int    `json:"retentionDays"`
	MinimumFreeBytes *int64  `json:"minimumFreeBytes"`
}

func (s *Server) recordingSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "CB-API-001", "method not allowed")
		return
	}
	deviceID, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if s.Recordings == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-REC-000", "recording service unavailable")
		return
	}
	settings, err := s.Recordings.Settings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-REC-011", err.Error())
		return
	}
	if r.Method == http.MethodPut {
		var request recordingSettingsRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "CB-REC-012", "invalid recording settings JSON")
			return
		}
		if request.Enabled != nil {
			settings.Enabled = *request.Enabled
		}
		if request.AutoMode != nil {
			settings.AutoMode = strings.TrimSpace(*request.AutoMode)
		}
		if request.RetentionDays != nil {
			settings.RetentionDays = *request.RetentionDays
		}
		if request.MinimumFreeBytes != nil {
			settings.MinimumFreeBytes = *request.MinimumFreeBytes
		}
		if err := s.Recordings.UpdateSettings(r.Context(), settings); err != nil {
			if errors.Is(err, recording.ErrAutomaticRecordingDisabled) {
				writeError(w, http.StatusConflict, "CB-REC-013", err.Error())
				return
			}
			writeError(w, http.StatusBadRequest, "CB-REC-014", err.Error())
			return
		}
		s.audit(r.Context(), deviceID, "recording.settings.update", "recording", "accepted")
	}
	usedBytes, err := s.Recordings.UsedBytes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-REC-011", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, recordingSettingsResponse{
		Enabled: settings.Enabled, AutoMode: settings.AutoMode, RetentionDays: settings.RetentionDays,
		MinimumFreeBytes: settings.MinimumFreeBytes, UsedBytes: usedBytes,
	})
}

func recordingResponseFromDB(current db.Recording) recordingResponse {
	response := recordingResponse{ID: current.ID, CallID: current.CallID, State: current.State, TriggerMode: current.TriggerMode,
		StartedAt: nil, StoppedAt: nil, DurationMs: current.DurationMs, Container: current.Container, Codec: current.Codec,
		SampleRate: current.SampleRate, Channels: current.Channels, Bitrate: current.Bitrate, SizeBytes: current.SizeBytes,
		SHA256: current.SHA256, FailureCode: current.FailureCode, FailureDetail: current.FailureDetail}
	if current.StartedAt != nil {
		value := current.StartedAt.UnixMilli()
		response.StartedAt = &value
	}
	if current.StoppedAt != nil {
		value := current.StoppedAt.UnixMilli()
		response.StoppedAt = &value
	}
	if current.RetentionExpiresAt != nil {
		value := current.RetentionExpiresAt.UnixMilli()
		response.RetentionExpiresAt = &value
	}
	if current.WaveformPath != "" {
		response.WaveformVersion = "v1"
		if data, err := os.ReadFile(current.WaveformPath); err == nil {
			var payload struct {
				Peaks []float32 `json:"peaks"`
			}
			if json.Unmarshal(data, &payload) == nil {
				response.WaveformPeaks = payload.Peaks
			}
		}
	}
	return response
}

func (s *Server) calls(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.listCalls(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.dial(w, r)
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "CB-API-001", "method not allowed")
}

func (s *Server) listCalls(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-CALL-000", "call database unavailable")
		return
	}
	_, limit, err := pagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CB-CALL-005", err.Error())
		return
	}
	stored, err := s.Database.ListCalls(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-CALL-004", err.Error())
		return
	}
	response := make([]callResponse, 0, len(stored))
	for _, current := range stored {
		response = append(response, s.callResponseFromDB(current))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) dial(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	deviceID, ok := s.authorize(w, r)
	if !ok {
		return
	}
	requestID, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	if s.CallControl == nil || s.CallModem == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-CALL-000", "voice modem is unavailable")
		return
	}
	var request dialRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "CB-CALL-001", "invalid JSON body")
		return
	}
	request.To = strings.TrimSpace(request.To)
	request.ClientCallID = strings.TrimSpace(request.ClientCallID)
	if request.To == "" || len(request.To) > 32 || request.ClientCallID == "" {
		writeError(w, http.StatusBadRequest, "CB-CALL-002", "to and clientCallId are required")
		return
	}
	if call.IsEmergencyNumber(request.To) {
		writeError(w, http.StatusForbidden, "CB-CALL-011", call.ErrEmergencyCall.Error())
		return
	}
	claimed, existing, err := s.claimIdempotency(r.Context(), deviceID, "call.dial", requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-CALL-004", err.Error())
		return
	}
	if !claimed {
		replayIdempotency(w, existing)
		return
	}
	current, event, err := s.CallControl.Dial(requestID, request.ClientCallID, request.To)
	if err != nil {
		slog.Warn("call control dial rejected", "request_id", requestID, "call_id", request.ClientCallID, "duration_ms", time.Since(startedAt).Milliseconds(), "error", err)
		s.writeIdempotentCallError(w, r.Context(), deviceID, "call.dial", requestID, err)
		return
	}
	if binder, ok := s.CallModem.(interface{ SetLogicalCallID(string) }); ok {
		binder.SetLogicalCallID(current.ID)
	}
	if err := s.CallModem.Dial(r.Context(), request.To); err != nil {
		slog.Warn("modem dial failed", "request_id", requestID, "call_id", current.ID, "duration_ms", time.Since(startedAt).Milliseconds(), "error", err)
		if errors.Is(err, modem.ErrActiveCall) {
			if cleanupErr := s.CallModem.Hangup(r.Context()); cleanupErr != nil {
				slog.Warn("stale modem call cleanup failed", "request_id", requestID, "error", cleanupErr)
			} else {
				slog.Info("stale modem call cleaned after dial rejection", "request_id", requestID)
			}
		}
		_, _, _ = s.CallControl.Hangup("modem-failure:"+requestID, current.ID, "modem_dial_failed")
		if ended, _, finishErr := s.CallControl.Finish(current.ID); finishErr == nil {
			current = ended
		}
		_, _ = s.persistCall(r.Context(), current, event)
		s.writeIdempotentCallError(w, r.Context(), deviceID, "call.dial", requestID, err)
		return
	}
	current, event, err = s.CallControl.MarkCellularReady(current.ID)
	if err != nil {
		// The AT command may have succeeded while the in-memory state update
		// failed. Release the modem and finish the logical call before
		// returning, otherwise the next dial sees a permanently busy line.
		_ = s.CallModem.Hangup(r.Context())
		if ended, finishEvent, finishErr := s.CallControl.Finish(current.ID); finishErr == nil {
			current = ended
			_, _ = s.persistCall(r.Context(), current, finishEvent)
		}
		slog.Warn("call cellular state update failed", "request_id", requestID, "call_id", current.ID, "duration_ms", time.Since(startedAt).Milliseconds(), "error", err)
		s.writeIdempotentCallError(w, r.Context(), deviceID, "call.dial", requestID, err)
		return
	}
	if _, err := s.persistCall(r.Context(), current, event); err != nil {
		s.writeIdempotentError(w, r.Context(), deviceID, "call.dial", requestID, http.StatusInternalServerError, "CB-CALL-004", err.Error())
		return
	}
	response := s.callResponseFromDomain(current)
	body, err := json.Marshal(response)
	if err != nil {
		s.writeIdempotentError(w, r.Context(), deviceID, "call.dial", requestID, http.StatusInternalServerError, "CB-CALL-004", err.Error())
		return
	}
	if s.Database != nil {
		if err := s.Database.CompleteIdempotency(r.Context(), deviceID, "call.dial", requestID, http.StatusCreated, body); err != nil {
			writeError(w, http.StatusInternalServerError, "CB-CALL-004", err.Error())
			return
		}
	}
	s.audit(r.Context(), deviceID, "call.dial", current.ID, "accepted")
	slog.Info("call dial accepted", "request_id", requestID, "call_id", current.ID, "duration_ms", time.Since(startedAt).Milliseconds())
	writeRawJSON(w, http.StatusCreated, body)
}

func (s *Server) callAction(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	deviceID, ok := s.authorize(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/calls/")
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(parts) == 1 && r.Method == http.MethodGet {
		s.getCall(w, r, parts[0])
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		s.deleteCall(w, r, parts[0], deviceID)
		return
	}
	if len(parts) == 2 && parts[1] == "ice" && r.Method == http.MethodGet {
		s.callICE(w, r, parts[0], deviceID)
		return
	}
	if len(parts) == 3 && parts[1] == "webrtc" && parts[2] == "offer" && r.Method == http.MethodPost {
		s.webRTCOffer(w, r, parts[0])
		return
	}
	if len(parts) == 3 && parts[1] == "recording" && (parts[2] == "start" || parts[2] == "stop") && r.Method == http.MethodPost {
		s.callRecordingAction(w, r, parts[0], parts[2])
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "CB-CALL-006", "call action not found")
		return
	}
	requestID, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	callID, action := parts[0], parts[1]
	if action != "answer" && action != "reject" && action != "hangup" && action != "dtmf" {
		writeError(w, http.StatusNotFound, "CB-CALL-006", "call action not found")
		return
	}
	if s.CallControl == nil || s.CallModem == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-CALL-000", "voice modem is unavailable")
		return
	}
	var dtmf dtmfRequest
	if action == "dtmf" {
		if decodeErr := json.NewDecoder(r.Body).Decode(&dtmf); decodeErr != nil {
			writeError(w, http.StatusBadRequest, "CB-CALL-007", "invalid JSON body")
			return
		}
		digit := []rune(strings.TrimSpace(dtmf.Digit))
		if len(digit) != 1 {
			writeError(w, http.StatusBadRequest, "CB-CALL-008", "digit must contain exactly one DTMF character")
			return
		}
	}
	claimed, existing, err := s.claimIdempotency(r.Context(), deviceID, "call."+action, requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-CALL-004", err.Error())
		return
	}
	if !claimed {
		replayIdempotency(w, existing)
		return
	}
	var (
		current call.Call
		event   call.Event
		modemOK bool
		persist = true
	)
	switch action {
	case "answer":
		current, event, err = s.CallControl.Answer(requestID, callID)
		if err == nil {
			err = s.CallModem.Answer(r.Context())
			modemOK = err == nil
		}
	case "reject", "hangup":
		current, event, err = s.CallControl.Hangup(requestID, callID, action)
		if err == nil {
			err = s.CallModem.Hangup(r.Context())
			modemOK = err == nil
			if errors.Is(err, modem.ErrNoActiveCall) {
				// The modem may have emitted its terminal URC just before
				// this action reached the adapter. The logical hangup is still
				// complete, and rolling the controller back here would leave
				// the app showing a call that the hardware has already ended.
				slog.Info("hangup observed modem already idle", "action", action, "call_id", callID)
				err = nil
				modemOK = true
			}
		}
	case "dtmf":
		digit := []rune(strings.TrimSpace(dtmf.Digit))
		if err = s.CallControl.DTMF(callID, digit[0]); err == nil {
			err = s.CallModem.DTMF(r.Context(), digit[0])
		}
	default:
		writeError(w, http.StatusNotFound, "CB-CALL-006", "call action not found")
		return
	}
	if err == nil && action == "answer" {
		current, event, err = s.CallControl.MarkCellularReady(callID)
	}
	if err == nil && modemOK && (action == "reject" || action == "hangup") {
		if ended, finishEvent, finishErr := s.CallControl.Finish(callID); finishErr == nil {
			current, event = ended, finishEvent
		} else if errors.Is(finishErr, call.ErrCallNotFound) {
			// The modem adapter may have delivered its terminal URC before
			// this handler reached Finish. The event reader owns that race;
			// do not persist the stale Ending snapshot over its terminal row.
			persist = false
		} else {
			err = finishErr
		}
	}
	if err != nil {
		if !modemOK && (action == "answer" || action == "reject" || action == "hangup") {
			_ = s.CallControl.Rollback(requestID, callID)
		}
		s.audit(r.Context(), deviceID, "call."+action, callID, "failed")
		slog.Warn("call action failed", "action", action, "call_id", callID, "duration_ms", time.Since(startedAt).Milliseconds(), "error", err)
		s.writeIdempotentCallError(w, r.Context(), deviceID, "call."+action, requestID, err)
		return
	}
	if action != "dtmf" {
		if persist {
			if _, err := s.persistCall(r.Context(), current, event); err != nil {
				s.writeIdempotentError(w, r.Context(), deviceID, "call."+action, requestID, http.StatusInternalServerError, "CB-CALL-004", err.Error())
				return
			}
		}
		if action == "hangup" || action == "reject" {
			s.closeWebRTC(callID)
		}
	}
	if s.Database != nil {
		if err := s.Database.CompleteIdempotency(r.Context(), deviceID, "call."+action, requestID, http.StatusNoContent, nil); err != nil {
			writeError(w, http.StatusInternalServerError, "CB-CALL-004", err.Error())
			return
		}
	}
	s.audit(r.Context(), deviceID, "call."+action, callID, "accepted")
	slog.Info("call action accepted", "action", action, "call_id", callID, "duration_ms", time.Since(startedAt).Milliseconds())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteCall(w http.ResponseWriter, r *http.Request, callID, deviceID string) {
	startedAt := time.Now()
	if s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-CALL-000", "call database unavailable")
		return
	}
	requestID, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	claimed, existing, err := s.claimIdempotency(r.Context(), deviceID, "call.delete", requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-CALL-004", err.Error())
		return
	}
	if !claimed {
		replayIdempotency(w, existing)
		return
	}
	current, err := s.Database.GetCall(r.Context(), callID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			body := []byte(`{"code":"CB-CALL-006","message":"call not found"}`)
			_ = s.Database.CompleteIdempotency(r.Context(), deviceID, "call.delete", requestID, http.StatusNotFound, body)
			writeError(w, http.StatusNotFound, "CB-CALL-006", "call not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "CB-CALL-004", err.Error())
		return
	}
	if current.EndedAt == nil {
		body := []byte(`{"code":"CB-CALL-010","message":"active call cannot be deleted"}`)
		_ = s.Database.CompleteIdempotency(r.Context(), deviceID, "call.delete", requestID, http.StatusConflict, body)
		writeError(w, http.StatusConflict, "CB-CALL-010", "active call cannot be deleted")
		return
	}
	sequence, err := s.Database.DeleteCall(r.Context(), callID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			body := []byte(`{"code":"CB-CALL-006","message":"call not found"}`)
			_ = s.Database.CompleteIdempotency(r.Context(), deviceID, "call.delete", requestID, http.StatusNotFound, body)
			writeError(w, http.StatusNotFound, "CB-CALL-006", "call not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "CB-CALL-004", err.Error())
		return
	}
	if err := s.Database.CompleteIdempotency(r.Context(), deviceID, "call.delete", requestID, http.StatusNoContent, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "CB-CALL-004", err.Error())
		return
	}
	s.audit(r.Context(), deviceID, "call.delete", callID, "accepted")
	if s.Events != nil {
		s.Events.Publish(sequence, "call.deleted", map[string]string{"id": callID})
	}
	slog.Info("call delete accepted", "call_id", callID, "duration_ms", time.Since(startedAt).Milliseconds())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getCall(w http.ResponseWriter, r *http.Request, callID string) {
	if strings.TrimSpace(callID) == "" {
		writeError(w, http.StatusNotFound, "CB-CALL-006", "call not found")
		return
	}
	if s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-CALL-000", "call database unavailable")
		return
	}
	stored, err := s.Database.GetCall(r.Context(), callID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "CB-CALL-006", "call not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "CB-CALL-004", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.callResponseFromDB(stored))
}

func (s *Server) callRecordingAction(w http.ResponseWriter, r *http.Request, callID, action string) {
	deviceID, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if s.Recordings == nil || s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-REC-000", "recording service unavailable")
		return
	}
	requestID, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	operation := "recording." + action
	claimed, existing, err := s.claimIdempotency(r.Context(), deviceID, operation, requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-REC-001", err.Error())
		return
	}
	if !claimed {
		replayIdempotency(w, existing)
		return
	}
	current, err := s.Database.GetCall(r.Context(), callID)
	if err != nil {
		status := http.StatusInternalServerError
		code := "CB-REC-002"
		if errors.Is(err, sql.ErrNoRows) {
			status, code = http.StatusNotFound, "CB-CALL-006"
		}
		s.writeIdempotentError(w, r.Context(), deviceID, operation, requestID, status, code, "call not found")
		return
	}
	var response recordingResponse
	status := http.StatusOK
	if action == "start" {
		if current.State != string(call.Active) {
			slog.Warn("recording start rejected", "call_id", callID, "call_state", current.State, "error_code", "CB-REC-003")
			s.writeIdempotentError(w, r.Context(), deviceID, operation, requestID, http.StatusConflict, "CB-REC-003", "recording requires an active call")
			return
		}
		stored, startErr := s.Recordings.Start(r.Context(), recording.CallMeta{ID: current.ID, Direction: current.Direction, Peer: current.Peer, StartedAt: current.StartedAt}, "manual", deviceID)
		if startErr != nil {
			slog.Error("recording start failed", "call_id", callID, "error", startErr)
			code, status := "CB-REC-004", http.StatusBadGateway
			if errors.Is(startErr, recording.ErrLowStorage) {
				code, status = "CB-REC-005", http.StatusInsufficientStorage
			} else if errors.Is(startErr, recording.ErrRecordingDisabled) {
				code, status = "CB-REC-015", http.StatusConflict
			}
			s.writeIdempotentError(w, r.Context(), deviceID, operation, requestID, status, code, startErr.Error())
			return
		}
		slog.Info("recording start accepted", "call_id", callID, "recording_id", stored.ID)
		response, status = s.recordingResponseFromDB(stored), http.StatusCreated
	} else {
		stopErr := s.Recordings.Stop(r.Context(), callID, "manual")
		if stopErr != nil && !errors.Is(stopErr, recording.ErrNotActive) {
			slog.Error("recording stop failed", "call_id", callID, "error", stopErr)
			s.writeIdempotentError(w, r.Context(), deviceID, operation, requestID, http.StatusBadGateway, "CB-REC-006", stopErr.Error())
			return
		}
		slog.Info("recording stop accepted", "call_id", callID)
		stored, getErr := s.Database.GetRecordingByCall(r.Context(), callID)
		if getErr != nil {
			s.writeIdempotentError(w, r.Context(), deviceID, operation, requestID, http.StatusNotFound, "CB-REC-007", "recording not found")
			return
		}
		response = s.recordingResponseFromDB(stored)
		status = http.StatusAccepted
	}
	body, err := json.Marshal(response)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-REC-001", err.Error())
		return
	}
	if err := s.Database.CompleteIdempotency(r.Context(), deviceID, operation, requestID, status, body); err != nil {
		writeError(w, http.StatusInternalServerError, "CB-REC-001", err.Error())
		return
	}
	s.audit(r.Context(), deviceID, operation, response.ID, "accepted")
	writeRawJSON(w, status, body)
}

func (s *Server) recordings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "CB-API-001", "method not allowed")
		return
	}
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if s.Recordings == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-REC-000", "recording service unavailable")
		return
	}
	after, limit, err := pagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CB-REC-001", err.Error())
		return
	}
	from, to, err := recordingTimeFilters(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CB-REC-001", err.Error())
		return
	}
	items, err := s.Recordings.List(r.Context(), after, limit, r.URL.Query().Get("direction"), strings.TrimSpace(r.URL.Query().Get("query")), from, to)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CB-REC-001", err.Error())
		return
	}
	response := make([]recordingResponse, 0, len(items))
	for _, item := range items {
		response = append(response, s.recordingResponseFromDB(item))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) recordingAction(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if s.Recordings == nil || s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-REC-000", "recording service unavailable")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/recordings/")
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(parts) == 2 && r.Method == http.MethodGet && (parts[1] == "audio" || parts[1] == "waveform") {
		s.serveRecordingAsset(w, r, parts[0], parts[1])
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "CB-REC-007", "recording not found")
		return
	}
	recordingID := parts[0]
	if r.Method == http.MethodGet {
		stored, err := s.Recordings.Get(r.Context(), recordingID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "CB-REC-007", "recording not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "CB-REC-001", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.recordingResponseFromDB(stored))
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "CB-API-001", "method not allowed")
		return
	}
	requestID, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	operation := "recording.delete"
	claimed, existing, err := s.claimIdempotency(r.Context(), deviceID, operation, requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-REC-001", err.Error())
		return
	}
	if !claimed {
		replayIdempotency(w, existing)
		return
	}
	if err := s.Recordings.Delete(r.Context(), recordingID); err != nil {
		status, code := http.StatusBadGateway, "CB-REC-008"
		if errors.Is(err, sql.ErrNoRows) {
			status, code = http.StatusNotFound, "CB-REC-007"
		}
		s.writeIdempotentError(w, r.Context(), deviceID, operation, requestID, status, code, err.Error())
		return
	}
	if err := s.Database.CompleteIdempotency(r.Context(), deviceID, operation, requestID, http.StatusNoContent, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "CB-REC-001", err.Error())
		return
	}
	s.audit(r.Context(), deviceID, operation, recordingID, "accepted")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serveRecordingAsset(w http.ResponseWriter, r *http.Request, recordingID, asset string) {
	var path string
	var err error
	if asset == "audio" {
		path, err = s.Recordings.AudioPath(r.Context(), recordingID)
	} else {
		path, err = s.Recordings.WaveformPath(r.Context(), recordingID)
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "CB-REC-007", "recording asset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-REC-001", err.Error())
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "CB-REC-007", "recording asset not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusNotFound, "CB-REC-007", "recording asset not found")
		return
	}
	w.Header().Set("Cache-Control", "private")
	if asset == "audio" {
		w.Header().Set("Content-Type", "audio/mp4")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), file)
}

func recordingTimeFilters(r *http.Request) (*time.Time, *time.Time, error) {
	parse := func(name string) (*time.Time, error) {
		value := strings.TrimSpace(r.URL.Query().Get(name))
		if value == "" {
			return nil, nil
		}
		milliseconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s must be Unix milliseconds", name)
		}
		parsed := time.UnixMilli(milliseconds)
		return &parsed, nil
	}
	from, err := parse("from")
	if err != nil {
		return nil, nil, err
	}
	to, err := parse("to")
	return from, to, err
}

func (s *Server) webRTCOffer(w http.ResponseWriter, r *http.Request, callID string) {
	startedAt := time.Now()
	deviceID, ok := s.authorize(w, r)
	if !ok {
		return
	}
	requestID, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	if s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-WEBRTC-000", "call database unavailable")
		return
	}
	if _, err := s.Database.GetCall(r.Context(), callID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			slog.Warn("WebRTC offer rejected: call not found", "call_id", callID)
			writeError(w, http.StatusNotFound, "CB-CALL-006", "call not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "CB-WEBRTC-000", err.Error())
		return
	}
	if s.WebRTC == nil {
		slog.Warn("WebRTC offer rejected: service unavailable", "call_id", callID)
		writeError(w, http.StatusServiceUnavailable, "CB-WEBRTC-000", "WebRTC service unavailable")
		return
	}
	if s.VoiceAudio == nil {
		slog.Warn("WebRTC offer rejected: voice audio unavailable", "call_id", callID)
		writeError(w, http.StatusServiceUnavailable, "CB-VOICE-001", "voice audio backend is unavailable")
		return
	}
	var request webRTCOfferRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "CB-WEBRTC-001", "invalid JSON body")
		return
	}
	if request.Type != "offer" || strings.TrimSpace(request.SDP) == "" {
		writeError(w, http.StatusBadRequest, "CB-WEBRTC-002", "an SDP offer is required")
		return
	}
	transport := webrtc.Transport(strings.ToLower(strings.TrimSpace(request.Transport)))
	if transport != webrtc.TransportTailnet && transport != webrtc.TransportPocket {
		writeError(w, http.StatusBadRequest, "CB-WEBRTC-003", "transport must be tailnet or pocket")
		return
	}
	claimed, existing, err := s.Database.ClaimIdempotency(r.Context(), deviceID, "webrtc.offer", requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-WEBRTC-006", err.Error())
		return
	}
	if !claimed {
		replayIdempotency(w, existing)
		return
	}
	s.webrtcMu.Lock()
	session := s.webrtcCalls[callID]
	if session == nil {
		var sessionErr error
		turnServers := s.WebRTCTurnServers
		if transport == webrtc.TransportTailnet {
			turnServer, _, turnReady := s.privateTURNServer(time.Now().UTC(), "gateway")
			if !turnReady {
				slog.Warn("WebRTC offer rejected: private TURN unavailable", "call_id", callID, "transport", transport)
				s.webrtcMu.Unlock()
				writeError(w, http.StatusServiceUnavailable, "CB-WEBRTC-008", "private tailnet TURN is not configured")
				return
			}
			turnServers = []pion.ICEServer{{URLs: turnServer.URLs, Username: turnServer.Username, Credential: turnServer.Credential}}
		}
		session, sessionErr = s.WebRTC.NewSession(transport, turnServers)
		if sessionErr != nil {
			slog.Warn("WebRTC session creation failed", "call_id", callID, "transport", transport, "error", sessionErr)
			s.webrtcMu.Unlock()
			s.writeIdempotentError(w, r.Context(), deviceID, "webrtc.offer", requestID, http.StatusBadGateway, "CB-WEBRTC-004", sessionErr.Error())
			return
		}
		s.webrtcCalls[callID] = session
		if s.VoiceAudio != nil {
			bridge := voice.NewBridge(s.VoiceAudio, session)
			if s.Recordings != nil {
				bridge.SetFrameHooks(
					func(pcm []int16) { _ = s.Recordings.WriteCellularFrame(callID, pcm) },
					func(pcm []int16) { _ = s.Recordings.WriteClientFrame(callID, pcm) },
				)
			}
			if bridgeErr := bridge.Start(context.Background(), modem.CallID(callID)); bridgeErr != nil {
				slog.Warn("voice bridge start failed", "call_id", callID, "error", bridgeErr)
				delete(s.webrtcCalls, callID)
				_ = session.Close()
				s.webrtcMu.Unlock()
				s.writeIdempotentError(w, r.Context(), deviceID, "webrtc.offer", requestID, http.StatusBadGateway, "CB-WEBRTC-007", bridgeErr.Error())
				return
			}
			s.voiceBridges[callID] = bridge
		}
		session.PeerConnection().OnConnectionStateChange(func(state pion.PeerConnectionState) {
			slog.Info("WebRTC peer connection state", "call_id", callID, "state", state.String())
			if s.CallControl == nil {
				return
			}
			if state == pion.PeerConnectionStateDisconnected || state == pion.PeerConnectionStateFailed {
				current, event, err := s.CallControl.MarkRecovering(callID, "media_"+strings.ToLower(state.String()))
				if err == nil {
					_, _ = s.persistCall(context.Background(), current, event)
				}
				s.closeWebRTC(callID)
				return
			}
			if state != pion.PeerConnectionStateConnected {
				return
			}
			current, event, err := s.CallControl.MarkMediaReady(callID)
			if err == nil {
				_, _ = s.persistCall(context.Background(), current, event)
			} else {
				slog.Warn("mark call media ready failed", "call_id", callID, "error", err)
			}
		})
	}
	s.webrtcMu.Unlock()
	answer, err := session.CreateAnswer(r.Context(), request.SDP, transport)
	if err != nil {
		slog.Warn("WebRTC answer creation failed", "call_id", callID, "transport", transport, "duration_ms", time.Since(startedAt).Milliseconds(), "error", err)
		s.writeIdempotentError(w, r.Context(), deviceID, "webrtc.offer", requestID, http.StatusBadGateway, "CB-WEBRTC-005", err.Error())
		return
	}
	body, err := json.Marshal(answer)
	if err != nil {
		s.writeIdempotentError(w, r.Context(), deviceID, "webrtc.offer", requestID, http.StatusInternalServerError, "CB-WEBRTC-006", err.Error())
		return
	}
	if err := s.Database.CompleteIdempotency(r.Context(), deviceID, "webrtc.offer", requestID, http.StatusOK, body); err != nil {
		writeError(w, http.StatusInternalServerError, "CB-WEBRTC-006", err.Error())
		return
	}
	slog.Info("WebRTC offer accepted", "call_id", callID, "transport", transport, "duration_ms", time.Since(startedAt).Milliseconds(), "answer_sdp_bytes", len(answer.SDP), "answer_media_sections", strings.Count(answer.SDP, "m="), "answer_candidates", strings.Count(answer.SDP, "a=candidate:"), "answer_has_fingerprint", strings.Contains(answer.SDP, "a=fingerprint:"), "answer_sdp_structure", sdpStructure(answer.SDP))
	writeRawJSON(w, http.StatusOK, body)
}

// sdpStructure keeps diagnostics useful without writing ICE credentials,
// fingerprints, or private candidate addresses into the service journal.
func sdpStructure(sdp string) string {
	var structure []string
	for _, rawLine := range strings.Split(sdp, "\n") {
		line := strings.TrimSpace(rawLine)
		switch {
		case strings.HasPrefix(line, "m="), strings.HasPrefix(line, "a=group:"), strings.HasPrefix(line, "a=mid:"),
			strings.HasPrefix(line, "a=send"), strings.HasPrefix(line, "a=recv"), strings.HasPrefix(line, "a=inactive"),
			strings.HasPrefix(line, "a=rtcp-mux"), strings.HasPrefix(line, "a=rtpmap:"), strings.HasPrefix(line, "a=fmtp:"),
			strings.HasPrefix(line, "a=setup:"), strings.HasPrefix(line, "a=ice-options:"), strings.HasPrefix(line, "a=end-of-candidates"):
			structure = append(structure, line)
		case strings.HasPrefix(line, "a=candidate:"), strings.HasPrefix(line, "a=ice-ufrag:"), strings.HasPrefix(line, "a=ice-pwd:"), strings.HasPrefix(line, "a=fingerprint:"):
			structure = append(structure, strings.SplitN(line, ":", 2)[0]+":<redacted>")
		}
	}
	return strings.Join(structure, "|")
}

type iceServerResponse struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

func (s *Server) callICE(w http.ResponseWriter, r *http.Request, callID, deviceID string) {
	if strings.TrimSpace(callID) == "" || s.Database == nil {
		writeError(w, http.StatusNotFound, "CB-CALL-006", "call not found")
		return
	}
	if _, err := s.Database.GetCall(r.Context(), callID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "CB-CALL-006", "call not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "CB-WEBRTC-000", err.Error())
		return
	}
	if s.NetworkMode != "tailnet" || !s.PrivateTURN.Enabled || strings.TrimSpace(s.PrivateTURN.Host) == "" || len(s.PrivateTURN.Secret) == 0 {
		slog.Warn("ICE configuration unavailable", "call_id", callID, "network_mode", s.NetworkMode, "private_turn_enabled", s.PrivateTURN.Enabled, "private_turn_host_set", strings.TrimSpace(s.PrivateTURN.Host) != "", "private_turn_secret_set", len(s.PrivateTURN.Secret) != 0)
		writeError(w, http.StatusServiceUnavailable, "CB-WEBRTC-008", "private tailnet TURN is not configured")
		return
	}
	turnServer, expiresAt, ok := s.privateTURNServer(time.Now().UTC(), deviceID)
	if !ok {
		slog.Warn("ICE credential generation failed", "call_id", callID, "device_id", deviceID)
		writeError(w, http.StatusServiceUnavailable, "CB-WEBRTC-008", "private tailnet TURN is not configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"policy":     "tailnet-turn",
		"iceServers": []iceServerResponse{turnServer},
		"expiresAt":  expiresAt.Format(time.RFC3339),
	})
	slog.Info("ICE configuration served", "call_id", callID, "device_id", deviceID, "policy", "tailnet-turn")
}

func (s *Server) privateTURNServer(now time.Time, subject string) (iceServerResponse, time.Time, bool) {
	if s.NetworkMode != "tailnet" || !s.PrivateTURN.Enabled || strings.TrimSpace(s.PrivateTURN.Host) == "" || len(s.PrivateTURN.Secret) == 0 {
		return iceServerResponse{}, time.Time{}, false
	}
	ttl := s.PrivateTURN.CredentialTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	port := s.PrivateTURN.Port
	if port <= 0 {
		port = 3478
	}
	expiresAt := now.Add(ttl).UTC()
	username := fmt.Sprintf("%d:%s", expiresAt.Unix(), strings.TrimSpace(subject))
	mac := hmac.New(sha1.New, s.PrivateTURN.Secret)
	_, _ = mac.Write([]byte(username))
	credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	host := strings.TrimSpace(s.PrivateTURN.Host)
	urls := []string{
		fmt.Sprintf("turn:%s:%d?transport=udp", host, port),
		fmt.Sprintf("turn:%s:%d?transport=tcp", host, port),
	}
	// Chromium's WebRTC resolver can behave differently from URLSession when
	// resolving a private *.ts.net name. Keep the hostname for normal Tailnet
	// operation, and add resolved Tailnet IPv4 candidates when available so a
	// DNS handover cannot prevent ICE from reaching the same private TURN.
	if addresses, err := net.LookupIP(host); err == nil {
		for _, address := range addresses {
			ipv4 := address.To4()
			if ipv4 == nil || ipv4.String() == host {
				continue
			}
			urls = append(urls,
				fmt.Sprintf("turn:%s:%d?transport=udp", ipv4.String(), port),
				fmt.Sprintf("turn:%s:%d?transport=tcp", ipv4.String(), port),
			)
		}
	}
	return iceServerResponse{URLs: urls, Username: username, Credential: credential}, expiresAt, true
}

func (s *Server) persistCall(ctx context.Context, current call.Call, event call.Event) (int64, error) {
	s.lineMu.Lock()
	if current.EndedAt != nil {
		s.Line.ActiveCallID = nil
	} else {
		callID := modem.CallID(current.ID)
		s.Line.ActiveCallID = &callID
	}
	s.lineMu.Unlock()
	if s.Database == nil {
		return 0, nil
	}
	sequence, err := s.Database.SaveCall(ctx, db.Call{
		ID: current.ID, Direction: string(current.Direction), Peer: current.Peer, State: string(current.State),
		StartedAt: current.StartedAt, ConnectedAt: current.ConnectedAt, EndedAt: current.EndedAt, EndReason: current.EndReason,
	})
	if err != nil {
		return 0, err
	}
	if current.EndedAt != nil && s.Recordings != nil {
		if stopErr := s.Recordings.Stop(context.Background(), current.ID, current.EndReason); stopErr != nil && !errors.Is(stopErr, recording.ErrNotActive) {
			// Recording finalization must never make a completed call look failed.
			s.audit(context.Background(), "system", "recording.stop", current.ID, "failed")
		}
	}
	if s.Events != nil {
		s.Events.Publish(sequence, event.Kind, s.callResponseFromDomain(current))
	}
	return sequence, nil
}

func (s *Server) closeWebRTC(callID string) {
	s.webrtcMu.Lock()
	session := s.webrtcCalls[callID]
	bridge := s.voiceBridges[callID]
	delete(s.webrtcCalls, callID)
	delete(s.voiceBridges, callID)
	s.webrtcMu.Unlock()
	if bridge != nil {
		_ = bridge.Stop(context.Background())
	}
	if session != nil {
		_ = session.Close()
	}
}

// HandleModemEvent is the boundary between unsolicited serial events and the
// persisted call state machine. It is safe to call from the modem reader
// goroutine; all HTTP/event work is kept out of the serial parser.
func (s *Server) HandleModemEvent(event modem.ModemEvent) {
	if s.CallControl == nil {
		return
	}
	switch event.Kind {
	case "incoming":
		current, stateEvent, err := s.CallControl.Incoming(string(event.CallID), event.Peer)
		if err == nil {
			if _, persistErr := s.persistCall(context.Background(), current, stateEvent); persistErr == nil {
				s.sendIncomingCallPush(current)
			}
		}
	case "ended":
		current, stateEvent, err := s.CallControl.RemoteEnded(string(event.CallID), event.Raw)
		if err != nil {
			return
		}
		_, _ = s.persistCall(context.Background(), current, stateEvent)
		if ended, finishEvent, finishErr := s.CallControl.Finish(current.ID); finishErr == nil {
			_, _ = s.persistCall(context.Background(), ended, finishEvent)
			s.closeWebRTC(ended.ID)
		}
	}
}

func (s *Server) HandleMessage(message *db.Message) {
	if message == nil {
		return
	}
	if s.Events != nil {
		s.Events.Publish(message.SyncSeq, "message.created", s.messageResponseFromDB(*message))
	}
	s.sendMessagePush(*message)
}

func (s *Server) sendIncomingCallPush(current call.Call) {
	if s.Database == nil {
		slog.Warn("push skipped", "kind", "voip", "reason", "database unavailable", "error_code", "BLOCKED_EXTERNAL_APNS")
		return
	}
	if s.PushSender == nil {
		slog.Warn("push skipped", "kind", "voip", "reason", "sender unavailable", "error_code", "BLOCKED_EXTERNAL_APNS", "call_id", current.ID)
		return
	}
	if _, err := uuid.Parse(current.ID); err != nil {
		return
	}
	targets, err := s.Database.ListPushTargets(context.Background())
	if err != nil {
		slog.Error("push target lookup failed", "kind", "voip", "call_id", current.ID, "error", err)
		return
	}
	if len(targets) == 0 {
		slog.Warn("push skipped", "kind", "voip", "reason", "no registered APNs and VoIP targets", "call_id", current.ID)
		return
	}
	slog.Info("push dispatch started", "kind", "voip", "call_id", current.ID, "target_count", len(targets))
	for _, target := range targets {
		target := target
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := s.PushSender.SendVoIP(ctx, push.Target{DeviceID: target.DeviceID, APNSToken: target.APNSToken, VoIPToken: target.VoIPToken, Environment: target.Environment, Locale: target.Locale}, push.VoIPPayload{
				CallUUID: current.ID, CallID: current.ID, Handle: current.Peer, GatewayID: s.GatewayID, IssuedAt: time.Now().Unix(),
			})
			if err != nil {
				slog.Error("push dispatch failed", "kind", "voip", "device_id", target.DeviceID, "call_id", current.ID, "error", err)
				s.audit(context.Background(), target.DeviceID, "push.voip", current.ID, "failed")
				return
			}
			s.audit(context.Background(), target.DeviceID, "push.voip", current.ID, "accepted")
			slog.Info("push dispatch accepted", "kind", "voip", "device_id", target.DeviceID, "call_id", current.ID)
		}()
	}
}

// sendYakPhonePush fires the official YakPhone PushKit notification
// (push.yakteam.com/v1/notify) from the API server so the phone wakes
// for an incoming SMS even when YakPhone is suspended. Voice calls use
// the same endpoint through the SIP server.
func (s *Server) sendYakPhonePush(pushType, callerURI, messageBody string) {
	token := s.YakPushToken
	if token == "" {
		slog.Warn("yakpush skipped", "reason", "no push token configured", "type", pushType)
		return
	}
	body := map[string]string{
		"token":       token,
		"caller_uri":  callerURI,
		"caller_name": strings.TrimPrefix(callerURI, "sip:"),
		"type":        pushType,
	}
	if messageBody != "" {
		body["message_body"] = messageBody
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, "https://push.yakteam.com/v1/notify", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("yakpush failed", "type", pushType, "err", err)
		return
	}
	defer resp.Body.Close()
	slog.Info("yakpush sent", "type", pushType, "status", resp.Status)
}

func (s *Server) sendMessagePush(message db.Message) {
	// YakPhone official push (push.yakteam.com) takes precedence: it is
	// the only channel the final architecture uses for SMS
	// notifications (type=message). The legacy APNs Broker is kept as a
	// fallback for non-YakPhone targets.
	if s.YakPushToken != "" {
		s.sendYakPhonePush("message", "sip:"+message.Peer, message.Body)
	}
	if s.Database == nil {
		slog.Warn("push skipped", "kind", "message", "reason", "database unavailable", "error_code", "BLOCKED_EXTERNAL_APNS", "message_id", message.ID)
		return
	}
	if s.PushSender == nil {
		slog.Warn("push skipped", "kind", "message", "reason", "sender unavailable", "error_code", "BLOCKED_EXTERNAL_APNS", "message_id", message.ID)
		return
	}
	targets, err := s.Database.ListPushTargets(context.Background())
	if err != nil {
		slog.Error("push target lookup failed", "kind", "message", "message_id", message.ID, "error", err)
		return
	}
	if len(targets) == 0 {
		slog.Warn("push skipped", "kind", "message", "reason", "no registered APNs and VoIP targets", "message_id", message.ID)
		return
	}
	slog.Info("push dispatch started", "kind", "message", "message_id", message.ID, "target_count", len(targets))
	for _, target := range targets {
		target := target
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := s.PushSender.SendMessage(ctx, push.Target{DeviceID: target.DeviceID, APNSToken: target.APNSToken, VoIPToken: target.VoIPToken, Environment: target.Environment, Locale: target.Locale}, push.MessagePayload{MessageID: message.ID, GatewayID: s.GatewayID, IssuedAt: time.Now().Unix()})
			if err != nil {
				slog.Error("push dispatch failed", "kind", "message", "device_id", target.DeviceID, "message_id", message.ID, "error", err)
				s.audit(context.Background(), target.DeviceID, "push.message", message.ID, "failed")
				return
			}
			s.audit(context.Background(), target.DeviceID, "push.message", message.ID, "accepted")
			slog.Info("push dispatch accepted", "kind", "message", "device_id", target.DeviceID, "message_id", message.ID)
		}()
	}
}

func callResponseFromDomain(current call.Call) callResponse {
	response := callResponse{ID: current.ID, Direction: string(current.Direction), Peer: current.Peer, State: string(current.State), StartedAt: current.StartedAt.UnixMilli(), EndReason: current.EndReason}
	if current.ConnectedAt != nil {
		value := current.ConnectedAt.UnixMilli()
		response.ConnectedAt = &value
	}
	if current.EndedAt != nil {
		value := current.EndedAt.UnixMilli()
		response.EndedAt = &value
	}
	return response
}

func callResponseFromDB(current db.Call) callResponse {
	response := callResponse{ID: current.ID, Direction: current.Direction, Peer: current.Peer, State: current.State, StartedAt: current.StartedAt.UnixMilli(), EndReason: current.EndReason,
		RecordingID: current.RecordingID, RecordingState: current.RecordingState, RecordingDurationMs: current.RecordingDurationMs}
	if current.ConnectedAt != nil {
		value := current.ConnectedAt.UnixMilli()
		response.ConnectedAt = &value
	}
	if current.EndedAt != nil {
		value := current.EndedAt.UnixMilli()
		response.EndedAt = &value
	}
	return response
}

func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 {
		writeError(w, http.StatusBadRequest, "CB-API-002", "Idempotency-Key is required")
		return "", false
	}
	return key, true
}

func (s *Server) claimIdempotency(ctx context.Context, deviceID, operation, requestKey string) (bool, db.IdempotencyRecord, error) {
	if s.Database == nil {
		return true, db.IdempotencyRecord{}, nil
	}
	return s.Database.ClaimIdempotency(ctx, deviceID, operation, requestKey)
}

func replayIdempotency(w http.ResponseWriter, record db.IdempotencyRecord) bool {
	if record.Pending {
		writeError(w, http.StatusConflict, "CB-API-003", "request with this Idempotency-Key is still in progress")
		return true
	}
	writeRawJSON(w, record.StatusCode, record.Body)
	return true
}

func writeCallError(w http.ResponseWriter, err error) {
	status, code := callErrorStatus(err)
	writeError(w, status, code, err.Error())
}

func (s *Server) writeIdempotentCallError(w http.ResponseWriter, ctx context.Context, deviceID, operation, requestID string, err error) {
	status, code := callErrorStatus(err)
	body, marshalErr := json.Marshal(map[string]string{"code": code, "message": err.Error()})
	if marshalErr == nil && s.Database != nil {
		_ = s.Database.CompleteIdempotency(ctx, deviceID, operation, requestID, status, body)
	}
	if marshalErr != nil {
		writeError(w, http.StatusInternalServerError, "CB-API-004", marshalErr.Error())
		return
	}
	writeRawJSON(w, status, body)
}

func (s *Server) writeIdempotentError(w http.ResponseWriter, ctx context.Context, deviceID, operation, requestID string, status int, code, message string) {
	body, err := json.Marshal(map[string]string{"code": code, "message": message})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-API-004", err.Error())
		return
	}
	if s.Database != nil {
		_ = s.Database.CompleteIdempotency(ctx, deviceID, operation, requestID, status, body)
	}
	writeRawJSON(w, status, body)
}

func callErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, call.ErrLineBusy):
		return http.StatusConflict, "CB-CALL-009"
	case errors.Is(err, call.ErrCallNotFound):
		return http.StatusNotFound, "CB-CALL-006"
	case errors.Is(err, call.ErrInvalidDTMF), errors.Is(err, call.ErrInvalidState), errors.Is(err, call.ErrDuplicateEvent):
		return http.StatusConflict, "CB-CALL-010"
	default:
		return http.StatusBadGateway, "CB-CALL-003"
	}
}

func (s *Server) messageAction(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-SMS-000", "database unavailable")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/messages/")
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(parts) == 2 && parts[1] == "read" && r.Method == http.MethodPost {
		requestID, valid := idempotencyKey(w, r)
		if !valid {
			return
		}
		claimed, existing, err := s.claimIdempotency(r.Context(), deviceID, "message.read", requestID)
		if err != nil {
			s.writeIdempotentError(w, r.Context(), deviceID, "message.read", requestID, http.StatusInternalServerError, "CB-SMS-007", err.Error())
			return
		}
		if !claimed {
			replayIdempotency(w, existing)
			return
		}
		if err := s.Database.MarkMessageRead(r.Context(), parts[0]); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				_ = s.Database.CompleteIdempotency(r.Context(), deviceID, "message.read", requestID, http.StatusNotFound, []byte(`{"code":"CB-SMS-006","message":"message not found"}`))
				writeError(w, http.StatusNotFound, "CB-SMS-006", "message not found")
				return
			}
			s.writeIdempotentError(w, r.Context(), deviceID, "message.read", requestID, http.StatusInternalServerError, "CB-SMS-007", err.Error())
			return
		}
		if err := s.Database.CompleteIdempotency(r.Context(), deviceID, "message.read", requestID, http.StatusNoContent, nil); err != nil {
			writeError(w, http.StatusInternalServerError, "CB-SMS-007", err.Error())
			return
		}
		s.audit(r.Context(), deviceID, "message.read", parts[0], "accepted")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		requestID, valid := idempotencyKey(w, r)
		if !valid {
			return
		}
		claimed, existing, err := s.claimIdempotency(r.Context(), deviceID, "message.delete", requestID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "CB-SMS-007", err.Error())
			return
		}
		if !claimed {
			replayIdempotency(w, existing)
			return
		}
		if err := s.Database.DeleteMessage(r.Context(), parts[0]); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				_ = s.Database.CompleteIdempotency(r.Context(), deviceID, "message.delete", requestID, http.StatusNotFound, []byte(`{"code":"CB-SMS-006","message":"message not found"}`))
				writeError(w, http.StatusNotFound, "CB-SMS-006", "message not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "CB-SMS-007", err.Error())
			return
		}
		if err := s.Database.CompleteIdempotency(r.Context(), deviceID, "message.delete", requestID, http.StatusNoContent, nil); err != nil {
			writeError(w, http.StatusInternalServerError, "CB-SMS-007", err.Error())
			return
		}
		s.audit(r.Context(), deviceID, "message.delete", parts[0], "accepted")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeError(w, http.StatusNotFound, "CB-SMS-006", "message action not found")
}

type pushRegistrationRequest struct {
	APNSToken   string `json:"apnsToken"`
	VoIPToken   string `json:"voipToken"`
	Environment string `json:"environment"`
	Locale      string `json:"locale"`
}

func (s *Server) deviceAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "CB-API-001", "method not allowed")
		return
	}
	deviceID, ok := s.authorize(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/devices/")
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if len(parts) != 2 || parts[1] != "push" || parts[0] != deviceID {
		writeError(w, http.StatusForbidden, "CB-AUTH-003", "device cannot update this device")
		return
	}
	requestID, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	if s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-PUSH-000", "database unavailable")
		return
	}
	var request pushRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "CB-PUSH-001", "invalid JSON body")
		return
	}
	if request.APNSToken == "" || request.VoIPToken == "" || (request.Environment != "sandbox" && request.Environment != "production") || request.Locale == "" {
		writeError(w, http.StatusBadRequest, "CB-PUSH-002", "invalid push registration")
		return
	}
	claimed, existing, err := s.Database.ClaimIdempotency(r.Context(), deviceID, "push.register", requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-PUSH-003", err.Error())
		return
	}
	if !claimed {
		replayIdempotency(w, existing)
		return
	}
	if err := s.Database.RegisterPush(r.Context(), deviceID, request.APNSToken, request.VoIPToken, request.Environment, request.Locale); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.writeIdempotentError(w, r.Context(), deviceID, "push.register", requestID, http.StatusNotFound, "CB-PUSH-004", "device not found")
			return
		}
		s.writeIdempotentError(w, r.Context(), deviceID, "push.register", requestID, http.StatusInternalServerError, "CB-PUSH-003", err.Error())
		return
	}
	slog.Info("push registration accepted", "device_id", deviceID, "apns_token_bytes", len(request.APNSToken)/2, "voip_token_bytes", len(request.VoIPToken)/2, "environment", request.Environment)
	if err := s.Database.CompleteIdempotency(r.Context(), deviceID, "push.register", requestID, http.StatusNoContent, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "CB-PUSH-003", err.Error())
		return
	}
	s.audit(r.Context(), deviceID, "push.register", deviceID, "accepted")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "CB-API-001", "method not allowed")
		return
	}
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if s.Events == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-EVENT-000", "event stream unavailable")
		return
	}
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		CheckOrigin: func(request *http.Request) bool {
			origin := strings.TrimSpace(request.Header.Get("Origin"))
			if origin == "" {
				return true // Native iOS clients generally omit Origin.
			}
			parsed, parseErr := url.Parse(origin)
			return parseErr == nil && parsed.Host != "" && strings.EqualFold(parsed.Host, request.Host)
		},
	}
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	events, cancel := s.Events.Subscribe()
	defer cancel()
	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case event, open := <-events:
			if !open {
				return
			}
			if err := connection.WriteJSON(event); err != nil {
				return
			}
		case <-keepAlive.C:
			if err := connection.WriteControl(websocket.PingMessage, []byte("cellbridge"), time.Now().Add(5*time.Second)); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

type messageResponse struct {
	ID        string `json:"id"`
	GatewayID string `json:"gatewayID"`
	LineID    string `json:"lineID"`
	ThreadKey string `json:"threadKey"`
	Direction string `json:"direction"`
	Peer      string `json:"peer"`
	Body      string `json:"body"`
	Encoding  string `json:"encoding"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`
}

type threadResponse struct {
	Key         string          `json:"key"`
	Peer        string          `json:"peer"`
	UnreadCount int             `json:"unreadCount"`
	LastMessage messageResponse `json:"lastMessage"`
}

func messageResponseFromDB(message db.Message) messageResponse {
	return messageResponse{
		ID: message.ID, ThreadKey: message.ThreadKey, Direction: message.Direction, Peer: message.Peer,
		Body: message.Body, Encoding: message.Encoding, Status: message.Status, CreatedAt: message.CreatedAt.UnixMilli(),
	}
}

// Route metadata is added at the API boundary because this Gateway owns one
// modem line. Keeping it on every entity response makes the iOS cache safe
// when a Home Gateway and a PocketBridge contain the same contact/thread.
func (s *Server) routeMetadata() (string, string) {
	return s.GatewayID, s.GatewayID + ":line"
}

func (s *Server) messageResponseFromDB(message db.Message) messageResponse {
	response := messageResponseFromDB(message)
	response.GatewayID, response.LineID = s.routeMetadata()
	return response
}

func (s *Server) callResponseFromDomain(current call.Call) callResponse {
	response := callResponseFromDomain(current)
	response.GatewayID, response.LineID = s.routeMetadata()
	return response
}

func (s *Server) callResponseFromDB(current db.Call) callResponse {
	response := callResponseFromDB(current)
	response.GatewayID, response.LineID = s.routeMetadata()
	return response
}

func (s *Server) recordingResponseFromDB(current db.Recording) recordingResponse {
	response := recordingResponseFromDB(current)
	response.GatewayID, response.LineID = s.routeMetadata()
	return response
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.Auth == nil {
		return "", true
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(header, "Bearer ") {
		slog.Warn("api authorization missing", "path", r.URL.Path, "remote_addr", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "CB-AUTH-001", "missing bearer token")
		return "", false
	}
	deviceID, err := s.Auth.Authorize(r.Context(), strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
	if err != nil {
		slog.Warn("api authorization rejected", "path", r.URL.Path, "remote_addr", r.RemoteAddr, "error", err)
		writeError(w, http.StatusUnauthorized, "CB-AUTH-001", "invalid token")
		return "", false
	}
	return deviceID, true
}

func forwardedFor(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
}

func tailscaleUser(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("Tailscale-User-Login"))
}

func (s *Server) audit(ctx context.Context, deviceID, action, target, result string) {
	if s.Database != nil {
		_ = s.Database.AppendAudit(ctx, deviceID, action, target, result)
	}
}

func pagination(r *http.Request) (int64, int, error) {
	after := int64(0)
	limit := 100
	var err error
	if value := r.URL.Query().Get("after"); value != "" {
		after, err = strconv.ParseInt(value, 10, 64)
		if err != nil || after < 0 {
			return 0, 0, fmt.Errorf("after must be a non-negative integer")
		}
	}
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 500 {
			return 0, 0, fmt.Errorf("limit must be between 1 and 500")
		}
	}
	return after, limit, nil
}

func threadMessageCursor(r *http.Request) (*int64, string, error) {
	value := strings.TrimSpace(r.URL.Query().Get("before"))
	beforeID := strings.TrimSpace(r.URL.Query().Get("beforeId"))
	if value == "" {
		if beforeID != "" {
			return nil, "", fmt.Errorf("before is required when beforeId is provided")
		}
		return nil, "", nil
	}
	before, err := strconv.ParseInt(value, 10, 64)
	if err != nil || before < 0 {
		return nil, "", fmt.Errorf("before must be a non-negative integer")
	}
	return &before, beforeID, nil
}

func decodeBase64(value string) ([]byte, error) {
	if decoded, err := base64.RawStdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.StdEncoding.DecodeString(value)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}
