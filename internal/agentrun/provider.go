package agentrun

import (
	"fmt"
	"os"
	"strings"
)

// Known inference env vars to auto-forward from the host environment.
// Split into credentials (injected via OpenShell provider, secure) and
// config (injected via --env, visible to the agent).
var inferenceCredentialVars = map[string][]string{
	HarnessClaude: {
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
	},
	HarnessPi: {
		"LITELLM_API_KEY",
	},
	HarnessOpencode: {
		"LITELLM_API_KEY",
		"ANTHROPIC_API_KEY",
		"CLAUDE_CODE_OAUTH_TOKEN",
	},
}

var inferenceConfigVars = map[string][]string{
	HarnessClaude: {
		"ANTHROPIC_BASE_URL",
		"CLAUDE_CODE_USE_VERTEX",
		"ANTHROPIC_VERTEX_PROJECT_ID",
		"CLOUD_ML_REGION",
		"ANTHROPIC_CUSTOM_HEADERS",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
	},
	HarnessPi: {
		"LITELLM_BASE_URL",
		"ANTHROPIC_BASE_URL",
	},
	HarnessOpencode: {
		"LITELLM_BASE_URL",
	},
}

// resolveInferenceEnv merges auto-forwarded host config env vars with explicit
// --env overrides. Credential vars are excluded — they go through providers.
// Returns --env flags for openshell sandbox create.
func resolveInferenceEnv(harness string, explicitEnv []string) []string {
	explicitKeys := make(map[string]bool)
	for _, e := range explicitEnv {
		if k, _, ok := strings.Cut(e, "="); ok {
			explicitKeys[k] = true
		}
	}

	// Build set of credential keys to exclude from --env forwarding
	credentialKeys := make(map[string]bool)
	for _, key := range inferenceCredentialVars[harness] {
		credentialKeys[key] = true
	}

	var envFlags []string

	// Auto-forward config vars from host (unless overridden by --env)
	for _, key := range inferenceConfigVars[harness] {
		if explicitKeys[key] {
			continue
		}
		if val := os.Getenv(key); val != "" {
			envFlags = append(envFlags, "--env", key+"="+val)
		}
	}

	// Add explicit --env values, but warn if they look like credentials
	for _, e := range explicitEnv {
		k, _, _ := strings.Cut(e, "=")
		if credentialKeys[k] {
			fmt.Printf("[agent-run] NOTE: %s will be injected via provider credential (not --env).\n", k)
			continue
		}
		envFlags = append(envFlags, "--env", e)
	}

	return envFlags
}

// resolveCredential finds a credential value from explicit --env args or host env.
func resolveCredential(key string, explicitEnv []string) string {
	for _, e := range explicitEnv {
		if k, v, ok := strings.Cut(e, "="); ok && k == key {
			return v
		}
	}
	return os.Getenv(key)
}

// createInferenceProviders creates the OpenShell providers needed for the
// harness to reach its inference endpoint, with credentials injected securely.
func createInferenceProviders(harness string, explicitEnv []string) (providerFlags []string, providerNames []string, err error) {
	return createInferenceProvidersFor(harness, "", explicitEnv)
}

// createInferenceProvidersFor is createInferenceProviders with the --inference
// backend (opencode only; "" selects the default).
func createInferenceProvidersFor(harness, inference string, explicitEnv []string) (providerFlags []string, providerNames []string, err error) {
	var flags []string
	var names []string

	switch harness {
	case HarnessClaude:
		name := "claude-code"
		apiKey := resolveCredential("ANTHROPIC_API_KEY", explicitEnv)
		if apiKey == "" {
			apiKey = resolveCredential("CLAUDE_CODE_OAUTH_TOKEN", explicitEnv)
		}
		if apiKey == "" {
			apiKey = resolveCredential("ANTHROPIC_AUTH_TOKEN", explicitEnv)
		}
		if apiKey == "" {
			apiKey = "none"
		}

		_ = execCmdSilent("openshell", "provider", "delete", name)
		if err := execCmd("openshell", "provider", "create",
			"--name", name,
			"--type", "claude-code",
			"--credential", fmt.Sprintf("api_key=%s", apiKey),
		); err != nil {
			return nil, nil, fmt.Errorf("failed to create claude-code provider: %w", err)
		}
		flags = append(flags, "--provider", name)
		names = append(names, name)

	case HarnessPi:
		apiKey := resolveCredential("LITELLM_API_KEY", explicitEnv)
		if apiKey == "" {
			return nil, nil, fmt.Errorf("LITELLM_API_KEY is required for Pi harness, set it in your environment or pass --env LITELLM_API_KEY=<key>")
		}
		if err := createLiteLLMProvider(apiKey); err != nil {
			return nil, nil, err
		}
		flags = append(flags, "--provider", "litellm-inference")
		names = append(names, "litellm-inference")

	case HarnessOpencode:
		backend, err := resolveOpencodeBackend(inference, explicitEnv)
		if err != nil {
			return nil, nil, err
		}
		if backend == inferenceLiteLLM {
			if err := createLiteLLMProvider(resolveCredential("LITELLM_API_KEY", explicitEnv)); err != nil {
				return nil, nil, err
			}
			flags = append(flags, "--provider", "litellm-inference")
			names = append(names, "litellm-inference")
			break
		}
		name := "anthropic"
		_ = execCmdSilent("openshell", "provider", "delete", name)
		if err := execCmd("openshell", "provider", "create",
			"--name", name,
			"--type", "anthropic",
			"--credential", fmt.Sprintf("api_key=%s", resolveCredential("ANTHROPIC_API_KEY", explicitEnv)),
		); err != nil {
			return nil, nil, fmt.Errorf("failed to create anthropic provider: %w", err)
		}
		flags = append(flags, "--provider", name)
		names = append(names, name)
	}

	return flags, names, nil
}

// createLiteLLMProvider (re)creates the litellm-inference provider.
func createLiteLLMProvider(apiKey string) error {
	name := "litellm-inference"
	_ = execCmdSilent("openshell", "provider", "delete", name)
	if err := execCmd("openshell", "provider", "create",
		"--name", name,
		"--type", "litellm-inference",
		"--credential", fmt.Sprintf("litellm_api_key=%s", apiKey),
		"--credential", "litellm_bearer_token=unused",
	); err != nil {
		return fmt.Errorf("failed to create litellm-inference provider: %w", err)
	}
	return nil
}
