package agentrun

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Constants for the agent run configuration.
const (
	AppID   = "4720923"
	AppSlug = "jlaska-agent"

	GitUser  = "jlaska-agent[bot]"
	GitEmail = "jlaska-agent[bot]@users.noreply.github.com"

	GitHubProviderName = "github-agent"

	SandboxReadyTimeout = 30 * time.Second
	SandboxReadyPoll    = 1 * time.Second
)

// Run executes the agent-run command with the given configuration.
func Run(cfg *Config) error {
	if cfg.Help || cfg.ListRepos {
		return nil
	}

	if cfg.Diag {
		return printDiagnostics(cfg)
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
		repoRoot := findRepoRoot()
		policyPath, err := GeneratePolicyWithHosts(cfg.Repo, repoRoot, cfg.MiseAllowHosts)
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

	// Find repo root for templates/profiles
	repoRoot := findRepoRoot()

	// State for cleanup
	var (
		sandboxName      string
		ghCred           *GitHubCredential
		providersCreated []string
	)

	// Cleanup function
	defer func() {
		fmt.Println("--- Cleanup ---")
		if sandboxName != "" {
			fmt.Printf("  Deleting sandbox: %s ... ", sandboxName)
			_ = execCmdSilent("openshell", "sandbox", "delete", sandboxName)
			fmt.Println("done")
		}
		if ghCred != nil && ghCred.Revocable() {
			fmt.Print("  Revoking installation token ... ")
			if err := revokeTokenFn(ghCred.Token); err != nil {
				fmt.Printf("warning: %v\n", err)
			} else {
				fmt.Println("done")
			}
		}
		for _, prov := range providersCreated {
			fmt.Printf("  Deleting provider: %s ... ", prov)
			_ = execCmdSilent("openshell", "provider", "delete", prov)
			fmt.Println("done")
		}
		fmt.Println("  Cleanup complete.")
	}()

	// Log configuration
	fmt.Printf("[agent-run] Repository: %s\n", cfg.Repo)
	fmt.Printf("[agent-run] Harness:    %s\n", cfg.Harness)
	if cfg.Model != "" {
		fmt.Printf("[agent-run] Model:      %s\n", cfg.Model)
	}

	// Generate sandbox name
	sandboxName = fmt.Sprintf("a-%s-%d", cfg.Harness, time.Now().Unix()%1000000)
	fmt.Printf("[agent-run] Sandbox:    %s\n", sandboxName)

	// Step 1: Require a supported OpenShell CLI, then verify the gateway
	fmt.Println("[agent-run] Checking OpenShell version...")
	osVersion, err := checkOpenShellVersion()
	if err != nil {
		return err
	}
	fmt.Printf("[agent-run] OpenShell:  %s (>= %s)\n", osVersion, MinOpenShellVersion)
	fmt.Println("[agent-run] Checking OpenShell gateway...")
	if err := execCmdSilent("openshell", "status"); err != nil {
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
	opts := HarnessOptions{Inference: cfg.Inference, Model: cfg.Model, ExplicitEnv: cfg.EnvVars}
	harnessProfiles, err := harness.ProvidersFor(opts)
	if err != nil {
		return err
	}
	if err := ensureProfiles(harnessProfiles); err != nil {
		return err
	}

	// Step 4: Create providers (GitHub + inference)
	fmt.Println("[agent-run] Creating GitHub provider...")
	_ = execCmd("openshell", "provider", "delete", GitHubProviderName)
	if err := execCmd("openshell", "provider", "create",
		"--name", GitHubProviderName,
		"--type", "github-agent",
		"--credential", fmt.Sprintf("api_token=%s", cred.Token),
	); err != nil {
		return fmt.Errorf("failed to create GitHub provider: %w", err)
	}
	providersCreated = append(providersCreated, GitHubProviderName)

	inferenceFlags, inferenceProviders, err := createInferenceProvidersFor(cfg.Harness, cfg.Inference, cfg.EnvVars)
	if err != nil {
		return fmt.Errorf("failed to create inference providers: %w", err)
	}
	providersCreated = append(providersCreated, inferenceProviders...)

	// Resolve inference env vars
	envFlags := resolveInferenceEnv(cfg.Harness, cfg.EnvVars)
	if len(envFlags) > 0 {
		fmt.Printf("[agent-run] Forwarding %d inference env var(s) to sandbox.\n", len(envFlags)/2)
	}

	// Step 5: Generate repo-specific policy
	fmt.Printf("[agent-run] Generating sandbox policy for %s...\n", cfg.Repo)
	policyTmp, err := GeneratePolicyWithHosts(cfg.Repo, repoRoot, cfg.MiseAllowHosts)
	if err != nil {
		return fmt.Errorf("failed to generate policy: %w", err)
	}
	defer func() { _ = os.Remove(policyTmp) }()

	// Step 6: Prepare the per-repo mise cache (degrades to none on problems)
	driver := detectDriver(cfg.Driver)
	cache, err := prepareCache(cfg, driver)
	if err != nil {
		return err
	}

	// Step 7: Create sandbox from the harness image
	fmt.Printf("[agent-run] Creating sandbox '%s'...\n", sandboxName)
	buildArgs := func(driverConfigJSON string) []string {
		return buildCreateArgs(sandboxName, image, policyTmp, inferenceFlags, envFlags, driverConfigJSON)
	}
	if err := createSandbox(sandboxName, buildArgs, cache); err != nil {
		return fmt.Errorf("failed to create sandbox: %w", err)
	}

	// Wait for sandbox to be ready
	if err := waitForSandbox(sandboxName); err != nil {
		return err
	}
	fmt.Println("[agent-run] Sandbox ready.")

	// Step 8: Map provider credentials to harness env vars + pre-seed config
	if err := harness.InitHookFor(opts)(sandboxName); err != nil {
		return fmt.Errorf("sandbox credential setup failed: %w", err)
	}
	if err := harness.PreseedFor(opts)(sandboxName); err != nil {
		return fmt.Errorf("harness config setup failed: %w", err)
	}

	// Step 9a: Configure global git credential helper (must happen before clone for private repos)
	fmt.Println("[agent-run] Configuring Git credentials...")
	credHelper := `git config --global credential.helper '!f() { echo "username=x-access-token"; echo "password=${api_token:-$GH_TOKEN}"; }; f'`
	if err := execCmd("openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c", credHelper); err != nil {
		return fmt.Errorf("git credential setup failed: %w", err)
	}

	// Step 9b: Clone repository
	fmt.Printf("[agent-run] Cloning %s...\n", cfg.Repo)
	if err := execCmd("openshell", "sandbox", "exec", "-n", sandboxName, "--",
		"git", "clone", fmt.Sprintf("https://github.com/%s.git", cfg.Repo), "/sandbox/repo"); err != nil {
		return fmt.Errorf("clone failed: %w", err)
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
	if err := execCmd("openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c", gitConfig); err != nil {
		return fmt.Errorf("git configuration failed: %w", err)
	}

	// Step 10: mise toolchain install (egress via the temporary mise_bootstrap rule)
	if err := runMisePhase(cfg, sandboxName, cache); err != nil {
		return err
	}

	// Step 11: Narrow egress before the agent starts. Fails closed: on error
	// the sandbox is deleted and the token revoked before returning.
	if err := enforceNarrowing(cfg, sandboxName, ghCred); err != nil {
		sandboxName, ghCred = "", nil // already cleaned up; skip in deferred cleanup
		return err
	}

	// Step 12: Launch agent
	fmt.Printf("[agent-run] Launching harness: %s\n", cfg.Harness)
	if cfg.Model != "" {
		fmt.Printf("[agent-run] Model override: %s\n", cfg.Model)
	}
	fmt.Println("[agent-run] Working directory: /sandbox/repo")
	fmt.Println("---")

	launchCmd := buildLaunchCmd(harness, cfg)
	_ = execCmd("openshell", "sandbox", "exec", "-n", sandboxName, "--workdir", "/sandbox/repo", "--tty", "--",
		"bash", "-c", launchCmd)

	fmt.Println("\n[agent-run] Harness exited. Cleaning up...")

	return nil
}

// buildCreateArgs assembles the `openshell sandbox create` arguments.
// driverConfigJSON is passed through only when non-empty.
func buildCreateArgs(name, image, policyPath string, inferenceFlags, envFlags []string, driverConfigJSON string) []string {
	args := []string{
		"sandbox", "create",
		"--name", name,
		"--from", image,
		"--provider", GitHubProviderName,
	}
	args = append(args, inferenceFlags...)
	args = append(args, "--policy", policyPath)
	args = append(args, envFlags...)
	if driverConfigJSON != "" {
		args = append(args, "--driver-config-json", driverConfigJSON)
	}
	return append(args, "--detach")
}

// buildLaunchCmd builds the bash -c command that execs the harness.
func buildLaunchCmd(h *Harness, cfg *Config) string {
	var parts []string
	if cfg.Model != "" && h.ModelFlag != "" {
		parts = append(parts, h.ModelFlag, shellQuote(cfg.Model))
	}
	for _, a := range cfg.PassthroughArgs {
		parts = append(parts, shellQuote(a))
	}
	return fmt.Sprintf("source ~/.profile 2>/dev/null; export GH_TOKEN=$api_token; exec %s %s", h.Command, strings.Join(parts, " "))
}

// waitForSandbox waits for a sandbox to become ready.
func waitForSandbox(name string) error {
	timeout := time.After(SandboxReadyTimeout)
	ticker := time.NewTicker(SandboxReadyPoll)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("sandbox did not become ready within %v", SandboxReadyTimeout)
		case <-ticker.C:
			out, _ := execCmdOutput("openshell", "sandbox", "list")
			if strings.Contains(out, name) && strings.Contains(out, "Ready") {
				return nil
			}
		}
	}
}

// printDiagnostics prints diagnostic information.
func printDiagnostics(cfg *Config) error {
	fmt.Println("=== agent-run diagnostics ===")

	// OpenShell version + minimum-version gate
	fmt.Print("  OpenShell version:  ")
	out, _ := execCmdOutput("openshell", "--version")
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
	driver := detectDriver(driverOverride)
	fmt.Printf("  Compute driver:     %s\n", driver)
	fmt.Printf("  Driver config:      cache mounts need gateway setting %s\n", allowDriverConfigHint(driver))

	// mise cache volume (docker/podman only; PVCs live in the sandbox namespace)
	if cfg != nil && cfg.Repo != "" && (driver == "docker" || driver == "podman") {
		if owner, name, err := ParseRepo(cfg.Repo); err == nil {
			vol := cacheVolumeName(owner, name)
			state := "absent (created on first run)"
			if execCmdSilent(driver, "volume", "inspect", vol) == nil {
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
	if keychainExists(KeychainService) {
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
	out, _ = execCmdOutput("openshell", "status")
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			fmt.Printf("    %s\n", line)
		}
	}

	// Active sandboxes
	fmt.Println("\n  Active sandboxes:")
	out, _ = execCmdOutput("openshell", "sandbox", "list")
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			fmt.Printf("    %s\n", line)
		}
	}

	// Providers
	fmt.Println("\n  Providers:")
	out, _ = execCmdOutput("openshell", "provider", "list")
	for _, line := range strings.Split(out, "\n") {
		if line != "" {
			fmt.Printf("    %s\n", line)
		}
	}

	fmt.Println("\n  NOTE: Diagnostics never print: private key, tokens, model API keys")
	return nil
}

// keychainExists checks if a keychain entry exists (macOS only).
func keychainExists(service string) bool {
	return execCmdSilent("security", "find-generic-password", "-s", service, "-a", os.Getenv("USER")) == nil
}

// findRepoRoot finds the repository root directory.
func findRepoRoot() string {
	if wd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(wd, "openshell")); err == nil {
			return wd
		}
	}
	return "."
}
