package enginev2

import (
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
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
	ID         entity.ShareID
	Tag        journalv2.ShareTag   // the share's tag in every journal
	Shard      entity.ShardID       // the share's own shard, which keys a clone's source open
	Limit      int64                // the share's journal limit, in bytes
	SyncPolicy journalv2.SyncPolicy // the share's sync.policy
	State      ShareState
}

// ShareState is what a bound share admits.
type ShareState uint8

const (
	Serving  ShareState = iota + 1 // reads and writes
	Quiesced                       // nothing new admitted
)

// Stats is the engine's counters and conditions, per share and journal.
type Stats struct{}
