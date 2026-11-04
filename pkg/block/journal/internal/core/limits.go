package core

import "time"

// The journal's fixed values: constants of the implementation, not settings,
// and not part of the public API, so changing one breaks no caller.
//
// decision: each is the value the journal's design proposes, and none is
// measured yet; changing one changes the implementation, not a deployment.
const (
	// SegmentCap is the size of a segment, in bytes.
	SegmentCap = 256 << 20

	// SyncBound is the longest a record written under the Bounded policy waits
	// to be synced.
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
