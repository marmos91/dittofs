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

// makeDraftWithDisposition builds a createDraft whose disposition + createAction
// match a destructive open (OVERWRITE / OVERWRITE_IF / SUPERSEDE). Used by the
// #565 tests below to exercise the disposition-implied FILE_WRITE_DATA check.
func makeDraftWithDisposition(
	t *testing.T,
	tree *TreeConnection,
	authCtx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	existingFile *metadata.File,
	baseName string,
	desiredAccess uint32,
	disposition types.CreateDisposition,
	action types.CreateAction,
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
			ShareAccess:       0x07,
			CreateDisposition: disposition,
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
		createAction:   action,
	}
}

// restrictedDACLFile creates an existing file whose DACL grants the requester
// only READ_DATA (no WRITE / DELETE). Used by both the #564 MAX-plus-explicit
// and #565 disposition-implied tests.
func restrictedDACLFile(
	t *testing.T,
	metaSvc *metadata.MetadataService,
	rootAuth *metadata.AuthContext,
	rootHandle metadata.FileHandle,
	name string,
	requesterSID string,
) *metadata.File {
	t.Helper()
	readOnlyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_READ_ATTRIBUTES | acl.ACE4_READ_ACL | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	f, err := metaSvc.CreateFile(rootAuth, rootHandle, name,
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
	return f
}

// TestCreate_MaxAllowedPlusExplicitDeniedBit_ReturnsAccessDenied covers
// smbtorture smb2.acls.MXAC-NOT-GRANTED (issue #564) at the SMB handler layer.
// CREATE with DesiredAccess = MAXIMUM_ALLOWED | FILE_WRITE_DATA against a file
// whose DACL only grants READ_DATA must fail STATUS_ACCESS_DENIED. The explicit
// WRITE_DATA bit is verified against the granted set independently of MAX.
//
// Per MS-SMB2 §3.3.5.9 paragraph 8 and Samba
// smbd_calculate_maximum_allowed_access_fsp.
func TestCreate_MaxAllowedPlusExplicitDeniedBit_ReturnsAccessDenied(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"
	existingFile := restrictedDACLFile(t, rt.GetMetadataService(), rootAuth, rootHandle, "mxac.txt", requesterSID)

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

	const (
		rightMaxAllowed uint32 = 0x02000000
		rightWriteData  uint32 = 0x00000002
	)
	draft := makeDraft(t, tree, requesterAuth, rootHandle, existingFile, "mxac.txt", rightMaxAllowed|rightWriteData)
	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusAccessDenied {
		t.Fatalf("MAX|WRITE_DATA on READ-only DACL: status = 0x%08x, expected STATUS_ACCESS_DENIED (0x%08x)",
			uint32(resp.Status), uint32(types.StatusAccessDenied))
	}
}

// TestCreate_OverwriteRestrictedFile_ReturnsAccessDenied covers smbtorture
// smb2.acls.OVERWRITE_READ_ONLY_FILE (issue #565) — OVERWRITE arm at
// source4/torture/smb2/acls.c:3150. CREATE with disposition FILE_OVERWRITE and
// DesiredAccess = FILE_READ_DATA against a DACL-restricted file must fail
// STATUS_ACCESS_DENIED because OVERWRITE implicitly requires FILE_WRITE_DATA
// to truncate the existing file, which the DACL does not grant.
//
// Per MS-FSA §2.1.5.1.2.1 and Samba open_file_ntcreate
// (`open_access_mask |= FILE_WRITE_DATA` when O_TRUNC).
func TestCreate_OverwriteRestrictedFile_ReturnsAccessDenied(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"
	existingFile := restrictedDACLFile(t, rt.GetMetadataService(), rootAuth, rootHandle, "overwrite.txt", requesterSID)

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

	const rightReadData uint32 = 0x00000001
	draft := makeDraftWithDisposition(t, tree, requesterAuth, rootHandle, existingFile, "overwrite.txt",
		rightReadData, types.FileOverwrite, types.FileOverwritten)
	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusAccessDenied {
		t.Fatalf("OVERWRITE on READ-only DACL: status = 0x%08x, expected STATUS_ACCESS_DENIED (0x%08x)",
			uint32(resp.Status), uint32(types.StatusAccessDenied))
	}
}

// TestCreate_SupersedeRestrictedFile_ReturnsAccessDenied covers smbtorture
// smb2.acls.OVERWRITE_READ_ONLY_FILE (issue #565) — SUPERSEDE arm at
// source4/torture/smb2/acls.c:3104. Mirrors the OVERWRITE test above but with
// FILE_SUPERSEDE disposition: SUPERSEDE replaces the existing file content,
// which inherently requires FILE_WRITE_DATA per MS-FSA §2.1.5.1.2.1 and must
// be denied when the DACL grants only READ_DATA.
func TestCreate_SupersedeRestrictedFile_ReturnsAccessDenied(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"
	existingFile := restrictedDACLFile(t, rt.GetMetadataService(), rootAuth, rootHandle, "supersede.txt", requesterSID)

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

	const rightReadData uint32 = 0x00000001
	draft := makeDraftWithDisposition(t, tree, requesterAuth, rootHandle, existingFile, "supersede.txt",
		rightReadData, types.FileSupersede, types.FileSuperseded)
	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusAccessDenied {
		t.Fatalf("SUPERSEDE on READ-only DACL: status = 0x%08x, expected STATUS_ACCESS_DENIED (0x%08x)",
			uint32(resp.Status), uint32(types.StatusAccessDenied))
	}
}

// TestCreate_OverwriteIfRestrictedFile_ReturnsAccessDenied covers the
// OVERWRITE_IF arm for #565 — same DACL-restricted truncate semantics as
// OVERWRITE and SUPERSEDE.
func TestCreate_OverwriteIfRestrictedFile_ReturnsAccessDenied(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"
	existingFile := restrictedDACLFile(t, rt.GetMetadataService(), rootAuth, rootHandle, "overwriteif.txt", requesterSID)

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

	const rightReadData uint32 = 0x00000001
	draft := makeDraftWithDisposition(t, tree, requesterAuth, rootHandle, existingFile, "overwriteif.txt",
		rightReadData, types.FileOverwriteIf, types.FileOverwritten)
	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusAccessDenied {
		t.Fatalf("OVERWRITE_IF on READ-only DACL: status = 0x%08x, expected STATUS_ACCESS_DENIED (0x%08x)",
			uint32(resp.Status), uint32(types.StatusAccessDenied))
	}
}

// makeDraftWithShareAccess assembles a createDraft with explicit
// DesiredAccess, ShareAccess, and CreateDisposition. Used by the share-mode
// conflict tests that need to exercise the per-bit deny semantics against a
// pre-seeded OpenFile.
func makeDraftWithShareAccess(
	t *testing.T,
	tree *TreeConnection,
	authCtx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	existingFile *metadata.File,
	baseName string,
	desiredAccess uint32,
	shareAccess uint32,
	disposition types.CreateDisposition,
	action types.CreateAction,
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
			ShareAccess:       shareAccess,
			CreateDisposition: disposition,
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
		createAction:   action,
	}
}

