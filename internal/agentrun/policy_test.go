package agentrun

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jlaska/agent-sandbox/openshell"
)

func TestParseRepo(t *testing.T) {
	tests := []struct {
		input     string
		wantOwner string
		wantName  string
		wantErr   bool
	}{
		{"jlaska/agent-sandbox", "jlaska", "agent-sandbox", false},
		{"example/other-repo", "example", "other-repo", false},
		{"org/repo-name", "org", "repo-name", false},
		{"org/repo.name", "org", "repo.name", false},
		{"org/repo_name", "org", "repo_name", false},
		{"Org-123/Repo-456", "Org-123", "Repo-456", false},
		// Invalid
		{"", "", "", true},
		{"noslash", "", "", true},
		{"/leadingslash", "", "", true},
		{"trailingslash/", "", "", true},
		{"../traversal", "", "", true},
		{"owner/../traversal", "", "", true},
		{"owner/repo/../other", "", "", true},
		{"owner/repo/extra", "", "", true},
		{"owner//repo", "", "", true},
		{"-invalid/repo", "", "", true},
		{"owner/-invalid", "", "", true},
		{"invalid-/repo", "", "", true},
		{"owner/invalid-", "", "", true},
		{"own er/repo", "", "", true},
		{"owner/re po", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			owner, name, err := ParseRepo(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseRepo(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if owner != tt.wantOwner {
					t.Errorf("owner = %q, want %q", owner, tt.wantOwner)
				}
				if name != tt.wantName {
					t.Errorf("name = %q, want %q", name, tt.wantName)
				}
			}
		})
	}
}

// testPolicyTemplate is a minimal sandbox-policy.yaml template.
const testPolicyTemplate = `network_policies:
  github_git:
    rules:
      - path: "/{{.Owner}}/{{.Repo}}.git/info/refs"
      - path: "/{{.Owner}}/{{.Repo}}.git/git-upload-pack"
      - path: "/{{.Owner}}/{{.Repo}}.git/git-receive-pack"
  github_api:
    rules:
      - path: "/repos/{{.Owner}}/{{.Repo}}"
      - path: "/repos/{{.Owner}}/{{.Repo}}/**"
      - path: "/repos/{{.Owner}}/{{.Repo}}/pulls"
  github_graphql:
    deny_rules:
      - fields: [mergePullRequest, enablePullRequestAutoMerge]
`

// generateTestPolicy renders testPolicyTemplate for repo.
func generateTestPolicy(repo string) (string, error) {
	return generatePolicyFrom(repo, []byte(testPolicyTemplate), SessionProviders{GitHubProvider: GitHubProviderType}, nil)
}

func TestGeneratePolicy_SandboxTest(t *testing.T) {

	result, err := generateTestPolicy("jlaska/agent-sandbox")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = os.Remove(result) }()

	content, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	policy := string(content)

	if !strings.Contains(policy, "jlaska/agent-sandbox") {
		t.Error("policy must contain jlaska/agent-sandbox")
	}
	if strings.Contains(policy, "example/other-repo") {
		t.Error("policy must NOT contain example/other-repo")
	}
	if strings.Contains(policy, "{{.Owner}}") || strings.Contains(policy, "{{.Repo}}") {
		t.Error("policy contains unresolved template placeholders")
	}
}

