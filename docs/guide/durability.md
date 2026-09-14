# Durability

DittoFS lets you trade write throughput against how much recent work a crash may
lose. This page explains the durability knobs that are **available today** and how
they map to measured throughput.

> **TL;DR** — By default a write is acknowledged once it is `fsync`'d to the
> share's **journal** *and* its metadata store, then replicated to the block store
> in the background. That survives a process or node **crash**, but not the loss of
> the device holding the journal.

## Two independent axes, not one tier

Every write travels: client → the share's on-disk journal (data) + metadata store
(inode, size, dirent) → asynchronous upload to the block store (S3). Two separate
settings decide how much of that a write waits for, and they are **independent** —
any combination of the two is legal, and neither implies the other:

| Axis | Per-share setting | Default | What it controls |
|---|---|---|---|
| Commit acknowledgement | `commit_ack` | `journal` | how far the **data** must travel before an NFS `COMMIT` / SMB `Flush` returns |
| Relaxed metadata commit | `relaxed_metadata_commit` | `false` | whether the **metadata** `fsync` is paid inline or deferred to a ticker |

Earlier releases folded these into a single three-valued `durability` enum
(`local` / `writeback` / `remote`) in the share's *local* block-store config. That
config no longer exists. On first start after the upgrade the server reads the old
enum (and the older `require_durable_commit` / `writeback` bools it composed) and
carries each half onto the share, so an existing deployment keeps the promise it
had. There is nothing to edit by hand.

### Axis 1 — commit acknowledgement (`commit_ack`)

| `commit_ack` | Ack after | Survives | Does **not** survive |
|---|---|---|---|
| `journal` *(default)* | the write is `fsync`'d to the share's journal | process crash, OOM-kill, host crash, power loss | loss of the device holding the journal |
| `block-store` | the data has reached the share's block store | device loss, whole-node loss | — |

`block-store` makes every `COMMIT`/`CLOSE` wait for an upload, so its throughput is
bounded by the round-trip to the block store rather than by the local disk. Choose
it for a share whose acknowledged writes must outlive the node; leave it at
`journal` otherwise.

`dfsctl share show <name>` prints the setting in force as a **Commit Ack** row,
spelled out ("journal (survives host crash)" / "block-store (survives device
loss)"), and the share JSON carries it as `commit_ack`. This is a promise an
operator cannot infer by watching the system behave, so check it there rather than
guessing from latency.

> **No flag sets this yet.** `commit_ack` is read-only on the CLI and the REST API
> in this release: it is populated by the upgrade carry-over described above, and
> otherwise takes its `journal` default. A share create/edit flag is still to come.

### Axis 2 — relaxed metadata commit (`relaxed_metadata_commit`)

When true, the per-op `FILE_SYNC`/`CLOSE` metadata flush drops from a synchronous
`badger.DB.Sync` to the ~100 ms deferred syncer (`durabilitySyncInterval`). The
*data* still goes through the journal `fsync`, so this relaxes metadata only: a
hard crash can lose the last ~100 ms of size/mtime updates, not the bytes.

