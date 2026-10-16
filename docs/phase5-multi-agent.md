# Phase 5 — Multi-agent Compatibility

> Historical record. The `shell` harness has since been removed and `opencode` added, and sandboxes now run from per-harness images. For current usage see the [README](../README.md) and [phase7-harness-images.md](phase7-harness-images.md).

## Summary

Phase 5 validates that multiple coding agents can operate within the OpenShell sandbox while sharing the same GitHub security policy. Each agent independently authenticates to its inference provider through the LiteLLM proxy and passes the full GitHub acceptance matrix.

## Usage

```bash
agent-run <repo> <harness> [--provider <provider>] [--model <model>] [--max]
```

### Harness / provider / model matrix

| Harness | litellm (default) | vertex | api |
|---------|-------------------|--------|-----|
| claude  | ✓ | ✓ | ✓ |
| pi      | ✓ |   |   |

### Examples

```bash
agent-run jlaska/agent-sandbox-test claude                                    # litellm, default model
agent-run jlaska/agent-sandbox-test claude --model claude-sonnet-4-20250514   # litellm, specific model
agent-run jlaska/agent-sandbox-test claude --provider vertex                  # direct Vertex AI
agent-run jlaska/agent-sandbox-test claude --provider vertex --model claude-sonnet-4-20250514
agent-run jlaska/agent-sandbox-test claude --provider api                     # direct api.anthropic.com
agent-run jlaska/agent-sandbox-test claude --provider litellm --max           # Max subscription billing
agent-run jlaska/agent-sandbox-test pi                                        # litellm, default model
agent-run jlaska/agent-sandbox-test pi --model go-mimo-v2.5                   # litellm, specific model
```

`--model` is passed through to the harness CLI as `--model <value>`. When omitted, each harness uses its own default.

## Architecture

The launcher separates three concerns: harness (which agent binary), provider (where model calls go), and model (which model to request). The GitHub credential path is identical for all combinations — the Phase 4 `github-agent` provider.

```text
agent-run <repo> <harness> [--provider <provider>] [--model <model>]
  │
  ├── GitHub provider (github-agent)
  │     └── credentials: GITHUB_TOKEN, GH_TOKEN (from minted installation token; OpenShell 0.1.4 stored keys are the profile's env_vars)
  │
  ├── Inference provider (one of):
  │     ├── litellm-inference  → litellm.internal.keener.us
  │     ├── vertex-ai          → *-aiplatform.googleapis.com (gcloud ADC)
  │     └── claude-code        → api.anthropic.com (builtin)
  │
  └── Harness launch: <harness-cmd> [--model <model>]
```

## 5.1 Claude Code

**Status:** Proven

### Auth path

Claude Code uses API key mode with the LiteLLM proxy:

- `ANTHROPIC_API_KEY` is an alias of the `LITELLM_API_KEY` provider placeholder (`export ANTHROPIC_API_KEY="$LITELLM_API_KEY"`). The gateway injects the real credential in the `x-api-key` HTTP header.
- `ANTHROPIC_BASE_URL` points to the LiteLLM proxy endpoint.
- The builtin `claude-code` OpenShell provider is required because OpenShell auto-detects the `claude` binary and demands its corresponding provider.

### Max subscription mode

An optional `--max` flag on `agent-run` adds `ANTHROPIC_CUSTOM_HEADERS` with the LiteLLM bearer token. This enables the LiteLLM header-forwarding path for Claude Max subscription billing instead of API key billing. Note: Max mode requires Claude Code OAuth login inside the sandbox or pre-configured OAuth tokens.

### Credential containment

| Credential | Contained? | Notes |
|---|---|---|
| GitHub token | Yes | Provider placeholder; gateway injects real token |
| LiteLLM API key | Yes | Provider placeholder; gateway injects real key |
| ANTHROPIC_API_KEY env var | Sentinel only | Alias of `$LITELLM_API_KEY` (provider placeholder) |
| LiteLLM bearer token (--max) | Partially | Passed via env var when Max mode is used |

### Known limitation

The LiteLLM key's model access list must include the models Claude Code requests. If the key restricts models (e.g., only `anthropic/claude-sonnet-4.6`), Claude Code will fail with a model access error. This is a LiteLLM proxy configuration concern, not a sandbox issue.

### Claude Code exit gate results

**16/16 tests passed.**

## 5.2 Pi

**Status:** Proven

### Pi auth path

Pi uses the `pi-provider-litellm` plugin to connect to the LiteLLM proxy:

- `LITELLM_BASE_URL` points to the LiteLLM proxy endpoint.
- `LITELLM_API_KEY` is the `litellm-inference` provider placeholder (stored key `LITELLM_API_KEY`).
- Pi is not pre-installed in the default OpenShell sandbox image. `agent-run.sh` installs it at sandbox creation via `npm install -g @earendil-works/pi-coding-agent` to `/sandbox/.npm-global`.

### npm registry access

The sandbox policy includes an `npm_registry` network policy allowing GET requests to `registry.npmjs.org` with `allow_encoded_slash: true` (required for scoped packages like `@earendil-works/pi-coding-agent`).

### Pi exit gate results

**16/16 tests passed.**

## 5.3 Codex CLI

**Status:** Skipped (user decision)

Codex CLI (`@openai/codex@0.117.0`) is pre-installed in the default OpenShell sandbox image. A builtin `codex` provider profile exists with OAuth credential fields. Setup would follow the same pattern as Claude Code but with OpenAI endpoints. Can be added when the user is ready.

## 5.4 OpenClaw

**Status:** Skipped — upstream blocker

OpenClaw/NemoClaw is not available:

- Not installed on the host (not in npm or pip)
- No OpenShell provider profile exists
- No public package found

Per the Phase 5 plan: "OpenClaw proven, or a concrete upstream blocker is documented without weakening the security model."

## Deliverables

| File | Purpose |
|---|---|
| `openshell/litellm-inference-profile.yaml` | Custom OpenShell provider profile for LiteLLM inference |
| `openshell/sandbox-policy.yaml` | Updated with LiteLLM, npm registry, and telemetry network policies |
| `scripts/agent-run.sh` | Updated with multi-agent support (claude, pi) and inference providers |
| `tests/phase5-exit-gate.sh` | Per-agent acceptance matrix test suite (16 tests each) |
| `docs/phase5-multi-agent.md` | This document |

## Common GitHub security policy

The shared policy from Phase 4 remains intact for all agents:

- Agent branches (`agent/**`): push allowed
- Protected branches (`main`, `feature/**`, etc.): push denied (GitHub rulesets)
- Tags: create/update/delete denied (GitHub rulesets)
- Merge APIs: denied (OpenShell L7 policy — REST PUT + GraphQL mutations)
- Unapproved repositories: access denied
- Credential containment: provider placeholders, not real tokens

No agent-specific weakening of the GitHub security policy was required.

## OpenShell findings

1. **Provider credential env var naming**: Credentials are injected as env vars using the credential `name` field, not the `env_vars` list. Multiple providers must use unique credential names to avoid conflicts.

2. **Encoded slash in L7 proxy**: npm scoped package URLs contain `%2F`. The default L7 proxy rejects encoded slashes. Fixed with `allow_encoded_slash: true` on the npm registry endpoint.

3. **Binary path warnings**: OpenShell warns about symlink resolution for binary paths that don't exist in the container at policy load time. These are non-blocking warnings.

4. **Claude Code auto-detection**: OpenShell requires the `claude-code` provider type when the `claude` binary is detected in the sandbox, even when using a custom inference endpoint.
