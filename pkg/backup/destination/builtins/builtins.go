// Package builtins wires the two built-in destination drivers (fs, s3) into
// the destination.Registry. Callers (cmd/dfs/main.go during Phase 6 wiring)
// import this package once at startup and invoke RegisterBuiltins.
//
// This package has NO init() function by design — explicit registration at
// main avoids init-order surprises across the module graph. See
// .planning/phases/03-destination-drivers-encryption/03-PATTERNS.md
// ("No Analog Found" section) for the rationale.
package builtins

import (
	"context"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	destfs "github.com/marmos91/dittofs/pkg/backup/destination/fs"
	dests3 "github.com/marmos91/dittofs/pkg/backup/destination/s3"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// RegisterBuiltins registers the "local" (filesystem) and "s3" drivers into
// destination.Registry using the typed models.BackupRepoKind constants so
// callers pass repo.Kind directly. Panics on duplicate registration
// (programmer error, not operator error).
//
// Call exactly once, at process startup, before any BackupRepo is loaded.
func RegisterBuiltins() {
	destination.Register(models.BackupRepoKindLocal, localFactory)
	destination.Register(models.BackupRepoKindS3, s3Factory)
}

// localFactory adapts destfs.New to the destination.Factory signature.
func localFactory(ctx context.Context, repo *models.BackupRepo) (destination.Destination, error) {
	return destfs.New(ctx, repo)
}

// s3Factory adapts dests3.New (variadic options) to destination.Factory.
// Callers who need blockStoreLister injection for the D-13 collision check
// can either (a) wrap this factory at a higher layer (Phase 6 API handler)
// or (b) set the lister post-construction via the exported With* options.
// Plan 06 covers end-to-end usage.
func s3Factory(ctx context.Context, repo *models.BackupRepo) (destination.Destination, error) {
	return dests3.New(ctx, repo)
}
