package shares

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

func (s *Service) AddShare(
	ctx context.Context,
	config *ShareConfig,
	storeProvider MetadataStoreProvider,
	metadataSvc MetadataServiceRegistrar,
	blockStoreProvider BlockStoreConfigProvider,
	localStoreDefaults *LocalStoreDefaults,
	syncerDefaults *SyncerDefaults,
) error {
	// A share whose name cannot encode a file handle can never serve a file, so
	// reject it before any state is created rather than at the first handle mint.
	if err := metadata.ValidateShareName(config.Name); err != nil {
		return err
	}

	if config.LocalBlockStoreID != "" && blockStoreProvider == nil {
		return fmt.Errorf("block store provider is required when LocalBlockStoreID is set for share %q", config.Name)
	}

	if metadataSvc == nil {
		return fmt.Errorf("metadata service registrar is required for share %q", config.Name)
	}

	// Phase 0: Reserve the share name under s.mu BEFORE any side-effecting init
	// (root-dir create, block-store start, metadata RegisterStoreForShare). This
	// closes the AddShare(sameName) race (REVIEW M2): the OLD ordering let two
	// racing callers both run RegisterStoreForShare (last writer wins on the
	// MetadataService store map) and only THEN recheck the registry under the
	// lock — so the loser would tear down its block store yet leave its metadata
	// store registered, pointing the MetadataService at a different store than
	// the registry exposes. Reserving here serializes on the name first: the
	// loser fails before touching any shared store, so no mismatch can form.
	//
	// The reservation is NOT inserted into registry, so a half-built share is
	// never visible to handlers; it is converted to a registry entry only in
	// Phase 4 after all init succeeds.
	if err := s.reserveShareName(config.Name); err != nil {
		return err
	}
	// Track whether the reservation still needs releasing. Phase 4 hands the
	// reservation off to the registry entry (under the same lock) and clears
	// this flag; every failure path releases it.
	reservationHeld := true
	defer func() {
		if reservationHeld {
			s.releaseShareName(config.Name)
		}
	}()

	// Phase 1: Build share struct (resolves metadata store, creates root dir).
	// Does NOT insert into registry yet -- share is invisible to handlers.
	share, metadataStore, err := s.prepareShare(ctx, config, storeProvider)
	if err != nil {
		return err
	}

	// Phase 2: Create per-share BlockStore if local block store config is provided.
	if config.LocalBlockStoreID != "" {
		if err := s.createBlockStoreForShare(ctx, share, config, blockStoreProvider, metadataStore, localStoreDefaults, syncerDefaults); err != nil {
			return fmt.Errorf("failed to create block store for share %q: %w", config.Name, err)
		}
	}

	// cleanupShare releases resources for a share that failed to fully initialize.
	cleanupShare := func() {
		if share.BlockStore != nil {
			_ = share.BlockStore.Close()
		}
		if share.remoteConfigID != "" {
			s.releaseRemoteStore(share.remoteConfigID)
		}
	}

	// Phase 2.5: Reconcile metadata file sizes against the local journal's durable
	// high-water mark (#1687 safety net). WRITE/COMMIT now commit file size via a
	// relaxed (deferred-fsync) metadata transaction, so a crash between a relaxed
	// size commit and its background fsync can leave metadata.Size smaller than the
	// data actually made durable in the journal. This grows metadata.Size up to the
	// journal size (max-only, never shrinks) BEFORE the share is registered and any
	// protocol handler can read it, so ACK'd bytes are never truncated.
	if config.LocalBlockStoreID != "" && share.BlockStore != nil {
		if err := reconcileMetadataSizeFromJournal(ctx, metadataStore, share.BlockStore); err != nil {
			cleanupShare()
			return fmt.Errorf("failed to reconcile metadata sizes for share %q: %w", config.Name, err)
		}
	}

	// Phase 3: Register metadata store. Safe to call unconditionally now: the
	// Phase-0 reservation guarantees no other AddShare for this name is in
	// flight, so this RegisterStoreForShare cannot be raced into a
	// last-writer-wins mismatch.
	if err := metadataSvc.RegisterStoreForShare(config.Name, metadataStore); err != nil {
		cleanupShare()
		return fmt.Errorf("failed to configure metadata for share: %w", err)
	}

	// Apply the per-share metadata writeback tier (#1757) parsed from the local
	// store config in createBlockStoreForShare. Set explicitly (true or false) so
	// a re-add that toggles writeback off is honored. Only the concrete
	// *metadata.Service implements the setter.
	if wb, ok := metadataSvc.(MetadataWritebackSetter); ok {
		wb.SetShareWriteback(config.Name, share.writeback)
	}

	// Let size commits consult the block store's durable extent. The resolver is
	// share-agnostic, so re-installing it on every AddShare is idempotent.
	if de, ok := metadataSvc.(MetadataDurableExtentSetter); ok {
		de.SetDurableExtentResolver(s.durableExtent)
	}

	// Phase 4: Convert the reservation into a registry entry under s.mu.
	// Only now is the share visible to protocol handlers. The reservation has
	// held the name exclusively since Phase 0, so registry[name] cannot already
	// exist here; we assert it defensively and hand off the reservation.
	s.mu.Lock()
	if _, exists := s.registry[config.Name]; exists {
		// Should be unreachable while the reservation is held, but stay
		// fail-safe: tear down rather than overwrite an existing share.
		s.mu.Unlock()
		cleanupShare()
		// Deregister the metadata store we just published so we do not leak a
		// registration for a share we are refusing to finalize.
		if remover, ok := metadataSvc.(MetadataServiceDeregistrar); ok {
			remover.RemoveStoreForShare(config.Name)
		}
		return fmt.Errorf("share %q already exists", config.Name)
	}
	s.registry[config.Name] = share
	if share.BlockStore != nil {
		s.blockStoreCache.Store(config.Name, share.BlockStore)
	}
	delete(s.reservations, config.Name)
	reservationHeld = false
	s.mu.Unlock()

	s.notifyShareChange()

	return nil
}

