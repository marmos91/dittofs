package runtime

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/adapters"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/identity"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/lifecycle"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/snapshotsched"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/stores"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/trash"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metrics"
)

// DefaultShutdownTimeout is the default timeout for graceful shutdown.
const DefaultShutdownTimeout = 30 * time.Second

// Type aliases re-exported for backward compatibility.
type (
	ProtocolAdapter = adapters.ProtocolAdapter
	RuntimeSetter   = adapters.RuntimeSetter
	AdapterFactory  = adapters.AdapterFactory
	AuxiliaryServer = lifecycle.AuxiliaryServer
)

type shareIdentityProvider struct {
	sharesSvc *shares.Service
}

func (p *shareIdentityProvider) GetShareIdentityInfo(shareName string) (*identity.ShareInfo, error) {
	share, err := p.sharesSvc.GetShare(shareName)
	if err != nil {
		return nil, err
	}
	return &identity.ShareInfo{
		Squash:       share.Squash,
		AnonymousUID: share.AnonymousUID,
		AnonymousGID: share.AnonymousGID,
	}, nil
}

// Runtime manages all runtime state for shares and protocol adapters.
// It composes sub-services for adapters, stores, shares, mounts,
// lifecycle, and identity mapping.
type Runtime struct {
	mu    sync.RWMutex
	store store.Store

	// identityStoreOverride, when non-nil, is returned by GetIdentityStore in
	// place of store. Test-only (set via SetIdentityStoreForTesting) so NFS
	// auth tests can inject a lightweight IdentityStore without a full
	// store.Store implementation.
	identityStoreOverride models.IdentityStore

	metadataService *metadata.Service

	adaptersSvc    *adapters.Service
	storesSvc      *stores.Service
	sharesSvc      *shares.Service
	lifecycleSvc   *lifecycle.Service
	identitySvc    *identity.Service
	mountTracker   *MountTracker
	clientRegistry *ClientRegistry

	// metrics is the Prometheus metrics handle, set at startup. It is the
	// carrier for inline adapter instruments (RED, connection, auth counters):
	// adapters reach it via Runtime so no per-adapter plumbing is needed. May
	// be nil; all metrics.Metrics Record* methods are nil-safe.
	metrics *metrics.Metrics

	// trashSvc is the recycle-bin service (list/restore/empty + reaper),
	// constructed lazily by Trash() and started/stopped by the lifecycle
	// Serve/shutdown path. Guarded by mu.
	trashSvc *trash.Service

	// snapSchedSvc is the background snapshot scheduler (policy-driven
	// create + prune), constructed lazily by SnapshotScheduler() and
	// started/stopped by the lifecycle Serve/shutdown path. Guarded by mu.
	snapSchedSvc *snapshotsched.Service
	// snapSchedPollInterval overrides the scheduler poll cadence (0 = default).
	// snapSchedDisabled, when true, prevents Serve from launching it.
	snapSchedPollInterval time.Duration
	snapSchedDisabled     bool

	localStoreDefaults *shares.LocalStoreDefaults
	syncerDefaults     *shares.SyncerDefaults
	gcDefaults         *GCDefaults
	settingsWatcher    *SettingsWatcher

	adapterProviders   map[string]any
	adapterProvidersMu sync.RWMutex

	// snapInFlight tracks per-share in-flight snapshot orchestration
	// goroutines so RemoveShare and Runtime.Shutdown can
	// cancel + wait before tearing down state. Keyed by share name.
	// See pkg/controlplane/runtime/snapshot.go.
	snapInFlight   map[string]*snapInFlight
	snapInFlightMu sync.Mutex

	// snapDeleteLocks is the shared registry of per-share RWMutexes that
	// serialize the snapshot GC mark phase (HeldHashes, RLock) against
	// the snapshot-delete write path (AcquireDeleteLock, Lock). The
	// registry is keyed by share name; every SnapshotHoldProvider built
	// for that share — across multiple GC runs and the delete path —
	// looks up the SAME mutex pointer here, so a per-instance mutex on
	// the provider can never collude with a delete on a different
	// provider instance.
	snapDeleteLocks   map[string]*sync.RWMutex
	snapDeleteLocksMu sync.Mutex

	// restoreLocks serializes RestoreSnapshot per share. Restore requires
	// the share be disabled, but two concurrent restore calls both observe
	// "disabled" and would otherwise interleave their destructive metadata
	// Reset + dump-replay (the single-`creating`-slot DB index only
	// serializes the safety-snap create, which frees between steps). A
	// per-share mutex held for the whole restore makes a second concurrent
	// restore fail fast with models.ErrRestoreInProgress. Keyed by share
	// name; the same pointer is reused per share via restoreLock().
	restoreLocks   map[string]*sync.Mutex
	restoreLocksMu sync.Mutex

	// runtimeCtx is a long-lived ctx cancelled by Runtime.Shutdown.
	// Snapshot orchestration goroutines derive their
	// child ctx from this so they outlive any caller request ctx
	// but die promptly on Runtime shutdown.
	runtimeCtx    context.Context
	runtimeCancel context.CancelFunc

	identityChangeCallbacks []func()

	// statusCheckers is the lazy per-entity cached health-checker
	// map backing [Runtime.BlockStoreChecker],
	// [Runtime.MetadataStoreChecker], [Runtime.AdapterChecker], and
	// [Runtime.ShareChecker]. Initialized in [New].
	statusCheckers *checkerCache
}

