package smb

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// relatedFollowerFrame builds a single compound sub-command frame (just a
// 64-byte SMB2 header, no body, NextCommand=0 so it is the last/only command)
// with FLAGS_RELATED_OPERATIONS set, suitable for feeding to
// compoundLoopState.processRemaining as the related follower of a failed
// predecessor.
func relatedFollowerFrame(cmd types.Command, sessionID uint64) []byte {
	hdr := &header.SMB2Header{
		Command:   cmd,
		Flags:     types.FlagRelated,
		MessageID: 2,
		SessionID: sessionID,
	}
	return hdr.Encode()
}

// TestProcessRemaining_SessionExpiredPropagates is the regression guard for the
// M-A1 async/sync drift: completeCompoundAfterAsyncCreate used to hardcode
// STATUS_INVALID_PARAMETER for a related follower of a session-failed
// predecessor, silently missing the SESSION_EXPIRED-propagation fix the sync
// path had (relatedSessionFailureStatus). Both paths now run this single shared
// loop, so this asserts the propagation once for both.
//
// A predecessor that failed with STATUS_NETWORK_SESSION_EXPIRED must propagate
// SESSION_EXPIRED to its related follower (smbtorture
// smb2.session.expire2s/expire2e), NOT collapse to INVALID_PARAMETER.
func TestProcessRemaining_SessionExpiredPropagates(t *testing.T) {
	ci := newConnInfoForDispatch(t, 1, types.Dialect0311)

	state := compoundLoopState{
		lastSessionID:        1,
		lastCmdSessionFailed: true,
		lastCmdFailed:        true,
		lastCmdStatus:        types.StatusNetworkSessionExpired,
	}
	// CLOSE is a related follower; it never dispatches because the session-level
	// failure gate fires first and emits the propagated status.
	frame := relatedFollowerFrame(types.SMB2Close, 1)
	state.processRemaining(context.Background(), frame, ci, false, nil)

	if len(state.responses) != 1 {
		t.Fatalf("expected exactly 1 follower response, got %d", len(state.responses))
	}
	resp := state.responses[0]
	if resp.respHeader.Status != types.StatusNetworkSessionExpired {
		t.Fatalf("follower status = 0x%x, want STATUS_NETWORK_SESSION_EXPIRED (0x%x) — async/sync drift regression",
			resp.respHeader.Status, types.StatusNetworkSessionExpired)
	}
	if !resp.respHeader.Flags.IsRelated() {
		t.Fatal("follower response must carry FLAGS_RELATED_OPERATIONS")
	}
}

// TestProcessRemaining_NonExpiredSessionFailureCollapses verifies the contrast:
// a non-expiry session-level failure (USER_SESSION_DELETED) collapses to
// STATUS_INVALID_PARAMETER for the related follower, because there is no valid
// session/tree context to inherit (MS-SMB2 3.3.5.2.7.2).
func TestProcessRemaining_NonExpiredSessionFailureCollapses(t *testing.T) {
	ci := newConnInfoForDispatch(t, 1, types.Dialect0311)

	state := compoundLoopState{
		lastSessionID:        1,
		lastCmdSessionFailed: true,
		lastCmdFailed:        true,
		lastCmdStatus:        types.StatusUserSessionDeleted,
	}
	frame := relatedFollowerFrame(types.SMB2Close, 1)
	state.processRemaining(context.Background(), frame, ci, false, nil)

	if len(state.responses) != 1 {
		t.Fatalf("expected exactly 1 follower response, got %d", len(state.responses))
	}
	if got := state.responses[0].respHeader.Status; got != types.StatusInvalidParameter {
		t.Fatalf("follower status = 0x%x, want STATUS_INVALID_PARAMETER (0x%x)",
			got, types.StatusInvalidParameter)
	}
}
