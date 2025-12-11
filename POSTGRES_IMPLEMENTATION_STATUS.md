# PostgreSQL Metadata Store Implementation Status

**Branch**: `feat/pg-metadata`
**Last Updated**: 2025-12-10
**Status**: Core implementation complete, testing and documentation pending

## Overview

This document tracks the implementation of PostgreSQL as a metadata store backend for DittoFS, enabling horizontal scaling in Kubernetes environments with persistent, distributed metadata storage.

## ✅ Completed Work

### 1. Core Implementation (100% Complete)

All core PostgreSQL metadata store functionality has been implemented:

#### Package Structure
- ✅ Created `pkg/store/metadata/postgres/` package
- ✅ Added dependencies: `pgx/v5` (driver), `golang-migrate/migrate` (migrations)
- ✅ Organized code into logical modules (file.go, directory.go, io.go, etc.)

#### Database Schema
- ✅ Designed normalized schema with proper indexing
- ✅ Implemented migration system with embedded SQL files
- ✅ Tables: files, parent_child_map, link_counts, shares, filesystem_capabilities, server_config
- ✅ Constraints: foreign keys, check constraints, unique constraints
- ✅ Indexes: optimized for lookup patterns (path lookups, parent-child queries)

#### Connection Management
- ✅ pgx connection pool with conservative sizing (10-15 connections)
- ✅ Configurable pool parameters (max_conns, min_conns, max_idle_time, health_check_period)
- ✅ Connection health checking and automatic retry
- ✅ Graceful shutdown with connection draining

#### Core Operations
All MetadataStore interface methods implemented:
- ✅ GetFile, Lookup, GetShareNameForHandle
- ✅ Create (dispatcher for files and directories)
- ✅ CreateRootDirectory
- ✅ SetFileAttributes
- ✅ CreateSymlink, ReadSymlink
- ✅ CreateSpecialFile, CreateHardLink (return NotSupported)
- ✅ PrepareWrite, CommitWrite, PrepareRead
- ✅ ReadDirectory (with offset-based pagination)
- ✅ Move (with deadlock prevention via ordered locking)
- ✅ RemoveFile (with link count tracking)
- ✅ RemoveDirectory (with empty check)
- ✅ GetFilesystemCapabilities, SetFilesystemCapabilities
- ✅ GetFilesystemStatistics (with 5-second cache)
- ✅ GetServerConfig, SetServerConfig
- ✅ Healthcheck

#### Interface Compliance
- ✅ All methods match MetadataStore interface signatures
- ✅ Proper AuthContext usage (first parameter for auth-required methods)
- ✅ Return types use *File instead of separate handle + attributes
- ✅ Context propagation from AuthContext.Context to database operations

#### Context Propagation (Critical Fix)
- ✅ All transaction methods use `ctx.Context` from AuthContext
- ✅ Enables request cancellation, timeouts, and graceful shutdown
- ✅ Supports distributed tracing through operation chain
- ✅ Only initialization code uses context.Background()

Benefits:
- NFS handlers can cancel operations on client disconnect
- Graceful shutdown properly cancels in-flight DB operations
- Request timeouts from NFS protocol are respected
- Tracing context flows through all database operations

#### Configuration Integration
- ✅ Added PostgreSQL configuration to `pkg/config/config.go`
- ✅ Implemented store factory in `pkg/config/stores.go`
- ✅ Environment variable support via Viper

#### Error Handling
- ✅ Comprehensive pgx error mapping to metadata.StoreError
- ✅ Proper error codes (ErrNotFound, ErrAlreadyExists, etc.)
- ✅ Path information in error messages for debugging

#### Performance Optimizations
- ✅ Statistics caching (5-second TTL) reduces DB queries
- ✅ Prepared statements via pgx (enabled by default)
- ✅ Efficient queries with proper joins
- ✅ Index-optimized lookups

### 2. Key Design Decisions

#### Handle Encoding
**Decision**: Use base64-encoded JSON with shareName + UUID
**Rationale**: Simple, debuggable, supports multi-share scenarios
**Format**: `base64({"share":"name","id":"uuid"})`

