# Blocks-Only Storage — Implementation Roadmap (#1493)

**Branch:** `1493-blocks-only-storage` (off `develop`).
**Design:** `.planning/2026-06-30-blocks-only-storage-design.md` (approved).
**Split:** 4 PRs. Each keeps `develop` green and is independently reviewable.

Locked decisions (2026-06-30):
- **PR count:** 4 (folded from the 6-component design).
- **Local index** (`hash → {logBlobID, rawOffset, rawLength}`): lives **in the metadata
  store**, alongside the existing `FileBlock` rows + `SyncedHashStore` — not a separate
  local KV. Raw log blobs carry no per-chunk framing, so the index must be persisted; the
  metadata store is the one durable place already in the share's transaction domain.
- **Migration:** automatic, **blocking at startup**, resumable/idempotent.
- **PR1 backends:** S3 + memory + filesystem-remote all implement the new interface.
- **Refactor in place, no additive parallel types.** Existing functions/structs/interfaces
  are reshaped to the new model and renamed — we do NOT leave the old CAS/additive surface
  beside the new one. Each PR refactors the surface in its blast radius; PR4 does the final
  sweep. (Per minimize-interface-surface + no-compat-shims.)
- **Docs updated per PR.** Every PR updates the docs it touches (`docs/internals/
  implementing-stores.md`, `architecture.md`, `docs/guide/*`); PR4 does the final docs pass.

## Naming model (fix everywhere)

The current names collide: `FileBlock` / `BlockRef` / `BlockState` / `ObjectID` actually
describe **chunks**, while the new model reserves **Block** for the packed remote container.
Normalize so "Block" means only the container:

| Current (means chunk) | New | Notes |
|---|---|---|
| `FileBlock` (row `{payloadID}/{offset}`) | `FileChunk` / chunk row | per-chunk metadata row |
| `BlockRef{Hash,Offset,Size}` | `ChunkRef` | chunk locator within a file |
| `BlockState` (Pending/Syncing/Remote) | `ChunkSyncState` | per-chunk sync state |
| `RemoteStore` (CAS, hash-keyed) | `RemoteBlockStore` (BlockID-keyed) | slim block interface |
| `ChunkReader.ReadChunk` (additive) | folded into `GetBlockRange` + decode | remove additive surface |
| `ChunkLocator{Offset,Length}` | `ChunkLocator{WireOffset,WireLength}` | drop `IsStandalone`/standalone |
| `ObjectID` (Merkle root) | reassess — keep only if still load-bearing | |

New names with no collision today: **Block** (packed container, `BlockID`), **block record**
(`BlockID → {blockHash, length, liveChunkCount, syncState}`), **log blob** (`logBlobID`),
**local index** (`hash → {logBlobID, rawOffset, rawLength}`).

---

## PR1 — Foundation: remote block interface + codec + backends

Pure new code. No wiring into the live path. Fully unit-tested.

- Slim `RemoteBlockStore` interface: `PutBlock(ctx, blockID, r)` (only writer),
  `GetBlock(ctx, blockID)`, `GetBlockRange(ctx, blockID, off, len)`,
  `DeleteBlock(ctx, blockID)`, `Walk(ctx, blocks/…)`.
- Block codec: streaming build `[preamble][record{hash,length} + enc(comp(chunk))]…`;
  range-resolve from a locator; sequential record parse for DR. Per-chunk transform reuses
  the existing compression/encryption decorators. Record sub-headers AEAD-framed on
  encrypted shares; plaintext otherwise.
- Backends implementing the interface: **S3, memory, filesystem-remote**.
- Conformance: new `blockstoretest` block-interface suite + codec round-trip
  (encrypted/plaintext, incompressible input, range reads, blake3 verify).

- Refactor/naming in this PR's blast radius: rename `RemoteStore`→`RemoteBlockStore`,
  reshape `ChunkLocator` fields to `WireOffset`/`WireLength`, begin folding the additive
  `ChunkReader.ReadChunk` into `GetBlockRange`+decode. Update `docs/internals/
  implementing-stores.md` for the new block-store contract.

DoD: interface + codec + 3 backends + conformance green; touched surface renamed; docs current.

