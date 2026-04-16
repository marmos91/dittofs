package restore

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// StoresService is the narrow interface restore needs from
// pkg/controlplane/runtime/stores.Service. Plan 04 shipped the real
// implementation; this interface keeps the restore engine unit-testable
// without pulling the full runtime.
//
// Method semantics:
//
//   - OpenMetadataStoreAtPath constructs a fresh engine instance at the
//     given pathOverride but does NOT register it. Memory ignores the
//     override, Badger uses it as a filesystem path, Postgres uses it as
//     a schema name.
//
//   - SwapMetadataStore atomically replaces the entry for name in the
//     live registry and returns the displaced instance so the caller
//     can close it + clean its backing.
//
//   - DropPostgresSchema issues `DROP SCHEMA <schema> CASCADE` using the
//     connection pool of the live store named originalName. Used by
//     CleanupTempBacking (pre-swap failure) and CommitSwap (post-swap
//     cleanup of the displaced schema).
type StoresService interface {
	OpenMetadataStoreAtPath(ctx context.Context, cfg *models.MetadataStoreConfig, pathOverride string) (metadata.MetadataStore, error)
	SwapMetadataStore(name string, newStore metadata.MetadataStore) (metadata.MetadataStore, error)
	DropPostgresSchema(ctx context.Context, originalName, schemaName string) error
}

// TempIdentity captures everything the restore coordinator needs to
// clean up on failure or commit (rename) on success. Returned by
// OpenFreshEngineAtTemp; consumed by CleanupTempBacking (pre-swap) and
// CommitSwap (post-swap).
//
// Fields:
//   - Kind: engine kind ("memory" | "badger" | "postgres"). Drives
//     the cleanup/commit dispatch.
//   - OriginalName: the stores.Service registry key for the live store.
//     Used by DropPostgresSchema to resolve the connection pool.
//   - OriginalPath: where the new engine must live after a successful
//     CommitSwap (Badger path, Postgres schema). Empty for memory.
//   - TempPath: the transient location (Badger dir, Postgres schema)
//     that OpenMetadataStoreAtPath wrote to. Empty for memory.
//   - ULID: correlation ID for logs + Plan 07's orphan sweep.
type TempIdentity struct {
	Kind         string
	OriginalName string
	OriginalPath string
	TempPath     string
	ULID         string
}