#### Transaction Strategy
**Decision**: Use transactions for all multi-step operations
**Rationale**: Ensures consistency, prevents partial updates
**Implementation**: Begin → Operations → Commit/Rollback pattern

#### Deadlock Prevention
**Decision**: Lock resources in deterministic order (sorted by UUID)
**Implementation**: In Move operation, lock parent directories in UUID order
**Example**: See `pkg/store/metadata/postgres/move.go:60-63`

#### Context Usage
**Decision**: Pass AuthContext with embedded context.Context
**Rationale**: Supports cancellation, timeouts, tracing without changing interface
**Pattern**: `ctx.Context` used for all DB operations except initialization

#### Stats Caching
**Decision**: Aggressive 5-second cache for filesystem statistics
**Rationale**: Statistics queries are expensive, data changes slowly
**Implementation**: Thread-safe cache with atomic updates

#### Pagination
**Decision**: Offset-based pagination for ReadDirectory
**Rationale**: Simple, works with SQL LIMIT/OFFSET, good enough for typical directory sizes
**Token Format**: Numeric offset as string

#### Capability Persistence
**Decision**: Persist capabilities to DB but cache in memory
**Rationale**: Survives restarts, but read from cache for performance
**Note**: SetFilesystemCapabilities doesn't take context (initialization-only)

## 🔄 Remaining Work

### 3. Unit Tests (Priority: HIGH)

**Location**: Create `pkg/store/metadata/postgres/*_test.go`

**Approach**: Use testcontainers-go for real PostgreSQL instance

```go
// Example test structure
func TestPostgresMetadataStore(t *testing.T) {
    // Use testcontainers to spin up PostgreSQL
    ctx := context.Background()
    postgresContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
        ContainerRequest: testcontainers.ContainerRequest{
            Image: "postgres:16-alpine",
            // ... configuration
        },
        Started: true,
    })

    // Run migrations
    // Create store
    // Run tests
}
```

**Test Coverage Needed**:
- ✅ Connection pool initialization and health checks
- ✅ Migration execution (up and down)
- ✅ CRUD operations for files and directories
- ✅ Transaction rollback on errors
- ✅ Context cancellation behavior
- ✅ Concurrent operations (deadlock prevention)
- ✅ Error mapping (pgx errors → StoreError)
- ✅ Statistics caching behavior
- ✅ Handle encoding/decoding
- ✅ Permission checking integration
- ✅ Move operation (same parent, cross-directory, with subdirectories)
- ✅ Link count tracking (RemoveFile)
- ✅ Pagination (ReadDirectory with large directories)

**Reference**: See existing tests in `pkg/store/metadata/memory/*_test.go` and `pkg/store/metadata/badger/*_test.go`

### 4. E2E Test Integration (Priority: HIGH)

**Files to Update**:

1. **`test/e2e/config.go`**:
   ```go
   const (
       MetadataMemory  MetadataType = "memory"
       MetadataBadger  MetadataType = "badger"
       MetadataPostgres MetadataType = "postgres" // ADD THIS
   )
   ```

2. **`test/e2e/postgres.go`** (NEW FILE):
   ```go
   // Similar to localstack.go, manage PostgreSQL container
   // Use testcontainers to spin up postgres:16-alpine
   // Return connection string for test configuration
   ```

3. **`test/e2e/framework.go`**:
   - Add PostgreSQL container management
   - Add cleanup in teardown
   - Add connection string to test context

4. **`test/e2e/main_test.go`**:
   - Add postgres-memory configuration
   - Add postgres-filesystem configuration
   - Add postgres-s3 configuration

**Test Configurations to Add**:
```go
{
    name:     "postgres-memory",
    metadata: MetadataPostgres,
    content:  ContentMemory,
},
{
    name:     "postgres-filesystem",
    metadata: MetadataPostgres,
    content:  ContentFilesystem,
},
{
    name:     "postgres-s3",
    metadata: MetadataPostgres,
    content:  ContentS3,
},
```

