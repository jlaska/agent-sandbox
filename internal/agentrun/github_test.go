package agentrun

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestListAppRepos(t *testing.T) {
	origHTTP := httpDo
	origOutput := execCmdOutputFn
	defer func() {
		httpDo = origHTTP
		execCmdOutputFn = origOutput
	}()

	_, keyPEM := testRSAKey(t)
	execCmdOutputFn = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "security" {
			return string(keyPEM), nil
		}
		return "", nil
	}

	// Mock that handles the token mint POST, the repo listing GET and the
	// token revoke DELETE (recorded in revoked).
	var revoked []string
	listMock := func(repoJSON string) func(*http.Request) (*http.Response, error) {
		revoked = nil
		return func(req *http.Request) (*http.Response, error) {
			if req.Method == "DELETE" && req.URL.Path == "/installation/token" {
				revoked = append(revoked, req.Header.Get("Authorization"))
				return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			if req.Method == "POST" && strings.Contains(req.URL.Path, "/access_tokens") {
				return &http.Response{
					StatusCode: 201,
					Body:       io.NopCloser(strings.NewReader(`{"token":"ghs_list_token"}`)),
				}, nil
			}
			if req.Method == "GET" && strings.Contains(req.URL.Path, "/installation/repositories") {
				return &http.Response{
					StatusCode: 200,
					Body:       io.NopCloser(strings.NewReader(repoJSON)),
				}, nil
			}
			return &http.Response{
				StatusCode: 404,
				Body:       io.NopCloser(strings.NewReader(`{"message":"Not Found"}`)),
			}, nil
		}
	}

	t.Run("successful listing", func(t *testing.T) {
		httpDo = listMock(`{"repositories":[{"full_name":"jlaska/agent-sandbox"},{"full_name":"example/other-repo"}]}`)

		repos, err := ListAppRepos()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(repos) != 2 {
			t.Fatalf("expected 2 repos, got %d", len(repos))
		}
		if repos[0] != "jlaska/agent-sandbox" {
			t.Errorf("repos[0] = %q, want jlaska/agent-sandbox", repos[0])
		}
		if repos[1] != "example/other-repo" {
			t.Errorf("repos[1] = %q, want example/other-repo", repos[1])
		}
		// The listing token reaches every repo on the installation, so it
		// must not outlive the listing.
		if want := []string{"Bearer ghs_list_token"}; !slices.Equal(revoked, want) {
			t.Errorf("revoked = %v, want %v", revoked, want)
		}
	})

	t.Run("API error", func(t *testing.T) {
		httpDo = func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 401,
				Body:       io.NopCloser(strings.NewReader(`{"message":"Bad credentials"}`)),
			}, nil
		}

		_, err := ListAppRepos()
		if err == nil {
			t.Error("expected error for API failure")
		}
	})

	t.Run("empty installation", func(t *testing.T) {
		httpDo = listMock(`{"repositories":[]}`)

		repos, err := ListAppRepos()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(repos) != 0 {
			t.Errorf("expected 0 repos, got %d", len(repos))
		}
	})
}

