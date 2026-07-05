package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockcodec"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// This file is the one-shot cas→blocks migration (#1493 PR4): the ONLY code
// outside the remote.LegacyCASStore implementations that still understands the
// legacy standalone-CAS layout (one sealed object per chunk under "cas/",
// located by a synced marker with an empty BlockID). It runs synchronously
// from Store.Start — blocking, before the share serves — and is resumable and
// idempotent by construction:
//
//   - Phase L imports pre-flip per-chunk local files into the log-blob
//     substrate (FSStore.MigrateLegacyChunkFiles).
//   - Phase R re-packs every chunk whose synced marker still carries a
//     standalone locator (BlockID == "") into packed blocks/<id> objects via
//     the carver's seal+codec path, then atomically commits the block record +
//     locator rewrites in one metadata transaction (DefaultCommitBlock's
//     last-wins overwrite). A crash between PutBlock and the commit leaves at
//     most one orphan block object — the same window the live carver has,
//     reconciled by the PR5 sweep — and never a partially-pointed block
//     record, so re-runs converge without leaks.
//   - Phase P purges whatever is left under cas/: after Phase R nothing in
//     that namespace is referenced (objects either had their locator rewritten
//     or were never marked synced — the pre-flip Put-then-Mark crash orphans).
//
// Detection is state-free (no sentinel, no journal): a metadata scan for
// standalone locators plus one remote LIST page. Re-running against a
// migrated share is a no-op.

// legacyChunkFileMigrator is implemented by the fs-backed local store; the
// memory local store has no per-chunk file layout and skips Phase L.
type legacyChunkFileMigrator interface {
	MigrateLegacyChunkFiles(ctx context.Context) (int, error)
}

// migrateLegacyCAS runs the full migration for this store. Called from Start.
func (bs *Store) migrateLegacyCAS(ctx context.Context) error {
	// Phase L — local per-chunk files → log-blob substrate. Skipped when the
	// local store has no log-blob substrate wired (index-less test fixtures
	// still on the legacy per-chunk writer) — such stores keep their legacy
	// files readable and simply don't migrate.
	if lm, ok := bs.local.(legacyChunkFileMigrator); ok && bs.localHasLogBlobSubstrate() {
		if n, err := lm.MigrateLegacyChunkFiles(ctx); err != nil {
			return fmt.Errorf("cas→blocks migration (local phase): %w", err)
		} else if n > 0 {
			logger.Info("cas→blocks migration: local per-chunk files imported", "chunks", n)
		}
	}
	return bs.syncer.migrateLegacyCASRemote(ctx)
}

// localHasLogBlobSubstrate reports whether the local store can receive
// migrated chunks (log-blob + chunk index wired). Probed via the narrow
// LocalChunkIndex setter contract the engine already uses for wiring.
func (bs *Store) localHasLogBlobSubstrate() bool {
	type prober interface{ HasLogBlobSubstrate() bool }
	if p, ok := bs.local.(prober); ok {
		return p.HasLogBlobSubstrate()
	}
	return false
}

