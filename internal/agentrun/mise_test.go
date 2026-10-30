package agentrun

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var regexpHashSuffix = regexp.MustCompile(`-[0-9a-f]{8}$`)

func TestCacheVolumeName(t *testing.T) {
	long := strings.Repeat("a", 40)
	tests := []struct {
		name, owner, repo string
		wantExact         string // exact expected name ("" = only check properties)
		wantHash          bool
	}{
		{"simple", "jlaska", "agent-sandbox", "mise-cache-jlaska-agent-sandbox", false},
		{"uppercase folds without hash", "JLaska", "Agent-Sandbox", "mise-cache-jlaska-agent-sandbox", false},
		{"dots get hash", "jlaska", "my.repo", "", true},
		{"underscore gets hash", "jlaska", "my_repo", "", true},
		{"trailing dot gets hash", "jlaska", "repo.", "", true},
		{"long gets truncated + hash", long, long, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cacheVolumeName(tt.owner, tt.repo)
			if tt.wantExact != "" && got != tt.wantExact {
				t.Errorf("got %q want %q", got, tt.wantExact)
			}
			if len(got) > 63 {
				t.Errorf("len %d > 63: %q", len(got), got)
			}
			if !strings.HasPrefix(got, "mise-cache-") {
				t.Errorf("bad prefix: %q", got)
			}
			if got != strings.ToLower(got) || strings.ContainsAny(got, "._") || strings.HasSuffix(got, "-") {
				t.Errorf("not a clean DNS label: %q", got)
			}
			hashed := regexpHashSuffix.MatchString(got)
			if tt.wantHash && !hashed {
				t.Errorf("expected -<8hex> suffix: %q", got)
			}
		})
	}
	if cacheVolumeName("a", "b.c") == cacheVolumeName("a", "b-c") {
		t.Error("b.c and b-c must not collide")
	}
	if cacheVolumeName(long, long+"x") == cacheVolumeName(long, long+"y") {
		t.Error("long names differing only at the tail must not collide")
	}
	if cacheVolumeName("O", "R.x") != cacheVolumeName("o", "r.x") {
		t.Error("hash must be case-insensitive")
	}
}

func TestDriverConfigGolden(t *testing.T) {
	const name = "mise-cache-jlaska-agent-sandbox"
	for _, d := range []string{"docker", "podman", "kubernetes"} {
		t.Run(d, func(t *testing.T) {
			got, err := buildDriverConfigJSON(d, name)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.TrimSpace(readTestdata(t, "driver-config-"+d+".json"))
			if got != want {
				t.Errorf("golden mismatch\n got %s\nwant %s", got, want)
			}
		})
	}
	if _, err := buildDriverConfigJSON("vm", name); err == nil {
		t.Error("vm has no mounts")
	}
}

