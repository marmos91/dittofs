package index

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// recd builds a Record for feeding a builder, with a nonzero placement
// unless the caller passes one.
func recd(kind RecordKind, file core.FileID, seq, version uint64, seg core.SegmentID, at uint32, off, n int64) Record {
	return Record{
		Kind:    kind,
		File:    file,
		Tag:     7,
		Segment: seg,
		At:      at,
		Off:     off,
		Len:     n,
		Numbers: nums(version, seq),
	}
}

// equalErr reports whether got fails the test for the sentinel want, printing
// the record the refusal names.
func equalErr(t *testing.T, step string, got error, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("%s = %v, want %v", step, got, want)
	}
}

// TestRebuildGolden is RFC 1 §4.5's walkthrough as the golden case: every
// record of alice.vhdx, with bob's interleaved, fed in every permutation of
// alice's records (5040), must rebuild to the same three held extents with
// the same offloaded bits, no marker, change version 9 — and leave bob his
// own entry. Replaying records in feed order rather than by precedence
// loses, among others, the fill's win over the release, which equal bits
// around hold back.
func TestRebuildGolden(t *testing.T) {
	const KiB = 1024
	alice, bob := fid(1), fid(2)
	records := []Record{
		recd(RecWrite, alice, 101, 7, 12, 0, 0, 8*KiB),
		recd(RecWrite, bob, 102, 6, 12, 8*KiB, 0, 4*KiB),
		recd(RecWrite, alice, 103, 8, 12, 16*KiB, 4*KiB, 8*KiB),
		recd(RecOffloaded, alice, 104, 7, 12, 32*KiB, 0, 8*KiB),
		recd(RecRelease, alice, 105, 7, 13, 0, 0, 4*KiB),
		recd(RecWrite, alice, 110, 9, 13, 8*KiB, 20*KiB, 4*KiB),
		recd(RecFill, alice, 120, 7, 14, 0, 0, 4*KiB),
	}
	aliceRecords := func() []Record {
		var rs []Record
		for _, r := range records {
			if r.File == alice {
				rs = append(rs, r)
			}
		}
		return rs
	}()
	bobRecords := []Record{records[1]}

	expect := func(t *testing.T, fs *files, ctr *Counters, step string) {
		t.Helper()
		want := []Piece{
			{Extent: ext(0, 4*KiB), Record: rec(14, 0), Version: ver(7), Offloaded: true},
			{Extent: ext(4*KiB, 8*KiB), Record: rec(12, 16*KiB), Version: ver(8)},
			{Extent: ext(20*KiB, 4*KiB), Record: rec(13, 8*KiB), Version: ver(9)},
		}
		if got := heldPieces(fs, alice); !slices.Equal(got, want) {
			t.Fatalf("%s: alice held:\n got  %+v\n want %+v", step, got, want)
		}
		if got := heldPieces(fs, bob); !slices.Equal(got, []Piece{
			{Extent: ext(0, 4*KiB), Record: rec(12, 8*KiB), Version: ver(6)},
		}) {
			t.Fatalf("%s: bob held %+v: another file's records moved alice's", step, got)
		}
		if n := markerCount(fs, alice); n != 0 {
			t.Fatalf("%s: alice keeps %d markers, want none", step, n)
		}
		if got := fs.asOf(alice); got != ver(9) {
			t.Fatalf("%s: alice's change version %v, want 9", step, got)
		}
		if next := ctr.NextVersion(); next.Compare(ver(9)) <= 0 {
			t.Fatalf("%s: NextVersion() = %v, want above every fed version", step, next)
		}
		if seq, err := ctr.NextSeq(); err != nil || seq <= 120 {
			t.Fatalf("%s: NextSeq() = %d, %v, want above 120", step, seq, err)
		}
		// Alice's three extents and bob's one: the rebuilt count is exact
		// without a walk.
		if n := fs.extentCount(); n != 4 {
			t.Fatalf("%s: extentCount() = %d, want 4", step, n)
		}
	}

	// Every permutation of alice's records (5040), with bob's fed first,
	// rebuilds the same index.
	var perm []Record
	var apply func(n int)
	apply = func(n int) {
		if n == len(aliceRecords) {
			var fs files
			fs.setExtentLimit(math.MaxInt64)
			var ctr Counters
			b := newRebuilder(&fs, &ctr)
			for _, r := range perm {
				if err := b.Feed(r); err != nil {
					t.Fatalf("Feed(%v) = %v", r.Numbers.Seq, err)
				}
			}
			if err := b.Finish(ver(0)); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			expect(t, &fs, &ctr, fmt.Sprintf("permutation %v", slices.Clone(perm)))
			return
		}
		for _, r := range aliceRecords {
			if slices.Contains(perm, r) {
				continue
			}
			perm = append(perm, r)
			apply(n + 1)
			perm = perm[:len(perm)-1]
		}
	}
	perm = slices.Clone(bobRecords)
	apply(0)
}

