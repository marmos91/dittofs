//go:build integration

package transfer

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/payload/store"
	"github.com/marmos91/dittofs/pkg/payload/store/fs"
	"github.com/marmos91/dittofs/pkg/payload/store/memory"
	s3store "github.com/marmos91/dittofs/pkg/payload/store/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ============================================================================
// Test Helpers
// ============================================================================

// testEnv holds the test environment with cache and block store.
type testEnv struct {
	cache      *cache.Cache
	blockStore store.BlockStore
	manager    *TransferManager
	cleanup    func()
}

// newMemoryEnv creates a test environment with memory block store.
func newMemoryEnv(t *testing.T) *testEnv {
	t.Helper()
	c := cache.New(0) // Unlimited
	bs := memory.New()
	m := New(c, bs, DefaultConfig())
	m.Start(context.Background())

	return &testEnv{
		cache:      c,
		blockStore: bs,
		manager:    m,
		cleanup: func() {
			m.Close()
			bs.Close()
		},
	}
}

// newFilesystemEnv creates a test environment with filesystem block store.
func newFilesystemEnv(t *testing.T) *testEnv {
	t.Helper()
	c := cache.New(0)

	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "transfer-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	bs, err := fs.NewWithPath(tmpDir)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to create fs store: %v", err)
	}

	m := New(c, bs, DefaultConfig())
	m.Start(context.Background())

	return &testEnv{
		cache:      c,
		blockStore: bs,
		manager:    m,
		cleanup: func() {
			m.Close()
			bs.Close()
			os.RemoveAll(tmpDir)
		},
	}
}

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

// s3BenchHelper manages S3 for benchmarks (Localstack or real AWS S3).
// Environment variables:
//   - S3_BENCHMARK_BUCKET: Use real AWS S3 with this bucket (must have AWS credentials configured)
//   - S3_BENCHMARK_REGION: AWS region (default: us-east-1)
//   - LOCALSTACK_ENDPOINT: Use external Localstack at this endpoint
//   - (none): Auto-start Localstack via testcontainers
type s3BenchHelper struct {
	container testcontainers.Container
	client    *s3.Client
	bucket    string
	isRealS3  bool // true if using real AWS S3
}

// newS3BenchHelper creates an S3 helper for benchmarks.
func newS3BenchHelper(b *testing.B) *s3BenchHelper {
	b.Helper()
	ctx := context.Background()

	// Option 1: Real AWS S3 (for production benchmarks)
	if bucket := os.Getenv("S3_BENCHMARK_BUCKET"); bucket != "" {
		region := os.Getenv("S3_BENCHMARK_REGION")
		if region == "" {
			region = "us-east-1"
		}

		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
		if err != nil {
			b.Fatalf("failed to load AWS config: %v", err)
		}

		client := s3.NewFromConfig(cfg)

		// Verify bucket access
		_, err = client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
		if err != nil {
			b.Fatalf("cannot access S3 bucket %s: %v", bucket, err)
		}

		b.Logf("Using real AWS S3 bucket: %s in %s", bucket, region)
		return &s3BenchHelper{
			client:   client,
			bucket:   bucket,
			isRealS3: true,
		}
	}

	// Option 2: External Localstack
	if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
		cfg, err := awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion("us-east-1"),
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
		)
		if err != nil {
			b.Fatalf("failed to load AWS config: %v", err)
		}

		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		})

		bucket := fmt.Sprintf("bench-bucket-%d", time.Now().UnixNano())
		_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		if err != nil {
			b.Fatalf("failed to create bucket: %v", err)
		}

		return &s3BenchHelper{
			client: client,
			bucket: bucket,
		}
	}

	// Option 3: Start Localstack container via testcontainers
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
		b.Fatalf("failed to start localstack: %v", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx)
		b.Fatalf("failed to get host: %v", err)
	}

	port, err := container.MappedPort(ctx, "4566")
	if err != nil {
		container.Terminate(ctx)
		b.Fatalf("failed to get port: %v", err)
	}

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		container.Terminate(ctx)
		b.Fatalf("failed to load AWS config: %v", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	bucket := fmt.Sprintf("bench-bucket-%d", time.Now().UnixNano())
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		container.Terminate(ctx)
		b.Fatalf("failed to create bucket: %v", err)
	}

	return &s3BenchHelper{
		container: container,
		client:    client,
		bucket:    bucket,
	}
}

