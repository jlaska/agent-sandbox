package agentrun

import (
	"slices"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		want    *Config
	}{
		{
			name:    "valid claude",
			args:    []string{"jlaska/agent-sandbox", "claude"},
			wantErr: false,
			want: &Config{
				Repo:    "jlaska/agent-sandbox",
				Harness: "claude",
			},
		},
		{
			name:    "valid pi",
			args:    []string{"jlaska/agent-sandbox", "pi"},
			wantErr: false,
			want: &Config{
				Repo:    "jlaska/agent-sandbox",
				Harness: "pi",
			},
		},
		{
			name:    "shell harness removed",
			args:    []string{"jlaska/agent-sandbox", "shell"},
			wantErr: true,
		},
		{
			name:    "with model override",
			args:    []string{"jlaska/agent-sandbox", "claude", "--model", "claude-3-opus"},
			wantErr: false,
			want: &Config{
				Repo:    "jlaska/agent-sandbox",
				Harness: "claude",
				Model:   "claude-3-opus",
			},
		},
		{
			name:    "with env var",
			args:    []string{"jlaska/agent-sandbox", "claude", "--env", "ANTHROPIC_API_KEY=sk-test"},
			wantErr: false,
			want: &Config{
				Repo:    "jlaska/agent-sandbox",
				Harness: "claude",
				EnvVars: []string{"ANTHROPIC_API_KEY=sk-test"},
			},
		},
		{
			name:    "with multiple env vars",
			args:    []string{"jlaska/agent-sandbox", "claude", "--env", "ANTHROPIC_API_KEY=sk-test", "--env", "ANTHROPIC_BASE_URL=https://example.com"},
			wantErr: false,
			want: &Config{
				Repo:    "jlaska/agent-sandbox",
				Harness: "claude",
				EnvVars: []string{"ANTHROPIC_API_KEY=sk-test", "ANTHROPIC_BASE_URL=https://example.com"},
			},
		},
		{
			name:    "env var without equals",
			args:    []string{"jlaska/agent-sandbox", "claude", "--env", "INVALID"},
			wantErr: true,
		},
		{
			name:    "missing repository",
			args:    []string{"claude"},
			wantErr: true,
		},
		{
			name:    "unknown harness",
			args:    []string{"jlaska/agent-sandbox", "unknown"},
			wantErr: true,
		},
		{
			name:    "help flag",
			args:    []string{"--help"},
			wantErr: false,
			want: &Config{
				Help: true,
			},
		},
		{
			name:    "no args shows help",
			args:    []string{},
			wantErr: false,
			want: &Config{
				Help: true,
			},
		},
		{
			name:    "list repos flag",
			args:    []string{"--list-repos"},
			wantErr: false,
			want: &Config{
				ListRepos: true,
			},
		},
		{
			name:    "passthrough args after --",
			args:    []string{"jlaska/agent-sandbox", "claude", "--", "-p", "fix the bug"},
			wantErr: false,
			want: &Config{
				Repo:            "jlaska/agent-sandbox",
				Harness:         "claude",
				PassthroughArgs: []string{"-p", "fix the bug"},
			},
		},
		{
			name:    "passthrough with model and flags",
			args:    []string{"jlaska/agent-sandbox", "claude", "--model", "opus-4-8", "--", "--verbose", "--allowedTools", "Bash"},
			wantErr: false,
			want: &Config{
				Repo:            "jlaska/agent-sandbox",
				Harness:         "claude",
				Model:           "opus-4-8",
				PassthroughArgs: []string{"--verbose", "--allowedTools", "Bash"},
			},
		},
		{
			name:    "bare -- with no passthrough",
			args:    []string{"jlaska/agent-sandbox", "claude", "--"},
			wantErr: false,
			want: &Config{
				Repo:            "jlaska/agent-sandbox",
				Harness:         "claude",
				PassthroughArgs: []string{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseArgs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if cfg.Help != tt.want.Help {
				t.Errorf("Help = %v, want %v", cfg.Help, tt.want.Help)
			}
			if cfg.ListRepos != tt.want.ListRepos {
				t.Errorf("ListRepos = %v, want %v", cfg.ListRepos, tt.want.ListRepos)
			}
			if cfg.Repo != tt.want.Repo {
				t.Errorf("Repo = %v, want %v", cfg.Repo, tt.want.Repo)
			}
			if cfg.Harness != tt.want.Harness {
				t.Errorf("Harness = %v, want %v", cfg.Harness, tt.want.Harness)
			}
			if cfg.Model != tt.want.Model {
				t.Errorf("Model = %v, want %v", cfg.Model, tt.want.Model)
			}
			if tt.want.EnvVars != nil {
				if len(cfg.EnvVars) != len(tt.want.EnvVars) {
					t.Errorf("EnvVars length = %d, want %d", len(cfg.EnvVars), len(tt.want.EnvVars))
				} else {
					for i, v := range cfg.EnvVars {
						if v != tt.want.EnvVars[i] {
							t.Errorf("EnvVars[%d] = %q, want %q", i, v, tt.want.EnvVars[i])
						}
					}
				}
			}
			if tt.want.PassthroughArgs != nil {
				if len(cfg.PassthroughArgs) != len(tt.want.PassthroughArgs) {
					t.Errorf("PassthroughArgs length = %d, want %d", len(cfg.PassthroughArgs), len(tt.want.PassthroughArgs))
				} else {
					for i, a := range cfg.PassthroughArgs {
						if a != tt.want.PassthroughArgs[i] {
							t.Errorf("PassthroughArgs[%d] = %q, want %q", i, a, tt.want.PassthroughArgs[i])
						}
					}
				}
			}
		})
	}
}

