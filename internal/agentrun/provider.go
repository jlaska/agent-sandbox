package agentrun

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
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
	// CLAUDE_CODE_OAUTH_TOKEN is not usable by opencode, but it is still a
	// secret: listing it keeps an explicit --env copy out of the sandbox env.
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
// --env overrides. Credential vars are excluded: the one the run uses goes
// through a provider, and the others are dropped. Returns --env flags for
// openshell sandbox create.
func resolveInferenceEnv(harness string, o HarnessOptions) []string {
	explicitKeys := make(map[string]bool)
	for _, e := range o.ExplicitEnv {
		if k, _, ok := strings.Cut(e, "="); ok {
			explicitKeys[k] = true
		}
	}

	// Build set of credential keys to exclude from --env forwarding
	credentialKeys := make(map[string]bool)
	for _, key := range inferenceCredentialVars[harness] {
		credentialKeys[key] = true
	}
	injected := injectedCredential(harness, o)

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

	// Add explicit --env values, but never forward a credential
	for _, e := range o.ExplicitEnv {
		k, _, _ := strings.Cut(e, "=")
		if credentialKeys[k] {
			if k == injected {
				fmt.Printf("[agent-run] NOTE: %s will be injected via provider credential (not --env).\n", k)
			} else {
				fmt.Printf("[agent-run] NOTE: %s is not used by %s in this run and is not forwarded (credentials never go through --env).\n", k, harness)
			}
			continue
		}
		envFlags = append(envFlags, "--env", e)
	}

	return envFlags
}

// injectedCredential is the host variable whose value the run stores in its
// inference provider ("" if none).
func injectedCredential(harness string, o HarnessOptions) string {
	switch harness {
	case HarnessClaude:
		_, source := claudeCredential(o.ExplicitEnv)
		return source
	case HarnessPi:
		return "LITELLM_API_KEY"
	case HarnessOpencode:
		if o.Backend == inferenceLiteLLM {
			return "LITELLM_API_KEY"
		}
		if o.Backend == inferenceAnthropic {
			return "ANTHROPIC_API_KEY"
		}
	}
	return ""
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
// Provider names are scoped to session (see sessionProviderName).
// For opencode, o.Backend selects the provider (see resolveHarnessOptions).
func createInferenceProviders(ctx context.Context, session, harness string, o HarnessOptions) (providerFlags []string, providerNames []string, err error) {
	var providerType string
	var creds []credentialPair

	switch harness {
	case HarnessClaude:
		// The claude-code profile declares ANTHROPIC_API_KEY (and CLAUDE_API_KEY);
		// 0.1.4 rejects any other stored key.
		apiKey, _ := claudeCredential(o.ExplicitEnv)
		providerType, creds = "claude-code", []credentialPair{{"ANTHROPIC_API_KEY", apiKey}}

	case HarnessPi:
		apiKey := resolveCredential("LITELLM_API_KEY", o.ExplicitEnv)
		if apiKey == "" {
			return nil, nil, fmt.Errorf("LITELLM_API_KEY is required for Pi harness, set it in your environment or pass --env LITELLM_API_KEY=<key>")
		}
		providerType, creds = litellmProvider(apiKey)

	case HarnessOpencode:
		if o.Backend == inferenceLiteLLM {
			providerType, creds = litellmProvider(resolveCredential("LITELLM_API_KEY", o.ExplicitEnv))
			break
		}
		providerType, creds = "anthropic", []credentialPair{{"ANTHROPIC_API_KEY", resolveCredential("ANTHROPIC_API_KEY", o.ExplicitEnv)}}

	default:
		return nil, nil, nil
	}

	name, err := createProvider(ctx, session, providerType, creds)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create %s provider: %w", providerType, err)
	}
	return []string{"--provider", name}, []string{name}, nil
}

// litellmProvider describes the litellm-inference provider. Only
// LITELLM_API_KEY is stored: the profile declares it as required, and the
// proxy resolves its placeholder wherever the client puts it (x-api-key for
// Claude Code, Authorization: Bearer for Pi/opencode), so the optional
// LITELLM_API_KEY_BEARER / LITELLM_BEARER_TOKEN credentials are not needed.
func litellmProvider(apiKey string) (string, []credentialPair) {
	return "litellm-inference", []credentialPair{{"LITELLM_API_KEY", apiKey}}
}

// credentialPair is one provider credential: Key is the env-var name the
// profile declares (OpenShell 0.1.4 accepts only declared env_vars as stored
// keys, and the sandbox sees a placeholder under that same name).
type credentialPair struct{ Key, Value string }

// sessionLabel is the provider label naming the agent-run session (the
// sandbox name) that owns the provider.
const sessionLabel = "agent-run.session"

// sessionProviderName is the name of session's provider of providerType.
// Names are per session so concurrent runs on one gateway never share, or
// collide on, a provider.
func sessionProviderName(session, providerType string) string {
	return session + "-" + providerType
}

// createProvider runs `openshell provider create` for session's provider of
// providerType, labelled with the session, and returns its name. Credentials
// are bare keys (`--credential KEY`): OpenShell reads each value from the
// child's environment of the same name, so secrets never appear in argv (ps,
// shell history, logs).
func createProvider(ctx context.Context, session, providerType string, creds []credentialPair) (string, error) {
	name := sessionProviderName(session, providerType)
	args := []string{"provider", "create", "--name", name, "--type", providerType,
		"--label", sessionLabel + "=" + session}
	env := make([]string, 0, len(creds))
	for _, c := range creds {
		args = append(args, "--credential", c.Key)
		env = append(env, c.Key+"="+c.Value)
	}
	return name, execCmdEnv(ctx, env, "openshell", args...)
}

// claudeCredential picks the secret for the claude-code provider and the host
// variable it came from, in priority order. The profile only declares
// ANTHROPIC_API_KEY, so every source is stored under that key; the OAuth token
// is re-exposed under its own name in the launch shell by claudeLaunchEnv.
func claudeCredential(explicitEnv []string) (value, source string) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		if v := resolveCredential(k, explicitEnv); v != "" {
			return v, k
		}
	}
	return "none", ""
}

// deleteProvider deletes provider name. `sandbox delete` is asynchronous and
// the gateway refuses to delete a provider while a sandbox is still attached
// ("is attached to sandbox(es)"), so that refusal is retried until
// sandboxGoneTimeout; any other failure is returned at once.
func deleteProvider(ctx context.Context, name string) error {
	deadline := time.Now().Add(sandboxGoneTimeout)
	for {
		out, err := execCmdOutput(ctx, "openshell", "provider", "delete", name)
		if err == nil {
			return nil
		}
		if !strings.Contains(out, "is attached to sandbox") || time.Now().After(deadline) {
			if msg := strings.Join(strings.Fields(out), " "); msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ErrInterrupted
		case <-time.After(sandboxGonePoll):
		}
	}
}
