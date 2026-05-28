# Phase 70: Storage Observability and Quotas - Research

**Researched:** 2026-03-21
**Domain:** Per-share storage quotas, usage tracking, protocol-level space reporting (NFS/SMB), CLI/API observability
**Confidence:** HIGH

## Summary

Phase 70 adds per-share byte quotas with unified enforcement across NFS and SMB, accurate storage consumption tracking (logical + physical), and quota-aware filesystem statistics reporting via both protocols. The existing codebase has substantial infrastructure already in place: `GetFilesystemStatistics()` is already called by both NFS FSSTAT and SMB `FileFsSizeInformation`; `ErrQuotaExceeded` and `ErrNoSpace` error codes already exist with correct mappings to `NFS3ERR_NOSPC` / `NFS4ERR_DQUOT` / `STATUS_DISK_FULL`; the `blockstore.Stats` struct exists with a `UsedSize: 0 // TODO` placeholder; and the `bytesize` package already handles human-readable byte parsing for CLI flags.

The primary work involves: (1) adding a `QuotaBytes` field to the Share model with GORM migration, (2) implementing incremental usage tracking via an atomic counter in the metadata service (avoiding per-FSSTAT full file scans), (3) injecting quota limits into `GetFilesystemStatistics()` so both NFS and SMB see quota-adjusted values, (4) adding quota enforcement in `PrepareWrite` as the single enforcement point, (5) wiring `BlockStore.GetStats().LocalDiskUsed` as physical size through the API, (6) extending CLI/API with `--quota-bytes` flag and quota/usage columns, and (7) removing all hardcoded filesystem statistics fallbacks.

**Primary recommendation:** Implement the incremental usage counter first (STATS-01/02/03), then layer quota enforcement on top of it (QUOTA-01 through QUOTA-05). The usage counter is a prerequisite for both quota checking and accurate FSSTAT reporting.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Quota basis: **logical file sizes** (sum of file sizes as seen by clients). Block overhead/dedup invisible to users
- Enforcement: **hard limit, no grace period**. Once quota reached, writes immediately rejected. In-flight writes that started before the limit are allowed to complete
- Default: **optional, unlimited by default**. QuotaBytes=0 means unlimited. Shares have no quota unless explicitly set
- Hot update: quota changes take effect **immediately** on next write check, no restart needed
- Over-quota (quota reduced below current usage): **block new writes only**. Existing data stays, reads/deletes/renames work normally
- ADS (alternate data streams): **count against quota**. ADS are real data stored as children
- Truncate (shrink): **always allowed** even at quota. Helps users recover from full quota
- File/directory creation (zero-byte): **allowed at quota**. Only data writes blocked
- NFS `df` total: **quota as total size** when quota is set. Standard behavior matching how NFS quotas work everywhere
- No-quota total: **local-only shares -> actual LocalStoreSize capacity; remote-backed shares -> 1 PiB constant**
- Physical size: **exposed via API/CLI only** (not in NFS/SMB protocol responses). Operators see physical_bytes alongside logical used_bytes
- UsedSize calculation: **incremental tracking** via atomic counter in metadata store. Updated on every write/truncate/delete. O(1) per FSSTAT call
- SMB vs NFS values: **identical underlying bytes**. SMB divides by cluster size for AllocationUnits but same source data
- Enforcement point: **MetadataService.PrepareWrite** -- single shared enforcement point for both NFS and SMB
- Partial writes: **reject entire write** if it would push usage over quota. No partial state
- Error codes: **standard protocol errors only** -- NFS3ERR_NOSPC / NFS4ERR_NOSPC / STATUS_DISK_FULL. No extra detail in wire response
- Flag: `--quota-bytes` accepting human-readable values (e.g., '10GiB'). Matches existing `--local-store-size` pattern
- `--quota-bytes 0` means **unlimited** (removes quota). Consistent with LocalStoreSize convention
- `dfsctl share list`: add **Quota** and **Used** columns (e.g., '10 GiB' / 'unlimited' and '3.2 GiB')
- `dfsctl share edit`: **add quota-bytes to interactive prompts** alongside local-store-size and read-buffer-size
- API: **extend GET /api/v1/shares** response with `quota_bytes`, `used_bytes`, `physical_bytes`, `usage_percent` fields
- JSON output includes **usage_percent** for monitoring/alerting convenience
- No RQUOTA protocol -- quotas via FSSTAT/FSINFO and NFSv4 GETATTR
- NFSv4 GETATTR: **implement quota_avail_hard, quota_avail_soft, quota_used** attributes. quota_avail_soft = same as hard (hard limits only)
- **Remove all hardcoded filesystem statistics**: the 1TB defaults in memory metadata store, the hardcoded fallback values in SMB handlers (1M total / 500K available blocks)
- **Replace with quota-aware stats path**: all FSSTAT/FSINFO/FileFsSizeInformation calls go through the unified GetFilesystemStatistics() which is quota-aware
- Used bytes tracked at **metadata store level** -- atomic counter in each metadata store instance, updated on Create/Write/Truncate/Remove
- Physical size from **BlockStore.GetStats()** -- wire LocalDiskUsed through API/CLI as physical_bytes

