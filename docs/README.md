# fuji — documentation index

fuji is a Go re-implementation of a **naked core** agent runtime with bundled tools. It is designed for headless use, fleet deployments, and embedding alongside other tooling (git, ripgrep, …) in a portable all-in-one coding agent.

This document tree is the migration spec: it is written so a Go engineer can
build fuji from these documents alone, using the behavioral reference model.

## Pinned source

The spec targets version 0.84.1 of the reference model. TypeScript contracts referenced throughout are from that distribution.

Public package exports: `dist/index.d.ts`. Core modules: `dist/core/*`.

## Document tree

| Doc | Purpose |
|-----|---------|
| [`decisions.md`](decisions.md) | Locked decisions (D1–D12), rationale, trade-offs, scope inventory (in/out), flagged assumptions |
| [`core-spec.md`](core-spec.md) | Architecture: module boundaries, core interfaces, data flow, deep dives (streaming, tool loop, cancellation, compaction, retries) |
| [`go-tree.md`](go-tree.md) | Proposed `cmd/`/`pkg/` tree with per-package responsibilities and interface sketches |
| [`adr/`](adr/) | Architecture Decision Records: one numbered record per consequential decision |
| [`phases.md`](phases.md) | Migration milestones 0..N with exit criteria and the golden-session parity harness |

## Reading order

1. `decisions.md` — scope and locked decisions (short)
2. `core-spec.md` — the architecture, in full
3. `go-tree.md` — how that architecture lands on disk
4. `adr/` — rationale for the consequential calls, as needed
5. `phases.md` — how to build it, incrementally

## Status

Ratified — all documents drafted against the reference model; final scope confirmation
round passed (assumptions F1–F3 resolved; cut `AgentSession` surface
recorded as V10). Docs: `decisions.md` (D1–D13), `core-spec.md`, `go-tree.md`,
nine ADRs, `phases.md`.
