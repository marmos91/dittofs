package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"

	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

const squashAnonID = uint32(65534)

// setupSquashShare stands up a share with the given squash mode, seeds a 0600
// file owned by fileOwner, and returns a handler with an SMB session for a user
// with callerUID, connected to the share.
func setupSquashShare(t *testing.T, squash models.SquashMode, fileOwner, callerUID uint32) (*Handler, *runtime.Runtime, *SMBHandlerContext, metadata.FileHandle) {
	t.Helper()

	rt, bsID := newTestShareRuntime(t)
	if err := rt.RegisterMetadataStore("squash-meta", badgertest.NewInMemory(t)); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	const shareName = "/squash"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:              shareName,
		MetadataStore:     "squash-meta",
		BlockStoreID:      bsID,
		Enabled:           true,
		DefaultPermission: string(models.PermissionReadWrite),
		Squash:            squash,
		AnonymousUID:      squashAnonID,
		AnonymousGID:      squashAnonID,
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	owner := fileOwner
	ownerCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &owner, GID: &owner},
	}
	if _, _, err := rt.GetMetadataService().CreateFile(ownerCtx, rootHandle, "secret.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular, Mode: 0o600, UID: fileOwner, GID: fileOwner,
	}); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	h := NewHandler()
	h.Registry = rt

	uid, gid := callerUID, callerUID
	sess := h.CreateSession("127.0.0.1:1", false, "caller", "")
	sess.User = &models.User{Username: "caller", Enabled: true, UID: &uid, GID: &gid}

	const treeID uint32 = 1
	h.StoreTree(&TreeConnection{
		TreeID:     treeID,
		SessionID:  sess.SessionID,
		ShareName:  shareName,
		Permission: models.PermissionReadWrite,
	})
	ctx := &SMBHandlerContext{
		Context:   context.Background(),
		TreeID:    treeID,
		SessionID: sess.SessionID,
		ShareName: shareName,
	}
	return h, rt, ctx, rootHandle
}

// readFile opens name for read and READs it, returning the READ status
// (STATUS_SUCCESS when permitted). With
// no ACL on the file the open grants the request as-is and the POSIX mode is
// enforced at READ, so READ is where a refusal shows.
func readFile(t *testing.T, h *Handler, ctx *SMBHandlerContext, name string) types.Status {
	t.Helper()
	resp, err := h.Create(ctx, &CreateRequest{
		FileName:          name,
		DesiredAccess:     uint32(types.FileReadData),
		FileAttributes:    types.FileAttributeNormal,
		ShareAccess:       0x07,
		CreateDisposition: types.FileOpen,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	if resp.Status != types.StatusSuccess {
		return resp.Status
	}
	rresp, err := h.Read(ctx, &ReadRequest{FileID: resp.FileID, Length: 16})
	if err != nil {
		t.Fatalf("Read(%q): %v", name, err)
	}
	// The file is empty, so a permitted READ reports end of file.
	if rresp.Status == types.StatusEndOfFile {
		return types.StatusSuccess
	}
	return rresp.Status
}

// The built-in admin is UID 0. On a share with the default squash mode
// (root_to_guest) it is squashed to the anonymous identity, as NFS root_squash
// does, so it must not read another user's 0600 file over SMB.
func TestSquash_DefaultShare_RootCannotReadOthersPrivateFile(t *testing.T) {
	h, _, ctx, _ := setupSquashShare(t, "", 2000, 0)

	if st := readFile(t, h, ctx, "secret.txt"); st != types.StatusAccessDenied {
		t.Fatalf("uid 0 opening uid 2000's 0600 file on a default share: status 0x%08x, want STATUS_ACCESS_DENIED", uint32(st))
	}
}

// root_to_admin is the opt-in that keeps root privileged. The session caches the
// identity it builds, so the squash must run past that cache: after the share's
// mode changes, the same session's next op is squashed.
func TestSquash_RootToAdmin_RootKeepsBypassUntilModeChanges(t *testing.T) {
	h, rt, ctx, _ := setupSquashShare(t, models.SquashRootToAdmin, 2000, 0)

	if st := readFile(t, h, ctx, "secret.txt"); st != types.StatusSuccess {
		t.Fatalf("uid 0 on a root_to_admin share: status 0x%08x, want STATUS_SUCCESS", uint32(st))
	}
	if err := rt.SetShareSquash(ctx.ShareName, models.SquashRootToGuest, nil, nil); err != nil {
		t.Fatalf("SetShareSquash: %v", err)
	}
	if st := readFile(t, h, ctx, "secret.txt"); st != types.StatusAccessDenied {
		t.Fatalf("uid 0 after the share switched to root_to_guest: status 0x%08x, want STATUS_ACCESS_DENIED", uint32(st))
	}
}

// On an all_to_guest share every SMB caller acts as the anonymous identity: a
// file it creates is owned by the anonymous UID/GID, and its own 0600 file —
// owned by its unsquashed UID — is another user's file to the squashed caller.
func TestSquash_AllToGuest_CallerBecomesAnonymous(t *testing.T) {
	const callerUID = 1000
	h, rt, ctx, rootHandle := setupSquashShare(t, models.SquashAllToGuest, callerUID, callerUID)

	if st := readFile(t, h, ctx, "secret.txt"); st != types.StatusAccessDenied {
		t.Fatalf("all_to_guest caller opening uid %d's 0600 file: status 0x%08x, want STATUS_ACCESS_DENIED", callerUID, uint32(st))
	}

	resp, err := h.Create(ctx, &CreateRequest{
		FileName:          "mine.txt",
		DesiredAccess:     uint32(types.FileReadData | types.FileWriteData),
		FileAttributes:    types.FileAttributeNormal,
		ShareAccess:       0x07,
		CreateDisposition: types.FileCreate,
	})
	if err != nil || resp.Status != types.StatusSuccess {
		t.Fatalf("Create(mine.txt): status 0x%08x err %v", uint32(resp.Status), err)
	}

	rootUID := uint32(0)
	file, err := rt.GetMetadataService().Lookup(&metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &rootUID, GID: &rootUID},
	}, rootHandle, "mine.txt")
	if err != nil {
		t.Fatalf("Lookup(mine.txt): %v", err)
	}
	if file.UID != squashAnonID || file.GID != squashAnonID {
		t.Fatalf("file created on an all_to_guest share is owned by %d:%d, want %d:%d",
			file.UID, file.GID, squashAnonID, squashAnonID)
	}
}