### Claude's Discretion
- Exact atomic counter implementation details (sync/atomic vs mutex-protected)
- NFSv4 quota attribute bitmap handling
- Migration path for existing shares (initialize counter from file size scan on startup)
- Cluster size constant for SMB allocation unit calculations

### Deferred Ideas (OUT OF SCOPE)
- **RQUOTA protocol (RFC 4559)**: Separate RPC program (100011) for `quota`/`repquota` commands. DittoFS uses per-share quotas (not per-user), so RQUOTA semantics don't map cleanly. FSSTAT + NFSv4 attrs cover `df` which is the primary use case. Defer unless there's demand.
- **Per-user/group quotas**: Explicitly out of scope per REQUIREMENTS.md. AUTH_UNIX is spoofable, massive complexity.
- **Quota alerts/webhooks**: Notify operators when shares approach quota limits. Could be a future observability feature.
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| QUOTA-01 | Per-share byte quota configurable via REST API and dfsctl | Share model `QuotaBytes` field, API request/response extension, CLI `--quota-bytes` flag using existing `bytesize.ParseByteSize()` |
| QUOTA-02 | Write operations rejected with NFS3ERR_NOSPC / STATUS_DISK_FULL when quota exceeded | `PrepareWrite` enforcement using atomic usage counter; `ErrNoSpace` already maps correctly in all protocol handlers (NFS v3/v4/SMB) |
| QUOTA-03 | NFS FSSTAT returns quota-adjusted TotalBytes and AvailableBytes | `GetFilesystemStatistics()` returns quota as TotalBytes when set; FSSTAT handler already consumes this correctly |
| QUOTA-04 | SMB FileFsSizeInformation and FileFsFullSizeInformation return quota-adjusted values | Same `GetFilesystemStatistics()` path; remove hardcoded fallbacks; existing cluster size conversion (4096 bytes) already correct |
| QUOTA-05 | `dfsctl share create/update --quota-bytes` manages quotas | Follow existing `--local-store-size` pattern in create.go/edit.go; extend `apiclient.Share` struct |
| STATS-01 | UsedSize returns actual block storage consumption (not just metadata file sizes) | `BlockStore.Stats()` has `UsedSize: 0 // TODO`; wire `LocalDiskUsed` from `GetStats()` as physical size |
| STATS-02 | Per-share storage usage available via REST API and CLI | Extend `ShareResponse` with `quota_bytes`, `used_bytes`, `physical_bytes`, `usage_percent`; add columns to `share list` |
| STATS-03 | Logical size (file sizes) and physical size (block storage) distinguished | Logical = atomic counter in metadata service (sum of file sizes); Physical = `BlockStoreStats.LocalDiskUsed` from engine |
</phase_requirements>

## Standard Stack

