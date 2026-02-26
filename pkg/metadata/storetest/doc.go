// Package storetest provides a conformance test suite for metadata store implementations.
//
// All metadata store backends (memory, badger, postgres) should pass these tests.
// The suite verifies that every store implementation satisfies the MetadataStore
// behavioral contract, catching regressions when store code changes.
//
// Usage:
//
//	func TestConformance(t *testing.T) {
//	    storetest.RunConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
//	        return memory.NewMemoryMetadataStoreWithDefaults()
//	    })
//	}
//
// The factory function receives *testing.T so it can call t.TempDir() for
// stores that need filesystem paths (e.g., BadgerDB) and t.Cleanup for teardown.
package storetest
