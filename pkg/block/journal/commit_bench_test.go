package journal

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// BenchmarkConcurrentCommit reproduces the fio rand-write-4k shape that #1736
// regressed on: many concurrent 4 KiB writes each followed by a durability
// Commit (direct=1 over NFS ⇒ a server-side fsync per write), all landing on
// one shard's active segment. It measures how a burst of concurrent commits on
// the SAME segment fd coalesces — or fails to. With an uncoalesced per-shard
// fsync every commit pays a full disk barrier; a group-commit lets one barrier
// satisfy the whole in-flight batch.
//
// parallelism controls the in-flight depth (fio uses iodepth=32 × numjobs=4).
// All ops target files that hash to a single shard so the fsync contention is
// real and not spread across shards.
func benchConcurrentCommit(b *testing.B, parallelism int) {
	s := benchStore(b)
	ctx := context.Background()
	data := make([]byte, 4<<10)

	// Pick FileIDs that all resolve to shard 0 so every commit fsyncs the same fd.
	ids := make([]FileID, parallelism)
	got := 0
	for i := 0; got < parallelism; i++ {
		id := FileID(fmt.Sprintf("cc-%d", i))
		if s.shardIndex(id) == 0 {
			ids[got] = id
			got++
		}
	}

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	var wg sync.WaitGroup
	perG := b.N / parallelism
	for g := 0; g < parallelism; g++ {
		wg.Add(1)
		go func(id FileID) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if err := s.WriteAt(ctx, id, int64(i%256)*int64(len(data)), data); err != nil {
					b.Errorf("WriteAt: %v", err)
					return
				}
				if err := s.Commit(ctx, id); err != nil {
					b.Errorf("Commit: %v", err)
					return
				}
			}
		}(ids[g])
	}
	wg.Wait()
}

func BenchmarkConcurrentCommit1(b *testing.B)   { benchConcurrentCommit(b, 1) }
func BenchmarkConcurrentCommit32(b *testing.B)  { benchConcurrentCommit(b, 32) }
func BenchmarkConcurrentCommit128(b *testing.B) { benchConcurrentCommit(b, 128) }
