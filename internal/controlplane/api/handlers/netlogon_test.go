package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
)

// TestNewNetlogonHandler_NilController verifies the constructor returns nil when
// no controller is registered, so the router can skip mounting the routes.
func TestNewNetlogonHandler_NilController(t *testing.T) {
	if h := NewNetlogonHandler(nil); h != nil {
		t.Fatalf("expected nil handler for nil controller, got %v", h)
	}
}

// TestNetlogonHandler_StatusOffline verifies the status endpoint reports the
// offline provider identity and marks pass-through enabled.
func TestNetlogonHandler_StatusOffline(t *testing.T) {
	prov := netlogon.NewMutableProvider(netlogon.BuildMachineCredential("DITTOFS$", "s3cr3t", "DITTOFS", "DITTOFS.AD", nil))
	ctrl := netlogon.NewController(netlogon.ProviderOffline, netlogon.NewAuthenticator(prov), prov, nil)
	h := NewNetlogonHandler(ctrl)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.Status(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", w.Code)
	}
	var dto netlogonStatusDTO
	if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !dto.Enabled || dto.Provider != "offline" {
		t.Errorf("unexpected status: %+v", dto)
	}
	if dto.AccountName != "DITTOFS$" || dto.Realm != "DITTOFS.AD" || dto.NetBIOSDomain != "DITTOFS" {
		t.Errorf("unexpected identity: %+v", dto)
	}
	if dto.RotationEnabled {
		t.Error("offline provider must not report rotation enabled")
	}
}

// TestNetlogonHandler_RotateOfflineConflict verifies rotating the offline
// provider yields a 409 Conflict (rotation is only valid for online-join).
func TestNetlogonHandler_RotateOfflineConflict(t *testing.T) {
	prov := netlogon.NewMutableProvider(netlogon.BuildMachineCredential("DITTOFS$", "s3cr3t", "DITTOFS", "DITTOFS.AD", nil))
	ctrl := netlogon.NewController(netlogon.ProviderOffline, netlogon.NewAuthenticator(prov), prov, nil)
	h := NewNetlogonHandler(ctrl)

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	h.Rotate(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status code = %d, want 409", w.Code)
	}
}
