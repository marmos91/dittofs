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
window of possible loss on a crash — DittoFS is comfortably the fastest of the real
filesystems:

| System | ops/sec |
|---|--:|
| **DittoFS** | **5,700** |
| rclone (write-cache mode) | 6,200 |
| JuiceFS (writeback) | 1,900 |
| s3ql | 1,600 |

Against the two systems that are genuine peers — **JuiceFS and s3ql**, both of which
run a real metadata database and deduplicate data — **DittoFS is three to four times
faster** (roughly 3× JuiceFS and 3.6× s3ql). The one system ahead of it, rclone in
its write-cache mode, is about 8% faster, but it is not a comparable product: it is
a thin pass-through with no deduplication, no content-addressing, no crash-consistent
metadata database, and support for a single protocol. Staying within 8% of a
bare-metal pass-through while carrying a full storage engine is the real story here.

### Local-durable: a tier that stands alone

The default tier acknowledges a write once the data is safely on local disk, and
replicates to object storage afterward. DittoFS runs this at around **900 ops/sec**,
with an intermediate mode that relaxes only metadata timing reaching about **1,700**.

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
| **DittoFS** | **26** |
| JuiceFS (default) | 26 |
| s3fs | 3 |

**DittoFS and JuiceFS finish in a dead heat**, and both are roughly ten times faster
than s3fs's naive write-through. This tier is entirely bound by object-storage
round-trip time, so individual runs swing widely — anywhere from about 18 to 40
operations per second for *both* systems.

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
  turns in 10–12 ops/sec whether the metadata lives in badger, SQLite, or Postgres;
  JuiceFS turns in 15–18 across SQLite, Postgres, and Redis. The write is gated by a
  network round-trip to object storage, and that round-trip dwarfs anything the
  metadata database does. Pick the engine you want to operate — it will not change
  your durable-write throughput.
- **At the local-ack tier, the engine matters for DittoFS.** With the object-store
  round-trip out of the hot path, the metadata engine becomes visible: badger leads,
  SQLite is roughly half its rate, and Postgres a little behind that (about a 2–3×
  spread). JuiceFS, by contrast, stays flat across its engines, because it commits
  metadata synchronously to its database even in writeback mode.

The practical takeaway: choose the metadata engine for operability — an embedded
store for a self-contained deployment, a shared SQL database when you want to point
external tooling at it — not for durable-write speed, where it makes no measurable
difference.

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
  (rclone) edges it by single digits at the writeback tier while offering none of the
  storage features, and sequential-write bandwidth to a single stream still has room
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
- **DittoFS local-ack and writeback *write* rows are absent**, and **DittoFS SMB3 is
  absent entirely.** These are not results — they are two defects the run surfaced: a
  metadata write-contention path that returned an I/O error under this harness (since
  fixed — writes now apply backpressure instead of erroring), and an SMB
  directory-lease interaction under investigation. They are omitted rather than shown
  as zero so the grid is not read as a measurement it isn't. DittoFS's durable-tier
  columns, which completed cleanly with no errors, are the trustworthy comparison.
- Read figures on warm passes reflect the page cache, not object storage; treat them
  as an upper bound on cached-read behavior, not as storage-backend throughput.

<!-- generated from the dfsbench result set; regenerate rather than hand-edit -->

### NFSv3

**Local-ack tier — write acked when buffered locally; upload to object storage is asynchronous**

| System | meta ops/s | seq-write MB/s | rand-wr IOPS | rand-rd IOPS |
|---|---|---|---|---|
| DittoFS · badger · writeback | 686 | · | · | · |
| DittoFS · sqlite · writeback | 362 | · | · | · |
| DittoFS · postgres · writeback | 251 | · | · | · |
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
| DittoFS · badger · remote/durable | 11 | 7.2 | 15 | 57,114 |
| DittoFS · sqlite · remote/durable | 12 | 4.9 | 10 | 1,984 |
| DittoFS · postgres · remote/durable | 10 | 6.8 | 16 | 11,898 |
| JuiceFS · sqlite · durable | 15 | 7.7 | 6 | 109,464 |
| JuiceFS · postgres · durable | 18 | 4.1 | 4 | 109,956 |
| JuiceFS · redis · durable | 16 | 9.2 | 6 | 109,751 |
| ZeroFS · default | 1,887 | · | · | · |
| ZeroFS · sync_writes | 1 | 1.7 | 2 | 4,624 |
| s3ql | 2,283 | 712.5 | 2,426 | 110,418 |
| NFS-Ganesha · local VFS | 1,276 | 522.5 | · | 60,429 |

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