// OpenFreshEngineAtTemp constructs a fresh engine instance at a
// temporary backing location based on cfg.Type. Does NOT register it
// with the runtime — caller (Executor.RunRestore) uses SwapMetadataStore
// after the restore stream is validated.
//
// Per-kind temp layout (D-05 step 6):
//
//   - memory:   in-memory instance; pathOverride is ignored. Empty by
//     construction; Backupable.Restore's "destination must be empty"
//     invariant (Phase 2 D-06) holds trivially.
//
//   - badger:   `<origPath>.restore-<ulid>` sibling directory. The
//     suffix format matches Plan 07's orphan-sweep scanner.
//
//   - postgres: `<origSchema>_restore_<ulid>` on the same connection
//     pool. Schema name is lowercased ULID to satisfy Postgres
//     identifier conventions.
//
// Returns the engine, a TempIdentity for cleanup/commit, and any open
// error. On error, the temp path is best-effort reclaimed (Badger:
// os.RemoveAll; Postgres: DROP SCHEMA via stores.DropPostgresSchema)
// so the caller need not.
func OpenFreshEngineAtTemp(
	ctx context.Context,
	stores StoresService,
	cfg *models.MetadataStoreConfig,
) (metadata.MetadataStore, TempIdentity, error) {
	if cfg == nil {
		return nil, TempIdentity{}, fmt.Errorf("open fresh engine: cfg is nil")
	}
	if cfg.Name == "" {
		return nil, TempIdentity{}, fmt.Errorf("open fresh engine: cfg.Name is empty")
	}

	tempULID := ulid.Make().String()

	switch cfg.Type {
	case "memory":
		store, err := stores.OpenMetadataStoreAtPath(ctx, cfg, "")
		if err != nil {
			return nil, TempIdentity{}, fmt.Errorf("open memory engine: %w", err)
		}
		return store, TempIdentity{
			Kind:         "memory",
			OriginalName: cfg.Name,
			ULID:         tempULID,
		}, nil

	case "badger":
		raw, err := cfg.GetConfig()
		if err != nil {
			return nil, TempIdentity{}, fmt.Errorf("parse badger cfg: %w", err)
		}
		origPath, _ := raw["path"].(string)
		if origPath == "" {
			return nil, TempIdentity{}, fmt.Errorf("badger cfg missing path")
		}
		tempPath := fmt.Sprintf("%s.restore-%s", origPath, tempULID)

		// Defense-in-depth: if the temp path already exists, the caller
		// either has a concurrent restore in flight (should be impossible
		// given Plan 07's overlap guard) or a ULID collision occurred.
		// Surface loudly — do NOT clobber existing data.
		if _, statErr := os.Stat(tempPath); statErr == nil {
			return nil, TempIdentity{}, fmt.Errorf("%w: %s", ErrFreshEngineExists, tempPath)
		}

		store, err := stores.OpenMetadataStoreAtPath(ctx, cfg, tempPath)
		if err != nil {
			// Best-effort reclaim: if OpenMetadataStoreAtPath created the
			// directory but failed mid-open, os.RemoveAll tidies up. If
			// nothing was created, this is a no-op.
			_ = os.RemoveAll(tempPath)
			return nil, TempIdentity{}, fmt.Errorf("open badger engine at %q: %w", tempPath, err)
		}
		return store, TempIdentity{
			Kind:         "badger",
			OriginalName: cfg.Name,
			OriginalPath: origPath,
			TempPath:     tempPath,
			ULID:         tempULID,
		}, nil

	case "postgres":
		raw, err := cfg.GetConfig()
		if err != nil {
			return nil, TempIdentity{}, fmt.Errorf("parse postgres cfg: %w", err)
		}
		origSchema, _ := raw["schema"].(string)
		if origSchema == "" {
			origSchema = "public"
		}
		tempSchema := fmt.Sprintf("%s_restore_%s", origSchema, strings.ToLower(tempULID))

		store, err := stores.OpenMetadataStoreAtPath(ctx, cfg, tempSchema)
		if err != nil {
			// Best-effort drop: if the engine created the schema then
			// failed mid-migration, DROP SCHEMA CASCADE reclaims it. If
			// CREATE SCHEMA never succeeded, DROP with IF EXISTS is a
			// no-op inside DropPostgresSchema.
			_ = stores.DropPostgresSchema(ctx, cfg.Name, tempSchema)
			return nil, TempIdentity{}, fmt.Errorf("open postgres engine at schema %q: %w", tempSchema, err)
		}
		return store, TempIdentity{
			Kind:         "postgres",
			OriginalName: cfg.Name,
			OriginalPath: origSchema,
			TempPath:     tempSchema,
			ULID:         tempULID,
		}, nil

	default:
		return nil, TempIdentity{}, fmt.Errorf("unsupported store type %q", cfg.Type)
	}
}

// CleanupTempBacking removes the temp path/schema after a failed restore.
// Safe on a zero TempIdentity (no-op). Called from Executor.RunRestore's
// defer on every pre-swap failure path.
//
// Per-kind cleanup:
//   - badger:   os.RemoveAll(TempPath). Missing dir is not an error.
//   - postgres: DROP SCHEMA tempSchema CASCADE. Idempotent via IF EXISTS.
//   - memory:   no-op (process-local, GC'd when the caller drops its
//     reference).
//
// Errors are returned so the caller can log them; cleanup failures do
// NOT re-enter the restore state machine.
func CleanupTempBacking(ctx context.Context, stores StoresService, id TempIdentity) error {
	switch id.Kind {
	case "badger":
		if id.TempPath == "" {
			return nil
		}
		return os.RemoveAll(id.TempPath)
	case "postgres":
		if id.TempPath == "" || id.OriginalName == "" {
			return nil
		}
		return stores.DropPostgresSchema(ctx, id.OriginalName, id.TempPath)
	case "memory", "":
		return nil
	default:
		return fmt.Errorf("unknown kind %q in TempIdentity", id.Kind)
	}
}
