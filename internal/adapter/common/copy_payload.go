package common

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// CopyPayload performs an O(1) file-level copy: bumps RefCount on every
// unique src hash and persists dstFileAttr.Blocks atomically in one
// metadata transaction. Cache invalidation (if cache != nil) runs
// POST-txn, after the commit succeeded.
//
// Transaction ownership:
//   - This helper opens the metadata txn via metadataStore.WithTransaction.
//   - Inside the txn, engine.CopyPayload invokes
//     coordinator.IncrementRefCount for each unique src hash; the
//     coordinator's RefCount UPDATEs share the same txn, so they
//     commit/roll back atomically with PutFile(dst).
//   - On any error (Increment failure, PutFile failure, ctx cancel), the
//     txn rolls back ALL writes — no partial dstFileAttr, no partial
//     RefCount bumps committed.
//   - cache.InvalidateFile runs ONLY on success, AFTER the commit.
//
// Wiring status: the helper is NOT yet routed from NFS/SMB CREATE-file
// copy paths — those continue using their existing flows. File-level
// dedup will route copy operations through this helper. The helper
// exists with full test coverage and is consumed only by tests.
//
// Note on engine txn semantics: engine.CopyPayload is invoked with the
// caller-supplied context (NOT a txn-bound ctx). The coordinator
// implementation (per-share runtime wrapper) is responsible for binding
// its RefCount UPDATEs to whatever txn is currently active for the
// caller's context — see pkg/blockstore/engine/coordinator.go for the
// contract. The metadata.Transaction passed to fn here is the canonical
// place where dst's PutFile lands; engine increments piggyback on the
// same txn through the coordinator's per-impl mechanism.
func CopyPayload(
	ctx context.Context,
	blockStore *engine.Store,
	metadataStore metadata.MetadataStore,
	cache CacheInvalidator,
	srcFileHandle, dstFileHandle metadata.FileHandle,
	srcPayloadID, dstPayloadID metadata.PayloadID,
) error {
	srcFile, err := metadataStore.GetFile(ctx, srcFileHandle)
	if err != nil {
		return fmt.Errorf("CopyPayload: fetch src file attr: %w", err)
	}

	err = metadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
		// Bind the active txn into the context so the per-share
		// metadataCoordinator's IncrementRefCount / DecrementRefCount
		// calls route through it instead of the connection pool. Without
		// this, every successful Increment commits on its own connection
		// and survives a downstream PutFile rollback — silent leak.
		// See pkg/metadata/tx_context.go for the carrier API.
		txCtx := metadata.WithTx(ctx, tx)

		// engine.CopyPayload bumps RefCount per unique hash via the
		// coordinator. The coordinator wires its UPDATEs into the active
		// txn (see coordinator.go transaction-ownership note).
		newBlocks, err := blockStore.CopyPayload(txCtx, string(srcPayloadID), string(dstPayloadID), srcFile.Blocks)
		if err != nil {
			return fmt.Errorf("engine copy payload: %w", err)
		}

		dstFile, err := tx.GetFile(ctx, dstFileHandle)
		if err != nil {
			return fmt.Errorf("fetch dst file attr: %w", err)
		}
		dstFile.Blocks = newBlocks
		dstFile.Size = srcFile.Size
		dstFile.Mtime = time.Now()
		if err := tx.PutFile(ctx, dstFile); err != nil {
			return fmt.Errorf("persist dst file attr: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// POST-txn: conservative invalidation for dst — its content changed
	// wholesale. Pass nil removedHashes so the cache drops everything
	// keyed at dstPayloadID. Other files referencing the shared hashes
	// via dedup keep their entries warm.
	if cache != nil {
		cache.InvalidateFile(dstPayloadID, nil)
	}
	return nil
}
