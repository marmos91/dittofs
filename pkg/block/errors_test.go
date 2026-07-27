package block_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// TestErrStopWalk_DetectsThroughWrap pins the Walk callback contract
// callbacks wrap ErrStopWalk with fmt.Errorf("...: %w", ...)
// and Walk implementations match via errors.Is.
func TestErrStopWalk_DetectsThroughWrap(t *testing.T) {
	wrapped := fmt.Errorf("gc found target deadbeef: %w", block.ErrStopWalk)
	if !errors.Is(wrapped, block.ErrStopWalk) {
		t.Fatalf("errors.Is should detect ErrStopWalk through fmt.Errorf wrap; got %v", wrapped)
	}
	if errors.Is(wrapped, block.ErrFutureFormat) {
		t.Fatalf("errors.Is must not cross-match unrelated sentinels")
	}
}

// TestErrFutureFormat_DetectsThroughWrap pins the boot-guard contract: a
// store wraps the sentinel with the path and the two format versions via
// fmt.Errorf("%w: ...", ...) and cmd/dfs/start matches via errors.Is.
func TestErrFutureFormat_DetectsThroughWrap(t *testing.T) {
	wrapped := fmt.Errorf("%w: share path %s is format v3, this build reads up to v2",
		block.ErrFutureFormat, "/data/share-a")
	if !errors.Is(wrapped, block.ErrFutureFormat) {
		t.Fatalf("errors.Is should detect ErrFutureFormat through fmt.Errorf wrap; got %v", wrapped)
	}
	// The wrapped error must carry the share path in its message — the
	// boot directive surfaces this verbatim to the operator.
	if !contains(wrapped.Error(), "/data/share-a") {
		t.Fatalf("wrapped error must carry the share path, got %q", wrapped.Error())
	}
}

// TestSentinelMessages_HaveBlockstorePrefix asserts the existing
// convention from pkg/block/errors.go (every package sentinel
// message starts with "blockstore:").
func TestSentinelMessages_HaveBlockstorePrefix(t *testing.T) {
	for _, e := range []error{
		block.ErrStopWalk,
	} {
		if !contains(e.Error(), "blockstore:") {
			t.Errorf("sentinel %v must use the blockstore: prefix convention", e)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
