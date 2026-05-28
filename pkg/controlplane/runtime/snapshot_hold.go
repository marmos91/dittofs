package runtime

import (
	"context"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// SnapshotHoldProvider is the runtime-side engine.HoldProvider that
// streams the union of every ready snapshot's manifest hashes into the
// block-GC mark phase, scoped to a fixed share list captured at
// construction time.
//
// Scoping: the provider is built once per remote at GC entry time via
// Runtime.snapshotHoldForRemote. The captured share list is the source
// of truth — the engine-passed `shares` argument to HeldHashes is
// informational only.
//
// Ground truth: only rows where state == models.StateReady are honored.
// A ready row whose on-disk manifest file is missing aborts the run via
// the mark fail-closed path (orphan-not-deleted is always preferred over
// live-data-deleted). Shares with no persistent local-store directory
// (in-memory backend) are skipped — no on-disk manifest can exist there.
type SnapshotHoldProvider struct {
	rt     *Runtime
	shares []string
}

// HeldHashes implements engine.HoldProvider. The engine-passed `shares`
// argument is informational only; iteration is over the closure-captured
// per-remote share list set at construction time.
//
// Behavior:
//   - r.store == nil short-circuits to nil (unconfigured runtime; no holds).
//   - For each captured share: resolve its local-store dir, list snapshots,
//     keep only state='ready', open the on-disk manifest file via
//     ReadManifest, and forward each ContentHash through fn.
//   - Empty local-store dir → skip share (memory backend; no manifest).
//   - Missing manifest file for a ready row → wrapped os.ErrNotExist returned;
//     the atomic-write contract guarantees ready implies a complete manifest.
//   - Any error wraps with share + snapshot ID + path context and returns
//     immediately (fail-closed).
func (p *SnapshotHoldProvider) HeldHashes(ctx context.Context, remoteEndpointID string, shares []string, fn func(blockstore.ContentHash) error) error {
	if p == nil || p.rt == nil || p.rt.store == nil {
		return nil
	}

	for _, shareName := range p.shares {
		localStoreDir, err := p.rt.sharesSvc.LocalStoreDir(shareName)
		if err != nil {
			return fmt.Errorf("snapshot hold: resolve local store dir for share %q: %w", shareName, err)
		}
		if localStoreDir == "" {
			// Memory backend has no persistent root; no on-disk manifest
			// can exist for this share.
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

// streamManifest opens the manifest file at path, parses it via
// ReadManifest, and forwards every hash through fn. The file handle is
// closed before this helper returns — avoiding defer-in-loop accumulation
// in the caller. Returns the count of hashes streamed for logging.
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
// hashes for the supplied per-remote share scope. configID is captured
// for log correlation only — the provider does not filter against it
// because the per-remote share list is the actual scope boundary (every
// share in the list, by construction, points at this remote).
func (r *Runtime) snapshotHoldForRemote(configID string, shareNames []string) engine.HoldProvider {
	_ = configID
	return &SnapshotHoldProvider{
		rt:     r,
		shares: append([]string(nil), shareNames...),
	}
}
