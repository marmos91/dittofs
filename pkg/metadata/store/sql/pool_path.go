// Store-level (pool-backed) half of the shared SQL implementation. Core holds
// the bodies that are one statement on whatever executor it was given, which is
// all a transaction needs. A store needs more: the writes below span several
// statements, so running them on the pool would autocommit each one separately
// and lose the atomicity the callers were promised. They have to open a real
// transaction first, and opening one is the store's job, not Core's.
//
// That is the whole reason these lived per-dialect until now: the delegate is
// identical in both backends but names the concrete store as its receiver, so
// there was nowhere shared to put it. Taking the store as a metadata.Transactor
// removes the last dialect-specific thing about them.
package sql

import (
	"context"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sharecache"
)

// PoolPath carries the store-level half of the shared SQL implementation. A
// store embeds it instead of embedding Core directly: it gets every promoted
// single-statement body through the Core inside, plus the transaction-wrapping
// writes declared here on top.
//
// The shadowing is load-bearing and it is embedding depth that makes it work.
// From the store, PoolPath's methods are one hop away and Core's namesakes are
// two, so the shallower one wins the selector. Dropping one of these methods
// would still build, because Core's promoted method satisfies the interface —
// and that is exactly the hazard. The write would go straight to the pool,
// where a multi-statement write is no longer atomic and a contended failure
// reaches the caller instead of being retried. A transaction embeds Core
// directly and so keeps the single-statement bodies, which is what it wants.
type PoolPath struct {
	*Core

	// T opens the transactions the writes below run in. Never nil on a store's
	// PoolPath. A transaction never has one, because it embeds Core directly
	// rather than going through here — nothing opens a transaction inside a
	// transaction.
	T metadata.Transactor

	// ShareCache fronts the share-options read, which every permission check
	// funnels through. Held by pointer because the cache carries a sync.Map and
	// three atomics: a copy would be both a vet error and its own cache, so an
	// invalidation on one would leave the other serving stale permissions.
	// Never nil on a store's PoolPath.
	ShareCache *sharecache.Cache
}

// ============================================================================
// Files and directories
// ============================================================================

// UpdateAttrs stores or updates file metadata, creating the entry if absent.
func (p PoolPath) UpdateAttrs(ctx context.Context, file *metadata.File) error {
	return p.T.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.UpdateAttrs(ctx, file)
	})
}

// SetManifest stores or updates file metadata and rewrites the stored block
// manifest from file.Blocks.
func (p PoolPath) SetManifest(ctx context.Context, file *metadata.File) error {
	return p.T.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetManifest(ctx, file)
	})
}

// DeleteFile removes file metadata by handle, reporting ErrNotFound when the
// handle does not exist.
func (p PoolPath) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
	return p.T.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteFile(ctx, handle)
	})
}

// SetChild adds or updates a child entry in a directory.
func (p PoolPath) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	return p.T.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetChild(ctx, dirHandle, name, childHandle)
	})
}

// DeleteChild removes a child entry from a directory.
func (p PoolPath) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	return p.T.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteChild(ctx, dirHandle, name)
	})
}

// SetLinkCount sets the hard link count for a file.
func (p PoolPath) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	return p.T.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.SetLinkCount(ctx, handle, count)
	})
}

// PutFilesystemMeta stores filesystem metadata for a share.
func (p PoolPath) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	return p.T.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFilesystemMeta(ctx, shareName, meta)
	})
}

// ============================================================================
// Block records
// ============================================================================

// CommitBlock atomically writes rec and marks each chunk synced within a single
// transaction. Idempotent on BlockID.
func (p PoolPath) CommitBlock(ctx context.Context, rec block.BlockRecord, chunks []block.BlockChunkCommit) error {
	return metadata.DefaultCommitBlock(ctx, p.T, rec, chunks, nil)
}

// DecrLiveChunkCount shadows the promoted Core method so the statement runs
// inside WithTransaction, and so inherits its busy/conflict retry budget.
//
// The other block-record writes can afford to surface a transient conflict:
// their caller retries the whole sweep. This one cannot. Block reclamation
// clears the synced marker before it decrements, so a re-visit resolves the
// block as unsynced and skips the decrement entirely — a decrement lost to a
// transient conflict is lost for good, and the block it belonged to is never
// reclaimed.
func (p PoolPath) DecrLiveChunkCount(ctx context.Context, blockID string, delta uint32) (uint32, error) {
	var remaining uint32
	err := p.T.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		remaining, err = tx.DecrLiveChunkCount(ctx, blockID, delta)
		return err
	})
	return remaining, err
}

// ============================================================================
// Synced hashes
// ============================================================================

// PutSyncedLocators shadows the promoted Core method so the batch runs inside
// one transaction. On the pool each statement would autocommit separately, and
// a failure part-way would leave some of the commit's hashes marked synced with
// the rest unmarked — a synced set that claims chunks the remote store never
// received.
//
// No caller needs this on a store today: PutSyncedLocators belongs to
// metadata.Transaction and every call site already holds a transaction. It is
// declared anyway because promotion makes the Core version reachable here
// whether or not anything calls it, and the reachable version should be the
// safe one.
func (p PoolPath) PutSyncedLocators(ctx context.Context, chunks []block.BlockChunkCommit) error {
	return p.T.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutSyncedLocators(ctx, chunks)
	})
}

// ============================================================================
// Shares
// ============================================================================

// GetShareOptions shadows the promoted Core method to put the share cache in
// front of it: every permission check funnels through this read, so the SELECT
// and the decode are worth skipping. The returned value is always a deep copy,
// so a caller can never reach the shared cache entry.
func (p PoolPath) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	// Ahead of the cache lookup, not just inside the backing read: a hit must
	// not report success for a request whose context has already given out.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if cached, ok := p.ShareCache.Get(shareName); ok {
		return sharecache.Clone(cached), nil
	}

	// Snapshot the invalidation generation BEFORE the backing read so a write
	// that races this read cannot leave a stale value cached (Store checks it).
	gen := p.ShareCache.Generation()

	options, err := p.Core.GetShareOptions(ctx, shareName)
	if err != nil {
		return nil, err
	}

	p.ShareCache.Store(shareName, options, gen)
	return sharecache.Clone(options), nil
}

// UpdateShareOptions shadows the promoted Core method to invalidate the share
// cache around it. The write itself is a single statement, so it does not need
// a transaction of its own.
func (p PoolPath) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	err := p.Core.UpdateShareOptions(ctx, shareName, options)
	// Drop the cached options AFTER the write lands, whatever it reported: an
	// extra invalidation costs a re-read, a missed one is a stale permission.
	p.ShareCache.InvalidateAll()
	return err
}

// DeleteShare removes a share and all its metadata. It runs inside a
// transaction so the share row and its inode rows are dropped atomically; see
// the Core method for the cascade rationale.
func (p PoolPath) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.T.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteShare(ctx, shareName)
	})
}

// CreateRootDirectory creates the root directory for a share.
//
// This runs the shared body through a transaction rather than on the pool: the
// probe and the create must not have a commit between them, or a concurrent
// caller slips in and leaves an orphaned root inode behind. Going through the
// transaction method is also what marks the share cache dirty.
func (p PoolPath) CreateRootDirectory(ctx context.Context, shareName string, attr *metadata.FileAttr) (*metadata.File, error) {
	var root *metadata.File
	err := p.T.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var txErr error
		root, txErr = tx.CreateRootDirectory(ctx, shareName, attr)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	return root, nil
}
