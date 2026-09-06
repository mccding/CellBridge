PRAGMA journal_mode = WAL;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS gateways (
    id TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    schema_version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS devices (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    public_key BLOB NOT NULL,
    push_token TEXT,
    voip_push_token TEXT,
    push_environment TEXT,
    push_locale TEXT,
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
);

CREATE TABLE IF NOT EXISTS device_tokens (
    token_hash BLOB PRIMARY KEY,
    device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('access', 'refresh')),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_device_tokens_device ON device_tokens(device_id);

CREATE TABLE IF NOT EXISTS idempotency_records (
    device_id TEXT NOT NULL,
    operation TEXT NOT NULL,
    request_key TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    response_json BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (device_id, operation, request_key)
);
CREATE INDEX IF NOT EXISTS idx_idempotency_created ON idempotency_records(created_at);

CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    thread_key TEXT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    peer TEXT NOT NULL,
    body TEXT NOT NULL,
    encoding TEXT NOT NULL,
    status TEXT NOT NULL,
    service_time INTEGER,
    created_at INTEGER NOT NULL,
    modem_storage TEXT,
    modem_index INTEGER,
    pdu_hash TEXT,
    sync_seq INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_thread_time ON messages(thread_key, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_sync ON messages(sync_seq);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_pdu_hash ON messages(pdu_hash) WHERE pdu_hash IS NOT NULL;

CREATE TABLE IF NOT EXISTS sms_segments (
    id TEXT PRIMARY KEY,
    message_id TEXT,
    sender TEXT,
    concat_ref TEXT,
    part_no INTEGER,
    total_parts INTEGER,
    raw_pdu BLOB,
    pdu_hash TEXT,
    received_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sms_segments_pdu_hash ON sms_segments(pdu_hash) WHERE pdu_hash IS NOT NULL;

CREATE TABLE IF NOT EXISTS calls (
    id TEXT PRIMARY KEY,
    direction TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    peer TEXT,
    state TEXT NOT NULL,
    started_at INTEGER NOT NULL,
    connected_at INTEGER,
    ended_at INTEGER,
    end_reason TEXT,
    recording_id TEXT,
    recording_state TEXT,
    recording_duration_ms INTEGER,
    sync_seq INTEGER NOT NULL,
    deleted_at INTEGER
);

CREATE TABLE IF NOT EXISTS recordings (
    id TEXT PRIMARY KEY,
    call_id TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'starting',
    trigger_mode TEXT NOT NULL DEFAULT 'manual',
    started_at INTEGER,
    stopped_at INTEGER,
    duration_ms INTEGER,
    file_path TEXT,
    container TEXT,
    codec TEXT,
    sample_rate INTEGER,
    channels INTEGER,
    bitrate INTEGER,
    size_bytes INTEGER,
    sha256 TEXT,
    waveform_path TEXT,
    failure_code TEXT,
    failure_detail TEXT,
    retention_expires_at INTEGER,
    created_by_device_id TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL DEFAULT 0,
    sync_seq INTEGER NOT NULL DEFAULT 0,
    path TEXT NOT NULL DEFAULT '',
    FOREIGN KEY(call_id) REFERENCES calls(id)
);

CREATE TABLE IF NOT EXISTS recording_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    enabled INTEGER NOT NULL DEFAULT 1,
    auto_mode TEXT NOT NULL DEFAULT 'off',
    retention_days INTEGER NOT NULL DEFAULT 90,
    minimum_free_bytes INTEGER NOT NULL DEFAULT 524288000,
    updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sync_log (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    operation TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id TEXT,
    action TEXT NOT NULL,
    target TEXT,
    result TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
