package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

// ============================================================================
// Test Helpers
// ============================================================================

// newTestHandlerWithShares creates a Handler with a PseudoFS built from
// the given share paths. Uses nil registry (pseudo-fs ops don't need it).
func newTestHandlerWithShares(shares []string) *Handler {
	pfs := pseudofs.New()
	pfs.Rebuild(shares)
	return NewHandler(nil, pfs)
}

// encodedOp represents an XDR-encoded operation for building COMPOUND args.
type encodedOp struct {
	opCode uint32
	args   []byte // XDR-encoded operation arguments (may be empty)
}

// encodeCompoundWithOps builds COMPOUND4args bytes from a tag, minor version,
// and sequence of encoded operations.
func encodeCompoundWithOps(tag string, minorVersion uint32, ops []encodedOp) []byte {
	var buf bytes.Buffer

	// Write tag as XDR opaque
	_ = xdr.WriteXDROpaque(&buf, []byte(tag))

	// Write minor version
	_ = xdr.WriteUint32(&buf, minorVersion)

	// Write number of ops
	_ = xdr.WriteUint32(&buf, uint32(len(ops)))

	// Write each op: opcode + args
	for _, op := range ops {
		_ = xdr.WriteUint32(&buf, op.opCode)
		if len(op.args) > 0 {
			buf.Write(op.args)
		}
	}

	return buf.Bytes()
}

// Operation encoders

func encodePutRootFH() encodedOp {
	return encodedOp{opCode: types.OP_PUTROOTFH}
}

func encodePutPubFH() encodedOp {
	return encodedOp{opCode: types.OP_PUTPUBFH}
}

func encodePutFH(handle []byte) encodedOp {
	var buf bytes.Buffer
	_ = xdr.WriteXDROpaque(&buf, handle)
	return encodedOp{opCode: types.OP_PUTFH, args: buf.Bytes()}
}

func encodeGetFH() encodedOp {
	return encodedOp{opCode: types.OP_GETFH}
}

func encodeSaveFH() encodedOp {
	return encodedOp{opCode: types.OP_SAVEFH}
}

func encodeRestoreFH() encodedOp {
	return encodedOp{opCode: types.OP_RESTOREFH}
}

func encodeLookup(name string) encodedOp {
	var buf bytes.Buffer
	_ = xdr.WriteXDRString(&buf, name)
	return encodedOp{opCode: types.OP_LOOKUP, args: buf.Bytes()}
}

func encodeLookupP() encodedOp {
	return encodedOp{opCode: types.OP_LOOKUPP}
}

func encodeGetAttr(bits ...uint32) encodedOp {
	var bitmap []uint32
	for _, bit := range bits {
		attrs.SetBit(&bitmap, bit)
	}

	var buf bytes.Buffer
	_ = attrs.EncodeBitmap4(&buf, bitmap)
	return encodedOp{opCode: types.OP_GETATTR, args: buf.Bytes()}
}

func encodeGetAttrEmpty() encodedOp {
	var buf bytes.Buffer
	_ = attrs.EncodeBitmap4(&buf, nil) // empty bitmap
	return encodedOp{opCode: types.OP_GETATTR, args: buf.Bytes()}
}

func encodeReadDir(cookie uint64, maxcount uint32, bits ...uint32) encodedOp {
	var bitmap []uint32
	for _, bit := range bits {
		attrs.SetBit(&bitmap, bit)
	}

	var buf bytes.Buffer
	_ = xdr.WriteUint64(&buf, cookie)     // cookie
	buf.Write(make([]byte, 8))            // cookieverf (8 zero bytes)
	_ = xdr.WriteUint32(&buf, 4096)       // dircount (hint)
	_ = xdr.WriteUint32(&buf, maxcount)   // maxcount
	_ = attrs.EncodeBitmap4(&buf, bitmap) // attr_request
	return encodedOp{opCode: types.OP_READDIR, args: buf.Bytes()}
}

func encodeAccess(mask uint32) encodedOp {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, mask)
	return encodedOp{opCode: types.OP_ACCESS, args: buf.Bytes()}
}

