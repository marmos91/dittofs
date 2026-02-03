//go:build e2e

package framework

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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PostgresConfig holds PostgreSQL connection configuration for tests.
type PostgresConfig struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
}

// ConnectionString returns a PostgreSQL connection string.
func (pc *PostgresConfig) ConnectionString() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		pc.User, pc.Password, pc.Host, pc.Port, pc.Database)
}

// LocalstackHelper manages Localstack S3 integration for tests.
type LocalstackHelper struct {
	T         *testing.T
	Container testcontainers.Container
	Endpoint  string
	Client    *s3.Client
	Buckets   []string
}

// Shared Localstack container for E2E tests (started once per test run)
var sharedLocalstackHelper *LocalstackHelper

// NewLocalstackHelper creates or returns a shared Localstack helper.
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
	// Use a longer timeout (3 minutes) because Localstack can be slow to start,
	// especially on first run when images need to be pulled.
	// Note: Localstack 3.0+ defaults to HTTPS, but we can use GATEWAY_LISTEN to force HTTP.
	req := testcontainers.ContainerRequest{
		Image:        "localstack/localstack:3.0",
		ExposedPorts: []string{"4566/tcp"},
		Env: map[string]string{
			"SERVICES":              "s3",
			"EAGER_SERVICE_LOADING": "1",
			"GATEWAY_LISTEN":        "0.0.0.0:4566", // Force HTTP mode (no TLS)
		},
		WaitingFor: wait.ForAll(
			wait.ForListeningPort("4566/tcp"),
			wait.ForHTTP("/_localstack/health").
				WithPort("4566/tcp"),
		).WithDeadline(3 * time.Minute),
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

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	helper := &LocalstackHelper{
		T:         t,
		Container: container,
		Endpoint:  endpoint,
		Buckets:   make([]string, 0),
	}

	helper.createClient()
	sharedLocalstackHelper = helper

	return helper
}

// createClient creates an S3 client configured for Localstack.
func (lh *LocalstackHelper) createClient() {
	lh.T.Helper()

	ctx := context.Background()

	cfg, err := awsConfig.LoadDefaultConfig(ctx,
		awsConfig.WithRegion("us-east-1"),
		awsConfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			"test", "test", "",
		)),
	)
	if err != nil {
		lh.T.Fatalf("Failed to load AWS config: %v", err)
	}

	lh.Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = &lh.Endpoint
		o.UsePathStyle = true
	})
}

// CreateBucket creates a new S3 bucket and registers it for cleanup.
func (lh *LocalstackHelper) CreateBucket(ctx context.Context, bucketName string) error {
	lh.T.Helper()

	_, err := lh.Client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return fmt.Errorf("failed to create bucket %s: %w", bucketName, err)
	}

	lh.Buckets = append(lh.Buckets, bucketName)
	return nil
}

// CleanupBucket removes a bucket and all its contents if it exists.
func (lh *LocalstackHelper) CleanupBucket(ctx context.Context, bucketName string) {
	listResp, err := lh.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return // Bucket doesn't exist
	}

	if listResp != nil {
		for _, obj := range listResp.Contents {
			_, _ = lh.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucketName),
				Key:    obj.Key,
			})
		}
	}

	_, _ = lh.Client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucketName),
	})
}

// Cleanup removes all created buckets and their contents.
func (lh *LocalstackHelper) Cleanup() {
	lh.T.Helper()
	ctx := context.Background()
	for _, bucketName := range lh.Buckets {
		lh.CleanupBucket(ctx, bucketName)
	}
}

// CheckLocalstackAvailable checks if Localstack is available.
func CheckLocalstackAvailable(t *testing.T) bool {
	t.Helper()

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
	return true
}

// PostgresHelper manages PostgreSQL container for E2E tests.
type PostgresHelper struct {
	T         *testing.T
	Container testcontainers.Container
	Host      string
	Port      int
	Database  string
	User      string
	Password  string
}

// Shared PostgreSQL container for E2E tests (started once per test run)
var sharedPostgresHelper *PostgresHelper

