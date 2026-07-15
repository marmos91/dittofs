package metadata

import (
	"sync"
	"sync/atomic"
	"testing"
)

// BenchmarkDeferredCommitGate compares the old RWMutex-guarded bool read that
// CommitWrite performed on every write op against the atomic.Bool load that
// replaced it. Run with -cpu=8 (or higher) to see the RWMutex read serialize
// under concurrent writers while the atomic load stays flat.
func BenchmarkDeferredCommitGate(b *testing.B) {
	b.Run("mutex", func(b *testing.B) {
		var mu sync.RWMutex
		flag := true
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				mu.RLock()
				_ = flag
				mu.RUnlock()
			}
		})
	})
	b.Run("atomic", func(b *testing.B) {
		var flag atomic.Bool
		flag.Store(true)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = flag.Load()
			}
		})
	})
}
