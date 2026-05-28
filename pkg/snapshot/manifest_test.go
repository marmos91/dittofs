package snapshot_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/snapshot"
)

// TestSkeleton_SentinelExists is the Task 1 RED gate: asserts the
// ErrInvalidManifestLine package-level sentinel is declared and non-nil
// before any of the Task 2/3 functionality lands. Task 3 replaces this
// with the full test suite.
func TestSkeleton_SentinelExists(t *testing.T) {
	if snapshot.ErrInvalidManifestLine == nil {
		t.Fatal("snapshot.ErrInvalidManifestLine must be a non-nil sentinel")
	}
	if got := snapshot.ErrInvalidManifestLine.Error(); got == "" {
		t.Fatalf("sentinel Error() must be non-empty, got %q", got)
	}
}
