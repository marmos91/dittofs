package journal

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
)

// The transition table is the specification: every residency transition the
// store can produce, asserted over the state model. The operations and their
// legal transitions:
//
//	WriteAt     * → Dirty
//	Commit      fsync covers Dirty's durability (Extent.Durable rises, State stays)
//	Carve       Dirty → Resident (residency rises; local bytes stay)
//	Hydrate     {Absent, Remote} → Resident
//	Invalidate  Resident → Remote (a Dirty refusal is the operation's definition)
//	Seed        Absent → Remote
//	Evict       Resident → Remote
//	Compact     never produces Lost

// extentStates reads the file's live extents through the state primitive.
func extentStates(t *testing.T, s *Store, id FileID) []Extent {
	t.Helper()
	ext, err := s.Extents(context.Background(), id)
	if err != nil {
		t.Fatalf("Extents: %v", err)
	}
	return ext
}

// evictStoreT is evictStore with an explicit segment size, which the compaction
// test needs (a default-sized segment would never roll).
func evictStoreT(t *testing.T, cfg Config) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// wantStates asserts the file's extents carry exactly the expected states,
// positionally.
func wantStates(t *testing.T, s *Store, id FileID, want ...State) {
	t.Helper()
	ext := extentStates(t, s, id)
	if len(ext) != len(want) {
		t.Fatalf("got %d extents %+v, want %d states %v", len(ext), ext, len(want), want)
	}
	for i, w := range want {
		if ext[i].State != w {
			t.Fatalf("extent %d: got %v, want %v (all: %+v)", i, ext[i].State, w, ext)
		}
	}
}

