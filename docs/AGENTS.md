# Agent runtime

## Registry

Each registered agent declares identity, description, capabilities, accepted task types, input/output schema, tool scopes, trust, cost/latency metadata, concurrency, health, execution mode, cancellation, and streaming support.

Checked-in built-ins are deterministic local implementations for general dialog, planning, coding, research, automation, testing, and synthesis. They validate orchestration offline and label their result `simulated`. Real adapters are operational when explicitly configured: `VEQRI_REMOTE_AGENT_ENDPOINT` plus `VEQRI_REMOTE_AGENT_TOKEN_REF` registers `external.remote`; `VEQRI_STDIO_AGENT_COMMAND` plus an optional JSON argument array registers `external.stdio`. Select those IDs through the API or `veqri ask --agents`.

## Task graphs

A request to several agents creates one synthesizer root and independent child nodes. Dependency-aware workers claim children in parallel. Terminal failed/cancelled/timed-out children unblock synthesis so the root can report partial completion. The synthesizer preserves successes, failures, artifacts, and uncertainty in detailed text, then produces a concise spoken summary.

Tasks use optimistic versions and explicit transitions. Cancellation propagates through contexts. Timeouts become `TIMED_OUT`. Transient retry is bounded for safe agents; shell tasks have zero automatic retries. A remote/untrusted agent can request a scope but cannot grant it.

## Adapter rules

- Built-in/local model: no network context transfer.
- Stdio/subprocess: one structured executable and JSON framing; no shell string.
- HTTP/gRPC: authenticate endpoints, bound messages/time, minimize context, retain correlation.
- MCP: optional bridge; tool calls still pass Veqri policy rather than inheriting server authority.
- Human step: represented as an expiring approval task state.

Remote output is potentially prompt-injected data. It cannot be reclassified as trusted merely because transport authentication succeeded.
