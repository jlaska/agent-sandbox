package agentrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Constants for the agent run configuration.
const (
	AppID   = "4720923"
	AppSlug = "jlaska-agent"

	GitUser  = "jlaska-agent[bot]"
	GitEmail = "jlaska-agent[bot]@users.noreply.github.com"

	// GitHubProviderType is the github-agent provider profile; each session's
	// provider of this type is named by sessionProviderName.
	GitHubProviderType = "github-agent"

	SandboxReadyTimeout = 10 * time.Minute
	SandboxReadyPoll    = 1 * time.Second
)

// ErrInterrupted is returned (wrapped) when a run was cut short by SIGINT or
// SIGTERM. Cleanup has already run by the time the caller sees it.
var ErrInterrupted = errors.New("interrupted by signal")

// Run executes the agent-run command with the given configuration.
func Run(cfg *Config) error { return RunContext(context.Background(), cfg) }

// RunContext is Run with a context that is cancelled on SIGINT/SIGTERM (see
// cmd/agent-run). Cancellation stops the run at the next step, terminates any
// running child, and then runs the normal cleanup exactly once (sandbox delete,
// token revoke, provider delete).
func RunContext(ctx context.Context, cfg *Config) error {
	err := run(ctx, cfg)
	if ctx.Err() != nil {
		if err != nil {
			if errors.Is(err, ErrInterrupted) {
				return err
			}
			return fmt.Errorf("%w: %v", ErrInterrupted, err)
		}
		return ErrInterrupted
	}
	return err
}

