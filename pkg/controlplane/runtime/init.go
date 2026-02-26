package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/cache/wal"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/store/postgres"
	"github.com/marmos91/dittofs/pkg/payload"
	"github.com/marmos91/dittofs/pkg/payload/offloader"
	blockstore "github.com/marmos91/dittofs/pkg/payload/store"
	blockfs "github.com/marmos91/dittofs/pkg/payload/store/fs"
	blockmemory "github.com/marmos91/dittofs/pkg/payload/store/memory"
	blocks3 "github.com/marmos91/dittofs/pkg/payload/store/s3"
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
				return nil, fmt.Errorf("badger metadata store requires path as string")
			}
		}
		return badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)

	case "postgres":
		pgCfg := &postgres.PostgresMetadataStoreConfig{}

		if host, ok := config["host"].(string); ok {
			pgCfg.Host = host
		} else {
			return nil, fmt.Errorf("postgres metadata store requires host")
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
			return nil, fmt.Errorf("postgres metadata store requires database")
		}
		if user, ok := config["user"].(string); ok {
			pgCfg.User = user
		} else {
			return nil, fmt.Errorf("postgres metadata store requires user")
		}
		if password, ok := config["password"].(string); ok {
			pgCfg.Password = password
		} else {
			return nil, fmt.Errorf("postgres metadata store requires password")
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

// EnsurePayloadService lazily creates the cache and PayloadService.
func (rt *Runtime) EnsurePayloadService(ctx context.Context) error {
	rt.mu.Lock()
	if rt.payloadService != nil {
		rt.mu.Unlock()
		return nil
	}
	cacheConfig := rt.cacheConfig
	rt.mu.Unlock()

	if cacheConfig == nil {
		return fmt.Errorf("cache configuration not set - call SetCacheConfig first")
	}

	payloadStores, err := rt.store.ListPayloadStores(ctx)
	if err != nil {
		return fmt.Errorf("failed to list payload stores: %w", err)
	}

	if len(payloadStores) == 0 {
		return fmt.Errorf("no payload stores configured - add a payload store first")
	}

	cacheFile := filepath.Join(cacheConfig.Path, "cache.dat")
	if err := os.MkdirAll(cacheConfig.Path, 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	persister, err := wal.NewMmapPersister(cacheFile)
	if err != nil {
		return fmt.Errorf("failed to create WAL persister: %w", err)
	}

	cacheInstance, err := cache.NewWithWal(cacheConfig.Size, persister)
	if err != nil {
		return fmt.Errorf("failed to create cache: %w", err)
	}

	logger.Info("Cache initialized", "path", cacheFile, "max_size", cacheConfig.Size)

	payloadStoreCfg := payloadStores[0]
	blockStore, err := CreateBlockStoreFromConfig(ctx, payloadStoreCfg.Type, payloadStoreCfg)
	if err != nil {
		return fmt.Errorf("failed to create block store: %w", err)
	}

	logger.Info("Loaded payload store", "name", payloadStoreCfg.Name, "type", payloadStoreCfg.Type)

	objectStore := memory.NewMemoryMetadataStoreWithDefaults()
	offloaderCfg := offloader.Config{
		ParallelUploads:   16,
		ParallelDownloads: 4,
		PrefetchBlocks:    4,
	}
	offloaderInstance := offloader.New(cacheInstance, blockStore, objectStore, offloaderCfg)

	payloadSvc, err := payload.New(cacheInstance, offloaderInstance)
	if err != nil {
		return fmt.Errorf("failed to create payload service: %w", err)
	}

	offloaderInstance.Start(ctx)

	rt.mu.Lock()
	rt.payloadService = payloadSvc
	rt.cacheInstance = cacheInstance
	rt.mu.Unlock()

	logger.Info("PayloadService initialized",
		"payload_store", payloadStoreCfg.Name,
		"parallel_uploads", offloaderCfg.ParallelUploads,
		"parallel_downloads", offloaderCfg.ParallelDownloads)

	return nil
}

// CreateBlockStoreFromConfig creates a block store from type and dynamic config.
func CreateBlockStoreFromConfig(ctx context.Context, storeType string, cfg interface {
	GetConfig() (map[string]any, error)
}) (blockstore.BlockStore, error) {
	config, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	switch storeType {
	case "memory":
		return blockmemory.New(), nil

	case "filesystem":
		path, ok := config["path"].(string)
		if !ok || path == "" {
			return nil, fmt.Errorf("filesystem payload store requires path")
		}
		return blockfs.New(blockfs.Config{BasePath: path})

	case "s3":
		bucket, ok := config["bucket"].(string)
		if !ok || bucket == "" {
			return nil, fmt.Errorf("s3 payload store requires bucket")
		}

		region := "us-east-1"
		if r, ok := config["region"].(string); ok && r != "" {
			region = r
		}

		var s3Opts []func(*awsconfig.LoadOptions) error
		s3Opts = append(s3Opts, awsconfig.WithRegion(region))

		endpoint, _ := config["endpoint"].(string)
		accessKey, _ := config["access_key_id"].(string)
		secretKey, _ := config["secret_access_key"].(string)
		if accessKey != "" && secretKey != "" {
			s3Opts = append(s3Opts, awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
			))
		}

		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, s3Opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config: %w", err)
		}

		var s3Client *s3.Client
		if endpoint != "" {
			s3Client = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
				o.BaseEndpoint = aws.String(endpoint)
				o.UsePathStyle = true // Required for localstack/MinIO
			})
		} else {
			s3Client = s3.NewFromConfig(awsCfg)
		}

		return blocks3.New(s3Client, blocks3.Config{
			Bucket: bucket,
			Region: region,
		}), nil

	default:
		return nil, fmt.Errorf("unsupported payload store type: %s", storeType)
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