**It also turns off per-read warm-read verification** — see
[Read integrity](#read-integrity-per-read-verification--self-heal) before enabling
it. That coupling is deliberate: the fast path is the point of the setting.

Like `commit_ack`, it is carried over from the old `writeback` bool on upgrade and
is not settable from the CLI in this release.

There is no data-corruption risk on either axis: on restart,
`reconcileMetadataSizeFromJournal` repairs each file's metadata size from the
journal's durable high-water mark, so a relaxed metadata commit can only lose the
*most recent* size/mtime update — never leave a file inconsistent.

## Namespace durability (`relaxed_durability`)

The two axes above govern **file data and the metadata paired with
it** (size, mtime, the block manifest). They do not govern **namespace**
operations — `create`, `unlink`, `rename`, `mkdir`, `rmdir`, and attribute-only
`setattr`. Those are controlled separately, by the metadata store's
`relaxed_durability` setting, which is **enabled by default** on the `badger` and
`postgres` stores. It is a per-store config key, set when the store is created or
edited:

```bash
dfsctl store metadata add --name badger-main --type badger \
  --config '{"path":"/var/lib/dittofs/metadata","relaxed_durability":false}'
```

When enabled, a namespace commit does not `fsync` inline. How long it stays at
risk depends on the backend:

| store | mechanism | window |
|---|---|---|
| `badger` | commit skips `fsync`; a background syncer calls `DB.Sync()` on an interval | ~100 ms |
| `postgres` | `SET LOCAL synchronous_commit = off` on the transaction | set by the server's own `wal_writer_delay` (PostgreSQL default 200 ms) |

Setting `relaxed_durability: false` restores a synchronous flush on every
namespace commit, at roughly a third of the create throughput.

### What can actually lose that window

The distinction that matters operationally is **how** the server died. The
numbers below were measured on `badger`; the mechanism (an acknowledged write
already sitting in the kernel page cache) applies to any backend:

| Failure | Namespace ops at risk |
|---|---|
| `SIGKILL`, OOM-kill, `dfs` panic, `systemctl kill` | **none** |
| Power loss, kernel panic, hypervisor reset, disk yank | last ~100 ms |

Killing the process does not lose acknowledged work at any setting. The write-ahead
log is memory-mapped, so an acknowledged namespace op is already in the kernel's
page cache, and the page cache outlives the process — the kernel still flushes it.
Only an event that takes the kernel down with it can lose the un-`fsync`'d tail.

This is bounded loss, never corruption or inconsistency. Data-paired writes stay
synchronous regardless of this setting, and on restart
`reconcileMetadataSizeFromJournal` repairs each file's size from the journal's
durable high-water mark. A lost namespace op leaves a file that was never created,
not a file that is half-created.

Operators who need every acknowledged `create` to survive power loss — and can
accept the throughput cost — should set `relaxed_durability: false`.

## The dirty-age ceiling (`dirty_expire`)

The axes above describe what must be durable **before an ack**. They say nothing
about a client that never asks for durability at all — one that writes and never
issues an NFS `COMMIT`/`FILE_SYNC` or an SMB `FLUSH`/`CLOSE`. Nothing in the
protocol obliges it to, and plenty of writers don't.

For such a writer the journal's durability points used to be: segment rotation
(every 256 MiB, per shard) and shutdown. Neither is age-based, so acknowledged
bytes could sit in the page cache indefinitely — a device-loss test wrote 35 MB
over 20 seconds with no fsync and lost every byte of it, including the ones
acknowledged at the very start of the run. Nothing was corrupted (the
committed-size clamp returns the file short rather than full-size-and-zeroed),
but the loss window had no ceiling in time.

A background loop now commits every shard still holding unfsynced records once
per interval, mirroring Linux writeback's `dirty_expire_centisecs`:

| | Value |
|---|---|
| Default | **30 s**, on for every share |
| Scope | server-wide, `blockstore.journal.dirty_expire` in the config file |
| Cost on the write path | none — the loop runs off the ack path and only touches shards that are actually dirty |

```yaml
blockstore:
  journal:
    dirty_expire: 5s    # tighter ceiling on work you do not want to redo
    # dirty_expire: -1s # turn it off: back to "no promise without fsync", unbounded in time
```

It used to be a per-share `dirty_expire_seconds` key in the local block-store
config; that config is gone and the knob is now one server-level setting shared by
every share's journal.

**This is a ceiling, not a guarantee.** `fsync` remains the only *synchronous*
durability point: a returned NFS `COMMIT` or SMB `FLUSH` says the bytes reached
the device, and nothing else does. The interval only bounds how much a
never-fsyncing writer can lose — it does not make an un-fsynced write durable,
and a crash mid-interval still loses everything written since the last pass.
Applications that care about a specific write must still `fsync` it.

Values below 1 s are clamped with a warning; an interval that short issues disk
barriers faster than a disk retires them.

## Read integrity: per-read verification & self-heal

Warm reads (bytes served from the share's journal without a block-store
round-trip) are verified per-record — there is no separate knob, the
`relaxed_metadata_commit` axis decides it:

| `relaxed_metadata_commit` | Warm-read check | On corruption |
|---|---|---|
| `false` *(default)* | per-record **CRC32** on every warm read | self-heal from the block store |
| `true` | none (raw fast read) | n/a — integrity comes from startup-recovery CRC + cold-fetch BLAKE3 |

With verification on, every warm read re-reads the covering journal record and
checks its stored CRC32 before returning the requested bytes. On a mismatch the
range is **self-healed**: the covering chunk is re-fetched from the block store,
which is BLAKE3-verified on the way in, re-hydrated into the journal, and the read
returns the *correct* bytes. The block store is the source of truth; the journal is
the local tier in front of it.

If the chunk has not yet been offloaded there is no good copy to heal from, so the
read **fails closed** with `EIO` (`NFS3ERR_IO` / `STATUS_DATA_CHECKSUM_ERROR`). It
never returns silently-wrong or zero-filled bytes.

This catches on-disk corruption of a valid segment that happens *after* startup
recovery (bit rot, or a bug mutating cached segment bytes) — the case the
startup-recovery CRC and the remote cold-fetch BLAKE3 don't cover on their own.

**Cost.** Verification reads the *whole covering record* to check its CRC, then
slices out the requested sub-range. For large sequential reads that is free (you
would read the record anyway); for **small random reads** it adds read
amplification (a whole record fetched to return a few KiB) plus the CRC32 CPU.
That is exactly why it is **off when the metadata commit is relaxed** and on
otherwise.

### Why coupled to the relaxed axis (not always-on, not never)

The journal is a **cache on disk over a verified block store**, not the primary
copy — so the right trade mirrors our closest analog rather than a replicated
primary store:

- **[JuiceFS](https://juicefs.com/docs/cloud/guide/cache/)** — an S3-backed
  filesystem with a local disk cache, like us — makes cache-checksum verification
  **opt-in** (`--verify-cache-checksum`) for exactly this reason: "consistency
  depends on the reliability of the disks — if data is tampered with, clients will
  read bad data." We match that posture and, like JuiceFS, heal by re-fetching from
  the object store (the source of truth).
- **Ceph BlueStore** verifies a CRC32c on *every* read and heals from a replica —
  but BlueStore is a **primary** store (the truth-holder), so always-on verify is
  the right default there. We (like JuiceFS) are a cache tier over a verified
  block store, so coupling verification to the durability choice is the better fit
  and preserves the fast read path for shares that asked for it.
- A **background scrubber** (tracked separately as
  [#1490](https://github.com/marmos91/dittofs/issues/1490)) is a *complement*, not
  a substitute — Ceph pairs per-read verify with deep-scrub, and so do we
  (per-read self-heal now, periodic scrub later).

## Measured throughput

File-create + 4 KiB write, 8 threads, NFSv3, badger + S3 block store (median ops/s;
full method and competitor comparison in [BENCHMARKS.md](../BENCHMARKS.md)). The
numbers were measured under the old enum; the setting each row selects is spelled
in the two-axis vocabulary:

| Setting | ops/s |
|---|--:|
| relaxed metadata + async journal commit | ~5700 |
| `relaxed_metadata_commit: true` | ~1680 |
| defaults (`commit_ack: journal`, metadata commit strict) | ~900 |
| `commit_ack: block-store` | ≈ JuiceFS-default (parity) |

For context, at a matched writeback guarantee DittoFS sustains **3.0× JuiceFS
`--writeback`** and **3.6× s3ql**; the middle row — data journal-durable, metadata
relaxed — is one no S3 filesystem competitor offers at all.

## Remaining work (#1758)

Both axes are shipped. One piece remains: the top row above additionally defers the
journal's own data commit, which is still a diagnostic toggle rather than a
supported setting and is gated on the journal switchover stabilizing. Until it
lands, `relaxed_metadata_commit` relaxes metadata only.

## See also

- [BENCHMARKS.md](../BENCHMARKS.md) — per-tier throughput and competitor matrix
- [Configuration](configuration.md) — full block-store and journal config reference
- [Choosing stores](choosing-stores.md) — block-store trade-offs
