# Veqri Android

Native Kotlin/Jetpack Compose client for pairing with a local Veqri Core, sending conversational requests, following delegated tasks, resolving tool approvals, and owning the Android call experience.

The checked-in debug default is intentionally deterministic and local. It exercises pairing, authenticated-client state, two-phase credential rotation, task progress, incoming/outgoing calls, transcript updates, TTS interruption, reconnects, notifications, Room caching, DataStore preferences, and Keystore credential storage without claiming that simulated media is real audio.

## Current status

| Capability | Status |
| --- | --- |
| Kotlin + Jetpack Compose + Material 3 application | Operational |
| One-time code pairing UI | Operational; simulator code is `123456` and is single-use for that simulator process |
| Real Core HTTP pairing adapter | Implemented for `POST /v1/pairing/claim`; requires a compatible Core |
| Authenticated WebSocket stream | Implemented for `/v1/device/events`, bearer auth, device identity, protocol v1, ping/pong, bounded reconnect backoff |
| Typed Android protocol boundary | Operational; the flat JSON wire is translated through generated Java Lite messages inside `DeviceJsonCodec` |
| Transport security | Release builds require HTTPS for every host; only the debug resource/policy permits cleartext `localhost` and emulator alias `10.0.2.2` |
| Command safety | Commands carry unique IDs and are never retried automatically |
| Text conversation and streaming task progress | Operational in deterministic simulator |
| Task list/detail, cancel, retry | Operational in UI and simulator |
| Expiring approval cards, approve once, deny | Operational; send a simulator request containing `approval` or `delete` |
| Room cache | Operational for messages and task summaries; schema v1 is checked in |
| Preferences DataStore | Operational for endpoint, commit-acknowledged transcript retention, and push-to-talk preference boundary |
| Android Keystore | Operational; the device access token is AES-GCM encrypted with a non-exportable Keystore key |
| Two-phase device credential rotation | Operational; active and candidate slots remain separate until HTTP confirmation or socket close code `4004` proves promotion |
| Incoming call notification and app-owned call UI | Operational for a connected app; full-screen intent is used only when Android grants the special access |
| Microphone foreground service | Operational after a visible activity has obtained microphone permission |
| Earpiece, speaker, wired, and Bluetooth route controls | Implemented through `AudioManager`; availability depends on device hardware and permission |
| WebRTC-compatible offer/answer/ICE boundary | Implemented as `VoiceMediaTransport` / `WebRtcEngine` |
| Real WebRTC media | **Not packaged.** Release builds fail clearly with “No native WebRTC engine” instead of reporting a fake connection |
| Local voice transport | **Simulated input/media only.** It sends no microphone packets; answer playback is separate from WebRTC media |
| Streaming STT/TTS | STT and Core audio chunks are deterministic simulation; one logical answer of at most 12,288 UTF-8 bytes is played through Android `TextToSpeech` |
| Android answer playback | Operational with an installed offline voice for the device language; text is split only inside the platform adapter and is never logged |
| TTS barge-in | Operational for platform playback and simulator state; interruption stops local speech immediately and does not cancel delegated tasks |
| Offline wake/push | Adapter point only. LAN delivery cannot wake a disconnected sleeping app without a configured external/self-hosted push channel |
| Android Telecom / `ConnectionService` | Deliberately deferred behind the app-owned call experience |

Google does not publish a stable first-party WebRTC Android Maven coordinate suitable for pinning here. A production media adapter should vendor a reviewed, pinned AAR (or build WebRTC reproducibly), implement `WebRtcEngine`, and replace `UnavailableVoiceMediaTransport` in release wiring. Until that happens, this module must not be described as carrying real voice media. Platform TTS makes final answers audible but does not turn the simulated microphone/WebRTC path into acoustic media.

## Pinned toolchain

- Android Gradle Plugin `9.2.1`
- Gradle `9.4.1` with wrapper SHA-256 verification
- Built-in Kotlin from AGP 9; Compose compiler plugin `2.3.21`
- `compileSdk 37`, `targetSdk 36`, `minSdk 26`
- Stable Compose BOM `2026.06.00`
- Activity Compose `1.13.0`, Lifecycle `2.10.0`
- Room `2.8.4`, DataStore `1.2.1`, OkHttp `5.3.2`
- Protobuf Gradle plugin `0.10.0`; `protoc` and `protobuf-javalite` `4.35.1`
- Resolved app and protocol configurations are pinned in `app/gradle.lockfile` and `protocol/gradle.lockfile`

