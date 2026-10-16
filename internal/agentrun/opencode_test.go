package agentrun

import (
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

func TestOpencodeHarnessLaunch(t *testing.T) {
	h, err := LookupHarness("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if h.ImageKey != "opencode" || h.Command != "opencode" || h.ModelFlag != "--model" {
		t.Errorf("unexpected harness: %+v", h)
	}
	got := buildLaunchCmd(h, &Config{Model: "anthropic/claude-sonnet-4-5", PassthroughArgs: []string{"--agent", "build"}})
	want := "source ~/.profile 2>/dev/null; exec opencode --model 'anthropic/claude-sonnet-4-5' '--agent' 'build'"
	if got != want {
		t.Errorf("launch cmd\n got: %s\nwant: %s", got, want)
	}
}

func TestOpencodeProvidersFor(t *testing.T) {
	h := harnesses["opencode"]
	clearOpencodeEnv(t)
	t.Setenv("LITELLM_API_KEY", "l")
	t.Setenv("ANTHROPIC_API_KEY", "a")
	for inf, want := range map[string]string{"": "litellm-inference", "litellm": "litellm-inference", "anthropic": "anthropic"} {
		got, err := h.ProvidersFor(HarnessOptions{Inference: inf})
		if err != nil || len(got) != 1 || got[0] != want {
			t.Errorf("inference %q: got (%v, %v), want [%s]", inf, got, err, want)
		}
	}
	t.Setenv("LITELLM_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := h.ProvidersFor(HarnessOptions{}); err == nil {
		t.Error("expected error without credentials")
	}
	// Other harnesses are unaffected by options.
	if got, err := harnesses["pi"].ProvidersFor(HarnessOptions{Inference: "anthropic"}); err != nil || len(got) != 1 || got[0] != "litellm-inference" {
		t.Errorf("pi: got (%v, %v)", got, err)
	}
}

func TestOpencodeInitHook(t *testing.T) {
	// The providers store ANTHROPIC_API_KEY / LITELLM_API_KEY directly, so the
	// hook only validates the backend; it must not touch the sandbox.
	for _, inference := range []string{"litellm", "anthropic"} {
		t.Run(inference, func(t *testing.T) {
			clearOpencodeEnv(t)
			t.Setenv("LITELLM_API_KEY", "l")
			t.Setenv("ANTHROPIC_API_KEY", "a")
			withMockExec(t, func(mock *mockCmd) {
				if err := harnesses["opencode"].InitHookFor(HarnessOptions{Inference: inference})("sb"); err != nil {
					t.Fatal(err)
				}
				if len(mock.calls) != 0 {
					t.Errorf("calls = %v, want none", mock.calls)
				}
			})
		})
	}
	t.Run("no credentials errors", func(t *testing.T) {
		clearOpencodeEnv(t)
		withMockExec(t, func(mock *mockCmd) {
			if err := harnesses["opencode"].InitHook()("sb"); err == nil || len(mock.calls) != 0 {
				t.Errorf("err = %v, calls = %v", err, mock.calls)
			}
		})
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
				flags, names, err := createInferenceProvidersFor(HarnessOpencode, tt.inference, nil)
				if err != nil {
					t.Fatal(err)
				}
				if len(names) != 1 || names[0] != tt.wantName || len(flags) != 2 || flags[1] != tt.wantName {
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
		withMockExec(t, func(mock *mockCmd) {
			_, _, err := createInferenceProviders(HarnessOpencode, nil)
			if err == nil || !strings.Contains(err.Error(), "needs an API key") || len(mock.calls) != 0 {
				t.Errorf("err = %v, calls = %v", err, mock.calls)
			}
		})
	})

	t.Run("create failure", func(t *testing.T) {
		clearOpencodeEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "a")
		withMockExec(t, func(mock *mockCmd) {
			mock.err = os.ErrPermission
			if _, _, err := createInferenceProviders(HarnessOpencode, nil); err == nil {
				t.Error("expected error")
			}
		})
	})
}

func TestResolveInferenceEnvOpencode(t *testing.T) {
	clearOpencodeEnv(t)
	t.Setenv("LITELLM_BASE_URL", "https://litellm.example.com")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-forward")
	flags := strings.Join(resolveInferenceEnv(HarnessOpencode, nil), " ")
	if !strings.Contains(flags, "LITELLM_BASE_URL=https://litellm.example.com") || strings.Contains(flags, "must-not-forward") {
		t.Errorf("flags = %q", flags)
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
		withMockExec(t, func(mock *mockCmd) {
			if err := h.PreseedFor(HarnessOptions{Model: "litellm/m"})("sb"); err != nil {
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
		withMockExec(t, func(mock *mockCmd) {
			err := h.Preseed()("sb")
			if err == nil || !strings.Contains(err.Error(), "LITELLM_BASE_URL") || len(mock.calls) != 0 {
				t.Errorf("err = %v calls = %v", err, mock.calls)
			}
		})
	})
	t.Run("anthropic writes config", func(t *testing.T) {
		clearOpencodeEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "a")
		withMockExec(t, func(mock *mockCmd) {
			if err := h.PreseedFor(HarnessOptions{Inference: "anthropic"})("sb"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(mock.calls[0], " "), `"enabled_providers"`) {
				t.Errorf("calls = %v", mock.calls)
			}
		})
	})
	t.Run("no credentials", func(t *testing.T) {
		clearOpencodeEnv(t)
		withMockExec(t, func(mock *mockCmd) {
			if err := h.Preseed()("sb"); err == nil || len(mock.calls) != 0 {
				t.Errorf("err = %v calls = %v", err, mock.calls)
			}
		})
	})
}

func TestHarnessOptionsFallthrough(t *testing.T) {
	// Harnesses without option-aware variants behave as before.
	withMockExec(t, func(mock *mockCmd) {
		if err := harnesses["claude"].PreseedFor(HarnessOptions{})("sb"); err != nil || len(mock.calls) != 1 {
			t.Errorf("claude preseed: %v %v", err, mock.calls)
		}
		if err := harnesses["pi"].InitHookFor(HarnessOptions{})("sb"); err != nil || len(mock.calls) != 1 { // claude preseed only; pi needs no init exec
			t.Errorf("pi init: %v %v", err, mock.calls)
		}
	})
}
