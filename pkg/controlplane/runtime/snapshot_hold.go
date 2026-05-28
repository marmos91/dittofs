package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// SnapshotHoldProvider streams the union of every ready snapshot's
// manifest hashes into the block-GC mark phase, scoped to a fixed share
// list captured at construction time. Only state='ready' rows contribute.
// A ready row whose on-disk manifest is missing aborts the run via the
// mark fail-closed path. Shares with no persistent local-store directory
// (memory backend) are skipped.
type SnapshotHoldProvider struct {
	rt     *Runtime
	shares []string
}

// HeldHashes implements engine.HoldProvider. The engine-passed shares
// argument is informational only; iteration uses the closure-captured
// per-remote share list set at construction time.
func (p *SnapshotHoldProvider) HeldHashes(ctx context.Context, remoteEndpointID string, _ []string, fn func(blockstore.ContentHash) error) error {
	if p == nil || p.rt == nil || p.rt.store == nil {
		return nil
	}

	for _, shareName := range p.shares {
		localStoreDir, err := p.rt.sharesSvc.LocalStoreDir(shareName)
		if err != nil {
			// Share removed between GC entry and hold enumeration —
			// no held hashes to contribute.
			if errors.Is(err, shares.ErrShareNotFound) {
				continue
			}
			return fmt.Errorf("snapshot hold: resolve local store dir for share %q: %w", shareName, err)
		}
		if localStoreDir == "" {
			continue
		}

		snaps, err := p.rt.store.ListSnapshots(ctx, shareName)
		if err != nil {
			return fmt.Errorf("snapshot hold: list snapshots for share %q: %w", shareName, err)
		}

		for _, snap := range snaps {
			if snap.State != models.StateReady {
				continue
			}
			manifestPath := snap.ManifestPath(localStoreDir)
			count, err := streamManifest(manifestPath, fn)
			if err != nil {
				return fmt.Errorf("snapshot hold: stream manifest for share %q snapshot %q at %q: %w",
					shareName, snap.ID, manifestPath, err)
			}
			logger.Debug("snapshot hold: streamed hashes",
				"share", shareName,
				"snapshot_id", snap.ID,
				"count", count,
				"remote_endpoint_id", remoteEndpointID,
			)
		}
	}
	return nil
}

// streamManifest opens the manifest at path, parses it, and forwards
// every hash through fn. Returns the count for logging.
func streamManifest(path string, fn func(blockstore.ContentHash) error) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	hs, err := snapshot.ReadManifest(f)
	if err != nil {
		return 0, err
	}
	if err := hs.ForEach(fn); err != nil {
		return 0, err
	}
	return hs.Len(), nil
}

// snapshotHoldForRemote returns an engine.HoldProvider that streams held
// hashes for the supplied per-remote share scope. Every share in the
// list, by construction, points at the caller's remote.
func (r *Runtime) snapshotHoldForRemote(shareNames []string) engine.HoldProvider {
	return &SnapshotHoldProvider{
		rt:     r,
		shares: append([]string(nil), shareNames...),
	}
}
