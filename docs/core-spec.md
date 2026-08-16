# fuji — core specification

**Version**: 1 (draft)
**Reference source**: the reference implementation **0.84.1**
**Scope**: the naked core — everything needed to run a headless coding agent,
nothing else.

This document is the authoritative architecture for fuji. It specifies module
boundaries, contracts, and data flow at the level a Go engineer needs to
implement the runtime from scratch. Decisions and rationale live in
[`decisions.md`](decisions.md) and [`adr/`](adr/); the on-disk package layout
is in [`go-tree.md`](go-tree.md); the build sequence is in
[`phases.md`](phases.md).

---

## 1. Purpose

fuji is a Go re-implementation of the reference agent core runtime: session
lifecycle, model/provider streaming, tool execution, session persistence,
compaction, retries, and resource handling — **without** the TUI, RPC server,
extension system, package manager, interactive auth, or any other surface not
required for headless operation.

It is designed to be:

- **Embeddable** — a library first; `cmd/fuji` is a thin one-shot CLI wrapper.
- **Portable** — one static binary, zero runtime dependencies on external
  tools (bundled tools, see §5.5), no node/Bun runtime.
- **Fleet-deployable** — one-shot process semantics (D7), config-driven (D9),
  no interactive flows (D8), deterministic single-threaded execution (§8).
- **Compatible at the session layer** — reads and writes JSONL v3
  session format verbatim (D2).

## 2. Guiding principles

| Principle | Consequence |
|-----------|-------------|
| Mirror the reference SDK surface conceptually | Porting is mechanical; the reference source is canonical |
| No extension model | Tool set is a compile-time contract; forks are the scaling mechanism |
| Single-threaded execution | Deterministic event order; trivial to reason about; cheap at scale |
| Bundled tools | The binary never depends on host tooling |
| Raw HTTP providers | Full control; no SDK dependencies |
| Config > code for customization | Settings, skills, prompt templates are data |

## 3. System overview

```
                        ┌──────────────────────────────────────────────┐
                        │                 cmd/fuji (CLI)               │
                        │   flags/env → Config  →  fuji.NewSession()   │
                        └───────────────┬──────────────────────────────┘
                                        │
                                        ▼
   ┌───────────────┐     ┌─────────────────────────────┐     ┌───────────────────┐
   │    config     │     │          Session            │     │   SessionManager   │
   │  (precedence  │────▶│  (agent loop, lifecycle)    │────▶│  JSONL v3 session  │
   │   merge)      │     │                             │     │  tree/branch/leaf  │
   └───────────────┘     │  emits Events → subscribers │     └───────────────────┘
                         └──────┬──────────────┬───────┘
                                │              │
                                ▼              ▼
                    ┌────────────────┐   ┌───────────────┐
                    │  ModelRuntime  │   │  tools (all)  │
                    │ raw HTTP/SSE   │   │ read/write/   │
                    │ per provider   │   │ edit/bash/    │
                    │ auth: env+cfg  │   │ grep/find/ls/ │
                    └────────────────┘   │ git           │
                                        └───────────────┘
   Resources (data): skills, prompt templates  ── .fuji/ and --skills dir
   Support: eventbus.Bus (integration), compaction (pure logic + LLM), exec (process primitives)
```

### 3.1 Process lifecycle (one-shot)

```
fuji run --prompt "@task.md" --cwd /repo --model claude-sonnet-4-5
```

1. Parse flags → merge with config file + env → `config.Config` (D9).
2. Discover resources (`.fuji/` + `--skills`) → skill + template sets.
3. `session.New(cfg)`:
   - resolve model from config (provider + model id + base URL + key)
   - open `SessionManager` (new session, or resume `--session <path>`)
   - register bundled tools, apply allow/deny from config
   - build system prompt (base + active tools + skills)
4. `session.Prompt(text)` — the agent loop runs to completion (§6.1).
5. Session persists on every `message_end` (append-only JSONL).
6. Exit: `0` on normal completion, non-zero on error/abort (§9).

There is no daemon, no RPC, no stdio protocol (D7). The loop itself must not
assume a transport: it is driven by method calls on `Session`, so a future
transport can wrap it without core changes.

