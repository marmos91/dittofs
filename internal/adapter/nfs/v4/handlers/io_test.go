package handlers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	memorymeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/payload"
	storemem "github.com/marmos91/dittofs/pkg/payload/store/memory"
	"github.com/marmos91/dittofs/pkg/payload/transfer"
)

// ============================================================================
// I/O Test Fixture (includes PayloadService for READ/WRITE/COMMIT)
// ============================================================================

// ioTestFixture extends realFSTestFixture with payload service support.
type ioTestFixture struct {
	handler    *Handler
	metaSvc    *metadata.MetadataService
	payloadSvc *payload.PayloadService
	store      metadata.MetadataStore
	rootHandle metadata.FileHandle
	shareName  string
}

// newIOTestFixture creates a test fixture with metadata + payload services.
func newIOTestFixture(t *testing.T, shareName string) *ioTestFixture {
	t.Helper()

	// Create in-memory metadata store
	metaStore := memorymeta.NewMemoryMetadataStoreWithDefaults()

	// Create cache, block store, and transfer manager for payload operations
	testCache := cache.New(0) // unlimited
	blockStore := storemem.New()
	transferMgr := transfer.New(testCache, blockStore, metaStore, transfer.DefaultConfig())

	payloadSvc, err := payload.New(testCache, transferMgr)
	if err != nil {
		t.Fatalf("create payload service: %v", err)
	}

	// Create runtime with payload service
	rt := runtime.New(nil)
	rt.SetPayloadService(payloadSvc)
	metaSvc := rt.GetMetadataService()
	metaSvc.SetDeferredCommit(false)

	// Register metadata store
	if err := rt.RegisterMetadataStore("test-meta", metaStore); err != nil {
		t.Fatalf("register store: %v", err)
	}

	// Add share
	shareConfig := &runtime.ShareConfig{
		Name:          shareName,
		MetadataStore: "test-meta",
		RootAttr:      &metadata.FileAttr{},
	}
	if err := rt.AddShare(context.Background(), shareConfig); err != nil {
		t.Fatalf("add share: %v", err)
	}

	// Get root handle
	share, err := rt.GetShare(shareName)
	if err != nil {
		t.Fatalf("get share: %v", err)
	}

	// Create pseudo-fs
	pfs := pseudofs.New()
	pfs.Rebuild([]string{shareName})

	handler := NewHandler(rt, pfs)

	return &ioTestFixture{
		handler:    handler,
		metaSvc:    metaSvc,
		payloadSvc: payloadSvc,
		store:      metaStore,
		rootHandle: share.RootHandle,
		shareName:  shareName,
	}
}

// createRegularFile creates a regular file in the metadata store and returns its handle.
func (fx *ioTestFixture) createRegularFile(t *testing.T, parentHandle metadata.FileHandle, name string, mode uint32, uid, gid uint32) metadata.FileHandle {
	t.Helper()

	authCtx := &metadata.AuthContext{
		Context:    context.Background(),
		ClientAddr: "test:9999",
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: &uid,
			GID: &gid,
		},
	}

	file, err := fx.metaSvc.CreateFile(authCtx, parentHandle, name, &metadata.FileAttr{
		Mode: mode,
		UID:  uid,
		GID:  gid,
	})
	if err != nil {
		t.Fatalf("create file %q: %v", name, err)
	}

	fh, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("encode handle: %v", err)
	}
	return fh
}

// writeContent writes content to a file's payload and updates metadata size.
func (fx *ioTestFixture) writeContent(t *testing.T, fileHandle metadata.FileHandle, data []byte) {
	t.Helper()

	ctx := context.Background()

	// Get file to obtain PayloadID
	file, err := fx.metaSvc.GetFile(ctx, fileHandle)
	if err != nil {
		t.Fatalf("get file for write: %v", err)
	}

	// Write via payload service
	if err := fx.payloadSvc.WriteAt(ctx, file.PayloadID, data, 0); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	// Update file size in metadata
	uid := uint32(0)
	gid := uint32(0)
	authCtx := &metadata.AuthContext{
		Context:    ctx,
		ClientAddr: "test:9999",
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: &uid,
			GID: &gid,
		},
	}
	newSize := uint64(len(data))
	if err := fx.metaSvc.SetFileAttributes(authCtx, fileHandle, &metadata.SetAttrs{
		Size: &newSize,
	}); err != nil {
		t.Fatalf("update file size: %v", err)
	}
}

