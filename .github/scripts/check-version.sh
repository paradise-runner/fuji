#!/usr/bin/env bash
#
# check-version.sh — release hygiene gate for CI.
#
# Ensures the version declared in cmd/fuji/version.go is a valid semver
# *release* (no -dev suffix), that it is bumped relative to the most recent
# git tag, and that the new version is documented in CHANGELOG.md.
#
# Exits non-zero if any check fails.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
version_file="${repo_root}/cmd/fuji/version.go"
changelog_file="${repo_root}/CHANGELOG.md"

# --- 1. Extract the current version from version.go ------------------------
#
# The version lives in a Go var that the release build overrides via ldflags.
# For source-of-truth checking we read whatever is baked into the source.
extract_version() {
  sed -n 's/.*var version = "\([^"]*\)".*/\1/p' "${version_file}" | head -n1
}

current="$(extract_version)"
if [[ -z "${current}" ]]; then
  echo "check-version: could not find 'var version = \"...\"' in ${version_file}" >&2
  exit 1
fi
echo "check-version: current version = ${current}"

invalid() {
  echo "check-version: ${1}" >&2
  exit 1
}

# --- 2. Validate semver (release, not pre-release/dev) ----------------------
#
# Require x.y.z with an optional patch that is numeric. "-dev" and other
# pre-release/build suffixes are not valid for a release.
semver_regex='^v?([0-9]+)\.([0-9]+)\.([0-9]+)$'
if [[ ! "${current}" =~ ${semver_regex} ]]; then
  invalid "version '${current}' is not a valid semver release (expected MAJOR.MINOR.PATCH without -dev/pre-release/build metadata)"
fi
major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
patch="${BASH_REMATCH[3]}"

# --- 3. Must be bumped relative to the latest release tag -------------------
#
# Tags are stored as v0.1.0 etc. Find the most recent tag that matches v*.
latest_tag="$(cd "${repo_root}" && git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"

if [[ -n "${latest_tag}" ]]; then
  latest_normalized="${latest_tag#v}"
  if [[ "${latest_normalized}" == "${current}" ]]; then
    invalid "version '${current}' matches latest tag '${latest_tag}'; must be bumped for a release"
  fi
  echo "check-version: latest release tag = ${latest_tag}"

  # Compare numerically.
  IFS='.' read -r -a latest_parts <<< "${latest_normalized}"
  if (( major < 10#${latest_parts[0]} )); then
    invalid "version '${current}' is older than latest tag '${latest_tag}'"
  elif (( major == 10#${latest_parts[0]} )); then
    if (( minor < 10#${latest_parts[1]} )); then
      invalid "version '${current}' is older than latest tag '${latest_tag}'"
    elif (( minor == 10#${latest_parts[1]} )); then
      if (( patch <= 10#${latest_parts[2]} )); then
        invalid "version '${current}' is not greater than latest tag '${latest_tag}'"
      fi
    fi
  fi
else
  echo "check-version: no prior version tag found (initial release)"
fi

# --- 4. Must be documented in CHANGELOG.md -----------------------------------
#
# Look for a heading of the form "## [x.y.z]" in the changelog.
if ! grep -Eq "^## \\[${current}\\]" "${changelog_file}"; then
  invalid "version '${current}' is not documented in CHANGELOG.md (expected a '## [${current}]' heading)"
fi

echo "check-version: OK — ${current} is a valid, bumped, and documented release version."