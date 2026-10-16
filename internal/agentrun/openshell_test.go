package agentrun

import (
	"errors"
	"os"
	"strings"
	"testing"
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
			v, err := checkOpenShellVersion()
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
			if _, err := checkOpenShellVersion(); err == nil {
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
			if err := ensureProfile("claude-code"); err != nil {
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
			if err := ensureProfile("claude-code"); err != nil {
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
			if err := ensureProfile("litellm-inference"); err != nil {
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
			if err := ensureProfile("claude-code"); err != nil {
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
			err := ensureProfile("anthropic")
			if err == nil || !strings.Contains(err.Error(), "failed to import anthropic") {
				t.Errorf("err = %v", err)
			}
		})
	})

	t.Run("unknown profile", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			if err := ensureProfile("nope"); err == nil {
				t.Error("expected error")
			}
		})
	})
}

func TestEnsureProfiles(t *testing.T) {
	withMockExec(t, func(mock *mockCmd) {
		mock.execOutput = func(string, ...string) (string, error) { return "[]", nil }
		if err := ensureProfiles(harnesses["pi"]); err != nil {
			t.Fatalf("unexpected error: %v", err)
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
				if got := detectDriver(tt.override); got != tt.want {
					t.Errorf("detectDriver() = %q, want %q", got, tt.want)
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
