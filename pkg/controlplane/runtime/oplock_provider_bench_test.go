package runtime

import "testing"

// BenchmarkOplockBreakerLookup compares the old per-op RWMutex+map provider
// lookup (GetAdapterProvider) against the atomic.Value fast path
// (OplockBreakerProvider) that the NFS read/write hot path now uses. Both the
// no-SMB (nil) and registered cases are measured; RunParallel exposes the
// RWMutex read contention that the atomic load avoids.
func BenchmarkOplockBreakerLookup(b *testing.B) {
	newRT := func(register bool) *Runtime {
		r := &Runtime{adapterProviders: make(map[string]any)}
		if register {
			r.SetAdapterProvider(oplockBreakerProviderKey, struct{}{})
		}
		return r
	}

	b.Run("map_noSMB", func(b *testing.B) {
		r := newRT(false)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = r.GetAdapterProvider(oplockBreakerProviderKey)
			}
		})
	})
	b.Run("atomic_noSMB", func(b *testing.B) {
		r := newRT(false)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = r.OplockBreakerProvider()
			}
		})
	})
	b.Run("map_registered", func(b *testing.B) {
		r := newRT(true)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = r.GetAdapterProvider(oplockBreakerProviderKey)
			}
		})
	})
	b.Run("atomic_registered", func(b *testing.B) {
		r := newRT(true)
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = r.OplockBreakerProvider()
			}
		})
	})
}
