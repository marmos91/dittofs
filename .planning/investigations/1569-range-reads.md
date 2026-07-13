# #1569 — Cold random-read amplification (range GET on the read path)

Investigation only. All code verified against `origin/develop` (working tree is a stale side-branch).

## TL;DR — the ticket premise is stale

The bottleneck as written ("a 4 KiB random read amplifies to a whole 16 MiB block
fetch — there is no byte-range GET") **does not match `origin/develop`.** The #1493
blocks-only flip already added packed-block byte-range reads, and the demand read path
already uses them. A cold 4 KiB read pulls **one FastCDC chunk** (min 1 MiB, avg 4 MiB,
max 16 MiB) via an S3 ranged `GetObject`, **not** the 16 MiB carve block.

So there is nothing to *build* for "range GET" — it exists and is wired. The residual
amplification is the **FastCDC chunk-size floor (1 MiB min)**, and that is the only real
lever left. Sub-chunk ranging is impossible by construction (see §3).

## 1. Current cold-read fetch granularity (verified on origin/develop)

Demand read path:

```
read_internal.go:101  Syncer.EnsureAvailableAndRead
  → fetch.go:401       inlineFetchOrWait (per blockIdx)
    → fetch.go:136     dispatchRemoteFetch
      → fetch.go:187   resolveAndReadChunk   (resolveLocator → GetLocator)
        → fetch.go:233 readChunkVerified
          → remoteStore.ReadChunk(loc.BlockID, loc.WireOffset, loc.WireLength, hash)
```

`readChunkVerified` (fetch.go ~L233):
```go
data, err := m.remoteStore.ReadChunk(ctx, loc.BlockID, loc.WireOffset, loc.WireLength, hash)
computed := block.ContentHash(blake3.Sum256(data))   // verify whole chunk
```

`s3.Store.ReadChunk` (pkg/block/remote/s3/store.go ~L392) issues:
```go
Range: aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
resp, _ := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket, Key: blocks/<blockID>, Range})
```

**So the cold GET is already ranged to the chunk's wire window inside the packed block
object.** `WireOffset`/`WireLength` live on `block.ChunkLocator` (pkg/block/locator.go):

```go
type ChunkLocator struct {
    BlockID    string  // enclosing packed object → blocks/<BlockID>
    WireOffset int64   // chunk's byte offset within the block object
    WireLength int64   // chunk's byte length within the block object
}
```

The locator→range mapping the task asks for **already exists**: `GetLocator(hash)` →
`(BlockID, WireOffset, WireLength)` → `ReadChunk` Range header.

Whole-object `GetBlock` (pkg/block/remote/s3/store.go ~L485) is **not** on the read
path. Its only callers are `compaction.go:196`, `shares/service.go:415`, and the
compression/encryption decorators — repack/migration/whole-block flows, not client reads.

### Granularity numbers (pkg/block/chunker/params.go)

| Constant | Value |
|---|---|
| MinChunkSize | **1 MiB** |
| AvgChunkSize | 4 MiB |
| MaxChunkSize | 16 MiB |
| DefaultBlockCarveBytes | 16 MiB (carve/packing target — irrelevant to read granularity) |

A cold 4 KiB read therefore fetches **1–16 MiB (avg 4 MiB)** = ~256×–4000× amplification,
with a **1 MiB hard floor**. The ticket's flat "16 MiB / 4000×" is the max-chunk worst
case (or predates #1493), not the typical case.

Note: the fetched chunk is written to the local CAS tier whole (`m.local.Put(ctx, fb.Hash,
data)` in both `inlineFetchOrWait` and `fetchResolvedBlock`), so amplification is a
one-time cost per chunk until eviction — the second read of any byte in that chunk is local.

## 2. Fix options, ranked by win

### A. (win: none — already done) Range GET on the read path
Already implemented (§1). **Action: do not rebuild.** Just prove it with the benchmark in
§4 — the "before" here is a fantasy unless someone is on a pre-#1493 build. If the
benchmark shows 16 MiB fetched per 4 KiB read, the bug is a *regression* (locator with
zeroed WireLength, or a fallback to `GetBlock`), not a missing feature — investigate that
instead.

### B. (win: high, the real lever) Per-share smaller FastCDC chunk size for random-access shares
Chunk size is the granularity floor. `MinChunkSize/AvgChunkSize/MaxChunkSize` are
hardcoded `const` in chunker/params.go. Make them a per-share `ChunkerConfig` so a
random-access share can run e.g. 64 KiB / 256 KiB / 1 MiB, dropping the read floor 16×.
- **Win:** cold 4 KiB read floor 1 MiB → 64 KiB (~16× less rx, proportional latency).
- **Cost:** more chunks → more `FileChunk` rows + `GetLocator` entries + index pressure;
  worse cross-file dedup; more objects (mitigated — chunks still pack into 16 MiB blocks).
- **Ceiling:** chunk stays the atomic unit (§3), so 64 KiB min is the new floor.
- ponytail: one knob, defaulted to today's constants; only random-access shares opt in.

### C. (win: situational, ~free) Resident hot-set: raise/pin the local cache
`maxDisk` (pkg/block/local/fs/eviction.go, default ~10 GiB via `defaultRemoteCacheSize`)
already gates eviction. If the working set fits, hot chunks never go remote and #1569 never
fires. Expose per-share `max_size` (largely already plumbed) and, optionally, a "pin
recently-read chunks" bias in eviction victim selection. This is a config/tuning lever, not
a new subsystem — the cheapest mitigation but only helps when the hot set is bounded.

**Recommendation:** ship **A-verification first** (benchmark proves range GET works and
sets the true baseline). Only if the 1 MiB floor is still too coarse for the target random
workload, do **B**. Keep **C** as the documented operator tuning knob. Do **not** build a
sub-chunk range path — §3.

## 3. Why sub-chunk ranging is impossible (the hard ceiling)

The chunk is simultaneously the unit of three things, all of which need the whole chunk:
1. **Content addressing** — `readChunkVerified` recomputes `blake3.Sum256(data)` over the
   entire chunk and compares to `fb.Hash`. A partial read can't be verified.
2. **Compression** — pkg/block/compression/decorator.go frames per chunk.
3. **Encryption (AEAD)** — pkg/block/encryption/decorator.go seals per chunk; you cannot
   decrypt bytes `[k, k+4096)` of an AEAD chunk without the whole ciphertext + tag.

`ReadChunk` returns the chunk's *wire* bytes; the decorator stack decrypts/decompresses to
plaintext, then the engine verifies. There is no layer that can serve a sub-chunk range.
Hence the only way to fetch less per random read is to **make chunks smaller** (option B).

## 4. BEFORE/AFTER benchmark + pprof

Goal: prove the cold random read fetches **one chunk, not the 16 MiB block** (validates A),
and measure the win from B if pursued.

### Setup — force eviction so reads go remote
Small local cache so the dataset can't stay resident. Two ways:
```bash
# Server config (~/.config/dittofs/config.yaml) for the S3-backed share:
#   local cache cap well under the dataset — forces eviction → cold reads
#   (maxDisk / defaultRemoteCacheSize; expose per-share max_size for the test)
# e.g. max_size: 256MiB against a 4 GiB dataset.
```
Backend: real S3 or Localstack (`test/e2e/run-e2e.sh --s3`). Prefer a real
~50–150 ms-RTT bucket to make per-fetch latency visible.

### Workload
```bash
# 1. Write a 4 GiB file through the mount so it's chunked + uploaded, then
#    drop caches to force cold reads (fill past cap OR restart server with small max_size).
dd if=/dev/urandom of=/mnt/dittofs/big.bin bs=1M count=4096
sync

# 2. Cold random 4 KiB reads, direct I/O (bypass page cache):
fio --name=coldrand --filename=/mnt/dittofs/big.bin \
    --rw=randread --bs=4k --direct=1 --ioengine=libaio \
    --iodepth=1 --numjobs=1 --runtime=60 --time_based \
    --randrepeat=0 --norandommap \
    --output-format=json --output=cold-$(git rev-parse --short HEAD).json
```

### Metrics
- **NIC rx bytes per read** — primary signal.
  `cat /sys/class/net/<if>/statistics/rx_bytes` before/after the run; divide delta by fio
  read count. Expected: **~chunk size / read**, NOT 16 MiB / read.
- **Latency percentiles** — fio `clat` p50/p99. Full-block fetch ≈ 120–260 ms; chunk fetch
  scales with chunk size (~1 MiB min ≈ proportionally lower on the same link).
- **Server-side counter** — `dataplaneMetrics.RecordBlockRangeRead(len(data))` already
  fires in `readChunkVerified` (fetch.go). Scrape `/metrics` (:9090) and confirm the
  histogram sum ≈ chunk sizes, and that `GetBlock`/whole-object counters stay flat.
- **pprof** — `DITTOFS_LOGGING_LEVEL=DEBUG`, capture
  `go tool pprof http://localhost:9090/debug/pprof/profile?seconds=30` during the run;
  confirm time is in S3 `GetObject` + BLAKE3 verify, not in a whole-block read.

### Targets / pass criteria
| | Expected on develop (A) | With option B (64 KiB min) |
|---|---|---|
| rx bytes / 4 KiB read | ≈ chunk size (1–4 MiB), **≪ 16 MiB** | ≈ 64 KiB |
| clat p50 | chunk-sized fetch latency | ~16× lower rx-bound latency |
| whole-block GetObject count | **0** on read path | 0 |

If "before" already shows ≪16 MiB rx per read, **A is confirmed working** and the ticket's
16 MiB claim is stale — close #1569 as "already fixed by #1493" or re-scope it to option B
(chunk-size knob) with the measured 1 MiB floor as the justification.

## 5. Implementation plan (only if pursuing B)

Range GET (A) needs **no code**. For the chunk-size knob (B):

| File | Change |
|---|---|
| `pkg/block/chunker/params.go` | Turn Min/Avg/Max consts into fields of a `Params` struct; keep current values as `DefaultParams`. |
| `pkg/block/chunker/chunker.go` | Thread `Params` into the chunker instead of package consts. |
| `pkg/block/engine/types.go` / carver | Add `ChunkerParams` to engine config; default `DefaultParams`; validate min≤avg≤max and min ≥ small floor. |
| control-plane share config + `docs/guide/configuration.md` | Per-share opt-in (e.g. `chunking.mode: random-access`) mapping to a small preset. Regenerate `docs/guide/cli.md` via `go run ./cmd/gendocs` if a flag is added. |
| storetest / blockstoretest | Ensure conformance suites run with a non-default small preset (dedup + read-back invariants hold at 64 KiB). |

Migration note: chunk size only affects **newly written** data; existing chunks keep their
size. No rechunk of existing shares — document that.

The locator→range mapping needs no change: `GetLocator` → `(BlockID, WireOffset,
WireLength)` → `ReadChunk` already carries whatever size the chunk was written at.
