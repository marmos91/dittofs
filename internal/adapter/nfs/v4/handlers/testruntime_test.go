package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/testruntime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// The fixtures live in testruntime because all three adapters need them and
// each used to carry its own copy. These two keep the in-package call sites
// reading as they did.

func newTestRuntime(t *testing.T, cps store.Store) *runtime.Runtime {
	t.Helper()
	return testruntime.New(t, cps)
}

func newTestShareRuntime(t *testing.T) (*runtime.Runtime, string) {
	t.Helper()
	return testruntime.NewWithShareStore(t)
}