Version choices follow the official [AGP 9.2 compatibility table](https://developer.android.com/build/releases/agp-9-2-0-release-notes), [built-in Kotlin migration guide](https://developer.android.com/build/migrate-to-built-in-kotlin), and [Compose BOM guidance](https://developer.android.com/develop/ui/compose/bom).

## Prerequisites

Install JDK 17 or newer and Android SDK Platform 37 plus Build Tools 36.0.0 or newer. Set the SDK location for the shell; `local.properties` is intentionally ignored because it is machine-specific.

```bash
cd apps/android
export ANDROID_HOME="$HOME/Library/Android/sdk"
export PATH="$ANDROID_HOME/platform-tools:$PATH"
```

## Exact build and test commands

Verify the wrapper and resolve dependencies:

```bash
./gradlew --version
```

Build the deterministic debug APK:

```bash
./gradlew :app:assembleDebug
```

Build the minified release artifact (unsigned until your deployment supplies a signing configuration):

```bash
./gradlew :app:assembleRelease
```

Run JVM unit tests, including deterministic transport, reconnect, approval denial, interruption, repository, and ViewModel coverage:

```bash
./gradlew :app:testDebugUnitTest
```

Verify that the committed Java Lite bindings match `device.proto` byte for byte:

```bash
./gradlew :protocol:checkAndroidProtocolBindings
```

After an intentional schema change, regenerate the committed mirror:

```bash
./gradlew :protocol:regenerateAndroidProtocolBindings
```

Compile the instrumentation test APK without a device:

```bash
./gradlew :app:assembleDebugAndroidTest
```

Run Compose instrumentation tests on an attached emulator/device:

```bash
adb devices
./gradlew :app:connectedDebugAndroidTest
```

Run the full local verification set:

```bash
./gradlew :protocol:checkAndroidProtocolBindings :app:testDebugUnitTest :app:lintDebug :app:assembleDebug :app:assembleRelease :app:assembleDebugAndroidTest
```

Install and launch the simulator build:

```bash
./gradlew :app:installDebug
adb shell am start -n com.veqri.android.debug/com.veqri.android.MainActivity
```

Pair with code `123456`. Send a normal message to exercise conversation/task completion. Send `delete a file after approval` to exercise the approval gate. The simulator only changes its own in-memory task state; it never deletes a file.

## Connect to a real Core

Build debug with the real network adapter:

```bash
./gradlew :app:assembleDebug -PveqriFakeTransport=false
```

The debug build defaults to the emulator host URL `http://10.0.2.2:7342`. The repository-level `go run ./cmd/veqri-build android` command finds a standard SDK installation, always selects this real transport, verifies the generated `BuildConfig`, and stages `build/release/Veqri-android-debug.apk`.

Then install the APK and enter a short-lived pairing code. Non-loopback endpoints must use HTTPS. Cleartext HTTP is accepted only for development hosts allowed by the debug network policy, including `localhost` and Android-emulator host alias `10.0.2.2`.

Expected pairing response:

```json
{
  "device_id": "durable-device-id",
  "access_token": "short-or-rotatable-device-token",
  "protocol_version": 1,
  "issued_at_epoch_millis": 1700000000000
}
```

The WebSocket endpoint is derived as `wss://<core>/v1/device/events` (or `ws://` for allowed loopback development). Every connection sends:

- `Authorization: Bearer <device token>`
- `X-Veqri-Device-Id`
- `X-Veqri-Protocol-Version: 1`
- `Sec-WebSocket-Protocol: veqri.v1`

The client accepts versioned event envelopes containing `id`, `type`, `correlation_id`, and `payload`. The wire remains the existing flat, snake-case JSON contract; `DeviceJsonCodec` translates it to and from generated Java Lite messages before mapping to the app's domain events and commands. Unknown/malformed events become a safe protocol error; raw payloads and authorization headers are never logged.

## Architecture

```text
Compose screens -> VeqriViewModel -> immutable render models
                               |
                               v
                       VeqriRepository
               /          |          |           \
      CoreTransport   Room cache   DataStore   Keystore
          |                 \
 HTTP pair + WebSocket       CallLifecycleController
          |                              |
 VoiceMediaTransport                 notifications + microphone FGS
          |
 WebRtcEngine boundary or explicit state-only simulator
```

- UI models contain only rendering values and stable identity fields used by lazy-list keys/actions.
- Generated protocol messages are confined to `DeviceJsonCodec`; repository, ViewModel, and Compose state continue to use immutable domain/render models.
- Compose text-entry mechanics stay local with `rememberSaveable`; business state remains in the repository/ViewModel.
- The repository is not an authoritative task store. Core events drive state, while Room is only a restart-friendly local cache.
- Foreground service startup follows Android restrictions: tapping Answer opens `MainActivity`; after it is visible and microphone permission is granted, media setup succeeds before the microphone FGS starts.
- A call survives activity recreation because repository/media/service ownership lives in the application container.
- Reconnect reuses the existing conversation/session identity. Side-effecting commands are not replayed.
- Transcript-retention actions are serialized with sends/call starts. Queue acceptance is not a commit: the switch and DataStore change only after Core's correlated `command.result`. Disabling scrubs/redacts Room before the fallible DataStore write, and paired bootstrap sanitizes cached content before displaying anything. An unknown outcome forces reconnect and blocks dependent work until a socket-generation-matched `sync.snapshot` supplies the authoritative policy; the command itself is never replayed.

## Security and privacy notes

- A one-time pairing code is sent only to the selected Core and is never persisted.
- Active and candidate access tokens are encrypted in separate AES-GCM slots using Android Keystore. Candidate version/expiry state is durable; promotion removes the old slot only after Core confirms commit.
- Android backup is disabled for this app.
- LAN/remote traffic requires TLS; the client rejects embedded URL credentials, queries, and fragments.
- Notification content is concise and uses private lock-screen visibility.
- “Forget local device” clears local credentials/cache. Immediate server-side revocation remains a Core-authoritative API operation and must not be replaced by a UI-only check.
- Transcript retention can be disabled in Conversation; pairing commits the initial device preference and reconnect snapshots reconcile it. Disabling atomically asks Core to scrub the current durable transcript before the local cache/UI is cleared. Live transcript text is cleared when the call ends. Long-term memory remains a Core concern and is not inferred from this cache.
- No authentication token, private key, raw authorization header, or media packet is logged.

## Source layout

- `network/`: Core transport contract, deterministic fake, authenticated OkHttp/WebSocket adapter
- `protocol/`: Java 17 Gradle module that generates and compiles the Android Java Lite projection
- `../../protocol/proto/veqri/v1/device.proto` and `../../protocol/generated/android/`: canonical Android projection and committed generated mirror
- `media/`: WebRTC-shaped media boundary, audio routing, explicit simulator/unavailable adapters
- `data/`: repository, immutable domain snapshots, Room/DataStore/Keystore implementations
- `call/`: incoming/active notifications, foreground service, notification actions
- `ui/`: immutable render models, ViewModel, Material 3 screens
- `src/test/`: deterministic JVM tests
- `src/androidTest/`: Compose instrumentation scaffold

## Verification status

The local and CI verification commands above include the checked-in-binding gate, JVM tests, lint, debug/release APK builds, and instrumentation APK compilation. The three instrumentation scenarios present during the last attached-device run passed on a physical Android 15 target; rerun `connectedDebugAndroidTest` for the current four-test suite when a device or emulator is available.

## Known integration work

1. Package and security-review a reproducible native WebRTC engine; add SDP/ICE and interruption integration tests.
2. Add a configurable push adapter for sleeping/disconnected incoming calls.
3. Add instrumented audio-route tests on representative Bluetooth/wired hardware.
4. Add Room migrations before incrementing schema version; destructive fallback is intentionally disabled.

When intentionally updating dependencies, regenerate and review the dependency lock:

```bash
./gradlew --write-locks :app:dependencies :protocol:dependencies
```
