package agentrun

import (
	"strings"
	"testing"
)

// mockCmd records calls and returns a configurable error.
type mockCmd struct {
	calls      [][]string
	err        error
	execOutput func(string, ...string) (string, error)
}

func (m *mockCmd) exec(name string, args ...string) error {
	m.calls = append(m.calls, append([]string{name}, args...))
	return m.err
}

func (m *mockCmd) defaultExecOutput(name string, args ...string) (string, error) {
	m.calls = append(m.calls, append([]string{name}, args...))
	if m.execOutput != nil {
		return m.execOutput(name, args...)
	}
	return "", m.err
}

func withMockExec(t *testing.T, fn func(mock *mockCmd)) {
	t.Helper()
	origCmd := execCmdFn
	origSilent := execCmdSilentFn
	origOutput := execCmdOutputFn
	defer func() {
		execCmdFn = origCmd
		execCmdSilentFn = origSilent
		execCmdOutputFn = origOutput
	}()

	m := &mockCmd{}
	execCmdFn = m.exec
	execCmdSilentFn = m.exec
	execCmdOutputFn = m.defaultExecOutput
	fn(m)
}

func TestResolveInferenceEnv(t *testing.T) {
	t.Run("auto-forwards config vars from host", func(t *testing.T) {
		t.Setenv("ANTHROPIC_BASE_URL", "https://example.com")
		t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")

		flags := resolveInferenceEnv(HarnessClaude, nil)

		found := map[string]bool{}
		for i := 0; i < len(flags)-1; i += 2 {
			if flags[i] == "--env" {
				k, _, _ := strings.Cut(flags[i+1], "=")
				found[k] = true
			}
		}
		if !found["ANTHROPIC_BASE_URL"] {
			t.Error("expected ANTHROPIC_BASE_URL to be forwarded")
		}
		if !found["CLAUDE_CODE_USE_VERTEX"] {
			t.Error("expected CLAUDE_CODE_USE_VERTEX to be forwarded")
		}
	})

	t.Run("credential vars excluded from env forwarding", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
		t.Setenv("ANTHROPIC_BASE_URL", "https://example.com")

		flags := resolveInferenceEnv(HarnessClaude, nil)

		for i := 0; i < len(flags)-1; i += 2 {
			if flags[i] == "--env" && strings.HasPrefix(flags[i+1], "ANTHROPIC_API_KEY=") {
				t.Error("ANTHROPIC_API_KEY should NOT be forwarded as --env (it goes through provider)")
			}
		}
	})

	t.Run("explicit credential env redirected to provider", func(t *testing.T) {
		flags := resolveInferenceEnv(HarnessClaude, []string{"ANTHROPIC_API_KEY=sk-explicit"})

		for i := 0; i < len(flags)-1; i += 2 {
			if flags[i] == "--env" && strings.HasPrefix(flags[i+1], "ANTHROPIC_API_KEY=") {
				t.Error("explicit ANTHROPIC_API_KEY should be redirected to provider, not --env")
			}
		}
	})

	t.Run("explicit config env forwarded", func(t *testing.T) {
		flags := resolveInferenceEnv(HarnessClaude, []string{"ANTHROPIC_BASE_URL=https://custom.com"})

		found := false
		for i := 0; i < len(flags)-1; i += 2 {
			if flags[i] == "--env" && flags[i+1] == "ANTHROPIC_BASE_URL=https://custom.com" {
				found = true
			}
		}
		if !found {
			t.Error("explicit config env should be forwarded")
		}
	})

	t.Run("unset host vars not forwarded", func(t *testing.T) {
		for _, key := range inferenceConfigVars[HarnessClaude] {
			t.Setenv(key, "")
		}

		flags := resolveInferenceEnv(HarnessClaude, nil)
		if len(flags) != 0 {
			t.Errorf("expected no flags when no env vars set, got %v", flags)
		}
	})

	t.Run("pi forwards config vars only", func(t *testing.T) {
		t.Setenv("LITELLM_API_KEY", "sk-litellm")
		t.Setenv("LITELLM_BASE_URL", "https://litellm.example.com")

		flags := resolveInferenceEnv(HarnessPi, nil)

		foundBaseURL := false
		foundAPIKey := false
		for i := 0; i < len(flags)-1; i += 2 {
			if flags[i] == "--env" {
				k, _, _ := strings.Cut(flags[i+1], "=")
				if k == "LITELLM_BASE_URL" {
					foundBaseURL = true
				}
				if k == "LITELLM_API_KEY" {
					foundAPIKey = true
				}
			}
		}
		if !foundBaseURL {
			t.Error("expected LITELLM_BASE_URL forwarded for pi")
		}
		if foundAPIKey {
			t.Error("LITELLM_API_KEY should NOT be forwarded as --env for pi")
		}
	})

	t.Run("unknown harness forwards nothing", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-test")

		flags := resolveInferenceEnv("bogus", nil)
		if len(flags) != 0 {
			t.Errorf("expected no auto-forward for unknown harness, got %v", flags)
		}
	})
}

