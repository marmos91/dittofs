package handlers

import (
	"bytes"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// encodeGetAttrArgs builds the XDR args for GETATTR: a bare bitmap4.
func encodeGetAttrArgs(t *testing.T, bitmap []uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := attrs.EncodeBitmap4(&buf, bitmap); err != nil {
		t.Fatalf("encode bitmap: %v", err)
	}
	return buf.Bytes()
}

// ctxForHandle returns a root-credentialed context with handle as its
// current filehandle.
func ctxForHandle(handle metadata.FileHandle) *types.CompoundContext {
	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = make([]byte, len(handle))
	copy(ctx.CurrentFH, handle)
	return ctx
}

// TestHandleGetAttr_WriteOnlyAttr asserts a GETATTR naming a settable-only
// attribute is refused. Those attributes are advertised as supported so the
// Linux client will send them in SETATTR, but they have no value to read, and
// answering with a bitmap that quietly omits them tells the client its request
// succeeded (RFC 7530 Section 5.5).
func TestHandleGetAttr_WriteOnlyAttr(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	for _, tc := range []struct {
		name string
		bit  uint32
	}{
		{"time_access_set", attrs.FATTR4_TIME_ACCESS_SET},
		{"time_modify_set", attrs.FATTR4_TIME_MODIFY_SET},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bitmap []uint32
			attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)
			attrs.SetBit(&bitmap, tc.bit)

			args := encodeGetAttrArgs(t, bitmap)
			result := fx.handler.handleGetAttr(ctxForHandle(fileHandle), bytes.NewReader(args))

			if result.Status != types.NFS4ERR_INVAL {
				t.Errorf("GETATTR(%s) status = %d, want NFS4ERR_INVAL (%d)",
					tc.name, result.Status, types.NFS4ERR_INVAL)
			}
		})
	}
}

// TestHandleGetAttr_UnknownAttr asserts a GETATTR naming an attribute number
// the protocol never defined succeeds and returns nothing for it. NFS4ERR_-
// ATTRNOTSUPP must not come back from GETATTR (RFC 7530 Section 13.1.11.1), and
// the request must survive decoding at all: bit 1000 needs a 32-word bitmap4.
func TestHandleGetAttr_UnknownAttr(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	var bitmap []uint32
	attrs.SetBit(&bitmap, 1000)

	args := encodeGetAttrArgs(t, bitmap)
	result := fx.handler.handleGetAttr(ctxForHandle(fileHandle), bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("GETATTR(bit 1000) status = %d, want NFS4_OK", result.Status)
	}

	reader := bytes.NewReader(result.Data)
	if _, err := xdr.DecodeUint32(reader); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	responseBitmap, err := attrs.DecodeBitmap4(reader)
	if err != nil {
		t.Fatalf("decode response bitmap: %v", err)
	}
	for i, word := range responseBitmap {
		if word != 0 {
			t.Errorf("response bitmap word %d = %#x, want no attributes returned", i, word)
		}
	}
	attrData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		t.Fatalf("decode attr_vals: %v", err)
	}
	if len(attrData) != 0 {
		t.Errorf("attr_vals = %d bytes, want empty", len(attrData))
	}
}

// TestHandleVerify_UnsupportedAttr asserts VERIFY and NVERIFY report an
// attribute outside the supported set instead of comparing the remainder and
// reporting a match the client never asked about (RFC 7530 Sections 16.15.5
// and 16.35.5).
func TestHandleVerify_UnsupportedAttr(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	var bitmap []uint32
	attrs.SetBit(&bitmap, 14) // FATTR4_ARCHIVE

	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 0)
	args := encodeVerifyArgs(t, bitmap, attrVals.Bytes())

	if result := fx.handler.handleVerify(ctxForHandle(fileHandle), bytes.NewReader(args)); result.Status != types.NFS4ERR_ATTRNOTSUPP {
		t.Errorf("VERIFY(archive) status = %d, want NFS4ERR_ATTRNOTSUPP (%d)",
			result.Status, types.NFS4ERR_ATTRNOTSUPP)
	}
	if result := fx.handler.handleNVerify(ctxForHandle(fileHandle), bytes.NewReader(args)); result.Status != types.NFS4ERR_ATTRNOTSUPP {
		t.Errorf("NVERIFY(archive) status = %d, want NFS4ERR_ATTRNOTSUPP (%d)",
			result.Status, types.NFS4ERR_ATTRNOTSUPP)
	}
}

