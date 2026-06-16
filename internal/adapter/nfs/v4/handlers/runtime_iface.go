package handlers

import (
	"context"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// nfsRuntime is the consumer-defined role interface for the NFSv4 handlers.
// It lists exactly the Runtime methods this package depends on — no more —
// so the handlers couple to the behaviour they use rather than to the
// concrete *runtime.Runtime god-object. *runtime.Runtime satisfies it
// implicitly (compile-time assertion below).
//
// It is kept separate from the NFSv3 interface because the two protocols use
// overlapping but distinct method sets (v4 needs root-handle resolution,
// handle→share routing and live settings; v3 needs the adapter-provider hook).
type nfsRuntime interface {
	// Per-share block-store resolution (used via common.ResolveFor*).
	common.BlockStoreRegistry

	// Metadata service access.
	GetMetadataService() *metadata.Service

	// Share + handle resolution.
	GetShare(name string) (*runtime.Share, error)
	GetRootHandle(shareName string) (metadata.FileHandle, error)
	GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error)

	// Identity / auth.
	GetIdentityStore() models.IdentityStore
	ApplyIdentityMapping(shareName string, ident *metadata.Identity) (*metadata.Identity, error)
}

var _ nfsRuntime = (*runtime.Runtime)(nil)
