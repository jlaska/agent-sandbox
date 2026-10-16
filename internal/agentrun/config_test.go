package agentrun

import (
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

	t.Run("all flags", func(t *testing.T) {
		cfg, err := ParseArgs(append(base,
			"--image", "agent-claude:local", "--no-mise", "--fresh-cache", "--no-cache",
			"--mise-allow-host", "example.com", "--mise-allow-host", "dl.example.org:8443",
			"--inference", "anthropic", "--driver", "podman"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Image != "agent-claude:local" || !cfg.NoMise || !cfg.FreshCache || !cfg.NoCache ||
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
		if _, err := ParseArgs(append(base, "--inference", i)); err != nil {
			t.Errorf("--inference %s rejected: %v", i, err)
		}
	}

	bad := map[string][]string{
		"bad driver":       {"--driver", "lxc"},
		"bad inference":    {"--inference", "openai"},
		"host with scheme": {"--mise-allow-host", "https://example.com"},
		"host with path":   {"--mise-allow-host", "example.com/x"},
		"host wildcard":    {"--mise-allow-host", "*.example.com"},
		"host bad port":    {"--mise-allow-host", "example.com:99999"},
		"host zero port":   {"--mise-allow-host", "example.com:0"},
		"host non-numeric": {"--mise-allow-host", "example.com:http"},
		"host empty":       {"--mise-allow-host", ""},
		"image whitespace": {"--image", "a b"},
	}
	for name, extra := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseArgs(append(append([]string{}, base...), extra...)); err == nil {
				t.Errorf("expected error for %v", extra)
			}
		})
	}
}

func TestShellHarnessRejected(t *testing.T) {
	_, err := ParseArgs([]string{"jlaska/agent-sandbox", "shell"})
	if err == nil || !strings.Contains(err.Error(), "shell harness was removed") {
		t.Errorf("err = %v", err)
	}
}