func New(s store.Store) *Runtime {
	rt := &Runtime{
		store:            s,
		metadataService:  metadata.New(),
		mountTracker:     NewMountTracker(),
		clientRegistry:   NewClientRegistry(),
		adapterProviders: make(map[string]any),
		snapInFlight:     make(map[string]*snapInFlight),
		snapDeleteLocks:  make(map[string]*sync.RWMutex),
		restoreLocks:     make(map[string]*sync.Mutex),
		storesSvc:        stores.New(),
		sharesSvc:        shares.New(),
		lifecycleSvc:     lifecycle.New(DefaultShutdownTimeout),
		identitySvc:      identity.New(),
		statusCheckers:   newCheckerCache(StatusCacheTTL),
	}

	// Long-lived ctx for snapshot orchestration goroutines.
	// Cancelled by Runtime shutdown.
	rt.runtimeCtx, rt.runtimeCancel = context.WithCancel(context.Background())

	// Install the recycle-bin policy on the shared metadata service. The
	// runtime owns a single MetadataService into which AddShare registers every
	// per-share store, so a single share-aware policy installed here makes the
	// recycle-on-delete decision live for ALL shares — present and future —
	// without per-share wiring. The policy reads the shares service's locked
	// TrashSettings snapshot per delete, so live SetShareTrashConfig changes
	// take effect immediately.
	rt.metadataService.SetTrashPolicy(&trashPolicy{sharesSvc: rt.sharesSvc})

	// Persist quota grace-timer transitions back to the control-plane DB so the
	// soft->grace->hard state survives restart. Only when a store is present.
	if s != nil {
		rt.metadataService.SetQuotaGracePersister(&quotaGracePersister{rt: rt})
	}

	rt.adaptersSvc = adapters.New(s, DefaultShutdownTimeout)
	rt.adaptersSvc.SetRuntime(rt)

	if s != nil {
		rt.settingsWatcher = NewSettingsWatcher(s, DefaultPollInterval)
	}

	return rt
}

// --- Adapter Management (delegated to adapters.Service) ---

func (r *Runtime) SetAdapterFactory(factory AdapterFactory) {
	r.adaptersSvc.SetAdapterFactory(factory)
}

func (r *Runtime) SetShutdownTimeout(d time.Duration) {
	if d == 0 {
		d = DefaultShutdownTimeout
	}
	r.adaptersSvc.SetShutdownTimeout(d)
	r.lifecycleSvc.SetShutdownTimeout(d)
}

// Shutdown drains in-flight snapshot goroutines, stops all protocol
// adapters, and closes metadata stores in that order.
//
// ORDER IS LOAD-BEARING:
//
//  1. shutdownSnapshots — cancel in-flight snapshot goroutines and wait.
//     These goroutines call into Backupable.Backup (on metadata stores)
//     and r.store (control-plane DB). If metadata stores or the control
//     plane were torn down first, snap goroutines would panic on
//     use-after-close.
//  2. StopAllAdapters — adapters no longer accept new RPCs. Existing
//     in-flight RPCs fail naturally (no waiters left to receive them).
//  3. CloseMetadataStores — now safe; nothing holds open references.
//
// Idempotent: a second call is a no-op (runtimeCancel is already
// triggered, adapters and storesSvc handle re-close internally).
//
// ctx bounds only the snapshot-drain step. If ctx fires before all
// goroutines exit, shutdownSnapshots returns and the rest of the
// sequence proceeds — runtimeCancel has already fired, so the orphan
// goroutines will exit on their own. Callers wanting a hard deadline
// should pass context.WithTimeout(...); callers passing
// context.Background block until full snapshot drain.
//
// Composes the existing piecewise lifecycle helpers (StopAllAdapters,
// CloseMetadataStores) which remain public for tests that need to
// drive the steps individually.
func (r *Runtime) Shutdown(ctx context.Context) error {
	// Stop the recycle-bin reaper if it was ever started. Guarded so a Runtime
	// that never served does not construct the service just to stop it; Stop is
	// idempotent so a double-stop (this + ctx cancellation) is harmless.
	r.mu.RLock()
	ts := r.trashSvc
	ss := r.snapSchedSvc
	r.mu.RUnlock()
	if ts != nil {
		ts.Stop()
	}
	if ss != nil {
		ss.Stop()
	}

	r.shutdownSnapshots(ctx)
	if err := r.StopAllAdapters(); err != nil {
		// Continue: snapshot drain already succeeded; the metadata-store
		// close still must run so file handles are released.
		logger.Warn("Runtime.Shutdown: StopAllAdapters error", "error", err)
	}
	r.CloseMetadataStores()
	return nil
}

