package shares

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func (s *Service) UpdateShare(name string, readOnly *bool, defaultPermission *string, retentionPolicy *block.RetentionPolicy, retentionTTL *time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	share, exists := s.registry[name]
	if !exists {
		return fmt.Errorf("share %q not found", name)
	}

	if readOnly != nil {
		share.ReadOnly = *readOnly
	}
	if defaultPermission != nil {
		share.DefaultPermission = *defaultPermission
	}
	if retentionPolicy != nil {
		share.RetentionPolicy = *retentionPolicy
	}
	if retentionTTL != nil {
		share.RetentionTTL = *retentionTTL
	}

	// Only the pin/non-pin distinction reaches the block store: the local tier
	// evicts whole fully-synced segments approx-LRU and has no ttl or lru knob to
	// receive, so the rest of the policy is metadata the API reports back.
	if (retentionPolicy != nil || retentionTTL != nil) && share.BlockStore != nil {
		// Pin mode disables eviction; switching away from pin re-enables it
		// (unless the share is local-only, in which case eviction stays disabled).
		if share.RetentionPolicy == block.RetentionPin {
			share.BlockStore.SetEvictionEnabled(false)
		} else if share.BlockStore.HasRemoteStore() {
			share.BlockStore.SetEvictionEnabled(true)
		}
	}

	return nil
}

// SetShareSquash updates the live in-memory squash policy (and optionally the
// anonymous UID/GID) for a share, so an NFS squash-config change applies to
// active clients without an adapter restart. The on-disk config is persisted
// separately by the API handler; this only refreshes the runtime Share that
// ResolveSharePermission / ApplyIdentityMapping read from. anonUID/anonGID are
// pointers so an unspecified field is left unchanged. Mirrors UpdateShare's
// lock+mutate pattern; GetShare hands callers a snapshot copy, so mutating the
// registry entry under the lock is race-free.
func (s *Service) SetShareSquash(name string, squash models.SquashMode, anonUID, anonGID *uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	share, exists := s.registry[name]
	if !exists {
		return fmt.Errorf("share %q not found", name)
	}

	share.Squash = squash
	if anonUID != nil {
		share.AnonymousUID = *anonUID
	}
	if anonGID != nil {
		share.AnonymousGID = *anonGID
	}

	return nil
}

// TrashSettings is a per-share recycle-bin policy snapshot, returned by value
// under the service lock so callers never read a mutating shared pointer.
type TrashSettings struct {
	Enabled         bool
	RetentionDays   int
	RestrictToAdmin bool
	MaxBytes        int64
	ExcludePatterns []string
}

// TrashSettingsForShare returns the recycle-bin policy for a share, read under
// the service lock. Returns ok=false if the share is unknown. Never hands out
// the *Share; the ExcludePatterns slice is copied so callers cannot observe a
// concurrent SetShareTrashConfig mutation.
//
// The recycle decision is read per-delete from many protocol goroutines while
// config can be updated live, so this accessor returns a VALUE under the lock
// (refs #190, #936 race lesson).
func (s *Service) TrashSettingsForShare(name string) (TrashSettings, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	share, exists := s.registry[name]
	if !exists {
		return TrashSettings{}, false
	}
	return TrashSettings{
		Enabled:         share.TrashEnabled,
		RetentionDays:   share.TrashRetentionDays,
		RestrictToAdmin: share.TrashRestrictToAdmin,
		MaxBytes:        share.TrashMaxBytes,
		ExcludePatterns: append([]string(nil), share.TrashExcludePatterns...),
	}, true
}

// EnabledTrashShares returns the names of all shares with trash enabled, read
// under the service lock. Used by the reaper loop to enumerate bins to sweep.
func (s *Service) EnabledTrashShares() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for name, share := range s.registry {
		if share.TrashEnabled {
			out = append(out, name)
		}
	}
	return out
}

