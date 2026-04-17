package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/marmos91/dittofs/internal/pathutil"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// ValidateBlockStoreConfig validates a block store config at configuration
// time and ensures any backing directory exists, mirroring the
// "instantiate before persisting" pattern used by metadata stores so
// handlers can reject bad config up-front instead of failing at attach.
//
// fs stores layer per-share subdirectories on top of the configured base
// path at share-attach time, so only the base path is materialised here.
// Remote stores are validated structurally only — reachability is left to
// the runtime health probe.
func ValidateBlockStoreConfig(kind models.BlockStoreKind, storeType string, cfg interface {
	GetConfig() (map[string]any, error)
}) error {
	config, err := cfg.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get config: %w", err)
	}

	switch kind {
	case models.BlockStoreKindLocal:
		switch storeType {
		case "memory":
			return nil
		case "fs":
			basePath, _ := config["path"].(string)
			if basePath == "" {
				return errors.New("fs local block store requires path in config")
			}
			expanded, err := pathutil.ExpandPath(basePath)
			if err != nil {
				return fmt.Errorf("failed to expand path %q: %w", basePath, err)
			}
			// Reject relative paths so MkdirAll cannot resolve against the
			// server's CWD, which would silently create directories in
			// unexpected locations.
			if !filepath.IsAbs(expanded) {
				return fmt.Errorf("fs local block store path must be absolute, got %q", basePath)
			}
			if err := os.MkdirAll(expanded, 0755); err != nil {
				return fmt.Errorf("failed to create block store directory %q: %w", expanded, err)
			}
			return nil
		default:
			return fmt.Errorf("unsupported local block store type: %s", storeType)
		}

	case models.BlockStoreKindRemote:
		switch storeType {
		case "memory":
			return nil
		case "s3":
			if bucket, _ := config["bucket"].(string); bucket == "" {
				return errors.New("s3 remote block store requires bucket in config")
			}
			return nil
		default:
			return fmt.Errorf("unsupported remote block store type: %s", storeType)
		}

	default:
		return fmt.Errorf("unsupported block store kind: %s", kind)
	}
}
