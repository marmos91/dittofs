//go:build e2e

package e2e

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
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// LocalstackHelper manages Localstack S3 integration for tests
type LocalstackHelper struct {
	T         *testing.T
	Container testcontainers.Container
	Endpoint  string
	Client    *s3.Client
	Buckets   []string
}

// Shared Localstack container for E2E tests (started once per test run)
var sharedLocalstackHelper *LocalstackHelper

// NewLocalstackHelper creates a new Localstack helper with a testcontainer
func NewLocalstackHelper(t *testing.T) *LocalstackHelper {
	t.Helper()

	// Reuse shared container if available
	if sharedLocalstackHelper != nil {
		return sharedLocalstackHelper
	}

	ctx := context.Background()

	// Check if external Localstack is configured via environment
	if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
		helper := &LocalstackHelper{
			T:        t,
			Endpoint: endpoint,
			Buckets:  make([]string, 0),
		}
		helper.createClient()
		sharedLocalstackHelper = helper
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

	// Get connection details
	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx)
		t.Fatalf("failed to get container host: %v", err)
	}

	port, err := container.MappedPort(ctx, "4566")
	if err != nil {
		container.Terminate(ctx)
		t.Fatalf("failed to get container port: %v", err)
	}

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	helper := &LocalstackHelper{
		T:         t,
		Container: container,
		Endpoint:  endpoint,
		Buckets:   make([]string, 0),
	}

	// Create S3 client
	helper.createClient()

	// Store as shared helper for reuse
	sharedLocalstackHelper = helper

	// NOTE: We do NOT register t.Cleanup() here because:
	// 1. When called from a subtest, cleanup runs after that subtest, not the parent
	// 2. This would terminate the container before other subtests can use it
	// 3. The Ryuk container (testcontainers garbage collector) will clean up
	//    containers automatically when the test process exits

	return helper
}

// createClient creates an S3 client configured for Localstack
func (lh *LocalstackHelper) createClient() {
	lh.T.Helper()

	ctx := context.Background()

	cfg, err := awsConfig.LoadDefaultConfig(ctx,
		awsConfig.WithRegion("us-east-1"),
		awsConfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			"test", // AccessKeyID
			"test", // SecretAccessKey
			"",     // SessionToken
		)),
	)
	if err != nil {
		lh.T.Fatalf("Failed to load AWS config: %v", err)
	}

	// Create S3 client with path-style URLs and custom endpoint (required for Localstack)
	lh.Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = &lh.Endpoint
		o.UsePathStyle = true
	})
}

// CreateBucket creates a new S3 bucket and registers it for cleanup
func (lh *LocalstackHelper) CreateBucket(ctx context.Context, bucketName string) error {
	lh.T.Helper()

	_, err := lh.Client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return fmt.Errorf("failed to create bucket %s: %w", bucketName, err)
	}

	// Register for cleanup
	lh.Buckets = append(lh.Buckets, bucketName)

	return nil
}

// Cleanup removes all created buckets and their contents
func (lh *LocalstackHelper) Cleanup() {
	lh.T.Helper()

	ctx := context.Background()

	for _, bucketName := range lh.Buckets {
		// List and delete all objects first
		listResp, err := lh.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucketName),
		})
		if err == nil && listResp != nil {
			for _, obj := range listResp.Contents {
				_, _ = lh.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket: aws.String(bucketName),
					Key:    obj.Key,
				})
			}
		}

		// Delete bucket
		_, _ = lh.Client.DeleteBucket(ctx, &s3.DeleteBucketInput{
			Bucket: aws.String(bucketName),
		})
	}
}

// SetupS3Config configures a TestConfig for S3 usage with Localstack
func SetupS3Config(t *testing.T, config *TestConfig, helper *LocalstackHelper) {
	t.Helper()

	ctx := context.Background()

	// Set S3 client
	config.s3Client = helper.Client

	// Create bucket for this config
	bucketName := fmt.Sprintf("dittofs-test-%s", config.Name)

	// Clean up existing bucket if it exists (for test isolation when reusing containers)
	helper.cleanupBucket(ctx, bucketName)

	if err := helper.CreateBucket(ctx, bucketName); err != nil {
		t.Fatalf("Failed to create S3 bucket: %v", err)
	}

	config.s3Bucket = bucketName
}

// cleanupBucket removes a bucket and all its contents if it exists
func (lh *LocalstackHelper) cleanupBucket(ctx context.Context, bucketName string) {
	// List and delete all objects first
	listResp, err := lh.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		// Bucket doesn't exist, nothing to clean
		return
	}

	if listResp != nil {
		for _, obj := range listResp.Contents {
			_, _ = lh.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucketName),
				Key:    obj.Key,
			})
		}
	}

	// Delete bucket
	_, _ = lh.Client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucketName),
	})
}

// CheckLocalstackAvailable checks if Localstack is available
// With testcontainers, this always returns true since we can start the container on demand
// If LOCALSTACK_ENDPOINT is set, it checks if the external service is accessible
func CheckLocalstackAvailable(t *testing.T) bool {
	t.Helper()

	// If external Localstack is configured, check if it's accessible
	if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
		helper := &LocalstackHelper{
			T:        t,
			Endpoint: endpoint,
			Buckets:  make([]string, 0),
		}
		helper.createClient()

		ctx := context.Background()
		_, err := helper.Client.ListBuckets(ctx, &s3.ListBucketsInput{})
		return err == nil
	}

	// With testcontainers, we can always start the container on demand
	// Check if Docker is available (testcontainers requirement)
	return true
}
