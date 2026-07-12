# Veqri protocol

`protocol/proto/veqri/v1` is the canonical cross-platform contract. Version 1 JSON APIs use the same field semantics; generated Protobuf code is intentionally reproducible rather than hand-edited.

Generate Go clients:

```sh
./scripts/generate-protocol.sh
```

Android's Gradle Protobuf plugin generates Kotlin/Java-lite bindings from the same source when that optional module is enabled. UTC timestamps, stable IDs, correlation/causation IDs, and idempotency keys are required at durable boundaries.
