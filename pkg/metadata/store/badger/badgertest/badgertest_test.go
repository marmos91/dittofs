package badgertest_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// The conformance suites run over the in-memory mode here; the on-disk mode
// runs them in the badger package itself.

func TestConformance(t *testing.T) {
	storetest.RunConformanceSuite(t, func(t *testing.T) metadata.Store { return badgertest.NewInMemory(t) })
}

func TestSnapshotConformance(t *testing.T) {
	storetest.RunSnapshotConformanceSuite(t, func(t *testing.T) metadata.Store { return badgertest.NewInMemory(t) })
}

func TestResetThenRestoreConformance(t *testing.T) {
	storetest.ResetThenRestoreConformance(t, func(t *testing.T) metadata.Store { return badgertest.NewInMemory(t) })
}

func TestLockPersistenceConformance(t *testing.T) {
	storetest.RunLockPersistenceSuite(t, func(t *testing.T) lock.LockStore { return badgertest.NewInMemory(t) })
}
