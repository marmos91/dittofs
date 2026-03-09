// Package storetest provides a conformance test suite for FileBlockStore implementations.
//
// All FileBlockStore backends (memory, badger, postgres) should pass these tests.
// The suite verifies that every implementation satisfies the FileBlockStore
// behavioral contract, catching regressions when store code changes.
//
// Usage:
//
//	func TestConformance(t *testing.T) {
//	    storetest.RunFileBlockStoreConformance(t, func(t *testing.T) blockstore.FileBlockStore {
//	        return createTestStore(t)
//	    })
//	}
//
// The factory function receives *testing.T so it can call t.TempDir() for
// stores that need filesystem paths and t.Cleanup for teardown.
package storetest
