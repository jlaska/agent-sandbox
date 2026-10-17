# agent-sandbox

Sandboxed agent launcher for AI coding agents. Runs Claude Code, Pi, or opencode
inside an [OpenShell](https://github.com/nvidia/openshell) container with
per-repository GitHub security constraints.

## What it does

`agent-run` handles the full lifecycle of a sandboxed agent session:

1. Check the OpenShell CLI version (>= 0.1.4) and resolve the harness image
2. Resolve GitHub credentials (App token or PAT)
3. Generate a repo-scoped L7 network policy
4. Create an isolated container from the harness image, with credential
   injection and an optional per-repo toolchain cache
5. Clone the target repo, configure Git identity
6. Install the repo's [mise](https://mise.jdx.dev) toolchain (if it has one),
   then remove the temporary install egress from the policy
7. Launch the agent harness with your inference env vars
8. Clean up everything on exit (sandbox, tokens, providers)

The agent gets exactly the permissions it needs for one repo — enforced at the
network layer — with credentials that self-destruct.

## Security model

| Layer | Enforcement |
|---|---|
| GitHub App + rulesets | Agent can only push `agent/**` branches, create PRs, comment. Cannot push `main`, merge, or manipulate tags. |
| L7 network policy | Per-binary enforcement: only `git` can reach github.com, only the harness can reach inference endpoints. Merge APIs blocked. |
| Token scope | Single-repo scoped, auto-expires (~1h), revoked on exit. |
| Container isolation | Podman rootless container, default-deny network, read-only system paths. |

## Installation

```bash
make build          # builds bin/agent-run
make install        # installs to GOPATH/bin
```

Requires: Go 1.21+, [OpenShell](https://github.com/nvidia/openshell) **0.1.4 or
newer**, and a container runtime for the OpenShell gateway (Podman, or Docker
as another OpenShell compute driver).
Building images locally also requires Podman.

### Upgrading from OpenShell 0.0.x

`agent-run` refuses older OpenShell CLIs. OpenShell has no in-place upgrade from
0.0.x, so:

1. Note your existing sandboxes and providers (`openshell sandbox list`,
   `openshell provider list`); they do not survive the upgrade.
2. Uninstall OpenShell 0.0.x following the upstream upgrade notes, then install
   0.1.4 or newer:

   ```bash
   curl -LsSf https://raw.githubusercontent.com/NVIDIA/OpenShell/main/install.sh | OPENSHELL_VERSION=v0.1.4 sh
   ```

   On macOS this installs NVIDIA's `nvidia/openshell` Homebrew formula, which
   runs the gateway as a `brew services` launchd service. Do not
   `brew upgrade` into Homebrew-core's `openshell` formula: it ships only the
   binaries, so the old 0.0.x gateway keeps running, and once stopped nothing
   starts a new one.
3. Migrate `~/.config/openshell/gateway.toml` to schema version 2. The 0.1.x
   gateway refuses the old format (`category=missing_version`), and this file
   overrides the formula's default. At minimum: add `[openshell] version = 2`,
   and replace `compute_drivers = ["podman"]` with
   `compute_driver = "podman"`. See the upstream
   [migration guide](https://docs.nvidia.com/openshell/latest/how-it-works/gateways/configuration#migrate-to-schema-version-2).
   Check the result with `openshell-gateway config preflight`.
4. Set aside the 0.0.x gateway state. 0.1.x cannot use it, and
   `openshell gateway destroy` no longer exists:
   `mv ~/.local/state/openshell/gateway ~/.local/state/openshell/gateway.bak`.
5. To use the toolchain cache, enable `allow_driver_config` for your compute
   driver (see [Toolchain cache](#toolchain-cache)). Then restart the gateway
   (`brew services restart nvidia/openshell/openshell` on macOS) and confirm
   that `openshell gateway info` reports the new version.
6. Recreate sandboxes. `agent-run` always creates a fresh sandbox per run.

OpenShell 0.1.4 ships no built-in provider profiles, so `provider create` fails
until the profile is imported. You do not need to do this by hand: `agent-run`
imports the profiles it needs (`github-agent`, `claude-code`, `anthropic`,
`litellm-inference`) from the copies embedded in the binary, only when the
gateway does not already list them. Sandboxes are always created with
`--from <image ref>`; community base image names and Dockerfile builds are no
longer supported.

## Usage

```bash
agent-run <owner/repo> <harness> [options] [-- <harness-args>...]
```

### Harnesses

| Harness | Binary | Description |
|---|---|---|
| `claude` | `claude` | Claude Code CLI |
| `pi` | `pi` | Pi coding agent |
| `opencode` | `opencode` | [opencode](https://github.com/anomalyco/opencode) coding agent |

The old plain-bash `shell` harness was removed; requesting it fails with an
error listing the supported harnesses.

Each harness runs from its own image
(`ghcr.io/jlaska/agent-<harness>`), pinned by digest in
`openshell/images.yaml` and embedded in the binary. See
[Harness images](#harness-images).

### Inference authentication

Agent-run passes your inference credentials through to the sandbox via
environment variables. Set them before running, or use `--env`:

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

opencode needs an API key. The backend is chosen by `--inference`, or when the
flag is omitted: LiteLLM if `LITELLM_API_KEY` resolves, otherwise Anthropic if
`ANTHROPIC_API_KEY` resolves, otherwise an error naming both.

```bash
# LiteLLM (also needs LITELLM_BASE_URL)
export LITELLM_API_KEY=sk-litellm-...
export LITELLM_BASE_URL=https://litellm.example.com
agent-run jlaska/agent-sandbox opencode --inference litellm --model litellm/<model-id>

# Anthropic API directly
export ANTHROPIC_API_KEY=sk-ant-...
agent-run jlaska/agent-sandbox opencode --inference anthropic
```

`CLAUDE_CODE_OAUTH_TOKEN` (Claude Pro/Max) alone is rejected for opencode with
an explicit error, because those tokens are not for use with third-party tools.
`--inference` is accepted for the opencode harness only.

#### How credentials reach the sandbox

Secrets are never put on a command line. `agent-run` creates each OpenShell
provider with the bare form `openshell provider create ... --credential KEY`,
which makes OpenShell read the value from the environment of that one child
process. In OpenShell 0.1.4 a provider only accepts the `env_vars` its profile
declares as credential keys, and the sandbox sees an opaque placeholder under
that same name:

| Provider | Stored keys (sandbox env vars) | Host source |
|---|---|---|
| `github-agent` | `GITHUB_TOKEN`, `GH_TOKEN` | minted App token or PAT |
| `claude-code` | `ANTHROPIC_API_KEY` | `ANTHROPIC_API_KEY`, else `CLAUDE_CODE_OAUTH_TOKEN`, else `ANTHROPIC_AUTH_TOKEN` |
| `anthropic` (opencode) | `ANTHROPIC_API_KEY` | `ANTHROPIC_API_KEY` |
| `litellm-inference` (pi, opencode) | `LITELLM_API_KEY` | `LITELLM_API_KEY` |

`CLAUDE_CODE_OAUTH_TOKEN` and `ANTHROPIC_AUTH_TOKEN` are not declared by any
profile, so they cannot be stored under their own names. When the OAuth token is
the credential, it is stored as `ANTHROPIC_API_KEY`, and the shell that launches
`claude` renames the placeholder to `CLAUDE_CODE_OAUTH_TOKEN` (and unsets
`ANTHROPIC_API_KEY`) right before `exec`. No dotfile in the sandbox is changed. The
`claude-code` profile injects it in the `x-api-key` header, so a Pro/Max OAuth
token only works where that is accepted (for example a LiteLLM proxy). The
optional `LITELLM_API_KEY_BEARER` / `LITELLM_BEARER_TOKEN` credentials of the
`litellm-inference` profile are no longer filled with a dummy value; the
`LITELLM_API_KEY` placeholder is resolved wherever the client puts it
(`x-api-key` or `Authorization: Bearer`).

### GitHub authentication

Two modes, auto-detected:

- **App mode** (default): Uses a GitHub App installation token minted from a
  private key stored in macOS Keychain. Repos are auto-discovered from the App
  installation. Tokens are revoked on exit.
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

Ctrl-C (SIGINT) and SIGTERM do not abandon the sandbox. `agent-run` stops at the
next step, terminates any running child, and then performs its normal cleanup
exactly once: it deletes the sandbox, revokes the installation token and deletes
the providers. It exits with status 130 (`Error: interrupted by signal`). A
second Ctrl-C during cleanup is ignored on purpose, because killing the cleanup
would leave a sandbox with bootstrap egress and a live token. While the harness
itself runs on a TTY, Ctrl-C goes to the harness inside the sandbox, not to
`agent-run`.

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
| `--image <ref>` | Use this image instead of the one pinned in `images.yaml` (see [Harness images](#harness-images)) |
| `--no-mise` | Skip the mise toolchain install |
| `--fresh-cache` | Clear the mise cache before installing (cannot be combined with `--no-cache`) |
| `--no-cache` | Do not mount a persistent mise cache |
| `--mise-allow-host host[:port]` | Extra host allowed during mise install (repeatable; default port 443) |
| `--inference litellm\|anthropic` | Inference backend (opencode only) |
| `--driver docker\|podman\|kubernetes\|vm` | Override compute driver detection |
| `--github-token <t>` | Use a GitHub PAT instead of App token |
| `--diag` | Print diagnostic information |
| `--list-repos` | List repositories accessible to the GitHub App |
| `--mint-token <repo>` | Mint a repo-scoped token (for scripts) |
| `--revoke-token <t>` | Revoke a previously minted token |
| `--generate-policy <repo>` | Generate a repo-scoped policy file |
| `--help` | Print usage |

`--diag` also reports the OpenShell version and the 0.1.4 check, the resolved
image ref per harness, the detected compute driver, the `allow_driver_config`
setting the cache needs, and whether the repo's cache volume exists.

## Harness images

Every harness runs from its own OCI image, built from a shared base, with all
versions pinned. `openshell/images.yaml` holds the digest-pinned refs and is
embedded in the `agent-run` binary.

An image whose digest Renovate has not pinned yet has an all-zero digest in
`images.yaml` (CI publishes the version tag when the image change merges; the
pin follows in a Renovate PR). Until then `agent-run` uses the version tag and
warns:

```text
[agent-run] WARNING: the claude image digest is not pinned yet in images.yaml; using tag ghcr.io/jlaska/agent-claude:2.1.301 (it may move). ...
```

### Local builds with `--image`

```bash
make images      # builds localhost/agent-claude:local, agent-pi:local, agent-opencode:local (Podman)
make smoke       # smoke-test the local images (see docs/phase7-harness-images.md)
agent-run jlaska/agent-sandbox claude --image localhost/agent-claude:local
```

`make image-claude`, `make image-pi` and `make image-opencode` build one image.
`--image` accepts any ref; a ref without `@sha256:` logs a warning because the
tag can move. The gateway's compute driver must be able to find the image in
its container engine's local store (build with the same Podman the gateway uses).

## Toolchains with mise

After the repo is cloned, `agent-run` looks in `/sandbox/repo` for a mise
config. It probes, in order: `mise.toml`, `.mise.toml`, `mise/config.toml`,
`.config/mise.toml`, `.config/mise/config.toml` and `.tool-versions`.

- **Config found:** each detected file is trusted (`mise trust <file>`) and
  `mise install --yes` runs inside the sandbox with `MISE_PARANOID=1` and
  `MISE_NO_HOOKS=1`. The repo's config runs while the temporary bootstrap egress
  is open, so this limits what it can do: paranoid mode trusts each file only for
  its current content and refuses plugins from outside mise's registry, and
  `[hooks]` (preinstall, postinstall, ...) are skipped. A tool's own build
  scripts (cargo/npm/pip backends) still run; the egress allowlist bounds where
  they can connect. The log shows the duration, the cache state (`cold`, `warm`
  or `none`) and the installed tools.
- **Dependency cooldown (3 days):** package versions published less than 3
  days ago are not installed, so a malicious release has time to be caught and
  yanked first. The base image sets this for everything in the sandbox, the
  agent included: npm `min-release-age=3` in the root-owned global npmrc
  (`/usr/local/etc/npmrc`), `UV_EXCLUDE_NEWER="3 days"` (uv, and mise's `pipx:`
  backend), and `MISE_MINIMUM_RELEASE_AGE=3d`. The mise install sets the same
  values explicitly, so they also apply with an `--image` that lacks them.
  The tools treat an exact pin differently: npm refuses a version pinned
  exactly (`pkg@1.2.3`) if it is too new (`ETARGET ... with a date before`); a
  range resolves to an older version. mise skips the check for versions pinned
  exactly in its config. To opt out for one command: `npm --min-release-age=0`,
  `uv --exclude-newer false`, `mise install --minimum-release-age 0s`. Change
  the period with `DEP_COOLDOWN_DAYS` in `openshell/images/base/Dockerfile` and
  `dependencyCooldownEnv` in `internal/agentrun/mise.go`; a test keeps them equal.
- **`--mise-unsafe`:** turns the hardening off (`mise trust /sandbox/repo`,
  hooks on, paranoid off, no dependency cooldown during the install) for repos
  that need hooks, unregistered plugins or a just-released dependency. The
  image's cooldown still applies to the agent afterwards. agent-run logs a warning.
- **No config:** the install is skipped and the log says what was looked for:
  `mise: no mise config found in /sandbox/repo (looked for: ...); skipping toolchain install`.
- **`--no-mise`:** skipped, logged as `mise: skipped (--no-mise)`.
- **Lockfile:** if a `mise.lock` is present the log says
  `lockfile checksums enforced`; otherwise it warns that downloads are not
  checksum-pinned. Run `mise lock` in the repo and commit `mise.lock`.

The install runs under a temporary `mise_bootstrap` network rule that allows
read-only access to the tool registries (GitHub releases, mise, PyPI, npm,
dl.k8s.io, get.helm.sh and similar). The rule is present at sandbox creation
and is **always removed before the agent starts**, even when mise is skipped.
If the removal cannot be verified, `agent-run` deletes the sandbox, revokes the
token and exits non-zero. If a tool needs a host outside the default list (for
example `nodejs.org` for node), pass it explicitly:

```bash
agent-run jlaska/agent-sandbox claude --mise-allow-host nodejs.org
```

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
the gateway config for your driver, then recreate the gateway:

```toml
[openshell]
version = 2

[openshell.gateway]
compute_driver = "podman"    # or docker / kubernetes

[openshell.drivers.podman]   # same driver name as above
allow_driver_config = true
```

If the gateway refuses the mount, `agent-run` prints this setting, deletes the
partial sandbox and retries once without a cache.

The cache is read-write for the agent, so a compromised agent could poison it
for the next run of the same repo. Use `--fresh-cache` after any suspicious
session; commit `mise.lock` so installs are checksum-verified. See
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

One-time setup: GHCR packages are private on first push. After the first publish
make `agent-claude`, `agent-pi` and `agent-opencode` **public** in the GitHub
package settings, otherwise sandboxes cannot pull them.

opencode publishes no checksum file, so its Dockerfile hard-codes sha256 values.
A Renovate version bump fails the build until you run
`openshell/images/refresh-checksums.sh` and commit the result. Details are in
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

## License

See [LICENSE](LICENSE) for details.
