package engine

import (
	"context"
	"iter"
	"log/slog"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// Deps is what the composition root hands New. Each interface holds exactly
// the methods the engine calls on that component, and the root passes the
// component's own type as it. Every member is required: New refuses a missing
// one by name, so a fixture supplies stubs of the whole interface, never nil.
type Deps struct {
	// Node is this node's identity, from its bootstrap; JournalID looks up its
	// devices under it.
	Node      entity.NodeID
	Devices   []string // one journal directory per device; Start opens each
	Open      func(ctx context.Context, dir string, floor journal.Version, expect journal.JournalID) (Journal, error)
	Existence Existence
	Content   Content
	Lifecycle Lifecycle
	Holds     CloneHold // a clone's hold on its source; the root adapts open state to it
	Clock     Clock
	Logger    *slog.Logger
	Tracer    Tracer
}

// Journal is one device's journal, as the engine calls it.
type Journal interface {
	Share(tag journal.ShareTag, limit int64, floor journal.Version) (Handle, error)
	Forget(ctx context.Context, tag journal.ShareTag) error
	Strand(tag journal.ShareTag) error
	Stats() journal.Stats
	// SyncMarks is the mark of each append stream's last sync, the journal's
	// Stats.SyncMarks, each naming its stream.
	SyncMarks() []entity.SyncMark
	// MarksRecorded hands the journal the marks a committed metadata
	// transaction recorded.
	MarksRecorded(marks []entity.SyncMark)
	Draw() (journal.Version, error)
	SetVersionLimit(mark journal.Version) error
	Repack(ctx context.Context, budget int64, scope journal.RepackScope) (freed int64, err error)
	Close() error
}

// Handle is one share's handle on a journal, as the engine calls it.
type Handle interface {
	WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, mtime int64) (journal.Version, error)
	ReadAt(ctx context.Context, id journal.FileID, off int64, p []byte) (n int, missing []journal.Extent, held []journal.Held, asOf journal.Version, err error)
	// Sync returns, per append stream holding a named record outside a
	// sealed segment, the mark covering the newest of them.
	Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error)
	Truncate(ctx context.Context, id journal.FileID, size int64) (journal.Version, error)
	Deallocate(ctx context.Context, id journal.FileID, e journal.Extent) (journal.Version, error)
	Delete(ctx context.Context, id journal.FileID) (journal.Version, error)
	CloneTarget(ctx context.Context, id journal.FileID, spec journal.CloneSpec) (journal.Version, error)
	Reserve(n int64, writes int) (Reservation, error)
	Since(ctx context.Context, id journal.FileID, from journal.Version) iter.Seq2[journal.Change, error]
	Settle(id journal.FileID, v journal.Version)
	Files() iter.Seq[journal.FileID]
	Close() error
}

// Reservation is what Reserve took for a clone's copies.
type Reservation interface {
	WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, mtime int64) (journal.Version, error)
	Close() error
}

// Existence is block metadata's existence view, as the engine calls it.
type Existence interface {
	Commit(ctx context.Context, pending []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error)
	Overwrites(ctx context.Context, share entity.ShareID, file entity.FileID, runs []entity.Extent) (bool, error)
	Applied(ctx context.Context, share entity.ShareID, file entity.FileID) (entity.JournalVersion, error)
	Size(ctx context.Context, share entity.ShareID, file entity.FileID) (int64, error)
	Allocation(ctx context.Context, share entity.ShareID, file entity.FileID, off int64) (run entity.Extent, hole bool, err error)
	Remove(ctx context.Context, share entity.ShareID, file entity.FileID, epoch uint64, op kvstore.RemoveOp, pending []kvstore.PendingOp) error
	Clone(ctx context.Context, op kvstore.CloneOp, pending []kvstore.PendingOp) error
}

// Content is block metadata's content view, as the engine calls it.
type Content interface {
	Covering(ctx context.Context, share entity.ShareID, file entity.FileID, off, n int64) iter.Seq2[entity.Span, error]
	Resume(ctx context.Context, share entity.ShareID, file entity.FileID) error
	PruneRemovals(ctx context.Context, share entity.ShareID, file entity.FileID, atOrBelow entity.JournalVersion) error
	UnfinishedRemovals(ctx context.Context, share entity.ShareID, shard uint64) iter.Seq2[kvstore.UnfinishedRemoval, error]
	VersionFloor(ctx context.Context, shares []entity.ShareID) (entity.JournalVersion, error)
}

// Lifecycle is the store's access outside block metadata's views: this
// node's devices' journal identities, the installation's tags, the shares'
// version limits and the shards' incarnations.
//
// decision: a device is named by its journal directory as the bootstrap
// writes it, one of Deps.Devices, looked up under Deps.Node, and the identity
// is the journal's sixteen bytes unnamed, so the store implements this
// without importing the journal. Revisit if the store gains a device
// identifier of its own.
type Lifecycle interface {
	JournalID(ctx context.Context, node entity.NodeID, device entity.JournalDir) ([16]byte, error)
	Tags(ctx context.Context) (map[entity.ShareID]entity.TagRecord, error)
	VersionLimits(ctx context.Context, shares []entity.ShareID) (map[entity.ShareID]entity.JournalVersion, error)
	RaiseVersionLimits(ctx context.Context, to map[entity.ShareID]entity.JournalVersion) error
	RaiseAtStart(ctx context.Context, shards []entity.ShardID) (map[entity.ShardID]uint64, error)
	RaiseAtAttach(ctx context.Context, shard entity.ShardID) (uint64, error)
}

// CloneHold is the part of open state a clone calls, in the engine's own
// types, so the engine imports no open state; the root adapts the open-state
// table to it, its errors included: a shard not yet served here or a table
// not running answers ErrDelay with DelayFrozen, which holds the clone for a
// retry, and Release of a hold already gone answers nil.
type CloneHold interface {
	// Hold takes over the source open the clone's first phase wrote, before
	// its second phase runs: open state's CloneOpen.
	Hold(ctx context.Context, src CloneSrc) (entity.OpenID, error)
	// Release ends the hold once the clone is done: open state's CloneClose.
	// An orphan it leaves reaches the filesystem through open state's own
	// Sink.Closed, not through the engine.
	Release(ctx context.Context, o entity.OpenID) error
	// Served returns once open state serves shard s, or at ctx's end: a
	// clone rebuilt at start waits on it before its batches run.
	Served(ctx context.Context, s entity.ShardID) error
}

// CloneSrc names a clone's source open.
type CloneSrc struct {
	Share entity.ShareID
	Shard entity.ShardID
	File  entity.FileID
	Dst   entity.FileID         // the destination, whose removal record holds the clone spec
	AsOf  entity.JournalVersion // the version the source was read at
}

// Clock is the node's one source of time, as the engine reads it.
type Clock interface {
	Now() time.Time
	Elapsed() time.Duration
	After(d time.Duration) <-chan struct{}
}

// Tracer starts a span named op, with attrs, as a child of the span ctx
// carries, and returns the context carrying it with the function that ends it
// with the operation's error.
type Tracer func(ctx context.Context, op string, attrs ...slog.Attr) (context.Context, func(err error))
