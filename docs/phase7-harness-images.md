# Phase 7 — Per-harness Images, mise Toolchains, OpenShell 0.1.4

Operator and maintainer notes for the harness images and the mise toolchain
flow. The user-facing summary is in the [README](../README.md); the plan and
tracker is GitHub issue #41.

## Why this phase exists

The sandbox used to run whatever Claude Code was baked into the OpenShell
community base image (Claude Code 2.1.156). That image is retired, and OpenShell
0.1.4 defaults to a plain Ubuntu image with no agent CLIs. Phase 7 replaces it
with images we own, and moves repo toolchains (kubectl, ansible, helm,
pre-commit, ...) out of the image and into the target repo's mise config.

## Design

### One image per harness, built from a shared base

```text
nvcr.io/nvidia/base/ubuntu:24.04@sha256:...        (Renovate: digest)
└── base   (built first by build.sh, never published)
    ├── agent-claude    -> ghcr.io/jlaska/agent-claude
    ├── agent-pi        -> ghcr.io/jlaska/agent-pi
    └── agent-opencode  -> ghcr.io/jlaska/agent-opencode
```

- **Per-harness images** keep each agent's runtime surface small and let each
  harness version move independently. A base change rebuilds all three.
- **The base is not published.** `openshell/images/build.sh` builds it locally
  as `localhost/agent-base:<tag>` and passes it to each harness build with
  `--build-arg BASE_IMAGE`. Only the three harness images have registry
  digests, so the base needs no digest pin of its own.
