// Package qdc507 contains the Linux voice runtime adapter for the certified
// Baiwang QDC507 path. Device-side helpers and kernel modules stay outside
// the CellBridge image and are accepted only through a fixed hash allowlist.
package qdc507

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

const (
	SampleRate = 8000
	Channels   = 1

	remoteRuntimeDir     = "/run/maccellular-call"
	remoteBridge         = remoteRuntimeDir + "/mavo-pcm-bridge.armv7"
	remoteAPRModule      = remoteRuntimeDir + "/qdc507_aprv3.ko"
	remoteVoiceModule    = remoteRuntimeDir + "/qdc507_voice.ko"
	remoteRoutePIDFile   = "/run/celldock-voice-route.pid"
	remoteRouteLogFile   = "/run/celldock-voice-route.log"
	remoteCalibrationPID = "/run/celldock-alsaucm.pid"
	remoteCalibrationLog = "/run/celldock-alsaucm.log"
	expectedKernel       = "3.18.44"
	expectedCardName     = "mdm9607-tomtom-i2s-snd-card"
)

type runtimeArtifact struct {
	name string
	mode string
	hash string
}

var trustedRuntimeArtifacts = []runtimeArtifact{
	{name: "mavo-pcm-bridge.armv7", mode: "755", hash: "88d47c15e61d1428a59c821fed804c2e6490e82859a085062f21966b58d167fc"},
	{name: "qdc507_aprv3.ko", mode: "644", hash: "3d82d3dec4f1e323201bba87156df9d41438e08314097353f2607f9117211d4a"},
	{name: "qdc507_voice.ko", mode: "644", hash: "ed3821682d5309969a01c764192c83feff9669c61ef237c69475cd1619cf296c"},
}

// Config describes host-side wiring. TTY is used only to pin ADB to the
// physical USB module that owns the configured modem port.
type Config struct {
	TTY        string
	RuntimeDir string
	ADBPath    string
	Bootstrap  bool
}

// Runner is injectable so ADB framing and fail-closed behavior are testable.
type Runner interface {
	Run(context.Context, ...string) (string, error)
}

type commandRunner struct{ path string }

// adbServerSocket pins every adb invocation to a private server socket
// instead of the host's shared tcp:5037 daemon. After a NAS reboot the
// OS-level adb daemon can mis-enumerate a ghost "emulator-XXXX" host
// transport, which wedges the shared server (observed 2026-09-06: all
// dials frozen until `adb kill-server`). A dedicated socket keeps the
// QDC507 transport list deterministic regardless of host daemon state.
const adbServerSocket = "tcp:localhost:5038"

func (r commandRunner) Run(ctx context.Context, args ...string) (string, error) {
	if isADBServerInvocation(args) {
		// USB transport discovery is single-listener: the OS-level adb
		// daemon (tcp:5037, auto-started by fnOS on every boot) claims
		// the module's USB transport and a second daemon can never
		// enumerate it. Kill ONLY the host 5037 daemon — never our own
		// 5038 private daemon (a broad pattern like "adb.*fork-server"
		// matches both and causes a cold-start loop: each call kills our
		// daemon, start-server re-enumerates USB, >10s, deadline
		// exceeded. Observed 2026-09-06 reboot).
		cleanup := exec.CommandContext(ctx, "pkill", "-f", "[a]db.*tcp:localhost:5037")
		_ = cleanup.Run()
		cleanup2 := exec.CommandContext(ctx, "pkill", "-f", "[a]db.*nodaemon.*5037")
		_ = cleanup2.Run()
		startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
		startup := exec.CommandContext(startCtx, r.path, "start-server")
		startup.Env = append(startup.Env, "ADB_SERVER_SOCKET="+adbServerSocket)
		startup.Env = append(startup.Env, "HOME=/root")
		if _, err := startup.CombinedOutput(); err != nil {
			slog.Warn("adb start-server (private socket)", "error", err)
		}
		startCancel()
	}
	cmd := exec.CommandContext(ctx, r.path, args...)
	if isADBServerInvocation(args) {
		cmd.Env = append(cmd.Env, "ADB_SERVER_SOCKET="+adbServerSocket)
		cmd.Env = append(cmd.Env, "HOME=/root")
		// Pin the client to our private server via -L (no env-var
		// dependency, no ADB_TRACE flag which this adb build rejects).
		cmd.Args = append([]string{r.path, "-L", adbServerSocket}, args...)
	}
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// isADBServerInvocation reports whether args begin an adb client call that
// needs the private server socket (any argument form; the path is adb).
func isADBServerInvocation(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "devices", "shell", "push", "pull", "-t", "kill-server", "start-server", "wait-for-device":
		return true
	}
	return false
}

