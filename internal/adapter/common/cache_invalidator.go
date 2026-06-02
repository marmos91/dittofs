package common

import (
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// CacheInvalidator is the minimal cache-surface adapter helpers (and the
// common.CopyPayload helper) depend on for surgical post-transaction
// invalidation. It is defined here in package common rather than imported
// from pkg/blockstore/engine so that adapter helpers remain decoupled from
// the concrete engine.Cache type — the engine's Cache type implements this
// interface implicitly via its InvalidateFile method.
//
// Contract:
//   - InvalidateFile is invoked POST-transaction, only after a successful
//     metadata commit. Pre-commit invalidation would drop warm cache
//     entries unnecessarily on rollback.
//   - removedHashes carries the diff between the old and new
//     FileAttr.Blocks lists (the hashes that disappeared). A nil slice is
//     a "drop everything for this payloadID" signal — used by CopyPayload
//     where dst content changes wholesale.
//   - Cross-file dedup: the cache MUST keep entries warm for hashes still
//     referenced by other files (single-entry-per-hash sharing).
//     Surgical invalidation is the mechanism that preserves that warmth.
type CacheInvalidator interface {
	InvalidateFile(payloadID metadata.PayloadID, removedHashes []block.ContentHash)
}

// diffRemovedHashes returns hashes present in oldBlocks but absent from
// newBlocks. Duplicates in oldBlocks (the same hash appearing at multiple
// offsets — legitimate when an identical chunk repeats in a file) are
// reported once per occurrence so callers can preserve refcount
// multiplicity.
//
// Used by the WriteToBlockStore + CopyPayload helpers to compute
// the surgical invalidation payload. For the "drop-all" case (CopyPayload
// destination), callers pass nil rather than a precomputed diff.
func diffRemovedHashes(oldBlocks, newBlocks []block.BlockRef) []block.ContentHash {
	if len(oldBlocks) == 0 {
		return nil
	}
	newSet := make(map[block.ContentHash]struct{}, len(newBlocks))
	for _, b := range newBlocks {
		newSet[b.Hash] = struct{}{}
	}
	var removed []block.ContentHash
	for _, b := range oldBlocks {
		if _, ok := newSet[b.Hash]; !ok {
			removed = append(removed, b.Hash)
		}
	}
	return removed
}
