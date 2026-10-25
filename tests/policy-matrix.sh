#!/usr/bin/env bash
# Policy matrix -- one allow/deny verdict per (rule, binary, method, path).
#
# OWNER-RUN integration test. Starts ONE agent-run session (or attaches to an
# existing sandbox), waits until egress is narrowed, then runs every case in
# tests/policy-matrix/cases.sh inside the sandbox and classifies the result by
# who answered:
#   deny   the sandbox refused connect() with EACCES (transparent interception,
#          what `openshell sandbox exec` uses), the proxy refused the CONNECT,
#          or the proxy answered with its own policy_denied / ssrf_denied 403 body
#   allow  any other HTTP status: the request reached the upstream server
#   error  anything else (unresolvable host, timeout, plain connection
#          refused); never counted as a deny, always a failure
# An upstream 401/404/422 is therefore "allow": the policy let it through.
# This is what the older exit gates got wrong by grepping for "error|403".
#
# Usage:
#   ./tests/policy-matrix.sh                         # claude session on jlaska/agent-sandbox
#   ./tests/policy-matrix.sh --expect hardened       # verdicts against the hardened baseline
#   ./tests/policy-matrix.sh --sandbox NAME          # probe an existing, already narrowed sandbox
#   ./tests/policy-matrix.sh --only gh-rest-merge,node-sentry
#   ./tests/policy-matrix.sh --list                  # print the cases, run nothing
#   ./tests/policy-matrix.sh --self-test             # check the classifier, no OpenShell
#
# Prerequisites (except --list / --self-test): make build, OpenShell >= 0.1.4 with
# the gateway running, GitHub App Keychain entries (the App installed on --repo),
# and the harness's inference credentials.
#
# Exit status: 0 = no FAIL/ERROR, 1 = at least one, 2 = usage/prerequisite error.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
AGENT_RUN="${AGENT_RUN:-${REPO_ROOT}/bin/agent-run}"
CASES_FILE="${SCRIPT_DIR}/policy-matrix/cases.sh"
PROBE_FILE="${SCRIPT_DIR}/policy-matrix/probe.sh"
PROBE_IN_SANDBOX="/tmp/policy-matrix-probe.sh"

REPO="${REPO:-jlaska/agent-sandbox}"
HARNESS="claude"
IMAGE=""
SANDBOX=""
EXPECT="current"
ONLY=""
TIMEOUT=900
HOLD_SECONDS=1800
KEEP_LOGS=false
TSV_OUT=""
MODE="run"
AGENT_ARGS=()

usage() { sed -n '2,/^$/s/^# \{0,1\}//p' "${BASH_SOURCE[0]}"; cat <<'USAGE'
Options:
  --repo OWNER/REPO     target repo for the session and {OWNER}/{REPO} (default: jlaska/agent-sandbox)
  --harness H           claude | pi | opencode (default: claude)
  --image REF           harness image (default: images.yaml via agent-run)
  --sandbox NAME        probe this existing sandbox instead of starting a session
  --expect PROFILE      current | hardened: which expectation column is the verdict (default: current)
  --only ID[,ID...]     run only these case IDs
  --tsv FILE            also write one tab-separated result row per case
  --timeout SECS        wait for agent-run to launch the harness (default 900)
  --keep-logs           keep the agent-run log
  --list                print the cases and exit
  --self-test           run the classifier against fixed probe lines and exit
  -- ARGS...            extra agent-run options (e.g. -- --inference litellm)
  -h, --help            show this help
USAGE
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help) usage; exit 0 ;;
        --repo) REPO="${2:?--repo needs a value}"; shift 2 ;;
        --harness) HARNESS="${2:?--harness needs a value}"; shift 2 ;;
        --image) IMAGE="${2:?--image needs a value}"; shift 2 ;;
        --sandbox) SANDBOX="${2:?--sandbox needs a value}"; shift 2 ;;
        --expect) EXPECT="${2:?--expect needs a value}"; shift 2 ;;
        --only) ONLY="${2:?--only needs a value}"; shift 2 ;;
        --tsv) TSV_OUT="${2:?--tsv needs a file}"; shift 2 ;;
        --timeout) TIMEOUT="${2:?--timeout needs a value}"; shift 2 ;;
        --keep-logs) KEEP_LOGS=true; shift ;;
        --list) MODE="list"; shift ;;
        --self-test) MODE="self-test"; shift ;;
        --) shift; AGENT_ARGS=("$@"); break ;;
        *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done
