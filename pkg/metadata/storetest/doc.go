// Package storetest provides a conformance test suite for metadata store implementations.
//
// The metadata store (badger, persistent and in-memory) should pass these tests.
// The suite verifies that every store implementation satisfies the MetadataStore
// behavioral contract, catching regressions when store code changes.
//
// Usage:
//
//	func TestConformance(t *testing.T) {
//	    storetest.RunConformanceSuite(t, func(t *testing.T) metadata.Store {
//	        return badgertest.NewInMemory(t)
//	    })
//	}
//
// The factory function receives *testing.T so it can call t.TempDir() for
// stores that need filesystem paths (e.g., BadgerDB) and t.Cleanup for teardown.
package storetest