func encodeIllegal() encodedOp {
	return encodedOp{opCode: types.OP_ILLEGAL}
}

func encodeSetClientID(clientName string) encodedOp {
	var buf bytes.Buffer
	buf.Write(make([]byte, 8))                    // client verifier (8 zero bytes)
	_ = xdr.WriteXDRString(&buf, clientName)      // client id string
	_ = xdr.WriteUint32(&buf, 0x40000000)         // callback program
	_ = xdr.WriteXDRString(&buf, "tcp")           // callback netid
	_ = xdr.WriteXDRString(&buf, "127.0.0.1.8.1") // callback addr
	_ = xdr.WriteUint32(&buf, 1)                  // callback_ident
	return encodedOp{opCode: types.OP_SETCLIENTID, args: buf.Bytes()}
}

func encodeSetClientIDConfirm(clientID uint64, confirmVerf ...[]byte) encodedOp {
	var buf bytes.Buffer
	_ = xdr.WriteUint64(&buf, clientID)
	if len(confirmVerf) > 0 && len(confirmVerf[0]) == 8 {
		buf.Write(confirmVerf[0])
	} else {
		buf.Write(make([]byte, 8)) // zero confirm verifier (for backward compat)
	}
	return encodedOp{opCode: types.OP_SETCLIENTID_CONFIRM, args: buf.Bytes()}
}

// Response decoder

// decodedCompoundResp holds decoded COMPOUND4res fields for test assertions.
type decodedCompoundResp struct {
	Status     uint32
	Tag        []byte
	NumResults uint32
	Results    []decodedOpResult
}

type decodedOpResult struct {
	OpCode    uint32
	Status    uint32
	ExtraData []byte // remaining bytes after status
}

func decodeCompoundResp(data []byte) (*decodedCompoundResp, error) {
	reader := bytes.NewReader(data)

	status, err := xdr.DecodeUint32(reader)
	if err != nil {
		return nil, err
	}

	tag, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, err
	}

	numResults, err := xdr.DecodeUint32(reader)
	if err != nil {
		return nil, err
	}

	results := make([]decodedOpResult, 0, numResults)
	for i := uint32(0); i < numResults; i++ {
		opCode, err := xdr.DecodeUint32(reader)
		if err != nil {
			return nil, err
		}

		opStatus, err := xdr.DecodeUint32(reader)
		if err != nil {
			return nil, err
		}

		// Read all remaining data for this result by peeking ahead
		// Note: For test purposes, we capture extra bytes until the next op
		// by reading to the end for the last result.
		var extra []byte
		if opStatus == types.NFS4_OK {
			// The extra data depends on the operation type
			switch opCode {
			case types.OP_GETFH:
				// Read opaque filehandle
				fh, err := xdr.DecodeOpaque(reader)
				if err == nil {
					extra = fh
				}
			case types.OP_ACCESS:
				// Read supported (uint32) + access (uint32)
				supported, _ := xdr.DecodeUint32(reader)
				access, _ := xdr.DecodeUint32(reader)
				extra = make([]byte, 8)
				binary.BigEndian.PutUint32(extra[0:4], supported)
				binary.BigEndian.PutUint32(extra[4:8], access)
			case types.OP_GETATTR:
				// Read all remaining GETATTR response data (bitmap + opaque attrvals)
				// which follows the status. Capture as raw bytes for test-level parsing.
				remaining := make([]byte, reader.Len())
				_, _ = reader.Read(remaining)
				extra = remaining
			case types.OP_READDIR:
				// Read remaining READDIR response data (cookieverf + entries + eof)
				remaining := make([]byte, reader.Len())
				_, _ = reader.Read(remaining)
				extra = remaining
			case types.OP_SETCLIENTID:
				// Read clientid (uint64) + confirm verifier (8 bytes)
				clientID, _ := xdr.DecodeUint64(reader)
				verf := make([]byte, 8)
				_, _ = reader.Read(verf)
				extra = make([]byte, 16)
				binary.BigEndian.PutUint64(extra[0:8], clientID)
				copy(extra[8:16], verf)
			}
		}

		results = append(results, decodedOpResult{
			OpCode:    opCode,
			Status:    opStatus,
			ExtraData: extra,
		})
	}

	return &decodedCompoundResp{
		Status:     status,
		Tag:        tag,
		NumResults: numResults,
		Results:    results,
	}, nil
}

