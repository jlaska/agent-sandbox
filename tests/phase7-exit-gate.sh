#!/usr/bin/env bash
# Phase 7 exit gate -- per-harness images, mise toolchains, egress narrowing.
#
# OWNER-RUN end-to-end test (issue #41, section 7). Needs OpenShell >= 0.1.4, a
# running gateway, GitHub App credentials (Keychain) and, for some targets,
# inference credentials. Only the `version-gate` target runs without OpenShell.
#
# Usage:
#   ./tests/phase7-exit-gate.sh [options]
#   ./tests/phase7-exit-gate.sh --only version-gate        # no OpenShell needed
#   ./tests/phase7-exit-gate.sh --image claude=localhost/agent-claude:local \
#       --image pi=localhost/agent-pi:local --image opencode=localhost/agent-opencode:local
#   ./tests/phase7-exit-gate.sh --make-fixture /tmp/mise-fixture
#
# Run with --help for all options, targets, and the DoD -> check mapping.
#
# Prerequisites (except for `--only version-gate`):
#   - make build (bin/agent-run), and make images when using --image <h>=agent-<h>:local
#   - OpenShell >= 0.1.4 with the gateway running; podman (or docker) for the cache checks
#   - GitHub App Keychain entries configured; the App must be installed on --mise-repo and --plain-repo
#   - inference credentials exported for the targets you want (see --help)
#
# Exit status: 0 = no failures, 1 = at least one FAIL, 2 = usage/prerequisite error.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
AGENT_RUN="${AGENT_RUN:-${REPO_ROOT}/bin/agent-run}"
IMAGES_YAML="${REPO_ROOT}/openshell/images.yaml"
VERSION_SH="${REPO_ROOT}/openshell/images/version.sh"
FIXTURE_TOML="${SCRIPT_DIR}/images/fixtures/mise.toml"

MIN_OPENSHELL="0.1.4"
MISE_REPO="${MISE_REPO:-jlaska/agent-sandbox-mise-fixture}"
PLAIN_REPO="${PLAIN_REPO:-jlaska/agent-sandbox-test}"
ENGINE="${ENGINE:-}"
DRIVER=""
TIMEOUT=900          # seconds to wait for agent-run to reach "Launching harness"
HOLD_SECONDS=1800    # upper bound for how long a session keeps the harness alive
HOLD_ARGS_OVERRIDE=""
ONLY=""
SKIP_TARGETS=""
HARNESSES="claude,pi,opencode"
FROM_KEYCHAIN=false
KEEP_LOGS=false
MAKE_FIXTURE=""
IMAGE_CLAUDE="" IMAGE_PI="" IMAGE_OPENCODE=""
MISE_ALLOW_HOSTS=()

ALL_TARGETS="version-gate claude pi opencode-litellm opencode-anthropic plain-repo cache"

PASS=0
FAIL=0
SKIP=0
SKIP_REASONS=()
FAIL_NAMES=()

usage() {
    cat <<'USAGE'
Usage: phase7-exit-gate.sh [options]

Targets (select with --only / --skip / --harness):
  version-gate        old-OpenShell PATH-shim negative test (runs without OpenShell)
  claude              image + version + mise + narrowing for the claude harness
  pi                  ... for pi                          (needs LITELLM_API_KEY)
  opencode-litellm    ... for opencode --inference litellm
                                                          (needs LITELLM_API_KEY, LITELLM_BASE_URL)
  opencode-anthropic  ... for opencode --inference anthropic
                                                          (needs ANTHROPIC_API_KEY)
  plain-repo          D11 skip log for a repo without mise config
  cache               mise cache reuse and --fresh-cache (podman/docker volume)

Options:
  --only T[,T...]         run only these targets
  --skip T[,T...]         do not run these targets (use for missing credentials)
  --harness H[,H...]      harnesses to cover: claude,pi,opencode (default: all;
                          opencode covers both backends)
  --image H=REF           image for harness H (repeatable; default: images.yaml).
                          e.g. --image claude=localhost/agent-claude:local
  --mise-repo OWNER/REPO  fixture repo with a mise.toml mirroring
                          tests/images/fixtures/mise.toml (default: jlaska/agent-sandbox-mise-fixture)
  --plain-repo OWNER/REPO repo WITHOUT any mise config (default: jlaska/agent-sandbox-test)
  --engine podman|docker  container engine holding the cache volume (default: podman,
                          else docker). Passed to agent-run as --driver unless --driver is given.
  --driver D              pass --driver D to agent-run (docker|podman|kubernetes|vm)
  --mise-allow-host H     pass --mise-allow-host H to agent-run (repeatable; e.g. nodejs.org)
  --from-keychain         macOS: load LITELLM_API_KEY / LITELLM_BASE_URL from the Keychain
                          entries litellm-api-key / anthropic-base-url if unset
  --timeout SECS          per-session wait for agent-run to launch the harness (default 900)
  --hold-args "ARGS"      args after `--` that keep the harness alive reading stdin
                          (default: claude "-p", pi "-p", opencode "run")
  --keep-logs             keep the per-session agent-run logs (path printed at the end)
  --make-fixture DIR      create a LOCAL git repo mirroring the mise fixture, print how to
                          publish it, and exit (nothing is pushed)
  -h, --help              show this help

Credentials: targets whose inference credentials are not in the environment are SKIPPED
with a reason, never failed. GitHub App credentials are required for every target except
version-gate.

How sessions work: each session runs agent-run in the background with its stdin held
open, so the harness (e.g. `claude -p`) blocks after the egress has been narrowed. The
checks then probe the live sandbox with `openshell sandbox exec`, and closing stdin ends
the harness so agent-run's own cleanup runs (and is verified).

DoD -> named checks (a check is reported as [target] name):
  DoD-2  claude --version == Dockerfile pin    image-ref, engine-image-in-use, harness-version  [claude]
  DoD-3  pi / opencode (litellm, anthropic)    image-ref, engine-image-in-use, harness-version  [pi]
                                               [opencode-litellm] [opencode-anthropic];
                                               inference-auth probes the wave-3 auth risk
  DoD-4  mise tools installed / D11 skip log   mise-fixture-matches, mise-install-log,
                                               mise-tool-kubectl, mise-tool-precommit,
                                               mise-shims-on-path          [each harness target]
                                               mise-skip-log, mise-skip-no-install,
                                               mise-skip-narrowed          [plain-repo]
  DoD-5  egress narrowed                       narrow-log, narrow-no-bootstrap-rule,
                                               narrow-pypi-denied, narrow-npm-denied [each harness]
  DoD-6  cache reuse + --fresh-cache           cache-mounted, cache-volume-labelled,
                                               cache-cold-first-run, cache-reuse-warm,
                                               cache-reuse-marker, cache-fresh-clears [cache]
  (DoD-1/D1) old OpenShell refused             version-gate-old-refused, version-gate-hint,
                                               version-gate-no-mutation, version-gate-min-accepted
  also: cleanup-sandbox-deleted [each harness/plain-repo]

Exit status: 0 = no failures, 1 = at least one FAIL, 2 = usage/prerequisite error.
USAGE
}

