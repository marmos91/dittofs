//go:build integration

package s3

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/pkg/payload/store"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// localstackHelper manages the Localstack container for S3 integration tests.
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

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			"test", "test", "",
		)),
	)
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
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
		t.Fatalf("failed to create test bucket: %v", err)
	}
}

// cleanup terminates the container if we started one.
func (lh *localstackHelper) cleanup() {
	if lh.container != nil {
		ctx := context.Background()
		_ = lh.container.Terminate(ctx)
	}
}

// testStore holds the test store and cleanup function.
type testStore struct {
	*Store
	bucketName string
	helper     *localstackHelper
}

// newTestStore creates a new S3 store for testing.
func newTestStore(t *testing.T, helper *localstackHelper) *testStore {
	t.Helper()

	bucketName := fmt.Sprintf("test-bucket-%d", time.Now().UnixNano())
	helper.createBucket(t, bucketName)

	s := New(helper.client, Config{
		Bucket:    bucketName,
		KeyPrefix: "blocks/",
	})

	return &testStore{
		Store:      s,
		bucketName: bucketName,
		helper:     helper,
	}
}

func TestStore_WriteAndRead(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world")

	// Write block
	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read block
	read, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if string(read) != string(data) {
		t.Errorf("ReadBlock returned %q, want %q", read, data)
	}
}

func TestStore_ReadBlockNotFound(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	_, err := s.ReadBlock(ctx, "nonexistent")
	if err != store.ErrBlockNotFound {
		t.Errorf("ReadBlock returned error %v, want %v", err, store.ErrBlockNotFound)
	}
}

func TestStore_ReadBlockRange(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world")

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read range from start
	read, err := s.ReadBlockRange(ctx, blockKey, 0, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}

	if string(read) != "hello" {
		t.Errorf("ReadBlockRange returned %q, want %q", read, "hello")
	}

	// Read range from middle
	read, err = s.ReadBlockRange(ctx, blockKey, 6, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}

	if string(read) != "world" {
		t.Errorf("ReadBlockRange returned %q, want %q", read, "world")
	}
}

func TestStore_ReadBlockRangeNotFound(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	_, err := s.ReadBlockRange(ctx, "nonexistent", 0, 10)
	if err != store.ErrBlockNotFound {
		t.Errorf("ReadBlockRange returned error %v, want %v", err, store.ErrBlockNotFound)
	}
}

func TestStore_DeleteBlock(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	blockKey := "share1/content123/chunk-0/block-0"
	data := []byte("hello world")

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Delete block
	if err := s.DeleteBlock(ctx, blockKey); err != nil {
		t.Fatalf("DeleteBlock failed: %v", err)
	}

	// Verify block is deleted
	_, err := s.ReadBlock(ctx, blockKey)
	if err != store.ErrBlockNotFound {
		t.Errorf("ReadBlock after delete returned error %v, want %v", err, store.ErrBlockNotFound)
	}
}

func TestStore_DeleteByPrefix(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	// Write multiple blocks
	blocks := map[string][]byte{
		"share1/content123/chunk-0/block-0": []byte("data0"),
		"share1/content123/chunk-0/block-1": []byte("data1"),
		"share1/content123/chunk-1/block-0": []byte("data2"),
		"share2/content456/chunk-0/block-0": []byte("data3"),
	}

	for key, data := range blocks {
		if err := s.WriteBlock(ctx, key, data); err != nil {
			t.Fatalf("WriteBlock(%s) failed: %v", key, err)
		}
	}

	// Delete all blocks for share1/content123
	if err := s.DeleteByPrefix(ctx, "share1/content123"); err != nil {
		t.Fatalf("DeleteByPrefix failed: %v", err)
	}

	// Verify share1/content123 blocks are deleted
	for key := range blocks {
		_, err := s.ReadBlock(ctx, key)
		if key[:17] == "share1/content123" {
			if err != store.ErrBlockNotFound {
				t.Errorf("ReadBlock(%s) after delete returned error %v, want %v", key, err, store.ErrBlockNotFound)
			}
		} else {
			if err != nil {
				t.Errorf("ReadBlock(%s) after delete returned unexpected error: %v", key, err)
			}
		}
	}

	// Verify share2 is untouched
	read, err := s.ReadBlock(ctx, "share2/content456/chunk-0/block-0")
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}
	if string(read) != "data3" {
		t.Errorf("ReadBlock returned %q, want %q", read, "data3")
	}
}

