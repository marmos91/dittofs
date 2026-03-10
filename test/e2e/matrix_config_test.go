//go:build e2e

package e2e

import (
	"fmt"
	"os"
)

// =============================================================================
// 3D Store Matrix Configuration (18 combos: 3 metadata x 2 local x 3 remote)
// =============================================================================
// This file defines the shared store matrix used by NFSv3 and NFSv4 matrix
// tests. The matrix covers all valid combinations of metadata, local block,
// and remote block store backends.

// matrixStoreConfig defines a combination of metadata, local block, and remote
// block store types for the 3D store matrix.
type matrixStoreConfig struct {
	metadataType string // "memory", "badger", "postgres"
	localType    string // "fs", "memory"
	remoteType   string // "none", "memory", "s3"
}

// testName returns a short triple format name for use as t.Run subtest name.
// Example: "memory/fs/none", "badger/memory/s3".
func (sc matrixStoreConfig) testName() string {
	return fmt.Sprintf("%s/%s/%s", sc.metadataType, sc.localType, sc.remoteType)
}

// needsPostgres returns true if this config requires a PostgreSQL container.
func (sc matrixStoreConfig) needsPostgres() bool {
	return sc.metadataType == "postgres"
}

// needsS3 returns true if this config requires a Localstack S3 container.
func (sc matrixStoreConfig) needsS3() bool {
	return sc.remoteType == "s3"
}

// hasRemote returns true if this config uses a remote block store.
func (sc matrixStoreConfig) hasRemote() bool {
	return sc.remoteType != "none"
}

// storeMatrix3D defines all 18 combinations of the 3D store matrix:
// 3 metadata types x 2 local types x 3 remote types.
var storeMatrix3D = []matrixStoreConfig{
	// memory metadata (6 combos)
	{"memory", "fs", "none"},       // MTX-01: local-only, fast
	{"memory", "fs", "memory"},     // MTX-02: with in-memory remote
	{"memory", "fs", "s3"},         // MTX-03: with S3 remote
	{"memory", "memory", "none"},   // MTX-04: fully in-memory
	{"memory", "memory", "memory"}, // MTX-05: all memory
	{"memory", "memory", "s3"},     // MTX-06: memory local + S3

	// badger metadata (6 combos)
	{"badger", "fs", "none"},       // MTX-07: persistent meta, local-only
	{"badger", "fs", "memory"},     // MTX-08: persistent meta + memory remote
	{"badger", "fs", "s3"},         // MTX-09: persistent meta + S3
	{"badger", "memory", "none"},   // MTX-10: persistent meta, memory local
	{"badger", "memory", "memory"}, // MTX-11: persistent meta + memory everywhere
	{"badger", "memory", "s3"},     // MTX-12: persistent meta + S3

	// postgres metadata (6 combos)
	{"postgres", "fs", "none"},       // MTX-13: distributed meta, local-only
	{"postgres", "fs", "memory"},     // MTX-14: distributed meta + memory remote
	{"postgres", "fs", "s3"},         // MTX-15: full production stack
	{"postgres", "memory", "none"},   // MTX-16: distributed meta, memory local
	{"postgres", "memory", "memory"}, // MTX-17: distributed meta + memory everywhere
	{"postgres", "memory", "s3"},     // MTX-18: distributed meta + S3
}

// shortMatrix3D defines 3-4 representative combos for testing.Short() mode.
// Selected to cover the key dimensions with minimal test time:
// - memory/fs/none: local-only, fastest possible
// - badger/fs/s3: persistent metadata + S3 remote (if available)
// - postgres/fs/s3: full production stack (if available)
// - memory/memory/memory: all in-memory (fast, no disk I/O)
var shortMatrix3D = []matrixStoreConfig{
	{"memory", "fs", "none"},       // Fast local-only
	{"memory", "memory", "memory"}, // All in-memory
	{"badger", "fs", "s3"},         // Persistent meta + S3
	{"postgres", "fs", "s3"},       // Full production stack
}

// isLocalOnly returns true if the DITTOFS_E2E_LOCAL_ONLY env var is set,
// indicating only combos with remoteType="none" should run.
func isLocalOnly() bool {
	return os.Getenv("DITTOFS_E2E_LOCAL_ONLY") == "1"
}

// getStoreMatrix returns the appropriate matrix based on testing mode and
// environment flags. In short mode, returns shortMatrix3D. If --local-only
// is set, filters to only remoteType="none" combos.
func getStoreMatrix(short bool) []matrixStoreConfig {
	matrix := storeMatrix3D
	if short {
		matrix = shortMatrix3D
	}

	if !isLocalOnly() {
		return matrix
	}

	var filtered []matrixStoreConfig
	for _, sc := range matrix {
		if sc.remoteType == "none" {
			filtered = append(filtered, sc)
		}
	}
	return filtered
}
