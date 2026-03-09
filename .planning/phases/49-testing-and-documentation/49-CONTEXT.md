# Phase 49: Testing and Documentation - Context

**Gathered:** 2026-03-09
**Status:** Ready for planning

<domain>
## Phase Boundary

Update E2E tests for the new block store CLI/architecture, add multi-share isolation tests, benchmark L1 cache with cache-tiers workload, implement cache stats/evict CLI and API, perform comprehensive legacy cleanup (remove ALL payload store references from the entire repository), and update all documentation to reflect the two-tier block store model.

**Scope expansion from discussion:** Added `dfsctl cache evict` and `dfsctl cache stats` CLI commands with REST API backing (resolves GitHub issue #246). Full legacy payload store removal across the entire codebase.

</domain>

<decisions>
## Implementation Decisions

### E2E Store Matrix Restructure
- Full 3D matrix: 3 metadata (memory/badger/postgres) x 2 local (fs/memory) x 3 remote (none/memory/s3) = 18 combos
- Filesystem payload store removed (Phase 42) — remote dimension: none, memory, s3 only
- Test helpers use only new CLI (`dfsctl store block local/remote`). Old payload CLI NOT tested
- Helper methods replaced entirely: `CreatePayloadStore` -> `CreateLocalBlockStore` / `CreateRemoteBlockStore`. Old methods deleted
- `payload_stores_test.go` replaced in-place -> `block_stores_test.go`
- storeConfig struct uses nested config: `storeConfig { metadataType, blockStore: { localType, remoteType } }`
- Both NFS and SMB mount tests run the full 18-combo matrix
- Expanded file operations: create, read, write, delete, mkdir, rename, truncate, append
- Small file sizes only (~1KB per combo) — larger file tests exist separately
- NFSv4 store matrix test (`nfsv4_store_matrix_test.go`) also updated to same 18-combo matrix
- Shared storeMatrix definition in a common file (e.g., `matrix_config_test.go`) — imported by v3, v4, and SMB matrix tests
- Quick mode: `testing.Short()` runs only 3-4 representative combos (memory/fs/none, badger/fs/s3, postgres/fs/s3). Full mode: all 18
- `run-e2e.sh` script updated with new flags: `--local-only`, `--with-remote` to control matrix dimensions
- Share CLI tests: matrix covers happy path + dedicated test for error cases (missing local store, invalid remote, local required)

### Multi-Share Isolation Tests
- 2 shares with different local paths
- Isolation properties tested:
  - **Data isolation**: Write to share A, verify share B sees nothing
  - **Deletion isolation**: Delete share A, verify share B data intact
  - **Concurrent writes**: Write simultaneously to both shares, no interference
  - **Cache independence**: Configure share A with tiny cache, write enough to trigger eviction, verify share B unaffected
  - **Cross-protocol lock visibility**: Lock file on share A via SMB, verify NFS sees the lock on same share. Verify same filename on share B is NOT locked
- Mount scenarios: both-NFS AND NFS+SMB mixed mounting
- Remote backend scenarios: one subtest with same remote (same S3 bucket — tests payloadID namespacing), another with different remotes
- Functional verification only — do NOT inspect internal cache directory structure
- Cache eviction test uses tiny cache size to trigger automatic eviction (fills share to exceed limit)

### Cache CLI and API (NEW — resolves GitHub #246)
- `dfsctl cache evict [--share <name>]` — evict cache for specific share or all shares
  - Flags: `--l1-only` (evict L1 memory cache only), `--local-only` (evict local FS blocks only)
  - Default: evict both L1 and local blocks
  - Safety: Requires remote store configured for local block eviction. Refuses if no remote (data loss)
  - Only evicts blocks with state=Remote. Dirty/Local blocks stay (safe — data backed up)
  - Output: quiet by default, `-v` shows detailed stats (blocks evicted, bytes freed, L1 entries cleared)
- `dfsctl cache stats [--share <name>]` — show cache statistics
  - Fields: block counts by state, size info, L1 hit/miss rate, offloader status (pending syncs, active uploads, queue depth)
  - Without --share: returns aggregated totals + per-share breakdown
  - With --share: returns specific share stats only
- All output formats supported: `-o json`, `-o yaml`, default table (consistent with other dfsctl commands)
- REST API endpoints: `GET /api/v1/shares/{name}/cache/stats` and `POST /api/v1/shares/{name}/cache/evict`
- Global endpoints: `GET /api/v1/cache/stats` (aggregated + per-share) and `POST /api/v1/cache/evict` (all shares)
- Close GitHub issue #246 after implementation

### L1 Cache Benchmark (cache-tiers workload)
- New workload type in `dfsctl bench run`: `--workload cache-tiers`
- Six-step benchmark workflow:
  1. Write — populate data via NFS/SMB
  2. Evict all cache — `dfsctl cache evict --share X` (both L1 + local blocks)
  3. Cold read — measure remote store (S3) latency
  4. Warm read — measure L1 + local disk (data now cached)
  5. Evict L1 only — `dfsctl cache evict --share X --l1-only`
  6. Warm read (L2 only) — measure local disk without L1 memory cache
- Multiple file sizes: 10MB, 100MB, 1GB sequentially — shows how cache performance scales
- Metrics: throughput (MB/s), L1 hit rate, p50/p99 latency. Includes cache stats after each read phase
- Requires authentication (cache evict API). Error out if not authenticated
- Results: inline table output (no JSON file)
- Informational only — no pass/fail assertions. Developers interpret results

### Backward Compatibility — Full Clean Break
- `--payload` flag on share creation: **hard error** — rejected with message pointing to `--local` / `--remote`
- `dfsctl store payload` commands: **removed entirely** — no deprecation warning, no dead code
- REST API `/api/v1/payload-stores` endpoints: **removed entirely** — only `/api/v1/block-stores/local` and `/remote`
- `pkg/apiclient/` payload store methods: **removed entirely** — only block store methods
- Config YAML key: `payload:` renamed to `block_store:` with local/remote sub-sections
- Environment variables: `DITTOFS_PAYLOAD_*` renamed to `DITTOFS_BLOCK_STORE_*`
- `dfs config validate`: detects old `payload:` key and prints migration hint
- `dfs config init`: generates new format with `block_store:` section by default
- JSON schema updated for new block_store structure
- **DOCS-04 requirement updated**: changed from "deprecation warning" to "removed entirely (clean break)"

### Comprehensive Legacy Cleanup — Zero Payload References
- Full rename everywhere: all variable names, struct fields, comments, log messages that say "payload store" → "block store" (local/remote)
- Scope: Go code, docs, configs, Dockerfiles, docker-compose, bench/, scripts, CI workflows, Makefiles, GH templates, schemas, env vars, K8s/Helm, POSIX test scripts, benchmark scripts
- Grep verification step: `grep -rn 'payload.store\|PayloadStore\|payload_store'` across entire repo must return empty
- E2E tests updated: all tests using payload store CLI commands rewritten for block store commands

### Documentation — Full Sweep
- **docs/ARCHITECTURE.md**: Full refactor — update all diagrams, replace payload sections with block store sections, add cache tiers, per-share isolation explanation
- **docs/CONFIGURATION.md**: New `block_store` config section, updated share config examples with `--local`/`--remote`, new `DITTOFS_BLOCK_STORE_*` env vars documented
- **CLAUDE.md**: Full overhaul — new pkg/blockstore/ directory tree, new CLI commands, two-tier architecture, cache CLI docs, remove all old cache/WAL references
- **README.md**: Updated with block store terminology, new CLI examples
- **docs/IMPLEMENTING_STORES.md**: Full rewrite for LocalStore and RemoteStore interfaces
- **docs/NFS.md, docs/TROUBLESHOOTING.md, docs/FAQ.md, docs/SECURITY.md**: Update any payload references
- **bench/**: Update docker-compose, configs, scripts for block store naming
- **K8s/Helm**: Update operator and Helm chart values/templates
- **.github/**: Update issue/PR templates if they reference payload
- All Dockerfiles updated
- All benchmark scripts updated

### Claude's Discretion
- CI gating strategy for 18-combo matrix (skip postgres/S3 if Docker/Localstack unavailable)
- E2E test subtest naming convention (short triple vs descriptive)
- Isolation test file organization (new file vs existing)
- Cache CLI test file organization
- `dfsctl cache stats` table format for all-shares view (summary table vs per-share blocks)
- Internal API response structure for cache stats/evict

</decisions>

<specifics>
## Specific Ideas

- User emphasized: "Remove any reference to old payload store and legacy code from the repository" — zero tolerance for leftover payload terminology
- User emphasized: "Check also Dockerfiles, POSIX compliance tests, benchmark scripts, and other scripts"
- Locks should be per-share: "If I lock file A with SMB I should see it locked with NFS on the same share" but NOT locked on a different share
- Cache eviction command preferred under `dfsctl cache` namespace rather than `dfsctl share`
- Benchmark workflow designed to isolate each cache tier (cold/L1+L2/L2-only) for clear performance attribution
- Issue #246 resolved via `dfsctl cache evict` (remote API), not `dfs cache clean` (server-side). Close #246 after implementation

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `test/e2e/store_matrix_test.go`: Existing 9-combo matrix — restructure to 18-combo 3D matrix
- `test/e2e/payload_stores_test.go`: Rename in-place to block_stores_test.go
- `test/e2e/helpers/`: ServerProcess, CLI runner, UniqueTestName — extend with block store helpers
- `cmd/dfsctl/commands/bench/`: Existing benchmark suite — add cache-tiers workload
- `pkg/bench/`: Benchmark engine — add cache-tier benchmark runner
- `internal/cli/output/`: Table/JSON/YAML formatters — reuse for cache stats output
- `internal/cli/prompt/`: Interactive prompts — reuse if needed for evict confirmation

### Established Patterns
- E2E tests use `helpers.StartServerProcess(t, "")` + `helpers.LoginAsAdmin(t, serverURL)` pattern
- CLI commands follow Cobra pattern in `cmd/dfsctl/commands/`
- API handlers follow thin-handler pattern delegating to Runtime methods
- E2E tests use `//go:build e2e` tag and `testing.Short()` for gating

### Integration Points
- `test/e2e/nfsv4_store_matrix_test.go`: Also needs 18-combo matrix update
- `pkg/controlplane/api/router.go`: Add cache stats/evict routes
- `internal/controlplane/api/handlers/`: Add cache handlers
- `pkg/controlplane/runtime/`: Add cache stats/evict Runtime methods (delegates to per-share BlockStore)
- `cmd/dfsctl/commands/`: Add `cache/` subcommand directory (stats.go, evict.go)
- `pkg/apiclient/`: Add CacheStats/CacheEvict client methods

</code_context>

<deferred>
## Deferred Ideas

- `dfsctl cache evict` with `--force` flag to evict even without remote store (destructive) — future enhancement
- `dfs cache clean` server-side command — decided against in favor of API-only approach
- Per-share cache metrics in Prometheus (hit rate, upload throughput) — future enhancement
- Global cap on total cache usage across all shares — not needed for v4.0
- Migration guide section in docs (v3.8 to v4.0 config format) — not requested

</deferred>

---

*Phase: 49-testing-and-documentation*
*Context gathered: 2026-03-09*
