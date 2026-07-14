# Release readiness

Veqri ships one user-facing desktop application whose executable also contains a managed Core mode. The standalone Core and CLI remain available for service/headless and advanced workflows. Android is built and distributed separately because an APK cannot share a physical executable format with desktop operating systems. A source build or a frontend-only build is not a desktop release.

## Desktop support matrix

| Target | Automated gate | Artifact | Remaining distribution work |
|---|---|---|---|
| Linux x64 | Ubuntu 24.04 native build and standalone/managed-Core runtime smoke | `veqri-linux-x64` containing `veqri-desktop` | distro packaging and signing |
| macOS Apple silicon | macOS 15 native build and standalone/managed-Core runtime smoke | `veqri-macos-arm64` containing `Veqri.app` | Developer ID signing and notarization |
| macOS Intel | macOS 15 Intel native build and standalone/managed-Core runtime smoke | `veqri-macos-x64` containing `Veqri.app` | Developer ID signing and notarization |
| Windows x64 | Windows Server 2025 native build and standalone/managed-Core runtime smoke | `veqri-windows-x64` containing `veqri-desktop.exe` | Authenticode signing and installer UX |
| Linux ARM64 / Windows ARM64 | Core and CLI are source-buildable; native Wails support is upstream-capable | none | add stable native-runner gates before claiming release support |

The artifacts are produced by `.github/workflows/ci.yml` on push, pull request, or manual dispatch and retained for 14 days. Unix executables and the macOS app bundle are wrapped in `tar.gz` so executable modes survive artifact download; Windows uses ZIP. Each main desktop archive contains one launchable application and `buildinfo.json`. A separate `*-service-tools` artifact contains standalone Core, CLI, the same build manifest, service templates, and deployment instructions for headless/advanced installs, avoiding filename and lifecycle conflicts with the desktop app. The archives are unsigned CI artifacts, not a substitute for signed installers. A platform is considered gated only after its native job passes on that operating system; local cross-compilation alone does not count.

## What the packaged-runtime smoke proves

`scripts/release-smoke.go` runs the built CLI against Core in an isolated temporary data directory and workspace. CI invokes it once with the standalone daemon and once with the private managed-Core mode inside the packaged desktop executable. It verifies:

1. `veqri version --json`, `/healthz`, and the desktop snapshot report exactly the same version, commit, and build time.
2. Core reaches `/readyz`.
3. The built CLI can read public health and authenticated diagnostics.
4. The authenticated desktop snapshot matches protocol v1.
5. A safe `settings.update` action commits and is visible in the next snapshot.
6. Core can be stopped after the scenario.

The smoke does not launch a graphical WebView, install a background service, exercise a real model, or validate code signing. Those checks remain separate because CI runners cannot honestly substitute for an installed end-user session.

## Local release check

Install the platform prerequisites from the root README, then run:

```sh
make release-check
```

For the direct cross-platform build entry point (including Windows without `make`):

```sh
go run ./cmd/veqri-build desktop
go run ./cmd/veqri-build android
go run ./cmd/veqri-build all
```

The canonical Core, CLI, and desktop outputs from these commands use the development identity `0.1.0-dev`; Android release identity is outside this builder. Consequently, `--release android` and `--release all` fail closed—build `binaries` and `desktop` explicitly for the currently versioned release artifacts. Release metadata is accepted only with an explicit gate:

```sh
VEQRI_VERSION=0.1.0-rc.1 \
VEQRI_COMMIT="$(git rev-parse HEAD)" \
VEQRI_BUILD_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
go run ./cmd/veqri-build --release desktop
```

`VEQRI_VERSION` must be a bare SemVer, `VEQRI_COMMIT` a full lowercase hexadecimal revision, and `VEQRI_BUILD_TIME` RFC3339. Partial metadata, release metadata without `--release`, and `--release` with development defaults all fail before a compiler, npm, or Gradle subprocess starts.

On Windows, where `make` is not routinely installed, use the equivalent commands from PowerShell:

```powershell
go run ./cmd/veqri-build desktop
go run ./cmd/veqri-build binaries
go run ./scripts/release-smoke.go
go run ./scripts/release-smoke.go --core build/release/veqri-desktop.exe --core-arg=--veqri-managed-core
```

Linux requires GTK 3 and WebKitGTK development packages for the native companion. Ubuntu 24.04 uses `libwebkit2gtk-4.1-dev`; the build selects the matching Wails tag.

## Release checklist

- Protocol generation leaves no unexpected source changes.
- `go test -race ./...` and `go vet ./...` pass.
- Desktop typecheck, Vitest, frontend build, native bridge tests, and host-native build pass.
- The packaged-runtime smoke passes on every supported desktop target.
- Android unit tests, lint, debug/release APK builds, and instrumentation APK compilation pass.
- `connectedDebugAndroidTest` is run and reported separately on an attached target.
- `govulncheck` and `npm audit --audit-level=high` pass with reviewed results.
- Service assets are reviewed on their host OS; Windows policy tests do not replace a real Service Control Manager/ACL smoke.
- Release binaries use final version metadata, icons, signing, and notarization where applicable.
- Known simulators and unavailable adapters remain labelled in UI, API, and release notes.

## Known blockers to a production claim

- Built-in agent work, STT, and acoustic media are simulated unless explicit external adapters are configured.
- Android release media, push wake, store signing, and a connected-device matrix are not complete.
- Desktop CI artifacts are not signed or wrapped in end-user installers yet; the contained application itself is launchable and manages Core.
- Background-service credential bootstrap for an interactive desktop user needs an explicit product flow, especially for the dedicated Windows service identity.

Until these items are closed, describe the build as a deterministic local MVP with cross-platform release gates, not as a finished production assistant.
