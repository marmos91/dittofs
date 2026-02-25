package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// VERIFY / NVERIFY Test Helpers
// ============================================================================

// encodeVerifyArgs builds the XDR args for VERIFY/NVERIFY: fattr4 (bitmap4 + opaque attr_vals).
func encodeVerifyArgs(t *testing.T, bitmap []uint32, attrData []byte) []byte {
	t.Helper()
	var buf bytes.Buffer

	// Encode bitmap4
	if err := attrs.EncodeBitmap4(&buf, bitmap); err != nil {
		t.Fatalf("encode bitmap: %v", err)
	}

	// Encode opaque attr_vals
	if err := xdr.WriteXDROpaque(&buf, attrData); err != nil {
		t.Fatalf("encode attr data: %v", err)
	}

	return buf.Bytes()
}

// getServerAttrVals encodes the server's current attributes for a file handle
// and returns only the opaque attr_vals portion (for use as VERIFY/NVERIFY input).
func getServerAttrVals(t *testing.T, fx *realFSTestFixture, handle metadata.FileHandle, bitmap []uint32) []byte {
	t.Helper()

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(handle))
	copy(ctx.CurrentFH, handle)

	// Use GETATTR to get the server's current attributes
	result := fx.handler.getAttrRealFS(ctx, bitmap)
	if result.Status != types.NFS4_OK {
		t.Fatalf("GETATTR for server attrs failed: status=%d", result.Status)
	}

	// Parse response: status + bitmap + opaque(attr_vals)
	reader := bytes.NewReader(result.Data)

	// Status
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Fatalf("GETATTR encoded status=%d", status)
	}

	// Skip bitmap
	if _, err := attrs.DecodeBitmap4(reader); err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}

	// Read opaque attr_vals
	attrData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		t.Fatalf("decode attr data: %v", err)
	}

	return attrData
}

// ============================================================================
// VERIFY Tests
// ============================================================================

func TestHandleVerify_Match(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	// Get the server's current attribute values for SIZE + MODE
	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)
	attrs.SetBit(&bitmap, attrs.FATTR4_MODE)
	serverAttrVals := getServerAttrVals(t, fx, fileHandle, bitmap)

	// Provide the same values to VERIFY -- should match
	args := encodeVerifyArgs(t, bitmap, serverAttrVals)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	result := fx.handler.handleVerify(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Errorf("VERIFY match status = %d, want NFS4_OK (%d)", result.Status, types.NFS4_OK)
	}
}

func TestHandleVerify_Mismatch(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	// Encode a wrong SIZE value (99999 instead of the actual 1024)
	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)

	var wrongAttrData bytes.Buffer
	_ = xdr.WriteUint64(&wrongAttrData, 99999) // wrong size

	args := encodeVerifyArgs(t, bitmap, wrongAttrData.Bytes())

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	result := fx.handler.handleVerify(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOT_SAME {
		t.Errorf("VERIFY mismatch status = %d, want NFS4ERR_NOT_SAME (%d)",
			result.Status, types.NFS4ERR_NOT_SAME)
	}
}

func TestHandleVerify_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		// CurrentFH is nil
	}

	// Args don't matter since we should fail before reading them
	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_TYPE)
	var attrData bytes.Buffer
	_ = xdr.WriteUint32(&attrData, types.NF4REG)
	args := encodeVerifyArgs(t, bitmap, attrData.Bytes())

	result := h.handleVerify(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("VERIFY no FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestHandleVerify_StaleHandle(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	// Use a stale handle (pseudo-fs handle that doesn't exist)
	staleHandle := []byte("pseudofs:/nonexistent/path/stale")

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_TYPE)
	var attrData bytes.Buffer
	_ = xdr.WriteUint32(&attrData, types.NF4DIR)
	args := encodeVerifyArgs(t, bitmap, attrData.Bytes())

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = staleHandle

	result := fx.handler.handleVerify(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_STALE {
		t.Errorf("VERIFY stale handle status = %d, want NFS4ERR_STALE (%d)",
			result.Status, types.NFS4ERR_STALE)
	}
}

func TestHandleVerify_PseudoFS(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	rootHandle := pfs.GetRootHandle()

	// Get the pseudo-fs root attributes for TYPE
	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_TYPE)

	// Pseudo-fs root is always NF4DIR
	var attrData bytes.Buffer
	_ = xdr.WriteUint32(&attrData, types.NF4DIR)

	args := encodeVerifyArgs(t, bitmap, attrData.Bytes())

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  make([]byte, len(rootHandle)),
	}
	copy(ctx.CurrentFH, rootHandle)

	result := h.handleVerify(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Errorf("VERIFY pseudo-fs status = %d, want NFS4_OK (%d)",
			result.Status, types.NFS4_OK)
	}
}

func TestHandleVerify_MultipleAttrs(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "multi.txt", metadata.FileTypeRegular, 0o755, 0, 0)

	// Request TYPE + SIZE + MODE
	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_TYPE)
	attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)
	attrs.SetBit(&bitmap, attrs.FATTR4_MODE)
	serverAttrVals := getServerAttrVals(t, fx, fileHandle, bitmap)

	args := encodeVerifyArgs(t, bitmap, serverAttrVals)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	result := fx.handler.handleVerify(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Errorf("VERIFY multi-attr match status = %d, want NFS4_OK", result.Status)
	}
}

