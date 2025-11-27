package handlers

import (
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// safeAdd performs checked addition of two uint64 values.
// Returns the sum and a boolean indicating whether overflow occurred.
func safeAdd(a, b uint64) (uint64, bool) {
	sum := a + b
	overflow := sum < a // If sum wrapped around, it will be less than a
	return sum, overflow
}

// buildWccAttr builds WCC (Weak Cache Consistency) attributes from FileAttr.
// Used in WRITE, COMMIT, and other procedures to help clients detect concurrent modifications.
//
// WCC data consists of file attributes before and after an operation, allowing clients
// to invalidate their caches if the file changed unexpectedly.
func buildWccAttr(attr *metadata.FileAttr) *types.WccAttr {
	return &types.WccAttr{
		Size: attr.Size,
		Mtime: types.TimeVal{
			Seconds:  uint32(attr.Mtime.Unix()),
			Nseconds: uint32(attr.Mtime.Nanosecond()),
		},
		Ctime: types.TimeVal{
			Seconds:  uint32(attr.Ctime.Unix()),
			Nseconds: uint32(attr.Ctime.Nanosecond()),
		},
	}
}
