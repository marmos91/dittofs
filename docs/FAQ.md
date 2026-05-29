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

DittoFS supports **NFSv3 over TCP** (28 procedures fully implemented), **NFSv4.0**, and **NFSv4.1** with features including:
- Compound operations and sessions
- File and directory delegations with CB_NOTIFY
- ACLs (Access Control Lists)
- Kerberos authentication via RPCSEC_GSS

### Does it support file locking?

NFSv3 does not include locking (NLM not implemented). However, NFSv4 provides built-in file locking support. SMB2 supports byte-range locking (shared and exclusive).

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

See [ARCHITECTURE.md](ARCHITECTURE.md) for details.

### Can I implement my own storage backend?

Absolutely! Implement either or both of these interfaces:

- **Metadata Store**: `pkg/metadata/Store` interface
- **Local Block Store**: `pkg/blockstore/local.LocalStore` interface
- **Remote Block Store**: `pkg/blockstore/remote.RemoteStore` interface

See [IMPLEMENTING_STORES.md](IMPLEMENTING_STORES.md) for implementation guidelines.

### How does performance compare to kernel NFS?

The lack of FUSE overhead and optimized Go implementation provides competitive performance for most workloads. Results show:

- Good sequential read/write performance
- Efficient handling of small files
- Low latency for metadata operations
- Scales well with concurrent connections

### Does metadata persist across server restarts?

It depends on the metadata store:

- **Memory backend** (`type: memory`): No, all data is lost on restart
- **BadgerDB backend** (`type: badger`): Yes, all metadata persists
- **PostgreSQL backend** (`type: postgres`): Yes, all metadata persists across restarts and supports distributed deployments

Configure your metadata store accordingly:

```bash
./dfsctl store metadata add --name persistent --type badger \
  --config '{"path":"/var/lib/dfs/metadata"}'
```

### Can I import an existing filesystem into DittoFS?

Not yet, but the path-based file handle strategy in BadgerDB enables this as a future feature. The
handles are deterministic based on file paths (`shareName:/path/to/file`), making filesystem scanning
and import possible.

### Is content deduplication supported?

Not currently, but the block store abstraction allows for implementing content-addressable storage
with deduplication. This could be added as a custom block store or a wrapper around existing stores.

### Does DittoFS deduplicate blocks across files?

Not yet in production. The v0.15.0 milestone is the refactor that puts
content-addressable storage (CAS) and FastCDC content-defined chunking
into place. Phase 10 ships the chunker (min=1 MiB / avg=4 MiB / max=16 MiB,
normalization level 2), BLAKE3 hashing, and the local hybrid tier
(append-only log + hash-keyed `blocks/{hh}/{hh}/{hex}` directory) behind
the experimental `use_append_log` feature flag. Phase 11 wires the remote
CAS write path and adds mark-sweep GC. Phase 13 delivers the file-level
dedup short-circuit that's expected to drive the primary VM-workload
40-80% storage reduction target.

