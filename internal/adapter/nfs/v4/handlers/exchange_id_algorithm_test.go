package handlers

import (
	"bytes"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// The tests below drive the whole COMPOUND path (ProcessCompound), which is
// what the RPC layer calls, so the flag validation, the record algorithm and
// the CREATE_SESSION confirmation phase are all exercised as a client sees
// them. They cover the EXCHANGE_ID cases of RFC 8881 Section 18.35.4 and the
// confirmation phase of Section 18.36.3.

// eidVerifier turns a short string into a co_verifier.
func eidVerifier(s string) [8]byte {
	var v [8]byte
	copy(v[:], s)
	return v
}

// ctxForUID returns a compound context whose AUTH_UNIX credential maps to uid,
// so EXCHANGE_ID and CREATE_SESSION see it as a distinct principal.
func ctxForUID(uid uint32, connID uint64) *types.CompoundContext {
	ctx := newTestCompoundContext()
	ctx.UID = &uid
	ctx.ConnectionID = connID
	return ctx
}

// doExchangeID sends a single-op EXCHANGE_ID COMPOUND and returns the decoded
// result. The operation status is returned in the result, not asserted here,
// so error cases can be checked by the caller.
func doExchangeID(
	t *testing.T,
	h *Handler,
	ctx *types.CompoundContext,
	ownerID string,
	verifier [8]byte,
	flags uint32,
) types.ExchangeIdRes {
	t.Helper()

	args := encodeExchangeIdArgs([]byte(ownerID), verifier, flags, types.SP4_NONE, nil)
	ops := []compoundOp{{opCode: types.OP_EXCHANGE_ID, data: args}}
	resp, err := h.ProcessCompound(ctx, buildCompoundArgsWithOps([]byte("eid"), 1, ops))
	if err != nil {
		t.Fatalf("EXCHANGE_ID ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	if _, err := xdr.DecodeUint32(reader); err != nil { // overall status
		t.Fatalf("decode overall status: %v", err)
	}
	if _, err := xdr.DecodeOpaque(reader); err != nil { // tag
		t.Fatalf("decode tag: %v", err)
	}
	numResults, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode numResults: %v", err)
	}
	if numResults != 1 {
		t.Fatalf("numResults = %d, want 1", numResults)
	}
	opCode, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode result opcode: %v", err)
	}
	if opCode != types.OP_EXCHANGE_ID {
		t.Fatalf("result opcode = %d, want OP_EXCHANGE_ID (%d)", opCode, types.OP_EXCHANGE_ID)
	}

	var res types.ExchangeIdRes
	if err := res.Decode(reader); err != nil {
		t.Fatalf("decode ExchangeIdRes: %v", err)
	}
	return res
}

// exchangeIDOK sends EXCHANGE_ID and fails the test unless it succeeded.
func exchangeIDOK(
	t *testing.T,
	h *Handler,
	ctx *types.CompoundContext,
	ownerID string,
	verifier [8]byte,
) types.ExchangeIdRes {
	t.Helper()
	res := doExchangeID(t, h, ctx, ownerID, verifier, 0)
	if res.Status != types.NFS4_OK {
		t.Fatalf("EXCHANGE_ID status = %d, want NFS4_OK", res.Status)
	}
	return res
}

// doCreateSession sends a single-op CREATE_SESSION COMPOUND and returns the
// decoded result, leaving the status for the caller to assert.
func doCreateSession(
	t *testing.T,
	h *Handler,
	ctx *types.CompoundContext,
	clientID uint64,
	seqID uint32,
) types.CreateSessionRes {
	t.Helper()

	secParms := []types.CallbackSecParms4{{CbSecFlavor: 0}} // AUTH_NONE
	csArgs := encodeCreateSessionArgsWithSec(clientID, seqID, 0, secParms)
	ops := []compoundOp{{opCode: types.OP_CREATE_SESSION, data: csArgs}}
	resp, err := h.ProcessCompound(ctx, buildCompoundArgsWithOps([]byte("cs"), 1, ops))
	if err != nil {
		t.Fatalf("CREATE_SESSION ProcessCompound error: %v", err)
	}

	reader := bytes.NewReader(resp)
	if _, err := xdr.DecodeUint32(reader); err != nil { // overall status
		t.Fatalf("decode overall status: %v", err)
	}
	if _, err := xdr.DecodeOpaque(reader); err != nil { // tag
		t.Fatalf("decode tag: %v", err)
	}
	if _, err := xdr.DecodeUint32(reader); err != nil { // numResults
		t.Fatalf("decode numResults: %v", err)
	}
	if _, err := xdr.DecodeUint32(reader); err != nil { // opcode
		t.Fatalf("decode result opcode: %v", err)
	}

	var res types.CreateSessionRes
	if err := res.Decode(reader); err != nil {
		t.Fatalf("decode CreateSessionRes: %v", err)
	}
	return res
}

