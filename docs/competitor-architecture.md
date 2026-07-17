# Competitor architecture — how other filesystems present object storage as POSIX

How DittoFS compares to the object-storage filesystems it is benchmarked against. This doc
grounds the *positioning* ("no FUSE, multi-protocol, pluggable metadata") and is the source of
truth for **how to configure each competitor's durability tier fairly** in the dfsbench matrix
(see `internal/dfsbench/`, tracked in #1739). Every claim is sourced; items that could not be
verified from primary docs are flagged ⚠.

## The problem, and the three ways to solve it

All of these systems expose S3-style object storage as a (more or less) POSIX filesystem. They
differ first in **how the client sees the filesystem**:

| Approach | How | Systems | Trade |
|---|---|---|---|
| **FUSE mount** | kernel FUSE module → userspace daemon | goofys, s3fs, rclone mount, **JuiceFS** (default) | simplest; FUSE context-switch tax, single host, POSIX fidelity varies |
| **LD_PRELOAD** | intercept libc syscalls in userspace, no kernel module | **CunoFS** (primary mode) | near-native for processes *launched under it*; not a general mount |
| **Userspace network-FS server** | implement NFS/SMB/etc.; the kernel's *own* client mounts it | **DittoFS**, **ZeroFS** | no FUSE module, works over the network, multi-client |

**DittoFS is a userspace NFSv3/v4 + SMB2/3 server** — the kernel's stock NFS/SMB client mounts it,
no FUSE and no preload. Only **ZeroFS** shares that approach; only DittoFS pairs it with **SMB**
and a **pluggable embedded metadata store**.

## The matrix

| Axis | **DittoFS** | ZeroFS | JuiceFS | CunoFS | goofys | s3fs | rclone mount |
|---|---|---|---|---|---|---|---|
| **Client** | NFS+SMB server | NFS+9P+NBD server | FUSE (+gateways) | LD_PRELOAD (+FUSE) | FUSE | FUSE | FUSE |
| **Kernel FUSE?** | **No** | No (FUSE optional) | Yes | Optional | Yes | Yes | Yes |
| **Metadata** | pluggable embedded (badger/sqlite/pg) | embedded LSM (SlateDB) on S3 | external DB (Redis/TiKV/SQL) | single-object + POSIX metadata (⚠ encoding undocumented) | derived from S3 keys | S3 keys + `x-amz-meta-*` | remote listing + VFS cache |
| **Durability on fsync/close** | durable-local-cache + async S3 | NFS: early-ack · 9P/NBD: strong | default: **strong-to-S3** · `--writeback`: local+async | client-side writeback (⚠ trigger undocumented) | fsync **ignored**, flush-on-close | **synchronous** durable-on-close/fsync | async writeback (5s default) |
| **Random writes** | ✓ | ✓ (RMW extents) | ✓ (slices) | partial/cached (Fusion for heavy) | ✗ sequential only | whole-object rewrite | needs cache-mode writes/full |
| **Atomic rename** | ✓ | ⚠ unverified | ✓ | ✓ | limited (>1000 children fails) | ✗ copy+delete | backend-dependent |
| **Hardlinks** | store-dependent | ✓ | ✓ | ✓ | ✗ | ✗ | ✗ |
| **Second protocol / topology** | **SMB**; multi-client server | 9P, NBD; single-host +HA | S3 gateway, WebDAV, CSI; multi-host (shared DB) | none (per-host client) | none; single host | none; single host | `serve` modes; single-host mount |
| **License** | OSS | AGPL-3.0 | Apache-2.0 (+ managed) | commercial (trial) | Apache-2.0 (unmaintained) | GPL-2.0 | MIT |

## Is any of them identical to DittoFS? — No. ZeroFS is the near-twin.

- **ZeroFS** is architecturally closest: a userspace multi-protocol server the kernel mounts,
  embedded LSM metadata (SlateDB, which itself persists to S3), a local cache, and async segments
  to S3. Divergences: it speaks **9P + NBD** where DittoFS speaks **SMB**; its metadata lives *on
  S3* (coupling metadata latency to object-store flushes) where DittoFS keeps metadata in a local
  embedded store; and its durability is **protocol-dependent** (see below).
- **JuiceFS** is the opposite design on two axes: **FUSE**, and an **external, separately-operated
  metadata DB** (Redis/TiKV/SQL) rather than embedded.
- **CunoFS** avoids FUSE like DittoFS but by a completely different mechanism — **syscall
  interception (LD_PRELOAD)**, an app-level interposition layer, not a filesystem server.
- **goofys / s3fs / rclone** are "S3-as-FUSE" with no separate metadata engine (namespace derived
  from object keys) — cheap but weak POSIX.

**The differentiators that hold:** DittoFS is the only **no-FUSE, multi-protocol (NFS + SMB),
pluggable-embedded-metadata** option. The "fuse-less" story is real — only ZeroFS shares it.

## Durability — the benchmark-fairness axis

A fair throughput comparison **must hold all systems to the same durability tier**, or a system
that acks from RAM will "win" for the wrong reason. Effective tiers:

| Tier | Systems (as configured for a fair, durable comparison) |
|---|---|
| **Durable-local-cache + async S3** (DittoFS's model) | **DittoFS** (default) · **JuiceFS `--writeback`** · rclone `--vfs-cache-mode writes` (⚠ `fsync` not a documented barrier) |
| **Strong (durable to S3 on fsync/close)** | **s3fs** (synchronous on close/fsync) · **JuiceFS default** · ZeroFS **9P/NBD** |
| **Early-ack / weak** (do NOT compare as durable) | ZeroFS over **NFS** (NFS COMMIT allows early return) · **goofys** (fsync ignored, RAM-buffered until close) · **rclone** default (5s async writeback) |

Practical harness rules (feeds #1739):
- **JuiceFS** must run `--writeback` to match DittoFS's tier — its *default* is strong-to-S3, which
  is a *harder* tier and would flatter DittoFS if compared against DittoFS's async default.
- **ZeroFS over NFS** early-acks like a writeback cache — a reasonable NFS-vs-NFS peer to DittoFS;
  do **not** compare DittoFS-NFS against ZeroFS-**9P/NBD** (strong) — that mismatches the contract.
- **goofys** cannot honor an fsync barrier at all → it belongs in a **read/throughput bucket**,
  labeled non-durable (#1749). Its no-random-write / no-rename limits also disqualify many workloads.
- **rclone** default is async (5s); pin `--vfs-cache-mode writes --vfs-write-back 0` to approach
  durable, and note `fsync` is not a documented durability barrier.
- **CunoFS** durability trigger is not publicly documented; run its **FUSE mount** mode (≈half its
  LD_PRELOAD speed, per its own docs) or its Fusion tier, and pin it down before treating any CunoFS
  number as durable-equivalent (#1748).

## Per-system notes

**ZeroFS** (AGPL-3.0) — NFS+9P+NBD from one userspace process; metadata in SlateDB (LSM) persisted
to S3 + a required local cache; immutable compressed encrypted segments to S3; encryption mandatory.
Optional leader/standby HA. ⚠ atomic rename / symlinks / xattrs not confirmed in primary docs.

**JuiceFS** (Apache-2.0) — FUSE mount + S3 gateway / WebDAV / CSI / Hadoop / Python SDK. External
pluggable metadata engine holds the full namespace + `file→chunk→slice→block` map; S3 sees only
opaque numbered blocks (you cannot reconstruct a file from the bucket alone). Atomic rename, xattrs,
flock/fcntl locks, mmap; passes pjdfstest. Requires operating the metadata DB.

**CunoFS** (commercial; 14-day Professional trial → Personal; `CUNO_INSTALL_LICENSE`) — LD_PRELOAD
primary, FUSE fallback (~half speed, four context switches/op), FlexMount hybrid. Stores each file
as a single unmodified object (no lock-in); rich POSIX incl. symlink/hardlink; write-caching with
Fusion for heavy random writes. ⚠ metadata storage location and exact durability trigger undocumented.

**goofys** (Apache-2.0, effectively unmaintained since 2020; fork: blampe/goofys) — FUSE, namespace
derived from S3 keys, ownership faked (mode/owner/group not stored). fsync ignored; flush-on-close;
no on-disk cache. No random writes, no symlink/hardlink, rename limited. "Performance first, POSIX
second."

**s3fs** (GPL-2.0) — FUSE, metadata in `x-amz-meta-*` headers, directories as `path/` objects.
**Synchronous** durable-on-close and fsync (blocks on the S3 PUT/multipart). Random writes rewrite
the whole object; rename = server-side copy (non-atomic); no hardlinks; symlinks + xattrs supported.

**rclone mount** (MIT) — FUSE (WinFsp/macFUSE/FUSE-T) with a VFS + on-disk cache. Four cache modes,
default `off`; caching modes upload on close after a `--vfs-write-back` window (default 5s) = async
writeback, not durable-on-close. ⚠ `fsync`→upload not documented.

## Sources

- ZeroFS — [architecture](https://www.zerofs.net/docs/architecture) · [GitHub](https://github.com/Barre/ZeroFS)
- JuiceFS — [architecture](https://github.com/juicedata/juicefs/blob/main/docs/en/introduction/architecture.md) · [metadata/data design](https://juicefs.com/en/blog/engineering/design-metadata-data-storage) · [cache/writeback](https://juicefs.com/docs/community/guide/cache/) · [IO processing](https://juicefs.com/docs/community/internals/io_processing/)
- CunoFS — [overview (modes)](https://cuno-cunofs.readthedocs-hosted.com/en/stable/user-guide-overview.html) · [core concepts](https://cuno-cunofs.readthedocs-hosted.com/en/stable/user-guide-core-concepts.html) · [technology](https://cuno.io/technology-detail/)
- goofys — [GitHub](https://github.com/kahing/goofys) · [maintained fork](https://github.com/blampe/goofys)
- s3fs — [FAQ](https://github.com/s3fs-fuse/s3fs-fuse/wiki/FAQ) · [README](https://github.com/s3fs-fuse/s3fs-fuse)
- rclone — [mount / VFS caching](https://rclone.org/commands/rclone_mount/)

_Competitor behavior evolves; re-verify before quoting externally. Last grounded: 2026-07-17._
