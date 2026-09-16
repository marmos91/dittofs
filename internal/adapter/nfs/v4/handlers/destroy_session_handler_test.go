package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

func TestHandleDestroySession_BadXDR(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Truncated args -- not enough for a 16-byte session ID
	truncatedArgs := []byte{0x00, 0x00, 0x00, 0x01}
	ops := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: truncatedArgs}}
	data := buildCompoundArgsWithOps([]byte("badxdr"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_BADXDR {
		t.Errorf("status = %d, want NFS4ERR_BADXDR (%d)",
			decoded.Status, types.NFS4ERR_BADXDR)
	}
	if decoded.NumResults != 1 {
		t.Fatalf("numResults = %d, want 1", decoded.NumResults)
	}
	if decoded.Results[0].OpCode != types.OP_DESTROY_SESSION {
		t.Errorf("result opcode = %d, want OP_DESTROY_SESSION", decoded.Results[0].OpCode)
	}
}

func TestHandleDestroySession_NonExistent(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Destroy a session that was never created
	var fakeSID types.SessionId4
	copy(fakeSID[:], []byte("fakesession12345"))

	var buf bytes.Buffer
	dsArgs := types.DestroySessionArgs{SessionID: fakeSID}
	_ = dsArgs.Encode(&buf)

	ops := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: buf.Bytes()}}
	data := buildCompoundArgsWithOps([]byte("noexist"), 1, ops)

	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	decoded, err := decodeCompoundResponse(resp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4ERR_BADSESSION {
		t.Errorf("status = %d, want NFS4ERR_BADSESSION (%d)",
			decoded.Status, types.NFS4ERR_BADSESSION)
	}
}

// TestHandleDestroySession_AnonymousStandalone_Rejected is the security
// regression guard for the audit attack: an anonymous client sends a bare
// (no SEQUENCE) DESTROY_SESSION targeting a victim's session over a connection
// that is not bound to any session and carries no v4.0 client state. The server
// MUST refuse, otherwise the victim's session is destroyed by an unrelated
// caller (RFC 8881 Section 18.37.3 -- the requester must own the target).
//
// Before the ownership fix this asserted NFS4_OK (the vulnerable behavior);
// it now asserts the destroy is rejected and the session survives.
func TestHandleDestroySession_AnonymousStandalone_Rejected(t *testing.T) {
	// Victim creates a session over connection 9100 (auto-bound at CREATE_SESSION).
	h, victimSession := createTestSessionWithConnectionID(t, 9100)

	// Attacker: standalone DESTROY_SESSION(victimSession) over an unbound
	// connection with no client state -- a fully anonymous caller.
	attackerCtx := newTestCompoundContext()
	attackerCtx.ConnectionID = 0 // not bound to any session
	attackerCtx.ClientState = nil

	var dsBuf bytes.Buffer
	dsArgs := types.DestroySessionArgs{SessionID: victimSession}
	_ = dsArgs.Encode(&dsBuf)
	dsOps := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: dsBuf.Bytes()}}
	dsData := buildCompoundArgsWithOps([]byte("attack"), 1, dsOps)
	dsResp, err := h.ProcessCompound(attackerCtx, dsData)
	if err != nil {
		t.Fatalf("DESTROY_SESSION error: %v", err)
	}

	decoded, err := decodeCompoundResponse(dsResp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status == types.NFS4_OK {
		t.Fatalf("anonymous standalone DESTROY_SESSION was accepted (status NFS4_OK): victim session destroyed -- vulnerability present")
	}
	if decoded.Results[0].Status != types.NFS4ERR_OP_NOT_IN_SESSION {
		t.Errorf("DESTROY_SESSION status = %d, want NFS4ERR_OP_NOT_IN_SESSION (%d)",
			decoded.Results[0].Status, types.NFS4ERR_OP_NOT_IN_SESSION)
	}

	// The victim's session must still exist and still be destroyable by its owner.
	if h.StateManager.GetSession(victimSession) == nil {
		t.Fatal("victim session was destroyed by the anonymous caller")
	}
}

