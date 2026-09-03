package shares

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/pathutil"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/compression"
	"github.com/marmos91/dittofs/pkg/block/encryption"
	"github.com/marmos91/dittofs/pkg/block/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	localmemory "github.com/marmos91/dittofs/pkg/block/local/memory"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	remotes3 "github.com/marmos91/dittofs/pkg/block/remote/s3"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// BlockStoreConfigProvider resolves block store configurations from the control plane DB.
//
// A share's LocalBlockStoreID/RemoteBlockStoreID normally hold the block store
// row's UUID, but the REST UpdateShare path historically persisted the raw
// *name* instead (#1312). Resolution therefore tries the UUID first and falls
// back to a kind-scoped name lookup so shares whose rows hold a name still load
// after a restart.
type BlockStoreConfigProvider interface {
	GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error)
	GetBlockStore(ctx context.Context, name string, kind models.BlockStoreKind) (*models.BlockStoreConfig, error)
}

// resolveBlockStoreConfig resolves a block store reference that may be either a
// UUID (the canonical form) or a name (#1312 legacy rows). It tries the UUID
// lookup first; on not-found it falls back to a name lookup scoped to the
// expected kind. The kind scope keeps a local name from accidentally resolving
// to a same-named remote store and vice versa.
func resolveBlockStoreConfig(
	ctx context.Context,
	provider BlockStoreConfigProvider,
	ref string,
	kind models.BlockStoreKind,
) (*models.BlockStoreConfig, error) {
	cfg, err := provider.GetBlockStoreByID(ctx, ref)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, models.ErrStoreNotFound) {
		return nil, err
	}
	// Fall back to name resolution for legacy rows that stored the name.
	byName, nameErr := provider.GetBlockStore(ctx, ref, kind)
	if nameErr != nil {
		// A real operational error (DB/context) on the name path must not be
		// masked as "not found"; only collapse to the familiar ID-lookup error
		// when the name is genuinely absent too.
		if errors.Is(nameErr, models.ErrStoreNotFound) {
			return nil, err
		}
		return nil, nameErr
	}
	return byName, nil
}

// LocalStoreDefaults holds default sizing for per-share local stores.
type LocalStoreDefaults struct {
	MaxSize uint64 // Maximum local store size per share (0 = unlimited)

	// ReadBufferBytes is the per-share read buffer budget in bytes (0 = disabled).
	ReadBufferBytes int64

	// MaxLogBytes is the effective append-log pressure budget (bytes) applied
	// to a share when its per-share block store config does NOT carry an
	// explicit `max_log_bytes`. It resolves the global config
	// blockstore.local.max_log_bytes (when set) or, failing that, the
	// system-deduced default (DeduceDefaults.MaxLogBytes). 0 leaves the
	// FSStore's own internal default in force. Precedence at the share level
	// is: per-store config["max_log_bytes"] > this global/deduced default.
	MaxLogBytes int64

	// DefaultRemoteCacheSize is the on-disk ceiling applied to a share's
	// local tier when a REMOTE block store is configured but no explicit
	// per-share LocalStoreSize is set. With a remote configured the local
	// tier is a bounded write-through cache; without this ceiling a fast
	// writer could exhaust the host volume. 0 leaves the conditional ceiling
	// off (the share keeps its system-deduced local size even with a
	// remote). Local-only shares ignore this entirely.
	DefaultRemoteCacheSize uint64

	// BackpressureMaxWait is how long a write stalls waiting for the syncer
	// to drain (freeing cache space) before returning ErrDiskFull, when the
	// remote is healthy but every cached chunk is unsynced. 0 defers to the
	// FSStore default (60s).
	BackpressureMaxWait time.Duration
}

// SyncerDefaults holds default syncer configuration applied to all shares.
type SyncerDefaults struct {
	ParallelDownloads  int
	PrefetchBlocks     int
	SmallFileThreshold int64
	UploadInterval     time.Duration
	UploadDelay        time.Duration

	// PrefetchWorkers is the number of read buffer prefetch workers per share (0 = disabled).
	PrefetchWorkers int
}

// sharedRemote holds a reference-counted remote store shared across shares.
type sharedRemote struct {
	store    remote.RemoteStore
	refCount int
}

// nonClosingRemote wraps a remote.RemoteStore and makes Close() a no-op.
// This prevents engine.Store.Close() from closing the shared remote;
// the shares.Service.releaseRemoteStore handles actual closing via ref counting.
type nonClosingRemote struct {
	remote.RemoteStore
}

func (n *nonClosingRemote) Close() error { return nil }

// Durable delegates to the wrapped remote chain so the no-op-Close wrapper
// still implements block.DurabilityReporter. Without this, embedding
// remote.RemoteStore (which has no Durable method) would silently drop the
// capability, and engine.Store.RemoteDurable() would type-assert to
// block.DurabilityReporter, fail, and report NOT durable for every production
// S3-remote share — breaking the honest COMMIT/CLOSE contract (#1274) with a
// spurious ErrNotDurableYet on every commit.
func (n *nonClosingRemote) Durable() bool { return block.IsDurable(n.RemoteStore) }

// --- remote.RemoteBlockStore proxy (#1414 object packing) ---
//
// The no-op-Close wrapper embeds only remote.RemoteStore, so without these
// forwards it would silently HIDE the block-keyed surface (PutBlock/GetBlock/
// GetBlockRange/DeleteBlock/WalkBlocks) of a wrapped store that implements it —
// exactly the trap the Durable() forward above fixes for durability. The
// snapshot durability-verify gate reaches the block store via
// engine.Store.RemoteStore() (this wrapper), so it MUST be able to probe a
// packed blocks/<id> object. Each method delegates to the embedded store when it
// implements RemoteBlockStore; otherwise it returns remote.ErrChunkReadUnsupported
// (a standalone-only remote is never asked for a block via the locator path).
func (n *nonClosingRemote) blockInner() (remote.RemoteBlockStore, error) {
	if rbs, ok := n.RemoteStore.(remote.RemoteBlockStore); ok {
		return rbs, nil
	}
	return nil, remote.ErrChunkReadUnsupported
}
func (n *nonClosingRemote) PutBlock(ctx context.Context, blockID string, r io.Reader) error {
	rbs, err := n.blockInner()
	if err != nil {
		return err
	}
	return rbs.PutBlock(ctx, blockID, r)
}
func (n *nonClosingRemote) GetBlock(ctx context.Context, blockID string) ([]byte, error) {
	rbs, err := n.blockInner()
	if err != nil {
		return nil, err
	}
	return rbs.GetBlock(ctx, blockID)
}
func (n *nonClosingRemote) GetBlockRange(ctx context.Context, blockID string, offset, length int64) ([]byte, error) {
	rbs, err := n.blockInner()
	if err != nil {
		return nil, err
	}
	return rbs.GetBlockRange(ctx, blockID, offset, length)
}
func (n *nonClosingRemote) DeleteBlock(ctx context.Context, blockID string) error {
	rbs, err := n.blockInner()
	if err != nil {
		return err
	}
	return rbs.DeleteBlock(ctx, blockID)
}
func (n *nonClosingRemote) WalkBlocks(ctx context.Context, fn func(blockID string, meta block.Meta) error) error {
	rbs, err := n.blockInner()
	if err != nil {
		return err
	}
	return rbs.WalkBlocks(ctx, fn)
}

