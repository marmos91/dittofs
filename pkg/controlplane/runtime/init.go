package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/pathutil"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
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
}) (metadata.Store, error) {
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
		// Optional per-store Badger cache overrides (#1245 Bug D). The docs
		// define 0 = unset (fall through to global config then RAM-relative
		// auto-sizing) and positive = explicit size in MiB. A negative value is
		// a config error — reject it here rather than letting it silently fall
		// through to auto-sizing as if it were unset.
		blockCacheMB := configInt64(config, "block_cache_mb")
		if blockCacheMB < 0 {
			return nil, fmt.Errorf("badger metadata store block_cache_mb must be >= 0 (0 = auto), got %d", blockCacheMB)
		}
		indexCacheMB := configInt64(config, "index_cache_mb")
		if indexCacheMB < 0 {
			return nil, fmt.Errorf("badger metadata store index_cache_mb must be >= 0 (0 = auto), got %d", indexCacheMB)
		}
		// Relaxed durability defers namespace-op fsyncs for higher single-thread
		// throughput; data-paired writes stay synchronous (#1573 Wall 1).
		// Shipped default: enabled. Operators restore strict per-txn fsync with
		// relaxed_durability: false.
		relaxedDurability := configBoolDefault(config, "relaxed_durability", true)
		return badger.NewBadgerMetadataStoreWithDefaultsAndCaches(ctx, dbPath, blockCacheMB, indexCacheMB, relaxedDurability)

	case "postgres":
		pgCfg := &postgres.PostgresMetadataStoreConfig{}
		// See badger branch: relaxed by default, opt out with relaxed_durability: false.
		pgCfg.RelaxedDurability = configBoolDefault(config, "relaxed_durability", true)

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
			MaxReadSize:           1024 * 1024,
			PreferredReadSize:     64 * 1024,
			MaxWriteSize:          1024 * 1024,
			PreferredWriteSize:    64 * 1024,
			MaxFileSize:           1024 * 1024 * 1024 * 100, // 100 GB
			MaxFilenameLen:        255,
			MaxPathLen:            4096,
			MaxHardLinkCount:      32767,
			SupportsHardLinks:     true,
			SupportsSymlinks:      true,
			CaseSensitive:         true,
			CasePreserving:        true,
			SupportsACLs:          false,
			SupportsExtendedAttrs: true, // EAs persist in the inodes.eas JSONB column (migration 000028)
			// File timestamps are stored as BIGINT unix nanoseconds (lossless),
			// so nanosecond resolution is accurate for the postgres backend.
			TimestampResolution: time.Nanosecond,
		}

		return postgres.NewPostgresMetadataStore(ctx, pgCfg, capabilities)

	case "sqlite":
		dbPath, ok := config["path"].(string)
		if !ok || dbPath == "" {
			dbPath, ok = config["db_path"].(string) // accept legacy key
			if !ok || dbPath == "" {
				return nil, errors.New("sqlite metadata store requires path as string")
			}
		}
		dbPath, err = pathutil.ExpandPath(dbPath)
		if err != nil {
			return nil, fmt.Errorf("failed to expand path %q: %w", dbPath, err)
		}

		sqliteCfg := &sqlite.SQLiteMetadataStoreConfig{
			Path:        dbPath,
			AutoMigrate: true,
		}

		capabilities := metadata.FilesystemCapabilities{
			MaxReadSize:           1024 * 1024,
			PreferredReadSize:     64 * 1024,
			MaxWriteSize:          1024 * 1024,
			PreferredWriteSize:    64 * 1024,
			MaxFileSize:           1024 * 1024 * 1024 * 100, // 100 GB
			MaxFilenameLen:        255,
			MaxPathLen:            4096,
			MaxHardLinkCount:      32767,
			SupportsHardLinks:     true,
			SupportsSymlinks:      true,
			CaseSensitive:         true,
			CasePreserving:        true,
			SupportsACLs:          false,
			SupportsExtendedAttrs: true, // EAs persist in the inodes.eas column.
			// File timestamps are stored as INTEGER unix nanoseconds (lossless).
			TimestampResolution: time.Nanosecond,
		}

		return sqlite.NewSQLiteMetadataStore(ctx, sqliteCfg, capabilities)

	default:
		return nil, fmt.Errorf("unsupported metadata store type: %s", storeType)
	}
}

