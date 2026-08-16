# Third-party notices

fuji embeds and/or depends on the following third-party components (ADR-006).
This file is shipped with release binaries per each component's license terms.

## ripgrep — MIT

- Project: https://github.com/BurntSushi/ripgrep
- License: MIT (dual MIT/Unlicense)
- Version pinned per release in `internal/embedbin` (`embedbin.Rg`).
- Distribution: embedded as a distinct binary component, extracted to the
  fuji private cache on first use. The upstream LICENSE file is reproduced
  below in abbreviated form; the full text is available from the project.

MIT License

Copyright (c) 2015 Andrew Gallant

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

## git — GPL-2.0

- Project: https://git-scm.com/
- License: GPL-2.0-only (see https://git.kernel.org/pub/scm/git/git/plain/COPYING)
- Version pinned per release in `internal/embedbin` (`embedbin.Git`).
- Distribution: embedded as a distinct, attributed component. GPL-2.0
  obligations (offering source) are documented in `docs/gate-m4.md` and the
  release process; the full COPYING text ships alongside this file in release
  artifacts as `third_party/git-COPYING`.
- fuji's own code is licensed separately and does not incorporate git code;
  the embedded git is a separate executable invoked as a subprocess.

## Reference: behavioral reference implementation

- Project: https://github.com/earendil-works/pi
- Pin: 0.84.1 (behavioral reference for session format and tool semantics;
  no reference code is copied into fuji).
