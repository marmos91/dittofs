package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// newTokenServer returns an httptest server that answers login with the given
// TokenResponse. Tests use it to simulate servers that strip tokens from the
// body (e.g. a misconfigured dfs-pro middleware).
func newTokenServer(t *testing.T, resp apiclient.TokenResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/login" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// withLoginFlags sets the package-level flag vars for runLogin and restores
// them on cleanup. runLogin reads these directly instead of cobra args.
func withLoginFlags(t *testing.T, server, username, password string) {
	t.Helper()
	origServer, origUser, origPass, origCtx := loginServer, loginUsername, loginPassword, loginContextName
	loginServer = server
	loginUsername = username
	loginPassword = password
	loginContextName = "test-ctx"
	t.Cleanup(func() {
		loginServer = origServer
		loginUsername = origUser
		loginPassword = origPass
		loginContextName = origCtx
	})
}

// withIsolatedConfig redirects the config home to a temp dir so the test
// doesn't touch the real config. credentials.Store reads APPDATA on Windows
// and XDG_CONFIG_HOME elsewhere, so set the right one for the current OS.
func withIsolatedConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", dir)
	} else {
		t.Setenv("XDG_CONFIG_HOME", dir)
	}
	return dir
}

func TestLoginRejectsEmptyAccessToken(t *testing.T) {
	server := newTokenServer(t, apiclient.TokenResponse{
		AccessToken:  "",
		RefreshToken: "refresh-xyz",
		TokenType:    "Bearer",
		ExpiresIn:    900,
	})
	defer server.Close()

	cfgDir := withIsolatedConfig(t)
	withLoginFlags(t, server.URL, "admin", "password123")

	err := runLogin(loginCmd, nil)
	if err == nil {
		t.Fatal("expected error when server returns empty access token, got nil")
	}
	if !strings.Contains(err.Error(), "no tokens") {
		t.Errorf("error should mention missing tokens, got: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cfgDir, "dfsctl", "config.json")); err == nil {
		t.Error("config file should not be written when tokens are empty")
	}
}

func TestLoginRejectsEmptyRefreshToken(t *testing.T) {
	server := newTokenServer(t, apiclient.TokenResponse{
		AccessToken:  "access-abc",
		RefreshToken: "",
		TokenType:    "Bearer",
		ExpiresIn:    900,
	})
	defer server.Close()

	cfgDir := withIsolatedConfig(t)
	withLoginFlags(t, server.URL, "admin", "password123")

	err := runLogin(loginCmd, nil)
	if err == nil {
		t.Fatal("expected error when server returns empty refresh token, got nil")
	}
	if !strings.Contains(err.Error(), "no tokens") {
		t.Errorf("error should mention missing tokens, got: %v", err)
	}

	if _, err := os.Stat(filepath.Join(cfgDir, "dfsctl", "config.json")); err == nil {
		t.Error("config file should not be written when tokens are empty")
	}
}

func TestLoginSavesContextOnSuccess(t *testing.T) {
	server := newTokenServer(t, apiclient.TokenResponse{
		AccessToken:  "access-abc",
		RefreshToken: "refresh-xyz",
		TokenType:    "Bearer",
		ExpiresIn:    900,
	})
	defer server.Close()

	cfgDir := withIsolatedConfig(t)
	withLoginFlags(t, server.URL, "admin", "password123")

	if err := runLogin(loginCmd, nil); err != nil {
		t.Fatalf("login should succeed with valid tokens, got: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cfgDir, "dfsctl", "config.json"))
	if err != nil {
		t.Fatalf("config file should be written on success: %v", err)
	}

	var cfg struct {
		CurrentContext string `json:"current_context"`
		Contexts       map[string]struct {
			ServerURL    string `json:"server_url"`
			Username     string `json:"username"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config.json is not valid JSON: %v\n%s", err, data)
	}

	if cfg.CurrentContext != "test-ctx" {
		t.Errorf("current_context = %q, want %q", cfg.CurrentContext, "test-ctx")
	}
	got, ok := cfg.Contexts["test-ctx"]
	if !ok {
		t.Fatalf("context %q not found in config: %+v", "test-ctx", cfg.Contexts)
	}
	if got.AccessToken != "access-abc" {
		t.Errorf("access_token = %q, want %q", got.AccessToken, "access-abc")
	}
	if got.RefreshToken != "refresh-xyz" {
		t.Errorf("refresh_token = %q, want %q", got.RefreshToken, "refresh-xyz")
	}
	if got.Username != "admin" {
		t.Errorf("username = %q, want %q", got.Username, "admin")
	}
	if got.ServerURL != server.URL {
		t.Errorf("server_url = %q, want %q", got.ServerURL, server.URL)
	}
}