// configInt64 extracts an integer value from a type-specific config map under
// key, returning 0 when the key is absent or not a number. Config maps are
// typically produced by json.Unmarshal (numbers decode to float64), but a
// directly-set map may carry int/int64, so all are accepted.
func configInt64(config map[string]any, key string) int64 {
	switch v := config[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// configBoolDefault extracts a bool value from a type-specific config map under
// key, returning def when the key is absent. Used for opt-out flags whose
// shipped default is not the zero value (e.g. relaxed_durability defaults to
// true — #1573 Wall 1).
func configBoolDefault(config map[string]any, key string, def bool) bool {
	if v, ok := config[key].(bool); ok {
		return v
	}
	return def
}

// LoadSharesFromStore loads shares from the database into the runtime.
func LoadSharesFromStore(ctx context.Context, rt *Runtime, s store.Store) error {
	shares, err := s.ListShares(ctx)
	if err != nil {
		return fmt.Errorf("failed to list shares: %w", err)
	}

	for _, share := range shares {
		shareConfig, err := buildShareConfig(ctx, s, share)
		if err != nil {
			return err
		}
		if shareConfig == nil {
			// Share references an unknown metadata store; already logged.
			continue
		}

		if err := rt.AddShare(ctx, shareConfig); err != nil {
			// Legacy-layout detection is a hard boot stop, not a
			// per-share warn-and-skip. Surface it so
			// cmd/dfs/commands/start.go can exit 78 with the operator
			// directive. Every other AddShare failure stays a
			// warn-and-skip (preserves existing behavior).
			if errors.Is(err, block.ErrLegacyLayoutDetected) {
				return fmt.Errorf("share %q: %w", share.Name, err)
			}
			logger.Warn("Failed to add share to runtime",
				"share", share.Name,
				"error", err)
			continue
		}

		logger.Info("Loaded share", "name", share.Name, "metadata_store", shareConfig.MetadataStore)
	}

	return nil
}

// buildShareConfig assembles the runtime ShareConfig for a persisted share row,
// resolving its metadata store, NFS export options, and netgroup. It returns
// (nil, nil) when the share references an unknown metadata store so the caller
// can skip it (already logged). Extracted so both startup load and live
// block-store rebind (#1532) build an identical config from the same row.
func buildShareConfig(ctx context.Context, s store.Store, share *models.Share) (*ShareConfig, error) {
	// Try by ID first, fall back to name lookup.
	metaStoreCfg, err := s.GetMetadataStoreByID(ctx, share.MetadataStoreID)
	if err != nil {
		metaStoreCfg, err = s.GetMetadataStore(ctx, share.MetadataStoreID)
		if err != nil {
			logger.Warn("Share references unknown metadata store",
				"share", share.Name,
				"metadata_store_id", share.MetadataStoreID)
			return nil, nil
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

	// Re-apply the persisted root owner so a restart does not reset the share
	// root to UID/GID 0: prepareShare defaults a nil RootAttr to the zero value
	// and the metadata stores force-sync existing root ownership to it (#1534).
	var rootAttr *metadata.FileAttr
	if share.OwnerUID != nil {
		uid := *share.OwnerUID
		gid := uid // default the group to the owner, never root
		if share.OwnerGID != nil {
			gid = *share.OwnerGID
		}
		rootAttr = &metadata.FileAttr{UID: uid, GID: gid}
	}

	return &ShareConfig{
		Name:                             share.Name,
		MetadataStore:                    metaStoreCfg.Name,
		RootAttr:                         rootAttr,
		ReadOnly:                         share.ReadOnly,
		Enabled:                          share.Enabled, // propagate DB Enabled flag so adapter gates read the correct runtime value.
		EncryptData:                      share.EncryptData,
		AclFlagInheritedCanonicalization: share.AclFlagInheritedCanonicalization,
		AccessBasedEnumeration:           share.AccessBasedEnumeration,
		ChangeNotifyDisabled:             share.ChangeNotifyDisabled,
		StreamsDisabled:                  share.StreamsDisabled,
		ContinuousAvailability:           share.ContinuousAvailability,
		AllowMFsymlink:                   share.AllowMFsymlink,
		TrashEnabled:                     share.TrashEnabled,
		TrashRetentionDays:               share.TrashRetentionDays,
		TrashRestrictToAdmin:             share.TrashRestrictToAdmin,
		TrashMaxBytes:                    share.TrashMaxBytes,
		TrashExcludePatterns:             share.GetTrashExcludePatterns(),
		DefaultPermission:                share.DefaultPermission,
		Squash:                           nfsOpts.GetSquashMode(),
		AnonymousUID:                     nfsOpts.GetAnonymousUID(),
		AnonymousGID:                     nfsOpts.GetAnonymousGID(),
		AllowAuthSys:                     nfsOpts.AllowAuthSys,
		AllowAuthSysSet:                  true,
		RequireKerberos:                  nfsOpts.RequireKerberos,
		MinKerberosLevel:                 nfsOpts.MinKerberosLevel,
		DisableReaddirplus:               nfsOpts.DisableReaddirplus,
		NetgroupName:                     netgroupName,
		BlockedOperations:                share.GetBlockedOps(),
		RetentionPolicy:                  share.GetRetentionPolicy(),
		RetentionTTL:                     share.GetRetentionTTL(),
		LocalStoreSize:                   share.LocalStoreSize,
		ReadBufferSize:                   share.ReadBufferSize,
		QuotaBytes:                       share.QuotaBytes,
		LocalBlockStoreID:                share.LocalBlockStoreID,
		RemoteBlockStoreID:               derefString(share.RemoteBlockStoreID),
	}, nil
}

// derefString safely dereferences a *string, returning "" if nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
