package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/auth/netlogon"
)

func TestRIDFromSID(t *testing.T) {
	cases := []struct {
		sid     string
		wantRID uint32
		wantOK  bool
	}{
		{"S-1-5-21-1937300967-2839264751-453550700-1105", 1105, true},
		{"S-1-5-21-1937300967-2839264751-453550700-513", 513, true},
		{"S-1-5-18", 18, true}, // well-known LocalSystem (last component is the RID/sub-authority)
		{"", 0, false},
		{"notasid", 0, false},
		{"S-1-5-21-1-2-3-", 0, false},                     // trailing dash, empty RID
		{"S-1-5-21-1-2-3-abc", 0, false},                  // non-numeric RID
		{"S-1-5-21-1-2-3-99999999999999999999", 0, false}, // overflows uint32
	}
	for _, c := range cases {
		gotRID, gotOK := ridFromSID(c.sid)
		if gotOK != c.wantOK || (gotOK && gotRID != c.wantRID) {
			t.Errorf("ridFromSID(%q) = (%d,%v), want (%d,%v)", c.sid, gotRID, gotOK, c.wantRID, c.wantOK)
		}
	}
}

func TestSynthesizeRIDIdentity(t *testing.T) {
	res := &netlogon.LogonResult{
		Username:   "alice",
		DomainName: "DITTOFS",
		UserSID:    "S-1-5-21-1-2-3-1105",
		GroupSIDs: []string{
			"S-1-5-21-1-2-3-513",  // Domain Users → 513
			"S-1-5-21-1-2-3-1104", // a group → 1104
			"malformed-group-sid", // skipped
		},
	}
	got := synthesizeRIDIdentity(res)
	if got == nil {
		t.Fatal("expected a synthesized identity, got nil")
	}
	if !got.Found {
		t.Error("expected Found=true")
	}
	if got.UID != 1105 || got.GID != 1105 {
		t.Errorf("UID/GID = %d/%d, want 1105/1105", got.UID, got.GID)
	}
	if got.Username != "alice" || got.Domain != "DITTOFS" || got.SID != "S-1-5-21-1-2-3-1105" {
		t.Errorf("identity fields not propagated: %+v", got)
	}
	// Only the two well-formed group SIDs contribute GIDs; the malformed one is skipped.
	wantGIDs := map[uint32]bool{513: true, 1104: true}
	if len(got.GIDs) != len(wantGIDs) {
		t.Fatalf("GIDs = %v, want exactly %v", got.GIDs, wantGIDs)
	}
	for _, g := range got.GIDs {
		if !wantGIDs[g] {
			t.Errorf("unexpected GID %d in %v", g, got.GIDs)
		}
	}
}

func TestSynthesizeRIDIdentity_BadUserSID(t *testing.T) {
	if got := synthesizeRIDIdentity(&netlogon.LogonResult{UserSID: "not-a-sid"}); got != nil {
		t.Errorf("expected nil for unparseable user SID, got %+v", got)
	}
}
