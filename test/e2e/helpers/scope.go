//go:build e2e

package helpers

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// TestScope provides per-test isolation through unique Postgres schemas and S3 prefixes.
// Each test should create its own scope to ensure isolation from other parallel tests.
type TestScope struct {
	t          *testing.T
	env        *TestEnvironment
	schemaName string
	s3Prefix   string
	db         *pgxpool.Pool
	cleanup    []func()
}

// invalidSchemaChars matches characters not allowed in Postgres schema names
var invalidSchemaChars = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// newScope creates a new TestScope with unique schema and S3 prefix.
// This is called from TestEnvironment.NewScope.
func newScope(t *testing.T, env *TestEnvironment) *TestScope {
	t.Helper()

	ctx := context.Background()

	// Generate unique identifiers
	shortUUID := uuid.New().String()[:8]
	sanitizedName := sanitizeTestName(t.Name())

	// Create unique schema name: test_<sanitized_test_name>_<uuid8>
	// Postgres schema names are limited to 63 chars, truncate if needed
	schemaName := fmt.Sprintf("test_%s_%s", sanitizedName, shortUUID)
	if len(schemaName) > 63 {
		schemaName = schemaName[:63]
	}

	// Create unique S3 prefix: test-<uuid8>/
	s3Prefix := fmt.Sprintf("test-%s/", shortUUID)

	scope := &TestScope{
		t:          t,
		env:        env,
		schemaName: schemaName,
		s3Prefix:   s3Prefix,
		cleanup:    make([]func(), 0),
	}

	// Create Postgres schema if Postgres is available
	if env.pgHelper != nil {
		scope.createSchema(ctx)
	}

	// Register cleanup via t.Cleanup for automatic cleanup on test completion
	t.Cleanup(func() {
		scope.doCleanup()
	})

	return scope
}

// sanitizeTestName converts a test name to a valid Postgres schema identifier.
// It replaces invalid characters with underscores and converts to lowercase.
func sanitizeTestName(name string) string {
	// Replace path separators and invalid chars with underscores
	sanitized := invalidSchemaChars.ReplaceAllString(name, "_")
	// Convert to lowercase for consistency
	sanitized = strings.ToLower(sanitized)
	// Remove consecutive underscores
	for strings.Contains(sanitized, "__") {
		sanitized = strings.ReplaceAll(sanitized, "__", "_")
	}
	// Trim leading/trailing underscores
	sanitized = strings.Trim(sanitized, "_")
	// Limit length to leave room for prefix and UUID
	if len(sanitized) > 40 {
		sanitized = sanitized[:40]
	}
	return sanitized
}

// createSchema creates the test-specific Postgres schema.
func (s *TestScope) createSchema(ctx context.Context) {
	s.t.Helper()

	// Connect to Postgres
	connStr := s.env.PostgresConnectionString()
	pool, err := pgxpool.New(ctx, connStr)
	require.NoError(s.t, err, "Failed to connect to Postgres for schema creation")

	// Create schema
	_, err = pool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", s.schemaName))
	require.NoError(s.t, err, "Failed to create Postgres schema %s", s.schemaName)

	pool.Close()

	// Create scoped connection with search_path set to the new schema
	scopedConnStr := fmt.Sprintf("%s&search_path=%s", connStr, s.schemaName)
	s.db, err = pgxpool.New(ctx, scopedConnStr)
	require.NoError(s.t, err, "Failed to create scoped Postgres connection")
}

// doCleanup performs all cleanup operations for the scope.
func (s *TestScope) doCleanup() {
	ctx := context.Background()

	// Run registered cleanup functions in reverse order
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		s.cleanup[i]()
	}

	// Close scoped database connection
	if s.db != nil {
		s.db.Close()
	}

	// Drop Postgres schema if it was created
	if s.env.pgHelper != nil {
		s.dropSchema(ctx)
	}

	// Clean up S3 objects with prefix
	if s.env.lsHelper != nil {
		s.cleanupS3Objects(ctx)
	}
}

// dropSchema drops the test-specific Postgres schema.
func (s *TestScope) dropSchema(ctx context.Context) {
	connStr := s.env.PostgresConnectionString()
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		s.t.Logf("Warning: Failed to connect to Postgres for schema cleanup: %v", err)
		return
	}
	defer pool.Close()

	_, err = pool.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", s.schemaName))
	if err != nil {
		s.t.Logf("Warning: Failed to drop Postgres schema %s: %v", s.schemaName, err)
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

// DB returns the scoped database connection pool.
// The connection has search_path set to the test's unique schema.
func (s *TestScope) DB() *pgxpool.Pool {
	return s.db
}

// SchemaName returns the unique Postgres schema name for this test.
func (s *TestScope) SchemaName() string {
	return s.schemaName
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
