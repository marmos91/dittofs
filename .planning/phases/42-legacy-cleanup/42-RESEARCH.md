# Phase 42: Legacy Cleanup - Research

**Researched:** 2026-03-09
**Domain:** Go codebase dead code removal (DirectWriteStore, filesystem payload store)
**Confidence:** HIGH

## Summary

Phase 42 is a pure deletion phase removing the `DirectWriteStore` interface, the filesystem payload store (`pkg/payload/store/fs/`), and all direct-write code paths from the cache, offloader, and runtime initialization. The codebase is mature and all target code is well-identified from the CONTEXT.md discussion. No new features, no architectural changes -- just removing dead code left over from the legacy filesystem backend that was replaced by the two-tier Local/Remote block store model.

The changes span approximately 15 source files plus 2 files to delete entirely. The key risk is stale import references or missed conditional branches that break `go build ./...`. The verification is straightforward: successful compilation and passing tests.

**Primary recommendation:** Execute as a single atomic plan -- delete `pkg/payload/store/fs/` directory, remove DirectWriteStore interface, strip all direct-write branches from cache/offloader/init.go, clean up E2E tests and CLI, and sweep comments. Verify with `go build ./...` && `go test ./...`.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Delete `DirectWriteStore` interface from `pkg/payload/store/store.go`
- Delete `pkg/payload/store/fs/` directory entirely (store.go + store_test.go)
- Remove `directWritePath` field, `SetDirectWritePath()`, and `IsDirectWrite()` from cache struct and methods
- Remove `IsDirectWrite()` checks from offloader (offloader.go, upload.go)
- Remove `blockfs` import and `DirectWriteStore` detection logic from `init.go`
- Remove all `if bc.directWritePath != nil` branches from cache (read.go, write.go, flush.go, cache.go)
- Keep non-direct-write code path only -- it becomes the sole path
- Remove `"filesystem"` case from E2E test matrices (store_matrix_test.go, nfsv4_store_matrix_test.go) -- 3 matrix entries removed
- Remove filesystem test helpers (temp dir creation for fs payload stores)
- Delete filesystem CRUD integration tests from payload_stores_test.go entirely
- Rewrite nfsv4_recovery_test.go to use memory stores instead of filesystem stores
- Remove `"filesystem"` case from CLI commands (dfsctl store payload add/edit)
- Update CLI help text to no longer list "filesystem" as a valid store type
- Remove "filesystem" from all comments, doc strings, and error type docs (errors.go Backend field)
- Clean all interface docs on BlockStore to only mention memory and S3
- In init.go: explicit `case "filesystem"` returns helpful error: "payload store type 'filesystem' removed in v4.0 -- use 'memory' or 's3'"
- Also keep generic `default` case for truly unknown types
- Error surfaces at startup during store creation (not config validate)
- CLI: remove `"filesystem"` case entirely -- default/unknown handler rejects it
- Full sweep for "direct write", "directwrite", "filesystem backend", "fs store" references in comments
- Update flush.go header comments to describe current behavior (cache-only writes, offloader handles remote sync)
- Remove directWritePath field comment from cache struct
- No trace of filesystem/direct-write left in codebase
- Fix cache_test.go `WriteDownloaded` -> `WriteFromRemote` breakage from Phase 41 rename (ALREADY DONE -- verified in current code)
- No new tests -- pure deletion phase
- Verification: `go build ./...` && `go test ./...`
- Single plan, single atomic commit
- Commit message: "refactor(42): remove DirectWriteStore and filesystem payload store"

