ALTER TABLE voice_sessions ADD COLUMN direction TEXT NOT NULL DEFAULT 'OUTGOING';
ALTER TABLE voice_sessions ADD COLUMN muted INTEGER NOT NULL DEFAULT 0;
ALTER TABLE voice_sessions ADD COLUMN push_to_talk INTEGER NOT NULL DEFAULT 0;
ALTER TABLE voice_sessions ADD COLUMN audio_route TEXT NOT NULL DEFAULT 'EARPIECE';

CREATE UNIQUE INDEX IF NOT EXISTS idx_voice_one_active_device
ON voice_sessions(device_id)
WHERE device_id IS NOT NULL AND state NOT IN ('ENDED', 'FAILED');
