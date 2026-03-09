// Package localtest provides a conformance test suite for local.LocalStore
// implementations.
//
// Any implementation of local.LocalStore can run this suite to verify it
// correctly implements the interface contract. The suite tests all four
// sub-interfaces (LocalReader, LocalWriter, LocalFlusher, LocalManager)
// with common scenarios.
//
// Usage:
//
//	func TestMyStore(t *testing.T) {
//	    factory := func(t *testing.T) local.LocalStore {
//	        return mystore.New()
//	    }
//	    localtest.RunSuite(t, factory)
//	}
package localtest
