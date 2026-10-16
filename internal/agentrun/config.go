package agentrun

import (
	"flag"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Supported values for --inference and --driver.
var (
	inferenceBackends = []string{"litellm", "anthropic"}
	computeDrivers    = []string{"docker", "podman", "kubernetes", "vm"}
)

// Config holds parsed command-line arguments and configuration.
type Config struct {
	// Positional arguments
	Repo    string
	Harness string

	// Optional flags
	Model          string
	EnvVars        []string
	GitHubToken    string
	Diag           bool
	ListRepos      bool
	Help           bool
	MintToken      bool
	RevokeToken    string
	GeneratePolicy bool

	// Image/toolchain flags
	Image          string   // --image: override the harness image ref
	NoMise         bool     // --no-mise: skip toolchain install
	FreshCache     bool     // --fresh-cache: clear the mise cache before install
	NoCache        bool     // --no-cache: don't mount a mise cache
	MiseAllowHosts []string // --mise-allow-host (repeatable, host[:port])
	Inference      string   // --inference litellm|anthropic (opencode only)
	Driver         string   // --driver: override compute driver detection

	// Passthrough args (everything after --)
	PassthroughArgs []string
}

// envVarCollector implements flag.Value for repeatable --env flags.
type envVarCollector struct {
	values *[]string
}

func (e *envVarCollector) String() string { return "" }
func (e *envVarCollector) Set(val string) error {
	if !strings.Contains(val, "=") {
		return fmt.Errorf("env var must be in KEY=VALUE format: %q", val)
	}
	*e.values = append(*e.values, val)
	return nil
}

// hostPortCollector implements flag.Value for repeatable --mise-allow-host flags.
type hostPortCollector struct {
	values *[]string
}

func (h *hostPortCollector) String() string { return "" }
func (h *hostPortCollector) Set(val string) error {
	if err := validateHostPort(val); err != nil {
		return err
	}
	*h.values = append(*h.values, val)
	return nil
}

var hostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)

// validateHostPort checks a host[:port] value: a bare hostname (no scheme,
// path or wildcard) with an optional port in 1-65535.
func validateHostPort(val string) error {
	host, port, hasPort := strings.Cut(val, ":")
	if !hostnamePattern.MatchString(host) {
		return fmt.Errorf("invalid host %q: expected host[:port] (no scheme, path or wildcard)", val)
	}
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid port in %q: must be 1-65535", val)
		}
	}
	return nil
}

func oneOf(val string, allowed []string) bool {
	for _, a := range allowed {
		if val == a {
			return true
		}
	}
	return false
}

