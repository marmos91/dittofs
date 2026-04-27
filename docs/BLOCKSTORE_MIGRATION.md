# Block Store Migration Guide

This document tracks operator-facing migration concerns for the v0.15.0
block-store + core-flow refactor. Each phase that ships a schema or
keyspace change adds its own section here so operators have a single
canonical reference for upgrade order, rollback scope, and known
caveats.

## Table of Contents

- [Phase 12 (v0.15.0 A3) — `file_block_refs` table](#phase-12-v0150-a3--file_block_refs-table)
- [Phase 14 (v0.15.x A5) — `dfsctl blockstore migrate`](#phase-14-v015x-a5--dfsctl-blockstore-migrate-placeholder)

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
CAS-keys atomically per file — see the placeholder section below.

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

## Phase 14 (v0.15.x A5) — `dfsctl blockstore migrate` (placeholder)

Phase 14 will ship `dfsctl blockstore migrate` to backfill
`FileAttr.Blocks []BlockRef` and CAS-keys atomically for every legacy
file written before Phase 12. The migration tool is also responsible
for retiring the Phase 11 dual-read shim per share once a share has
been fully migrated.

> **This section is a placeholder.** The Phase 14 plan owns the full
> operator guide (invocation, dry-run, resumability, per-file failure
> handling, observability, rollback). Until then, refer to the Phase 14
> design notes in `.planning/ROADMAP.md §"Phase 14: Migration tool
> (A5)"`.

Phase 15 (A6) deletes the dual-read code paths entirely once Phase 14
has migrated all production workloads.

## Cross-references

- [ARCHITECTURE.md — Phase 12 Engine API + BlockRef + Cache](ARCHITECTURE.md#phase-12-engine-api--blockref--cache-v0150-a3)
- [ARCHITECTURE.md — Dual-Read Window](ARCHITECTURE.md#dual-read-window-phase-11--phase-14)
- [IMPLEMENTING_STORES.md — FileAttr.Blocks []BlockRef](IMPLEMENTING_STORES.md#fileattrblocks-blockref-v0150-phase-12)
- [CLI.md — `dfsctl blockstore audit-refcounts`](CLI.md#dfsctl-blockstore-audit-refcounts-share)
- [FAQ.md — What's a BlockRef?](FAQ.md#whats-a-blockref)
- [CONFIGURATION.md — Unified Cache (v0.15.0 Phase 12)](CONFIGURATION.md#unified-cache-v0150-phase-12)