// durableExtent reports how far a payload's bytes are on stable storage in its
// share's block store. A share with no block store answers "unknown" (false),
// and the caller then commits the size unclamped as before.
//
// It is the commit-time half of the size invariant whose restart-time half is
// reconcileMetadataSizeFromJournal: a persisted size never runs ahead of the
// durable data, and share start grows it back up to that data.
func (s *Service) durableExtent(shareName string, payloadID metadata.PayloadID) (int64, bool) {
	bs, err := s.GetBlockStoreForShare(shareName)
	if err != nil || bs == nil {
		return 0, false
	}
	return bs.DurableExtent(context.Background(), payloadID)
}

// payloadSizer is the narrow read the size reconcile needs: one file's persisted
// size, addressed by payload, without the link count, chunk manifest and derived
// path GetFileByPayloadID also loads and the reconcile then throws away.
type payloadSizer interface {
	FileSizeByPayloadID(ctx context.Context, payloadID metadata.PayloadID) (uint64, bool, error)
}

// payloadSizeLookup returns the cheapest size-by-payload read the store offers,
// falling back to the full GetFileByPayloadID load for stores that have none.
func payloadSizeLookup(metadataStore metadata.Store) func(context.Context, metadata.PayloadID) (uint64, bool, error) {
	if ps, ok := metadataStore.(payloadSizer); ok {
		return ps.FileSizeByPayloadID
	}
	return func(ctx context.Context, payloadID metadata.PayloadID) (uint64, bool, error) {
		f, err := metadataStore.GetFileByPayloadID(ctx, payloadID)
		if err != nil {
			if metadata.IsNotFoundError(err) {
				return 0, false, nil
			}
			return 0, false, err
		}
		if f == nil {
			return 0, false, nil
		}
		return f.Size, true, nil
	}
}

