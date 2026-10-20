package agentrun

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
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

func TestNewSessionName(t *testing.T) {
	re := regexp.MustCompile(`^a-claude-[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		n, err := newSessionName("claude")
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(n) {
			t.Fatalf("session name %q does not match %s", n, re)
		}
		if seen[n] {
			t.Fatalf("duplicate session name %q", n)
		}
		seen[n] = true
	}
}

func TestBuildLaunchCmd(t *testing.T) {
	cfg := &Config{Model: "opus", PassthroughArgs: []string{"-p", "fix it"}}
	got := buildLaunchCmd(harnesses["claude"], cfg, HarnessOptions{})
	if !strings.Contains(got, "exec claude --model 'opus' '-p' 'fix it'") {
		t.Errorf("got %q", got)
	}
	got = buildLaunchCmd(harnesses["pi"], &Config{}, HarnessOptions{})
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
			_ = printDiagnostics(context.Background(), &Config{Harness: "pi", Image: "agent-pi:local"})
		})
		for _, want := range []string{
			"openshell 0.0.113", "Version gate:       FAIL", "agent-pi:local",
			"ghcr.io/jlaska/agent-claude:", "Compute driver:     podman",
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

// waitForSandbox reads this sandbox's own phase: a Ready neighbour whose name
// contains ours must not end the wait.
func TestWaitForSandboxOwnPhase(t *testing.T) {
	orig := sandboxReadyPoll
	sandboxReadyPoll = time.Millisecond
	defer func() { sandboxReadyPoll = orig }()

	t.Run("pending while a longer-named neighbour is Ready", func(t *testing.T) {
		t.Setenv("AGENT_RUN_READY_TIMEOUT", "50ms")
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = func(_ string, args ...string) (string, error) {
				switch strings.Join(args, " ") {
				case "sandbox get a-claude-12345 -o json":
					return `{"name":"a-claude-12345","phase":"Provisioning"}`, nil
				case "sandbox list":
					return "NAME             CREATED              PHASE\na-claude-123456  2026-10-17 00:13:38  Ready\n", nil
				}
				return "", errors.New("unexpected")
			}
			err := waitForSandbox(context.Background(), "a-claude-12345")
			if err == nil || !strings.Contains(err.Error(), "last status: Provisioning") {
				t.Errorf("err = %v", err)
			}
		})
	})

	t.Run("becomes Ready", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			polls := 0
			m.execOutput = func(string, ...string) (string, error) {
				polls++
				if polls < 3 {
					return "Error: × sandbox not found", errors.New("exit 1")
				}
				return `{"name":"sb","phase":"Ready"}`, nil
			}
			if err := waitForSandbox(context.Background(), "sb"); err != nil || polls != 3 {
				t.Errorf("err=%v polls=%d", err, polls)
			}
		})
	})

	t.Run("a different sandbox in the reply is an error, not Ready", func(t *testing.T) {
		withMockExec(t, func(m *mockCmd) {
			m.execOutput = func(string, ...string) (string, error) { return `{"name":"other","phase":"Ready"}`, nil }
			if _, _, err := sandboxPhase(context.Background(), "sb"); err == nil {
				t.Error("expected error")
			}
		})
	})
}

func TestSandboxReadyTimeout(t *testing.T) {
	t.Setenv("AGENT_RUN_READY_TIMEOUT", "")
	if got := sandboxReadyTimeout(); got != SandboxReadyTimeout || got < 5*time.Minute {
		t.Errorf("default = %v, want SandboxReadyTimeout (long enough for a first image pull)", got)
	}
	t.Setenv("AGENT_RUN_READY_TIMEOUT", "25m")
	if got := sandboxReadyTimeout(); got != 25*time.Minute {
		t.Errorf("override = %v", got)
	}
	t.Setenv("AGENT_RUN_READY_TIMEOUT", "soon")
	var got time.Duration
	out := captureStdout(t, func() { got = sandboxReadyTimeout() })
	if got != SandboxReadyTimeout || !strings.Contains(out, "invalid AGENT_RUN_READY_TIMEOUT") {
		t.Errorf("invalid override: got %v out %q", got, out)
	}
}