// TestHandleVerify_UnreadableAttr asserts VERIFY and NVERIFY refuse the two
// attributes that are supported yet have no value to compare against:
// rdattr_error, which only ever exists per-entry inside a READDIR reply, and
// the settable-only timestamps.
func TestHandleVerify_UnreadableAttr(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	for _, tc := range []struct {
		name string
		bit  uint32
	}{
		{"rdattr_error", attrs.FATTR4_RDATTR_ERROR},
		{"time_access_set", attrs.FATTR4_TIME_ACCESS_SET},
		{"time_modify_set", attrs.FATTR4_TIME_MODIFY_SET},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bitmap []uint32
			attrs.SetBit(&bitmap, tc.bit)

			var attrVals bytes.Buffer
			_ = xdr.WriteUint32(&attrVals, 0)
			args := encodeVerifyArgs(t, bitmap, attrVals.Bytes())

			if result := fx.handler.handleVerify(ctxForHandle(fileHandle), bytes.NewReader(args)); result.Status != types.NFS4ERR_INVAL {
				t.Errorf("VERIFY(%s) status = %d, want NFS4ERR_INVAL (%d)",
					tc.name, result.Status, types.NFS4ERR_INVAL)
			}
			if result := fx.handler.handleNVerify(ctxForHandle(fileHandle), bytes.NewReader(args)); result.Status != types.NFS4ERR_INVAL {
				t.Errorf("NVERIFY(%s) status = %d, want NFS4ERR_INVAL (%d)",
					tc.name, result.Status, types.NFS4ERR_INVAL)
			}
		})
	}
}

// TestHandleSetAttr_SizeOnNonRegularFile asserts SETATTR of the size attribute
// is refused for objects that have no byte stream, and that a directory is
// distinguished from the rest so the client learns why.
func TestHandleSetAttr_SizeOnNonRegularFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fileType metadata.FileType
		want     uint32
	}{
		{"directory", metadata.FileTypeDirectory, types.NFS4ERR_ISDIR},
		{"symlink", metadata.FileTypeSymlink, types.NFS4ERR_INVAL},
		{"fifo", metadata.FileTypeFIFO, types.NFS4ERR_INVAL},
		{"socket", metadata.FileTypeSocket, types.NFS4ERR_INVAL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRealFSTestFixture(t, "/export")
			handle := fx.createTestFile(t, fx.rootHandle, tc.name, tc.fileType, 0o644, 0, 0)

			var attrVals bytes.Buffer
			_ = xdr.WriteUint64(&attrVals, 0)

			var bitmap []uint32
			attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)

			args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
			result := fx.handler.handleSetAttr(ctxForHandle(handle), bytes.NewReader(args))

			if result.Status != tc.want {
				t.Errorf("SETATTR(size) on %s status = %d, want %d", tc.name, result.Status, tc.want)
			}
		})
	}
}

