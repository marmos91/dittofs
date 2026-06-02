package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Handler-level regression coverage for the MS-FSA §2.1.5.18 delete-on-close
// permission semantics exercised by smbtorture
// smb2.delete-on-close-perms.{CREATE,CREATE_IF,OVERWRITE_IF,READONLY}.
//
// These tests drive completeCreateAfterBreak directly with a pre-built
// createDraft (mirroring TestCreate_DaclEnforcement_*) so the DOC-permission
// arm of step 6d is exercised without bringing up the wire stack.

// setupDocTest stands up a Handler with an in-memory metadata store and
// returns a SMBHandlerContext / parent handle pair the test can use to seed
// the file under test. Authenticates as root for the parent-directory ops so
// the test scenarios isolate the DOC permission check on the *file*.
func setupDocTest(t *testing.T) (*Handler, *runtime.Runtime, *SMBHandlerContext, metadata.FileHandle, *metadata.AuthContext) {
	t.Helper()

	rt := runtime.New(nil)

	memStore := memory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("test-meta", memStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	shareName := "/doc-test"
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

// makeDocDraft builds a createDraft for a CREATE with the FILE_DELETE_ON_CLOSE
// option set. Disposition / fileExists / createAction / existingFile are
// supplied by the caller to cover the OPEN-existing and CREATE-new arms.
func makeDocDraft(
	t *testing.T,
	tree *TreeConnection,
	authCtx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	existingFile *metadata.File,
	baseName string,
	disposition types.CreateDisposition,
	createAction types.CreateAction,
	desiredAccess uint32,
	reqFileAttrs types.FileAttributes,
) *createDraft {
	t.Helper()
	var existingHandle metadata.FileHandle
	fileExists := existingFile != nil
	if fileExists {
		eh, err := metadata.EncodeFileHandle(existingFile)
		if err != nil {
			t.Fatalf("EncodeFileHandle: %v", err)
		}
		existingHandle = eh
	}
	return &createDraft{
		req: &CreateRequest{
			FileName:          baseName,
			DesiredAccess:     desiredAccess,
			FileAttributes:    reqFileAttrs,
			ShareAccess:       0x07, // R|W|D
			CreateDisposition: disposition,
			CreateOptions:     types.FileDeleteOnClose | types.FileNonDirectoryFile,
		},
		tree:           tree,
		authCtx:        authCtx,
		filename:       baseName,
		baseName:       baseName,
		parentHandle:   parentHandle,
		existingFile:   existingFile,
		existingHandle: existingHandle,
		fileExists:     fileExists,
		createAction:   createAction,
	}
}

// TestCreate_DeleteOnClose_ReadOnlyFile_ReturnsCannotDelete locks down the
// MS-FSA §2.1.5.18 gate exercised by smb2.delete-on-close-perms.READONLY
// step 6: OPEN-ing an existing file with FILE_ATTRIBUTE_READONLY and the
// FILE_DELETE_ON_CLOSE create option MUST return STATUS_CANNOT_DELETE,
// regardless of whether the caller holds DELETE on the file's DACL.
//
// The mode-synthesis path here mirrors what
// internal/adapter/smb/handlers/SMBModeFromAttrs(FileAttributeReadonly,
// false) produces after metadata.ApplyModeDefault strips the
// modeDOSExplicit bit (0o444). The owner-write-clear leg of
// fileAttrToSMBAttributesInternal restores FILE_ATTRIBUTE_READONLY on
// QUERY_INFO so the DOC-arm of completeCreateAfterBreak can recognize the
// readonly state.
func TestCreate_DeleteOnClose_ReadOnlyFile_ReturnsCannotDelete(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDocTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	metaSvc := rt.GetMetadataService()

	// Persist a regular file with the mode bits CREATE+FILE_ATTRIBUTE_READONLY
	// would leave after the store round-trip (owner-write cleared, no DACL).
	readonlyFile, _, err := metaSvc.CreateFile(rootAuth, rootHandle, "ro.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o444,
			UID:  0,
			GID:  0,
		})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	// OPEN existing READONLY file with DELETE_ON_CLOSE. DesiredAccess carries
	// SEC_STD_DELETE | SEC_RIGHTS_FILE_READ so hasDeleteAccess is satisfied —
	// the MS-FSA §2.1.5.18 readonly veto must still fire.
	const desiredAccess uint32 = 0x00130089 // SEC_RIGHTS_FILE_READ | SEC_STD_DELETE
	draft := makeDocDraft(t, tree, rootAuth, rootHandle, readonlyFile,
		"ro.txt", types.FileOpen, types.FileOpened, desiredAccess, 0)

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusCannotDelete {
		t.Fatalf("DOC on existing READONLY: status = 0x%08x, expected STATUS_CANNOT_DELETE (0x%08x)",
			uint32(resp.Status), uint32(types.StatusCannotDelete))
	}
}

// TestCreate_DeleteOnClose_NewFileWithReadOnlyAttr_ReturnsCannotDelete covers
// smb2.delete-on-close-perms.READONLY step 1: a CREATE that asks to mark a
// new file READONLY *and* DELETE_ON_CLOSE simultaneously is an immediate
// MS-FSA §2.1.5.18 contradiction. completeCreateAfterBreak must reject the
// open with STATUS_CANNOT_DELETE before the file is materialized in the
// metadata store.
func TestCreate_DeleteOnClose_NewFileWithReadOnlyAttr_ReturnsCannotDelete(t *testing.T) {
	h, _, smbCtx, rootHandle, rootAuth := setupDocTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	const desiredAccess uint32 = 0x001F01FF // SEC_RIGHTS_DIR_ALL (carries DELETE)
	draft := makeDocDraft(t, tree, rootAuth, rootHandle, nil,
		"new-ro.txt", types.FileCreate, types.FileCreated,
		desiredAccess, types.FileAttributeReadonly)

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusCannotDelete {
		t.Fatalf("CREATE+READONLY+DOC: status = 0x%08x, expected STATUS_CANNOT_DELETE",
			uint32(resp.Status))
	}
}

// TestCreate_DeleteOnClose_WithoutDeleteAccess_ReturnsAccessDenied locks down
// the MS-FSA §2.1.5.18 gate that the DOC option requires DELETE in
// DesiredAccess (Samba: smbd_set_initial_delete_on_close). This is the
// "hasDeleteAccess" leg of step 6d and is the first guard the DOC-perms
// suite relies on for new-file scenarios.
func TestCreate_DeleteOnClose_WithoutDeleteAccess_ReturnsAccessDenied(t *testing.T) {
	h, _, smbCtx, rootHandle, rootAuth := setupDocTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	// DesiredAccess deliberately omits SEC_STD_DELETE / GENERIC_ALL /
	// MAXIMUM_ALLOWED — hasDeleteAccess must return false and the DOC gate
	// must fail the open with STATUS_ACCESS_DENIED.
	const desiredAccess uint32 = 0x00120089 // SEC_RIGHTS_FILE_READ only
	draft := makeDocDraft(t, tree, rootAuth, rootHandle, nil,
		"no-del.txt", types.FileCreate, types.FileCreated, desiredAccess, 0)

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusAccessDenied {
		t.Fatalf("DOC without DELETE in DesiredAccess: status = 0x%08x, expected STATUS_ACCESS_DENIED",
			uint32(resp.Status))
	}
}