func newOpsTestContext() *types.CompoundContext {
	return &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}
}

// ============================================================================
// Tests
// ============================================================================

func TestPutRootFH_GetFH(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export", "/data/archive"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeGetFH(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResp(resp)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if decoded.Status != types.NFS4_OK {
		t.Errorf("overall status = %d, want NFS4_OK", decoded.Status)
	}
	if decoded.NumResults != 2 {
		t.Fatalf("numResults = %d, want 2", decoded.NumResults)
	}

	// PUTROOTFH result
	if decoded.Results[0].Status != types.NFS4_OK {
		t.Errorf("PUTROOTFH status = %d, want NFS4_OK", decoded.Results[0].Status)
	}

	// GETFH result
	if decoded.Results[1].Status != types.NFS4_OK {
		t.Errorf("GETFH status = %d, want NFS4_OK", decoded.Results[1].Status)
	}
	fh := decoded.Results[1].ExtraData
	if !pseudofs.IsPseudoFSHandle(fh) {
		t.Errorf("GETFH returned handle %q, expected pseudofs handle", string(fh))
	}
}

func TestPutPubFH_EqualsPutRootFH(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})

	// Get handle from PUTROOTFH
	ctx1 := newOpsTestContext()
	data1 := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeGetFH(),
	})
	resp1, err := h.ProcessCompound(ctx1, data1)
	if err != nil {
		t.Fatalf("ProcessCompound 1 error: %v", err)
	}
	decoded1, _ := decodeCompoundResp(resp1)
	rootHandle := decoded1.Results[1].ExtraData

	// Get handle from PUTPUBFH
	ctx2 := newOpsTestContext()
	data2 := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutPubFH(),
		encodeGetFH(),
	})
	resp2, err := h.ProcessCompound(ctx2, data2)
	if err != nil {
		t.Fatalf("ProcessCompound 2 error: %v", err)
	}
	decoded2, _ := decodeCompoundResp(resp2)
	pubHandle := decoded2.Results[1].ExtraData

	if !bytes.Equal(rootHandle, pubHandle) {
		t.Errorf("PUTPUBFH handle %q != PUTROOTFH handle %q", string(pubHandle), string(rootHandle))
	}
}

func TestPutFH_ValidHandle(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})

	// Get root handle first
	rootHandle := h.PseudoFS.GetRootHandle()

	ctx := newOpsTestContext()
	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutFH(rootHandle),
		encodeGetFH(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4_OK {
		t.Errorf("status = %d, want NFS4_OK", decoded.Status)
	}

	gotHandle := decoded.Results[1].ExtraData
	if !bytes.Equal(gotHandle, rootHandle) {
		t.Errorf("GETFH returned %q, want %q", string(gotHandle), string(rootHandle))
	}
}

func TestPutFH_OversizedHandle(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	// Create a handle that's too large (130 bytes)
	bigHandle := bytes.Repeat([]byte("x"), 130)

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutFH(bigHandle),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_BADHANDLE {
		t.Errorf("status = %d, want NFS4ERR_BADHANDLE (%d)",
			decoded.Status, types.NFS4ERR_BADHANDLE)
	}
}

