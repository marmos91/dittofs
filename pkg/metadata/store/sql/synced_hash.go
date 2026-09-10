package sql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// synced_hashes records, per CAS content hash, that the chunk has been mirrored
// to the remote store at least once and where its bytes ended up: NULL block
// columns for a standalone object, a (block_id, block_offset, block_length)
// triple for a chunk packed inside a block object. Presence of the row is the
// marker; absence means local-only.
//
// Every statement here is spelled once: the columns, the predicates and the
// conflict clause are identical in both dialects, and only the placeholders and
// the clock expression that stamps synced_at differ, which Dialect.Placeholder
// and Dialect.Now supply.

// syncedLocatorCols is the locator column list every read names and every write
// supplies, in the order the scans below read them. Naming it once is what
// keeps a column added to one statement from being missed by the others.
const syncedLocatorCols = `block_id, block_offset, block_length`

// syncedUpsertConflict is the last-wins tail PutSyncedLocators appends to the
// multi-row INSERT it generates. Every column the table carries besides the key
// is restated, so an upsert leaves exactly the row a DELETE followed by an
// INSERT would.
const syncedUpsertConflict = ` ON CONFLICT (hash) DO UPDATE SET` +
	` synced_at = excluded.synced_at, block_id = excluded.block_id,` +
	` block_offset = excluded.block_offset, block_length = excluded.block_length`

// EnumerateSynced streams every synced marker with its locator and first-mirror
// time. The locator columns live in the same row, so yielding them here lets a
// caller resolve locators in a single scan instead of a GetLocator round trip
// per hash — which on a pool limited to one connection costs one serial
// statement per marker. A row with NULL/empty block columns yields the zero
// (standalone) locator.
func (c *Core) EnumerateSynced(ctx context.Context, fn func(hash block.ContentHash, loc block.ChunkLocator, syncedAt time.Time) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rows, err := c.X.Query(ctx, `SELECT hash, synced_at, `+syncedLocatorCols+` FROM synced_hashes`)
	if err != nil {
		return fmt.Errorf("synced enumerate: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			raw      []byte
			syncedAt time.Time
			blockID  sql.NullString
			off, ln  sql.NullInt64
		)
		if err := rows.Scan(&raw, &syncedAt, &blockID, &off, &ln); err != nil {
			return fmt.Errorf("synced enumerate scan: %w", err)
		}
		if len(raw) != len(block.ContentHash{}) {
			// A malformed hash row cannot be reduced to a ContentHash. Skip it
			// rather than corrupt the sweep's candidate set.
			continue
		}
		var h block.ContentHash
		copy(h[:], raw)
		loc, err := locatorFromCols(blockID, off, ln)
		if err != nil {
			return fmt.Errorf("synced enumerate: %w", err)
		}
		if err := fn(h, loc, syncedAt); err != nil {
			return err
		}
	}
	return rows.Err()
}

// IsSynced reports whether hash has been MarkSynced'd at least once. An absent
// row is "not yet synced", not an error.
func (c *Core) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var present int
	err := c.X.QueryRow(ctx,
		`SELECT 1 FROM synced_hashes WHERE hash = `+c.D.Placeholder(1), hash[:],
	).Scan(&present)
	if c.D.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("synced get: %w", err)
	}
	return true, nil
}

// MarkSynced records that hash has been mirrored to remote, persisting loc's
// block columns atomically with the marker. Idempotent and first-wins:
// re-applying the same hash is a no-op that preserves the first locator. A
// standalone locator (BlockID == "") leaves the block columns NULL, identical
// to a pre-locator row, so existing data needs no migration.
func (c *Core) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	blockID, off, length := locatorArgs(loc)
	// synced_at is stamped by the statement's own clock, matching the rows
	// PutSyncedLocators writes.
	mark := `INSERT INTO synced_hashes (hash, synced_at, ` + syncedLocatorCols + `)` +
		` VALUES (` + c.D.Placeholder(1) + `, ` + c.D.Now() + `, ` +
		c.D.Placeholder(2) + `, ` + c.D.Placeholder(3) + `, ` + c.D.Placeholder(4) + `)` +
		` ON CONFLICT (hash) DO NOTHING`
	if _, err := c.X.Exec(ctx, mark, hash[:], blockID, off, length); err != nil {
		return fmt.Errorf("synced mark: %w", err)
	}
	return nil
}

// GetLocator returns the recorded remote locator for hash: (zero, false, nil)
// when no row exists; a synced row with NULL/empty block columns yields the
// zero (standalone) locator with found == true.
func (c *Core) GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.ChunkLocator{}, false, err
	}
	var blockID sql.NullString
	var off, length sql.NullInt64
	err := c.X.QueryRow(ctx,
		`SELECT `+syncedLocatorCols+` FROM synced_hashes WHERE hash = `+c.D.Placeholder(1), hash[:],
	).Scan(&blockID, &off, &length)
	if c.D.IsNoRows(err) {
		return block.ChunkLocator{}, false, nil
	}
	if err != nil {
		return block.ChunkLocator{}, false, fmt.Errorf("synced get locator: %w", err)
	}
	loc, err := locatorFromCols(blockID, off, length)
	if err != nil {
		return block.ChunkLocator{}, false, fmt.Errorf("synced get locator: %w", err)
	}
	return loc, true, nil
}

