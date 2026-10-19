package agentrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/jlaska/agent-sandbox/openshell"
)

// MinOpenShellVersion is the oldest OpenShell CLI agent-run supports.
const MinOpenShellVersion = "0.1.4"

// openShellUpgradeHint is appended to version-gate errors. OpenShell has no
// in-place upgrade path from 0.0.x.
const openShellUpgradeHint = "uninstall the old OpenShell, install v" + MinOpenShellVersion +
	" or newer (https://github.com/NVIDIA/OpenShell/releases), then recreate the gateway and sandboxes"

// versionPattern finds the first MAJOR.MINOR.PATCH in `openshell --version`
// output. clap prints "openshell 0.1.4"; dev builds append a suffix such as
// "-dev.6+g2bf9969ab", which is ignored (only the numeric core is compared).
var versionPattern = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

type semver [3]int

func (v semver) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// less reports whether v sorts before o.
func (v semver) less(o semver) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

// parseOpenShellVersion extracts the version from `openshell --version` output.
func parseOpenShellVersion(out string) (semver, error) {
	m := versionPattern.FindStringSubmatch(out)
	if m == nil {
		return semver{}, fmt.Errorf("could not parse OpenShell version from %q", strings.TrimSpace(out))
	}
	var v semver
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return semver{}, fmt.Errorf("could not parse OpenShell version from %q: %w", strings.TrimSpace(out), err)
		}
		v[i] = n
	}
	return v, nil
}

// checkOpenShellVersion runs `openshell --version` and requires >= MinOpenShellVersion.
// It returns the detected version string even when the gate fails.
func checkOpenShellVersion(ctx context.Context) (string, error) {
	out, err := execCmdOutput(ctx, "openshell", "--version")
	if err != nil {
		return "", fmt.Errorf("failed to run 'openshell --version': %w", err)
	}
	return gateOpenShellVersion(out)
}

// gateOpenShellVersion applies the minimum-version gate to --version output.
func gateOpenShellVersion(out string) (string, error) {
	v, err := parseOpenShellVersion(out)
	if err != nil {
		return "", err
	}
	minV, _ := parseOpenShellVersion(MinOpenShellVersion)
	if v.less(minV) {
		return v.String(), fmt.Errorf("OpenShell %s is too old; agent-run requires >= %s: %s", v, MinOpenShellVersion, openShellUpgradeHint)
	}
	return v.String(), nil
}

// profileFiles maps provider profile ids to their path in openshell.ProviderProfiles.
var profileFiles = map[string]string{
	"github-agent":      "github-agent-profile.yaml",
	"litellm-inference": "litellm-inference-profile.yaml",
	"claude-code":       "providers/claude-code.yaml",
	"anthropic":         "providers/anthropic.yaml",
}

// installedProfiles returns the profile ids `openshell provider list-profiles`
// shows, listed once per run. It prefers the JSON form (an array of objects
// with an "id" field) and falls back to the table output, where any
// whitespace- or '|'-separated token counts (ids are whole tokens, so
// "claude-code-extra" never matches "claude-code").
func installedProfiles(ctx context.Context) map[string]bool {
	ids := map[string]bool{}
	if out, err := execCmdOutput(ctx, "openshell", "provider", "list-profiles", "-o", "json"); err == nil {
		var entries []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(out), &entries) == nil {
			for _, e := range entries {
				ids[e.ID] = true
			}
			return ids
		}
	}
	out, _ := execCmdOutput(ctx, "openshell", "provider", "list-profiles")
	for _, tok := range strings.FieldsFunc(out, func(r rune) bool { return r == '|' || unicode.IsSpace(r) }) {
		ids[tok] = true
	}
	return ids
}

// ensureProfile imports a provider profile from the embedded FS unless the
// gateway already has it (installed). OpenShell 0.1.4 ships no built-in
// profiles, so `provider create --type <id>` fails until the profile is
// imported. If the gateway already has the id (for example the stock upstream
// `anthropic` profile, whose binaries name only curl, or an older import of
// ours), it is compared with the desired document and updated when it differs.
func ensureProfile(ctx context.Context, id string, installed bool) error {
	path, ok := profileFiles[id]
	if !ok {
		return fmt.Errorf("unknown provider profile %q", id)
	}
	data, err := profileData(id, path)
	if err != nil {
		return err
	}
	if installed {
		return reconcileProfile(ctx, id, data)
	}
	tmp, cleanup, err := stageProfile(id, data)
	if err != nil {
		return err
	}
	defer cleanup()
	fmt.Printf("[agent-run] Importing provider profile %s...\n", id)
	if err := execCmd(ctx, "openshell", "provider", "profile", "import", "--file", tmp); err != nil {
		return fmt.Errorf("failed to import %s provider profile: %w", id, err)
	}
	return nil
}

