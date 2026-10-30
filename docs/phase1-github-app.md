# Phase 1 - GitHub App and Rulesets

## GitHub App

- **App name:** jlaska-agent
- **App ID:** 4720923
- **App slug:** jlaska-agent
- **Installation ID:** 156618153
- **Installed on:** `jlaska/agent-sandbox-test` only (not all repositories)

### Repository permissions

| Permission | Access |
|---|---|
| Metadata | Read-only |
| Contents | Read and write |
| Pull requests | Read and write |
| Issues | Read-only |
| Checks | Read-only |
| Commit statuses | Read-only |
| Actions | Read-only |

Every session token requests exactly this set (`installationTokenPermissions`
in `internal/agentrun/token.go`), so it never carries a permission the App
gains later. agent-run rejects a grant broader than the request, and fails with
a clear error if the App lacks one of these permissions (GitHub answers 422).

**Workflows** and **Administration** must stay off. With Workflows write, an
agent could push `.github/workflows/*` to an `agent/**` branch, and a same-repo
PR would run it with the repository's Actions secrets.

## GitHub Rulesets

Three rulesets protect the scratch repository:

### 1. Protect default branch (ID: 21516245)

- **Target:** `refs/heads/main`
- **Rules:** require PR (0 approvals minimum), block force-push, block deletion
- **Bypass actors:** none (App and all non-admin actors are subject to these rules)

### 2. Protect tags (ID: 21516364)

- **Target:** all tags (`~ALL`)
- **Rules:** block creation, update, deletion
- **Bypass actors:** Repository admin (role ID 5) only

### 3. Restrict non-agent branches (ID: 21516387)

- **Target:** all branches except `main` and `agent/**`
- **Rules:** block creation, update, deletion, force-push
- **Bypass actors:** Repository admin (role ID 5) only

### fnmatch note

GitHub's `**` in ref patterns does not span `/` in all contexts. The
exclude list uses both `refs/heads/agent/*` and `refs/heads/agent/**/*`
to cover single-level and nested agent branches. Verified working for:

- `agent/test` (flat)
- `agent/claude/test` (two levels)
- `agent/tasks/123/fix` (three levels)

## Ruleset verification matrix

| Branch | Rules applied | Effect for App |
|---|---|---|
| `agent/test` | none | allowed |
| `agent/claude/test` | none | allowed |
| `agent/tasks/123/fix` | none | allowed |
| `feature/foo` | creation, update, deletion, non_fast_forward | blocked |
| `release/foo` | creation, update, deletion, non_fast_forward | blocked |
| `main` | pull_request, non_fast_forward, deletion | must use PR |
| any tag | creation, update, deletion | blocked |

## Phase 1.3 Authorization Test Results

Tested 2026-08-26 by authenticating as the App with a minted installation token.

| Test | Expected | Result |
|---|---|---|
| Clone approved repo (HTTPS) | PASS | PASS |
| Push `agent/test` | PASS | PASS |
| Push `agent/foo/bar` | PASS | PASS |
| Push `agent/tasks/123/fix` | PASS | PASS |
| Push `main` | FAIL | FAIL (GH013: rule violation) |
| Push `feature/foo` | FAIL | FAIL (GH013: rule violation) |
| Push `release/foo` | FAIL | FAIL (GH013: rule violation) |
| Create tag | FAIL | FAIL |
| Force-push `main` (divergent) | FAIL | FAIL (GH013: non_fast_forward) |
| Delete `main` | FAIL | FAIL |
| Create PR | PASS | PASS |
| Comment on PR | PASS | PASS |
| Read PR checks | PASS | PASS |
| `gh pr merge` | FAIL | FAIL |
| REST merge endpoint | FAIL | **PASS (merged)** |
| GraphQL `mergePullRequest` | FAIL | FAIL (already merged) |
| GraphQL `enableAutoMerge` | FAIL | FAIL (not allowed for repo) |
| Clone unapproved repo | FAIL | FAIL |

### Merge API finding

The REST `PUT /repos/{owner}/{repo}/pulls/{number}/merge` endpoint succeeded.
This confirms the issue's architectural analysis: GitHub rulesets block direct
pushes to `main` but do **not** block the PR merge API when the App has
`Contents: write`. The merge API bypasses push rules because it operates
through GitHub's internal merge process.

`gh pr merge` failed because the CLI attempted a merge method not meeting
the PR requirements (0 approvals needed but some other constraint), while
the direct REST call succeeded.

**Mitigation:** OpenShell L7 policy (Phase 4) must block merge REST/GraphQL
endpoints. This is the intended design per the issue's security analysis.