// ReadChunk delegates the remote.ChunkReader capability (#1414) to the wrapped
// store. The syncer's read path type-asserts ChunkReader on ITS remote — this
// wrapper — to serve a chunk whose only copy lives inside a packed block.
// Without this forward every cold read of a packed chunk (local copy lost:
// restart, eviction, torn tail) on a production share failed with
// ErrChunkReadUnsupported instead of recovering from the remote block.
func (n *nonClosingRemote) ReadChunk(ctx context.Context, blockID string, offset, length int64, hash block.ContentHash) ([]byte, error) {
	cr, ok := n.RemoteStore.(remote.ChunkReader)
	if !ok {
		return nil, remote.ErrChunkReadUnsupported
	}
	return cr.ReadChunk(ctx, blockID, offset, length, hash)
}

// buildSyncerConfigFromDefaults merges SyncerDefaults into a engine.SyncerConfig.
func buildSyncerConfigFromDefaults(defaults *SyncerDefaults) engine.SyncerConfig {
	cfg := engine.DefaultConfig()
	if defaults == nil {
		return cfg
	}
	if defaults.ParallelDownloads > 0 {
		cfg.ParallelDownloads = defaults.ParallelDownloads
	}
	if defaults.PrefetchBlocks > 0 {
		cfg.PrefetchBlocks = defaults.PrefetchBlocks
	}
	if defaults.SmallFileThreshold != 0 {
		cfg.SmallFileThreshold = defaults.SmallFileThreshold
	}
	if defaults.UploadInterval > 0 {
		cfg.UploadInterval = defaults.UploadInterval
	}
	if defaults.UploadDelay > 0 {
		cfg.UploadDelay = defaults.UploadDelay
	}
	return cfg
}

// remotePinnedUploads returns the per-remote parallel_uploads override: > 0
// pins the carver's upload window, 0/absent keeps the adaptive auto-tune
// (#1407 / #1432). Any lookup/parse miss falls back to 0 (adaptive) so a
// malformed value never blocks share creation — validateParallelUploads already
// rejects out-of-range values at store-create time. The value arrives as a JSON
// number (float64) from the stored config blob. We still re-validate here
// (integer-ness + 0..engine.MaxParallelUploads clamp) as defense-in-depth against a
// corrupted or old-server config blob that skipped validateParallelUploads —
// silently truncating 2.5 or honoring an absurd window would risk FD/goroutine
// exhaustion.
func remotePinnedUploads(ctx context.Context, provider BlockStoreConfigProvider, ref string) int {
	if ref == "" || provider == nil {
		return 0
	}
	cfg, err := resolveBlockStoreConfig(ctx, provider, ref, models.BlockStoreKindRemote)
	if err != nil {
		return 0
	}
	m, err := cfg.GetConfig()
	if err != nil {
		return 0
	}
	var n int
	switch v := m["parallel_uploads"].(type) {
	case float64:
		if v != math.Trunc(v) { // fractional => malformed, fall back to adaptive
			return 0
		}
		n = int(v)
	case int:
		n = v
	default:
		return 0
	}
	if n <= 0 || n > engine.MaxParallelUploads {
		return 0
	}
	return n
}

// mergeLocalStoreDefaults returns a copy of the system defaults with per-share
// overrides applied. Non-zero ShareConfig values take precedence.
//
// remoteConfigured signals that the share has a remote block store, which
// makes the local tier a bounded write-through cache rather than durable
// storage. In that case, when the operator set no explicit per-share size
// (config.LocalStoreSize == 0), apply the DefaultRemoteCacheSize ceiling so
// a fast writer cannot exhaust the host volume — this takes precedence over
// the generic system-deduced MaxSize, which is sized for the durable
// local-only tier rather than a transient cache. An explicit per-share
// --local-store-size always wins. Local-only shares keep the existing
// MaxSize unchanged.
func mergeLocalStoreDefaults(defaults *LocalStoreDefaults, config *ShareConfig, remoteConfigured bool) *LocalStoreDefaults {
	if defaults == nil {
		defaults = &LocalStoreDefaults{}
	}
	merged := *defaults // shallow copy
	switch {
	case config.LocalStoreSize > 0:
		// Explicit per-share override always wins.
		merged.MaxSize = uint64(config.LocalStoreSize)
	case remoteConfigured && merged.DefaultRemoteCacheSize > 0:
		// Remote-backed share, no explicit override: bound the
		// write-through cache at the remote-cache default.
		merged.MaxSize = merged.DefaultRemoteCacheSize
		logger.Info("applying default local-cache ceiling for remote-backed share",
			"share", config.Name,
			"max_size", merged.MaxSize)
	}
	if config.ReadBufferSize > 0 {
		merged.ReadBufferBytes = config.ReadBufferSize
	}
	return &merged
}

// legacyLocalOnlyMigrator is the local-store surface the shares service drives
// to finish an async pre-journal local-only migration in the background.
// Implemented by the journal-backed fs store; other backends never satisfy it,
// so the drive block is a no-op for them.
type legacyLocalOnlyMigrator interface {
	MigratedFromLegacyLocalOnly() bool
	LegacyPendingPayloads() []string
	MaterializeLegacyPayload(payloadID string) error
	FinishLegacyMigration() error
}

