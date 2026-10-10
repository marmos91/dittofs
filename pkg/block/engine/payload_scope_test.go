package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

func awaitPayloadScope(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("payload scope did not finish")
		return nil
	}
}

func TestPayloadScopeCancellationReleasesPartialAcquisition(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()
	owned := make(chan struct{})
	unblock := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- bs.WithPayloadScope(ctx, []string{"b"}, true, func(context.Context) error { close(owned); <-unblock; return nil })
	}()
	<-owned
	cancelled, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	err := bs.WithPayloadScope(cancelled, []string{"b", "a"}, true, func(context.Context) error { t.Error("cancelled callback ran"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire: %v", err)
	}
	// Sorted acquisition first obtained a, which must have been released when
	// acquiring b failed. Cancellation must also retire the waiter reference.
	if err := bs.WithPayloadScope(ctx, []string{"a"}, true, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	close(unblock)
	if err := awaitPayloadScope(t, done); err != nil {
		t.Fatal(err)
	}
	bs.admission.mu.Lock()
	defer bs.admission.mu.Unlock()
	if len(bs.admission.entries) != 0 {
		t.Fatalf("leaked admission entries: %d", len(bs.admission.entries))
	}
}

func TestPayloadScopeNestedOperationsWhileCloseQueued(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()
	entered := make(chan struct{})
	resume := make(chan struct{})
	operation := make(chan error, 1)
	go func() {
		operation <- bs.WithPayloadScope(ctx, []string{"p", "p"}, true, func(ctx context.Context) error {
			close(entered)
			<-resume
			if _, err := bs.WriteAt(ctx, "p", nil, []byte("data"), 0); err != nil {
				return err
			}
			if _, err := bs.ReadAt(ctx, "p", make([]byte, 4), 0); err != nil {
				return err
			}
			_, err := bs.Flush(ctx, "p")
			return err
		})
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- bs.Close() }()
	// Confirm the lifecycle writer is queued: a new try-read fails while the
	// existing callback still owns its read hold.
	deadline := time.Now().Add(5 * time.Second)
	for bs.closeMu.TryRLock() {
		bs.closeMu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("Close never queued")
		}
		time.Sleep(time.Millisecond)
	}
	close(resume)
	if err := awaitPayloadScope(t, operation); err != nil {
		t.Fatal(err)
	}
	if err := awaitPayloadScope(t, closed); err != nil {
		t.Fatal(err)
	}
	if err := bs.WithPayloadScope(ctx, []string{"p"}, false, func(context.Context) error { return nil }); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("post-Close scope: %v", err)
	}
}

func TestPayloadScopeOwnershipIsSpecificToStoreAndPayload(t *testing.T) {
	a, b := newTestEngine(t, 0, 0), newTestEngine(t, 0, 0)
	var retained context.Context
	err := a.WithPayloadScope(context.Background(), []string{"p"}, false, func(ctx context.Context) error {
		retained = ctx
		if err := a.WithPayloadScope(ctx, []string{"p"}, true, func(context.Context) error { return nil }); err == nil {
			t.Error("shared ownership was upgraded")
		}
		if err := a.WithPayloadScope(ctx, []string{"q"}, false, func(context.Context) error { return nil }); err == nil {
			t.Error("ownership silently expanded")
		}
		return b.WithPayloadScope(ctx, []string{"p"}, true, func(ctx context.Context) error {
			// A scope for b must neither mask a's scope nor confer b's exclusive
			// ownership on a. Reusing a is legal, upgrading it still is not.
			if err := a.WithPayloadScope(ctx, []string{"p"}, true, func(context.Context) error { return nil }); err == nil {
				t.Error("other store granted exclusive ownership")
			}
			_, err := a.WriteAt(ctx, "p", nil, []byte("x"), 0)
			return err
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if scopeFor(retained, &a.admission) != nil {
		t.Fatal("callback context retained admission after return")
	}
}

func BenchmarkPayloadScope(b *testing.B) {
	for _, nested := range []bool{false, true} {
		name := "outer"
		if nested {
			name = "nested"
		}
		b.Run(name, func(b *testing.B) {
			bs := &Store{}
			ctx := context.Background()
			ids := []string{"payload"}
			callback := func(context.Context) error { return nil }
			if nested {
				callback = func(ctx context.Context) error {
					return bs.WithPayloadScope(ctx, ids, false, func(context.Context) error { return nil })
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := bs.WithPayloadScope(ctx, ids, false, callback); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestTryPayloadScopeReleasesPartialAcquisitionWithoutChangingEpoch(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()
	version, releaseObserver, err := bs.ObservePayload(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseObserver()
	err = bs.WithPayloadScope(ctx, []string{"b"}, false, func(context.Context) error {
		tryCtx, cancelTry := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancelTry()
		entered, err := bs.TryWithPayloadScope(tryCtx, []string{"b", "a"}, true, func(context.Context) error {
			t.Error("callback ran despite busy second payload")
			return nil
		})
		if err != nil || entered {
			t.Fatalf("try: entered=%v err=%v", entered, err)
		}
		if got := version(); got != 0 {
			t.Fatalf("failed try changed epoch to %d", got)
		}
		// A retained partial acquisition or queued exclusive waiter would block
		// one of these shared operations until the outer holder released b.
		waitCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return bs.WithPayloadScope(waitCtx, []string{"a", "b"}, false, func(context.Context) error { return nil })
	})
	if err != nil {
		t.Fatal(err)
	}
	err = bs.WithPayloadScope(ctx, []string{"a"}, true, func(context.Context) error {
		if version() != 0 {
			t.Error("exclusive epoch changed before operation finished")
		}
		return nil
	})
	if err != nil || version() != 1 {
		t.Fatalf("exclusive epoch: version=%d err=%v", version(), err)
	}
	// The observer retains the same entry after ordinary scope references end.
	if err := bs.WithPayloadScope(ctx, []string{"a"}, false, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if version() != 1 {
		t.Fatal("shared scope reset or changed the epoch")
	}
	bs.admission.mu.Lock()
	defer bs.admission.mu.Unlock()
	if len(bs.admission.entries) != 1 || bs.admission.entries["a"].refs != 1 {
		t.Fatalf("unexpected observer references: %#v", bs.admission.entries)
	}
}

func TestTryPayloadScopeCancellationDoesNotEnter(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	entered, err := bs.TryWithPayloadScope(ctx, []string{"a"}, true, func(context.Context) error {
		t.Error("cancelled callback ran")
		return nil
	})
	if entered || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled try: entered=%v err=%v", entered, err)
	}
	if len(bs.admission.entries) != 0 {
		t.Fatal("cancelled try leaked an admission entry")
	}
}
