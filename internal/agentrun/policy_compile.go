package agentrun

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jlaska/agent-sandbox/openshell"
)

// CompilePolicy composes the session policy from the embedded baseline, the
// repository's .agent-policy.yaml requests and the caller's grants. It is
// pure: it reads no files and creates nothing, so the caller can run it (and
// fail) before any sandbox resource exists.
//
// Order of checks:
//  1. The baseline renders exactly as GenerateSessionPolicy renders it.
//  2. Every request host and grant host passes the universal restrictions
//     (hostRestriction). No grant can override them.
//  3. Each request is matched against the grants that apply to Repo. A
//     required request without one is an error; an optional one is omitted
//     with a warning. A grant no request uses adds nothing (info).
//  4. Each granted host becomes one repo_* network_policies entry, inserted
//     under network_policies without touching a baseline byte, and the result
//     is decoded again to prove every baseline rule is unchanged.
//
// All hard failures are collected and returned together. The result is
// returned even on error (without PolicyYAML) so its decisions can be shown.
func CompilePolicy(in CompileInput) (*CompileResult, error) {
	return compilePolicyFrom(openshell.SandboxPolicyTemplate, in)
}

// CompileInput is everything CompilePolicy needs. Manifest is nil when the
// repository has no .agent-policy.yaml.
type CompileInput struct {
	Repo      string // owner/name
	Harness   string // HarnessClaude, HarnessPi or HarnessOpencode
	Providers SessionProviders
	MiseHosts []string // --mise-allow-host values, as for GenerateSessionPolicy
	Manifest  *AgentPolicy
	Grants    []Grant // from AllowHostGrants and ParseGrants/LoadGrantsFile
}

// CompileResult is the compiled policy and how each request was decided.
type CompileResult struct {
	// PolicyYAML is the policy to pass to openshell sandbox create. It is
	// byte-identical to the baseline when no request is granted, and empty
	// when CompilePolicy returns an error.
	PolicyYAML string
	// Decisions has one entry per manifest request, sorted by host.
	Decisions []Decision
	// Diagnostics are sorted by severity (error, warning, info), then host.
	Diagnostics []Diagnostic
}

// Outcome is what happened to one repository request.
type Outcome string

const (
	OutcomeGranted Outcome = "granted" // a rule was generated
	OutcomeOmitted Outcome = "omitted" // optional, no grant: left out
	OutcomeDenied  Outcome = "denied"  // compile fails
)

// Decision records how one request was decided and why.
type Decision struct {
	Host     string
	Reason   string // the repository's stated reason
	Optional bool
	Outcome  Outcome
	Detail   string // why it got this outcome
	// Set only for granted requests.
	Rule    string   // network_policies key
	Methods []string // in grantMethods order
	Paths   []string // sorted
	Sources []string // grants that authorized it, sorted
}

func (d Decision) String() string {
	s := fmt.Sprintf("%s %s: %s", d.Outcome, d.Host, d.Detail)
	if d.Outcome == OutcomeGranted {
		s += fmt.Sprintf(" [rule %s; %s on %s]", d.Rule, strings.Join(d.Methods, ","), strings.Join(d.Paths, ","))
	}
	return s
}

// Severity ranks a Diagnostic. Only SeverityError fails the compile.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Diagnostic is one message about a request or grant.
type Diagnostic struct {
	Severity Severity
	Host     string
	Message  string
}

func (d Diagnostic) String() string { return fmt.Sprintf("%s: %s", d.Severity, d.Message) }

// repoRulePrefix namespaces generated network_policies keys. The baseline
// may not use it (compilePolicyFrom checks), so a generated rule can never
// replace a baseline rule.
const repoRulePrefix = "repo_"

// repoRuleKey maps a host to its rule key. DNS names contain no underscore,
// so replacing dots with underscores is reversible and two hosts never share
// a key.
func repoRuleKey(host string) string {
	return repoRulePrefix + strings.ReplaceAll(host, ".", "_")
}

