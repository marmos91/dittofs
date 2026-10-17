package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// TestCreate_SDBufferUnmappedACETypeRefused sends a CREATE whose
// SMB2_CREATE_SD_BUFFER carries a deny of an ACE type the ACL model cannot
// hold. Creating the file without that descriptor would drop the deny, so the
// CREATE must fail with STATUS_INVALID_PARAMETER and leave no entry behind. A
// descriptor the model can hold must still create the file and be applied.
func TestCreate_SDBufferUnmappedACETypeRefused(t *testing.T) {
	const (
		fileAllAccess  uint32 = 0x001F01FF
		fileWriteData  uint32 = 0x00000002
		deniedCallback uint8  = 0x0A // ACCESS_DENIED_CALLBACK_ACE_TYPE
	)
	bob := buildSID(t, "S-1-5-21-1000-1000-1000-1104")
	everyone := buildSID(t, "S-1-1-0")
	allowEveryone := buildACE(accessAllowedACEType, 0, fileAllAccess, everyone)

	for _, tc := range []struct {
		name     string
		denyType uint8
		want     types.Status
	}{
		{"callback_deny_refused", deniedCallback, types.StatusInvalidParameter},
		{"plain_deny_applied", accessDeniedACEType, types.StatusSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rt, bsID := newTestShareRuntime(t)
			if err := rt.RegisterMetadataStore("sd-meta", badgertest.NewInMemory(t)); err != nil {
				t.Fatalf("RegisterMetadataStore: %v", err)
			}
			const shareName = "/sd"
			if err := rt.AddShare(ctx, &runtime.ShareConfig{
				Name:              shareName,
				MetadataStore:     "sd-meta",
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
			uid, gid := uint32(0), uint32(0)
			sess := h.CreateSession("127.0.0.1:12345", false, "root", "")
			sess.User = &models.User{Username: "root", UID: &uid, GID: &gid}
			h.StoreTree(&TreeConnection{TreeID: 1, SessionID: sess.SessionID, ShareName: shareName, Permission: models.PermissionReadWrite})
			smbCtx := &SMBHandlerContext{Context: ctx, TreeID: 1, SessionID: sess.SessionID, ShareName: shareName, User: sess.User}

			aces := append(buildACE(tc.denyType, 0, fileWriteData, bob), allowEveryone...)
			sd := buildSelfRelativeSD(t, seDACLPresent, nil, nil, buildRawDACL(2, aces))

			resp, err := h.Create(smbCtx, &CreateRequest{
				FileName:          "sd.txt",
				DesiredAccess:     fileAllAccess,
				FileAttributes:    types.FileAttributeNormal,
				ShareAccess:       0x07,
				CreateDisposition: types.FileCreate,
				CreateOptions:     types.FileNonDirectoryFile,
				CreateContexts:    []CreateContext{{Name: SDBufferCreateContextTag, Data: sd}},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if resp.Status != tc.want {
				t.Errorf("Create status = %v, want %v", resp.Status, tc.want)
			}

			rootAuth := &metadata.AuthContext{Context: ctx, Identity: &metadata.Identity{UID: &uid, GID: &gid}}
			f, lookupErr := rt.GetMetadataService().Lookup(rootAuth, rootHandle, "sd.txt")
			if tc.want != types.StatusSuccess {
				if !metadata.IsNotFoundError(lookupErr) {
					t.Errorf("sd.txt exists after a refused CREATE: file=%v err=%v", f, lookupErr)
				}
				return
			}
			if lookupErr != nil {
				t.Fatalf("Lookup(sd.txt): %v", lookupErr)
			}
			if f.ACL == nil || len(f.ACL.ACEs) != 2 || f.ACL.ACEs[0].Type != acl.ACE4_ACCESS_DENIED_ACE_TYPE {
				t.Errorf("sd.txt ACL = %+v, want the client's [deny, allow]", f.ACL)
			}
		})
	}
}
