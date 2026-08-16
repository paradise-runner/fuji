//go:build fuji_embed_git

package embedbin

import _ "embed"

// Release build: the pinned minimal static git binary is embedded here
// (GPL-2.0 — see ADR-006 and docs/gate-m4.md for distribution notes).

//go:embed assets/git
var gitBlob []byte

// Git is the git asset (git tool backend).
var Git = Asset{
	Name:         "git",
	Version:      "2.45.2",
	PinnedSHA256: "<set-by-release-process>",
	Blob:         gitBlob,
	Available:    true,
}
