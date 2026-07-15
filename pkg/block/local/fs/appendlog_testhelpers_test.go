package fs

import (
	"os"
	"testing"
	"time"

	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newFSStoreForTest constructs an FSStore in t.TempDir with the given
// options and a memory metadata backend (serving as both the file-chunk
// store and the mandatory LocalChunkIndex). Registers t.Cleanup to Close
// the store. Shared by /05/06/07/09 test files in the fs package.
func newFSStoreForTest(t *testing.T, opts FSStoreOptions) *FSStore {
	t.Helper()
	dir, err := os.MkdirTemp("", "fsstore-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	// The metadata backend is mandatory: it doubles as the LocalChunkIndex for
	// the log-blob substrate. A memory store serves both facets.
	bc, err := NewWithOptions(dir, 1<<30, memmeta.NewMemoryMetadataStoreWithDefaults(), opts)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() {
		_ = bc.Close()
		// On Windows, file handles may linger after Close due to
		// kernel-level delayed release. Retry so cleanup doesn't
		// fail the test for a timing issue.
		for range 5 {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = os.RemoveAll(dir)
	})
	return bc
}
