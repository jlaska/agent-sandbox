#!/usr/bin/env bash
# L7 capture -- record which (method, host, path) requests a real agent-run
# session makes, so inference rules can be pinned to the paths harnesses use.
#
# Runs agent-run unchanged in the foreground (interactive sessions keep the
# terminal), follows the new sandbox's OpenShell logs while it runs, and prints
# every network decision with query strings dropped, deduplicated and counted.
# `openshell logs` (v0.1.4) shows one OCSF line per L7 request, allowed or not:
#   HTTP:POST [INFO] ALLOWED POST http://api.anthropic.com:443/v1/messages [policy:anthropic_api engine:l7]
# and one per refused connection:
#   NET:OPEN [MED] DENIED /usr/bin/curl(0) -> example.com:443 [reason:transparent_tcp_policy_denied]
# Re-run after a harness version bump: a new path shows up here before it
# shows up as a failure.
#
# Usage:
#   ./tests/l7-capture.sh <agent-run args...>
#   ./tests/l7-capture.sh jlaska/agent-sandbox pi --model gpt-6-luna -- -p "read README.md and summarize it"
#   ./tests/l7-capture.sh jlaska/agent-sandbox claude          # interactive; exit the harness to finish
#
# Prerequisites: make build, OpenShell >= 0.1.4 with the gateway running, and
# the harness's real inference credentials (a dummy key stops at the first 401).
#
# Output: the summary on stdout; the raw log is kept and its path printed.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
AGENT_RUN="${AGENT_RUN:-${REPO_ROOT}/bin/agent-run}"

if [[ $# -lt 2 || "$1" == -h || "$1" == --help ]]; then
    sed -n '2,/^$/s/^# \{0,1\}//p' "${BASH_SOURCE[0]}"
    [[ $# -lt 2 ]] && exit 2
    exit 0
fi
harness="$2"
[[ -x "$AGENT_RUN" ]] || { echo "l7-capture: $AGENT_RUN not found (make build)" >&2; exit 2; }

raw="$(mktemp "${TMPDIR:-/tmp}/l7-capture.XXXXXX")"
sandboxes() { openshell sandbox list 2>/dev/null | awk '{print $1}' | grep -E "^a-${harness}-[0-9a-f]{8}$" | sort; }
before="$(sandboxes)"

# Watcher: find the sandbox this run creates (agent-run names it
# a-<harness>-<8 hex>), then follow its logs until it is deleted. $$ is this
# script's pid, also inside the subshell.
(
    name=""
    while [[ -z "$name" ]]; do
        kill -0 "$$" 2>/dev/null || exit 0
        name="$(comm -13 <(printf '%s\n' "$before") <(sandboxes) | head -n1)"
        [[ -z "$name" ]] && sleep 1
    done
    echo "$name" >"$raw.name"
    # `logs --tail` fails until the sandbox is up and ends when it is deleted.
    while kill -0 "$$" 2>/dev/null; do
        openshell logs "$name" --tail --source sandbox >>"$raw" 2>/dev/null
        sleep 1
    done
) &
watcher=$!

"$AGENT_RUN" "$@"
rc=$?
sleep 2
pkill -P "$watcher" 2>/dev/null
kill "$watcher" 2>/dev/null
wait "$watcher" 2>/dev/null

echo ""
echo "=== L7 requests ($(cat "$raw.name" 2>/dev/null || echo 'sandbox not found')) ==="
# A restarted `logs --tail` replays recent lines, so counts are upper bounds.
# Fields: count, decision, method (or binary for connection denials), URL,
# rule (or the denial reason). FAILED is an upstream connection that failed.
summary="$(
    {
        sed -nE 's#.*HTTP:[A-Z]+ \[[A-Z]+ *\] ([A-Z]+) ([A-Z]+) http://([^ ?]+)[^ ]* \[policy:([^ ]+) .*#\1 \2 \3 \4#p' "$raw"
        sed -nE 's#.*NET:OPEN \[[A-Z]+ *\] (DENIED|BLOCKED) ([^ ]+)\([0-9]+\) -> ([^ ]+) \[(policy|reason):([^] ]+).*#\1 \2 \3 \5#p' "$raw"
        sed -nE 's#.*NET:FAIL \[[A-Z]+ *\] ([^ ]+).*#FAILED - \1 -#p' "$raw"
    } | sort | uniq -c | sort -k4,4 -k3,3
)"
if [[ -n "$summary" ]]; then
    echo "$summary"
else
    echo "  (none: no HTTP or denied NET lines in the log)"
fi
echo ""
echo "raw log: $raw"
rm -f "$raw.name"
exit "$rc"
