package storebackups

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/backup"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TargetKindMetadata is the single supported target kind in v0.13.0 (D-25).
// Future block-store-backup work adds "block" without changing this file's
// public surface — just register an additional branch in DefaultResolver.
const TargetKindMetadata = "metadata"

// BackupRepoTarget adapts *models.BackupRepo to scheduler.Target. The
// scheduler only needs ID + Schedule; this adapter supplies exactly those
// without leaking the full BackupRepo struct into pkg/backup/scheduler.
type BackupRepoTarget struct {
	repo *models.BackupRepo
}

// NewBackupRepoTarget returns a scheduler-facing wrapper. Panics if repo
// is nil — programmer error, not operator error.
func NewBackupRepoTarget(repo *models.BackupRepo) *BackupRepoTarget {
	if repo == nil {
		panic("storebackups: NewBackupRepoTarget called with nil repo")
	}
	return &BackupRepoTarget{repo: repo}
}

// ID returns the repo ID (stable across restarts — used as jitter seed + mutex key).
func (t *BackupRepoTarget) ID() string { return t.repo.ID }

// Schedule returns the cron expression. Empty string if the repo has no schedule.
func (t *BackupRepoTarget) Schedule() string {
	if t.repo.Schedule == nil {
		return ""
	}
	return *t.repo.Schedule
}

// Repo returns the underlying repo row (for callers like Service.RunBackup
// that need full repo fields after scheduler delivers the target ID).
func (t *BackupRepoTarget) Repo() *models.BackupRepo { return t.repo }

// StoreResolver resolves a (target_kind, target_id) pair into the concrete
// backup-source + identity snapshot needed by the executor. D-26 moved FK
// validation from the DB layer to the service layer — this interface is
// that service-layer validator.
//
// Implementations must:
//   - Return ErrInvalidTargetKind wrapped for unknown kinds (non-"metadata" in v0.13.0).
//   - Return ErrRepoNotFound wrapped when the target config row is missing
//     OR the runtime instance is not registered.
//   - Return backup.ErrBackupUnsupported wrapped when the runtime store
//     does not implement backup.Backupable.
//   - On success return (source, storeID, storeKind) where storeID is the
//     engine-persistent store_id (Phase-5 D-06) and storeKind is the
//     driver kind ("memory"|"badger"|"postgres").
type StoreResolver interface {
	Resolve(ctx context.Context, targetKind, targetID string) (source backup.Backupable, storeID, storeKind string, err error)
}

// RestoreResolver extends StoreResolver with the lookups Phase-5 RunRestore
// needs beyond the plain Resolve return value:
//
//   - ResolveWithName is identical to Resolve but additionally returns the
//     metadata store's config.Name. The name drives
//     shares.Service.ListEnabledSharesForStore for the REST-02 pre-flight
//     gate.
//
//   - ResolveCfg returns the full *models.MetadataStoreConfig so the
//     restore executor can populate Params.TargetStoreCfg without another
//     round-trip through the config getter.
//
// DefaultResolver satisfies this interface; Phase-5 RunRestore type-asserts
// the Service's StoreResolver to RestoreResolver and errors cleanly if the
// caller plugged in a custom resolver that doesn't implement it.
type RestoreResolver interface {
	StoreResolver
	ResolveWithName(ctx context.Context, targetKind, targetID string) (source backup.Backupable, storeID, storeKind, storeName string, err error)
	ResolveCfg(ctx context.Context, targetKind, targetID string) (*models.MetadataStoreConfig, error)
}

// MetadataStoreRegistry is the minimum shape DefaultResolver needs from
// pkg/controlplane/runtime/stores.Service.
type MetadataStoreRegistry interface {
	GetMetadataStore(name string) (metadata.MetadataStore, error)
}

// MetadataStoreConfigGetter is the minimum shape DefaultResolver needs
// from pkg/controlplane/store (GORMStore satisfies this transitively via
// MetadataStoreConfigStore).
type MetadataStoreConfigGetter interface {
	GetMetadataStoreByID(ctx context.Context, id string) (*models.MetadataStoreConfig, error)
}

