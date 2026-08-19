package runtime

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/engine"
)

// CheckManifests runs the metadata-only manifest-coverage scan for the named
// share: per payload it compares the byte ranges the manifest rows cover
// against the file's recorded size and reports uncovered ranges, rows that
// carry no parseable chunk offset, and rows whose hash the synced-hash store
// does not know.
//
// The scan fetches no block and reads no file data, so its cost is a metadata
// walk regardless of how much data the share holds.
//
// The unknown-hash check runs only when the share has a remote store. A
// local-only share never marks a hash synced, so running it there would report
// every row in the share as unknown. A share whose block store cannot be
// resolved is treated as local-only — the conservative direction, since it
// suppresses a check rather than inventing findings.
//
// Returns ErrShareNotFound (wrapped) when the share is unknown.
func (r *Runtime) CheckManifests(ctx context.Context, shareName string) (*engine.ManifestCheckResult, error) {
	mds, err := r.GetMetadataStoreForShare(shareName)
	if err != nil {
		return nil, err
	}

	checkSynced := false
	if bs, bsErr := r.sharesSvc.GetBlockStoreForShare(shareName); bsErr != nil {
		logger.Debug("store check: block store lookup failed, skipping the synced-hash check",
			"share", shareName, "error", bsErr)
	} else if bs != nil {
		checkSynced = bs.HasRemoteStore()
	}

	res, err := engine.CheckManifests(ctx, shareName, mds, checkSynced)
	if err != nil {
		return nil, err
	}
	logger.Info("store check: complete",
		"share", shareName,
		"files_scanned", res.FilesScanned,
		"damaged_payloads", res.DamagedPayloads,
		"claimed_uncovered_bytes", res.ClaimedUncoveredBytes,
		"unplaceable_rows", res.UnplaceableRows,
		"unknown_hash_rows", res.UnknownHashRows,
		"duration_ms", res.DurationMS,
	)
	return res, nil
}
