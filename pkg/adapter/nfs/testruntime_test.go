package nfs

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// newTestRuntime builds a runtime that provisions every share's journal under
// a temp dir. AddShare fails without a journal root.
func newTestRuntime(t *testing.T, cps store.Store) *runtime.Runtime {
	t.Helper()
	rt := runtime.New(cps)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: t.TempDir()})
	return rt
}
