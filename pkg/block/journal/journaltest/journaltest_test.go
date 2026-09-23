package journaltest

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal"
)

// The helper's whole contract is "hand back a usable journal store whose
// lifecycle ends with the test." A caller that trusted it for anything else
// would be trusting the store's own tests to cover this package, which is the
// coupling this package exists to avoid — so pin the two halves that callers
// actually rely on: the store is open and serves bytes, and it is not left
// open once the test ends.

func TestNew_ReturnsUsableStore(t *testing.T) {
	s := New(t)

	if s.Closed() {
		t.Fatal("New returned an already-closed store")
	}

	ctx := context.Background()
	id := journal.FileID("journaltest-probe")
	data := []byte("hello journal")
	if err := s.WriteAt(ctx, id, 0, data); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	dst := make([]byte, len(data))
	n, _, err := s.ReadAt(ctx, id, 0, dst)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(data) || string(dst) != string(data) {
		t.Fatalf("ReadAt = (%d, %q), want (%d, %q)", n, dst, len(data), data)
	}
}

// TestNew_ClosesAtCleanup proves the store is closed by the helper rather than
// left to the caller: a subtest's cleanups run when it returns, so by the time
// the outer body resumes the store the subtest opened must be closed. If the
// helper forgot its tb.Cleanup, the store would still be open here.
func TestNew_ClosesAtCleanup(t *testing.T) {
	var s *journal.Store
	t.Run("inner", func(t *testing.T) {
		s = New(t)
		if s.Closed() {
			t.Fatal("store was closed before the subtest ended")
		}
	})

	if !s.Closed() {
		t.Fatal("New did not close its store when the test ended")
	}
}
