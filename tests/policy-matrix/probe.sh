#!/usr/bin/env bash
# In-sandbox probe for tests/policy-matrix.sh. Do not run on the host.
#
# Usage: probe.sh VIA METHOD URL [BODY]
#        probe.sh fs-write DIR
#
# VIA picks the process that opens the connection, because OpenShell matches a
# rule's binaries against that process OR ANY ANCESTOR:
#   curl    curl run directly (ancestor: the exec shell only)
#   node    curl spawned by node     (node is bound to inference/telemetry/npm rules)
#   python  curl spawned by python3.12 (bound to litellm_inference)
#   git     curl spawned by a git alias (git is bound to github_git/github_api)
#   gh      gh api itself              (gh is bound to github_api/github_graphql)
#
# BODY names a fixed request body: none (default), json-empty, graphql-viewer,
# graphql-merge.
#
# Prints exactly one line the host parses:
#   PM http=<code> connect=<code> rc=<rc> denied=<policy_denied|ssrf_denied|-> policy=<name|-> l4=<eacces|-> dns=<fail|->
# denied names the OpenShell proxy's own 403 body (OpenShell v0.1.4
# crates/openshell-supervisor-network/src/{proxy.rs,l7/rest.rs}); connect=403
# means the proxy refused the CONNECT tunnel; l4=eacces means the sandbox
# refused the connect() itself. Any other HTTP status came from upstream, i.e.
# the policy let the request through.

set -u
# shellcheck disable=SC1090  # the sandbox profile exports the proxy and credential placeholders
source ~/.profile 2>/dev/null

if [[ "${1:-}" == fs-write ]]; then
    f="${2:?fs-write needs a directory}/.policy-matrix-probe.$$"
    if err="$( (: >"$f") 2>&1)"; then
        rm -f "$f"
        echo "PM fs rc=0"
    else
        echo "PM fs rc=1 err=$(printf '%s' "$err" | tr -s '\n ' ' ' | cut -c1-80)"
    fi
    exit 0
fi

via="${1:?via}" method="${2:?method}" url="${3:?url}" body="${4:-none}"
case "$body" in
    none) data="" ;;
    json-empty) data='{}' ;;
    graphql-viewer) data='{"query":"query { viewer { login } }"}' ;;
    graphql-merge) data='{"query":"mutation { mergePullRequest(input: {pullRequestId: \"PR_policy_matrix_nonexistent\"}) { clientMutationId } }"}' ;;
    *) echo "PM error=unknown-body:$body"; exit 0 ;;
esac

out="$(mktemp)" errf="$(mktemp)" dataf="$(mktemp)"
trap 'rm -f "$out" "$errf" "$dataf"' EXIT
printf '%s' "$data" >"$dataf"

# -v: the transparent-mode denial is only visible in curl's verbose trace.
args=(-sSv -m 20 -o "$out" -w '%{http_code} %{http_connect}' -X "$method")
[[ -n "$data" ]] && args+=(-H 'Content-Type: application/json' --data-binary "@$dataf")
args+=("$url")

codes=""
case "$via" in
    curl)
        codes="$(curl "${args[@]}" 2>"$errf")"; rc=$? ;;
    node)
        # shellcheck disable=SC2016  # JavaScript, not shell
        codes="$(node -e 'const r=require("child_process").spawnSync("curl",process.argv.slice(1),{stdio:["ignore","pipe","inherit"],encoding:"utf8"});process.stdout.write(r.stdout||"");process.exit(r.status===null?125:r.status)' -- "${args[@]}" 2>"$errf")"; rc=$? ;;
    python)
        codes="$(python3.12 -c 'import subprocess,sys; sys.exit(subprocess.run(["curl"]+sys.argv[1:]).returncode)' "${args[@]}" 2>"$errf")"; rc=$? ;;
    git)
        codes="$(git -c 'alias.pm=!curl' pm "${args[@]}" 2>"$errf")"; rc=$? ;;
    gh)
        path="/${url#https://api.github.com/}"
        ghargs=(api --method "$method" --include "$path")
        [[ -n "$data" ]] && ghargs+=(--input "$dataf")
        gh "${ghargs[@]}" >"$out" 2>"$errf"; rc=$?
        # --include prints the status line first, e.g. "HTTP/2.0 404 Not Found".
        # Fall back to gh's "(HTTP 404)" error suffix if the status line is missing.
        http="$(sed -n '1s/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$out")"
        [[ -z "$http" ]] && http="$(sed -n 's/.*(HTTP \([0-9][0-9][0-9]\)).*/\1/p' "$errf" | head -n1)"
        connect=000
        if [[ -z "$http" ]] && grep -qiE 'proxyconnect|forbidden' "$errf"; then connect=403; fi
        codes="${http:-000} $connect" ;;
    *) echo "PM error=unknown-via:$via"; exit 0 ;;
esac

read -r http connect <<<"$codes"
# The proxy's own denial bodies (proxy.rs build_json_error_response*, l7/rest.rs).
denied="$(grep -oE '"error":"(policy_denied|ssrf_denied)"' "$out" 2>/dev/null | head -n1 | cut -d'"' -f4)"
policy="$(sed -n 's/.*"policy":"\([^"]*\)".*/\1/p' "$out" 2>/dev/null | head -n1)"
# Transparent interception (what `openshell sandbox exec` gets: no proxy env):
# the supervisor answers a denied connect() with EACCES via seccomp
# (openshell-sandbox/src/linux/seccomp_notify.rs). curl -v logs it as
# "Immediate connect fail for <ip>: Permission denied"; Go (gh) as
# "connect: permission denied".
l4=-
grep -qE 'connect fail for .*: Permission denied|connect: permission denied' "$errf" && l4=eacces
# A name that does not resolve is not a policy verdict: the policy DNS hands out
# synthetic 198.18.x.x addresses, so this means the host resolver failed (e.g. a
# DNS blocklist answering 0.0.0.0).
dns=-
grep -qE 'Could not resolve host|no such host' "$errf" && dns=fail
echo "PM http=${http:-000} connect=${connect:-000} rc=$rc denied=${denied:--} policy=${policy:--} l4=$l4 dns=$dns"
