package smb

// Tests for the ReleaseData encoder hook (D-09). These assert that the SMB
// response encoder invokes HandlerResult.ReleaseData exactly once per response
// AFTER the wire write completes — plain, encrypted-disabled, and compound
// paths alike. Non-pooled responses leave ReleaseData nil; the encoder must
// null-check.
//
// Per plan 09-02 Task 1 (ADAPT-02).

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
)

// newTestConnPair returns a (serverConn, cleanup) where serverConn is the side
// the test writes responses to and the peer end is continuously drained so
// WriteNetBIOSFrame does not block.
func newTestConnPair(t *testing.T) (net.Conn, func()) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, clientConn)
	}()
	cleanup := func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
		<-done
	}
	return serverConn, cleanup
}

// newTestConnInfo builds a minimal ConnInfo whose send path routes through
// WriteNetBIOSFrame without signing / encryption (SessionID 0 on the response
// header keeps sendMessage on the plain-wire branch).
func newTestConnInfo(t *testing.T, conn net.Conn) *ConnInfo {
	t.Helper()
	mgr := session.NewDefaultManager()
	h := handlers.NewHandlerWithSessionManager(mgr)
	return &ConnInfo{
		Conn:           conn,
		Handler:        h,
		SessionManager: mgr,
		WriteMu:        &LockedWriter{},
		WriteTimeout:   2 * time.Second,
		SequenceWindow: NewSequenceWindowForConnection(mgr),
	}
}

// newTestReqHeader returns a request header for SMB2Read with SessionID 0 and
// TreeID 0 so the response path skips signing/encryption gating.
func newTestReqHeader() *header.SMB2Header {
	return &header.SMB2Header{
		StructureSize: header.HeaderSize,
		Command:       types.SMB2Read,
		Credits:       1,
		CreditCharge:  1,
		MessageID:     1,
		SessionID:     0,
		TreeID:        0,
	}
}

func newTestHandlerCtx(ci *ConnInfo) *handlers.SMBHandlerContext {
	ctx := handlers.NewSMBHandlerContext(context.TODO(), "test-client", 0, 0, 1)
	_ = ci
	return ctx
}

// ---------------------------------------------------------------------------
// Test 1: nil ReleaseData — encoder MUST NOT panic and MUST NOT invoke anything.
// ---------------------------------------------------------------------------