// createBlockStoreForShare creates and starts a per-share BlockStore.
func (s *Service) createBlockStoreForShare(
	ctx context.Context,
	share *Share,
	config *ShareConfig,
	blockStoreProvider BlockStoreConfigProvider,
	fileChunkStore block.EngineFileChunkStore,
	localStoreDefaults *LocalStoreDefaults,
	syncerDefaults *SyncerDefaults,
) error {
	// Resolve local block store config from DB (by UUID, or by name for #1312
	// legacy rows).
	localCfg, err := resolveBlockStoreConfig(ctx, blockStoreProvider, config.LocalBlockStoreID, models.BlockStoreKindLocal)
	if err != nil {
		return fmt.Errorf("failed to resolve local block store config %q: %w", config.LocalBlockStoreID, err)
	}
	if localCfg.Kind != models.BlockStoreKindLocal {
		return fmt.Errorf("block store config %q has kind %q, expected %q", config.LocalBlockStoreID, localCfg.Kind, models.BlockStoreKindLocal)
	}

	// Merge per-share size overrides into effective defaults. A configured
	// remote makes the local tier a bounded write-through cache, so the
	// conditional default ceiling applies (see mergeLocalStoreDefaults).
	remoteConfigured := config.RemoteBlockStoreID != ""
	effectiveDefaults := mergeLocalStoreDefaults(localStoreDefaults, config, remoteConfigured)

	// A remote-backed share whose local dir still holds the pre-journal
	// blobs/+logs/ layout is migrated in place: the local dirs are archived aside
	// so the journal opens clean, and the bytes are re-materialized from the
	// remote via a cold seed below. A local-only share passes false so the
	// guardrail refuses to open a legacy dir (its bytes are the sole copy).
	localStore, err := CreateLocalStoreFromConfig(ctx, localCfg.Type, localCfg, config.Name, effectiveDefaults, fileChunkStore, remoteConfigured)
	if err != nil {
		return fmt.Errorf("failed to create local store: %w", err)
	}

	var remoteStore remote.RemoteStore
	var remoteConfigID string
	if config.RemoteBlockStoreID != "" {
		remoteStore, remoteConfigID, err = s.acquireRemoteStore(ctx, config.RemoteBlockStoreID, blockStoreProvider)
		if err != nil {
			_ = localStore.Close()
			return fmt.Errorf("failed to create remote store: %w", err)
		}
	}

	// Pin mode keeps blocks stored locally indefinitely. The pin is held by the
	// local store itself so the health-driven SetEvictionEnabled calls that Start
	// and the syncer make cannot lift it. Eviction also requires a remote store
	// (so evicted blocks can be re-fetched); that half is enforced by Start
	// reconciling against remote health, which is false without a remote.
	localStore.SetEvictionPinned(config.RetentionPolicy == block.RetentionPin)
	// Note: SetSkipFsync was removed. Local-disk durability is now
	// unconditional (the syncer will refetch from S3 on the rare crash path).

	syncerCfg := buildSyncerConfigFromDefaults(syncerDefaults)
	// A per-remote parallel_uploads override pins the carver's upload window;
	// 0 (the default) keeps the adaptive auto-tune (#1407 / #1432).
	if pinned := remotePinnedUploads(ctx, blockStoreProvider, config.RemoteBlockStoreID); pinned > 0 {
		syncerCfg.ParallelUploads = pinned
	}

	// Wrap shared remote in nonClosingRemote so engine.Close() doesn't close it;
	// releaseRemoteStore handles actual closing via ref counting.
	var engineRemote remote.RemoteStore
	if remoteStore != nil {
		engineRemote = &nonClosingRemote{remoteStore}
	}

	syncer := engine.NewSyncer(localStore, engineRemote, fileChunkStore, syncerCfg)

	// Write-path backpressure is now internal to the journal-backed local store
	// (Config.EvictMaxWait): a full local cache stalls the writer while the carve
	// dispatcher drains unsynced bytes, so there is no SetBackpressureSource
	// wiring to inject here anymore.

	// Wire the block-carve substrate (#1414 object packing, PR3 global flip).
	// Assert on the RAW remoteStore, not engineRemote: the carver holds its own
	// dedicated RemoteBlockStore reference, so wiring it straight to the
	// underlying store skips the no-op-Close wrapper's per-call forwarding hop.
	// (The wrapper does forward the block-keyed surface + ReadChunk these days,
	// so this is conventional rather than required.) Every shipped remote (memory,
	// s3) implements RemoteBlockStore, so this flips carve on for EVERY share
	// with a remote — no feature flag. SetSyncedHashStore (called by engine.New
	// below) derives the blockCommitter from the same per-share metadata store
	// and recomputes carveActive; once all deps are set, new writes route to the
	// carver (blocks/) and never to the legacy standalone mirror (cas/). A remote
	// that does not implement RemoteBlockStore leaves carve disabled and the
	// legacy per-hash mirror in effect (back-compat).
	if remoteStore != nil {
		if rbs, ok := remoteStore.(remote.RemoteBlockStore); ok {
			syncer.SetRemoteBlockStore(rbs)
		}
	}

	cleanup := func() {
		_ = syncer.Close()
		_ = localStore.Close()
		if remoteConfigID != "" {
			s.releaseRemoteStore(remoteConfigID)
		}
	}

	// Wire the metadata coordinator so the engine can invoke RefCount
	// mutations + FileAttr.Blocks persistence without importing
	// pkg/metadata on its hot paths. The fileChunkStore on the engine
	// seam is the per-share metadata store cast to EngineFileChunkStore;
	// the coordinator wraps the same store as a metadata.Store
	// for the typed operations.
	var coordinator engine.MetadataCoordinator
	if metadataStore, ok := fileChunkStore.(metadata.Store); ok {
		coordinator = newMetadataCoordinator(metadataStore)
	}

	engineCfg := engine.BlockStoreConfig{
		Local:          localStore,
		Remote:         engineRemote,
		Syncer:         syncer,
		FileChunkStore: fileChunkStore,
		Coordinator:    coordinator,
	}
	// Thread the SyncedHashStore from the same per-share metadata
	// backend the coordinator wraps so the engine's mirror-loop Flush
	// can MarkSynced after each successful remote.Put. The interface
	// check is the standard runtime-type narrowing used elsewhere in
	// this factory (RollupStore, MetadataStore).
	if shs, ok := fileChunkStore.(metadata.SyncedHashStore); ok {
		engineCfg.SyncedHashStore = shs
	}
	if effectiveDefaults != nil {
		engineCfg.ReadBufferBytes = effectiveDefaults.ReadBufferBytes
	}
	if syncerDefaults != nil {
		engineCfg.PrefetchWorkers = syncerDefaults.PrefetchWorkers
	}

	bs, err := engine.New(engineCfg)
	if err != nil {
		cleanup()
		return fmt.Errorf("failed to create BlockStore: %w", err)
	}

	// Apply the per-share durability tier (#1758), composed from the local store
	// config. The "durability" enum (local|writeback|remote) selects the tier;
	// when absent, the raw "require_durable_commit" (#1274) and "writeback"
	// (#1757) bools are honored for backward compatibility. require_durable_commit
	// is set before Start so it governs the very first commit; the metadata
	// writeback flag is stashed and applied to the metadata service in AddShare
	// (which holds the registrar), after RegisterStoreForShare.
	if localStoreCfg, cfgErr := localCfg.GetConfig(); cfgErr == nil {
		writeback, requireDurableCommit := resolveDurabilityTier(localStoreCfg, config.Name)
		bs.SetRequireDurableCommit(requireDurableCommit)
		share.writeback = writeback
		// Durable tiers (local-durable and remote) verify warm reads per-record so
		// on-disk corruption is caught and healed/failed-closed instead of returning
		// silently-wrong bytes; the writeback tier keeps the raw fast read.
		if vr, ok := localStore.(interface{ SetVerifyReads(bool) }); ok {
			vr.SetVerifyReads(!writeback)
		}
	} else {
		logger.Warn("failed to read local block store config for durability tier; defaulting to local",
			"share", config.Name, "error", cfgErr)
	}

	if err := bs.Start(ctx); err != nil {
		cleanup()
		return fmt.Errorf("failed to start BlockStore: %w", err)
	}

	if err := seedColdIfNeeded(ctx, bs, localStore, fileChunkStore, remoteConfigured, config.Name); err != nil {
		cleanup()
		return err
	}

	// A local-only share (no remote to re-fetch from) that carried a complete
	// pre-journal layout re-ingests its bytes from the surviving append logs in
	// the BACKGROUND — AddShare must not block on O(total-bytes) work. Reads that
	// arrive before a payload is drained fault it in per-payload (never zero-
	// fill). When every payload is drained the archived legacy dirs are deleted;
	// a crash before then leaves them on disk and the next open resumes. The
	// final rollup is done here (outside any metadata txn — the coordinator is
	// non-reentrant), never from inside a read.
	if m, ok := localStore.(legacyLocalOnlyMigrator); ok && m.MigratedFromLegacyLocalOnly() {
		shareName := config.Name
		go func() {
			// Detached from the AddShare context, which may be cancelled once the
			// call returns; the store's own close gate stops the drain on shutdown.
			bgCtx := context.Background()
			pending := m.LegacyPendingPayloads()
			started := time.Now()
			lastLog := started
			logger.Info("legacy local-only migration: re-ingesting archived append logs",
				"share", shareName, "payloads_total", len(pending))
			for i, payloadID := range pending {
				if err := m.MaterializeLegacyPayload(payloadID); err != nil {
					logger.Error("legacy local-only migration: materialize failed; leaving archive for retry on next start",
						"share", shareName, "payload", payloadID, "error", err)
					return
				}
				if time.Since(lastLog) >= migrationProgressInterval {
					lastLog = time.Now()
					logger.Info("legacy local-only migration: re-ingesting archived append logs",
						"share", shareName, "payloads_done", i+1, "payloads_total", len(pending),
						"elapsed", time.Since(started).Round(time.Second))
				}
			}
			if err := bs.DrainRollups(bgCtx); err != nil {
				logger.Error("legacy local-only migration: rollup drain failed; leaving archive for retry on next start",
					"share", shareName, "error", err)
				return
			}
			if err := m.FinishLegacyMigration(); err != nil {
				logger.Error("legacy local-only migration: cleanup of archived legacy dirs failed",
					"share", shareName, "error", err)
				return
			}
			logger.Info("migrated pre-journal local-only layout: re-ingested bytes from append logs and removed the archive",
				"share", shareName)
		}()
	}

	// Thread the inline metrics recorder into the new store's eviction/
	// backpressure path. nil when the runtime has not yet installed a handle
	// (startup share-loading precedes metrics.New); SetMetrics back-fills
	// those shares once it arrives. Read under mu — SetMetrics may run
	// concurrently on another goroutine.
	s.mu.RLock()
	rec := s.metricsRec
	s.mu.RUnlock()
	if rec != nil {
		bs.SetMetrics(rec)
	}

	// Safe without lock: share is not yet in the registry.
	share.BlockStore = bs
	share.remoteConfigID = remoteConfigID
	// Compute the persistent gc-state directory for this share. Only fs-backed
	// local stores produce a non-empty path; in-memory backends skip
	// last-run.json persistence entirely (engine.PersistLastRunSummary is a
	// no-op on empty rootDir).
	share.gcStateRoot = deriveGCStateRoot(localCfg, config.Name)
	// per-share local data dir for the migration journal.
	// Same source-of-truth + emptiness semantics as gcStateRoot — memory
	// backends produce "" so the status handler can short-circuit.
	share.localStoreDir = deriveLocalStoreDir(localCfg, config.Name)

	// A pinned share never evicts, so a bounded local tier can fill and then
	// fail reads with ErrDiskFull once the working set exceeds it. Warn the
	// operator at startup (no behavior change) so the misconfiguration is
	// visible before it bites a client.
	if config.RetentionPolicy == block.RetentionPin && remoteStore != nil && bs.LocalStats().MaxDisk > 0 {
		logger.Warn("pinned share with a bounded local tier: reads will fail with ErrDiskFull once the working set exceeds the local tier — raise local_store_size or drop the pin",
			"share", config.Name,
			"local_store_size", bs.LocalStats().MaxDisk)
	}

	logger.Info("Per-share BlockStore initialized",
		"share", config.Name,
		"mode", modeLabel(remoteStore != nil),
		"local_type", localCfg.Type,
		"retention", config.RetentionPolicy,
		"retention_ttl", config.RetentionTTL)

	return nil
}

