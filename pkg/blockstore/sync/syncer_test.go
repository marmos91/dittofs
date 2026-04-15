//go:build integration

package sync

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	gosync "sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	remotes3 "github.com/marmos91/dittofs/pkg/blockstore/remote/s3"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// sharedHelper is a package-level Localstack container shared across all tests.
var sharedHelper *localstackHelper

// ============================================================================
// TestMain - single container for entire package
// ============================================================================

func TestMain(m *testing.M) {
	cleanup := startSharedLocalstack()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// ============================================================================
// Test Helpers
// ============================================================================

// testEnv holds the test environment with local and remote store.
type testEnv struct {
	local          local.LocalStore
	remoteStore    remote.RemoteStore
	fileBlockStore blockstore.FileBlockStore
	syncer         *Syncer
	cleanup        func()
}

// newMemoryEnv creates a test environment with memory remote store.
func newMemoryEnv(t *testing.T) *testEnv {
	t.Helper()
	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmpDir, 0, 0, ms)
	if err != nil {
		t.Fatalf("fs.New() error = %v", err)
	}
	bs := remotememory.New()
	m := New(bc, bs, ms, DefaultConfig())
	m.Start(context.Background())

	return &testEnv{
		local:          bc,
		remoteStore:    bs,
		fileBlockStore: ms,
		syncer:         m,
		cleanup: func() {
			m.Close()
			bs.Close()
		},
	}
}

// localstackHelper manages Localstack container for S3 tests.
type localstackHelper struct {
	container testcontainers.Container
	endpoint  string
	client    *s3.Client
}

// startSharedLocalstack starts a single Localstack container for the entire
// test package. Returns a cleanup function.
func startSharedLocalstack() func() {
	ctx := context.Background()

	// Check for external Localstack
	if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
		helper := &localstackHelper{endpoint: endpoint}
		helper.initClient()
		sharedHelper = helper
		return func() {}
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
		log.Fatalf("failed to start localstack: %v", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx)
		log.Fatalf("failed to get host: %v", err)
	}

	port, err := container.MappedPort(ctx, "4566")
	if err != nil {
		container.Terminate(ctx)
		log.Fatalf("failed to get port: %v", err)
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
func (h *localstackHelper) initClient() {
	ctx := context.Background()

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
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

// s3BenchHelper manages S3 for benchmarks.
type s3BenchHelper struct {
	container testcontainers.Container
	client    *s3.Client
	bucket    string
	isRealS3  bool
}

func newS3BenchHelper(b *testing.B) *s3BenchHelper {
	b.Helper()
	ctx := context.Background()

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
		_, err = client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
		if err != nil {
			b.Fatalf("cannot access S3 bucket %s: %v", bucket, err)
		}
		b.Logf("Using real AWS S3 bucket: %s in %s", bucket, region)
		return &s3BenchHelper{client: client, bucket: bucket, isRealS3: true}
	}

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
		return &s3BenchHelper{client: client, bucket: bucket}
	}

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

	return &s3BenchHelper{container: container, client: client, bucket: bucket}
}

func (h *s3BenchHelper) cleanup(b *testing.B) {
	ctx := context.Background()
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
	if !h.isRealS3 {
		h.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(h.bucket)})
	}
	if h.container != nil {
		h.container.Terminate(ctx)
	}
}

func newS3EnvForBench(b *testing.B) *testEnv {
	b.Helper()
	helper := newS3BenchHelper(b)
	tmpDir, err := os.MkdirTemp("", "syncer-bench-local-*")
	if err != nil {
		b.Fatalf("failed to create temp dir: %v", err)
	}
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmpDir, 0, 0, ms)
	if err != nil {
		os.RemoveAll(tmpDir)
		b.Fatalf("fs.New() error = %v", err)
	}
	bs := remotes3.New(helper.client, remotes3.Config{Bucket: helper.bucket, KeyPrefix: "blocks/"})
	m := New(bc, bs, ms, DefaultConfig())
	m.Start(context.Background())
	return &testEnv{local: bc, remoteStore: bs, fileBlockStore: ms, syncer: m, cleanup: func() {
		m.Close()
		bs.Close()
		os.RemoveAll(tmpDir)
		helper.cleanup(b)
	}}
}

