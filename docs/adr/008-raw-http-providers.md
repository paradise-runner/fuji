# ADR-008 — raw `net/http` providers, no SDKs

**Status**: Accepted (decision D10)

## Context

The reference agent streams from providers through typed clients. For fuji,
the options were official provider Go SDKs (Anthropic, OpenAI) or raw
`net/http` + fuji-owned SSE parsing.

## Decision

Providers speak **raw HTTP** with fuji-owned SSE parsing. No provider SDK
dependencies, no shared HTTP layer beyond `net/http`. Each provider family
(`transport/anthropic.go`, `transport/openai.go`,
`transport/openai-compatible.go`) owns its wire grammar and maps it onto the
shared stream events (`text_delta`, `reasoning_delta`, `tool_call`, `usage`,
`stop`, `error`, `aborted`).

## Consequences

- **Positive**: full control over requests (retries, budget/usage headers,
  tool-call loop semantics, custom base URLs); zero heavyweight deps; small
  attack surface; static-binary friendly.
- **Positive**: the `StreamFn` contract (never throw; encode failures as
  events) is enforced at the transport boundary.
- **Negative**: provider protocol drift must be tracked in fuji's transports;
  no SDK conveniences (typed clients, automatic retries).
- **Negative**: each new provider family is a hand-written transport.

## Alternatives

- **Official SDKs** — faster to build, but pull in dependency trees, control
  less of the streaming/retry behavior, and complicate static builds;
  rejected.
- **A shared SSE library** — accepted only at `internal/sse` (primitive
  reader/writer), not as a full provider abstraction.