// cleanup removes all objects from the bucket and terminates the container if started.
func (h *s3BenchHelper) cleanup(b *testing.B) {
	ctx := context.Background()

	// Delete all objects in the bucket with the benchmark prefix
	prefix := "blocks/"
	paginator := s3.NewListObjectsV2Paginator(h.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(h.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			b.Logf("warning: failed to list objects for cleanup: %v", err)
			break
		}

		if len(page.Contents) == 0 {
			break
		}

		var objects []s3types.ObjectIdentifier
		for _, obj := range page.Contents {
			objects = append(objects, s3types.ObjectIdentifier{Key: obj.Key})
		}

		_, err = h.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(h.bucket),
			Delete: &s3types.Delete{Objects: objects},
		})
		if err != nil {
			b.Logf("warning: failed to delete objects: %v", err)
		}
	}

	// Delete the bucket (only for Localstack - real S3 bucket is reused)
	if !h.isRealS3 {
		h.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(h.bucket)})
	}

	// Terminate container if we started one
	if h.container != nil {
		h.container.Terminate(ctx)
	}
}

// newS3EnvForBench creates an S3 test environment for benchmarks.
// Supports three modes via environment variables:
//   - S3_BENCHMARK_BUCKET: Use real AWS S3 (requires AWS credentials)
//   - LOCALSTACK_ENDPOINT: Use external Localstack
//   - (default): Auto-start Localstack via testcontainers
func newS3EnvForBench(b *testing.B) *testEnv {
	b.Helper()

	helper := newS3BenchHelper(b)
	c := cache.New(0)

	bs := s3store.New(helper.client, s3store.Config{
		Bucket:    helper.bucket,
		KeyPrefix: "blocks/",
	})

	m := New(c, bs, DefaultConfig())
	m.Start(context.Background())

	return &testEnv{
		cache:      c,
		blockStore: bs,
		manager:    m,
		cleanup: func() {
			m.Close()
			bs.Close()
			helper.cleanup(b)
		},
	}
}

// newS3Env creates a test environment with S3 block store.
func newS3Env(t *testing.T, helper *localstackHelper) *testEnv {
	t.Helper()
	c := cache.New(0)

	bucket := fmt.Sprintf("test-bucket-%d", time.Now().UnixNano())
	helper.createBucket(t, bucket)

	bs := s3store.New(helper.client, s3store.Config{
		Bucket:    bucket,
		KeyPrefix: "blocks/",
	})

	m := New(c, bs, DefaultConfig())
	m.Start(context.Background())

	return &testEnv{
		cache:      c,
		blockStore: bs,
		manager:    m,
		cleanup: func() {
			m.Close()
			bs.Close()
		},
	}
}

// randomData generates random data of specified size.
func randomData(size int) []byte {
	data := make([]byte, size)
	rand.Read(data)
	return data
}

// ============================================================================
// Integration Tests
// ============================================================================

func TestTransferManager_WriteAndFlush_Memory(t *testing.T) {
	env := newMemoryEnv(t)
	defer env.cleanup()
	testWriteAndFlush(t, env)
}

func TestTransferManager_WriteAndFlush_Filesystem(t *testing.T) {
	env := newFilesystemEnv(t)
	defer env.cleanup()
	testWriteAndFlush(t, env)
}

func TestTransferManager_WriteAndFlush_S3(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.close(t)
	env := newS3Env(t, helper)
	defer env.cleanup()
	testWriteAndFlush(t, env)
}