// harnessRepoBinaries are the binaries generated rules bind, per harness:
// the harness's own process, as in the inference rules. Tools the agent runs
// match through OpenShell's ancestor matching; tools it does not spawn (git,
// gh, mise) do not.
var harnessRepoBinaries = map[string][]string{
	HarnessClaude:   {"/usr/local/bin/claude", "/usr/local/bin/node"},
	HarnessPi:       {"/usr/local/bin/node"}, // pi is a node script
	HarnessOpencode: {"/usr/local/bin/opencode"},
}

func compilePolicyFrom(tmpl []byte, in CompileInput) (*CompileResult, error) {
	res := &CompileResult{}
	binaries, ok := harnessRepoBinaries[in.Harness]
	if !ok {
		return res, fmt.Errorf("compile policy: unknown harness %q", in.Harness)
	}
	baseline, err := renderBaselinePolicy(in.Repo, tmpl, in.Providers, in.MiseHosts)
	if err != nil {
		return res, err
	}
	baseHosts, baseKeys, err := baselineInventory(baseline)
	if err != nil {
		return res, err
	}
	for _, k := range baseKeys {
		if strings.HasPrefix(k, repoRulePrefix) {
			return res, fmt.Errorf("compile policy: baseline rule %s uses the reserved %s prefix", k, repoRulePrefix)
		}
	}

	c := &compiler{res: res, repo: in.Repo, baseHosts: baseHosts}
	grants := c.checkGrants(in.Grants)
	var requests []NetworkRequest
	if in.Manifest != nil {
		if err := in.Manifest.validate(); err != nil {
			c.add(SeverityError, "", fmt.Sprintf("%s: %v", AgentPolicyFile, err))
		} else {
			requests = in.Manifest.Network
		}
	}
	rules := c.decide(requests, grants, binaries)

	sort.Slice(res.Decisions, func(i, j int) bool { return res.Decisions[i].Host < res.Decisions[j].Host })
	sevRank := map[Severity]int{SeverityError: 0, SeverityWarning: 1, SeverityInfo: 2}
	sort.SliceStable(res.Diagnostics, func(i, j int) bool {
		a, b := res.Diagnostics[i], res.Diagnostics[j]
		if a.Severity != b.Severity {
			return sevRank[a.Severity] < sevRank[b.Severity]
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.Message < b.Message
	})
	var errs []error
	for _, d := range res.Diagnostics {
		if d.Severity == SeverityError {
			errs = append(errs, errors.New(d.Message))
		}
	}
	if len(errs) > 0 {
		return res, fmt.Errorf("repository policy rejected:\n%w", errors.Join(errs...))
	}

	policy, err := composePolicy(baseline, rules)
	if err != nil {
		return res, err
	}
	res.PolicyYAML = policy
	return res, nil
}

// compiler accumulates decisions and diagnostics for one compile.
type compiler struct {
	res       *CompileResult
	repo      string
	baseHosts map[string][]string
}

func (c *compiler) add(sev Severity, host, msg string) {
	c.res.Diagnostics = append(c.res.Diagnostics, Diagnostic{Severity: sev, Host: host, Message: msg})
}

// checkGrants re-validates each grant and applies the universal restrictions,
// returning the grants that pass. A restricted grant is a hard failure even
// when nothing requests its host or it is limited to another repo: such a
// grant is never valid.
func (c *compiler) checkGrants(grants []Grant) []Grant {
	var ok []Grant
	for _, g := range grants {
		if err := g.validate(); err != nil {
			c.add(SeverityError, g.Host, err.Error())
			continue
		}
		if why := hostRestriction(g.Host, c.baseHosts); why != "" {
			c.add(SeverityError, g.Host, fmt.Sprintf("%s: host %s cannot be granted: %s", g.Source, g.Host, why))
			continue
		}
		ok = append(ok, g)
	}
	return ok
}

// decide records a Decision for every request and returns the rules to
// generate, keyed by rule name.
func (c *compiler) decide(requests []NetworkRequest, grants []Grant, binaries []string) map[string]repoRule {
	requested := map[string]bool{}
	rules := map[string]repoRule{}
	for _, r := range requests {
		requested[r.Host] = true
		d := Decision{Host: r.Host, Reason: r.Reason, Optional: r.Optional}
		if why := hostRestriction(r.Host, c.baseHosts); why != "" {
			d.Outcome, d.Detail = OutcomeDenied, why
			c.res.Decisions = append(c.res.Decisions, d)
			c.add(SeverityError, r.Host, fmt.Sprintf("%s requests %s, which is not allowed: %s", AgentPolicyFile, r.Host, why))
			continue
		}
		var matched []Grant
		for _, g := range grants {
			if g.Host == r.Host && g.appliesTo(c.repo) {
				matched = append(matched, g)
			}
		}
		if len(matched) == 0 {
			hint := fmt.Sprintf("authorize it with --allow-host %s or a grants file entry for host %s", r.Host, r.Host)
			if r.Optional {
				d.Outcome, d.Detail = OutcomeOmitted, "optional and not granted; "+hint
				c.add(SeverityWarning, r.Host, fmt.Sprintf("optional request for %s (%q) is not granted and was omitted; %s", r.Host, r.Reason, hint))
			} else {
				d.Outcome, d.Detail = OutcomeDenied, "required and not granted; "+hint
				c.add(SeverityError, r.Host, fmt.Sprintf("%s requires %s (%q), which is not granted; %s", AgentPolicyFile, r.Host, r.Reason, hint))
			}
			c.res.Decisions = append(c.res.Decisions, d)
			continue
		}
		d.Outcome = OutcomeGranted
		d.Rule = repoRuleKey(r.Host)
		d.Methods, d.Paths, d.Sources = unionGrants(matched)
		d.Detail = "authorized by " + strings.Join(d.Sources, "; ")
		c.res.Decisions = append(c.res.Decisions, d)
		rules[d.Rule] = newRepoRule(d, binaries)
	}

	// Grants cap requests; they never add egress on their own.
	for _, g := range grants {
		switch {
		case !g.appliesTo(c.repo):
			c.add(SeverityInfo, g.Host, fmt.Sprintf("%s: grant for %s is limited to %s and does not apply to %s", g.Source, g.Host, strings.Join(g.Repos, ", "), c.repo))
		case !requested[g.Host]:
			c.add(SeverityInfo, g.Host, fmt.Sprintf("%s: grant for %s is unused (no %s request for it) and adds nothing", g.Source, g.Host, AgentPolicyFile))
		}
	}
	return rules
}

// unionGrants merges the methods, paths and sources of grants for one host.
// All of them are trusted, so their union is what the caller authorized.
func unionGrants(grants []Grant) (methods, paths, sources []string) {
	ms, ps := map[string]bool{}, map[string]bool{}
	for _, g := range grants {
		for _, m := range g.Methods {
			ms[m] = true
		}
		for _, p := range g.Paths {
			if !ps[p] {
				ps[p] = true
				paths = append(paths, p)
			}
		}
		sources = append(sources, g.Source)
	}
	for _, m := range grantMethods {
		if ms[m] {
			methods = append(methods, m)
		}
	}
	sort.Strings(paths)
	sort.Strings(sources)
	return methods, paths, sources
}

// ---- universal host restrictions ------------------------------------------

// localSuffixes name hosts on the local machine, cluster or LAN.
var localSuffixes = []string{".localhost", ".local", ".cluster.local", ".internal", ".svc", ".home.arpa", ".lan"}

// metadataHosts are cloud instance-metadata names that do not already end in
// a local suffix.
var metadataHosts = map[string]bool{
	"metadata.goog":              true,
	"metadata.google.internal":   true,
	"instance-data.ec2.internal": true,
}

// githubDomains are GitHub-operated domains. The baseline scopes GitHub
// access to the target repo, so no other GitHub host may be added.
var githubDomains = []string{"github.com", "githubusercontent.com", "github.io", "ghcr.io"}

var numericLabel = regexp.MustCompile(`^(0x[0-9a-f]*|[0-9]+)$`)

// hostRestriction returns why host may never be reached through a repo rule,
// or "" if it may. baseHosts maps each host the baseline governs to its rule
// keys. These checks run before any grant is considered.
func hostRestriction(host string, baseHosts map[string][]string) string {
	if err := validateDNSName(host); err != nil {
		return err.Error()
	}
	labels := strings.Split(host, ".")
	// A numeric last label is never a DNS name: resolvers read 127.1,
	// 2130706433 and 0x7f.0.0.1 as IPv4 addresses.
	if isIPLiteral(host) || numericLabel.MatchString(labels[len(labels)-1]) {
		return "IP address literals are not allowed"
	}
	run := 0
	for _, l := range labels {
		if numericLabel.MatchString(l) {
			if run++; run == 4 {
				return "names that embed an IPv4 address (wildcard DNS) are not allowed"
			}
		} else {
			run = 0
		}
	}
	if metadataHosts[host] {
		return "cloud metadata endpoints are not allowed"
	}
	if host == "localhost" || len(labels) == 1 {
		return "local and single-label names are not allowed"
	}
	for _, s := range localSuffixes {
		if strings.HasSuffix(host, s) {
			return fmt.Sprintf("local and internal names (*%s) are not allowed", s)
		}
	}
	if keys, ok := baseHosts[host]; ok {
		return fmt.Sprintf("already governed by the baseline policy (%s); a repo rule would broaden it", strings.Join(keys, ", "))
	}
	for _, d := range githubDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return "GitHub hosts outside the baseline are not allowed"
		}
	}
	return ""
}

