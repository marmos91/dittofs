package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
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

// getHealth GETs /health from addr and returns the response and its body,
// after confirming the reply came from the server under test.
//
// A dial that lands on an unrelated process listening on the same address
// otherwise surfaces as an assertion about response content, which points the
// reader at the health handler instead of at the collision. Only this server
// answers /health with a "dittofs" service field, so a reply that lacks it is
// reported as a foreign responder, quoting what actually answered.
func getHealth(t *testing.T, addr string) (*http.Response, []byte) {
	t.Helper()

	resp, err := http.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatalf("GET /health on %s: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		t.Fatalf("read /health body from %s: %v", addr, err)
	}

	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Service string `json:"service"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Data.Service != "dittofs" {
		t.Fatalf("/health on %s was answered by another process, not this server: "+
			"status=%d Server=%q Content-Type=%q body=%q",
			addr, resp.StatusCode, resp.Header.Get("Server"), resp.Header.Get("Content-Type"), body)
	}

	return resp, body
}

// testSetup creates control plane store and APIConfig for testing.
//
// Callers that start the server pass freePort(t) so the OS picks the port: a
// hard-coded number can already be held by unrelated local tooling, and the
// server binds 127.0.0.1 while a dial of "localhost" may resolve ::1 first, so
// the collision is not refused at bind time and instead surfaces downstream as
// an assertion about the reply. Callers that only construct a server and never
// listen pass 0 — the port is never bound, so no number is needed.
//
// None of these tests may call t.Parallel(): NewServer writes the process-wide
// mutex and block profile rates on every call, in both directions, so a
// concurrent NewServer would race the assertions in the pprof tests below.
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
	cpStore, cfg := testSetup(t, freePort(t))

	server, err := NewServer(cfg, nil, cpStore, Timeouts{Restore: 30 * time.Minute})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	// startServer also asserts Start returns nil on graceful shutdown.
	addr, stop := startServer(t, server)
	defer stop()

	resp, _ := getHealth(t, addr)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	if contentType := resp.Header.Get("Content-Type"); contentType != "application/json" {
		t.Errorf("Expected Content-Type 'application/json', got '%s'", contentType)
	}
}

func TestAPIServer_Port(t *testing.T) {
	port := freePort(t)
	cpStore, cfg := testSetup(t, port)

	server, err := NewServer(cfg, nil, cpStore, Timeouts{Restore: 30 * time.Minute})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	if server.Port() != port {
		t.Errorf("Expected port %d, got %d", port, server.Port())
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

	server, err := NewServer(cfg, nil, cpStore, Timeouts{Restore: 30 * time.Minute})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	// After applyDefaults, port should be 8080
	if server.Port() != 8080 {
		t.Errorf("Expected default port 8080, got %d", server.Port())
	}
}

func TestAPIServer_HealthEndpoint_NoRuntime(t *testing.T) {
	cpStore, cfg := testSetup(t, freePort(t))

	server, err := NewServer(cfg, nil, cpStore, Timeouts{Restore: 30 * time.Minute})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	addr, stop := startServer(t, server)
	defer stop()

	// Liveness (should always be OK), and answered by this server.
	resp, _ := getHealth(t, addr)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	// Test readiness endpoint (should be 503 with no runtime)
	resp2, err := http.Get("http://" + addr + "/health/ready")
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()

	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("Expected status %d, got %d", http.StatusServiceUnavailable, resp2.StatusCode)
	}
}

func TestAPIServer_RootRedirectsToHealth(t *testing.T) {
	cpStore, cfg := testSetup(t, freePort(t))

	server, err := NewServer(cfg, nil, cpStore, Timeouts{Restore: 30 * time.Minute})
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	addr, stop := startServer(t, server)
	defer stop()

	// Confirm this server owns the address before reading anything into the
	// redirect below.
	getHealth(t, addr)

	// Create a client that doesn't follow redirects
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get("http://" + addr + "/")
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

	_, err := NewServer(cfg, nil, cpStore, Timeouts{Restore: 30 * time.Minute})
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

// TestAPIServer_StopDrainsInflightHandlers pins the shutdown fence: Stop must
// not return while a request that outlived http.Server.Shutdown's deadline is
// still running, because the caller closes the control-plane store the handlers
// read directly once Stop reports. Two branches: a handler that finishes inside
// the drain bound is joined (Stop returns nil), and one that does not is
// abandoned after the bound (Stop returns an error) rather than blocking the
// process. The store close is ordered after whichever branch Stop took.
func TestAPIServer_StopDrainsInflightHandlers(t *testing.T) {
	t.Run("handler finishes inside the drain bound", func(t *testing.T) {
		cpStore, cfg := testSetup(t, freePort(t))
		server, err := NewServer(cfg, nil, cpStore, Timeouts{Restore: 30 * time.Minute})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		server.drainTimeout = 5 * time.Second

		entered := make(chan struct{})
		release := make(chan struct{})
		// Insert the blocking handler inside the in-flight tracker, so the
		// request is counted exactly as a real one would be.
		server.server.Handler = server.trackInflight(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
			w.WriteHeader(http.StatusOK)
		}))

		addr, stop := startServer(t, server)
		defer stop()

		go func() { _, _ = http.Get("http://" + addr + "/health") }()
		<-entered

		// Shutdown's own deadline expires immediately, so the drain is what has
		// to keep Stop waiting for the handler.
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer shutdownCancel()
		stopReturned := make(chan error, 1)
		go func() { stopReturned <- server.Stop(shutdownCtx) }()

		select {
		case err := <-stopReturned:
			t.Fatalf("Stop returned before the in-flight handler finished: %v", err)
		case <-time.After(200 * time.Millisecond):
		}

		close(release)
		select {
		case <-stopReturned:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop did not return after the handler finished")
		}
	})

	t.Run("handler outlasts the drain bound", func(t *testing.T) {
		cpStore, cfg := testSetup(t, freePort(t))
		server, err := NewServer(cfg, nil, cpStore, Timeouts{Restore: 30 * time.Minute})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		server.drainTimeout = 100 * time.Millisecond

		entered := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		server.server.Handler = server.trackInflight(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
		}))

		addr, stop := startServer(t, server)
		defer stop()

		go func() { _, _ = http.Get("http://" + addr + "/health") }()
		<-entered

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer shutdownCancel()
		select {
		case err := <-stopReturnedChan(server, shutdownCtx):
			if err == nil {
				t.Fatal("Stop returned nil, want an error for an undrained handler")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Stop did not give up on the undrained handler")
		}
	})
}

// stopReturnedChan runs Stop in a goroutine and returns its result channel.
func stopReturnedChan(s *Server, ctx context.Context) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- s.Stop(ctx) }()
	return ch
}

// TestAPIServer_DrainRefusesRequestsAdmittedAfterItStarts pins the admission
// gate: Drain sets draining before it waits, so a request arriving once the
// drain is under way is refused (503) instead of being counted and dispatched
// into a handler that would touch the store being closed. Without the gate, a
// request accepted by the still-open listener on the forced-exit path could Add
// after Wait began — a prohibited WaitGroup use — and reach the store after
// Drain returned.
func TestAPIServer_DrainRefusesRequestsAdmittedAfterItStarts(t *testing.T) {
	cpStore, cfg := testSetup(t, 0)
	server, err := NewServer(cfg, nil, cpStore, Timeouts{Restore: 30 * time.Minute})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// A drain that blocks until the counted request is released, so we can send
	// another request while it is still waiting and observe the gate rather than
	// a closed listener.
	server.drainTimeout = 5 * time.Second
	release := make(chan struct{})
	entered := make(chan struct{})
	go func() {
		server.trackInflight(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
		})).ServeHTTP(newRecordingResponseWriter(), httptest.NewRequest(http.MethodGet, "/health", nil))
	}()
	<-entered // the blocking request is counted before the drain starts

	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drainResult := make(chan bool, 1)
	go func() { drainResult <- server.Drain(drainCtx) }()

	// Wait until draining is set, then send a request through the tracker.
	deadline := time.After(2 * time.Second)
	for {
		server.drainMu.RLock()
		draining := server.draining
		server.drainMu.RUnlock()
		if draining {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Drain did not set the draining gate")
		case <-time.After(5 * time.Millisecond):
		}
	}

	rec := newRecordingResponseWriter()
	server.trackInflight(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler ran after draining began")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d for a request refused mid-drain", rec.status, http.StatusServiceUnavailable)
	}

	close(release)
	if !<-drainResult {
		t.Error("Drain returned false, want true once the counted request finished")
	}
}

// recordingResponseWriter captures the status written through it.
type recordingResponseWriter struct {
	header http.Header
	status int
}

func newRecordingResponseWriter() *recordingResponseWriter {
	return &recordingResponseWriter{header: make(http.Header)}
}

func (w *recordingResponseWriter) Header() http.Header { return w.header }

func (w *recordingResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(b), nil
}

func (w *recordingResponseWriter) WriteHeader(status int) { w.status = status }

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

	cpStore, cfg := testSetup(t, 0)
	cfg.Pprof = true
	cfg.PprofMutexRate = 137 // distinctive, non-default value

	if _, err := NewServer(cfg, nil, cpStore, Timeouts{Restore: 30 * time.Minute}); err != nil {
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

	cpStore, cfg := testSetup(t, 0)
	cfg.Pprof = false

	if _, err := NewServer(cfg, nil, cpStore, Timeouts{Restore: 30 * time.Minute}); err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	if got := goruntime.SetMutexProfileFraction(-1); got != 0 {
		t.Errorf("mutex profile fraction = %d, want 0 (reset when pprof off)", got)
	}
}

// TestGetHealth_NamesAForeignResponder pins the diagnosis getHealth exists for:
// when a process other than the server under test answers /health, the failure
// must identify that responder rather than read as a defect in the health
// handler. A status-and-content-type check cannot do that — the stand-in here
// passes both — so only the service field distinguishes "my server answered"
// from "something answered".
func TestGetHealth_NamesAForeignResponder(t *testing.T) {
	// Well-formed JSON, so the service field — not a parse failure — is what
	// has to tell the two servers apart.
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Server", "Caddy")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy","data":{"service":"something-else"}}`))
	}))
	defer foreign.Close()

	fake := &testing.T{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		getHealth(fake, strings.TrimPrefix(foreign.URL, "http://"))
	}()
	<-done

	if !fake.Failed() {
		t.Error("getHealth accepted a reply from a process that is not the server under test")
	}
}
