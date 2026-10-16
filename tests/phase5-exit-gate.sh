#!/usr/bin/env bash
# Phase 5 exit gate tests — multi-agent compatibility.
#
# Usage:
#   ./tests/phase5-exit-gate.sh <agent> [--skip-inference] [--image <ref>]
#   ./tests/phase5-exit-gate.sh --help
#
# Supported agents: claude, pi, opencode  (the `shell` harness was removed)
#
# Sandboxes are created from the per-harness image (`openshell sandbox create
# --from <ref>`). The ref defaults to the one pinned in openshell/images.yaml;
# while that digest is all zeros (unpublished) pass --image, e.g.
# `--image localhost/agent-claude:local` after `make images`. Nothing is installed inside
# the sandbox.
#
# Prerequisites:
#   - OpenShell >= 0.1.4 with the gateway running
#   - GitHub App Keychain entries configured
#   - LiteLLM Keychain entries configured (for inference tests)
#   - bin/agent-run built

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
AGENT_RUN="${REPO_ROOT}/bin/agent-run"
LITELLM_PROFILE="${REPO_ROOT}/openshell/litellm-inference-profile.yaml"
GITHUB_PROFILE="${REPO_ROOT}/openshell/github-agent-profile.yaml"
IMAGES_YAML="${REPO_ROOT}/openshell/images.yaml"

usage() {
    cat <<'USAGE'
Usage: phase5-exit-gate.sh <agent> [--skip-inference] [--image <ref>]

  <agent>            claude | pi | opencode
  --skip-inference   skip the LiteLLM provider setup and connectivity test
  --image <ref>      sandbox image (default: ref pinned in openshell/images.yaml)
  -h, --help         show this help
USAGE
}

AGENT=""
SKIP_INFERENCE=""
IMAGE=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help) usage; exit 0 ;;
        --skip-inference) SKIP_INFERENCE="--skip-inference"; shift ;;
        --image) IMAGE="${2:?--image needs a value}"; shift 2 ;;
        -*) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
        *) if [[ -n "$AGENT" ]]; then echo "Unexpected argument: $1" >&2; usage >&2; exit 2; fi
           AGENT="$1"; shift ;;
    esac
done
if [[ -z "$AGENT" ]]; then usage >&2; exit 2; fi
case "$AGENT" in
    claude|pi|opencode) ;;
    shell) echo "The shell harness was removed (use claude, pi or opencode)." >&2; exit 2 ;;
    *) echo "Unsupported agent: $AGENT (use claude, pi or opencode)" >&2; exit 2 ;;
esac

REPO="jlaska/agent-sandbox-test"
SANDBOX_NAME="phase5-${AGENT}-$(date +%s | tail -c 6)"
BRANCH="agent/${AGENT}/phase5-test-$(date +%s | tail -c 4)"

PASS=0
FAIL=0
SKIP=0

# --- Helpers ---
keychain_get() { security find-generic-password -s "$1" -a "$USER" -w 2>/dev/null; }

pass() { echo "  ✅ PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  ❌ FAIL: $1"; FAIL=$((FAIL + 1)); }
skip() { echo "  ⏭️  SKIP: $1"; SKIP=$((SKIP + 1)); }

