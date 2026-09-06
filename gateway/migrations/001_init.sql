PRAGMA journal_mode = WAL;

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
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
);

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

CREATE TABLE IF NOT EXISTS sms_segments (
    id TEXT PRIMARY KEY,
    message_id TEXT,
    sender TEXT,
    concat_ref TEXT,
    part_no INTEGER,
    total_parts INTEGER,
    raw_pdu BLOB,
    received_at INTEGER NOT NULL
);

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
    sync_seq INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS recordings (
    id TEXT PRIMARY KEY,
    call_id TEXT NOT NULL,
    path TEXT NOT NULL,
    duration_ms INTEGER,
    created_at INTEGER NOT NULL
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

