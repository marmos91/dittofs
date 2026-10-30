package platform

import (
	"errors"
	"runtime"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// TestAllocateNotImplemented checks Allocate on an open file is still not
// implemented on Linux, where allocation on the operating system (RFC 28
// Appendix A, "Allocation, per platform") is still to be written.
func TestAllocateNotImplemented(t *testing.T) {
	var f storage.File
	t.Cleanup(func() {
		runtime.KeepAlive(f)
	})
	prof.Watch(t, prof.Options{})

	f, err := NewOS(t.TempDir()).Create("a")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := f.Allocate(0, 1); !errors.Is(err, core.ErrNotImplemented) {
		t.Errorf("Allocate on an open file: %v, want core.ErrNotImplemented", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
