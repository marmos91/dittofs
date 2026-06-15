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

	if !overSoft {
		// Under all soft thresholds: clear any running grace timer.
		s.clearGrace(shareName, iq.Scope, iq.ID, &iq)
		return nil
	}

	// Over a soft threshold. With no grace configured, soft is advisory only —
	// allow the op (the hard ceiling above is the real block).
	if iq.GraceSeconds <= 0 {
		return nil
	}

	now := time.Now()
	// Re-read the grace timer under lock: a concurrent clearGrace (because this
	// identity's usage dropped below soft on another write) may have reset it
	// since iq was copied out of the map, so the local copy can be stale.
	graceStart, ok := s.identityQuotas.liveGraceStartedAt(shareName, iq.Scope, iq.ID)
	if !ok {
		// Quota was removed concurrently — nothing to enforce.
		return nil
	}
	if graceStart.IsZero() {
		// Start the grace window now; allow this op.
		s.startGrace(shareName, iq.Scope, iq.ID, &iq, now)
		return nil
	}
	// Grace running: enforce soft-as-hard once the window has elapsed.
	if now.After(graceStart.Add(time.Duration(iq.GraceSeconds) * time.Second)) {
		return &StoreError{Code: ErrQuotaExceeded, Message: "quota exceeded (" + scope.String() + " grace period expired)"}
	}
	return nil
}

func (s *Service) startGrace(shareName string, scope QuotaScope, id uint32, iq *IdentityQuota, t time.Time) {
	if s.identityQuotas.updateGraceStartedAt(shareName, scope, id, t) {
		iq.GraceStartedAt = t
		s.persistGrace(shareName, scope, id, t)
	}
}

func (s *Service) clearGrace(shareName string, scope QuotaScope, id uint32, iq *IdentityQuota) {
	if iq.GraceStartedAt.IsZero() {
		return
	}
	if s.identityQuotas.updateGraceStartedAt(shareName, scope, id, time.Time{}) {
		iq.GraceStartedAt = time.Time{}
		s.persistGrace(shareName, scope, id, time.Time{})
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
