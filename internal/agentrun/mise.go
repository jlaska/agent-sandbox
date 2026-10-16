package agentrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Paths and labels shared by the mise phase and the cache.
const (
	sandboxRepoDir = "/sandbox/repo"
	// miseCacheMount is the per-repo cache mount target. It is a subpath of the
	// workspace root (/sandbox), never /sandbox itself: OpenShell rejects mounts
	// that replace the workspace root.
	miseCacheMount = "/sandbox/.mise-cache"

	// Resource-admission labels a volume/PVC must carry before OpenShell 0.1.4
	// lets a sandbox attach it (crates/openshell-core/src/resource_admission.rs).
	labelAttachable          = "openshell.ai/sandbox-attachable"
	labelAttachableWorkspace = "openshell.ai/sandbox-attachable-workspace"

	// Cache modes.
	cacheModeNone   = "none"
	cacheModeVolume = "volume"
	cacheModePVC    = "pvc"
)

// miseConfigCandidates are the files probed (relative to the repo root) to
// decide whether the repo wants a mise toolchain (D11).
var miseConfigCandidates = []string{
	"mise.toml", ".mise.toml", "mise/config.toml",
	".config/mise.toml", ".config/mise/config.toml", ".tool-versions",
}

// cachePlan is the outcome of step 6.
type cachePlan struct {
	Mode             string // cacheModeNone | cacheModeVolume | cacheModePVC
	Volume           string // volume / PVC name (empty for none)
	DriverConfigJSON string // --driver-config-json value ("" = none)
	Driver           string
}

func noCache(driver string) *cachePlan { return &cachePlan{Mode: cacheModeNone, Driver: driver} }

// ---- cache naming -----------------------------------------------------------

var nonNameChars = regexp.MustCompile(`[^a-z0-9-]`)

// maxVolumeNameLen keeps the name a valid DNS-1123 label (Kubernetes volume
// names) and short enough for every engine.
const maxVolumeNameLen = 63

// cacheVolumeName returns mise-cache-<owner>-<repo>, lower-cased with every
// character outside [a-z0-9-] replaced by '-'. If sanitising was lossy (dots,
// underscores, trailing '-') or the name exceeds 63 chars, it is truncated and
// suffixed with "-<8 hex of sha256(owner/repo)>" so distinct repos cannot
// collide ("a.b/c" vs "a-b/c").
func cacheVolumeName(owner, repo string) string {
	lo, lr := strings.ToLower(owner), strings.ToLower(repo)
	name := "mise-cache-" + nonNameChars.ReplaceAllString(lo, "-") + "-" + nonNameChars.ReplaceAllString(lr, "-")
	lossy := nonNameChars.MatchString(lo) || nonNameChars.MatchString(lr) || strings.HasSuffix(name, "-")
	if !lossy && len(name) <= maxVolumeNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(lo + "/" + lr))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	if len(name) > maxVolumeNameLen-len(suffix) {
		name = name[:maxVolumeNameLen-len(suffix)]
	}
	return strings.TrimRight(name, "-") + suffix
}

// openshellWorkspace is the OpenShell workspace the CLI targets:
// OPENSHELL_WORKSPACE, else "default" (docs/how-it-works/workspaces.mdx).
func openshellWorkspace() string {
	if w := strings.TrimSpace(os.Getenv("OPENSHELL_WORKSPACE")); w != "" {
		return w
	}
	return "default"
}

// ---- driver-config JSON -----------------------------------------------------

// buildDriverConfigJSON returns the --driver-config-json value mounting the
// named volume/PVC read-write at miseCacheMount, or "" for drivers without
// mount support. Shapes verified against OpenShell v0.1.4:
//
//	docker/podman: {"<driver>":{"mounts":[{"type":"volume","source":N,"target":T,"read_only":false}]}}
//	kubernetes:    {"kubernetes":{"volumes":[{"name":..,"persistent_volume_claim":{"claim_name":N,"read_only":false}}],
//	                "containers":{"agent":{"volume_mounts":[{"name":..,"mount_path":T,"read_only":false}]}}}}
//
// read_only defaults to true upstream, so it is always set explicitly.
func buildDriverConfigJSON(driver, name string) (string, error) {
	var v any
	switch driver {
	case "docker", "podman":
		v = map[string]any{driver: map[string]any{"mounts": []any{
			orderedMap{{"type", "volume"}, {"source", name}, {"target", miseCacheMount}, {"read_only", false}},
		}}}
	case "kubernetes":
		v = map[string]any{"kubernetes": orderedMap{
			{"volumes", []any{orderedMap{
				{"name", "mise-cache"},
				{"persistent_volume_claim", orderedMap{{"claim_name", name}, {"read_only", false}}},
			}}},
			{"containers", map[string]any{"agent": map[string]any{"volume_mounts": []any{orderedMap{
				{"name", "mise-cache"}, {"mount_path", miseCacheMount}, {"read_only", false},
			}}}}},
		}}
	default:
		return "", fmt.Errorf("driver %q has no mount support", driver)
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// orderedMap marshals as a JSON object with keys in slice order (stable
// golden files and readable output).
type orderedMap [][2]any

func (o orderedMap) MarshalJSON() ([]byte, error) {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			sb.WriteByte(',')
		}
		k, _ := json.Marshal(kv[0])
		v, err := json.Marshal(kv[1])
		if err != nil {
			return nil, err
		}
		sb.Write(k)
		sb.WriteByte(':')
		sb.Write(v)
	}
	sb.WriteByte('}')
	return []byte(sb.String()), nil
}