// runDestructiveShareViolationCase exercises the smbtorture
// smb2.acls.OVERWRITE_READ_ONLY_FILE sharing_tcases shape (#575):
//
//  1. First handle: CREATE(desired=READ_DATA, share=SHARE_READ, disp=OPEN_IF).
//     Recorded as an OpenFile in the handler's open table.
//  2. Second handle: CREATE(desired=READ_DATA, share=SHARE_READ, disp=DESTRUCTIVE).
//     Must return STATUS_SHARING_VIOLATION because the destructive disposition
//     implicitly requires FILE_WRITE_DATA, which the existing handle's
//     SHARE_READ-only share mode does not allow.
//
// Mirrors Samba `open_file_ntcreate`'s `open_access_mask |= FILE_WRITE_DATA`
// for O_TRUNC dispositions, which is then used for the share-mode conflict
// check in `open_mode_check`.
func runDestructiveShareViolationCase(t *testing.T, fname string, disposition types.CreateDisposition, action types.CreateAction) {
	t.Helper()
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	// File owned by the test user with a permissive DACL (the sharing_tcases
	// arm runs AFTER the test restores the original SD, so the DACL is not
	// the restriction under test here — the share mode is).
	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"
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
	fullAccessACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA | acl.ACE4_READ_ATTRIBUTES | acl.ACE4_WRITE_ATTRIBUTES | acl.ACE4_DELETE | acl.ACE4_READ_ACL | acl.ACE4_WRITE_ACL | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	existingFile, err := rt.GetMetadataService().CreateFile(rootAuth, rootHandle, fname,
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
			UID:  requesterUID,
			GID:  requesterGID,
			ACL:  fullAccessACL,
		})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	// Seed the handler open table with the first handle (SHARE_READ only).
	const (
		fileReadData  uint32 = 0x00000001
		fileShareRead uint32 = 0x01
	)
	encHandle, err := metadata.EncodeFileHandle(existingFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	firstFileID := h.GenerateFileID()
	h.StoreOpenFile(&OpenFile{
		FileID:         firstFileID,
		TreeID:         smbCtx.TreeID,
		SessionID:      smbCtx.SessionID,
		Path:           fname,
		ShareName:      smbCtx.ShareName,
		DesiredAccess:  fileReadData,
		ShareAccess:    fileShareRead,
		MetadataHandle: encHandle,
	})

	// Second handle: destructive disposition, READ_DATA, SHARE_READ.
	draft := makeDraftWithShareAccess(t, tree, requesterAuth, rootHandle, existingFile, fname,
		fileReadData, fileShareRead, disposition, action)
	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSharingViolation {
		t.Fatalf("disp=%d with SHARE_READ vs existing SHARE_READ-only holder: status = 0x%08x, expected STATUS_SHARING_VIOLATION (0x%08x)",
			disposition, uint32(resp.Status), uint32(types.StatusSharingViolation))
	}
}

