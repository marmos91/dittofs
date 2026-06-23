package netlogon

import "testing"

func TestSAMInfo4ToResult(t *testing.T) {
	var key [16]byte
	key[0] = 0xAB
	res, err := samInfo4ToResult(
		"S-1-5-21-1-2-3", 1103, []uint32{513, 1104}, key, "alice", "DITTOFS",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.UserSID != "S-1-5-21-1-2-3-1103" {
		t.Fatalf("user SID: %q", res.UserSID)
	}
	if len(res.GroupSIDs) != 2 || res.GroupSIDs[0] != "S-1-5-21-1-2-3-513" {
		t.Fatalf("group SIDs: %v", res.GroupSIDs)
	}
	if res.SessionBaseKey != key {
		t.Fatal("session key not propagated")
	}
	if res.Username != "alice" {
		t.Fatalf("username: %q", res.Username)
	}
	if res.DomainName != "DITTOFS" {
		t.Fatalf("domain name: %q", res.DomainName)
	}
}
