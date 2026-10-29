#!/usr/bin/env bash
# Run the workflow gate (tests/workflow-gate.sh) for several harnesses, and
# optionally report the result on a PR as the `workflow-gate` commit status.
#
# OWNER-RUN, same prerequisites as tests/workflow-gate.sh. Harnesses run one
# after another (they share the fixture repo); each one's log is kept on a
# FAIL.
#
# Credentials: LITELLM_API_KEY and LITELLM_BASE_URL from the environment or,
# on macOS, the Keychain (services litellm-api-key and anthropic-base-url).
# claude goes through the same LiteLLM endpoint: it gets them as
# ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL, and the host's other Claude Code
# inference settings (Vertex, custom headers) are dropped so agent-run does not
# forward them.
#
# --pr N checks out the PR head in a temporary worktree and gates that code:
# its agent-run, its gate scripts, and the harnesses tests/gate-harnesses.sh
# selects for it. A harness whose image sources changed is built locally
# (openshell/images/build.sh --local) and gated with --image. The PR head gets
# a `workflow-gate/<harness>` status per harness and the aggregate
# `workflow-gate` status: pending while running, then success or failure.
# PRs from forks are refused: the PR's code runs on this machine with your
# credentials.
#
# Usage:
#   ./tests/gate-all.sh                         # all harnesses, this checkout
#   ./tests/gate-all.sh --harness pi,opencode
#   ./tests/gate-all.sh --pr 119                # gate a PR and post its status
#   GATE_ARGS_PI="--model gpt-6-luna" ./tests/gate-all.sh --harness pi
#
# Per-harness agent-run options come from GATE_ARGS_CLAUDE, GATE_ARGS_PI and
# GATE_ARGS_OPENCODE (split on whitespace); options after -- go to every
# harness.
#
# Exit status: 0 = all PASS, 1 = at least one FAIL, 2 = usage/prerequisite error.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

HARNESSES=""
PR=""
POST_STATUS=true
GATE_OPTS=()
AGENT_ARGS=()

usage() { sed -n '2,/^$/s/^# \{0,1\}//p' "${BASH_SOURCE[0]}"; cat <<'USAGE'
Options:
  --harness LIST     comma-separated claude,pi,opencode (default: all; with
                     --pr, the harnesses the PR affects)
  --pr N             gate PR N of this repository (see above)
  --no-status        with --pr, do not post commit statuses
  --repo OWNER/REPO  passed to tests/workflow-gate.sh (target repo)
  --skip-inference   passed to tests/workflow-gate.sh
  --keep-logs        passed to tests/workflow-gate.sh; also keeps this run's logs
  -- ARGS...         extra agent-run options for every harness
  -h, --help         show this help
USAGE
}

KEEP_LOGS=false
while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help) usage; exit 0 ;;
        --harness) HARNESSES="$(tr ',' ' ' <<<"${2:?--harness needs a value}")"; shift 2 ;;
        --pr) PR="${2:?--pr needs a value}"; shift 2 ;;
        --no-status) POST_STATUS=false; shift ;;
        --repo) GATE_OPTS+=(--repo "${2:?--repo needs a value}"); shift 2 ;;
        --skip-inference) GATE_OPTS+=(--skip-inference); shift ;;
        --keep-logs) GATE_OPTS+=(--keep-logs); KEEP_LOGS=true; shift ;;
        --) shift; AGENT_ARGS=("$@"); break ;;
        *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done
for h in $HARNESSES; do
    case "$h" in claude|pi|opencode) ;; *) echo "Unknown harness: $h" >&2; exit 2 ;; esac
done
[[ -z "$PR" || "$PR" =~ ^[0-9]+$ ]] || { echo "--pr needs a PR number" >&2; exit 2; }
[[ -n "$PR" ]] || POST_STATUS=false
command -v gh >/dev/null || { echo "gh not on PATH" >&2; exit 2; }

# --- Credentials ---
keychain_get() { command -v security >/dev/null && security find-generic-password -s "$1" -w 2>/dev/null; }
[[ -n "${LITELLM_API_KEY:-}" ]] || LITELLM_API_KEY="$(keychain_get litellm-api-key)" || true
[[ -n "${LITELLM_BASE_URL:-}" ]] || LITELLM_BASE_URL="$(keychain_get anthropic-base-url)" || true
if [[ -z "${LITELLM_API_KEY:-}" || -z "${LITELLM_BASE_URL:-}" ]]; then
    if [[ " ${GATE_OPTS[*]-} " != *" --skip-inference "* ]]; then
        echo "LITELLM_API_KEY and LITELLM_BASE_URL are required (environment or Keychain)" >&2; exit 2
    fi
