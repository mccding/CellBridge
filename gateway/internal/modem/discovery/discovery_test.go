package discovery

import (
	"context"
	"os"
	"reflect"
	"testing"
)

type fakeExchanger struct {
	responses map[string][]string
}

func (f fakeExchanger) Exchange(_ context.Context, command string) ([]string, error) {
	return f.responses[command], nil
}

func TestProbeParsesIdentityAndStatus(t *testing.T) {
	report := Probe(context.Background(), Candidate{Path: "/dev/serial/by-id/usb-Quectel_2c7c_0125"}, fakeExchanger{responses: map[string][]string{
		"ATI":       {"ATI", "Quectel EC25", "REVISION 01", "OK"},
		"AT+CGMI":   {"Quectel", "OK"},
		"AT+CGMM":   {"EC25", "OK"},
		"AT+CGMR":   {"REVISION 01", "OK"},
		"AT+CPIN?":  {"+CPIN: READY", "OK"},
		"AT+CEREG?": {"+CEREG: 0,1", "OK"},
		"AT+CSQ":    {"+CSQ: 20,99", "OK"},
	}})
	if report.Identity.Model != "EC25" || report.SIM != "ready" || report.Registration != "registered" || report.Signal != -73 {
		t.Fatalf("unexpected report: %#v", report)
	}
	if report.Capabilities.Vendor != "quectel-compatible" || !report.Capabilities.SMS {
		t.Fatalf("unexpected capabilities: %#v", report.Capabilities)
	}
}

func TestProbeRecognizesQDC507VoiceControlPath(t *testing.T) {
	report := Probe(context.Background(), Candidate{Path: "/dev/serial/by-id/usb-BAIWANG_Baiwang-if02"}, fakeExchanger{responses: map[string][]string{
		"ATI":                     {"BAIWANG", "QDC507", "QDC507GLEFM21", "OK"},
		"AT+CGMI":                 {"BAIWANG", "OK"},
		"AT+CGMM":                 {"QDC507", "OK"},
		"AT+CGMR":                 {"QDC507GLEFM21", "OK"},
		"AT+CPIN?":                {"+CPIN: READY", "OK"},
		"AT+CEREG?":               {"+CEREG: 0,1", "OK"},
		"AT+CSQ":                  {"+CSQ: 25,99", "OK"},
		`AT+QCFG="ims"`:           {`+QCFG: "ims",1,1`, "OK"},
		`AT+QCFG="volte_disable"`: {`+QCFG: "volte/disable",0`, "OK"},
		"AT+VTS=?":                {"+VTS: (0-9,A-D,*,#),(0-255)", "OK"},
		"AT+QPCMV=?":              {"+QPCMV: (0,1),(0-2)", "OK"},
	}})
	if !report.Capabilities.Voice || !report.Capabilities.DTMF || !report.Capabilities.RequiresBootstrap {
		t.Fatalf("expected QDC507 voice controls: %#v", report.Capabilities)
	}
	if report.Capabilities.Tier != "voice_control_only" || report.Capabilities.Audio == nil || report.Capabilities.Audio.Backend != "uac-qdc507" {
		t.Fatalf("unexpected QDC507 capability tier: %#v", report.Capabilities)
	}
}

func TestEnumeratePrefersByID(t *testing.T) {
	root := t.TempDir()
	if err := mkdirAll(root + "/serial/by-id"); err != nil {
		t.Fatal(err)
	}
	if err := touch(root + "/serial/by-id/usb-Quectel_2c7c_0125-if00"); err != nil {
		t.Fatal(err)
	}
	if err := touch(root + "/ttyUSB0"); err != nil {
		t.Fatal(err)
	}
	got, err := Enumerate(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Candidate{{Path: root + "/serial/by-id/usb-Quectel_2c7c_0125-if00", Source: "serial-by-id", USBVID: "2c7c", USBPID: "0125"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// Small wrappers keep the test independent of platform-specific os helpers.
var mkdirAll = func(path string) error { return os.MkdirAll(path, 0o755) }
var touch = func(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	return file.Close()
}