func stageProfile(id string, data []byte) (path string, cleanup func(), err error) {
	tmp, err := os.CreateTemp("", "agent-run-profile-*.yaml")
	if err != nil {
		return "", nil, fmt.Errorf("failed to stage provider profile %s: %w", id, err)
	}
	cleanup = func() { _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", nil, fmt.Errorf("failed to stage provider profile %s: %w", id, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to stage provider profile %s: %w", id, err)
	}
	return tmp.Name(), cleanup, nil
}

// reconcileProfile updates an installed profile that differs from the desired
// document in any field it sets (credentials and their env_vars, endpoints,
// binaries, ...), not just binaries: a stale credential env_var makes 0.1.4
// reject `provider create --credential KEY`, and stale endpoints would not
// match the policy. Fields the gateway adds (resource_version, timestamps,
// defaults) are ignored. `provider profile export <id> -o json` supplies the
// current document and resource_version; `provider profile update <id> -f`
// requires that non-zero resource_version (server
// handle_update_provider_profiles, v0.1.4
// crates/openshell-server/src/grpc/provider.rs). Problems here are warnings:
// the run continues with the profile as the gateway has it.
func reconcileProfile(ctx context.Context, id string, desired []byte) error {
	var want any
	if err := yaml.Unmarshal(desired, &want); err != nil {
		return fmt.Errorf("failed to parse embedded profile %s: %w", id, err)
	}
	want, err := normalizeJSON(want)
	if err != nil {
		return fmt.Errorf("failed to parse embedded profile %s: %w", id, err)
	}
	out, err := execCmdOutput(ctx, "openshell", "provider", "profile", "export", id, "-o", "json")
	if err != nil {
		fmt.Printf("[agent-run] WARN: could not export provider profile %s to compare it: %v\n", id, err)
		return nil
	}
	var have map[string]any
	if json.Unmarshal([]byte(extractJSONObject(out)), &have) != nil {
		fmt.Printf("[agent-run] WARN: could not parse exported provider profile %s; leaving it as is\n", id)
		return nil
	}
	diff := profileDiff("", want, have)
	if len(diff) == 0 {
		return nil
	}
	rv, _ := have["resource_version"].(float64)
	if rv <= 0 {
		fmt.Printf("[agent-run] WARN: provider profile %s differs from agent-run's (%s) but has no resource_version; leaving it as is\n", id, strings.Join(diff, ", "))
		return nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(desired, &doc); err != nil || len(doc.Content) == 0 {
		return fmt.Errorf("failed to parse embedded profile %s: %v", id, err)
	}
	root := doc.Content[0]
	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "resource_version"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatUint(uint64(rv), 10)})
	updated, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("failed to render profile %s: %w", id, err)
	}
	tmp, cleanup, err := stageProfile(id, updated)
	if err != nil {
		return err
	}
	defer cleanup()
	fmt.Printf("[agent-run] Updating provider profile %s (differs in: %s)...\n", id, strings.Join(diff, ", "))
	if err := execCmd(ctx, "openshell", "provider", "profile", "update", id, "--file", tmp); err != nil {
		fmt.Printf("[agent-run] WARN: could not update provider profile %s: %v (sandboxes attached to it block updates; the stale profile may reject credentials or deny traffic)\n", id, err)
	}
	return nil
}

// normalizeJSON round-trips v through JSON so YAML- and JSON-decoded values
// compare alike (numbers become float64, maps map[string]any).
func normalizeJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	err = json.Unmarshal(b, &out)
	return out, err
}