### Claude's Discretion
- Exact ordering of file edits within the single plan
- Whether any additional dead code surfaces during removal (follow the dependency chain)
- Minor wording adjustments on updated comments

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| CLEAN-01 | DirectWriteStore interface removed from pkg/payload/store/store.go | Lines 51-60 of store.go: remove `DirectWriteStore` interface and its doc comment (10 lines) |
| CLEAN-02 | pkg/payload/store/fs/ entirely deleted | Directory contains store.go (377 lines) + store_test.go (548 lines). `rm -rf` |
| CLEAN-03 | directWritePath, SetDirectWritePath, IsDirectWrite removed from cache | cache.go lines 69-73 (field), lines 111-123 (two methods); also GetDirtyBlocks lines 450-452 guard |
| CLEAN-04 | IsDirectWrite checks removed from offloader | offloader.go lines 152-153 (Flush); upload.go lines 31-33 (uploadPendingBlocks) |
| CLEAN-05 | blockfs import and DirectWriteStore detection removed from init.go | init.go line 21 (import), lines 215-231 (detection block + fallback); replace `case "filesystem"` at lines 294-299 with error |
| CLEAN-06 | All direct-write branches removed from cache operations | read.go lines 100-104; write.go lines 140-147 + lines 200-202; flush.go lines 108-114 + lines 163-166 + line 180 |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| Go | 1.25.5 | Language runtime | Project language |
| go build | built-in | Compilation verification | Standard Go toolchain |
| go test | built-in | Test verification | Standard Go toolchain |
| go vet | built-in | Static analysis | Standard Go toolchain |

### Supporting
No additional libraries needed. This is a pure deletion phase -- no new dependencies.

## Architecture Patterns

### Recommended Deletion Order

The safest deletion order minimizes intermediate broken states:

```
1. Delete pkg/payload/store/fs/ directory entirely
2. Remove DirectWriteStore interface from store.go
3. Remove blockfs import and DirectWriteStore detection from init.go
   (replace "filesystem" case with error, keep default case)
4. Remove directWritePath field and methods from cache.go
5. Remove direct-write branches from read.go, write.go, flush.go
6. Remove IsDirectWrite() checks from offloader.go and upload.go
7. Clean up E2E test matrices (store_matrix_test.go, nfsv4_store_matrix_test.go)
8. Remove filesystem tests from payload_stores_test.go
9. Rewrite nfsv4_recovery_test.go to use memory stores
10. Remove offloader_test.go filesystem test helpers and tests
11. Clean CLI commands (add.go, edit.go)
12. Sweep comments and documentation (errors.go, README.md, flush.go header)
```

### Pattern: Replace With Error vs Remove

Two patterns for handling the `"filesystem"` case across the codebase:

**In init.go (server-side):** Replace with explicit error case
```go
case "filesystem":
    return nil, fmt.Errorf("payload store type 'filesystem' removed in v4.0 -- use 'memory' or 's3'")
```

**In CLI (client-side):** Remove the case entirely; let `default` handle it
```go
default:
    return nil, fmt.Errorf("unknown store type: %s (supported: memory, s3)", storeType)
```

### Anti-Patterns to Avoid
- **Leaving orphan imports:** After deleting the fs package, the `blockfs` import in init.go will fail compilation. Must remove in same commit.
- **Missing comment sweep:** Old comments referencing "filesystem" or "direct write" create confusion for future developers.
- **Partial branch removal:** Removing the direct-write check but leaving `isDirect` variable creates dead code warnings.

## Don't Hand-Roll

Not applicable -- this is a deletion phase. No new code needed.

## Common Pitfalls

### Pitfall 1: Orphan Imports After Deletion
**What goes wrong:** Deleting `pkg/payload/store/fs/` but forgetting the `blockfs` import alias in `init.go` causes compilation failure.
**Why it happens:** Go enforces no unused imports.
**How to avoid:** Delete directory AND remove all import references in the same commit.
**Warning signs:** `go build ./...` fails with "imported and not used" error.

