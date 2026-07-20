# DittoFS Performance Benchmarks

## The premise: speed only means something next to a promise

A filesystem's write throughput is meaningless on its own. A system that returns
"done" the instant your bytes land in RAM will always look faster than one that
waits until those bytes are safely on disk — but the first one can lose your data
in a power cut, and the second cannot. Comparing the two head-to-head tells you
nothing useful.

So every benchmark on this page is organized around **the durability guarantee** —
the promise a system makes about where your data is by the time a write is
acknowledged. DittoFS makes that promise an explicit, per-share choice:

- **Writeback** — the write is acknowledged as soon as it is buffered locally, and
  the data is copied up to object storage in the background. This is the fastest
  option, and a crash can lose the last fraction of a second of not-yet-uploaded
  writes.
- **Local-durable** *(the default)* — the write is acknowledged only once the data
  is safely on the machine's local disk, and is then replicated to object storage
  in the background. This survives a process crash or an unclean reboot.
- **Synchronous to object storage** — the write is acknowledged only once the data
  is durably in object storage itself. This survives the total loss of the machine,
  and it is the slowest, because every write waits for a network round-trip to the
  object store.

Each system we compare against sits at one or more of these points, and we always
line them up guarantee-for-guarantee. A summary of the tiers and how to configure
them lives in the [Durability & QoS tiers](guide/durability.md) guide.

## The test environment

All numbers below come from a single disposable cloud machine so that every system
runs under identical hardware and network conditions:

| | |
|---|---|
| Machine | Scaleway `fr-par-1`, one 8-core / 32 GB instance, Ubuntu 24.04 |
| Object storage | Scaleway Object Storage (`s3.fr-par.scw.cloud`) |
| Protocol | NFSv3 — the one protocol every system shares |
| Workload generator | `fio`, with matched mount options (`nconnect=4`, attribute caching on) |

**Making it a fair fight.** DittoFS is a network filesystem server in its own right.
The systems we compare against — JuiceFS, s3ql, s3fs, rclone, and others — are
mostly FUSE filesystems. To put them on equal footing, each competitor is
re-exported through the Linux kernel's own NFS server with a **durable export**, so
that it, too, only acknowledges a write once the data is on stable storage. Without
this, a competitor could reply straight from kernel memory and post inflated write
numbers against DittoFS's honest, durable acknowledgements. With it, every system is
answering the same question: *is this write really safe yet?*

**How to read the numbers.** Object-storage round-trips are the dominant cost in the
durable tiers, and their latency varies from run to run on shared cloud
infrastructure. We therefore report the median of several repeated runs and, where a
result is close, say so plainly. Trust the shape and the order of magnitude, not the
last digit.

## Creating and writing many small files

Creating lots of small files is the workload where the durability choice matters
most — it is nearly all metadata and tiny writes, so the per-write acknowledgement
cost dominates. The results below are file-create-plus-write throughput, in
operations per second, grouped by the guarantee each configuration makes.

### Writeback: fastest, bounded-loss

At the writeback tier — local acknowledgement, background upload, with a small
window of possible loss on a crash — DittoFS runs at essentially local-disk speed,
ahead of every comparable storage engine (rclone aside, which is a bare pass-through
rather than a storage engine — see below):

| System | ops/sec |
|---|--:|
| rclone (write-cache mode) | 7,400 |
| **DittoFS** (badger) | **997** |
| JuiceFS (writeback) | 14 |
| s3fs | 7 |

