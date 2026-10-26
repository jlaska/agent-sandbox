# shellcheck shell=bash
# Shared agent-run session helpers for owner-run integration tests (sourced).
#
# start_session runs a real agent-run session in the background and returns
# once the harness is launched (egress narrowed, repo cloned at /sandbox/repo);
# end_session stops it. The harness is held in non-interactive mode by a stdin
# fifo that never delivers a prompt, so tests can `openshell sandbox exec` into
# the sandbox while the session's providers and narrowed policy are live.
#
# Inputs (set before calling): AGENT_RUN, REPO, HARNESS, WORKDIR, and
# optionally IMAGE, AGENT_ARGS (extra agent-run options), TIMEOUT (seconds to
# wait for launch, default 900), HOLD_SECONDS (default 1800).
# Outputs: SANDBOX (sandbox name), S_LOG (agent-run log path).

S_PID="" S_HOLD="" S_LOG="" OWN_SESSION=false

hold_args() { case "$HARNESS" in claude|pi) echo "-p" ;; opencode) echo "run" ;; esac; }

start_session() {
    local fifo="$WORKDIR/stdin.fifo" holdargs args=() waited=0 timeout="${TIMEOUT:-900}"
    S_LOG="$WORKDIR/agent-run.log"
    mkfifo "$fifo" || return 1
    read -r -a holdargs <<<"$(hold_args)"
    [[ -n "${IMAGE:-}" ]] && args+=(--image "$IMAGE")
    # One byte up front: claude -p proceeds without stdin if none arrives in 3s.
    (printf ' '; exec sleep "${HOLD_SECONDS:-1800}") >"$fifo" &
    S_HOLD=$!
    "$AGENT_RUN" "$REPO" "$HARNESS" ${args[@]+"${args[@]}"} ${AGENT_ARGS[@]+"${AGENT_ARGS[@]}"} \
        -- "${holdargs[@]}" <"$fifo" >"$S_LOG" 2>&1 &
    S_PID=$!
    OWN_SESSION=true
    while [[ $waited -lt $timeout ]]; do
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
