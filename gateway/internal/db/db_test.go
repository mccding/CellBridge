package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateAndSyncMessage(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sequence, err := database.InsertMessage(context.Background(), Message{
		ID: "msg-1", ThreadKey: "+86138", Direction: "inbound", Peer: "+86138", Body: "测试", Encoding: "ucs2", Status: "sent", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := database.Sync(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Seq != sequence || changes[0].EntityType != "message" {
		t.Fatalf("changes = %#v", changes)
	}
}

func TestSyncLimitValidation(t *testing.T) {
	database, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Sync(context.Background(), 0, 501); err == nil {
		t.Fatal("expected invalid limit")
	}
}

func TestInsertOutboundMessagesAllowsMissingPDUHash(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "outbound-pdu.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	for index := 1; index <= 2; index++ {
		if _, err := database.InsertMessage(context.Background(), Message{
			ID: "outbound-" + string(rune('0'+index)), ThreadKey: "+8613800138000", Direction: "outbound",
			Peer: "+8613800138000", Body: "测试", Encoding: "ucs2", Status: "sent", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("insert outbound %d: %v", index, err)
		}
	}

	var stored sql.NullString
	if err := database.QueryRowContext(context.Background(), "SELECT pdu_hash FROM messages WHERE direction = 'outbound' LIMIT 1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Valid {
		t.Fatalf("missing outbound PDU hash stored as %q", stored.String)
	}
	listed, err := database.ListMessages(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].PDUHash != "" || listed[1].PDUHash != "" {
		t.Fatalf("outbound messages with NULL PDU hash = %#v", listed)
	}
}

func TestListThreadMessagesPagesNewestFirstWithStableCursor(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "thread-pages.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := time.UnixMilli(1_700_000_000_000)
	for index := 0; index < 205; index++ {
		id := "thread-msg-" + time.UnixMilli(int64(index)).Format("150405.000")
		if _, err := database.InsertMessage(context.Background(), Message{
			ID: id, ThreadKey: "+8613800138000",
			Direction: "inbound", Peer: "+8613800138000", Body: "消息", Encoding: "ucs2", Status: "sent",
			PDUHash:   "hash-" + id,
			CreatedAt: base.Add(time.Duration(index) * time.Millisecond),
		}); err != nil {
			t.Fatal(err)
		}
	}

	newest, err := database.ListThreadMessages(context.Background(), "+8613800138000", nil, "", 101)
	if err != nil || len(newest) != 101 {
		t.Fatalf("newest page = %d, %v", len(newest), err)
	}
	if !newest[0].CreatedAt.Before(newest[len(newest)-1].CreatedAt) {
		t.Fatalf("newest page is not chronological: %v -> %v", newest[0].CreatedAt, newest[len(newest)-1].CreatedAt)
	}

	before := newest[0].CreatedAt.UnixMilli()
	older, err := database.ListThreadMessages(context.Background(), "+8613800138000", &before, newest[0].ID, 101)
	if err != nil || len(older) != 101 {
		t.Fatalf("older page = %d, %v", len(older), err)
	}
	if !older[len(older)-1].CreatedAt.Before(newest[0].CreatedAt) {
		t.Fatalf("cursor leaked newer message: %v >= %v", older[len(older)-1].CreatedAt, newest[0].CreatedAt)
	}
}

func TestIdempotencyClaimAndReplay(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	claimed, record, err := database.ClaimIdempotency(context.Background(), "dev-1", "sms.send", "req-1")
	if err != nil || !claimed || !record.Pending {
		t.Fatalf("first claim = %v, %#v, %v", claimed, record, err)
	}
	if err := database.CompleteIdempotency(context.Background(), "dev-1", "sms.send", "req-1", 201, []byte(`{"id":"msg-1"}`)); err != nil {
		t.Fatal(err)
	}
	claimed, record, err = database.ClaimIdempotency(context.Background(), "dev-1", "sms.send", "req-1")
	if err != nil || claimed || record.Pending || record.StatusCode != 201 || string(record.Body) != `{"id":"msg-1"}` {
		t.Fatalf("replay = %v, %#v, %v", claimed, record, err)
	}
}

func TestAuditLogDoesNotStoreMessageBody(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := database.AppendAudit(context.Background(), "dev-1", "sms.send", "msg-1", "accepted"); err != nil {
		t.Fatal(err)
	}
	var target, result string
	if err := database.QueryRow("SELECT target, result FROM audit_log WHERE device_id = ?", "dev-1").Scan(&target, &result); err != nil {
		t.Fatal(err)
	}
	if target != "msg-1" || result != "accepted" {
		t.Fatalf("audit row = %q, %q", target, result)
	}
}

func TestPendingSMSSegmentsCanBeRestoredAndLinked(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1234)
	if err := database.InsertSMSSegment(context.Background(), SMSSegment{ID: "seg-1", Sender: "+86138", ConcatRef: 7, PartNo: 1, TotalParts: 2, RawPDU: []byte{1, 2}, PDUHash: "hash-1", ReceivedAt: now}); err != nil {
		t.Fatal(err)
	}
	segments, err := database.ListPendingSMSSegments(context.Background(), "+86138", 7, 2)
	if err != nil || len(segments) != 1 || segments[0].PDUHash != "hash-1" {
		t.Fatalf("pending segments = %#v, %v", segments, err)
	}
	if err := database.LinkSMSSegment(context.Background(), "hash-1", "msg-1"); err != nil {
		t.Fatal(err)
	}
	segments, err = database.ListPendingSMSSegments(context.Background(), "+86138", 7, 2)
	if err != nil || len(segments) != 0 {
		t.Fatalf("linked segments = %#v, %v", segments, err)
	}
}

func TestMigrateUpgradesLegacyRecordingTable(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "legacy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.Exec(`CREATE TABLE calls (id TEXT PRIMARY KEY, direction TEXT, peer TEXT, state TEXT, started_at INTEGER, connected_at INTEGER, ended_at INTEGER, end_reason TEXT, recording_id TEXT, sync_seq INTEGER NOT NULL);
		CREATE TABLE recordings (id TEXT PRIMARY KEY, call_id TEXT NOT NULL, path TEXT NOT NULL, duration_ms INTEGER, created_at INTEGER NOT NULL);
		INSERT INTO recordings(id, call_id, path, duration_ms, created_at) VALUES ('rec-legacy', 'call-legacy', '/var/lib/cellbridge/recordings/old.m4a', 1000, 1234);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state, path string
	if err := database.QueryRow(`SELECT state, file_path FROM recordings WHERE id = 'rec-legacy'`).Scan(&state, &path); err != nil {
		t.Fatal(err)
	}
	if state != "ready" || path != "/var/lib/cellbridge/recordings/old.m4a" {
		t.Fatalf("legacy recording = %q, %q", state, path)
	}
	if _, err := database.SaveCall(context.Background(), Call{ID: "call-legacy", Direction: "outbound", State: "ended", StartedAt: time.UnixMilli(1234)}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordingSettingsArePersistentAndSafeByDefault(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "settings.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	defaults := RecordingSettings{Enabled: true, AutoMode: "off", RetentionDays: 90, MinimumFreeBytes: 500 * 1024 * 1024}
	if err := database.EnsureRecordingSettings(context.Background(), defaults); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetRecordingSettings(context.Background())
	if err != nil || !got.Enabled || got.AutoMode != "off" || got.RetentionDays != 90 {
		t.Fatalf("default settings = %#v, %v", got, err)
	}
	updated := RecordingSettings{Enabled: false, AutoMode: "off", RetentionDays: 30, MinimumFreeBytes: 1024, UpdatedAt: time.UnixMilli(42)}
	if err := database.SaveRecordingSettings(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	got, err = database.GetRecordingSettings(context.Background())
	if err != nil || got.Enabled || got.RetentionDays != 30 || got.MinimumFreeBytes != 1024 || got.UpdatedAt.UnixMilli() != 42 {
		t.Fatalf("updated settings = %#v, %v", got, err)
	}
}
