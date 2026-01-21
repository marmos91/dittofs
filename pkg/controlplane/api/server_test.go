package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// testSetup creates control plane store and APIConfig for testing.
func testSetup(t *testing.T, port int) (store.Store, APIConfig) {
	t.Helper()

	// Create in-memory SQLite control plane store for testing
	dbConfig := store.Config{
		Type: "sqlite",
		SQLite: store.SQLiteConfig{
			Path: ":memory:", // In-memory database for testing
		},
	}
	cpStore, err := store.New(&dbConfig)
	if err != nil {
		t.Fatalf("Failed to create control plane store: %v", err)
	}

	// Create API config with a valid JWT secret (>= 32 characters)
	enabled := true
	cfg := APIConfig{
		Enabled:      &enabled,
		Port:         port,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		IdleTimeout:  10 * time.Second,
		JWT: JWTConfig{
			Secret:               "test-secret-key-for-testing-only-32chars",
			AccessTokenDuration:  15 * time.Minute,
			RefreshTokenDuration: 7 * 24 * time.Hour,
		},
	}

	return cpStore, cfg
}

func TestAPIServer_Lifecycle(t *testing.T) {
	cpStore, cfg := testSetup(t, 18080)

	server, err := NewServer(cfg, nil, cpStore)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start server in background
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Make request to health endpoint
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", cfg.Port))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	// Verify response content type
	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Expected Content-Type 'application/json', got '%s'", contentType)
	}

	// Shutdown
	cancel()

	// Wait for server to stop
	select {
	case err := <-errChan:
		if err != nil {
			t.Errorf("Expected nil on graceful shutdown, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Server did not shutdown in time")
	}
}

func TestAPIServer_Port(t *testing.T) {
	cpStore, cfg := testSetup(t, 9999)

	server, err := NewServer(cfg, nil, cpStore)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	if server.Port() != 9999 {
		t.Errorf("Expected port 9999, got %d", server.Port())
	}
}

func TestAPIServer_DefaultConfig(t *testing.T) {
	cpStore, _ := testSetup(t, 0)

	enabled := true
	cfg := APIConfig{
		Enabled: &enabled,
		// Port and timeouts not set - should use defaults
		JWT: JWTConfig{
			Secret: "test-secret-key-for-testing-only-32chars",
		},
	}

	server, err := NewServer(cfg, nil, cpStore)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	// After applyDefaults, port should be 8080
	if server.Port() != 8080 {
		t.Errorf("Expected default port 8080, got %d", server.Port())
	}
}

func TestAPIServer_HealthEndpoint_NoRuntime(t *testing.T) {
	cpStore, cfg := testSetup(t, 18081)

	server, err := NewServer(cfg, nil, cpStore)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server in background
	go func() {
		_ = server.Start(ctx)
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Test liveness endpoint (should always be OK)
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", cfg.Port))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	// Test readiness endpoint (should be 503 with no runtime)
	resp2, err := http.Get(fmt.Sprintf("http://localhost:%d/health/ready", cfg.Port))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()

	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Expected status %d, got %d", http.StatusServiceUnavailable, resp2.StatusCode)
	}
}

func TestAPIServer_RootRedirectsToHealth(t *testing.T) {
	cpStore, cfg := testSetup(t, 18082)

	server, err := NewServer(cfg, nil, cpStore)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server in background
	go func() {
		_ = server.Start(ctx)
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Create a client that doesn't follow redirects
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/", cfg.Port))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("Expected status %d, got %d", http.StatusTemporaryRedirect, resp.StatusCode)
	}

	location := resp.Header.Get("Location")
	if location != "/health" {
		t.Errorf("Expected redirect to '/health', got '%s'", location)
	}
}

func TestAPIServer_StoresEndpoint(t *testing.T) {
	cpStore, cfg := testSetup(t, 18083)

	server, err := NewServer(cfg, nil, cpStore)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server in background
	go func() {
		_ = server.Start(ctx)
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Test stores endpoint (should be 503 with no runtime)
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health/stores", cfg.Port))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Expected status %d, got %d", http.StatusServiceUnavailable, resp.StatusCode)
	}

	var response struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if response.Status != "unhealthy" {
		t.Errorf("Expected status 'unhealthy', got '%s'", response.Status)
	}
}

func TestAPIServer_InvalidJWTSecret(t *testing.T) {
	cpStore, _ := testSetup(t, 0)

	enabled := true
	cfg := APIConfig{
		Enabled: &enabled,
		JWT: JWTConfig{
			Secret: "short", // Too short, should fail
		},
	}

	_, err := NewServer(cfg, nil, cpStore)
	if err == nil {
		t.Fatal("Expected error for invalid JWT secret, got nil")
	}
}
