# Troubleshooting

## Core will not start

- `non-loopback VEQRI_ADDR requires...`: return to `127.0.0.1:7342` or configure both TLS files.
- `address already in use`: run `lsof -nP -iTCP:7342 -sTCP:LISTEN` and choose another loopback port consistently in clients.
- SQLite error: verify data-directory ownership/free space, then run `veqri diagnostics`. Preserve the database/WAL files before repair.
- Token too short: remove only a known development token file after preserving needed state, or provide a 32+ character `VEQRI_AUTH_TOKEN`.

## CLI unauthorized

Start Core once so it creates the OS-keychain credential (or reported `~/.veqri/admin.token` fallback), set matching `VEQRI_DATA_DIR`, or export `VEQRI_AUTH_TOKEN`. Do not paste the token into URLs or logs.

## Android cannot pair

- Code expires after five minutes and is single-use.
- Emulator host URL is normally `http://10.0.2.2:7342`; `127.0.0.1` points at the emulator.
- Physical-device HTTP/LAN is rejected by design; enable TLS/LAN explicitly.
- A revoked device must pair again.

## Android call does not appear

Grant notification permission, check full-screen special access, and verify Core sees the device online. Heads-up fallback is expected when full-screen intent is unavailable. LAN alone cannot wake a stopped/sleeping app; configure optional push.

## No real audio

The default says `simulated-no-audio`. That is expected. Configure/build a reviewed WebRTC provider; do not add an unpinned `+` Maven version. The simulator validates dialog/control/TTS interruption only.

## Task stays queued

Check emergency stop, agent/connector kill switch, agent health/concurrency, dependencies, pending approval, and worker logs. `veqri task show ID` and the desktop task graph expose state/error. After restart, safe tasks show `Recovered after restart`.

## Approval cannot be reused

Expected: approvals are single-use and expiring. Create a new request if arguments change. A started command with uncertain crash outcome requires manual inspection, not retry.

## Desktop live mode disconnects

Use `http://127.0.0.1:7342`, matching token, protocol version 1, and a localhost origin. Browser live mode embeds a development token; packaged use should inject through the Wails bridge. Mock mode remains available for UI work.

## Connector failures

- Slack: preserve raw body; verify timestamp/signature before JSON; filter bot loops.
- Mattermost: outgoing webhook simulator expects JSON; production uses bot WebSocket/REST.
- Teams: no live verifier means fail-closed by design; public HTTPS/JWT configuration is required.
- Generic webhook: timestamp, nonce, and exact raw body all participate in HMAC; nonce replay returns conflict.

## Diagnostic export

Use the desktop diagnostics page/action or `veqri diagnostics`. Redacted export is the default. Review any unredacted bundle before sharing; it may contain paths, IDs, and task summaries even though secrets remain excluded.