func testWriteAndFlush(t *testing.T, env *testEnv) {
	ctx := context.Background()
	payloadID := "export/test-file.bin"

	// Write data to cache (simulating NFS writes)
	data := randomData(8 * 1024 * 1024) // 8MB = 2 blocks
	chunkIdx := uint32(0)

	// Write in 32KB chunks (NFS-like)
	chunkSize := 32 * 1024
	for offset := 0; offset < len(data); offset += chunkSize {
		end := offset + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[offset:end]

		if err := env.cache.WriteAt(ctx, payloadID, chunkIdx, chunk, uint32(offset)); err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Notify transfer manager of write
		env.manager.OnWriteComplete(ctx, payloadID, chunkIdx, uint32(offset), uint32(len(chunk)))
	}

	// Wait a bit for eager uploads
	time.Sleep(100 * time.Millisecond)

	// Flush remaining data
	result, err := env.manager.Flush(ctx, payloadID)
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	if !result.Finalized {
		t.Error("Flush should be finalized")
	}

	// Verify data in block store
	exists, err := env.manager.Exists(ctx, "", payloadID)
	if err != nil {
		t.Fatalf("Exists failed: %v", err)
	}
	if !exists {
		t.Error("File should exist in block store")
	}

	// Verify size
	size, err := env.manager.GetFileSize(ctx, "", payloadID)
	if err != nil {
		t.Fatalf("GetFileSize failed: %v", err)
	}
	if size != uint64(len(data)) {
		t.Errorf("Size mismatch: got %d, want %d", size, len(data))
	}
}

func TestTransferManager_DownloadOnCacheMiss_Memory(t *testing.T) {
	env := newMemoryEnv(t)
	defer env.cleanup()
	testDownloadOnCacheMiss(t, env)
}

func TestTransferManager_DownloadOnCacheMiss_Filesystem(t *testing.T) {
	env := newFilesystemEnv(t)
	defer env.cleanup()
	testDownloadOnCacheMiss(t, env)
}

func TestTransferManager_DownloadOnCacheMiss_S3(t *testing.T) {
	helper := newLocalstackHelper(t)
	defer helper.close(t)
	env := newS3Env(t, helper)
	defer env.cleanup()
	testDownloadOnCacheMiss(t, env)
}

func testDownloadOnCacheMiss(t *testing.T, env *testEnv) {
	ctx := context.Background()
	payloadID := "export/download-test.bin"

	// Write blocks directly to block store (simulating pre-existing data)
	blockData := randomData(BlockSize)
	blockKey := FormatBlockKey(payloadID, 0, 0)
	if err := env.blockStore.WriteBlock(ctx, blockKey, blockData); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Request data via EnsureAvailable (should download from block store)
	if err := env.manager.EnsureAvailable(ctx, payloadID, 0, 0, BlockSize); err != nil {
		t.Fatalf("EnsureAvailable failed: %v", err)
	}

	// Verify data is now in cache
	dest := make([]byte, BlockSize)
	found, err := env.cache.Read(ctx, payloadID, 0, 0, BlockSize, dest)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if !found {
		t.Error("Data should be in cache after EnsureAvailable")
	}

	// Verify data matches
	for i := 0; i < len(blockData); i++ {
		if dest[i] != blockData[i] {
			t.Errorf("Data mismatch at byte %d", i)
			break
		}
	}
}

func TestTransferManager_ConcurrentOperations_Memory(t *testing.T) {
	env := newMemoryEnv(t)
	defer env.cleanup()
	testConcurrentOperations(t, env)
}

