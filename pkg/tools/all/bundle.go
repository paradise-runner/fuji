package all

import (
	"time"

	"fuji/internal/embedbin"
	"fuji/internal/mutqueue"
	"fuji/pkg/tools"
	"fuji/pkg/tools/bash"
	"fuji/pkg/tools/edit"
	"fuji/pkg/tools/find"
	"fuji/pkg/tools/git"
	"fuji/pkg/tools/grep"
	"fuji/pkg/tools/ls"
	"fuji/pkg/tools/read"
	"fuji/pkg/tools/write"
)

// Resolvers locates the embedded binary backends.
type Resolvers struct {
	RgPath  func() (string, error)
	GitPath func() (string, error)
}

// BuildResolvers wires the embedded-binary bootstrap (extract-to-cache) with
// PATH fallback for dev builds (ADR-006).
func BuildResolvers(agentDir string) Resolvers {
	cacheDir := embedbin.CacheDir(agentDir)
	return Resolvers{
		RgPath:  func() (string, error) { return embedbin.Rg.Ensure(cacheDir) },
		GitPath: func() (string, error) { return embedbin.Git.Ensure(cacheDir) },
	}
}

// BundledNames is the fixed tool contract (D6).
var BundledNames = []string{"read", "write", "edit", "bash", "grep", "find", "ls", "git"}

// RegisterAll registers the full bundled tool set into reg. The mutation
// queue is shared per session so file edits serialize.
func RegisterAll(reg *tools.Registry, cwd string, res Resolvers, defaultBashTimeout time.Duration) *mutqueue.Queue {
	queue := mutqueue.New()
	reg.Register(read.New(cwd))
	reg.Register(write.New(cwd))
	reg.Register(edit.New(cwd, queue))
	reg.Register(bash.New(cwd, defaultBashTimeout))
	reg.Register(grep.New(cwd, res.RgPath))
	reg.Register(find.New(cwd))
	reg.Register(ls.New(cwd))
	reg.Register(git.New(cwd, res.GitPath))
	return queue
}
