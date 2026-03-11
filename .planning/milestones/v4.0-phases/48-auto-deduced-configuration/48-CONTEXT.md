# Phase 48: Auto-Deduced Configuration - Context

**Gathered:** 2026-03-09
**Status:** Ready for planning

<domain>
## Phase Boundary

Derive buffer/cache sizes and concurrency settings from system resources (CPU, memory) instead of hardcoded defaults. Auto-deduced values serve as per-share defaults when shares don't specify explicit values. Remove the old global `cache` and `offloader` config sections entirely. Provide startup logging and a CLI command to inspect deduced values.

</domain>

<decisions>
## Implementation Decisions

### Config Field Naming (v4.0 Block Store Terminology)
- `WriteBufferMemory` (from requirements) becomes **`LocalStoreSize`** — max local block storage per share (auto: 25% of available memory)
- `ReadCacheMemory` (from requirements) becomes **`L1CacheSize`** — in-memory L1 read cache per share (auto: 12.5% of available memory)
- `ParallelUploads` becomes **`ParallelSyncs`** — concurrent syncs to remote store (auto: max(4, GOMAXPROCS))
- `ParallelDownloads` becomes **`ParallelFetches`** — concurrent fetches from remote store (auto: max(8, GOMAXPROCS*2))
- `MaxPendingSize` auto-deduced as 50% of LocalStoreSize (backpressure for dirty blocks)
- `PrefetchBlocks` removed as a static config field — replaced with dynamic prefetch (see below)

### Dynamic Prefetch (Replacing Static PrefetchBlocks)
- **Filesystem prefetch** (remote → local store): bounded by remaining LocalStoreSize capacity. Don't aggressively prefetch to disk when local store is nearly full
- **L1 prefetch** (local store → memory): bounded by available L1CacheSize budget. Prefetch into memory based on remaining L1 capacity
- Static PrefetchBlocks config field removed entirely
- Prefetch adapts dynamically to both disk and memory pressure

### Config Structure
- **No global config section** for block store settings — old `cache` and `offloader` YAML sections removed entirely from Config struct
- Auto-deduced values computed once at startup and used as defaults when creating per-share BlockStores
- Per-share overrides via **flat optional fields** on share create/edit API: `--local-store-size`, `--l1-cache-size`, `--parallel-syncs`, `--parallel-fetches`
- Zero value on a share field means "use auto-deduced default"
- Old `cache` and `offloader` sections removed entirely (breaking change, acceptable for experimental project)

### Memory Detection
- **Cgroup-aware with OS fallback**: Check cgroup v2 `memory.max` first (containers/K8s), fall back to OS-level detection
- Linux: `/proc/meminfo` fallback
- macOS: `sysctl hw.memsize`
- Windows: `GlobalMemoryStatusEx` via syscall (build-tagged stub)
- If cgroup v2 `memory.max` is unreadable or set to `max` (unlimited), fall back to OS physical memory
- Log INFO message about which detection source was used

### CPU Detection
- Use `runtime.GOMAXPROCS(0)` directly — already cgroup-aware since Go 1.19+
- No custom cgroup CPU detection needed

### System Detection Package
- New **`internal/sysinfo/`** package with build-tagged OS files:
  - `sysinfo_linux.go` — cgroup v2 → /proc/meminfo
  - `sysinfo_darwin.go` — sysctl hw.memsize
  - `sysinfo_windows.go` — GlobalMemoryStatusEx
  - `sysinfo_other.go` — safe fallback: 4GiB memory, GOMAXPROCS CPUs, WARN log
- Exports: `AvailableMemory() uint64`, `AvailableCPUs() int`

### Deduction Scope
- Per-share budget: 25% of memory is the DEFAULT per-share LocalStoreSize. Multiple shares can each get 25% — user responsibility to tune multi-share setups
- **Floor values only** (no ceiling):
  - LocalStoreSize >= 256MiB
  - L1CacheSize >= 64MiB
  - ParallelSyncs >= 4
  - ParallelFetches >= 8
- Deduction runs **once at server startup**. All shares created during the run use the same deduced defaults

### Auto-Deduction Logic Location
- **`pkg/blockstore/defaults.go`** — functions like `DeduceLocalStoreSize(availableMem)`, `DeduceParallelSyncs(cpus)`
- **`SystemDetector` interface** with `AvailableMemory()` and `AvailableCPUs()` for testability
- Production uses real detector backed by `internal/sysinfo/`; tests inject mock
- Memory detection only (no disk space detection in this phase)