func (r *Runtime) CreateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	return r.adaptersSvc.CreateAdapter(ctx, cfg)
}

func (r *Runtime) DeleteAdapter(ctx context.Context, adapterType string) error {
	return r.adaptersSvc.DeleteAdapter(ctx, adapterType)
}

func (r *Runtime) UpdateAdapter(ctx context.Context, cfg *models.AdapterConfig) error {
	return r.adaptersSvc.UpdateAdapter(ctx, cfg)
}

func (r *Runtime) EnableAdapter(ctx context.Context, adapterType string) error {
	return r.adaptersSvc.EnableAdapter(ctx, adapterType)
}

func (r *Runtime) DisableAdapter(ctx context.Context, adapterType string) error {
	return r.adaptersSvc.DisableAdapter(ctx, adapterType)
}

func (r *Runtime) StopAllAdapters() error {
	return r.adaptersSvc.StopAllAdapters()
}

func (r *Runtime) LoadAdaptersFromStore(ctx context.Context) error {
	return r.adaptersSvc.LoadAdaptersFromStore(ctx)
}

func (r *Runtime) ListRunningAdapters() []string {
	return r.adaptersSvc.ListRunningAdapters()
}

func (r *Runtime) IsAdapterRunning(adapterType string) bool {
	return r.adaptersSvc.IsAdapterRunning(adapterType)
}

// AddAdapter directly starts a pre-created adapter (for testing, bypasses store).
func (r *Runtime) AddAdapter(adapter ProtocolAdapter) error {
	return r.adaptersSvc.AddAdapter(adapter)
}

// --- Metadata Store Management (delegated to stores.Service) ---

func (r *Runtime) RegisterMetadataStore(name string, metaStore metadata.Store) error {
	return r.storesSvc.RegisterMetadataStore(name, metaStore)
}

func (r *Runtime) GetMetadataStore(name string) (metadata.Store, error) {
	return r.storesSvc.GetMetadataStore(name)
}

func (r *Runtime) GetMetadataStoreForShare(shareName string) (metadata.Store, error) {
	share, err := r.sharesSvc.GetShare(shareName)
	if err != nil {
		return nil, err
	}
	return r.storesSvc.GetMetadataStore(share.MetadataStore)
}

// LocalStoreDir returns the on-disk data directory for the named share's
// local block store. Used by the migration status REST handler to locate
// the per-share `.migration-state.jsonl` journal.
//
// Returns an empty string + nil error when the share's local store has
// no persistent root (memory backend) — handlers should treat "" as
// "no journal available" rather than an error. Returns an
// ErrShareNotFound-wrapped error when the share is unknown so callers
// can map it deterministically to 404.
//
// The accessor is read-only: the path is computed at AddShare time
// from the controlplane DB's BlockStoreConfig and never mutated
// afterward, so a value-or-error response is sufficient (no per-call
// recomputation).
func (r *Runtime) LocalStoreDir(shareName string) (string, error) {
	return r.sharesSvc.LocalStoreDir(shareName)
}