func TestPrepareCache(t *testing.T) {
	cfg := &Config{Repo: "jlaska/agent-sandbox"}

	t.Run("docker creates labelled volume when missing", func(t *testing.T) {
		t.Setenv("OPENSHELL_WORKSPACE", "")
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = func(string, ...string) (string, error) { return "", errors.New("no such volume") }
			var plan *cachePlan
			captureStdout(t, func() { plan, _ = prepareCache(context.Background(), cfg, "docker") })
			if plan.Mode != cacheModeVolume || plan.DriverConfigJSON == "" {
				t.Fatalf("plan = %+v", plan)
			}
			want := "docker volume create --label openshell.ai/sandbox-attachable=true --label openshell.ai/sandbox-attachable-workspace=default mise-cache-jlaska-agent-sandbox"
			if !slices.Contains(callStrings(m), want) {
				t.Errorf("calls %v missing %q", callStrings(m), want)
			}
		})
	})

	t.Run("podman honours OPENSHELL_WORKSPACE and reuses labelled volume", func(t *testing.T) {
		t.Setenv("OPENSHELL_WORKSPACE", "team-a")
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = func(string, ...string) (string, error) {
				return `{"openshell.ai/sandbox-attachable":"true","openshell.ai/sandbox-attachable-workspace":"team-a"}`, nil
			}
			var plan *cachePlan
			captureStdout(t, func() { plan, _ = prepareCache(context.Background(), cfg, "podman") })
			if plan.Mode != cacheModeVolume || !strings.HasPrefix(plan.DriverConfigJSON, `{"podman"`) {
				t.Fatalf("plan = %+v", plan)
			}
			for _, c := range callStrings(m) {
				if strings.Contains(c, "volume create") {
					t.Errorf("must not create existing volume: %s", c)
				}
			}
		})
	})

	t.Run("existing volume with wrong labels degrades", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = func(string, ...string) (string, error) { return "null\n", nil }
			var plan *cachePlan
			out := captureStdout(t, func() { plan, _ = prepareCache(context.Background(), cfg, "docker") })
			if plan.Mode != cacheModeNone || !strings.Contains(out, "WARN") || !strings.Contains(out, "volume rm") {
				t.Errorf("plan=%+v out=%q", plan, out)
			}
		})
	})

	t.Run("volume create failure degrades", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.err = errors.New("boom")
			m.execOutput = func(string, ...string) (string, error) { return "", errors.New("missing") }
			var plan *cachePlan
			out := captureStdout(t, func() { plan, _ = prepareCache(context.Background(), cfg, "docker") })
			if plan.Mode != cacheModeNone || !strings.Contains(out, "WARN") {
				t.Errorf("plan=%+v out=%q", plan, out)
			}
		})
	})

	t.Run("kubernetes mounts PVC and prints kubectl hint", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			var plan *cachePlan
			out := captureStdout(t, func() { plan, _ = prepareCache(context.Background(), cfg, "kubernetes") })
			if plan.Mode != cacheModePVC || plan.Volume != "mise-cache-jlaska-agent-sandbox" {
				t.Errorf("plan = %+v", plan)
			}
			if !strings.Contains(out, "kubectl -n <ns> apply") || !strings.Contains(out, "PersistentVolumeClaim") {
				t.Errorf("hint missing: %q", out)
			}
			if len(m.calls) != 0 {
				t.Errorf("k8s must not run commands: %v", m.calls)
			}
		})
	})

	for _, tt := range []struct {
		name, driver, want string
		noCache            bool
	}{
		{"vm", "vm", "microVM", false},
		{"unknown", "unknown", "unknown or unsupported", false},
		{"--no-cache", "docker", "--no-cache", true},
	} {
		t.Run("none: "+tt.name, func(t *testing.T) {
			withMockExec(t, func(m *mockCmd) {
				c := *cfg
				c.NoCache = tt.noCache
				var plan *cachePlan
				out := captureStdout(t, func() { plan, _ = prepareCache(context.Background(), &c, tt.driver) })
				if plan.Mode != cacheModeNone || plan.DriverConfigJSON != "" || !strings.Contains(out, tt.want) {
					t.Errorf("plan=%+v out=%q", plan, out)
				}
				if len(m.calls) != 0 {
					t.Errorf("no commands expected: %v", m.calls)
				}
			})
		})
	}
}

