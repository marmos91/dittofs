package offloader

import (
	"context"
	"fmt"
	"slices"

	"github.com/marmos91/dittofs/internal/logger"
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
	defer m.uploadsMu.Unlock()
	return m.uploads[payloadID]
}

// handleUploadSuccess performs common post-upload tasks:
// 1. Registers block in ObjectStore for deduplication
// 2. Tracks block hash for finalization
// 3. Marks block as uploaded in cache
//
// This consolidates the success handling from both startBlockUpload and uploadRemainingBlocks.
func (m *Offloader) handleUploadSuccess(ctx context.Context, payloadID string, chunkIdx, blockIdx uint32, hash [32]byte, dataSize uint32) {
	// Register block in ObjectStore for deduplication
	objBlock := metadata.NewObjectBlock(
		metadata.ContentHash{}, // ChunkHash - will be set during finalization
		blockIdx,
		hash,
		dataSize,
	)
	objBlock.MarkUploaded()
	_ = m.objectStore.PutBlock(ctx, objBlock)

	// Track block hash for finalization
	state := m.getUploadState(payloadID)
	if state != nil {
		key := blockKey{chunkIdx: chunkIdx, blockIdx: blockIdx}
		state.blocksMu.Lock()
		state.blockHashes[key] = hash
		state.blocksMu.Unlock()
	}

	// Mark block as uploaded so it can be evicted
	m.cache.MarkBlockUploaded(ctx, payloadID, chunkIdx, blockIdx)
}

// getOrderedBlockHashes returns block hashes in order (sorted by chunk/block index).
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

	// Collect keys and sort by chunk index first, then block index
	keys := make([]blockKey, 0, len(state.blockHashes))
	for k := range state.blockHashes {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b blockKey) int {
		if a.chunkIdx != b.chunkIdx {
			return int(a.chunkIdx) - int(b.chunkIdx)
		}
		return int(a.blockIdx) - int(b.blockIdx)
	})

	// Build ordered hash list
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

// DeleteWithRefCount deletes a finalized file using reference counting.
// For files with an ObjectID (finalized), this decrements reference counts
// and cascades delete when counts reach zero.
//
// Parameters:
//   - objectID: The content hash of the finalized object
//   - blockHashes: The hashes of all blocks in the object (for cascade delete)
//
// Returns nil if successful. If ObjectStore is not configured, falls back to
// prefix-based delete using payloadID.
func (m *Offloader) DeleteWithRefCount(ctx context.Context, payloadID string, objectID metadata.ContentHash, blockHashes []metadata.ContentHash) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("offloader is closed")
	}

	// Clean up upload state for this file
	m.uploadsMu.Lock()
	delete(m.uploads, payloadID)
	m.uploadsMu.Unlock()

	// If no ObjectStore, fall back to prefix-based delete
	if m.objectStore == nil {
		if m.blockStore != nil {
			return m.blockStore.DeleteByPrefix(ctx, payloadID+"/")
		}
		return nil
	}

	// Decrement object reference count
	refCount, err := m.objectStore.DecrementObjectRefCount(ctx, objectID)
	if err != nil {
		// Object might not exist (race condition or unfinalized)
		logger.Warn("Failed to decrement object refcount",
			"objectID", objectID,
			"error", err)
		// Fall back to prefix delete
		if m.blockStore != nil {
			return m.blockStore.DeleteByPrefix(ctx, payloadID+"/")
		}
		return nil
	}

	// If reference count > 0, other files still reference this object
	if refCount > 0 {
		logger.Debug("Object still has references, not deleting blocks",
			"objectID", objectID,
			"refCount", refCount)
		return nil
	}

	// Object reference count is 0 - cascade delete blocks
	logger.Info("Object refcount reached 0, cascade deleting blocks",
		"objectID", objectID,
		"blockCount", len(blockHashes))

	// Delete each block (decrement refcount, delete if 0)
	for _, blockHash := range blockHashes {
		blockRefCount, err := m.objectStore.DecrementBlockRefCount(ctx, blockHash)
		if err != nil {
			logger.Warn("Failed to decrement block refcount",
				"blockHash", blockHash,
				"error", err)
			continue
		}

		if blockRefCount == 0 {
			// Block refcount is 0 - safe to delete from block store
			blockKey := blockHash.String()
			if m.blockStore != nil {
				if err := m.blockStore.DeleteBlock(ctx, blockKey); err != nil {
					logger.Warn("Failed to delete block from store",
						"blockKey", blockKey,
						"error", err)
				}
			}

			// Delete block metadata
			if err := m.objectStore.DeleteBlock(ctx, blockHash); err != nil {
				logger.Warn("Failed to delete block metadata",
					"blockHash", blockHash,
					"error", err)
			}
		}
	}

	// Delete object metadata
	if err := m.objectStore.DeleteObject(ctx, objectID); err != nil {
		logger.Warn("Failed to delete object metadata",
			"objectID", objectID,
			"error", err)
	}

	return nil
}