// HealthcheckShare returns the named share's overall health, computed
// as the worst-of its block store engine and metadata store. The
// runtime owns both registries, so this is the natural place to wire
// the lookup before delegating to [Share.Healthcheck].
//
// Lookup-failure semantics:
//
//   - "share not found" → [health.StatusUnknown]. The runtime can't
//     say anything definitive about a share it doesn't know about.
//   - "metadata store not loaded" → [health.StatusUnknown] as well.
//     The store may have been registered earlier but evicted, or
//     never registered (a startup misconfiguration). Without
//     a way to distinguish those cases — the registry doesn't expose
//     the difference — the conservative answer is StatusUnknown:
//     the probe is indeterminate, not the share itself broken. A
//     follow-up phase can sharpen this once the store registry can
//     report "configured but not currently loaded" vs "never
//     registered".
func (r *Runtime) HealthcheckShare(ctx context.Context, shareName string) health.Report {
	// Capture start so every early-return path populates LatencyMs,
	// matching what Share.Healthcheck does. A flat zero on
	// lookup-failure reports would silently mask non-trivial registry
	// lookup time from any monitoring consumer charting probe latency.
	start := time.Now()
	earlyReturn := func(status health.Status, msg string) health.Report {
		end := time.Now()
		return health.Report{
			Status:    status,
			Message:   msg,
			CheckedAt: end.UTC(),
			LatencyMs: end.Sub(start).Milliseconds(),
		}
	}

	// Honor caller cancellation before doing any registry lookups.
	// Otherwise a canceled probe would surface as "share not found"
	// or "metadata store not loaded" instead of the expected
	// context-cancellation StatusUnknown described by the Checker
	// contract.
	if err := ctx.Err(); err != nil {
		return earlyReturn(health.StatusUnknown, err.Error())
	}

	share, err := r.sharesSvc.GetShare(shareName)
	if err != nil {
		return earlyReturn(health.StatusUnknown, "share not found: "+err.Error())
	}

	metaStore, err := r.storesSvc.GetMetadataStore(share.MetadataStore)
	if err != nil {
		return earlyReturn(health.StatusUnknown, "metadata store "+share.MetadataStore+" not loaded: "+err.Error())
	}

	return share.Healthcheck(ctx, metaStore)
}

func (r *Runtime) ListMetadataStores() []string {
	return r.storesSvc.ListMetadataStores()
}

func (r *Runtime) CountMetadataStores() int {
	return r.storesSvc.CountMetadataStores()
}

func (r *Runtime) CloseMetadataStores() {
	r.storesSvc.CloseMetadataStores()
}

// --- Share Management (delegated to shares.Service) ---

func (r *Runtime) AddShare(ctx context.Context, config *ShareConfig) error {
	r.mu.RLock()
	localDefaults := r.localStoreDefaults
	syncDefaults := r.syncerDefaults
	r.mu.RUnlock()
	if err := r.sharesSvc.AddShare(ctx, config, r.storesSvc, r.metadataService, r.store, localDefaults, syncDefaults); err != nil {
		return err
	}
	// Wire quota into the metadata service (0 = unlimited).
	// Always set explicitly to ensure consistency after restarts when a
	// quota was removed (set to 0) via the API.
	r.metadataService.SetQuotaForShare(config.Name, config.QuotaBytes)
	// Load per-identity (user/group) quotas from the control-plane DB so they
	// are enforced immediately after a restart. Best-effort: a load failure
	// must not prevent the share from coming up.
	if err := r.LoadIdentityQuotasForShare(ctx, config.Name); err != nil {
		logger.Warn("failed to load identity quotas for share", "share", config.Name, "error", err)
	}
	// Restore durable default-user grace timers so default-user soft→grace→hard
	// enforcement survives a restart. Best-effort, same as identity quotas.
	if err := r.LoadDefaultUserGraceForShare(ctx, config.Name); err != nil {
		logger.Warn("failed to load default-user grace timers for share", "share", config.Name, "error", err)
	}
	return nil
}

// RegisterShareForTesting registers a minimal enabled share in the share
// service, bypassing the full AddShare store-composition path. Test-only:
// used by protocol handler unit tests that register a metadata store directly
// and need the share to resolve in identity mapping / permission lookups.
func (r *Runtime) RegisterShareForTesting(name string) {
	r.sharesSvc.RegisterShareForTesting(name)
}

// RemoveShare removes a share. Snapshot orchestration goroutines for the
// share are cancelled and drained BEFORE the per-share snapshots/ tree is
// wiped (hook inside sharesSvc.RemoveShare) — without this
// ordering a still-running snap goroutine could write into the
// about-to-be-deleted directory.
//
// Per the invariant in shares/service.go ("DB row is the source of
// truth"), snapshot DB rows are NOT cascade-deleted: the cancelled
// goroutine has already flipped its row to state=failed (or the
// startup-recovery sweep will), and that orphan row
// is harmless because the on-disk manifest is wiped and the hold filter
// returns false once the snapshots/ tree is gone.
func (r *Runtime) RemoveShare(name string) error {
	r.cancelAndWaitInFlightSnaps(name) // drain BEFORE tree wipe
	// sharesSvc.RemoveShare now performs ordered best-effort teardown and may
	// return an aggregated error (REVIEW M4). We must NOT early-return on it:
	// the metadata deregistration below is what prevents the unbounded
	// service-map growth #897/#907 fixed, and it has to run even when the
	// block-store Close or snapshot-dir wipe failed. Aggregate instead.
	rmErr := r.sharesSvc.RemoveShare(name)
	// Deregister the share's per-share store / lock manager / unified view /
	// notifier / quota from the metadata service, mirroring the AddShare
	// registration above. Without this the service maps grow unbounded across
	// add/remove churn and a same-name re-add reuses the stale lock manager.
	r.metadataService.RemoveStoreForShare(name)
	// Purge durable default-user grace timers for the gone share so the
	// user_grace side table cannot accumulate orphan rows across add/remove
	// churn. Best-effort, off the critical path.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.PurgeDefaultUserGrace(ctx, name)
	return rmErr
}

