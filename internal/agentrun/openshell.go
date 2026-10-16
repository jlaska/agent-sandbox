package agentrun

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

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
func checkOpenShellVersion() (string, error) {
	out, err := execCmdOutput("openshell", "--version")
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

// profileInstalled reports whether `openshell provider list-profiles` already
// shows the profile id. It prefers the JSON form (an array of objects with an
// "id" field) and falls back to a whole-word match on the table output.
func profileInstalled(id string) bool {
	if out, err := execCmdOutput("openshell", "provider", "list-profiles", "-o", "json"); err == nil {
		var entries []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(out), &entries) == nil {
			for _, e := range entries {
				if e.ID == id {
					return true
				}
			}
			return false
		}
	}
	out, _ := execCmdOutput("openshell", "provider", "list-profiles")
	return regexp.MustCompile(`(^|[\s|])` + regexp.QuoteMeta(id) + `($|[\s|])`).MatchString(out)
}

// ensureProfile imports a provider profile from the embedded FS unless the
// gateway already has it. OpenShell 0.1.4 ships no built-in profiles, so
// `provider create --type <id>` fails until the profile is imported.
func ensureProfile(id string) error {
	path, ok := profileFiles[id]
	if !ok {
		return fmt.Errorf("unknown provider profile %q", id)
	}
	if profileInstalled(id) {
		return nil
	}
	data, err := openshell.ProviderProfiles.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read embedded profile %s: %w", id, err)
	}
	tmp, err := os.CreateTemp("", "agent-run-profile-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to stage provider profile %s: %w", id, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to stage provider profile %s: %w", id, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to stage provider profile %s: %w", id, err)
	}
	fmt.Printf("[agent-run] Importing provider profile %s...\n", id)
	if err := execCmd("openshell", "provider", "profile", "import", "--file", tmp.Name()); err != nil {
		return fmt.Errorf("failed to import %s provider profile: %w", id, err)
	}
	return nil
}

// ensureProfiles imports every profile the run needs (github-agent plus the
// harness's own).
func ensureProfiles(h *Harness) error {
	for _, id := range append([]string{GitHubProviderName}, h.Providers()...) {
		if err := ensureProfile(id); err != nil {
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
func detectDriver(override string) string {
	if override != "" {
		return override
	}
	out, err := execCmdOutput("openshell", "gateway", "info", "-o", "json")
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
