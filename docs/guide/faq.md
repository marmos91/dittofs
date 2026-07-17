# Frequently Asked Questions

Common questions about DittoFS and their answers.

## Table of Contents

- [General Questions](#general-questions)
- [Technical Questions](#technical-questions)
- [Usage Questions](#usage-questions)
- [Comparison Questions](#comparison-questions)
- [Known Limitations](#known-limitations)
  - [NFS Protocol Limitations](#nfs-protocol-limitations)
  - [SMB Client Limitations](#smb-client-limitations)
  - [Storage Backend Limitations](#storage-backend-limitations)
  - [General Limitations](#general-limitations)
  - [POSIX Compliance Summary](#posix-compliance-summary)

## General Questions

### What is DittoFS?

DittoFS is a modular virtual filesystem written entirely in Go that decouples file access protocols
from storage backends. It supports NFSv3, NFSv4/v4.1, and SMB2 with pluggable metadata and block
stores, making it easy to serve files over multiple protocols from various backends (memory,
filesystem, S3, BadgerDB, PostgreSQL, etc.).

### Why not use FUSE?

FUSE adds an additional abstraction layer and requires kernel modules. DittoFS runs entirely in
userspace and implements protocols directly, giving better control over protocol behavior, easier
debugging, and no kernel dependencies. This also makes deployment simpler - just a single binary with
no special permissions.

### Can I use this in production?

**Not yet**. DittoFS is experimental and needs:
- More testing and hardening
- Security auditing
- Performance optimization
- Production deployment experience

Use it for development, testing, and experimentation, but wait for a stable 1.0 release before production use.

### What license is DittoFS under?

DittoFS is released under the MIT License, which is permissive and allows commercial use.

## Technical Questions

### Which NFS versions are supported?

DittoFS supports **NFSv3 over TCP** (28 procedures fully implemented), **NFSv4.0**, **NFSv4.1**, and **NFSv4.2** (extended attributes, RFC 8276) with features including:
- Compound operations and sessions
- File and directory delegations with CB_NOTIFY
- ACLs (Access Control Lists)
- Kerberos authentication via RPCSEC_GSS

### Does it support file locking?

Yes. **NFSv4** provides built-in (in-protocol) file locking — nothing extra to
enable, and the recommended path. **SMB2** supports byte-range locking (shared
and exclusive).

**NFSv3** locking uses the separate NLM (Network Lock Manager) side protocol.
DittoFS implements NLM (v1/v3 with 32-bit offsets, v4 with 64-bit) and NSM, over
both TCP and UDP. Because BSD/macOS lock clients (`rpc.lockd`/`rpc.statd`) reach
NLM/NSM over **UDP** and discover it via the standard portmapper on **port 111**,
NFSv3 locking from those clients requires two opt-in settings on the server:

- enable the UDP transport: `dfsctl adapter settings nfs update --udp-enabled true`
- enable the portmapper, reachable on **port 111**: `dfsctl adapter settings nfs
  update --portmapper-enabled true --portmapper-port 111` (binding 111 needs root
  / `NET_BIND_SERVICE`; on Kubernetes the operator exposes it via the Service)

then restart the adapter. Both are **disabled by default**. Note that NFSv3
**mount and read/write work without any of this** — only NLM locking needs it.
If you don't need cross-client locking, mount with `-o nolock`, or prefer
`-o vers=4` (`dfsctl share mount --nfs-version 4.1`) to get locking in-protocol
with no NLM/portmapper setup. See [NFS.md](nfs.md#nfsv3-file-locking-nlmnsm).

### Does it support Kerberos authentication?

Yes. NFSv4 supports Kerberos via RPCSEC_GSS, and SMB supports Kerberos via SPNEGO alongside NTLM.

### Can I implement my own protocol adapter?

Yes! That's one of the main goals of DittoFS. Implement the `Adapter` interface and wire it to the metadata/block stores:

```go
type Adapter interface {
    Serve(ctx context.Context) error
    Stop(ctx context.Context) error
    SetRuntime(*runtime.Runtime)
    Protocol() string
    Port() int
}
```

See [ARCHITECTURE.md](../internals/architecture.md) for details.

### Can I implement my own storage backend?

Absolutely! Implement either or both of these interfaces:

- **Metadata Store**: `pkg/metadata/Store` interface
- **Local Block Store**: `pkg/block/local.LocalStore` interface
- **Remote Block Store**: `pkg/block/remote.RemoteStore` interface

See [IMPLEMENTING_STORES.md](../internals/implementing-stores.md) for implementation guidelines.

### How does performance compare to kernel NFS?

The lack of FUSE overhead and optimized Go implementation provides competitive performance for most workloads. Results show:

- Good sequential read/write performance
- Efficient handling of small files
- Low latency for metadata operations
- Scales well with concurrent connections

### My BadgerDB logs "Block cache might be too small ... hit-ratio: 0.26 ... sets-rejected" — what do I do?

That message means the BadgerDB metadata engine's in-memory **block cache** is
undersized for your working set, so it is thrashing: most lookups miss the cache
and hit disk. A low hit-ratio also widens the window for the dedup
transaction-conflict race and the append-log "pressure wait timed out" stall, so
it is worth fixing.

By default the block and index caches **auto-size from the memory available to
the process** (≈15 % / ≈7.5 %, with 512/256 MiB floors and 4 GiB/2 GiB
ceilings), so a 4 GiB host already gets ~614 MiB / ~307 MiB without tuning. If
you still see the warning, your hot metadata set is larger than the auto-sized
cache — raise it explicitly:

```yaml
metadata:
  badger:
    block_cache_mb: 2048   # raise first; 0 = auto-size
    index_cache_mb: 1024
```

or via env (`DITTOFS_METADATA_BADGER_BLOCK_CACHE_MB=2048`). See
[CONFIGURATION.md → BadgerDB cache sizing](configuration.md#badgerdb-cache-sizing-config-file)
for a sizing table keyed to object/metadata count. Aim for a hit-ratio above
~0.8 with no `sets-rejected`.

### Does metadata persist across server restarts?

It depends on the metadata store:

- **Memory backend** (`type: memory`): No, all data is lost on restart
- **BadgerDB backend** (`type: badger`): Yes, all metadata persists
- **PostgreSQL backend** (`type: postgres`): Yes, all metadata persists across restarts and supports distributed deployments

Configure your metadata store accordingly:

```bash
./dfsctl store metadata add --name persistent --type badger \
  --config '{"path":"/var/lib/dittofs/metadata"}'
```

### Can I import an existing filesystem into DittoFS?

Not yet, but the path-based file handle strategy in BadgerDB enables this as a future feature. The
handles are deterministic based on file paths (`shareName:/path/to/file`), making filesystem scanning
and import possible.

### Does DittoFS deduplicate blocks across files?

Yes. The block store is content-addressable (CAS): file content is chunked
with FastCDC (min 1 MiB / avg 4 MiB / max 16 MiB, normalization level 2),
each chunk is hashed with BLAKE3, and chunks are stored under a hash-keyed
`blocks/{hh}/{hh}/{hex}` layout locally and `cas/{hh}/{hh}/{hex}` remotely.
Two files that share a chunk reference the same stored object, so
**chunk-level dedup is automatic** on write.

On top of that, a **file-level dedup short-circuit** can skip uploading a
file's chunks entirely when an identical file already exists (see the next
two questions).

### What's an ObjectID and when does it get computed?

An `ObjectID` is a BLAKE3 Merkle root over a file's content-defined
chunk hashes:

    ObjectID = BLAKE3("dittofs:objectid:v1\x00" || h0 || h1 || ... || hN-1)

where `hi` is the i-th `BlockRef.Hash` when `FileAttr.Blocks` is sorted
by `Offset`. Two files with byte-identical content always have the
same ObjectID; two files differing in even one byte have different
ObjectIDs (FastCDC + BLAKE3 are both deterministic, so identical
chunks yield identical hashes).

DittoFS computes ObjectID **lazily — at file quiesce**, when every
chunk has finished uploading to remote storage. Mid-write the
ObjectID is the all-zero sentinel meaning "not yet quiesced". The
post-Flush coordinator hook
(`Syncer.persistFileChunksAfterFlush`) writes it in the same metadata
transaction that updates `FileAttr.Blocks`/`Size`/`Mtime`. Partial
flushes (some blocks still `Pending`) leave it at zero so the
short-circuit lookup never returns a half-quiesced file.

A non-zero ObjectID always reflects a fully-`Remote` consistent
state. Empty files dedup to one canonical constant
`BLAKE3("dittofs:objectid:v1\x00")`; files written before ObjectID existed
keep the all-zero sentinel until their next full flush recomputes it.

See
[ARCHITECTURE.md — File-Level Dedup](../internals/architecture.md#file-level-dedup-objectid--merkle-root)
for the full design.

### Why doesn't my file dedup until I close it?

DittoFS's file-level dedup short-circuit fires at quiesce, not on every
write. Mid-write, blocks dedup at the *chunk* level (`GetByHash`) — if your
write produces a chunk that already exists remotely, no PUT is issued.

But the savings of skipping every chunk's upload entirely (file-level
dedup) only kick in once the full file fingerprint exists — the
ObjectID — and that fingerprint is computed at the post-Flush
coordinator hook on `Close`/`Flush`/`fsync`, when the BlockRef list
stabilizes.

This is intentional: file-level dedup targets the workflow of cloning
a VM image or copying a large file (where the whole content arrives
in one burst). Random in-place writes inside a running VM benefit
from the chunk-level path and don't get penalized waiting for a
quiesce-only fingerprint. The trigger condition — "all blocks `Pending`
AND no prior ObjectID" — explicitly excludes the running-VM hot path.

### How does garbage collection work?

The block-store GC is a fail-closed mark-sweep over the union of every live
block's `ContentHash`:

1. **Mark.** Stream every `FileChunk`'s `ContentHash` via the
   `MetadataStore.EnumerateFileChunks(ctx, fn)` cursor across **every
   share that targets the same remote** (cross-share aggregation by
   `bucket+endpoint+prefix`, not share name). The live set is built on
   disk under `<localStore>/gc-state/<runID>/db/` (memory-bounded
   regardless of metadata size). Hold providers then add hashes the
   namespace no longer references but that must survive: snapshot
   manifests, and files unlinked while still held open (NFSv4 open
   stateids, SMB open handles) — POSIX unlink-while-open data stays
   readable until the last close, beyond the grace period.
2. **Sweep.** A single `RemoteStore.Walk` enumerates every CAS object
   cluster-wide (the backend paginates internally). An object is kept
   iff its hash is in the live set OR its `LastModified` is newer than
   `snapshot − gc.grace_period` (default 1h). Otherwise it is deleted.

The mark phase is fail-closed: any error aborts the sweep entirely.
Sweep-side per-object DELETE failures are captured and continue;
garbage that survives a transient is reclaimed on the next run.

Triggers:

- Periodic GC is not yet wired. There is no scheduler; schedule via cron
  until one ships.
- On-demand via `dfsctl store block gc <share> [--dry-run]`. Inspect the
  most recent run with `dfsctl store block gc-status <share>`. The
  mark-sweep is global across every share that targets the same remote, so
  `<share>` selects which remote(s) to scan.

See [ARCHITECTURE.md](../internals/architecture.md#garbage-collection-mark-sweep)
and [CONFIGURATION.md](configuration.md) for the full design and every
`gc.*` knob.

### Why is the cache cold after a write?

It is **not**. Cache invalidation is **surgical**: a write drops only the
chunk-level entries that the write actually invalidated, not the entire
file. Other chunks referenced by the file — and any chunks shared with
other files via cross-file dedup — stay warm.

If you are seeing whole-file cold misses after a write, that is a bug.
File a report with the `dfsctl store block audit-refcounts <share>`
output (see below) — refcount drift between `FileChunk.RefCount` and
`FileAttr.Blocks` is the most common root cause.

The mechanism: `engine.WriteAt` returns the new `[]BlockRef`, the
caller commits it in the same metadata transaction that updates
`Mtime`/`Size`, then calls `Cache.InvalidateFile(payloadID,
removedHashes)` with **only the hashes that disappeared from the
file**. Hashes that survived (unchanged ranges) and hashes still
referenced by other files via dedup remain in the cache.

### How do I run the refcount audit?

The audit checks the invariant `∑ FileChunk.RefCount == ∑ len(FileAttr.Blocks)`
— every block reference in `FileAttr.Blocks` across all files MUST be
matched by a refcount on the corresponding `FileChunk`:

```bash
# Aggregate counts to stdout (text by default)
dfsctl store block audit-refcounts /archive

# Structured JSON for log aggregation / alerting
dfsctl store block audit-refcounts /archive -o json

# YAML if your tooling prefers it
dfsctl store block audit-refcounts /archive -o yaml
```

The output reports `share`, `started_at`, `duration_ms`,
`total_files`, `total_refs`, `total_refcount`, and `delta`. **A non-zero
`delta` indicates refcount drift** and SHOULD be triaged. The audit
also persists its last-run summary at
`<localStore>/audit-state/last-inv02.json`.

The audit is **operator-invoked**, not periodic. Schedule via cron at
the cadence that matches your operational risk tolerance.

For belt-and-braces protection, the property-based fuzzer at
`pkg/metadata/storetest/inv02_fuzz_test.go` runs against all three
built-in backends in CI on every PR, asserting the refcount invariant at
every quiescent point under concurrent create/delete/copy load.

See [CLI.md](cli.md) for the full reference.

### What's a BlockRef?

A `BlockRef` is the 3-tuple `(Hash ContentHash, Offset uint64, Size
uint32)` defined in `pkg/block/types.go`. `FileAttr.Blocks
[]BlockRef` is the **authoritative content list** for every file: which
chunks compose the file, where each chunk sits inside the file, and how
big it is.

The list is:

- **Sorted by `Offset`** so the engine can binary-search it
  (`findBlocksForRange` in `pkg/block/engine/range.go`).
- **Populated on every sync finalization** — the engine returns the
  new `[]BlockRef` from `WriteAt`/`Truncate`/`Delete`/`CopyPayload`
  and the caller persists it in the same metadata transaction.

`BlockRef.Hash` is the `ContentHash` (32-byte BLAKE3) under which the
chunk is stored in the CAS keyspace `cas/{hh}/{hh}/{hex}`. Two files
referencing the same chunk via dedup share one `Hash`, which is what
makes cross-file dedup work both for storage (CAS) and in the cache (a
shared hash hits the same entry).

See [ARCHITECTURE.md](../internals/architecture.md#engine-api--blockref--cache)
for the full design and
[IMPLEMENTING_STORES.md](../internals/implementing-stores.md) for storage-encoding
requirements.

### How do I migrate an older store layout?

Upgrades from the v0.16–v0.21 standalone-CAS layout are automatic: each
share converts its leftover per-chunk objects into packed blocks on the
first start after the upgrade, blocking until done (idempotent and
resumable — a killed migration converges on the next start).

Stores still on the pre-v0.16 `.blk` layout must first be migrated with
dittofs v0.21 or earlier (`dfs migrate-to-cas`, removed in later
releases); the boot guard refuses `.blk` layouts with exit code 78. See
[the migration guide](block-store-migration.md) for the full runbook.

## Usage Questions

### Can I use this with Windows clients?

Yes. DittoFS supports SMB2, which is the native Windows file sharing protocol. Windows clients can connect directly without additional software. NFS is also available (Windows 10 Pro and Enterprise include an NFS client).

### How do I mount DittoFS shares?

**Linux (NFS):**
```bash
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test
```

**macOS (NFS):**
```bash
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,resvport localhost:/export /mnt/test
```

**Windows (SMB):**
```powershell
net use Z: \\localhost\export /user:username password
```

See [NFS.md](nfs.md) and [SMB.md](smb.md) for more details.

### Can I have multiple shares with different backends?

Yes! This is a core feature. Create stores and shares via CLI:

```bash
# Create metadata stores
./dfsctl store metadata add --name fast-memory --type memory
./dfsctl store metadata add --name persistent-db --type badger \
  --config '{"path":"/var/lib/dittofs/metadata"}'

# Create block stores (local for fast access, remote for durability)
./dfsctl store block local add --name local-disk --type fs \
  --config '{"path":"/var/lib/dittofs/blocks"}'
./dfsctl store block remote add --name cloud-s3 --type s3 \
  --config '{"region":"us-east-1","bucket":"my-bucket"}'

# Create shares referencing different stores
./dfsctl share create --name /temp --metadata fast-memory --local local-disk
./dfsctl share create --name /archive --metadata persistent-db \
  --local local-disk --remote cloud-s3
```

See [CONFIGURATION.md](configuration.md) for more examples.

### Can multiple shares share the same metadata store?

Yes! Multiple shares can reference the same store instance for resource efficiency:

```bash
# Create one shared metadata store
./dfsctl store metadata add --name shared-meta --type badger \
  --config '{"path":"/var/lib/dittofs/shared-metadata"}'

# Create separate remote block stores
./dfsctl store block add --kind local --name shared-local --type fs \
  --config '{"path":"/var/lib/dittofs/blocks"}'
./dfsctl store block add --kind remote --name s3-prod --type s3 \
  --config '{"region":"us-east-1","bucket":"prod-bucket"}'
./dfsctl store block add --kind remote --name s3-archive --type s3 \
  --config '{"region":"us-east-1","bucket":"archive-bucket"}'

# Both shares use the same metadata store; remote stores are ref-counted
./dfsctl share create --name /prod --metadata shared-meta \
  --local shared-local --remote s3-prod
./dfsctl share create --name /archive --metadata shared-meta \
  --local shared-local --remote s3-archive
```

### Is there a recycle bin / can I recover deleted files?

Yes, on an opt-in, per-share basis. When a share has the recycle bin
enabled, deleting a file or directory moves it into a visible
`#recycle` directory at the share root instead of destroying it. You
can browse and restore from `#recycle` over NFS or SMB like any other
folder (just drag the item back out), or manage it with
`dfsctl trash`:

```bash
# Enable the bin on a new or existing share
dfsctl share create --name /docs ... --enable-trash
dfsctl share edit /docs --enable-trash true

# List, restore, inspect, and empty the bin
dfsctl trash list /docs
dfsctl trash restore /docs "#recycle/report.txt"
dfsctl trash status /docs
dfsctl trash empty /docs --force
```

Retention is configurable per share: `--trash-retention-days N`
auto-purges entries older than N days (`0` = keep forever), and
`--trash-max-size BYTES` caps the bin and evicts oldest-first when
exceeded (`0` = unbounded). A background reaper enforces both hourly.

Caveats:

- The bin is a **single shared `#recycle` per share** — not per-user.
- Triggers are **unlink** (NFS REMOVE/RMDIR, SMB delete-on-close) and
  **replace-overwrite** (a rename/copy that clobbers an existing file
  recycles the victim). In-place truncate/overwrite of a file's
  *content* is **not** recycled.
- Deleting an item that is already inside `#recycle` is **permanent**.
- **Disabling trash auto-empties the bin**, permanently deleting its
  contents.
- Exclude globs (`--trash-exclude GLOB`, repeatable) cause matching
  deletions to bypass the bin entirely.

See [CLI.md](cli.md#recycle-bin-trash) for the full command reference,
[CONFIGURATION.md](configuration.md#recycle-bin-trash) for the
per-share settings, and [ARCHITECTURE.md](../internals/architecture.md#metadataservice)
for the recycle-trap design.

### How do I enable debug logging?

**Via environment variable:**
```bash
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start
```

**Via configuration:**
```yaml
logging:
  level: DEBUG
  format: text
```

### Why do I get "permission denied" errors?

Common causes:

1. **Identity mapping**: Try enabling `map_all_to_anonymous: true` for development
2. **Root directory permissions**: Set `mode: 0777` temporarily to isolate the issue
3. **Client UID mismatch**: Check your UID with `id` command
4. **Export restrictions**: Check `allowed_clients` in configuration

See [TROUBLESHOOTING.md](troubleshooting.md) for solutions.

## Comparison Questions

### How does DittoFS compare to traditional NFS servers?

| Feature | Traditional NFS | DittoFS |
|---------|----------------|---------|
| Permission Requirements | Kernel-level | Userspace only |
| Storage Backend | Filesystem only | Pluggable |
| Metadata Backend | Filesystem only | Pluggable (Memory/BadgerDB/PostgreSQL) |
| Language | C/C++ | Pure Go |
| Deployment | Complex (kernel modules) | Single binary |
| Multi-protocol | Separate servers | Unified (NFS + SMB) |
| Customization | Limited | Full control |

### How does DittoFS compare to cloud storage gateways?

| Feature | Cloud Gateways | DittoFS |
|---------|---------------|---------|
| Vendor Lock-in | Often present | None |
| Protocol Support | Limited | Extensible |
| Storage Backend | Vendor-specific | Pluggable |
| Cost | Often high | Free and open-source |
| Customization | Limited | Full control |
| Deployment | Complex | Single binary |

### How does DittoFS compare to go-nfs?

Both are NFS implementations in Go, but with different goals:

**go-nfs:**
- Library-focused
- Embeddable in other projects
- Minimal configuration

**DittoFS:**
- Complete server application
- Store registry pattern for sharing resources
- Multi-share and multi-protocol support (NFS + SMB)
- Extensive configuration system
- Multiple backend options
- Production features (metrics, rate limiting, graceful shutdown)
- NFSv4/v4.1 support with delegations and Kerberos

### What's unique about DittoFS?

1. **Store Registry Pattern**: Named, reusable stores that can be shared across exports
2. **Multi-Protocol**: NFS (v3, v4, v4.1) and SMB2 from a single server
3. **Production-Oriented**: Built-in metrics, rate limiting, graceful shutdown
4. **Flexible Storage**: Mix and match backends per share
5. **Pure Go**: Easy deployment, no C dependencies
6. **Modern Architecture**: Designed for cloud-native deployments

## Durability & caching

**What durability guarantee does a write get?**

DittoFS follows the standard NFS and SMB durability contract — the same one a stock
Linux kernel NFS server or Samba gives you:

- **Writes are buffered and acknowledged fast; durability is guaranteed at COMMIT
  (NFSv3), or at FLUSH / CLOSE (SMB2/3).** This is exactly what the protocols
  specify. A Linux `knfsd` export defaults to `sync` (durable at COMMIT); Samba
  defaults to `strict sync = yes` (honor client FLUSH). DittoFS matches both. The
  `async` knfsd mode some setups use is a non-default, explicitly-unsafe opt-in that
  can silently lose data on a crash — DittoFS does **not** do that by default.
- **At COMMIT/FLUSH the data is made durable in DittoFS's local block store**
  (fsync'd to local disk), and is uploaded to the object store (S3) **asynchronously**
  by the background syncer. So a write that has been COMMIT-acked is **crash-durable**
  — it survives a server process crash or power loss via the local journal — and its
  object-store copy follows shortly after. This is the same "local-durable, async to
  object store" tier as JuiceFS `--writeback` or a kernel NFS server over a local
  disk. It means a *total node loss* (local disk destroyed) between COMMIT and the
  syncer catching up can lose the not-yet-uploaded tail; use snapshots / the syncer's
  drain for stronger guarantees before decommissioning a node.

**Do I need to configure durability?**

Usually no — the tradeoff is already in your hands through the **standard protocol**,
and DittoFS honors whatever the client asks:

- NFSv3 clients tag each write `UNSTABLE` / `FILE_SYNC` and decide when to COMMIT
  (on `fsync`/`close`/cache pressure). Want maximum speed? Let the client buffer and
  COMMIT less often, or the application skip `fsync`. Want stricter safety? The client
  can mount `sync` or issue `FILE_SYNC` writes. DittoFS obeys the flag either way.
- SMB2/3 clients get the same via the write-through flag + SMB2 FLUSH.

There is no DittoFS-specific durability knob to learn — you tune it the way you'd tune
any NFS/SMB server: with mount options and application `fsync` behavior.

**Caching and read performance.** DittoFS is a *native userspace* server (no kernel
FUSE), so warm reads are served from the standard NFS/SMB **client** page cache (as
with any server) plus DittoFS's own local block-store cache. Standard client mount
tunings — `actimeo` (attribute cache), `nconnect` (parallel connections, default 1),
`rsize`/`wsize` — apply exactly as they would to a kernel NFS server.

## Known Limitations

### NFS Protocol Limitations

These limitations are fundamental constraints of the NFSv3 protocol. Many are resolved by NFSv4.

#### ETXTBSY (Text File Busy)

| Status | Reason |
|--------|--------|
| Not supported | NFS protocol limitation |

NFS servers have no way to know if any client is executing a file, so ETXTBSY cannot be enforced. This affects all NFS implementations. In practice, most package managers remove-then-replace rather than overwrite executables.

#### Timestamps (Y2106 Limitation)

| Status | Reason |
|--------|--------|
| NFSv3: Max 2106-02-07 | NFSv3 uses 32-bit unsigned seconds |
| NFSv4: No practical limit | NFSv4 uses 64-bit timestamps |

NFSv3's `nfstime3` structure uses a 32-bit unsigned integer for seconds since Unix epoch. NFSv4 resolves this with 64-bit timestamps.

#### File Locking (NFSv3)

| Status | Reason |
|--------|--------|
| NFSv3: Supported (opt-in) | NLM/NSM over TCP+UDP; needs `--udp-enabled` and a portmapper on port 111 for BSD/macOS clients |
| NFSv4: Supported | Built-in (in-protocol) locking, no setup |

NFSv3 mount and read/write work with no extra configuration. Only NLM byte-range
locking needs the UDP transport plus a portmapper reachable on port 111 — see
[Does it support file locking?](#does-it-support-file-locking) above. NFSv4 is
the simplest path: locking is in-protocol.

NFSv3 relies on the NLM (Network Lock Manager) side protocol for locking. DittoFS
implements NLM (v1/v3/v4) and NSM over TCP and UDP, but they are opt-in: enable
`--udp-enabled` and a portmapper on port 111 for BSD/macOS clients (see above).
NFSv4 has built-in locking support with no setup.

#### Extended Attributes

| Status | Reason |
|--------|--------|
| NFSv3 / v4.0 / v4.1: Not supported | No xattr operations in those protocol versions |
| NFSv4.2: Supported | RFC 8276 (`user.*` namespace, values up to 64 KiB) |

Extended attributes are not part of NFSv3, NFSv4.0, or NFSv4.1. Mount with `-o vers=4.2`
to use them via the standard Linux tools (`setfattr` / `getfattr`). Only the `user.*`
namespace is exposed, and values are stored inline up to 64 KiB (a larger value returns
`NFS4ERR_XATTR2BIG`). The xattr namespace is shared with SMB extended attributes / named
streams, so a value set over one protocol is readable over the other. See
[NFS.md → NFSv4.2 Status](nfs.md#nfsv42-status) for details.

#### fallocate/posix_fallocate

| Status | Reason |
|--------|--------|
| NFSv3: not supported | No ALLOCATE procedure in NFSv3 |
| NFSv4.2: supported (best-effort) | In-protocol ALLOCATE / DEALLOCATE (RFC 7862) |

NFSv3 has no procedure for pre-allocating disk space, so `fallocate` over a
v3 mount is unsupported (space is allocated on actual write).

Over a **NFSv4.2** mount DittoFS implements the RFC 7862 sparse-file cluster —
`ALLOCATE`, `DEALLOCATE`, `SEEK` (SEEK_HOLE/SEEK_DATA) and `READ_PLUS`:

- `fallocate <file>` (ALLOCATE) extends the file's logical size; the
  newly-covered range reads back as zeros.
- `fallocate -p <file>` (DEALLOCATE / punch hole) marks a byte range as a hole
  that reads as zeros and reclaims the backing block storage.
- `lseek(SEEK_HOLE)` / `lseek(SEEK_DATA)` report the next hole/data boundary.

**ALLOCATE is best-effort, not a physical reservation.** DittoFS is
thin-provisioned over a content-addressed/deduplicating block store (and
optionally S3), so a true up-front physical reservation is neither possible nor
meaningful. RFC 7862 permits a server to satisfy ALLOCATE without a physical
reservation: DittoFS guarantees the requested range is readable (as a sparse
hole until written) and grows the file size, but does **not** pre-reserve space
— an out-of-space condition surfaces on the eventual write, exactly as for an
ordinary sparse file.

### SMB Client Limitations

#### macOS Mount Owner-Only Access

| Status | Reason |
|--------|--------|
| Handled by dfsctl | Apple security restriction - only mount owner can access |

macOS restricts SMB mount access to the mount owner regardless of Unix permissions. When using `sudo dfsctl share mount`, it automatically uses `sudo -u $SUDO_USER` to mount as your user. See [SMB.md](smb.md) for workarounds.

### Storage Backend Limitations

#### Hard Links

All backends (Memory, BadgerDB, PostgreSQL) fully support hard links via the NFS LINK procedure.

#### Special Files

| Type | Status | Notes |
|------|--------|-------|
| Character devices | Metadata only | MKNOD creates entry, no device functionality |
| Block devices | Metadata only | MKNOD creates entry, no device functionality |
| FIFOs | Metadata only | MKNOD creates entry, no pipe functionality |
| Sockets | Metadata only | MKNOD creates entry, no socket functionality |

DittoFS can create special file entries via MKNOD, but they don't function as actual devices, pipes, or sockets.

### General Limitations

#### Single Node Only

DittoFS currently runs as a single server instance:
- No clustering or high availability
- No replication (except via S3 bucket replication)
- Single point of failure

#### Security

DittoFS is experimental and has not been security audited. See [SECURITY.md](security.md) for detailed recommendations.

### POSIX Compliance Summary

DittoFS achieves **99.99% pass rate** on [pjdfstest](https://github.com/saidsay-so/pjdfstest) POSIX compliance tests (8789 tests, 1 expected failure).

This pass rate applies to **all metadata backends** (Memory, BadgerDB, PostgreSQL).

| Metric | Value |
|--------|-------|
| Total tests | 8789 |
| Passed | 8788 |
| Failed (expected) | 1 |
| Pass rate | 99.99% |

#### Expected Failures

| Test Pattern | Reason |
|--------------|--------|
| `utimensat/09.t:test5` | NFSv3 32-bit timestamp limit (max year 2106) |
| `open::etxtbsy` | NFS protocol limitation (not testable) |
| `flock/*` | NFSv3 NLM locking is opt-in (`--udp-enabled` + portmapper on 111); use `vers=4` |
| `fcntl/lock*` | NFSv3 NLM locking is opt-in (`--udp-enabled` + portmapper on 111); use `vers=4` |
| `lockf/*` | NFSv3 NLM locking is opt-in (`--udp-enabled` + portmapper on 111); use `vers=4` |
| `xattr/*`, `*xattr/*` | Not in NFSv3 |
| `fallocate/*` | No ALLOCATE in NFSv3 (supported over NFSv4.2 — see above) |
| `chflags/*` | BSD-specific |

**Note**: Only `utimensat/09.t:test5` actually fails in current pjdfstest runs. Other patterns either don't have tests in the suite or the tests are skipped.

See `test/posix/KNOWN_FAILURES.md` for the complete list with detailed explanations.

## Still Have Questions?

- Check the other documentation in [docs/](.)
- Search [existing GitHub issues](https://github.com/marmos91/dittofs/issues)
- Open a [new issue](https://github.com/marmos91/dittofs/issues/new) for bugs or feature requests
- Review [CLAUDE.md](../../CLAUDE.md) for detailed development guidance
