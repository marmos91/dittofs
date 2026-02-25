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
	blockstore "github.com/marmos91/dittofs/pkg/payload/store"
	blockfs "github.com/marmos91/dittofs/pkg/payload/store/fs"
	blockmemory "github.com/marmos91/dittofs/pkg/payload/store/memory"
	blocks3 "github.com/marmos91/dittofs/pkg/payload/store/s3"
	"github.com/marmos91/dittofs/pkg/payload/transfer"
)

// InitializeFromStore creates and initializes a runtime from the database.
// It loads metadata stores from the persistent store and creates live instances.
//
// Note: Shares are NOT loaded here. Call LoadSharesFromStore separately after
// setting cache configuration with SetCacheConfig (required for PayloadService).
//
// Returns an initialized runtime ready for use by adapters.
func InitializeFromStore(ctx context.Context, s store.Store) (*Runtime, error) {
	rt := New(s)

	// Load and create metadata stores
	if err := loadMetadataStores(ctx, rt, s); err != nil {
		return nil, fmt.Errorf("failed to load metadata stores: %w", err)
	}

	// Shares are loaded separately via LoadSharesFromStore after cache config is set

	return rt, nil
}

// loadMetadataStores loads metadata store configurations from the database
// and creates live instances for each.
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

// CreateMetadataStoreFromConfig creates a metadata store instance based on type and config.
// This is exported so API handlers can create stores dynamically.
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
			// Backward compatibility: also accept "db_path"
			dbPath, ok = config["db_path"].(string)
			if !ok || dbPath == "" {
				return nil, fmt.Errorf("badger metadata store requires path as string")
			}
		}
		return badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)

	case "postgres":
		pgCfg := &postgres.PostgresMetadataStoreConfig{}

		// Required connection parameters
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
		// Accept both "database" and "dbname" for compatibility with different conventions:
		// - "database": used by some Go database drivers and our config format
		// - "dbname": used by PostgreSQL connection strings and libpq
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

		// Connection pool settings
		if maxConns, ok := config["max_conns"].(float64); ok {
			pgCfg.MaxConns = int32(maxConns)
		}
		if minConns, ok := config["min_conns"].(float64); ok {
			pgCfg.MinConns = int32(minConns)
		}

		// Enable auto-migrate by default for dynamic store creation
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

// EnsurePayloadService creates the cache and PayloadService if not already initialized.
// This is called lazily when the first share that needs content operations is created.
// The cache is mandatory - all content operations go through the WAL-backed cache for crash safety.
//
// Returns nil if PayloadService already exists or if there are no payload stores configured.
func (rt *Runtime) EnsurePayloadService(ctx context.Context) error {
	rt.mu.Lock()
	// Check if already initialized
	if rt.payloadService != nil {
		rt.mu.Unlock()
		return nil
	}
	cacheConfig := rt.cacheConfig
	rt.mu.Unlock()

	// Validate cache config is set
	if cacheConfig == nil {
		return fmt.Errorf("cache configuration not set - call SetCacheConfig first")
	}

	// Load payload stores from DB
	payloadStores, err := rt.store.ListPayloadStores(ctx)
	if err != nil {
		return fmt.Errorf("failed to list payload stores: %w", err)
	}

	if len(payloadStores) == 0 {
		return fmt.Errorf("no payload stores configured - add a payload store first")
	}

	// Create cache with WAL persistence
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

	// Create block store from first payload store (single global PayloadService design)
	// For now, we use the first payload store. Future: could support per-share payload stores.
	payloadStoreCfg := payloadStores[0]
	blockStore, err := CreateBlockStoreFromConfig(ctx, payloadStoreCfg.Type, payloadStoreCfg)
	if err != nil {
		return fmt.Errorf("failed to create block store: %w", err)
	}

	logger.Info("Loaded payload store", "name", payloadStoreCfg.Name, "type", payloadStoreCfg.Type)

	// Create object store for deduplication (uses memory store for tracking)
	objectStore := memory.NewMemoryMetadataStoreWithDefaults()

	// Create transfer manager
	transferCfg := transfer.Config{
		ParallelUploads:   16,
		ParallelDownloads: 4,
		PrefetchBlocks:    4,
	}
	transferMgr := transfer.New(cacheInstance, blockStore, objectStore, transferCfg)

	// Create PayloadService
	payloadSvc, err := payload.New(cacheInstance, transferMgr)
	if err != nil {
		return fmt.Errorf("failed to create payload service: %w", err)
	}

	// Start transfer manager background workers
	transferMgr.Start(ctx)

	// Set PayloadService on runtime (thread-safe)
	rt.mu.Lock()
	rt.payloadService = payloadSvc
	rt.cacheInstance = cacheInstance
	rt.mu.Unlock()

	logger.Info("PayloadService initialized",
		"payload_store", payloadStoreCfg.Name,
		"parallel_uploads", transferCfg.ParallelUploads,
		"parallel_downloads", transferCfg.ParallelDownloads)

	return nil
}

// CreateBlockStoreFromConfig creates a block store instance based on type and config.
// This is exported so API handlers can create stores dynamically.
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

		// Build S3 config
		var s3Opts []func(*awsconfig.LoadOptions) error
		s3Opts = append(s3Opts, awsconfig.WithRegion(region))

		// Handle custom endpoint (for localstack, MinIO, etc.)
		endpoint, _ := config["endpoint"].(string)

		// Handle credentials if provided
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

		// Create S3 client with custom endpoint if specified
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

// LoadSharesFromStore loads share configurations from the database and adds them to the runtime.
// This must be called AFTER SetCacheConfig to ensure PayloadService can be initialized.
func LoadSharesFromStore(ctx context.Context, rt *Runtime, s store.Store) error {
	shares, err := s.ListShares(ctx)
	if err != nil {
		return fmt.Errorf("failed to list shares: %w", err)
	}

	for _, share := range shares {
		// Get the metadata store - try by ID first, then by name
		metaStoreCfg, err := s.GetMetadataStoreByID(ctx, share.MetadataStoreID)
		if err != nil {
			// MetadataStoreID might be a name instead of UUID, try lookup by name
			metaStoreCfg, err = s.GetMetadataStore(ctx, share.MetadataStoreID)
			if err != nil {
				logger.Warn("Share references unknown metadata store",
					"share", share.Name,
					"metadata_store_id", share.MetadataStoreID)
				continue
			}
		}

		// Load NFS adapter config for protocol-specific options
		nfsOpts := models.DefaultNFSExportOptions()
		nfsCfg, err := s.GetShareAdapterConfig(ctx, share.ID, "nfs")
		if err == nil && nfsCfg != nil {
			_ = nfsCfg.ParseConfig(&nfsOpts)
		}

		// Resolve netgroup name from ID for runtime
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
