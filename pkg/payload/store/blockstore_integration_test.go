//go:build integration

package store_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/payload/offloader"
	"github.com/marmos91/dittofs/pkg/payload/store"
	blockmemory "github.com/marmos91/dittofs/pkg/payload/store/memory"
	blocks3 "github.com/marmos91/dittofs/pkg/payload/store/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// sharedHelper is a package-level Localstack container shared across all tests.
var sharedHelper *localstackHelper

// localstackHelper manages the Localstack container for integration tests.
type localstackHelper struct {
	container testcontainers.Container
	endpoint  string
	client    *s3.Client
}

// startSharedLocalstack starts a single Localstack container for the entire
// test package. Returns a cleanup function.
func startSharedLocalstack() func() {
	ctx := context.Background()

	// Check if external Localstack is configured via environment
	if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
		helper := &localstackHelper{endpoint: endpoint}
		helper.initClient()
		sharedHelper = helper
		return func() {}
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
		log.Fatalf("failed to start localstack container: %v", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		log.Fatalf("failed to get container host: %v", err)
	}

	port, err := container.MappedPort(ctx, "4566")
	if err != nil {
		_ = container.Terminate(ctx)
		log.Fatalf("failed to get container port: %v", err)
	}

	helper := &localstackHelper{
		container: container,
		endpoint:  fmt.Sprintf("http://%s:%s", host, port.Port()),
	}
	helper.initClient()
	sharedHelper = helper

	return func() {
		_ = container.Terminate(ctx)
	}
}

// initClient creates an S3 client configured for Localstack.
func (lh *localstackHelper) initClient() {
	ctx := context.Background()

	cfg, err := awsConfig.LoadDefaultConfig(ctx,
		awsConfig.WithRegion("us-east-1"),
		awsConfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			"test", "test", "",
		)),
	)
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
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

func TestMain(m *testing.M) {
	cleanup := startSharedLocalstack()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// TestS3BlockStore_Integration runs block store tests against Localstack.
func TestS3BlockStore_Integration(t *testing.T) {
	ctx := context.Background()

	bucketName := "dittofs-blockstore-test"
	sharedHelper.createBucket(t, bucketName)
	defer sharedHelper.cleanupBucket(bucketName)

	// Create block store
	blockStore := blocks3.New(sharedHelper.client, blocks3.Config{
		Bucket:    bucketName,
		KeyPrefix: "blocks/",
	})
	defer blockStore.Close()

	t.Run("WriteAndReadBlock", func(t *testing.T) {
		blockKey := "share1/content123/block-0"
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
		blockKey := "share1/content456/block-0"
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
		blockKey := "share1/content789/block-0"
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
			blockKey := fmt.Sprintf("%sblock-%d", prefix, i)
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
			blockKey := fmt.Sprintf("%sblock-%d", prefix, i)
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

	bucketName := "dittofs-flusher-test"
	sharedHelper.createBucket(t, bucketName)
	defer sharedHelper.cleanupBucket(bucketName)

	// Create block store
	blockStore := blocks3.New(sharedHelper.client, blocks3.Config{
		Bucket:    bucketName,
		KeyPrefix: "blocks/",
	})
	defer blockStore.Close()

	// Create file block store for deduplication
	fileBlockStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()

	// Create cache
	cacheTmpDir := t.TempDir()
	bc, err := cache.New(cacheTmpDir, 0, 0, fileBlockStore)
	if err != nil {
		t.Fatalf("cache.New() error = %v", err)
	}
	defer bc.Close()

	// Create flusher
	f := offloader.New(bc, blockStore, fileBlockStore, offloader.Config{
		ParallelUploads:   4,
		ParallelDownloads: 4,
	})
	f.Start(ctx) // Start queue workers for downloads
	defer f.Close()

	t.Run("FlushSmallFile", func(t *testing.T) {
		payloadID := "share1/content-small"
		data := []byte("hello world from flusher test")

		err := bc.WriteAt(ctx, payloadID, data, 0)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		_, err = f.Flush(ctx, payloadID)
		if err != nil {
			t.Fatalf("Flush failed: %v", err)
		}

		// Flush is decoupled from upload — trigger immediate sync
		f.SyncNow(ctx)

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

		data := make([]byte, 10*1024*1024)
		for i := range data {
			data[i] = byte(i % 256)
		}

		err := bc.WriteAt(ctx, payloadID, data, 0)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		_, err = f.Flush(ctx, payloadID)
		if err != nil {
			t.Fatalf("Flush failed: %v", err)
		}

		f.SyncNow(ctx)

		keys, err := blockStore.ListByPrefix(ctx, payloadID+"/")
		if err != nil {
			t.Fatalf("ListByPrefix failed: %v", err)
		}

		if len(keys) != 2 {
			t.Errorf("Expected 2 blocks (8MB block size), got %d: %v", len(keys), keys)
		}
	})

	t.Run("ReadFromS3", func(t *testing.T) {
		payloadID := "share1/content-read"

		// Pre-populate S3 with a block
		blockKey := payloadID + "/block-0"
		originalData := []byte("data from S3 for read test")
		err := blockStore.WriteBlock(ctx, blockKey, originalData)
		if err != nil {
			t.Fatalf("WriteBlock failed: %v", err)
		}

		// Register FileBlock so resolveStoreKey can find it
		err = fileBlockStore.PutFileBlock(ctx, &metadata.FileBlock{
			ID:            payloadID + "/0",
			State:         metadata.BlockStateRemote,
			BlockStoreKey: blockKey,
			DataSize:      uint32(len(originalData)),
		})
		if err != nil {
			t.Fatalf("PutFileBlock failed: %v", err)
		}

		// Ensure data is available (cache miss -> S3 fetch -> cache)
		err = f.EnsureAvailable(ctx, payloadID, 0, uint32(len(originalData)))
		if err != nil {
			t.Fatalf("EnsureAvailable failed: %v", err)
		}

		// Read from cache
		readData := make([]byte, len(originalData))
		found, err := bc.ReadAt(ctx, payloadID, readData, 0)
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

	// Create file block store for deduplication
	fileBlockStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()

	// Create cache
	cacheTmpDir := t.TempDir()
	bc, err := cache.New(cacheTmpDir, 0, 0, fileBlockStore)
	if err != nil {
		t.Fatalf("cache.New() error = %v", err)
	}
	defer bc.Close()

	// Create transfer manager
	f := offloader.New(bc, blockStore, fileBlockStore, offloader.Config{
		ParallelUploads:   4,
		ParallelDownloads: 4,
	})
	f.Start(ctx) // Start queue workers for downloads
	defer f.Close()

	t.Run("FlushAndRead", func(t *testing.T) {
		payloadID := "share1/content1"
		data := []byte("test data for memory store")

		// Write to cache
		err := bc.WriteAt(ctx, payloadID, data, 0)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Flush
		_, err = f.Flush(ctx, payloadID)
		if err != nil {
			t.Fatalf("Flush failed: %v", err)
		}

		// Flush is decoupled from upload — trigger immediate sync
		f.SyncNow(ctx)

		// Clear cache to force block store read
		bc.EvictMemory(ctx, payloadID)

		// Ensure data is available (cache miss -> block store fetch -> cache)
		err = f.EnsureAvailable(ctx, payloadID, 0, uint32(len(data)))
		if err != nil {
			t.Fatalf("EnsureAvailable failed: %v", err)
		}

		// Read from cache
		readData := make([]byte, len(data))
		found, err := bc.ReadAt(ctx, payloadID, readData, 0)
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
