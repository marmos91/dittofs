# DittoFS Integration Tests

This directory contains integration tests for DittoFS that require external services like databases, cloud storage, etc.

## Overview

Integration tests verify that DittoFS works correctly with real external dependencies:

- **S3 Integration Tests** (`s3/`) - Test S3 content store against Localstack
- **Badger Integration Tests** (`badger/`) - Test Badger metadata store with persistent storage *(future)*

## Quick Start

### Prerequisites

- Docker and Docker Compose installed
- Go 1.21 or later
- `jq` (optional, for prettier output)

### Running All Integration Tests

```bash
./test/integration/run-tests.sh
```

This script will:
1. Start required services (Localstack) via Docker Compose
2. Wait for services to be ready
3. Run all integration tests
4. Clean up Docker resources

### Running Specific Tests

```bash
# Run only S3 integration tests
./test/integration/run-tests.sh s3

# Run only Badger integration tests (when available)
./test/integration/run-tests.sh badger
```

### Manual Testing (Without Docker Management)

If you already have services running or want to manage Docker separately:

```bash
# Terminal 1: Start services manually
docker compose -f test/integration/docker-compose.yml up

# Terminal 2: Run tests without Docker management
./test/integration/run-tests.sh --no-docker
```

### Cleanup

```bash
# Stop and remove all Docker resources
./test/integration/run-tests.sh --cleanup
```

## Test Structure

```
test/integration/
├── README.md                    # This file
├── docker-compose.yml           # Service definitions (Localstack, etc.)
├── run-tests.sh                 # Test orchestration script
├── localstack-init/             # Localstack initialization scripts
│   └── 01-setup.sh              # Initial S3 bucket setup
└── s3/                          # S3 integration tests
    └── s3_test.go               # S3 content store tests
```

## Services

### Localstack

**Purpose**: S3-compatible object storage for testing S3 content store

**Endpoint**: http://localhost:4566

**Health Check**: `curl http://localhost:4566/_localstack/health`

**Configuration**:
- Services: S3 only (minimal footprint)
- Credentials: `test` / `test` (static for testing)
- Region: `us-east-1`
- Path-style URLs: Enabled (required for Localstack)

**Manual Access**:
```bash
# Using AWS CLI with Localstack
aws --endpoint-url=http://localhost:4566 s3 ls

# Or using awslocal (Localstack wrapper)
awslocal s3 ls
```

## Writing Integration Tests

### Best Practices

1. **Use build tags**: All integration tests must have `//go:build integration`
2. **Clean up resources**: Create and destroy test resources in each test
3. **Use unique names**: Generate unique bucket/database names per test
4. **Set timeouts**: Use reasonable timeouts to avoid hanging tests
5. **Check prerequisites**: Verify service connectivity before running tests

### Example Integration Test

```go
//go:build integration

package myservice_test

import (
    "context"
    "testing"

    "github.com/marmos91/dittofs/pkg/store/content"
    // ... other imports
)

func TestMyServiceIntegration(t *testing.T) {
    ctx := context.Background()

    // Setup: Create test resources
    service := setupTestService(t)
    defer cleanupTestService(service)

    // Test: Perform operations
    // ...

    // Verify: Check results
    // ...
}

func setupTestService(t *testing.T) *Service {
    t.Helper()
    // Create service with unique test resources
    return service
}

func cleanupTestService(service *Service) {
    // Clean up test resources
}
```

### Test Naming Convention

- Package: `<service>_test` (e.g., `s3_test`, `badger_test`)
- Files: `<service>_test.go`
- Functions: `Test<Service>_<Feature>` (e.g., `TestS3_Multipart`)

## CI/CD Integration

### GitHub Actions

Example workflow for integration tests:

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
        run: ./test/integration/run-tests.sh

      - name: Cleanup
        if: always()
        run: ./test/integration/run-tests.sh --cleanup
```

### GitLab CI

Example `.gitlab-ci.yml`:

```yaml
integration-tests:
  stage: test
  image: golang:1.21
  services:
    - docker:dind
  script:
    - ./test/integration/run-tests.sh
  after_script:
    - ./test/integration/run-tests.sh --cleanup
```

## Environment Variables

Integration tests respect these environment variables:

- `LOCALSTACK_ENDPOINT` - Override Localstack endpoint (default: `http://localhost:4566`)
- `AWS_ACCESS_KEY_ID` - AWS credentials for S3 tests (default: `test`)
- `AWS_SECRET_ACCESS_KEY` - AWS secret key (default: `test`)
- `AWS_REGION` - AWS region (default: `us-east-1`)

## Troubleshooting

### Localstack won't start

```bash
# Check if port 4566 is already in use
lsof -i :4566

# Kill any processes using the port
kill -9 <PID>

# Or use a different port by editing docker-compose.yml
```

### Tests timeout

```bash
# Check Localstack health
curl http://localhost:4566/_localstack/health

# View Localstack logs
docker compose -f test/integration/docker-compose.yml logs -f localstack
```

### Tests fail with connection refused

```bash
# Ensure Localstack is running
docker compose -f test/integration/docker-compose.yml ps

# Check if containers are healthy
docker compose -f test/integration/docker-compose.yml ps --format json | jq '.[] | {name: .Name, health: .Health}'
```

### Clean slate

```bash
# Remove all containers, networks, and volumes
docker compose -f test/integration/docker-compose.yml down -v --remove-orphans

# Remove test data
rm -rf test/integration/localstack-data

# Restart
./test/integration/run-tests.sh
```

## Performance Considerations

### Test Parallelization

Integration tests can run in parallel using Go's test parallelization:

```go
func TestSomething(t *testing.T) {
    t.Parallel()  // Run this test in parallel with others
    // ... test code
}
```

**Note**: Be careful with parallel tests when they share resources (buckets, databases, etc.)

### Resource Limits

Docker Compose services are configured with reasonable defaults. Adjust in `docker-compose.yml` if needed:

```yaml
services:
  localstack:
    deploy:
      resources:
        limits:
          cpus: '2'
          memory: 1G
```

## Adding New Integration Tests

1. **Create test directory**: `test/integration/<service>/`
2. **Write tests**: `<service>_test.go` with `//go:build integration`
3. **Update docker-compose.yml**: Add service if needed
4. **Update run-tests.sh**: Add test filter case
5. **Document in README**: Add service section and usage

## Related Documentation

- [Main README](../../README.md) - Project overview
- [E2E Tests](../e2e/README.md) - End-to-end NFS protocol tests
- [CLAUDE.md](../../CLAUDE.md) - Development guidelines

## Support

For issues or questions:
- Open an issue: https://github.com/marmos91/dittofs/issues
- Check discussions: https://github.com/marmos91/dittofs/discussions
