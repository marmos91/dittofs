package auth

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// authContextRuntime is the minimal ShareIdentityRuntime for exercising
// BuildAuthContext: a share that permits the caller and an identity mapping
// that passes the identity through untouched.
type authContextRuntime struct {
	share *runtime.Share
	store models.IdentityStore
}

func (r *authContextRuntime) GetShare(string) (*runtime.Share, error) { return r.share, nil }
func (r *authContextRuntime) GetIdentityStore() models.IdentityStore  { return r.store }
func (r *authContextRuntime) ApplyIdentityMapping(_ string, ident *metadata.Identity) (*metadata.Identity, error) {
	return ident, nil
}

// TestBuildAuthContext_CarriesGSSSID verifies that a GSS-resolved identity's
// Windows half reaches AuthContext.Identity on the v3 path. Credentials carry
// only the numeric triple, so without reading SID/GroupSIDs back out of the Go
// context a Kerberos principal's SID-keyed ACEs never match here even though
// the same user's SMB requests do match them.
func TestBuildAuthContext_CarriesGSSSID(t *testing.T) {
	const (
		userSID  = "S-1-5-21-3623811015-3361044348-30300820-1013"
		groupSID = "S-1-5-21-3623811015-3361044348-30300820-513"
	)

	uid := uint32(1001)
	gid := uint32(1002)
	gssSID := userSID
	store := newPermMockStore()
	store.usersByUID[uid] = &models.User{Username: "alice", UID: &uid, Enabled: true}
	store.perm = models.PermissionReadWrite

	rt := &authContextRuntime{
		share: &runtime.Share{Name: "/export", Enabled: true, DefaultPermission: "read-write"},
		store: store,
	}

	ctx := gss.ContextWithIdentity(context.Background(), &metadata.Identity{
		UID:       &uid,
		GID:       &gid,
		GIDs:      []uint32{1002},
		SID:       &gssSID,
		GroupSIDs: []string{groupSID},
	})

	authCtx, err := BuildAuthContext(ctx, rt, "/export", Credentials{
		UID:  &uid,
		GID:  &gid,
		GIDs: []uint32{1002},
	})
	if err != nil {
		t.Fatalf("BuildAuthContext error: %v", err)
	}
	if authCtx.Identity == nil {
		t.Fatal("Identity is nil")
	}
	if authCtx.Identity.SID == nil || *authCtx.Identity.SID != userSID {
		t.Errorf("Identity.SID = %v, want %q", authCtx.Identity.SID, userSID)
	}
	if len(authCtx.Identity.GroupSIDs) != 1 || authCtx.Identity.GroupSIDs[0] != groupSID {
		t.Errorf("Identity.GroupSIDs = %v, want [%s]", authCtx.Identity.GroupSIDs, groupSID)
	}
}
