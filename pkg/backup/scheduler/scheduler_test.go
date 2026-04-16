package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// fakeTarget is a trivial Target implementation for tests.
type fakeTarget struct {
	id    string
	sched string
}

func (t fakeTarget) ID() string       { return t.id }
func (t fakeTarget) Schedule() string { return t.sched }

// TestScheduler exercises every documented Scheduler behavior (T1..T8) via
// sub-tests — each sub-test corresponds to one behavior in the plan's
// <behavior> block. Named sub-tests keep the failure report precise.
func TestScheduler(t *testing.T) {
	// T1: NewScheduler returns a *Scheduler with an embedded cron.Cron.
	t.Run("T1 NewScheduler initializes defaults", func(t *testing.T) {
		s := NewScheduler()
		require.NotNil(t, s, "NewScheduler must return non-nil")
		require.NotNil(t, s.cron, "internal cron.Cron must be initialized")
		require.NotNil(t, s.overlap, "overlap guard must be initialized")
		require.Equal(t, DefaultMaxJitter, s.maxJit, "default jitter should be DefaultMaxJitter")
	})

	// T2: Register with a valid schedule records the entry and indexes it by target.ID().
	t.Run("T2 Register records entry by target ID", func(t *testing.T) {
		s := NewScheduler()
		s.SetJobFn(func(ctx context.Context, targetID string) error { return nil })

		tgt := fakeTarget{id: "r1", sched: "* * * * *"}
		require.NoError(t, s.Register(tgt))
		require.True(t, s.IsRegistered("r1"), "r1 should be registered")
		ids := s.Registered()
		require.Len(t, ids, 1)
		require.Equal(t, "r1", ids[0])
	})

	// T3: Register with an invalid schedule returns ErrScheduleInvalid AND does NOT insert.
	t.Run("T3 Register rejects invalid schedule", func(t *testing.T) {
		cases := []struct{ name, sched string }{
			{"empty", ""},
			{"gibberish", "not a cron"},
			{"out of range", "99 * * * *"},
			{"too few fields", "0 *"},
			{"unknown tz", "CRON_TZ=Not/Real 0 * * * *"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := NewScheduler()
				err := s.Register(fakeTarget{id: "bad", sched: tc.sched})
				require.Error(t, err)
				require.Truef(t, errors.Is(err, models.ErrScheduleInvalid),
					"error should wrap ErrScheduleInvalid: %v", err)
				require.False(t, s.IsRegistered("bad"), "no entry should be recorded")
				require.Empty(t, s.Registered())
			})
		}
	})

	// T4: Unregister removes the entry; subsequent ticks for that repo do not fire JobFn.
	t.Run("T4 Unregister removes entry", func(t *testing.T) {
		s := NewScheduler(WithMaxJitter(0))
		var calls atomic.Int64
		s.SetJobFn(func(ctx context.Context, targetID string) error {
			calls.Add(1)
			return nil
		})

		require.NoError(t, s.Register(fakeTarget{id: "r1", sched: "* * * * *"}))
		require.True(t, s.IsRegistered("r1"))

		s.Unregister("r1")
		require.False(t, s.IsRegistered("r1"))
		require.Empty(t, s.Registered())

		// Unregister unknown ID is a no-op.
		s.Unregister("does-not-exist")

		// Subsequent fire on r1 after Unregister: the cron entry is gone;
		// calling fire() directly still routes through the overlap guard but
		// without a cron entry there's no tick. We verify via state, not fires.
		require.Equal(t, int64(0), calls.Load())
	})

	// T5: Start begins the cron loop (non-blocking); Stop cancels ctx, in-flight fires abort.
	t.Run("T5 Start and Stop are idempotent", func(t *testing.T) {
		s := NewScheduler()
		s.SetJobFn(func(ctx context.Context, targetID string) error { return nil })

		// Start without entries — should not panic.
		s.Start()
		// Start is idempotent — second call is a no-op.
		s.Start()

		require.NoError(t, s.Stop(context.Background()))
		// Stop is idempotent — safe to call twice.
		require.NoError(t, s.Stop(context.Background()))
	})

	// T5': Stop cancels in-flight fires that are sleeping on their jitter offset.
	t.Run("T5b Stop cancels in-flight jitter sleep", func(t *testing.T) {
		s := NewScheduler()
		var invoked atomic.Bool
		s.SetJobFn(func(ctx context.Context, targetID string) error {
			invoked.Store(true)
			return nil
		})

		require.NoError(t, s.Register(fakeTarget{id: "r1", sched: "* * * * *"}))
		s.Start()

		done := make(chan struct{})
		go func() {
			s.fire("r1", 1*time.Second)
			close(done)
		}()

		time.Sleep(20 * time.Millisecond)
		require.NoError(t, s.Stop(context.Background()))

		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("fire() should have returned promptly after Stop cancelled runCtx")
		}
		require.False(t, invoked.Load(), "JobFn must not run when Stop interrupts the pre-fire sleep")
	})

	// T6: Concurrent ticks on the same repo produce exactly one JobFn invocation
	//      while the first is running — overlap guard working.
	t.Run("T6 OverlapUnderLoad yields exactly one concurrent run", func(t *testing.T) {
		s := NewScheduler(WithMaxJitter(0))

		var running, peakRunning atomic.Int32
		var invocations atomic.Int64
		release := make(chan struct{})

		s.SetJobFn(func(ctx context.Context, targetID string) error {
			invocations.Add(1)
			cur := running.Add(1)
			if cur > peakRunning.Load() {
				peakRunning.Store(cur)
			}
			<-release
			running.Add(-1)
			return nil
		})

		require.NoError(t, s.Register(fakeTarget{id: "r1", sched: "* * * * *"}))
		s.Start()
		t.Cleanup(func() { _ = s.Stop(context.Background()) })

		const n = 50
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				s.fire("r1", 0)
			}()
		}
		close(start)

		deadline := time.Now().Add(2 * time.Second)
		for invocations.Load() < 1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		require.GreaterOrEqual(t, invocations.Load(), int64(1), "at least one fire must reach JobFn")

		time.Sleep(30 * time.Millisecond)
		close(release)
		wg.Wait()

		require.Equal(t, int32(1), peakRunning.Load(), "at most one JobFn should run concurrently")
		require.Equal(t, int64(1), invocations.Load(), "overlap guard must reject all but one concurrent fire")
	})

	// T7: When JobFn returns an error, the scheduler logs but does NOT remove the entry.
	t.Run("T7 JobFn error does not remove entry", func(t *testing.T) {
		s := NewScheduler(WithMaxJitter(0))
		var calls atomic.Int64
		s.SetJobFn(func(ctx context.Context, targetID string) error {
			calls.Add(1)
			return errors.New("intentional failure")
		})

		require.NoError(t, s.Register(fakeTarget{id: "r1", sched: "* * * * *"}))
		s.Start()
		t.Cleanup(func() { _ = s.Stop(context.Background()) })

		s.fire("r1", 0)
		require.Equal(t, int64(1), calls.Load())
		require.True(t, s.IsRegistered("r1"), "entry must persist after JobFn error")

		s.fire("r1", 0)
		require.Equal(t, int64(2), calls.Load())
	})

	// T8: PhaseOffset applied — fire() with a non-zero offset delays JobFn by
	//      approximately that offset.
	t.Run("T8 Jitter offset delays JobFn", func(t *testing.T) {
		s := NewScheduler()
		var invokedAt atomic.Int64
		s.SetJobFn(func(ctx context.Context, targetID string) error {
			invokedAt.Store(time.Now().UnixNano())
			return nil
		})

		require.NoError(t, s.Register(fakeTarget{id: "r1", sched: "* * * * *"}))
		s.Start()
		t.Cleanup(func() { _ = s.Stop(context.Background()) })

		start := time.Now()
		offset := 100 * time.Millisecond
		s.fire("r1", offset)

		require.NotZero(t, invokedAt.Load(), "JobFn must be invoked")
		elapsed := time.Duration(invokedAt.Load() - start.UnixNano())
		require.GreaterOrEqual(t, elapsed, offset-50*time.Millisecond,
			"JobFn should wait ~offset (got %v, wanted >= %v)", elapsed, offset-50*time.Millisecond)
		require.LessOrEqual(t, elapsed, offset+200*time.Millisecond,
			"JobFn should not wait significantly longer than offset (got %v)", elapsed)
	})
}

