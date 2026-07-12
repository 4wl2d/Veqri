# Veqri Desktop

React + TypeScript + Vite frontend for Veqri’s lightweight native tray companion. It contains presentation and authenticated transport code only; task decomposition, policy evaluation, approvals, retries, connector behavior, and tool execution remain in Veqri Core.

## Prerequisites

- Recommended production runtime: Node.js 24.17.0 LTS
- Supported for this package: Node.js `>=22.12.0 <25`
- Verified locally with Node.js 22.23.1 and npm 10.9.8

Dependencies are exact-pinned in `package.json` and resolved in `package-lock.json`.

## Exact commands

Install from a clean checkout:

```bash
cd apps/desktop
npm ci
```

Start the deterministic mock dashboard (no Core, network, or credentials required):

```bash
cd apps/desktop
cp .env.example .env.local
npm run dev
```

Open the printed localhost URL. `.env.example` defaults to `VITE_VEQRI_MODE=mock`.

Start against a live localhost Core in browser-development mode:

```bash
cd apps/desktop
cp .env.example .env.local
```

Then edit `.env.local` to set `VITE_VEQRI_MODE=live`, `VITE_VEQRI_CORE_URL=http://127.0.0.1:7342`, and a disposable local-development token in `VITE_VEQRI_DEV_TOKEN`, followed by:

```bash
npm run dev
```

Never commit `.env.local`. Production native builds must inject a keychain-backed token through `window.veqriShell`; they must not bundle `VITE_VEQRI_DEV_TOKEN`.

Run the complete desktop verification:

```bash
cd apps/desktop
npm run typecheck
npm test
npm run build
```

Preview the production assets locally:

```bash
cd apps/desktop
npm run preview
```

Build the native Wails companion, which embeds `apps/desktop/dist` and injects the local Core credential at runtime rather than compiling it into frontend assets:

```bash
cd apps/desktop
npm run native:build
```

Platform outputs are `../../build/bin/Veqri.app` on macOS, `../../build/bin/veqri-desktop` on Linux, and `../../build/bin/veqri-desktop.exe` on Windows. The Node build driver uses the pinned Wails v2.12.0 CLI with production tags and structured subprocess arguments, so the same npm command works without POSIX shell utilities. Linux auto-detects WebKitGTK 4.1; set `VEQRI_WEBKIT2GTK_VERSION=4.0` or `4.1` only when an explicit distro override is required.

## Routes

The app uses hash routing so deep links work inside a native webview without server-side history fallbacks.

| Route | Screen |
| --- | --- |
| `#/` | Dashboard |
| `#/core` | Core health and emergency stop |
| `#/devices` | Paired Android devices and revocation |
| `#/voice` | Active voice/WebRTC sessions |
| `#/conversations` | Conversation history |
| `#/tasks` | Durable task list |
| `#/tasks/:taskId` | Task graph and node details |
| `#/approvals` | Exact tool arguments and single-use decisions |
| `#/agents` | Capability-based agent registry and kill switches |
| `#/tools` | Tool permissions and policy evaluation order |
| `#/connectors` | Messaging connector health, retry, and kill switches |
| `#/providers` | AI, STT, TTS, media, and push providers |
| `#/audit` | Filterable correlated audit log |
| `#/settings` | Working theme control, enforced Core retention status, and honest read-only status for remaining desktop, notification, and network preferences |
| `#/diagnostics` | Health, bounded logs, plain local SQLite backup, and exports |

Loading, empty, disconnected, retrying, and failed states are explicit. Cached snapshots stay visible during stream reconnects and are visibly marked stale. Destructive or exposure-increasing actions use named confirmation dialogs. Navigation, dialogs, forms, tables, focus indicators, live status announcements, reduced motion, and responsive layouts are keyboard/screen-reader aware.

## Native shell boundary

The shell injects one narrow bridge before React starts:

```ts
interface VeqriShellBridge {
  getRuntimeConfig(): Promise<{
    mode: "mock" | "live";
    api_base_url: string;
    auth_token: string;
  }>;
  setTrayBadge?(count: number): Promise<void>;
  showDesktopNotification?(title: string, body: string): Promise<void>;
  revealFile?(absolutePath: string): Promise<void>;
}
```

The checked-in Wails bridge reads the local Core credential at runtime (environment/keychain-backed Core source or its permission-restricted fallback file). The client restricts live origins to loopback hosts, uses Bearer authentication for HTTP, and sends the token as a base64url WebSocket subprotocol rather than in a URL. See [`API_CONTRACT.md`](API_CONTRACT.md).

## Operational status

| Capability | Status |
| --- | --- |
| All required screens and routes | Operational |
| Typed authenticated HTTP client | Operational; requires matching Core endpoints |
| Reconnecting WebSocket event client | Operational; deterministic bounded backoff |
| Deterministic mock snapshot and actions | Operational, offline |
| Approval, cancel, retry, reprioritize, dismiss, revoke, kill-switch, backup/export actions | Operational in mock and through the live Core action contract |
| Dark/light/system theme and keyboard accessibility | Operational |
| Rolling transcript/task/event/audit retention expiry | Operational in Core; read-only desktop status reflects `VEQRI_RETENTION_DAYS` (`0` disables automatic expiry) |
| Login/tray lifecycle, OS notifications, quiet hours, and LAN configuration controls | Read-only and clearly marked unavailable until enforcement is implemented; configure Core/service startup outside the UI |
| Native desktop shell and runtime credential bridge | Production-tag Wails build with host-native Linux/macOS/Windows CI gates; signing, installers, tray badge, OS notification, and file-reveal methods remain release work |
| Orchestration, security policy, tool execution | Intentionally absent from UI; authoritative in Core |

## Verification status

As of 2026-07-12, the package was verified locally on macOS with:

```text
npm run typecheck  # passed
npm test           # passed (transport, reconnect, mock actions, routes, states, safety UX)
npm run build      # passed
npm run native:build # passed; packaged Veqri.app stayed in its event loop until a clean interrupt
```

Linux x64, macOS ARM64/Intel, and Windows x64 native builds plus packaged Core/CLI smoke scenarios are defined in the repository CI matrix. They must pass on their own host runners before a release can claim those targets; see [`../../docs/RELEASE.md`](../../docs/RELEASE.md).
