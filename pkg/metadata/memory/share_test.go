package memory

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	metadatatesting "github.com/marmos91/dittofs/pkg/metadata/testing"
)

// TestMemoryMetadataStore runs the complete MetadataStore test suite
// against the MemoryMetadataStore implementation.
func TestMemoryMetadataStore(t *testing.T) {
	suite := &metadatatesting.StoreTestSuite{
		NewStore: func() metadata.MetadataStore {
			return NewMemoryMetadataStoreWithDefaults()
		},
	}

	suite.Run(t)
}
