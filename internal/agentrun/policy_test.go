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
		{"jlaska/homelab", "jlaska", "homelab", false},
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
	return generatePolicyFrom(repo, []byte(testPolicyTemplate), nil)
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
	if strings.Contains(policy, "jlaska/homelab") {
		t.Error("policy must NOT contain jlaska/homelab")
	}
	if strings.Contains(policy, "{{.Owner}}") || strings.Contains(policy, "{{.Repo}}") {
		t.Error("policy contains unresolved template placeholders")
	}
}

func TestGeneratePolicy_Homelab(t *testing.T) {

	result, err := generateTestPolicy("jlaska/homelab")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = os.Remove(result) }()

	content, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	policy := string(content)

	if !strings.Contains(policy, "jlaska/homelab") {
		t.Error("policy must contain jlaska/homelab")
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
	if _, err := generatePolicyFrom("jlaska/agent-sandbox", nil, nil); err == nil {
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
		captureStdout(t, func() { err = narrowPolicy(context.Background(), &Config{}, "sb1") })
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
				_, err := verifyNarrowed(context.Background(), "sb1")
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %v, want containing %q", err, tt.wantErr)
				}
			})
		})
	}
	// An empty/truncated rule set must not pass the "mise_bootstrap is gone" check vacuously.
	for _, tt := range []struct{ name, body, wantErr string }{
		{"missing network_policies", `{"version":3,"status":"effective","policy":{"version":1}}`, "no network_policies"},
		{"empty network_policies", `{"version":3,"status":"effective","policy":{"network_policies":{}}}`, "no network_policies"},
		{"always-present rule missing", `{"version":3,"status":"effective","policy":{"network_policies":{"other":{"name":"o"}}}}`, "github_git"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withMockExec(t, func(m *mockCmd) {
				m.execOutput = func(string, ...string) (string, error) { return tt.body, nil }
				if _, err := verifyNarrowed(context.Background(), "sb1"); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %v, want containing %q", err, tt.wantErr)
				}
			})
		})
	}
	t.Run("garbage output", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = func(string, ...string) (string, error) { return "not json", nil }
			if _, err := verifyNarrowed(context.Background(), "sb1"); err == nil {
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
			if v, err := verifyNarrowed(context.Background(), "sb1"); err != nil || v != 3 {
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
				captureStdout(t, func() { _, err = enforceNarrowing(context.Background(), &Config{}, "sb1", tt.cred) })
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
				if _, err := enforceNarrowing(context.Background(), &Config{}, "sb1", app); err != nil {
					t.Fatal(err)
				}
			})
			if slices.Contains(callStrings(m), "openshell sandbox delete sb1") || len(revoked) != 0 {
				t.Error("must not clean up on success")
			}
		})
	})
}
