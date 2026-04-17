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
//  2. Stage old canonical out of the way (Badger: rename to a sibling
//     `.old-<ulid>` directory; Postgres: rename schema to
//     `<name>_old_<ulid>`). This frees the canonical name.
//  3. Move temp → canonical (Badger: os.Rename; Postgres: RENAME SCHEMA
//     via the stores.Service RenamePostgresSchema extension point;
//     Memory: no-op — step 10 registry swap already made fresh
//     canonical).
//  4. Delete the staged old (Badger: os.RemoveAll; Postgres: DROP
//     SCHEMA CASCADE). Failure here leaves an orphan that Plan 07's
//     startup sweep reclaims.
//
// Ordering rationale (Copilot #384): the original "delete old, then
// rename temp" left the canonical backing missing if the rename step
// failed — a subsequent server restart could not re-open the store.
// Staging old first preserves a recoverable copy until the rename has
// landed; only then do we free its storage.
//
// Errors are returned so the caller (Executor.RunRestore) can log them;
// they do NOT fail the restore — the registry swap at D-05 step 10 has
// already committed and live clients see the new data through the
// in-memory pointer.
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

	// Steps 2-4: per-kind rename-first cleanup.
	switch id.Kind {
	case "badger":
		if id.OriginalPath == "" || id.TempPath == "" {
			return fmt.Errorf("badger TempIdentity missing paths (orig=%q temp=%q)",
				id.OriginalPath, id.TempPath)
		}
		// Step 2: stage old canonical out of the way. Reuses the
		// restore ULID for a unique sibling name; `.old-` prefix is
		// recognised by Plan 07's orphan sweep.
		stagedOld := id.OriginalPath + ".old-" + id.ULID
		if err := os.Rename(id.OriginalPath, stagedOld); err != nil {
			return fmt.Errorf("stage old badger path %q -> %q: %w",
				id.OriginalPath, stagedOld, err)
		}
		// Step 3: promote temp to canonical. Both paths live on the same
		// filesystem (Phase-5 guarantee), so rename is atomic.
		if err := os.Rename(id.TempPath, id.OriginalPath); err != nil {
			// Best-effort: restore old name so the server can recover
			// on next restart. Either outcome is logged by caller.
			_ = os.Rename(stagedOld, id.OriginalPath)
			return fmt.Errorf("rename temp %q -> %q: %w",
				id.TempPath, id.OriginalPath, err)
		}
		// Step 4: free the old backing. Failure = orphan (sweep-friendly).
		if err := os.RemoveAll(stagedOld); err != nil {
			return fmt.Errorf("remove staged old %q: %w", stagedOld, err)
		}
		return nil

	case "postgres":
		if id.OriginalPath == "" || id.TempPath == "" {
			return fmt.Errorf("postgres TempIdentity missing schema names (orig=%q temp=%q)",
				id.OriginalPath, id.TempPath)
		}
		// Step 2: rename old schema to a unique "_old_<ulid>" name to
		// free the canonical name for the promote step.
		stagedOld := id.OriginalPath + "_old_" + id.ULID
		if err := renamePostgresSchema(ctx, stores, id.OriginalName, id.OriginalPath, stagedOld); err != nil {
			return fmt.Errorf("stage old postgres schema %q -> %q: %w",
				id.OriginalPath, stagedOld, err)
		}
		// Step 3: promote temp schema to canonical.
		if err := renamePostgresSchema(ctx, stores, id.OriginalName, id.TempPath, id.OriginalPath); err != nil {
			// Best-effort rollback so the canonical name is recoverable.
			_ = renamePostgresSchema(ctx, stores, id.OriginalName, stagedOld, id.OriginalPath)
			return fmt.Errorf("rename temp postgres schema %q -> %q: %w",
				id.TempPath, id.OriginalPath, err)
		}
		// Step 4: drop the staged-old schema. Failure = orphan (sweep).
		if err := stores.DropPostgresSchema(ctx, id.OriginalName, stagedOld); err != nil {
			return fmt.Errorf("drop staged-old postgres schema %q: %w", stagedOld, err)
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
