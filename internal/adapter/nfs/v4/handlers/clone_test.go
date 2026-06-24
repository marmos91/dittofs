package handlers

import (
	"bytes"
	"io"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// encCloneArgs encodes CLONE4args: src_stateid, dst_stateid, src_offset,
// dst_offset, count (RFC 7862 Section 15.13).
func encCloneArgs(src, dst *types.Stateid4, srcOff, dstOff, count uint64) io.Reader {
	var buf bytes.Buffer
	writeStateid(&buf, src)
	writeStateid(&buf, dst)
	_ = xdr.WriteUint64(&buf, srcOff)
	_ = xdr.WriteUint64(&buf, dstOff)
	_ = xdr.WriteUint64(&buf, count)
	return bytes.NewReader(buf.Bytes())
}

// cloneCtx builds a compound context with both CURRENT_FH (destination) and
// SAVED_FH (source) populated, as a CLONE compound would have after PUTFH/SAVEFH.
func cloneCtx(current, saved []byte) *types.CompoundContext {
	ctx := xattrCtx(current)
	ctx.SavedFH = saved
	return ctx
}

func TestDecodeCloneArgs(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		src, dst, so, do, c, st := decodeCloneArgs(encCloneArgs(anonStateid(), anonStateid(), 100, 200, 300))
		if st != types.NFS4_OK {
			t.Fatalf("status = %d, want OK", st)
		}
		if src == nil || dst == nil || so != 100 || do != 200 || c != 300 {
			t.Errorf("decoded (so=%d do=%d c=%d)", so, do, c)
		}
	})

	t.Run("whole-file (count 0)", func(t *testing.T) {
		_, _, so, do, c, st := decodeCloneArgs(encCloneArgs(anonStateid(), anonStateid(), 0, 0, 0))
		if st != types.NFS4_OK || so != 0 || do != 0 || c != 0 {
			t.Fatalf("decoded (so=%d do=%d c=%d st=%d)", so, do, c, st)
		}
	})

	t.Run("src overflow -> INVAL", func(t *testing.T) {
		_, _, _, _, _, st := decodeCloneArgs(encCloneArgs(anonStateid(), anonStateid(), ^uint64(0)-5, 0, 100))
		if st != types.NFS4ERR_INVAL {
			t.Fatalf("status = %d, want INVAL", st)
		}
	})

	t.Run("dst overflow -> INVAL", func(t *testing.T) {
		_, _, _, _, _, st := decodeCloneArgs(encCloneArgs(anonStateid(), anonStateid(), 0, ^uint64(0)-5, 100))
		if st != types.NFS4ERR_INVAL {
			t.Fatalf("status = %d, want INVAL", st)
		}
	})

	t.Run("truncated -> BADXDR", func(t *testing.T) {
		var buf bytes.Buffer
		writeStateid(&buf, anonStateid())
		writeStateid(&buf, anonStateid())
		_ = xdr.WriteUint64(&buf, 0) // src offset only; dst offset + count missing
		_, _, _, _, _, st := decodeCloneArgs(bytes.NewReader(buf.Bytes()))
		if st != types.NFS4ERR_BADXDR {
			t.Fatalf("status = %d, want BADXDR", st)
		}
	})
}

func TestHandleClone_Validation(t *testing.T) {
	h := sparseTestHandler(t)

	t.Run("no current filehandle -> NOFILEHANDLE", func(t *testing.T) {
		ctx := cloneCtx(realHandle, realHandle)
		ctx.CurrentFH = nil
		res := h.handleClone(ctx, encCloneArgs(anonStateid(), anonStateid(), 0, 0, 0))
		if res.Status != types.NFS4ERR_NOFILEHANDLE {
			t.Fatalf("status = %d, want NOFILEHANDLE", res.Status)
		}
		if res.OpCode != types.OP_CLONE {
			t.Errorf("opcode = %d, want OP_CLONE", res.OpCode)
		}
	})

	t.Run("no saved filehandle -> NOFILEHANDLE", func(t *testing.T) {
		ctx := cloneCtx(realHandle, nil)
		res := h.handleClone(ctx, encCloneArgs(anonStateid(), anonStateid(), 0, 0, 0))
		if res.Status != types.NFS4ERR_NOFILEHANDLE {
			t.Fatalf("status = %d, want NOFILEHANDLE", res.Status)
		}
	})

	t.Run("pseudo-fs destination -> ROFS", func(t *testing.T) {
		root := h.PseudoFS.GetRootHandle()
		res := h.handleClone(cloneCtx(root, realHandle), encCloneArgs(anonStateid(), anonStateid(), 0, 0, 0))
		if res.Status != types.NFS4ERR_ROFS {
			t.Fatalf("status = %d, want ROFS", res.Status)
		}
	})

	t.Run("pseudo-fs source -> ROFS", func(t *testing.T) {
		root := h.PseudoFS.GetRootHandle()
		res := h.handleClone(cloneCtx(realHandle, root), encCloneArgs(anonStateid(), anonStateid(), 0, 0, 0))
		if res.Status != types.NFS4ERR_ROFS {
			t.Fatalf("status = %d, want ROFS", res.Status)
		}
	})

	t.Run("truncated args -> BADXDR", func(t *testing.T) {
		res := h.handleClone(cloneCtx(realHandle, realHandle), bytes.NewReader([]byte{0x00, 0x01}))
		if res.Status != types.NFS4ERR_BADXDR {
			t.Fatalf("status = %d, want BADXDR", res.Status)
		}
		if res.OpCode != types.OP_CLONE {
			t.Errorf("opcode = %d, want OP_CLONE", res.OpCode)
		}
	})
}

func TestCloneErr(t *testing.T) {
	res := cloneErr(types.NFS4ERR_INVAL)
	if res.Status != types.NFS4ERR_INVAL || res.OpCode != types.OP_CLONE {
		t.Fatalf("cloneErr envelope: status=%d op=%d", res.Status, res.OpCode)
	}
	r := bytes.NewReader(res.Data)
	st, _ := xdr.DecodeUint32(r)
	if st != types.NFS4ERR_INVAL {
		t.Errorf("encoded status = %d, want INVAL", st)
	}
}
