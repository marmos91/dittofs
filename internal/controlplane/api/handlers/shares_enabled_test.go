package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestShareResponse_IncludesEnabled verifies D-28: the ShareResponse JSON
// always includes the `enabled` field (no omitempty) so clients can render
// the disabled state explicitly.
func TestShareResponse_IncludesEnabled(t *testing.T) {
	share := &models.Share{
		ID:      "s1",
		Name:    "/alice",
		Enabled: false, // deliberately false to exercise the no-omitempty path
	}
	resp := shareToResponse(share)
	if resp.Enabled {
		t.Fatalf("ShareResponse.Enabled should mirror models.Share.Enabled (false)")
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"enabled":false`) {
		t.Errorf("JSON missing enabled=false; got %s", s)
	}
}

// TestShareResponse_EnabledTrue verifies the true case emits `"enabled":true`.
func TestShareResponse_EnabledTrue(t *testing.T) {
	share := &models.Share{ID: "s1", Name: "/alice", Enabled: true}
	b, err := json.Marshal(shareToResponse(share))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"enabled":true`) {
		t.Errorf("JSON missing enabled=true; got %s", b)
	}
}
