package shares

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cubbit/dittofs/internal/logger"
	"github.com/cubbit/dittofs/pkg/block/middleware"
	"github.com/cubbit/dittofs/pkg/block/middleware/compression"
	"github.com/cubbit/dittofs/pkg/block/middleware/encryption"
	"github.com/cubbit/dittofs/pkg/block/middleware/encryption/keyprovider"
	"github.com/cubbit/dittofs/pkg/block/remote"
	remotememory "github.com/cubbit/dittofs/pkg/block/remote/memory"
	remotes3 "github.com/cubbit/dittofs/pkg/block/remote/s3"
	"github.com/cubbit/dittofs/pkg/controlplane/models"
)

// BlockStoreConfigProvider resolves block store configurations from the control plane DB.
//
// A share's BlockStoreID normally holds the block store row's UUID, but the
// REST UpdateShare path historically persisted the raw *name* instead (#1312).
// Resolution therefore tries the UUID first and falls back to a name lookup so
// shares whose rows hold a name still load after a restart.
type BlockStoreConfigProvider interface {
	GetBlockStoreByID(ctx context.Context, id string) (*models.BlockStoreConfig, error)
	GetBlockStore(ctx context.Context, name string) (*models.BlockStoreConfig, error)
}

// resolveBlockStoreConfig resolves a block store reference that may be either a
// UUID (the canonical form) or a name (#1312 legacy rows). It tries the UUID
// lookup first; on not-found it falls back to a name lookup.
func resolveBlockStoreConfig(
	ctx context.Context,
	provider BlockStoreConfigProvider,
	ref string,
) (*models.BlockStoreConfig, error) {
	cfg, err := provider.GetBlockStoreByID(ctx, ref)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, models.ErrStoreNotFound) {
		return nil, err
	}
	// Fall back to name resolution for legacy rows that stored the name.
	byName, nameErr := provider.GetBlockStore(ctx, ref)
	if nameErr != nil {
		// A real operational error (DB/context) on the name path must not be
		// masked as "not found"; only collapse to the familiar ID-lookup error
		// when the name is genuinely absent too.
		if errors.Is(nameErr, models.ErrStoreNotFound) {
			return nil, err
		}
		return nil, nameErr
	}
	return byName, nil
}

// LocalStoreDefaults holds the server-level settings for per-share local state.
type LocalStoreDefaults struct {
	// JournalRoot is the directory holding every share's state directory. Each
	// share keeps its own subdirectory beneath it, so no two shares write into
	// the same directory.
	JournalRoot string
}

// sharedRemote holds a reference-counted remote store shared across shares.
type sharedRemote struct {
	store    remote.RemoteStore
	refCount int
}

// acquireRemoteStore returns a shared remote store, creating it if needed.
// Uses double-checked locking to avoid holding s.mu during potentially slow
// network/DB I/O (config resolution, S3 client initialization).
// Returns the store, its config ID, and any error.
func (s *Service) acquireRemoteStore(ctx context.Context, ref string, provider BlockStoreConfigProvider) (remote.RemoteStore, string, error) {
	// Fast path: when the share already persists the canonical UUID (the common
	// case) and the store is live, take it without a config-resolution DB read.
	// Legacy name refs (#1312) miss here — the map is keyed by UUID — and fall
	// through to full resolution below.
	s.mu.Lock()
	if sr, ok := s.remoteStores[ref]; ok {
		sr.refCount++
		s.mu.Unlock()
		return sr.store, ref, nil
	}
	s.mu.Unlock()

	// Resolve config (by UUID, or by name for #1312 legacy rows) so the
	// ref-count map is always keyed by the canonical store UUID. Two shares
	// referencing the same remote — one by UUID, one by legacy name — must
	// share the single ref-counted store.
	remoteCfg, err := resolveBlockStoreConfig(ctx, provider, ref)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve remote block store config %q: %w", ref, err)
	}
	configID := remoteCfg.ID

	s.mu.Lock()
	if sr, ok := s.remoteStores[configID]; ok {
		sr.refCount++
		s.mu.Unlock()
		return sr.store, configID, nil
	}
	s.mu.Unlock()

	newStore, err := CreateRemoteStoreFromConfig(ctx, remoteCfg.Type, remoteCfg)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create remote store: %w", err)
	}

	// The pipeline runs its stages in the order remoteStages returns them,
	// so that slice IS the seal order: compress, then encrypt.
	stages, err := remoteStages(ctx, remoteCfg)
	if err != nil {
		_ = newStore.Close()
		return nil, "", fmt.Errorf("failed to apply block store policy: %w", err)
	}
	if len(stages) > 0 {
		wrapped, err := middleware.New(newStore, stages...)
		if err != nil {
			// No pipeline exists to close the stages, so release them here:
			// the encryption stage holds the key provider.
			for _, stage := range stages {
				if c, ok := stage.(io.Closer); ok {
					_ = c.Close()
				}
			}
			_ = newStore.Close()
			return nil, "", fmt.Errorf("failed to build the block store pipeline: %w", err)
		}
		newStore = wrapped
	}

	// Double-check: another goroutine may have created the store concurrently.
	s.mu.Lock()
	if sr, ok := s.remoteStores[configID]; ok {
		sr.refCount++
		s.mu.Unlock()
		// We lost the race; discard our now-redundant store. newStore is the
		// fully-decorated (encryption/compression) stack, so a Close failure
		// here could leak a key provider — surface it rather than swallow it.
		if err := newStore.Close(); err != nil {
			logger.Warn("acquireRemoteStore: failed to close duplicate remote store",
				"config_id", configID, "error", err)
		}
		return sr.store, configID, nil
	}

	s.remoteStores[configID] = &sharedRemote{
		store:    newStore,
		refCount: 1,
	}
	s.mu.Unlock()

	logger.Info("Created shared remote store", "config_id", configID, "type", remoteCfg.Type)
	return newStore, configID, nil
}

