package agentrun

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

const docsManifest = `version: 1
network:
  - host: docs.example.org
    reason: "fetch API reference docs"
`

const optionalManifest = `version: 1
network:
  - host: docs.example.org
    reason: "fetch API reference docs"
    optional: true
`

// withGitHub points the preflight's GitHub API at handler for one test.
func withGitHub(t *testing.T, handler func(req *http.Request) (*http.Response, error)) {
	t.Helper()
	origHTTP, origBase := httpDo, githubAPIBase
	httpDo, githubAPIBase = handler, "https://api.github.test"
	t.Cleanup(func() { httpDo, githubAPIBase = origHTTP, origBase })
}

// countingReader counts the bytes read from it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestFetchAgentPolicy(t *testing.T) {
	ctx := context.Background()
	t.Run("present", func(t *testing.T) {
		withGitHub(t, func(req *http.Request) (*http.Response, error) {
			if got := req.URL.RequestURI(); got != "/repos/example/repo/contents/.agent-policy.yaml?ref="+flowSHA {
				t.Errorf("request %s", got)
			}
			if req.Header.Get("Authorization") != "Bearer tok" || req.Header.Get("Accept") != "application/vnd.github.raw+json" {
				t.Errorf("headers %v", req.Header)
			}
			return httpResponse(http.StatusOK, docsManifest), nil
		})
		data, found, err := fetchAgentPolicy(ctx, "tok", "example/repo", flowSHA)
		if err != nil || !found || string(data) != docsManifest {
			t.Errorf("data=%q found=%v err=%v", data, found, err)
		}
	})
	t.Run("absent", func(t *testing.T) {
		withGitHub(t, func(*http.Request) (*http.Response, error) { return httpResponse(http.StatusNotFound, `{}`), nil })
		data, found, err := fetchAgentPolicy(ctx, "tok", "example/repo", flowSHA)
		if err != nil || found || data != nil {
			t.Errorf("data=%q found=%v err=%v, want a clean not-found", data, found, err)
		}
	})
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run("API error "+http.StatusText(status), func(t *testing.T) {
			withGitHub(t, func(*http.Request) (*http.Response, error) { return httpResponse(status, `{"message":"nope"}`), nil })
			if _, _, err := fetchAgentPolicy(ctx, "tok", "example/repo", flowSHA); err == nil {
				t.Error("a non-404 API error must fail")
			}
		})
	}
	t.Run("transport error", func(t *testing.T) {
		withGitHub(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") })
		if _, _, err := fetchAgentPolicy(ctx, "tok", "example/repo", flowSHA); err == nil {
			t.Error("a transport error must fail")
		}
	})
	t.Run("oversized by Content-Length is declined unread", func(t *testing.T) {
		body := &countingReader{r: strings.NewReader(strings.Repeat("a", maxManifestBytes+10))}
		withGitHub(t, func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, ContentLength: maxManifestBytes + 10, Body: io.NopCloser(body)}, nil
		})
		_, _, err := fetchAgentPolicy(ctx, "tok", "example/repo", flowSHA)
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("err = %v", err)
		}
		if body.n != 0 {
			t.Errorf("read %d bytes of a body declared too large", body.n)
		}
	})
	t.Run("oversized without Content-Length stops at the limit", func(t *testing.T) {
		body := &countingReader{r: strings.NewReader(strings.Repeat("a", 4*maxManifestBytes))}
		withGitHub(t, func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: io.NopCloser(body)}, nil
		})
		_, _, err := fetchAgentPolicy(ctx, "tok", "example/repo", flowSHA)
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("err = %v", err)
		}
		if body.n > maxManifestBytes+1 {
			t.Errorf("read %d bytes, want at most %d", body.n, maxManifestBytes+1)
		}
	})
	t.Run("exactly at the limit is read", func(t *testing.T) {
		withGitHub(t, func(*http.Request) (*http.Response, error) {
			return httpResponse(http.StatusOK, strings.Repeat("a", maxManifestBytes)), nil
		})
		if _, found, err := fetchAgentPolicy(ctx, "tok", "example/repo", flowSHA); err != nil || !found {
			t.Errorf("found=%v err=%v", found, err)
		}
	})
	t.Run("invalid SHA", func(t *testing.T) {
		withGitHub(t, func(*http.Request) (*http.Response, error) { t.Error("no request expected"); return nil, nil })
		if _, _, err := fetchAgentPolicy(ctx, "tok", "example/repo", "main"); err == nil {
			t.Error("a ref that is not a SHA must be refused")
		}
	})
}

