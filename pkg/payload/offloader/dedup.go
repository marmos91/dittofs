package offloader

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
)

// getUploadState returns the upload state for a file, or nil if not found.
func (m *Offloader) getUploadState(payloadID string) *fileUploadState {
	m.uploadsMu.Lock()
	state := m.uploads[payloadID]
	m.uploadsMu.Unlock()
	return state
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
