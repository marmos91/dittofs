package enginev2

import (
	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// The components the root will pass satisfy the interfaces Deps declares, so
// a signature that drifts from its component fails to build here rather than
// at the first composition. The journal's own are checked where it is
// wrapped, in the test fixture.
var (
	_ Existence = kvstore.Existence{}
	_ Content   = kvstore.Content{}
	_ Clock     = journalv2.Clock(nil)
)
