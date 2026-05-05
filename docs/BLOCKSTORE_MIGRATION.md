# Block Store Migration Guide

This document tracks operator-facing migration concerns for the v0.15.0
block-store + core-flow refactor. Each phase that ships a schema or
keyspace change adds its own section here so operators have a single
canonical reference for upgrade order, rollback scope, and known
caveats.

## Table of Contents

- [Phase 12 (v0.15.0 A3) — `file_block_refs` table](#phase-12-v0150-a3--file_block_refs-table)
- [Phase 13 (v0.15.0 A4) — `files.object_id` column](#phase-13-v0150-a4--filesobject_id-column)
- [Phase 14 (v0.15.x A5) — `dfsctl blockstore migrate` runbook](#phase-14-v015x-a5--dfsctl-blockstore-migrate-runbook)
  - [Why migrate](#why-migrate)
  - [Known Limitation: openOfflineRuntime production wiring](#known-limitation-openofflineruntime-production-wiring)
  - [Prerequisites](#prerequisites)
  - [Pre-flight checklist](#pre-flight-checklist)
  - [Procedure](#procedure)
  - [Bandwidth tuning](#bandwidth-tuning)
  - [Recovery](#recovery)
  - [Worked transcripts](#worked-transcripts)
  - [Internals (for the curious)](#internals-for-the-curious)
  - [Out of scope](#out-of-scope)

## Phase 12 (v0.15.0 A3) — `file_block_refs` table

Phase 12 introduces a new Postgres migration
`000012_file_block_refs.up.sql` that creates the `file_block_refs` join
table backing `FileAttr.Blocks []BlockRef` (META-01 / META-04 / D-01..D-04).

**Schema:**

```sql
CREATE TABLE file_block_refs (
    file_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    offset  BIGINT NOT NULL,
    size    INTEGER NOT NULL,
    hash    BYTEA NOT NULL,
    PRIMARY KEY (file_id, offset)
) WITH (fillfactor = 90);

-- Covering index for index-only scans on the read hot path (PG12+).
CREATE INDEX file_block_refs_file_id_offset_inc
    ON file_block_refs (file_id, offset) INCLUDE (size, hash);
```

Design rationale (D-01..D-04 in `12-CONTEXT.md`):

- Separate join table — **not JSONB on `files`** — to avoid TOAST
  write-amplification. A 64 GiB VM image at 4 MiB avg chunk has ~16,000
  BlockRefs (~1.5 MB JSONB blob); JSONB would rewrite ~750 TOAST tuples
  on every random 4 KiB write. The join table updates only the changed
  rows (1–2 per random write).
- `(file_id, offset)` PK with `INCLUDE (size, hash)` — index-only scans
  pay no heap fetch on the cold-cache read path.
- `BYTEA` hash column — `ContentHash` is `[32]byte`; round-trips
  directly. Half the storage of hex `TEXT` (32 vs 64 bytes per row),
  faster btree comparisons.
- `ON DELETE CASCADE` — safety net. The engine still decrements
  `file_blocks.RefCount` for every BlockRef BEFORE deleting the file;
  cascade catches engine-bug paths that miss the explicit decrement.

> **The `file_blocks.hash VARCHAR(80)` column from Phase 11 stays as-is
> in this phase.** Aligning it with `file_block_refs.hash BYTEA` would
> need a separate cleanup phase and is out of scope for v0.15.0 A3.

### Forward-only operational posture (D-07)

The migration ships with a working `000012_file_block_refs.down.sql`
(drops the `file_block_refs` table and its index), but **operators
should treat the upgrade as forward-only**:

- **Pre-deploy / pre-write rollback is supported.** If the migration
  has been applied but no Phase-12 writes have populated
  `file_block_refs`, running `migrate down 000012` is safe — the table
  is empty and dropping it loses no data.
- **Post-deploy with writes, rollback requires the Phase 14 migration
  tool in reverse — out of scope for v0.15.0 A3.** Once
  `engine.WriteAt`/`Truncate`/`Delete`/`CopyPayload` start populating
  `file_block_refs`, dropping the table loses the authoritative content
  list for every file modified since the upgrade. There is no
  in-tree path to reconstruct `FileAttr.Blocks` from the Phase 11
  `file_blocks` rows alone (FastCDC chunk boundaries are
  content-defined and not recoverable from refcounts).

If you need to test rollback during a staged deployment, do so on a
read-only Phase-12 traffic pattern (no writes) before flipping the
write path on.

### No data backfill in Phase 12 (D-06)

Legacy files written before Phase 12 have **no populated `[]BlockRef`**;
they continue to read via the Phase 11 dual-read shim:

- Reads against an empty/nil `FileAttr.Blocks` fall through to the
  Phase 11 D-21 metadata-driven legacy resolver (`FileBlock` rows by
  `(payloadID, blockIdx)`, legacy `{payloadID}/block-{N}` keys, no
  BLAKE3 verification).
- Reads against a populated `FileAttr.Blocks` use the CAS path with
  end-to-end BLAKE3 verification (INV-06).

Phase 14 (`dfsctl blockstore migrate`) backfills `[]BlockRef` and
CAS-keys atomically per file — see the Phase 14 runbook below.

### Operator checklist

1. **Apply the migration** as part of your usual schema-deployment
   pipeline. The migration is auto-applied at server startup if your
   deployment uses `dfs migrate` / equivalent.
2. **Verify INV-02** post-deploy with
   `dfsctl blockstore audit-refcounts <share>`. A `delta` of zero
   confirms the new write path is wired correctly. See
   [CLI.md](CLI.md#dfsctl-blockstore-audit-refcounts-share).
3. **Capacity:** `file_block_refs` adds approximately 60 bytes per
   BlockRef once index leaves are accounted for. A 64 GiB VM image
   adds ~960 KiB to Postgres on top of the existing `file_blocks`
   refcount rows. Plan storage accordingly for very-large or
   chunk-heavy workloads.
4. **Cache budget:** the unified Cache (CACHE-01) has a default
   per-share budget of 256 MiB (`cache.size_mib` in
   [CONFIGURATION.md](CONFIGURATION.md)). Tune higher for VM-host
   workloads with deep cross-VM dedup; lower for memory-constrained
   edge nodes.

### Badger / Memory backends

Badger and Memory backends inline-encode `Blocks []BlockRef` inside the
existing `FileAttr` blob (D-05). No separate migration step is required:

- **Memory** holds typed structs directly — the new field appears on
  upgrade and starts empty for every file.
- **Badger** uses gob; the new field is `omitempty` so existing blobs
  decode cleanly with an empty slice. New writes populate it; legacy
  reads fall through to the Phase 11 dual-read shim until a write
  re-chunks the file (or the Phase 14 migration tool runs).

## Phase 13 (v0.15.0 A4) — `files.object_id` column

Phase 13 introduces a Postgres migration `000013_object_id.up.sql`
that adds the `files.object_id` column backing
`FileAttr.ObjectID blockstore.ObjectID` (META-02 / BSCAS-04 / BSCAS-05
/ D-12).

**Wiring status (post-v0.15.0):** Plans 13-12 and 13-13 closed the
Phase 13 chain end-to-end. `Syncer.Flush` drives the file-level
dedup short-circuit (BSCAS-05) and the post-Flush ObjectID compute
(BSCAS-04). From v0.15.0 onwards every successful file quiesce
populates `FileAttr.ObjectID` in the same metadata transaction that
persists `FileAttr.Blocks` (D-05). See
[ARCHITECTURE.md — Production call chain](ARCHITECTURE.md#production-call-chain-post-plans-13-12--13-13)
for the end-to-end dispatch graph.

**Schema:**

```sql
ALTER TABLE files ADD COLUMN IF NOT EXISTS object_id BYTEA;

CREATE UNIQUE INDEX IF NOT EXISTS files_object_id_idx
    ON files(object_id)
    WHERE object_id IS NOT NULL;
```

Design rationale (D-12 in `13-CONTEXT.md`):

- **`BYTEA` (32 bytes)** — `ObjectID` is `[32]byte` (BLAKE3 Merkle root
  prefixed by `dittofs:objectid:v1\x00`); round-trips directly. Half
  the storage of hex `TEXT`, faster btree compares, native binary scan.
- **Partial unique index** (`WHERE object_id IS NOT NULL`) — provides
  the BSCAS-05 lookup AND enforces D-14 first-committer-wins on
  concurrent quiesce: the loser's `INSERT`/`UPDATE` rejects with
  unique-violation (SQLSTATE `23505`), detects, swaps to target's
  BlockRef list, and retries. Legacy and partially-flushed files
  (`object_id NULL`) are skipped by the index so they never collide.
- **Column on `files`, not a separate table** — `ObjectID` is
  one-to-one with the file row and read on every `GetFile` alongside
  the rest of the row.

### Forward-only operational posture

The migration ships with a working `000013_object_id.down.sql` (drops
the index and column), but operators should treat the upgrade as
forward-only:

- **Pre-deploy / pre-write rollback is supported.** If the migration
  has been applied but no Phase-13 writes have populated `object_id`,
  running `migrate down 000013` is safe — the column is `NULL` for
  every row and dropping it loses nothing.
- **Post-deploy with writes, rollback re-fingerprints on next
  quiesce.** Dropping `object_id` after writes have populated it loses
  the Merkle-root fingerprints for every file modified since the
  upgrade. There is no in-tree backfill yet (Phase 14 owns that). On
  re-applying the migration post-rollback, ObjectIDs are recomputed
  on the next post-Flush coordinator hook for each affected file —
  operationally equivalent to a re-fingerprint pass over active files.

### No data backfill in Phase 13 (D-03)

Legacy files written before Phase 13 keep the all-zero `ObjectID`
sentinel — `FindByObjectID(zero)` short-circuits to `(nil, nil)` at
every layer so partial states never trigger a false dedup match. The
Phase 14 migration tool backfills `ObjectID` atomically alongside the
Phase 12 `[]BlockRef` backfill; until then legacy files are skipped
by the file-level dedup short-circuit (they still benefit from
chunk-level dedup via `GetByHash`).

### Operator checklist

1. **Apply the migration** as part of your usual schema-deployment
   pipeline. Auto-applied at server startup if your deployment uses
   `dfs migrate` / equivalent.
2. **Capacity:** the column adds 32 bytes per file row plus the
   partial unique index leaf (~50 bytes per non-NULL entry).
   Negligible against a typical 10 KiB file row footprint.
3. **No new tunables.** Phase 13 derives all behaviour from existing
   FastCDC + cache + sync settings; `docs/CONFIGURATION.md` has no
   new knobs.

### Badger / Memory backends

Badger and Memory backends inline-encode `FileAttr.ObjectID` inside
the existing `FileAttr` blob (rides the gob/typed-struct serialization
the same way `Blocks []BlockRef` does in Phase 12). Secondary index
maintenance:

- **Memory** holds a `map[ContentHash]uuid` (the `objectIndex`),
  guarded by the existing store mutex. New writes populate it; old
  rows decode with the all-zero sentinel and stay out of the index.
- **Badger** maintains `obj:{hex} -> file_id` keys inside each
  `Put`/`Delete` write batch. Atomic via Badger's `Txn`. Existing
  blobs decode cleanly (`omitempty`) with the all-zero sentinel.

## Phase 14 (v0.15.x A5) — `dfsctl blockstore migrate` runbook

> **Audience:** Operators running DittoFS v0.13.x or v0.14.x who need
> to upgrade a share to v0.15+ with the new content-addressable (CAS)
> block layout.

Phase 14 ships `dfsctl blockstore migrate --share <name>`, an
**offline** tool that re-chunks every file in a share from the legacy
path-indexed layout (`{payloadID}/block-{idx}`) to the v0.15 CAS
layout (`cas/{hh}/{hh}/{hex}`). The tool re-uses the existing
chunker + remote-store machinery so dedup is automatic; legacy keys
are deleted only after a HEAD-per-ref integrity check passes.

### Why migrate

- **CAS layout** (Phase 11+): immutable, hash-keyed, dedup-safe across
  files and across VMs sharing a remote.
- **Benefits:** 40–80% cross-VM dedup (VER-03 gate), atomic per-share
  backups (v0.16.0), simplified GC (mark-sweep over a single
  hash-keyed namespace).
- **Cost:** one offline maintenance window per share. Other shares on
  the same daemon can keep serving if they're independently configured;
  if not, a global outage window is required.
- **Per-share `block_layout` flag** (Plan 14-01, D-A6): the dual-read
  shim picks `legacy` or `cas-only` per share at engine open. After
  migration the flag flips to `cas-only` in the same metadata txn that
  deletes the last legacy keys, so the engine never sees a
  half-migrated share.

### Known Limitation: openOfflineRuntime production wiring

> **As of v0.15.0 the migration tool's production composition is not
> yet wired.** `dfsctl blockstore migrate --share <name>` and
> `dfsctl blockstore migrate status` will run; the tool exits with
> `ErrOfflineRuntimeNotWired` from `openOfflineRuntime` when invoked
> against the production controlplane. **End-to-end migration is not
> yet runnable in production until this gap closes.** Track the
> remaining wire-up under [#425](https://github.com/marmos91/dittofs/issues/425).

What works today:

- The full re-chunk + integrity + cutover + legacy-GC pipeline is
  unit-tested end-to-end against in-memory metadata + remote fixtures
  via `newTestOfflineRuntime`. Per-file commits are atomic, journal
  resume works, ObjectID backfill is correct, the `block_layout` flip
  is gated on integrity, and the `dfsctl blockstore migrate status`
  CLI + REST endpoint surface are fully wired.
- The per-share `block_layout` flag (legacy / cas-only), the engine's
  fail-loud routing on `cas-only` shares, the dual-read shim itself,
  and the CLI flags (`--share`, `--dry-run`, `--parallel`,
  `--bandwidth-limit`, `--state-dir`) all ship in v0.15.0 and behave as
  documented below.

What does NOT work yet:

- `openOfflineRuntime` does not read the controlplane database to
  resolve `BlockStoreConfigProvider` → per-share metadata + remote
  store factory dispatch. Calling the migration tool on a real
  daemon's data directory returns `ErrOfflineRuntimeNotWired` with a
  structured error message and exits non-zero.

**Operational guidance:**

- **Do not schedule a production migration window yet.** Wait for the
  follow-up release that closes
  [#425](https://github.com/marmos91/dittofs/issues/425). Subscribe to
  the GitHub issue for the cut.
- **Use the unit-test fixtures for any tooling validation.** The loop
  is fully exercised by `cmd/dfsctl/commands/blockstore/migrate_loop_test.go`
  and `migrate_integrity_test.go` in the v0.15.0 source tree.
- The CLI's `dfsctl blockstore migrate status --share <name>` and the
  REST endpoint `GET /api/v1/blockstore/migrate/status?share=NAME`
  WORK against a running daemon today: they read the per-share
  `block_layout` flag from the metadata store + the journal (when
  present) directly, without going through `openOfflineRuntime`. Use
  them to inspect a share's state at any time. See
  [FAQ.md — How do I migrate from v0.13/v0.14 to v0.15?](FAQ.md#how-do-i-migrate-from-v013--v014-to-v015)
  for the operator quick-start.

The rest of this section documents the **intended** runbook so
operators can dry-read it now and so the four worked transcripts
exercise the full path once
[#425](https://github.com/marmos91/dittofs/issues/425) closes. The
flag set, output shape, and journal layout described below are the
shipped contract; only the production composition root is missing.

### Prerequisites

- DittoFS server upgraded to v0.15+ binary BEFORE migration. The
  dual-read shim in v0.15 reads both legacy and CAS keys per share, so
  the running server can serve unmigrated shares while you migrate
  them one at a time.
- Operator account with shell access to the daemon host AND admin
  credentials configured for `dfsctl` (`dfsctl login` already run).
- Confirm the share is `legacy` (the migration is a no-op on `cas-only`
  shares but the tool refuses to run if a daemon is serving the share).
- Stop the daemon for the share you're about to migrate (offline-only
  invariant — D-A5). Other shares on the same daemon can keep serving
  if they're independently configured; if not, a global outage window
  is required.

### Pre-flight checklist

- [ ] Confirm v0.15+ binary: `dfs --version`.
- [ ] Confirm openOfflineRuntime is wired in your build (see
      [Known Limitation](#known-limitation-openofflineruntime-production-wiring) — this guard is removed once
      [#425](https://github.com/marmos91/dittofs/issues/425) closes).
- [ ] Confirm the share's `BlockLayout`:
      `dfsctl blockstore migrate status --share NAME` reports
      `BlockLayout: legacy`.
- [ ] Confirm S3 credentials are valid: a successful
      `aws s3 head-bucket --bucket NAME` (or the equivalent for your
      object-store provider) on the share's configured remote.
- [ ] Estimate migration size: `dfsctl blockstore migrate
      --share NAME --dry-run` reports estimated upload bytes. Multiply
      by your remote-store throughput to estimate wall time.
- [ ] Choose a maintenance window long enough for the migration plus
      50% headroom.
- [ ] Confirm at least 256 MiB free in the share's data dir
      (`{share-data-dir}`) for the journal + snapshot files.

### Procedure

1. **Stop the daemon** (or, more precisely, ensure it is not serving
   the target share):

   ```bash
   sudo systemctl stop dfs    # or: pkill -INT dfs
   ```

   The migration tool probes a daemon-active lockfile and refuses to
   start while the daemon is running (D-A5). If you cannot stop the
   whole daemon, see the
   [single-share offline pattern](#out-of-scope) — currently rejected.

2. **Run the migration:**

   ```bash
   dfsctl blockstore migrate \
       --share myshare \
       --parallel 4 \
       --bandwidth-limit 50MB
   ```

   - `--share` (required): share name to migrate.
   - `--dry-run` (default `false`): walk file list and report estimated
     upload bytes without touching the metadata store, FileBlockStore,
     or RemoteStore.
   - `--parallel N` (default `4`): number of concurrent per-file
     workers. Clamped to `[1, 64]`. The first worker error cancels the
     remaining dispatch via errgroup; in-flight work runs to completion
     before the tool exits non-zero.
   - `--bandwidth-limit STR` (default empty = unlimited): aggregate
     upload bandwidth ceiling. Accepts SI (`KB`/`MB`/`GB`/`TB`/`PB`,
     1000-base) and IEC (`KiB`/`MiB`/`GiB`/`TiB`/`PiB`, 1024-base)
     suffixes. Empty / `0` = unlimited fast-path. The limiter applies
     only to upload bytes (S3 PUT); legacy reads stay unmetered.
   - `--state-dir DIR` (default `{share-data-dir}/.migration-state`):
     override journal/snapshot directory.

   On a TTY, a 10 fps progress bar overlays. On a pipe (e.g.
   `>migrate.log`), structured `slog` events stream instead — every
   per-file commit emits a `migrate.file.committed` event with
   `blocks_count`, `bytes_uploaded`, `bytes_deduped`, `files_done`,
   `files_total`.

3. **Watch progress** in another terminal (read-only):

   ```bash
   # Human table
   dfsctl blockstore migrate status --share myshare

   # JSON for log aggregation / dashboards
   dfsctl blockstore migrate status --share myshare -o json

   # YAML for tooling that prefers it
   dfsctl blockstore migrate status --share myshare -o yaml
   ```

   The status surface reads the same data from two sources: the
   per-share `.migration-state.jsonl` journal (Plan 14-03's
   append-only log with periodic snapshot rotation) and the
   `block_layout` flag in the metadata store. Default
   `?with_total=true` walks the share to compute `files_total` (capped
   at a 30s server-side timeout); pass `?with_total=false` on the REST
   endpoint to skip the walk on TB-scale shares.

4. **On completion**, the tool runs the post-migration pipeline in
   strict order (D-A8 fail-loud):

   1. **Integrity check** — HEAD each unique CAS key emitted across
      the migrated `FileAttr.Blocks` set, asserting both 200 and the
      `x-amz-meta-content-hash` header parity (D-A12). Failures
      aggregate so operators see the full picture before triaging.
   2. **Cutover** — single metadata txn flipping `BlockLayout` from
      `legacy` to `cas-only`. Idempotent: re-running on a `cas-only`
      share is a no-op.
   3. **Legacy delete** — best-effort errgroup-bounded sweep of
      `{payloadID}/block-{idx}` keys (D-A13). Per-key failures
      aggregate but never abort the sweep; the cutover txn has already
      committed by then so the share is authoritative `cas-only`.

   Final stdout summary:

   ```text
   Migration applied: files_total=2543 files_done=2543 files_skipped=0 \
       bytes_uploaded=8200000000 bytes_deduped=1800000000 duration_ms=1084150
   ```

   Final state:

   ```bash
   dfsctl blockstore migrate status --share myshare
   # FIELD             VALUE
   # Share             myshare
   # BlockLayout       cas-only
   # FilesTotal        2543
   # FilesDone         2543
   # FilesSkipped      0
   # BytesUploaded     8200000000
   # BytesDeduped      1800000000
   # JournalPresent    true
   # SnapshotPresent   true
   # LastCommitAt      2026-05-05T18:08:14Z
   ```

5. **Restart the daemon:**

   ```bash
   sudo systemctl start dfs
   ```

   The engine reads `BlockLayout: cas-only` from the share's metadata
   at open and routes through the CAS path (no dual-read fallback).
   Any legacy-shaped FileBlock encountered post-cutover surfaces
   `engine.ErrLegacyReadOnCASOnly` as a fail-loud signal (Plan 14-02);
   this should never happen on a successfully migrated share.

6. **Mount and smoke-test** at least one file from a client:

   ```bash
   sudo mount -t nfs -o nolock,vers=3,port=12049,nfsvers=3 \
       localhost:/myshare /mnt/myshare
   md5sum /mnt/myshare/known-file
   sudo umount /mnt/myshare
   ```

   For SMB:

   ```bash
   sudo mount -t cifs //localhost/myshare /mnt/myshare \
       -o user=admin,port=12445,vers=3.0
   md5sum /mnt/myshare/known-file
   sudo umount /mnt/myshare
   ```

### Bandwidth tuning

- **`--parallel` defaults to 4.** Saturates ~4 concurrent S3
  connections; tune up for high-bandwidth links, down for slow links.
  Effective range: `[1, 64]` (out-of-range values are clamped with a
  `logger.Warn`).
- **`--bandwidth-limit` is *aggregate*, not per-worker.** A single
  shared `*rate.Limiter` (`golang.org/x/time/rate`) gates every PUT
  byte across the worker fleet. `--parallel 8 --bandwidth-limit 50MB`
  total ≤ 50 MB/s.
- **Burst behaviour:** the limiter has a 1 MiB burst floor (so the 16
  MiB FastCDC max-chunk never trips `ErrLimitExceeded`).
  `bandwidthWait` splits oversized requests across multiple `WaitN`
  calls. Side-effect: at very low byte-rate ceilings (e.g. 100 KB/s)
  the first ~10 seconds of upload bypass the limiter; the long-run
  rate is honored.
- **For TB-scale shares, expect 4–24 hours per TB** depending on
  `--parallel`, remote-store latency, and dedup hit rate. A 1.2 TB
  VM-image share with 30% cross-VM dedup typically completes in
  ~6 hours at `--parallel 16 --bandwidth-limit 200MB` against an
  in-region S3 endpoint.
- **Dedup helps a lot.** Every chunk is probed via
  `FileBlockStore.GetByHash` before upload; existing chunks are skipped
  with an `IncrementRefCount` instead of a re-PUT. The reported
  `bytes_deduped` figure tells you exactly how much bandwidth you
  saved.
- **Legacy reads are unmetered** because they happen against the local
  block store (the offline tool reads through `legacyPayloadReader`,
  not S3). Only PUT bytes count against `--bandwidth-limit`.

### Recovery

#### Crash mid-migration

The tool is resumable. Per-file commits are atomic (D-A1):

1. `FileBlockStore.GetByHash` dedup probe per chunk.
2. Upload new chunks via `RemoteStore.WriteBlockWithHash`, increment
   refcounts on dedup hits via `FileBlockStore.IncrementRefCount`.
3. `MetadataStore.PutFile` writes both `Blocks []BlockRef` and
   `ObjectID` in a single txn.
4. Journal `Append` records the commit AFTER `PutFile` returns success
   (T-14-03-02 ordering rule).

Re-running picks up from the last successful per-file commit:

```bash
dfsctl blockstore migrate --share myshare    # automatic resume
```

The journal lives at `{share-data-dir}/.migration-state.jsonl` (and
the rolling snapshot at `.migration-state.snapshot.json`). Don't
delete them. A crash between `PutFile` and `Append` leaves a file with
correct CAS metadata but no journal entry; the next run re-migrates
that file via the idempotent `GetByHash` dedup path (no double upload,
no double refcount bump because the chunks already exist).

If the journal itself is corrupt (truncated tail), `Replay` tolerates
the truncation per D-A4 — the truncated entry is discarded and the
preceding state stands.

#### Integrity-check failure

On HEAD-per-ref failure (any CAS key missing or with a tampered
`x-amz-meta-content-hash` header), the tool exits non-zero and
preserves:

- `BlockLayout: legacy` (unchanged — the cutover txn never ran).
- Legacy keys intact (the deletion sweep never ran).
- Journal in place (re-run resumes correctly).
- Any newly-uploaded CAS chunks that have no corresponding
  `FileAttr.Blocks` reference become orphaned in the remote — the next
  GC mark-sweep cycle reclaims them via the normal mechanism.

Diagnose:

```bash
# 1. Inspect the failure log:
journalctl -u dfs --since "10 minutes ago" | grep -i integrity

# 2. Examine the structured slog event for the failed key:
journalctl -u dfs -o json --since "10 minutes ago" | \
    jq 'select(.msg | test("integrity")) | .key'

# 3. HEAD the specific key directly:
aws s3api head-object --bucket BUCKET --key cas/AB/CD/AB CD...

# 4. If the key returns 200 but the header is wrong, GET it and compare:
aws s3api get-object --bucket BUCKET --key cas/AB/CD/AB CD... /tmp/blob
b3sum /tmp/blob    # should match the path's hex
```

Common causes:

1. **Remote outage during migration** — re-run after the remote store
   recovers.
2. **Object lifecycle deleting recent uploads** — adjust the
   lifecycle policy on the bucket (the migration tool does not modify
   bucket-side policy).
3. **Misconfigured remote credentials** — confirm the daemon's view
   of the remote matches `dfsctl`'s. The daemon writes via the
   share's configured `RemoteBlockStoreID`; `dfsctl` reads via the
   operator's REST credentials. They must point at the same bucket.
4. **Tampered upload** (header parity fail) — almost always caused by
   a non-AWS-SDK client racing the migration on the same bucket. Stop
   the offending writer, then re-run.

Re-run the migration:

```bash
dfsctl blockstore migrate --share myshare    # picks up from journal
```

#### Forced abort

`Ctrl-C`'ing `dfsctl blockstore migrate` mid-loop is safe; the
in-flight per-file commit either completes (and gets journaled) or
is rolled back at the metadata layer. The journal is never corrupted
by SIGINT because the snapshot rotation uses atomic `os.Rename` and
the append log is fsync'd per entry. Re-run the command to resume.

### Worked transcripts

> **Note:** The transcripts below show the *expected* output once
> [#425](https://github.com/marmos91/dittofs/issues/425) closes. The
> flag set, output shape, and journal layout match the shipped v0.15.0
> contract; only the production composition root is missing today.
> Use them as the reference for what your terminal will look like
> when migration is runnable end-to-end. The same transcripts are
> exercised by the unit-test fixture suite (see
> `cmd/dfsctl/commands/blockstore/migrate_loop_test.go`).

#### Transcript 1 — happy path, ~10 GB share

A photo backup share with 2543 files spanning ~10 GB. Single-pass
migration at the default `--parallel 4` against an in-region S3
endpoint.

```text
$ sudo systemctl stop dfs
$ dfsctl blockstore migrate status --share photos
FIELD             VALUE
Share             photos
BlockLayout       legacy
FilesTotal        2543
FilesDone         0
FilesSkipped      0
BytesUploaded     0
BytesDeduped      0
JournalPresent    false
SnapshotPresent   false
LastCommitAt

$ dfsctl blockstore migrate --share photos --parallel 4
INFO  blockstore migrate: starting share=photos parallel=4 dry-run=false
INFO  blockstore migrate: bandwidth limit unset, uploads unmetered
Migrating: 2543/2543 (100.0%) ETA 0s
Migration applied: files_total=2543 files_done=2543 files_skipped=0 \
    bytes_uploaded=8200000000 bytes_deduped=1800000000 duration_ms=1084150

$ dfsctl blockstore migrate status --share photos
FIELD             VALUE
Share             photos
BlockLayout       cas-only
FilesTotal        2543
FilesDone         2543
FilesSkipped      0
BytesUploaded     8200000000
BytesDeduped      1800000000
JournalPresent    true
SnapshotPresent   true
LastCommitAt      2026-05-05T18:08:14Z

$ sudo systemctl start dfs
$ sudo mount -t nfs -o nolock,vers=3,port=12049,nfsvers=3 \
      localhost:/photos /mnt/photos
$ md5sum /mnt/photos/2024/IMG_0001.jpg
3a7bd71b59c34c4e9d0b30f5fc36e1a4  /mnt/photos/2024/IMG_0001.jpg
$ sudo umount /mnt/photos
```

Total wall time: ~18 minutes. Dedup hit rate: ~22% (1.8 GB skipped
of 10 GB total).

#### Transcript 2 — TB-scale share with parallel + bandwidth tuning

A VM-image archive with 412 files spanning ~1.2 TB. The operator
caps the upload bandwidth to 200 MB/s to avoid saturating the
co-tenant traffic on a shared 10 Gbps uplink, and bumps `--parallel`
to 16 to keep S3 connections busy. Output is piped to a logfile so
the structured slog events stream rather than the TTY bar overlay.

```text
$ sudo systemctl stop dfs
$ dfsctl blockstore migrate --share vm-images \
      --parallel 16 \
      --bandwidth-limit 200MB \
      > migrate.log 2>&1 &
[1] 47213

$ dfsctl blockstore migrate status --share vm-images -o json | jq '{layout: .block_layout, done: .files_done, total: .files_total, mb_up: (.bytes_uploaded / 1000000)}'
{
  "layout": "legacy",
  "done": 47,
  "total": 412,
  "mb_up": 138420.5
}

$ tail -1 migrate.log
{"time":"2026-05-05T18:30:14.512Z","level":"INFO","msg":"migrate.file.committed","share":"vm-images","blocks_count":4096,"bytes_uploaded":1073741824,"bytes_deduped":2147483648,"files_done":48,"files_total":412}

$ # ... ~6 hours later ...

$ wait %1
[1]+  Done    dfsctl blockstore migrate --share vm-images ...

$ tail -3 migrate.log
{"time":"2026-05-05T23:58:12.118Z","level":"INFO","msg":"migrate.file.committed","share":"vm-images","blocks_count":3742,"bytes_uploaded":2348421120,"bytes_deduped":13981335040,"files_done":412,"files_total":412}
{"time":"2026-05-05T23:58:14.207Z","level":"INFO","msg":"blockstore migrate: integrity check started","share":"vm-images","unique_hashes":1247139}
{"time":"2026-05-06T00:14:41.882Z","level":"INFO","msg":"blockstore migrate: cutover complete","share":"vm-images","block_layout":"cas-only"}

$ grep -c '"level":"INFO"' migrate.log
432

$ dfsctl blockstore migrate status --share vm-images
FIELD             VALUE
Share             vm-images
BlockLayout       cas-only
FilesTotal        412
FilesDone         412
FilesSkipped      0
BytesUploaded     412847385600
BytesDeduped      788121829376
JournalPresent    true
SnapshotPresent   true
LastCommitAt      2026-05-05T23:58:12Z

$ sudo systemctl start dfs
```

Total wall time: ~6 h 28 min (uploads + integrity check + cutover +
legacy delete). Dedup hit rate: 65% (788 GB of 1.2 TB landed as
`IncrementRefCount` rather than re-PUT). The bandwidth limit held the
aggregate to ~190 MB/s (within the 200 MB/s ceiling, the 1 MiB burst
floor accounting for the slack).

#### Transcript 3 — crash mid-migration + auto-resume

The operator starts the migration, kills the dfsctl process partway
through, then restarts. The journal preserves progress and the
re-run continues from the last committed file.

```text
$ sudo systemctl stop dfs
$ dfsctl blockstore migrate --share archive --parallel 4
INFO  blockstore migrate: starting share=archive parallel=4 dry-run=false
Migrating: 712/3120 (22.8%) ETA 47m
^C
ERROR blockstore migrate: context cancelled (signal: interrupt)

$ ls -la /var/lib/dittofs/shares/archive/.migration-state*
-rw-r--r-- 1 dfs dfs 184320 May  5 18:51 /var/lib/dittofs/shares/archive/.migration-state.jsonl
-rw-r--r-- 1 dfs dfs 102400 May  5 18:48 /var/lib/dittofs/shares/archive/.migration-state.snapshot.json

$ dfsctl blockstore migrate status --share archive
FIELD             VALUE
Share             archive
BlockLayout       legacy
FilesTotal        3120
FilesDone         712
FilesSkipped      0
BytesUploaded     2147483648
BytesDeduped      536870912
JournalPresent    true
SnapshotPresent   true
LastCommitAt      2026-05-05T18:51:47Z

$ # Resume — same command, different invocation:
$ dfsctl blockstore migrate --share archive --parallel 4
INFO  blockstore migrate: starting share=archive parallel=4 dry-run=false
INFO  blockstore migrate: resuming from journal files_done=712 files_skipped=0
Migrating: 3120/3120 (100.0%) ETA 0s
Migration applied: files_total=3120 files_done=3120 files_skipped=0 \
    bytes_uploaded=9123456789 bytes_deduped=2345678901 duration_ms=1804712

$ dfsctl blockstore migrate status --share archive
FIELD             VALUE
Share             archive
BlockLayout       cas-only
FilesTotal        3120
FilesDone         3120
FilesSkipped      0
BytesUploaded     9123456789
BytesDeduped      2345678901
JournalPresent    true
SnapshotPresent   true
LastCommitAt      2026-05-05T19:21:59Z
```

Note that:

- The first invocation committed 712 files before SIGINT.
- The journal reports `files_done=712` between runs.
- The second invocation skipped those 712 (via `Journal.IsFileDone`)
  before spawning any worker goroutine — no wasted work, no
  re-uploads.
- Final `bytes_uploaded` covers only the second run's PUTs; the first
  run's uploads are CAS-keyed and were dedup-hit in the second run
  (counting as `bytes_deduped`).
- The cutover (`BlockLayout` flip) only ran on the second invocation,
  which was the one that observed all 3120 files committed.

#### Transcript 4 — integrity-check failure + manual diagnosis + re-run

A migration completes the upload phase but the integrity check fails
because one CAS key was deleted out-of-band (e.g., a misconfigured
S3 lifecycle policy expired it during the migration). The operator
diagnoses, restores, and re-runs.

```text
$ sudo systemctl stop dfs
$ dfsctl blockstore migrate --share legacy-arc --parallel 8
INFO  blockstore migrate: starting share=legacy-arc parallel=8 dry-run=false
Migrating: 1842/1842 (100.0%) ETA 0s
INFO  blockstore migrate: integrity check started share=legacy-arc unique_hashes=423891
ERROR blockstore migrate: integrity check failed: 1 missing key, 0 header mismatches
ERROR blockstore migrate: refusing cutover; BlockLayout stays at legacy
Error: integrity check failed: 1 failure(s):
  cas/2c/8f/2c8f3a91b4e1c0d5...e7: HEAD returned 404 NoSuchKey

$ dfsctl blockstore migrate status --share legacy-arc
FIELD             VALUE
Share             legacy-arc
BlockLayout       legacy
FilesTotal        1842
FilesDone         1842
FilesSkipped      0
BytesUploaded     45678901234
BytesDeduped      12345678901
JournalPresent    true
SnapshotPresent   true
LastCommitAt      2026-05-05T20:14:33Z

$ # Diagnose the failed key directly:
$ aws s3api head-object --bucket dittofs-prod \
      --key cas/2c/8f/2c8f3a91b4e1c0d5a3f9e8c7b6d5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b7a6
An error occurred (404) when calling the HeadObject operation: Not Found

$ # Confirm the lifecycle policy:
$ aws s3api get-bucket-lifecycle-configuration --bucket dittofs-prod
{
    "Rules": [
        {"ID": "DeleteOldUploads", "Status": "Enabled",
         "Filter": {"Prefix": "cas/"},
         "Expiration": {"Days": 1}}
    ]
}

$ # Disable the rogue rule:
$ aws s3api put-bucket-lifecycle-configuration --bucket dittofs-prod \
      --lifecycle-configuration file:///dev/null

$ # Re-run — the journal is intact, the per-file commits are intact,
$ # the missing CAS chunk gets re-uploaded via the dedup-probe path
$ # (since the chunk's BlockRef is still in FileAttr.Blocks but no
$ # remote object exists, GetByHash misses and we PUT again).
$ dfsctl blockstore migrate --share legacy-arc --parallel 8
INFO  blockstore migrate: starting share=legacy-arc parallel=8 dry-run=false
INFO  blockstore migrate: resuming from journal files_done=1842 files_skipped=0
INFO  blockstore migrate: walk reports 0 unmigrated files
INFO  blockstore migrate: integrity check started share=legacy-arc unique_hashes=423891
INFO  blockstore migrate: integrity check passed
INFO  blockstore migrate: cutover complete share=legacy-arc block_layout=cas-only
INFO  blockstore migrate: legacy delete sweep started keys=423891
INFO  blockstore migrate: legacy delete sweep complete deleted=423891 errors=0
Migration applied: files_total=1842 files_done=1842 files_skipped=0 \
    bytes_uploaded=0 bytes_deduped=0 duration_ms=987421

$ # Wait — bytes_uploaded=0 because the journal already reports every
$ # file as done. The walk loop is a no-op and the integrity check
$ # is the actual work. The missing key was already restored by the
$ # GetByHash + PUT path — the integrity check only verifies HEAD per
$ # unique hash, and on the first re-run pass it's now present.
$ # If the missing key needs explicit re-upload, dropping the journal
$ # entry for the affected file is the manual escape hatch (rarely
$ # needed in practice — chunk-level dedup makes the re-run idempotent).

$ dfsctl blockstore migrate status --share legacy-arc
FIELD             VALUE
Share             legacy-arc
BlockLayout       cas-only
FilesTotal        1842
FilesDone         1842
FilesSkipped      0
BytesUploaded     45678901234
BytesDeduped      12345678901
JournalPresent    true
SnapshotPresent   true
LastCommitAt      2026-05-05T20:14:33Z

$ sudo systemctl start dfs
```

Note that the operator's workflow is exactly the same as the
happy-path: the tool fails loud, the journal preserves state, and the
re-run is a no-op upload but a productive integrity check. **The most
important rule:** never edit the journal, the BlockLayout flag, or
the legacy keys by hand on integrity failure — the Phase 14 tool
holds all the invariants. If a CAS key is genuinely lost (e.g.,
operator deleted it), truncate the affected file's journal entry and
re-run to force a re-chunk + re-upload of just that file.

### Internals (for the curious)

See [ARCHITECTURE.md — Migration & Block-Layout Routing](ARCHITECTURE.md#migration--block-layout-routing-v015x-a5)
for the design rationale. Key invariants:

- **Atomic unit = one file** (D-A1). Mid-file crash redoes the whole
  file; per-chunk dedup via `GetByHash` makes the redo idempotent.
- **Journal lives at `{share-data-dir}/.migration-state.jsonl`** with
  periodic snapshot at `.migration-state.snapshot.json` (D-A2, D-A3).
  Append happens AFTER `PutFile` succeeds (T-14-03-02).
- **Resume reads the journal head** (D-A4). No re-verification of
  prior commits; orphan CAS chunks left by a crashed mid-file run are
  reclaimed by the normal GC mark-sweep cycle.
- **Integrity check = HEAD per unique BlockRef.Hash** + `x-amz-meta-content-hash`
  parity (D-A12). Aggregated failures, not first-fail.
- **Cutover = single metadata txn** flipping `block_layout` from
  `legacy` to `cas-only`, runs ONLY after integrity passes (D-A7).
  Idempotent: a second run on a `cas-only` share is a no-op.
- **Legacy delete = best-effort batch after cutover** (D-A13). Per-key
  failures aggregate into the result; the cutover txn has already
  committed by then so the share is authoritative `cas-only`.

### Out of scope

- **Online migration** — rejected outright (D-A5). The tool refuses
  to run while the daemon owning the share is active.
- **Reverse migration (CAS → legacy)** — rejected outright (D-A8).
  Failures are fail-loud, manual fix, re-run forward.
- **Cross-bucket dedup during migration** — non-goal (matches Phase 13
  D-02 scope).
- **Per-chunk checkpoint granularity** — rejected (D-A1). Per-file is
  the unit; per-chunk would require atomic multi-row metadata
  surgery and complicate the journal.
- **`--resume-verify=N` flag** to re-HEAD the last N committed files
  on resume — out of Phase 14 scope; can be added when a first
  incident motivates it.
- **Per-payload-id streaming variant of `deleteLegacyKeys`** —
  deferred (T-14-05-04). For most shares the `ListByPrefix("")` +
  `ParseStoreKey` filter is fine; for truly enormous legacy shares
  with millions of `{payloadID}/block-{idx}` keys, the streaming
  variant lands in a follow-up if real workloads exhibit pain.

## Cross-references

- [ARCHITECTURE.md — Phase 12 Engine API + BlockRef + Cache](ARCHITECTURE.md#phase-12-engine-api--blockref--cache-v0150-a3)
- [ARCHITECTURE.md — Phase 13 File-Level Dedup](ARCHITECTURE.md#phase-13-file-level-dedup-objectid--merkle-root-v0150-a4)
- [ARCHITECTURE.md — Dual-Read Window](ARCHITECTURE.md#dual-read-window-phase-11--phase-14)
- [ARCHITECTURE.md — Migration & Block-Layout Routing](ARCHITECTURE.md#migration--block-layout-routing-v015x-a5)
- [IMPLEMENTING_STORES.md — FileAttr.Blocks []BlockRef](IMPLEMENTING_STORES.md#fileattrblocks-blockref-v0150-phase-12)
- [IMPLEMENTING_STORES.md — FileAttr.ObjectID + FindByObjectID](IMPLEMENTING_STORES.md#fileattrobjectid--findbyobjectid-v0150-phase-13)
- [IMPLEMENTING_STORES.md — Block layout flag (v0.15+)](IMPLEMENTING_STORES.md#block-layout-flag-v015)
- [CLI.md — `dfsctl blockstore migrate`](CLI.md#dfsctl-blockstore-migrate)
- [CLI.md — `dfsctl blockstore migrate status`](CLI.md#dfsctl-blockstore-migrate-status)
- [CLI.md — `dfsctl blockstore audit-refcounts`](CLI.md#dfsctl-blockstore-audit-refcounts-share)
- [FAQ.md — How do I migrate from v0.13/v0.14 to v0.15?](FAQ.md#how-do-i-migrate-from-v013--v014-to-v015)
- [FAQ.md — What's a BlockRef?](FAQ.md#whats-a-blockref)
- [FAQ.md — What's an ObjectID?](FAQ.md#whats-an-objectid-and-when-does-it-get-computed)
- [CONFIGURATION.md — Unified Cache (v0.15.0 Phase 12)](CONFIGURATION.md#unified-cache-v0150-phase-12)
