package shares

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// InjectShareForTesting inserts a share directly, bypassing AddShare validation.
func (s *Service) InjectShareForTesting(share *Share) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry[share.Name] = share
}

// RegisterShareForTesting inserts a minimal enabled Share into the registry
// without the full AddShare composition (no block/remote stores, no DB row).
// Test-only — used by handler unit tests that register a metadata store via
// MetadataService.RegisterStoreForShare and need the share to also resolve in
// identity mapping and permission lookups (the production invariant: every
// share with a metadata store is a registered share). If the share already
// exists it is left untouched.
func (s *Service) RegisterShareForTesting(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.registry[name]; exists {
		return
	}
	s.registry[name] = &Share{
		Name:              name,
		Enabled:           true,
		DefaultPermission: "read-write",
		Squash:            models.SquashNone,
		// Mirror the AddShare default (allowAuthSys defaults true when unset):
		// a bare test share must permit AUTH_SYS, otherwise the v4 export
		// auth-flavor policy denies every AUTH_UNIX op.
		AllowAuthSys: true,
	}
}

// SetLocalStoreDirForTesting overrides the per-share localStoreDir field.
// Test-only — used by handler unit tests that bypass the full
// AddShare composition path (which requires a DB-backed
// BlockStoreConfig). Returns ErrShareNotFound if the share is not
// registered.
func (s *Service) SetLocalStoreDirForTesting(name, dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	share, ok := s.registry[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	share.localStoreDir = dir
	return nil
}

// SetBlockStoreForTesting overrides the per-share BlockStore field under the
// service lock. Test-only — used by tests that compose a minimal BlockStore
// out of band and must publish it into the registry. Production code sets
// BlockStore only during AddShare. Returns ErrShareNotFound if the share is
// not registered.
//
// Use this instead of mutating a *Share returned by GetShare: GetShare now
// hands back a snapshot copy, so registry state must be changed through a
// locked setter.
func (s *Service) SetBlockStoreForTesting(name string, bs *engine.Store) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	share, ok := s.registry[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	share.BlockStore = bs
	return nil
}

// SetEnabledForTesting overrides the per-share Enabled flag under the service
// lock. Test-only — production code flips Enabled via DisableShare/EnableShare
// (which also persist the DB row). Use this instead of mutating a *Share
// returned by GetShare, which is now a snapshot copy. Returns ErrShareNotFound
// if the share is not registered.
func (s *Service) SetEnabledForTesting(name string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	share, ok := s.registry[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	share.Enabled = enabled
	return nil
}

// SetSharePolicyForTesting overrides a share's DefaultPermission and Squash mode
// in the registry under lock. Test-only — lets NFS auth tests exercise the
// export-squash permission policy without a full AddShare flow.
func (s *Service) SetSharePolicyForTesting(name, defaultPermission string, squash models.SquashMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	share, ok := s.registry[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	share.DefaultPermission = defaultPermission
	share.Squash = squash
	return nil
}

// SetExportAuthPolicyForTesting overrides a share's AllowAuthSys and
// RequireKerberos export auth-flavor fields in the registry under lock.
// Test-only — lets NFS auth tests exercise the per-share export auth-flavor
// policy (NFS4ERR_WRONGSEC enforcement) without a full AddShare flow. Use this
// instead of mutating a *Share returned by GetShare, which is now a snapshot
// copy. Returns ErrShareNotFound if the share is not registered.
func (s *Service) SetExportAuthPolicyForTesting(name string, allowAuthSys, requireKerberos bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	share, ok := s.registry[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	share.AllowAuthSys = allowAuthSys
	share.RequireKerberos = requireKerberos
	return nil
}

// SetMinKerberosLevelForTesting overrides a share's MinKerberosLevel (the GSS
// protection floor: "", "krb5", "krb5i", "krb5p") in the registry under lock.
// Test-only — lets NFS auth tests exercise the min_kerberos_level floor
// (NFS4ERR_WRONGSEC / MOUNT refusal) without a full AddShare flow. Returns
// ErrShareNotFound if the share is not registered.
func (s *Service) SetMinKerberosLevelForTesting(name, minLevel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	share, ok := s.registry[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	share.MinKerberosLevel = minLevel
	return nil
}

// SetShareRemoteForTest installs a remote.RemoteStore for the named share
// and registers it under a synthetic configID derived from the store's
// pointer identity. Two calls with the same remote store for different
// shares share one configID — matching production ref-counting behavior
// — so DistinctRemoteStores() dedupes correctly.
//
// Test-only: panics if the share does not exist. Intended for runtime-
// package tests that need to exercise RunBlockGC's enumeration without
// standing up a full engine.Store. Not safe for production callers.
func (s *Service) SetShareRemoteForTest(shareName string, rs remote.RemoteStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	share, ok := s.registry[shareName]
	if !ok {
		panic(fmt.Sprintf("SetShareRemoteForTest: share %q not registered", shareName))
	}
	// Derive a stable configID from the remote store pointer so calls that
	// pass the same rs for different shares land in the same sharedRemote
	// bucket (mirroring production ref-count semantics).
	cid := fmt.Sprintf("test-remote-%p", rs)
	if existing, ok := s.remoteStores[cid]; ok {
		existing.refCount++
	} else {
		s.remoteStores[cid] = &sharedRemote{
			store:    rs,
			refCount: 1,
		}
	}
	share.remoteConfigID = cid
}