## 4. Module contracts

Interfaces below are given in Go. They are the fuji equivalents of the
reference TypeScript contracts (source file noted per module). The on-disk
package layout is in [`go-tree.md`](go-tree.md); names here are
package-qualified.

### 4.1 `session` — Session lifecycle

*Reference source: `dist/core/agent-session.ts`,
`dist/core/agent-session-runtime.ts`,
`dist/core/agent-session-services.ts`*

```go
type Config struct {
    Cwd            string        // default: process working directory
    AgentDir       string        // default: ~/.fuji  (D11, F2)
    Config         config.Config // merged settings (D9)
    ModelRuntime   *modelrt.Runtime
    SessionManager *sessionmgr.Manager
    Skills         []skills.Skill // from .fuji discovery + --skills
    Templates      []templates.Template
    Tools          []tools.Definition // the bundled tool set (D6)
    AllowedTools   []string          // allowlist; nil = all bundled
    ExcludedTools  []string          // denylist, applied after allowlist
}

type Session struct { /* unexported */ }

func New(cfg Config) (*Session, error)
func (s *Session) Subscribe(fn func(Event)) func()   // returns unsubscribe
func (s *Session) Prompt(text string, opts PromptOptions) error
func (s *Session) Steer(text string) error           // queue mid-run steering message
func (s *Session) FollowUp(text string) error        // queue post-run follow-up
func (s *Session) Abort() error                      // cancel current op, wait for idle
func (s *Session) WaitForIdle() error
func (s *Session) Compact(customInstructions string) (CompactionResult, error)
func (s *Session) SetModel(m Model) error
func (s *Session) SetThinkingLevel(l ThinkingLevel)
func (s *Session) ExecuteBash(cmd string, onChunk func(string)) (BashResult, error)
func (s *Session) AbortBash()
func (s *Session) Dispose()
```

**State accessors** (mirror `AgentSession` getters): `Model()`, `ThinkingLevel()`,
`IsStreaming()`, `IsIdle()`, `IsCompacting()`, `IsRetrying()`, `RetryAttempt()`,
`SessionFile()`, `SessionID()`, `Messages()`, `SystemPrompt()`,
`ActiveToolNames()`, `AllTools()`, `SessionStats()`.

**Lifecycle rules** (mirror the reference):

- `Prompt` when idle starts an agent run. When streaming, it queues the message
  (`steer` = deliver after current turn's tool calls; `followUp` = deliver only
  when the agent would otherwise stop). `Prompt` without a `StreamingBehavior`
  while streaming is an error (as in the reference).
- `Prompt` validates model + auth before starting (non-streaming path).
- Session persistence is handled internally: on every `message_end` the message
  is appended via `SessionManager` (single writer).
- `Dispose` removes all listeners and detaches from the loop.
- `Abort` cancels the in-flight provider request, waits for the loop to reach
  idle, then resolves. Aborted turns leave a partial assistant message with
  `stopReason: "aborted"`.

**Event surface** (`Event` union — mirrors `AgentSessionEvent`):

```
agent_start, agent_end {messages, willRetry},
turn_start, turn_end {message, toolResults},
message_start, message_update {assistantMessageEvent}, message_end,
tool_execution_start {toolCallId, toolName, args},
tool_execution_update {toolCallId, toolName, args, partialResult},
tool_execution_end {toolCallId, toolName, result, isError},
agent_settled, queue_update {steering, followUp},
compaction_start {reason: manual|threshold|overflow},
compaction_end {reason, result, aborted, willRetry, errorMessage},
entry_appended {entry}, session_info_changed, thinking_level_changed,
auto_retry_start {attempt, maxAttempts, delayMs, errorMessage},
auto_retry_end {success, attempt, finalError},
summarization_retry_* {…}, bash_execution_update {id, delta}
```

Subscribers receive events synchronously in loop order. A subscriber may not
re-enter the session (deadlock avoidance: the loop dispatches events
sequentially and does not call back into itself).

### 4.2 `loop` — the agent loop

