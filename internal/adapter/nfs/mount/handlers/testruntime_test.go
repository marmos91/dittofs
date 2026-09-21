package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/testruntime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// The fixture lives in testruntime because all three adapters need it and each
// used to carry its own copy. This keeps the in-package call sites reading as
// they did. Only the share-store form is used here; the plain one is reachable
// as testruntime.New when a test needs to bring its own control-plane store.

func newTestShareRuntime(t *testing.T) (*runtime.Runtime, string) {
	t.Helper()
	return testruntime.NewWithShareStore(t)
}
