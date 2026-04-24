package common

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// BlockStoreRegistry is the narrow interface satisfied implicitly by
// *runtime.Runtime (see pkg/controlplane/runtime/runtime.go:341).
// Declared here (not in runtime) so common/ does not import runtime
// and stays testable with trivial mocks.
type BlockStoreRegistry interface {
	GetBlockStoreForHandle(ctx context.Context, handle metadata.FileHandle) (*engine.BlockStore, error)
}

// ResolveForRead returns the per-share BlockStore for the given handle.
// Read-only resolution — no permission side effects. Handles are opaque
// (CLAUDE.md invariant 3); this helper never inspects the handle bytes.
// A nil registry produces a typed error rather than a panic so mis-wired
// handler tests fail with a readable message.
func ResolveForRead(ctx context.Context, reg BlockStoreRegistry, handle metadata.FileHandle) (*engine.BlockStore, error) {
	if reg == nil {
		return nil, fmt.Errorf("common.ResolveForRead: nil BlockStoreRegistry")
	}
	return reg.GetBlockStoreForHandle(ctx, handle)
}

// ResolveForWrite is the write-path twin of ResolveForRead. Semantically
// identical today; kept as a separate named helper so the call-site seam
// is already in place for Phase 12 (API-01) when the signatures diverge
// to take []BlockRef (see D-12). One edit later, not two.
func ResolveForWrite(ctx context.Context, reg BlockStoreRegistry, handle metadata.FileHandle) (*engine.BlockStore, error) {
	if reg == nil {
		return nil, fmt.Errorf("common.ResolveForWrite: nil BlockStoreRegistry")
	}
	return reg.GetBlockStoreForHandle(ctx, handle)
}