func TestGetFH_NoCurrentFH(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	// GETFH without setting a current filehandle
	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodeGetFH(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			decoded.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestSaveFH_RestoreFH(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})

	// PUTROOTFH, SAVEFH, LOOKUP("export"), RESTOREFH, GETFH
	// After RESTOREFH, GETFH should return root handle (not /export handle)
	ctx := newOpsTestContext()
	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeSaveFH(),
		encodeLookup("export"),
		encodeRestoreFH(),
		encodeGetFH(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4_OK {
		t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
	}
	if decoded.NumResults != 5 {
		t.Fatalf("numResults = %d, want 5", decoded.NumResults)
	}

	// All ops should succeed
	for i, r := range decoded.Results {
		if r.Status != types.NFS4_OK {
			t.Errorf("result[%d] status = %d, want NFS4_OK", i, r.Status)
		}
	}

	// GETFH should return root handle (restored from saved)
	rootHandle := h.PseudoFS.GetRootHandle()
	gotHandle := decoded.Results[4].ExtraData
	if !bytes.Equal(gotHandle, rootHandle) {
		t.Errorf("GETFH after RESTOREFH = %q, want root handle %q",
			string(gotHandle), string(rootHandle))
	}
}

func TestRestoreFH_NoSavedFH(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeRestoreFH(), // No SAVEFH was done
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_RESTOREFH {
		t.Errorf("status = %d, want NFS4ERR_RESTOREFH (%d)",
			decoded.Status, types.NFS4ERR_RESTOREFH)
	}
	if decoded.NumResults != 2 {
		t.Fatalf("numResults = %d, want 2", decoded.NumResults)
	}
	if decoded.Results[0].Status != types.NFS4_OK {
		t.Errorf("PUTROOTFH status = %d, want NFS4_OK", decoded.Results[0].Status)
	}
	if decoded.Results[1].Status != types.NFS4ERR_RESTOREFH {
		t.Errorf("RESTOREFH status = %d, want NFS4ERR_RESTOREFH", decoded.Results[1].Status)
	}
}

func TestLookup_PseudoFSRoot(t *testing.T) {
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
		t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
	}
	if decoded.NumResults != 3 {
		t.Fatalf("numResults = %d, want 3", decoded.NumResults)
	}

	// GETFH should return /export node handle (still pseudo-fs since registry is nil)
	fh := decoded.Results[2].ExtraData
	if !pseudofs.IsPseudoFSHandle(fh) {
		t.Errorf("expected pseudo-fs handle for /export, got %q", string(fh))
	}
}

func TestLookup_Nonexistent(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeLookup("nonexistent"),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_NOENT {
		t.Errorf("status = %d, want NFS4ERR_NOENT (%d)",
			decoded.Status, types.NFS4ERR_NOENT)
	}
}

func TestLookup_InvalidUTF8(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeLookup(string([]byte{0xFF, 0xFE})),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_BADCHAR {
		t.Errorf("status = %d, want NFS4ERR_BADCHAR (%d)",
			decoded.Status, types.NFS4ERR_BADCHAR)
	}
}

func TestLookup_StopsCompoundOnError(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	// PUTROOTFH, LOOKUP("nonexistent"), GETFH
	// GETFH should NOT execute because LOOKUP fails
	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeLookup("nonexistent"),
		encodeGetFH(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	// Only 2 results: PUTROOTFH succeeded, LOOKUP failed, GETFH not executed
	if decoded.NumResults != 2 {
		t.Fatalf("numResults = %d, want 2 (GETFH should not execute)", decoded.NumResults)
	}
	if decoded.Results[0].Status != types.NFS4_OK {
		t.Errorf("PUTROOTFH status = %d, want NFS4_OK", decoded.Results[0].Status)
	}
	if decoded.Results[1].Status != types.NFS4ERR_NOENT {
		t.Errorf("LOOKUP status = %d, want NFS4ERR_NOENT", decoded.Results[1].Status)
	}
}

func TestLookupP_ChildToRoot(t *testing.T) {
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
		t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
	}
	if decoded.NumResults != 4 {
		t.Fatalf("numResults = %d, want 4", decoded.NumResults)
	}

	// GETFH should return root handle (LOOKUPP from /export goes to root)
	rootHandle := h.PseudoFS.GetRootHandle()
	gotHandle := decoded.Results[3].ExtraData
	if !bytes.Equal(gotHandle, rootHandle) {
		t.Errorf("LOOKUPP from /export should return root, got %q want %q",
			string(gotHandle), string(rootHandle))
	}
}

