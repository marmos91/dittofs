package smb

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/handlers"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAsyncCreditConnInfo builds a ConnInfo wired for async completion sends:
// a real session manager shared with the handler, a live sequence window, and
// a drained pipe so WriteNetBIOSFrame succeeds.
func newAsyncCreditConnInfo(t *testing.T, mgr *session.Manager) (*ConnInfo, func()) {
	t.Helper()
	serverConn, cleanup := newTestConnPair(t)
	ci := &ConnInfo{
		Conn:           serverConn,
		Handler:        handlers.NewHandlerWithSessionManager(mgr),
		SessionManager: mgr,
		WriteMu:        &LockedWriter{},
		WriteTimeout:   2 * time.Second,
		SequenceWindow: NewSequenceWindowForConnection(mgr),
	}
	return ci, cleanup
}

// TestSendAsyncCompletionResponse_AdvancesSessionCredits pins the async credit
// wiring: a standalone async completion must advance BOTH the connection's
// sequence window and the session's credit bookkeeping. The grantAdaptive
// strategy throttles on Session.GetOutstanding, so an async grant that only
// extends the wire window leaves that throttle input blind to async traffic.
func TestSendAsyncCompletionResponse_AdvancesSessionCredits(t *testing.T) {
	mgr := session.NewDefaultManager()
	sess := mgr.CreateSession("10.0.0.1:1234", false, "async-client", "TEST")
	ci, cleanup := newAsyncCreditConnInfo(t, mgr)
	defer cleanup()

	grantedBefore := mgr.GetSessionStats(sess.SessionID).Granted
	outstandingBefore := mgr.GetSessionStats(sess.SessionID).Outstanding
	windowBefore := ci.SequenceWindow.Size()

	err := SendAsyncCompletionResponse(sess.SessionID, 100, 5,
		types.SMB2Create, types.StatusSuccess, []byte{0x01}, ci)
	require.NoError(t, err)

	stats := mgr.GetSessionStats(sess.SessionID)
	assert.Greater(t, stats.Granted, grantedBefore,
		"async completion must record the grant in session credits (red without the wiring)")
	assert.Greater(t, stats.Outstanding, outstandingBefore,
		"async grant must advance the outstanding balance the adaptive strategy samples")
	assert.Greater(t, ci.SequenceWindow.Size(), windowBefore,
		"async grant must still extend the wire sequence window")
}

// TestSendAsyncChangeNotifyResponse_AdvancesSessionCredits pins the same
// invariant for CHANGE_NOTIFY completions, which arrive without a correlated
// client request.
func TestSendAsyncChangeNotifyResponse_AdvancesSessionCredits(t *testing.T) {
	mgr := session.NewDefaultManager()
	sess := mgr.CreateSession("10.0.0.1:1234", false, "notify-client", "TEST")
	ci, cleanup := newAsyncCreditConnInfo(t, mgr)
	defer cleanup()

	require.True(t, ci.TryReserveAsync(),
		"CHANGE_NOTIFY reserves an async slot the sender releases")

	grantedBefore := mgr.GetSessionStats(sess.SessionID).Granted
	windowBefore := ci.SequenceWindow.Size()

	notify := &handlers.ChangeNotifyResponse{}
	err := SendAsyncChangeNotifyResponse(sess.SessionID, 200, 7, notify, ci)
	require.NoError(t, err)

	stats := mgr.GetSessionStats(sess.SessionID)
	assert.Greater(t, stats.Granted, grantedBefore,
		"async CHANGE_NOTIFY must record the grant in session credits")
	assert.Greater(t, ci.SequenceWindow.Size(), windowBefore,
		"async CHANGE_NOTIFY must still extend the wire sequence window")
}

// TestSendAsyncCompletionResponse_NilSessionManagerGuard proves async
// completions tolerate a ConnInfo whose SessionManager is nil — the pre-fix
// shape never touched the manager, and async completions can fire on
// connections where the sync dispatch path never ran. The grant must fall
// back to the wire window alone, not panic.
func TestSendAsyncCompletionResponse_NilSessionManagerGuard(t *testing.T) {
	serverConn, cleanup := newTestConnPair(t)
	defer cleanup()

	sw := session.NewCommandSequenceWindow(8192)
	ci := &ConnInfo{
		Conn:           serverConn,
		Handler:        handlers.NewHandlerWithSessionManager(session.NewDefaultManager()),
		SessionManager: nil,
		WriteMu:        &LockedWriter{},
		WriteTimeout:   2 * time.Second,
		SequenceWindow: sw,
	}

	windowBefore := sw.Size()
	err := SendAsyncCompletionResponse(42, 1, 1,
		types.SMB2Create, types.StatusSuccess, []byte{0x01}, ci)
	require.NoError(t, err)
	assert.Greater(t, sw.Size(), windowBefore,
		"grant must fall back to the wire window when no session manager is wired")
}
