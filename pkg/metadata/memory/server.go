package memory

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Configuration & Health
// ============================================================================

// SetServerConfig sets the server-wide configuration.
//
// This stores global server settings that apply across all shares and operations.
// Configuration changes are applied atomically - concurrent operations see either
// the old or new configuration, never a partial update.
//
// Thread Safety:
// The configuration update is protected by the store's mutex, ensuring that
// no operation observes a partially-updated configuration.
//
// Use Cases:
//   - Initial server setup
//   - Runtime configuration updates
//   - Administrative configuration changes
//
// Note: This does NOT validate configuration consistency. The caller should
// ensure the provided configuration is valid before calling this method.
func (s *MemoryMetadataStore) SetServerConfig(ctx context.Context, config metadata.ServerConfig) error {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Store the new configuration
	s.serverConfig = config

	return nil
}

// GetServerConfig returns the current server configuration.
//
// This retrieves the global server settings for use by protocol handlers,
// management tools, and monitoring systems.
//
// Thread Safety:
// The configuration is read under the store's mutex, ensuring a consistent
// snapshot of the configuration at a point in time.
//
// Use Cases:
//   - Protocol handlers checking server settings
//   - Administrative tools displaying configuration
//   - Health checks verifying configuration
//
// Note: The returned ServerConfig is a copy of the internal configuration.
// Modifying the returned struct does not affect the server's configuration.
// Use SetServerConfig to update configuration.
func (s *MemoryMetadataStore) GetServerConfig(ctx context.Context) (metadata.ServerConfig, error) {
	// Check context before acquiring lock
	if err := ctx.Err(); err != nil {
		return metadata.ServerConfig{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Return a copy of the configuration
	return s.serverConfig, nil
}
