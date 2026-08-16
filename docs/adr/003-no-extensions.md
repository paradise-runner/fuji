# ADR-003 — no extension model

**Status**: Accepted (decision D3)

## Context

The reference agent's extension system (loader, runner, hooks,
`discoverAndLoadExtensions`, trust prompts, package manager) is its
customization mechanism — and its largest source of moving parts. The fuji
requirement is a pure core for fleet deployments and headless use.

## Decision

fuji has **no extension system and no plugin ABI** (no WASM, no dynamic
loading, no user code executed from project directories). The tool set is
fixed at compile time. Extending fuji is not supported; customization happens
by forking the runtime.

## Consequences

- **Positive**: deterministic behavior (no user code in the process); tiny
  trust surface (no trust prompts, no `trust.json`); trivial deployment
  (nothing to install); single-threaded execution stays simple.
- **Positive**: resources that were extension-provided become either
  bundled or config-driven (skills via `.fuji`/`--skills`, prompt templates
  via config).
- **Negative**: no runtime extensibility. Any new tool or hook requires a
  fork. Teams needing different behavior must own their fork.
- **Negative**: forks diverge; bug fixes in upstream fuji don't flow to forks
  automatically.

## Alternatives

- **WASM plugin ABI** — extensibility without language coupling, but adds a
  plugin host, versioned ABI, sandboxing, and trust questions; all against
  the "pure core" requirement.
- **Compile-time plugin registration (Go `//go:linkname`-style)** — breaks
  single-binary guarantees and static builds; rejected.
