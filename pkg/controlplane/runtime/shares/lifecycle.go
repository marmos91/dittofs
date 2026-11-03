package shares

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
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
) error {
	// Fold the name before anything keys off it: spellings differing only in
	// leading slashes share a state directory, so the second one must fail the
	// reservation below as a duplicate instead of adopting the first one's.
	//
	// The fold touches only leading slashes. The name arrives from a persisted
	// share row, where a '%' is a character and not an escape, so decoding it
	// would key the state directory and the registry off a name no row holds.
	config.Name = metadata.NormalizeShareName(config.Name)

	// A share whose name cannot encode a file handle can never serve a file, so
	// reject it before any state is created rather than at the first handle mint.
	if err := metadata.ValidateShareName(config.Name); err != nil {
		return err
	}

	// Every share references the block store its content is to be kept in.
	if config.BlockStoreID == "" {
		return fmt.Errorf("share %q has no block store: every share must reference one", config.Name)
	}
	if blockStoreProvider == nil {
		return fmt.Errorf("block store provider is required to resolve the block store for share %q", config.Name)
	}

	if metadataSvc == nil {
		return fmt.Errorf("metadata service registrar is required for share %q", config.Name)
	}

	// Phase 0: Reserve the share name under s.mu BEFORE any side-effecting init
	// (root-dir create, remote acquire, metadata RegisterStoreForShare), so two
	// racing callers cannot both run RegisterStoreForShare (last writer wins on
	// the MetadataService store map) and leave the metadata service pointing at a
	// different store than the registry exposes. The loser fails before touching
	// any shared store.
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

	// Phase 2: Create the share's state directory, which holds the migration
	// journal, the persistent gc-state and the snapshots tree. None when no
	// root is configured.
	if localStoreDefaults != nil && localStoreDefaults.JournalRoot != "" {
		dir := ShareJournalDir(localStoreDefaults.JournalRoot, config.Name)
		if err := checkUnderJournalRoot(localStoreDefaults.JournalRoot, dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create share directory: %w", err)
		}
		share.localStoreDir = dir
		share.gcStateRoot = filepath.Join(dir, "gc-state")
	}
	share.writeback = config.RelaxedMetadataCommit

	// Acquire the share's ref-counted remote store. Block GC reaches the remote
	// through it.
	_, remoteConfigID, err := s.acquireRemoteStore(ctx, config.BlockStoreID, blockStoreProvider)
	if err != nil {
		return fmt.Errorf("failed to create remote store for share %q: %w", config.Name, err)
	}
	share.remoteConfigID = remoteConfigID

	// cleanupShare releases resources for a share that failed to fully initialize.
	cleanupShare := func() {
		s.releaseRemoteStore(remoteConfigID)
	}

	// Phase 3: Register metadata store. Safe to call unconditionally now: the
	// Phase-0 reservation guarantees no other AddShare for this name is in
	// flight, so this RegisterStoreForShare cannot be raced into a
	// last-writer-wins mismatch.
	if err := metadataSvc.RegisterStoreForShare(config.Name, metadataStore); err != nil {
		cleanupShare()
		return fmt.Errorf("failed to configure metadata for share: %w", err)
	}

	// Apply the share's relaxed metadata commit tier. Set explicitly (true or
	// false) so a re-add that toggles it off is honored. Only the concrete
	// *metadata.Service implements the setter.
	if wb, ok := metadataSvc.(MetadataWritebackSetter); ok {
		wb.SetShareWriteback(config.Name, share.writeback)
	}

	// Phase 4: Convert the reservation into a registry entry under s.mu.
	// Only now is the share visible to protocol handlers. The reservation has
	// held the name exclusively since Phase 0, so registry[name] cannot already
	// exist here; we assert it defensively and hand off the reservation.
	s.mu.Lock()
	_, exists := s.registry[config.Name]
	// Shutdown may have run while the phases above were running; a share
	// published now would outlive the fence, so it is dropped instead — its
	// persisted row is still there for the next boot to load.
	closed := s.closed
	if closed || exists {
		// The exists branch should be unreachable while the reservation is
		// held, but stay fail-safe: tear down rather than overwrite an existing
		// share.
		s.mu.Unlock()
		cleanupShare()
		// Deregister the metadata store we just published so we do not leak a
		// registration for a share we are refusing to finalize.
		if remover, ok := metadataSvc.(MetadataServiceDeregistrar); ok {
			remover.RemoveStoreForShare(config.Name)
		}
		if closed {
			return fmt.Errorf("cannot add share %q: %w", config.Name, ErrShuttingDown)
		}
		return fmt.Errorf("share %q already exists", config.Name)
	}
	s.registry[config.Name] = share
	delete(s.reservations, config.Name)
	reservationHeld = false
	s.mu.Unlock()

	logger.Info("Share registered",
		"share", config.Name,
		"retention", config.RetentionPolicy,
		"retention_ttl", config.RetentionTTL)

	s.notifyShareChange()

	return nil
}

