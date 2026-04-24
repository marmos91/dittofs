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

# Set up pre-commit hooks (recommended)
make setup-hooks

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

# Set up pre-commit hooks (recommended)
make setup-hooks

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

### Pre-commit Hooks

DittoFS includes pre-commit hooks that automatically check formatting and run static analysis before each commit. Set them up once after cloning:

```bash
make setup-hooks
```

The hook checks `gofmt -s` on staged Go files, then runs `go vet ./...` and `golangci-lint run` (if installed) across the full repository. Use `git commit --no-verify` to skip when needed (e.g., WIP commits).

### Linting and Formatting

```bash
# Format code
make fmt          # or: gofmt -s -w .

# Static analysis
make vet          # or: go vet ./...

# Run linters (if golangci-lint is installed)
make lint         # or: golangci-lint run
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

NFSv4/v4.1 tests are in `internal/adapter/nfs/v4/handlers/` and cover sessions, delegations, ACLs, and Kerberos.

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

1. Add handler in `internal/adapter/nfs/v3/handlers/` or `internal/adapter/nfs/mount/handlers/`
2. Implement XDR request/response parsing
3. Extract auth context from call
4. Delegate business logic to repository methods
5. Update dispatch table in `dispatch.go`
6. Add test coverage

Example:
```go
// internal/adapter/nfs/v3/handlers/myproc.go
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

DittoFS uses a Service-oriented architecture where **stores are simple CRUD interfaces**. Business logic (permission checking, caching, locking) lives in the Service layer (`MetadataService`) and the per-share `BlockStore` engine.

**Metadata Store:**

1. Implement `pkg/metadata/MetadataStore` interface (simple CRUD operations)
2. Handle file handle generation (must be unique and stable)
3. Implement root directory creation (`CreateRootDirectory`)
4. Ensure thread safety (concurrent access across shares)
5. Consider persistence strategy for handles
6. **Note**: Permission checking is handled by `MetadataService`, not stores

**Block Store (Local):**

1. Implement `pkg/blockstore/local.LocalStore` interface (ReadAt, WriteAt, Delete, Exists, Flush, List, Close)
2. Support random-access reads/writes
3. Each share gets an isolated local storage directory
4. Test with conformance suite in `pkg/blockstore/local/localtest/`

**Block Store (Remote):**

1. Implement `pkg/blockstore/remote.RemoteStore` interface (ReadBlock, WriteBlock, DeleteBlock, HealthCheck, Close)
2. Remote stores are shared across shares via ref counting
3. Test with conformance suite in `pkg/blockstore/remote/remotetest/`

Example:
```go
// pkg/blockstore/local/mybackend/store.go
type MyLocalStore struct {
    basePath string
}

func (s *MyLocalStore) ReadAt(ctx context.Context, blockID string, p []byte, offset int64) (int, error) {
    // Read block data from your backend
}

func (s *MyLocalStore) WriteAt(ctx context.Context, blockID string, data []byte, offset int64) (int, error) {
    // Write block data to your backend
}
```

See [IMPLEMENTING_STORES.md](IMPLEMENTING_STORES.md) for detailed implementation guide.

### Adding a new metadata.ErrorCode

Every `metadata.ErrorCode` that should surface to NFS and/or SMB clients is
mapped to protocol-specific status codes through a single consolidated
table at `internal/adapter/common/errmap.go`. Adding a new code is a
one-edit-per-table contract — the Go type system forces you to populate
every protocol column.

1. **Declare the constant** — add the new value in
   `pkg/metadata/errors/errors.go` and re-export it from
   `pkg/metadata/errors.go`.

2. **Add the general-context row** — insert a new row in
   `internal/adapter/common/errmap.go`'s `errorMap`:

   ```go
   merrs.ErrFooBar: {
       NFS3: nfs3types.NFS3ErrXXX,
       NFS4: nfs4types.NFS4ERR_XXX,
       SMB:  smbtypes.StatusXXX,
   },
   ```

   The `protoCodes` struct literal requires all three columns — you
   cannot accidentally forget a protocol.