### Core (all already in project)
| Library | Purpose | Why Standard |
|---------|---------|--------------|
| `sync/atomic` | Atomic int64 counter for usage tracking | Lock-free O(1) updates on write/truncate/delete hot path; Go stdlib |
| `gorm.io/gorm` | GORM auto-migration for `QuotaBytes` column | Already used for all control plane persistence |
| `internal/bytesize` | Human-readable byte size parsing (`10GiB`, `500MiB`) | Already used for `--local-store-size` and `--read-buffer-size` |
| `github.com/spf13/cobra` | CLI flag registration for `--quota-bytes` | Already used for all CLI commands |

### Supporting (all already in project)
| Library | Purpose | When to Use |
|---------|---------|-------------|
| `pkg/metadata/errors` | `ErrNoSpace` / `ErrQuotaExceeded` error codes | Already defined with correct protocol mappings |
| `pkg/blockstore/engine` | `BlockStoreStats.LocalDiskUsed` for physical size | Already computed by `GetStats()` |
| `internal/cli/output` | Table rendering for share list with new columns | Already used for all CLI table output |

**No new dependencies required.** Every library and pattern needed for this phase already exists in the project.

## Architecture Patterns

### Recommended Modification Points

```
pkg/
├── controlplane/
│   ├── models/share.go          # ADD: QuotaBytes int64 field
│   ├── runtime/shares/service.go # MODIFY: thread quota to metadata service
│   └── runtime/runtime.go       # MODIFY: expose share stats for API
│
├── metadata/
│   ├── service.go               # MODIFY: GetFilesystemStatistics() → quota-aware
│   ├── io.go                    # MODIFY: PrepareWrite() → quota check
│   ├── types.go                 # No change needed (FilesystemStatistics already sufficient)
│   ├── store/memory/server.go   # MODIFY: add atomic counter, remove 1TB default
│   ├── store/badger/server.go   # MODIFY: add atomic counter, startup scan
│   └── store/postgres/server.go # MODIFY: add atomic counter, startup query
│
├── blockstore/
│   ├── store.go                 # No change to Stats struct needed
│   └── engine/engine.go         # MODIFY: Stats() returns real UsedSize
│
├── apiclient/shares.go          # MODIFY: add quota/usage fields
│
internal/
├── controlplane/api/handlers/shares.go  # MODIFY: add quota fields, wire usage
├── adapter/nfs/v3/handlers/fsstat.go    # MINOR: remove fallback (already works)
├── adapter/nfs/v4/attrs/encode.go       # ADD: FATTR4 quota attributes
├── adapter/smb/v2/handlers/query_info.go # MODIFY: remove hardcoded fallbacks
└── adapter/smb/v2/handlers/converters.go # MODIFY: add ErrQuotaExceeded mapping
│
cmd/dfsctl/commands/share/
├── create.go                    # ADD: --quota-bytes flag
├── edit.go                      # ADD: --quota-bytes flag and interactive prompt
└── list.go                      # MODIFY: add Quota and Used columns
```

### Pattern 1: Incremental Usage Counter (Atomic)
**What:** Each metadata store instance maintains an `atomic.Int64` counter for total used bytes. Updated on every size-changing operation (write commit, truncate, delete, create with size).
**When to use:** Every `GetFilesystemStatistics()` call reads this counter instead of scanning all files.
**Why `sync/atomic` over mutex:** The counter is updated on every write commit (hot path). An atomic Add is ~1ns vs mutex Lock/Unlock at ~25ns. With concurrent writes from multiple NFS/SMB clients, this difference matters.

```go
// In metadata store (memory/badger/postgres):
type MemoryMetadataStore struct {
    // ... existing fields ...
    usedBytes atomic.Int64 // incremental counter for logical file sizes
}

// Updated on write commit:
func (store *MemoryMetadataStore) PutFile(ctx context.Context, file *File) error {
    // ... existing logic ...
    // Track size delta:
    oldSize := previousFile.Size  // from existing file if update
    newSize := file.Size
    if file.Type == FileTypeRegular {
        store.usedBytes.Add(int64(newSize) - int64(oldSize))
    }
    // ... persist ...
}

// O(1) read in GetFilesystemStatistics:
func (store *MemoryMetadataStore) GetFilesystemStatistics(...) (*FilesystemStatistics, error) {
    usedBytes := uint64(store.usedBytes.Load())
    // ... apply quota logic ...
}
```

