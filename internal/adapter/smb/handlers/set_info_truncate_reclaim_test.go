// Regression coverage for SET_INFO FileEndOfFileInformation truncate-down: a
// size-down must physically discard block data past the new EOF, so that a
// later re-extend exposes a zero-filled hole rather than the pre-truncation
// bytes (silent data-integrity / info-leak bug — BUGREPORT-sparse-truncate).
package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	cpstore "github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	metamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// setupTruncateTest builds a memory-backed runtime with a single share
// containing a regular file `repro.bin`, returning the handler, root auth
// context, and an OpenFile granting read+write data access (the access an
// SMB truncate authorizes from).
func setupTruncateTest(t *testing.T) (*Handler, *metadata.AuthContext, *OpenFile) {
	t.Helper()
	ctx := context.Background()

	cps, err := cpstore.New(&cpstore.Config{
		Type:   cpstore.DatabaseTypeSQLite,
		SQLite: cpstore.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("cpstore.New: %v", err)
	}
	rt := runtime.New(cps)

	if _, err := cps.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "trunc-meta", Type: "memory"}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	if err := rt.RegisterMetadataStore("trunc-meta", metamemory.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	localBSID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{
		Name: "trunc-bs", Kind: models.BlockStoreKindLocal, Type: "memory",
	})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}

	const shareName = "/trunc"
	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:              shareName,
		MetadataStore:     "trunc-meta",
		Enabled:           true,
		LocalBlockStoreID: localBSID,
		RootAttr:          &metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777},
	}); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	rootHandle, err := rt.GetRootHandle(shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	uid, gid := uint32(0), uint32(0)
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}

	metaSvc := rt.GetMetadataService()
	file, _, err := metaSvc.CreateFile(authCtx, rootHandle, "repro.bin", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0o644,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	h := NewHandler()
	h.Registry = rt

	open := &OpenFile{
		FileID:         [16]byte{0x7C, 0x01},
		MetadataHandle: handle,
		ParentHandle:   rootHandle,
		FileName:       "repro.bin",
		Path:           "repro.bin",
		ShareName:      shareName,
		GrantedAccess: uint32(types.FileWriteData) | uint32(types.FileReadData) |
			uint32(types.FileWriteAttributes) | uint32(types.FileReadAttributes),
		DesiredAccess: uint32(types.FileWriteData) | uint32(types.FileReadData),
	}
	h.StoreOpenFile(open)
	return h, authCtx, open
}

// writeAt mirrors the SMB WRITE handler seam: reserve the size via PrepareWrite
// then push bytes through common.WriteToBlockStore at an absolute offset.
func writeAt(t *testing.T, h *Handler, authCtx *metadata.AuthContext, open *OpenFile, data []byte, offset uint64) {
	t.Helper()
	metaSvc := h.Registry.GetMetadataService()
	blockStore, err := common.ResolveForWrite(authCtx.Context, h.Registry, open.MetadataHandle)
	if err != nil {
		t.Fatalf("ResolveForWrite: %v", err)
	}
	writeOp, err := metaSvc.PrepareWrite(authCtx, open.MetadataHandle, offset+uint64(len(data)))
	if err != nil {
		t.Fatalf("PrepareWrite@%d: %v", offset, err)
	}
	if err := common.WriteToBlockStore(authCtx.Context, blockStore, writeOp.PayloadID, data, offset); err != nil {
		t.Fatalf("WriteToBlockStore@%d: %v", offset, err)
	}
	// Commit the size to metadata, exactly as the SMB WRITE handler does.
	if _, err := metaSvc.CommitWrite(authCtx, writeOp); err != nil {
		t.Fatalf("CommitWrite@%d: %v", offset, err)
	}
}

// truncateTo drives the real SET_INFO FileEndOfFileInformation handler — the
// code path under test — to shrink the file to newSize.
func truncateTo(t *testing.T, h *Handler, authCtx *metadata.AuthContext, open *OpenFile, newSize uint64) {
	t.Helper()
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, newSize)
	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileEndOfFileInformation, buf)
	if err != nil {
		t.Fatalf("setFileInfoFromStore(EOF %d): %v", newSize, err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("SET_INFO EOF %d: status = 0x%08X, want SUCCESS", newSize, uint32(resp.Status))
	}
}

