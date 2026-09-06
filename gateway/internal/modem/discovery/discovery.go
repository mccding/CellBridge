package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
	"github.com/cellbridge/cellbridge/gateway/internal/modem/at"
	"github.com/cellbridge/cellbridge/gateway/internal/modem/qdc507"
)

type Candidate struct {
	Path   string `json:"path"`
	Source string `json:"source"`
	USBVID string `json:"usbVid,omitempty"`
	USBPID string `json:"usbPid,omitempty"`
}

type Report struct {
	Candidate
	Identity     Identity           `json:"identity"`
	SIM          string             `json:"sim"`
	Registration string             `json:"registration"`
	Signal       int                `json:"rssi,omitempty"`
	Capabilities modem.Capabilities `json:"capabilities"`
	Warnings     []string           `json:"warnings,omitempty"`
	Error        string             `json:"error,omitempty"`
}

type Identity struct {
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty"`
	Revision     string `json:"revision,omitempty"`
}

var vidPIDPattern = regexp.MustCompile(`(?i)(?:^|[_:])([0-9a-f]{4})[_:]([0-9a-f]{4})(?:[-_:]|$)`)

// Enumerate follows the plan's stable-device-path priority and only falls
// back to generic TTY names when no by-id path exists.
func Enumerate(devRoot string) ([]Candidate, error) {
	var candidates []Candidate
	byID := filepath.Join(devRoot, "serial", "by-id", "*")
	paths, err := filepath.Glob(byID)
	if err != nil {
		return nil, fmt.Errorf("glob serial by-id: %w", err)
	}
	for _, path := range paths {
		candidate := Candidate{Path: path, Source: "serial-by-id"}
		if match := vidPIDPattern.FindStringSubmatch(filepath.Base(path)); len(match) == 3 {
			candidate.USBVID, candidate.USBPID = strings.ToLower(match[1]), strings.ToLower(match[2])
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		patterns := []string{"ttyUSB*", "ttyACM*", "cu.usbserial*", "cu.usbmodem*"}
		for _, pattern := range patterns {
			paths, globErr := filepath.Glob(filepath.Join(devRoot, pattern))
			if globErr != nil {
				return nil, fmt.Errorf("glob %s: %w", pattern, globErr)
			}
			for _, path := range paths {
				candidates = append(candidates, Candidate{Path: path, Source: "generic-tty"})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates, nil
}

type Exchanger interface {
	Exchange(context.Context, string) ([]string, error)
}

func Probe(ctx context.Context, candidate Candidate, exchanger Exchanger) Report {
	report := Report{Candidate: candidate, Capabilities: modem.Capabilities{USBVID: candidate.USBVID, USBPID: candidate.USBPID}}
	ati, err := exchanger.Exchange(ctx, "ATI")
	if err != nil {
		report.Error = err.Error()
		return report
	}
	report.Identity = parseIdentity(ati)
	identityQueries := map[string]func(string){
		"AT+CGMI": func(value string) { report.Identity.Manufacturer = value },
		"AT+CGMM": func(value string) { report.Identity.Model = value },
		"AT+CGMR": func(value string) { report.Identity.Revision = value },
	}
	for command, assign := range identityQueries {
		lines, commandErr := exchanger.Exchange(ctx, command)
		if commandErr != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s: %v", command, commandErr))
			continue
		}
		if value := firstPayload(lines); value != "" {
			assign(value)
		}
	}
	report.Capabilities.Model = report.Identity.Model
	report.Capabilities.Vendor = vendorFor(report.Identity.Manufacturer, report.Identity.Model)
	report.Capabilities.SMS = true

	for command, parse := range map[string]func(string){
		"AT+CPIN?":  func(value string) { report.SIM = parseSIM(value) },
		"AT+CEREG?": func(value string) { report.Registration = parseRegistration(value) },
		"AT+CSQ":    func(value string) { report.Signal = parseCSQ(value) },
	} {
		lines, commandErr := exchanger.Exchange(ctx, command)
		if commandErr != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s: %v", command, commandErr))
			continue
		}
		value := firstPayload(lines)
		parse(value)
	}
	if qdc507.IsQDC507(report.Capabilities.Vendor, report.Identity.Model) {
		voiceCapabilities, capabilityErr := qdc507.ProbeCapabilities(ctx, exchanger, report.Capabilities.Vendor, report.Identity.Model)
		if capabilityErr != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("QDC507 voice capability probe: %v", capabilityErr))
		} else {
			report.Capabilities.Voice = voiceCapabilities.Voice
			report.Capabilities.DTMF = voiceCapabilities.DTMF
			report.Capabilities.Audio = voiceCapabilities.Audio
			report.Capabilities.RequiresBootstrap = voiceCapabilities.RequiresBootstrap
			report.Capabilities.Tier = voiceCapabilities.Tier
		}
	}
	identity := report.Identity.Manufacturer + "\x00" + report.Identity.Model + "\x00" + report.Identity.Revision
	report.Capabilities.FirmwareFingerprint = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(identity)))
	return report
}

func parseIdentity(lines []string) Identity {
	var nonEmpty []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "ATI" || at.IsFinal(line) || strings.HasPrefix(line, "+") {
			continue
		}
		nonEmpty = append(nonEmpty, line)
	}
	identity := Identity{}
	if len(nonEmpty) > 0 {
		identity.Model = nonEmpty[0]
	}
	if len(nonEmpty) > 1 {
		identity.Revision = nonEmpty[1]
	}
	return identity
}

func firstPayload(lines []string) string {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && line != "OK" && !at.IsFinal(line) {
			return line
		}
	}
	return ""
}

func parseCSQ(line string) int {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return 0
	}
	parts := strings.Split(strings.TrimSpace(line[colon+1:]), ",")
	if len(parts) == 0 {
		return 0
	}
	value, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || value == 99 {
		return 0
	}
	// 3GPP CSQ 0..31 maps to approximately -113..-51 dBm.
	return -113 + (value * 2)
}

func parseSIM(line string) string {
	value := strings.ToLower(firstValue(line))
	switch {
	case value == "ready":
		return "ready"
	case strings.Contains(value, "sim pin"), strings.Contains(value, "sim puk"):
		return "locked"
	case value == "not ready":
		return "absent"
	default:
		return "unknown"
	}
}

func parseRegistration(line string) string {
	value := firstValue(line)
	parts := strings.Split(value, ",")
	status := strings.TrimSpace(parts[len(parts)-1])
	switch status {
	case "1", "5":
		return "registered"
	case "2":
		return "searching"
	case "3":
		return "denied"
	default:
		return "unknown"
	}
}

func firstValue(line string) string {
	if colon := strings.IndexByte(line, ':'); colon >= 0 {
		return strings.TrimSpace(line[colon+1:])
	}
	return strings.TrimSpace(line)
}

func vendorFor(manufacturer, model string) string {
	joined := strings.ToLower(manufacturer + " " + model)
	switch {
	case strings.Contains(joined, "quectel"), strings.Contains(joined, "qdc507"), strings.Contains(joined, "dji"):
		return "quectel-compatible"
	default:
		return manufacturer
	}
}

func EncodeReports(reports []Report) ([]byte, error) {
	return json.MarshalIndent(reports, "", "  ")
}

func EnsureDevicePath(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("device path unavailable: %w", err)
	}
	return nil
}