// RebindShareBlockStore hot-reloads a running share's per-share BlockStore so a
// changed local/remote block-store binding takes effect WITHOUT a server
// restart (see #1532). The syncer mode (local-only vs. remote) and the remote
// target are fixed at BlockStore construction, so the only correct way to
// change them on a live share is to tear the old store down and build a new one.
//
// The fs local backend is not safe to double-open, and for a remote-only change
// the local directory is unchanged, so the old store is fully drained + closed
// BEFORE the new one is constructed over the same dir. The share therefore has a
// brief I/O gap during the swap: in-flight ops complete (Close drains them),
// new ops briefly get ErrClosed and are retried by the NFS/SMB client. Binding
// a remote also backfills pre-existing local blocks — the rebuilt syncer's
// Start seeds the pending-upload set from disk.
//
// newConfig carries the new binding; oldConfig is identical except for the
// block-store IDs and is used only to rebuild the previous store if the new one
// fails to build, so a rebind failure never leaves the share storeless.
func (s *Service) RebindShareBlockStore(
	ctx context.Context,
	newConfig *ShareConfig,
	oldConfig *ShareConfig,
	storeProvider MetadataStoreProvider,
	blockStoreProvider BlockStoreConfigProvider,
	localStoreDefaults *LocalStoreDefaults,
	syncerDefaults *SyncerDefaults,
) error {
	name := newConfig.Name
	if newConfig.LocalBlockStoreID == "" {
		return fmt.Errorf("cannot rebind share %q: no local block store configured", name)
	}

	// Serialize rebinds: overlapping teardown/rebuild over the same local dir is
	// unsafe.
	s.rebindMu.Lock()
	defer s.rebindMu.Unlock()

	s.mu.RLock()
	share, ok := s.registry[name]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("cannot rebind share %q: not found in registry", name)
	}

	// Resolve the metadata store (fileChunkStore); unchanged by a binding change.
	fileChunkStore, err := storeProvider.GetMetadataStore(newConfig.MetadataStore)
	if err != nil {
		return fmt.Errorf("failed to resolve metadata store for share %q: %w", name, err)
	}

	// Pre-validate the new binding resolves BEFORE tearing down the live store,
	// so a bad binding fails fast without disrupting the running share.
	if _, err := resolveBlockStoreConfig(ctx, blockStoreProvider, newConfig.LocalBlockStoreID, models.BlockStoreKindLocal); err != nil {
		return fmt.Errorf("failed to resolve new local block store %q: %w", newConfig.LocalBlockStoreID, err)
	}
	if newConfig.RemoteBlockStoreID != "" {
		if _, err := resolveBlockStoreConfig(ctx, blockStoreProvider, newConfig.RemoteBlockStoreID, models.BlockStoreKindRemote); err != nil {
			return fmt.Errorf("failed to resolve new remote block store %q: %w", newConfig.RemoteBlockStoreID, err)
		}
	}

	s.mu.RLock()
	oldBS := share.BlockStore
	oldRemoteConfigID := share.remoteConfigID
	s.mu.RUnlock()

	// Cancel any in-flight warm job for this share so it cannot keep fetching
	// into the block store that is about to be drained and closed.
	s.warmJobs.cancelForShare(name)

	// Flush pending uploads to the OLD remote before teardown so switching or
	// detaching a remote does not strand unmirrored blocks. Best-effort: a drain
	// error must not block the rebind.
	if oldBS != nil {
		if err := oldBS.DrainAllUploads(ctx); err != nil {
			logger.Warn("rebind: failed to drain uploads before teardown; continuing",
				"share", name, "error", err)
		}
		if err := oldBS.Close(); err != nil {
			logger.Warn("rebind: error closing previous block store; continuing",
				"share", name, "error", err)
		}
	}

	// Build the new store over the same (now-closed) local dir.
	rebuilt := &Share{Name: name}
	if buildErr := s.createBlockStoreForShare(ctx, rebuilt, newConfig, blockStoreProvider, fileChunkStore, localStoreDefaults, syncerDefaults); buildErr != nil {
		// Recovery: rebuild the previous binding so the share is not left
		// storeless. oldConfig differs from newConfig only in the block-store IDs.
		logger.Error("rebind: failed to build new block store; restoring previous binding",
			"share", name, "error", buildErr)
		recovered := &Share{Name: name}
		if recErr := s.createBlockStoreForShare(ctx, recovered, oldConfig, blockStoreProvider, fileChunkStore, localStoreDefaults, syncerDefaults); recErr != nil {
			// Both failed: the share keeps its now-closed store (ops return
			// ErrClosed, not a nil-deref panic) and needs a restart. Release the
			// original remote ref since nothing holds it anymore.
			if oldRemoteConfigID != "" {
				s.releaseRemoteStore(oldRemoteConfigID)
			}
			return fmt.Errorf("rebind failed for share %q and previous binding could not be restored (%v); share needs a restart: %w", name, recErr, buildErr)
		}
		// Previous binding restored. Swap it in and drop the original remote ref
		// (the recovery rebuild acquired its own) — but only if the share is
		// still registered; a concurrent RemoveShare would already have released
		// oldRemoteConfigID, so releasing it again here underflows the ref-count.
		s.mu.Lock()
		if cur, ok := s.registry[name]; !ok || cur != share {
			s.mu.Unlock()
			if closeErr := recovered.BlockStore.Close(); closeErr != nil {
				logger.Warn("rebind: failed to close recovered block store after concurrent share removal",
					"share", name, "error", closeErr)
			}
			if recovered.remoteConfigID != "" {
				s.releaseRemoteStore(recovered.remoteConfigID)
			}
			return fmt.Errorf("share %q was removed during rebind (new binding also failed: %v)", name, buildErr)
		}
		share.BlockStore = recovered.BlockStore
		share.remoteConfigID = recovered.remoteConfigID
		share.gcStateRoot = recovered.gcStateRoot
		share.localStoreDir = recovered.localStoreDir
		s.blockStoreCache.Store(name, share.BlockStore)
		s.mu.Unlock()
		if oldRemoteConfigID != "" {
			s.releaseRemoteStore(oldRemoteConfigID)
		}
		return fmt.Errorf("failed to rebind block store for share %q (previous binding restored): %w", name, buildErr)
	}

	// Swap the new store into the registry, but only if the share is still
	// registered under the same pointer. A concurrent RemoveShare (which does
	// not take rebindMu) can delete it and release oldRemoteConfigID while we
	// rebuild; swapping into the stale pointer and releasing the old ref again
	// would double-decrement the shared remote ref-count and could close a
	// remote store still used by other shares.
	s.mu.Lock()
	if cur, ok := s.registry[name]; !ok || cur != share {
		s.mu.Unlock()
		// Share removed during rebind. Tear down the store we just built and
		// drop its own remote ref; leave the old ref to RemoveShare.
		if closeErr := rebuilt.BlockStore.Close(); closeErr != nil {
			logger.Warn("rebind: failed to close new block store after concurrent share removal",
				"share", name, "error", closeErr)
		}
		if rebuilt.remoteConfigID != "" {
			s.releaseRemoteStore(rebuilt.remoteConfigID)
		}
		return fmt.Errorf("share %q was removed during rebind", name)
	}
	share.BlockStore = rebuilt.BlockStore
	share.remoteConfigID = rebuilt.remoteConfigID
	share.gcStateRoot = rebuilt.gcStateRoot
	share.localStoreDir = rebuilt.localStoreDir
	s.blockStoreCache.Store(name, share.BlockStore)
	s.mu.Unlock()

	// Drop the previous remote ref now that the new store holds its own.
	if oldRemoteConfigID != "" {
		s.releaseRemoteStore(oldRemoteConfigID)
	}

	s.notifyShareChange()
	logger.Info("Per-share BlockStore rebound live",
		"share", name,
		"local_block_store_id", newConfig.LocalBlockStoreID,
		"remote_block_store_id", newConfig.RemoteBlockStoreID,
		"mode", modeLabel(newConfig.RemoteBlockStoreID != ""))
	return nil
}

