//go:build integration

package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/pkg/cache"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/payload/store"
	blockmemory "github.com/marmos91/dittofs/pkg/payload/store/memory"
	blocks3 "github.com/marmos91/dittofs/pkg/payload/store/s3"
	"github.com/marmos91/dittofs/pkg/payload/transfer"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// localstackHelper manages the Localstack container for integration tests.
type localstackHelper struct {
	container testcontainers.Container
	endpoint  string
	client    *s3.Client
}

// newLocalstackHelper starts a Localstack container or connects to an existing one.
func newLocalstackHelper(t *testing.T) *localstackHelper {
	t.Helper()
	ctx := context.Background()

	// Check if external Localstack is configured via environment
	if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
		helper := &localstackHelper{endpoint: endpoint}
		helper.createClient(t)
		return helper
	}

	// Start Localstack container using testcontainers
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
		t.Fatalf("failed to start localstack container: %v", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("failed to get container host: %v", err)
	}

	port, err := container.MappedPort(ctx, "4566")
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("failed to get container port: %v", err)
	}

	helper := &localstackHelper{
		container: container,
		endpoint:  fmt.Sprintf("http://%s:%s", host, port.Port()),
	}
	helper.createClient(t)

	return helper
}

// createClient creates an S3 client configured for Localstack.
func (lh *localstackHelper) createClient(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	cfg, err := awsConfig.LoadDefaultConfig(ctx,
		awsConfig.WithRegion("us-east-1"),
		awsConfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			"test", "test", "",
		)),
	)
	if err != nil {
		t.Fatalf("Failed to load AWS config: %v", err)
	}

	lh.client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = &lh.endpoint
		o.UsePathStyle = true
	})
}

// createBucket creates a new S3 bucket.
func (lh *localstackHelper) createBucket(t *testing.T, bucketName string) {
	t.Helper()
	ctx := context.Background()

	_, err := lh.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		t.Fatalf("Failed to create test bucket: %v", err)
	}
}

// cleanupBucket removes a bucket and all its contents.
func (lh *localstackHelper) cleanupBucket(bucketName string) {
	ctx := context.Background()

	listResp, _ := lh.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	})
	if listResp != nil {
		for _, obj := range listResp.Contents {
			_, _ = lh.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucketName),
				Key:    obj.Key,
			})
		}
	}

	_, _ = lh.client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucketName),
	})
}

// cleanup terminates the container if we started one.
func (lh *localstackHelper) cleanup() {
	if lh.container != nil {
		ctx := context.Background()
		_ = lh.container.Terminate(ctx)
	}
}