func TestLookupP_RootStaysAtRoot(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeLookupP(),
		encodeGetFH(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4_OK {
		t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
	}

	// LOOKUPP from root should return root (root's parent is root)
	rootHandle := h.PseudoFS.GetRootHandle()
	gotHandle := decoded.Results[2].ExtraData
	if !bytes.Equal(gotHandle, rootHandle) {
		t.Errorf("LOOKUPP from root should stay at root, got %q want %q",
			string(gotHandle), string(rootHandle))
	}
}

func TestGetAttr_PseudoFSRoot(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeGetAttr(attrs.FATTR4_TYPE, attrs.FATTR4_FSID),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	// Parse the raw compound response directly to handle GETATTR response format
	reader := bytes.NewReader(resp)

	// Overall status
	overallStatus, _ := xdr.DecodeUint32(reader)
	if overallStatus != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", overallStatus)
	}

	// Tag (skip)
	_, _ = xdr.DecodeOpaque(reader)

	// Num results
	numResults, _ := xdr.DecodeUint32(reader)
	if numResults != 2 {
		t.Fatalf("numResults = %d, want 2", numResults)
	}

	// Result 1: PUTROOTFH (opcode + status only)
	op1, _ := xdr.DecodeUint32(reader)
	st1, _ := xdr.DecodeUint32(reader)
	if op1 != types.OP_PUTROOTFH || st1 != types.NFS4_OK {
		t.Fatalf("PUTROOTFH: opCode=%d status=%d", op1, st1)
	}

	// Result 2: GETATTR (opcode + status + bitmap + opaque attrvals)
	op2, _ := xdr.DecodeUint32(reader)
	st2, _ := xdr.DecodeUint32(reader)
	if op2 != types.OP_GETATTR {
		t.Fatalf("expected OP_GETATTR (%d), got %d", types.OP_GETATTR, op2)
	}
	if st2 != types.NFS4_OK {
		t.Fatalf("GETATTR status = %d, want NFS4_OK", st2)
	}

	// Decode response bitmap
	respBitmap, err := attrs.DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}

	// Should have TYPE (bit 1) and FSID (bit 8) set
	if !attrs.IsBitSet(respBitmap, attrs.FATTR4_TYPE) {
		t.Error("response bitmap should have FATTR4_TYPE set")
	}
	if !attrs.IsBitSet(respBitmap, attrs.FATTR4_FSID) {
		t.Error("response bitmap should have FATTR4_FSID set")
	}

	// Read attr data opaque
	attrData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		t.Fatalf("decode attr data: %v", err)
	}
	attrDataReader := bytes.NewReader(attrData)

	// First attr: TYPE (uint32) - should be NF4DIR (2)
	fileType, err := xdr.DecodeUint32(attrDataReader)
	if err != nil {
		t.Fatalf("decode file type: %v", err)
	}
	if fileType != types.NF4DIR {
		t.Errorf("TYPE = %d, want NF4DIR (%d)", fileType, types.NF4DIR)
	}

	// Second attr: FSID (two uint64s) - should be (0, 1)
	fsidMajor, err := xdr.DecodeUint64(attrDataReader)
	if err != nil {
		t.Fatalf("decode FSID major: %v", err)
	}
	fsidMinor, err := xdr.DecodeUint64(attrDataReader)
	if err != nil {
		t.Fatalf("decode FSID minor: %v", err)
	}
	if fsidMajor != 0 || fsidMinor != 1 {
		t.Errorf("FSID = (%d, %d), want (0, 1)", fsidMajor, fsidMinor)
	}
}