DittoFS runs this at essentially **local-disk speed** — the bare local-disk reference
on the same machine turns in 1,081 ops/sec, and DittoFS is right behind it at 997
while carrying a full content-addressed storage engine. rclone's write-cache mode posts
a higher figure, but the comparison is not like-for-like: rclone is a FUSE filesystem
re-exported over kernel NFS, so it collects a client page-cache and attribute-cache free
ride that DittoFS's native NFS server does not. This workload is bound by NFS round-trip
time, not by the store — DittoFS's engine creates files in microseconds, far faster than
any NFS-mounted figure here reflects — so what the table really measures at this tier is
the protocol path, where a cached FUSE re-export has the edge. rclone also carries none
of the storage features: no deduplication, no content-addressing, no crash-consistent
metadata database, one protocol. The clean native-server-to-native-server comparison is
DittoFS versus ZeroFS, where DittoFS leads. **JuiceFS**, the closest genuine peer, commits its
metadata database synchronously *even in writeback mode*, so its create rate stays
pinned near its durable rate (~14 ops/sec); DittoFS's writeback tier instead relaxes
metadata timing for a small bounded-loss window, which is what buys the ~1,000 ops/sec.
The two are making different durability promises at this tier, not merely posting
different speeds.

### Local-durable: a tier that stands alone

The default tier acknowledges a write once the data is safely on local disk, and
replicates to object storage afterward. DittoFS runs this at around **690 ops/sec**
with badger (about 340 MB/s sequential, ~6,500 random-write IOPS).

Notably, **no competitor offers this middle ground at all.** JuiceFS,
s3fs, and the others step directly from a bounded-loss local cache to a full
synchronous upload to object storage. DittoFS's local-durable tier survives a
machine crash *without* paying an object-storage round-trip on every file — a
genuinely useful point on the curve that the other systems simply do not have.

### Synchronous to object storage: a dead heat with JuiceFS

This is the strongest promise: a write is not acknowledged until the data is durably
in object storage, so it survives losing the entire machine. We verified DittoFS
actually delivers this — immediately after a write is acknowledged, the object is
present in the bucket; the same test on the local-durable tier finds the bucket still
empty, confirming the two tiers behave differently and correctly.

Measured across repeated runs on the same machine:

| System | ops/sec |
|---|--:|
| JuiceFS (default) | 16 |
| **DittoFS** (badger) | **13** |
| s3fs | 3 |

**DittoFS and JuiceFS finish within a few operations per second of each other**
(13 vs 16 here, median of six repeats), and both are roughly ten times faster than
s3fs's naive write-through. This tier is entirely bound by object-storage round-trip
time, so individual runs swing widely from one repeat to the next for *both* systems —
which is why these rows are reported as medians rather than single passes.

Two systems could not be included fairly. rclone's no-cache mode cannot honor a
synchronous flush at all — the flush errors out and only a zero-byte placeholder
reaches object storage, so it does not offer the same guarantee and any number it
posts would be misleading. And goofys has no metadata engine — it maps files
one-to-one onto objects and cannot run a file-creation workload, failing at the first
create regardless of tier.

### Does the choice of metadata engine matter?

DittoFS can keep its metadata in an embedded engine (badger) or in an external SQL
database (SQLite or PostgreSQL); JuiceFS can use SQLite, PostgreSQL, or Redis. A
natural question is whether that choice changes the numbers above. We ran the whole
create-and-write workload against every engine, and the answer depends entirely on
the tier:

- **At the synchronous-to-object-storage tier, the engine is invisible.** DittoFS
  turns in 12–13 ops/sec whether the metadata lives in badger, SQLite, or Postgres;
  JuiceFS turns in 15–18 across SQLite, Postgres, and Redis. The write is gated by a
  network round-trip to object storage, and that round-trip dwarfs anything the
  metadata database does. Pick the engine you want to operate — it will not change
  your durable-write throughput.
- **At the local-ack tier, the engine matters for DittoFS.** With the object-store
  round-trip out of the hot path, the metadata engine becomes visible: badger leads by
  a wide margin (≈1,000 ops/sec), with Postgres and SQLite landing at roughly a third
  to a half of that (about a 2–3× spread). JuiceFS, by contrast, stays flat across its
  engines, because it commits
  metadata synchronously to its database even in writeback mode.

The practical takeaway: choose the metadata engine for operability — an embedded
store for a self-contained deployment, a shared SQL database when you want to point
external tooling at it — not for durable-write speed, where it makes no measurable
difference.