// Audio wraps host ALSA with the QDC507 module-side bootstrap. Bootstrap is
// attempted only when this backend is explicitly enabled in configuration.
type Audio struct {
	host modem.VoiceAudio
	cfg  Config
	run  Runner

	mu       sync.Mutex
	call     modem.CallID
	prepared      bool
	routePrepared bool
	once     sync.Once
}

func Open(host modem.VoiceAudio, cfg Config) (*Audio, error) {
	if host == nil {
		return nil, errors.New("QDC507 voice backend requires a host audio adapter")
	}
	if strings.TrimSpace(cfg.TTY) == "" {
		return nil, errors.New("QDC507 voice backend requires the configured modem TTY")
	}
	if strings.TrimSpace(cfg.RuntimeDir) == "" {
		return nil, errors.New("QDC507 voice backend requires an external runtime directory")
	}
	path := strings.TrimSpace(cfg.ADBPath)
	if path == "" {
		path = "adb"
	}
	return &Audio{host: host, cfg: cfg, run: commandRunner{path: path}}, nil
}

func OpenWithRunner(host modem.VoiceAudio, cfg Config, runner Runner) (*Audio, error) {
	audio, err := Open(host, cfg)
	if err != nil {
		return nil, err
	}
	if runner == nil {
		return nil, errors.New("QDC507 voice backend requires an ADB command runner")
	}
	audio.run = runner
	return audio, nil
}

func (a *Audio) Probe(ctx context.Context) (modem.AudioCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return modem.AudioCapabilities{}, err
	}
	if _, err := a.host.Probe(ctx); err != nil {
		return modem.AudioCapabilities{}, fmt.Errorf("probe host UAC audio: %w", err)
	}
	if !a.cfg.Bootstrap {
		return modem.AudioCapabilities{}, errors.New("QDC507 voice bootstrap is disabled; complete controlled module validation before enabling it")
	}
	if err := verifyRuntimeDirectory(a.cfg.RuntimeDir); err != nil {
		return modem.AudioCapabilities{}, err
	}
	transport, err := a.transportID(ctx)
	if err != nil {
		return modem.AudioCapabilities{}, err
	}
	if err := a.verifyTarget(ctx, transport); err != nil {
		return modem.AudioCapabilities{}, err
	}
	return modem.AudioCapabilities{Backend: "uac-qdc507", SampleRate: SampleRate, Channels: Channels}, nil
}

// routeAlive reports whether the remote VoLTE route is actually
// streaming right now: tracked mavo session alive + audio_enable + both
// hw:0,4 streams RUNNING. It never mutates state.
func (a *Audio) routeAlive(ctx context.Context) (bool, error) {
	transport, err := a.transportID(ctx)
	if err != nil {
		return false, err
	}
	alive, err := a.adb(ctx, transport, "shell", routeAliveScript())
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(alive) == "alive", nil
}

// PrepareRoute brings the remote VoLTE route up (calibration + mavo
// session) without opening the host capture/playback pair. The route is
// left RUNNING persistently once calibrated — the 2026-09-01 milestone
// that produced real audio kept mavo alive and audio_enable=1 across
// calls; killing and re-spawning it per call wedged the UAC stream into
// silence. A dead route (module reboot, crashed mavo) is rebuilt and
// re-verified here.
func (a *Audio) PrepareRoute(ctx context.Context) error {
	return a.ensureRuntime(ctx)
}

func (a *Audio) Start(ctx context.Context, callID modem.CallID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	if a.call != "" && a.call != callID {
		cur := a.call
		a.mu.Unlock()
		return fmt.Errorf("QDC507 audio is already active for call %s", cur)
	}
	a.mu.Unlock()
	if err := a.ensureRuntime(ctx); err != nil {
		return err
	}
	a.mu.Lock()
	a.call = callID
	a.mu.Unlock()
	return a.host.Start(ctx, callID)
}

func (a *Audio) ReadPCM(samples []int16) (int, error) { return a.host.ReadPCM(samples) }

func (a *Audio) WritePCM(samples []int16) (int, error) { return a.host.WritePCM(samples) }

