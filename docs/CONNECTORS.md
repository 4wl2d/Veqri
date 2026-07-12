# Messaging and event connectors

Verified against official documentation on 2026-07-12. Live credentials are optional; deterministic simulators cover normalization, dedupe, progress, and same-thread final delivery.

## Shared contract

Adapters start/stop, report health, verify incoming requests, normalize into `EventEnvelope`, send/update messages, reply in thread, send progress, upload artifacts, resolve identity, and deduplicate. Connector-specific objects never cross into orchestration.

Content remains `untrusted`; identity, channel selection, mention state, quiet hours, rate limits, task policy, approval policy, and auto-call policy are evaluated independently.

## Slack

Local-first production should prefer [Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/): persist/dedupe then acknowledge `envelope_id`, reconnect on refresh. Optional HTTP Events verifies the untouched raw body using Slack's [v0 HMAC recipe](https://docs.slack.dev/authentication/verifying-requests-from-slack/), rejects timestamp skew over five minutes, and dedupes `event_id`. Bot-originated messages are filtered.

Thread replies use [`chat.postMessage`](https://docs.slack.dev/reference/methods/chat.postMessage/) with the original root `thread_ts`; a new child never becomes the root. The checked-in HTTP verifier/normalizer and simulator are operational. Socket Mode and live outbound require a bot token/signing-secret reference.

## Mattermost

Production uses a least-privileged [bot account](https://developers.mattermost.com/integrate/reference/bot-accounts/) with `/api/v4/websocket`, dedupes post ID, ignores its own user, and replies via `POST /api/v4/posts` with the original `root_id`. See the official [API documentation](https://developers.mattermost.com/api-documentation/).

Outgoing webhooks are compatibility-only: they work in public channels and define a shared body token, not HMAC. The simulator JSON endpoint uses constant-time token comparison. Live WebSocket/outbound requires a bot-token reference.

## Microsoft Teams

Do not begin new work on the archived Bot Framework SDK; support ended 2025-12-31. Microsoft recommends the [Teams SDK / Microsoft 365 Agents SDK](https://learn.microsoft.com/en-us/microsoftteams/platform/teams-sdk/teams/sdk-comparison). A Go deployment can use the official [Bot Connector Activity REST](https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-concepts?view=azure-bot-service-4.0) directly or isolate Teams SDK v2 in a sidecar.

Live ingress must implement the complete [Bot Connector JWT specification](https://learn.microsoft.com/en-nz/azure/bot-service/rest-api/bot-framework-rest-connector-authentication?view=azure-bot-service-4.0): RS256/current JWKS, issuer, audience, nbf/exp, exact service URL, endorsement, refresh, and fail-closed behavior. The checked-in boundary refuses live activity without a verifier. Teams also needs a public HTTPS callback; use a user-managed reverse proxy/tunnel.

## Generic webhook and local events

Generic webhooks sign `timestamp.nonce.raw_body` with HMAC-SHA256, allow five minutes of skew, and persist each nonce for replay prevention. Local apps may use authenticated CLI emission, the signed localhost SDK, stdio JSON, process-completion, filesystem, IDE/build, CI callback, or Git hook adapters.

## Simulation

```sh
./scripts/simulate-connectors.sh
```

Simulator connector IDs end with `-simulator`; final deliveries are recorded and emitted as same-target `connector.reply` events. Live adapters keep delivery pending until their credentialed sender confirms success.
