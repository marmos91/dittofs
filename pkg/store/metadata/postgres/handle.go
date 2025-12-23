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

// encodeFileHandle creates a file handle from share name and UUID.
// This is a convenience wrapper around metadata.EncodeShareHandle that
// returns nil on error (share names are validated at configuration time,
// so encoding errors should not occur in practice).
func encodeFileHandle(shareName string, id uuid.UUID) metadata.FileHandle {
	handle, err := metadata.EncodeShareHandle(shareName, id)
	if err != nil {
		return nil
	}
	return handle
}