// acquireRemoteStore returns a shared remote store, creating it if needed.
// Uses double-checked locking to avoid holding s.mu during potentially slow
// network/DB I/O (config resolution, S3 client initialization).
// Returns the store, its config ID, and any error.
func (s *Service) acquireRemoteStore(ctx context.Context, ref string, provider BlockStoreConfigProvider) (remote.RemoteStore, string, error) {
	// Fast path: when the share already persists the canonical UUID (the common
	// case) and the store is live, take it without a config-resolution DB read.
	// Legacy name refs (#1312) miss here — the map is keyed by UUID — and fall
	// through to full resolution below.
	s.mu.Lock()
	if sr, ok := s.remoteStores[ref]; ok {
		sr.refCount++
		s.mu.Unlock()
		return sr.store, ref, nil
	}
	s.mu.Unlock()

	// Resolve config (by UUID, or by name for #1312 legacy rows) so the
	// ref-count map is always keyed by the canonical store UUID. Two shares
	// referencing the same remote — one by UUID, one by legacy name — must
	// share the single ref-counted store.
	remoteCfg, err := resolveBlockStoreConfig(ctx, provider, ref, models.BlockStoreKindRemote)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve remote block store config %q: %w", ref, err)
	}
	if remoteCfg.Kind != models.BlockStoreKindRemote {
		return nil, "", fmt.Errorf("block store config %q has kind %q, expected %q", ref, remoteCfg.Kind, models.BlockStoreKindRemote)
	}
	configID := remoteCfg.ID

	s.mu.Lock()
	if sr, ok := s.remoteStores[configID]; ok {
		sr.refCount++
		s.mu.Unlock()
		return sr.store, configID, nil
	}
	s.mu.Unlock()

	newStore, err := CreateRemoteStoreFromConfig(ctx, remoteCfg.Type, remoteCfg)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create remote store: %w", err)
	}

	// Decorator order matters: encryption sits BELOW compression on the
	// data flow (caller → compression → encryption → inner). Compress
	// plaintext first so the compressor sees redundancy; encrypted bytes
	// are incompressible by design.
	//
	// Apply order in code is therefore encryption first (innermost),
	// then compression (outermost).
	encWrapped, err := maybeWrapEncryption(ctx, newStore, remoteCfg)
	if err != nil {
		_ = newStore.Close()
		return nil, "", fmt.Errorf("failed to apply encryption policy: %w", err)
	}
	newStore = encWrapped

	wrapped, err := maybeWrapCompression(newStore, remoteCfg)
	if err != nil {
		_ = newStore.Close()
		return nil, "", fmt.Errorf("failed to apply compression policy: %w", err)
	}
	newStore = wrapped

	// Double-check: another goroutine may have created the store concurrently.
	s.mu.Lock()
	if sr, ok := s.remoteStores[configID]; ok {
		sr.refCount++
		s.mu.Unlock()
		// We lost the race; discard our now-redundant store. newStore is the
		// fully-decorated (encryption/compression) stack, so a Close failure
		// here could leak a key provider — surface it rather than swallow it.
		if err := newStore.Close(); err != nil {
			logger.Warn("acquireRemoteStore: failed to close duplicate remote store",
				"config_id", configID, "error", err)
		}
		return sr.store, configID, nil
	}

	s.remoteStores[configID] = &sharedRemote{
		store:    newStore,
		refCount: 1,
	}
	s.mu.Unlock()

	logger.Info("Created shared remote store", "config_id", configID, "type", remoteCfg.Type)
	return newStore, configID, nil
}