**Run Commands**:
```bash
# Run E2E tests with PostgreSQL (requires sudo for NFS mount)
sudo go test -tags=e2e -v ./test/e2e/ -run "TestE2E/postgres"

# Run specific configuration
sudo go test -tags=e2e -v -run "TestE2E/postgres-filesystem" ./test/e2e/
```

### 5. Docker Compose Integration (Priority: MEDIUM)

**File**: `docker-compose.yml`

**Add PostgreSQL Service**:
```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: dittofs
      POSTGRES_USER: dittofs
      POSTGRES_PASSWORD: dittofs
    ports:
      - "5432:5432"
    volumes:
      - postgres-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U dittofs"]
      interval: 5s
      timeout: 5s
      retries: 5
    profiles:
      - postgres-backend

volumes:
  postgres-data:
```

**Update DittoFS Service**:
```yaml
  dittofs:
    # ... existing config ...
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      # ... existing env vars ...
      DITTOFS_METADATA_POSTGRES_HOST: postgres
      DITTOFS_METADATA_POSTGRES_PORT: 5432
      DITTOFS_METADATA_POSTGRES_DATABASE: dittofs
      DITTOFS_METADATA_POSTGRES_USER: dittofs
      DITTOFS_METADATA_POSTGRES_PASSWORD: dittofs
    profiles:
      - postgres-backend
```

**Usage**:
```bash
# Start with PostgreSQL backend
docker-compose --profile postgres-backend up

# Start specific services
docker-compose up postgres
docker-compose up dittofs postgres
```

### 6. Documentation Updates (Priority: HIGH)

#### A. `docs/CONFIGURATION.md`

Add section for PostgreSQL metadata store:

```markdown
### PostgreSQL Metadata Store

PostgreSQL provides persistent, distributed metadata storage suitable for production deployments with horizontal scaling.

**Configuration**:

```yaml
metadata:
  stores:
    production:
      type: postgres
      postgres:
        # Connection
        host: localhost
        port: 5432
        database: dittofs
        user: dittofs
        password: ${POSTGRES_PASSWORD}  # Use env var

        # TLS (recommended for production)
        sslmode: require  # disable, require, verify-ca, verify-full

        # Connection Pool
        max_conns: 15           # Maximum connections
        min_conns: 2            # Minimum connections
        max_idle_time: 30m      # Close idle connections after
        health_check_period: 1m # Health check interval

        # Migrations
        auto_migrate: false  # Manual control recommended
        migrations_path: ""  # Use embedded migrations
```

**Connection Pool Sizing**:
- Start with 10-15 max connections
- Adjust based on:
  - Number of concurrent NFS clients
  - Database server capacity
  - Connection latency

**TLS Configuration**:
- Use `sslmode: require` or higher in production
- Provide certificate paths for `verify-ca` and `verify-full`

**Migration Strategy**:
- Set `auto_migrate: false` in production
- Run migrations manually during maintenance windows
- Test migrations in staging first

**Performance Tips**:
- Enable prepared statements (default)
- Use connection pooling (default)
- Monitor statistics cache hit rate
- Scale horizontally by adding DittoFS instances
```

#### B. `docs/ARCHITECTURE.md`

Update store list:

```markdown
**4. Metadata Store** (`pkg/store/metadata/store.go`)
- Stores file/directory structure, attributes, permissions
- Handles access control and root directory creation
- Implementations:
  - `pkg/store/metadata/memory/`: In-memory (fast, ephemeral)
  - `pkg/store/metadata/badger/`: BadgerDB (persistent, embedded)
  - `pkg/store/metadata/postgres/`: PostgreSQL (persistent, distributed)
```

Add section on scaling:

