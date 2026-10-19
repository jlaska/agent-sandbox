#!/usr/bin/env bash
# Refresh the pinned sha256 digests of the opencode release assets in opencode/Dockerfile.
#
# opencode publishes no checksum file, so the Dockerfile hardcodes
# OPENCODE_SHA256_AMD64 / OPENCODE_SHA256_ARM64. Run this after every OPENCODE_VERSION
# bump (including Renovate PRs). It reads OPENCODE_VERSION from the Dockerfile, downloads
# the same release assets the Dockerfile downloads, and rewrites the two ARGs in place.
#
# Usage: refresh-checksums.sh [--check]
#   --check   change nothing; print the expected digests and exit 1 if the Dockerfile differs
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dockerfile="$here/opencode/Dockerfile"
check=0
case "${1:-}" in
  "") ;;
  --check) check=1 ;;
  -h | --help) sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
  *) echo "refresh-checksums.sh: unknown argument: $1" >&2; exit 2 ;;
esac

version="$("$here/version.sh" opencode)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

sha256_of() {
  if command -v sha256sum > /dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

fetch_digest() { # <asset arch: x64|arm64>
  local f="opencode-linux-$1.tar.gz"
  curl -fsSL --retry 3 -o "$tmp/$f" \
    "https://github.com/anomalyco/opencode/releases/download/v${version}/${f}" >&2
  sha256_of "$tmp/$f"
}

current() { sed -n "s/^ARG $1=\\([^[:space:]]*\\)\$/\\1/p" "$dockerfile" | head -n1; }

want_amd64="$(fetch_digest x64)"
want_arm64="$(fetch_digest arm64)"
have_amd64="$(current OPENCODE_SHA256_AMD64)"
have_arm64="$(current OPENCODE_SHA256_ARM64)"

if [ "$have_amd64" = "$want_amd64" ] && [ "$have_arm64" = "$want_arm64" ]; then
  echo "opencode ${version}: checksums in Dockerfile are up to date"
  exit 0
fi

if [ "$check" -eq 1 ]; then
  {
    echo "opencode ${version}: checksums in openshell/images/opencode/Dockerfile are out of date."
    echo "  OPENCODE_SHA256_AMD64 have ${have_amd64:-<missing>}"
    echo "                        want ${want_amd64}"
    echo "  OPENCODE_SHA256_ARM64 have ${have_arm64:-<missing>}"
    echo "                        want ${want_arm64}"
    echo "Fix: run openshell/images/refresh-checksums.sh and commit the result."
  } >&2
  exit 1
fi

# Portable in-place edit: write a temp file and move it over (BSD and GNU sed differ on -i).
sed -e "s/^ARG OPENCODE_SHA256_AMD64=.*/ARG OPENCODE_SHA256_AMD64=${want_amd64}/" \
    -e "s/^ARG OPENCODE_SHA256_ARM64=.*/ARG OPENCODE_SHA256_ARM64=${want_arm64}/" \
    "$dockerfile" > "$tmp/Dockerfile"
cat "$tmp/Dockerfile" > "$dockerfile"
echo "opencode ${version}: updated ${dockerfile}"
