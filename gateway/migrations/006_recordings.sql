-- The embedded migrator applies this upgrade idempotently. The ALTER TABLE
-- statements below are the human-readable one-time migration plan for
-- installations that do not use the embedded migrator.
ALTER TABLE calls ADD COLUMN recording_state TEXT;
ALTER TABLE calls ADD COLUMN recording_duration_ms INTEGER;
ALTER TABLE recordings ADD COLUMN state TEXT NOT NULL DEFAULT 'ready';
ALTER TABLE recordings ADD COLUMN trigger_mode TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE recordings ADD COLUMN started_at INTEGER;
ALTER TABLE recordings ADD COLUMN stopped_at INTEGER;
ALTER TABLE recordings ADD COLUMN file_path TEXT;
ALTER TABLE recordings ADD COLUMN container TEXT;
ALTER TABLE recordings ADD COLUMN codec TEXT;
ALTER TABLE recordings ADD COLUMN sample_rate INTEGER;
ALTER TABLE recordings ADD COLUMN channels INTEGER;
ALTER TABLE recordings ADD COLUMN bitrate INTEGER;
ALTER TABLE recordings ADD COLUMN size_bytes INTEGER;
ALTER TABLE recordings ADD COLUMN sha256 TEXT;
ALTER TABLE recordings ADD COLUMN waveform_path TEXT;
ALTER TABLE recordings ADD COLUMN failure_code TEXT;
ALTER TABLE recordings ADD COLUMN failure_detail TEXT;
ALTER TABLE recordings ADD COLUMN retention_expires_at INTEGER;
ALTER TABLE recordings ADD COLUMN created_by_device_id TEXT;
ALTER TABLE recordings ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE recordings ADD COLUMN sync_seq INTEGER NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS recording_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    enabled INTEGER NOT NULL DEFAULT 1,
    auto_mode TEXT NOT NULL DEFAULT 'off',
    retention_days INTEGER NOT NULL DEFAULT 90,
    minimum_free_bytes INTEGER NOT NULL DEFAULT 524288000,
    updated_at INTEGER NOT NULL
);
