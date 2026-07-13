# DittoFS performance investigations — consolidated hand-off

Six parallel investigations off the #1466 real-hardware benchmark (SCW POP2-HC-32C-64G → SCW S3 fr-par, develop `0b2361b8`). Each has a standalone plan with a before/after benchmark + pprof design. This file ranks them and surfaces the shared root causes. Benchmark report: https://claude.ai/code/artifact/afb8557d-1b06-4ab7-b2e8-69b212850323

## Two premises the benchmark *disproved* (verify-before-fix paid off twice)

- **#1572 was not sparse holes / not S3.** Holes zero-fill from memory (330k IOPS); dense synced data reads locally (2419 IOPS); only randomly-written files collapse (2 IOPS, **NIC rx=0**). A live pprof named the real cause (below).
- **#1569 range-GET already exists.** The demand read path already issues a byte-range `GetObject` for the chunk's window (`s3.Store.ReadChunk`). No 16 MiB whole-block fetch on client reads. Largely a non-issue.

## The shared root cause: synchronous per-op durability NFS defers to COMMIT

Three of the six findings are the **same bug wearing three hats** — a `fsync`/flush done per operation that NFS correctly defers to COMMIT:

| Finding | Per-op work | Where |
|---|---|---|
| Write path 3× (130 vs 408 MB/s) | append-log `fsync` **per write** | `pkg/block/local/fs/appendwrite.go` → `groupCommit.Sync` |
| Metadata S1 (168 ops/s, write leg) | append-log `fsync` **per payload** — group-commit never coalesces *across files* (one coordinator per open log) | `groupcommit.go` |
| SMB < NFS | `FlushPendingWriteForFile` + atime writes **per op** | `internal/adapter/smb/handlers/{write,read}.go` |

**One principle fixes all three:** honor UNSTABLE — fsync only at real durability points (NFS COMMIT, SMB FLUSH/CLOSE, drain), and make the append-log fsync leader **per-store** (coalesce N fds into one journal commit) instead of per-file. Durability contract is preserved (group-commit, durable-before-return — never async write-back). This is exactly #1416's unanswered "why only 64 files/s."

## Ranked roadmap (impact × ease)

1. **#1572-A · `fmt.Sscanf`→`strconv`** in the three `parseBlockIdx` sites (badger/pg/sqlite). Trivial, mechanical, kills ~32% of read CPU on its own. *Do first.* → `1572-read-manifest-enumeration.md`
2. **Defer per-op durability (the shared root)** — per-store append-log fsync leader + honor UNSTABLE on the write path and SMB; lazy atime. Moves write-path 3×, SMB read/write, and the metadata *write* leg together. Moderate effort, one design. Risk: wider crash-loss window for un-COMMITted data — protocol-legal (write `Verf` already emitted), *provided every durability point fsyncs*. → `write-path-throughput.md`, `smb-vs-nfs-throughput.md`, `1573-metadata-group-commit.md`
3. **#1572-B · `GetFileChunkAtOffset`** — single indexed Get with a `off ≤ T < off+DataSize` covering check, routed onto both read hot paths (incl. the prefetch probe), replacing full `ListFileChunks` enumeration. Kills the O(N²). **Guard:** the covering check must gate the result or a hole serves a neighbour chunk's bytes (silent corruption) — cover with storetest hole/EOF cases. → `1572-read-manifest-enumeration.md`
4. **#1573-S4 · same-dir create OCC** — `createEntry` RMWs the parent inode → badger SSI conflict → retry-backoff serializes same-dir creates. Dir-level create batching / lazy parent-mtime. Fixes the metadata *create* leg. → `1573-metadata-group-commit.md`
5. **#1466 · S3 multipart** — the adaptive window settling at ~24 is *correct* (c64 494 < c24 561 = the goodput knee); more `PutObject` concurrency is a dead lever. The gap to the 1253 ceiling is architectural: one `PutObject` per 16 MiB block vs rclone's N concurrent multipart parts. Add multipart to `PutBlock` + carve 16→64 MiB. **Risk:** if the 561 wall is client-CPU (TLS + double-BLAKE3), multipart alone won't reach 1253 — the before/after pprof at c24/c64 discriminates CPU-wall vs network-wall. → `1466-upload-multipart-concurrency.md`
6. **#1569 · per-share chunk size** — range-GET already done; the only real lever for random-access shares is a smaller FastCDC floor (min 1 MiB → less amplification, at the cost of more rows/objects/worse dedup), or sizing/pinning the local cache. Low priority. → `1569-range-reads.md`

## Before/after harness (common shape)

Each plan specifies exact commands. Pattern: capture the **before** baseline (already have #1572's: fio randwrite→settle→randread = 2 IOPS + pprof), implement, re-run the same fio/`dfsctl bench`/`bench/parity` workload with a 25–30 s CPU profile from `:9090/debug/pprof/profile` (or `:8080` with the dfsctl token), and diff the hot-function shares. Targets: #1572 → thousands of read IOPS, `ListFileChunks`/`Fscanf` out of the profile; write-path/SMB → rclone/NFS parity, `syscall.Fsync` collapsed; metadata → ~1200 ops/s, fsyncs/file 3→1; #1466 → ≥900 Mbit/s single-stream.
