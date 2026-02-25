package handlers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// SETATTR Test Helpers
// ============================================================================

// encodeSetAttrArgs builds the XDR args for SETATTR: stateid4 + fattr4
func encodeSetAttrArgs(t *testing.T, stateid *types.Stateid4, bitmap []uint32, attrData []byte) []byte {
	t.Helper()
	var buf bytes.Buffer

	// Encode stateid4 (16 bytes)
	types.EncodeStateid4(&buf, stateid)

	// Encode fattr4: bitmap + opaque attr_vals
	if err := attrs.EncodeBitmap4(&buf, bitmap); err != nil {
		t.Fatalf("encode bitmap: %v", err)
	}
	if err := xdr.WriteXDROpaque(&buf, attrData); err != nil {
		t.Fatalf("encode attr data: %v", err)
	}

	return buf.Bytes()
}

// specialStateid returns an all-zeros special stateid (anonymous).
func specialStateid() *types.Stateid4 {
	return &types.Stateid4{Seqid: 0}
}

// ============================================================================
// SETATTR Tests
// ============================================================================

func TestHandleSetAttr_Mode(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o755, 0, 0)

	ctx := newRealFSContext(0, 0) // root user
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Build SETATTR args: set mode to 0644
	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 0o644)

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_MODE)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SETATTR mode status = %d, want NFS4_OK", result.Status)
	}

	// Verify mode was changed
	file, err := fx.store.GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	if file.Mode&0o7777 != 0o644 {
		t.Errorf("mode = 0%o, want 0644", file.Mode&0o7777)
	}

	// Verify attrsset bitmap in response
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader) // status
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}
	retBitmap, err := attrs.DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode attrsset bitmap: %v", err)
	}
	if !attrs.IsBitSet(retBitmap, attrs.FATTR4_MODE) {
		t.Error("attrsset bitmap should have MODE bit set")
	}
}

func TestHandleSetAttr_Size(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Truncate to 0
	var attrVals bytes.Buffer
	_ = xdr.WriteUint64(&attrVals, 0)

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SETATTR size status = %d, want NFS4_OK", result.Status)
	}

	file, err := fx.store.GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	if file.Size != 0 {
		t.Errorf("size = %d, want 0", file.Size)
	}
}

func TestHandleSetAttr_Owner(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0) // root can chown
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	var attrVals bytes.Buffer
	_ = xdr.WriteXDRString(&attrVals, "1000@localdomain")

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_OWNER)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SETATTR owner status = %d, want NFS4_OK", result.Status)
	}

	file, err := fx.store.GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	if file.UID != 1000 {
		t.Errorf("UID = %d, want 1000", file.UID)
	}
}

func TestHandleSetAttr_Group(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0) // root can chgrp
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	var attrVals bytes.Buffer
	_ = xdr.WriteXDRString(&attrVals, "1000@localdomain")

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_OWNER_GROUP)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SETATTR group status = %d, want NFS4_OK", result.Status)
	}

	file, err := fx.store.GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	if file.GID != 1000 {
		t.Errorf("GID = %d, want 1000", file.GID)
	}
}

func TestHandleSetAttr_TimeServerTime(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	// Record time before the operation
	timeBefore := time.Now().Add(-time.Second)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Set atime and mtime to server time
	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, attrs.SET_TO_SERVER_TIME4) // atime
	_ = xdr.WriteUint32(&attrVals, attrs.SET_TO_SERVER_TIME4) // mtime

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_TIME_ACCESS_SET)
	attrs.SetBit(&bitmap, attrs.FATTR4_TIME_MODIFY_SET)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SETATTR server time status = %d, want NFS4_OK", result.Status)
	}

	file, err := fx.store.GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("get file: %v", err)
	}

	// Timestamps should be set to approximately "now"
	if file.Atime.Before(timeBefore) {
		t.Errorf("atime %v should be after %v", file.Atime, timeBefore)
	}
	if file.Mtime.Before(timeBefore) {
		t.Errorf("mtime %v should be after %v", file.Mtime, timeBefore)
	}
}

func TestHandleSetAttr_TimeClientTime(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	refTime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

	var attrVals bytes.Buffer
	// mtime: SET_TO_CLIENT_TIME4 + nfstime4
	_ = xdr.WriteUint32(&attrVals, attrs.SET_TO_CLIENT_TIME4)
	_ = xdr.WriteUint64(&attrVals, uint64(refTime.Unix()))
	_ = xdr.WriteUint32(&attrVals, uint32(refTime.Nanosecond()))

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_TIME_MODIFY_SET)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SETATTR client time status = %d, want NFS4_OK", result.Status)
	}

	file, err := fx.store.GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("get file: %v", err)
	}

	if !file.Mtime.Equal(refTime) {
		t.Errorf("mtime = %v, want %v", file.Mtime, refTime)
	}
}

