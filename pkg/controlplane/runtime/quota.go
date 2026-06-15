package runtime

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// modelQuotaToIdentityQuota converts a persisted control-plane Quota into the
// metadata service's runtime IdentityQuota. Returns ok=false for an
// unrecognized scope. The default-user scope is modeled as a QuotaScopeUser
// entry keyed by metadata.DefaultUserID.
func modelQuotaToIdentityQuota(q *models.Quota) (metadata.IdentityQuota, bool) {
	iq := metadata.IdentityQuota{
		LimitBytes:   q.LimitBytes,
		SoftBytes:    q.SoftBytes,
		LimitFiles:   q.LimitFiles,
		SoftFiles:    q.SoftFiles,
		GraceSeconds: q.GraceSeconds,
	}
	if q.GraceStartedAt != nil {
		iq.GraceStartedAt = *q.GraceStartedAt
	}
	switch q.Scope {
	case models.QuotaScopeUser:
		iq.Scope = metadata.QuotaScopeUser
		if q.IdentityID != nil {
			iq.ID = *q.IdentityID
		}
	case models.QuotaScopeGroup:
		iq.Scope = metadata.QuotaScopeGroup
		if q.IdentityID != nil {
			iq.ID = *q.IdentityID
		}
	case models.QuotaScopeDefaultUser:
		iq.Scope = metadata.QuotaScopeUser
		iq.ID = metadata.DefaultUserID
	default:
		return metadata.IdentityQuota{}, false
	}
	return iq, true
}

// LoadIdentityQuotasForShare loads all persisted per-identity quotas for a share
// from the control-plane DB into the metadata service. Called during AddShare so
// quotas are enforced immediately after a restart.
func (r *Runtime) LoadIdentityQuotasForShare(ctx context.Context, shareName string) error {
	// No control-plane store (test fixtures, embedded use): nothing to load.
	if r.store == nil {
		return nil
	}
	quotas, err := r.store.ListQuotas(ctx, shareName)
	if err != nil {
		return err
	}
	iqs := make([]metadata.IdentityQuota, 0, len(quotas))
	for _, q := range quotas {
		if iq, ok := modelQuotaToIdentityQuota(q); ok {
			iqs = append(iqs, iq)
		} else {
			logger.Warn("skipping quota with unknown scope", "share", shareName, "scope", q.Scope)
		}
	}
	r.metadataService.ReplaceIdentityQuotas(shareName, iqs)
	return nil
}

// UpdateIdentityQuota hot-updates a single per-identity quota in the metadata
// service from a persisted model row. Mirrors UpdateShareQuota.
func (r *Runtime) UpdateIdentityQuota(q *models.Quota) {
	if iq, ok := modelQuotaToIdentityQuota(q); ok {
		r.metadataService.SetIdentityQuota(q.ShareName, iq)
	}
}

// RemoveIdentityQuota removes a single per-identity quota from the metadata
// service. scope is a models scope string; identityID is nil for default-user.
func (r *Runtime) RemoveIdentityQuota(shareName, scope string, identityID *uint32) {
	mScope, id, ok := metadataScope(scope, identityID)
	if !ok {
		return
	}
	r.metadataService.RemoveIdentityQuota(shareName, mScope, id)
}

// GetIdentityQuotaUsage returns the live per-identity usage (bytes + file count)
// for the given share / scope / identity, read from the metadata store backing
// the share. scope is a models scope string; identityID is nil for default-user.
// Usage for the default-user scope is not meaningful (it is a fallback limit, not
// a real owner identity), so it always returns (0, 0). Any lookup failure (share
// not loaded, unknown scope) degrades to (0, 0) so REST responses stay
// well-formed. Mirrors GetShareUsage for the per-identity case.
func (r *Runtime) GetIdentityQuotaUsage(shareName, scope string, identityID *uint32) (bytes, files int64) {
	if scope == models.QuotaScopeDefaultUser {
		return 0, 0
	}
	mScope, id, ok := metadataScope(scope, identityID)
	if !ok {
		return 0, 0
	}
	store, err := r.metadataService.GetStoreForShare(shareName)
	if err != nil {
		return 0, 0
	}
	usage, err := store.GetQuotaUsage(mScope, id)
	if err != nil {
		return 0, 0
	}
	return usage.Bytes, usage.Files
}

// metadataScope maps a models scope string + identityID to the metadata scope +
// usage id used by the service map.
func metadataScope(scope string, identityID *uint32) (metadata.QuotaScope, uint32, bool) {
	switch scope {
	case models.QuotaScopeUser:
		if identityID == nil {
			return 0, 0, false
		}
		return metadata.QuotaScopeUser, *identityID, true
	case models.QuotaScopeGroup:
		if identityID == nil {
			return 0, 0, false
		}
		return metadata.QuotaScopeGroup, *identityID, true
	case models.QuotaScopeDefaultUser:
		return metadata.QuotaScopeUser, metadata.DefaultUserID, true
	default:
		return 0, 0, false
	}
}

// quotaGracePersister persists grace-timer transitions from the metadata
// enforcer back to the control-plane DB. Implements metadata.QuotaGracePersister.
type quotaGracePersister struct {
	rt *Runtime
}

// PersistQuotaGrace writes the new grace_started_at for a quota. A zero t clears
// the timer. The default-user sentinel uid is translated back to the
// default-user scope with a NULL identity. Best-effort: errors are logged, not
// surfaced to the write path.
func (p *quotaGracePersister) PersistQuotaGrace(shareName string, scope metadata.QuotaScope, id uint32, t time.Time) {
	modelScope := models.QuotaScopeGroup
	var identityID *uint32
	if scope == metadata.QuotaScopeUser {
		if id == metadata.DefaultUserID {
			modelScope = models.QuotaScopeDefaultUser
		} else {
			modelScope = models.QuotaScopeUser
			v := id
			identityID = &v
		}
	} else {
		v := id
		identityID = &v
	}

	var tp *time.Time
	if !t.IsZero() {
		tp = &t
	}
	// Use a short bounded context: this runs off the write hot path.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.rt.store.SetQuotaGraceStartedAt(ctx, shareName, modelScope, identityID, tp); err != nil {
		logger.Warn("failed to persist quota grace timer",
			"share", shareName, "scope", modelScope, "error", err)
	}
}