// createSessionOK confirms a client ID and returns the new session ID.
func createSessionOK(
	t *testing.T,
	h *Handler,
	ctx *types.CompoundContext,
	clientID uint64,
	seqID uint32,
) types.SessionId4 {
	t.Helper()
	res := doCreateSession(t, h, ctx, clientID, seqID)
	if res.Status != types.NFS4_OK {
		t.Fatalf("CREATE_SESSION status = %d, want NFS4_OK", res.Status)
	}
	return res.SessionID
}

// sequenceOverallStatus probes a session with a single-op SEQUENCE COMPOUND and
// returns the COMPOUND status, which is NFS4ERR_BADSESSION once the session is
// gone.
func sequenceOverallStatus(
	t *testing.T,
	h *Handler,
	sessionID types.SessionId4,
	slotID, seqID uint32,
) uint32 {
	t.Helper()

	ops := []compoundOp{{opCode: types.OP_SEQUENCE, data: encodeSequenceArgs(sessionID, slotID, seqID, 0, false)}}
	resp, err := h.ProcessCompound(newTestCompoundContext(),
		buildCompoundArgsWithOps([]byte("seq"), 1, ops))
	if err != nil {
		t.Fatalf("SEQUENCE ProcessCompound error: %v", err)
	}
	status, err := xdr.DecodeUint32(bytes.NewReader(resp))
	if err != nil {
		t.Fatalf("decode SEQUENCE overall status: %v", err)
	}
	return status
}

// destroySession sends a standalone DESTROY_SESSION over ctx's connection and
// returns the COMPOUND status.
func destroySession(t *testing.T, h *Handler, ctx *types.CompoundContext, sessionID types.SessionId4) uint32 {
	t.Helper()

	var buf bytes.Buffer
	args := types.DestroySessionArgs{SessionID: sessionID}
	if err := args.Encode(&buf); err != nil {
		t.Fatalf("encode DestroySessionArgs: %v", err)
	}
	ops := []compoundOp{{opCode: types.OP_DESTROY_SESSION, data: buf.Bytes()}}
	resp, err := h.ProcessCompound(ctx, buildCompoundArgsWithOps([]byte("ds"), 1, ops))
	if err != nil {
		t.Fatalf("DESTROY_SESSION ProcessCompound error: %v", err)
	}
	status, err := xdr.DecodeUint32(bytes.NewReader(resp))
	if err != nil {
		t.Fatalf("decode DESTROY_SESSION overall status: %v", err)
	}
	return status
}

// ============================================================================
// eia_flags validation (RFC 8881 Section 18.35.3)
// ============================================================================

func TestExchangeID_UndefinedFlagBitRejected(t *testing.T) {
	h := newTestHandler()

	// Bit 0x4 carries no meaning for a v4.1 client; the server must refuse it
	// rather than register the owner ID.
	res := doExchangeID(t, h, newTestCompoundContext(), "undefined-flag-client",
		eidVerifier("verify01"), 0x4)
	if res.Status != types.NFS4ERR_INVAL {
		t.Fatalf("status for undefined flag bit 0x4 = %d, want NFS4ERR_INVAL (%d)",
			res.Status, types.NFS4ERR_INVAL)
	}

	// Nothing may have been registered by the rejected request.
	if got := len(h.StateManager.ListV41Clients()); got != 0 {
		t.Errorf("registered v4.1 clients = %d, want 0 after a rejected EXCHANGE_ID", got)
	}
}

func TestExchangeID_ReplyOnlyFlagRejected(t *testing.T) {
	h := newTestHandler()

	// EXCHGID4_FLAG_CONFIRMED_R is set by the server in eir_flags only.
	res := doExchangeID(t, h, newTestCompoundContext(), "confirmed-r-client",
		eidVerifier("verify01"),
		types.EXCHGID4_FLAG_USE_NON_PNFS|types.EXCHGID4_FLAG_CONFIRMED_R)
	if res.Status != types.NFS4ERR_INVAL {
		t.Fatalf("status for client-set CONFIRMED_R = %d, want NFS4ERR_INVAL (%d)",
			res.Status, types.NFS4ERR_INVAL)
	}
}

