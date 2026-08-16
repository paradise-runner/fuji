# fuji — migration phases

**Status**: draft. Build sequence from the pinned 0.84.1 reference.
Each phase has exit criteria; phases are gated by review (marked **GATE**)
before the next phase starts. The parity harness (§10) is introduced early
and grows with the implementation.

## The golden-session parity harness

The backbone of migration correctness. Because model output is
nondeterministic, parity is measured against **recorded provider tapes**:

1. **Record** — a record/replay HTTP server fronts the real provider
   (custom base URL config). Run the reference headless (print mode) over a
   fixed task suite (`test/fixtures/tasks/*.md`) and record the raw SSE
   bodies per task (`test/fixtures/tapes/<task>.jsonl`).
2. **Replay** — run the reference and fuji against the same tape (identical
   HTTP responses ⇒ identical assistant text and tool calls).
3. **Diff** — compare structurally:
   - session file entry sequence (types, ids, parent ids, tool call names,
     args order, tool results, compaction entries)
   - event sequence (tool_execution_* order — fuji sequential ⇒ equals
     source order)
   - exit codes
   Text equality is expected where the tape is identical; where the tape
   cannot guarantee it, compare session *shape* with a small tolerance list.

The harness lives in `test/parity/` (layout in `go-tree.md`). It is run
green from phase 5 onward.

## Phase 0 — Foundations

**Goal**: module, build, and the primitives everything depends on.

- `go mod init fuji`; CI (static build `CGO_ENABLED=0`, `go vet`, `gofmt`).
- `pkg/config`: settings struct, precedence merge (flags > env > project file
  > user file > defaults), path resolution (`~/.fuji`, project `.fuji`).
- `internal/jsonl`: strict JSONL reader/writer.
- `internal/sse`: SSE event reader/writer.
- `pkg/exec`: context-aware process execution (timeout, streaming, env/cwd).
- `pkg/messages`: content blocks, `AgentMessage` model (the reference
  `dist/core/messages.ts`, and the base reference message types).
- `pkg/schema`: JSON-Schema helpers for tool parameters.

**Exit criteria**:
- Static binary builds with `CGO_ENABLED=0`.
- Unit tests: precedence merge table, JSONL round-trip, SSE parse of a
  recorded Anthropic response, exec timeout/kill behavior.

## Phase 1 — Session store

**Goal**: verbatim JSONL v3 persistence, no agent yet.

- `pkg/sessionmgr`: header, entry types, append/leaf/branch/reset, context
  building (`buildSessionContext`), migration v1/v2→v3, `--<cwd>--` directory
  encoding, `list`/`open`/`continueRecent`/`inMemory`.
- Ignore-on-read policy for foreign entries (F1).

**Exit criteria**:
- Golden session files from the 0.84.1 reference load without loss on the
  supported subset; fuji-written files load in the reference.
- Round-trip property test: append N messages, branch, reload ⇒ identical
  tree and context.

## Phase 2 — Model runtime and transports

**Goal**: `StreamFn` contract with raw HTTP.

- `pkg/modelrt`: registry, model catalog, auth resolution (env/config),
  custom base URLs, thinking-level clamping.
- `internal/providers` + `pkg/modelrt/transport`: Anthropic, OpenAI,
  OpenAI-compatible transports; SSE event mapping; stream failure encoding
  (never throw).

**Exit criteria**:
- Tape harness: recorded provider responses replay through fuji's transports
  producing the expected event sequence (`text_delta`…`stop`).
- Error paths: auth failure, mid-stream drop, timeout — each yields an
  `error`/`aborted` event + `StopReason:error` message, no escape error.

## Phase 3 — Agent loop

**Goal**: the single-threaded state machine.

- `pkg/loop`: `runLoop`/`runLoopContinue` (core-spec §4.2), event emission,
  queue drain points (steering/follow-up), `beforeToolCall`/`afterToolCall`
  hooks, sequential tool execution, `ShouldStopAfterTurn`/`PrepareNextTurn`.

**Exit criteria**:
- Loop tests with a fake `StreamFn`: single-turn, multi-turn tool loops,
  steering/follow-up ordering, abort mid-stream, blocked-tool path.
- Determinism test: same tape + same input ⇒ identical event sequence.

