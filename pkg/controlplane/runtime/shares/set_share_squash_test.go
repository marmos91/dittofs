package shares

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestSetShareSquash_UpdatesLiveShare verifies that SetShareSquash mutates the
// in-memory runtime share so a squash-config change is visible to readers
// (GetShare) without a restart, and that anon UID/GID are updated only when a
// non-nil pointer is supplied.
func TestSetShareSquash_UpdatesLiveShare(t *testing.T) {
	svc := New()
	svc.registry["/export"] = &Share{
		Name:         "/export",
		Squash:       models.SquashRootToAdmin,
		AnonymousUID: 65534,
		AnonymousGID: 65534,
	}

	newUID := uint32(1000)
	if err := svc.SetShareSquash("/export", models.SquashRootToGuest, &newUID, nil); err != nil {
		t.Fatalf("SetShareSquash: %v", err)
	}

	got, err := svc.GetShare("/export")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if got.Squash != models.SquashRootToGuest {
		t.Errorf("Squash = %q, want %q", got.Squash, models.SquashRootToGuest)
	}
	if got.AnonymousUID != 1000 {
		t.Errorf("AnonymousUID = %d, want 1000", got.AnonymousUID)
	}
	// nil anonGID must leave the existing value untouched.
	if got.AnonymousGID != 65534 {
		t.Errorf("AnonymousGID = %d, want 65534 (unchanged)", got.AnonymousGID)
	}
}

// TestSetShareSquash_UnknownShare verifies SetShareSquash reports a not-found
// error for a share that is not registered.
func TestSetShareSquash_UnknownShare(t *testing.T) {
	svc := New()
	if err := svc.SetShareSquash("/missing", models.SquashRootToGuest, nil, nil); err == nil {
		t.Fatal("expected error for unknown share, got nil")
	}
}
