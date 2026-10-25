# shellcheck shell=bash
# Policy matrix cases for tests/policy-matrix.sh (sourced; not executable).
#
#   pm_case ID VIA METHOD URL BODY CURRENT HARDENED "WHY"
#
# VIA     process that opens the connection; see probe.sh (curl node python git gh fs)
# URL     {OWNER}/{REPO} expand to the target repo. For VIA=fs, URL is a directory
#         and METHOD is "write".
# BODY    none | json-empty | graphql-viewer | graphql-merge
# CURRENT expected decision with today's baseline: allow | deny | record
# HARDENED expected decision after baseline hardening (PR 2): allow | deny | record
#         "record" = report what happened without a verdict (decision still open).
#
# Every probe must be harmless if the policy ALLOWS it: OpenShell decides on
# host, method and path pattern, so probes use paths that match the rule but
# name nothing real (nonexistent refs, PR 0, made-up endpoints) or empty bodies
# the upstream rejects. Never add a probe that would change anything upstream
# when allowed.

# --- GitHub via gh (bound to github_api, github_graphql, the github-agent provider)
pm_case gh-own-repo-read        gh GET    "https://api.github.com/repos/{OWNER}/{REPO}"                                         none           allow allow  "agent reads its own repo"
pm_case gh-own-pr-create        gh POST   "https://api.github.com/repos/{OWNER}/{REPO}/pulls"                                   json-empty     allow allow  "agent opens PRs"
pm_case gh-own-comment          gh POST   "https://api.github.com/repos/{OWNER}/{REPO}/issues/0/comments"                       json-empty     allow allow  "agent comments"
pm_case gh-own-pr-patch         gh PATCH  "https://api.github.com/repos/{OWNER}/{REPO}/pulls/0"                                 json-empty     allow allow  "PATCH any PR, incl. human ones (kept: reversible, attributed to the App)"
pm_case gh-own-ref-delete-agent gh DELETE "https://api.github.com/repos/{OWNER}/{REPO}/git/refs/heads/agent/policy-matrix-none"  none           allow allow  "agent deletes its own agent/** branches"
pm_case gh-own-ref-delete-other gh DELETE "https://api.github.com/repos/{OWNER}/{REPO}/git/refs/heads/policy-matrix-none"        none           deny  deny   "ref delete outside agent/** (default branch class)"
pm_case gh-own-tag-delete       gh DELETE "https://api.github.com/repos/{OWNER}/{REPO}/git/refs/tags/policy-matrix-none"         none           deny  deny   "tag delete"
pm_case gh-rest-merge           gh PUT    "https://api.github.com/repos/{OWNER}/{REPO}/pulls/0/merge"                           json-empty     deny  deny   "REST merge"
pm_case gh-graphql-query        gh POST   "https://api.github.com/graphql"                                                      graphql-viewer allow allow  "GraphQL queries"
pm_case gh-graphql-merge        gh POST   "https://api.github.com/graphql"                                                      graphql-merge  deny  deny   "GraphQL mergePullRequest"
pm_case gh-other-repo-read      gh GET    "https://api.github.com/repos/octocat/Hello-World"                                    none           deny  deny   "another repo via the API (was open via the provider profile)"
pm_case gh-other-ref-delete     gh DELETE "https://api.github.com/repos/octocat/Hello-World/git/refs/heads/policy-matrix-none"  none           deny  deny   "ref delete on another repo"
pm_case gh-user                 gh GET    "https://api.github.com/user"                                                         none           allow allow  "identity"
pm_case gh-user-repos           gh GET    "https://api.github.com/user/repos"                                                   none           deny  deny   "GET /user/** enumerates repos/emails/keys"
pm_case gh-user-repo-create     gh POST   "https://api.github.com/user/repos"                                                   json-empty     deny  deny   "create repo"

