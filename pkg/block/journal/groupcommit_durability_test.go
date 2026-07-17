package journal

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
)

// The group-commit's entire durability guarantee rests on the leader's fsync
// actually happening and covering the right bytes. The crash-recovery tests
// reopen from the same tmpfiles, where the OS page cache serves the bytes
// whether or not fsync ran — so they cannot tell a real fsync from a no-op.
// These tests use the per-shard segSync seam to assert the fsync directly:
// removing or breaking it makes them fail, which is what proves they aren't
// hollow (see #1736 durability review).

// sameShardIDs returns count FileIDs that all hash to shardIdx, so their commits
// contend on one shard's fsync (the batching the tests need to exercise).
func sameShardIDs(t *testing.T, s *Store, shardIdx uint64, count int) []FileID {
	t.Helper()
	var ids []FileID
	for i := 0; len(ids) < count; i++ {
		id := FileID(fmt.Sprintf("gc-%d", i))
		if s.shardIndex(id) == shardIdx {
			ids = append(ids, id)
		}
	}
	return ids
}

// TestGroupCommit_FsyncOnCommit proves a Commit actually issues an fsync. If the
// group-commit ever stopped fsyncing (the failure the crash tests can't catch),
// this fails.
func TestGroupCommit_FsyncOnCommit(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	sh := s.shardFor("f")

	var syncs int32
	sh.segSync = func(seg *segmentMeta) error {
		atomic.AddInt32(&syncs, 1)
		return seg.fd.Sync()
	}

	if err := s.WriteAt(ctx, "f", 0, []byte("x")); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Commit(ctx, "f"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if atomic.LoadInt32(&syncs) == 0 {
		t.Fatal("Commit did not fsync the segment — durability guarantee is hollow")
	}
}

// TestGroupCommit_FsyncErrorPropagates proves a failed fsync surfaces to the
// caller (leader path), and that the sticky error is scoped to the failed batch —
// a later commit whose fsync succeeds returns nil.
func TestGroupCommit_FsyncErrorPropagates(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	sh := s.shardFor("f")

	injected := errors.New("injected fsync EIO")
	var fail atomic.Bool
	fail.Store(true)
	sh.segSync = func(seg *segmentMeta) error {
		if fail.Load() {
			return injected
		}
		return seg.fd.Sync()
	}

	if err := s.WriteAt(ctx, "f", 0, []byte("x")); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Commit(ctx, "f"); err == nil {
		t.Fatal("Commit must return an error when the fsync fails")
	}

	// Recover: a subsequent commit whose fsync succeeds must return nil — the
	// sticky error watermark cannot poison later commits.
	fail.Store(false)
	if err := s.WriteAt(ctx, "f", 0, []byte("y")); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Commit(ctx, "f"); err != nil {
		t.Fatalf("commit after a recovered fsync should succeed, got %v", err)
	}
}

// TestGroupCommit_ErrorReachesBatchedFollower is the regression test for the
// review finding: a follower batched onto a leader's fsync must return that
// fsync's error, and must NOT be able to read a later successful batch's nil.
// It deterministically builds a 2-member batch whose fsync errors and asserts
// BOTH the leader and the piggybacking follower fail.
func TestGroupCommit_ErrorReachesBatchedFollower(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	ids := sameShardIDs(t, s, 0, 3)
	sh := s.shardFor(ids[0])

	inSync := make(chan int)    // spy announces it entered (Nth call)
	release := make(chan error) // test injects each call's result
	var calls int32
	sh.segSync = func(seg *segmentMeta) error {
		inSync <- int(atomic.AddInt32(&calls, 1))
		return <-release
	}

	for _, id := range ids {
		if err := s.WriteAt(ctx, id, 0, []byte("x")); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
	}

	errC := make(chan error, 3)

	// Leader L: its batch covers only itself; it blocks inside segSync.
	go func() { errC <- s.Commit(ctx, ids[0]) }()
	if n := <-inSync; n != 1 {
		t.Fatalf("expected first sync call, got %d", n)
	}

	// F1, F2 enqueue as waiters while L holds the fsync. Wait until both have
	// bumped reqSeq so the next batch is guaranteed to cover both of them.
	go func() { errC <- s.Commit(ctx, ids[1]) }()
	go func() { errC <- s.Commit(ctx, ids[2]) }()
	waitReqSeq(sh, 3)

	// Release L successfully; F1 then leads batch B (covers F1 + F2).
	release <- nil
	if err := <-errC; err != nil {
		t.Fatalf("leader L should have succeeded, got %v", err)
	}

	// Batch B's fsync errors — both its leader and its batched follower must fail.
	if n := <-inSync; n != 2 {
		t.Fatalf("expected second sync call, got %d", n)
	}
	release <- errors.New("injected fsync EIO")

	e1, e2 := <-errC, <-errC
	if e1 == nil || e2 == nil {
		t.Fatalf("a batched errored fsync must fail BOTH commits: e1=%v e2=%v", e1, e2)
	}

	// The failed watermark is scoped: a fresh commit with a healthy fsync succeeds.
	sh.segSync = func(seg *segmentMeta) error { return seg.fd.Sync() }
	if err := s.Commit(ctx, ids[0]); err != nil {
		t.Fatalf("commit after the errored batch should succeed, got %v", err)
	}
}

// waitReqSeq spins until the shard has enqueued at least target commits.
func waitReqSeq(sh *shard, target uint64) {
	for {
		sh.commitMu.Lock()
		r := sh.reqSeq
		sh.commitMu.Unlock()
		if r >= target {
			return
		}
		runtime.Gosched()
	}
}
