CREATE TABLE IF NOT EXISTS device_tokens (
    token_hash BLOB PRIMARY KEY,
    device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('access', 'refresh')),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_device_tokens_device ON device_tokens(device_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_pdu_hash ON messages(pdu_hash) WHERE pdu_hash IS NOT NULL;

