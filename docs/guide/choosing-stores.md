# Choosing Stores

A DittoFS share is assembled from a **metadata store** (where file/directory metadata
lives) and a **block store** (where file content lives). There is also one **control-plane
database** per server (separate from any share). This guide helps you pick each one. For
the exact config keys and CLI flags, see [Configuration](configuration.md).

> **Three different things — don't confuse them:**
> | Layer | What it holds | Choices | Configured by |
> |-------|---------------|---------|---------------|
> | Control-plane database | Users, shares, permissions, policies | `sqlite`, `postgres` | `database.*` in config |
> | Metadata store (per share) | Inodes, names, attrs, ACLs, dedup index | `memory`, `badger`, `sqlite`, `postgres` | `dfsctl store metadata add` |
> | Block store (per share) | File content (chunks) | local `fs`/`memory` + remote `s3` | `dfsctl store block …` |

## Metadata store (per share)

This is the hot path for every `lookup`, `getattr`, `readdir`, and `create`. Pick by
durability needs and how many server processes must share it.

| Store | Durable? | Concurrency | Ops overhead | When to choose |
|-------|----------|-------------|--------------|----------------|
| `memory` | ❌ lost on restart | in-process | none | Tests, throwaway demos, caching-only workloads |
| `badger` | ✅ embedded LSM | single process | none (embedded) | **Default.** Single-node servers wanting durability with zero external deps |
| `sqlite` | ✅ single file (WAL) | single writer | minimal (one file) | Edge / appliance / single-binary deploys; easy to back up (copy the file) |
| `postgres` | ✅ external RDBMS | multi-writer (MVCC) | run/operate a DB | Multiple server processes, HA, or horizontal scale |

**Best practices**

- **Badger** auto-sizes its block/index caches from available RAM (cgroup-aware in
  containers). For large metadata sets, watch the cache hit ratio and set
  `metadata.badger.block_cache_mb` / `index_cache_mb` explicitly if it drops. Each
  isolated share can run its own Badger instance.
- **SQLite** is pure-Go (no cgo) and reuses the PostgreSQL data model (hard links via
  `parent_child_map`, `nlink`, recursive-CTE path reconstruction, `object_id` dedup index).
  It is **single-writer** — fine for one server, not for multi-process HA.
- **PostgreSQL** is the only option that supports multiple server processes against the
  same metadata. Size the connection pool (`MaxConns`, default 10) to your concurrency.
- **Memory** keeps nothing across restarts. Never use it for data you want back.

## Block store (per share)

Content is split into content-addressed chunks (FastCDC chunking + BLAKE3 hashing,
**dedup is always on**, no toggle). Each share has a **local tier** (fast, on-box) and an
optional **remote tier** (durable, off-box). The local tier acts as a **write-through
cache** in front of the remote — it is not the source of truth once a remote is attached.

| Tier / type | Latency | Capacity | Durability | When to choose |
|-------------|---------|----------|------------|----------------|
| local `memory` | lowest | RAM-bound | ❌ ephemeral | Tests only |
| local `fs` | low (disk) | disk-bound | ✅ on that host | Always — this is the cache/fast tier |
| remote `s3` | network | effectively unlimited | ✅ off-box, replicated by provider | Durable, scalable backing store |

**Best practices**

- Run **local `fs` + remote `s3`** for real workloads: writes hit local first and sync to
  S3 in the background; reads are served from cache and fetched on miss.
- Size the local cache to your hot set. The remote write-through cache defaults to ~10 GiB
  (`blockstore.local.default_remote_cache_size`); raise it if your working set is larger.
- DittoFS speaks the **S3 API**, so [Cubbit DS3](https://www.cubbit.io/) (a DittoFS sponsor),
  MinIO, Ceph RGW, GCS (set `force_path_style: false`), Backblaze B2, Wasabi, DigitalOcean
  Spaces, Alibaba OSS, Oracle OCI, Storj, etc. all work —
  see the verified endpoint snippets in [Configuration § Block Store](configuration.md#6-block-store-configuration).
- Dedup happens automatically across files in a share; identical content is stored once.
- Tune append-log pressure (`max_log_bytes`, default ~25% of capacity) and durability
  (`require_durable_commit`) per store — see [Configuration](configuration.md).
- To migrate a legacy block layout to the content-addressed layout, see
  [Block store migration](block-store-migration.md).

## Control-plane database

One per server, holds users/shares/permissions/policies — **not** file data.

| Type | When to choose |
|------|----------------|
| `sqlite` | **Default.** Single binary, nothing extra to run. |
| `postgres` | Multiple server replicas or you already operate Postgres. |

## A typical setup

```bash
# Durable single-node share: badger metadata, fs cache, S3 backing
dfsctl store metadata add --name default     --type badger
dfsctl store block local  add --name local-cache --type fs
dfsctl store block remote add --name s3-remote   --type s3
dfsctl share create --name /export --metadata default \
  --local local-cache --remote s3-remote
```

Building a custom backend instead of choosing a built-in one? See
[Implementing stores](../internals/implementing-stores.md).
