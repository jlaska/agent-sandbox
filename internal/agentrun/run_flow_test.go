package agentrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// flowMock drives a whole Run through the exec seams. onExec, when it returns
// handled=true, replaces the default (successful) result of an execCmd call.
type flowMock struct {
	m      *mockCmd
	sbName string
	// sbDeleted is set by `sandbox delete <sbName>`. Like the gateway, whose
	// delete is asynchronous, `sandbox get` keeps reporting the sandbox for
	// getsUntilGone more polls, and `provider delete` is refused while it
	// is still there ("is attached to sandbox(es)").
	sbDeleted     bool
	getsUntilGone int
	// failProviderDelete makes every `provider delete` fail outright.
	failProviderDelete bool
	onExec             func(joined string) (handled bool, err error)
	// execs records each execCmd/execCmdSilent/execInteractive call with the
	// context it ran under.
	execs []execRecord
}

type execRecord struct {
	cmd string
	ctx context.Context
}

// runFlow installs flow mocks (policy fixtures, a Ready sandbox list, empty
// profile list) and runs fn from an empty directory: a run needs no checkout,
// since the policy template and profiles are embedded.
func runFlow(t *testing.T, onExec func(joined string) (bool, error), fn func(f *flowMock)) {
	t.Helper()
	wd, _ := os.Getwd()
	// Fixtures are read relative to the package, so load them before chdir.
	eff := readTestdataAt(t, wd, "policy-get-effective-clean.json")
	rev := readTestdataAt(t, wd, "policy-rev-loaded.json")
	if err := os.Chdir(t.TempDir()); err != nil {
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
			case f.sbName != "" && joined == "sandbox get "+f.sbName+" -o json":
				if f.sbDeleted && f.getsUntilGone == 0 {
					return "Error: × sandbox not found", errors.New("exit status 1")
				}
				if f.sbDeleted {
					f.getsUntilGone--
				}
				return `{"name":"` + f.sbName + `","phase":"Ready"}`, nil
			case strings.HasPrefix(joined, "provider delete "):
				if f.failProviderDelete {
					return "Error: × permission denied", errors.New("exit status 1")
				}
				if f.sbName != "" && (!f.sbDeleted || f.getsUntilGone > 0) {
					return "! Failed to delete provider: provider is attached to sandbox(es): " + f.sbName, errors.New("exit status 1")
				}
				return "✓ Deleted provider", nil
			case strings.Contains(joined, "policy get") && strings.Contains(joined, "--full"):
				return eff, nil
			case strings.Contains(joined, "policy get") && strings.Contains(joined, "--rev 3"):
				return rev, nil
			}
			return "", nil
		}
		exec := func(ctx context.Context, name string, args ...string) error {
			m.calls = append(m.calls, append([]string{name}, args...))
			f.execs = append(f.execs, execRecord{name + " " + strings.Join(args, " "), ctx})
			joined := strings.Join(args, " ")
			if i := slices.Index(args, "--name"); i >= 0 && strings.HasPrefix(joined, "sandbox create") {
				f.sbName = args[i+1]
			}
			if f.onExec != nil {
				if handled, err := f.onExec(name + " " + joined); handled {
					return err
				}
			}
			if f.sbName != "" && joined == "sandbox delete "+f.sbName {
				f.sbDeleted = true
			}
			return nil
		}
		execCmdFn, execCmdSilentFn, execInteractiveFn = exec, exec, exec
		output := execCmdOutputFn
		execCmdOutputFn = func(ctx context.Context, name string, args ...string) (string, error) {
			f.execs = append(f.execs, execRecord{name + " " + strings.Join(args, " "), ctx})
			return output(ctx, name, args...)
		}
		origT, origP := sandboxGoneTimeout, sandboxGonePoll
		sandboxGoneTimeout, sandboxGonePoll = time.Second, time.Millisecond
		defer func() { sandboxGoneTimeout, sandboxGonePoll = origT, origP }()
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
		execInteractiveFn = func(_ context.Context, name string, args ...string) error {
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
				// Each session provider is deleted once, by the cleanup.
				for _, typ := range []string{"claude-code", "github-agent"} {
					if n := countMatching(calls, "provider delete "+sessionProviderName(f.sbName, typ)); n != 1 {
						t.Errorf("%s provider delete ran %d times, want 1 (cleanup)", typ, n)
					}
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

// The default exec functions honour their context: a running child gets
// SIGTERM when it is cancelled, new children refuse to start, and a detached
// context lets cleanup commands run after cancellation.
func TestExecContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- defaultExecCmdSilent(ctx, "sleep", "30") }()
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
	if err := defaultExecCmdSilent(ctx, "true"); err == nil {
		t.Error("a new child must not start under a cancelled context")
	}
	if err := defaultExecCmdSilent(detached(ctx), "true"); err != nil {
		t.Errorf("a detached context must run children after cancel: %v", err)
	}
}

// Cleanup children run in their own process group, so a second Ctrl-C at the
// terminal (delivered to the whole foreground group) cannot kill them; normal
// children stay in agent-run's group.
func TestDetachedChildOwnProcessGroup(t *testing.T) {
	pgid := func(ctx context.Context) int {
		t.Helper()
		out, err := defaultExecCmdOutput(ctx, "sh", "-c", "ps -o pgid= -p $$")
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			t.Fatalf("pgid %q: %v", out, err)
		}
		return n
	}
	own := syscall.Getpgrp()
	if got := pgid(context.Background()); got != own {
		t.Errorf("normal child pgid = %d, want agent-run's %d", got, own)
	}
	if got := pgid(detached(context.Background())); got == own {
		t.Errorf("detached child shares agent-run's process group %d", own)
	}
}

