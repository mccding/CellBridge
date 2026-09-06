package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSimpleRILStatusKeepsUnverifiedCapabilitiesDisabled(t *testing.T) {
	directory := t.TempDir()
	program := filepath.Join(directory, "fake-ril")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' 'modem is ONLINE' 'Registered with a network' 'Packet switch domain attach state :' 'Attached'\n"
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	line, err := (&SimpleRILAdapter{Path: program, Timeout: time.Second}).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if line.Registration != "registered" {
		t.Fatalf("registration = %q", line.Registration)
	}
	if line.SIM != "unknown" || line.Voice || line.SMS || line.DTMF {
		t.Fatalf("unverified capabilities were enabled: %#v", line)
	}
}

func TestSimpleRILStatusMarksSIMReadyOnlyFromValidatedIMSI(t *testing.T) {
	directory := t.TempDir()
	program := filepath.Join(directory, "fake-ril")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' 'modem is ONLINE' 'IMSI is 460015130491329' 'Registered with a network'\n"
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	line, err := (&SimpleRILAdapter{Path: program, Timeout: time.Second}).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if line.SIM != "ready" || line.Registration != "registered" {
		t.Fatalf("validated SIM/network state not reflected: %#v", line)
	}
	if line.Voice || line.SMS || line.DTMF {
		t.Fatalf("business capabilities were enabled without HIL: %#v", line)
	}
}

func TestSimpleRILMutationsFailClosed(t *testing.T) {
	adapter := SimpleRILAdapter{}
	if err := adapter.SendSMS(context.Background(), "10086", "test"); err != ErrUnsupported {
		t.Fatalf("SendSMS error = %v", err)
	}
	if _, err := adapter.Dial(context.Background(), "10086"); err != ErrUnsupported {
		t.Fatalf("Dial error = %v", err)
	}
}

func TestVerifiedCallAdapterRequiresRadioTruth(t *testing.T) {
	directory := t.TempDir()
	program := filepath.Join(directory, "fake-ril")
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    "dial 18551188565")
      printf '%s\n' 'SUCCESS (UNCONDITIONAL)' '[NOTIFICATION]' 'call state ORIGINATION' '[NOTIFICATION]' 'call state ALERTING' '[NOTIFICATION]' 'call state CONVERSATION'
      ;;
    call_end)
      printf '%s\n' 'SUCCESS (UNCONDITIONAL)' '[NOTIFICATION]' 'call state DISCONNECTING' '[NOTIFICATION]' 'call state END'
      ;;
    quit)
      exit 0
      ;;
  esac
done
`
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	adapter := SimpleRILAdapter{Path: program, Timeout: time.Second, EnableVerifiedCall: true}
	call, err := adapter.Dial(context.Background(), "18551188565")
	if err != nil {
		t.Fatal(err)
	}
	if call.State != "active" || call.Direction != "outbound" {
		t.Fatalf("dial result = %#v", call)
	}
	if err := adapter.Hangup(context.Background(), call.ID); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedCallAdapterRejectsUnverifiedTarget(t *testing.T) {
	adapter := SimpleRILAdapter{EnableVerifiedCall: true}
	if _, err := adapter.Dial(context.Background(), "1855-118-8565"); err == nil || !strings.Contains(err.Error(), "digits") {
		t.Fatalf("invalid target error = %v", err)
	}
}

func TestVerifiedSMSAdapterSendsEncodedPDU(t *testing.T) {
	directory := t.TempDir()
	program := filepath.Join(directory, "fake-ril")
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    mo_sms_gsm\ *)
      printf '%s\n' 'SUCCESS (UNCONDITIONAL)'
      ;;
    quit)
      exit 0
      ;;
  esac
done
`
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	adapter := SimpleRILAdapter{Path: program, Timeout: time.Second, EnableVerifiedSMS: true}
	if err := adapter.SendSMS(context.Background(), "18551188565", "你好"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedSMSAdapterRejectsQMIResultCodeOne(t *testing.T) {
	directory := t.TempDir()
	program := filepath.Join(directory, "fake-ril")
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    mo_sms_gsm\ *)
      printf '%s\n' 'MESSAGE ID 0' 'RESULT CODE 1' 'CAUSE CODE VALID 1' 'CAUSE_CODE 42' 'ERROR_CLASS_VALID 1' 'ERROR_CLASS 3'
      ;;
    quit)
      exit 0
      ;;
  esac
done
`
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	adapter := SimpleRILAdapter{Path: program, Timeout: time.Second, EnableVerifiedSMS: true}
	err := adapter.SendSMS(context.Background(), "18551188565", "你好")
	if err == nil || !strings.Contains(err.Error(), "RESULT CODE 1") || !strings.Contains(err.Error(), "CAUSE CODE 42") || !strings.Contains(err.Error(), "ERROR CLASS 3") {
		t.Fatalf("SMS error = %v", err)
	}
}

func TestParseSMSTerminalResultSupportsVendorFieldSpellings(t *testing.T) {
	result, ok := parseSMSTerminalResult("MESSAGE ID 7\nRESULT CODE 1\nCAUSE_CODE_VALID 1\nCAUSE_CODE 42\nERROR_CLASS_VALID 1\nERROR_CLASS 3\n")
	if !ok {
		t.Fatal("SMS terminal result was not parsed")
	}
	if got := result.summary(); !strings.Contains(got, "MESSAGE ID 7") || !strings.Contains(got, "CAUSE CODE 42") || !strings.Contains(got, "ERROR CLASS 3") {
		t.Fatalf("summary = %q", got)
	}
}

func TestBuildGSMSMSSubmitPDUUsesUCS2(t *testing.T) {
	pdu, err := buildGSMSMSSubmitPDU("+8613800138000", "测试")
	if err != nil {
		t.Fatal(err)
	}
	want := "0001000D91683108108300F00008046D4B8BD5"
	if pdu != want {
		t.Fatalf("PDU = %s, want %s", pdu, want)
	}
}

func TestBuildGSMSMSSubmitPDURejectsInvalidOrOversizedInput(t *testing.T) {
	if _, err := buildGSMSMSSubmitPDU("10086", ""); err == nil {
		t.Fatal("empty SMS body was accepted")
	}
	if _, err := buildGSMSMSSubmitPDU("100-86", "test"); err == nil {
		t.Fatal("invalid SMS target was accepted")
	}
	body := strings.Repeat("测", 71)
	if _, err := buildGSMSMSSubmitPDU("10086", body); err == nil {
		t.Fatal("oversized SMS body was accepted")
	}
}
