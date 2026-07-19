package journal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// chunk256 is a payload sized so four appends overflow a 1 MiB segment, giving
// tests a deterministic seal boundary.
const chunk256 = 256 << 10

// evictStore opens a single-shard, 1 MiB-segment store on a settable clock so a
// test controls both which shard a file lands in and each segment's lastAccess.
func evictStore(t *testing.T, cfg Config) (*Store, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	if cfg.ShardCount == 0 {
		cfg.ShardCount = 1
	}
	if cfg.SegmentSize == 0 {
		cfg.SegmentSize = minSegmentSize
	}
	s, err := Open(t.TempDir(), cfg, newFakeRemote(), clk)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

// fillUntilSealed writes fixed-size chunks to id (all distinct offsets, so all
// stay live) until the shard has at least wantSealed sealed segments. synced
// picks Hydrate (evictable) vs WriteAt (dirty).
func fillUntilSealed(t *testing.T, s *Store, id FileID, synced bool, wantSealed int) {
	t.Helper()
	ctx := context.Background()
	sh := s.shardFor(id)
	buf := bytes.Repeat([]byte{0xAB}, chunk256)
	var off int64
	for {
		sh.mu.Lock()
		n := len(sh.sealed)
		sh.mu.Unlock()
		if n >= wantSealed {
			return
		}
		var err error
		if synced {
			err = s.Hydrate(ctx, id, off, buf)
		} else {
			err = s.WriteAt(ctx, id, off, buf)
		}
		if err != nil {
			t.Fatalf("fill write: %v", err)
		}
		off += chunk256
	}
}

func sealedSegs(sh *shard) []*segmentMeta {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	out := make([]*segmentMeta, 0, len(sh.sealed))
	for _, seg := range sh.sealed {
		out = append(out, seg)
	}
	return out
}

func TestEvictColdestSyncedSegment(t *testing.T) {
	s, _ := evictStore(t, Config{})
	ctx := context.Background()

	fillUntilSealed(t, s, "f", true, 2)
	segs := sealedSegs(s.shardFor("f"))
	if len(segs) < 2 {
		t.Fatalf("want >=2 sealed segments, got %d", len(segs))
	}
	// Make one segment strictly coldest.
	var cold, warm *segmentMeta
	segs[0].lastAccess.Store(100)
	segs[1].lastAccess.Store(200)
	cold, warm = segs[0], segs[1]
	for _, seg := range segs[2:] {
		seg.lastAccess.Store(300)
	}

	before := s.diskBytes.Load()
	res, err := s.Evict(ctx, 0)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted != 1 {
		t.Fatalf("targetBytes=0 must evict exactly one segment, got %d", res.SegmentsEvicted)
	}
	if _, err := os.Stat(s.segPath(cold.id)); !os.IsNotExist(err) {
		t.Fatalf("coldest segment %d should be unlinked, stat err=%v", cold.id, err)
	}
	if _, err := os.Stat(s.segPath(warm.id)); err != nil {
		t.Fatalf("warmer segment %d must survive, stat err=%v", warm.id, err)
	}
	if s.diskBytes.Load() != before-res.BytesFreed {
		t.Fatalf("diskBytes not reconciled: got %d want %d", s.diskBytes.Load(), before-res.BytesFreed)
	}
}

// TestEvictSkipsClaimedSegment: a segment already claimed (busy, e.g. GC mid-op)
// must not abort the eviction pass — the next-coldest evictable segment is taken
// instead, and the claimed one is left untouched.
func TestEvictSkipsClaimedSegment(t *testing.T) {
	s, _ := evictStore(t, Config{})
	ctx := context.Background()

	fillUntilSealed(t, s, "f", true, 2)
	segs := sealedSegs(s.shardFor("f"))
	if len(segs) < 2 {
		t.Fatalf("want >=2 sealed segments, got %d", len(segs))
	}
	// segs[0] is the coldest but is claimed out from under eviction.
	segs[0].lastAccess.Store(100)
	segs[1].lastAccess.Store(200)
	for _, seg := range segs[2:] {
		seg.lastAccess.Store(300)
	}
	claimed, next := segs[0], segs[1]
	claimed.busy.Store(true)

	res, err := s.Evict(ctx, 0)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted != 1 {
		t.Fatalf("a claimed coldest must not abort the pass, evicted %d", res.SegmentsEvicted)
	}
	if _, err := os.Stat(s.segPath(next.id)); !os.IsNotExist(err) {
		t.Fatalf("next-coldest %d should be evicted, stat err=%v", next.id, err)
	}
	if _, err := os.Stat(s.segPath(claimed.id)); err != nil {
		t.Fatalf("claimed segment %d must be left untouched, stat err=%v", claimed.id, err)
	}
}

func TestEvictSyncedGateRefusesDirty(t *testing.T) {
	s, _ := evictStore(t, Config{})
	ctx := context.Background()

	fillUntilSealed(t, s, "f", false, 1) // dirty
	if s.UnsyncedBytes() <= 0 {
		t.Fatalf("expected pinned unsynced bytes, got %d", s.UnsyncedBytes())
	}
	segs := sealedSegs(s.shardFor("f"))
	res, err := s.Evict(ctx, 1<<30)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted != 0 {
		t.Fatalf("synced-gate must refuse dirty segments, evicted %d", res.SegmentsEvicted)
	}
	for _, seg := range segs {
		if _, err := os.Stat(s.segPath(seg.id)); err != nil {
			t.Fatalf("dirty segment %d must not be evicted, stat err=%v", seg.id, err)
		}
	}
}

func TestEvictedRangeReadsCold(t *testing.T) {
	s, _ := evictStore(t, Config{})
	ctx := context.Background()

	target := bytes.Repeat([]byte{0x5A}, chunk256)
	if err := s.Hydrate(ctx, "f", 0, target); err != nil {
		t.Fatalf("Hydrate target: %v", err)
	}
	fillUntilSealed(t, s, "f", true, 2) // seal the segment holding the target

	res, err := s.Evict(ctx, 1<<30) // evict every synced segment
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted == 0 {
		t.Fatalf("expected to evict synced segments")
	}

	// The evicted range must read back as cold (not a zero-filled hole).
	dst := make([]byte, chunk256)
	_, cold, err := s.ReadAt(ctx, "f", 0, dst)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !cold {
		t.Fatalf("evicted range must report cold, not a hole")
	}
	// DataExtents must still report the evicted range as present.
	ext, err := s.DataExtents(ctx, "f", chunk256)
	if err != nil {
		t.Fatalf("DataExtents: %v", err)
	}
	if len(ext) == 0 || ext[0][0] != 0 {
		t.Fatalf("evicted range must remain in DataExtents, got %v", ext)
	}
}

// TestPressureBackpressureDirtyPinned is the journal-internal analog of the
// blockstoretest PressureChannel_INV05 probe (skipped there because it needs
// backend internals): once the local cap is exceeded and every segment is
// pinned by unsynced bytes, the write path backpressures and finally fails with
// ErrLocalStoreFull (the cap is a soft pressure threshold; ErrLocalStoreFull
// signals genuinely-nothing-evictable, not mere overshoot).
func TestPressureBackpressureDirtyPinned(t *testing.T) {
	s, _ := evictStore(t, Config{MaxLocalBytes: 2 << 20, EvictMaxWait: 50 * time.Millisecond})
	ctx := context.Background()

	buf := bytes.Repeat([]byte{0xCD}, chunk256)
	var gotFull bool
	var off int64
	for i := 0; i < 64; i++ {
		if err := s.WriteAt(ctx, "f", off, buf); err != nil {
			if errors.Is(err, ErrLocalStoreFull) {
				gotFull = true
				break
			}
			t.Fatalf("WriteAt: %v", err)
		}
		off += chunk256
	}
	if !gotFull {
		t.Fatalf("expected ErrLocalStoreFull once dirty segments pin the cap")
	}
	if s.UnsyncedBytes() <= 0 {
		t.Fatalf("dirty-pinned pressure must surface via UnsyncedBytes, got %d", s.UnsyncedBytes())
	}
}

// TestEnsureSpaceEvictsSyncedUnderPressure is the release side: synced segments
// are evictable, so writes past the cap keep succeeding by shedding cold ones.
func TestEnsureSpaceEvictsSyncedUnderPressure(t *testing.T) {
	s, _ := evictStore(t, Config{MaxLocalBytes: 2 << 20, EvictMaxWait: 50 * time.Millisecond})
	ctx := context.Background()

	buf := bytes.Repeat([]byte{0x11}, chunk256)
	var off int64
	for i := 0; i < 40; i++ { // ~10 MiB through a 2 MiB cap
		if err := s.Hydrate(ctx, "f", off, buf); err != nil {
			t.Fatalf("Hydrate under pressure: %v", err)
		}
		off += chunk256
	}
	if s.diskBytes.Load() > (2<<20)+s.cfg.SegmentSize {
		t.Fatalf("disk usage %d ran away past cap+one-segment", s.diskBytes.Load())
	}
}

// TestEvictForceSealsSyncedActive covers a working set smaller than the
// segment-roll threshold: every synced record sits in the never-sealed active
// segment, so before the force-seal fall-through an explicit Evict freed nothing.
// Now Evict seals the fully-synced active and reclaims it.
func TestEvictForceSealsSyncedActive(t *testing.T) {
	s, _ := evictStore(t, Config{}) // 1 shard, 1 MiB segment
	ctx := context.Background()

	// Two synced 256 KiB records (512 KiB total) stay in the active segment: well
	// under the 1 MiB roll threshold, so nothing seals on its own.
	buf := bytes.Repeat([]byte{0x3C}, chunk256)
	if err := s.Hydrate(ctx, "f", 0, buf); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if err := s.Hydrate(ctx, "f", chunk256, buf); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if segs := sealedSegs(s.shardFor("f")); len(segs) != 0 {
		t.Fatalf("working set below roll threshold must leave 0 sealed segments, got %d", len(segs))
	}

	before := s.diskBytes.Load()
	res, err := s.Evict(ctx, 1<<30)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted < 1 || res.BytesFreed <= 0 {
		t.Fatalf("force-seal fall-through must reclaim the synced active, got %+v", res)
	}
	if s.diskBytes.Load() >= before {
		t.Fatalf("local disk usage must drop: before=%d after=%d", before, s.diskBytes.Load())
	}
	// The reclaimed range must read back cold (remote-backed), never as a hole.
	dst := make([]byte, chunk256)
	if _, cold, err := s.ReadAt(ctx, "f", 0, dst); err != nil {
		t.Fatalf("ReadAt: %v", err)
	} else if !cold {
		t.Fatalf("evicted range must report cold, not a hole")
	}
}

// TestEvictForceSealSkipsDirtyActive proves the fall-through never seals (and so
// never strands) unsynced bytes: an active segment holding dirty records is left
// untouched, and Evict reclaims nothing.
func TestEvictForceSealSkipsDirtyActive(t *testing.T) {
	s, _ := evictStore(t, Config{})
	ctx := context.Background()

	buf := bytes.Repeat([]byte{0xD1}, chunk256)
	if err := s.WriteAt(ctx, "f", 0, buf); err != nil { // dirty, unsynced
		t.Fatalf("WriteAt: %v", err)
	}
	res, err := s.Evict(ctx, 1<<30)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if res.SegmentsEvicted != 0 {
		t.Fatalf("dirty active must not be sealed-and-evicted, got %+v", res)
	}
	if segs := sealedSegs(s.shardFor("f")); len(segs) != 0 {
		t.Fatalf("dirty active must not be force-sealed, got %d sealed", len(segs))
	}
}

// pretendCarveOneSealed models one slow syncer step: it flips the first dirty
// sealed segment it finds to fully synced (as carve does after uploading its
// records) and drops the drained bytes from the unsynced counter, so the segment
// becomes evictable and the write-path backpressure sees drain progress. It
// returns false when no dirty sealed segment remains.
func pretendCarveOneSealed(s *Store) bool {
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, seg := range sh.sealed {
			if seg.syncedRecords.Load() < seg.records.Load() {
				seg.syncedRecords.Store(seg.records.Load())
				drained := seg.liveBytes.Load()
				sh.mu.Unlock()
				s.unsynced.Add(-drained)
				return true
			}
		}
		sh.mu.Unlock()
	}
	return false
}

