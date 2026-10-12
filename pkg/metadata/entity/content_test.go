package entity

import (
	"testing"
)

func TestJournalVersionOrder(t *testing.T) {
	a, b := VersionOf(1), VersionOf(2)
	if !a.Less(b) || b.Less(a) || a.Compare(a) != 0 || MaxVersion(a, b) != b {
		t.Error("versions do not order by counter")
	}
	if a.Epoch() != 0 || b.Counter() != 2 || !(JournalVersion{}).IsZero() {
		t.Error("epoch and counter halves")
	}
	// The epoch is the high half: a higher epoch outranks any counter.
	var e JournalVersion
	e[7] = 1
	if !VersionOf(1 << 63).Less(e) {
		t.Error("the epoch is not the high half")
	}
}

func TestExtent(t *testing.T) {
	e := ExtentOf(10, 20)
	for _, c := range []struct {
		o                  Extent
		overlaps, contains bool
		inter              Extent
	}{
		{ExtentOf(0, 10), false, false, Extent{Offset: 10}},
		{ExtentOf(5, 15), true, false, ExtentOf(10, 15)},
		{ExtentOf(12, 18), true, true, ExtentOf(12, 18)},
		{ExtentOf(19, 30), true, false, ExtentOf(19, 20)},
		{ExtentOf(20, 25), false, false, Extent{Offset: 20}},
	} {
		if e.Overlaps(c.o) != c.overlaps || e.Contains(c.o) != c.contains || e.Intersect(c.o) != c.inter {
			t.Errorf("%v against %v: overlaps %v contains %v intersect %v", e, c.o, e.Overlaps(c.o), e.Contains(c.o), e.Intersect(c.o))
		}
	}
	tail := Extent{Offset: 100, Length: EndOfFile}
	if tail.End() != EndOfFile || !tail.Contains(ExtentOf(1<<40, 1<<41)) {
		t.Error("an extent to end of file does not reach it")
	}
}

func TestChunkRef(t *testing.T) {
	r := ChunkRef{Offset: 100, Length: 50, Skip: 10, Chunk: ChunkHash{1}, Born: 2}
	if c := r.Clip(ExtentOf(120, 200)); c.Offset != 120 || c.Length != 30 || c.Skip != 30 {
		t.Errorf("clip %+v", c)
	}
	if c := r.Clip(ExtentOf(0, 50)); c.Length != 0 {
		t.Errorf("clip outside the ref: %+v", c)
	}
	// Born 2: snapshots 3 and on see it while live.
	for k, want := range map[SnapshotCut]bool{1: false, 2: false, 3: true, 9: true} {
		if r.VisibleAt(k) != want {
			t.Errorf("live, born 2, at %d: %v", k, !want)
		}
	}
	// Superseded at 5: snapshots 3 to 5 see it.
	r.Died = 5
	for k, want := range map[SnapshotCut]bool{2: false, 3: true, 5: true, 6: false} {
		if r.VisibleAt(k) != want {
			t.Errorf("born 2, died 5, at %d: %v", k, !want)
		}
	}
	if r.IsLive() || r.IsZeroRef() || !(ChunkRef{}).IsZeroRef() {
		t.Error("live or zero")
	}
}

func TestRemovalMasks(t *testing.T) {
	r := Removal{Version: VersionOf(4), Extent: Extent{Offset: 3 << 20, Length: EndOfFile}}
	if !r.Masks(VersionOf(3), ExtentOf(2<<20, 4<<20)) {
		t.Error("an older ref over the extent is not masked")
	}
	if r.Masks(VersionOf(4), ExtentOf(3<<20, 4<<20)) || r.Masks(VersionOf(3), ExtentOf(0, 1<<20)) {
		t.Error("masked content at the removal's version, or outside its extent")
	}
	r.Done = true
	if r.Masks(VersionOf(1), ExtentOf(3<<20, 4<<20)) {
		t.Error("a done removal masks")
	}
}

func TestBlockRetired(t *testing.T) {
	if (Block{State: BlockLive}).Retired() || !(Block{State: BlockRetired}).Retired() || (Block{State: BlockRetired, Live: 1}).Retired() {
		t.Error("Retired")
	}
}
