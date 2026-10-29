package nfs

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/testruntime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// The fixtures live in testruntime, shared with the other adapters, so a fix
// to how a share is torn down lands once. These two keep the in-package call
// sites reading as they did.

func newTestRuntime(t *testing.T, cps store.Store) *runtime.Runtime {
	t.Helper()
	return testruntime.New(t, cps)
}

func newTestShareRuntime(t *testing.T) (*runtime.Runtime, string) {
	t.Helper()
	return testruntime.NewWithShareStore(t)
}
