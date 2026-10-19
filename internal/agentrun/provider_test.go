package agentrun

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// mockCmd records calls and returns a configurable error.
type mockCmd struct {
	calls      [][]string
	envs       [][]string // env passed with each execCmdEnv call (parallel to envCalls)
	envCalls   [][]string // argv of each execCmdEnv call
	err        error
	execOutput func(string, ...string) (string, error)
}

func (m *mockCmd) exec(_ context.Context, name string, args ...string) error {
	m.calls = append(m.calls, append([]string{name}, args...))
	return m.err
}

func (m *mockCmd) execEnv(_ context.Context, env []string, name string, args ...string) error {
	call := append([]string{name}, args...)
	m.calls = append(m.calls, call)
	m.envCalls = append(m.envCalls, call)
	m.envs = append(m.envs, env)
	return m.err
}

func (m *mockCmd) defaultExecOutput(_ context.Context, name string, args ...string) (string, error) {
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
	origEnv := execCmdEnvFn
	origInteractive := execInteractiveFn
	origTee := execCmdTeeFn
	defer func() {
		execCmdTeeFn = origTee
		execInteractiveFn = origInteractive
		execCmdEnvFn = origEnv
		execCmdFn = origCmd
		execCmdSilentFn = origSilent
		execCmdOutputFn = origOutput
	}()

	m := &mockCmd{}
	execCmdFn = m.exec
	execCmdSilentFn = m.exec
	execCmdOutputFn = m.defaultExecOutput
	execCmdEnvFn = m.execEnv
	execInteractiveFn = m.exec
	execCmdTeeFn = m.defaultExecOutput
	fn(m)
	assertNoSecretsInArgv(t, m)
}

// assertNoSecretsInArgv enforces the credential contract on every constructed
// command: values handed to a child via its environment never appear in any
// argv, and every --credential is the bare KEY form (no "=VALUE").
func assertNoSecretsInArgv(t *testing.T, m *mockCmd) {
	t.Helper()
	for _, env := range m.envs {
		for _, kv := range env {
			_, v, _ := strings.Cut(kv, "=")
			if v == "" {
				continue
			}
			for _, c := range m.calls {
				for _, a := range c {
					if a == v || strings.HasSuffix(a, "="+v) || (len(v) >= 8 && strings.Contains(a, v)) {
						t.Errorf("secret value for %q leaked into argv: %v", strings.SplitN(kv, "=", 2)[0], c)
					}
				}
			}
		}
	}
	for _, c := range m.calls {
		for i, a := range c {
			if a == "--credential" && i+1 < len(c) && strings.Contains(c[i+1], "=") {
				t.Errorf("--credential must be the bare KEY form, got %q in %v", c[i+1], c)
			}
		}
	}
}

func TestResolveInferenceEnv(t *testing.T) {
	t.Run("auto-forwards config vars from host", func(t *testing.T) {
		t.Setenv("ANTHROPIC_BASE_URL", "https://example.com")
		t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")

		flags := resolveInferenceEnv(HarnessClaude, HarnessOptions{})

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

		flags := resolveInferenceEnv(HarnessClaude, HarnessOptions{})

		for i := 0; i < len(flags)-1; i += 2 {
			if flags[i] == "--env" && strings.HasPrefix(flags[i+1], "ANTHROPIC_API_KEY=") {
				t.Error("ANTHROPIC_API_KEY should NOT be forwarded as --env (it goes through provider)")
			}
		}
	})

	t.Run("explicit credential env redirected to provider", func(t *testing.T) {
		flags := resolveInferenceEnv(HarnessClaude, HarnessOptions{ExplicitEnv: []string{"ANTHROPIC_API_KEY=sk-explicit"}})

		for i := 0; i < len(flags)-1; i += 2 {
			if flags[i] == "--env" && strings.HasPrefix(flags[i+1], "ANTHROPIC_API_KEY=") {
				t.Error("explicit ANTHROPIC_API_KEY should be redirected to provider, not --env")
			}
		}
	})

	t.Run("explicit config env forwarded", func(t *testing.T) {
		flags := resolveInferenceEnv(HarnessClaude, HarnessOptions{ExplicitEnv: []string{"ANTHROPIC_BASE_URL=https://custom.com"}})

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

		flags := resolveInferenceEnv(HarnessClaude, HarnessOptions{})
		if len(flags) != 0 {
			t.Errorf("expected no flags when no env vars set, got %v", flags)
		}
	})

	t.Run("pi forwards config vars only", func(t *testing.T) {
		t.Setenv("LITELLM_API_KEY", "sk-litellm")
		t.Setenv("LITELLM_BASE_URL", "https://litellm.example.com")

		flags := resolveInferenceEnv(HarnessPi, HarnessOptions{})

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

		flags := resolveInferenceEnv("bogus", HarnessOptions{})
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
			flags, names, err := createInferenceProviders(context.Background(), HarnessClaude, HarnessOptions{})
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

			t.Setenv("LITELLM_API_KEY", "sk-litellm")
			flags, names, err := createInferenceProviders(context.Background(), HarnessPi, HarnessOptions{})
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
			_, _, err := createInferenceProviders(context.Background(), HarnessPi, HarnessOptions{})
			if err == nil || !strings.Contains(err.Error(), "LITELLM_API_KEY") {
				t.Errorf("expected LITELLM_API_KEY error, got %v", err)
			}
		})
	})

	t.Run("unknown harness creates no providers", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			flags, names, err := createInferenceProviders(context.Background(), "bogus", HarnessOptions{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(names) != 0 || len(flags) != 0 {
				t.Errorf("expected no providers/flags, got %v / %v", names, flags)
			}
		})
	})
}

