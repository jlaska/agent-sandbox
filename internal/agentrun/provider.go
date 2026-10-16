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
		apiKey, _ := claudeCredential(explicitEnv)
		_ = execCmdSilent("openshell", "provider", "delete", name)
		// The claude-code profile declares ANTHROPIC_API_KEY (and CLAUDE_API_KEY);
		// 0.1.4 rejects any other stored key.
		if err := createProvider(name, "claude-code", []credentialPair{{"ANTHROPIC_API_KEY", apiKey}}); err != nil {
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
		if err := createProvider(name, "anthropic",
			[]credentialPair{{"ANTHROPIC_API_KEY", resolveCredential("ANTHROPIC_API_KEY", explicitEnv)}}); err != nil {
			return nil, nil, fmt.Errorf("failed to create anthropic provider: %w", err)
		}
		flags = append(flags, "--provider", name)
		names = append(names, name)
	}

	return flags, names, nil
}

// createLiteLLMProvider (re)creates the litellm-inference provider. Only
// LITELLM_API_KEY is stored: the profile declares it as required, and the
// proxy resolves its placeholder wherever the client puts it (x-api-key for
// Claude Code, Authorization: Bearer for Pi/opencode), so the optional
// LITELLM_API_KEY_BEARER / LITELLM_BEARER_TOKEN credentials are not needed.
func createLiteLLMProvider(apiKey string) error {
	name := "litellm-inference"
	_ = execCmdSilent("openshell", "provider", "delete", name)
	if err := createProvider(name, "litellm-inference", []credentialPair{{"LITELLM_API_KEY", apiKey}}); err != nil {
		return fmt.Errorf("failed to create litellm-inference provider: %w", err)
	}
	return nil
}

// credentialPair is one provider credential: Key is the env-var name the
// profile declares (OpenShell 0.1.4 accepts only declared env_vars as stored
// keys, and the sandbox sees a placeholder under that same name).
type credentialPair struct{ Key, Value string }

// createProvider runs `openshell provider create` with bare-key credentials
// (`--credential KEY`): OpenShell reads each value from the child's environment
// of the same name, so secrets never appear in argv (ps, shell history, logs).
func createProvider(name, providerType string, creds []credentialPair) error {
	args := []string{"provider", "create", "--name", name, "--type", providerType}
	env := make([]string, 0, len(creds))
	for _, c := range creds {
		args = append(args, "--credential", c.Key)
		env = append(env, c.Key+"="+c.Value)
	}
	return execCmdEnv(env, "openshell", args...)
}

// claudeCredential picks the secret for the claude-code provider and the host
// variable it came from, in priority order. The profile only declares
// ANTHROPIC_API_KEY, so every source is stored under that key; the OAuth token
// is re-exposed under its own name inside the sandbox by claudeInitHook.
func claudeCredential(explicitEnv []string) (value, source string) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		if v := resolveCredential(k, explicitEnv); v != "" {
			return v, k
		}
	}
	return "none", ""
}