case "$EXPECT" in current|hardened) ;; *) echo "--expect must be current or hardened" >&2; exit 2 ;; esac
case "$HARNESS" in claude|pi|opencode) ;; *) echo "Unknown harness: $HARNESS" >&2; exit 2 ;; esac
if [[ "$REPO" != */* ]]; then echo "--repo must be OWNER/REPO" >&2; exit 2; fi
OWNER="${REPO%%/*}" REPONAME="${REPO#*/}"

# --- Cases (bash 3.2: parallel indexed arrays, no associative arrays) ---
C_ID=() C_VIA=() C_METHOD=() C_URL=() C_BODY=() C_CUR=() C_HARD=() C_WHY=()
pm_case() {
    if [[ $# -ne 8 ]]; then echo "cases.sh: pm_case needs 8 fields, got $#: $*" >&2; exit 2; fi
    local u="${4//\{OWNER\}/$OWNER}"
    C_ID+=("$1") C_VIA+=("$2") C_METHOD+=("$3") C_URL+=("${u//\{REPO\}/$REPONAME}")
    C_BODY+=("$5") C_CUR+=("$6") C_HARD+=("$7") C_WHY+=("$8")
}
# shellcheck source=tests/policy-matrix/cases.sh
source "$CASES_FILE"

validate_cases() {
    local i j bad=0
    for ((i = 0; i < ${#C_ID[@]}; i++)); do
        case "${C_VIA[$i]}" in curl|node|python|git|gh|fs) ;; *) echo "case ${C_ID[$i]}: bad via '${C_VIA[$i]}'" >&2; bad=1 ;; esac
        case "${C_CUR[$i]}/${C_HARD[$i]}" in
            allow/allow|allow/deny|allow/record|deny/allow|deny/deny|deny/record|record/allow|record/deny|record/record) ;;
            *) echo "case ${C_ID[$i]}: bad expectation '${C_CUR[$i]}/${C_HARD[$i]}'" >&2; bad=1 ;;
        esac
        for ((j = 0; j < i; j++)); do
            [[ "${C_ID[$j]}" == "${C_ID[$i]}" ]] && { echo "duplicate case id ${C_ID[$i]}" >&2; bad=1; }
        done
    done
    if [[ -n "$ONLY" ]]; then
        local want found
        for want in ${ONLY//,/ }; do
            found=0
            for ((i = 0; i < ${#C_ID[@]}; i++)); do [[ "${C_ID[$i]}" == "$want" ]] && found=1; done
            [[ $found -eq 1 ]] || { echo "--only: unknown case id '$want'" >&2; bad=1; }
        done
    fi
    return $bad
}

selected() { [[ -z "$ONLY" ]] || [[ ",$ONLY," == *",$1,"* ]]; }

# classify "PM ..." -> prints "DECISION<TAB>detail"; DECISION is allow, deny or error.
classify() {
    local line="$1" kv http=000 connect=000 rc="" denied=- policy=- l4=- dns=- err="" fsrc=""
    case "$line" in
        "PM fs "*)
            for kv in $line; do case "$kv" in rc=*) fsrc="${kv#rc=}" ;; esac; done
            err="${line#*err=}"; [[ "$err" == "$line" ]] && err=""
            if [[ "$fsrc" == 0 ]]; then printf 'allow\twrite succeeded\n'
            elif [[ "$fsrc" == 1 ]]; then printf 'deny\t%s\n' "${err:-write refused}"
            else printf 'error\tunparsed fs probe: %s\n' "$line"; fi
            return ;;
        "PM error="*) printf 'error\tprobe: %s\n' "${line#PM error=}"; return ;;
        "PM "*) ;;
        *) printf 'error\tno probe result: %s\n' "${line:-<empty>}"; return ;;
    esac
    for kv in $line; do
        case "$kv" in
            http=*) http="${kv#http=}" ;; connect=*) connect="${kv#connect=}" ;;
            rc=*) rc="${kv#rc=}" ;; denied=*) denied="${kv#denied=}" ;; policy=*) policy="${kv#policy=}" ;;
            l4=*) l4="${kv#l4=}" ;; dns=*) dns="${kv#dns=}" ;;
        esac
    done
    if [[ "$connect" == 403 ]]; then
        printf 'deny\tCONNECT refused by proxy\n'
    elif [[ "$l4" == eacces ]]; then
        printf 'deny\tconnect() refused by sandbox (EACCES)\n'
    elif [[ "$denied" != - ]]; then
        if [[ "$policy" != - ]]; then printf 'deny\t%s (policy %s)\n' "$denied" "$policy"
        else printf 'deny\t%s\n' "$denied"; fi
    elif [[ "$http" =~ ^[1-5][0-9][0-9]$ ]]; then
        printf 'allow\tupstream HTTP %s\n' "$http"
    elif [[ "$dns" == fail ]]; then
        printf 'error\thost did not resolve; not a policy verdict (DNS blocklist on the host?)\n'
    else
        printf 'error\tno HTTP response (rc=%s)\n' "$rc"
    fi
}

