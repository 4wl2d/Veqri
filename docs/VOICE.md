# Voice architecture

## Boundaries

- `VoiceActivityDetector`: speech probability per frame.
- `StreamingSTT`: audio frames to partial/final transcripts.
- `StreamingTTS`: text to cancellable chunks.
- `MediaTransport`: start/recover authenticated media sessions.
- `WakeWordDetector`: optional local trigger.
- `RecordingPolicy`: per-device/conversation consent.

Dialog state is persisted separately from media. The explicit states are `IDLE`, `RINGING`, `CONNECTING`, `LISTENING`, `TRANSCRIBING`, `THINKING`, `DELEGATING`, `WAITING_FOR_RESULT`, `SPEAKING`, `INTERRUPTED`, `WAITING_FOR_APPROVAL`, `RECONNECTING`, `FAILED`, and `ENDED`.

## Deterministic pipeline

The default provider is operational for control-plane testing:

1. Core initiates or Android starts a session.
2. The simulator transitions through connecting/listening.
3. UTF-8 frame fragments become partial/final mock STT results.
4. A final transcript creates a durable task.
5. Core emits one bounded full spoken summary to Android and also streams mock TTS word chunks as server-side progress evidence.
6. Android speaks the full summary once through an installed offline platform voice; chunks never trigger additional utterances.
7. Barge-in stops Android playback, cancels the Core TTS context immediately, and transitions to `INTERRUPTED`.
8. The delegated task remains completed/running; interruption never cancels it.
9. Reconnect recovers the same session ID/conversation.

Android platform TTS provides audible answer playback but is not WebRTC media and does not provide microphone input. The simulator does not provide Opus, echo cancellation, noise suppression, gain control, TURN, or acoustic latency evidence.

Core retains each live `MediaSession` handle by durable voice-session ID. End, decline-after-answer, setup failure, replacement during reconnect, and daemon shutdown remove and close that handle exactly once; terminal database state never intentionally leaves a media capture/transport resource running.

## Real WebRTC provider

A provider must use full-duplex audio, Opus, AEC/NS/AGC, authenticated signaling, and recoverable session IDs. Data-channel/event-stream messages carry transcript, state, task progress, approvals, call control, interruption, ping/pong, and capability negotiation.

Google currently does not publish a stable supported Android Maven coordinate for WebRTC. The reproducible official route is a pinned WebRTC branch-head build using [`build_aar.py`](https://webrtc.googlesource.com/src/+/refs/heads/main/tools_webrtc/android/build_aar.py), with commit/checksum/licenses archived. Any third-party prebuilt SDK requires a dedicated supply-chain ADR.

Pion WebRTC can implement the Go side behind `MediaTransport`; TURN remains user-managed. Latency targets (500 ms partial transcript, 250 ms visible state change, prompt first audio, perceptibly immediate interruption) are measured objectives, not hardcoded timers.

## Push limitation

Doze can suspend LAN networking, so a stopped/sleeping Android app cannot reliably ring over a private socket. An optional high-priority user-visible push adapter is necessary for reliable wake; the local simulator documents this instead of pretending otherwise. See [Android Doze guidance](https://developer.android.com/training/monitoring-device-state/doze-standby).
