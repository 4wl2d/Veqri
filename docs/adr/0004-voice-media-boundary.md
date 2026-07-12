# ADR 0004: Media transport boundary and deterministic voice simulator

- Status: accepted
- Date: 2026-07-12

## Decision

Call control, state synchronization, transcript updates, TTS chunks, interruption, and reconnect behavior are implemented independently of media transport. `MediaTransport`, streaming STT, and streaming TTS interfaces permit a native WebRTC adapter without coupling dialog state to one SDK.

The checked-in default is a deterministic local simulator: authenticated call signaling, simulated audio frames/transcripts, streaming TTS events, immediate barge-in, and reconnect semantics are testable without credentials. It is labelled simulated in every UI and diagnostic output. Production audio requires selecting the feature-flagged WebRTC provider and provisioning network/TURN settings where necessary.

## Consequences

The control-plane vertical slice is reproducible in CI. The repository does not claim that simulated frames provide real acoustic echo cancellation or Android-to-PC media; those properties must be validated when a concrete WebRTC SDK/provider is enabled.