self_test() {
    local fails=0 got
    t() { # line expected-decision
        got="$(classify "$1" | cut -f1)"
        if [[ "$got" == "$2" ]]; then echo "  ok   $2 <- $1"
        else echo "  FAIL want $2 got $got <- $1"; fails=$((fails + 1)); fi
    }
    t "PM http=000 connect=403 rc=56 denied=- policy=-" deny
    t "PM http=403 connect=000 rc=0 denied=policy_denied policy=github_api" deny
    t "PM http=403 connect=000 rc=0 denied=ssrf_denied policy=-" deny
    t "PM http=403 connect=200 rc=0 denied=- policy=-" allow
    t "PM http=404 connect=200 rc=0 denied=- policy=-" allow
    t "PM http=401 connect=000 rc=1 denied=- policy=-" allow
    t "PM http=000 connect=000 rc=6 denied=- policy=-" error
    t "PM http=000 connect=000 rc=28 denied=- policy=-" error
    t "PM http=000 connect=000 rc=7 denied=- policy=- l4=eacces dns=-" deny
    t "PM http=000 connect=000 rc=7 denied=- policy=- l4=- dns=-" error
    t "PM http=000 connect=000 rc=6 denied=- policy=- l4=- dns=fail" error
    t "PM error=unknown-via:x" error
    t "" error
    t "bash: line 1: node: command not found" error
    t "PM fs rc=0" allow
    t "PM fs rc=1 err=Permission denied" deny
    t "PM fs rc=" error
    validate_cases || fails=$((fails + 1))
    [[ $fails -eq 0 ]] && echo "self-test: ok (${#C_ID[@]} cases valid)" || echo "self-test: $fails failure(s)"
    [[ $fails -eq 0 ]]
}

list_cases() {
    local i
    printf '%-26s %-6s %-6s %-7s %-8s %s\n' ID VIA METHOD CURRENT HARDENED URL
    for ((i = 0; i < ${#C_ID[@]}; i++)); do
        selected "${C_ID[$i]}" || continue
        printf '%-26s %-6s %-6s %-7s %-8s %s\n' "${C_ID[$i]}" "${C_VIA[$i]}" "${C_METHOD[$i]}" \
            "${C_CUR[$i]}" "${C_HARD[$i]}" "${C_URL[$i]}"
    done
}

validate_cases || exit 2
case "$MODE" in
    list) list_cases; exit 0 ;;
    self-test) self_test; exit $? ;;
esac

# --- Session (same stdin-hold technique as tests/phase7-exit-gate.sh) ---
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/policy-matrix.XXXXXX")"
S_PID="" S_HOLD="" S_LOG="$WORKDIR/agent-run.log" OWN_SESSION=false

hold_args() { case "$HARNESS" in claude|pi) echo "-p" ;; opencode) echo "run" ;; esac; }

start_session() {
    local fifo="$WORKDIR/stdin.fifo" holdargs args=() waited=0
    mkfifo "$fifo" || return 1
    read -r -a holdargs <<<"$(hold_args)"
    [[ -n "$IMAGE" ]] && args+=(--image "$IMAGE")
    # One byte up front: claude -p proceeds without stdin if none arrives in 3s.
    (printf ' '; exec sleep "$HOLD_SECONDS") >"$fifo" &
    S_HOLD=$!
    "$AGENT_RUN" "$REPO" "$HARNESS" ${args[@]+"${args[@]}"} ${AGENT_ARGS[@]+"${AGENT_ARGS[@]}"} \
        -- "${holdargs[@]}" <"$fifo" >"$S_LOG" 2>&1 &
    S_PID=$!
    OWN_SESSION=true
    while [[ $waited -lt $TIMEOUT ]]; do
        grep -q 'Launching harness' "$S_LOG" 2>/dev/null && break
        kill -0 "$S_PID" 2>/dev/null || break
        sleep 2; waited=$((waited + 2))
    done
    SANDBOX="$(sed -n 's/^\[agent-run\] Sandbox: *//p' "$S_LOG" | head -n1)"
    if ! grep -q 'Launching harness' "$S_LOG" 2>/dev/null; then
        echo "agent-run did not reach 'Launching harness'; last lines of $S_LOG:" >&2
        tail -n 8 "$S_LOG" | sed 's/^/  | /' >&2
        return 1
    fi
    sleep 3
    kill -0 "$S_PID" 2>/dev/null || { echo "harness exited right after launch (log: $S_LOG)" >&2; return 1; }
}

