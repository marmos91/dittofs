package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// makeNewFileDraft assembles a createDraft for a fresh (not-yet-existing) file
// or directory under the given parent, ready for completeCreateAfterBreak.
// It mirrors what Create() produces after path resolution + the pre-break
// share-mode check on the FileCreated branch.
func makeNewFileDraft(
	tree *TreeConnection,
	authCtx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	req *CreateRequest,
	isDir bool,
) *createDraft {
	return &createDraft{
		req:                req,
		tree:               tree,
		authCtx:            authCtx,
		parentHandle:       parentHandle,
		filename:           req.FileName,
		baseName:           req.FileName,
		fileExists:         false,
		createAction:       types.FileCreated,
		isDirectoryRequest: isDir,
	}
}

// TestCreate_AllocSize_HonouredForNewFile reproduces the assertion in Samba
// smb2.durable-open.alloc-size (source4/torture/smb2/durable_open.c:2754): a
// fresh CREATE that carries an "AlSi" SMB2_CREATE_ALLOCATION_SIZE create
// context with a non-zero requested allocation MUST report a non-zero
// out.alloc_size, cluster-aligned. The create uses a batch oplock + durable
// request to match the smbtorture wire shape.
func TestCreate_AllocSize_HonouredForNewFile(t *testing.T) {
	h, _, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	req := &CreateRequest{
		FileName:          "alloc.txt",
		OplockLevel:       OplockLevelBatch,
		DesiredAccess:     0x001F01FF, // GENERIC_ALL expanded
		ShareAccess:       0x07,
		CreateDisposition: types.FileCreate,
		CreateOptions:     0,
		AllocationSize:    0x1000, // requested via AlSi context
	}

	draft := makeNewFileDraft(tree, rootAuth, rootHandle, req, false)

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("CREATE status = 0x%08x, expected STATUS_SUCCESS", uint32(resp.Status))
	}
	if resp.AllocationSize == 0 {
		t.Fatalf("out.alloc_size = 0, expected non-zero (requested 0x1000)")
	}
	if resp.AllocationSize != 0x1000 {
		t.Fatalf("out.alloc_size = 0x%x, expected 0x1000", resp.AllocationSize)
	}
	if resp.EndOfFile != 0 {
		t.Fatalf("out.size = %d, expected 0 for fresh empty file", resp.EndOfFile)
	}
}

// TestCreate_AllocSize_IgnoredForDirectory reproduces Samba
// smb2.create.dir_alloc_size (source4/torture/smb2/create.c:2064): a directory
// created with a large requested allocation MUST ignore it and report only its
// own (zero) cluster-aligned size — never the client reservation.
func TestCreate_AllocSize_IgnoredForDirectory(t *testing.T) {
	h, _, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	tree := &TreeConnection{TreeID: smbCtx.TreeID, SessionID: smbCtx.SessionID, ShareName: smbCtx.ShareName}
	h.StoreTree(tree)

	req := &CreateRequest{
		FileName:          "allocdir",
		OplockLevel:       OplockLevelNone,
		DesiredAccess:     0x001F01FF,
		ShareAccess:       0x07,
		CreateDisposition: types.FileCreate,
		CreateOptions:     types.FileDirectoryFile,
		AllocationSize:    1 << 30, // 1 GiB requested — must be ignored
	}

	draft := makeNewFileDraft(tree, rootAuth, rootHandle, req, true)

	resp := h.completeCreateAfterBreak(smbCtx, draft)
	if resp.Status != types.StatusSuccess {
		t.Fatalf("CREATE status = 0x%08x, expected STATUS_SUCCESS", uint32(resp.Status))
	}
	const oneMiB = 1 << 20
	if resp.AllocationSize > oneMiB {
		t.Fatalf("dir out.alloc_size = 0x%x, expected <= 1MiB (reservation must be ignored)", resp.AllocationSize)
	}
}

// TestCreate_AllocSize_EndToEndDurableBatch reproduces the full wire path of
// Samba smb2.durable-open.alloc-size end-to-end through Handler.Create: a fresh
// CREATE with a batch oplock, a DHnQ durable request, and an "AlSi" allocation
// create context (0x1000). The first assertion of the smbtorture test is
// out.alloc_size != 0. This drives the real decode → Create → response build
// chain (not just completeCreateAfterBreak in isolation) so any upstream
// zeroing of req.AllocationSize is caught.
func TestCreate_AllocSize_EndToEndDurableBatch(t *testing.T) {
	h, smbCtx := setupCreateWireTest(t)
	smbCtx.Context = context.Background()
	smbCtx.Permission = models.PermissionReadWrite
	if tree, ok := h.GetTree(smbCtx.TreeID); ok {
		tree.Permission = models.PermissionReadWrite
		h.StoreTree(tree)
	}
	h.DurableStore = newMockDurableStore()

	contexts := []CreateContext{
		// DHnQ durable handle request: 16-byte reserved blob per MS-SMB2
		// §2.2.13.2.3.
		{Name: DurableHandleV1RequestTag, Data: make([]byte, 16)},
		// AlSi allocation-size request: 8-byte LE = 0x1000.
		{Name: "AlSi", Data: []byte{0x00, 0x10, 0, 0, 0, 0, 0, 0}},
	}
	body := buildCreateRequestBodyWithContexts("durable_alloc.txt", types.FileCreate, 0, contexts)

	req, err := DecodeCreateRequest(body)
	if err != nil {
		t.Fatalf("DecodeCreateRequest: %v", err)
	}
	if req.AllocationSize != 0x1000 {
		t.Fatalf("decode: req.AllocationSize = 0x%x, expected 0x1000", req.AllocationSize)
	}
	req.OplockLevel = OplockLevelBatch
	req.DesiredAccess = 0x001F01FF

	resp, err := h.Create(smbCtx, req)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("CREATE status = 0x%08x, expected STATUS_SUCCESS", uint32(resp.Status))
	}
	if resp.AllocationSize == 0 {
		t.Fatalf("out.alloc_size = 0, expected non-zero (requested 0x1000 via AlSi)")
	}
	if resp.AllocationSize != 0x1000 {
		t.Fatalf("out.alloc_size = 0x%x, expected 0x1000", resp.AllocationSize)
	}
}
