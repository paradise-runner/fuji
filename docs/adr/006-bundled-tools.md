# ADR-006 — bundled tools: embed the real binaries

**Status**: Accepted (decision D6); mechanism per this ADR.

## Context

The bundled tool set (read, write, edit, bash, grep, find, ls, git) must work
on hosts where nothing is installed — "you shouldn't need to worry whether a
tool is available." The question was *how* to bundle: pure-Go
reimplementations, embedded real binaries, or a mix.

## Decision

**Embed the real binaries for both external-tool cases, executed as
subprocesses:**

- **Search (`grep`/`find` backing)**: **embed ripgrep's static binary** via
  `//go:embed`. Ripgrep's regex semantics, speed, and flags are the behavior
  the reference agent exposes (`dist/core/tools/grep.ts` shells out to `rg`); no pure-Go
  scanner matches that parity.
- **Git (`git`)**: **embed a static git binary** (cross-compiled, minimal
  feature build), executed as a subprocess. Real git behavior (plumbing,
  worktrees, submodules, hooks) rather than a library approximation; the
  "all-in-one" agent should behave like the git everyone knows.
- **Bash (`bash`)**: the host shell is used **by design** — the tool's
  purpose is running commands on the host, so it is not bundled. Timeouts,
  cwd confinement, and output truncation (core-spec §5.3, §8) mitigate the
  host surface.

Consistency rationale: a single mechanism (embedded binaries + subprocess)
for everything that would otherwise be a host dependency keeps behavior
parity highest and the mental model uniform — no per-tool library quirks to
learn or test.

## Consequences

- **Positive**: single static binary artifact per target, deterministic
  behavior, no host dependencies, behavior parity with the reference (which shells out to the same tools).
- **Negative**: release matrix grows — both ripgrep and git must be
  cross-compiled per OS/arch (darwin/linux/windows × amd64/arm64) and pinned
  per release; binary size grows (git is large; consider a minimal static
  build with only the needed command set).
- **Negative**: **licensing** — ripgrep is MIT, but git is GPL-2.0. Embedding
  git's binary into a distributed combined binary raises GPL distribution
  considerations; must be reviewed with legal and documented in the release
  process (e.g. keeping the embedded git as a distinct attributed component).
- **Negative**: `go-git` (pure Go) was rejected, so no pure-Go git fallback
  exists if embedded-git proves operationally awkward — revisit at gate M4 if
  needed.

## Alternatives

- **`go-git` for git** — pure Go, no license issue, but behavior drift vs the
  real CLI (submodules, exotic worktrees, hooks); rejected per "bundle the
  whole thing".
- **Pure-Go grep** — no ripgrep parity; rejected.
- **Shell out to system tools** — breaks the "don't worry if a tool is
  available" requirement; rejected.
