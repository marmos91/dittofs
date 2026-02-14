package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// CREATE Test Helpers
// ============================================================================

// encodeCreateDirArgs encodes CREATE4args for creating a directory.
func encodeCreateDirArgs(name string) []byte {
	var buf bytes.Buffer
	// objtype = NF4DIR
	_ = xdr.WriteUint32(&buf, types.NF4DIR)
	// No type-specific data for NF4DIR
	// objname
	_ = xdr.WriteXDRString(&buf, name)
	// createattrs: empty bitmap + empty opaque
	_ = xdr.WriteUint32(&buf, 0) // bitmap length = 0
	_ = xdr.WriteUint32(&buf, 0) // attr data length = 0
	return buf.Bytes()
}

// encodeCreateSymlinkArgs encodes CREATE4args for creating a symlink.
func encodeCreateSymlinkArgs(name, target string) []byte {
	var buf bytes.Buffer
	// objtype = NF4LNK
	_ = xdr.WriteUint32(&buf, types.NF4LNK)
	// linkdata (symlink target)
	_ = xdr.WriteXDRString(&buf, target)
	// objname
	_ = xdr.WriteXDRString(&buf, name)
	// createattrs: empty bitmap + empty opaque
	_ = xdr.WriteUint32(&buf, 0) // bitmap length = 0
	_ = xdr.WriteUint32(&buf, 0) // attr data length = 0
	return buf.Bytes()
}

// encodeCreateBlockDevArgs encodes CREATE4args for a block device (unsupported).
func encodeCreateBlockDevArgs(name string) []byte {
	var buf bytes.Buffer
	// objtype = NF4BLK
	_ = xdr.WriteUint32(&buf, types.NF4BLK)
	// specdata1, specdata2
	_ = xdr.WriteUint32(&buf, 8) // major
	_ = xdr.WriteUint32(&buf, 0) // minor
	// objname
	_ = xdr.WriteXDRString(&buf, name)
	// createattrs: empty bitmap + empty opaque
	_ = xdr.WriteUint32(&buf, 0)
	_ = xdr.WriteUint32(&buf, 0)
	return buf.Bytes()
}

// encodeCreateRegularFileArgs encodes CREATE4args for a regular file (invalid for CREATE).
func encodeCreateRegularFileArgs(name string) []byte {
	var buf bytes.Buffer
	// objtype = NF4REG (not valid for CREATE)
	_ = xdr.WriteUint32(&buf, types.NF4REG)
	// objname
	_ = xdr.WriteXDRString(&buf, name)
	// createattrs: empty bitmap + empty opaque
	_ = xdr.WriteUint32(&buf, 0)
	_ = xdr.WriteUint32(&buf, 0)
	return buf.Bytes()
}

// encodeRemoveArgs encodes REMOVE4args.
func encodeRemoveArgs(target string) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteXDRString(&buf, target)
	return buf.Bytes()
}

// newTestAuthCtx creates a test AuthContext with the given UID/GID.
func newTestAuthCtx(uid, gid uint32) *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:    context.Background(),
		ClientAddr: "192.168.1.100:9999",
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: &uid,
			GID: &gid,
		},
	}
}

// ============================================================================
// CREATE Tests
// ============================================================================

func TestCreate_Directory_Success(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0) // root user
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeCreateDirArgs("newdir")
	result := fx.handler.handleCreate(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("CREATE dir status = %d, want NFS4_OK", result.Status)
	}

	if result.OpCode != types.OP_CREATE {
		t.Errorf("CREATE opcode = %d, want OP_CREATE (%d)", result.OpCode, types.OP_CREATE)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)

	// Status
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	// change_info4: atomic (bool), changeid_before (uint64), changeid_after (uint64)
	atomic, _ := xdr.DecodeUint32(reader)
	if atomic != 1 {
		t.Errorf("change_info atomic = %d, want 1", atomic)
	}
	before, _ := xdr.DecodeUint64(reader)
	after, _ := xdr.DecodeUint64(reader)
	if before == 0 {
		t.Error("changeid_before should not be 0")
	}
	if after == 0 {
		t.Error("changeid_after should not be 0")
	}

	// attrset bitmap (should be empty = length 0)
	bitmapLen, _ := xdr.DecodeUint32(reader)
	if bitmapLen != 0 {
		t.Errorf("attrset bitmap len = %d, want 0", bitmapLen)
	}

	// CurrentFH should now point to the new directory
	if ctx.CurrentFH == nil {
		t.Fatal("CurrentFH should be set to new directory handle")
	}

	// Verify the new entry exists via metadata service
	authCtx := newTestAuthCtx(0, 0)
	child, err := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "newdir")
	if err != nil {
		t.Fatalf("Lookup after CREATE: %v", err)
	}
	if child.Type != metadata.FileTypeDirectory {
		t.Errorf("created entry type = %v, want directory", child.Type)
	}
}

