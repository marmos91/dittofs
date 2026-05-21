package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Handler-level coverage for the #529 DACL enforcement gate on CREATE.
//
// The unit tests for MetadataService.CheckFileAccess (in pkg/metadata) exercise
// the helper in isolation. These tests close the loop at the SMB handler layer:
// they construct a fully-populated createDraft, invoke completeCreateAfterBreak
// directly, and assert the on-wire CreateResponse.Status matches the expected
// per-bit-DACL outcome.
//
// completeCreateAfterBreak is the call-site where CheckFileAccess runs, so
// driving it directly with a pre-set draft (skipping the lease-break / async
// park plumbing that lives upstream in Create()) is the minimal handler-level
// exercise that still covers the integration: draft → DACL gate → response.

// setupDaclTest stands up a Handler with an in-memory metadata store, a single
// share, a parent directory, and returns a SMBHandlerContext / parent handle
// pair the test can use to seed the file under test.
func setupDaclTest(t *testing.T) (*Handler, *runtime.Runtime, *SMBHandlerContext, metadata.FileHandle, *metadata.AuthContext) {
	t.Helper()

	rt := runtime.New(nil)

	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	shareName := "/dacl-test"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "test-meta",
		RootAttr: &metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
		},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}

	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	rootUID := uint32(0)
	rootGID := uint32(0)
	rootAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &rootUID,
			GID: &rootGID,
		},
	}

	h := NewHandler()
	h.Registry = rt

	smbCtx := &SMBHandlerContext{
		SessionID: 1,
		TreeID:    1,
		ShareName: shareName,
	}

	return h, rt, smbCtx, rootHandle, rootAuth
}

// makeDraft assembles a createDraft for an existing file under the given
// parent. The draft mirrors what Create() would have produced after path
// resolution + the pre-break share-mode check, ready for
// completeCreateAfterBreak.
func makeDraft(
	t *testing.T,
	tree *TreeConnection,
	authCtx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	existingFile *metadata.File,
	baseName string,
	desiredAccess uint32,
) *createDraft {
	t.Helper()
	encHandle, err := metadata.EncodeFileHandle(existingFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	return &createDraft{
		req: &CreateRequest{
			FileName:          baseName,
			DesiredAccess:     desiredAccess,
			ShareAccess:       0x07, // R|W|D
			CreateDisposition: types.FileOpen,
			CreateOptions:     0,
		},
		tree:           tree,
		authCtx:        authCtx,
		filename:       baseName,
		baseName:       baseName,
		parentHandle:   parentHandle,
		existingFile:   existingFile,
		existingHandle: encHandle,
		fileExists:     true,
		createAction:   types.FileOpened,
	}
}

// TestCreate_DaclEnforcement_DeniesWhenBitNotGranted covers Copilot review
// case (1): a CREATE on a file whose DACL denies the requested bits must
// fail with STATUS_ACCESS_DENIED.
func TestCreate_DaclEnforcement_DeniesWhenBitNotGranted(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	metaSvc := rt.GetMetadataService()

	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// DACL: ALLOW READ_DATA to requester only — no WRITE.
	readOnlyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_READ_ATTRIBUTES | acl.ACE4_READ_ACL | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	existingFile, err := metaSvc.CreateFile(rootAuth, rootHandle, "deny.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
			UID:  9999, // not the requester
			GID:  9999,
			ACL:  readOnlyACL,
		})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	requesterGID := uint32(2001)
	sidStr := requesterSID
	requesterAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &requesterUID,
			GID: &requesterGID,
			SID: &sidStr,
		},
	}

	// Request WRITE_DATA — not in the DACL. Expect STATUS_ACCESS_DENIED.
	const rightWriteData uint32 = 0x00000002
	draft := makeDraft(t, tree, requesterAuth, rootHandle, existingFile, "deny.txt", rightWriteData)
	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusAccessDenied {
		t.Fatalf("WRITE on read-only DACL: status = 0x%08x, expected STATUS_ACCESS_DENIED (0x%08x)",
			uint32(resp.Status), uint32(types.StatusAccessDenied))
	}
}

