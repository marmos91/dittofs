package platform

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal/internal/storage"
	"github.com/marmos91/dittofs/pkg/block/journal/journaltest/prof"
)

// closedAllocateOp is the Op of the error Allocate returns on a closed file:
// the call it would have made first.
const closedAllocateOp = "fallocate"

// TestAllocateOpenFile checks Allocate on an open file allocates through the
// table. TestClosedFile checks it on a closed one on both systems.
func TestAllocateOpenFile(t *testing.T) {
	var f storage.File
	t.Cleanup(func() {
		runtime.KeepAlive(f)
	})
	prof.Watch(t, prof.Options{})

	dir := t.TempDir()
	r := &recorder{}
	f, err := newOS(dir, r.table()).Create("a")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = f.Allocate(0, 1)
	expectAllocate(t, r.take(), filepath.Join(dir, "a"), 0, 1, 1)

	if err != nil {
		t.Fatalf("Allocate on an open file: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
