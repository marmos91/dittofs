package engine

import (
	"math/rand"
	"testing"
)

// BenchmarkPlanWindow_SinglePayloadConcurrent reproduces the rig's warm
// random-read contention: many concurrent reads of the SAME payload (fio's
// single-file random-read shape) all funnel through planWindow's global
// readaheadMu on every read (scheduleReadahead runs it whenever a remote is
// configured — i.e. always for dittofs-s3). The only shared state exercised is
// readaheadMu, so ns/op here IS the per-read frontier-update cost under
// contention. Run with -mutexprofile to confirm the lock, -cpuprofile for CPU.
func BenchmarkPlanWindow_SinglePayloadConcurrent(b *testing.B) {
	m := &Syncer{config: SyncerConfig{PrefetchBlocks: 8}}
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewSource(1)) //nolint:gosec // bench offsets
		for pb.Next() {
			start := uint64(rng.Intn(1 << 18))
			m.planWindow("payload", start, start)
		}
	})
}

// BenchmarkPlanWindow_MultiPayloadConcurrent is the multi-file shape: concurrent
// reads spread across many payloads. A per-payload/sharded lock removes the
// cross-payload contention this has today (all payloads share one readaheadMu).
func BenchmarkPlanWindow_MultiPayloadConcurrent(b *testing.B) {
	m := &Syncer{config: SyncerConfig{PrefetchBlocks: 8}}
	payloads := make([]string, 256)
	for i := range payloads {
		payloads[i] = "payload-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewSource(2)) //nolint:gosec // bench offsets
		for pb.Next() {
			p := payloads[rng.Intn(len(payloads))]
			start := uint64(rng.Intn(1 << 18))
			m.planWindow(p, start, start)
		}
	})
}
