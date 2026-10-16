package core

// SyncPolicy is a share's sync policy: when a write through the share's handle
// becomes durable without an explicit sync.
type SyncPolicy uint8

const (
	// Bounded, the default, makes a write durable by the first of a sync
	// naming its file and the sync bound after the write.
	//
	// decision: Bounded is the zero value, so a share whose policy was never
	// set gets the default rather than the weaker policy. Revisit only if the
	// default policy changes.
	Bounded SyncPolicy = iota

	// Standard makes a write durable only by a sync naming its file. A write
	// no sync names is as durable as on a local filesystem, written back by
	// the operating system on its own schedule.
	Standard
)
