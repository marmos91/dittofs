package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/stretchr/testify/require"
)

func TestWarmScopeCloseAfterProgressCancelsPeerDownload(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.remote.gateHash = f.dstRow.Hash
	callback := make(chan struct{})
	warmed := make(chan error, 1)
	var callbackErr error
	var result WarmResult
	go func() {
		var err error
		result, err = f.bs.WarmAll(ctx, func(done, total int64) {
			if done != 1 {
				return
			}
			select {
			case <-f.remote.entered:
			case <-ctx.Done():
				callbackErr = ctx.Err()
				return
			}
			close(callback)
			callbackErr = f.bs.Close()
		})
		warmed <- err
	}()
	select {
	case <-callback:
	case <-time.After(5 * time.Second):
		cancel()
		_ = waitWarmScope(t, warmed)
		t.Fatal("warm never reported progress while its peer download was stalled")
	}
	var err error
	select {
	case err = <-warmed:
	case <-time.After(2 * time.Second):
		t.Error("Close from nonzero progress waited for the stalled peer download")
		cancel() // Release a regressed worker so the test and cleanup can finish.
		err = waitWarmScope(t, warmed)
	}
	require.ErrorIs(t, err, ErrStoreClosed)
	require.NoError(t, callbackErr)
	require.NoError(t, ctx.Err(), "Close must cancel the warm run, not its caller")
	require.EqualValues(t, 1, result.BlocksFetched)
	require.Zero(t, f.remote.inFlight.Load())
}

func TestWarmScopeReadAfterProgressWithCloseQueued(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.remote.gateHash = f.dstRow.Hash

	// Keep an ordinary operation active so Close remains queued even if it
	// promptly cancels the other warm worker. Its context shares the caller's
	// parent but must not be cancelled when warming stops.
	ordinaryEntered := make(chan struct{})
	releaseOrdinary := make(chan struct{})
	ordinary := make(chan error, 1)
	go func() {
		ordinary <- f.bs.WithPayloadScope(ctx, []string{"unrelated"}, false, func(ctx context.Context) error {
			close(ordinaryEntered)
			<-releaseOrdinary
			return ctx.Err()
		})
	}()
	<-ordinaryEntered
	callback := make(chan struct{})
	readNow := make(chan struct{})
	warmed := make(chan error, 1)
	var callbackErr error
	go func() {
		_, err := f.bs.WarmAll(ctx, func(done, total int64) {
			if done != 1 {
				return
			}
			select {
			case <-f.remote.entered:
			case <-ctx.Done():
				callbackErr = ctx.Err()
				return
			}
			close(callback)
			<-readNow
			_, callbackErr = f.bs.GetSize(ctx, "warm-src")
		})
		warmed <- err
	}()
	select {
	case <-callback:
	case <-time.After(5 * time.Second):
		close(readNow)
		close(releaseOrdinary)
		cancel()
		_ = waitWarmScope(t, warmed)
		t.Fatal("warm never reached nonzero progress with its peer stalled")
	}
	closed := make(chan error, 1)
	go func() { closed <- f.bs.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for f.bs.closeMu.TryRLock() {
		f.bs.closeMu.RUnlock()
		if time.Now().After(deadline) {
			close(readNow)
			close(releaseOrdinary)
			cancel()
			t.Fatal("Close never queued behind the ordinary operation")
		}
		time.Sleep(time.Millisecond)
	}
	close(readNow)
	close(releaseOrdinary)
	require.NoError(t, waitWarmScope(t, ordinary))
	var err error
	select {
	case err = <-warmed:
	case <-time.After(2 * time.Second):
		t.Error("GetSize from nonzero progress remained behind Close and a stalled warm download")
		cancel()
		err = waitWarmScope(t, warmed)
	}
	require.NoError(t, waitWarmScope(t, closed))
	require.ErrorIs(t, err, ErrStoreClosed)
	require.ErrorIs(t, callbackErr, ErrStoreClosed)
	require.NoError(t, ctx.Err(), "shutdown must leave the caller and ordinary operation contexts live")
	require.Zero(t, f.remote.inFlight.Load())
}

func TestWarmScopeUnregistersEveryExit(t *testing.T) {
	for _, outcome := range []string{"success", "cancel", "fetch error", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			f := newWarmScopeFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("warm callback or download failed")
			if outcome == "fetch error" {
				f.remote.hook = func(context.Context, block.ContentHash) error { return failure }
			}
			var err error
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				_, err = f.bs.WarmAll(ctx, func(done, total int64) {
					if outcome == "cancel" && done == 0 {
						cancel()
					}
					if outcome == "panic" && done == 1 {
						panic(failure)
					}
				})
			}()
			switch outcome {
			case "success":
				require.NoError(t, err)
			case "cancel":
				require.ErrorIs(t, err, context.Canceled)
			case "fetch error":
				require.ErrorIs(t, err, failure)
			case "panic":
				require.Equal(t, failure, recovered)
			}
			if outcome != "panic" {
				require.Nil(t, recovered)
			}
			f.bs.warming.mu.Lock()
			registered := len(f.bs.warming.runs)
			f.bs.warming.mu.Unlock()
			require.Zero(t, registered)
			require.Zero(t, f.remote.inFlight.Load())
		})
	}
}

