package basestore

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// GenerateHandle mints a fresh UUID-based file handle for the named share.
//
// The path a caller passes to the store-level GenerateHandle is ignored: every
// backend addresses files by UUID, and the path is carried in the file record.
// Returns an error rather than panicking when the encoded handle would exceed
// the 64-byte limit.
func GenerateHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return metadata.GenerateNewHandle(shareName)
}
