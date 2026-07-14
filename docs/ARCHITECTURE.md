# Architecture

## System shape

Veqri is a local modular monolith with two native clients. Core owns authoritative state and side effects; clients render state and submit authenticated intent.

```text
Android ── authenticated HTTP + command/event WebSocket ─┐
Desktop ─ authenticated HTTP + event WebSocket ─────────┤
CLI ───── authenticated loopback HTTP ──────────────────┤
Connectors ─ verified ingress / normalized envelopes ───┤
                                                        ▼
   API adapters → durable event/conversation write → policy
                       ↓                         ↓
                 SQLite WAL ← task graph workers → agent registry
                       ↑                         ↓
                 audit/delivery ← approved tool runtime
                       ↓
                Android / desktop / original connector thread
```

The daemon is one Go binary. Domain packages do not import Android, React, Slack, Mattermost, Teams, or a model provider. Provider-specific code normalizes data before invoking the domain.

## Durable processing

1. Verify authentication/signature and replay window.
2. Normalize a trigger into `events.Envelope`.
3. Insert the event with a source-scoped unique idempotency key.
4. Persist its conversation turn and complete task graph.
5. Workers claim runnable nodes transactionally.
6. An agent/tool streams progress while durable task state remains authoritative.
7. The final node persists structured output and a written/spoken summary.
8. Delivery records preserve the target and their own idempotency key.

In-memory channels only reduce latency. If every process-local signal is lost, workers poll persisted `QUEUED` tasks and clients refresh snapshots.

Restart recovery requeues agent work and unstarted tool work. A state-changing invocation already recorded as `STARTED` is marked uncertain instead of replayed; arbitrary external effects cannot be made exactly-once by a local database alone.

## Core modules

- `core/events`: normalized durable trigger envelope.
- `core/conversation`: conversations, text turns, voice sessions, and explicit dialog transitions.
- `core/tasks`: versioned task nodes, dependency graphs, terminal/active states.
- `core/agents`: capability registry, concurrency limits, workers, cancellation, progress, synthesis.
- `core/tools`: typed definitions and invocation records.
- `core/policy`: capability/risk decisions, emergency stop, connector/agent kill switches.
- `core/approvals`: expiring single-use decisions.
- `core/delivery`: idempotent target/attempt records.
- `core/persistence`: migrations and transactional repositories.
- `core/voice`: VAD/STT/TTS/media/wake-word/recording boundaries and deterministic providers.
- `core/observability`: redacted audit contract.

## Clients

Android uses Compose, immutable render state, Room as cache, DataStore for preferences, and Keystore for its device token. A foreground microphone service starts only after visible user interaction. The application-owned call UI is independent of a future Telecom/Core-Telecom adapter.

Desktop is a React/TypeScript client inside a checked-in Wails native shell/runtime bridge. Its live API rejects non-loopback origins and never contains orchestration logic. The native companion, web frontend, mock mode, and live Core contract are operational; optional tray badge/notification integrations remain shell extensions.

## Media

Call control and dialog state do not depend on acoustic transport. The default `simulated-no-audio` transport validates ringing, answer, reconnect, transcript, task, TTS chunks, and barge-in deterministically. A real WebRTC adapter must implement the same `MediaTransport` boundary and be supply-chain reviewed; see `docs/VOICE.md`.

## Data ownership

SQLite on the PC is authoritative. Android Room is only an offline cache. Long-term memory is not enabled. With a positive `VEQRI_RETENTION_DAYS`, Core asynchronously applies a rolling UTC cutoff at startup and every six hours: expired turns and processed event content are removed or scrubbed, safe terminal task/tool/approval/delivery content is scrubbed, and audit rows use the same cutoff. Active or operationally unresolved graphs are excluded. `0` disables automatic content expiry. A separate fixed maintenance pass still removes pairing sessions expired for more than 24 hours and completed desktop action results older than seven days; its two-table whitelist cannot alter unresolved work.

Desktop backup is a persistence operation rather than a raw file copy: SQLite writes a same-directory hidden temporary snapshot, Core opens that snapshot read-only for `quick_check`, syncs it, and atomically publishes a unique final file. Backup and diagnostic artifacts stay under private Core-owned directories and are never included in live-database retention sweeps.

## Decisions

- [ADR 0001](adr/0001-local-modular-monolith.md): modular monolith and durable queue.
- [ADR 0002](adr/0002-secure-local-connectivity.md): loopback, TLS, pairing, credentials.
- [ADR 0003](adr/0003-versioned-protocol-and-json-edge.md): canonical Protobuf and inspectable JSON edge.
- [ADR 0004](adr/0004-voice-media-boundary.md): transport-independent voice and simulator.

## Repository tree

The proposed and implemented tree is recorded in [architecture/REPOSITORY_TREE.md](architecture/REPOSITORY_TREE.md).
