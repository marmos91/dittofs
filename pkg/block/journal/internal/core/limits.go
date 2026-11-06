package core

import "time"

// The journal's fixed values: constants of the implementation, not settings,
// and not part of the public API, so changing one breaks no caller.
//
// decision: each is the value the journal's design proposes, and none is
// measured yet; changing one changes the implementation, not a deployment.
const (
	// SegmentCap is the size of a segment, in bytes.
	//
	// ponytail: the size is constant, so a stream writing at an NVMe
	// drive's 5 GB/s rolls about twenty times a second — each roll a seal's
	// two syncs of 256 MiB, a flush a transfer that flushes only at its end
	// never asked for. Upgrade to a larger constant, chosen by the
	// sequential-write row and the repack amplification row of the journal's
	// own benchmarks measured at two or three sizes, when the spare-refusal
	// counter or the sequential row shows rolls arriving at the device's
	// speed.
	SegmentCap = 256 << 20

	// SyncBound is the longest a record waits to be made durable by a sync of
	// its own accord, when no Sync names its file.
	SyncBound = time.Second

	// OfferPins is the most segments one offer pins.
	OfferPins = 64

	// JournalPins is the most segments all running offers pin together.
	JournalPins = 256

	// FloorBuckets is the number of floor buckets a file with no index entry
	// is checked against when it is filled.
	FloorBuckets = 4096

	// IdleSealAge is how long an active segment takes no append before it is
	// sealed, once past the idle-seal threshold.
	IdleSealAge = 30 * time.Second
)