// TestS3BlockStore_Integration runs block store tests against Localstack.
func TestS3BlockStore_Integration(t *testing.T) {
	ctx := context.Background()

	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	bucketName := "dittofs-blockstore-test"
	helper.createBucket(t, bucketName)
	defer helper.cleanupBucket(bucketName)

	// Create block store
	blockStore := blocks3.New(helper.client, blocks3.Config{
		Bucket:    bucketName,
		KeyPrefix: "blocks/",
	})
	defer blockStore.Close()

	t.Run("WriteAndReadBlock", func(t *testing.T) {
		blockKey := "share1/content123/chunk-0/block-0"
		data := []byte("hello world from block store")

		// Write block
		err := blockStore.WriteBlock(ctx, blockKey, data)
		if err != nil {
			t.Fatalf("WriteBlock failed: %v", err)
		}

		// Read block back
		readData, err := blockStore.ReadBlock(ctx, blockKey)
		if err != nil {
			t.Fatalf("ReadBlock failed: %v", err)
		}

		if string(readData) != string(data) {
			t.Errorf("Data mismatch: got %q, want %q", readData, data)
		}
	})

	t.Run("ReadBlockRange", func(t *testing.T) {
		blockKey := "share1/content456/chunk-0/block-0"
		data := []byte("0123456789abcdefghij")

		// Write block
		err := blockStore.WriteBlock(ctx, blockKey, data)
		if err != nil {
			t.Fatalf("WriteBlock failed: %v", err)
		}

		// Read partial range
		rangeData, err := blockStore.ReadBlockRange(ctx, blockKey, 5, 10)
		if err != nil {
			t.Fatalf("ReadBlockRange failed: %v", err)
		}

		expected := "56789abcde"
		if string(rangeData) != expected {
			t.Errorf("Range data mismatch: got %q, want %q", rangeData, expected)
		}
	})

	t.Run("DeleteBlock", func(t *testing.T) {
		blockKey := "share1/content789/chunk-0/block-0"
		data := []byte("to be deleted")

		// Write block
		err := blockStore.WriteBlock(ctx, blockKey, data)
		if err != nil {
			t.Fatalf("WriteBlock failed: %v", err)
		}

		// Delete block
		err = blockStore.DeleteBlock(ctx, blockKey)
		if err != nil {
			t.Fatalf("DeleteBlock failed: %v", err)
		}

		// Try to read - should fail
		_, err = blockStore.ReadBlock(ctx, blockKey)
		if err != store.ErrBlockNotFound {
			t.Errorf("Expected ErrBlockNotFound, got: %v", err)
		}
	})

	t.Run("ListByPrefix", func(t *testing.T) {
		prefix := "share2/content-list/"

		// Write multiple blocks
		for i := 0; i < 3; i++ {
			blockKey := fmt.Sprintf("%schunk-0/block-%d", prefix, i)
			err := blockStore.WriteBlock(ctx, blockKey, []byte(fmt.Sprintf("block %d", i)))
			if err != nil {
				t.Fatalf("WriteBlock failed: %v", err)
			}
		}

		// List blocks
		keys, err := blockStore.ListByPrefix(ctx, prefix)
		if err != nil {
			t.Fatalf("ListByPrefix failed: %v", err)
		}

		if len(keys) != 3 {
			t.Errorf("Expected 3 keys, got %d: %v", len(keys), keys)
		}
	})

	t.Run("DeleteByPrefix", func(t *testing.T) {
		prefix := "share3/content-delete/"

		// Write multiple blocks
		for i := 0; i < 3; i++ {
			blockKey := fmt.Sprintf("%schunk-0/block-%d", prefix, i)
			err := blockStore.WriteBlock(ctx, blockKey, []byte(fmt.Sprintf("block %d", i)))
			if err != nil {
				t.Fatalf("WriteBlock failed: %v", err)
			}
		}

		// Delete all by prefix
		err := blockStore.DeleteByPrefix(ctx, prefix)
		if err != nil {
			t.Fatalf("DeleteByPrefix failed: %v", err)
		}

		// List - should be empty
		keys, err := blockStore.ListByPrefix(ctx, prefix)
		if err != nil {
			t.Fatalf("ListByPrefix failed: %v", err)
		}

		if len(keys) != 0 {
			t.Errorf("Expected 0 keys after delete, got %d: %v", len(keys), keys)
		}
	})
}

