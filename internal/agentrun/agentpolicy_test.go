package agentrun

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseAgentPolicyValid(t *testing.T) {
	p, err := ParseAgentPolicy([]byte(`version: 1
network:
  - host: docs.example.org
    reason: "fetch API reference docs"
  - host: api.example.org
    reason: schema downloads
    optional: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Network) != 2 || p.Network[0].Host != "docs.example.org" || p.Network[0].Optional || !p.Network[1].Optional {
		t.Errorf("unexpected manifest: %+v", p)
	}
	for _, src := range []string{"version: 1\n", "version: 1\nnetwork: []\n"} {
		p, err := ParseAgentPolicy([]byte(src))
		if err != nil || len(p.Network) != 0 {
			t.Errorf("%q: got %+v, %v", src, p, err)
		}
	}
}

func TestParseAgentPolicyRejects(t *testing.T) {
	entry := func(host string) string { return "  - host: " + host + "\n    reason: r\n" }
	var many strings.Builder
	many.WriteString("version: 1\nnetwork:\n")
	for i := 0; i <= maxNetworkRequests; i++ {
		many.WriteString(entry(fmt.Sprintf("h%d.example.org", i)))
	}
	tests := []struct {
		name, src, want string
	}{
		{"empty file", "", "empty"},
		{"bad YAML", "version: [1\n", "invalid YAML"},
		{"not a mapping", "- 1\n", "top level must be a mapping"},
		{"two documents", "version: 1\n---\nversion: 1\n", "exactly one YAML document"},
		{"missing version", "network: []\n", "unsupported version 0"},
		{"version 2", "version: 2\n", "unsupported version 2"},
		{"quoted version", "version: \"1\"\n", "invalid content"},
		{"filesystem section", "version: 1\nfilesystem:\n  requests: []\n", `unsupported top-level key "filesystem"`},
		{"other top-level key", "version: 1\nnetwork_policies: {}\n", "unsupported top-level key"},
		{"unknown request field", "version: 1\nnetwork:\n  - host: docs.example.org\n    reason: r\n    port: 8443\n", "field port not found"},
		{"duplicate top-level key", "version: 1\nversion: 1\n", `duplicate key "version"`},
		{"duplicate request key", "version: 1\nnetwork:\n  - host: docs.example.org\n    host: other.example.org\n    reason: r\n", `duplicate key "host"`},
		{"anchor", "version: 1\nnetwork:\n  - &a\n    host: docs.example.org\n    reason: r\n", "anchors are not allowed"},
		{"alias", "version: 1\nx: &a 1\nnetwork: *a\n", "anchors are not allowed"},
		{"bare alias", "version: 1\nnetwork:\n  - host: docs.example.org\n    reason: *r\n", "invalid YAML"},
		{"merge key", "version: 1\nnetwork:\n  - <<: {host: docs.example.org}\n    reason: r\n", "merge keys"},
		{"explicit tag", "version: 1\nnetwork:\n  - host: !!str docs.example.org\n    reason: r\n", "explicit YAML tags"},
		{"oversized", "version: 1\n#" + strings.Repeat("x", maxManifestBytes) + "\n", "limit"},
		{"too many entries", many.String(), "too many network requests"},
		{"duplicate host", "version: 1\nnetwork:\n" + entry("docs.example.org") + entry("docs.example.org"), "duplicate host docs.example.org"},
		{"missing reason", "version: 1\nnetwork:\n  - host: docs.example.org\n", "reason is required"},
		{"blank reason", "version: 1\nnetwork:\n  - host: docs.example.org\n    reason: \"  \"\n", "reason is required"},
		{"control char in reason", "version: 1\nnetwork:\n  - host: docs.example.org\n    reason: \"a\\x1b[2Jb\"\n", "control"},
		{"long reason", "version: 1\nnetwork:\n  - host: docs.example.org\n    reason: " + strings.Repeat("r", maxReasonLen+1) + "\n", "longer than"},
		{"bad optional", "version: 1\nnetwork:\n  - host: docs.example.org\n    reason: r\n    optional: maybe\n", "invalid content"},
		{"missing host", "version: 1\nnetwork:\n  - reason: r\n", "host is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseAgentPolicy([]byte(tt.src))
			if err == nil {
				t.Fatalf("accepted:\n%s", tt.src)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestParseAgentPolicyBadHosts(t *testing.T) {
	tests := map[string]string{
		"https://docs.example.org":       "not a URL",
		"docs.example.org:8443":          "port",
		"docs.example.org/api":           "path",
		"user@docs.example.org":          "userinfo",
		"*.example.org":                  "wildcard",
		"Docs.Example.org":               "lowercase",
		"docs.example.org.":              "not a valid DNS name",
		"-docs.example.org":              "not a valid DNS name",
		"docs_x.example.org":             "not a valid DNS name",
		"docs..example.org":              "not a valid DNS name",
		"::1":                            "IP address literal",
		"[::1]":                          "IP address literal",
		"fe80::1%en0":                    "IP address literal",
		"192.0.2.1":                      "IP address literal",
		strings.Repeat("a", 64) + ".org": "not a valid DNS name",
	}
	for host, want := range tests {
		t.Run(host, func(t *testing.T) {
			src := "version: 1\nnetwork:\n  - host: \"" + host + "\"\n    reason: r\n"
			_, err := ParseAgentPolicy([]byte(src))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error %v does not mention %q", err, want)
			}
		})
	}
}
