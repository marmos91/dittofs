//go:build e2e

package e2e

import (
	"fmt"
	"os"
)

// =============================================================================
// Store Matrix Configuration (6 combos: 3 metadata x 2 block)
// =============================================================================
// This file defines the shared store matrix used by NFSv3 and NFSv4 matrix
// tests. The matrix covers all valid combinations of metadata and block store
// backends.

// matrixStoreConfig defines a combination of metadata and block store types.
type matrixStoreConfig struct {
	metadataType string // "memory", "badger", "postgres"
	blockType    string // "memory", "s3"
}

// testName returns a short pair format name for use as t.Run subtest name.
// Example: "memory/memory", "badger/s3".
func (sc matrixStoreConfig) testName() string {
	return fmt.Sprintf("%s/%s", sc.metadataType, sc.blockType)
}

// needsPostgres returns true if this config requires a PostgreSQL container.
func (sc matrixStoreConfig) needsPostgres() bool {
	return sc.metadataType == "postgres"
}

// needsS3 returns true if this config requires a Localstack S3 container.
func (sc matrixStoreConfig) needsS3() bool {
	return sc.blockType == "s3"
}

// storeMatrix defines all 6 combinations: 3 metadata types x 2 block types.
var storeMatrix = []matrixStoreConfig{
	{"memory", "memory"},   // MTX-01: fully in-memory, fastest
	{"memory", "s3"},       // MTX-02: in-memory metadata + S3 blocks
	{"badger", "memory"},   // MTX-03: persistent metadata, in-memory blocks
	{"badger", "s3"},       // MTX-04: persistent metadata + S3 blocks
	{"postgres", "memory"}, // MTX-05: distributed metadata, in-memory blocks
	{"postgres", "s3"},     // MTX-06: full production stack
}

// shortMatrix defines representative combos for testing.Short() mode.
// Selected to cover the key dimensions with minimal test time.
var shortMatrix = []matrixStoreConfig{
	{"memory", "memory"}, // Fastest, no containers
	{"badger", "s3"},     // Persistent metadata + S3
	{"postgres", "s3"},   // Full production stack
}

// isContainerFreeOnly reports whether DITTOFS_E2E_LOCAL_ONLY is set, meaning
// only combos that need no object-store container should run.
func isContainerFreeOnly() bool {
	return os.Getenv("DITTOFS_E2E_LOCAL_ONLY") == "1"
}

// isS3Only reports whether DITTOFS_E2E_WITH_REMOTE is set, meaning only combos
// backed by an S3 block store should run.
func isS3Only() bool {
	return os.Getenv("DITTOFS_E2E_WITH_REMOTE") == "1"
}

// getStoreMatrix returns the appropriate matrix based on testing mode and
// environment flags. In short mode, returns shortMatrix. The env flags narrow
// the matrix to the combos that skip, or exclusively use, an S3 block store.
func getStoreMatrix(short bool) []matrixStoreConfig {
	matrix := storeMatrix
	if short {
		matrix = shortMatrix
	}

	if isContainerFreeOnly() {
		var filtered []matrixStoreConfig
		for _, sc := range matrix {
			if !sc.needsS3() {
				filtered = append(filtered, sc)
			}
		}
		return filtered
	}

	if isS3Only() {
		var filtered []matrixStoreConfig
		for _, sc := range matrix {
			if sc.needsS3() {
				filtered = append(filtered, sc)
			}
		}
		return filtered
	}

	return matrix
}
