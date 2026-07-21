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
	want := time.Now().Add(budget)
	if diff := got.Sub(want); diff > 100*time.Millisecond || diff < -100*time.Millisecond {
		t.Fatalf("Deadline %v not ~now+budget %v", got, want)
	}
}