func (r *Runtime) UpdateShare(name string, readOnly *bool, defaultPermission *string, retentionPolicy *block.RetentionPolicy, retentionTTL *time.Duration) error {
	return r.sharesSvc.UpdateShare(name, readOnly, defaultPermission, retentionPolicy, retentionTTL)
}

// SetShareTrashConfig applies recycle-bin settings to a live share under the
// shares-service write lock and persists them via the runtime's store (#190).
// Thin passthrough so API handlers, which only hold *Runtime, can hot-update
// trash policy without reaching into sharesSvc/store directly.
func (r *Runtime) SetShareTrashConfig(name string, cfg shares.TrashSettings) error {
	return r.sharesSvc.SetShareTrashConfig(r.store, name, cfg)
}

// SetShareNetgroup updates the live netgroup association for a share's NFS
// export. An empty netgroupName clears the association (allow-all). Takes
// effect immediately for subsequent CheckNetgroupAccess calls.
func (r *Runtime) SetShareNetgroup(name, netgroupName string) error {
	return r.sharesSvc.SetShareNetgroup(name, netgroupName)
}

// DisableShare sets enabled=false on the share's DB row and runtime
// registry, then notifies adapters so active sessions drop.
// Idempotent on already-disabled shares (returns shares.ErrShareAlreadyDisabled
// which callers typically treat as a benign no-op). Backs the
// POST /api/v1/shares/{name}/disable handler.
func (r *Runtime) DisableShare(ctx context.Context, name string) error {
	return r.sharesSvc.DisableShare(ctx, r.store, name)
}

// EnableShare inverts DisableShare. Idempotent on already-enabled shares
// (no DB write). Backs the POST /api/v1/shares/{name}/enable handler.
func (r *Runtime) EnableShare(ctx context.Context, name string) error {
	return r.sharesSvc.EnableShare(ctx, r.store, name)
}

func (r *Runtime) GetShare(name string) (*Share, error) {
	return r.sharesSvc.GetShare(name)
}

// SetBlockStoreForTesting publishes a per-share BlockStore into the registry
// under the share service lock. Test-only — production code sets BlockStore
// only during AddShare. Use this instead of mutating a *Share returned by
// GetShare, which is now a snapshot copy.
func (r *Runtime) SetBlockStoreForTesting(name string, bs *engine.Store) error {
	return r.sharesSvc.SetBlockStoreForTesting(name, bs)
}

// SetEnabledForTesting overrides a share's Enabled flag in the registry under
// the share service lock. Test-only — use instead of mutating a *Share
// returned by GetShare, which is now a snapshot copy.
func (r *Runtime) SetEnabledForTesting(name string, enabled bool) error {
	return r.sharesSvc.SetEnabledForTesting(name, enabled)
}

func (r *Runtime) GetRootHandle(shareName string) (metadata.FileHandle, error) {
	return r.sharesSvc.GetRootHandle(shareName)
}

func (r *Runtime) ListShares() []string {
	return r.sharesSvc.ListShares()
}

func (r *Runtime) ShareExists(name string) bool {
	return r.sharesSvc.ShareExists(name)
}

func (r *Runtime) OnShareChange(callback func(shares []string)) func() {
	return r.sharesSvc.OnShareChange(callback)
}

func (r *Runtime) GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error) {
	return r.sharesSvc.GetShareNameForHandle(ctx, handle)
}

func (r *Runtime) CountShares() int {
	return r.sharesSvc.CountShares()
}

// UpdateShareQuota hot-updates the byte quota for a share in the metadata service.
// quotaBytes of 0 means unlimited.
func (r *Runtime) UpdateShareQuota(shareName string, quotaBytes int64) {
	r.metadataService.SetQuotaForShare(shareName, quotaBytes)
}

// GetShareUsage returns the logical used bytes and physical disk bytes for a share.
// Returns (0, 0) if the share is not found or has no store.
func (r *Runtime) GetShareUsage(shareName string) (usedBytes int64, physicalBytes int64) {
	// Get logical used bytes from the metadata store's atomic counter.
	metaStore, err := r.metadataService.GetStoreForShare(shareName)
	if err == nil {
		usedBytes = metaStore.GetUsedBytes()
	}

	// Get physical bytes from the block store.
	bs, bsErr := r.sharesSvc.GetBlockStoreForShare(shareName)
	if bsErr == nil {
		if stats, statsErr := bs.Stats(); statsErr == nil {
			physicalBytes = int64(stats.UsedSize)
		}
	}
	return usedBytes, physicalBytes
}