# SIGTERM, never stdin EOF: EOF would send the primed byte to the harness as a prompt.
end_session() {
    [[ "$OWN_SESSION" == true ]] || return 0
    local i=0
    kill -TERM "$S_PID" 2>/dev/null
    while [[ $i -lt 60 ]] && kill -0 "$S_PID" 2>/dev/null; do sleep 2; i=$((i + 1)); done
    if kill -0 "$S_PID" 2>/dev/null; then
        kill -KILL "$S_PID" 2>/dev/null
        [[ -n "$SANDBOX" ]] && openshell sandbox delete "$SANDBOX" >/dev/null 2>&1
    fi
    wait "$S_PID" 2>/dev/null
    [[ -n "$S_HOLD" ]] && { kill "$S_HOLD" 2>/dev/null; wait "$S_HOLD" 2>/dev/null; }
    OWN_SESSION=false
}

finish() {
    end_session
    if [[ "$KEEP_LOGS" == true ]]; then echo "logs kept in $WORKDIR"; else rm -rf "$WORKDIR"; fi
}
trap finish EXIT
trap 'exit 130' INT TERM HUP

sbx() { openshell sandbox exec -n "$SANDBOX" -- "$@"; }

if [[ -z "$SANDBOX" ]]; then
    [[ -x "$AGENT_RUN" ]] || { echo "agent-run binary missing at $AGENT_RUN (run: make build)" >&2; exit 2; }
    command -v openshell >/dev/null || { echo "openshell not on PATH" >&2; exit 2; }
    echo "Starting $HARNESS session on $REPO (waits for egress narrowing)..."
    start_session || exit 2
fi
echo "Sandbox: $SANDBOX   expectations: $EXPECT"

# Install the probe once; base64 keeps quoting out of the exec argv.
if ! sbx bash -c "printf '%s' '$(base64 <"$PROBE_FILE" | tr -d '\n')' | base64 -d >$PROBE_IN_SANDBOX" >/dev/null 2>&1; then
    echo "could not install the probe in $SANDBOX" >&2
    exit 2
fi

PASS=0 FAIL=0 ERR=0 REC=0 CHANGES=0
FAIL_IDS=()
[[ -n "$TSV_OUT" ]] && printf 'id\tvia\tmethod\turl\tcurrent\thardened\tobserved\tdetail\n' >"$TSV_OUT"

for ((i = 0; i < ${#C_ID[@]}; i++)); do
    id="${C_ID[$i]}"
    selected "$id" || continue
    if [[ "${C_VIA[$i]}" == fs ]]; then
        raw="$(sbx bash "$PROBE_IN_SANDBOX" fs-write "${C_URL[$i]}" 2>&1)"
    else
        raw="$(sbx bash "$PROBE_IN_SANDBOX" "${C_VIA[$i]}" "${C_METHOD[$i]}" "${C_URL[$i]}" "${C_BODY[$i]}" 2>&1)"
    fi
    result="$(classify "$(printf '%s\n' "$raw" | grep '^PM ' | tail -n1)")"
    got="${result%%	*}" detail="${result#*	}"
    if [[ "$got" == error ]] && ! printf '%s\n' "$raw" | grep -q '^PM '; then
        detail="$detail ($(printf '%s' "$raw" | tail -n1 | cut -c1-100))"
    fi

    if [[ "$EXPECT" == current ]]; then want="${C_CUR[$i]}" other="${C_HARD[$i]}"; else want="${C_HARD[$i]}" other="${C_CUR[$i]}"; fi
    note=""
    if [[ "$EXPECT" == current && "$other" != "$want" ]]; then note="  [hardened: $other]"; CHANGES=$((CHANGES + 1)); fi

    if [[ "$got" == error ]]; then
        status="ERROR "; ERR=$((ERR + 1)); FAIL_IDS+=("$id")
    elif [[ "$want" == record ]]; then
        status="RECORD"; REC=$((REC + 1))
    elif [[ "$got" == "$want" ]]; then
        status="PASS  "; PASS=$((PASS + 1))
    else
        status="FAIL  "; FAIL=$((FAIL + 1)); FAIL_IDS+=("$id"); detail="expected $want; $detail"
    fi
    printf '%s %-26s %-5s %-5s %s -- %s%s\n' "$status" "$id" "$got" "${C_VIA[$i]}" "$detail" "${C_WHY[$i]}" "$note"
    [[ -n "$TSV_OUT" ]] && printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$id" "${C_VIA[$i]}" "${C_METHOD[$i]}" \
        "${C_URL[$i]}" "${C_CUR[$i]}" "${C_HARD[$i]}" "$got" "$detail" >>"$TSV_OUT"
done

echo ""
echo "Summary: $PASS pass, $FAIL fail, $ERR error, $REC recorded"
[[ "$EXPECT" == current && $CHANGES -gt 0 ]] && echo "         $CHANGES case(s) are expected to change with baseline hardening (see [hardened: ...])"
[[ ${#FAIL_IDS[@]} -gt 0 ]] && echo "Failed: ${FAIL_IDS[*]}"
[[ $FAIL -eq 0 && $ERR -eq 0 ]]
