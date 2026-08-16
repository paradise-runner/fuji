# ADR-009 — single-threaded core execution

**Status**: Accepted

## Context

The reference agent is single-threaded by virtue of the TS event loop, but
its default tool execution mode is `"parallel"` (concurrent tool calls per
batch, via promise-style concurrency). Go is a concurrent runtime; the naive
port spawns goroutines everywhere. The requirement is a core that is simple
to reason about and cheap to deploy in fleets.

## Decision

fuji executes **one logical thread per session**: a single loop goroutine
drives the state machine (core-spec §4.2, §7); providers, tools, and
persistence run *inline* on that goroutine and are awaited sequentially. Tool
execution mode is fixed to **`sequential`**. Go's runtime may schedule the
blocking I/O (HTTP, process spawn), but the logical execution order is total
and reproducible. Embedding applications can run multiple sessions (one per
goroutine) if they need application-level concurrency.

## Consequences

- **Positive**: deterministic event order — same input + same provider output
  ⇒ same event sequence ⇒ same session file. Reproducible bugs, trivially
  testable, auditable.
- **Positive**: no data races by construction; no locks, no actor plumbing,
  no backpressure design; low memory profile per process → fleet scale.
- **Positive**: the parity harness can assert exact tool-result ordering.
- **Negative**: models emitting multiple independent tool calls per turn take
  longer (no parallel execution). Accepted for v1.
- **Negative**: a slow tool blocks the loop (mitigated by per-tool timeouts,
  core-spec §5.3).
- **Deferred**: a future `parallel` mode can be added behind the existing
  `ToolExecutionMode` knob without changing the event contract.

## Alternatives

- **Goroutine-per-tool with `select`-driven loop** — real parallelism, but
  adds concurrency invariants, nondeterministic completion order, and
  debugging cost; rejected for v1.
- **Worker pool + queue** — complexity without a demonstrated need; rejected.
