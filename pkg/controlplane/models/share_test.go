package models

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestShare_JSON_IncludesEnabled — Phase 6 D-28 regression guard.
//
// Locks in the Phase 5 landing of the `enabled` JSON tag on models.Share.
// The Phase-6 CLI `share list` / `share show` columns and the Phase-6
// apiclient `Share.Enabled` field depend on this field round-tripping
// without `omitempty` semantics; if a future edit drops the tag or adds
// `omitempty`, this test fails.
func TestShare_JSON_IncludesEnabled(t *testing.T) {
	// Marshal: `enabled:true` is emitted.
	b, err := json.Marshal(Share{Enabled: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"enabled":true`)) {
		t.Errorf("Share JSON missing \"enabled\":true — got %s", b)
	}

	// Marshal: `enabled:false` is ALSO emitted. If someone adds omitempty
	// this test will fail — disabled shares must still surface in API
	// responses so the operator can enable them.
	b2, err := json.Marshal(Share{Enabled: false})
	if err != nil {
		t.Fatalf("marshal false: %v", err)
	}
	if !bytes.Contains(b2, []byte(`"enabled":false`)) {
		t.Errorf("Share JSON must emit \"enabled\":false (no omitempty) — got %s", b2)
	}

	// Unmarshal: `enabled:true` round-trips.
	var s Share
	if err := json.Unmarshal([]byte(`{"enabled":true}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !s.Enabled {
		t.Errorf("Share.Enabled=false after unmarshal of {\"enabled\":true}")
	}
}
