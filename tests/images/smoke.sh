#!/usr/bin/env bash
# Smoke test for an agent-sandbox harness image.
#
# Usage: smoke.sh <image-ref> <claude|pi|opencode> [--skip-network] [--skip-policy-contract]
#
#   --skip-network          skip the mise fixture install (needs internet egress)
#   --skip-policy-contract  skip "every policy binary for this harness is in binaries.txt"
#
# The container engine is `podman`; override with CONTAINER_ENGINE. Exits non-zero
# if any check fails. Commands run in a login shell (bash -lc), like `sandbox exec`.
# SC2015: pass() never fails, so `A && pass || fail` is a safe if/else.
# SC2016: the single-quoted scripts are expanded inside the container on purpose.
# shellcheck disable=SC2015,SC2016
set -uo pipefail

if [ $# -lt 2 ]; then
  sed -n '2,/^set -uo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
  exit 2
fi
image="$1"
harness="$2"
shift 2
skip_network=0
skip_policy=0
for a in "$@"; do
  case "$a" in
    --skip-network) skip_network=1 ;;
    --skip-policy-contract) skip_policy=1 ;;
    *) echo "unknown option: $a" >&2; exit 2 ;;
  esac
done
case "$harness" in claude | pi | opencode) ;; *) echo "bad harness: $harness" >&2; exit 2 ;; esac

engine="${CONTAINER_ENGINE:-podman}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
img_dir="$root/openshell/images"
fixture="$root/tests/images/fixtures/mise.toml"
policy="$root/openshell/sandbox-policy.yaml"

fails=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; fails=$((fails + 1)); }
# check <description> <command...>: PASS if the command succeeds.
check() {
  local desc="$1"
  shift
  if "$@" >/dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}
# sh_in <script>: run a script in the image as its default user, in a login shell.
sh_in() { "$engine" run --rm --entrypoint bash "$image" -lc "$1"; }

want_version="$("$img_dir/version.sh" "$harness")" || { echo "cannot determine pinned version" >&2; exit 2; }
echo "== smoke: $image ($harness $want_version) via $engine"

# --- user, home, cache dirs, env -------------------------------------------------
[ "$(sh_in 'id -un')" = "sandbox" ] && pass "user is sandbox" || fail "user is sandbox"
[ "$(sh_in 'id -u')" = "1500" ] && pass "uid is 1500" || fail "uid is 1500"
[ "$(sh_in 'echo $HOME')" = "/sandbox" ] && pass "HOME is /sandbox" || fail "HOME is /sandbox"
check "mise cache dirs exist and are writable by sandbox" \
  sh_in 'for d in /sandbox/.mise-cache/data /sandbox/.mise-cache/cache /sandbox/.config/mise; do [ "$(stat -c %U "$d")" = sandbox ] && [ -w "$d" ] || exit 1; done'
check "mise env vars set" \
  sh_in '[ "$MISE_DATA_DIR" = /sandbox/.mise-cache/data ] && [ "$MISE_CACHE_DIR" = /sandbox/.mise-cache/cache ] && [ "$MISE_CONFIG_DIR" = /sandbox/.config/mise ]'
check "mise shims on PATH in login shell" sh_in 'case ":$PATH:" in *:/sandbox/.mise-cache/data/shims:*) ;; *) exit 1 ;; esac'
check "MISE_YES not set in image" sh_in '[ -z "${MISE_YES:-}" ]'
cooldown="$(sed -n 's/^ARG DEP_COOLDOWN_DAYS=//p' "$img_dir/base/Dockerfile")"
check "dependency cooldown: npm min-release-age=$cooldown (global npmrc)" \
  sh_in "[ \"\$(npm config get min-release-age)\" = $cooldown ] && [ \"\$(stat -c %U /usr/local/etc/npmrc)\" = root ]"
check "dependency cooldown: MISE_MINIMUM_RELEASE_AGE=${cooldown}d, UV_EXCLUDE_NEWER='$cooldown days'" \
  sh_in "[ \"\$MISE_MINIMUM_RELEASE_AGE\" = ${cooldown}d ] && [ \"\$UV_EXCLUDE_NEWER\" = '$cooldown days' ]"
check "iproute2 present (required by OpenShell)" sh_in 'command -v ip'