### Pattern 2: Quota-Aware GetFilesystemStatistics
**What:** The existing `GetFilesystemStatistics()` method is modified to return quota-adjusted values when a quota is configured for the share.
**When to use:** Every FSSTAT (NFS) and FileFsSizeInformation (SMB) call.

```go
// In MetadataService.GetFilesystemStatistics():
func (s *MetadataService) GetFilesystemStatistics(ctx context.Context, handle FileHandle) (*FilesystemStatistics, error) {
    store, err := s.storeForHandle(handle)
    if err != nil {
        return nil, err
    }
    stats, err := store.GetFilesystemStatistics(ctx, handle)
    if err != nil {
        return nil, err
    }

    // Apply quota overlay
    shareName := shareNameForHandle(handle)
    quota := s.getQuotaForShare(shareName)
    if quota > 0 {
        stats.TotalBytes = quota
        if stats.UsedBytes > quota {
            stats.AvailableBytes = 0
        } else {
            stats.AvailableBytes = quota - stats.UsedBytes
        }
    }
    return stats, nil
}
```

### Pattern 3: PrepareWrite Quota Enforcement
**What:** A quota check is added at the beginning of `PrepareWrite()` before validating permissions or building the WriteOperation.
**When to use:** Every NFS WRITE and SMB WRITE operation.

```go
// In MetadataService.PrepareWrite():
func (s *MetadataService) PrepareWrite(ctx *AuthContext, handle FileHandle, newSize uint64) (*WriteOperation, error) {
    // ... existing context check ...

    // Quota check (before file lookup for early rejection)
    shareName := shareNameForHandle(handle)
    quota := s.getQuotaForShare(shareName)
    if quota > 0 {
        store, _ := s.storeForHandle(handle)
        currentUsed := store.GetUsedBytes() // atomic load
        // Get current file size for delta calculation
        file, _ := store.GetFile(ctx.Context, handle)
        delta := int64(0)
        if newSize > file.Size {
            delta = int64(newSize - file.Size)
        }
        if currentUsed + delta > int64(quota) {
            return nil, &StoreError{Code: ErrNoSpace, Message: "share quota exceeded"}
        }
    }

    // ... rest of existing PrepareWrite logic ...
}
```

### Pattern 4: BlockStore UsedSize from LocalDiskUsed
**What:** The `engine.BlockStore.Stats()` method returns the actual `LocalDiskUsed` as `UsedSize` instead of 0.
**When to use:** For physical size reporting via API/CLI.

```go
// In engine/engine.go:
func (bs *BlockStore) Stats() (*blockstore.Stats, error) {
    localStats := bs.local.Stats()
    files := bs.local.ListFiles()
    return &blockstore.Stats{
        UsedSize:     uint64(localStats.DiskUsed), // was: 0 // TODO
        ContentCount: uint64(len(files)),
        TotalSize:    uint64(localStats.MaxDisk),
    }, nil
}
```

### Anti-Patterns to Avoid
- **Scanning all files on every FSSTAT call:** The memory store currently does `for _, attr := range store.files { usedBytes += attr.Attr.Size }`. This must be replaced with the atomic counter read.
- **Per-protocol quota enforcement:** Quota must NOT be checked in NFS or SMB handlers. The single enforcement point is `MetadataService.PrepareWrite()`.
- **Using `ErrQuotaExceeded` for quota enforcement:** Per user decision, use `ErrNoSpace` which maps to the standard NFS3ERR_NOSPC / STATUS_DISK_FULL. `ErrQuotaExceeded` maps to NFS4ERR_DQUOT which is for per-user quotas (not applicable here).
- **Holding locks across GetFilesystemStatistics:** The atomic counter read must be lock-free. Do not hold the store's read lock while building the statistics response.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Byte size parsing | Custom parser for "10GiB" | `bytesize.ParseByteSize()` | Already handles all units (KB, KiB, MB, MiB, GB, GiB, TB, TiB) |
| Byte size display | Custom formatter | `bytesize.ByteSize(n).String()` | Already used for `LocalStoreSize` display |
| Error code mapping | Protocol-specific quota errors | `ErrNoSpace` via existing error chains | NFS v3 xdr/errors.go, NFS v4 types/errors.go, SMB converters.go already map correctly |
| SMB cluster size math | Custom allocation unit calculation | Existing `clusterSize` const (4096) in `converters.go` | `sectorsPerUnit=8`, `bytesPerSector=512` already defined |
| GORM migration | Manual SQL ALTER TABLE | GORM AutoMigrate with new field | Adding `QuotaBytes int64` to Share model triggers automatic column addition |