// DefaultResolver resolves "metadata" targets via the runtime stores
// registry + persistent store config lookup. Additional target kinds
// (e.g. "block") plug in by wrapping this resolver (chain-of-responsibility)
// or by replacing it entirely — no v0.13.0 plan branch changes, but the
// extension point is explicit.
type DefaultResolver struct {
	configs  MetadataStoreConfigGetter
	registry MetadataStoreRegistry
}

// NewDefaultResolver composes a resolver from the persistent config getter
// and the runtime stores registry.
func NewDefaultResolver(configs MetadataStoreConfigGetter, registry MetadataStoreRegistry) *DefaultResolver {
	return &DefaultResolver{configs: configs, registry: registry}
}

// Resolve implements StoreResolver.
func (r *DefaultResolver) Resolve(ctx context.Context, targetKind, targetID string) (backup.Backupable, string, string, error) {
	src, storeID, storeKind, _, err := r.ResolveWithName(ctx, targetKind, targetID)
	return src, storeID, storeKind, err
}

// ResolveWithName implements RestoreResolver. Mirrors Resolve but also
// surfaces cfg.Name — the metadata store's registry key, used by Phase-5
// RunRestore to drive shares.Service.ListEnabledSharesForStore.
func (r *DefaultResolver) ResolveWithName(ctx context.Context, targetKind, targetID string) (backup.Backupable, string, string, string, error) {
	if targetKind != TargetKindMetadata {
		return nil, "", "", "", fmt.Errorf("%w: %q", ErrInvalidTargetKind, targetKind)
	}

	cfg, err := r.configs.GetMetadataStoreByID(ctx, targetID)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("%w: target_id=%q: %v", models.ErrStoreNotFound, targetID, err)
	}

	metaStore, err := r.registry.GetMetadataStore(cfg.Name)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("%w: metadata store %q not loaded: %v", models.ErrStoreNotFound, cfg.Name, err)
	}

	src, ok := metaStore.(backup.Backupable)
	if !ok {
		return nil, "", "", "", fmt.Errorf("%w: store %q (type=%s)", backup.ErrBackupUnsupported, cfg.Name, cfg.Type)
	}

	// Phase 5 D-06: prefer the engine-persistent store_id over cfg.ID. The
	// control-plane DB row's ID is volatile across DB resets; the engine's
	// own ULID (Badger: cfg:store_id key; Postgres: server_config.store_id;
	// Memory: assigned on construction) survives. Using the engine ID here
	// is what makes the manifest.store_id == target.store_id gate meaningful
	// for preventing cross-store restore contamination (Pitfall #4).
	storeIDer, ok := metaStore.(interface{ GetStoreID() string })
	if !ok {
		return nil, "", "", "", fmt.Errorf("store %q (type=%s) does not expose GetStoreID; Phase 5 D-06 contract violated",
			cfg.Name, cfg.Type)
	}
	engineID := storeIDer.GetStoreID()
	if engineID == "" {
		return nil, "", "", "", fmt.Errorf("store %q (type=%s) returned empty store_id", cfg.Name, cfg.Type)
	}
	return src, engineID, cfg.Type, cfg.Name, nil
}

// ResolveCfg implements RestoreResolver. Returns the full
// *models.MetadataStoreConfig for the given (kind, id) — Phase-5
// RunRestore passes this into restore.Params.TargetStoreCfg.
func (r *DefaultResolver) ResolveCfg(ctx context.Context, targetKind, targetID string) (*models.MetadataStoreConfig, error) {
	if targetKind != TargetKindMetadata {
		return nil, fmt.Errorf("%w: %q", ErrInvalidTargetKind, targetKind)
	}
	cfg, err := r.configs.GetMetadataStoreByID(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("%w: target_id=%q: %v", models.ErrStoreNotFound, targetID, err)
	}
	return cfg, nil
}

// Compile-time assertions that DefaultResolver satisfies StoreResolver
// + RestoreResolver and that store.Store satisfies MetadataStoreConfigGetter.
var (
	_ StoreResolver             = (*DefaultResolver)(nil)
	_ RestoreResolver           = (*DefaultResolver)(nil)
	_ MetadataStoreConfigGetter = (store.Store)(nil)
)