// GetBlockStoreForHandle resolves the per-share BlockStore from a file handle.
func (r *Runtime) GetBlockStoreForHandle(ctx context.Context, handle metadata.FileHandle) (*engine.Store, error) {
	return r.sharesSvc.GetBlockStoreForHandle(ctx, handle)
}

// --- Lifecycle (delegated to lifecycle.Service) ---

func (r *Runtime) SetAPIServer(server AuxiliaryServer) {
	r.lifecycleSvc.SetAPIServer(server)
}

// SetMetrics installs the Prometheus metrics handle. Call once at startup
// before Serve. Passing nil disables inline instrument recording.
func (r *Runtime) SetMetrics(m *metrics.Metrics) {
	r.metrics = m
}

// Metrics returns the Prometheus metrics handle (may be nil). All Record*
// methods on the returned value are nil-safe, so callers need not check.
func (r *Runtime) Metrics() *metrics.Metrics {
	if r == nil {
		return nil
	}
	return r.metrics
}

func (r *Runtime) Serve(ctx context.Context) error {
	r.clientRegistry.StartSweeper(ctx)

	// Launch the recycle-bin reaper alongside the client sweeper. Like the
	// sweeper it exits on ctx cancellation (the lifecycle shutdown path) or on
	// an explicit Trash().Stop() from Runtime.Shutdown.
	r.Trash().Start(ctx)

	// Launch the snapshot scheduler unless disabled. Same lifecycle as the
	// reaper: exits on ctx cancellation or SnapshotScheduler().Stop() from
	// Runtime.Shutdown. Policy-free fleets pay one ListPolicies query per tick.
	if !r.snapSchedDisabled {
		r.SnapshotScheduler().Start(ctx)
	}

	// Reconcile snapshot rows abandoned by a prior crash BEFORE
	// adapters start serving. Metadata stores and shares are already
	// registered by the cmd/dfs boot sequence at this point; running
	// recovery here means by the time the first CreateSnapshot RPC
	// arrives, the partial unique index slot for any
	// previously-crashed share is already released. Failure is logged
	// but non-fatal: the operator can still serve, and DeleteSnapshot
	// reconciles whatever rows we could not flip.
	if err := r.recoverOrphanedSnapshots(r.runtimeCtx); err != nil {
		logger.Error("snapshot recovery returned error (continuing startup)", "error", err)
	}

	// #810: Detect and roll back any restore that a prior crash left
	// half-applied. Runs AFTER recoverOrphanedSnapshots (so a safety
	// snapshot stranded in 'creating' is reconciled first) and BEFORE
	// adapters serve — a half-restored share must never be client-reachable.
	// Failure is logged but non-fatal; the marker is retained so a later
	// boot retries the rollback.
	if err := r.recoverInterruptedRestores(r.runtimeCtx); err != nil {
		logger.Error("restore recovery returned error (continuing startup)", "error", err)
	}

	return r.lifecycleSvc.Serve(ctx, r.settingsWatcher, r.adaptersSvc, r.metadataService, r.storesSvc, r.store, r)
}

// ShutdownSnapshots exposes shutdownSnapshots for the lifecycle.Service
// shutdown sequence so the normal server path (signal -> ctx cancel ->
// lifecycle.shutdown) drains in-flight snapshot goroutines BEFORE
// StopAllAdapters / CloseMetadataStores. Direct callers should prefer
// Runtime.Shutdown which orchestrates the full sequence; this method
// exists to satisfy lifecycle.SnapshotDrainer without exporting
// internal lifecycle details.
func (r *Runtime) ShutdownSnapshots(ctx context.Context) {
	r.shutdownSnapshots(ctx)
}

// --- Identity Mapping (delegated to identity.Service) ---

func (r *Runtime) ApplyIdentityMapping(shareName string, ident *metadata.Identity) (*metadata.Identity, error) {
	return r.identitySvc.ApplyIdentityMapping(shareName, ident, &shareIdentityProvider{sharesSvc: r.sharesSvc})
}

// --- Client Tracking (delegated to ClientRegistry) ---

// Clients returns the client registry for protocol client tracking.
func (r *Runtime) Clients() *ClientRegistry {
	return r.clientRegistry
}

// --- Mount Tracking (delegated to MountTracker) ---

func (r *Runtime) Mounts() *MountTracker {
	return r.mountTracker
}

