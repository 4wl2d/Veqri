# Veqri protocol

`protocol/proto/veqri/v1` is the canonical cross-platform contract. `device.proto` is a self-contained projection of the current Android commands, reconnect snapshot, and events. Version 1 Android transport remains flat JSON over HTTP/WebSocket; `DeviceJsonCodec` uses the generated messages as typed codec intermediates without exposing them to repository, ViewModel, or Compose state.

Regenerate all committed bindings from the repository root:

```sh
make generate
```

This runs the Go generator and `:protocol:regenerateAndroidProtocolBindings`. The Android Java 17 module uses Protobuf Gradle plugin `0.10.0`, `protoc` `4.35.1`, and `protobuf-javalite` `4.35.1`; its committed Java Lite mirror lives in `protocol/generated/android`.

Regenerate and diff-check the Go clients, then verify the Android mirror against fresh output:

```sh
make check-generated
```

The exact Android commands are also available directly:

```sh
cd apps/android
./gradlew --no-daemon :protocol:regenerateAndroidProtocolBindings
./gradlew --no-daemon :protocol:checkAndroidProtocolBindings
```

The check task compares both the relative file set and file bytes and is part of Android CI. UTC timestamps, stable IDs, correlation/causation IDs, and idempotency keys are required at durable boundaries. `DeviceTtsSpeak.text` is limited to 12,288 UTF-8 bytes.
