package memory_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/memory"
)

// TestMemoryStore_Durable verifies the in-memory local backend reports NOT
// durable by default (#1274) and that SetDurable overrides the type default.
func TestMemoryStore_Durable(t *testing.T) {
	s := memory.New()

	var _ block.DurabilityReporter = s

	if s.Durable() {
		t.Fatal("memory local store should report NOT durable by default")
	}
	s.SetDurable(true)
	if !s.Durable() {
		t.Fatal("SetDurable(true) should make the memory store report durable")
	}
	s.SetDurable(false)
	if s.Durable() {
		t.Fatal("SetDurable(false) should restore NOT durable")
	}
}
