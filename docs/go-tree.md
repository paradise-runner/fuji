# fuji — proposed Go tree

**Status**: draft. Mirrors the module contracts in
[`core-spec.md`](core-spec.md); package names are normative, file layout
suggestive.

## Top-level layout

```
fuji/
├── cmd/
│   └── fuji/
│       ├── main.go          # flag/env parsing, SIGINT handling, exit codes (§9 of core-spec)
│       ├── run.go           # "fuji run" one-shot orchestration
│       └── version.go
├── pkg/
│   ├── session/             # Session lifecycle, event surface, orchestration
│   ├── loop/                # single-threaded agent loop state machine
│   ├── modelrt/             # ModelRuntime: registry, auth resolution, streaming
│   │   └── transport/       # raw HTTP/SSE per provider family
│   ├── sessionmgr/          # JSONL v3 session store
│   ├── tools/               # Definition contract + registry
│   │   ├── read/  write/  edit/  bash/  grep/  find/  ls/  git/
│   ├── exec/                # process primitives (context, timeout, streaming)
│   ├── compaction/          # pure compaction logic + summarization driver
│   ├── skills/              # skill discovery + system-prompt formatting
│   ├── templates/           # file-based prompt templates
│   ├── config/              # settings model, precedence merge, paths
│   ├── resource/            # resource discovery (.fuji dirs, --skills)
│   ├── messages/            # content blocks, AgentMessage model
│   ├── eventbus/            # pub/sub bus
│   ├── sourceinfo/          # tool source metadata
│   └── schema/              # JSON-Schema helpers for tool parameters
└── internal/
    ├── providers/           # vendor-agnostic SSE event grammar (shared)
    ├── sse/                 # SSE reader/writer primitives
    ├── truncate/            # output truncation (head/tail/line)
    ├── editdiff/            # unified-diff/unique-match edit semantics
    ├── mutqueue/            # file mutation queue
    └── jsonl/               # strict JSONL line reader/writer
```

## Module responsibilities

### `cmd/fuji`

Thin CLI over the library. Owns: flag parsing (`--prompt`, `--cwd`,
`--session`, `--skills`, `--model`, `--provider`, `--base-url`, `--timeout`,
`--tools`, `--no-tools`, `--version`), env reading, signal handling,
subscribing to the session for minimal stdout progress (stderr), exit-code
mapping (§9 of core-spec). Contains no agent logic.

### `pkg/session`

- `session.Config`, `session.New(cfg)`
- `Session` state machine: IDLE/STREAMING/EXECUTING/COMPACTING/RETRYING
- event dispatch (`Subscribe`), `Prompt`/`Steer`/`FollowUp`/`Abort`/
  `WaitForIdle`/`Compact`/model+thinking management/`ExecuteBash`
- wires: `loop.LoopConfig` construction (hooks → session behavior),
  persistence on `message_end`, compaction triggers, retry callbacks

### `pkg/loop`

- the state machine of core-spec §4.2: `runLoop(prompts, ctx, cfg, emit)`
- event emission, queue drain points, `beforeToolCall`/`afterToolCall` hooks
- no I/O knowledge; everything arrives via `LoopConfig` callbacks

### `pkg/modelrt`

- `Runtime`: provider registry, model catalog, auth resolution
- `Runtime.Stream/Complete` implementing the `StreamFn` contract
- `transport/`: `anthropic.go`, `openai.go`, `openai-compatible.go`
  (custom base URLs) — each maps provider SSE events to shared events
  (`text_delta`, `reasoning_delta`, `tool_call`, `usage`, `stop`, …)

### `pkg/sessionmgr`

- `Manager` with verbatim JSONL v3 semantics (tree/leaf/branch,
  context building, migration, `--<cwd>--` directory encoding)
- `internal/jsonl` for strict parsing; oversized/unknown entries ignored (F1)

### `pkg/tools`

- `Definition`, `Result`, registry (`tools.Registry`)
- `tools/read|write|edit|bash|grep|find|ls|git`: one package per tool, each
  exposing `New(cwd string, opts) Definition`
- shared: schema validation (`pkg/schema`), truncation (`internal/truncate`),
  edit semantics (`internal/editdiff`), mutation queue (`internal/mutqueue`)

### `pkg/exec`

- `exec.Cmd` wrapper: context cancellation, timeouts, streaming output,
  env/cwd control; used by `bash` and the embedded-binary subprocesses
  (`grep`, `git` — ADR-006)

### `pkg/compaction`

- pure functions: `EstimateTokens`, `ShouldCompact`, `FindCutPoint`,
  summarization prompt builder
- driver: summarization LLM call via `modelrt` with shared retry budget;
  used by `session`

### `pkg/skills` / `pkg/templates`

- discovery from `--skills` flag, project `.fuji/`, user `~/.fuji/`
- parsing (frontmatter), `FormatSkillsForPrompt`, template expansion

### `pkg/config`

- typed settings struct, precedence merge (flags > env > project file >
  user file > defaults), path resolution, env-var map for provider keys

### `pkg/resource`

- mirrors the *subset* of the reference `ResourceLoader` that survives D3:
  skills, prompt templates, context files. No extension discovery, no
  packages.

### `pkg/messages`

- `AgentMessage`, content blocks (text/image), `ToolResultMessage`,
  `BashExecutionMessage` — the message model shared by loop, sessionmgr,
  and providers

### `pkg/eventbus`

- `Bus`/`Controller` per core-spec §4.8

### `pkg/sourceinfo`

- source metadata attached to tool definitions (label, docs) — minimal,
  mirrors `core/source-info.ts`

## Dependency rules

- **`cmd/fuji` → `pkg/*` only.** No business logic in `cmd`.
- **`pkg/session` is the only orchestrator.** It depends on `loop`,
  `modelrt`, `sessionmgr`, `tools`, `skills`, `templates`, `config`,
  `compaction`, `messages`.
- **`pkg/loop` depends only on** `pkg/modelrt` (types), `pkg/messages`,
  `pkg/tools` (Definition), `pkg/config` (timeouts). No `sessionmgr`, no
  `skills`.
- **`pkg/sessionmgr`, `pkg/compaction`, `pkg/skills`, `pkg/templates`,
  `pkg/config`, `pkg/eventbus`, `pkg/messages`** are leaves: no internal
  dependencies beyond `pkg/schema`/`internal/*`.
- **`pkg/modelrt` depends on** `internal/sse`, `pkg/messages` (message types),
  `pkg/config` (timeouts/keys). `transport/` never talks to `session`.
- **`internal/*`** are package-private utilities; no `pkg/` may import
  `internal/` of another module — they are within the same module, so the Go
  `internal/` rule applies at module root (fine: single module).

## Test layout (parity harness)

```
test/
├── fixtures/            # golden sessions + transcripts
├── parity/              # structural diff runner (core-spec §10; phases.md)
│   ├── run-reference/   # scripted reference headless runs producing fixtures
│   └── run-fuji/        # fuji runs over the same task suite
└── integration/         # provider mocks (recorded SSE), end-to-end CLI tests
```

Unit tests live next to packages (`pkg/loop/loop_test.go`, …). The parity
harness is described operationally in [`phases.md`](phases.md).

## Build

- Single module (`module fuji`), Go ≥ 1.22.
- Static binary: `CGO_ENABLED=0 go build ./cmd/fuji`.
- Embedded assets: skills/templates defaults plus the ripgrep and git
  static binaries (ADR-006) via `//go:embed`, extracted to a private cache
  dir at first use.
- Zero runtime deps beyond stdlib + embedded binaries (rg, git).