// remoteStages builds the middleware stack for a remote config, in SEAL order.
//
// decision: this function's statement order is the only thing that enforces
// compress-before-encrypt. The stages accept each other in either arrangement,
// and a swap produces a working store that compresses ciphertext at a ratio of
// ~1.0 forever, silently — no error, no log line, just a ratio that never
// improves. It stays an ordering convention rather than a type because this is
// the sole construction site; give the order its own type the moment a second
// site builds a stack. TestRemoteStages_CompressionBeforeEncryption is the only
// test a reordering here fails: it calls this function and asserts the stage
// types in the order returned. TestPipeline_CompressBeforeEncrypt shows the two
// arrangements differ by sealed size, but it builds its own stages and never
// reaches this code, so it stays green through a swap here.
func remoteStages(ctx context.Context, cfg *models.BlockStoreConfig) ([]middleware.Transform, error) {
	parsed, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("parse block store config: %w", err)
	}

	var stages []middleware.Transform

	if raw, ok := parsed["compression"]; ok {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("marshal compression sub-config: %w", err)
		}
		policy, err := compression.ParsePolicy(encoded)
		if err != nil {
			return nil, err
		}
		stage, err := compression.NewTransform(policy)
		if err != nil {
			return nil, err
		}
		stages = append(stages, stage)
	}

	if raw, ok := parsed["encryption"]; ok {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("marshal encryption sub-config: %w", err)
		}
		policy, err := encryption.ParsePolicy(encoded)
		if err != nil {
			return nil, err
		}
		provider, err := keyprovider.NewProvider(ctx, policy.Key)
		if err != nil {
			return nil, fmt.Errorf("create key provider: %w", err)
		}
		stage, err := encryption.NewTransform(policy, provider)
		if err != nil {
			_ = provider.Close()
			return nil, err
		}
		stages = append(stages, stage)
	}

	return stages, nil
}

// releaseRemoteStore decrements the reference count and closes the remote store if no longer used.
// Close happens outside the lock to avoid blocking share operations during network I/O.
func (s *Service) releaseRemoteStore(configID string) {
	var storeToClose remote.RemoteStore

	s.mu.Lock()
	sr, ok := s.remoteStores[configID]
	if !ok {
		s.mu.Unlock()
		return
	}
	sr.refCount--
	if sr.refCount <= 0 {
		storeToClose = sr.store
		delete(s.remoteStores, configID)
	}
	s.mu.Unlock()

	if storeToClose != nil {
		_ = storeToClose.Close()
		logger.Info("Closed shared remote store", "config_id", configID)
	}
}

