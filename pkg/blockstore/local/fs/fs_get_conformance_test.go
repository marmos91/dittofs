package fs_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/local/localtest"
)

// TestFSStore_GetConformance wires the LocalStore.Get conformance suite
// against *fs.FSStore. The FS-backed store satisfies the optional
// chunkStorer capability inside RunGetSuite (it implements StoreChunk),
// so the round-trip + fresh-allocation defenses run in addition to the
// missing-hash sentinel assertion.
func TestFSStore_GetConformance(t *testing.T) {
	factory := func(t *testing.T) local.LocalStore {
		t.Helper()
		dir := t.TempDir()
		bc, err := fs.NewWithOptions(dir, 1<<30, 1<<30, nil, fs.FSStoreOptions{
			UseAppendLog: true,
			MaxLogBytes:  1 << 30,
		})
		if err != nil {
			t.Fatalf("NewWithOptions: %v", err)
		}
		t.Cleanup(func() { _ = bc.Close() })
		return bc
	}
	localtest.RunGetSuite(t, factory)
}
