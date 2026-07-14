# Veqri

Veqri is a local-first personal AI orchestrator: a Go daemon and CLI, native Android client, desktop companion, durable agent/task runtime, approved PC tools, messaging connector boundaries, and a WebRTC-compatible voice control plane. It runs without a Veqri cloud account. External models, STT/TTS, push, TURN, and messaging credentials are optional adapters.

This repository is an implementation, not only a design. The deterministic default exercises text delegation, parallel task graphs, persisted progress/results, Android pairing and streaming, single-use shell approvals, connector thread routing, simulated call/STT/TTS/barge-in, restart recovery, and desktop administration entirely on one machine.

## Operational status

| Capability | Status in this checkout |
|---|---|
| Core HTTP/WebSocket APIs, SQLite WAL state, migrations, event dedupe, task workers, cancellation, recovery, audit | Operational |
| Versioned Protobuf/gRPC contracts | Generated from one canonical schema; the running MVP edge is HTTP/WebSocket and a live gRPC listener remains an adapter gap |
| Rolling SQLite content/audit retention | Operational; `VEQRI_RETENTION_DAYS=0` retains indefinitely, positive values sweep asynchronously at startup and every six hours |
| Private storage maintenance and SQLite backup | Operational; private directories/files are permission-restricted, transient pairing/desktop results are cleaned on a fixed schedule, and backups are integrity-checked before atomic publication |
| Deterministic general/planner/coding/research/automation agents and result synthesizer | Operational, explicitly simulated domain work |
| Android pairing, authenticated command/event stream, conversation/tasks/approvals/call UI, Room/DataStore/Keystore | Operational |
| Android acoustic WebRTC media | Interface and SDP/ICE boundary implemented; checked-in provider is a clearly labelled no-audio simulator |
| Android answer playback and immediate barge-in without task cancellation | Operational through an installed offline Android `TextToSpeech` voice; Core mock TTS chunks remain a simulator |
| Structured shell, filesystem, Git, and SSRF-hardened HTTP tools | Operational; shell is wired into the approval runtime, the others are typed policy-ready packages |
| Desktop React companion and native Wails shell | Operational in browser/mock and live-core modes; host-native Linux, macOS ARM64/Intel, and Windows x64 build gates are checked in, while signed installers and optional tray hooks remain release work |
| Slack | Verified HTTP Events ingress plus deterministic simulator; Socket Mode/live outbound requires credentials |
| Mattermost | Verified compatibility ingress plus deterministic simulator; production bot WebSocket/outbound requires credentials |
| Teams | Fail-closed Activity/JWT boundary plus deterministic simulator; live Bot Connector endpoint requires public HTTPS and credentials |
| Generic signed webhooks and local CLI events | Operational |
| Push wake, TURN, cloud AI/STT/TTS | Optional, not configured |

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and the ADRs for decisions; limitations are never presented as completed functionality.

## Repository layout

```text
apps/android/             Kotlin + Compose Android application
apps/desktop/             React + TypeScript desktop companion frontend
cmd/veqri-core/           local daemon entry point
cmd/veqri-cli/            authenticated CLI entry point
core/                     transport-independent domain/runtime packages
connectors/               normalized messaging and local-event adapters
agents/                   built-in and adapter agent implementations
tools/                    structured PC tool implementations
protocol/proto/veqri/v1/  canonical cross-platform contracts
protocol/generated/       reproducible generated clients
deploy/                   Docker, systemd, launchd, and Windows service assets
docs/                     architecture, security, operations, and ADRs
tests/                    deterministic integration and end-to-end scenarios
```

## Prerequisites

- Go `1.26.5`.
- Protobuf compiler `35.1` and `protoc-gen-go v1.36.11` / `protoc-gen-go-grpc v1.6.2` for regeneration.
- Node `24.17.0` LTS recommended (the desktop build is also verified on Node `22.23.1`).
- JDK 21 and Android SDK platform 37/build-tools 37 for Android. The Gradle 9.4.1 wrapper is checked in and checksum-verified.
- Linux native desktop builds additionally need GTK 3 and WebKitGTK development packages. Ubuntu 24.04 uses `libgtk-3-dev` and `libwebkit2gtk-4.1-dev`.

## One-command application builds

Build the self-contained application for the current desktop OS from the repository root:

```sh
go run ./cmd/veqri-build
```