### Pitfall 2: Offloader Integration Tests Still Import fs
**What goes wrong:** `pkg/payload/offloader/offloader_test.go` imports `"github.com/marmos91/dittofs/pkg/payload/store/fs"` (line 24) and uses `fs.NewWithPath` in `newFilesystemEnv` and `newFilesystemEnvForBench`.
**Why it happens:** The integration test file creates filesystem test environments alongside memory and S3.
**How to avoid:** Remove the `fs` import, the `newFilesystemEnv` function, the `newFilesystemEnvForBench` function, and all test/benchmark functions that call them (10+ functions).
**Warning signs:** Build tag `integration` tests fail with missing import.

### Pitfall 3: E2E Test storeMatrix Slice Uses storeConfig Type
**What goes wrong:** The `storeMatrix` var in `store_matrix_test.go` is a `[]storeConfig` literal. The `nfsv4_store_matrix_test.go` reuses the same `storeMatrix` variable. Removing entries from the slice in one file affects both test files.
**Why it happens:** Shared variable across test files in the same package.
**How to avoid:** The `storeMatrix` variable is defined in `store_matrix_test.go` (line 30). Remove the 3 filesystem entries there and both test files are updated.

### Pitfall 4: Recovery Test Uses Filesystem for Persistence
**What goes wrong:** `nfsv4_recovery_test.go` `TestServerRestartRecovery` uses `"filesystem"` payload store type because it needs data to persist across server restarts.
**Why it happens:** Filesystem and badger are the persistent backends; memory is ephemeral.
**How to avoid:** Per CONTEXT.md decision: switch to memory stores. This changes the recovery test to verify restart behavior with ephemeral stores (like `TestStaleNFSHandle` already does). The test verifies the server restart flow, not data persistence specifically.
**Warning signs:** Test logic changes meaning -- review carefully.

### Pitfall 5: Missed Direct-Write Guard in GetDirtyBlocks
**What goes wrong:** `cache.go:GetDirtyBlocks` (line 450) has a direct-write early return: `if bc.directWritePath != nil { return nil, nil }`. If this guard is left in, the function will never early-return for S3 backends (which is correct), but if `directWritePath` field is removed, the code won't compile.
**Why it happens:** This guard is in `cache.go`, not in the more obvious `read.go/write.go/flush.go` files.
**How to avoid:** Remove the guard along with the field removal.

### Pitfall 6: flush.go Header Comment References Direct-Write
**What goes wrong:** The Flush function's doc comment (line 22-25) mentions "For FS backends (directWritePath set), fsync guarantees durability."
**Why it happens:** Historical comment from when both code paths existed.
**How to avoid:** Rewrite the Flush doc comment to describe only the current behavior (cache-only writes + offloader sync).

### Pitfall 7: offloader Flush Doc Comment References FS Backend
**What goes wrong:** The `offloader.go:Flush` function has a doc comment (lines 135-144) mentioning "For FS backends (direct-write): data is already in the payload store via pwrite, so this is a no-op."
**Why it happens:** Historical code path documentation.
**How to avoid:** Remove the FS backend paragraph from the Flush doc comment.

## Code Examples

### Exact Lines to Remove from cache.go

```go
// REMOVE: field declaration (lines 69-73)
directWritePath func(payloadID string, blockIdx uint64) string

// REMOVE: SetDirectWritePath method (lines 111-117)
func (bc *BlockCache) SetDirectWritePath(fn func(payloadID string, blockIdx uint64) string) {
    bc.directWritePath = fn
}

// REMOVE: IsDirectWrite method (lines 119-123)
func (bc *BlockCache) IsDirectWrite() bool {
    return bc.directWritePath != nil
}

// REMOVE: direct-write guard in GetDirtyBlocks (lines 447-452)
if bc.directWritePath != nil {
    return nil, nil
}
```

### Exact Lines to Remove from read.go

```go
// REMOVE: direct-write path resolution (lines 97-104)
// Replace with: just the lookupFileBlock path (lines 105-120)
var path string
if bc.directWritePath != nil {
    if p := bc.directWritePath(payloadID, blockIdx); p != "" {
        path = p
    }
}
if path == "" {
    // ... lookupFileBlock path stays
```

