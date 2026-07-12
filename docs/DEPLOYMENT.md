# Deployment

## Foreground development

```sh
export VEQRI_ADDR=127.0.0.1:7342
export VEQRI_DATA_DIR="$HOME/.veqri"
export VEQRI_DATABASE="$VEQRI_DATA_DIR/veqri.db"
export VEQRI_RETENTION_DAYS=30
go run ./cmd/veqri-core
```

`VEQRI_RETENTION_DAYS=N` enables a non-blocking startup sweep and six-hour periodic sweeps for SQLite transcript/event/safe terminal-task content and audit rows strictly older than the rolling UTC cutoff. Set it to `0` to retain indefinitely. Active and unresolved task work is deferred, and automatic expiry does not disable future retention for a conversation.

Core writes structured JSON logs to stderr. The admin credential source/path is logged, never its value. Configure log rotation and deletion in the process supervisor; SQLite retention does not control stderr logs.

## Build binaries

```sh
mkdir -p build/bin
go build -trimpath -o build/bin/veqri-core ./cmd/veqri-core
go build -trimpath -o build/bin/veqri ./cmd/veqri-cli
```

## Background services

### macOS launchd

Create private data and tool-workspace directories first:

```sh
install -d -m 700 "$HOME/Library/Application Support/Veqri"
install -d -m 700 "$HOME/Library/Application Support/Veqri/workspaces/default"
```

Copy `deploy/launchd/ai.veqri.core.plist`, then replace `__VEQRI_CORE_PATH__`, `__VEQRI_DATA_DIR__`, and `__VEQRI_WORKSPACE__` with absolute paths. Use the dedicated `workspaces/default` directory for the final placeholder; the launch agent sets both its working directory and `VEQRI_WORKSPACES` to that boundary. Then install it:

```sh
cp deploy/launchd/ai.veqri.core.plist "$HOME/Library/LaunchAgents/ai.veqri.core.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/ai.veqri.core.plist"
```

Grant accessibility/microphone automation only to the exact signed binary that needs it.

### Linux systemd user service

```sh
install -d -m 700 "$HOME/.local/share/veqri"
install -d -m 700 "$HOME/.local/share/veqri/workspaces/default"
install -Dm755 build/bin/veqri-core "$HOME/.local/bin/veqri-core"
install -Dm644 deploy/systemd/veqri-core.service "$HOME/.config/systemd/user/veqri-core.service"
systemctl --user daemon-reload
systemctl --user enable --now veqri-core
```

The unit uses the dedicated `~/.local/share/veqri/workspaces/default` directory as both `WorkingDirectory` and the only configured tool workspace. Add explicit workspace paths to `VEQRI_WORKSPACES` and `ReadWritePaths` together if broader access is intentional; do not point a background service at the user's home directory.

### Windows

Create a dedicated non-administrator Windows account, grant it the **Log on as a service** right, ensure it can read and execute the installed binary, and run an elevated PowerShell only for service creation. The installer requires the credential and an absolute tool-workspace path. Before any directory, ACL, service, or registry mutation, it obtains a Windows service-logon token for the supplied credential and verifies both `WindowsPrincipal.IsInRole(Administrator)` and the token's authorization-group SIDs. This catches direct and nested membership in local Administrators. Built-in/virtual service identities and the LocalSystem/LocalService/NetworkService SIDs are rejected. Missing Windows APIs, failed service logon, unresolved identity, or unavailable/incomplete group membership fail closed.

```powershell
$credential = Get-Credential -Message "Dedicated non-administrator Veqri service account"
./deploy/windows/install-service.ps1 `
  -BinaryPath C:\Veqri\veqri-core.exe `
  -Workspace C:\Veqri\Workspace `
  -DataDir C:\ProgramData\Veqri `
  -ServiceCredential $credential
Start-Service VeqriCore
```

`LOGON32_LOGON_SERVICE` requires the account right before installation, so an otherwise correct credential without **Log on as a service** is intentionally rejected. For local accounts use `.\user`; for domain accounts use `DOMAIN\user` or a UPN. The password is passed from `SecureString` directly to the Windows logon API and its temporary unmanaged buffer is zeroed; it is never printed.

Run the same preflight without creating directories, changing ACLs, creating the service, or writing the registry by adding `-WhatIf`:

```powershell
./deploy/windows/install-service.ps1 `
  -BinaryPath C:\Veqri\veqri-core.exe `
  -Workspace C:\Veqri\Workspace `
  -DataDir C:\ProgramData\Veqri `
  -ServiceCredential $credential `
  -WhatIf
```

The script uses the verified account SID rather than re-resolving an untrusted display name. The service identity receives Modify on its workspace. For the sensitive data directory, the installer rejects reparse points at the root, in descendants, or in its ancestor path; takes ownership as local Administrators; replaces the root DACL; resets existing descendants to inherit it; and verifies both ownership and that only the service identity (Modify), SYSTEM (Full Control), and local Administrators (Full Control) remain before service creation. It does not rely on additive grants that could preserve an older explicit `Users` or `Everyone` ACE, nor does it leave an untrusted owner able to rewrite the DACL. The installer then writes loopback/data/database/workspace environment values to the service-specific registry key. Review the account's remaining non-administrator rights and service identity before starting it. Veqri denies privilege escalation; installing the service does not grant agents administrator tools.

Pure policy tests can run under PowerShell/Pester on Windows, Linux, or macOS because they inject synthetic token assessments and never invoke `LogonUserW`. A Go static test additionally verifies that the native check remains before every installer mutation:

```sh
pwsh -NoProfile -Command "Invoke-Pester ./deploy/windows/tests/ServiceAccountPolicy.Tests.ps1"
go test ./deploy/windows
```

These tests cover LocalSystem, direct and nested administrator membership, built-in SIDs, unavailable group verification, and a safe non-administrator assessment. They do not replace a Windows-host smoke test of the real service-logon token, Service Control Manager, registry, and ACL operations.

## Docker

```sh
docker build -f deploy/docker/Dockerfile -t veqri-core .
docker run --rm -v veqri-data:/var/lib/veqri veqri-core
```

The image intentionally binds loopback, which is suitable for a same-container client but not host exposure. Explicit TLS/reverse-proxy configuration is required to publish it. Host PC automation is generally better served by the native daemon.

## LAN / remote modes

1. Same-device loopback: default.
2. LAN: TLS certificate/key, firewall, Android pairing.
3. User VPN: preferred remote mode.
4. User reverse proxy: required for Teams/public webhook; preserve raw request bodies.
5. Self-hosted relay: optional adapter, not included.
6. TURN: optional media provider configuration.

There is no Veqri-hosted account or cloud dependency.

## Backup and upgrade

Create a desktop backup before upgrades. The backup is a consistent plain SQLite file, not an encrypted archive: store it on an encrypted volume or protect it with the operator's backup encryption. Run the new binary once in foreground; embedded migrations are transactional and versioned. Do not downgrade across a schema migration without restoring a matching backup. `PRAGMA quick_check` is exposed through diagnostics/tests.
