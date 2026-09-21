package state

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// shortenRetryDelays collapses the backoff between send attempts. What these
// tests assert is what happens once every attempt has failed, not how long the
// waits between them are. No test in this package runs in parallel, so nothing
// else reads this.
func shortenRetryDelays(t *testing.T) {
	t.Helper()
	saved := backchannelRetryDelays
	backchannelRetryDelays = [backchannelMaxRetries]time.Duration{
		time.Millisecond, time.Millisecond, time.Millisecond,
	}
	t.Cleanup(func() { backchannelRetryDelays = saved })
}

// TestSendCallback_WritesToEveryBackBoundConnection covers the walk past write
// failures.
//
// A session may hold up to maxConnsPerSession back-bound connections, and the
// freshest are the ones most likely to be unusable. Stopping after a fixed
// number of them leaves a working route untried, and the send is then reported
// as no callback path at all: the client's verdict is cleared and the recall
// that provoked it revokes a delegation the client could still have been told
// about.
func TestSendCallback_WritesToEveryBackBoundConnection(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()
	sender.callbackTimeout = 200 * time.Millisecond

	// Two connections whose writes fail, and a third that takes the bytes. The
	// walk used to stop at the second.
	for _, connID := range []uint64{7501, 7502} {
		sm.RegisterConnWriter(connID, func([]byte) error {
			return fmt.Errorf("connection reset by peer")
		})
		if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
			t.Fatalf("BindConnToSession(%d): %v", connID, err)
		}
	}

	var wroteOnLive atomic.Bool
	const liveConnID = uint64(7503)
	sm.RegisterConnWriter(liveConnID, func([]byte) error {
		wroteOnLive.Store(true)
		return nil
	})
	if _, err := sm.BindConnToSession(liveConnID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession(live): %v", err)
	}

	// Candidates are ranked most recently active first, and the stamps are set
	// here rather than left to the order the binds happened in: a tie would be
	// broken by insertion order, and which connection is tried first is the
	// whole subject of the test. The two that fail are the freshest, which is
	// also the shape that makes this reachable in the first place.
	sm.connMu.Lock()
	now := time.Now()
	for _, b := range sm.connBySession[sessionID] {
		if b.ConnectionID == liveConnID {
			b.LastActivity = now.Add(-time.Minute)
		} else {
			b.LastActivity = now
		}
	}
	sm.connMu.Unlock()

	// The reply never comes, so the send ends at its own timeout. What matters
	// is that the third connection was reached at all.
	err := sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte("walk-file")),
	})
	if err == nil {
		t.Fatal("the send reported success although no reply was ever delivered")
	}
	if !wroteOnLive.Load() {
		t.Error("the walk gave up with a usable back-bound connection still untried")
	}
}

// TestSendCallback_ExhaustedWalkIsNotReportedAsNeverAttempted pins the
// classification the walk had to carry over rather than a defect it fixes: the
// two-candidate version already got this right, and the rewrite is where it
// would most easily be lost.
//
// Bytes that reached a transport and failed there are evidence about the
// client. Reporting an exhausted walk as "never attempted" instead would have
// the recall call the whole send local, leaving CBPathUp standing on a route
// that provably carries nothing.
func TestSendCallback_ExhaustedWalkIsNotReportedAsNeverAttempted(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()
	sender.callbackTimeout = 200 * time.Millisecond

	for _, connID := range []uint64{7601, 7602, 7603} {
		sm.RegisterConnWriter(connID, func([]byte) error {
			return fmt.Errorf("connection reset by peer")
		})
		if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
			t.Fatalf("BindConnToSession(%d): %v", connID, err)
		}
	}

	err := sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte("all-fail-file")),
	})
	if err == nil {
		t.Fatal("the send reported success although every write failed")
	}
	if errors.Is(err, errCallbackNotAttempted) {
		t.Errorf("a send whose every write reached a transport and failed was reported as never attempted: %v", err)
	}
}

// TestSendRecallV41_KeepsTheCurrentFailureOverAStaleOne covers which failure
// the walk hands to the currency guard.
//
// Retaining the last failure of any generation lets a session whose callback
// parameters were replaced mid-attempt displace an earlier, still-current
// failure. The guard then discards the stale one and applies nothing, so
// CBPathUp stays true although a session did fail against the parameters it
// still holds — and the next OPEN is offered a delegation on that verdict.
func TestSendRecallV41_KeepsTheCurrentFailureOverAStaleOne(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	defer sm.Shutdown()
	shortenRetryDelays(t)

	clientID, sessions := newBackchannelClient(t, sm, "stale-failure-client", 2)

	// Every write fails, so both sessions report a transport failure. The walk
	// starts at the first session and moves on to the second, whose writer
	// replaces its own callback parameters — so its failure is stamped with a
	// generation that is already superseded by the time it is recorded, while
	// the first session's failure is still current.
	var tried [2]atomic.Bool
	for i, sessionID := range sessions {
		connID := uint64(7701 + i)
		sender := sm.GetSession(sessionID).backchannelSender
		stale := i == 1
		reached := &tried[i]
		sm.RegisterConnWriter(connID, func([]byte) error {
			reached.Store(true)
			if stale {
				sender.setParams(0x40000001, []types.CallbackSecParms4{{CbSecFlavor: 0}})
			}
			return fmt.Errorf("connection reset by peer")
		})
		if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
			t.Fatalf("BindConnToSession(%d): %v", connID, err)
		}
		sender.callbackTimeout = 100 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go sender.Run(ctx)
	}
	sm.setCBPathUp(clientID, true)

	deleg := &DelegationState{
		ClientID:   clientID,
		FileHandle: []byte("stale-failure-file"),
		DelegType:  types.OPEN_DELEGATE_READ,
		Stateid:    types.Stateid4{Seqid: 1},
	}

	done := make(chan struct{})
	go func() {
		sm.sendRecallV41(deleg, sm.GetSession(sessions[0]).backchannelSender)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("sendRecallV41 did not return")
	}
	deleg.StopRecallTimer()

	if !tried[0].Load() || !tried[1].Load() {
		t.Fatalf("the walk reached sessions %v, so no stale failure could displace a current one",
			[2]bool{tried[0].Load(), tried[1].Load()})
	}
	if cbPathUp(sm, clientID) {
		t.Error("a stale-generation failure displaced the current-generation one, so no verdict was applied")
	}
}

