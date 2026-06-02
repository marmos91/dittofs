package snapshot

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// HashSetFromMetadataStore enumerates every FileBlock content-hash known
// to store and returns them as a deduplicated HashSet. It is the
// post-verify walker for Runtime.RestoreSnapshot: after a successful
// Reset+Restore the orchestrator hands the freshly-restored metadata to
// this helper, then re-runs VerifyRemoteDurability against the result.
//
// Reuses the MetadataStore.EnumerateFileBlocks surface (the same one GC
// mark uses) so the helper stays backend-agnostic and inherits the
// streaming-via-cursor + ctx.Done contract.
//
// Legacy pre-CAS rows emit the zero ContentHash per the interface
// contract; they correspond to no remote object and are skipped to avoid
// spurious verify failures.
func HashSetFromMetadataStore(ctx context.Context, store metadata.Store) (*block.HashSet, error) {
	if store == nil {
		return nil, fmt.Errorf("snapshot: hashset from metadata: nil store")
	}
	hs := block.NewHashSet(0)
	var zero block.ContentHash
	err := store.EnumerateFileBlocks(ctx, func(h block.ContentHash) error {
		if h != zero {
			hs.Add(h)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("snapshot: hashset from metadata: %w", err)
	}
	return hs, nil
}