// ---- step 6: prepare cache --------------------------------------------------

// prepareCache is step 6: decide whether and how to mount the per-repo mise
// cache. It never fails the run for cache problems: it degrades to no cache and
// says why. (The returned error is reserved for programming errors.)
func prepareCache(cfg *Config, driver string) (*cachePlan, error) {
	if cfg.NoCache {
		fmt.Println("[agent-run] mise cache: disabled (--no-cache); tools are installed into the sandbox only")
		return noCache(driver), nil
	}
	owner, repo, err := ParseRepo(cfg.Repo)
	if err != nil {
		return nil, err
	}
	name := cacheVolumeName(owner, repo)
	workspace := openshellWorkspace()

	switch driver {
	case "docker", "podman":
		if reason := ensureCacheVolume(driver, name, workspace); reason != "" {
			fmt.Printf("[agent-run] WARN: mise cache: %s; continuing without a cache\n", reason)
			return noCache(driver), nil
		}
	case "kubernetes":
		printPVCHint(name, workspace)
	case "vm":
		fmt.Println("[agent-run] mise cache: none (the microVM driver does not support mounts); tools are installed into the sandbox only")
		return noCache(driver), nil
	default:
		fmt.Printf("[agent-run] mise cache: none (compute driver %q unknown or unsupported; use --driver to override); tools are installed into the sandbox only\n", driver)
		return noCache(driver), nil
	}

	js, err := buildDriverConfigJSON(driver, name)
	if err != nil {
		return nil, err
	}
	mode := cacheModeVolume
	if driver == "kubernetes" {
		mode = cacheModePVC
	}
	fmt.Printf("[agent-run] mise cache: %s %s -> %s (driver %s)\n", mode, name, miseCacheMount, driver)
	return &cachePlan{Mode: mode, Volume: name, DriverConfigJSON: js, Driver: driver}, nil
}

// ensureCacheVolume makes sure a labelled named volume exists in the engine
// the gateway driver uses. It returns "" on success or a human reason on
// failure. An existing volume with wrong labels cannot be relabelled and would
// be refused by resource admission, so that is a failure too.
func ensureCacheVolume(engine, name, workspace string) string {
	out, err := execCmdOutput(engine, "volume", "inspect", "--format", "{{json .Labels}}", name)
	if err == nil {
		var labels map[string]string
		if json.Unmarshal([]byte(extractJSONObjectOrNull(out)), &labels) != nil ||
			labels[labelAttachable] != "true" || labels[labelAttachableWorkspace] != workspace {
			return fmt.Sprintf("%s volume %s exists without labels %s=true and %s=%s (labels cannot be changed in place; run '%s volume rm %s' and retry)",
				engine, name, labelAttachable, labelAttachableWorkspace, workspace, engine, name)
		}
		return ""
	}
	if err := execCmd(engine, "volume", "create",
		"--label", labelAttachable+"=true",
		"--label", labelAttachableWorkspace+"="+workspace,
		name); err != nil {
		return fmt.Sprintf("could not create %s volume %s: %v", engine, name, err)
	}
	return ""
}

// extractJSONObjectOrNull is extractJSONObject but maps "no object" (for
// example the literal "null" Docker prints for no labels) to "null".
func extractJSONObjectOrNull(out string) string {
	if s := extractJSONObject(out); s != "" {
		return s
	}
	return "null"
}

