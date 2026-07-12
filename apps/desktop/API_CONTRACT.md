# Veqri desktop API contract (protocol v1)

The canonical machine-readable TypeScript definitions are in `src/api/types.ts`. JSON field names are `snake_case`; unknown top-level protocol versions are rejected. Timestamps are RFC 3339 UTC strings. IDs are opaque durable strings.

## Authentication and origin

- Core origin defaults to `http://127.0.0.1:7342` and must resolve syntactically to `localhost`, `127.0.0.1`, or `[::1]`. The browser client rejects credentials, paths, queries, and fragments in the configured origin.
- HTTP sends `Authorization: Bearer <token>`, `X-Veqri-Client: desktop`, and `X-Veqri-Protocol-Version: 1`.
- WebSocket connects to `/api/v1/events` with exactly these subprotocols: `["veqri.v1", "veqri.auth.<base64url-encoded-UTF8-token>"]`. The token is never placed in the URL.

## Snapshot

`GET /api/v1/desktop/snapshot`

Response shape:

```ts
interface DesktopSnapshot {
  protocol_version: 1;
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
```

Every nested field is required except values explicitly typed as `null` or optional in [`src/api/types.ts`](src/api/types.ts). [`src/data/mockSnapshot.ts`](src/data/mockSnapshot.ts) is a complete deterministic protocol-v1 example payload.

## Actions

`POST /api/v1/desktop/actions`

Exact request envelope:

```json
{
  "request_id": "uuid-created-by-desktop",
  "action": {
    "type": "task.cancel",
    "task_id": "task-release"
  }
}
```

`action` is exactly one of:

```ts
type DesktopAction =
  | { type: "approval.resolve"; approval_id: string; decision: "approved" | "denied"; note?: string }
  | { type: "task.cancel"; task_id: string }
  | { type: "task.retry"; task_id: string }
  | { type: "device.revoke"; device_id: string }
  | { type: "voice.end"; session_id: string }
  | { type: "connector.retry"; connector_id: string }
  | { type: "connector.kill_switch.set"; connector_id: string; enabled: boolean }
  | { type: "agent.kill_switch.set"; agent_id: string; enabled: boolean }
  | { type: "core.emergency_stop.set"; enabled: boolean }
  | { type: "settings.update"; patch: Partial<DesktopSettings> }
  | { type: "backup.create" }
  | { type: "diagnostics.export"; redact: boolean };
```

`DesktopSettings` reserves fields for native/Core behavior. The client only emits `settings.update` for `theme`. `transcript_retention_days` and `audit_retention_days` are authoritative read-only runtime values sourced from Core's `VEQRI_RETENTION_DAYS`; Core applies the same rolling UTC cutoff to both. Login/tray lifecycle, OS notifications, quiet hours, and listener configuration remain read-only because saving those values would not enforce the claimed behavior.

Exact success response:

```json
{
  "request_id": "same-request-id",
  "accepted": true,
  "occurred_at": "2026-07-12T09:42:18.000Z",
  "revision": 43,
  "message": "Human-readable, sanitized result.",
  "artifact_path": null
}
```

`artifact_path` is a local absolute/display path or `null`. A non-2xx HTTP status is a failed request even if the body contains JSON. `401` and `403` are treated as authentication failures. Core should deduplicate `request_id` so retrying a timed-out POST cannot repeat a side effect.

## Event stream

`WS /api/v1/events`

Exact event envelope:

```json
{
  "id": "evt-1043",
  "type": "task.changed",
  "occurred_at": "2026-07-12T09:42:18.000Z",
  "correlation_id": "corr-release-01",
  "sequence": 1043,
  "data": {
    "revision": 43,
    "entity_id": "task-release"
  }
}
```

- `type`: `snapshot.changed | task.changed | approval.changed | core.changed | heartbeat`
- `correlation_id`: string or `null`
- `data.entity_id`: optional; `data.revision` is always required
- On a non-heartbeat event whose revision is newer than the displayed snapshot, the desktop debounces and reloads the authenticated snapshot.
- The desktop reconnects with deterministic exponential delays of 500 ms, 1 s, 2 s, 4 s, 8 s, then 10 s (capped), and reports failure after eight retries. Cached data remains visible but is marked stale.