func TestHandleSetAttr_MultipleAttrs(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o755, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Set mode + owner together (bits in ascending order: 33, 36)
	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 0o600)                 // MODE
	_ = xdr.WriteXDRString(&attrVals, "1001@localdomain") // OWNER

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_MODE)
	attrs.SetBit(&bitmap, attrs.FATTR4_OWNER)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SETATTR multiple attrs status = %d, want NFS4_OK", result.Status)
	}

	file, err := fx.store.GetFile(context.Background(), fileHandle)
	if err != nil {
		t.Fatalf("get file: %v", err)
	}

	if file.Mode&0o7777 != 0o600 {
		t.Errorf("mode = 0%o, want 0600", file.Mode&0o7777)
	}
	if file.UID != 1001 {
		t.Errorf("UID = %d, want 1001", file.UID)
	}
}

func TestHandleSetAttr_NoCurrentFH(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	// CurrentFH is nil

	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 0o644)

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_MODE)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("SETATTR no FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestHandleSetAttr_PseudoFS(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  pfs.GetRootHandle(),
	}

	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 0o644)

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_MODE)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := h.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ROFS {
		t.Errorf("SETATTR pseudo-fs status = %d, want NFS4ERR_ROFS (%d)",
			result.Status, types.NFS4ERR_ROFS)
	}
}

func TestHandleSetAttr_UnsupportedAttr(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Try to set FATTR4_TYPE (read-only, bit 1)
	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, types.NF4REG)

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_TYPE)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_ATTRNOTSUPP {
		t.Errorf("SETATTR unsupported attr status = %d, want NFS4ERR_ATTRNOTSUPP (%d)",
			result.Status, types.NFS4ERR_ATTRNOTSUPP)
	}
}

func TestHandleSetAttr_InvalidMode(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Mode > 07777
	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 0o10000)

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_MODE)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_INVAL {
		t.Errorf("SETATTR invalid mode status = %d, want NFS4ERR_INVAL (%d)",
			result.Status, types.NFS4ERR_INVAL)
	}
}

func TestHandleSetAttr_BadOwner(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	var attrVals bytes.Buffer
	_ = xdr.WriteXDRString(&attrVals, "unknown_user@localdomain")

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_OWNER)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_BADOWNER {
		t.Errorf("SETATTR bad owner status = %d, want NFS4ERR_BADOWNER (%d)",
			result.Status, types.NFS4ERR_BADOWNER)
	}
}

func TestHandleSetAttr_AttrssetBitmap(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o755, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Set mode + owner_group
	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 0o644)                // MODE
	_ = xdr.WriteXDRString(&attrVals, "500@localdomain") // OWNER_GROUP

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_MODE)
	attrs.SetBit(&bitmap, attrs.FATTR4_OWNER_GROUP)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SETATTR status = %d, want NFS4_OK", result.Status)
	}

	// Parse response to verify attrsset bitmap
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("encoded status = %d, want NFS4_OK", status)
	}

	retBitmap, err := attrs.DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode attrsset: %v", err)
	}

	// attrsset should match requested attrs
	if !attrs.IsBitSet(retBitmap, attrs.FATTR4_MODE) {
		t.Error("attrsset should have MODE bit set")
	}
	if !attrs.IsBitSet(retBitmap, attrs.FATTR4_OWNER_GROUP) {
		t.Error("attrsset should have OWNER_GROUP bit set")
	}

	// attrsset should NOT have bits we didn't request
	if attrs.IsBitSet(retBitmap, attrs.FATTR4_OWNER) {
		t.Error("attrsset should NOT have OWNER bit set (not requested)")
	}
}

func TestHandleSetAttr_AttrssetEmptyOnFailure(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	// Invalid mode -> error
	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 0o10000) // too large

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_MODE)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctx, bytes.NewReader(args))

	if result.Status == types.NFS4_OK {
		t.Fatal("SETATTR should have failed")
	}

	// Parse response to verify empty attrsset bitmap
	reader := bytes.NewReader(result.Data)
	_, _ = xdr.DecodeUint32(reader) // status

	retBitmap, err := attrs.DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode attrsset: %v", err)
	}

	// On error, attrsset should be empty (0 words)
	if len(retBitmap) > 0 {
		// Check no bits are set
		for _, word := range retBitmap {
			if word != 0 {
				t.Errorf("attrsset bitmap should be empty on error, got %v", retBitmap)
				break
			}
		}
	}
}