// createDirectory creates a test directory and returns its handle.
func (fx *ioTestFixture) createDirectory(t *testing.T, parentHandle metadata.FileHandle, name string) metadata.FileHandle {
	t.Helper()

	fileID := uuid.New()
	fileHandle, err := metadata.EncodeShareHandle(fx.shareName, fileID)
	if err != nil {
		t.Fatalf("encode handle: %v", err)
	}

	now := time.Now()
	file := &metadata.File{
		ID:        fileID,
		ShareName: fx.shareName,
		Path:      "/" + name,
		FileAttr: metadata.FileAttr{
			Type:  metadata.FileTypeDirectory,
			Mode:  0o755,
			UID:   0,
			GID:   0,
			Nlink: 2,
			Atime: now,
			Mtime: now,
			Ctime: now,
		},
	}

	ctx := context.Background()
	if err := fx.store.PutFile(ctx, file); err != nil {
		t.Fatalf("put dir: %v", err)
	}
	if err := fx.store.SetChild(ctx, parentHandle, name, fileHandle); err != nil {
		t.Fatalf("set child: %v", err)
	}
	if err := fx.store.SetParent(ctx, fileHandle, parentHandle); err != nil {
		t.Fatalf("set parent: %v", err)
	}
	if err := fx.store.SetLinkCount(ctx, fileHandle, 2); err != nil {
		t.Fatalf("set link count: %v", err)
	}

	return fileHandle
}

// ============================================================================
// XDR Encoding Helpers for I/O Operations
// ============================================================================

// Special stateids per RFC 7530
var (
	anonymousStateid = types.Stateid4{
		Seqid: 0,
		Other: [types.NFS4_OTHER_SIZE]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}
)

func encodeOpenArgs(
	seqid uint32,
	shareAccess uint32,
	shareDeny uint32,
	clientID uint64,
	owner []byte,
	openType uint32,
	createMode uint32,
	claimType uint32,
	filename string,
) []byte {
	var buf bytes.Buffer

	// seqid
	_ = xdr.WriteUint32(&buf, seqid)
	// share_access
	_ = xdr.WriteUint32(&buf, shareAccess)
	// share_deny
	_ = xdr.WriteUint32(&buf, shareDeny)
	// open_owner4: clientid + owner
	_ = xdr.WriteUint64(&buf, clientID)
	_ = xdr.WriteXDROpaque(&buf, owner)
	// openflag4: opentype
	_ = xdr.WriteUint32(&buf, openType)

	if openType == types.OPEN4_CREATE {
		// createhow4: createmode
		_ = xdr.WriteUint32(&buf, createMode)
		switch createMode {
		case types.UNCHECKED4, types.GUARDED4:
			// createattrs: empty bitmap + empty opaque
			_ = xdr.WriteUint32(&buf, 0) // bitmap length
			_ = xdr.WriteUint32(&buf, 0) // attr data length
		case types.EXCLUSIVE4:
			// verifier (8 bytes)
			buf.Write(make([]byte, 8))
		}
	}

	// open_claim4: claim_type
	_ = xdr.WriteUint32(&buf, claimType)
	if claimType == types.CLAIM_NULL {
		_ = xdr.WriteXDRString(&buf, filename)
	}

	return buf.Bytes()
}

func encodeOpenConfirmArgs(stateid *types.Stateid4, seqid uint32) []byte {
	var buf bytes.Buffer
	types.EncodeStateid4(&buf, stateid)
	_ = xdr.WriteUint32(&buf, seqid)
	return buf.Bytes()
}

func encodeCloseArgs(seqid uint32, stateid *types.Stateid4) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, seqid)
	types.EncodeStateid4(&buf, stateid)
	return buf.Bytes()
}

func encodeReadArgs(stateid *types.Stateid4, offset uint64, count uint32) []byte {
	var buf bytes.Buffer
	types.EncodeStateid4(&buf, stateid)
	_ = xdr.WriteUint64(&buf, offset)
	_ = xdr.WriteUint32(&buf, count)
	return buf.Bytes()
}

