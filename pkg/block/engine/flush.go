package engine

import (
	"context"

	"github.com/marmos91/dittofs/pkg/block"
)

// Flush ensures all dirty data for a payload is persisted by delegating
// to the syncer's mirror loop. CAS StoreChunk already dedups physically
// by content hash, so no separate file-level dedup hook runs here.
func (bs *Store) Flush(ctx context.Context, payloadID string) (*block.FlushResult, error) {
	if err := bs.enter(); err != nil {
		return nil, err
	}
	defer bs.closeMu.RUnlock()
	// Delegate to syncer's mirror loop.
	return bs.syncer.Flush(ctx, payloadID)
}

// DrainAllUploads waits for all pending uploads to complete.
func (bs *Store) DrainAllUploads(ctx context.Context) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	return bs.syncer.DrainAllUploads(ctx)
}

// DrainRollups forces the local store to roll up every currently-dirty
// payload into CAS + the FileBlock manifest, bypassing the
// stabilization-window gate. The snapshot-create orchestration calls this
// BEFORE the metadata Backup() so the dump observes a fully-populated
// FileAttr.Blocks (and therefore a non-empty snapshot manifest). It must
// run before DrainAllUploads — rollup is what produces the CAS chunks the
// syncer then mirrors to the remote.
func (bs *Store) DrainRollups(ctx context.Context) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	return bs.local.DrainRollups(ctx)
}

// ResetLocalState clears the local store's per-payload append-log state so
// post-restore reads resolve purely through the restored CAS manifest.
// The snapshot-restore orchestration calls this BEFORE the metadata Reset()
// + Restore() (not after): clearing the overlay first leaves no dirty
// intervals for a background rollup worker to flush into the freshly-restored
// metadata, so a file modified in place after the snapshot is not served from
// a stale append-log record overlaid on the restored CAS bytes.
func (bs *Store) ResetLocalState(ctx context.Context) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	return bs.local.ResetLocalState(ctx)
}