func TestCreateSandboxRetriesWithoutCache(t *testing.T) {
	args := func(j string) []string {
		a := []string{"sandbox", "create", "--name", "sb"}
		if j != "" {
			a = append(a, "--driver-config-json", j)
		}
		return a
	}
	// rejecting answers the cached create with a refusal and `sandbox get`
	// with gone (not found) after `gone` polls.
	rejecting := func(m *mockCmd, gone int) {
		polls := 0
		m.execOutput = func(_ string, args ...string) (string, error) {
			if len(args) > 1 && args[0] == "sandbox" && args[1] == "get" {
				polls++
				if polls > gone {
					return "Error: × sandbox not found", errors.New("exit 1")
				}
				return `{"name":"sb","phase":"Deleting"}`, nil
			}
			return "Error: caller driver config is disabled; a gateway administrator must enable allow_driver_config", errors.New("exit 1")
		}
	}
	fastPoll := func(t *testing.T, timeout time.Duration) {
		origT, origP := sandboxGoneTimeout, sandboxGonePoll
		sandboxGoneTimeout, sandboxGonePoll = timeout, time.Millisecond
		t.Cleanup(func() { sandboxGoneTimeout, sandboxGonePoll = origT, origP })
	}
	t.Run("rejection -> WARN with hint, delete, wait, retry", func(t *testing.T) {
		fastPoll(t, time.Minute)
		withMockExec(t, func(m *mockCmd) {
			rejecting(m, 2) // deletion is asynchronous: still listed twice
			plan := &cachePlan{Mode: cacheModeVolume, Volume: "v", DriverConfigJSON: `{"docker":{}}`, Driver: "docker"}
			var err error
			out := captureStdout(t, func() { err = createSandbox(context.Background(), "sb", args, plan) })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "WARN") || !strings.Contains(out, "[openshell.drivers.docker] allow_driver_config = true") {
				t.Errorf("out = %q", out)
			}
			if plan.Mode != cacheModeNone || plan.DriverConfigJSON != "" {
				t.Errorf("plan not downgraded: %+v", plan)
			}
			calls := callStrings(m)
			del := slices.Index(calls, "openshell sandbox delete sb")
			polls := countMatching(calls, "openshell sandbox get sb -o json")
			if del < 0 || polls != 3 || calls[len(calls)-1] != "openshell sandbox create --name sb" {
				t.Errorf("want delete, 3 polls, then plain create; calls = %v", calls)
			}
		})
	})
	t.Run("name still taken -> error, no blind recreate", func(t *testing.T) {
		fastPoll(t, 20*time.Millisecond)
		withMockExec(t, func(m *mockCmd) {
			rejecting(m, 1<<30)
			plan := &cachePlan{Mode: cacheModeVolume, DriverConfigJSON: `{}`, Driver: "docker"}
			var err error
			captureStdout(t, func() { err = createSandbox(context.Background(), "sb", args, plan) })
			if err == nil || !strings.Contains(err.Error(), "still exists") {
				t.Errorf("err = %v", err)
			}
			if calls := callStrings(m); calls[len(calls)-1] == "openshell sandbox create --name sb" {
				t.Errorf("recreated while the name was taken: %v", calls)
			}
		})
	})
	t.Run("create output streams through the tee seam", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			var teed bool
			execCmdTeeFn = func(_ context.Context, name string, a ...string) (string, error) {
				teed = true
				return "", nil
			}
			plan := &cachePlan{Mode: cacheModeVolume, DriverConfigJSON: `{}`, Driver: "docker"}
			if err := createSandbox(context.Background(), "sb", args, plan); err != nil || !teed {
				t.Errorf("err=%v teed=%v", err, teed)
			}
		})
	})
	t.Run("unrelated failure is returned, no retry", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			// Mentions driver config, but is not a refusal of the mount.
			m.execOutput = func(string, ...string) (string, error) {
				return "Error: invalid image ghcr.io/x (driver config: pull failed)", errors.New("exit 1")
			}
			plan := &cachePlan{Mode: cacheModeVolume, DriverConfigJSON: `{}`, Driver: "docker"}
			var err error
			captureStdout(t, func() { err = createSandbox(context.Background(), "sb", args, plan) })
			if err == nil || plan.DriverConfigJSON == "" || len(m.calls) != 1 {
				t.Errorf("err=%v plan=%+v calls=%v", err, plan, m.calls)
			}
		})
	})
	t.Run("no cache -> single plain create", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			if err := createSandbox(context.Background(), "sb", args, noCache("docker")); err != nil {
				t.Fatal(err)
			}
			if len(m.calls) != 1 || strings.Contains(callStrings(m)[0], "driver-config") {
				t.Errorf("calls = %v", m.calls)
			}
		})
	})
}

