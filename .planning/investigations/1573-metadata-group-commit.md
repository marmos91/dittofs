# #1573 — Metadata group-commit: naming the per-op fsync serializer

Investigation only. All code verified against `origin/develop` @ `0b2361b8`.
Bottleneck: DittoFS pure-metadata ~168 ops/s (create/stat/delete small files) vs
rclone 1384 / Ganesha 1154 on SCW POP2-HC-32C-64G. Each small file over NFSv3 =
CREATE + WRITE + COMMIT.

---

## 1. The actual synchronous fsync serializers (named + cited)

There is no single serializer. There are **three** synchronous durability points
per small file, and which one dominates depends on the workload's directory
fanout and whether files carry content. All three must be named because the fix
surface differs.

### S1 — Append-log per-**file** inline fsync (on the WRITE RPC)

- `AppendWrite` fsyncs every framed record via `lf.groupCommit.Sync(ctx)`
  — `pkg/block/local/fs/appendwrite.go` (`AppendWrite`, ~L360–585; the mutex
  window is documented as "pwrite + fsync + tree.Insert").
- The coordinator is **per open log file** (one `groupCommit` per payload,
  allocated in `getOrCreateLog`, `appendwrite.go` ~L192–299):
  `pkg/block/local/fs/groupcommit.go:9` — *"Scope: one coordinator per open log
  file … different log files fsync independently."*
- Its **adaptive bypass** (`groupcommit.go`, `Sync`, ~L59–96) fires the fsync
  **inline** when the queue is empty and no fsync is in flight.
- Consequence for a small-files workload: **every op is a different payload →
  different coordinator → the group-commit never coalesces across files.** Each
  file's single write does its own inline `f.Sync()`. The existing group-commit
  only helps concurrent writers to the *same* file (SMB append, large sequential
  write) — it does nothing for the "N files, one write each" workload.
- `layout: {baseDir}/logs/{payloadID}.log`, one fd per payload
  (`appendwrite.go` `logPath` ~L167). The log fd is owned for the FSStore
  lifetime, not pooled.

NFS COMMIT does **not** add a block fsync here: `common.CommitBlockStore`
(`internal/adapter/common/write_payload.go:105`) calls `engine.Flush`, and for
the fs-local store `localDurable=true` → it acks immediately (a drain, not an
fsync). The block bytes were already made durable by S1 on the WRITE.

### S2 — Badger CREATE commit fsync

- `createEntry` writes the dir-entry + child inode + parent WCC inside **one**
  `store.WithTransaction` → `s.db.Update` (`pkg/metadata/file_create.go:182`;
  transaction wrapper `pkg/metadata/store/badger/transaction.go:100`).
- The store forces `opts = opts.WithSyncWrites(true)`
  (`pkg/metadata/store/badger/store.go:263`, crash-consistency invariant #583/#588,
  pinned by `sync_writes_test.go`). → **one value-log fsync per CREATE.**
- Badger **does** internally group-commit concurrent `writeCh` submissions (one
  `vlog.sync()` per batch). So 16-way CREATEs *should* coalesce — **unless S4
  prevents them ever being concurrent at the writeCh.**

### S3 — Badger COMMIT-flush fsync

- NFS COMMIT → `FlushPendingWriteForFile` (`pkg/metadata/io.go:379`) →
  `flushPendingWrite` → badger commit → **second fsync per file** (persists
  size/mtime). The WRITE RPC's metadata update is deferred into `pendingWrites`
  (no badger fsync on WRITE), so metadata durability is one fsync at COMMIT.
- Serialized per-file by `pendingWrites.GetFlushLock(handle)` (per-file lock —
  no cross-file contention).

So per small file: **1 badger fsync (CREATE) + 1 append-log fsync (WRITE) + 1
badger fsync (COMMIT)** ≈ 3 fsyncs, and the append-log one never coalesces
across files.

### S4 — Same-parent-directory OCC conflict retry (the #1416 open question)

`createEntry` re-reads the **parent inode** inside the create txn and mutates it
(adds the child entry via `SetChild`, brackets parent WCC before/after, bumps
parent mtime). N concurrent creates **in the same directory** all
read-modify-write the same parent-inode key → badger SSI (serializable) abort →
`WithTransaction`'s retry loop (`transaction.go`, `maxTransactionRetries=20`,
exponential backoff **1–5ms jittered**). This **serializes same-dir creates**
regardless of fsync batching, and is the most likely reason badger's native
group-commit "isn't absorbing" the 16-way writers that #1416 measured (~64
files/s ≈ one fsync of latency per file). This is a **hot-key contention**
serializer, not an fsync.

