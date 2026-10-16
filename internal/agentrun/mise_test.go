package agentrun

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
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
			captureStdout(t, func() { plan, _ = prepareCache(cfg, "docker") })
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
			captureStdout(t, func() { plan, _ = prepareCache(cfg, "podman") })
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
			out := captureStdout(t, func() { plan, _ = prepareCache(cfg, "docker") })
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
			out := captureStdout(t, func() { plan, _ = prepareCache(cfg, "docker") })
			if plan.Mode != cacheModeNone || !strings.Contains(out, "WARN") {
				t.Errorf("plan=%+v out=%q", plan, out)
			}
		})
	})

	t.Run("kubernetes mounts PVC and prints kubectl hint", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			var plan *cachePlan
			out := captureStdout(t, func() { plan, _ = prepareCache(cfg, "kubernetes") })
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
				out := captureStdout(t, func() { plan, _ = prepareCache(&c, tt.driver) })
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
	t.Run("rejection -> WARN with hint, delete, retry", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = func(string, ...string) (string, error) {
				return "Error: caller driver config is disabled; a gateway administrator must enable allow_driver_config", errors.New("exit 1")
			}
			plan := &cachePlan{Mode: cacheModeVolume, Volume: "v", DriverConfigJSON: `{"docker":{}}`, Driver: "docker"}
			var err error
			out := captureStdout(t, func() { err = createSandbox("sb", args, plan) })
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
			if !slices.Contains(calls, "openshell sandbox delete sb") || calls[len(calls)-1] != "openshell sandbox create --name sb" {
				t.Errorf("calls = %v", calls)
			}
		})
	})
	t.Run("unrelated failure is returned, no retry", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = func(string, ...string) (string, error) { return "image pull failed", errors.New("exit 1") }
			plan := &cachePlan{Mode: cacheModeVolume, DriverConfigJSON: `{}`, Driver: "docker"}
			var err error
			captureStdout(t, func() { err = createSandbox("sb", args, plan) })
			if err == nil || plan.DriverConfigJSON == "" || len(m.calls) != 1 {
				t.Errorf("err=%v plan=%+v calls=%v", err, plan, m.calls)
			}
		})
	})
	t.Run("no cache -> single plain create", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			if err := createSandbox("sb", args, noCache("docker")); err != nil {
				t.Fatal(err)
			}
			if len(m.calls) != 1 || strings.Contains(callStrings(m)[0], "driver-config") {
				t.Errorf("calls = %v", m.calls)
			}
		})
	})
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
				if err := runMisePhase(&Config{NoMise: true}, "sb", plan); err != nil {
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
				if err := runMisePhase(&Config{}, "sb", plan); err != nil {
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
				if err := runMisePhase(&Config{}, "sb", plan); err != nil {
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
				if strings.Contains(c, "mise trust /sandbox/repo && mise install --yes") {
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
				if err := runMisePhase(&Config{FreshCache: true}, "sb", plan); err != nil {
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
			}
			if !cleared {
				t.Error("cache not cleared")
			}
		})
	})

	t.Run("install failure hints at --mise-allow-host", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			probeMock("mise.toml\n", "0", "")(m)
			execCmdFn = func(name string, args ...string) error {
				m.calls = append(m.calls, append([]string{name}, args...))
				return errors.New("exit 1")
			}
			var err error
			captureStdout(t, func() { err = runMisePhase(&Config{}, "sb", noCache("docker")) })
			if err == nil || !strings.Contains(err.Error(), "--mise-allow-host") {
				t.Errorf("err = %v", err)
			}
		})
	})
}

func TestCacheStateAndDetectionError(t *testing.T) {
	if got := cacheState("sb", noCache("vm"), false); !strings.Contains(got, "ephemeral") {
		t.Error(got)
	}
	if got := cacheState("sb", &cachePlan{Mode: cacheModeVolume}, true); !strings.HasPrefix(got, "cold") {
		t.Error(got)
	}
	withMockExec(t, func(m *mockCmd) {
		m.err = errors.New("x")
		if got := cacheState("sb", &cachePlan{Mode: cacheModeVolume}, false); got != "unknown" {
			t.Error(got)
		}
		if _, err := detectMiseConfig("sb"); err == nil {
			t.Error("detection error must propagate")
		}
	})
}

// Narrowing must run even when mise is skipped: the mise phase itself never
// touches policy, and enforceNarrowing is unconditional in Run.
func TestNarrowingIndependentOfMise(t *testing.T) {
	withMockExec(t, func(m *mockCmd) {
		narrowMock(t, "policy-get-effective-clean.json", "policy-rev-loaded.json", nil)(m)
		captureStdout(t, func() {
			if err := runMisePhase(&Config{NoMise: true}, "sb1", noCache("docker")); err != nil {
				t.Fatal(err)
			}
			if err := enforceNarrowing(&Config{NoMise: true}, "sb1", nil); err != nil {
				t.Fatal(err)
			}
		})
		if got := callStrings(m); len(got) == 0 || got[0] != "openshell policy update sb1 --remove-rule mise_bootstrap --wait" {
			t.Errorf("narrowing did not run with --no-mise: %v", got)
		}
	})
}
