package memory

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// TestStore_Durable verifies the in-memory remote backend reports NOT durable
// by default (#1274) and that SetDurable overrides the type default.
func TestStore_Durable(t *testing.T) {
	s := New()

	var _ block.DurabilityReporter = s

	if s.Durable() {
		t.Fatal("memory remote store should report NOT durable by default")
	}
	s.SetDurable(true)
	if !s.Durable() {
		t.Fatal("SetDurable(true) should make the memory remote store report durable")
	}
}