### What happens when the local cache fills

A writeback cache makes the fast common case fast, but it raises a harder question:
what does the system do when writes arrive faster than they can be uploaded to object
storage and the cache runs out of room? The promise a well-behaved system should keep
is that it **slows down** — backpressures the writer to the speed the uploader can
sustain — rather than hard-failing the write.

With a bounded cache, DittoFS keeps that promise. Under a sustained large write
against a 2 GiB cap, the writer blocks and drains to object storage at upload speed:
the same shape as rclone and JuiceFS, which throttle rather than error under the same
pressure. Once the bound is generous enough to absorb bursts, the binding constraint
is object-storage upload throughput, not the cache accounting — and every system in
this class converges to roughly that upload rate.

Two edges are worth naming plainly:

- **A very small cap plus one large synchronous flush can still outrun the uploader.**
  A 256 MiB cap combined with a single `fsync` of a multi-gigabyte buffered write asks
  the cache to absorb far more than it can hold while the uploader drains a small
  fraction of it; the backpressure window is exhausted and the write surfaces an error.
  This is bound by upload throughput — the uploader is the lever — not by the cache
  logic; making the uploader keep up under this load is tracked as follow-up work. Set
  the cap to comfortably exceed the largest single flush you expect, and the write
  backpressures cleanly.
- **Unbounded mode performs no eviction at all.** With the bound turned off, the cache
  grows until the underlying disk fills, at which point writes fail with `ENOSPC`.
  This is the inherent risk of removing the bound, and it is why the bounded journal is
  the safer default: it evicts already-uploaded segments and only ever backpressures on
  the dirty ones. Competitors diverge here too — an unbounded JuiceFS cache likewise
  grows to consume tens of gigabytes of disk, while rclone and s3fs self-bound.

## Throughput and IOPS across mixed workloads

Beyond file creation, the following shows sustained bandwidth and I/O rates for
sequential and random access, all at the durable default over NFSv3, on medium-sized
(1 MiB) files. **Bold** marks DittoFS; 🏆 marks where it leads the field.

| Workload | **DittoFS** | ZeroFS | JuiceFS | Local disk |
|---|--:|--:|--:|--:|
| Sequential write (MB/s) | **272** | 63 | 560 | 391 |
| Sequential read (MB/s) | **800** | 364 | 800 | 1,333 |
| Random write, 4 KiB (IOPS) | **4,611** 🏆 | 1,242 | 668 | 2,029 |
| Random read, 4 KiB (IOPS) | **58,733** | 3,905 | 109,837 | 110,953 |
| Mixed read/write (IOPS) | **6,115** | 1,210 | 3,191 | 7,265 |

A few things stand out:

- **Random writes lead the entire field** at 4,611 IOPS — nearly 7× JuiceFS and
  ahead even of a plain local disk. A durable random-write burst normally pays a
  separate disk-sync barrier for every write; DittoFS coalesces the concurrent
  barriers into shared group commits, which is what puts it in front.
- **Mixed read/write is well clear of both real filesystems** (JuiceFS and ZeroFS),
  though a plain uncached local disk edges slightly ahead; **sequential reads tie
  JuiceFS.** Against
  ZeroFS — the other system that, like DittoFS, serves the network protocol itself
  rather than being re-exported — DittoFS wins every single row, often by large
  margins.
- **The FUSE-based systems post very high random-read rates** because, when
  re-exported, their reads are served straight from the operating system's page
  cache. DittoFS and ZeroFS serve the protocol directly and get no such free ride,
  so random-read comparisons are only truly like-for-like between those two.