// TestFlusher_Integration tests the complete flusher workflow with S3.
func TestFlusher_Integration(t *testing.T) {
	ctx := context.Background()

	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	bucketName := "dittofs-flusher-test"
	helper.createBucket(t, bucketName)
	defer helper.cleanupBucket(bucketName)

	// Create block store
	blockStore := blocks3.New(helper.client, blocks3.Config{
		Bucket:    bucketName,
		KeyPrefix: "blocks/",
	})
	defer blockStore.Close()

	// Create cache
	c := cache.New(0)
	defer c.Close()

	// Create object store for deduplication
	objectStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()

	// Create flusher
	f := transfer.New(c, blockStore, objectStore, transfer.Config{
		ParallelUploads:   4,
		ParallelDownloads: 4,
	})
	f.Start(ctx) // Start queue workers for downloads
	defer f.Close()

	t.Run("FlushSmallFile", func(t *testing.T) {
		payloadID := "share1/content-small"
		data := []byte("hello world from flusher test")

		// Write data to cache
		err := c.WriteAt(ctx, payloadID, 0, data, 0)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Notify transfer manager of write completion
		f.OnWriteComplete(ctx, payloadID, 0, 0, uint32(len(data)))

		// Flush remaining data
		_, err = f.Flush(ctx, payloadID)
		if err != nil {
			t.Fatalf("Flush failed: %v", err)
		}

		// Wait for eager uploads to complete
		if err := f.WaitForAllUploads(ctx, payloadID); err != nil {
			t.Fatalf("WaitForAllUploads failed: %v", err)
		}

		// Verify data is in S3
		keys, err := blockStore.ListByPrefix(ctx, payloadID+"/")
		if err != nil {
			t.Fatalf("ListByPrefix failed: %v", err)
		}

		if len(keys) != 1 {
			t.Errorf("Expected 1 block, got %d: %v", len(keys), keys)
		}
	})

	t.Run("FlushLargeFile", func(t *testing.T) {
		payloadID := "share1/content-large"

		// Write 10MB of data (will create 3 blocks: 4MB + 4MB + 2MB)
		data := make([]byte, 10*1024*1024)
		for i := range data {
			data[i] = byte(i % 256)
		}

		err := c.WriteAt(ctx, payloadID, 0, data, 0)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Notify transfer manager of write completion
		f.OnWriteComplete(ctx, payloadID, 0, 0, uint32(len(data)))

		// Flush remaining data
		_, err = f.Flush(ctx, payloadID)
		if err != nil {
			t.Fatalf("Flush failed: %v", err)
		}

		// Wait for eager uploads to complete
		if err := f.WaitForAllUploads(ctx, payloadID); err != nil {
			t.Fatalf("WaitForAllUploads failed: %v", err)
		}

		// Verify blocks are in S3
		keys, err := blockStore.ListByPrefix(ctx, payloadID+"/")
		if err != nil {
			t.Fatalf("ListByPrefix failed: %v", err)
		}

		if len(keys) != 3 {
			t.Errorf("Expected 3 blocks, got %d: %v", len(keys), keys)
		}
	})

	t.Run("ReadFromS3", func(t *testing.T) {
		payloadID := "share1/content-read"

		// Pre-populate S3 with a block
		blockKey := payloadID + "/chunk-0/block-0"
		originalData := []byte("data from S3 for read test")
		err := blockStore.WriteBlock(ctx, blockKey, originalData)
		if err != nil {
			t.Fatalf("WriteBlock failed: %v", err)
		}

		// Ensure data is available (cache miss -> S3 fetch -> cache)
		err = f.EnsureAvailable(ctx, payloadID, 0, 0, uint32(len(originalData)))
		if err != nil {
			t.Fatalf("EnsureAvailable failed: %v", err)
		}

		// Read from cache
		readData := make([]byte, len(originalData))
		found, err := c.ReadAt(ctx, payloadID, 0, 0, uint32(len(originalData)), readData)
		if err != nil {
			t.Fatalf("Read failed: %v", err)
		}
		if !found {
			t.Fatal("Expected data to be found in cache after EnsureAvailable")
		}

		if string(readData) != string(originalData) {
			t.Errorf("Data mismatch: got %q, want %q", readData, originalData)
		}
	})
}

// TestFlusher_WithMemoryStore tests transfer manager with in-memory block store (fast).
func TestFlusher_WithMemoryStore(t *testing.T) {
	ctx := context.Background()

	// Create in-memory block store
	blockStore := blockmemory.New()
	defer blockStore.Close()

	// Create cache
	c := cache.New(0)
	defer c.Close()

	// Create object store for deduplication
	objectStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()

	// Create transfer manager
	f := transfer.New(c, blockStore, objectStore, transfer.Config{
		ParallelUploads:   4,
		ParallelDownloads: 4,
	})
	f.Start(ctx) // Start queue workers for downloads
	defer f.Close()

	t.Run("FlushAndRead", func(t *testing.T) {
		payloadID := "share1/content1"
		data := []byte("test data for memory store")

		// Write to cache
		err := c.WriteAt(ctx, payloadID, 0, data, 0)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Notify transfer manager of write completion
		f.OnWriteComplete(ctx, payloadID, 0, 0, uint32(len(data)))

		// Flush
		_, err = f.Flush(ctx, payloadID)
		if err != nil {
			t.Fatalf("Flush failed: %v", err)
		}

		// Wait for eager uploads to complete
		if err := f.WaitForAllUploads(ctx, payloadID); err != nil {
			t.Fatalf("WaitForAllUploads failed: %v", err)
		}

		// Clear cache to force block store read
		c.Remove(ctx, payloadID)

		// Ensure data is available (cache miss -> block store fetch -> cache)
		err = f.EnsureAvailable(ctx, payloadID, 0, 0, uint32(len(data)))
		if err != nil {
			t.Fatalf("EnsureAvailable failed: %v", err)
		}

		// Read from cache
		readData := make([]byte, len(data))
		found, err := c.ReadAt(ctx, payloadID, 0, 0, uint32(len(data)), readData)
		if err != nil {
			t.Fatalf("Read failed: %v", err)
		}
		if !found {
			t.Fatal("Expected data to be found in cache after EnsureAvailable")
		}

		if string(readData) != string(data) {
			t.Errorf("Data mismatch: got %q, want %q", readData, data)
		}
	})
}