func (r *Runtime) RecordMount(clientAddr, shareName string, mountTime int64) {
	r.mountTracker.Record(clientAddr, "nfs", shareName, mountTime)
}

func (r *Runtime) RemoveMount(clientAddr string) bool {
	return r.mountTracker.RemoveByClient(clientAddr)
}

func (r *Runtime) RemoveAllMounts() int {
	return r.mountTracker.RemoveAll()
}

// ListMounts converts unified mount records to the legacy NFS format.
func (r *Runtime) ListMounts() []*LegacyMountInfo {
	unified := r.mountTracker.List()
	result := make([]*LegacyMountInfo, 0, len(unified))
	for _, m := range unified {
		ts, ok := m.AdapterData.(int64)
		if !ok {
			ts = m.MountedAt.Unix()
		}
		result = append(result, &LegacyMountInfo{
			ClientAddr: m.ClientAddr,
			ShareName:  m.ShareName,
			MountTime:  ts,
		})
	}
	return result
}

// --- Client Management ---

// DisconnectClient performs protocol-specific teardown for a client.
// It looks up the client record, finds the adapter by protocol, closes the
// TCP connection (triggering cleanup chain), then deregisters the client.
// Returns the removed ClientRecord or nil if not found.
func (r *Runtime) DisconnectClient(clientID string) *ClientRecord {
	record := r.clientRegistry.Get(clientID)
	if record == nil {
		return nil
	}

	// Force-close the TCP connection — this triggers handleConnectionClose()
	// which handles protocol-specific cleanup (NFS state revocation, SMB LOGOFF).
	r.adaptersSvc.ForceCloseClientConnection(record.Protocol, record.Address)

	// Best-effort deregister — cleanup chain may have already removed it.
	r.clientRegistry.Deregister(clientID)

	// Return the snapshot taken before teardown to avoid TOCTOU: the client
	// existed when we started, and we acted on it regardless of race with
	// the cleanup chain.
	return record
}

// --- Service Access ---

func (r *Runtime) Store() store.Store                    { return r.store }
func (r *Runtime) GetMetadataService() *metadata.Service { return r.metadataService }

// SIDMapper returns the machine SID mapper for Windows identity mapping.
// Returns nil if the runtime has not been started yet (Serve not called).
func (r *Runtime) SIDMapper() *sid.SIDMapper { return r.lifecycleSvc.SIDMapper() }

// SetLocalStoreDefaults sets the default sizing for per-share local stores.
func (r *Runtime) SetLocalStoreDefaults(cfg *shares.LocalStoreDefaults) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.localStoreDefaults = cfg
}

// SetSyncerDefaults sets the default syncer configuration for per-share BlockStores.
func (r *Runtime) SetSyncerDefaults(cfg *shares.SyncerDefaults) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncerDefaults = cfg
}

// GCDefaults captures the operator-configured GC knobs the runtime threads
// into engine.Options on every CollectGarbage invocation. Without this
// wiring the engine silently falls back to its hardcoded defaults (1h
// grace, 1000-sample dry run) regardless of what the operator put in
// gc.* config.
type GCDefaults struct {
	GracePeriod      time.Duration
	DryRunSampleSize int
}

// SetGCDefaults sets the operator-configured GC knobs the runtime forwards
// to engine.CollectGarbage via engine.Options. Pass nil to revert to engine
// defaults.
func (r *Runtime) SetGCDefaults(cfg *GCDefaults) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gcDefaults = cfg
}

// gcDefaultsSnapshot returns a copy of the current GCDefaults under the
// runtime lock, or nil when the operator has not configured them.
func (r *Runtime) gcDefaultsSnapshot() *GCDefaults {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.gcDefaults == nil {
		return nil
	}
	cp := *r.gcDefaults
	return &cp
}

// DrainAllUploads waits for all in-flight uploads across all per-share BlockStores to complete.
func (r *Runtime) DrainAllUploads(ctx context.Context) error {
	return r.sharesSvc.DrainAllBlockStores(ctx)
}

// GetBlockStoreStats returns block store statistics, optionally filtered by share name.
func (r *Runtime) GetBlockStoreStats(shareName string) (*shares.BlockStoreStatsResponse, error) {
	return r.sharesSvc.GetBlockStoreStats(shareName)
}

// EvictBlockStore evicts block store data for the given share (or all shares).
func (r *Runtime) EvictBlockStore(ctx context.Context, shareName string, opts shares.EvictOptions) (*shares.EvictResult, error) {
	return r.sharesSvc.EvictBlockStore(ctx, shareName, opts)
}

func (r *Runtime) GetUserStore() models.UserStore { return r.store }

func (r *Runtime) GetIdentityStore() models.IdentityStore {
	if r.identityStoreOverride != nil {
		return r.identityStoreOverride
	}
	return r.store
}

