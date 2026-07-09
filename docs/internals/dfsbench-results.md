# dfsbench Results — 2026-07-09

Fresh benchmark pass from the `dfsbench` harness (`internal/dfsbench/`, issue #1602). This
supersedes the stale numbers in [`../BENCHMARKS.md`](../BENCHMARKS.md) for the workloads covered
here. It is a **partial** pass — read it as a first real data point, not a complete matrix.

## What this run measured

- **Subject:** `dittofs-s3` — DittoFS serving BadgerDB metadata + a local fs cache + an S3 remote
  block store, mounted over its own **native** userspace NFSv3 server (no FUSE, no knfsd).
- **Competitor:** `rclone` — a FUSE writeback mount (`--vfs-cache-mode writes`) re-exported over
  kernel NFS (knfsd). This is the "FUSE re-export" archetype every S3-FUSE tool shares.
- **Anchor:** `local-disk` — bare fio against a scratch dir (no FS layer, no S3): the hardware
  ceiling every cell is read against.
- **Not captured:** `zerofs` (native NFS twin) — its mount fails through the harness with a
  binary-only Heisenbug (see [Known gaps](#known-gaps)). `juicefs` / `s3fs` / `s3ql` not run this
  pass.

Single-tenant Scaleway VM (fr-par-1), S3 = Scaleway Object Storage (Paris), 256 MiB files, 4
fio jobs, 60 s/pass, NFSv3. Reads run a **warm** pass then a **cold** pass after evicting the
local cache (`store block evict` for DittoFS; a mount-bounce for FUSE), so cold = genuinely
served from S3.

## Baseline (ceilings)

| | seq | rand-4k |
|---|---|---|
| local-disk (bare scratch) | **866 MB/s** | **5,082 IOPS** |

## Comparison — DittoFS vs competitor

| Workload | Pass | **DittoFS** (native) | **rclone** (FUSE re-export) | Gap | Read against ceiling |
|---|---|---|---|---|---|
| seq-read | warm | **972 MB/s** | 1,369 MB/s | rclone **1.4×** | both cache-served (>ceiling) |
| seq-read | **cold** | **63 MB/s** | — *(flaked)* | — | real S3 cold fetch |
| rand-read-4k | warm | **7,416 IOPS** | — *(flaked)* | — | ~1.5× ceiling (cached) |
| rand-read-4k | **cold** | **6,405 IOPS** | — *(flaked)* | — | S3-served |
| seq-write | warm | **1 MB/s** | 1,936 MB/s | **rclone ~1900×** | rclone local-acks |

*rclone's random-read and cold cells hit its documented NFSv3 `ftruncate` flake and are omitted.*

### FUSE-tax (system-wide context switches during the pass)

The harness meters `/proc` context switches — the tax a FUSE re-export pays crossing
kernel↔userspace that a native server avoids.

| Workload (warm) | DittoFS CTXSW/s | rclone CTXSW/s | CPU% (Ditto / rclone) |
|---|---|---|---|
| seq-read | **38,944** | 64,465 | 44 / 45 |
| seq-write | 5,921 | 173,491 | 3 / 57 |

On seq-read, DittoFS delivers 71% of rclone's throughput at **60% of the context switches** —
native serving is measurably more CPU-efficient per byte. (The seq-write row isn't comparable:
DittoFS is stalling, not working — see below.)

## Reading the gaps

1. **Reads: competitive and more efficient.** Warm seq-read 972 MB/s (cache-served, above the
   disk ceiling); the eviction barrier drops it to a real **63 MB/s cold-from-S3**. Random reads
   hold 6.4k IOPS cold. DittoFS trails rclone's warm read by 1.4× but does it with ~40% fewer
   context switches — the native-serving thesis holds.
2. **Writes are the wall — and it reproduced every run.** DittoFS native seq-write is **~1 MB/s**
   with **multi-second** per-I/O latency (p50 4.5 s, p99 8.1 s) and CPU pinned at **3%** — the
   signature of *stalling on a synchronous commit*, not doing work. rclone's writeback local-acks
   at ~1,900 MB/s. This is the single largest gap and points straight at the tracked write-path
   work (per-write/commit fsync — #1573, #1466, #1416). The legacy `BENCHMARKS.md` claim that
   "writes never block on S3" does **not** hold on this NFS→S3 path today.

## Known gaps

- **zerofs (native twin) — blocked by a harness Heisenbug.** zerofs mounts and serves NFSv3
  correctly every way it is tested by hand (validated repeatedly on the VM), but fails 100%
  through the `dfsbench` binary with `mount.nfs: Protocol family not supported` — identical
  commands, different outcome. Root cause is not yet found; it needs binary-level tracing of the
  backend's exec path, not more live guessing. The backend fix already includes: waiting for
  zerofs's real "Starting NFS server" readiness line (it binds the socket ~35 s before it serves,
  after loading its encryption key + warming the LSM from S3), stopping the boot-time kernel NFS
  server so zerofs can claim port 2049, and a mount retry.
- **Single size / three workloads.** 256 MiB only; `mixed-rw` and `metadata` not run this pass.
- **rclone NFSv3 flake.** Its random-read and cold cells are unreliable over NFSv3 (dir-cache
  empties → fio `ftruncate` EIO); nfs4/smb3 are steadier. Recorded, not counted.

## Harness fixes landed with this doc

The `dittofs-s3` backend recipe was stale (written against an older `dfsctl`, never validated
against a live server). This pass is the first that drove a real `dfs`, and it fixed the whole
bring-up: set `DITTOFS_CONTROLPLANE_SECRET` + a known admin password, `dfsctl login`, create the
metadata + local + remote stores with the current flag schema, and create the share with
`--default-permission read-write` so the squashed NFS root can write. Plus a shared
`prepareMountpoint` that force-lazy-unmounts a stale mount before mounting — without it, a mount
left by a crashed run wedges the next run in uninterruptible D-state.
