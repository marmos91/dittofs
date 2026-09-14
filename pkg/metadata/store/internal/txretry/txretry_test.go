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

// TestBackoff_CtxCancelWinsAgainstAReadyTimer pins the cancellation check
// against the case where the backoff timer is ready too.
//
// A select picks uniformly among ready cases. A cancelled ctx makes Done()
// ready at once, and a budget this small leaves a jittered wait of at most a
// microsecond, so the timer becomes ready alongside it — at which point
// returning the timer's branch retries a request whose context is already
// cancelled. The single-shot sibling test above only catches this when the
// runner happens to deschedule the goroutine past the wait, which is why it
// failed in CI and passed locally.
func TestBackoff_CtxCancelWinsAgainstAReadyTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 2000; i++ {
		if Backoff(ctx, time.Now().Add(time.Microsecond), 0) {
			t.Fatalf("Backoff retried a cancelled ctx on iteration %d", i)
		}
	}
}
