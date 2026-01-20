//go:build integration

package transfer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/marmos91/dittofs/pkg/payload/store/fs"
	s3store "github.com/marmos91/dittofs/pkg/payload/store/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ============================================================================
// Filesystem GC Integration Tests
// ============================================================================

func TestCollectGarbage_Filesystem(t *testing.T) {
	ctx := context.Background()

	// Create temp directory for filesystem store
	tmpDir, err := os.MkdirTemp("", "gc-fs-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create filesystem block store
	blockStore, err := fs.New(fs.Config{
		BasePath:  tmpDir,
		CreateDir: true,
	})
	require.NoError(t, err)
	defer blockStore.Close()

	// Create blocks for two files
	validPayloadID := "export/valid-file.txt"
	orphanPayloadID := "export/orphan-file.txt"

	// Write blocks
	data := make([]byte, 1024)
	require.NoError(t, blockStore.WriteBlock(ctx, validPayloadID+"/chunk-0/block-0", data))
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-0", data))
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-1", data))

	// Set up reconciler with only valid file
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	createFileWithPayloadID(ctx, t, store, "/export", validPayloadID)

	// Run GC
	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 3, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanFiles)
	assert.Equal(t, 2, stats.OrphanBlocks)
	assert.Equal(t, 0, stats.Errors)

	// Verify orphan blocks were deleted
	keys, _ := blockStore.ListByPrefix(ctx, orphanPayloadID)
	assert.Empty(t, keys, "orphan blocks should be deleted from filesystem")

	// Verify valid blocks still exist
	keys, _ = blockStore.ListByPrefix(ctx, validPayloadID)
	assert.Len(t, keys, 1, "valid blocks should remain on filesystem")

	// Verify directory was cleaned up
	orphanDir := filepath.Join(tmpDir, orphanPayloadID)
	_, err = os.Stat(orphanDir)
	assert.True(t, os.IsNotExist(err), "orphan directory should be removed")
}

func TestCollectGarbage_Filesystem_LargeScale(t *testing.T) {
	ctx := context.Background()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "gc-fs-large-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create filesystem block store
	blockStore, err := fs.New(fs.Config{
		BasePath:  tmpDir,
		CreateDir: true,
	})
	require.NoError(t, err)
	defer blockStore.Close()

	// Create 100 files, 50% orphans
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")

	data := make([]byte, 1024)
	for i := 0; i < 100; i++ {
		payloadID := fmt.Sprintf("export/file-%d.txt", i)
		require.NoError(t, blockStore.WriteBlock(ctx, payloadID+"/chunk-0/block-0", data))

		if i%2 == 0 {
			createFileWithPayloadID(ctx, t, store, "/export", payloadID) // 50 valid files
		}
	}

	// Run GC
	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 100, stats.BlocksScanned)
	assert.Equal(t, 50, stats.OrphanFiles)
	assert.Equal(t, 50, stats.OrphanBlocks)
}

// ============================================================================
// S3 GC Integration Tests
// ============================================================================

func TestCollectGarbage_S3(t *testing.T) {
	ctx := context.Background()

	// Start Localstack or use existing
	endpoint, cleanup := getS3Endpoint(t)
	defer cleanup()

	// Create S3 client
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	require.NoError(t, err)

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	// Create bucket
	bucket := "gc-test-bucket"
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	require.NoError(t, err)
	defer func() {
		// Cleanup bucket
		cleanupBucket(ctx, client, bucket)
	}()

	// Create S3 block store (uses the client we already created)
	blockStore := s3store.New(client, s3store.Config{
		Bucket:    bucket,
		KeyPrefix: "blocks/",
	})

	// Create blocks for two files
	validPayloadID := "export/valid-file.txt"
	orphanPayloadID := "export/orphan-file.txt"

	data := make([]byte, 1024)
	require.NoError(t, blockStore.WriteBlock(ctx, validPayloadID+"/chunk-0/block-0", data))
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-0", data))
	require.NoError(t, blockStore.WriteBlock(ctx, orphanPayloadID+"/chunk-0/block-1", data))

	// Set up reconciler with only valid file
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")
	createFileWithPayloadID(ctx, t, store, "/export", validPayloadID)

	// Run GC
	stats := CollectGarbage(ctx, blockStore, reconciler, nil)

	assert.Equal(t, 3, stats.BlocksScanned)
	assert.Equal(t, 1, stats.OrphanFiles)
	assert.Equal(t, 2, stats.OrphanBlocks)
	assert.Equal(t, 0, stats.Errors)

	// Verify orphan blocks were deleted from S3
	keys, _ := blockStore.ListByPrefix(ctx, orphanPayloadID)
	assert.Empty(t, keys, "orphan blocks should be deleted from S3")

	// Verify valid blocks still exist in S3
	keys, _ = blockStore.ListByPrefix(ctx, validPayloadID)
	assert.Len(t, keys, 1, "valid blocks should remain in S3")
}

// ============================================================================
// S3 Benchmarks
// ============================================================================

func BenchmarkCollectGarbage_Filesystem(b *testing.B) {
	ctx := context.Background()

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "gc-bench-*")
	require.NoError(b, err)
	defer os.RemoveAll(tmpDir)

	// Create filesystem block store
	blockStore, err := fs.New(fs.Config{
		BasePath:  tmpDir,
		CreateDir: true,
	})
	require.NoError(b, err)
	defer blockStore.Close()

	// Set up reconciler with 50% orphans
	reconciler := newGCTestReconciler()
	store := reconciler.addShare("/export")

	data := make([]byte, 1024)
	for i := 0; i < 100; i++ {
		payloadID := fmt.Sprintf("export/file-%d.txt", i)
		blockStore.WriteBlock(ctx, payloadID+"/chunk-0/block-0", data)

		if i%2 == 0 {
			createFileWithPayloadID(ctx, b, store, "/export", payloadID)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Use dry run to avoid modifying the store
		CollectGarbage(ctx, blockStore, reconciler, &GCOptions{DryRun: true})
	}
}

// ============================================================================
// Helpers
// ============================================================================

// getS3Endpoint returns an S3 endpoint and cleanup function.
// Uses LOCALSTACK_ENDPOINT env var if set, otherwise starts a container.
func getS3Endpoint(t *testing.T) (string, func()) {
	t.Helper()

	// Check for pre-started Localstack
	if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
		t.Logf("Using pre-started Localstack at %s", endpoint)
		return endpoint, func() {}
	}

	// Start Localstack container
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "localstack/localstack:3.0",
		ExposedPorts: []string{"4566/tcp"},
		WaitingFor:   wait.ForListeningPort("4566/tcp"),
		Env: map[string]string{
			"SERVICES": "s3",
		},
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	host, err := container.Host(ctx)
	require.NoError(t, err)

	port, err := container.MappedPort(ctx, "4566")
	require.NoError(t, err)

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())
	t.Logf("Started Localstack at %s", endpoint)

	return endpoint, func() {
		container.Terminate(ctx)
	}
}

// cleanupBucket removes all objects from a bucket and deletes it.
func cleanupBucket(ctx context.Context, client *s3.Client, bucket string) {
	// List and delete all objects
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			break
		}

		if len(page.Contents) == 0 {
			continue
		}

		objects := make([]s3types.ObjectIdentifier, len(page.Contents))
		for i, obj := range page.Contents {
			objects[i] = s3types.ObjectIdentifier{Key: obj.Key}
		}

		client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &s3types.Delete{Objects: objects},
		})
	}

	// Delete bucket
	client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucket),
	})
}
