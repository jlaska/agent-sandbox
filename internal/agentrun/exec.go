package agentrun

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// childGrace is how long a child gets to exit after SIGTERM before it is killed.
const childGrace = 10 * time.Second

// detachedKey marks a context made by detached.
type detachedKey struct{}

// detached returns a context for cleanup work that must finish even after ctx
// was cancelled by SIGINT/SIGTERM (sandbox delete, provider delete, the
// fail-closed delete, log/evidence collection). Children started under it are
// also put in their own process group, so a further Ctrl-C at the terminal,
// which signals the whole foreground group, cannot kill them.
func detached(ctx context.Context) context.Context {
	return context.WithValue(context.WithoutCancel(ctx), detachedKey{}, true)
}

// command builds an exec.Cmd bound to ctx: when ctx is cancelled a running
// child gets SIGTERM (then SIGKILL after childGrace), and a new child refuses
// to start.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = childGrace
	if ctx.Value(detachedKey{}) != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
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
	// execInteractiveFn is execCmd with agent-run's stdin attached. Only the
	// harness launch uses it: `openshell sandbox exec` forwards stdin into the
	// sandbox, so any earlier command given it would drain input meant for the
	// harness (`echo prompt | agent-run ... -- -p`).
	execInteractiveFn = defaultExecInteractive
	// execCmdTeeFn streams output like execCmd and also returns it, for a
	// long command whose failure text must be inspected (sandbox create).
	execCmdTeeFn = defaultExecCmdTee
)

func execCmd(ctx context.Context, name string, args ...string) error {
	return execCmdFn(ctx, name, args...)
}

// execInteractive runs the one command that gets agent-run's stdin.
func execInteractive(ctx context.Context, name string, args ...string) error {
	return execInteractiveFn(ctx, name, args...)
}

// execCmdEnv is execCmd with extra environment entries ("KEY=VALUE") for the child.
func execCmdEnv(ctx context.Context, env []string, name string, args ...string) error {
	return execCmdEnvFn(ctx, env, name, args...)
}

func execCmdTee(ctx context.Context, name string, args ...string) (string, error) {
	return execCmdTeeFn(ctx, name, args...)
}

func execCmdSilent(ctx context.Context, name string, args ...string) error {
	return execCmdSilentFn(ctx, name, args...)
}

func execCmdOutput(ctx context.Context, name string, args ...string) (string, error) {
	return execCmdOutputFn(ctx, name, args...)
}

// defaultExecCmd streams output but gives the child no stdin (/dev/null).
func defaultExecCmd(ctx context.Context, name string, args ...string) error {
	cmd := command(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func defaultExecCmdEnv(ctx context.Context, env []string, name string, args ...string) error {
	cmd := command(ctx, name, args...)
	// Later entries win in os/exec, so env overrides any inherited duplicate.
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func defaultExecInteractive(ctx context.Context, name string, args ...string) error {
	cmd := command(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// lockedBuffer is a bytes.Buffer safe for the two goroutines os/exec uses to
// copy a child's stdout and stderr.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func defaultExecCmdTee(ctx context.Context, name string, args ...string) (string, error) {
	var buf lockedBuffer
	cmd := command(ctx, name, args...)
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	err := cmd.Run()
	return buf.buf.String(), err
}

func defaultExecCmdSilent(ctx context.Context, name string, args ...string) error {
	return command(ctx, name, args...).Run()
}

func defaultExecCmdOutput(ctx context.Context, name string, args ...string) (string, error) {
	out, err := command(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// shellQuote wraps a string in single quotes for safe embedding in bash -c.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
