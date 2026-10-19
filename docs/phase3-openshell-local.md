# Phase 3 — Local OpenShell and Providers v2

> Historical record for OpenShell 0.0.113. `agent-run` now requires OpenShell 0.1.4 or newer, which has no in-place upgrade from 0.0.x; see "Upgrading from OpenShell 0.0.x" in the [README](../README.md).

**Date:** 2026-08-26
**OpenShell version:** 0.0.113 (pinned)
**Compute driver:** Podman 6.0.2 (Apple Hypervisor VM)

## Installation

OpenShell was installed via the official install script with version pinning:

```bash
curl -LsSf https://raw.githubusercontent.com/NVIDIA/OpenShell/main/install.sh | OPENSHELL_VERSION=v0.0.113 sh
```

The installer:

- Added the `nvidia/openshell` Homebrew tap
- Installed the `openshell` CLI and gateway binary
- Registered a launchd service via `brew services`

### Compute driver selection

**Chosen:** Podman (rootless container isolation)
**Available alternatives:** MicroVM (libkrun + Apple Hypervisor), Docker Desktop

Podman was selected because:

1. Already installed and running (v6.0.2 with Apple Hypervisor VM)
2. Rootless containers provide good isolation without a root daemon
3. Well-supported by OpenShell on macOS
4. Lower friction than MicroVM for proving sandbox behavior

MicroVM would provide stronger (VM-boundary) isolation but is opt-in and was not required to prove Phase 3 goals. It can be revisited for production use.

### Gateway configuration

The gateway auto-detection does not find Podman's API socket on macOS. Manual configuration was required:

**`~/.config/openshell/gateway.toml`:**

```toml
[openshell.gateway]
compute_drivers = ["podman"]

[openshell.drivers.podman]
socket_path = "/var/folders/w0/dvf1mnks5xb9yshjt5gbf69m0000gn/T/podman/podman-machine-default-api.sock"
```

> **Note:** The socket path is under `/var/folders` which is session-specific on macOS. If the Podman machine is recreated, this path may change. Find the current path with:
>
> ```bash
> podman machine inspect podman-machine-default | grep -A1 PodmanSocket
> ```

After configuration:

```bash
brew services restart nvidia/openshell/openshell
openshell status  # → Connected, Authenticated (mTLS transport)
```

## Sandbox lifecycle

### Create

```bash
openshell sandbox create --name <name>           # persistent sandbox
openshell sandbox create --name <name> --no-keep  # auto-delete after command exits
openshell sandbox create --name <name> -- <cmd>   # run command immediately
```

### Execute commands

```bash
openshell sandbox exec -n <name> -- <command>
```

### List

```bash
openshell sandbox list
```

### Delete

```bash
openshell sandbox delete <name>
```

Deletion takes ~15–25 seconds as the Podman container is stopped and removed. No orphaned containers remain after deletion.

### Logs

```bash
openshell logs <name>                    # combined sandbox + gateway logs
```

Sandbox-internal logs are at:

- `/var/log/openshell.YYYY-MM-DD.log` — structured supervisor log
- `/var/log/openshell-ocsf.YYYY-MM-DD.log` — OCSF-formatted security events

## Sandbox environment

| Property | Value |
|---|---|
| User | `sandbox` (uid 998) |
| Working directory | `/sandbox` |
| Writable paths | `/sandbox`, `/tmp` |
| Read-only paths | `/usr`, `/lib`, `/proc`, `/dev/urandom`, `/app`, `/etc`, `/var/log` |
| Filesystem sandbox | Landlock v2 (best-effort compatibility) |
| Kernel | Linux 6.18.5 (Podman machine, Fedora 43) |
| Pre-installed tools | `python3` (3.14.3), `git`, `curl`, `sh`, `gh`, `claude`, `codex`, `copilot` |
| Pre-configured dirs | `.claude/`, `.agents/`, `.uv/`, `.venv/` |

## Default network policy

The sandbox ships with a default policy at `/etc/openshell/policy.yaml` that implements:

### Architecture

