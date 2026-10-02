// Package journaltest opens throwaway journal stores for tests that live
// outside pkg/block/journal and need a real local tier rather than a stand-in.
package journaltest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journal"
)

// New opens a journal store rooted in a fresh temp dir, taking the store's own
// defaults, and closes it when the test ends. Cleanups run last-registered
// first, so the close lands before the temp dir is removed — which is what lets
// the removal succeed on platforms that refuse to unlink an open file.
func New(tb testing.TB) *journal.Store {
	tb.Helper()
	s, err := journal.Open(tb.TempDir(), journal.Config{})
	if err != nil {
		tb.Fatalf("journal.Open: %v", err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	return s
}