*Reference source: `agent-core/dist/agent-loop.ts`,
`dist/core/agent-session.ts` (turn orchestration)*

The agent loop is a **single-threaded state machine** in fuji. It converts the
conversation context to provider messages, streams an assistant response,
executes tool calls, and repeats until the model stops calling tools.

```go
type LoopConfig struct {
    Model            modelrt.Model
    ThinkingLevel    ThinkingLevel
    ConvertToLLM     func([]AgentMessage) []LLMMessage   // AgentMessage → provider messages
    TransformContext func([]AgentMessage) []AgentMessage // e.g. pruning; must not throw
    GetAPIKey        func(provider string) (string, bool)
    ShouldStopAfterTurn func(ctx ShouldStopContext) bool
    PrepareNextTurn  func(ctx ShouldStopContext) *TurnUpdate
    GetSteeringMessages func() []AgentMessage
    GetFollowUpMessages func() []AgentMessage
    BeforeToolCall   func(ctx BeforeToolCallContext) (*BeforeToolCallResult, error)
    AfterToolCall    func(ctx AfterToolCallContext) (*AfterToolCallResult, error)
    Signal           context.Context // cancellation
    StreamFn         StreamFn        // provider stream (satisfied by ModelRuntime)
}

// StreamFn contract (mirrors reference): must NOT return an error for
// request/model/runtime failures. Failures are encoded in the returned stream
// via protocol events and a final assistant message with StopReason
// "error"|"aborted" and ErrorMessage set.
type StreamFn func(model modelrt.Model, ctx Context, opts StreamOptions) (*Stream, error)

type Stream struct {
    // channel of events: text_delta, reasoning_delta, tool_call,
    // tool_result_usage, stop, error, aborted, …
}
```

**Loop algorithm** (mirrors `runAgentLoop` / `runAgentLoopContinue`):

1. Emit `agent_start`. Append prompt messages to context.
2. **Turn**: emit `turn_start`. Build provider messages
   (`TransformContext` → `ConvertToLLM`). Resolve API key. Call `StreamFn`.
3. Stream events → `message_start`/`message_update`/…; accumulate assistant
   message; decode tool calls (and reasoning deltas when thinking enabled).
4. On stream stop: if `stopReason` is `tool_use`, execute the tool batch
   (§6.2); else the turn is done.
5. Emit `turn_end {message, toolResults}`.
6. `ShouldStopAfterTurn`? → emit `agent_end`, return.
7. `PrepareNextTurn` may replace model/context/thinking for the next turn.
8. Drain steering queue (`GetSteeringMessages`) → if non-empty, inject and go
   to step 2.
9. Drain follow-up queue (`GetFollowUpMessages`) → if non-empty, inject and go
   to step 2.
10. Emit `agent_end {messages, willRetry}`.

**Deviation from the reference (deliberate, documented):** the reference's
default tool execution mode is `"parallel"` (concurrent tool execution). fuji
runs **sequential** mode only — one tool at a time, deterministic completion
order, no concurrency (§8). The `ToolExecutionMode` knob exists in the
contract for future work but is fixed to `sequential` in v1.

### 4.3 `modelrt` — ModelRuntime and providers

*Reference source: `dist/core/model-runtime.ts`, and the reference provider
clients*

```go
type Runtime struct { /* providers, models, auth resolution */ }

func New(cfg Config) (*Runtime, error)
func (r *Runtime) Providers() []Provider
func (r *Runtime) Models(providerID string) []Model
func (r *Runtime) Model(providerID, modelID string) (Model, bool)
func (r *Runtime) Stream(model Model, ctx Context, opts StreamOptions) (*Stream, error)
func (r *Runtime) CheckAuth(providerID string) error
```

**Provider model** (D8, D10): a provider is a named configuration of

```
ProviderID  string  // e.g. "anthropic"
BaseURL     string  // custom URLs supported
APIKey      string  // from env (e.g. ANTHROPIC_API_KEY) or config; never interactive
Models      []Model // id → capabilities (thinking levels, context window, tool support)
```

**Transport contract** (raw `net/http`, no SDKs):

