package runtime

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// InitializeFromStore creates and initializes a runtime from the database.
// It loads metadata stores and shares from the persistent store and
// creates live instances of each.
//
// Returns an initialized runtime ready for use by adapters.
func InitializeFromStore(ctx context.Context, s store.Store) (*Runtime, error) {
	rt := New(s)

	// Load and create metadata stores
	if err := loadMetadataStores(ctx, rt, s); err != nil {
		return nil, fmt.Errorf("failed to load metadata stores: %w", err)
	}

	// Load and add shares
	if err := loadShares(ctx, rt, s); err != nil {
		return nil, fmt.Errorf("failed to load shares: %w", err)
	}

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
		metaStore, err := createMetadataStore(ctx, storeCfg.Type, storeCfg)
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

// createMetadataStore creates a metadata store instance based on type.
func createMetadataStore(ctx context.Context, storeType string, cfg interface {
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
		dbPath, _ := config["db_path"].(string)
		if dbPath == "" {
			return nil, fmt.Errorf("badger metadata store requires db_path configuration")
		}
		return badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)

	default:
		return nil, fmt.Errorf("unsupported metadata store type: %s", storeType)
	}
}

// loadShares loads share configurations from the database and adds them to the runtime.
func loadShares(ctx context.Context, rt *Runtime, s store.Store) error {
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

		shareConfig := &ShareConfig{
			Name:              share.Name,
			MetadataStore:     metaStoreCfg.Name,
			ReadOnly:          share.ReadOnly,
			DefaultPermission: share.DefaultPermission,
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