func TestCreate_Symlink_Success(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeCreateSymlinkArgs("mylink", "/tmp/target")
	result := fx.handler.handleCreate(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("CREATE symlink status = %d, want NFS4_OK", result.Status)
	}

	// Verify the symlink exists and has the right target
	authCtx := newTestAuthCtx(0, 0)
	child, err := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "mylink")
	if err != nil {
		t.Fatalf("Lookup after CREATE symlink: %v", err)
	}
	if child.Type != metadata.FileTypeSymlink {
		t.Errorf("created entry type = %v, want symlink", child.Type)
	}
	if child.LinkTarget != "/tmp/target" {
		t.Errorf("symlink target = %q, want %q", child.LinkTarget, "/tmp/target")
	}

	// CurrentFH should point to the new symlink
	if ctx.CurrentFH == nil {
		t.Fatal("CurrentFH should be set to new symlink handle")
	}
}

func TestCreate_PseudoFS_ReturnsROFS(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = pfs.GetRootHandle()

	args := encodeCreateDirArgs("forbidden")
	result := h.handleCreate(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ROFS {
		t.Errorf("CREATE on pseudo-fs status = %d, want NFS4ERR_ROFS (%d)",
			result.Status, types.NFS4ERR_ROFS)
	}
}

func TestCreate_InvalidName_NullByte(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	// Name with null byte should produce BADCHAR
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NF4DIR) // objtype
	// We need to encode the name with a null byte embedded.
	// WriteXDRString won't include \x00 in XDR string length, so encode manually.
	nameBytes := []byte("bad\x00name")
	_ = xdr.WriteUint32(&buf, uint32(len(nameBytes)))
	buf.Write(nameBytes)
	// Pad to 4-byte boundary
	pad := (4 - len(nameBytes)%4) % 4
	buf.Write(make([]byte, pad))
	// createattrs: empty bitmap + empty opaque
	_ = xdr.WriteUint32(&buf, 0)
	_ = xdr.WriteUint32(&buf, 0)

	result := fx.handler.handleCreate(ctx, bytes.NewReader(buf.Bytes()))

	if result.Status != types.NFS4ERR_BADCHAR {
		t.Errorf("CREATE with null byte status = %d, want NFS4ERR_BADCHAR (%d)",
			result.Status, types.NFS4ERR_BADCHAR)
	}
}