```markdown
### Horizontal Scaling with PostgreSQL

PostgreSQL metadata store enables horizontal scaling:

1. **Multiple DittoFS Instances**: Run multiple instances sharing one PostgreSQL database
2. **Load Balancing**: Use Kubernetes services or external load balancers
3. **Session Affinity**: Not required - any instance can serve any request
4. **Connection Pooling**: Each instance maintains its own connection pool
5. **Statistics Caching**: Reduces database load, 5-second TTL

**Deployment Example**:
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dittofs
spec:
  replicas: 3  # Multiple instances
  template:
    spec:
      containers:
      - name: dittofs
        env:
        - name: DITTOFS_METADATA_POSTGRES_HOST
          value: postgres-service
```
```

#### C. `CLAUDE.md`

Update store list:

```markdown
- `pkg/store/metadata/memory/`: In-memory (fast, ephemeral, full hard link support)
- `pkg/store/metadata/badger/`: BadgerDB (persistent, embedded, path-based handles)
- `pkg/store/metadata/postgres/`: PostgreSQL (persistent, distributed, UUID-based handles)
```

Add note about context usage:

```markdown
**5. Context Propagation**

All metadata store operations receive `*metadata.AuthContext` which contains:
- `Context`: Standard Go context for cancellation/timeout
- `Identity`: Effective user identity for permission checks
- `ClientAddr`: Client network address

**Important**: Always use `ctx.Context` for database operations, never `context.Background()`, except in initialization code. This ensures proper cancellation propagation.
```

#### D. `test/e2e/README.md`

Add PostgreSQL test configuration:

```markdown
### PostgreSQL Configurations

Test PostgreSQL metadata store with various content stores:

```bash
# PostgreSQL + Memory (fastest)
sudo ./run-e2e.sh --config postgres-memory

# PostgreSQL + Filesystem (production-like)
sudo ./run-e2e.sh --config postgres-filesystem

# PostgreSQL + S3 (requires Localstack)
sudo ./run-e2e.sh --s3 --config postgres-s3
```

**Requirements**:
- Docker (for PostgreSQL container via testcontainers)
- Adequate memory for PostgreSQL instance

**Performance Notes**:
- Slower than memory store (network + disk I/O)
- Representative of production performance
- Use for realistic performance testing
```

## 📊 Implementation Statistics

- **Files Created**: 13
- **Lines of Code**: ~2,500
- **Methods Implemented**: 28
- **Database Tables**: 6
- **Indexes**: 8
- **Test Coverage**: 0% (pending)

## 🔑 Key Files Reference

### Core Implementation
- `pkg/store/metadata/postgres/store.go` - Store initialization, connection pool
- `pkg/store/metadata/postgres/file.go` - GetFile, Lookup operations
- `pkg/store/metadata/postgres/directory.go` - CreateRootDirectory, ReadDirectory
- `pkg/store/metadata/postgres/create.go` - File and directory creation
- `pkg/store/metadata/postgres/attributes.go` - SetFileAttributes, CreateSymlink
- `pkg/store/metadata/postgres/io.go` - PrepareWrite, CommitWrite, PrepareRead
- `pkg/store/metadata/postgres/move.go` - Move/rename operations
- `pkg/store/metadata/postgres/remove.go` - RemoveFile, RemoveDirectory
- `pkg/store/metadata/postgres/capabilities.go` - Filesystem capabilities and statistics
- `pkg/store/metadata/postgres/access.go` - Permission checking
- `pkg/store/metadata/postgres/util.go` - Handle encoding, serialization, error mapping
- `pkg/store/metadata/postgres/migrations/` - Database schema migrations

### Configuration
- `pkg/config/config.go` - PostgreSQL configuration struct
- `pkg/config/stores.go` - Store factory implementation

### Schema
- `pkg/store/metadata/postgres/migrations/000001_initial_schema.up.sql` - Tables and indexes
- `pkg/store/metadata/postgres/migrations/000001_initial_schema.down.sql` - Teardown

## 🐛 Known Issues / Limitations

1. **Hard Links**: Not fully supported (returns NotSupported like BadgerDB)
2. **Special Files**: Not supported (devices, FIFOs, sockets)
3. **Link Counts**: Tracked internally but always reported as 1 to NFS clients
4. **Prepared Statements**: Relied on pgx default behavior, no explicit preparation
5. **Connection Pool**: No dynamic resizing, fixed at initialization

## 🚀 Testing Strategy

### Unit Tests (testcontainers-go)
```bash
go test ./pkg/store/metadata/postgres/...
```

### E2E Tests (real NFS mount)
```bash
cd test/e2e
sudo ./run-e2e.sh --config postgres-filesystem
```

### Manual Testing
```bash
# 1. Start PostgreSQL
docker run -d --name postgres \
  -e POSTGRES_DB=dittofs \
  -e POSTGRES_USER=dittofs \
  -e POSTGRES_PASSWORD=dittofs \
  -p 5432:5432 \
  postgres:16-alpine

