package qdc507

import (
	"context"
	"testing"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

type fakeExchanger struct {
	responses map[string][]string
}

func (f fakeExchanger) Exchange(_ context.Context, command string) ([]string, error) {
	return f.responses[command], nil
}

func TestProbeCapabilitiesRecognizesQDC507VoiceGates(t *testing.T) {
	value, err := ProbeCapabilities(context.Background(), fakeExchanger{responses: map[string][]string{
		`AT+QCFG="ims"`:           {`+QCFG: "ims",1,1`, "OK"},
		`AT+QCFG="volte_disable"`: {`+QCFG: "volte/disable",0`, "OK"},
		"AT+VTS=?":                {"+VTS: (0-9,A-D,*,#),(0-255)", "OK"},
		"AT+QPCMV=?":              {"+QPCMV: (0,1),(0-2)", "OK"},
	}}, "Baiwang", "QDC507")
	if err != nil {
		t.Fatal(err)
	}
	if !value.Voice || !value.DTMF || !value.RequiresBootstrap || value.Tier != modem.CapabilityVoiceControlOnly {
		t.Fatalf("capabilities = %#v", value)
	}
	if value.Audio == nil || value.Audio.Backend != "uac-qdc507" {
		t.Fatalf("audio = %#v", value.Audio)
	}
}

func TestProbeCapabilitiesDoesNotPromoteGenericEC25(t *testing.T) {
	value, err := ProbeCapabilities(context.Background(), fakeExchanger{}, "Quectel", "EC25")
	if err != nil {
		t.Fatal(err)
	}
	if value.Voice || value.DTMF || value.Tier != modem.CapabilitySMSOnly {
		t.Fatalf("capabilities = %#v", value)
	}
}
