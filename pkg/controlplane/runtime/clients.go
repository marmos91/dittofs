package runtime

import "github.com/marmos91/dittofs/pkg/controlplane/runtime/clients"

// Type aliases for the clients sub-service, re-exported so callers can use
// the runtime package directly.
type (
	ClientRecord   = clients.ClientRecord
	NfsDetails     = clients.NfsDetails
	SmbDetails     = clients.SmbDetails
	ClientRegistry = clients.Registry
)

// NewClientRegistry creates a registry with the default TTL.
func NewClientRegistry() *ClientRegistry {
	return clients.NewRegistry(clients.DefaultTTL)
}