# --- Argument parsing ---
while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help) usage; exit 0 ;;
        --only) ONLY="${2:?--only needs a value}"; shift 2 ;;
        --skip) SKIP_TARGETS="${2:?--skip needs a value}"; shift 2 ;;
        --harness) HARNESSES="${2:?--harness needs a value}"; shift 2 ;;
        --image)
            spec="${2:?--image needs HARNESS=REF}"
            case "$spec" in
                claude=?*) IMAGE_CLAUDE="${spec#claude=}" ;;
                pi=?*) IMAGE_PI="${spec#pi=}" ;;
                opencode=?*) IMAGE_OPENCODE="${spec#opencode=}" ;;
                *) echo "Invalid --image '$spec' (expected claude=REF, pi=REF or opencode=REF)" >&2; exit 2 ;;
            esac
            shift 2 ;;
        --mise-repo) MISE_REPO="${2:?--mise-repo needs a value}"; shift 2 ;;
        --plain-repo) PLAIN_REPO="${2:?--plain-repo needs a value}"; shift 2 ;;
        --engine) ENGINE="${2:?--engine needs a value}"; shift 2 ;;
        --driver) DRIVER="${2:?--driver needs a value}"; shift 2 ;;
        --mise-allow-host) MISE_ALLOW_HOSTS+=("${2:?--mise-allow-host needs a value}"); shift 2 ;;
        --from-keychain) FROM_KEYCHAIN=true; shift ;;
        --timeout) TIMEOUT="${2:?--timeout needs a value}"; shift 2 ;;
        --hold-args) HOLD_ARGS_OVERRIDE="${2:?--hold-args needs a value}"; shift 2 ;;
        --keep-logs) KEEP_LOGS=true; shift ;;
        --make-fixture) MAKE_FIXTURE="${2:?--make-fixture needs a directory}"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done

for t in ${ONLY//,/ } ${SKIP_TARGETS//,/ }; do
    case " $ALL_TARGETS " in
        *" $t "*) ;;
        *) echo "Unknown target: $t (targets: $ALL_TARGETS)" >&2; exit 2 ;;
    esac
done
for h in ${HARNESSES//,/ }; do
    case "$h" in
        claude|pi|opencode) ;;
        shell) echo "The shell harness was removed (use claude, pi or opencode)." >&2; exit 2 ;;
        *) echo "Unknown harness: $h (use claude, pi or opencode)" >&2; exit 2 ;;
    esac
done

# --- Fixture helper (local only) ---
make_fixture() {
    local dir="$1"
    if [[ -e "$dir" ]] && [[ -n "$(ls -A "$dir" 2>/dev/null)" ]]; then
        echo "Refusing to use non-empty directory: $dir" >&2
        return 2
    fi
    mkdir -p "$dir" || return 2
    cp "$FIXTURE_TOML" "$dir/mise.toml" || return 2
    cat >"$dir/README.md" <<'README'
# agent-sandbox mise fixture

Test fixture for agent-sandbox `tests/phase7-exit-gate.sh`.
`mise.toml` mirrors `tests/images/fixtures/mise.toml` in jlaska/agent-sandbox.
README
    (
        cd "$dir" || exit 1
        git init -q -b main &&
            git add mise.toml README.md &&
            git -c user.name="phase7 fixture" -c user.email="fixture@example.invalid" \
                -c commit.gpgsign=false commit -q -m "mise fixture"
    ) || return 2
    cat <<EOF
Created local fixture repo: $dir  (nothing was pushed)

To publish it (private repo) and make it reachable by agent-run:
  gh repo create <owner>/agent-sandbox-mise-fixture --private --source "$dir" --push
  # then add the repo to the jlaska-agent GitHub App installation
  # (Settings -> Applications -> jlaska-agent -> Configure -> Repository access)
  ./tests/phase7-exit-gate.sh --mise-repo <owner>/agent-sandbox-mise-fixture ...

The plain (no mise config) repo defaults to jlaska/agent-sandbox-test; it must contain
none of: mise.toml .mise.toml mise/config.toml .config/mise.toml .config/mise/config.toml .tool-versions
EOF
}

if [[ -n "$MAKE_FIXTURE" ]]; then
    make_fixture "$MAKE_FIXTURE"
    exit $?
fi

