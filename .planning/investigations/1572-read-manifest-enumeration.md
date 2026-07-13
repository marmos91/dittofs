# #1572 — Random reads O(N²): full FileChunk-manifest enumeration per read

Verified against `origin/develop` @ `0b2361b8` (not the working tree).

## Root cause (confirmed by 25s CPU profile, develop `0b2361b8`)

Every read window resolves its covering chunk by enumerating the **entire**
per-payload FileChunk manifest and Go-sorting it — and does so on **two**
independent hot paths per read:

1. **Engine read path** — `pkg/block/engine/read_internal.go:readLocalByHash`
   (L142) calls `bs.fileChunkStore.ListFileChunks(payloadID)` then walks the
   result with `findRowCoveringOffset` (L232, O(N)).
2. **Syncer download/prefetch path** — `pkg/block/engine/fetch.go`:
   - `EnsureAvailableAndRead` (L~380) takes ONE `listFileChunksSnapshot`
     (L47→`ListFileChunks`, L57) and reuses it across the block loop (already
     de-duplicated to 1 scan per read — good), BUT
   - `enqueuePrefetch` → `blockIsLocal` → `resolveFileChunk` (L~40) takes a
     **fresh** `ListFileChunks` scan per probed block.

`ListFileChunks` (badger `objects.go:398` / `listFileChunksTxn` L~531) does
**N badger Gets + N JSON unmarshals + an O(N log N) sort**, where each sort
comparison calls `parseBlockIdx` (L717) → `fmt.Sscanf(idx,"%d",&v)`.

For a 512 MiB file randomly read at 4k, each read touches 1 block but scans all
N rows (N ≈ fileSize/avgChunkSize). Over a full random pass: **O(N²)** scans →
CPU-bound → ~2 IOPS / 585 ms with zero network. Profile shares match exactly:
81% `ListFileChunks`, 75% `resolveFileChunk`, 36% `enqueuePrefetch→blockIsLocal`,
32% `fmt.Fscanf` (the Sscanf inside the sort).

## Chunk geometry (correctness constraints)

- FileChunk **ID = `{payloadID}/{chunkOffset}`** where `chunkOffset` is the
  chunk's **absolute byte offset** (FastCDC, variable size), parsed by
  `block.ParseChunkOffset` (`pkg/block/ids.go`, already `strconv`-free/fast).
- Covering a target offset T is **NOT** `T/BlockSize` keyed: chunks are
  variable-size, so the covering row is *the row with the largest
  `chunkOffset ≤ T` whose `chunkOffset + DataSize > T`*. This is exactly what
  `findRowCoveringOffset` / `resolveFileChunkFromRows` compute today.
- **Sparse holes MUST zero-fill.** If the largest `chunkOffset ≤ T` ends before
  T (`T ≥ off+DataSize`), or no row exists, the lookup returns `(nil, nil)`
  (NOT an error). Callers: `readLocalByHash`→`false`→`clear(dest)`;
  `EnsureAvailableAndRead`→not-local→fetch→nil→`zeroBlockRegion`. Preserve.
- One row per offset (overwrite upserts the same `{pid}/{off}` ID), so
  "largest ≤ T" is deterministic.
- Backend index keys are **lexicographic decimal** (`fb-file:{pid}:{off}`,
  `id LIKE 'pid/%'`), which is why every backend re-sorts in Go — decimal
  strings are not numerically ordered ("100" < "20"). No backend currently
  supports a numeric range seek without an index/schema change.

## Is a by-offset lookup already available? No.

`block.EngineFileChunkStore` (`pkg/block/filechunk.go:145`) exposes only
`GetFileChunk(id)`, `ListFileChunks(payloadID)`, `EnumeratePayloads`. There is
no covering-offset accessor. SQL table is `file_blocks` with only `id VARCHAR`
(no `chunk_offset` column). Minimal addition required.

## Fix

### Fix A (trivial, immediate ~32%) — kill `fmt.Sscanf` in key parsing
Replace `fmt.Sscanf(x,"%d",&v)` with `strconv.Atoi` (or `ParseUint`) at all
three sibling sites:
- `pkg/metadata/store/badger/objects.go:720` (`parseBlockIdx`)
- `pkg/metadata/store/postgres/objects.go:463` (`pgParseBlockIdx`)
- `pkg/metadata/store/sqlite/objects.go:464` (`pgParseBlockIdx`)

(No hot `Sprintf` key-*building* on the read path — `inFlightKey` is per-block,
cheap. Leave it.)

### Fix B (the real fix) — resolve the SINGLE covering chunk, not the manifest
Add a covering-offset lookup and route the two hot paths through it.

**Interface (minimal, per the "minimize interface surface" rule):**
- Add ONE method to the badger backend only (the profiled hot path):
  ```go
  // GetFileChunkAtOffset returns the FileChunk covering absolute byte
  // offset off (largest chunkOffset ≤ off with off < chunkOffset+DataSize),
  // or (nil, nil) for a sparse hole / empty payload.
  func (s *BadgerMetadataStore) GetFileChunkAtOffset(ctx, payloadID string, off uint64) (*metadata.FileChunk, error)
  ```
  Impl: iterate the `fb-file:{payloadID}:` secondary index **KeysOnly**
  (`opts.PrefetchValues=false`), `strconv`-parse the offset from each key, track
  the best `off ≤ target`; then a **single** `Get(fb:{blockID})` + one
  unmarshal; apply the `target < off+DataSize` covering check, else `(nil,nil)`.
  Eliminates the N Gets, N unmarshals, and the sort. O(N) cheap key-scan + O(1)
  materialize. `// ponytail: O(n) keys-only scan; add a zero-padded/big-endian
  fb-off index for O(log n) seek only if profiles still show it.`
