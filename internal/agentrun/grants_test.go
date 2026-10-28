package agentrun

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseGrantsDefaults(t *testing.T) {
	gs, err := ParseGrants([]byte(`version: 1
grants:
  - host: Docs.Example.org
  - host: api.example.org
    repos: [example/other-repo]
    methods: [GET, POST]
    paths: ["/v1/**", "/health"]
`), "grants.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []Grant{
		{Host: "docs.example.org", Methods: []string{"GET", "HEAD"}, Paths: []string{"/**"}, Source: "grants.yaml grants[0]"},
		{Host: "api.example.org", Repos: []string{"example/other-repo"}, Methods: []string{"GET", "POST"}, Paths: []string{"/v1/**", "/health"}, Source: "grants.yaml grants[1]"},
	}
	if !reflect.DeepEqual(gs, want) {
		t.Errorf("got %+v\nwant %+v", gs, want)
	}
}

func TestParseGrantsRejects(t *testing.T) {
	tests := []struct{ name, src, want string }{
		{"bad version", "version: 2\ngrants: []\n", "unsupported version 2"},
		{"unknown top-level", "version: 1\nusers: []\n", "unsupported top-level key"},
		{"unknown field", "version: 1\ngrants:\n  - host: docs.example.org\n    port: 80\n", "field port not found"},
		{"duplicate key", "version: 1\ngrants:\n  - host: docs.example.org\n    host: x.example.org\n", "duplicate key"},
		{"anchor", "version: 1\ngrants:\n  - &g {host: docs.example.org}\n", "anchors"},
		{"bad method", "version: 1\ngrants:\n  - host: docs.example.org\n    methods: [CONNECT]\n", `"CONNECT" is not one of`},
		{"lowercase method", "version: 1\ngrants:\n  - host: docs.example.org\n    methods: [get]\n", `"get" is not one of`},
		{"duplicate method", "version: 1\ngrants:\n  - host: docs.example.org\n    methods: [GET, GET]\n", "duplicate"},
		{"relative path", "version: 1\ngrants:\n  - host: docs.example.org\n    paths: [api]\n", "not an absolute path"},
		{"path with query", "version: 1\ngrants:\n  - host: docs.example.org\n    paths: [\"/a?b\"]\n", "not an absolute path"},
		{"dot segment", "version: 1\ngrants:\n  - host: docs.example.org\n    paths: [\"/a/../b\"]\n", "dot segment"},
		{"bad repo", "version: 1\ngrants:\n  - host: docs.example.org\n    repos: [justaname]\n", "owner/repo"},
		{"bad host", "version: 1\ngrants:\n  - host: https://docs.example.org\n", "not a URL"},
		{"wildcard host", "version: 1\ngrants:\n  - host: \"*.example.org\"\n", "wildcard"},
		{"oversized", "version: 1\n#" + strings.Repeat("x", maxGrantsBytes) + "\n", "limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseGrants([]byte(tt.src), "g.yaml")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %v does not mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadGrantsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grants.yaml")
	if err := os.WriteFile(path, []byte("version: 1\ngrants:\n  - host: docs.example.org\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gs, err := LoadGrantsFile(path)
	if err != nil || len(gs) != 1 || gs[0].Source != path+" grants[0]" {
		t.Errorf("got %+v, %v", gs, err)
	}
	big := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(big, []byte("version: 1\n#"+strings.Repeat("x", 2*maxGrantsBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGrantsFile(big); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("oversized file: %v", err)
	}
	if _, err := LoadGrantsFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestAllowHostGrants(t *testing.T) {
	gs, err := AllowHostGrants([]string{"Docs.Example.org"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Grant{{Host: "docs.example.org", Methods: []string{"GET", "HEAD"}, Paths: []string{"/**"}, Source: "--allow-host Docs.Example.org"}}
	if !reflect.DeepEqual(gs, want) {
		t.Errorf("got %+v", gs)
	}
	for _, bad := range []string{"docs.example.org:8443", "https://docs.example.org", "*.example.org", ""} {
		if _, err := AllowHostGrants([]string{bad}); err == nil {
			t.Errorf("--allow-host %q accepted", bad)
		}
	}
}

// Each error names the host once: --allow-host already does, a grants file
// entry (named by index) gets it added.
func TestGrantErrorNamesHostOnce(t *testing.T) {
	_, err := AllowHostGrants([]string{"169.254.169.254"})
	want := `--allow-host 169.254.169.254: host "169.254.169.254": IP address literals are not allowed`
	if err == nil || err.Error() != want {
		t.Errorf("got %v, want %s", err, want)
	}
	g := Grant{Host: "docs.example.org", Methods: []string{"FETCH"}, Paths: []string{"/**"}, Source: "grants.yaml grants[0]"}
	if err := g.validate(); err == nil || !strings.HasPrefix(err.Error(), `grants.yaml grants[0] (host "docs.example.org"): methods:`) {
		t.Errorf("grants file entry: got %v", err)
	}
}

func TestGrantAppliesTo(t *testing.T) {
	any := Grant{Host: "docs.example.org"}
	limited := Grant{Host: "docs.example.org", Repos: []string{"example/other-repo"}}
	if !any.appliesTo("example/repo") {
		t.Error("grant without repos should apply to every repo")
	}
	if limited.appliesTo("example/repo") {
		t.Error("grant limited to another repo applied")
	}
	if !limited.appliesTo("Example/Other-Repo") {
		t.Error("repo match should be case-insensitive")
	}
}
