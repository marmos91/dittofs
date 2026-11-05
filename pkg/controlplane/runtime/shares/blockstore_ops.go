package shares

import (
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/remote"
)

// RemoteStoreEntry describes a distinct remote block store that is referenced
// by one or more shares. Surface used by production block-GC enumeration
// (Runtime.RunBlockGC): we want each underlying remote store scanned exactly
// once per run, not once per share.
type RemoteStoreEntry struct {
	// Store is the underlying remote store (NOT the nonClosingRemote wrapper).
	Store remote.RemoteStore
	// ConfigID is the remote block-store config UUID this entry represents.
	// Empty string indicates a test-only binding (SetShareRemoteForTest).
	ConfigID string
	// Shares are the registered share names that reference this remote.
	Shares []string
}

// DistinctRemoteStores returns every distinct underlying remote.RemoteStore
// referenced by at least one registered share. Shares that reference the same
// remote-store config (ref-counted via remoteStores) are grouped into a
// single entry — deduped by ConfigID, NOT by the per-share nonClosingRemote
// wrapper pointer. Local-only shares (no remote) contribute nothing.
//
// Returned entries have a non-nil Store and a non-empty Shares slice. Order
// is unspecified (map iteration).
func (s *Service) DistinctRemoteStores() []RemoteStoreEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Bucket share names by the configID they reference. configID == "" means
	// "local-only share" — skipped entirely.
	sharesByConfigID := make(map[string][]string, len(s.remoteStores))
	for name, sh := range s.registry {
		if sh.remoteConfigID == "" {
			continue
		}
		sharesByConfigID[sh.remoteConfigID] = append(sharesByConfigID[sh.remoteConfigID], name)
	}

	out := make([]RemoteStoreEntry, 0, len(sharesByConfigID))
	for cid, shareNames := range sharesByConfigID {
		sr, ok := s.remoteStores[cid]
		if !ok || sr == nil || sr.store == nil {
			// Orphaned configID → skip. DistinctRemoteStores is a read-only
			// surface; we don't try to self-heal bookkeeping here.
			continue
		}
		out = append(out, RemoteStoreEntry{
			Store:    sr.store,
			ConfigID: cid,
			Shares:   shareNames,
		})
	}
	return out
}

// ApplyDurability puts a share's relaxed metadata commit tier into effect on
// the running share, so an edit does not wait for a restart.
//
// The registry's own copy is updated too, so a later read of the share reports
// what is actually in force rather than what it was started with.
func (s *Service) ApplyDurability(name string, relaxedMetadataCommit bool, metadataSvc any) error {
	s.mu.Lock()
	share, exists := s.registry[name]
	if exists {
		share.writeback = relaxedMetadataCommit
	}
	s.mu.Unlock()
	if !exists {
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}

	if wb, ok := metadataSvc.(MetadataWritebackSetter); ok {
		wb.SetShareWriteback(name, relaxedMetadataCommit)
	}
	return nil
}
