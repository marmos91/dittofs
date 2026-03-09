package offloader

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// getOrCreateUploadState returns the upload state for a file, creating it if needed.
func (m *Offloader) getOrCreateUploadState(payloadID string) *fileUploadState {
	m.uploadsMu.Lock()
	defer m.uploadsMu.Unlock()

	state, exists := m.uploads[payloadID]
	if !exists {
		state = &fileUploadState{
			uploaded:    make(map[blockKey]bool),
			blockHashes: make(map[blockKey][32]byte),
		}
		m.uploads[payloadID] = state
	}
	return state
}

// getUploadState returns the upload state for a file, or nil if not found.
func (m *Offloader) getUploadState(payloadID string) *fileUploadState {
	m.uploadsMu.Lock()
	state := m.uploads[payloadID]
	m.uploadsMu.Unlock()
	return state
}

// handleUploadSuccess registers the block for dedup, tracks its hash, and marks it remote.
func (m *Offloader) handleUploadSuccess(ctx context.Context, payloadID string, blockIdx uint64, hash [32]byte, dataSize uint32) {
	blockID := fmt.Sprintf("%s/%d", payloadID, blockIdx)
	fb, err := m.fileBlockStore.GetFileBlock(ctx, blockID)
	if err != nil {
		fb = &metadata.FileBlock{
			ID:       blockID,
			RefCount: 1,
		}
	}
	fb.Hash = hash
	fb.DataSize = dataSize
	fb.BlockStoreKey = cache.FormatStoreKey(payloadID, blockIdx)
	fb.State = metadata.BlockStateRemote
	if err := m.fileBlockStore.PutFileBlock(ctx, fb); err != nil {
		logger.Error("Failed to register block in FileBlockStore",
			"payloadID", payloadID, "blockIdx", blockIdx, "error", err)
	}

	m.trackBlockHash(payloadID, blockIdx, hash)
	m.cache.MarkBlockRemote(ctx, payloadID, blockIdx)
}

// getOrderedBlockHashes returns block hashes in order (sorted by block index).
func (m *Offloader) getOrderedBlockHashes(payloadID string) [][32]byte {
	state := m.getUploadState(payloadID)
	if state == nil {
		return nil
	}

	state.blocksMu.Lock()
	defer state.blocksMu.Unlock()

	if len(state.blockHashes) == 0 {
		return nil
	}

	keys := make([]blockKey, 0, len(state.blockHashes))
	for k := range state.blockHashes {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b blockKey) int {
		return cmp.Compare(a, b)
	})

	hashes := make([][32]byte, len(keys))
	for i, k := range keys {
		hashes[i] = state.blockHashes[k]
	}

	return hashes
}

// invokeFinalizationCallback calls the finalization callback with ordered block hashes.
func (m *Offloader) invokeFinalizationCallback(ctx context.Context, payloadID string) {
	m.mu.RLock()
	callback := m.onFinalized
	m.mu.RUnlock()

	if callback != nil {
		hashes := m.getOrderedBlockHashes(payloadID)
		if len(hashes) > 0 {
			callback(ctx, payloadID, hashes)
		}
	}
}

// DeleteWithRefCount decrements RefCount for each block and deletes blocks that reach zero.
func (m *Offloader) DeleteWithRefCount(ctx context.Context, payloadID string, blockIDs []string) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("offloader is closed")
	}

	m.uploadsMu.Lock()
	delete(m.uploads, payloadID)
	m.uploadsMu.Unlock()

	if m.fileBlockStore == nil {
		if m.blockStore != nil {
			return m.blockStore.DeleteByPrefix(ctx, payloadID+"/")
		}
		return nil
	}

	for _, blockID := range blockIDs {
		newCount, err := m.fileBlockStore.DecrementRefCount(ctx, blockID)
		if err != nil {
			logger.Warn("Failed to decrement block refcount",
				"blockID", blockID, "error", err)
			continue
		}

		if newCount == 0 {
			fb, err := m.fileBlockStore.GetFileBlock(ctx, blockID)
			if err != nil {
				continue
			}

			if fb.BlockStoreKey != "" && m.blockStore != nil {
				if err := m.blockStore.DeleteBlock(ctx, fb.BlockStoreKey); err != nil {
					logger.Warn("Failed to delete block from store",
						"blockID", blockID,
						"error", err)
				}
			}

			if err := m.fileBlockStore.DeleteFileBlock(ctx, blockID); err != nil {
				logger.Warn("Failed to delete block metadata",
					"blockID", blockID,
					"error", err)
			}
		}
	}

	return nil
}
