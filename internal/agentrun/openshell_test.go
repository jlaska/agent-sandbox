package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// captureStdout returns everything fn printed to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	_ = w.Close()
	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	return string(buf[:n])
}

func TestGateOpenShellVersion(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    string
		wantErr string
	}{
		{"0.1.4 accepted", "openshell 0.1.4\n", "0.1.4", ""},
		{"0.2.0 accepted", "openshell 0.2.0", "0.2.0", ""},
		{"1.0.0 accepted", "1.0.0", "1.0.0", ""},
		{"0.1.10 sorts above 0.1.4", "openshell 0.1.10", "0.1.10", ""},
		{"dev suffix", "openshell 0.1.5-dev.6+g2bf9969ab\n", "0.1.5", ""},
		{"v prefix", "openshell v0.1.4", "0.1.4", ""},
		{"0.0.113 rejected", "openshell 0.0.113\n", "0.0.113", "too old"},
		{"0.1.3 rejected", "openshell 0.1.3", "0.1.3", "requires >= 0.1.4"},
		{"garbage", "command not found", "", "could not parse"},
		{"empty", "", "", "could not parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gateOpenShellVersion(tt.out)
			if got != tt.want {
				t.Errorf("version = %q, want %q", got, tt.want)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
			if tt.wantErr == "too old" && !strings.Contains(err.Error(), "install v0.1.4") {
				t.Errorf("expected upgrade hint, got %v", err)
			}
		})
	}
}

func TestCheckOpenShellVersion(t *testing.T) {
	t.Run("runs openshell --version", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			mock.execOutput = func(string, ...string) (string, error) { return "openshell 0.1.4\n", nil }
			v, err := checkOpenShellVersion(context.Background())
			if err != nil || v != "0.1.4" {
				t.Fatalf("got %q, %v", v, err)
			}
			if got := strings.Join(mock.calls[0], " "); got != "openshell --version" {
				t.Errorf("call = %q", got)
			}
		})
	})

	t.Run("command failure", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			mock.execOutput = func(string, ...string) (string, error) { return "", errors.New("boom") }
			if _, err := checkOpenShellVersion(context.Background()); err == nil {
				t.Error("expected error")
			}
		})
	})
}

func TestEnsureProfile(t *testing.T) {
	importCalls := func(mock *mockCmd) int {
		n := 0
		for _, c := range mock.calls {
			if strings.Contains(strings.Join(c, " "), "provider profile import --file") {
				n++
			}
		}
		return n
	}

	t.Run("imports when absent", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			mock.execOutput = func(string, ...string) (string, error) { return "[]", nil }
			if err := ensureProfileListed("claude-code"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n := importCalls(mock); n != 1 {
				t.Errorf("import calls = %d, want 1", n)
			}
		})
	})

	t.Run("idempotent when present (json)", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			mock.execOutput = func(string, ...string) (string, error) {
				return `[{"id":"anthropic"},{"id":"claude-code"}]`, nil
			}
			if err := ensureProfileListed("claude-code"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n := importCalls(mock); n != 0 {
				t.Errorf("import calls = %d, want 0", n)
			}
		})
	})

	t.Run("idempotent when present (table fallback)", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			mock.execOutput = func(_ string, args ...string) (string, error) {
				if strings.Contains(strings.Join(args, " "), "-o json") {
					return "", errors.New("unsupported")
				}
				return "ID  CATEGORY\nlitellm-inference  inference\n", nil
			}
			if err := ensureProfileListed("litellm-inference"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n := importCalls(mock); n != 0 {
				t.Errorf("import calls = %d, want 0", n)
			}
		})
	})

	t.Run("table fallback does not match substrings", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			mock.execOutput = func(_ string, args ...string) (string, error) {
				if strings.Contains(strings.Join(args, " "), "-o json") {
					return "", errors.New("unsupported")
				}
				return "claude-code-extra  source\n", nil
			}
			if err := ensureProfileListed("claude-code"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n := importCalls(mock); n != 1 {
				t.Errorf("import calls = %d, want 1", n)
			}
		})
	})

	t.Run("import failure", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			mock.execOutput = func(string, ...string) (string, error) { return "[]", nil }
			mock.err = errors.New("denied")
			err := ensureProfileListed("anthropic")
			if err == nil || !strings.Contains(err.Error(), "failed to import anthropic") {
				t.Errorf("err = %v", err)
			}
		})
	})

	t.Run("unknown profile", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			if err := ensureProfileListed("nope"); err == nil {
				t.Error("expected error")
			}
		})
	})
}