// TestRebuildPrecedenceTiesAndCopies is the precedence rule on ties: equal
// versions are ordered by sequence number, a repack copy succeeds its
// original wherever it is fed, a newer version wins however low its sequence
// number, and the same record carried in two places is one record, read from
// its newer home.
func TestRebuildPrecedenceTiesAndCopies(t *testing.T) {
	t.Run("repack copy wins its record's home", func(t *testing.T) {
		// W(v7, s5) in segment 1 and its repack copy W(v7, s9) in segment 2:
		// the copy has the higher seq at the same version, so the piece is
		// read from it, whatever the feed order.
		for _, order := range [][]int{{0, 1}, {1, 0}} {
			var fs files
			b := newRebuilder(&fs, &Counters{})
			rs := []Record{
				recd(RecWrite, fid(1), 5, 7, 1, 0, 0, 8),
				recd(RecWrite, fid(1), 9, 7, 2, 4096, 0, 8),
			}
			for _, i := range order {
				if err := b.Feed(rs[i]); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Finish(ver(0)); err != nil {
				t.Fatal(err)
			}
			if got := heldPieces(&fs, fid(1)); !slices.Equal(got, []Piece{
				{Extent: ext(0, 8), Record: rec(2, 4096), Version: ver(7)},
			}) {
				t.Fatalf("order %v: held %+v, want the copy's record", order, got)
			}
		}
	})

	t.Run("the greater version wins with the lower seq", func(t *testing.T) {
		// A write at (v8, s3) against a write at (v7, s9) over the same
		// bytes: the higher version wins, its lower sequence number
		// notwithstanding. Replaying by arrival, the v7 write fed second
		// would land over it.
		for _, order := range [][]int{{0, 1}, {1, 0}} {
			var fs files
			b := newRebuilder(&fs, &Counters{})
			rs := []Record{
				recd(RecWrite, fid(1), 3, 8, 1, 0, 0, 8),
				recd(RecWrite, fid(1), 9, 7, 2, 0, 0, 8),
			}
			for _, i := range order {
				if err := b.Feed(rs[i]); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Finish(ver(0)); err != nil {
				t.Fatal(err)
			}
			if got := heldPieces(&fs, fid(1)); !slices.Equal(got, []Piece{
				{Extent: ext(0, 8), Record: rec(1, 0), Version: ver(8)},
			}) {
				t.Fatalf("order %v: held %+v, want the v8 bytes", order, got)
			}
		}
	})

	t.Run("one record carried in two segments", func(t *testing.T) {
		// The same removal re-appended after a failed sync, its original
		// still on disk: one marker, not two, both copies noted for the
		// drop ranges. The same write carried in two placements is one
		// piece, read from the greater (segment, offset).
		var fs files
		b := newRebuilder(&fs, &Counters{})
		id := fid(1)
		for _, r := range []Record{
			recd(RecDeallocate, id, 5, 9, 1, 0, 4, 8),
			recd(RecDeallocate, id, 5, 9, 3, 96, 4, 8),
			recd(RecWrite, id, 8, 7, 1, 0, 0, 4),
			recd(RecWrite, id, 8, 7, 3, 200, 0, 4),
		} {
			if err := b.Feed(r); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.Finish(ver(0)); err != nil {
			t.Fatal(err)
		}
		if n := markerCount(&fs, id); n != 1 {
			t.Fatalf("markers = %d, want one for the carried removal", n)
		}
		if got := fs.since(id, core.Version{}, nil); len(got) != 2 {
			t.Fatalf("since = %+v, want the marker and the one held piece", got)
		}
		var segs []core.SegmentID
		fs.view(id, func(f *file) {
			for _, s := range f.segs {
				segs = append(segs, s.seg)
			}
		})
		if !slices.Contains(segs, core.SegmentID(1)) || !slices.Contains(segs, core.SegmentID(3)) {
			t.Fatalf("segments noted = %v, want both copies' segments 1 and 3", segs)
		}
		// The surviving placement is the run's greater (segment, offset),
		// so a copied record reads from its newer home — the failed
		// segment's copy must not be the one the piece names.
		if got := heldPieces(&fs, id); !slices.Equal(got, []Piece{
			{Extent: ext(0, 4), Record: rec(3, 200), Version: ver(7)},
		}) {
			t.Fatalf("held = %+v, want the piece read from the copy at (3, 200)", got)
		}
	})
}

// TestRebuildRemovals: a rebuilt marker starts unsettled, each removal drops
// the content below its version whatever the content's offloaded bit — an
// offloaded piece too — and leaves content at or above it; a clone target's
// marker keeps its own copy of the spec; truncate to 0, deallocate and delete
// behave at a rebuild exactly as at run time.
func TestRebuildRemovals(t *testing.T) {
	// A truncate below a newer write: the write, fed first or last, survives
	// by precedence, and the marker stays unsettled until settled above it.
	t.Run("removal below newer content, and unsettled markers", func(t *testing.T) {
		for _, order := range [][]int{{0, 1}, {1, 0}} {
			var fs files
			b := newRebuilder(&fs, &Counters{})
			id := fid(1)
			rs := []Record{
				recd(RecTruncate, id, 5, 8, 1, 0, 4, 0),
				recd(RecWrite, id, 9, 9, 2, 0, 0, 16),
			}
			for _, i := range order {
				if err := b.Feed(rs[i]); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Finish(ver(0)); err != nil {
				t.Fatal(err)
			}
			// The write straddles the truncate's size; only the part past it
			// would be held by a removal that dropped content whatever its
			// version.
			if got := heldPieces(&fs, id); !slices.Equal(got, []Piece{
				{Extent: ext(0, 16), Record: rec(2, 0), Version: ver(9)},
			}) {
				t.Fatalf("order %v: held %+v, want the v9 write whole", order, got)
			}
			// Every rebuilt marker is unsettled: Since yields it, and
			// settling at or above its version drops it.
			if changes := fs.since(id, core.Version{}, nil); len(changes) != 2 {
				t.Fatalf("since = %+v, want the truncate marker beside the write", changes)
			}
			fs.settleRemovals(id, ver(8))
			if n := markerCount(&fs, id); n != 0 {
				t.Fatalf("markers after settling = %d, want none", n)
			}
		}
	})

	// A removal drops content below its version whatever its bit: the
	// offloaded-bit check belongs to the call that appends a release, not
	// to the record at a rebuild.
	t.Run("removal drops offloaded content too", func(t *testing.T) {
		var fs files
		b := newRebuilder(&fs, &Counters{})
		id := fid(1)
		for _, r := range []Record{
			recd(RecWrite, id, 1, 5, 1, 0, 0, 8),
			recd(RecOffloaded, id, 2, 5, 1, 64, 0, 8),
			recd(RecDeallocate, id, 3, 9, 2, 0, 0, 8),
		} {
			if err := b.Feed(r); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.Finish(ver(0)); err != nil {
			t.Fatal(err)
		}
		if got := heldPieces(&fs, id); len(got) != 0 {
			t.Fatalf("held = %+v, want nothing: the mark gated nothing at a rebuild", got)
		}
		if n := markerCount(&fs, id); n != 1 {
			t.Fatalf("markers = %d, want the deallocate's", n)
		}
	})

	// A clone target's marker keeps an independent copy of its spec, and
	// truncate to 0 and delete leave their to-end markers.
	t.Run("clone spec and to-end kinds", func(t *testing.T) {
		spec := core.CloneSpec{SrcShare: core.ShareID{3}, Src: fid(9), SrcOff: 1 << 20, DstOff: 4, Len: 8, AsOf: ver(11)}
		cases := []struct {
			name   string
			kind   RecordKind
			off, n int64
			clone  *core.CloneSpec
			want   marker
		}{
			{"clone target", RecCloneTarget, 4, 8, &spec, marker{extent: ext(4, 8), version: ver(12), kind: core.ChangeCloneTarget, clone: &spec}},
			{"truncate to 0", RecTruncate, 0, 0, nil, marker{extent: toEnd(0), version: ver(12), kind: core.ChangeTruncate}},
			{"delete", RecDelete, 0, 0, nil, marker{extent: toEnd(0), version: ver(12), kind: core.ChangeDelete}},
		}
		for _, c := range cases {
			var fs files
			id := fid(1)
			r := recd(c.kind, id, 12, 12, 1, 0, c.off, c.n)
			r.Clone = c.clone
			b := newRebuilder(&fs, &Counters{})
			if err := b.Feed(r); err != nil {
				t.Fatal(err)
			}
			if err := b.Finish(ver(0)); err != nil {
				t.Fatal(err)
			}
			var got marker
			fs.view(id, func(f *file) {
				if len(f.markers) != 1 {
					t.Fatalf("%s: markers = %+v, want one", c.name, f.markers)
				}
				got = f.markers[0]
			})
			if got.kind != c.want.kind || got.extent != c.want.extent || got.version != c.want.version {
				t.Fatalf("%s: marker = %+v, want %+v", c.name, got, c.want)
			}
			if (got.clone == nil) != (c.want.clone == nil) {
				t.Fatalf("%s: marker clone = %+v", c.name, got.clone)
			}
			if got.clone != nil && *got.clone != spec {
				t.Fatalf("%s: marker keeps spec %+v, want %+v", c.name, *got.clone, spec)
			}
		}
	})
}

// TestRebuildOffloadedBits: the offloaded-bit pass decides a piece's bit from
// the marks covering it — an offloaded record at version v sets bits only on
// pieces at or below v, an unmark with a higher sequence number clears what
// an offloaded record set, one with a lower does not, a fill keeps its bit
// against an unmark fed before it and loses it against one after, an unmark
// below a piece's version leaves its bit, and a repack copy carries its
// original's bit. Each case in several feed orders.
func TestRebuildOffloadedBits(t *testing.T) {
	// Build a two-piece file — v5 offloaded by a mark, v8 dirty — and apply
	// each mark scene over it, in both feed orders.
	scene := func(t *testing.T, marks []Record, want []Piece, orders ...[]int) {
		t.Helper()
		for _, order := range orders {
			var fs files
			b := newRebuilder(&fs, &Counters{})
			rs := append([]Record{
				recd(RecWrite, fid(1), 1, 5, 1, 0, 0, 8),
				recd(RecOffloaded, fid(1), 2, 5, 1, 64, 0, 8),
				recd(RecWrite, fid(1), 3, 8, 1, 128, 8, 8),
			}, marks...)
			for _, i := range order {
				if err := b.Feed(rs[i]); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Finish(ver(0)); err != nil {
				t.Fatal(err)
			}
			if got := heldPieces(&fs, fid(1)); !slices.Equal(got, want) {
				t.Fatalf("order %v: held\n got  %+v\n want %+v", order, got, want)
			}
		}
	}
	// off takes Record indices, so a scene orders [0 1 2] plus mark records.
	dirtyV8 := Piece{Extent: ext(8, 8), Record: rec(1, 128), Version: ver(8)}
	bit := func(p Piece) Piece { p.Offloaded = true; return p }

	// An offloaded record above the v5 mark's version marks the v8 piece;
	// one at v5 leaves it dirty.
	scene(t,
		[]Record{recd(RecOffloaded, fid(1), 9, 8, 2, 0, 0, 16)},
		[]Piece{bit(Piece{Extent: ext(0, 8), Record: rec(1, 0), Version: ver(5)}), bit(dirtyV8)},
		[]int{0, 1, 2, 3}, []int{3, 2, 1, 0},
	)
	scene(t,
		[]Record{recd(RecOffloaded, fid(1), 9, 5, 2, 0, 8, 8)},
		[]Piece{bit(Piece{Extent: ext(0, 8), Record: rec(1, 0), Version: ver(5)}), dirtyV8},
		[]int{0, 1, 2, 3}, []int{3, 2, 1, 0},
	)

	// An unmark with a higher seq than the offloaded record clears the bit;
	// one with a lower seq does not.
	scene(t,
		[]Record{recd(RecUnmark, fid(1), 9, 5, 2, 0, 0, 8)},
		[]Piece{{Extent: ext(0, 8), Record: rec(1, 0), Version: ver(5)}, dirtyV8},
		[]int{0, 1, 2, 3}, []int{3, 2, 1, 0},
	)
	scene(t,
		[]Record{recd(RecUnmark, fid(1), 1, 5, 0, 0, 0, 8)},
		[]Piece{bit(Piece{Extent: ext(0, 8), Record: rec(1, 0), Version: ver(5)}), dirtyV8},
		[]int{0, 1, 2, 3}, []int{3, 2, 1, 0},
	)

	// An unmark below a piece's version leaves its bit: the mark names
	// content newer than what it unmarks.
	scene(t,
		[]Record{recd(RecUnmark, fid(1), 4, 4, 1, 0, 0, 16)},
		[]Piece{bit(Piece{Extent: ext(0, 8), Record: rec(1, 0), Version: ver(5)}), dirtyV8},
		[]int{0, 1, 2, 3}, []int{3, 2, 1, 0},
	)

	// A fill keeps its bit against an unmark with a lower sequence number
	// and loses it to one with a higher, exactly as a real offloaded record
	// would: the fill's own record is its mark.
	t.Run("fill against an unmark", func(t *testing.T) {
		for _, scene2 := range []struct {
			unmark Record
			want   bool
		}{
			{recd(RecUnmark, fid(1), 3, 7, 1, 0, 0, 4), true},  // before the fill
			{recd(RecUnmark, fid(1), 9, 7, 1, 0, 0, 4), false}, // after it
		} {
			for _, order := range [][]int{{0, 1}, {1, 0}} {
				var fs files
				b := newRebuilder(&fs, &Counters{})
				id := fid(1)
				rs := []Record{
					scene2.unmark,
					recd(RecFill, id, 5, 7, 2, 0, 0, 4),
				}
				for _, i := range order {
					if err := b.Feed(rs[i]); err != nil {
						t.Fatal(err)
					}
				}
				if err := b.Finish(ver(0)); err != nil {
					t.Fatal(err)
				}
				want := []Piece{{Extent: ext(0, 4), Record: rec(2, 0), Version: ver(7), Offloaded: scene2.want}}
				if got := heldPieces(&fs, id); !slices.Equal(got, want) {
					t.Fatalf("order %v: held = %+v, want the fill offloaded=%v", order, got, scene2.want)
				}
			}
		}
	})

	// A repack copy keeps its original's bit: the copy is the same content
	// in a new place, and no mark of the original's seq makes it here, so
	// the original's mark still decides its bit.
	scene(t,
		[]Record{
			recd(RecOffloaded, fid(1), 2, 5, 1, 64, 0, 8),
			recd(RecWrite, fid(1), 9, 5, 2, 4096, 0, 8), // the repack copy
		},
		[]Piece{bit(Piece{Extent: ext(0, 8), Record: rec(2, 4096), Version: ver(5)}), dirtyV8},
		[]int{0, 1, 2, 3, 4}, []int{4, 3, 2, 1, 0},
	)
}

// TestRebuildRetainInjection: a retained piece fed through Retain appears in
// the rebuilt file in offset order, keeps the bit the caller gave it — a
// mark of held content, offloaded record or fill alike, never sets or clears
// it, because the mark is about the bytes the record offered, not the older
// content it superseded — an empty piece is ignored, and the per-segment
// retained bytes count it.
func TestRebuildRetainInjection(t *testing.T) {
	var fs files
	id := fid(1)
	b := newRebuilder(&fs, &Counters{})
	retained := Retained{Piece: Piece{Extent: ext(4, 4), Record: rec(2, 0), Skip: 4, Version: ver(3), Offloaded: true}, By: ver(7)}
	for _, r := range []Record{
		recd(RecWrite, id, 7, 7, 2, 8, 0, 16),
		recd(RecOffloaded, id, 8, 6, 3, 0, 0, 8),
		recd(RecUnmark, id, 9, 9, 3, 64, 0, 8),
	} {
		if err := b.Feed(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Retain(id, retained); err != nil {
		t.Fatal(err)
	}
	if err := b.Retain(id, Retained{Piece: Piece{Extent: ext(100, 0)}, By: ver(1)}); err != nil { // ignored: empty
		t.Fatal(err)
	}
	// A retained piece for a file no record was fed for lands all the
	// same: the recovery code can hold pieces of a file whose records
	// went through another builder pass, and dropping them would lose the
	// superseded versions an offload still has to offer.
	kept := fid(4)
	if err := b.Retain(kept, Retained{
		Piece: Piece{Extent: ext(0, 8), Record: rec(9, 0), Version: ver(2)},
		By:    ver(3),
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Finish(ver(0)); err != nil {
		t.Fatal(err)
	}
	var got []Retained
	fs.view(id, func(f *file) { got = f.retained.list(nil) })
	// The retained piece keeps the bit its caller gave it: the offloaded
	// record at v6 and the unmark at v9 name the v6 content, not the v3
	// piece under it, and neither may flip the piece's bit. The held v7
	// piece is above the mark's version and stays dirty either way.
	want := []Retained{{Piece: Piece{Extent: ext(4, 4), Record: rec(2, 0), Skip: 4, Version: ver(3), Offloaded: true}, By: ver(7)}}
	if !slices.Equal(got, want) {
		t.Fatalf("retained = %+v, want %+v", got, want)
	}
	if p := heldPieces(&fs, id); len(p) != 1 || p[0].Offloaded {
		t.Fatalf("held = %+v, want the v7 write dirty", p)
	}
	if sb := fs.segmentBytes(2); sb.Retained != 4 {
		t.Fatalf("segment 2 retains %d bytes, want 4", sb.Retained)
	}
	checkSegments(t, 0, &fs, 2, 3)

	var lone []Retained
	fs.view(kept, func(f *file) { lone = f.retained.list(nil) })
	if want := []Retained{{Piece: Piece{Extent: ext(0, 8), Record: rec(9, 0), Version: ver(2)}, By: ver(3)}}; !slices.Equal(lone, want) {
		t.Fatalf("retained for the recordless file = %+v, want %+v", lone, want)
	}

	// A Retain after the Finish is refused and changes nothing.
	if err := b.Retain(fid(1), Retained{Piece: Piece{Extent: ext(0, 8), Version: ver(1)}, By: ver(1)}); !errors.Is(err, errRebuilt) {
		t.Fatalf("Retain after Finish = %v, want errRebuilt", err)
	}
	if n := fs.len(); n != 2 {
		t.Fatalf("a refused Retain left %d entries, want 2", n)
	}
}

// TestRebuildMarksNeverTouchRetainedBits: a fill's synthetic mark and an
// offloaded record's mark set no retained piece's bit, however far below the
// mark's version the piece sits, and an unmark clears none. A retained piece
// overlaps newer content by construction — the mark names the offered
// bytes, which are held or filled, never the superseded ones under them.
func TestRebuildMarksNeverTouchRetainedBits(t *testing.T) {
	for _, tc := range []struct {
		name string
		mark Record
	}{
		{"an offloaded record", recd(RecOffloaded, fid(1), 8, 7, 2, 0, 0, 8)},
		{"a fill", recd(RecFill, fid(1), 8, 8, 2, 0, 0, 8)},
		{"an unmark", recd(RecUnmark, fid(1), 8, 7, 2, 0, 0, 8)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, bit := range []bool{false, true} {
				var fs files
				id := fid(1)
				b := newRebuilder(&fs, &Counters{})
				for _, r := range []Record{
					recd(RecWrite, id, 1, 3, 2, 0, 0, 8),  // the v3 write superseded
					recd(RecWrite, id, 7, 7, 3, 64, 0, 8), // the v7 write covering it
				} {
					if err := b.Feed(r); err != nil {
						t.Fatal(err)
					}
				}
				if err := b.Feed(tc.mark); err != nil {
					t.Fatal(err)
				}
				if err := b.Retain(id, Retained{
					Piece: Piece{Extent: ext(0, 8), Record: rec(2, 0), Version: ver(3), Offloaded: bit},
					By:    ver(7),
				}); err != nil {
					t.Fatal(err)
				}
				if err := b.Finish(ver(0)); err != nil {
					t.Fatal(err)
				}
				var got []Retained
				fs.view(id, func(f *file) { got = f.retained.list(nil) })
				want := []Retained{{
					Piece: Piece{Extent: ext(0, 8), Record: rec(2, 0), Version: ver(3), Offloaded: bit},
					By:    ver(7),
				}}
				if !slices.Equal(got, want) {
					t.Fatalf("bit %v: retained = %+v, want the piece's bit untouched %+v", bit, got, want)
				}
			}
		})
	}
}

// TestRebuildCountersAndBuckets: after a rebuild the counters issue above
// every number on disk and the floor; the buckets start at the higher of the
// two; a file whose records leave nothing held and no marker keeps its tag
// and its asOf equals the bucket start, so a fill resolved below it is
// refused as stale; a failed Finish on a non-zero epoch leaves everything
// unchanged.
func TestRebuildCountersAndBuckets(t *testing.T) {
	mk := func() (*files, *Counters, *rebuilder, core.FileID) {
		fs := &files{}
		ctr := &Counters{}
		_ = ctr.SetVersionLimit(ver(math.MaxUint64 - 1))
		b := newRebuilder(fs, ctr)
		id := fid(1)
		for _, r := range []Record{
			recd(RecWrite, id, 3, 5, 1, 0, 0, 4),
			recd(RecOffloaded, id, 4, 5, 1, 64, 0, 4),
			recd(RecRelease, id, 5, 5, 1, 128, 0, 4),
		} {
			if err := b.Feed(r); err != nil {
				t.Fatal(r)
			}
		}
		return fs, ctr, b, id
	}

	t.Run("counters above the disk and floor", func(t *testing.T) {
		fs, ctr, b, id := mk()
		if err := b.Finish(ver(4)); err != nil {
			t.Fatal(err)
		}
		if next := ctr.NextVersion(); next.Compare(ver(5)) <= 0 {
			t.Fatalf("NextVersion() = %v, want above every fed version", next)
		}
		if seq, err := ctr.NextSeq(); err != nil || seq <= 5 {
			t.Fatalf("NextSeq() = %d, %v, want above every fed seq", seq, err)
		}
		// The file holds nothing and keeps no marker: its change version is
		// gone, its tag stays, and its asOf is the bucket start — the higher
		// of the highest fed version and the floor, 5 here.
		if got := fs.asOf(id); got != ver(5) {
			t.Fatalf("asOf = %v, want 5, the bucket start", got)
		}
		// A file with no entry of its own reads the same start off its
		// bucket: the highest fed version reached every bucket, not just
		// the one the settle chanced to raise. A floor below the disk that
		// the buckets ignored would serve a stale fill as fresh.
		if got := fs.asOf(fid(5)); got != ver(5) {
			t.Fatalf("asOf of a file with no entry = %v, want 5, the fed maximum", got)
		}
		if err := fs.fill(id, write(0, 4, rec(2, 0), 0), ver(4)); !errors.Is(err, core.ErrStaleFill) {
			t.Fatalf("fill below the bucket start = %v, want ErrStaleFill", err)
		}
		fs.view(id, func(f *file) {
			if tag, ok := f.owner(); !ok || tag != 7 {
				t.Fatalf("owner = %d, %v, want the fed records' tag", tag, ok)
			}
		})
	})

	t.Run("a floor above every version lifts every bucket", func(t *testing.T) {
		fs, _, b, id := mk()
		if err := b.Finish(ver(90)); err != nil {
			t.Fatal(err)
		}
		if got := fs.asOf(id); got != ver(90) {
			t.Fatalf("asOf = %v, want the floor's counter 90", got)
		}
		// Any file, one with no entry included, is checked against its
		// bucket, which the floor start stored whole.
		if got := fs.asOf(fid(5)); got != ver(90) {
			t.Fatalf("asOf of a file with no entry = %v, want 90", got)
		}
	})

	t.Run("a failed Finish changes nothing", func(t *testing.T) {
		fs, _, b, id := mk()
		err := b.Finish(core.Version{Epoch: 1, Counter: 90})
		if !errors.Is(err, core.ErrUnknownFormat) {
			t.Fatalf("Finish with an epoch floor = %v, want ErrUnknownFormat", err)
		}
		if present(fs, id) {
			t.Fatal("a failed Finish applied records to the index")
		}
		// The builder is still usable: nothing was applied and no counters
		// moved, so a good floor rebuilds it.
		if err := b.Finish(ver(0)); err != nil {
			t.Fatal(err)
		}
		if !present(fs, id) {
			t.Fatal("the retrying Finish applied nothing")
		}
	})

	t.Run("a marker keeps the change version", func(t *testing.T) {
		var fs files
		id := fid(1)
		b := newRebuilder(&fs, &Counters{})
		if err := b.Feed(recd(RecDelete, id, 3, 9, 1, 0, 0, 0)); err != nil {
			t.Fatal(err)
		}
		if err := b.Finish(ver(0)); err != nil {
			t.Fatal(err)
		}
		if got := fs.asOf(id); got != ver(9) {
			t.Fatalf("asOf = %v, want the delete's version", got)
		}
	})
}

// TestRebuildRefusals is the Feed validation table: each refusal names the
// sentinel it must, buffers nothing, and moves no counter.
func TestRebuildRefusals(t *testing.T) {
	spec := &core.CloneSpec{Src: fid(9), SrcOff: 100, DstOff: 4, Len: 8}
	cloneRecord := func(over func(*Record)) Record {
		r := recd(RecCloneTarget, fid(1), 5, 5, 1, 0, 4, 8)
		r.Clone = spec
		if over != nil {
			over(&r)
		}
		return r
	}
	cases := []struct {
		name string
		rec  Record
		want error
	}{
		{"negative offset", recd(RecWrite, fid(1), 5, 5, 1, 0, -1, 8), core.ErrUnknownFormat},
		{"negative length", recd(RecWrite, fid(1), 5, 5, 1, 0, 0, -8), core.ErrUnknownFormat},
		{"end overflowing", recd(RecWrite, fid(1), 5, 5, 1, 0, math.MaxInt64-4, 8), core.ErrUnknownFormat},
		{"unknown kind", Record{Kind: RecordKind(99), File: fid(1), Numbers: nums(5, 5)}, core.ErrUnknownFormat},
		{"zero kind", Record{File: fid(1), Numbers: nums(5, 5)}, core.ErrUnknownFormat},
		{"write of no bytes", recd(RecWrite, fid(1), 5, 5, 1, 0, 0, 0), core.ErrUnknownFormat},
		{"fill of no bytes", recd(RecFill, fid(1), 5, 5, 1, 0, 4, 0), core.ErrUnknownFormat},
		{"release of no bytes", recd(RecRelease, fid(1), 5, 5, 1, 0, 4, 0), core.ErrUnknownFormat},
		{"offloaded of no bytes", recd(RecOffloaded, fid(1), 5, 5, 1, 0, 4, 0), core.ErrUnknownFormat},
		{"unmark of no bytes", recd(RecUnmark, fid(1), 5, 5, 1, 0, 4, 0), core.ErrUnknownFormat},
		{"truncate with a length", recd(RecTruncate, fid(1), 5, 5, 1, 0, 4, 4), core.ErrUnknownFormat},
		{"delete over an extent", recd(RecDelete, fid(1), 5, 5, 1, 0, 4, 0), core.ErrUnknownFormat},
		{"delete with a length", recd(RecDelete, fid(1), 5, 5, 1, 0, 0, 4), core.ErrUnknownFormat},
		{"deallocate of no bytes", recd(RecDeallocate, fid(1), 5, 5, 1, 0, 4, 0), core.ErrZeroLength},
		{"clone target of no bytes", cloneRecord(func(r *Record) { r.Len = 0; r.Clone.Len = 0 }), core.ErrZeroLength},
		{"clone target without a spec", cloneRecord(func(r *Record) { r.Clone = nil }), core.ErrUnknownFormat},
		{"clone target whose spec disagrees", cloneRecord(func(r *Record) { r.Clone.DstOff = 8 }), core.ErrUnknownFormat},
		{"clone target whose spec length disagrees", cloneRecord(func(r *Record) { r.Clone.Len = 4 }), core.ErrUnknownFormat},
		{"a spec on a write", func() Record {
			r := recd(RecWrite, fid(1), 5, 5, 1, 0, 0, 8)
			r.Clone = spec
			return r
		}(), core.ErrUnknownFormat},
		{"an epoch in the version", func() Record {
			r := recd(RecWrite, fid(1), 5, 5, 1, 0, 0, 8)
			r.Numbers.Version.Epoch = 1
			return r
		}(), core.ErrUnknownFormat},
		{"a version above every issued", recd(RecWrite, fid(1), 5, math.MaxUint64, 1, 0, 0, 8), core.ErrUnknownFormat},
		{"a seq above every issued", func() Record {
			r := recd(RecWrite, fid(1), math.MaxUint64, 5, 1, 0, 0, 8)
			return r
		}(), core.ErrUnknownFormat},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var fs files
			ctr := &Counters{}
			b := newRebuilder(&fs, ctr)
			equalErr(t, "Feed", b.Feed(c.rec), c.want)
			// A refused record buffers nothing: no entry for it, and the
			// counters it carried were never observed.
			if err := b.Finish(ver(0)); err != nil {
				t.Fatal(err)
			}
			if got := ctr.NextVersion(); got != ver(1) {
				t.Fatalf("a refused record raised the version counter to %v", got)
			}
			if seq, err := ctr.NextSeq(); err != nil || seq != 1 {
				t.Fatalf("a refused record raised the seq counter to %d", seq)
			}
			if n := fs.len(); n != 0 {
				t.Fatalf("a refused record left %d entries", n)
			}
		})
	}
}

// TestRebuildOverTheBound: a rebuild over the extent bound succeeds with the
// true count, and content writes stay refused at the bound until the count
// falls below it.
func TestRebuildOverTheBound(t *testing.T) {
	var fs files
	fs.setExtentLimit(1)
	id := fid(1)
	b := newRebuilder(&fs, &Counters{})
	for _, r := range []Record{
		recd(RecWrite, id, 1, 1, 1, 0, 0, 4),
		recd(RecWrite, id, 2, 2, 1, 64, 100, 4),
	} {
		if err := b.Feed(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Finish(ver(0)); err != nil {
		t.Fatalf("Finish over the bound = %v, want nil", err)
	}
	if n := fs.extentCount(); n != 2 {
		t.Fatalf("extentCount() = %d, want the true 2", n)
	}
	if n := fs.indexRefusals(); n != 0 {
		t.Fatalf("a rebuild counted %d refusals, want 0", n)
	}
	if _, err := fs.admitWrite(id, ext(8, 4), false); !isNoSpaceIndex(err) {
		t.Fatalf("admitWrite over the bound = %v, want the index refusal", err)
	}
	if n := fs.indexRefusals(); n != 1 {
		t.Fatalf("indexRefusals() = %d, want the refused write counted", n)
	}
}

// isNoSpaceIndex reports whether err is the bound's named refusal.
func isNoSpaceIndex(err error) bool {
	var n *core.NoSpaceError
	return errors.As(err, &n) && n.Limit == core.LimitIndex
}

// TestRebuildRivalled: records of one FileID under two tags rebuild the file
// rivalled, refusing a check through either tag, identically for every feed
// order.
func TestRebuildRivalled(t *testing.T) {
	build := func(order []int) *files {
		var fs files
		b := newRebuilder(&fs, &Counters{})
		id := fid(1)
		rs := []Record{
			recd(RecWrite, id, 1, 1, 1, 0, 0, 4),
			recd(RecWrite, id, 2, 2, 2, 64, 8, 4),
		}
		rs[1].Tag = 9
		for _, i := range order {
			if err := b.Feed(rs[i]); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.Finish(ver(0)); err != nil {
			t.Fatal(err)
		}
		return &fs
	}
	for _, order := range [][]int{{0, 1}, {1, 0}} {
		fs := build(order)
		id := fid(1)
		for _, tag := range []core.ShareTag{7, 9, 12} {
			fs.view(id, func(f *file) {
				if err := f.check(tag, id); !errors.Is(err, core.ErrOtherShare) {
					t.Fatalf("order %v: check through tag %d = %v, want refused", order, tag, err)
				}
			})
		}
		// Both tags' records are on disk, whichever fed first: the first in
		// sequence order owns the entry, the second is the rival.
		fs.view(id, func(f *file) {
			if f.tag != 7 || f.rival != 9 {
				t.Fatalf("order %v: tags = %d, %d, want 7 owning and 9 the rival", order, f.tag, f.rival)
			}
		})
	}
}

// TestRebuildRefusesAfterADrainedShard: Finish drains a shard under its lock
// and marks it, so a feeder arriving after the drain — before the Finish
// returning has even set finished — is refused with errRebuilt rather than
// handed a nil for a record nobody replays. Without the mark, a feeder that
// raced Finish into a drained shard would lose its record and see success.
func TestRebuildRefusesAfterADrainedShard(t *testing.T) {
	var fs files
	ctr := &Counters{}
	id := fid(1)
	b := newRebuilder(&fs, ctr)
	if err := b.Feed(recd(RecWrite, id, 1, 1, 1, 0, 0, 4)); err != nil {
		t.Fatal(err)
	}
	// Drain id's shard the way Finish's worker does, without ending the
	// builder: the window a feeder racing Finish lands in.
	s := b.shards[hashID(id)>>(64-6)]
	if err := b.replayShard(s); err != nil {
		t.Fatal(err)
	}
	if b.finished.Load() {
		t.Fatal("the builder is finished before Finish returned")
	}
	if err := b.Feed(recd(RecWrite, id, 2, 2, 1, 64, 0, 4)); !errors.Is(err, errRebuilt) {
		t.Fatalf("Feed into a drained shard = %v, want errRebuilt", err)
	}
	if err := b.Retain(id, Retained{Piece: Piece{Extent: ext(0, 4), Version: ver(1)}, By: ver(1)}); !errors.Is(err, errRebuilt) {
		t.Fatalf("Retain into a drained shard = %v, want errRebuilt", err)
	}
	// The refused record moved no counter of either kind: the numbers were
	// never observed, so the journal's next issue still exceeds only what
	// the taken record carried.
	if got := ctr.NextVersion(); got != ver(2) {
		t.Fatalf("NextVersion() = %v after the refused feed, want 2", got)
	}
	if seq, err := ctr.NextSeq(); err != nil || seq != 2 {
		t.Fatalf("NextSeq() = %d, %v after the refused feed, want 2", seq, err)
	}
	// A record for a file of another shard still feeds, and its shard
	// replays at the coming Finish.
	other := fid(9)
	if fs.shard(other) == fs.shard(id) {
		t.Skip("the ids share a shard; pick another")
	}
	if err := b.Feed(recd(RecWrite, other, 3, 3, 2, 0, 0, 4)); err != nil {
		t.Fatal(err)
	}
	if err := b.Finish(ver(0)); err != nil {
		t.Fatal(err)
	}
	if p := heldPieces(&fs, other); len(p) != 1 {
		t.Fatalf("the other shard's record was lost: %+v", p)
	}
}

// TestRebuildRefusesAfterFinish: feeding a finished builder is an error that
// changes nothing, a Record whatever its shape, and it never panics.
func TestRebuildRefusesAfterFinish(t *testing.T) {
	var fs files
	b := newRebuilder(&fs, &Counters{})
	id := fid(1)
	if err := b.Feed(recd(RecWrite, id, 1, 1, 1, 0, 0, 4)); err != nil {
		t.Fatal(err)
	}
	if err := b.Finish(ver(0)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Record{
		recd(RecWrite, id, 2, 2, 1, 64, 0, 4),
		{Kind: 200, File: id, Numbers: nums(9, 9)}, // invalid: no panic, still an error
	} {
		if err := b.Feed(bad); !errors.Is(err, errRebuilt) {
			t.Fatalf("Feed after Finish = %v, want errRebuilt", err)
		}
	}
	if got := fs.asOf(id); got != ver(1) {
		t.Fatalf("a refused feed changed the index: asOf = %v", got)
	}

	// A builder whose Finish failed on its floor still feeds, and the
	// records fed meanwhile rebuild at the next Finish.
	var fs2 files
	b2 := newRebuilder(&fs2, &Counters{})
	if err := b2.Finish(core.Version{Epoch: 3}); !errors.Is(err, core.ErrUnknownFormat) {
		t.Fatalf("Finish = %v, want ErrUnknownFormat", err)
	}
	if err := b2.Feed(recd(RecWrite, id, 1, 1, 1, 0, 0, 4)); err != nil {
		t.Fatalf("Feed after a failed Finish = %v, want nil", err)
	}
	if err := b2.Finish(ver(0)); err != nil {
		t.Fatal(err)
	}
	if !present(&fs2, id) {
		t.Fatal("a feed between a failed Finish and a good one was lost")
	}
}

// TestRebuildSegmentsAndTags is the note pass: every segment holding a record
// of the file is noted, every one of the file's records claims its tag, and
// the drop ranges over them never under-cover a record.
func TestRebuildSegmentsAndTags(t *testing.T) {
	var fs files
	id := fid(1)
	b := newRebuilder(&fs, &Counters{})
	// Nine records over nine segments, offsets spread over the file: the
	// ninth-range merge fires on whichever segment holds two.
	for i := range int64(9) {
		seg := core.SegmentID(1 + i)
		r := recd(RecWrite, id, uint64(i+1), uint64(i+1), seg, 0, i*4096, 4096)
		if i == 8 {
			r = recd(RecWrite, id, 9, 9, core.SegmentID(1), 500, 8*4096, 4096)
		}
		if err := b.Feed(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Finish(ver(0)); err != nil {
		t.Fatal(err)
	}
	fs.view(id, func(f *file) {
		var total int
		for _, s := range f.segs {
			if len(s.ranges) > maxDropRanges {
				t.Fatalf("segment %d keeps %d ranges, want at most %d", s.seg, len(s.ranges), maxDropRanges)
			}
			lo := int64(-1)
			for i, r := range s.ranges {
				if i > 0 && r.start < lo {
					t.Fatalf("ranges out of order: %+v", s.ranges)
				}
				lo = max(lo, r.end)
				if r.start >= r.end {
					t.Fatalf("empty range %+v kept", r)
				}
			}
			total += len(s.ranges)
		}
		if got, want := fs.dropRangeCount(), int64(total); got != want {
			t.Fatalf("dropRangeCount() = %d, want the sum %d over the files", got, want)
		}
		// Every record's extent is covered in its own segment by a range
		// whose pair is at or below its: the soundness property.
		for i := range int64(9) {
			seg := core.SegmentID(1 + i)
			off, n := i*4096, int64(4096)
			if i == 8 {
				seg, off, n = 1, 8*4096, 4096
			}
			var s *segRecords
			for j := range f.segs {
				if f.segs[j].seg == seg {
					s = &f.segs[j]
				}
			}
			if s == nil {
				t.Fatalf("segment %d never noted", seg)
			}
			for _, r := range s.ranges {
				if r.start <= off && r.end >= off+n {
					if r.low.Compare(nums(uint64(i+1), uint64(i+1))) <= 0 {
						goto covered
					}
				}
			}
			t.Fatalf("record %d at [%d, %d) under-covered in segment %d: %+v", i+1, off, n, seg, s.ranges)
		covered:
		}
		if tag, ok := f.owner(); !ok || tag != 7 {
			t.Fatalf("owner = %d, %v, want 7", tag, ok)
		}
	})
}
