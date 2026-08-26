package smb

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// A later request's response must not go out while an earlier request on the
// same connection is still unanswered — that is the window in which the
// earlier request's lease break notification is overtaken.
func TestRequestOrder_LaterResponseWaitsForEarlier(t *testing.T) {
	o := NewRequestOrder()
	first := o.Begin()
	second := o.Begin()

	arrived := make(chan struct{})
	go func() {
		second.WaitTurn(context.Background())
		close(arrived)
	}()

	select {
	case <-arrived:
		t.Fatal("second response went out while the first request was still unanswered")
	case <-time.After(50 * time.Millisecond):
	}

	first.Release()

	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("second response never went out after the first released")
	}
}

// Releasing out of order must not advance the order past a request that is
// still holding its place.
func TestRequestOrder_OutOfOrderRelease(t *testing.T) {
	o := NewRequestOrder()
	first, second, third := o.Begin(), o.Begin(), o.Begin()

	arrived := make(chan struct{})
	go func() {
		third.WaitTurn(context.Background())
		close(arrived)
	}()

	second.Release() // the middle request finishes first
	select {
	case <-arrived:
		t.Fatal("third response went out while the first request was still unanswered")
	case <-time.After(50 * time.Millisecond):
	}

	first.Release()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("third response never went out after every earlier request released")
	}
}

// A handler that must wait on client traffic releases early. Its own response
// then goes out without waiting — it has already given up its place.
func TestRequestOrder_EarlyReleaseIsIdempotent(t *testing.T) {
	o := NewRequestOrder()
	first := o.Begin()
	second := o.Begin()

	first.Release() // stepped out to wait for a break acknowledgement

	done := make(chan struct{})
	go func() {
		second.WaitTurn(context.Background())
		first.WaitTurn(context.Background()) // the early releaser's own response
		first.Release()                      // the read loop's deferred release
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("early release did not let the responses behind it through")
	}
}

// The wait is bounded so an unexpectedly blocked handler delays one
// connection's responses instead of stalling them for good.
func TestRequestOrder_WaitTimesOut(t *testing.T) {
	o := NewRequestOrder()
	o.waitTimeout = 20 * time.Millisecond
	o.Begin() // never released
	second := o.Begin()

	start := time.Now()
	second.WaitTurn(context.Background())
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("returned after %v, before the wait timeout", elapsed)
	}
}

// A cancelled request stops waiting for its turn rather than pinning the
// goroutine to a connection that is going away.
func TestRequestOrder_WaitStopsOnContextCancel(t *testing.T) {
	o := NewRequestOrder()
	o.Begin() // never released
	second := o.Begin()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		second.WaitTurn(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitTurn ignored context cancellation")
	}
}

// A nil token is inert, so dispatch paths with no read loop behind them need
// no special casing.
func TestRequestOrder_NilTokenIsInert(t *testing.T) {
	var t0 *OrderToken
	t0.WaitTurn(context.Background())
	t0.Release()

	var o *RequestOrder
	if o.Begin() != nil {
		t.Fatal("a nil RequestOrder must issue a nil token")
	}
	if OrderTokenFrom(context.Background()) != nil {
		t.Fatal("a context with no token must report none")
	}
}

