package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Shared test container for all tests
var sharedTestContainer *testContainer

// TestMain sets up a shared PostgreSQL container for all tests
func TestMain(m *testing.M) {
	// Set up shared container
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "postgres:16-alpine",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_DB":       "dittofs_test",
			"POSTGRES_USER":     "dittofs_test",
			"POSTGRES_PASSWORD": "dittofs_test",
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
		fmt.Fprintf(os.Stderr, "failed to start postgres container: %v\n", err)
		os.Exit(1)
	}

	// Get connection details
	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		fmt.Fprintf(os.Stderr, "failed to get container host: %v\n", err)
		os.Exit(1)
	}

	port, err := container.MappedPort(ctx, "5432")
	if err != nil {
		_ = container.Terminate(ctx)
		fmt.Fprintf(os.Stderr, "failed to get container port: %v\n", err)
		os.Exit(1)
	}

	connStr := fmt.Sprintf("postgres://dittofs_test:dittofs_test@%s:%s/dittofs_test?sslmode=disable",
		host, port.Port())

	sharedTestContainer = &testContainer{
		container: container,
		connStr:   connStr,
	}

	// Run tests
	exitCode := m.Run()

	// Clean up
	if err := container.Terminate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to terminate container: %v\n", err)
	}

	os.Exit(exitCode)
}