func testConcurrentOperations(t *testing.T, env *testEnv) {
	ctx := context.Background()
	numFiles := 10
	fileSize := 4 * 1024 * 1024 // 4MB per file

	var wg sync.WaitGroup
	errors := make(chan error, numFiles)

	for i := 0; i < numFiles; i++ {
		wg.Add(1)
		go func(fileIdx int) {
			defer wg.Done()

			payloadID := fmt.Sprintf("export/concurrent-%d.bin", fileIdx)
			data := randomData(fileSize)

			// Write to cache
			if err := env.cache.WriteAt(ctx, payloadID, 0, data, 0); err != nil {
				errors <- fmt.Errorf("file %d: Write failed: %w", fileIdx, err)
				return
			}

			// Notify and flush
			env.manager.OnWriteComplete(ctx, payloadID, 0, 0, uint32(fileSize))
			if _, err := env.manager.Flush(ctx, payloadID); err != nil {
				errors <- fmt.Errorf("file %d: Flush failed: %w", fileIdx, err)
				return
			}

			// Wait for eager uploads to complete before verification
			if err := env.manager.WaitForEagerUploads(ctx, payloadID); err != nil {
				errors <- fmt.Errorf("file %d: WaitForEagerUploads failed: %w", fileIdx, err)
				return
			}

			// Verify
			exists, err := env.manager.Exists(ctx, "", payloadID)
			if err != nil {
				errors <- fmt.Errorf("file %d: Exists failed: %w", fileIdx, err)
				return
			}
			if !exists {
				errors <- fmt.Errorf("file %d: should exist", fileIdx)
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}

// ============================================================================
// Benchmarks
// ============================================================================

func BenchmarkUpload_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkUpload(b, env)
}

func BenchmarkUpload_Filesystem(b *testing.B) {
	env := newFilesystemEnvForBench(b)
	defer env.cleanup()
	benchmarkUpload(b, env)
}

func BenchmarkDownload_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkDownload(b, env)
}

func BenchmarkDownload_Filesystem(b *testing.B) {
	env := newFilesystemEnvForBench(b)
	defer env.cleanup()
	benchmarkDownload(b, env)
}

func BenchmarkFlush_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkFlush(b, env)
}

func BenchmarkFlush_Filesystem(b *testing.B) {
	env := newFilesystemEnvForBench(b)
	defer env.cleanup()
	benchmarkFlush(b, env)
}

func BenchmarkConcurrentUpload_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkConcurrentUpload(b, env, 4)
}

func BenchmarkConcurrentUpload_Filesystem(b *testing.B) {
	env := newFilesystemEnvForBench(b)
	defer env.cleanup()
	benchmarkConcurrentUpload(b, env, 4)
}

// Benchmark helpers that use testing.B
func newMemoryEnvForBench(b *testing.B) *testEnv {
	b.Helper()
	c := cache.New(0)
	bs := memory.New()
	m := New(c, bs, DefaultConfig())
	m.Start(context.Background())

	return &testEnv{
		cache:      c,
		blockStore: bs,
		manager:    m,
		cleanup: func() {
			m.Close()
			bs.Close()
		},
	}
}

func newFilesystemEnvForBench(b *testing.B) *testEnv {
	b.Helper()
	c := cache.New(0)

	tmpDir, err := os.MkdirTemp("", "transfer-bench-*")
	if err != nil {
		b.Fatalf("failed to create temp dir: %v", err)
	}

	bs, err := fs.NewWithPath(tmpDir)
	if err != nil {
		os.RemoveAll(tmpDir)
		b.Fatalf("failed to create fs store: %v", err)
	}

	m := New(c, bs, DefaultConfig())
	m.Start(context.Background())

	return &testEnv{
		cache:      c,
		blockStore: bs,
		manager:    m,
		cleanup: func() {
			m.Close()
			bs.Close()
			os.RemoveAll(tmpDir)
		},
	}
}