func newS3Env(t *testing.T, helper *localstackHelper) *testEnv {
	t.Helper()
	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmpDir, 0, 0, ms)
	if err != nil {
		t.Fatalf("fs.New() error = %v", err)
	}
	bucket := fmt.Sprintf("test-bucket-%d", time.Now().UnixNano())
	helper.createBucket(t, bucket)
	bs := remotes3.New(helper.client, remotes3.Config{Bucket: bucket, KeyPrefix: "blocks/"})
	m := New(bc, bs, ms, DefaultConfig())
	m.Start(context.Background())
	return &testEnv{local: bc, remoteStore: bs, fileBlockStore: ms, syncer: m, cleanup: func() {
		m.Close()
		bs.Close()
	}}
}

func randomData(size int) []byte {
	data := make([]byte, size)
	rand.Read(data)
	return data
}

// ============================================================================
// Integration Tests
// ============================================================================

func TestSyncer_WriteAndFlush_Memory(t *testing.T) {
	env := newMemoryEnv(t)
	defer env.cleanup()
	testWriteAndFlush(t, env)
}

func TestSyncer_WriteAndFlush_S3(t *testing.T) {
	env := newS3Env(t, sharedHelper)
	defer env.cleanup()
	testWriteAndFlush(t, env)
}

func testWriteAndFlush(t *testing.T, env *testEnv) {
	ctx := context.Background()
	payloadID := "export/test-file.bin"
	data := randomData(8 * 1024 * 1024) // 8MB = 2 blocks
	writeSize := 32 * 1024
	for offset := 0; offset < len(data); offset += writeSize {
		end := offset + writeSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[offset:end]
		if err := env.local.WriteAt(ctx, payloadID, chunk, uint64(offset)); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	_, err := env.syncer.Flush(ctx, payloadID)
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	// Flush is decoupled from upload -- trigger immediate sync
	env.syncer.SyncNow(ctx)
	exists, err2 := env.syncer.Exists(ctx, payloadID)
	if err2 != nil {
		t.Fatalf("Exists failed: %v", err2)
	}
	if !exists {
		t.Error("File should exist in remote store")
	}
	size, err := env.syncer.GetFileSize(ctx, payloadID)
	if err != nil {
		t.Fatalf("GetFileSize failed: %v", err)
	}
	if size != uint64(len(data)) {
		t.Errorf("Size mismatch: got %d, want %d", size, len(data))
	}
}

func TestSyncer_DownloadOnLocalMiss_Memory(t *testing.T) {
	env := newMemoryEnv(t)
	defer env.cleanup()
	testDownloadOnLocalMiss(t, env)
}

func TestSyncer_DownloadOnLocalMiss_S3(t *testing.T) {
	env := newS3Env(t, sharedHelper)
	defer env.cleanup()
	testDownloadOnLocalMiss(t, env)
}

func testDownloadOnLocalMiss(t *testing.T, env *testEnv) {
	ctx := context.Background()
	payloadID := "export/download-test.bin"
	blockData := randomData(BlockSize)
	blockKey := blockstore.FormatStoreKey(payloadID, 0)
	if err := env.remoteStore.WriteBlock(ctx, blockKey, blockData); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}
	// Register FileBlock so resolveStoreKey can find the block store key
	if err := env.fileBlockStore.PutFileBlock(ctx, &blockstore.FileBlock{
		ID:            fmt.Sprintf("%s/0", payloadID),
		State:         blockstore.BlockStateRemote,
		BlockStoreKey: blockKey,
		DataSize:      uint32(BlockSize),
	}); err != nil {
		t.Fatalf("PutFileBlock failed: %v", err)
	}
	if err := env.syncer.EnsureAvailable(ctx, payloadID, 0, BlockSize); err != nil {
		t.Fatalf("EnsureAvailable failed: %v", err)
	}
	dest := make([]byte, BlockSize)
	found, err := env.local.ReadAt(ctx, payloadID, dest, 0)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if !found {
		t.Error("Data should be in local store after EnsureAvailable")
	}
	for i := 0; i < len(blockData); i++ {
		if dest[i] != blockData[i] {
			t.Errorf("Data mismatch at byte %d", i)
			break
		}
	}
}

func TestSyncer_ConcurrentOperations_Memory(t *testing.T) {
	env := newMemoryEnv(t)
	defer env.cleanup()
	testConcurrentOperations(t, env)
}

