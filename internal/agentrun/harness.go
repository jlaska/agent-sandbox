package agentrun

import (
	"context"
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
// choosing providers, preseed config and launch environment. Build it with
// resolveHarnessOptions so the opencode backend is resolved exactly once.
type HarnessOptions struct {
	// Inference is the --inference value ("litellm", "anthropic" or "").
	Inference string
	// Model is the --model value, used to declare the model for LiteLLM.
	Model string
	// ExplicitEnv is the --env KEY=VALUE list (credential/config lookups).
	ExplicitEnv []string
	// Backend is the resolved opencode inference backend (inferenceLiteLLM or
	// inferenceAnthropic); empty for other harnesses.
	Backend string
}

// resolveHarnessOptions builds the options for a run of harness and, for
// opencode, resolves the inference backend (failing if none is usable).
func resolveHarnessOptions(harness string, cfg *Config) (HarnessOptions, error) {
	o := HarnessOptions{Inference: cfg.Inference, Model: cfg.Model, ExplicitEnv: cfg.EnvVars}
	if harness == HarnessOpencode {
		backend, err := resolveOpencodeBackend(cfg.Inference, cfg.EnvVars)
		if err != nil {
			return o, err
		}
		// opencode drops a provider with no models, so a LiteLLM run without
		// a litellm/<id> model fails inside the sandbox with an opaque
		// "UnknownError" (ProviderNoProvidersError in its logs).
		if _, ok := liteLLMModelID(cfg.Model); backend == inferenceLiteLLM && !ok {
			return o, fmt.Errorf("the opencode litellm backend requires --model litellm/<model-id> (got %q); opencode only declares that model for the LiteLLM provider (or use --inference anthropic)", cfg.Model)
		}
		o.Backend = backend
	}
	return o, nil
}

// liteLLMModelID returns the model id of an opencode litellm/<id> model.
func liteLLMModelID(model string) (string, bool) {
	id, ok := strings.CutPrefix(model, "litellm/")
	return id, ok && id != ""
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

	// profiles returns the OpenShell provider profile ids the harness needs
	// imported before its inference provider can be created.
	profiles func(HarnessOptions) []string
	// preseed writes harness config so interactive first-run wizards are
	// skipped (nil: nothing to write).
	preseed func(ctx context.Context, sandboxName string, o HarnessOptions) error
	// launchEnv returns shell statements run in the launch shell right before
	// the harness is exec'd (nil: none).
	launchEnv func(HarnessOptions) string
}

// Providers returns the provider profile ids that must exist on the gateway
// for this harness (see ensureProfile). The github-agent profile is needed by
// every harness and is handled separately in Run.
func (h *Harness) Providers(o HarnessOptions) []string {
	if h.profiles == nil {
		return nil
	}
	return h.profiles(o)
}

// Preseed writes the harness config in the sandbox (a no-op for harnesses
// without one).
func (h *Harness) Preseed(ctx context.Context, sandboxName string, o HarnessOptions) error {
	if h.preseed == nil {
		return nil
	}
	return h.preseed(ctx, sandboxName, o)
}

// LaunchEnv returns the shell statements that prepare the harness environment
// in the launch shell ("" when there are none).
func (h *Harness) LaunchEnv(o HarnessOptions) string {
	if h.launchEnv == nil {
		return ""
	}
	return h.launchEnv(o)
}

func fixedProfiles(ids ...string) func(HarnessOptions) []string {
	return func(HarnessOptions) []string { return ids }
}

// harnesses is the harness registry. To add a harness, add a constant above,
// an entry here, an image in openshell/images.yaml, and the per-harness cases
// in provider.go (credentials/providers) and resolveHarnessOptions.
var harnesses = map[string]*Harness{
	HarnessClaude: {
		Name:      HarnessClaude,
		Command:   "claude",
		ModelFlag: "--model",
		ImageKey:  "claude",
		profiles:  fixedProfiles("claude-code"),
		preseed:   claudePreseed,
		launchEnv: claudeLaunchEnv,
	},
	HarnessPi: {
		Name:      HarnessPi,
		Command:   "pi",
		ModelFlag: "--model",
		ImageKey:  "pi",
		profiles:  fixedProfiles("litellm-inference"),
	},
	// opencode 1.19.3: both the default TUI command (`opencode --help`, pinned
	// image) and `opencode run` (packages/opencode/src/cli/cmd/run.ts,
	// alias ["m"]) accept -m/--model in provider/model form. The providers
	// store their credentials under the env-var names the opencode config
	// reads (ANTHROPIC_API_KEY, LITELLM_API_KEY), so no launch env is needed.
	HarnessOpencode: {
		Name:      HarnessOpencode,
		Command:   "opencode",
		ModelFlag: "--model",
		ImageKey:  "opencode",
		profiles:  opencodeProfiles,
		preseed:   opencodePreseed,
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

func opencodeProfiles(o HarnessOptions) []string {
	if o.Backend == inferenceLiteLLM {
		return []string{"litellm-inference"}
	}
	return []string{"anthropic"}
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
		if id, ok := liteLLMModelID(model); ok {
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
func opencodePreseed(ctx context.Context, sandboxName string, o HarnessOptions) error {
	baseURL := resolveCredential("LITELLM_BASE_URL", o.ExplicitEnv)
	if o.Backend == inferenceLiteLLM && baseURL == "" {
		return fmt.Errorf("LITELLM_BASE_URL is required for the opencode litellm backend, set it in your environment or pass --env LITELLM_BASE_URL=<url>")
	}
	cfg, err := opencodeConfig(o.Backend, baseURL, o.Model)
	if err != nil {
		return err
	}
	return execCmd(ctx, "openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c",
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

// claudeLaunchEnv exposes the provider's ANTHROPIC_API_KEY placeholder as
// CLAUDE_CODE_OAUTH_TOKEN when the host credential was an OAuth token, and
// drops the API-key name so Claude Code does not prefer it. The claude-code
// profile only accepts ANTHROPIC_API_KEY as a stored key, so the token travels
// under that name and is renamed here, in the launch shell only: no dotfile is
// edited, so whatever ~/.profile and ~/.bashrc do cannot run it twice. With a
// real API key (or ANTHROPIC_AUTH_TOKEN) nothing needs renaming.
func claudeLaunchEnv(o HarnessOptions) string {
	if _, source := claudeCredential(o.ExplicitEnv); source != "CLAUDE_CODE_OAUTH_TOKEN" {
		return ""
	}
	return `export CLAUDE_CODE_OAUTH_TOKEN="$ANTHROPIC_API_KEY"; unset ANTHROPIC_API_KEY`
}

// claudePreseed writes ~/.claude.json. Claude Code checks it for
// hasCompletedOnboarding and per-project hasTrustDialogAccepted; without
// these, every sandbox launch triggers interactive onboarding/trust prompts.
func claudePreseed(ctx context.Context, sandboxName string, _ HarnessOptions) error {
	return execCmd(ctx, "openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c",
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