// migrateLegacyCASRemote runs Phases R and P against the share's remote store.
func (m *Syncer) migrateLegacyCASRemote(ctx context.Context) error {
	m.mu.RLock()
	rbs := m.remoteBlockStore
	sealer := m.chunkSealer
	committer := m.blockCommitter
	shs := m.syncedHashStore
	m.mu.RUnlock()

	if m.remoteStore == nil || shs == nil {
		return nil // local-only share (or fixture without sync state): nothing standalone can exist
	}

	// The legacy accessor lives on the raw (unwrapped) remote stack — the same
	// object SetRemoteBlockStore received — because the nonClosingRemote
	// wrapper deliberately proxies only the RemoteStore surface.
	legacy, hasLegacy := rbs.(remote.LegacyCASStore)

	// Phase R — collect every synced hash whose locator is still standalone.
	// Hashes are collected first (32 B each; bounded by the synced set) so no
	// metadata writes happen while the enumeration iterator is open.
	enum, canEnumerate := shs.(SyncedHashIndex)
	var standalone []block.ContentHash
	if canEnumerate {
		// Drain the enumeration into a slice, THEN resolve locators: the sqlite
		// metadata pool is MaxOpenConns(1), so calling GetLocator inside the
		// EnumerateSynced callback (while its cursor still holds the only
		// connection) deadlocks waiting for a second — which blocks Store.Start
		// and, via LoadSharesFromStore, the whole server from coming up.
		var synced []block.ContentHash
		if err := enum.EnumerateSynced(ctx, func(h block.ContentHash, _ time.Time) error {
			synced = append(synced, h)
			return nil
		}); err != nil {
			return fmt.Errorf("cas→blocks migration: enumerate synced hashes: %w", err)
		}
		for _, h := range synced {
			if err := ctx.Err(); err != nil {
				return err
			}
			loc, ok, err := shs.GetLocator(ctx, h)
			if err != nil {
				return err
			}
			if ok && loc.BlockID == "" { // standalone locator: pre-flip layout
				standalone = append(standalone, h)
			}
		}
	}

	if len(standalone) > 0 {
		if rbs == nil || committer == nil {
			return fmt.Errorf("cas→blocks migration: %d standalone chunks found but the block substrate is not wired", len(standalone))
		}
		if err := m.repackStandaloneChunks(ctx, legacy, hasLegacy, sealer, committer, standalone); err != nil {
			return err
		}
	}

	// Phase P — purge the now-unreferenced remainder of the cas/ namespace.
	// When nothing standalone was found and the remote is unreachable this is
	// deliberately non-fatal: the purge is pure cleanup and retries next boot.
	if !hasLegacy {
		return nil
	}
	var purged int
	purgeErr := legacy.WalkLegacyChunks(ctx, func(h block.ContentHash, _ int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := legacy.DeleteLegacyChunk(ctx, h); err != nil {
			return fmt.Errorf("purge cas object %s: %w", h, err)
		}
		purged++
		return nil
	})
	if purgeErr != nil {
		if len(standalone) == 0 {
			logger.Warn("cas→blocks migration: cas/ purge skipped (remote unavailable?) — will retry next start",
				"error", purgeErr)
			return nil
		}
		return fmt.Errorf("cas→blocks migration: purge cas/ namespace: %w", purgeErr)
	}
	if purged > 0 || len(standalone) > 0 {
		logger.Info("cas→blocks migration complete",
			"repacked_chunks", len(standalone), "purged_objects", purged)
	}
	return nil
}

// migrationChunk is one chunk staged for re-packing: its plaintext plus the
// local log-blob location when the bytes are also local-resident (zero
// otherwise — the commit skips the local index entry in that case).
type migrationChunk struct {
	hash  block.ContentHash
	data  []byte
	local block.LocalChunkLocation
}

// repackStandaloneChunks packs the standalone chunks into carve-sized blocks.
// Memory is bounded at ~carveBlockSize of plaintext per batch (plus the sealed
// copy), matching the live carver.
func (m *Syncer) repackStandaloneChunks(
	ctx context.Context,
	legacy remote.LegacyCASStore,
	hasLegacy bool,
	sealer remote.ChunkSealer,
	committer blockCommitter,
	standalone []block.ContentHash,
) error {
	target := m.carveBlockSize()
	var (
		batch      []migrationChunk
		batchBytes int64
		repacked   int
		lost       int
	)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := m.packAndCommitMigrated(ctx, sealer, committer, batch); err != nil {
			return err
		}
		// Locators now point at the new block: the standalone objects are
		// unreferenced. Delete best-effort; Phase P sweeps any failure.
		if hasLegacy {
			for _, c := range batch {
				if err := legacy.DeleteLegacyChunk(ctx, c.hash); err != nil {
					logger.Warn("cas→blocks migration: standalone object delete failed (purge will retry)",
						"hash", c.hash.String(), "error", err)
				}
			}
		}
		repacked += len(batch)
		batch = batch[:0]
		batchBytes = 0
		return nil
	}

	for _, h := range standalone {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, localLoc, err := m.migrationChunkBytes(ctx, legacy, hasLegacy, h)
		if err != nil {
			if errors.Is(err, block.ErrChunkNotFound) {
				// Pre-existing data loss: the marker points at an object that no
				// longer exists anywhere. Reads of this chunk were already failing
				// on the old path. Surface loudly, drop the marker so the
				// post-migration invariant (no standalone locators) holds, and
				// keep going.
				logger.Error("cas→blocks migration: standalone chunk lost (no local copy, no remote object) — dropping synced marker",
					"hash", h.String())
				if derr := committer.DeleteSynced(ctx, h); derr != nil {
					return fmt.Errorf("cas→blocks migration: drop lost marker %s: %w", h, derr)
				}
				lost++
				continue
			}
			return fmt.Errorf("cas→blocks migration: read standalone chunk %s: %w", h, err)
		}

		batch = append(batch, migrationChunk{hash: h, data: data, local: localLoc})
		batchBytes += int64(len(data))
		if batchBytes >= target {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}

	if lost > 0 {
		logger.Error("cas→blocks migration: chunks unrecoverable during migration",
			"lost", lost, "repacked", repacked)
	}
	return nil
}