// profileDiff returns the paths at which want is not contained in have: every
// map key want sets must match (extra keys in have are ignored), lists must
// have the same length with each element contained in its counterpart, and
// lists of scalars compare as sets (binaries order does not matter).
func profileDiff(path string, want, have any) []string {
	at := path
	if at == "" {
		at = "(root)"
	}
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok {
			return []string{at}
		}
		var diff []string
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			diff = append(diff, profileDiff(p, w[k], h[k])...)
		}
		return diff
	case []any:
		h, ok := have.([]any)
		if !ok || len(h) != len(w) {
			return []string{at}
		}
		if scalars(w) && scalars(h) {
			a, b := scalarStrings(w), scalarStrings(h)
			slices.Sort(a)
			slices.Sort(b)
			if !slices.Equal(a, b) {
				return []string{at}
			}
			return nil
		}
		for i := range w {
			if len(profileDiff(fmt.Sprintf("%s[%d]", path, i), w[i], h[i])) > 0 {
				return []string{at}
			}
		}
		return nil
	default:
		if fmt.Sprint(want) != fmt.Sprint(have) || (want == nil) != (have == nil) {
			return []string{at}
		}
		return nil
	}
}

func scalars(l []any) bool {
	for _, v := range l {
		switch v.(type) {
		case map[string]any, []any:
			return false
		}
	}
	return true
}

func scalarStrings(l []any) []string {
	out := make([]string, len(l))
	for i, v := range l {
		out[i] = fmt.Sprint(v)
	}
	return out
}

// ensureProfiles imports or reconciles every profile the run needs
// (github-agent plus the harness's own, as resolved for this run by
// Harness.Providers), listing the gateway's profiles once.
func ensureProfiles(ctx context.Context, harnessProfiles []string) error {
	installed := installedProfiles(ctx)
	for _, id := range append([]string{GitHubProviderName}, harnessProfiles...) {
		if err := ensureProfile(ctx, id, installed[id]); err != nil {
			return err
		}
	}
	return nil
}

// driverUnknown is returned when the gateway's compute driver can't be determined.
const driverUnknown = "unknown"

// detectDriver returns the gateway's compute driver: the --driver override if
// set, else the first compute driver reported by `openshell gateway info -o json`
// (compute_drivers[0].name), else "unknown".
func detectDriver(ctx context.Context, override string) string {
	if override != "" {
		return override
	}
	out, err := execCmdOutput(ctx, "openshell", "gateway", "info", "-o", "json")
	if err != nil {
		return driverUnknown
	}
	return parseDriver(out)
}

// parseDriver extracts the compute driver name from `gateway info -o json` output.
func parseDriver(out string) string {
	var info struct {
		ComputeDrivers []struct {
			Name string `json:"name"`
		} `json:"compute_drivers"`
	}
	if json.Unmarshal([]byte(out), &info) != nil || len(info.ComputeDrivers) == 0 {
		return driverUnknown
	}
	if name := strings.ToLower(strings.TrimSpace(info.ComputeDrivers[0].Name)); name != "" {
		return name
	}
	return driverUnknown
}

// allowDriverConfigHint describes the gateway setting that
// `sandbox create --driver-config-json` requires.
func allowDriverConfigHint(driver string) string {
	if driver == "" || driver == driverUnknown {
		return "[openshell.drivers.<driver>] allow_driver_config = true"
	}
	return fmt.Sprintf("[openshell.drivers.%s] allow_driver_config = true", driver)
}

// profileBinaries overrides the `binaries` list of vendored upstream profiles.
// Upstream's examples name generic paths (anthropic.yaml allows only curl) and
// say to edit `binaries` for your own image; the vendored files stay
// byte-identical to upstream, so the paths our images provide are set here at
// import time. Every path must appear in some openshell/images/*/binaries.txt.
var profileBinaries = map[string][]string{
	"anthropic":   {"/usr/bin/curl", "/usr/local/bin/opencode"},
	"claude-code": {"/usr/local/bin/claude"},
}

// profileData returns the embedded profile, with its binaries replaced when
// profileBinaries has an entry for id.
func profileData(id, path string) ([]byte, error) {
	data, err := openshell.ProviderProfiles.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded profile %s: %w", id, err)
	}
	bins, ok := profileBinaries[id]
	if !ok {
		return data, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return nil, fmt.Errorf("failed to parse embedded profile %s: %v", id, err)
	}
	root := doc.Content[0]
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, b := range bins {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: b})
	}
	replaced := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "binaries" {
			root.Content[i+1] = seq
			replaced = true
		}
	}
	if !replaced {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "binaries"}, seq)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("failed to render profile %s: %w", id, err)
	}
	return out, nil
}
