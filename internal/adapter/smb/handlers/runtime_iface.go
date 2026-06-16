package handlers

import (
	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metrics"
)

// smbRuntime is the consumer-defined role interface for the SMB handlers.
// It lists exactly the Runtime methods this package depends on — no more —
// so the handlers couple to the behaviour they use rather than to the
// concrete *runtime.Runtime god-object. *runtime.Runtime satisfies it
// implicitly (compile-time assertion below).
type smbRuntime interface {
	// Per-share block-store resolution (used via common.ResolveFor* and
	// directly for FLUSH / copychunk / scavenger paths).
	common.BlockStoreRegistry

	// Metadata service access.
	GetMetadataService() *metadata.Service

	// Share + handle resolution.
	GetShare(name string) (*runtime.Share, error)
	GetRootHandle(shareName string) (metadata.FileHandle, error)
	ListShares() []string

	// Share-change notification (tree-connect cache invalidation).
	OnShareChange(callback func(shares []string)) func()

	// Identity / auth backends.
	GetUserStore() models.UserStore
	GetIdentityMappingStore() store.IdentityMappingStore

	// Metrics returns the inline metrics sink (may be nil; all Record* methods
	// are nil-receiver safe). SESSION_SETUP uses it to record SMB auth attempts.
	Metrics() *metrics.Metrics
}

var _ smbRuntime = (*runtime.Runtime)(nil)