func run(ctx context.Context, cfg *Config) error {
	if cfg.Help || cfg.ListRepos {
		return nil
	}

	if cfg.Diag {
		return printDiagnostics(ctx, cfg)
	}

	if cfg.RevokeToken != "" {
		return RevokeToken(cfg.RevokeToken)
	}

	if cfg.MintToken {
		result, err := MintToken(cfg.Repo)
		if err != nil {
			return err
		}
		fmt.Print(result.Token)
		return nil
	}

	if cfg.GeneratePolicy {
		policyPath, err := GeneratePolicyWithHosts(cfg.Repo, cfg.MiseAllowHosts)
		if err != nil {
			return err
		}
		fmt.Print(policyPath)
		return nil
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return err
	}
	// Resolve per-run harness options once (the opencode backend, which needs
	// a usable key) before anything is created.
	opts, err := resolveHarnessOptions(cfg.Harness, cfg)
	if err != nil {
		return err
	}

	// State for cleanup
	var (
		sandboxName      string
		cleanupSandbox   string // set once creation starts; the deferred cleanup deletes it
		sandboxDeleting  bool   // a delete of sandboxName was accepted (it finishes asynchronously)
		ghCred           *GitHubCredential
		providersCreated []string
	)

	// interrupted is checked between steps; children also fail fast once ctx
	// is cancelled, but the mocked/ignored-error steps need an explicit check.
	interrupted := func() error {
		if ctx.Err() != nil {
			return ErrInterrupted
		}
		return nil
	}

	// Cleanup runs exactly once, on every exit path including SIGINT/SIGTERM
	// (the signal only cancels ctx; it never skips this deferred function).
	var cleanupOnce sync.Once
	defer cleanupOnce.Do(func() {
		// Cleanup must finish even after a signal cancelled ctx, and a second
		// Ctrl-C must not reach its children (see detached).
		ctx := detached(ctx)
		fmt.Println("--- Cleanup ---")
		if interrupted() != nil {
			fmt.Println("  Interrupted by signal.")
		}
		if cleanupSandbox != "" {
			fmt.Printf("  Deleting sandbox: %s ... ", cleanupSandbox)
			if err := execCmdSilent(ctx, "openshell", "sandbox", "delete", cleanupSandbox); err != nil {
				fmt.Printf("warning: %v (delete it manually: openshell sandbox delete %s)\n", err, cleanupSandbox)
			} else {
				fmt.Println("done")
				sandboxDeleting = true
			}
		}
		if ghCred != nil && ghCred.Revocable() {
			fmt.Print("  Revoking installation token ... ")
			if err := revokeTokenFn(ghCred.Token); err != nil {
				fmt.Printf("warning: %v\n", err)
			} else {
				fmt.Println("done")
			}
		}
		// The gateway refuses to delete a provider while the sandbox is still
		// attached, and the sandbox delete above only starts the removal.
		if sandboxDeleting && len(providersCreated) > 0 {
			fmt.Printf("  Waiting for sandbox %s to be removed ... ", sandboxName)
			if err := waitSandboxAbsent(ctx, sandboxName); err != nil {
				fmt.Printf("warning: %v\n", err)
			} else {
				fmt.Println("done")
			}
		}
		for _, prov := range providersCreated {
			fmt.Printf("  Deleting provider: %s ... ", prov)
			if err := deleteProvider(ctx, prov); err != nil {
				fmt.Printf("warning: %v (delete it manually: openshell provider delete %s)\n", err, prov)
			} else {
				fmt.Println("done")
			}
		}
		fmt.Println("  Cleanup complete.")
	})

	// Log configuration
	fmt.Printf("[agent-run] Repository: %s\n", cfg.Repo)
	fmt.Printf("[agent-run] Harness:    %s\n", cfg.Harness)
	if cfg.Model != "" {
		fmt.Printf("[agent-run] Model:      %s\n", cfg.Model)
	}

	// The sandbox name identifies the session; provider names derive from it.
	sandboxName, err = newSessionName(cfg.Harness)
	if err != nil {
		return err
	}
	fmt.Printf("[agent-run] Sandbox:    %s\n", sandboxName)

	// Step 1: Require a supported OpenShell CLI, then verify the gateway
	fmt.Println("[agent-run] Checking OpenShell version...")
	osVersion, err := checkOpenShellVersion(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("[agent-run] OpenShell:  %s (>= %s)\n", osVersion, MinOpenShellVersion)
	fmt.Println("[agent-run] Checking OpenShell gateway...")
	if err := execCmdSilent(ctx, "openshell", "status"); err != nil {
		return fmt.Errorf("OpenShell gateway is not running")
	}

	// Step 2: Resolve harness + image (images.yaml or --image)
	harness, err := LookupHarness(cfg.Harness)
	if err != nil {
		return err
	}
	image, err := resolveImage(harness, cfg.Image)
	if err != nil {
		return err
	}
	fmt.Printf("[agent-run] Image:      %s\n", image)

	// Resolve GitHub credentials
	fmt.Printf("[agent-run] Resolving GitHub credentials...\n")
	cred, err := ResolveGitHubCredential(cfg)
	if err != nil {
		return fmt.Errorf("GitHub auth failed: %w", err)
	}
	ghCred = cred
	fmt.Printf("[agent-run] GitHub auth: %s", cred.Mode)
	if !cred.ExpiresAt.IsZero() {
		fmt.Printf(" (expires %s)", cred.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Println()

	// In App mode, verify the repo is accessible via the App installation.
	// In PAT mode, trust the user (L7 policy still constrains operations).
	if cred.Mode == GitHubModeApp {
		accessible, err := IsAppRepoAccessible(cfg.Repo)
		if err != nil {
			return fmt.Errorf("failed to verify repo access: %w", err)
		}
		if !accessible {
			return fmt.Errorf("repository %q is not accessible via the GitHub App; use --list-repos to see available repos", cfg.Repo)
		}
	}

	// Step 3: Ensure provider profiles are imported (0.1.4 has no built-ins)
	fmt.Println("[agent-run] Ensuring provider profiles...")
	if err := ensureProfiles(ctx, harness.Providers(opts)); err != nil {
		return err
	}

	if err := interrupted(); err != nil {
		return err
	}

	// Step 4: Create providers (GitHub + inference)
	fmt.Println("[agent-run] Creating GitHub provider...")
	// The github-agent profile declares GITHUB_TOKEN and GH_TOKEN; 0.1.4 only
	// accepts those as stored keys, and the sandbox gets a placeholder under
	// each name (gh reads GH_TOKEN first, then GITHUB_TOKEN).
	ghProvider, err := createProvider(ctx, sandboxName, GitHubProviderType, []credentialPair{
		{"GITHUB_TOKEN", cred.Token},
		{"GH_TOKEN", cred.Token},
	})
	if err != nil {
		return fmt.Errorf("failed to create GitHub provider: %w", err)
	}
	providersCreated = append(providersCreated, ghProvider)

	inferenceFlags, inferenceProviders, err := createInferenceProviders(ctx, sandboxName, cfg.Harness, opts)
	if err != nil {
		return fmt.Errorf("failed to create inference providers: %w", err)
	}
	providersCreated = append(providersCreated, inferenceProviders...)
	providerFlags := append([]string{"--provider", ghProvider}, inferenceFlags...)

	// Resolve inference env vars
	envFlags := resolveInferenceEnv(cfg.Harness, opts)
	if len(envFlags) > 0 {
		fmt.Printf("[agent-run] Forwarding %d inference env var(s) to sandbox.\n", len(envFlags)/2)
	}

	if err := interrupted(); err != nil {
		return err
	}

	// Step 5: Generate repo-specific policy
	fmt.Printf("[agent-run] Generating sandbox policy for %s...\n", cfg.Repo)
	policyTmp, err := GeneratePolicyWithHosts(cfg.Repo, cfg.MiseAllowHosts)
	if err != nil {
		return fmt.Errorf("failed to generate policy: %w", err)
	}
	defer func() { _ = os.Remove(policyTmp) }()

	if err := interrupted(); err != nil {
		return err
	}

	// Step 6: Prepare the per-repo mise cache (degrades to none on problems)
	driver := detectDriver(ctx, cfg.Driver)
	cache, err := prepareCache(ctx, cfg, driver)
	if err != nil {
		return err
	}

	if err := interrupted(); err != nil {
		return err
	}

	// Step 7: Create sandbox from the harness image
	fmt.Printf("[agent-run] Creating sandbox '%s'...\n", sandboxName)
	buildArgs := func(driverConfigJSON string) []string {
		return buildCreateArgs(sandboxName, image, policyTmp, providerFlags, envFlags, driverConfigJSON)
	}
	cleanupSandbox = sandboxName
	if err := createSandbox(ctx, sandboxName, buildArgs, cache); err != nil {
		return fmt.Errorf("failed to create sandbox: %w", err)
	}

	if err := interrupted(); err != nil {
		return err
	}

	// Wait for sandbox to be ready
	if err := waitForSandbox(ctx, sandboxName); err != nil {
		return err
	}
	fmt.Println("[agent-run] Sandbox ready.")

	if err := interrupted(); err != nil {
		return err
	}

	// Step 8: Pre-seed harness config
	if err := harness.Preseed(ctx, sandboxName, opts); err != nil {
		return fmt.Errorf("harness config setup failed: %w", err)
	}

	if err := interrupted(); err != nil {
		return err
	}

	// Step 9a: Configure global git credential helper (must happen before clone for private repos)
	fmt.Println("[agent-run] Configuring Git credentials...")
	credHelper := `git config --global credential.helper '!f() { echo "username=x-access-token"; echo "password=${GH_TOKEN:-$GITHUB_TOKEN}"; }; f'`
	if err := execCmd(ctx, "openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c", credHelper); err != nil {
		return fmt.Errorf("git credential setup failed: %w", err)
	}

	if err := interrupted(); err != nil {
		return err
	}

	// Step 9b: Clone repository
	fmt.Printf("[agent-run] Cloning %s...\n", cfg.Repo)
	if err := execCmd(ctx, "openshell", "sandbox", "exec", "-n", sandboxName, "--",
		"git", "clone", fmt.Sprintf("https://github.com/%s.git", cfg.Repo), "/sandbox/repo"); err != nil {
		return fmt.Errorf("clone failed: %w", err)
	}

	if err := interrupted(); err != nil {
		return err
	}

	// Step 9c: Configure Git identity
	fmt.Println("[agent-run] Configuring Git identity...")
	gitConfig := fmt.Sprintf(`
		cd /sandbox/repo
		git config user.name '%s'
		git config user.email '%s'
		git config commit.gpgsign false
		git config tag.gpgsign false
	`, GitUser, GitEmail)
	if err := execCmd(ctx, "openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c", gitConfig); err != nil {
		return fmt.Errorf("git configuration failed: %w", err)
	}

	if err := interrupted(); err != nil {
		return err
	}

	// Step 10: mise toolchain install (egress via the temporary mise_bootstrap rule)
	if err := runMisePhase(ctx, cfg, sandboxName, cache); err != nil {
		return err
	}

	if err := interrupted(); err != nil {
		return err
	}

	// Step 11: Narrow egress before the agent starts. Fails closed: on error
	// the sandbox is deleted and the token revoked before returning.
	res, err := enforceNarrowing(ctx, cfg, sandboxName, ghCred)
	if err != nil {
		// Skip in the deferred cleanup only what enforceNarrowing really did;
		// a failed sandbox delete is retried there.
		if res.SandboxDeleted {
			cleanupSandbox = ""
			sandboxDeleting = true
		}
		if res.TokenRevoked {
			ghCred = nil
		}
		return err
	}
	if err := interrupted(); err != nil {
		return err
	}

	// Step 12: Launch agent
	fmt.Printf("[agent-run] Launching harness: %s\n", cfg.Harness)
	if cfg.Model != "" {
		fmt.Printf("[agent-run] Model override: %s\n", cfg.Model)
	}
	fmt.Println("[agent-run] Working directory: /sandbox/repo")
	fmt.Println("---")

	launchCmd := buildLaunchCmd(harness, cfg, opts)
	// The harness's exit status is not an agent-run failure. If SIGINT/SIGTERM
	// ended it, RunContext reports the interruption after cleanup.
	_ = execInteractive(ctx, "openshell", "sandbox", "exec", "-n", sandboxName, "--workdir", "/sandbox/repo", ttyFlag(), "--",
		"bash", "-c", launchCmd)

	fmt.Println("\n[agent-run] Harness exited. Cleaning up...")

	return nil
}

// buildCreateArgs assembles the `openshell sandbox create` arguments.
// driverConfigJSON is passed through only when non-empty.
func buildCreateArgs(name, image, policyPath string, providerFlags, envFlags []string, driverConfigJSON string) []string {
	args := []string{
		"sandbox", "create",
		"--name", name,
		"--from", image,
	}
	args = append(args, providerFlags...)
	args = append(args, "--policy", policyPath)
	args = append(args, envFlags...)
	if driverConfigJSON != "" {
		args = append(args, "--driver-config-json", driverConfigJSON)
	}
	return append(args, "--detach")
}

// stdinIsTerminal reports whether agent-run's stdin is a terminal; replaceable
// in tests.
var stdinIsTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// ttyFlag picks the PTY mode for the harness exec. An interactive run forces a
// PTY (auto-detection misses it when stdout is redirected, and the TUI then
// renders one character per line). A piped stdin must not get one: the harness
// would see a terminal instead of the pipe, so `claude -p` exits with "Input
// must be provided" instead of reading its prompt.
func ttyFlag() string {
	if stdinIsTerminal() {
		return "--tty"
	}
	return "--no-tty"
}

// buildLaunchCmd builds the bash -c command that execs the harness. The
// harness's launch env (claude's OAuth rename) runs after ~/.profile, in this
// shell only, so it applies exactly once whatever the dotfiles do.
func buildLaunchCmd(h *Harness, cfg *Config, o HarnessOptions) string {
	var parts []string
	if cfg.Model != "" && h.ModelFlag != "" {
		parts = append(parts, h.ModelFlag, shellQuote(cfg.Model))
	}
	for _, a := range cfg.PassthroughArgs {
		parts = append(parts, shellQuote(a))
	}
	env := ""
	if e := h.LaunchEnv(o); e != "" {
		env = e + "; "
	}
	return fmt.Sprintf("source ~/.profile 2>/dev/null; %sexec %s %s", env, h.Command, strings.Join(parts, " "))
}

// sandboxPhase reads the phase of sandbox name from `openshell sandbox get
// <name> -o json` (a top-level "phase", e.g. "Ready"), which describes exactly
// that sandbox. exists is false when the gateway reports it not found; err is
// set for any other failure.
func sandboxPhase(ctx context.Context, name string) (phase string, exists bool, err error) {
	out, err := execCmdOutput(ctx, "openshell", "sandbox", "get", name, "-o", "json")
	if err != nil {
		if strings.Contains(strings.ToLower(out), "not found") {
			return "", false, nil
		}
		return "", false, fmt.Errorf("openshell sandbox get %s failed: %w", name, err)
	}
	var doc struct {
		Name  string `json:"name"`
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(out)), &doc); err != nil {
		return "", false, fmt.Errorf("could not parse 'openshell sandbox get %s' output: %w", name, err)
	}
	if doc.Name != "" && doc.Name != name {
		return "", false, fmt.Errorf("openshell sandbox get %s returned sandbox %q", name, doc.Name)
	}
	return doc.Phase, true, nil
}

// sandboxReadyTimeout is how long waitForSandbox waits. The first run of a new
// image digest pulls a multi-GB harness image (and a kubernetes sandbox may
// wait for its PVC), so the default is generous; AGENT_RUN_READY_TIMEOUT
// (a Go duration such as 15m) overrides it.
func sandboxReadyTimeout() time.Duration {
	if v := os.Getenv("AGENT_RUN_READY_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		fmt.Printf("[agent-run] WARN: ignoring invalid AGENT_RUN_READY_TIMEOUT %q (want a duration such as 15m)\n", v)
	}
	return SandboxReadyTimeout
}

// sandboxReadyPoll is waitForSandbox's poll interval (a var for tests).
var sandboxReadyPoll = SandboxReadyPoll

// waitForSandbox waits until this sandbox's own phase is Ready (see
// sandboxPhase): another sandbox being Ready, or a name that merely contains
// this one, never counts.
func waitForSandbox(ctx context.Context, name string) error {
	limit := sandboxReadyTimeout()
	deadline := time.Now().Add(limit)
	for {
		var last string
		phase, exists, err := sandboxPhase(ctx, name)
		switch {
		case err != nil:
			last = err.Error()
		case !exists:
			last = "not found"
		case strings.EqualFold(phase, "Ready"):
			return nil
		default:
			last = phase
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sandbox %s did not become ready within %v (last status: %s; the first pull of a harness image can be slow, raise the limit with AGENT_RUN_READY_TIMEOUT=20m)", name, limit, last)
		}
		select {
		case <-ctx.Done():
			return ErrInterrupted
		case <-time.After(sandboxReadyPoll):
		}
	}
}

// printDiagnostics prints diagnostic information.
func printDiagnostics(ctx context.Context, cfg *Config) error {
	fmt.Println("=== agent-run diagnostics ===")

	// OpenShell version + minimum-version gate
	fmt.Print("  OpenShell version:  ")
	out, _ := execCmdOutput(ctx, "openshell", "--version")
	if out != "" {
		fmt.Println(strings.TrimSpace(out))
		if _, err := gateOpenShellVersion(out); err != nil {
			fmt.Printf("  Version gate:       FAIL (%v)\n", err)
		} else {
			fmt.Printf("  Version gate:       ok (>= %s)\n", MinOpenShellVersion)
		}
	} else {
		fmt.Println("not found")
		fmt.Printf("  Version gate:       FAIL (requires >= %s)\n", MinOpenShellVersion)
	}

	// Resolved image refs (--image applies to the selected harness only)
	fmt.Println("  Harness images:")
	for _, name := range HarnessNames() {
		h := harnesses[name]
		override := ""
		if cfg != nil && cfg.Harness == name {
			override = cfg.Image
		}
		ref, err := resolveImage(h, override)
		if err != nil {
			ref = "ERROR: " + err.Error()
		}
		fmt.Printf("    %-10s %s\n", name+":", ref)
	}

	// Compute driver
	driverOverride := ""
	if cfg != nil {
		driverOverride = cfg.Driver
	}
	driver := detectDriver(ctx, driverOverride)
	fmt.Printf("  Compute driver:     %s\n", driver)
	fmt.Printf("  Driver config:      cache mounts need gateway setting %s\n", allowDriverConfigHint(driver))

	// mise cache volume (docker/podman only; PVCs live in the sandbox namespace)
	if cfg != nil && cfg.Repo != "" && (driver == "docker" || driver == "podman") {
		if owner, name, err := ParseRepo(cfg.Repo); err == nil {
			vol := cacheVolumeName(owner, name)
			state := "absent (created on first run)"
			if execCmdSilent(ctx, driver, "volume", "inspect", vol) == nil {
				state = "present"
			}
			fmt.Printf("  mise cache volume:  %s (%s)\n", vol, state)
		}
	}

	// opencode backend selection
	{
		var env []string
		inference := ""
		if cfg != nil {
			env, inference = cfg.EnvVars, cfg.Inference
		}
		backend, err := resolveOpencodeBackend(inference, env)
		if err != nil {
			backend = "unavailable (" + err.Error() + ")"
		}
		fmt.Printf("  opencode backend:   %s\n", backend)
		if resolveCredential("LITELLM_BASE_URL", env) != "" {
			fmt.Println("  LITELLM_BASE_URL:   set")
		} else {
			fmt.Println("  LITELLM_BASE_URL:   not set")
		}
	}

	fmt.Println("  Token minting:      Go-native (repo-scoped)")
	fmt.Printf("  Installation ID:    %s\n", InstallationID)
	fmt.Printf("  GitHub App slug:    %s\n", AppSlug)
	fmt.Printf("  GitHub App ID:      %s\n", AppID)
	fmt.Println("  Repo discovery:     dynamic (via GitHub App installation)")
	fmt.Printf("  Supported harnesses: %s\n", strings.Join(HarnessNames(), ", "))

	// GitHub App keychain
	fmt.Println("\n  GitHub App credentials:")
	fmt.Printf("    %s: ", KeychainService)
	if keychainExists(ctx, KeychainService) {
		fmt.Println("present")
	} else {
		fmt.Println("MISSING")
	}

	// Inference env vars
	fmt.Println("\n  Inference env vars (host):")
	allInferenceVars := []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_VERTEX",
		"ANTHROPIC_VERTEX_PROJECT_ID", "CLOUD_ML_REGION",
		"ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
		"LITELLM_API_KEY", "LITELLM_BASE_URL",
	}
	for _, key := range allInferenceVars {
		val := os.Getenv(key)
		if val != "" {
			// Redact sensitive values
			display := val
			if strings.Contains(strings.ToLower(key), "key") || strings.Contains(strings.ToLower(key), "token") {
				if len(display) > 8 {
					display = display[:4] + "..." + display[len(display)-4:]
				} else {
					display = "****"
				}
			}
			fmt.Printf("    %-45s %s\n", key, display)
		}
	}

	// Installed agents
	fmt.Println("\n  Installed agents (host):")
	for _, cmd := range []string{"claude", "pi", "codex"} {
		fmt.Printf("    %-8s: ", cmd)
		if path, err := exec.LookPath(cmd); err == nil {
			fmt.Println(path)
		} else {
			fmt.Println("not found")
		}
	}

	// Gateway status
	fmt.Println("\n  Gateway status:")
	out, _ = execCmdOutput(ctx, "openshell", "status")
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			fmt.Printf("    %s\n", line)
		}
	}

	// Active sandboxes
	fmt.Println("\n  Active sandboxes:")
	out, _ = execCmdOutput(ctx, "openshell", "sandbox", "list")
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			fmt.Printf("    %s\n", line)
		}
	}

	// Providers
	fmt.Println("\n  Providers:")
	out, _ = execCmdOutput(ctx, "openshell", "provider", "list")
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			fmt.Printf("    %s\n", line)
		}
	}

	fmt.Println("\n  NOTE: Diagnostics never print: private key, tokens, model API keys")
	return nil
}

// keychainExists checks if a keychain entry exists (macOS only).
func keychainExists(ctx context.Context, service string) bool {
	return execCmdSilent(context.Background(), "security", "find-generic-password", "-s", service, "-a", os.Getenv("USER")) == nil
}

// newSessionName returns a fresh session (sandbox) name, a-<harness>-<8 hex>.
// The random suffix keeps concurrent and repeated runs apart; a collision
// makes `sandbox create` fail rather than reuse a sandbox.
func newSessionName(harness string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating session name: %w", err)
	}
	return fmt.Sprintf("a-%s-%s", harness, hex.EncodeToString(b)), nil
}
