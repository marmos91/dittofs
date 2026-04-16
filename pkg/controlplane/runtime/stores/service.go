package stores

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/pathutil"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// Service manages named metadata store instances.
type Service struct {
	mu       sync.RWMutex
	registry map[string]metadata.MetadataStore
}

func New() *Service {
	return &Service{
		registry: make(map[string]metadata.MetadataStore),
	}
}

func (s *Service) RegisterMetadataStore(name string, metaStore metadata.MetadataStore) error {
	if metaStore == nil {
		return fmt.Errorf("cannot register nil metadata store")
	}
	if name == "" {
		return fmt.Errorf("cannot register metadata store with empty name")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.registry[name]; exists {
		return fmt.Errorf("metadata store %q already registered", name)
	}

	s.registry[name] = metaStore
	return nil
}

func (s *Service) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	metaStore, exists := s.registry[name]
	if !exists {
		return nil, fmt.Errorf("metadata store %q not found", name)
	}
	return metaStore, nil
}

func (s *Service) ListMetadataStores() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.registry))
	for name := range s.registry {
		names = append(names, name)
	}
	return names
}

func (s *Service) CountMetadataStores() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.registry)
}

// CloseMetadataStores closes all stores that implement io.Closer.
func (s *Service) CloseMetadataStores() {
	s.mu.RLock()
	snapshot := make(map[string]metadata.MetadataStore, len(s.registry))
	maps.Copy(snapshot, s.registry)
	s.mu.RUnlock()

	for name, store := range snapshot {
		if closer, ok := store.(io.Closer); ok {
			logger.Debug("Closing metadata store", "store", name)
			if err := closer.Close(); err != nil {
				logger.Error("Metadata store close error", "store", name, "error", err)
			}
		}
	}
}

// SwapMetadataStore atomically replaces the store registered under name with
// newStore and returns the displaced instance so the caller can Close() it
// and clean up its backing path/schema.
//
// The write lock makes this the commit point for Phase-5's in-place restore
// (D-05 step 10 / D-23): concurrent readers see either the old or the new
// store — never a nil or half-initialized entry.
//
// This method does NOT call Close() on the displaced store — the caller
// (Phase-5 CommitSwap) owns the close + backing-path cleanup so it can
// coordinate schema drops, directory renames, or deferred disposal without
// the registry getting involved.
//
// Errors:
//   - "cannot swap to nil metadata store" — newStore is nil.
//   - "cannot swap metadata store with empty name" — name is empty.
//   - "metadata store %q not registered" — no entry for name.
func (s *Service) SwapMetadataStore(name string, newStore metadata.MetadataStore) (metadata.MetadataStore, error) {
	if newStore == nil {
		return nil, fmt.Errorf("cannot swap to nil metadata store")
	}
	if name == "" {
		return nil, fmt.Errorf("cannot swap metadata store with empty name")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	old, exists := s.registry[name]
	if !exists {
		return nil, fmt.Errorf("metadata store %q not registered", name)
	}
	s.registry[name] = newStore

	logger.Info("Metadata store swapped",
		"name", name,
		"old_type", fmt.Sprintf("%T", old),
		"new_type", fmt.Sprintf("%T", newStore),
	)
	return old, nil
}

// OpenMetadataStoreAtPath constructs a fresh engine instance using cfg but
// overrides the backing path/schema per pathOverride. The returned engine is
// NOT registered; callers (Phase-5 restore orchestrator) use
// SwapMetadataStore to install it at commit time.
//
// pathOverride semantics per cfg.Type (D-08):
//   - "memory":   pathOverride is ignored; always a fresh in-memory instance.
//   - "badger":   pathOverride is the filesystem backing directory.
//   - "postgres": pathOverride is the target schema name (same connection
//     pool config as cfg).
//
// Errors:
//   - cfg == nil                     — programming error, fail fast.
//   - unsupported cfg.Type           — unknown engine kind.
//   - empty pathOverride for badger  — filesystem engines cannot share state.
//   - empty pathOverride for postgres — schema name must be caller-supplied.
//   - engine-specific open failures  — wrapped with the path/schema for
//     operator diagnosis.
//
// The caller owns cleanup of pathOverride on failure (deleting the temp
// directory, dropping the temp schema). OpenMetadataStoreAtPath never
// mutates the registry.
func (s *Service) OpenMetadataStoreAtPath(
	ctx context.Context,
	cfg *models.MetadataStoreConfig,
	pathOverride string,
) (metadata.MetadataStore, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cannot open metadata store: cfg is nil")
	}

	switch cfg.Type {
	case "memory":
		// Memory engine has no persistent backing; pathOverride is ignored
		// by design. Construction always succeeds and always returns an
		// empty store (Phase-2 D-06 "destination must be empty" holds by
		// construction).
		return memory.NewMemoryMetadataStoreWithDefaults(), nil

	case "badger":
		if pathOverride == "" {
			return nil, fmt.Errorf("badger engine requires non-empty pathOverride")
		}
		dbPath, err := pathutil.ExpandPath(pathOverride)
		if err != nil {
			return nil, fmt.Errorf("expand badger pathOverride %q: %w", pathOverride, err)
		}
		store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
		if err != nil {
			return nil, fmt.Errorf("open badger engine at %q: %w", dbPath, err)
		}
		return store, nil

	case "postgres":
		if pathOverride == "" {
			return nil, fmt.Errorf("postgres engine requires non-empty pathOverride (schema name)")
		}
		store, err := openPostgresAtSchema(ctx, cfg, pathOverride)
		if err != nil {
			return nil, fmt.Errorf("open postgres engine at schema %q: %w", pathOverride, err)
		}
		return store, nil

	default:
		return nil, fmt.Errorf("unsupported metadata store type %q", cfg.Type)
	}
}

