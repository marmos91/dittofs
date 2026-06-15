package metadata

import "time"

// quotaDimension is one enforceable axis of a quota check (bytes or inodes).
type quotaDimension struct {
	name  string // "bytes" or "files", for diagnostics
	used  int64  // current usage on this axis
	delta int64  // amount this op would add (>0)
	soft  int64  // soft threshold (0 = none)
	hard  int64  // hard ceiling (0 = unlimited)
}

// checkIdentityQuotas enforces the per-user and per-group quotas (if any) for a
// file owner. delta is the byte increase; fileDelta is the inode increase (1 for
// a create, 0 for a pure write/resize). It returns ErrQuotaExceeded when a hard
// limit (or an expired soft+grace) would be crossed.
//
// Enforcement is best-effort, matching the existing per-share soft quota in
// PrepareWrite: under concurrency a few ops may slip past simultaneously and
// briefly exceed the limit until usage catches up. Hard atomic enforcement is
// impractical for userspace NFS/SMB. Truncate/shrink (delta<=0 && fileDelta<=0)
// is always allowed.
func (s *Service) checkIdentityQuotas(shareName string, store Store, uid, gid uint32, byteDelta, fileDelta int64) error {
	if byteDelta <= 0 && fileDelta <= 0 {
		return nil
	}
	if !s.identityQuotas.hasAny(shareName) {
		return nil
	}

	// User quota: explicit uid quota, else default-user fallthrough.
	if iq, ok := s.identityQuotas.resolveUser(shareName, uid); ok {
		if err := s.enforceOne(shareName, store, QuotaScopeUser, uid, iq, byteDelta, fileDelta); err != nil {
			return err
		}
	}
	// Group quota (no default-group concept).
	if iq, ok := s.identityQuotas.get(shareName, QuotaScopeGroup, gid); ok {
		if err := s.enforceOne(shareName, store, QuotaScopeGroup, gid, iq, byteDelta, fileDelta); err != nil {
			return err
		}
	}
	return nil
}

// enforceOne applies the soft/hard/grace state machine for a single resolved
// quota against the live usage for (scope, usageID). usageID is the identity the
// usage counter is keyed by (the real uid/gid — NOT the default-user sentinel).
func (s *Service) enforceOne(shareName string, store Store, scope QuotaScope, usageID uint32, iq IdentityQuota, byteDelta, fileDelta int64) error {
	usage, err := store.GetQuotaUsage(scope, usageID)
	if err != nil {
		// Usage lookup failure must not wedge writes; treat as no-quota.
		return nil
	}

	dims := []quotaDimension{
		{name: "bytes", used: usage.Bytes, delta: byteDelta, soft: iq.SoftBytes, hard: iq.LimitBytes},
		{name: "files", used: usage.Files, delta: fileDelta, soft: iq.SoftFiles, hard: iq.LimitFiles},
	}

	overSoft := false
	for _, d := range dims {
		if d.delta <= 0 {
			continue
		}
		projected := d.used + d.delta
		// Hard ceiling always blocks.
		if d.hard > 0 && projected > d.hard {
			return &StoreError{Code: ErrQuotaExceeded, Message: "quota exceeded (" + scope.String() + " " + d.name + " hard limit)"}
		}
		// Soft threshold crossed.
		if d.soft > 0 && projected > d.soft {
			overSoft = true
		}
	}

	// A default-user fallback (the resolved quota is keyed by the DefaultUserID
	// sentinel) must track grace PER REAL USER, not on the shared sentinel row —
	// otherwise one user's soft breach would start/expire grace for everyone.
	// Such grace is ephemeral (in-memory, keyed by the real usageID). Explicit
	// user/group quotas use the persisted per-row grace.
	isDefaultUserFallback := scope == QuotaScopeUser && iq.ID == DefaultUserID

	if !overSoft {
		// Under all soft thresholds: clear any running grace timer.
		s.clearGrace(shareName, scope, usageID, isDefaultUserFallback, iq.ID)
		return nil
	}

	// Over a soft threshold. With no grace configured, soft is advisory only —
	// allow the op (the hard ceiling above is the real block).
	if iq.GraceSeconds <= 0 {
		return nil
	}

	now := time.Now()
	graceStart := s.liveGrace(shareName, scope, usageID, isDefaultUserFallback, iq.ID)
	if graceStart.IsZero() {
		// Start the grace window now; allow this op.
		s.startGrace(shareName, scope, usageID, isDefaultUserFallback, iq.ID, now)
		return nil
	}
	// Grace running: enforce soft-as-hard once the window has elapsed.
	if now.After(graceStart.Add(time.Duration(iq.GraceSeconds) * time.Second)) {
		return &StoreError{Code: ErrQuotaExceeded, Message: "quota exceeded (" + scope.String() + " grace period expired)"}
	}
	return nil
}

// liveGrace reads the current grace start for an enforced identity: the
// ephemeral per-real-user timer for a default-user fallback, otherwise the
// persisted per-row timer. Re-reading here (rather than trusting the copied iq)
// avoids acting on a value a concurrent clear has already reset.
func (s *Service) liveGrace(shareName string, scope QuotaScope, usageID uint32, isDefaultUserFallback bool, rowID uint32) time.Time {
	if isDefaultUserFallback {
		return s.identityQuotas.dynGraceStartedAt(shareName, scope, usageID)
	}
	t, _ := s.identityQuotas.liveGraceStartedAt(shareName, scope, rowID)
	return t
}

func (s *Service) startGrace(shareName string, scope QuotaScope, usageID uint32, isDefaultUserFallback bool, rowID uint32, t time.Time) {
	if isDefaultUserFallback {
		s.identityQuotas.setDynGrace(shareName, scope, usageID, t)
		return
	}
	if s.identityQuotas.updateGraceStartedAt(shareName, scope, rowID, t) {
		s.persistGrace(shareName, scope, rowID, t)
	}
}

func (s *Service) clearGrace(shareName string, scope QuotaScope, usageID uint32, isDefaultUserFallback bool, rowID uint32) {
	if isDefaultUserFallback {
		s.identityQuotas.setDynGrace(shareName, scope, usageID, time.Time{})
		return
	}
	if s.identityQuotas.updateGraceStartedAt(shareName, scope, rowID, time.Time{}) {
		s.persistGrace(shareName, scope, rowID, time.Time{})
	}
}

func (s *Service) persistGrace(shareName string, scope QuotaScope, id uint32, t time.Time) {
	s.mu.RLock()
	p := s.quotaGracePersist
	s.mu.RUnlock()
	if p != nil {
		p.PersistQuotaGrace(shareName, scope, id, t)
	}
}