func benchmarkUpload(b *testing.B, env *testEnv) {
	ctx := context.Background()
	data := randomData(BlockSize) // 4MB block

	b.SetBytes(int64(BlockSize))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("export/bench-upload-%d.bin", i)

		// Write to cache
		if err := env.cache.WriteAt(ctx, payloadID, 0, data, 0); err != nil {
			b.Fatalf("Write failed: %v", err)
		}

		// Trigger upload and wait
		env.manager.OnWriteComplete(ctx, payloadID, 0, 0, uint32(BlockSize))
		if _, err := env.manager.Flush(ctx, payloadID); err != nil {
			b.Fatalf("Flush failed: %v", err)
		}
	}
}

func benchmarkDownload(b *testing.B, env *testEnv) {
	ctx := context.Background()
	data := randomData(BlockSize)

	// Pre-populate block store
	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("export/bench-download-%d.bin", i)
		blockKey := FormatBlockKey(payloadID, 0, 0)
		if err := env.blockStore.WriteBlock(ctx, blockKey, data); err != nil {
			b.Fatalf("WriteBlock failed: %v", err)
		}
	}

	b.SetBytes(int64(BlockSize))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("export/bench-download-%d.bin", i)
		if err := env.manager.EnsureAvailable(ctx, payloadID, 0, 0, BlockSize); err != nil {
			b.Fatalf("EnsureAvailable failed: %v", err)
		}
	}
}

func benchmarkFlush(b *testing.B, env *testEnv) {
	ctx := context.Background()
	data := randomData(BlockSize)

	b.SetBytes(int64(BlockSize))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("export/bench-flush-%d.bin", i)

		// Write to cache (partial block to ensure flush uploads it)
		if err := env.cache.WriteAt(ctx, payloadID, 0, data[:BlockSize/2], 0); err != nil {
			b.Fatalf("Write failed: %v", err)
		}

		// Flush
		if _, err := env.manager.Flush(ctx, payloadID); err != nil {
			b.Fatalf("Flush failed: %v", err)
		}
	}
}

func benchmarkConcurrentUpload(b *testing.B, env *testEnv, parallelism int) {
	ctx := context.Background()
	data := randomData(BlockSize)

	b.SetBytes(int64(BlockSize) * int64(parallelism))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup

		for j := 0; j < parallelism; j++ {
			wg.Add(1)
			go func(fileIdx int) {
				defer wg.Done()

				payloadID := fmt.Sprintf("export/bench-concurrent-%d-%d.bin", i, fileIdx)

				if err := env.cache.WriteAt(ctx, payloadID, 0, data, 0); err != nil {
					return
				}

				env.manager.OnWriteComplete(ctx, payloadID, 0, 0, uint32(BlockSize))
				env.manager.Flush(ctx, payloadID)
			}(j)
		}

		wg.Wait()
	}
}

// ============================================================================
// Large File Benchmarks
// ============================================================================

func BenchmarkLargeFile_16MB_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkLargeFile(b, env, 16*1024*1024)
}

func BenchmarkLargeFile_64MB_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkLargeFile(b, env, 64*1024*1024)
}

func BenchmarkLargeFile_16MB_Filesystem(b *testing.B) {
	env := newFilesystemEnvForBench(b)
	defer env.cleanup()
	benchmarkLargeFile(b, env, 16*1024*1024)
}

func BenchmarkLargeFile_64MB_Filesystem(b *testing.B) {
	env := newFilesystemEnvForBench(b)
	defer env.cleanup()
	benchmarkLargeFile(b, env, 64*1024*1024)
}

func benchmarkLargeFile(b *testing.B, env *testEnv, fileSize int) {
	ctx := context.Background()
	data := randomData(fileSize)
	writeChunkSize := 32 * 1024 // 32KB (NFS-like writes)

	b.SetBytes(int64(fileSize))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("export/large-file-%d.bin", i)

		// Write in chunks (simulating NFS)
		for offset := 0; offset < fileSize; offset += writeChunkSize {
			end := offset + writeChunkSize
			if end > fileSize {
				end = fileSize
			}

			if err := env.cache.WriteAt(ctx, payloadID, 0, data[offset:end], uint32(offset)); err != nil {
				b.Fatalf("Write failed: %v", err)
			}
			env.manager.OnWriteComplete(ctx, payloadID, 0, uint32(offset), uint32(end-offset))
		}

		// Flush
		if _, err := env.manager.Flush(ctx, payloadID); err != nil {
			b.Fatalf("Flush failed: %v", err)
		}
	}
}

