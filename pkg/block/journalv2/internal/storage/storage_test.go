package storage_test

import (
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
)

// The seam implements the interfaces as the operating system's storage does;
// this fails to compile if they change and the seam does not follow. The
// operating system's storage asserts the same in its own package.
var _ storage.FS = (*journalv2test.Seam)(nil)
