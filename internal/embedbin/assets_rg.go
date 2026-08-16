//go:build fuji_embed_rg

package embedbin

import _ "embed"

// Release build: the pinned ripgrep static binary is embedded here. The
// release process copies the cross-compiled rg binary to assets/rg and
// records its SHA-256 in PinnedSHA256 (see docs/gate-m4.md).

//go:embed assets/rg
var rgBlob []byte

// Rg is the ripgrep asset (grep tool backend).
var Rg = Asset{
	Name:         "rg",
	Version:      "14.1.1",
	PinnedSHA256: "<set-by-release-process>",
	Blob:         rgBlob,
	Available:    true,
}

// Git keeps the placeholder build in the release build unless git is embedded.
var gitBlobPlaceholder []byte

//go:embed assets/PLACEHOLDER
var gitBlob []byte

// Git is the git asset (git tool backend).
var Git = Asset{
	Name:      "git",
	Version:   "2.45.2",
	Blob:      gitBlob,
	Available: false,
}
