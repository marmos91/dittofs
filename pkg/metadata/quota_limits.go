package metadata

import (
	"sync"
	"time"
)

// IdentityQuota is the runtime view of a per-identity quota limit, loaded into
// the metadata Service from the control-plane DB and consulted on the write /
// create hot path. It is protocol-agnostic. All byte/file fields are limits;
// 0 means "no limit on this dimension". Soft must be <= Limit when both are set.
//
// The grace state machine (GraceStartedAt) is mutated in place by the enforcer
// and is also persisted back to the control-plane row so it survives restart.
type IdentityQuota struct {
	// Scope selects user vs group keying. DefaultUser quotas are modeled as a
	// QuotaScopeUser entry under the sentinel DefaultUserID id.
	Scope QuotaScope
	// ID is the uid (user / default-user) or gid (group) the quota applies to.
	ID uint32

	// LimitBytes is the hard byte ceiling (0 = unlimited).
	LimitBytes int64
	// SoftBytes is the soft byte threshold (0 = no soft threshold).
	SoftBytes int64
	// LimitFiles is the hard inode (file-count) ceiling (0 = unlimited).
	LimitFiles int64
	// SoftFiles is the soft inode threshold (0 = no soft threshold).
	SoftFiles int64

	// GraceSeconds is how long usage may stay over a soft threshold before the
	// soft threshold is enforced as hard. 0 disables grace (soft acts as a
	// warning only and is never escalated).
	GraceSeconds int64
	// GraceStartedAt records when usage first crossed a soft threshold. Zero
	// means no grace timer is running.
	GraceStartedAt time.Time
}

// DefaultUserID is the sentinel uid used to key a default-user quota: a single
// QuotaScopeUser entry that applies to any uid without an explicit user quota.
// It is intentionally MaxUint32 so it never collides with a real uid in
// practice (uids are 32-bit but real-world uids never reach this value, and the
// store charges usage to actual file-owner uids, never to this sentinel).
const DefaultUserID = ^uint32(0)

// quotaLimits holds the hot-updatable per-identity quota limits for the Service.
// Keyed share -> quota-key -> *IdentityQuota. Guarded by its own mutex so the
// write/create hot path does not contend with the broad Service.mu.
type quotaLimits struct {
	mu sync.RWMutex
	// byShare[share][key] = limit
	byShare map[string]map[identityQuotaKey]*IdentityQuota

	// dynGrace holds ephemeral grace timers for default-user fallbacks, keyed by
	// the REAL uid (not the default-user sentinel) so each user gets an
	// independent grace window. These are NOT persisted: a default-user quota is
	// a shared fallback whose single DB row cannot hold per-user grace state, so
	// the timer is in-memory only and resets on restart (acceptable for the
	// fallback case; explicit user/group quotas keep their persisted per-row
	// grace). Keyed share -> (scope,realID) -> grace start.
	dynGrace map[string]map[identityQuotaKey]time.Time
}

type identityQuotaKey struct {
	scope QuotaScope
	id    uint32
}

func newQuotaLimits() *quotaLimits {
	return &quotaLimits{
		byShare:  make(map[string]map[identityQuotaKey]*IdentityQuota),
		dynGrace: make(map[string]map[identityQuotaKey]time.Time),
	}
}

// dynGraceStartedAt returns the ephemeral default-user grace timer for a real
// identity (zero if none).
func (q *quotaLimits) dynGraceStartedAt(share string, scope QuotaScope, realID uint32) time.Time {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if m := q.dynGrace[share]; m != nil {
		return m[identityQuotaKey{scope, realID}]
	}
	return time.Time{}
}

// setDynGrace sets (zero clears) the per-real-user default-user grace timer for
// a real identity. It returns whether the stored value actually changed, so the
// caller persists the durable copy only on a real transition (the clear path
// runs on every under-soft write and must not churn the DB).
func (q *quotaLimits) setDynGrace(share string, scope QuotaScope, realID uint32, t time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := identityQuotaKey{scope, realID}
	m := q.dynGrace[share]
	if t.IsZero() {
		if m == nil {
			return false
		}
		if _, ok := m[key]; !ok {
			return false
		}
		delete(m, key)
		if len(m) == 0 {
			delete(q.dynGrace, share)
		}
		return true
	}
	if m == nil {
		m = make(map[identityQuotaKey]time.Time)
		q.dynGrace[share] = m
	}
	if prev, ok := m[key]; ok && prev.Equal(t) {
		return false
	}
	m[key] = t
	return true
}