// RebindShareBlockStore moves a running share onto the remote store its
// changed binding names, so the change takes effect without a server restart.
// The new remote is acquired before the old one is released, so a failure
// leaves the share on its previous binding. Once CloseBlockStores has run, a
// rebind is refused with ErrShuttingDown: it is checked on entry and again at
// the swap, since the fence can fall while the new remote is being acquired.
func (s *Service) RebindShareBlockStore(ctx context.Context, name, blockStoreID string, provider BlockStoreConfigProvider) error {
	s.mu.RLock()
	_, exists := s.registry[name]
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return fmt.Errorf("cannot rebind share %q: %w", name, ErrShuttingDown)
	}
	if !exists {
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}

	_, configID, err := s.acquireRemoteStore(ctx, blockStoreID, provider)
	if err != nil {
		return fmt.Errorf("rebind share %q: %w", name, err)
	}

	s.mu.Lock()
	share, exists := s.registry[name]
	closed = s.closed
	var old string
	if exists && !closed {
		old = share.remoteConfigID
		share.remoteConfigID = configID
	}
	s.mu.Unlock()

	if closed {
		s.releaseRemoteStore(configID)
		return fmt.Errorf("cannot rebind share %q: %w", name, ErrShuttingDown)
	}
	if !exists {
		s.releaseRemoteStore(configID)
		return fmt.Errorf("%w: %q", ErrShareNotFound, name)
	}
	s.releaseRemoteStore(old)
	return nil
}

// durableOverrideSetter is implemented by block stores (local and remote) that
// expose a per-store durability override (block stores embed a
// block.DurabilityReporter type-default; this lets an operator flip it).
type durableOverrideSetter interface {
	SetDurable(bool)
}

// applyDurableOverride reads an optional "durable" bool from the per-store
// config and applies it to store when present, overriding the type-default
// durability (#1274). A non-bool "durable" value is warned and ignored so the
// type-default stands. label/shareName feed the diagnostic log line.
func applyDurableOverride(store any, config map[string]any, label, shareName string) {
	v, ok := config["durable"]
	if !ok {
		return
	}
	b, ok := v.(bool)
	if !ok {
		logger.Warn("block store config has durable but it is not a bool; ignoring",
			"store", label, "share", shareName, "value", v)
		return
	}
	setter, ok := store.(durableOverrideSetter)
	if !ok {
		// Every shipped store implements SetDurable; a store that does not is a
		// programmer error, surface it instead of silently dropping the override.
		logger.Warn("block store does not support a durable override; ignoring config[\"durable\"]",
			"store", label, "share", shareName)
		return
	}
	setter.SetDurable(b)
	logger.Info("block store durability overridden by config", "store", label, "share", shareName, "durable", b)
}

// CreateRemoteStoreFromConfig creates a remote store from type and dynamic config.
func CreateRemoteStoreFromConfig(ctx context.Context, storeType string, cfg interface {
	GetConfig() (map[string]any, error)
}) (remote.RemoteStore, error) {
	config, err := cfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	switch storeType {
	case "memory":
		store := remotememory.New()
		applyDurableOverride(store, config, "remote "+storeType, "")
		return store, nil

	case "filesystem":
		return nil, errors.New("remote store type 'filesystem' removed in v4.0 -- use 'memory' or 's3'")

	case "s3":
		bucket, ok := config["bucket"].(string)
		if !ok || bucket == "" {
			return nil, errors.New("s3 remote store requires bucket")
		}

		region := "us-east-1"
		if r, ok := config["region"].(string); ok && r != "" {
			region = r
		}

		endpoint, _ := config["endpoint"].(string)
		prefix, _ := config["prefix"].(string)
		accessKey, _ := config["access_key_id"].(string)
		secretKey, _ := config["secret_access_key"].(string)
		if accessKey == "" || secretKey == "" {
			return nil, errors.New("s3 remote store requires access_key_id and secret_access_key")
		}
		// When a custom endpoint is set (MinIO, Synology, etc.), default to
		// path-style addressing — virtual-hosted style rarely works on
		// non-AWS S3-compatible services. This matches v0.8.x behavior.
		// Only override when the key is absent; honor explicit false.
		forcePathStyle, hasPathStyle := config["force_path_style"].(bool)
		if endpoint != "" && !hasPathStyle {
			forcePathStyle = true
		}
		allowPrivate, _ := config["allow_private_endpoint"].(bool)

		store, err := remotes3.NewFromConfig(ctx, remotes3.Config{
			Bucket:         bucket,
			Region:         region,
			Endpoint:       endpoint,
			AccessKey:      accessKey,
			SecretKey:      secretKey,
			KeyPrefix:      prefix,
			ForcePathStyle: forcePathStyle,
			AllowPrivate:   allowPrivate,
		})
		if err != nil {
			return nil, err
		}
		applyDurableOverride(store, config, "remote "+storeType, "")
		return store, nil

	default:
		return nil, fmt.Errorf("unsupported remote store type: %s", storeType)
	}
}
