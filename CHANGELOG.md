# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-08-17

### Added

- Initial release.
- Release workflow (`.github/workflows/release.yml`) that builds `fuji` binaries
  for multiple platforms and publishes a GitHub Release when a version tag
  (e.g. `v0.1.0`) is pushed.
- CI version check (`.github/scripts/check-version.sh`) that validates, on every
  build, that the declared version is a valid semver release, is bumped relative
  to the latest release tag, and is documented in this changelog.