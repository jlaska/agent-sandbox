package agentrun

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jlaska/agent-sandbox/openshell"
)

const compileTestRepo = "example/repo"

var compileTestProviders = SessionProviders{GitHubProvider: "s1-github-agent", AnthropicProvider: "s1-claude-code"}

func mustManifest(t *testing.T, src string) *AgentPolicy {
	t.Helper()
	p, err := ParseAgentPolicy([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// manifestFor builds a manifest requesting hosts (all required).
func manifestFor(hosts ...string) *AgentPolicy {
	p := &AgentPolicy{Version: 1}
	for _, h := range hosts {
		p.Network = append(p.Network, NetworkRequest{Host: h, Reason: "test"})
	}
	return p
}

func allowHosts(t *testing.T, hosts ...string) []Grant {
	t.Helper()
	gs, err := AllowHostGrants(hosts)
	if err != nil {
		t.Fatal(err)
	}
	return gs
}

func compileInput(m *AgentPolicy, grants []Grant) CompileInput {
	return CompileInput{Repo: compileTestRepo, Harness: HarnessClaude, Providers: compileTestProviders, Manifest: m, Grants: grants}
}

// sessionBaseline is what GenerateSessionPolicy writes for in.
func sessionBaseline(t *testing.T, in CompileInput) string {
	t.Helper()
	path, err := GenerateSessionPolicy(in.Repo, in.Providers, in.MiseHosts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(path) }()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustCompile(t *testing.T, in CompileInput) *CompileResult {
	t.Helper()
	res, err := CompilePolicy(in)
	if err != nil {
		t.Fatalf("CompilePolicy: %v", err)
	}
	return res
}

func diagnostics(res *CompileResult, sev Severity) []string {
	var out []string
	for _, d := range res.Diagnostics {
		if d.Severity == sev {
			out = append(out, d.Message)
		}
	}
	return out
}

type compiledPolicy struct {
	NetworkPolicies map[string]map[string]any `yaml:"network_policies"`
}

func repoRules(t *testing.T, policy string) map[string]repoRule {
	t.Helper()
	var p compiledPolicy
	if err := yaml.Unmarshal([]byte(policy), &p); err != nil {
		t.Fatal(err)
	}
	out := map[string]repoRule{}
	for k, v := range p.NetworkPolicies {
		if !strings.HasPrefix(k, repoRulePrefix) {
			continue
		}
		b, _ := yaml.Marshal(v)
		var r repoRule
		if err := yaml.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		out[k] = r
	}
	return out
}

func TestCompileNoRequestsIsBaseline(t *testing.T) {
	cases := map[string]CompileInput{
		"no manifest":          compileInput(nil, nil),
		"empty manifest":       compileInput(manifestFor(), nil),
		"no inference binding": {Repo: compileTestRepo, Harness: HarnessPi, Providers: SessionProviders{GitHubProvider: "s1-github-agent"}},
		"mise hosts":           {Repo: compileTestRepo, Harness: HarnessOpencode, Providers: compileTestProviders, MiseHosts: []string{"nodejs.org", "dl.example.org:8443"}},
		"unused grant":         compileInput(nil, allowHosts(t, "docs.example.org")),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			res := mustCompile(t, in)
			if want := sessionBaseline(t, in); res.PolicyYAML != want {
				t.Errorf("compiled policy differs from GenerateSessionPolicy output")
			}
		})
	}
}

func TestCompileGrantedRule(t *testing.T) {
	in := compileInput(mustManifest(t, "version: 1\nnetwork:\n  - host: docs.example.org\n    reason: \"fetch API reference docs\"\n"), allowHosts(t, "docs.example.org"))
	res := mustCompile(t, in)

	want := repoRule{
		Name: "repo_docs_example_org",
		Endpoints: []repoEndpoint{{
			Host: "docs.example.org", Port: 443, Protocol: "rest", Enforcement: "enforce",
			Rules: []repoAllow{{Allow: repoMatch{"GET", "/**"}}, {Allow: repoMatch{"HEAD", "/**"}}},
		}},
		Binaries: []repoBinary{{"/usr/local/bin/claude"}, {"/usr/local/bin/node"}},
	}
	got := repoRules(t, res.PolicyYAML)
	if !reflect.DeepEqual(got, map[string]repoRule{"repo_docs_example_org": want}) {
		t.Errorf("repo rules = %+v\nwant %+v", got, want)
	}
	if strings.Contains(strings.SplitN(res.PolicyYAML, "repo_docs_example_org:", 2)[1][:400], "credential_binding") {
		t.Error("repo rule carries a credential_binding")
	}
	d := res.Decisions
	if len(d) != 1 || d[0].Outcome != OutcomeGranted || d[0].Rule != "repo_docs_example_org" ||
		!reflect.DeepEqual(d[0].Sources, []string{"--allow-host docs.example.org"}) {
		t.Errorf("decisions = %+v", d)
	}
	if s := d[0].String(); !strings.Contains(s, "granted docs.example.org") || !strings.Contains(s, "GET,HEAD on /**") {
		t.Errorf("Decision.String() = %q", s)
	}
}

// Removing the generated block must give back the baseline byte for byte.
func TestCompileKeepsBaselineBytes(t *testing.T) {
	in := compileInput(manifestFor("docs.example.org", "api.example.org"), allowHosts(t, "docs.example.org", "api.example.org"))
	res := mustCompile(t, in)
	base := sessionBaseline(t, in)
	block, err := renderRepoRules(map[string]repoRule{
		"repo_api_example_org":  newRepoRule(res.Decisions[0], harnessRepoBinaries[HarnessClaude]),
		"repo_docs_example_org": newRepoRule(res.Decisions[1], harnessRepoBinaries[HarnessClaude]),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.PolicyYAML, block) {
		t.Fatalf("generated block not found in policy:\n%s", block)
	}
	if strings.Replace(res.PolicyYAML, "\n"+block, "", 1) != base {
		t.Error("baseline bytes changed by composition")
	}
}

func TestCompileHarnessBinaries(t *testing.T) {
	for _, h := range []string{HarnessClaude, HarnessPi, HarnessOpencode} {
		t.Run(h, func(t *testing.T) {
			in := compileInput(manifestFor("docs.example.org"), allowHosts(t, "docs.example.org"))
			in.Harness = h
			res := mustCompile(t, in)
			r := repoRules(t, res.PolicyYAML)["repo_docs_example_org"]
			if len(r.Binaries) == 0 {
				t.Fatal("no binaries")
			}
			list := readBinariesTxt(t, h)
			for _, b := range r.Binaries {
				if !list[b.Path] {
					t.Errorf("binary %s not in %s/binaries.txt", b.Path, h)
				}
			}
		})
	}
	in := compileInput(nil, nil)
	in.Harness = "bogus"
	if _, err := CompilePolicy(in); err == nil {
		t.Error("unknown harness accepted")
	}
}

func TestCompileMethodsAndPaths(t *testing.T) {
	m := manifestFor("docs.example.org")
	tests := []struct {
		name          string
		grants        []Grant
		methods, path []string
	}{
		{"allow-host is read-only", allowHosts(t, "docs.example.org"), []string{"GET", "HEAD"}, []string{"/**"}},
		{"file defaults are read-only", parseTestGrants(t, "version: 1\ngrants:\n  - host: docs.example.org\n"), []string{"GET", "HEAD"}, []string{"/**"}},
		{"explicit methods widen", parseTestGrants(t, "version: 1\ngrants:\n  - host: docs.example.org\n    methods: [POST, GET]\n    paths: [\"/v1/**\"]\n"), []string{"GET", "POST"}, []string{"/v1/**"}},
		{"grants union", append(allowHosts(t, "docs.example.org"), parseTestGrants(t, "version: 1\ngrants:\n  - host: docs.example.org\n    repos: [example/repo]\n    methods: [DELETE]\n    paths: [\"/tmp/*\"]\n")...),
			[]string{"GET", "HEAD", "DELETE"}, []string{"/**", "/tmp/*"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := mustCompile(t, compileInput(m, tt.grants))
			d := res.Decisions[0]
			if !reflect.DeepEqual(d.Methods, tt.methods) || !reflect.DeepEqual(d.Paths, tt.path) {
				t.Errorf("methods %v paths %v, want %v %v", d.Methods, d.Paths, tt.methods, tt.path)
			}
			r := repoRules(t, res.PolicyYAML)["repo_docs_example_org"]
			if n := len(r.Endpoints[0].Rules); n != len(tt.methods)*len(tt.path) {
				t.Errorf("%d allow rules, want %d", n, len(tt.methods)*len(tt.path))
			}
		})
	}
}

func parseTestGrants(t *testing.T, src string) []Grant {
	t.Helper()
	gs, err := ParseGrants([]byte(src), "grants.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return gs
}

func TestCompileRequiredUngrantedFails(t *testing.T) {
	res, err := CompilePolicy(compileInput(manifestFor("docs.example.org"), nil))
	if err == nil {
		t.Fatal("required ungranted request compiled")
	}
	for _, want := range []string{"docs.example.org", "--allow-host docs.example.org", "grants file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if res == nil || res.PolicyYAML != "" || len(res.Decisions) != 1 || res.Decisions[0].Outcome != OutcomeDenied {
		t.Errorf("result = %+v", res)
	}
}

func TestCompileOptionalUngrantedOmitted(t *testing.T) {
	m := mustManifest(t, "version: 1\nnetwork:\n  - host: docs.example.org\n    reason: r\n    optional: true\n  - host: api.example.org\n    reason: r\n")
	in := compileInput(m, allowHosts(t, "api.example.org"))
	res := mustCompile(t, in)
	if w := diagnostics(res, SeverityWarning); len(w) != 1 || !strings.Contains(w[0], "docs.example.org") || !strings.Contains(w[0], "--allow-host docs.example.org") {
		t.Errorf("warnings = %v", w)
	}
	rules := repoRules(t, res.PolicyYAML)
	if _, ok := rules["repo_docs_example_org"]; ok {
		t.Error("omitted host has a rule")
	}
	if _, ok := rules["repo_api_example_org"]; !ok {
		t.Error("granted host has no rule")
	}
	if res.Decisions[1].Host != "docs.example.org" || res.Decisions[1].Outcome != OutcomeOmitted {
		t.Errorf("decisions = %+v", res.Decisions)
	}
}

func TestCompileGrantForOtherRepo(t *testing.T) {
	grants := parseTestGrants(t, "version: 1\ngrants:\n  - host: docs.example.org\n    repos: [example/other-repo]\n")
	_, err := CompilePolicy(compileInput(manifestFor("docs.example.org"), grants))
	if err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Errorf("grant for another repo applied: %v", err)
	}
	res := mustCompile(t, compileInput(nil, grants))
	if info := diagnostics(res, SeverityInfo); len(info) != 1 || !strings.Contains(info[0], "limited to example/other-repo") {
		t.Errorf("info = %v", info)
	}
}

func TestCompileUnusedGrantIsInfo(t *testing.T) {
	res := mustCompile(t, compileInput(manifestFor(), allowHosts(t, "docs.example.org")))
	if info := diagnostics(res, SeverityInfo); len(info) != 1 || !strings.Contains(info[0], "unused") {
		t.Errorf("info = %v", info)
	}
	if len(repoRules(t, res.PolicyYAML)) != 0 {
		t.Error("an unused grant added a rule")
	}
}

// Every class of universal restriction denies the host both as a repo request
// (even when a grant is built to cover it) and as a grant alone.
func TestCompileHardDenies(t *testing.T) {
	hosts := map[string]string{
		"localhost":                     "local",
		"dev.localhost":                 "*.localhost",
		"printer.local":                 "*.local",
		"api.default.svc.cluster.local": "*.local",
		"db.internal":                   "*.internal",
		"api.default.svc":               "*.svc",
		"nas.home.arpa":                 "*.home.arpa",
		"router.lan":                    "*.lan",
		"intranet":                      "single-label",
		"metadata.google.internal":      "metadata",
		"metadata.goog":                 "metadata",
		"127.0.0.1":                     "IP address",
		"127.1":                         "IP address",
		"2130706433":                    "IP address",
		"0x7f.0.0.1":                    "IP address",
		"10.0.0.1.nip.io":               "embed an IPv4",
		"github.com":                    "baseline policy (github_git, mise_bootstrap)",
		"api.github.com":                "baseline policy",
		"api.anthropic.com":             "baseline policy (anthropic_api, opencode_anthropic)",
		"registry.npmjs.org":            "baseline policy",
		"pypi.org":                      "baseline policy",
		"objects.githubusercontent.com": "baseline policy",
		"uploads.github.com":            "GitHub hosts",
		"raw.githubusercontent.com":     "baseline policy (mise_bootstrap)",
		"gist.githubusercontent.com":    "GitHub hosts",
		"githubusercontent.com":         "GitHub hosts",
		"example.github.io":             "GitHub hosts",
		"ghcr.io":                       "GitHub hosts",
		"pkg.ghcr.io":                   "GitHub hosts",
	}
	for host, want := range hosts {
		grant := Grant{Host: host, Methods: []string{"GET"}, Paths: []string{"/**"}, Source: "test grant"}
		t.Run("request "+host, func(t *testing.T) {
			m := &AgentPolicy{Version: 1, Network: []NetworkRequest{{Host: host, Reason: "r", Optional: true}}}
			res, err := CompilePolicy(compileInput(m, []Grant{grant}))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error %v does not mention %q", err, want)
			}
			// A host that fails manifest validation (an IP literal) makes the
			// manifest malformed, so it gets no decision at all.
			if res.PolicyYAML != "" || (len(res.Decisions) > 0 && res.Decisions[0].Outcome != OutcomeDenied) {
				t.Errorf("result = %+v", res)
			}
		})
		t.Run("grant "+host, func(t *testing.T) {
			_, err := CompilePolicy(compileInput(nil, []Grant{grant}))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("grant for %s accepted: %v", host, err)
			}
		})
	}
	// --mise-allow-host hosts are baseline hosts too.
	in := compileInput(manifestFor("nodejs.org"), allowHosts(t, "nodejs.org"))
	in.MiseHosts = []string{"nodejs.org"}
	if _, err := CompilePolicy(in); err == nil || !strings.Contains(err.Error(), "mise_bootstrap") {
		t.Errorf("mise host request: %v", err)
	}
	// An IP-literal grant built in code is caught by re-validation.
	if _, err := CompilePolicy(compileInput(nil, []Grant{{Host: "::1", Methods: []string{"GET"}, Paths: []string{"/**"}}})); err == nil {
		t.Error("IPv6 grant accepted")
	}
}

// Every baseline host is protected, derived from the rendered policy rather
// than a list in this file.
func TestCompileDeniesEveryBaselineHost(t *testing.T) {
	base, err := renderBaselinePolicy(compileTestRepo, openshell.SandboxPolicyTemplate, compileTestProviders, nil)
	if err != nil {
		t.Fatal(err)
	}
	hosts, _, err := baselineInventory(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) < 10 {
		t.Fatalf("only %d baseline hosts found", len(hosts))
	}
	for h := range hosts {
		if why := hostRestriction(h, hosts); why == "" {
			t.Errorf("baseline host %s is not restricted", h)
		}
	}
}

func TestCompileAggregatesErrors(t *testing.T) {
	m := manifestFor("docs.example.org", "localhost", "api.github.com")
	_, err := CompilePolicy(compileInput(m, []Grant{{Host: "db.internal", Methods: []string{"GET"}, Paths: []string{"/**"}, Source: "g"}}))
	if err == nil {
		t.Fatal("compiled")
	}
	for _, want := range []string{"docs.example.org", "localhost", "api.github.com", "db.internal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error lacks %s: %v", want, err)
		}
	}
	// An invalid manifest built in code fails too.
	if _, err := CompilePolicy(compileInput(&AgentPolicy{Version: 2}, nil)); err == nil {
		t.Error("invalid manifest compiled")
	}
}