// NewPostgresHelper creates or returns a shared PostgreSQL helper.
func NewPostgresHelper(t *testing.T) *PostgresHelper {
	t.Helper()

	// Reuse shared container if available
	if sharedPostgresHelper != nil {
		return sharedPostgresHelper
	}

	ctx := context.Background()

	// Check if external PostgreSQL is configured via environment
	if host := os.Getenv("POSTGRES_HOST"); host != "" {
		port := 5432
		if p := os.Getenv("POSTGRES_PORT"); p != "" {
			_, _ = fmt.Sscanf(p, "%d", &port)
		}
		database := os.Getenv("POSTGRES_DATABASE")
		if database == "" {
			database = "dittofs_e2e"
		}
		user := os.Getenv("POSTGRES_USER")
		if user == "" {
			user = "dittofs"
		}
		password := os.Getenv("POSTGRES_PASSWORD")
		if password == "" {
			password = "dittofs"
		}

		helper := &PostgresHelper{
			T:        t,
			Host:     host,
			Port:     port,
			Database: database,
			User:     user,
			Password: password,
		}
		sharedPostgresHelper = helper
		return helper
	}

	// Start PostgreSQL container using testcontainers postgres module
	// Use a custom wait strategy with a longer timeout (5 minutes) because Docker can be slow
	// on some systems, especially on first run when images need to be pulled.
	// PostgreSQL outputs "database system is ready" twice during startup (once during
	// bootstrap, once when fully ready), so we wait for 2 occurrences.
	container, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("dittofs_e2e"),
		postgres.WithUsername("dittofs_e2e"),
		postgres.WithPassword("dittofs_e2e"),
		testcontainers.WithWaitStrategyAndDeadline(5*time.Minute,
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2),
			wait.ForListeningPort("5432/tcp"),
		),
	)
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("failed to get container host: %v", err)
	}

	port, err := container.MappedPort(ctx, "5432")
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("failed to get container port: %v", err)
	}

	helper := &PostgresHelper{
		T:         t,
		Container: container,
		Host:      host,
		Port:      port.Int(),
		Database:  "dittofs_e2e",
		User:      "dittofs_e2e",
		Password:  "dittofs_e2e",
	}

	sharedPostgresHelper = helper
	return helper
}

// GetConfig returns PostgreSQL configuration for creating a metadata store.
func (ph *PostgresHelper) GetConfig() *PostgresConfig {
	return &PostgresConfig{
		Host:     ph.Host,
		Port:     ph.Port,
		Database: ph.Database,
		User:     ph.User,
		Password: ph.Password,
	}
}

// ConnectionString returns a PostgreSQL connection string.
func (ph *PostgresHelper) ConnectionString() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		ph.User, ph.Password, ph.Host, ph.Port, ph.Database)
}

// TruncateTables clears all data from the database tables for test isolation.
func (ph *PostgresHelper) TruncateTables() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connStr := ph.ConnectionString()
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		return fmt.Errorf("failed to connect for truncation: %w", err)
	}
	defer pool.Close()

	_, err = pool.Exec(ctx, `
		TRUNCATE TABLE
			parent_child_map,
			link_counts,
			files,
			shares
		CASCADE
	`)
	if err != nil {
		// Tables might not exist yet (first test run), that's ok
		return nil
	}

	return nil
}

// Cleanup terminates the PostgreSQL container.
func (ph *PostgresHelper) Cleanup() {
	if ph.Container != nil {
		ctx := context.Background()
		_ = ph.Container.Terminate(ctx)
	}
}

// CheckPostgresAvailable checks if PostgreSQL is running and accessible.
func CheckPostgresAvailable(t *testing.T) bool {
	t.Helper()

	if host := os.Getenv("POSTGRES_HOST"); host != "" {
		port := 5432
		if p := os.Getenv("POSTGRES_PORT"); p != "" {
			_, _ = fmt.Sscanf(p, "%d", &port)
		}
		database := os.Getenv("POSTGRES_DATABASE")
		if database == "" {
			database = "dittofs_e2e"
		}
		user := os.Getenv("POSTGRES_USER")
		if user == "" {
			user = "dittofs"
		}
		password := os.Getenv("POSTGRES_PASSWORD")
		if password == "" {
			password = "dittofs"
		}

		connStr := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
			user, password, host, port, database)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		pool, err := pgxpool.New(ctx, connStr)
		if err != nil {
			return false
		}
		defer pool.Close()

		return pool.Ping(ctx) == nil
	}

	// With testcontainers, we can always start the container on demand
	return true
}

// CleanupSharedContainers terminates all shared test containers.
// This should be called from TestMain after all tests complete.
func CleanupSharedContainers() {
	ctx := context.Background()

	// Cleanup PostgreSQL container
	if sharedPostgresHelper != nil && sharedPostgresHelper.Container != nil {
		_ = sharedPostgresHelper.Container.Terminate(ctx)
		sharedPostgresHelper = nil
	}

	// Cleanup Localstack container
	if sharedLocalstackHelper != nil && sharedLocalstackHelper.Container != nil {
		_ = sharedLocalstackHelper.Container.Terminate(ctx)
		sharedLocalstackHelper = nil
	}
}