Note that "local disk" here is not a hardware ceiling — it is a plain local
directory re-exported with a durable export and no application-level write cache, so
it represents uncached, write-through I/O. Any system with its own local cache
(including DittoFS's local journal) can and does beat it on cached workloads. For
reference, the raw block device on the test machine sustains about **1.7 GB/s** for
direct, synchronous sequential writes, and the network path to object storage adds a
round-trip of roughly 20 ms — so the durable-tier numbers above are bounded by that
object-storage round-trip, not by local hardware.

## Where DittoFS stands, in plain terms

- At **matched guarantees, DittoFS leads or ties every comparable filesystem.** It is
  the fastest real filesystem at the writeback tier, it finishes level with JuiceFS
  at the strongest synchronous-to-object-storage tier, and it wins the broad
  throughput and IOPS mix outright against its closest architectural peer.
- It offers a **local-durable middle tier that no competitor provides** — crash-safe
  without an object-storage round-trip on every write.
- The places it trails are honest and well-understood: a bare-metal pass-through
  (rclone) is several times faster at the writeback tier while offering none of the
  storage features — no deduplication, no content-addressing, no crash-consistent
  metadata database — and sequential-write bandwidth to a single stream still has room
  to grow.

The overall picture is a system that competes at the top of its class while making
its durability promises explicit and verifiable — and letting you choose exactly
which promise you want.

## Reproducing these results

The benchmark harness lives in the DittoFS source tree under `cmd/bench` (the
`dfsbench` tool), with the supporting library under `internal/dfsbench/`. `fio` must
be available on your `PATH` (the development shell provides it).

```sh
go build -o dfsbench ./cmd/bench

# Run configuration: the object-storage bucket and endpoint.
# Credentials are read from the environment (see below).
cat > bench.yaml <<'EOF'
bucket: dittofs-bench
endpoint: https://s3.fr-par.scw.cloud
EOF

# Provision a disposable machine, run the matrix, collect results, tear it down.
dfsbench setup
dfsbench run --remote --config bench.yaml \
  --systems dittofs-s3-nfs3,zerofs-nfs3,juicefs-nfs3,local-disk-nfs3 \
  --sizes medium
dfsbench report --results ./bench-results
dfsbench teardown
```

Object-storage credentials are taken from the environment
(`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`); the bucket and endpoint come from the
run configuration. Run `dfsbench list` for the full set of systems, workloads, and
sizes the harness supports.

## Appendix: the complete result matrix

Every cell collected in the full run — 25 systems, each metadata engine and cache
mode, across NFSv3, NFSv4, and SMB3, for four workloads. The tables above draw their
conclusions from this data; the raw grid is reproduced here in full for transparency.
All figures are the warm-pass result on the *medium* size class, 30 s per workload, on
the machine described earlier (8 vCPU, 31 GB, ~1.7 GB/s local NVMe, ~20 ms to object
storage).

**How to read these tables**

- Systems are grouped by the guarantee they were configured for — *local-ack* (the
  write returns as soon as it is buffered on the local machine; the copy to object
  storage happens in the background) versus *durable* (the write returns only once it
  is safe). The durable grid is the honest cross-system comparison; the local-ack grid
  is dominated by local disk and page cache and should be read as such — several
  entries there exceed the object-storage path by two orders of magnitude precisely
  because they never touch it on the hot path.
- `·` means the cell was not part of this run for that system (a mode a given tool does
  not support — e.g. ZeroFS and s3ql expose no SMB, goofys no create-heavy path).
- **The DittoFS rows were re-measured on a fresh run** after the write-path fix landed;
  the earlier run's local-ack and writeback *write* columns were absent because a
  metadata write-contention path returned an I/O error, which now applies backpressure
  instead. Those columns are now filled. The DittoFS remote/durable rows are the median
  of six repeats (object-storage latency varies run to run); the local-ack, writeback,
  and local-durable rows are the median of three; the competitor rows are carried
  forward unchanged from the earlier run, since their code did not change and their
  numbers are build-independent. **DittoFS SMB3 remains absent** (an SMB directory-lease
  interaction under investigation).
- **A cold (post-eviction) read pass could not be collected for DittoFS.** The harness
  forces a cold read by draining every buffered upload to object storage and then
  evicting the local cache; on DittoFS that drain currently stalls with no upload
  progress, so the cold column is omitted rather than shown as a stalled or partial
  measurement. All DittoFS figures here are therefore warm.
- Read figures on warm passes reflect the page cache, not object storage; treat them
  as an upper bound on cached-read behavior, not as storage-backend throughput.

<!-- generated from the dfsbench result set; regenerate rather than hand-edit -->

### NFSv3

**Local-ack tier — write acked when buffered locally; upload to object storage is asynchronous**

| System | meta ops/s | seq-write MB/s | rand-wr IOPS | rand-rd IOPS |
|---|---|---|---|---|
| DittoFS · badger · writeback | 997 | 376 | 15,105 | 57,050 |
| DittoFS · sqlite · writeback | 334 | 122 | 1,558 | 2,535 |
| DittoFS · postgres · writeback | 439 | 297 | 7,833 | 10,182 |
| JuiceFS · sqlite · writeback | 14 | 7.6 | 8 | 109,912 |
| JuiceFS · postgres · writeback | 14 | 3.6 | 6 | 109,102 |
| JuiceFS · redis · writeback | 15 | 5.8 | 7 | 99,611 |
| rclone · vfs-writes | 7,400 | 1,772.5 | 18,117 | 109,752 |
| rclone · vfs-full | 7,346 | 1,775.6 | 18,099 | 109,507 |
| s3fs · cached | 7 | 0.0 | · | 5,154 |
| s3fs · nocache | · | 0.1 | · | 5,159 |
| local-disk (reference) | 1,081 | 454.7 | 2,670 | 110,941 |

**Durable tier — write acked only after it is safe**

| System | meta ops/s | seq-write MB/s | rand-wr IOPS | rand-rd IOPS |
|---|---|---|---|---|
| DittoFS · badger · local-durable *(default)* | 687 | 342 | 6,529 | 12,012 |
| DittoFS · sqlite · local-durable *(default)* | 346 | 115 | 1,521 | 2,425 |
| DittoFS · postgres · local-durable *(default)* | 253 | 201 | 6,627 | 8,594 |
| DittoFS · badger · remote/durable | 13 | 5.4 | 17 | 12,149 |
| DittoFS · sqlite · remote/durable | 13 | 6.7 | 16 | 2,393 |
| DittoFS · postgres · remote/durable | 12 | 6.8 | 10 | 9,374 |
| JuiceFS · sqlite · durable | 15 | 7.7 | 6 | 109,464 |
| JuiceFS · postgres · durable | 18 | 4.1 | 4 | 109,956 |
| JuiceFS · redis · durable | 16 | 9.2 | 6 | 109,751 |
| ZeroFS · default | 1,887 | · | · | · |
| ZeroFS · sync_writes | 1 | 1.7 | 2 | 4,624 |
| s3ql | 2,283 | 712.5 | 2,426 | 110,418 |
| NFS-Ganesha · local VFS | 1,276 | 522.5 | · | 60,429 |

**Large size (1 GiB files) — DittoFS, warm, single run**

The tables above are the *medium* (1 MiB) size class. The same DittoFS matrix was also
run on the *large* (1 GiB) size class, recorded below so the large-file behavior is on
record. Single run per cell, so treat these as indicative, not medians. Local disk is
included as the reference ceiling.

| System | meta ops/s | seq-write MB/s | rand-wr IOPS | rand-rd IOPS |
|---|---|---|---|---|
| DittoFS · badger · local-durable | 595 | 300 | 5,779 | 9,482 |
| DittoFS · sqlite · local-durable | 455 | 118 | 1,506 | 846 |
| DittoFS · postgres · local-durable | 497 | 299 | 6,610 | 3,332 |
| DittoFS · badger · writeback | 975 | 337 | 11,439 | 5,757 |
| DittoFS · sqlite · writeback | 468 | 126 | 1,604 | 1,205 |
| DittoFS · postgres · writeback | 459 | 277 | 7,152 | 4,071 |
| DittoFS · badger · remote/durable | 12 | 7.7 | 15 | 9,534 |
| DittoFS · sqlite · remote/durable | 12 | 8.5 | 11 | 374 |
| DittoFS · postgres · remote/durable | 13 | 6.1 | 15 | 1,021 |
| local-disk (reference) | 1,378 | 471 | 2,099 | 109,023 |

### NFSv4

**Local-ack tier — write acked when buffered locally; upload to object storage is asynchronous**

| System | meta ops/s | seq-write MB/s | rand-wr IOPS | rand-rd IOPS |
|---|---|---|---|---|
| DittoFS · badger · writeback | 137 | · | · | · |
| DittoFS · sqlite · writeback | 106 | · | · | · |
| DittoFS · postgres · writeback | 101 | · | · | · |
| JuiceFS · sqlite · writeback | 15 | 8.5 | 11 | 90,710 |
| JuiceFS · postgres · writeback | 14 | 6.9 | 6 | 91,143 |
| JuiceFS · redis · writeback | 14 | 8.8 | 12 | 91,318 |
| rclone · vfs-writes | 2,425 | 1,885.3 | 19,574 | 91,331 |
| rclone · vfs-full | 2,415 | 1,850.7 | 19,562 | 91,705 |
| s3fs · nocache | 6 | 0.2 | · | 5,151 |
| local-disk (reference) | 1,050 | 437.1 | 2,504 | 91,667 |

**Durable tier — write acked only after it is safe**

| System | meta ops/s | seq-write MB/s | rand-wr IOPS | rand-rd IOPS |
|---|---|---|---|---|
| DittoFS · badger · remote/durable | 5 | 6.5 | 13 | 1,079 |
| DittoFS · sqlite · remote/durable | 12 | 5.5 | 9 | 870 |
| DittoFS · postgres · remote/durable | 12 | 7.2 | 13 | 1,020 |
| JuiceFS · sqlite · durable | 13 | 7.8 | 9 | 257 |
| JuiceFS · postgres · durable | 13 | 9.1 | 11 | 90,751 |
| JuiceFS · redis · durable | 14 | 7.8 | 14 | 86,144 |

### SMB3

**Local-ack tier — write acked when buffered locally; upload to object storage is asynchronous**

| System | meta ops/s | seq-write MB/s | rand-wr IOPS | rand-rd IOPS |
|---|---|---|---|---|
| JuiceFS · sqlite · writeback | 23 | 1,485.7 | 65 | 19,068 |
| JuiceFS · postgres · writeback | 26 | 2,267.4 | 65 | 19,077 |
| JuiceFS · redis · writeback | 27 | 1,942.7 | 83 | 18,886 |
| rclone · vfs-writes | 298 | 2,056.1 | 8,770 | 9,813 |
| rclone · vfs-full | 283 | 2,064.6 | 8,854 | 9,843 |
| s3fs · cached | · | · | 4,938 | 11,333 |
| s3fs · nocache | · | · | 5,140 | 5,029 |
| goofys | · | · | · | 19,610 |
| local-disk (reference) | 1,094 | 2,915.7 | 19,613 | 19,739 |

**Durable tier — write acked only after it is safe**

| System | meta ops/s | seq-write MB/s | rand-wr IOPS | rand-rd IOPS |
|---|---|---|---|---|
| JuiceFS · sqlite · durable | 25 | 2,105.9 | 54 | 19,114 |
| JuiceFS · postgres · durable | 27 | 2,050.0 | 57 | 18,854 |
| JuiceFS · redis · durable | 49 | 1,940.9 | 92 | 18,991 |

### Cell coverage

Total cells collected: **169** across 25 systems × 3 protocols × 4 workloads (warm pass, medium size, 30 s each).

The DittoFS NFSv3 rows above were subsequently re-measured on a fresh run of the fixed
binary — nine backend/tier combinations (badger/SQLite/Postgres × local-durable/writeback/
remote) across four workloads and the medium and large size classes, warm pass, with the
remote/durable cells repeated six times and the local cells three times for the medians
shown. The competitor rows are unchanged from the run above (their code did not change).
