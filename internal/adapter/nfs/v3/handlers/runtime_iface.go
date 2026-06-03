package handlers

import (
	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// nfsRuntime is the consumer-defined role interface for the NFSv3 handlers.
// It lists exactly the Runtime methods this package depends on — no more —
// so the handlers couple to the behaviour they use rather than to the
// concrete *runtime.Runtime god-object. *runtime.Runtime satisfies it
// implicitly (compile-time assertion below).
type nfsRuntime interface {
	// Per-share block-store resolution (used via common.ResolveFor*).
	common.BlockStoreRegistry

	// Metadata service access.
	GetMetadataService() *metadata.Service

	// Share resolution.
	GetShare(name string) (*runtime.Share, error)

	// Identity / auth.
	GetIdentityStore() models.IdentityStore
	ApplyIdentityMapping(shareName string, ident *metadata.Identity) (*metadata.Identity, error)

	// Adapter-provided extensions (e.g. GSS processor) keyed by name.
	GetAdapterProvider(key string) any
}

var _ nfsRuntime = (*runtime.Runtime)(nil)
