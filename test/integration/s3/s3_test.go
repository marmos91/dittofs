//go:build integration

package s3_test

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
	"github.com/marmos91/dittofs/pkg/content"
	s3store "github.com/marmos91/dittofs/pkg/content/store/s3"
	contenttesting "github.com/marmos91/dittofs/pkg/content/store/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
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

// TestS3ContentStore_Integration runs the complete content store test suite
// against a real S3-compatible service (Localstack via testcontainers).
func TestS3ContentStore_Integration(t *testing.T) {
	ctx := context.Background()

	// Start Localstack container
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	bucketName := "dittofs-test-bucket"
	helper.createBucket(t, bucketName)
	defer helper.cleanupBucket(bucketName)

	// Run standard test suite
	testCounter := 0
	suite := &contenttesting.StoreTestSuite{
		NewStore: func() content.ContentStore {
			testCounter++
			store, err := s3store.NewS3ContentStore(ctx, s3store.S3ContentStoreConfig{
				Client:        helper.client,
				Bucket:        bucketName,
				KeyPrefix:     fmt.Sprintf("test-%d/", testCounter),
				PartSize:      5 * 1024 * 1024,
				StatsCacheTTL: 1, // Effectively disables caching for tests
			})
			if err != nil {
				t.Fatalf("Failed to create S3 content store for test %d: %v", testCounter, err)
			}
			return store
		},
	}

	suite.Run(t)
}

// TestS3ContentStore_Multipart tests multipart upload functionality.
func TestS3ContentStore_Multipart(t *testing.T) {
	ctx := context.Background()

	// Start Localstack container
	helper := newLocalstackHelper(t)
	defer helper.cleanup()

	bucketName := "dittofs-multipart-test"
	helper.createBucket(t, bucketName)
	defer helper.cleanupBucket(bucketName)

	store, err := s3store.NewS3ContentStore(ctx, s3store.S3ContentStoreConfig{
		Client:   helper.client,
		Bucket:   bucketName,
		PartSize: 5 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("Failed to create S3 content store: %v", err)
	}

	t.Run("MultipartUpload", func(t *testing.T) {
		contentID := metadata.ContentID("multipart-test-content")

		// Begin multipart upload
		uploadID, err := store.BeginMultipartUpload(ctx, contentID)
		if err != nil {
			t.Fatalf("Failed to begin multipart upload: %v", err)
		}

		// Upload 3 parts (each 5MB)
		partSize := 5 * 1024 * 1024
		for i := 1; i <= 3; i++ {
			data := make([]byte, partSize)
			for j := range data {
				data[j] = byte(i) // Fill with part number
			}

			err = store.UploadPart(ctx, contentID, uploadID, i, data)
			if err != nil {
				t.Fatalf("Failed to upload part %d: %v", i, err)
			}
		}

		// Complete multipart upload
		err = store.CompleteMultipartUpload(ctx, contentID, uploadID, []int{1, 2, 3})
		if err != nil {
			t.Fatalf("Failed to complete multipart upload: %v", err)
		}

		// Verify content size
		size, err := store.GetContentSize(ctx, contentID)
		if err != nil {
			t.Fatalf("Failed to get content size: %v", err)
		}

		expectedSize := uint64(3 * partSize)
		if size != expectedSize {
			t.Errorf("Expected size %d, got %d", expectedSize, size)
		}

		// Verify content exists
		exists, err := store.ContentExists(ctx, contentID)
		if err != nil {
			t.Fatalf("Failed to check content exists: %v", err)
		}
		if !exists {
			t.Error("Content should exist after multipart upload")
		}
	})

	t.Run("AbortMultipartUpload", func(t *testing.T) {
		contentID := metadata.ContentID("abort-test-content")

		// Begin multipart upload
		uploadID, err := store.BeginMultipartUpload(ctx, contentID)
		if err != nil {
			t.Fatalf("Failed to begin multipart upload: %v", err)
		}

		// Upload one part
		data := make([]byte, 5*1024*1024)
		err = store.UploadPart(ctx, contentID, uploadID, 1, data)
		if err != nil {
			t.Fatalf("Failed to upload part: %v", err)
		}

		// Abort multipart upload
		err = store.AbortMultipartUpload(ctx, contentID, uploadID)
		if err != nil {
			t.Fatalf("Failed to abort multipart upload: %v", err)
		}

		// Verify content doesn't exist
		exists, err := store.ContentExists(ctx, contentID)
		if err != nil {
			t.Fatalf("Failed to check content exists: %v", err)
		}
		if exists {
			t.Error("Content should not exist after abort")
		}
	})
}