// TestBackpressureWaitsForSyncer is the BUG 2 positive case: a writer that
// outpaces a live (slow) syncer under a tight local cap must backpressure and
// keep succeeding — never return ErrLocalStoreFull — because the syncer keeps
// draining unsynced bytes and refreshing the wait budget.
func TestBackpressureWaitsForSyncer(t *testing.T) {
	s, _ := evictStore(t, Config{MaxLocalBytes: 2 << 20, EvictMaxWait: 200 * time.Millisecond})
	ctx := context.Background()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // slow syncer: drains one sealed segment every few ms
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case <-time.After(3 * time.Millisecond):
				pretendCarveOneSealed(s)
			}
		}
	}()

	buf := bytes.Repeat([]byte{0x9E}, chunk256)
	var off int64
	for i := 0; i < 60; i++ { // ~15 MiB through a 2 MiB cap
		if err := s.WriteAt(ctx, "f", off, buf); err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("write %d must backpressure on a live syncer, never fail: %v", i, err)
		}
		off += chunk256
	}
	close(done)
	wg.Wait()
}

// TestBackpressureTerminalWhenNoSyncer is the BUG 2 negative case: with no syncer
// draining, the unsynced bytes pinning the cap never fall, so the write path must
// still fail with ErrLocalStoreFull within a bounded budget rather than hang.
func TestBackpressureTerminalWhenNoSyncer(t *testing.T) {
	s, _ := evictStore(t, Config{MaxLocalBytes: 2 << 20, EvictMaxWait: 50 * time.Millisecond})
	ctx := context.Background()

	buf := bytes.Repeat([]byte{0xCD}, chunk256)
	var off int64
	var gotFull bool
	start := time.Now()
	for i := 0; i < 64; i++ {
		err := s.WriteAt(ctx, "f", off, buf)
		if errors.Is(err, ErrLocalStoreFull) {
			gotFull = true
			break
		}
		if err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		off += chunk256
	}
	if !gotFull {
		t.Fatalf("expected ErrLocalStoreFull with no syncer to drain the cap")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("backpressure must be bounded, not hang: took %v", elapsed)
	}
}

func TestConcurrentWriteEvictRace(t *testing.T) {
	s, _ := evictStore(t, Config{ShardCount: 4})
	ctx := context.Background()

	buf := bytes.Repeat([]byte{0x7E}, chunk256)
	ids := []FileID{"a", "b", "c", "d", "e", "f"}
	var wg sync.WaitGroup

	for _, id := range ids {
		wg.Add(1)
		go func(id FileID) {
			defer wg.Done()
			var off int64
			for i := 0; i < 16; i++ {
				if err := s.Hydrate(ctx, id, off, buf); err != nil {
					t.Errorf("Hydrate: %v", err)
					return
				}
				off += chunk256
			}
		}(id)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if _, err := s.Evict(ctx, 1<<20); err != nil {
				t.Errorf("Evict: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}
