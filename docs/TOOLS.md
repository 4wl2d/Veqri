# PC tools

## Authorization path

Agent intent → typed schema validation → risk classification → capability policy → optional expiring approval → durable invocation ledger → execution → bounded/redacted output → audit.

The policy decision is server-side. UI approval cards show the exact binary/arguments, scope, workspace, risk, and expiry. Approval tokens are consumed once.

## Shell

Executes one binary with `exec.CommandContext` and an argument array. It confines CWD, filters environment variables, rejects NULs, scripts/interpreters, privilege escalation (including Windows `runas.exe`/`sudo.exe`), and secret-like variables, streams bounded stdout/stderr, redacts configured values, supports timeout/cancellation, and records exit state. Every executable is resolved through PATH and symlinks to a regular native binary before classification; the canonical path and SHA-256 identity are included in the exact approval payload and rechecked immediately before execution. Executables in mutable locations are copied and hashed from the same open descriptor into a private, digest-keyed, non-writable execution directory, then launched from that verified copy while retaining the approved canonical path as `argv[0]` and audit identity. Root-protected platform binaries under the trusted system directories run directly after canonical/digest revalidation because macOS AMFI rejects copied Apple platform binaries; those directories are not writable by Veqri's non-privileged identity. This prevents an approved alias, replaced PATH/workspace binary, or validation-to-launch pathname swap from changing identity, and reclassification prevents an older approval from bypassing a newer privilege denylist. Legacy non-dry-run tasks without the exact canonical path and digest fail closed and require a new approval. Read-only classification is conservative; unknown binaries are state-changing.

## Filesystem

Uses workspace-root confinement, traversal/symlink defenses, bounded read/list/search, UTF-8/base64 output, atomic write, expected-hash preconditions, exact patches, dry-run plans, and hashes. Move/delete are high-risk operations and never hidden behind UI gestures.

## Git

Exposes a fixed operation enum rather than arbitrary arguments. Read operations include status/diff/log. Branch/worktree/commit/push are stateful; push is external communication. Repository metadata is validated, environment is hardened, outputs bounded, explicit refspecs required, and force push is always denied.

## HTTP

Requires method/domain/port allowlists; bounds body/response/headers; validates every redirect; prevents HTTPS downgrade; resolves and rejects private/special IP ranges; dials the validated IP while preserving Host/TLS SNI; injects credentials only through configured secret references; and redacts audit output.

## Native applications

Adapter priority is official API, official CLI, plugin, OS automation API, accessibility, then explicitly enabled visual coordinates. Platform adapters feature-detect official commands and return explicit unsupported errors. Coordinate automation is absent by default.

## Notifications

Notification intent is typed as Android notification/call, desktop notification, queued speech, or originating-thread reply. Delivery gets its own durable ID and idempotency key. Speaking is serialized per voice session; low-priority results wait rather than overlap.
