# v1.0 audit — Wave 0 perf baseline

Captured 2026-05-28 against HEAD `a06bc94e` on branch
`v1.0/wave0-mechanical-cleanup` (post Wave 0 mechanical cleanup, pre
Wave 1).

Per `.planning/v1.0-audit/PLAN.md` Wave 0 step 6. Per-area audits in
Wave 1 should diff their post-fix profile against the artifacts here.

---

## Macro-bench (Scaleway, public-IP NFSv3)

Two ephemeral VMs from Pulumi `bench` stack:

- **Server**: `212.47.241.220` (Scaleway, fr-par, 4 vCPU, 7.7 GiB RAM,
  150 GB block volume at `/data`). `dittofs-badger-s3` system.
- **Client**: `51.15.199.235` (Scaleway base stack, persistent).

DittoFS deployed from the local cleaned tree (HEAD `a06bc94e`),
cross-compiled `linux/amd64` and SCP'd to `/usr/local/bin/{dfs,dfsctl}`.
Pprof enabled via `controlplane.pprof: true` + env
`DITTOFS_CONTROLPLANE_PPROF=true`. Both shares mounted via NFSv3 on TCP
port 12049 over the public IP path (private network exists but client
mount uses public).

Two shares:
- `/export` — badger metadata + local fs block store (no remote)
- `/export-s3` — badger metadata + local fs block store + Scaleway S3
  remote (`dittofs-bench` bucket, fr-par, prefix `v1.0-baseline/`)

### Workload parameters

Reduced from the canonical `scripts/run-bench.sh` defaults to make the
bench complete within a tractable VM lifetime — see "Reliability notes"
below for why.

| Param        | Baseline used   | Canonical default |
|--------------|-----------------|-------------------|
| Duration     | 30s             | 60s               |
| Threads      | 4               | 4                 |
| File size    | 128 / 256 MiB   | 1 GiB             |
| Block size   | 4 KiB           | 4 KiB             |
| Workloads    | seq-write only  | all 6             |

### Results