// ============================================================================
// Sequential Write Performance (Critical Path)
// ============================================================================

func BenchmarkSequentialWrite_32KB_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkSequentialWrite(b, env, 32*1024)
}

func BenchmarkSequentialWrite_64KB_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkSequentialWrite(b, env, 64*1024)
}

func BenchmarkSequentialWrite_32KB_Filesystem(b *testing.B) {
	env := newFilesystemEnvForBench(b)
	defer env.cleanup()
	benchmarkSequentialWrite(b, env, 32*1024)
}

func benchmarkSequentialWrite(b *testing.B, env *testEnv, writeSize int) {
	ctx := context.Background()
	data := randomData(writeSize)
	totalBytes := int64(b.N) * int64(writeSize)

	b.SetBytes(int64(writeSize))
	b.ResetTimer()

	payloadID := "export/sequential-write.bin"
	for i := 0; i < b.N; i++ {
		// Calculate file offset and map to chunk/offset within chunk
		fileOffset := uint64(i) * uint64(writeSize)
		chunkIdx := uint32(fileOffset / uint64(cache.ChunkSize))
		offsetInChunk := uint32(fileOffset % uint64(cache.ChunkSize))

		if err := env.cache.WriteAt(ctx, payloadID, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatalf("Write failed: %v", err)
		}
		env.manager.OnWriteComplete(ctx, payloadID, chunkIdx, offsetInChunk, uint32(writeSize))
	}

	b.StopTimer()

	// Report throughput
	b.ReportMetric(float64(totalBytes)/(float64(b.Elapsed().Nanoseconds())/1e9)/1024/1024, "MB/s")
}

// ============================================================================
// S3 Benchmarks (Requires Localstack)
// ============================================================================

func BenchmarkUpload_S3(b *testing.B) {
	env := newS3EnvForBench(b)
	if env == nil {
		b.Skip("S3 environment not available")
	}
	defer env.cleanup()
	benchmarkUpload(b, env)
}

func BenchmarkDownload_S3(b *testing.B) {
	env := newS3EnvForBench(b)
	if env == nil {
		b.Skip("S3 environment not available")
	}
	defer env.cleanup()
	benchmarkDownload(b, env)
}

func BenchmarkFlush_S3(b *testing.B) {
	env := newS3EnvForBench(b)
	if env == nil {
		b.Skip("S3 environment not available")
	}
	defer env.cleanup()
	benchmarkFlush(b, env)
}

func BenchmarkConcurrentUpload_S3(b *testing.B) {
	env := newS3EnvForBench(b)
	if env == nil {
		b.Skip("S3 environment not available")
	}
	defer env.cleanup()
	benchmarkConcurrentUpload(b, env, 4)
}

func BenchmarkLargeFile_16MB_S3(b *testing.B) {
	env := newS3EnvForBench(b)
	if env == nil {
		b.Skip("S3 environment not available")
	}
	defer env.cleanup()
	benchmarkLargeFile(b, env, 16*1024*1024)
}

func BenchmarkLargeFile_64MB_S3(b *testing.B) {
	env := newS3EnvForBench(b)
	if env == nil {
		b.Skip("S3 environment not available")
	}
	defer env.cleanup()
	benchmarkLargeFile(b, env, 64*1024*1024)
}

func BenchmarkSequentialWrite_32KB_S3(b *testing.B) {
	env := newS3EnvForBench(b)
	if env == nil {
		b.Skip("S3 environment not available")
	}
	defer env.cleanup()
	benchmarkSequentialWrite(b, env, 32*1024)
}
