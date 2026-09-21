package handlers

import (
	"testing"

	smbtesting "github.com/marmos91/dittofs/internal/adapter/smb/handlers/testing"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// The fixtures themselves live in the testing subpackage so a handler concern
// can take its tests with it when it moves out of this package. These two keep
// the in-package call sites reading as they did.

func newTestRuntime(t *testing.T, cps store.Store) *runtime.Runtime {
	t.Helper()
	return smbtesting.NewRuntime(t, cps)
}

func newTestShareRuntime(t *testing.T) (*runtime.Runtime, string) {
	t.Helper()
	return smbtesting.NewShareRuntime(t)
}