# --- Helpers ---
pass() { echo "  ✅ PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  ❌ FAIL: $1"; FAIL=$((FAIL + 1)); FAIL_NAMES+=("$1"); }
skip() { echo "  ⏭️  SKIP: $1"; SKIP=$((SKIP + 1)); SKIP_REASONS+=("$1"); }

keychain_get() { security find-generic-password -s "$1" -a "$USER" -w 2>/dev/null; }

contains_word() { case ",$1," in *",$2,"*) return 0 ;; *) return 1 ;; esac; }

# target_wanted NAME: honours --only / --skip.
target_wanted() {
    local only=",${ONLY// /,}," skp=",${SKIP_TARGETS// /,},"
    if [[ -n "$ONLY" && "$only" != *",$1,"* ]]; then return 1; fi
    [[ "$skp" != *",$1,"* ]]
}

harness_of() { case "$1" in opencode-*) echo opencode ;; *) echo "$1" ;; esac; }

harness_selected() { contains_word "$HARNESSES" "$(harness_of "$1")"; }

image_override() {
    case "$1" in
        claude) echo "$IMAGE_CLAUDE" ;;
        pi) echo "$IMAGE_PI" ;;
        opencode) echo "$IMAGE_OPENCODE" ;;
    esac
}

# Expected image ref for a harness: --image override, else images.yaml.
expected_image() {
    local h="$1" o
    o="$(image_override "$h")"
    if [[ -n "$o" ]]; then echo "$o"; return 0; fi
    awk -v h="$h" '
        $1 == h ":" { inblock = 1; next }
        inblock && $1 == "ref:" { print $2; exit }
        /^  [a-z]+:/ { inblock = 0 }
    ' "$IMAGES_YAML"
}

hold_args_for() {
    if [[ -n "$HOLD_ARGS_OVERRIDE" ]]; then echo "$HOLD_ARGS_OVERRIDE"; return; fi
    case "$1" in claude|pi) echo "-p" ;; opencode) echo "run" ;; esac
}

# Missing-credential reason for a target ("" when it can run).
missing_creds() {
    case "$1" in
        pi) [[ -n "${LITELLM_API_KEY:-}" ]] || echo "LITELLM_API_KEY not set" ;;
        opencode-litellm)
            if [[ -z "${LITELLM_API_KEY:-}" ]]; then echo "LITELLM_API_KEY not set"
            elif [[ -z "${LITELLM_BASE_URL:-}" ]]; then echo "LITELLM_BASE_URL not set"; fi ;;
        opencode-anthropic) [[ -n "${ANTHROPIC_API_KEY:-}" ]] || echo "ANTHROPIC_API_KEY not set" ;;
    esac
}

extra_args_for() { # target -> agent-run options (one per line)
    case "$1" in
        opencode-litellm) echo "--inference"; echo "litellm" ;;
        opencode-anthropic) echo "--inference"; echo "anthropic" ;;
    esac
}

# --- Session management ---
S_PID="" S_HOLD="" S_LOG="" S_SANDBOX="" S_ALIVE=0
SANDBOXES_SEEN=()
WORKDIR=""

# start_session LABEL HARNESS REPO [agent-run options...]
# Returns 0 when agent-run reached "Launching harness" (egress already narrowed).
start_session() {
    local label="$1" harness="$2" repo="$3"
    shift 3
    local fifo="$WORKDIR/$label.fifo" holdargs
    S_LOG="$WORKDIR/$label.log"
    S_SANDBOX="" S_ALIVE=0
    rm -f "$fifo"
    mkfifo "$fifo" || return 1
    read -r -a holdargs <<<"$(hold_args_for "$harness")"
    sleep "$HOLD_SECONDS" >"$fifo" &
    S_HOLD=$!
    "$AGENT_RUN" "$repo" "$harness" "$@" -- "${holdargs[@]}" <"$fifo" >"$S_LOG" 2>&1 &
    S_PID=$!

    local waited=0
    while [[ $waited -lt $TIMEOUT ]]; do
        grep -q 'Launching harness' "$S_LOG" 2>/dev/null && break
        kill -0 "$S_PID" 2>/dev/null || break
        sleep 2
        waited=$((waited + 2))
    done
    S_SANDBOX="$(sed -n 's/^\[agent-run\] Sandbox: *//p' "$S_LOG" | head -n1)"
    [[ -n "$S_SANDBOX" ]] && SANDBOXES_SEEN+=("$S_SANDBOX")
    if ! grep -q 'Launching harness' "$S_LOG" 2>/dev/null; then
        echo "     agent-run did not reach 'Launching harness' (log: $S_LOG); last lines:"
        tail -n 8 "$S_LOG" | sed 's/^/       | /'
        end_session
        return 1
    fi
    sleep 3
    if kill -0 "$S_PID" 2>/dev/null; then S_ALIVE=1; fi
    return 0
}

# end_session: close stdin (harness exits), wait for agent-run's own cleanup.
end_session() {
    [[ -n "$S_HOLD" ]] && { kill "$S_HOLD" 2>/dev/null; wait "$S_HOLD" 2>/dev/null; }
    local i=0
    while [[ $i -lt 60 ]] && kill -0 "$S_PID" 2>/dev/null; do sleep 2; i=$((i + 1)); done
    if kill -0 "$S_PID" 2>/dev/null; then
        echo "     agent-run did not exit after stdin closed; terminating (harness ignores EOF?)"
        kill "$S_PID" 2>/dev/null
        [[ -n "$S_SANDBOX" ]] && openshell sandbox delete "$S_SANDBOX" >/dev/null 2>&1
    fi
    wait "$S_PID" 2>/dev/null
    S_HOLD="" S_ALIVE=0
}