func TestGetAttr_EmptyBitmap(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeGetAttrEmpty(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	// Parse raw compound response
	reader := bytes.NewReader(resp)

	overallStatus, _ := xdr.DecodeUint32(reader)
	if overallStatus != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", overallStatus)
	}
	_, _ = xdr.DecodeOpaque(reader) // tag
	_, _ = xdr.DecodeUint32(reader) // numResults

	// Skip PUTROOTFH result
	_, _ = xdr.DecodeUint32(reader) // opcode
	_, _ = xdr.DecodeUint32(reader) // status

	// GETATTR result
	op2, _ := xdr.DecodeUint32(reader)
	st2, _ := xdr.DecodeUint32(reader)
	if op2 != types.OP_GETATTR {
		t.Fatalf("expected OP_GETATTR, got %d", op2)
	}
	if st2 != types.NFS4_OK {
		t.Fatalf("GETATTR status = %d, want NFS4_OK", st2)
	}

	// Response bitmap should be empty (intersect of empty request with supported = empty)
	respBitmap, err := attrs.DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}
	allZero := true
	for _, word := range respBitmap {
		if word != 0 {
			allZero = false
			break
		}
	}
	if !allZero {
		t.Errorf("empty bitmap request should produce empty response bitmap, got %v", respBitmap)
	}

	// Attr data should be empty opaque (length 0)
	attrData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		t.Fatalf("decode attr data: %v", err)
	}
	if len(attrData) != 0 {
		t.Errorf("attr data length = %d, want 0 for empty bitmap request", len(attrData))
	}
}

func TestReadDir_PseudoFSRoot(t *testing.T) {
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
		t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
	}
	if decoded.NumResults != 2 {
		t.Fatalf("numResults = %d, want 2", decoded.NumResults)
	}
	if decoded.Results[1].Status != types.NFS4_OK {
		t.Fatalf("READDIR status = %d, want NFS4_OK", decoded.Results[1].Status)
	}

	// Parse the READDIR response manually
	// The extra data contains: cookieverf(8) + entries... + eof
	extraReader := bytes.NewReader(decoded.Results[1].ExtraData)

	// cookieverf (8 bytes)
	cookieVerf := make([]byte, 8)
	_, _ = extraReader.Read(cookieVerf)

	// Read entries
	var entryNames []string
	for {
		hasNext, err := xdr.DecodeUint32(extraReader)
		if err != nil || hasNext == 0 {
			break
		}

		// cookie
		_, _ = xdr.DecodeUint64(extraReader)

		// name
		name, err := xdr.DecodeString(extraReader)
		if err != nil {
			t.Fatalf("decode entry name: %v", err)
		}
		entryNames = append(entryNames, name)

		// attrs (bitmap + opaque data)
		_, _ = attrs.DecodeBitmap4(extraReader)
		_, _ = xdr.DecodeOpaque(extraReader)
	}

	// eof
	eof, err := xdr.DecodeUint32(extraReader)
	if err != nil {
		t.Fatalf("decode eof: %v", err)
	}
	if eof != 1 {
		t.Errorf("eof = %d, want 1 (true)", eof)
	}

	// Should contain "data" and "export" (sorted)
	if len(entryNames) != 2 {
		t.Fatalf("entry count = %d, want 2, got %v", len(entryNames), entryNames)
	}
	if entryNames[0] != "data" {
		t.Errorf("entry[0] = %q, want \"data\"", entryNames[0])
	}
	if entryNames[1] != "export" {
		t.Errorf("entry[1] = %q, want \"export\"", entryNames[1])
	}
}

func TestAccess_PseudoFS(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	allBits := uint32(ACCESS4_READ | ACCESS4_LOOKUP | ACCESS4_MODIFY |
		ACCESS4_EXTEND | ACCESS4_DELETE | ACCESS4_EXECUTE)

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutRootFH(),
		encodeAccess(allBits),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4_OK {
		t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
	}
	if decoded.Results[1].Status != types.NFS4_OK {
		t.Fatalf("ACCESS status = %d, want NFS4_OK", decoded.Results[1].Status)
	}

	// Decode supported and access from extra data
	if len(decoded.Results[1].ExtraData) != 8 {
		t.Fatalf("ACCESS extra data length = %d, want 8", len(decoded.Results[1].ExtraData))
	}
	supported := binary.BigEndian.Uint32(decoded.Results[1].ExtraData[0:4])
	access := binary.BigEndian.Uint32(decoded.Results[1].ExtraData[4:8])

	if supported != allBits {
		t.Errorf("supported = 0x%x, want 0x%x", supported, allBits)
	}
	if access != allBits {
		t.Errorf("access = 0x%x, want 0x%x", access, allBits)
	}
}