// TestScheduler_RegisterIdempotent — re-registering the same (ID, schedule) is a no-op.
func TestScheduler_RegisterIdempotent(t *testing.T) {
	s := NewScheduler()
	s.SetJobFn(func(ctx context.Context, targetID string) error { return nil })

	tgt := fakeTarget{id: "r1", sched: "0 * * * *"}
	require.NoError(t, s.Register(tgt))
	require.NoError(t, s.Register(tgt), "re-register same (id, schedule) should be no-op")
	require.Equal(t, 1, len(s.Registered()))
}

// TestScheduler_RegisterReplacesOnScheduleChange — re-registering same ID with a
// different schedule removes the old entry and installs the new one.
func TestScheduler_RegisterReplacesOnScheduleChange(t *testing.T) {
	s := NewScheduler()
	s.SetJobFn(func(ctx context.Context, targetID string) error { return nil })

	require.NoError(t, s.Register(fakeTarget{id: "r1", sched: "0 * * * *"}))
	require.NoError(t, s.Register(fakeTarget{id: "r1", sched: "*/5 * * * *"}))

	ids := s.Registered()
	require.Len(t, ids, 1)
	require.Equal(t, "r1", ids[0])
}

// TestScheduler_FireWithoutJobFn — fire with no JobFn set logs and returns; no panic.
func TestScheduler_FireWithoutJobFn(t *testing.T) {
	s := NewScheduler(WithMaxJitter(0))
	require.NoError(t, s.Register(fakeTarget{id: "r1", sched: "* * * * *"}))
	s.Start()
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	// Should not panic even though JobFn is nil.
	s.fire("r1", 0)
}

