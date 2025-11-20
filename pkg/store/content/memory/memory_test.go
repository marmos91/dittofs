package memory

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/store/content"
	contenttesting "github.com/marmos91/dittofs/pkg/store/content/testing"
)

// TestMemoryContentStore runs the complete ContentStore test suite
// against the MemoryContentStore implementation.
func TestMemoryContentStore(t *testing.T) {
	suite := &contenttesting.StoreTestSuite{
		NewStore: func() content.ContentStore {
			store, err := NewMemoryContentStore(context.Background())
			if err != nil {
				t.Fatalf("Failed to create MemoryContentStore: %v", err)
			}
			return store
		},
	}

	suite.Run(t)
}
