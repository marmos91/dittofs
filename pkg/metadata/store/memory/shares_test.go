package memory_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// TestBlockLayoutConformance runs the per-share BlockLayout conformance
// scenarios against the in-memory metadata store.
func TestBlockLayoutConformance(t *testing.T) {
	storetest.RunBlockLayoutSuite(t, func(t *testing.T) metadata.Store {
		return memory.NewMemoryMetadataStoreWithDefaults()
	})
}