func TestGeneratePolicy_OtherRepo(t *testing.T) {

	result, err := generateTestPolicy("example/other-repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = os.Remove(result) }()

	content, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	policy := string(content)

	if !strings.Contains(policy, "example/other-repo") {
		t.Error("policy must contain example/other-repo")
	}
	if strings.Contains(policy, "agent-sandbox") {
		t.Error("policy must NOT contain agent-sandbox")
	}
}

func TestGeneratePolicy_ContainsExpectedRules(t *testing.T) {
	result, err := generateTestPolicy("jlaska/agent-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(result) }()

	content, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	policy := string(content)

	// Allowed Git operations
	for _, expected := range []string{
		"git-upload-pack",
		"git-receive-pack",
		"info/refs",
	} {
		if !strings.Contains(policy, expected) {
			t.Errorf("policy missing expected Git operation: %s", expected)
		}
	}

	// Allowed PR/comment/review operations
	for _, expected := range []string{
		"/pulls",
	} {
		if !strings.Contains(policy, expected) {
			t.Errorf("policy missing expected API path: %s", expected)
		}
	}

	// Merge operations must be denied
	if !strings.Contains(policy, "mergePullRequest") {
		t.Error("policy must contain mergePullRequest denial")
	}
	if !strings.Contains(policy, "enablePullRequestAutoMerge") {
		t.Error("policy must contain enablePullRequestAutoMerge denial")
	}
}

func TestGeneratePolicy_MalformedRepo(t *testing.T) {

	malformed := []string{
		"",
		"noslash",
		"../traversal",
		"owner/../other",
		"-bad/repo",
		"owner/-bad",
	}

	for _, repo := range malformed {
		t.Run(repo, func(t *testing.T) {
			_, err := generateTestPolicy(repo)
			if err == nil {
				t.Errorf("GeneratePolicy(%q) should fail for malformed repo", repo)
			}
		})
	}
}

func TestGeneratePolicy_NoDuplicateRepos(t *testing.T) {
	result, err := generateTestPolicy("jlaska/agent-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(result) }()

	content, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	policy := string(content)

	count := strings.Count(policy, "jlaska/agent-sandbox")
	// Count how many unique path entries reference the repo
	lines := strings.Split(policy, "\n")
	repoLines := 0
	for _, line := range lines {
		if strings.Contains(line, "jlaska/agent-sandbox") {
			repoLines++
		}
	}
	if count != repoLines {
		t.Errorf("unexpected repo reference count mismatch: total=%d lines=%d", count, repoLines)
	}
}

func TestGeneratePolicy_EmptyTemplate(t *testing.T) {
	if _, err := generatePolicyFrom("jlaska/agent-sandbox", nil, SessionProviders{GitHubProvider: GitHubProviderType}, nil); err == nil {
		t.Error("expected error for an empty policy template")
	}
}

// The embedded template is what agent-run renders, wherever it runs from.
func TestGeneratePolicy_UsesEmbeddedTemplate(t *testing.T) {
	disk, err := os.ReadFile(filepath.Join(realRepoRoot, "openshell", "sandbox-policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk, openshell.SandboxPolicyTemplate) {
		t.Fatal("embedded sandbox-policy.yaml differs from openshell/sandbox-policy.yaml")
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()
	path, err := GeneratePolicy("jlaska/agent-sandbox")
	if err != nil {
		t.Fatalf("GeneratePolicy outside a checkout: %v", err)
	}
	defer func() { _ = os.Remove(path) }()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "pypi_registry") || !strings.Contains(string(b), "/jlaska/agent-sandbox.git/") {
		t.Error("rendered policy is not the embedded template for jlaska/agent-sandbox")
	}
}

func TestGeneratePolicy_NoWildcardRepoWrite(t *testing.T) {
	result, err := generateTestPolicy("jlaska/agent-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(result) }()

	content, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	policy := string(content)

	// Ensure no wildcard write rule for /repos/**
	lines := strings.Split(policy, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == `path: "/repos/**"` {
			// Check if the preceding lines indicate a write method
			for j := i - 1; j >= 0 && j >= i-3; j-- {
				prev := strings.TrimSpace(lines[j])
				if prev == "method: POST" || prev == "method: PUT" || prev == "method: PATCH" || prev == "method: DELETE" {
					t.Errorf("found wildcard /repos/** with write method %s at line %d", prev, j+1)
				}
			}
		}
	}
}

// ---- real template / mise_bootstrap ---------------------------------------

const realRepoRoot = "../.."

type renderedPolicy struct {
	NetworkPolicies map[string]struct {
		Name      string `yaml:"name"`
		Endpoints []struct {
			Host string `yaml:"host"`
			Port int    `yaml:"port"`
		} `yaml:"endpoints"`
		Binaries []struct {
			Path string `yaml:"path"`
		} `yaml:"binaries"`
	} `yaml:"network_policies"`
}

func renderRealPolicy(t *testing.T, hosts []string) (string, renderedPolicy) {
	t.Helper()
	path, err := GeneratePolicyWithHosts("jlaska/agent-sandbox", hosts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(path) }()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p renderedPolicy
	if err := yaml.Unmarshal(b, &p); err != nil {
		t.Fatalf("rendered policy is not YAML: %v", err)
	}
	return string(b), p
}

func TestRealPolicyParsesAndMiseBootstrapBoundToMiseOnly(t *testing.T) {
	_, p := renderRealPolicy(t, nil)
	mb, ok := p.NetworkPolicies[miseBootstrapRule]
	if !ok {
		t.Fatal("mise_bootstrap rule missing")
	}
	if len(mb.Binaries) != 1 || mb.Binaries[0].Path != "/usr/local/bin/mise" {
		t.Errorf("mise_bootstrap binaries = %v, want only /usr/local/bin/mise", mb.Binaries)
	}
	hosts := map[string]bool{}
	for _, e := range mb.Endpoints {
		hosts[e.Host] = true
	}
	for _, h := range []string{"github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com",
		"api.github.com", "mise-versions.jdx.dev", "mise.jdx.dev", "pypi.org", "files.pythonhosted.org",
		"registry.npmjs.org", "dl.k8s.io", "cdn.dl.k8s.io", "get.helm.sh", "storage.googleapis.com"} {
		if !hosts[h] {
			t.Errorf("mise_bootstrap missing host %s", h)
		}
	}
	// No other rule may reference mise.
	for key, r := range p.NetworkPolicies {
		if key == miseBootstrapRule {
			continue
		}
		for _, b := range r.Binaries {
			if strings.HasSuffix(b.Path, "/mise") {
				t.Errorf("rule %s unexpectedly binds mise", key)
			}
		}
	}
	// pypi_registry gives uv/uvx (and only them) runtime PyPI access.
	pr, ok := p.NetworkPolicies["pypi_registry"]
	if !ok {
		t.Fatal("pypi_registry rule missing")
	}
	var prBins, prHosts []string
	for _, b := range pr.Binaries {
		prBins = append(prBins, b.Path)
	}
	for _, e := range pr.Endpoints {
		prHosts = append(prHosts, e.Host)
	}
	if strings.Join(prBins, ",") != "/usr/local/bin/uv,/usr/local/bin/uvx" {
		t.Errorf("pypi_registry binaries = %v, want uv and uvx", prBins)
	}
	if strings.Join(prHosts, ",") != "pypi.org,files.pythonhosted.org" {
		t.Errorf("pypi_registry hosts = %v, want pypi.org and files.pythonhosted.org", prHosts)
	}
	// opencode inference rules exist and bind only opencode.
	for _, key := range []string{"opencode_litellm", "opencode_anthropic"} {
		r, ok := p.NetworkPolicies[key]
		if !ok || len(r.Binaries) != 1 || r.Binaries[0].Path != "/usr/local/bin/opencode" {
			t.Errorf("%s: %+v", key, r)
		}
	}
}

func TestRealPolicyExtraMiseHostsRender(t *testing.T) {
	_, p := renderRealPolicy(t, []string{"nodejs.org", "example.com:8443", "NodeJS.org"})
	var got []string
	for _, e := range p.NetworkPolicies[miseBootstrapRule].Endpoints {
		got = append(got, e.Host+":"+strconv.Itoa(e.Port))
	}
	for _, want := range []string{"nodejs.org:443", "example.com:8443"} {
		if !slices.Contains(got, want) {
			t.Errorf("extra host %s not rendered; endpoints=%v", want, got)
		}
	}
	if n := strings.Count(strings.Join(got, " "), "nodejs.org"); n != 1 {
		t.Errorf("duplicate extra host not de-duplicated: %v", got)
	}
	if _, err := GeneratePolicyWithHosts("jlaska/agent-sandbox", []string{"https://evil/x"}); err == nil {
		t.Error("invalid extra host must be rejected")
	}
	// The GitHub hosts carry the credential binding; an unbound duplicate would
	// make OpenShell reject the whole policy.
	for _, h := range []string{"github.com", "API.github.com:443"} {
		if _, err := GeneratePolicyWithHosts("jlaska/agent-sandbox", []string{h}); err == nil {
			t.Errorf("--mise-allow-host %s must be rejected", h)
		}
	}
}

// boundPolicy is the subset of the rendered policy the credential-binding
// tests look at.
type boundPolicy struct {
	NetworkPolicies map[string]struct {
		Endpoints []struct {
			Host              string `yaml:"host"`
			Port              int    `yaml:"port"`
			Path              string `yaml:"path"`
			CredentialBinding *struct {
				Provider string `yaml:"provider"`
			} `yaml:"credential_binding"`
			Rules []struct {
				Allow struct {
					Method string `yaml:"method"`
					Path   string `yaml:"path"`
				} `yaml:"allow"`
			} `yaml:"rules"`
		} `yaml:"endpoints"`
		Binaries []struct {
			Path string `yaml:"path"`
		} `yaml:"binaries"`
	} `yaml:"network_policies"`
}

func renderSessionPolicy(t *testing.T, providers SessionProviders) boundPolicy {
	t.Helper()
	path, err := GenerateSessionPolicy("jlaska/agent-sandbox", providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(path) }()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p boundPolicy
	if err := yaml.Unmarshal(b, &p); err != nil {
		t.Fatalf("rendered policy is not YAML: %v", err)
	}
	return p
}

// Inference hosts and the providers serving them (SessionProviders).
const (
	anthropicHost = "api.anthropic.com"
	liteLLMHost   = "litellm.internal.keener.us"
)

// Every GitHub endpoint binds the session's GitHub provider, every inference
// endpoint binds the session's inference provider for that host if it has
// one, and nothing else binds any provider. All provider profiles are
// endpointless, so these bindings are the only place credentials resolve.
func TestSessionPolicyBindings(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	for _, tc := range []struct {
		harness string
		opts    HarnessOptions
	}{
		{HarnessClaude, HarnessOptions{}},
		{HarnessClaude, HarnessOptions{ExplicitEnv: []string{"ANTHROPIC_BASE_URL=https://" + liteLLMHost}}},
		{HarnessPi, HarnessOptions{}},
		{HarnessOpencode, HarnessOptions{Backend: inferenceLiteLLM}},
		{HarnessOpencode, HarnessOptions{Backend: inferenceAnthropic}},
	} {
		h, err := LookupHarness(tc.harness)
		if err != nil {
			t.Fatal(err)
		}
		profiles := h.Providers(tc.opts)
		sp := harnessSessionProviders("a-claude-5a717a7c", h, tc.opts)
		if sp.AnthropicProvider == "" && sp.LiteLLMProvider == "" {
			t.Errorf("%s %v: no inference provider bound", tc.harness, profiles)
		}
		bound := map[string]int{}
		for key, r := range renderSessionPolicy(t, sp).NetworkPolicies {
			for _, e := range r.Endpoints {
				got := ""
				if e.CredentialBinding != nil {
					got = e.CredentialBinding.Provider
				}
				want := ""
				switch {
				case githubCredentialHosts[e.Host]:
					want = sp.GitHubProvider
				case e.Host == anthropicHost:
					want = sp.AnthropicProvider
				case e.Host == liteLLMHost:
					want = sp.LiteLLMProvider
				}
				if got != want {
					t.Errorf("%s %v: %s endpoint %s%s: credential_binding.provider = %q, want %q", tc.harness, profiles, key, e.Host, e.Path, got, want)
				}
				if got != "" {
					bound[got]++
				}
			}
		}
		if bound[sp.GitHubProvider] < 5 { // github_git, github_api, github_graphql, mise_bootstrap x2
			t.Errorf("%s: only %d GitHub endpoints bound; want at least 5", tc.harness, bound[sp.GitHubProvider])
		}
		for _, inf := range []string{sp.AnthropicProvider, sp.LiteLLMProvider} {
			if inf != "" && bound[inf] < 2 { // the claude/node rule and the opencode rule
				t.Errorf("%s: only %d endpoints bind %s; want at least 2", tc.harness, bound[inf], inf)
			}
		}
	}
}

// Claude's credential is bound to the one inference host ANTHROPIC_BASE_URL
// sends it to: api.anthropic.com unless the base URL names another host.
func TestClaudeProviderBinding(t *testing.T) {
	h, err := LookupHarness(HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	const name = "s1-claude-code"
	for _, tc := range []struct {
		hostEnv, explicit          string
		wantAnthropic, wantLiteLLM string
	}{
		{"", "", name, ""},
		{"https://api.anthropic.com", "", name, ""},
		{"https://API.anthropic.com/", "", name, ""},
		{"https://" + liteLLMHost, "", "", name},
		{"", "ANTHROPIC_BASE_URL=https://" + liteLLMHost + "/", "", name},
		{"https://" + liteLLMHost, "ANTHROPIC_BASE_URL=https://api.anthropic.com", name, ""},
		{"not a url", "", name, ""},
	} {
		t.Setenv("ANTHROPIC_BASE_URL", tc.hostEnv)
		var opts HarnessOptions
		if tc.explicit != "" {
			opts.ExplicitEnv = []string{tc.explicit}
		}
		sp := harnessSessionProviders("s1", h, opts)
		if sp.AnthropicProvider != tc.wantAnthropic || sp.LiteLLMProvider != tc.wantLiteLLM {
			t.Errorf("env %q explicit %q: anthropic=%q litellm=%q, want %q %q", tc.hostEnv, tc.explicit,
				sp.AnthropicProvider, sp.LiteLLMProvider, tc.wantAnthropic, tc.wantLiteLLM)
		}
	}
}

// Without inference providers (GeneratePolicyWithHosts) only GitHub is bound.
func TestPolicyWithoutInferenceProviders(t *testing.T) {
	for key, r := range renderSessionPolicy(t, SessionProviders{GitHubProvider: GitHubProviderType}).NetworkPolicies {
		for _, e := range r.Endpoints {
			if e.CredentialBinding != nil && !githubCredentialHosts[e.Host] {
				t.Errorf("%s endpoint %s binds %q with no inference provider attached", key, e.Host, e.CredentialBinding.Provider)
			}
		}
	}
}

// Inference hosts are reachable only from the agent binaries, and the policy
// has no telemetry hosts: sentry.io accepts events for any project, so it is
// an upload channel, and CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC turns both
// off in Claude Code.
func TestSessionPolicyInferenceScope(t *testing.T) {
	agents := map[string]bool{"/usr/local/bin/claude": true, "/usr/local/bin/node": true, "/usr/local/bin/opencode": true}
	p := renderSessionPolicy(t, sessionProviders("s1", []string{"claude-code"}))
	for key, r := range p.NetworkPolicies {
		inference := false
		for _, e := range r.Endpoints {
			switch e.Host {
			case "sentry.io", "statsig.anthropic.com":
				t.Errorf("%s allows telemetry host %s", key, e.Host)
			case anthropicHost, liteLLMHost:
				inference = true
			}
		}
		if !inference {
			continue
		}
		for _, b := range r.Binaries {
			if !agents[b.Path] {
				t.Errorf("%s: inference rule binds %s; only agent binaries may reach inference hosts", key, b.Path)
			}
		}
	}
}

// The repo-scoped GitHub rules are the only GitHub access, so they must not
// reach past the repo's agent/** refs or into /user/**.
func TestSessionPolicyGitHubScope(t *testing.T) {
	p := renderSessionPolicy(t, SessionProviders{GitHubProvider: GitHubProviderType})
	api, ok := p.NetworkPolicies["github_api"]
	if !ok {
		t.Fatal("github_api rule missing")
	}
	deletes := 0
	for _, e := range api.Endpoints {
		for _, r := range e.Rules {
			a := r.Allow
			if a.Method == "DELETE" {
				deletes++
				if a.Path != "/repos/jlaska/agent-sandbox/git/refs/heads/agent/**" {
					t.Errorf("github_api DELETE %s: only agent/** branches may be deleted", a.Path)
				}
			}
			if strings.HasPrefix(a.Path, "/user/") {
				t.Errorf("github_api allows %s %s: /user/** enumerates repos, emails and keys", a.Method, a.Path)
			}
			if a.Method != "GET" && !strings.HasPrefix(a.Path, "/repos/jlaska/agent-sandbox/") {
				t.Errorf("github_api allows %s %s outside the target repo", a.Method, a.Path)
			}
		}
	}
	if deletes != 1 {
		t.Errorf("github_api has %d DELETE rules, want 1", deletes)
	}
}

func TestGenerateSessionPolicyRejectsBadProviderName(t *testing.T) {
	for _, name := range []string{"", "Upper", `x" y`, "a\nb", "-lead", strings.Repeat("a", 129)} {
		if _, err := GenerateSessionPolicy("jlaska/agent-sandbox", SessionProviders{GitHubProvider: name}, nil); err == nil {
			t.Errorf("GitHub provider name %q must be rejected", name)
		}
		if name == "" {
			continue // an empty inference provider means "not attached"
		}
		for _, p := range []SessionProviders{
			{GitHubProvider: GitHubProviderType, AnthropicProvider: name},
			{GitHubProvider: GitHubProviderType, LiteLLMProvider: name},
		} {
			if _, err := GenerateSessionPolicy("jlaska/agent-sandbox", p, nil); err == nil {
				t.Errorf("inference provider name %q must be rejected", name)
			}
		}
	}
}

func readBinariesTxt(t *testing.T, harness string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(realRepoRoot, "openshell", "images", harness, "binaries.txt"))
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			set[l] = true
		}
	}
	return set
}

// Every policy binary must be a real path some image provides, and (mirroring
// tests/images/smoke.sh) each harness's applicable rules must only use paths in
// that harness's binaries.txt.
func TestRealPolicyBinariesSubsetOfBinariesTxt(t *testing.T) {
	_, p := renderRealPolicy(t, nil)
	harnessNames := []string{"claude", "pi", "opencode"}
	lists := map[string]map[string]bool{}
	union := map[string]bool{}
	for _, h := range harnessNames {
		lists[h] = readBinariesTxt(t, h)
		for k := range lists[h] {
			union[k] = true
		}
	}
	for key, r := range p.NetworkPolicies {
		for _, b := range r.Binaries {
			if !union[b.Path] {
				t.Errorf("rule %s: binary %s is in no binaries.txt", key, b.Path)
			}
		}
	}
	// Every rule is checked for every harness. Only binaries *named* for a
	// different harness (claude/pi/opencode) are skipped; shared paths such as
	// the node and curl in the litellm rule (which is how pi reaches LiteLLM,
	// as a node script) must be in each harness's own binaries.txt.
	for _, h := range harnessNames {
		for key, r := range p.NetworkPolicies {
			for _, b := range r.Binaries {
				base := filepath.Base(b.Path)
				if base != h && slices.Contains(harnessNames, base) {
					continue
				}
				if !lists[h][b.Path] {
					t.Errorf("harness %s: rule %s binary %s not in %s/binaries.txt", h, key, b.Path, h)
				}
			}
		}
	}
}

// ---- narrowing --------------------------------------------------------------

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// narrowMock serves policy get fixtures; get args: <sb> --full -o json (effective)
// or <sb> --rev N -o json (revision).
func narrowMock(t *testing.T, effective, rev string, updateErr error) func(*mockCmd) {
	return func(m *mockCmd) {
		m.execOutput = func(_ string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "policy get") && strings.Contains(joined, "--full"):
				return readTestdata(t, effective), nil
			case strings.Contains(joined, "policy get") && strings.Contains(joined, "--rev 3"):
				return readTestdata(t, rev), nil
			}
			if strings.HasPrefix(joined, "policy list") {
				return "REV STATUS\n3 loaded\n", nil
			}
			t.Errorf("unexpected output call: %s", joined)
			return "", nil
		}
		execErr := func(_ context.Context, name string, args ...string) error {
			m.calls = append(m.calls, append([]string{name}, args...))
			if strings.Contains(strings.Join(args, " "), "policy update") && updateErr != nil {
				return updateErr
			}
			return nil
		}
		execCmdFn, execCmdSilentFn = execErr, execErr
	}
}

func callStrings(m *mockCmd) []string {
	var out []string
	for _, c := range m.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func TestNarrowPolicyCommandSequence(t *testing.T) {
	withMockExec(t, func(m *mockCmd) {
		narrowMock(t, "policy-get-effective-clean.json", "policy-rev-loaded.json", nil)(m)
		var err error
		captureStdout(t, func() { err = narrowPolicy(context.Background(), &Config{}, "sb1", nil) })
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"openshell policy update sb1 --remove-rule mise_bootstrap --wait",
			"openshell policy get sb1 --full -o json",
			"openshell policy get sb1 --rev 3 -o json",
		}
		got := callStrings(m)
		if !slices.Equal(got, want) {
			t.Errorf("calls:\n got %v\nwant %v", got, want)
		}
	})
}

func TestVerifyNarrowedRejections(t *testing.T) {
	tests := []struct {
		name, effective, rev, wantErr string
	}{
		{"rule still present", "policy-get-effective-with-mise.json", "policy-rev-loaded.json", "still present"},
		{"revision pending", "policy-get-effective-clean.json", "policy-rev-pending.json", `"pending", not loaded`},
		{"revision failed", "policy-get-effective-clean.json", "policy-rev-failed.json", "bad rule"},
		{"revision superseded", "policy-get-effective-clean.json", "policy-rev-superseded.json", "not loaded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMockExec(t, func(m *mockCmd) {
				narrowMock(t, tt.effective, tt.rev, nil)(m)
				_, err := verifyNarrowed(context.Background(), "sb1", nil)
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %v, want containing %q", err, tt.wantErr)
				}
			})
		})
	}
	// An empty/truncated rule set must not pass the "mise_bootstrap is gone" check vacuously.
	for _, tt := range []struct{ name, body, wantErr string }{
		{"missing network_policies", `{"version":3,"status":"effective","policy_source":"sandbox","policy":{"version":1}}`, "no network_policies"},
		{"empty network_policies", `{"version":3,"status":"effective","policy_source":"sandbox","policy":{"network_policies":{}}}`, "no network_policies"},
		{"always-present rule missing", `{"version":3,"status":"effective","policy_source":"sandbox","policy":{"network_policies":{"other":{"name":"o"}}}}`, "github_git"},
		// A gateway-wide global policy replaces the repo-scoped one.
		{"global policy source", `{"version":3,"status":"effective","policy_source":"global","policy":{"network_policies":{"github_git":{"name":"g"}}}}`, `source is "global"`},
		{"missing policy source", `{"version":3,"status":"effective","policy":{"network_policies":{"github_git":{"name":"g"}}}}`, `source is ""`},
		// A stale (pre-endpointless) profile on the gateway widens the policy.
		{"provider rule with endpoints", `{"version":3,"status":"effective","policy_source":"sandbox","policy":{"network_policies":{"github_git":{"name":"g"},"_provider_a-claude-1-claude-code":{"name":"p","endpoints":[{"host":"sentry.io","port":443}]}}}}`, "_provider_a-claude-1-claude-code adds 1 endpoint"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withMockExec(t, func(m *mockCmd) {
				m.execOutput = func(string, ...string) (string, error) { return tt.body, nil }
				if _, err := verifyNarrowed(context.Background(), "sb1", nil); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %v, want containing %q", err, tt.wantErr)
				}
			})
		})
	}
	t.Run("garbage output", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = func(string, ...string) (string, error) { return "not json", nil }
			if _, err := verifyNarrowed(context.Background(), "sb1", nil); err == nil {
				t.Error("expected parse error")
			}
		})
	})
	t.Run("tolerates stderr noise around JSON", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			narrowMock(t, "policy-get-effective-clean.json", "policy-rev-loaded.json", nil)(m)
			orig := m.execOutput
			m.execOutput = func(n string, a ...string) (string, error) {
				o, e := orig(n, a...)
				return "warning: something\n" + o + "\ntrailer\n", e
			}
			if v, err := verifyNarrowed(context.Background(), "sb1", nil); err != nil || v != 3 {
				t.Errorf("v=%d err=%v", v, err)
			}
		})
	})
}