func TestResolveDefaultBranchSHA(t *testing.T) {
	serve := func(info, ref string) func(*http.Request) (*http.Response, error) {
		return func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/repos/example/repo":
				return httpResponse(http.StatusOK, info), nil
			case "/repos/example/repo/git/ref/heads/trunk", "/repos/example/repo/git/ref/heads/release/v1":
				return httpResponse(http.StatusOK, ref), nil
			}
			return httpResponse(http.StatusNotFound, `{}`), nil
		}
	}
	ref := func(name, typ, sha string) string {
		return `{"ref":"refs/heads/` + name + `","object":{"type":"` + typ + `","sha":"` + sha + `"}}`
	}
	t.Run("ok", func(t *testing.T) {
		withGitHub(t, serve(`{"default_branch":"trunk"}`, ref("trunk", "commit", flowSHA)))
		branch, sha, err := resolveDefaultBranchSHA(context.Background(), "tok", "example/repo")
		if err != nil || branch != "trunk" || sha != flowSHA {
			t.Errorf("branch=%q sha=%q err=%v", branch, sha, err)
		}
	})
	t.Run("branch with a slash", func(t *testing.T) {
		withGitHub(t, serve(`{"default_branch":"release/v1"}`, ref("release/v1", "commit", flowSHA)))
		if branch, _, err := resolveDefaultBranchSHA(context.Background(), "tok", "example/repo"); err != nil || branch != "release/v1" {
			t.Errorf("branch=%q err=%v", branch, err)
		}
	})
	for _, tt := range []struct{ name, info, ref string }{
		{"repo not found", `{}`, ""},
		{"option-like branch", `{"default_branch":"-x"}`, ""},
		{"revision-like branch", `{"default_branch":"a..b"}`, ""},
		{"short SHA", `{"default_branch":"trunk"}`, ref("trunk", "commit", "abc123")},
		{"tag object", `{"default_branch":"trunk"}`, ref("trunk", "tag", flowSHA)},
		{"other ref", `{"default_branch":"trunk"}`, `{"ref":"refs/heads/trunkx","object":{"type":"commit","sha":"` + flowSHA + `"}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withGitHub(t, func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/repos/example/repo" {
					if tt.info == `{}` {
						return httpResponse(http.StatusNotFound, `{"message":"Not Found"}`), nil
					}
					return httpResponse(http.StatusOK, tt.info), nil
				}
				return httpResponse(http.StatusOK, tt.ref), nil
			})
			if _, _, err := resolveDefaultBranchSHA(context.Background(), "tok", "example/repo"); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestLoadCallerGrants(t *testing.T) {
	gs, err := loadCallerGrants(&Config{AllowHosts: []string{"Docs.Example.org"}})
	if err != nil || len(gs) != 1 || gs[0].Host != "docs.example.org" {
		t.Fatalf("grants=%v err=%v", gs, err)
	}
	for _, h := range []string{"169.254.169.254", "https://docs.example.org", "*.example.org", "docs.example.org:8443"} {
		if _, err := loadCallerGrants(&Config{AllowHosts: []string{h}}); err == nil {
			t.Errorf("--allow-host %s: expected an error", h)
		}
	}
	path := t.TempDir() + "/grants.yaml"
	if err := os.WriteFile(path, []byte("version: 1\ngrants:\n  - host: docs.example.org\n    repos: [example/other-repo]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gs, err = loadCallerGrants(&Config{AllowHosts: []string{"a.example.org"}, GrantsFile: path})
	if err != nil || len(gs) != 2 || gs[1].Source != path+" grants[0]" {
		t.Fatalf("grants=%v err=%v", gs, err)
	}
	if _, err := loadCallerGrants(&Config{GrantsFile: t.TempDir() + "/missing.yaml"}); err == nil {
		t.Error("a missing grants file must fail")
	}
}

// onlyPreflightCommands fails unless every command run is one of the
// read-only checks before the preflight: nothing was created.
func onlyPreflightCommands(t *testing.T, f *flowMock) {
	t.Helper()
	for _, c := range callStrings(f.m) {
		if c != "openshell --version" && c != "openshell status" {
			t.Errorf("command ran despite a failed preflight: %s", c)
		}
	}
}

func TestRunRequiredUngrantedFailsBeforeCreating(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		f.manifest = docsManifest
		var err error
		out := captureStdout(t, func() { err = Run(flowConfig()) })
		if err == nil || !strings.Contains(err.Error(), "requires docs.example.org") {
			t.Fatalf("err = %v", err)
		}
		onlyPreflightCommands(t, f)
		for _, want := range []string{
			"denied docs.example.org: required and not granted",
			"ERROR: .agent-policy.yaml requires docs.example.org",
			"Pinned main to commit " + flowSHA,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})
}

// Hard boundaries fail before creation even with a grant: a grant for a
// baseline host is rejected by the compiler, and an IP literal before the
// token is minted.
func TestRunRestrictedGrantFailsBeforeCreating(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		cfg := flowConfig()
		cfg.AllowHosts = []string{"api.github.com"}
		if err := Run(cfg); err == nil || !strings.Contains(err.Error(), "already governed by the baseline") {
			t.Fatalf("err = %v", err)
		}
		onlyPreflightCommands(t, f)
	})
	runFlow(t, nil, func(f *flowMock) {
		cfg := flowConfig()
		cfg.AllowHosts = []string{"169.254.169.254"}
		if err := Run(cfg); err == nil || !strings.Contains(err.Error(), "IP address literals") {
			t.Fatalf("err = %v", err)
		}
		if len(f.m.calls) != 0 || len(f.httpReqs) != 0 {
			t.Errorf("an invalid grant must fail before anything runs: calls=%v http=%v", callStrings(f.m), f.httpReqs)
		}
	})
}

func TestRunMalformedManifestFailsBeforeCreating(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		f.manifest = "version: 1\nfilesystem: {}\n"
		if err := Run(flowConfig()); err == nil || !strings.Contains(err.Error(), "unsupported top-level key") {
			t.Fatalf("err = %v", err)
		}
		onlyPreflightCommands(t, f)
	})
}

func TestRunManifestAPIErrorFailsBeforeCreating(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		f.github = func(req *http.Request) (*http.Response, error) {
			f.github = nil // only the first call (the repo lookup) fails
			return httpResponse(http.StatusBadGateway, `{"message":"bad gateway"}`), nil
		}
		if err := Run(flowConfig()); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
			t.Fatalf("err = %v", err)
		}
		onlyPreflightCommands(t, f)
	})
}

// createdPolicy captures the policy file passed to `sandbox create`.
func createdPolicy(t *testing.T, policy *string, mode *os.FileMode, path *string) func(string) (bool, error) {
	return func(joined string) (bool, error) {
		if !strings.Contains(joined, "sandbox create") {
			return false, nil
		}
		args := strings.Fields(joined)
		i := slices.Index(args, "--policy")
		*path = args[i+1]
		b, err := os.ReadFile(*path)
		if err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(*path)
		*policy, *mode = string(b), fi.Mode().Perm()
		return false, nil
	}
}

func TestRunOptionalUngrantedWarnsAndProceeds(t *testing.T) {
	var policy, path string
	var mode os.FileMode
	runFlow(t, createdPolicy(t, &policy, &mode, &path), func(f *flowMock) {
		f.manifest = optionalManifest
		var err error
		out := captureStdout(t, func() { err = Run(flowConfig()) })
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(out, "WARN: optional request for docs.example.org") || !strings.Contains(out, "granted no hosts") {
			t.Errorf("output:\n%s", out)
		}
		if strings.Contains(policy, repoRulePrefix) || !strings.Contains(policy, "github_git:") {
			t.Errorf("an omitted request must add no rule:\n%s", policy)
		}
		if indexMatching(callStrings(f.m), "--tty") < 0 {
			t.Error("harness not launched")
		}
	})
}

func TestRunGrantedRequestUsesCompiledPolicy(t *testing.T) {
	var policy, path string
	var mode os.FileMode
	runFlow(t, createdPolicy(t, &policy, &mode, &path), func(f *flowMock) {
		f.manifest = docsManifest
		f.effective = `{"version":3,"status":"effective","policy_source":"sandbox","policy":{"network_policies":{` +
			`"github_git":{"name":"github-git"},` +
			`"repo_docs_example_org":{"name":"repo_docs_example_org","endpoints":[{"host":"docs.example.org","port":443}]}}}}`
		cfg := flowConfig()
		cfg.AllowHosts = []string{"docs.example.org"}
		var err error
		out := captureStdout(t, func() { err = Run(cfg) })
		if err != nil {
			t.Fatalf("Run: %v\n%s", err, out)
		}
		if !strings.Contains(policy, "  repo_docs_example_org:\n") || !strings.Contains(policy, "host: docs.example.org") {
			t.Errorf("compiled policy lacks the repo rule:\n%s", policy)
		}
		if mode != 0o600 {
			t.Errorf("policy file mode %v, want 0600", mode)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("policy file %s not removed: %v", path, err)
		}
		if !strings.Contains(out, "granted 1 host(s): docs.example.org") {
			t.Errorf("output:\n%s", out)
		}
		calls := callStrings(f.m)
		launch := indexMatching(calls, "--tty")
		if launch < 0 {
			t.Fatal("harness not launched")
		}
	})
}

// The granted rule must be in the loaded policy: if OpenShell dropped it the
// run fails closed, like a failed narrowing.
func TestRunGrantedRuleMissingFailsClosed(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		f.manifest = docsManifest
		cfg := flowConfig()
		cfg.AllowHosts = []string{"docs.example.org"}
		err := Run(cfg)
		if err == nil || !strings.Contains(err.Error(), "repo_docs_example_org (docs.example.org) is missing") {
			t.Fatalf("err = %v", err)
		}
		if indexMatching(callStrings(f.m), "--tty") >= 0 {
			t.Error("harness launched without its granted rule verified")
		}
	})
}

func TestRunNoRepoPolicyIgnoresManifest(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		f.manifest = docsManifest // would fail: required and not granted
		cfg := flowConfig()
		cfg.NoRepoPolicy = true
		var err error
		out := captureStdout(t, func() { err = Run(cfg) })
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(out, "--no-repo-policy: ignored the repository's .agent-policy.yaml") {
			t.Errorf("output:\n%s", out)
		}
		for _, r := range f.httpReqs {
			if strings.Contains(r, "/contents/") {
				t.Errorf("--no-repo-policy fetched the manifest: %s", r)
			}
		}
		// The clone is still pinned.
		if indexMatching(callStrings(f.m), "checkout --quiet -B main "+flowSHA) < 0 {
			t.Errorf("clone not pinned: %v", callStrings(f.m))
		}
	})
}

func TestRunNoManifestUsesBaseline(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		var err error
		out := captureStdout(t, func() { err = Run(flowConfig()) })
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(out, "No .agent-policy.yaml at "+flowSHA[:12]+"; using the baseline policy.") {
			t.Errorf("output:\n%s", out)
		}
		calls := callStrings(f.m)
		clone := indexMatching(calls, "git clone")
		pin := indexMatching(calls, "checkout --quiet -B main "+flowSHA)
		ident := indexMatching(calls, "git config user.name")
		if clone < 0 || pin < clone || ident < pin {
			t.Errorf("want clone, pin, identity in order: clone=%d pin=%d ident=%d", clone, pin, ident)
		}
		if indexMatching(calls, "fetch --quiet origin") >= 0 {
			t.Error("fetched a commit the clone already has")
		}
	})
}

// The branch moved after the preflight: the clone lacks the pinned commit, so
// it is fetched before the checkout.
func TestRunPinFetchesMovedCommit(t *testing.T) {
	onExec := func(joined string) (bool, error) {
		if strings.Contains(joined, "cat-file -e "+flowSHA) {
			return true, errors.New("exit status 128")
		}
		return false, nil
	}
	runFlow(t, onExec, func(f *flowMock) {
		if err := Run(flowConfig()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		calls := callStrings(f.m)
		fetch := indexMatching(calls, "git -C /sandbox/repo fetch --quiet origin "+flowSHA)
		pin := indexMatching(calls, "checkout --quiet -B main "+flowSHA)
		if fetch < 0 || pin < fetch {
			t.Errorf("want fetch before checkout: fetch=%d pin=%d %v", fetch, pin, calls)
		}
	})
}

func TestRunHeadMismatchFailsBeforeLaunch(t *testing.T) {
	runFlow(t, nil, func(f *flowMock) {
		f.head = strings.Repeat("f", 40)
		err := Run(flowConfig())
		if !errors.Is(err, errHeadMismatch) {
			t.Fatalf("err = %v, want errHeadMismatch", err)
		}
		calls := callStrings(f.m)
		if indexMatching(calls, "--tty") >= 0 || indexMatching(calls, "git config user.name") >= 0 {
			t.Errorf("run continued past a HEAD mismatch: %v", calls)
		}
		if countMatching(calls, "sandbox delete "+f.sbName) != 1 {
			t.Errorf("cleanup did not delete the sandbox once: %v", calls)
		}
	})
}

func TestExplainPolicy(t *testing.T) {
	origCred, origRevoke := resolveGitHubCredentialFn, revokeTokenFn
	defer func() { resolveGitHubCredentialFn, revokeTokenFn = origCred, origRevoke }()
	resolveGitHubCredentialFn = func(*Config) (*GitHubCredential, error) {
		return &GitHubCredential{Mode: GitHubModeApp, Token: "ghs_explain"}, nil
	}
	for _, tt := range []struct {
		name     string
		manifest string
		allow    []string
		wantErr  bool
		want     []string
	}{
		{"no manifest, unused grant", "", []string{"docs.python.org"}, false, []string{
			"Manifest:    none", "--allow-host docs.python.org: docs.python.org GET,HEAD on /**",
			"info: --allow-host docs.python.org: grant for docs.python.org is unused", "Result:      ok, baseline policy",
		}},
		{"granted", docsManifest, []string{"docs.example.org"}, false, []string{
			"Manifest:    .agent-policy.yaml at " + flowSHA,
			"granted docs.example.org: authorized by --allow-host docs.example.org [rule repo_docs_example_org; GET,HEAD on /**]",
			"Result:      ok, 1 repo rule(s) for docs.example.org",
		}},
		{"rejected", docsManifest, nil, true, []string{
			"denied docs.example.org: required and not granted", "error: .agent-policy.yaml requires", "Result:      rejected",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var revoked []string
			revokeTokenFn = func(tok string) error { revoked = append(revoked, tok); return nil }
			runFlow(t, nil, func(f *flowMock) {
				f.manifest = tt.manifest
				cfg := flowConfig()
				cfg.GitHubToken = ""
				cfg.ExplainPolicy = true
				cfg.AllowHosts = tt.allow
				var err error
				out := captureStdout(t, func() { err = Run(cfg) })
				if (err != nil) != tt.wantErr {
					t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
				}
				if len(f.m.calls) != 0 {
					t.Errorf("--explain-policy ran commands: %v", callStrings(f.m))
				}
				if !slices.Equal(revoked, []string{"ghs_explain"}) {
					t.Errorf("revoked = %v, want the preflight token once", revoked)
				}
				for _, want := range append(tt.want, "Repository:  jlaska/agent-sandbox", "Commit:      "+flowSHA+" (main)", "Harness:     claude") {
					if !strings.Contains(out, want) {
						t.Errorf("output missing %q:\n%s", want, out)
					}
				}
			})
		})
	}
}
