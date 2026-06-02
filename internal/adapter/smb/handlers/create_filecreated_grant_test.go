package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// TestCreate_FileCreated_GrantsResolvedDesiredAccess covers the regression
// behind #560 (smb2.acls.INHERITFLAGS) and #561 (smb2.acls.SDFLAGSVSCHOWN):
//
// Both tests build a parent directory with an inheritable DACL that grants
// the creator only a narrow subset of rights (SEC_FILE_WRITE_DATA |
// SEC_STD_WRITE_DAC). They then CREATE a child file with
// DesiredAccess=SEC_RIGHTS_FILE_ALL (0x001F01FF) and assert via
// FileAccessInformation that the open's granted access mask equals
// SEC_RIGHTS_FILE_ALL — not the narrowed subset that the inherited DACL
// would imply.
//
// Per MS-FSA §2.1.5.1.2 CreateFile and Samba
// source3/smbd/open.c::open_file_ntcreate, when a new file is created the
// server grants the creator the resolved DesiredAccess as-is. The parent
// DACL gates the create operation (already enforced upstream via
// CheckParentWriteAccess); the inherited child DACL governs subsequent
// opens by other principals and must not narrow the creator's handle.
//
// Before the fix, completeCreateAfterBreak re-evaluated DesiredAccess
// against the just-inherited child DACL for the FileCreated path, yielding
// the smbtorture symptom: granted=0x000e0002 vs expected=0x001f01ff.
func TestCreate_FileCreated_GrantsResolvedDesiredAccess(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	metaSvc := rt.GetMetadataService()

	creatorUID := uint32(1000)
	creatorGID := uint32(1000)

	// Parent directory's DACL: a single OI|CI ACE that only grants
	// WRITE_DATA + WRITE_DAC to the creator (mirrors smbtorture
	// test_inheritance_flags / test_sd_flags_vs_chown parent SD shape).
	parentACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag: acl.ACE4_FILE_INHERIT_ACE |
					acl.ACE4_DIRECTORY_INHERIT_ACE,
				// SEC_FILE_WRITE_DATA | SEC_STD_WRITE_DAC; APPEND_DATA is
				// added because DittoFS's PermissionWrite mapping currently
				// requires both bits on the parent (this is independent of
				// the bug under test — see permToACLMask in auth_permissions.go).
				AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA | acl.ACE4_WRITE_ACL,
				Who:        "1000@localdomain",
			},
		},
	}
	parentDir, _, err := metaSvc.CreateDirectory(rootAuth, rootHandle, "testdir",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
			UID:  creatorUID,
			GID:  creatorGID,
			ACL:  parentACL,
		})
	if err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	parentHandle, err := metadata.EncodeFileHandle(parentDir)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	creatorAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID:  &creatorUID,
			GID:  &creatorGID,
			GIDs: []uint32{creatorGID},
		},
	}

	const secRightsFileAll uint32 = 0x001F01FF

	draft := &createDraft{
		req: &CreateRequest{
			FileName:          "testfile",
			DesiredAccess:     secRightsFileAll,
			ShareAccess:       0x07,
			CreateDisposition: types.FileCreate,
			CreateOptions:     0,
			FileAttributes:    types.FileAttributeNormal,
		},
		tree:         tree,
		authCtx:      creatorAuth,
		filename:     "testdir\\testfile",
		baseName:     "testfile",
		parentHandle: parentHandle,
		fileExists:   false,
		createAction: types.FileCreated,
	}

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("CREATE: status=0x%08x, expected STATUS_SUCCESS", uint32(resp.Status))
	}

	openFile, ok := h.GetOpenFile(resp.FileID)
	if !ok {
		t.Fatal("OpenFile not registered after CREATE")
	}

	if openFile.GrantedAccess != secRightsFileAll {
		t.Errorf("GrantedAccess = 0x%08x; want 0x%08x (delta 0x%08x)",
			openFile.GrantedAccess, secRightsFileAll,
			secRightsFileAll^openFile.GrantedAccess)
	}
}

// TestCreate_FileCreated_ExpandsGenericAll covers the MAXIMUM_ALLOWED /
// GENERIC_ALL branch of resolveAccessFlags on the FileCreated path: when
// the caller asks for GENERIC_ALL on CREATE, the handle's GrantedAccess
// must report the full SEC_RIGHTS_FILE_ALL specific-rights mask
// (0x001F01FF), per MS-DTYP §2.4.3 GenericMapping for file objects.
func TestCreate_FileCreated_ExpandsGenericAll(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	metaSvc := rt.GetMetadataService()

	creatorUID := uint32(1000)
	creatorGID := uint32(1000)

	parentDir, _, err := metaSvc.CreateDirectory(rootAuth, rootHandle, "gendir",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
			UID:  creatorUID,
			GID:  creatorGID,
		})
	if err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	parentHandle, err := metadata.EncodeFileHandle(parentDir)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	creatorAuth := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID:  &creatorUID,
			GID:  &creatorGID,
			GIDs: []uint32{creatorGID},
		},
	}

	const (
		genericAll       uint32 = 0x10000000
		secRightsFileAll uint32 = 0x001F01FF
	)

	draft := &createDraft{
		req: &CreateRequest{
			FileName:          "f",
			DesiredAccess:     genericAll,
			ShareAccess:       0x07,
			CreateDisposition: types.FileCreate,
			CreateOptions:     0,
			FileAttributes:    types.FileAttributeNormal,
		},
		tree:         tree,
		authCtx:      creatorAuth,
		filename:     "gendir\\f",
		baseName:     "f",
		parentHandle: parentHandle,
		fileExists:   false,
		createAction: types.FileCreated,
	}

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("CREATE: status=0x%08x, expected STATUS_SUCCESS", uint32(resp.Status))
	}

	openFile, ok := h.GetOpenFile(resp.FileID)
	if !ok {
		t.Fatal("OpenFile not registered after CREATE")
	}

	if openFile.GrantedAccess&secRightsFileAll != secRightsFileAll {
		t.Errorf("GrantedAccess = 0x%08x; expected GENERIC_ALL to expand to >= 0x%08x",
			openFile.GrantedAccess, secRightsFileAll)
	}
	if openFile.GrantedAccess&genericAll != 0 {
		t.Errorf("GrantedAccess = 0x%08x; GENERIC_ALL bit must be stripped after expansion",
			openFile.GrantedAccess)
	}
}