// DeleteSynced removes the synced marker for hash. Idempotent: zero rows
// affected is not an error.
func (c *Core) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := c.X.Exec(ctx,
		`DELETE FROM synced_hashes WHERE hash = `+c.D.Placeholder(1), hash[:],
	); err != nil {
		return fmt.Errorf("synced delete: %w", err)
	}
	return nil
}

// PutSyncedLocators writes the marker and locator of every chunk, overwriting
// whatever marker each hash already carries. It is the batched, last-wins form
// of DeleteSynced-then-MarkSynced per chunk and leaves the same rows, in one
// generated INSERT per batch rather than two statements per chunk — a block
// object packs hundreds of chunks into a single commit.
//
// It takes its executor rather than riding on Core, because a commit large
// enough to span more than one batch runs several statements: on the
// pool-backed store those would autocommit independently, and a crash between
// them would leave some of the block's chunks pointing at the new object and
// the rest at the old. Passing the executor keeps the caller's transaction
// visible at the call site.
//
// An empty slice is a no-op.
func PutSyncedLocators(ctx context.Context, x Executor, d Dialect, chunks []block.BlockChunkCommit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	chunks = lastPerHash(chunks)

	// synced_at is the dialect's clock expression rather than a bound value,
	// so a row costs four parameters, not five. The rows per batch are DERIVED
	// from that count rather than fixed, so a column added to the statement
	// shrinks the batch instead of pushing it past maxBoundParams, where the
	// whole Exec would fail.
	const colsPerRow = 4
	const rowsPerBatch = maxBoundParams / colsPerRow
	now := d.Now()

	for start := 0; start < len(chunks); start += rowsPerBatch {
		batch := chunks[start:min(start+rowsPerBatch, len(chunks))]
		var sb strings.Builder
		sb.WriteString(`INSERT INTO synced_hashes (hash, synced_at, ` + syncedLocatorCols + `) VALUES `)
		args := make([]any, 0, len(batch)*colsPerRow)
		for i, c := range batch {
			if i > 0 {
				sb.WriteByte(',')
			}
			// The row's bound columns are the hash and the three locator
			// values; synced_at sits between them as the clock expression.
			base := len(args)
			sb.WriteString(`(` + d.Placeholder(base+1) + `,` + now)
			for col := 1; col < colsPerRow; col++ {
				sb.WriteString(`,` + d.Placeholder(base+col+1))
			}
			sb.WriteByte(')')
			blockID, off, length := locatorArgs(c.Remote)
			args = append(args, c.Hash[:], blockID, off, length)
		}
		sb.WriteString(syncedUpsertConflict)
		if _, err := x.Exec(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("synced put locators: %w", err)
		}
	}
	return nil
}

// lastPerHash drops all but the final occurrence of each hash, keeping the
// survivors in their original order, and returns chunks untouched when there is
// nothing to drop.
//
// A repeated hash has to go before the rows reach one statement: postgres
// rejects an ON CONFLICT DO UPDATE that would touch the same row twice in a
// single INSERT, while sqlite quietly applies both. Keeping the last occurrence
// is what a per-chunk sequential form would have left behind.
func lastPerHash(chunks []block.BlockChunkCommit) []block.BlockChunkCommit {
	last := make(map[block.ContentHash]int, len(chunks))
	for i, c := range chunks {
		last[c.Hash] = i
	}
	if len(last) == len(chunks) {
		return chunks
	}
	out := make([]block.BlockChunkCommit, 0, len(last))
	for i, c := range chunks {
		if last[c.Hash] == i {
			out = append(out, c)
		}
	}
	return out
}

// locatorArgs maps a ChunkLocator onto the (block_id, block_offset,
// block_length) write args: NULL for a standalone chunk, so its row is
// identical to a pre-locator row, and the recorded values for a block-resident
// one.
func locatorArgs(loc block.ChunkLocator) (blockID, off, length any) {
	if loc.IsStandalone() {
		return nil, nil, nil
	}
	return loc.BlockID, loc.WireOffset, loc.WireLength
}

// locatorFromCols builds a ChunkLocator from already-scanned (block_id,
// block_offset, block_length) columns. A NULL or empty block_id yields the zero
// (standalone) locator; a block_id with either companion column missing is a
// row no writer here can produce, and reporting it beats resolving the chunk to
// the wrong bytes.
func locatorFromCols(blockID sql.NullString, off, length sql.NullInt64) (block.ChunkLocator, error) {
	if !blockID.Valid || blockID.String == "" {
		return block.ChunkLocator{}, nil
	}
	if !off.Valid || !length.Valid {
		return block.ChunkLocator{}, fmt.Errorf("corrupt locator row: block_id %q with NULL offset/length", blockID.String)
	}
	return block.ChunkLocator{BlockID: blockID.String, WireOffset: off.Int64, WireLength: length.Int64}, nil
}

// PutSyncedLocators writes the marker and locator of every chunk, last-wins per
// hash, over the executor this Core holds.
//
// Reached through a transaction's Core the statements already share the
// caller's transaction, which is where this belongs and how every caller uses
// it: PutSyncedLocators is part of metadata.Transaction, not of
// metadata.SyncedHashStore. A store's Core runs on the pool, where the several
// statements would autocommit one at a time and a failure part-way would leave
// some hashes marked synced and the rest not — so PoolPath overrides this to
// open a transaction first rather than leaving the unsafe version reachable.
func (c *Core) PutSyncedLocators(ctx context.Context, chunks []block.BlockChunkCommit) error {
	return PutSyncedLocators(ctx, c.X, c.D, chunks)
}