## PR2 — Persistence layer: local log-blob tier + metadata block records

The durable substrate. New code; old standalone path still the default writer.

- Local tier (`pkg/block/local/fs`): replace per-chunk CAS files with RocksDB-style
  **log blobs** — append-at-tail (`pwrite`), concurrent `pread` carve, ~1 GB rotation,
  whole-blob eviction, torn-tail recovery (truncate to last validated chunk).
- **Local index in metadata:** `hash → {logBlobID, rawOffset, rawLength}` rows.
- Metadata locator: `SyncedHashStore` → `hash → {BlockID, wireOffset, wireLength}`.
- Metadata **block record:** `BlockID → {blockHash, length, liveChunkCount, syncState}`.
- **Single-transaction per-block commit** of all of a block's chunk locators + its block
  record (delivers #1416 — one fsync per block, not per file).
- Conformance: `storetest` (block-record + locator + batched commit) and
  `blockstoretest` append/log-blob behavior.

DoD: log-blob local tier + metadata schema + atomic per-block commit, conformance green.

## PR3 — Sync engine + read path (the behavioral flip)

New shares now write blocks. Legacy CAS reader is kept, quarantined, for un-migrated data
(removed in PR4).

- Sync engine: real-time **carve** (block-size ~16 MiB or 5s idle), `pread` the carved
  region, stream transform+frame into `PutBlock`, then the atomic locator+block-record
  write. Crash after `PutBlock` before the locator write → chunks stay unsynced, re-carved
  into a fresh block (idempotent on `blockID`).
- Read path: local-index hit → raw blob, zero decode; miss → `GetBlockRange` via locator →
  decode → `blake3(plaintext)==hash` verify (fail-closed).
- **GC (delete-only):** `liveChunkCount` decremented on refcount-0 cascade; at 0, delete
  block (local + remote) + drop record. No scan.
- **Corruption/self-heal:** `blake3(block)==blockHash` on fetch (checksum-in-parent);
  local mismatch → refetch block from remote, re-verify, re-stage; remote mismatch →
  fail-closed + metric/alert.
- Retire old rollup→standalone-mirror (`mirrorChunk` + standalone `MarkSynced`).

DoD: blocks-only write+read for new shares, GC + corruption handling, conformance + e2e
green. Legacy CAS reader present only for back-compat reads.

## PR4 — Migration + legacy cleanup (definition of done)

- **Migration:** automatic, blocking at startup, resumable/idempotent. Detect `cas/<hash>`
  (local + remote), re-pack into blocks, rewrite locators in one pass, delete old objects.
  Legacy standalone reader lives only inside this routine.
- **Delete the standalone/CAS path end to end:** `RemoteStore.Put/Get/GetRange`-by-hash,
  `FormatCASKey`/`ParseCASKey`, the `cas/` walk + keying, `ReadBlockVerified`'s standalone
  use, per-chunk local CAS storage remnants, dead config knobs/tests/interfaces.
- Leave no vestigial interfaces (minimize-interface-surface rule).
- **Final naming sweep** (`FileBlock`→`FileChunk`, `BlockRef`→`ChunkRef`,
  `BlockState`→`ChunkSyncState`, etc. — anything not already renamed in PR1–3) and **final
  docs pass** across `docs/internals/` + `docs/guide/`, then regenerate `docs/guide/cli.md`
  via `go run ./cmd/gendocs` if any command/flag changed.

DoD: migration converts a `cas/<hash>` fixture byte-identically + resumable; no standalone
code remains (`grep` clean); naming model fully applied; docs current; full suite + e2e green.

---

## Out of scope (deferred follow-up issues)

#1487 GC compaction · #1488 chunk-range refetch · #1489 remote manifests (namespace DR) ·
#1490 background scrubber · #1491 decoupled log-blob/block sizing · #1492 group-commit fsync.

## Discarded

The in-flight `1414-pr3b` additive commits (`BlockWriter` / `ChunkTransformer` /
`MarkSyncedBatch`, S3/memory pass-through `PutBlock`) are **not** cherry-picked — they
assume the additive two-object-kind model this design supersedes (notably `Put`-by-hash,
which the slim interface removes). Re-derived from scratch per this roadmap.
