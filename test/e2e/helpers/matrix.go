//go:build e2e

package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/stretchr/testify/require"
)

// MatrixStoreSetup holds the names of all stores and shares created for a
// store matrix test. Cleanup is registered automatically via t.Cleanup.
type MatrixStoreSetup struct {
	MetaStoreName  string
	BlockStoreName string
	ShareName      string
}

// MatrixSetupConfig describes a store combination for setup.
type MatrixSetupConfig struct {
	MetadataType string // "memory", "badger", "postgres"
	BlockType    string // "memory", "s3"
}

// s3HelperCreds returns the credentials an S3-emulator helper (Localstack or
// MinIO) accepts, defaulting to Localstack's "test"/"test" when unset.
func s3HelperCreds(h *framework.LocalstackHelper) (accessKey, secretKey string) {
	if h.AccessKey != "" && h.SecretKey != "" {
		return h.AccessKey, h.SecretKey
	}
	return "test", "test"
}

// SetupS3CompatibleShare provisions a badger-metadata + s3-block-store share
// whose block store points at the given S3-compatible emulator (Localstack or
// MinIO). It is the e2e fixture for the S3-compatible backend presets
// documented in docs/CONFIGURATION.md: a custom endpoint that auto-enables
// path-style addressing in the s3 store factory. All resources are registered
// for cleanup via t.Cleanup. Returns the share name.
func SetupS3CompatibleShare(
	t *testing.T,
	runner *CLIRunner,
	shareName string,
	s3Helper *framework.LocalstackHelper,
) string {
	t.Helper()

	metaName := UniqueTestName("meta")
	blockName := UniqueTestName("block")

	_, err := runner.CreateMetadataStore(metaName, "badger",
		WithMetaDBPath(filepath.Join(t.TempDir(), "badger")))
	require.NoError(t, err, "Should create badger metadata store")
	t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaName) })

	bucketName := strings.ReplaceAll(
		fmt.Sprintf("dittofs-s3c-%s", UniqueTestName("bkt")), "_", "-")
	require.NoError(t, s3Helper.CreateBucket(context.Background(), bucketName),
		"Should create S3 bucket on emulator")
	t.Cleanup(func() { s3Helper.CleanupBucket(context.Background(), bucketName) })

	accessKey, secretKey := s3HelperCreds(s3Helper)
	_, err = runner.CreateBlockStore(blockName, "s3",
		WithBlockS3Config(bucketName, "us-east-1", s3Helper.Endpoint, accessKey, secretKey),
		WithBlockAllowPrivateEndpoint())
	require.NoError(t, err, "Should create s3 block store")
	t.Cleanup(func() { _ = runner.DeleteBlockStore(blockName) })

	_, err = runner.CreateShare(shareName, metaName, blockName)
	require.NoError(t, err, "Should create share")
	t.Cleanup(func() { _ = runner.DeleteShare(shareName) })

	return shareName
}

// SetupStoreMatrix creates the metadata and block stores for a store matrix
// test, then creates a share referencing them.
// All resources are registered for cleanup via t.Cleanup.
//
// This extracts the common setup pattern shared between TestStoreMatrixOperations
// and TestStoreMatrixV4.
func SetupStoreMatrix(
	t *testing.T,
	runner *CLIRunner,
	shareName string,
	sc MatrixSetupConfig,
	pgHelper *framework.PostgresHelper,
	lsHelper *framework.LocalstackHelper,
) *MatrixStoreSetup {
	t.Helper()

	setup := &MatrixStoreSetup{
		MetaStoreName:  UniqueTestName("meta"),
		BlockStoreName: UniqueTestName("block"),
		ShareName:      shareName,
	}

	// Create metadata store
	var metaOpts []MetadataStoreOption
	switch sc.MetadataType {
	case "badger":
		badgerPath := filepath.Join(t.TempDir(), "badger")
		metaOpts = append(metaOpts, WithMetaDBPath(badgerPath))
	case "postgres":
		require.NotNil(t, pgHelper, "PostgreSQL helper not available")
		pgConfig := pgHelper.GetConfig()
		configJSON, err := json.Marshal(map[string]interface{}{
			"host":     pgConfig.Host,
			"port":     pgConfig.Port,
			"database": pgConfig.Database,
			"user":     pgConfig.User,
			"password": pgConfig.Password,
		})
		require.NoError(t, err, "Failed to marshal postgres config")
		metaOpts = append(metaOpts, WithMetaRawConfig(string(configJSON)))
	}

	_, err := runner.CreateMetadataStore(setup.MetaStoreName, sc.MetadataType, metaOpts...)
	require.NoError(t, err, "Should create metadata store (%s)", sc.MetadataType)
	t.Cleanup(func() { _ = runner.DeleteMetadataStore(setup.MetaStoreName) })

	// Create the block store
	var blockOpts []BlockStoreOption
	if sc.BlockType == "s3" {
		require.NotNil(t, lsHelper, "Localstack helper not available")
		bucketName := strings.ReplaceAll(
			fmt.Sprintf("dittofs-mtx-%s", UniqueTestName("bkt")), "_", "-")
		err := lsHelper.CreateBucket(context.Background(), bucketName)
		require.NoError(t, err, "Should create S3 bucket")
		t.Cleanup(func() { lsHelper.CleanupBucket(context.Background(), bucketName) })

		accessKey, secretKey := s3HelperCreds(lsHelper)
		blockOpts = append(blockOpts, WithBlockS3Config(
			bucketName, "us-east-1", lsHelper.Endpoint, accessKey, secretKey),
			WithBlockAllowPrivateEndpoint())
	}

	_, err = runner.CreateBlockStore(setup.BlockStoreName, sc.BlockType, blockOpts...)
	require.NoError(t, err, "Should create block store (%s)", sc.BlockType)
	t.Cleanup(func() { _ = runner.DeleteBlockStore(setup.BlockStoreName) })

	// Create the share
	_, err = runner.CreateShare(shareName, setup.MetaStoreName, setup.BlockStoreName)
	require.NoError(t, err, "Should create share")
	t.Cleanup(func() { _ = runner.DeleteShare(shareName) })

	return setup
}