func encodeWriteArgs(stateid *types.Stateid4, offset uint64, stable uint32, data []byte) []byte {
	var buf bytes.Buffer
	types.EncodeStateid4(&buf, stateid)
	_ = xdr.WriteUint64(&buf, offset)
	_ = xdr.WriteUint32(&buf, stable)
	_ = xdr.WriteXDROpaque(&buf, data)
	return buf.Bytes()
}

func encodeCommitArgs(offset uint64, count uint32) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteUint64(&buf, offset)
	_ = xdr.WriteUint32(&buf, count)
	return buf.Bytes()
}

// ============================================================================
// OPEN Tests
// ============================================================================

func TestOpen_CreateFile_Success(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0) // root user
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgs(
		1,                             // seqid
		types.OPEN4_SHARE_ACCESS_BOTH, // share_access
		types.OPEN4_SHARE_DENY_NONE,   // share_deny
		0x12345678,                    // clientid
		[]byte("test-owner"),          // owner
		types.OPEN4_CREATE,            // opentype
		types.UNCHECKED4,              // createmode
		types.CLAIM_NULL,              // claim_type
		"newfile.txt",                 // filename
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("OPEN CREATE status = %d, want NFS4_OK", result.Status)
	}
	if result.OpCode != types.OP_OPEN {
		t.Errorf("OPEN opcode = %d, want OP_OPEN (%d)", result.OpCode, types.OP_OPEN)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader) // status
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	// stateid4: seqid + other[12]
	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		t.Fatalf("decode stateid: %v", err)
	}
	if stateid.Seqid != 1 {
		t.Errorf("stateid.seqid = %d, want 1", stateid.Seqid)
	}

	// change_info4
	_, _ = xdr.DecodeUint32(reader) // atomic
	_, _ = xdr.DecodeUint64(reader) // before
	_, _ = xdr.DecodeUint64(reader) // after

	// rflags
	rflags, _ := xdr.DecodeUint32(reader)
	if rflags&types.OPEN4_RESULT_CONFIRM == 0 {
		t.Error("rflags should have OPEN4_RESULT_CONFIRM set")
	}

	// attrset
	bitmapLen, _ := xdr.DecodeUint32(reader)
	if bitmapLen != 0 {
		t.Errorf("attrset bitmap len = %d, want 0", bitmapLen)
	}

	// delegation_type
	delegType, _ := xdr.DecodeUint32(reader)
	if delegType != types.OPEN_DELEGATE_NONE {
		t.Errorf("delegation type = %d, want OPEN_DELEGATE_NONE (%d)", delegType, types.OPEN_DELEGATE_NONE)
	}

	// Verify CurrentFH changed to the new file
	if bytes.Equal(ctx.CurrentFH, []byte(fx.rootHandle)) {
		t.Error("CurrentFH should have changed to the new file's handle")
	}

	// Verify file exists
	authCtx := newTestAuthCtx(0, 0)
	child, lookupErr := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "newfile.txt")
	if lookupErr != nil {
		t.Fatalf("Lookup after OPEN CREATE: %v", lookupErr)
	}
	if child.Type != metadata.FileTypeRegular {
		t.Errorf("created file type = %v, want regular", child.Type)
	}
}

func TestOpen_ExistingFile_NocreateSuccess(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	// Create a file first
	fx.createRegularFile(t, fx.rootHandle, "existing.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgs(
		1,
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		0x12345678,
		[]byte("test-owner"),
		types.OPEN4_NOCREATE,
		0,
		types.CLAIM_NULL,
		"existing.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("OPEN NOCREATE status = %d, want NFS4_OK", result.Status)
	}
}

func TestOpen_NocreateNonexistent_ReturnsNOENT(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgs(
		1,
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		0x12345678,
		[]byte("test-owner"),
		types.OPEN4_NOCREATE,
		0,
		types.CLAIM_NULL,
		"nonexistent.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOENT {
		t.Errorf("OPEN NOCREATE nonexistent status = %d, want NFS4ERR_NOENT (%d)",
			result.Status, types.NFS4ERR_NOENT)
	}
}

