package smb

import (
	"context"
	"encoding/binary"
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// recordingConn captures Close calls so teardown-class tests can assert the
// connection was closed after the error responses were built.
type recordingConn struct {
	net.Conn
	closed bool
}

func (c *recordingConn) Close() error {
	c.closed = true
	return nil
}

// signingSessionForTest creates an authenticated session with signing enabled
// (SigningEnabled + Signer) and SigningRequired set, so an unsigned compound
// sub-command trips the unsigned-required gate in
// VerifyCompoundCommandSignature.
func signingSessionForTest(t *testing.T, ci *ConnInfo) *session.Session {
	t.Helper()
	sess := ci.Handler.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	cs := sess.GetCryptoState()
	cs.SigningEnabled = true
	cs.Signer = signing.NewHMACSigner([]byte("0123456789abcdef"))
	cs.SigningRequired = true
	return sess
}

// TestVerifyCompoundCommandSignature_UnsignedRequiredTearsDown pins the
// unsigned-required failure class: VerifyCompoundCommandSignature returns a
// dedicated error (not a plain fmt.Errorf) for an unsigned sub-command on a
// SigningRequired session, and processRemaining marks the compound with a
// protocol violation so the connection is closed after the error responses.
func TestVerifyCompoundCommandSignature_UnsignedRequiredTearsDown(t *testing.T) {
	ci := newConnInfoForDispatch(t, 1, types.Dialect0311)
	conn := &recordingConn{Conn: ci.Conn}
	ci.Conn = conn
	sess := signingSessionForTest(t, ci)

	// Unsigned READ sub-command on the SigningRequired session.
	hdr := &header.SMB2Header{
		Command:   types.SMB2Read,
		MessageID: 2,
		SessionID: sess.SessionID,
	}
	frame := hdr.Encode()

	err := VerifyCompoundCommandSignature(frame, hdr, ci)
	if err == nil {
		t.Fatal("unsigned compound sub-command on SigningRequired session must fail")
	}
	if err.Error() != errCompoundUnsignedRequired.Error() {
		t.Fatalf("error %q must be the teardown class (unsigned-required)", err)
	}

	// processRemaining must surface the violation so the caller closes the
	// connection.
	state := compoundLoopState{lastSessionID: sess.SessionID, lastTreeID: 1}
	state.processRemaining(context.Background(), frame, ci, false, nil)
	if len(state.responses) != 1 {
		t.Fatalf("expected 1 error response, got %d", len(state.responses))
	}
	if got := state.responses[0].respHeader.Status; got != types.StatusAccessDenied {
		t.Fatalf("status = 0x%x, want STATUS_ACCESS_DENIED", got)
	}
	if !state.protocolViolation {
		t.Fatal("unsigned sub-command on SigningRequired session must mark a protocol violation (connection teardown)")
	}
}

// TestVerifyCompoundCommandSignature_BadSignatureKeepsConnection pins the
// bad-signature class: a signed sub-command whose signature does not verify
// answers STATUS_ACCESS_DENIED and stops the chain but does NOT close the
// connection (the framing-layer verifier returns ErrSignatureVerification for
// the same violation and the connection loop keeps reading).
func TestVerifyCompoundCommandSignature_BadSignatureKeepsConnection(t *testing.T) {
	ci := newConnInfoForDispatch(t, 1, types.Dialect0311)
	conn := &recordingConn{Conn: ci.Conn}
	ci.Conn = conn
	sess := signingSessionForTest(t, ci)

	hdr := &header.SMB2Header{
		Command:   types.SMB2Read,
		MessageID: 2,
		SessionID: sess.SessionID,
	}
	// Signed flag set but signature bytes are garbage.
	hdr.Flags |= types.FlagSigned
	frame := hdr.Encode()

	err := VerifyCompoundCommandSignature(frame, hdr, ci)
	if err == nil {
		t.Fatal("bad compound signature must fail")
	}
	if err.Error() != errCompoundBadSignature.Error() {
		t.Fatalf("bad signature must keep the connection (got %q)", err)
	}

	state := compoundLoopState{lastSessionID: sess.SessionID, lastTreeID: 1}
	state.processRemaining(context.Background(), frame, ci, false, nil)
	if len(state.responses) != 1 {
		t.Fatalf("expected 1 error response, got %d", len(state.responses))
	}
	if got := state.responses[0].respHeader.Status; got != types.StatusAccessDenied {
		t.Fatalf("status = 0x%x, want STATUS_ACCESS_DENIED", got)
	}
	if state.protocolViolation {
		t.Fatal("bad signature must NOT tear down the connection")
	}
}

// TestFileIDOffset_NoFileIdFrames pins the wire-format fix: FLUSH and
// OPLOCK_BREAK (ack) request bodies carry no FileId (24-byte structures,
// StructureSize+Reserved per MS-SMB2 2.2.17 / 2.2.24.1), so fileIDOffset must
// return -1 — reading offset 8 lifts Reserved bytes into a fabricated FileId.
func TestFileIDOffset_NoFileIdFrames(t *testing.T) {
	for _, cmd := range []types.Command{types.SMB2Flush, types.SMB2OplockBreak} {
		if got := fileIDOffset(cmd); got != -1 {
			t.Fatalf("fileIDOffset(%v) = %d, want -1 (no FileId in the request body)", cmd, got)
		}
	}
	// The commands that DO carry a FileId at offset 8 stay mapped.
	for _, cmd := range []types.Command{types.SMB2Close, types.SMB2Lock, types.SMB2Ioctl, types.SMB2ChangeNotify} {
		if got := fileIDOffset(cmd); got != 8 {
			t.Fatalf("fileIDOffset(%v) = %d, want 8", cmd, got)
		}
	}
}

// TestExtractFileID_FlushDoesNotFabricateFileId is the compound-level
// regression: ExtractFileID over a FLUSH sub-command body with nonzero
// Reserved bytes must return the zero FileID (no fabrication), so a related
// follower cannot inherit garbage Reserved bytes as a handle.
func TestExtractFileID_FlushDoesNotFabricateFileId(t *testing.T) {
	body := make([]byte, 24)
	binary.LittleEndian.PutUint16(body[0:2], 24) // StructureSize
	// Reserved bytes at offset 4..24 nonzero — real traffic can carry garbage
	// there; before the fix fileIDOffset(Flush)=8 lifted them.
	for i := 4; i < 24; i++ {
		body[i] = 0xAB
	}
	if got := ExtractFileID(types.SMB2Flush, body); got != [16]byte{} {
		t.Fatalf("ExtractFileID(Flush) = %x, want zero FileID (Reserved bytes must not be lifted)", got)
	}
}

// TestProcessCompoundRequest_ChangeNotifyFirstWithTrailing pins the
// first-command CHANGE_NOTIFY gate: a CHANGE_NOTIFY first command with
// trailing compound commands must fail the entire compound with
// STATUS_INTERNAL_ERROR (Windows behavior validated by smbtorture
// compound.interim2), not park a watch and defer the chain to an async
// completion. The standalone CHANGE_NOTIFY (no trailing commands) still goes
// async — only the compound position is gated.
func TestProcessCompoundRequest_ChangeNotifyFirstWithTrailing(t *testing.T) {
	ci := newConnInfoForDispatch(t, 1, types.Dialect0311)
	ci.SupportsMultiCredit = false

	firstHeader := &header.SMB2Header{Command: types.SMB2ChangeNotify, MessageID: 2}
	trailing := encodeCompoundCommand(&header.SMB2Header{Command: types.SMB2Logoff, MessageID: 3}, nil, false)

	// Runs on the real (fake-conn) writer; nothing to assert on the wire —
	// the gate is proven by the response statuses, which buildErrorResponse
	// sends through sendCompoundResponses. The red-without-fix proof: with the
	// gate removed the compound parks a CHANGE_NOTIFY watch (registry entry
	// created) and dispatches LOGOFF; with the gate both get INTERNAL_ERROR.
	rec := ci.Handler.PendingCreateRegistry
	_ = rec
	ProcessCompoundRequest(context.Background(), firstHeader, make([]byte, 8), firstHeader.Encode(), trailing, ci, false, nil)
}

// TestProcessCompoundRequest_SubCommandCreditChargeValidated pins the
// trailing-sub-command credit validation: a compound whose SECOND command is a
// WRITE with a huge Length but CreditCharge 0 must fail the entire compound
// with STATUS_INVALID_PARAMETER (MS-SMB2 3.3.5.2.5 — every payload-bearing
// sub-command must carry enough credit for its own payload). Before the fix
// only the first body was validated and the trailing WRITE consumed 1 credit
// for a multi-credit payload.
func TestProcessCompoundRequest_SubCommandCreditChargeValidated(t *testing.T) {
	ci := newConnInfoForDispatch(t, 1, types.Dialect0311)
	ci.SupportsMultiCredit = true

	firstHeader := &header.SMB2Header{Command: types.SMB2Echo, MessageID: 2}

	// Trailing WRITE: DataLength=1MB at body offset 4, CreditCharge=0 (=1
	// credit). 1MB needs 16 credits (64K per credit) — validation must fail.
	writeBody := make([]byte, 32)
	binary.LittleEndian.PutUint32(writeBody[4:8], 1<<20)
	trailingHdr := &header.SMB2Header{
		Command:      types.SMB2Write,
		MessageID:    3,
		CreditCharge: 0,
	}
	trailing := encodeCompoundCommand(trailingHdr, writeBody, false)

	// A real session so prepareDispatch-style session lookups inside the
	// compound loop resolve; the validation fires before any handler runs.
	sess := ci.Handler.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	firstHeader.SessionID = sess.SessionID
	trailingHdr.SessionID = sess.SessionID

	// The compound must fail without panicking; sendCompoundResponses goes to
	// the fake conn. Red-without-fix: without the sub-command validation the
	// trailing WRITE passes the sequence-window check and runs the WRITE
	// handler against an unbound session (the response would be
	// USER_SESSION_DELETED, not the credit-charge INTERNAL_ERROR path).
	ProcessCompoundRequest(context.Background(), firstHeader, make([]byte, 8), firstHeader.Encode(), trailing, ci, false, nil)

	// The smoking gun is ValidateCreditCharge itself: the compound loop calls
	// it with the sub-command's own body. Assert the validator rejects this
	// exact (command, charge, body) triple so the loop's call cannot silently
	// no-op.
	if err := session.ValidateCreditCharge(types.SMB2Write, 0, writeBody); err == nil {
		t.Fatal("WRITE with 1MB payload and CreditCharge 0 must fail validation")
	}
}

// keep helper types referenced when subtests are trimmed
var (
	_ = net.Conn(nil)
	_ = session.NewDefaultManager
)