// PostgresRestoreOrphan represents one leftover restore-temp schema
// discovered by ListPostgresRestoreOrphans. Name is the schema name as it
// appears in information_schema.schemata; CreatedAt is derived from the
// ULID suffix embedded in the schema name (Phase-5 temp schemas use the
// form `<origSchema>_restore_<ulid>`).
type PostgresRestoreOrphan struct {
	Name      string
	CreatedAt time.Time
}

// ListPostgresRestoreOrphans enumerates schemas on the same connection as
// the live Postgres store named originalName that match the Phase-5 restore
// temp-schema naming convention: `<origSchema>_restore_<ulid>`. The caller
// (Phase-5 orphan sweep) passes the full prefix including the `_restore_`
// separator.
//
// This method is REQUIRED (not optional) for Phase-5 D-14 Postgres orphan
// sweep: if the registered store does not implement schema enumeration, the
// method returns a clear error rather than silently degrading. Silent
// skip-on-type-mismatch would let orphan schemas accumulate indefinitely
// on restart after a crash-interrupted restore — operator never sees a
// warning, GC never runs, eventually the database fills up.
//
// Returns: a non-nil slice (empty when there are no matches) and error on
// connection or permission failures.
//
// Errors:
//   - originalName not registered — wraps the GetMetadataStore error.
//   - registered store does not expose ListSchemasByPrefix — returns a
//     clear error pointing at the type (e.g. memory / badger can never
//     sweep Postgres orphans; callers that dispatch on engine kind should
//     not hit this branch).
//   - enumeration query failure — propagated verbatim from the engine.
func (s *Service) ListPostgresRestoreOrphans(
	ctx context.Context,
	originalName string,
	schemaPrefix string,
) ([]PostgresRestoreOrphan, error) {
	live, err := s.GetMetadataStore(originalName)
	if err != nil {
		return nil, fmt.Errorf("resolve live store for orphan listing: %w", err)
	}

	lister, ok := live.(interface {
		ListSchemasByPrefix(ctx context.Context, prefix string) ([]postgres.RestoreOrphan, error)
	})
	if !ok {
		return nil, fmt.Errorf(
			"store %q does not support schema enumeration (not Postgres?) — "+
				"cannot sweep restore orphans", originalName)
	}

	raw, err := lister.ListSchemasByPrefix(ctx, schemaPrefix)
	if err != nil {
		return nil, fmt.Errorf("list schemas by prefix %q: %w", schemaPrefix, err)
	}

	orphans := make([]PostgresRestoreOrphan, 0, len(raw))
	for _, r := range raw {
		orphans = append(orphans, PostgresRestoreOrphan{
			Name:      r.Name,
			CreatedAt: r.CreatedAt,
		})
	}
	return orphans, nil
}

