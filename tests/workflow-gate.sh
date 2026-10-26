#!/usr/bin/env bash
# Workflow gate -- the agent's GitHub workflow, end to end, in a real
# agent-run session.
#
# OWNER-RUN integration test. Starts one agent-run session (as
# tests/policy-matrix.sh does), then works the target repo from inside the
# sandbox the way an agent would: fetch, push agent/** branches, open a PR,
# comment, review, read checks, mark it ready. It also checks what must be
# refused, and requires the right refusal:
#   ruleset  GitHub rejects the push (GH013 repository rule violations):
#            push main, force-push main, push a non-agent branch, push a tag;
#            deleting main is refused by GitHub itself (default branch)
#   policy   the OpenShell proxy refuses the request (policy_denied, or a 403
#            for git): REST and GraphQL merge, auto-merge, cloning a repo the
#            App is not installed on
# A refusal for any other reason (network error, 404, a draft PR) is a FAIL:
# the guard under test did not act. The PR stays a draft until the merge
# checks are done, so a policy regression cannot merge it.
#
# Host-side checks: the target repo has the three rulesets, and no App
# (Integration) can bypass them. Unless --skip-inference, a second short
# session asks the harness to reply with a token, through its real provider.
#
# Usage:
#   ./tests/workflow-gate.sh                                   # claude on the fixture repo
#   ./tests/workflow-gate.sh --harness pi -- --model gpt-6-luna
#   ./tests/workflow-gate.sh --harness opencode --skip-inference -- --inference anthropic --env ANTHROPIC_API_KEY=dummy
#
# The target repo must be one where test branches and closed PRs are
# acceptable, with the rulesets of this repo (default: the public fixture
# jlaska/agent-sandbox-mise-fixture). Branches, the tag (if a regression
# created it) and the PR are removed on exit with your own gh login.
#
# Prerequisites: make build, OpenShell >= 0.1.4 with the gateway running, GitHub
# App Keychain entries (the App installed on --repo), gh logged in as a repo
# admin (rulesets read, cleanup), and the harness's inference credentials.
#
# Exit status: 0 = all PASS, 1 = at least one FAIL, 2 = usage/prerequisite error.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
AGENT_RUN="${AGENT_RUN:-${REPO_ROOT}/bin/agent-run}"

REPO="${REPO:-jlaska/agent-sandbox-mise-fixture}"
# Permanent canary: a repo that must never be added to the App installation.
CANARY_REPO="${CANARY_REPO:-jlaska/agent-sandbox-denied}"
HARNESS="claude"
IMAGE=""
SKIP_INFERENCE=false
KEEP_LOGS=false
AGENT_ARGS=()

usage() { sed -n '2,/^$/s/^# \{0,1\}//p' "${BASH_SOURCE[0]}"; cat <<'USAGE'
Options:
  --repo OWNER/REPO     target repo (default: jlaska/agent-sandbox-mise-fixture)
  --harness H           claude | pi | opencode (default: claude)
  --image REF           harness image (default: images.yaml via agent-run)
  --skip-inference      skip the inference round trip
  --keep-logs           keep the agent-run logs (always kept on a FAIL)
  -- ARGS...            extra agent-run options for both sessions
  -h, --help            show this help
USAGE
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help) usage; exit 0 ;;
        --repo) REPO="${2:?--repo needs a value}"; shift 2 ;;
        --harness) HARNESS="${2:?--harness needs a value}"; shift 2 ;;
        --image) IMAGE="${2:?--image needs a value}"; shift 2 ;;
        --skip-inference) SKIP_INFERENCE=true; shift ;;
        --keep-logs) KEEP_LOGS=true; shift ;;
        --) shift; AGENT_ARGS=("$@"); break ;;
        *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done
