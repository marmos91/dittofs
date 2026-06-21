package fs

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestFSStore_Durable verifies the fs local backend reports durable by default
// (#1274) and that SetDurable overrides the type default.
func TestFSStore_Durable(t *testing.T) {
	mds := memory.NewMemoryMetadataStoreWithDefaults()
	bc, err := NewWithOptions(t.TempDir(), 0, mds, FSStoreOptions{})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })

	// Type default: fs is durable.
	if !bc.Durable() {
		t.Fatal("fs local store should report durable by default")
	}

	// Capability is wired.
	var _ block.DurabilityReporter = bc

	// Override flips it (e.g. a tmpfs-backed share).
	bc.SetDurable(false)
	if bc.Durable() {
		t.Fatal("SetDurable(false) should make the fs store report NOT durable")
	}
	bc.SetDurable(true)
	if !bc.Durable() {
		t.Fatal("SetDurable(true) should restore durable")
	}
}
