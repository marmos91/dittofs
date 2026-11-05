package storage_test

import (
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// The seam implements the interfaces as the operating system's storage does;
// this fails to compile if they change and the seam does not follow. The
// operating system's storage asserts the same in its own package.
var _ storage.FS = (*journaltest.Seam)(nil)
