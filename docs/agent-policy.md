# Repository network policy

A repository can ask `agent-run` for HTTPS access to hosts beyond the baseline
policy by committing `.agent-policy.yaml` at its root. This page is the
reference for that manifest, the caller's grants file, and how `agent-run`
decides what a session gets. The summary is in the
[README](../README.md#repository-network-requests).

## Trust model

Three parties are involved, and only one of them is the repository:

1. **The repository requests.** `.agent-policy.yaml` is untrusted input from
   the repo. It lists hosts and a reason for each. It cannot choose a port,
   protocol, method, path or binary, and it never authorizes anything by
   itself.
2. **The caller authorizes.** Whoever runs `agent-run` grants requests with
   `--allow-host` flags or a grants file (`--grants FILE`). Both come from
   outside the repository. A grant only caps a request: a grant for a host the
   repo did not request adds nothing.
3. **`agent-run` enforces hard restrictions.** Some hosts can never be reached
   through a repo rule, whatever the manifest asks or the caller grants (see
   [Restricted hosts](#restricted-hosts)).

Every check fails closed and runs before any sandbox resource exists. A
malformed manifest, a required request with no grant, or a request or grant
for a restricted host stops the run while there is nothing to clean up but the
GitHub token, which is revoked. After the sandbox starts, `agent-run` checks
that the loaded policy contains exactly the granted `repo_*` rules (none
missing, none extra, each for its own host only) before it launches the agent.

OpenShell's runtime denials are the final boundary. The compiled policy is
what OpenShell enforces for every connection: a request outside a rule's host,
method or path is refused (`policy_denied`) no matter what the agent does.

Keep the grants file somewhere the repository cannot write to, such as your
own config directory. Do not point `--grants` at a file inside a checkout of
the repository being run: that would let the repo grant itself.

## `.agent-policy.yaml`

```yaml
version: 1
network:
  - host: docs.example.org
    reason: "fetch API reference docs"
  - host: example.org
    reason: "download test fixtures"
    optional: true
```

| Key | Required | Meaning |
|---|---|---|
| `version` | yes | Must be `1`. |
| `network` | no | List of requests, at most 50. |
| `network[].host` | yes | Bare lowercase DNS name: no scheme, port, path, userinfo, wildcard or trailing dot. Each host at most once. |
| `network[].reason` | yes | Why the repo needs it, shown to the caller. Printable text, at most 256 bytes. |
| `network[].optional` | no | `false` (default): the request is required. `true`: it is optional. |

The parser is strict. It rejects files over 64 KiB, more than one YAML
document, anchors, aliases, merge keys, explicit tags, duplicate keys, unknown
keys at any level and any top-level section other than `version` and
`network`. There is no `filesystem` section; a manifest that has one fails.

A repository without `.agent-policy.yaml` runs with the baseline policy,
exactly as before. `--no-repo-policy` ignores the manifest even when one
exists.

### Required and optional requests

- A **required** request (the default) with no grant fails the run before any
  sandbox is created, with a hint naming the `--allow-host` flag or grants
  entry that would authorize it.
- An **optional** request with no grant is left out with a warning, and the
  session runs without it:

  ```text
  [agent-run] WARN: optional request for example.org ("download test fixtures") is not granted and was omitted; authorize it with --allow-host example.org or a grants file entry for host example.org
  ```

A request for a [restricted host](#restricted-hosts) fails the run even when
it is optional.

## Grants

### `--allow-host`

`--allow-host HOST` (repeatable) grants the repository's request for `HOST`
for this run: read-only (`GET` and `HEAD`) on every path (`/**`), for any
repository.

### Grants file

`--grants FILE` reads grants from a file. It can be combined with
`--allow-host`.

```yaml
version: 1
grants:
  - host: docs.example.org
  - host: api.example.org
    repos: [example/other-repo]
    methods: [GET, HEAD, POST]
    paths: ["/v1/search", "/v1/items/**"]
```

| Key | Required | Meaning |
|---|---|---|
| `version` | yes | Must be `1`. |
| `grants` | no | List of grants, at most 200. |
| `grants[].host` | yes | DNS name, as for a request (lowercased on load). |
| `grants[].repos` | no | `owner/name` repositories the grant applies to (case-insensitive). Omitted: every repository. |
| `grants[].methods` | no | Any of `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`. Default `[GET, HEAD]`. |
| `grants[].paths` | no | 1 to 32 absolute URL paths (at most 256 bytes each), with `*` and `**` globs. No query, fragment or `.`/`..` segments. Default `["/**"]`. |

The file uses the same strict YAML rules as the manifest, with a 256 KiB
limit.

Grants are read-only by default. To allow more, list the methods and narrow
the paths, as for `api.example.org` above. Keep the widening as small as the
task needs: `GET` is not read-only on every service, and every method you add
applies to every listed path.

When several grants cover the same host and repository, the session gets the
union of their methods and paths. A grant limited to other repositories, or
for a host the repository did not request, adds nothing and is reported as
`info`. A grant for a restricted host is an error even if nothing requests it.

## The generated rule

Each granted request becomes one `network_policies` entry named `repo_` plus
the host with dots replaced by underscores (`example.org` becomes
`repo_example_org`). The rule:

- allows only that host, on port 443, with `protocol: rest` and
  `enforcement: enforce` (L7: every request's method and path are checked);
- has one `allow` per granted method and path;
- binds only the harness's own binaries (`/usr/local/bin/claude` and
  `/usr/local/bin/node` for claude, `node` for pi, `/usr/local/bin/opencode`
  for opencode). Tools the agent spawns match through OpenShell's ancestor
  matching; processes outside the agent's tree, such as `agent-run`'s own
  `git`, `gh` and `mise` steps or a bare `openshell sandbox exec`, do not;
- has no credential binding: no provider secret is ever sent to the host.

The rule is inserted next to the baseline rules without changing any of them,
and the composed policy is decoded again to prove every baseline rule is
unchanged.

## Restricted hosts

These hosts are refused before any grant is considered. Neither the manifest
nor any grant can override them:

| Restriction | Examples |
|---|---|
| Not a bare DNS name: a URL, scheme, port, path, query, fragment, userinfo or wildcard (the manifest must also be lowercase; grants are lowercased) | `https://example.org`, `example.org:8443`, `*.example.org`, `user@example.org` |
| IP address literals, including forms only resolvers read as IPv4 (a numeric last label) | `169.254.169.254`, `[::1]`, `127.1`, `2130706433`, `0x7f.0.0.1` |
| Names that embed an IPv4 address (four numeric labels in a row, as wildcard DNS services use) | `10.0.0.1.nip.io` |
| Cloud metadata names | `metadata.goog`, `metadata.google.internal`, `instance-data.ec2.internal` |
| `localhost` and single-label names | `localhost`, `intranet` |
| Local and internal suffixes: `.localhost`, `.local`, `.cluster.local`, `.internal`, `.svc`, `.home.arpa`, `.lan` | `printer.local`, `api.default.svc` |
| Any host the rendered baseline policy already governs, including the temporary mise bootstrap hosts and `--mise-allow-host` values | `api.github.com`, `registry.npmjs.org`, `api.anthropic.com`, `dl.k8s.io` |
| GitHub domains and their subdomains: `github.com`, `githubusercontent.com`, `github.io`, `ghcr.io` | `gist.github.com`, `raw.githubusercontent.com` |

Baseline hosts are restricted because a second rule for the same host would
broaden the baseline's per-binary, per-path scoping (for example, the
repository-scoped GitHub API rules). The bootstrap hosts stay restricted
after the bootstrap rule is removed, so a repo cannot keep install egress for
the agent.

## `--explain-policy`

`--explain-policy` runs the preflight and the compile, prints every input and
decision, and exits. It creates no sandbox, provider or cache, clones nothing,
and revokes the GitHub App token it minted. It exits non-zero when a session
would be rejected.

```bash
agent-run example/other-repo claude --explain-policy --allow-host example.org
```

```text
Repository:  example/other-repo
Commit:      0123456789abcdef0123456789abcdef01234567 (main)
Harness:     claude
Manifest:    .agent-policy.yaml at 0123456789abcdef0123456789abcdef01234567
Grants:
  --allow-host example.org: example.org GET,HEAD on /**
Decisions:
  granted example.org: authorized by --allow-host example.org [rule repo_example_org; GET,HEAD on /**]
Diagnostics:
  (none)
Result:      ok, 1 repo rule(s) for example.org
```

Pass the same flags you would pass to the session (`--grants`, `--allow-host`,
`--mise-allow-host`, the harness and its options) so the result matches.

## SHA pinning

The preflight resolves the repository's default branch to a commit SHA
through the GitHub API, then reads `.agent-policy.yaml` at that SHA (the
contents API with `?ref=<sha>`; a 404 means no manifest, any other error fails
the run). Inside the sandbox, `agent-run` clones the repository and checks out
the default branch at exactly that SHA. If the branch moved after the
preflight and the clone does not contain the commit (for example after a
force-push), it fetches the commit by SHA first. It then verifies that `HEAD`
is the preflight SHA, so the policy always comes from the commit the agent
works on. A push to the branch during the run changes nothing, and a manifest
the agent edits during the session grants nothing to that session.

## Symlinked manifests

`.agent-policy.yaml` is read through GitHub's contents API, which follows a
symlink whose target is a regular file in the same repository: the response
is the target file's content (checked against a public repository whose
root-level symlink points into a subdirectory). A symlinked manifest is
therefore read from its target at the same pinned commit. That grants the repo
nothing it could not do with a regular file, and the content goes through
the same strict parser. GitHub documents that a symlink it cannot resolve to
a file in the repository (a target outside the repository, a directory or a
missing file) is returned as a description of the symlink instead. That case
was not checked live, but whatever comes back is not a valid manifest (it is
not a mapping with only `version` and `network`), so the run fails.
