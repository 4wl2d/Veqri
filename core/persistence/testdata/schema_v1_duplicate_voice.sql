INSERT INTO schema_migrations(version, applied_at)
VALUES(1, '2026-01-01T00:00:00Z');

INSERT INTO devices(id, name, platform, credential_hash, capabilities_json, created_at, key_version)
VALUES
    ('device-a', 'Primary phone', 'android', X'01', '{}', '2026-01-01T00:00:00Z', 1),
    ('device-b', 'Secondary phone', 'android', X'02', '{}', '2026-01-01T00:00:00Z', 1);

INSERT INTO conversations(id, external_key, title, transcript_retention, created_at, updated_at)
VALUES
    ('conversation-old', 'fixture:old', 'Old call', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
    ('conversation-tie-a', 'fixture:tie-a', 'Tie A', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
    ('conversation-tie-z', 'fixture:tie-z', 'Tie Z', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
    ('conversation-ended', 'fixture:ended', 'Ended call', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
    ('conversation-single', 'fixture:single', 'Single call', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');

INSERT INTO voice_sessions(
    id, conversation_id, device_id, state, transport, interrupted,
    started_at, ended_at, correlation_id
)
VALUES
    ('voice-old', 'conversation-old', 'device-a', 'LISTENING', 'simulated', 0,
        '2026-01-01T10:00:00Z', NULL, 'correlation-old'),
    ('voice-tie-a', 'conversation-tie-a', 'device-a', 'CONNECTING', 'simulated', 0,
        '2026-01-01T12:00:00Z', NULL, 'correlation-tie-a'),
    ('voice-tie-z', 'conversation-tie-z', 'device-a', 'RINGING', 'simulated', 0,
        '2026-01-01T12:00:00Z', NULL, 'correlation-tie-z'),
    ('voice-ended', 'conversation-ended', 'device-a', 'ENDED', 'simulated', 0,
        '2026-01-01T09:00:00Z', '2026-01-01T09:30:00Z', 'correlation-ended'),
    ('voice-single', 'conversation-single', 'device-b', 'SPEAKING', 'simulated', 0,
        '2026-01-01T11:00:00Z', NULL, 'correlation-single');
