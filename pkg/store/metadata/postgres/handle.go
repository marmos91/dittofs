package postgres

import (
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// decodeFileHandle extracts the share name and UUID from a file handle
// This is a convenience wrapper around metadata.DecodeFileHandle
func decodeFileHandle(handle metadata.FileHandle) (shareName string, id uuid.UUID, err error) {
	return metadata.DecodeFileHandle(handle)
}
