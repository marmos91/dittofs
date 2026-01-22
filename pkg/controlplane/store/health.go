package store

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ============================================
// HEALTH & LIFECYCLE
// ============================================

func (s *GORMStore) Healthcheck(ctx context.Context) error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying database: %w", err)
	}
	return sqlDB.PingContext(ctx)
}

func (s *GORMStore) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying database: %w", err)
	}
	return sqlDB.Close()
}

// Compile-time interface checks
var _ Store = (*GORMStore)(nil)
var _ models.UserStore = (*GORMStore)(nil)
var _ models.IdentityStore = (*GORMStore)(nil)
