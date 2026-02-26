package memory_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestConformance(t *testing.T) {
	storetest.RunConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
		return memory.NewMemoryMetadataStoreWithDefaults()
	})
}
