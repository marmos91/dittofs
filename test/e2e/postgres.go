//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PostgresHelper manages PostgreSQL container for E2E tests
type PostgresHelper struct {
	T         *testing.T
	Container testcontainers.Container
	Host      string
	Port      int
	Database  string
	User      string
	Password  string
}

// PostgresConfig holds PostgreSQL connection configuration for tests
type PostgresConfig struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
}

// Shared PostgreSQL container for E2E tests (started once per test run)
var sharedPostgresHelper *PostgresHelper

// NewPostgresHelper creates a new PostgreSQL helper with a testcontainer
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
			fmt.Sscanf(p, "%d", &port)
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

	// Start PostgreSQL container using testcontainers
	req := testcontainers.ContainerRequest{
		Image:        "postgres:16-alpine",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_DB":       "dittofs_e2e",
			"POSTGRES_USER":     "dittofs_e2e",
			"POSTGRES_PASSWORD": "dittofs_e2e",
		},
		WaitingFor: wait.ForAll(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
			wait.ForListeningPort("5432/tcp"),
		),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}

	// Get connection details
	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx)
		t.Fatalf("failed to get container host: %v", err)
	}

	port, err := container.MappedPort(ctx, "5432")
	if err != nil {
		container.Terminate(ctx)
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

	// Store as shared helper for reuse
	sharedPostgresHelper = helper

	// NOTE: We do NOT register t.Cleanup() here because:
	// 1. When called from a subtest, cleanup runs after that subtest, not the parent
	// 2. This would terminate the container before other subtests can use it
	// 3. The Ryuk container (testcontainers garbage collector) will clean up
	//    containers automatically when the test process exits

	return helper
}

// GetConfig returns PostgreSQL configuration for creating a metadata store
func (ph *PostgresHelper) GetConfig() *PostgresConfig {
	return &PostgresConfig{
		Host:     ph.Host,
		Port:     ph.Port,
		Database: ph.Database,
		User:     ph.User,
		Password: ph.Password,
	}
}

// ConnectionString returns a PostgreSQL connection string
func (ph *PostgresHelper) ConnectionString() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		ph.User, ph.Password, ph.Host, ph.Port, ph.Database)
}

// Cleanup terminates the PostgreSQL container
func (ph *PostgresHelper) Cleanup() {
	if ph.Container != nil {
		ctx := context.Background()
		ph.Container.Terminate(ctx)
	}
}

// TruncateTables clears all data from the database tables for test isolation
// This should be called before each test when reusing containers
func (ph *PostgresHelper) TruncateTables() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connStr := ph.ConnectionString()
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		return fmt.Errorf("failed to connect for truncation: %w", err)
	}
	defer pool.Close()

	// Truncate all tables in correct order (respecting foreign keys)
	// Use CASCADE to handle foreign key constraints
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

// CheckPostgresAvailable checks if PostgreSQL is running and accessible
func CheckPostgresAvailable(t *testing.T) bool {
	t.Helper()

	// Check if external PostgreSQL is configured
	if host := os.Getenv("POSTGRES_HOST"); host != "" {
		port := 5432
		if p := os.Getenv("POSTGRES_PORT"); p != "" {
			fmt.Sscanf(p, "%d", &port)
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

	// Check if Docker is available for testcontainers
	// We can't easily check Docker without starting something,
	// so we assume Docker is available if we get this far
	return true
}

// SetupPostgresConfig configures a TestConfig for PostgreSQL usage
func SetupPostgresConfig(t *testing.T, config *TestConfig, helper *PostgresHelper) {
	t.Helper()

	// Truncate tables to ensure test isolation when reusing containers
	if err := helper.TruncateTables(); err != nil {
		t.Logf("Warning: failed to truncate tables (may be first run): %v", err)
	}

	// Set PostgreSQL connection config in the test config
	config.postgresConfig = helper.GetConfig()
}
