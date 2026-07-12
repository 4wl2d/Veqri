# Android client

## Stack and ownership

The app uses Kotlin, Compose Material 3, coroutines/Flow, immutable ViewModel render models, Room cache, DataStore preferences, and Android Keystore credentials. Core remains authoritative for conversations, tasks, approvals, and audit.

Pinned build matrix: AGP 9.2.1, Gradle 9.4.1, Kotlin/Compose plugin 2.3.21, compile SDK 37, target SDK 36, Compose BOM 2026.06.00, Activity 1.13.0, Lifecycle 2.10.0, Room 2.8.4, DataStore 1.2.1, and OkHttp 5.3.2. The direct pins are in `apps/android/app/build.gradle.kts`; resolved configurations are locked in `apps/android/app/gradle.lockfile`.

## Pairing and transport

1. Admin creates an eight-digit, five-minute pairing code.
2. Android calls `/v1/pairing/claim` with the code, device name, and protocol version.
3. Core consumes the code atomically and returns one device credential.
4. Android stores it in Keystore; Core stores a SHA-256 hash.
5. Android opens `/v1/device/events` using bearer auth, device ID, and `veqri.v1`.

Commands are never retried by the live transport because they may cause effects. Reconnect has bounded exponential delay. A revoked device receives WebSocket close code 4003 and must pair again.

Transcript-retention changes wait for Core's correlated commit result. When retention is disabled, Android scrubs/redacts Room before attempting the fallible DataStore write. On every paired process restart it sanitizes cached content before loading UI state and waits for a socket-generation-matched authoritative snapshot, so stale local preferences or delayed events cannot disclose an older transcript while Core is offline.

Release builds require HTTPS even for `10.0.2.2`; that address is only an emulator host alias in an emulator and can be an ordinary LAN address on physical hardware. A debug-only network-security resource and `BuildConfig.DEBUG` policy permit cleartext `localhost`/`10.0.2.2` for local emulator development.

## Call behavior

The MVP uses an application-owned call UI. Incoming calls use a high-importance `CallStyle` notification. Answer opens visible UI before starting the microphone foreground service, satisfying Android 14+ while-in-use permission rules. The service declares microphone type, not `phoneCall`; Telecom/Core-Telecom remains behind an abstraction.

Full-screen intent is checked at runtime and degrades to heads-up notification if special access is unavailable. Android 13+ notification permission is requested explicitly. Audio routing uses communication-device APIs on supported versions and exposes speaker, earpiece, wired headset, and Bluetooth choices.

Official constraints: [foreground-service background starts](https://developer.android.com/develop/background-work/services/fgs/restrictions-bg-start), [microphone service type](https://developer.android.com/develop/background-work/services/fgs/service-types), [Android 14 full-screen behavior](https://developer.android.com/about/versions/14/behavior-changes-14), and [Core-Telecom evolution path](https://developer.android.com/develop/connectivity/telecom/voip-app/telecom).

## Media status

The app contains a WebRTC-compatible offer/answer/ICE engine boundary. Release builds do not silently substitute an unreviewed Maven SDK: the checked-in simulator carries no acoustic media and says so in UI. See `docs/VOICE.md` for enabling a reviewed provider.

Final answer playback is independently operational through Android's installed `TextToSpeech` engine. Core emits one bounded `tts.speak` message containing the concise spoken result; server-side synthetic chunks update progress only and are never replayed as separate utterances. The adapter selects an installed voice that declares it does not require a network connection, keeps response text out of logs and utterance IDs, reports missing voice data safely, and stops playback before sending the durable barge-in command. Install offline voice data for the device language if Android reports no suitable voice.

Approval cards show the exact canonical invocation JSON, requested permission scopes, policy reason, risk, and expiry before enabling the single-use decision. Live approval events and reconnect snapshots use the same payload; excess snapshot records are dropped whole rather than truncating arguments or scopes into a misleading approval.

## Build and test

```sh
cd apps/android
./gradlew --no-daemon testDebugUnitTest
./gradlew --no-daemon lintDebug assembleDebug assembleRelease assembleDebugAndroidTest
```

Run instrumentation only with an attached target:

```sh
adb devices
./gradlew --no-daemon connectedDebugAndroidTest
```
