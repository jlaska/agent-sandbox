package agentrun

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Supported harness types.
const (
	HarnessClaude = "claude"
	HarnessPi     = "pi"
	// HarnessOpencode is the opencode CLI (github.com/anomalyco/opencode).
	HarnessOpencode = "opencode"
)

// HarnessOptions carries per-run settings that some harnesses need when
// choosing providers, init hooks and preseed config (currently only opencode,
// whose inference backend is selectable).
type HarnessOptions struct {
	// Inference is the --inference value ("litellm", "anthropic" or "").
	Inference string
	// Model is the --model value, used to declare the model for LiteLLM.
	Model string
	// ExplicitEnv is the --env KEY=VALUE list (credential/config lookups).
	ExplicitEnv []string
}

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

	// Optional option-aware variants; when set they take precedence over the
	// plain forms above via ProvidersFor / InitHookFor / PreseedFor.
	profilesFor func(HarnessOptions) ([]string, error)
	initHookFor func(sandboxName string, o HarnessOptions) error
	preseedFor  func(sandboxName string, o HarnessOptions) error
}

// ProvidersFor is Providers narrowed to the backend selected by o.
func (h *Harness) ProvidersFor(o HarnessOptions) ([]string, error) {
	if h.profilesFor != nil {
		return h.profilesFor(o)
	}
	return h.profiles, nil
}

// InitHookFor is InitHook for the backend selected by o.
func (h *Harness) InitHookFor(o HarnessOptions) func(sandboxName string) error {
	if h.initHookFor != nil {
		return func(sb string) error { return h.initHookFor(sb, o) }
	}
	return h.InitHook()
}

// PreseedFor is Preseed for the backend selected by o.
func (h *Harness) PreseedFor(o HarnessOptions) func(sandboxName string) error {
	if h.preseedFor != nil {
		return func(sb string) error { return h.preseedFor(sb, o) }
	}
	return h.Preseed()
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
		Name:        HarnessClaude,
		Command:     "claude",
		ModelFlag:   "--model",
		ImageKey:    "claude",
		profiles:    []string{"claude-code"},
		initHook:    func(sb string) error { return claudeInitHook(sb, HarnessOptions{}) },
		initHookFor: claudeInitHook,
		preseed:     claudePreseed,
	},
	HarnessPi: {
		Name:      HarnessPi,
		Command:   "pi",
		ModelFlag: "--model",
		ImageKey:  "pi",
		profiles:  []string{"litellm-inference"},
	},
	// opencode 1.19.3: both the default TUI command (`opencode --help`, pinned
	// image) and `opencode run` (packages/opencode/src/cli/cmd/run.ts,
	// alias ["m"]) accept -m/--model in provider/model form.
	HarnessOpencode: {
		Name:        HarnessOpencode,
		Command:     "opencode",
		ModelFlag:   "--model",
		ImageKey:    "opencode",
		profiles:    []string{"anthropic", "litellm-inference"},
		profilesFor: opencodeProfiles,
		initHook:    func(sb string) error { return opencodeInitHook(sb, HarnessOptions{}) },
		initHookFor: opencodeInitHook,
		preseed:     func(sb string) error { return opencodePreseed(sb, HarnessOptions{}) },
		preseedFor:  opencodePreseed,
	},
}

// Backend names for opencode (--inference values).
const (
	inferenceLiteLLM   = "litellm"
	inferenceAnthropic = "anthropic"
)

// resolveOpencodeBackend picks the opencode inference backend. An explicit
// --inference wins; otherwise LiteLLM is used if LITELLM_API_KEY resolves,
// else Anthropic if ANTHROPIC_API_KEY resolves. opencode needs an API key, so
// a Pro/Max CLAUDE_CODE_OAUTH_TOKEN alone is rejected with a specific error.
func resolveOpencodeBackend(inference string, env []string) (string, error) {
	hasLiteLLM := resolveCredential("LITELLM_API_KEY", env) != ""
	hasAnthropic := resolveCredential("ANTHROPIC_API_KEY", env) != ""
	hasOAuth := resolveCredential("CLAUDE_CODE_OAUTH_TOKEN", env) != ""
	oauthErr := fmt.Errorf("opencode needs an API key: CLAUDE_CODE_OAUTH_TOKEN (Claude Pro/Max OAuth) is not usable with third-party tools; set ANTHROPIC_API_KEY or LITELLM_API_KEY")

	switch inference {
	case inferenceLiteLLM:
		if !hasLiteLLM {
			return "", fmt.Errorf("--inference litellm requires LITELLM_API_KEY, set it in your environment or pass --env LITELLM_API_KEY=<key>")
		}
		return inferenceLiteLLM, nil
	case inferenceAnthropic:
		if !hasAnthropic {
			if hasOAuth {
				return "", oauthErr
			}
			return "", fmt.Errorf("--inference anthropic requires ANTHROPIC_API_KEY, set it in your environment or pass --env ANTHROPIC_API_KEY=<key>")
		}
		return inferenceAnthropic, nil
	case "":
	default:
		return "", fmt.Errorf("invalid --inference %q; supported: %s, %s", inference, inferenceLiteLLM, inferenceAnthropic)
	}

	switch {
	case hasLiteLLM:
		return inferenceLiteLLM, nil
	case hasAnthropic:
		return inferenceAnthropic, nil
	case hasOAuth:
		return "", oauthErr
	}
	return "", fmt.Errorf("opencode needs an inference API key: set LITELLM_API_KEY (litellm backend) or ANTHROPIC_API_KEY (anthropic backend)")
}

