package enginev2

import (
	"context"
	"iter"
	"log/slog"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// Deps is what the composition root hands New. Each interface holds exactly
// the methods the engine calls on that component, and the root passes the
// component's own type as it. Every member is required: New refuses a missing
// one by name, so a fixture supplies stubs of the whole interface, never nil.
type Deps struct {
	Devices   []string // one journal directory per device; Start opens each
	Open      func(ctx context.Context, dir string, floor journalv2.Version, expect journalv2.JournalID) (Journal, error)
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
	Share(tag journalv2.ShareTag, limit int64, floor journalv2.Version, policy journalv2.SyncPolicy) (Handle, error)
	Forget(ctx context.Context, tag journalv2.ShareTag) error
	Strand(tag journalv2.ShareTag) error
	Stats() journalv2.Stats
	Draw() (journalv2.Version, error)
	SetVersionLimit(mark journalv2.Version) error
	Repack(ctx context.Context, budget int64, scope journalv2.RepackScope) (freed int64, err error)
	Close() error
}

// Handle is one share's handle on a journal, as the engine calls it.
type Handle interface {
	WriteAt(ctx context.Context, id journalv2.FileID, off int64, p []byte, mtime int64) (journalv2.Version, error)
	ReadAt(ctx context.Context, id journalv2.FileID, off int64, p []byte) (n int, missing []journalv2.Extent, held []journalv2.Held, asOf journalv2.Version, err error)
	Sync(ctx context.Context, ids ...journalv2.FileID) error
	Truncate(ctx context.Context, id journalv2.FileID, size int64) (journalv2.Version, error)
	Deallocate(ctx context.Context, id journalv2.FileID, e journalv2.Extent) (journalv2.Version, error)
	Delete(ctx context.Context, id journalv2.FileID) (journalv2.Version, error)
	CloneTarget(ctx context.Context, id journalv2.FileID, spec journalv2.CloneSpec) (journalv2.Version, error)
	Reserve(n int64, writes int) (Reservation, error)
	Since(ctx context.Context, id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error]
	Settle(id journalv2.FileID, v journalv2.Version)
	Files() iter.Seq[journalv2.FileID]
	Close() error
}

// Reservation is what Reserve took for a clone's copies.
type Reservation interface {
	WriteAt(ctx context.Context, id journalv2.FileID, off int64, p []byte, mtime int64) (journalv2.Version, error)
	Close() error
}

// Existence is block metadata's existence view, as the engine calls it.
type Existence interface {
	Commit(ctx context.Context, pending []kvstore.PendingExistence) (map[entity.FileID]error, error)
	Overwrites(ctx context.Context, share entity.ShareID, file entity.FileID, runs []entity.Extent) (bool, error)
	Applied(ctx context.Context, share entity.ShareID, file entity.FileID) (entity.JournalVersion, error)
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

// Lifecycle is the store's access outside block metadata's views: a device's
// journal identity, the shares' version limits and the shards' incarnations.
//
// decision: a device is named by its journal directory, the string Deps.Devices
// holds, and the identity is the journal's sixteen bytes unnamed, so the store
// implements this without importing the journal. Revisit if the store gains a
// device identifier of its own.
type Lifecycle interface {
	JournalID(ctx context.Context, device string) ([16]byte, error)
	VersionLimits(ctx context.Context, shares []entity.ShareID) (map[entity.ShareID]entity.JournalVersion, error)
	RaiseVersionLimits(ctx context.Context, to map[entity.ShareID]entity.JournalVersion) error
	RaiseIncarnations(ctx context.Context, shards []entity.ShardID) error
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
