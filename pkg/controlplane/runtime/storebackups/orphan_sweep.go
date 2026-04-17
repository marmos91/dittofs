package storebackups

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/stores"
)

// DefaultRestoreOrphanGraceWindow is the minimum age before a temp
// restore directory/schema is considered safe to reclaim. Matches the
// Phase-3 destination orphan sweep default. Younger orphans are
// preserved so an in-flight restore on a concurrent process (unexpected
// but defensively guarded) isn't clobbered.
const DefaultRestoreOrphanGraceWindow = 1 * time.Hour

// PostgresOrphanLister is the narrow contract SweepRestoreOrphans needs
// for Postgres schema enumeration + cleanup. Satisfied by
// *stores.Service (pkg/controlplane/runtime/stores.Service) per Plan 04
// — the REAL implementation is REQUIRED, not optional. This interface
// exists only so the sweep is unit-testable with a stub; there is no
// silent-skip fallback for callers that don't provide it.
type PostgresOrphanLister interface {
	ListPostgresRestoreOrphans(ctx context.Context, originalName, schemaPrefix string) ([]stores.PostgresRestoreOrphan, error)
	DropPostgresSchema(ctx context.Context, originalName, schemaName string) error
}

// SweepRestoreOrphans enumerates every metadata store config (via the
// narrow MetadataStoreConfigLister satisfied directly by the composite
// control-plane store.Store) and reclaims any leftover
// `<path>.restore-<ulid>` directories (Badger) or
// `<schema>_restore_<ulid>` schemas (Postgres) older than graceWindow.
// Memory stores are ephemeral — no sweep applies.
//
// Errors per orphan are logged at WARN and swallowed; the sweep never
// returns an error that would block Service.Serve.
//
// D-14: this is the counterpart to the Phase-3 destination orphan
// sweep but for the source-engine side of restore. The Postgres path
// calls stores.Service.ListPostgresRestoreOrphans DIRECTLY (required
// interface per Plan 04) — no optional-type-assertion silent skip, no
// noop fallback lister.
//
// Production wiring: runtime.Runtime constructs Service with
//   - WithMetadataConfigs(composite store.Store) — composite Store
//     implements ListMetadataStores per pkg/controlplane/store/metadata.go:20
//   - WithStores(*stores.Service) — implements PostgresOrphanLister
//
// Test wiring: tests pass stub implementations of both interfaces.
func SweepRestoreOrphans(
	ctx context.Context,
	configs MetadataStoreConfigLister,
	storesSvc PostgresOrphanLister,
	graceWindow time.Duration,
) {
	if graceWindow <= 0 {
		graceWindow = DefaultRestoreOrphanGraceWindow
	}
	cfgs, err := configs.ListMetadataStores(ctx)
	if err != nil {
		logger.Warn("SweepRestoreOrphans: list metadata store configs failed", "error", err)
		return
	}
	now := time.Now()
	for _, cfg := range cfgs {
		switch cfg.Type {
		case "badger":
			sweepBadgerOrphans(cfg, now, graceWindow)
		case "postgres":
			sweepPostgresOrphans(ctx, storesSvc, cfg, now, graceWindow)
		case "memory":
			// no backing to sweep — memory stores are process-local
		default:
			// unknown type — skip silently (future engines register their
			// own cleanup semantics)
		}
	}
}

// sweepBadgerOrphans scans the parent directory of cfg's backing path
// for entries matching `<base>.restore-<ulid>` older than grace. Uses
// filesystem mtime for age (CleanupTempBacking's os.RemoveAll + fresh
// engine open both touch the temp path mtime when it's live; stale
// ones keep their original mtime from the aborted restore).
func sweepBadgerOrphans(cfg *models.MetadataStoreConfig, now time.Time, grace time.Duration) {
	raw, err := cfg.GetConfig()
	if err != nil {
		logger.Warn("SweepRestoreOrphans: parse badger cfg", "name", cfg.Name, "error", err)
		return
	}
	path, _ := raw["path"].(string)
	if path == "" {
		return
	}
	parent := filepath.Dir(path)
	base := filepath.Base(path)
	entries, err := os.ReadDir(parent)
	if err != nil {
		if os.IsNotExist(err) {
			// Parent gone — nothing to sweep; silent
			return
		}
		logger.Warn("SweepRestoreOrphans: read parent", "parent", parent, "error", err)
		return
	}
	prefix := base + ".restore-"
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		orphanPath := filepath.Join(parent, entry.Name())
		info, err := entry.Info()
		if err != nil {
			logger.Warn("SweepRestoreOrphans: stat entry",
				"path", orphanPath, "error", err)
			continue
		}
		age := now.Sub(info.ModTime())
		if age < grace {
			logger.Debug("SweepRestoreOrphans: skip young badger orphan",
				"path", orphanPath, "age", age, "grace", grace)
			continue
		}
		logger.Warn("SweepRestoreOrphans: reclaiming badger orphan",
			"path", orphanPath, "age", age)
		if err := os.RemoveAll(orphanPath); err != nil {
			logger.Warn("SweepRestoreOrphans: remove failed",
				"path", orphanPath, "error", err)
		}
	}
}

// sweepPostgresOrphans calls stores.Service.ListPostgresRestoreOrphans
// DIRECTLY (required interface per Plan 04) — no optional assertion,
// no silent skip. Errors are logged and swallowed; the sweep continues
// for other stores.
func sweepPostgresOrphans(
	ctx context.Context,
	storesSvc PostgresOrphanLister,
	cfg *models.MetadataStoreConfig,
	now time.Time,
	grace time.Duration,
) {
	raw, err := cfg.GetConfig()
	if err != nil {
		logger.Warn("SweepRestoreOrphans: parse postgres cfg", "name", cfg.Name, "error", err)
		return
	}
	origSchema, _ := raw["schema"].(string)
	if origSchema == "" {
		origSchema = "public"
	}
	schemaPrefix := origSchema + "_restore_"
	orphans, err := storesSvc.ListPostgresRestoreOrphans(ctx, cfg.Name, schemaPrefix)
	if err != nil {
		logger.Warn("SweepRestoreOrphans: list postgres orphans",
			"name", cfg.Name, "error", err)
		return
	}
	for _, o := range orphans {
		age := now.Sub(o.CreatedAt)
		// Zero CreatedAt (non-ULID suffix) → treat as unknown age;
		// skip to avoid reclaiming a schema we can't date.
		if o.CreatedAt.IsZero() {
			logger.Debug("SweepRestoreOrphans: skip postgres orphan with unknown age",
				"schema", o.Name)
			continue
		}
		if age < grace {
			logger.Debug("SweepRestoreOrphans: skip young postgres orphan",
				"schema", o.Name, "age", age, "grace", grace)
			continue
		}
		logger.Warn("SweepRestoreOrphans: dropping postgres orphan schema",
			"schema", o.Name, "age", age)
		if err := storesSvc.DropPostgresSchema(ctx, cfg.Name, o.Name); err != nil {
			logger.Warn("SweepRestoreOrphans: drop schema failed",
				"schema", o.Name, "error", err)
		}
	}
}

// Compile-time check: *stores.Service (Plan 04) satisfies
// PostgresOrphanLister. If Plan 04's method signatures drift, this
// check breaks at build time rather than silently at runtime.
var _ PostgresOrphanLister = (*stores.Service)(nil)
