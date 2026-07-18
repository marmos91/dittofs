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

## Knobs available today

Two independent per-share flags live in the share's **local block-store config**.
Neither is set by default (both `false` = the local-durable default tier).

### `writeback` — relax metadata durability *(new)*

Downgrades the per-op metadata flush on `FILE_SYNC` writes and `CLOSE` from a
synchronous `badger.DB.Sync` to the deferred relaxed path (flushed by the 100 ms
background syncer, `durabilitySyncInterval`). Data is still journalled; only
metadata (size/mtime/dirent) durability is deferred, bounded to roughly that
100 ms interval.

```bash
dfsctl store block local edit <share> --config '{"writeback": true}'
```

Use it for create/write-heavy workloads that can tolerate losing the last ~100 ms
of *metadata* on a hard crash (scratch space, CI artifacts, re-derivable data). On
its own this flag lands in the **local-durable** band (data still journal-`fsync`'d,
metadata relaxed — ~1680 ops/s below); combined with the journal async-commit half
([#1758](https://github.com/marmos91/dittofs/issues/1758)) it reaches the full
writeback tier. It is the single biggest create-throughput lever — see below.

### `require_durable_commit` — synchronous-to-S3

The opposite direction: makes `CLOSE`/`COMMIT` block until the data is durably in
the remote (S3) store, so an acknowledged write survives losing the whole node.
Slow by design. See
[Configuration → require_durable_commit](configuration.md#require_durable_commit)
for the full CLOSE/COMMIT semantics.

```bash
dfsctl store block local edit <share> --config '{"require_durable_commit": true}'
```

## Measured throughput per tier

File-create + 4 KiB write, 8 threads, NFSv3, badger + S3 remote (median ops/s;
full method and competitor comparison in [BENCHMARKS.md](../BENCHMARKS.md#create-throughput-across-durability-tiers-1735)):

| Config | Tier | ops/s |
|---|---|--:|
| `writeback: true` + async journal | writeback | ~5700 |
| `writeback: true` | local-durable (metadata relaxed) | ~1680 |
| *default* | local-durable | ~900 |
| `require_durable_commit: true` | synchronous-to-S3 | not yet benchmarked (#1758) |

For context, at a matched writeback guarantee DittoFS sustains **3.0× JuiceFS
`--writeback`**; the local-durable middle tier is one no S3 filesystem competitor
offers at all.

## Planned: named `durability` tier (#1758)

Today you compose the tier from the two flags above. Two gaps remain:

- The **writeback** row above still needs the journal's async-commit half, which is
  a diagnostic toggle, not yet a supported config.
- There is no single, discoverable per-share **`durability`** enum.

[#1758](https://github.com/marmos91/dittofs/issues/1758) folds all of this into one
per-share setting — `durability: writeback | local | remote` — composing journal
async-commit, metadata `writeback`, and block `require_durable_commit` into three
named tiers. The `remote` tier is the productionized synchronous-to-S3 path. Until
that lands, use the two flags above; the default (neither set) is unchanged and
remains local-durable.

## See also

- [BENCHMARKS.md](../BENCHMARKS.md) — per-tier throughput and competitor matrix
- [Configuration](configuration.md) — full block-store config reference
- [Choosing stores](choosing-stores.md) — local vs remote block-store trade-offs
