# ADR-004 — one-shot CLI transport

**Status**: Accepted (decision D7)

## Context

The reference agent ships interactive, print/JSON, and RPC modes. fuji is
headless and fleet-deployable. Fleet orchestrators need *some* way to drive
agents, but a server surface (RPC/HTTP/stdio daemon) reintroduces lifecycle,
auth, and port-management complexity.

## Decision

v1 exposes one-shot process invocation only:

```
fuji run --prompt <text|@file> [--session <path>] [--skills <dir>] [flags…]
```

Exit code 0 on completion, non-zero on failure/abort (see `core-spec.md` §9).
No RPC server, no JSON-over-stdio daemon, no long-lived control channel. The
agent loop is specified transport-agnostic (driven by `Session` method calls)
so a transport can be added later without core changes.

## Consequences

- **Positive**: per-task processes — the simplest fleet deployment (no daemon
  lifecycle, natural isolation, trivial scaling, no port/auth management).
- **Positive**: crash isolation; a hung agent kills only its own process.
- **Negative**: per-invocation cold start (model connection, resource
  discovery, tool setup).
- **Negative**: no warm connection reuse across tasks in v1.

## Alternatives

- **JSON-over-stdio daemon** — warm process, but adds a protocol,
  backpressure, and daemon lifecycle; deferred.
- **Lean HTTP server** — warm connections but reintroduces RPC-shaped
  complexity; deferred. Both remain possible because the loop is
  transport-agnostic.
