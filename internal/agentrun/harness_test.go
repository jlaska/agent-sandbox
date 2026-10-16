package agentrun

import (
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
	if got := strings.Join(HarnessNames(), ","); got != "claude,pi" {
		t.Errorf("HarnessNames() = %q", got)
	}
	for _, name := range HarnessNames() {
		h := harnesses[name]
		if h.ImageKey == "" || h.Command == "" || len(h.Providers()) == 0 {
			t.Errorf("harness %s incomplete: %+v", name, h)
		}
		for _, id := range h.Providers() {
			path, ok := profileFiles[id]
			if !ok {
				t.Errorf("%s: provider profile %q has no embedded file mapping", name, id)
				continue
			}
			if _, err := openshell.ProviderProfiles.ReadFile(path); err != nil {
				t.Errorf("%s: embedded profile %s: %v", name, path, err)
			}
		}
		if h.InitHook() == nil || h.Preseed() == nil {
			t.Errorf("harness %s: hooks must never be nil", name)
		}
	}
}

func TestPreseed(t *testing.T) {
	t.Run("claude writes ~/.claude.json", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			if err := harnesses["claude"].Preseed()("test-sb"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(mock.calls) != 1 || !strings.Contains(strings.Join(mock.calls[0], " "), "hasTrustDialogAccepted") {
				t.Errorf("unexpected calls: %v", mock.calls)
			}
		})
	})

	t.Run("pi is noop", func(t *testing.T) {
		withMockExec(t, func(mock *mockCmd) {
			if err := harnesses["pi"].Preseed()("test-sb"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(mock.calls) != 0 {
				t.Errorf("expected 0 calls, got %v", mock.calls)
			}
		})
	})
}
