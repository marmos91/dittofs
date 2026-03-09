package io

import (
	"context"

	"github.com/marmos91/dittofs/pkg/blockstore/local"
)

// WriteAt writes data at the specified offset to the local cache.
// Writes go directly to cache. The periodic syncer handles background upload.
func WriteAt(ctx context.Context, localStore local.LocalWriter, payloadID string, data []byte, offset uint64) error {
	if len(data) == 0 {
		return nil
	}
	return localStore.WriteAt(ctx, payloadID, data, offset)
}