// TestHandleSetAttr_EmptyPrincipal asserts an empty owner or owner_group is
// rejected as a malformed argument. NFS4ERR_BADOWNER means a name that could
// not be mapped to a local identity, which an empty string is not.
func TestHandleSetAttr_EmptyPrincipal(t *testing.T) {
	for _, tc := range []struct {
		name string
		bit  uint32
	}{
		{"owner", attrs.FATTR4_OWNER},
		{"owner_group", attrs.FATTR4_OWNER_GROUP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRealFSTestFixture(t, "/export")
			handle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

			var attrVals bytes.Buffer
			_ = xdr.WriteXDRString(&attrVals, "")

			var bitmap []uint32
			attrs.SetBit(&bitmap, tc.bit)

			args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
			result := fx.handler.handleSetAttr(ctxForHandle(handle), bytes.NewReader(args))

			if result.Status != types.NFS4ERR_INVAL {
				t.Errorf("SETATTR(empty %s) status = %d, want NFS4ERR_INVAL (%d)",
					tc.name, result.Status, types.NFS4ERR_INVAL)
			}
		})
	}
}

// TestHandleSetAttr_TrailingAttrVals asserts attr_vals carrying more bytes than
// the bitmap accounts for is rejected. The surplus is a value for an attribute
// that was never named, so the two halves of the fattr4 disagree.
func TestHandleSetAttr_TrailingAttrVals(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	handle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	var attrVals bytes.Buffer
	_ = xdr.WriteUint32(&attrVals, 0o644)
	_ = xdr.WriteUint32(&attrVals, 0) // extraneous

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_MODE)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctxForHandle(handle), bytes.NewReader(args))

	if result.Status != types.NFS4ERR_BADXDR {
		t.Errorf("SETATTR with trailing attr_vals status = %d, want NFS4ERR_BADXDR (%d)",
			result.Status, types.NFS4ERR_BADXDR)
	}
}

// TestHandleSetAttr_InvalidNanoseconds asserts a settime4 whose nseconds field
// overflows a second is rejected rather than silently rolling the timestamp
// forward past the instant the client named.
func TestHandleSetAttr_InvalidNanoseconds(t *testing.T) {
	for _, tc := range []struct {
		name string
		bit  uint32
	}{
		{"time_access_set", attrs.FATTR4_TIME_ACCESS_SET},
		{"time_modify_set", attrs.FATTR4_TIME_MODIFY_SET},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRealFSTestFixture(t, "/export")
			handle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

			var attrVals bytes.Buffer
			_ = xdr.WriteUint32(&attrVals, attrs.SET_TO_CLIENT_TIME4)
			_ = xdr.WriteUint64(&attrVals, uint64(time.Now().Unix()))
			_ = xdr.WriteUint32(&attrVals, 1_000_000_000)

			var bitmap []uint32
			attrs.SetBit(&bitmap, tc.bit)

			args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
			result := fx.handler.handleSetAttr(ctxForHandle(handle), bytes.NewReader(args))

			if result.Status != types.NFS4ERR_INVAL {
				t.Errorf("SETATTR(%s, nseconds=1e9) status = %d, want NFS4ERR_INVAL (%d)",
					tc.name, result.Status, types.NFS4ERR_INVAL)
			}
		})
	}
}

// TestHandleSetAttr_SizeAboveMaxFileSize asserts a size beyond the value
// FATTR4_MAXFILESIZE advertises is refused. Reporting success would leave the
// file at a length no GETATTR can read back, and on a backend that stores the
// size as a signed 64-bit column the value cannot round-trip at all.
func TestHandleSetAttr_SizeAboveMaxFileSize(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	handle := fx.createTestFile(t, fx.rootHandle, "test.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	var attrVals bytes.Buffer
	_ = xdr.WriteUint64(&attrVals, ^uint64(0))

	var bitmap []uint32
	attrs.SetBit(&bitmap, attrs.FATTR4_SIZE)

	args := encodeSetAttrArgs(t, specialStateid(), bitmap, attrVals.Bytes())
	result := fx.handler.handleSetAttr(ctxForHandle(handle), bytes.NewReader(args))

	if result.Status != types.NFS4ERR_FBIG {
		t.Errorf("SETATTR(size=U64_MAX) status = %d, want NFS4ERR_FBIG (%d)",
			result.Status, types.NFS4ERR_FBIG)
	}
}
