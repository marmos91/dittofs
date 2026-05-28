package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// SnapshotHoldProvider streams the union of every held snapshot's
// manifest hashes into the block-GC mark phase, scoped to a fixed share
// list captured at construction time.
//
// D-23-02 filter: a snapshot contributes its hashes iff its on-disk
// manifest.hashes file exists, regardless of state. The on-disk manifest
// is the ground truth (Phase 22 D-04); its existence is the only fact
// GC needs. This covers three windows the prior state='ready' filter
// missed: (1) creating-post-manifest-pre-flip, (2) failed-retained-for-retry,
// (3) failed → creating retry (same id, same dir, atomic overwrite).
//
// A manifest missing via os.IsNotExist is the no-hold short-circuit. Any
// other stat error (e.g., permission denied, I/O fault) propagates so the
// mark phase aborts (INV-04 fail-closed). Shares with no persistent
// local-store directory (memory backend) are skipped.
//
// Delete-vs-mark race: the locks slice holds the per-share RWMutex
// pointers borrowed from Runtime.snapDeleteLocks (snapshotDeleteLock).
// HeldHashes RLocks every entry for the duration of the scan;
// AcquireDeleteLock(shareName) Lock-s the SAME shared mutex. Because
// every provider built for a share borrows the SAME pointer, a delete
// arriving on a different provider instance still blocks (or is blocked
// by) any concurrent mark. D-23-04.
type SnapshotHoldProvider struct {
	rt     *Runtime
	shares []string
	locks  []*sync.RWMutex
}

// HeldHashes implements engine.HoldProvider. The engine-passed shares
// argument is informational only; iteration uses the closure-captured
// per-remote share list set at construction time.
func (p *SnapshotHoldProvider) HeldHashes(ctx context.Context, remoteEndpointID string, _ []string, fn func(blockstore.ContentHash) error) error {
	if p == nil || p.rt == nil || p.rt.store == nil {
		return nil
	}

	// D-23-04: RLock every borrowed per-share mutex to block concurrent
	// snapshot-delete writers from removing rows + dirs mid-stream.
	// Locks are acquired in the construction order baked into p.locks;
	// the delete path acquires a single share's lock at a time, so
	// there is no cycle. Concurrent HeldHashes callers all RLock and
	// proceed in parallel under the read side.
	for _, lock := range p.locks {
		lock.RLock()
	}
	defer func() {
		for i := len(p.locks) - 1; i >= 0; i-- {
			p.locks[i].RUnlock()
		}
	}()

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
			manifestPath := snap.ManifestPath(localStoreDir)
			if _, err := os.Stat(manifestPath); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					// D-23-02: no manifest = no hold. Covers
					// pre-manifest-write creating rows and
					// operator-deleted ready/failed manifests.
					continue
				}
				return fmt.Errorf("snapshot hold: stat manifest %q: %w", manifestPath, err)
			}
			count, err := streamManifest(manifestPath, fn)
			if err != nil {
				return fmt.Errorf("snapshot hold: stream manifest for share %q snapshot %q at %q: %w",
					shareName, snap.ID, manifestPath, err)
			}
			logger.Debug("snapshot hold: streamed hashes",
				"share", shareName,
				"snapshot_id", snap.ID,
				"state", snap.State,
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
	defer func() { _ = f.Close() }()

	hs, err := snapshot.ReadManifest(f)
	if err != nil {
		return 0, err
	}
	if err := hs.ForEach(fn); err != nil {
		return 0, err
	}
	return hs.Len(), nil
}

// AcquireDeleteLock is the write-side counterpart used by the snapshot
// orchestration layer (Phase 23 plans 23-04/05) before invoking
// store.DeleteSnapshot + os.RemoveAll of the snapshot dir. Holding the
// lock blocks new HeldHashes callers (across ALL provider instances
// scoped to the same share) until release is invoked, so a concurrent
// GC mark phase never observes a snapshot whose row has been removed
// but whose manifest is still being read (or vice versa). D-23-04.
//
// The looked-up mutex is the SAME pointer that any concurrently-built
// SnapshotHoldProvider would have borrowed for shareName via
// snapshotDeleteLock, so the per-instance bug (each new provider got a
// fresh mutex, breaking mutual exclusion) cannot recur.
func (p *SnapshotHoldProvider) AcquireDeleteLock(shareName string) (release func()) {
	lock := p.rt.snapshotDeleteLock(shareName)
	lock.Lock()
	return lock.Unlock
}

// snapshotHoldForRemote returns an engine.HoldProvider that streams held
// hashes for the supplied per-remote share scope. Every share in the
// list, by construction, points at the caller's remote.
//
// The returned provider borrows per-share RWMutex pointers from
// Runtime.snapDeleteLocks (via snapshotDeleteLock). Multiple providers
// built across multiple GC runs all hold pointers to the SAME mutex
// per share, so AcquireDeleteLock on any instance blocks (or is blocked
// by) every concurrent mark, closing the delete-vs-mark race the
// per-instance mutex previously left open.
func (r *Runtime) snapshotHoldForRemote(shareNames []string) engine.HoldProvider {
	scoped := append([]string(nil), shareNames...)
	locks := make([]*sync.RWMutex, 0, len(scoped))
	for _, name := range scoped {
		locks = append(locks, r.snapshotDeleteLock(name))
	}
	return &SnapshotHoldProvider{
		rt:     r,
		shares: scoped,
		locks:  locks,
	}
}

// snapshotDeleteLock returns the shared per-share RWMutex that
// serializes the snapshot GC mark phase against the snapshot-delete
// write path. The mutex is allocated on first lookup and reused
// thereafter so every caller for the same share name sees the SAME
// pointer — required for AcquireDeleteLock(shareName) to actually
// block any concurrent HeldHashes that has the corresponding RLock,
// regardless of which provider instance constructed each. D-23-04.
func (r *Runtime) snapshotDeleteLock(shareName string) *sync.RWMutex {
	r.snapDeleteLocksMu.Lock()
	defer r.snapDeleteLocksMu.Unlock()
	if r.snapDeleteLocks == nil {
		r.snapDeleteLocks = make(map[string]*sync.RWMutex)
	}
	lock, ok := r.snapDeleteLocks[shareName]
	if !ok {
		lock = &sync.RWMutex{}
		r.snapDeleteLocks[shareName] = lock
	}
	return lock
}
