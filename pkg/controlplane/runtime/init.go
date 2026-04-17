package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/pathutil"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
)

// InitializeFromStore creates a runtime and loads metadata stores from the database.
func InitializeFromStore(ctx context.Context, s store.Store) (*Runtime, error) {
	rt := New(s)
	if err := loadMetadataStores(ctx, rt, s); err != nil {
		return nil, fmt.Errorf("failed to load metadata stores: %w", err)
	}
	return rt, nil
}

func loadMetadataStores(ctx context.Context, rt *Runtime, s store.Store) error {
	stores, err := s.ListMetadataStores(ctx)
	if err != nil {
		return fmt.Errorf("failed to list metadata stores: %w", err)
	}

	for _, storeCfg := range stores {
		metaStore, err := CreateMetadataStoreFromConfig(ctx, storeCfg.Type, storeCfg)
		if err != nil {
			return fmt.Errorf("failed to create metadata store %q: %w", storeCfg.Name, err)
		}

		if err := rt.RegisterMetadataStore(storeCfg.Name, metaStore); err != nil {
			return fmt.Errorf("failed to register metadata store %q: %w", storeCfg.Name, err)
		}

		logger.Info("Loaded metadata store", "name", storeCfg.Name, "type", storeCfg.Type)
	}

	return nil
}

// CreateMetadataStoreFromConfig creates a metadata store instance from type and config.
func CreateMetadataStoreFromConfig(ctx context.Context, storeType string, cfg interface {
	GetConfig() (map[string]any, error)
}) (metadata.MetadataStore, error) {
	config, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	switch storeType {
	case "memory":
		return memory.NewMemoryMetadataStoreWithDefaults(), nil

	case "badger":
		dbPath, ok := config["path"].(string)
		if !ok || dbPath == "" {
			dbPath, ok = config["db_path"].(string) // accept legacy key
			if !ok || dbPath == "" {
				return nil, errors.New("badger metadata store requires path as string")
			}
		}
		dbPath, err = pathutil.ExpandPath(dbPath)
		if err != nil {
			return nil, fmt.Errorf("failed to expand path %q: %w", dbPath, err)
		}
		return badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)

	case "postgres":
		pgCfg := &postgres.PostgresMetadataStoreConfig{}

		if host, ok := config["host"].(string); ok {
			pgCfg.Host = host
		} else {
			return nil, errors.New("postgres metadata store requires host")
		}
		if port, ok := config["port"].(float64); ok {
			pgCfg.Port = int(port)
		} else if portInt, ok := config["port"].(int); ok {
			pgCfg.Port = portInt
		} else {
			pgCfg.Port = 5432 // default
		}
		if database, ok := config["database"].(string); ok {
			pgCfg.Database = database
		} else if dbname, ok := config["dbname"].(string); ok {
			pgCfg.Database = dbname
		} else {
			return nil, errors.New("postgres metadata store requires database")
		}
		if user, ok := config["user"].(string); ok {
			pgCfg.User = user
		} else {
			return nil, errors.New("postgres metadata store requires user")
		}
		if password, ok := config["password"].(string); ok {
			pgCfg.Password = password
		} else {
			return nil, errors.New("postgres metadata store requires password")
		}
		if sslmode, ok := config["sslmode"].(string); ok {
			pgCfg.SSLMode = sslmode
		} else {
			pgCfg.SSLMode = "disable" // default for local dev
		}

		if maxConns, ok := config["max_conns"].(float64); ok {
			pgCfg.MaxConns = int32(maxConns)
		}
		if minConns, ok := config["min_conns"].(float64); ok {
			pgCfg.MinConns = int32(minConns)
		}

		pgCfg.AutoMigrate = true

		capabilities := metadata.FilesystemCapabilities{
			MaxReadSize:         1024 * 1024,
			PreferredReadSize:   64 * 1024,
			MaxWriteSize:        1024 * 1024,
			PreferredWriteSize:  64 * 1024,
			MaxFileSize:         1024 * 1024 * 1024 * 100, // 100 GB
			MaxFilenameLen:      255,
			MaxPathLen:          4096,
			MaxHardLinkCount:    32767,
			SupportsHardLinks:   true,
			SupportsSymlinks:    true,
			CaseSensitive:       true,
			CasePreserving:      true,
			SupportsACLs:        false,
			TimestampResolution: time.Nanosecond,
		}

		return postgres.NewPostgresMetadataStore(ctx, pgCfg, capabilities)

	default:
		return nil, fmt.Errorf("unsupported metadata store type: %s", storeType)
	}
}

