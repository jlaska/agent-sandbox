package agentrun

import (
	"strings"
	"testing"
)

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func TestBuildCreateArgs(t *testing.T) {
	args := buildCreateArgs("a-claude-1", "ghcr.io/jlaska/agent-claude:1@sha256:abc", "/tmp/p.yaml",
		[]string{"--provider", "claude-code"}, []string{"--env", "A=b"}, "")

	i := indexOf(args, "--from")
	if i < 0 || args[i+1] != "ghcr.io/jlaska/agent-claude:1@sha256:abc" {
		t.Errorf("--from <ref> missing: %v", args)
	}
	if args[0] != "sandbox" || args[1] != "create" || args[len(args)-1] != "--detach" {
		t.Errorf("unexpected shape: %v", args)
	}
	if indexOf(args, "--driver-config-json") >= 0 {
		t.Errorf("empty driver config must not be passed: %v", args)
	}
	if i := indexOf(args, "--policy"); i < 0 || args[i+1] != "/tmp/p.yaml" {
		t.Errorf("--policy missing: %v", args)
	}

	args = buildCreateArgs("n", "img", "p", nil, nil, `{"docker":{}}`)
	if i := indexOf(args, "--driver-config-json"); i < 0 || args[i+1] != `{"docker":{}}` {
		t.Errorf("driver config not passed through: %v", args)
	}
}

func TestBuildLaunchCmd(t *testing.T) {
	cfg := &Config{Model: "opus", PassthroughArgs: []string{"-p", "fix it"}}
	got := buildLaunchCmd(harnesses["claude"], cfg)
	if !strings.Contains(got, "exec claude --model 'opus' '-p' 'fix it'") {
		t.Errorf("got %q", got)
	}
	got = buildLaunchCmd(harnesses["pi"], &Config{})
	if !strings.Contains(got, "exec pi ") || strings.Contains(got, "--model") {
		t.Errorf("got %q", got)
	}
}

func TestPrintDiagnostics(t *testing.T) {
	withMockExec(t, func(mock *mockCmd) {
		mock.execOutput = func(_ string, args ...string) (string, error) {
			switch strings.Join(args, " ") {
			case "--version":
				return "openshell 0.0.113\n", nil
			case "gateway info -o json":
				return `{"compute_drivers":[{"name":"podman"}]}`, nil
			}
			return "", nil
		}
		out := captureStdout(t, func() {
			_ = printDiagnostics(&Config{Harness: "pi", Image: "agent-pi:local"})
		})
		for _, want := range []string{
			"openshell 0.0.113", "Version gate:       FAIL", "agent-pi:local",
			"not yet published", "Compute driver:     podman",
			"[openshell.drivers.podman] allow_driver_config = true",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("diag output missing %q", want)
			}
		}
	})
}

// A refused OpenShell version must stop the run before any mutating call,
// including the deferred sandbox delete.
func TestRunVersionGateMakesNoMutatingCalls(t *testing.T) {
	withMockExec(t, func(mock *mockCmd) {
		mock.execOutput = func(string, ...string) (string, error) { return "openshell 0.0.113\n", nil }
		err := Run(&Config{Repo: "jlaska/agent-sandbox", Harness: HarnessClaude})
		if err == nil || !strings.Contains(err.Error(), "too old") {
			t.Fatalf("want version-gate error, got %v", err)
		}
		for _, c := range mock.calls {
			if strings.Contains(strings.Join(c, " "), "delete") {
				t.Errorf("unexpected mutating call after refused version: %v", c)
			}
		}
	})
}
