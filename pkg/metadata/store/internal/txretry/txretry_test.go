package txretry

import (
	"context"
	"testing"
	"time"
)

func TestBackoff_ExhaustedBudgetReturnsFalse(t *testing.T) {
	// A deadline already in the past leaves no budget: no wait, no retry.
	if Backoff(context.Background(), time.Now().Add(-time.Second), 0) {
		t.Fatal("Backoff past deadline should return false")
	}
}

func TestBackoff_RetriesWithinBudget(t *testing.T) {
	// A comfortable deadline yields a retry after a bounded wait.
	start := time.Now()
	if !Backoff(context.Background(), start.Add(time.Second), 0) {
		t.Fatal("Backoff within budget should return true")
	}
	// attempt 0 caps at baseBackoff (5ms); the actual wait is jittered in
	// (0, 5ms], so it must not exceed maxBackoff.
	if waited := time.Since(start); waited > maxBackoff {
		t.Fatalf("attempt-0 wait %v exceeded maxBackoff %v", waited, maxBackoff)
	}
}

func TestBackoff_CtxCancelStopsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Backoff(ctx, time.Now().Add(time.Second), 0) {
		t.Fatal("Backoff with cancelled ctx should return false")
	}
}

func TestDeadline_TightensToEarlierCtxDeadline(t *testing.T) {
	// A ctx deadline sooner than now+budget wins.
	soon := time.Now().Add(50 * time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), soon)
	defer cancel()
	if got := Deadline(ctx); got.After(soon) {
		t.Fatalf("Deadline %v should not exceed earlier ctx deadline %v", got, soon)
	}
}

func TestDeadline_DefaultsToBudget(t *testing.T) {
	// No ctx deadline: now+budget, within a small slack.
	got := Deadline(context.Background())
	want := time.Now().Add(Budget)
	if diff := got.Sub(want); diff > 100*time.Millisecond || diff < -100*time.Millisecond {
		t.Fatalf("Deadline %v not ~now+budget %v", got, want)
	}
}

// TestWaitOrCancel_CancelledCtxWithAReadyTimer pins the cancellation check
// against the case where the timer is ready too.
//
// A select picks uniformly among ready cases, so when a cancelled context and a
// fired timer are both ready the timer's branch wins about half the time —
// returning "retry" for a request whose caller has already given up. Feeding an
// already-fired channel makes both cases ready on every iteration, so this
// fails against the unguarded version deterministically rather than waiting for
// a loaded runner to deschedule the goroutine past the wait.
//
// Driving the real Backoff cannot do that: whether its jittered timer has fired
// by the time the select is reached depends on the machine, and on a fast one
// only Done() is ready, which the unguarded version also answers correctly.
func TestWaitOrCancel_CancelledCtxWithAReadyTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 200; i++ {
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		if waitOrCancel(ctx, fired) {
			t.Fatalf("retried a cancelled ctx on iteration %d", i)
		}
	}
}

// TestWaitOrCancel_LiveCtxTakesTheTimer is the other half: an uncancelled
// context must still retry when the timer fires, or the guard above would have
// turned every backoff into a give-up.
func TestWaitOrCancel_LiveCtxTakesTheTimer(t *testing.T) {
	fired := make(chan time.Time, 1)
	fired <- time.Now()
	if !waitOrCancel(context.Background(), fired) {
		t.Fatal("a live ctx must retry when the timer fires")
	}
}
