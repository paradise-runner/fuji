// Command fuji is the one-shot CLI for the fuji agent core (core-spec §3.1,
// D7): `fuji run --prompt <text|@file> [flags…]`. Exit codes follow the
// contract in core-spec §9.
package main

import (
	"fmt"
	"os"
)

// exit codes (core-spec §9).
const (
	ExitOK         = 0
	ExitConfigAuth = 1
	ExitRuntime    = 2
	ExitAborted    = 3
	ExitSigint     = 130
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return ExitConfigAuth
	}
	switch args[0] {
	case "run":
		return runCmd(args[1:])
	case "--version", "-v", "version":
		fmt.Println(versionString())
		return ExitOK
	case "--help", "-h", "help":
		printUsage(os.Stdout)
		return ExitOK
	default:
		fmt.Fprintf(os.Stderr, "fuji: unknown command %q\n\n", args[0])
		printUsage(os.Stderr)
		return ExitConfigAuth
	}
}

func printUsage(w *os.File) {
	fmt.Fprint(w, `fuji — headless coding agent core in Go

Usage:
  fuji run --prompt <text|@file> [flags…]   Run one agent task
  fuji version                               Print version
  fuji help                                  Show this help

Run flags:
  --prompt <text|@file>   Prompt text, or @path to a file containing it (required)
  --cwd <dir>             Working directory (default: current directory)
  --session <path>        Resume an existing session file
  --skills <dir>          Extra skills directory (P7)
  --model <id>            Model id (default: provider default)
  --provider <id>         Provider (anthropic, openai, or a custom id)
  --base-url <url>        Custom provider base URL
  --thinking <level>      Thinking level: off|minimal|low|medium|high|xhigh|max
  --timeout <secs>        Per-turn timeout in seconds
  --tools <list>          Tool allowlist (comma-separated)
  --no-tools              Run with no tools
  --app-url <url>         App attribution URL (OpenRouter HTTP-Referer);
                          auto-set when --base-url is OpenRouter
  --app-title <name>      App display name (OpenRouter X-OpenRouter-Title);
                          auto-set to "fuji" when --base-url is OpenRouter
  --app-categories <list> App categories (OpenRouter X-OpenRouter-Categories)
  --log-level <level>     debug|info|warn|error (JSON lines on stderr)
  --version               Print version

Environment:
  ANTHROPIC_API_KEY / OPENAI_API_KEY / <PROVIDER>_API_KEY   Provider keys
  FUJI_* variables mirror the flags (see pkg/config)

Exit codes: 0 ok, 1 config/auth error, 2 runtime failure, 3 aborted (SIGINT).
`)
}