func TestEnforceNarrowingFailsClosed(t *testing.T) {
	var revoked []string
	orig := revokeTokenFn
	revokeTokenFn = func(tok string) error { revoked = append(revoked, tok); return nil }
	defer func() { revokeTokenFn = orig }()

	app := &GitHubCredential{Mode: GitHubModeApp, Token: "ghs_tok"}
	pat := &GitHubCredential{Mode: GitHubModePAT, Token: "ghp_tok"}

	for _, tt := range []struct {
		name       string
		updateErr  error
		rev        string
		cred       *GitHubCredential
		wantRevoke int
	}{
		{"update fails", errors.New("exit 1"), "policy-rev-loaded.json", app, 1},
		{"verification fails", nil, "policy-rev-failed.json", app, 1},
		{"pat is not revoked", errors.New("exit 124"), "policy-rev-loaded.json", pat, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			revoked = nil
			withMockExec(t, func(m *mockCmd) {
				narrowMock(t, "policy-get-effective-clean.json", tt.rev, tt.updateErr)(m)
				var err error
				captureStdout(t, func() { _, err = enforceNarrowing(context.Background(), &Config{}, "sb1", tt.cred, nil) })
				if err == nil || !strings.Contains(err.Error(), "agent not launched") {
					t.Fatalf("expected fail-closed error, got %v", err)
				}
				if !slices.Contains(callStrings(m), "openshell sandbox delete sb1") {
					t.Errorf("sandbox not deleted: %v", callStrings(m))
				}
				if len(revoked) != tt.wantRevoke {
					t.Errorf("revoked = %v, want %d", revoked, tt.wantRevoke)
				}
			})
		})
	}

	t.Run("success deletes nothing", func(t *testing.T) {
		revoked = nil
		withMockExec(t, func(m *mockCmd) {
			narrowMock(t, "policy-get-effective-clean.json", "policy-rev-loaded.json", nil)(m)
			captureStdout(t, func() {
				if _, err := enforceNarrowing(context.Background(), &Config{}, "sb1", app, nil); err != nil {
					t.Fatal(err)
				}
			})
			if slices.Contains(callStrings(m), "openshell sandbox delete sb1") || len(revoked) != 0 {
				t.Error("must not clean up on success")
			}
		})
	})
}

