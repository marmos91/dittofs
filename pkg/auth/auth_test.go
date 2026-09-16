package auth

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Result carries metadata.Identity directly rather than a parallel type, so
// every field survives a round-trip through it. This guards the reason that
// choice was made: a parallel result type with a field-by-field converter is
// how the Windows half (SID/GroupSIDs/Domain) previously got dropped from a
// copy, silently denying SID-keyed ACL ACEs.
func TestResult_PreservesEveryIdentityField(t *testing.T) {
	uid, gid := uint32(1000), uint32(1001)
	sid := "S-1-5-21-1-2-3-1001"
	want := &metadata.Identity{
		UID:       &uid,
		GID:       &gid,
		GIDs:      []uint32{1001, 2002},
		SID:       &sid,
		GroupSIDs: []string{"S-1-5-21-1-2-3-2002"},
		Username:  "alice",
		Domain:    "EXAMPLE.COM",
	}

	got := (&Result{Identity: want}).Identity

	if got.UID != want.UID || got.GID != want.GID {
		t.Fatalf("uid/gid lost: got %v/%v", got.UID, got.GID)
	}
	if len(got.GIDs) != 2 || got.GIDs[1] != 2002 {
		t.Fatalf("gids lost: got %v", got.GIDs)
	}
	if got.SID == nil || *got.SID != sid {
		t.Fatalf("SID lost: got %v", got.SID)
	}
	if len(got.GroupSIDs) != 1 || got.GroupSIDs[0] != "S-1-5-21-1-2-3-2002" {
		t.Fatalf("GroupSIDs lost: got %v", got.GroupSIDs)
	}
	if got.Username != "alice" || got.Domain != "EXAMPLE.COM" {
		t.Fatalf("username/domain lost: got %q/%q", got.Username, got.Domain)
	}
}