// reserveShareName claims a share name for an in-flight AddShare. It fails if
// the name is already registered or already reserved by a concurrent AddShare.
func (s *Service) reserveShareName(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Refuse before any side-effecting init, so a share arriving during shutdown
	// never opens a journal or starts a dispatcher it would have to tear down.
	// The publish in Phase 4 checks again: the fence can fall between here and
	// there.
	if s.closed {
		return fmt.Errorf("cannot add share %q: %w", name, ErrShuttingDown)
	}
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

// rootExists reports whether the metadata store already holds a root directory
// for shareName. A lookup that fails for any reason other than the share being
// absent is returned as an error rather than read as "absent": a share must not
// be handed a fresh root because its store could not be read.
func rootExists(ctx context.Context, store metadata.Store, shareName string) (bool, error) {
	_, err := store.GetRootHandle(ctx, shareName)
	switch {
	case err == nil:
		return true, nil
	case metadata.IsNotFoundError(err):
		return false, nil
	default:
		return false, err
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

	// decision: the fold claims the journal directory and the name reservation,
	// never an existing metadata namespace. A share keys its metadata — the root,
	// every inode under it, every handle it has handed out — by the name as
	// written, so minting a root under the folded name while the unfolded one
	// already has one would leave the real files addressable by nothing. Refuse
	// that share rather than serve it empty. Renaming the namespace would be the
	// other way out; it reissues every handle, and a handle must survive a
	// restart. The spellings are compared in the store, not against the name
	// as it was persisted, so correcting the persisted name alone does not
	// silence this.
	unfolded := strings.TrimPrefix(config.Name, "/")
	foldedRoot, err := rootExists(ctx, metadataStore, config.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to look up the root of share %q: %w", config.Name, err)
	}
	if !foldedRoot {
		unfoldedRoot, err := rootExists(ctx, metadataStore, unfolded)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to look up the root of share %q: %w", unfolded, err)
		}
		if unfoldedRoot {
			return nil, nil, fmt.Errorf(
				"share %q would be served from a new, empty root: its files are keyed by %q, which this build addresses as %q",
				config.Name, unfolded, config.Name)
		}

		// decision: a root under the name's decoded spelling is reported, not
		// refused, because unlike the slash-trimmed spelling above it is not
		// evidence on its own. No registered share is ever named without a
		// leading slash, so a root under one can only be a leftover; a decoded
		// spelling is an ordinary name, so "/a b" may equally be a live sibling
		// of "/a%20b" that owns that root outright. Refusing on it would make
		// two lawful shares block each other by load order, and the seam cannot
		// tell the two apart — it sees one metadata store, not the set of share
		// rows. Promote this to a refusal from somewhere that holds every row
		// and can say the spelling belongs to no other share.
		if decoded, derr := url.PathUnescape(config.Name); derr == nil {
			if folded := metadata.NormalizeShareName(decoded); folded != config.Name {
				decodedRoot, rerr := rootExists(ctx, metadataStore, folded)
				if rerr != nil {
					return nil, nil, fmt.Errorf("failed to look up the root of share %q: %w", folded, rerr)
				}
				if decodedRoot {
					logger.Warn("share has no root of its own while its decoded spelling does; "+
						"if no share by that name exists, this one is being served empty and its files are keyed by the other name",
						"share", config.Name, "decoded", folded)
				}
			}
		}
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

// RemoveShare removes a share from the registry and releases its remote store.
// Does not close the underlying metadata store.
//
// Teardown is ordered best-effort (REVIEW M4): every step runs even if an
// earlier one fails, and the per-step errors are aggregated into the returned
// error so the registry / snapshot-dir / remote-ref state is never left
// half-removed by an early return. The registry entry is dropped first (under
// the lock) so the share disappears from routing immediately; the remaining
// steps are pure resource teardown.
func (s *Service) RemoveShare(name string) error {
	s.mu.Lock()
	share, exists := s.registry[name]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("share %q not found", name)
	}
	remoteConfigID := share.remoteConfigID
	localStoreDir := share.localStoreDir
	delete(s.registry, name)
	s.mu.Unlock()

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

	// Always release the remote-store reference even if a prior step errored,
	// otherwise a failure would leak the shared remote ref-count.
	if remoteConfigID != "" {
		s.releaseRemoteStore(remoteConfigID)
	}

	s.notifyShareChange()
	// The share is gone, so every access decision an adapter resolved against
	// it is stale. Raised after the registry entry is dropped so a subscriber's
	// re-resolve cannot find the share it is being told to forget.
	s.InvalidateAuthCache()

	return errors.Join(errs...)
}

// CloseBlockStores is the shutdown fence: once it has run no share is
// published any more, so nothing added during shutdown outlives the metadata
// stores the shutdown is about to close. It never returns early; ctx is kept
// for the lifecycle's closer contract.
func (s *Service) CloseBlockStores(_ context.Context) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}