// staleSize names a file whose persisted metadata size trails the journal's
// durable high-water mark, and the mark to grow it to.
type staleSize struct {
	id          string
	journalSize uint64
}

// findStaleSizes reports which of the journal's files have a persisted metadata
// size below their journal high-water mark. The comparison is read-only and
// order-free, so workers take contiguous slices of the list rather than one
// goroutine per file: share start compares every locally-resident file, and on a
// store holding millions of them that is what the share's visibility waits on.
//
// A real store error (I/O, corruption) aborts the scan rather than being
// swallowed — skipping a file would leave its metadata.Size stale and truncate
// reads. A file the metadata store does not know is an orphan journal entry and
// is simply skipped.
func findStaleSizes(ctx context.Context, metadataStore metadata.Store, localStore local.LocalStore, files []string) ([]staleSize, error) {
	if len(files) == 0 {
		return nil, nil
	}
	workers := min(len(files), runtime.NumCPU(), 8)
	chunk := (len(files) + workers - 1) / workers

	sizeOf := payloadSizeLookup(metadataStore)
	var (
		mu    sync.Mutex
		stale []staleSize
	)
	g, gctx := errgroup.WithContext(ctx)
	for start := 0; start < len(files); start += chunk {
		batch := files[start:min(start+chunk, len(files))]
		g.Go(func() error {
			var found []staleSize
			for _, id := range batch {
				journalSize, ok := localStore.FileSize(gctx, id)
				if !ok || journalSize < 0 {
					continue
				}
				size, known, err := sizeOf(gctx, metadata.PayloadID(id))
				if err != nil {
					return fmt.Errorf("reconcile size: lookup payload %s: %w", id, err)
				}
				// Grow only, and compare in uint64: a size past the journal mark
				// must never cast negative and shrink the file.
				if !known || size >= uint64(journalSize) {
					continue
				}
				found = append(found, staleSize{id: id, journalSize: uint64(journalSize)})
			}
			if len(found) == 0 {
				return nil
			}
			mu.Lock()
			stale = append(stale, found...)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return stale, nil
}

// reconcileMetadataSizeFromJournal grows each file's metadata size up to the
// local journal's durable high-water mark (#1687 crash-safety net). It is the
// load-bearing counterpart to the relaxed (deferred-fsync) size commits done on
// WRITE/COMMIT: if the server crashed after the data was fsync'd into the journal
// but before the relaxed metadata size fsync ran, metadata.Size is stale/smaller
// than the durable data. Reading that file would truncate ACK'd bytes (#588
// class). This runs on share start, before the share is visible to handlers, and
// only ever GROWS the size — a legitimate shrink (SetAttr/truncate) commits
// strictly and is never rolled back here.
//
// bs.Local.ListFiles/FileSize are served from the journal's in-memory index
// (populated by recover() at open), so the common no-mismatch case is cheap: a
// point read of metadata per file, and a strict UpdateAttrs only on an actual gap.
func reconcileMetadataSizeFromJournal(ctx context.Context, metadataStore metadata.Store, bs *engine.Store) error {
	if bs == nil {
		return nil
	}
	localStore := bs.Local()
	if localStore == nil {
		return nil
	}
	stale, err := findStaleSizes(ctx, metadataStore, localStore, localStore.ListFiles(ctx))
	if err != nil {
		return err
	}
	for _, m := range stale {
		// Grow to the journal high-water mark under a STRICT transaction; re-read
		// inside the txn so a concurrent legitimate update can't be clobbered.
		if err := metadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
			cur, err := tx.GetFileByPayloadID(ctx, metadata.PayloadID(m.id))
			if err != nil {
				return err
			}
			if cur == nil || cur.Size >= m.journalSize {
				return nil // another writer already caught up; never shrink
			}
			cur.Size = m.journalSize
			return tx.UpdateAttrs(ctx, cur)
		}); err != nil {
			return fmt.Errorf("reconcile size for payload %s: %w", m.id, err)
		}
	}
	return nil
}

// reserveShareName claims a share name for an in-flight AddShare. It fails if
// the name is already registered or already reserved by a concurrent AddShare.
func (s *Service) reserveShareName(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.registry[name]; exists {
		return fmt.Errorf("share %q already exists", name)
	}
	if _, reserved := s.reservations[name]; reserved {
		return fmt.Errorf("share %q is already being added", name)
	}
	s.reservations[name] = struct{}{}
	return nil
}

// releaseShareName drops an in-flight AddShare reservation. Idempotent.
func (s *Service) releaseShareName(name string) {
	s.mu.Lock()
	delete(s.reservations, name)
	s.mu.Unlock()
}

// rootModeForDefaultPermission projects a share's default_permission onto the
// share root directory's POSIX permission bits. NFSv3 has no ACL transport, so
// for its clients the mode bits are the only access signal — and the Linux
// client enforces them *client-side*, before any RPC. The bits can therefore
// only restrict, never grant (the server's export gate + ACL remain the real
// authority); but a too-narrow root silently blocked legitimate access. The
// only case that needed opening is read-write: a non-root user on a read-write
// share was denied at the 0755 root before reaching the server. So read-write
// (and admin) widen "other" to rwx; read and the secure none/unset default keep
// the historical 0755 (the export gate still denies ungranted access server-
// side). Per-user grants remain NFSv4/SMB-only — mode bits cannot express them.
func rootModeForDefaultPermission(perm string) uint32 {
	switch models.SharePermission(perm) {
	case models.PermissionReadWrite, models.PermissionAdmin:
		return 0o777
	default: // none / unset / read: preserve the historical 0755 root
		return 0o755
	}
}

// prepareShare validates config, resolves the metadata store, and creates the
// root directory. Returns the built Share (not yet in the registry) and the
// metadata store. The caller (AddShare) is responsible for inserting the share
// into the registry after all initialization (including BlockStore) succeeds.
func (s *Service) prepareShare(
	ctx context.Context,
	config *ShareConfig,
	storeProvider MetadataStoreProvider,
) (*Share, metadata.Store, error) {
	// Early duplicate check (optimistic -- AddShare rechecks under write lock).
	s.mu.RLock()
	if _, exists := s.registry[config.Name]; exists {
		s.mu.RUnlock()
		return nil, nil, fmt.Errorf("share %q already exists", config.Name)
	}
	s.mu.RUnlock()

	if storeProvider == nil {
		return nil, nil, errors.New("metadata store provider not initialized")
	}

	metadataStore, err := storeProvider.GetMetadataStore(config.MetadataStore)
	if err != nil {
		return nil, nil, err
	}

	rootAttr := config.RootAttr
	if rootAttr == nil {
		rootAttr = &metadata.FileAttr{}
	}
	if rootAttr.Type == 0 {
		rootAttr.Type = metadata.FileTypeDirectory
	}
	if rootAttr.Mode == 0 {
		rootAttr.Mode = rootModeForDefaultPermission(config.DefaultPermission)
	}
	if rootAttr.Atime.IsZero() {
		now := time.Now()
		rootAttr.Atime = now
		rootAttr.Mtime = now
		rootAttr.Ctime = now
	}

	rootFile, err := metadataStore.CreateRootDirectory(ctx, config.Name, rootAttr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create root directory: %w", err)
	}

	// Propagate a read-only share into the metadata store's ShareOptions.
	// The runtime seeds metadata via CreateRootDirectory (not CreateShare),
	// which leaves Options zero-valued, so without this the permission layer
	// never sees ShareOptions.ReadOnly — the store-level discriminator that lets
	// it deny writes/creates with ErrReadOnly (EROFS) rather than ErrAccessDenied
	// (EACCES). Only touch read-only shares; read-write shares use the zero default.
	if config.ReadOnly {
		shareOpts, err := metadataStore.GetShareOptions(ctx, config.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read share options: %w", err)
		}
		shareOpts.ReadOnly = true
		if err := metadataStore.UpdateShareOptions(ctx, config.Name, shareOpts); err != nil {
			return nil, nil, fmt.Errorf("failed to set read-only share options: %w", err)
		}
	}

	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encode root handle: %w", err)
	}

	allowAuthSys := config.AllowAuthSys
	if !config.AllowAuthSysSet && !allowAuthSys {
		allowAuthSys = true
	}

	share := &Share{
		Name:                             config.Name,
		MetadataStore:                    config.MetadataStore,
		RootHandle:                       rootHandle,
		ReadOnly:                         config.ReadOnly,
		Enabled:                          config.Enabled,
		EncryptData:                      config.EncryptData,
		AclFlagInheritedCanonicalization: config.AclFlagInheritedCanonicalization,
		AccessBasedEnumeration:           config.AccessBasedEnumeration,
		ChangeNotifyDisabled:             config.ChangeNotifyDisabled,
		StreamsDisabled:                  config.StreamsDisabled,
		ContinuousAvailability:           config.ContinuousAvailability,
		AllowMFsymlink:                   config.AllowMFsymlink,
		TrashEnabled:                     config.TrashEnabled,
		TrashRetentionDays:               config.TrashRetentionDays,
		TrashRestrictToAdmin:             config.TrashRestrictToAdmin,
		TrashMaxBytes:                    config.TrashMaxBytes,
		TrashExcludePatterns:             config.TrashExcludePatterns,
		DefaultPermission:                config.DefaultPermission,
		Squash:                           config.Squash,
		AnonymousUID:                     config.AnonymousUID,
		AnonymousGID:                     config.AnonymousGID,
		DisableReaddirplus:               config.DisableReaddirplus,
		AllowAuthSys:                     allowAuthSys,
		RequireKerberos:                  config.RequireKerberos,
		MinKerberosLevel:                 config.MinKerberosLevel,
		NetgroupName:                     config.NetgroupName,
		BlockedOperations:                config.BlockedOperations,
		RetentionPolicy:                  config.RetentionPolicy,
		RetentionTTL:                     config.RetentionTTL,
	}

	return share, metadataStore, nil
}