// maybeWrapEncryption inspects the remote config's "encryption" key and,
// when present, wraps inner with an encryption.EncryptedRemote. Returns
// inner unchanged when the key is absent.
//
// Key-provider lifetime is bound to the decorator: NewRemote captures
// the provider, and EncryptedRemote.Close calls provider.Close. The
// outer releaseRemoteStore path therefore closes the provider as part
// of the normal decorator teardown.
func maybeWrapEncryption(ctx context.Context, inner remote.RemoteStore, cfg *models.BlockStoreConfig) (remote.RemoteStore, error) {
	parsed, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("parse block store config: %w", err)
	}
	raw, ok := parsed["encryption"]
	if !ok {
		return inner, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal encryption sub-config: %w", err)
	}
	policy, err := encryption.ParsePolicy(encoded)
	if err != nil {
		return nil, err
	}
	provider, err := keyprovider.NewProvider(ctx, policy.Key)
	if err != nil {
		return nil, fmt.Errorf("create key provider: %w", err)
	}
	wrapped, err := encryption.NewRemote(inner, policy, provider)
	if err != nil {
		_ = provider.Close()
		return nil, err
	}
	return wrapped, nil
}

// maybeWrapCompression inspects the remote config's "compression" key
// and, when present, wraps inner with a compression.Decorator. Returns
// inner unchanged when the key is absent.
func maybeWrapCompression(inner remote.RemoteStore, cfg *models.BlockStoreConfig) (remote.RemoteStore, error) {
	parsed, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("parse block store config: %w", err)
	}
	raw, ok := parsed["compression"]
	if !ok {
		return inner, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal compression sub-config: %w", err)
	}
	policy, err := compression.ParsePolicy(encoded)
	if err != nil {
		return nil, err
	}
	return compression.NewRemote(inner, policy)
}

// releaseRemoteStore decrements the reference count and closes the remote store if no longer used.
// Close happens outside the lock to avoid blocking share operations during network I/O.
func (s *Service) releaseRemoteStore(configID string) {
	var storeToClose remote.RemoteStore

	s.mu.Lock()
	sr, ok := s.remoteStores[configID]
	if !ok {
		s.mu.Unlock()
		return
	}
	sr.refCount--
	if sr.refCount <= 0 {
		storeToClose = sr.store
		delete(s.remoteStores, configID)
	}
	s.mu.Unlock()

	if storeToClose != nil {
		_ = storeToClose.Close()
		logger.Info("Closed shared remote store", "config_id", configID)
	}
}

// deriveGCStateRoot returns the per-share gc-state directory used by the GC
// engine to persist its run state and last-run.json: the share's local store
// directory plus a `gc-state` suffix. Returns "" whenever that directory is
// unresolvable — engine.PersistLastRunSummary treats "" as "do not persist".
func deriveGCStateRoot(localCfg interface {
	GetConfig() (map[string]any, error)
}, shareName string) string {
	dir := deriveLocalStoreDir(localCfg, shareName)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "gc-state")
}

// deriveLocalStoreDir returns the per-share on-disk data directory the
// migration tool uses to host `.migration-state.jsonl` and the rolling
// snapshot. Returns "" for in-memory or unresolvable configs (the REST
// status handler treats "" as "no journal available", not an error).
//
// Path layout: `<basePath>/shares/<sanitized>/`. Note the absence of a
// "blocks" or "gc-state" suffix — the migration journal lives at the
// share root next to the blocks directory, not inside it. This matches
// the offline migration tool's --state-dir contract: the operator
// passes the same path the daemon would compute here.
func deriveLocalStoreDir(localCfg interface {
	GetConfig() (map[string]any, error)
}, shareName string) string {
	if localCfg == nil {
		return ""
	}
	cfg, err := localCfg.GetConfig()
	if err != nil {
		return ""
	}
	basePath, ok := cfg["path"].(string)
	if !ok || basePath == "" {
		return ""
	}
	expanded, err := pathutil.ExpandPath(basePath)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(expanded) {
		return ""
	}
	return filepath.Join(expanded, "shares", sanitizeShareName(shareName))
}

const (
	// minDirtyExpire is the shortest dirty-age commit interval a share may
	// configure. Anything below it is a misconfiguration rather than a tuning
	// choice: the loop would issue barriers faster than a disk retires them.
	minDirtyExpire = time.Second
	// maxDirtyExpireSeconds is where a seconds value stops fitting a
	// time.Duration (~292 years).
	maxDirtyExpireSeconds = float64(math.MaxInt64 / int64(time.Second))
)

// dirtyExpiryFromConfig reads the dirty_expire_seconds key, which caps how long
// an acknowledged write may stay in the page cache before the journal fsyncs it
// on its own. Zero (absent, or an unusable value) leaves the journal default in
// place; a negative value disables the loop, leaving the client's own fsync and
// segment rotation as the only durability points.
func dirtyExpiryFromConfig(config map[string]any) time.Duration {
	v, ok := config["dirty_expire_seconds"]
	if !ok {
		return 0
	}
	n, isNum := v.(float64)
	if !isNum || math.IsNaN(n) || math.Abs(n) > maxDirtyExpireSeconds {
		logger.Warn("block store config has dirty_expire_seconds but it is invalid; ignoring", "value", v)
		return 0
	}
	d := time.Duration(n * float64(time.Second))
	// A sub-second interval would put a disk barrier on the store far more often
	// than it can retire one; a typo must not do that.
	if n > 0 && d < minDirtyExpire {
		logger.Warn("block store config dirty_expire_seconds is below the floor; clamping",
			"value", n, "floor", minDirtyExpire)
		return minDirtyExpire
	}
	return d
}

