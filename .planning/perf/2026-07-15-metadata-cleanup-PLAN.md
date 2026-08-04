# Metadata Store Cleanup — Sequenced Plan

Capstone of three read-only audits (badger schema, Go entity model, SQL). **Goal:
simpler, cleaner, easier for a human to maintain** — fewer types, fewer tables, one
source of truth. Lower hot-path write cost and #1687 relief are the co-benefit.

Tracker: **#1715**. Findings: `2026-07-15-metadata-{schema-inventory,entity-model,sql-schema}.md`.
Rendered artifacts linked from the issue.

## Root cause the three audits converge on

Every backend **rewrites the whole block manifest on an attr-only change**:
- badger re-serializes `Blocks[]` inside the `f:` blob on every `PutFile`;
- SQL runs `DELETE + INSERT×M` on `file_block_refs` on every `PutFile` for a regular file
  (pg transaction.go:476 / sqlite:407).

A `chmod`/`utimes`/`close` rewrites the file's entire chunk list. Shared cause:
**`PutFile(File)` conflates attribute mutation with manifest mutation.** This is both the
biggest write-amplifier and the biggest source of accidental complexity. Wave 1 patches
it per-backend; Wave 3 fixes the seam in the interface.

## Wave 1 — independent, ship now

Order within the wave: quick hygiene first (low risk, builds confidence), then the
manifest seam (the real win), each its own PR.

| # | Change | Backends | Files | Verify | Risk |
|---|---|---|---|---|---|
| 1 | Delete dead `d:` device prefix | badger | encoding.go:40,51 | grep no reader; build | none |
| 2 | Drop dead/redundant SQL indexes + pg `updated_at` trigger | pg+sqlite | new migration `000039`(pg)/`000006`(sqlite) | conformance suite; EXPLAIN unaffected | low |
| 3 | Drop dead `pending_writes` table | pg | migration + snapshot_store.go:56 | grep no reader; conformance | low |
| 4 | Relocate `ShareSession` out of core types | badger/mem | types.go:161 → store/memory | build; memory-store tests | low |
| 5 | Fold `FilesystemMeta` → `cap:fs` | badger | store.go:339; statfs path | statfs read-path test | low |
| 6 | `Nlink` → one source (keep `l:`, drop blob field) | badger | file_types.go:42; files.go:151-168; encoding | grep every `File.Nlink` reader trusts `l:` override; hardlink test | low-med |
| 7 | Fold SUID/SGID-clear into deferred flush | badger | io.go:245-257 / 452-454 | GETATTR MODE-visibility (io.go:234-238); SUID e2e | low-med |
| 8 | **Separate attr-write from manifest-write** | ALL | badger: `fb:<uuid>` split (encoding, PutFile, every File reader). SQL: blocks-dirty gate in `putFileChunkRefs` | attr-only op writes NO manifest rows; truncate still prunes; conformance + e2e | **medium** |
| 8b | sqlite multi-row manifest INSERT (folds into 8) | sqlite | file_block_refs.go:32-48 | carve writes one statement | low |

Verification note for #8 (the important one): the guard must keep the manifest write
whenever `Blocks` legitimately changed — i.e. carve/finalization and truncate
(`PruneChunkRefsToSize`). The test that matters: a `chmod` on an M-chunk file writes zero
manifest rows; a truncate still prunes.

## Wave 2 — with the journal switchover (#1692)

These land as the switchover's cleanup tail (same PR family as `li:`/`ro:` deletion). Do
NOT land on develop before the journal is the live local store.