// RemoveShare removes a share from the registry and closes its BlockStore.
// Does not close the underlying metadata store.
//
// Teardown is ordered best-effort (REVIEW M4): every step runs even if an
// earlier one fails, and the per-step errors are aggregated into the returned
// error so the registry / snapshot-dir / block-store / remote-ref state is
// never left half-removed by an early return. The registry entry is dropped
// first (under the lock) so the share disappears from routing immediately;
// the remaining steps are pure resource teardown.
//
// bs.Close() is now drain-safe: it takes the engine's lifecycle
// write lock, which blocks until all in-flight WriteAt/ReadAt/Flush ops on the
// store have completed, so calling it here outside s.mu can no longer race a
// client mid-transfer into a torn op or panic.
func (s *Service) RemoveShare(name string) error {
	s.mu.Lock()
	share, exists := s.registry[name]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("share %q not found", name)
	}
	bs := share.BlockStore
	remoteConfigID := share.remoteConfigID
	localStoreDir := share.localStoreDir
	delete(s.registry, name)
	s.blockStoreCache.Delete(name)
	s.mu.Unlock()

	// Cancel any in-flight warm job for this share so it cannot keep fetching
	// into a block store that is about to be closed.
	s.warmJobs.cancelForShare(name)

	var errs []error

	// Cleanup per-share snapshot directories alongside registry removal.
	// The DB row is the source of truth; orphaned files left behind on a
	// removal error are operationally harmless, so we log + aggregate but
	// continue with the rest of the teardown.
	if localStoreDir != "" {
		snapsDir := filepath.Join(localStoreDir, "snapshots")
		if err := os.RemoveAll(snapsDir); err != nil {
			logger.Warn("RemoveShare: failed to remove snapshots dir",
				"share", name, "dir", snapsDir, "error", err)
			errs = append(errs, fmt.Errorf("remove snapshots dir %q: %w", snapsDir, err))
		}
	}

	if bs != nil {
		if err := bs.Close(); err != nil {
			logger.Warn("Failed to close BlockStore for share", "share", name, "error", err)
			errs = append(errs, fmt.Errorf("close block store for share %q: %w", name, err))
		}
	}

	// Always release the remote-store reference even if a prior step errored,
	// otherwise a Close failure would leak the shared remote ref-count.
	if remoteConfigID != "" {
		s.releaseRemoteStore(remoteConfigID)
	}

	s.notifyShareChange()

	return errors.Join(errs...)
}