**Key insight:** The existing codebase has nearly all the building blocks. This phase is primarily wiring existing components together, not building new infrastructure.

## Common Pitfalls

### Pitfall 1: Scanning Performance Regression
**What goes wrong:** Leaving the memory store's `computeStatistics()` file scan in place alongside the new atomic counter, causing two different usage values.
**Why it happens:** The scan is used both by `GetFilesystemStatistics()` and by `computeStatistics()`. If only one is updated, they diverge.
**How to avoid:** Replace ALL usage computation with the atomic counter. Remove the file scan loop entirely from `GetFilesystemStatistics()`. The `computeStatistics()` method should also use the counter.
**Warning signs:** FSSTAT calls taking longer than 1ms, or `df` showing different values at different times.

### Pitfall 2: Counter Drift from Crashes
**What goes wrong:** The atomic counter tracks deltas but is only in memory. If the server crashes mid-operation, the counter may drift from the actual sum of file sizes.
**Why it happens:** The counter is incremented on PutFile but if the server crashes between the PutFile and the counter update, or vice versa, they desynchronize.
**How to avoid:** On metadata store startup, scan all files once to initialize the counter (the "migration path"). This is acceptable because startup is infrequent. For the memory store, the counter is always accurate because both die together on crash.
**Warning signs:** After server restart, `df` shows different usage than before the restart.

### Pitfall 3: TOCTOU in Quota Check
**What goes wrong:** Two concurrent writes both check the quota, both pass, both write, pushing usage over quota.
**Why it happens:** The check (read counter) and the act (increment counter + write) are not atomic.
**How to avoid:** Per the user's decision, accept this as eventual consistency. The atomic counter provides best-effort enforcement. The window is small (nanoseconds) and the overshoot is bounded by maximum write size. For strict enforcement, use `atomic.CompareAndSwap` loop: read current, compute new, CAS to new value, retry if CAS fails.
**Warning signs:** Usage slightly exceeds quota under heavy concurrent writes.

### Pitfall 4: SMB Missing ErrQuotaExceeded Mapping
**What goes wrong:** The SMB error converter in `converters.go` handles `ErrNoSpace` -> `StatusDiskFull` but does NOT handle `ErrQuotaExceeded`. If quota enforcement returns `ErrQuotaExceeded` instead of `ErrNoSpace`, SMB clients get `StatusInternalError`.
**How to avoid:** Use `ErrNoSpace` for share-level quota enforcement (per user decision). If `ErrQuotaExceeded` is ever used, also add the mapping: `case metadata.ErrQuotaExceeded: return types.StatusDiskFull`.
**Warning signs:** SMB clients getting "internal error" instead of "disk full" on quota violations.

### Pitfall 5: NFSv4 GETATTR Quota Attributes Not in Bitmap
**What goes wrong:** `df` on NFSv4 mounts uses GETATTR with `space_total`, `space_free`, `space_avail` attributes, but these are not currently defined in the DittoFS NFSv4 attribute encoder (`encode.go`). Only `FATTR4_SPACE_USED` (per-file) exists.
**Why it happens:** The NFSv4 implementation currently supports file-level attributes but not filesystem-level space attributes. These are different: `space_used` is per-file, while `space_total`/`space_free`/`space_avail` are per-filesystem.
**How to avoid:** Add `FATTR4_SPACE_TOTAL` (bit 59, word 1), `FATTR4_SPACE_FREE` (bit 60), `FATTR4_SPACE_AVAIL` (bit 61) to the attribute encoder. These should call `GetFilesystemStatistics()` when requested. Note: NFSv4 `df` falls back to FSSTAT-like semantics if these attrs are not supported, so this is important for correct reporting.
**Warning signs:** `df` on NFSv4 mounts showing wildly incorrect or zero values for total/available space.