case "$HARNESS" in claude|pi|opencode) ;; *) echo "Unknown harness: $HARNESS" >&2; exit 2 ;; esac
if [[ "$REPO" != */* ]]; then echo "--repo must be OWNER/REPO" >&2; exit 2; fi
[[ -x "$AGENT_RUN" ]] || { echo "agent-run binary missing at $AGENT_RUN (run: make build)" >&2; exit 2; }
command -v openshell >/dev/null || { echo "openshell not on PATH" >&2; exit 2; }
command -v gh >/dev/null || { echo "gh not on PATH" >&2; exit 2; }

TS="$(date +%s)"
BRANCH="agent/gate-${TS}"
BRANCH_NESTED="agent/gate/${TS}/nested"
BAD_BRANCH="feature/gate-${TS}"
TAG="gate-${TS}"
PR_NUM=""

PASS=0 FAIL=0
pass() { echo "PASS   $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL   $1"; FAIL=$((FAIL + 1)); }
# Last non-empty output line, flattened, for FAIL details.
tail1() { printf '%s\n' "$1" | sed '/^[[:space:]]*$/d' | tail -n1 | tr -s '\t ' ' ' | cut -c1-140; }

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/workflow-gate.XXXXXX")"
SANDBOX=""
# shellcheck source=tests/lib/session.sh
source "${SCRIPT_DIR}/lib/session.sh"

# Runs in /sandbox/repo, where agent-run cloned the repo and set up git.
sbr() { openshell sandbox exec -n "$SANDBOX" --workdir /sandbox/repo -- bash -c "$1" 2>&1; }

# expect_ruleset LABEL SCRIPT [WHY PATTERN]: the push must be rejected by a
# GitHub ruleset, or by GitHub for the reason PATTERN matches.
expect_ruleset() {
    local out why="ruleset" re='GH013|repository rule violations'
    [[ $# -ge 4 ]] && why="$3" re="$4"
    out="$(sbr "$2")"
    if printf '%s' "$out" | grep -qE "$re"; then
        pass "$1 (refused by $why)"
    else
        fail "$1: not refused by $why ($(printf '%s\n' "$out" | grep -m1 -E 'rejected|error' | cut -c1-140))"
    fi
}

# expect_policy LABEL SCRIPT: the request must be refused by the OpenShell proxy.
expect_policy() {
    local out
    out="$(sbr "$2")"
    if printf '%s' "$out" | grep -q 'policy_denied'; then
        pass "$1 (refused by policy)"
    else
        fail "$1: not refused by policy ($(tail1 "$out"))"
    fi
}

# shellcheck disable=SC2329  # invoked via trap
cleanup() {
    end_session
    local b
    [[ -n "$PR_NUM" ]] && gh pr close "$PR_NUM" --repo "$REPO" >/dev/null 2>&1
    for b in "$BRANCH" "$BRANCH_NESTED" "$BAD_BRANCH"; do
        gh api -X DELETE "repos/$REPO/git/refs/heads/$b" >/dev/null 2>&1
    done
    gh api -X DELETE "repos/$REPO/git/refs/tags/$TAG" >/dev/null 2>&1
    if [[ "$KEEP_LOGS" == true || $FAIL -gt 0 ]]; then echo "logs kept in $WORKDIR"; else rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

echo "=== Workflow gate: $HARNESS on $REPO ($(date -u +%Y-%m-%dT%H:%M:%SZ)) ==="

# --- Host: rulesets ---
rulesets="$(gh api "repos/$REPO/rulesets" --jq '.[] | select(.enforcement == "active") | .name' 2>&1)"
for want in "Protect default branch" "Protect tags" "Restrict non-agent branches"; do
    if printf '%s\n' "$rulesets" | grep -qxF "$want"; then
        pass "ruleset active: $want"
    else
        fail "ruleset missing or not active: $want"
    fi
done
app_bypass="$(gh api "repos/$REPO/rulesets" --jq '.[].id' 2>/dev/null | while read -r id; do
    gh api "repos/$REPO/rulesets/$id" --jq '.bypass_actors[]? | select(.actor_type == "Integration") | .actor_id'
done)"
if [[ -z "$app_bypass" ]]; then
    pass "no App can bypass the rulesets"
else
    fail "rulesets have Integration bypass actors: $(echo "$app_bypass" | tr '\n' ' ')"
fi

# --- Session ---
echo "Starting $HARNESS session on $REPO (waits for egress narrowing)..."
start_session || exit 2
echo "Sandbox: $SANDBOX"

# Credentials: placeholders only, no App key material.
for var in GH_TOKEN GITHUB_TOKEN; do
    val="$(sbr "printenv $var")"
    if [[ "$val" == openshell:resolve:* ]]; then
        pass "$var is a placeholder"
    else
        fail "$var is not a placeholder (${#val} chars)"
    fi
done
keys="$(sbr 'find /sandbox /tmp -name "*.pem" -not -path "/sandbox/repo/*" -exec grep -l "PRIVATE KEY" {} + 2>/dev/null; true')"
if [[ -z "$keys" ]]; then pass "no private key (.pem) in /sandbox or /tmp"; else fail "private key found: $(tail1 "$keys")"; fi

# Allowed: fetch and agent/** pushes.
out="$(sbr 'git fetch origin && echo FETCH-OK')"
if [[ "$out" == *FETCH-OK* ]]; then pass "fetch"; else fail "fetch ($(tail1 "$out"))"; fi
for b in "$BRANCH" "$BRANCH_NESTED"; do
    out="$(sbr "git checkout -q -B '$b' origin/main && echo '$b' >gate.txt && git add gate.txt && git commit -q -m 'workflow gate: $b' && git push origin '$b'")"
    if [[ "$out" == *"[new branch]"* ]]; then pass "push $b"; else fail "push $b ($(tail1 "$out"))"; fi
done

# Refused by rulesets.
expect_ruleset "push main" \
    "git checkout -q -B gate-main origin/main && echo x >gate.txt && git add gate.txt && git commit -q -m 'workflow gate: main' && git push origin HEAD:main"
expect_ruleset "force-push main" \
    "git checkout -q -B gate-force origin/main && git commit -q --amend -m 'workflow gate: rewritten main' && git push --force origin HEAD:main"
# GitHub refuses to delete the default branch before rulesets apply.
expect_ruleset "delete main" "git push origin --delete main" \
    "GitHub (default branch)" 'refusing to delete the current branch|GH013|repository rule violations'
expect_ruleset "push $BAD_BRANCH" \
    "git checkout -q -B '$BAD_BRANCH' origin/main && echo x >gate.txt && git add gate.txt && git commit -q -m 'workflow gate: non-agent' && git push origin '$BAD_BRANCH'"
expect_ruleset "push tag $TAG" "git tag '$TAG' origin/main && git push origin 'refs/tags/$TAG'"

# Allowed: PR workflow, on a draft so no merge can land.
out="$(sbr "gh pr create --draft --base main --head '$BRANCH' --title 'Workflow gate ($HARNESS)' --body 'Automated workflow gate; closed on exit.'")"
PR_NUM="$(printf '%s\n' "$out" | grep -oE '/pull/[0-9]+' | tail -n1 | cut -d/ -f3)"
if [[ -n "$PR_NUM" ]]; then
    pass "create draft PR #$PR_NUM"
    out="$(sbr "gh pr comment $PR_NUM --body 'workflow gate comment'")"
    if [[ "$out" == *issuecomment* ]]; then pass "comment"; else fail "comment ($(tail1 "$out"))"; fi
    out="$(sbr "gh pr review $PR_NUM --comment --body 'workflow gate review' && echo REVIEW-OK")"
    if [[ "$out" == *REVIEW-OK* ]]; then pass "review"; else fail "review ($(tail1 "$out"))"; fi
    out="$(sbr "gh api repos/$REPO/commits/\$(git rev-parse '$BRANCH')/check-runs --jq .total_count")"
    if [[ "$(tail1 "$out")" =~ ^[0-9]+$ ]]; then pass "read checks"; else fail "read checks ($(tail1 "$out"))"; fi

    node_id="$(sbr "gh pr view $PR_NUM --json id -q .id" | tail -n1)"
    expect_policy "REST merge" "gh api -X PUT repos/$REPO/pulls/$PR_NUM/merge -f merge_method=merge"
    expect_policy "GraphQL mergePullRequest" \
        "gh api graphql -f query='mutation { mergePullRequest(input: {pullRequestId: \"$node_id\"}) { pullRequest { number } } }'"
    expect_policy "GraphQL enablePullRequestAutoMerge" \
        "gh api graphql -f query='mutation { enablePullRequestAutoMerge(input: {pullRequestId: \"$node_id\"}) { pullRequest { number } } }'"

    out="$(sbr "gh pr ready $PR_NUM && echo READY-OK")"
    if [[ "$out" == *READY-OK* ]]; then pass "mark ready for review"; else fail "mark ready ($(tail1 "$out"))"; fi
else
    fail "create draft PR ($(tail1 "$out")); PR and merge checks not run"
fi

# Refused by policy: a repo outside the session (git sees the proxy's 403).
out="$(sbr "git clone -q https://github.com/$CANARY_REPO.git /tmp/canary")"
if printf '%s' "$out" | grep -q 'error: 403'; then
    pass "clone $CANARY_REPO (refused by policy)"
else
    fail "clone $CANARY_REPO: not refused by policy ($(tail1 "$out"))"
fi

end_session

# --- Inference round trip, through the harness and its provider ---
if [[ "$SKIP_INFERENCE" == true ]]; then
    echo "SKIP   inference (--skip-inference)"
else
    prompt="Reply with exactly WORKFLOW-GATE-OK and nothing else."
    case "$HARNESS" in claude|pi) pargs=(-p "$prompt") ;; opencode) pargs=(run "$prompt") ;; esac
    iargs=()
    [[ -n "$IMAGE" ]] && iargs+=(--image "$IMAGE")
    "$AGENT_RUN" "$REPO" "$HARNESS" ${iargs[@]+"${iargs[@]}"} ${AGENT_ARGS[@]+"${AGENT_ARGS[@]}"} \
        -- "${pargs[@]}" </dev/null >"$WORKDIR/inference.log" 2>&1
    if grep -v 'Reply with exactly' "$WORKDIR/inference.log" | grep -q 'WORKFLOW-GATE-OK'; then
        pass "inference round trip"
    else
        # The harness's output sits between agent-run's launch and cleanup
        # lines; if it never launched, agent-run's own last lines say why.
        harness_out="$(awk '/^\[agent-run\] Working directory:/ {on=1; next} on && /^(\[agent-run\]|--- Cleanup ---)/ {exit} on && !/^---$/' "$WORKDIR/inference.log")"
        if grep -q 'Launching harness' "$WORKDIR/inference.log"; then
            fail "inference round trip: no WORKFLOW-GATE-OK in the harness output; its last lines:"
        else
            fail "inference round trip: the harness never launched; agent-run's last lines:"
            harness_out="$(cat "$WORKDIR/inference.log")"
        fi
        printf '%s\n' "$harness_out" | sed '/^[[:space:]]*$/d' | tail -n 8 | sed 's/^/         | /'
    fi
fi

echo ""
echo "Summary: $PASS pass, $FAIL fail"
[[ $FAIL -eq 0 ]] || exit 1
exit 0