func TestIllegal(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodeIllegal(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_OP_ILLEGAL {
		t.Errorf("status = %d, want NFS4ERR_OP_ILLEGAL (%d)",
			decoded.Status, types.NFS4ERR_OP_ILLEGAL)
	}
}

func TestSetClientID(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodeSetClientID("test-client-1"),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4_OK {
		t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
	}
	if decoded.Results[0].Status != types.NFS4_OK {
		t.Fatalf("SETCLIENTID status = %d, want NFS4_OK", decoded.Results[0].Status)
	}

	// Extra data should contain clientid (8 bytes) + confirm verifier (8 bytes)
	if len(decoded.Results[0].ExtraData) != 16 {
		t.Fatalf("SETCLIENTID extra data length = %d, want 16", len(decoded.Results[0].ExtraData))
	}

	clientID := binary.BigEndian.Uint64(decoded.Results[0].ExtraData[0:8])
	if clientID == 0 {
		t.Error("SETCLIENTID returned client ID 0, want non-zero")
	}
}

func TestSetClientIDConfirm(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	// Step 1: SETCLIENTID to get client ID and confirm verifier
	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodeSetClientID("confirm-test-client"),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)
	if decoded.Status != types.NFS4_OK {
		t.Fatalf("SETCLIENTID status = %d, want NFS4_OK", decoded.Status)
	}

	// Extract client ID and confirm verifier from response
	if len(decoded.Results[0].ExtraData) != 16 {
		t.Fatalf("SETCLIENTID extra data length = %d, want 16", len(decoded.Results[0].ExtraData))
	}
	clientID := binary.BigEndian.Uint64(decoded.Results[0].ExtraData[0:8])
	confirmVerf := decoded.Results[0].ExtraData[8:16]

	// Step 2: SETCLIENTID_CONFIRM with correct verifier
	data = encodeCompoundWithOps("", 0, []encodedOp{
		encodeSetClientIDConfirm(clientID, confirmVerf),
	})

	resp, err = h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ = decodeCompoundResp(resp)

	if decoded.Status != types.NFS4_OK {
		t.Fatalf("status = %d, want NFS4_OK", decoded.Status)
	}
	if decoded.Results[0].Status != types.NFS4_OK {
		t.Fatalf("SETCLIENTID_CONFIRM status = %d, want NFS4_OK", decoded.Results[0].Status)
	}
}

func TestSetClientIDConfirm_StaleClientID(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	// SETCLIENTID_CONFIRM with unknown client ID should fail
	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodeSetClientIDConfirm(99999),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_STALE_CLIENTID {
		t.Errorf("status = %d, want NFS4ERR_STALE_CLIENTID (%d)",
			decoded.Status, types.NFS4ERR_STALE_CLIENTID)
	}
}