// TestSendCallback_WalkStopsAtItsBudget covers what bounds the walk now that it
// is no longer capped at a fixed number of candidates.
//
// Every write can spend a full write timeout before it fails, and the candidate
// set is re-read as the walk goes, so a session holding many connections would
// run far past the budget the recall watchdog charges for the whole send. The
// watchdog would then call a recall wedged and revoke its delegation five
// seconds later with the callback still on the wire, which is the outcome the
// derived bound exists to prevent.
func TestSendCallback_WalkStopsAtItsBudget(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()
	sender.callbackTimeout = 300 * time.Millisecond

	// Eight connections whose writes each burn most of the budget and then
	// fail. Unbounded, the walk would spend eight of them.
	const writeCost = 100 * time.Millisecond
	var writes atomic.Int32
	for i := 0; i < 8; i++ {
		connID := uint64(7801 + i)
		sm.RegisterConnWriter(connID, func([]byte) error {
			writes.Add(1)
			time.Sleep(writeCost)
			return fmt.Errorf("connection reset by peer")
		})
		if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
			t.Fatalf("BindConnToSession(%d): %v", connID, err)
		}
	}

	start := time.Now()
	err := sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte("budget-file")),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the send reported success although every write failed")
	}
	if errors.Is(err, errCallbackNotAttempted) {
		t.Errorf("a walk cut short by its budget was reported as never attempted: %v", err)
	}
	// The deadline decides whether another write may start, so the walk is
	// bounded by it plus the one write that crosses it — which is what
	// worstCaseSendDuration charges. One extra write of scheduling margin on
	// top; it may overshoot by one write but not by five.
	if limit := sender.callbackTimeout + 2*writeCost; elapsed > limit {
		t.Errorf("the walk ran for %s against a budget of %s (limit %s) after %d writes",
			elapsed, sender.callbackTimeout, limit, writes.Load())
	}
}

// TestWorstCaseSendDuration_CoversTheWalkThatCrossesItsDeadline is the check
// that the watchdog's arithmetic and the walk's actual bound agree.
//
// The walk's deadline decides whether another write may start, not when the
// walk ends, so the phase runs to the deadline plus whichever write crosses it.
// A bound that charged the walk one budget would be exactly consumed by walk
// plus reply wait, leaving no slack — and an attempt finishing on its bound
// races the watchdog that is supposed to outlast it, which reports a recall
// wedged and revokes its delegation with the callback still on the wire.
func TestWorstCaseSendDuration_CoversTheWalkThatCrossesItsDeadline(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()
	sender.callbackTimeout = 400 * time.Millisecond

	// The backoff between attempts is charged to the bound as well, and at its
	// real length it dwarfs the per-attempt budget this is measuring — the
	// comparison below would hold against any walk at all.
	shortenRetryDelays(t)

	// Writes that each consume most of the budget, so one of them starts just
	// under the deadline and carries the walk past it.
	const writeCost = 300 * time.Millisecond
	for i := 0; i < 6; i++ {
		connID := uint64(7901 + i)
		sm.RegisterConnWriter(connID, func([]byte) error {
			time.Sleep(writeCost)
			return fmt.Errorf("connection reset by peer")
		})
		if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
			t.Fatalf("BindConnToSession(%d): %v", connID, err)
		}
	}

	start := time.Now()
	_ = sender.sendCallback(context.Background(), CallbackRequest{
		OpCode:  types.OP_CB_RECALL,
		Payload: EncodeCBRecallOp(&types.Stateid4{Seqid: 1}, false, []byte("bound-file")),
	})
	walk := time.Since(start)

	// What the bound allocates one attempt, net of the reply wait and the slack.
	perAttempt := sender.worstCaseSendDuration() / backchannelMaxRetries
	if allowed := perAttempt - 2*sender.callbackTimeout; walk > allowed {
		t.Errorf("the walk ran for %s but the bound allocates it %s per attempt "+
			"(%s total, less the reply wait and the slack)",
			walk, allowed, perAttempt)
	}
}