// A gateway that already has the stock upstream profile (anthropic: curl only)
// must get our binaries via `provider profile update`, with the exported
// resource_version (0.1.4 rejects an update without one); equal binaries are a
// no-op.
func TestEnsureProfileReconcilesBinaries(t *testing.T) {
	listAndExport := func(exportJSON string, exportErr error) func(string, ...string) (string, error) {
		return func(_ string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "list-profiles"):
				return `[{"id":"anthropic"}]`, nil
			case strings.Contains(joined, "profile export anthropic -o json"):
				return exportJSON, exportErr
			}
			t.Errorf("unexpected output call: %s", joined)
			return "", nil
		}
	}
	updates := func(m *mockCmd) []string {
		var out []string
		for _, c := range callStrings(m) {
			if strings.Contains(c, "provider profile update") {
				out = append(out, c)
			}
		}
		return out
	}

	t.Run("stale binaries are updated", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = listAndExport(`{"id":"anthropic","resource_version":7,"binaries":["/usr/bin/curl","/usr/local/bin/curl"]}`, nil)
			var staged string
			execCmdFn = func(_ context.Context, name string, args ...string) error {
				m.calls = append(m.calls, append([]string{name}, args...))
				if len(args) >= 5 && args[2] == "update" {
					b, err := os.ReadFile(args[len(args)-1])
					if err != nil {
						t.Fatal(err)
					}
					staged = string(b)
				}
				return nil
			}
			captureStdout(t, func() {
				if err := ensureProfileListed("anthropic"); err != nil {
					t.Fatal(err)
				}
			})
			got := updates(m)
			if len(got) != 1 || !strings.HasPrefix(got[0], "openshell provider profile update anthropic --file ") {
				t.Fatalf("update calls = %v", got)
			}
			for _, want := range []string{"resource_version: 7", "/usr/local/bin/opencode", "endpoints: []"} {
				if !strings.Contains(staged, want) {
					t.Errorf("staged profile missing %q:\n%s", want, staged)
				}
			}
			if strings.Contains(strings.Join(callStrings(m), "\n"), "profile import") {
				t.Error("an installed profile must be updated, not imported")
			}
		})
	})

	t.Run("equal document is a no-op (server fields ignored)", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = listAndExport(exportOf(t, "anthropic", func(d map[string]any) {
				d["created_at"] = "2026-10-16 00:00:00"
				d["credentials"].([]any)[0].(map[string]any)["query_param"] = ""
			}), nil)
			if err := ensureProfileListed("anthropic"); err != nil {
				t.Fatal(err)
			}
			if got := updates(m); len(got) != 0 {
				t.Errorf("unexpected updates: %v", got)
			}
		})
	})

	// Same binaries, but an older import declared other credential env_vars:
	// 0.1.4 would reject `provider create --credential ANTHROPIC_API_KEY`.
	for name, mutate := range map[string]func(map[string]any){
		"stale credential env_vars": func(d map[string]any) {
			d["credentials"].([]any)[0].(map[string]any)["env_vars"] = []any{"api_key"}
		},
		"stale endpoints": func(d map[string]any) {
			// An import from before the profile went endpointless.
			d["endpoints"] = []any{map[string]any{"host": "api.anthropic.com", "port": 443}}
		},
		"stale auth_style": func(d map[string]any) {
			d["credentials"].([]any)[0].(map[string]any)["auth_style"] = "bearer"
		},
	} {
		t.Run(name+" are updated", func(t *testing.T) {
			withMockExec(t, func(m *mockCmd) {
				m.execOutput = listAndExport(exportOf(t, "anthropic", mutate), nil)
				out := captureStdout(t, func() {
					if err := ensureProfileListed("anthropic"); err != nil {
						t.Fatal(err)
					}
				})
				if got := updates(m); len(got) != 1 {
					t.Errorf("updates = %v", got)
				}
				if !strings.Contains(out, "differs in:") {
					t.Errorf("out = %q", out)
				}
			})
		})
	}

	t.Run("export failure leaves the profile alone", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = listAndExport("", errors.New("denied"))
			out := captureStdout(t, func() {
				if err := ensureProfileListed("anthropic"); err != nil {
					t.Fatal(err)
				}
			})
			if len(updates(m)) != 0 || !strings.Contains(out, "WARN") {
				t.Errorf("updates=%v out=%q", updates(m), out)
			}
		})
	})

	t.Run("update failure warns and continues", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = listAndExport(`{"resource_version":2,"binaries":["/usr/bin/curl"]}`, nil)
			m.err = errors.New("profile attached to a sandbox")
			out := captureStdout(t, func() {
				if err := ensureProfileListed("anthropic"); err != nil {
					t.Fatalf("a failed update must not abort the run: %v", err)
				}
			})
			if !strings.Contains(out, "WARN: could not update provider profile anthropic") {
				t.Errorf("out = %q", out)
			}
		})
	})

	t.Run("missing resource_version is not updated blindly", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = listAndExport(`{"binaries":["/usr/bin/curl"]}`, nil)
			captureStdout(t, func() { _ = ensureProfileListed("anthropic") })
			if len(updates(m)) != 0 {
				t.Errorf("updates = %v", updates(m))
			}
		})
	})
}

