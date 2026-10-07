package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// TestCreate_NewFileGroupFollowsUsersPrimaryGID creates a file over SMB as a
// session user and checks the group that owns it. A user's own GID is their
// primary group, as the NFS identity resolver treats it: a user with a GID and
// no group memberships must not get the 1000 default, and a user with both
// must get their own GID, not the first group's. A user without a GID keeps
// the first group with one.
func TestCreate_NewFileGroupFollowsUsersPrimaryGID(t *testing.T) {
	own, group := uint32(2002), uint32(3003)
	cases := []struct {
		name    string
		gid     *uint32
		groups  []models.Group
		wantGID uint32
	}{
		{name: "own GID, no groups", gid: &own, wantGID: own},
		{name: "own GID and a group", gid: &own, groups: []models.Group{{Name: "staff", GID: &group}}, wantGID: own},
		{name: "a group, no own GID", groups: []models.Group{{Name: "staff", GID: &group}}, wantGID: group},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rt, bsID := newTestShareRuntime(t)
			ms, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
			if err != nil {
				t.Fatalf("open badger metadata store: %v", err)
			}
			t.Cleanup(func() { _ = ms.Close() })
			if err := rt.RegisterMetadataStore("gid-meta", ms); err != nil {
				t.Fatalf("RegisterMetadataStore: %v", err)
			}
			const shareName = "/gid"
			if err := rt.AddShare(ctx, &runtime.ShareConfig{
				Name:              shareName,
				MetadataStore:     "gid-meta",
				BlockStoreID:      bsID,
				Enabled:           true,
				DefaultPermission: string(models.PermissionReadWrite),
				RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
			}); err != nil {
				t.Fatalf("AddShare: %v", err)
			}
			rootHandle, err := rt.GetRootHandle(shareName)
			if err != nil {
				t.Fatalf("GetRootHandle: %v", err)
			}

			h := NewHandler()
			h.Registry = rt
			uid := uint32(2002)
			sess := h.CreateSession("127.0.0.1:12345", false, "bob", "")
			sess.User = &models.User{Username: "bob", UID: &uid, GID: tc.gid, Groups: tc.groups}
			h.StoreTree(&TreeConnection{
				TreeID:     1,
				SessionID:  sess.SessionID,
				ShareName:  shareName,
				Permission: models.PermissionReadWrite,
			})
			smbCtx := &SMBHandlerContext{
				Context:   ctx,
				TreeID:    1,
				SessionID: sess.SessionID,
				ShareName: shareName,
				User:      sess.User,
			}

			resp, err := h.Create(smbCtx, &CreateRequest{
				FileName:          "smb.txt",
				DesiredAccess:     0x001F01FF, // SEC_RIGHTS_FILE_ALL
				FileAttributes:    types.FileAttributeNormal,
				ShareAccess:       0x07,
				CreateDisposition: types.FileCreate,
				CreateOptions:     types.FileNonDirectoryFile,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if resp.Status != types.StatusSuccess {
				t.Fatalf("Create: status 0x%08x, want STATUS_SUCCESS", uint32(resp.Status))
			}

			rootUID, rootGID := uint32(0), uint32(0)
			rootAuth := &metadata.AuthContext{Context: ctx, Identity: &metadata.Identity{UID: &rootUID, GID: &rootGID}}
			f, err := rt.GetMetadataService().Lookup(rootAuth, rootHandle, "smb.txt")
			if err != nil {
				t.Fatalf("Lookup(smb.txt): %v", err)
			}
			if f.UID != uid || f.GID != tc.wantGID {
				t.Errorf("smb.txt is owned by %d:%d, want %d:%d", f.UID, f.GID, uid, tc.wantGID)
			}
		})
	}
}
