# Threat model

## Assets and trust boundaries

Protected assets include device/admin credentials, private files, shell authority, connector identities, conversation content, task/artifact history, and the ability to communicate externally. Trust boundaries exist at every connector, Android/LAN connection, remote agent/provider, file/web content, tool output, and native application API.

Veqri assumes the local user account and operating system may grant sensitive authority. It does not assume that a Slack workspace, message author, web page, model, plugin, remote agent, or command output is trustworthy.

## Threats and controls

### Malicious connector messages and prompt injection

Messages are normalized as `untrusted` even from a configured platform. Identity, channel, mention, and connector policy are separate inputs. Content cannot grant tool scopes. Agents never call tools directly; the policy engine evaluates the source, actor, agent, tool, arguments, workspace, risk, and approval. Remote context should be minimized.

### Command injection

The shell tool invokes one binary with an argument array. Shell interpreters and privilege escalators are denied. CWD is confined to canonical workspaces; environment names are allowlisted; secret-like variables require references. State-changing/destructive commands require approval. The approval binds a symlink-resolved canonical path and SHA-256 digest. For executables from mutable locations, Veqri copies and hashes bytes from the same open descriptor into a private digest-keyed directory and launches only that sealed copy, closing the pathname-swap window between validation and process start. Root-protected platform binaries run directly after canonical/digest revalidation because macOS AMFI rejects copied Apple binaries; the service identity cannot replace files in those trusted directories. Legacy executable approvals without the canonical path and exact digest are not replayed. Tests cover interpreter strings, NULs, traversal, executable replacement during launch, output bounds, cancellation, and redaction.

### Path traversal and symlink escape

Filesystem/Git/shell paths are canonicalized against explicit workspace roots. The filesystem tool uses root-confined resolution and rechecks parents for writes. Moves/deletes are high-risk and approval-ready.

### SSRF and DNS rebinding

The HTTP tool requires scheme/domain/method/port allowlists, resolves each hop, rejects loopback/private/link-local/multicast/unspecified addresses, dials the validated concrete IP while preserving Host/SNI, revalidates redirects, blocks HTTPS downgrade, and bounds request/response sizes.

### Agent impersonation or compromised remote agent

Agent definitions are registered by the owner, have declared execution mode/trust/tool scopes/concurrency, and cannot expand their own scopes. Remote adapters need authenticated endpoints and minimum context. Output remains untrusted and is sanitized before model reuse.

### Device theft and unauthorized LAN access

Core binds to loopback by default. LAN requires TLS. Pairing codes are HMAC-hashed, five-minute, and single-use; claims are capped by shared rolling per-peer and global limits. Device credentials are random, stored in Android Keystore, hash-only in Core, independently revocable, and two-phase rotatable without invalidating the active credential before the replacement is persisted. Pending rotation credentials expire after five minutes and can only confirm their own promotion. A paired credential remains owner-class for tasks, approvals, conversations, and its voice sessions, but operational inventory, audit, diagnostics, local-trust ingress, and low-level tools require the administrator credential. Active sockets close with code 4003 on revocation and 4004 after confirmed rotation.

### Event replay and duplicate effects

Connector-specific IDs plus source/instance scope are unique in SQLite. Slack timestamps have a five-minute window; generic webhooks require a signed timestamp and unique nonce. Approval decisions and delivery keys are single-use. A started state-changing invocation is never automatically replayed after a crash.

### Malicious tool output

Outputs are size-bounded and configured secrets are redacted. Output is data, not policy. Audit summaries avoid raw output. Diagnostic exports omit tokens, private keys, authorization headers, and sensitive environment values.

### Excessive private logging

Logs carry IDs and sanitized error classes, not credentials or message bodies. Transcript retention is user-controlled. Audit retains enough structured metadata to explain a side effect without duplicating raw private content. A positive `VEQRI_RETENTION_DAYS` applies the same rolling cutoff to SQLite content and audit rows while deferring active/unresolved task graphs; `0` retains indefinitely. Process logs are emitted to stderr, so the service supervisor—not SQLite retention—owns their rotation.

### Supply chain

Go, npm, and Gradle dependencies are pinned with `go.sum`, `package-lock.json`, Gradle dependency locks, and wrapper checksums. Generated protocol code is reproducible. Google does not publish a supported stable WebRTC Android Maven artifact; real media therefore requires a pinned branch-head AAR/checksum/license review or an explicitly accepted third-party SDK ADR.

## Residual risks

- An approved arbitrary program can perform effects beyond Veqri's visibility.
- Exactly-once external side effects cannot be guaranteed if a process dies after the OS performs an action but before persistence; Veqri prefers an uncertain failure over replay.
- A compromised local account can read the admin token and database.
- LAN-only networking cannot reliably wake a sleeping/stopped Android app; optional push adds a new trust boundary.
- The default media simulator does not validate acoustic echo cancellation, codec interoperability, or TURN traversal.
- Android speech output accepts only a bounded full-answer event and selects a voice that declares offline operation; installed TTS engines and voice packages remain part of the device trust boundary.

## Emergency response

Use the desktop/HTTP emergency stop to deny new tool execution, revoke affected devices, activate per-agent/connector kill switches, stop Core, rotate connector/device credentials, inspect the audit log, and restore from a verified SQLite backup.
