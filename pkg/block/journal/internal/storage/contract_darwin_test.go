package storage_test

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/platform"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// The operating system's storage is implemented on macOS, so the contract
// runs on it here.
func init() {
	targets = append(targets, target{
		name: "platform.OS",
		open: func(t *testing.T, dir string) storage.FS {
			return platform.NewOS(dir)
		},
	})
}