func TestTransition_WriteAtProducesDirty(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	if err := s.WriteAt(ctx, "f", 0, make([]byte, 4096)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	wantStates(t, s, "f", StateDirty)
}

func TestTransition_CommitMakesDirtyDurable(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	if err := s.WriteAt(ctx, "f", 0, make([]byte, 4096)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Commit(ctx, "f"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	ext := extentStates(t, s, "f")
	if len(ext) != 1 || ext[0].State != StateDirty {
		t.Fatalf("commit must not change residency, got %+v", ext)
	}
	if !ext[0].Durable {
		t.Fatal("committed dirty range must be Durable: an fsync covered it")
	}
}

func TestTransition_CarveMakesResident(t *testing.T) {
	s, _, _, _ := carveStore(t, Config{})
	ctx := context.Background()
	data := randBytes(128<<10, 1)
	if err := s.WriteAt(ctx, "f", 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	res, err := s.Carve(ctx, CarveOptions{Force: true})
	if err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if res.BlocksWritten == 0 {
		t.Fatal("carve packed nothing; the test would assert on a no-op")
	}
	ext := extentStates(t, s, "f")
	if len(ext) != 1 || ext[0].State != StateResident {
		t.Fatalf("carved range must be Resident, got %+v", ext)
	}
	if !ext[0].Durable {
		t.Fatal("resident range must be Durable")
	}
}

func TestTransition_InvalidateTakesResidentToRemote(t *testing.T) {
	s, _, _, _ := carveStore(t, Config{})
	ctx := context.Background()
	const chunk = 128 << 10
	if err := s.WriteAt(ctx, "f", 0, randBytes(chunk, 2)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if err := s.Invalidate(ctx, "f", 0, chunk); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	wantStates(t, s, "f", StateRemote)
}

func TestTransition_InvalidateRefusesDirty(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	const chunk = 128 << 10
	if err := s.WriteAt(ctx, "f", 0, randBytes(chunk, 3)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.Invalidate(ctx, "f", 0, chunk); err != nil {
		t.Fatalf("Invalidate on dirty: %v", err)
	}
	// Dirty → Remote is data loss: nothing was uploaded, so there is no copy to
	// fetch back. The refusal is the operation's definition — the range stays
	// dirty.
	wantStates(t, s, "f", StateDirty)
}

func TestTransition_HydrateTakesRemoteToResident(t *testing.T) {
	s, _, _, _ := carveStore(t, Config{})
	ctx := context.Background()
	const chunk = 128 << 10
	buf := randBytes(chunk, 4)
	if err := s.WriteAt(ctx, "f", 0, buf); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	if err := s.Invalidate(ctx, "f", 0, chunk); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	// Hydrate fills the cold range: Remote → Resident.
	if err := s.Hydrate(ctx, "f", 0, buf, 0); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	wantStates(t, s, "f", StateResident)
}

func TestTransition_HydrateFillsHoleToResident(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	const chunk = 128 << 10
	if err := s.Hydrate(ctx, "f", 0, randBytes(chunk, 5), 0); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	wantStates(t, s, "f", StateResident)
}

func TestTransition_SeedTakesAbsentToRemote(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	if err := s.SeedCold(ctx, "f", [][2]int64{{0, 1 << 20}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	wantStates(t, s, "f", StateRemote)
}

func TestTransition_SeedSkipsLiveLocalBytes(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	if err := s.WriteAt(ctx, "f", 0, make([]byte, 4096)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := s.SeedCold(ctx, "f", [][2]int64{{0, 4096}}); err != nil {
		t.Fatalf("SeedCold: %v", err)
	}
	// Seeding over live local bytes would shadow them — including bytes not on
	// the remote. Absent stays absent, live stays live.
	wantStates(t, s, "f", StateDirty)
}

func TestTransition_EvictTakesResidentToRemote(t *testing.T) {
	s, _ := evictStore(t, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	ctx := context.Background()
	const chunk = 128 << 10
	buf := randBytes(chunk, 6)
	// Hydrate appends records already marked synced, so the segment qualifies
	// for eviction without a carve.
	if err := s.Hydrate(ctx, "f", 0, buf, 0); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	wantStates(t, s, "f", StateResident)
	// fillUntilSealed's pattern: the segment must be sealed before it is
	// evictable, so write past the roll threshold once.
	if err := s.Hydrate(ctx, "g", 0, buf, 0); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if _, err := s.Evict(ctx, 0); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	wantStates(t, s, "f", StateRemote)
}

func TestTransition_CompactNeverProducesLost(t *testing.T) {
	s := evictStoreT(t, Config{SegmentSize: minSegmentSize, ShardCount: 1})
	ctx := context.Background()
	seedRepackable(t, s, true)
	res, err := s.gc(ctx, gcOptions{Force: true})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.SegmentsRepacked == 0 {
		t.Fatal("nothing repacked; the test would assert on a no-op")
	}
	for _, ext := range extentStates(t, s, "keep") {
		if ext.State == StateLost {
			t.Fatalf("compaction produced StateLost: %+v", ext)
		}
	}
}

func TestTransition_FailedLossKeepsResident(t *testing.T) {
	s, _, _, _ := carveStore(t, Config{})
	ctx := context.Background()
	const chunk = 128 << 10
	if err := s.WriteAt(ctx, "f", 0, randBytes(chunk, 7)); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
		t.Fatalf("Carve: %v", err)
	}
	// Break the cold log so the demote's append fails: residency loss that
	// cannot be made durable must be refused, never taken blind.
	s.coldMu.Lock()
	s.coldFD = nil
	s.dir = t.TempDir() + "/missing"
	s.coldMu.Unlock()
	err := s.Invalidate(ctx, "f", 0, chunk)
	if err == nil {
		t.Fatal("demotion without a durable marker must fail")
	}
	if !errors.Is(err, ErrStateLost) {
		t.Fatalf("want ErrStateLost, got %v", err)
	}
	// The refusal leaves the bytes resident: the local copy is the only one.
	wantStates(t, s, "f", StateResident)
}

func TestTransition_ConcurrentWritersHoldTheModel(t *testing.T) {
	s := testStore(t, Config{})
	ctx := context.Background()
	const chunk = 4096
	const writers = 8
	const rounds = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			buf := make([]byte, chunk)
			<-start
			for r := 0; r < rounds; r++ {
				if err := s.WriteAt(ctx, "f", int64((w*rounds+r)%writers)*chunk, buf); err != nil {
					t.Errorf("WriteAt: %v", err)
					return
				}
				if err := s.Commit(ctx, "f"); err != nil {
					t.Errorf("Commit: %v", err)
					return
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	for _, ext := range extentStates(t, s, "f") {
		switch ext.State {
		case StateDirty, StateResident:
		default:
			t.Fatalf("concurrent writers produced %v on an append-only store: %+v", ext.State, ext)
		}
	}
}

// TestModel_RandomOperationsAgainstNaiveOracle runs random operation sequences
// against the store and a naive oracle — a plain map of offset → state. The
// oracle mirrors the transition table directly and asserts what the store must
// never do: report Lost on anything, or demote a range that was never durable.
type naiveOracle struct {
	// byOff is one entry per written range: the state the table says it holds.
	byOff map[int64]State
}

func newNaiveOracle() *naiveOracle { return &naiveOracle{byOff: make(map[int64]State)} }

func (o *naiveOracle) apply(op string, off int64) {
	switch op {
	case "write":
		o.byOff[off] = StateDirty
	case "carve":
		// Flush packs dirty ranges; a remote range has no local bytes to carve.
		if st, ok := o.byOff[off]; ok && st == StateDirty {
			o.byOff[off] = StateResident
		}
	case "hydrate":
		// A fill replaces Absent/Remote only — live local bytes are the newer copy.
		if st, ok := o.byOff[off]; ok && st == StateRemote {
			o.byOff[off] = StateResident
		}
	case "invalidate", "evict":
		if st, ok := o.byOff[off]; ok && st == StateResident {
			o.byOff[off] = StateRemote
		}
	}
	// seed: only touches Absent ranges, which the oracle does not track.
}

func TestModel_RandomOperationsAgainstNaiveOracle(t *testing.T) {
	s, _, _, _ := carveStore(t, Config{})
	ctx := context.Background()
	const (
		chunk = 64 << 10
		spans = 8
	)
	oracle := newNaiveOracle()
	rng := rand.New(rand.NewSource(42))
	buf := make([]byte, chunk)
	ops := 0
	for i := 0; i < 300; i++ {
		span := rng.Intn(spans)
		off := int64(span) * chunk
		switch rng.Intn(5) {
		case 0: // write
			if err := s.WriteAt(ctx, "f", off, buf); err != nil {
				t.Fatalf("WriteAt: %v", err)
			}
			oracle.apply("write", off)
			ops++
		case 1: // carve
			if _, err := s.Carve(ctx, CarveOptions{Force: true}); err != nil {
				t.Fatalf("Carve: %v", err)
			}
			for k := 0; k < spans; k++ {
				oracle.apply("carve", int64(k)*chunk)
			}
			ops++
		case 2: // hydrate (a fill: only Absent/Remote ranges take it)
			if err := s.Hydrate(ctx, "f", off, buf, 0); err != nil {
				t.Fatalf("Hydrate: %v", err)
			}
			oracle.apply("hydrate", off)
			ops++
		case 3: // invalidate
			if err := s.Invalidate(ctx, "f", off, chunk); err != nil {
				t.Fatalf("Invalidate: %v", err)
			}
			oracle.apply("invalidate", off)
			ops++
		case 4: // compact
			if _, err := s.gc(ctx, gcOptions{Force: true}); err != nil {
				t.Fatalf("GC: %v", err)
			}
			ops++
		}

		// The invariant after every step: no extent the store reports is Lost,
		// and every extent the oracle calls Resident the store reports Resident
		// (a state the oracle holds local-but-not-remote must never be reported
		// Remote — that is demotion without a durable marker, the silent-zeros
		// shape).
		ext := extentStates(t, s, "f")
		for _, e := range ext {
			if e.State == StateLost {
				t.Fatalf("step %d: store reported StateLost: %+v", ops, e)
			}
		}
		for k := 0; k < spans; k++ {
			oOff := int64(k) * chunk
			want := oracle.byOff[oOff]
			if want != StateResident {
				continue
			}
			found := false
			for _, e := range ext {
				if e.Off == oOff {
					found = true
					if e.State != StateResident {
						t.Fatalf("step %d: oracle Resident, store %v at +%d (all: %+v)", ops, e.State, oOff, ext)
					}
				}
			}
			if !found {
				t.Fatalf("step %d: oracle Resident, store reports no extent at +%d (all: %+v)", ops, oOff, ext)
			}
		}
	}
	if ops == 0 {
		t.Fatal("no operations ran")
	}
}