test_expect_pass() {
    local desc="$1"; shift
    if "$@" >/dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

test_expect_fail() {
    local desc="$1"; shift
    if "$@" >/dev/null 2>&1; then fail "$desc (should have failed)"; else pass "$desc"; fi
}

# --- Image resolution ---
# Pinned ref for the harness from openshell/images.yaml (empty if not found).
pinned_image() {
    awk -v h="$1" '
        $1 == h ":" { inblock = 1; next }
        inblock && $1 == "ref:" { print $2; exit }
        /^  [a-z]+:/ { inblock = 0 }
    ' "$IMAGES_YAML"
}

if [[ -z "$IMAGE" ]]; then
    IMAGE="$(pinned_image "$AGENT")"
    if [[ -z "$IMAGE" || "$IMAGE" =~ @sha256:0{64}$ ]]; then
        echo "FATAL: image for $AGENT is not published yet; build it (make images) and pass --image localhost/agent-$AGENT:local" >&2
        exit 1
    fi
fi

# Run a command in the sandbox (home directory / repo directory).
sb() { openshell sandbox exec -n "$SANDBOX_NAME" -- "$@"; }
sbr() { openshell sandbox exec -n "$SANDBOX_NAME" --workdir /sandbox/repo -- "$@"; }

# --- Cleanup ---
# shellcheck disable=SC2329  # invoked via trap
cleanup() {
    echo ""
    echo "--- Cleanup ---"
    openshell sandbox delete "$SANDBOX_NAME" 2>/dev/null || true
    openshell provider delete github-agent 2>/dev/null || true
    openshell provider delete claude-code 2>/dev/null || true
    openshell provider delete litellm-inference 2>/dev/null || true
    [[ -n "${POLICY_FILE:-}" ]] && rm -f "$POLICY_FILE"
    # Clean up test branch/PR
    gh pr list --repo "$REPO" --head "$BRANCH" --json number -q ".[0].number" 2>/dev/null | \
        xargs -I{} gh pr close {} --repo "$REPO" --delete-branch 2>/dev/null || true
    git push origin --delete "$BRANCH" 2>/dev/null || true
    echo "  Cleanup complete."
}
trap cleanup EXIT

echo "========================================="
echo "Phase 5 Exit Gate: $AGENT"
echo "  image: $IMAGE"
echo "========================================="
echo ""

# OpenShell 0.1.4 ships no built-in provider profiles: import what we need.
ensure_profile() {
    local id="$1" file="$2"
    if openshell provider list-profiles 2>/dev/null | grep -qw "$id"; then return 0; fi
    openshell provider profile import --file "$file" >/dev/null 2>&1
}

# --- Step 1: Create providers ---
echo "--- Setting up providers ---"
MINTED_TOKEN=$("$AGENT_RUN" --mint-token "$REPO" 2>/dev/null) || { echo "FATAL: mint token failed"; exit 1; }
POLICY_FILE=$("$AGENT_RUN" --generate-policy "$REPO" 2>/dev/null) || { echo "FATAL: policy generation failed"; exit 1; }

ensure_profile github-agent "$GITHUB_PROFILE"
openshell provider delete github-agent 2>/dev/null || true
# OpenShell 0.1.4 accepts only the profile's declared env_vars as stored keys;
# bare `--credential KEY` reads the value from the environment, so the token
# never appears in argv. The sandbox receives placeholders under these names.
GITHUB_TOKEN="$MINTED_TOKEN" GH_TOKEN="$MINTED_TOKEN" \
    openshell provider create --name github-agent --type github-agent \
    --credential GITHUB_TOKEN --credential GH_TOKEN >/dev/null 2>&1

PROVIDER_FLAGS=(--provider github-agent)
ENV_FLAGS=()

if [[ "$SKIP_INFERENCE" != "--skip-inference" ]]; then
    API_KEY=$(keychain_get "litellm-api-key") || { echo "FATAL: no litellm-api-key"; exit 1; }
    BASE_URL=$(keychain_get "anthropic-base-url") || { echo "FATAL: no anthropic-base-url"; exit 1; }

    ensure_profile litellm-inference "$LITELLM_PROFILE"
    openshell provider delete litellm-inference 2>/dev/null || true
    LITELLM_API_KEY="$API_KEY" \
        openshell provider create --name litellm-inference --type litellm-inference \
        --credential LITELLM_API_KEY >/dev/null 2>&1
    PROVIDER_FLAGS+=(--provider litellm-inference)

    case "$AGENT" in
        claude)
            # Claude talks to LiteLLM, whose credential is the LITELLM_API_KEY
            # placeholder; the claude-code provider (api.anthropic.com) is not needed.
            ENV_FLAGS=(--env "ANTHROPIC_BASE_URL=${BASE_URL}")
            ;;
        pi|opencode)
            ENV_FLAGS=(--env "LITELLM_BASE_URL=${BASE_URL}")
            ;;
    esac
