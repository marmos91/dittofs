// Package badgertest opens Badger metadata stores for tests.
package badgertest

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// NewInMemory opens an in-memory Badger metadata store with the default
// capabilities and closes it when tb finishes.
func NewInMemory(tb testing.TB) *badger.BadgerMetadataStore {
	tb.Helper()
	s, err := badger.NewInMemoryBadgerMetadataStore(context.Background())
	if err != nil {
		tb.Fatalf("open in-memory badger metadata store: %v", err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	return s
}