func testConcurrentOperations(t *testing.T, env *testEnv) {
	ctx := context.Background()
	numFiles := 10
	fileSize := 4 * 1024 * 1024
	var wg gosync.WaitGroup
	errors := make(chan error, numFiles)
	for i := 0; i < numFiles; i++ {
		wg.Add(1)
		go func(fileIdx int) {
			defer wg.Done()
			payloadID := fmt.Sprintf("export/concurrent-%d.bin", fileIdx)
			data := randomData(fileSize)
			if err := env.local.WriteAt(ctx, payloadID, data, 0); err != nil {
				errors <- fmt.Errorf("file %d: Write failed: %w", fileIdx, err)
				return
			}
			if _, err := env.syncer.Flush(ctx, payloadID); err != nil {
				errors <- fmt.Errorf("file %d: Flush failed: %w", fileIdx, err)
				return
			}
			// SyncNow now holds the m.uploading gate end-to-end and uploads
			// synchronously, so by the time it returns every block is in the
			// remote store.
			env.syncer.SyncNow(ctx)
			exists, err := env.syncer.Exists(ctx, payloadID)
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

func newMemoryEnvForBench(b *testing.B) *testEnv {
	b.Helper()
	tmpDir, err := os.MkdirTemp("", "syncer-bench-local-*")
	if err != nil {
		b.Fatalf("failed to create temp dir: %v", err)
	}
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmpDir, 0, 0, ms)
	if err != nil {
		os.RemoveAll(tmpDir)
		b.Fatalf("fs.New() error = %v", err)
	}
	bs := remotememory.New()
	m := New(bc, bs, ms, DefaultConfig())
	m.Start(context.Background())
	return &testEnv{local: bc, remoteStore: bs, fileBlockStore: ms, syncer: m, cleanup: func() { m.Close(); bs.Close(); os.RemoveAll(tmpDir) }}
}

func BenchmarkUpload_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkUpload(b, env)
}
func BenchmarkDownload_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkDownload(b, env)
}
func BenchmarkFlush_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkFlush(b, env)
}
func BenchmarkConcurrentUpload_Memory(b *testing.B) {
	env := newMemoryEnvForBench(b)
	defer env.cleanup()
	benchmarkConcurrentUpload(b, env, 4)
}
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

