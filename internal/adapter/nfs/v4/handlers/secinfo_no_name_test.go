package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// SECINFO_NO_NAME
// ============================================================================

// encodeSecInfoNoNameArgs encodes SECINFO_NO_NAME4args: the style enum.
func encodeSecInfoNoNameArgs(style uint32) []byte {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, style)
	return buf.Bytes()
}

// newPseudoFSRootContext returns a handler serving a pseudo-fs with one
// /export junction, plus a context whose current filehandle is the root of
// that tree.
func newPseudoFSRootContext(t *testing.T) (*Handler, *types.CompoundContext) {
	t.Helper()

	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		CurrentFH:  append([]byte(nil), pfs.GetRootHandle()...),
	}
	return NewHandler(nil, pfs), ctx
}

func TestHandleSecInfoNoName_CurrentFHConsumesFH(t *testing.T) {
	h, ctx := newPseudoFSRootContext(t)

	args := encodeSecInfoNoNameArgs(types.SECINFO_STYLE4_CURRENT_FH)
	result := h.handleSecInfoNoName(ctx, nil, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SECINFO_NO_NAME(CURRENT_FH) status = %d, want NFS4_OK", result.Status)
	}
	if result.OpCode != types.OP_SECINFO_NO_NAME {
		t.Errorf("opcode = %d, want OP_SECINFO_NO_NAME (%d)", result.OpCode, types.OP_SECINFO_NO_NAME)
	}
	if ctx.CurrentFH != nil {
		t.Error("CurrentFH should be consumed by SECINFO_NO_NAME")
	}

	// The body is a SECINFO4res: NFS4_OK followed by the flavor array.
	reader := bytes.NewReader(result.Data)
	status, _ := xdr.DecodeUint32(reader)
	if status != types.NFS4_OK {
		t.Errorf("encoded status = %d, want NFS4_OK", status)
	}
	arrayLen, _ := xdr.DecodeUint32(reader)
	if arrayLen != 2 {
		t.Errorf("flavor array length = %d, want 2", arrayLen)
	}
}

func TestHandleSecInfoNoName_ParentOfRootIsNoEnt(t *testing.T) {
	h, ctx := newPseudoFSRootContext(t)

	args := encodeSecInfoNoNameArgs(types.SECINFO_STYLE4_PARENT)
	result := h.handleSecInfoNoName(ctx, nil, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOENT {
		t.Errorf("SECINFO_NO_NAME(PARENT) at the pseudo-fs root status = %d, want NFS4ERR_NOENT (%d)",
			result.Status, types.NFS4ERR_NOENT)
	}
}

func TestHandleSecInfoNoName_ParentOfShareRootSucceeds(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = append([]byte(nil), fx.rootHandle...)

	args := encodeSecInfoNoNameArgs(types.SECINFO_STYLE4_PARENT)
	result := fx.handler.handleSecInfoNoName(ctx, nil, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SECINFO_NO_NAME(PARENT) of a share root status = %d, want NFS4_OK", result.Status)
	}
	if ctx.CurrentFH != nil {
		t.Error("CurrentFH should be consumed by SECINFO_NO_NAME")
	}
}

func TestHandleSecInfoNoName_ParentOfFileIsNotDir(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "hello.txt", metadata.FileTypeRegular, 0o644, 0, 0)

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = append([]byte(nil), fileHandle...)

	args := encodeSecInfoNoNameArgs(types.SECINFO_STYLE4_PARENT)
	result := fx.handler.handleSecInfoNoName(ctx, nil, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOTDIR {
		t.Errorf("SECINFO_NO_NAME(PARENT) of a file status = %d, want NFS4ERR_NOTDIR (%d)",
			result.Status, types.NFS4ERR_NOTDIR)
	}
}

func TestHandleSecInfoNoName_NoCurrentFH(t *testing.T) {
	h, ctx := newPseudoFSRootContext(t)
	ctx.CurrentFH = nil

	args := encodeSecInfoNoNameArgs(types.SECINFO_STYLE4_CURRENT_FH)
	result := h.handleSecInfoNoName(ctx, nil, bytes.NewReader(args))

	if result.Status != types.NFS4ERR_NOFILEHANDLE {
		t.Errorf("SECINFO_NO_NAME without a filehandle status = %d, want NFS4ERR_NOFILEHANDLE (%d)",
			result.Status, types.NFS4ERR_NOFILEHANDLE)
	}
}

func TestHandleSecInfoNoName_UnknownStyleIsInval(t *testing.T) {
	h, ctx := newPseudoFSRootContext(t)

	result := h.handleSecInfoNoName(ctx, nil, bytes.NewReader(encodeSecInfoNoNameArgs(2)))

	if result.Status != types.NFS4ERR_INVAL {
		t.Errorf("SECINFO_NO_NAME with an unknown style status = %d, want NFS4ERR_INVAL (%d)",
			result.Status, types.NFS4ERR_INVAL)
	}
}

func TestHandleSecInfoNoName_BadXDR(t *testing.T) {
	h, ctx := newPseudoFSRootContext(t)

	result := h.handleSecInfoNoName(ctx, nil, bytes.NewReader([]byte{0x00}))

	if result.Status != types.NFS4ERR_BADXDR {
		t.Errorf("SECINFO_NO_NAME with truncated args status = %d, want NFS4ERR_BADXDR (%d)",
			result.Status, types.NFS4ERR_BADXDR)
	}
}

func TestHandleSecInfoNoName_RefusedFlavorStillAnswers(t *testing.T) {
	// Same rule as SECINFO: the flavor policy's refusal is the answer, not an
	// error to hand back.
	fx := newRealFSTestFixture(t, "/export")

	if err := fx.rt.SetExportAuthPolicyForTesting("/export", true, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	ctx := newRealFSContext(0, 0)
	ctx.CurrentFH = append([]byte(nil), fx.rootHandle...)

	args := encodeSecInfoNoNameArgs(types.SECINFO_STYLE4_PARENT)
	result := fx.handler.handleSecInfoNoName(ctx, nil, bytes.NewReader(args))

	if result.Status != types.NFS4_OK {
		t.Fatalf("SECINFO_NO_NAME(PARENT) on a Kerberos-only share over AUTH_SYS status = %d, want NFS4_OK",
			result.Status)
	}
}