After removal, the function simply uses `lookupFileBlock` always. Remove the `var path string` declaration before the `if bc.directWritePath`, and remove the `if path == ""` wrapper around the lookupFileBlock block.

### Exact Lines to Remove from write.go

```go
// REMOVE: direct payload store path resolution (lines 137-147)
var path string
var isDirect bool
if bc.directWritePath != nil {
    if p := bc.directWritePath(payloadID, blockIdx); p != "" {
        path = p
        isDirect = true
    }
}
if path == "" {
    path = bc.blockPath(blockID)
}
// REPLACE WITH:
path := bc.blockPath(blockID)

// REMOVE: isDirect branch in state setting (lines 200-202)
if isDirect {
    fb.State = metadata.BlockStateRemote
    fb.BlockStoreKey = FormatStoreKey(payloadID, blockIdx)
} else if fb.State == 0 {
// REPLACE WITH:
if fb.State == 0 {
```

### Exact Lines to Remove from flush.go

```go
// REMOVE: direct payload store path resolution in flushBlock (lines 106-117)
var path string
var isDirect bool
if bc.directWritePath != nil {
    if p := bc.directWritePath(payloadID, blockIdx); p != "" {
        path = p
        isDirect = true
    }
}
if path == "" {
    path = bc.blockPath(blockID)
}
// REPLACE WITH:
path := bc.blockPath(blockID)

// REMOVE: isDirect branch in state setting (lines 163-168)
if isDirect {
    fb.State = metadata.BlockStateRemote
    fb.BlockStoreKey = FormatStoreKey(payloadID, blockIdx)
} else {
    fb.State = metadata.BlockStateLocal
}
// REPLACE WITH:
fb.State = metadata.BlockStateLocal

// REMOVE: isDirect check for diskUsed tracking (line 180)
if !isDirect {
    bc.diskUsed.Add(int64(dataSize) - prevDiskSize)
}
// REPLACE WITH:
bc.diskUsed.Add(int64(dataSize) - prevDiskSize)
```

### init.go: Replace filesystem case with error

```go
// BEFORE (lines 294-299):
case "filesystem":
    path, ok := config["path"].(string)
    if !ok || path == "" {
        return nil, fmt.Errorf("filesystem payload store requires path")
    }
    return blockfs.New(blockfs.Config{BasePath: path})

// AFTER:
case "filesystem":
    return nil, fmt.Errorf("payload store type 'filesystem' removed in v4.0 -- use 'memory' or 's3'")
```

Also remove the entire DirectWriteStore detection block (lines 211-231):
```go
// REMOVE: entire if block
if dws, ok := blockStore.(blockstore.DirectWriteStore); ok {
    bc.SetDirectWritePath(func(payloadID string, blockIdx uint64) string {
        storeKey := cache.FormatStoreKey(payloadID, blockIdx)
        path, err := dws.BlockFilePath(storeKey)
        if err != nil {
            return "" // Fall back to cache path
        }
        return path
    })
    logger.Info("Direct-write optimization enabled for filesystem payload backend")
} else {
    bc.SetSkipFsync(true)
    logger.Info("S3 cache optimization: fsync skipped (durability via S3)")
}
```

Replace with just:
```go
bc.SetSkipFsync(true)
```

And remove the `blockfs` import (line 21) and `blockstore` import alias if no longer needed (check if other code uses `blockstore.BlockStore` -- yes it does at line 284, so keep that import).

### E2E storeMatrix: Remove 3 entries

```go
// REMOVE these 3 entries from storeMatrix in store_matrix_test.go:
{"memory", "filesystem"},   // MTX-02
{"badger", "filesystem"},   // MTX-05
{"postgres", "filesystem"}, // MTX-08
```

### offloader_test.go: Remove filesystem functions