func TestVerifyNarrowedRepoRules(t *testing.T) {
	doc := func(rules string) string {
		return `{"version":3,"status":"effective","policy_source":"sandbox","policy":{"network_policies":{"github_git":{"name":"g"}` + rules + `}}}`
	}
	const docsRule = `,"repo_docs_example_org":{"name":"repo_docs_example_org","endpoints":[{"host":"docs.example.org","port":443}]}`
	want := map[string]string{"repo_docs_example_org": "docs.example.org"}
	rev := readTestdata(t, "policy-rev-loaded.json")
	for _, tt := range []struct {
		name, rules string
		want        map[string]string
		wantErr     string
	}{
		{"exact", docsRule, want, ""},
		{"none expected, none present", "", nil, ""},
		{"missing", "", want, "is missing"},
		{"extra", docsRule, nil, "did not grant"},
		{"extra by name", `,"x":{"name":"repo_x","endpoints":[{"host":"x.example.org"}]}`, nil, "did not grant"},
		{"wrong host", `,"repo_docs_example_org":{"name":"repo_docs_example_org","endpoints":[{"host":"evil.example.org","port":443}]}`, want, `endpoint for "evil.example.org"`},
		{"second host", `,"repo_docs_example_org":{"name":"repo_docs_example_org","endpoints":[{"host":"docs.example.org"},{"host":"evil.example.org"}]}`, want, `endpoint for "evil.example.org"`},
		{"no endpoints", `,"repo_docs_example_org":{"name":"repo_docs_example_org"}`, want, "no endpoints"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withMockExec(t, func(m *mockCmd) {
				m.execOutput = func(_ string, args ...string) (string, error) {
					if slices.Contains(args, "--rev") {
						return rev, nil
					}
					return doc(tt.rules), nil
				}
				_, err := verifyNarrowed(context.Background(), "sb1", tt.want)
				if tt.wantErr == "" && err != nil {
					t.Errorf("err = %v", err)
				}
				if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
					t.Errorf("err = %v, want containing %q", err, tt.wantErr)
				}
			})
		})
	}
}