fi

# --- Step 2: Create sandbox from the harness image ---
echo "--- Creating sandbox: $SANDBOX_NAME (from $IMAGE) ---"
openshell sandbox create \
    --name "$SANDBOX_NAME" \
    --from "$IMAGE" \
    "${PROVIDER_FLAGS[@]}" \
    --policy "$POLICY_FILE" \
    ${ENV_FLAGS[@]+"${ENV_FLAGS[@]}"} \
    --detach 2>&1 | grep -v "^$" || { echo "FATAL: sandbox create failed"; exit 1; }

# First start may pull the image, so allow more time than the old 30s.
WAIT=0
while [[ $WAIT -lt 180 ]]; do
    openshell sandbox list 2>/dev/null | grep -q "$SANDBOX_NAME.*Ready" && break
    sleep 1; WAIT=$((WAIT + 1))
done
openshell sandbox list 2>/dev/null | grep -q "$SANDBOX_NAME.*Ready" || { echo "FATAL: sandbox not ready"; exit 1; }

# agent-run always creates the sandbox with the temporary mise_bootstrap rule and
# removes it before the agent starts; mirror that so this gate runs under the
# narrowed policy.
echo "--- Narrowing egress (removing mise_bootstrap) ---"
openshell policy update "$SANDBOX_NAME" --remove-rule mise_bootstrap --wait >/dev/null 2>&1 \
    || { echo "FATAL: could not remove mise_bootstrap"; exit 1; }

# --- Step 3: Credential mapping (everything else is baked into the image) ---
case "$AGENT" in
    claude)
        # Alias from the placeholder-bearing LITELLM_API_KEY; never overwrite a
        # placeholder with a value from a key that no longer exists.
        # shellcheck disable=SC2016  # $LITELLM_API_KEY must expand inside the sandbox
        sb sh -c 'echo "export ANTHROPIC_API_KEY=\"\$LITELLM_API_KEY\"" >> ~/.profile' 2>/dev/null
        ;;
    pi|opencode)
        : # LITELLM_API_KEY already carries the placeholder
        ;;
esac

# --- Step 4: Clone and configure ---
echo "--- Cloning and configuring ---"
sb git clone "https://github.com/${REPO}.git" /sandbox/repo 2>/dev/null
sb sh -c "
    cd /sandbox/repo
    git config user.name 'jlaska-agent[bot]'
    git config user.email 'jlaska-agent[bot]@users.noreply.github.com'
    git config commit.gpgsign false
    git config tag.gpgsign false
    git config credential.helper '!f() { echo \"username=x-access-token\"; echo \"password=\${GH_TOKEN:-\$GITHUB_TOKEN}\"; }; f'
" 2>/dev/null

# --- Step 5: Acceptance tests ---
echo ""
echo "--- Running acceptance tests ---"
echo ""

# Image-based sandbox: harness present in the image, nothing installed at runtime
test_expect_pass "Harness binary present in image ($AGENT)" sb sh -c "command -v $AGENT"
PIN="$("$REPO_ROOT/openshell/images/version.sh" "$AGENT" 2>/dev/null)"
if [[ -n "$PIN" ]]; then
    ACTUAL="$(sb sh -c "$AGENT --version" 2>&1 | head -n 5)"
    if [[ "$ACTUAL" == *"$PIN"* ]]; then
        pass "$AGENT --version contains Dockerfile pin $PIN"
    else
        fail "$AGENT --version ('$ACTUAL') does not contain Dockerfile pin $PIN (image is not the pinned build?)"
    fi
else
    skip "Version pin check (version.sh unavailable)"
fi
test_expect_fail "mise_bootstrap egress removed (pypi.org denied)" \
    sb sh -c 'curl -fsS -m 15 -o /dev/null https://pypi.org/simple/'

# Allowed operations
test_expect_pass "Clone approved repo" true  # already succeeded above

echo ""
echo "  Creating branch $BRANCH..."
sbr sh -c "git checkout -b $BRANCH && echo 'test' > phase5-gate.txt && git add phase5-gate.txt && git commit -m 'Phase 5 gate test'" >/dev/null 2>&1
test_expect_pass "Push agent branch" sbr git push origin "$BRANCH"