### Verdict (to be confirmed by the experiment in §4)

- **Write/small-file workload:** S1 (append-log per-file inline fsync) dominates,
  because it is the one durability point that *structurally cannot* coalesce
  across files, and OS filesystem-journal commits (ext4 `data=ordered`) tend to
  serialize independent-fd fsyncs anyway.
- **CREATE-only, same-directory workload:** S4 (parent-inode OCC conflict) is the
  serializer; badger never sees 16 concurrent independent commits, so S2's native
  batching is defeated.
- **CREATE-only, one-dir-per-file workload:** badger group-commit should absorb
  S2; if it does not, the residual serializer is inside badger's `doWrites`
  (single vlog fsync goroutine) and is not something we can beat without relaxing
  SyncWrites (rejected — #583).

The pprof/fsync trace in §4 decides which of S1/S4 to fix first.

---

## 2. Backend-agnostic group-commit design

Goal: **coalesce N concurrent independent durable ops into one durable commit,
returning each caller only after ITS bytes/records are durable** (group-commit,
NOT async write-back — NFS COMMIT/SMB Flush durability contract preserved). Reuse
the leader/piggyback pattern that already exists in `groupcommit.go`
(`inFlight` guard + broadcast fan-in), hoisted to the boundaries that don't
currently coalesce.

### Fix A — Append-log: per-**store** fsync leader (targets S1)

Replace the per-payload `groupCommit` with **one coordinator per `FSStore`**.
Each payload keeps its own fd/pwrite; the *fsync phase* is batched behind a single
store-level leader:

1. Writer does its `pwrite` under the per-file mutex (unchanged), then enqueues
   its fd on the store leader and blocks.
2. The leader collects all fds pending within a tiny window, issues their fsyncs,
   then broadcasts completion to all waiters.
3. On ext4/xfs the concurrent per-fd fsyncs collapse into **one journal
   commit**, so N files amortize to ~one commit's latency.

Keep the existing adaptive bypass so a lone writer sees zero added latency.
This is the smallest change that makes the "N files, one write each" workload
coalesce. *ponytail: cheapest realization is a 0.5–1ms time-batched leader; only
add fd-dedup/fairness if the profile shows tail latency.*

### Fix B — Metadata: `WithTransaction` group-commit at the store durability boundary (targets S2/S3, postgres/sqlite mainly)

Add an optional store-level commit-leader (same pattern) so concurrent
`WithTransaction` closures that have finished their reads/writes join one durable
commit. Realizations:

- **badger:** native `writeCh` coalescing already does this — **no new layer**;
  the win comes from Fix C (remove S4 so concurrency reaches the writeCh).
  *(ponytail: don't wrap badger in a second group-commit; measure first.)*
- **postgres:** a shared "commit leader" goroutine batches pending txns into one
  `COMMIT` (one WAL fsync) — big win, each COMMIT currently fsyncs.
- **sqlite:** WAL mode + a commit-leader that batches into one `COMMIT`
  (`PRAGMA synchronous=NORMAL` + periodic checkpoint) — big win.
- **memory:** no-op (already durable-free).

Contract: a single narrow addition, `DurableBarrier(ctx) error` / an internal
"join current commit group" primitive on the metadata store — default
implementation = call through immediately (memory/badger), overridden by
postgres/sqlite. Keep the interface surface minimal (per repo guidance: no new
capability-interface zoo — fold onto the existing store contract).

### Fix C — Kill the same-directory OCC hot key on CREATE (targets S4, backend-agnostic)

So concurrent creates actually arrive at the store's group-commit concurrently.
Preferred: **directory-level create batching** — serialize creates per parent dir
with a short-lived mutex + coalescing queue, then apply **N creates as ONE
transaction** (one durable commit for N files). This is the metadata analogue of
group-commit and is the highest-amortization option (turns N conflicting
single-file txns into 1 commit). Cheaper alternative: stop mutating parent mtime
*inside* the create txn (lazy/coalesced dir-mtime update, or a conflict-free
counter) so the child-entry writes stop conflicting on the parent-inode key.

---

## 3. Concrete implementation plan (files, call sites)

**Phase 1 — measure (no code change beyond bench flags).** Land the §4 harness
first; do not write a fix before the profile names S1 vs S4.

**Fix A (append-log leader):**
- `pkg/block/local/fs/groupcommit.go` — generalize `groupCommit` from
  per-file to a per-store leader that batches `(fd)` fsyncs; keep the adaptive
  bypass + broadcast.
- `pkg/block/local/fs/fs.go` / `appendwrite.go` — move the coordinator from
  `logFile` to `FSStore`; `AppendWrite` (~L533) enqueues fd on the store leader
  instead of `lf.groupCommit.Sync`.
- Preserve lock order (per-file `mu` → leader → per-store log lock); the existing
  `TestGroupCommit_NoLogsMuTouch` grep gate stays valid.
- Reuse/extend `appendwrite_group_commit_bench_test.go` and
  `groupcommit_test.go`; add a "N distinct payloads, one write each" bench that
  currently shows N fsyncs.

**Fix B (metadata store group-commit):**
- `pkg/metadata/store/*` contract: add the minimal `DurableBarrier`/join
  primitive (default pass-through).
- `pkg/metadata/store/postgres/*`, `.../sqlite/*` (if present): commit-leader
  goroutine.
- `pkg/metadata/io.go` `flushPendingWrite` and `pkg/metadata/file_create.go`
  `createEntry` route their commit through the barrier.

**Fix C (dir-batch / lazy parent mtime):**
- `pkg/metadata/file_create.go` `createEntry` (~L182): per-parent coalescing
  queue OR drop in-txn parent-mtime RMW.
- `pkg/metadata/store/badger/transaction.go`: verify conflict-retry counters drop
  (add a metric/counter for OCC retries to confirm).

Ship Fix A first (smallest diff, directly targets the structurally-uncoalescable
S1), re-measure, then Fix C, then Fix B for non-badger backends.

---

## 4. BEFORE/AFTER benchmark + pprof (prove which fsync dominates)

Harness exists: `cmd/bench/metadata.go` + `cmd/dfsctl/commands/bench/` (`run.go`,
`compare.go`). Metric: **ops/s for create/stat/delete of small files at 16-way
concurrency.** Target: ~168 → toward the **~1200 band** (rclone 1384 / Ganesha 1154).

**Run (on the SCW box, develop `0b2361b8`, badger default backend):**
```bash
# BEFORE
dfsctl bench --workload metadata --files 20000 --concurrency 16 --size 4k \
  --out before.json
# 16-way, all files in ONE directory (exposes S4)
dfsctl bench --workload metadata --concurrency 16 --dir-fanout 1   --out before_1dir.json
# 16-way, one directory per worker (isolates S1/S2 from S4)
dfsctl bench --workload metadata --concurrency 16 --dir-fanout 16  --out before_fanout.json
```
(If a flag is missing, add it to `cmd/bench/metadata.go` — cheap and part of the
diagnosis.)

**Prove the serializer:**
1. **pprof BLOCK + MUTEX profile** on `dfs` (`runtime.SetBlockProfileRate(1)`,
   `runtime.SetMutexProfileFraction(1)`; pprof already wired per #671):
   ```bash
   go tool pprof -top http://localhost:9090/debug/pprof/block
   go tool pprof -top http://localhost:9090/debug/pprof/mutex
   ```
   - Time in `os.(*File).Sync` / badger `valueLog.sync` → **fsync-bound (S1/S2/S3)**.
   - Time in `time.Sleep` inside `WithTransaction` retry, or on the flush/dir
     mutex → **OCC-bound (S4)**.
2. **fsync count** during a fixed 30s run (Linux):
   ```bash
   strace -f -c -e trace=fsync,fdatasync -p $(pgrep dfs)   # count + total time
   # or: sudo bpftrace -e 'kprobe:vfs_fsync { @[pid]=count(); }'
   ```
   Expect ≈ 3×files BEFORE; ≈ 1×(files/batch) AFTER Fix A.
3. **A/B the two hypotheses** with `--dir-fanout`: if throughput jumps 1→16
   fanout, S4 (OCC) is a serializer; if flat, S1/S2 fsync dominates.

**Acceptance:** AFTER ops/s ≥ ~4× BEFORE on the write workload with fsync count
per file dropping from ~3 to ~1 (batched), crash-consistency tests
(`sync_writes_test.go`, block durability suite) still green, and each op still
returns only after its durable barrier completes (no async-ack regression).
