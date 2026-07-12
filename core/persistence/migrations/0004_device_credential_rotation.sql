ALTER TABLE devices ADD COLUMN pending_credential_hash BLOB;
ALTER TABLE devices ADD COLUMN pending_key_version INTEGER;
ALTER TABLE devices ADD COLUMN pending_credential_expires_at TEXT;

CREATE INDEX IF NOT EXISTS idx_devices_pending_credential
ON devices(pending_credential_expires_at)
WHERE pending_credential_hash IS NOT NULL;