- `Stream` opens a POST to `{baseURL}/v1/messages` (Anthropic-style) or
  `{baseURL}/chat/completions` (OpenAI-style) with streaming enabled.
- Response bodies are parsed incrementally: an SSE reader (`bufio.Scanner`
  over `text/event-stream`) feeds a typed event channel.
- Each provider family owns its event grammar (e.g. `content_block_delta`,
  `message_delta`, `message_stop` for Anthropic) mapped onto the shared
  `Stream` events: `text_delta`, `reasoning_delta`, `tool_call`, `usage`,
  `stop`, `error`, `aborted`.
- Failures never escape `Stream` as errors (StreamFn contract): they are
  encoded as `error`/`aborted` events and a final assistant message with
  `StopReason == error`.
- `complete` (non-streaming) is `Stream` + accumulate.

**Thinking levels**: `off|minimal|low|medium|high|xhigh|max`, clamped to model
capabilities. Default `medium` (the reference default). Requests carry the
level per provider convention.

**Retry policy** (mirrors the reference settings): exponential backoff with
jitter over a budget; retryable classes: overloaded, rate-limit, transient
5xx, stream drops. Context-overflow errors are **not** retryable (compaction
handles them). Retry state surfaces via `auto_retry_start/end` events. The
same budget/backoff applies to compaction and branch-summary summarization
calls.

### 4.4 `sessionmgr` — session persistence

*Reference source: `dist/core/session-manager.ts`*

JSONL v3 format, **verbatim compatibility** (D2). Entries:

| Type | Fields (besides `type,id,parentId,timestamp`) |
|------|------------------------------------------------|
| `session` (header, first line) | `version: 3, id, timestamp, cwd, parentSession?` |
| `message` | `message: AgentMessage` |
| `thinking_level_change` | `thinkingLevel` |
| `model_change` | `provider, modelId` |
| `compaction` | `summary, firstKeptEntryId, tokensBefore, details?, usage?, fromHook?` |
| `branch_summary` | `fromId, summary, details?, usage?, fromHook?` |
| `custom` | `customType, data?` — ignored by fuji (F1) |
| `custom_message` | `customType, content, details?, display` — ignored by fuji (F1) |
| `label` | `targetId, label?` |
| `session_info` | `name?` |

`label` and `session_info` entries are **read for format compatibility only —
fuji never writes them** (no bookmark/naming surface; `setSessionName` is
cut, see V10).

```go
type Manager struct { /* sessionId, sessionFile, fileEntries, byId, leafId */ }

func Create(cwd string, sessionDir string) *Manager   // new
func Open(path string, sessionDir string, cwdOverride string) (*Manager, error)
func ContinueRecent(cwd string, sessionDir string) (*Manager, error)
func InMemory(cwd string) *Manager
func List(cwd string, sessionDir string) ([]SessionInfo, error)

func (m *Manager) AppendMessage(msg AgentMessage) string      // returns entry id
func (m *Manager) AppendThinkingLevelChange(l string) string
func (m *Manager) AppendModelChange(provider, modelID string) string
func (m *Manager) AppendCompaction(summary, firstKeptEntryID string, tokensBefore int) string
func (m *Manager) Branch(fromID string)
func (m *Manager) ResetLeaf()
func (m *Manager) BranchWithSummary(fromID string, summary string) string
func (m *Manager) BuildSessionContext() SessionContext        // compaction-aware
func (m *Manager) BuildContextEntries() []SessionEntry
func (m *Manager) GetTree() []SessionTreeNode
func (m *Manager) GetBranch(fromID string) []SessionEntry
func (m *Manager) GetHeader() *SessionHeader
func (m *Manager) GetSessionID() string
func (m *Manager) GetSessionFile() string
```

**Semantics** (mirror the reference exactly):

- Append-only files; entries form a tree via `id`/`parentId`; the leaf pointer
  marks the current path; `Branch` moves the leaf (no history rewrite);
  `branchWithSummary` appends a `branch_summary` entry.
- `BuildSessionContext` walks leaf→root, applies compaction entries (latest
  compaction entry replaces everything before `firstKeptEntryId`), converts
  entries to LLM messages (`sessionEntryToContextMessages`).
