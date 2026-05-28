package snapshot

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// HashSetFromMetadataStore enumerates every FileBlock content-hash known to
// store and returns them as a deduplicated HashSet. It is the post-verify
// walker for Runtime.RestoreSnapshot step 7 (D-24-14): after a successful
// Reset+Restore the orchestrator hands the freshly-restored metadata to
// this helper, then re-runs VerifyRemoteDurability against the resulting
// set to confirm every restored hash is actually reachable on the remote.
//
// Design choice (per P24-03 PATTERNS audit):
// The MetadataStore interface already exposes EnumerateFileBlocks(ctx, fn)
// for exactly this purpose — the GC mark phase calls it to populate the
// live-block set with the same fail-closed semantics. Reusing that surface
// keeps this helper backend-agnostic, sidesteps AuthContext gymnastics
// (the GC mark phase doesn't construct one either), and inherits the
// streaming-via-cursor + ctx.Done() contract documented on the interface.
//
// Behavior:
//   - The zero ContentHash (legacy pre-CAS rows, per the interface contract)
//     is skipped: it is never a real remote object and including it would
//     guarantee a spurious ErrRestoreVerifyFailed.
//   - Duplicates collapse automatically via HashSet.Add — two FileBlock
//     rows that share a hash contribute one entry, matching the manifest
//     contract used during snapshot create.
//   - The walk respects ctx cancellation: EnumerateFileBlocks checks
//     ctx.Err() between emissions and returns the wrapped ctx error, which
//     this helper surfaces unchanged so callers can errors.Is the
//     context sentinel.
//
// The returned HashSet is non-nil even on empty stores (Len() == 0).
func HashSetFromMetadataStore(ctx context.Context, store metadata.MetadataStore) (*blockstore.HashSet, error) {
	if store == nil {
		return nil, fmt.Errorf("snapshot: hashset from metadata: nil store")
	}
	hs := blockstore.NewHashSet(0)
	var zero blockstore.ContentHash
	err := store.EnumerateFileBlocks(ctx, func(h blockstore.ContentHash) error {
		if h == zero {
			// Legacy pre-CAS rows emit the zero hash by interface contract;
			// they correspond to no remote object and would cause a false
			// negative in VerifyRemoteDurability.
			return nil
		}
		hs.Add(h)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("snapshot: hashset from metadata: %w", err)
	}
	return hs, nil
}