// ---- composition -----------------------------------------------------------

// repoRule is a generated network_policies entry. Its fields are the only
// ones a repo rule can have: no credential_binding, no access preset.
type repoRule struct {
	Name      string         `yaml:"name"`
	Endpoints []repoEndpoint `yaml:"endpoints"`
	Binaries  []repoBinary   `yaml:"binaries"`
}

type repoEndpoint struct {
	Host        string      `yaml:"host"`
	Port        int         `yaml:"port"`
	Protocol    string      `yaml:"protocol"`
	Enforcement string      `yaml:"enforcement"`
	Rules       []repoAllow `yaml:"rules"`
}

type repoAllow struct {
	Allow repoMatch `yaml:"allow"`
}

type repoMatch struct {
	Method string `yaml:"method"`
	Path   string `yaml:"path"`
}

type repoBinary struct {
	Path string `yaml:"path"`
}

// newRepoRule builds the rule for a granted decision: HTTPS on 443, L7
// enforced, one allow per path × method.
func newRepoRule(d Decision, binaries []string) repoRule {
	ep := repoEndpoint{Host: d.Host, Port: 443, Protocol: "rest", Enforcement: "enforce"}
	for _, p := range d.Paths {
		for _, m := range d.Methods {
			ep.Rules = append(ep.Rules, repoAllow{Allow: repoMatch{Method: m, Path: p}})
		}
	}
	r := repoRule{Name: repoRuleKey(d.Host), Endpoints: []repoEndpoint{ep}}
	for _, b := range binaries {
		r.Binaries = append(r.Binaries, repoBinary{Path: b})
	}
	return r
}

