package storage_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/platform"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
)

// The operating system's storage is implemented on Linux, so the contract
// runs on it here.
func init() {
	targets = append(targets, target{
		name: "platform.OS",
		open: func(t *testing.T, dir string) storage.FS { return platform.NewOS(dir) },
	})
}
