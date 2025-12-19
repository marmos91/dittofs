# DittoFS Integration Tests

This directory contains integration tests for DittoFS that require external services like databases, cloud storage, etc.

## Overview

Integration tests verify that DittoFS works correctly with real external dependencies:

- **S3 Integration Tests** (`s3/`) - Test S3 content store against Localstack
- **Badger Integration Tests** (`badger/`) - Test Badger metadata store with persistent storage

## Quick Start

### Prerequisites

- Docker installed and running
- Go 1.21 or later

### Running All Integration Tests

```bash
go test -tags=integration -v ./test/integration/...
```

Tests use [testcontainers-go](https://golang.testcontainers.org/) to automatically start and manage Docker containers. No manual Docker setup is required.

### Running Specific Tests

```bash
# Run only S3 integration tests
go test -tags=integration -v ./test/integration/s3/...

# Run only Badger integration tests
go test -tags=integration -v ./test/integration/badger/...
```

## Test Structure

```
test/integration/
├── README.md                    # This file
├── badger/                      # Badger integration tests
│   └── badger_test.go           # Badger metadata store tests
└── s3/                          # S3 integration tests
    └── s3_test.go               # S3 content store tests
```

## How It Works

### Testcontainers

Integration tests use testcontainers-go to:

1. **Automatically start containers** - No manual `docker-compose up` needed
2. **Manage container lifecycle** - Containers are started before tests and cleaned up after
3. **Dynamic port mapping** - Tests use dynamically allocated ports to avoid conflicts
4. **Health checks** - Tests wait for services to be healthy before running

### S3 Tests (Localstack)

The S3 integration tests automatically start a Localstack container:

```go
// Tests create their own Localstack container
helper := newLocalstackHelper(t)
defer helper.cleanup()
```

You can also use an external Localstack instance by setting:
```bash
LOCALSTACK_ENDPOINT=http://localhost:4566 go test -tags=integration ./test/integration/s3/...
```

### Badger Tests

Badger tests use a temporary directory and don't require Docker:

```bash
go test -tags=integration -v ./test/integration/badger/...
```

## Writing Integration Tests

### Best Practices

1. **Use build tags**: All integration tests must have `//go:build integration`
2. **Use testcontainers**: Let tests manage their own containers
3. **Clean up resources**: Use `defer` for cleanup
4. **Use unique names**: Generate unique bucket/database names per test
5. **Set timeouts**: Use reasonable timeouts to avoid hanging tests

### Example Integration Test

```go
//go:build integration

package myservice_test

import (
    "context"
    "testing"
    "github.com/testcontainers/testcontainers-go"
)

func TestMyServiceIntegration(t *testing.T) {
    ctx := context.Background()

    // Start container using testcontainers
    container, err := testcontainers.GenericContainer(ctx, ...)
    if err != nil {
        t.Fatalf("failed to start container: %v", err)
    }
    defer container.Terminate(ctx)

    // Run tests against the container
    // ...
}
```

## CI/CD Integration

### GitHub Actions

```yaml
name: Integration Tests

on:
  push:
    branches: [main, develop]
  pull_request:

jobs:
  integration:
    runs-on: ubuntu-latest

    steps:
      - uses: actions/checkout@v4

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.21'

      - name: Run Integration Tests
        run: go test -tags=integration -v -timeout=10m ./test/integration/...
```

## Environment Variables

Integration tests respect these environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `LOCALSTACK_ENDPOINT` | Use external Localstack instead of testcontainers | (testcontainers) |
| `AWS_ACCESS_KEY_ID` | AWS credentials for S3 tests | `test` |
| `AWS_SECRET_ACCESS_KEY` | AWS secret key | `test` |
| `AWS_REGION` | AWS region | `us-east-1` |

## Troubleshooting

### Docker not running

```
Error: Cannot connect to the Docker daemon
```

Start Docker Desktop or the Docker daemon.

### Port conflicts

Testcontainers uses dynamic port allocation, so port conflicts are rare. If you see connection errors, check that Docker is running and has available resources.

### Tests timeout

Increase the timeout:
```bash
go test -tags=integration -v -timeout=15m ./test/integration/...
```

### Container startup issues

Enable testcontainers debug logging:
```bash
TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration -v ./test/integration/...
```

## Related Documentation

- [Main README](../../README.md) - Project overview
- [E2E Tests](../e2e/README.md) - End-to-end NFS protocol tests
- [CLAUDE.md](../../CLAUDE.md) - Development guidelines