**`dittofs-s3` (S3-backed share), seq-write @ 128 MiB / 30s / 4 threads:**
- Throughput: 115.0 MB/s
- Latency P50/P95/P99: 1.3 ms / 1.6 ms / 2.3 ms
- Ops: 512, bytes: 512 MiB
- 4 errors during the run (uncategorized — bench harness counts but
  doesn't log)
- Full JSON: `macro/s3/seq-write-result.json`

**`dittofs-fs` (local fs share), seq-write @ 128 MiB / 30s / 4 threads:**
- **Did not complete.** Stalled at ~75% progress indicator for ≥60s and
  was killed. No JSON was emitted. Server log
  (`macro/fs/server-errors.log`) shows continuous rollup failures
  during the stall:
  - `rollup: tree/logIndex divergence — stable interval [0,1048576)
    has no logIndex entries`
  - `rollup: ObjectIDPersister: engine: object_id already mapped to
    another file (Conflict: badger PutFile)`
  - `file-level dedup: increment refcount on target hash <UUID>: no
    FileBlock with hash <UUID>: file block not found`
- A previous attempt (1 GiB, 60s, all workloads) made it through
  seq-write + rand-write (rand-write 100% reached) before hanging in
  the final commit/sync of rand-write — `dfsctl bench` ended in
  uninterruptible disk-wait (`Dl`) and could not be `kill -9`'d; only
  recovered by `systemctl restart dfs.service` on the server.

These are reproducible against the cleaned tree. They are surfaced as
Wave 1 follow-ups below (B2/B6/B7).

### pprof profiles captured

All under `http://212.47.241.220:8080/debug/pprof/*`. CPU profiles are
30 s (s3) or 15 s + 20 s stall sample (fs) wall-clock samples taken
during the seq-write workload.

| Dimension   | `macro/fs/`              | `macro/s3/`                      |
|-------------|--------------------------|----------------------------------|
| CPU (run)   | `cpu.pprof`              | (not captured — completed too fast) |
| CPU (stall) | `cpu-stall.pprof`        | n/a                              |
| Heap        | `heap.pprof`             | `heap-postseqwrite.pprof`        |
| Goroutine   | `goroutine.pprof`        | `goroutine-postseqwrite.pprof`   |
| Allocs      | `allocs.pprof`           | `allocs-postseqwrite.pprof`      |
| Mutex       | `mutex.pprof` (empty)    | `mutex-postseqwrite.pprof` (empty) |
| Block       | `block.pprof` (empty)    | `block-postseqwrite.pprof` (empty) |

Mutex and block profiles are present as ~235-byte header-only
responses — `runtime.SetMutexProfileFraction` /
`runtime.SetBlockProfileRate` are never called in `dfs`. See follow-up
B3 below; mutex/block dimensions for Wave 1 will require a one-line
runtime registration tied to the pprof config flag.

### Reproducing

```bash
# Server side (212.47.241.220)
# 1. Cross-build from current tree:
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o .build/dfs ./cmd/dfs
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o .build/dfsctl ./cmd/dfsctl
scp .build/dfs .build/dfsctl root@<server>:/usr/local/bin/

# 2. Enable pprof via env on dfs.service unit
# 3. Recreate stores + shares per scripts/run-bench.sh §3.

# Client side
mount -t nfs -o tcp,port=12049,mountport=12049,hard,vers=3,rsize=1048576,wsize=1048576 \
  <server>:/export-s3 /mnt/bench-s3
dfsctl bench run /mnt/bench-s3 --system dittofs-s3 \
  --duration 30s --threads 4 --file-size 128MiB --block-size 4KiB \
  --workload seq-write --clean --save /tmp/dittofs-s3.json
```

---

## Micro-bench gates (D-40 / D-41 / D-42)

Run on host (Apple M1 Max, darwin/arm64, Go 1.25.x). Profile artefacts
under `micro/`.

### D-40 — AppendWrite group-commit median (`pkg/blockstore/local/fs/appendwrite_group_commit_bench_test.go`)

`BenchmarkAppendWrite_GroupCommit -benchtime=5x`, 5 runs:

| Run | ns/op    |
|-----|----------|
| 1   | 7 573 683 |
| 2   | 6 971 217 |
| 3   | 6 983 375 |
| 4   | 6 384 300 |
| 5   | 6 579 792 |

Median: **6.97 ms/op** (8 fsyncs total, 1 fsync per op, 26 allocs, 2.7
KiB B/op for the 3-iter run).

No legacy `tryDirectDiskWrite` comparator is exposed as a Go test —
the D-40 ratio gate documented in `test/e2e/BENCHMARKS.md` is run
manually; not automated. Profiles: `micro/d40-{cpu,mem}.pprof`,
`micro/d40-{bench,5runs}.txt`.

### D-41 — BLAKE3 ≥ 3.0× SHA-256 on 256 MiB (`pkg/blockstore/hash_bench_test.go`)

Platform-aware gate (amd64 ≥ 3.0×, arm64 ≥ 0.5×). On arm64 (M1 Max):

- BLAKE3: 66.27 ms/op → 4051 MB/s
- SHA-256: 110.98 ms/op → 2419 MB/s
- Ratio: **1.67×** (SHA-256 / BLAKE3 — i.e. BLAKE3 is 1.67× faster on
  darwin/arm64 because Apple silicon has HW SHA accel)

Test passes its arm64 gate (≥ 0.5×). The amd64 ≥ 3× gate is NOT
verified by this baseline — needs to be re-run on a Linux/amd64 host.
Profiles: `micro/d41-{cpu,mem}.pprof`, `micro/d41-{bench,gate}.txt`.

### D-42 — FastCDC boundary stability ≥ 70% (`pkg/blockstore/chunker/chunker_test.go`)

`TestChunker_BoundaryStability_70pct` over 20 iterations of 1–4096-byte
prefix shifts:

- Mean preservation: **1.000** (perfect — every shift preserves all 62
  boundaries)
- Gate met (≥ 0.70).

Profiles: `micro/d42-gate.txt`.

---

## Wave 1 follow-up gaps surfaced during baseline

These are not Wave 0 blockers but should be tracked by the relevant
Wave 1 area-pair or by the Wave 3 bench refactor.

### B1 — `dfsctl bench run` progress reporting is misleading and hangs

The `seq-write: N%` progress indicator does not track real bytes written
and freezes for minutes around 50-75% before either resuming or
hanging completely. With 4-thread × 1 GiB workload over WAN, the
seq-write phase took ≥ 15 minutes despite a 60 s nominal `--duration`.
With 128 MiB it stalls at 75% indefinitely on the local-fs share.
Workload duration semantics need a redesign — currently the bench is
not reliable enough to be a CI gate or a reproducible audit baseline.

### B2 — Block-store rollup wedges on residual state from interrupted writes

After a partial write fails (`context canceled` from a killed client or
restarted server), the rollup loop emits one of two errors in a tight
loop forever for the offending payloadID:

```
rollupFile failed payloadID=export/_dfsctl_bench/seq_write_N.dat
  error="rollup: tree/logIndex divergence — stable interval [0,1048576)
         has no logIndex entries" source=ticker
```

```
rollupFile failed payloadID=export/_dfsctl_bench/seq_write_N.dat
  error="rollup: ObjectIDPersister: engine: object_id already mapped to
         another file\nConflict: badger PutFile: object_id already
         mapped to file <UUID>" source=ticker
```

The mappings are durable in badger; the only recovery is to wipe both
metadata and block-store directories and re-create the share. No
self-healing path exists. Wave 1 file-lifecycle / rollup area-pair
should add either a startup sweep that GCs orphan object_id→file
mappings, or a reconcile path that detects and rebinds.

Sample errors in `macro/fs/server-errors.log` (1398 lines).

### B6 — File-level dedup refcount-increment on missing FileBlock

```
file-level dedup: file-level dedup: increment refcount on target
  hash <UUID>: coordinator: no FileBlock with hash <UUID>: file block
  not found
```

Observed during the same fs-share run. Suggests dedup index +
file-block table can diverge under crash/restart, with the dedup
short-circuit referencing a hash that has no FileBlock backing.

### B7 — Client-visible NFS hang requires server restart

When `dfsctl bench` is killed mid-rand-write at 1 GiB, the dfsctl
process enters uninterruptible disk-wait (`Dl`) and cannot be killed
even with `SIGKILL`. The NFS mount is not responsive to `umount -f` or
`umount -l`. Only recovery is `systemctl restart dfs.service` on the
server, after which the client process becomes a zombie. NFS COMMIT
semantics: the server is acking writes but not finishing the COMMIT
phase for some payloadIDs (likely tied to B2's rollup wedge).

### B3 — Mutex / block pprof endpoints return empty profiles

`runtime.SetMutexProfileFraction` and `runtime.SetBlockProfileRate` are
never called. With `controlplane.pprof: true`, `/debug/pprof/mutex` and
`/debug/pprof/block` return 200 with ~235-byte header-only bodies.
Wave 1 control-plane area-pair (or wherever pprof lives) should tie
these to config flags (e.g. `controlplane.pprof_mutex_rate: 100`,
`pprof_block_rate_ns: 1_000_000`).

### B4 — `bench/infra/scripts/dittofs-badger-s3.sh` hard-codes `main`

The install script clones from `DITTOFS_BRANCH=${DITTOFS_BRANCH:-main}`.
For per-PR / per-branch audits this requires env override at every SSH
call. Wave 3 bench refactor should accept a pre-built local binary
(`DFS_BIN=/path/to/dfs`) and skip the in-script `go build`.

### B5 — Bench VM uses public IP for NFS

The private NIC is provisioned but the client mount uses the bench
server's public IP. Adds ~0.4 ms RTT and is a cost + attack-surface
footgun if a bench VM is left running. Wave 3 should switch to
private-NIC binding.

### B8 — `bench run` `errors` field not surfaced

The `dittofs-s3` result counts 4 errors but `dfsctl bench run` table
output ("WORKLOAD…") omits the count. Only visible in the saved JSON.
Bench refactor should surface error counts inline.

---

## Tear-down

```bash
cd bench/infra && PULUMI_CONFIG_PASSPHRASE="" pulumi destroy --stack bench --yes
```

The base stack (client VM + private network + S3 bucket) is preserved.
