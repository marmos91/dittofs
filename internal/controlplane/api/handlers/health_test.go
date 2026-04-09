package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
	"github.com/marmos91/dittofs/pkg/metadata"
	memoryMeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// mockAdapter implements runtime.ProtocolAdapter for testing
type mockAdapter struct {
	protocol string
	port     int
}

func (m *mockAdapter) Serve(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (m *mockAdapter) Stop(ctx context.Context) error {
	return nil
}

func (m *mockAdapter) Protocol() string {
	return m.protocol
}

func (m *mockAdapter) Port() int {
	return m.port
}

// Healthcheck satisfies the [adapters.ProtocolAdapter] interface (the
// new method added in phase U-C). The mock has no real lifecycle
// state, so it always reports healthy with the current timestamp;
// tests that need richer behaviour should use a dedicated fake.
func (m *mockAdapter) Healthcheck(_ context.Context) health.Report {
	return health.Report{
		Status:    health.StatusHealthy,
		CheckedAt: time.Now().UTC(),
	}
}

func TestLiveness_ReturnsOK(t *testing.T) {
	handler := NewHealthHandler(nil)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	handler.Liveness(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("Expected status 'healthy', got '%s'", resp.Status)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected Data to be a map, got %T", resp.Data)
	}

	if data["service"] != "dittofs" {
		t.Errorf("Expected service 'dittofs', got '%s'", data["service"])
	}

	if data["started_at"] == nil || data["started_at"] == "" {
		t.Error("Expected started_at to be set")
	}

	if data["uptime"] == nil || data["uptime"] == "" {
		t.Error("Expected uptime to be set")
	}

	// With nil registry, control_plane_db should be "unknown"
	if data["control_plane_db"] != "unknown" {
		t.Errorf("Expected control_plane_db 'unknown', got '%s'", data["control_plane_db"])
	}
}

func TestLiveness_WithRegistry_ReturnsDBReachable(t *testing.T) {
	// Create a real in-memory SQLite store for the control plane DB
	cpStore, err := store.New(&store.Config{
		Type:   store.DatabaseTypeSQLite,
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("Failed to create in-memory SQLite store: %v", err)
	}
	defer func() { _ = cpStore.Close() }()

	reg := runtime.New(cpStore)
	handler := NewHealthHandler(reg)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	handler.Liveness(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("Expected status 'healthy', got '%s'", resp.Status)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected Data to be a map, got %T", resp.Data)
	}

	if data["control_plane_db"] != "reachable" {
		t.Errorf("Expected control_plane_db 'reachable', got '%s'", data["control_plane_db"])
	}
}

func TestLiveness_WithRegistry_ReturnsDBUnreachableWhenHealthcheckFails(t *testing.T) {
	// Create a real in-memory SQLite store, then close it so Healthcheck fails.
	cpStore, err := store.New(&store.Config{
		Type:   store.DatabaseTypeSQLite,
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("Failed to create in-memory SQLite store: %v", err)
	}
	// Close the store immediately so the DB ping will fail.
	_ = cpStore.Close()

	reg := runtime.New(cpStore)
	handler := NewHealthHandler(reg)
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	handler.Liveness(w, req)

	// Degraded is still "alive" -- HTTP 200
	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Status != "degraded" {
		t.Errorf("Expected status 'degraded', got '%s'", resp.Status)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected Data to be a map, got %T", resp.Data)
	}

	if data["control_plane_db"] != "unreachable" {
		t.Errorf("Expected control_plane_db 'unreachable', got '%s'", data["control_plane_db"])
	}
}

func TestReadiness_NoRegistry_Returns503(t *testing.T) {
	handler := NewHealthHandler(nil)
	req := httptest.NewRequest("GET", "/health/ready", nil)
	w := httptest.NewRecorder()

	handler.Readiness(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected status %d, got %d", http.StatusServiceUnavailable, w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Status != "unhealthy" {
		t.Errorf("Expected status 'unhealthy', got '%s'", resp.Status)
	}

	if resp.Error != "registry not initialized" {
		t.Errorf("Expected error 'registry not initialized', got '%s'", resp.Error)
	}
}

func TestReadiness_NoShares_ReturnsOK(t *testing.T) {
	reg := runtime.New(nil)
	handler := NewHealthHandler(reg)
	req := httptest.NewRequest("GET", "/health/ready", nil)
	w := httptest.NewRecorder()

	handler.Readiness(w, req)

	// Readiness returns OK if registry is initialized, even without shares
	// This allows Kubernetes pods to become ready before configuration is complete
	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("Expected status 'healthy', got '%s'", resp.Status)
	}
}

func TestReadiness_WithSharesNoAdapters_ReturnsOK(t *testing.T) {
	ctx := context.Background()
	reg := runtime.New(nil)

	// Register metadata store
	metaStore := memoryMeta.NewMemoryMetadataStoreWithDefaults()
	if err := reg.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("Failed to register metadata store: %v", err)
	}

	// Add a share
	shareConfig := &runtime.ShareConfig{
		Name:          "/test",
		MetadataStore: "test-meta",
		RootAttr:      &metadata.FileAttr{},
	}
	if err := reg.AddShare(ctx, shareConfig); err != nil {
		t.Fatalf("Failed to add share: %v", err)
	}

	handler := NewHealthHandler(reg)
	req := httptest.NewRequest("GET", "/health/ready", nil)
	w := httptest.NewRecorder()

	handler.Readiness(w, req)

	// Readiness returns OK if registry is initialized, even without adapters
	// This allows Kubernetes pods to become ready before adapters start
	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("Expected status 'healthy', got '%s'", resp.Status)
	}

	// Should still report share count
	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected Data to be a map, got %T", resp.Data)
	}

	if data["shares"].(float64) != 1 {
		t.Errorf("Expected 1 share, got %v", data["shares"])
	}
}

func TestReadiness_WithSharesAndAdapters_ReturnsOK(t *testing.T) {
	ctx := context.Background()
	reg := runtime.New(nil)

	// Register metadata store
	metaStore := memoryMeta.NewMemoryMetadataStoreWithDefaults()
	if err := reg.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("Failed to register metadata store: %v", err)
	}

	// Add a share
	shareConfig := &runtime.ShareConfig{
		Name:          "/test",
		MetadataStore: "test-meta",
		RootAttr:      &metadata.FileAttr{},
	}
	if err := reg.AddShare(ctx, shareConfig); err != nil {
		t.Fatalf("Failed to add share: %v", err)
	}

	// Add a mock adapter
	adapter := &mockAdapter{protocol: "test", port: 12345}
	if err := reg.AddAdapter(adapter); err != nil {
		t.Fatalf("Failed to add adapter: %v", err)
	}
	defer func() {
		_ = reg.StopAllAdapters()
	}()

	handler := NewHealthHandler(reg)
	req := httptest.NewRequest("GET", "/health/ready", nil)
	w := httptest.NewRecorder()

	handler.Readiness(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("Expected status 'healthy', got '%s'", resp.Status)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected Data to be a map, got %T", resp.Data)
	}

	if data["shares"].(float64) != 1 {
		t.Errorf("Expected 1 share, got %v", data["shares"])
	}

	// Check adapter info in response
	adapters, ok := data["adapters"].(map[string]interface{})
	if !ok {
		t.Fatalf("Expected adapters to be a map, got %T", data["adapters"])
	}

	if adapters["running"].(float64) != 1 {
		t.Errorf("Expected 1 running adapter, got %v", adapters["running"])
	}
}
