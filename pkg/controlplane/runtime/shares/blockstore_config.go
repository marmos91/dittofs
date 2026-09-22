package shares

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/compression"
	"github.com/marmos91/dittofs/pkg/block/encryption"
	"github.com/marmos91/dittofs/pkg/block/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/remote"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	remotes3 "github.com/marmos91/dittofs/pkg/block/remote/s3"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// BlockStoreConfigProvider resolves block store configurations from the control plane DB.
//
// A share's BlockStoreID normally holds the block store row's UUID, but the
// REST UpdateShare path historically persisted the raw *name* instead (#1312).
// Resolution therefore tries the UUID first and falls back to a name lookup so
// shares whose rows hold a name still load after a restart.
type BlockStoreConfigProvider interface {
	GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error)
	GetBlockStore(ctx context.Context, name string) (*models.BlockStoreConfig, error)
}

// resolveBlockStoreConfig resolves a block store reference that may be either a
// UUID (the canonical form) or a name (#1312 legacy rows). It tries the UUID
// lookup first; on not-found it falls back to a name lookup.
func resolveBlockStoreConfig(
	ctx context.Context,
	provider BlockStoreConfigProvider,
	ref string,
) (*models.BlockStoreConfig, error) {
	cfg, err := provider.GetBlockStoreByID(ctx, ref)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, models.ErrStoreNotFound) {
		return nil, err
	}
	// Fall back to name resolution for legacy rows that stored the name.
	byName, nameErr := provider.GetBlockStore(ctx, ref)
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
	// JournalRoot is the directory holding every share's journal. Each share
	// opens its own subdirectory beneath it, so no two shares write into the
	// same directory. It replaces the per-share path a block store config used
	// to carry.
	JournalRoot string

	// ChunkSize is the FastCDC minimum chunk size in bytes; average and maximum
	// derive from it unless ChunkMax overrides the ceiling. 0 keeps the
	// built-in profile.
	ChunkSize uint64

	// ChunkMax overrides the derived maximum chunk size in bytes. 0 keeps the
	// derived value.
	ChunkMax uint64

	// DirtyExpire bounds how long a write may sit unflushed before the journal
	// fsyncs it. Negative disables the timer; 0 keeps the journal default.
	DirtyExpire time.Duration

	MaxSize uint64 // Journal ceiling per share (0 = the journal derives its own)

	// ReadBufferBytes is the per-share read buffer budget in bytes (0 = disabled).
	ReadBufferBytes int64

	// MaxLogBytes is the effective append-log pressure budget (bytes) applied
	// to a share when its per-share block store config does NOT carry an
	// explicit `max_log_bytes`. It resolves the global config
	// blockstore.journal.max_log_bytes (when set) or, failing that, the
	// system-deduced default (DeduceDefaults.MaxLogBytes). 0 leaves the
	// FSStore's own internal default in force. Precedence at the share level
	// is: per-store config["max_log_bytes"] > this global/deduced default.
	MaxLogBytes int64

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
// / ReadChunk delegates the remote.ChunkReader capability (#1414) to the wrapped
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