// printPVCHint tells the operator how to provide the Kubernetes PVC. agent-run
// cannot know the sandbox namespace, so it does not try to verify or create it.
func printPVCHint(name, workspace string) {
	fmt.Printf("[agent-run] mise cache: expecting PersistentVolumeClaim %q in the sandbox namespace (kubernetes driver).\n", name)
	fmt.Println("[agent-run]   If it does not exist the sandbox stays Pending. Create it once (replace <ns>, adjust size/storageClass):")
	fmt.Printf("[agent-run]   kubectl -n <ns> apply -f - <<'EOF'\n"+
		"[agent-run]   apiVersion: v1\n"+
		"[agent-run]   kind: PersistentVolumeClaim\n"+
		"[agent-run]   metadata:\n"+
		"[agent-run]     name: %s\n"+
		"[agent-run]     labels:\n"+
		"[agent-run]       %s: \"true\"\n"+
		"[agent-run]       %s: %q\n"+
		"[agent-run]   spec:\n"+
		"[agent-run]     accessModes: [ReadWriteOnce]\n"+
		"[agent-run]     resources: {requests: {storage: 10Gi}}\n"+
		"[agent-run]   EOF\n", name, labelAttachable, labelAttachableWorkspace, workspace)
	fmt.Println("[agent-run]   Note: a mount under /sandbox replaces OpenShell's default workspace PVC for this sandbox.")
}

// isDriverConfigRejection reports whether `sandbox create` output shows the
// gateway refusing the cache mount: driver config disabled
// ("caller driver config is disabled ... allow_driver_config"), resource
// admission ("external resource not admitted by required labels"), or an
// invalid/unsupported driver_config.
func isDriverConfigRejection(out string) bool {
	l := strings.ToLower(out)
	for _, m := range []string{"allow_driver_config", "driver config", "driver_config", "not admitted"} {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// ---- step 7: create with cache fallback -----------------------------------

// createSandbox runs `sandbox create`. When a cache mount is requested and the
// gateway refuses it, it WARNs with the gateway setting to enable, deletes the
// partial sandbox and retries without the cache (plan is downgraded to none).
func createSandbox(name string, args func(driverConfigJSON string) []string, plan *cachePlan) error {
	if plan.DriverConfigJSON == "" {
		return execCmd("openshell", args("")...)
	}
	out, err := execCmdOutput("openshell", args(plan.DriverConfigJSON)...)
	fmt.Print(out)
	if err == nil {
		return nil
	}
	if !isDriverConfigRejection(out) {
		return err
	}
	fmt.Printf("[agent-run] WARN: the gateway refused the mise cache mount for the %s driver; continuing without a cache.\n", plan.Driver)
	fmt.Printf("[agent-run] WARN: to enable it set %s in the gateway config (and label the volume/PVC %s=true, %s=<workspace>), then recreate the gateway.\n",
		allowDriverConfigHint(plan.Driver), labelAttachable, labelAttachableWorkspace)
	_ = execCmdSilent("openshell", "sandbox", "delete", name)
	*plan = *noCache(plan.Driver)
	return execCmd("openshell", args("")...)
}

// ---- step 10: mise phase ----------------------------------------------------

// parseMiseConfigProbe extracts the known config files from the probe output,
// ignoring blank lines and any other noise.
func parseMiseConfigProbe(out string) []string {
	known := map[string]bool{}
	for _, c := range miseConfigCandidates {
		known[c] = true
	}
	var found []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "./"))
		if known[line] && !seen[line] {
			seen[line] = true
			found = append(found, line)
		}
	}
	return found
}

func sandboxSh(sandboxName, script string) (string, error) {
	return execCmdOutput("openshell", "sandbox", "exec", "-n", sandboxName, "--", "sh", "-c", script)
}

func detectMiseConfig(sandboxName string) ([]string, error) {
	script := "cd " + sandboxRepoDir + " || exit 3; for f in " + strings.Join(miseConfigCandidates, " ") +
		"; do if [ -f \"$f\" ]; then echo \"$f\"; fi; done; exit 0"
	out, err := sandboxSh(sandboxName, script)
	if err != nil {
		return nil, fmt.Errorf("mise config detection failed: %w", err)
	}
	return parseMiseConfigProbe(out), nil
}

