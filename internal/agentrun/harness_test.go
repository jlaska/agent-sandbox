package agentrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jlaska/agent-sandbox/openshell"
)

func TestLookupHarness(t *testing.T) {
	tests := []struct {
		name        string
		harness     string
		wantErr     string
		wantCommand string
	}{
		{"claude", "claude", "", "claude"},
		{"pi", "pi", "", "pi"},
		{"opencode", "opencode", "", "opencode"},
		{"shell removed", "shell", "shell harness was removed", ""},
		{"unknown", "nope", `unknown harness "nope"`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := LookupHarness(tt.harness)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h.Command != tt.wantCommand || h.Name != tt.harness {
				t.Errorf("got %+v", h)
			}
		})
	}
}

func TestHarnessRegistry(t *testing.T) {
	if got := strings.Join(HarnessNames(), ","); got != "claude,opencode,pi" {
		t.Errorf("HarnessNames() = %q", got)
	}
	for _, name := range HarnessNames() {
		h := harnesses[name]
		for _, o := range []HarnessOptions{{Backend: inferenceLiteLLM}, {Backend: inferenceAnthropic}} {
			if h.ImageKey == "" || h.Command == "" || len(h.Providers(o)) == 0 {
				t.Errorf("harness %s incomplete: %+v", name, h)
			}
			for _, id := range h.Providers(o) {
				path, ok := profileFiles[id]
				if !ok {
					t.Errorf("%s: provider profile %q has no embedded file mapping", name, id)
					continue
				}
				if _, err := openshell.ProviderProfiles.ReadFile(path); err != nil {
					t.Errorf("%s: embedded profile %s: %v", name, path, err)
				}
			}
		}
	}
}

// A harness without a hook is a no-op, never a nil call.
func TestHarnessNilHooks(t *testing.T) {
	h := &Harness{Name: "bare", Command: "bare"}
	withMockExec(t, func(m *mockCmd) {
		if err := h.Preseed(context.Background(), "sb", HarnessOptions{}); err != nil || len(m.calls) != 0 {
			t.Errorf("err=%v calls=%v", err, m.calls)
		}
	})
	if h.Providers(HarnessOptions{}) != nil || h.LaunchEnv(HarnessOptions{}) != "" {
		t.Error("nil hooks must yield no providers and no launch env")
	}
}

func TestPreseed(t *testing.T) {
	t.Run("claude writes ~/.claude.json", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			if err := harnesses["claude"].Preseed(context.Background(), "test-sb", HarnessOptions{}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(mock.calls) != 1 || !strings.Contains(strings.Join(mock.calls[0], " "), "hasTrustDialogAccepted") {
				t.Errorf("unexpected calls: %v", mock.calls)
			}
		})
	})

	t.Run("pi is noop", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			if err := harnesses["pi"].Preseed(context.Background(), "test-sb", HarnessOptions{}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(mock.calls) != 0 {
				t.Errorf("expected 0 calls, got %v", mock.calls)
			}
		})
	})
}

// The OAuth rename happens in the launch shell only, after ~/.profile, and no
// dotfile is edited, so it applies exactly once whatever the dotfiles source.
func TestClaudeOAuthLaunchEnv(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		t.Setenv(k, "")
	}
	h := harnesses[HarnessClaude]

	t.Run("OAuth token is renamed at launch", func(t *testing.T) {
		o := HarnessOptions{ExplicitEnv: []string{"CLAUDE_CODE_OAUTH_TOKEN=tok"}}
		got := buildLaunchCmd(h, &Config{}, o)
		want := `source ~/.profile 2>/dev/null; export CLAUDE_CODE_OAUTH_TOKEN="$ANTHROPIC_API_KEY"; unset ANTHROPIC_API_KEY; exec claude `
		if got != want {
			t.Errorf("launch cmd =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("API key needs no rename", func(t *testing.T) {
		o := HarnessOptions{ExplicitEnv: []string{"ANTHROPIC_API_KEY=k", "CLAUDE_CODE_OAUTH_TOKEN=tok"}}
		if got := buildLaunchCmd(h, &Config{}, o); strings.Contains(got, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Errorf("launch cmd = %q", got)
		}
	})

	t.Run("no sandbox-side setup step and no dotfile writes", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			o := HarnessOptions{ExplicitEnv: []string{"CLAUDE_CODE_OAUTH_TOKEN=tok"}}
			if err := h.Preseed(context.Background(), "sb", o); err != nil {
				t.Fatal(err)
			}
			for _, c := range callStrings(m) {
				if strings.Contains(c, ".bashrc") || strings.Contains(c, ".profile") {
					t.Errorf("dotfile edited: %s", c)
				}
			}
		})
	})

	// Run the generated launch line in a real shell whose ~/.profile sources
	// ~/.bashrc (the base image layout that ran the old alias twice): the
	// token must survive.
	t.Run("token survives a profile that sources bashrc", func(t *testing.T) {
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("PS1=x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(". ~/.bashrc\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		probe := &Harness{Name: "probe", Command: "sh", launchEnv: claudeLaunchEnv}
		cfg := &Config{PassthroughArgs: []string{"-c", `printf '%s|%s' "$CLAUDE_CODE_OAUTH_TOKEN" "${ANTHROPIC_API_KEY-unset}"`}}
		line := buildLaunchCmd(probe, cfg, HarnessOptions{ExplicitEnv: []string{"CLAUDE_CODE_OAUTH_TOKEN=tok"}})
		cmd := exec.Command("bash", "-c", line)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=placeholder-1"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != "placeholder-1|unset" {
			t.Errorf("got %q, want the placeholder under CLAUDE_CODE_OAUTH_TOKEN and ANTHROPIC_API_KEY unset", out)
		}
	})
}