func TestStore_ListByPrefix(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	// Write multiple blocks
	blocks := map[string][]byte{
		"share1/content123/chunk-0/block-0": []byte("data0"),
		"share1/content123/chunk-0/block-1": []byte("data1"),
		"share1/content123/chunk-1/block-0": []byte("data2"),
		"share2/content456/chunk-0/block-0": []byte("data3"),
	}

	for key, data := range blocks {
		if err := s.WriteBlock(ctx, key, data); err != nil {
			t.Fatalf("WriteBlock(%s) failed: %v", key, err)
		}
	}

	// List all blocks for share1/content123
	keys, err := s.ListByPrefix(ctx, "share1/content123")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 3 {
		t.Errorf("ListByPrefix returned %d keys, want 3: %v", len(keys), keys)
	}

	// List all blocks for share1
	keys, err = s.ListByPrefix(ctx, "share1")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 3 {
		t.Errorf("ListByPrefix returned %d keys, want 3: %v", len(keys), keys)
	}

	// List all blocks
	keys, err = s.ListByPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 4 {
		t.Errorf("ListByPrefix returned %d keys, want 4: %v", len(keys), keys)
	}
}

func TestStore_ListByPrefix_Empty(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	// List non-existent prefix
	keys, err := s.ListByPrefix(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}

	if len(keys) != 0 {
		t.Errorf("ListByPrefix returned %d keys, want 0", len(keys))
	}
}

func TestStore_ClosedOperations(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)

	// Close the store
	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// All operations should return ErrStoreClosed
	if _, err := s.ReadBlock(ctx, "key"); err != store.ErrStoreClosed {
		t.Errorf("ReadBlock on closed store returned %v, want %v", err, store.ErrStoreClosed)
	}

	if err := s.WriteBlock(ctx, "key", []byte("data")); err != store.ErrStoreClosed {
		t.Errorf("WriteBlock on closed store returned %v, want %v", err, store.ErrStoreClosed)
	}

	if err := s.DeleteBlock(ctx, "key"); err != store.ErrStoreClosed {
		t.Errorf("DeleteBlock on closed store returned %v, want %v", err, store.ErrStoreClosed)
	}

	if _, err := s.ListByPrefix(ctx, ""); err != store.ErrStoreClosed {
		t.Errorf("ListByPrefix on closed store returned %v, want %v", err, store.ErrStoreClosed)
	}

	if err := s.HealthCheck(ctx); err != store.ErrStoreClosed {
		t.Errorf("HealthCheck on closed store returned %v, want %v", err, store.ErrStoreClosed)
	}
}

func TestStore_HealthCheck(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	// Should be healthy
	if err := s.HealthCheck(ctx); err != nil {
		t.Errorf("HealthCheck failed: %v", err)
	}
}

func TestStore_OverwriteBlock(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	blockKey := "share1/content123/chunk-0/block-0"

	// Write initial data
	if err := s.WriteBlock(ctx, blockKey, []byte("initial")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Overwrite with new data
	if err := s.WriteBlock(ctx, blockKey, []byte("updated")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read and verify
	read, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if string(read) != "updated" {
		t.Errorf("ReadBlock returned %q, want %q", read, "updated")
	}
}

func TestStore_LargeBlock(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	blockKey := "share1/content123/chunk-0/block-0"

	// Write 4MB block (BlockSize)
	data := make([]byte, store.BlockSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Read full block
	read, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if len(read) != store.BlockSize {
		t.Errorf("ReadBlock returned %d bytes, want %d", len(read), store.BlockSize)
	}

	// Verify some bytes
	for i := 0; i < 100; i++ {
		if read[i] != byte(i%256) {
			t.Errorf("ReadBlock[%d] = %d, want %d", i, read[i], byte(i%256))
		}
	}

	// Read range from middle
	offset := int64(store.BlockSize / 2)
	length := int64(1024)
	rangeData, err := s.ReadBlockRange(ctx, blockKey, offset, length)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}

	if len(rangeData) != int(length) {
		t.Errorf("ReadBlockRange returned %d bytes, want %d", len(rangeData), length)
	}
}

func TestStore_DeleteNonExistent(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	s := newTestStore(t, helper)
	defer s.Close()

	// Delete non-existent block should not error (S3 behavior)
	if err := s.DeleteBlock(ctx, "nonexistent/block"); err != nil {
		t.Errorf("DeleteBlock on non-existent block returned error: %v", err)
	}

	// DeleteByPrefix on non-existent prefix should not error
	if err := s.DeleteByPrefix(ctx, "nonexistent/prefix"); err != nil {
		t.Errorf("DeleteByPrefix on non-existent prefix returned error: %v", err)
	}
}

func TestStore_KeyPrefix(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	ctx := context.Background()
	bucketName := fmt.Sprintf("test-bucket-%d", time.Now().UnixNano())
	helper.createBucket(t, bucketName)

	// Create store with custom prefix
	s := New(helper.client, Config{
		Bucket:    bucketName,
		KeyPrefix: "custom/prefix/",
	})
	defer s.Close()

	blockKey := "share1/block-0"
	data := []byte("test data")

	if err := s.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Verify key includes prefix by listing directly from S3
	resp, err := helper.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2 failed: %v", err)
	}

	if len(resp.Contents) != 1 {
		t.Fatalf("Expected 1 object, got %d", len(resp.Contents))
	}

	expectedKey := "custom/prefix/share1/block-0"
	if *resp.Contents[0].Key != expectedKey {
		t.Errorf("S3 key = %q, want %q", *resp.Contents[0].Key, expectedKey)
	}

	// Read should still work with block key (without prefix)
	read, err := s.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}

	if string(read) != string(data) {
		t.Errorf("ReadBlock returned %q, want %q", read, data)
	}
}

