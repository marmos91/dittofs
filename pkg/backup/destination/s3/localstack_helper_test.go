//go:build integration

package s3

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// localstackHelper owns the shared Localstack container and an S3 client
// configured to speak to it. Exactly one instance is created per test
// binary invocation (see TestMain) — per-test containers are forbidden by
// MEMORY.md: they cause TestCollectGarbage_S3-style flakes (exit 245 from
// Docker contention).
type localstackHelper struct {
	endpoint  string
	container testcontainers.Container
	client    *awss3.Client
}

// sharedHelper is the package-level singleton. Integration tests reach
// into it via sharedHelper.createBucket / sharedHelper.client / ...
var sharedHelper *localstackHelper

// TestMain manages the Localstack container lifetime for every integration
// test in this package. If the LOCALSTACK_ENDPOINT env var is set, we
// reuse an externally-running Localstack and skip container management
// entirely — useful on CI runners where Docker-in-Docker is unavailable.
func TestMain(m *testing.M) {
	cleanup := startSharedLocalstack()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// startSharedLocalstack starts (or reuses) Localstack and returns a
// cleanup callback. Any failure is fatal — there is no degraded mode for
// integration tests.
func startSharedLocalstack() func() {
	ctx := context.Background()

	if endpoint := os.Getenv("LOCALSTACK_ENDPOINT"); endpoint != "" {
		helper := &localstackHelper{endpoint: endpoint}
		if err := helper.initClient(); err != nil {
			log.Fatalf("init external Localstack client: %v", err)
		}
		sharedHelper = helper
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

	helper := &localstackHelper{endpoint: endpoint, container: c}
	if err := helper.initClient(); err != nil {
		_ = c.Terminate(ctx)
		log.Fatalf("init Localstack client: %v", err)
	}
	sharedHelper = helper
	return func() { _ = c.Terminate(context.Background()) }
}

// initClient builds a path-style S3 client pointing at the helper's
// endpoint with dummy credentials (Localstack accepts anything).
func (h *localstackHelper) initClient() error {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", ""),
		),
	)
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}
	h.client = awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(h.endpoint)
		o.UsePathStyle = true
	})
	return nil
}

// createBucket creates bucket in Localstack. Fatals on error — the test
// cannot usefully continue with a missing bucket.
func (h *localstackHelper) createBucket(t *testing.T, name string) {
	t.Helper()
	_, err := h.client.CreateBucket(context.Background(), &awss3.CreateBucketInput{
		Bucket: aws.String(name),
	})
	if err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
}

// deleteBucket drains and removes bucket. Best-effort: cleanup errors are
// swallowed so a failed test still reports its real cause.
func (h *localstackHelper) deleteBucket(t *testing.T, name string) {
	t.Helper()
	out, err := h.client.ListObjectsV2(context.Background(), &awss3.ListObjectsV2Input{
		Bucket: aws.String(name),
	})
	if err == nil {
		for _, o := range out.Contents {
			_, _ = h.client.DeleteObject(context.Background(), &awss3.DeleteObjectInput{
				Bucket: aws.String(name),
				Key:    o.Key,
			})
		}
	}
	// Also abort any in-flight multipart uploads so DeleteBucket succeeds.
	mpu, err := h.client.ListMultipartUploads(context.Background(), &awss3.ListMultipartUploadsInput{
		Bucket: aws.String(name),
	})
	if err == nil {
		for _, u := range mpu.Uploads {
			_, _ = h.client.AbortMultipartUpload(context.Background(), &awss3.AbortMultipartUploadInput{
				Bucket:   aws.String(name),
				Key:      u.Key,
				UploadId: u.UploadId,
			})
		}
	}
	_, _ = h.client.DeleteBucket(context.Background(), &awss3.DeleteBucketInput{
		Bucket: aws.String(name),
	})
}
