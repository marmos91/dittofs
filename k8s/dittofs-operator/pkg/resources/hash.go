// Package resources provides utilities for building Kubernetes resources.
package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// ConfigHashAnnotation is the annotation key for the configuration hash.
// When this value changes, Kubernetes triggers a rolling update of the StatefulSet.
const ConfigHashAnnotation = "dittofs.io/config-hash"

// ComputeConfigHash computes a deterministic SHA256 hash of configuration data.
// The hash includes:
// - ConfigMap content (the YAML configuration)
// - All referenced Secrets (JWT secret, admin password, database credentials)
// - CRD generation number (for extra safety)
//
// The secrets map keys should be sorted before calling this function, or this
// function will sort them internally to ensure deterministic output.
//
// Parameters:
//   - configData: The ConfigMap content (YAML string)
//   - secrets: Map of secret name to secret data bytes
//   - generation: The CRD metadata.generation value
//
// Returns:
//   - A hex-encoded SHA256 hash string
func ComputeConfigHash(configData string, secrets map[string][]byte, generation int64) string {
	h := sha256.New()

	// Hash ConfigMap content
	h.Write([]byte(configData))

	// Hash secrets in sorted order for determinism
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		h.Write([]byte(k))
		h.Write(secrets[k])
	}

	// Hash generation number
	h.Write([]byte(fmt.Sprintf("gen:%d", generation)))

	return hex.EncodeToString(h.Sum(nil))
}