- **flush → relaxed** — the actual #1687 dissolution (moves the metadata fsync off `db.lock`).
  **Design + measurements: #1687 comment (2026-07-16). The naive "swap io.go:434 to
  withRelaxedTransaction" ALONE is a size-corruption bug** — nothing reconciles
  `journal.FileSize` into `metadata.Size` on reopen (FileSize's only consumer is DataExtents),
  so a crash after a relaxed size-commit silently truncates an ACK'd file (#588 class). Ship
  THREE together: (1) **startup size reconciliation** `metadata.Size = max(metadata.Size,
  journal.FileSize(id))` per share over `journal.ListFiles()`, never shrink — the load-bearing
  precondition; (2) relax `flushPendingWrite` (io.go:434) → `withRelaxedTransaction` gated by a
  `durable` flag; (3) **keep FILE_SYNC (Stable=2) strict** (`durable=true`), COMMIT/DATA_SYNC/SMB
  pass `false`. Ordering already correct (journal fsync before metadata). Biggest win = SMB
  per-write (removes its only fsync) + halves NFS COMMIT. Requires store `RelaxedDurability=true`.
- **Delete `li:`/`ro:` (badger) + `local_chunk_index`/`rollup_offsets` (SQL)** — journal
  `.idx` is authoritative. Universal.
- **Collapse `BlockChunkCommit` → `{Hash, Remote}`** — `.Local` always zero post-journal.
- **Slim `block.FileChunk`** — grep field usage after #1692; drop `LocalPath` (and likely
  `BlockStoreKey`/`LastSyncAttemptAt`). Verify before acting.

## Wave 3 — design track (consider after the quick wins)

- **Reshape the store interface** — split attribute mutation from manifest mutation
  (`UpdateAttrs(id, delta)` vs `SetManifest(id, refs)`), so no backend can rewrite the
  manifest on an attr change. Makes Wave-1 #8 structural instead of a per-backend guard,
  and lets the entities slim. A design decision to take deliberately, not a mechanical edit.
- **Unify the manifest model** across backends (embedded slice / join table / derived
  projection are three shapes for one concept).
- **Binary-encode the residual attr-only `f:` blob (badger)** — speculative, only if the
  manifest split alone doesn't move the wall.

## Docs refresh — DONE (2026-07-15, in working tree, awaiting PR)

Captured the *current* design while it was fresh:
- **NEW `docs/internals/metadata-store.md`** — maintainer-facing design doc: entity model +
  persistence classes, the `metadata.Store` interface (embeds `Files`), per-backend
  realization (badger key-prefix table / SQL relational table / memory), the centerpiece
  "same entity, three encodings" table, the durability model (badger fsync-per-commit vs
  sqlite WAL+NORMAL vs pg synchronous_commit), and a "Known simplification work" section
  linking #1715 + the three audit docs (planned items clearly labeled not-on-develop).
- **EDITED `docs/internals/implementing-stores.md`** — added the missing `sqlite/` backend to
  the reference list + a "See also" cross-link to the new page. (No mkdocs nav exists;
  discoverability is via cross-links to architecture.md / acl-design.md / implementing-stores.md.)

Reconciliation the docs pass surfaced (accuracy notes, not plan changes):
- Concrete top-level interface is `metadata.Store` (embeds `Files`), not "MetadataStore".
- Xattr has **two** physical backings — inline `FileAttr.EAs` **and** named-stream child
  entities (`xattr.go`, stream-wins) — the entity audit's "no xattr keyspace" (§1.8) covers
  only the inline backing. EAs verdict is unaffected (KEEP either way).
- `FileAttr.Rdev` confirmed (was `[S]` in the entity audit); `d:` prefix confirmed dead.

**Parked on branch `docs/metadata-store`** (off develop, pushed to origin, NO PR). Do NOT
ship standalone — the docs describe the *current* design, which Wave-1/2 changes (Blocks
split, nlink dedup, `li:`/`ro:` removal) will alter. **Ship them WITH the code PR that
changes what they describe, updating the doc in the same PR** (e.g. the `fb:` split PR
updates the "same entity, three encodings" table + the badger key-prefix table). Retrieve
with `git checkout docs/metadata-store -- docs/internals/metadata-store.md docs/internals/implementing-stores.md`.

## Out of scope / refuted

- Cross-thread commit coalescing / group-commit — refuted ×3 (sync-vs-write, not sync-vs-sync).
- badger vlog/memtable tuning — eliminated (values sub-ValueThreshold).
- Merging CREATE+WRITE+COMMIT into one txn — distinct NFS RPCs.
- GORM footguns — N/A; SQL backends are raw pgx/database-sql.