- **Do NOT widen `EngineFileChunkStore`.** In the engine, define a narrow
  consumer interface and fall back to the existing scan for other backends:
  ```go
  type chunkAtOffsetResolver interface {
      GetFileChunkAtOffset(ctx context.Context, payloadID string, off uint64) (*block.FileChunk, error)
  }
  ```
  A single engine helper `resolveCovering(ctx, payloadID, off)` type-asserts to
  `chunkAtOffsetResolver`; else `ListFileChunks` + `findRowCoveringOffset`
  (unchanged behaviour on memory/sqlite/postgres — none is the hot path;
  memory is RAM-only, SQL is a follow-up).

**Rewire callers:**
- `read_internal.go readLocalByHash`: drop the `ListFileChunks` scan; the
  `for currentOffset` loop calls `resolveCovering(payloadID, currentOffset)` per
  segment (already loops per covered chunk). Keep the DataSize clamp, heal, and
  sparse→false logic verbatim.
- `fetch.go`:
  - `resolveFileChunk(payloadID, blockIdx)` → `resolveCovering(payloadID,
    blockIdx*BlockSize)` (drops `listFileChunksSnapshot`).
  - `blockIsLocal` (prefetch probe) rides on `resolveFileChunk` → now O(1)-ish.
  - `EnsureAvailableAndRead`: replace the one-shot `listFileChunksSnapshot` +
    `blockIsLocalFromRows(rows,idx)` / `inlineFetchOrWait(...,rows)` with a
    per-block `fb := resolveCovering(payloadID, idx*BlockSize)`; change
    `inlineFetchOrWait(...,rows)` (L437) to take the resolved `fb`. For a K-block
    read that is K cheap lookups (random 4k → K=1). The old "single snapshot"
    optimization existed only to amortize the expensive scan; a cheap point
    lookup makes it unnecessary. Delete `listFileChunksSnapshot`,
    `resolveFileChunkFromRows`, `blockIsLocalFromRows` if unused after rewire.

**Ordering:** Fix A first (safe, isolated, gives immediate relief) → Fix B
badger method + engine helper → rewire read_internal → rewire fetch/syncer →
delete dead snapshot helpers.

**Tests:** extend `pkg/metadata/storetest` with a covering-offset case (exact
start, mid-chunk, hole between chunks, past-EOF, empty payload) asserting
badger's `GetFileChunkAtOffset` matches a `ListFileChunks`+walk oracle. Existing
`storetest` FileChunk conformance + engine read tests must stay green
(sparse/zero-fill regression guard).

## Non-hot backends (follow-up, not blocking)
SQL efficient path needs `payload_id` + `chunk_offset` columns backfilled from
`id`, indexed `(payload_id, chunk_offset)`, then `... WHERE payload_id=? AND
chunk_offset<=? ORDER BY chunk_offset DESC LIMIT 1`. Deferred: SQL is not the
profiled backend and the fallback keeps it correct.

## Before/After benchmark + pprof

Same box, same MinIO/badger share, NFSv3 mount at default port `12049`.

```bash
# mount the DittoFS share, cd into it
M=/mnt/dittofs                 # NFSv3 mount of a badger+remote share
# 1) lay down a fragmented file (many small chunks -> large N)
fio --name=w --directory=$M --rw=randwrite --bs=4k --size=512M \
    --direct=1 --runtime=30 --time_based --end_fsync=1
sync; sleep 20                 # let rollup/carve settle (manifest fully populated)
# 2) drop client cache so reads hit the server, then measure random reads
fio --name=r --directory=$M --rw=randread --bs=4k --size=512M \
    --direct=1 --runtime=30 --time_based --group_reporting   # <-- IOPS metric
# 3) CPU profile the read (run during step 2)
curl -s -H "Authorization: Bearer $DFS_DEBUG_TOKEN" \
    'http://127.0.0.1:9090/debug/pprof/profile?seconds=25' > read.pprof
go tool pprof -top -nodecount=15 read.pprof
```

**Metrics & targets**
| Metric | BEFORE (baseline) | AFTER (target) |
|---|---|---|
| randread 4k IOPS | ~2 (≈585 ms/op) | thousands (rclone-band / IO-bound, not CPU-bound) |
| `pprof -top` `ListFileChunks` | ~81% | out of top-15 (≈0 on read path) |
| `pprof -top` `fmt.Fscanf`/`Sscanf` | ~32% | absent |
| `resolveFileChunk`/`blockIsLocal` | 75% / 36% | negligible (single row materialize) |

PASS = IOPS jumps by orders of magnitude AND `ListFileChunks`/`Fscanf` no
longer appear in the read profile's hot set; remaining time shifts to
badger index iteration / blake3 verify / network — i.e. genuinely IO-bound.

## Biggest risk
The covering semantics must be preserved **exactly** — returning the
largest-offset row without the `target < off+DataSize` check would serve a
*neighbouring* chunk's bytes for a sparse hole (silent data corruption) instead
of zero-filling. The storetest hole/past-EOF cases are the guard. Secondary
risk: the `EnsureAvailableAndRead` rewire touches the download/zero-fill/
prefetch orchestration — keep `zeroBlockRegion` / `needLocalReadAt` branches
byte-for-byte.
