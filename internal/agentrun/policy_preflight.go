package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
)

// The policy preflight runs after the GitHub credential is resolved and
// before any sandbox resource (profiles, providers, cache, sandbox) exists.
// It pins the default branch to a commit, reads that commit's
// .agent-policy.yaml through the GitHub API and compiles the session policy,
// so a manifest or grant that cannot be honoured fails the run while there is
// still nothing to clean up but the token. The sandbox then clones exactly
// that commit (pinClone). OpenShell's runtime denials stay the final
// enforcement boundary.

// maxGitHubJSONBytes bounds the GitHub API metadata responses the preflight
// reads (repository and ref objects are a few KiB).
const maxGitHubJSONBytes = 1 << 20

// resolveGitHubCredentialFn is ResolveGitHubCredential, replaceable in tests.
var resolveGitHubCredentialFn = ResolveGitHubCredential

// policyPreflight is the outcome of runPolicyPreflight.
type policyPreflight struct {
	Repo          string
	Harness       string
	DefaultBranch string
	SHA           string // the commit whose manifest was compiled; the clone is pinned to it
	// ManifestSource says where the manifest came from: the repo path at SHA,
	// "none" (no manifest) or "ignored (--no-repo-policy)".
	ManifestSource string
	Grants         []Grant
	// Result is set once CompilePolicy ran, also when it failed.
	Result *CompileResult
}

// loadCallerGrants turns --allow-host and --grants into grants. It reads no
// repository data, so it runs (and fails) before any credential is minted.
func loadCallerGrants(cfg *Config) ([]Grant, error) {
	grants, err := AllowHostGrants(cfg.AllowHosts)
	if err != nil {
		return nil, err
	}
	if cfg.GrantsFile != "" {
		fromFile, err := LoadGrantsFile(cfg.GrantsFile)
		if err != nil {
			return nil, err
		}
		grants = append(grants, fromFile...)
	}
	return grants, nil
}

// runPolicyPreflight pins the default branch, fetches and parses the
// manifest (unless cfg.NoRepoPolicy) and compiles the session policy. The
// returned preflight is non-nil whenever the compile ran, so its decisions
// can be printed on failure too.
func runPolicyPreflight(ctx context.Context, cfg *Config, token string, providers SessionProviders, grants []Grant) (*policyPreflight, error) {
	branch, sha, err := resolveDefaultBranchSHA(ctx, token, cfg.Repo)
	if err != nil {
		return nil, fmt.Errorf("policy preflight: %w", err)
	}
	pf := &policyPreflight{Repo: cfg.Repo, Harness: cfg.Harness, DefaultBranch: branch, SHA: sha, Grants: grants}

	var manifest *AgentPolicy
	switch {
	case cfg.NoRepoPolicy:
		pf.ManifestSource = "ignored (--no-repo-policy)"
	default:
		data, found, err := fetchAgentPolicy(ctx, token, cfg.Repo, sha)
		if err != nil {
			return nil, fmt.Errorf("policy preflight: %w", err)
		}
		if !found {
			pf.ManifestSource = "none"
			break
		}
		pf.ManifestSource = fmt.Sprintf("%s at %s", AgentPolicyFile, sha)
		if manifest, err = ParseAgentPolicy(data); err != nil {
			return pf, fmt.Errorf("policy preflight: %s@%s: %w", cfg.Repo, sha, err)
		}
	}

	res, err := CompilePolicy(CompileInput{
		Repo:      cfg.Repo,
		Harness:   cfg.Harness,
		Providers: providers,
		MiseHosts: cfg.MiseAllowHosts,
		Manifest:  manifest,
		Grants:    grants,
	})
	pf.Result = res
	return pf, err
}

// repoRules maps each granted repo_* rule key to its host: exactly the
// repo rules the loaded policy must contain (verifyNarrowed).
func (pf *policyPreflight) repoRules() map[string]string {
	rules := map[string]string{}
	if pf == nil || pf.Result == nil {
		return rules
	}
	for _, d := range pf.Result.Decisions {
		if d.Outcome == OutcomeGranted {
			rules[d.Rule] = d.Host
		}
	}
	return rules
}

