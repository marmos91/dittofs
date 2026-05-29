package smb

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
)

// fakeAddr lets newConnInfoForDispatch satisfy net.Conn.RemoteAddr without a
// real pipe.
type fakeAddr struct{}

func (fakeAddr) Network() string { return "tcp" }
func (fakeAddr) String() string  { return "127.0.0.1:1" }

type fakeConn struct{ net.Conn }

func (fakeConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (fakeConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) Read([]byte) (int, error)         { return 0, nil }
func (fakeConn) Write(b []byte) (int, error)      { return len(b), nil }
func (fakeConn) Close() error                     { return nil }

func newConnInfoForDispatch(t *testing.T, connID uint64, dialect types.Dialect) *ConnInfo {
	t.Helper()
	mgr := session.NewDefaultManager()
	h := handlers.NewHandlerWithSessionManager(mgr)
	cs := NewConnectionCryptoState()
	cs.SetDialect(dialect)
	return &ConnInfo{
		ConnID:         connID,
		Conn:           fakeConn{},
		Handler:        h,
		SessionManager: mgr,
		WriteMu:        &LockedWriter{},
		WriteTimeout:   2 * time.Second,
		CryptoState:    cs,
	}
}

// TestPrepareDispatch_RejectsUnboundChannel_SMB3 verifies that a data-path
// command (e.g. READ) arriving on an SMB 3.x connection that is neither the
// session's origin nor a bound channel is rejected with
// STATUS_USER_SESSION_DELETED. Mirrors smbtorture session.bind2 / session.
// bind_invalid_auth at session.c:2209 / 2485 (per Samba
// smbXsrv_session_find_channel, smb2_server.c:2246).
func TestPrepareDispatch_RejectsUnboundChannel_SMB3(t *testing.T) {
	for _, dialect := range []types.Dialect{types.Dialect0300, types.Dialect0302, types.Dialect0311} {
		t.Run(dialect.String(), func(t *testing.T) {
			ci := newConnInfoForDispatch(t, 2, dialect)
			sess := ci.Handler.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
			sess.OriginConnID = 1 // origin is conn 1; we're dispatching on conn 2

			hdr := &header.SMB2Header{
				Command:   types.SMB2Logoff,
				SessionID: sess.SessionID,
				MessageID: 1,
			}
			_, _, errStatus := prepareDispatch(context.Background(), hdr, ci)
			if errStatus != types.StatusUserSessionDeleted {
				t.Fatalf("errStatus=0x%x, want StatusUserSessionDeleted (0x%x)", errStatus, types.StatusUserSessionDeleted)
			}
		})
	}
}

// TestPrepareDispatch_AllowsOriginConnection_SMB3 verifies that a session-
// gated command on the session's origin connection passes the unbound-channel
// gate. LOGOFF is used because it sets NeedsSession=true and NeedsTree=false.
func TestPrepareDispatch_AllowsOriginConnection_SMB3(t *testing.T) {
	ci := newConnInfoForDispatch(t, 1, types.Dialect0311)
	sess := ci.Handler.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.OriginConnID = 1 // matches ci.ConnID

	hdr := &header.SMB2Header{
		Command:   types.SMB2Logoff,
		SessionID: sess.SessionID,
		MessageID: 1,
	}
	_, _, errStatus := prepareDispatch(context.Background(), hdr, ci)
	if errStatus != 0 {
		t.Fatalf("errStatus=0x%x, want 0 on origin connection", errStatus)
	}
}

// TestPrepareDispatch_AllowsBoundChannel_SMB3 verifies that a session-gated
// command on a connection registered as a bound channel passes the gate.
func TestPrepareDispatch_AllowsBoundChannel_SMB3(t *testing.T) {
	ci := newConnInfoForDispatch(t, 2, types.Dialect0311)
	sess := ci.Handler.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.OriginConnID = 1
	sess.AddChannel(&session.Channel{ConnID: 2, Dialect: types.Dialect0311})

	hdr := &header.SMB2Header{
		Command:   types.SMB2Logoff,
		SessionID: sess.SessionID,
		MessageID: 1,
	}
	_, _, errStatus := prepareDispatch(context.Background(), hdr, ci)
	if errStatus != 0 {
		t.Fatalf("errStatus=0x%x, want 0 on bound channel", errStatus)
	}
}

// TestPrepareDispatch_DoesNotGateSMB2x verifies the unbound-channel gate only
// applies to SMB 3.x. SMB 2.x sessions are per-connection; the existing
// session-not-found path already covers cross-connection abuse.
func TestPrepareDispatch_DoesNotGateSMB2x(t *testing.T) {
	ci := newConnInfoForDispatch(t, 2, types.Dialect0210)
	sess := ci.Handler.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.OriginConnID = 1

	hdr := &header.SMB2Header{
		Command:   types.SMB2Logoff,
		SessionID: sess.SessionID,
		MessageID: 1,
	}
	_, _, errStatus := prepareDispatch(context.Background(), hdr, ci)
	if errStatus != 0 {
		t.Fatalf("errStatus=0x%x, want 0 for SMB 2.x (per-connection scoping handles cross-conn elsewhere)", errStatus)
	}
}