// buildSyncerConfigFromDefaults merges SyncerDefaults into a engine.RemoteSyncConfig.
func buildSyncerConfigFromDefaults(defaults *SyncerDefaults) engine.RemoteSyncConfig {
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
	cfg, err := resolveBlockStoreConfig(ctx, provider, ref)
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
// JournalSize is the whole of the sizing policy: a positive value is the
// ceiling eviction reclaims against; unset (or negative) hands the decision to
// the journal, which sizes a soft cap off the volume's free space at open.
func mergeLocalStoreDefaults(defaults *LocalStoreDefaults, config *ShareConfig) *LocalStoreDefaults {
	if defaults == nil {
		defaults = &LocalStoreDefaults{}
	}
	merged := *defaults // shallow copy
	merged.MaxSize = uint64(max(config.JournalSize, 0))
	if config.ReadBufferSize > 0 {
		merged.ReadBufferBytes = config.ReadBufferSize
	}
	return &merged
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
	// Merge per-share size overrides into effective defaults.
	effectiveDefaults := mergeLocalStoreDefaults(localStoreDefaults, config)

	// The journal is provisioned under the server-level root rather than
	// configured per share. Opening it refuses a directory still holding the
	// pre-journal layout rather than stamping an empty journal over bytes that
	// may be their own only copy.
	localStore, err := OpenShareJournal(config.Name, effectiveDefaults)
	if err != nil {
		return fmt.Errorf("failed to open share journal: %w", err)
	}

	var remoteStore remote.RemoteStore
	var remoteConfigID string
	if config.BlockStoreID != "" {
		remoteStore, remoteConfigID, err = s.acquireRemoteStore(ctx, config.BlockStoreID, blockStoreProvider)
		if err != nil {
			_ = localStore.Close()
			return fmt.Errorf("failed to create remote store: %w", err)
		}
	}

	// Pin mode keeps blocks stored locally indefinitely. The local store holds the
	// pin itself, so the health-driven SetEvictionEnabled calls made by Start and
	// by the syncer cannot lift it: those calls say only what they know about
	// remote health, and either reason on its own holds eviction off.
	localStore.SetEvictionPinned(config.RetentionPolicy == block.RetentionPin)
	// Note: SetSkipFsync was removed. Local-disk durability is now
	// unconditional (the syncer will refetch from S3 on the rare crash path).

	syncerCfg := buildSyncerConfigFromDefaults(syncerDefaults)
	// The share's FastCDC profile configures the carver the flush pass builds,
	// so it rides the syncer config rather than the journal's: journal's seam is
	// content-agnostic and has no use for it.
	//
	// Every share's journal is disk-backed, so the read-amplification trade the
	// setting governs always applies — there is no longer a memory-backed local
	// tier to exempt.
	if cp, ok := journalChunkParams(effectiveDefaults); ok {
		syncerCfg.ChunkParams = cp
	}
	// A per-remote parallel_uploads override pins the carver's upload window;
	// 0 (the default) keeps the adaptive auto-tune (#1407 / #1432).
	if pinned := remotePinnedUploads(ctx, blockStoreProvider, config.BlockStoreID); pinned > 0 {
		syncerCfg.ParallelUploads = pinned
	}

	// Wrap shared remote in nonClosingRemote so engine.Close() doesn't close it;
	// releaseRemoteStore handles actual closing via ref counting.
	var engineRemote remote.RemoteStore
	if remoteStore != nil {
		engineRemote = &nonClosingRemote{remoteStore}
	}

	syncer := engine.NewRemoteSync(localStore, engineRemote, fileChunkStore, syncerCfg)

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
		RemoteSync:     syncer,
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

	// Apply the share's durability choice. The acknowledgement rule is set
	// before Start so it governs the very first commit; the relaxed metadata
	// flag is stashed and applied to the metadata service in AddShare (which
	// holds the registrar), after RegisterStoreForShare.
	relaxed := config.RelaxedMetadataCommit
	bs.SetRequireDurableCommit(config.CommitAck == models.CommitAckBlockStore)
	share.writeback = relaxed
	// A share acknowledging durably verifies warm reads per-record so on-disk
	// corruption is caught and healed/failed-closed instead of returning
	// silently-wrong bytes; the relaxed tier keeps the raw fast read.
	localStore.SetVerifyReads(!relaxed)

	if err := bs.Start(ctx); err != nil {
		cleanup()
		return fmt.Errorf("failed to start BlockStore: %w", err)
	}

	if err := seedColdIfNeeded(ctx, bs, localStore, fileChunkStore, config.BlockStoreID != "", config.Name); err != nil {
		cleanup()
		return err
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
	// The share's journal directory doubles as the home of the migration
	// journal and the persistent gc-state. OpenShareJournal has already
	// refused an unconfigured root, so this is never empty here.
	share.localStoreDir = ShareJournalDir(effectiveDefaults.JournalRoot, config.Name)
	share.gcStateRoot = filepath.Join(share.localStoreDir, "gc-state")

	// A pinned share never evicts, so a bounded local tier can fill and then
	// fail reads with ErrDiskFull once the working set exceeds it. Warn the
	// operator at startup (no behavior change) so the misconfiguration is
	// visible before it bites a client.
	if config.RetentionPolicy == block.RetentionPin && remoteStore != nil && bs.MaxLocalBytes() > 0 {
		logger.Warn("pinned share with a bounded local tier: reads will fail with ErrDiskFull once the working set exceeds the local tier — raise journal_size or drop the pin",
			"share", config.Name,
			"journal_size", bs.MaxLocalBytes())
	}

	logger.Info("Per-share BlockStore initialized",
		"share", config.Name,
		"mode", modeLabel(remoteStore != nil),
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

	// Serialize rebinds: overlapping teardown/rebuild over the same local dir is
	// unsafe.
	s.rebindMu.Lock()
	defer s.rebindMu.Unlock()

	s.mu.RLock()
	closed := s.closed
	share, ok := s.registry[name]
	s.mu.RUnlock()
	// A rebind during shutdown would close a store the fence is already closing
	// and then build a replacement it never sees. Refuse before the teardown so
	// the share keeps the store it has for the little that is left of the
	// process. The two swaps below check again: the fence can fall while the
	// rebuild is in flight, which is the window that makes this path worse than
	// AddShare rather than merely equal to it.
	if closed {
		return fmt.Errorf("cannot rebind share %q: %w", name, ErrShuttingDown)
	}
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
	if newConfig.BlockStoreID != "" {
		if _, err := resolveBlockStoreConfig(ctx, blockStoreProvider, newConfig.BlockStoreID); err != nil {
			return fmt.Errorf("failed to resolve new block store %q: %w", newConfig.BlockStoreID, err)
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
		cur, stillRegistered := s.registry[name]
		closedNow := s.closed
		if closedNow || !stillRegistered || cur != share {
			s.mu.Unlock()
			s.discardUnpublished(name, recovered)
			if closedNow {
				return fmt.Errorf("share %q could not be rebound and its previous binding could not be restored (%w); new binding also failed: %v",
					name, ErrShuttingDown, buildErr)
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
	cur, stillRegistered := s.registry[name]
	// Shutdown closed every block store while this one was being rebuilt.
	// Swapping it in now is the back door the fence cannot see: the snapshot it
	// closed held the OLD store, so the new one would run its carve dispatcher
	// on past the metadata store's close.
	closedNow := s.closed
	if closedNow || !stillRegistered || cur != share {
		s.mu.Unlock()
		s.discardUnpublished(name, rebuilt)
		if closedNow {
			// The share keeps its now-closed old store, so its ops answer
			// ErrClosed for the rest of the shutdown.
			return fmt.Errorf("share %q was rebound but the result could not be published: %w", name, ErrShuttingDown)
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
		"block_store_id", newConfig.BlockStoreID,
		"mode", modeLabel(newConfig.BlockStoreID != ""))
	return nil
}

// discardUnpublished tears down a block store a rebind built but could not
// install, and drops the remote reference that store acquired.
//
// The OUTGOING store's remote reference is deliberately not touched here. From
// this point it is not knowable whether a concurrent RemoveShare has already
// released it, and releasing it twice underflows the shared ref-count and can
// close a remote other shares are still using. The cost of the other direction
// is a reference that outlives its share — on a process that is leaving, or on
// one share whose removal already accounted for it.
func (s *Service) discardUnpublished(name string, built *Share) {
	if built.BlockStore != nil {
		if closeErr := built.BlockStore.Close(); closeErr != nil {
			logger.Warn("rebind: failed to close the block store that could not be published",
				"share", name, "error", closeErr)
		}
	}
	if built.remoteConfigID != "" {
		s.releaseRemoteStore(built.remoteConfigID)
	}
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
	remoteCfg, err := resolveBlockStoreConfig(ctx, provider, ref)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve remote block store config %q: %w", ref, err)
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

const (
	// minDirtyExpire is the shortest dirty-age commit interval a share may
	// configure. Anything below it is a misconfiguration rather than a tuning
	// choice: the loop would issue barriers faster than a disk retires them.
	minDirtyExpire = time.Second
)

// durableOverrideSetter is implemented by block stores (local and remote) that
// expose a per-store durability override (block stores embed a
// block.DurabilityReporter type-default; this lets an operator flip it).
type durableOverrideSetter interface {
	SetDurable(bool)
}

// openJournalStore opens (or recovers) the share's journal at its own layout
// root (journal/ under the share dir — the journal's own convention) and wires
// the resolved budgets in. The local tier is *journal.Store directly: no
// adapter layer between the composition code and the store.
//
// max_disk threads into Config.MaxLocalBytes (0 defers to Open's free-space
// default); max_log_bytes threads as the Stats size hint only — it does not
// gate writes. applyDurableOverride (at the call site) applies config["durable"]
// to the returned store via its SetDurable.
func openJournalStore(shareDir string, maxDisk, maxLogBytes int64, cfg journal.Config) (*journal.Store, error) {
	// Refuse a pre-journal directory before journal.Open stamps an empty one
	// over it. journal cannot make this call itself: it sees only shareDir/journal
	// and, finding no format stamp there, correctly adopts the directory as new.
	// The legacy bytes sit one level up, in a layout only this package knows.
	if err := checkLegacyLayout(shareDir); err != nil {
		return nil, err
	}
	cfg.MaxLocalBytes = maxDisk
	cfg.MaxLogBytes = maxLogBytes
	// slog.SetDefault routes the configured process logger here.
	cfg.Logger = slog.Default()
	return journal.Open(filepath.Join(shareDir, "journal"), cfg)
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
