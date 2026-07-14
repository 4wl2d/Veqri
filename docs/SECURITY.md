# Security operations

## Defaults

- Loopback bind only.
- TLS required for non-loopback bind.
- Random `0600` admin token; independently revocable, two-phase rotatable device credentials.
- Every authenticated HTTP request carries an explicit `X-Veqri-Protocol-Version`; a missing or incompatible version fails with HTTP 426.
- Metrics, device inventory, audit, diagnostics, local-event ingestion, low-level tools, and outbound call creation require the local administrator credential.
- No plaintext secrets in SQLite provider/connector records.
- Local read-only tools may be allowed; state changes require evaluation, destructive/external/secret actions require approval, privilege escalation is denied.
- Emergency stop and per-agent/per-connector kill switches are enforced in Core.
- Rolling content and audit retention is configurable with `VEQRI_RETENTION_DAYS`; `0` retains indefinitely.

A paired Android device is an owner-class client: it may inspect the owner's conversations/tasks and resolve pending approvals, including work originating from connectors. It cannot mint `local`-trust events, enumerate devices/audit/diagnostics, or invoke low-level tools directly. Pairing therefore grants broad personal-assistant authority, not a guest scope. Voice-session control remains bound to its target device. Revoke a lost/shared device immediately; narrower per-device roles are a post-MVP policy extension.

Pairing claims are limited over a rolling 60-second window to five admitted attempts per peer IPv4 address or IPv6 `/64`, and thirty globally. Both claim-route aliases share the same in-memory limiter, and forwarded-address headers are not trusted.

## Secret references

Configuration stores locators such as `keychain:veqri/slack/bot-token`, not secret values. Development environment variables are supported for simulators but should not be used for packaged production. Tools receive only references/scoped resolved values; remote agents do not receive the whole environment.

Never log bearer tokens, signing secrets, private keys, authorization headers, full sensitive environment variables, or unredacted command output. Diagnostic exports use `0600` and default to path/content redaction.

Device rotation stores active and five-minute pending hashes, never raw credentials. The pending credential is scoped to confirmation until promotion. Prepare, confirm, and cancellation state changes are committed atomically with sanitized audit records. The old credential remains active until the client has persisted and confirms its replacement; a lost confirmation response is recovered by idempotently confirming again with the promoted credential.

## Network enablement

Do not set `VEQRI_ADDR=0.0.0.0:*` without a certificate/key, firewall restriction, and explicit paired-device review. Prefer a user-managed VPN over a public reverse proxy. Configure TURN only for the media provider that needs it. Generic webhook/Teams public endpoints should be isolated by a reverse proxy with request-size and rate limits in addition to Core validation.

## Approval semantics

Approvals expire after ten minutes by default, are bound to one task/tool/argument object, record the deciding device/admin, and become consumed or denied atomically with the task transition. Retrying the same decision returns conflict. External messages cannot approve their own privileged request.

User-initiated task cancellation, retry, reprioritization, and dismissal commit their state change and sanitized actor audit entry in the same SQLite transaction. If the audit insert fails, the task mutation is rolled back.

## Backup and deletion

Desktop backup uses SQLite `VACUUM INTO` to create a consistent local copy. Backups contain private conversations/audit and should be encrypted by the user's storage/backup system. Retention sweeps affect only the live database; they do not rewrite or delete existing backups or diagnostic exports.

For a positive `VEQRI_RETENTION_DAYS`, Core starts an asynchronous sweep at startup and repeats it every six hours. The sweep deletes turns and scrubs conversation titles, processed event content, and terminal task/tool/approval/delivery content strictly older than the rolling UTC cutoff. It deletes SQLite artifact metadata but does not delete arbitrary workspace or externally managed files referenced by artifact URIs. Active task graphs and terminal graphs with pending approvals, pending deliveries, or uncertain tool invocations are retained until safe. Audit rows use the same cutoff, with task-linked audit retained while its graph is active or unresolved. Each successful sweep and its sanitized counts commit atomically; an audit-write failure rolls the sweep back.

Automatic expiry never disables `transcript_retention`, so future turns in that conversation can still be retained. Explicit transcript deletion is different: it removes current turns and disables future retention for that conversation. Structured process logs go to stderr and are not stored in SQLite; their rotation/deletion is controlled separately by launchd, systemd, the Windows service wrapper, Docker, or another process supervisor.

Android transcript-retention changes are fail closed. Pairing commits the initial device default with the device record. Later changes commit the device setting, conversation flag, historical turns/title/event payload scrub, safe terminal graph scrub, and audit fact before Core sends the correlated success result. The graph scrub clears task prompts/results, tool input/output/errors after a graph becomes terminal, resolved approval arguments/reasons/scopes, terminal delivery targets/errors, and managed artifact metadata; active, pending, or outcome-uncertain operational records are deferred until safe. Android does not treat WebSocket queue acceptance as success and blocks retention-dependent sends after an unknown outcome or new socket until an authoritative reconnect snapshot arrives. A paired process restart sanitizes Room before loading cached state, so a failed DataStore write cannot reveal an older transcript while Core is offline; the authoritative snapshot then repopulates permitted content. Content commands may reduce retention but cannot re-enable a Core-disabled device/conversation policy; re-enabling requires the explicit acknowledged policy command. Explicit `/v1/ask` retention changes use the same canonical conversation scrub, and terminal non-retained graphs are reconciled after approval expiry, cancellation, and restart recovery.

## Review checklist

Before enabling a new adapter:

1. Verify official authentication/replay documentation.
2. Define normalized identity and idempotency.
3. Declare minimum scopes/secrets/context.
4. Classify every side effect and failure/retry behavior.
5. Add contract, injection, replay, cancellation, and redaction tests.
6. Confirm kill switch and audit coverage.
7. Update threat model and ADR if trust materially changes.

See [THREAT_MODEL.md](THREAT_MODEL.md) for adversaries and residual risks.
