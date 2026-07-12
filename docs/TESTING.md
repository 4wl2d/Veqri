# Testing

## Test layers

- Unit: state machines, policy, schemas, auth, providers, tool validation/execution.
- Persistence: migrations, idempotent ingestion, optimistic transitions, approval expiry, recovery.
- Connector contract: signatures/tokens/JWT fail-closed behavior, normalization, dedupe, thread roots.
- Integration: authenticated HTTP/WebSocket, worker runtime, SQLite, Android/desktop edge mappings.
- E2E: text delegation, voice interruption, connector reply, shell approval, restart recovery.
- Android: ViewModel/repository/transport JVM tests, lint, APK and instrumentation-APK compilation; connected tests when a device exists.
- Desktop: DOM interaction tests, API validation/reconnect tests, typecheck, production build.

## Commands

```sh
go test -race ./...
go vet ./...

cd apps/desktop
npm ci
npm run typecheck
npm test -- --run
npm run build

cd ../android
./gradlew --no-daemon testDebugUnitTest lintDebug assembleDebug assembleRelease assembleDebugAndroidTest
```

Packaged Core/CLI and desktop-edge smoke after a native build:

```sh
make release-check
```

This starts the compiled Core with an isolated data directory, then checks readiness, CLI health/diagnostics, desktop protocol-v1 snapshot loading, and a committed safe settings action. The Linux/macOS/Windows CI matrix and artifact scope are documented in [RELEASE.md](RELEASE.md).

Network-backed dependency vulnerability checks:

```sh
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
cd apps/desktop && npm audit --audit-level=high
cd native && go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
```

## Required deterministic scenarios

### A — Android text

Pair → connect device WebSocket → `conversation.send_text` → durable event/turn/task → mock agent progress → final assistant message/task event.

### B — Voice

Create/answer call → partial/final simulated transcript → task → TTS chunks → interrupt → verify task is not cancelled → new listening/reconnect state.

### C — Connector

Verify Slack HMAC or use authenticated simulator → normalize/dedupe → run task → progress retains target → one final delivery uses original channel/root thread.

### D — Shell approval

Read-only command allowed → state-changing command waits → approve once → invoke once/audit; deny path creates no invocation. Shell strings, traversal, privilege, timeout, and redaction have adversarial tests.

### E — Restart

Persist queued/running task → stop/reopen runtime → recover safe task → preserve one task/delivery. A started state-changing invocation becomes uncertain failure and is not replayed.

Tests use bounded polling and deterministic providers; fixed sleeps are avoided except tiny mock latency under a context deadline.

## Instrumentation limitation

`assembleDebugAndroidTest` proves instrumentation compiles. `connectedDebugAndroidTest` is reported separately and must not be claimed when `adb devices` has no attached target.
