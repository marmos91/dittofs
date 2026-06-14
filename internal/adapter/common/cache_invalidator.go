package common

import (
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// CacheInvalidator is the minimal cache-surface adapter helpers (and the
// common.CopyPayload helper) depend on for surgical post-transaction
// invalidation. It is defined here in package common rather than imported
// from pkg/block/engine so that adapter helpers remain decoupled from
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