### Pitfall 6: Removing Hardcoded Fallbacks Breaks Error Paths
**What goes wrong:** The SMB handlers have fallback blocks that return hardcoded values (1M total / 500K available) when `GetFilesystemStatistics()` fails. Removing these without ensuring the function never fails causes clients to see errors.
**Why it happens:** `GetFilesystemStatistics()` can fail if the handle is invalid, context is cancelled, or the store is unavailable.
**How to avoid:** Keep a minimal error fallback, but make it accurate. When removing hardcoded values, ensure `GetFilesystemStatistics()` has sensible defaults (e.g., 0 used, large total) for transient errors, OR let the error propagate as a protocol error.
**Warning signs:** SMB clients disconnecting or showing "network error" when accessing shares during transient store issues.

## Code Examples

### Example 1: Share Model QuotaBytes Field
```go
// pkg/controlplane/models/share.go
type Share struct {
    // ... existing fields ...
    ReadBufferSize     int64     `gorm:"default:0;column:read_buffer_size" json:"read_buffer_size"`
    QuotaBytes         int64     `gorm:"default:0;column:quota_bytes" json:"quota_bytes"` // 0 = unlimited
    CreatedAt          time.Time `gorm:"autoCreateTime" json:"created_at"`
    // ...
}
```

### Example 2: API Response with Quota/Usage Fields
```go
// internal/controlplane/api/handlers/shares.go
type ShareResponse struct {
    // ... existing fields ...
    LocalStoreSize     string    `json:"local_store_size,omitempty"`
    ReadBufferSize     string    `json:"read_buffer_size,omitempty"`
    QuotaBytes         string    `json:"quota_bytes,omitempty"`       // human-readable, e.g., "10 GiB"
    UsedBytes          int64     `json:"used_bytes"`                   // logical, in bytes
    PhysicalBytes      int64     `json:"physical_bytes"`               // block store disk usage
    UsagePercent       float64   `json:"usage_percent"`                // 0-100, for monitoring
    CreatedAt          time.Time `json:"created_at"`
    UpdatedAt          time.Time `json:"updated_at"`
}
```

### Example 3: CLI Share List with Quota/Used Columns
```go
// cmd/dfsctl/commands/share/list.go
type shareRow struct {
    Name              string `json:"name"`
    MetadataStore     string `json:"metadata_store"`
    LocalBlockStore   string `json:"local_block_store"`
    RemoteBlockStore  string `json:"remote_block_store"`
    Quota             string `json:"quota"`
    Used              string `json:"used"`
    DefaultPermission string `json:"default_permission"`
}

func (sl ShareList) Headers() []string {
    return []string{"NAME", "METADATA STORE", "LOCAL STORE", "REMOTE STORE", "QUOTA", "USED", "PERMISSION"}
}
```

### Example 4: NFSv4 Filesystem Space Attributes
```go
// internal/adapter/nfs/v4/attrs/encode.go
// Filesystem space attributes (word 1)
const (
    FATTR4_SPACE_TOTAL     = 59 // uint64: total filesystem space
    FATTR4_SPACE_FREE      = 60 // uint64: free filesystem space
    FATTR4_SPACE_AVAIL     = 61 // uint64: available space (may differ from free)
    FATTR4_QUOTA_AVAIL_HARD = 62 // uint64: hard quota remaining
    FATTR4_QUOTA_AVAIL_SOFT = 63 // uint64: soft quota remaining (= hard for us)
    FATTR4_QUOTA_USED       = 64 // uint64: quota used
)
```