# --- harness version and labels --------------------------------------------------
got="$(sh_in "$harness --version" 2>&1 | head -n1)"
case "$got" in *"$want_version"*) pass "$harness --version contains $want_version" ;; *) fail "$harness --version is '$got', want $want_version" ;; esac
label() { "$engine" image inspect --format "{{index .Config.Labels \"$1\"}}" "$image"; }
[ "$(label io.github.jlaska.agent-sandbox.harness)" = "$harness" ] && pass "label harness=$harness" || fail "label harness=$harness"
[ "$(label io.github.jlaska.agent-sandbox.harness-version)" = "$want_version" ] && pass "label harness-version=$want_version" || fail "label harness-version=$want_version (got '$(label io.github.jlaska.agent-sandbox.harness-version)')"
[ "$(label org.opencontainers.image.version)" = "$want_version" ] && pass "label image.version=$want_version" || fail "label image.version=$want_version"
for l in source revision created title; do
  [ -n "$(label "org.opencontainers.image.$l")" ] && pass "label image.$l set" || fail "label image.$l set"
done
tools_ok=1
for t in node npm mise uv gh git jq make python3 curl; do
  sh_in "command -v $t" >/dev/null 2>&1 || { fail "tool on PATH: $t"; tools_ok=0; }
done
[ "$tools_ok" -eq 1 ] && pass "base tools on PATH (node npm mise uv gh git jq make python3 curl)"

# --- binaries.txt: exist, executable, real files ---------------------------------
bin_list="$img_dir/$harness/binaries.txt"
while IFS= read -r p; do
  case "$p" in '' | '#'*) continue ;; esac
  # shellcheck disable=SC2016 # $1 is expanded by the inner shell
  if "$engine" run --rm --entrypoint bash "$image" -c '[ -x "$1" ] && [ "$(readlink -f "$1")" = "$1" ]' _ "$p" >/dev/null 2>&1; then
    pass "binary is a real executable: $p"
  else
    fail "binary missing, not executable, or a symlink: $p"
  fi
done <"$bin_list"

# --- policy contract (strict) ----------------------------------------------------
if [ "$skip_policy" -eq 1 ]; then
  echo "SKIP: policy contract (--skip-policy-contract)"
else
  missing="$(uv run --quiet --with pyyaml python -I - "$policy" "$bin_list" "$harness" <<'PY'
import sys, yaml, os
policy, bins, harness = sys.argv[1:4]
listed = {l.strip() for l in open(bins) if l.strip() and not l.startswith("#")}
others = {"claude", "pi", "opencode"} - {harness}
doc = yaml.safe_load(open(policy))
bad = set()
for key, net in (doc.get("network_policies") or {}).items():
    paths = [b["path"] for b in net.get("binaries", [])]
    # Check every rule; skip only binaries named for another harness (claude/pi/opencode).
    # Shared paths (e.g. pi reaching litellm via node) must be in the list for this harness.
    for p in paths:
        if os.path.basename(p) in others:
            continue
        if p not in listed:
            bad.add(f"{p} (network {key})")
print("\n".join(sorted(bad)))
PY
)" || { fail "policy contract check could not run"; missing=""; }
  if [ -z "$missing" ]; then
    pass "policy contract: every policy binary for $harness is in binaries.txt"
  else
    fail "policy contract: policy binaries not in $harness/binaries.txt:"
    while IFS= read -r m; do echo "      - $m"; done <<<"$missing"
  fi
fi

# --- harness-specific checks -----------------------------------------------------
case "$harness" in
  claude)
    check "DISABLE_AUTOUPDATER=1" sh_in '[ "${DISABLE_AUTOUPDATER:-}" = 1 ]'
    ;;
  pi)
    check "pi-provider-litellm extension installed" sh_in 'pi list 2>&1 | grep -q "pi-provider-litellm"'
    check "pi-provider-litellm extension files present" sh_in 'ls /sandbox/.pi/agent/npm/node_modules/pi-provider-litellm/package.json'
    ;;
  opencode)
    check "OPENCODE_DISABLE_AUTOUPDATE=1" sh_in '[ "${OPENCODE_DISABLE_AUTOUPDATE:-}" = 1 ]'
    check "opencode.json has autoupdate=false" sh_in 'jq -e ".autoupdate == false" /sandbox/.config/opencode/opencode.json'
    ;;
esac

# --- mise fixture ----------------------------------------------------------------
if [ "$skip_network" -eq 1 ]; then
  echo "SKIP: mise fixture install (--skip-network)"
else
  # shellcheck disable=SC2016 # expanded inside the container
  if out="$("$engine" run --rm -i --entrypoint bash "$image" -lc '
      set -e
      mkdir -p /tmp/fx && cat > /tmp/fx/mise.toml && cd /tmp/fx
      mise trust
      mise install --yes
      kubectl version --client
      pre-commit --version' <"$fixture" 2>&1)"; then
    pass "mise fixture: installed and ran kubectl + pre-commit"
  else
    fail "mise fixture install/run"
    echo "$out" | tail -n 20 | sed 's/^/      /'
  fi
fi

echo
if [ "$fails" -eq 0 ]; then
  echo "RESULT: all checks passed for $image"
else
  echo "RESULT: $fails check(s) FAILED for $image"
  exit 1
fi