fi
export LITELLM_API_KEY LITELLM_BASE_URL
unset CLAUDE_CODE_USE_VERTEX ANTHROPIC_VERTEX_PROJECT_ID CLOUD_ML_REGION ANTHROPIC_CUSTOM_HEADERS CLAUDE_CODE_OAUTH_TOKEN

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/gate-all.XXXXXX")" || exit 2
SRC="$REPO_ROOT"
STATUS_REPO="" HEAD_SHA="" FETCH_REF="" WORKTREE=""
FAILED=false

# shellcheck disable=SC2329  # invoked via trap
cleanup() {
    if [[ -n "$WORKTREE" ]]; then git -C "$REPO_ROOT" worktree remove --force "$WORKTREE" >/dev/null 2>&1; fi
    if [[ -n "$FETCH_REF" ]]; then git -C "$REPO_ROOT" update-ref -d "$FETCH_REF" >/dev/null 2>&1; fi
    if [[ "$KEEP_LOGS" == true || "$FAILED" == true ]]; then echo "logs kept in $WORKDIR"; else rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

# status STATE CONTEXT DESCRIPTION: a commit status on the PR head.
status() {
    [[ "$POST_STATUS" == true ]] || return 0
    gh api -X POST "repos/$STATUS_REPO/statuses/$HEAD_SHA" \
        -f state="$1" -f context="$2" -f description="$(cut -c1-140 <<<"$3")" >/dev/null \
        || echo "warning: could not post the $2 status" >&2
}

# --- Source: this checkout, or the PR head in a worktree ---
declare -a MODES=()   # "harness mode" lines, mode = pin | build
if [[ -n "$PR" ]]; then
    STATUS_REPO="$(cd "$REPO_ROOT" && gh repo view --json nameWithOwner --jq .nameWithOwner)" \
        || { echo "cannot resolve this checkout's GitHub repository" >&2; exit 2; }
    info="$(gh pr view "$PR" -R "$STATUS_REPO" --json headRefOid,baseRefOid,isCrossRepository,state \
        --jq '"\(.headRefOid) \(.baseRefOid) \(.isCrossRepository) \(.state)"')" \
        || { echo "cannot read PR #$PR of $STATUS_REPO" >&2; exit 2; }
    read -r HEAD_SHA BASE_SHA cross state <<<"$info"
    [[ "$cross" == false ]] || { echo "PR #$PR is from a fork; refusing to run its code here" >&2; exit 2; }
    [[ "$state" == OPEN ]] || echo "note: PR #$PR is $state"
    # Fetch into named refs: objects only FETCH_HEAD points to can be pruned.
    FETCH_REF="refs/gate-all/pr-$PR"
    git -C "$REPO_ROOT" fetch -q "https://github.com/$STATUS_REPO.git" \
        "+refs/pull/$PR/head:$FETCH_REF" "+$BASE_SHA:$FETCH_REF-base" 2>/dev/null \
        || git -C "$REPO_ROOT" fetch -q "https://github.com/$STATUS_REPO.git" \
            "+refs/pull/$PR/head:$FETCH_REF" "+refs/heads/main:$FETCH_REF-base" \
        || { echo "cannot fetch PR #$PR" >&2; exit 2; }
    [[ "$(git -C "$REPO_ROOT" rev-parse "$FETCH_REF")" == "$HEAD_SHA" ]] \
        || { echo "PR #$PR moved while fetching; rerun" >&2; exit 2; }
    WORKTREE="$WORKDIR/src"
    git -C "$REPO_ROOT" worktree add -q --detach "$WORKTREE" "$HEAD_SHA" || exit 2
    SRC="$WORKTREE"
    echo "=== PR #$PR at ${HEAD_SHA:0:12} ==="
    while IFS= read -r line; do MODES+=("$line"); done \
        < <(cd "$SRC" && ./tests/gate-harnesses.sh "$FETCH_REF-base" "$HEAD_SHA")
    git -C "$REPO_ROOT" update-ref -d "$FETCH_REF-base" >/dev/null 2>&1
    if [[ -n "$HARNESSES" ]]; then
        # An explicit list overrides the selection; keep each one's build mode.
        sel=()
        for h in $HARNESSES; do
            m=pin
            for line in ${MODES[@]+"${MODES[@]}"}; do [[ "$line" == "$h build" ]] && m=build; done
            sel+=("$h $m")
        done
        MODES=("${sel[@]}")
    fi
else
    for h in ${HARNESSES:-claude pi opencode}; do MODES+=("$h pin"); done
fi

if [[ ${#MODES[@]} -eq 0 ]]; then
    echo "No harness affected; nothing to gate."
    status success workflow-gate "No harness affected"
    exit 0
fi

names=""
for line in "${MODES[@]}"; do names="$names ${line%% *}"; done
names="${names# }"
echo "Gating: $names"
for h in $names; do status pending "workflow-gate/$h" "Queued (tests/gate-all.sh)"; done
status pending workflow-gate "Running: $names"

# --- Build ---
if ! make -C "$SRC" build >"$WORKDIR/build.log" 2>&1; then
    FAILED=true
    tail -n 20 "$WORKDIR/build.log"
    for h in $names; do status failure "workflow-gate/$h" "agent-run build failed"; done
    status failure workflow-gate "agent-run build failed"
    exit 1
fi

# --- Gate each harness ---
RESULTS=()
for line in "${MODES[@]}"; do
    h="${line%% *}" mode="${line#* }"
    log="$WORKDIR/$h.log"
    image_args=()
    echo ""
    if [[ "$mode" == build ]]; then
        echo "=== $h: building localhost/agent-$h:local ==="
        if ! "$SRC/openshell/images/build.sh" --local "$h" >"$WORKDIR/$h-image.log" 2>&1; then
            FAILED=true
            tail -n 20 "$WORKDIR/$h-image.log"
            RESULTS+=("$h FAIL image build failed")
            status failure "workflow-gate/$h" "Image build failed"
            continue
        fi
        image_args=(--image "localhost/agent-$h:local")
    fi
    case "$h" in
        claude) read -r -a hargs <<<"${GATE_ARGS_CLAUDE:-}" ;;
        pi) read -r -a hargs <<<"${GATE_ARGS_PI:-}" ;;
        opencode) read -r -a hargs <<<"${GATE_ARGS_OPENCODE:-}" ;;
    esac
    status pending "workflow-gate/$h" "Running"
    echo "=== $h ($mode) ==="
    if [[ "$h" == claude ]]; then
        env ANTHROPIC_API_KEY="$LITELLM_API_KEY" ANTHROPIC_BASE_URL="$LITELLM_BASE_URL" \
            "$SRC/tests/workflow-gate.sh" --harness "$h" ${image_args[@]+"${image_args[@]}"} ${GATE_OPTS[@]+"${GATE_OPTS[@]}"} \
            -- ${hargs[@]+"${hargs[@]}"} ${AGENT_ARGS[@]+"${AGENT_ARGS[@]}"} 2>&1 | tee "$log"
    else
        "$SRC/tests/workflow-gate.sh" --harness "$h" ${image_args[@]+"${image_args[@]}"} ${GATE_OPTS[@]+"${GATE_OPTS[@]}"} \
            -- ${hargs[@]+"${hargs[@]}"} ${AGENT_ARGS[@]+"${AGENT_ARGS[@]}"} 2>&1 | tee "$log"
    fi
    rc=${PIPESTATUS[0]}
    summary="$(grep -E '^Summary: ' "$log" | tail -n1)"
    summary="${summary#Summary: }"
    if [[ $rc -eq 0 ]]; then
        RESULTS+=("$h PASS ${summary:-ok}")
        status success "workflow-gate/$h" "${summary:-passed} ($mode)"
    else
        FAILED=true
        RESULTS+=("$h FAIL ${summary:-exit $rc}")
        status failure "workflow-gate/$h" "${summary:-exit $rc} ($mode)"
    fi
done

echo ""
echo "=== Workflow gate results ==="
for r in "${RESULTS[@]}"; do
    read -r h verdict rest <<<"$r"
    printf '%-9s %-5s %s\n' "$h" "$verdict" "$rest"
done
if [[ "$FAILED" == true ]]; then
    status failure workflow-gate "Failed: $(for r in "${RESULTS[@]}"; do [[ "$r" == *" FAIL "* ]] && printf '%s ' "${r%% *}"; done)"
    exit 1
fi
status success workflow-gate "Passed: $names"
exit 0
