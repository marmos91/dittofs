# Durability & QoS tiers

DittoFS lets you trade write throughput against how much recent work a crash may
lose. This page explains the durability spectrum, the knobs that are **available
today**, and how they map to measured throughput.

> **TL;DR** — The default is **local-durable**: a write is acknowledged only after
> it is `fsync`'d to the local journal *and* metadata store, then replicated to S3
> in the background. That survives a process or node **crash**. You can move
> *weaker+faster* (writeback) or *stronger+slower* (synchronous-to-S3) per share.

## The spectrum

Every write travels: client → local journal (data) + metadata store (inode, size,
dirent) → asynchronous upload to the remote (S3) block store. Two independent
`fsync` points and one remote-ack point define where a config sits:

| Tier | What must be durable before ack | Survives | Relative speed |
|---|---|---|---|
| **Writeback** ‡ | nothing synchronously — local write, deferred `fsync` | bounded-loss only | fastest |
| **Local-durable** *(default)* | local journal + metadata `fsync` | process/node **crash** | middle |
| **Synchronous-to-S3** | data acknowledged **in S3** | total node loss | slowest |

‡ The full Writeback tier (defer *both* the data-journal and metadata `fsync`)
needs the journal async-commit half, which is not yet a supported config — see
[#1758](https://github.com/marmos91/dittofs/issues/1758). The shipped `writeback`
flag below relaxes **metadata only** (data stays journal-`fsync`'d), which lands
*between* local-durable and full writeback.

There is no data-corruption risk in any tier: on restart,
`reconcileMetadataSizeFromJournal` repairs each file's metadata size from the
journal's durable high-water mark, so a relaxed metadata commit can only lose the
*most recent* size/mtime update — never leave a file inconsistent.

## Selecting a tier

The recommended knob is the per-share **`durability`** enum in the share's
**local block-store config** — `local` (default), `writeback`, or `remote`:

```bash
dfsctl store block local edit <share> --config '{"durability": "remote"}'
```

| `durability` | Ack after | Survives | ~ops/s (create nj=8) |
|---|---|---|--:|
| `local` *(default)* | local journal + metadata `fsync` | process/node crash | ~900 |
| `writeback` | local write, metadata `fsync` deferred ~100 ms | crash, ≤ 100 ms metadata loss | ~1680 |
| `remote` | data durable in S3 | node/disk loss | slow (by design) |

- **`local`** — the default; unchanged from earlier releases.
- **`writeback`** — relaxes the per-op `FILE_SYNC`/`CLOSE` metadata flush from a
  synchronous `badger.DB.Sync` to the 100 ms deferred syncer
  (`durabilitySyncInterval`). Data stays journal-`fsync`'d, so today this lands in
  the **local-durable** band (metadata relaxed only). The full data-writeback tier
  (~5700 ops/s) additionally needs the journal async-commit half — see
  [Remaining work](#remaining-work-1758). Use it for create/write-heavy workloads
  that tolerate losing the last ~100 ms of *metadata* on a hard crash.
- **`remote`** — makes `CLOSE`/`COMMIT` block until the data is durable in the
  remote (S3) store, so an acknowledged write survives losing the whole node. Slow
  by design.

### Underlying flags (advanced / backward-compatible)

The enum composes two lower-level per-share bools, which still work directly if you
need finer control. When `durability` is set it takes precedence; when it is
absent these are honored unchanged:

- **`writeback: true`** — the metadata-relaxed bool on its own (equivalent to
  `durability: writeback`).
- **`require_durable_commit: true`** — the strict CLOSE/COMMIT bool (equivalent to
  `durability: remote`); see
  [Configuration → require_durable_commit](configuration.md#require_durable_commit)
  for the full semantics.

## Read integrity: per-read verification & self-heal

Warm reads (bytes served from the local journal without a remote round-trip) are
verified **per durability tier** — there is no separate knob, the tier you pick
above decides it:

| Tier | Warm-read check | On corruption |
|---|---|---|
| **`writeback`** | none (raw fast read) | n/a — integrity comes from startup-recovery CRC + remote cold-fetch BLAKE3 |
| **`local`** *(default)* / **`remote`** | per-record **CRC32** on every warm read | self-heal (remote) or fail closed with **EIO** (local-only) |

On the durable tiers, every warm read re-reads the covering journal record and
checks its stored CRC32 before returning the requested bytes:

- **Remote-backed share** — on a CRC mismatch the range is **self-healed**: the
  covering chunk is re-fetched from the remote (S3) block store, which is
  BLAKE3-verified on the way in, re-hydrated into the local journal, and the read
  returns the *correct* bytes. The remote is the source of truth; the local tier
  is a cache.
- **Local-only share** — there is no good copy to heal from, so the read **fails
  closed** with `EIO` (`NFS3ERR_IO` / `STATUS_DATA_CHECKSUM_ERROR`). It never
  returns silently-wrong or zero-filled bytes.

This catches on-disk corruption of a valid segment that happens *after* startup
recovery (bit rot, or a bug mutating cached segment bytes) — the case the
startup-recovery CRC and the remote cold-fetch BLAKE3 don't cover on their own.

**Cost.** Verification reads the *whole covering record* to check its CRC, then
slices out the requested sub-range. For large sequential reads that is free (you
would read the record anyway); for **small random reads** it adds read
amplification (a whole record fetched to return a few KiB) plus the CRC32 CPU.
That is exactly why it is **off on the fast `writeback` default** and on for the
durability-sensitive tiers.

### Why opt-in-on-durable-tiers (not always-on, not never)

The local tier is a **cache on disk over a verified remote**, not the primary
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
  remote, so opt-in-on-durable-tiers is the better fit and preserves the fast
  default read path.
- A **background scrubber** (tracked separately as
  [#1490](https://github.com/marmos91/dittofs/issues/1490)) is a *complement*, not
  a substitute — Ceph pairs per-read verify with deep-scrub, and so do we
  (per-read self-heal now, periodic scrub later).

## Measured throughput per tier

File-create + 4 KiB write, 8 threads, NFSv3, badger + S3 remote (median ops/s;
full method and competitor comparison in [BENCHMARKS.md](../BENCHMARKS.md)):

| Config | Tier | ops/s |
|---|---|--:|
| `durability: writeback` + async journal | writeback | ~5700 |
| `durability: writeback` | local-durable (metadata relaxed) | ~1680 |
| `durability: local` *(default)* | local-durable | ~900 |
| `durability: remote` | synchronous-to-S3 | ≈ JuiceFS-default (parity) |

For context, at a matched writeback guarantee DittoFS sustains **3.0× JuiceFS
`--writeback`** and **3.6× s3ql**; the local-durable middle tier is one no S3
filesystem competitor offers at all.

## Remaining work (#1758)

The `durability` enum is shipped: **`local`** and **`remote`** are fully functional,
and **`writeback`** selects the metadata-relaxed path. One piece remains — the full
**data-writeback** tier (~5700 ops/s) needs the journal async-commit half, still a
diagnostic toggle rather than a supported config and gated on the journal
switchover stabilizing. Until it lands, `durability: writeback` delivers the
metadata-relaxed (local-durable) band; `local` and `remote` are complete.

## See also

- [BENCHMARKS.md](../BENCHMARKS.md) — per-tier throughput and competitor matrix
- [Configuration](configuration.md) — full block-store config reference
- [Choosing stores](choosing-stores.md) — local vs remote block-store trade-offs