# sbx CMD...: run a command in the live session sandbox.
sbx() { openshell sandbox exec -n "$S_SANDBOX" -- "$@"; }

log_has() { grep -qF -- "$1" "$S_LOG"; }

session_alive_or_fail() { # label
    if [[ $S_ALIVE -ne 1 ]]; then
        fail "[$1] probe-session: harness exited right after launch, sandbox already cleaned up (hold args '$(hold_args_for "$(harness_of "$1")")' do not keep it alive; try --hold-args; log: $S_LOG)"
        return 1
    fi
    return 0
}

check_sandbox_deleted() { # label
    if openshell sandbox list 2>/dev/null | grep -qF -- "$S_SANDBOX"; then
        fail "[$1] cleanup-sandbox-deleted: $S_SANDBOX still listed after agent-run exit"
    else
        pass "[$1] cleanup-sandbox-deleted"
    fi
}

# --- Target: version-gate ---
run_version_gate() {
    echo ""
    echo "--- [version-gate] old-OpenShell PATH shim ---"
    if [[ ! -x "$AGENT_RUN" ]]; then
        fail "[version-gate] agent-run binary missing at $AGENT_RUN (run: make build)"
        return
    fi
    local shim="$WORKDIR/shim" shimlog="$WORKDIR/shim.log" out rc
    mkdir -p "$shim"
    cat >"$shim/openshell" <<'SHIM'
#!/bin/sh
# Fake openshell for the version-gate test. Records every invocation.
echo "$*" >> "$SHIM_LOG"
case "$1" in
    --version) echo "openshell $SHIM_VERSION" ;;
    *) echo "shim: unsupported command: $*" >&2; exit 1 ;;
esac
SHIM
    chmod +x "$shim/openshell"

    : >"$shimlog"
    out="$(PATH="$shim:$PATH" SHIM_VERSION=0.0.113 SHIM_LOG="$shimlog" "$AGENT_RUN" "$PLAIN_REPO" claude </dev/null 2>&1)"
    rc=$?
    if [[ $rc -ne 0 && "$out" == *"too old"* && "$out" == *"requires >= ${MIN_OPENSHELL}"* ]]; then
        pass "[version-gate] version-gate-old-refused (openshell 0.0.113 -> exit $rc)"
    else
        fail "[version-gate] version-gate-old-refused (rc=$rc): $(printf '%s' "$out" | tail -n 3 | tr '\n' ' ')"
    fi
    if [[ "$out" == *"uninstall the old OpenShell"* && "$out" == *"v${MIN_OPENSHELL}"* ]]; then
        pass "[version-gate] version-gate-hint (upgrade hint printed)"
    else
        fail "[version-gate] version-gate-hint (no upgrade hint in output)"
    fi
    # Only --version (and cleanup of the never-created sandbox) may reach the old CLI.
    if grep -Eq '(create|import|policy|exec|clone|update)' "$shimlog"; then
        fail "[version-gate] version-gate-no-mutation: old openshell was driven: $(tr '\n' ';' <"$shimlog")"
    else
        pass "[version-gate] version-gate-no-mutation (shim saw: $(tr '\n' ';' <"$shimlog"))"
    fi

    : >"$shimlog"
    out="$(PATH="$shim:$PATH" SHIM_VERSION=0.1.3 SHIM_LOG="$shimlog" "$AGENT_RUN" "$PLAIN_REPO" claude </dev/null 2>&1)"
    if [[ "$out" == *"too old"* ]]; then
        pass "[version-gate] version-gate-old-refused (0.1.3 refused)"
    else
        fail "[version-gate] version-gate-old-refused (0.1.3 was not refused)"
    fi

    for v in "$MIN_OPENSHELL" "${MIN_OPENSHELL}-dev.6+g2bf9969ab"; do
        : >"$shimlog"
        out="$(PATH="$shim:$PATH" SHIM_VERSION="$v" SHIM_LOG="$shimlog" "$AGENT_RUN" "$PLAIN_REPO" claude </dev/null 2>&1)"
        # It still fails afterwards (the shim has no gateway), but not on the version gate.
        if [[ "$out" != *"too old"* && "$out" == *"OpenShell:  ${v%%-*}"* ]]; then
            pass "[version-gate] version-gate-min-accepted ($v passes the gate)"
        else
            fail "[version-gate] version-gate-min-accepted ($v rejected by the gate)"
        fi
    done
}

