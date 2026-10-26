package agentrun

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	InstallationID  = "156618153"
	KeychainService = "github-app-jlaska-agent"
	KeychainAccount = "private-key"
)

// Replaceable for testing.
var (
	httpDo        = defaultHTTPDo
	githubAPIBase = "https://api.github.com"
	timeNow       = time.Now
)

// TokenResult holds a minted installation token and its expiration.
type TokenResult struct {
	Token     string
	ExpiresAt time.Time
}

// errRepoNotAccessible means the GitHub App installation cannot reach the
// repository a token was requested for.
var errRepoNotAccessible = errors.New("repository is not accessible via the GitHub App")

// MintToken mints a repository-scoped GitHub App installation token.
// The returned token is authorized only for the specified repository.
func MintToken(repo string) (*TokenResult, error) {
	owner, name, err := ParseRepo(repo)
	if err != nil {
		return nil, fmt.Errorf("invalid repository: %w", err)
	}

	keyPEM, err := getAppPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("failed to get App private key: %w", err)
	}

	jwt, err := generateAppJWT(AppID, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to generate JWT: %w", err)
	}

	return mintInstallationToken(jwt, InstallationID, owner, name)
}

// RevokeToken revokes a GitHub App installation token.
func RevokeToken(token string) error {
	req, err := http.NewRequest("DELETE", githubAPIBase+"/installation/token", nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpDo(req)
	if err != nil {
		return fmt.Errorf("revocation request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("token revocation failed (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func getAppPrivateKey() ([]byte, error) {
	raw, err := execCmdOutputFn(context.Background(), "security", "find-generic-password",
		"-s", KeychainService,
		"-a", KeychainAccount,
		"-w")
	if err != nil {
		return nil, fmt.Errorf("keychain lookup failed: %w", err)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("keychain entry is empty")
	}

	// macOS Keychain hex-encodes multi-line values stored via -w.
	if isHexEncoded(raw) {
		decoded, err := hex.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("failed to decode hex key: %w", err)
		}
		return decoded, nil
	}

	return []byte(raw), nil
}

func generateAppJWT(appID string, keyPEM []byte) (string, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block")
	}

	var rsaKey *rsa.PrivateKey

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return "", fmt.Errorf("failed to parse private key: PKCS1=%w, PKCS8=%v", err, err2)
		}
		var ok bool
		rsaKey, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("private key is not RSA")
		}
	} else {
		rsaKey = key
	}

	now := timeNow()
	header := base64URLEncode([]byte(`{"alg":"RS256","typ":"JWT"}`))

	payload := map[string]interface{}{
		"iss": appID,
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal JWT payload: %w", err)
	}
	payloadB64 := base64URLEncode(payloadJSON)

	sigInput := header + "." + payloadB64
	hashed := sha256.Sum256([]byte(sigInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("failed to sign JWT: %w", err)
	}

	return sigInput + "." + base64URLEncode(sig), nil
}

// mintInstallationTokenUnscoped mints an installation token with access to all
// repos the App is installed on (no repository scope restriction).
func mintInstallationTokenUnscoped(jwt, installationID string) (string, error) {
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", githubAPIBase, installationID)

	req, err := http.NewRequest("POST", url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpDo(req)
	if err != nil {
		return "", fmt.Errorf("API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("token minting failed (HTTP %d): %s", resp.StatusCode, redactTokenInResponse(string(respBody)))
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("API returned empty token")
	}

	return result.Token, nil
}

func mintInstallationToken(jwt, installationID, owner, repoName string) (*TokenResult, error) {
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", githubAPIBase, installationID)

	body := map[string]interface{}{
		"repositories": []string{repoName},
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpDo(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// GitHub answers 422 when the repository is not one the installation can
	// reach (including a repo's old name after a rename).
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return nil, fmt.Errorf("%w: %s/%s (HTTP 422): %s", errRepoNotAccessible, owner, repoName, redactTokenInResponse(string(respBody)))
	}
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("token minting failed (HTTP %d): %s", resp.StatusCode, redactTokenInResponse(string(respBody)))
	}

	var result struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
		Repos     []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if result.Token == "" {
		return nil, fmt.Errorf("API returned empty token")
	}

	// Validate that the token is scoped to exactly the requested repo. A
	// response without the repositories list is not proof of scoping, so it
	// fails closed too.
	expectedRepo := owner + "/" + repoName
	if len(result.Repos) != 1 || result.Repos[0].FullName != expectedRepo {
		names := make([]string, len(result.Repos))
		for i, r := range result.Repos {
			names[i] = r.FullName
		}
		return nil, fmt.Errorf("token scoped to unexpected repositories %q (expected only %q)", names, expectedRepo)
	}

	expiresAt, _ := time.Parse(time.RFC3339, result.ExpiresAt)

	return &TokenResult{
		Token:     result.Token,
		ExpiresAt: expiresAt,
	}, nil
}

func isHexEncoded(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		isDigit := c >= '0' && c <= '9'
		isLower := c >= 'a' && c <= 'f'
		isUpper := c >= 'A' && c <= 'F'
		if !isDigit && !isLower && !isUpper {
			return false
		}
	}
	return true
}

func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func redactTokenInResponse(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func defaultHTTPDo(req *http.Request) (*http.Response, error) {
	return http.DefaultClient.Do(req)
}
