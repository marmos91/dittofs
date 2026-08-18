package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/marmos91/dittofs/internal/pathutil"
	"github.com/marmos91/dittofs/pkg/block/encryption"
	"github.com/marmos91/dittofs/pkg/block/encryption/keyprovider"
	"github.com/marmos91/dittofs/pkg/block/engine"
	s3store "github.com/marmos91/dittofs/pkg/block/remote/s3"
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
			rawPath, exists := config["path"]
			if !exists {
				return errors.New("fs local block store requires path in config")
			}
			basePath, ok := rawPath.(string)
			if !ok {
				return errors.New("fs local block store path must be a string")
			}
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
			bucket, ok := config["bucket"].(string)
			if !ok || bucket == "" {
				return errors.New("s3 remote block store requires bucket in config")
			}
			accessKeyID, ok := config["access_key_id"].(string)
			if !ok || accessKeyID == "" {
				return errors.New("s3 remote block store requires access_key_id in config")
			}
			secretAccessKey, ok := config["secret_access_key"].(string)
			if !ok || secretAccessKey == "" {
				return errors.New("s3 remote block store requires secret_access_key in config")
			}
			// SSRF guard: reject endpoints pointing at cloud metadata,
			// loopback, link-local, or private/internal hosts before the
			// create-time HealthCheck can dial them. allow_private_endpoint
			// opts in to private object stores (MinIO/Localstack) but still
			// blocks the metadata endpoint.
			endpoint, _ := config["endpoint"].(string)
			allowPrivate, _ := config["allow_private_endpoint"].(bool)
			if err := s3store.ValidateEndpoint(endpoint, allowPrivate); err != nil {
				return err
			}
			if err := validateCompressionSubconfig(config); err != nil {
				return err
			}
			if err := validateParallelUploads(config); err != nil {
				return err
			}
			if err := validateEncryptionSubconfig(config); err != nil {
				return err
			}
			return nil
		default:
			return fmt.Errorf("unsupported remote block store type: %s", storeType)
		}

	default:
		return fmt.Errorf("unsupported block store kind: %s", kind)
	}
}

// validateCompressionSubconfig accepts the parsed `compression` value
// from a BlockStoreConfig and verifies its shape. An absent key is
// allowed (compression is opt-in). When present, the value MUST be a
// JSON object; an `algo` key, if set, MUST be either "zstd" or "lz4".
func validateCompressionSubconfig(config map[string]any) error {
	raw, ok := config["compression"]
	if !ok {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("compression: expected object, got %T", raw)
	}
	algoVal, present := obj["algo"]
	if !present {
		return nil // defaults to zstd
	}
	algoStr, ok := algoVal.(string)
	if !ok {
		return fmt.Errorf("compression.algo: expected string, got %T", algoVal)
	}
	switch algoStr {
	case "zstd", "lz4":
		return nil
	default:
		return fmt.Errorf("compression.algo: unsupported value %q (want zstd or lz4)", algoStr)
	}
}

// validateParallelUploads checks the optional per-remote parallel_uploads
// override. 0 / absent means "use the server default" (CPU-deduced); a
// positive value pins the per-remote upload concurrency. JSON numbers decode
// as float64.
func validateParallelUploads(config map[string]any) error {
	raw, ok := config["parallel_uploads"]
	if !ok {
		return nil
	}
	n, ok := raw.(float64)
	if !ok {
		return fmt.Errorf("parallel_uploads: expected number, got %T", raw)
	}
	if n != float64(int(n)) {
		return fmt.Errorf("parallel_uploads: expected integer, got %v", n)
	}
	if n < 0 || n > engine.MaxParallelUploads {
		return fmt.Errorf("parallel_uploads: must be between 0 and %d (got %d)", engine.MaxParallelUploads, int(n))
	}
	return nil
}

// validateEncryptionSubconfig accepts the parsed `encryption` value from a
// BlockStoreConfig and verifies its shape. An absent key is allowed
// (encryption is opt-in). When present, the value is run through the same
// encryption.ParsePolicy the attach path uses — so the accepted object shape
// and AEAD names never drift from it — and the key config is then checked for
// the fields its provider kind requires. Reachability (opening the key file,
// dialling the KMIP server) is left to attach time.
func validateEncryptionSubconfig(config map[string]any) error {
	raw, ok := config["encryption"]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("encryption: %w", err)
	}
	policy, err := encryption.ParsePolicy(encoded)
	if err != nil {
		return err
	}
	switch policy.Key.Kind {
	case keyprovider.KindLocal:
		if policy.Key.File == "" {
			return errors.New("encryption.key.file is required for the local key provider")
		}
	case keyprovider.KindKMIP:
		if policy.Key.Endpoint == "" {
			return errors.New("encryption.key.endpoint is required for the kmip key provider")
		}
		if policy.Key.KeyUID == "" {
			return errors.New("encryption.key.key_uid is required for the kmip key provider")
		}
		if policy.Key.ClientCert == "" || policy.Key.ClientKey == "" {
			return errors.New("encryption.key.client_cert and encryption.key.client_key are required for the kmip key provider")
		}
	case "":
		return errors.New("encryption.key.kind is required (want local or kmip)")
	default:
		return fmt.Errorf("encryption.key.kind: unsupported value %q (want local or kmip)", policy.Key.Kind)
	}
	return nil
}