func TestExchangeID_DefinedArgumentFlagsAccepted(t *testing.T) {
	h := newTestHandler()

	flags := uint32(types.EXCHGID4_FLAG_SUPP_MOVED_REFER |
		types.EXCHGID4_FLAG_SUPP_MOVED_MIGR |
		types.EXCHGID4_FLAG_BIND_PRINC_STATEID |
		types.EXCHGID4_FLAG_USE_NON_PNFS)

	res := doExchangeID(t, h, newTestCompoundContext(), "defined-flags-client",
		eidVerifier("verify01"), flags)
	if res.Status != types.NFS4_OK {
		t.Fatalf("status for defined argument flags = %d, want NFS4_OK", res.Status)
	}
	roles := uint32(types.EXCHGID4_FLAG_USE_NON_PNFS |
		types.EXCHGID4_FLAG_USE_PNFS_MDS |
		types.EXCHGID4_FLAG_USE_PNFS_DS)
	if res.Flags&roles == 0 {
		t.Error("eir_flags must carry one of the EXCHGID4_FLAG_USE_* role bits")
	}
}

func TestExchangeID_FenceOpsFlagAcceptedOnV42(t *testing.T) {
	h := newTestHandler()

	// Bit 0x4 is EXCHGID4_FLAG_SUPP_FENCE_OPS, which a v4.2 client may request.
	args := encodeExchangeIdArgs([]byte("fence-ops-client"), eidVerifier("verify01"),
		types.EXCHGID4_FLAG_SUPP_FENCE_OPS, types.SP4_NONE, nil)
	ops := []compoundOp{{opCode: types.OP_EXCHANGE_ID, data: args}}
	resp, err := h.ProcessCompound(newTestCompoundContext(),
		buildCompoundArgsWithOps([]byte("eid42"), 2, ops))
	if err != nil {
		t.Fatalf("EXCHANGE_ID ProcessCompound error: %v", err)
	}
	status, err := xdr.DecodeUint32(bytes.NewReader(resp))
	if err != nil {
		t.Fatalf("decode overall status: %v", err)
	}
	if status != types.NFS4_OK {
		t.Fatalf("v4.2 SUPP_FENCE_OPS request status = %d, want NFS4_OK", status)
	}
}

// ============================================================================
// Update requests: EXCHGID4_FLAG_UPD_CONFIRMED_REC_A (cases 6-9)
// ============================================================================

func TestExchangeID_UpdateWithoutAnyRecord(t *testing.T) {
	h := newTestHandler()

	res := doExchangeID(t, h, newTestCompoundContext(), "update-nonexistent",
		eidVerifier("verify01"), types.EXCHGID4_FLAG_UPD_CONFIRMED_REC_A)
	if res.Status != types.NFS4ERR_NOENT {
		t.Fatalf("update against no record = %d, want NFS4ERR_NOENT (%d)",
			res.Status, types.NFS4ERR_NOENT)
	}
}

func TestExchangeID_UpdateWithUnconfirmedRecord(t *testing.T) {
	// An unconfirmed record is not updatable: there is no confirmed record to
	// update, whatever verifier and principal the update carries.
	tests := []struct {
		name     string
		verifier [8]byte
		uid      uint32
	}{
		{"otherVerifierOtherPrincipal", eidVerifier("verify02"), 2222},
		{"otherVerifierSamePrincipal", eidVerifier("verify02"), 1111},
		{"sameVerifierOtherPrincipal", eidVerifier("verify01"), 2222},
		{"sameVerifierSamePrincipal", eidVerifier("verify01"), 1111},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler()
			owner := "update-unconfirmed"

			first := exchangeIDOK(t, h, ctxForUID(1111, 0), owner, eidVerifier("verify01"))

			res := doExchangeID(t, h, ctxForUID(tt.uid, 0), owner, tt.verifier,
				types.EXCHGID4_FLAG_UPD_CONFIRMED_REC_A)
			if res.Status != types.NFS4ERR_NOENT {
				t.Fatalf("update against unconfirmed record = %d, want NFS4ERR_NOENT (%d)",
					res.Status, types.NFS4ERR_NOENT)
			}

			// The rejected update leaves the unconfirmed record intact, so it
			// can still be confirmed.
			createSessionOK(t, h, ctxForUID(1111, 0), first.ClientID, first.SequenceID)
		})
	}
}