3. **Lock-context override (if applicable)** — if the code has different
   semantics in SMB2 LOCK / NFSv4 LOCK requests versus general I/O, add a
   row in `internal/adapter/common/lock_errmap.go`'s `lockErrorMap` with
   the lock-context codes. `MapLockTo*` accessors consult this table
   first and fall through to `errorMap`.

4. **Update the coverage enumeration** — append the new code to
   `allErrorCodes()` in `internal/adapter/common/errmap_test.go` and bump
   the `expectedCount` constant in `TestErrorMapCoverage`. The coverage
   test fails if you forget this step.

5. **Add a test case** — pick the right tier:
   - **E2E tier** (triggerable via real NFS/SMB file operations): add a
     trigger helper in `test/e2e/helpers/error_triggers.go` and a row in
     `TestCrossProtocol_ErrorConformance` in
     `test/e2e/cross_protocol_test.go`. Expected errnos are derived at
     runtime from `common.MapToNFS3` / `common.MapToSMB` — you do not
     hand-transcribe protocol codes into the test table.
   - **Unit tier** (exotic codes — quota, grace-period, deadlock,
     connection-limits): add the code to `exoticCodes()` in
     `internal/adapter/common/errmap_test.go`. `TestExoticErrorCodes`
     picks it up automatically and asserts every protocol mapper
     returns the `errorMap` row's expected value.
   - **Not sure which tier**: run `TestCrossProtocolUnitConformance` —
     it fails if the new code lives in neither list.

6. **Run the test suite** — `go test -race ./internal/adapter/common/...`
   must stay green.

### Adding a New Protocol Adapter

Adapters receive a runtime reference and **interact with services, not stores directly**.

1. Create new package in `pkg/adapter/`
2. Implement `Adapter` interface:
   - `Serve(ctx)`: Start protocol server
   - `Stop(ctx)`: Graceful shutdown
   - `SetRuntime()`: Receive runtime reference (provides access to services)
   - `Protocol()`: Return name
   - `Port()`: Return listen port
3. Use `runtime.GetBlockStoreForHandle()` for per-share block store access
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

