package handlers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	memorymeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// ============================================================================
// Real-FS Test Infrastructure
// ============================================================================

// realFSTestFixture holds state for real-FS handler tests.
type realFSTestFixture struct {
	handler    *Handler
	metaSvc    *metadata.MetadataService
	store      metadata.MetadataStore
	rootHandle metadata.FileHandle
	shareName  string
}

// newRealFSTestFixture creates a test fixture with an in-memory metadata store
// and a pseudo-fs with the given share. It creates the root directory in the store.
func newRealFSTestFixture(t *testing.T, shareName string) *realFSTestFixture {
	t.Helper()

	// Create in-memory metadata store
	store := memorymeta.NewMemoryMetadataStoreWithDefaults()

	// Create a runtime with nil control-plane store (tests don't need persistence)
	// The runtime creates its own MetadataService
	rt := runtime.New(nil)
	metaSvc := rt.GetMetadataService()
	metaSvc.SetDeferredCommit(false) // Disable deferred commits for testing

	// Register the in-memory store for our share
	if err := metaSvc.RegisterStoreForShare(shareName, store); err != nil {
		t.Fatalf("register store: %v", err)
	}

	// Create root directory handle
	rootID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootID)
	if err != nil {
		t.Fatalf("encode root handle: %v", err)
	}

	// Create root directory in the store
	rootFile := &metadata.File{
		ID:        rootID,
		ShareName: shareName,
		Path:      "/",
		FileAttr: metadata.FileAttr{
			Type:  metadata.FileTypeDirectory,
			Mode:  0o755,
			UID:   0,
			GID:   0,
			Nlink: 2,
			Atime: time.Now(),
			Mtime: time.Now(),
			Ctime: time.Now(),
		},
	}
	if err := store.PutFile(context.Background(), rootFile); err != nil {
		t.Fatalf("put root file: %v", err)
	}
	if err := store.SetLinkCount(context.Background(), rootHandle, 2); err != nil {
		t.Fatalf("set root link count: %v", err)
	}

	// Create pseudo-fs with the share
	pfs := pseudofs.New()
	pfs.Rebuild([]string{shareName})

	// Create handler with the real runtime
	handler := NewHandler(rt, pfs)

	return &realFSTestFixture{
		handler:    handler,
		metaSvc:    metaSvc,
		store:      store,
		rootHandle: rootHandle,
		shareName:  shareName,
	}
}

// createTestFile creates a file in the store and returns its handle.
func (f *realFSTestFixture) createTestFile(t *testing.T, parentHandle metadata.FileHandle, name string, fileType metadata.FileType, mode uint32, uid, gid uint32) metadata.FileHandle {
	t.Helper()

	fileID := uuid.New()
	fileHandle, err := metadata.EncodeShareHandle(f.shareName, fileID)
	if err != nil {
		t.Fatalf("encode file handle: %v", err)
	}

	now := time.Now()
	file := &metadata.File{
		ID:        fileID,
		ShareName: f.shareName,
		Path:      "/" + name,
		FileAttr: metadata.FileAttr{
			Type:  fileType,
			Mode:  mode,
			UID:   uid,
			GID:   gid,
			Nlink: 1,
			Size:  1024,
			Atime: now,
			Mtime: now,
			Ctime: now,
		},
	}

	if fileType == metadata.FileTypeSymlink {
		file.LinkTarget = "/tmp/target"
	}

	ctx := context.Background()
	if err := f.store.PutFile(ctx, file); err != nil {
		t.Fatalf("put file: %v", err)
	}
	if err := f.store.SetChild(ctx, parentHandle, name, fileHandle); err != nil {
		t.Fatalf("set child: %v", err)
	}
	if err := f.store.SetParent(ctx, fileHandle, parentHandle); err != nil {
		t.Fatalf("set parent: %v", err)
	}
	if err := f.store.SetLinkCount(ctx, fileHandle, file.Nlink); err != nil {
		t.Fatalf("set link count: %v", err)
	}

	return fileHandle
}

// newRealFSContext creates a CompoundContext with AUTH_UNIX credentials.
func newRealFSContext(uid, gid uint32) *types.CompoundContext {
	return &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "192.168.1.100:9999",
		AuthFlavor: 1, // AUTH_UNIX
		UID:        &uid,
		GID:        &gid,
	}
}

// ============================================================================
// LOOKUP Real-FS Tests
// ============================================================================