func TestEndToEnd_BrowsePseudoFS(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export", "/data/archive"})
	ctx := newOpsTestContext()

	// Navigate: PUTROOTFH -> GETATTR(TYPE) -> LOOKUP("data") ->
	// GETATTR(TYPE) -> LOOKUP("archive") -> GETFH
	data := encodeCompoundWithOps("browse", 0, []encodedOp{
		encodePutRootFH(),
		encodeGetAttr(attrs.FATTR4_TYPE),
		encodeLookup("data"),
		encodeGetAttr(attrs.FATTR4_TYPE),
		encodeLookup("archive"),
		encodeGetFH(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	// Parse the raw compound response to validate all 6 results
	reader := bytes.NewReader(resp)

	overallStatus, _ := xdr.DecodeUint32(reader)
	if overallStatus != types.NFS4_OK {
		t.Fatalf("overall status = %d, want NFS4_OK", overallStatus)
	}

	tag, _ := xdr.DecodeOpaque(reader)
	if string(tag) != "browse" {
		t.Errorf("tag = %q, want %q", string(tag), "browse")
	}

	numResults, _ := xdr.DecodeUint32(reader)
	if numResults != 6 {
		t.Fatalf("numResults = %d, want 6", numResults)
	}

	// Result 1: PUTROOTFH (status only)
	op, _ := xdr.DecodeUint32(reader)
	st, _ := xdr.DecodeUint32(reader)
	if op != types.OP_PUTROOTFH || st != types.NFS4_OK {
		t.Errorf("result[0] PUTROOTFH: op=%d status=%d", op, st)
	}

	// Result 2: GETATTR (status + bitmap + opaque attrvals)
	op, _ = xdr.DecodeUint32(reader)
	st, _ = xdr.DecodeUint32(reader)
	if op != types.OP_GETATTR || st != types.NFS4_OK {
		t.Errorf("result[1] GETATTR: op=%d status=%d", op, st)
	}
	_, _ = attrs.DecodeBitmap4(reader) // skip response bitmap
	_, _ = xdr.DecodeOpaque(reader)    // skip attr data

	// Result 3: LOOKUP (status only)
	op, _ = xdr.DecodeUint32(reader)
	st, _ = xdr.DecodeUint32(reader)
	if op != types.OP_LOOKUP || st != types.NFS4_OK {
		t.Errorf("result[2] LOOKUP(data): op=%d status=%d", op, st)
	}

	// Result 4: GETATTR (status + bitmap + opaque attrvals)
	op, _ = xdr.DecodeUint32(reader)
	st, _ = xdr.DecodeUint32(reader)
	if op != types.OP_GETATTR || st != types.NFS4_OK {
		t.Errorf("result[3] GETATTR: op=%d status=%d", op, st)
	}
	_, _ = attrs.DecodeBitmap4(reader) // skip response bitmap
	_, _ = xdr.DecodeOpaque(reader)    // skip attr data

	// Result 5: LOOKUP (status only)
	op, _ = xdr.DecodeUint32(reader)
	st, _ = xdr.DecodeUint32(reader)
	if op != types.OP_LOOKUP || st != types.NFS4_OK {
		t.Errorf("result[4] LOOKUP(archive): op=%d status=%d", op, st)
	}

	// Result 6: GETFH (status + opaque filehandle)
	op, _ = xdr.DecodeUint32(reader)
	st, _ = xdr.DecodeUint32(reader)
	if op != types.OP_GETFH || st != types.NFS4_OK {
		t.Errorf("result[5] GETFH: op=%d status=%d", op, st)
	}
	fh, _ := xdr.DecodeOpaque(reader)
	if !pseudofs.IsPseudoFSHandle(fh) {
		t.Errorf("GETFH for /data/archive not a pseudo-fs handle: %q", string(fh))
	}
}

func TestLookup_NoCurrentFH(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodeLookup("export"),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			decoded.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestSaveFH_NoCurrentFH(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodeSaveFH(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			decoded.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestAccess_NoCurrentFH(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodeAccess(0x3F),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			decoded.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestGetAttr_NoCurrentFH(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodeGetAttr(attrs.FATTR4_TYPE),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			decoded.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestPutFH_EmptyHandle(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodePutFH([]byte{}),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_BADHANDLE {
		t.Errorf("status = %d, want NFS4ERR_BADHANDLE (%d)",
			decoded.Status, types.NFS4ERR_BADHANDLE)
	}
}

func TestLookupP_NoCurrentFH(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodeLookupP(),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			decoded.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestReadDir_NoCurrentFH(t *testing.T) {
	h := newTestHandlerWithShares([]string{"/export"})
	ctx := newOpsTestContext()

	data := encodeCompoundWithOps("", 0, []encodedOp{
		encodeReadDir(0, 4096, attrs.FATTR4_TYPE),
	})

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, _ := decodeCompoundResp(resp)

	if decoded.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			decoded.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}
