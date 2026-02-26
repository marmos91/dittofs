package store

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

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

// Compile-time assertions that GORMStore satisfies all interfaces.
var (
	_ Store                    = (*GORMStore)(nil)
	_ UserStore                = (*GORMStore)(nil)
	_ GroupStore               = (*GORMStore)(nil)
	_ ShareStore               = (*GORMStore)(nil)
	_ PermissionStore          = (*GORMStore)(nil)
	_ MetadataStoreConfigStore = (*GORMStore)(nil)
	_ PayloadStoreConfigStore  = (*GORMStore)(nil)
	_ AdapterStore             = (*GORMStore)(nil)
	_ SettingsStore            = (*GORMStore)(nil)
	_ AdminStore               = (*GORMStore)(nil)
	_ HealthStore              = (*GORMStore)(nil)
	_ NetgroupStore            = (*GORMStore)(nil)
	_ IdentityMappingStore     = (*GORMStore)(nil)

	// Assertions for adapter-facing interfaces defined in models package.
	_ models.UserStore     = (*GORMStore)(nil)
	_ models.IdentityStore = (*GORMStore)(nil)
)
