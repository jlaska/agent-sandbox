package agentrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// flowMock drives a whole Run through the exec seams. onExec, when it returns
// handled=true, replaces the default (successful) result of an execCmd call.
type flowMock struct {
	m      *mockCmd
	sbName string
	onExec func(joined string) (handled bool, err error)
}

// runFlow installs flow mocks (policy fixtures, a Ready sandbox list, empty
// profile list) and runs fn from the repository root, as findRepoRoot needs.
func runFlow(t *testing.T, onExec func(joined string) (bool, error), fn func(f *flowMock)) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	// Fixtures are read relative to the package, so load them before chdir.
	eff := readTestdataAt(t, wd, "policy-get-effective-clean.json")
	rev := readTestdataAt(t, wd, "policy-rev-loaded.json")
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-flow-secret")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	origTTY := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	defer func() { stdinIsTerminal = origTTY }()

	withMockExec(t, func(m *mockCmd) {
		f := &flowMock{m: m, onExec: onExec}
		m.execOutput = func(_ string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			switch {
			case joined == "--version":
				return "openshell 0.1.4\n", nil
			case strings.Contains(joined, "list-profiles"):
				return "[]", nil
			case joined == "sandbox list":
				return f.sbName + " Ready\n", nil
			case strings.Contains(joined, "policy get") && strings.Contains(joined, "--full"):
				return eff, nil
			case strings.Contains(joined, "policy get") && strings.Contains(joined, "--rev 3"):
				return rev, nil
			}
			return "", nil
		}
		exec := func(name string, args ...string) error {
			m.calls = append(m.calls, append([]string{name}, args...))
			joined := strings.Join(args, " ")
			if i := slices.Index(args, "--name"); i >= 0 && strings.HasPrefix(joined, "sandbox create") {
				f.sbName = args[i+1]
			}
			if f.onExec != nil {
				if handled, err := f.onExec(name + " " + joined); handled {
					return err
				}
			}
			return nil
		}
		execCmdFn, execCmdSilentFn, execInteractiveFn = exec, exec, exec
		captureStdout(t, func() { fn(f) })
	})
}

func readTestdataAt(t *testing.T, pkgDir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(pkgDir, "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func flowConfig() *Config {
	return &Config{
		Repo: "jlaska/agent-sandbox", Harness: HarnessClaude, Image: "localhost/agent-claude:local",
		GitHubToken: "ghp_flow_pat", NoMise: true, NoCache: true,
	}
}

func indexMatching(calls []string, sub string) int {
	for i, c := range calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

func countMatching(calls []string, sub string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// A piped stdin launches the harness without a PTY so it can read the pipe;
// with a PTY `claude -p` sees a terminal and exits without reading its prompt.
func TestRunPipedStdinLaunchesWithoutTTY(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		stdinIsTerminal = func() bool { return false }
		if err := Run(flowConfig()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		calls := callStrings(f.m)
		if indexMatching(calls, "--no-tty") < 0 {
			t.Errorf("harness launch must use --no-tty for a piped stdin: %v", calls)
		}
		if indexMatching(calls, "--tty") >= 0 {
			t.Errorf("harness launch must not force --tty for a piped stdin: %v", calls)
		}
	})
}

// Only the harness launch gets agent-run's stdin; preparation commands that
// inherited it would drain a piped prompt before the harness starts.
func TestRunOnlyHarnessLaunchGetsStdin(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		var interactive [][]string
		execInteractiveFn = func(name string, args ...string) error {
			interactive = append(interactive, append([]string{name}, args...))
			return nil
		}
		if err := Run(flowConfig()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(interactive) != 1 || !slices.Contains(interactive[0], "--workdir") {
			t.Errorf("want exactly the harness launch with stdin, got %v", interactive)
		}
		if indexMatching(callStrings(f.m), "--workdir /sandbox/repo") >= 0 {
			t.Errorf("harness launch ran without stdin: %v", callStrings(f.m))
		}
	})
}

// Narrowing is unconditional: with --no-mise it still runs, still before the
// harness launch, and no mise install happens.
func TestRunNoMiseStillNarrowsBeforeLaunch(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		if err := Run(flowConfig()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		calls := callStrings(f.m)
		narrow := indexMatching(calls, "policy update "+f.sbName+" --remove-rule mise_bootstrap --wait")
		launch := indexMatching(calls, "--tty")
		if narrow < 0 || launch < 0 || narrow > launch {
			t.Errorf("narrow=%d launch=%d, want narrow before launch: %v", narrow, launch, calls)
		}
		if indexMatching(calls, "mise install") >= 0 {
			t.Errorf("--no-mise must not install: %v", calls)
		}
		// Secrets reach providers via env, with env-var-name keys.
		keys, env := providerCreateCall(t, f.m, "github-agent")
		if !slices.Equal(keys, []string{"GITHUB_TOKEN", "GH_TOKEN"}) || !slices.Contains(env, "GH_TOKEN=ghp_flow_pat") {
			t.Errorf("github-agent keys=%v env=%v", keys, env)
		}
		if indexMatching(calls, "export GH_TOKEN") >= 0 || indexMatching(calls, "api_token") >= 0 {
			t.Errorf("launch/hook must not clobber placeholders: %v", calls)
		}
		if countMatching(calls, "sandbox delete "+f.sbName) != 1 {
			t.Errorf("sandbox delete count != 1: %v", calls)
		}
	})
}