// TestCreate_DaclEnforcement_AllowsWhenBitGranted covers Copilot review
// case (2): a CREATE on a file whose DACL allows the requested bits must
// succeed (STATUS_SUCCESS).
func TestCreate_DaclEnforcement_AllowsWhenBitGranted(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	metaSvc := rt.GetMetadataService()

	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	allowACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_READ_ATTRIBUTES | acl.ACE4_READ_ACL | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	existingFile, err := metaSvc.CreateFile(rootAuth, rootHandle, "allow.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
			UID:  9999,
			GID:  9999,
			ACL:  allowACL,
		})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	requesterGID := uint32(2001)
	sidStr := requesterSID
	requesterAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &requesterUID,
			GID: &requesterGID,
			SID: &sidStr,
		},
	}

	// Request READ_DATA — allowed by the DACL. Expect STATUS_SUCCESS.
	const rightReadData uint32 = 0x00000001
	draft := makeDraft(t, tree, requesterAuth, rootHandle, existingFile, "allow.txt", rightReadData)
	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("READ on allow-read DACL: status = 0x%08x, expected STATUS_SUCCESS",
			uint32(resp.Status))
	}
}

// TestCreate_DaclEnforcement_MaximumAllowedNeverDenies covers Copilot review
// case (3): MAXIMUM_ALLOWED on a restrictive DACL must NOT fail the open. The
// CREATE succeeds and the handle's effective access reflects what the DACL
// actually grants (a subset of what the requester would otherwise want).
func TestCreate_DaclEnforcement_MaximumAllowedNeverDenies(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	metaSvc := rt.GetMetadataService()

	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// DACL allows only READ_DATA — WRITE/DELETE/etc are not granted.
	readOnlyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA,
			},
		},
	}
	existingFile, err := metaSvc.CreateFile(rootAuth, rootHandle, "max.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
			UID:  9999,
			GID:  9999,
			ACL:  readOnlyACL,
		})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	requesterGID := uint32(2001)
	sidStr := requesterSID
	requesterAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &requesterUID,
			GID: &requesterGID,
			SID: &sidStr,
		},
	}

	// MAXIMUM_ALLOWED — must never deny even though the DACL grants almost
	// nothing.
	const rightMaxAllowed uint32 = 0x02000000
	draft := makeDraft(t, tree, requesterAuth, rootHandle, existingFile, "max.txt", rightMaxAllowed)
	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("MAXIMUM_ALLOWED on restrictive DACL: status = 0x%08x, expected STATUS_SUCCESS",
			uint32(resp.Status))
	}
}

// TestCreate_DaclEnforcement_NilACLPermissive guards against the WPTS BVT
// regression that triggered the post-PR rework: a file with no DACL must NOT
// have any bits denied at the open gate, even for non-POSIX-rwx bits like
// DELETE / WRITE_DAC / WRITE_OWNER. POSIX-mode enforcement happens later in
// the per-op layer, not here.
func TestCreate_DaclEnforcement_NilACLPermissive(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	metaSvc := rt.GetMetadataService()

	existingFile, err := metaSvc.CreateFile(rootAuth, rootHandle, "no_dacl.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o600, // owner rw — but no DACL stored
			UID:  9999,
			GID:  9999,
			// ACL: nil
		})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	requesterUID := uint32(2001)
	requesterGID := uint32(2001)
	requesterAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: &requesterUID,
			GID: &requesterGID,
		},
	}

	// Non-owner requesting DELETE | WRITE_DAC | WRITE_OWNER — bits the POSIX
	// rwx mapping cannot encode. Pre-#529 server permitted these opens; the
	// reworked gate must continue to permit them when there's no DACL.
	const (
		rightDelete     uint32 = 0x00010000
		rightWriteDac   uint32 = 0x00040000
		rightWriteOwner uint32 = 0x00080000
	)
	desired := rightDelete | rightWriteDac | rightWriteOwner
	draft := makeDraft(t, tree, requesterAuth, rootHandle, existingFile, "no_dacl.txt", desired)
	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("nil-ACL high-namespace request: status = 0x%08x, expected STATUS_SUCCESS",
			uint32(resp.Status))
	}
}
