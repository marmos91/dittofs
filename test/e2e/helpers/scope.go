//go:build e2e

package helpers

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

// TestScope provides per-test isolation through unique S3 prefixes.
// Each test should create its own scope to ensure isolation from other parallel tests.
type TestScope struct {
	t        *testing.T
	env      *TestEnvironment
	s3Prefix string
	cleanup  []func()
}

// newScope creates a new TestScope with a unique S3 prefix.
// This is called from TestEnvironment.NewScope.
func newScope(t *testing.T, env *TestEnvironment) *TestScope {
	t.Helper()

	shortUUID := uuid.New().String()[:8]

	// Create unique S3 prefix: test-<uuid8>/
	s3Prefix := fmt.Sprintf("test-%s/", shortUUID)

	scope := &TestScope{
		t:        t,
		env:      env,
		s3Prefix: s3Prefix,
		cleanup:  make([]func(), 0),
	}

	// Register cleanup via t.Cleanup for automatic cleanup on test completion
	t.Cleanup(func() {
		scope.doCleanup()
	})

	return scope
}

// doCleanup performs all cleanup operations for the scope.
func (s *TestScope) doCleanup() {
	ctx := context.Background()

	// Run registered cleanup functions in reverse order
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		s.cleanup[i]()
	}

	// Clean up S3 objects with prefix
	if s.env.lsHelper != nil {
		s.cleanupS3Objects(ctx)
	}
}

// cleanupS3Objects deletes all S3 objects with the scope's prefix.
func (s *TestScope) cleanupS3Objects(ctx context.Context) {
	client := s.env.S3Client()
	if client == nil {
		return
	}

	// List all buckets and clean objects with our prefix in each
	listBucketsResp, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		s.t.Logf("Warning: Failed to list S3 buckets for cleanup: %v", err)
		return
	}

	for _, bucket := range listBucketsResp.Buckets {
		s.cleanupBucketPrefix(ctx, client, *bucket.Name)
	}
}

// cleanupBucketPrefix deletes all objects with the scope's prefix from a bucket.
func (s *TestScope) cleanupBucketPrefix(ctx context.Context, client *s3.Client, bucketName string) {
	// List objects with our prefix
	listResp, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
		Prefix: aws.String(s.s3Prefix),
	})
	if err != nil {
		// Bucket might not exist or have no objects with this prefix
		return
	}

	// Delete each object
	for _, obj := range listResp.Contents {
		_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucketName),
			Key:    obj.Key,
		})
	}
}

// S3Prefix returns the unique S3 prefix for this test.
func (s *TestScope) S3Prefix() string {
	return s.s3Prefix
}

// S3Client returns the S3 client from the environment.
func (s *TestScope) S3Client() *s3.Client {
	return s.env.S3Client()
}

// Env returns the parent TestEnvironment.
func (s *TestScope) Env() *TestEnvironment {
	return s.env
}

// RegisterCleanup registers an additional cleanup function to be called
// when the scope is cleaned up. Functions are called in reverse order.
func (s *TestScope) RegisterCleanup(fn func()) {
	s.cleanup = append(s.cleanup, fn)
}

// T returns the testing.T for the scope.
func (s *TestScope) T() *testing.T {
	return s.t
}

// Context returns a context for the scope.
func (s *TestScope) Context() context.Context {
	return s.env.Context()
}
