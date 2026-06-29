package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// gcListLatencyRemote wraps a remote store and injects a fixed per-page latency into
// Walk, modeling an S3 ListObjectsV2 paginated LIST (one network round-trip per
// `pageSize` keys). The index sweep never calls Walk, so it pays none of this —
// that gap is exactly the cost this change removes. Get/Delete/etc. fall through
// to the embedded store unchanged.
type gcListLatencyRemote struct {
	remote.RemoteStore
	pageSize    int
	pageLatency time.Duration
}

func (l *gcListLatencyRemote) Walk(ctx context.Context, fn func(block.ContentHash, block.Meta) error) error {
	n := 0
	return l.RemoteStore.Walk(ctx, func(h block.ContentHash, m block.Meta) error {
		if n%l.pageSize == 0 {
			time.Sleep(l.pageLatency) // one ListObjectsV2 round-trip per page
		}
		n++
		return fn(h, m)
	})
}

// allHeldProvider injects every hash in the set as "held", so the entire object
// population is live: the sweep makes ZERO deletes and we measure only the
// candidate-enumeration cost (S3 LIST vs local index scan), nothing else.
type allHeldProvider struct{ hashes []block.ContentHash }

func (p allHeldProvider) HeldHashes(_ context.Context, _ string, _ []string, fn func(block.ContentHash) error) error {
	for _, h := range p.hashes {
		if err := fn(h); err != nil {
			return err
		}
	}
	return nil
}

// TestGCSweepLatencyBenchmark compares the wall-clock sweep cost of the
// namespace Walk sweep (S3 LIST, FullScan=true) against the index sweep
// (FullScan=false, no LIST) over an all-live population, with a per-page latency
// modeling real S3 LIST pagination round-trips. Run explicitly:
//
//	go test ./pkg/block/engine/ -run TestGCSweepLatencyBenchmark -v -timeout 20m
//
// It is skipped under -short. The headline: Walk cost grows ~linearly with
// object count (N/pageSize round-trips); index cost stays flat (local scan).
func TestGCSweepLatencyBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("latency benchmark skipped under -short")
	}
	const (
		pageSize    = 1000                  // S3 ListObjectsV2 keys per page
		pageLatency = 20 * time.Millisecond // modeled LIST round-trip per page
	)

	ctx := context.Background()
	pastGrace := time.Now().Add(-2 * time.Hour)

	for _, n := range []int{10_000, 100_000} {
		// Build the all-live population once, reuse across both sweeps.
		rs := remotememory.New()
		t.Cleanup(func() { _ = rs.Close() })
		rec := newGCMSReconciler()
		_ = rec.addShare("bench")
		synced := newRecordingSyncedHashStore()
		hashes := make([]block.ContentHash, n)
		for i := 0; i < n; i++ {
			h := hashFromString(fmt.Sprintf("bench-%d-%d", n, i))
			hashes[i] = h
			if err := rs.Put(ctx, h, []byte("x")); err != nil {
				t.Fatalf("seed remote: %v", err)
			}
			synced.markSyncedAtForTest(h, pastGrace)
		}
		hold := allHeldProvider{hashes: hashes}
		lat := &gcListLatencyRemote{RemoteStore: rs, pageSize: pageSize, pageLatency: pageLatency}

		run := func(fullScan bool) (time.Duration, *GCStats) {
			opts := &Options{
				GCStateRoot:     t.TempDir(),
				GracePeriod:     time.Minute,
				SyncedHashIndex: synced,
				HoldProvider:    hold,
				FullScan:        fullScan,
			}
			start := time.Now()
			stats := CollectGarbage(ctx, lat, rec, opts)
			return time.Since(start), stats
		}

		// FullScan=true → Walk sweep (pays the modeled S3 LIST latency).
		walkDur, walkStats := run(true)
		// FullScan=false → index sweep (no LIST).
		idxDur, idxStats := run(false)

		if walkStats.ObjectsSwept != 0 || idxStats.ObjectsSwept != 0 {
			t.Fatalf("expected 0 sweeps (all live): walk=%d index=%d",
				walkStats.ObjectsSwept, idxStats.ObjectsSwept)
		}
		pages := n / pageSize
		t.Logf("N=%-7d  walk(LIST)=%-10v  index(no-LIST)=%-10v  speedup=%.1fx  (modeled %d LIST pages @ %v)",
			n, walkDur.Round(time.Millisecond), idxDur.Round(time.Millisecond),
			float64(walkDur)/float64(idxDur), pages, pageLatency)
	}
}
