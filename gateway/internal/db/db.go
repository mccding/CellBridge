package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// The embedded migration is intentionally kept next to the repository code;
// release packages still contain the human-readable copy in /migrations.
//
//go:embed schema.sql
var schemaFS embed.FS

type DB struct {
	*sql.DB
}

func Open(path string) (*DB, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	return &DB{DB: database}, nil
}

func (database *DB) Migrate(ctx context.Context) error {
	if _, err := database.ExecContext(ctx, `PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;`); err != nil {
		return fmt.Errorf("enable sqlite pragmas: %w", err)
	}
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read embedded schema: %w", err)
	}
	if _, err := database.ExecContext(ctx, string(schema)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if err := ensureColumn(ctx, database.DB, "sms_segments", "pdu_hash", "TEXT"); err != nil {
		return fmt.Errorf("upgrade sms_segments: %w", err)
	}
	if err := ensureColumn(ctx, database.DB, "devices", "push_environment", "TEXT"); err != nil {
		return fmt.Errorf("upgrade devices.push_environment: %w", err)
	}
	if err := ensureColumn(ctx, database.DB, "devices", "push_locale", "TEXT"); err != nil {
		return fmt.Errorf("upgrade devices.push_locale: %w", err)
	}
	for _, column := range []struct {
		table string
		name  string
		kind  string
	}{
		{"calls", "recording_state", "TEXT"},
		{"calls", "recording_duration_ms", "INTEGER"},
		{"calls", "deleted_at", "INTEGER"},
		{"recordings", "state", "TEXT NOT NULL DEFAULT 'ready'"},
		{"recordings", "trigger_mode", "TEXT NOT NULL DEFAULT 'manual'"},
		{"recordings", "started_at", "INTEGER"},
		{"recordings", "stopped_at", "INTEGER"},
		{"recordings", "file_path", "TEXT"},
		{"recordings", "container", "TEXT"},
		{"recordings", "codec", "TEXT"},
		{"recordings", "sample_rate", "INTEGER"},
		{"recordings", "channels", "INTEGER"},
		{"recordings", "bitrate", "INTEGER"},
		{"recordings", "size_bytes", "INTEGER"},
		{"recordings", "sha256", "TEXT"},
		{"recordings", "waveform_path", "TEXT"},
		{"recordings", "failure_code", "TEXT"},
		{"recordings", "failure_detail", "TEXT"},
		{"recordings", "retention_expires_at", "INTEGER"},
		{"recordings", "created_by_device_id", "TEXT"},
		{"recordings", "updated_at", "INTEGER NOT NULL DEFAULT 0"},
		{"recordings", "sync_seq", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := ensureColumn(ctx, database.DB, column.table, column.name, column.kind); err != nil {
			return fmt.Errorf("upgrade %s.%s: %w", column.table, column.name, err)
		}
	}
	if _, err := database.ExecContext(ctx, `UPDATE recordings SET
		state = COALESCE(NULLIF(state, ''), 'ready'),
		file_path = COALESCE(NULLIF(file_path, ''), path),
		updated_at = CASE WHEN updated_at = 0 THEN created_at ELSE updated_at END
		WHERE file_path IS NULL OR file_path = '' OR updated_at = 0`); err != nil {
		return fmt.Errorf("upgrade recording legacy values: %w", err)
	}
	// Outbound SMS messages do not have an inbound PDU hash. Older builds
	// stored that absent value as the empty string, which made the partial
	// unique index reject every subsequent outbound message. Normalize legacy
	// empty values to SQL NULL so the index only deduplicates real PDUs.
	if _, err := database.ExecContext(ctx, `UPDATE messages SET pdu_hash = NULL WHERE pdu_hash = ''`); err != nil {
		return fmt.Errorf("upgrade message PDU hashes: %w", err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE sms_segments SET pdu_hash = NULL WHERE pdu_hash = ''`); err != nil {
		return fmt.Errorf("upgrade SMS segment PDU hashes: %w", err)
	}
	if _, err := database.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_sms_segments_pdu_hash ON sms_segments(pdu_hash) WHERE pdu_hash IS NOT NULL`); err != nil {
		return fmt.Errorf("upgrade SMS dedup index: %w", err)
	}
	// A call may contain multiple user-started recording segments. Only an
	// in-flight segment is exclusive so a new segment can be started after the
	// previous one has finished finalizing.
	if _, err := database.ExecContext(ctx, `DROP INDEX IF EXISTS idx_recordings_call; DROP INDEX IF EXISTS idx_recordings_active_call; CREATE UNIQUE INDEX IF NOT EXISTS idx_recordings_active_call ON recordings(call_id) WHERE state IN ('starting', 'recording', 'finalizing')`); err != nil {
		return fmt.Errorf("upgrade recording indexes: %w", err)
	}
	if _, err := database.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_recordings_time ON recordings(created_at DESC); CREATE INDEX IF NOT EXISTS idx_recordings_sync ON recordings(sync_seq)`); err != nil {
		return fmt.Errorf("upgrade recording indexes: %w", err)
	}
	return nil
}

func (database *DB) Close() error { return database.DB.Close() }

func (database *DB) CreateDevice(ctx context.Context, deviceID, name string, publicKey []byte) error {
	_, err := database.ExecContext(ctx, `INSERT INTO devices(id, name, public_key, created_at) VALUES (?, ?, ?, ?)`, deviceID, name, publicKey, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("create device: %w", err)
	}
	return nil
}

func (database *DB) StoreToken(ctx context.Context, tokenHash []byte, deviceID, kind string, createdAt, expiresAt time.Time) error {
	_, err := database.ExecContext(ctx, `INSERT INTO device_tokens(token_hash, device_id, kind, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`, tokenHash, deviceID, kind, createdAt.UnixMilli(), expiresAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("store device token: %w", err)
	}
	return nil
}

func (database *DB) LookupToken(ctx context.Context, tokenHash []byte, kind string, now time.Time) (string, error) {
	var deviceID string
	var revokedAt sql.NullInt64
	var deviceRevokedAt sql.NullInt64
	err := database.QueryRowContext(ctx, `SELECT t.device_id, t.revoked_at, d.revoked_at
		FROM device_tokens t JOIN devices d ON d.id = t.device_id
		WHERE t.token_hash = ? AND t.kind = ? AND t.expires_at > ?`, tokenHash, kind, now.UnixMilli()).Scan(&deviceID, &revokedAt, &deviceRevokedAt)
	if err != nil {
		return "", err
	}
	if revokedAt.Valid || deviceRevokedAt.Valid {
		return "", fmt.Errorf("device token revoked")
	}
	return deviceID, nil
}

func (database *DB) RevokeToken(ctx context.Context, tokenHash []byte, kind string) error {
	result, err := database.ExecContext(ctx, `UPDATE device_tokens SET revoked_at = ? WHERE token_hash = ? AND kind = ? AND revoked_at IS NULL`, time.Now().UnixMilli(), tokenHash, kind)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (database *DB) RevokeDevice(ctx context.Context, deviceID string) error {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	now := time.Now().UnixMilli()
	result, err := transaction.ExecContext(ctx, `UPDATE devices SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, deviceID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE device_tokens SET revoked_at = ? WHERE device_id = ?`, now, deviceID); err != nil {
		return err
	}
	return transaction.Commit()
}

func (database *DB) AppendAudit(ctx context.Context, deviceID, action, target, result string) error {
	_, err := database.ExecContext(ctx, `INSERT INTO audit_log(device_id, action, target, result, created_at) VALUES (?, ?, ?, ?, ?)`, deviceID, action, target, result, time.Now().UnixMilli())
	return err
}

func (database *DB) RegisterPush(ctx context.Context, deviceID, apnsToken, voipToken, environment, locale string) error {
	result, err := database.ExecContext(ctx, `UPDATE devices SET push_token = ?, voip_push_token = ?, push_environment = ?, push_locale = ? WHERE id = ? AND revoked_at IS NULL`, apnsToken, voipToken, environment, locale, deviceID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type IdempotencyRecord struct {
	StatusCode int
	Body       []byte
	Pending    bool
}

type PushTarget struct {
	DeviceID    string
	APNSToken   string
	VoIPToken   string
	Environment string
	Locale      string
}

func (database *DB) ListPushTargets(ctx context.Context) ([]PushTarget, error) {
	rows, err := database.QueryContext(ctx, `SELECT id, push_token, voip_push_token, push_environment, push_locale
		FROM devices WHERE revoked_at IS NULL AND push_token IS NOT NULL AND voip_push_token IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []PushTarget
	for rows.Next() {
		var target PushTarget
		var environment, locale sql.NullString
		if err := rows.Scan(&target.DeviceID, &target.APNSToken, &target.VoIPToken, &environment, &locale); err != nil {
			return nil, err
		}
		if environment.Valid {
			target.Environment = environment.String
		}
		if locale.Valid {
			target.Locale = locale.String
		}
		result = append(result, target)
	}
	return result, rows.Err()
}

// ClaimIdempotency atomically reserves a request key. A caller that receives
// an existing record must replay it (or return conflict while it is pending)
// instead of touching a modem a second time.
func (database *DB) ClaimIdempotency(ctx context.Context, deviceID, operation, requestKey string) (bool, IdempotencyRecord, error) {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, IdempotencyRecord{}, err
	}
	defer transaction.Rollback()
	result, err := transaction.ExecContext(ctx, `INSERT INTO idempotency_records(device_id, operation, request_key, status_code, response_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(device_id, operation, request_key) DO NOTHING`,
		deviceID, operation, requestKey, 102, []byte{}, time.Now().UnixMilli())
	if err != nil {
		return false, IdempotencyRecord{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, IdempotencyRecord{}, err
	}
	if changed == 1 {
		if err := transaction.Commit(); err != nil {
			return false, IdempotencyRecord{}, err
		}
		return true, IdempotencyRecord{StatusCode: 102, Pending: true}, nil
	}
	var record IdempotencyRecord
	var body []byte
	err = transaction.QueryRowContext(ctx, `SELECT status_code, response_json FROM idempotency_records
		WHERE device_id = ? AND operation = ? AND request_key = ?`, deviceID, operation, requestKey).Scan(&record.StatusCode, &body)
	if err != nil {
		return false, IdempotencyRecord{}, err
	}
	record.Body = append([]byte(nil), body...)
	record.Pending = record.StatusCode == 102
	if err := transaction.Commit(); err != nil {
		return false, IdempotencyRecord{}, err
	}
	return false, record, nil
}

func (database *DB) CompleteIdempotency(ctx context.Context, deviceID, operation, requestKey string, statusCode int, body []byte) error {
	if body == nil {
		body = []byte{}
	}
	result, err := database.ExecContext(ctx, `UPDATE idempotency_records SET status_code = ?, response_json = ?, created_at = ?
		WHERE device_id = ? AND operation = ? AND request_key = ?`, statusCode, body, time.Now().UnixMilli(), deviceID, operation, requestKey)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type Message struct {
	ID           string
	ThreadKey    string
	Direction    string
	Peer         string
	Body         string
	Encoding     string
	Status       string
	ServiceTime  *time.Time
	CreatedAt    time.Time
	ModemStorage string
	ModemIndex   *int
	PDUHash      string
	SyncSeq      int64
}

type Call struct {
	ID                  string
	Direction           string
	Peer                string
	State               string
	StartedAt           time.Time
	ConnectedAt         *time.Time
	EndedAt             *time.Time
	EndReason           string
	RecordingID         string
	RecordingState      string
	RecordingDurationMs *int64
	SyncSeq             int64
}

func (database *DB) SaveCall(ctx context.Context, call Call) (int64, error) {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer transaction.Rollback()
	sequence, err := appendSyncLog(ctx, transaction, "call", call.ID, "upsert")
	if err != nil {
		return 0, err
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO calls(id, direction, peer, state, started_at, connected_at, ended_at, end_reason, recording_id, recording_state, recording_duration_ms, sync_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET direction=excluded.direction, peer=excluded.peer, state=excluded.state,
		started_at=excluded.started_at, connected_at=excluded.connected_at, ended_at=excluded.ended_at,
		end_reason=excluded.end_reason, recording_id=COALESCE(excluded.recording_id, calls.recording_id),
		recording_state=COALESCE(excluded.recording_state, calls.recording_state),
		recording_duration_ms=COALESCE(excluded.recording_duration_ms, calls.recording_duration_ms), sync_seq=excluded.sync_seq`,
		call.ID, call.Direction, call.Peer, call.State, call.StartedAt.UnixMilli(), nullTime(call.ConnectedAt), nullTime(call.EndedAt), call.EndReason,
		nullString(call.RecordingID), nullString(call.RecordingState), nullInt64(call.RecordingDurationMs), sequence)
	if err != nil {
		return 0, err
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return sequence, nil
}

func (database *DB) GetCall(ctx context.Context, callID string) (Call, error) {
	var call Call
	var startedAt int64
	var connectedAt, endedAt sql.NullInt64
	var recordingID, recordingState sql.NullString
	var recordingDuration sql.NullInt64
	err := database.QueryRowContext(ctx, `SELECT id, direction, peer, state, started_at, connected_at, ended_at, end_reason, recording_id, recording_state, recording_duration_ms, sync_seq FROM calls WHERE id = ?`, callID).Scan(
		&call.ID, &call.Direction, &call.Peer, &call.State, &startedAt, &connectedAt, &endedAt, &call.EndReason, &recordingID, &recordingState, &recordingDuration, &call.SyncSeq)
	if err != nil {
		return Call{}, err
	}
	call.StartedAt = time.UnixMilli(startedAt)
	if connectedAt.Valid {
		value := time.UnixMilli(connectedAt.Int64)
		call.ConnectedAt = &value
	}
	if endedAt.Valid {
		value := time.UnixMilli(endedAt.Int64)
		call.EndedAt = &value
	}
	if recordingID.Valid {
		call.RecordingID = recordingID.String
	}
	if recordingState.Valid {
		call.RecordingState = recordingState.String
	}
	if recordingDuration.Valid {
		value := recordingDuration.Int64
		call.RecordingDurationMs = &value
	}
	return call, nil
}

func (database *DB) ListCalls(ctx context.Context, limit int) ([]Call, error) {
	if limit < 1 || limit > 500 {
		return nil, fmt.Errorf("call limit must be between 1 and 500")
	}
	rows, err := database.QueryContext(ctx, `SELECT id, direction, peer, state, started_at, connected_at, ended_at, end_reason, recording_id, recording_state, recording_duration_ms, sync_seq
		FROM calls WHERE deleted_at IS NULL ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var calls []Call
	for rows.Next() {
		var current Call
		var peer sql.NullString
		var recordingID, recordingState sql.NullString
		var recordingDuration sql.NullInt64
		var startedAt int64
		var connectedAt, endedAt sql.NullInt64
		if err := rows.Scan(&current.ID, &current.Direction, &peer, &current.State, &startedAt, &connectedAt, &endedAt, &current.EndReason, &recordingID, &recordingState, &recordingDuration, &current.SyncSeq); err != nil {
			return nil, err
		}
		if peer.Valid {
			current.Peer = peer.String
		}
		if recordingID.Valid {
			current.RecordingID = recordingID.String
		}
		if recordingState.Valid {
			current.RecordingState = recordingState.String
		}
		if recordingDuration.Valid {
			value := recordingDuration.Int64
			current.RecordingDurationMs = &value
		}
		current.StartedAt = time.UnixMilli(startedAt)
		if connectedAt.Valid {
			value := time.UnixMilli(connectedAt.Int64)
			current.ConnectedAt = &value
		}
		if endedAt.Valid {
			value := time.UnixMilli(endedAt.Int64)
			current.EndedAt = &value
		}
		calls = append(calls, current)
	}
	return calls, rows.Err()
}

// DeleteCall hides a completed call from the recents list without physically
// removing the row. Recordings reference calls, so a tombstone preserves the
// recording tab while still synchronizing a real call-history deletion.
func (database *DB) DeleteCall(ctx context.Context, callID string) (int64, error) {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer transaction.Rollback()
	result, err := transaction.ExecContext(ctx, `UPDATE calls SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, time.Now().UnixMilli(), callID)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed == 0 {
		return 0, sql.ErrNoRows
	}
	sequence, err := appendSyncLog(ctx, transaction, "call", callID, "delete")
	if err != nil {
		return 0, err
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE calls SET sync_seq = ? WHERE id = ?`, sequence, callID); err != nil {
		return 0, err
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return sequence, nil
}

type Recording struct {
	ID                 string
	CallID             string
	State              string
	TriggerMode        string
	StartedAt          *time.Time
	StoppedAt          *time.Time
	DurationMs         *int64
	FilePath           string
	Container          string
	Codec              string
	SampleRate         int
	Channels           int
	Bitrate            int
	SizeBytes          *int64
	SHA256             string
	WaveformPath       string
	FailureCode        string
	FailureDetail      string
	RetentionExpiresAt *time.Time
	CreatedByDeviceID  string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	SyncSeq            int64
}

type RecordingSettings struct {
	Enabled          bool
	AutoMode         string
	RetentionDays    int
	MinimumFreeBytes int64
	UpdatedAt        time.Time
}

func (database *DB) EnsureRecordingSettings(ctx context.Context, settings RecordingSettings) error {
	updatedAt := settings.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	_, err := database.ExecContext(ctx, `INSERT INTO recording_settings(id, enabled, auto_mode, retention_days, minimum_free_bytes, updated_at)
		VALUES (1, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, boolInt(settings.Enabled), settings.AutoMode, settings.RetentionDays, settings.MinimumFreeBytes, updatedAt.UnixMilli())
	return err
}

func (database *DB) GetRecordingSettings(ctx context.Context) (RecordingSettings, error) {
	var settings RecordingSettings
	var enabled int
	var updatedAt int64
	err := database.QueryRowContext(ctx, `SELECT enabled, auto_mode, retention_days, minimum_free_bytes, updated_at FROM recording_settings WHERE id = 1`).Scan(
		&enabled, &settings.AutoMode, &settings.RetentionDays, &settings.MinimumFreeBytes, &updatedAt)
	if err != nil {
		return RecordingSettings{}, err
	}
	settings.Enabled = enabled != 0
	settings.UpdatedAt = time.UnixMilli(updatedAt)
	return settings, nil
}

func (database *DB) SaveRecordingSettings(ctx context.Context, settings RecordingSettings) error {
	updatedAt := settings.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	_, err := database.ExecContext(ctx, `INSERT INTO recording_settings(id, enabled, auto_mode, retention_days, minimum_free_bytes, updated_at)
		VALUES (1, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, auto_mode=excluded.auto_mode,
		retention_days=excluded.retention_days, minimum_free_bytes=excluded.minimum_free_bytes, updated_at=excluded.updated_at`,
		boolInt(settings.Enabled), settings.AutoMode, settings.RetentionDays, settings.MinimumFreeBytes, updatedAt.UnixMilli())
	return err
}

func (database *DB) SaveRecording(ctx context.Context, recording Recording) (int64, error) {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer transaction.Rollback()
	sequence, err := appendSyncLog(ctx, transaction, "recording", recording.ID, "upsert")
	if err != nil {
		return 0, err
	}
	now := recording.UpdatedAt
	if now.IsZero() {
		now = time.Now()
	}
	created := recording.CreatedAt
	if created.IsZero() {
		created = now
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO recordings
		(id, call_id, state, trigger_mode, started_at, stopped_at, duration_ms, file_path, container, codec, sample_rate, channels, bitrate, size_bytes, sha256, waveform_path, failure_code, failure_detail, retention_expires_at, created_by_device_id, created_at, updated_at, sync_seq, path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET state=excluded.state, trigger_mode=excluded.trigger_mode,
		started_at=excluded.started_at, stopped_at=excluded.stopped_at, duration_ms=excluded.duration_ms,
		file_path=excluded.file_path, container=excluded.container, codec=excluded.codec,
		sample_rate=excluded.sample_rate, channels=excluded.channels, bitrate=excluded.bitrate,
		size_bytes=excluded.size_bytes, sha256=excluded.sha256, waveform_path=excluded.waveform_path,
		failure_code=excluded.failure_code, failure_detail=excluded.failure_detail,
		retention_expires_at=excluded.retention_expires_at, created_by_device_id=excluded.created_by_device_id,
		updated_at=excluded.updated_at, sync_seq=excluded.sync_seq, path=excluded.path`,
		recording.ID, recording.CallID, recording.State, recording.TriggerMode,
		nullTime(recording.StartedAt), nullTime(recording.StoppedAt), nullInt64(recording.DurationMs),
		nullString(recording.FilePath), nullString(recording.Container), nullString(recording.Codec),
		recording.SampleRate, recording.Channels, recording.Bitrate, nullInt64(recording.SizeBytes),
		nullString(recording.SHA256), nullString(recording.WaveformPath), nullString(recording.FailureCode),
		nullString(recording.FailureDetail), nullTime(recording.RetentionExpiresAt), nullString(recording.CreatedByDeviceID),
		created.UnixMilli(), now.UnixMilli(), sequence, recording.FilePath)
	if err != nil {
		return 0, fmt.Errorf("save recording: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return sequence, nil
}

func (database *DB) GetRecording(ctx context.Context, recordingID string) (Recording, error) {
	row := database.QueryRowContext(ctx, recordingSelect+` WHERE id = ?`, recordingID)
	return scanRecording(row)
}

func (database *DB) GetRecordingByCall(ctx context.Context, callID string) (Recording, error) {
	row := database.QueryRowContext(ctx, recordingSelect+` WHERE call_id = ? AND state != 'deleted' ORDER BY created_at DESC LIMIT 1`, callID)
	return scanRecording(row)
}

func (database *DB) ListRecordings(ctx context.Context, after int64, limit int, direction, query string, from, to *time.Time) ([]Recording, error) {
	if limit < 1 || limit > 500 {
		return nil, fmt.Errorf("recording limit must be between 1 and 500")
	}
	statement := recordingSelect + ` WHERE state != 'deleted' AND sync_seq > ?`
	args := []any{after}
	if direction != "" {
		if direction != "inbound" && direction != "outbound" {
			return nil, fmt.Errorf("recording direction must be inbound or outbound")
		}
		statement += ` AND call_id IN (SELECT id FROM calls WHERE direction = ?)`
		args = append(args, direction)
	}
	if query != "" {
		statement += ` AND call_id IN (SELECT id FROM calls WHERE peer LIKE ?)`
		args = append(args, "%"+query+"%")
	}
	if from != nil {
		statement += ` AND created_at >= ?`
		args = append(args, from.UnixMilli())
	}
	if to != nil {
		statement += ` AND created_at <= ?`
		args = append(args, to.UnixMilli())
	}
	statement += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := database.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Recording
	for rows.Next() {
		recording, scanErr := scanRecording(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, recording)
	}
	return result, rows.Err()
}

const recordingSelect = `SELECT id, call_id, state, trigger_mode, started_at, stopped_at, duration_ms, file_path, container, codec, sample_rate, channels, bitrate, size_bytes, sha256, waveform_path, failure_code, failure_detail, retention_expires_at, created_by_device_id, created_at, updated_at, sync_seq FROM recordings`

type rowScanner interface{ Scan(...any) error }

func scanRecording(row rowScanner) (Recording, error) {
	var recording Recording
	var startedAt, stoppedAt, duration, size, retention, createdAt, updatedAt sql.NullInt64
	var filePath, container, codec, sha256, waveform, failureCode, failureDetail, deviceID sql.NullString
	var sampleRate, channels, bitrate int
	err := row.Scan(&recording.ID, &recording.CallID, &recording.State, &recording.TriggerMode, &startedAt, &stoppedAt, &duration,
		&filePath, &container, &codec, &sampleRate, &channels, &bitrate, &size, &sha256, &waveform, &failureCode, &failureDetail,
		&retention, &deviceID, &createdAt, &updatedAt, &recording.SyncSeq)
	if err != nil {
		return Recording{}, err
	}
	if startedAt.Valid {
		value := time.UnixMilli(startedAt.Int64)
		recording.StartedAt = &value
	}
	if stoppedAt.Valid {
		value := time.UnixMilli(stoppedAt.Int64)
		recording.StoppedAt = &value
	}
	if duration.Valid {
		value := duration.Int64
		recording.DurationMs = &value
	}
	if size.Valid {
		value := size.Int64
		recording.SizeBytes = &value
	}
	if retention.Valid {
		value := time.UnixMilli(retention.Int64)
		recording.RetentionExpiresAt = &value
	}
	if createdAt.Valid {
		recording.CreatedAt = time.UnixMilli(createdAt.Int64)
	}
	if updatedAt.Valid {
		recording.UpdatedAt = time.UnixMilli(updatedAt.Int64)
	}
	recording.FilePath, recording.Container, recording.Codec = filePath.String, container.String, codec.String
	recording.SHA256, recording.WaveformPath = sha256.String, waveform.String
	recording.FailureCode, recording.FailureDetail, recording.CreatedByDeviceID = failureCode.String, failureDetail.String, deviceID.String
	recording.SampleRate, recording.Channels, recording.Bitrate = sampleRate, channels, bitrate
	return recording, nil
}

func (database *DB) UpdateCallRecording(ctx context.Context, callID, recordingID, state string, durationMs *int64) error {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	sequence, err := appendSyncLog(ctx, transaction, "call", callID, "upsert")
	if err != nil {
		return err
	}
	result, err := transaction.ExecContext(ctx, `UPDATE calls SET recording_id = ?, recording_state = ?, recording_duration_ms = ?, sync_seq = ? WHERE id = ?`, recordingID, state, nullInt64(durationMs), sequence, callID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return transaction.Commit()
}

func (database *DB) ListMessages(ctx context.Context, after int64, limit int) ([]Message, error) {
	if limit < 1 || limit > 500 {
		return nil, fmt.Errorf("message limit must be between 1 and 500")
	}
	rows, err := database.QueryContext(ctx, `SELECT id, thread_key, direction, peer, body, encoding, status, service_time, created_at, modem_storage, modem_index, pdu_hash, sync_seq
		FROM messages WHERE sync_seq > ? ORDER BY created_at ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var message Message
		var serviceTime, createdAt sql.NullInt64
		var modemIndex sql.NullInt64
		var pduHash sql.NullString
		if err := rows.Scan(&message.ID, &message.ThreadKey, &message.Direction, &message.Peer, &message.Body, &message.Encoding, &message.Status, &serviceTime, &createdAt, &message.ModemStorage, &modemIndex, &pduHash, &message.SyncSeq); err != nil {
			return nil, err
		}
		if pduHash.Valid {
			message.PDUHash = pduHash.String
		}
		if serviceTime.Valid {
			value := time.UnixMilli(serviceTime.Int64)
			message.ServiceTime = &value
		}
		if createdAt.Valid {
			message.CreatedAt = time.UnixMilli(createdAt.Int64)
		}
		if modemIndex.Valid {
			value := int(modemIndex.Int64)
			message.ModemIndex = &value
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// ListThreadMessages returns one chronological page for a single thread.
// A nil cursor starts at the newest messages; a cursor returns messages older
// than (createdAt, id). The database query reads newest-first so the result is
// reversed before returning, keeping the API contract chronological.
func (database *DB) ListThreadMessages(ctx context.Context, threadKey string, beforeCreatedAt *int64, beforeID string, limit int) ([]Message, error) {
	if strings.TrimSpace(threadKey) == "" {
		return nil, fmt.Errorf("thread key must not be empty")
	}
	if limit < 1 || limit > 501 {
		return nil, fmt.Errorf("thread message limit must be between 1 and 501")
	}

	query := `SELECT id, thread_key, direction, peer, body, encoding, status, service_time, created_at, modem_storage, modem_index, pdu_hash, sync_seq
		FROM messages WHERE thread_key = ?`
	args := []any{threadKey}
	if beforeCreatedAt != nil {
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, *beforeCreatedAt, *beforeCreatedAt, beforeID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var message Message
		var serviceTime, createdAt sql.NullInt64
		var modemIndex sql.NullInt64
		var pduHash sql.NullString
		if err := rows.Scan(&message.ID, &message.ThreadKey, &message.Direction, &message.Peer, &message.Body, &message.Encoding, &message.Status, &serviceTime, &createdAt, &message.ModemStorage, &modemIndex, &pduHash, &message.SyncSeq); err != nil {
			return nil, err
		}
		if pduHash.Valid {
			message.PDUHash = pduHash.String
		}
		if serviceTime.Valid {
			value := time.UnixMilli(serviceTime.Int64)
			message.ServiceTime = &value
		}
		if createdAt.Valid {
			message.CreatedAt = time.UnixMilli(createdAt.Int64)
		}
		if modemIndex.Valid {
			value := int(modemIndex.Int64)
			message.ModemIndex = &value
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	return messages, nil
}

type Thread struct {
	Key         string
	Peer        string
	UnreadCount int
	LastMessage Message
}

func (database *DB) ListThreads(ctx context.Context) ([]Thread, error) {
	rows, err := database.QueryContext(ctx, `SELECT id, thread_key, direction, peer, body, encoding, status, created_at
		FROM messages ORDER BY thread_key ASC, created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	threads := make(map[string]*Thread)
	var order []string
	for rows.Next() {
		var message Message
		var createdAt int64
		if err := rows.Scan(&message.ID, &message.ThreadKey, &message.Direction, &message.Peer, &message.Body, &message.Encoding, &message.Status, &createdAt); err != nil {
			return nil, err
		}
		message.CreatedAt = time.UnixMilli(createdAt)
		thread := threads[message.ThreadKey]
		if thread == nil {
			thread = &Thread{Key: message.ThreadKey, Peer: message.Peer, LastMessage: message}
			threads[message.ThreadKey] = thread
			order = append(order, message.ThreadKey)
		}
		if message.Direction == "inbound" && message.Status != "read" {
			thread.UnreadCount++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]Thread, 0, len(order))
	for _, key := range order {
		result = append(result, *threads[key])
	}
	return result, nil
}

func (database *DB) MarkMessageRead(ctx context.Context, messageID string) error {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	result, err := transaction.ExecContext(ctx, `UPDATE messages SET status = 'read', sync_seq = ? WHERE id = ?`, 0, messageID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		if err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	sequence, err := appendSyncLog(ctx, transaction, "message", messageID, "upsert")
	if err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE messages SET sync_seq = ? WHERE id = ?`, sequence, messageID); err != nil {
		return err
	}
	return transaction.Commit()
}

func (database *DB) DeleteMessage(ctx context.Context, messageID string) error {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	result, err := transaction.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, messageID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		if err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if _, err := appendSyncLog(ctx, transaction, "message", messageID, "delete"); err != nil {
		return err
	}
	return transaction.Commit()
}

type Change struct {
	Seq        int64  `json:"seq"`
	EntityType string `json:"type"`
	EntityID   string `json:"id"`
	Operation  string `json:"op"`
}

func (database *DB) InsertMessage(ctx context.Context, message Message) (int64, error) {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer transaction.Rollback()
	sequence, err := appendSyncLog(ctx, transaction, "message", message.ID, "upsert")
	if err != nil {
		return 0, err
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO messages
		(id, thread_key, direction, peer, body, encoding, status, service_time, created_at, modem_storage, modem_index, pdu_hash, sync_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ID, message.ThreadKey, message.Direction, message.Peer, message.Body, message.Encoding, message.Status,
		nullTime(message.ServiceTime), message.CreatedAt.UnixMilli(), message.ModemStorage, nullInt(message.ModemIndex), nullString(message.PDUHash), sequence)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return sequence, nil
}

// UpdateMessageStatus records each SMS delivery milestone in the same sync
// log used by the client. In particular, an accepted modem submission must
// survive a later serial/network error as submitted rather than becoming a
// local-only or falsely failed message.
func (database *DB) UpdateMessageStatus(ctx context.Context, messageID, status string) (int64, error) {
	if strings.TrimSpace(messageID) == "" || strings.TrimSpace(status) == "" {
		return 0, fmt.Errorf("message ID and status are required")
	}
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer transaction.Rollback()
	sequence, err := appendSyncLog(ctx, transaction, "message", messageID, "upsert")
	if err != nil {
		return 0, err
	}
	result, err := transaction.ExecContext(ctx, `UPDATE messages SET status = ?, sync_seq = ? WHERE id = ?`, status, sequence, messageID)
	if err != nil {
		return 0, fmt.Errorf("update message status: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed == 0 {
		return 0, sql.ErrNoRows
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return sequence, nil
}

func (database *DB) MessageExistsByPDUHash(ctx context.Context, pduHash string) (bool, error) {
	var exists int
	err := database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE pdu_hash = ?)`, pduHash).Scan(&exists)
	return exists == 1, err
}

type SMSSegment struct {
	ID         string
	MessageID  *string
	Sender     string
	ConcatRef  uint8
	PartNo     int
	TotalParts int
	RawPDU     []byte
	PDUHash    string
	ReceivedAt time.Time
}

func (database *DB) InsertSMSSegment(ctx context.Context, segment SMSSegment) error {
	_, err := database.ExecContext(ctx, `INSERT INTO sms_segments(id, message_id, sender, concat_ref, part_no, total_parts, raw_pdu, pdu_hash, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, segment.ID, segment.MessageID, segment.Sender, segment.ConcatRef, segment.PartNo, segment.TotalParts, segment.RawPDU, segment.PDUHash, segment.ReceivedAt.UnixMilli())
	return err
}

func (database *DB) ListPendingSMSSegments(ctx context.Context, sender string, concatRef uint8, totalParts int) ([]SMSSegment, error) {
	rows, err := database.QueryContext(ctx, `SELECT id, message_id, sender, concat_ref, part_no, total_parts, raw_pdu, pdu_hash, received_at
		FROM sms_segments WHERE message_id IS NULL AND sender = ? AND concat_ref = ? AND total_parts = ? ORDER BY part_no ASC`, sender, concatRef, totalParts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SMSSegment
	for rows.Next() {
		var segment SMSSegment
		var messageID, storedSender, pduHash sql.NullString
		var concat, part, total, receivedAt int64
		if err := rows.Scan(&segment.ID, &messageID, &storedSender, &concat, &part, &total, &segment.RawPDU, &pduHash, &receivedAt); err != nil {
			return nil, err
		}
		if messageID.Valid {
			segment.MessageID = &messageID.String
		}
		if storedSender.Valid {
			segment.Sender = storedSender.String
		}
		segment.ConcatRef = uint8(concat)
		segment.PartNo = int(part)
		segment.TotalParts = int(total)
		if pduHash.Valid {
			segment.PDUHash = pduHash.String
		}
		segment.ReceivedAt = time.UnixMilli(receivedAt)
		result = append(result, segment)
	}
	return result, rows.Err()
}

func (database *DB) LinkSMSSegment(ctx context.Context, pduHash, messageID string) error {
	_, err := database.ExecContext(ctx, `UPDATE sms_segments SET message_id = ? WHERE pdu_hash = ? AND message_id IS NULL`, messageID, pduHash)
	return err
}

func (database *DB) PrunePendingSMSSegments(ctx context.Context, before time.Time) error {
	_, err := database.ExecContext(ctx, `DELETE FROM sms_segments WHERE message_id IS NULL AND received_at < ?`, before.UnixMilli())
	return err
}

func (database *DB) SMSSegmentExistsByPDUHash(ctx context.Context, pduHash string) (bool, error) {
	var exists int
	err := database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sms_segments WHERE pdu_hash = ?)`, pduHash).Scan(&exists)
	return exists == 1, err
}

func (database *DB) AppendChange(ctx context.Context, entityType, entityID, operation string) (int64, error) {
	return appendSyncLog(ctx, database.DB, entityType, entityID, operation)
}

func (database *DB) Sync(ctx context.Context, after int64, limit int) ([]Change, error) {
	if limit < 1 || limit > 500 {
		return nil, fmt.Errorf("sync limit must be between 1 and 500")
	}
	rows, err := database.QueryContext(ctx, `SELECT seq, entity_type, entity_id, operation FROM sync_log WHERE seq > ? ORDER BY seq ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var changes []Change
	for rows.Next() {
		var change Change
		if err := rows.Scan(&change.Seq, &change.EntityType, &change.EntityID, &change.Operation); err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

func appendSyncLog(ctx context.Context, executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, entityType, entityID, operation string) (int64, error) {
	result, err := executor.ExecContext(ctx, `INSERT INTO sync_log(entity_type, entity_id, operation, created_at) VALUES (?, ?, ?, ?)`, entityType, entityID, operation, time.Now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("append sync log: %w", err)
	}
	return result.LastInsertId()
}

func nullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UnixMilli()
}

func nullInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func ensureColumn(ctx context.Context, executor *sql.DB, table, column, declaration string) error {
	rows, err := executor.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var (
		cid          int
		name         string
		columnType   string
		notNull      int
		defaultValue any
		primaryKey   int
	)
	for rows.Next() {
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = executor.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+declaration)
	return err
}