func TestEnsureProfiles(t *testing.T) {
	withMockExec(t, func(mock *mockCmd) {
		mock.execOutput = func(string, ...string) (string, error) { return "[]", nil }
		if err := ensureProfiles(context.Background(), harnesses["pi"].Providers(HarnessOptions{})); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		lists := 0
		for _, c := range callStrings(mock) {
			if strings.Contains(c, "list-profiles") {
				lists++
			}
		}
		if lists != 1 {
			t.Errorf("list-profiles calls = %d, want 1 per run", lists)
		}
		var imports int
		for _, c := range mock.calls {
			if strings.Contains(strings.Join(c, " "), "profile import") {
				imports++
			}
		}
		if imports != 2 { // github-agent + litellm-inference
			t.Errorf("imports = %d, want 2", imports)
		}
	})
}

func TestDetectDriver(t *testing.T) {
	tests := []struct {
		name     string
		override string
		out      string
		err      error
		want     string
	}{
		{"override wins", "kubernetes", "", nil, "kubernetes"},
		{"podman from gateway info", "", `{"compute_drivers":[{"name":"Podman"}]}`, nil, "podman"},
		{"first driver used", "", `{"compute_drivers":[{"name":"docker"},{"name":"vm"}]}`, nil, "docker"},
		{"no drivers", "", `{"compute_drivers":[]}`, nil, "unknown"},
		{"empty name", "", `{"compute_drivers":[{"name":" "}]}`, nil, "unknown"},
		{"bad json", "", "Gateway Info", nil, "unknown"},
		{"command error", "", "", errors.New("no gateway"), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMockExec(t, func(mock *mockCmd) {
				mock.execOutput = func(string, ...string) (string, error) { return tt.out, tt.err }
				if got := detectDriver(context.Background(), tt.override); got != tt.want {
					t.Errorf("detectDriver(context.Background()) = %q, want %q", got, tt.want)
				}
				if tt.override != "" && len(mock.calls) != 0 {
					t.Error("override should not shell out")
				}
			})
		})
	}
}

func TestAllowDriverConfigHint(t *testing.T) {
	if got := allowDriverConfigHint("podman"); got != "[openshell.drivers.podman] allow_driver_config = true" {
		t.Errorf("got %q", got)
	}
	if got := allowDriverConfigHint("unknown"); !strings.Contains(got, "<driver>") {
		t.Errorf("got %q", got)
	}
}