func TestExchangeID_UpdateWrongVerifier(t *testing.T) {
	// A confirmed record cannot be updated from a different incarnation, and
	// the wrong verifier is reported before the principal is even considered.
	tests := []struct {
		name string
		uid  uint32
	}{
		{"otherPrincipal", 2222},
		{"samePrincipal", 1111},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler()
			owner := "update-wrong-verifier"
			ctx := ctxForUID(1111, 0)

			first := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify01"))
			createSessionOK(t, h, ctx, first.ClientID, first.SequenceID)

			res := doExchangeID(t, h, ctxForUID(tt.uid, 0), owner, eidVerifier("verify02"),
				types.EXCHGID4_FLAG_UPD_CONFIRMED_REC_A)
			if res.Status != types.NFS4ERR_NOT_SAME {
				t.Fatalf("update with a different verifier = %d, want NFS4ERR_NOT_SAME (%d)",
					res.Status, types.NFS4ERR_NOT_SAME)
			}
		})
	}
}

func TestExchangeID_UpdateWrongPrincipal(t *testing.T) {
	h := newTestHandler()
	owner := "update-wrong-principal"
	ctx := ctxForUID(1111, 0)

	first := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify01"))
	createSessionOK(t, h, ctx, first.ClientID, first.SequenceID)

	res := doExchangeID(t, h, ctxForUID(2222, 0), owner, eidVerifier("verify01"),
		types.EXCHGID4_FLAG_UPD_CONFIRMED_REC_A)
	if res.Status != types.NFS4ERR_PERM {
		t.Fatalf("update by another principal = %d, want NFS4ERR_PERM (%d)",
			res.Status, types.NFS4ERR_PERM)
	}
}

func TestExchangeID_UpdateConfirmedRecord(t *testing.T) {
	h := newTestHandler()
	owner := "update-confirmed"
	ctx := ctxForUID(1111, 0)

	first := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify01"))
	createSessionOK(t, h, ctx, first.ClientID, first.SequenceID)

	res := doExchangeID(t, h, ctx, owner, eidVerifier("verify01"),
		types.EXCHGID4_FLAG_UPD_CONFIRMED_REC_A)
	if res.Status != types.NFS4_OK {
		t.Fatalf("update of a confirmed record = %d, want NFS4_OK", res.Status)
	}
	if res.ClientID != first.ClientID {
		t.Errorf("client ID changed on update: %d -> %d", first.ClientID, res.ClientID)
	}
	if res.Flags&types.EXCHGID4_FLAG_CONFIRMED_R == 0 {
		t.Error("eir_flags must carry CONFIRMED_R for a confirmed record")
	}
	if res.Flags&types.EXCHGID4_FLAG_UPD_CONFIRMED_REC_A != 0 {
		t.Error("eir_flags must never carry UPD_CONFIRMED_REC_A")
	}
}

// ============================================================================
// Non-update requests (cases 1-5)
// ============================================================================

func TestExchangeID_UnconfirmedRecordIsReplaced(t *testing.T) {
	// Any second EXCHANGE_ID on an unconfirmed record replaces it with a fresh
	// client ID, so the owner ID never has two readings at once.
	tests := []struct {
		name     string
		verifier [8]byte
		uid      uint32
	}{
		{"otherVerifierOtherPrincipal", eidVerifier("verify02"), 2222},
		{"otherVerifierSamePrincipal", eidVerifier("verify02"), 1111},
		{"sameVerifierOtherPrincipal", eidVerifier("verify01"), 2222},
		{"sameVerifierSamePrincipal", eidVerifier("verify01"), 1111},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler()
			owner := "replace-unconfirmed"

			first := exchangeIDOK(t, h, ctxForUID(1111, 0), owner, eidVerifier("verify01"))
			second := exchangeIDOK(t, h, ctxForUID(tt.uid, 0), owner, tt.verifier)

			if second.ClientID == first.ClientID {
				t.Fatalf("client ID unchanged (%d) -- an unconfirmed record must be replaced",
					first.ClientID)
			}
			if second.Flags&types.EXCHGID4_FLAG_CONFIRMED_R != 0 {
				t.Error("a replacement record is unconfirmed and must not carry CONFIRMED_R")
			}

			// The replaced client ID is gone, so it can no longer be confirmed.
			res := doCreateSession(t, h, ctxForUID(1111, 0), first.ClientID, first.SequenceID)
			if res.Status != types.NFS4ERR_STALE_CLIENTID {
				t.Errorf("CREATE_SESSION on the replaced client ID = %d, want NFS4ERR_STALE_CLIENTID (%d)",
					res.Status, types.NFS4ERR_STALE_CLIENTID)
			}
		})
	}
}

