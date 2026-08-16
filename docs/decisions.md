# fuji — decision log

Status: **ratified** (draft-level decisions confirmed during scoping; flagged
assumptions F1–F2 are open for review).

This document records the decisions that shape fuji's scope and architecture,
the rationale behind them, and the trade-offs accepted. Consequential decisions
have an associated ADR under [`adr/`](adr/).

Reference source: the reference model **0.84.1**, `dist/` tree. See
[`README.md`](README.md).

---

## D1 — fuji's public API mirrors the reference SDK surface

**Decision.** fuji's public Go API is a conceptual 1:1 map of the reference
TypeScript SDK surface:

| reference (TS) | fuji (Go) |
| TS SDK surface | fuji's Go API surface |
| **Note:** fuji provides a conceptual 1:1 map. Consumers of the original SDK must rewrite call sites.

**Rationale.** Porting is mechanical when the conceptual model is identical:
logic, tests, and session artifacts translate one-to-one. The reference SDK is
the canonical description of "the core", already decoupled from the TUI.

**Trade-offs accepted.** Go has no structural typing — the map is conceptual,
not source-compatible. Consumers of the original SDK must rewrite call sites.

**ADR.** [adr/001-core-api.md](adr/001-core-api.md)

## D2 — session format compatibility

**Decision.** fuji reads and writes the reference session format **verbatim**:
JSONL files, version **v3**, tree structure via `id`/`parentId`, append-only
with a movable leaf pointer for branching.

- Header: `{type:"session", version, id, timestamp, cwd, parentSession?}`
- Entry types: `message`, `thinking_level_change`, `model_change`,
  `compaction`, `branch_summary`, `custom`, `custom_message`, `label`,
  `session_info`
- Default location mirrors the reference:
  `~/.agent/sessions/--<cwd>--/<ts>_<uuid>.jsonl` (fuji's own default
  location per D11; see F2 for the agent dir naming)

**Rationale.** Sessions are the agent's memory and its audit trail. Format
compatibility means fuji can work on the same projects, and the (see
`phases.md`) can diff fuji output against fuji output structurally.

**Trade-offs accepted.** The v3 format carries entry types fuji does not
produce (`custom`, `custom_message` from extensions). Per F1, fuji does not
special-case them.

**ADR.** [adr/002-session-format.md](adr/002-session-format.md)

## D3 — no extension model. Pure core, bundled tools. Extending = forking

**Decision.** fuji has **no extension system** and no plugin ABI (no WASM, no
dynamic loading). The tool set is fixed at compile time and bundled into the
binary. Extending fuji is not supported; customization happens by forking.

**Rationale.** Extensions are the reference's customization mechanism and its
main source of moving parts (loader, runner, hooks, resource discovery, trust
model). Dropping them yields a pure, deterministic core that is trivial to
deploy at fleet scale and to reason about in a single-threaded process. "Fleet
deployments and headless use" was the stated requirement; fork-and-own is the
scaling mechanism for teams that need more.

**Consequences.**

- The **tool set is a contract**: it must be enumerated and specified exactly
  (see D6 and `core-spec.md` §Tools).
- No `trust.json` / project-trust machinery (no code from project dirs is
  executed).
- No package manager, no `discoverAndLoadExtensions`, no `ExtensionRunner`,
  no extension hooks in the session lifecycle.
- Resources that were "extension-provided" in the reference model become built-in or
  config-driven (skills via D11; prompt templates via config).

**ADR.** [adr/003-no-extensions.md](adr/003-no-extensions.md)

## D4 — deliverables: spec + ADRs + Go tree sketch + phases

**Decision.** The deliverable is this documentation set:

1. `core-spec.md` — architecture, module contracts, data flow, deep dives
2. `go-tree.md` — proposed `cmd/`/`pkg/` tree, per-package responsibilities
3. `adr/` — numbered ADRs for consequential decisions
4. `phases.md` — migration milestones with exit criteria and parity harness

