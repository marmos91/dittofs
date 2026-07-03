package runtime

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// OpenFileEnumerator is implemented by protocol adapters that track live open
// handles server-side (the NFSv4 state manager and the SMB open-file table).
// Any adapter provider registered via SetAdapterProvider that implements this
// interface contributes its open files to the block-GC open-handle hold
// (#1448): an open-but-unlinked file's blocks must survive GC until last
// close, not merely for the grace period.
//
// EnumerateOpenFiles streams the opaque metadata file handle of every file
// that currently has at least one open handle. Handles may be stale (share
// removed, file purged) — the consumer skips those. fn must not be called
// while adapter-internal locks are held if fn can block (the hold consumer
// performs metadata-store reads inside fn).
type OpenFileEnumerator interface {
	EnumerateOpenFiles(ctx context.Context, fn func(fileHandle []byte) error) error
}

// openFileEnumerators snapshots the currently registered adapter providers
// that implement OpenFileEnumerator. NFSv4 registers its state manager under
// the "nfs" provider key; SMB registers its handler under "smb_open_files".
func (r *Runtime) openFileEnumerators() []OpenFileEnumerator {
	r.adapterProvidersMu.RLock()
	defer r.adapterProvidersMu.RUnlock()
	var out []OpenFileEnumerator
	for _, p := range r.adapterProviders {
		if e, ok := p.(OpenFileEnumerator); ok {
			out = append(out, e)
		}
	}
	return out
}

// forEachOpenUnlinkedFile streams every currently-open file that belongs to
// one of the scoped shares and has been unlinked (nlink == 0). Files that are
// still linked are skipped: their hashes are already in the GC mark live set
// via the store's live-set query, so holding them would be redundant work.
//
// Stale handles (share gone, file object purged) are skipped: there is no
// data left to protect behind them. Any other metadata-store error propagates
// so the caller fails closed (a GC pass that cannot determine the held set
// must not sweep).
func (r *Runtime) forEachOpenUnlinkedFile(ctx context.Context, shares map[string]struct{}, fn func(shareName string, file *metadata.File) error) error {
	// A file open via multiple protocols at once (e.g. NFSv4 + SMB) is
	// reported by every enumerator; visit each handle once so per-file work
	// (GetFile) and observability counters don't scale with protocol fan-out.
	seen := make(map[string]struct{})
	for _, enum := range r.openFileEnumerators() {
		err := enum.EnumerateOpenFiles(ctx, func(fileHandle []byte) error {
			if _, dup := seen[string(fileHandle)]; dup {
				return nil
			}
			seen[string(fileHandle)] = struct{}{}
			handle := metadata.FileHandle(fileHandle)
			shareName, err := r.GetShareNameForHandle(ctx, handle)
			if err != nil {
				// Stale or foreign handle (share removed mid-scan) — nothing
				// to hold for it.
				logger.Debug("open-handle hold: unresolvable handle skipped", "err", err)
				return nil
			}
			if _, ok := shares[shareName]; !ok {
				return nil
			}
			mds, err := r.GetMetadataStoreForShare(shareName)
			if err != nil {
				return fmt.Errorf("open-handle hold: metadata store for share %q: %w", shareName, err)
			}
			file, err := mds.GetFile(ctx, handle)
			if err != nil {
				if metadata.IsNotFoundError(err) {
					// File object already purged — the open handle can no
					// longer reach any data, so there is nothing to hold.
					return nil
				}
				return fmt.Errorf("open-handle hold: get file for share %q: %w", shareName, err)
			}
			if file.Nlink != 0 {
				// Still linked — covered by the store live-set query.
				return nil
			}
			return fn(shareName, file)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// openHandleHoldProvider implements engine.HoldProvider by injecting the
// block hashes of every open-but-unlinked file into the GC mark live set
// (#1448). The store live-set query (EnumerateFileChunks) deliberately
// excludes nlink=0 inodes (#1433) so deleted files are reclaimed; this hold
// re-adds exactly the unlinked files that still have a live open handle
// (NFSv4 open stateids, SMB open table), restoring POSIX
// unlink-while-open semantics beyond the grace period. Once the last handle
// closes (or the client's lease/session expires), the hold disappears and the
// next GC pass reclaims the blocks as usual.
type openHandleHoldProvider struct {
	rt     *Runtime
	shares map[string]struct{}
}

// HeldHashes implements engine.HoldProvider. The engine-passed shares
// argument is informational only; iteration uses the closure-captured share
// set fixed at construction time (same contract as SnapshotHoldProvider).
func (p *openHandleHoldProvider) HeldHashes(ctx context.Context, remoteEndpointID string, _ []string, fn func(block.ContentHash) error) error {
	if p == nil || p.rt == nil {
		return nil
	}
	// Dedup across files before emitting: many open handles can reference
	// files sharing chunks.
	union := block.NewHashSet(0)
	held := 0
	err := p.rt.forEachOpenUnlinkedFile(ctx, p.shares, func(_ string, file *metadata.File) error {
		for i := range file.Blocks {
			union.Add(file.Blocks[i].Hash)
		}
		held++
		return nil
	})
	if err != nil {
		return err
	}
	if held == 0 {
		return nil
	}
	if err := union.ForEach(fn); err != nil {
		return fmt.Errorf("open-handle hold: emit union: %w", err)
	}
	logger.Debug("open-handle hold: emitted held hashes",
		"open_unlinked_files", held,
		"distinct_hashes", union.Len(),
		"remote_endpoint_id", remoteEndpointID,
	)
	return nil
}

// openPayloadIDsForShare returns the PayloadIDs of every open-but-unlinked
// file in the share. The stranded-row reconcile must not reap these payloads'
// rows: reaping would both drop their hashes from the mark live set and
// destroy the manifest the open handle still reads through.
func (r *Runtime) openPayloadIDsForShare(ctx context.Context, shareName string) (map[string]struct{}, error) {
	held := make(map[string]struct{})
	scope := map[string]struct{}{shareName: {}}
	err := r.forEachOpenUnlinkedFile(ctx, scope, func(_ string, file *metadata.File) error {
		if file.PayloadID != "" {
			held[string(file.PayloadID)] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return held, nil
}

// multiHoldProvider chains hold providers: every provider contributes its
// held hashes to the mark live set; any error fails the whole enumeration
// (fail-closed).
type multiHoldProvider []engine.HoldProvider

func (m multiHoldProvider) HeldHashes(ctx context.Context, remoteEndpointID string, shares []string, fn func(block.ContentHash) error) error {
	for _, p := range m {
		if p == nil {
			continue
		}
		if err := p.HeldHashes(ctx, remoteEndpointID, shares, fn); err != nil {
			return err
		}
	}
	return nil
}

// gcHoldForShare returns the full hold set for a single share's local-tier GC
// pass: snapshot manifests plus open-but-unlinked files.
func (r *Runtime) gcHoldForShare(shareName string) engine.HoldProvider {
	return r.gcHoldForRemote([]string{shareName})
}

// gcHoldForRemote returns the full hold set for a remote-tier GC pass scoped
// to the shares that reference the remote: snapshot manifests plus
// open-but-unlinked files.
func (r *Runtime) gcHoldForRemote(shareNames []string) engine.HoldProvider {
	scope := make(map[string]struct{}, len(shareNames))
	for _, name := range shareNames {
		scope[name] = struct{}{}
	}
	return multiHoldProvider{
		r.snapshotHoldForRemote(shareNames),
		&openHandleHoldProvider{rt: r, shares: scope},
	}
}
