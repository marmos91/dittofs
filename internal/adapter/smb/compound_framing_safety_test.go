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
	sess.CryptoState.SigningRequired = true

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

// cancelBody is the 4-byte SMB2 CANCEL request body (StructureSize=4, Reserved=0).
// A CANCEL handler returns a nil result (MS-SMB2 §3.3.5.16: no response) only
// when the body is at least 4 bytes; a shorter body yields an error result.
var cancelBody = []byte{0x04, 0x00, 0x00, 0x00}

// encodeCompoundCommand encodes hdr+body as one compound member. When withNext
// is true the member is padded to an 8-byte-aligned length and its NextCommand
// field is set to that length so a following member can be appended.
func encodeCompoundCommand(hdr *header.SMB2Header, body []byte, withNext bool) []byte {
	if withNext {
		// 8-byte align so NextCommand satisfies the alignment + min-size gates.
		total := (header.HeaderSize + len(body) + 7) &^ 7
		hdr.NextCommand = uint32(total)
	}
	frame := append(hdr.Encode(), body...)
	if withNext {
		for len(frame) < int(hdr.NextCommand) {
			frame = append(frame, 0)
		}
	}
	return frame
}

// TestProcessRemaining_CancelSubcommandNoPanic verifies that a CANCEL appearing
// as a subsequent compound command does not panic. ProcessRequestWithFileID...
// returns a nil result for CANCEL (MS-SMB2 §3.3.5.16 requires no response);
// before the fix processRemaining passed that nil to buildResponseHeaderAndBody,
// which dereferenced result.Status and crashed the connection goroutine (DoS).
// After the fix the nil subcommand is skipped and the chain continues.
func TestProcessRemaining_CancelSubcommandNoPanic(t *testing.T) {
	ci := newConnInfoForDispatch(t, 1, types.Dialect0311)

	// CANCEL (yields nil result) chained to a trailing CANCEL. Both emit no
	// response per §3.3.5.16, so the response list must be empty — and the
	// loop must not panic on the first nil.
	first := encodeCompoundCommand(&header.SMB2Header{Command: types.SMB2Cancel, MessageID: 2}, cancelBody, true)
	second := encodeCompoundCommand(&header.SMB2Header{Command: types.SMB2Cancel, MessageID: 3}, cancelBody, false)
	frame := append(first, second...)

	state := compoundLoopState{lastSessionID: 0, lastTreeID: 0}
	state.processRemaining(context.Background(), frame, ci, false, nil)

	if len(state.responses) != 0 {
		t.Fatalf("CANCEL subcommands must emit no response, got %d", len(state.responses))
	}
}

// TestProcessRemaining_CancelThenRealCommandContinues verifies that after a nil
// CANCEL subcommand the loop continues and still emits a response for the next
// command (a nil result must skip, not abort the chain). The trailing command
// uses an invalid command code so prepareDispatch returns a non-nil error
// result (STATUS_INVALID_PARAMETER) — proving the following member was
// processed despite the preceding nil.
func TestProcessRemaining_CancelThenRealCommandContinues(t *testing.T) {
	ci := newConnInfoForDispatch(t, 1, types.Dialect0311)

	const invalidCommand = types.Command(0xFFFF)
	first := encodeCompoundCommand(&header.SMB2Header{Command: types.SMB2Cancel, MessageID: 2}, cancelBody, true)
	second := encodeCompoundCommand(&header.SMB2Header{Command: invalidCommand, MessageID: 3}, nil, false)
	frame := append(first, second...)

	state := compoundLoopState{lastSessionID: 0, lastTreeID: 0}
	state.processRemaining(context.Background(), frame, ci, false, nil)

	if len(state.responses) != 1 {
		t.Fatalf("expected 1 response (CANCEL skipped, invalid command answered), got %d", len(state.responses))
	}
	if got := state.responses[0].respHeader.Status; got != types.StatusInvalidParameter {
		t.Fatalf("trailing command status = 0x%x, want STATUS_INVALID_PARAMETER (0x%x)", got, types.StatusInvalidParameter)
	}
}

// TestProcessCompoundRequest_CancelFirstNoPanic verifies that a CANCEL as the
// FIRST command of a compound does not panic. The first-command path in
// ProcessCompoundRequest previously passed the nil CANCEL result straight to
// buildResponseHeaderAndBody (result.Status nil deref → connection-goroutine
// crash). After the fix the nil first command is skipped and the remaining
// chain is processed.
func TestProcessCompoundRequest_CancelFirstNoPanic(t *testing.T) {
	ci := newConnInfoForDispatch(t, 1, types.Dialect0311)

	const invalidCommand = types.Command(0xFFFF)
	firstHeader := &header.SMB2Header{Command: types.SMB2Cancel, MessageID: 2}
	// Trailing member: invalid command → non-nil error response, proving the
	// chain continued past the nil first command.
	remaining := encodeCompoundCommand(&header.SMB2Header{Command: invalidCommand, MessageID: 3}, nil, false)

	// This must not panic.
	ProcessCompoundRequest(context.Background(), firstHeader, cancelBody, firstHeader.Encode(), remaining, ci, false, nil)
}