func (a *SMBAdapter) handleRead(ctx context.Context, handle []byte, blockID string) ([]byte, error) {
    // Resolve per-share block store from file handle
    blockStore, _ := a.runtime.GetBlockStoreForHandle(ctx, handle)
    buf := make([]byte, size)
    blockStore.ReadAt(ctx, blockID, buf, 0)
    return buf, nil
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

- Additional repository backend implementations (Redis, custom)
- Performance optimization and profiling
- Test coverage expansion (especially NFSv4 test coverage)
- Protocol compliance testing
- Kerberos authentication testing

### Medium Priority

- Documentation improvements
- Example applications and tutorials
- Monitoring and observability
- Distributed PostgreSQL testing

### Future Work

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

## CI Workflows

DittoFS uses GitHub Actions with a tiered CI strategy: fast checks on PRs, comprehensive validation on push, and full matrix testing weekly.

### Workflow Overview

| Workflow | Trigger | Purpose | Duration |
|----------|---------|---------|----------|
| `lint.yml` | PR, push | Go lint + vet | ~2 min |
| `unit-tests.yml` | PR, push | Unit tests with race detection | ~3 min |
| `windows-build.yml` | PR, push | Windows build + unit tests | ~5 min |
| `integration-tests.yml` | push, weekly | Integration tests (S3, BadgerDB) | ~10 min |
| `e2e-tests.yml` | PR, push | E2E tests (NFS, SMB, cross-protocol, Kerberos) | ~15 min |
| `smb-conformance.yml` | PR (memory), push (all), weekly | WPTS BVT + smbtorture + Kerberos | ~20 min |
| `smb-client-compat.yml` | push, weekly | Windows/macOS/Linux SMB client testing | ~10 min |
| `posix-tests.yml` | push, weekly | POSIX compliance (pjdfstest) | ~15 min |
| `operator-tests.yml` | push, weekly | Operational scenario tests | ~10 min |

### What Runs on PR (Fast, Must-Pass Before Merge)

These workflows gate pull request merges and should complete in under 5 minutes:

- **lint.yml** -- Go formatting, vetting, and linting
- **unit-tests.yml** -- All unit tests with race detection
- **windows-build.yml** -- Windows cross-compilation verification
- **smb-conformance.yml** -- Memory-only WPTS BVT + memory-only smbtorture (fast feedback)
- **e2e-tests.yml** -- Full E2E suite including SMB3, cross-protocol, and Kerberos tests

### What Runs on Push to Develop (Comprehensive)

These run after merging to develop and should complete in under 30 minutes:

- Everything from PR checks
- **smb-conformance.yml** -- All storage profiles (memory, memory-fs, badger-fs, badger-s3, postgres-s3)
- **smb-client-compat.yml** -- Multi-OS client compatibility (Linux, macOS, Windows)
- **integration-tests.yml** -- Backend-specific integration tests
- **posix-tests.yml** -- POSIX compliance validation

### What Runs Weekly (Full Matrix)

The weekly cron runs the complete test matrix (Monday mornings UTC):

- Everything from push checks
- **smb-conformance.yml** -- Kerberos smbtorture (runs only on push/weekly, not PRs)
- **operator-tests.yml** -- Operational scenarios
- Auto-creates GitHub issues for regressions

### How to Manually Trigger Workflows

All workflows support `workflow_dispatch` for manual triggering:

```bash
# Trigger via GitHub CLI
gh workflow run smb-conformance.yml
gh workflow run smb-conformance.yml -f profile=badger-fs
gh workflow run smb-client-compat.yml
gh workflow run e2e-tests.yml
```

Or use the GitHub Actions UI: navigate to the workflow, click "Run workflow", and select options.

### Interpreting Conformance Test Results

When a conformance test fails in CI:

1. **Check the step summary** -- GitHub Actions provides a table with pass/fail/known counts
2. **Download artifacts** -- TRX files (WPTS) and log files (smbtorture) are uploaded as artifacts
3. **Check KNOWN_FAILURES.md** -- If the failing test is in the known failures list, it was expected
4. **For new failures:**
   - If the failure is in a genuinely unimplemented feature, add it to `KNOWN_FAILURES.md`
   - If the failure is in an implemented feature, it is a regression and needs fixing
   - Use `--keep` flag locally to leave containers running for debugging

### How to Add New CI Jobs

Follow this template pattern for new workflow jobs:

```yaml
  new-job:
    name: Descriptive Job Name
    runs-on: ubuntu-latest
    timeout-minutes: 15
    strategy:
      fail-fast: false
      matrix:
        profile: >-
          ${{
            github.event_name == 'pull_request'
              && fromJson('["memory"]')
            || fromJson('["memory", "memory-fs", "badger-fs"]')
          }}
    steps:
      - uses: actions/checkout@v4
      - name: Run tests
        run: ./your-test-script.sh --profile ${{ matrix.profile }}
      - name: Upload results
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: results-${{ matrix.profile }}
          path: results/
          retention-days: 30
```

Key guidelines:
- Use matrix strategy with tiered profiles (memory-only on PR, all on push/weekly)
- Always set `timeout-minutes` to prevent runaway jobs
- Use `fail-fast: false` so one profile failure does not cancel others
- Upload artifacts with `if: always()` so results are available even on failure
- Add concurrency groups to prevent duplicate runs

## Submitting Changes

1. Fork the repository
2. Set up pre-commit hooks (`make setup-hooks`)
3. Create a feature branch (`git checkout -b feature/amazing-feature`)
4. Make your changes
5. Run tests (`go test ./...`)
6. Commit your changes — pre-commit hooks check formatting and lint automatically
7. Push to the branch (`git push origin feature/amazing-feature`)
8. Open a Pull Request

## Getting Help

- Open an issue on GitHub for bugs or feature requests
- Check existing issues for similar problems
- Review the [Architecture](ARCHITECTURE.md) and [FAQ](FAQ.md) documentation

## License

By contributing to DittoFS, you agree that your contributions will be licensed under the MIT License.