func TestIsDriverConfigRejection(t *testing.T) {
	for out, want := range map[string]bool{
		"Error: caller driver config is disabled; a gateway administrator must enable allow_driver_config": true,
		"Error: external resource not admitted by required labels":                                         true,
		"Error: invalid driver_config: unknown field mounts":                                               true,
		"Error: unsupported driver config for driver vm":                                                   true,
		"Error: invalid image ghcr.io/x (driver config: pull failed)":                                      false,
		"Error: driver_config rejected because image pull failed":                                          false,
		"Error: sandbox already exists":                                                                    false,
	} {
		if got := isDriverConfigRejection(out); got != want {
			t.Errorf("isDriverConfigRejection(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestParseMiseConfigProbe(t *testing.T) {
	out := "warning: noise\n\n./mise.toml\n  .tool-versions \nrandom.txt\nmise.toml\n.config/mise/config.toml\n"
	got := parseMiseConfigProbe(out)
	want := []string{"mise.toml", ".tool-versions", ".config/mise/config.toml"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if len(parseMiseConfigProbe("")) != 0 {
		t.Error("empty probe must find nothing")
	}
}

// probeMock answers sandbox exec calls by substring.
func probeMock(found string, cacheCount string, lock string) func(*mockCmd) {
	return func(m *mockCmd) {
		m.execOutput = func(_ string, args ...string) (string, error) {
			j := strings.Join(args, " ")
			switch {
			case strings.Contains(j, "for f in mise.toml"):
				return found, nil
			case strings.Contains(j, "wc -l"):
				return cacheCount, nil
			case strings.Contains(j, "mise ls --current"):
				return "kubectl 1.30.0\n", nil
			case strings.Contains(j, "mise*.lock"):
				return lock, nil
			}
			return "", nil
		}
	}
}

func TestRunMisePhaseLogPaths(t *testing.T) {
	plan := &cachePlan{Mode: cacheModeVolume}

	t.Run("--no-mise", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			out := captureStdout(t, func() {
				if err := runMisePhase(context.Background(), &Config{NoMise: true}, "sb", plan); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "mise: skipped (--no-mise)") || len(m.calls) != 0 {
				t.Errorf("out=%q calls=%v", out, m.calls)
			}
		})
	})

	t.Run("no config", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			probeMock("", "", "")(m)
			out := captureStdout(t, func() {
				if err := runMisePhase(context.Background(), &Config{}, "sb", plan); err != nil {
					t.Fatal(err)
				}
			})
			want := "mise: no mise config found in /sandbox/repo (looked for: mise.toml, .mise.toml, mise/config.toml, .config/mise.toml, .config/mise/config.toml, .tool-versions); skipping toolchain install"
			if !strings.Contains(out, want) {
				t.Errorf("out=%q", out)
			}
			for _, c := range callStrings(m) {
				if strings.Contains(c, "mise install") {
					t.Errorf("must not install: %s", c)
				}
			}
		})
	})

	t.Run("found, warm cache, lockfile", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			probeMock("mise.toml\n", "3\n", "./mise.lock\n")(m)
			out := captureStdout(t, func() {
				if err := runMisePhase(context.Background(), &Config{}, "sb", plan); err != nil {
					t.Fatal(err)
				}
			})
			for _, w := range []string{"config found: mise.toml", "cache: warm (3 tools)", "install finished in",
				"kubectl 1.30.0", "lockfile checksums enforced"} {
				if !strings.Contains(out, w) {
					t.Errorf("out missing %q:\n%s", w, out)
				}
			}
			var installed bool
			for _, c := range callStrings(m) {
				if strings.Contains(c, "mise trust 'mise.toml' && mise install --locked --yes") {
					installed = true
				}
			}
			if !installed {
				t.Errorf("install not run: %v", callStrings(m))
			}
		})
	})

	t.Run("found, no lockfile, fresh cache", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			probeMock(".tool-versions\n", "0", "")(m)
			out := captureStdout(t, func() {
				if err := runMisePhase(context.Background(), &Config{FreshCache: true}, "sb", plan); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "WARN: mise: no mise.lock") || !strings.Contains(out, "--fresh-cache: clearing /sandbox/.mise-cache/{data,cache}") {
				t.Errorf("out=%s", out)
			}
			var cleared bool
			for _, c := range callStrings(m) {
				if strings.Contains(c, "-mindepth 1 -delete") {
					cleared = true
				}
				if strings.Contains(c, "--locked") {
					t.Errorf("install without a lockfile must not pass --locked: %s", c)
				}
			}
			if !cleared {
				t.Error("cache not cleared")
			}
		})
	})

	t.Run("install failure hints at --mise-allow-host", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			probeMock("mise.toml\n", "0", "")(m)
			execCmdFn = func(_ context.Context, name string, args ...string) error {
				m.calls = append(m.calls, append([]string{name}, args...))
				return errors.New("exit 1")
			}
			var err error
			captureStdout(t, func() { err = runMisePhase(context.Background(), &Config{}, "sb", noCache("docker")) })
			if err == nil || !strings.Contains(err.Error(), "--mise-allow-host") {
				t.Errorf("err = %v", err)
			}
		})
	})
}

