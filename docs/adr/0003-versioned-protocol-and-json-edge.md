# ADR 0003: Versioned Protobuf contracts with a JSON/WebSocket edge

- Status: accepted
- Date: 2026-07-12

## Decision

Cross-platform semantic contracts live in `protocol/proto/veqri/v1`. Protobuf is the canonical schema and reserves room for generated Go/Kotlin gRPC clients. The first vertical slice uses versioned JSON over authenticated HTTP and WebSocket because it is easy to inspect from Android, desktop, connector simulators, and the CLI. Every request advertises protocol version 1; incompatible majors fail explicitly.

Timestamps are UTC RFC 3339 at the JSON edge and Protobuf timestamps in generated clients. Durable IDs, correlation IDs, causation IDs, and idempotency keys are mandatory at ingestion and retained end-to-end.