- Migration on load: v1/v2 → v3 (`migrateSessionEntries`).
- Default directory mirrors the reference:
  `{agentDir}/sessions/--{cwd with '/'→'-'}--/{unixTs}_{uuid}.jsonl`.
  fuji's `agentDir` default is `~/.fuji` (D11, F2).
- Persistence: write-through append per entry (`entry_appended` event);
  buffered flush per agent turn.

### 4.5 `tools` — bundled tool set

*Reference source: `dist/core/tools/*`,
`dist/core/tools/tool-definition-wrapper.ts`*

```go
type Definition struct {
    Name        string            // LLM-facing name, e.g. "read"
    Label       string            // human label
    Description string            // LLM description
    PromptSnippet    string       // optional system-prompt snippet
    PromptGuidelines []string     // optional system-prompt guideline bullets
    Parameters  json.RawMessage   // JSON Schema (Go) — fuji's schema language
    // Execute runs the tool. Never panics; returns result or error.
    Execute    func(callID string, params json.RawMessage,
                   ctx context.Context, onUpdate func(Update)) (Result, error)
}

type Result struct {
    Content    []ContentBlock // text/image, returned to the model
    Details    any            // structured detail for logs
    IsError    bool
    Terminate  bool           // hint: stop after current tool batch (all must agree)
}
```

**Bundled tools** (D6 — the contract):

| Tool | Params | Semantics (reference parity) |
|------|--------|----------------------|
| `read` | `path, offset?, limit?` | `DEFAULT_MAX_BYTES`/`DEFAULT_MAX_LINES` truncation; image MIME detection (no resize in v1 — no image pipeline) |
| `write` | `path, content` | full-file write |
| `edit` | `path, oldString, newString, replaceAll?` | unique-match semantics via unified diff (`edit-diff.ts`), file mutation queue serialization |
| `bash` | `command, timeout?` | `bash-executor.ts` semantics: streaming output chunks, truncation, `fullOutputPath` spill-over for large outputs, abort via signal |
| `grep` | `pattern, path?, …` | ripgrep-backed (embedded rg binary, ADR-006) |
| `find` | `path, pattern?, …` | glob/file discovery |
| `ls` | `path` | directory listing with truncation |
| `git` | subcommand struct | status/diff/log/commit/… (fuji addition) — embedded git binary (ADR-006) |

**File mutation queue** (mirror `withFileMutationQueue`): all file-mutating
tools serialize through a per-session queue so concurrent edits (future
parallel mode; also `executeBash` from session) can never interleave on the
same file. Sequential mode makes this trivially safe today; the queue is kept
for contract parity.

**Truncation** (mirror `truncate.ts`): head/tail/line truncation with explicit
markers; `DEFAULT_MAX_BYTES` and `DEFAULT_MAX_LINES` defaults from the
reference.

### 4.6 `skills` and `templates` — resources

*Reference source: `dist/core/skills.ts`, `dist/core/prompt-templates.ts`*

Skills are data (D3, D11):

- Discovery order: explicit `--skills <dir>` > project `.fuji/skills/` >
  `~/.fuji/skills/`.
- Format: the reference skill format — directory per skill with `SKILL.md`
  (frontmatter: `name`, `description`; body: instructions), or root-level
  `.md` files discovered as individual skills (reference `docs/skills.md`).
- Injection: `formatSkillsForPrompt` renders an "Available skills" section
  appended to the system prompt, exactly as the reference does. Skill
  invocation syntax `/skill:name args` expands to the skill body on
  `Prompt`/`Steer`/`FollowUp` (`_expandSkillCommand` parity).

Prompt templates (reference `docs/prompt-templates.md`): file-based `*.md`
templates under `.fuji/prompt-templates/` (user) and project
`.fuji/prompt-templates/`; expansion on `Prompt` unless disabled
(`PromptOptions.ExpandPromptTemplates`).

### 4.7 `config` — settings

*Reference source: `dist/core/settings-manager.ts` (adapted to D9)*

