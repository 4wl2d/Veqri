export const DESKTOP_PROTOCOL_VERSION = 1 as const;

export type HealthTone = "healthy" | "degraded" | "offline" | "disabled" | "unknown";
export type RiskLevel =
  | "READ_ONLY"
  | "LOW"
  | "STATE_CHANGING"
  | "DESTRUCTIVE"
  | "PRIVILEGED"
  | "EXTERNAL_COMMUNICATION"
  | "SECRET_ACCESS";

export type TaskStatus =
  | "CREATED"
  | "QUEUED"
  | "ASSIGNED"
  | "RUNNING"
  | "WAITING_FOR_CHILDREN"
  | "WAITING_FOR_APPROVAL"
  | "BLOCKED"
  | "COMPLETED"
  | "PARTIALLY_COMPLETED"
  | "FAILED"
  | "CANCEL_REQUESTED"
  | "CANCELLED"
  | "TIMED_OUT";

export type VoiceState =
  | "IDLE"
  | "RINGING"
  | "CONNECTING"
  | "LISTENING"
  | "TRANSCRIBING"
  | "THINKING"
  | "DELEGATING"
  | "WAITING_FOR_RESULT"
  | "SPEAKING"
  | "INTERRUPTED"
  | "WAITING_FOR_APPROVAL"
  | "RECONNECTING"
  | "FAILED"
  | "ENDED";

export interface CoreStatus {
  status: "healthy" | "degraded" | "offline";
  version: string;
  protocol_version: number;
  started_at: string;
  uptime_seconds: number;
  bind_address: string;
  service_mode: "foreground" | "background" | "tray";
  database: {
    status: HealthTone;
    path: string;
    size_bytes: number;
    wal_enabled: boolean;
    migration_version: number;
  };
  queue: {
    queued: number;
    running: number;
    blocked: number;
  };
  emergency_stop: boolean;
  cpu_percent: number | null;
  memory_bytes: number | null;
}

export interface Device {
  id: string;
  name: string;
  platform: "android";
  model: string;
  status: "online" | "offline" | "revoked";
  paired_at: string;
  last_seen_at: string;
  app_version: string;
	key_version: number;
  capabilities: string[];
  network: "localhost" | "lan" | "vpn" | "relay";
}

export interface VoiceSession {
  id: string;
  conversation_id: string;
  device_id: string;
  device_name: string;
  state: VoiceState;
  started_at: string;
  duration_seconds: number;
  transport: "webrtc" | "simulated";
  codec: string;
  round_trip_ms: number;
  packet_loss_percent: number;
  active_task_count: number;
  partial_transcript: string;
}

export interface Conversation {
  id: string;
  title: string;
  source: "android" | "desktop" | "slack" | "mattermost" | "teams" | "webhook";
  participant: string;
  updated_at: string;
  turn_count: number;
  active_task_count: number;
  retention: "retained" | "session_only" | "disabled";
  last_message: string;
  correlation_id: string;
}

export interface TaskArtifact {
  id: string;
  name: string;
  media_type: string;
  size_bytes: number;
}

export interface Task {
  id: string;
  parent_task_id: string | null;
  root_task_id: string;
  goal: string;
  status: TaskStatus;
  progress_percent: number;
  assigned_agent_id: string | null;
  assigned_agent_name: string | null;
  current_tool: string | null;
  allowed_tools: string[];
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  retry_count: number;
  max_retries: number;
  priority: number;
  dismissed: boolean;
  error: string | null;
  summary: string;
  dependencies: string[];
  artifacts: TaskArtifact[];
  correlation_id: string;
}

export interface Agent {
  id: string;
  name: string;
  description: string;
  capabilities: string[];
  tool_scopes: string[];
  trust_level: "untrusted" | "known" | "trusted" | "local";
  execution_mode: "built_in" | "local_process" | "local_model" | "http" | "grpc" | "stdio" | "mcp";
  health: HealthTone;
  active_tasks: number;
  concurrency_limit: number;
  supports_streaming: boolean;
  supports_cancellation: boolean;
  kill_switch: boolean;
  latency_ms: number;
}

export interface ToolPermission {
  id: string;
  name: string;
  description: string;
  risk: RiskLevel;
  status: "allowed" | "approval_required" | "denied";
  scopes: string[];
  workspace_boundary: string | null;
  running_invocations: number;
  supported_os: string[];
}

export interface Policy {
  id: string;
  name: string;
  description: string;
  priority: number;
  decision: "ALLOW" | "ALLOW_WITH_REDACTION" | "REQUIRE_APPROVAL" | "DENY";
  match_summary: string;
  enabled: boolean;
  updated_at: string;
}

export interface Approval {
  id: string;
  task_id: string;
  task_goal: string;
  requested_by_agent: string;
  tool_name: string;
  permission: string;
  risk: RiskLevel;
  arguments: Record<string, unknown>;
  command_preview: string | null;
  reason: string;
  requested_at: string;
  expires_at: string;
  status: "pending" | "approved" | "denied" | "expired" | "consumed";
}

