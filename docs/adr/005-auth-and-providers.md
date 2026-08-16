# ADR-005 — auth via env and config only; custom provider URLs

**Status**: Accepted (decision D8)

## Context

The reference agent supports interactive auth: browser OAuth flows, credential
storage (`auth-storage.ts`, `auth-guidance.ts`), and `login()`/`logout()` on
the model runtime. All of these assume a human at a terminal — fuji has no
terminal and is deployed in fleets where provisioning is automated.

## Decision

Credentials come from **environment variables** (provider convention:
`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, …) or the **config file** (D9),
resolved at `NewSession`/preflight. Custom provider **base URLs** are
supported via config (mirroring the reference custom-provider URL support).
There is no interactive flow, no credential store, no `auth.json`, no OAuth.

## Consequences

- **Positive**: keys never live in files by default (env), fleet-friendly
  (inject via orchestrator), no browser dependency.
- **Positive**: `ModelRuntime` shrinks to a provider factory: id + base URL +
  key + model id → HTTP transport.
- **Negative**: OAuth-based providers (e.g. some hosted gateways) are out of
  scope in v1.
- **Negative**: no key rotation prompts — rotation is an ops concern (env
  change + redeploy).

## Alternatives

- **Keep a file credential store** — adds file permissions/encryption
  concerns; rejected for headless use.
- **Interactive login** — impossible headless; rejected.