// ParseArgs parses command-line arguments into a Config.
// Flags may appear before, after, or between positional arguments.
func ParseArgs(args []string) (*Config, error) {
	cfg := &Config{}

	fs := flag.NewFlagSet("agent-run", flag.ContinueOnError)
	fs.StringVar(&cfg.Model, "model", "", "override model name")
	fs.Var(&envVarCollector{values: &cfg.EnvVars}, "env", "pass env var to sandbox (KEY=VAL, repeatable)")
	fs.StringVar(&cfg.GitHubToken, "github-token", "", "GitHub PAT (alternative to App token)")
	fs.BoolVar(&cfg.Diag, "diag", false, "print diagnostic information")
	fs.BoolVar(&cfg.ListRepos, "list-repos", false, "list approved repositories")
	fs.BoolVar(&cfg.Help, "help", false, "print usage")
	fs.BoolVar(&cfg.MintToken, "mint-token", false, "mint a repo-scoped token and print it")
	fs.StringVar(&cfg.RevokeToken, "revoke-token", "", "revoke a previously minted token")
	fs.BoolVar(&cfg.GeneratePolicy, "generate-policy", false, "generate a repo-scoped policy file and print its path")

	fs.StringVar(&cfg.Image, "image", "", "override the harness image ref")
	fs.BoolVar(&cfg.NoMise, "no-mise", false, "skip mise toolchain install")
	fs.BoolVar(&cfg.FreshCache, "fresh-cache", false, "clear the mise cache before install")
	fs.BoolVar(&cfg.NoCache, "no-cache", false, "do not mount a mise cache")
	fs.Var(&hostPortCollector{values: &cfg.MiseAllowHosts}, "mise-allow-host", "extra host[:port] allowed during mise install (repeatable)")
	fs.StringVar(&cfg.Inference, "inference", "", "inference backend: litellm|anthropic (opencode only)")
	fs.StringVar(&cfg.Driver, "driver", "", "compute driver override: docker|podman|kubernetes|vm")

	// Split at -- separator: everything after becomes passthrough args.
	for i, a := range args {
		if a == "--" {
			cfg.PassthroughArgs = args[i+1:]
			args = args[:i]
			break
		}
	}

	// Separate flags from positional args so flags work in any position.
	var flagArgs, positional []string
	flagsWithValue := map[string]bool{
		"--model": true, "--revoke-token": true, "--env": true, "--github-token": true,
		"--image": true, "--mise-allow-host": true, "--inference": true, "--driver": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if flagsWithValue[a] && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
		} else {
			positional = append(positional, a)
		}
	}

	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}

	if cfg.Help {
		printUsage()
		return cfg, nil
	}

	if cfg.ListRepos {
		printRepos()
		return cfg, nil
	}

	if cfg.RevokeToken != "" {
		return cfg, nil
	}

	if cfg.MintToken || cfg.GeneratePolicy {
		if len(positional) < 1 {
			if cfg.MintToken {
				return nil, fmt.Errorf("usage: agent-run --mint-token <owner/repo>")
			}
			return nil, fmt.Errorf("usage: agent-run --generate-policy <owner/repo>")
		}
		cfg.Repo = positional[0]
		if _, _, err := ParseRepo(cfg.Repo); err != nil {
			return nil, fmt.Errorf("invalid repository: %w", err)
		}
		return cfg, nil
	}

	if len(positional) == 0 {
		cfg.Help = true
		printUsage()
		return cfg, nil
	}

	if len(positional) < 2 {
		return nil, fmt.Errorf("usage: agent-run <owner/repo> <harness> [options] [-- <harness-args>...]")
	}

	cfg.Repo = positional[0]
	cfg.Harness = positional[1]

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate checks the configuration for validity.
func (c *Config) Validate() error {
	if c.Repo == "" {
		return fmt.Errorf("repository is required")
	}

	if _, _, err := ParseRepo(c.Repo); err != nil {
		return fmt.Errorf("invalid repository: %w", err)
	}

	if c.Harness == "" {
		return fmt.Errorf("harness is required")
	}

	if _, err := LookupHarness(c.Harness); err != nil {
		return err
	}

	if c.Inference != "" && !oneOf(c.Inference, inferenceBackends) {
		return fmt.Errorf("invalid --inference %q; supported: %s", c.Inference, strings.Join(inferenceBackends, ", "))
	}
	if c.Inference != "" && c.Harness != HarnessOpencode {
		return fmt.Errorf("--inference applies to the opencode harness only (harness is %q)", c.Harness)
	}
	if c.Harness == HarnessOpencode {
		// Fail before any sandbox exists if no usable backend/key is available.
		if _, err := resolveOpencodeBackend(c.Inference, c.EnvVars); err != nil {
			return err
		}
	}
	// --fresh-cache clears the persistent per-repo cache; --no-cache means there
	// is none, so the combination is contradictory (and would silently do nothing).
	if c.FreshCache && c.NoCache {
		return fmt.Errorf("--fresh-cache and --no-cache are mutually exclusive: --no-cache mounts no persistent cache, so there is nothing to clear")
	}
	if c.Driver != "" && !oneOf(c.Driver, computeDrivers) {
		return fmt.Errorf("invalid --driver %q; supported: %s", c.Driver, strings.Join(computeDrivers, ", "))
	}
	if strings.ContainsAny(c.Image, " \t\n") {
		return fmt.Errorf("invalid --image %q: must not contain whitespace", c.Image)
	}
	for _, h := range c.MiseAllowHosts {
		if err := validateHostPort(h); err != nil {
			return err
		}
	}

	return nil
}

func printUsage() {
	fmt.Println("Usage: agent-run <owner/repo> <harness> [options] [-- <harness-args>...]")
	fmt.Println()
	fmt.Printf("  Harnesses:  %s\n", strings.Join(HarnessNames(), ", "))
	fmt.Println()
	fmt.Println("  Inference auth is configured via environment variables:")
	fmt.Println("    ANTHROPIC_API_KEY          Direct API key")
	fmt.Println("    CLAUDE_CODE_OAUTH_TOKEN    Max/Pro subscription (from claude setup-token)")
	fmt.Println("    ANTHROPIC_BASE_URL         Custom endpoint (e.g. LiteLLM proxy)")
	fmt.Println("    CLAUDE_CODE_USE_VERTEX=1   Vertex AI mode")
	fmt.Println()
	fmt.Println("  --model <m>     Override model (passed to harness CLI)")
	fmt.Println("  --env KEY=VAL   Pass env var to sandbox (repeatable)")
	fmt.Println("  --image <ref>   Override the harness image (default: pinned in images.yaml)")
	fmt.Println("  --no-mise       Skip mise toolchain install")
	fmt.Println("  --fresh-cache   Clear the mise cache before install")
	fmt.Println("  --no-cache      Do not mount a mise cache")
	fmt.Println("  --mise-allow-host host[:port]")
	fmt.Println("                  Extra host allowed during mise install (repeatable)")
	fmt.Println("  --inference litellm|anthropic")
	fmt.Println("                  Inference backend (opencode only)")
	fmt.Println("  --driver docker|podman|kubernetes|vm")
	fmt.Println("                  Override compute driver detection")
	fmt.Println("  --diag          Print diagnostic info")
	fmt.Println("  --github-token <pat>\n                  GitHub PAT (alternative to the App token)")
	fmt.Println("  --list-repos    List approved repositories")
	fmt.Println("  --mint-token    Mint a repo-scoped token and print it")
	fmt.Println("  --revoke-token <t>\n                  Revoke a previously minted token")
	fmt.Println("  --generate-policy\n                  Generate a repo-scoped policy file and print its path")
	fmt.Println("  --help          Show this help")
}

func printRepos() {
	repos, err := ListAppRepos()
	if err != nil {
		fmt.Printf("Could not list repos: %v\n", err)
		fmt.Println("(Requires GitHub App credentials in Keychain)")
		return
	}
	fmt.Println("Accessible repositories (via GitHub App):")
	for _, repo := range repos {
		fmt.Printf("  %s\n", repo)
	}
}