// StopRollups stops and drains the rollup worker pool of every registered
// share's block store. The runtime calls this during shutdown BEFORE it closes
// the metadata stores (#1543): the rollup ticker persists FileChunk manifests
// and rollup offsets through the metadata store, so it must be fenced first or
// an in-flight rollup races the DB close ("sql: database is closed") and can
// drop a local chunk that was never mirrored.
//
// The ctx bounds the TOTAL drain time (an overall deadline): each store is
// given the time remaining until ctx's deadline as its grace, so shutdown stays
// bounded regardless of share count. Once the budget is spent the worker-pool
// fence still runs (that is the load-bearing part — it stops the ticker); only
// the best-effort drain is skipped, and those intervals resume on restart.
//
// Best-effort — a per-share drain error is logged, not propagated, so one share
// cannot block the rest of shutdown. Drains run outside the registry lock (a
// drain can block up to its grace window). The block stores stay OPEN; their
// full teardown still happens in RemoveShare.
func (s *Service) StopRollups(ctx context.Context) {
	type namedStore struct {
		name string
		bs   *engine.Store
	}
	s.mu.RLock()
	stores := make([]namedStore, 0, len(s.registry))
	for name, share := range s.registry {
		if share.BlockStore != nil {
			stores = append(stores, namedStore{name: name, bs: share.BlockStore})
		}
	}
	s.mu.RUnlock()

	for _, ns := range stores {
		// grace = time left until the shared deadline (overall bound). No
		// deadline → 0, which defers to the store's default. Budget already
		// spent → a 1ms floor so we still fence the pool without reviving the
		// 30s default that GracefulStopRollup applies to grace <= 0.
		grace := time.Duration(0)
		if dl, ok := ctx.Deadline(); ok {
			if grace = time.Until(dl); grace <= 0 {
				grace = time.Millisecond
			}
		}
		if err := ns.bs.StopRollup(grace); err != nil {
			logger.Warn("Failed to stop rollup for share; remaining rollups resume on restart",
				"share", ns.name, "error", err)
		}
	}
}

