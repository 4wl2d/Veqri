# ADR 0002: Secure local connectivity and explicit LAN enablement

- Status: accepted
- Date: 2026-07-12

## Decision

The daemon binds to `127.0.0.1:7342` by default. A non-loopback bind is rejected unless TLS certificate and key paths are configured. Local administration uses a randomly generated 256-bit bearer token stored in the desktop OS keychain. Headless systems without a credential service use an explicitly reported `0600` fallback file; an environment token is supported for ephemeral development. Android pairing uses a hashed, single-use, five-minute code and returns a distinct revocable device credential; only credential hashes are stored. Device key rotation uses prepare/confirm: the active hash remains valid while a confirmation-only replacement hash is pending for at most five minutes, and promotion plus audit commit atomically after the client persists the replacement. Cancellation and idempotent confirmation cover lost-response recovery.

Browser WebSockets carry the credential in a negotiated subprotocol rather than a query string. Connector requests use their platform verification mechanism plus replay protection. Secrets are referenced through environment/keychain locators and never stored in database configuration JSON.

## Consequences

LAN pairing requires TLS configuration. LAN-only incoming-call delivery cannot wake a fully stopped Android process; that needs an optional push adapter. Development remains cloud-free.
