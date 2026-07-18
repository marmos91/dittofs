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

## Measured throughput per tier

File-create + 4 KiB write, 8 threads, NFSv3, badger + S3 remote (median ops/s;
full method and competitor comparison in [BENCHMARKS.md](../BENCHMARKS.md#create-throughput-across-durability-tiers-1735)):

| Config | Tier | ops/s |
|---|---|--:|
| `durability: writeback` + async journal | writeback | ~5700 |
| `durability: writeback` | local-durable (metadata relaxed) | ~1680 |
| `durability: local` *(default)* | local-durable | ~900 |
| `durability: remote` | synchronous-to-S3 | not yet benchmarked |

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