- **OPA (Open Policy Agent)** engine for L4 (CONNECT) decisions
- **L7 engine** for HTTP method/path enforcement
- **Binary identity enforcement** — network access depends on which binary makes the request, resolved via `/proc/net/tcp` inode lookup + `/proc/{pid}/exe`

### Default policy groups

| Policy | Allowed binaries | Allowed endpoints | Access level |
|---|---|---|---|
| `claude_code` | `/usr/local/bin/claude`, `/usr/bin/node` | api.anthropic.com, statsig.anthropic.com, sentry.io, raw.githubusercontent.com, platform.claude.com | Full |
| `github_ssh_over_https` | `/usr/bin/git` | github.com:443 | Git read-only (info/refs, git-upload-pack) |
| `github_rest_api` | `/usr/local/bin/claude`, `/usr/bin/gh` | api.github.com:443 | Read-only |
| `copilot` | `/usr/bin/copilot`, copilot node modules | github.com, api.github.com, copilot APIs | Full |
| `pypi` | python, pip, uv binaries | pypi.org, files.pythonhosted.org, github.com | Full |
| `codex` | `/usr/bin/codex`, `/usr/bin/node` | api.openai.com, auth.openai.com, chatgpt.com | Full |
| `vscode` / `cursor` | curl, wget, editor binaries | VS Code / Cursor update/marketplace hosts | Full |
| `nvidia_inference` | curl, bash, opencode | integrate.api.nvidia.com | Full |

### Key security observations

1. **Git push is disabled by default** — `git-receive-pack` is commented out in the `github_ssh_over_https` policy. Git clone/fetch work; push does not.
2. **`curl` to github.com is blocked** — only `git-remote-http` (via `/usr/bin/git`) is allowed to connect to github.com in the `github_ssh_over_https` policy.
3. **GitHub REST API is read-only** — the `github_rest_api` policy enforces `access: read-only`.
4. **No GitHub write credential present** — push attempts fail with "could not read Username" since no token is injected.

### Deny log workflow

Denied requests appear in `openshell logs <name>` as OCSF events:

```text
[OCSF] NET:OPEN [MED] DENIED /usr/bin/curl(83) -> httpbin.org:443
  [policy:- engine:opa] [reason:endpoint httpbin.org:443 is not allowed by any policy]

[OCSF] NET:OPEN [MED] DENIED /usr/bin/curl(92) -> github.com:443
  [policy:- engine:opa] [reason:binary '/usr/bin/curl' not allowed in policy 'copilot']
```

Each denial includes:

- The binary path and PID
- The target endpoint
- The policy that was evaluated
- The specific denial reason (endpoint not in any policy, or binary not allowed)

The supervisor also flushes denial analysis and policy proposals to the gateway for review.

## Test results (2026-08-26)

| Test | Expected | Result |
|---|---|---|
| Sandbox creates successfully | PASS | ✅ PASS |
| User is `sandbox` (uid 998) | PASS | ✅ PASS |
| Write to `/sandbox` | PASS | ✅ PASS |
| Write to `/tmp` | PASS | ✅ PASS |
| List `/` | FAIL | ✅ FAIL (Permission denied) |
| `curl` to httpbin.org | FAIL | ✅ FAIL (403 from proxy) |
| `curl` to github.com | FAIL | ✅ FAIL (binary not allowed) |
| `curl` to api.github.com | FAIL | ✅ FAIL (binary not allowed) |
| `git clone` (read) | PASS | ✅ PASS (via `github_ssh_over_https`) |
| `git push` (no credential) | FAIL | ✅ FAIL (no username) |
| `ping` external | FAIL | ✅ FAIL (Operation not permitted) |
| DNS resolution | FAIL | ✅ FAIL (connection refused) |
| Python available | PASS | ✅ PASS |
| Git available | PASS | ✅ PASS |
| Sandbox delete cleans up | PASS | ✅ PASS |
| No orphaned Podman containers | PASS | ✅ PASS |

## Phase 3 exit gate status

- [x] Local OpenShell runs reliably on the Mac
- [x] Agent/shell processes can edit and run local commands
- [x] Default-deny network behavior is understood
- [x] Provider credentials are not yet required to prove basic sandbox operation
- [x] Sandbox cleanup is reliable
