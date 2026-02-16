//go:build integration

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

func setupAdapterSettingsTest(t *testing.T) (store.Store, *AdapterSettingsHandler, *models.AdapterConfig) {
	t.Helper()

	dbConfig := store.Config{
		Type: "sqlite",
		SQLite: store.SQLiteConfig{
			Path: ":memory:",
		},
	}
	cpStore, err := store.New(&dbConfig)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	ctx := context.Background()

	// Create NFS adapter
	adapter := &models.AdapterConfig{
		ID:      uuid.New().String(),
		Type:    "nfs",
		Enabled: true,
		Port:    12049,
	}
	if _, err := cpStore.CreateAdapter(ctx, adapter); err != nil {
		t.Fatalf("Failed to create adapter: %v", err)
	}

	// Ensure settings exist for the adapter
	if err := cpStore.EnsureAdapterSettings(ctx); err != nil {
		t.Fatalf("Failed to ensure adapter settings: %v", err)
	}

	handler := NewAdapterSettingsHandler(cpStore, nil)
	return cpStore, handler, adapter
}

func makeAdapterSettingsRequest(t *testing.T, method, adapterType, path string, body any, queryParams ...string) *http.Request {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewReader(data)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	url := "/api/v1/adapters/" + adapterType + "/settings"
	if path != "" {
		url += "/" + path
	}
	if len(queryParams) > 0 {
		url += "?" + queryParams[0]
	}

	req := httptest.NewRequest(method, url, reqBody)
	req.Header.Set("Content-Type", "application/json")

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("type", adapterType)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	return req
}