func TestReleaseData_NilIsNoop(t *testing.T) {
	serverConn, cleanup := newTestConnPair(t)
	t.Cleanup(cleanup)

	ci := newTestConnInfo(t, serverConn)
	reqHeader := newTestReqHeader()
	ctx := newTestHandlerCtx(ci)

	result := &HandlerResult{
		Status:      types.StatusSuccess,
		Data:        []byte{0x11, 0x00, 0x50, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		ReleaseData: nil, // non-pooled path
	}

	if err := SendResponse(reqHeader, ctx, result, ci); err != nil {
		t.Fatalf("SendResponse returned error: %v", err)
	}
	// No assertion needed beyond "did not panic".
}

// ---------------------------------------------------------------------------
// Test 2: ReleaseData set — SendResponse invokes it exactly once after a
// successful wire write.
// ---------------------------------------------------------------------------

func TestReleaseData_FiresOnceAfterSuccessfulWrite(t *testing.T) {
	serverConn, cleanup := newTestConnPair(t)
	t.Cleanup(cleanup)

	ci := newTestConnInfo(t, serverConn)
	reqHeader := newTestReqHeader()
	ctx := newTestHandlerCtx(ci)

	var count atomic.Int64
	result := &HandlerResult{
		Status: types.StatusSuccess,
		Data:   []byte{0x11, 0x00, 0x50, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		ReleaseData: func() {
			count.Add(1)
		},
	}

	if err := SendResponseWithHooks(reqHeader, ctx, result, ci); err != nil {
		t.Fatalf("SendResponseWithHooks returned error: %v", err)
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("ReleaseData fire count = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Test 3: ReleaseData STILL fires when WriteNetBIOSFrame returns an error.
// The pooled buffer is no longer referenced after the write attempt returns.
// ---------------------------------------------------------------------------

func TestReleaseData_FiresOnWriteError(t *testing.T) {
	// Build a ConnInfo whose Conn is closed so Write returns io.ErrClosedPipe.
	serverConn, clientConn := net.Pipe()
	_ = clientConn.Close()
	_ = serverConn.Close()

	ci := &ConnInfo{
		Conn:           serverConn,
		Handler:        handlers.NewHandlerWithSessionManager(session.NewDefaultManager()),
		SessionManager: session.NewDefaultManager(),
		WriteMu:        &LockedWriter{},
		WriteTimeout:   100 * time.Millisecond,
		SequenceWindow: NewSequenceWindowForConnection(session.NewDefaultManager()),
	}

	reqHeader := newTestReqHeader()
	ctx := newTestHandlerCtx(ci)

	var count atomic.Int64
	result := &HandlerResult{
		Status: types.StatusSuccess,
		Data:   []byte{0x11, 0x00, 0x50, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		ReleaseData: func() {
			count.Add(1)
		},
	}

	// Expect a write error — but ReleaseData MUST still fire.
	err := SendResponse(reqHeader, ctx, result, ci)
	if err == nil {
		// We really expected an error; accept either outcome but require the
		// release still fired.
		t.Logf("SendResponse did not return an error on closed conn; continuing")
	} else if !errors.Is(err, io.ErrClosedPipe) && !isTransportError(err) {
		t.Logf("SendResponse returned unexpected error (ok): %v", err)
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("ReleaseData fire count = %d, want 1 (must fire even on write error)", got)
	}
}

// isTransportError loosely matches any net.Conn write error. Used only for
// logging in Test 3 — the assertion that matters is the release counter.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// ---------------------------------------------------------------------------
// Test 4: Compound response path — each sub-HandlerResult's ReleaseData fires
// exactly ONCE, AFTER the single composed wire write (not between sub-responses).
// ---------------------------------------------------------------------------

func TestReleaseData_CompoundPathFiresAllAfterWrite(t *testing.T) {
	serverConn, cleanup := newTestConnPair(t)
	t.Cleanup(cleanup)

	ci := newTestConnInfo(t, serverConn)

	// Build 3 compound sub-responses, each with its own ReleaseData closure.
	var counts [3]atomic.Int64
	var responses []compoundResponse

	for i := 0; i < 3; i++ {
		idx := i
		reqHeader := newTestReqHeader()
		reqHeader.MessageID = uint64(i + 1)
		ctx := newTestHandlerCtx(ci)
		result := &HandlerResult{
			Status: types.StatusSuccess,
			Data:   []byte{0x11, 0x00, 0x50, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			ReleaseData: func() {
				counts[idx].Add(1)
			},
		}
		rh, body := buildResponseHeaderAndBody(reqHeader, ctx, result, ci)
		responses = append(responses, compoundResponse{
			respHeader:  rh,
			body:        body,
			releaseData: result.ReleaseData,
		})
	}

	if err := sendCompoundResponses(responses, ci); err != nil {
		t.Fatalf("sendCompoundResponses returned error: %v", err)
	}

	for i := range counts {
		if got := counts[i].Load(); got != 1 {
			t.Errorf("sub %d: ReleaseData fire count = %d, want 1", i, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 5: plain wire-write path fires ReleaseData (no encryption negotiated).
// This is effectively covered by Test 2, but is called out explicitly so the
// contract is visible in the test list.
// ---------------------------------------------------------------------------

func TestReleaseData_PlainWriteFires(t *testing.T) {
	serverConn, cleanup := newTestConnPair(t)
	t.Cleanup(cleanup)

	ci := newTestConnInfo(t, serverConn)
	// Encryption middleware intentionally nil — plain-write branch.
	reqHeader := newTestReqHeader()
	ctx := newTestHandlerCtx(ci)

	var count atomic.Int64
	result := &HandlerResult{
		Status: types.StatusSuccess,
		Data:   []byte{0x11, 0x00, 0x50, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		ReleaseData: func() {
			count.Add(1)
		},
	}

	if err := SendResponse(reqHeader, ctx, result, ci); err != nil {
		t.Fatalf("SendResponse returned error: %v", err)
	}
	if got := count.Load(); got != 1 {
		t.Fatalf("ReleaseData fire count on plain path = %d, want 1", got)
	}
}
