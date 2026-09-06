ALTER TABLE sms_segments ADD COLUMN pdu_hash TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_sms_segments_pdu_hash ON sms_segments(pdu_hash) WHERE pdu_hash IS NOT NULL;
