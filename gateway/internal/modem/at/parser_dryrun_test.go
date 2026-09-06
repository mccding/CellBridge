package at

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSendPDUDryRunUsesCMGW verifies the SMS dry-run path issues
// AT+CMGW (store only, no network cost) instead of AT+CMGS (real send).
func TestSendPDUDryRunUsesCMGW(t *testing.T) {
	port := &scriptedPort{dryRun: true}
	client := NewClient(port)
	client.SetDryRun(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.SendPDU(ctx, 17, "0001000B818155118865F50008044F60597D")
	if err != nil {
		t.Fatalf("SendPDU dry run error: %v", err)
	}
	if len(response) == 0 || response[0] != "+CMGW: 1" {
		t.Fatalf("unexpected response: %#v", response)
	}
	all := strings.Join(port.writes, "")
	if strings.Contains(all, "AT+CMGS") {
		t.Fatal("dry run must not issue AT+CMGS (real network send)")
	}
	if !strings.Contains(all, "AT+CMGW=17") {
		t.Fatalf("expected AT+CMGW command, got writes: %q", all)
	}
}

// TestSendTextSMSTextDryRunUsesCMGW covers the short-code text path.
func TestSendTextSMSTextDryRunUsesCMGW(t *testing.T) {
	port := &scriptedPort{dryRun: true}
	client := NewClient(port)
	client.SetDryRun(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.SendTextSMS(ctx, "10010", "hello")
	if err != nil {
		t.Fatalf("SendTextSMS dry run error: %v", err)
	}
	if len(response) == 0 || response[0] != "+CMGW: 1" {
		t.Fatalf("unexpected response: %#v", response)
	}
	all := strings.Join(port.writes, "")
	if strings.Contains(all, "AT+CMGS") {
		t.Fatal("dry run must not issue AT+CMGS (real network send)")
	}
	if !strings.Contains(all, "AT+CMGW=\"10010\"") {
		t.Fatalf("expected AT+CMGW text command, got writes: %q", all)
	}
}

// TestSendPDUNormalUsesCMGS guards the non-dry-run path still works.
func TestSendPDUNormalUsesCMGS(t *testing.T) {
	port := &scriptedPort{}
	client := NewClient(port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.SendPDU(ctx, 17, "0001000B818155118865F50008044F60597D")
	if err != nil {
		t.Fatalf("SendPDU normal error: %v", err)
	}
	if len(response) == 0 || response[0] != "+CMGS: 1" {
		t.Fatalf("unexpected response: %#v", response)
	}
	all := strings.Join(port.writes, "")
	if !strings.Contains(all, "AT+CMGS=17") {
		t.Fatalf("expected AT+CMGS command, got writes: %q", all)
	}
}