package sms

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cellbridge/cellbridge/gateway/internal/db"
	"github.com/cellbridge/cellbridge/gateway/internal/modem"
)

type fakeSMSModem struct {
	payloads []modem.SMSPayload
}

func (f *fakeSMSModem) SendSMS(_ context.Context, _ string, payload modem.SMSPayload) (modem.SMSID, error) {
	f.payloads = append(f.payloads, payload)
	return "sms-1", nil
}

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestEngineIngestsAndDeduplicatesPDU(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(database, nil)
	engine.Now = func() time.Time { return time.Unix(100, 0) }
	segments, err := EncodeSubmitSegments("10086", "hello", 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(segments[0].PDU)
	if err != nil {
		t.Fatal(err)
	}
	message, err := engine.Ingest(context.Background(), modem.RawSMS{RawPDU: raw, ModemIndex: 1})
	if err != nil || message == nil || message.Body != "hello" {
		t.Fatalf("ingest = %#v, %v", message, err)
	}
	duplicate, err := engine.Ingest(context.Background(), modem.RawSMS{RawPDU: raw, ModemIndex: 1})
	if err != nil || duplicate != nil {
		t.Fatalf("duplicate = %#v, %v", duplicate, err)
	}
}

func TestEngineSendsLongChineseSMSAsPDU(t *testing.T) {
	database := openTestDB(t)
	modemClient := &fakeSMSModem{}
	engine := NewEngine(database, modemClient)
	message, err := engine.Send(context.Background(), "+8613800138000", strings.Repeat("测", 80))
	if err != nil || message == nil || message.Status != "sent" {
		t.Fatalf("send = %#v, %v", message, err)
	}
	if len(modemClient.payloads) != 2 || modemClient.payloads[0].PDU == "" || modemClient.payloads[0].Total != 2 {
		t.Fatalf("payloads = %#v", modemClient.payloads)
	}
}

func TestEnginePersistsSubmittedSMSWhenFinalModemResultIsUnknown(t *testing.T) {
	database := openTestDB(t)
	modemClient := &uncertainSMSModem{}
	engine := NewEngine(database, modemClient)
	message, err := engine.Send(context.Background(), "10086", "测试")
	if err != nil || message == nil || message.Status != "submitted" {
		t.Fatalf("send = %#v, %v", message, err)
	}
	var status string
	if err := database.QueryRowContext(context.Background(), "SELECT status FROM messages WHERE id = ?", message.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "submitted" {
		t.Fatalf("stored status = %q", status)
	}
}

type uncertainSMSModem struct{}

func (uncertainSMSModem) SendSMS(context.Context, string, modem.SMSPayload) (modem.SMSID, error) {
	return "", errors.Join(modem.ErrSMSSubmissionUnknown, errors.New("serial response timed out after Ctrl-Z"))
}