// allocTo drives SET_INFO FileAllocationInformation (allocation-size set) to
// shrink the file to newSize — the other SMB info class that can truncate.
func allocTo(t *testing.T, h *Handler, authCtx *metadata.AuthContext, open *OpenFile, newSize uint64) {
	t.Helper()
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, newSize)
	resp, err := h.setFileInfoFromStore(nil, authCtx, open, types.FileAllocationInformation, buf)
	if err != nil {
		t.Fatalf("setFileInfoFromStore(alloc %d): %v", newSize, err)
	}
	if resp.Status != types.StatusSuccess {
		t.Fatalf("SET_INFO alloc %d: status = 0x%08X, want SUCCESS", newSize, uint32(resp.Status))
	}
}

// readAtOffset reads count bytes at offset through the block store, as the SMB
// READ handler does.
func readAtOffset(t *testing.T, h *Handler, authCtx *metadata.AuthContext, open *OpenFile, offset uint64, count uint32) []byte {
	t.Helper()
	blockStore, err := common.ResolveForRead(authCtx.Context, h.Registry, open.MetadataHandle)
	if err != nil {
		t.Fatalf("ResolveForRead: %v", err)
	}
	res, err := common.ReadFromBlockStore(authCtx.Context, blockStore, mustPayloadID(t, h, authCtx, open), offset, count)
	if err != nil {
		t.Fatalf("ReadFromBlockStore@%d: %v", offset, err)
	}
	return res.Data
}

// TestSetInfo_AllocationTruncateDown_ReExtend_ReadsZeros is the
// FileAllocationInformation twin of the EOF test: SetAllocationInfo below the
// current size (Samba does this — setinfo.c sets AllocationInformation=0) must
// also discard block data past the new EOF, so a re-extend reads zeros.
func TestSetInfo_AllocationTruncateDown_ReExtend_ReadsZeros(t *testing.T) {
	h, authCtx, open := setupTruncateTest(t)

	const off = uint64(1_000_000)
	writeAt(t, h, authCtx, open, []byte("ABCDEFGH"), off)
	allocTo(t, h, authCtx, open, 1000)                      // allocation-driven truncate DOWN
	writeAt(t, h, authCtx, open, []byte("ZZZZ"), 2_000_000) // re-extend

	for i, b := range readAtOffset(t, h, authCtx, open, off, 8) {
		if b != 0 {
			t.Fatalf("re-extended hole leaked stale data after alloc-truncate: byte %x at offset %d, want zero", b, off+uint64(i))
		}
	}
}

// TestSetInfo_TruncateDown_ReExtend_ReadsZeros reproduces the sparse-truncate
// data-integrity bug end-to-end through the SMB set_info handler:
//
//  1. write bytes far past EOF,
//  2. truncate DOWN below them (must discard the tail),
//  3. re-extend past the old offset (the region becomes a hole),
//  4. read the offset back — POSIX requires zeros.
//
// Before the fix, step 2 updated metadata size only and never called
// blockStore.Truncate, so the tail bytes survived and step 4 returned them.
func TestSetInfo_TruncateDown_ReExtend_ReadsZeros(t *testing.T) {
	h, authCtx, open := setupTruncateTest(t)

	const off = uint64(1_000_000)
	secret := []byte("ABCDEFGH")

	// 1. Write 8 bytes at a high offset.
	writeAt(t, h, authCtx, open, secret, off)

	// 2. Truncate DOWN below the written data — must discard it.
	truncateTo(t, h, authCtx, open, 1000)

	// 3. Re-extend past the old offset via a write further out; [1000, 2_000_000)
	//    (including off) is now a hole.
	writeAt(t, h, authCtx, open, []byte("ZZZZ"), 2_000_000)

	// 4. Read back the offset — it must read as zeros, not the discarded bytes.
	blockStore, err := common.ResolveForRead(authCtx.Context, h.Registry, open.MetadataHandle)
	if err != nil {
		t.Fatalf("ResolveForRead: %v", err)
	}
	res, err := common.ReadFromBlockStore(authCtx.Context, blockStore, mustPayloadID(t, h, authCtx, open), off, uint32(len(secret)))
	if err != nil {
		t.Fatalf("ReadFromBlockStore@%d: %v", off, err)
	}

	for i, b := range res.Data {
		if b != 0 {
			t.Fatalf("re-extended hole leaked stale data: byte %#x at offset %d (full read %x), want zero", b, off+uint64(i), res.Data)
		}
	}
}

// mustPayloadID returns the file's current PayloadID (assigned on first write).
func mustPayloadID(t *testing.T, h *Handler, authCtx *metadata.AuthContext, open *OpenFile) metadata.PayloadID {
	t.Helper()
	file, err := h.Registry.GetMetadataService().GetFile(authCtx.Context, open.MetadataHandle)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	return file.PayloadID
}