# --- GitHub bypass attempts: other binaries, and children of git
pm_case curl-graphql-merge      curl POST "https://api.github.com/graphql"                                                      graphql-merge  deny  deny   "curl is not a GitHub binary"
pm_case node-github-read        node GET  "https://api.github.com/repos/{OWNER}/{REPO}"                                         none           deny  deny   "agent tree (node) has no GitHub rule"
pm_case git-graphql-merge       git  POST "https://api.github.com/graphql"                                                      graphql-merge  deny  deny   "git child skips github_graphql deny (gh-only rule)"
pm_case git-rest-merge          git  PUT  "https://api.github.com/repos/{OWNER}/{REPO}/pulls/0/merge"                           json-empty     deny  deny   "git child REST merge"
pm_case git-own-ref-delete      git  DELETE "https://api.github.com/repos/{OWNER}/{REPO}/git/refs/heads/policy-matrix-none"     none           deny  deny   "git child deletes a non-agent ref"
pm_case git-own-upload-pack     git  GET  "https://github.com/{OWNER}/{REPO}.git/info/refs?service=git-upload-pack"             none           allow allow  "clone/fetch own repo"
pm_case git-other-upload-pack   git  GET  "https://github.com/octocat/Hello-World.git/info/refs?service=git-upload-pack"        none           deny  deny   "fetch another repo"
pm_case git-other-receive-pack  git  POST "https://github.com/octocat/Hello-World.git/git-receive-pack"                          none           deny  deny   "push to another repo"
pm_case git-github-releases     git  GET  "https://github.com/cli/cli/releases"                                                 none           deny  deny   "release downloads after bootstrap"

# --- Inference and telemetry, from the agent's process tree (node ancestor)
pm_case node-anthropic-messages node POST "https://api.anthropic.com/v1/messages"                                               json-empty     allow allow  "inference"
pm_case node-anthropic-files    node POST "https://api.anthropic.com/v1/files"                                                  json-empty     allow deny   "upload API reachable (exfil with any key)"
pm_case curl-anthropic-files    curl POST "https://api.anthropic.com/v1/files"                                                  json-empty     record deny  "anthropic provider binds bare curl"
pm_case node-sentry             node POST "https://sentry.io/api/0/policy-matrix-none/"                                         json-empty     allow deny   "anyone can own a Sentry project"
pm_case node-statsig            node POST "https://statsig.anthropic.com/policy-matrix-none"                                    json-empty     allow record "telemetry"
pm_case node-claude-platform    node POST "https://platform.claude.com/policy-matrix-none"                                      json-empty     allow record "platform"
pm_case node-litellm-other      node POST "https://litellm.internal.keener.us/policy-matrix-none"                               json-empty     allow deny   "LiteLLM non-inference paths"
pm_case curl-litellm            curl POST "https://litellm.internal.keener.us/v1/chat/completions"                              json-empty     allow deny   "bare curl bound to litellm"
pm_case python-litellm          python POST "https://litellm.internal.keener.us/v1/chat/completions"                            json-empty     allow deny   "python3.12 bound to litellm"

# --- Hosts nobody is granted
pm_case curl-unlisted           curl GET  "https://example.com/"                                                                none           deny  deny   "unlisted host"
pm_case node-unlisted           node GET  "https://example.com/"                                                                none           deny  deny   "unlisted host from agent tree"
pm_case curl-metadata           curl GET  "http://169.254.169.254/latest/meta-data/"                                            none           deny  deny   "cloud metadata"

# --- Package registries
pm_case node-npm-read           node GET  "https://registry.npmjs.org/left-pad"                                                 none           allow allow  "npm installs"
pm_case node-npm-write          node PUT  "https://registry.npmjs.org/policy-matrix-none"                                       json-empty     deny  deny   "npm publish"
pm_case curl-npm                curl GET  "https://registry.npmjs.org/left-pad"                                                 none           deny  deny   "npm rule is node-only"
pm_case curl-pypi               curl GET  "https://pypi.org/simple/"                                                            none           deny  deny   "pypi rule is uv-only"

# --- Bootstrap egress must be gone after narrowing
pm_case curl-dl-k8s             curl GET  "https://dl.k8s.io/release/stable.txt"                                                none           deny  deny   "mise_bootstrap removed"
pm_case node-gcs                node GET  "https://storage.googleapis.com/"                                                     none           deny  deny   "mise_bootstrap removed"

# --- Filesystem (Landlock + image)
pm_case fs-sandbox              fs   write "/sandbox"                                                                           none           allow allow  "workspace"
pm_case fs-tmp                  fs   write "/tmp"                                                                               none           allow allow  "scratch"
pm_case fs-usr                  fs   write "/usr"                                                                               none           deny  deny   "read-only system"
pm_case fs-etc                  fs   write "/etc"                                                                               none           deny  deny   "read-only config"
