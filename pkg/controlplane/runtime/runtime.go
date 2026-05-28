package runtime

import (
	"context"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/adapters"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/identity"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/lifecycle"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/stores"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/health"
	"github.com/marmos91/dittofs/pkg/metadata"
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

	metadataService *metadata.MetadataService

	adaptersSvc    *adapters.Service
	storesSvc      *stores.Service
	sharesSvc      *shares.Service
	lifecycleSvc   *lifecycle.Service
	identitySvc    *identity.Service
	mountTracker   *MountTracker
	clientRegistry *ClientRegistry

	localStoreDefaults *shares.LocalStoreDefaults
	syncerDefaults     *shares.SyncerDefaults
	gcDefaults         *GCDefaults
	settingsWatcher    *SettingsWatcher

	adapterProviders   map[string]any
	adapterProvidersMu sync.RWMutex

	// snapInFlight tracks per-share in-flight snapshot orchestration
	// goroutines so RemoveShare (plan 23-05) and Runtime.Shutdown can
	// cancel + wait before tearing down state. Keyed by share name.
	// See pkg/controlplane/runtime/snapshot.go (Phase 23, D-23-17).
	snapInFlight   map[string]*snapInFlight
	snapInFlightMu sync.Mutex

	// snapDeleteLocks is the shared registry of per-share RWMutexes that
	// serialize the snapshot GC mark phase (HeldHashes, RLock) against
	// the snapshot-delete write path (AcquireDeleteLock, Lock). The
	// registry is keyed by share name; every SnapshotHoldProvider built
	// for that share — across multiple GC runs and the delete path —
	// looks up the SAME mutex pointer here, so a per-instance mutex on
	// the provider can never collude with a delete on a different
	// provider instance. D-23-04.
	snapDeleteLocks   map[string]*sync.RWMutex
	snapDeleteLocksMu sync.Mutex

	// runtimeCtx is a long-lived ctx cancelled by Runtime.Shutdown
	// (plan 23-05). Snapshot orchestration goroutines derive their
	// child ctx from this so they outlive any caller request ctx
	// but die promptly on Runtime shutdown. D-23-17.
	runtimeCtx    context.Context
	runtimeCancel context.CancelFunc

	snapshotCfg SnapshotDefaults

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
		storesSvc:        stores.New(),
		sharesSvc:        shares.New(),
		lifecycleSvc:     lifecycle.New(DefaultShutdownTimeout),
		identitySvc:      identity.New(),
		statusCheckers:   newCheckerCache(StatusCacheTTL),
		snapshotCfg:      SnapshotDefaults{SyncGateConcurrency: defaultSyncGateConcurrency},
	}

	// Long-lived ctx for snapshot orchestration goroutines (D-23-17).
	// Cancelled by Runtime shutdown in plan 23-05.
	rt.runtimeCtx, rt.runtimeCancel = context.WithCancel(context.Background())

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
// ORDER IS LOAD-BEARING (D-23-17):
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

func (r *Runtime) RegisterMetadataStore(name string, metaStore metadata.MetadataStore) error {
	return r.storesSvc.RegisterMetadataStore(name, metaStore)
}

func (r *Runtime) GetMetadataStore(name string) (metadata.MetadataStore, error) {
	return r.storesSvc.GetMetadataStore(name)
}

func (r *Runtime) GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error) {
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
	return nil
}

// RemoveShare removes a share. Snapshot orchestration goroutines for the
// share are cancelled and drained BEFORE the per-share snapshots/ tree is
// wiped (Phase 22 D-15 hook inside sharesSvc.RemoveShare) — without this
// ordering a still-running snap goroutine could write into the
// about-to-be-deleted directory.
//
// Per Phase 22 invariant (shares/service.go:776 "DB row is the source of
// truth"), snapshot DB rows are NOT cascade-deleted: the cancelled
// goroutine has already flipped its row to state=failed per D-23-09 (or the
// startup-recovery sweep in plan 23-05 / D-23-18 will), and that orphan row
// is harmless because the on-disk manifest is wiped and the hold filter
// (D-23-02) returns false once the snapshots/ tree is gone. D-23-17.
func (r *Runtime) RemoveShare(name string) error {
	r.cancelAndWaitInFlightSnaps(name) // D-23-17: drain BEFORE tree wipe
	return r.sharesSvc.RemoveShare(name)
}

