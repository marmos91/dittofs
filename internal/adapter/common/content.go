package common

import (
	"github.com/marmos91/dittofs/pkg/block"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// ErrNoContentStore is what a data operation (read, write, commit, flush,
// copy, clone, allocation and hole probes) answers: block.ErrNoContentStore
// carried in the I/O row, so each protocol's StatusForErr maps it to its own
// generic I/O status.
var ErrNoContentStore error = &merrs.StoreError{
	Code:    merrs.ErrIOError,
	Message: block.ErrNoContentStore.Error(),
	Cause:   block.ErrNoContentStore,
}
