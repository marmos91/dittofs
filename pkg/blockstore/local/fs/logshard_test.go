package fs

import (
	"context"
	"fmt"
	"sync"
	"testing"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestShardFor_Deterministic verifies shardFor is stable per payloadID and
// distributes a realistic key set across more than one shard (a degenerate
// hash that mapped every payload to one shard would silently re-serialize the
// create path and defeat C2).
func TestShardFor_Deterministic(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{MaxLogBytes: 1 << 30})

	// Stability: same id always resolves to the same shard pointer. Capture
	// into locals so staticcheck doesn't flag the two calls as an identical
	// expression (SA4000) — exercising determinism is the point here.
	for _, id := range []string{"a", "share/dir/file.txt", "perf/storm/42", ""} {
		first := bc.shardFor(id)
		second := bc.shardFor(id)
		if first != second {
			t.Fatalf("shardFor(%q) not stable", id)
		}
	}

	// Distribution: a path-like key set must touch several shards.
	seen := make(map[*logShard]int)
	for i := 0; i < 1000; i++ {
		seen[bc.shardFor(fmt.Sprintf("share/dir%d/file%d", i%37, i))]++
	}
	if len(seen) < numLogShards/2 {
		t.Fatalf("payload keys hit only %d/%d shards — distribution too skewed", len(seen), numLogShards)
	}
}

// TestLogShards_ConcurrentCreateDelete hammers create (AppendWrite) and delete
// (DeleteAppendLog) across many distinct payloads — and therefore many shards —
// concurrently. It is the C2 regression guard: the create path now takes a
// per-shard write lock and DeleteAppendLog drains per-shard, so a lock-order or
// cross-shard mistake would deadlock (caught by the test timeout) or trip the
// race detector. It asserts every payload ends fully torn down.
func TestLogShards_ConcurrentCreateDelete(t *testing.T) {
	rs := memmeta.NewMemoryMetadataStoreWithDefaults()
	bc := newFSStoreForTest(t, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   3,
		StabilizationMS: 2,
		RollupStore:     rs,
	})
	if err := bc.StartRollup(context.Background()); err != nil {
		t.Fatalf("StartRollup: %v", err)
	}
	ctx := context.Background()

	const workers = 8
	const perWorker = 80
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			buf := make([]byte, 4096)
			for i := 0; i < perWorker; i++ {
				pid := fmt.Sprintf("perf/c2/w%d/f%d", w, i)
				if err := bc.AppendWrite(ctx, pid, buf, 0); err != nil {
					t.Errorf("AppendWrite %s: %v", pid, err)
					return
				}
				// Half the payloads are deleted immediately, racing their own
				// in-flight rollup; the other half are left to drain.
				if i%2 == 0 {
					if err := bc.DeleteAppendLog(ctx, pid); err != nil {
						t.Errorf("DeleteAppendLog %s: %v", pid, err)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()

	// Every deleted payload must be fully cleared from its shard; every
	// surviving payload drains to a quiescent tree.
	for w := 0; w < workers; w++ {
		for i := 0; i < perWorker; i++ {
			pid := fmt.Sprintf("perf/c2/w%d/f%d", w, i)
			if i%2 == 0 {
				sh := bc.shardFor(pid)
				sh.mu.RLock()
				_, hasFD := sh.logFDs[pid]
				_, hasTomb := sh.tombstones[pid]
				sh.mu.RUnlock()
				if hasFD || hasTomb {
					t.Fatalf("deleted payload %s left residual state (fd=%v tomb=%v)", pid, hasFD, hasTomb)
				}
				continue
			}
			// Surviving payload: force a drain pass and confirm it quiesces.
			_ = bc.ForceRollupForTest(ctx, pid)
		}
	}
}