// ============================================================================
// NVERIFY Tests
// ============================================================================

func TestHandleNVerify_Match(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	// Get the server's current values -- they will match
	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)
	serverAttrVals := getServerAttrVals(t, fx, fileHandle, bitmap)

	args := encodeVerifyArgs(t, bitmap, serverAttrVals)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	result := fx.handler.handleNVerify(ctx, bytes.NewReader(args))

	// NVERIFY returns NFS4ERR_SAME when attributes match
	if result.Status != types.NFS4ERR_SAME {
		t.Errorf("NVERIFY match status = %d, want NFS4ERR_SAME (%d)",
			result.Status, types.NFS4ERR_SAME)
	}
}

func TestHandleNVerify_Mismatch(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	// Encode a wrong SIZE value
	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)

	var wrongAttrData bytes.Buffer
	_ = xdr.WriteUint64(&wrongAttrData, 99999) // wrong size

	args := encodeVerifyArgs(t, bitmap, wrongAttrData.Bytes())

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	result := fx.handler.handleNVerify(ctx, bytes.NewReader(args))

	// NVERIFY returns NFS4_OK when attributes do NOT match
	if result.Status != types.NFS4_OK {
		t.Errorf("NVERIFY mismatch status = %d, want NFS4_OK (%d)",
			result.Status, types.NFS4_OK)
	}
}

func TestHandleNVerify_NoCurrentFH(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		// CurrentFH is nil
	}

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_TYPE)
	var attrData bytes.Buffer
	_ = xdr.WriteUint32(&attrData, types.NF4REG)
	args := encodeVerifyArgs(t, bitmap, attrData.Bytes())

	result := h.handleNVerify(ctx, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("NVERIFY no FH status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

// ============================================================================
// Compound Sequence Tests
// ============================================================================

func TestVerifyNVerify_CompoundSequence(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "guarded.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	// Get current server attrs for SIZE
	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)
	serverAttrVals := getServerAttrVals(t, fx, fileHandle, bitmap)

	t.Run("VERIFY_match_continues_compound", func(t *testing.T) {
		// PUTFH + VERIFY(matching) + GETATTR should all succeed
		verifyArgs := encodeVerifyArgs(t, bitmap, serverAttrVals)

		data := encodeCompoundWithOps("", 0, []encodedOp{
			encodePutFH([]byte(fileHandle)),
			{opCode: types.OP_VERIFY, args: verifyArgs},
			encodeGetAttr(attrs.FATTR4_TYPE),
		})

		ctx := newRealFSContext(0, 0)
		resp, err := fx.handler.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		// Parse raw response
		reader := bytes.NewReader(resp)
		overallStatus, _ := xdr.DecodeUint32(reader)
		if overallStatus != types.NFS4_OK {
			t.Fatalf("compound status = %d, want NFS4_OK", overallStatus)
		}

		// Skip tag
		_, _ = xdr.DecodeOpaque(reader)
		numResults, _ := xdr.DecodeUint32(reader)

		if numResults != 3 {
			t.Fatalf("numResults = %d, want 3 (PUTFH + VERIFY + GETATTR)", numResults)
		}

		// Check each result
		for i := uint32(0); i < numResults; i++ {
			_, _ = xdr.DecodeUint32(reader) // opcode
			st, _ := xdr.DecodeUint32(reader)
			if st != types.NFS4_OK {
				t.Errorf("result[%d] status = %d, want NFS4_OK", i, st)
			}
			// Skip extra data for GETATTR
			if i == 2 {
				// bitmap + opaque
				_, _ = attrs.DecodeBitmap4(reader)
				_, _ = xdr.DecodeOpaque(reader)
			}
		}
	})

	t.Run("VERIFY_mismatch_stops_compound", func(t *testing.T) {
		// PUTFH + VERIFY(wrong size) + GETATTR
		// GETATTR should NOT execute because VERIFY fails

		var wrongAttrData bytes.Buffer
		_ = xdr.WriteUint64(&wrongAttrData, 99999)
		verifyArgs := encodeVerifyArgs(t, bitmap, wrongAttrData.Bytes())

		data := encodeCompoundWithOps("", 0, []encodedOp{
			encodePutFH([]byte(fileHandle)),
			{opCode: types.OP_VERIFY, args: verifyArgs},
			encodeGetAttr(attrs.FATTR4_TYPE),
		})

		ctx := newRealFSContext(0, 0)
		resp, err := fx.handler.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("ProcessCompound error: %v", err)
		}

		// Parse raw response
		reader := bytes.NewReader(resp)
		overallStatus, _ := xdr.DecodeUint32(reader)

		if overallStatus != types.NFS4ERR_NOT_SAME {
			t.Fatalf("compound status = %d, want NFS4ERR_NOT_SAME (%d)",
				overallStatus, types.NFS4ERR_NOT_SAME)
		}

		_, _ = xdr.DecodeOpaque(reader) // tag
		numResults, _ := xdr.DecodeUint32(reader)

		// Only 2 results: PUTFH succeeded, VERIFY failed, GETATTR not executed
		if numResults != 2 {
			t.Fatalf("numResults = %d, want 2 (GETATTR should not execute)", numResults)
		}
	})
}