### Example 5: Atomic Counter Initialization on Startup
```go
// In metadata store (e.g., badger/store.go):
func (s *BadgerMetadataStore) initUsedBytesCounter(ctx context.Context) error {
    // One-time scan on startup to initialize atomic counter
    var totalUsed int64
    // ... iterate all files, sum sizes of regular files ...
    s.usedBytes.Store(totalUsed)
    return nil
}
```

## State of the Art

| Old Approach | Current Approach | Impact |
|--------------|------------------|--------|
| Full file scan on every FSSTAT | Incremental atomic counter | O(1) per FSSTAT instead of O(n) |
| Hardcoded 1TB default in memory store | Quota-based or capacity-based total | Accurate `df` output |
| Hardcoded 1M/500K blocks in SMB fallbacks | Unified stats path, no fallbacks | Consistent reporting |
| `UsedSize: 0 // TODO` in BlockStore.Stats() | `LocalDiskUsed` from local store | Physical size available |

**Deprecated/outdated:**
- `computeStatistics()` file scan in memory store: Replaced by atomic counter
- Hardcoded fallback values in SMB `query_info.go` (cases 3 and 7): Removed entirely
- `maxStorageBytes` as the sole total for memory store: Quota takes precedence when set

## Open Questions

1. **CAS Loop vs Simple Add for Quota Enforcement**
   - What we know: `atomic.Add` is fastest but allows TOCTOU overruns. `atomic.CompareAndSwap` loop provides strict enforcement but is slower under contention.
   - What's unclear: Whether strict enforcement matters enough to justify CAS overhead.
   - Recommendation: Use simple `atomic.Add` with optimistic check. The overrun is bounded by max write size (~1MB typically) and self-corrects on next FSSTAT. Per user decision: "hard limit, no grace period" but "in-flight writes that started before the limit are allowed to complete" -- this naturally allows small overruns.

2. **NFSv4 GETATTR Quota Attribute Bit Numbers**
   - What we know: RFC 7530 defines quota attributes in the recommended attribute range (bits 56-63 of word 1). The exact bit assignments vary between RFC 7530 (NFSv4.0) and RFC 8881 (NFSv4.1).
   - What's unclear: Whether Linux/macOS NFSv4 clients actually request these attributes for `df`.
   - Recommendation: Implement `FATTR4_SPACE_TOTAL/FREE/AVAIL` first (these are what `df` uses). Add `FATTR4_QUOTA_*` attributes as secondary. If clients don't request quota attrs, FSSTAT/FSINFO fallback handles `df` correctly.

3. **No-Quota Total for Local-Only Shares**
   - What we know: Per decision, local-only shares should report "actual LocalStoreSize capacity" as total. This is `local.Stats().MaxDisk`.
   - What's unclear: How to pass the local store capacity through to `GetFilesystemStatistics()` in the metadata layer, since the metadata service doesn't have direct access to the block store.
   - Recommendation: Pass the capacity as a configuration value when creating the share's metadata store, similar to how `maxStorageBytes` is already used. Or have the MetadataService query the runtime for block store stats.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing + testify (already in project) |
| Config file | None needed -- uses existing `go test` infrastructure |
| Quick run command | `go test ./pkg/metadata/... ./pkg/blockstore/engine/... ./pkg/controlplane/... -count=1 -timeout 60s` |
| Full suite command | `go test ./... -count=1 -timeout 120s` |

### Phase Requirements -> Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| STATS-01 | BlockStore.Stats() returns non-zero UsedSize | unit | `go test ./pkg/blockstore/engine/ -run TestStats_UsedSize -x` | No -- Wave 0 |
| STATS-02 | Per-share usage via REST API | integration | `go test ./internal/controlplane/api/handlers/ -run TestShareList_Usage -x` | No -- Wave 0 |
| STATS-03 | Logical vs physical size distinguished | unit | `go test ./pkg/metadata/... -run TestUsedBytes_Logical -x` | No -- Wave 0 |
| QUOTA-01 | Quota persisted via API/CLI | integration | `go test ./internal/controlplane/api/handlers/ -run TestShareCreate_Quota -x` | No -- Wave 0 |
| QUOTA-02 | Write rejected at quota | unit | `go test ./pkg/metadata/ -run TestPrepareWrite_QuotaEnforcement -x` | No -- Wave 0 |
| QUOTA-03 | NFS FSSTAT quota-adjusted | unit | `go test ./internal/adapter/nfs/v3/handlers/ -run TestFsStat_QuotaAdjusted -x` | No -- Wave 0 |
| QUOTA-04 | SMB FileFsSizeInfo quota-adjusted | unit | `go test ./internal/adapter/smb/v2/handlers/ -run TestQueryInfo_FsSizeQuota -x` | No -- Wave 0 |
| QUOTA-05 | CLI --quota-bytes flag works | unit | `go test ./cmd/dfsctl/commands/share/ -run TestCreate_QuotaFlag -x` | No -- Wave 0 |

