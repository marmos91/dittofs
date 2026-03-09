package memory_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/local/localtest"
	"github.com/marmos91/dittofs/pkg/blockstore/local/memory"
)

func TestMemoryStoreConformance(t *testing.T) {
	factory := func(t *testing.T) local.LocalStore {
		t.Helper()
		return memory.New()
	}
	localtest.RunSuite(t, factory)
}
