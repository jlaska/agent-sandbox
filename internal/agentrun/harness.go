package agentrun

import (
	"fmt"
	"sort"
	"strings"
)

// Supported harness types.
const (
	HarnessClaude = "claude"
	HarnessPi     = "pi"
)

// Harness describes one agent CLI that agent-run can launch in a sandbox.
type Harness struct {
	// Name is the CLI-facing harness name (the second positional argument).
	Name string
	// Command is the binary exec'd inside the sandbox.
	Command string
	// ModelFlag is the harness flag that selects a model ("" if unsupported).
	ModelFlag string
	// ImageKey is the key of this harness in openshell/images.yaml.
	ImageKey string

	// profiles lists the OpenShell provider profile ids the harness needs
	// imported before its inference provider can be created.
	profiles []string
	initHook func(sandboxName string) error
	preseed  func(sandboxName string) error
}

// Providers returns the provider profile ids that must exist on the gateway
// for this harness (see ensureProfile). The github-agent profile is needed by
// every harness and is handled separately in Run.
func (h *Harness) Providers() []string { return h.profiles }

// InitHook returns the function that maps provider-injected credentials to the
// env vars the harness expects. It never returns nil.
func (h *Harness) InitHook() func(sandboxName string) error {
	if h.initHook == nil {
		return func(string) error { return nil }
	}
	return h.initHook
}

// Preseed returns the function that writes harness config so interactive
// first-run wizards are skipped. It never returns nil.
func (h *Harness) Preseed() func(sandboxName string) error {
	if h.preseed == nil {
		return func(string) error { return nil }
	}
	return h.preseed
}

// harnesses is the harness registry. To add a harness (WP4c: opencode), add a
// constant above, an entry here, an image in openshell/images.yaml, and the
// per-harness cases in provider.go (credentials/providers) and Validate.
var harnesses = map[string]*Harness{
	HarnessClaude: {
		Name:      HarnessClaude,
		Command:   "claude",
		ModelFlag: "--model",
		ImageKey:  "claude",
		profiles:  []string{"claude-code"},
		initHook:  claudeInitHook,
		preseed:   claudePreseed,
	},
	HarnessPi: {
		Name:      HarnessPi,
		Command:   "pi",
		ModelFlag: "--model",
		ImageKey:  "pi",
		profiles:  []string{"litellm-inference"},
		initHook:  piInitHook,
	},
}

// LookupHarness returns the registered harness with the given name.
func LookupHarness(name string) (*Harness, error) {
	if h, ok := harnesses[name]; ok {
		return h, nil
	}
	if name == "shell" {
		return nil, fmt.Errorf("the shell harness was removed; supported harnesses: %s", strings.Join(HarnessNames(), ", "))
	}
	return nil, fmt.Errorf("unknown harness %q; supported: %s", name, strings.Join(HarnessNames(), ", "))
}

// HarnessNames returns the registered harness names, sorted.
func HarnessNames() []string {
	names := make([]string, 0, len(harnesses))
	for n := range harnesses {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// claudeInitHook maps the claude-code provider credential to the env vars
// Claude Code expects.
func claudeInitHook(sandboxName string) error {
	oauthToken := resolveCredential("CLAUDE_CODE_OAUTH_TOKEN", nil)
	if oauthToken != "" {
		return execCmd("openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c",
			`echo 'export CLAUDE_CODE_OAUTH_TOKEN="$api_key"' >> ~/.bashrc
				 echo 'export CLAUDE_CODE_OAUTH_TOKEN="$api_key"' >> ~/.profile`)
	}
	return execCmd("openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c",
		`echo 'export ANTHROPIC_API_KEY="$api_key"' >> ~/.bashrc
			 echo 'export ANTHROPIC_API_KEY="$api_key"' >> ~/.profile`)
}

// piInitHook maps the litellm-inference provider credential for Pi.
func piInitHook(sandboxName string) error {
	return execCmd("openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c",
		`echo 'export LITELLM_API_KEY="$litellm_api_key"' >> ~/.bashrc
			 echo 'export LITELLM_API_KEY="$litellm_api_key"' >> ~/.profile`)
}

// claudePreseed writes ~/.claude.json. Claude Code checks it for
// hasCompletedOnboarding and per-project hasTrustDialogAccepted; without
// these, every sandbox launch triggers interactive onboarding/trust prompts.
func claudePreseed(sandboxName string) error {
	return execCmd("openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c",
		`mkdir -p ~/.claude && cat > ~/.claude.json << 'PRESEED'
{
  "hasCompletedOnboarding": true,
  "projects": {
    "/sandbox/repo": {
      "hasTrustDialogAccepted": true,
      "hasCompletedProjectOnboarding": true
    }
  }
}
PRESEED`)
}