- **Everything is pinned.** Every version is an `ARG *_VERSION=` directly under
  a `# renovate:` comment; there are no `latest` tags. Downloads are verified by
  sha256 where upstream publishes checksums (opencode's are hard-coded, see
  [Renovate](#renovate)). Auto-updaters are off in every harness.
- **Digest-pinned at runtime.** `openshell/images.yaml` holds
  `ref: <image>:<ver>@sha256:<digest>` per harness and is `go:embed`-ed into
  `agent-run`. The mutable `<ver>` tag is for humans; the digest is what the
  sandbox pulls. An all-zero digest means "not published yet".
- **Contract with the policy.** OpenShell 0.1.4 matches network rules by the
  binary's real path, so every binary the policy names must be a real file (not
  a symlink) at a stable path. `openshell/images/<harness>/binaries.txt` lists
  those paths and the smoke test and Go contract tests enforce them.
- **User and paths.** The image runs as user `sandbox` (UID/GID 1500, home
  `/sandbox`). mise data and cache live under `/sandbox/.mise-cache`, which is
  the per-repo cache mount target.

### Two-phase egress

The policy always contains a `mise_bootstrap` rule bound only to
`/usr/local/bin/mise` (its child processes match through the parent-process
rule), read-only, for the registry hosts. `agent-run` removes it after the mise
phase and before the agent starts:

1. `openshell policy update <sandbox> --remove-rule mise_bootstrap --wait`
2. `openshell policy get <sandbox> --full -o json` must show no
   `mise_bootstrap` rule.
3. `openshell policy get <sandbox> --rev <N> -o json` for the effective
   revision must report status `loaded`.

`--wait` alone is not proof (it also exits 0 for "unchanged" and "superseded"),
which is why the two `policy get` checks exist. In 0.1.4 `--full` reports only
`status: "effective"` with no revision state, so revision state is read with
`--rev`.

Narrowing always runs, including with `--no-mise` or when the repo has no mise
config, because the rule is added at create time before the repo is cloned.

## Threat notes

- **Narrowing fails closed.** Any failure of the removal or either check deletes
  the sandbox, revokes the GitHub token and exits non-zero; the agent is never
  launched with bootstrap egress. OpenShell itself defaults to
  `policy_validation_failure_mode = fail_closed`, so a bad policy revision
  blocks all egress rather than opening it.
- **Bootstrap egress is exposure during install.** While `mise install` runs,
  anything run by the repo's mise config (tasks, plugins, backends) can reach
  the bootstrap hosts, read-only. It cannot reach GitHub write APIs or
  inference endpoints through that rule. Only repos you trust should have their
  mise config executed; `--no-mise` skips the phase.
- **Per-repo cache poisoning.** The cache volume is per repository
  (`mise-cache-<owner>-<repo>`) and read-write for the agent. An agent that is
  compromised or prompt-injected during one run can modify cached tools, which
  the next run of the same repo will use. The blast radius is limited to that
  repo's cache: other repos have their own volumes and the images are not
  touched. Mitigations: `--fresh-cache`, `--no-cache`, and committing
  `mise.lock` so freshly downloaded artifacts are checksum-verified. A
  read-only cache for the agent phase is a recorded follow-up.
- **Provider rules are unaffected by narrowing.** Rules attached by providers
  (`_provider_*`) are not part of the base policy that `--remove-rule` edits, so
  narrowing neither removes nor widens them. Provider credentials and
  inference/GitHub access are whatever the providers grant for the whole run.
- **Policy Advisor.** The in-sandbox Policy Advisor (`policy.local`) can only
  propose rules, and only operators can apply policy changes. Note that
  `agent-run` does not configure the advisor or `approval_mode` anywhere:
  `openshell/sandbox-policy.yaml` has no such setting, so the behaviour is
  whatever your gateway defaults to. Keep approval manual (or the advisor
  disabled) on the gateway you use with `agent-run`.
- **Hash pinning.** OpenShell hash-pins a binary on first use. A path whose
  content changes after that is denied; that is by design and also why the
  image, not the running sandbox, is where binaries get updated.
- **Images contain no secrets** and no repo-specific policy.

## Local build and smoke test

All builds use Podman.

```bash
make images                       # base, then claude, pi, opencode -> localhost/agent-<h>:local
make image-pi                     # one harness
make smoke                        # smoke-test every local image
make smoke HARNESSES=pi           # one harness
make smoke SMOKE_FLAGS=--skip-network   # skip the mise fixture install (needs internet)
make lint-images                  # hadolint + shellcheck (needs both installed)
./openshell/images/version.sh claude    # print the pinned version from the Dockerfile
```

Run an image with `agent-run`:

```bash
make build
./bin/agent-run jlaska/agent-sandbox claude --image localhost/agent-claude:local
```

The smoke test (`tests/images/smoke.sh <image-ref> <harness>`) checks, as the
`sandbox` user: user and home, the harness `--version` equals the pinned ARG,
every `binaries.txt` path exists, is executable and is its own `readlink -f`,
every policy binary for the harness is in `binaries.txt`, the harness's
auto-update setting, and that a mise fixture (kubectl, pre-commit) installs and
runs.

`build.sh` can also be called directly, for example for another architecture:

```bash
./openshell/images/build.sh --platform linux/amd64 --local claude
```

## Renovate

`.github/renovate.json5` tracks:

- the base image digest and every Dockerfile `ARG *_VERSION` (via the
  `# renovate:` comment above it),
- the `ref:` lines in `openshell/images.yaml`, including their digests,
- GitHub Actions (SHA pinned) and Go modules.

Bumps are grouped per harness (`harness-claude`, `harness-pi`,
`harness-opencode`) and for the shared base; harness CLIs wait 3 days after
release; nothing automerges.

Flow for a harness bump:

1. Renovate opens a PR changing the Dockerfile `ARG`.
2. CI builds that harness on amd64 and arm64 and smoke-tests it. No push.
3. Merge. The Images workflow publishes the multi-arch manifest to GHCR and
   writes the digest to the job summary.
4. Renovate opens a second PR updating the `images.yaml` ref and digest
   (label `image-digest`).
5. Merge it and tag an `agent-run` release; released binaries embed the digests.

Plain `X.Y.Z` tags only are considered for `images.yaml`; the
`X.Y.Z-YYYYMMDD.sha7` tags are ignored.

First publish: packages start private on GHCR. After the first publish, set
`agent-claude`, `agent-pi` and `agent-opencode` to **public** (one time), then
fill the digests into `images.yaml` from Renovate's PR or the job summary.

### opencode checksum refresh

opencode publishes no checksum file, so `openshell/images/opencode/Dockerfile`
hard-codes `OPENCODE_SHA256_AMD64` and `OPENCODE_SHA256_ARM64`. After any
`OPENCODE_VERSION` bump (including a Renovate PR), the build fails until they
are refreshed. That failure is intentional.

```bash
./openshell/images/refresh-checksums.sh --check   # verify; exits 1 if the Dockerfile differs
./openshell/images/refresh-checksums.sh           # rewrite the two ARGs, then commit
```

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `OpenShell X is too old; agent-run requires >= 0.1.4` | Upgrade as described in the [README](../README.md#upgrading-from-openshell-00x). There is no in-place upgrade. |
| `image for <harness> not yet published; build locally (make images) and pass --image <ref>` | `images.yaml` still has the zero digest. Build locally and pass `--image localhost/agent-<h>:local`, or fill in the published digest. |
| `provider create` fails with an unknown type | A provider profile is not imported. `agent-run` imports them itself; if you created providers by hand, run `openshell provider list-profiles` and import the missing one. |
| Egress denied for a binary that is allowed in the policy | OpenShell matches the **real path** of the binary. A symlink (`/usr/bin/python3`) or a script run through an interpreter (`npm`, `pi` are node scripts) does not match. Bind the rule to the real path (`/usr/bin/python3.12`, `/usr/local/bin/node`) and list it in `binaries.txt`. `make smoke` reports violations. |
| A binary is denied after it was replaced | Binaries are hash-pinned on first use. Rebuild the image rather than replacing binaries in a running sandbox. |
| `egress narrowing failed (sandbox deleted, agent not launched)` | Fail-closed by design. The message says whether the removal failed, the rule was still present, or the revision was not `loaded` (a `load_error` is included when OpenShell gives one). Check `openshell policy list <sandbox>` before retrying; a rejected revision blocks all egress under `fail_closed`. |
| `WARN: the gateway refused the mise cache mount` | The gateway disallows driver config. Add `allow_driver_config = true` under `[openshell.drivers.<driver>]` and recreate the gateway. The run continues without a cache. |
| `volume ... exists without labels ...` | The cache volume was created without the labels OpenShell requires (`openshell.ai/sandbox-attachable=true` and `openshell.ai/sandbox-attachable-workspace=<workspace>`). Labels cannot be changed in place: `podman volume rm mise-cache-<owner>-<repo>` and rerun. |
| Kubernetes sandbox stays Pending | The `mise-cache-<owner>-<repo>` PVC is missing. Create it with the manifest `agent-run` prints, or use `--no-cache`. |
| `mise install failed: ... --mise-allow-host` | A tool needs a host outside the bootstrap list. Look in the sandbox logs for the denied host and rerun with `--mise-allow-host host[:port]`. Unauthenticated `api.github.com` is limited to 60 requests/hour, which can also fail installs of many GitHub-hosted tools. |
| `--fresh-cache and --no-cache are mutually exclusive` | `--no-cache` mounts nothing, so there is nothing to clear. Use one or the other. |
| `--inference applies to the opencode harness only` | `--inference` is only for opencode. |
| opencode: `CLAUDE_CODE_OAUTH_TOKEN ... is not usable with third-party tools` | opencode needs `ANTHROPIC_API_KEY` or `LITELLM_API_KEY`. |
| opencode image build fails on a checksum | Run `openshell/images/refresh-checksums.sh` (see above). |
| Error says the `shell` harness was removed | Use `claude`, `pi` or `opencode`. |
