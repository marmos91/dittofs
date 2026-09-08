package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestCompoundV41_ReadRejectsAnotherClientsStateid drives the owner-to-client
// binding end to end: two clients each get their own EXCHANGE_ID and
// CREATE_SESSION, one opens a file, and the other presents the resulting open
// stateid to READ.
//
// The StateManager tests hand ValidateStateid a client ID directly, so they
// prove the guard works but not that the handlers reach it. This covers the
// wiring — SEQUENCE putting the session's client on the compound context, and
// the READ call site passing it through — which is what silently breaks when a
// new I/O call site is added or the context is refactored.
func TestCompoundV41_ReadRejectsAnotherClientsStateid(t *testing.T) {
	const filename = "v41-cross-client-read.txt"

	fx := newIOTestFixture(t, "/export")
	sessionA := createSessionOn(t, fx.handler, "cross-client-a")
	sessionB := createSessionOn(t, fx.handler, "cross-client-b")

	// Client A opens the file and keeps the stateid.
	openArgs := encodeOpenArgs(
		0, types.OPEN4_SHARE_ACCESS_BOTH, types.OPEN4_SHARE_DENY_NONE,
		0, []byte("owner-a"),
		types.OPEN4_CREATE, types.UNCHECKED4, types.CLAIM_NULL, filename,
	)
	resp := processV41Compound(t, fx.handler, "open", fx.rootHandle, []compoundOp{
		{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionA, 0, 1, 0, false)},
		{opCode: types.OP_OPEN, data: openArgs},
	})
	openStateid, err := types.DecodeStateid4(skipToLastResult(t, resp, 2, types.OP_OPEN))
	if err != nil {
		t.Fatalf("decode OPEN stateid: %v", err)
	}

	file, err := fx.metaSvc.Lookup(newTestAuthCtx(0, 0), fx.rootHandle, filename)
	if err != nil {
		t.Fatalf("lookup %s: %v", filename, err)
	}
	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("encode handle: %v", err)
	}

	readArgs := encodeReadArgs(openStateid, 0, 1024)

	// Client B presenting client A's stateid must be refused.
	if got := readStatusV41(t, fx.handler, fileHandle, sessionB, 1, readArgs); got != types.NFS4ERR_BAD_STATEID {
		t.Errorf("READ from the other client = %d, want NFS4ERR_BAD_STATEID (%d)", got, types.NFS4ERR_BAD_STATEID)
	}

	// The owning client must still be able to use it.
	if got := readStatusV41(t, fx.handler, fileHandle, sessionA, 2, readArgs); got != types.NFS4_OK {
		t.Errorf("READ from the owning client = %d, want NFS4_OK", got)
	}
}

// readStatusV41 runs SEQUENCE + READ on the given session and returns the READ
// operation's status. A COMPOUND reports the status of its last executed
// operation as the overall status, so with both results present that is READ's
// own. It deliberately does not assert success: the refusal case is the point.
func readStatusV41(t *testing.T, h *Handler, fh metadata.FileHandle, sessionID types.SessionId4, slotSeq uint32, readArgs []byte) uint32 {
	t.Helper()

	ctx := newTestCompoundContext()
	ctx.CurrentFH = make([]byte, len(fh))
	copy(ctx.CurrentFH, fh)

	resp, err := h.ProcessCompound(ctx, buildCompoundArgsWithOps([]byte("read"), 1, []compoundOp{
		{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionID, 0, slotSeq, 0, false)},
		{opCode: types.OP_READ, data: readArgs},
	}))
	if err != nil {
		t.Fatalf("READ COMPOUND error: %v", err)
	}

	reader := bytes.NewReader(resp)
	status, _ := xdr.DecodeUint32(reader)
	_, _ = xdr.DecodeOpaque(reader) // tag
	numResults, _ := xdr.DecodeUint32(reader)
	if numResults != 2 {
		t.Fatalf("numResults = %d, want 2 (READ did not run; SEQUENCE status %d)", numResults, status)
	}
	return status
}