func TestCompileDeterministic(t *testing.T) {
	hosts := []string{"docs.example.org", "api.example.org", "cdn.example.net", "my-docs.example.org"}
	fileGrants := parseTestGrants(t, "version: 1\ngrants:\n  - host: cdn.example.net\n    methods: [GET]\n    paths: [\"/b/**\", \"/a/**\"]\n  - host: cdn.example.net\n    methods: [HEAD]\n")
	first := mustCompile(t, compileInput(manifestFor(hosts...), append(allowHosts(t, hosts[0], hosts[1], hosts[3]), fileGrants...)))
	again := mustCompile(t, compileInput(manifestFor(hosts...), append(allowHosts(t, hosts[0], hosts[1], hosts[3]), fileGrants...)))
	rev := slices.Clone(hosts)
	slices.Reverse(rev)
	revGrants := slices.Clone(fileGrants)
	slices.Reverse(revGrants)
	reordered := mustCompile(t, compileInput(manifestFor(rev...), append(revGrants, allowHosts(t, hosts[3], hosts[1], hosts[0])...)))
	if first.PolicyYAML != again.PolicyYAML || first.PolicyYAML != reordered.PolicyYAML {
		t.Error("compiled policy depends on run or input order")
	}
	if !reflect.DeepEqual(first.Decisions, reordered.Decisions) {
		t.Errorf("decisions depend on input order:\n%+v\n%+v", first.Decisions, reordered.Decisions)
	}
	if !slices.IsSortedFunc(first.Decisions, func(a, b Decision) int { return strings.Compare(a.Host, b.Host) }) {
		t.Error("decisions not sorted by host")
	}
}