// TestHandleDestroySession_OwnerStandalone_OverBoundConnection covers the
// legitimate session-exempt path: the owner sends a standalone (no SEQUENCE)
// DESTROY_SESSION for its own session over a connection that is bound to that
// session. The server authorizes via the connection binding and destroys it.
func TestHandleDestroySession_OwnerStandalone_OverBoundConnection(t *testing.T) {
	// Owner creates a session over connection 9200; CREATE_SESSION auto-binds it.
	h, ownerSession := createTestSessionWithConnectionID(t, 9200)

	// Standalone DESTROY_SESSION(ownerSession) over the same bound connection,
	// no SEQUENCE op. Authorization comes from the connection->session binding.
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 9200

	var dsBuf bytes.Buffer
	dsArgs := types.DestroySessionArgs{SessionID: ownerSession}
	_ = dsArgs.Encode(&dsBuf)
	dsOps := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: dsBuf.Bytes()}}
	dsData := buildCompoundArgsWithOps([]byte("self"), 1, dsOps)
	dsResp, err := h.ProcessCompound(ctx, dsData)
	if err != nil {
		t.Fatalf("DESTROY_SESSION error: %v", err)
	}

	decoded, err := decodeCompoundResponse(dsResp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}

	if decoded.Status != types.NFS4_OK {
		t.Errorf("owner standalone DESTROY_SESSION status = %d, want NFS4_OK", decoded.Status)
	}
	if decoded.Results[0].OpCode != types.OP_DESTROY_SESSION {
		t.Errorf("result opcode = %d, want OP_DESTROY_SESSION", decoded.Results[0].OpCode)
	}
	if h.StateManager.GetSession(ownerSession) != nil {
		t.Error("session still exists after the owner destroyed it")
	}
}

// TestHandleDestroySession_OwnerStandalone_OverDifferentConnection pins that
// same-client ownership does not stand in for the connection binding. The owner
// has two sessions on two connections and destroys session A over the
// connection bound only to session B.
//
// RFC 8881 Section 18.37.3: "DESTROY_SESSION MUST be invoked on a connection
// that is associated with the session being destroyed." A sibling connection
// bound only to another of the owner's sessions is not that connection, so a
// standalone destroy over it is refused even though the same client owns both.
// Over session A's own connection the identical request succeeds, which is what
// makes the refusal about the binding rather than about the request.
func TestHandleDestroySession_OwnerStandalone_OverDifferentConnection(t *testing.T) {
	h := newTestHandler()

	clientID, seqID := registerExchangeID(t, h, "ds-multi-session-owner")
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}

	createSession := func(connID uint64, seq uint32, tag string) types.SessionId4 {
		ctx := newTestCompoundContext()
		ctx.ConnectionID = connID
		csArgs := encodeCreateSessionArgsWithSec(clientID, seq, 0, secParms)
		ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
		data := buildCompoundArgsWithOps([]byte(tag), 1, ops)
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("CREATE_SESSION %s error: %v", tag, err)
		}
		reader := bytes.NewReader(resp)
		_, _ = xdr.DecodeUint32(reader) // overall status
		_, _ = xdr.DecodeOpaque(reader) // tag
		_, _ = xdr.DecodeUint32(reader) // numResults
		_, _ = xdr.DecodeUint32(reader) // opcode
		var csRes types.CreateSessionRes
		if err := csRes.Decode(reader); err != nil {
			t.Fatalf("decode CreateSessionRes %s: %v", tag, err)
		}
		if csRes.Status != types.NFS4_OK {
			t.Fatalf("CREATE_SESSION %s status = %d", tag, csRes.Status)
		}
		return csRes.SessionID
	}

	// Same client, two sessions on two connections. CREATE_SESSION increments
	// the EXCHANGE_ID sequence, so the second uses seqID+1.
	sessionA := createSession(9300, seqID, "csa")
	sessionB := createSession(9301, seqID+1, "csb")

	// Standalone DESTROY_SESSION(sessionA) over connection 9301 (bound to B).
	ctx := newTestCompoundContext()
	ctx.ConnectionID = 9301

	var dsBuf bytes.Buffer
	dsArgs := types.DestroySessionArgs{SessionID: sessionA}
	_ = dsArgs.Encode(&dsBuf)
	dsOps := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: dsBuf.Bytes()}}
	dsData := buildCompoundArgsWithOps([]byte("cross-own"), 1, dsOps)
	dsResp, err := h.ProcessCompound(ctx, dsData)
	if err != nil {
		t.Fatalf("DESTROY_SESSION error: %v", err)
	}

	decoded, err := decodeCompoundResponse(dsResp)
	if err != nil {
		t.Fatalf("decode response error: %v", err)
	}
	if decoded.Status == types.NFS4_OK {
		t.Error("standalone DESTROY_SESSION over a connection not bound to the target should be refused")
	}
	if h.StateManager.GetSession(sessionA) == nil {
		t.Error("session A should survive a destroy over a sibling connection")
	}
	if h.StateManager.GetSession(sessionB) == nil {
		t.Error("session B should be unaffected")
	}

	// Over the connection the target session is actually bound to, it works.
	ctxOwn := newTestCompoundContext()
	ctxOwn.ConnectionID = 9300
	var ownBuf bytes.Buffer
	ownArgs := types.DestroySessionArgs{SessionID: sessionA}
	_ = ownArgs.Encode(&ownBuf)
	ownOps := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: ownBuf.Bytes()}}
	ownResp, err := h.ProcessCompound(ctxOwn, buildCompoundArgsWithOps([]byte("own-conn"), 1, ownOps))
	if err != nil {
		t.Fatalf("DESTROY_SESSION over own connection error: %v", err)
	}
	ownDecoded, err := decodeCompoundResponse(ownResp)
	if err != nil {
		t.Fatalf("decode own-connection response: %v", err)
	}
	if ownDecoded.Status != types.NFS4_OK {
		t.Errorf("destroy over the target's own connection status = %d, want NFS4_OK", ownDecoded.Status)
	}
	if h.StateManager.GetSession(sessionA) != nil {
		t.Error("session A should be destroyed over its own connection")
	}
}

