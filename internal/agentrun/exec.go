package agentrun

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// childGrace is how long a child gets to exit after SIGTERM before it is killed.
const childGrace = 10 * time.Second

var (
	childCtxMu sync.RWMutex
	childCtx   = context.Background()
)

// setChildContext makes every default exec function start its child under ctx:
// when ctx is cancelled (SIGINT/SIGTERM to agent-run) running children get
// SIGTERM, and new children refuse to start. It returns a restore function that
// puts the background context back; Run calls it before cleanup so the cleanup
// commands (sandbox delete, provider delete) are not themselves cancelled.
func setChildContext(ctx context.Context) (restore func()) {
	childCtxMu.Lock()
	childCtx = ctx
	childCtxMu.Unlock()
	return func() {
		childCtxMu.Lock()
		childCtx = context.Background()
		childCtxMu.Unlock()
	}
}

// command builds an exec.Cmd bound to the current child context.
func command(name string, args ...string) *exec.Cmd {
	childCtxMu.RLock()
	ctx := childCtx
	childCtxMu.RUnlock()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = childGrace
	return cmd
}

// Command execution functions, replaceable in tests.
var (
	execCmdFn       = defaultExecCmd
	execCmdSilentFn = defaultExecCmdSilent
	execCmdOutputFn = defaultExecCmdOutput
	// execCmdEnvFn runs a command with extra KEY=VALUE entries added to the
	// child's environment. It is how secrets reach `openshell provider create
	// --credential KEY` (bare-key form) without appearing in argv.
	execCmdEnvFn = defaultExecCmdEnv
)

func execCmd(name string, args ...string) error {
	return execCmdFn(name, args...)
}

// execCmdEnv is execCmd with extra environment entries ("KEY=VALUE") for the child.
func execCmdEnv(env []string, name string, args ...string) error {
	return execCmdEnvFn(env, name, args...)
}

func execCmdSilent(name string, args ...string) error {
	return execCmdSilentFn(name, args...)
}

func execCmdOutput(name string, args ...string) (string, error) {
	return execCmdOutputFn(name, args...)
}

func defaultExecCmd(name string, args ...string) error {
	cmd := command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func defaultExecCmdEnv(env []string, name string, args ...string) error {
	cmd := command(name, args...)
	// Later entries win in os/exec, so env overrides any inherited duplicate.
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func defaultExecCmdSilent(name string, args ...string) error {
	cmd := command(name, args...)
	return cmd.Run()
}

func defaultExecCmdOutput(name string, args ...string) (string, error) {
	cmd := command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// shellQuote wraps a string in single quotes for safe embedding in bash -c.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
