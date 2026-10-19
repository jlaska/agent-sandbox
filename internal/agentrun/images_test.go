package agentrun

import (
	"strings"
	"testing"
)

const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

const testImagesYAML = `images:
  claude:
    ref: ghcr.io/jlaska/agent-claude:1.0.0@` + testDigest + `
  pi:
    ref: ghcr.io/jlaska/agent-pi:2.0.0@sha256:0000000000000000000000000000000000000000000000000000000000000000
`

func TestResolveImageFrom(t *testing.T) {
	claude, pi := harnesses["claude"], harnesses["pi"]

	t.Run("published ref from yaml", func(t *testing.T) {
		got, err := resolveImageFrom([]byte(testImagesYAML), claude, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "ghcr.io/jlaska/agent-claude:1.0.0@" + testDigest; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// Before Renovate pins the digest, the default run uses the version tag
	// (with a warning) instead of failing for every user.
	t.Run("zero digest falls back to the tag with a warning", func(t *testing.T) {
		var got string
		var err error
		out := captureStdout(t, func() { got, err = resolveImageFrom([]byte(testImagesYAML), pi, "") })
		if err != nil || got != "ghcr.io/jlaska/agent-pi:2.0.0" {
			t.Errorf("got (%q, %v)", got, err)
		}
		if !strings.Contains(out, "WARNING: the pi image digest is not pinned yet") {
			t.Errorf("out = %q", out)
		}
	})

	t.Run("harness missing from yaml", func(t *testing.T) {
		_, err := resolveImageFrom([]byte("images: {}\n"), claude, "")
		if err == nil || !strings.Contains(err.Error(), "no image configured") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("invalid yaml", func(t *testing.T) {
		if _, err := resolveImageFrom([]byte("images: [unterminated"), claude, ""); err == nil {
			t.Error("expected parse error")
		}
	})

	t.Run("override wins, even over unpublished", func(t *testing.T) {
		got, err := resolveImageFrom([]byte(testImagesYAML), pi, "agent-pi:local")
		if err != nil || got != "agent-pi:local" {
			t.Errorf("got %q, %v", got, err)
		}
	})

	t.Run("unpinned override warns", func(t *testing.T) {
		out := captureStdout(t, func() {
			_, _ = resolveImageFrom([]byte(testImagesYAML), pi, "agent-pi:local")
		})
		if !strings.Contains(out, "WARNING") || !strings.Contains(out, "not digest-pinned") {
			t.Errorf("expected unpinned WARN, got %q", out)
		}
	})

	t.Run("pinned override does not warn", func(t *testing.T) {
		out := captureStdout(t, func() {
			_, _ = resolveImageFrom([]byte(testImagesYAML), pi, "example.com/pi:1@"+testDigest)
		})
		if out != "" {
			t.Errorf("expected no output, got %q", out)
		}
	})
}

// TestResolveImageEmbedded checks every registered harness has an entry in the
// embedded images.yaml (published or not).
func TestResolveImageEmbedded(t *testing.T) {
	for _, name := range HarnessNames() {
		var ref string
		var err error
		out := captureStdout(t, func() { ref, err = resolveImage(harnesses[name], "") })
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !strings.Contains(ref, "@sha256:") && !strings.Contains(out, "not pinned yet") {
			t.Errorf("%s: embedded ref %q is neither digest-pinned nor warned about", name, ref)
		}
	}
}