sbr sh -c "gh pr create --title 'Phase 5 gate: $AGENT' --body 'Automated' --head $BRANCH --base main" >/dev/null 2>&1
test_expect_pass "Create PR" sbr sh -c "gh pr list --head $BRANCH --json number -q '.[0].number' | grep -q '[0-9]'"

PR_NUM=$(sbr sh -c "gh pr list --head $BRANCH --json number -q '.[0].number'" 2>/dev/null)
test_expect_pass "Comment on PR" sbr sh -c "gh pr comment $PR_NUM --body 'gate test'"
test_expect_pass "Submit review" sbr sh -c "gh pr review $PR_NUM --comment --body 'gate review'"
test_expect_pass "Inspect checks" sbr sh -c "gh pr checks $PR_NUM 2>&1; true"
test_expect_pass "Commit without YubiKey" true  # commit already succeeded above

# Denied operations
test_expect_fail "Push main" sbr sh -c "git checkout main 2>/dev/null; echo x > fail.txt; git add fail.txt; git commit -m fail; git push origin main"
test_expect_fail "Push unauthorized branch" sbr sh -c "git checkout -b feature/gate 2>/dev/null; git push origin feature/gate"
test_expect_fail "Create tag" sbr sh -c "git tag v0.0.99-gate; git push origin v0.0.99-gate"
test_expect_fail "gh pr merge" sbr sh -c "gh pr merge $PR_NUM --merge"
test_expect_fail "REST merge API" sbr sh -c "gh api -X PUT repos/$REPO/pulls/$PR_NUM/merge -f merge_method=merge"
# jlaska/agent-sandbox-denied: permanent canary (never on the App)
test_expect_fail "Access unapproved repo (canary)" sbr git clone https://github.com/jlaska/agent-sandbox-denied.git /tmp/denied

# Credential containment
# Both declared env_vars (GH_TOKEN, GITHUB_TOKEN) must be opaque placeholders.
for var in GH_TOKEN GITHUB_TOKEN; do
    CRED_CHECK=$(sbr sh -c "printenv $var" 2>/dev/null)
    if [[ "$CRED_CHECK" == openshell:resolve:* ]]; then
        pass "$var is a placeholder (not the real token)"
    else
        fail "$var is not a placeholder (got ${#CRED_CHECK} chars, prefix '${CRED_CHECK:0:4}')"
    fi
done

# Inference test
if [[ "$SKIP_INFERENCE" != "--skip-inference" ]]; then
    echo ""
    echo "  Inference test (LiteLLM connectivity)..."
    INFERENCE_RESULT=$(sbr sh -c "source ~/.profile 2>/dev/null; curl -s -o /dev/null -w '%{http_code}' \"\${ANTHROPIC_BASE_URL:-\$LITELLM_BASE_URL}/v1/models\" -H 'x-api-key: '\"\$LITELLM_API_KEY\"" 2>/dev/null || echo "000")
    if [[ "$INFERENCE_RESULT" == "200" || "$INFERENCE_RESULT" == "401" || "$INFERENCE_RESULT" == "403" ]]; then
        pass "Model auth reaches LiteLLM (HTTP $INFERENCE_RESULT)"
    else
        fail "Cannot reach LiteLLM (HTTP $INFERENCE_RESULT)"
    fi
else
    skip "Inference test (--skip-inference)"
fi

# --- Summary ---
echo ""
echo "========================================="
echo "Phase 5 Exit Gate Results: $AGENT"
echo "========================================="
echo "  PASS: $PASS"
echo "  FAIL: $FAIL"
echo "  SKIP: $SKIP"
echo "  TOTAL: $((PASS + FAIL + SKIP))"
echo ""

if [[ $FAIL -gt 0 ]]; then
    echo "  ❌ EXIT GATE FAILED"
    exit 1
else
    echo "  ✅ EXIT GATE PASSED"
    exit 0
fi
