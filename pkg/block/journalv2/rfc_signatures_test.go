package journalv2

import (
	"context"
	"io"
	"iter"
	"time"
)

// The journal's calls, written exactly as the journal's contract writes them,
// with no options. If a parameter or result is dropped, reordered or retyped,
// this file stops compiling.
var (
	_ = func() (JournalID, error) { return Init(context.Context(nil), "", InstallationID{}) }
	_ = func() (*Journal, error) { return Open(context.Context(nil), "", Version{}, JournalID{}) }
	_ = func() error { return Restamp(context.Context(nil), "", JournalID{}, InstallationID{}) }
	_ = func(j *Journal) error { return j.Close() }

	_ = func(j *Journal) (*Handle, error) { return j.Share(ShareTag(0), int64(0), Version{}, Bounded) }
	_ = func(j *Journal) error { return j.Forget(context.Context(nil), ShareTag(0)) }
	_ = func(j *Journal) error { return j.Strand(ShareTag(0)) }
	_ = func(j *Journal) (Version, error) { return j.Draw() }
	_ = func(j *Journal) { j.SetVersionLimit(Version{}) }

	_ = func(h *Handle) error { return h.Close() }
	_ = func(h *Handle) error { return h.SetSyncPolicy(Standard) }
	_ = func(h *Handle) (*Reservation, error) { return h.Reserve(int64(0), 0) }
	_ = func(h *Handle) (Version, error) {
		return h.WriteAt(context.Context(nil), FileID{}, int64(0), []byte(nil), int64(0))
	}
	_ = func(h *Handle) error { return h.Sync(context.Context(nil), FileID{}, FileID{}) }
	_ = func(h *Handle) (int, []Extent, []Held, Version, error) {
		return h.ReadAt(context.Context(nil), FileID{}, int64(0), []byte(nil))
	}
	_ = func(h *Handle) error {
		return h.Fill(context.Context(nil), FileID{}, int64(0), []byte(nil), Version{}, Version{})
	}
	_ = func(h *Handle) (Version, error) { return h.Truncate(context.Context(nil), FileID{}, int64(0)) }
	_ = func(h *Handle) (Version, error) { return h.Deallocate(context.Context(nil), FileID{}, Extent{}) }
	_ = func(h *Handle) (Version, error) { return h.Delete(context.Context(nil), FileID{}) }
	_ = func(h *Handle) (Version, error) { return h.CloneTarget(context.Context(nil), FileID{}, CloneSpec{}) }
	_ = func(h *Handle) iter.Seq2[Change, error] { return h.Since(context.Context(nil), FileID{}, Version{}) }
	_ = func(h *Handle) { h.Settle(FileID{}, Version{}) }
	_ = func(h *Handle) ([]Extent, error) { return h.Extents(FileID{}) }
	_ = func(h *Handle) iter.Seq[FileID] { return h.Files() }
	_ = func(h *Handle) iter.Seq2[FileID, time.Time] { return h.DirtyFiles() }

	_ = func(r *Reservation) (Version, error) {
		return r.WriteAt(context.Context(nil), FileID{}, int64(0), []byte(nil), int64(0))
	}
	_ = func(r *Reservation) error { return r.Close() }

	_ = CloneSpec{SrcShare: ShareID{}, Src: FileID{}, SrcOff: 0, DstOff: 0, Len: 0, AsOf: Version{}}
	_ = Change{Kind: ChangeWrite, Extent: Extent{}, Version: Version{}, Mtime: 0, Clone: (*CloneSpec)(nil)}
	_ = []ChangeKind{ChangeWrite, ChangeTruncate, ChangeDeallocate, ChangeDelete, ChangeCloneTarget, ChangeLoss}

	_ = func(h *Handle) error {
		return h.Offload(context.Context(nil), FileID{}, Version{}, int64(0), int64(0), func(ctx context.Context, o Offer, report func(offloaded []Extent) error) error {
			return report([]Extent(nil))
		})
	}
	_ = func(h *Handle) error {
		return h.OffloadMany(context.Context(nil), []Capture(nil), int64(0), int64(0), func(ctx context.Context, offers []Offer, report func(id FileID, offloaded []Extent) error) error {
			return report(FileID{}, []Extent(nil))
		})
	}
	_ = func(h *Handle) error {
		return h.MarkOffloaded(context.Context(nil), FileID{}, []Extent(nil), Version{}, Version{})
	}
	_ = func(h *Handle) error { return h.Unmark(context.Context(nil), FileID{}, []Extent(nil)) }
	_ = func(h *Handle) error { return h.Release(context.Context(nil), FileID{}, []Extent(nil)) }
	_ = func(h *Handle) error {
		return h.Hold(context.Context(nil), ShareID{}, SnapshotCut(0), map[FileID]Version(nil))
	}
	_ = func(h *Handle) error { return h.Unhold(context.Context(nil), ShareID{}, SnapshotCut(0)) }
	_ = func(h *Handle) int64 { return h.Held(ShareID{}) }
	_ = func(h *Handle) error { return h.Stamp(context.Context(nil), FileID{}, Version{}, SnapshotCut(0)) }
	_ = func(j *Journal) (int64, error) { return j.Repack(context.Context(nil), int64(0), RepackScope{}) }

	_ = Offer{ID: FileID{}, Dirty: []OfferedExtent(nil), Neighbours: []Held(nil), Superseded: []OfferedExtent(nil),
		Oldest: Version{}, Newest: Version{}, Offered: io.ReaderAt(nil)}
	_ = OfferedExtent{Extent: Extent{}, Version: Version{}, Cut: SnapshotCut(0), Stamped: false}
	_ = Capture{ID: FileID{}, Through: Version{}}
	_ = RepackScope{Share: ShareTag(0), Segments: []SegmentID(nil)}
)