// seedDynGrace installs durable per-real-user default-user grace timers for a
// share, replacing any existing ephemeral entries. Called at share load to
// restore grace state persisted across a restart. Entries are keyed by real uid
// under QuotaScopeUser (the only scope with a default-user fallback).
func (q *quotaLimits) seedDynGrace(share string, byUID map[uint32]time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	m := make(map[identityQuotaKey]time.Time, len(byUID))
	for uid, t := range byUID {
		if t.IsZero() {
			continue
		}
		m[identityQuotaKey{QuotaScopeUser, uid}] = t
	}
	if len(m) == 0 {
		delete(q.dynGrace, share)
		return
	}
	q.dynGrace[share] = m
}

// set installs or replaces a single identity quota for a share.
func (q *quotaLimits) set(share string, iq IdentityQuota) {
	q.mu.Lock()
	defer q.mu.Unlock()
	m := q.byShare[share]
	if m == nil {
		m = make(map[identityQuotaKey]*IdentityQuota)
		q.byShare[share] = m
	}
	copyIQ := iq
	m[identityQuotaKey{iq.Scope, iq.ID}] = &copyIQ
}

// remove deletes a single identity quota for a share.
func (q *quotaLimits) remove(share string, scope QuotaScope, id uint32) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if m := q.byShare[share]; m != nil {
		delete(m, identityQuotaKey{scope, id})
		if len(m) == 0 {
			delete(q.byShare, share)
		}
	}
}

// replaceShare atomically replaces all quotas for a share (used on bulk reload).
func (q *quotaLimits) replaceShare(share string, quotas []IdentityQuota) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(quotas) == 0 {
		delete(q.byShare, share)
		return
	}
	m := make(map[identityQuotaKey]*IdentityQuota, len(quotas))
	for i := range quotas {
		copyIQ := quotas[i]
		m[identityQuotaKey{copyIQ.Scope, copyIQ.ID}] = &copyIQ
	}
	q.byShare[share] = m
}

// get returns a copy of the quota for an exact (scope,id), or false.
func (q *quotaLimits) get(share string, scope QuotaScope, id uint32) (IdentityQuota, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if m := q.byShare[share]; m != nil {
		if iq, ok := m[identityQuotaKey{scope, id}]; ok {
			return *iq, true
		}
	}
	return IdentityQuota{}, false
}

// resolveUser returns the effective user quota for a uid: the explicit user
// quota if present, otherwise the default-user quota if configured.
func (q *quotaLimits) resolveUser(share string, uid uint32) (IdentityQuota, bool) {
	if iq, ok := q.get(share, QuotaScopeUser, uid); ok {
		return iq, true
	}
	return q.get(share, QuotaScopeUser, DefaultUserID)
}

// hasAny reports whether a share has any identity quotas configured.
func (q *quotaLimits) hasAny(share string) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.byShare[share]) > 0
}

// liveGraceStartedAt returns the current persisted grace-start for a quota and
// whether the quota still exists. Used to re-read the timer under lock just
// before an enforce decision, so a concurrent clearGrace cannot leave the
// caller acting on a stale copy.
func (q *quotaLimits) liveGraceStartedAt(share string, scope QuotaScope, id uint32) (time.Time, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	m := q.byShare[share]
	if m == nil {
		return time.Time{}, false
	}
	iq := m[identityQuotaKey{scope, id}]
	if iq == nil {
		return time.Time{}, false
	}
	return iq.GraceStartedAt, true
}

// updateGraceStartedAt mutates the persisted grace timer for a stored quota and
// returns whether the value changed (so the caller can persist it back).
func (q *quotaLimits) updateGraceStartedAt(share string, scope QuotaScope, id uint32, t time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	m := q.byShare[share]
	if m == nil {
		return false
	}
	iq := m[identityQuotaKey{scope, id}]
	if iq == nil {
		return false
	}
	if iq.GraceStartedAt.Equal(t) {
		return false
	}
	iq.GraceStartedAt = t
	return true
}
