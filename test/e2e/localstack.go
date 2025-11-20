package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// LocalstackHelper manages Localstack S3 integration for tests
type LocalstackHelper struct {
	T        *testing.T
	Endpoint string
	Client   *s3.Client
	Buckets  []string
}

// NewLocalstackHelper creates a new Localstack helper
func NewLocalstackHelper(t *testing.T) *LocalstackHelper {
	t.Helper()

	// Get Localstack endpoint from environment or use default
	endpoint := os.Getenv("LOCALSTACK_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:4566"
	}

	helper := &LocalstackHelper{
		T:        t,
		Endpoint: endpoint,
		Buckets:  make([]string, 0),
	}

	// Create S3 client
	helper.createClient()

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
	if err := helper.CreateBucket(ctx, bucketName); err != nil {
		t.Fatalf("Failed to create S3 bucket: %v", err)
	}

	config.s3Bucket = bucketName
}

// CheckLocalstackAvailable checks if Localstack is running and accessible
func CheckLocalstackAvailable(t *testing.T) bool {
	t.Helper()

	helper := NewLocalstackHelper(t)
	ctx := context.Background()

	// Try to list buckets as a health check
	_, err := helper.Client.ListBuckets(ctx, &s3.ListBucketsInput{})
	return err == nil
}
