package adapter_test

import (
	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/adapter/nfs"
	"github.com/marmos91/dittofs/pkg/adapter/smb"
)

// Compile-time assertions that both concrete adapters satisfy adapter.Adapter.
// If SetRuntime regains a *runtime.Runtime parameter (or any other method
// drifts), the build fails here.
var (
	_ adapter.Adapter = (*nfs.NFSAdapter)(nil)
	_ adapter.Adapter = (*smb.Adapter)(nil)
)