func TestLookup_RealFS_ResolvesChild(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a child file
	childHandle := fx.createTestFile(t, fx.rootHandle, "hello.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	// Build CompoundContext with real-FS root handle as current FH
	ctx := newRealFSContext(0, 0) // root user for permissions
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	// Call the handler directly
	reader := bytes.NewReader(nil)
	result := fx.handler.lookupInRealFS(ctx, "hello.txt")

	_ = reader // reader unused for LOOKUP real-FS (name passed directly)

	if result.Status != types.NFS4_OK {
		t.Fatalf("LOOKUP status = %d, want NFS4_OK", result.Status)
	}

	// CurrentFH should now be the child handle
	if !bytes.Equal(ctx.CurrentFH, []byte(childHandle)) {
		t.Errorf("CurrentFH = %q, want %q", string(ctx.CurrentFH), string(childHandle))
	}
}

func TestLookup_RealFS_NotFound(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	result := fx.handler.lookupInRealFS(ctx, "nonexistent.txt")

	if result.Status != types.NFS4ERR_NOENT {
		t.Errorf("LOOKUP status = %d, want NFS4ERR_NOENT (%d)", result.Status, types.NFS4ERR_NOENT)
	}
}

// ============================================================================
// LOOKUPP Real-FS Tests
// ============================================================================

func TestLookupP_RealFS_NavigatesToParent(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a child directory
	childHandle := fx.createTestFile(t, fx.rootHandle, "subdir", metadata.FileTypeDirectory, 0o755, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(childHandle))
	copy(ctx.CurrentFH, childHandle)

	result := fx.handler.lookupParentInRealFS(ctx)

	if result.Status != types.NFS4_OK {
		t.Fatalf("LOOKUPP status = %d, want NFS4_OK", result.Status)
	}

	// CurrentFH should now be the parent (root) handle
	if !bytes.Equal(ctx.CurrentFH, []byte(fx.rootHandle)) {
		t.Errorf("CurrentFH = %q, want root handle %q", string(ctx.CurrentFH), string(fx.rootHandle))
	}
}

func TestLookupP_RealFS_ShareRootCrossesToPseudoFS(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	// LOOKUPP from share root should cross back to pseudo-fs
	result := fx.handler.lookupParentInRealFS(ctx)

	if result.Status != types.NFS4_OK {
		t.Fatalf("LOOKUPP status = %d, want NFS4_OK", result.Status)
	}

	// CurrentFH should now be a pseudo-fs handle
	if !pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		t.Errorf("CurrentFH should be pseudo-fs handle, got %q", string(ctx.CurrentFH))
	}
}

// ============================================================================
// GETATTR Real-FS Tests
// ============================================================================

func TestGetAttr_RealFS_ReturnsCorrectAttributes(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a regular file
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Request TYPE, SIZE, MODE attributes
	var requested []uint32
	attrs.SetBit(&requested, attrs.FATTR4_TYPE)
	attrs.SetBit(&requested, attrs.FATTR4_SIZE)
	attrs.SetBit(&requested, attrs.FATTR4_MODE)

	result := fx.handler.getAttrRealFS(ctx, requested)

	if result.Status != types.NFS4_OK {
		t.Fatalf("GETATTR status = %d, want NFS4_OK", result.Status)
	}

	// Parse the response
	reader := bytes.NewReader(result.Data)

	// Status
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	// Response bitmap
	respBitmap, err := attrs.DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}

	if !attrs.IsBitSet(respBitmap, attrs.FATTR4_TYPE) {
		t.Error("response bitmap should have TYPE set")
	}
	if !attrs.IsBitSet(respBitmap, attrs.FATTR4_SIZE) {
		t.Error("response bitmap should have SIZE set")
	}
	if !attrs.IsBitSet(respBitmap, attrs.FATTR4_MODE) {
		t.Error("response bitmap should have MODE set")
	}

	// Attr data
	attrData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		t.Fatalf("decode attr data: %v", err)
	}
	attrReader := bytes.NewReader(attrData)

	// TYPE (NF4REG = 1)
	fileType, _ := xdr.DecodeUint32(attrReader)
	if fileType != types.NF4REG {
		t.Errorf("TYPE = %d, want NF4REG (%d)", fileType, types.NF4REG)
	}

	// SIZE (1024)
	size, _ := xdr.DecodeUint64(attrReader)
	if size != 1024 {
		t.Errorf("SIZE = %d, want 1024", size)
	}

	// MODE (0644)
	mode, _ := xdr.DecodeUint32(attrReader)
	if mode != 0o644 {
		t.Errorf("MODE = %o, want 0644", mode)
	}
}

func TestGetAttr_RealFS_DirectoryType(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	var requested []uint32
	attrs.SetBit(&requested, attrs.FATTR4_TYPE)

	result := fx.handler.getAttrRealFS(ctx, requested)
	if result.Status != types.NFS4_OK {
		t.Fatalf("GETATTR status = %d, want NFS4_OK", result.Status)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)
	_, _ = xdr.DecodeUint32(reader) // status
	_, _ = attrs.DecodeBitmap4(reader)
	attrData, _ := xdr.DecodeOpaque(reader)
	attrReader := bytes.NewReader(attrData)

	fileType, _ := xdr.DecodeUint32(attrReader)
	if fileType != types.NF4DIR {
		t.Errorf("TYPE = %d, want NF4DIR (%d)", fileType, types.NF4DIR)
	}
}

