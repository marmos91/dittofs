---
phase: 11-cas-write-path-gc-rewrite-a2
fixed_at: 2026-04-25T00:00:00Z
review_path: .planning/phases/11-cas-write-path-gc-rewrite-a2/11-REVIEW-2.md
iteration: 2
findings_in_scope: 6
fixed: 6
skipped: 0
status: all_fixed
---

# Phase 11: Code Review Fix Report (Pass 2)

**Fixed at:** 2026-04-25
**Source review:** `.planning/phases/11-cas-write-path-gc-rewrite-a2/11-REVIEW-2.md`
**Iteration:** 2

**Summary:**
- Findings in scope: 6
- Fixed: 6
- Skipped: 0

All Pass-2 findings (1 critical, 1 warning, 4 info) have been resolved. Each
fix is a separate signed commit on the `gsd/phase-11-cas-write-path-a2`
branch. After every fix the targeted package was rebuilt and unit-tested;
the full Phase 11 test scope (`pkg/blockstore/...`,
`pkg/metadata/...`, `pkg/controlplane/runtime/...`,
`internal/adapter/common/...`, `pkg/config/...`,
`internal/controlplane/api/...`) was re-run after the last commit and is
green.

## Fixed Issues

### CR-2-01: `WriteFromRemote` overwrites CAS metadata row with legacy-key zero-hash row

**Files modified:** `pkg/blockstore/local/fs/fs.go`, `pkg/blockstore/local/fs/fs_test.go`
**Commit:** `8545a73c`
**Applied fix:** When `diskIndexLookup` misses (post-restart, or any block
fetched on a node that didn't produce it), `WriteFromRemote` now consults
the FileBlockStore via `lookupFileBlock` and preserves the canonical Hash +
CAS BlockStoreKey from the syncer's earlier registration. Only the
local-cache fields (`State`, `LocalPath`, `LastAccess`) are mutated. A
regression test exercises the cross-restart fetch path and asserts the
metadata row keeps Hash and CAS key intact, and that subsequent
`dispatchRemoteFetch` returns non-zero data.

### WR-2-01: LSL-08 LRU race re-inserts an evicted entry on the post-read `lruTouch`

**Files modified:** `pkg/blockstore/local/fs/chunkstore.go`
**Commit:** `c97ee94f`
**Applied fix:** `ReadChunk` now `os.Stat`s the chunk path before calling
`lruTouch`. If a concurrent `lruEvictOne` unlinked the file between
`os.Open` and the post-read promotion, the stat returns ENOENT and the
re-insert is skipped — eliminating the ghost LRU entry that would
otherwise cause `diskUsed` drift. The open fd still drains correctly via
the deferred `f.Close()`, and the next read for that hash surfaces
`ErrChunkNotFound` under the engine's accept-and-refetch posture
(T-11-B-08), which is functionally correct.

### IN-2-01: GC handler `RunGC` echoes internal `err.Error()` to API client

**Files modified:** `internal/controlplane/api/handlers/block_gc.go`
**Commit:** `669035de`
**Applied fix:** The HTTP 500 response body is now the static string
`"GC failed"`; the underlying error is logged at Debug with the share
name for operator postmortems. Matches the pattern used in `GCStatus` for
non-`ErrShareNotFound` errors. Eliminates leakage of filesystem paths,
DB messages, or reconciler details through the operator-only API.

### IN-2-02: Verifier early-error path closes the body but does not drain

**Files modified:** `pkg/blockstore/remote/s3/verifier.go`
**Commit:** `069424db`
**Applied fix:** `verifyingReader.Close()` now drains up to 16 KiB
(`maxBodyDrainBytes`, the standard Go-stdlib pattern) from the underlying
body before invoking `src.Close()` whenever the caller closes before EOF
(`!v.done`). On the EOF-mismatch path `v.done` is already true and no
drain is needed (the body has already returned io.EOF). This restores
HTTP/1.1 connection-pool reuse for the abandoned-stream case (network
I/O failure mid-read, caller cancellation, etc.), avoiding the burned
TCP connection per failed GET.

### IN-2-03: GC sweep treats `LastModified` clock-skew between server and S3 as operator's problem

**Files modified:** `pkg/config/config.go`
**Commit:** `32d88ef8`
**Applied fix:** `GCConfig.Validate()` now hard-rejects any positive
`grace_period` below 5m with a clear error citing clock-skew absorption,
and emits a `logger.Warn` for values in `[5m, 10m)` so operators on the
recommended-floor edge know they're in the unsafe window. Zero remains
allowed and falls through to the engine's 1h default. The doc comment on
the `GracePeriod` field was updated to match the new contract.

### IN-2-04: No paginated test for `ListByPrefixWithMeta`

**Files modified:** `pkg/blockstore/remote/memory/store_test.go`
**Commit:** `8a48457a`
**Applied fix:** Added `TestStore_ListByPrefixWithMeta_LargePrefix`
(memory backend) seeding 1500 objects under `cas/aa/bb/` and asserting
all 1500 are returned, with `Size`/`LastModified` populated for each.
Memory backend is single-page by construction so the test is a
regression guardrail — if a future refactor caps the in-memory response
or breaks the metadata wiring, the GC sweep's silent under-counting
would surface immediately. The S3-side paginated test remains deferred
(Localstack-heavy, per user note).

## Skipped Issues

None.

---

_Fixed: 2026-04-25_
_Fixer: Claude (gsd-code-fixer)_
_Iteration: 2_
