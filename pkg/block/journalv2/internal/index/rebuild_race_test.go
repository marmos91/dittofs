//go:build race

package index

import (
	"errors"
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

// TestRebuildConcurrentFeeders: sixteen goroutines feed one builder — each a
// shuffled slice of one history — interleaved with Retain calls, and after
// every one has joined, Finish rebuilds a snapshot equal to the sequential
// feed of the same records, drop ranges included: the result depends only
// on the records, not on how they raced in. The race detector finds a feeder
// appending under another file's shard lock, or a replay touching two
// files' state. A Feed after Finish returns errRebuilt and changes nothing.
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
			b.Retain(k.file, k.p)
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
		for range feeders {
			order := slices.Clone(h.recs)
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			wg.Go(func() {
				for _, r := range order {
					if err := b.Feed(r); err != nil {
						t.Errorf("Feed: %v", err)
						return
					}
				}
			})
		}
		for _, k := range keeps {
			wg.Go(func() { b.Retain(k.file, k.p) })
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

	// Feeding the finished builder is an error that changes nothing.
	fs := &files{}
	b := newRebuilder(fs, &Counters{})
	if err := b.Feed(h.recs[0]); err != nil {
		t.Fatal(err)
	}
	if err := b.Finish(ver(0)); err != nil {
		t.Fatal(err)
	}
	if err := b.Feed(recd(RecWrite, fid(1), 2, 2, 1, 0, 0, 4)); !errors.Is(err, errRebuilt) {
		t.Fatalf("Feed after Finish = %v, want errRebuilt", err)
	}
	if n := fs.len(); n != 1 {
		t.Fatalf("a refused feed left %d entries", n)
	}
}

// snapshotResult carries a snapshot out of a closure.
type snapshotResult struct {
	snap indexSnap
}
