package s3

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// TestStore_Durable verifies the s3 remote backend reports durable by default
// (#1274) and that SetDurable overrides the type default. Durable() does not
// touch the s3 client, so a nil client is fine here.
func TestStore_Durable(t *testing.T) {
	s := New(nil, Config{Bucket: "b"})

	var _ block.DurabilityReporter = s

	if !s.Durable() {
		t.Fatal("s3 remote store should report durable by default")
	}
	s.SetDurable(false)
	if s.Durable() {
		t.Fatal("SetDurable(false) should make the s3 store report NOT durable")
	}
}
