package agentrun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// GitHub credential modes.
const (
	GitHubModeApp = "app"
	GitHubModePAT = "pat"
)

// GitHubCredential holds resolved GitHub auth.
type GitHubCredential struct {
	Mode      string
	Token     string
	ExpiresAt time.Time // zero for PAT
}

// appKeychainExists checks if the GitHub App private key is in the keychain.
func appKeychainExists() bool {
	return execCmdSilent(context.Background(), "security", "find-generic-password",
		"-s", KeychainService, "-a", KeychainAccount) == nil
}

// Revocable returns true if the token should be revoked on cleanup.
func (gc *GitHubCredential) Revocable() bool {
	return gc.Mode == GitHubModeApp
}

// ResolveGitHubCredential determines the GitHub auth mode and obtains a token.
// Priority: explicit --github-token / GITHUB_TOKEN env → App mode (keychain).
func ResolveGitHubCredential(cfg *Config) (*GitHubCredential, error) {
	// 1. Explicit PAT from flag or env
	pat := cfg.GitHubToken
	if pat == "" {
		pat = os.Getenv("GITHUB_TOKEN")
	}
	if pat != "" {
		return &GitHubCredential{
			Mode:  GitHubModePAT,
			Token: pat,
		}, nil
	}

	// 2. GitHub App token minting (requires PEM in keychain)
	if appKeychainExists() {
		result, err := MintToken(cfg.Repo)
		if err != nil {
			return nil, fmt.Errorf("GitHub App token minting failed: %w", err)
		}
		return &GitHubCredential{
			Mode:      GitHubModeApp,
			Token:     result.Token,
			ExpiresAt: result.ExpiresAt,
		}, nil
	}

	return nil, fmt.Errorf("no GitHub credentials found; either:\n" +
		"  - Install the GitHub App and store its key in Keychain (" + KeychainService + ")\n" +
		"  - Provide --github-token <PAT> or set GITHUB_TOKEN env var")
}

// ListAppRepos queries the GitHub App installation for accessible repositories.
// It mints a short-lived installation token (scoped to all repos the App can
// access) and queries GET /installation/repositories.
func ListAppRepos() ([]string, error) {
	keyPEM, err := getAppPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("failed to get App private key: %w", err)
	}

	jwt, err := generateAppJWT(AppID, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to generate JWT: %w", err)
	}

	// Mint a token without repo scope to list all accessible repos.
	token, err := mintInstallationTokenUnscoped(jwt, InstallationID)
	if err != nil {
		return nil, fmt.Errorf("failed to mint token for repo listing: %w", err)
	}

	url := fmt.Sprintf("%s/installation/repositories", githubAPIBase)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpDo(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing repos failed (HTTP %d): %s", resp.StatusCode, redactTokenInResponse(string(body)))
	}

	var result struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	var repos []string
	for _, r := range result.Repositories {
		repos = append(repos, r.FullName)
	}
	return repos, nil
}

// IsAppRepoAccessible checks if a repository is accessible via the GitHub App.
func IsAppRepoAccessible(repo string) (bool, error) {
	repos, err := ListAppRepos()
	if err != nil {
		return false, err
	}
	for _, r := range repos {
		if r == repo {
			return true, nil
		}
	}
	return false, nil
}
