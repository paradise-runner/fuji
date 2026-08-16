# ADR-001 — fuji's public API mirrors the reference agent's SDK surface

**Status**: Accepted (decision D1)

## Context

The reference agent exposes a stable SDK surface (`dist/index.d.ts`) that is
already decoupled from the TUI: `createAgentSession`, `AgentSession`,
`ModelRuntime`, `SessionManager`, `session.subscribe`, tool factories,
`EventBus`. fuji must offer an embeddable Go API without designing a novel
surface that has to be re-learned.

## Decision

fuji's public Go API is a conceptual 1:1 map of the reference agent's SDK
surface. The mapping table lives in `decisions.md` D1; interface sketches in
`core-spec.md` §4.

## Consequences

- **Positive**: porting logic is mechanical; the reference source is a
  line-by-line reference implementation; tests translate; the parity harness
diffs like against like.
- **Positive**: consumers of the reference SDK understand fuji's API
immediately.
- **Negative**: Go has no structural typing — no source compatibility;
  call sites must be rewritten.
- **Negative**: TS idioms (optional fields, unions, async) map to Go
  patterns (option structs, tagged unions via interfaces, explicit errors)
  that must be documented per-module — done in `core-spec.md`.

## Alternatives

- **Design a fresh Go-native API** — cleaner Go ergonomics, but loses the
  reference-parity property and the migration becomes a redesign.
- **Bind the reference TS core via embedded JS runtime** — rejected by the
  portability/fleet goals (node/Bun dependency, heavy binary).