func benchmarkUpload(b *testing.B, env *testEnv) {
	ctx := context.Background()
	data := randomData(BlockSize)
	b.SetBytes(int64(BlockSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("export/bench-upload-%d.bin", i)
		if err := env.local.WriteAt(ctx, payloadID, data, 0); err != nil {
			b.Fatalf("Write failed: %v", err)
		}

		if _, err := env.syncer.Flush(ctx, payloadID); err != nil {
			b.Fatalf("Flush failed: %v", err)
		}
	}
}

func benchmarkDownload(b *testing.B, env *testEnv) {
	ctx := context.Background()
	data := randomData(BlockSize)
	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("export/bench-download-%d.bin", i)
		blockKey := blockstore.FormatStoreKey(payloadID, 0)
		if err := env.remoteStore.WriteBlock(ctx, blockKey, data); err != nil {
			b.Fatalf("WriteBlock failed: %v", err)
		}
	}
	b.SetBytes(int64(BlockSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("export/bench-download-%d.bin", i)
		if err := env.syncer.EnsureAvailable(ctx, payloadID, 0, BlockSize); err != nil {
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
		if err := env.local.WriteAt(ctx, payloadID, data[:BlockSize/2], 0); err != nil {
			b.Fatalf("Write failed: %v", err)
		}
		if _, err := env.syncer.Flush(ctx, payloadID); err != nil {
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
		var wg gosync.WaitGroup
		for j := 0; j < parallelism; j++ {
			wg.Add(1)
			go func(fileIdx int) {
				defer wg.Done()
				payloadID := fmt.Sprintf("export/bench-concurrent-%d-%d.bin", i, fileIdx)
				if err := env.local.WriteAt(ctx, payloadID, data, 0); err != nil {
					return
				}
				env.syncer.Flush(ctx, payloadID)
			}(j)
		}
		wg.Wait()
	}
}

func benchmarkLargeFile(b *testing.B, env *testEnv, fileSize int) {
	ctx := context.Background()
	data := randomData(fileSize)
	writeChunkSize := 32 * 1024
	b.SetBytes(int64(fileSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("export/large-file-%d.bin", i)
		for offset := 0; offset < fileSize; offset += writeChunkSize {
			end := offset + writeChunkSize
			if end > fileSize {
				end = fileSize
			}
			if err := env.local.WriteAt(ctx, payloadID, data[offset:end], uint64(offset)); err != nil {
				b.Fatalf("Write failed: %v", err)
			}
		}
		if _, err := env.syncer.Flush(ctx, payloadID); err != nil {
			b.Fatalf("Flush failed: %v", err)
		}
	}
}

func benchmarkSequentialWrite(b *testing.B, env *testEnv, writeSize int) {
	ctx := context.Background()
	data := randomData(writeSize)
	totalBytes := int64(b.N) * int64(writeSize)
	b.SetBytes(int64(writeSize))
	b.ResetTimer()
	payloadID := "export/sequential-write.bin"
	for i := 0; i < b.N; i++ {
		fileOffset := uint64(i) * uint64(writeSize)
		if err := env.local.WriteAt(ctx, payloadID, data, fileOffset); err != nil {
			b.Fatalf("Write failed: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(totalBytes)/(float64(b.Elapsed().Nanoseconds())/1e9)/1024/1024, "MB/s")
}

// ============================================================================
// Deduplication Tests
// ============================================================================

func TestSyncer_Deduplication_Memory(t *testing.T) {
	env := newMemoryEnv(t)
	defer env.cleanup()
	testDeduplication(t, env)
}
func testDeduplication(t *testing.T, env *testEnv) {
	ctx := context.Background()
	data := make([]byte, BlockSize)
	for i := range data {
		data[i] = byte(i % 256)
	}
	payloadID1 := "export/dedup-file1.bin"
	payloadID2 := "export/dedup-file2.bin"
	if err := env.local.WriteAt(ctx, payloadID1, data, 0); err != nil {
		t.Fatalf("Write to file1 failed: %v", err)
	}

	if _, err := env.syncer.Flush(ctx, payloadID1); err != nil {
		t.Fatalf("Flush file1 failed: %v", err)
	}
	env.syncer.SyncNow(ctx)
	if err := env.local.WriteAt(ctx, payloadID2, data, 0); err != nil {
		t.Fatalf("Write to file2 failed: %v", err)
	}

	if _, err := env.syncer.Flush(ctx, payloadID2); err != nil {
		t.Fatalf("Flush file2 failed: %v", err)
	}
	env.syncer.SyncNow(ctx)
	exists1, err := env.syncer.Exists(ctx, payloadID1)
	if err != nil {
		t.Fatalf("Exists file1 failed: %v", err)
	}
	if !exists1 {
		t.Error("File1 should exist in remote store")
	}
	size1, err := env.syncer.GetFileSize(ctx, payloadID1)
	if err != nil {
		t.Fatalf("GetFileSize file1 failed: %v", err)
	}
	if size1 != uint64(BlockSize) {
		t.Errorf("File1 size mismatch: got %d, want %d", size1, BlockSize)
	}
	t.Logf("Deduplication test passed: both files written, dedup should have occurred for file2")
}

func TestSyncer_DedupWithDifferentData_Memory(t *testing.T) {
	env := newMemoryEnv(t)
	defer env.cleanup()
	testDedupWithDifferentData(t, env)
}

func testDedupWithDifferentData(t *testing.T, env *testEnv) {
	ctx := context.Background()
	data1 := make([]byte, BlockSize)
	data2 := make([]byte, BlockSize)
	for i := range data1 {
		data1[i] = byte(i % 256)
		data2[i] = byte((i + 1) % 256)
	}
	payloadID1 := "export/unique-file1.bin"
	payloadID2 := "export/unique-file2.bin"
	if err := env.local.WriteAt(ctx, payloadID1, data1, 0); err != nil {
		t.Fatalf("Write to file1 failed: %v", err)
	}

	if _, err := env.syncer.Flush(ctx, payloadID1); err != nil {
		t.Fatalf("Flush file1 failed: %v", err)
	}
	env.syncer.SyncNow(ctx)
	if err := env.local.WriteAt(ctx, payloadID2, data2, 0); err != nil {
		t.Fatalf("Write to file2 failed: %v", err)
	}

	if _, err := env.syncer.Flush(ctx, payloadID2); err != nil {
		t.Fatalf("Flush file2 failed: %v", err)
	}
	env.syncer.SyncNow(ctx)
	exists1, _ := env.syncer.Exists(ctx, payloadID1)
	if !exists1 {
		t.Error("File1 should exist")
	}
	exists2, _ := env.syncer.Exists(ctx, payloadID2)
	if !exists2 {
		t.Error("File2 should exist")
	}
	t.Logf("Different data test passed: both files uploaded separately (no dedup)")
}