func TestCacheStateAndDetectionError(t *testing.T) {
	if got := cacheState(context.Background(), "sb", noCache("vm"), false); !strings.Contains(got, "ephemeral") {
		t.Error(got)
	}
	if got := cacheState(context.Background(), "sb", &cachePlan{Mode: cacheModeVolume}, true); !strings.HasPrefix(got, "cold") {
		t.Error(got)
	}
	withMockExec(t, func(m *mockCmd) {
		m.err = errors.New("x")
		if got := cacheState(context.Background(), "sb", &cachePlan{Mode: cacheModeVolume}, false); got != "unknown" {
			t.Error(got)
		}
		if _, err := detectMiseConfig(context.Background(), "sb"); err == nil {
			t.Error("detection error must propagate")
		}
	})
}

// Narrowing must run even when mise is skipped: the mise phase itself never
// touches policy. The Run-level proof (narrowing before launch under
// --no-mise) is TestRunNoMiseStillNarrowsBeforeLaunch; this checks the pieces.
func TestNarrowingIndependentOfMise(t *testing.T) {
	withMockExec(t, func(m *mockCmd) {
		narrowMock(t, "policy-get-effective-clean.json", "policy-rev-loaded.json", nil)(m)
		captureStdout(t, func() {
			if err := runMisePhase(context.Background(), &Config{NoMise: true}, "sb1", noCache("docker")); err != nil {
				t.Fatal(err)
			}
			if _, err := enforceNarrowing(context.Background(), &Config{NoMise: true}, "sb1", nil, nil); err != nil {
				t.Fatal(err)
			}
		})
		if got := callStrings(m); len(got) == 0 || got[0] != "openshell policy update sb1 --remove-rule mise_bootstrap --wait" {
			t.Errorf("narrowing did not run with --no-mise: %v", got)
		}
	})
}

// The sandbox is deleted right after a failed install, so denied hosts must be
// printed before the error is returned.
func TestMiseInstallFailurePrintsDeniedEgress(t *testing.T) {
	withMockExec(t, func(m *mockCmd) {
		probeMock("mise.toml\n", "0", "")(m)
		orig := m.execOutput
		m.execOutput = func(n string, a ...string) (string, error) {
			if len(a) > 0 && a[0] == "logs" {
				return "INFO ok\nWARN CONNECT denied host=evil.example.com:443 binary=/usr/bin/mise\n", nil
			}
			return orig(n, a...)
		}
		execCmdFn = func(_ context.Context, name string, args ...string) error {
			m.calls = append(m.calls, append([]string{name}, args...))
			return errors.New("exit 1")
		}
		var err error
		out := captureStdout(t, func() { err = runMisePhase(context.Background(), &Config{}, "sb", noCache("docker")) })
		if err == nil || !strings.Contains(out, "evil.example.com:443") {
			t.Errorf("err=%v out=%s", err, out)
		}
		if strings.Contains(err.Error(), "appear in the sandbox logs") {
			t.Errorf("error still points at logs of a deleted sandbox: %v", err)
		}
	})
}

