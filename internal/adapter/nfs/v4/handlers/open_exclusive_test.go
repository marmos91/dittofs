package handlers

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// encodeOpenExclusiveArgs builds OPEN4args for an EXCLUSIVE4 create with the
// given 8-byte verifier (big-endian encoding of verf).
func encodeOpenExclusiveArgs(
	seqid, shareAccess, shareDeny uint32,
	clientID uint64, owner []byte, verf uint64, filename string,
) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, seqid)
	_ = xdr.WriteUint32(&buf, shareAccess)
	_ = xdr.WriteUint32(&buf, shareDeny)
	_ = xdr.WriteUint64(&buf, clientID)
	_ = xdr.WriteXDROpaque(&buf, owner)
	_ = xdr.WriteUint32(&buf, types.OPEN4_CREATE)
	_ = xdr.WriteUint32(&buf, types.EXCLUSIVE4)
	var verifier [8]byte
	binary.BigEndian.PutUint64(verifier[:], verf)
	buf.Write(verifier[:])
	_ = xdr.WriteUint32(&buf, types.CLAIM_NULL)
	_ = xdr.WriteXDRString(&buf, filename)
	return buf.Bytes()
}

// encodeOpenUncheckedSizeArgs builds OPEN4args for an UNCHECKED4 create whose
// createattrs request the given file size (FATTR4_SIZE).
func encodeOpenUncheckedSizeArgs(
	seqid, shareAccess, shareDeny uint32,
	clientID uint64, owner []byte, size uint64, filename string,
) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, seqid)
	_ = xdr.WriteUint32(&buf, shareAccess)
	_ = xdr.WriteUint32(&buf, shareDeny)
	_ = xdr.WriteUint64(&buf, clientID)
	_ = xdr.WriteXDROpaque(&buf, owner)
	_ = xdr.WriteUint32(&buf, types.OPEN4_CREATE)
	_ = xdr.WriteUint32(&buf, types.UNCHECKED4)

	// createattrs (fattr4 = bitmap4 + opaque attr_vals) requesting size.
	var attrVals bytes.Buffer
	_ = xdr.WriteUint64(&attrVals, size)
	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)
	_ = attrs.EncodeBitmap4(&buf, bitmap)
	_ = xdr.WriteXDROpaque(&buf, attrVals.Bytes())

	_ = xdr.WriteUint32(&buf, types.CLAIM_NULL)
	_ = xdr.WriteXDRString(&buf, filename)
	return buf.Bytes()
}

// TestOpen_Exclusive4_RetrySameVerifier verifies RFC 7530 §16.16.3 idempotency:
// an EXCLUSIVE4 create followed by a retransmission with the SAME verifier must
// succeed (the create is idempotent), returning a valid stateid rather than
// NFS4ERR_EXIST.
func TestOpen_Exclusive4_RetrySameVerifier(t *testing.T) {
	const clientID = uint64(0xCAFE)
	owner := []byte("excl-owner")
	const verf = uint64(0x0123456789abcdef)

	fx := newIOTestFixture(t, "/export")
	ctx := newRealFSContext(0, 0)

	open := func() *types.CompoundResult {
		ctx.CurrentFH = make([]byte, len(fx.rootHandle))
		copy(ctx.CurrentFH, fx.rootHandle)
		args := encodeOpenExclusiveArgs(1, types.OPEN4_SHARE_ACCESS_BOTH,
			types.OPEN4_SHARE_DENY_NONE, clientID, owner, verf, "excl.txt")
		return fx.handler.handleOpen(ctx, bytes.NewReader(args))
	}

	r1 := open()
	if r1.Status != types.NFS4_OK {
		t.Fatalf("first EXCLUSIVE4 OPEN status = %d, want NFS4_OK", r1.Status)
	}

	// Retransmission with the same verifier: must be idempotent success.
	r2 := open()
	if r2.Status != types.NFS4_OK {
		t.Fatalf("EXCLUSIVE4 retry (same verifier) status = %d, want NFS4_OK", r2.Status)
	}

	// The reply must carry a decodable stateid.
	rd := bytes.NewReader(r2.Data)
	if _, err := xdr.DecodeUint32(rd); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if _, err := types.DecodeStateid4(rd); err != nil {
		t.Fatalf("EXCLUSIVE4 retry reply missing valid stateid: %v", err)
	}
}