func opencodeProfiles(o HarnessOptions) ([]string, error) {
	backend, err := resolveOpencodeBackend(o.Inference, o.ExplicitEnv)
	if err != nil {
		return nil, err
	}
	if backend == inferenceLiteLLM {
		return []string{"litellm-inference"}, nil
	}
	return []string{"anthropic"}, nil
}

// opencodeInitHook needs no sandbox-side work: the providers store their
// credentials under the env-var names the opencode config reads
// (ANTHROPIC_API_KEY, LITELLM_API_KEY), so the sandbox already carries those
// placeholders. It still validates the backend choice.
func opencodeInitHook(_ string, o HarnessOptions) error {
	_, err := resolveOpencodeBackend(o.Inference, o.ExplicitEnv)
	return err
}

// opencodeConfigPath is the global config the image already seeds with
// autoupdate=false (openshell/images/opencode/Dockerfile).
const opencodeConfigPath = "~/.config/opencode/opencode.json"

// opencodeConfig renders ~/.config/opencode/opencode.json for a backend. It
// is a superset of the image-baked file ($schema + autoupdate=false) so the
// preseed never drops those keys. Keys (v1.19.3, packages/core/src/v1/config):
// config.ts "share" (manual|auto|disabled), "autoupdate", "enabled_providers";
// provider.ts "npm", "name", "options.{baseURL,apiKey}", "models".
func opencodeConfig(backend, liteLLMBaseURL, model string) ([]byte, error) {
	cfg := map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"autoupdate": false,
		"share":      "disabled",
	}
	switch backend {
	case inferenceAnthropic:
		cfg["enabled_providers"] = []string{"anthropic"}
		cfg["provider"] = map[string]any{
			"anthropic": map[string]any{
				"options": map[string]any{"apiKey": "{env:ANTHROPIC_API_KEY}"},
			},
		}
	case inferenceLiteLLM:
		models := map[string]any{}
		if id, ok := strings.CutPrefix(model, "litellm/"); ok && id != "" {
			models[id] = map[string]any{"name": id}
		}
		cfg["enabled_providers"] = []string{"litellm"}
		cfg["provider"] = map[string]any{
			"litellm": map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": "LiteLLM",
				"options": map[string]any{
					"baseURL": strings.TrimRight(liteLLMBaseURL, "/"),
					"apiKey":  "{env:LITELLM_API_KEY}",
				},
				"models": models,
			},
		}
	default:
		return nil, fmt.Errorf("unknown opencode backend %q", backend)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// opencodePreseed writes the global opencode config for the selected backend.
func opencodePreseed(sandboxName string, o HarnessOptions) error {
	backend, err := resolveOpencodeBackend(o.Inference, o.ExplicitEnv)
	if err != nil {
		return err
	}
	baseURL := resolveCredential("LITELLM_BASE_URL", o.ExplicitEnv)
	if backend == inferenceLiteLLM && baseURL == "" {
		return fmt.Errorf("LITELLM_BASE_URL is required for the opencode litellm backend, set it in your environment or pass --env LITELLM_BASE_URL=<url>")
	}
	cfg, err := opencodeConfig(backend, baseURL, o.Model)
	if err != nil {
		return err
	}
	return execCmd("openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c",
		"mkdir -p ~/.config/opencode && cat > "+opencodeConfigPath+" << 'PRESEED'\n"+string(cfg)+"PRESEED")
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

// claudeInitHook aliases the sandbox's ANTHROPIC_API_KEY placeholder (the only
// key the claude-code profile accepts) as CLAUDE_CODE_OAUTH_TOKEN when that was
// the host credential, and hides the API-key name so Claude Code does not
// prefer it. The alias copies the placeholder; it never overwrites one. With a
// real API key (or ANTHROPIC_AUTH_TOKEN) no sandbox-side work is needed.
func claudeInitHook(sandboxName string, o HarnessOptions) error {
	if _, source := claudeCredential(o.ExplicitEnv); source != "CLAUDE_CODE_OAUTH_TOKEN" {
		return nil
	}
	return execCmd("openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c",
		`echo 'export CLAUDE_CODE_OAUTH_TOKEN="$ANTHROPIC_API_KEY"; unset ANTHROPIC_API_KEY' | tee -a ~/.bashrc >> ~/.profile`)
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