func TestGetNFSSettings_OK(t *testing.T) {
	_, handler, _ := setupAdapterSettingsTest(t)

	req := makeAdapterSettingsRequest(t, http.MethodGet, "nfs", "", nil)
	w := httptest.NewRecorder()

	handler.GetSettings(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GetSettings() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp NFSSettingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	// Verify all default fields are present
	if resp.MinVersion == "" {
		t.Error("Expected non-empty MinVersion")
	}
	if resp.MaxVersion == "" {
		t.Error("Expected non-empty MaxVersion")
	}
	if resp.LeaseTime == 0 {
		t.Error("Expected non-zero LeaseTime")
	}
	if resp.Version != 1 {
		t.Errorf("Version = %d, want 1", resp.Version)
	}
}

func TestGetNFSSettings_NotFound(t *testing.T) {
	dbConfig := store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	}
	cpStore, err := store.New(&dbConfig)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	handler := NewAdapterSettingsHandler(cpStore, nil)

	req := makeAdapterSettingsRequest(t, http.MethodGet, "nfs", "", nil)
	w := httptest.NewRecorder()

	handler.GetSettings(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("GetSettings() status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestGetNFSDefaults(t *testing.T) {
	_, handler, _ := setupAdapterSettingsTest(t)

	req := makeAdapterSettingsRequest(t, http.MethodGet, "nfs", "defaults", nil)
	w := httptest.NewRecorder()

	handler.GetDefaults(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GetDefaults() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SettingsDefaultsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if resp.Defaults == nil {
		t.Error("Expected non-nil defaults")
	}
	if resp.Ranges == nil {
		t.Error("Expected non-nil ranges")
	}

	// Verify ranges contain lease_time
	if _, ok := resp.Ranges["lease_time"]; !ok {
		t.Error("Expected lease_time in ranges")
	}
}

func TestPatchNFSSettings_PartialUpdate(t *testing.T) {
	cpStore, handler, _ := setupAdapterSettingsTest(t)
	ctx := context.Background()

	// Get original settings for comparison
	adapter, _ := cpStore.GetAdapter(ctx, "nfs")
	original, _ := cpStore.GetNFSAdapterSettings(ctx, adapter.ID)
	originalGracePeriod := original.GracePeriod

	leaseTime := 120
	patchReq := PatchNFSSettingsRequest{
		LeaseTime: &leaseTime,
	}

	req := makeAdapterSettingsRequest(t, http.MethodPatch, "nfs", "", patchReq)
	w := httptest.NewRecorder()

	handler.PatchSettings(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("PatchSettings() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp NFSSettingsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.LeaseTime != 120 {
		t.Errorf("LeaseTime = %d, want 120", resp.LeaseTime)
	}
	if resp.GracePeriod != originalGracePeriod {
		t.Errorf("GracePeriod changed: %d -> %d, should be unchanged", originalGracePeriod, resp.GracePeriod)
	}
}

func TestPatchNFSSettings_ValidationError(t *testing.T) {
	_, handler, _ := setupAdapterSettingsTest(t)

	// lease_time=5 is below minimum (10)
	leaseTime := 5
	patchReq := PatchNFSSettingsRequest{
		LeaseTime: &leaseTime,
	}

	req := makeAdapterSettingsRequest(t, http.MethodPatch, "nfs", "", patchReq)
	w := httptest.NewRecorder()

	handler.PatchSettings(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("PatchSettings() status = %d, want %d, body = %s", w.Code, http.StatusUnprocessableEntity, w.Body.String())
	}

	var resp ValidationErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to unmarshal validation error: %v", err)
	}

	if _, ok := resp.Errors["lease_time"]; !ok {
		t.Error("Expected per-field error for lease_time")
	}
}

func TestPatchNFSSettings_Force(t *testing.T) {
	_, handler, _ := setupAdapterSettingsTest(t)

	// lease_time=5 is below minimum, but force bypasses validation
	leaseTime := 5
	patchReq := PatchNFSSettingsRequest{
		LeaseTime: &leaseTime,
	}

	req := makeAdapterSettingsRequest(t, http.MethodPatch, "nfs", "", patchReq, "force=true")
	w := httptest.NewRecorder()

	handler.PatchSettings(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("PatchSettings(force) status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp NFSSettingsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.LeaseTime != 5 {
		t.Errorf("LeaseTime = %d, want 5 (forced)", resp.LeaseTime)
	}
}

func TestPatchNFSSettings_DryRun(t *testing.T) {
	cpStore, handler, _ := setupAdapterSettingsTest(t)
	ctx := context.Background()

	leaseTime := 200
	patchReq := PatchNFSSettingsRequest{
		LeaseTime: &leaseTime,
	}

	req := makeAdapterSettingsRequest(t, http.MethodPatch, "nfs", "", patchReq, "dry_run=true")
	w := httptest.NewRecorder()

	handler.PatchSettings(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("PatchSettings(dry_run) status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Response should show the updated value
	var resp NFSSettingsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.LeaseTime != 200 {
		t.Errorf("Response LeaseTime = %d, want 200", resp.LeaseTime)
	}

	// But DB should be unchanged
	adapter, _ := cpStore.GetAdapter(ctx, "nfs")
	dbSettings, _ := cpStore.GetNFSAdapterSettings(ctx, adapter.ID)
	defaults := models.NewDefaultNFSSettings("")
	if dbSettings.LeaseTime != defaults.LeaseTime {
		t.Errorf("DB LeaseTime = %d, want %d (unchanged by dry_run)", dbSettings.LeaseTime, defaults.LeaseTime)
	}
}

func TestPutNFSSettings_FullReplace(t *testing.T) {
	_, handler, _ := setupAdapterSettingsTest(t)

	putReq := PutNFSSettingsRequest{
		MinVersion:              "3",
		MaxVersion:              "4.0",
		LeaseTime:               300,
		GracePeriod:             120,
		DelegationRecallTimeout: 60,
		CallbackTimeout:         10,
		LeaseBreakTimeout:       45,
		MaxConnections:          500,
		MaxClients:              5000,
		MaxCompoundOps:          100,
		MaxReadSize:             2097152,
		MaxWriteSize:            2097152,
		PreferredTransferSize:   2097152,
		DelegationsEnabled:      false,
		BlockedOperations:       []string{"WRITE"},
	}

	req := makeAdapterSettingsRequest(t, http.MethodPut, "nfs", "", putReq)
	w := httptest.NewRecorder()

	handler.PutSettings(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("PutSettings() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp NFSSettingsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.LeaseTime != 300 {
		t.Errorf("LeaseTime = %d, want 300", resp.LeaseTime)
	}
	if resp.GracePeriod != 120 {
		t.Errorf("GracePeriod = %d, want 120", resp.GracePeriod)
	}
	if resp.MaxClients != 5000 {
		t.Errorf("MaxClients = %d, want 5000", resp.MaxClients)
	}
	if resp.DelegationsEnabled {
		t.Error("DelegationsEnabled should be false")
	}
	if len(resp.BlockedOperations) != 1 || resp.BlockedOperations[0] != "WRITE" {
		t.Errorf("BlockedOperations = %v, want [WRITE]", resp.BlockedOperations)
	}
}

func TestResetNFSSettings_All(t *testing.T) {
	cpStore, handler, _ := setupAdapterSettingsTest(t)
	ctx := context.Background()

	// First modify settings
	adapter, _ := cpStore.GetAdapter(ctx, "nfs")
	settings, _ := cpStore.GetNFSAdapterSettings(ctx, adapter.ID)
	settings.LeaseTime = 999
	cpStore.UpdateNFSAdapterSettings(ctx, settings)

	req := makeAdapterSettingsRequest(t, http.MethodPost, "nfs", "reset", nil)
	w := httptest.NewRecorder()

	handler.ResetSettings(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("ResetSettings() status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp NFSSettingsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	defaults := models.NewDefaultNFSSettings("")
	if resp.LeaseTime != defaults.LeaseTime {
		t.Errorf("LeaseTime = %d, want %d (default)", resp.LeaseTime, defaults.LeaseTime)
	}
}

func TestResetNFSSettings_Specific(t *testing.T) {
	cpStore, handler, _ := setupAdapterSettingsTest(t)
	ctx := context.Background()

	// Modify lease_time and grace_period
	adapter, _ := cpStore.GetAdapter(ctx, "nfs")
	settings, _ := cpStore.GetNFSAdapterSettings(ctx, adapter.ID)
	settings.LeaseTime = 999
	settings.GracePeriod = 888
	cpStore.UpdateNFSAdapterSettings(ctx, settings)

	// Reset only lease_time
	req := makeAdapterSettingsRequest(t, http.MethodPost, "nfs", "reset", nil, "setting=lease_time")
	w := httptest.NewRecorder()

	handler.ResetSettings(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("ResetSettings(specific) status = %d, want %d, body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp NFSSettingsResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	defaults := models.NewDefaultNFSSettings("")
	if resp.LeaseTime != defaults.LeaseTime {
		t.Errorf("LeaseTime = %d, want %d (reset to default)", resp.LeaseTime, defaults.LeaseTime)
	}
	// grace_period should still be modified
	if resp.GracePeriod == defaults.GracePeriod {
		t.Error("GracePeriod should still be modified (not reset)")
	}
}