// ============================================================================
// READDIR Real-FS Tests
// ============================================================================

func TestReadDir_RealFS_ListsEntries(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create some children
	fx.createTestFile(t, fx.rootHandle, "file1.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)
	fx.createTestFile(t, fx.rootHandle, "file2.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)
	fx.createTestFile(t, fx.rootHandle, "subdir", metadata.FileTypeDirectory, 0o755, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	var attrRequest []uint32
	attrs.SetBit(&attrRequest, attrs.FATTR4_TYPE)

	result := fx.handler.readDirRealFS(ctx, 0, 8192, attrRequest)

	if result.Status != types.NFS4_OK {
		t.Fatalf("READDIR status = %d, want NFS4_OK", result.Status)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)

	// Status
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	// Cookie verifier (8 bytes)
	cookieVerf := make([]byte, 8)
	reader.Read(cookieVerf)

	// Read entries
	var entryNames []string
	for {
		hasNext, err := xdr.DecodeUint32(reader)
		if err != nil || hasNext == 0 {
			break
		}

		// cookie
		_, _ = xdr.DecodeUint64(reader)

		// name
		name, err := xdr.DecodeString(reader)
		if err != nil {
			t.Fatalf("decode entry name: %v", err)
		}
		entryNames = append(entryNames, name)

		// attrs (bitmap + opaque data)
		_, _ = attrs.DecodeBitmap4(reader)
		_, _ = xdr.DecodeOpaque(reader)
	}

	// eof
	eof, _ := xdr.DecodeUint32(reader)
	if eof != 1 {
		t.Errorf("eof = %d, want 1 (true)", eof)
	}

	if len(entryNames) != 3 {
		t.Fatalf("entry count = %d, want 3, got %v", len(entryNames), entryNames)
	}

	// Entries should contain all 3 names (sorted by name)
	found := map[string]bool{}
	for _, name := range entryNames {
		found[name] = true
	}
	for _, expected := range []string{"file1.txt", "file2.txt", "subdir"} {
		if !found[expected] {
			t.Errorf("missing entry %q in READDIR result %v", expected, entryNames)
		}
	}
}

// ============================================================================
// ACCESS Real-FS Tests
// ============================================================================

func TestAccess_RealFS_RootGetsAll(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	fileHandle := fx.createTestFile(t, fx.rootHandle, "restricted.txt", metadata.FileTypeRegular, 0o000, 1000, 1000)

	ctx := newRealFSContext(0, 0) // root
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	allBits := uint32(ACCESS4_READ | ACCESS4_LOOKUP | ACCESS4_MODIFY |
		ACCESS4_EXTEND | ACCESS4_DELETE | ACCESS4_EXECUTE)

	result := fx.handler.accessRealFS(ctx, allBits)

	if result.Status != types.NFS4_OK {
		t.Fatalf("ACCESS status = %d, want NFS4_OK", result.Status)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	supported, _ := xdr.DecodeUint32(reader)
	access, _ := xdr.DecodeUint32(reader)

	if supported != allBits {
		t.Errorf("supported = 0x%x, want 0x%x", supported, allBits)
	}
	if access != allBits {
		t.Errorf("access = 0x%x, want 0x%x (root should get all)", access, allBits)
	}
}

func TestAccess_RealFS_OwnerPermissions(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// File with mode 0644 owned by UID 1000
	fileHandle := fx.createTestFile(t, fx.rootHandle, "owned.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	ctx := newRealFSContext(1000, 1000) // owner
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	allBits := uint32(ACCESS4_READ | ACCESS4_MODIFY | ACCESS4_EXECUTE)

	result := fx.handler.accessRealFS(ctx, allBits)
	if result.Status != types.NFS4_OK {
		t.Fatalf("ACCESS status = %d, want NFS4_OK", result.Status)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)
	_, _ = xdr.DecodeUint32(reader) // status
	_, _ = xdr.DecodeUint32(reader) // supported
	access, _ := xdr.DecodeUint32(reader)

	// Owner should have READ (0o6xx) but NOT MODIFY (no write bit)
	// Mode 0644: owner = rw-, group = r--, other = r--
	if access&ACCESS4_READ == 0 {
		t.Error("owner should have READ access (0644)")
	}
	if access&ACCESS4_EXECUTE != 0 {
		t.Error("owner should NOT have EXECUTE access (0644)")
	}
}

func TestAccess_RealFS_OtherPermissions(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// File with mode 0700 owned by UID 1000
	fileHandle := fx.createTestFile(t, fx.rootHandle, "private.txt", metadata.FileTypeRegular, 0o700, 1000, 1000)

	ctx := newRealFSContext(2000, 2000) // different user
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	allBits := uint32(ACCESS4_READ | ACCESS4_MODIFY | ACCESS4_EXECUTE)

	result := fx.handler.accessRealFS(ctx, allBits)
	if result.Status != types.NFS4_OK {
		t.Fatalf("ACCESS status = %d, want NFS4_OK", result.Status)
	}

	reader := bytes.NewReader(result.Data)
	_, _ = xdr.DecodeUint32(reader) // status
	_, _ = xdr.DecodeUint32(reader) // supported
	access, _ := xdr.DecodeUint32(reader)

	// Other user should have NO access (mode 0700, other = ---)
	if access != 0 {
		t.Errorf("other user should have no access for mode 0700, got 0x%x", access)
	}
}

// ============================================================================
// READLINK Tests
// ============================================================================

func TestReadLink_RealFS_ReturnsTarget(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a symlink
	linkHandle := fx.createTestFile(t, fx.rootHandle, "mylink", metadata.FileTypeSymlink, 0o777, 1000, 1000)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(linkHandle))
	copy(ctx.CurrentFH, linkHandle)

	result := fx.handler.handleReadLink(ctx, bytes.NewReader(nil))

	if result.Status != types.NFS4_OK {
		t.Fatalf("READLINK status = %d, want NFS4_OK", result.Status)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	target, err := xdr.DecodeString(reader)
	if err != nil {
		t.Fatalf("decode target: %v", err)
	}
	if target != "/tmp/target" {
		t.Errorf("target = %q, want %q", target, "/tmp/target")
	}
}

func TestReadLink_PseudoFS_ReturnsINVAL(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  pfs.GetRootHandle(),
	}

	result := h.handleReadLink(ctx, bytes.NewReader(nil))

	if result.Status != types.NFS4ERR_INVAL {
		t.Errorf("READLINK on pseudo-fs status = %d, want NFS4ERR_INVAL (%d)",
			result.Status, types.NFS4ERR_INVAL)
	}
}

func TestReadLink_RealFS_NotSymlink(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a regular file (not a symlink)
	fileHandle := fx.createTestFile(t, fx.rootHandle, "regular.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	result := fx.handler.handleReadLink(ctx, bytes.NewReader(nil))

	// Should return INVAL (not a symlink)
	if result.Status != types.NFS4ERR_INVAL {
		t.Errorf("READLINK on regular file status = %d, want NFS4ERR_INVAL (%d)",
			result.Status, types.NFS4ERR_INVAL)
	}
}

func TestReadLink_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		// CurrentFH is nil
	}

	result := h.handleReadLink(ctx, bytes.NewReader(nil))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("READLINK without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

// ============================================================================
// Regression: Pseudo-fs tests still pass
// ============================================================================

func TestRegression_PseudoFS_LookupStillWorks(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export", "/data/archive"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeLookup("export"),
		encodeGetFH(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)
	if decoded.Status != types.NFS4_OK {
		t.Errorf("pseudo-fs LOOKUP regression: status = %d, want NFS4_OK", decoded.Status)
	}
}

func TestRegression_PseudoFS_GetAttrStillWorks(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeGetAttr(attrs.FATTR4_TYPE),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	// Parse raw response
	reader := bytes.NewReader(resp)
	overallStatus, _ := xdr.DecodeUint32(reader)
	if overallStatus != types.NFS4_OK {
		t.Errorf("pseudo-fs GETATTR regression: status = %d, want NFS4_OK", overallStatus)
	}
}

func TestRegression_PseudoFS_ReadDirStillWorks(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export", "/data/archive"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeReadDir(0, 8192, attrs.FATTR4_TYPE),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)
	if decoded.Status != types.NFS4_OK {
		t.Errorf("pseudo-fs READDIR regression: status = %d, want NFS4_OK", decoded.Status)
	}
}

func TestRegression_PseudoFS_AccessStillWorks(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeAccess(0x3F),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)
	if decoded.Status != types.NFS4_OK {
		t.Errorf("pseudo-fs ACCESS regression: status = %d, want NFS4_OK", decoded.Status)
	}
}

func TestRegression_PseudoFS_LookupPStillWorks(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeLookup("export"),
		encodeLookupP(),
		encodeGetFH(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)
	if decoded.Status != types.NFS4_OK {
		t.Errorf("pseudo-fs LOOKUPP regression: status = %d, want NFS4_OK", decoded.Status)
	}
}