// baselineInventory returns the hosts the baseline's network_policies govern
// (host -> sorted rule keys) and its sorted rule keys.
func baselineInventory(baseline string) (map[string][]string, []string, error) {
	var doc struct {
		NetworkPolicies map[string]struct {
			Endpoints []struct {
				Host string `yaml:"host"`
			} `yaml:"endpoints"`
		} `yaml:"network_policies"`
	}
	if err := yaml.Unmarshal([]byte(baseline), &doc); err != nil {
		return nil, nil, fmt.Errorf("baseline policy is not valid YAML: %w", err)
	}
	if len(doc.NetworkPolicies) == 0 {
		return nil, nil, fmt.Errorf("baseline policy has no network_policies")
	}
	hosts := map[string][]string{}
	var keys []string
	for k, r := range doc.NetworkPolicies {
		keys = append(keys, k)
		for _, e := range r.Endpoints {
			h := strings.ToLower(e.Host)
			if !slices.Contains(hosts[h], k) {
				hosts[h] = append(hosts[h], k)
			}
		}
	}
	for h := range hosts {
		sort.Strings(hosts[h])
	}
	sort.Strings(keys)
	return hosts, keys, nil
}

// networkPoliciesHeader matches the top-level network_policies key line.
var networkPoliciesHeader = regexp.MustCompile(`(?m)^network_policies:[ \t]*\n`)