### Startup Visibility
- **INFO log line** at startup: `Auto-deduced defaults: LocalStoreSize=2GiB (25% of 8GiB), L1CacheSize=1GiB (12.5% of 8GiB), ParallelSyncs=8 (CPUs=8), ParallelFetches=16 (CPUs*2=16)`
- **WARN log** when deduced values hit minimum floors: `Low memory detected (512MiB available). LocalStoreSize set to minimum 256MiB. Consider adding more memory or setting explicit values.`
- **Per-share INFO log** on share creation: `Share /export: LocalStoreSize=4GiB (override, default=2GiB), ParallelSyncs=8 (auto-deduced)` — shows which values are custom vs auto

### CLI: `dfs config show --deduced`
- Add `--deduced` flag to existing `dfs config show` command
- Output: YAML with inline comments showing deduced values and their source
- Header section with detected system info: `# System: 8 CPUs, 16GiB memory (cgroup: 8GiB limit)`
- Shows computation: `# Deduction: LocalStoreSize=25% of 8GiB, L1CacheSize=12.5% of 8GiB, ...`

### Config Template Update
- Update `dfs config init` generated template to remove old cache/offloader sections
- Add comment explaining auto-deduction and per-share overrides
- Users see the new model immediately on fresh installs

### Testing Strategy
- **Injected `SystemDetector` interface** for testability
- Test scenarios (core + edge cases):
  - Normal machine (8GiB / 8 CPU)
  - Small machine (512MiB / 1 CPU) — hits floor values
  - Large machine (256GiB / 64 CPU)
  - User override preserves explicit values
  - Partial override (some auto, some manual)
  - Cgroup-limited container (physical 64GiB but cgroup limit 4GiB)
- Tests for `internal/sysinfo/` per-platform detection (build-tagged test files)

### Documentation
- All doc updates (ARCHITECTURE.md, CONFIGURATION.md, CLAUDE.md) deferred to Phase 49

### Claude's Discretion
- Exact SystemDetector interface method signatures and return types
- Internal implementation of cgroup v2 memory.max parsing
- Build tag syntax and file organization within internal/sysinfo/
- Error message wording for detection failures
- Exact YAML comment format in `--deduced` output
- Test helper structure and assertion patterns

</decisions>

<specifics>
## Specific Ideas

- "Syncing" and "fetching" terminology from Phase 41 carries through: ParallelSyncs (to remote), ParallelFetches (from remote)
- Dynamic prefetch idea: filesystem prefetch bounded by remaining LocalStoreSize, L1 prefetch bounded by remaining L1CacheSize — adapts to actual pressure rather than static count
- MaxPendingSize at 50% of LocalStoreSize — natural backpressure scaling
- User emphasized removing old "cache" terminology entirely — v4.0 is about local/remote block stores

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/config/defaults.go` — existing `ApplyDefaults()` pattern, will be simplified (remove cache/offloader sections)
- `pkg/config/config.go:CacheConfig` and `OffloaderConfig` — structs to remove
- `internal/bytesize/` — byte size parsing and formatting, used for LocalStoreSize and L1CacheSize
- `cmd/dfs/commands/config/` — existing config subcommands, add `--deduced` flag here

### Established Patterns
- Build-tagged files: already used in `pkg/config/config.go` (runtime.GOOS for config dir detection)
- `ApplyDefaults()` pattern: zero values replaced with defaults, explicit values preserved
- Config validation via `Validate()` function after defaults applied
- Viper + mapstructure decode hooks for custom types (ByteSize, Duration)

### Integration Points
- `pkg/config/defaults.go` — remove `applyCacheDefaults()` and `applyOffloaderDefaults()`
- `pkg/config/config.go` — remove `CacheConfig` and `OffloaderConfig` structs
- `pkg/config/defaults_test.go` — remove cache/offloader default tests, add auto-deduction tests
- `cmd/dfs/commands/config/show.go` — add `--deduced` flag
- `cmd/dfs/commands/start.go` — log deduced values at startup
- `pkg/blockstore/defaults.go` — new file with deduction functions
- `pkg/controlplane/runtime/blockstores/` (from Phase 46) — uses deduced defaults in EnsureBlockStore

</code_context>

<deferred>
## Deferred Ideas

- Disk space detection for LocalStoreSize — not needed for v4.0, memory budget is sufficient
- Dynamic re-detection if cgroup limits change mid-run — deferred, startup-once is sufficient
- Server-wide memory budget split among shares — user responsibility for now, could add later if needed
- Property-based testing for deduction logic — standard table-driven tests are sufficient
- Documentation updates (ARCHITECTURE.md, CONFIGURATION.md, CLAUDE.md) — Phase 49

</deferred>

---

*Phase: 48-auto-deduced-configuration*
*Context gathered: 2026-03-09*
