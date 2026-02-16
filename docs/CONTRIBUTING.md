# Contributing to DittoFS

DittoFS is in active development and welcomes contributions! This guide will help you get started with development.

## Table of Contents

- [Getting Started](#getting-started)
- [Development Workflow](#development-workflow)
- [Testing](#testing)
- [Benchmarking](#benchmarking)
- [Common Development Tasks](#common-development-tasks)
- [Areas Needing Attention](#areas-needing-attention)

## Getting Started

### Using Nix (Recommended)

The easiest way to get a complete development environment is using [Nix](https://nixos.org/):

```bash
# Clone repository
git clone https://github.com/marmos91/dittofs.git
cd dittofs

# Enter development shell (installs all dependencies automatically)
nix develop

# Or with direnv (auto-activates when entering directory)
direnv allow

# Build and run
go build -o dfs cmd/dfs/main.go
./dfs init
./dfs start
```

The Nix flake provides:
- Go 1.25 with gopls, delve debugger
- golangci-lint for code quality
- NFS utilities for E2E testing (Linux)
- ACL libraries for POSIX compliance testing

### Manual Setup (Alternative)

If you prefer not to use Nix, install dependencies manually:

#### Prerequisites

- Go 1.25 or higher
- NFS client tools (for E2E testing)
  - Linux: `nfs-common` package
  - macOS: Built-in NFS client
- Git

### Clone and Setup

```bash
# Clone repository
git clone https://github.com/marmos91/dittofs.git
cd dittofs

# Install dependencies
go mod download

# Build
go build -o dfs cmd/dfs/main.go

# Run with development settings
./dfs init
./dfs start --log-level DEBUG
```

## Development Workflow

### Building

```bash
# Build the main binary
go build -o dfs cmd/dfs/main.go

# Install dependencies
go mod download
```

### Running

```bash
# Run server with defaults (port 2049, INFO logging)
./dfs start

# Run with debug logging and custom settings
./dfs start --log-level DEBUG

# Use environment variables for quick config overrides
DITTOFS_LOGGING_LEVEL=DEBUG DITTOFS_ADAPTERS_NFS_PORT=12049 ./dfs start
```

### Linting and Formatting

```bash
# Format code
go fmt ./...

# Static analysis
go vet ./...

# Run linters (if golangci-lint is installed)
golangci-lint run
```

## Testing

### Unit Tests

```bash
# Run all tests
go test ./...

# Run with coverage
go test -cover ./...

# Run with race detection
go test -race ./...

# Run specific package
go test ./pkg/metadata/store/memory/
```

### Integration Tests

```bash
# Run integration tests (S3, BadgerDB, etc.)
go test -v ./test/integration/...
```

### E2E Testing Framework

DittoFS includes a comprehensive end-to-end testing framework that validates real-world NFS operations by:

- **Starting a real DittoFS server** with configurable backends
- **Mounting the NFS filesystem** using platform-native mount commands
- **Executing real file operations** using standard Go `os` package functions
- **Testing all combinations** of adapters and storage backends

Test suites cover:

- Basic file operations (create, read, write, delete)
- Directory operations (mkdir, readdir, rename)
- Symbolic and hard links
- File attributes and permissions
- Idempotency guarantees
- Edge cases and boundary conditions

```bash
# Run E2E tests (requires NFS client installed)
go test -v -timeout 30m ./test/e2e/...

# Run specific E2E suite
go test -v ./test/e2e -run TestE2E/memory/BasicOperations

# Test specific backend
go test -v ./test/e2e -run TestE2E/filesystem/
```

See [test/e2e/README.md](../test/e2e/README.md) for detailed documentation.

### NFS Client Testing

```bash
# Mount on Linux
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test

# Mount on macOS (requires resvport)
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,resvport localhost:/export /mnt/test

# Test operations
cd /mnt/test
ls -la
echo "test" > file.txt
cat file.txt

# Unmount
sudo umount /mnt/test
```

## Benchmarking

DittoFS includes a comprehensive benchmark suite for performance testing:

```bash
# Run comprehensive benchmark suite (separate from tests)
./scripts/benchmark.sh

# Run with profiling (CPU and memory)
./scripts/benchmark.sh --profile

# Compare with previous results
./scripts/benchmark.sh --compare

# Custom configuration
BENCH_TIME=30s BENCH_COUNT=5 ./scripts/benchmark.sh

# Run specific benchmarks manually
go test -bench='BenchmarkE2E/memory/ReadThroughput' -benchtime=20s ./test/e2e/
go test -bench='BenchmarkE2E/filesystem' -benchmem ./test/e2e/

# Generate CPU profile for specific benchmark
go test -bench=BenchmarkE2E/memory/WriteThroughput/100MB \
    -cpuprofile=cpu.prof -benchtime=30s ./test/e2e/

# Analyze profile
go tool pprof cpu.prof
go tool pprof -http=:8080 cpu.prof
```

**Important**: Benchmarks are stress tests designed to push DittoFS to its limits. They:
- Test with files from 4KB to 100MB
- Create thousands of files/directories
- Run mixed concurrent workloads
- Profile CPU and memory usage
- Compare different storage backends

Results are saved to `benchmark_results/<timestamp>/` and should NOT be committed to the repository.

See `test/e2e/BENCHMARKS.md` for detailed documentation and `test/e2e/COMPARISON_GUIDE.md` for comparing with other NFS implementations.

## Common Development Tasks

### Adding a New NFS Procedure

1. Add handler in `internal/protocol/nfs/v3/handlers/` or `internal/protocol/nfs/mount/handlers/`
2. Implement XDR request/response parsing
3. Extract auth context from call
4. Delegate business logic to repository methods
5. Update dispatch table in `dispatch.go`
6. Add test coverage

Example:
```go
// internal/protocol/nfs/v3/handlers/myproc.go
func HandleMyProc(ctx context.Context, call *rpc.Call, metadata metadata.Store) (*rpc.Reply, error) {
    // 1. Parse XDR request
    req := xdr.DecodeMyProcArgs(call.Body)

    // 2. Extract auth context
    authCtx := dispatch.ExtractAuthContext(call)

    // 3. Delegate to repository
    result, err := metadata.MyOperation(ctx, authCtx, req.Handle)

    // 4. Encode response
    return xdr.EncodeMyProcRes(result), nil
}
```

### Adding a New Store Backend

DittoFS uses a Service-oriented architecture where **stores are simple CRUD interfaces**. Business logic (permission checking, caching, locking) lives in the Service layer (`MetadataService`, `BlockService`).

**Metadata Store:**

1. Implement `pkg/metadata/MetadataStore` interface (simple CRUD operations)
2. Handle file handle generation (must be unique and stable)
3. Implement root directory creation (`CreateRootDirectory`)
4. Ensure thread safety (concurrent access across shares)
5. Consider persistence strategy for handles
6. **Note**: Permission checking is handled by `MetadataService`, not stores

**Content Store:**

1. Implement `pkg/blocks/ContentStore` interface (simple CRUD operations)
2. Support random-access reads/writes (`ReadAt`/`WriteAt`)
3. Handle sparse files and truncation
4. Consider implementing optional interfaces for efficiency (`IncrementalWriteStore`)
5. **Note**: Caching is handled by `BlockService`, not stores
6. Test with the integration test suite in `test/integration/`

Example:
```go
// pkg/blocks/store/mybackend/store.go
type MyContentStore struct {
    // Your implementation - just CRUD, no business logic
}

func (s *MyContentStore) ReadAt(ctx context.Context, id content.ContentID, offset int64, size int64) ([]byte, error) {
    // Simple read from your backend
}

func (s *MyContentStore) WriteAt(ctx context.Context, id content.ContentID, data []byte, offset int64) error {
    // Simple write to your backend
}

// Register with BlockService (which handles caching, routing)
payloadSvc.RegisterStoreForShare("/myshare", myContentStore)
```

See [IMPLEMENTING_STORES.md](IMPLEMENTING_STORES.md) for detailed implementation guide.

### Adding a New Protocol Adapter

Adapters receive a runtime reference and **interact with services, not stores directly**.

1. Create new package in `pkg/adapter/`
2. Implement `Adapter` interface:
   - `Serve(ctx)`: Start protocol server
   - `Stop(ctx)`: Graceful shutdown
   - `SetRuntime()`: Receive runtime reference (provides access to services)
   - `Protocol()`: Return name
   - `Port()`: Return listen port
3. Use `runtime.GetMetadataService()` and `runtime.GetBlockService()` for operations
4. Register in `cmd/dfs/main.go`
5. Update README with usage instructions

Example:
```go
// pkg/adapter/smb/adapter.go
type SMBAdapter struct {
    config  SMBConfig
    runtime *runtime.Runtime
}

func (a *SMBAdapter) SetRuntime(rt *runtime.Runtime) {
    a.runtime = rt
}

func (a *SMBAdapter) handleRead(ctx context.Context, shareName string, contentID content.ContentID) ([]byte, error) {
    // Use BlockService (handles caching automatically)
    return a.runtime.GetBlockService().ReadAt(ctx, shareName, contentID, 0, size)
}

func (a *SMBAdapter) Serve(ctx context.Context) error {
    // Start SMB server
}

func (a *SMBAdapter) Stop(ctx context.Context) error {
    // Graceful shutdown
}
```

## Areas Needing Attention

### High Priority

- Additional repository backend implementations (Redis, PostgreSQL, custom)
- Performance optimization and profiling
- Test coverage expansion
- Protocol compliance testing

### Medium Priority

- SMB/CIFS adapter implementation
- Documentation improvements
- Example applications and tutorials
- Monitoring and observability

### Future Work

- NFSv4 support
- Advanced caching strategies
- Multi-region replication

## Code Guidelines

### Separation of Concerns

**Protocol handlers should ONLY handle protocol-level concerns:**
- XDR encoding/decoding
- RPC message framing
- Procedure dispatch
- Converting between wire types and internal types

**Business logic belongs in repository implementations:**
- Permission checks (`CheckAccess`)
- File creation/deletion
- Directory traversal
- Metadata updates

Example:
```go
// GOOD: Handler delegates to repository
func HandleLookup(ctx *AuthContext, dirHandle, name string) {
    // Parse XDR request
    // Call repo.Lookup(ctx, dirHandle, name)
    // Encode XDR response
}

// BAD: Handler implements permission checks
func HandleLookup(ctx *AuthContext, dirHandle, name string) {
    attr := getFile(dirHandle)
    if attr.UID != ctx.UID { /* check permissions */ }  // ❌ Wrong layer
}
```

### Error Handling

Return proper NFS error codes via `metadata.ExportError`:

```go
// Examples from metadata/errors.go
ErrNotDirectory      // NFS3ERR_NOTDIR
ErrNoEntity          // NFS3ERR_NOENT
ErrAccess            // NFS3ERR_ACCES
ErrExist             // NFS3ERR_EXIST
ErrNotEmpty          // NFS3ERR_NOTEMPTY
```

Log appropriately:
- `logger.Debug()`: Expected/normal errors (permission denied, file not found)
- `logger.Error()`: Unexpected errors (I/O errors, invariant violations)

## Submitting Changes

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Make your changes
4. Run tests (`go test ./...`)
5. Run linters (`go fmt ./...` and `go vet ./...`)
6. Commit your changes (`git commit -m 'Add amazing feature'`)
7. Push to the branch (`git push origin feature/amazing-feature`)
8. Open a Pull Request

## Getting Help

- Open an issue on GitHub for bugs or feature requests
- Check existing issues for similar problems
- Review the [Architecture](ARCHITECTURE.md) and [FAQ](FAQ.md) documentation

## License

By contributing to DittoFS, you agree that your contributions will be licensed under the MIT License.
