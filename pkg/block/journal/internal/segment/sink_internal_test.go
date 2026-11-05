package segment

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// TestSinkCalledUnderAppendDomain pins the contract the sink's doc states:
// Track and a stream Sync's Synced both run with the segment append domain
// held, so the consumer may not take a lock of this package.
func TestSinkCalledUnderAppendDomain(t *testing.T) {
	var s *Stream
	var trackHeld, syncedHeld, tracked, synced bool
	held := func() bool {
		if s.appendMu.TryLock() {
			s.appendMu.Unlock()
			return false
		}
		return true
	}

	s = streamProbeStream(t)
	s.sink = Sink{
		Track:  func(Written) { tracked, trackHeld = true, held() },
		Synced: func(core.SegmentID, int64) { synced, syncedHeld = true, held() },
	}
	t.Cleanup(func() { _ = s.Close() })
	// The first append takes a spare the pool's worker makes on its own
	// goroutine: wait for it rather than race the make.
	for deadline := time.Now().Add(10 * time.Second); s.pool.Counters().Spares < 1; {
		if time.Now().After(deadline) {
			t.Fatal("the pool never readied a spare")
		}
		time.Sleep(time.Millisecond)
	}

	h := format.RecordHeader{Kind: format.KindTruncate, File: core.NewFileID([16]byte{1}), Extent: core.Extent{Off: 10}, Version: core.Version{Counter: 1}, Tag: 7}
	if _, err := s.Append(h, format.RecordBody{}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !tracked || !synced {
		t.Fatalf("the sink saw Track %v and Synced %v, want both", tracked, synced)
	}
	if !trackHeld {
		t.Error("Track ran without the segment append domain held")
	}
	if !syncedHeld {
		t.Error("Synced ran without the segment append domain held")
	}
}
