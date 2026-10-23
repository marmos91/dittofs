package journalv2

import (
	"io"
	"iter"
	"time"
)

// The journal's calls, written exactly as the journal's contract writes them,
// with no options. If a parameter or result is dropped, reordered or retyped,
// this file stops compiling.
var (
	_ = func() (JournalID, error) { return Init("", InstallationID{}) }
	_ = func() (*Journal, error) { return Open("", Version{}, JournalID{}) }
	_ = func() error { return Restamp("", JournalID{}, InstallationID{}) }
	_ = func(j *Journal) error { return j.Close() }

	_ = func(j *Journal) (*Handle, error) { return j.Share(ShareTag(0), int64(0), Version{}, Bounded) }
	_ = func(j *Journal) error { return j.Forget(ShareTag(0)) }
	_ = func(j *Journal) error { return j.Strand(ShareTag(0)) }
	_ = func(j *Journal) (Version, error) { return j.Draw() }
	_ = func(j *Journal) { j.SetVersionLimit(Version{}) }

	_ = func(h *Handle) error { return h.Close() }
	_ = func(h *Handle) error { return h.SetSyncPolicy(Standard) }
	_ = func(h *Handle) (*Reservation, error) { return h.Reserve(int64(0), 0) }
	_ = func(h *Handle) (Version, error) { return h.WriteAt(FileID{}, int64(0), []byte(nil), int64(0)) }
	_ = func(h *Handle) error { return h.Sync(FileID{}, FileID{}) }
	_ = func(h *Handle) (int, []Extent, []Held, Version, error) {
		return h.ReadAt(FileID{}, int64(0), []byte(nil))
	}
	_ = func(h *Handle) error { return h.Fill(FileID{}, int64(0), []byte(nil), Version{}, Version{}) }
	_ = func(h *Handle) (Version, error) { return h.Truncate(FileID{}, int64(0)) }
	_ = func(h *Handle) (Version, error) { return h.Deallocate(FileID{}, Extent{}) }
	_ = func(h *Handle) (Version, error) { return h.Delete(FileID{}) }
	_ = func(h *Handle) (Version, error) { return h.CloneTarget(FileID{}, CloneSpec{}) }
	_ = func(h *Handle) iter.Seq2[Change, error] { return h.Since(FileID{}, Version{}) }
	_ = func(h *Handle) { h.Settle(FileID{}, Version{}) }
	_ = func(h *Handle) ([]Extent, error) { return h.Extents(FileID{}) }
	_ = func(h *Handle) iter.Seq[FileID] { return h.Files() }
	_ = func(h *Handle) iter.Seq2[FileID, time.Time] { return h.DirtyFiles() }

	_ = func(r *Reservation) (Version, error) { return r.WriteAt(FileID{}, int64(0), []byte(nil), int64(0)) }
	_ = func(r *Reservation) error { return r.Close() }

	_ = CloneSpec{SrcShare: ShareID{}, Src: FileID{}, SrcOff: 0, DstOff: 0, Len: 0, AsOf: Version{}}
	_ = Change{Kind: ChangeWrite, Extent: Extent{}, Version: Version{}, Mtime: 0, Clone: (*CloneSpec)(nil)}
	_ = []ChangeKind{ChangeWrite, ChangeTruncate, ChangeDeallocate, ChangeDelete, ChangeCloneTarget, ChangeLoss}

	_ = func(h *Handle) error {
		return h.Offload(FileID{}, Version{}, int64(0), int64(0), func(o Offer, report func(offloaded []Extent) error) error {
			return report([]Extent(nil))
		})
	}
	_ = func(h *Handle) error {
		return h.OffloadMany([]Capture(nil), int64(0), int64(0), func(offers []Offer, report func(id FileID, offloaded []Extent) error) error {
			return report(FileID{}, []Extent(nil))
		})
	}
	_ = func(h *Handle) error { return h.MarkOffloaded(FileID{}, []Extent(nil), Version{}, Version{}) }
	_ = func(h *Handle) error { return h.Unmark(FileID{}, []Extent(nil)) }
	_ = func(h *Handle) error { return h.Release(FileID{}, []Extent(nil)) }
	_ = func(h *Handle) error { return h.Hold(ShareID{}, SnapshotCut(0), map[FileID]Version(nil)) }
	_ = func(h *Handle) error { return h.Unhold(ShareID{}, SnapshotCut(0)) }
	_ = func(h *Handle) int64 { return h.Held(ShareID{}) }
	_ = func(h *Handle) error { return h.Stamp(FileID{}, Version{}, SnapshotCut(0)) }
	_ = func(j *Journal) (int64, error) { return j.Repack(int64(0), RepackScope{}) }

	_ = Offer{ID: FileID{}, Dirty: []OfferedExtent(nil), Neighbours: []Held(nil), Superseded: []OfferedExtent(nil),
		Oldest: Version{}, Newest: Version{}, Offered: io.ReaderAt(nil)}
	_ = OfferedExtent{Extent: Extent{}, Version: Version{}, Cut: SnapshotCut(0), Stamped: false}
	_ = Capture{ID: FileID{}, Through: Version{}}
	_ = RepackScope{Share: ShareTag(0), Segments: []SegmentID(nil)}
)