# 2. Configure DittoFS
cat > config.yaml <<EOF
metadata:
  stores:
    test:
      type: postgres
      postgres:
        host: localhost
        port: 5432
        database: dittofs
        user: dittofs
        password: dittofs
        auto_migrate: true

content:
  stores:
    test:
      type: filesystem
      filesystem:
        base_path: /tmp/dittofs

shares:
  - name: /test
    metadata_store: test
    content_store: test
EOF

# 3. Start DittoFS
./dittofs start --config config.yaml

# 4. Mount and test
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/test /mnt/test
cd /mnt/test
# ... perform operations ...
```

## 📝 Next Steps (Priority Order)

1. **Write unit tests with testcontainers** (HIGH)
   - Set up testcontainers infrastructure
   - Test all CRUD operations
   - Test error handling and edge cases
   - Aim for >80% coverage

2. **Update E2E test integration** (HIGH)
   - Add PostgreSQL configurations
   - Create postgres.go helper (like localstack.go)
   - Update framework.go for container management
   - Run full E2E suite

3. **Update documentation** (HIGH)
   - CONFIGURATION.md - PostgreSQL section
   - ARCHITECTURE.md - Scaling information
   - CLAUDE.md - Context usage notes
   - test/e2e/README.md - PostgreSQL testing

4. **Docker Compose integration** (MEDIUM)
   - Add PostgreSQL service
   - Add health checks
   - Add profiles for different backends
   - Document usage

5. **Performance testing** (MEDIUM)
   - Benchmark against memory and BadgerDB stores
   - Test with multiple concurrent clients
   - Optimize queries if needed
   - Document findings

6. **Production hardening** (LOW - post-MVP)
   - Add prepared statement caching metrics
   - Add connection pool metrics
   - Add query performance logging
   - Consider read replicas for reads

## 🔗 Useful References

- **pgx Documentation**: https://pkg.go.dev/github.com/jackc/pgx/v5
- **golang-migrate**: https://github.com/golang-migrate/migrate
- **testcontainers-go**: https://golang.testcontainers.org/
- **PostgreSQL 16 Docs**: https://www.postgresql.org/docs/16/

## 💡 Tips for Resuming Work

1. **Start with unit tests**: They provide immediate feedback and catch regressions
2. **Use testcontainers**: Real PostgreSQL instance, no mocking complexity
3. **Reference existing tests**: Memory and BadgerDB implementations have good test coverage
4. **Test concurrency**: PostgreSQL shines with concurrent access, test it
5. **Monitor performance**: Compare with BadgerDB baseline from test/e2e/BENCHMARKS.md
6. **Check context usage**: Verify ctx.Context flows through all operations
7. **Test graceful shutdown**: Ensure in-flight operations are cancelled properly

## 🎯 Success Criteria

The PostgreSQL implementation is complete when:

- ✅ Core implementation compiles and passes interface checks
- ⬜ Unit tests pass with >80% coverage
- ⬜ E2E tests pass for all configurations (postgres-memory, postgres-filesystem, postgres-s3)
- ⬜ Documentation is complete and accurate
- ⬜ Docker Compose configuration works
- ⬜ Performance is acceptable (within 2x of BadgerDB for typical operations)
- ⬜ No known critical bugs or data corruption issues

---

**Last Commit**: `85746d7` - "fix(postgres): Fix interface signatures and context propagation"

**Build Status**: ✅ Compiles successfully, all interface signatures match

**Ready for**: Unit testing and E2E integration