// SetShareTrashConfig updates a live share's recycle-bin settings under the
// write lock and persists them to the DB. Pairs safely with the RLock
// TrashSettingsForShare accessor: the exact fields the reader copies are the
// ones this mutates while holding the write lock.
//
// Returns ErrShareNotFound if the share name is unknown in the runtime
// registry. The optional store argument persists the change; pass nil to
// update runtime state only (used by tests that bypass the DB).
func (s *Service) SetShareTrashConfig(store ShareStore, name string, cfg TrashSettings) error {
	excludes := append([]string(nil), cfg.ExcludePatterns...)

	s.mu.Lock()
	share, exists := s.registry[name]
	if !exists {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	share.TrashEnabled = cfg.Enabled
	share.TrashRetentionDays = cfg.RetentionDays
	share.TrashRestrictToAdmin = cfg.RestrictToAdmin
	share.TrashMaxBytes = cfg.MaxBytes
	share.TrashExcludePatterns = excludes
	s.mu.Unlock()

	if store == nil {
		return nil
	}

	dbShare, err := store.GetShare(context.Background(), name)
	if err != nil {
		return fmt.Errorf("load share %q: %w", name, err)
	}
	dbShare.TrashEnabled = cfg.Enabled
	dbShare.TrashRetentionDays = cfg.RetentionDays
	dbShare.TrashRestrictToAdmin = cfg.RestrictToAdmin
	dbShare.TrashMaxBytes = cfg.MaxBytes
	dbShare.SetTrashExcludePatterns(excludes)
	if err := store.UpdateShare(context.Background(), dbShare); err != nil {
		return fmt.Errorf("persist trash config for share %q: %w", name, err)
	}
	return nil
}

// SetShareNetgroup updates the live netgroup association for a share's NFS
// export. An empty netgroupName clears the association (allow-all). The change
// takes effect immediately: CheckNetgroupAccess reads NetgroupName from this
// runtime registry, so subsequent NFS operations honour the new allowlist
// without an adapter restart. Returns an error if the share is unknown.
func (s *Service) SetShareNetgroup(name, netgroupName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	share, exists := s.registry[name]
	if !exists {
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	share.NetgroupName = netgroupName
	return nil
}

// GetShareNetgroupName returns the live netgroup association for a share,
// read under s.mu. Callers (e.g. CheckNetgroupAccess) must use this rather
// than reading NetgroupName off a *Share returned by GetShare: GetShare hands
// back the shared registry pointer with the lock already dropped, so reading
// the field there races with SetShareNetgroup's write under s.mu. The bool is
// false when the share is unknown.
func (s *Service) GetShareNetgroupName(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	share, exists := s.registry[name]
	if !exists {
		return "", false
	}
	return share.NetgroupName, true
}

// DisableShare sets enabled=false on the share's DB row and runtime Share
// struct, then invokes notifyShareChange so adapters drop active sessions.
// DB-first-then-runtime ordering is crash-consistent: if the process dies
// between the two, the next boot reconciles runtime from DB.
//
// Idempotent: re-calling on an already-disabled share returns
// ErrShareAlreadyDisabled without writing to DB or disturbing adapters.
//
// Returns ErrShareNotFound if the share name is unknown at either layer.
// Timeout bound is whatever the caller provides via ctx.
//
// Requires `"enabled"` in GORMStore.UpdateShare's update whitelist —
// otherwise the store.UpdateShare call silently drops the flag.
func (s *Service) DisableShare(ctx context.Context, store ShareStore, name string) error {
	// Runtime registry must know the share before we touch the DB — prevents
	// a DB-disabled/runtime-absent inconsistency when the startup load missed
	// a share (partial boot) or the caller passed a stale name.
	s.mu.RLock()
	_, exists := s.registry[name]
	s.mu.RUnlock()
	if !exists {
		return fmt.Errorf("%w: runtime registry: %q", ErrShareNotFound, name)
	}

	dbShare, err := store.GetShare(ctx, name)
	if err != nil {
		return fmt.Errorf("load share %q: %w", name, err)
	}
	if !dbShare.Enabled {
		return ErrShareAlreadyDisabled
	}
	dbShare.Enabled = false
	if err := store.UpdateShare(ctx, dbShare); err != nil {
		return fmt.Errorf("persist disabled state for share %q: %w", name, err)
	}

	s.mu.Lock()
	share, stillExists := s.registry[name]
	if !stillExists {
		s.mu.Unlock()
		return fmt.Errorf("%w: runtime registry: %q", ErrShareNotFound, name)
	}
	share.Enabled = false
	s.mu.Unlock()

	s.notifyShareChange()
	return nil
}

// EnableShare inverts DisableShare. Idempotent: re-calling on an
// already-enabled share is a no-op (returns nil, no DB write).
func (s *Service) EnableShare(ctx context.Context, store ShareStore, name string) error {
	// Registry-first check: same rationale as DisableShare — avoid a DB row
	// that moves while the runtime has no matching entry.
	s.mu.RLock()
	_, exists := s.registry[name]
	s.mu.RUnlock()
	if !exists {
		return fmt.Errorf("%w: runtime registry: %q", ErrShareNotFound, name)
	}

	dbShare, err := store.GetShare(ctx, name)
	if err != nil {
		return fmt.Errorf("load share %q: %w", name, err)
	}
	if dbShare.Enabled {
		return nil
	}
	dbShare.Enabled = true
	if err := store.UpdateShare(ctx, dbShare); err != nil {
		return fmt.Errorf("persist enabled state for share %q: %w", name, err)
	}

	s.mu.Lock()
	share, stillExists := s.registry[name]
	if !stillExists {
		s.mu.Unlock()
		return fmt.Errorf("%w: runtime registry: %q", ErrShareNotFound, name)
	}
	share.Enabled = true
	s.mu.Unlock()

	s.notifyShareChange()
	return nil
}

// IsShareEnabled returns the runtime Enabled flag for the named share.
// Mirror of GetShare read-path discipline (RLock + registry lookup).
func (s *Service) IsShareEnabled(name string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	share, exists := s.registry[name]
	if !exists {
		return false, fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	return share.Enabled, nil
}

// ListEnabledSharesForStore returns the names of all runtime shares that
// (a) have Enabled=true AND (b) reference metadataStoreName as their
// metadata store.
func (s *Service) ListEnabledSharesForStore(metadataStoreName string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for name, share := range s.registry {
		if share.Enabled && share.MetadataStore == metadataStoreName {
			out = append(out, name)
		}
	}
	return out
}
