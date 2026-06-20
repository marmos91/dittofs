package store

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"gorm.io/gorm/clause"
)

// GetSIDMapping returns the durable mapping for a foreign domain SID.
// Returns models.ErrSIDMappingNotFound when no mapping exists.
func (s *GORMStore) GetSIDMapping(ctx context.Context, sid string) (*models.SIDMapping, error) {
	var m models.SIDMapping
	err := s.db.WithContext(ctx).Where("sid = ?", sid).First(&m).Error
	if err != nil {
		return nil, convertNotFoundError(err, models.ErrSIDMappingNotFound)
	}
	return &m, nil
}

// ListSIDMappings returns all durable foreign-SID mappings.
func (s *GORMStore) ListSIDMappings(ctx context.Context) ([]*models.SIDMapping, error) {
	return listAll[models.SIDMapping](s.db, ctx)
}

// AllocateSIDMapping idempotently binds a foreign domain SID to a stable Unix
// UID/GID and returns the durable mapping.
//
// The binding is keyed on the full SID and is allocated EXACTLY ONCE: if a
// mapping already exists for the SID, the existing UnixID/IsGroup is returned
// unchanged regardless of the requested value. This enforces the never-remap
// invariant — re-resolving a foreign SID always yields the same identity, so a
// principal can never be silently re-attributed to files owned by a different
// UID/GID.
//
// The first caller for a given SID wins under concurrency: the insert uses
// ON CONFLICT DO NOTHING and the row is re-read, so a racing caller observes
// the winner's allocation rather than overwriting it.
func (s *GORMStore) AllocateSIDMapping(ctx context.Context, sid string, unixID uint32, isGroup bool, displayName string) (*models.SIDMapping, error) {
	// Fast path: return any existing binding without touching it.
	if existing, err := s.GetSIDMapping(ctx, sid); err == nil {
		return existing, nil
	} else if !errors.Is(err, models.ErrSIDMappingNotFound) {
		return nil, err
	}

	row := &models.SIDMapping{
		SID:         sid,
		UnixID:      unixID,
		IsGroup:     isGroup,
		DisplayName: displayName,
	}

	// ON CONFLICT DO NOTHING: if a concurrent caller inserted the same SID
	// between our read and this write, we do NOT overwrite their allocation.
	res := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "sid"}}, DoNothing: true}).
		Create(row)
	if res.Error != nil {
		return nil, res.Error
	}

	// Re-read so that on a conflict (RowsAffected == 0) we return the winner's
	// durable mapping, not the rejected candidate.
	return s.GetSIDMapping(ctx, sid)
}

// DeleteSIDMapping removes a foreign-SID mapping. Intended for administrative
// cleanup only — normal resolution never deletes mappings (never-remap).
// Returns models.ErrSIDMappingNotFound when no mapping exists.
func (s *GORMStore) DeleteSIDMapping(ctx context.Context, sid string) error {
	return deleteByField[models.SIDMapping](s.db, ctx, "sid", sid, models.ErrSIDMappingNotFound)
}

// Compile-time assertion that GORMStore satisfies SIDMappingStore.
var _ SIDMappingStore = (*GORMStore)(nil)
