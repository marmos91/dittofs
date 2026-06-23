package handlers

import (
	"bytes"
	"io"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// sparseTestHandler builds a Handler with no runtime registry. That is enough to
// exercise the protocol-layer concerns of the RFC 7862 sparse ops (XDR decode,
// pseudo-fs rejection, stateid validation, arg validation); the data path
// (segment computation, zero reads, reclaim) is covered by the pkg/block hole
// map unit tests and the test/e2e sparse suite.
func sparseTestHandler(t *testing.T) *Handler {
	t.Helper()
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	return NewHandler(nil, pfs)
}

// anonStateid returns the all-zeros (anonymous) special stateid, which
// ValidateStateid accepts on both read and write families.
func anonStateid() *types.Stateid4 { return &types.Stateid4{} }

func writeStateid(buf *bytes.Buffer, sid *types.Stateid4) {
	_ = xdr.WriteUint32(buf, sid.Seqid)
	_, _ = buf.Write(sid.Other[:])
}

func encSeekArgs(sid *types.Stateid4, offset uint64, what uint32) io.Reader {
	var buf bytes.Buffer
	writeStateid(&buf, sid)
	_ = xdr.WriteUint64(&buf, offset)
	_ = xdr.WriteUint32(&buf, what)
	return bytes.NewReader(buf.Bytes())
}

func encAllocArgs(sid *types.Stateid4, offset, length uint64) io.Reader {
	var buf bytes.Buffer
	writeStateid(&buf, sid)
	_ = xdr.WriteUint64(&buf, offset)
	_ = xdr.WriteUint64(&buf, length)
	return bytes.NewReader(buf.Bytes())
}

// --- SEEK ---

func TestHandleSeek_Validation(t *testing.T) {
	h := sparseTestHandler(t)

	t.Run("truncated args -> BADXDR", func(t *testing.T) {
		res := h.handleSeek(xattrCtx(realHandle), bytes.NewReader([]byte{0x00, 0x01}))
		if res.Status != types.NFS4ERR_BADXDR {
			t.Fatalf("status = %d, want BADXDR", res.Status)
		}
		if res.OpCode != types.OP_SEEK {
			t.Errorf("opcode = %d, want OP_SEEK", res.OpCode)
		}
	})

	t.Run("invalid sa_what -> INVAL", func(t *testing.T) {
		res := h.handleSeek(xattrCtx(realHandle), encSeekArgs(anonStateid(), 0, 99))
		if res.Status != types.NFS4ERR_INVAL {
			t.Fatalf("status = %d, want INVAL", res.Status)
		}
	})

	t.Run("pseudo-fs handle -> ISDIR", func(t *testing.T) {
		root := h.PseudoFS.GetRootHandle()
		res := h.handleSeek(xattrCtx(root), encSeekArgs(anonStateid(), 0, types.NFS4_CONTENT_DATA))
		if res.Status != types.NFS4ERR_ISDIR {
			t.Fatalf("status = %d, want ISDIR", res.Status)
		}
	})

	t.Run("no current filehandle -> NOFILEHANDLE", func(t *testing.T) {
		ctx := xattrCtx(realHandle)
		ctx.CurrentFH = nil
		res := h.handleSeek(ctx, encSeekArgs(anonStateid(), 0, types.NFS4_CONTENT_DATA))
		if res.Status != types.NFS4ERR_NOFILEHANDLE {
			t.Fatalf("status = %d, want NOFILEHANDLE", res.Status)
		}
	})
}

// --- READ_PLUS ---

func TestHandleReadPlus_Validation(t *testing.T) {
	h := sparseTestHandler(t)

	t.Run("truncated args -> BADXDR", func(t *testing.T) {
		res := h.handleReadPlus(xattrCtx(realHandle), bytes.NewReader([]byte{0x00}))
		if res.Status != types.NFS4ERR_BADXDR {
			t.Fatalf("status = %d, want BADXDR", res.Status)
		}
		if res.OpCode != types.OP_READ_PLUS {
			t.Errorf("opcode = %d, want OP_READ_PLUS", res.OpCode)
		}
	})

	t.Run("pseudo-fs handle -> ISDIR", func(t *testing.T) {
		root := h.PseudoFS.GetRootHandle()
		var buf bytes.Buffer
		writeStateid(&buf, anonStateid())
		_ = xdr.WriteUint64(&buf, 0)
		_ = xdr.WriteUint32(&buf, 4096)
		res := h.handleReadPlus(xattrCtx(root), bytes.NewReader(buf.Bytes()))
		if res.Status != types.NFS4ERR_ISDIR {
			t.Fatalf("status = %d, want ISDIR", res.Status)
		}
	})
}

// encodeReadPlusResok / hole encoding shape check (no runtime needed).
func TestEncodeReadPlusResok(t *testing.T) {
	contents := []readPlusContent{
		{Hole: false, Offset: 0, Data: []byte("abcd")},
		{Hole: true, Offset: 4, Length: 12},
	}
	res := encodeReadPlusResok(false, contents)
	if res.Status != types.NFS4_OK || res.OpCode != types.OP_READ_PLUS {
		t.Fatalf("unexpected result envelope: status=%d op=%d", res.Status, res.OpCode)
	}
	r := bytes.NewReader(res.Data)
	st, _ := xdr.DecodeUint32(r)
	if st != types.NFS4_OK {
		t.Fatalf("encoded status = %d", st)
	}
	eof, _ := xdr.DecodeBool(r)
	if eof {
		t.Errorf("eof = true, want false")
	}
	n, _ := xdr.DecodeUint32(r)
	if n != 2 {
		t.Fatalf("content count = %d, want 2", n)
	}
	// First member: data.
	kind, _ := xdr.DecodeUint32(r)
	if kind != types.NFS4_CONTENT_DATA {
		t.Fatalf("member 0 kind = %d, want DATA", kind)
	}
	off, _ := xdr.DecodeUint64(r)
	data, _ := xdr.DecodeOpaque(r)
	if off != 0 || string(data) != "abcd" {
		t.Errorf("member 0 = (off=%d data=%q)", off, data)
	}
	// Second member: hole.
	kind, _ = xdr.DecodeUint32(r)
	if kind != types.NFS4_CONTENT_HOLE {
		t.Fatalf("member 1 kind = %d, want HOLE", kind)
	}
	hoff, _ := xdr.DecodeUint64(r)
	hlen, _ := xdr.DecodeUint64(r)
	if hoff != 4 || hlen != 12 {
		t.Errorf("member 1 hole = (off=%d len=%d), want (4,12)", hoff, hlen)
	}
}

// --- ALLOCATE / DEALLOCATE arg decoding ---

func TestDecodeAllocArgs(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		sid, off, length, st := decodeAllocArgs(encAllocArgs(anonStateid(), 100, 200))
		if st != types.NFS4_OK {
			t.Fatalf("status = %d, want OK", st)
		}
		if sid == nil || off != 100 || length != 200 {
			t.Errorf("decoded (off=%d len=%d)", off, length)
		}
	})

	t.Run("overflow -> INVAL", func(t *testing.T) {
		_, _, _, st := decodeAllocArgs(encAllocArgs(anonStateid(), ^uint64(0)-5, 100))
		if st != types.NFS4ERR_INVAL {
			t.Fatalf("status = %d, want INVAL", st)
		}
	})

	t.Run("truncated -> BADXDR", func(t *testing.T) {
		var buf bytes.Buffer
		writeStateid(&buf, anonStateid())
		_ = xdr.WriteUint64(&buf, 0) // offset only, missing length
		_, _, _, st := decodeAllocArgs(bytes.NewReader(buf.Bytes()))
		if st != types.NFS4ERR_BADXDR {
			t.Fatalf("status = %d, want BADXDR", st)
		}
	})
}

func TestHandleAllocateDeallocate_PseudoFS(t *testing.T) {
	h := sparseTestHandler(t)
	root := h.PseudoFS.GetRootHandle()

	if res := h.handleAllocate(xattrCtx(root), encAllocArgs(anonStateid(), 0, 4096)); res.Status != types.NFS4ERR_ROFS {
		t.Errorf("ALLOCATE on pseudo-fs status = %d, want ROFS", res.Status)
	}
	if res := h.handleDeallocate(xattrCtx(root), encAllocArgs(anonStateid(), 0, 4096)); res.Status != types.NFS4ERR_ROFS {
		t.Errorf("DEALLOCATE on pseudo-fs status = %d, want ROFS", res.Status)
	}
}

func TestPunchLen(t *testing.T) {
	cases := []struct{ off, length, size, want uint64 }{
		{0, 100, 200, 100}, // within file
		{50, 100, 120, 70}, // clamped to EOF
		{0, 200, 200, 200}, // exactly to EOF
	}
	for _, c := range cases {
		if got := punchLen(c.off, c.length, c.size); got != c.want {
			t.Errorf("punchLen(%d,%d,%d) = %d, want %d", c.off, c.length, c.size, got, c.want)
		}
	}
}