**Rationale.** A single document becomes unwieldy; ADRs capture the *why* of
each consequential call; the Go tree sketch bridges spec → implementation; the
phase plan makes the migration a reviewed, shippable process.

## D5 — depth: module-level contracts + deep dives

**Decision.** `core-spec.md` specifies at module level: boundaries, exported
interfaces with signatures, data flow, and error/edge semantics. The
concurrency-sensitive areas get deep-dive treatment: tool-call streaming,
the agent loop, cancellation and timeouts, compaction, retry policy, session
persistence.

**Rationale.** Full class-level detail would duplicate the pinned reference model source;
module-level contracts with deep dives on the hard parts is the right
resolution for a rewrite.

## D6 — tools are bundled; the binary never depends on external tools

**Decision.** The bundled tool set is compiled into the binary. No runtime
dependency on external binaries being installed.

**Bundled tools (initial contract):**

| Tool | reference model source (`dist/core/tools/`) | Notes |
|------|-------------------------------|-------|
| `read` | `read.ts` | max bytes/lines truncation, offsets |
| `write` | `write.ts` | full-file writes |
| `edit` | `edit.ts`, `edit-diff.ts` | unified-diff / patch semantics, file mutation queue |
| `bash` | `bash.ts`, `bash-executor.ts` | spawn with timeout, working-dir sandboxing, output truncation, streaming updates |
| `grep` | `grep.ts` | ripgrep-backed search |
| `find` | `find.ts` | glob/file discovery |
| `ls` | `ls.ts` | directory listing |
| `git` | *(none — fuji addition)* | status/diff/log/commit for the all-in-one workflow |

**Bundling mechanism (decided, see ADR-006):** the external-tool backends —
ripgrep for search and git for the git tool — are **embedded as real static
binaries** via `//go:embed` and executed as subprocesses. No pure-Go
substitutes (no `go-git`, no pure-Go regex scanner). The `bash` tool uses the
host shell by design (its purpose is executing commands on the host).

**Rationale.** "You shouldn't need to worry whether a tool is available" — a
portable all-in-one agent. Fleet deployments cannot assume host tooling.

## D7 — one-shot CLI only

**Decision.** The v1 surface is one-shot process invocation:

```
fuji run --prompt <text|@file> [--session <path>] [--skills <dir>] [flags…]
```

Exit code 0 on completion, non-zero on failure/abort. **No** RPC server, **no**
JSON-over-stdio daemon, **no** long-lived control channel.

**Rationale.** Per-task processes are the simplest fleet deployment: no
daemon lifecycle, no port/auth management, natural isolation, trivial
horizontal scaling. The agent loop is specified transport-agnostic so a
transport (e.g. stdio job protocol) can be added later without core changes.

**Consequences.** Model connections are per-process: no connection reuse
across tasks; cold-start latency per invocation is accepted for v1.

**ADR.** [adr/004-transport-one-shot-cli.md](adr/004-transport-one-shot-cli.md)

## D8 — auth via env and config only; custom provider URLs supported

**Decision.** No interactive auth flows (no OAuth/browser flows, no
`auth-guidance`, no credential storage prompts). Credentials come from:

- environment variables (provider convention: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, …)
- config file (D9)

Custom provider base URLs are supported via config (mirroring the reference
`ProviderConfig` custom URL support).

**Consequences.** `ModelRuntime` in fuji is a provider factory: provider id +
base URL + API key (env/config) + model id → HTTP transport. No `login()`,
no credential store, no `auth.json`.

**ADR.** [adr/005-auth-and-providers.md](adr/005-auth-and-providers.md)

## D9 — config precedence: defaults < config file < env < flags

**Decision.** Settings resolve with strict precedence:

```
built-in defaults  <  fuji.toml/json (project + user)  <  env vars  <  CLI flags
```