// No sandbox-side script may assign from a legacy stored-key name
// ($api_key, $api_token, $litellm_*): those no longer exist and expand empty.
func TestNoLegacyCredentialKeysInHooks(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "o")
	t.Setenv("LITELLM_API_KEY", "l")
	for name, h := range harnesses {
		withMockExec(t, func(mock *mockCmd) {
			o := HarnessOptions{Inference: "litellm", Backend: inferenceLiteLLM, ExplicitEnv: []string{"LITELLM_BASE_URL=https://x"}}
			_ = h.Preseed(context.Background(), "sb", o)
			mock.calls = append(mock.calls, []string{h.LaunchEnv(o)})
			for _, c := range mock.calls {
				joined := strings.Join(c, " ")
				for _, legacy := range []string{"$api_key", "$api_token", "$litellm_"} {
					if strings.Contains(joined, legacy) {
						t.Errorf("%s: legacy key %s in %s", name, legacy, joined)
					}
				}
			}
		})
	}
	if strings.Contains(buildLaunchCmd(harnesses["claude"], &Config{}, HarnessOptions{}), "api_token") {
		t.Error("launch command must not reference api_token")
	}
}

func TestClaudeCredential(t *testing.T) {
	cases := []struct {
		name         string
		env          map[string]string
		wantVal, src string
	}{
		{"api key first", map[string]string{"ANTHROPIC_API_KEY": "a", "CLAUDE_CODE_OAUTH_TOKEN": "o"}, "a", "ANTHROPIC_API_KEY"},
		{"oauth", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "o", "ANTHROPIC_AUTH_TOKEN": "t"}, "o", "CLAUDE_CODE_OAUTH_TOKEN"},
		{"auth token", map[string]string{"ANTHROPIC_AUTH_TOKEN": "t"}, "t", "ANTHROPIC_AUTH_TOKEN"},
		{"none", nil, "none", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
				t.Setenv(k, tt.env[k])
			}
			v, src := claudeCredential(nil)
			if v != tt.wantVal || src != tt.src {
				t.Errorf("got (%q, %q), want (%q, %q)", v, src, tt.wantVal, tt.src)
			}
		})
	}
}

// credentialEnv returns the KEY=VALUE env of the single execCmdEnv call whose
// argv contains the provider --type, plus the declared --credential keys.
func providerCreateCall(t *testing.T, m *mockCmd, typ string) (keys []string, env []string) {
	t.Helper()
	for i, c := range m.envCalls {
		if !strings.Contains(strings.Join(c, " "), "--type "+typ) {
			continue
		}
		for j, a := range c {
			if a == "--credential" {
				keys = append(keys, c[j+1])
			}
		}
		return keys, m.envs[i]
	}
	t.Fatalf("no provider create for type %s in %v", typ, m.calls)
	return nil, nil
}

