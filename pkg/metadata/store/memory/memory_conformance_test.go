package memory_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestConformance(t *testing.T) {
	storetest.RunConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
		return memory.NewMemoryMetadataStoreWithDefaults()
	})
}

func TestBackupConformance(t *testing.T) {
	storetest.RunBackupConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
		return memory.NewMemoryMetadataStoreWithDefaults()
	})
}

func TestResetThenRestoreConformance(t *testing.T) {
	storetest.ResetThenRestoreConformance(t, func(t *testing.T) metadata.MetadataStore {
		return memory.NewMemoryMetadataStoreWithDefaults()
	})
}

func TestLockPersistenceConformance(t *testing.T) {
	storetest.RunLockPersistenceSuite(t, func(t *testing.T) lock.LockStore {
		return memory.NewMemoryMetadataStoreWithDefaults()
	})
}
