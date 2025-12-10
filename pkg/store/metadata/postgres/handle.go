package postgres

import (
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// generateFileHandle creates a new file handle for a file in a share
// Uses UUID v4 for collision resistance across multiple DittoFS instances
func generateFileHandle(shareName string) (metadata.FileHandle, uuid.UUID, error) {
	id := uuid.New()
	handle, err := metadata.EncodeShareHandle(shareName, id)
	if err != nil {
		return nil, uuid.Nil, err
	}
	return handle, id, nil
}

// decodeFileHandle extracts the share name and UUID from a file handle
// This is a convenience wrapper around metadata.DecodeFileHandle
func decodeFileHandle(handle metadata.FileHandle) (shareName string, id uuid.UUID, err error) {
	return metadata.DecodeFileHandle(handle)
}