Remove these functions and their callers:
- `newFilesystemEnv` (lines 84-118)
- `newFilesystemEnvForBench` (lines 595-619)
- `TestOffloader_WriteAndFlush_Filesystem` (line 425-428)
- `TestOffloader_DownloadOnCacheMiss_Filesystem` (lines 486-489)
- `TestOffloader_Deduplication_Filesystem` (lines 883-886)
- `BenchmarkUpload_Filesystem` (lines 627-630)
- `BenchmarkDownload_Filesystem` (lines 637-640)
- `BenchmarkFlush_Filesystem` (lines 645-648)
- `BenchmarkConcurrentUpload_Filesystem` (lines 653-657)
- `BenchmarkLargeFile_16MB_Filesystem` (lines 673-677)
- `BenchmarkLargeFile_64MB_Filesystem` (lines 678-681)
- `BenchmarkSequentialWrite_32KB_Filesystem` (lines 693-697)

Remove the `fs` import (line 24): `"github.com/marmos91/dittofs/pkg/payload/store/fs"`

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| Direct-write to filesystem payload store | Cache + async offloader to S3/memory | v4.0 Phase 42 | Eliminates dead code, simplifies cache logic |
| Three backend types (memory, filesystem, s3) | Two backend types (memory, s3) | v4.0 Phase 42 | Simpler configuration, fewer test permutations |
| `DirectWriteStore` interface for FS optimization | N/A (removed) | v4.0 Phase 42 | One fewer interface to maintain |

**Deprecated/outdated after this phase:**
- `DirectWriteStore` interface: Completely removed
- Filesystem payload store: Completely removed
- Direct-write cache path: Completely removed
- `pkg/payload/store/fs/` package: Deleted entirely

## Open Questions

None. All decisions are locked in CONTEXT.md. The scope is well-defined and the code is fully identified.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (built-in), go 1.25.5 |
| Config file | go.mod (project root) |
| Quick run command | `go build ./...` |
| Full suite command | `go build ./... && go test ./...` |

### Phase Requirements -> Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| CLEAN-01 | DirectWriteStore interface removed | compilation | `go build ./pkg/payload/store/...` | N/A (deletion) |
| CLEAN-02 | pkg/payload/store/fs/ deleted | compilation | `go build ./...` (fails if referenced) | N/A (deletion) |
| CLEAN-03 | Cache direct-write methods removed | compilation + unit | `go test ./pkg/cache/...` | Existing cache_test.go |
| CLEAN-04 | Offloader direct-write checks removed | compilation | `go build ./pkg/payload/offloader/...` | Existing offloader_test.go |
| CLEAN-05 | init.go blockfs import removed | compilation | `go build ./pkg/controlplane/runtime/...` | N/A (compilation) |
| CLEAN-06 | Cache direct-write branches removed | compilation + unit | `go test ./pkg/cache/...` | Existing cache_test.go |

### Sampling Rate
- **Per task commit:** `go build ./... && go test ./...`
- **Per wave merge:** `go build ./... && go test ./...`
- **Phase gate:** Full suite green before verification

### Wave 0 Gaps
None -- existing test infrastructure covers all phase requirements. This is a deletion phase that reduces test code, not adds it.

## Sources

### Primary (HIGH confidence)
- Direct source code analysis of all 15+ files listed in CONTEXT.md
- All code locations verified by reading actual source files
- Line numbers cross-referenced with current codebase state

### Secondary (MEDIUM confidence)
- CONTEXT.md decisions from user discussion (locked constraints)
- REQUIREMENTS.md CLEAN-01 through CLEAN-06 definitions

### Tertiary (LOW confidence)
- None -- all findings verified against actual source code

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - pure Go deletion, no external dependencies
- Architecture: HIGH - all target code identified and line numbers verified
- Pitfalls: HIGH - comprehensive grep search identified all references; offloader_test.go filesystem dependency discovered during research

**Research date:** 2026-03-09
**Valid until:** Indefinite (codebase-specific research, not library-version-dependent)
