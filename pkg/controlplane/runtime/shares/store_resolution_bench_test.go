package shares

import (
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/engine"
)

// These benchmarks measure the per-op cost of resolving a share's block store,
// the path every NFS/SMB read/write takes. "before" is the old RWMutex+map
// lookup; "after" is the lock-free sync.Map fast path that GetBlockStoreForShare
// now uses. Run with -cpu to see the contention delta under concurrency:
//
//	go test -run x -bench BenchmarkResolveBlockStore -cpu 1,8,32 ./pkg/controlplane/runtime/shares/
func benchService(shares int) (*Service, []string) {
	s := New()
	names := make([]string, shares)
	for i := range names {
		name := fmt.Sprintf("share-%d", i)
		names[i] = name
		// Sentinel store: resolution never dereferences it.
		bs := &engine.Store{}
		s.registry[name] = &Share{Name: name, BlockStore: bs}
		s.blockStoreCache.Store(name, bs)
	}
	return s, names
}

// resolveLocked replicates the pre-cache implementation so the benchmark shows a
// true before/after on the same data structure.
func (s *Service) resolveLocked(name string) (*engine.Store, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	share, ok := s.registry[name]
	if !ok {
		return nil, fmt.Errorf("share %q not found", name)
	}
	if share.BlockStore == nil {
		return nil, fmt.Errorf("share %q has no block store configured", name)
	}
	return share.BlockStore, nil
}

func BenchmarkResolveBlockStore_Before_RWMutex(b *testing.B) {
	s, names := benchService(16)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := s.resolveLocked(names[i%len(names)]); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

func BenchmarkResolveBlockStore_After_SyncMap(b *testing.B) {
	s, names := benchService(16)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := s.GetBlockStoreForShare(names[i%len(names)]); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}
