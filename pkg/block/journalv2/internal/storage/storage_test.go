package storage_test

import (
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
)

// The seam is the only implementation so far; this fails to compile if the
// interfaces change and the seam does not follow.
var _ storage.FS = (*journalv2test.Seam)(nil)
