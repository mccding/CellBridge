package qdc507

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

// Exchanger is the small AT surface needed to identify the QDC507 voice
// control path. It deliberately does not include mutating commands.
type Exchanger interface {
	Exchange(context.Context, string) ([]string, error)
}

// IsQDC507 accepts the printed model and the compatible vendor string. The
// audio runtime is QDC507-specific; a generic EC25 must not inherit it merely
// because it exposes the same USB VID/PID.
func IsQDC507(vendor, model string) bool {
	joined := strings.ToLower(strings.TrimSpace(vendor) + " " + strings.TrimSpace(model))
	return strings.Contains(joined, "qdc507") || strings.Contains(joined, "baiwang")
}

// ProbeCapabilities verifies the read-only modem gates required by the
// QDC507/DJI certified voice path. It reports voice-control capability before
// the module-side ADB/VoLTE PCM runtime is loaded; the Gateway promotes this
// to full_voice only after VoiceAudio.Probe succeeds.
func ProbeCapabilities(ctx context.Context, exchanger Exchanger, vendor, model string) (modem.Capabilities, error) {
	capabilities := modem.Capabilities{
		Vendor: vendor,
		Model:  model,
		SMS:    true,
		Tier:   modem.CapabilitySMSOnly,
	}
	if !IsQDC507(vendor, model) {
		return capabilities, nil
	}

	ims, imsOK, err := queryIMS(ctx, exchanger)
	if err != nil {
		return capabilities, err
	}
	volteDisabled, volteOK, err := queryVoLTEDisabled(ctx, exchanger)
	if err != nil {
		return capabilities, err
	}
	dtmfOK := querySupported(ctx, exchanger, "AT+VTS=?", "+VTS:")
	pcmRouteOK := querySupported(ctx, exchanger, "AT+QPCMV=?", "+QPCMV:")
	if imsOK && ims && volteOK && !volteDisabled && pcmRouteOK {
		capabilities.Voice = true
		capabilities.RequiresBootstrap = true
		capabilities.Tier = modem.CapabilityVoiceControlOnly
		capabilities.Audio = &modem.AudioCapabilities{Backend: "uac-qdc507", SampleRate: 8000, Channels: 1}
	}
	capabilities.DTMF = dtmfOK
	return capabilities, nil
}

func querySupported(ctx context.Context, exchanger Exchanger, command, marker string) bool {
	lines, err := exchanger.Exchange(ctx, command)
	if err != nil {
		return false
	}
	for _, line := range lines {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), strings.ToUpper(marker)) {
			return true
		}
	}
	return false
}

func queryIMS(ctx context.Context, exchanger Exchanger) (bool, bool, error) {
	lines, err := exchanger.Exchange(ctx, `AT+QCFG="ims"`)
	if err != nil {
		return false, false, err
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), `+qcfg: "ims"`) {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			return false, false, fmt.Errorf("invalid QDC507 IMS response %q", line)
		}
		return strings.TrimSpace(fields[1]) == "1" && strings.TrimSpace(fields[2]) == "1", true, nil
	}
	return false, false, fmt.Errorf("QDC507 IMS capability was not reported")
}

func queryVoLTEDisabled(ctx context.Context, exchanger Exchanger) (bool, bool, error) {
	lines, err := exchanger.Exchange(ctx, `AT+QCFG="volte_disable"`)
	if err != nil {
		return false, false, err
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if !strings.HasPrefix(lower, `+qcfg: "volte/disable"`) && !strings.HasPrefix(lower, `+qcfg: "volte_disable"`) {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			return false, false, fmt.Errorf("invalid QDC507 VoLTE response %q", line)
		}
		value, parseErr := strconv.Atoi(strings.TrimSpace(fields[1]))
		if parseErr != nil || (value != 0 && value != 1) {
			return false, false, fmt.Errorf("invalid QDC507 VoLTE disable value %q", line)
		}
		return value == 1, true, nil
	}
	return false, false, fmt.Errorf("QDC507 VoLTE capability was not reported")
}
