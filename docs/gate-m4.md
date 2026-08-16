# GATE M4 — embedded-binary mechanism review (rg + git)

**Phase**: 4 (Tools) — gate before Phase 5.
**Status**: for review. Decision ADR-006; mechanism implemented in
`internal/embedbin`; this document is the operational review.

## Mechanism

- Static binaries are embedded via `//go:embed` under
  `internal/embedbin/assets/` and extracted on first use to a **private cache
  dir** (`{agentDir}/bin`, default `~/.fuji/bin`) — never to a shared/system
  location.
- **Pinning**: each asset declares `{name, version, PinnedSHA256}`. The
  embedded blob must match its pinned SHA-256 before extraction; the extracted
  file is re-verified before being reused. A mismatched pin refuses to run
  (fail-loud, never a silent downgrade).
- **Build tags**: `fuji_embed_rg` / `fuji_embed_git` embed the real binaries;
  the default dev build embeds a placeholder and tools fall back to the host
  `PATH` (`exec.LookPath`), so the repository builds and tests without the
  binaries present.
- **Atomic install**: write `name.tmp` → verify → rename, so a partial
  extraction can never be executed.

## Cache lifecycle

| Aspect | Behavior |
|--------|----------|
| Location | `~/.fuji/bin/` (private per user; no shared dirs) |
| Naming | `rg-14.1.1`, `git-2.45.2` (versioned → upgrade = new file, old pruned by the OS/user) |
| First use | extract + verify + exec on the loop goroutine (sequential; no races) |
| Reuse | cached file re-hashed against the pin; mismatch → re-extract |
| Cleanup | none automatic (versioned names keep old copies; users may delete freely — next run re-extracts) |
| Corruption | hash check catches it; re-extract is transparent |

## Per-platform release matrix

Each release must produce, per target (darwin/linux/windows × amd64/arm64):

1. **ripgrep** (`rg`) — upstream static builds exist per platform; copy to
   `internal/embedbin/assets/rg`, record SHA-256, build with
   `-tags fuji_embed_rg`. Size ≈ 5–8 MB per binary.
2. **git** — a **minimal static build** is required (no docs, no contrib,
   `NO_PERL=1`, `NO_TCLTK=1`, `NO_GETTEXT=1`, `NO_OPENSSL=1` for Linux) to keep
   the payload reasonable; even minimal, git is **30–60 MB** per binary.
   Windows git must be cross-compiled with the MSYS2 toolchain.

Release CI embeds per-target binaries and runs the full test suite with the
tags before shipping. The version pin and SHA-256 for each asset are recorded
in the release notes.

## Licensing

- **ripgrep (MIT)** — no distribution constraints beyond the license text
  (included in `third_party/`).
- **git (GPL-2.0)** — embedding git's binary into a combined distributed
  binary raises GPL distribution obligations (source availability). Mitigations
  per ADR-006:
  - the embedded git is a distinct, attributable component (version recorded,
    license text shipped in `third_party/`);
  - the release process documents the component boundary;
  - if legal review flags the combined-distribution form, the fallback is to
    *download* the pinned git at first use (rejected in ADR-006 as a host
    dependency, but the mechanism is a one-line change in `embedbin.Ensure`).

## Confirmation requested

1. Embedded rg + git mechanism (extract → cache → verify → exec) is sound.
2. Minimal git command set is confirmed: passthrough of `git <args>` covering
   status, diff, log, commit, add, branch, stash, show, checkout — no
   porcelain wrapper in v1.
3. GPL-2.0 distribution approach (distinct attributed component) is accepted
   or legal review is scheduled.
