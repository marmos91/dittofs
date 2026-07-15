package runtime

import "testing"

// TestOplockBreakerProviderStoreSafety guards that registering a nil provider,
// then re-registering with a different concrete type, never panics the
// atomic.Value (Store panics on a nil interface or a changing concrete type).
func TestOplockBreakerProviderStoreSafety(t *testing.T) {
	r := &Runtime{adapterProviders: make(map[string]any)}
	if got := r.OplockBreakerProvider(); got != nil {
		t.Fatalf("unregistered = %v, want nil", got)
	}
	r.SetAdapterProvider(oplockBreakerProviderKey, nil) // nil interface
	if got := r.OplockBreakerProvider(); got != nil {
		t.Fatalf("after nil register = %v, want nil", got)
	}
	r.SetAdapterProvider(oplockBreakerProviderKey, struct{ a int }{1})
	r.SetAdapterProvider(oplockBreakerProviderKey, "different-type") // type changes
	if got := r.OplockBreakerProvider(); got != "different-type" {
		t.Fatalf("after re-register = %v, want different-type", got)
	}
}

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
