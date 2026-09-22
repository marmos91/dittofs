package journal

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

// BenchmarkWave7GroupCommit_ShardFanOut isolates the one variable the adapter
// benchmark could only guess at.
//
// A FILE_SYNC write through the v3 handlers costs ~130x an UNSTABLE one, and
// almost none of that is the caller's own syscall time -- it waits for a
// shard's group-commit leader to fsync for it. Concurrency should therefore
// amortise one fsync over many writers, but measured through the adapter,
// eight writers bought only 1.5x the throughput.
//
// Group commit is per shard, and shards are picked by hashing the FileID, so
// writers touching distinct files land on distinct leaders and each pays its
// own fsync. This sweeps ShardCount with the rest held fixed: if that is the
// mechanism, per-op cost is lowest at one shard and rises with the count. If
// cost is flat across the sweep, the fan-out is not what limits the batching
// and the adapter result needs a different explanation.
//
//	go test ./pkg/block/journal/ -run xxx \
//	    -bench Wave7GroupCommit_ShardFanOut -benchtime 200x -cpu 8
//
// Read it at a fixed -cpu: the point is the trend across shard counts, and
// varying both at once confounds them. Absolute values are macOS F_FULLFSYNC
// and do not transfer; the trend does.
func BenchmarkWave7GroupCommit_ShardFanOut(b *testing.B) {
	for _, shards := range []int{1, 2, 4, 16} {
		b.Run(fmt.Sprintf("shards=%d", shards), func(b *testing.B) {
			s, err := openJournal(b.TempDir(), Config{ShardCount: shards})
			if err != nil {
				b.Fatalf("Open: %v", err)
			}
			b.Cleanup(func() { _ = s.Close() })

			ctx := context.Background()
			data := make([]byte, 4096)
			var seq atomic.Uint64

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					// A distinct FileID per operation, which is what a
					// create-heavy workload does and what spreads the writers
					// across shards.
					id := FileID(fmt.Sprintf("w7-%d", seq.Add(1)))
					if err := s.WriteAt(ctx, id, 0, data); err != nil {
						b.Fatalf("WriteAt: %v", err)
					}
					if err := s.Commit(ctx, id); err != nil {
						b.Fatalf("Commit: %v", err)
					}
				}
			})
		})
	}
}

// BenchmarkWave7GroupCommit_SharedFile is the control for the sweep above.
//
// Every writer commits the same FileID, so every writer lands on one shard and
// one leader. This is the shape group commit is built for, and it bounds what
// the fan-out sweep could recover: if the shared-file case is not markedly
// cheaper per operation than shards=16, then batching is weak regardless of
// how writers are distributed and shard fan-out was never the lever.
func BenchmarkWave7GroupCommit_SharedFile(b *testing.B) {
	s, err := openJournal(b.TempDir(), Config{})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	data := make([]byte, 4096)
	const id = FileID("w7-shared")
	var off atomic.Int64

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := s.WriteAt(ctx, id, off.Add(int64(len(data)))-int64(len(data)), data); err != nil {
				b.Fatalf("WriteAt: %v", err)
			}
			if err := s.Commit(ctx, id); err != nil {
				b.Fatalf("Commit: %v", err)
			}
		}
	})
}
