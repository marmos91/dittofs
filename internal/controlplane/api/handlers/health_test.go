package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
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

func TestReadiness_NoShares_Returns503(t *testing.T) {
	reg := runtime.New(nil)
	handler := NewHealthHandler(reg)
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

	if resp.Error != "no shares configured" {
		t.Errorf("Expected error 'no shares configured', got '%s'", resp.Error)
	}
}

func TestReadiness_WithSharesNoAdapters_Returns503(t *testing.T) {
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

	// Should return 503 because no adapters are running
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

	if resp.Error != "no adapters running" {
		t.Errorf("Expected error 'no adapters running', got '%s'", resp.Error)
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

func TestStores_NoRegistry_Returns503(t *testing.T) {
	handler := NewHealthHandler(nil)
	req := httptest.NewRequest("GET", "/health/stores", nil)
	w := httptest.NewRecorder()

	handler.Stores(w, req)

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
}

func TestStores_WithHealthyStores_ReturnsOK(t *testing.T) {
	reg := runtime.New(nil)

	// Register a healthy metadata store
	metaStore := memoryMeta.NewMemoryMetadataStoreWithDefaults()
	if err := reg.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("Failed to register metadata store: %v", err)
	}

	handler := NewHealthHandler(reg)
	req := httptest.NewRequest("GET", "/health/stores", nil)
	w := httptest.NewRecorder()

	handler.Stores(w, req)

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

	// Check that we got the stores response
	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected Data to be a map, got %T", resp.Data)
	}

	metadataStores, ok := data["metadata_stores"].([]interface{})
	if !ok {
		t.Fatalf("Expected metadata_stores to be an array")
	}
	if len(metadataStores) != 1 {
		t.Errorf("Expected 1 metadata store, got %d", len(metadataStores))
	}
}

func TestStores_ChecksMetadataStoreHealth(t *testing.T) {
	reg := runtime.New(nil)

	// Register a healthy metadata store
	metaStore := memoryMeta.NewMemoryMetadataStoreWithDefaults()
	if err := reg.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("Failed to register metadata store: %v", err)
	}

	handler := NewHealthHandler(reg)
	req := httptest.NewRequest("GET", "/health/stores", nil)
	w := httptest.NewRecorder()

	handler.Stores(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp Response
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	data := resp.Data.(map[string]interface{})
	metadataStores := data["metadata_stores"].([]interface{})

	if len(metadataStores) != 1 {
		t.Fatalf("Expected 1 metadata store, got %d", len(metadataStores))
	}

	store := metadataStores[0].(map[string]interface{})
	if store["name"] != "test-meta" {
		t.Errorf("Expected store name 'test-meta', got '%s'", store["name"])
	}
	if store["status"] != "healthy" {
		t.Errorf("Expected store status 'healthy', got '%s'", store["status"])
	}
	if store["latency"] == nil || store["latency"] == "" {
		t.Error("Expected latency to be set")
	}
}
