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
	MetaStoreName   string
	LocalStoreName  string
	RemoteStoreName string
	ShareName       string
}

// MatrixSetupConfig describes a 3D store combination for setup.
type MatrixSetupConfig struct {
	MetadataType string // "memory", "badger", "postgres"
	LocalType    string // "fs", "memory"
	RemoteType   string // "none", "memory", "s3"
}

// HasRemote returns true if this config uses a remote block store.
func (c MatrixSetupConfig) HasRemote() bool {
	return c.RemoteType != "none"
}

// s3HelperCreds returns the credentials an S3-emulator helper (Localstack or
// MinIO) accepts, defaulting to Localstack's "test"/"test" when unset.
func s3HelperCreds(h *framework.LocalstackHelper) (accessKey, secretKey string) {
	if h.AccessKey != "" && h.SecretKey != "" {
		return h.AccessKey, h.SecretKey
	}
	return "test", "test"
}

// SetupS3CompatibleShare provisions a badger-metadata + fs-local + s3-remote
// share whose remote points at the given S3-compatible emulator (Localstack or
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
	localName := UniqueTestName("local")
	remoteName := UniqueTestName("remote")

	_, err := runner.CreateMetadataStore(metaName, "badger",
		WithMetaDBPath(filepath.Join(t.TempDir(), "badger")))
	require.NoError(t, err, "Should create badger metadata store")
	t.Cleanup(func() { _ = runner.DeleteMetadataStore(metaName) })

	_, err = runner.CreateLocalBlockStore(localName, "fs",
		WithBlockRawConfig(fmt.Sprintf(`{"path":"%s"}`,
			filepath.Join(t.TempDir(), "local-blocks"))))
	require.NoError(t, err, "Should create fs local block store")
	t.Cleanup(func() { _ = runner.DeleteLocalBlockStore(localName) })

	bucketName := strings.ReplaceAll(
		fmt.Sprintf("dittofs-s3c-%s", UniqueTestName("bkt")), "_", "-")
	require.NoError(t, s3Helper.CreateBucket(context.Background(), bucketName),
		"Should create S3 bucket on emulator")
	t.Cleanup(func() { s3Helper.CleanupBucket(context.Background(), bucketName) })

	accessKey, secretKey := s3HelperCreds(s3Helper)
	_, err = runner.CreateRemoteBlockStore(remoteName, "s3",
		WithBlockS3Config(bucketName, "us-east-1", s3Helper.Endpoint, accessKey, secretKey),
		WithBlockAllowPrivateEndpoint())
	require.NoError(t, err, "Should create s3 remote block store")
	t.Cleanup(func() { _ = runner.DeleteRemoteBlockStore(remoteName) })

	_, err = runner.CreateShare(shareName, metaName, localName, WithShareRemote(remoteName))
	require.NoError(t, err, "Should create share")
	t.Cleanup(func() { _ = runner.DeleteShare(shareName) })

	return shareName
}

// SetupStoreMatrix creates metadata, local block, and (optionally) remote block
// stores for a 3D store matrix test, then creates a share referencing them.
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
		MetaStoreName:   UniqueTestName("meta"),
		LocalStoreName:  UniqueTestName("local"),
		RemoteStoreName: UniqueTestName("remote"),
		ShareName:       shareName,
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

	// Create local block store
	var localOpts []BlockStoreOption
	if sc.LocalType == "fs" {
		fsPath := filepath.Join(t.TempDir(), "local-blocks")
		localOpts = append(localOpts, WithBlockRawConfig(
			fmt.Sprintf(`{"path":"%s"}`, fsPath)))
	}

	_, err = runner.CreateLocalBlockStore(setup.LocalStoreName, sc.LocalType, localOpts...)
	require.NoError(t, err, "Should create local block store (%s)", sc.LocalType)
	t.Cleanup(func() { _ = runner.DeleteLocalBlockStore(setup.LocalStoreName) })

	// Create remote block store if needed
	var shareOpts []ShareOption
	if sc.HasRemote() {
		var remoteOpts []BlockStoreOption
		if sc.RemoteType == "s3" {
			require.NotNil(t, lsHelper, "Localstack helper not available")
			bucketName := strings.ReplaceAll(
				fmt.Sprintf("dittofs-mtx-%s", UniqueTestName("bkt")), "_", "-")
			err := lsHelper.CreateBucket(context.Background(), bucketName)
			require.NoError(t, err, "Should create S3 bucket")
			t.Cleanup(func() { lsHelper.CleanupBucket(context.Background(), bucketName) })

			accessKey, secretKey := s3HelperCreds(lsHelper)
			remoteOpts = append(remoteOpts, WithBlockS3Config(
				bucketName, "us-east-1", lsHelper.Endpoint, accessKey, secretKey),
				WithBlockAllowPrivateEndpoint())
		}

		_, err = runner.CreateRemoteBlockStore(setup.RemoteStoreName, sc.RemoteType, remoteOpts...)
		require.NoError(t, err, "Should create remote block store (%s)", sc.RemoteType)
		t.Cleanup(func() { _ = runner.DeleteRemoteBlockStore(setup.RemoteStoreName) })

		shareOpts = append(shareOpts, WithShareRemote(setup.RemoteStoreName))
	}

	// Create the share
	_, err = runner.CreateShare(shareName, setup.MetaStoreName, setup.LocalStoreName, shareOpts...)
	require.NoError(t, err, "Should create share")
	t.Cleanup(func() { _ = runner.DeleteShare(shareName) })

	return setup
}
