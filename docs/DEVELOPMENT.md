# Development

## Toolchain

Versions and installation commands are in the root README. Dependencies are pinned in `go.mod`/`go.sum`, `apps/desktop/package-lock.json`, Android app/protocol lockfiles, and Gradle wrapper metadata.

## Build loop

```sh
make generate
make check-generated
gofmt -w $(find . -name '*.go' -not -path './apps/*')
go test ./...
go vet ./...
cd apps/desktop && npm ci && npm run typecheck && npm test -- --run && npm run build
cd ../android && ./gradlew --no-daemon :protocol:checkAndroidProtocolBindings testDebugUnitTest lintDebug assembleDebug assembleRelease assembleDebugAndroidTest
```

`make generate` refreshes both Go and committed Android Java Lite output. `make check-generated` regenerates the Go clients with pinned tools and rejects any diff, then verifies that fresh Android output matches the committed mirror. `make build`, `make test`, and `make lint` wrap the normal paths.

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

Add new field numbers; never reuse old numbers. Android commands, snapshot, and events belong in the self-contained `protocol/proto/veqri/v1/device.proto` projection; regenerate and commit `protocol/generated/android`, then update the flat JSON mapping in `DeviceJsonCodec`. Generated Java Lite messages must stay inside that codec boundary rather than becoming domain, ViewModel, or Compose state. Run compatibility tests and update desktop edge mappings when the shared semantics change. JSON clients must ignore unknown additive fields and reject incompatible major versions.

## Adding connectors/tools/agents

Follow the security review checklist in `docs/SECURITY.md`. A new tool needs schema, scopes, risk, policy tests, timeout/cancellation, output bounds/redaction, supported OS, audit, dry-run, and restart behavior. A new connector needs official verification sources, source-scoped dedupe, loop prevention, thread-root preservation, and rate/retry tests.

## Local configuration

The daemon reads environment variables; it does not implicitly load `.env`. Use an isolated `VEQRI_DATA_DIR` for tests. Never point tests at `~/.veqri`.