// The default install trusts only the detected files, under paranoid mode and
// with hooks off; --mise-unsafe restores the plain repo-wide trust.
func TestMiseInstallScript(t *testing.T) {
	got := miseInstallScript([]string{"mise.toml", ".tool-versions"}, false, false)
	want := "source ~/.profile 2>/dev/null; cd /sandbox/repo && export MISE_PARANOID=1 MISE_NO_HOOKS=1 " +
		"MISE_MINIMUM_RELEASE_AGE=3d UV_EXCLUDE_NEWER='3 days' npm_config_min_release_age=3 && " +
		"mise trust 'mise.toml' && mise trust '.tool-versions' && mise install --yes"
	if got != want {
		t.Errorf("hardened:\n got %s\nwant %s", got, want)
	}
	got = miseInstallScript([]string{"mise.toml"}, true, false)
	if strings.Contains(got, "MISE_PARANOID") || strings.Contains(got, "MISE_NO_HOOKS") ||
		!strings.Contains(got, "export "+dependencyCooldownOffEnv+" && mise trust /sandbox/repo && mise install --yes") {
		t.Errorf("unsafe: %s", got)
	}
	// A committed mise.lock installs strictly from it, hardened or not.
	for _, unsafe := range []bool{false, true} {
		if got := miseInstallScript([]string{"mise.toml"}, unsafe, true); !strings.HasSuffix(got, " && mise install --locked --yes") {
			t.Errorf("locked (unsafe=%v): %s", unsafe, got)
		}
	}
}

// The cooldown exports must survive a real shell intact (UV_EXCLUDE_NEWER
// holds a space).
func TestDependencyCooldownEnvShell(t *testing.T) {
	for env, want := range map[string]string{
		dependencyCooldownEnv:    "3d|3 days|3",
		dependencyCooldownOffEnv: "0s|false|0",
	} {
		out, err := exec.Command("bash", "-c", "export "+env+
			` && printf '%s|%s|%s' "$MISE_MINIMUM_RELEASE_AGE" "$UV_EXCLUDE_NEWER" "$npm_config_min_release_age"`).Output()
		if err != nil || string(out) != want {
			t.Errorf("export %s: got %q (%v), want %q", env, out, err, want)
		}
	}
}

// agent-run's cooldown must match the one baked into the base image.
func TestDependencyCooldownMatchesBaseImage(t *testing.T) {
	data, err := os.ReadFile("../../openshell/images/base/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ARG DEP_COOLDOWN_DAYS=(\d+)$`).FindSubmatch(data)
	if m == nil {
		t.Fatal("ARG DEP_COOLDOWN_DAYS not found in the base Dockerfile")
	}
	days := string(m[1])
	want := "MISE_MINIMUM_RELEASE_AGE=" + days + "d UV_EXCLUDE_NEWER='" + days + " days' npm_config_min_release_age=" + days
	if dependencyCooldownEnv != want {
		t.Errorf("dependencyCooldownEnv = %q, want %q (base image DEP_COOLDOWN_DAYS=%s)", dependencyCooldownEnv, want, days)
	}
}

func TestRunMisePhaseHardening(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		withMockExec(t, func(m *mockCmd) {
			probeMock("mise.toml\n", "0", "")(m)
			out := captureStdout(t, func() {
				if err := runMisePhase(context.Background(), &Config{MiseUnsafe: unsafe}, "sb", noCache("docker")); err != nil {
					t.Fatal(err)
				}
			})
			install := ""
			for _, c := range callStrings(m) {
				if strings.Contains(c, "mise install") {
					install = c
				}
			}
			if hardened := strings.Contains(install, "MISE_PARANOID=1 MISE_NO_HOOKS=1") &&
				strings.Contains(install, dependencyCooldownEnv); hardened == unsafe {
				t.Errorf("unsafe=%v: install = %q", unsafe, install)
			}
			if unsafe && !strings.Contains(out, "WARN: mise: --mise-unsafe") {
				t.Errorf("no unsafe warning: %s", out)
			}
			if !unsafe && !strings.Contains(out, "hardened install") {
				t.Errorf("no hardened log: %s", out)
			}
		})
	}
}
