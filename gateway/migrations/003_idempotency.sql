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
