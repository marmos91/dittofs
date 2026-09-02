package sql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
)

// Core is the shared half of a SQL-backed metadata store: an executor to run
// statements on and the dialect that supplies their text and classifies their
// errors.
//
// Both a store and a transaction embed one. The store's carries a pool
// executor, the transaction's an executor over the open transaction — which is
// the whole point: one body serves both, and a transaction-level call cannot
// silently escape to the pool and outlive a rollback.
//
// Embed it by pointer and anonymously, so its methods are promoted onto the
// dialect type. That also keeps the durability asymmetry a compile-time fact:
// a dialect that must not advertise an optional interface simply never declares
// the method, and no shared type can hand it one by accident.
type Core struct {
	// X runs the statements. Never nil.
	X Executor
	// D supplies statement text and classifies driver errors. Never nil.
	D Dialect
	// Caps reports the store's currently configured filesystem capabilities.
	// It is a function rather than a value because SetFilesystemCapabilities
	// replaces them at runtime, and a copy taken at construction would go
	// stale. Never nil.
	Caps func() metadata.FilesystemCapabilities
	// Quota accumulates the usage changes a write owes the store's quota
	// cache, which applies them once the transaction commits. Set only on a
	// transaction's Core; the pool's Core leaves it nil, and the one method
	// that reads it is reached solely through a transaction-level shadow.
	//
	// Left unguarded on purpose. A guard would have to invent an error for a
	// state the package cannot produce, and nothing could ever exercise it, so
	// it would sit here accruing confidence without having refused anything.
	// The bare dereference is the louder failure and it is a safe one:
	// DeleteShare aggregates the usage before it deletes any row, and a panic
	// inside a transaction unwinds past the commit, so nothing reaches the
	// database either way.
	Quota *basestore.QuotaDelta
}

// GetFileChunk reads one chunk by id, reporting metadata.ErrFileChunkNotFound
// when absent.
func (c *Core) GetFileChunk(ctx context.Context, id string) (*metadata.FileChunk, error) {
	chunk, err := ScanFileChunk(c.X.QueryRow(ctx, c.D.Chunks().SelectByID, id))
	if c.D.IsNoRows(err) {
		return nil, metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get file chunk: %w", err)
	}
	return chunk, nil
}

// GetByHash looks up a finalized chunk by content hash, returning (nil, nil)
// when absent.
//
// Dedup matches only Remote chunks: Pending or Syncing rows have not been
// confirmed on the remote and are unsafe dedup targets. That scoping lives in
// the dialect's SelectByHash statement.
func (c *Core) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileChunk, error) {
	chunk, err := ScanFileChunk(c.X.QueryRow(ctx, c.D.Chunks().SelectByHash, hash.String()))
	if c.D.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find file chunk by hash: %w", err)
	}
	return chunk, nil
}

// Put stores or updates a chunk row.
//
// The hash column is persisted whenever the chunk carries a non-zero content
// hash, regardless of state. The hash is derived at rollup time — long before
// the block reaches the remote — and is the key the engine's CAS read path uses
// to resolve a chunk. Gating the write on IsRemote() left every Pending row with
// a NULL hash; reads then survived only while the bytes stayed in the local
// append log or RAM cache, and broke the moment local state went cold (restart
// plus cache eviction, or a snapshot restore's ResetLocalState). The memory and
// badger backends store the hash inline on the row, so this matches them.
//
// LastSyncAttemptAt is persisted as NULL when zero so the janitor's
// last_sync_attempt_at < cutoff predicate excludes never-claimed rows naturally
// instead of matching every Pending row.
func (c *Core) Put(ctx context.Context, chunk *metadata.FileChunk) error {
	var hash *string
	if !chunk.Hash.IsZero() {
		h := chunk.Hash.String()
		hash = &h
	}
	var lastSyncAttemptAt *time.Time
	if !chunk.LastSyncAttemptAt.IsZero() {
		t := chunk.LastSyncAttemptAt
		lastSyncAttemptAt = &t
	}
	if _, err := c.X.Exec(ctx, c.D.Chunks().Upsert,
		chunk.ID, hash, chunk.DataSize, chunk.StartOffset,
		chunk.RefCount, chunk.LastAccess, chunk.CreatedAt, chunk.State, lastSyncAttemptAt); err != nil {
		return fmt.Errorf("put file chunk: %w", err)
	}
	return nil
}