Project config is repo-committable and fleet-friendly; env and flags override
for ephemeral/secret values. Settings surface (mirroring the reference model `settings.md`):
model selection, provider URLs, thinking level, tool allow/deny (fleet
safety), timeout budgets, retry settings, auto-compaction threshold,
session dir, skills dir.

**Consequences.** `SettingsManager` becomes a merged config struct with a
defined precedence resolution; no runtime settings mutation UI (the reference
`/settings` command is TUI — out of scope).

## D10 — raw `net/http`; no provider SDK dependencies

**Decision.** Providers speak raw HTTP with fuji-owned SSE parsing. No official
provider Go SDKs, no shared HTTP client layers beyond `net/http`.

**Rationale.** Full control over request/response handling (streaming, retry,
budget headers, tool-call loop semantics), zero heavyweight dependencies, and a
small attack surface for fleet deployment. `stream()` maps to an
`io.ReadCloser` SSE stream with fuji-defined event parsing per provider.

**Trade-offs accepted.** Provider protocol drift must be tracked in fuji's own
transports; no SDK-level conveniences (automatic retries, typed clients).

**ADR.** [adr/008-raw-http-providers.md](adr/008-raw-http-providers.md)

## D11 — `.fuji` resource directory and `--skills <dir>`

**Decision.** Mirror the reference agent's `.agent` directory convention with
`.fuji`:

| reference model | fuji |
|----|------|
| `~/.agent/settings.json` | `~/.fuji/settings.json` |
| `.agent/settings.json` | `.fuji/settings.json` |
| `~/.agent/skills/` | `~/.fuji/skills/` |
| `.agent/skills/` | `.fuji/skills/` |

Skills remain file-based resources (data, not code — consistent with D3): a
`SKILL.md`-style file tree discovered from the default locations or from an
explicit `--skills <dir>` flag. fuji recognizes the same skill format the reference model does
(frontmatter + body; see `dist/core/skills.ts` and the reference model `docs/skills.md`).

**Rationale.** Users already know the pattern; skills are the one resource
category that makes sense as user-extensible data in a no-extension core.

**Consequences.** No trust machinery needed (skills are inert data loaded from
explicit locations — they cannot execute code). Skills are injected into the
system prompt like the reference model does (`formatSkillsForPrompt`).

**ADR.** [adr/007-fuji-directory-and-skills.md](adr/007-fuji-directory-and-skills.md)

## D12 — pin reference model version 0.84.1

**Decision.** The spec targets the reference model **0.84.1** (the installed npm distribution).
Upgrades to the pin are deliberate, reviewed events.

**Rationale.** Avoids chasing a moving target; makes the spec traceable to a
concrete source revision; parity harness expectations are stable.

## D13 — single-threaded core execution

**Decision.** One logical thread per session: a single loop goroutine drives
the state machine; providers, tools, and persistence run inline and are
awaited sequentially; tool execution mode is fixed to `sequential` (the
reference default is `parallel` — a documented deviation, core-spec V1).

**Rationale.** Deterministic event ordering, no races by construction, low
memory profile, cheap to deploy many binaries. Sequential tool execution is
the accepted trade-off.

**ADR.** [adr/009-single-threaded.md](adr/009-single-threaded.md)

---

## D14 — `--model` is authoritative; no silent default swap

**Decision.** When the user passes an explicit model id (`--model` or
`FUJI_MODEL`), it is authoritative: `modelrt.Runtime.Model` must resolve it
from the provider catalog and fails if absent (unknown ids are added to the
catalog at startup, so lookup succeeds). It must **not** silently fall back
to the provider's first catalog model. The default-model fallback applies
only when no model was requested.

**Rationale.** The original `Model()` implementation returned the provider's
first model whenever the requested id was not found, and the catalog-merge
code relied on `Model()` to detect absence — so `--model meta-llama/...`
silently streamed `gpt-5`, costing real tokens and breaking model
selection. Explicit user intent must win over convenience fallbacks.

---

## Scope inventory