func TestOpen_GuardedExisting_ReturnsEXIST(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fx.createRegularFile(t, fx.rootHandle, "existing.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgs(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		0x12345678,
		[]byte("test-owner"),
		types.OPEN4_CREATE,
		types.GUARDED4,
		types.CLAIM_NULL,
		"existing.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_EXIST {
		t.Errorf("OPEN GUARDED existing status = %d, want NFS4ERR_EXIST (%d)",
			result.Status, types.NFS4ERR_EXIST)
	}
}

func TestOpen_UncheckedExisting_OpensFile(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fx.createRegularFile(t, fx.rootHandle, "existing.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgs(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		0x12345678,
		[]byte("test-owner"),
		types.OPEN4_CREATE,
		types.UNCHECKED4,
		types.CLAIM_NULL,
		"existing.txt",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Errorf("OPEN UNCHECKED existing status = %d, want NFS4_OK", result.Status)
	}
}

func TestOpen_PseudoFS_ReturnsROFS(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = pfs.GetRootHandle()

	args := encodeOpenArgs(
		1,
		types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE,
		0x12345678,
		[]byte("test-owner"),
		types.OPEN4_CREATE,
		types.UNCHECKED4,
		types.CLAIM_NULL,
		"test.txt",
	)

	result := h.handleOpen(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ROFS {
		t.Errorf("OPEN on pseudo-fs status = %d, want NFS4ERR_ROFS (%d)",
			result.Status, types.NFS4ERR_ROFS)
	}
}

func TestOpen_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = nil

	args := encodeOpenArgs(1, 0, 0, 0, nil, 0, 0, 0, "")
	result := h.handleOpen(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("OPEN without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestOpen_InvalidFilename(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeOpenArgs(
		1,
		types.OPEN4_SHARE_ACCESS_READ,
		types.OPEN4_SHARE_DENY_NONE,
		0x12345678,
		[]byte("test-owner"),
		types.OPEN4_NOCREATE,
		0,
		types.CLAIM_NULL,
		"bad/name",
	)

	result := fx.handler.handleOpen(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_BADNAME {
		t.Errorf("OPEN with slash status = %d, want NFS4ERR_BADNAME (%d)",
			result.Status, types.NFS4ERR_BADNAME)
	}
}

// ============================================================================
// OPEN_CONFIRM Tests
// ============================================================================

func TestOpenConfirm_Success(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	// First do an OPEN to get a stateid
	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	openArgs := encodeOpenArgs(
		1, types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE,
		0x12345678, []byte("test-owner"),
		types.OPEN4_CREATE, types.UNCHECKED4, types.CLAIM_NULL, "file.txt",
	)
	openResult := fx.handler.handleOpen(ctx, bytes.NewReader(openArgs))
	if openResult.Status != types.NFS4_OK {
		t.Fatalf("OPEN status = %d, want NFS4_OK", openResult.Status)
	}

	// Parse the stateid from OPEN response
	openReader := bytes.NewReader(openResult.Data)
	_, _ = xdr.DecodeUint32(openReader) // status
	openStateid, err := types.DecodeStateid4(openReader)
	if err != nil {
		t.Fatalf("decode open stateid: %v", err)
	}

	// OPEN_CONFIRM
	confirmArgs := encodeOpenConfirmArgs(openStateid, 2)
	confirmResult := fx.handler.handleOpenConfirm(ctx, bytes.NewReader(confirmArgs))

	if confirmResult.Status != types.NFS4_OK {
		t.Fatalf("OPEN_CONFIRM status = %d, want NFS4_OK", confirmResult.Status)
	}

	// Parse response
	confirmReader := bytes.NewReader(confirmResult.Data)
	status, _ := xdr.DecodeUint32(confirmReader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	confirmStateid, err := types.DecodeStateid4(confirmReader)
	if err != nil {
		t.Fatalf("decode confirm stateid: %v", err)
	}

	// Seqid should be incremented
	if confirmStateid.Seqid != openStateid.Seqid+1 {
		t.Errorf("confirm seqid = %d, want %d", confirmStateid.Seqid, openStateid.Seqid+1)
	}
}

func TestOpenConfirm_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = nil

	args := encodeOpenConfirmArgs(&anonymousStateid, 1)
	result := h.handleOpenConfirm(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("OPEN_CONFIRM without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

// ============================================================================
// CLOSE Tests
// ============================================================================

func TestClose_Success(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "closeme.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	args := encodeCloseArgs(1, &anonymousStateid)
	result := fx.handler.handleClose(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("CLOSE status = %d, want NFS4_OK", result.Status)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	// Closed stateid should be zeroed
	closedStateid, err := types.DecodeStateid4(reader)
	if err != nil {
		t.Fatalf("decode closed stateid: %v", err)
	}
	if closedStateid.Seqid != 0 {
		t.Errorf("closed seqid = %d, want 0", closedStateid.Seqid)
	}
	for i, b := range closedStateid.Other {
		if b != 0 {
			t.Errorf("closed other[%d] = %d, want 0", i, b)
		}
	}

	// CLOSE should NOT clear CurrentFH per RFC 7530
	if ctx.CurrentFH == nil {
		t.Error("CurrentFH should NOT be cleared by CLOSE")
	}
}

func TestClose_PseudoFS_ReturnsINVAL(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = pfs.GetRootHandle()

	args := encodeCloseArgs(1, &anonymousStateid)
	result := h.handleClose(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_INVAL {
		t.Errorf("CLOSE on pseudo-fs status = %d, want NFS4ERR_INVAL (%d)",
			result.Status, types.NFS4ERR_INVAL)
	}
}

func TestClose_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = nil

	args := encodeCloseArgs(1, &anonymousStateid)
	result := h.handleClose(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("CLOSE without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

// ============================================================================
// READ Tests
// ============================================================================

func TestRead_Success(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "readme.txt", 0o644, 0, 0)
	content := []byte("Hello, NFSv4!")
	fx.writeContent(t, fileHandle, content)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	args := encodeReadArgs(&anonymousStateid, 0, 1024)
	result := fx.handler.handleRead(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("READ status = %d, want NFS4_OK", result.Status)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader) // status
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	eof, _ := xdr.DecodeUint32(reader)
	if eof != 1 {
		t.Errorf("eof = %d, want 1 (EOF for small file read completely)", eof)
	}

	readData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		t.Fatalf("decode read data: %v", err)
	}
	if !bytes.Equal(readData, content) {
		t.Errorf("read data = %q, want %q", string(readData), string(content))
	}
}

func TestRead_PartialRead(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "data.txt", 0o644, 0, 0)
	content := []byte("0123456789ABCDEF")
	fx.writeContent(t, fileHandle, content)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Read 5 bytes starting at offset 3
	args := encodeReadArgs(&anonymousStateid, 3, 5)
	result := fx.handler.handleRead(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("READ partial status = %d, want NFS4_OK", result.Status)
	}

	reader := bytes.NewReader(result.Data)
	_, _ = xdr.DecodeUint32(reader) // status

	eof, _ := xdr.DecodeUint32(reader)
	if eof != 0 {
		t.Errorf("eof = %d, want 0 (partial read)", eof)
	}

	readData, _ := xdr.DecodeOpaque(reader)
	expected := content[3:8] // "34567"
	if !bytes.Equal(readData, expected) {
		t.Errorf("read data = %q, want %q", string(readData), string(expected))
	}
}

func TestRead_BeyondEOF(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "small.txt", 0o644, 0, 0)
	content := []byte("tiny")
	fx.writeContent(t, fileHandle, content)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Read at offset beyond file size
	args := encodeReadArgs(&anonymousStateid, 1000, 100)
	result := fx.handler.handleRead(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("READ beyond EOF status = %d, want NFS4_OK", result.Status)
	}

	reader := bytes.NewReader(result.Data)
	_, _ = xdr.DecodeUint32(reader) // status

	eof, _ := xdr.DecodeUint32(reader)
	if eof != 1 {
		t.Errorf("eof = %d, want 1", eof)
	}

	readData, _ := xdr.DecodeOpaque(reader)
	if len(readData) != 0 {
		t.Errorf("read data length = %d, want 0 (beyond EOF)", len(readData))
	}
}

func TestRead_EmptyFile(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "empty.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	args := encodeReadArgs(&anonymousStateid, 0, 1024)
	result := fx.handler.handleRead(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("READ empty file status = %d, want NFS4_OK", result.Status)
	}

	reader := bytes.NewReader(result.Data)
	_, _ = xdr.DecodeUint32(reader) // status

	eof, _ := xdr.DecodeUint32(reader)
	if eof != 1 {
		t.Errorf("eof = %d, want 1 (empty file)", eof)
	}

	readData, _ := xdr.DecodeOpaque(reader)
	if len(readData) != 0 {
		t.Errorf("read data length = %d, want 0", len(readData))
	}
}

func TestRead_Directory_ReturnsISDIR(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	dirHandle := fx.createDirectory(t, fx.rootHandle, "adir")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(dirHandle))
	copy(ctx.CurrentFH, dirHandle)

	args := encodeReadArgs(&anonymousStateid, 0, 1024)
	result := fx.handler.handleRead(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ISDIR {
		t.Errorf("READ directory status = %d, want NFS4ERR_ISDIR (%d)",
			result.Status, types.NFS4ERR_ISDIR)
	}
}

func TestRead_PseudoFS_ReturnsISDIR(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = pfs.GetRootHandle()

	args := encodeReadArgs(&anonymousStateid, 0, 1024)
	result := h.handleRead(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ISDIR {
		t.Errorf("READ on pseudo-fs status = %d, want NFS4ERR_ISDIR (%d)",
			result.Status, types.NFS4ERR_ISDIR)
	}
}

func TestRead_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = nil

	args := encodeReadArgs(&anonymousStateid, 0, 1024)
	result := h.handleRead(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("READ without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

// ============================================================================
// WRITE Tests
// ============================================================================

func TestWrite_Success(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "writeme.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0) // root can write
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	content := []byte("Hello from NFSv4 WRITE!")
	args := encodeWriteArgs(&anonymousStateid, 0, types.UNSTABLE4, content)
	result := fx.handler.handleWrite(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("WRITE status = %d, want NFS4_OK", result.Status)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	// count
	count, _ := xdr.DecodeUint32(reader)
	if count != uint32(len(content)) {
		t.Errorf("write count = %d, want %d", count, len(content))
	}

	// committed
	committed, _ := xdr.DecodeUint32(reader)
	if committed != types.UNSTABLE4 {
		t.Errorf("committed = %d, want UNSTABLE4 (%d)", committed, types.UNSTABLE4)
	}

	// writeverf (8 bytes fixed)
	verf := make([]byte, 8)
	n, err := reader.Read(verf)
	if err != nil || n != 8 {
		t.Fatalf("read writeverf: %v (n=%d)", err, n)
	}

	// Verify verifier matches serverBootVerifier
	if !bytes.Equal(verf, serverBootVerifier[:]) {
		t.Error("writeverf should match server boot verifier")
	}

	// Verify content was written by reading it back
	authCtx := newTestAuthCtx(0, 0)
	file, _ := fx.metaSvc.GetFile(authCtx.Context, fileHandle)
	if file.Size != uint64(len(content)) {
		t.Errorf("file size = %d, want %d", file.Size, len(content))
	}

	readBuf := make([]byte, len(content))
	n2, err := fx.payloadSvc.ReadAt(context.Background(), file.PayloadID, readBuf, 0)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(readBuf[:n2], content) {
		t.Errorf("read back = %q, want %q", string(readBuf[:n2]), string(content))
	}
}

func TestWrite_AtOffset(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "offset.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	content := []byte("World!")
	args := encodeWriteArgs(&anonymousStateid, 5, types.UNSTABLE4, content)
	result := fx.handler.handleWrite(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("WRITE at offset status = %d, want NFS4_OK", result.Status)
	}

	// Verify file size is offset + length
	authCtx := newTestAuthCtx(0, 0)
	file, _ := fx.metaSvc.GetFile(authCtx.Context, fileHandle)
	expectedSize := uint64(5 + len(content))
	if file.Size != expectedSize {
		t.Errorf("file size = %d, want %d", file.Size, expectedSize)
	}
}

func TestWrite_PseudoFS_ReturnsROFS(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = pfs.GetRootHandle()

	args := encodeWriteArgs(&anonymousStateid, 0, types.UNSTABLE4, []byte("data"))
	result := h.handleWrite(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ROFS {
		t.Errorf("WRITE on pseudo-fs status = %d, want NFS4ERR_ROFS (%d)",
			result.Status, types.NFS4ERR_ROFS)
	}
}

func TestWrite_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = nil

	args := encodeWriteArgs(&anonymousStateid, 0, 0, []byte("data"))
	result := h.handleWrite(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("WRITE without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

// ============================================================================
// COMMIT Tests
// ============================================================================

func TestCommit_Success(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "commit.txt", 0o644, 0, 0)

	// Write some data first
	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	writeArgs := encodeWriteArgs(&anonymousStateid, 0, types.UNSTABLE4, []byte("data to commit"))
	writeResult := fx.handler.handleWrite(ctx, bytes.NewReader(writeArgs))
	if writeResult.Status != types.NFS4_OK {
		t.Fatalf("WRITE for COMMIT setup: status = %d", writeResult.Status)
	}

	// COMMIT
	commitArgs := encodeCommitArgs(0, 0) // offset=0, count=0 = all
	commitResult := fx.handler.handleCommit(ctx, bytes.NewReader(commitArgs))

	if commitResult.Status != types.NFS4_OK {
		t.Fatalf("COMMIT status = %d, want NFS4_OK", commitResult.Status)
	}

	// Parse response
	reader := bytes.NewReader(commitResult.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	// writeverf (8 bytes fixed)
	verf := make([]byte, 8)
	n, err := reader.Read(verf)
	if err != nil || n != 8 {
		t.Fatalf("read writeverf: %v (n=%d)", err, n)
	}

	// Verifier should match WRITE verifier
	if !bytes.Equal(verf, serverBootVerifier[:]) {
		t.Error("COMMIT writeverf should match server boot verifier")
	}
}

func TestCommit_EmptyFile_Success(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "empty.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	args := encodeCommitArgs(0, 0)
	result := fx.handler.handleCommit(ctx, bytes.NewReader(args))

	// COMMIT on empty file should succeed
	if result.Status != types.NFS4_OK {
		t.Errorf("COMMIT on empty file status = %d, want NFS4_OK", result.Status)
	}
}

func TestCommit_PseudoFS_ReturnsROFS(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = pfs.GetRootHandle()

	args := encodeCommitArgs(0, 0)
	result := h.handleCommit(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ROFS {
		t.Errorf("COMMIT on pseudo-fs status = %d, want NFS4ERR_ROFS (%d)",
			result.Status, types.NFS4ERR_ROFS)
	}
}

func TestCommit_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = nil

	args := encodeCommitArgs(0, 0)
	result := h.handleCommit(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("COMMIT without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

// ============================================================================
// Write-Read Roundtrip Tests
// ============================================================================

func TestWriteReadRoundtrip(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	// OPEN (create file)
	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	openArgs := encodeOpenArgs(
		1, types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE,
		0x12345678, []byte("test-owner"),
		types.OPEN4_CREATE, types.UNCHECKED4, types.CLAIM_NULL, "roundtrip.txt",
	)
	openResult := fx.handler.handleOpen(ctx, bytes.NewReader(openArgs))
	if openResult.Status != types.NFS4_OK {
		t.Fatalf("OPEN status = %d", openResult.Status)
	}

	// WRITE
	content := []byte("NFSv4 roundtrip test data -- longer content to verify chunked behavior")
	writeArgs := encodeWriteArgs(&anonymousStateid, 0, types.UNSTABLE4, content)
	writeResult := fx.handler.handleWrite(ctx, bytes.NewReader(writeArgs))
	if writeResult.Status != types.NFS4_OK {
		t.Fatalf("WRITE status = %d", writeResult.Status)
	}

	// COMMIT
	commitArgs := encodeCommitArgs(0, 0)
	commitResult := fx.handler.handleCommit(ctx, bytes.NewReader(commitArgs))
	if commitResult.Status != types.NFS4_OK {
		t.Fatalf("COMMIT status = %d", commitResult.Status)
	}

	// READ back
	readArgs := encodeReadArgs(&anonymousStateid, 0, uint32(len(content)+100))
	readResult := fx.handler.handleRead(ctx, bytes.NewReader(readArgs))
	if readResult.Status != types.NFS4_OK {
		t.Fatalf("READ status = %d", readResult.Status)
	}

	// Verify read data
	reader := bytes.NewReader(readResult.Data)
	_, _ = xdr.DecodeUint32(reader) // status
	eof, _ := xdr.DecodeUint32(reader)
	readData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		t.Fatalf("decode read data: %v", err)
	}

	if !bytes.Equal(readData, content) {
		t.Errorf("roundtrip data mismatch:\n  got:  %q\n  want: %q", string(readData), string(content))
	}
	if eof != 1 {
		t.Errorf("eof = %d, want 1 (read all data)", eof)
	}
}

func TestWriteReadRoundtrip_MultipleWrites(t *testing.T) {
	fx := newIOTestFixture(t, "/export")

	fileHandle := fx.createRegularFile(t, fx.rootHandle, "multi.txt", 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Write "Hello" at offset 0
	args1 := encodeWriteArgs(&anonymousStateid, 0, types.UNSTABLE4, []byte("Hello"))
	res1 := fx.handler.handleWrite(ctx, bytes.NewReader(args1))
	if res1.Status != types.NFS4_OK {
		t.Fatalf("WRITE 1 status = %d", res1.Status)
	}

	// Write " World" at offset 5
	args2 := encodeWriteArgs(&anonymousStateid, 5, types.UNSTABLE4, []byte(" World"))
	res2 := fx.handler.handleWrite(ctx, bytes.NewReader(args2))
	if res2.Status != types.NFS4_OK {
		t.Fatalf("WRITE 2 status = %d", res2.Status)
	}

	// READ full content
	readArgs := encodeReadArgs(&anonymousStateid, 0, 1024)
	readResult := fx.handler.handleRead(ctx, bytes.NewReader(readArgs))
	if readResult.Status != types.NFS4_OK {
		t.Fatalf("READ status = %d", readResult.Status)
	}

	reader := bytes.NewReader(readResult.Data)
	_, _ = xdr.DecodeUint32(reader) // status
	_, _ = xdr.DecodeUint32(reader) // eof
	readData, _ := xdr.DecodeOpaque(reader)

	expected := "Hello World"
	if string(readData) != expected {
		t.Errorf("multi-write data = %q, want %q", string(readData), expected)
	}
}

// ============================================================================
// Stateid4 Tests
// ============================================================================

func TestStateid4_IsSpecial_AllZeros(t *testing.T) {
	sid := types.Stateid4{Seqid: 0}
	// Other is default zero
	if !sid.IsSpecialStateid() {
		t.Error("all-zeros stateid should be special")
	}
}

func TestStateid4_IsSpecial_AllOnes(t *testing.T) {
	// RFC 7530 Section 9.1.4.3: READ bypass stateid has seqid=0xFFFFFFFF, other=all-ones
	sid := types.Stateid4{Seqid: 0xFFFFFFFF}
	for i := range sid.Other {
		sid.Other[i] = 0xFF
	}
	if !sid.IsSpecialStateid() {
		t.Error("all-ones stateid should be special")
	}
}

func TestStateid4_NotSpecial_NonzeroSeqid(t *testing.T) {
	sid := types.Stateid4{Seqid: 1}
	if sid.IsSpecialStateid() {
		t.Error("non-zero seqid stateid should NOT be special")
	}
}

func TestStateid4_NotSpecial_MixedOther(t *testing.T) {
	sid := types.Stateid4{Seqid: 0}
	sid.Other[0] = 0x42 // not all zeros, not all ones
	if sid.IsSpecialStateid() {
		t.Error("mixed other stateid should NOT be special")
	}
}

func TestStateid4_EncodeDecode(t *testing.T) {
	original := types.Stateid4{Seqid: 42}
	for i := range original.Other {
		original.Other[i] = byte(i + 1)
	}

	var buf bytes.Buffer
	types.EncodeStateid4(&buf, &original)

	decoded, err := types.DecodeStateid4(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded.Seqid != original.Seqid {
		t.Errorf("seqid = %d, want %d", decoded.Seqid, original.Seqid)
	}
	if decoded.Other != original.Other {
		t.Errorf("other = %v, want %v", decoded.Other, original.Other)
	}
}