The output is one launchable artifact under `build/release`: `Veqri.app` on macOS, `veqri-desktop.exe` on Windows, or `veqri-desktop` on Linux. `build/release/buildinfo.json` records the exact identity embedded in the artifact. Ordinary local builds use `0.1.0-dev`; an official release build must opt in with `--release` and provide a strict SemVer, full commit, and RFC3339 build time. The desktop executable contains both the Wails UI and Core. On launch it starts its own managed Core process from the same executable, waits for readiness, proves child ownership with an ephemeral nonce, verifies credential compatibility, and injects the local credential. Closing the app stops that managed Core; an unexpected Core exit closes the owning UI instead of leaving it connected to a replaceable port. If another Core already owns the loopback origin, managed mode fails safely; set `VEQRI_DESKTOP_CORE_MODE=external` only when that existing process is explicitly trusted and managed elsewhere.

For a release-metadata build, use one identity for Core, CLI, and desktop:

```sh
export VEQRI_VERSION=0.1.0-rc.1
export VEQRI_COMMIT="$(git rev-parse HEAD)"
export VEQRI_BUILD_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
go run ./cmd/veqri-build --release binaries
go run ./cmd/veqri-build --release desktop
```

Build the Android client without exporting an SDK path manually:

```sh
go run ./cmd/veqri-build android
```

This locates Android SDK Platform 37 in `ANDROID_HOME`, `ANDROID_SDK_ROOT`, or the standard macOS/Linux/Windows SDK directory and writes `build/release/Veqri-android-debug.apk`. The APK uses the real Core transport and defaults to `http://10.0.2.2:7342` for an emulator. It is debug-signed and is not a store release.

Build the standalone binaries plus both application artifacts available on the current host. The combined `all` target is development-only; Android release identity is outside this builder:

```sh
go run ./cmd/veqri-build all
```

Native desktop packages must be built on their target OS; the CI matrix produces Linux x64, macOS ARM64/Intel, and Windows x64 artifacts. One physical file cannot run across desktop and Android runtimes, so each platform receives its native format while sharing this build entry point.

macOS setup used for this build:

```sh
brew install go protobuf
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
cd /path/to/veqri/apps/desktop && npm ci
cd /path/to/veqri/apps/android && ./gradlew --version
```

## Generate protocol code

```sh
./scripts/generate-protocol.sh
```

Generated Go code is committed so normal builds do not depend on a generator. Regeneration must leave `git diff` clean.

## Start Veqri Core

The secure default binds only to `127.0.0.1:7342`. The first start stores the admin credential in the OS keychain (or a clearly reported `~/.veqri/admin.token` `0600` fallback on headless systems) and creates `~/.veqri/veqri.db`. On Unix, Core enforces `0700` on its data/artifact directories and `0600` on the SQLite database, fallback token, backups, and diagnostic exports.

```sh
go run ./cmd/veqri-core
```

For an isolated development instance:

```sh
export VEQRI_DATA_DIR="$PWD/.veqri-dev"
export VEQRI_DATABASE="$VEQRI_DATA_DIR/veqri.db"
export VEQRI_ADDR=127.0.0.1:7342
go run ./cmd/veqri-core
```

A non-loopback bind is rejected unless both `VEQRI_TLS_CERT_FILE` and `VEQRI_TLS_KEY_FILE` are set. Copy `.env.example` as a reference; the daemon intentionally does not parse `.env` files implicitly.

## Build and use the CLI

```sh
go run ./cmd/veqri-build binaries
./build/bin/veqri version --json
./build/bin/veqri status
./build/bin/veqri ask --wait "Ask the coding agent to inspect the repository"
./build/bin/veqri task list
```

The CLI reads `VEQRI_AUTH_TOKEN`, the OS keychain, or the `admin.token` fallback inside `VEQRI_DATA_DIR` / `~/.veqri`.

Submit a local application event:

```sh
printf '{"goal":"Review the completed build"}\n' > /tmp/veqri-event.json
./build/bin/veqri emit build.completed --data /tmp/veqri-event.json --task
```

## Start the desktop UI

Mock mode is deterministic and needs no core:

```sh
cd apps/desktop
npm ci
npm run dev
```

Live mode:

```sh
cd apps/desktop
cp .env.example .env.local
# Set VITE_VEQRI_MODE=live, VITE_VEQRI_CORE_URL=http://127.0.0.1:7342,
# and VITE_VEQRI_DEV_TOKEN to a disposable development token.
npm run dev
```

Do not ship an admin token embedded in browser assets. A packaged Wails shell must inject it through the runtime bridge.

Build the self-contained native application on the current Linux, macOS, or Windows host:

```sh
cd apps/desktop
npm run native:build
```

