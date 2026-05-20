# Phase 17: Unified BlockStore interface + legacy delete + migration tool - Context

**Gathered:** 2026-05-20
**Status:** Ready for planning

<domain>
## Phase Boundary

Collapse `LocalStore` (22 methods) and `RemoteStore` (12 methods) onto a single `BlockStore` interface keyed by `ContentHash`. Local additionally implements `BlockStoreAppend` (the random-write absorber tier). Delete the legacy path-keyed `.blk` layout entirely, the engine dual-read shim, and all legacy-tier helpers (`IsBlockLocal`, `GetBlockData`, `ExistsOnDisk`, `DeleteBlockFile`, `DeleteAllBlockFiles`, `TruncateBlockFiles`, `WriteFromRemote`, `CopyBlock`, `FormatStoreKey`, `UseAppendLog` flag, `ErrAppendLogDisabled`). Collapse `localtest/` + `remotetest/` into a single `blockstoretest/` conformance suite covering fs + s3 + memory backends. Ship one-shot legacy → CAS migration command. Boot fails hard on un-migrated stores.

This is Phase 2 of 4 in v0.16.0 (CAS Convergence) — depends on Phase 16 (Cache RAM-only, shipped PR #522). Subsumes v0.15.0 Phase 15 (A6 legacy cleanup) entirely.

**In scope:** unified `BlockStore` + `BlockStoreAppend` interface definitions, narrowed `LocalStore`, renamed `RemoteStore` methods (`WriteBlock`→`Put`, `ReadBlock`→`Get`, `ReadBlockRange`→`GetRange`, `DeleteBlock`→`Delete`, `HeadObject`→`Head`, collapse `ListByPrefix*`/`DeleteByPrefix` into `Walk`), deletion of `pkg/blockstore/local/fs/write.go` + dual-read shim, consolidated `blockstoretest/` suite, `dfs migrate-to-cas` offline subcommand, sentinel-file boot guard.

**Out of scope:** Syncer simplification (Phase 18), `ComputeObjectID` relocation to rollup (Phase 18), write-path RAM optimizations (Phase 19), cold-cache benchmarks (deferred to v0.17+), any backward-compat shim or feature flag for legacy reads.

</domain>

<decisions>
## Implementation Decisions

### PR shape
- **D-01:** Phase 17 ships as a single mega-PR per spec. All Decision-2 work (new interfaces + local narrowing + remote rename + legacy `.blk` writer deletion + dual-read shim deletion + `blockstoretest/` consolidation + `dfs migrate-to-cas` subcommand + boot guard) in one PR against `develop`. Review burden is high (~6k LoC delta, ~30–40% net reduction) but the spec's "no flag-gated half-states, no dual-read shims" rule is strict — split commits would either leave develop unbuildable mid-sequence or require transient compat shims that contradict the spec. Internal commit ordering within the PR may still be staged (interfaces → consumers → deletions) for `git log -p` reviewability.

### Migration tool placement
- **D-02:** `dfs migrate-to-cas` is an OFFLINE server-side cobra subcommand on the `dfs` binary (NOT `dfsctl blockstore migrate-to-cas` as the spec phrased it). Server daemon must be stopped (or the command refuses if a PID/lock file is detected). Touches local filesystem + metadata store directly; no REST round-trip, no in-flight client quiescing. Mirrors the existing `pkg/blockstore/migrate/migrate_offline.go` pattern from Phase 14 A5.
- **D-03:** Migration tool is idempotent — re-running after a partial completion picks up where it left off. State journaled per-share at `<storage_dir>/<share>/.dittofs-migrate-to-cas.state` (JSON: last-processed `.blk` path, byte offset, timestamp, version). On crash recovery, resume from the journal.
- **D-04:** `--dry-run` flag walks the legacy `.blk` tree and reports `file count`, `total bytes`, `estimated dedup ratio` (from FastCDC pre-pass on a sample), `estimated migration duration` — without writing anything. Required for ops before destructive run.
- **D-05:** `--share <name>` flag scopes migration to one share at a time. Default = all shares. Lets ops migrate the largest share off-hours independently.
- **D-06:** Progress reporting to stdout: `files/sec`, `MiB/sec`, `ETA`, `dedup hits`. Optional `--json` flag emits one JSON object per line for machine parsing (operator/CI consumers). Important on multi-TB stores where the migration may run for hours.

### Walk + Meta contract
- **D-07:** Walk callback error semantics — callback returns `blockstore.ErrStopWalk` sentinel for clean early-exit (e.g., GC found its target); any other non-nil error halts the walk and `Walk` returns it wrapped with `fmt.Errorf("walk halted at %s: %w", hash, err)`. Context cancellation aborts immediately (callback not re-invoked after `ctx.Err() != nil`). Pattern mirrors `filepath.SkipDir` / `fs.SkipAll`, idiomatic Go.
- **D-08:** `Meta` struct lives in `pkg/blockstore` (next to existing `types.go`), minimal fields per spec: `Size int64` + `LastModified time.Time`. The `ContentHash` is the key, NOT echoed inside `Meta` (no redundancy). S3's `x-amz-meta-content-hash` header is preserved INSIDE the s3 backend as defense-in-depth per BSCAS-06 (verified during `ReadBlockVerified`) but is not exposed through `Meta`.

### Conformance suite factoring
- **D-09:** `pkg/blockstore/blockstoretest/` exposes TWO top-level entrypoints: `func BlockStoreConformance(t *testing.T, factory func() BlockStore)` and `func BlockStoreAppendConformance(t *testing.T, factory func() BlockStoreAppend)`. Backends call whichever applies — fs calls both, s3/memory call only `BlockStoreConformance`. Discoverable (each backend's test file explicitly declares which contracts it claims). Matches existing `localtest/suite.go` + `remotetest/suite.go` shape so the consolidation is a structural collapse, not a redesign.

### Boot-time hard-error detection
- **D-10:** Un-migrated store detection uses a sentinel marker file: `<share_dir>/.cas-migrated-v1` (per-share, NOT per-storage-dir — `--share <name>` produces a per-share sentinel and operational semantics are cleaner this way) written by `dfs migrate-to-cas` ONLY at successful completion (atomic rename from `.cas-migrated-v1.tmp` to ensure no partial state appears migrated). `*fs.FSStore` constructor (`NewFSStore`) stats this file at open time; cheap O(1). Records timestamp + tool version inside the file for audit trail. Survives mid-migration crash because the file is only written at the very end.
- **D-11:** Detection failure mode — `NewFSStore` returns a typed sentinel `blockstore.ErrLegacyLayoutDetected` (new error var in `pkg/blockstore/errors.go`) wrapping the offending share path. `cmd/dfs/start` unwraps via `errors.Is` (sentinel is an `errors.New` value, not a typed struct — `errors.Is` is the idiomatic match for sentinel-var detection), prints a multi-line directive to stderr ("Detected legacy `.blk` layout at <path>. v0.16+ requires CAS migration. Run `dfs migrate-to-cas --share <name>` (or `dfs migrate-to-cas` for all shares) before starting. See docs/CONFIGURATION.md §migration."), and exits with code 78 (`EX_CONFIG` from `sysexits.h` — "configuration error"). Per-share fail-fast: first un-migrated share halts boot.

### Claude's Discretion
- Exact LocalStore method cull from 22 → ~12 — spec lists the deletions, but borderline methods (`Truncate`, `EvictMemory`, `SetRetentionPolicy`, `SetEvictionEnabled`, `Stats`, `ListFiles`, `GetStoredFileSize`, `Healthcheck`, `SyncFileBlocks`, `SyncFileBlocksForFile`, `DeleteAppendLog`) are lifecycle/admin rather than byte-access. Researcher + planner decide which stay on the narrowed `LocalStore` (as superset of `BlockStoreAppend`) vs become free functions or move to a separate `LocalStoreAdmin` interface.
- Internal commit ordering inside the mega-PR (additive interfaces first, consumers migrated, then deletions) — planner-decided. Spec requires the PR to merge atomically; internal commit hygiene is a quality-of-review concern only.
- Whether `Walk` exposes its concurrency knob (e.g., for fs backend's 256-shard parallel walk) — keep internal unless conformance tests need to assert a serial order.
- Whether `Has` is implemented as HEAD (S3) or as `Get` with `Range: bytes=0-0` (more portable, costlier) — backend-specific; pick per backend.

</decisions>

<canonical_refs>
## Canonical References

**Downstream agents MUST read these before planning or implementing.**

### v0.16.0 design (locked spec — SOURCE OF TRUTH)
- `~/.claude/plans/reactive-sprouting-moonbeam.md` §"Decision 2" + §"Critical Files (modified) — Block store (Decision 2, Phase 17)" + §"Sequencing" — locks interface shapes, deletion list, conformance approach, migration tool intent.
- `.planning/ROADMAP.md` lines 37–55 (v0.16.0 milestone) + line 46 (Phase 17 entry) — locked decisions + intended outcome.

### Phase 16 carry-forward (already shipped)
- `.planning/phases/16-cache-mmap-removal/16-CONTEXT.md` §"Implementation Decisions" — especially **D-01** locking the `local.Get(ctx, hash ContentHash) ([]byte, error)` signature that Phase 17's `BlockStore.Get` adopts verbatim. Zero rename churn at call sites.
- `.planning/phases/16-cache-mmap-removal/16-VERIFICATION.md` — Phase 16 success criteria (warm-cache D-06 PASS ratio 0.890); Phase 17 must keep this gate green.

### GitHub tracking
- https://github.com/marmos91/dittofs/issues/515 — v0.16.0 parent tracking issue
- https://github.com/marmos91/dittofs/issues/517 — Phase 17 sub-issue (this phase)
- https://github.com/marmos91/dittofs/issues/426 — v0.15.0 Phase 15 (A6 legacy cleanup) — SUBSUMED by Phase 17, can be closed on ship.

### Existing interfaces to be replaced
- `pkg/blockstore/local/local.go:52` `LocalStore` interface (22 methods) — narrow to ~12; embed `BlockStoreAppend`.
- `pkg/blockstore/remote/remote.go:63` `RemoteStore` interface (12 methods) — rename `WriteBlock`→`Put`, `ReadBlock`→`Get`, `ReadBlockRange`→`GetRange`, `DeleteBlock`→`Delete`, `HeadObject`→`Head`; collapse `ListByPrefix*`/`DeleteByPrefix` into `Walk`; delete `CopyBlock`, `WriteBlockWithHash` (hash is the key).
- `pkg/blockstore/store.go` — existing top-level helpers (may grow / shrink with interface relocation).
- `pkg/blockstore/types.go` — DELETE `FormatStoreKey` + legacy `BlockStoreKey` parsing.
- `pkg/blockstore/errors.go` — add `ErrStopWalk`, `ErrLegacyLayoutDetected`, `ErrChunkNotFound` (if not already present from Phase 16).

### Code to delete entirely
- `pkg/blockstore/local/fs/write.go` — legacy path-keyed writer (`WriteAt`, `tryDirectDiskWrite`, `ensureBlockFile`, `directDiskWriteThreshold`, `<share>/<file>/<idx>.blk` layout, `memBlock` map).
- `pkg/blockstore/engine/store.go` — engine dual-read shim (`{payloadID}/block-{idx}` vs `cas/...` branching based on `len(blocks) == 0`).
- `pkg/blockstore/local/localtest/` — collapsed into `pkg/blockstore/blockstoretest/`.
- `pkg/blockstore/remote/remotetest/` — collapsed into `pkg/blockstore/blockstoretest/`.

### Code to introduce / heavily modify
- `pkg/blockstore/blockstore.go` (NEW or extended) — `BlockStore` + `BlockStoreAppend` interfaces, `Meta` struct.
- `pkg/blockstore/blockstoretest/` (NEW directory) — `BlockStoreConformance` + `BlockStoreAppendConformance` entrypoints. Scenarios: Put-Get roundtrip, Get-not-found, range read, delete, walk, head, idempotent Put, concurrent Put-same-hash; AppendLog: AppendWrite-then-rollup → chunks via `Walk`, log deleted via `DeleteLog`.
- `cmd/dfs/commands/migrate_to_cas.go` (NEW) — offline cobra subcommand (idempotent, `--dry-run`, `--share`, progress + `--json`).
- `pkg/blockstore/migrate/migrate_to_cas.go` (NEW) — shared library backing the subcommand; runs FastCDC over `.blk` content, writes via `Put(hash, data)`, rebuilds `FileAttr.Blocks` manifest, deletes `.blk` files, writes `.cas-migrated-v1` sentinel at end.
- `pkg/blockstore/local/fs/fs.go` — `NewFSStore` adds sentinel-file check; returns `ErrLegacyLayoutDetected` on miss when `.blk` files present.
- `cmd/dfs/commands/start.go` — unwraps `ErrLegacyLayoutDetected` via `errors.Is`, prints directive, exits 78.
- `pkg/blockstore/remote/s3/store.go` — rename methods to match `BlockStore`; keep `x-amz-meta-content-hash` for defense-in-depth.

### Existing helpers (reuse)
- `pkg/blockstore/local/fs/chunkstore.go::ReadChunk(h ContentHash)` — already returns chunk bytes by hash (used by Phase 16 `local.Get`).
- `pkg/blockstore/migrate/` (Phase 14 A5) — patterns for offline runtime, per-share scope, bandwidth controls, progress reporting, status journaling. The new `migrate_to_cas.go` follows this layout.
- `pkg/blockstore/chunker/` FastCDC chunker — reused for legacy `.blk` → CAS chunking inside migration.
- `pkg/blockstore/objectid.go::ComputeObjectID` — recomputed during migration to rebuild `FileAttr.Blocks` manifest with correct ObjectIDs.

### Project conventions
- `CLAUDE.md` (project root) — protocol-handler boundary rule (handlers stay protocol-only), AuthContext threading, error-code conventions, sign commits, no Claude Code mentions.
- `pkg/blockstore/doc.go` + `pkg/blockstore/local/local.go` doc comments — existing tone/format for interface docs.

</canonical_refs>

<code_context>
## Existing Code Insights

### Reusable Assets
- `chunkstore.ReadChunk(h)` — already provides the hash-keyed read path that `BlockStore.Get` maps onto on the local side; Phase 16 already shipped `*FSStore.Get` wrapping it. Phase 17 only needs to consolidate the type the engine takes (`BlockStore` instead of `*FSStore`).
- `pkg/blockstore/migrate/` Phase 14 A5 infrastructure — `migrate_offline.go`, `migrate_progress.go`, `migrate_status.go`, `migrate_workers.go`, `migrate_loop.go`, `migrate_runtime.go`, `migrate_legacy_reader.go` are an entire reusable framework for offline migrations with per-share scope, journaling, progress, status reporting. `migrate_to_cas.go` plugs into this framework rather than reinventing.
- `localtest/suite.go` + `remotetest/suite.go` — existing conformance suites; structural collapse into `blockstoretest/` retains the test scenarios, just unifies the factory shape and adds the `Walk`/`Head` cases.
- BLAKE3 hashing path (`pkg/blockstore/local/fs/rollup.go:325` `blake3ContentHash`) — used by migration to compute hashes for chunked-from-`.blk` data.

### Established Patterns
- `LocalStore` interface methods take `ctx context.Context` first — `BlockStore` follows.
- Error-wrapping via `fmt.Errorf("...: %w", err)` + sentinels in `pkg/blockstore/errors.go` — `ErrStopWalk` + `ErrLegacyLayoutDetected` join `ErrChunkNotFound`, `ErrBlockNotFound`, `ErrCASContentMismatch`.
- Phase 16's "forward-compat naming" rule: any new method signature added in Phase N must match Phase N+1's eventual interface verbatim. Phase 17 closes this contract — `BlockStore.Get` signature equals `local.LocalStore.Get` from Phase 16.
- Conformance suites live next to the package they verify (`localtest/` next to `local/`, `remotetest/` next to `remote/`). Phase 17 hoists to `pkg/blockstore/blockstoretest/` because there is now one contract for both.
- Sentinel marker files for irreversible state transitions are not currently used in DittoFS — `.cas-migrated-v1` is a new pattern. Establish convention in `pkg/blockstore/doc.go`.
- Cobra subcommand naming: project uses `dfs <verb>` (e.g., `dfs start`, `dfs status`) — `dfs migrate-to-cas` fits.
- Existing exit codes: scan `cmd/dfs/` for prior `os.Exit` usage; 78 (`EX_CONFIG`) is new for this project but standard per sysexits(3).

### Integration Points
- `pkg/blockstore/engine/engine.go:181–185` — Cache construction in `BlockStore.Start()`. Phase 17 narrows the receiver type from `*fs.FSStore` (or `local.LocalStore`) to `blockstore.BlockStore` at the engine-side closure; no logic change inside the engine.
- `pkg/blockstore/engine/syncer.go` + `upload.go` — Syncer currently calls `WriteBlock` / `WriteBlockWithHash` / `ReadBlock` on `RemoteStore`. Phase 17 renames the call sites to `Put` / `Get` (the WriteBlockWithHash collapse is mechanical — Put always takes the hash as the key). Syncer's *logic* simplification is Phase 18; Phase 17 only does the renames.
- Adapter layer (`internal/adapter/{nfs,smb}`) does NOT call `LocalStore` / `RemoteStore` directly post-Phase-9 — it goes through `internal/adapter/common/` helpers which take `engine.BlockStore`. Phase 17 may not need adapter changes if engine's public API is preserved.
- Boot path: `cmd/dfs/start.go` → instantiates shares → each share calls `pkg/blockstore/local/fs.NewFSStore` (or the chosen backend). `ErrLegacyLayoutDetected` surfaces back through the share init and trips the dfs-start hard-error path.
- Config (`pkg/config/`) — no new fields; migration is governed by sentinel file presence + CLI flags, not config.

</code_context>

<specifics>
## Specific Ideas

- `dfs migrate-to-cas` (NOT `dfsctl blockstore migrate-to-cas` as the spec phrased it) — user explicitly chose offline server-side cobra subcommand over the REST-mediated form. Downstream agents: follow user's decision (D-02), not spec wording.
- Sentinel marker file `.cas-migrated-v1` is the canonical migration completion proof. Naming convention: `.cas-migrated-vN` for future schema bumps.
- Mega-PR shape is the user's explicit call (D-01), in alignment with spec. The PR will be large; researcher + planner should structure internal commits for `git log -p` readability even though the merge is atomic.
- Exit code 78 (`EX_CONFIG`) for the boot hard-error is new for the project — note in `docs/CONFIGURATION.md` migration section.
- `Meta` echoes nothing redundant — hash is the key, period. S3's `x-amz-meta-content-hash` stays in the s3 backend internals (BSCAS-06 defense-in-depth) but is invisible to `Meta` consumers.
- `BlockStoreAppend` reads as "a BlockStore that also supports appends" per spec § Decision 2 — the embedding makes the relationship explicit. Naming intent locked.

</specifics>

<deferred>
## Deferred Ideas

- **Online migration via dfsctl REST** — explicitly deferred (D-02 chose offline). If operators later need remote-triggered migration, revisit in v0.17+ with a `dfsctl migrate-to-cas` thin wrapper that SSHs to the server and runs the offline subcommand. Not in v0.16.0.
- **Continue-on-error walks** — `ErrStopWalk` + any-error-halts (D-07) is the locked contract. GC use cases that need fault-tolerant sweeps will handle their own retry/skip in the callback.
- **Hash echoed in `Meta`** — explicitly chose minimal `{Size, LastModified}` (D-08). If a future caller needs hash-verification during Walk without re-keying, add `Meta.VerifyHash() error` method on a backend-specific extension type, not on the shared struct.
- **Single-RunSuite conformance API** — chose two top-level funcs (D-09). If a future backend wants opt-in feature flags (e.g., "supports concurrent Put"), add `BlockStoreOptionalConformance(t, factory, feat)` rather than refactoring the base.
- **Config flag `migration_completed`** — chose sentinel file (D-10). Footgun ruled out: file requires migration tool to write, can't be hand-flipped.
- **`LocalStoreAdmin` separate interface** for lifecycle methods (`SetRetentionPolicy`, `Stats`, etc.) — captured under Claude's Discretion; researcher decides if narrowed `LocalStore` cleanly splits.
- **macOS-specific mmap unlink-race investigations** — moot post-Phase-16 already. No carry-over.
- **Cold-cache benchmarks** — deferred from Phase 16 to v0.17+. Phase 17 keeps warm-cache D-06 gate at ≤1.02 vs Phase 16 baseline.

### Phase 17 → Phase 18 carry-overs (added during revision)
- **Engine-LocalStore admin-superset methods kept transitionally:** `ReadAt`, `WriteAt`, `Flush`, `IsBlockLocal`, `GetBlockData`, `WriteFromRemote`, `DeleteAllBlockFiles`, `DeleteAppendLog` remain on the narrowed `LocalStore` interface (and on `*FSStore`) through Phase 17 and ship in the mega-PR. Engine consumes them at: `engine.go:147,320,423,635,800,828`; `fetch.go:140,155,192,302,420,482`; `syncer.go:381`; `upload.go:168`; `dedup.go:248`. Plan 17-04 was originally written to delete these from the interface but Phase 17 cannot complete the engine-consumer rewrite within D-01's atomic-merge constraint (every commit must be `go build ./...`-clean). Phase 18's Syncer simplification naturally rewrites these sites onto `BlockStore.Put/Get/Walk` — that's the right place to land the deletion. The narrowed `LocalStore` therefore remains a strict admin-superset of `BlockStoreAppend` through Phase 17, with the legacy methods carrying a `// Deprecated: removed in Phase 18` godoc tag for grep-detection at the start of Phase 18.

</deferred>

---

*Phase: 17-unified-blockstore*
*Context gathered: 2026-05-20*
