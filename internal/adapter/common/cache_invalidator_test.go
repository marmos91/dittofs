package common

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// recordingInvalidator captures InvalidateFile calls so tests can assert
// invocation count + arguments. Used by Task 1/Task 2 wiring tests.
type recordingInvalidator struct {
	calls []invalidatorCall
}

type invalidatorCall struct {
	payloadID metadata.PayloadID
	removed   []block.ContentHash
}

func (r *recordingInvalidator) InvalidateFile(payloadID metadata.PayloadID, removed []block.ContentHash) {
	r.calls = append(r.calls, invalidatorCall{payloadID: payloadID, removed: removed})
}

// TestCacheInvalidatorInterface_Compiles is a compile-time + behavioural
// guarantee that the CacheInvalidator interface (seam for
// CACHE-05 post-txn invalidation) accepts a payloadID + removedHashes
// pair. The interface is consumed by cache rewrite when the
// engine.Cache type implements it; this plan defines the surface.
func TestCacheInvalidatorInterface_Compiles(t *testing.T) {
	var inv CacheInvalidator = &recordingInvalidator{}
	hashes := []block.ContentHash{{0x01}, {0x02}}
	inv.InvalidateFile(metadata.PayloadID("test"), hashes)

	got := inv.(*recordingInvalidator)
	if len(got.calls) != 1 {
		t.Fatalf("expected 1 InvalidateFile call, got %d", len(got.calls))
	}
	if got.calls[0].payloadID != metadata.PayloadID("test") {
		t.Errorf("got payloadID %q, want %q", got.calls[0].payloadID, "test")
	}
	if len(got.calls[0].removed) != 2 {
		t.Errorf("got %d removed hashes, want 2", len(got.calls[0].removed))
	}
}