// A signal during the run must not leave cleanup running under the cancelled
// context: every cleanup command gets a live, detached one.
func TestRunCleanupAfterInterruptUsesDetachedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	onExec := func(joined string) (bool, error) {
		if strings.Contains(joined, "git clone") {
			cancel() // Ctrl-C arrives mid-run
			return true, errors.New("signal: terminated")
		}
		return false, nil
	}
	runFlow(t, onExec, func(f *flowMock) {
		err := RunContext(ctx, flowConfig())
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("err = %v, want ErrInterrupted", err)
		}
		clone := slices.IndexFunc(f.execs, func(r execRecord) bool { return strings.Contains(r.cmd, "git clone") })
		if clone < 0 {
			t.Fatal("clone never ran")
		}
		var deletes int
		for _, r := range f.execs[clone+1:] {
			if strings.Contains(r.cmd, "sandbox delete") || strings.Contains(r.cmd, "provider delete") {
				if strings.Contains(r.cmd, "sandbox delete") {
					deletes++
				}
				if r.ctx.Err() != nil || r.ctx.Value(detachedKey{}) == nil {
					t.Errorf("cleanup %q ran under a cancelled or non-detached context", r.cmd)
				}
			}
		}
		if deletes != 1 {
			t.Errorf("sandbox deletes = %d, want 1", deletes)
		}
	})
}

// Ctrl-C during narrowing: the fail-closed path deletes the sandbox, and the
// gateway finishes that delete asynchronously, refusing provider deletes until
// then. Cleanup must wait for the sandbox to go, then delete every provider.
func TestRunFailClosedWaitsForSandboxBeforeProviderDeletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	onExec := func(joined string) (bool, error) {
		if strings.Contains(joined, "--remove-rule mise_bootstrap") {
			cancel()
			return true, errors.New("signal: interrupt")
		}
		return false, nil
	}
	runFlow(t, onExec, func(f *flowMock) {
		f.getsUntilGone = 3
		var err error
		out := captureStdout(t, func() { err = RunContext(ctx, flowConfig()) })
		if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "egress narrowing failed") {
			t.Fatalf("err = %v", err)
		}
		for _, want := range []string{
			"deletion accepted", "Waiting for sandbox " + f.sbName + " to be removed ... done",
			"Deleting provider: " + sessionProviderName(f.sbName, GitHubProviderType) + " ... done",
			"Deleting provider: " + sessionProviderName(f.sbName, "claude-code") + " ... done",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "warning") {
			t.Errorf("unexpected warning:\n%s", out)
		}
		calls := callStrings(f.m)
		if n := countMatching(calls, "provider delete "+sessionProviderName(f.sbName, GitHubProviderType)); n != 1 {
			t.Errorf("github provider deletes = %d, want 1 (no refused attempt): %v", n, calls)
		}
		narrow := slices.IndexFunc(f.execs, func(r execRecord) bool { return strings.Contains(r.cmd, "--remove-rule") })
		if narrow < 0 {
			t.Fatal("narrowing never ran")
		}
		for _, r := range f.execs[narrow+1:] {
			if (strings.Contains(r.cmd, "provider delete") || strings.Contains(r.cmd, "sandbox get")) &&
				(r.ctx.Err() != nil || r.ctx.Value(detachedKey{}) == nil) {
				t.Errorf("%q ran under a cancelled or non-detached context", r.cmd)
			}
		}
	})
}

// A provider delete that fails is reported with the manual command, not "done".
func TestRunCleanupReportsFailedProviderDelete(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		f.failProviderDelete = true
		out := captureStdout(t, func() {
			if err := Run(flowConfig()); err != nil {
				t.Fatalf("Run: %v", err)
			}
		})
		gh := sessionProviderName(f.sbName, GitHubProviderType)
		want := "Deleting provider: " + gh + " ... warning: exit status 1: Error: × permission denied " +
			"(delete it manually: openshell provider delete " + gh + ")"
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	})
}

// A signal that breaks narrowing must not break the fail-closed delete.
func TestEnforceNarrowingFailClosedAfterInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	withMockExec(t, func(m *mockCmd) {
		var deleteCtx context.Context
		execCmdFn = func(c context.Context, name string, args ...string) error {
			m.calls = append(m.calls, append([]string{name}, args...))
			cancel()
			return errors.New("signal: terminated")
		}
		execCmdSilentFn = func(c context.Context, name string, args ...string) error {
			m.calls = append(m.calls, append([]string{name}, args...))
			deleteCtx = c
			return c.Err()
		}
		var res narrowOutcome
		captureStdout(t, func() { res, _ = enforceNarrowing(ctx, &Config{}, "sb", nil, nil) })
		if !res.SandboxDeleted || deleteCtx == nil || deleteCtx.Err() != nil {
			t.Errorf("fail-closed delete did not run under a live context: res=%+v", res)
		}
	})
}

func TestDefaultExecCmdEnv(t *testing.T) {
	t.Setenv("AGENT_RUN_TEST_SECRET", "from-parent")
	// The extra env wins over the inherited value and is visible to the child
	// only; success is the child's own check.
	if err := defaultExecCmdEnv(context.Background(), []string{"AGENT_RUN_TEST_SECRET=from-env-seam"}, "sh", "-c",
		`[ "$AGENT_RUN_TEST_SECRET" = from-env-seam ]`); err != nil {
		t.Errorf("env not passed to child: %v", err)
	}
	if os.Getenv("AGENT_RUN_TEST_SECRET") != "from-parent" {
		t.Error("parent environment must not change")
	}
}