Precedence (highest wins): **CLI flags > env vars > project config
(`.fuji/fuji.toml`) > user config (`~/.fuji/fuji.toml`) > built-in defaults**.

Settings surface (v1): model (`provider`, `modelId`, `baseURL`), thinking
level, tools allow/deny, timeout budgets (turn, bash), retry policy (max
attempts, backoff), auto-compaction (enabled, threshold, reserve, keep-recent),
session dir override, skills dir override. Secrets never in config files —
env only.

### 4.8 `eventbus` — integration bus

*Reference source: `dist/core/event-bus.ts`*

```go
type Bus interface {
    Emit(channel string, data any)
    On(channel string, handler func(any)) func()  // unsubscribe
}
```

Used by embedding consumers to observe internals beyond the session event
stream (e.g. diagnostics, provider requests). Not used by the core loop itself
(the loop uses typed Go callbacks).

### 4.9 `compaction`

*Reference source: `dist/core/compaction/*`*

Pure logic (mirror reference):

- `estimateTokens(msg)` — chars/4 heuristic (conservative overestimate).
- `shouldCompact(contextTokens, contextWindow, settings)` — trigger check for
  auto-compaction: `contextTokens + reserveTokens >= contextWindow`.
- `findCutPoint(entries, startIndex, endIndex, keepRecentTokens)` — walk
  backwards from newest accumulating estimates; cut at the point where
  `>= keepRecentTokens` is retained; prefer user-message boundaries; a
  mid-turn cut splits the turn (summarize up to the user message that started
  it).
- `compact(...)` — build a summarization prompt over the entries before the
  cut, run a summarization LLM call (same StreamFn/retry machinery as turns),
  return `CompactionResult{summary, firstKeptEntryId, tokensBefore}`.

Integration (via `Session`): manual `Compact()`, threshold auto-compaction
after each turn (`_checkCompaction` parity), and overflow recovery when the
provider rejects context as too long. The session appends a `compaction` entry,
reloads context (`firstKeptEntryId`), and emits `compaction_start/end`.
Compaction and branch summarization share the retry budget/backoff of turns
(`_summarizationRetryCallbacks` parity).

### 4.10 `exec` — process primitives

*Reference source: `dist/core/exec.ts`, `dist/core/bash-executor.ts`*

- `exec.Command` wrapper with `context` cancellation, timeout, streaming
  stdout/stderr, env injection, working directory.
- Bash tool uses these primitives; `BashOperations` interface kept for future
  remote execution (SSH) — v1 ships the local implementation only
  (`createLocalBashOperations` parity).
- Embedded binaries (ripgrep, git — ADR-006) are extracted to a private
  cache dir on first use and executed through the same primitives.

## 5. Data-flow deep dives

### 5.1 The turn loop in Go

fuji implements the loop as an explicit state machine driven by a single
goroutine (the "loop goroutine"). There is no `select`-based actor swarm; the
loop pulls from explicit queues only at defined drain points:

```
IDLE ──Prompt──▶ PREFLIGHT ──validated──▶ STREAMING ──stop:tool_use──▶ EXECUTING_TOOLS
                                          │  ▲                            │
                                          │  │                            ▼
                                          │  └──stop:done / turn_end───────┐
                                          │                               │
                                          └──stop:error/aborted ──► retry?─► TERMINAL
```

Queues (steering/follow-up) are drained only at loop points 8/9 of §4.2 —
never asynchronously. This is what keeps the process deterministic: message
ordering is total and reproducible.

### 5.2 Tool-call streaming