// grantedHosts returns the granted hosts, sorted.
func (pf *policyPreflight) grantedHosts() []string {
	var hosts []string
	for _, h := range pf.repoRules() {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

// printSession prints the preflight for a session run: the manifest source,
// warnings and info lines and a one-line summary on success, or every
// decision and diagnostic on failure.
func (pf *policyPreflight) printSession(failed bool) {
	fmt.Printf("[agent-run] Pinned %s to commit %s.\n", pf.DefaultBranch, pf.SHA)
	switch pf.ManifestSource {
	case "none":
		fmt.Printf("[agent-run] No %s at %s; using the baseline policy.\n", AgentPolicyFile, shortSHA(pf.SHA))
	case "ignored (--no-repo-policy)":
		fmt.Printf("[agent-run] --no-repo-policy: ignored the repository's %s; using the baseline policy.\n", AgentPolicyFile)
	default:
		fmt.Printf("[agent-run] Repository policy: %s\n", pf.ManifestSource)
	}
	if pf.Result == nil {
		return
	}
	if failed && len(pf.Result.Decisions) > 0 {
		fmt.Println("[agent-run] Repository policy decisions:")
		for _, d := range pf.Result.Decisions {
			fmt.Printf("[agent-run]   %s\n", d)
		}
	}
	for _, d := range pf.Result.Diagnostics {
		switch {
		case d.Severity == SeverityWarning:
			fmt.Printf("[agent-run] WARN: %s\n", d.Message)
		case d.Severity == SeverityInfo:
			fmt.Printf("[agent-run] %s\n", d)
		case failed:
			fmt.Printf("[agent-run] ERROR: %s\n", d.Message)
		}
	}
	if failed {
		return
	}
	if hosts := pf.grantedHosts(); len(hosts) > 0 {
		fmt.Printf("[agent-run] Repository policy: granted %d host(s): %s\n", len(hosts), strings.Join(hosts, ", "))
	} else if pf.ManifestSource != "none" && pf.ManifestSource != "ignored (--no-repo-policy)" {
		fmt.Println("[agent-run] Repository policy: granted no hosts; using the baseline policy.")
	}
}

// printExplain prints the full preflight for --explain-policy.
func (pf *policyPreflight) printExplain(w io.Writer, compileErr error) {
	_, _ = fmt.Fprintf(w, "Repository:  %s\n", pf.Repo)
	_, _ = fmt.Fprintf(w, "Commit:      %s (%s)\n", pf.SHA, pf.DefaultBranch)
	_, _ = fmt.Fprintf(w, "Harness:     %s\n", pf.Harness)
	_, _ = fmt.Fprintf(w, "Manifest:    %s\n", pf.ManifestSource)
	_, _ = fmt.Fprintln(w, "Grants:")
	if len(pf.Grants) == 0 {
		_, _ = fmt.Fprintln(w, "  (none)")
	}
	for _, g := range pf.Grants {
		_, _ = fmt.Fprintf(w, "  %s: %s %s on %s\n", g.Source, g.Host, strings.Join(g.Methods, ","), strings.Join(g.Paths, ","))
	}
	if pf.Result == nil {
		return
	}
	_, _ = fmt.Fprintln(w, "Decisions:")
	if len(pf.Result.Decisions) == 0 {
		_, _ = fmt.Fprintln(w, "  (none)")
	}
	for _, d := range pf.Result.Decisions {
		_, _ = fmt.Fprintf(w, "  %s\n", d)
	}
	_, _ = fmt.Fprintln(w, "Diagnostics:")
	if len(pf.Result.Diagnostics) == 0 {
		_, _ = fmt.Fprintln(w, "  (none)")
	}
	for _, d := range pf.Result.Diagnostics {
		_, _ = fmt.Fprintf(w, "  %s\n", d)
	}
	switch hosts := pf.grantedHosts(); {
	case compileErr != nil:
		_, _ = fmt.Fprintln(w, "Result:      rejected (a session would fail before creating a sandbox)")
	case len(hosts) == 0:
		_, _ = fmt.Fprintln(w, "Result:      ok, baseline policy (no repo rules)")
	default:
		_, _ = fmt.Fprintf(w, "Result:      ok, %d repo rule(s) for %s\n", len(hosts), strings.Join(hosts, ", "))
	}
}

// explainPolicy is --explain-policy: run the preflight and compile, print
// the result and exit. It creates no sandbox, provider or cache and clones
// nothing; an App token minted for the preflight is revoked before return.
func explainPolicy(ctx context.Context, cfg *Config, opts HarnessOptions, grants []Grant) error {
	harness, err := LookupHarness(cfg.Harness)
	if err != nil {
		return err
	}
	// Provider names only feed credential bindings; any session name works.
	session, err := newSessionName(cfg.Harness)
	if err != nil {
		return err
	}
	cred, err := resolveGitHubCredentialFn(cfg)
	if err != nil {
		return fmt.Errorf("GitHub auth failed: %w", err)
	}
	if cred.Revocable() {
		defer func() {
			fmt.Print("[agent-run] Revoking installation token ... ")
			if rerr := revokeTokenFn(cred.Token); rerr != nil {
				fmt.Printf("warning: %v\n", rerr)
			} else {
				fmt.Println("done")
			}
		}()
	}
	pf, err := runPolicyPreflight(ctx, cfg, cred.Token, sessionProviders(session, harness.Providers(opts)), grants)
	if pf != nil {
		pf.printExplain(os.Stdout, err)
	}
	return err
}

// ---- GitHub API -------------------------------------------------------------

// shaPattern is a full commit SHA (SHA-1, or SHA-256 for repositories that
// use it).
var shaPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// branchPattern is a conservative branch name: it is passed to git as an
// argument (pinClone), so it must not look like an option or a revision
// expression.
var branchPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/+-]*$`)

func validateBranchName(b string) error {
	if len(b) > 255 || !branchPattern.MatchString(b) || strings.Contains(b, "..") || strings.Contains(b, "//") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") {
		return fmt.Errorf("default branch %q is not a supported branch name", b)
	}
	return nil
}

// githubGet performs an authenticated GET of path (under githubAPIBase)
// through httpDo. The caller closes the body.
func githubGet(ctx context.Context, token, path, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBase+path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := httpDo(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return resp, nil
}

// githubGetJSON GETs path and decodes a 200 response into out.
func githubGetJSON(ctx context.Context, token, path string, out any) error {
	resp, err := githubGet(ctx, token, path, "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubJSONBytes+1))
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s failed (HTTP %d): %s", path, resp.StatusCode, redactTokenInResponse(string(body)))
	}
	if len(body) > maxGitHubJSONBytes {
		return fmt.Errorf("GET %s: response is larger than %d bytes", path, maxGitHubJSONBytes)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s: could not decode response: %w", path, err)
	}
	return nil
}

// repoPath is /repos/{owner}/{name}.
func repoPath(repo string) (string, error) {
	owner, name, err := ParseRepo(repo)
	if err != nil {
		return "", err
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// resolveDefaultBranchSHA returns repo's default branch and the commit it
// points to now. The SHA is immutable: everything after the preflight uses it,
// so a push to the branch during the run changes nothing.
func resolveDefaultBranchSHA(ctx context.Context, token, repo string) (branch, sha string, err error) {
	base, err := repoPath(repo)
	if err != nil {
		return "", "", err
	}
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := githubGetJSON(ctx, token, base, &info); err != nil {
		return "", "", fmt.Errorf("resolving the default branch of %s: %w", repo, err)
	}
	if err := validateBranchName(info.DefaultBranch); err != nil {
		return "", "", err
	}
	var segs []string
	for _, s := range strings.Split(info.DefaultBranch, "/") {
		segs = append(segs, url.PathEscape(s))
	}
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := githubGetJSON(ctx, token, base+"/git/ref/heads/"+strings.Join(segs, "/"), &ref); err != nil {
		return "", "", fmt.Errorf("resolving %s of %s to a commit: %w", info.DefaultBranch, repo, err)
	}
	if ref.Ref != "refs/heads/"+info.DefaultBranch || ref.Object.Type != "commit" || !shaPattern.MatchString(ref.Object.SHA) {
		return "", "", fmt.Errorf("resolving %s of %s: unexpected ref %q (%s %q)", info.DefaultBranch, repo, ref.Ref, ref.Object.Type, ref.Object.SHA)
	}
	return info.DefaultBranch, ref.Object.SHA, nil
}

// fetchAgentPolicy reads .agent-policy.yaml at sha. found is false on 404 (no
// manifest); any other failure is an error. A body over maxManifestBytes is
// declined without being read in full.
func fetchAgentPolicy(ctx context.Context, token, repo, sha string) (data []byte, found bool, err error) {
	if !shaPattern.MatchString(sha) {
		return nil, false, fmt.Errorf("invalid commit SHA %q", sha)
	}
	base, err := repoPath(repo)
	if err != nil {
		return nil, false, err
	}
	path := base + "/contents/" + AgentPolicyFile + "?ref=" + sha
	resp, err := githubGet(ctx, token, path, "application/vnd.github.raw+json")
	if err != nil {
		return nil, false, fmt.Errorf("fetching %s: %w", AgentPolicyFile, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, false, fmt.Errorf("fetching %s at %s failed (HTTP %d): %s", AgentPolicyFile, sha, resp.StatusCode, redactTokenInResponse(string(body)))
	}
	tooBig := fmt.Errorf("%s at %s is larger than %d bytes (the manifest limit)", AgentPolicyFile, sha, maxManifestBytes)
	if resp.ContentLength > maxManifestBytes {
		return nil, false, tooBig
	}
	// One byte past the limit tells an oversized body from one at the limit.
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("fetching %s: %w", AgentPolicyFile, err)
	}
	if len(data) > maxManifestBytes {
		return nil, false, tooBig
	}
	return data, true, nil
}

// ---- clone pinning ------------------------------------------------------------

// errHeadMismatch marks a clone whose HEAD is not the preflight commit.
var errHeadMismatch = errors.New("clone HEAD is not the preflight commit")

// pinClone puts the clone (sandboxRepoDir) on branch at exactly sha, the
// commit whose manifest was compiled, fetching sha first when the clone lacks
// it (the branch moved after the preflight), and verifies HEAD. The agent
// then works on a normal branch.
func pinClone(ctx context.Context, sandboxName, branch, sha string) error {
	if !shaPattern.MatchString(sha) {
		return fmt.Errorf("invalid commit SHA %q", sha)
	}
	if err := validateBranchName(branch); err != nil {
		return err
	}
	git := func(args ...string) []string {
		return append([]string{"sandbox", "exec", "-n", sandboxName, "--", "git", "-C", sandboxRepoDir}, args...)
	}
	fmt.Printf("[agent-run] Checking out %s at %s...\n", branch, sha)
	if execCmdSilent(ctx, "openshell", git("cat-file", "-e", sha+"^{commit}")...) != nil {
		fmt.Printf("[agent-run] %s moved after the preflight; fetching %s...\n", branch, shortSHA(sha))
		if err := execCmd(ctx, "openshell", git("fetch", "--quiet", "origin", sha)...); err != nil {
			return fmt.Errorf("fetching the preflight commit %s failed: %w", sha, err)
		}
	}
	if err := execCmd(ctx, "openshell", git("checkout", "--quiet", "-B", branch, sha)...); err != nil {
		return fmt.Errorf("checking out %s at %s failed: %w", branch, sha, err)
	}
	out, err := execCmdOutput(ctx, "openshell", git("rev-parse", "HEAD")...)
	if err != nil {
		return fmt.Errorf("git rev-parse HEAD failed: %w", err)
	}
	if head := strings.TrimSpace(out); head != sha {
		return fmt.Errorf("%w: HEAD is %q, want %s", errHeadMismatch, head, sha)
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
