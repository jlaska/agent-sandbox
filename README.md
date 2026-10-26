# agent-sandbox

Sandboxed agent launcher for AI coding agents. Runs Claude Code, Pi, or opencode
inside an [OpenShell](https://github.com/nvidia/openshell) container with
per-repository GitHub security constraints.

## What it does

`agent-run` handles the full lifecycle of a sandboxed agent session:

1. Check the OpenShell CLI version (>= 0.1.4) and resolve the harness image
2. Resolve GitHub credentials (App token or PAT)
3. Pin the default branch to a commit and compile a repo-scoped L7 network
   policy: the baseline plus any `.agent-policy.yaml` requests the caller
   granted (see [Repository network requests](#repository-network-requests))
4. Create an isolated container from the harness image, with credential
   injection and an optional per-repo toolchain cache
5. Clone the target repo at the pinned commit, configure Git identity
6. Install the repo's [mise](https://mise.jdx.dev) toolchain (if it has one),
   then remove the temporary install egress from the policy
7. Launch the agent harness
8. Clean up everything on exit (sandbox, tokens, providers)

The agent gets exactly the permissions it needs for one repo — enforced at the
network layer — with credentials that self-destruct.

## Security model

| Layer | Enforcement |
|---|---|
| GitHub App + rulesets | Agent can only push `agent/**` branches, create PRs, comment. Cannot push `main`, merge, or manipulate tags. |
| L7 network policy | Per-binary enforcement: only `git`/`gh` can reach GitHub, and only for the target repo (branch deletes limited to `agent/**`); only the harness can reach inference endpoints, and only their inference and model-listing paths (no file uploads, batches, MCP gateway or key admin), and no telemetry hosts are allowed (Claude Code runs with `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`). Provider profiles carry credentials only; the policy binds each session's providers to their hosts. Merge APIs blocked. Verified per rule by `make policy-matrix`. |
| Repository requests | A repo can ask for extra HTTPS hosts in `.agent-policy.yaml`, but only the caller grants them (`--allow-host`, `--grants`). Grants never add a host the repo did not request, and cannot override the hard boundaries (IP literals, local and metadata names, GitHub hosts, hosts the baseline already governs). Every check runs before any sandbox resource exists; the loaded policy must contain exactly the granted `repo_*` rules. |
| Token scope | Single-repo scoped (minting it is the access check; agent-run never mints an installation-wide token for a session), auto-expires (~1h), revoked on exit. |
| Container isolation | Rootless container, default-deny network, read-only system paths enforced by Landlock (`hard_requirement`: the sandbox does not start without it). |
| Dependency cooldown | Package versions published in the last 3 days are not installed (npm, uv, mise). |

## Installation

```bash
make build          # builds bin/agent-run
make install        # installs to GOPATH/bin
```

The binary embeds the sandbox policy template, the pinned image refs and the
provider profiles, so it runs from any directory. A policy file in the working
directory is never used; change `openshell/sandbox-policy.yaml` and rebuild.

Requires Go 1.21+ and [OpenShell](https://github.com/nvidia/openshell)
**0.1.4 or newer** with a running gateway (Podman or Docker compute driver).
Building images locally also requires Podman.

Install OpenShell:

```bash
curl -LsSf https://raw.githubusercontent.com/NVIDIA/OpenShell/main/install.sh | OPENSHELL_VERSION=v0.1.4 sh
```

On macOS this installs NVIDIA's `nvidia/openshell` Homebrew formula, which runs
the gateway as a `brew services` launchd service. (Homebrew-core's `openshell`
formula ships only the binaries, without a gateway service.) To use the
toolchain cache, enable `allow_driver_config` in the gateway config (see
[Toolchain cache](#toolchain-cache)).

## Usage

```bash
agent-run <owner/repo> <harness> [options] [-- <harness-args>...]
```

| Harness | Description |
|---|---|
| `claude` | Claude Code CLI |
| `pi` | Pi coding agent |
| `opencode` | [opencode](https://github.com/anomalyco/opencode) coding agent |

Each harness runs from its own image; see [Harness images](#harness-images).

### Inference authentication

`agent-run` reads inference settings from your environment (or `--env`):

```bash
# Direct Anthropic API
export ANTHROPIC_API_KEY=sk-ant-...
agent-run jlaska/agent-sandbox claude

# Claude Max/Pro subscription (via OAuth token from `claude setup-token`)
export CLAUDE_CODE_OAUTH_TOKEN=<token>
agent-run jlaska/agent-sandbox claude

# LiteLLM proxy
export ANTHROPIC_API_KEY=sk-litellm-...
export ANTHROPIC_BASE_URL=https://litellm.example.com
agent-run jlaska/agent-sandbox claude

# Vertex AI
export CLAUDE_CODE_USE_VERTEX=1
export ANTHROPIC_VERTEX_PROJECT_ID=my-project
agent-run jlaska/agent-sandbox claude

# Explicit env override
agent-run jlaska/agent-sandbox claude --env ANTHROPIC_API_KEY=sk-ant-...
```

#### opencode backends

opencode needs an API key. `--inference` picks the backend; without it,
`agent-run` uses LiteLLM if `LITELLM_API_KEY` is set, otherwise Anthropic if
`ANTHROPIC_API_KEY` is set.

```bash
# LiteLLM (also needs LITELLM_BASE_URL)
export LITELLM_API_KEY=sk-litellm-...
export LITELLM_BASE_URL=https://litellm.example.com
agent-run jlaska/agent-sandbox opencode --inference litellm --model litellm/<model-id>

# Anthropic API directly
export ANTHROPIC_API_KEY=sk-ant-...
agent-run jlaska/agent-sandbox opencode --inference anthropic
```

A `CLAUDE_CODE_OAUTH_TOKEN` (Claude Pro/Max) is not usable with opencode,
because those tokens are not for use with third-party tools.

#### How credentials reach the sandbox

Secrets never enter the sandbox or a command line. `agent-run` stores each one
in an OpenShell provider (`openshell provider create ... --credential KEY`
reads the value from that child's environment). The sandbox sees only an opaque
placeholder, which OpenShell's proxy replaces with the real value on outbound
requests. A provider accepts only the env vars its profile declares, and the
placeholder appears under that name:

| Provider type | Sandbox env vars | Host source |
|---|---|---|
| `github-agent` | `GITHUB_TOKEN`, `GH_TOKEN` | minted App token or PAT |
| `claude-code` | `ANTHROPIC_API_KEY` | `ANTHROPIC_API_KEY`, else `CLAUDE_CODE_OAUTH_TOKEN`, else `ANTHROPIC_AUTH_TOKEN` |
| `anthropic` (opencode) | `ANTHROPIC_API_KEY` | `ANTHROPIC_API_KEY` |
| `litellm-inference` (pi, opencode) | `LITELLM_API_KEY` | `LITELLM_API_KEY` |

An OAuth token is stored as `ANTHROPIC_API_KEY`; the shell that launches
`claude` renames the placeholder to `CLAUDE_CODE_OAUTH_TOKEN` right before
`exec`. The `claude-code` profile sends the credential in the `x-api-key`
header, so a Pro/Max OAuth token only works where that header is accepted (for
example a LiteLLM proxy). `agent-run` imports the provider profiles it needs from copies embedded
in the binary when the gateway does not have them, and updates a gateway copy
that differs. The profiles have no endpoints: the sandbox policy binds each
session's providers (`credential_binding`) to the hosts where their
credentials may be used, so a profile cannot widen the policy. A profile
attached to a running sandbox cannot be updated, so stop running sessions
before upgrading `agent-run`.

Each run is one session, identified by its sandbox name (`a-<harness>-<8 hex>`,
for example `a-claude-3f9c01ab`). Its providers are named
`<session>-<type>` (`a-claude-3f9c01ab-github-agent`) and labelled
`agent-run.session=<session>`, so several sessions can share a gateway. The
cleanup deletes only that session's providers. To find what a crashed session
left behind: `openshell provider list --label-selector agent-run.session=<session>`.

### GitHub authentication

Two modes, auto-detected:

- **App mode** (default): Uses a GitHub App installation token minted from a
  private key stored in macOS Keychain. The token is scoped to the target repo;
  GitHub refuses to mint it for a repo the App cannot reach (give a renamed repo
  by its current name). Tokens are revoked on exit. `--list-repos` uses a
  short-lived installation-wide token to list the App's repos and revokes it
  right away.
- **PAT mode**: Pass a Personal Access Token via `--github-token` or
  `GITHUB_TOKEN` env var. No App required. L7 sandbox policy still constrains
  operations.

```bash
# App mode (auto, if App key is in Keychain)
agent-run jlaska/agent-sandbox claude

# PAT mode
agent-run --github-token ghp_xxx jlaska/agent-sandbox claude
```

### Interrupting a run

Ctrl-C (SIGINT) and SIGTERM stop the run at the next step and then clean up
exactly once: delete the sandbox, revoke the installation token and delete the
providers. `agent-run` exits with status 130 (`Error: interrupted by signal`).
A second Ctrl-C cannot interrupt the cleanup. While the harness runs on a TTY,
Ctrl-C goes to the harness inside the sandbox, not to `agent-run`.

### Passthrough args

Everything after `--` is forwarded to the harness:

```bash
agent-run jlaska/agent-sandbox claude -- -p "fix the failing test"
agent-run jlaska/agent-sandbox claude -- --model opus-4-8 --allowedTools Bash,Read,Edit
```

### Flags

| Flag | Description |
|---|---|
| `--model <m>` | Override the default model (passed to the harness as `--model`) |
| `--env KEY=VAL` | Pass environment variable to sandbox (repeatable) |
| `--image <ref>` | Use this image instead of the pinned one (see [Harness images](#harness-images)) |
| `--no-mise` | Skip the mise toolchain install |
| `--mise-unsafe` | Install the mise toolchain without hardening (see [Toolchains with mise](#toolchains-with-mise)) |
| `--fresh-cache` | Clear the mise cache before installing (cannot be combined with `--no-cache`) |
| `--no-cache` | Do not mount a persistent mise cache |
| `--mise-allow-host host[:port]` | Extra host allowed during mise install (repeatable; default port 443) |
| `--allow-host <host>` | Grant the repo's `.agent-policy.yaml` request for `host` (HTTPS GET/HEAD on any path; repeatable) |
| `--grants <file>` | Grants file authorizing repo requests (per host: repos, methods, paths) |
| `--no-repo-policy` | Ignore the repo's `.agent-policy.yaml` and use the baseline policy |
| `--explain-policy` | Resolve and compile the session policy, print every decision and exit; creates no sandbox or providers and revokes its token |
| `--inference litellm\|anthropic` | Inference backend (opencode only) |
| `--driver docker\|podman\|kubernetes\|vm` | Override compute driver detection |
| `--github-token <t>` | Use a GitHub PAT instead of App token |
| `--diag` | Print diagnostics: OpenShell version, image refs, compute driver, cache settings |
| `--list-repos` | List repositories accessible to the GitHub App |
| `--mint-token <owner/repo>` | Mint a repo-scoped token and print it (for scripts) |
| `--revoke-token <t>` | Revoke a previously minted token |
| `--generate-policy <owner/repo>` | Generate the repo-scoped baseline policy file and print its path (no repo requests; the repo policy flags do not apply) |
| `--version` | Print the agent-run version (the release tag; `git describe` for `make build`) |
| `--help` | Print usage |

### Repository network requests

The trust boundary is: the repository requests, the caller grants. A repo may
commit `.agent-policy.yaml` asking for HTTPS access to extra hosts:

```yaml
version: 1
network:
  - host: docs.example.org
    reason: "fetch API reference docs"
    optional: true   # default false
```

Before creating anything, `agent-run` resolves the default branch to a commit
SHA, reads the manifest at that commit through the GitHub API and compiles the
session policy. A request becomes a `repo_*` rule (HTTPS on 443, for the
harness process) only when a caller grant covers it. A required request with
no grant fails the run; an optional one is left out with a warning. Malformed
or oversized (over 64 KiB) manifests and requests or grants for prohibited
hosts fail too. The sandbox then clones exactly that commit, even if the
branch moved since.

```bash
# What would this run be allowed? (no sandbox, no providers, no clone)
agent-run example/other-repo claude --explain-policy --allow-host docs.example.org

# Grant it for this run (GET/HEAD on every path)
agent-run example/other-repo claude --allow-host docs.example.org

# Or from a grants file, scoped per repo, method and path
agent-run example/other-repo claude --grants ./grants.yaml
```

```yaml
# grants.yaml
version: 1
grants:
  - host: docs.example.org
    repos: [example/other-repo]  # optional; omitted = any repo
    methods: [GET, HEAD]         # optional; default [GET, HEAD]
    paths: ["/api/**"]           # optional; default ["/**"]
```

`--no-repo-policy` ignores the manifest. OpenShell's runtime denials remain the
final enforcement boundary.

## Harness images

Every harness runs from its own OCI image (`ghcr.io/jlaska/agent-<harness>`),
built from a shared base with all versions pinned. `openshell/images.yaml` holds
the digest-pinned refs and is embedded in the `agent-run` binary.

When an image's digest is not pinned yet (all zeros in `images.yaml`),
`agent-run` uses the version tag and prints a warning.

### Local builds with `--image`

```bash
make images      # builds localhost/agent-claude:local, agent-pi:local, agent-opencode:local (Podman)
make smoke       # smoke-test the local images (see docs/phase7-harness-images.md)
agent-run jlaska/agent-sandbox claude --image localhost/agent-claude:local
```

`make image-claude`, `make image-pi` and `make image-opencode` build one image.
`--image` accepts any ref; a ref without `@sha256:` logs a warning because the
tag can move. The gateway's container engine must have the image in its local
store (build with the same Podman the gateway uses).

## Toolchains with mise

After the repo is cloned, `agent-run` looks in `/sandbox/repo` for a mise
config: `mise.toml`, `.mise.toml`, `mise/config.toml`, `.config/mise.toml`,
`.config/mise/config.toml` or `.tool-versions`. If none is found, or with
`--no-mise`, the install is skipped and logged.

The install is hardened, because the repo's config runs while install egress is
open:

- Only the detected config files are trusted, with `MISE_PARANOID=1`: each file
  is trusted for its current content only, and plugins from outside mise's
  registry are refused.
- `MISE_NO_HOOKS=1` skips `[hooks]` (preinstall, postinstall, ...). A tool's own
  build scripts (cargo/npm/pip backends) still run; the egress allowlist bounds
  where they can connect.
- A 3-day dependency cooldown applies (see below).

`--mise-unsafe` turns all three off for that install, for repos that need hooks,
unregistered plugins or a just-released dependency. `agent-run` logs a warning.

Commit a `mise.lock` (`mise lock`) so downloads are checksum-verified; without
one, `agent-run` warns.

### Install egress

The install runs under a temporary `mise_bootstrap` network rule that allows
read-only access to the tool registries (GitHub releases, mise, PyPI, npm,
dl.k8s.io, get.helm.sh and similar). The rule is **always removed before the
agent starts**, even when mise is skipped. If the removal cannot be verified,
or the effective policy is not the sandbox's own (a gateway-wide global
policy) or a provider profile adds endpoints to it, `agent-run` deletes the
sandbox, revokes the token and exits non-zero. If a tool
needs another host (for example `nodejs.org` for node), allow it:

```bash
agent-run jlaska/agent-sandbox claude --mise-allow-host nodejs.org
```

### Runtime package registries

Two registries stay reachable while the agent runs, read-only and bound to one
package manager each: `registry.npmjs.org` for `node` (npm), and `pypi.org` and
`files.pythonhosted.org` for `uv`/`uvx`. So the agent can run `uv run pytest`,
`uv sync` or `uvx <tool>` against a repo's `pyproject.toml`. The dependency
cooldown applies. Python interpreters are not downloadable at runtime (uv
fetches them from GitHub): use the image's Python 3.12, or pin `python` in the
repo's `mise.toml` so mise installs it during the bootstrap.

### Dependency cooldown

Package versions published less than 3 days ago are not installed, so a
malicious release has time to be caught and yanked first. The base image sets
this for everything in the sandbox, the agent included:

| Tool | Setting |
|---|---|
| npm | `min-release-age=3` in the root-owned global npmrc (`/usr/local/etc/npmrc`) |
| uv (and mise's `pipx:` backend) | `UV_EXCLUDE_NEWER="3 days"` |
| mise | `MISE_MINIMUM_RELEASE_AGE=3d` |

npm refuses a version pinned exactly (`pkg@1.2.3`) that is too new
(`ETARGET ... with a date before`), and resolves a range to an older version.
mise does not filter versions pinned exactly in its config. To opt out for one
command: `npm --min-release-age=0`, `uv --exclude-newer false`,
`mise install --minimum-release-age 0s`.

The period is `DEP_COOLDOWN_DAYS` in `openshell/images/base/Dockerfile` and
`dependencyCooldownEnv` in `internal/agentrun/mise.go` (a test keeps them equal).

### Toolchain cache

Installed tools are cached per repository in a volume named
`mise-cache-<owner>-<repo>`, mounted at `/sandbox/.mise-cache`, so a second run
of the same repo skips the downloads. `--fresh-cache` empties the cache before
installing, and `--no-cache` mounts none.

| Driver | Behaviour |
|---|---|
| `docker`, `podman` | `agent-run` creates a labelled named volume in that engine if missing and mounts it |
| `kubernetes` | Expects a PersistentVolumeClaim of the same name in the sandbox namespace; `agent-run` prints the `kubectl` manifest to create it once. Sandboxes stay Pending until it exists, and a fresh PVC may be root-owned, so check ownership if installs fail |
| `vm` or unknown | No cache; the reason is logged |

The driver is read from `openshell gateway info`; override it with `--driver`.
Mounting needs the gateway to allow caller-supplied driver config. Set this in
the gateway config (`~/.config/openshell/gateway.toml`) for your driver, then
restart the gateway:

```toml
[openshell]
version = 2

[openshell.gateway]
compute_driver = "podman"    # or docker / kubernetes

[openshell.drivers.podman]   # same driver name as above
allow_driver_config = true
```

If the gateway refuses the mount, `agent-run` prints this setting and retries
once without a cache.

The cache is read-write for the agent, so a compromised agent could poison it
for the next run of the same repo. Use `--fresh-cache` after any suspicious
session, and commit `mise.lock`. See
[docs/phase7-harness-images.md](docs/phase7-harness-images.md#threat-notes).

## Releasing and updating images (maintainers)

1. Change a pin (or Renovate does) in `openshell/images/**` and open a PR.
2. CI builds the changed harnesses with Podman on amd64 and arm64 and runs the
   smoke tests. PR runs never push.
3. Merging to `main` publishes multi-arch images to
   `ghcr.io/jlaska/agent-<harness>` (tags `<ver>` and
   `<ver>-<YYYYMMDD>.<sha7>`) and writes the digest to the job summary.
4. Renovate opens a PR bumping the digest in `openshell/images.yaml`.
5. Merge that PR and tag an `agent-run` release; the new binary embeds the new
   digests.

New GHCR packages are private; make each `agent-<harness>` package public in the
GitHub package settings so sandboxes can pull it.

opencode publishes no checksum file, so its Dockerfile hard-codes sha256 values.
After a version bump, run `openshell/images/refresh-checksums.sh` and commit the
result. Details are in
[docs/phase7-harness-images.md](docs/phase7-harness-images.md#renovate).

## Development

```bash
make test       # run tests
make vet        # go vet
make lint       # golangci-lint
make fmt        # format source
make setup      # install dev dependencies + pre-commit hooks
make images     # build harness images locally with Podman
make smoke      # smoke-test the local images
make help       # list all targets
```

### Policy matrix

`make policy-matrix` starts one session and checks every network rule from inside
the sandbox. Each case is one binary, method and URL with an expected decision
(`tests/policy-matrix/cases.sh`). A result counts as **deny** only when OpenShell
itself refused it: the sandbox refused `connect()`, or the proxy answered with
its own denial. Any upstream reply, including a 401 or 404, means the policy
allowed it. Anything else (for example a host your DNS blocklist answers with
`0.0.0.0`) is an error, never a deny. Probes use paths that match a rule but name
nothing real, so an allowed probe changes nothing upstream.

```bash
make policy-matrix                                   # claude on jlaska/agent-sandbox
make policy-matrix MATRIX_FLAGS="--expect hardened"  # verdicts for the hardened baseline
./tests/policy-matrix.sh --list                      # show the cases
./tests/policy-matrix.sh --sandbox NAME --only node-sentry
```

Each case records today's decision and the decision expected after baseline
hardening. Cases where the two differ are marked `[hardened: ...]`.

`--repo-policy granted|ungranted` runs only the repository policy cases, on the
fixture `jlaska/agent-sandbox-mise-fixture`, whose `.agent-policy.yaml`
requests `example.org` as optional. With `granted` the session gets
`--allow-host example.org`. The script first checks that `repo_example_org` is
in the loaded policy, then that a GET from the harness's process tree is
allowed, a POST is denied with `policy_denied`, bare `curl` is denied and
`example.net` (never requested) is denied. With `ungranted` it checks for the
omission warning and that no `repo_*` rule is loaded, and every case is denied.

```bash
make policy-matrix MATRIX_FLAGS="--repo-policy granted"
make policy-matrix MATRIX_FLAGS="--repo-policy ungranted"
```

### Workflow gate

`make workflow-gate` works a repository from inside a real session the way an
agent does: it pushes `agent/**` branches, opens a draft PR, comments, reviews,
reads checks and marks the PR ready. It also requires each forbidden operation
to be refused for the right reason. GitHub rulesets must refuse pushes to
`main`, non-agent branches and tags. OpenShell must refuse merges, auto-merge
and other repositories. Any other failure counts as a FAIL. A second short
session asks the harness for a reply through its real inference provider. The
default target is the public fixture `jlaska/agent-sandbox-mise-fixture`, which
has this repository's rulesets. Branches and the PR are removed on exit.

```bash
make workflow-gate                                                   # claude
./tests/workflow-gate.sh --harness pi -- --model gpt-6-luna
./tests/workflow-gate.sh --harness opencode -- --inference litellm --model litellm/gpt-6-luna
```

## License

See [LICENSE](LICENSE) for details.