func TestValidateMalformedRepos(t *testing.T) {
	malformed := []string{
		"../traversal",
		"owner/../other",
		"-invalid/repo",
		"owner//repo",
		"",
		"noslash",
	}

	for _, repo := range malformed {
		t.Run(repo, func(t *testing.T) {
			cfg := &Config{
				Repo:    repo,
				Harness: "claude",
			}
			err := cfg.Validate()
			if err == nil {
				t.Errorf("Validate() should reject malformed repo %q", repo)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name: "valid config",
			cfg: &Config{
				Repo:    "jlaska/agent-sandbox",
				Harness: "claude",
			},
			wantErr: false,
		},
		{
			name: "missing repo",
			cfg: &Config{
				Harness: "claude",
			},
			wantErr: true,
		},
		{
			name: "missing harness",
			cfg: &Config{
				Repo: "jlaska/agent-sandbox",
			},
			wantErr: true,
		},
		{
			name: "unknown harness",
			cfg: &Config{
				Repo:    "jlaska/agent-sandbox",
				Harness: "invalid",
			},
			wantErr: true,
		},
		{
			name: "any valid repo accepted",
			cfg: &Config{
				Repo:    "unknown/repo",
				Harness: "claude",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseArgsNewFlags(t *testing.T) {
	base := []string{"jlaska/agent-sandbox", "claude"}
	ocBase := []string{"jlaska/agent-sandbox", "opencode"}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("LITELLM_API_KEY", "sk-lite")

	t.Run("all flags", func(t *testing.T) {
		cfg, err := ParseArgs(append(append([]string{}, ocBase...),
			"--image", "agent-opencode:local", "--no-mise", "--fresh-cache",
			"--mise-allow-host", "example.com", "--mise-allow-host", "dl.example.org:8443",
			"--inference", "anthropic", "--driver", "podman"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Image != "agent-opencode:local" || !cfg.NoMise || !cfg.FreshCache ||
			cfg.Inference != "anthropic" || cfg.Driver != "podman" {
			t.Errorf("unexpected config: %+v", cfg)
		}
		if len(cfg.MiseAllowHosts) != 2 || cfg.MiseAllowHosts[0] != "example.com" || cfg.MiseAllowHosts[1] != "dl.example.org:8443" {
			t.Errorf("MiseAllowHosts = %v", cfg.MiseAllowHosts)
		}
	})

	t.Run("flags before positionals and = form", func(t *testing.T) {
		cfg, err := ParseArgs([]string{"--driver", "docker", "--image=x:1", "jlaska/agent-sandbox", "pi"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Driver != "docker" || cfg.Image != "x:1" || cfg.Harness != "pi" {
			t.Errorf("unexpected config: %+v", cfg)
		}
	})

	for _, d := range []string{"docker", "podman", "kubernetes", "vm"} {
		if _, err := ParseArgs(append(base, "--driver", d)); err != nil {
			t.Errorf("--driver %s rejected: %v", d, err)
		}
	}
	for _, i := range []string{"litellm", "anthropic"} {
		if _, err := ParseArgs(append(append([]string{}, ocBase...), "--inference", i)); err != nil {
			t.Errorf("--inference %s rejected: %v", i, err)
		}
	}

	bad := map[string][]string{
		"bad driver":          {"--driver", "lxc"},
		"bad inference":       {"--inference", "openai"},
		"host with scheme":    {"--mise-allow-host", "https://example.com"},
		"host with path":      {"--mise-allow-host", "example.com/x"},
		"host wildcard":       {"--mise-allow-host", "*.example.com"},
		"host bad port":       {"--mise-allow-host", "example.com:99999"},
		"host zero port":      {"--mise-allow-host", "example.com:0"},
		"host non-numeric":    {"--mise-allow-host", "example.com:http"},
		"host empty":          {"--mise-allow-host", ""},
		"image whitespace":    {"--image", "a b"},
		"inference on claude": {"--inference", "anthropic"},
		"fresh with no-cache": {"--fresh-cache", "--no-cache"},
	}
	for name, extra := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseArgs(append(append([]string{}, base...), extra...)); err == nil {
				t.Errorf("expected error for %v", extra)
			}
		})
	}
}

func TestParseArgsPolicyFlags(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	cfg, err := ParseArgs([]string{"--allow-host", "docs.example.org", "jlaska/agent-sandbox", "claude",
		"--allow-host", "api.example.org", "--grants", "/tmp/grants.yaml", "--no-repo-policy", "--explain-policy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(cfg.AllowHosts, []string{"docs.example.org", "api.example.org"}) || cfg.GrantsFile != "/tmp/grants.yaml" ||
		!cfg.NoRepoPolicy || !cfg.ExplainPolicy || cfg.Repo != "jlaska/agent-sandbox" || cfg.Harness != "claude" {
		t.Errorf("unexpected config: %+v", cfg)
	}
	for _, h := range []string{"https://docs.example.org", "docs.example.org:8443", "*.example.org", "10.0.0.1", ""} {
		if _, err := ParseArgs([]string{"jlaska/agent-sandbox", "claude", "--allow-host", h}); err == nil {
			t.Errorf("--allow-host %q: expected error", h)
		}
	}
	// --generate-policy keeps printing the baseline policy only.
	for _, extra := range [][]string{
		{"--allow-host", "docs.example.org"}, {"--grants", "g.yaml"}, {"--no-repo-policy"}, {"--explain-policy"},
	} {
		if _, err := ParseArgs(append([]string{"--generate-policy", "jlaska/agent-sandbox"}, extra...)); err == nil ||
			!strings.Contains(err.Error(), "baseline policy only") {
			t.Errorf("--generate-policy %v: err = %v", extra, err)
		}
	}
	if cfg, err := ParseArgs([]string{"--generate-policy", "jlaska/agent-sandbox", "--mise-allow-host", "nodejs.org"}); err != nil || !cfg.GeneratePolicy {
		t.Errorf("--generate-policy: cfg=%+v err=%v", cfg, err)
	}
}

func TestShellHarnessRejected(t *testing.T) {
	_, err := ParseArgs([]string{"jlaska/agent-sandbox", "shell"})
	if err == nil || !strings.Contains(err.Error(), "shell harness was removed") {
		t.Errorf("err = %v", err)
	}
}

// The opencode backend is resolved once in Run, before anything is created;
// a run with no usable key fails without running a single command.
func TestRunOpencodeBackendValidatedEarly(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("LITELLM_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	cfg, err := ParseArgs([]string{"jlaska/agent-sandbox", "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	withMockExec(t, func(m *mockCmd) {
		if err := Run(cfg); err == nil {
			t.Error("opencode with no credentials must fail before any sandbox exists")
		}
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth")
		if err := Run(cfg); err == nil || !strings.Contains(err.Error(), "OAUTH") {
			t.Errorf("oauth-only err = %v", err)
		}
		if len(m.calls) != 0 {
			t.Errorf("commands ran before the backend check: %v", callStrings(m))
		}
	})
	if _, err := ParseArgs([]string{"jlaska/agent-sandbox", "claude", "--inference", "litellm"}); err == nil ||
		!strings.Contains(err.Error(), "opencode harness only") {
		t.Errorf("err = %v", err)
	}
}

// --version prints the stamped version and needs no positionals.
func TestParseArgsVersion(t *testing.T) {
	orig := Version
	Version = "v9.9.9"
	defer func() { Version = orig }()
	var cfg *Config
	var err error
	out := captureStdout(t, func() { cfg, err = ParseArgs([]string{"--version"}) })
	if err != nil || !cfg.Version || cfg.Help {
		t.Fatalf("--version: cfg=%+v err=%v", cfg, err)
	}
	if out != "agent-run v9.9.9\n" {
		t.Errorf("output = %q", out)
	}
	withMockExec(t, func(m *mockCmd) {
		if err := Run(cfg); err != nil || len(m.calls) != 0 {
			t.Errorf("Run(--version) err=%v calls=%v, want no commands", err, m.calls)
		}
	})
}

// --diag runs where a run would be refused: with no positionals at all, and
// for opencode without an inference key (whose diag line explains why).
func TestParseArgsDiag(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("LITELLM_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")

	cfg, err := ParseArgs([]string{"--diag"})
	if err != nil || !cfg.Diag || cfg.Help {
		t.Fatalf("bare --diag: cfg=%+v err=%v", cfg, err)
	}

	cfg, err = ParseArgs([]string{"jlaska/agent-sandbox", "opencode", "--diag"})
	if err != nil {
		t.Fatalf("opencode --diag without a key: %v", err)
	}
	if cfg.Repo != "jlaska/agent-sandbox" || cfg.Harness != "opencode" || !cfg.Diag {
		t.Errorf("cfg = %+v", cfg)
	}
	withMockExec(t, func(m *mockCmd) {
		var runErr error
		out := captureStdout(t, func() { runErr = Run(cfg) })
		if runErr != nil {
			t.Fatal(runErr)
		}
		if !strings.Contains(out, "opencode backend:   unavailable (") {
			t.Errorf("diag lacks the opencode backend line:\n%s", out)
		}
	})
}

func TestParseArgsMiseUnsafe(t *testing.T) {
	cfg, err := ParseArgs([]string{"jlaska/agent-sandbox", "claude", "--mise-unsafe"})
	if err != nil || !cfg.MiseUnsafe {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if _, err := ParseArgs([]string{"jlaska/agent-sandbox", "claude", "--mise-unsafe", "--no-mise"}); err == nil {
		t.Error("--mise-unsafe with --no-mise must be rejected")
	}
}
