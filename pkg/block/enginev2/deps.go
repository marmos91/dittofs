package enginev2

import (
	"context"
	"iter"
	"log/slog"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/openstate"
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
	Opens     Opens
	// Closed is open state's Sink.Closed: what a clone's CloneClose leaves,
	// an orphan whose release only the filesystem service can run.
	Closed func(f entity.FileID, r openstate.CloseResult)
	Clock  Clock
	Logger *slog.Logger
	Tracer Tracer
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

// Opens is open state's hold on a clone's source: CloneOpen takes over the
// source record the clone's first phase wrote, and CloneClose ends it once
// the clone is done.
type Opens interface {
	CloneOpen(ctx context.Context, src openstate.FileKey, c openstate.CloneSource) (entity.OpenID, error)
	CloneClose(ctx context.Context, id entity.OpenID) (openstate.CloseResult, error)
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