# --- Target: a harness on the mise fixture repo ---
run_harness_target() {
    local target="$1" harness expected pin reason
    harness="$(harness_of "$target")"
    echo ""
    echo "--- [$target] image, version, mise, narrowing ---"
    if reason="$(missing_creds "$target")" && [[ -n "$reason" ]]; then
        skip "[$target] all checks ($reason)"
        return
    fi
    expected="$(expected_image "$harness")"
    if [[ -z "$expected" || "$expected" =~ @sha256:0{64}$ ]]; then
        fail "[$target] image-ref: image for $harness is unpublished (zero digest in images.yaml); build it and pass --image $harness=agent-$harness:local"
        return
    fi
    pin="$("$VERSION_SH" "$harness" 2>/dev/null)"
    if [[ -z "$pin" ]]; then
        fail "[$target] harness-version: could not read the Dockerfile pin via $VERSION_SH"
        return
    fi

    local args=() a
    args+=(--image "$expected")
    while IFS= read -r a; do [[ -n "$a" ]] && args+=("$a"); done < <(extra_args_for "$target")
    [[ -n "$DRIVER" ]] && args+=(--driver "$DRIVER")
    for a in ${MISE_ALLOW_HOSTS[@]+"${MISE_ALLOW_HOSTS[@]}"}; do args+=(--mise-allow-host "$a"); done

    if ! start_session "$target" "$harness" "$MISE_REPO" "${args[@]}"; then
        fail "[$target] session: agent-run failed before launching the harness (see log above)"
        return
    fi

    # image-ref: what agent-run resolved and passed to `sandbox create --from`
    local logged
    logged="$(sed -n 's/^\[agent-run\] Image: *//p' "$S_LOG" | head -n1)"
    if [[ "$logged" == "$expected" ]]; then
        pass "[$target] image-ref ($logged)"
    else
        fail "[$target] image-ref: agent-run used '$logged', expected '$expected'"
    fi

    # narrowing evidence in the agent-run log
    if log_has 'Egress narrowed: mise_bootstrap removed'; then
        pass "[$target] narrow-log"
    else
        fail "[$target] narrow-log: no 'Egress narrowed' line before launch"
    fi
    if log_has 'mise: install finished'; then
        pass "[$target] mise-install-log ($(grep -F 'mise: install finished' "$S_LOG" | head -n1 | sed 's/^\[agent-run\] //'))"
    else
        fail "[$target] mise-install-log: no 'mise: install finished' line (tool hosts missing from the allowlist? try --mise-allow-host)"
    fi

    if session_alive_or_fail "$target"; then
        probe_engine_image "$target" "$expected"
        probe_version "$target" "$harness" "$pin"
        probe_mise "$target"
        probe_narrowing "$target"
        probe_inference "$target"
    fi

    end_session
    check_sandbox_deleted "$target"
}

probe_engine_image() { # target expected-ref
    local target="$1" ref="$2" cid have want
    if [[ -z "$ENGINE" ]] || ! command -v "$ENGINE" >/dev/null 2>&1; then
        skip "[$target] engine-image-in-use (no container engine on PATH)"
        return
    fi
    cid="$("$ENGINE" ps -a --filter "name=$S_SANDBOX" --format '{{.ID}}' 2>/dev/null | head -n1)"
    if [[ -z "$cid" ]]; then
        skip "[$target] engine-image-in-use (no $ENGINE container named like $S_SANDBOX; driver may not be $ENGINE)"
        return
    fi
    have="$("$ENGINE" inspect --format '{{.Image}}' "$cid" 2>/dev/null | sed 's/^sha256://')"
    want="$("$ENGINE" image inspect --format '{{.Id}}' "$ref" 2>/dev/null | sed 's/^sha256://')"
    if [[ -n "$have" && "$have" == "$want" ]]; then
        pass "[$target] engine-image-in-use (container $cid runs image ${want:0:12} = $ref)"
    else
        fail "[$target] engine-image-in-use: container image '${have:0:12}' != '$ref' (${want:0:12})"
    fi
}