func TestHandleDestroySession_OwnershipCheck_NFS4ERR_NOT_SAME(t *testing.T) {
	h := newTestHandler()
	ctx := newTestCompoundContext()

	// Client A: register + create session
	clientAID, seqA := registerExchangeID(t, h, "ds-owner-a")
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}
	csArgsA := encodeCreateSessionArgsWithSec(clientAID, seqA, 0, secParms)
	csOpsA := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgsA}}
	csDataA := buildCompoundArgsWithOps([]byte("cs-a"), 1, csOpsA)
	csRespA, err := h.ProcessCompound(ctx, csDataA)
	if err != nil {
		t.Fatalf("CREATE_SESSION A error: %v", err)
	}
	readerA := bytes.NewReader(csRespA)
	_, _ = xdr.DecodeUint32(readerA) // overall status
	_, _ = xdr.DecodeOpaque(readerA) // tag
	_, _ = xdr.DecodeUint32(readerA) // numResults
	_, _ = xdr.DecodeUint32(readerA) // opcode
	var csResA types.CreateSessionRes
	if err := csResA.Decode(readerA); err != nil {
		t.Fatalf("decode CreateSessionRes A: %v", err)
	}
	sessionA := csResA.SessionID

	// Client B: register + create session
	clientBID, seqB := registerExchangeID(t, h, "ds-attacker-b")
	csArgsB := encodeCreateSessionArgsWithSec(clientBID, seqB, 0, secParms)
	csOpsB := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgsB}}
	csDataB := buildCompoundArgsWithOps([]byte("cs-b"), 1, csOpsB)
	csRespB, err := h.ProcessCompound(ctx, csDataB)
	if err != nil {
		t.Fatalf("CREATE_SESSION B error: %v", err)
	}
	readerB := bytes.NewReader(csRespB)
	_, _ = xdr.DecodeUint32(readerB)
	_, _ = xdr.DecodeOpaque(readerB)
	_, _ = xdr.DecodeUint32(readerB)
	_, _ = xdr.DecodeUint32(readerB)
	var csResB types.CreateSessionRes
	if err := csResB.Decode(readerB); err != nil {
		t.Fatalf("decode CreateSessionRes B: %v", err)
	}
	sessionB := csResB.SessionID

	// Client B issues: SEQUENCE(sessionB) + DESTROY_SESSION(sessionA)
	seqArgs := encodeSequenceArgs(sessionB, 0, 1, 0, true)
	var dsBuf bytes.Buffer
	dsArgs := types.DestroySessionArgs{SessionID: sessionA}
	_ = dsArgs.Encode(&dsBuf)
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_DESTROY_SESSION, data: dsBuf.Bytes()},
	}
	data := buildCompoundArgsWithOps([]byte("cross-destroy"), 1, ops)
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	// Decode: skip overall status + SEQUENCE result, examine DESTROY_SESSION result
	reader := bytes.NewReader(resp)
	overallStatus, _ := xdr.DecodeUint32(reader)
	if overallStatus == types.NFS4_OK {
		t.Fatal("overall status should be non-OK (DESTROY_SESSION must fail)")
	}
	_, _ = xdr.DecodeOpaque(reader) // tag
	numResults, _ := xdr.DecodeUint32(reader)
	if numResults < 2 {
		t.Fatalf("numResults = %d, want >= 2 (SEQUENCE + DESTROY_SESSION)", numResults)
	}
	// Skip SEQUENCE result
	_, _ = xdr.DecodeUint32(reader) // opcode
	var seqRes types.SequenceRes
	if err := seqRes.Decode(reader); err != nil {
		t.Fatalf("decode SequenceRes: %v", err)
	}
	if seqRes.Status != types.NFS4_OK {
		t.Fatalf("SEQUENCE status = %d, want NFS4_OK", seqRes.Status)
	}
	// Read DESTROY_SESSION result
	opCode, _ := xdr.DecodeUint32(reader)
	if opCode != types.OP_DESTROY_SESSION {
		t.Errorf("result opcode = %d, want OP_DESTROY_SESSION (%d)", opCode, types.OP_DESTROY_SESSION)
	}
	dsStatus, _ := xdr.DecodeUint32(reader)
	if dsStatus != types.NFS4ERR_NOT_SAME {
		t.Errorf("DESTROY_SESSION status = %d, want NFS4ERR_NOT_SAME (%d)", dsStatus, types.NFS4ERR_NOT_SAME)
	}
}