// CreateLocalStoreFromConfig creates a local store instance from a block store config.
func CreateLocalStoreFromConfig(
	ctx context.Context,
	storeType string,
	cfg interface {
		GetConfig() (map[string]any, error)
	},
	shareName string,
	defaults *LocalStoreDefaults,
	fileChunkStore block.EngineFileChunkStore,
	migrateLegacy bool,
) (local.LocalStore, error) {
	config, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	var maxDisk int64
	if defaults != nil {
		maxDisk = int64(defaults.MaxSize)
	}

	// Remote-cache backpressure window (how long a write stalls for the
	// syncer to drain before ErrDiskFull). Threaded into FSStoreOptions
	// below; zero defers to the FSStore default.
	var backpressureMaxWait time.Duration
	if defaults != nil {
		backpressureMaxWait = defaults.BackpressureMaxWait
	}

	// Per-store max_size from config JSON takes precedence over defaults
	if v, ok := config["max_size"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			maxDisk = int64(n)
		} else {
			logger.Warn("block store config has max_size but it is invalid or non-positive; ignoring", "value", v)
		}
	}

	// Append is mandatory on the local tier — the use_append_log opt-out
	// flag was deleted with the legacy path-keyed writer. Budgets still
	// surface through FSStoreOptions to fs.NewWithOptions; invalid values
	// are warned and ignored.
	var fsOpts fs.FSStoreOptions
	fsOpts.BackpressureMaxWait = backpressureMaxWait
	// A remote-backed share may carry a pre-journal blobs/+logs/ layout from an
	// upgrade; archive it aside so the journal opens clean (the caller then cold-
	// seeds from the surviving manifest). A local-only share has no remote to
	// re-fetch from, so it takes the async log-only migration path instead: the
	// bytes are re-ingested from the surviving append logs when they are complete
	// (no compacted log), and the guardrail stays fatal otherwise.
	fsOpts.MigrateLegacyLayout = migrateLegacy
	fsOpts.MigrateLegacyLocalOnly = !migrateLegacy
	// Local-cache size-hint default. Precedence (lowest first):
	// FSStore internal default < global/deduced default (plumbed via
	// LocalStoreDefaults.MaxLogBytes) < per-store config["max_log_bytes"].
	// max_log_bytes no longer gates writes; it only feeds the Stats size hint.
	// Seed fsOpts.MaxLogBytes from the global/deduced default here; the
	// per-store config branch below overrides it when present.
	if defaults != nil && defaults.MaxLogBytes > 0 {
		fsOpts.MaxLogBytes = defaults.MaxLogBytes
	}
	if v, ok := config["max_log_bytes"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			// FIX-15: JSON-decoded numbers land here as float64. Values above
			// 2^53 (~9 PiB) lose integer precision, and non-integer values
			// silently truncate. Warn so a misconfigured budget surfaces in
			// logs instead of producing a budget that is off by hundreds of
			// kilobytes from what the operator typed.
			// Reject out-of-range and non-integer values rather than perform
			// an implementation-defined float64->int64 cast (which on out-of-range
			// inputs can produce a negative or garbage budget).
			if n > float64(math.MaxInt64) || n != math.Trunc(n) {
				logger.Warn("config: max_log_bytes is out of range or non-integer; keeping default", "value", n)
			} else {
				fsOpts.MaxLogBytes = int64(n)
			}
		} else {
			logger.Warn("block store config has max_log_bytes but it is invalid or non-positive; ignoring", "value", v)
		}
	}
	fsOpts.DirtyExpiry = dirtyExpiryFromConfig(config)
	// chunk_size sets the FastCDC Min for this share's carve chunker (#1569) —
	// the dominant knob for effective chunk size and thus random-read
	// amplification. Avg/Max are derived (4x/8x Min) unless chunk_max overrides
	// the ceiling. Absent => the FSStore default (1M/4M/16M, byte-identical to
	// pre-#1569). Lower it (e.g. 131072 = 128 KiB) on random-access shares
	// (VM images / databases): trades weaker dedup + more FileChunk manifest
	// rows for far less read amplification. Reads never re-chunk, so changing
	// this only affects newly written data.
	if v, ok := config["chunk_size"]; ok {
		if n, ok := v.(float64); ok && n > 0 && n == math.Trunc(n) && n <= float64(math.MaxInt32) {
			cp := chunker.Params{Min: int(n), Avg: int(n) * 4, Max: int(n) * 8}
			if mv, ok := config["chunk_max"]; ok {
				if m, ok := mv.(float64); ok && m > 0 && m == math.Trunc(m) && m <= float64(math.MaxInt32) {
					cp.Max = int(m)
					if cp.Avg > cp.Max {
						cp.Avg = cp.Max
					}
				} else {
					logger.Warn("block store config has chunk_max but it is invalid; ignoring", "value", mv)
				}
			}
			if err := cp.Validate(); err != nil {
				logger.Warn("block store config chunk_size produced invalid chunker params; keeping default", "error", err)
			} else {
				fsOpts.ChunkParams = cp
			}
		} else {
			logger.Warn("block store config has chunk_size but it is invalid or non-positive; ignoring", "value", v)
		}
	}
	switch storeType {
	case "fs":
		basePath, ok := config["path"].(string)
		if !ok || basePath == "" {
			return nil, errors.New("fs local store requires path in config")
		}
		expanded, err := pathutil.ExpandPath(basePath)
		if err != nil {
			return nil, fmt.Errorf("failed to expand path %q: %w", basePath, err)
		}
		// Defense-in-depth: ValidateBlockStoreConfig rejects relative paths at
		// create/update time, but pre-existing or out-of-band configs could
		// still carry them. Guard here so filepath.Join doesn't resolve
		// against the server's CWD.
		if !filepath.IsAbs(expanded) {
			return nil, fmt.Errorf("fs local store path must be absolute, got %q", basePath)
		}
		sanitized := sanitizeShareName(shareName)
		// The FSStore creates `blocks/` (CAS) and `logs/` (append log) as
		// siblings under its baseDir. A previous layout produced a doubled
		// `shares/{name}/blocks/blocks/...` path. Existing pre-v0.16 installs
		// migrate via `dfs migrate-to-cas` (which uses share-root as its
		// state-dir, already aligned with deriveLocalStoreDir).
		shareDir := filepath.Join(expanded, "shares", sanitized)
		if err := os.MkdirAll(shareDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create share directory: %w", err)
		}

		// The SyncedHashStore and LocalChunkIndex are derived from this same
		// fileChunkStore backend inside NewWithOptions. There is no longer a
		// rollup worker pool (the journal carves dirty ranges directly), so the
		// former RollupStore guard and StartRollup call are gone.
		store, err := fs.NewWithOptions(shareDir, maxDisk, fileChunkStore, fsOpts)
		if err != nil {
			return nil, err
		}
		applyDurableOverride(store, config, "local "+storeType, shareName)
		return store, nil

	case "memory":
		store := localmemory.New()
		applyDurableOverride(store, config, "local "+storeType, shareName)
		return store, nil

	default:
		return nil, fmt.Errorf("unsupported local store type: %s", storeType)
	}
}

