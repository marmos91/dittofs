// Package remotetest provides a conformance test suite for remote.RemoteStore
// implementations.
//
// Any implementation of remote.RemoteStore can run this suite to verify it
// correctly implements the interface contract.
//
// Usage:
//
//	func TestMyStore(t *testing.T) {
//	    factory := func(t *testing.T) remote.RemoteStore {
//	        return mystore.New()
//	    }
//	    remotetest.RunSuite(t, factory)
//	}
package remotetest
