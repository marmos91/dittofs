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
> | Block store (per share) | File content (chunks) | `s3`, `memory` | `dfsctl store block …` |
> | Journal (per share) | Writes not yet offloaded to the block store | none — always on disk | `blockstore.journal.path` in config |

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
**dedup is always on**, no toggle). Every share has exactly **one block store** — the
durable home for its content — and an on-disk **journal** in front of it. The journal is
not a store you choose: it is provisioned automatically under `blockstore.journal.path`,
absorbs writes, and hands them to an async syncer that offloads them to the block store.

| Type | Latency | Capacity | Durability | When to choose |
|------|---------|----------|------------|----------------|
| `s3` | network | effectively unlimited | ✅ off-box, replicated by provider | **Default.** Durable, scalable backing store |
| `memory` | lowest | RAM-bound | ❌ ephemeral | Tests only |

**Best practices**

- Run **`s3`** for real workloads: writes hit the journal first and sync to S3 in the
  background; reads are served from the journal and fetched from S3 on miss.
- Size the journal to your hot set with the per-share `--journal-size`. Leaving it unset
  means no configured ceiling — see [Configuration § Journal size](configuration.md#journal-size-and-eviction).
- DittoFS speaks the **S3 API**, so [Cubbit DS3](https://www.cubbit.io/) (a DittoFS sponsor),
  MinIO, Ceph RGW, GCS (set `force_path_style: false`), Backblaze B2, Wasabi, DigitalOcean
  Spaces, Alibaba OSS, Oracle OCI, Storj, etc. all work —
  see the verified endpoint snippets in [Configuration § Block Store](configuration.md#6-block-store-configuration).
- Dedup happens automatically across files in a share; identical content is stored once.
- Pick a **commit acknowledgement** per share (`journal` or `block-store`; see the
  [durability guide](durability.md)) — it sets how far a write must land before an NFS
  COMMIT or SMB Flush is acknowledged.
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
# Durable single-node share: badger metadata, S3 backing
dfsctl store metadata add --name default     --type badger
dfsctl store block add --name s3-remote --type s3
dfsctl share create --name /export --metadata default --block-store s3-remote
```

Building a custom backend instead of choosing a built-in one? See
[Implementing stores](../internals/implementing-stores.md).
