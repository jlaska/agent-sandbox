# Phase 4 - Secure GitHub/OpenShell Integration + `agent-run`

> Historical record for OpenShell 0.0.113. `agent-run` now requires OpenShell 0.1.4 or newer, which has no in-place upgrade from 0.0.x; see "Upgrading from OpenShell 0.0.x" in the [README](../README.md).

## Overview

Phase 4 joins the GitHub App authorization model (Phase 1-2) with the local
OpenShell sandbox (Phase 3) to produce a complete, secure agent workflow.

## Components

### OpenShell Version

- **OpenShell:** 0.0.113
- **Gateway:** local, mTLS-authenticated (`https://localhost:17670`)

### Custom Provider Profile

File: `openshell/github-agent-profile.yaml`

A custom OpenShell provider profile (`github-agent`) extends the built-in
`github` profile with:

- **Git push** (`git-receive-pack`) for `jlaska/agent-sandbox-test`
- **REST API** write rules scoped to the approved repository
- **GraphQL** read-only queries (mutations handled via REST)
- **Credential injection** via `api_token` placeholder resolved by the gateway

The credential is injected as `api_token=openshell:resolve:env:...` — a
placeholder that the gateway resolves to the real token on outbound requests.
`GITHUB_TOKEN`/`GH_TOKEN` are intentionally NOT set in the sandbox.

Git authentication uses a credential helper that returns the placeholder:

```bash
git config credential.helper \
  '!f() { echo "username=x-access-token"; echo "password=${api_token}"; }; f'
```

### Sandbox Policy

File: `openshell/sandbox-policy.yaml`

The sandbox policy defines three network policy groups:

| Policy | Host | Purpose |
|---|---|---|
| `github_git` | `github.com:443` | Git Smart HTTP (clone/fetch/push) |
| `github_api` | `api.github.com:443` | REST API (PRs, comments, reviews) |
| `github_graphql` | `api.github.com:443/graphql` | GraphQL API (queries + PR mutations) |

#### Git operations (github_git)

- `GET /jlaska/agent-sandbox-test.git/info/refs*`
- `POST /jlaska/agent-sandbox-test.git/git-upload-pack` (fetch)
- `POST /jlaska/agent-sandbox-test.git/git-receive-pack` (push)

Only the approved repository's push path is allowed. Other repositories are
denied at the L7 layer.

#### REST API (github_api)

Allowed:

- `GET /repos/jlaska/agent-sandbox-test/**` (read metadata, contents)
- `POST /repos/.../pulls` (create PR)
- `PATCH /repos/.../pulls/*` (update PR)
- `POST /repos/.../issues/*/comments` (PR comments)
- `POST /repos/.../pulls/*/reviews` (PR reviews)
- `DELETE /repos/.../git/refs/**` (branch cleanup)
- `GET /user`, `/rate_limit`, `/app` (identity, rate limits)

No `PUT` method is allowed — this implicitly blocks the merge REST endpoint.

#### GraphQL API (github_graphql)

Allowed:

- All queries (read operations)
- Mutations: `createPullRequest`, `updatePullRequest`, `addComment`,
  `addPullRequestReview`, `submitPullRequestReview`, `requestReviews`,
  `closePullRequest`, `reopenPullRequest`, `markPullRequestReadyForReview`,
  `convertPullRequestToDraft`

Denied (deny_rules):

- `mergePullRequest`
- `enablePullRequestAutoMerge`

### Merge Blocking

Merge is blocked by two independent controls:

1. **GitHub rulesets** prevent direct push to `main`
2. **OpenShell L7 policy** blocks:
   - REST `PUT /repos/*/pulls/*/merge`
   - GraphQL `mergePullRequest` mutation
   - GraphQL `enablePullRequestAutoMerge` mutation

This addresses the finding from Phase 1 that the REST merge API succeeded
despite rulesets (because the merge API bypasses push rules).

### `agent-run` Launcher

File: `scripts/agent-run.sh`

Usage:

```bash
agent-run <owner/repo> <agent>
agent-run --diag
agent-run --list-repos
```

The launcher performs:

1. Validates repository against an explicit allowlist
2. Verifies OpenShell gateway is running
3. Mints a fresh App installation token (via `scripts/mint-token.sh`)
4. Creates an OpenShell GitHub provider with the token
5. Generates a repo-specific sandbox policy (from template)
6. Creates an ephemeral sandbox with provider and policy
7. Waits for sandbox readiness
8. Clones the target repository via HTTPS
9. Configures sandbox-local Git identity and credential helper
10. Launches the requested agent
11. On exit (trap-guaranteed): revokes token, deletes sandbox, deletes provider

Diagnostics mode (`--diag`) prints:

- OpenShell and gh-token versions
- Gateway status
- App metadata
- Active sandboxes and providers

It never prints: private key, tokens, model API keys.

## Security Properties

| Property | How enforced |
|---|---|
| Agent identity separated from human | GitHub App installation token (not user SSH/PAT) |
| Private key trusted-host-only | macOS Keychain; never enters sandbox |
| Fresh short-lived token per session | `gh-token` mints; `agent-run` revokes on exit |
| Credential containment | Sandbox receives placeholder; gateway resolves on proxy |
| Agent branch namespace only | GitHub rulesets + OpenShell L7 policy |
| No merge | GitHub rulesets (push) + OpenShell L7 (REST/GraphQL merge APIs) |
| Unapproved repos blocked | OpenShell L7 path rules + App installation scope |
| Human Git unchanged | No global Git config changes; signing disabled only in sandbox |

## Exit Gate Test Results

Tested 2026-08-26T18:44:10Z.

| Test | Expected | Result |
|---|---|---|
| Start without YubiKey | PASS | PASS |
| Clone/fetch approved repo | PASS | PASS |
| Push `agent/*` (flat) | PASS | PASS |
| Push `agent/*/` (nested) | PASS | PASS |
| Create PR | PASS | PASS |
| Comment on PR | PASS | PASS |
| Submit PR review | PASS | PASS |
| Inspect checks | PASS | PASS |
| Push `main` | FAIL | PASS (denied by rulesets) |
| Push unauthorized branch | FAIL | PASS (denied by rulesets) |
| Tag operations | FAIL | PASS (denied by rulesets) |
| `gh pr merge` | FAIL | PASS (denied by L7 policy) |
| REST merge endpoint | FAIL | PASS (denied by L7 policy) |
| GraphQL `mergePullRequest` | FAIL | PASS (denied by L7 policy) |
| GraphQL `enableAutoMerge` | FAIL | PASS (denied by L7 policy) |
| Real token recoverable | FAIL | PASS (placeholder only) |
| `GITHUB_TOKEN` set in sandbox | FAIL | PASS (not set) |
| Private key in sandbox | FAIL | PASS (not present) |
| Unapproved repo access | FAIL | PASS (denied by L7 policy) |
| Human Git unchanged | PASS | PASS |

All 20 tests passed. 0 failures.

## Key Findings

### Credential injection via provider profile

The OpenShell provider injects the credential as `api_token` (the credential
name from the profile), not as `GITHUB_TOKEN` or `GH_TOKEN`. The `env_vars`
field in the profile specifies which host env vars to read FROM, not which
sandbox env vars to set.

Git authentication requires a manual credential helper configuration that
maps the `api_token` placeholder to the git credential protocol.

### Policy vs profile separation

- **Provider profile**: defines endpoint declarations and credential injection
  rules. The profile's rules determine which requests receive credential headers.
- **Sandbox policy**: defines L7 network allow/deny rules. The policy controls
  which requests are permitted through the gateway.

Both must agree for a request to succeed with credentials.

### GraphQL mutation control

OpenShell supports fine-grained GraphQL mutation control via `fields` in
allow rules and `deny_rules`. The `deny_rules` take precedence over allow
rules for matching fields. The `name` in deny rules maps to the GraphQL
field name (e.g., `mergePullRequest`), not the operation name from the query
document.
