# ADR 0001: Local modular monolith with durable boundaries

- Status: accepted
- Date: 2026-07-12

## Decision

Veqri Core is one Go daemon organized as domain packages. SQLite is the source of truth for events, task graphs, approvals, tool invocations, deliveries, conversations, device identities, and audit history. HTTP/WebSocket/gRPC-compatible contracts remain adapters around the domain; Android, desktop, and connector code do not own orchestration state.

The runnable queue is derived from persisted task state. In-memory channels may wake workers but never establish ownership of work. Task claims and state transitions use transactions and optimistic versions. Restart recovery requeues safe agent work. A state-changing tool invocation recorded as started but not completed is never replayed automatically; it becomes an explicit uncertain failure.

## Rationale

A modular monolith minimizes operational complexity for a personal local-first product while retaining replaceable persistence, media, agent, tool, and connector boundaries. SQLite WAL provides robust single-user durability without a hosted database.

## Consequences

The daemon is the authoritative state owner. Horizontal multi-core execution is outside the first MVP. Remote agents and media relays remain adapters. Schema and protocol migrations require backward-compatibility review.