// migrationChunkBytes returns the plaintext for hash, preferring the local
// tier (no download) and falling back to a BLAKE3-verified legacy remote read.
// The local read is verified here too — migration is the last gate before the
// standalone copy is deleted.
func (m *Syncer) migrationChunkBytes(
	ctx context.Context,
	legacy remote.LegacyCASStore,
	hasLegacy bool,
	h block.ContentHash,
) ([]byte, block.LocalChunkLocation, error) {
	var zero block.LocalChunkLocation
	if has, err := m.local.Has(ctx, h); err == nil && has {
		data, gerr := m.local.Get(ctx, h)
		if gerr == nil {
			if block.ContentHash(blake3.Sum256(data)) == h {
				loc := zero
				if committer := m.getBlockCommitter(); committer != nil {
					if l, ok, lerr := committer.GetLocalLocation(ctx, h); lerr == nil && ok {
						loc = l
					}
				}
				return data, loc, nil
			}
			logger.Warn("cas→blocks migration: local copy failed verification, falling back to remote",
				"hash", h.String())
		}
	}
	if !hasLegacy {
		return nil, zero, fmt.Errorf("chunk %s: %w", h, block.ErrChunkNotFound)
	}
	data, err := legacy.ReadLegacyChunkVerified(ctx, h)
	if err != nil {
		return nil, zero, err
	}
	return data, zero, nil
}

// getBlockCommitter returns the wired block committer under the syncer lock.
func (m *Syncer) getBlockCommitter() blockCommitter {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.blockCommitter
}

// packAndCommitMigrated seals, frames, uploads, and atomically commits one
// batch as a packed block — the migration twin of carveAndCommitBlock, with
// the chunk bytes already in hand instead of read from the log blob.
func (m *Syncer) packAndCommitMigrated(
	ctx context.Context,
	sealer remote.ChunkSealer,
	committer blockCommitter,
	batch []migrationChunk,
) error {
	m.mu.RLock()
	rbs := m.remoteBlockStore
	m.mu.RUnlock()
	if rbs == nil {
		return errors.New("cas→blocks migration: remote block store not wired")
	}

	blockID, err := newBlockID()
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	builder, err := blockcodec.NewBuilder(&buf, blockID, nil)
	if err != nil {
		return fmt.Errorf("migration: new builder: %w", err)
	}

	commits := make([]block.BlockChunkCommit, 0, len(batch))
	for _, c := range batch {
		wire := c.data
		if sealer != nil {
			wire, err = sealer.SealChunk(ctx, c.hash, c.data)
			if err != nil {
				return fmt.Errorf("migration: seal chunk %s: %w", c.hash, err)
			}
		}
		chunkLoc, err := builder.Add(c.hash, wire)
		if err != nil {
			return fmt.Errorf("migration: frame chunk %s: %w", c.hash, err)
		}
		commits = append(commits, block.BlockChunkCommit{
			Hash:   c.hash,
			Remote: chunkLoc,
			Local:  c.local,
		})
	}
	if _, err := builder.Finish(); err != nil {
		return fmt.Errorf("migration: finish block: %w", err)
	}

	blockBytes := buf.Bytes()
	blockHash := block.ContentHash(blake3.Sum256(blockBytes))

	// PutBlock before the commit: a crash in between leaves an orphan block
	// object (PR5 reconcile), never an unbacked record.
	if err := rbs.PutBlock(ctx, blockID, bytes.NewReader(blockBytes)); err != nil {
		return fmt.Errorf("migration: put block %s: %w", blockID, err)
	}

	rec := block.BlockRecord{
		BlockID:        blockID,
		BlockHash:      blockHash,
		Length:         int64(len(blockBytes)),
		LiveChunkCount: uint32(len(commits)),
		SyncState:      block.BlockStateRemote,
	}
	if err := metadata.DefaultCommitBlock(ctx, committer, rec, commits); err != nil {
		return fmt.Errorf("migration: commit block %s: %w", blockID, err)
	}
	return nil
}