> **Final confirmation round (maintainer, passed):** all rows below confirmed
> as-is; the cut `AgentSession` surface is recorded explicitly (see
> `core-spec.md` V10).

### In scope (fuji's core)

| reference model source (`dist/`) | fuji role |
|---------------------|-----------|
| `core/agent-session.ts` | session lifecycle: prompt loop, subscribe, compaction triggers, bash execution, model/thinking management (minus extension bindings, HTML export, TUI-only surface) |
| `core/agent-session-runtime.ts` | session construction/wiring |
| `core/agent-session-services.ts` | service composition (settings, loader, session manager) |
| `core/session-manager.ts` | JSONL v3 session store: tree/leaf/branch, context building, migration |
| `core/model-runtime.ts`, `core/model-registry.ts`, `core/model-resolver.ts` | model/provider registry and selection |
| `core/messages.ts` | message model (text/image content blocks, bash execution messages) |
| `core/event-bus.ts` | pub/sub bus |
| `core/skills.ts` | skill discovery + system-prompt formatting |
| `core/prompt-templates.ts` | file-based prompt template expansion |
| `core/tools/*` | bundled tools (D6) |
| `core/tools/edit-diff.ts`, `file-mutation-queue.ts` | edit semantics |
| `core/compaction/` | compaction: cut-point finding, summarization, auto-compaction |
| `core/bash-executor.ts`, `core/exec.ts` | process execution primitives |
| `core/settings-manager.ts` | settings model (adapted to D9 precedence) |
| `core/resource-loader.ts` | resource discovery (subset: skills, prompt templates, context files) |
| `core/source-info.ts` | source metadata for tool definitions |
| `core/defaults.ts`, `core/config.ts` | defaults and path resolution |
| provider transports | raw HTTP + SSE per provider (new, per D10) |

### Out of scope (stripped from the reference)

| reference model surface | reason |
|------------|--------|
| `modes/interactive/` (TUI: editor, footer, components, themes) | replaced by headless operation |
| `modes/index.ts` `runPrintMode`, `runRpcMode`, `RpcClient` | replaced by one-shot CLI (D7) |
| `modes/` print/JSON output renderers | minimal structured output instead |
| `core/extensions/*` (loader, runner, wrapper, types) | D3 |
| `core/trust-manager.ts` | no untrusted code executed (D3) |
| `core/auth-storage.ts`, `core/auth-guidance.ts` | D8 |
| `core/package-manager.ts` | no packages (D3) |
| `core/export-html/` | TUI-adjacent, out of v1 |
| `core/footer-data-provider.ts`, `core/keybindings.ts` | TUI-only |
| `core/experimental.ts` | reviewed feature flags, not core |
| `utils/clipboard.ts`, `utils/image-*`, `utils/shell.ts` | TUI/UX utilities |
| `migrations.ts` | package-version migrations, N/A |
| `AgentSession` methods: `navigateTree`, `getUserMessagesForForking`, `sendCustomMessage`, `cycleModel`, `setSessionName`, `exportToHtml`, `clearQueue`, pending-message getters, `bindExtensions` | TUI/extension-adjacent; explicitly excluded from fuji's `Session` surface (see `core-spec.md` V10) |

## Flagged assumptions (resolved 2025 — confirmed by maintainer)

- **F1 — session interop edge cases — RESOLVED as stated.** fuji does **not**
  special-case reference session entries it doesn't produce (`custom`,
  `custom_message`, foreign tool calls): it processes what it understands and
  ignores the rest. Session format compatibility (D2) stays.
- **F2 — `.fuji` vs the reference agent dir — RESOLVED: fuji uses its own
  locations.** `~/.fuji` (user) and project `.fuji/` (D11, ADR-007). No
  sharing of the reference's global state.
- **F3 — bundling mechanism — RESOLVED: embed the real binaries.** Both
  ripgrep and git are embedded static binaries (ADR-006); no `go-git`, no
  pure-Go substitutes. The `bash` tool uses the host shell by design.
