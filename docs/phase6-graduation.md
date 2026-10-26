# Phase 6 — Graduation

## Summary

Phase 6 proves the full agent workflow can produce review-ready pull requests
on the target repository with no human GitHub authentication or YubiKey
interaction. This graduation was performed against `jlaska/agent-sandbox-test`.

## Adapted scope

All tasks were executed against `jlaska/agent-sandbox-test` — the same
repository used in Phases 0–5. This means:

- **6.1 (Inspect repository)** — conventions are already well understood from
  prior phases.
- **6.2 (Add to GitHub App installation)** — the App has been installed on
  this repository since Phase 1.
- **6.3 (Extend allowlists/policies)** — allowlists and OpenShell policies
  have been in place since Phase 4.
- **6.4 (Review-ready PR proof)** — the full workflow is proven by the
  Phase 6 exit gate test and the security regression matrix.

Using `agent-run` on another repository needs the App installed on it and the
same rulesets; `agent-run` generates the repo-specific OpenShell network policy
for each session.

## 6.1 Repository conventions

| Convention | Status |
|---|---|
| Default branch | `main` |
| Agent branch namespace | `agent/**` |
| Pre-commit/validation | None (trivial proving repo) |
| CI/checks | None configured |
| Bot access (Renovate, etc.) | Not applicable |

## 6.2 GitHub App installation

| Item | Status |
|---|---|
| App installed on repo | ✅ Phase 1 |
| Repository-only installation (not all repos) | ✅ |
| Rulesets applied | ✅ Phase 1 |

### Active rulesets

| Ruleset | Target | Enforcement | App bypass |
|---|---|---|---|
| Protect default branch | `main` | Active | None |
| Protect tags | All tags | Active | None (admin only) |
| Restrict non-agent branches | All except `main`, `agent/*`, `agent/**/*` | Active | None (admin only) |

## 6.3 Allowlists and policies

| Item | Status |
|---|---|
| `agent-run` approved repos | `jlaska/agent-sandbox-test` ✅ |
| OpenShell Git policy (clone/fetch/push) | Scoped to `jlaska/agent-sandbox-test` ✅ |
| OpenShell API policy (REST) | Scoped to `jlaska/agent-sandbox-test` ✅ |
| OpenShell GraphQL policy | Queries allowed, merge mutations denied ✅ |
| Merge API blocked | REST PUT + GraphQL `mergePullRequest` + `enablePullRequestAutoMerge` ✅ |

## 6.4 Review-ready PR proof

The Phase 6 exit gate test (`tests/phase6-exit-gate.sh`, since replaced by
`tests/workflow-gate.sh`) proves the complete
workflow inside an OpenShell sandbox:

1. Clone the approved repository
2. Create an `agent/**` branch
3. Commit without YubiKey
4. Push the agent branch
5. Create a **draft** PR
6. Comment on the PR
7. Submit a PR review
8. Inspect checks
9. Mark the PR **ready for review**
10. Verify merge is blocked (REST, GraphQL, and `gh pr merge`)
11. Verify unauthorized operations fail (main push, tags, unapproved repos)
12. Verify credential containment (placeholder, not real token)
13. Verify inference connectivity

## Supported agent results

| Agent | Security tests | Inference | Status |
|---|---|---|---|
| Claude Code | 16/16 pass | LiteLLM (HTTP 200) | ✅ Proven |
| Pi | 16/16 pass | LiteLLM (HTTP 200) | ✅ Proven |
| Codex CLI | — | — | Skipped (not installed) |
| OpenClaw | — | — | Skipped (upstream blocker) |

## Exit gate

| Requirement | Status |
|---|---|
| At least one review-ready PR proves the full workflow | ✅ |
| All supported agents have documented results | ✅ |
| Human merge/review remains the handoff boundary | ✅ |
| Security regression matrix passes | ✅ (Claude 16/16, Pi 16/16) |
| No YubiKey or human GitHub credential used | ✅ |

## Deliverables

| File | Purpose |
|---|---|
| `tests/phase6-exit-gate.sh` | Graduation exit gate test (prerequisites + security regression + PR proof); since replaced by `tests/workflow-gate.sh` |
| `docs/phase6-graduation.md` | This document |

## Using agent-run on another repository

1. **Add the repository to the App installation** (GitHub UI — USER ACTION)
2. **Apply rulesets** matching this repository's configuration
3. **Verify Renovate** and existing automation still work
4. **Run the exit gate**: `./tests/workflow-gate.sh --repo <owner>/<repo>`
5. **Inspect the repository's conventions** (build, deploy and pre-commit
   tooling) and adapt the sandbox policy if additional network endpoints are
   needed (e.g., container registries, package repositories)