// LoadSharesFromStore loads shares from the database into the runtime.
func LoadSharesFromStore(ctx context.Context, rt *Runtime, s store.Store) error {
	shares, err := s.ListShares(ctx)
	if err != nil {
		return fmt.Errorf("failed to list shares: %w", err)
	}

	for _, share := range shares {
		// Try by ID first, fall back to name lookup
		metaStoreCfg, err := s.GetMetadataStoreByID(ctx, share.MetadataStoreID)
		if err != nil {
			metaStoreCfg, err = s.GetMetadataStore(ctx, share.MetadataStoreID)
			if err != nil {
				logger.Warn("Share references unknown metadata store",
					"share", share.Name,
					"metadata_store_id", share.MetadataStoreID)
				continue
			}
		}

		nfsOpts := models.DefaultNFSExportOptions()
		nfsCfg, err := s.GetShareAdapterConfig(ctx, share.ID, "nfs")
		if err == nil && nfsCfg != nil {
			_ = nfsCfg.ParseConfig(&nfsOpts)
		}

		var netgroupName string
		if nfsOpts.NetgroupID != nil && *nfsOpts.NetgroupID != "" {
			if ns, ok := s.(store.NetgroupStore); ok {
				ng, ngErr := ns.GetNetgroupByID(ctx, *nfsOpts.NetgroupID)
				if ngErr == nil {
					netgroupName = ng.Name
				} else {
					logger.Warn("Share references unknown netgroup",
						"share", share.Name,
						"netgroup_id", *nfsOpts.NetgroupID)
				}
			}
		}

		shareConfig := &ShareConfig{
			Name:               share.Name,
			MetadataStore:      metaStoreCfg.Name,
			ReadOnly:           share.ReadOnly,
			Enabled:            share.Enabled, // Plan 05-09 D-02: propagate DB Enabled flag so adapter gates read the correct runtime value.
			EncryptData:        share.EncryptData,
			DefaultPermission:  share.DefaultPermission,
			Squash:             nfsOpts.GetSquashMode(),
			AnonymousUID:       nfsOpts.GetAnonymousUID(),
			AnonymousGID:       nfsOpts.GetAnonymousGID(),
			AllowAuthSys:       nfsOpts.AllowAuthSys,
			AllowAuthSysSet:    true,
			RequireKerberos:    nfsOpts.RequireKerberos,
			MinKerberosLevel:   nfsOpts.MinKerberosLevel,
			DisableReaddirplus: nfsOpts.DisableReaddirplus,
			NetgroupName:       netgroupName,
			BlockedOperations:  share.GetBlockedOps(),
			RetentionPolicy:    share.GetRetentionPolicy(),
			RetentionTTL:       share.GetRetentionTTL(),
			LocalStoreSize:     share.LocalStoreSize,
			ReadBufferSize:     share.ReadBufferSize,
			QuotaBytes:         share.QuotaBytes,
			LocalBlockStoreID:  share.LocalBlockStoreID,
			RemoteBlockStoreID: derefString(share.RemoteBlockStoreID),
		}

		if err := rt.AddShare(ctx, shareConfig); err != nil {
			logger.Warn("Failed to add share to runtime",
				"share", share.Name,
				"error", err)
			continue
		}

		logger.Info("Loaded share", "name", share.Name, "metadata_store", metaStoreCfg.Name)
	}

	return nil
}

// derefString safely dereferences a *string, returning "" if nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
