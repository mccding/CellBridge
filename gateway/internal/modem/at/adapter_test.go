package at

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/modem"
	"github.com/cellbridge/cellbridge/gateway/internal/sms"
)

type scriptedPort struct {
	mu          sync.Mutex
	read        bytes.Buffer
	writes      []string
	incomingURC bool
	dryRun      bool // +CMGW responses instead of +CMGS
}

func (p *scriptedPort) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	value := string(data)
	p.writes = append(p.writes, value)
	command := strings.TrimSpace(value)
	switch {
	case strings.Contains(value, "\x1a"):
		if p.dryRun {
			p.read.WriteString("\r\n+CMGW: 1\r\nOK\r\n")
		} else {
			p.read.WriteString("\r\n+CMGS: 1\r\nOK\r\n")
		}
	case strings.HasPrefix(command, "AT+CMGS="), strings.HasPrefix(command, "AT+CMGW="):
		p.read.WriteString("\r\n> ")
	case strings.HasPrefix(command, "AT+CMGL"):
		p.read.WriteString("\r\n+CMGL: 2,0,,23\r\n00000D916831553322F10000FF\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CPIN"):
		p.read.WriteString("\r\n+CPIN: READY\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CEREG"):
		p.read.WriteString("\r\n+CEREG: 0,1\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CSQ"):
		if p.incomingURC {
			p.read.WriteString("\r\nRING\r\n")
		}
		p.read.WriteString("\r\n+CSQ: 20,99\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CGMI"):
		p.read.WriteString("\r\nQuectel\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CGMM"):
		p.read.WriteString("\r\nEC25\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CGMR"):
		p.read.WriteString("\r\nEC25EFAR06A01M4G\r\nOK\r\n")
	default:
		p.read.WriteString("\r\nOK\r\n")
	}
	return len(data), nil
}

func (p *scriptedPort) Read(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.read.Len() == 0 {
		return 0, io.EOF
	}
	return p.read.Read(data)
}

func (p *scriptedPort) Close() error { return nil }

func TestAdapterControlsCallsAndSMS(t *testing.T) {
	port := &scriptedPort{}
	adapter := NewAdapter(NewClient(port))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	callID, err := adapter.Dial(ctx, "+8613800138000")
	if err != nil || callID == "" {
		t.Fatalf("dial = %q, %v", callID, err)
	}
	if err := adapter.Answer(ctx, callID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.SendDTMF(ctx, callID, '5'); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Hangup(ctx, callID); err != nil {
		t.Fatal(err)
	}
	segments, err := sms.EncodeSubmitSegments("10086", "hello", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.SendSMS(ctx, "10086", modem.SMSPayload{PDU: segments[0].PDU}); err != nil {
		t.Fatal(err)
	}
	status, err := adapter.Status(ctx)
	if err != nil || status.SIM != "ready" || status.Registration != "registered" || status.Signal.Bars != 4 {
		t.Fatalf("status = %#v, %v", status, err)
	}
	if len(port.writes) < 8 {
		t.Fatalf("writes = %#v", port.writes)
	}
}

func TestClientSendPDUWaitsForPrompt(t *testing.T) {
	port := &scriptedPort{}
	client := NewClient(port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lines, err := client.SendPDU(ctx, 5, "00010005912143F50000FF")
	if err != nil || len(lines) == 0 || lines[len(lines)-1] != "OK" {
		t.Fatalf("send PDU = %#v, %v", lines, err)
	}
}

func TestClientDispatchesInterleavedCallURC(t *testing.T) {
	port := &scriptedPort{incomingURC: true}
	client := NewClient(port)
	var received []string
	client.SetLineHandler(func(line string) { received = append(received, line) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Exchange(ctx, "AT+CSQ"); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0] != "RING" {
		t.Fatalf("interleaved URC = %#v", received)
	}
}