// SetIdentityStoreForTesting overrides the identity store returned by
// GetIdentityStore. Test-only.
func (r *Runtime) SetIdentityStoreForTesting(s models.IdentityStore) {
	r.identityStoreOverride = s
}

// SetSharePolicyForTesting overrides a registered share's DefaultPermission and
// Squash mode. Test-only.
func (r *Runtime) SetSharePolicyForTesting(name, defaultPermission string, squash models.SquashMode) error {
	return r.sharesSvc.SetSharePolicyForTesting(name, defaultPermission, squash)
}

// GetIdentityMappingStore returns the identity mapping store if supported.
// Returns nil if the underlying store does not implement IdentityMappingStore.
func (r *Runtime) GetIdentityMappingStore() store.IdentityMappingStore {
	if ims, ok := r.store.(store.IdentityMappingStore); ok {
		return ims
	}
	return nil
}

// OnIdentityMappingChange registers a callback invoked when identity mappings
// are created or deleted via the API. Adapters use this to invalidate their
// identity resolver caches. Returns an unsubscribe function.
func (r *Runtime) OnIdentityMappingChange(fn func()) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.identityChangeCallbacks = append(r.identityChangeCallbacks, fn)
	idx := len(r.identityChangeCallbacks) - 1
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if idx < len(r.identityChangeCallbacks) {
			r.identityChangeCallbacks[idx] = nil
		}
	}
}

// NotifyIdentityMappingChange fires all registered identity change callbacks.
func (r *Runtime) NotifyIdentityMappingChange() {
	r.mu.RLock()
	cbs := make([]func(), len(r.identityChangeCallbacks))
	copy(cbs, r.identityChangeCallbacks)
	r.mu.RUnlock()
	for _, fn := range cbs {
		if fn != nil {
			fn()
		}
	}
}

// --- Settings Access ---

func (r *Runtime) GetSettingsWatcher() *SettingsWatcher { return r.settingsWatcher }

func (r *Runtime) GetNFSSettings() *models.NFSAdapterSettings {
	if r.settingsWatcher == nil {
		return nil
	}
	return r.settingsWatcher.GetNFSSettings()
}

func (r *Runtime) GetSMBSettings() *models.SMBAdapterSettings {
	if r.settingsWatcher == nil {
		return nil
	}
	return r.settingsWatcher.GetSMBSettings()
}

// --- Adapter Providers ---

func (r *Runtime) SetAdapterProvider(key string, p any) {
	r.adapterProvidersMu.Lock()
	defer r.adapterProvidersMu.Unlock()
	r.adapterProviders[key] = p
}

func (r *Runtime) GetAdapterProvider(key string) any {
	r.adapterProvidersMu.RLock()
	defer r.adapterProvidersMu.RUnlock()
	return r.adapterProviders[key]
}

// SetNFSClientProvider is deprecated; use SetAdapterProvider("nfs", p).
func (r *Runtime) SetNFSClientProvider(p any) { r.SetAdapterProvider("nfs", p) }

// NFSClientProvider is deprecated; use GetAdapterProvider("nfs").
func (r *Runtime) NFSClientProvider() any { return r.GetAdapterProvider("nfs") }

// snapInFlight tracks the per-share orchestration goroutines launched
// by Runtime.CreateSnapshot. See pkg/controlplane/runtime/snapshot.go
// for the registration + cleanup helpers.
//
// The done map keys are snapshot IDs; each chan is buffered (cap 1) and
// receives exactly one snapResult before the goroutine closes it, so
// WaitForSnapshot can surface the orchestration error via
// errors.Is without consulting the DB.
type snapInFlight struct {
	wg sync.WaitGroup
	// cancels is keyed by snapshot ID so completed snapshots can release
	// their cancel func (and the derived ctx attached to runtimeCtx) at
	// unregisterSnap time instead of leaking for the share's lifetime.
	cancels map[string]context.CancelFunc
	done    map[string]chan snapResult
	mu      sync.Mutex
	// draining is set true by cancelAndWaitInFlightSnaps after it has
	// cancelled the per-snap ctxs but BEFORE wg.Wait, so concurrent
	// WaitForSnapshot callers continue to observe the per-snap doneCh
	// (instead of falling through to GetSnapshot and reporting a row
	// still in state='creating'). A registerSnapInFlight observing a
	// draining entry replaces the map slot with a fresh entry — the
	// original entry pointer is still held locally by the draining
	// caller, so its wg.Wait remains valid.
	draining bool
}

// snapResult is sent exactly once on a snap's done channel, immediately
// before close. nil err == success; non-nil err is wrapped.
type snapResult struct {
	err error
}