func TestExchangeID_RestartKeepsOldStateUntilConfirmed(t *testing.T) {
	h := newTestHandler()
	owner := "restarting-client"
	ctx := ctxForUID(1111, 0)

	first := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify01"))
	oldSession := createSessionOK(t, h, ctx, first.ClientID, first.SequenceID)

	// A new incarnation of the same client: same principal, new verifier.
	second := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify02"))
	if second.ClientID == first.ClientID {
		t.Fatalf("client ID unchanged (%d) -- a restart must get a new one", first.ClientID)
	}
	if second.Flags&types.EXCHGID4_FLAG_CONFIRMED_R != 0 {
		t.Error("the new record is unconfirmed and must not carry CONFIRMED_R")
	}

	// The previous incarnation's session survives until the new client ID is
	// confirmed.
	if got := sequenceOverallStatus(t, h, oldSession, 0, 1); got != types.NFS4_OK {
		t.Fatalf("SEQUENCE on the old session before confirmation = %d, want NFS4_OK", got)
	}

	createSessionOK(t, h, ctx, second.ClientID, second.SequenceID)

	// Confirming the new client ID collapses the two records into one and takes
	// the old incarnation's session with it.
	if got := sequenceOverallStatus(t, h, oldSession, 0, 2); got != types.NFS4ERR_BADSESSION {
		t.Fatalf("SEQUENCE on the old session after confirmation = %d, want NFS4ERR_BADSESSION (%d)",
			got, types.NFS4ERR_BADSESSION)
	}
}

func TestExchangeID_RestartRecordSupersededByFurtherExchangeID(t *testing.T) {
	h := newTestHandler()
	owner := "restart-twice"
	ctx := ctxForUID(1111, 0)

	first := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify01"))
	oldSession := createSessionOK(t, h, ctx, first.ClientID, first.SequenceID)

	second := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify02"))

	// A third EXCHANGE_ID discards the unconfirmed record and is processed
	// against the confirmed one again, so the confirmed record's session is
	// still alive and the second client ID is no longer confirmable.
	third := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify03"))
	if third.ClientID == second.ClientID || third.ClientID == first.ClientID {
		t.Fatalf("third EXCHANGE_ID reused a client ID (%d)", third.ClientID)
	}
	if got := sequenceOverallStatus(t, h, oldSession, 0, 1); got != types.NFS4_OK {
		t.Fatalf("SEQUENCE on the confirmed session = %d, want NFS4_OK", got)
	}
	res := doCreateSession(t, h, ctx, second.ClientID, second.SequenceID)
	if res.Status != types.NFS4ERR_STALE_CLIENTID {
		t.Fatalf("CREATE_SESSION on the discarded client ID = %d, want NFS4ERR_STALE_CLIENTID (%d)",
			res.Status, types.NFS4ERR_STALE_CLIENTID)
	}
}

func TestExchangeID_PrincipalCollisionOnStatelessRecord(t *testing.T) {
	h := newTestHandler()
	owner := "collision-no-state"
	ctx := ctxForUID(1111, 7)

	first := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify01"))
	session := createSessionOK(t, h, ctx, first.ClientID, first.SequenceID)
	if got := destroySession(t, h, ctx, session); got != types.NFS4_OK {
		t.Fatalf("DESTROY_SESSION = %d, want NFS4_OK", got)
	}

	// The confirmed record holds nothing, so a colliding owner ID from another
	// principal takes it over rather than being refused.
	second := exchangeIDOK(t, h, ctxForUID(2222, 8), owner, eidVerifier("verify01"))
	if second.ClientID == first.ClientID {
		t.Fatalf("client ID unchanged (%d) -- a stateless record must be replaced", first.ClientID)
	}
}

