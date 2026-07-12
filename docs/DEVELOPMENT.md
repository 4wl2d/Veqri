# Development

## Toolchain

Versions and installation commands are in the root README. Dependencies are pinned in `go.mod`/`go.sum`, `apps/desktop/package-lock.json`, Android version catalog/locks, and Gradle wrapper metadata.

## Build loop

```sh
./scripts/generate-protocol.sh
gofmt -w $(find . -name '*.go' -not -path './apps/*')
go test ./...
go vet ./...
cd apps/desktop && npm ci && npm run typecheck && npm test -- --run && npm run build
cd ../android && ./gradlew --no-daemon testDebugUnitTest lintDebug assembleDebug
```

`make build`, `make test`, and `make lint` wrap the normal paths.

`make release-check` additionally builds the native desktop companion and runs the built Core/CLI against an isolated real listener. The host must provide the Wails platform dependencies described in the root README. Cross-platform artifacts and host-native gates are defined in [RELEASE.md](RELEASE.md).

## Architectural rules

- Domain code remains independent of transports/UI/database details.
- Durable work is derived from SQLite, never an in-memory queue.
- State transitions are explicit and transactional.
- External operations carry idempotency/correlation and define crash semantics.
- Provider/tool/connector boundaries use interfaces; internal helper functions do not need ceremonial interfaces.
- Structured process invocation is mandatory.
- A simulator is labelled simulator in API/UI/docs and cannot satisfy a real-provider claim.

## Adding a migration

Add a monotonically numbered `core/persistence/migrations/NNNN_name.sql`. Migrations must be forward-only, transactional, and preserve persisted task/event compatibility. Add a fresh-database and upgrade-path test. Never edit a released migration; this repository is pre-1.0 and `0001` remains the initial baseline until its first release.

## Adding protocol fields

Add new field numbers; never reuse old numbers. Regenerate, run compatibility tests, and update both Android/desktop edge mappings. JSON clients must ignore unknown additive fields and reject incompatible major versions.

## Adding connectors/tools/agents

Follow the security review checklist in `docs/SECURITY.md`. A new tool needs schema, scopes, risk, policy tests, timeout/cancellation, output bounds/redaction, supported OS, audit, dry-run, and restart behavior. A new connector needs official verification sources, source-scoped dedupe, loop prevention, thread-root preservation, and rate/retry tests.

## Local configuration

The daemon reads environment variables; it does not implicitly load `.env`. Use an isolated `VEQRI_DATA_DIR` for tests. Never point tests at `~/.veqri`.