// imageBinaries returns the union of every openshell/images/*/binaries.txt.
func imageBinaries(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../../openshell/images/*/binaries.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no binaries.txt files found: %v", err)
	}
	bins := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				bins[line] = true
			}
		}
	}
	return bins
}

func TestProfileData(t *testing.T) {
	t.Run("overrides vendored keys and keeps the rest", func(t *testing.T) {
		data, err := profileData("anthropic", profileFiles["anthropic"])
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			ID        string   `yaml:"id"`
			Binaries  []string `yaml:"binaries"`
			Endpoints []struct {
				Host string `yaml:"host"`
			} `yaml:"endpoints"`
		}
		if err := yaml.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		if p.ID != "anthropic" || len(p.Endpoints) != 0 {
			t.Errorf("profile content not preserved or endpoints not cleared: %+v", p)
		}
		if want := profileOverrides["anthropic"]["binaries"]; !reflect.DeepEqual(p.Binaries, want) {
			t.Errorf("binaries = %v, want %v", p.Binaries, want)
		}
	})

	t.Run("profiles without an override are unchanged", func(t *testing.T) {
		data, err := profileData("github-agent", profileFiles["github-agent"])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "id: github-agent") {
			t.Errorf("unexpected content:\n%s", data)
		}
	})
}

// Every binary a provider profile grants (after overrides) must be a real path
// that some image provides, or the provider's endpoints are unreachable.
func TestProfileBinariesExistInImages(t *testing.T) {
	images := imageBinaries(t)
	for id, path := range profileFiles {
		data, err := profileData(id, path)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		var p struct {
			Binaries []string `yaml:"binaries"`
		}
		if err := yaml.Unmarshal(data, &p); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if len(p.Binaries) == 0 {
			t.Errorf("%s: no binaries", id)
		}
		for _, b := range p.Binaries {
			if !images[b] {
				t.Errorf("%s: binary %s is not in any openshell/images/*/binaries.txt", id, b)
			}
		}
	}
}

// ensureProfileListed is ensureProfile for one id, with the installed state
// read from the (mocked) gateway listing as ensureProfiles does.
func ensureProfileListed(id string) error {
	ctx := context.Background()
	return ensureProfile(ctx, id, installedProfiles(ctx)[id])
}

// exportOf renders what `provider profile export <id> -o json` returns for
// agent-run's own profile (plus resource_version 7), after mutate.
func exportOf(t *testing.T, id string, mutate func(map[string]any)) string {
	t.Helper()
	data, err := profileData(id, profileFiles[id])
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	norm, err := normalizeJSON(doc)
	if err != nil {
		t.Fatal(err)
	}
	d := norm.(map[string]any)
	d["resource_version"] = 7
	if mutate != nil {
		mutate(d)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProfileDiffEmptyList(t *testing.T) {
	want := map[string]any{"endpoints": []any{}}
	for name, tc := range map[string]struct {
		have map[string]any
		diff bool
	}{
		"absent key matches":     {map[string]any{}, false},
		"empty list matches":     {map[string]any{"endpoints": []any{}}, false},
		"stale endpoints differ": {map[string]any{"endpoints": []any{map[string]any{"host": "api.github.com"}}}, true},
	} {
		if got := len(profileDiff("", want, tc.have)) > 0; got != tc.diff {
			t.Errorf("%s: diff = %v, want %v", name, got, tc.diff)
		}
	}
}

// Scalar lists such as binaries compare as sets: the gateway may reorder them.
func TestProfileDiffScalarListOrder(t *testing.T) {
	want := map[string]any{"binaries": []any{"/usr/local/bin/claude", "/usr/local/bin/node"}}
	if d := profileDiff("", want, map[string]any{"binaries": []any{"/usr/local/bin/node", "/usr/local/bin/claude"}}); len(d) > 0 {
		t.Errorf("reordered binaries reported as a diff: %v", d)
	}
	if d := profileDiff("", want, map[string]any{"binaries": []any{"/usr/local/bin/claude", "/usr/bin/curl"}}); len(d) == 0 {
		t.Error("different binaries not reported")
	}
}

// Every provider profile must stay endpointless: profile endpoints become a
// `_provider_*` rule that unions with the sandbox policy and reopens whatever
// it narrows (every repo the GitHub token reaches, every inference path, the
// telemetry hosts). The sandbox policy binds the providers instead.
func TestProfilesEndpointless(t *testing.T) {
	for id, path := range profileFiles {
		data, err := profileData(id, path)
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := yaml.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		eps, ok := p["endpoints"].([]any)
		if !ok || len(eps) != 0 {
			t.Errorf("%s endpoints = %#v, want an explicit empty list", id, p["endpoints"])
		}
	}
}
