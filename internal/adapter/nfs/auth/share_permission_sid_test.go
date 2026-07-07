package auth

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// A direct AD/SID grant (#1528) authorizes an NFS login that has no local user
// (GetUserByUID fails) by matching one of its GIDs, even when the share default
// is "none" — the NFS analogue of the SMB PAC-SID path.
func TestResolveSharePermission_SIDGrantByGID(t *testing.T) {
	share := &runtime.Share{Name: "/export", DefaultPermission: "none", Squash: models.SquashRootToGuest}

	t.Run("group GID grant grants access under default none", func(t *testing.T) {
		store := newPermMockStore() // no local user for this UID
		store.sidPerm = models.PermissionReadWrite
		store.sidMatchIDs = map[uint32]bool{1104: true} // the granted group's GID

		res, err := ResolveSharePermission(
			context.Background(), store, share, "/export", "10.0.0.1:1", ptrUID(50001), []uint32{1104})
		if err != nil {
			t.Fatalf("expected access via SID grant, got denial: %v", err)
		}
		if res.ReadOnly {
			t.Errorf("read-write SID grant should not be read-only")
		}
	})

	t.Run("no GID match still denies under default none", func(t *testing.T) {
		store := newPermMockStore()
		store.sidPerm = models.PermissionReadWrite
		store.sidMatchIDs = map[uint32]bool{1104: true}

		_, err := ResolveSharePermission(
			context.Background(), store, share, "/export", "10.0.0.1:1", ptrUID(50001), []uint32{9999})
		if err != ErrShareAccessDenied {
			t.Fatalf("expected ErrShareAccessDenied for an unmatched GID, got %v", err)
		}
	})

	t.Run("read-level SID grant coerces read-only", func(t *testing.T) {
		store := newPermMockStore()
		store.sidPerm = models.PermissionRead
		store.sidMatchIDs = map[uint32]bool{1104: true}

		res, err := ResolveSharePermission(
			context.Background(), store, share, "/export", "10.0.0.1:1", ptrUID(50001), []uint32{1104})
		if err != nil {
			t.Fatalf("unexpected denial: %v", err)
		}
		if !res.ReadOnly {
			t.Errorf("read-level SID grant should be read-only")
		}
	})
}