// TestOpen_Exclusive4_RetryDifferentVerifier verifies that an EXCLUSIVE4 create
// retransmitted with a DIFFERENT verifier (i.e. a genuine conflict on an
// already-existing file) returns NFS4ERR_EXIST.
func TestOpen_Exclusive4_RetryDifferentVerifier(t *testing.T) {
	const clientID = uint64(0xBEEF)
	owner := []byte("excl-owner2")

	fx := newIOTestFixture(t, "/export")
	ctx := newRealFSContext(0, 0)

	open := func(verf uint64) *types.CompoundResult {
		ctx.CurrentFH = make([]byte, len(fx.rootHandle))
		copy(ctx.CurrentFH, fx.rootHandle)
		args := encodeOpenExclusiveArgs(1, types.OPEN4_SHARE_ACCESS_BOTH,
			types.OPEN4_SHARE_DENY_NONE, clientID, owner, verf, "excl2.txt")
		return fx.handler.handleOpen(ctx, bytes.NewReader(args))
	}

	if r := open(0x1111111111111111); r.Status != types.NFS4_OK {
		t.Fatalf("first EXCLUSIVE4 OPEN status = %d, want NFS4_OK", r.Status)
	}

	if r := open(0x2222222222222222); r.Status != types.NFS4ERR_EXIST {
		t.Fatalf("EXCLUSIVE4 with different verifier status = %d, want NFS4ERR_EXIST", r.Status)
	}
}

// TestOpen_Unchecked4_ExistingFileTruncates verifies RFC 7530 §16.16: an
// UNCHECKED4 OPEN of an existing file applies the supplied createattrs. A
// requested size=0 must truncate the file's metadata size to zero.
func TestOpen_Unchecked4_ExistingFileTruncates(t *testing.T) {
	const clientID = uint64(0xF00D)
	owner := []byte("unchecked-owner")

	fx := newIOTestFixture(t, "/export")
	ctx := newRealFSContext(0, 0)

	// Create a file and give it some content/size.
	fh := fx.createRegularFile(t, fx.rootHandle, "trunc.txt", 0o644, 0, 0)
	fx.writeContent(t, fh, []byte("hello world contents"))

	pre, err := fx.metaSvc.GetFile(context.Background(), fh)
	if err != nil {
		t.Fatalf("get file pre: %v", err)
	}
	if pre.Size == 0 {
		t.Fatalf("setup: expected non-zero size before truncate")
	}

	// UNCHECKED4 OPEN of the existing file requesting size=0.
	ctx.CurrentFH = make([]byte, len(fx.rootHandle))
	copy(ctx.CurrentFH, fx.rootHandle)
	args := encodeOpenUncheckedSizeArgs(1, types.OPEN4_SHARE_ACCESS_BOTH,
		types.OPEN4_SHARE_DENY_NONE, clientID, owner, 0, "trunc.txt")
	r := fx.handler.handleOpen(ctx, bytes.NewReader(args))
	if r.Status != types.NFS4_OK {
		t.Fatalf("UNCHECKED4 OPEN existing status = %d, want NFS4_OK", r.Status)
	}

	post, err := fx.metaSvc.GetFile(context.Background(), metadata.FileHandle(ctx.CurrentFH))
	if err != nil {
		t.Fatalf("get file post: %v", err)
	}
	if post.Size != 0 {
		t.Fatalf("file size after UNCHECKED4 size=0 = %d, want 0 (truncation skipped)", post.Size)
	}
}
