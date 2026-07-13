# Local sequential-write throughput: 130–149 MB/s vs rclone 408 MB/s (~3×)

Investigation only. Verified against `origin/develop` @ `0b2361b8`. No code changed.

## TL;DR

The gap is **fsync-per-write on the append-log**, not FastCDC/BLAKE3/upload.

`internal/adapter/nfs/v3/handlers/write.go` deliberately defaults every WRITE to
**UNSTABLE** — the comment even says the data "leaves the data in the local cache
(crash-safe via the WAL) and the client calls COMMIT when it needs durability."
But the layer underneath ignores that intent: `pkg/block/local/fs/appendwrite.go
AppendWrite` **unconditionally** calls `lf.groupCommit.Sync(ctx)` (→ `f.Sync()`,
a real fsync) on *every* framed record. For a single sequential stream there is
exactly one in-flight writer, so `groupCommit`'s adaptive-bypass
(`pkg/block/local/fs/groupcommit.go`) fires an **inline fsync on every call** —
zero coalescing. On SCW cloud block storage an fsync is milliseconds, so ~150
fsyncs/s ≈ 150 MB/s at a 1 MiB NFS wsize. rclone's file-cache ingest writes to
the page cache and never fsyncs until flush/close → memory-bandwidth-bound 408 MB/s.

