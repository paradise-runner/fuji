# fuji

`fuji` is a pure, naked core for agentic work at scale. Written in Go, it delivers an embeddable, headless agent runtime with bundled tools for a guaranteed agentic experience across fleet deployments.

---

## Key Features

- **Pure Naked Core**: Lightweight, single-threaded execution engine without heavy framework dependencies, dynamic plugin runtimes, or interactive TUI overhead.
- **Guaranteed Bundled Tools**: Standardized, deterministic tool implementations (`read`, `write`, `edit`, `bash`, `grep`, `find`, `ls`, `git`) with embedded tool support to eliminate host environment drift.
- **Embeddable & Headless**: Usable as a Go library or as a one-shot CLI designed for automation, batch pipelines, and fleet orchestrators.
- **Provider Agnostic**: Direct HTTP/SSE streaming integrations for Anthropic and OpenAI-compatible providers with custom base URL support.
- **Session Continuity**: Full JSONL v3 session compatibility (compatible with standard session logs) supporting branching, compaction, and resumes.

---

## Quick Start

### 1. Installation

Build the static binary:

```bash
go build -o fuji ./cmd/fuji
```

### 2. Set Up Credentials

Configure your model provider API key via environment variables:

```bash
# Anthropic
export ANTHROPIC_API_KEY="your-anthropic-key"

# Or OpenAI
export OPENAI_API_KEY="your-openai-key"
```

### 3. Run a Task

Run `fuji` in one-shot mode:

```bash
# Direct prompt string
fuji run --prompt "Fix failing tests in pkg/session"

# Prompt from a file
fuji run --prompt @task.md --cwd /path/to/repo --model claude-sonnet-4-5
```

---

## CLI Reference

```
Usage:
  fuji run --prompt <text|@file> [flags...]   Run one agent task
  fuji version                                Print version
  fuji help                                   Show help

Flags:
  --prompt <text|@file>   Prompt text, or @path to a prompt file (required)
  --cwd <dir>             Working directory (default: current directory)
  --session <path>        Resume an existing session file
  --skills <dir>          Path to custom skills directory (.fuji/skills)
  --model <id>            Model identifier (e.g. claude-3-7-sonnet-20250219, gpt-4o)
  --provider <id>         Provider: anthropic (default) or openai
  --base-url <url>        Custom provider API base URL
  --thinking <level>      Thinking level: off | minimal | low | medium | high | xhigh | max
  --timeout <secs>        Per-turn timeout in seconds
  --app-url <url>         App attribution URL (OpenRouter HTTP-Referer, required for rankings);
                          auto-set when --base-url is OpenRouter
  --app-title <name>      App display name (OpenRouter X-OpenRouter-Title);
                          auto-set to "fuji" when --base-url is OpenRouter
  --app-categories <list> App marketplace categories, comma-separated (OpenRouter X-OpenRouter-Categories)
  --tools <list>          Comma-separated allowlist of tools
  --no-tools              Disable all tools
  --log-level <level>     Log level: debug | info | warn | error (emits JSONL to stderr)
```

### Exit Codes

- `0`: Success (task completed normally)
- `1`: Configuration or authentication error
- `2`: Runtime failure
- `3`: Aborted (SIGINT / cancellation)

---

## Bundled Tools

`fuji` guarantees the following standard tools across all environments:

| Tool | Description |
|------|-------------|
| `read` | Read file contents (text or images) with offset/limit pagination |
| `write` | Create or overwrite files (auto-creates directories) |
| `edit` | Atomic search-and-replace edits with mutation serialization |
| `bash` | Execute shell commands with configurable timeouts and streaming output |
| `grep` | Fast regex and literal file search (powered by ripgrep) |
| `find` | Locate files matching glob patterns |
| `ls` | Directory listing with file metadata |
| `git` | Execute Git operations |

---

## Go Library Usage

Embed `fuji` directly into your Go services:

```go
package main

import (
	"context"
	"log"

	"fuji/pkg/config"
	"fuji/pkg/session"
)

func main() {
	cfg := config.Config{
		Cwd:      ".",
		Provider: "anthropic",
		Model:    "claude-3-7-sonnet-20250219",
		ApiKey:   "your-api-key",
	}

	sess, err := session.New(cfg)
	if err != nil {
		log.Fatalf("failed to create session: %v", err)
	}
	defer sess.Close()

	if err := sess.Prompt(context.Background(), "Analyze this repo and summarize it"); err != nil {
		log.Fatalf("agent loop error: %v", err)
	}
}
```

---

## Configuration Hierarchy

`fuji` merges configuration in order of increasing precedence:

1. Defaults
2. User config (`~/.fuji/config.json`)
3. Project config (`<cwd>/.fuji/config.json`)
4. Environment variables (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `FUJI_*`)
5. CLI flags (`--model`, `--provider`, etc.)

---

## Documentation

For architectural deep-dives, specs, and decision records, explore the [`docs/`](docs/) directory:

- [`docs/core-spec.md`](docs/core-spec.md) — Architecture, module contracts, and data flow
- [`docs/decisions.md`](docs/decisions.md) — Core decisions and trade-offs
- [`docs/adr/`](docs/adr/) — Architecture Decision Records
# fuji
