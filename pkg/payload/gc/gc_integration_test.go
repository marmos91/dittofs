//go:build integration

package gc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/pkg/payload/store/fs"
	s3store "github.com/marmos91/dittofs/pkg/payload/store/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// localstackHelper manages Localstack container for S3 tests.
type localstackHelper struct {
	container testcontainers.Container
	endpoint  string
	client    *s3.Client
}

// newLocalstackHelper starts or connects to Localstack.
func newLocalstackHelper(t *testing.T) *localstackHelper {
	t.Helper()
	ctx := context.Background()

	// Check for external Localstack
	if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
		helper := &localstackHelper{endpoint: endpoint}
		helper.createClient(t)
		return helper
	}

	// Start Localstack container
	req := testcontainers.ContainerRequest{
		Image:        "localstack/localstack:3.0",
		ExposedPorts: []string{"4566/tcp"},
		Env: map[string]string{
			"SERVICES":              "s3",
			"DEFAULT_REGION":        "us-east-1",
			"EAGER_SERVICE_LOADING": "1",
		},
		WaitingFor: wait.ForAll(
			wait.ForListeningPort("4566/tcp"),
			wait.ForHTTP("/_localstack/health").
				WithPort("4566/tcp").
				WithStartupTimeout(60*time.Second),
		),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("failed to start localstack: %v", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx)
		t.Fatalf("failed to get host: %v", err)
	}

	port, err := container.MappedPort(ctx, "4566")
	if err != nil {
		container.Terminate(ctx)
		t.Fatalf("failed to get port: %v", err)
	}

	helper := &localstackHelper{
		container: container,
		endpoint:  fmt.Sprintf("http://%s:%s", host, port.Port()),
	}
	helper.createClient(t)
	return helper
}

func (h *localstackHelper) createClient(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
	}

	h.client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(h.endpoint)
		o.UsePathStyle = true
	})
}

func (h *localstackHelper) createBucket(t *testing.T, bucket string) {
	t.Helper()
	ctx := context.Background()

	_, err := h.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		t.Fatalf("failed to create bucket: %v", err)
	}
}

func (h *localstackHelper) close(t *testing.T) {
	if h.container != nil {
		if err := h.container.Terminate(context.Background()); err != nil {
			t.Logf("warning: failed to terminate container: %v", err)
		}
	}
}

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

	// Reuse the shared localstackHelper.
	helper := newLocalstackHelper(t)
	defer helper.close(t)

	// Create a dedicated bucket for GC testing with unique name to avoid flakiness.
	bucketName := fmt.Sprintf("gc-test-%d", time.Now().UnixNano())
	helper.createBucket(t, bucketName)

	// Create S3 block store using the helper's client.
	blockStore := s3store.New(helper.client, s3store.Config{
		Bucket:    bucketName,
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
		CollectGarbage(ctx, blockStore, reconciler, &Options{DryRun: true})
	}
}