## Phase 4 — Tools

**Goal**: the full bundled tool set.

- `pkg/tools` + per-tool packages: read, write, edit (unique-match/diff
  semantics), bash (streaming, truncation, `fullOutputPath`), grep (embedded
  rg), find, ls, git (embedded git binary).
- `internal/truncate`, `internal/editdiff`, `internal/mutqueue`.
- embedded-binary bootstrap: extract-from-embed → private cache dir → exec
  (with integrity check and per-release pinning).

**GATE M4**: review the embedded-binary mechanism (rg + git) operationally —
extraction, cache lifecycle, per-platform release matrix, git binary size,
licensing (git is GPL-2.0; see ADR-006). Confirm the minimal git command set.

**Exit criteria**:
- Tool behavior tests mirroring the reference tool semantics (truncation
  markers, edit no-match errors, bash timeout, output spill-over).
- `fuji run --prompt "list files"` produces correct `ls`/`find` tool calls
  against a tape.

## Phase 5 — Session orchestration

**Goal**: `Session` wires loop + store + runtime + tools together.

- `pkg/session`: `New`, `Prompt`/`Steer`/`FollowUp`/`Abort`/`WaitForIdle`/
  `Compact`/model+thinking/bash execution; event surface; persistence on
  `message_end`; retry policy (`auto_retry_start/end`); preflight validation.
- `cmd/fuji`: `run` subcommand, flag/env wiring, SIGINT → `Abort`, exit codes.

**Exit criteria**:
- First end-to-end run: `fuji run --prompt …` against a tape completes and
  writes a loadable session file.
- **Parity harness green** on the base task suite (structure diff clean).

## Phase 6 — Compaction

**Goal**: long-session memory management.

- `pkg/compaction`: `EstimateTokens`, `ShouldCompact`, `FindCutPoint`,
  summarization prompt, summarization driver with shared retry budget;
  integration in `session` (manual, threshold, overflow); branch summaries.

**Exit criteria**:
- Cut-point tests match the `findCutPoint` behavior on recorded sessions.
- Parity: a long task (multi-compaction) diffs structurally against the
  reference fixture (compaction entry placement, `firstKeptEntryId`, context
  rebuild).

## Phase 7 — Resources

**Goal**: skills, prompt templates, `.fuji` discovery, `--skills`.

- `pkg/skills`, `pkg/templates`, `pkg/resource`: discovery order (flag >
  project > user), skill format parsing, `FormatSkillsForPrompt`, template
  expansion, `/skill:name` expansion in `Prompt`/`Steer`/`FollowUp`.

**Exit criteria**:
- A skill placed in `--skills <dir>` changes the system prompt exactly as
  in the reference (snapshot diff against the reference output on the same
  tape).

## Phase 8 — CLI and fleet hardening

**Goal**: production-ready one-shot operation.

- Exit-code contract (§9 of core-spec), SIGINT/SIGTERM behavior.
- Timeout budgets everywhere (turn, tool, bash), tool allow/deny config.
- Structured logging (JSON lines to stderr) for fleet observability;
  `--log-level`.
- Static release build; embed rg + git binaries (ADR-006); license/attribution
  pass (git GPL-2.0).

**Exit criteria**:
- `fuji run` under `timeout`, kill, and disk-full scenarios exits per contract
  without corruption of the session file.
- Binary runs on a bare container with no tooling installed (git/rg present
  only as embedded assets).

## Phase 9 — Parity freeze and soak

**Goal**: prove behavioral parity at scale.

- Extend task suite: multi-file edits, bash-heavy tasks, git workflows,
  long sessions with compaction, error-injection tasks.
- Soak: repeat each task N times against tapes; zero structural drift.

**Exit criteria**:
- Full suite green; deviation register (core-spec §10) is the only allowed
  difference; document residual tolerances.

## Phase 10 — Docs freeze and handoff

**Goal**: the spec matches the implementation.

- Reconcile `core-spec.md`, `go-tree.md`, ADRs, and `phases.md` with the
  shipped code; flag any deviation as a new ADR or V-entry.
- Write the embedding guide (library usage, multi-session patterns).

**Exit criteria**: a reviewer can build fuji from the docs alone and the docs
describe the shipped implementation.