// ============================================================================
// Benchmarks
// ============================================================================

// benchmarkHelper is a shared helper for benchmark tests.
// It uses a package-level container to avoid restarting for each benchmark.
type benchmarkHelper struct {
	helper *localstackHelper
	store  *Store
}

// newBenchmarkHelper creates a benchmark helper with a shared Localstack container.
func newBenchmarkHelper(b *testing.B) *benchmarkHelper {
	b.Helper()

	// Create localstack helper (reuses existing container if LOCALSTACK_ENDPOINT is set)
	ctx := context.Background()

	// Check if external Localstack is configured via environment
	endpoint := os.Getenv("LOCALSTACK_ENDPOINT")
	if endpoint == "" {
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
			b.Fatalf("failed to start localstack container: %v", err)
		}

		host, err := container.Host(ctx)
		if err != nil {
			_ = container.Terminate(ctx)
			b.Fatalf("failed to get container host: %v", err)
		}

		port, err := container.MappedPort(ctx, "4566")
		if err != nil {
			_ = container.Terminate(ctx)
			b.Fatalf("failed to get container port: %v", err)
		}

		endpoint = fmt.Sprintf("http://%s:%s", host, port.Port())

		b.Cleanup(func() {
			_ = container.Terminate(ctx)
		})
	}

	// Create S3 client
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			"test", "test", "",
		)),
	)
	if err != nil {
		b.Fatalf("failed to load AWS config: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = &endpoint
		o.UsePathStyle = true
	})

	// Create bucket
	bucketName := fmt.Sprintf("bench-bucket-%d", time.Now().UnixNano())
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		b.Fatalf("failed to create test bucket: %v", err)
	}

	store := New(client, Config{
		Bucket:    bucketName,
		KeyPrefix: "blocks/",
	})

	b.Cleanup(func() {
		store.Close()
	})

	return &benchmarkHelper{
		store: store,
	}
}

func BenchmarkWriteBlock(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1024},
		{"64KB", 64 * 1024},
		{"1MB", 1024 * 1024},
		{"4MB", 4 * 1024 * 1024},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			bh := newBenchmarkHelper(b)
			ctx := context.Background()
			data := make([]byte, sz.size)

			b.SetBytes(int64(sz.size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				blockKey := fmt.Sprintf("bench/chunk-0/block-%d", i)
				if err := bh.store.WriteBlock(ctx, blockKey, data); err != nil {
					b.Fatalf("WriteBlock failed: %v", err)
				}
			}
		})
	}
}

func BenchmarkReadBlock(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1024},
		{"64KB", 64 * 1024},
		{"1MB", 1024 * 1024},
		{"4MB", 4 * 1024 * 1024},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			bh := newBenchmarkHelper(b)
			ctx := context.Background()
			data := make([]byte, sz.size)
			blockKey := "bench/chunk-0/block-0"

			// Pre-write the block
			if err := bh.store.WriteBlock(ctx, blockKey, data); err != nil {
				b.Fatalf("WriteBlock failed: %v", err)
			}

			b.SetBytes(int64(sz.size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := bh.store.ReadBlock(ctx, blockKey); err != nil {
					b.Fatalf("ReadBlock failed: %v", err)
				}
			}
		})
	}
}

func BenchmarkReadBlockRange(b *testing.B) {
	bh := newBenchmarkHelper(b)
	ctx := context.Background()

	// Write a 4MB block
	blockKey := "bench/chunk-0/block-0"
	data := make([]byte, 4*1024*1024)
	if err := bh.store.WriteBlock(ctx, blockKey, data); err != nil {
		b.Fatalf("WriteBlock failed: %v", err)
	}

	ranges := []struct {
		name   string
		offset int64
		length int64
	}{
		{"1KB_start", 0, 1024},
		{"1KB_middle", 2 * 1024 * 1024, 1024},
		{"64KB_start", 0, 64 * 1024},
		{"64KB_middle", 2 * 1024 * 1024, 64 * 1024},
	}

	for _, r := range ranges {
		b.Run(r.name, func(b *testing.B) {
			b.SetBytes(r.length)
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := bh.store.ReadBlockRange(ctx, blockKey, r.offset, r.length); err != nil {
					b.Fatalf("ReadBlockRange failed: %v", err)
				}
			}
		})
	}
}

func BenchmarkWriteBlock_Parallel(b *testing.B) {
	bh := newBenchmarkHelper(b)
	ctx := context.Background()
	data := make([]byte, 64*1024) // 64KB blocks

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			blockKey := fmt.Sprintf("bench/parallel/block-%d", i)
			if err := bh.store.WriteBlock(ctx, blockKey, data); err != nil {
				b.Fatalf("WriteBlock failed: %v", err)
			}
			i++
		}
	})
}