// DropPostgresSchema issues `DROP SCHEMA <schemaName> CASCADE IF EXISTS`
// using the connection pool of the live Postgres store named originalName.
// Used by Phase-5 restore's CommitSwap after a successful swap to reclaim
// the displaced schema's storage, and by the D-14 orphan sweep to reclaim
// leftover `<orig>_restore_<ulid>` schemas older than the grace window.
//
// Idempotent: the underlying `IF EXISTS` swallows the case where the schema
// has already been dropped by a concurrent process.
//
// Caller MUST validate schemaName against SQL injection upstream — the
// Phase-5 orchestrator generates schema names from ULIDs only (T-05-04-03
// mitigation). This method applies double-quote escaping as defense in
// depth but does not otherwise sanitize the input.
//
// Errors:
//   - originalName not registered — wraps GetMetadataStore error.
//   - registered store does not expose DropSchema — clear error pointing
//     at the type (e.g. memory / badger cannot drop Postgres schemas).
//   - DROP SCHEMA execution failure — propagated verbatim from the engine.
func (s *Service) DropPostgresSchema(ctx context.Context, originalName, schemaName string) error {
	live, err := s.GetMetadataStore(originalName)
	if err != nil {
		return fmt.Errorf("resolve live store for schema drop: %w", err)
	}

	dropper, ok := live.(interface {
		DropSchema(ctx context.Context, schema string) error
	})
	if !ok {
		return fmt.Errorf(
			"store %q does not support schema drop (not Postgres?)",
			originalName)
	}
	if err := dropper.DropSchema(ctx, schemaName); err != nil {
		return fmt.Errorf("drop schema %q on store %q: %w", schemaName, originalName, err)
	}
	return nil
}

// openPostgresAtSchema constructs a Postgres-backed metadata store against
// the schema named by pathOverride using the same connection config encoded
// in cfg.
//
// Implementation approach for Phase-5 plan 04:
//
//	The in-tree Postgres engine today is single-schema (public by default)
//	and does not yet accept a per-instance SchemaName override. Building a
//	full schema-scoped engine requires non-trivial migration handling and
//	search_path plumbing that belongs to a later plan in this phase
//	(fresh_store.go + restore executor).
//
//	Plan 04's contract is the METHOD SIGNATURE + dispatch behavior — the
//	four methods must exist with correct error semantics so Plan 06 / 07
//	can wire the full flow. For postgres, we return a clear
//	"not yet implemented at this call site" error; Plan 06's fresh_store.go
//	replaces this stub with a real search_path-scoped construction path
//	and the corresponding migration runner.
//
// This deviation is tracked as Rule 4 (architectural): schema isolation
// semantics for the Postgres engine are an upstream design question that
// cannot be resolved at the registry layer alone.
func openPostgresAtSchema(
	ctx context.Context,
	cfg *models.MetadataStoreConfig,
	schema string,
) (metadata.MetadataStore, error) {
	_ = ctx
	_ = cfg
	_ = schema
	return nil, errors.New(
		"postgres schema-scoped open is not yet wired in Plan 04; " +
			"Plan 06 (fresh_store.go) replaces this stub with a real " +
			"search_path-based construction path")
}
