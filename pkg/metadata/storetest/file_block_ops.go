package storetest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	bsstoretest "github.com/marmos91/dittofs/pkg/blockstore/storetest"
)

// runFileBlockOpsTests delegates to the blockstore/storetest conformance suite.
// This thin wrapper adapts the MetadataStore factory to a FileBlockStore factory
// so existing conformance suite callers continue to work without changes.
func runFileBlockOpsTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	bsstoretest.RunFileBlockStoreConformance(t, func(t *testing.T) blockstore.FileBlockStore {
		return factory(t)
	})
}