func (r *Runtime) UpdateShare(name string, readOnly *bool, defaultPermission *string, retentionPolicy *blockstore.RetentionPolicy, retentionTTL *time.Duration) error {
	return r.sharesSvc.UpdateShare(name, readOnly, defaultPermission, retentionPolicy, retentionTTL)
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
func (r *Runtime) GetBlockStoreForHandle(ctx context.Context, handle metadata.FileHandle) (*engine.BlockStore, error) {
	return r.sharesSvc.GetBlockStoreForHandle(ctx, handle)
}

// --- Lifecycle (delegated to lifecycle.Service) ---

func (r *Runtime) SetAPIServer(server AuxiliaryServer) {
	r.lifecycleSvc.SetAPIServer(server)
}

func (r *Runtime) Serve(ctx context.Context) error {
	r.clientRegistry.StartSweeper(ctx)

	// D-23-18: Reconcile snapshot rows abandoned by a prior crash BEFORE
	// adapters start serving. Metadata stores and shares are already
	// registered by the cmd/dfs boot sequence at this point; running
	// recovery here means by the time the first CreateSnapshot RPC
	// arrives, the Phase 22 D-08 partial unique index slot for any
	// previously-crashed share is already released. Failure is logged
	// but non-fatal: the operator can still serve, and DeleteSnapshot
	// reconciles whatever rows we could not flip.
	if err := r.recoverOrphanedSnapshots(r.runtimeCtx); err != nil {
		logger.Error("snapshot recovery returned error (continuing startup)", "error", err)
	}

	return r.lifecycleSvc.Serve(ctx, r.settingsWatcher, r.adaptersSvc, r.metadataService, r.storesSvc, r.store)
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

func (r *Runtime) Store() store.Store                            { return r.store }
func (r *Runtime) GetMetadataService() *metadata.MetadataService { return r.metadataService }

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
// grace, 16-way sweep, 1000-sample dry run) regardless of what the
// operator put in gc.* config.
type GCDefaults struct {
	GracePeriod      time.Duration
	SweepConcurrency int
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

func (r *Runtime) GetUserStore() models.UserStore         { return r.store }
func (r *Runtime) GetIdentityStore() models.IdentityStore { return r.store }

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

// --- Snapshot Defaults (Phase 23 D-23-22) ---

// defaultSyncGateConcurrency is the defense-in-depth default applied
// when the operator has not configured snapshot.sync_gate_concurrency.
// Matches the SnapshotConfig default landed in plan 23-01.
const defaultSyncGateConcurrency = 16

// SnapshotDefaults captures operator-configured knobs the Runtime threads
// into snapshot orchestration. Populated from the YAML knob
// `snapshot.sync_gate_concurrency` per D-23-22 (schema in plan 23-01).
// cmd/dfs/start.go calls SetSnapshotDefaults from the parsed config at
// boot.
type SnapshotDefaults struct {
	// SyncGateConcurrency bounds the parallel Head() probes the sync
	// gate fires during VerifyRemoteDurability. Default 16 (matches
	// SnapshotConfig default).
	SyncGateConcurrency int
}

// SetSnapshotDefaults sets the operator-configured snapshot knobs the
// runtime threads into Runtime.CreateSnapshot orchestration. Values
// <= 0 are coerced to the built-in default (defense-in-depth — the
// config loader already validates, but a programmatic caller could
// pass zero).
func (r *Runtime) SetSnapshotDefaults(d SnapshotDefaults) {
	if d.SyncGateConcurrency <= 0 {
		d.SyncGateConcurrency = defaultSyncGateConcurrency
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshotCfg = d
}

// snapshotDefaults returns a copy of the current SnapshotDefaults under
// the runtime read lock. Used by the orchestration goroutine to read
// SyncGateConcurrency at launch time.
func (r *Runtime) snapshotDefaults() SnapshotDefaults {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg := r.snapshotCfg
	if cfg.SyncGateConcurrency <= 0 {
		cfg.SyncGateConcurrency = defaultSyncGateConcurrency
	}
	return cfg
}

// snapInFlight tracks the per-share orchestration goroutines launched
// by Runtime.CreateSnapshot. See pkg/controlplane/runtime/snapshot.go
// for the registration + cleanup helpers.
//
// The done map keys are snapshot IDs; each chan is buffered (cap 1) and
// receives exactly one snapResult before the goroutine closes it, so
// WaitForSnapshot (plan 23-06) can surface the orchestration error via
// errors.Is without consulting the DB. D-23-17.
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
// before close. nil err == success; non-nil err is wrapped per D-23-12.
type snapResult struct {
	err error
}
