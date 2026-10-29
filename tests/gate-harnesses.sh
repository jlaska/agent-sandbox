#!/usr/bin/env bash
# Which harnesses a change must pass the workflow gate for.
#
# Usage: tests/gate-harnesses.sh BASE [HEAD]
#
# Compares HEAD (default: HEAD) with its merge base with BASE, as a PR diff
# does. Prints one line per harness to gate, in a stable order:
#   <harness> pin     gate the image pinned in openshell/images.yaml
#   <harness> build   the image's sources changed: build it locally and gate that
# No output means no harness is affected.
#
#   openshell/images/<h>/*           <h> build
#   openshell/images/* (base, tools) all build
#   openshell/images.yaml            each harness whose pin changed, pin
#   agent-run, its embedded config, the gate and this script: all pin
#
# Used by tests/gate-all.sh --pr and .github/workflows/workflow-gate.yml.
#
# Exit status: 0 = ok, 2 = usage error or unknown commit.

set -euo pipefail

[[ $# -ge 1 && $# -le 2 ]] || { sed -n '2,/^$/s/^# \{0,1\}//p' "$0" >&2; exit 2; }
base="$1" head="${2:-HEAD}"
for c in "$base" "$head"; do
    git cat-file -e "$c^{commit}" 2>/dev/null || { echo "gate-harnesses: unknown commit: $c" >&2; exit 2; }
done

all="claude pi opencode"
# Per harness: "", "pin" or "build" (build wins). Read and set indirectly.
# shellcheck disable=SC2034
claude="" pi="" opencode=""
mark() { # mark HARNESS|all MODE
    local h hs="$1"
    [[ "$hs" == all ]] && hs="$all"
    for h in $hs; do
        if [[ "$2" == build || -z "${!h}" ]]; then printf -v "$h" '%s' "$2"; fi
    done
}

while IFS= read -r f; do
    case "$f" in
        openshell/images/claude/*) mark claude build ;;
        openshell/images/pi/*) mark pi build ;;
        openshell/images/opencode/* | openshell/images/refresh-checksums.sh) mark opencode build ;;
        openshell/images/*) mark all build ;;
        openshell/images.yaml)
            # Captured, not piped into grep -q: an early exit would SIGPIPE git under pipefail.
            pins="$(git diff "$base...$head" -- openshell/images.yaml)"
            for h in $all; do
                if grep -qE "^[-+].*/agent-$h:" <<<"$pins"; then mark "$h" pin; fi
            done ;;
        cmd/* | internal/* | openshell/* | go.mod | go.sum | Makefile \
            | tests/workflow-gate.sh | tests/lib/* | tests/gate-*.sh \
            | .github/workflows/workflow-gate.yml) mark all pin ;;
    esac
done < <(git diff --name-only "$base...$head")

for h in $all; do
    [[ -n "${!h}" ]] && echo "$h ${!h}"
done
exit 0
