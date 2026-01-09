package postgres

import (
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// decodeFileHandle extracts the share name and UUID from a file handle
// This is a convenience wrapper around metadata.DecodeFileHandle
func decodeFileHandle(handle metadata.FileHandle) (shareName string, id uuid.UUID, err error) {
	return metadata.DecodeFileHandle(handle)
}

// encodeFileHandle creates a file handle from share name and UUID string
func encodeFileHandle(shareName string, idStr string) (metadata.FileHandle, error) {
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	return metadata.EncodeShareHandle(shareName, id)
}
