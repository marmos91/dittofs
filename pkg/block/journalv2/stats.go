package journalv2

import "time"

// Stats returns the journal's statistics: journal-wide, with a per-share
// breakdown. It is served from counters the journal maintains: it neither
// walks the placement index or the segment set nor blocks a write, and it is
// safe to call concurrently with every other operation. After Close it
// returns the zero Stats, since it has no error to return.
//
// decision: the stub returns the zero Stats because the signature has no
// error to carry ErrNotImplemented. A zero Stats reads as an empty journal, so
// no caller may depend on it until the counters land.
func (j *Journal) Stats() Stats {
	return Stats{}
}

// Stats is the journal's statistics. It is not a consistent snapshot of the
// journal. It guarantees only these inequalities, each kept by the order in
// which the counters are updated:
//
//   - DirtyBytes <= HeldBytes;
//   - UsedBytes >= the storage the segments allocate;
//   - ReservedBytes >= 0.
//
// Any other relation between two fields may be momentarily off by one
// in-flight operation.
//
// Each field's comment names its kind: a gauge is a level that rises and
// falls, a counter only rises and restarts at zero on open, a histogram is a
// distribution, and a value is reported as it is rather than charted.
//
// Every map and slice in a Stats belongs to the caller: Stats returns copies
// and never keeps them.
type Stats struct {
	// MaxBytes is the configured capacity. Kind: gauge.
	MaxBytes int64

	// UsedBytes is the local storage allocated: the segments, each at its
	// accounted size. Clean held bytes, dirty bytes and unreclaimed bytes all
	// count here. Kind: gauge.
	UsedBytes int64

	// ReservedBytes is the record bytes reserved by in-flight writes inside
	// allocated segments, not yet written. Kind: gauge.
	ReservedBytes int64

	// HeldBytes is the sum of held extent lengths. Kind: gauge.
	HeldBytes int64

	// DirtyBytes is the held bytes whose offloaded bit is unset. Kind: gauge.
	DirtyBytes int64

	// OldestDirty is when the oldest held extent whose offloaded bit is unset
	// was written: the age of the upload backlog. Zero when nothing is dirty.
	// Kind: value.
	OldestDirty time.Time

	// Files is the number of files with at least one held extent. Kind: gauge.
	Files int

	// ExtentCount is the number of placement index entries: held extents,
	// retained extents and removal markers. Kind: gauge.
	ExtentCount int64

	// DropRanges is the number of ranges the removal-drop test keeps per file
	// and segment. Kind: gauge.
	DropRanges int

	// Segments is the number of segments in total, and Sealed the number of
	// them sealed. Both are gauges.
	Segments, Sealed int

	// UnreclaimedBytes is the storage in segments not yet unlinked that backs
	// no held or retained extent and no live record: what repack can recover.
	// Kind: gauge.
	UnreclaimedBytes int64

	// HeadroomBytes is the reserved headroom for records without bytes, and
	// RepackReserveBytes the repack reserve, each as allocated now. Both are
	// gauges.
	HeadroomBytes, RepackReserveBytes int64

	// PinnedSegments is the number of segments held by running offers. Kind:
	// gauge.
	PinnedSegments int

	// OpenDescriptors is the number of open segment descriptors, and
	// DescriptorLimit the bound they are held under. Both are gauges.
	OpenDescriptors, DescriptorLimit int

	// DamagedSegments is the number of segments holding records that do not
	// verify. Kind: gauge.
	DamagedSegments int

	// LastSyncError is the most recent sync failure; nil when no sync has
	// failed since open. Kind: value.
	LastSyncError *SyncFailure

	// SyncDuration is the time per sync. Kind: histogram.
	SyncDuration Histogram

	// RemovalMarkers is the number of removal markers not yet settled. Kind:
	// gauge.
	RemovalMarkers int

	// VersionLimit is the version limit, VersionsLeft how many versions remain
	// below it, and NextVersion the next version the counter issues. Kinds:
	// value, gauge, value.
	VersionLimit Version
	VersionsLeft uint64
	NextVersion  Version

	// ExtentLimit is the configured index entry bound. Kind: gauge.
	ExtentLimit int64

	// Counters are the cumulative counts since open. Kind: counters.
	Counters Counters

	// RejectedSegments is the number of segments not attached at open. Kind:
	// gauge.
	RejectedSegments int

	// Recovery is how the last open rebuilt the journal. Kind: value.
	Recovery RecoveryStats

	// Losses are the most recent loss events, oldest first. The journal keeps
	// a bounded number of them; a poller that sees a gap in their sequence
	// numbers knows how many it missed. Kind: value.
	Losses []LossEvent

	// LossGeneration is raised on every loss of content not yet offloaded and
	// every failed sync window. It is never lower than any value this process
	// has exposed for this journal directory. Kind: value.
	LossGeneration uint64

	// TagConflicts are the files recovery rebuilt under two tags. Every call on
	// such a file is refused until one of the tags is forgotten. Kind: value.
	TagConflicts []TagConflict

	// Shares holds, for every tag that holds a record, attached or not, that
	// share's statistics. Kind: per share.
	Shares map[ShareTag]ShareStats
}