### Sampling Rate
- **Per task commit:** `go test ./pkg/metadata/... ./pkg/blockstore/engine/... -count=1 -timeout 60s`
- **Per wave merge:** `go test ./... -count=1 -timeout 120s`
- **Phase gate:** Full suite green before `/gsd:verify-work`

### Wave 0 Gaps
- [ ] `pkg/metadata/service_quota_test.go` -- covers QUOTA-02, QUOTA-03 (PrepareWrite enforcement, GetFilesystemStatistics quota adjustment)
- [ ] `pkg/blockstore/engine/stats_test.go` -- covers STATS-01 (UsedSize from LocalDiskUsed)
- [ ] `internal/controlplane/api/handlers/shares_quota_test.go` -- covers QUOTA-01, STATS-02 (API quota fields, usage in response)
- [ ] `pkg/metadata/store/memory/counter_test.go` -- covers STATS-03 (atomic counter accuracy)

## Sources

### Primary (HIGH confidence)
- DittoFS source code: Direct analysis of all referenced files (see canonical refs in CONTEXT.md)
- `pkg/metadata/errors.go` / `errors/errors.go` -- `ErrNoSpace` (line 33/50) and `ErrQuotaExceeded` (line 34/52-53) already defined
- `internal/adapter/nfs/xdr/errors.go` -- `ErrNoSpace` -> `NFS3ErrNoSpc` mapping (line 105-108)
- `internal/adapter/nfs/v4/types/errors.go` -- `ErrQuotaExceeded` -> `NFS4ERR_DQUOT` mapping (line 47-48)
- `internal/adapter/smb/v2/handlers/converters.go` -- `ErrNoSpace` -> `StatusDiskFull` mapping (line 373-374)
- `pkg/blockstore/engine/engine.go` -- `Stats()` with `UsedSize: 0 // TODO` (line 279), `GetStats()` with `LocalDiskUsed` (line 353)
- `pkg/metadata/store/memory/server.go` -- File scan in `GetFilesystemStatistics()` (lines 130-137), 1TB default (lines 146-148)
- `internal/adapter/smb/v2/handlers/query_info.go` -- Hardcoded fallbacks (lines 860-865, 896-903)
- RFC 1813 Section 3.3.18 -- FSSTAT procedure definition
- RFC 7530 Section 5 -- NFSv4 FATTR4 attribute definitions

### Secondary (MEDIUM confidence)
- `.planning/research/PITFALLS.md` -- Pitfall 6 (TOCTOU), Pitfall 7 (scanning performance), Pitfall 13 (NFS/SMB inconsistency)
- `.planning/research/ARCHITECTURE.md` -- Quota enforcement flow diagram
- `.planning/research/FEATURES.md` -- Quota enforcement at write path

### Tertiary (LOW confidence)
- NFSv4 GETATTR filesystem space attribute bit numbers -- need verification against specific RFC version (7530 vs 8881) for exact bit positions

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all libraries already in use, no new dependencies
- Architecture: HIGH -- all modification points identified and verified in source code
- Pitfalls: HIGH -- verified through source code analysis and existing PITFALLS.md research
- NFSv4 quota attributes: MEDIUM -- bit numbers need RFC verification, but fallback to FSSTAT works

**Research date:** 2026-03-21
**Valid until:** 2026-04-21 (stable domain, no external dependencies changing)