// TestCreate_SupersedeWithReadShareConflictsExistingHolder covers
// smb2.acls.OVERWRITE_READ_ONLY_FILE sharing_tcases SUPERSEDE arm (#575) at
// samba 4.22.6 source4/torture/smb2/acls.c:3175. The destructive disposition
// implicitly requires FILE_WRITE_DATA, which conflicts with an existing
// SHARE_READ-only handle.
func TestCreate_SupersedeWithReadShareConflictsExistingHolder(t *testing.T) {
	runDestructiveShareViolationCase(t, "supersede_share.txt", types.FileSupersede, types.FileSuperseded)
}

// TestCreate_OverwriteWithReadShareConflictsExistingHolder covers the
// OVERWRITE arm of the same sharing_tcases loop (#575).
func TestCreate_OverwriteWithReadShareConflictsExistingHolder(t *testing.T) {
	runDestructiveShareViolationCase(t, "overwrite_share.txt", types.FileOverwrite, types.FileOverwritten)
}

// TestCreate_OverwriteIfWithReadShareConflictsExistingHolder covers the
// OVERWRITE_IF arm of the same sharing_tcases loop (#575).
func TestCreate_OverwriteIfWithReadShareConflictsExistingHolder(t *testing.T) {
	runDestructiveShareViolationCase(t, "overwriteif_share.txt", types.FileOverwriteIf, types.FileOverwritten)
}

// TestCreate_OpenWithReadShareDoesNotConflictExistingHolder is the negative
// control for #575: a non-destructive disposition (FILE_OPEN) with the same
// READ_DATA + SHARE_READ shape against a SHARE_READ-only holder must succeed,
// because the disposition-implied FILE_WRITE_DATA fold does not fire and the
// raw DesiredAccess carries no write requirement.
func TestCreate_OpenWithReadShareDoesNotConflictExistingHolder(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"
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
	fullAccessACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_WRITE_DATA | acl.ACE4_READ_ATTRIBUTES | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	existingFile, err := rt.GetMetadataService().CreateFile(rootAuth, rootHandle, "open_share.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
			UID:  requesterUID,
			GID:  requesterGID,
			ACL:  fullAccessACL,
		})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	const (
		fileReadData  uint32 = 0x00000001
		fileShareRead uint32 = 0x01
	)
	encHandle, err := metadata.EncodeFileHandle(existingFile)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}
	h.StoreOpenFile(&OpenFile{
		FileID:         h.GenerateFileID(),
		TreeID:         smbCtx.TreeID,
		SessionID:      smbCtx.SessionID,
		Path:           "open_share.txt",
		ShareName:      smbCtx.ShareName,
		DesiredAccess:  fileReadData,
		ShareAccess:    fileShareRead,
		MetadataHandle: encHandle,
	})

	draft := makeDraftWithShareAccess(t, tree, requesterAuth, rootHandle, existingFile, "open_share.txt",
		fileReadData, fileShareRead, types.FileOpen, types.FileOpened)
	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("FILE_OPEN READ_DATA + SHARE_READ vs SHARE_READ holder: status = 0x%08x, expected STATUS_SUCCESS",
			uint32(resp.Status))
	}
}

// TestCreate_OpenRestrictedFileForRead_Succeeds is the positive control for
// #565: the same READ-only DACL allows FILE_OPEN with DesiredAccess=READ_DATA
// (the spec test's first row at acls.c:3088). Guards against the
// disposition-implied augmentation accidentally firing for non-destructive
// dispositions.
func TestCreate_OpenRestrictedFileForRead_Succeeds(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	requesterUID := uint32(2001)
	requesterSID := "S-1-5-21-1-2-3-2001"
	existingFile := restrictedDACLFile(t, rt.GetMetadataService(), rootAuth, rootHandle, "open.txt", requesterSID)

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

	const rightReadData uint32 = 0x00000001
	draft := makeDraftWithDisposition(t, tree, requesterAuth, rootHandle, existingFile, "open.txt",
		rightReadData, types.FileOpen, types.FileOpened)
	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("FILE_OPEN READ on READ-only DACL: status = 0x%08x, expected STATUS_SUCCESS",
			uint32(resp.Status))
	}
}
