package engine

import (
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// The components the root will pass satisfy the interfaces Deps declares, so
// a signature that drifts from its component fails to build here rather than
// at the first composition. The journal's own are checked where it is
// wrapped, in the test fixture.
var (
	_ Existence = kvstore.Existence{}
	_ Lifecycle = kvstore.Lifecycle{}
	_ Content   = kvstore.Content{}
	_ Clock     = journal.Clock(nil)
)
