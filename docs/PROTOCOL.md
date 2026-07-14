# Protocol

## Canonical schema

`protocol/proto/veqri/v1` is canonical for shared semantics. Go and Kotlin-compatible options are generated from the same sources. The first edge transport is JSON over HTTP/WebSocket; a gRPC service definition is included for strongly typed local/remote adapters.

Version negotiation uses major `1`, minor `0`, and a capability list. Every authenticated HTTP request sends `X-Veqri-Protocol-Version: 1`; a missing or malformed header, or an incompatible major, receives HTTP 426. Every WebSocket client must offer `veqri.v1`; the Android device WebSocket also sends the explicit protocol header during its authenticated upgrade.

## Identity and time

- UUID-like durable IDs are opaque.
- `correlation_id` follows one user intent through events, turns, task nodes, invocations, audit, delivery, and voice.
- `causation_id` identifies the immediately preceding durable event.
- `idempotency_key` is stable at every external-operation boundary.
- JSON timestamps are RFC 3339 UTC; Protobuf uses `google.protobuf.Timestamp`.

## HTTP surfaces

- `/healthz`, `/readyz`: unauthenticated minimal health. `/metrics` is administrator-only.
- `/v1/pairings`: admin pairing creation. `/v1/pairing/claim` and `/v1/pairings/claim` are public one-time claim aliases with shared rolling limits of five admitted attempts per peer IPv4 address or IPv6 `/64`, and thirty globally per minute. Android includes `retain_transcript`; consuming the code, creating the device, and storing that device privacy default are one transaction, so the HTTP 201 response acknowledges all three.
- `/v1/devices/self/credential-rotation/*`: device-authenticated two-phase credential rotation.
- `/v1/ask`: administrator or paired-owner requests. `/v1/events` is administrator-only because it assigns local trust.
- `/v1/tasks`, `/v1/tasks/{id}/priority`, `/v1/tasks/{id}/dismiss`, `/v1/approvals`: administrator or paired-owner durable task and approval control. Priority is bounded to `-100..100`; dismissal hides only terminal tasks from default lists and does not delete their correlated audit history. Cancel, retry, priority, and dismissal changes commit with their actor audit entry.
- `/v1/tools/shell`, `/v1/devices`, `/v1/audit`, `/v1/diagnostics`: administrator-only low-level and operational surfaces.
- `/v1/voice/calls`: administrator-only call creation. Existing session control is available to the administrator and its target device.
- `/v1/connectors/*`, `/v1/webhooks/*`: verified ingress and simulators.
- `/v1/device/events`: Android command/event WebSocket.
- `/api/v1/desktop/*`: desktop snapshot/actions/event contract.

Bearer credentials are never accepted in URL queries. Browser WebSockets send a base64url token through `veqri.auth.<token>` as a secondary subprotocol. Android sends its bearer header and claimed device ID; Core verifies they match.

## Device credential rotation

Rotation is two-phase so a lost response or failed Keystore write cannot strand a paired device:

1. Call `POST /v1/devices/self/credential-rotation/prepare` with the active device bearer and protocol header. The request has no body. HTTP 201 returns `device_id`, `credential`, `key_version`, `prepared_at`, `expires_at`, `correlation_id`, and `protocol_version`. The raw replacement appears only in `credential` in this one response; Core stores only its hash. The active credential remains valid. A second prepare while one is pending returns HTTP 409.
2. Persist the replacement in a separate Android Keystore slot while retaining the active credential and the returned `key_version`/expiry as durable recovery state. Arm the transport to use the replacement on the next reconnect, but keep the existing socket and old credential until confirmation.
3. Call `POST /v1/devices/self/credential-rotation/confirm` using the replacement bearer and body `{"key_version": 2}` (using the value returned by prepare). The pending bearer is accepted only by this endpoint. HTTP 200 returns `confirmed: true`; Core atomically promotes the pending hash, increments `key_version`, removes the old hash, and records the audit entry. A retry with the promoted bearer and same version returns `already_confirmed: true`.
4. After first confirmation, Core closes old device streams with application close code `4004`; reconnect using the promoted credential. This code is distinct from revocation code `4003`.

If the prepare response is lost before the replacement is stored, call `POST /v1/devices/self/credential-rotation/cancel` with the still-active bearer, then prepare again. Cancel is idempotent and returns `cancelled: false` when nothing is pending. Pending credentials expire after five minutes; expired confirmation returns HTTP 410 and never invalidates the active credential. Clients must not automatically retry prepare or confirm with a different key version.

Android must treat this as a durable state machine, not a single-token overwrite. If the app restarts with a stored replacement and pending version, it tries confirm with that replacement before opening the general event stream. HTTP 200 or WebSocket close code 4004 proves promotion and allows deletion of the old slot. HTTP 410 means restore/use the old slot, cancel the expired pending state with the old bearer, and prepare again. If neither credential authenticates, stop automatic retries and require pairing. This ordering also covers the race where Core closes the old stream immediately after commit, before the confirm response reaches Android.

## Android command stream

Commands contain `command_id`, `protocol_version`, and a discriminated `type`. Command IDs are idempotency keys; side-effecting commands are not retried automatically by the client. Events are normalized into:

- `conversation.message_added`
- `task.changed`
- `approval.changed`
- `voice.incoming` / `voice.changed`
- `transcript.partial` / `transcript.final`
- `tts.speak`
- `tts.changed`
- `command.result`

`conversation.set_transcript_retention` is commit-acknowledged. Core replies once with a `command.result` whose `correlation_id` and payload `command_id` equal the submitted command ID and whose status is `COMMITTED` or `REJECTED`. `COMMITTED` is sent only after the device default, optional current-conversation policy, canonical transcript scrub, and audit fact commit together. Android serializes user actions around this command and changes DataStore/UI state only after that result. A timeout or disconnect is an unknown outcome: Android does not retry or send work carrying the old preference; it reconnects and waits for `sync.snapshot`.

Text/call content commands cannot elevate retention. `retain_transcript=false` may opt down for new work, but `true` is intersected with Core's durable device and existing-conversation policies. Therefore a stale client after a lost acknowledgement or an administrator-disabled conversation cannot silently re-enable storage; only the explicit acknowledged policy command can opt back in.

`tts.speak` is emitted only when Core actually starts the matching active voice session and carries one logical concise answer (`session_id`, `conversation_id`, `status=BUFFERING`, and `text`) for Android-side playback. Core bounds the UTF-8 text to 12 KiB. `tts.changed` carries server-side streaming status only; Android must not speak individual synthetic/provider chunks. This separation makes replay, queued results, and barge-in behavior unambiguous.

On connect or reconnect, Core sends one bounded `sync.snapshot` before live deltas. Android atomically replaces its visible/cache window from that snapshot, pruning stale records; `transcript_retention` is the authoritative device value and resolves an unconfirmed privacy-command outcome. The payload is capped below the client's 128 KiB frame limit and prefers active tasks/pending approvals before recent history. Subsequent stream events are ordered deltas until the next reconnect.

## Generation

```sh
./scripts/generate-protocol.sh
go test ./protocol/generated/go/...
```

Compatibility checks compare checked-in schemas/generated code. Fields must be added, not renumbered or repurposed; removed fields should be reserved in a subsequent migration.
