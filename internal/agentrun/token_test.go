package agentrun

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testRSAKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return key, keyPEM
}

func TestGenerateAppJWT(t *testing.T) {
	_, keyPEM := testRSAKey(t)

	t.Run("valid JWT", func(t *testing.T) {
		jwt, err := generateAppJWT("12345", keyPEM)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		parts := strings.Split(jwt, ".")
		if len(parts) != 3 {
			t.Fatalf("JWT should have 3 parts, got %d", len(parts))
		}
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			t.Error("JWT contains empty parts")
		}
	})

	t.Run("invalid PEM", func(t *testing.T) {
		_, err := generateAppJWT("12345", []byte("not a pem key"))
		if err == nil {
			t.Error("expected error for invalid PEM")
		}
	})

	t.Run("empty key", func(t *testing.T) {
		_, err := generateAppJWT("12345", []byte{})
		if err == nil {
			t.Error("expected error for empty key")
		}
	})

	t.Run("PKCS8 key", func(t *testing.T) {
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		pkcs8PEM := pem.EncodeToMemory(&pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: pkcs8Bytes,
		})
		jwt, err := generateAppJWT("12345", pkcs8PEM)
		if err != nil {
			t.Fatalf("PKCS8 key should work: %v", err)
		}
		if jwt == "" {
			t.Error("JWT should not be empty")
		}
	})
}

func TestIsHexEncoded(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{"abcdef0123456789", true},
		{"ABCDEF0123456789", true},
		{"-----BEGIN RSA PRIVATE KEY-----", false},
		{"ghijkl", false},
		{"abc def", false},
		{"abc\ndef", false},
	}
	for _, tt := range tests {
		got := isHexEncoded(tt.input)
		if got != tt.want {
			t.Errorf("isHexEncoded(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestGetAppPrivateKey(t *testing.T) {
	origOutput := execCmdOutputFn
	defer func() { execCmdOutputFn = origOutput }()

	t.Run("normal PEM key", func(t *testing.T) {
		_, keyPEM := testRSAKey(t)
		execCmdOutputFn = func(_ context.Context, name string, args ...string) (string, error) {
			return string(keyPEM), nil
		}
		result, err := getAppPrivateKey()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(result), "BEGIN RSA PRIVATE KEY") {
			t.Error("expected PEM key in result")
		}
	})

	t.Run("hex-encoded PEM key", func(t *testing.T) {
		_, keyPEM := testRSAKey(t)
		hexKey := hex.EncodeToString(keyPEM)
		execCmdOutputFn = func(_ context.Context, name string, args ...string) (string, error) {
			return hexKey, nil
		}
		result, err := getAppPrivateKey()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(result), "BEGIN RSA PRIVATE KEY") {
			t.Error("expected PEM key after hex decode")
		}
	})

	t.Run("keychain error", func(t *testing.T) {
		execCmdOutputFn = func(_ context.Context, name string, args ...string) (string, error) {
			return "", fmt.Errorf("not found")
		}
		_, err := getAppPrivateKey()
		if err == nil {
			t.Error("expected error for keychain failure")
		}
	})

	t.Run("empty keychain value", func(t *testing.T) {
		execCmdOutputFn = func(_ context.Context, name string, args ...string) (string, error) {
			return "", nil
		}
		_, err := getAppPrivateKey()
		if err == nil {
			t.Error("expected error for empty keychain value")
		}
	})
}

// grantedPermsJSON is a 201 "permissions" field granting exactly
// installationTokenPermissions.
const grantedPermsJSON = `,"permissions":{"metadata":"read","contents":"write","pull_requests":"write","issues":"read","checks":"read","statuses":"read","actions":"read"}`

func TestInstallationTokenPermissionsExcludeDangerous(t *testing.T) {
	for _, k := range []string{"workflows", "administration"} {
		if level, ok := installationTokenPermissions[k]; ok {
			t.Errorf("installationTokenPermissions requests %s=%s; it must never be requested", k, level)
		}
	}
}