// renderRepoRules serializes rules as network_policies entries (two-space
// indented, sorted by key). Empty when there are none.
func renderRepoRules(rules map[string]repoRule) (string, error) {
	if len(rules) == 0 {
		return "", nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(rules); err != nil { // yaml.v3 sorts map keys
		return "", fmt.Errorf("encode repo rules: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("encode repo rules: %w", err)
	}
	var out strings.Builder
	out.WriteString("  # Generated from " + AgentPolicyFile + " requests authorized by caller grants.\n")
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		out.WriteString("  " + line + "\n")
	}
	return out.String(), nil
}

// composePolicy inserts the repo rules directly under network_policies,
// leaving every baseline byte in place, then verifies the result.
func composePolicy(baseline string, rules map[string]repoRule) (string, error) {
	block, err := renderRepoRules(rules)
	if err != nil {
		return "", err
	}
	if block == "" {
		return baseline, nil
	}
	locs := networkPoliciesHeader.FindAllStringIndex(baseline, -1)
	if len(locs) != 1 {
		return "", fmt.Errorf("compile policy: baseline has %d network_policies keys, want 1", len(locs))
	}
	at := locs[0][1]
	composed := baseline[:at] + "\n" + block + baseline[at:]
	if err := verifyComposed(baseline, composed, rules); err != nil {
		return "", fmt.Errorf("compile policy: %w", err)
	}
	return composed, nil
}

// verifyComposed decodes both documents and checks that composed is exactly
// the baseline plus rules: every top-level section and baseline rule is
// structurally unchanged, and each added rule decodes (strictly) to what was
// generated.
func verifyComposed(baseline, composed string, rules map[string]repoRule) error {
	var base, comp map[string]any
	if err := yaml.Unmarshal([]byte(baseline), &base); err != nil {
		return fmt.Errorf("baseline is not valid YAML: %w", err)
	}
	if err := yaml.Unmarshal([]byte(composed), &comp); err != nil {
		return fmt.Errorf("composed policy is not valid YAML: %w", err)
	}
	if len(base) != len(comp) {
		return fmt.Errorf("composed policy has %d top-level keys, baseline has %d", len(comp), len(base))
	}
	for k, v := range base {
		if k == "network_policies" {
			continue
		}
		if !reflect.DeepEqual(v, comp[k]) {
			return fmt.Errorf("composed policy changed baseline section %s", k)
		}
	}
	baseNP, _ := base["network_policies"].(map[string]any)
	compNP, _ := comp["network_policies"].(map[string]any)
	if len(compNP) != len(baseNP)+len(rules) {
		return fmt.Errorf("composed policy has %d rules, want %d baseline + %d repo", len(compNP), len(baseNP), len(rules))
	}
	for k, v := range baseNP {
		if !reflect.DeepEqual(v, compNP[k]) {
			return fmt.Errorf("composed policy changed baseline rule %s", k)
		}
	}
	for k, want := range rules {
		raw, ok := compNP[k]
		if !ok {
			return fmt.Errorf("composed policy lacks repo rule %s", k)
		}
		b, err := yaml.Marshal(raw)
		if err != nil {
			return err
		}
		var got repoRule
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&got); err != nil {
			return fmt.Errorf("repo rule %s does not round-trip: %w", k, err)
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("repo rule %s does not round-trip", k)
		}
	}
	return nil
}