func TestWarmScopeCloseCancelsAllRegisteredRuns(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 4)
	f.remote.hook = func(ctx context.Context, _ block.ContentHash) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	warmed := make(chan error, 2)
	for range 2 {
		go func() { _, err := f.bs.WarmAll(ctx, nil); warmed <- err }()
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("both warm runs did not start their workers")
		}
	}
	closed := make(chan error, 1)
	go func() { closed <- f.bs.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Error("Close did not cancel all registered warm runs")
		cancel()
		require.NoError(t, waitWarmScope(t, closed))
	}
	for range 2 {
		require.ErrorIs(t, waitWarmScope(t, warmed), ErrStoreClosed)
	}
	require.NoError(t, ctx.Err())
	_, err := f.bs.WarmAll(ctx, nil)
	require.ErrorIs(t, err, ErrStoreClosed, "a closed engine must reject another warm registration")
	f.bs.warming.mu.Lock()
	defer f.bs.warming.mu.Unlock()
	require.Empty(t, f.bs.warming.runs)
}

func TestWarmScopeFinalProgressMayCloseSuccessfulRun(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	var callbackErr error
	result, err := f.bs.WarmAll(ctx, func(done, total int64) {
		if done > 0 && done == total {
			callbackErr = f.bs.Close()
		}
	})
	require.NoError(t, callbackErr)
	require.NoError(t, err, "closing from the final callback must preserve completed work")
	require.EqualValues(t, 2, result.BlocksFetched)
}

func TestWarmScopeCallerDeadlineIsPreserved(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	f.remote.gateHash = f.dstRow.Hash
	_, err := f.bs.WarmAll(ctx, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, ErrStoreClosed)
	require.Zero(t, f.remote.inFlight.Load())
	f.bs.warming.mu.Lock()
	defer f.bs.warming.mu.Unlock()
	require.Empty(t, f.bs.warming.runs)
}

func TestWarmScopeCallerCancellationCauseIsPreserved(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	_, err := f.bs.WarmAll(ctx, func(done, total int64) {
		if done == 0 {
			cancel(ErrStoreClosed)
		}
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrStoreClosed, "the caller's cause must not masquerade as engine shutdown")
	_, err = f.bs.GetSize(context.Background(), "warm-src")
	require.NoError(t, err, "caller cancellation must leave the store open")
}

func TestWarmScopeRejectsRegistrationWhileCloseIsQueued(t *testing.T) {
	f := newWarmScopeFixture(t)
	ctx := context.Background()
	_, release, err := f.bs.enterPayload(ctx, "unrelated")
	require.NoError(t, err)
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	closed := make(chan error, 1)
	go func() { closed <- f.bs.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for f.bs.closeMu.TryRLock() {
		f.bs.closeMu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("Close never queued")
		}
		time.Sleep(time.Millisecond)
	}
	warmed := make(chan error, 1)
	go func() { _, err := f.bs.WarmAll(ctx, nil); warmed <- err }()
	var warmErr error
	returned := false
	select {
	case warmErr = <-warmed:
		returned = true
	case <-time.After(time.Second):
		t.Error("a new warm registration waited behind shutdown")
	}
	release()
	released = true
	require.NoError(t, waitWarmScope(t, closed))
	if !returned {
		warmErr = waitWarmScope(t, warmed)
	}
	require.ErrorIs(t, warmErr, ErrStoreClosed)
	require.Zero(t, f.remote.reads.Load())
}