// durableOverrideSetter is implemented by block stores (local and remote) that
// expose a per-store durability override (block stores embed a
// block.DurabilityReporter type-default; this lets an operator flip it).
type durableOverrideSetter interface {
	SetDurable(bool)
}

// applyDurableOverride reads an optional "durable" bool from the per-store
// config and applies it to store when present, overriding the type-default
// durability (#1274). A non-bool "durable" value is warned and ignored so the
// type-default stands. label/shareName feed the diagnostic log line.
func applyDurableOverride(store any, config map[string]any, label, shareName string) {
	v, ok := config["durable"]
	if !ok {
		return
	}
	b, ok := v.(bool)
	if !ok {
		logger.Warn("block store config has durable but it is not a bool; ignoring",
			"store", label, "share", shareName, "value", v)
		return
	}
	setter, ok := store.(durableOverrideSetter)
	if !ok {
		// Every shipped store implements SetDurable; a store that does not is a
		// programmer error, surface it instead of silently dropping the override.
		logger.Warn("block store does not support a durable override; ignoring config[\"durable\"]",
			"store", label, "share", shareName)
		return
	}
	setter.SetDurable(b)
	logger.Info("block store durability overridden by config", "store", label, "share", shareName, "durable", b)
}

// parseRequireDurableCommit reads the optional per-share "require_durable_commit"
// bool from the local store config (#1274). Read the same conservative way as
// config["durable"]: absent or non-bool → false (default), so the commit seam
// acks once Flush succeeds and the remote mirror stays async — ordinary
// NFS/POSIX writes never EIO. When true, CLOSE/COMMIT only succeed once the data
// is on a durable store.
func parseRequireDurableCommit(config map[string]any, shareName string) bool {
	v, ok := config["require_durable_commit"]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		logger.Warn("block store config has require_durable_commit but it is not a bool; ignoring",
			"share", shareName, "value", v)
		return false
	}
	return b
}

// resolveDurabilityTier composes the per-share durability knobs into the two
// underlying behaviors (#1758). The optional "durability" enum in the local
// store config selects a named tier; when absent, the older raw bools
// ("writeback", "require_durable_commit") are honored unchanged for backward
// compatibility. When "durability" is present it is authoritative — the raw
// bools are ignored.
//
//	local     (default) — journal + metadata fsync, async S3 (node-crash-safe)
//	writeback           — per-op FILE_SYNC metadata flush relaxed (deferred to
//	                      the ticker); data still journal-fsync durable. The full
//	                      data-writeback tier additionally needs the journal
//	                      async-commit half, tracked separately.
//	remote              — CLOSE/COMMIT block until data is durable in the remote
//	                      (S3) store (require_durable_commit); survives node loss.
func resolveDurabilityTier(config map[string]any, shareName string) (writeback, requireDurableCommit bool) {
	v, ok := config["durability"]
	if !ok {
		return parseWritebackConfig(config, shareName), parseRequireDurableCommit(config, shareName)
	}
	tier, ok := v.(string)
	if !ok {
		logger.Warn("block store config has durability but it is not a string; defaulting to local",
			"share", shareName, "value", v)
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "", "local":
		return false, false
	case "writeback":
		logger.Info("durability tier: writeback (metadata flush relaxed)", "share", shareName)
		return true, false
	case "remote":
		logger.Info("durability tier: remote (ack-on-S3, strict CLOSE/COMMIT)", "share", shareName)
		return false, true
	default:
		logger.Warn("unknown durability tier; defaulting to local",
			"share", shareName, "durability", tier)
		return false, false
	}
}

// parseWritebackConfig reads the optional per-share "writeback" bool from the
// local store config (#1757). Read the same conservative way as
// config["require_durable_commit"]: absent or non-bool → false (default,
// durable). When true, the share's per-op FILE_SYNC metadata flush takes the
// relaxed deferred-fsync path (see metadata.Service.SetShareWriteback).
func parseWritebackConfig(config map[string]any, shareName string) bool {
	v, ok := config["writeback"]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		logger.Warn("block store config has writeback but it is not a bool; ignoring",
			"share", shareName, "value", v)
		return false
	}
	if b {
		logger.Info("metadata writeback tier enabled by config", "share", shareName)
	}
	return b
}

// CreateRemoteStoreFromConfig creates a remote store from type and dynamic config.
func CreateRemoteStoreFromConfig(ctx context.Context, storeType string, cfg interface {
	GetConfig() (map[string]any, error)
}) (remote.RemoteStore, error) {
	config, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	switch storeType {
	case "memory":
		store := remotememory.New()
		applyDurableOverride(store, config, "remote "+storeType, "")
		return store, nil

	case "filesystem":
		return nil, errors.New("remote store type 'filesystem' removed in v4.0 -- use 'memory' or 's3'")

	case "s3":
		bucket, ok := config["bucket"].(string)
		if !ok || bucket == "" {
			return nil, errors.New("s3 remote store requires bucket")
		}

		region := "us-east-1"
		if r, ok := config["region"].(string); ok && r != "" {
			region = r
		}

		endpoint, _ := config["endpoint"].(string)
		prefix, _ := config["prefix"].(string)
		accessKey, _ := config["access_key_id"].(string)
		secretKey, _ := config["secret_access_key"].(string)
		if accessKey == "" || secretKey == "" {
			return nil, errors.New("s3 remote store requires access_key_id and secret_access_key")
		}
		// When a custom endpoint is set (MinIO, Synology, etc.), default to
		// path-style addressing — virtual-hosted style rarely works on
		// non-AWS S3-compatible services. This matches v0.8.x behavior.
		// Only override when the key is absent; honor explicit false.
		forcePathStyle, hasPathStyle := config["force_path_style"].(bool)
		if endpoint != "" && !hasPathStyle {
			forcePathStyle = true
		}
		allowPrivate, _ := config["allow_private_endpoint"].(bool)

		store, err := remotes3.NewFromConfig(ctx, remotes3.Config{
			Bucket:         bucket,
			Region:         region,
			Endpoint:       endpoint,
			AccessKey:      accessKey,
			SecretKey:      secretKey,
			KeyPrefix:      prefix,
			ForcePathStyle: forcePathStyle,
			AllowPrivate:   allowPrivate,
		})
		if err != nil {
			return nil, err
		}
		applyDurableOverride(store, config, "remote "+storeType, "")
		return store, nil

	default:
		return nil, fmt.Errorf("unsupported remote store type: %s", storeType)
	}
}