func TestCompileRuleNamesNeverCollide(t *testing.T) {
	hosts := []string{"docs.example.org", "docs-example.org", "docs.example-org.net", "docs-example-org.net"}
	res := mustCompile(t, compileInput(manifestFor(hosts...), allowHosts(t, hosts...)))
	rules := repoRules(t, res.PolicyYAML)
	if len(rules) != len(hosts) {
		t.Errorf("%d rules for %d hosts: %v", len(rules), len(hosts), rules)
	}
	base, _ := renderBaselinePolicy(compileTestRepo, openshell.SandboxPolicyTemplate, compileTestProviders, nil)
	_, keys, _ := baselineInventory(base)
	for _, k := range keys {
		if strings.HasPrefix(k, repoRulePrefix) {
			t.Errorf("baseline rule %s is in the repo namespace", k)
		}
	}
	// A baseline that used the namespace is refused, not overwritten.
	tmpl := strings.Replace(string(openshell.SandboxPolicyTemplate), "  npm_registry:", "  repo_registry_npmjs_org:", 1)
	if _, err := compilePolicyFrom([]byte(tmpl), compileInput(nil, nil)); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("baseline in repo namespace: %v", err)
	}
}

func TestVerifyComposedRejectsChanges(t *testing.T) {
	in := compileInput(manifestFor("docs.example.org"), allowHosts(t, "docs.example.org"))
	res := mustCompile(t, in)
	base := sessionBaseline(t, in)
	rules := repoRules(t, res.PolicyYAML)
	if err := verifyComposed(base, res.PolicyYAML, rules); err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(res.PolicyYAML, "path: /usr/local/bin/gh", "path: /usr/bin/curl", 1)
	if err := verifyComposed(base, tampered, rules); err == nil {
		t.Error("changed baseline rule passed verification")
	}
	widened := strings.Replace(res.PolicyYAML, "method: HEAD", "method: POST", 1)
	if err := verifyComposed(base, widened, rules); err == nil {
		t.Error("changed repo rule passed verification")
	}
}
