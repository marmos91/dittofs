package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Compile-time assertion: MemoryMetadataStore implements Resetable.
var _ metadata.Resetable = (*MemoryMetadataStore)(nil)

// Reset truncates all in-memory metadata state under the store-level
// write lock. The same *MemoryMetadataStore instance is reused — no
// close/reopen, no Service unregister/register dance.
//
// DATA fields (re-initialized to fresh empty maps):
//   - shares, files, parents, children, linkCounts, deviceNumbers
//   - pendingWrites, sessions, sortedDirCache
//   - rollupOffsets (guarded by rollupMu)
//   - synced (guarded by syncedMu)
//   - objectIndex
//   - usedBytes (atomically reset to 0)
//   - serverConfig (operational state — cleared, not config)
//   - Lazy sub-stores (fileBlockData, lockStore, clientStore, durableStore)
//     are nilled to match the "never initialized" state mirroring
//     Restore's nil-snapshot branches (backup.go lines 376-399).
//
// CONFIG fields (PRESERVED across Reset — engine identity / static config):
//   - capabilities, maxStorageBytes, maxFiles
//   - storeID (engine-persistent identifier; immutable for instance lifetime)
//   - attrPool (sync.Pool — implementation detail, no state to reset)
func (s *MemoryMetadataStore) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("memory reset cancelled: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// DATA: shares + file tree.
	s.shares = make(map[string]*shareData)
	s.files = make(map[string]*fileData)
	s.parents = make(map[string]metadata.FileHandle)
	s.children = make(map[string]map[string]metadata.FileHandle)
	s.linkCounts = make(map[string]uint32)
	s.deviceNumbers = make(map[string]*deviceNumber)
	s.pendingWrites = make(map[string]*metadata.WriteOperation)

	// DATA: transient/cache state.
	s.sessions = make(map[string]*metadata.ShareSession)
	s.sortedDirCache = make(map[string][]string)

	// DATA: object-id secondary index.
	s.objectIndex = make(map[blockstore.ContentHash]string)

	// DATA: server config (operational, not static — clear so a restore
	// observes a fresh-engine baseline before applying the dump).
	s.serverConfig = metadata.MetadataServerConfig{}

	// DATA: lazy sub-stores (nil mirrors "never initialized"; matches
	// the nil branches in Restore at backup.go lines 376-399).
	s.fileBlockData = nil
	s.lockStore = nil
	s.clientStore = nil
	s.durableStore = nil

	// DATA: usedBytes counter.
	s.usedBytes.Store(0)

	// rollupOffsets is guarded by its own mutex.
	s.rollupMu.Lock()
	s.rollupOffsets = make(map[string]uint64)
	s.rollupMu.Unlock()

	// synced is guarded by its own mutex.
	s.syncedMu.Lock()
	s.synced = make(map[blockstore.ContentHash]time.Time)
	s.syncedMu.Unlock()

	// CONFIG: capabilities, maxStorageBytes, maxFiles, storeID, attrPool
	// are intentionally NOT touched. They are engine-identity / static
	// config, not data. Widening Reset to include them would change the
	// engine's identity at the API surface and is out of scope.

	return nil
}