probe_version() { # target harness pin
    local target="$1" harness="$2" pin="$3" out
    out="$(sbx "$harness" --version 2>&1 | head -n 3)"
    if [[ "$out" =~ (^|[^0-9.])${pin//./\\.}([^0-9]|$) ]]; then
        pass "[$target] harness-version ($harness --version = $(printf '%s' "$out" | head -n1); pin $pin)"
    else
        fail "[$target] harness-version: '$harness --version' printed '$(printf '%s' "$out" | head -n1)', Dockerfile pin is $pin"
    fi
}

probe_mise() { # target
    local target="$1" kver pver out
    kver="$(sed -n 's/^kubectl *= *"\(.*\)".*/\1/p' "$FIXTURE_TOML")"
    pver="$(sed -n 's/^"pipx:pre-commit" *= *"\(.*\)".*/\1/p' "$FIXTURE_TOML")"

    if [[ "$(sbx cat /sandbox/repo/mise.toml 2>/dev/null)" == "$(cat "$FIXTURE_TOML")" ]]; then
        pass "[$target] mise-fixture-matches (repo mise.toml == tests/images/fixtures/mise.toml)"
    else
        fail "[$target] mise-fixture-matches: $MISE_REPO:mise.toml differs from tests/images/fixtures/mise.toml (update the fixture repo)"
    fi
    out="$(sbx bash -c 'source ~/.profile 2>/dev/null; cd /sandbox/repo && mise exec -- kubectl version --client' 2>&1)"
    if [[ -n "$kver" && "$out" == *"$kver"* ]]; then
        pass "[$target] mise-tool-kubectl (kubectl $kver)"
    else
        fail "[$target] mise-tool-kubectl: expected $kver, got: $(printf '%s' "$out" | head -n2 | tr '\n' ' ')"
    fi
    out="$(sbx bash -c 'source ~/.profile 2>/dev/null; cd /sandbox/repo && mise exec -- pre-commit --version' 2>&1)"
    if [[ -n "$pver" && "$out" == *"$pver"* ]]; then
        pass "[$target] mise-tool-precommit (pre-commit $pver; pipx via uv reached pypi during bootstrap)"
    else
        fail "[$target] mise-tool-precommit: expected $pver, got: $(printf '%s' "$out" | head -n2 | tr '\n' ' ')"
    fi
    out="$(sbx bash -c 'source ~/.profile 2>/dev/null; cd /sandbox/repo && kubectl version --client' 2>&1)"
    if [[ -n "$kver" && "$out" == *"$kver"* ]]; then
        pass "[$target] mise-shims-on-path (kubectl resolves via shims without 'mise exec')"
    else
        fail "[$target] mise-shims-on-path: bare kubectl did not run: $(printf '%s' "$out" | head -n2 | tr '\n' ' ')"
    fi
}

# probe_denied TARGET CHECKNAME HOST: HTTPS to HOST must not succeed from the sandbox.
probe_denied() {
    local target="$1" name="$2" host="$3" out code
    out="$(sbx sh -c "curl -sS -m 20 -o /dev/null -w 'http=%{http_code}' https://${host}/ 2>&1; echo \" rc=\$?\"" 2>&1)"
    code="$(printf '%s' "$out" | sed -n 's/.*http=\([0-9]*\).*/\1/p' | head -n1)"
    if [[ "$out" == *"not found"* ]]; then
        fail "[$target] $name: probe could not run curl in the sandbox: $out"
    elif [[ -z "$code" ]]; then
        fail "[$target] $name: probe output not understood: $out"
    elif [[ "$code" =~ ^[23] ]]; then
        fail "[$target] $name: $host reachable after narrowing (HTTP $code)"
    else
        pass "[$target] $name ($host denied: HTTP $code)"
    fi
}

probe_narrowing() { # target
    local target="$1" doc
    doc="$(openshell policy get "$S_SANDBOX" --full -o json 2>&1)"
    if [[ "$doc" != *network_policies* ]]; then
        fail "[$target] narrow-no-bootstrap-rule: unexpected 'policy get --full -o json' output: $(printf '%s' "$doc" | head -n2 | tr '\n' ' ')"
    elif [[ "$doc" == *mise_bootstrap* ]]; then
        fail "[$target] narrow-no-bootstrap-rule: mise_bootstrap still in the active policy"
    else
        pass "[$target] narrow-no-bootstrap-rule"
    fi
    probe_denied "$target" narrow-pypi-denied pypi.org
    probe_denied "$target" narrow-npm-denied registry.npmjs.org
}

# Wave-3 runtime risk: do inference credentials work through the proxy placeholder swap?
probe_inference() { # target
    local target="$1" bearer xkey hdr
    case "$target" in
        pi|opencode-litellm)
            local base="${LITELLM_BASE_URL:-}"
            if [[ -z "$base" ]]; then
                skip "[$target] inference-auth (LITELLM_BASE_URL not set on the host)"
                return
            fi
            bearer="$(sbx bash -c "source ~/.profile 2>/dev/null; curl -s -m 30 -o /dev/null -w '%{http_code}' -H \"Authorization: Bearer \$LITELLM_API_KEY\" '${base%/}/v1/models'" 2>&1)"
            xkey="$(sbx bash -c "source ~/.profile 2>/dev/null; curl -s -m 30 -o /dev/null -w '%{http_code}' -H \"x-api-key: \$LITELLM_API_KEY\" '${base%/}/v1/models'" 2>&1)"
            if [[ "$bearer" == 200 ]]; then
                pass "[$target] inference-auth (Authorization: Bearer placeholder swapped, HTTP 200)"
            elif [[ "$xkey" == 200 ]]; then
                fail "[$target] inference-auth: x-api-key works (200) but Authorization: Bearer gives $bearer; opencode sends Bearer (wave-3 risk 3)"
            else
                fail "[$target] inference-auth: Bearer=$bearer x-api-key=$xkey (expected 200); check LITELLM_BASE_URL/key and the litellm profile"
            fi
            ;;
        opencode-anthropic)
            # shellcheck disable=SC2016  # $ANTHROPIC_API_KEY must expand inside the sandbox
            hdr="$(sbx bash -c 'source ~/.profile 2>/dev/null; curl -s -m 30 -o /dev/null -w "%{http_code}" -H "x-api-key: $ANTHROPIC_API_KEY" -H "anthropic-version: 2023-06-01" https://api.anthropic.com/v1/models' 2>&1)"
            if [[ "$hdr" == 200 ]]; then
                pass "[$target] inference-auth (api.anthropic.com accepts the swapped key, HTTP 200)"
            else
                fail "[$target] inference-auth: api.anthropic.com/v1/models returned $hdr (expected 200)"
            fi
            ;;
        claude)
            skip "[$target] inference-auth (claude is covered by phase5-exit-gate.sh claude)"
            ;;
    esac
}

# --- Target: plain repo (D11 skip log) ---
run_plain_repo() {
    local target="$1" harness="$2" expected args=() a
    echo ""
    echo "--- [plain-repo] no mise config -> skip log (harness: $harness) ---"
    expected="$(expected_image "$harness")"
    if [[ -z "$expected" || "$expected" =~ @sha256:0{64}$ ]]; then
        fail "[plain-repo] mise-skip-log: image for $harness is unpublished; pass --image $harness=agent-$harness:local"
        return
    fi
    args+=(--image "$expected")
    while IFS= read -r a; do [[ -n "$a" ]] && args+=("$a"); done < <(extra_args_for "$target")
    [[ -n "$DRIVER" ]] && args+=(--driver "$DRIVER")
    if ! start_session plain-repo "$harness" "$PLAIN_REPO" "${args[@]}"; then
        fail "[plain-repo] session: agent-run failed before launching the harness"
        return
    fi
    if log_has 'mise: config found'; then
        fail "[plain-repo] mise-skip-log: $PLAIN_REPO has a mise config; pick a repo without one via --plain-repo"
    elif log_has 'no mise config found in /sandbox/repo (looked for:' && log_has 'skipping toolchain install'; then
        pass "[plain-repo] mise-skip-log ($(grep -F 'no mise config found' "$S_LOG" | head -n1 | sed 's/^\[agent-run\] //' | cut -c1-110)...)"
    else
        fail "[plain-repo] mise-skip-log: skip message (D11) not found in $S_LOG"
    fi
    if log_has 'mise: installing toolchain'; then
        fail "[plain-repo] mise-skip-no-install: mise install ran for a repo without config"
    else
        pass "[plain-repo] mise-skip-no-install"
    fi
    if log_has 'Egress narrowed: mise_bootstrap removed'; then
        pass "[plain-repo] mise-skip-narrowed (narrowing still ran with mise skipped)"
    else
        fail "[plain-repo] mise-skip-narrowed: narrowing did not run when mise was skipped"
    fi
    end_session
    check_sandbox_deleted plain-repo
}