Sequential execution (fuji's fixed mode):

1. On assistant message stop with tool calls, emit
   `tool_execution_start {toolCallId, toolName, args}` for the first call.
2. `BeforeToolCall` hook → if blocked, synthesize an error tool result.
3. Schema-validate args (`json.RawMessage` → tool param schema).
4. `Execute(callID, params, ctx, onUpdate)`:
   - `onUpdate` streams partial progress → `tool_execution_update`
     events and (for bash) `bash_execution_update {delta}`.
   - For `bash`, output is streamed chunk-wise to the model *and* to
     subscribers; a truncation marker and optional `fullOutputPath` are
     attached when output exceeds limits.
5. On completion, `AfterToolCall` hook may override content/details/isError/
   terminate. Emit `tool_execution_end {result, isError}`.
6. Append `tool_result` message; repeat for the next call in source order.
7. After the batch: if every result set `Terminate`, stop; else next turn.

Determinism note: with sequential mode, tool-result message order equals tool
call source order — a property the parity harness can assert (§10).

### 5.3 Cancellation and timeouts

- One `context.Context` per session (`Session.ctx`), derived per operation:
  - provider request: derived from session ctx + per-turn timeout (config)
  - tool execution: derived from session ctx + per-tool timeout
  - bash: derived from session ctx + bash timeout (`timeout` param or
    config default)
- `Abort()` cancels the session-level context for in-flight work, marks the
  assistant message `stopReason: aborted`, and waits for the loop to reach
  IDLE. Pending steering/follow-up messages are preserved (see `clearQueue`).
- Bash is a child process; abort sends SIGTERM then SIGKILL after grace
  (mirroring the reference's `abortBash`).
- Compaction and branch summarization have their own cancel handles
  (`abortCompaction`, `abortBranchSummary`).

### 5.4 Retry policy

Mirror the reference's `settings.retry`:

- Retryable: overloaded, rate-limited, transient 5xx, mid-stream drops,
  summarization-call failures.
- Not retryable: auth failures, 4xx validation, context-overflow (→
  compaction), user abort.
- Exponential backoff with jitter; events `auto_retry_start`/`auto_retry_end`
  carry attempt/maxAttempts/delayMs; `agent_end.willRetry` communicates
  retry state to the CLI.
- Summarization retries (`summarization_retry_*` events) share the same
  budget so one transient drop does not fail a compaction.

### 5.5 Compaction flow

```
[after each turn] estimateContextTokens(context)
        │
        ▼
 shouldCompact? ──no──▶ continue
        │
       yes
        ▼
 emit compaction_start{reason: threshold|overflow}
 findCutPoint(entries, keepRecentTokens)        (pure)
 build summarization prompt over pre-cut entries (pure)
 summarization LLM call ──(retry budget, abortable)──▶ summary
 SessionManager.AppendCompaction(summary, firstKeptEntryId, tokensBefore)
 reload context from firstKeptEntryId
 emit compaction_end{result, aborted, willRetry}
```

### 5.6 Session persistence flow

```
loop emits message_end ──▶ Session persists message
                              │
                              ▼
                    SessionManager.AppendMessage(msg)
                    → serialize entry → append line to session file
                    → emit entry_appended
                    → advance leaf pointer
```

Write-through per message; file handle held open per session; rewrite only on
migration/branch-with-summary (`_rewriteFile` parity). The session is the
single writer; no other component writes the file.

## 6. Error model

| Class | Handling |
|-------|----------|
| Provider protocol errors | encoded in stream as `error` event + `StopReason:error`; never escape `StreamFn` |
| Retryable failures | retry policy (§5.4) |
| Context overflow | not retried → overflow compaction (§5.5) |
| Tool failures | tool result with `IsError:true` + message; loop continues (model observes the error) |
| Tool panics | recovered at the `Execute` boundary; converted to error result |
| Config/auth errors | fail fast at `NewSession`/`Prompt` preflight with actionable message |
| IO errors on session file | surfaced via `entry_appended`-adjacent error events; session marked degraded, loop continues in-memory (reference parity: session persists best-effort) |

## 7. Threading & determinism

fuji is **single-threaded** (D13):

- One logical execution thread: the loop goroutine. Providers, tools, and
  persistence run *inline* on that goroutine; blocking operations (HTTP,
  process spawn) are awaited sequentially — Go's runtime schedules them, but
  the *logical* execution is a single sequence.
- No goroutine-per-tool, no goroutine-per-message, no background writers.
- Event ordering is total and reproducible: same input + same provider output
  ⇒ same event sequence ⇒ same session file.
- Subscribers may not re-enter the session (documented in §4.1).

**Trade-off accepted:** sequential tool execution means a model emitting
multiple independent tool calls per turn takes longer than the reference's
parallel mode.
Worth it for determinism, debuggability, and fleet-scale cost.

**Embedding note:** fuji is a library; embedding applications may run
multiple `Session`s — one per goroutine — if they need concurrency at the
application level. Each session remains internally single-threaded.

## 8. Security surface

Stripping extensions removes the reference's largest attack surface
(untrusted code
execution, trust prompts). Remaining surface, with controls:

| Surface | Control |
|---------|---------|
| Bash tool | timeout always applied; cwd sandboxed to session cwd; env whitelist; output truncation |
| File tools | path resolution confined to session cwd (symlink-aware); tool allow/deny via config |
| Provider endpoints | custom base URLs must be explicit config; TLS required (no plaintext HTTP) |
| Session files | JSONL parsed with strict schema; oversized/foreign entries ignored (F1) |
| Resources (skills/templates) | data-only: parsed, never executed; sourced from explicit locations |

## 9. Exit codes (CLI contract)

| Code | Meaning |
|------|---------|
| 0 | session completed normally |
| 1 | configuration/auth error, prompt preflight failure |
| 2 | runtime failure after preflight (unrecoverable loop error) |
| 3 | aborted (SIGINT / `Abort`) |

`SIGINT` maps to `Session.Abort()`; a second `SIGINT` forces exit 130.

## 10. Deviation register (reference → fuji)

Every deliberate deviation, so parity work is explicit:

| # | reference | fuji | Reason |
|---|----|------|--------|
| V1 | tool execution mode default `parallel` | `sequential` fixed | single-threaded (§7) |
| V2 | extensions add tools/hooks | no extensions; fixed tool set | D3 |
| V3 | interactive auth (OAuth, credential store) | env/config keys only | D8 |
| V4 | RPC + print modes | one-shot CLI | D7 |
| V5 | `~/.reference/agent` | `~/.fuji` (flag F2) | D11 |
| V6 | image auto-resize in `read` | none in v1 | no image pipeline in core |
| V7 | `custom`/`custom_message` entries, extension tool calls in sessions | ignored on read; never written | F1 |
| V8 | provider SDKs | raw HTTP/SSE | D10 |
| V9 | HTML export, themes, footer, keybindings | absent | TUI-adjacent, out of scope |
| V10 | `AgentSession` surface: `navigateTree`, `getUserMessagesForForking`, `sendCustomMessage`, `cycleModel`, `setSessionName`, `exportToHtml`, `clearQueue`, pending-message getters, `bindExtensions` | absent from `Session` | headless scope — confirmed final (no TUI/extension callers) |

## 11. Reference index (reference source → fuji package)

| reference source (`dist/` or package) | fuji package (`go-tree.md`) |
|--------------------------------|------------------------------|
| `core/agent-session.ts` | `pkg/session` |
| `core/agent-session-runtime.ts`, `-services.ts` | `pkg/session` (wiring) |
| `agent-core/dist/agent-loop.ts` | `pkg/loop` |
| `core/session-manager.ts` | `pkg/sessionmgr` |
| `core/model-runtime.ts`, `core/model-registry.ts`, `core/model-resolver.ts` | `pkg/modelrt` |
| reference provider clients | `pkg/modelrt/transport` (raw HTTP) |
| `core/tools/*` | `pkg/tools` (+ `pkg/tools/{read,write,edit,bash,grep,find,ls,git}`) |
| `core/bash-executor.ts`, `core/exec.ts` | `pkg/exec` |
| `core/compaction/*` | `pkg/compaction` |
| `core/skills.ts` | `pkg/skills` |
| `core/prompt-templates.ts` | `pkg/templates` |
| `core/settings-manager.ts`, `core/config.ts`, `core/defaults.ts` | `pkg/config` |
| `core/event-bus.ts` | `pkg/eventbus` |
| `core/messages.ts` | `pkg/messages` |
| `core/resource-loader.ts` | `pkg/resource` |
| `core/source-info.ts` | `pkg/sourceinfo` |
| `cmd/fuji` | `cmd/fuji` (CLI) |
