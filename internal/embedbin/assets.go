package embedbin

import _ "embed"

// Placeholder build (default): real binaries are not embedded; tools fall
// back to the host PATH. The release process builds with the fuji_embed_rg /
// fuji_embed_git tags after dropping the pinned static binaries into
// assets/ (see docs/gate-m4.md).

//go:embed assets/PLACEHOLDER
var rgBlob []byte

//go:embed assets/PLACEHOLDER
var gitBlob []byte

// Rg is the ripgrep asset (grep tool backend).
var Rg = Asset{
	Name:      "rg",
	Version:   "14.1.1",
	Blob:      rgBlob,
	Available: false,
}

// Git is the git asset (git tool backend).
var Git = Asset{
	Name:      "git",
	Version:   "2.45.2",
	Blob:      gitBlob,
	Available: false,
}
