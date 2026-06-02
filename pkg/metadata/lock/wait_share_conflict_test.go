package lock

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WaitForShareConflictClear backs the SMB CREATE deferred-open resume for the
// share-violation case (smbtorture replay
// dhv2-pending1n-vs-violation-lease-{close,ack}-sane). It differs from
// WaitForBreakCompletion in two ways these tests pin:
//
//  1. It returns as soon as the caller's live share-mode predicate clears
//     (modelling the conflicting holder CLOSEing its open) — without any
//     break-wait signal, since file-lease CLOSEs do not signal the channel.
//  2. On ctx timeout it does NOT force-complete (tombstone) the holder's
//     breaking lease, so the holder's later ACK still succeeds.

// TestWaitForShareConflictClear_ClearsOnConflictDrop proves the wait returns nil
// promptly once the predicate reports the conflict gone, even with the holder's
// lease still breaking and no break-wait signal fired (the file-CLOSE path).
func TestWaitForShareConflictClear_ClearsOnConflictDrop(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	ctx := context.Background()
	holderKey := [16]byte{1, 0, 0, 0}

	// Holder takes an RWH lease and is broken (Handle strip) by a conflicting open.
	_, _, err := lm.RequestLease(ctx, FileHandle("file1"), holderKey, [16]byte{}, "owner1", "client1", "/share",
		LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	require.NoError(t, err)
	require.NoError(t, lm.BreakLeasesOnOpenConflict("file1", nil, BreakReasonSharingViolation))

	var conflict atomic.Bool
	conflict.Store(true)

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- lm.WaitForShareConflictClear(waitCtx, "file1", func() bool { return conflict.Load() })
	}()

	// Simulate the holder CLOSEing (its conflicting open is removed) without any
	// break-wait signal — the poll must catch the cleared predicate.
	time.Sleep(150 * time.Millisecond)
	conflict.Store(false)

	select {
	case err := <-done:
		assert.NoError(t, err, "wait must return nil once the conflict clears")
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForShareConflictClear did not return after the conflict cleared")
	}
}

// TestWaitForShareConflictClear_ReturnsWhenBreakDrainsButConflictPersists proves
// the deterministic ack-sane exit: once the holder ACKs its break (Breaking
// cleared) but keeps its open (predicate still true), the wait returns promptly
// so the caller's final recheck yields SHARING_VIOLATION — without stalling to
// the deadline.
func TestWaitForShareConflictClear_ReturnsWhenBreakDrainsButConflictPersists(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	ctx := context.Background()
	holderKey := [16]byte{2, 0, 0, 0}

	_, _, err := lm.RequestLease(ctx, FileHandle("file2"), holderKey, [16]byte{}, "owner1", "client1", "/share",
		LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	require.NoError(t, err)
	require.NoError(t, lm.BreakLeasesOnOpenConflict("file2", nil, BreakReasonSharingViolation))

	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		// Conflict persists forever (holder keeps its open after ACK).
		done <- lm.WaitForShareConflictClear(waitCtx, "file2", func() bool { return true })
	}()

	// Holder ACKs the break to RW (Handle stripped), keeping its open. This
	// clears Breaking; the wait must then exit on the no-breaking-lease branch.
	time.Sleep(150 * time.Millisecond)
	require.NoError(t, lm.AcknowledgeLeaseBreak(ctx, holderKey, LeaseStateRead|LeaseStateWrite, 0))

	select {
	case err := <-done:
		assert.NoError(t, err, "wait must return nil (no err) once the break drains, even with the conflict live")
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForShareConflictClear did not return after the break drained")
	}

	// The holder's lease must NOT have been force-completed to None: the ACK
	// already moved it to RW and the wait left it intact.
	state, _, found := lm.GetLeaseState(ctx, holderKey)
	require.True(t, found, "holder lease must still exist (not tombstoned)")
	assert.Equal(t, LeaseStateRead|LeaseStateWrite, state, "ACKed RW must survive — the wait must not force-complete it")
}

// TestWaitForShareConflictClear_TimeoutDoesNotForceComplete proves that a
// genuine never-released conflict times out (ctx error) AND leaves the holder's
// breaking lease intact — so a holder that ACKs after the deadline still
// succeeds (the ack-sane UNSUCCESSFUL bug is gone).
func TestWaitForShareConflictClear_TimeoutDoesNotForceComplete(t *testing.T) {
	t.Parallel()

	lm := NewManager()
	ctx := context.Background()
	holderKey := [16]byte{3, 0, 0, 0}

	_, _, err := lm.RequestLease(ctx, FileHandle("file3"), holderKey, [16]byte{}, "owner1", "client1", "/share",
		LeaseStateRead|LeaseStateWrite|LeaseStateHandle, false)
	require.NoError(t, err)
	require.NoError(t, lm.BreakLeasesOnOpenConflict("file3", nil, BreakReasonSharingViolation))

	// Pin the break Breaking across the wait: a callback is required so the
	// break does not auto-resolve. With no registered callback the break stays
	// pending (Breaking=true) until ACK or force-complete; assert that the wait
	// times out WITHOUT force-completing.
	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	// Conflict persists AND the break never drains (no ACK, no callback).
	err = lm.WaitForShareConflictClear(waitCtx, "file3", func() bool { return true })
	require.Error(t, err, "a never-released conflict must time out")
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// The lease must NOT be a timeout tombstone — it is still breaking, so a
	// late ACK succeeds (contrast WaitForBreakCompletion, which would have
	// force-revoked it to None and made the ACK fail STATUS_UNSUCCESSFUL).
	require.NoError(t, lm.AcknowledgeLeaseBreak(ctx, holderKey, LeaseStateRead|LeaseStateWrite, 0),
		"holder ACK after the deferred-open timeout must still succeed (lease not tombstoned)")
}
