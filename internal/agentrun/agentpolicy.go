package agentrun

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// AgentPolicyFile is the repository manifest a repo uses to request extra
// network access. It is untrusted input: it holds requests, never
// authorization. CompilePolicy grants a request only when a caller grant
// (--allow-host or a grants file) covers it.
const AgentPolicyFile = ".agent-policy.yaml"

// Manifest limits. The manifest comes from the repository, so every bound is
// enforced before its content is used.
const (
	maxManifestBytes   = 64 << 10
	maxNetworkRequests = 50
	maxReasonLen       = 256
)

// AgentPolicy is a parsed, validated .agent-policy.yaml (version 1).
//
//	version: 1
//	network:
//	  - host: docs.example.org
//	    reason: "fetch API reference docs"
//	    optional: false   # default false
type AgentPolicy struct {
	Version int              `yaml:"version"`
	Network []NetworkRequest `yaml:"network"`
}

// NetworkRequest asks for HTTPS access to one host. The repo cannot choose a
// port, protocol, method or path: those come from the matching grant.
type NetworkRequest struct {
	Host   string `yaml:"host"`
	Reason string `yaml:"reason"`
	// Optional requests that no grant covers are omitted with a warning;
	// required ones fail the compile.
	Optional bool `yaml:"optional"`
}

// ParseAgentPolicy strictly decodes a repository manifest. It rejects files
// over maxManifestBytes, multiple documents, anchors, aliases, explicit tags,
// duplicate or unknown keys, any version other than 1, unsupported sections
// (filesystem and anything else), and invalid requests (see validate).
func ParseAgentPolicy(data []byte) (*AgentPolicy, error) {
	var p AgentPolicy
	if err := decodeStrictYAML(data, maxManifestBytes, map[string]bool{"version": true, "network": true}, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", AgentPolicyFile, err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", AgentPolicyFile, err)
	}
	return &p, nil
}

// validate checks a decoded manifest. CompilePolicy calls it again, so a
// manifest built in code gets the same checks as a parsed one.
func (p *AgentPolicy) validate() error {
	if p.Version != 1 {
		return fmt.Errorf("unsupported version %d (want version: 1)", p.Version)
	}
	if len(p.Network) > maxNetworkRequests {
		return fmt.Errorf("too many network requests: %d (limit %d)", len(p.Network), maxNetworkRequests)
	}
	var errs []error
	seen := map[string]bool{}
	for i, r := range p.Network {
		where := fmt.Sprintf("network[%d]", i)
		if err := validateDNSName(r.Host); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
			continue
		}
		if seen[r.Host] {
			errs = append(errs, fmt.Errorf("%s: duplicate host %s", where, r.Host))
		}
		seen[r.Host] = true
		if err := validateReason(r.Reason); err != nil {
			errs = append(errs, fmt.Errorf("%s (%s): %w", where, r.Host, err))
		}
	}
	return errors.Join(errs...)
}

// validateReason requires a short, printable reason. It is shown to the
// caller (--explain-policy), so control characters, which could rewrite the
// terminal, are rejected.
func validateReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("reason is required")
	}
	if len(reason) > maxReasonLen {
		return fmt.Errorf("reason is longer than %d bytes", maxReasonLen)
	}
	for _, r := range reason {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return fmt.Errorf("reason contains a control or invalid character")
		}
	}
	return nil
}

// dnsNamePattern is a bare lowercase DNS name: LDH labels of 1-63 characters,
// no leading or trailing hyphen, no trailing dot.
var dnsNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// validateDNSName checks that host is a bare lowercase DNS name, naming the
// first problem found. Numeric forms that only resolvers read as IPv4
// (127.1, 0x7f.0.0.1) pass this syntax check; hostRestriction denies them.
func validateDNSName(host string) error {
	switch {
	case host == "":
		return fmt.Errorf("host is required")
	case strings.Contains(host, "://"):
		return fmt.Errorf("host %q: must be a bare hostname, not a URL (no scheme)", host)
	case strings.Contains(host, "@"):
		return fmt.Errorf("host %q: must not contain userinfo", host)
	case strings.ContainsAny(host, "/?#"):
		return fmt.Errorf("host %q: must not contain a path, query or fragment", host)
	case strings.Contains(host, "*"):
		return fmt.Errorf("host %q: wildcards are not allowed", host)
	case isIPLiteral(host):
		return fmt.Errorf("host %q: IP address literals are not allowed", host)
	case strings.Contains(host, ":"):
		return fmt.Errorf("host %q: must not contain a port (the port is always 443)", host)
	case host != strings.ToLower(host):
		return fmt.Errorf("host %q: must be lowercase", host)
	case len(host) > 253 || !dnsNamePattern.MatchString(host):
		return fmt.Errorf("host %q: not a valid DNS name", host)
	}
	return nil
}

// isIPLiteral reports whether host parses as an IPv4 or IPv6 address,
// optionally bracketed or with a zone. Numeric forms that only resolvers
// accept (decimal, hex, short dotted) are caught by hostRestriction.
func isIPLiteral(host string) bool {
	h := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	_, err := netip.ParseAddr(h)
	return err == nil
}

// decodeStrictYAML decodes one YAML document from data into out. Beyond
// yaml.v3's KnownFields check it rejects oversized input, extra documents,
// anchors, aliases and explicit tags, duplicate mapping keys at any depth, and
// top-level keys outside allowedTop.
func decodeStrictYAML(data []byte, maxBytes int, allowedTop map[string]bool, out any) error {
	if len(data) > maxBytes {
		return fmt.Errorf("file is %d bytes (limit %d)", len(data), maxBytes)
	}
	var root yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("file is empty")
		}
		return fmt.Errorf("invalid YAML: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("file must contain exactly one YAML document")
	}
	if err := checkYAMLNode(&root); err != nil {
		return err
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("top level must be a mapping")
	}
	top := root.Content[0]
	for i := 0; i < len(top.Content); i += 2 {
		k := top.Content[i].Value
		if !allowedTop[k] {
			return fmt.Errorf("line %d: unsupported top-level key %q", top.Content[i].Line, k)
		}
	}
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	if err := strict.Decode(out); err != nil {
		return fmt.Errorf("invalid content: %w", err)
	}
	return nil
}

// checkYAMLNode walks n, rejecting aliases, anchors, explicit tags and
// duplicate mapping keys.
func checkYAMLNode(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode {
		return fmt.Errorf("line %d: YAML aliases are not allowed", n.Line)
	}
	if n.Anchor != "" {
		return fmt.Errorf("line %d: YAML anchors are not allowed", n.Line)
	}
	if n.Style&yaml.TaggedStyle != 0 {
		return fmt.Errorf("line %d: explicit YAML tags are not allowed", n.Line)
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: mapping keys must be scalars", k.Line)
			}
			// A merge key splices another mapping in, which would bypass
			// the duplicate check below.
			if k.Value == "<<" || k.Tag == "!!merge" {
				return fmt.Errorf("line %d: YAML merge keys are not allowed", k.Line)
			}
			if seen[k.Value] {
				return fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
			}
			seen[k.Value] = true
		}
	}
	for _, c := range n.Content {
		if err := checkYAMLNode(c); err != nil {
			return err
		}
	}
	return nil
}
