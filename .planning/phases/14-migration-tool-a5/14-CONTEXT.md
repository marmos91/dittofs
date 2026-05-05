# Phase 14: Migration tool (A5) — Context

**Gathered:** 2026-05-05
**Status:** Ready for planning
**Milestone:** v0.15.0 Block Store + Core-Flow Refactor
**GH issue:** [#425](https://github.com/marmos91/dittofs/issues/425)
**Requirements:** MIG-01, MIG-02, MIG-03, MIG-04 (and gates VER-04 by integrity check)

<domain>
## Phase Boundary

Ship `dfsctl blockstore migrate --share <name>` — an **offline** tool that converts a v0.13/v0.14 share's block layout from path-indexed legacy keys (`{payloadID}/block-{idx}`) to the v0.15 CAS layout (`cas/{hash[0:2]}/{hash[2:4]}/{hash_hex}`). The tool re-chunks each file via FastCDC, uploads CAS chunks (dedup-aware via `GetByHash`), updates `FileAttr.Blocks` with the new `[]BlockRef` list, runs a post-migration HEAD-per-ref integrity check, then atomically flips the per-share `block_layout` flag from `legacy` to `cas-only` and deletes legacy keys.

**Core work:**

1. **MIG-01:** new `cmd/dfsctl/commands/blockstore/migrate.go` with subcommands `migrate`, `migrate status`. Re-chunks legacy blocks via FastCDC, uploads CAS chunks (existing engine `Put` path), updates `FileAttr.Blocks` in metadata txn, populates `FileAttr.ObjectID` (Phase 13 backfill — see D-A14 below).
2. **MIG-02:** resumability via per-share append-only journal `.migration-state.jsonl` (D-A1..A4); flags `--dry-run`, `--parallel N` (default 4), `--bandwidth-limit MB/s`.
3. **MIG-03:** dual-read shim already lives in engine from Phase 11; this phase adds the per-share `block_layout` flag (legacy | cas-only) on the share metadata record so the shim picks the right code path per share. Phase 15 removes the shim entirely.
4. **MIG-04:** post-migration integrity check = HEAD per BlockRef across all migrated files; legacy keys deleted only after the check passes (end-of-share batch).
5. **REST endpoint** `GET /api/v1/blockstore/migrate/status` — surfaced in this phase per user direction (overrode the recommended "defer to dittofs-pro").
6. **Operator docs:** `docs/BLOCKSTORE_MIGRATION.md` runbook + ARCHITECTURE.md section + IMPLEMENTING_STORES.md schema note + FAQ entry.

**Out of scope:**

- Removal of dual-read shim — Phase 15 (A6).
- Online migration (per-file lock with active writers) — rejected. Offline only.
- Reverse migration (CAS → legacy) — rejected. Failure mode is fail-loud, manual fix, re-run.
- New Prometheus metrics — milestone-wide deferral matches Phase 11/12/13.
- Per-bucket / cross-bucket dedup — non-goal (matches Phase 13 D-02 scope).
- ADR file — phase artifacts are the design record (matches v0.15.0 milestone precedent).

</domain>

<decisions>
## Implementation Decisions

### Resumability + state file (D-A1..A4)

- **D-A1: Per-file checkpoint granularity.** Atomic unit = one file: re-chunk + upload all CAS chunks + update `FileAttr.Blocks` + journal commit. Crash mid-file = redo whole file. `GetByHash` makes per-chunk re-uploads idempotent. State stays simple: file list + done set.
- **D-A2: State file lives in the share's local data dir.** Path: `{share-data-dir}/.migration-state.jsonl` (and rolling snapshot `.migration-state.snapshot.json`). One file per share. Atomic rename within same fs.
- **D-A3: Append-only journal + periodic snapshot.** Each per-file commit appends one JSON line; every N entries (TBD by planner — default 1000) the tool fsyncs + atomic-renames a compacted snapshot. Resume = replay snapshot → tail journal. Cheap writes + bounded recovery time.
- **D-A4: Trust journal head on resume.** Resume from last committed file. Orphan CAS chunks left by a crashed mid-file run are reclaimed by GC later (their hashes won't appear in any committed `FileAttr.Blocks`). No re-verification of past files. `--resume-verify=N` flag is a future enhancement, not Phase 14.

### Online vs offline + shim cutover (D-A5..A8)

- **D-A5: Offline only.** Tool refuses to run if the share is mounted/active in any server process. Detection: probe the daemon's `dfs status` (or socket lockfile) before starting. Operator schedules a maintenance window. Matches ROADMAP wording verbatim.
- **D-A6: Per-share `block_layout` flag lives on the share record in the metadata store.** New field `block_layout: legacy | cas-only` (string enum, default `legacy` for migrated-from-v0.13/14 shares; default `cas-only` for greenfield-v0.15 shares created post-merge). Engine reads on share open. Toggle is a metadata txn — replicates with metadata, single source of truth.
- **D-A7: Auto cutover on `migrate` success after integrity check.** Tool flips `block_layout` to `cas-only` in the same metadata txn that issues the deletion of the last legacy keys, only after HEAD-per-ref integrity check returns 200 for every BlockRef. Operator sees one-step success.
- **D-A8: No rollback. Fail loud.** On integrity-check failure or operator abort: exit non-zero, leave shim on `legacy`, leave journal in place, leave any CAS chunks in S3 (orphaned hashes — GC reclaims). No automatic legacy-key restore. Operator inspects, fixes (S3 outage, hash mismatch), re-runs. Aligns with end-of-share legacy GC (D-A12).

### Bandwidth + parallelism (D-A9..A11)

- **D-A9: Shared global token bucket on uploads only.** Single `*rate.Limiter` shared across parallel workers, governs S3 PUT bytes. Legacy reads from local disk are unmetered. Matches ROADMAP risk note "aggregate of parallel workers, not per-worker."
- **D-A10: `--parallel` default = 4. Tool refuses to run if daemon is active.** ROADMAP-mandated default. Offline-only invariant (D-A5) means no live daemon contention possible. No coordination needed with running server's autoscaled `remote.parallel_uploads`.
- **D-A11: `--bandwidth-limit` = MB/s decimal (1,000,000 bytes/s) with suffix support.** Accept `KB/MB/GB` (decimal) and `KiB/MiB/GiB` (binary). `--bandwidth-limit=50MB` = 50_000_000 B/s. Implemented via `golang.org/x/time/rate.Limiter`; each worker calls `WaitN(ctx, len(chunkBytes))` before PUT.

### Integrity check + legacy GC (D-A12..A13)

- **D-A12: HEAD per BlockRef across all migrated files.** Iterate every unique `ContentHash` referenced by any migrated `FileAttr.Blocks`, S3 HEAD each, assert 200. Linear in unique-blocks (dedup helps). Strong existence guarantee. CAS keys = hash so mismatch is impossible by construction. Add `x-amz-meta-content-hash` HEAD parity check while we're at it — zero extra request cost.
- **D-A13: Legacy key deletion happens at end-of-share, after integrity check passes.** All `FileAttr.Blocks` updated first; integrity check runs on the full migrated set; only then does the tool iterate legacy `{payloadID}/block-{idx}` keys and delete. Enables D-A8 fail-loud rollback (legacy intact on integrity failure).

### CLI / status / REST (D-A14..A16)

- **D-A14: ObjectID backfill happens during migration.** Each file gets its `FileAttr.ObjectID` populated (Phase 13 BLAKE3 Merkle root) in the same metadata txn that updates `FileAttr.Blocks`. Closes the Phase 13 D-03 deferral ("legacy files keep all-zeros sentinel; Phase 14 migration backfills").
- **D-A15: Progress reporting = structured slog + TTY progress bar when stdout is a tty.** All events through `slog.Info` (machine-parseable). On `term.IsTerminal(os.Stdout.Fd())`, render per-second-refreshed bar (files done / total, bytes uploaded, ETA). Pipe-safe by default.
- **D-A16: Status command = CLI human + `-o json` AND REST endpoint shipped in Phase 14.** `dfsctl blockstore migrate status --share NAME` reads `.migration-state.jsonl` + queries metadata for `block_layout`. Default human table; JSON via `-o json` (existing dfsctl convention). REST endpoint `GET /api/v1/blockstore/migrate/status` added now (user-elected — overrode "defer" recommendation) for dittofs-pro UI consumption.

### Documentation (D-A17..A20)

- **D-A17: Primary operator runbook = `docs/BLOCKSTORE_MIGRATION.md`.** Hand-written: pre-flight checklist, step-by-step (stop daemon → `migrate` → verify → restart with `cas-only` shim), bandwidth tuning advice, common failure modes, recovery procedures. CLI flag reference auto-generated from cobra into `docs/CLI.md` (existing pattern). REST endpoint added to OpenAPI spec.
- **D-A18: Internals docs updated.** `docs/ARCHITECTURE.md` — new section explaining dual-read shim + per-share `block_layout` flag + migration tool boundary. `docs/IMPLEMENTING_STORES.md` — note that new metadata backends MUST persist `block_layout` field on share record (conformance suite addition). `docs/FAQ.md` — add "How do I migrate from v0.13 to v0.15?" entry.
- **D-A19: Runbook includes four worked transcripts.** (1) ~10 GB share happy path; (2) TB-scale share with `--parallel` + `--bandwidth-limit` tuning; (3) crash mid-migration + auto-resume; (4) integrity-check failure + manual diagnosis + re-run. Copy-pasteable commands and expected stdout snippets.
- **D-A20: Design docs live in phase artifacts only.** `.planning/phases/14-migration-tool-a5/{14-CONTEXT,14-RESEARCH,14-PLAN}.md` are the design record. No new ADR. Matches Phase 11/12/13 precedent.

### Claude's Discretion

- Concrete journal snapshot interval (D-A3 mentions default 1000 entries — planner picks; tunable).
- Cobra command/subcommand naming detail (`migrate` vs `migrate run`, `status` flag layout) — match existing dfsctl conventions.
- REST handler placement within the existing `cmd/dfs/` API tree.
- Worker pool implementation style (`errgroup` vs custom semaphore) — pick whatever already used in `pkg/blockstore/engine/syncer.go`.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### Roadmap + requirements
- `.planning/ROADMAP.md` §"Phase 14: Migration tool (A5)" (lines 247–269) — goal, deps, requirements, success criteria, files-to-touch, key risks.
- `.planning/REQUIREMENTS.md` — MIG-01..MIG-04 definitions; gates VER-04.
- `.planning/PROJECT.md` — milestone v0.15.0 framing.

### Prior-phase decisions Phase 14 builds on
- `.planning/phases/11-cas-write-path-gc-rewrite-a2/11-CONTEXT.md` — CAS layout `cas/{hash[0:2]}/{hash[2:4]}/{hash_hex}`, three-state lifecycle (Pending → Syncing → Remote), dual-read shim introduction.
- `.planning/phases/12-cdc-read-path-metadata-engine-api-a3/12-CONTEXT.md` — `FileAttr.Blocks []BlockRef` schema, `MetadataCoordinator.IncrementRefCount`, engine `ReadAt/WriteAt([]BlockRef, …)` signature, conformance suite.
- `.planning/phases/13-merkle-root-file-level-dedup-a4/13-CONTEXT.md` — `FileAttr.ObjectID` BLAKE3 Merkle root (D-01..D-03), legacy-file all-zeros sentinel awaiting Phase 14 backfill.

### Operator-facing docs (to update)
- `docs/BLOCKSTORE_MIGRATION.md` — **new** in this phase (D-A17).
- `docs/ARCHITECTURE.md` — update with shim + `block_layout` + migration boundary (D-A18).
- `docs/IMPLEMENTING_STORES.md` — add `block_layout` share-record schema note (D-A18).
- `docs/CLI.md` — auto-regenerated from cobra after `migrate` lands (D-A17).
- `docs/FAQ.md` — v0.13→v0.15 migration entry (D-A18).

### Codebase maps (already exist)
- `.planning/codebase/STRUCTURE.md` — `cmd/dfsctl/commands/` Cobra layout (where `blockstore/migrate.go` plugs in).
- `.planning/codebase/ARCHITECTURE.md` — engine + syncer + adapter layering.
- `.planning/codebase/CONVENTIONS.md` — slog usage, dfsctl `-o json` flag, error-mapping conventions.

### External (downstream agents will need)
- `golang.org/x/time/rate` — token-bucket reference (D-A11).
- `github.com/spf13/cobra` — already a dep.
- `lukechampine.com/blake3` — already in tree (Phase 13 D-08 amended).

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/blockstore/engine/syncer.go` — existing CAS write path (`drainPayloadToRemote`, `Flush`). Migration tool reuses it: re-chunked blocks go through the same `Put` path so dedup is automatic.
- `pkg/blockstore/engine/cas.go` (or equivalent) — `GetByHash(ctx, hash) (BlockRef, bool)` is the dedup probe. Tool calls it before each PUT to avoid wasted bandwidth (also pays off on resume).
- `pkg/metadata/.../store.go` — `MetadataCoordinator.IncrementRefCount` lets the tool bump refcounts without re-uploading on dedup hits.
- `pkg/blockstore/local/legacy.go` (or equivalent v0.13 reader) — legacy `{payloadID}/block-{idx}` reader from the dual-read shim. Migration tool reads through it directly (offline) to feed FastCDC.
- FastCDC chunker from Phase 10 — invoked via `engine.Chunk(reader)`; min=1MB / avg=4MB / max=16MB / level 2 are locked.
- `cmd/dfsctl/cmdutil/util.go` — auth + output formatting + `-o json` plumbing.
- Existing structured slog pattern in `cmd/dfs/` and `pkg/blockstore/engine/`.

### Established Patterns
- Cobra command tree under `cmd/dfsctl/commands/<noun>/<verb>.go` — migration follows: `cmd/dfsctl/commands/blockstore/migrate.go`, optionally `migrate_status.go`.
- Per-share atomic operations go through metadata txns (Phase 12 D-37 seam).
- `errgroup`-based worker pools in syncer code.
- REST endpoints registered in `cmd/dfs/<api>/router.go` with `-o json` parity to dfsctl.

### Integration Points
- Share record schema (every metadata backend: Postgres, Badger, memory) gains `block_layout` column/field. Conformance suite (`pkg/metadata/storetest/`) adds a test that asserts the field round-trips.
- Engine `bs.Open(share)` reads `block_layout` once and routes legacy reads through the shim or the new CAS path accordingly.
- `pkg/blockstore/engine/gc.go` — orphaned CAS chunks left by a crashed migration are GC-reclaimable through normal mark-sweep; no special path needed.
- Daemon-active probe — reuse whatever `dfs status` / lockfile mechanism currently exists (codebase scout: check `cmd/dfs/`).

</code_context>

<specifics>
## Specific Ideas

- ROADMAP risk note "Resumability must tolerate mid-chunk crashes without duplicate uploads" → satisfied by D-A1 (per-file granularity) + `GetByHash` dedup safety net (D-A4).
- ROADMAP risk note "Bandwidth limit must apply to the aggregate of parallel workers, not per-worker" → satisfied by D-A9 (single shared `rate.Limiter`).
- ROADMAP risk note "Legacy key deletion must happen only after all references in FileAttr.Blocks are confirmed migrated" → satisfied by D-A12 + D-A13 (HEAD-per-ref + end-of-share batch deletion).
- Four-week duration includes production rollout window — operator docs (D-A17..A19) are the long-tail deliverable.

</specifics>

<deferred>
## Deferred Ideas

- `--resume-verify=N` flag (re-verify last N committed files via HEAD on resume) — out of Phase 14 scope; can be added when first incident demands it.
- `migrate revert` (full reverse migration CAS → legacy) — rejected outright. Re-run forward path on failure.
- Two-step manual cutover (`migrate` then explicit `migrate cutover`) — rejected in favor of auto cutover gated on integrity check (D-A7).
- Per-chunk checkpoint granularity — rejected (D-A1).
- Bandwidth limit on legacy reads — rejected (D-A9 uploads-only).
- Cross-bucket dedup during migration — explicit non-goal (matches Phase 13 D-02).
- New Prometheus metrics for migration progress — observability phase milestone-wide.
- ADR file for migration design — phase artifacts are the record (D-A20).

</deferred>

---

*Phase: 14-migration-tool-a5*
*Context gathered: 2026-05-05*