func TestProviderCredentialKeysMatchProfiles(t *testing.T) {
	// OpenShell 0.1.4 accepts only the profile's declared env_vars as stored
	// keys (CredentialProfile::accepted_stored_keys).
	declared := func(id string) map[string]bool {
		data, err := profileData(id, profileFiles[id])
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Credentials []struct {
				Name    string   `yaml:"name"`
				EnvVars []string `yaml:"env_vars"`
			} `yaml:"credentials"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, c := range doc.Credentials {
			for _, e := range c.EnvVars {
				m[e] = true
			}
		}
		return m
	}

	t.Run("github-agent", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			if err := createProvider(context.Background(), GitHubProviderName, "github-agent", []credentialPair{
				{"GITHUB_TOKEN", "ghs_secret"}, {"GH_TOKEN", "ghs_secret"}}); err != nil {
				t.Fatal(err)
			}
			keys, env := providerCreateCall(t, m, "github-agent")
			if !slices.Equal(keys, []string{"GITHUB_TOKEN", "GH_TOKEN"}) || !slices.Equal(env, []string{"GITHUB_TOKEN=ghs_secret", "GH_TOKEN=ghs_secret"}) {
				t.Errorf("keys=%v env=%v", keys, env)
			}
			for _, k := range keys {
				if !declared("github-agent")[k] {
					t.Errorf("%s not declared by profile", k)
				}
			}
		})
	})

	t.Run("claude-code", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret")
		withMockExec(t, func(m *mockCmd) {
			if _, _, err := createInferenceProviders(context.Background(), HarnessClaude, HarnessOptions{}); err != nil {
				t.Fatal(err)
			}
			keys, env := providerCreateCall(t, m, "claude-code")
			if !slices.Equal(keys, []string{"ANTHROPIC_API_KEY"}) || !slices.Equal(env, []string{"ANTHROPIC_API_KEY=sk-ant-secret"}) {
				t.Errorf("keys=%v env=%v", keys, env)
			}
			if !declared("claude-code")[keys[0]] {
				t.Errorf("%s not declared by profile", keys[0])
			}
		})
	})

	t.Run("claude-code with oauth token stores it under ANTHROPIC_API_KEY", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-secret")
		withMockExec(t, func(m *mockCmd) {
			if _, _, err := createInferenceProviders(context.Background(), HarnessClaude, HarnessOptions{}); err != nil {
				t.Fatal(err)
			}
			keys, env := providerCreateCall(t, m, "claude-code")
			if !slices.Equal(keys, []string{"ANTHROPIC_API_KEY"}) || !slices.Equal(env, []string{"ANTHROPIC_API_KEY=oauth-secret"}) {
				t.Errorf("keys=%v env=%v", keys, env)
			}
		})
	})

	t.Run("litellm-inference", func(t *testing.T) {
		t.Setenv("LITELLM_API_KEY", "sk-l-secret")
		withMockExec(t, func(m *mockCmd) {
			if _, _, err := createInferenceProviders(context.Background(), HarnessPi, HarnessOptions{}); err != nil {
				t.Fatal(err)
			}
			keys, env := providerCreateCall(t, m, "litellm-inference")
			if !slices.Equal(keys, []string{"LITELLM_API_KEY"}) || !slices.Equal(env, []string{"LITELLM_API_KEY=sk-l-secret"}) {
				t.Errorf("keys=%v env=%v", keys, env)
			}
			if !declared("litellm-inference")[keys[0]] {
				t.Errorf("%s not declared by profile", keys[0])
			}
		})
	})

	t.Run("anthropic", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			t.Setenv("ANTHROPIC_API_KEY", "sk-a-secret")
			t.Setenv("LITELLM_API_KEY", "")
			if _, _, err := createInferenceProviders(context.Background(), HarnessOpencode, HarnessOptions{Backend: inferenceAnthropic}); err != nil {
				t.Fatal(err)
			}
			keys, _ := providerCreateCall(t, m, "anthropic")
			if !slices.Equal(keys, []string{"ANTHROPIC_API_KEY"}) || !declared("anthropic")[keys[0]] {
				t.Errorf("keys=%v", keys)
			}
		})
	})

	t.Run("create failure is reported", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "x")
		withMockExec(t, func(m *mockCmd) {
			m.err = errors.New("boom")
			if _, _, err := createInferenceProviders(context.Background(), HarnessClaude, HarnessOptions{}); err == nil {
				t.Error("expected error")
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

func TestDeleteProvider(t *testing.T) {
	origT, origP := sandboxGoneTimeout, sandboxGonePoll
	sandboxGoneTimeout, sandboxGonePoll = 50*time.Millisecond, time.Millisecond
	defer func() { sandboxGoneTimeout, sandboxGonePoll = origT, origP }()
	attached := "! Failed to delete provider p: provider 'p' is attached to sandbox(es): sb\nError: × failed to delete 1 provider: p"

	t.Run("refused while attached, then deleted", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			n := 0
			m.execOutput = func(string, ...string) (string, error) {
				if n++; n < 3 {
					return attached, errors.New("exit status 1")
				}
				return "✓ Deleted provider p", nil
			}
			if err := deleteProvider(context.Background(), "p"); err != nil || n != 3 {
				t.Errorf("err=%v attempts=%d", err, n)
			}
		})
	})
	t.Run("still attached at the deadline", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = func(string, ...string) (string, error) { return attached, errors.New("exit status 1") }
			err := deleteProvider(context.Background(), "p")
			if err == nil || !strings.Contains(err.Error(), "is attached to sandbox(es): sb") {
				t.Errorf("err = %v", err)
			}
		})
	})
	t.Run("other failure is not retried", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = func(string, ...string) (string, error) {
				return "Error: × provider not found", errors.New("exit status 1")
			}
			err := deleteProvider(context.Background(), "p")
			if err == nil || !strings.Contains(err.Error(), "provider not found") || len(m.calls) != 1 {
				t.Errorf("err=%v calls=%d", err, len(m.calls))
			}
		})
	})
}
