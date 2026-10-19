#!/usr/bin/env bash
# Build the agent-sandbox images with podman (decision D16: no docker, no bake).
#
# Usage:
#   build.sh [--engine podman] [--platform linux/arm64|linux/amd64] [--registry R]
#            [--tag-suffix S] [--local] <base|claude|pi|opencode|all>
#
# Flow: `base` is always built first as localhost/agent-base:<tag> (<tag> is `local`
# with --local, otherwise the short git sha). Each harness is then built with
# --build-arg BASE_IMAGE=<that tag>, so base never needs a registry digest.
#
# Tags for harness <h> (version from `version.sh <h>`, the Dockerfile ARG):
#   --local     localhost/agent-<h>:local
#   otherwise   <registry>/agent-<h>:<ver>, plus :<ver>-<suffix> when --tag-suffix is set
#
# Environment: CONTAINER_ENGINE overrides --engine (default podman); REGISTRY and
# TAG_SUFFIX provide defaults for --registry (ghcr.io/jlaska) and --tag-suffix.
# Platform defaults to the host architecture. Images are built single-arch; CI
# combines per-arch images into a manifest list.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
engine="${CONTAINER_ENGINE:-}"
engine_flag="podman"
platform=""
registry="${REGISTRY:-ghcr.io/jlaska}"
suffix="${TAG_SUFFIX:-}"
local_build=0
target=""

usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --engine) engine_flag="${2:?--engine needs a value}"; shift 2 ;;
    --platform) platform="${2:?--platform needs a value}"; shift 2 ;;
    --registry) registry="${2:?--registry needs a value}"; shift 2 ;;
    --tag-suffix) suffix="${2:?--tag-suffix needs a value}"; shift 2 ;;
    --local) local_build=1; shift ;;
    -h | --help) usage; exit 0 ;;
    base | claude | pi | opencode | all) target="$1"; shift ;;
    *) echo "build.sh: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done
[ -n "$target" ] || { usage >&2; exit 2; }
engine="${engine:-$engine_flag}"

if [ -z "$platform" ]; then
  case "$(uname -m)" in
    arm64 | aarch64) platform="linux/arm64" ;;
    x86_64 | amd64) platform="linux/amd64" ;;
    *) echo "build.sh: cannot infer platform from $(uname -m); use --platform" >&2; exit 2 ;;
  esac
fi

revision="$(git -C "$here" rev-parse HEAD 2>/dev/null || echo unknown)"
created="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
if [ "$local_build" -eq 1 ]; then
  base_tag="local"
else
  base_tag="$(git -C "$here" rev-parse --short=7 HEAD 2>/dev/null || echo local)"
fi
base_ref="localhost/agent-base:${base_tag}"

common_args=(--platform "$platform" --format docker)
label_args=(--build-arg "REVISION=${revision}" --build-arg "CREATED=${created}")

build_base() {
  echo "==> building ${base_ref} (${platform})"
  "$engine" build "${common_args[@]}" -t "$base_ref" "$here/base"
}

build_harness() {
  local h="$1" ver tags=()
  ver="$("$here/version.sh" "$h")"
  if [ "$local_build" -eq 1 ]; then
    tags+=(-t "localhost/agent-${h}:local")
  else
    tags+=(-t "${registry}/agent-${h}:${ver}")
    [ -z "$suffix" ] || tags+=(-t "${registry}/agent-${h}:${ver}-${suffix}")
  fi
  echo "==> building agent-${h} ${ver} (${platform}) on ${base_ref}"
  "$engine" build "${common_args[@]}" "${label_args[@]}" --build-arg "BASE_IMAGE=${base_ref}" "${tags[@]}" "$here/$h"
}

build_base
case "$target" in
  base) ;;
  all) for h in claude pi opencode; do build_harness "$h"; done ;;
  *) build_harness "$target" ;;
esac