# --- Target: cache ---
cache_volume_name() {
    local name
    name="$("$AGENT_RUN" --diag "$MISE_REPO" claude --driver "$ENGINE" 2>/dev/null | sed -n 's/^ *mise cache volume: *\([^ ]*\).*/\1/p' | head -n1)"
    if [[ -z "$name" ]]; then
        name="mise-cache-$(printf '%s' "${MISE_REPO%%/*}-${MISE_REPO#*/}" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-\n' '-')"
    fi
    echo "$name"
}

run_cache() {
    local target="$1" harness="$2" expected vol args=() a before after
    echo ""
    echo "--- [cache] reuse and --fresh-cache (harness: $harness, engine: ${ENGINE:-none}) ---"
    if [[ -z "$ENGINE" ]] || ! command -v "$ENGINE" >/dev/null 2>&1; then
        skip "[cache] all checks (no podman/docker found; use --engine)"
        return
    fi
    expected="$(expected_image "$harness")"
    if [[ -z "$expected" || "$expected" =~ @sha256:0{64}$ ]]; then
        fail "[cache] cache-mounted: image for $harness is unpublished; pass --image $harness=agent-$harness:local"
        return
    fi
    args+=(--image "$expected" --driver "${DRIVER:-$ENGINE}")
    while IFS= read -r a; do [[ -n "$a" ]] && args+=("$a"); done < <(extra_args_for "$target")
    for a in ${MISE_ALLOW_HOSTS[@]+"${MISE_ALLOW_HOSTS[@]}"}; do args+=(--mise-allow-host "$a"); done

    vol="$(cache_volume_name)"
    echo "  cache volume: $vol (removing any existing copy first)"
    "$ENGINE" volume rm -f "$vol" >/dev/null 2>&1

    # Run 1: cold
    start_session cache-1 "$harness" "$MISE_REPO" "${args[@]}" || { fail "[cache] cache-cold-first-run: session failed"; return; }
    if log_has "mise cache: volume $vol"; then
        pass "[cache] cache-mounted (volume $vol attached)"
    else
        fail "[cache] cache-mounted: no 'mise cache: volume $vol' line: $(grep -F 'mise cache' "$S_LOG" | head -n2 | tr '\n' ' ')"
    fi
    if log_has '(cache: cold)'; then
        pass "[cache] cache-cold-first-run (cache: cold)"
    else
        fail "[cache] cache-cold-first-run: expected '(cache: cold)': $(grep -F 'installing toolchain' "$S_LOG" | head -n1)"
    fi
    before="$(sed -n 's/.*install finished in \([^ ]*\).*/\1/p' "$S_LOG" | head -n1)"
    if [[ $S_ALIVE -eq 1 ]]; then
        sbx sh -c 'mkdir -p /sandbox/.mise-cache/data && echo gate > /sandbox/.mise-cache/data/gate-marker' >/dev/null 2>&1
    fi
    end_session
    if "$ENGINE" volume inspect --format '{{json .Labels}}' "$vol" 2>/dev/null | grep -q 'openshell.ai/sandbox-attachable'; then
        pass "[cache] cache-volume-labelled ($ENGINE volume $vol carries the attachable labels)"
    else
        fail "[cache] cache-volume-labelled: $ENGINE volume $vol missing or unlabelled"
    fi

    # Run 2: warm
    start_session cache-2 "$harness" "$MISE_REPO" "${args[@]}" || { fail "[cache] cache-reuse-warm: session failed"; return; }
    after="$(sed -n 's/.*install finished in \([^ ]*\).*/\1/p' "$S_LOG" | head -n1)"
    if log_has '(cache: warm'; then
        pass "[cache] cache-reuse-warm ($(grep -oF '(cache: warm' "$S_LOG" | head -n1) ...; install ${before:-?} cold -> ${after:-?} warm)"
    else
        fail "[cache] cache-reuse-warm: expected '(cache: warm ...)': $(grep -F 'installing toolchain' "$S_LOG" | head -n1)"
    fi
    if [[ $S_ALIVE -eq 1 ]]; then
        if [[ "$(sbx cat /sandbox/.mise-cache/data/gate-marker 2>/dev/null)" == gate ]]; then
            pass "[cache] cache-reuse-marker (file written in run 1 is present in run 2)"
        else
            fail "[cache] cache-reuse-marker: marker from run 1 missing; the volume was not reused"
        fi
    else
        fail "[cache] cache-reuse-marker: harness exited early, cannot probe (log: $S_LOG)"
    fi
    end_session

    # Run 3: --fresh-cache
    start_session cache-3 "$harness" "$MISE_REPO" "${args[@]}" --fresh-cache || { fail "[cache] cache-fresh-clears: session failed"; return; }
    if log_has '--fresh-cache: clearing' && log_has 'cold (cleared by --fresh-cache)'; then
        pass "[cache] cache-fresh-clears (agent-run cleared the cache before install)"
    else
        fail "[cache] cache-fresh-clears: no clearing log lines: $(grep -F 'fresh-cache' "$S_LOG" | head -n2 | tr '\n' ' ')"
    fi
    if [[ $S_ALIVE -eq 1 ]]; then
        if sbx test -e /sandbox/.mise-cache/data/gate-marker 2>/dev/null; then
            fail "[cache] cache-fresh-clears: marker in data/ survived --fresh-cache"
        else
            pass "[cache] cache-fresh-clears (marker in data/ removed, tools reinstalled)"
        fi
    fi
    end_session
    echo "  (cache volume $vol left in place; remove with: $ENGINE volume rm $vol)"
}