func TestCreate_InvalidName_Slash(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeCreateDirArgs("bad/name")
	result := fx.handler.handleCreate(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_BADNAME {
		t.Errorf("CREATE with slash status = %d, want NFS4ERR_BADNAME (%d)",
			result.Status, types.NFS4ERR_BADNAME)
	}
}

func TestCreate_UnsupportedType_BlockDevice(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeCreateBlockDevArgs("myblkdev")
	result := fx.handler.handleCreate(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOTSUPP {
		t.Errorf("CREATE block device status = %d, want NFS4ERR_NOTSUPP (%d)",
			result.Status, types.NFS4ERR_NOTSUPP)
	}
}

func TestCreate_RegularFile_ReturnsBadType(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeCreateRegularFileArgs("myfile.txt")
	result := fx.handler.handleCreate(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_BADTYPE {
		t.Errorf("CREATE regular file status = %d, want NFS4ERR_BADTYPE (%d)",
			result.Status, types.NFS4ERR_BADTYPE)
	}
}

func TestCreate_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = nil

	args := encodeCreateDirArgs("test")
	result := h.handleCreate(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("CREATE without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestCreate_Directory_SetsCurrentFH(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	originalFH := make([]byte, len(ctx.CurrentFH))
	copy(originalFH, ctx.CurrentFH)

	args := encodeCreateDirArgs("subdir")
	result := fx.handler.handleCreate(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("CREATE status = %d, want NFS4_OK", result.Status)
	}

	// CurrentFH should have changed (different from parent)
	if bytes.Equal(ctx.CurrentFH, originalFH) {
		t.Error("CurrentFH should have changed to the new directory's handle")
	}
}

// ============================================================================
// REMOVE Tests
// ============================================================================

func TestRemove_File_Success(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create a file to remove
	fx.createTestFile(t, fx.rootHandle, "removeme.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeRemoveArgs("removeme.txt")
	result := fx.handler.handleRemove(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("REMOVE file status = %d, want NFS4_OK", result.Status)
	}

	if result.OpCode != types.OP_REMOVE {
		t.Errorf("REMOVE opcode = %d, want OP_REMOVE (%d)", result.OpCode, types.OP_REMOVE)
	}

	// Parse response
	reader := bytes.NewReader(result.Data)

	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	// change_info4
	atomic, _ := xdr.DecodeUint32(reader)
	if atomic != 1 {
		t.Errorf("change_info atomic = %d, want 1", atomic)
	}
	before, _ := xdr.DecodeUint64(reader)
	after, _ := xdr.DecodeUint64(reader)
	if before == 0 {
		t.Error("changeid_before should not be 0")
	}
	if after == 0 {
		t.Error("changeid_after should not be 0")
	}

	// Verify file no longer exists
	authCtx := newTestAuthCtx(0, 0)
	_, err := fx.metaSvc.Lookup(authCtx, fx.rootHandle, "removeme.txt")
	if err == nil {
		t.Error("file should not exist after REMOVE")
	}
}

func TestRemove_Directory_Success(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Create an empty directory using metaSvc directly
	dirAuthCtx := newTestAuthCtx(0, 0)
	dirAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}
	_, err := fx.metaSvc.CreateDirectory(dirAuthCtx, fx.rootHandle, "emptydir", dirAttr)
	if err != nil {
		t.Fatalf("create test directory: %v", err)
	}

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeRemoveArgs("emptydir")
	result := fx.handler.handleRemove(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("REMOVE directory status = %d, want NFS4_OK", result.Status)
	}

	// Verify directory is gone
	_, err = fx.metaSvc.Lookup(dirAuthCtx, fx.rootHandle, "emptydir")
	if err == nil {
		t.Error("directory should not exist after REMOVE")
	}
}

func TestRemove_PseudoFS_ReturnsROFS(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = pfs.GetRootHandle()

	args := encodeRemoveArgs("export")
	result := h.handleRemove(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ROFS {
		t.Errorf("REMOVE on pseudo-fs status = %d, want NFS4ERR_ROFS (%d)",
			result.Status, types.NFS4ERR_ROFS)
	}
}

func TestRemove_Nonexistent_ReturnsNOENT(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	args := encodeRemoveArgs("nosuchfile")
	result := fx.handler.handleRemove(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOENT {
		t.Errorf("REMOVE nonexistent status = %d, want NFS4ERR_NOENT (%d)",
			result.Status, types.NFS4ERR_NOENT)
	}
}

func TestRemove_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = nil

	args := encodeRemoveArgs("test")
	result := h.handleRemove(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("REMOVE without FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestRemove_InvalidName(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)

	// Name with slash should be BADNAME
	args := encodeRemoveArgs("bad/name")
	result := fx.handler.handleRemove(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_BADNAME {
		t.Errorf("REMOVE with slash status = %d, want NFS4ERR_BADNAME (%d)",
			result.Status, types.NFS4ERR_BADNAME)
	}
}