// SIGINT/SIGTERM cancel the context mid-run: the run stops, the cleanup runs
// exactly once, and it runs after the interrupted step.
func TestRunSignalRunsCleanupOnce(t *testing.T) {
	for _, tt := range []struct {
		name, at     string
		launchCalled bool
	}{
		{"during clone", "git clone", false},
		{"during harness launch", "--tty", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			onExec := func(joined string) (bool, error) {
				if strings.Contains(joined, tt.at) {
					cancel() // the signal arrives while this child runs
					return true, errors.New("signal: terminated")
				}
				return false, nil
			}
			runFlow(t, onExec, func(f *flowMock) {
				err := RunContext(ctx, flowConfig())
				if !errors.Is(err, ErrInterrupted) {
					t.Fatalf("err = %v, want ErrInterrupted", err)
				}
				calls := callStrings(f.m)
				hit := indexMatching(calls, tt.at)
				del := indexMatching(calls, "sandbox delete "+f.sbName)
				if hit < 0 || del < hit {
					t.Errorf("cleanup must run after the interrupted step: hit=%d delete=%d", hit, del)
				}
				if n := countMatching(calls, "sandbox delete "+f.sbName); n != 1 {
					t.Errorf("sandbox delete ran %d times, want exactly 1", n)
				}
				if n := countMatching(calls, "provider delete claude-code"); n != 2 { // pre-create + cleanup
					t.Errorf("claude-code provider delete ran %d times, want 2 (pre-create, cleanup)", n)
				}
				if n := countMatching(calls, "provider delete github-agent"); n != 2 {
					t.Errorf("github-agent provider delete ran %d times, want 2", n)
				}
				if got := indexMatching(calls, "--tty") >= 0; got != tt.launchCalled {
					t.Errorf("launch called = %v, want %v", got, tt.launchCalled)
				}
			})
		})
	}
}

// A signal that lands before the launch must not start the harness even though
// every earlier step succeeded.
func TestRunSignalAfterNarrowingDoesNotLaunch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	onExec := func(joined string) (bool, error) {
		if strings.Contains(joined, "--remove-rule mise_bootstrap") {
			cancel()
		}
		return false, nil
	}
	runFlow(t, onExec, func(f *flowMock) {
		if err := RunContext(ctx, flowConfig()); !errors.Is(err, ErrInterrupted) {
			t.Fatalf("err = %v", err)
		}
		calls := callStrings(f.m)
		if indexMatching(calls, "--tty") >= 0 {
			t.Error("harness launched after interruption")
		}
		if countMatching(calls, "sandbox delete "+f.sbName) != 1 {
			t.Errorf("cleanup did not delete the sandbox once: %v", calls)
		}
	})
}

// A failed fail-closed delete is reported accurately and retried by the
// deferred cleanup (the old code claimed "sandbox deleted" and never retried).
func TestRunNarrowingFailureDeleteFailureIsRetried(t *testing.T) {
	onExec := func(joined string) (bool, error) {
		switch {
		case strings.Contains(joined, "--remove-rule mise_bootstrap"):
			return true, errors.New("exit 1")
		case strings.Contains(joined, "sandbox delete"):
			return true, errors.New("gateway unreachable")
		}
		return false, nil
	}
	runFlow(t, onExec, func(f *flowMock) {
		err := Run(flowConfig())
		if err == nil || !strings.Contains(err.Error(), "SANDBOX DELETE FAILED") || strings.Contains(err.Error(), "sandbox deleted") {
			t.Fatalf("err = %v, want an accurate delete-failure message", err)
		}
		if n := countMatching(callStrings(f.m), "sandbox delete "+f.sbName); n != 2 {
			t.Errorf("sandbox delete attempts = %d, want 2 (fail-closed + deferred retry)", n)
		}
		if indexMatching(callStrings(f.m), "--tty") >= 0 {
			t.Error("harness launched despite failed narrowing")
		}
	})
}

// Narrowing failure with a successful delete does not delete twice.
func TestRunNarrowingFailureDeletesOnce(t *testing.T) {
	onExec := func(joined string) (bool, error) {
		if strings.Contains(joined, "--remove-rule mise_bootstrap") {
			return true, errors.New("exit 1")
		}
		return false, nil
	}
	runFlow(t, onExec, func(f *flowMock) {
		err := Run(flowConfig())
		if err == nil || !strings.Contains(err.Error(), "(sandbox deleted, agent not launched)") {
			t.Fatalf("err = %v", err)
		}
		if n := countMatching(callStrings(f.m), "sandbox delete "+f.sbName); n != 1 {
			t.Errorf("sandbox delete attempts = %d, want 1", n)
		}
	})
}

// The default exec functions honour the child context: a running child gets
// SIGTERM when it is cancelled, new children refuse to start, and restoring
// the background context lets cleanup commands run.
func TestChildContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	restore := setChildContext(ctx)
	defer restore()

	done := make(chan error, 1)
	go func() { done <- defaultExecCmdSilent("sleep", "30") }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled child should report an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child was not terminated on cancel")
	}
	if err := defaultExecCmdSilent("true"); err == nil {
		t.Error("a new child must not start under a cancelled context")
	}
	restore()
	if err := defaultExecCmdSilent("true"); err != nil {
		t.Errorf("after restore children must run: %v", err)
	}
}

func TestDefaultExecCmdEnv(t *testing.T) {
	t.Setenv("AGENT_RUN_TEST_SECRET", "from-parent")
	// The extra env wins over the inherited value and is visible to the child
	// only; success is the child's own check.
	if err := defaultExecCmdEnv([]string{"AGENT_RUN_TEST_SECRET=from-env-seam"}, "sh", "-c",
		`[ "$AGENT_RUN_TEST_SECRET" = from-env-seam ]`); err != nil {
		t.Errorf("env not passed to child: %v", err)
	}
	if os.Getenv("AGENT_RUN_TEST_SECRET") != "from-parent" {
		t.Error("parent environment must not change")
	}
}
