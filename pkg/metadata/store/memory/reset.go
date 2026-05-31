package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

var _ metadata.Resetable = (*MemoryMetadataStore)(nil)

// Reset truncates all in-memory metadata state under the store-level
// write lock. Engine-identity fields (capabilities, maxStorageBytes,
// maxFiles, storeID, attrPool) are preserved — they are static config,
// not data, and altering them would change the engine's API-level
// identity. Lazy sub-stores are nilled to mirror Restore's
// "never-initialized" branches.
func (s *MemoryMetadataStore) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("memory reset cancelled: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.shares = make(map[string]*shareData)
	s.files = make(map[string]*fileData)
	s.parents = make(map[string]metadata.FileHandle)
	s.children = make(map[string]map[string]metadata.FileHandle)
	s.linkCounts = make(map[string]uint32)
	s.deviceNumbers = make(map[string]*deviceNumber)
	s.pendingWrites = make(map[string]*metadata.WriteOperation)
	s.sessions = make(map[string]*metadata.ShareSession)
	s.sortedDirCache = make(map[string][]string)
	s.objectIndex = make(map[blockstore.ContentHash]string)
	s.serverConfig = metadata.MetadataServerConfig{}

	s.fileBlockData = nil
	s.lockStore = nil
	s.clientStore = nil
	s.durableStore = nil
	s.recoveryStore = nil

	s.usedBytes.Store(0)

	s.rollupMu.Lock()
	s.rollupOffsets = make(map[string]uint64)
	s.rollupMu.Unlock()

	s.syncedMu.Lock()
	s.synced = make(map[blockstore.ContentHash]time.Time)
	s.syncedMu.Unlock()

	return nil
}
