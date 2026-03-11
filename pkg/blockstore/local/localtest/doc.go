// Package localtest provides a conformance test suite for local.LocalStore
// implementations.
//
// Any implementation of local.LocalStore can run this suite to verify it
// correctly implements the interface contract covering reads, writes,
// flush, lifecycle, and block state transitions.
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
