package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// waitForServerReady waits for the API server to either accept TCP connections
// on `addr` or fail to start. It races a TCP-dial poll against the server's
// errChan so a Start failure (bind error, etc.) surfaces immediately rather
// than as a generic listener timeout — and so the dial loop doesn't spuriously
// succeed against an unrelated process already listening on `addr`.
//
// Calls t.Fatalf on Start error, dial timeout, or unexpected nil from Start
// (Start returns nil only on graceful shutdown, which hasn't happened yet).
func waitForServerReady(t *testing.T, addr string, errChan <-chan error, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()

	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}

		select {
		case startErr := <-errChan:
			t.Fatalf("server.Start returned before listener was reachable: %v", startErr)
		case <-deadline:
			t.Fatalf("server did not start listening on %s within %s", addr, timeout)
		case <-tick.C:
		}
	}
}

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
	cfg := APIConfig{
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

	server, err := NewServer(cfg, nil, cpStore, 30*time.Minute)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start server in background
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()

	// Wait until the server's listener accepts connections — racing against
	// errChan so a bind failure surfaces as a real error, not a vague timeout.
	waitForServerReady(t, fmt.Sprintf("localhost:%d", cfg.Port), errChan, 5*time.Second)

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

	server, err := NewServer(cfg, nil, cpStore, 30*time.Minute)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	if server.Port() != 9999 {
		t.Errorf("Expected port 9999, got %d", server.Port())
	}
}

func TestAPIServer_DefaultConfig(t *testing.T) {
	cpStore, _ := testSetup(t, 0)

	cfg := APIConfig{
		// Port and timeouts not set - should use defaults
		JWT: JWTConfig{
			Secret: "test-secret-key-for-testing-only-32chars",
		},
	}

	server, err := NewServer(cfg, nil, cpStore, 30*time.Minute)
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

	server, err := NewServer(cfg, nil, cpStore, 30*time.Minute)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server in background
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()

	waitForServerReady(t, fmt.Sprintf("localhost:%d", cfg.Port), errChan, 5*time.Second)

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

	server, err := NewServer(cfg, nil, cpStore, 30*time.Minute)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server in background
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()

	waitForServerReady(t, fmt.Sprintf("localhost:%d", cfg.Port), errChan, 5*time.Second)

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

func TestAPIServer_InvalidJWTSecret(t *testing.T) {
	cpStore, _ := testSetup(t, 0)

	cfg := APIConfig{
		JWT: JWTConfig{
			Secret: "short", // Too short, should fail
		},
	}

	_, err := NewServer(cfg, nil, cpStore, 30*time.Minute)
	if err == nil {
		t.Fatal("Expected error for invalid JWT secret, got nil")
	}
}

func TestAPIConfig_PprofRateDefaults(t *testing.T) {
	tests := []struct {
		name          string
		in            APIConfig
		wantMutexRate int
		wantBlockRate int
	}{
		{
			name:          "pprof off leaves rates at zero",
			in:            APIConfig{Pprof: false},
			wantMutexRate: 0,
			wantBlockRate: 0,
		},
		{
			name:          "pprof on fills sensible defaults",
			in:            APIConfig{Pprof: true},
			wantMutexRate: 100,
			wantBlockRate: 1_000_000,
		},
		{
			name:          "explicit rates preserved when pprof on",
			in:            APIConfig{Pprof: true, PprofMutexRate: 5, PprofBlockRateNs: 250},
			wantMutexRate: 5,
			wantBlockRate: 250,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.in
			cfg.ApplyDefaults()
			if cfg.PprofMutexRate != tt.wantMutexRate {
				t.Errorf("PprofMutexRate = %d, want %d", cfg.PprofMutexRate, tt.wantMutexRate)
			}
			if cfg.PprofBlockRateNs != tt.wantBlockRate {
				t.Errorf("PprofBlockRateNs = %d, want %d", cfg.PprofBlockRateNs, tt.wantBlockRate)
			}
		})
	}
}

// TestNewServer_PprofSamplingWired verifies NewServer actually applies the
// mutex sampling fraction to the Go runtime when Pprof is enabled — the gap
// that left /debug/pprof/mutex header-only. SetMutexProfileFraction(-1) leaves
// the rate unchanged and returns the value currently in effect, so we read the
// prior value, restore it on cleanup, and never write a transient 0 that a
// concurrent test could observe. NewServer also sets the block profile rate;
// there is no runtime getter for it, so we cannot read/assert it, but cleanup
// resets it to 0 so this test cannot leak block sampling into later tests.
// Mutates global runtime state — do not add t.Parallel().
func TestNewServer_PprofSamplingWired(t *testing.T) {
	prev := goruntime.SetMutexProfileFraction(-1) // read without changing
	t.Cleanup(func() {
		goruntime.SetMutexProfileFraction(prev)
		goruntime.SetBlockProfileRate(0)
	})

	cpStore, cfg := testSetup(t, 18099)
	cfg.Pprof = true
	cfg.PprofMutexRate = 137 // distinctive, non-default value

	if _, err := NewServer(cfg, nil, cpStore, 30*time.Minute); err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	if got := goruntime.SetMutexProfileFraction(-1); got != 137 {
		t.Errorf("mutex profile fraction = %d, want 137", got)
	}
}

// TestNewServer_PprofOffResetsSampling verifies NewServer is authoritative when
// Pprof is off: it resets the global mutex fraction to 0 even if a prior caller
// (another server, bench tooling) had enabled sampling, so "no overhead when
// pprof is off" holds regardless of call order. Mutates global runtime state —
// do not add t.Parallel().
func TestNewServer_PprofOffResetsSampling(t *testing.T) {
	prev := goruntime.SetMutexProfileFraction(-1)
	t.Cleanup(func() {
		goruntime.SetMutexProfileFraction(prev)
		goruntime.SetBlockProfileRate(0)
	})

	goruntime.SetMutexProfileFraction(50) // simulate sampling left on by a prior caller

	cpStore, cfg := testSetup(t, 18100)
	cfg.Pprof = false

	if _, err := NewServer(cfg, nil, cpStore, 30*time.Minute); err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	if got := goruntime.SetMutexProfileFraction(-1); got != 0 {
		t.Errorf("mutex profile fraction = %d, want 0 (reset when pprof off)", got)
	}
}
