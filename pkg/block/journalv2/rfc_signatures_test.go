package journalv2

import (
	"context"
	"io"
	"iter"
	"log/slog"
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

	_ = func(j *Journal) Stats { return j.Stats() }
	_ = func(j *Journal) Inspector { return j.Inspect() }
	_ = func(in Inspector) ([]IndexEntry, error) { return in.File(FileID{}) }
	_ = func(in Inspector) iter.Seq2[FileID, []IndexEntry] { return in.All() }
	_ = func(in Inspector) []SegmentInfo { return in.Segments() }
	_ = func(in Inspector) (VerifyReport, error) { return in.Verify(context.Context(nil)) }
	_ = Tracer(func(ctx context.Context, op SpanOp, attrs ...slog.Attr) (context.Context, func(err error)) {
		return ctx, nil
	})

	_ = Offer{ID: FileID{}, Dirty: []OfferedExtent(nil), Neighbours: []Held(nil), Superseded: []OfferedExtent(nil),
		Oldest: Version{}, Newest: Version{}, Offered: io.ReaderAt(nil)}
	_ = OfferedExtent{Extent: Extent{}, Version: Version{}, Cut: SnapshotCut(0), Stamped: false}
	_ = Capture{ID: FileID{}, Through: Version{}}
	_ = RepackScope{Share: ShareTag(0), Segments: []SegmentID(nil)}
	_ = Stats{
		MaxBytes: 0, UsedBytes: 0, ReservedBytes: 0, HeldBytes: 0, DirtyBytes: 0,
		OldestDirty: time.Time{}, Files: 0, ExtentCount: 0, DropRanges: 0,
		Segments: 0, Sealed: 0, UnreclaimedBytes: 0,
		HeadroomBytes: 0, RepackReserveBytes: 0, PinnedSegments: 0,
		OpenDescriptors: 0, DescriptorLimit: 0, DamagedSegments: 0,
		LastSyncError: (*SyncFailure)(nil), SyncDuration: Histogram{},
		RemovalMarkers: 0, VersionLimit: Version{}, VersionsLeft: 0, NextVersion: Version{},
		ExtentLimit: 0, Counters: Counters{}, RejectedSegments: 0,
		Recovery: RecoveryStats{}, Losses: []LossEvent(nil), LossGeneration: 0,
		TagConflicts: []TagConflict(nil), Shares: map[ShareTag]ShareStats(nil),
	}
	_ = ShareStats{Limit: 0, UsedBytes: 0, ReservedBytes: 0, HeldBytes: 0, DirtyBytes: 0, UnreclaimedBytes: 0}
	_ = Counters{
		WrittenBytes: 0, FilledBytes: 0, FillRefused: 0, OfferedBytes: 0, MarkedBytes: 0,
		ReleasedBytes: 0, FreedBytes: 0, RepackedBytes: 0, ReservationRefused: 0,
		Syncs: 0, SyncFailures: 0, SyncReappendedBytes: 0, HeadroomDraws: 0,
		CorruptExtentsDataLoss: 0, CorruptExtentsRepairable: 0, StaleExtents: 0,
		TornTails: 0, ScanResyncs: 0, IndexRefusals: 0, SegmentReopens: 0,
		SpareRefusals: 0, DoubleRetires: 0,
	}
	_ = Histogram{Bounds: []time.Duration(nil), Counts: []uint64(nil)}
	_ = RecoveryStats{CatalogSegments: 0, ScannedSegments: 0, Duration: time.Duration(0)}
	_ = SyncFailure{Err: error(nil), At: time.Time{}}
	_ = LossEvent{SeqNum: 0, Share: ShareTag(0), File: FileID{}, Extent: Extent{}, Version: Version{},
		Reason: LossCorrupt, Offloaded: false, AtOpen: false}
	_ = []LossReason{LossCorrupt, LossStale, LossFailedWindow}
	_ = TagConflict{File: FileID{}, Tags: [2]ShareTag{}}
)
