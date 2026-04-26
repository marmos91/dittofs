# Phase 11: CAS write path + GC rewrite (A2) — Pattern Map

**Mapped:** 2026-04-25
**Files analyzed:** 27 (4 NEW, 17 MODIFY, 6 DOCS/CONFIG)
**Analogs found:** 27 / 27 (every Phase 11 target has an in-tree analog post-Phase-10)

---

## File Classification

### NEW files (PR-A: write/state, PR-B: nothing new, PR-C: GC + CLI/REST + tests)

| New File | Role | Data Flow | Closest Analog | Match Quality |
|----------|------|-----------|----------------|---------------|
| `pkg/blockstore/engine/syncer_crash_test.go` | unit test (deterministic crash injection) | event-driven (kill-points) | `pkg/blockstore/engine/syncer_put_error_test.go:27-96` (`failingPutFileBlockStore`) | exact (same fault-injection wrapper pattern, extend with kill-point indices) |
| `pkg/blockstore/engine/gcstate.go` (new — disk-backed live set) | utility (gc-state persistence) | batch I/O | `pkg/blockstore/local/fs/appendwrite.go:45-80` (`getOrCreateLog` per-payload Badger-temp pattern) + Badger usage in `pkg/metadata/store/badger/objects.go:46-105` | partial (no exact analog — Badger temp-store pattern is new; closest is fs.go's directory-backed per-payload state) |
| `test/e2e/cas_immutable_overwrites_test.go` (`TestBlockStoreImmutableOverwrites`) | e2e test (canonical correctness) | request-response over real S3 | `test/e2e/...` existing E2E + Localstack fixture (already used per CONTEXT.md `<code_context>`) | role-match (no existing CAS-immutability E2E; pattern from existing Localstack-backed E2E) |
| `test/e2e/x_amz_meta_content_hash_test.go` (BSCAS-06 D-33) | e2e test (external-verifier sanity) | head-object verification | same Localstack fixture as above | role-match |
| `cmd/dfsctl/commands/store/block/gc.go` (`dfsctl store block gc <share> [--dry-run]`) | CLI command | request-response (REST POST) | `cmd/dfsctl/commands/store/block/evict.go:13-98` (entire file) | exact (same cobra+apiclient pattern, swap evict for gc) |
| `cmd/dfsctl/commands/store/block/gc_status.go` | CLI command | request-response (REST GET) | `cmd/dfsctl/commands/store/block/evict.go:13-98` + `cmd/dfsctl/commands/store/block/health.go` | exact |
| (REST handler endpoint) `internal/controlplane/api/handlers/block_stores.go` extension — `POST /api/v1/store/block/{name}/gc` + `GET /api/v1/store/block/{name}/gc-status` | REST handler | request-response | `internal/controlplane/api/handlers/block_stores.go:30-130` (`Create` / `Evict` handlers) — same file already wraps `runtime.Runtime`-rooted operations | in-place add (extend BlockStoreHandler) |

### MODIFY files

| Modified File | Role | Data Flow | Closest Analog (intra-file pattern) | Match Quality |
|---------------|------|-----------|--------------------------------------|---------------|
| `pkg/blockstore/types.go` | type / parser declaration | N/A | `pkg/blockstore/types.go:185-205` (`FormatStoreKey` / `ParseStoreKey`) — symmetric pair to mirror as `FormatCASKey`/`ParseCASKey` | exact (D-29 explicitly mirrors this pair) |
| `pkg/blockstore/types.go:64-83` (BlockState collapse) | enum | N/A | self (existing 4-state enum + `String()` method) | in-place rewrite |
| `pkg/blockstore/types.go:111-153` (FileBlock new fields) | struct | N/A | self (existing `FileBlock` struct + `NewFileBlock` constructor) | in-place add (`State` already exists from Phase 10; add `LastSyncAttemptAt time.Time`) |
| `pkg/blockstore/engine/syncer.go` (full upload-path rewrite) | service (orchestrator) | event-driven periodic + claim-batch | self post-Phase-10 (already at `engine/syncer.go`) — `periodicUploader` (lines 463-506) and `SyncNow` (lines 404-461) keep the shape, swap internals | in-place rewrite |
| `pkg/blockstore/engine/upload.go` (the actual upload + state-transition body — to be largely replaced or moved into `syncer.go`) | service | streaming I/O | self (`syncFileBlock` at `engine/upload.go:72-141` — the canonical analog for the rewrite target) | full replace |
| `pkg/blockstore/engine/engine.go` (dual-read resolver + verification entry) | orchestrator | request-response read | `pkg/blockstore/engine/engine.go:174-178, 553-640` (`ReadAt`, `readAtInternal`, `ensureAndReadFromLocal`) | in-place edit (insert dual-read decision before fetch) |
| `pkg/blockstore/engine/gc.go` (full mark-sweep rewrite) | service | batch enumeration + parallel sweep | self (`CollectGarbage` at lines 71-192) — keeps `GCStats`/`Options` shape, replaces algorithm | full replace |
| `pkg/blockstore/remote/s3/store.go:222-239` (`WriteBlock` add metadata header) | adapter (remote store) | request-response | self (existing `WriteBlock`) | in-place edit (add `Metadata: map[string]string{...}` to `PutObjectInput`) |
| `pkg/blockstore/remote/s3/store.go:241-261` (`ReadBlock` streaming verify) | adapter | streaming I/O | self (existing `ReadBlock`) + `readResponseBody` helper | in-place edit (wrap `resp.Body` in BLAKE3 verifier `io.Reader`; pre-check `resp.Metadata`) |
| `pkg/blockstore/local/local.go:49-164` (LocalStore narrowing) | interface | N/A | self (current 22-method interface) | shrink (delete 5 named methods per LSL-07) |
| `pkg/blockstore/local/fs/flush.go:94-169` (TD-09 stage+release) | service (write-path I/O) | file I/O under lock | self (current `flushBlock` body) | in-place edit (move `os.OpenFile`/`f.Write` outside `mb.mu`) |
| `pkg/blockstore/local/fs/eviction.go` (LSL-08 in-process LRU eviction) | utility | LRU eviction | `pkg/blockstore/local/fs/eviction.go:14-100` (`ensureSpace` + `collectRemoteCandidates`) | in-place edit (key by ContentHash, drop FileBlockStore lookups on hot path) |
| `pkg/metadata/store.go` (FileBlockStore alias point) + `pkg/blockstore/store.go:12-53` (FileBlockStore interface itself) — add `EnumerateFileBlocks(ctx, fn func(ContentHash) error) error` | interface | event-driven cursor | `pkg/blockstore/store.go:35-52` (`ListLocalBlocks` / `ListRemoteBlocks` / `ListUnreferenced`) — pagination-style methods are the closest existing pattern | role-match (existing methods return slices; new method takes a callback for streaming) |
| `pkg/metadata/store/badger/objects.go` — implement `EnumerateFileBlocks` | store impl | iterator | `pkg/metadata/store/badger/objects.go:266-316` (`ListLocalBlocks` Badger prefix-iterator pattern) | exact (Badger `Iterator.Seek/Next` over `fb:` prefix) |
| `pkg/metadata/store/postgres/objects.go` — implement `EnumerateFileBlocks` | store impl | server-side cursor | (existing `List*Blocks` Postgres impls) | role-match (server-side cursor with batched fetch) |
| `pkg/metadata/store/memory/objects.go` — implement `EnumerateFileBlocks` | store impl | direct map iteration | (existing memory `List*Blocks` impls) | exact (range over map) |
| `pkg/metadata/storetest/file_block_ops.go` (extend) | conformance test | table-driven | `pkg/metadata/storetest/file_block_ops.go:65-98` (`testListLocalBlocks` style) | exact (add `testEnumerateFileBlocks_Empty/Single/LargeFanout/MidIterationError`) |
| `pkg/blockstore/local/localtest/suite.go` (delete `testMarkBlockState` + `testGetDirtyBlocks`; add LSL-08 eviction tests) | conformance test | unit | `pkg/blockstore/local/localtest/suite.go:332-365` (`testMarkBlockState`) — to delete; `pkg/blockstore/local/localtest/appendlog_suite.go:39-45` (5-scenario sub-test pattern) — for new LSL-08 tests | exact pattern reuse |
| `pkg/controlplane/runtime/blockgc.go` (cross-share enumeration entry — already in place) | runtime entry | N/A | self — `RunBlockGC` (lines 25-71) already iterates `DistinctRemoteStores()` per D-03 | reuse as-is (no change required if it covers Phase 11 semantics) |
| `pkg/controlplane/runtime/shares/service.go:898-949` (`DistinctRemoteStores`) | runtime entry | N/A | self — already exists (post-Phase-08) | reuse as-is |
| `pkg/config/config.go` — new gc.* and syncer.* knobs | config schema | N/A | self (existing knob declarations) | in-place add |

### DOCS

| Doc | Section |
|-----|---------|
| `docs/ARCHITECTURE.md` | Replace path-prefix GC desc with mark-sweep; add 3-state lifecycle diagram; CAS dual-read window; `gc-state/` dir |
| `docs/FAQ.md` | "How does GC work in v0.15.0?", "What is the dual-read window?", "Residual `{payloadID}/block-…` keys after upgrade?" |
| `docs/IMPLEMENTING_STORES.md` | `MetadataStore.EnumerateFileBlocks(ctx, fn)` cursor contract; `RemoteStore` PUT-with-metadata-headers requirement (BSCAS-06) |
| `docs/CONFIGURATION.md` | `gc.interval`, `gc.sweep_concurrency`, `gc.grace_period`, `gc.dry_run_sample_size`, `syncer.upload_concurrency`, `syncer.claim_batch_size`, `syncer.claim_timeout` |
| `docs/CLI.md` | `dfsctl store block gc <share> [--dry-run]` + `dfsctl store block gc-status <share>` |
| `docs/CONTRIBUTING.md` | (Claude's discretion per D-34) "Adding a new metadata-backend `EnumerateFileBlocks`" recipe |

---

## Pattern Assignments (depth on highest blast-radius files)

### `pkg/blockstore/types.go` — `FormatCASKey` / `ParseCASKey` (BSCAS-01, D-29)

**Analog:** `pkg/blockstore/types.go:185-205` (existing `FormatStoreKey` / `ParseStoreKey` pair)

**Excerpt to mirror** (lines 185-205):
```go
// FormatStoreKey returns the block store key (S3 object key) for a block.
// Format: "{payloadID}/block-{blockIdx}".
func FormatStoreKey(payloadID string, blockIdx uint64) string {
    return fmt.Sprintf("%s/block-%d", payloadID, blockIdx)
}

// ParseStoreKey extracts the payloadID and block index from a store key.
// Store key format: "{payloadID}/block-{blockIdx}".
// Returns ("", 0, false) if the key format is invalid.
func ParseStoreKey(storeKey string) (payloadID string, blockIdx uint64, ok bool) {
    idx := strings.LastIndex(storeKey, "/block-")
    if idx < 0 || idx == 0 {
        return "", 0, false
    }
    payloadID = storeKey[:idx]
    blockIdx, err := strconv.ParseUint(storeKey[idx+len("/block-"):], 10, 64)
    if err != nil {
        return "", 0, false
    }
    return payloadID, blockIdx, true
}
```

**Phase 11 additions:**
- `FormatCASKey(h ContentHash) string` returns `"cas/" + hex[0:2] + "/" + hex[2:4] + "/" + hex` (uses existing `ContentHash.CASKey()` for the `blake3:`-prefixed identity form; this is a separate flat-path renderer for S3).
- `ParseCASKey(key string) (ContentHash, error)` validates `cas/{hh}/{hh}/{hex}` shape and returns a typed error (use `blockstore.ErrInvalidHash` family — see `types.go:58`). Per D-29 returns wrapped error on malformed input (matching the `ParseBlockID` style at lines 216-231 which uses `fmt.Errorf("...: %w", err)` rather than the older `(_, _, false)` sentinel).

### `pkg/blockstore/types.go:64-83` — Collapse 4-state `BlockState` → 3 states (STATE-01)

**Analog:** the file itself (current declaration).

**Current shape to rewrite:**
```go
type BlockState uint8
const (
    BlockStateDirty   BlockState = 0
    BlockStateLocal   BlockState = 1
    BlockStateSyncing BlockState = 2
    BlockStateRemote  BlockState = 3
)
```

**Target:**
- Drop `Dirty` and `Local` → introduce `Pending` as the post-AppendWrite/post-CommitChunks pre-upload state. Three states only: `BlockStatePending = 0` (so existing zero-valued legacy rows stay safe), `BlockStateSyncing = 1`, `BlockStateRemote = 2`.
- Update `String()` to match. Migration fallback in `IsRemote()` (lines 156-163) needs revisit — legacy `BlockStateDirty(==0)` blocks with non-empty `BlockStoreKey` should still be treated as Remote during the dual-read window (per D-21).

### `pkg/blockstore/types.go:111-153` — `FileBlock` new fields (D-12, D-13)

**Add fields:**
- `LastSyncAttemptAt time.Time` (drives D-14 janitor: requeue Syncing rows whose attempt > `syncer.claim_timeout`).
- `State` field already exists (line 139). The new state machine just shrinks the value space; no schema change required for that field.

Backend changes:
- `pkg/metadata/store/badger/objects.go:46-105` (PutFileBlock JSON-serializes the struct — add field is forward-compatible).
- Postgres backend may need a column add — planner audits the postgres `objects.go` row shape.

### `pkg/blockstore/engine/syncer.go` + `engine/upload.go` — full CAS upload-path rewrite (BSCAS-01/03/06, STATE-01..03, INV-03)

**Analog (the rewrite target itself):** `pkg/blockstore/engine/upload.go:72-141` (`syncFileBlock`).

**Current shape to retain:**
- `Syncer` struct + `NewSyncer` (`syncer.go:32-87`) — keep.
- `periodicUploader` ticker + `uploading atomic.Bool` overlap-prevention (`syncer.go:463-506`) — keep.
- `SyncNow` two-phase loop (`syncer.go:404-461`) — keep the outer loop, swap the inner `syncFileBlock` for the new claim-batch + parallel-upload pool.
- `revertToLocal` style failure path (`upload.go:23-26`) — adapt to new state names; one Put-FileBlock-style txn that flips `Syncing → Pending` with `LastSyncAttemptAt` cleared.

**Current shape to replace** (`upload.go:72-141`):
```go
func (m *Syncer) syncFileBlock(ctx context.Context, fb *blockstore.FileBlock) error {
    if fb.State != blockstore.BlockStateLocal { return nil }
    fb.State = blockstore.BlockStateSyncing
    if err := m.fileBlockStore.PutFileBlock(ctx, fb); err != nil {
        return fmt.Errorf("mark block %s syncing: %w", fb.ID, err)
    }
    // ...read fb.LocalPath, sha256.Sum256(data), dedup check, FormatStoreKey,
    //    m.remoteStore.WriteBlock(ctx, storeKey, data),
    //    fb.State = BlockStateRemote, m.fileBlockStore.PutFileBlock(...)
}
```

**Target (per D-11..D-15 + D-25):**
1. **Claim batch** (D-13): one txn flips `N` Pending blocks to `Syncing` + sets `LastSyncAttemptAt = now` (`config.ClaimBatchSize`, default 32). Replaces the current per-block `PutFileBlock(state=Syncing)` call.
2. **Parallel upload pool** (D-25): `config.UploadConcurrency` (default 8) goroutines drain the claim batch.
3. **Per-block upload** replaces `crypto/sha256` + `FormatStoreKey` with:
   - BLAKE3 hash via `lukechampine.com/blake3` over the bytes (already a Phase 10 dep — see `pkg/blockstore/hash_bench_test.go` for usage).
   - `key := blockstore.FormatCASKey(hash)` instead of `FormatStoreKey(payloadID, blockIdx)`.
   - `m.remoteStore.WriteBlock(ctx, key, data)` — the S3 layer (see `s3/store.go` excerpt below) sets the `x-amz-meta-content-hash` header.
4. **Single owner of `Syncing → Remote` transition** (D-15): same goroutine that received PUT 200 calls `m.fileBlockStore.PutFileBlock(state=Remote)`. No callback up the LocalStore stack.
5. **INV-03 ordering** (D-11): PUT-success → metadata-txn-success. CAS keys are content-defined → re-uploads are byte-identical to the same key → idempotent.
6. **Restart janitor** (D-14): one-shot pass at `Start()` requeues `Syncing` rows with `LastSyncAttemptAt > config.ClaimTimeout` back to `Pending`.

**Imports to add:** `lukechampine.com/blake3` (replaces `crypto/sha256` import at `upload.go:5`).

**Sentinel filename rename (D-28):** Scout already shows the file is at `pkg/blockstore/engine/syncer.go` (post-Phase-10 TD-01 merge). Phase 11 D-28 degrades to a no-op — confirm in PR-A intro and skip.

### `pkg/blockstore/engine/engine.go` — dual-read resolver (D-21, D-22, INV-06)

**Analog:** `pkg/blockstore/engine/engine.go:174-178` (`ReadAt`) + `:553-640` (`readAtInternal`, `tryL1Read`, `ensureAndReadFromLocal`).

**Current shape:**
```go
func (bs *BlockStore) ReadAt(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error) {
    return bs.readAtInternal(ctx, payloadID, data, offset)
}
// readAtInternal path: read buffer → local store → remote (via syncer.fetchBlock)
```

**Phase 11 dual-read insertion point** (per D-21 — by metadata key shape, NOT by S3 trial-and-error):
- In `readAtInternal` per-block loop, before calling `bs.local.ReadAt` / remote fetch, look up the corresponding `FileBlock` row via `fileBlockStore.GetFileBlock(blockID)`.
- If the row carries a non-zero `ContentHash`: route through new CAS read path (`m.remoteStore.ReadBlock(ctx, FormatCASKey(hash))` → streaming BLAKE3 verifier per D-18 → return).
- Otherwise: legacy path (`syncer.fetchBlock(ctx, payloadID, blockIdx)` which uses `FormatStoreKey` per `engine/fetch.go:14-27`). One DB lookup per block; no doubled S3 GET.

**Cache key bifurcation** (D-22): `pkg/blockstore/engine/cache.go` (`ReadBuffer`) keys by `(payloadID, blockIdx)` today; CAS reads should key by `ContentHash`. Two key spaces coexist Phase 11 → Phase 14. Phase 12 (CACHE-01..06) collapses readbuffer + prefetcher into a single `Cache` keyed by `ContentHash` — Phase 11 just adds the second key space without removing the first.

### `pkg/blockstore/engine/gc.go` — full mark-sweep rewrite (GC-01..04, INV-04)

**Analog (the rewrite target):** the entire current `gc.go` (192 lines). Keeps the public surface (`CollectGarbage(ctx, remoteStore, reconciler, options) *GCStats`, `GCStats`, `Options`) so `pkg/controlplane/runtime/blockgc.go:25-71` (`Runtime.RunBlockGC`) keeps compiling.

**Current shape (path-prefix algorithm, lines 71-192):**
```go
func CollectGarbage(ctx, remoteStore, reconciler, options) *GCStats {
    blocks, err := remoteStore.ListByPrefix(ctx, options.SharePrefix)
    // group by payloadID via ParseStoreKey
    // for each payloadID: GetFileByPayloadID; if not found → orphan → DeleteByPrefix
}
```

**Target (mark-sweep, per D-01..D-10):**
1. **Mark phase** (D-02): For every share (cross-share union per D-03 — already wired by `Runtime.RunBlockGC` calling `DistinctRemoteStores`), call new `MetadataStore.EnumerateFileBlocks(ctx, fn)`. Each yielded `ContentHash` is appended to the on-disk live set under `<localStore>/gc-state/<runID>/`.
2. **Live-set backing impl** (D-01): Badger temp store opened at the per-run dir (recommend reuse of `dgraph-io/badger` already imported by `pkg/metadata/store/badger/`). `incomplete.flag` marker created at start. Next-run cleanup detects stale dirs (mark is idempotent → no resume logic).
3. **Snapshot time `T`** (D-05): record at mark start. Sweep deletes only objects with S3 `LastModified < T - grace` (`gc.grace_period`, default 1h).
4. **Sweep phase** (D-04, D-07): bounded worker pool (`gc.sweep_concurrency`, default 16). Workers each handle a subset of the 256 top-level `cas/XX/` prefixes. Per-prefix: list `cas/XX/YY/*`, for each object check `LastModified` then probe live-set; on absent → DELETE. Per-prefix DELETE errors continue + capture (D-07).
5. **Fail-closed** (D-06): any mark-phase error aborts sweep (workers do not start). Distinct from sweep DELETE errors.
6. **Dry-run** (D-09): skip DELETEs; log/return up to `gc.dry_run_sample_size` (default 1000) candidate keys.
7. **Observability** (D-10): slog INFO at start/end with `run_id`, `hashes_marked`, `objects_swept`, `bytes_freed`, `duration_ms`, `error_count`. Persist `<localStore>/gc-state/last-run.json` (overwritten each run); `dfsctl store block gc-status` reads it.
8. **GC-04 reconfirmation:** must NOT introduce `BackupHoldProvider` coupling. Phase 08 deleted that scaffolding (per Phase 08 archive); Phase 11 keeps it deleted.

**Existing `parseShareName` helper** (`gc.go:197-208`): obsoleted by the cross-share union approach (no per-share grouping needed in mark phase). Delete.

### `pkg/blockstore/remote/s3/store.go:222-261` — PUT metadata header + GET streaming verify (BSCAS-06, INV-06)

**WriteBlock analog** (lines 222-239):
```go
func (s *Store) WriteBlock(ctx context.Context, blockKey string, data []byte) error {
    if err := s.checkClosed(); err != nil { return err }
    key := s.fullKey(blockKey)
    _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
        Bucket: aws.String(s.bucket),
        Key:    aws.String(key),
        Body:   bytes.NewReader(data),
    })
    if err != nil { return fmt.Errorf("s3 put object: %w", err) }
    return nil
}
```

**Phase 11 edit** (BSCAS-06): extend `PutObjectInput` with `Metadata: map[string]string{"content-hash": "blake3:" + hex.EncodeToString(hash[:])}`. AWS SDK normalizes keys to lowercase and prepends `x-amz-meta-` → wire header becomes `x-amz-meta-content-hash`. Header value matches `ContentHash.CASKey()` format already on the type at `types.go:36-38`.

Caller (the rewritten syncer) computes the BLAKE3 hash, passes it as a parameter — recommend a new `WriteBlockWithHash(ctx, key, hash, data)` method on `RemoteStore` interface, defaulting `WriteBlock` to compute-and-set internally for callers that don't have the hash pre-computed (or pass it via context — planner picks). Memory remote impl + remotetest conformance need matching method.

**ReadBlock analog** (lines 241-261):
```go
func (s *Store) ReadBlock(ctx context.Context, blockKey string) ([]byte, error) {
    // ...
    resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{...})
    // ...
    defer func() { _ = resp.Body.Close() }()
    return readResponseBody(resp.Body, resp.ContentLength, maxBlockReadSize)
}
```

**Phase 11 streaming verifier wrap** (D-18, D-19):
1. Pre-check: `expectedCAS := resp.Metadata["content-hash"]`; if non-empty and != caller-supplied hash → return `ErrCASContentMismatch` (header pre-check, D-19).
2. Wrap `resp.Body` in an `io.Reader` adapter that feeds bytes to a `blake3.Hasher` as the caller reads them (D-18). On EOF, compare `hasher.Sum(nil)` to the expected `ContentHash`. On mismatch → discard buffer, return wrapped `ErrCASContentMismatch`. Zero extra allocation.
3. Add new error sentinel `ErrCASContentMismatch` to `pkg/blockstore/errors.go` (existing `ErrInvalidHash`, `ErrBlockNotFound`, `ErrStoreClosed` neighbor pattern — see `types.go:58` reference).

Engine error mapping flows through Phase 09's `internal/adapter/common/content_errmap.go` (per Phase 09 PATTERNS.md `## File Classification`) without adapter-side changes.

### `pkg/blockstore/local/local.go:49-164` — Interface narrowing (LSL-07)

**Analog:** the file itself (current 22-method interface).

**Methods to delete** (5, per D-26):
- `MarkBlockRemote(ctx, payloadID, blockIdx) bool` (lines 138-139)
- `MarkBlockSyncing(ctx, payloadID, blockIdx) bool` (lines 141-142)
- `MarkBlockLocal(ctx, payloadID, blockIdx) bool` (lines 144-145)
- `GetDirtyBlocks(ctx, payloadID) ([]PendingBlock, error)` (lines 87-88)
- `SetSkipFsync(skip bool)` (lines 120-121)

State transitions migrate to `engine.Syncer` as the sole caller (D-15). `GetDirtyBlocks` is replaced by direct on-disk state inspection from Phase 10's AppendWrite log + LSL-08 eviction. `SetSkipFsync` becomes irrelevant once writes go through AppendWrite.

**Conformance** (`pkg/blockstore/local/localtest/suite.go:332-365`): delete `testMarkBlockState`. Delete `testGetDirtyBlocks` (lines 138-157). All implementations under `pkg/blockstore/local/fs/`, `pkg/blockstore/local/memory/` need the corresponding methods removed.

**Caller audit** (D-26): `WriteAt` and `EvictMemory` deferred for Phase 12+ (planner-discretion confirmation that all callers of those two outside the syncer-claim path have a migration target).

### `pkg/blockstore/local/fs/flush.go:94-169` — TD-09 stage-bytes-and-release

**Analog:** the file itself (current `flushBlock` body).

**Critical-section excerpt to fix** (lines 95-163):
```go
mb.mu.Lock()
if !mb.dirty || mb.data == nil { mb.mu.Unlock(); return "", 0, nil }
// ... mkdir, openfile, write, close ALL HAPPEN UNDER mb.mu ...
f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
if _, err := f.Write(mb.data[:dataSize]); err != nil { ... mb.mu.Unlock() ... }
_ = f.Close()
// ... metadata update, buffer release, mb.mu.Unlock() at line 163
```

**Phase 11 target (D-23, stage-and-release):**
```go
mb.mu.Lock()
if !mb.dirty || mb.data == nil { mb.mu.Unlock(); return "", 0, nil }
staged := bytes.Clone(mb.data[:mb.dataSize])  // O(dataSize) copy
dataSize := mb.dataSize
mb.mu.Unlock()
// All disk I/O outside the lock:
if err := os.MkdirAll(...); err != nil { return ... }
f, err := os.OpenFile(path, ...)
if _, err := f.Write(staged); err != nil { ... }
_ = f.Close()
// Reacquire briefly to flip flags + buffer release:
mb.mu.Lock()
mb.data, mb.dataSize, mb.dirty = nil, 0, false
mb.mu.Unlock()
```

Constant-cost copy per flush; concurrent readers/writers unblocked during disk I/O. Note: existing post-write metadata block (lines 142-153, `bc.queueFileBlockUpdate(fb)`) and atomic counters (`bc.diskUsed.Add(...)` line 162) need careful re-ordering with the new lock split — concurrent observers must not see the size delta before the disk write completes successfully. Planner picks the exact ordering in PR-B.

### `pkg/blockstore/local/fs/eviction.go` — LSL-08 in-process LRU (D-27)

**Analog:** `pkg/blockstore/local/fs/eviction.go:14-100` (`ensureSpace` + `collectRemoteCandidates`).

**Current shape (excerpt, lines 28-65):**
```go
func (bc *FSStore) ensureSpace(ctx context.Context, needed int64) error {
    // ...
    var candidates []*blockstore.FileBlock
    for bc.diskUsed.Load()+needed > bc.maxDisk {
        if candidates == nil { candidates = bc.collectRemoteCandidates() }  // FROM diskIndex (TD-02d)
        // evict via TTL or LRU policy
    }
}
```

**Phase 11 edit (D-27):**
- Phase 10's `CommitChunks` already promotes chunks into `blocks/{hh}/{hh}/{hex}` atomically (per `pkg/blockstore/local/fs/appendwrite.go:45-80` and `pkg/blockstore/local/localtest/appendlog_suite.go:90-100`).
- Track an in-process LRU keyed by `ContentHash`. Eviction = `os.Remove(blocks/{hh}/{hh}/{hex})` directly. No `FileBlockStore` lookup.
- `collectRemoteCandidates` becomes "scan in-process LRU for least-recently-accessed". Drop the diskIndex shape; replace with a new `lruIndex map[ContentHash]*lruEntry` (planner picks impl — `container/list` or custom).
- Self-contained; no metadata-store callbacks. The engine refetches from CAS on the next read if the local copy was evicted.

### `pkg/metadata/store.go` + `pkg/blockstore/store.go:12-53` — `EnumerateFileBlocks` cursor (D-02)

**Analog:** `pkg/blockstore/store.go:35-52` (existing pagination methods).

**Existing pagination shape:**
```go
ListLocalBlocks(ctx context.Context, olderThan time.Duration, limit int) ([]*FileBlock, error)
ListRemoteBlocks(ctx context.Context, limit int) ([]*FileBlock, error)
ListUnreferenced(ctx context.Context, limit int) ([]*FileBlock, error)
```

**Phase 11 addition** (recommended signature per CONTEXT.md):
```go
// EnumerateFileBlocks streams every FileBlock's ContentHash through fn in
// implementation-defined order. Returns the first non-nil error from fn or
// from the underlying store iterator. Does not load the full set into memory.
EnumerateFileBlocks(ctx context.Context, fn func(ContentHash) error) error
```

**Implementation analog** (Badger): `pkg/metadata/store/badger/objects.go:266-316` (`ListLocalBlocks`) — uses `txn.NewIterator(opts)` with `it.Seek(prefix)` / `it.ValidForPrefix(prefix)` / `it.Next()` over `fb:` prefix. Phase 11 implementation: same iterator, but instead of accumulating into `result []*metadata.FileBlock`, deserialize each block, call `fn(block.Hash)`, and bail out on iterator-exhausted or fn-error. No `limit` param — iterator runs to completion (or first fn error).

**Conformance** (`pkg/metadata/storetest/file_block_ops.go:65-98` — `testListLocalBlocks` style). Add four sub-tests per D-02: empty store / single file / large fanout / error mid-iteration aborts.

### `pkg/blockstore/engine/syncer_crash_test.go` — INV-03 deterministic crash injection (D-16)

**Analog:** `pkg/blockstore/engine/syncer_put_error_test.go:27-96` (`failingPutFileBlockStore` wrapper + counter + `errBoomPut` sentinel + driving call).

**Existing fault-injection pattern excerpt:**
```go
type failingPutFileBlockStore struct {
    blockstore.FileBlockStore
    putCount atomic.Int64
    allowed  int64
}

func (f *failingPutFileBlockStore) PutFileBlock(ctx context.Context, block *blockstore.FileBlock) error {
    n := f.putCount.Add(1)
    if n > f.allowed { return errBoomPut }
    return f.FileBlockStore.PutFileBlock(ctx, block)
}
```

**Phase 11 extension:** add a parallel `crashingRemoteStore` wrapper that triggers a kill-point at three positions (D-16):
1. `pre-PUT` — wrapper returns `errKillPoint` before calling underlying `WriteBlock`. Asserts: no S3 object, no state change.
2. `between-PUT-success-and-metadata-txn` — wrapper succeeds the PUT, then `crashingFileBlockStore.PutFileBlock` returns `errKillPoint`. Asserts: S3 object exists at CAS key, FileBlock row stays `State=Syncing`. GC reaps after grace.
3. `post-metadata-txn` — both succeed. Asserts: `State=Remote`, fully consistent.

Same `metadatamemory` + `remotememory` fixtures as the existing test (lines 50-65). No Localstack dependency. Generic reusable harness deferred to test-infra phase.

### `cmd/dfsctl/commands/store/block/gc.go` (NEW) — `dfsctl store block gc <share> [--dry-run]`

**Analog:** `cmd/dfsctl/commands/store/block/evict.go:13-98` (entire file).

**Excerpt to mirror** (lines 13-72):
```go
var evictCmd = &cobra.Command{
    Use:   "evict",
    Short: "Evict block store data",
    Long:  `Evict block store data ... Examples: dfsctl store block evict ...`,
    RunE:  runBlockStoreEvict,
}

func init() {
    evictCmd.Flags().String("share", "", "Evict data for a specific share only")
    evictCmd.Flags().Bool("read-buffer-only", false, "...")
}

func runBlockStoreEvict(cmd *cobra.Command, _ []string) error {
    client, err := cmdutil.GetAuthenticatedClient()
    if err != nil { return err }
    shareName, _ := cmd.Flags().GetString("share")
    req := &apiclient.BlockStoreEvictOptions{...}
    var resp *apiclient.BlockStoreEvictResult
    if shareName != "" { resp, err = client.BlockStoreEvictForShare(shareName, req) }
    else                { resp, err = client.BlockStoreEvict(req) }
    // output formatting via cmdutil.GetOutputFormatParsed()
}
```

**Phase 11 adaptation:**
- New `gcCmd` with `Use: "gc <share>"`, `Args: cobra.MinimumNArgs(1)` so the share name is positional (not a flag — matches `dfsctl store block gc <share>` per CONTEXT.md D-08).
- `--dry-run` bool flag.
- New `client.BlockStoreGC(shareName, &apiclient.BlockStoreGCOptions{DryRun: dryRun})` apiclient method (mirror existing `BlockStoreEvictForShare`).
- Wire from `cmd/dfsctl/commands/store/block/block.go:34-40` (`init()` adds subcommands): `Cmd.AddCommand(gcCmd)` and `Cmd.AddCommand(gcStatusCmd)`.

### `internal/controlplane/api/handlers/block_stores.go` — REST handler additions

**Analog:** `internal/controlplane/api/handlers/block_stores.go:30-130` (existing `BlockStoreHandler` struct + handler methods like `Create`).

**Existing handler pattern excerpt** (lines 30-85):
```go
type BlockStoreHandler struct {
    store   store.BlockStoreConfigStore
    runtime *runtime.Runtime
}
// extractKind, validateBlockStoreType helpers
func (h *BlockStoreHandler) Create(w http.ResponseWriter, r *http.Request) {
    // chi.URLParam, decodeJSONBody, BadRequest helpers
}
```

**Phase 11 additions:**
- New `RunGC(w, r)` method on `BlockStoreHandler` — extract `name := chi.URLParam(r, "name")`, decode `{dry_run: bool}` body, call `h.runtime.RunBlockGC(ctx, sharePrefix, dryRun)` (already implemented at `pkg/controlplane/runtime/blockgc.go:25-71`), respond with `*engine.GCStats` JSON.
- New `GCStatus(w, r)` method — read `<localStore>/gc-state/last-run.json` (per D-10), respond with stored summary.
- Route registration: planner audits the existing chi router setup to wire `POST /api/v1/store/block/{name}/gc` and `GET /api/v1/store/block/{name}/gc-status`.

### `pkg/controlplane/runtime/blockgc.go` — already in place (no Phase 11 change required)

**Reuse as-is.** `RunBlockGC` (lines 25-71) already iterates `DistinctRemoteStores()` and calls `engine.CollectGarbage` per remote — exactly the cross-share aggregation D-03 specifies. Phase 11 only changes the body of `engine.CollectGarbage`; the orchestration layer is correct as-shipped post-Phase-08.

The package-level `collectGarbageFn = engine.CollectGarbage` indirection (lines 73-77) means tests can keep injecting fakes against the new mark-sweep impl.

### `test/e2e/cas_immutable_overwrites_test.go` (NEW — canonical correctness)

**Analog:** existing `test/e2e/` E2E tests with the Localstack fixture (CONTEXT.md `<code_context>` confirms the fixture is in place; Phase 11 reuses it for D-33 too).

**Test shape** (per CONTEXT.md `<domain>` & ROADMAP success criterion #1):
1. Create file via NFS or SMB through DittoFS; write payload `A`.
2. Trigger sync (`dfsctl ... drain-uploads` or wait for periodic). Capture CAS key `cas/...A_hash`.
3. Overwrite same file with payload `B`. Trigger sync. Capture new key `cas/...B_hash`.
4. Assert via direct S3 client: BOTH keys exist (immutability — A's bytes are not stomped).
5. Run GC (`dfsctl store block gc <share>`). Assert: `cas/...A_hash` deleted (no longer in any FileAttr.Blocks live set); `cas/...B_hash` retained.
6. Read file back; assert payload == `B` and BLAKE3 verification succeeded (INV-06).

Pre-Phase-11 expectation: this test FAILS on develop (per STATE.md "Pending Todos" and the Phase 10 "TestBlockStoreImmutableOverwrites E2E skeleton drafted and confirmed failing" requirement).

### `test/e2e/x_amz_meta_content_hash_test.go` (NEW — BSCAS-06 D-33 external-verifier)

**Analog:** same Localstack fixture as the canonical correctness test.

**Test shape:**
1. Write a file through DittoFS; trigger sync.
2. Use the AWS SDK directly (or `aws-sdk-go-v2/service/s3` `HeadObject`) bypassing DittoFS to fetch the head of the corresponding `cas/...` key.
3. Assert `resp.Metadata["content-hash"]` equals `"blake3:" + hex(BLAKE3(payload))`.

Satisfies BSCAS-06's "external tooling can verify without DittoFS metadata" criterion.

---

## Shared Patterns

### Structured logging (slog INFO at lifecycle points)

**Source:** `pkg/blockstore/engine/syncer.go:472, 138-139, 195-200, 472-505`; `pkg/controlplane/runtime/blockgc.go:31-46, 63-68`.

**Apply to:** new GC mark/sweep boundaries, syncer claim-batch start/end, restart janitor pass.

**Example:**
```go
logger.Info("GC: starting",
    "run_id", runID,
    "config_id", entry.ConfigID,
    "shares", entry.Shares,
    "dry_run", opts.DryRun,
    "snapshot_time", T,
    "grace_period", opts.GracePeriod)
```

### Error sentinels under `pkg/blockstore/errors.go`

**Source:** existing `blockstore.ErrBlockNotFound`, `ErrInvalidHash`, `ErrStoreClosed`, `ErrRemoteUnavailable`, `ErrFileBlockNotFound`, `ErrInvalidPayloadID` (see `types.go:58`, `syncer.go:124`, `syncer.go:268`, `errors.go`).

**Apply to:** new sentinels Phase 11 introduces:
- `ErrCASContentMismatch` — streaming verifier or header pre-check found a mismatch on S3 GET (INV-06).
- `ErrCASKeyMalformed` — `ParseCASKey` rejection.
- `ErrLiveSetIncomplete` — gc-state mark phase encountered a stale `incomplete.flag` from a prior run; the new run cleans up but logs.

These flow through Phase 09's `internal/adapter/common/content_errmap.go` for adapter mapping (no adapter-side change needed per Phase 09 PATTERNS.md `## Shared Patterns`).

### Conformance suites (CLAUDE.md invariant 7)

**Source:** `pkg/metadata/storetest/`, `pkg/blockstore/local/localtest/`, `pkg/blockstore/remote/remotetest/`.

**Apply to:**
- `EnumerateFileBlocks` cursor — every metadata backend (memory, badger, postgres) MUST pass.
- LSL-07 narrowed `LocalStore` — every implementation under `pkg/blockstore/local/{fs,memory}/` recompiles cleanly with the 5 dropped methods removed.
- LSL-08 in-process-LRU eviction — new sub-tests in `pkg/blockstore/local/localtest/` modeled after `appendlog_suite.go:39-45` 5-scenario layout.
- `RemoteStore.WriteBlock` MUST set the `x-amz-meta-content-hash` header (or equivalent backend-specific metadata field) when called via the new hash-aware variant — extend `pkg/blockstore/remote/remotetest/` with a header-set assertion.

### Per-share block-store ownership (CLAUDE.md invariant 4)

**Source:** `pkg/controlplane/runtime/shares/service.go:865-895` (`GetBlockStoreForHandle`, `GetBlockStoreForShare`); `pkg/controlplane/runtime/shares/service.go:898-949` (`DistinctRemoteStores` + `RemoteStoreEntry`).

**Apply to:** GC cross-share aggregation MUST go through `DistinctRemoteStores()` — never iterate `s.registry` directly to enumerate remotes (that would double-count shares sharing a remote config).

### AuthContext-bearing operations (CLAUDE.md invariant 2)

**Source:** all `MetadataStore` Files-interface methods (e.g., `pkg/metadata/store.go:33-46`).

**Apply to:** `EnumerateFileBlocks(ctx, fn)` must take `context.Context` (not `*AuthContext`) since GC is a system-internal operation (not per-user) — but it still threads `ctx` for cancellation. Confirm this in the conformance test: cursor must respect `ctx.Done()`.

---

## No Analog Found

Files with no close in-tree match (planner should use RESEARCH.md or external references):

| File | Role | Data Flow | Reason |
|------|------|-----------|--------|
| `pkg/blockstore/engine/gcstate.go` (disk-backed live set) | utility | append-then-probe over Badger temp store | No existing pattern in DittoFS for run-scoped temp Badger DBs. Closest is the per-payload AppendWrite log; planner picks Badger temp-store wrapper API. |
| BLAKE3 streaming `io.Reader` verifier | utility | streaming I/O | No streaming-hash wrapper exists in-tree. Planner builds a thin wrapper using `lukechampine.com/blake3`'s `Hasher.Write` — the package is already a dep from Phase 10 hash benchmarks (`pkg/blockstore/hash_bench_test.go`). |

---

## Metadata

**Analog search scope:**
- `pkg/blockstore/`, `pkg/blockstore/engine/`, `pkg/blockstore/local/`, `pkg/blockstore/local/fs/`, `pkg/blockstore/local/localtest/`, `pkg/blockstore/remote/s3/`, `pkg/blockstore/chunker/`
- `pkg/metadata/`, `pkg/metadata/store/`, `pkg/metadata/storetest/`
- `pkg/controlplane/runtime/`, `pkg/controlplane/runtime/shares/`
- `cmd/dfsctl/commands/store/block/`
- `internal/controlplane/api/handlers/`
- `.planning/phases/09-adapter-layer-cleanup-adapt/09-PATTERNS.md` (multi-PR + per-requirement-atomic-commit pattern Phase 11 inherits)

**Files scanned:** 24 source files read end-to-end or in targeted ranges; ~60 files surfaced via grep for callers/usages.

**Pattern extraction date:** 2026-04-25.

**Key invariant verified during mapping:** the syncer file is already at `pkg/blockstore/engine/syncer.go` post-Phase-10 TD-01 merge; D-28 rename degrades to a no-op.