func (a *Audio) Stop(ctx context.Context) error {
	a.mu.Lock()
	active := a.call != ""
	a.call = ""
	a.mu.Unlock()
	// Only the host UAC pair is torn down. The module-side VoLTE route
	// (mavo + audio_enable=1) stays RUNNING across calls — the
	// 2026-09-01 milestone with real audio kept it persistent; killing
	// it per call wedged the UAC stream into silence.
	if !active {
		return nil
	}
	return a.host.Stop(ctx)
}

func (a *Audio) Close() error {
	var err error
	a.once.Do(func() {
		err = a.Stop(context.Background())
		if closeErr := a.host.Close(); err == nil {
			err = closeErr
		}
	})
	return err
}

func (a *Audio) ensureRuntime(ctx context.Context) error {
	if !a.cfg.Bootstrap {
		return errors.New("QDC507 voice bootstrap is disabled")
	}
	a.mu.Lock()
	prepared := a.prepared
	a.mu.Unlock()
	if prepared {
		// Per-call route rotation (2026-09-06 findings): the QDC507 UAC
		// gadget's ASYNC capture stream dies after aplay/arecord have
		// touched it once. A fresh mavo voice-route session restores it.
		// The 09:01 milestone that first produced real audio (peak 4586)
		// had just re-created the route; every silent call followed a
		// reused session. So each call explicitly re-rotates the route —
		// never trust a cached "alive" session.
		slog.Info("QDC507 rotating voice route for call")
		a.mu.Lock()
		a.prepared = false
		a.mu.Unlock()
		return a.startRemoteRoute(ctx)
	}
	if err := verifyRuntimeDirectory(a.cfg.RuntimeDir); err != nil {
		return err
	}
	transport, err := a.transportID(ctx)
	if err != nil {
		return err
	}
	if err := a.verifyTarget(ctx, transport); err != nil {
		return err
	}
	if _, err := a.adb(ctx, transport, "shell", "mkdir -p "+remoteRuntimeDir); err != nil {
		return fmt.Errorf("create QDC507 runtime directory: %w", err)
	}
	for _, artifact := range trustedRuntimeArtifacts {
		local := filepath.Join(a.cfg.RuntimeDir, artifact.name)
		if _, err := a.adb(ctx, transport, "push", local, remoteRuntimeDir+"/"+artifact.name); err != nil {
			return fmt.Errorf("push QDC507 runtime %s: %w", artifact.name, err)
		}
		if _, err := a.adb(ctx, transport, "shell", "chmod "+artifact.mode+" "+remoteRuntimeDir+"/"+artifact.name); err != nil {
			return fmt.Errorf("protect QDC507 runtime %s: %w", artifact.name, err)
		}
	}
	if _, err := a.adb(ctx, transport, "shell", "test -d /sys/module/qdc507_aprv3 || insmod "+remoteAPRModule); err != nil {
		return fmt.Errorf("load QDC507 APR module: %w", err)
	}
	if _, err := a.adb(ctx, transport, "shell", "test -d /sys/module/qdc507_voice || insmod "+remoteVoiceModule); err != nil {
		return fmt.Errorf("load QDC507 voice module: %w", err)
	}
	// After a NAS reboot the module's USB stack re-enumerates; the ALSA
	// PCM nodes may lag the ADB transport by seconds. Retry the device
	// check rather than failing the first dial (observed 2026-09-06:
	// fresh-boot first call → "PCM devices unavailable", then a stuck
	// route rotate; a gateway restart hid the same race).
	var ready string
	for attempt := 0; attempt < 5; attempt++ {
		ready, err = a.adb(ctx, transport, "shell", "test -e /dev/snd/pcmC0D4p && test -e /dev/snd/pcmC0D4c && test -e /dev/snd/pcmC0D5p && test -e /dev/snd/pcmC0D6c && echo ready")
		if err == nil && strings.TrimSpace(ready) == "ready" {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err != nil || strings.TrimSpace(ready) != "ready" {
		return fmt.Errorf("QDC507 VoLTE PCM devices unavailable: %w (%s)", err, ready)
	}
	card, err := a.adb(ctx, transport, "shell", "cat /proc/asound/cards")
	if err != nil || !strings.Contains(card, expectedCardName) {
		return fmt.Errorf("QDC507 ALSA card %q is unavailable: %w (%s)", expectedCardName, err, card)
	}
	if err := a.ensureCalibration(ctx, transport); err != nil {
		return err
	}
	endpoints, err := a.adb(ctx, transport, "shell", "test -c /dev/ttyGS0 && test -p /run/voc_svr && echo ready")
	if err != nil || strings.TrimSpace(endpoints) != "ready" {
		return fmt.Errorf("QDC507 voice endpoints unavailable: %w (%s)", err, endpoints)
	}
	a.mu.Lock()
	a.prepared = true
	a.mu.Unlock()
	return a.startRemoteRoute(ctx)
}

func (a *Audio) ensureCalibration(ctx context.Context, transport string) error {
	script := `owned=0; if test -s /run/celldock-alsaucm.pid; then read pid expected_start < /run/celldock-alsaucm.pid || true; current_start=$(cut -d ' ' -f 22 /proc/$pid/stat 2>/dev/null); argv0=$(tr '\000' '\n' < /proc/$pid/cmdline 2>/dev/null | sed -n '1p'); test "$current_start" = "$expected_start" && test "$argv0" = /usr/bin/alsaucm_test && owned=1 || true; fi; if test "$owned" -eq 0; then rm -f /run/alsaucm_test /run/celldock-alsaucm.pid /run/celldock-alsaucm.log; nohup /usr/bin/alsaucm_test </dev/null >> /run/celldock-alsaucm.log 2>&1 & pid=$!; starttime=$(cut -d ' ' -f 22 /proc/$pid/stat 2>/dev/null); printf '%s %s\n' "$pid" "$starttime" > /run/celldock-alsaucm.pid; n=0; while test "$n" -lt 50 && test ! -p /run/alsaucm_test; do kill -0 "$pid" 2>/dev/null || exit 72; sleep 0.1; n=$((n+1)); done; test -p /run/alsaucm_test || exit 73; fi; if ! grep -q 'ACDB -> Sent VocProc Cal!' /run/celldock-alsaucm.log 2>/dev/null; then printf 'open snd_soc_msm_9x07_Tomtom_I2S\n' > /run/alsaucm_test; printf 'set _verb VoLTE\n' > /run/alsaucm_test; printf 'set _enadev Auxpcm Rx\n' > /run/alsaucm_test; printf 'set _enadev Auxpcm Tx\n' > /run/alsaucm_test; n=0; while test "$n" -lt 100; do grep -q 'ACDB -> Sent VocProc Cal!' /run/celldock-alsaucm.log 2>/dev/null && break; sleep 0.1; n=$((n+1)); done; fi; grep -q 'ACDB -> Sent VocProc Cal!' /run/celldock-alsaucm.log`
	output, err := a.adb(ctx, transport, "shell", script)
	if err != nil {
		return fmt.Errorf("QDC507 VoLTE calibration unavailable: %w (%s)", err, output)
	}
	return nil
}

// writeModuleScript stores a shell script on the module via base64 over
// adb shell, avoiding host-path coupling for adb push.
func (a *Audio) writeModuleScript(ctx context.Context, path, content string) error {
	transport, err := a.transportID(ctx)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	if _, err := a.adb(ctx, transport, "shell", "echo "+encoded+" | base64 -d > "+path); err != nil {
		return err
	}
	_, _ = a.adb(ctx, transport, "shell", "chmod 700 "+path)
	return nil
}

func (a *Audio) startRemoteRoute(ctx context.Context) error {
	transport, err := a.transportID(ctx)
	if err != nil {
		return err
	}
	// Always rotate: kill every mavo session and start one fresh. The
	// QDC507 UAC capture stream dies once the host pair has touched the
	// gadget; only a new voice-route session revives it (2026-09-06).
	// killall without -9 was leaving survivors (observed: 3 mavo PIDs
	// alive after rotate, audio_enable toggled to 0 by one of them,
	// every later call silent) — use -9 and verify zero survivors.
	for attempt := 0; attempt < 3; attempt++ {
		_, _ = a.adb(ctx, transport, "shell", "killall -9 mavo-pcm-bridge.armv7 2>/dev/null; sleep 0.5")
		_ = a.stopRemoteRoute(ctx)
		out, err := a.adb(ctx, transport, "shell", "pgrep -f [m]avo-pcm-bridge.armv7 | wc -l")
		if err == nil && strings.TrimSpace(out) == "0" {
			break
		}
		slog.Warn("QDC507 route rotate: mavo survivors, retry", "survivors", strings.TrimSpace(out))
	}
	_ = a.stopRemoteRoute(ctx)
	// Launch the route session through a script file on the module.
	// Inline `case \"$pid...\"` quoting breaks across the adb shell
	// boundary (the pid file never gets written); the file form is what
	// was verified working on the module directly.
	launcher := "#!/system/bin/sh\n" +
		"rm -f " + remoteRoutePIDFile + " " + remoteRouteLogFile + "\n" +
		"nohup " + remoteBridge + " --voice-route-session --verbose </dev/null >> " + remoteRouteLogFile + " 2>&1 &\n" +
		"pid=$!\n" +
		"starttime=$(cut -d ' ' -f 22 /proc/$pid/stat 2>/dev/null)\n" +
		"printf '%s %s\\n' \"$pid\" \"$starttime\" > " + remoteRoutePIDFile + "\n"
	if err := a.writeModuleScript(ctx, "/data/celldock-route-launch.sh", launcher); err != nil {
		return fmt.Errorf("stage QDC507 route launcher: %w", err)
	}
	if _, err := a.adb(ctx, transport, "shell", "sh /data/celldock-route-launch.sh"); err != nil {
		return fmt.Errorf("start QDC507 D4/UAC route: %w", err)
	}
	for attempt := 0; attempt < 40; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, _ := a.adb(ctx, transport, "shell", routeReadyScript())
		if strings.TrimSpace(ready) == "ready" {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	logText, _ := a.adb(ctx, transport, "shell", "tail -n 40 "+remoteRouteLogFile+" 2>/dev/null")
	return fmt.Errorf("QDC507 D4/UAC route did not enter RUNNING: %s", logText)
}

func (a *Audio) stopRemoteRoute(ctx context.Context) error {
	transport, err := a.transportID(ctx)
	if err != nil {
		return err
	}
	script := "if test -s " + remoteRoutePIDFile + "; then read pid expected_start < " + remoteRoutePIDFile + " || exit 70; current_start=$(cut -d ' ' -f 22 /proc/$pid/stat 2>/dev/null); argv0=$(tr '\\000' '\\n' < /proc/$pid/cmdline 2>/dev/null | sed -n '1p'); arg1=$(tr '\\000' '\\n' < /proc/$pid/cmdline 2>/dev/null | sed -n '2p'); if test \"$current_start\" = \"$expected_start\" && test \"$argv0\" = \"" + remoteBridge + "\" && test \"$arg1\" = --voice-route-session; then kill -TERM $pid 2>/dev/null || true; n=0; while kill -0 $pid 2>/dev/null && test $n -lt 50; do sleep 0.1; n=$((n+1)); done; kill -0 $pid 2>/dev/null && exit 71 || true; fi; fi; rm -f " + remoteRoutePIDFile
	output, err := a.adb(ctx, transport, "shell", script)
	if err != nil {
		return fmt.Errorf("stop QDC507 D4/UAC route: %w (%s)", err, output)
	}
	return nil
}

// routeAliveScript checks the live route state without pid-file trust:
// any mavo session owning hw:0,4, audio_enable=1, and both hw:0,4
// substreams RUNNING.
func routeAliveScript() string {
	return "ps | grep -q mavo-pcm-bridge && test \"$(cat /sys/class/android_usb/f_audio/audio_enable 2>/dev/null)\" = 1 && grep -q '^state: RUNNING' /proc/asound/card0/pcm4p/sub0/status && grep -q '^state: RUNNING' /proc/asound/card0/pcm4c/sub0/status && echo alive"
}

func routeReadyScript() string {
	return "test -s " + remoteRoutePIDFile + " && read pid expected_start < " + remoteRoutePIDFile + " && test \"$(cut -d ' ' -f 22 /proc/$pid/stat 2>/dev/null)\" = \"$expected_start\" && test \"$(tr '\\000' '\\n' < /proc/$pid/cmdline 2>/dev/null | sed -n '1p')\" = \"" + remoteBridge + "\" && tr '\\000' '\\n' < /proc/$pid/cmdline 2>/dev/null | grep -q '^--voice-route-session$' && grep -q 'VoLTE route session active on hw:0,4' " + remoteRouteLogFile + " && test \"$(cat /sys/class/android_usb/f_audio/audio_enable 2>/dev/null)\" = 1 && grep -q '^state: RUNNING' /proc/asound/card0/pcm4p/sub0/status && grep -q '^state: RUNNING' /proc/asound/card0/pcm4c/sub0/status && echo ready"
}

func (a *Audio) verifyTarget(ctx context.Context, transport string) error {
	uid, err := a.adb(ctx, transport, "shell", "id -u")
	if err != nil || strings.TrimSpace(uid) != "0" {
		return fmt.Errorf("QDC507 ADB root unavailable: %w (%s)", err, uid)
	}
	kernel, err := a.adb(ctx, transport, "shell", "uname -r")
	if err != nil || strings.TrimSpace(kernel) != expectedKernel {
		return fmt.Errorf("QDC507 kernel mismatch: want %s, got %q", expectedKernel, strings.TrimSpace(kernel))
	}
	return nil
}

func (a *Audio) transportID(ctx context.Context) (string, error) {
	resolved, err := filepath.EvalSymlinks(a.cfg.TTY)
	if err != nil {
		return "", fmt.Errorf("resolve QDC507 TTY: %w", err)
	}
	devicePath := filepath.Join("/sys/class/tty", filepath.Base(resolved), "device")
	sysfsPath, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return "", fmt.Errorf("resolve QDC507 USB topology: %w", err)
	}
	usbPath := ""
	for _, component := range strings.Split(filepath.ToSlash(sysfsPath), "/") {
		if strings.Contains(component, "-") && strings.Contains(component, ":") && component[0] >= '0' && component[0] <= '9' {
			usbPath = strings.SplitN(component, ":", 2)[0]
			break
		}
	}
	if usbPath == "" {
		return "", fmt.Errorf("QDC507 USB parent not found in %s", sysfsPath)
	}
	// The private adb daemon (ADB_SERVER_SOCKET) starts async to the
	// module's first enumeration; retry briefly so a fresh-boot probe
	// doesn't degrade voice to control-only (observed 2026-09-06).
	var transport string
	for attempt := 0; attempt < 8; attempt++ {
		devices, lerr := a.run.Run(ctx, "devices", "-l")
		if lerr != nil {
			return "", fmt.Errorf("list ADB devices for USB path %s: %w (%s)", usbPath, lerr, devices)
		}
		transport, err = SelectTransportID(devices, usbPath)
		if err == nil {
			return transport, nil
		}
		if ctx.Err() != nil {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(750 * time.Millisecond):
		}
	}
	return "", err
}

func (a *Audio) adb(ctx context.Context, transport string, args ...string) (string, error) {
	pinned := append([]string{"-t", transport}, args...)
	return a.run.Run(ctx, pinned...)
}

func verifyRuntimeDirectory(directory string) error {
	directory = filepath.Clean(strings.TrimSpace(directory))
	if directory == "." || directory == "" || !filepath.IsAbs(directory) {
		return errors.New("QDC507 runtime directory must be an absolute path")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("QDC507 runtime directory is unavailable: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return errors.New("QDC507 runtime directory must be a private real directory")
	}
	for _, artifact := range trustedRuntimeArtifacts {
		path := filepath.Join(directory, artifact.name)
		fileInfo, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("QDC507 runtime artifact %s is unavailable: %w", artifact.name, err)
		}
		if !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 || fileInfo.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("QDC507 runtime artifact %s is not a private regular file", artifact.name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read QDC507 runtime artifact %s: %w", artifact.name, err)
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != artifact.hash {
			return fmt.Errorf("QDC507 runtime artifact %s failed fixed SHA-256 verification", artifact.name)
		}
	}
	return nil
}

// SelectTransportID accepts only one ready serial-less ADB target with the
// exact physical USB path, preventing a second Android/ADB device from being
// mistaken for the modem.
func SelectTransportID(devices, usbPath string) (string, error) {
	usbPath = strings.TrimSpace(usbPath)
	if usbPath == "" {
		return "", errors.New("empty QDC507 USB path")
	}
	var result string
	for _, line := range strings.Split(devices, "\n") {
		fields := strings.Fields(line)
		statusIndex := -1
		for index, field := range fields {
			if field == "device" {
				statusIndex = index
				break
			}
		}
		if statusIndex < 0 {
			continue
		}
		matched := false
		transport := ""
		for _, field := range fields[statusIndex+1:] {
			switch {
			case strings.HasPrefix(field, "usb:") && strings.TrimPrefix(field, "usb:") == usbPath:
				matched = true
			case strings.HasPrefix(field, "transport_id:"):
				transport = strings.TrimPrefix(field, "transport_id:")
			}
		}
		if matched && transport != "" {
			if result != "" {
				return "", fmt.Errorf("multiple ready ADB devices match USB path %s", usbPath)
			}
			result = transport
		}
	}
	if result == "" {
		return "", fmt.Errorf("no ready ADB device matches USB path %s", usbPath)
	}
	return result, nil
}

var _ modem.VoiceAudio = (*Audio)(nil)