# --- Cleanup ---
# shellcheck disable=SC2329  # invoked via trap
cleanup() {
    [[ -n "$S_HOLD" ]] && kill "$S_HOLD" 2>/dev/null
    [[ -n "$S_PID" ]] && kill "$S_PID" 2>/dev/null
    if command -v openshell >/dev/null 2>&1 && [[ ${#SANDBOXES_SEEN[@]} -gt 0 ]]; then
        local sb
        for sb in "${SANDBOXES_SEEN[@]}"; do
            if openshell sandbox list 2>/dev/null | grep -qF -- "$sb"; then
                openshell sandbox delete "$sb" >/dev/null 2>&1
            fi
        done
    fi
    if [[ -n "$WORKDIR" ]]; then
        if $KEEP_LOGS; then echo "Logs kept in: $WORKDIR"; else rm -rf "$WORKDIR"; fi
    fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# --- Prerequisites ---
needs_openshell=false
for t in claude pi opencode-litellm opencode-anthropic plain-repo cache; do
    if target_wanted "$t"; then needs_openshell=true; fi
done

if [[ ! -x "$AGENT_RUN" ]]; then
    echo "ERROR: $AGENT_RUN not found or not executable (run: make build)" >&2
    exit 2
fi
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/phase7-gate.XXXXXX")" || exit 2

if $FROM_KEYCHAIN; then
    if [[ -z "${LITELLM_API_KEY:-}" ]]; then LITELLM_API_KEY="$(keychain_get litellm-api-key)" || true; fi
    if [[ -z "${LITELLM_BASE_URL:-}" ]]; then LITELLM_BASE_URL="$(keychain_get anthropic-base-url)" || true; fi
    export LITELLM_API_KEY LITELLM_BASE_URL
fi

if [[ -z "$ENGINE" ]]; then
    if command -v podman >/dev/null 2>&1; then ENGINE=podman
    elif command -v docker >/dev/null 2>&1; then ENGINE=docker; fi
fi

if $needs_openshell; then
    if ! command -v openshell >/dev/null 2>&1; then
        echo "ERROR: openshell not found on PATH (needed for everything except --only version-gate)" >&2
        exit 2
    fi
    osv="$(openshell --version 2>&1 | head -n1)"
    if [[ "$osv" =~ ([0-9]+)\.([0-9]+)\.([0-9]+) ]]; then
        have=$(( BASH_REMATCH[1] * 1000000 + BASH_REMATCH[2] * 1000 + BASH_REMATCH[3] ))
        IFS=. read -r mj mn pt <<<"$MIN_OPENSHELL"
        want=$(( mj * 1000000 + mn * 1000 + pt ))
        if [[ $have -lt $want ]]; then
            echo "ERROR: $osv is older than $MIN_OPENSHELL; install OpenShell $MIN_OPENSHELL or newer (see issue #41 section 7.1)" >&2
            exit 2
        fi
    else
        echo "ERROR: cannot parse 'openshell --version': $osv" >&2
        exit 2
    fi
    if ! openshell status >/dev/null 2>&1; then
        echo "ERROR: OpenShell gateway is not running (openshell status failed)" >&2
        exit 2
    fi
fi

echo "========================================="
echo "Phase 7 Exit Gate"
echo "  agent-run:  $AGENT_RUN"
echo "  harnesses:  $HARNESSES"
echo "  mise repo:  $MISE_REPO   plain repo: $PLAIN_REPO"
echo "========================================="

# --- Run targets ---
first_target=""
if target_wanted version-gate; then run_version_gate; fi

for t in claude pi opencode-litellm opencode-anthropic; do
    if ! harness_selected "$t" || ! target_wanted "$t"; then continue; fi
    run_harness_target "$t"
    if [[ -z "$first_target" && -z "$(missing_creds "$t")" ]]; then first_target="$t"; fi
done

# plain-repo and cache reuse the first runnable harness target (default: claude)
if [[ -z "$first_target" ]]; then
    for t in claude pi opencode-litellm opencode-anthropic; do
        if harness_selected "$t" && [[ -z "$(missing_creds "$t")" ]]; then first_target="$t"; break; fi
    done
fi
if target_wanted plain-repo; then
    if [[ -n "$first_target" ]]; then
        run_plain_repo "$first_target" "$(harness_of "$first_target")"
    else
        skip "[plain-repo] all checks (no runnable harness selected)"
    fi
fi
if target_wanted cache; then
    if [[ -n "$first_target" ]]; then
        run_cache "$first_target" "$(harness_of "$first_target")"
    else
        skip "[cache] all checks (no runnable harness selected)"
    fi
fi

# --- Summary ---
echo ""
echo "========================================="
echo "Phase 7 Exit Gate Results"
echo "========================================="
echo "  PASS: $PASS"
echo "  FAIL: $FAIL"
echo "  SKIP: $SKIP"
echo "  TOTAL: $((PASS + FAIL + SKIP))"
if [[ $SKIP -gt 0 ]]; then
    echo ""
    echo "  Skipped:"
    printf '    - %s\n' "${SKIP_REASONS[@]}"
fi
if [[ $FAIL -gt 0 ]]; then
    echo ""
    echo "  Failed:"
    printf '    - %s\n' "${FAIL_NAMES[@]}"
    $KEEP_LOGS || echo "  (re-run with --keep-logs to keep the agent-run session logs)"
fi
echo ""

if [[ $FAIL -gt 0 ]]; then
    echo "  ❌ EXIT GATE FAILED"
    exit 1
else
    echo "  ✅ EXIT GATE PASSED"
    exit 0
fi
