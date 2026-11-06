package enginetest

import (
	"errors"
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
)

var (
	fileA = journal.NewFileID([16]byte{'a'})
	fileB = journal.NewFileID([16]byte{'b'})
)

func ver(n uint64) journal.Version { return journal.Version{Counter: n} }

func ext(off, n int64) journal.Extent { return journal.Extent{Off: off, Len: n} }

// fake returns a fake journal with a version limit and a handle on tag 1
// with the given limit.
func fake(t *testing.T, limit int64) (*FakeJournal, engine.Handle) {
	t.Helper()
	j := NewFakeJournal()
	if err := j.SetVersionLimit(ver(1 << 32)); err != nil {
		t.Fatal(err)
	}
	h, err := j.Share(1, limit, journal.Version{})
	if err != nil {
		t.Fatal(err)
	}
	return j, h
}

func write(t *testing.T, h engine.Handle, id journal.FileID, off int64, p string) journal.Version {
	t.Helper()
	v, err := h.WriteAt(t.Context(), id, off, []byte(p), 7)
	if err != nil {
		t.Fatalf("write %q at %d: %v", p, off, err)
	}
	return v
}

func changes(t *testing.T, h engine.Handle, id journal.FileID, from journal.Version) []journal.Change {
	t.Helper()
	var out []journal.Change
	for c, err := range h.Since(t.Context(), id, from) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// A read serves the newest bytes held, names each part's version, reports
// every part not held as missing with its bytes untouched, and answers the
// file's latest change as asOf.
func TestFakeReadServesHeldAndReportsMissing(t *testing.T) {
	_, h := fake(t, 0)
	v1 := write(t, h, fileA, 2, "aaaaaa") // [2, 8)
	v2 := write(t, h, fileA, 4, "bb")     // [4, 6)
	if v2.Compare(v1) <= 0 {
		t.Fatalf("versions %v then %v do not rise", v1, v2)
	}
	p := []byte("..........")
	n, missing, held, asOf, err := h.ReadAt(t.Context(), fileA, 0, p)
	if err != nil {
		t.Fatal(err)
	}
	if string(p) != "..aabbaa.." || n != 6 {
		t.Errorf("read %q, n %d; want ..aabbaa.., 6", p, n)
	}
	if want := []journal.Extent{ext(0, 2), ext(8, 2)}; !slices.Equal(missing, want) {
		t.Errorf("missing %v, want %v", missing, want)
	}
	if want := []journal.Held{{Extent: ext(2, 2), Version: v1}, {Extent: ext(4, 2), Version: v2}, {Extent: ext(6, 2), Version: v1}}; !slices.Equal(held, want) {
		t.Errorf("held %v, want %v", held, want)
	}
	if asOf != v2 {
		t.Errorf("asOf %v, want %v", asOf, v2)
	}
}

// Removals stop holding their range and leave markers; Since yields what is
// above its from in version order, and Settle drops markers at or below its
// version.
func TestFakeRemovalsLeaveMarkersUntilSettled(t *testing.T) {
	_, h := fake(t, 0)
	ctx := t.Context()
	w := write(t, h, fileA, 0, "abcdefgh")
	d, err := h.Deallocate(ctx, fileA, ext(2, 2))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := h.Truncate(ctx, fileA, 6)
	if err != nil {
		t.Fatal(err)
	}
	got := changes(t, h, fileA, journal.Version{})
	kinds := []journal.ChangeKind{journal.ChangeWrite, journal.ChangeWrite, journal.ChangeDeallocate, journal.ChangeTruncate}
	if len(got) != len(kinds) {
		t.Fatalf("Since = %+v, want kinds %v", got, kinds)
	}
	for i, c := range got {
		if c.Kind != kinds[i] {
			t.Errorf("change %d is kind %v, want %v", i, c.Kind, kinds[i])
		}
	}
	if got[0].Extent != ext(0, 2) || got[1].Extent != ext(4, 2) || got[0].Version != w || got[0].Mtime != 7 {
		t.Errorf("held after removals: %+v, %+v; want [0,2) and [4,6) at %v, mtime 7", got[0], got[1], w)
	}
	if got := changes(t, h, fileA, d); len(got) != 1 || got[0].Version != tr {
		t.Errorf("Since(%v) = %+v; want the truncate alone", d, got)
	}
	h.Settle(fileA, d)
	if got := changes(t, h, fileA, w); len(got) != 1 || got[0].Kind != journal.ChangeTruncate {
		t.Errorf("after Settle(%v): Since = %+v; want the truncate alone", d, got)
	}
}

// A loss stops holding the extent, is reported by Since at the version it
// dropped and joins the loss ring; a corrupt loss and a failed window's raise
// the generation and a stale one does not; a failed window's, never synced,
// is not reported by Since; a loss of nothing held records nothing.
func TestFakeLossRingAndGeneration(t *testing.T) {
	j, h := fake(t, 0)
	v := write(t, h, fileA, 0, "abcd")
	j.Lose(fileA, ext(0, 2), journal.LossCorrupt)
	j.Lose(fileB, ext(0, 2), journal.LossCorrupt)
	s := j.Stats()
	if s.LossGeneration != 1 || len(s.Losses) != 1 {
		t.Fatalf("generation %d, ring %+v; want one loss", s.LossGeneration, s.Losses)
	}
	if e := s.Losses[0]; e.File != fileA || e.Extent != ext(0, 2) || e.Version != v || e.Reason != journal.LossCorrupt {
		t.Errorf("loss event %+v", e)
	}
	_, missing, _, _, _ := h.ReadAt(t.Context(), fileA, 0, make([]byte, 4))
	if !slices.Equal(missing, []journal.Extent{ext(0, 2)}) {
		t.Errorf("missing after the loss: %v", missing)
	}
	if got := changes(t, h, fileA, journal.Version{}); len(got) != 2 || got[1].Kind != journal.ChangeLoss {
		t.Errorf("Since = %+v; want the write's rest, then the loss", got)
	}
	j.Lose(fileA, ext(2, 1), journal.LossStale)
	if s := j.Stats(); s.LossGeneration != 1 || len(s.Losses) != 2 {
		t.Errorf("after a stale loss: generation %d, ring %+v; want generation 1, two losses", s.LossGeneration, s.Losses)
	}
	j.Lose(fileA, ext(3, 1), journal.LossFailedWindow)
	if s := j.Stats(); s.LossGeneration != 2 || len(s.Losses) != 3 {
		t.Errorf("after a failed window's loss: generation %d, ring %+v; want generation 2, three losses", s.LossGeneration, s.Losses)
	}
	if got := changes(t, h, fileA, journal.Version{}); len(got) != 2 || got[1].Extent != ext(2, 1) {
		t.Errorf("Since after a failed window's loss = %+v; want the two synced losses, the stale one last", got)
	}
}

// Each refusal is the journal's named one.
func TestFakeRefusals(t *testing.T) {
	j, h := fake(t, 4)
	ctx := t.Context()
	write(t, h, fileA, 0, "abc")
	var ns *journal.NoSpaceError
	if _, err := h.WriteAt(ctx, fileA, 3, []byte("de"), 0); !errors.As(err, &ns) || ns.Limit != journal.LimitShare {
		t.Errorf("write past the share's limit: %v", err)
	}
	other, err := j.Share(2, 0, journal.Version{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.WriteAt(ctx, fileA, 0, []byte("x"), 0); !errors.Is(err, journal.ErrOtherShare) {
		t.Errorf("write to another share's file: %v", err)
	}
	// A file belongs to the tag of its records: reading one with none claims
	// nothing.
	if _, _, _, _, err := other.ReadAt(ctx, fileB, 0, make([]byte, 1)); err != nil {
		t.Errorf("read of a file with no records: %v", err)
	}
	for _, err := range other.Since(ctx, fileB, journal.Version{}) {
		t.Errorf("Since of a file with no records: %v", err)
	}
	if _, err := h.WriteAt(ctx, fileB, 0, []byte("y"), 0); err != nil {
		t.Errorf("write to a file another share only read: %v", err)
	}
	if _, err := h.Deallocate(ctx, fileA, ext(1, 0)); !errors.Is(err, journal.ErrZeroLength) {
		t.Errorf("zero-length deallocate: %v", err)
	}
	if err := j.SetVersionLimit(ver(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Draw(); !errors.As(err, &ns) || ns.Limit != journal.LimitVersion {
		t.Errorf("draw at the version limit: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Truncate(ctx, fileA, 0); !errors.Is(err, journal.ErrClosed) {
		t.Errorf("truncate after Close: %v", err)
	}
}

// Share raises the counter above its floor, and nothing is issued before a
// version limit.
func TestFakeVersionsAboveFloorAndLimit(t *testing.T) {
	j := NewFakeJournal()
	if _, err := j.Draw(); !errors.Is(err, journal.ErrNoSpace) {
		t.Errorf("draw before a version limit: %v", err)
	}
	if err := j.SetVersionLimit(ver(100)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Share(1, 0, ver(50)); err != nil {
		t.Fatal(err)
	}
	if v, err := j.Draw(); err != nil || v != ver(51) {
		t.Errorf("draw after a floor of 50 = %v, %v; want 51", v, err)
	}
}
