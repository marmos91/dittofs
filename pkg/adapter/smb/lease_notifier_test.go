package smb

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
)

// fakeTransport records each SendFrame call.
type fakeTransport struct {
	calls   atomic.Int32
	failErr error
}

func (f *fakeTransport) SendFrame(sessionID uint64, plaintext []byte, encrypt bool) error {
	f.calls.Add(1)
	if f.failErr != nil {
		return f.failErr
	}
	return nil
}

// fakeSessionLookup implements sessionHandlerAPI with a static map.
type fakeSessionLookup struct {
	sessions map[uint64]*session.Session
}

func (f *fakeSessionLookup) GetSession(sessionID uint64) (*session.Session, bool) {
	sess, ok := f.sessions[sessionID]
	return sess, ok
}

func TestOrderedTransports_SkipsChannelsWithNilTransport(t *testing.T) {
	sess := session.NewSession(42, "127.0.0.1:445", false, "user", "")
	ready := &fakeTransport{}
	sess.AddChannel(&session.Channel{ConnID: 1, Transport: nil})
	sess.AddChannel(&session.Channel{ConnID: 2, Transport: ready})

	n := &transportNotifier{
		sessionConns:  &sync.Map{},
		oplockFileIDs: &sync.Map{},
		handler:       &fakeSessionLookup{sessions: map[uint64]*session.Session{42: sess}},
	}

	transports := n.orderedTransports(42, sess)
	if len(transports) != 1 {
		t.Fatalf("expected 1 transport (nil filtered), got %d", len(transports))
	}
}

func TestOrderedTransports_EmptyWhenNoChannelsOrFallback(t *testing.T) {
	sess := session.NewSession(42, "127.0.0.1:445", false, "user", "")
	n := &transportNotifier{
		sessionConns:  &sync.Map{},
		oplockFileIDs: &sync.Map{},
		handler:       &fakeSessionLookup{sessions: map[uint64]*session.Session{42: sess}},
	}
	if got := n.orderedTransports(42, sess); len(got) != 0 {
		t.Errorf("expected empty transports for session without channels, got %d", len(got))
	}
}

func TestSendLeaseBreak_DeliversOnceOnSingleChannel(t *testing.T) {
	// With one channel bound to the session, the notifier delivers exactly
	// once — NOT once per channel.
	sess := session.NewSession(42, "127.0.0.1:445", false, "user", "")
	only := &fakeTransport{}
	sess.AddChannel(&session.Channel{ConnID: 1, Transport: only})

	n := &transportNotifier{
		sessionConns:  &sync.Map{},
		oplockFileIDs: &sync.Map{},
		handler:       &fakeSessionLookup{sessions: map[uint64]*session.Session{42: sess}},
	}

	var leaseKey [16]byte
	if err := n.SendLeaseBreak(42, leaseKey, 0, 0, 0); err != nil {
		t.Fatalf("SendLeaseBreak: %v", err)
	}
	if only.calls.Load() != 1 {
		t.Errorf("expected 1 delivery, got %d", only.calls.Load())
	}
}

func TestSendLeaseBreak_FailsOverToNextChannel(t *testing.T) {
	// Broadcasting is wrong (duplicate breaks), but a single-channel failure
	// must fail over to the next bound channel.
	sess := session.NewSession(42, "127.0.0.1:445", false, "user", "")
	failing := &fakeTransport{failErr: errors.New("broken pipe")}
	ok := &fakeTransport{}
	sess.AddChannel(&session.Channel{ConnID: 1, Transport: failing})
	sess.AddChannel(&session.Channel{ConnID: 2, Transport: ok})

	n := &transportNotifier{
		sessionConns:  &sync.Map{},
		oplockFileIDs: &sync.Map{},
		handler:       &fakeSessionLookup{sessions: map[uint64]*session.Session{42: sess}},
	}

	var leaseKey [16]byte
	if err := n.SendLeaseBreak(42, leaseKey, 0, 0, 0); err != nil {
		t.Fatalf("expected nil error when failover succeeds, got %v", err)
	}
	if ok.calls.Load() != 1 {
		t.Errorf("expected failover delivery to ok channel, got %d calls", ok.calls.Load())
	}
}

func TestSendLeaseBreak_AllChannelsFailingReturnsError(t *testing.T) {
	sess := session.NewSession(42, "127.0.0.1:445", false, "user", "")
	a := &fakeTransport{failErr: errors.New("dead")}
	b := &fakeTransport{failErr: errors.New("dead")}
	sess.AddChannel(&session.Channel{ConnID: 1, Transport: a})
	sess.AddChannel(&session.Channel{ConnID: 2, Transport: b})

	n := &transportNotifier{
		sessionConns:  &sync.Map{},
		oplockFileIDs: &sync.Map{},
		handler:       &fakeSessionLookup{sessions: map[uint64]*session.Session{42: sess}},
	}

	var leaseKey [16]byte
	if err := n.SendLeaseBreak(42, leaseKey, 0, 0, 0); err == nil {
		t.Fatal("expected error when all channels fail, got nil")
	}
}