// ShareStats is one share's statistics. Every field is a gauge.
type ShareStats struct {
	// Limit is the share's limit.
	Limit int64

	// UsedBytes is the record bytes of the share's records still on disk.
	UsedBytes int64

	ReservedBytes int64
	HeldBytes     int64
	DirtyBytes    int64

	// UnreclaimedBytes is the unreclaimed storage counted to the share: that
	// of its records.
	UnreclaimedBytes int64
}

// Counters are the journal's cumulative counts since open. Every field is a
// counter: it only rises, and restarts at zero on open. Each answers something
// only the journal knows.
type Counters struct {
	// WrittenBytes is the client bytes accepted by WriteAt.
	WrittenBytes uint64

	// FilledBytes is the remote bytes placed by Fill.
	FilledBytes uint64

	// FillRefused is the number of fills refused as older than the file.
	FillRefused uint64

	// OfferedBytes is the bytes offered at offload, and MarkedBytes the bytes
	// marked offloaded.
	OfferedBytes, MarkedBytes uint64

	// ReleasedBytes is the bytes released, and FreedBytes the storage actually
	// returned by unlinking segments.
	ReleasedBytes, FreedBytes uint64

	// RepackedBytes is the bytes moved by repack.
	RepackedBytes uint64

	// ReservationRefused is the number of writes refused at capacity.
	ReservationRefused uint64

	// Syncs is the number of syncs issued, and SyncFailures the number failed.
	Syncs, SyncFailures uint64

	// SyncReappendedBytes is the bytes re-appended to a new segment after a
	// failed sync.
	SyncReappendedBytes uint64

	// HeadroomDraws is the number of records without bytes appended from the
	// reserved headroom while the journal was at its limit.
	HeadroomDraws uint64

	// CorruptExtentsDataLoss is the number of extents dropped as corrupt with
	// their offloaded bit unset, and CorruptExtentsRepairable the number with
	// it set.
	CorruptExtentsDataLoss, CorruptExtentsRepairable uint64

	// StaleExtents is the number of extents dropped as older than offloaded
	// content.
	StaleExtents uint64

	// TornTails is the number of torn tails truncated at open.
	TornTails uint64

	// ScanResyncs is the number of scans that found a record by
	// resynchronising past a bad one.
	ScanResyncs uint64

	// IndexRefusals is the number of writes refused at the index entry bound.
	IndexRefusals uint64

	// SegmentReopens is the number of reads that had to reopen a closed
	// segment.
	SegmentReopens uint64

	// SpareRefusals is the number of rolls refused at once because no spare
	// was ready.
	SpareRefusals uint64

	// DoubleRetires is the number of retires that subtracted nothing from the
	// accounted footprint.
	DoubleRetires uint64
}

// Histogram is a distribution of durations over fixed buckets.
//
// decision: Bounds, Counts and Sum are what a metrics exporter needs to
// publish the distribution, and nothing more is kept.
type Histogram struct {
	// Bounds are the buckets' upper bounds, ascending.
	Bounds []time.Duration

	// Counts holds one more entry than Bounds: Counts[i] is the number of
	// samples at or below Bounds[i] and above the bound before it, and the
	// last entry the number above every bound.
	Counts []uint64

	// Sum is the total of every sample.
	Sum time.Duration
}

// RecoveryStats is how an open rebuilt the journal.
type RecoveryStats struct {
	// CatalogSegments is the number of segments read by their catalog, and
	// ScannedSegments the number read by scan.
	CatalogSegments, ScannedSegments int

	// Duration is how long the rebuild took.
	Duration time.Duration
}

// SyncFailure is a failed sync, and when it failed.
type SyncFailure struct {
	Err error
	At  time.Time
}

// LossEvent is an extent the journal stopped holding without being asked.
type LossEvent struct {
	// SeqNum is the event's sequence number in the loss ring, not the sequence
	// number of the record it dropped.
	SeqNum uint64

	Share   ShareTag
	File    FileID
	Extent  Extent
	Version Version
	Reason  LossReason

	// Offloaded is whether the extent's offloaded bit was set.
	Offloaded bool

	// AtOpen reports that the event was found at open, from a loss record
	// still on disk.
	AtOpen bool
}

// LossReason is why the journal stopped holding an extent. The zero
// LossReason names none.
type LossReason uint8

// The loss reasons.
const (
	// LossCorrupt is an extent dropped as corrupt.
	LossCorrupt LossReason = iota + 1

	// LossStale is an extent dropped as older than offloaded content.
	LossStale

	// LossFailedWindow is an extent dropped because its sync window failed.
	LossFailedWindow
)

// TagConflict is a file recovery rebuilt under two tags.
type TagConflict struct {
	File FileID
	Tags [2]ShareTag
}
