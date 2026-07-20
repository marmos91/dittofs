# Sizing & Hardware

How to size the machine that runs `dfs`. For picking *which* stores to run, see
[Choosing Stores](choosing-stores.md); this page covers the hardware they need.

> **Read this first.** DittoFS is experimental and **not production ready**. There is no
> supported production configuration and no true high-availability today (see
> [single-node](#single-node-not-a-cluster) below). Treat everything here as guidance for a
> **pilot / evaluation / internal single-node** deployment, and load-test with your own
> workload before committing.

## Single-node, not a cluster

Size **one box**, not a fleet. Block stores are per-share and their local cache directories
are **always isolated** — even with a shared PostgreSQL metadata store (which is
multi-writer), you cannot run active-active `dfs` replicas against the same data. So
"sizing" means sizing a single server, plus:

- an **S3 backend** you size and scale separately (Cubbit DS3, MinIO, Ceph RGW, …), and
- a **control-plane database** (SQLite by default — nothing to size).

The realistic topology is **badger metadata + local `fs` cache + remote `s3` backing**.

## What drives each resource

| Resource | What drives it | How to size it |
|----------|----------------|----------------|
| **RAM** | Badger auto-sizes its block/index caches from available RAM (cgroup-aware in containers). Plus per-connection NFS/SMB buffers. FastCDC/BLAKE3 streaming buffers are pooled and **capped at the chunk size** — they do *not* grow with file size. | Give the process a real RAM budget: badger spends it on metadata cache, so more RAM means a hotter cache and faster `lookup`/`getattr`/`readdir`. If the cache hit ratio drops on a large metadata set, pin `metadata.badger.block_cache_mb` / `index_cache_mb`. |
| **Disk (local cache)** | The local `fs` tier is a **write-through cache** in front of S3, defaulting to **10 GiB per share** (`blockstore.local.default_remote_cache_size`). Metadata (badger LSM) also lives on disk and grows with inode/file count. | Size the cache to your **hot working set**, not total data — total data lives in S3. Raise the ceiling if the working set exceeds 10 GiB. Use NVMe/SSD: writes hit local first. |
| **CPU** | The write path is CPU-bound: FastCDC chunking + BLAKE3 hashing on every write (dedup is always on), plus encryption if enabled. | Cores help write throughput and concurrent connections. At small scale writes are typically per-op / `fsync`-bound rather than CPU-starved. |
| **Network** | Background sync to the S3 backend, plus client NFS/SMB traffic. | Bandwidth to the S3 endpoint gates durable-write acknowledgement and cold-read latency. Keep `dfs` close to its S3 backend. |

## Starting points

Anchor points, not guarantees — validate against your workload.

| Tier | vCPU | RAM | Local cache disk | Notes |
|------|------|-----|------------------|-------|
| **Pilot / eval** | 2–4 | 4–8 GiB | 20–50 GiB SSD | Default 10 GiB cache is fine for small hot sets |
| **Single-node "serious"** | 8 | 16–32 GiB | 100–500 GiB NVMe (≥ hot working set) | Raise the badger cache; NVMe matters — writes hit local first |
| **Heavy** | 16+ | 64 GiB+ | 1 TiB+ NVMe | Past the tested envelope — validate, don't assume |

Add, separately: an **S3 backend** sized for *total* data, and a **control-plane DB**
(SQLite by default; PostgreSQL only if you already operate one or want replicas of the
control plane).

## Rules of thumb

- **Cache = hot set, S3 = everything.** Don't size local disk for total capacity.
- **RAM buys metadata speed.** The metadata store is the hot path for every filesystem
  operation; give badger room to cache it.
- **NVMe for the local tier.** Writes land locally before syncing to S3.
- **Co-locate with S3.** Round-trips to the backend gate durable writes and cold reads.
- **One box.** No active-active HA today — plan for a single node.

## See also

- [Choosing Stores](choosing-stores.md) — picking metadata and block stores
- [Durability](durability.md) — how far a write must land before it is acknowledged
- [Configuration](configuration.md) — every config key and CLI flag
