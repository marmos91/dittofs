//go:build race

package index

import (
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// keepEntry is one retained piece handed to Retain in the concurrent feed,
// with its file.
type keepEntry struct {
	file core.FileID
	p    Retained
}

// collectKeeps names the retained pieces the concurrent test injects: every
// seventh content record stands in for the pieces a later epic's hold rules
// would compute.
func collectKeeps(recs []Record) []keepEntry {
	var keeps []keepEntry
	for i, r := range recs {
		if r.Len <= 0 {
			continue
		}
		if i%7 == 3 {
			keeps = append(keeps, keepEntry{
				file: r.File,
				p: Retained{
					Piece: Piece{Extent: core.Extent{Off: r.Off, Len: 4}, Version: r.Numbers.Version},
					By:    r.Numbers.Version,
				},
			})
		}
	}
	return keeps
}

// TestRebuildConcurrentFeeders: sixteen goroutines feed one builder — one
// shuffled history split into sixteen disjoint slices, so every record
// arrives exactly once and no duplicate can mask a lost append —
// interleaved with Retain calls, and after every one has joined, Finish
// rebuilds a snapshot equal to the sequential feed of the same records, drop
// ranges included: the result depends only on the records, not on how they
// raced in. The race detector finds a feeder appending under another file's
// shard lock, or a replay touching two files' state.
func TestRebuildConcurrentFeeders(t *testing.T) {
	h := drive(t, 7, 200)
	keeps := collectKeeps(h.recs)

	// The sequential expectation: the same records, the same keeps, fed
	// and finished alone.
	want := func() snapshotResult {
		fs := &files{}
		b := newRebuilder(fs, &Counters{})
		for _, r := range h.recs {
			if err := b.Feed(r); err != nil {
				t.Fatal(err)
			}
		}
		for _, k := range keeps {
			if err := b.Retain(k.file, k.p); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.Finish(ver(0)); err != nil {
			t.Fatal(err)
		}
		return snapshotResult{snap: snapshot(fs, true)}
	}()

	const feeders = 16
	for round := range 3 {
		fs := &files{}
		b := newRebuilder(fs, &Counters{})
		var wg sync.WaitGroup
		rng := rand.New(rand.NewPCG(uint64(round), 3375))
		shuffled := slices.Clone(h.recs)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		// Disjoint slices: a record fed by one goroutine but lost would be
		// lost for the round, not rescued by another goroutine's copy.
		per := (len(shuffled) + feeders - 1) / feeders
		for i := range feeders {
			part := shuffled[min(i*per, len(shuffled)):min((i+1)*per, len(shuffled))]
			wg.Go(func() {
				for _, r := range part {
					if err := b.Feed(r); err != nil {
						t.Errorf("Feed: %v", err)
						return
					}
				}
			})
		}
		for _, k := range keeps {
			wg.Go(func() {
				if err := b.Retain(k.file, k.p); err != nil {
					t.Errorf("Retain: %v", err)
				}
			})
		}
		wg.Wait()
		if err := b.Finish(ver(0)); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		got := snapshot(fs, true)
		if d := diffSnap(got, want.snap); d != "" {
			t.Fatalf("round %d: %s", round, d)
		}
	}
}

// snapshotResult carries a snapshot out of a closure.
type snapshotResult struct {
	snap indexSnap
}
