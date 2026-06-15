package smb

import (
	"context"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestParseCompoundCommand_NextCommandTooSmall verifies that ParseCompoundCommand
// returns an error (not a panic) when NextCommand is non-zero but smaller than
// HeaderSize. Such a value is 8-byte aligned (passes the alignment gate) but
// points inside the header, which would otherwise produce a negative-length
// slice at body extraction (data[64:N] with N < 64).
func TestParseCompoundCommand_NextCommandTooSmall(t *testing.T) {
	for _, nextCmd := range []uint32{8, 16, 32, 40, 56} { // all 8-byte-aligned but < 64
		t.Run(fmt.Sprintf("NextCommand=%d", nextCmd), func(t *testing.T) {
			hdr := &header.SMB2Header{
				Command:     types.SMB2Read,
				MessageID:   1,
				NextCommand: nextCmd,
			}
			// Construct a buffer large enough to pass the size gate.
			data := make([]byte, 128)
			copy(data, hdr.Encode())
			_, _, _, err := ParseCompoundCommand(data)
			if err == nil {
				t.Fatal("expected error for NextCommand < HeaderSize, got nil")
			}
		})
	}
}

// TestSplitCompoundBody_NextCommandTooSmall verifies that splitCompoundBody does
// not panic and returns an empty body when NextCommand < HeaderSize. This path
// is reached by parseSMB2Message for the first command of every incoming SMB2
// message, so it is reachable by any unauthenticated client.
func TestSplitCompoundBody_NextCommandTooSmall(t *testing.T) {
	for _, nextCmd := range []uint32{8, 16, 32, 40, 56} {
		t.Run(fmt.Sprintf("NextCommand=%d", nextCmd), func(t *testing.T) {
			hdr := &header.SMB2Header{
				Command:     types.SMB2Read,
				MessageID:   1,
				NextCommand: nextCmd,
			}
			message := make([]byte, 128)
			copy(message, hdr.Encode())
			// This must not panic.
			body, remaining := splitCompoundBody(message, hdr)
			if len(body) != 0 {
				t.Fatalf("expected empty body for NextCommand=%d, got %d bytes", nextCmd, len(body))
			}
			if remaining != nil {
				t.Fatalf("expected nil remaining for NextCommand=%d, got %d bytes", nextCmd, len(remaining))
			}
		})
	}
}

// TestProcessRemaining_RelatedSignatureNotSkipped verifies that a related
// sub-command carrying the wire sentinel SessionID (0xFFFFFFFFFFFFFFFF per
// MS-SMB2 §2.2.3.1) is presented with the resolved SessionID during
// VerifyCompoundCommandSignature, not the sentinel. With a session that has
// SigningRequired=true and an unsigned related frame: before the fix the session
// lookup on the sentinel fails (ok=false) and signing is silently skipped; after
// the fix the sentinel is resolved to the real session, the signing gate fires,
// and the command is rejected with STATUS_ACCESS_DENIED.
func TestProcessRemaining_RelatedSignatureNotSkipped(t *testing.T) {
	ci := newConnInfoForDispatch(t, 1, types.Dialect0311)

	// Authenticated (non-guest, non-null) session with signing required so an
	// unsigned message trips the signing gate in VerifyCompoundCommandSignature.
	sess := ci.Handler.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.GetCryptoState().SigningRequired = true

	// Related sub-command with the wire sentinel SessionID; NOT signed.
	hdr := &header.SMB2Header{
		Command:   types.SMB2Read,
		Flags:     types.FlagRelated,
		MessageID: 2,
		SessionID: 0xFFFFFFFFFFFFFFFF,
	}
	frame := hdr.Encode()

	state := compoundLoopState{
		lastSessionID: sess.SessionID,
		lastTreeID:    1,
	}
	// isEncrypted=false so signature verification runs.
	state.processRemaining(context.Background(), frame, ci, false, nil)

	if len(state.responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(state.responses))
	}
	got := state.responses[0].respHeader.Status
	if got != types.StatusAccessDenied {
		t.Fatalf("related unsigned command status = 0x%x, want STATUS_ACCESS_DENIED (0x%x) — auth bypass regression",
			got, types.StatusAccessDenied)
	}
}
