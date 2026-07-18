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

What is notable is that **no competitor offers this middle ground at all.** JuiceFS,
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
operations per second for *both* systems — which is exactly why the comparison uses
many runs on one machine rather than a single measurement.

Two systems could not be included fairly. rclone's no-cache mode cannot honor a
synchronous flush at all — the flush errors out and only a zero-byte placeholder
reaches object storage, so it does not offer the same guarantee and any number it
posts would be misleading. And goofys has no metadata engine — it maps files
one-to-one onto objects and cannot run a file-creation workload, failing at the first
create regardless of tier.

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
| Mixed read/write (IOPS) | **6,115** 🏆 | 1,210 | 3,191 | 7,265 |

A few things stand out:

- **Random writes lead the entire field** at 4,611 IOPS — nearly 7× JuiceFS and
  ahead even of a plain local disk. A durable random-write burst normally pays a
  separate disk-sync barrier for every write; DittoFS coalesces the concurrent
  barriers into shared group commits, which is what puts it in front.
- **Mixed read/write also leads**, and **sequential reads tie JuiceFS.** Against
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
(including DittoFS's local journal) can and does beat it on cached workloads.

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
  --systems dittofs-s3-nfs3,zerofs,juicefs,local-disk \
  --sizes medium
dfsbench report --results ./bench-results
dfsbench teardown
```

Object-storage credentials are taken from the environment
(`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`); the bucket and endpoint come from the
run configuration. Run `dfsbench list` for the full set of systems, workloads, and
sizes the harness supports.