// SeedColdFromManifest seeds a cold journal interval for every FileChunk in the
// share's metadata manifest so a subsequent read faults the bytes in from the
// remote store instead of zero-filling. It is used when the journal has no local
// copy of the data — after a snapshot restore wiped the local tier, or after a
// pre-journal upgrade archived the legacy local layout aside. Remote-backed
// shares only (the caller gates on that); the cold fetch it arms is
// BLAKE3-verified. One ListFileChunks per payload — O(chunks), acceptable for a
// rare control-plane / startup path — and the seeds themselves are batched, so
// the local tier's durable write costs one per batch rather than one per file.
//
// The report it returns is what the seed observed on the way through: how much
// it covered, how much of it the manifest does not yet call remote, and a few
// extents to read back. Seeding alone proves nothing about content, so a caller
// that archived the only local copy aside is expected to verify before it
// reports success.
func SeedColdFromManifest(ctx context.Context, bs *engine.Store, metaStore metadata.Store) (coldSeedReport, error) {
	var report coldSeedReport
	// EnumeratePayloads is a callback iteration with no cheap denominator, so
	// the heartbeat reports a running count rather than a fraction.
	started := time.Now()
	lastLog := started
	logger.Info("seeding cold intervals from the metadata manifest")
	// Extents are buffered across payloads and flushed as one durable write. The
	// bound is on extents rather than payloads because that is what the buffer
	// actually holds: a share of huge files would otherwise buffer the whole
	// manifest before it hit any payload count worth flushing at.
	//
	// It bounds what the buffering adds, not the peak: a payload whose own extents
	// exceed the bound goes in whole and flushes on the next check. Splitting one
	// would buy nothing — ListFileChunks has already materialized that payload's
	// rows in full before this sees them, so the slice is live either way.
	batch := make([]engine.ColdSeed, 0, 256)
	buffered := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := bs.SeedColdBatch(ctx, batch); err != nil {
			return fmt.Errorf("seed cold %d payloads from %s: %w", len(batch), batch[0].PayloadID, err)
		}
		batch, buffered = batch[:0], 0
		return nil
	}
	err := metaStore.EnumeratePayloads(ctx, func(payloadID string) error {
		rows, err := metaStore.ListFileChunks(ctx, payloadID)
		if err != nil {
			return fmt.Errorf("list manifest for %s: %w", payloadID, err)
		}
		extents := make([][2]int64, 0, len(rows))
		for _, row := range rows {
			if row == nil {
				continue
			}
			off, ok := block.ParseChunkOffset(row.ID)
			if !ok {
				// Which range this row describes cannot be recovered — that is what
				// makes it unplaceable — so it is logged verbatim and seeding
				// continues: one such row is not worth stranding every other share.
				// Leaving the range unseeded is safe because the engine reconciles an
				// uncovered read against the manifest whether or not the range is
				// cold, so it refuses with ErrManifestInconsistent rather than serving
				// zeros.
				logger.Error("cold seed: unplaceable manifest row, range will not be seeded",
					"payload", payloadID, "row", row.ID, "size", row.DataSize)
				report.unplaceable++
				continue
			}
			extents = append(extents, [2]int64{int64(off), int64(row.DataSize)})
			report.chunks++
			// Whether a chunk reached the remote is answered by the synced-hash
			// store, not by FileChunk.State: the carve path records synced markers
			// and leaves the row state at Pending for the life of the payload, so
			// reading State here would call every chunk unsynced. A lookup failure
			// counts as unsynced — the archive stays when we cannot tell.
			synced, serr := metaStore.IsSynced(ctx, row.Hash)
			if serr != nil || !synced {
				report.unsynced++
			}
			// Sample the first hashed extent of the first few payloads. A
			// zero-length chunk or one with no hash yet cannot be checked
			// against anything, so it is not worth a sample slot.
			if len(report.samples) < coldVerifySamples && row.DataSize > 0 &&
				row.Hash != (block.ContentHash{}) && report.sampledPayload(payloadID) {
				report.samples = append(report.samples, coldSample{
					payloadID: payloadID,
					offset:    int64(off),
					length:    int64(row.DataSize),
					hash:      row.Hash,
				})
			}
		}
		if len(extents) > 0 {
			batch = append(batch, engine.ColdSeed{PayloadID: payloadID, Extents: extents})
			buffered += len(extents)
		}
		if buffered >= coldSeedBatchExtents {
			if err := flush(); err != nil {
				return err
			}
		}
		report.payloads++
		if time.Since(lastLog) >= migrationProgressInterval {
			lastLog = time.Now()
			logger.Info("seeding cold intervals from the metadata manifest",
				"payloads", report.payloads, "chunks", report.chunks,
				"elapsed", time.Since(started).Round(time.Second))
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	return report, flush()
}
