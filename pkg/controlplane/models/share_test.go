package models

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestShare_JSON_IncludesEnabled is a regression guard.
//
// Locks in the `enabled` JSON tag on models.Share. The CLI
// `share list` / `share show` columns and the apiclient `Share.Enabled`
// field depend on this field round-tripping without `omitempty`
// semantics; if a future edit drops the tag or adds `omitempty`, this
// test fails.
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

// TestShare_JSON_AclFlagInheritedCanonicalization — T1 of #514.
//
// Locks in the JSON tag on Share.AclFlagInheritedCanonicalization so any
// future edit that drops it or adds omitempty will fail. The boolean must
// round-trip in both directions; omitempty would hide an explicit `false`
// (Samba-extension opt-out) on disabled shares.
func TestShare_JSON_AclFlagInheritedCanonicalization(t *testing.T) {
	// Marshal: explicit true is emitted.
	b, err := json.Marshal(Share{AclFlagInheritedCanonicalization: true})
	if err != nil {
		t.Fatalf("marshal true: %v", err)
	}
	if !bytes.Contains(b, []byte(`"acl_flag_inherited_canonicalization":true`)) {
		t.Errorf("Share JSON missing \"acl_flag_inherited_canonicalization\":true — got %s", b)
	}

	// Marshal: explicit false is also emitted (no omitempty).
	b2, err := json.Marshal(Share{AclFlagInheritedCanonicalization: false})
	if err != nil {
		t.Fatalf("marshal false: %v", err)
	}
	if !bytes.Contains(b2, []byte(`"acl_flag_inherited_canonicalization":false`)) {
		t.Errorf("Share JSON must emit \"acl_flag_inherited_canonicalization\":false (no omitempty) — got %s", b2)
	}

	// Unmarshal: false round-trips.
	var s Share
	if err := json.Unmarshal([]byte(`{"acl_flag_inherited_canonicalization":false}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.AclFlagInheritedCanonicalization {
		t.Errorf("Share.AclFlagInheritedCanonicalization=true after unmarshal of false")
	}
}
