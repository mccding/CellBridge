package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/config"
	"github.com/cellbridge/cellbridge/gateway/internal/db"
	"github.com/cellbridge/cellbridge/gateway/internal/modem/discovery"
	"github.com/pion/ice/v4"
	"github.com/pion/stun/v3"
	"gopkg.in/yaml.v3"
)

// cellbridge-admin contains operator actions that are intentionally separate
// from the public Gateway API. It never exports access tokens, refresh tokens,
// APNs tokens, message bodies, IMSI/ICCID, or raw modem logs.
func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx := context.Background()
	var err error
	switch {
	case os.Args[1] == "security-probe":
		err = securityProbe(ctx, os.Args[2:])
	case len(os.Args) >= 3 && os.Args[1] == "diagnostics" && os.Args[2] == "export":
		err = diagnostics(ctx, os.Args[3:])
	case os.Args[1] == "backup":
		err = backup(ctx, os.Args[2:])
	case os.Args[1] == "restore":
		err = restore(os.Args[2:])
	case os.Args[1] == "device-revoke":
		err = revokeDevice(ctx, os.Args[2:])
	case os.Args[1] == "turn-test":
		err = turnTest(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cellbridge-admin:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  cellbridge-admin security-probe [--config FILE]
  cellbridge-admin diagnostics export [--data-dir DIR] [--dev-root DIR] [--config FILE] --output FILE
  cellbridge-admin backup --data-dir DIR --output FILE [--config FILE] [--include-recordings]
  cellbridge-admin restore --data-dir DIR --input FILE --confirm-restore
  cellbridge-admin device-revoke --data-dir DIR --device-id DEVICE --confirm-revoke
  cellbridge-admin turn-test --server turn:host:3478 --username USER --credential SECRET`)
}

func securityProbe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("security-probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "/etc/cellbridge/config.yaml", "Gateway configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	settings, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ssOutput, err := commandOutput(ctx, "ss", "-lntup")
	if err != nil {
		return fmt.Errorf("read listening sockets: %w", err)
	}
	tailscalePath, err := exec.LookPath("tailscale")
	if err != nil {
		return fmt.Errorf("tailscale is required: %w", err)
	}
	serveOutput, serveErr := commandOutput(ctx, tailscalePath, "serve", "status")
	funnelOutput, funnelErr := commandOutput(ctx, tailscalePath, "funnel", "status")
	statusOutput, statusErr := commandOutput(ctx, tailscalePath, "status", "--json")

	checks := []struct {
		name string
		pass bool
	}{
		{"Network Mode: TAILNET ONLY", settings.Network.Mode == "tailnet" && !settings.Network.PublicFallback},
		{"Gateway Public Exposure: NONE", !hasNonLoopbackListener(ssOutput, "8787")},
		{"ADB Public Exposure: NONE", !hasNonLoopbackListener(ssOutput, "5037")},
		{"TURN Public Exposure: NONE", !hasNonTailnetListener(ssOutput, "3478")},
		{"Funnel: DISABLED", funnelErr == nil && !funnelIsEnabled(funnelOutput)},
		{"Gateway Loopback Bind: PASS", isLoopbackBind(settings.Server.Listen)},
		{"Tailscale Authenticated: PASS", statusErr == nil && strings.Contains(statusOutput, `"BackendState": "Running"`)},
		{"Serve HTTPS Tailnet-only: PASS", serveErr == nil && strings.Contains(strings.ToLower(serveOutput), "tailnet only") && strings.Contains(serveOutput, "127.0.0.1:8787")},
	}
	if settings.WebRTC.PrivateTURN.Enabled {
		checks = append(checks, struct {
			name string
			pass bool
		}{"Private TURN Bind: PASS", hasTailnetListener(ssOutput, "3478")})
	} else {
		checks = append(checks, struct {
			name string
			pass bool
		}{"Private TURN Bind: NOT_CONFIGURED", true})
	}

	failed := false
	for _, check := range checks {
		state := "FAIL"
		if check.pass {
			state = "PASS"
		}
		fmt.Printf("%s [%s]\n", check.name, state)
		failed = failed || !check.pass
	}
	if failed {
		return errors.New("security probe failed")
	}
	return nil
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(output), err
	}
	return string(output), nil
}

func isLoopbackBind(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hasNonLoopbackListener(output, port string) bool {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, ":"+port) {
			continue
		}
		if strings.Contains(line, "127.0.0.1:"+port) || strings.Contains(line, "[::1]:"+port) {
			continue
		}
		return true
	}
	return false
}

func hasNonTailnetListener(output, port string) bool {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, ":"+port) {
			continue
		}
		if strings.Contains(line, "100.") || strings.Contains(line, "fd7a:") {
			continue
		}
		return true
	}
	return false
}

func hasTailnetListener(output, port string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, ":"+port) && (strings.Contains(line, "100.") || strings.Contains(line, "fd7a:")) {
			return true
		}
	}
	return false
}

func funnelIsEnabled(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "(funnel") || strings.Contains(lower, "funnel on") || strings.Contains(lower, "public")
}

func revokeDevice(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("device-revoke", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", "/var/lib/cellbridge", "Gateway data directory")
	deviceID := flags.String("device-id", "", "device identifier")
	confirmed := flags.Bool("confirm-revoke", false, "required confirmation for revoking the device")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*deviceID) == "" || !*confirmed {
		return errors.New("device-revoke requires --device-id and --confirm-revoke")
	}
	database, err := db.Open(filepath.Join(*dataDir, "cellbridge.sqlite"))
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		return err
	}
	if err := database.RevokeDevice(ctx, strings.TrimSpace(*deviceID)); err != nil {
		return fmt.Errorf("revoke device: %w", err)
	}
	fmt.Printf("{\"status\":\"ok\",\"deviceId\":%q,\"revoked\":true}\n", strings.TrimSpace(*deviceID))
	return nil
}

func turnTest(args []string) error {
	flags := flag.NewFlagSet("turn-test", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	server := flags.String("server", "", "TURN URI, for example turn:turn.example.com:3478")
	username := flags.String("username", "", "TURN username")
	credential := flags.String("credential", "", "TURN credential")
	timeout := flags.Duration("timeout", 10*time.Second, "candidate gathering timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *server == "" || *username == "" || *credential == "" {
		return errors.New("turn-test requires --server, --username, and --credential")
	}
	uri, err := stun.ParseURI(*server)
	if err != nil {
		return fmt.Errorf("parse TURN URI: %w", err)
	}
	if uri.Scheme != stun.SchemeTypeTURN && uri.Scheme != stun.SchemeTypeTURNS {
		return errors.New("--server must use turn: or turns:")
	}
	uri.Username = *username
	uri.Password = *credential
	agent, err := ice.NewAgent(&ice.AgentConfig{
		Urls:           []*stun.URI{uri},
		CandidateTypes: []ice.CandidateType{ice.CandidateTypeRelay},
	})
	if err != nil {
		return fmt.Errorf("create TURN test agent: %w", err)
	}
	defer agent.Close()
	var relayFound bool
	var relayMu sync.Mutex
	var finish sync.Once
	gathered := make(chan struct{})
	if err := agent.OnCandidate(func(candidate ice.Candidate) {
		if candidate == nil {
			finish.Do(func() { close(gathered) })
			return
		}
		if candidate.Type() == ice.CandidateTypeRelay {
			relayMu.Lock()
			relayFound = true
			relayMu.Unlock()
		}
	}); err != nil {
		return fmt.Errorf("register TURN candidate handler: %w", err)
	}
	if err := agent.GatherCandidates(); err != nil {
		return fmt.Errorf("TURN candidate gathering failed to start: %w", err)
	}
	deadline := time.NewTimer(*timeout)
	defer deadline.Stop()
	select {
	case <-gathered:
	case <-deadline.C:
		return errors.New("TURN candidate gathering timed out")
	}
	relayMu.Lock()
	hasRelay := relayFound
	relayMu.Unlock()
	if !hasRelay {
		return errors.New("TURN responded but no relay candidate was gathered")
	}
	fmt.Println(`{"status":"ok","relayCandidate":true}`)
	return nil
}

func diagnostics(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("diagnostics export", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", "/var/lib/cellbridge", "Gateway data directory")
	devRoot := flags.String("dev-root", "/dev", "device root used for read-only probe enumeration")
	configPath := flags.String("config", "", "optional YAML configuration path")
	output := flags.String("output", "cellbridge-diagnostics.zip", "diagnostic zip path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*output) == "" {
		return errors.New("--output is required")
	}

	temporary, err := os.MkdirTemp("", "cellbridge-diagnostics-")
	if err != nil {
		return fmt.Errorf("create temporary diagnostic directory: %w", err)
	}
	defer os.RemoveAll(temporary)

	configValue := config.Default()
	if *configPath != "" {
		configValue, err = config.Load(*configPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
	}
	reports, err := discovery.Enumerate(*devRoot)
	if err != nil {
		return fmt.Errorf("enumerate modem candidates: %w", err)
	}
	if err := writeJSON(filepath.Join(temporary, "version.json"), map[string]any{
		"component": "cellbridge-admin",
		"runtime":   runtime.Version(),
		"goos":      runtime.GOOS,
		"goarch":    runtime.GOARCH,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(temporary, "probe.json"), reports); err != nil {
		return err
	}

	health := map[string]any{"status": "ok", "database": "not_checked"}
	databasePath := filepath.Join(*dataDir, "cellbridge.sqlite")
	if _, statErr := os.Stat(databasePath); statErr == nil {
		database, openErr := db.Open(databasePath)
		if openErr != nil {
			health = map[string]any{"status": "degraded", "database": "open_failed"}
		} else {
			defer database.Close()
			var one int
			if queryErr := database.QueryRowContext(ctx, "SELECT 1").Scan(&one); queryErr != nil {
				health = map[string]any{"status": "degraded", "database": "query_failed"}
			} else {
				health["database"] = "ok"
			}
		}
	} else if !os.IsNotExist(statErr) {
		health = map[string]any{"status": "degraded", "database": "stat_failed"}
	}
	if err := writeJSON(filepath.Join(temporary, "health.json"), health); err != nil {
		return err
	}

	interfaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("list network interfaces: %w", err)
	}
	network := make([]map[string]any, 0, len(interfaces))
	for _, iface := range interfaces {
		network = append(network, map[string]any{"name": iface.Name, "up": iface.Flags&net.FlagUp != 0, "loopback": iface.Flags&net.FlagLoopback != 0})
	}
	sort.Slice(network, func(i, j int) bool { return network[i]["name"].(string) < network[j]["name"].(string) })
	if err := writeJSON(filepath.Join(temporary, "network.json"), network); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(temporary, "turn-test.json"), map[string]any{
		"status":  "not_run",
		"reason":  "TURN endpoint and credentials are deployment-specific; run the configured external turn-test during NAS acceptance",
		"checked": false,
	}); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(temporary, "recent-redacted.log"), []byte("diagnostic export intentionally excludes raw logs and message content\n"), 0o640); err != nil {
		return fmt.Errorf("write redacted log: %w", err)
	}
	if err := writeYAML(filepath.Join(temporary, "config-redacted.yaml"), safeConfig(configValue)); err != nil {
		return err
	}
	return zipDirectory(temporary, *output)
}

func backup(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", "/var/lib/cellbridge", "Gateway data directory")
	configPath := flags.String("config", "", "optional public YAML configuration path")
	output := flags.String("output", "", "backup zip path")
	includeRecordings := flags.Bool("include-recordings", false, "include recordings in the archive")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" {
		return errors.New("backup requires --output")
	}
	sourceDB := filepath.Join(*dataDir, "cellbridge.sqlite")
	if _, err := os.Stat(sourceDB); err != nil {
		return fmt.Errorf("database is unavailable: %w", err)
	}
	temporary, err := os.MkdirTemp("", "cellbridge-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	database, err := db.Open(sourceDB)
	if err != nil {
		return err
	}
	defer database.Close()
	consistentDB := filepath.Join(temporary, "db.sqlite")
	if _, err := database.ExecContext(ctx, "VACUUM INTO ?", consistentDB); err != nil {
		return fmt.Errorf("create consistent database copy: %w", err)
	}
	if err := exportDevicePublicKeys(ctx, database, filepath.Join(temporary, "device-public-keys.json")); err != nil {
		return err
	}
	publicConfig := config.Default()
	if *configPath != "" {
		publicConfig, err = config.Load(*configPath)
		if err != nil {
			return err
		}
	}
	if err := writeYAML(filepath.Join(temporary, "config-public.yaml"), safeConfig(publicConfig)); err != nil {
		return err
	}
	if *includeRecordings {
		if err := copyTree(filepath.Join(*dataDir, "recordings"), filepath.Join(temporary, "recordings")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return zipDirectory(temporary, *output)
}

func restore(args []string) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data-dir", "/var/lib/cellbridge", "Gateway data directory")
	input := flags.String("input", "", "backup zip path")
	confirmed := flags.Bool("confirm-restore", false, "required confirmation for replacing the database")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" || !*confirmed {
		return errors.New("restore requires --input and --confirm-restore")
	}
	archive, err := zip.OpenReader(*input)
	if err != nil {
		return fmt.Errorf("open backup: %w", err)
	}
	defer archive.Close()
	temporary, err := os.MkdirTemp("", "cellbridge-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	for _, entry := range archive.File {
		if !allowedArchivePath(entry.Name) {
			return fmt.Errorf("backup contains unsupported path %q", entry.Name)
		}
		destination := filepath.Join(temporary, filepath.FromSlash(entry.Name))
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(destination, 0o750); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
			return err
		}
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		output, createErr := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
		if createErr == nil {
			_, createErr = io.Copy(output, reader)
		}
		_ = output.Close()
		_ = reader.Close()
		if createErr != nil {
			return createErr
		}
	}
	if _, err := os.Stat(filepath.Join(temporary, "db.sqlite")); err != nil {
		return errors.New("backup has no db.sqlite")
	}
	if err := os.MkdirAll(*dataDir, 0o750); err != nil {
		return err
	}
	destinationDB := filepath.Join(*dataDir, "cellbridge.sqlite")
	if _, err := os.Stat(destinationDB); err == nil {
		backupPath := destinationDB + ".pre-restore-" + time.Now().UTC().Format("20060102T150405Z")
		if err := os.Rename(destinationDB, backupPath); err != nil {
			return fmt.Errorf("preserve current database: %w", err)
		}
	}
	if err := copyFile(filepath.Join(temporary, "db.sqlite"), destinationDB, 0o640); err != nil {
		return err
	}
	if sourceConfig := filepath.Join(temporary, "config-public.yaml"); fileExists(sourceConfig) {
		if err := copyFile(sourceConfig, filepath.Join(*dataDir, "config-public.yaml"), 0o640); err != nil {
			return err
		}
	}
	if sourceRecordings := filepath.Join(temporary, "recordings"); fileExists(sourceRecordings) {
		destinationRecordings := filepath.Join(*dataDir, "recordings")
		if fileExists(destinationRecordings) {
			preserved := destinationRecordings + ".pre-restore-" + time.Now().UTC().Format("20060102T150405Z")
			if err := os.Rename(destinationRecordings, preserved); err != nil {
				return fmt.Errorf("preserve current recordings: %w", err)
			}
		}
		if err := copyTree(sourceRecordings, destinationRecordings); err != nil {
			return fmt.Errorf("restore recordings: %w", err)
		}
	}
	return nil
}

func safeConfig(value config.Config) map[string]any {
	return map[string]any{
		"network":   map[string]any{"mode": value.Network.Mode, "tailnet_hostname": value.Network.TailnetHostname, "public_fallback": value.Network.PublicFallback},
		"server":    map[string]any{"listen": value.Server.Listen, "public_base_url": value.Server.PublicBaseURL},
		"data":      map[string]any{"dir": value.Data.Dir},
		"modem":     map[string]any{"adapter": value.Modem.Adapter, "tty": value.Modem.TTY, "baud": value.Modem.Baud, "allow_usb_identity_changes": value.Modem.AllowUSBIdentityChanges},
		"voice":     map[string]any{"enabled": value.Voice.Enabled, "backend": value.Voice.Backend, "codec": value.Voice.Codec, "sample_rate": value.Voice.SampleRate, "rx_path": value.Voice.RXPath, "tx_path": value.Voice.TXPath, "runtime_dir": value.Voice.RuntimeDir, "bootstrap": value.Voice.Bootstrap},
		"webrtc":    map[string]any{"remote_ice_policy": value.WebRTC.RemoteICEPolicy, "local_ice_policy": value.WebRTC.LocalICEPolicy, "media_network_policy": value.WebRTC.MediaNetworkPolicy, "turn_credential_ttl": value.WebRTC.TurnCredentialTTL, "private_turn": map[string]any{"enabled": value.WebRTC.PrivateTURN.Enabled, "host": value.WebRTC.PrivateTURN.Host, "port": value.WebRTC.PrivateTURN.Port, "bind_ip": value.WebRTC.PrivateTURN.BindIP}},
		"push":      map[string]any{"mode": value.Push.Mode, "broker_url": value.Push.BrokerURL},
		"recording": map[string]any{"enabled": value.Recording.Enabled, "retention_days": value.Recording.RetentionDays, "minimum_free_bytes": value.Recording.MinimumFreeBytes, "auto_mode": value.Recording.AutoMode},
		"security":  map[string]any{"pairing_local_only": value.Security.PairingLocalOnly, "admin_local_only": value.Security.AdminLocalOnly, "redact_logs": value.Security.RedactLogs},
	}
}

func exportDevicePublicKeys(ctx context.Context, database *db.DB, path string) error {
	rows, err := database.QueryContext(ctx, "SELECT id, hex(public_key) FROM devices ORDER BY id")
	if err != nil {
		return fmt.Errorf("read device public keys: %w", err)
	}
	defer rows.Close()
	keys := make([]map[string]string, 0)
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			return err
		}
		keys = append(keys, map[string]string{"id": id, "publicKeyHex": key})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return writeJSON(path, keys)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func writeYAML(path string, value any) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func zipDirectory(source, output string) error {
	if fileExists(output) {
		return fmt.Errorf("refusing to overwrite existing archive %s", output)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	archive := zip.NewWriter(file)
	closeArchive := func() error {
		if closeErr := archive.Close(); closeErr != nil {
			_ = file.Close()
			return closeErr
		}
		return file.Close()
	}
	err = filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		entry, err := archive.Create(filepath.ToSlash(relative))
		if err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(entry, input)
		_ = input.Close()
		return copyErr
	})
	closeErr := closeArchive()
	if err != nil {
		return err
	}
	return closeErr
}

func copyTree(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func allowedArchivePath(name string) bool {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean != name || filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	return clean == "db.sqlite" || clean == "config-public.yaml" || clean == "device-public-keys.json" || strings.HasPrefix(clean, "recordings"+string(filepath.Separator))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
