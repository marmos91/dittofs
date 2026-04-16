//go:build integration

// Applies the cross-driver conformance suite to the S3 destination driver
// through a shared Localstack container. Run with:
//
//	go test -tags=integration ./pkg/backup/destination/destinationtest/... -count=1
//
// MEMORY.md forbids per-test containers (TestCollectGarbage_S3-style flake
// from Docker contention): this file owns exactly one Localstack lifetime
// via TestMain, and every subtest created by destinationtest.Run creates
// and tears down an isolated bucket.
//
// Set LOCALSTACK_ENDPOINT to reuse an externally-running Localstack (skips
// container management). Cross-package Localstack helpers cannot be shared
// via TestMain, so this file carries a minimal copy of the startup logic
// from pkg/backup/destination/s3/localstack_helper_test.go.
package destinationtest_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/destination/destinationtest"
	dests3 "github.com/marmos91/dittofs/pkg/backup/destination/s3"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// conformanceLocalstack is the package-level singleton for integration
// tests in this file. TestMain fills it once; every subtest uses the
// endpoint + client to create isolated buckets.
var conformanceLocalstack struct {
	endpoint  string
	client    *awss3.Client
	container testcontainers.Container
}

// TestMain manages the Localstack container lifetime for every
// integration test in this file. The shared container is the MEMORY.md
// canonical pattern — per-test containers are forbidden.
func TestMain(m *testing.M) {
	cleanup := startLocalstackForConformance()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// startLocalstackForConformance starts (or reuses) Localstack and returns
// a cleanup callback. Any startup failure is fatal — there is no degraded
// mode for integration tests.
func startLocalstackForConformance() func() {
	ctx := context.Background()
	if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
		conformanceLocalstack.endpoint = endpoint
		conformanceLocalstack.client = initS3Client(endpoint)
		return func() {}
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
				WithStartupTimeout(90*time.Second),
		),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		log.Fatalf("localstack start: %v", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		log.Fatalf("localstack host: %v", err)
	}
	port, err := c.MappedPort(ctx, "4566")
	if err != nil {
		_ = c.Terminate(ctx)
		log.Fatalf("localstack port: %v", err)
	}
	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())
	conformanceLocalstack.endpoint = endpoint
	conformanceLocalstack.container = c
	conformanceLocalstack.client = initS3Client(endpoint)
	return func() { _ = c.Terminate(context.Background()) }
}

// initS3Client builds a path-style S3 client pointing at the Localstack
// endpoint. Credentials are the Localstack dummies.
func initS3Client(endpoint string) *awss3.Client {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", ""),
		),
	)
	if err != nil {
		log.Fatalf("aws cfg: %v", err)
	}
	return awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
}

// uniqueBucketName builds a Localstack-safe, <=63-character bucket name.
// Bucket names must be lowercase, 3..63 chars, no underscores.
func uniqueBucketName() string {
	return "conf-" + strings.ToLower(ulid.Make().String()[:16])
}

// drainAndDeleteBucket empties bucket then deletes it. Best-effort:
// individual failures are swallowed so a failed test reports its real
// cause rather than a follow-on cleanup error.
func drainAndDeleteBucket(t *testing.T, client *awss3.Client, bucket string) {
	t.Helper()
	// Delete every object.
	var token *string
	for {
		out, err := client.ListObjectsV2(context.Background(), &awss3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			ContinuationToken: token,
		})
		if err != nil {
			break
		}
		for _, o := range out.Contents {
			_, _ = client.DeleteObject(context.Background(), &awss3.DeleteObjectInput{
				Bucket: aws.String(bucket),
				Key:    o.Key,
			})
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}
	// Abort any stale multipart uploads that would otherwise block bucket
	// deletion.
	if mpu, err := client.ListMultipartUploads(context.Background(), &awss3.ListMultipartUploadsInput{
		Bucket: aws.String(bucket),
	}); err == nil {
		for _, u := range mpu.Uploads {
			_, _ = client.AbortMultipartUpload(context.Background(), &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(bucket),
				Key:      u.Key,
				UploadId: u.UploadId,
			})
		}
	}
	_, _ = client.DeleteBucket(context.Background(), &awss3.DeleteBucketInput{
		Bucket: aws.String(bucket),
	})
}

// TestConformance_S3Driver runs the destinationtest.Run suite against the
// S3 driver. The factory creates a fresh bucket per subtest so state from
// one case cannot bleed into another — cleanup drains the bucket in
// t.Cleanup.
func TestConformance_S3Driver(t *testing.T) {
	destinationtest.Run(t, func(t *testing.T, keyRef string) destination.Destination {
		bucket := uniqueBucketName()
		_, err := conformanceLocalstack.client.CreateBucket(context.Background(), &awss3.CreateBucketInput{
			Bucket: aws.String(bucket),
		})
		require.NoError(t, err)
		t.Cleanup(func() {
			drainAndDeleteBucket(t, conformanceLocalstack.client, bucket)
		})

		repo := &models.BackupRepo{
			ID:                "s3-conformance",
			Kind:              models.BackupRepoKindS3,
			EncryptionEnabled: keyRef != "",
			EncryptionKeyRef:  keyRef,
		}
		require.NoError(t, repo.SetConfig(map[string]any{
			"bucket":           bucket,
			"region":           "us-east-1",
			"endpoint":         conformanceLocalstack.endpoint,
			"access_key":       "test",
			"secret_key":       "test",
			"force_path_style": true,
			"max_retries":      3,
			"grace_window":     "24h",
		}))
		s, err := dests3.New(context.Background(), repo)
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
