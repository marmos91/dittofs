package engine

import (
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Settings are the engine's node-level settings, handed to Apply. Every one
// must be positive.
type Settings struct {
	ExistenceAge        time.Duration // existence_age: how long a write's existence may stay uncommitted
	GroupCommitMaxFiles int           // group_commit.max_files: the most files one group commit batch holds
	CloneMaxLen         int64         // clone_max_len: the longest extent one Clone takes, in bytes
}

// Share is one share's storage binding, as Start and Bind take it.
type Share struct {
	ID        entity.ShareID
	Tag       journal.ShareTag   // the share's tag in every journal
	Shard     entity.ShardID     // the share's own shard, which keys a clone's source open
	Namespace entity.NamespaceID // the share's write namespace: a clone's two shares must share it
	Limit     int64              // the share's journal limit, in bytes
	State     ShareState
}

// ShareState is what a bound share admits.
type ShareState uint8

const (
	Serving  ShareState = iota + 1 // reads and writes
	Quiesced                       // nothing new admitted
)

// Stats is the engine's counters and conditions, per journal and per share.
// Every map belongs to the caller.
type Stats struct {
	Journals map[string]JournalStats // per device, as Deps.Devices names it
	Shares   map[entity.ShareID]ShareStats
}

// WriteOpts is what a write carries besides its bytes.
type WriteOpts struct {
	Suspend entity.TimeMask // the times this write leaves alone
	// Done is called exactly once for every write Write accepted: by the
	// existence commit that records its usage or the commit recording its
	// loss, or by the engine, charging nothing, when it drops the write
	// uncommitted. It is never called if Write fails.
	Done func()
}

// Overlay is a file's size, times and version as its uncommitted operations
// leave them, replayed in version order: a truncate sets Size and SizeSet, and
// each later write raises Size to its end.
type Overlay struct {
	Size         int64 // SizeSet: the size; else the end of the pending writes
	SizeSet      bool  // a truncate is pending, so Size is exact
	Mtime, Ctime time.Time
	Version      entity.JournalVersion
}