func TestResolveCredential(t *testing.T) {
	t.Run("from explicit env", func(t *testing.T) {
		val := resolveCredential("MY_KEY", []string{"MY_KEY=explicit"})
		if val != "explicit" {
			t.Errorf("got %q, want explicit", val)
		}
	})

	t.Run("from host env", func(t *testing.T) {
		t.Setenv("MY_KEY", "from-host")
		val := resolveCredential("MY_KEY", nil)
		if val != "from-host" {
			t.Errorf("got %q, want from-host", val)
		}
	})

	t.Run("explicit wins over host", func(t *testing.T) {
		t.Setenv("MY_KEY", "from-host")
		val := resolveCredential("MY_KEY", []string{"MY_KEY=explicit"})
		if val != "explicit" {
			t.Errorf("got %q, want explicit", val)
		}
	})

	t.Run("missing returns empty", func(t *testing.T) {
		val := resolveCredential("NONEXISTENT_KEY", nil)
		if val != "" {
			t.Errorf("got %q, want empty", val)
		}
	})
}

func TestCreateInferenceProviders(t *testing.T) {
	t.Run("claude creates claude-code provider", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			t.Setenv("ANTHROPIC_API_KEY", "sk-test")
			flags, names, err := createInferenceProviders(HarnessClaude, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(names) != 1 || names[0] != "claude-code" {
				t.Errorf("names = %v, want [claude-code]", names)
			}
			if len(flags) != 2 || flags[0] != "--provider" || flags[1] != "claude-code" {
				t.Errorf("flags = %v, want [--provider claude-code]", flags)
			}
		})
	})

	t.Run("pi creates litellm-inference provider", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			mock.execOutput = func(name string, args ...string) (string, error) {
				if name == "openshell" && len(args) > 0 && args[0] == "provider" {
					return "litellm-inference\n", nil
				}
				return "", nil
			}
			execCmdOutputFn = mock.execOutput

			t.Setenv("LITELLM_API_KEY", "sk-litellm")
			flags, names, err := createInferenceProviders(HarnessPi, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(names) != 1 || names[0] != "litellm-inference" {
				t.Errorf("names = %v, want [litellm-inference]", names)
			}
			if len(flags) != 2 || flags[0] != "--provider" || flags[1] != "litellm-inference" {
				t.Errorf("flags = %v, want [--provider litellm-inference]", flags)
			}
		})
	})

	t.Run("pi errors without LITELLM_API_KEY", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			t.Setenv("LITELLM_API_KEY", "")
			_, _, err := createInferenceProviders(HarnessPi, nil)
			if err == nil || !strings.Contains(err.Error(), "LITELLM_API_KEY") {
				t.Errorf("expected LITELLM_API_KEY error, got %v", err)
			}
		})
	})

	t.Run("unknown harness creates no providers", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			flags, names, err := createInferenceProviders("bogus", nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(names) != 0 || len(flags) != 0 {
				t.Errorf("expected no providers/flags, got %v / %v", names, flags)
			}
		})
	})
}

func TestRunSandboxInitHook(t *testing.T) {
	t.Run("claude maps ANTHROPIC_API_KEY", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			err := harnesses["claude"].InitHook()("test-sb")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(mock.calls) != 1 {
				t.Fatalf("expected 1 call, got %d", len(mock.calls))
			}
			cmd := strings.Join(mock.calls[0], " ")
			if !strings.Contains(cmd, "ANTHROPIC_API_KEY") {
				t.Error("expected ANTHROPIC_API_KEY in init hook")
			}
		})
	})

	t.Run("claude maps CLAUDE_CODE_OAUTH_TOKEN when set", func(t *testing.T) {
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-token")
		withMockExec(t, func(mock *mockCmd) {
			err := harnesses["claude"].InitHook()("test-sb")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			cmd := strings.Join(mock.calls[0], " ")
			if !strings.Contains(cmd, "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Error("expected CLAUDE_CODE_OAUTH_TOKEN in init hook")
			}
		})
	})

	t.Run("pi maps LITELLM_API_KEY", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			err := harnesses["pi"].InitHook()("test-sb")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			cmd := strings.Join(mock.calls[0], " ")
			if !strings.Contains(cmd, "LITELLM_API_KEY") {
				t.Error("expected LITELLM_API_KEY in init hook")
			}
		})
	})
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "'hello'"},
		{"fix the bug", "'fix the bug'"},
		{"it's a test", `'it'\''s a test'`},
		{"", "''"},
		{"--model=opus", "'--model=opus'"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := shellQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFindRepoRoot(t *testing.T) {
	t.Run("finds openshell directory", func(t *testing.T) {
		root := findRepoRoot()
		if root == "" {
			t.Error("findRepoRoot returned empty")
		}
	})
}