// TestScheduler_RegisterNilTarget — defensive: nil target returns a clean error.
func TestScheduler_RegisterNilTarget(t *testing.T) {
	s := NewScheduler()
	err := s.Register(nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, models.ErrRepoNotFound))
}

// TestScheduler_WithOverlapGuard — the same guard can be shared with an on-demand
// path (D-23). We verify the guard is actually used by calling TryLock externally
// and confirming fire() can't acquire.
func TestScheduler_WithOverlapGuard(t *testing.T) {
	shared := NewOverlapGuard()
	s := NewScheduler(WithOverlapGuard(shared), WithMaxJitter(0))

	var calls atomic.Int64
	s.SetJobFn(func(ctx context.Context, targetID string) error {
		calls.Add(1)
		return nil
	})

	require.NoError(t, s.Register(fakeTarget{id: "r1", sched: "* * * * *"}))
	s.Start()
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	// Acquire externally (simulating on-demand RunBackup holding the lock).
	unlock, ok := shared.TryLock("r1")
	require.True(t, ok)
	defer unlock()

	// Scheduler's fire should see the held lock and skip.
	s.fire("r1", 0)
	require.Equal(t, int64(0), calls.Load(), "scheduler should skip when shared guard is held externally")
}

// TestScheduler_OverlapUnderLoad_Standalone exposes the T6 scenario as a named
// test so the acceptance criteria "go test -run TestScheduler_OverlapUnderLoad
// -count=5 -race" has a target.
func TestScheduler_OverlapUnderLoad(t *testing.T) {
	s := NewScheduler(WithMaxJitter(0))

	var running, peakRunning atomic.Int32
	var invocations atomic.Int64
	release := make(chan struct{})

	s.SetJobFn(func(ctx context.Context, targetID string) error {
		invocations.Add(1)
		cur := running.Add(1)
		if cur > peakRunning.Load() {
			peakRunning.Store(cur)
		}
		<-release
		running.Add(-1)
		return nil
	})

	require.NoError(t, s.Register(fakeTarget{id: "r1", sched: "* * * * *"}))
	s.Start()
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	const n = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.fire("r1", 0)
		}()
	}
	close(start)

	deadline := time.Now().Add(2 * time.Second)
	for invocations.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.GreaterOrEqual(t, invocations.Load(), int64(1))

	time.Sleep(30 * time.Millisecond)
	close(release)
	wg.Wait()

	require.Equal(t, int32(1), peakRunning.Load(), "exactly one concurrent JobFn")
	require.Equal(t, int64(1), invocations.Load(), "one winner, all others skipped")
}