// Delete removes one chunk row, reporting metadata.ErrFileChunkNotFound when
// there was nothing to remove.
func (c *Core) Delete(ctx context.Context, id string) error {
	result, err := c.X.Exec(ctx, c.D.Chunks().Delete, id)
	if err != nil {
		return fmt.Errorf("delete file chunk: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrFileChunkNotFound
	}
	return nil
}

// IncrementRefCount atomically bumps one chunk's RefCount.
func (c *Core) IncrementRefCount(ctx context.Context, id string) error {
	result, err := c.X.Exec(ctx, c.D.Chunks().IncrementRef, id)
	if err != nil {
		return fmt.Errorf("increment ref count: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrFileChunkNotFound
	}
	return nil
}

// DecrementRefCount atomically decrements one chunk's RefCount, floored at zero,
// and returns the new value.
func (c *Core) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	var newCount uint32
	err := c.X.QueryRow(ctx, c.D.Chunks().DecrementRef, id).Scan(&newCount)
	if c.D.IsNoRows(err) {
		return 0, metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("decrement ref count: %w", err)
	}
	return newCount, nil
}

// DecrementRefCountAndReap decrements ref_count and, when it reaches zero,
// deletes the row. The `ref_count = 0` predicate on the DELETE means a bump that
// landed between the two statements leaves the row alive. Returns (0, nil) for
// an already-swept row rather than an error.
//
// The caller must supply a transaction executor: the two statements are only
// atomic against a concurrent AddRef if they share one transaction.
func (c *Core) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	var newCount uint32
	err := c.X.QueryRow(ctx, c.D.Chunks().DecrementRef, id).Scan(&newCount)
	if c.D.IsNoRows(err) {
		return 0, nil // tolerate an already-swept row
	}
	if err != nil {
		return 0, fmt.Errorf("decrement ref count: %w", err)
	}
	if newCount == 0 {
		if _, err := c.X.Exec(ctx, c.D.Chunks().ReapZeroRef, id); err != nil {
			return 0, fmt.Errorf("reap zero-ref block: %w", err)
		}
	}
	return newCount, nil
}

// AddRef bumps RefCount on the row(s) indexed by a content hash, implementing
// the FileChunkStore.AddRef contract the in-memory dedup LRU hit path uses to
// reference an already-stored chunk without creating a row.
//
// A single UPDATE performs the bump, so row-level locking serializes contended
// updates against the same row and this is TOCTOU-free against a concurrent
// decrement cascade. The statement matches only Remote rows: a Pending row
// (which now also carries its hash) is not a valid dedup donor, so this must
// miss it and return metadata.ErrUnknownHash, letting the caller fall back to
// the full Put path.
//
// The hash index is non-unique, so one hash may match several rows in legacy
// data. The statement deliberately has no LIMIT — all matching rows are bumped
// uniformly, so accounting stays correct regardless of which row a later
// decrement targets. Only ref_count is touched; chunk state never is.
func (c *Core) AddRef(ctx context.Context, hash block.ContentHash) error {
	result, err := c.X.Exec(ctx, c.D.Chunks().AddRef, hash.String())
	if err != nil {
		return fmt.Errorf("add ref: %w", err)
	}
	if result.RowsAffected() == 0 {
		return metadata.ErrUnknownHash
	}
	return nil
}

// ListFileChunks returns the chunks belonging to one payload, in block order.
// The id range is a prefilter under byte ordering; block.ChunksForPayload
// decides membership and final order.
func (c *Core) ListFileChunks(ctx context.Context, payloadID string) ([]*metadata.FileChunk, error) {
	lo, hi := block.PayloadPrefixRange(payloadID)
	rows, err := c.X.Query(ctx, c.D.Chunks().ListByPayloadRange, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("list file chunks: %w", err)
	}
	defer rows.Close()
	result, err := ScanFileChunkRows(rows)
	if err != nil {
		return nil, err
	}
	return block.ChunksForPayload(result, payloadID), nil
}

// EnumerateFileChunks streams every live-set ContentHash through fn.
//
// A malformed hash aborts enumeration rather than being coerced to the zero
// hash: coercing would invite the GC mark phase to read the row as a legacy
// pre-CAS entry, and the sweep would reap a still-live CAS object once the grace
// TTL lapsed. Failing closed here means the sweep is skipped instead.
func (c *Core) EnumerateFileChunks(ctx context.Context, fn func(block.ContentHash) error) error {
	rows, err := c.X.Query(ctx, c.D.Chunks().EnumerateHashes)
	if err != nil {
		return fmt.Errorf("enumerate file chunks: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate file chunks: %w", err)
		}
		var hash sql.NullString
		if err := rows.Scan(&hash); err != nil {
			return fmt.Errorf("enumerate file chunks: scan: %w", err)
		}
		var h block.ContentHash
		if hash.Valid {
			parsed, perr := metadata.ParseContentHash(hash.String)
			if perr != nil {
				return fmt.Errorf("enumerate file chunks: parse hash %q: %w", hash.String, perr)
			}
			h = parsed
		}
		if err := fn(h); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("enumerate file chunks: rows: %w", err)
	}
	return nil
}