The low-level output is `build/bin/Veqri.app` on macOS, `build/bin/veqri-desktop` on Linux, and `build/bin/veqri-desktop.exe` on Windows. These files contain the managed Core entry point as well as the UI. Prefer `go run ./cmd/veqri-build` for the consistently named `build/release` artifact. The build uses the pinned Wails v2.12.0 CLI through a platform-neutral Node driver; it does not rely on POSIX shell utilities. See [docs/RELEASE.md](docs/RELEASE.md) for the support matrix and remaining signing/installer work.

## Build and pair Android

```sh
go run ./cmd/veqri-build android
adb install -r build/release/Veqri-android-debug.apk
```

The lower-level equivalent is `cd apps/android && ./gradlew --no-daemon :app:assembleDebug -PveqriFakeTransport=false`.

Create a five-minute, single-use pairing code on the PC:

```sh
./build/bin/veqri pair
```

On Android, enter the returned core URL and eight-digit code. The emulator reaches a host loopback core at `http://10.0.2.2:7342`; a physical device needs explicitly configured TLS/LAN access. The resulting credential is stored in Android Keystore and only its SHA-256 hash is stored by Core.

## Shell approval vertical slice

Read-only structured commands from the local CLI can run under policy:

```sh
./build/bin/veqri shell --wait --cwd "$PWD" pwd
```

A state-changing command waits for a single-use approval:

```sh
./build/bin/veqri shell --cwd "$PWD" touch veqri-approval-demo.txt
./build/bin/veqri approve APPROVAL_ID
./build/bin/veqri task show TASK_ID
```

Use `veqri deny APPROVAL_ID` to verify that the command never executes. Shell interpreters (`sh -c`, `bash -c`, PowerShell command strings), privilege escalation, paths outside configured workspaces, secret-like environment injection, and automatic retries of state-changing work are denied.

## Run connector simulators

With Core running:

```sh
./scripts/simulate-connectors.sh
```

Or one adapter directly:

```sh
TOKEN=${VEQRI_AUTH_TOKEN:-$(security find-generic-password -s ai.veqri -a admin-token -w 2>/dev/null || tr -d '\r\n' < "${VEQRI_DATA_DIR:-$HOME/.veqri}/admin.token")}
curl --fail --silent --show-error \
  -H "Authorization: Bearer $TOKEN" \
  -H 'X-Veqri-Protocol-Version: 1' \
  -H 'Content-Type: application/json' \
  -d '{"text":"run the tests","channel_id":"C-local","thread_id":"T-local","message_id":"M-1"}' \
  http://127.0.0.1:7342/v1/connectors/simulate/slack
```

Progress and the final simulated reply retain the original channel/thread target and are idempotent across restart.

## Tests and checks

All Go tests, including integration/E2E and security cases:

```sh
go test -race ./...
go vet ./...
```

Desktop:

```sh
cd apps/desktop
npm ci
npm run typecheck
npm test -- --run
npm run build
```

Android:

```sh
cd apps/android
./gradlew --no-daemon testDebugUnitTest lintDebug assembleDebug
./gradlew --no-daemon assembleRelease assembleDebugAndroidTest
```

Connected instrumentation requires an attached emulator/device:

```sh
cd apps/android
./gradlew --no-daemon connectedDebugAndroidTest
```

One command for the routinely available checks:

```sh
make test
```

After installing native desktop prerequisites, build the release binaries and run the packaged-runtime smoke:

```sh
make release-check
```

## Background service and deployment

- Linux user service: `deploy/systemd/veqri-core.service`.
- macOS launch agent template: `deploy/launchd/ai.veqri.core.plist` (replace placeholders with absolute paths).
- Windows service bootstrap: `deploy/windows/install-service.ps1` (requires a service-logon-capable dedicated non-administrator credential, verifies effective/nested local-Administrators membership, and fails closed before ACL or service changes).
- Container build: `docker build -f deploy/docker/Dockerfile -t veqri-core .`.

The systemd and launchd templates use a dedicated tool workspace rather than inheriting an arbitrary current directory. The tray UI is intentionally a separate companion; orchestration never runs inside it. See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).

## Security

Veqri treats connector messages, model output, web content, files, and command output as untrusted data. A capability policy mediates tools; risky commands display exact structured arguments and wait for an expiring approval. The daemon has an emergency stop and agent/connector kill switches. Secrets are never persisted in configuration JSON or logs.

Read [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) before enabling LAN, remote agents, native app control, or live connectors. Report security problems privately rather than opening a public issue with credentials or logs.