// runMisePhase is step 10: install the repo's mise toolchain (D11 logging).
func runMisePhase(cfg *Config, sandboxName string, plan *cachePlan) error {
	if cfg.NoMise {
		fmt.Println("[agent-run] mise: skipped (--no-mise)")
		return nil
	}
	found, err := detectMiseConfig(sandboxName)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		fmt.Printf("[agent-run] mise: no mise config found in %s (looked for: %s); skipping toolchain install\n",
			sandboxRepoDir, strings.Join(miseConfigCandidates, ", "))
		if cfg.FreshCache {
			fmt.Println("[agent-run] mise: --fresh-cache ignored (no toolchain install)")
		}
		return nil
	}
	fmt.Printf("[agent-run] mise: config found: %s\n", strings.Join(found, ", "))

	if cfg.FreshCache {
		fmt.Printf("[agent-run] mise: --fresh-cache: clearing %s/{data,cache}\n", miseCacheMount)
		clear := "set -e; for d in data cache; do if [ -d " + miseCacheMount + "/$d ]; then find " +
			miseCacheMount + "/$d -mindepth 1 -delete; fi; done"
		if _, err := sandboxSh(sandboxName, clear); err != nil {
			return fmt.Errorf("failed to clear mise cache: %w", err)
		}
	}

	state := cacheState(sandboxName, plan, cfg.FreshCache)
	fmt.Printf("[agent-run] mise: installing toolchain (cache: %s)...\n", state)
	start := time.Now()
	install := "source ~/.profile 2>/dev/null; cd " + sandboxRepoDir + " && mise trust " + sandboxRepoDir + " && mise install --yes"
	if err := execCmd("openshell", "sandbox", "exec", "-n", sandboxName, "--", "bash", "-c", install); err != nil {
		// The sandbox is deleted right after this returns, so surface the
		// denied hosts now (`openshell logs`, v0.1.4 main.rs Logs: -n, --since,
		// --source sandbox).
		printDeniedEgress(sandboxName)
		return fmt.Errorf("mise install failed: %w (if a tool needs a host outside the bootstrap allowlist, re-run with --mise-allow-host <host>[:port]; denied connections, if any, are listed above because the sandbox is deleted on exit)", err)
	}
	fmt.Printf("[agent-run] mise: install finished in %s (cache: %s)\n", time.Since(start).Round(100*time.Millisecond), state)

	if out, err := sandboxSh(sandboxName, "source ~/.profile 2>/dev/null; cd "+sandboxRepoDir+" && mise ls --current"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.TrimSpace(line) != "" {
				fmt.Printf("[agent-run] mise:   %s\n", strings.TrimSpace(line))
			}
		}
	} else {
		fmt.Printf("[agent-run] WARN: mise: could not list installed tools: %v\n", err)
	}

	lock, lerr := sandboxSh(sandboxName, "cd "+sandboxRepoDir+" && find . -maxdepth 3 -name 'mise*.lock' -print -quit")
	if lerr == nil && strings.TrimSpace(lock) != "" {
		fmt.Println("[agent-run] mise: mise.lock present; lockfile checksums enforced")
	} else {
		fmt.Println("[agent-run] WARN: mise: no mise.lock found; downloads are not checksum-pinned. Run `mise lock` in the repo and commit mise.lock")
	}
	return nil
}

// printDeniedEgress prints the sandbox log lines that mention a denial, so the
// operator can see which host to pass to --mise-allow-host after the sandbox
// has been cleaned up. Best effort: it never fails the run.
func printDeniedEgress(sandboxName string) {
	out, err := execCmdOutput("openshell", "logs", sandboxName, "--source", "sandbox", "--since", "15m", "-n", "500")
	if err != nil {
		fmt.Printf("[agent-run] WARN: could not read sandbox logs: %v\n", err)
		return
	}
	var denied []string
	for _, line := range strings.Split(out, "\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, "deny") || strings.Contains(l, "denied") || strings.Contains(l, "blocked") {
			denied = append(denied, strings.TrimSpace(line))
		}
	}
	if len(denied) == 0 {
		fmt.Println("[agent-run] mise: no denied connections found in the sandbox logs")
		return
	}
	const maxLines = 20
	if len(denied) > maxLines {
		denied = denied[len(denied)-maxLines:]
	}
	fmt.Println("[agent-run] mise: denied connections from the sandbox logs (last up to 20):")
	for _, line := range denied {
		fmt.Printf("[agent-run]   %s\n", line)
	}
}

// cacheState describes the cache for logging: "none (ephemeral)", "cold"
// (empty volume / just cleared) or "warm (N tool dirs)".
func cacheState(sandboxName string, plan *cachePlan, fresh bool) string {
	if plan == nil || plan.Mode == cacheModeNone {
		return "none (ephemeral)"
	}
	if fresh {
		return "cold (cleared by --fresh-cache)"
	}
	out, err := sandboxSh(sandboxName, "ls -A "+miseCacheMount+"/data/installs 2>/dev/null | wc -l")
	if err != nil {
		return "unknown"
	}
	n, perr := strconv.Atoi(strings.TrimSpace(out))
	if perr != nil {
		return "unknown"
	}
	if n == 0 {
		return "cold"
	}
	return fmt.Sprintf("warm (%d tools)", n)
}