export interface Connector {
  id: string;
  name: string;
  kind: "slack" | "mattermost" | "teams" | "webhook" | "local_events";
  mode: "live" | "simulated";
  health: HealthTone;
  enabled: boolean;
  kill_switch: boolean;
  last_event_at: string | null;
  events_today: number;
  target_summary: string;
  error: string | null;
}

export interface Provider {
  id: string;
  name: string;
  category: "ai" | "stt" | "tts" | "media" | "push";
  adapter: string;
  mode: "local" | "remote" | "simulated";
  health: HealthTone;
  enabled: boolean;
  secret_reference: string | null;
  latency_ms: number | null;
  detail: string;
}

export interface AuditEntry {
  id: string;
  occurred_at: string;
  category: "security" | "task" | "tool" | "approval" | "connector" | "device" | "system";
  action: string;
  actor: string;
  target: string;
  decision: "allowed" | "denied" | "recorded" | "failed" | "redacted";
  summary: string;
  correlation_id: string;
  redacted: boolean;
}

export interface Diagnostics {
  generated_at: string;
  checks: Array<{
    id: string;
    name: string;
    status: HealthTone;
    detail: string;
    checked_at: string;
  }>;
  event_stream: {
    connected_clients: number;
    last_event_id: string;
    backlog: number;
  };
  webrtc: {
    active_peers: number;
    stun: string;
    turn: string;
  };
  storage: {
    free_bytes: number;
    backup_count: number;
    last_backup_at: string | null;
    last_backup_path: string | null;
  };
  recent_logs: Array<{
    id: string;
    occurred_at: string;
    level: "DEBUG" | "INFO" | "WARN" | "ERROR";
    component: string;
    message: string;
    correlation_id: string | null;
  }>;
}

export interface DesktopSettings {
  theme: "dark" | "light" | "system";
  start_at_login: boolean;
  close_to_tray: boolean;
  desktop_notifications: boolean;
  transcript_retention_days: number;
  audit_retention_days: number;
  announce_background_results: boolean;
  quiet_hours_enabled: boolean;
  quiet_hours_start: string;
  quiet_hours_end: string;
  lan_access_enabled: boolean;
  redact_diagnostics: boolean;
}

export interface DesktopSnapshot {
  protocol_version: typeof DESKTOP_PROTOCOL_VERSION;
  revision: number;
  generated_at: string;
  core: CoreStatus;
  devices: Device[];
  voice_sessions: VoiceSession[];
  conversations: Conversation[];
  tasks: Task[];
  agents: Agent[];
  tools: ToolPermission[];
  policies: Policy[];
  approvals: Approval[];
  connectors: Connector[];
  providers: Provider[];
  audit_entries: AuditEntry[];
  diagnostics: Diagnostics;
  settings: DesktopSettings;
}

export type DesktopAction =
  | { type: "approval.resolve"; approval_id: string; decision: "approved" | "denied"; note?: string }
  | { type: "task.cancel"; task_id: string }
  | { type: "task.retry"; task_id: string }
  | { type: "task.reprioritize"; task_id: string; priority: number }
  | { type: "task.dismiss"; task_id: string }
  | { type: "device.revoke"; device_id: string }
  | { type: "voice.end"; session_id: string }
  | { type: "connector.retry"; connector_id: string }
  | { type: "connector.kill_switch.set"; connector_id: string; enabled: boolean }
  | { type: "agent.kill_switch.set"; agent_id: string; enabled: boolean }
  | { type: "core.emergency_stop.set"; enabled: boolean }
  | { type: "settings.update"; patch: Partial<DesktopSettings> }
  | { type: "backup.create" }
  | { type: "diagnostics.export"; redact: boolean };

export interface DesktopActionRequest {
  request_id: string;
  action: DesktopAction;
}

export interface DesktopActionResponse {
  request_id: string;
  accepted: boolean;
  occurred_at: string;
  revision: number;
  message: string;
  artifact_path: string | null;
}

export interface DesktopEvent {
  id: string;
  type: "snapshot.changed" | "task.changed" | "approval.changed" | "core.changed" | "heartbeat";
  occurred_at: string;
  correlation_id: string | null;
  sequence: number;
  data: {
    revision: number;
    entity_id?: string;
  };
}

export type LoadState = "loading" | "ready" | "empty" | "disconnected" | "retrying" | "failed";
export type StreamState = "connecting" | "connected" | "retrying" | "disconnected" | "failed";

export interface RuntimeConfig {
  mode: "mock" | "live";
  api_base_url: string;
  auth_token: string;
}

export interface VeqriShellBridge {
  getRuntimeConfig(): Promise<RuntimeConfig>;
  setTrayBadge?(count: number): Promise<void>;
  showDesktopNotification?(title: string, body: string): Promise<void>;
  revealFile?(absolutePath: string): Promise<void>;
}

declare global {
  interface Window {
    veqriShell?: VeqriShellBridge;
    go?: {
      main?: {
        Bridge?: {
          GetRuntimeConfig(): Promise<RuntimeConfig>;
        };
      };
    };
  }
}
