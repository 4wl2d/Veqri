CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS devices (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    platform TEXT NOT NULL,
    credential_hash BLOB NOT NULL,
    capabilities_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    last_seen_at TEXT,
    revoked_at TEXT,
    key_version INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS pairing_sessions (
    id TEXT PRIMARY KEY,
    code_hash BLOB NOT NULL UNIQUE,
    expires_at TEXT NOT NULL,
    consumed_at TEXT,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS connectors (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    name TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 0,
    config_json TEXT NOT NULL DEFAULT '{}',
    secret_refs_json TEXT NOT NULL DEFAULT '{}',
    health TEXT NOT NULL DEFAULT 'unknown',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS conversations (
    id TEXT PRIMARY KEY,
    external_key TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL,
    transcript_retention INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS turns (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    text TEXT NOT NULL,
    final INTEGER NOT NULL,
    correlation_id TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_turns_conversation_created ON turns(conversation_id, created_at);

CREATE TABLE IF NOT EXISTS events (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    version INTEGER NOT NULL,
    source_kind TEXT NOT NULL,
    source_connector_id TEXT NOT NULL DEFAULT '',
    source_instance_id TEXT NOT NULL DEFAULT '',
    actor_id TEXT NOT NULL,
    actor_display_name TEXT NOT NULL DEFAULT '',
    occurred_at TEXT NOT NULL,
    received_at TEXT NOT NULL,
    conversation_key TEXT NOT NULL,
    correlation_id TEXT NOT NULL,
    causation_id TEXT,
    idempotency_key TEXT NOT NULL,
    trust_level TEXT NOT NULL,
    reply_target_json TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    processing_status TEXT NOT NULL DEFAULT 'PENDING',
    processing_error TEXT,
    processed_at TEXT,
    UNIQUE(source_kind, source_connector_id, source_instance_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_events_pending ON events(processing_status, received_at);
CREATE INDEX IF NOT EXISTS idx_events_correlation ON events(correlation_id);

CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    parent_task_id TEXT REFERENCES tasks(id),
    root_task_id TEXT NOT NULL,
    conversation_id TEXT REFERENCES conversations(id),
    goal TEXT NOT NULL,
    task_type TEXT NOT NULL,
    input_json TEXT NOT NULL,
    assigned_agent_id TEXT NOT NULL,
    allowed_tools_json TEXT NOT NULL,
    approval_policy TEXT NOT NULL,
    status TEXT NOT NULL,
    progress INTEGER NOT NULL DEFAULT 0 CHECK(progress >= 0 AND progress <= 100),
    progress_message TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    retry_count INTEGER NOT NULL DEFAULT 0,
    max_retries INTEGER NOT NULL DEFAULT 0,
    timeout_seconds INTEGER NOT NULL DEFAULT 300,
    error TEXT NOT NULL DEFAULT '',
    result_json TEXT,
    user_facing_summary TEXT NOT NULL DEFAULT '',
    artifacts_json TEXT NOT NULL DEFAULT '[]',
    correlation_id TEXT NOT NULL,
    causation_id TEXT,
    idempotency_key TEXT NOT NULL UNIQUE,
    version INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_tasks_runnable ON tasks(status, created_at);
CREATE INDEX IF NOT EXISTS idx_tasks_root ON tasks(root_task_id, created_at);
CREATE INDEX IF NOT EXISTS idx_tasks_conversation ON tasks(conversation_id, created_at);

CREATE TABLE IF NOT EXISTS task_dependencies (
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    depends_on_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    PRIMARY KEY(task_id, depends_on_task_id),
    CHECK(task_id <> depends_on_task_id)
);

CREATE TABLE IF NOT EXISTS agents (
    id TEXT PRIMARY KEY,
    definition_json TEXT NOT NULL,
    health TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    kill_switch INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tool_invocations (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    tool_name TEXT NOT NULL,
    input_json TEXT NOT NULL,
    risk TEXT NOT NULL,
    status TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    exit_code INTEGER,
    output_json TEXT,
    error TEXT NOT NULL DEFAULT '',
    correlation_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE
);
CREATE INDEX IF NOT EXISTS idx_invocations_task ON tool_invocations(task_id);

CREATE TABLE IF NOT EXISTS approvals (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    tool_name TEXT NOT NULL,
    tool_arguments_json TEXT NOT NULL,
    requested_scopes_json TEXT NOT NULL,
    risk TEXT NOT NULL,
    reason TEXT NOT NULL,
    status TEXT NOT NULL,
    requested_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    decided_at TEXT,
    decided_by TEXT NOT NULL DEFAULT '',
    consumed_at TEXT,
    correlation_id TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_approvals_pending ON approvals(status, expires_at);

CREATE TABLE IF NOT EXISTS deliveries (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    target_json TEXT NOT NULL,
    priority INTEGER NOT NULL,
    status TEXT NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    delivered_at TEXT,
    correlation_id TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deliveries_pending ON deliveries(status, priority DESC, created_at);

CREATE TABLE IF NOT EXISTS artifacts (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    name TEXT NOT NULL,
    media_type TEXT NOT NULL,
    uri TEXT NOT NULL,
    sha256 TEXT,
    size_bytes INTEGER,
    description TEXT,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS policies (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    priority INTEGER NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    rule_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_entries (
    id TEXT PRIMARY KEY,
    occurred_at TEXT NOT NULL,
    actor_kind TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    action TEXT NOT NULL,
    resource_kind TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    decision TEXT,
    details_json TEXT NOT NULL,
    correlation_id TEXT NOT NULL,
    task_id TEXT,
    conversation_id TEXT,
    connector_id TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_occurred ON audit_entries(occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_correlation ON audit_entries(correlation_id);

CREATE TABLE IF NOT EXISTS voice_sessions (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    device_id TEXT REFERENCES devices(id),
    state TEXT NOT NULL,
    transport TEXT NOT NULL,
    interrupted INTEGER NOT NULL DEFAULT 0,
    started_at TEXT NOT NULL,
    ended_at TEXT,
    correlation_id TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS provider_configurations (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    provider TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 0,
    config_json TEXT NOT NULL DEFAULT '{}',
    secret_refs_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS secret_references (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    locator TEXT NOT NULL,
    purpose TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(provider, locator)
);

CREATE TABLE IF NOT EXISTS webhook_replay_nonces (
    connector_id TEXT NOT NULL,
    nonce TEXT NOT NULL,
    received_at TEXT NOT NULL,
    PRIMARY KEY(connector_id, nonce)
);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS desktop_action_results (
    request_id TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    result_json TEXT,
    created_at TEXT NOT NULL,
    completed_at TEXT
);
