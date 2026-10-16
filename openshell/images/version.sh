#!/usr/bin/env bash
# Print the pinned version of a harness image, read from its Dockerfile.
# The Dockerfile `ARG <HARNESS>_VERSION=x.y.z` is the single source of truth;
# build.sh, smoke.sh and CI all call this script instead of duplicating the pin.
#
# Usage: version.sh <claude|pi|opencode>
set -euo pipefail

harness="${1:-}"
case "$harness" in
  claude | pi | opencode) ;;
  *) echo "usage: version.sh <claude|pi|opencode>" >&2; exit 2 ;;
esac

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
arg="$(printf '%s' "$harness" | tr '[:lower:]' '[:upper:]')_VERSION"
ver="$(sed -n "s/^ARG ${arg}=\\([^[:space:]]*\\)\$/\\1/p" "$dir/$harness/Dockerfile" | head -n1)"
if [ -z "$ver" ]; then
  echo "version.sh: ARG ${arg} not found in $dir/$harness/Dockerfile" >&2
  exit 1
fi
printf '%s\n' "$ver"