FastCDC boundary scan (#1555: ~0.63 ms/MiB ≈ 1.6 GB/s/core) and BLAKE3 run on the
**async rollup path** (`pkg/block/local/fs/rollup.go rollupFileInner`, background
workers), off the client-ack latency path. They only gate throughput indirectly,
when sustained writes fill `maxLogBytes` and `AppendWrite` blocks on `pressureCh`.

## The local ingest path (per client WRITE, what the client waits on)

```
NFS/SMB WRITE handler
  → runtime → engine.Store.WriteAt        (pkg/block/engine/readwrite.go)
     → local.AppendWrite                  (pkg/block/local/fs/appendwrite.go)
        1. writeRecord(lf.f, off, data)   -- ONE pwrite: 16B frame + payload (memcpy + CRC32c, HW-accel)
        2. lf.groupCommit.Sync(ctx)       -- ***fsync EVERY record*** (the wall)
        3. idx.Append + tree.Insert       -- in-memory, cheap
     → return; syncer uploads in background
--- asynchronous, NOT on the ack path ---
  rollupFileInner: reconstructStream → FastCDC (chunker.Next) → BLAKE3 → stageRollupChunk → 1 fsync/pass
  carver/syncer: codec → PutBlock to S3
```

## Stage attribution (single-stream sequential, `--remote memory`)

| Stage | Where | On ack path? | Cost | Verdict |
|---|---|---|---|---|
| **Append-log fsync** | `appendwrite.go` `groupCommit.Sync` | **YES, every write** | ms-scale cloud-disk fsync; no coalescing single-stream | **DOMINANT — the 3×** |
| pwrite + CRC32c | `writeRecord` (appendlog.go) | yes | CRC32c HW-accel (Castagnoli); ~GB/s | negligible |
| payload alloc / frame | `writeRecord` `make([]byte,...)` | yes | 1 alloc/record | minor; poolable |
| FastCDC boundary scan | `chunker.Next` via `rollupFileInner` | no (async) | ~0.63 ms/MiB (#1555), ~1.6 GB/s/core | secondary ceiling only under backpressure |
| BLAKE3 | `blake3ContentHash` (rollup.go) | no (async) | ~1/3 of scan | secondary |
| reconstruct buffer | `reconstructStream` (pooled) | no (async) | pooled `getReconstructBuf` | already mitigated |
| metadata commit | ObjectIDPersister | no (async, per rollup pass) | once per pass | not per-write |

## Fixes, ranked by expected win

### 1. Honor UNSTABLE — defer the append-log fsync (THE fix, ~3× → rclone-parity)
Make `AppendWrite`'s `groupCommit.Sync` **conditional**. UNSTABLE writes append
to the log (page cache) and return **without fsync**; durability is paid at the
existing durability points that already exist:
- NFS `COMMIT` → `commit.go` → `common.CommitBlockStore`
- NFS DATA_SYNC/FILE_SYNC WRITE → `write.go flushStableWrite` → `CommitBlockStore`
- SMB Flush/close, snapshot `DrainRollups`, graceful stop

This is precisely what write.go's own comment already promises and what rclone
does. Expected: page-cache-bound, toward 408 MB/s.

Mechanics:
- Add `FSStoreOptions.SyncEveryWrite bool` (default **false**). Thread a
  `sync bool` (or reuse a per-write stability hint) into `AppendWrite`; skip
  `groupCommit.Sync` when false, but still advance `eofPos`/`idx`/`tree` (those
  are in-memory and already correct pre-fsync).
- Track an **unsynced watermark** per `logFile` (`syncedPos <= eofPos`). Add
  `FSStore.Sync(ctx, payloadID)` that fsyncs `lf.f` via `groupCommit.Sync` and
  advances `syncedPos`. Wire it into `engine.Store.Flush`/`CommitBlockStore` so
  COMMIT fsyncs the log **before** returning durable.
- Optional safety valve: a time/byte-bounded background flusher (e.g. fsync any
  log dirty > N ms or > M bytes) so an idle-then-crash window is bounded even
  without a client COMMIT. `groupCommit` already has the fan-in plumbing to add
  a timer arm.

Crash-safety: NFS UNSTABLE explicitly permits losing un-COMMITted data; the
client re-sends when the write verifier (`Verf: serverBootTime`, already emitted)
changes across a reboot. So deferral is protocol-correct.

### 2. Group-commit the fsync across concurrent streams (helps multi-stream only)
`groupCommit` already coalesces concurrent in-flight fsyncs, so parallel writers
to different/same files already benefit. Single-stream gets nothing from it —
which is why fix #1 (defer, not coalesce) is the lever. Low extra work; mostly
already done.

### 3. Rollup-path CPU (only matters once #1 removes the fsync wall)
Once ingest is page-cache-bound, a sustained large write can outrun the 2-worker
rollup pool and hit `maxLogBytes` backpressure. Then FastCDC/BLAKE3 throughput
becomes the ceiling. Levers, in order:
- Raise default `rollupWorkers` (currently 2) / size to `GOMAXPROCS`.
- Pool the `writeRecord` payload alloc + the per-chunk hash path (sync.Pool);
  assert `-benchmem bytes/op` stays flat vs input size (#1555 Refinement 2).
- FastCDC: SIMD/wider gear-hash window or a cheaper rolling hash; verify no
  double-hash between `blake3ContentHash` (rollup) and the carve/codec path
  (`carver.go carveAndCommitBlock`) — #1266 flagged a possible double-BLAKE3 on
  the upload side. Confirm the carver reuses the rollup's hash rather than
  re-hashing.
- These are all **background-throughput**, secondary to #1.

## Before/after benchmark + pprof

Use the existing harness (no new tooling — per #1555). `--remote memory` makes
uploads instant so the measurement isolates the **local ingest** path; the
FSStore still writes+fsyncs a real append-log in a temp dir, so the fsync cost is
faithfully present.

### Command (run on the SCW POP2-HC box, same disk as the rclone test)

```bash
go build -o dfsbench ./cmd/bench

# BASELINE (develop, fsync-per-write)
./dfsbench blockstore \
  --workload sequential-write \
  --remote memory \
  --block-size $((1<<20)) \     # 1 MiB, matches NFS wsize
  --ops 20000 \                 # ~20 GiB, long enough to be steady-state
  --working-set 1 \
  --phase baseline \
  --profile-dir ./prof

# POST-FIX (SyncEveryWrite=false / deferred fsync)
./dfsbench blockstore --workload sequential-write --remote memory \
  --block-size $((1<<20)) --ops 20000 --working-set 1 \
  --phase post-fix --profile-dir ./prof
```

Captures land in `./prof/blockstore/{baseline,post-fix}/sequential-write-<ts>/`
with `cpu.pprof`, `heap.pprof`, `goroutine.pprof` (add `--full-profiles` for
mutex/block).

### Metrics & targets

- **Primary: throughput (MB/s)** from `printResult`. Baseline ~130–149 MB/s →
  target ≥ 400 MB/s (rclone parity), stretch = local-disk write-bandwidth ceiling
  (~500 MB/s on the box). Confirm the local-disk ceiling once with
  `dd bs=1M count=20000 conv=fdatasync` vs no fdatasync.
- **CPU proof:** `go tool pprof -top cpu.pprof` — baseline shows `syscall.Fsync`/
  `Fdatasync` (via `os.(*File).Sync` ← `groupCommit.Sync`) as a top leaf; post-fix
  it should collapse to near-zero on the ingest path. This is the direct
  before/after signal that the fsync was the wall.
- **Alloc proof:** `go tool pprof -top heap.pprof` — `-benchmem`/heap bytes/op must
  stay flat as `--block-size` grows (streaming/pool invariant, #1555 Refinement 2);
  watch `writeRecord` payload alloc and `getReconstructBuf`.
- **End-to-end confirm:** repeat over real NFS mount with
  `dfsctl bench --workload seq-write` (or `fio`/`dd` to the mount) to verify the
  microbench win shows through the full NFS RPC path, not just the engine.

### Durability regression gate (must stay green)
- `pkg/block/blockstoretest` conformance + append conformance.
- A crash/durability test: UNSTABLE writes + `COMMIT` → kill → recover → data
  present; UNSTABLE writes + crash **without** COMMIT → loss tolerated + verifier
  (`Verf`) changed (protocol-correct). Mirror the existing
  `rollupPreSyncFailHook` durability-test style.
- SMB Flush/close and NFS DATA_SYNC/FILE_SYNC still fsync (report `committed`
  correctly).

## Risk

Deferring the append-log fsync widens the crash-loss window for un-COMMITted
UNSTABLE data. This is protocol-legal for NFS (write verifier already implemented)
but the fix **must** guarantee every durability point fsyncs the log: NFS COMMIT,
DATA_SYNC/FILE_SYNC WRITE, SMB Flush/close, snapshot drain, graceful stop.
Miss one and a client that trusts COMMIT loses acknowledged-durable data. Keep
`SyncEveryWrite=true` as an operator opt-out for paranoid/no-COMMIT clients.
