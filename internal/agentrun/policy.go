package agentrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/jlaska/agent-sandbox/openshell"
)

// repoPattern validates owner/repo format: alphanumeric, hyphens, underscores, dots.
var repoPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?/[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`)

// ParseRepo splits "owner/repo" into its components with validation.
func ParseRepo(repo string) (owner, name string, err error) {
	if repo == "" {
		return "", "", fmt.Errorf("repository cannot be empty")
	}

	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repository must be in owner/repo format: %q", repo)
	}

	owner, name = parts[0], parts[1]

	if !repoPattern.MatchString(repo) {
		return "", "", fmt.Errorf("invalid repository name %q: must contain only alphanumeric, hyphens, underscores, dots", repo)
	}

	if strings.Contains(owner, "..") || strings.Contains(name, "..") {
		return "", "", fmt.Errorf("repository name contains path traversal: %q", repo)
	}

	if strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repository name contains extra path separator: %q", repo)
	}

	return owner, name, nil
}

// miseHost is one extra host the mise_bootstrap rule allows (--mise-allow-host).
type miseHost struct {
	Host string
	Port int
}

// policyData is passed to the sandbox policy template.
type policyData struct {
	Owner          string
	Repo           string
	GitHubProvider string // provider instance bound to the GitHub endpoints
	MiseExtraHosts []miseHost
}

// providerNameRE matches OpenShell provider instance names as agent-run
// creates them (sessionProviderName), so a name can never break out of the
// quoted YAML scalar it is rendered into.
var providerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

// githubCredentialHosts carry the GitHub credential_binding in the template.
// --mise-allow-host may not add them: OpenShell requires every endpoint on a
// host to agree on the binding, and the template already covers them.
var githubCredentialHosts = map[string]bool{"github.com": true, "api.github.com": true}

// parseMiseHosts converts validated host[:port] values (default port 443)
// into template data, dropping duplicates while keeping order.
func parseMiseHosts(hosts []string) ([]miseHost, error) {
	var out []miseHost
	seen := map[miseHost]bool{}
	for _, h := range hosts {
		if err := validateHostPort(h); err != nil {
			return nil, err
		}
		host, port, hasPort := strings.Cut(h, ":")
		if githubCredentialHosts[strings.ToLower(host)] {
			return nil, fmt.Errorf("--mise-allow-host %s: already allowed during bootstrap (GitHub endpoints are fixed by the policy)", h)
		}
		mh := miseHost{Host: strings.ToLower(host), Port: 443}
		if hasPort {
			mh.Port, _ = strconv.Atoi(port)
		}
		if !seen[mh] {
			seen[mh] = true
			out = append(out, mh)
		}
	}
	return out, nil
}

// GeneratePolicy generates a repository-scoped sandbox policy from the
// embedded template (openshell/sandbox-policy.yaml, built into the binary so a
// policy file in the working directory can never replace it). Returns the path
// to a temporary file containing the generated policy.
func GeneratePolicy(repo string) (string, error) {
	return GeneratePolicyWithHosts(repo, nil)
}

// GeneratePolicyWithHosts is GeneratePolicy plus extra hosts (host[:port]) that
// the temporary mise_bootstrap rule may reach. The GitHub endpoints bind a
// provider named GitHubProviderType ("github-agent"), the name a manually
// created provider gets (tests/phase4-exit-gate.sh); sessions use
// GenerateSessionPolicy.
func GeneratePolicyWithHosts(repo string, miseHosts []string) (string, error) {
	return GenerateSessionPolicy(repo, GitHubProviderType, miseHosts)
}

// GenerateSessionPolicy renders the policy for a session whose GitHub provider
// instance is githubProvider. OpenShell rejects the policy unless that provider
// is attached to the sandbox and uses an endpointless profile.
func GenerateSessionPolicy(repo, githubProvider string, miseHosts []string) (string, error) {
	return generatePolicyFrom(repo, openshell.SandboxPolicyTemplate, githubProvider, miseHosts)
}

// generatePolicyFrom renders tmpl for repo and writes it to a temp file.
func generatePolicyFrom(repo string, tmpl []byte, githubProvider string, miseHosts []string) (string, error) {
	owner, name, err := ParseRepo(repo)
	if err != nil {
		return "", fmt.Errorf("invalid repository: %w", err)
	}
	if !providerNameRE.MatchString(githubProvider) {
		return "", fmt.Errorf("invalid GitHub provider name %q", githubProvider)
	}
	content := tmpl

	extra, err := parseMiseHosts(miseHosts)
	if err != nil {
		return "", err
	}

	result, err := renderPolicy(string(content), policyData{Owner: owner, Repo: name, GitHubProvider: githubProvider, MiseExtraHosts: extra})
	if err != nil {
		return "", err
	}

	// Validate: the generated policy must contain the requested repo
	// and no unresolved template placeholders.
	fullRepo := owner + "/" + name
	if !strings.Contains(result, fullRepo) {
		return "", fmt.Errorf("generated policy does not contain target repository %q", fullRepo)
	}
	if strings.Contains(result, "{{.") {
		return "", fmt.Errorf("generated policy contains unresolved template placeholders")
	}

	// OpenShell 0.1.4 rejects unknown policy fields and fails closed on a bad
	// revision, so make sure at least the YAML itself is well-formed here.
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(result), &doc); err != nil {
		return "", fmt.Errorf("generated policy is not valid YAML: %w", err)
	}

	// Write to temp file
	tmpfile, err := os.CreateTemp("", "agent-run-policy.*.yaml")
	if err != nil {
		return "", fmt.Errorf("failed to create temp policy file: %w", err)
	}

	if _, err := tmpfile.WriteString(result); err != nil {
		_ = tmpfile.Close()
		_ = os.Remove(tmpfile.Name())
		return "", fmt.Errorf("failed to write policy file: %w", err)
	}

	if err := tmpfile.Close(); err != nil {
		_ = os.Remove(tmpfile.Name())
		return "", err
	}

	return tmpfile.Name(), nil
}

// renderPolicy executes the policy template.
func renderPolicy(content string, data policyData) (string, error) {
	tmpl, err := template.New("policy").Parse(content)
	if err != nil {
		return "", fmt.Errorf("failed to parse policy template: %w", err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute policy template: %w", err)
	}
	return buf.String(), nil
}

// miseBootstrapRule is the network_policies key of the temporary egress rule
// that lets `mise install` reach tool registries. It is removed before the
// agent starts (two-phase narrowing).
const miseBootstrapRule = "mise_bootstrap"

// alwaysPresentRule is a network_policies key every generated policy contains
// (openshell/sandbox-policy.yaml); verifyNarrowed uses it as a sanity anchor.
const alwaysPresentRule = "github_git"

// revokeTokenFn is RevokeToken, replaceable in tests.
var revokeTokenFn = RevokeToken

// narrowPolicy is step 11: remove the mise_bootstrap rule and verify it is
// gone and the resulting revision is Loaded. It runs even when mise was skipped
// (the rule is always present at create time) and returns an error on any
// failure; callers must treat that as fatal (see enforceNarrowing).
func narrowPolicy(ctx context.Context, _ *Config, sandboxName string) error {
	fmt.Printf("[agent-run] Narrowing egress: removing %s rule...\n", miseBootstrapRule)
	if err := execCmd(ctx, "openshell", "policy", "update", sandboxName,
		"--remove-rule", miseBootstrapRule, "--wait"); err != nil {
		return fmt.Errorf("failed to remove %s rule: %w", miseBootstrapRule, err)
	}
	version, err := verifyNarrowed(ctx, sandboxName)
	if err != nil {
		return err
	}
	fmt.Printf("[agent-run] Egress narrowed: %s removed (policy revision %d loaded).\n", miseBootstrapRule, version)
	return nil
}

// narrowOutcome reports which fail-closed cleanups enforceNarrowing completed,
// so the caller's deferred cleanup only skips what is really done and retries
// the rest.
type narrowOutcome struct {
	SandboxDeleted bool
	TokenRevoked   bool
}

// enforceNarrowing runs narrowPolicy and fails closed: if narrowing or its
// verification fails, the sandbox is deleted and the GitHub token revoked
// before the error is returned, so the agent can never run with bootstrap
// egress. The sandbox's policy revisions are printed first (they die with the
// sandbox). The error states truthfully whether the delete succeeded; when it
// did not, the caller's deferred cleanup retries it.
func enforceNarrowing(ctx context.Context, cfg *Config, sandboxName string, cred *GitHubCredential) (narrowOutcome, error) {
	var out narrowOutcome
	err := narrowPolicy(ctx, cfg, sandboxName)
	if err == nil {
		return out, nil
	}
	fmt.Printf("[agent-run] ERROR: egress narrowing failed: %v\n", err)
	// A signal may be what failed narrowing: the fail-closed steps must still
	// run, so they use a context that is not cancelled with ctx.
	ctx = detached(ctx)
	printPolicyHistory(ctx, sandboxName)
	fmt.Printf("[agent-run] Failing closed: deleting sandbox %s and revoking credentials.\n", sandboxName)
	status := "sandbox deleted"
	if derr := execCmdSilent(ctx, "openshell", "sandbox", "delete", sandboxName); derr != nil {
		fmt.Printf("[agent-run] warning: sandbox delete failed: %v (will retry during cleanup)\n", derr)
		status = fmt.Sprintf("SANDBOX DELETE FAILED (%v; cleanup will retry, otherwise run: openshell sandbox delete %s)", derr, sandboxName)
	} else {
		fmt.Printf("[agent-run] Sandbox %s deletion accepted.\n", sandboxName)
		out.SandboxDeleted = true
	}
	if cred != nil && cred.Revocable() {
		if rerr := revokeTokenFn(cred.Token); rerr != nil {
			fmt.Printf("[agent-run] warning: token revoke failed: %v (will retry during cleanup)\n", rerr)
		} else {
			fmt.Println("[agent-run] Installation token revoked.")
			out.TokenRevoked = true
		}
	}
	return out, fmt.Errorf("egress narrowing failed (%s, agent not launched): %w", status, err)
}

// printPolicyHistory prints `openshell policy list <sandbox>` (revision
// statuses and load errors) so the evidence survives the sandbox delete.
func printPolicyHistory(ctx context.Context, sandboxName string) {
	out, err := execCmdOutput(ctx, "openshell", "policy", "list", sandboxName)
	out = strings.TrimSpace(out)
	if err != nil || out == "" {
		return
	}
	fmt.Printf("[agent-run] Policy revisions of %s before deletion:\n", sandboxName)
	for _, line := range strings.Split(out, "\n") {
		fmt.Printf("[agent-run]   %s\n", line)
	}
}

// policyGetDoc is the subset of `openshell policy get -o json` we read.
// Effective view: {"scope","sandbox","version","active_version","hash",
// "status":"effective","policy":{"network_policies":{<key>:{"name":...}}}}.
// Revision view (--rev N): same keys, but "status" is the revision state
// (pending|loaded|failed|superseded) and "load_error" may be set.
type policyGetDoc struct {
	Version   uint32 `json:"version"`
	Status    string `json:"status"`
	LoadError string `json:"load_error"`
	Policy    *struct {
		NetworkPolicies map[string]struct {
			Name string `json:"name"`
		} `json:"network_policies"`
	} `json:"policy"`
}

// extractJSONObject returns the outermost {...} in out, tolerating stderr
// noise captured alongside the JSON.
func extractJSONObject(out string) string {
	start, end := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if start < 0 || end < start {
		return ""
	}
	return out[start : end+1]
}

func getPolicyDoc(ctx context.Context, args ...string) (*policyGetDoc, error) {
	out, err := execCmdOutput(ctx, "openshell", append([]string{"policy", "get"}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("openshell policy get %s failed: %w", strings.Join(args, " "), err)
	}
	var doc policyGetDoc
	if err := json.Unmarshal([]byte(extractJSONObject(out)), &doc); err != nil {
		return nil, fmt.Errorf("could not parse 'openshell policy get %s' output: %w", strings.Join(args, " "), err)
	}
	return &doc, nil
}

// verifyNarrowed checks, via two `policy get` calls, that (1) the effective
// policy no longer contains mise_bootstrap and (2) the latest revision (the
// effective version) has status loaded. `--wait` alone is not enough: it also
// exits 0 for "unchanged" and "superseded". Returns the verified revision.
func verifyNarrowed(ctx context.Context, sandboxName string) (uint32, error) {
	eff, err := getPolicyDoc(ctx, sandboxName, "--full", "-o", "json")
	if err != nil {
		return 0, err
	}
	if eff.Policy == nil {
		return 0, fmt.Errorf("verification failed: policy get returned no policy payload")
	}
	// An empty or truncated rule set would pass the absence check below
	// vacuously; github_git is in every generated policy, so require it.
	if len(eff.Policy.NetworkPolicies) == 0 {
		return 0, fmt.Errorf("verification failed: effective policy has no network_policies")
	}
	if _, ok := eff.Policy.NetworkPolicies[alwaysPresentRule]; !ok {
		return 0, fmt.Errorf("verification failed: effective policy lacks the always-present %s rule (unexpected policy payload)", alwaysPresentRule)
	}
	for key, rule := range eff.Policy.NetworkPolicies {
		if key == miseBootstrapRule || rule.Name == miseBootstrapRule || rule.Name == "mise-bootstrap" {
			return 0, fmt.Errorf("verification failed: %s rule is still present in the effective policy", miseBootstrapRule)
		}
	}
	if eff.Version == 0 {
		return 0, fmt.Errorf("verification failed: policy get returned no revision number")
	}
	rev, err := getPolicyDoc(ctx, sandboxName, "--rev", strconv.FormatUint(uint64(eff.Version), 10), "-o", "json")
	if err != nil {
		return 0, err
	}
	if !strings.EqualFold(rev.Status, "loaded") {
		msg := fmt.Sprintf("verification failed: policy revision %d is %q, not loaded", eff.Version, rev.Status)
		if rev.LoadError != "" {
			msg += ": " + rev.LoadError
		}
		return 0, fmt.Errorf("%s", msg)
	}
	return eff.Version, nil
}