func TestHandleDestroySession_OwnerCanDestroy_WithSequence(t *testing.T) {
	// Owner destroys their own session via a SEQUENCE-prefixed compound. The
	// ownership check must NOT fire (requesting client == owning client), so the
	// result must not be NFS4ERR_NOT_SAME. Destroying a session from within a
	// SEQUENCE on that same session legitimately returns NFS4ERR_DELAY because the
	// SEQUENCE itself holds an in-flight slot; this guards that the ownership
	// check doesn't reject the legitimate owner path.
	h, sessionID := createTestSession(t)
	ctx := newTestCompoundContext()

	seqArgs := encodeSequenceArgs(sessionID, 0, 1, 0, true)
	var dsBuf bytes.Buffer
	dsArgs := types.DestroySessionArgs{SessionID: sessionID}
	_ = dsArgs.Encode(&dsBuf)
	ops := []compoundOp{
		{opCode: types.OP_SEQUENCE, data: seqArgs},
		{opCode: types.OP_DESTROY_SESSION, data: dsBuf.Bytes()},
	}
	data := buildCompoundArgsWithOps([]byte("self-destroy"), 1, ops)
	resp, err := h.ProcessCompound(ctx, data)
	if err != nil {
		t.Fatalf("ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	_, _ = xdr.DecodeUint32(reader) // overall status
	_, _ = xdr.DecodeOpaque(reader) // tag
	numResults, _ := xdr.DecodeUint32(reader)
	if numResults < 2 {
		t.Fatalf("numResults = %d, want 2 (SEQUENCE + DESTROY_SESSION)", numResults)
	}
	// Skip SEQUENCE
	_, _ = xdr.DecodeUint32(reader)
	var seqRes types.SequenceRes
	_ = seqRes.Decode(reader)
	// Read DESTROY_SESSION
	opCode, _ := xdr.DecodeUint32(reader)
	if opCode != types.OP_DESTROY_SESSION {
		t.Errorf("result opcode = %d, want OP_DESTROY_SESSION", opCode)
	}
	dsStatus, _ := xdr.DecodeUint32(reader)
	if dsStatus == types.NFS4ERR_NOT_SAME {
		t.Errorf("DESTROY_SESSION status = NFS4ERR_NOT_SAME (%d): ownership check wrongly rejected the owning client",
			types.NFS4ERR_NOT_SAME)
	}
}

// TestHandleDestroySession_SharedConnection covers a connection carrying
// sessions of two clients, which RFC 8881 Section 2.10.3.1 allows. Each is
// destroyable over it because the connection is associated with each, and
// reading the requester off whichever session bound last would refuse the one
// that did not.
func TestHandleDestroySession_SharedConnection(t *testing.T) {
	h := newTestHandler()
	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}}

	createSession := func(connID uint64, clientID uint64, seq uint32, tag string) types.SessionId4 {
		ctx := newTestCompoundContext()
		ctx.ConnectionID = connID
		csArgs := encodeCreateSessionArgsWithSec(clientID, seq, 0, secParms)
		ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
		data := buildCompoundArgsWithOps([]byte(tag), 1, ops)
		resp, err := h.ProcessCompound(ctx, data)
		if err != nil {
			t.Fatalf("CREATE_SESSION %s error: %v", tag, err)
		}
		reader := bytes.NewReader(resp)
		_, _ = xdr.DecodeUint32(reader) // overall status
		_, _ = xdr.DecodeOpaque(reader) // tag
		_, _ = xdr.DecodeUint32(reader) // numResults
		_, _ = xdr.DecodeUint32(reader) // opcode
		var csRes types.CreateSessionRes
		if err := csRes.Decode(reader); err != nil {
			t.Fatalf("decode CreateSessionRes %s: %v", tag, err)
		}
		if csRes.Status != types.NFS4_OK {
			t.Fatalf("CREATE_SESSION %s status = %d", tag, csRes.Status)
		}
		return csRes.SessionID
	}

	destroyOverConn := func(connID uint64, target types.SessionId4, tag string) uint32 {
		ctx := newTestCompoundContext()
		ctx.ConnectionID = connID
		var dsBuf bytes.Buffer
		dsArgs := types.DestroySessionArgs{SessionID: target}
		_ = dsArgs.Encode(&dsBuf)
		ops := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: dsBuf.Bytes()}}
		resp, err := h.ProcessCompound(ctx, buildCompoundArgsWithOps([]byte(tag), 1, ops))
		if err != nil {
			t.Fatalf("DESTROY_SESSION %s error: %v", tag, err)
		}
		decoded, err := decodeCompoundResponse(resp)
		if err != nil {
			t.Fatalf("decode %s response: %v", tag, err)
		}
		return decoded.Status
	}

	// Clients A and B both put a session on connection 9400, A's first.
	clientAID, seqA := registerExchangeID(t, h, "ds-shared-conn-a")
	clientBID, seqB := registerExchangeID(t, h, "ds-shared-conn-b")
	sessionA := createSession(9400, clientAID, seqA, "cs-a")
	sessionB := createSession(9400, clientBID, seqB, "cs-b")

	// A session on a connection of its own, owned by a third client.
	clientCID, seqC := registerExchangeID(t, h, "ds-shared-conn-c")
	sessionC := createSession(9401, clientCID, seqC, "cs-c")

	// A's session bound the shared connection first, B's last. Both are
	// associated with it, so both are destroyable over it.
	if status := destroyOverConn(9400, sessionA, "older-binding"); status != types.NFS4_OK {
		t.Errorf("destroy of the older binding status = %d, want NFS4_OK", status)
	}
	if h.StateManager.GetSession(sessionA) != nil {
		t.Error("session A should be destroyed")
	}
	if status := destroyOverConn(9400, sessionB, "newer-binding"); status != types.NFS4_OK {
		t.Errorf("destroy of the newer binding status = %d, want NFS4_OK", status)
	}

	// A session bound to no connection here is not destroyable over it.
	if status := destroyOverConn(9400, sessionC, "unbound-elsewhere"); status == types.NFS4_OK {
		t.Error("destroy of a session not bound to this connection should be refused")
	}
	if h.StateManager.GetSession(sessionC) == nil {
		t.Error("session C should survive")
	}
}