func TestExchangeID_PrincipalCollisionOnRecordWithState(t *testing.T) {
	h := newTestHandler()
	owner := "collision-with-state"
	ctx := ctxForUID(1111, 7)

	first := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify01"))
	session := createSessionOK(t, h, ctx, first.ClientID, first.SequenceID)

	// The confirmed record still holds a session under a live lease, so the
	// colliding principal is told to pick another owner ID.
	res := doExchangeID(t, h, ctxForUID(2222, 8), owner, eidVerifier("verify01"), 0)
	if res.Status != types.NFS4ERR_CLID_INUSE {
		t.Fatalf("colliding EXCHANGE_ID = %d, want NFS4ERR_CLID_INUSE (%d)",
			res.Status, types.NFS4ERR_CLID_INUSE)
	}

	// The refused request changed nothing.
	if got := sequenceOverallStatus(t, h, session, 0, 1); got != types.NFS4_OK {
		t.Errorf("SEQUENCE after a refused collision = %d, want NFS4_OK", got)
	}
}

func TestExchangeID_ConfirmedIdempotentReturn(t *testing.T) {
	h := newTestHandler()
	owner := "confirmed-idempotent-compound"
	ctx := ctxForUID(1111, 0)

	first := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify01"))
	createSessionOK(t, h, ctx, first.ClientID, first.SequenceID)

	second := exchangeIDOK(t, h, ctx, owner, eidVerifier("verify01"))
	if second.ClientID != first.ClientID {
		t.Errorf("client ID changed on a repeat EXCHANGE_ID: %d -> %d",
			first.ClientID, second.ClientID)
	}
	if second.Flags&types.EXCHGID4_FLAG_CONFIRMED_R == 0 {
		t.Error("eir_flags must carry CONFIRMED_R for an already confirmed record")
	}
}

// ============================================================================
// CREATE_SESSION confirmation phase (RFC 8881 Section 18.36.3)
// ============================================================================

func TestCreateSession_ConfirmationByOtherPrincipalRejected(t *testing.T) {
	h := newTestHandler()

	first := exchangeIDOK(t, h, ctxForUID(1111, 0), "confirm-other-principal",
		eidVerifier("verify01"))

	res := doCreateSession(t, h, ctxForUID(2222, 0), first.ClientID, first.SequenceID)
	if res.Status != types.NFS4ERR_CLID_INUSE {
		t.Fatalf("confirmation by another principal = %d, want NFS4ERR_CLID_INUSE (%d)",
			res.Status, types.NFS4ERR_CLID_INUSE)
	}

	// No client record changed, so the owning principal can still confirm.
	createSessionOK(t, h, ctxForUID(1111, 0), first.ClientID, first.SequenceID)
}

func TestCreateSession_PrincipalChangeAfterConfirmationAllowed(t *testing.T) {
	h := newTestHandler()
	ctx := ctxForUID(1111, 0)

	first := exchangeIDOK(t, h, ctx, "confirm-then-change", eidVerifier("verify01"))
	res1 := doCreateSession(t, h, ctx, first.ClientID, first.SequenceID)
	if res1.Status != types.NFS4_OK {
		t.Fatalf("first CREATE_SESSION = %d, want NFS4_OK", res1.Status)
	}

	// The client ID is already confirmed, so the confirmation phase is skipped
	// and a second session may come from a different principal.
	res2 := doCreateSession(t, h, ctxForUID(2222, 0), first.ClientID, res1.SequenceID+1)
	if res2.Status != types.NFS4_OK {
		t.Fatalf("second CREATE_SESSION from another principal = %d, want NFS4_OK", res2.Status)
	}
}

func TestCreateSession_UnconfirmedRecordExpiresAfterALease(t *testing.T) {
	sm := state.NewStateManager(20 * time.Millisecond)
	h := NewHandler(nil, pseudofs.New(), sm)
	ctx := newTestCompoundContext()

	first := exchangeIDOK(t, h, ctx, "expiring-unconfirmed", eidVerifier("verify01"))

	// Inside the lease period the client ID still confirms.
	inside := exchangeIDOK(t, h, ctx, "inside-lease", eidVerifier("verify01"))
	createSessionOK(t, h, ctx, inside.ClientID, inside.SequenceID)

	time.Sleep(40 * time.Millisecond)

	res := doCreateSession(t, h, ctx, first.ClientID, first.SequenceID)
	if res.Status != types.NFS4ERR_STALE_CLIENTID {
		t.Fatalf("CREATE_SESSION after the lease period = %d, want NFS4ERR_STALE_CLIENTID (%d)",
			res.Status, types.NFS4ERR_STALE_CLIENTID)
	}
}