func TestResolveGitHubCredential(t *testing.T) {
	t.Run("PAT from flag", func(t *testing.T) {
		cfg := &Config{
			Repo:        "jlaska/agent-sandbox",
			GitHubToken: "ghp_test123",
		}
		cred, err := ResolveGitHubCredential(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.Mode != GitHubModePAT {
			t.Errorf("Mode = %q, want %q", cred.Mode, GitHubModePAT)
		}
		if cred.Token != "ghp_test123" {
			t.Errorf("Token = %q, want ghp_test123", cred.Token)
		}
		if cred.Revocable() {
			t.Error("PAT should not be revocable")
		}
	})

	t.Run("PAT from env", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "ghp_from_env")
		cfg := &Config{Repo: "jlaska/agent-sandbox"}
		cred, err := ResolveGitHubCredential(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.Mode != GitHubModePAT {
			t.Errorf("Mode = %q, want %q", cred.Mode, GitHubModePAT)
		}
		if cred.Token != "ghp_from_env" {
			t.Errorf("Token = %q, want ghp_from_env", cred.Token)
		}
	})

	t.Run("PAT flag takes priority over env", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "ghp_from_env")
		cfg := &Config{
			Repo:        "jlaska/agent-sandbox",
			GitHubToken: "ghp_from_flag",
		}
		cred, err := ResolveGitHubCredential(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.Token != "ghp_from_flag" {
			t.Errorf("Token = %q, want ghp_from_flag (flag should win)", cred.Token)
		}
	})

	t.Run("App mode with keychain", func(t *testing.T) {
		origSilent := execCmdSilentFn
		origOutput := execCmdOutputFn
		origHTTP := httpDo
		defer func() {
			execCmdSilentFn = origSilent
			execCmdOutputFn = origOutput
			httpDo = origHTTP
		}()

		// Clear GITHUB_TOKEN so we fall through to App mode
		t.Setenv("GITHUB_TOKEN", "")

		// Mock keychain to report App PEM exists
		_, keyPEM := testRSAKey(t)
		execCmdSilentFn = func(_ context.Context, name string, args ...string) error {
			if name == "security" {
				return nil // keychain entry exists
			}
			return nil
		}
		execCmdOutputFn = func(_ context.Context, name string, args ...string) (string, error) {
			if name == "security" {
				return string(keyPEM), nil
			}
			return "", nil
		}

		// Mock HTTP for token minting
		httpDo = func(req *http.Request) (*http.Response, error) {
			resp := `{"token":"ghs_app_token","expires_at":"2025-01-01T01:00:00Z","repositories":[{"full_name":"jlaska/agent-sandbox"}]}`
			return &http.Response{
				StatusCode: 201,
				Body:       io.NopCloser(strings.NewReader(resp)),
			}, nil
		}

		cfg := &Config{Repo: "jlaska/agent-sandbox"}
		cred, err := ResolveGitHubCredential(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.Mode != GitHubModeApp {
			t.Errorf("Mode = %q, want %q", cred.Mode, GitHubModeApp)
		}
		if cred.Token != "ghs_app_token" {
			t.Errorf("Token = %q, want ghs_app_token", cred.Token)
		}
		if !cred.Revocable() {
			t.Error("App token should be revocable")
		}
	})

	t.Run("App mode, repo not accessible", func(t *testing.T) {
		origSilent := execCmdSilentFn
		origOutput := execCmdOutputFn
		origHTTP := httpDo
		defer func() {
			execCmdSilentFn = origSilent
			execCmdOutputFn = origOutput
			httpDo = origHTTP
		}()
		t.Setenv("GITHUB_TOKEN", "")
		_, keyPEM := testRSAKey(t)
		execCmdSilentFn = func(context.Context, string, ...string) error { return nil }
		execCmdOutputFn = func(_ context.Context, name string, _ ...string) (string, error) {
			if name == "security" {
				return string(keyPEM), nil
			}
			return "", nil
		}
		calls := 0
		httpDo = func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: 422,
				Body:       io.NopCloser(strings.NewReader(`{"message":"There is at least one repository that does not exist or is not accessible to the parent installation."}`)),
			}, nil
		}
		_, err := ResolveGitHubCredential(&Config{Repo: "jlaska/agent-sandbox-test"})
		if err == nil || !strings.Contains(err.Error(), `"jlaska/agent-sandbox-test" is not accessible via the GitHub App`) || !strings.Contains(err.Error(), "--list-repos") {
			t.Errorf("err = %v, want an actionable not-accessible error", err)
		}
		// One scoped mint is the whole access check: no installation-wide token.
		if calls != 1 {
			t.Errorf("%d GitHub API calls, want 1", calls)
		}
	})

	t.Run("neither available", func(t *testing.T) {
		origSilent := execCmdSilentFn
		defer func() { execCmdSilentFn = origSilent }()

		t.Setenv("GITHUB_TOKEN", "")

		// Mock keychain to report no App PEM
		execCmdSilentFn = func(_ context.Context, name string, args ...string) error {
			if name == "security" {
				return fmt.Errorf("not found")
			}
			return nil
		}

		cfg := &Config{Repo: "jlaska/agent-sandbox"}
		_, err := ResolveGitHubCredential(cfg)
		if err == nil {
			t.Error("expected error when no credentials available")
		}
		if !strings.Contains(err.Error(), "no GitHub credentials found") {
			t.Errorf("error should mention no credentials: %v", err)
		}
	})

	t.Run("PAT takes priority over App", func(t *testing.T) {
		origSilent := execCmdSilentFn
		defer func() { execCmdSilentFn = origSilent }()

		// Both PAT and App available
		execCmdSilentFn = func(_ context.Context, name string, args ...string) error {
			return nil // keychain exists
		}

		cfg := &Config{
			Repo:        "jlaska/agent-sandbox",
			GitHubToken: "ghp_explicit",
		}
		cred, err := ResolveGitHubCredential(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.Mode != GitHubModePAT {
			t.Errorf("Mode = %q, want %q (PAT should take priority)", cred.Mode, GitHubModePAT)
		}
	})
}
