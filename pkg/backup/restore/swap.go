package restore

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// CommitSwap finalizes a successful store swap (D-05 steps 11-12):
//
//  1. Close the old store (if it implements io.Closer).
//  2. Remove the old backing (Badger: os.RemoveAll; Postgres: DROP
//     SCHEMA via stores.DropPostgresSchema; Memory: no-op).
//  3. Rename temp → canonical (Badger: os.Rename; Postgres: RENAME
//     SCHEMA via the stores.Service RenamePostgresSchema extension
//     point; Memory: no-op — the registry swap in step 10 already made
//     the fresh instance canonical).
//
// Errors are returned so the caller (Executor.RunRestore) can log them;
// they do NOT fail the restore since the registry swap at D-05 step 10
// has already committed and clients see the new data. Orphan temp paths
// / residual old backing are reclaimed by Plan 07's startup orphan
// sweep.
func CommitSwap(
	ctx context.Context,
	stores StoresService,
	oldStore metadata.MetadataStore,
	id TempIdentity,
) error {
	// Step 1: close the displaced engine. Memory stores don't implement
	// io.Closer (nothing to release); Badger + Postgres do.
	if closer, ok := oldStore.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			return fmt.Errorf("close old store: %w", err)
		}
	}

	// Steps 2 + 3: per-kind post-swap cleanup and rename.
	switch id.Kind {
	case "badger":
		if id.OriginalPath == "" || id.TempPath == "" {
			return fmt.Errorf("badger TempIdentity missing paths (orig=%q temp=%q)",
				id.OriginalPath, id.TempPath)
		}
		// Remove the old canonical directory — the old store was just
		// closed above so no locks remain. RemoveAll is idempotent for
		// a missing directory.
		if err := os.RemoveAll(id.OriginalPath); err != nil {
			return fmt.Errorf("remove old badger path %q: %w", id.OriginalPath, err)
		}
		// Rename the temp directory into place. os.Rename is atomic on
		// the same filesystem (Phase-5 guarantee: temp lives sibling to
		// canonical).
		if err := os.Rename(id.TempPath, id.OriginalPath); err != nil {
			return fmt.Errorf("rename temp %q -> %q: %w", id.TempPath, id.OriginalPath, err)
		}
		return nil

	case "postgres":
		if id.OriginalPath == "" || id.TempPath == "" {
			return fmt.Errorf("postgres TempIdentity missing schema names (orig=%q temp=%q)",
				id.OriginalPath, id.TempPath)
		}
		// Drop the old schema (free its storage) then rename temp →
		// canonical. DropPostgresSchema is CASCADE + IF EXISTS, so a
		// missing schema is tolerated.
		if err := stores.DropPostgresSchema(ctx, id.OriginalName, id.OriginalPath); err != nil {
			return fmt.Errorf("drop old postgres schema %q: %w", id.OriginalPath, err)
		}
		if err := renamePostgresSchema(ctx, stores, id.OriginalName, id.TempPath, id.OriginalPath); err != nil {
			return fmt.Errorf("rename temp postgres schema %q -> %q: %w",
				id.TempPath, id.OriginalPath, err)
		}
		return nil

	case "memory":
		// Memory stores have no persistent backing. The registry swap
		// in D-05 step 10 already made the fresh instance canonical;
		// the displaced oldStore will be GC'd when nothing references
		// it. No rename primitive needed.
		return nil

	default:
		return fmt.Errorf("unknown kind %q in TempIdentity", id.Kind)
	}
}

// renamePostgresSchema is an extension point: if the stores.Service
// concrete type exposes a RenamePostgresSchema method (ALTER SCHEMA old
// RENAME TO new), we call it. If not — the concrete implementation has
// not yet shipped the rename primitive — we return a clear error so the
// caller logs it. In that case the restored data remains live under the
// temp schema name; operator can follow up with a manual rename or
// Plan 07's orphan sweep tidies up after the grace window.
//
// Plan 04 shipped DropPostgresSchema as a method on stores.Service;
// rename is deferred to a follow-up iteration of that plan. Scope-
// flexible per plan guidance: if Plan 07 surfaces the need, the
// stores.Service interface can be extended without touching the
// restore engine.
func renamePostgresSchema(ctx context.Context, stores StoresService, liveName, oldSchema, newSchema string) error {
	if r, ok := stores.(interface {
		RenamePostgresSchema(ctx context.Context, liveName, oldSchema, newSchema string) error
	}); ok {
		return r.RenamePostgresSchema(ctx, liveName, oldSchema, newSchema)
	}
	return fmt.Errorf("RenamePostgresSchema not supported by stores.Service; "+
		"restored data lives under temp schema %q (rename required manually or via Plan 07 sweep)",
		oldSchema)
}
