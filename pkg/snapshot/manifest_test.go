package snapshot_test

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// TestSkeleton_SentinelExists pins the Task 1 sentinel after Tasks 2/3
// add the real test suite.
func TestSkeleton_SentinelExists(t *testing.T) {
	if snapshot.ErrInvalidManifestLine == nil {
		t.Fatal("snapshot.ErrInvalidManifestLine must be a non-nil sentinel")
	}
}

// TestTask2_BasicRoundTrip is the Task 2 RED gate: asserts the three
// public functions exist with the spec signatures and at least one hash
// round-trips. The full suite (round-trip, atomicity, malformed,
// large-set) lands in Task 3.
func TestTask2_BasicRoundTrip(t *testing.T) {
	hs := blockstore.NewHashSet(1)
	var h blockstore.ContentHash
	for i := range h {
		h[i] = byte(i)
	}
	hs.Add(h)

	var buf bytes.Buffer
	if err := snapshot.WriteManifest(&buf, hs); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	got, err := snapshot.ReadManifest(&buf)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.Len() != 1 || !got.Contains(h) {
		t.Fatalf("round-trip lost the hash: len=%d contains=%v", got.Len(), got.Contains(h))
	}
}
