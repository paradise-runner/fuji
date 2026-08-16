// Package git implements the git tool — a fuji addition (core-spec §4.5, D6):
// a passthrough to the embedded (or host) git binary for the all-in-one
// workflow: status/diff/log/commit/… Arguments are passed directly to git's
// execve, never through a shell.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"fuji/internal/truncate"
	"fuji/pkg/exec"
	"fuji/pkg/schema"
	"fuji/pkg/tools"
)

// Params is the git subcommand invocation.
type Params struct {
	Args []string `json:"args"`
}

// Resolver locates the git binary (embedded or PATH).
type Resolver func() (string, error)

// New builds the git tool Definition.
func New(cwd string, resolve Resolver) tools.Definition {
	params, _ := schema.Object([]string{"args"}, map[string]schema.Schema{
		"args": schema.Arr("Git subcommand arguments, e.g. [\"status\", \"--short\"] or [\"diff\"]. Do not pass shell operators or pipes — run those through bash instead.", schema.Str("")),
	}).Raw()
	return tools.Definition{
		Name: "git", Label: "git",
		Description:   "Run a git command (status, diff, log, commit, add, branch, stash, show, etc.) in the working directory. Output is tail-truncated. Use bash for compound commands with pipes/redirects.",
		PromptSnippet: "Run git commands for the all-in-one workflow",
		Parameters:    params,
		Execute: func(callID string, raw json.RawMessage, ctx context.Context, onUpdate func(tools.Update)) (tools.Result, error) {
			var p Params
			if err := json.Unmarshal(raw, &p); err != nil {
				return tools.ErrorResult(err), nil
			}
			if len(p.Args) == 0 {
				return tools.ErrorResult(fmt.Errorf("git: args must contain at least one subcommand")), nil
			}
			return execute(ctx, cwd, p, resolve)
		},
	}
}

func execute(ctx context.Context, cwd string, p Params, resolve Resolver) (tools.Result, error) {
	gitPath, err := resolve()
	if err != nil || gitPath == "" {
		return tools.ErrorResult(fmt.Errorf("git is not available (embedded binary not built)")), nil
	}
	// Defense-in-depth: reject shell metacharacters in args (they are passed
	// directly to execve, so this only catches accidental injection). Spaces
	// are allowed (e.g. -m "commit message").
	for _, a := range p.Args {
		if strings.ContainsAny(a, ";&|`$<>{}()[]!\\\"'") {
			return tools.ErrorResult(fmt.Errorf("git: argument %q contains shell metacharacters; run compound commands through bash instead", a)), nil
		}
	}
	cmd := exec.New(gitPath, p.Args...).WithDir(cwd)
	// Make -p pagination impossible in non-interactive use.
	cmd.WithEnv("GIT_PAGER=cat", "PAGER=cat", "GIT_TERMINAL_PROMPT=0")
	res, runErr := cmd.Run(ctx)
	if runErr != nil && res.ExitCode == 0 {
		// spawn failure
		return tools.ErrorResult(fmt.Errorf("failed to run git: %v", runErr)), nil
	}
	tr := truncate.TruncateTail(res.Stdout, truncate.Options{})
	output := tr.Content
	if tr.Truncated {
		output += fmt.Sprintf("\n\n[Showing last %d of %d lines (%s). Use git commands with narrower output (e.g. --max-count, --stat) for more.]",
			tr.OutputLines, tr.TotalLines, truncate.FormatSize(truncate.DefaultMaxBytes))
	}
	if res.ExitCode != 0 {
		stderr := strings.TrimSpace(res.Stderr)
		msg := stderr
		if msg == "" {
			msg = fmt.Sprintf("git exited with code %d", res.ExitCode)
		}
		if output != "" {
			msg = output + "\n\n" + msg
		}
		return tools.ErrorResult(fmt.Errorf("%s", msg)), nil
	}
	if strings.TrimSpace(res.Stderr) != "" {
		output = strings.TrimSpace(output) + "\n" + strings.TrimSpace(res.Stderr)
	}
	return tools.TextResult(output), nil
}