func TestMintInstallationToken(t *testing.T) {
	origHTTP := httpDo
	origBase := githubAPIBase
	defer func() {
		httpDo = origHTTP
		githubAPIBase = origBase
	}()

	t.Run("successful mint", func(t *testing.T) {
		httpDo = func(req *http.Request) (*http.Response, error) {
			if req.Method != "POST" {
				t.Errorf("expected POST, got %s", req.Method)
			}
			if !strings.Contains(req.URL.Path, "/access_tokens") {
				t.Errorf("unexpected path: %s", req.URL.Path)
			}
			if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ") {
				t.Error("missing Bearer authorization")
			}

			body, _ := io.ReadAll(req.Body)
			var reqBody map[string]interface{}
			if err := json.Unmarshal(body, &reqBody); err != nil {
				t.Fatalf("failed to parse request body: %v", err)
			}
			repos, ok := reqBody["repositories"].([]interface{})
			if !ok || len(repos) != 1 || repos[0] != "my-repo" {
				t.Errorf("expected repositories=[my-repo], got %v", reqBody["repositories"])
			}
			var perms struct {
				Permissions map[string]string `json:"permissions"`
			}
			_ = json.Unmarshal(body, &perms)
			if !reflect.DeepEqual(perms.Permissions, installationTokenPermissions) {
				t.Errorf("permissions = %v, want exactly %v", perms.Permissions, installationTokenPermissions)
			}

			resp := `{"token":"ghs_test123","expires_at":"2025-01-01T00:00:00Z","repositories":[{"full_name":"owner/my-repo"}]` + grantedPermsJSON + `}`
			return &http.Response{
				StatusCode: 201,
				Body:       io.NopCloser(strings.NewReader(resp)),
			}, nil
		}

		result, err := mintInstallationToken("test-jwt", "12345", "owner", "my-repo")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Token != "ghs_test123" {
			t.Errorf("token = %q, want ghs_test123", result.Token)
		}
		if !reflect.DeepEqual(result.Permissions, installationTokenPermissions) {
			t.Errorf("permissions = %v, want %v", result.Permissions, installationTokenPermissions)
		}
	})

	// A narrower grant is not a risk; GitHub reports what it granted.
	t.Run("narrower grant accepted", func(t *testing.T) {
		httpDo = func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 201,
				Body:       io.NopCloser(strings.NewReader(`{"token":"ghs_test","expires_at":"2025-01-01T00:00:00Z","repositories":[{"full_name":"owner/my-repo"}],"permissions":{"metadata":"read","contents":"read"}}`)),
			}, nil
		}
		result, err := mintInstallationToken("jwt", "12345", "owner", "my-repo")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := FormatPermissions(result.Permissions); got != "contents=read metadata=read" {
			t.Errorf("permissions = %q, want contents=read metadata=read", got)
		}
	})

	// The permission check fails closed on an over-broad or unverifiable grant,
	// and never echoes the token.
	for name, tc := range map[string]struct{ perms, want string }{
		"no permissions":   {``, "no permissions"},
		"extra permission": {`,"permissions":{"metadata":"read","workflows":"write"}`, "unrequested permission workflows=write"},
		"escalated level":  {`,"permissions":{"metadata":"read","issues":"write"}`, "issues=write, above the requested issues=read"},
		"admin level":      {`,"permissions":{"contents":"admin"}`, "contents=admin"},
		"unknown level":    {`,"permissions":{"contents":"owner"}`, "contents=owner"},
	} {
		t.Run(name, func(t *testing.T) {
			httpDo = func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 201,
					Body:       io.NopCloser(strings.NewReader(`{"token":"ghs_secret","expires_at":"2025-01-01T00:00:00Z","repositories":[{"full_name":"owner/my-repo"}]` + tc.perms + `}`)),
				}, nil
			}
			_, err := mintInstallationToken("jwt", "12345", "owner", "my-repo")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "ghs_secret") {
				t.Errorf("error leaks the token: %v", err)
			}
		})
	}

	t.Run("missing permission (422)", func(t *testing.T) {
		httpDo = func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 422,
				Body:       io.NopCloser(strings.NewReader(`{"message":"The permissions requested are not granted to this installation.","documentation_url":"https://docs.github.com/rest/apps/apps#create-an-installation-access-token-for-an-app","status":"422"}`)),
			}, nil
		}
		_, err := mintInstallationToken("jwt", "12345", "owner", "my-repo")
		if errors.Is(err, errRepoNotAccessible) {
			t.Fatalf("err = %v, must not be errRepoNotAccessible", err)
		}
		if !errors.Is(err, errMissingPermissions) || !strings.Contains(err.Error(), "actions=read") || !strings.Contains(err.Error(), "GitHub App") {
			t.Errorf("err = %v, want errMissingPermissions naming the permission set and the App settings", err)
		}
	})

	t.Run("API error", func(t *testing.T) {
		httpDo = func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 401,
				Body:       io.NopCloser(strings.NewReader(`{"message":"Bad credentials"}`)),
			}, nil
		}

		_, err := mintInstallationToken("bad-jwt", "12345", "owner", "repo")
		if err == nil {
			t.Error("expected error for 401 response")
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("error should mention status code: %v", err)
		}
	})

	t.Run("wrong repo in response", func(t *testing.T) {
		httpDo = func(req *http.Request) (*http.Response, error) {
			resp := `{"token":"ghs_test","expires_at":"2025-01-01T00:00:00Z","repositories":[{"full_name":"owner/other-repo"}]}`
			return &http.Response{
				StatusCode: 201,
				Body:       io.NopCloser(strings.NewReader(resp)),
			}, nil
		}

		_, err := mintInstallationToken("jwt", "12345", "owner", "my-repo")
		if err == nil {
			t.Error("expected error for wrong repo in response")
		}
		if !strings.Contains(err.Error(), "unexpected repositories") {
			t.Errorf("error should mention unexpected repositories: %v", err)
		}
	})

	// The scope check must not pass vacuously or with extra repos.
	for name, repos := range map[string]string{
		"no repositories list": ``,
		"empty repositories":   `,"repositories":[]`,
		"extra repository":     `,"repositories":[{"full_name":"owner/my-repo"},{"full_name":"owner/other-repo"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			httpDo = func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 201,
					Body:       io.NopCloser(strings.NewReader(`{"token":"ghs_test","expires_at":"2025-01-01T00:00:00Z"` + repos + `}`)),
				}, nil
			}
			if _, err := mintInstallationToken("jwt", "12345", "owner", "my-repo"); err == nil || !strings.Contains(err.Error(), "unexpected repositories") {
				t.Errorf("err = %v, want an unexpected-repositories error", err)
			}
		})
	}

	t.Run("repo not accessible (422)", func(t *testing.T) {
		httpDo = func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 422,
				Body:       io.NopCloser(strings.NewReader(`{"message":"There is at least one repository that does not exist or is not accessible to the parent installation."}`)),
			}, nil
		}
		_, err := mintInstallationToken("jwt", "12345", "owner", "renamed")
		if !errors.Is(err, errRepoNotAccessible) || !strings.Contains(err.Error(), "owner/renamed") {
			t.Errorf("err = %v, want errRepoNotAccessible naming owner/renamed", err)
		}
	})

	t.Run("empty token in response", func(t *testing.T) {
		httpDo = func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 201,
				Body:       io.NopCloser(strings.NewReader(`{"token":"","expires_at":"2025-01-01T00:00:00Z"}`)),
			}, nil
		}

		_, err := mintInstallationToken("jwt", "12345", "owner", "repo")
		if err == nil {
			t.Error("expected error for empty token")
		}
	})
}

func TestRevokeToken(t *testing.T) {
	origHTTP := httpDo
	defer func() { httpDo = origHTTP }()

	t.Run("successful revocation", func(t *testing.T) {
		httpDo = func(req *http.Request) (*http.Response, error) {
			if req.Method != "DELETE" {
				t.Errorf("expected DELETE, got %s", req.Method)
			}
			if !strings.Contains(req.URL.Path, "/installation/token") {
				t.Errorf("unexpected path: %s", req.URL.Path)
			}
			return &http.Response{
				StatusCode: 204,
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}

		err := RevokeToken("ghs_test123")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("revocation failure", func(t *testing.T) {
		httpDo = func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 401,
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}

		err := RevokeToken("bad-token")
		if err == nil {
			t.Error("expected error for failed revocation")
		}
	})
}

func TestMintTokenIntegration(t *testing.T) {
	origHTTP := httpDo
	origOutput := execCmdOutputFn
	origTime := timeNow
	defer func() {
		httpDo = origHTTP
		execCmdOutputFn = origOutput
		timeNow = origTime
	}()

	_, keyPEM := testRSAKey(t)

	execCmdOutputFn = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "security" {
			return string(keyPEM), nil
		}
		return "", nil
	}

	timeNow = func() time.Time {
		return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	httpDo = func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		var reqBody map[string]interface{}
		_ = json.Unmarshal(body, &reqBody)

		repos := reqBody["repositories"].([]interface{})
		if len(repos) != 1 || repos[0] != "agent-sandbox" {
			t.Errorf("expected repos=[agent-sandbox], got %v", repos)
		}

		resp := `{"token":"ghs_scoped","expires_at":"2025-01-01T01:00:00Z","repositories":[{"full_name":"jlaska/agent-sandbox"}]` + grantedPermsJSON + `}`
		return &http.Response{
			StatusCode: 201,
			Body:       io.NopCloser(strings.NewReader(resp)),
		}, nil
	}

	result, err := MintToken("jlaska/agent-sandbox")
	if err != nil {
		t.Fatalf("MintToken() error: %v", err)
	}
	if result.Token != "ghs_scoped" {
		t.Errorf("token = %q, want ghs_scoped", result.Token)
	}
}
