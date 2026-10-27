package agentrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests run pinClone's git commands for real, against a local bare
// repository, instead of matching them against a mock: the "openshell sandbox
// exec -n NAME --" prefix is stripped and /sandbox/repo becomes a local clone.
// The bare repo sets uploadpack.allowAnySHA1InWant, as GitHub serves any
// commit of a repository by SHA, so a commit no ref reaches can be fetched.

// localGit runs git in dir with a fixed identity and no user/system config.
func localGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.org",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.org")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	localGit(t, dir, "add", name)
	localGit(t, dir, "commit", "-q", "-m", name)
	return localGit(t, dir, "rev-parse", "HEAD")
}

// runPinCloneLocally sends pinClone's `openshell sandbox exec` git commands
// to clone and returns every git command line it ran.
func runPinCloneLocally(t *testing.T, clone string) *[]string {
	t.Helper()
	var ran []string
	toLocal := func(name string, args []string) *exec.Cmd {
		prefix := []string{"sandbox", "exec", "-n", "sbx", "--", "git", "-C", sandboxRepoDir}
		if name != "openshell" || len(args) < len(prefix) || !slices.Equal(args[:len(prefix)], prefix) {
			t.Fatalf("unexpected command: %s %s", name, strings.Join(args, " "))
		}
		gitArgs := args[len(prefix):]
		ran = append(ran, strings.Join(gitArgs, " "))
		cmd := exec.Command("git", append([]string{"-C", clone}, gitArgs...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		return cmd
	}
	origCmd, origSilent, origOutput := execCmdFn, execCmdSilentFn, execCmdOutputFn
	t.Cleanup(func() { execCmdFn, execCmdSilentFn, execCmdOutputFn = origCmd, origSilent, origOutput })
	execCmdFn = func(_ context.Context, name string, args ...string) error {
		out, err := toLocal(name, args).CombinedOutput()
		if err != nil {
			t.Logf("%s", out)
		}
		return err
	}
	execCmdSilentFn = func(_ context.Context, name string, args ...string) error {
		return toLocal(name, args).Run()
	}
	execCmdOutputFn = func(_ context.Context, name string, args ...string) (string, error) {
		out, err := toLocal(name, args).Output()
		return string(out), err
	}
	return &ran
}

// setupMovedBranch makes an origin whose main moved after the preflight
// resolved it to pinned, then clones origin as agent-run's sandbox clone
// would. rewrite force-pushes main to a sibling commit (pinned unreachable
// from any ref); otherwise main just gains a commit on top of pinned.
func setupMovedBranch(t *testing.T, rewrite bool) (clone, pinned, moved string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	work, origin := filepath.Join(root, "work"), filepath.Join(root, "origin.git")
	clone = filepath.Join(root, "clone")
	localGit(t, root, "init", "-q", "-b", "main", work)
	base := commitFile(t, work, "a.txt", "a")
	pinned = commitFile(t, work, "b.txt", "b") // what the preflight resolved
	localGit(t, root, "clone", "-q", "--bare", work, origin)
	localGit(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")

	if rewrite {
		localGit(t, work, "reset", "-q", "--hard", base)
		moved = commitFile(t, work, "c.txt", "rewritten")
		localGit(t, work, "push", "-q", "--force", origin, "main")
	} else {
		moved = commitFile(t, work, "c.txt", "c")
		localGit(t, work, "push", "-q", origin, "main")
	}
	// file:// goes through upload-pack, so the clone gets only what refs reach.
	localGit(t, root, "clone", "-q", "file://"+origin, clone)
	if head := localGit(t, clone, "rev-parse", "HEAD"); head != moved {
		t.Fatalf("clone HEAD %s, want the moved main %s", head, moved)
	}
	return clone, pinned, moved
}

func TestPinCloneLocalGitForcePushedBranch(t *testing.T) {
	clone, pinned, _ := setupMovedBranch(t, true)
	if exec.Command("git", "-C", clone, "cat-file", "-e", pinned+"^{commit}").Run() == nil {
		t.Fatal("setup: the clone already has the pinned commit")
	}
	ran := runPinCloneLocally(t, clone)
	var err error
	out := captureStdout(t, func() { err = pinClone(context.Background(), "sbx", "main", pinned) })
	if err != nil {
		t.Fatalf("pinClone: %v\n%s", err, out)
	}
	if !strings.Contains(out, "main moved after the preflight; fetching "+pinned[:12]) {
		t.Errorf("no fetch message:\n%s", out)
	}
	if !slices.Contains(*ran, "fetch --quiet origin "+pinned) {
		t.Errorf("pinned commit not fetched: %v", *ran)
	}
	if head := localGit(t, clone, "rev-parse", "HEAD"); head != pinned {
		t.Errorf("HEAD = %s, want %s", head, pinned)
	}
	if b := localGit(t, clone, "symbolic-ref", "--short", "HEAD"); b != "main" {
		t.Errorf("on branch %q, want main (not detached)", b)
	}
	if _, err := os.Stat(filepath.Join(clone, "c.txt")); !os.IsNotExist(err) {
		t.Errorf("worktree has the rewritten commit's file: %v", err)
	}
}

func TestPinCloneLocalGitAdvancedBranch(t *testing.T) {
	clone, pinned, _ := setupMovedBranch(t, false)
	ran := runPinCloneLocally(t, clone)
	var err error
	out := captureStdout(t, func() { err = pinClone(context.Background(), "sbx", "main", pinned) })
	if err != nil {
		t.Fatalf("pinClone: %v\n%s", err, out)
	}
	for _, c := range *ran {
		if strings.HasPrefix(c, "fetch") {
			t.Errorf("fetched a commit the clone has: %v", *ran)
		}
	}
	if head := localGit(t, clone, "rev-parse", "HEAD"); head != pinned {
		t.Errorf("HEAD = %s, want %s (the agent must not run the newer commit)", head, pinned)
	}
}

// A pinned commit the remote does not have fails the pin (and so the run,
// before the agent launches) instead of leaving the clone on the moved head.
func TestPinCloneLocalGitUnknownCommitFails(t *testing.T) {
	clone, _, moved := setupMovedBranch(t, true)
	runPinCloneLocally(t, clone)
	missing := strings.Repeat("e", 40)
	var err error
	captureStdout(t, func() { err = pinClone(context.Background(), "sbx", "main", missing) })
	if err == nil || !strings.Contains(err.Error(), "fetching the preflight commit") {
		t.Fatalf("err = %v, want a fetch failure", err)
	}
	if head := localGit(t, clone, "rev-parse", "HEAD"); head != moved {
		t.Errorf("HEAD changed to %s on a failed pin", head)
	}
}