// The defect this ordering exists for, driven through the real dispatch path:
// a SET_INFO rename (message 1) owes a lease break, and the CREATE the client
// pipelined behind it (message 2) is handled first. The CREATE's response must
// not reach the wire before the break, or the client acknowledges a lease key
// it has never been told.
func TestProcessSingleRequest_BreakPrecedesLaterResponse(t *testing.T) {
	serverConn, commands, cleanup := newRecordingConnPair(t)
	t.Cleanup(cleanup)

	ci := newTestConnInfo(t, serverConn)
	ci.RequestOrder = NewRequestOrder()

	rename := ci.RequestOrder.Begin() // message 1: read first, dispatched second
	create := ci.RequestOrder.Begin() // message 2: read second, dispatched first

	// Message 2 is a CREATE on a session this connection does not have, so it
	// is answered from the dispatch gate — the point is which response reaches
	// the wire first, not what it says. ECHO cannot stand in for it: ECHO is
	// deliberately exempt from the order.
	createHeader := &header.SMB2Header{
		StructureSize: header.HeaderSize,
		Command:       types.SMB2Create,
		Credits:       1,
		CreditCharge:  1,
		MessageID:     2,
		SessionID:     0x1234,
	}

	answered := make(chan error, 1)
	go func() {
		// Stands in for message 2's handler: it runs to completion long
		// before message 1's has started.
		answered <- ProcessSingleRequest(
			WithOrderToken(context.Background(), create),
			createHeader, nil, nil, ci, false, nil,
		)
		create.Release()
	}()

	select {
	case <-commands:
		t.Fatal("the later request was answered before the earlier request's break was sent")
	case <-time.After(100 * time.Millisecond):
	}

	// Message 1's handler finally runs and dispatches its break notification,
	// exactly as the lease notifier does — synchronously, before its own
	// response — then steps out of the order to wait for the acknowledgement.
	breakHeader := &header.SMB2Header{
		StructureSize: header.HeaderSize,
		Command:       types.SMB2OplockBreak,
		MessageID:     ^uint64(0),
	}
	if err := SendMessage(breakHeader, MakeErrorBody(), ci); err != nil {
		t.Fatalf("sending the break notification failed: %v", err)
	}
	rename.Release()

	if err := <-answered; err != nil {
		t.Fatalf("ProcessSingleRequest returned %v", err)
	}

	got := make([]types.Command, 0, 2)
	for range 2 {
		select {
		case cmd := <-commands:
			got = append(got, cmd)
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d frames reached the wire: %v", len(got), got)
		}
	}
	if got[0] != types.SMB2OplockBreak || got[1] != types.SMB2Create {
		t.Fatalf("wire order was %v; the break must precede the later request's response", got)
	}
}

// newRecordingConnPair returns a server-side conn whose frames are reported,
// in wire order, as the SMB2 command each one carries.
func newRecordingConnPair(t *testing.T) (net.Conn, <-chan types.Command, func()) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	commands := make(chan types.Command, 8)
	done := make(chan struct{})

	go func() {
		defer close(done)
		length := make([]byte, 4)
		for {
			if _, err := io.ReadFull(clientConn, length); err != nil {
				return
			}
			frame := make([]byte, binary.BigEndian.Uint32(length))
			if _, err := io.ReadFull(clientConn, frame); err != nil {
				return
			}
			// SMB2 header: ProtocolId(4) StructureSize(2) CreditCharge(2)
			// Status(4) Command(2) — MS-SMB2 §2.2.1.2.
			if len(frame) >= 14 {
				commands <- types.Command(binary.LittleEndian.Uint16(frame[12:14]))
			}
		}
	}()

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			_ = serverConn.Close()
			_ = clientConn.Close()
			<-done
		})
	}
	return serverConn, commands, cleanup
}

// The two-phase pattern the rename branch uses: a handler releases its place
// to wait on a break acknowledgement, then reaches its own response and asks
// for its turn a second time. It must not queue for a place it no longer
// holds — the order has moved on without it, so nothing would ever wake it and
// it would sit there until the wait timeout, on exactly the path this ordering
// exists to protect.
func TestRequestOrder_ReleasedTokenDoesNotQueueAgain(t *testing.T) {
	o := NewRequestOrder()
	o.waitTimeout = time.Hour // a stall here must hang the test, not pass it
	first := o.Begin()
	second := o.Begin()
	_ = first // never releases: the order never reaches second's sequence

	second.Release() // stepped out to wait for a break acknowledgement

	returned := make(chan struct{})
	go func() {
		second.WaitTurn(context.Background()) // now sending its own response
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("a released token queued for a place it had given up")
	}
}