Track progress: [#419](https://github.com/marmos91/dittofs/issues/419).

### What's an ObjectID and when does it get computed?

An `ObjectID` is a BLAKE3 Merkle root over a file's content-defined
chunk hashes, defined by Phase 13 (v0.15.0 A4):

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
(`Syncer.persistFileBlocksAfterFlush`) writes it in the same metadata
transaction that updates `FileAttr.Blocks`/`Size`/`Mtime`. Partial
flushes (some blocks still `Pending`) leave it at zero so the
short-circuit lookup never returns a half-quiesced file.

A non-zero ObjectID always reflects a fully-`Remote` consistent
state. Empty files dedup to one canonical constant
`BLAKE3("dittofs:objectid:v1\x00")`; legacy pre-Phase-13 files keep
the all-zero sentinel until Phase 14 backfills.

See
[ARCHITECTURE.md — Phase 13 File-Level Dedup](ARCHITECTURE.md#phase-13-file-level-dedup-objectid--merkle-root-v0150-a4)
for the full design.

### Why doesn't my file dedup until I close it?

DittoFS's file-level dedup short-circuit (BSCAS-05) fires at quiesce,
not on every write. Mid-write, blocks dedup at the *chunk* level
(Phase 11 `GetByHash`) — if your write produces a chunk that already
exists remotely, no PUT is issued.

But the savings of skipping every chunk's upload entirely (file-level
dedup) only kick in once the full file fingerprint exists — the
ObjectID — and that fingerprint is computed at the post-Flush
coordinator hook on `Close`/`Flush`/`fsync`, when the BlockRef list
stabilizes.

This is intentional: file-level dedup targets the workflow of cloning
a VM image or copying a large file (where the whole content arrives
in one burst). Random in-place writes inside a running VM benefit
from the chunk-level path and don't get penalized waiting for a
quiesce-only fingerprint. The trigger condition (D-09) — "all blocks
`Pending` AND no prior ObjectID" — explicitly excludes the running-VM
hot path.

### How does garbage collection work in v0.15.0?

v0.15.0 (Phase 11 / A2) replaces the previous path-prefix GC with a
fail-closed mark-sweep over the union of every live block's
`ContentHash`:

1. **Mark.** Stream every `FileBlock`'s `ContentHash` via the
   `MetadataStore.EnumerateFileBlocks(ctx, fn)` cursor across **every
   share that targets the same remote** (cross-share aggregation by
   `bucket+endpoint+prefix`, not share name). The live set is built on
   disk under `<localStore>/gc-state/<runID>/db/` (memory-bounded
   regardless of metadata size).
2. **Sweep.** A single `RemoteStore.Walk` enumerates every CAS object
   cluster-wide (the backend paginates internally). An object is kept
   iff its hash is in the live set OR its `LastModified` is newer than
   `snapshot − gc.grace_period` (default 1h). Otherwise it is deleted.

The mark phase is fail-closed: any error aborts the sweep entirely.
Sweep-side per-object DELETE failures are captured and continue;
garbage that survives a transient is reclaimed on the next run.

Triggers:

- v0.15.0 ships only on-demand GC. `gc.interval` is reserved for a
  periodic-scheduler phase — any configured value emits a startup WARN
  and is otherwise ignored today; schedule via cron until then.
- On-demand via `dfsctl store block gc <share> [--dry-run]`. Inspect the
  most recent run with `dfsctl store block gc-status <share>`.

See [ARCHITECTURE.md](ARCHITECTURE.md#garbage-collection-mark-sweep-v0150-phase-11)
and [CONFIGURATION.md](CONFIGURATION.md) for the full design and every
`gc.*` knob.

### What is the dual-read window?

Phase 11 introduces the CAS keyspace `cas/{hh}/{hh}/{hex}`, but
existing data written before v0.15.0 lives at the legacy
`{payloadID}/block-{N}` keys. Both keyspaces coexist during the
**dual-read window** (Phase 11 → Phase 14):

- Reads consult the metadata store: a `FileBlock` row with a non-zero
  `ContentHash` is read from CAS with end-to-end BLAKE3 verification
  (header pre-check on `x-amz-meta-content-hash` plus streaming
  verifier over the body).
- A `FileBlock` row with a zero `ContentHash` is read from the legacy
  key with no verification (BLAKE3 cannot be retroactively applied).

Resolution is by metadata key shape (one DB lookup per block), NOT by
S3 trial-and-error.

Phase 14 (A5) ships `dfsctl blockstore migrate`, which re-chunks all
legacy data to CAS. Phase 15 (A6) deletes the legacy code path
entirely. The dual-read code is intentionally on a deletion clock.

### Why is the cache cold after a write?

It is **not**. v0.15.0 (Phase 12 / A3) makes cache invalidation
**surgical** (CACHE-05): a write drops only the chunk-level entries
that the write actually invalidated, not the entire file. Other chunks
referenced by the file — and any chunks shared with other files via
cross-VM dedup (CACHE-02) — stay warm.

If you are seeing whole-file cold misses after a write, that is a bug.
File a report with the `dfsctl blockstore audit-refcounts <share>`
output (see below) — refcount drift between `FileBlock.RefCount` and
`FileAttr.Blocks` is the most common root cause.

The mechanism: `engine.WriteAt` returns the new `[]BlockRef`, the
caller commits it in the same metadata transaction that updates
`Mtime`/`Size`, then calls `Cache.InvalidateFile(payloadID,
removedHashes)` with **only the hashes that disappeared from the
file**. Hashes that survived (unchanged ranges) and hashes still
referenced by other files via dedup remain in the cache.

### How do I run the INV-02 audit?

INV-02 is the invariant `∑ FileBlock.RefCount == ∑ len(FileAttr.Blocks)`
— every block reference in `FileAttr.Blocks` across all files MUST be
matched by a refcount on the corresponding `FileBlock`. v0.15.0 (Phase
12 / A3) ships an operator-facing audit:

```bash
# Aggregate counts to stdout (text by default)
dfsctl blockstore audit-refcounts /archive

# Structured JSON for log aggregation / alerting
dfsctl blockstore audit-refcounts /archive --output json

# YAML if your tooling prefers it
dfsctl blockstore audit-refcounts /archive --output yaml
```

The output reports `share`, `started_at`, `duration_ms`,
`total_files`, `total_refs`, `total_refcount`, and `delta`. **A non-zero
`delta` indicates refcount drift** and SHOULD be triaged. The audit
also persists its last-run summary at
`<localStore>/audit-state/last-inv02.json` (mirrors Phase 11 GC's
`last-run.json`).

The audit is **operator-invoked**, not periodic. Schedule via cron at
the cadence that matches your operational risk tolerance until a
periodic-scheduler phase ships.

For belt-and-braces protection, the property-based fuzzer at
`pkg/metadata/storetest/inv02_fuzz_test.go` runs against all 3
built-in backends in CI on every PR, asserting INV-02 at every
quiescent point under concurrent create/delete/copy load.

See [CLI.md](CLI.md#dfsctl-blockstore-audit-refcounts-share) for the
full reference.

### What's a BlockRef?

A `BlockRef` is the 3-tuple of `(Hash ContentHash, Offset uint64, Size
uint32)` defined in `pkg/blockstore/types.go`. `FileAttr.Blocks
[]BlockRef` is the **authoritative content list** for every file in
v0.15.0 Phase 12+: which chunks compose the file, where each chunk
sits inside the file, and how big it is.

The list is:

- **Sorted by `Offset`** so the engine can binary-search it
  (`findBlocksForRange` in `pkg/blockstore/engine/range.go`).
- **Populated on every sync finalization** — the engine returns the
  new `[]BlockRef` from `WriteAt`/`Truncate`/`Delete`/`CopyPayload`
  and the caller persists it in the same metadata transaction.
- **Empty/nil for legacy files** written before v0.15.0 Phase 12;
  empty `[]BlockRef` triggers the Phase 11 dual-read shim (D-20),
  which falls back to the metadata-driven legacy resolver until the
  Phase 14 migration tool backfills the BlockRef list.

`BlockRef.Hash` is the `ContentHash` (32-byte BLAKE3) under which
the chunk is stored in the CAS keyspace `cas/{hh}/{hh}/{hex}`.
Two files referencing the same chunk via dedup share one `Hash`,
which is what makes cross-VM dedup work both for storage (CAS) and
in the cache (CACHE-02 cross-file dedup hits the same entry).

See [ARCHITECTURE.md](ARCHITECTURE.md#phase-12-engine-api--blockref--cache-v0150-a3)
for the full Phase 12 design and
[IMPLEMENTING_STORES.md](IMPLEMENTING_STORES.md#fileattrblocks-blockref-v0150-phase-12)
for storage-encoding requirements.

### How do I migrate from v0.13 / v0.14 to v0.15?

Use `dfsctl blockstore migrate --share <name>` per share. The
migration is offline (the daemon must be stopped for the share). See
[BLOCKSTORE_MIGRATION.md](BLOCKSTORE_MIGRATION.md#phase-14-v015x-a5--dfsctl-blockstore-migrate-runbook)
for the full operator runbook with worked transcripts (happy path,
TB-scale tuning, crash + auto-resume, integrity-check failure +
diagnosis).

Quick version:

1. Stop the daemon: `sudo systemctl stop dfs`
2. Migrate: `dfsctl blockstore migrate --share myshare --parallel 4`
3. Verify: `dfsctl blockstore migrate status --share myshare` shows
   `BlockLayout: cas-only`.
4. Restart: `sudo systemctl start dfs`

The migration is resumable (per-file atomic via the
`.migration-state.jsonl` journal), dry-run-able (`--dry-run` reports
upload byte estimates without writing), and bandwidth-cappable
(`--bandwidth-limit 50MB` honors SI / IEC suffixes; the limit is
aggregate across `--parallel` workers, not per-worker).

> **Known Limitation (v0.15.0):** The migration tool's production
> composition root (`openOfflineRuntime`) is not yet wired —
> end-to-end migration on a real daemon currently exits with
> `ErrOfflineRuntimeNotWired`. The full re-chunk + integrity +
> cutover pipeline is unit-tested via in-memory fixtures, and the
> per-share `block_layout` flag, the engine fail-loud routing, and
> the `dfsctl blockstore migrate status` CLI + REST surfaces all
> ship today. Track the production wire-up under
> [#425](https://github.com/marmos91/dittofs/issues/425); do not
> schedule a production migration window until it closes. See
> [BLOCKSTORE_MIGRATION.md — Known Limitation](BLOCKSTORE_MIGRATION.md#known-limitation-openofflineruntime-production-wiring)
> for the full operator-facing context.

Phase 15 (A6) is intentionally deferred until Phase 14's migration
tool has been rolled out across production workloads. Once every
production share reports `BlockLayout: cas-only`, Phase 15 deletes
the dual-read shim and every legacy `{payloadID}/block-{idx}`
code path.

### Why are residual `{payloadID}/block-{N}` keys present after upgrading to v0.15.0?

Those are legacy data written before v0.15.0. Phase 11's CAS write path
only generates `cas/{hh}/{hh}/{hex}` keys; existing `{payloadID}/block-`
objects remain in place and are read via the dual-read shim (see
above). The Phase 11 mark-sweep GC **does NOT delete legacy keys** — it
only sweeps the `cas/` prefix. Legacy objects are migrated to CAS by
the v0.15.x `dfsctl blockstore migrate` tool (Phase 14), and the
legacy code path is removed in Phase 15.

If you see residual legacy keys and want to reclaim the space before
Phase 14 ships, you can manually delete `{payloadID}/block-` objects
for files you have since deleted from DittoFS — but this is not
required for correctness, and the migration tool handles it
automatically.

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

See [NFS.md](NFS.md) and [SMB.md](SMB.md) for more details.

### Can I have multiple shares with different backends?

Yes! This is a core feature. Create stores and shares via CLI:

```bash
# Create metadata stores
./dfsctl store metadata add --name fast-memory --type memory
./dfsctl store metadata add --name persistent-db --type badger \
  --config '{"path":"/var/lib/dfs/metadata"}'

# Create block stores (local for fast access, remote for durability)
./dfsctl store block local add --name local-disk --type fs \
  --config '{"path":"/var/lib/dfs/blocks"}'
./dfsctl store block remote add --name cloud-s3 --type s3 \
  --config '{"region":"us-east-1","bucket":"my-bucket"}'

# Create shares referencing different stores
./dfsctl share create --name /temp --metadata fast-memory --local local-disk
./dfsctl share create --name /archive --metadata persistent-db \
  --local local-disk --remote cloud-s3
```

See [CONFIGURATION.md](CONFIGURATION.md) for more examples.

### Can multiple shares share the same metadata store?

Yes! Multiple shares can reference the same store instance for resource efficiency:

```bash
# Create one shared metadata store
./dfsctl store metadata add --name shared-meta --type badger \
  --config '{"path":"/var/lib/dfs/shared-metadata"}'

# Create separate remote block stores
./dfsctl store block add --kind local --name shared-local --type fs \
  --config '{"path":"/var/lib/dfs/blocks"}'
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

See [TROUBLESHOOTING.md](TROUBLESHOOTING.md) for solutions.

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
| NFSv3: Not implemented | NLM protocol not implemented |
| NFSv4: Supported | Built-in locking |

NFSv3 relies on the NLM (Network Lock Manager) protocol for locking, which is not implemented. NFSv4 has built-in locking support.

#### Extended Attributes

| Status | Reason |
|--------|--------|
| Not supported | Not in NFSv3 base specification |

Extended attributes (xattrs) are not part of NFSv3. They require NFS extensions (RFC 8276 for NFSv4.2).

#### fallocate/posix_fallocate

| Status | Reason |
|--------|--------|
| Not supported | No ALLOCATE procedure in NFSv3 |

NFSv3 has no procedure for pre-allocating disk space. Space is allocated on actual write.

### SMB Client Limitations

#### macOS Mount Owner-Only Access

| Status | Reason |
|--------|--------|
| Handled by dfsctl | Apple security restriction - only mount owner can access |

macOS restricts SMB mount access to the mount owner regardless of Unix permissions. When using `sudo dfsctl share mount`, it automatically uses `sudo -u $SUDO_USER` to mount as your user. See [SMB.md](SMB.md) for workarounds.

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

DittoFS is experimental and has not been security audited. See [SECURITY.md](SECURITY.md) for detailed recommendations.

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
| `flock/*` | NLM not implemented (NFSv3 only) |
| `fcntl/lock*` | NLM not implemented (NFSv3 only) |
| `lockf/*` | NLM not implemented (NFSv3 only) |
| `xattr/*`, `*xattr/*` | Not in NFSv3 |
| `fallocate/*` | No ALLOCATE in NFSv3 |
| `chflags/*` | BSD-specific |

**Note**: Only `utimensat/09.t:test5` actually fails in current pjdfstest runs. Other patterns either don't have tests in the suite or the tests are skipped.

See `test/posix/known_failures.txt` for the complete list with detailed explanations.

## Still Have Questions?

- Check the other documentation in [docs/](.)
- Search [existing GitHub issues](https://github.com/marmos91/dittofs/issues)
- Open a [new issue](https://github.com/marmos91/dittofs/issues/new) for bugs or feature requests
- Review [CLAUDE.md](../CLAUDE.md) for detailed development guidance
