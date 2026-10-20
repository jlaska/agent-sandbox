package agentrun

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

func clearOpencodeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"LITELLM_API_KEY", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "LITELLM_BASE_URL"} {
		t.Setenv(k, "")
	}
}

func TestResolveOpencodeBackend(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		inference string
		want      string
		wantErr   []string
	}{
		{"litellm only", map[string]string{"LITELLM_API_KEY": "l"}, "", "litellm", nil},
		{"anthropic only", map[string]string{"ANTHROPIC_API_KEY": "a"}, "", "anthropic", nil},
		{"both defaults to litellm", map[string]string{"LITELLM_API_KEY": "l", "ANTHROPIC_API_KEY": "a"}, "", "litellm", nil},
		{"both, explicit anthropic wins", map[string]string{"LITELLM_API_KEY": "l", "ANTHROPIC_API_KEY": "a"}, "anthropic", "anthropic", nil},
		{"both, explicit litellm", map[string]string{"LITELLM_API_KEY": "l", "ANTHROPIC_API_KEY": "a"}, "litellm", "litellm", nil},
		{"oauth only", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "o"}, "", "", []string{"needs an API key", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}},
		{"oauth only, explicit anthropic", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "o"}, "anthropic", "", []string{"needs an API key", "CLAUDE_CODE_OAUTH_TOKEN"}},
		{"oauth plus litellm", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "o", "LITELLM_API_KEY": "l"}, "", "litellm", nil},
		{"none", nil, "", "", []string{"LITELLM_API_KEY", "ANTHROPIC_API_KEY"}},
		{"explicit litellm without key", map[string]string{"ANTHROPIC_API_KEY": "a"}, "litellm", "", []string{"--inference litellm requires LITELLM_API_KEY"}},
		{"explicit anthropic without key", map[string]string{"LITELLM_API_KEY": "l"}, "anthropic", "", []string{"--inference anthropic requires ANTHROPIC_API_KEY"}},
		{"invalid", map[string]string{"LITELLM_API_KEY": "l"}, "bogus", "", []string{`invalid --inference "bogus"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearOpencodeEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := resolveOpencodeBackend(tt.inference, nil)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("got %q, want error", got)
				}
				for _, w := range tt.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("err = %q, want containing %q", err, w)
					}
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("got (%q, %v), want %q", got, err, tt.want)
			}
		})
	}

	t.Run("explicit --env credentials count", func(t *testing.T) {
		clearOpencodeEnv(t)
		got, err := resolveOpencodeBackend("", []string{"ANTHROPIC_API_KEY=a"})
		if err != nil || got != "anthropic" {
			t.Errorf("got (%q, %v)", got, err)
		}
	})
}

// opencodeOpts resolves run options the way Run does.
func opencodeOpts(t *testing.T, cfg *Config) HarnessOptions {
	t.Helper()
	o, err := resolveHarnessOptions(HarnessOpencode, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestOpencodeHarnessLaunch(t *testing.T) {
	h, err := LookupHarness("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if h.ImageKey != "opencode" || h.Command != "opencode" || h.ModelFlag != "--model" {
		t.Errorf("unexpected harness: %+v", h)
	}
	got := buildLaunchCmd(h, &Config{Model: "anthropic/claude-sonnet-4-5", PassthroughArgs: []string{"--agent", "build"}}, HarnessOptions{Backend: inferenceAnthropic})
	want := "source ~/.profile 2>/dev/null; exec opencode --model 'anthropic/claude-sonnet-4-5' '--agent' 'build'"
	if got != want {
		t.Errorf("launch cmd\n got: %s\nwant: %s", got, want)
	}
}

func TestOpencodeProviders(t *testing.T) {
	h := harnesses["opencode"]
	clearOpencodeEnv(t)
	t.Setenv("LITELLM_API_KEY", "l")
	t.Setenv("ANTHROPIC_API_KEY", "a")
	for inf, want := range map[string]string{"": "litellm-inference", "litellm": "litellm-inference", "anthropic": "anthropic"} {
		got := h.Providers(opencodeOpts(t, &Config{Inference: inf}))
		if len(got) != 1 || got[0] != want {
			t.Errorf("inference %q: got %v, want [%s]", inf, got, want)
		}
	}
	t.Setenv("LITELLM_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := resolveHarnessOptions(HarnessOpencode, &Config{}); err == nil {
		t.Error("expected error without credentials")
	}
	// Other harnesses are unaffected by options and need no backend.
	o, err := resolveHarnessOptions(HarnessPi, &Config{Inference: "anthropic"})
	if err != nil || o.Backend != "" {
		t.Errorf("pi: o=%+v err=%v", o, err)
	}
	if got := harnesses["pi"].Providers(o); len(got) != 1 || got[0] != "litellm-inference" {
		t.Errorf("pi: got %v", got)
	}
}

// The backend is resolved once: later steps read it from the options and no
// longer consult the environment, so a change mid-run cannot split the run
// across two backends.
func TestOpencodeBackendResolvedOnce(t *testing.T) {
	clearOpencodeEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "a")
	o := opencodeOpts(t, &Config{})
	t.Setenv("LITELLM_API_KEY", "l") // would now win if re-resolved
	t.Setenv("LITELLM_BASE_URL", "https://litellm.example.com")
	if got := harnesses["opencode"].Providers(o); len(got) != 1 || got[0] != "anthropic" {
		t.Errorf("providers = %v", got)
	}
	withMockExec(t, func(mock *mockCmd) {
		_, names, err := createInferenceProviders(context.Background(), "s1", HarnessOpencode, o)
		if err != nil || len(names) != 1 || names[0] != "s1-anthropic" {
			t.Errorf("names=%v err=%v", names, err)
		}
	})
}

func TestCreateInferenceProvidersOpencode(t *testing.T) {
	cases := []struct {
		name, inference string
		env             map[string]string
		wantName        string
		wantCred        string
	}{
		{"litellm", "", map[string]string{"LITELLM_API_KEY": "sk-l"}, "litellm-inference", "LITELLM_API_KEY"},
		{"anthropic", "", map[string]string{"ANTHROPIC_API_KEY": "sk-a"}, "anthropic", "ANTHROPIC_API_KEY"},
		{"explicit anthropic", "anthropic", map[string]string{"ANTHROPIC_API_KEY": "sk-a", "LITELLM_API_KEY": "sk-l"}, "anthropic", "ANTHROPIC_API_KEY"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			clearOpencodeEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			withMockExec(t, func(mock *mockCmd) {
				flags, names, err := createInferenceProviders(context.Background(), "s1", HarnessOpencode, opencodeOpts(t, &Config{Inference: tt.inference}))
				if err != nil {
					t.Fatal(err)
				}
				if want := "s1-" + tt.wantName; len(names) != 1 || names[0] != want || len(flags) != 2 || flags[1] != want {
					t.Errorf("flags=%v names=%v", flags, names)
				}
				keys, env := providerCreateCall(t, mock, tt.wantName)
				if !slices.Equal(keys, []string{tt.wantCred}) || len(env) != 1 || !strings.HasPrefix(env[0], tt.wantCred+"=sk-") {
					t.Errorf("keys = %v env-keys = %d", keys, len(env))
				}
			})
		})
	}

	t.Run("oauth only", func(t *testing.T) {
		clearOpencodeEnv(t)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "o")
		if _, err := resolveHarnessOptions(HarnessOpencode, &Config{}); err == nil || !strings.Contains(err.Error(), "needs an API key") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("create failure", func(t *testing.T) {
		clearOpencodeEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "a")
		o := opencodeOpts(t, &Config{})
		withMockExec(t, func(mock *mockCmd) {
			mock.err = os.ErrPermission
			if _, _, err := createInferenceProviders(context.Background(), "s1", HarnessOpencode, o); err == nil {
				t.Error("expected error")
			}
		})
	})
}

func TestResolveInferenceEnvOpencode(t *testing.T) {
	clearOpencodeEnv(t)
	t.Setenv("LITELLM_BASE_URL", "https://litellm.example.com")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-forward")
	flags := strings.Join(resolveInferenceEnv(HarnessOpencode, opencodeOpts(t, &Config{})), " ")
	if !strings.Contains(flags, "LITELLM_BASE_URL=https://litellm.example.com") || strings.Contains(flags, "must-not-forward") {
		t.Errorf("flags = %q", flags)
	}
}

// An explicit --env CLAUDE_CODE_OAUTH_TOKEN for opencode is dropped (it is a
// secret, and opencode cannot use it), and the NOTE says so instead of
// claiming a provider injects it.
func TestResolveInferenceEnvOpencodeDropsOAuth(t *testing.T) {
	clearOpencodeEnv(t)
	o := opencodeOpts(t, &Config{EnvVars: []string{"ANTHROPIC_API_KEY=k", "CLAUDE_CODE_OAUTH_TOKEN=t"}})
	var flags []string
	out := captureStdout(t, func() { flags = resolveInferenceEnv(HarnessOpencode, o) })
	if strings.Contains(strings.Join(flags, " "), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("OAuth token forwarded: %v", flags)
	}
	if !strings.Contains(out, "NOTE: ANTHROPIC_API_KEY will be injected via provider credential") {
		t.Errorf("missing injected NOTE: %q", out)
	}
	if !strings.Contains(out, "NOTE: CLAUDE_CODE_OAUTH_TOKEN is not used by opencode in this run and is not forwarded") ||
		strings.Contains(out, "CLAUDE_CODE_OAUTH_TOKEN will be injected") {
		t.Errorf("misleading NOTE: %q", out)
	}
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "opencode", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Errorf("%s mismatch (run go test -update)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestOpencodeConfigGolden(t *testing.T) {
	t.Run("litellm", func(t *testing.T) {
		got, err := opencodeConfig("litellm", "https://litellm.example.com/v1/", "litellm/claude-sonnet-4-5")
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, "preseed-litellm.json", got)
	})
	t.Run("anthropic", func(t *testing.T) {
		got, err := opencodeConfig("anthropic", "", "")
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, "preseed-anthropic.json", got)
	})
	t.Run("unknown backend", func(t *testing.T) {
		if _, err := opencodeConfig("x", "", ""); err == nil {
			t.Error("expected error")
		}
	})
}

// The preseed overwrites the file the image bakes; it must keep those keys.
func TestOpencodeConfigKeepsImageBakedKeys(t *testing.T) {
	df, err := os.ReadFile("../../openshell/images/opencode/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(df), `"autoupdate": false`) {
		t.Fatal("Dockerfile no longer bakes autoupdate=false; update opencodeConfig")
	}
	for _, backend := range []string{"litellm", "anthropic"} {
		got, _ := opencodeConfig(backend, "https://x", "")
		for _, k := range []string{`"$schema": "https://opencode.ai/config.json"`, `"autoupdate": false`} {
			if !strings.Contains(string(got), k) {
				t.Errorf("%s config lacks %s", backend, k)
			}
		}
	}
}

func TestOpencodePreseed(t *testing.T) {
	h := harnesses["opencode"]
	t.Run("litellm writes config", func(t *testing.T) {
		clearOpencodeEnv(t)
		t.Setenv("LITELLM_API_KEY", "l")
		t.Setenv("LITELLM_BASE_URL", "https://litellm.example.com")
		o := opencodeOpts(t, &Config{Model: "litellm/m"})
		withMockExec(t, func(mock *mockCmd) {
			if err := h.Preseed(context.Background(), "sb", o); err != nil {
				t.Fatal(err)
			}
			cmd := strings.Join(mock.calls[0], " ")
			for _, w := range []string{"-n sb", "~/.config/opencode/opencode.json", "@ai-sdk/openai-compatible", "https://litellm.example.com", `"m"`} {
				if !strings.Contains(cmd, w) {
					t.Errorf("command lacks %q: %s", w, cmd)
				}
			}
		})
	})
	t.Run("litellm needs base url", func(t *testing.T) {
		clearOpencodeEnv(t)
		t.Setenv("LITELLM_API_KEY", "l")
		o := opencodeOpts(t, &Config{})
		withMockExec(t, func(mock *mockCmd) {
			err := h.Preseed(context.Background(), "sb", o)
			if err == nil || !strings.Contains(err.Error(), "LITELLM_BASE_URL") || len(mock.calls) != 0 {
				t.Errorf("err = %v calls = %v", err, mock.calls)
			}
		})
	})
	t.Run("anthropic writes config", func(t *testing.T) {
		clearOpencodeEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "a")
		o := opencodeOpts(t, &Config{Inference: "anthropic"})
		withMockExec(t, func(mock *mockCmd) {
			if err := h.Preseed(context.Background(), "sb", o); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(mock.calls[0], " "), `"enabled_providers"`) {
				t.Errorf("calls = %v", mock.calls)
			}
		})
	})
	t.Run("unresolved backend is an error", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			if err := h.Preseed(context.Background(), "sb", HarnessOptions{}); err == nil || len(mock.calls) != 0 {
				t.Errorf("err = %v calls = %v", err, mock.calls)
			}
		})
	})
}
