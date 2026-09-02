# Audit Report — dittofs (4ec814bc2)
Stack: Go (module github.com/marmos91/dittofs, part of the `dfs` server binary — pkg/block/engine has no build root or deploy surface of its own; it is a library package compiled into cmd/dfs). Concurrency via stdlib sync/atomic/context, golang.org/x/sync/errgroup, a hand-rolled resizable dynamicSemaphore (x/sync/semaphore.Weighted can't resize live). Content hashing via lukechampine.com/blake3. Local persistence via pkg/block/journal (Badger-backed log-blob substrate) and pkg/block/local; durable tier via pkg/block/remote (S3-compatible). Standard `testing` package, table-driven tests, no test framework/mocking library observed. No HTTP/RPC framework in this package — it is the data-plane engine consumed by pkg/controlplane/runtime/shares/ (construction) and internal/adapter/{nfs,smb,common}/ (call sites), which own the actual protocol/API surfaces. · Areas: 8 · Findings: 1 HIGH / 13 MED / 52 LOW

## Summary by dimension

| Dimension | HIGH | MED | LOW |
|---|---|---|---|
| bugs | 0 | 6 | 4 |
| security | 0 | 0 | 0 |
| slop | 0 | 1 | 6 |
| perf | 0 | 2 | 10 |
| structure | 1 | 4 | 18 |
| bloat | 0 | 0 | 6 |
| comments | 0 | 0 | 8 |

## Summary by area

| Area | Findings |
|---|---|
| composition-lifecycle | 8 |
| cross | 2 |
| gc-mark-sweep-compaction | 15 |
| manifest-check-repair | 4 |
| offline-readiness | 3 |
| read-path-cold-fetch | 6 |
| reclaim-reconcile-audit | 10 |
| syncer-upload-health | 9 |
| write-path-carve | 9 |

## Findings

### [LOW] sweepByWalk and sweepFromSyncedIndex duplicate the same end-of-scan bookkeeping block verbatim · `bloat` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/gc.go:709`
- **What:** gc.go:709-717 (sweepByWalk) and gc_sweep_index.go:127-138 (sweepFromSyncedIndex) both end w/ identical seq: lock statsMu, fold `scanned` into stats.ObjectsScanned, unlock, then if options.ProgressCallback != nil, lock again, snapshot `*stats`, unlock, invoke callback.
- **Why it matters:** copy-paste of non-trivial lock/unlock/lock/unlock bookkeeping across two sweep kernels that already share other helpers (newSweepErrorRecorder, recordDryRunCandidate) for exactly this reason — a change to progress-snapshot logic must land in two places or drifts silently.
- **Fix:** factor into one helper `finishSweepScan(stats *GCStats, mu *sync.Mutex, scanned int64, options *Options)`, call from both kernels.
- **Verified:** CONFIRMED. gc.go:708-717 and gc_sweep_index.go:126-138 byte-identical (one comment differs). Both prod-reachable: CollectGarbage calls sweepByWalk (gc.go:481) and sweepFromSyncedIndex (gc.go:497).

### [LOW] Dangling '(subsumes A6)' reference with no meaning anywhere in the repo · `comments` · area: read-path-cold-fetch
- **Where:** `pkg/block/engine/fetch.go:236`
- **What:** comment `// Legacy path deleted (subsumes A6). Any` — "A6" unresolved anywhere else in repo.
- **Why it matters:** dangling pointer to an external plan/decision doc not in repo; adds zero info over "Legacy path deleted".
- **Fix:** drop `(subsumes A6)` fragment.
- **Verified:** CONFIRMED. `grep -rn 'A6'` over pkg/ hits only this line + unrelated hex literals. Reachable via dispatchRemoteFetch (Syncer). CLAUDE.md forbids decision IDs in source comments.

### [LOW] Garbled comment punctuation ("GCStateRoot.: without this") · `comments` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/gc.go:55`
- **What:** doc comment on gcRootLocks: "...share a GCStateRoot.: without this, two concurrent calls..." — period immediately followed by colon.
- **Why it matters:** malformed punctuation breaks sentence, reader has to reparse; obscures intended meaning.
- **Fix:** replace ".: " with ": " → "...share a GCStateRoot: without this, two concurrent calls...".
- **Verified:** CONFIRMED gc.go:54-55. `.: ` artifact present verbatim; same comment also has mangled tail ("violation by data path"). gcRootLocks used by CollectGarbage.

### [LOW] Garbled comment punctuation ("would also work).: included") · `comments` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/gc.go:200`
- **What:** RemoteEndpointID doc: "...for S3 a bucket/prefix would also work).: included in the engine's start/complete log lines..." — same `.: ` glue, second clause has no subject.
- **Why it matters:** same class as gc.go:55 — garbled punctuation, misleading rather than descriptive.
- **Fix:** replace ").: " with ") — " or split into two sentences.
- **Verified:** CONFIRMED gc.go:198-204, `.: ` artifact present verbatim. Reachable: Options.RemoteEndpointID set at runtime/blockgc.go:109,:310 (non-test), consumed gc.go:457/464/468/519 via collectGarbage; blockgc.go:397 `collectGarbageFn = engine.CollectGarbage` is prod binding. Doc-comment text only, cosmetic.

### [LOW] Garbled comment punctuation + missing period in GCState.Add doc · `comments` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/gcstate.go:111`
- **What:** Add() doc: "...hash are no-ops at the data layer.: writes are buffered through a Badger WriteBatch and flushed every gcAddBatchSize hashes FlushAdd() forces a flush..." — `.: ` glitch plus missing period before "FlushAdd()" runs two sentences together.
- **Why it matters:** reads as one ungrammatical run-on; obscures FlushAdd() as a distinct operation from the batching just described.
- **Fix:** reflow to "...no-ops at the data layer: writes are buffered through a Badger WriteBatch and flushed every gcAddBatchSize hashes. FlushAdd() forces a flush so callers can rely on Has() seeing every preceding Add()."
- **Verified:** CONFIRMED gcstate.go:110-114, both defects present (`.: ` artifact + missing period before "FlushAdd()"). gcstate.go:61 is a separate correct sentence, not a fix of this one. Reachable: NewGCState (gc.go:432), FlushAdd (gc.go:599), Add via markPhase inside collectGarbage.

### [LOW] Inline lock comment duplicates the gcRootLocks doc comment near-verbatim · `comments` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/gc.go:394`
- **What:** comment at collectGarbage's rootLock acquisition (gc.go:394-399) restates almost word-for-word the package-level doc on `gcRootLocks` (gc.go:54-59), a few dozen lines above in same file.
- **Why it matters:** redundant restatement adds no new info; maintenance hazard — the two copies already differ in wording (Run A/Run B vs one run/the other; "sweep" vs "delete") and can drift further.
- **Fix:** trim inline comment to a one-line pointer, e.g. "// serializes against other CollectGarbage runs sharing this GCStateRoot — see gcRootLocks doc.", keep full rationale only on the gcRootLocks declaration.
- **Verified:** CONFIRMED duplication (gc.go:54-59 vs gc.go:394-399), already drifted in wording; `.: ` artifact survives only in the var copy. Inline copy's only non-redundant line: "Lock is acquired before any disk-state work and released on return". Reachable: collectGarbage is body of CollectGarbage (gc.go:335) / CollectGarbageLocal (gc.go:348), bound prod at runtime/blockgc.go:167,:397.

### [LOW] BlocksCached stat is documented as a live signal but is structurally always zero · `bloat` · area: composition-lifecycle
- **Where:** `pkg/block/engine/stats.go:18`
- **What:** `BlockStoreStats.BlocksCached` doc says lets operators see local-tier read-cache vs write-side split. `classifyBlocks` never increments it (own ponytail comment at stats.go:219-223 admits distinction "no longer observable and stays zero"). Exposed as `blocks_cached` JSON, printed by `dfsctl store block stats`.
- **Why it matters:** doc promises a signal impl admits elsewhere it can never produce; permanent-0 counter shown to operators as meaningful.
- **Fix:** drop field, or match `LocalMemMax`'s honest "always 0, kept for wire compat" treatment (stats.go:29-32).
- **Verified:** CONFIRMED. Only write site (classifyBlocks) never touches it. Aggregation `runtime/shares/blockstore_ops.go:301` (+= 0). CLI row at `cmd/dfsctl/commands/store/block/stats.go:77`.

### [LOW] MetadataReconciler/MultiShareReconciler split is speculative — weaker interface never satisfied alone · `bloat` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/gc.go:291`
- **What:** `CollectGarbage`/`CollectGarbageLocal`/`markPhase` take `MetadataReconciler`, then type-assert to `MultiShareReconciler` via `sharesForReconciler` (gc.go:608-616). Miss → nil shares → `markPhase` hard-aborts ("reconciler reports zero shares", gc.go:538). Only implementers repo-wide: `perRemoteReconciler` (runtime/blockgc.go:249, prod) and `gcMSReconciler` (gc_test.go:52) — both satisfy the stronger interface. `ok==false` branch never tested.
- **Why it matters:** weaker param type buys nothing; converts compile-time contract into runtime fail-closed abort in highest-consequence file.
- **Fix:** change 3 signatures to `MultiShareReconciler` directly; drop/panic the assertion fallback; drop separately-exported `MetadataReconciler` if unused elsewhere.
- **Verified:** CONFIRMED. No MetadataReconciler-only value ever passed.

### [LOW] Duplicated atomic-write-JSON-to-tmp-then-rename instead of shared helper · `bloat` · area: reclaim-reconcile-audit
- **Where:** `pkg/block/engine/audit_state.go:256`
- **What:** `persistAuditLastRun` reimplements `PersistLastRunSummary`'s (gcstate.go:302) MkdirAll(0o755)→MarshalIndent→WriteFile(tmp,0o644)→Rename→remove-tmp-on-failure sequence. Own doc admits it "matches the engine.PersistLastRunSummary contract."
- **Why it matters:** two copies of same atomic-write pattern to keep in sync.
- **Fix:** factor shared `atomicWriteJSON(dir, name string, v any) error`; `PersistLastRunSummary` takes concrete `GCRunSummary` so needs signature change too (`any` param) — ~15 lines either way, low value.
- **Verified:** CONFIRMED, both reachable in prod (audit via runtime/blockaudit.go:53, GC via gc.go:436/471/507). Low value.

### [LOW] Stale comment references non-existent function ensureUploadLimiter · `comments` · area: syncer-upload-health
- **Where:** `pkg/block/engine/syncer.go:120`
- **What:** comment says uploadLimiter "Lazily created by ensureUploadLimiter." No such function anywhere (grep = 0). Actually eager: `newDynamicSemaphore(startWindow)` in `NewSyncer` at syncer.go:217.
- **Why it matters:** misleads grep for lazy-init behavior that doesn't exist.
- **Fix:** rewrite comment to describe eager construction in NewSyncer.
- **Verified:** CONFIRMED. Field live prod (carve_dispatch.go:104-115, syncer.go:799).

### [LOW] Stale comment references non-existent function carveChunkBytes · `comments` · area: syncer-upload-health
- **Where:** `pkg/block/engine/syncer.go:277`
- **What:** `recomputeCarveActive` doc names fallback path "carveChunkBytes" — 0 matches repo-wide.
- **Why it matters:** misleads reader locating the described fallback.
- **Fix:** name real hash-keyed fallback fn, or drop parenthetical.
- **Verified:** CONFIRMED. recomputeCarveActive prod-called from NewSyncer / setRemoteBlockStore.

### [LOW] Stale comment references non-existent function carveFlush (2 sites in this file) · `comments` · area: syncer-upload-health
- **Where:** `pkg/block/engine/syncer.go:117`
- **What:** comments at 117 (uploadLimiter doc) and 852 (SyncNow doc) say serialization happens "in carveFlush" — no such function exists (grep = 0, only carveMu/carveDispatcher/Carve are real). Line 392 Flush doc correctly says "carveMu", not carveFlush — claim of a 3rd site there is wrong, only 2 sites in this file.
- **Why it matters:** dead-symbol reference repeated; real mechanism (journal.Carve/carveMu) left unnamed.
- **Fix:** replace "carveFlush" with real call path at both sites.
- **Verified:** CONFIRMED but count corrected: 2 sites (117, 852), not 3 — line 392 is clean.

### [LOW] blockRefHashes doc comment claims it feeds OnRead's hint API; only feeds InvalidateFile · `slop` · area: read-path-cold-fetch
- **Where:** `pkg/block/engine/read_internal.go:13`
- **What:** doc says extracts ContentHash "for OnRead's hint API." Sole call site: readwrite.go:386, `InvalidateFile(payloadID, blockRefHashes(blocks))`. Every prod OnRead call passes nil hashes (readwrite.go:95,244,337,390; flush.go:246).
- **Why it matters:** describes caller/API that doesn't exist for this helper.
- **Fix:** rewrite doc to name `Cache.InvalidateFile`; consider moving helper beside that call site.
- **Verified:** CONFIRMED, single call site is InvalidateFile not OnRead.

### [LOW] sweepByWalk recomputes grace cutoff per-object instead of hoisting once · `perf` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/gc.go:680`
- **What:** `meta.LastModified.After(snapshotTime.Add(-gracePeriod))` recomputed inside per-object Walk callback. Sibling `sweepFromSyncedIndex` (gc_sweep_index.go:47) hoists `graceCutoff := snapshotTime.Add(-gracePeriod)` once before its loop.
- **Why it matters:** repeated arithmetic across a Walk spanning millions of local chunks; asymmetry with correctly-hoisted sibling shows oversight.
- **Fix:** hoist `graceCutoff` once before `store.Walk`, matching sweepFromSyncedIndex.
- **Verified:** CONFIRMED. Non-allocating so real perf delta is micro; value is symmetry with sibling kernel.

### [LOW] blockRefHashes misplaced in read-path file; doc names wrong caller · `structure` · area: read-path-cold-fetch
- **Where:** `pkg/block/engine/read_internal.go:15`
- **What:** defined in read-path file, only called from write/delete-path (readwrite.go:386, inside `Store.Delete`). Doc says "for OnRead's hint API" but OnRead never receives it (nil everywhere).
- **Why it matters:** one-concern-per-file violated; doc drift same class as elsewhere in package.
- **Fix:** move next to InvalidateFile's call site (readwrite.go or cache.go); fix doc to name InvalidateFile.
- **Verified:** CONFIRMED. Store.Delete is NFS REMOVE/SMB delete path — reachable prod.

### [LOW] SyncQueue.downloadWorker takes context.Context and drops it · `structure` · area: syncer-upload-health
- **Where:** `pkg/block/engine/sync_queue.go:188`
- **What:** `downloadWorker(_ context.Context, id int)` — param named `_`, never read. Call site sync_queue.go:91 passes real ctx. Body uses `q.workerCtx` (captured at Start, sync_queue.go:77-79) instead.
- **Why it matters:** looks load-bearing but isn't; misleads editor into thinking passing a different ctx changes behavior.
- **Fix:** drop param — `func (q *SyncQueue) downloadWorker(id int)`, call site `go q.downloadWorker(i)`.
- **Verified:** CONFIRMED. Reachable via q.Start(ctx) in Syncer.Start ← engine.go:265.

### [LOW] Syncer.bs is a full *Store back-reference used for one narrow purpose · `structure` · area: syncer-upload-health
- **Where:** `pkg/block/engine/syncer.go:424`
- **What:** `dataplaneMetrics()` reaches through `m.bs` (full `*Store` field, syncer.go:70) just to read `bs.metrics.Load()`. Field comment (62-69) justifies whole-Store back-pointer via a `bs.cache`/InvalidateFile call site that no longer exists in the package.
- **Why it matters:** bidirectional coupling, unbounded reach through bs vs. what's actually needed; stale justification comment.
- **Fix:** replace `bs *Store` with narrow interface/setter for the metrics read (e.g. `SetMetricsRecorder(DataplaneMetrics)`); keep cache-invalidation need on its own seam.
- **Verified:** CONFIRMED. `.bs` used only at syncer.go:425,428 + assignment engine.go:207.

### [LOW] Compaction's per-item enumeration callbacks skip ctx cancellation check · `structure` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/compaction.go:143`
- **What:** `EnumerateSynced` callback (143-148) and `WalkBlockRecords` callback (155-162) never check `ctx.Err()`; only outer candidate loop (167-169) does. Sibling sweep kernels (gc.go:665-667, gc_sweep_index.go:51-53) check inside their per-item callback.
- **Why it matters:** shutdown mid-compaction-scan keeps churning instead of returning promptly; inconsistent with package's own convention.
- **Fix:** add `if err := ctx.Err(); err != nil { return err }` at top of both callbacks.
- **Verified:** CONFIRMED. Reachable via CompactBlocks ← runtime/blockgc.go:596. Impact bounded by whether underlying store query honours ctx.

### [LOW] gcRootLock registry lives inline in gc.go instead of own file · `structure` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/gc.go:76`
- **What:** `gcRootLocksMu`/`gcRootLocks`/`gcRootLock`/`acquireGCRootLock`/`releaseGCRootLock` (~52 lines, gc.go:76-127) is a generic per-path refcounted-mutex registry, unrelated to gc.go's own package doc. Has dedicated `gc_root_lock_test.go` but no matching `gc_root_lock.go`, unlike `gc_block.go`/`gc_sweep_index.go` which are properly split.
- **Why it matters:** violates one-concern-per-file; gc.go already largest file in set (876 lines).
- **Fix:** move type + 3 funcs into new `gc_root_lock.go`, no behavior change.
- **Verified:** CONFIRMED. `wc -l gc.go` = 876. Reachable via acquireGCRootLock at gc.go:400 in CollectGarbage. Pure mechanical move.

### [LOW] ReconcileMetaView.GetLocator is dead surface for both in-scope consumers · `structure` · area: reclaim-reconcile-audit
- **Where:** `pkg/block/engine/reconcile.go:28`
- **What:** `ReconcileMetaView` declares `GetLocator`; `Reconcile()` never calls it, `ReclaimMetaView` embeds it and never calls it either. Sole caller is `CompactMetaView` (compaction.go:237) via embedding. Doc comment overclaims scope ("the read-only per-share metadata surface the reporter consumes").
- **Why it matters:** every implementer/fake forced to carry a method neither real caller invokes; violates minimize-interface-surface.
- **Fix:** split `GetLocator` into its own tiny interface embedded only by `CompactMetaView` in compaction.go; leave `ReconcileMetaView` with just `EnumerateSynced`+`WalkBlockRecords`.
- **Verified:** CONFIRMED, but method is live in prod via CompactBlocks ← runtime/blockgc.go:596 — misplaced, not dead code.

### [LOW] Ad hoc error strings instead of package sentinel errors · `structure` · area: reclaim-reconcile-audit
- **Where:** `pkg/block/engine/audit_state.go:103`
- **What:** `AuditRefcounts` returns `errors.New("audit-refcounts: metadata store is nil")` (103) and `errors.New("audit-refcounts: share is empty")` (106) instead of sentinel vars. Package already has `ErrClosed`/`ErrStoreClosed` (types.go:9,17) as the sentinel convention.
- **Why it matters:** callers can only string-match to distinguish these preconditions; errors.Is unavailable.
- **Fix:** declare `ErrNilMetadataStore`/`ErrEmptyShare` sentinels next to `ErrClosed` in types.go, return directly.
- **Verified:** CONFIRMED, reachable via runtime/blockaudit.go:26 ← handlers/blockstore_audit.go:72 ← dfsctl. Low value: no caller branches on these today.

### [LOW] ResetLocalState deletes files sequentially, one fsync each, defeating the journal's own commit batching · `perf` · area: write-path-carve
- **Where:** `pkg/block/engine/flush.go:307`
- **What:** ResetLocalState loops sequential `bs.local.Delete` per payloadID, no concurrency. journal.Store.Delete → appendTombstone → groupCommit fsyncs per call; groupCommit only batches concurrent callers hitting the barrier together, never engaged by a sequential loop.
- **Why it matters:** Snapshot-restore on remote-backed share w/ many resident files = N sequential fsyncs (only path reaching ResetLocalState: snapshot.go:1509). Minutes of restore time at scale when group-commit machinery already exists to collapse it.
- **Fix:** Fan deletes out with bounded concurrency (errgroup + worker cap, mirror carvePass's uploadLimiter) so same-shard deletes overlap and ride groupCommit's single fsync.
- **Verified:** CONFIRMED. flush.go:307-311 sequential; journal Delete (store.go:759-776) always appendTombstone→groupCommit (segment.go:466-471); sequential caller always becomes leader, pays full fsync. Rare operator path, no correctness impact.

### [LOW] applyPayloadRepairs re-sorts entire covered-extent list on every repair action — O(n²log n), unnecessary sort · `perf` · area: manifest-check-repair
- **Where:** `pkg/block/engine/manifest_repair.go:280`
- **What:** `covered = coalesceExtents(append(covered, ...))` runs full sort.Slice+merge over ALL covered extents per action, not once. R actions × C covered = O(R*C log C). overlapsCovered (line 208), the only consumer, does unconditional linear scan, never relies on sort order.
- **Why it matters:** Sort is pure waste even ignoring loop repetition — overlapsCovered behaves identically sorted or not. Repeating per action = redundant recompute on repair hot path, holds metadata tx open longer.
- **Fix:** Drop resort: `covered = append(covered, [2]uint64{a.Offset, a.Offset+uint64(a.Size)})`. If overlapsCovered ever becomes order-dependent, coalesce once after the whole loop.
- **Verified:** CONFIRMED. Reachable via CheckManifests→repairPayload→applyPayloadRepairs, ApplyRepairs settable from REST handler. Downgraded HIGH→LOW: actions capped ≤1000 (maxManifestCheckFindings), rare operator path not a hot loop.

### [LOW] AuditRefcounts re-audits every hardlink to the same file — redundant ListFileChunks per link, not per unique payload · `perf` · area: reclaim-reconcile-audit
- **Where:** `pkg/block/engine/audit_state.go:238`
- **What:** walkAuditShareFiles calls GetFile + auditFileManifest (→ListFileChunks) once per dirent. No visited-handle/payloadID dedup. A hardlinked file appears as N dirents sharing one payloadID/FileChunk row-set; audit reruns full reconciliation per link.
- **Why it matters:** Backup/snapshot-style trees with heavy hardlink use scale audit cost with total link count not unique file count — pure redundant recompute, each rerun a full metadata round trip + O(blocks) map rebuild.
- **Fix:** Track visited-handle/payloadID set in AuditRefcounts/walkAuditShareFiles; skip GetFile/auditFileManifest for already-audited handle, still count per-link TotalFiles from cached result.
- **Verified:** CONFIRMED. Reachable via dfsctl store block audit→BlockStoreAuditRefcounts. Downgraded HIGH→LOW: perf only, on-demand operator command, bites only hardlink-heavy trees.

### [LOW] WarmAll silently drops unplaceable manifest rows with zero signal · `bugs` · area: read-path-cold-fetch
- **Where:** `pkg/block/engine/warm.go:116`
- **What:** `absOff, ok := block.ParseChunkOffset(fb.ID); if !ok { continue }` drops row silently — no log, no counter. `total` computed AFTER filter so progress()/WarmResult look like a clean complete run even with excluded rows.
- **Why it matters:** Every other consumer of this condition (findRowCoveringOffset→ErrManifestInconsistent, resolveNextChunkStart, DataExtents) treats it as reportable; WarmAll swallows it. WarmAll exists to pre-warm before going remote-unreachable — a skipped range is never fetched, caller has no way to know it wasn't warmed.
- **Fix:** Count/log skipped unplaceable rows (Warn/Error w/ payloadID+fb.ID, or surface via WarmResult) instead of bare `continue`, mirror SeedColdFromManifest's report.unplaceable pattern.
- **Verified:** CONFIRMED at warm.go:104-107. Reachable: shares/blockstore_ops.go:415 (dfsctl share warm). Downgraded MED→LOW: uncovered read still refuses via ErrManifestInconsistent (not silent data loss) — this is observability gap only.

### [LOW] Health-monitor ticker never switches to fast unhealthy-probe cadence when remote already down at Start() · `bugs` · area: syncer-upload-health
- **Where:** `pkg/block/engine/sync_health.go:140`
- **What:** Start()'s eager synchronous probe can set healthy=false before monitorLoop even runs. monitorLoop unconditionally starts ticker at hm.healthyInterval (30s default). Only reset-to-fast site is guarded by `newCount>=threshold && hm.healthy.Load()`; if healthy already false at loop start, that gate never fires — ticker stuck slow for the whole outage.
- **Why it matters:** HealthMonitor gates every fetch/carve/evict decision. Boot-time outage (misconfigured endpoint, network not up yet) delays recovery detection by up to ~25s (healthyInterval-unhealthyInterval). Untested: every test config sets both intervals equal/close, so this branch's correctness has never been exercised.
- **Fix:** Seed ticker from current health state: `interval := hm.healthyInterval; if !hm.healthy.Load() { interval = hm.unhealthyInterval }`. Add regression test w/ distinct intervals starting already-failed, assert recovery within ~one unhealthyInterval.
- **Verified:** CONFIRMED. Start (sync_health.go:79-100) sets healthy=false before monitorLoop; Reset gated by `&& hm.healthy.Load()` (164-171) never true once already false. Reachable via syncer.go:652/691 on real remote HealthCheck. Downgraded MED→LOW: only delays detection ~25s, no correctness impact.

### [LOW] Reconcile() scans in the UNSAFE order — opposite of ReclaimRecords()'s documented TOCTOU-safe order · `bugs` · area: reclaim-reconcile-audit
- **Where:** `pkg/block/engine/reconcile.go:169`
- **What:** Reconcile builds refSet via EnumerateSynced FIRST then classifies via WalkBlockRecords SECOND. ReclaimRecords (reclaim.go:129-141) does the opposite, with a comment explaining a commit landing between the two scans would misclassify a live block as leaked if live-set were built first (the #1525 TOCTOU). reconcile.go has no comment/test addressing the identical race in its own inverted order.
- **Why it matters:** A commit landing between Reconcile's two scans is invisible to refSet(T1) but visible to WalkBlockRecords(T2) → fresh healthy record misclassified as leaked; ReconcileReport (blockgc_reconcile_report.go:74) runs unlocked, fully concurrent with live commits — race window is real. Doesn't itself delete (ReclaimRecords re-derives correctly), but operator-facing leaked-count report can misstate under concurrent write load.
- **Fix:** Swap scans to match reclaim.go: WalkBlockRecords first into candidate map, EnumerateSynced second to prune. Add raceView-style regression test mirroring TestReclaimRecords_CommitDuringScanNotDeleted.
- **Verified:** CONFIRMED. reconcile.go:169-193 vs reclaim.go:129-141 (13-line comment on correct order). Unlocked at blockgc_reconcile_report.go:74. Downgraded MED→LOW: Reconcile mutates nothing, blast radius is a misstated count only.

### [LOW] doc.go's package overview describes a ReadBuffer/Prefetcher API that no longer exists — replaced by Cache · `slop` · area: composition-lifecycle
- **Where:** `pkg/block/engine/doc.go:16`
- **What:** doc.go:16-36 describes "ReadBuffer" (NewReadBuffer(maxBytes)) and separate "Prefetcher" type. Neither exists — cache.go's single `Cache`/`NewCache` folded both together per engine.go:273-279. `NewReadBuffer` referenced only in doc.go:26 and engine.go:56, dead identifier.
- **Why it matters:** Reader grepping ReadBuffer/Prefetcher/NewReadBuffer to understand the cache path finds nothing — package's own design-rationale doc points at dead names instead of Cache/NewCache.
- **Fix:** Rewrite doc.go's cache section around Cache/NewCache (seqThreshold=3, maxPrefetchDepth=8 adaptive doubling, reqQueueSize=64 non-blocking submit — behavior facts still accurate). Fix NewReadBuffer ref in engine.go:56.
- **Verified:** CONFIRMED. Repo-wide grep for NewReadBuffer: 2 hits, both comments (doc.go:26, engine.go:56). No ReadBuffer/Prefetcher type declared anywhere. Documentation drift only, no behavior.

### [LOW] WriteAt doc comment cites a function that does not exist and a hook the package's own other comment says was removed · `slop` · area: write-path-carve
- **Where:** `pkg/block/engine/readwrite.go:101`
- **What:** WriteAt doc + inline comment (lines 67-77, 96-106) describe canonical []ChunkRef projection happening in "the post-Flush hook", citing `Syncer.snapshotChunkRefs` — zero grep matches anywhere, function doesn't exist. Also cites coordinator's `PersistFileChunks` — zero production call sites (test fakes only). coordinator.go:85-90 already states "the engine itself no longer drives it from a local-store chunk-lifecycle hook, of which none remain" — directly contradicts readwrite.go's claim. Real path: blocksink.go CommitBlock→metadata.CommitCarvedChunks/DefaultCommitBlock during carve.
- **Why it matters:** Hallucinated/stale API ref on exported, heavily-relied-on write path. Maintainer trusting this would chase dead machinery (snapshotChunkRefs, PersistFileChunks) instead of the actual path when diagnosing a residency-projection bug.
- **Fix:** Rewrite readwrite.go:67-77/96-106 to describe actual mechanism: FastCDC chunking + FileChunk rows + Blocks/ObjectID projection happen synchronously inside carve BlockSink's commit tx (blocksink.go CommitBlock→metadata.DefaultCommitBlock), not a post-Flush hook. Drop dead reference.
- **Verified:** CONFIRMED. snapshotChunkRefs: single comment hit, nothing else. `.PersistFileChunks(` outside tests: zero call sites. coordinator.go:88-90 self-contradicts readwrite.go. Comment on production-reachable Store.WriteAt.

### [LOW] Flush() comment claims a metadata quiesce runs on the default-policy early-return path; code does nothing of the sort · `slop` · area: write-path-carve
- **Where:** `pkg/block/engine/flush.go:39`
- **What:** Comment says branch "still performs the per-payload FileChunk metadata quiesce that syncer.Flush would ... only the remote carve drain is skipped." Code immediately following (42-44) is a bare early return, no Carve call, nothing manifest-related. The actual quiesce (`m.local.Carve(..., Force: true)`) lives inside Syncer.Flush (syncer.go:415), skipped entirely by this branch.
- **Why it matters:** Comment describes behavior code doesn't implement. Misleads on restart-recovery safety: default async-remote policy leaves manifest projection to unforced background carve dispatcher (age/size gated), not quiesced here as claimed — wrong premise for diagnosing a later residency-truth bug.
- **Fix:** Either (a) call `bs.local.Carve(ctx, journal.CarveOptions{FileID: journal.FileID(payloadID), Force: true})` before early return, or (b) correct comment to say quiesce is NOT performed here, left to background dispatcher, note the staleness window.
- **Verified:** CONFIRMED. flush.go:39-41 claim vs 42-44 bare return. Manifest work is syncer.go:415's forced carve, skipped by this branch. LOW: comment-only, no behavior change implied — default async policy skip is the intended #1621 fix.

### [LOW] HealthMonitor ticker never switches to fast unhealthy-probe interval when eager start-up probe fails (duplicate angle) · `slop` · area: syncer-upload-health
- **Where:** `pkg/block/engine/sync_health.go:140`
- **What:** monitorLoop unconditionally creates ticker at hm.healthyInterval regardless of current health state. Reset-to-fast only inside `newCount>=threshold && hm.healthy.Load()` — never fires if Start()'s eager probe already set healthy=false. Doc comment (169-170) says "Switch to faster probing for quicker recovery detection" — doesn't happen for a startup outage.
- **Why it matters:** Same circuit breaker every evict/trust-remote decision depends on. Stuck at slow interval = recovery detection up to 6x slower after any start-of-process outage. Untested: fastHealthConfig() sets both intervals to identical 10ms, so this code path never distinguished under test — a guard that's never fired.
- **Fix:** `interval := hm.healthyInterval; if !hm.healthy.Load() { interval = hm.unhealthyInterval }; ticker := time.NewTicker(interval)`. Test w/ distinct intervals + probe failing from first call, assert unhealthy cadence.
- **Verified:** CONFIRMED sync_health.go:140/171/84-89. Reachable via syncer.go:691/652 production remote-backed share. Downgraded MED→LOW: only affects probe cadence (recovery in ≤30s vs ≤5s), no durability/data path wrong.

### [LOW] Compaction candidate filter excludes exactly the husk blocks its own crash-recovery path exists to clean up · `slop` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/compaction.go:158`
- **What:** `if lb > 0 && rec.Length > 0 && float64(lb)/float64(rec.Length) < opts.LiveRatio` — `lb==0` (zero live locators) is excluded from candidates by the `lb>0` conjunct, so compactOneBlock never runs for a full husk. Doc comment (40-42) claims "A re-run converges: ... compactOne finds nothing to move and simply deletes the husk" but that re-run can never reach compactOneBlock for lb==0 blocks — both husk-cleanup branches (len(moveable)==0 at :257, ErrChunkNotFound at :196) are unreachable via CompactBlocks' own loop for this case.
- **Why it matters:** Compaction's documented crash-recovery guarantee (step2/step3 partial-crash leaves stale LiveChunkCount>0, zero real locators) is false — never self-heals via compaction. compaction_test.go only covers partial-dead (lb>0); husk path untested/unreachable.
- **Fix:** Drop `lb > 0` conjunct: `if rec.Length > 0 && float64(lb)/float64(rec.Length) < opts.LiveRatio`. Zero lb ratios to 0, already `< opts.LiveRatio`. Add test seeding LiveChunkCount>0/zero-locators record, assert CompactBlocks reaps it.
- **Verified:** CONFIRMED compaction.go:158/40-42. Reachable blockgc.go:596. Downgraded MED→LOW: refuted "permanently strands" claim — ReclaimRecords (reclaim.go:151-153) has NO lb>0 gate, reaps this husk on its own separately-scheduled pass (blockgc_reconcile_reclaim.go:122), so it's a delay not permanence.

### [LOW] chunkWindowResolver rescans full row list per offset on non-badger backends — O(K*N) walk instead of O(N log N) · `perf` · area: read-path-cold-fetch
- **Where:** `pkg/block/engine/read_internal.go:217`
- **What:** rows cached once by list(), but coveringRow falls back to findRowCoveringOffset (O(N) linear) and nextPlaceableStart falls back to minStartAfter (O(N)) on every call for any store not implementing chunkAtOffsetResolver/chunkAtOrAfterOffsetResolver (badger only). collectCoveringChunks calls covering() once per chunk in window → O(K*N) total.
- **Why it matters:** Row list already fully materialized in list() but re-scanned from scratch every lookup instead of indexed once. WarmAll (warm.go:101-131) already does the right pattern (slices.Sort + sort.Search) for the identical successor-lookup problem; chunkWindowResolver, serving actual cold-read/prefetch on sqlite/postgres, doesn't apply it.
- **Fix:** In list(), build+cache a slice sorted by parsed absolute offset (unplaceable rows separated). Binary-search (sort.Search) in findRowCoveringOffset/minStartAfter instead of linear scan. O(N log N) once + O(K log N) lookups.
- **Verified:** CONFIRMED read_internal.go:170/229/450, fetch.go:107. Downgraded MED→LOW: pure in-memory uint64 compares on already-materialized slice (~1e6 compares, sub-ms) next to K remote GETs at hundreds of ms each — nowhere near a bottleneck; file's own "not the profiled hot path" note (:311) directionally right.

### [LOW] compactOneBlock re-fetches locators one-by-one, defeating the EnumerateSynced batching CompactBlocks just did · `perf` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/compaction.go:237`
- **What:** CompactBlocks (line 143) does one EnumerateSynced scan per share explicitly to avoid GetLocator-per-hash round trips (comment cites sqlite MaxOpenConns(1) O(N) serial cost) — but only keeps aggregated liveBytes[blockID], discards per-hash locator. compactOneBlock's moveable-selection loop then calls v.GetLocator once per chunk record in every candidate block — thousands of serial single-row queries on the same pool the batching was built to avoid.
- **Why it matters:** Self-contradicts the file's own documented rationale two paragraphs above. N candidate blocks × M chunks each = O(N*M) individual queries instead of the one sequential scan already paid for.
- **Fix:** Build hash→ChunkLocator (or hash→BlockID) map during the same EnumerateSynced pass used for liveBytes; thread into compactOneBlock so moveable loop does map lookup not GetLocator call.
- **Verified:** CONFIRMED compaction.go:143-150/237. Reachable blockgc.go:596. Downgraded MED→LOW: only over CANDIDATE blocks (already below LiveRatio, ~128 records at 8MiB/64KiB not "thousands"), background GC pass off client path; naive fix has real memory tradeoff current code deliberately avoids.

### [LOW] sweepByWalk hex-encodes every walked hash unconditionally, even on the common keep-alive path · `perf` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/gc.go:668`
- **What:** `key := h.String()` (hex.EncodeToString, fresh alloc) runs for every object before grace-window/live-set checks that usually return early. key only used on error/delete branches (addError, recordDryRunCandidate).
- **Why it matters:** sweepByWalk runs over entire local chunk namespace; per-object allocation multiplied across millions of live/kept objects for a value thrown away unused most of the time.
- **Fix:** Move `key := h.String()` down into each branch that actually needs it (addError calls, recordDryRunCandidate, delete-error path).
- **Verified:** CONFIRMED gc.go:668 vs filters at 675-687; ContentHash.String is hex.EncodeToString(h[:]) (types.go:27-29). Reachable gc.go:481. LOW not MED: small alloc next to a disk-backed gcs.Has lookup already paid per object.

### [LOW] sweepFromSyncedIndex hex-encodes every synced hash unconditionally, even on the common live-keep path · `perf` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/gc_sweep_index.go:54`
- **What:** `key := h.String()` runs for every marker EnumerateSynced yields, before syncedAt-zero/grace-window/live-set checks (present==true majority case). Same hex.EncodeToString allocation-per-call as gc.go:668.
- **Why it matters:** This is the whole synced-hash index — whole live remote object population. Computing+discarding hex string per hash on common path wastes allocation at exactly the scale (millions of markers) surrounding code was engineered to bound.
- **Fix:** Compute key lazily inside branches that use it (addError, recordDryRunCandidate) instead of unconditionally at callback top.
- **Verified:** CONFIRMED gc_sweep_index.go:54 vs early returns at 59-77. Reachable gc.go:497. LOW, same reason as sweepByWalk — dominated by per-hash gcs.Has disk-backed probe in same iteration.

### [LOW] AuditRefcounts share walk is fully sequential — no concurrency despite the package's established errgroup/semaphore pattern · `perf` · area: reclaim-reconcile-audit
- **Where:** `pkg/block/engine/audit_state.go:207`
- **What:** walkAuditShareFiles (207-249) recurses whole share tree one dir at a time; per regular file, GetFile (228) then ListFileChunks (167 via auditFileManifest 126) strictly series. No goroutines/errgroup/pool. syncer.go/fetch.go same pkg already use errgroup+dynsem for I/O-bound calls.
- **Why it matters:** 2 sync metadata round trips/file, zero concurrency → linear wall-clock w/ file count. Same shape as reconcileMetadataSizeFromJournal, measured minutes at ~10M entries. Audit tool least useful exactly when share largest.
- **Fix:** Parallelize per-dir-entry audit w/ bounded pool (errgroup + existing dynamicSemaphore, same shape as fetch.go/syncer.go); aggregate counters w/ atomics or mutex instead of closure-captured plain vars.
- **Verified:** CONFIRMED. Serial walk confirmed at 207-249. Reachable: engine.AuditRefcounts <- runtime/blockaudit.go:53 <- handlers/blockstore_audit.go:72 <- dfsctl audit cmd. No correctness impact, no spec issue — on-demand operator tool, pure-read, per-entry GetFile load-bearing (comment 220-226). MED overstated -> LOW; parallelizing adds SSI/read contention on live store for a hand-run tool.

### [LOW] manifestShortfall buffers every payload ID before processing instead of streaming inline · `perf` · area: offline-readiness
- **Where:** `pkg/block/engine/offline.go:201`
- **What:** manifestShortfall collects ALL payload IDs from EnumeratePayloads into `var payloads []string` (201-207), then second pass calls ListFileChunks/DataExtents/subtractExtents per ID (208-235). Holds full share's IDs in memory before any work starts.
- **Why it matters:** Pure sequential fold, no concurrency needed — stats.go's populateBlockCounts already calls ListFileChunks directly inside the EnumeratePayloads callback (198-206), same pkg, same fileChunkStore, proving inline is safe (badger's EnumeratePayloads runs callback inside db.View). Buffering-first pays for an O(payload count) live-string slice (multi-hundred-MB at #1891-scale file counts) and defers first result byte until enumeration fully completes, for no benefit.
- **Fix:** Move loop body (ctx.Err check, ListFileChunks, placedRanges, DataExtents, subtractExtents, confirmShortfall, accumulate) into the EnumeratePayloads callback closure, mirroring populateBlockCounts. Drops the `payloads []string` buffer.
- **Verified:** CONFIRMED. Buffer-then-loop at 200-235. Counter-pattern real (stats.go:198-206). Reachable: manifestShortfall <- shortfallMemo.get (384) <- OfflineReadiness <- runtime/offline.go:22. No correctness impact, memoized behind shortfallInterval, cost is one string slice. LOW.

### [LOW] doc.go's package doc describes a ReadBuffer/Prefetcher subsystem that pre-loads blocks; the actual Cache's loadFn is a permanent no-op miss, so nothing it describes ever fires · `structure` · area: composition-lifecycle
- **Where:** `pkg/block/engine/doc.go:18`
- **What:** doc.go:16-36 ("Read buffer and prefetch (formerly pkg/block/readbuffer)") names live types `ReadBuffer`/`Prefetcher`/`NewReadBuffer` w/ described LRU+RWMutex+adaptive-depth-prefetch behavior. None exist (zero declarations) — folded into single `Cache` type (cache.go, engine.go:107/274). Behavior claim also false: engine.go's loadByHash (290-299, ponytail-marked) is permanent no-op miss; cache.go:46-51 states plainly "no read-through: loadByHash always misses."
- **Why it matters:** Aspirational/legacy description drift (same class as legacy_migration.go:28). Top-level, most-authoritative doc comment for the pkg — engineer investigating cold-read latency reads this, believes prefetch actively populates a read cache, misdiagnoses instead of finding the actually-dead read-through path the accurate internal comments describe.
- **Fix:** Rewrite "Read buffer and prefetch" section to name `Cache` (not ReadBuffer/Prefetcher/NewReadBuffer); state plainly write-side-only today (populated via OnChunkComplete), loadByHash a permanent miss until journal local store gains hash-keyed lookup — point at engine.go's ponytail comment as authoritative status.
- **Verified:** CONFIRMED. doc.go:16-36 names ReadBuffer/NewReadBuffer/Prefetcher as live; grep zero declarations anywhere (only retrospective mentions engine.go:56/107/275, cache.go:127). loadByHash (290-299) unconditional ErrChunkNotFound; cache.go:46-51 confirms no read-through. Package live (NewCache wired engine.go:281). Doc-only drift, no runtime effect -> LOW not MED.

### [LOW] PutBlock-then-commit crash ordering is claimed safe but never exercised by a test that actually fails between PutBlock and the metadata commit · `structure` · area: write-path-carve
- **Where:** `pkg/block/engine/blocksink.go:327`
- **What:** blocksink.go:327-328 asserts "PutBlock first: a crash before the commit leaves an orphan block (GC reclaims it), never an unbacked record." blocksink_test.go's only fault-injection test (TestJournalCarveSeam_CrashMidCommitReCarveIsNoOp, line 171) uses failOnceSink whose CommitBlock (160) calls the REAL inner.CommitBlock (full PutBlock + successful DefaultCommitBlock) and injects error only AFTER real commit succeeded — tests "commit succeeded, then later failure," not "PutBlock succeeded, then metadata commit itself failed/crashed." No test makes s.rbs.PutBlock (329) succeed then s.committer.WithTransaction/DefaultCommitBlock (348) fail or crash before running.
- **Why it matters:** Per repo rule ("a guard that has never REFUSED anything is unverified, not proven"), the single ordering claim justifying safety of every carve in the write-path — the exact seam the audit brief names as priority — has zero fault-injection coverage of its own documented failure mode. Whether the orphan block is actually invisible-but-harmless is asserted, not demonstrated.
- **Fix:** Add test where PutBlock succeeds but WithTransaction/DefaultCommitBlock errors: assert (1) CommitBlock returns that error, (2) journal records stay dirty/re-carveable, (3) no FileChunk/BlockRecord rows landed, (4) re-carve after recovery works, orphaned remote object unreferenced (GC-reclaimable if plumbing reachable from this pkg's tests).
- **Verified:** CONFIRMED. blocksink.go:327-329 ordering assertion; failOnceSink (blocksink_test.go:157-168) calls REAL inner.CommitBlock then errors after — its own comment says "commits for real, then returns injected error" = crash-after-commit, not mid-commit. No stub found making PutBlock succeed then WithTransaction/DefaultCommitBlock (348) fail; stubJournalRemote.PutBlock (21) only errors unconditionally. Coverage gap, not a live defect (ordering correct in code) -> LOW.

### [LOW] ErrChunkNotFound wrap-and-log block duplicated between fetchResolvedBlock and inlineFetchOrWait, drift risk self-flagged in a comment · `structure` · area: read-path-cold-fetch
- **Where:** `pkg/block/engine/fetch.go:649`
- **What:** fetchResolvedBlock (436-451) and inlineFetchOrWait (636-648) both handle errors.Is(err, block.ErrChunkNotFound) w/ identical logger.Error("CAS object missing for live FileChunk — possible GC race or live-data-loss", block_id/store_key/hash) + near-identical fmt.Errorf wrap. inlineFetchOrWait's comment (649-654) explicitly says "Mirror the ErrChunkNotFound branch above... we MUST set completionErr to the same wrapped error the direct caller sees — otherwise the waiter receives the raw err."
- **Why it matters:** Per repo convention, hand-synced duplicate whose own comment names the exact failure mode (waiter sees different error than direct caller) is a maintenance hazard, not incidental repetition — next edit to one copy's log fields/wrap silently breaks the invariant, nothing enforces it at compile time.
- **Fix:** Extract helper `func (m *Syncer) wrapChunkNotFound(fb *block.FileChunk, storeKey string) error` doing the logger.Error + fmt.Errorf once; call from both fetchResolvedBlock and inlineFetchOrWait (assign to completionErr in latter). Removes hand-sync requirement.
- **Verified:** CONFIRMED byte-identical dup: fetch.go:436-451 and 636-648, same log fields + same wrap, comment 649-654 states hand-sync is a correctness requirement. Both production. Currently IN sync -> no behavioral defect today, maintenance hazard only -> LOW. Fix: one casMissingErr(fb, storeKey) helper.

### [LOW] HealthMonitor defaults duplicated as unnamed literals instead of reusing DefaultConfig's constants · `structure` · area: syncer-upload-health
- **Where:** `pkg/block/engine/sync_health.go:49`
- **What:** NewHealthMonitor falls back to bare literals 30*time.Second/5*time.Second/3 (50-59) when config fields unset. types.go's DefaultConfig() (154-156) independently hardcodes same 30s/3/5s trio as unnamed literals too (unlike rest of that func, which uses DefaultParallelDownloads/DefaultPrefetchBlocks/DefaultDemandFetchTimeout named consts, 146-153). Two copies, no shared source.
- **Why it matters:** Pkg's own convention (named consts w/ justifying comment for thresholds/intervals) skipped here. Changing one without other silently desyncs DefaultConfig()'s SyncerConfig{} from NewHealthMonitor's fallback — caller building SyncerConfig{} by hand gets fallback-path values, DefaultConfig() callers get types.go copy, currently identical by coincidence not definition.
- **Fix:** Add DefaultHealthCheckInterval/DefaultUnhealthyCheckInterval/DefaultHealthCheckFailureThreshold consts in types.go (same treatment as DefaultParallelDownloads), use in both NewHealthMonitor fallback and DefaultConfig().
- **Verified:** CONFIRMED. sync_health.go:50-59 bare 30s/5s/3; types.go:154-156 DefaultConfig() repeats identical values while neighbors use named consts. Reachable: NewHealthMonitor <- syncer.go:691; config via runtime.DefaultConfig() (shares/blockstore_config.go:214) and hand-built SyncerConfig{} literals both live. No functional bug today (values identical), pure drift risk. LOW not MED — no observable behavior change.

### [LOW] Drained downloads log at Error for expected shutdown-time context cancellation · `structure` · area: syncer-upload-health
- **Where:** `pkg/block/engine/sync_queue.go:266`
- **What:** drainDownloads (215-226, called from downloadWorker on stopCh close, 206-209) calls processRequest for every queued request at shutdown. ctx derives from q.workerCtx, already cancelled by Start goroutine (82-85) at this point. For TransferDownload, reaches recordResult (266-284), which does logger.Error("Transfer failed", ...) unconditionally on err != nil — no context.Canceled/ctx.Err() check. Every download pending at Stop() logs an ERROR for normal graceful shutdown.
- **Why it matters:** Violates stated log-level discipline ("expected errors log at Debug, unexpected at Error"). Shutdown-caused cancellation is expected. HealthMonitor.monitorLoop, same pkg (sync_health.go:157-160), already does `if ctx.Err() != nil { return }` before treating probe failure as real — codebase knows the distinction; SyncQueue's drain path doesn't apply it.
- **Fix:** In recordResult, check errors.Is(err, context.Canceled) (or q.workerCtx.Err() != nil) — but also check for ErrClosed (see Verified correction) — and log Debug/Info instead of Error for that case, keep Error for genuine failures.
- **Verified:** CONFIRMED w/ correction. Chain confirmed (206-209 -> drain -> processRequest -> recordResult 266-284, unconditional Error). CORRECTION: err is not context.Canceled — fetchBlock (378) -> canProcess/checkReady (syncer.go:357-367) returns ctx.Err(), but fetchBlock converts it to ErrClosed; a fix testing only errors.Is(err, context.Canceled) misses it, must also test ErrClosed. Also pollutes q.lastError/lastErrorAt w/ shutdown artifact. LOW: shutdown-only log noise, no data effect.

### [LOW] planPayloadRepairs accepts the full metadata.Store for a single IsSynced call, breaking the package's own narrow-interface convention · `structure` · area: manifest-check-repair
- **Where:** `pkg/block/engine/manifest_repair.go:107`
- **What:** planPayloadRepairs(ctx, store metadata.Store, path, payloadID string, f *metadata.File, rowIDs map[string]struct{}, unplaceable []*block.FileChunk, covered [][2]uint64, checkSynced bool) takes the entire metadata.Store interface (store.go:426-467) but body uses exactly one method: `store.IsSynced(ctx, ref.Hash)` at line 156.
- **Why it matters:** Exact anti-pattern the pkg explicitly avoids elsewhere — gc.go:264-271's SyncedHashIndex is "the narrow slice of the synced-hash marker store the GC sweep depends on... GC declares only what it uses." manifest_repair.go widens a pure planning function's (read-only path, no transaction) coupling surface to the whole store contract, against "accept interfaces, return structs" / minimal-interface convention.
- **Fix:** Accept a 1-method interface (or metadata.SyncedHashStore) in place of `store metadata.Store`: `type syncedChecker interface { IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) }`, change param type.
- **Verified:** CONFIRMED. planPayloadRepairs (107-116) takes metadata.Store, body uses only store.IsSynced (156). Reachable: sole caller repairPayload (manifest_check.go:389) <- engine.CheckManifests (221) <- runtime.CheckManifests (manifestcheck.go:48) <- integrity_scheduler.go:90 + dfsctl store check. Pkg precedent explicit (gc.go:264-271). Fix: one-method interface as param type. Pure structure, no behavior change — LOW.

### [LOW] Repairs/RepairsPlanned silently stop being exact once the global cap fills, contradicting the struct's own 'every count is exact' doc comment — and the cross-payload case is untested · `structure` · area: manifest-check-repair
- **Where:** `pkg/block/engine/manifest_check.go:385`
- **What:** ManifestCheckResult doc (81) states "Every count is exact — the per-payload detail lists and the Findings list are capped, but the totals are taken as each defect is found, before anything is dropped for display." Holds for UncoveredRanges/ClaimedUncoveredRanges/DamagedPayloads. NOT for RepairsPlanned/Applied/Skipped: repairPayload's early return (385-388) skips planPayloadRepairs entirely for any payload walked after res.Repairs already holds maxManifestCheckFindings entries — so that payload's repairable rows never count into RepairsPlanned at all, not just omitted from display. Field comment (149-150) tries to disclose this but self-contradicts ("stops planning once full" IS stopping the work, for every subsequent payload).
- **Why it matters:** Doc comments must describe current code, not aspirational (same class as confirmed legacy_migration.go:28). Operator reading RepairsPlanned would expect exactness like DamagedPayloads/UncoveredRanges, undercounting outstanding repairable damage past 1000 damaged payloads. TestRepairPlanHonoursTheReportCap only covers single-mega-payload case (exact there, capping happens after full counting inside one call) — doesn't pin the cross-payload early-return at 385-388.
- **Fix:** Either make RepairsPlanned exact past cap (count candidates even when skipping display append) or fix field doc (139) to state RepairsPlanned undercounts past cap, rewrite self-contradictory sentence (149-150). Add table-driven test w/ >1 payload spanning cap boundary to pin chosen behavior.
- **Verified:** CONFIRMED. manifest_check.go:385-388 returns before planPayloadRepairs once len(res.Repairs) >= maxManifestCheckFindings — counter not merely display-capped, contradicts struct doc :81-83. In-function trim comment (:398-401) true only within one payload; field comment :149-152 half-discloses. LOW — undercount in operator report, no data risk.

### [LOW] Duplicated scan+classify algorithm — only one copy got the TOCTOU-safe order · `structure` · area: reclaim-reconcile-audit
- **Where:** `pkg/block/engine/reconcile.go:169`
- **What:** reconcile.go builds refSet via EnumerateSynced FIRST (169-177), classifies via WalkBlockRecords SECOND (179-193). reclaim.go's near-identical classify loop for same two classes does opposite order deliberately: WalkBlockRecords first (151-157), EnumerateSynced second (166-173), w/ 18-line comment (130-145) explaining live-set-first is unsafe — a DefaultCommitBlock landing between the two scans writes record+locator atomically, so a stale live-set built first misses the new locator while the later record walk sees LiveChunkCount>0 → false orphan/leaked classification. reclaim.go carries regression test TestReclaimRecords_CommitDuringScanNotDeleted (#1525 TOCTOU). reconcile.go never got the fix.
- **Why it matters:** Two hand-copies of one algorithm instead of one shared helper — "duplicated/boilerplate hides drift bugs" class, already burned twice (#1980/#1981). ReclaimRecords' own doc says it "re-derives the classification here — never trusting a stale report," a tacit admission the two implementations are known to diverge in correctness. Fix landed in the wrong caller.
- **Fix:** Extract `classifyOrphanRecords(ctx, view ReconcileMetaView, sink func(rec block.BlockRecord, leaked bool)) error` always walking WalkBlockRecords before EnumerateSynced (safe order); both Reconcile and ReclaimRecords call it — Reconcile feeding a tally sink, ReclaimRecords a delete-candidate-map sink.
- **Verified:** CONFIRMED. reconcile.go:169-177 EnumerateSynced first, classify 179-193; reclaim.go:129-173 reverse order + 18-line hazard comment (130-145: DefaultCommitBlock writes record+locators atomically, mid-scan commit walked w/ LiveChunkCount>0 but missing from stale live set). reconcile.go has exact hazard -> false class-2 LeakedBlocks entry. Reachable: engine.Reconcile <- blockgc_reconcile_report.go:74. Severity LOW not MED: report read-only, ReclaimRecords re-derives (blockgc_reconcile_reclaim.go:122) so no deletion follows false positive — operator false alarm only. Fix: swap reconcile.go's two scans to record-walk-first.

### [LOW] Four files each carry their own package doc comment, conflicting with doc.go · `structure` · area: reclaim-reconcile-audit
- **Where:** `pkg/block/engine/reclaim.go:1`
- **What:** gc_block.go:1 ("Package engine — block-aware GC reclaim..."), reclaim.go:1 ("Package engine — orphan-storage reclaim..."), reconcile.go:1 ("Package engine — read-only orphan-storage reporter..."), audit_state.go:1 ("Package engine — audit") each have a comment block directly above `package engine` w/ no blank line — makes each a package doc comment in Go's eyes. doc.go:1 already carries canonical package doc.
- **Why it matters:** Go convention (staticcheck ST1000: at most one file/pkg should have a package comment) — go doc/pkg.go.dev pick one arbitrarily among several, so `go doc pkg/block/engine` output nondeterministic/misleading, and the four competing summaries contradict each other about what the package IS.
- **Fix:** Insert blank line between each file's descriptive comment and `package engine` (demotes to ordinary file comment), or drop the "Package engine — " prefix and start sentence w/ file's own subject.
- **Verified:** CONFIRMED. No blank line separates comment from package clause: gc_block.go 1-11/package :12; reclaim.go 1-20/package :21; audit_state.go 1-26/package :27; reconcile.go 1-5/package :6 — all four are package doc comments alongside doc.go:1's canonical one. Five package comments in one package; go doc picks arbitrarily. Not currently caught: .golangci.yml enables staticcheck but ST1000 not in enabled check set, lint green. Cosmetic — LOW.

### [LOW] resolveGracePeriod forces reclaim/reconcile to fabricate a throwaway GC-sweep Options struct · `structure` · area: reclaim-reconcile-audit
- **Where:** `pkg/block/engine/reclaim.go:246`
- **What:** resolveGracePeriod(options *Options) (gc.go:362) takes the central GC-sweep config struct (GCStateRoot, SyncedHashIndex, BlockReclaimer, DryRun, GracePeriodSet, ...). reclaim.go:246 (`resolveGracePeriod(&Options{GracePeriod: opts.GracePeriod})`) and reconcile.go:149 (same pattern) each construct a mostly-zero-valued Options{} purely to read back one derived time.Duration.
- **Why it matters:** ReclaimOptions/ReconcileOptions are already purpose-built independent option types — coupling to GC sweep's much wider Options type means a field added to Options for gc.go's own use silently becomes reachable (and misleadingly zero) inside reclaim.go/reconcile.go's call, and a reader must go read gc.go's Options doc to know which of a dozen fields matter here (only two).
- **Fix:** Change resolveGracePeriod to take primitives: `resolveGracePeriod(period time.Duration, isSet bool) time.Duration`. gc.go's own call sites pass options.GracePeriod/GracePeriodSet; reclaim.go/reconcile.go pass opts.GracePeriod, false directly — no Options{} construction needed.
- **Verified:** CONFIRMED. resolveGracePeriod(options *Options) gc.go:362; called w/ throwaway sweep-config struct at reclaim.go:246, reconcile.go:149 (identical). Both entry points have own option types (ReconcileOptions reconcile.go:121, ReclaimOptions). Side effect: neither plumbs GracePeriodSet, so operator-supplied explicit zero grace silently promoted to 1h default on these two paths (gc.go:369-371) — expressible on sweep path, not here. Reachable: blockgc_reconcile_report.go:74 / blockgc_reconcile_reclaim.go:122. LOW.

### [LOW] manifestShortfall/confirmShortfall break package-wide %w error-wrapping convention · `structure` · area: offline-readiness
- **Where:** `pkg/block/engine/offline.go:206`
- **What:** manifestShortfall propagates bare `err` from chunks.EnumeratePayloads (206), chunks.ListFileChunks (214), index.DataExtents (222), confirmShortfall (229); confirmShortfall itself does same for its ListFileChunks re-read (267). None wrap w/ fmt.Errorf("...: %w", err) or name the failing payload ID.
- **Why it matters:** Every sibling file in pkg (fetch.go, syncer.go, reconcile.go, manifest_repair.go, readwrite.go, carve_dispatch.go) wraps propagated errors w/ stage-name + %w — CLAUDE.md idiom rule states this explicitly. offlineReadinessOf turns this into operator-facing Reason string via `"manifest cross-check did not run: " + err.Error()` (156) — on this exact #2110-class residency cross-check path, unwrapped/unqualified error is the only diagnostic on gate refusal, and w/ EnumeratePayloads/ListFileChunks called once per payload in a loop, no way to tell which payload or which of four calls failed.
- **Fix:** Wrap each propagation: `fmt.Errorf("enumerate payloads: %w", err)`, `fmt.Errorf("list file chunks %q: %w", id, err)`, `fmt.Errorf("data extents %q: %w", id, err)`, confirmShortfall's `fmt.Errorf("re-list file chunks %q: %w", payloadID, err)` — matching `"list file blocks for %s: %w"` style already in fetch.go:148, syncer.go:499.
- **Verified:** CONFIRMED. Bare err propagation at 206/214/222/229, confirmShortfall 267. Sibling code wraps w/ stage name + %w (reconcile.go:176, reclaim.go:156, manifest_repair.go:155). Reachable: offlineReadinessOf <- Store.OfflineReadiness (93/99) <- runtime/offline.go:22, rendered as Reason string at :156 w/ no payload ID, no indication which of four calls failed. LOW — diagnostics only.

### [LOW] doc.go package doc describes a removed ReadBuffer/Prefetcher design as if still current · `bloat` · area: composition-lifecycle
- **Where:** `pkg/block/engine/doc.go:16`
- **What:** Section "# Read buffer and prefetch (formerly pkg/block/readbuffer)" (16-36) describes `ReadBuffer` (LRU, RWMutex, copy-on-read, per-share instance, secondary payloadID->blockIdx index) and `Prefetcher` (adaptive 1->2->4->8 depth, bounded worker pool) as live design. Neither type exists — `type ReadBuffer`/`func NewReadBuffer`/`type Prefetcher` absent (grep confirmed). engine.go:273-279 + tests show both replaced by single `Cache` type (NewCache, cacheInterface, nullCache); engine.go's own comment says so ("A single Cache type replaces the legacy ReadBuffer + Prefetcher pair").
- **Why it matters:** CLAUDE.md idiom rule: doc comments describe code now, not aspirational/legacy (same class as legacy_migration.go:28). `go doc engine` reader gets a deleted design — dead documentation for dead abstractions.
- **Fix:** Rewrite section to describe current `Cache` type (cache.go) or delete section and point to cache.go's own doc comment.
- **Verified:** CONFIRMED. doc.go:16-36 describes ReadBuffer (LRU, RWMutex, NewReadBuffer returns nil at 0, payloadID->blockIdx secondary index) and Prefetcher (adaptive, worker pool) as live. rg over repo: no `type ReadBuffer`, no `func NewReadBuffer`, no `type Prefetcher` — only survivors ReadBufferBytes config field (engine.go:57) and stats fields ReadBufferEntries/Used/Max (stats.go:41-43). engine.go:107/:275 confirm fold into Cache/cacheInterface/nullCache. Production file, surfaced by `go doc`. Fix: replace two paragraphs w/ Cache/nullCache description.

### [LOW] doc.go's CollectGarbage usage example doesn't match the real signature/API · `bloat` · area: composition-lifecycle
- **Where:** `pkg/block/engine/doc.go:74`
- **What:** Usage example calls `engine.CollectGarbage(ctx, remoteStore, registry, &engine.Options{DryRun: true})` and reads `dryStats.OrphanBlocks`. Real signature (gc.go:335): `CollectGarbage(ctx context.Context, reconciler MetadataReconciler, options *Options) *GCStats` — no remoteStore/registry params.
- **Why it matters:** Stale example that would not compile against current API. Reader following the doc example writes broken code.
- **Fix:** Update example to `engine.CollectGarbage(ctx, reconciler, &engine.Options{DryRun: true})`.
- **Verified:** PARTIALLY REFUTED but core defect CONFIRMED. Real sig gc.go:335 = CollectGarbage(ctx, reconciler MetadataReconciler, options *Options); doc example (74/78) passes 4 args (remoteStore, registry) — would not compile. Second half of claim is a MISREAD: GCStats DOES have OrphanBlocks (gc.go:160, legacy alias of ObjectsSwept, alongside OrphanFiles/BytesReclaimed/Errors, populated by finalizeStats) — no defect there. Fix: drop remoteStore+registry from the two example lines only.
### [HIGH] Remote sweep reclaims a hash a concurrent dedup write just re-referenced — no write barrier between mark snapshot and delete · `structure` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/gc_sweep_index.go:69`
- **What:** sweepFromSyncedIndex decides orphan-vs-live from syncedAt-vs-grace (60-67) + gcs.Has(h) (69-77), gcs = mark-phase live set snapshotted before sweep (markPhase, gc.go:464). Nothing revalidates between snapshot and delete+DeleteSynced (90-121). Dedup hit (engineDeduper.IsChunkDurable, blocksink.go:68-70) lets a new file reference an already-synced hash with zero footprint change — manifest row only, no journal write, no PutBlock — and never refreshes syncedAt. If that hash was already past-grace-orphaned at mark time, the resurrection manifest row can land after the mark snapshot; sweep then reclaims the block + DeleteSynced with the live row still pointing at it.
- **Why it matters:** Classic CAS-GC resurrection TOCTOU (checklist #8/#9: mark/sweep needs refresh or serialization vs a concurrent writer). Reader of the new file's chunk gets an unresolvable hash — silent zeros/error, manifest claims data exists, no log names the collision. Distinct from all prior journal-audit bugs (seed/eviction/reap-boundary/marker-loss); this is a live mark-sweep race with no analogue among them.
- **Fix:** On dedup hit, refresh/touch the synced marker's syncedAt (needs a Touch/Refresh on SyncedHashStore) so the existing grace-period gate protects resurrection the same way it protects a fresh upload. One write inside the already-open manifest transaction; no change to gc_sweep_index.go's decision logic.
- **Verified:** CONFIRMED, both refutation attempts fail. (a) Dedup does NOT refresh syncedAt: CommitBlock (blocksink.go:295-300) `continue`s on Data==nil, deduped chunks never reach PutSyncedLocators/MarkSynced. (b) Unlink does NOT clear the marker: only DeleteSynced callers are gc_sweep_index.go:113, gc_block.go:133/157, legacy_migration.go:208 — no delete/reap path clears it, so an orphan keeps its original syncedAt and IsSynced still answers true. gcRootLock serializes GC-vs-GC only; nothing quiesces writers. Net: dedup onto a past-grace orphan + invisible-to-mark manifest row + sweep frees the only durable copy — live row, deleted block, local record already flagged synced/evictable.

### [MED] GetStats() misclassifies local-only carved chunks as BlocksRemote, and BlocksLocal is never populated · `bugs` · area: composition-lifecycle
- **Where:** `pkg/block/engine/stats.go:216`
- **What:** classifyBlocks: BlockStatePending+nonzero-hash → BlocksRemote++ (comment: "remote copy is authoritative"). But manifestRows() (blocksink.go:100-116, shared by both sinks) writes every carved row State=Pending+nonzero-hash as the PERMANENT terminal state — nothing ever flips a per-file FileChunk row to BlockStateRemote (only BlockRecord.SyncState gets that). So a local-only share (remote==nil) counts every committed chunk into BlocksRemote though no remote exists. BlocksLocal (struct field, doc'd at engine.go:36) is never incremented anywhere.
- **Why it matters:** Residency-truth reporting failure: `dfsctl store block stats` for a local-only share can show HasRemote=false, BlocksRemote>0, Blocks Local=0 — self-contradictory. No test (stats_test.go) asserts these counters, so it's never been caught.
- **Fix:** Gate the Pending+nonzero-hash branch on bs.remote != nil before crediting BlocksRemote; count into BlocksLocal instead when remote is nil. Or rename the field / add a ponytail note on its real ceiling.
- **Verified:** CONFIRMED. BlocksLocal has zero writers repo-wide (only struct field, docs, aggregator, dfsctl display). syncer.go:473-475 comment corroborates: carve path "never transitions FileChunk.State to BlockStateRemote." Every real BlockStateRemote assignment is BlockRecord.SyncState (compaction.go:313, legacy_migration.go:347, blocksink.go:338). Downgraded HIGH→MED: reporting-only, no eviction/reclaim decision reads it.

### [MED] Truncate reaps manifest rows + decrements refcounts BEFORE the local physical truncate — late local.Truncate failure leaves reclaimed metadata pointing at un-clipped bytes · `bugs` · area: write-path-carve
- **Where:** `pkg/block/engine/readwrite.go:223`
- **What:** Order: narrowChunkRow(201) → DecrementRefCountAndReapMany(224) → ReprojectBlocks(230) — all committed — THEN bs.local.Truncate(236). If (236) fails (ctx cancel, ENOSPC, badger error), returns at 237 with no rollback: rows already reaped, refcounts already decremented (maybe to zero → GC-eligible), File.Blocks already reprojected. readAtInternal queries bs.local.ReadAt FIRST, so a still-live local interval past newSize is served as-is on a later read — stale, "deleted" content, no error, nothing logged. Doc comment (171-177) claims this "mirrors Delete's ordering" — false; Delete does local.Delete FIRST (367), reap SECOND (421-426), the opposite.
- **Why it matters:** truncate_reclaim.go documents exactly this failure shape ("later re-extend re-exposes discarded data... silent data-integrity/info-leak bug") and is itself best-effort, so nothing upstream compensates. No test injects a local.Truncate failure.
- **Fix:** Reorder: bs.local.Truncate first, coordinator reap/reproject only after it durably succeeds (match Delete's actual order). If coordinator-first is required for another reason, treat a subsequent local.Truncate failure as fatal and surface a distinct error/metric.
- **Verified:** CONFIRMED. Doc comment factually inverted vs Delete's real order. Reachable: internal/adapter/common/truncate_reclaim.go:43 (NFSv3/v4 SETATTR + CREATE-truncate, SMB SetEndOfFile), create.go:526, clone.go:325 — best-effort by contract, own header names this exact failure shape. journal.Store.Truncate can genuinely fail (ctx.Err, errClosed, marker append/fsync). MED not HIGH: needs a failure precisely in that window.

### [MED] PunchHole reaps refcounts/manifest BEFORE the zero-overwrite loop — mid-loop WriteAt failure leaves stale non-zero bytes readable after refcounts already dropped · `bugs` · area: write-path-carve
- **Where:** `pkg/block/engine/readwrite.go:309`
- **What:** Same ordering defect as Truncate. DecrementRefCountAndReapMany + ReprojectBlocks (309-318) for fully-covered blocks run BEFORE the zero-fill loop (324-335, 1MiB bs.local.WriteAt chunks). Mid-loop WriteAt failure (ctx cancel, disk error) returns at 332 with the range only partly zeroed — but covered blocks' rows already reaped, refcounts already decremented. Local ReadAt resolves the un-zeroed tail directly as warm data (no reconciliation), so a DEALLOCATE-region read returns leaked old bytes while the CAS refcount may already be GC-eligible.
- **Why it matters:** Same residency-truth mismatch as Truncate. No test drives a mid-loop WriteAt failure.
- **Fix:** Zero the range locally first, run coordinator reap/reproject only after every write in the loop succeeds — mirrors the Truncate fix, keeps both consistent with Delete's actual local-then-metadata order.
- **Verified:** CONFIRMED, more reachable than the Truncate case. Reachable: internal/adapter/nfs/v4/handlers/deallocate.go:91, after metadata.Service.PunchHole already pruned FileAttr.Blocks. Trigger broader than disk faults: journal AppendWrite can return ErrPressureTimeout under append-log pressure alone, no disk fault needed for a multi-MiB deallocate to hit it mid-range.

### [MED] ReconcileLocalView.ListUnsynced never implemented — class-4 (stranded dirty-local chunk) reporting is structurally dead in every real deployment · `slop` · area: reclaim-reconcile-audit
- **Where:** `pkg/block/engine/reconcile.go:32`
- **What:** Interface declares ListUnsynced; doc comment claims `*fs.FSStore satisfies it`. Repo-wide grep: only the decl, the use (reconcile.go:228), and a test-only `fakeUnsynced` stub. No production type implements it — not *fs.FSStore, not its embedded journal.Store (only exposes UnsyncedBytes() int64, a counter not an enumerator), not memory.MemoryStore. Only prod call site: blockgc_reconcile_report.go:30 `le.Store.(engine.ReconcileLocalView)` — always ok=false, `locals` stays empty, class-4 loop (reconcile.go:224-234) never runs on real data. No compile-time assertion would've caught it (contrast gc_block.go:164's `var _ BlockReclaimer = ...`).
- **Why it matters:** Same shape as legacy_migration.go's dead-interface finding, landing on the class tied directly to residency-truth: class 4 is the detector for dirty-local-only chunks (the ONLY durable copy) gone stranded. Operator running ReconcileReport before a reclaim pass gets permanent false-zero StrandedLocalChunks for every real share. Green unit test proves nothing — it hand-supplies the fake.
- **Fix:** Implement ListUnsynced for real on *fs.FSStore, or drop the false doc claim + add a compile-time assertion / explicit runtime warning (pattern already used at blockgc_reconcile_report.go:52-57) so a missing implementation is visible.
- **Verified:** CONFIRMED and live. Served at GET /api/v1/blockstore/reconcile-report (router.go:334, handlers/block_gc.go:312) and `dfsctl store block reconcile` (cmd/dfsctl/commands/store/block/reconcile.go:58) — StrandedLocalChunks reports 0 for every share always. reconcile.go:33 comment is false.

### [MED] PunchHole reaps refcount/manifest durably, then zero-overwrites on the same non-durable WriteAt path — no fsync barrier before client is ack'd · `structure` · area: write-path-carve
- **Where:** `pkg/block/engine/readwrite.go:309`
- **What:** PunchHole: (1) DecrementRefCountAndReapMany + ReprojectBlocks — durable metadata txn (309-318); (2) zero-fill via bs.local.WriteAt (324-335) — rides the same deferred-fsync path as an ordinary WRITE (journal appendRecord: "It never fsyncs", journal/segment.go:256). Sole caller internal/adapter/nfs/v4/handlers/deallocate.go returns NFS4_OK at line 101 with no Flush/Commit. So manifest-says-gone is durable before the zero-write is durable, nothing forces the write durable before the client is told it succeeded.
- **Why it matters:** On power loss in that window, reap is durable, zeros aren't. Read path resolves local reads from the journal interval index directly, never consults the manifest for warm data — pre-punch bytes can reappear after an acked DEALLOCATE. No RFC 7862 verifier mechanism lets the client detect this, unlike WRITE. Contrast: journal.Store.Truncate explicitly fsyncs a fencing marker first for exactly this reason ("Durability first...", journal/store.go:975) — PunchHole is the odd one out among Delete/Truncate/PunchHole in this file.
- **Fix:** Either call bs.local.Commit(ctx, payloadID) after the zero-overwrite loop before PunchHole returns (closes the gap for every caller), or have the NFSv4.2 DEALLOCATE handler call CommitBlockStore/Flush after PunchHole succeeds. Prefer the former.
- **Verified:** CONFIRMED, ordering and durability both verified as described. Severity HIGH→MED: window is power-loss only (a SIGKILL keeps the pwrite in page cache), and needs no client COMMIT.

### [MED] Repair's synced-hash check has no synchronization with GC's marker-clear ordering — boolean check where the pattern needs a lock/generation guard · `structure` · area: manifest-check-repair
- **Where:** `pkg/block/engine/manifest_repair.go:155`
- **What:** planPayloadRepairs gates RepairRecreateRow on store.IsSynced(hash) (155-162), re-checked in repairRow via tx.IsSynced (380-387) — plain boolean reads, zero coordination with concurrent GC. gc_block.go's ReclaimDeadChunk (last-chunk branch, 147-160) deletes the remote object THEN removes the block record THEN clears the synced marker (DeleteSynced, 157) — deliberate crash-safety ordering, but leaves IsSynced==true for a window after the bytes are actually gone. GC's live-set is built from FileChunk rows (coordinator.go:624) — exactly what's missing for the row repair is trying to recreate — so the hash needing repair is by construction sweep-eligible concurrently.
- **Why it matters:** The CAS-GC mark/sweep race the checklist calls out (item #8/#9). If recreate lands in the DeleteBlock-to-DeleteSynced window, repair fabricates a manifest row pointing at bytes already gone — a genuinely Lost range gets written back as falsely Synced, no comment anywhere names the shared invariant.
- **Fix:** Take the per-remote GC-exclusion lock around ApplyRepairs, or re-verify recreate evidence against the block record (not just the boolean marker) at write time. At minimum a ponytail: comment at :155 and :380 naming the window.
- **Verified:** CONFIRMED, and worse than claimed: gcRootLock (gc.go:400) only serializes GC-vs-GC, never vs a writer/repairer, and markPhase snapshots the live set from FileChunk rows — precisely the rows a repair candidate is missing — so any hash needing repair is unconditionally sweep-eligible. Reachable via REST ApplyRepairs. Severity HIGH→MED: needs a damaged payload plus an operator repair-with-apply overlapping a GC run.

### [MED] Cache subsystem fully wired in production but the read path never consults it — Get/Put dead, feature is a no-op that still costs memory and goroutines · `bugs` · area: cross
- **Where:** `pkg/block/engine/cache.go:205`
- **What:** Store.ReadAt uses Syncer.scheduleReadahead, a separate mechanism unrelated to Cache. Cache.Get has zero callers; Cache.Put's only caller is prefetchWorker, fed by Store.loadByHash (engine.go:297) which unconditionally returns ErrChunkNotFound. Every production loadCache() call passes nil hashes (OnRead(payloadID, nil, 0) — reset signal only), so seqTracker/prefetch logic can never fire. NewCache is constructed whenever ReadBufferBytes > 0 (default 12.5% of RAM floored at 64MiB), spinning up 4 background workers per share for nothing.
- **Why it matters:** read_buffer_size is a documented per-share config knob and CacheStats is surfaced via GetStats()/dfsctl as if reflecting real cache behavior. In production this does nothing for read latency or S3 GET reduction while costing real memory reservation and idle goroutines per share. Operator tuning read_buffer_size gets zero effect, no signal it's inert.
- **Fix:** Either wire Cache.Get into readAtInternal's local-miss path with real OnRead hashes from ReadAt (restore the promised behavior), or delete Cache/NewCache/prefetch pool/loadByHash entirely, drop ReadBufferBytes/PrefetchWorkers config, stop reporting CacheStats.
- **Verified:** CONFIRMED, stronger than claimed. cache.go:46 comment ("populated on write side via OnChunkComplete") is stale — OnChunkComplete exists nowhere in non-test code. Severity HIGH→MED, memory claim REFUTED: nothing is ever Put, so the LRU stays empty — actual cost is 4 idle goroutines/share plus an inert knob, not a memory reservation. Partially self-acknowledged: ponytail: marker at engine.go:294-296 ("leaves the CAS read cache hint-only") — but the hint is dead too.

### [MED] OfflineReadiness.Safe() can report true for up to 60s after a fresh residency loss, contradicting its "live, not stale" contract · `bugs` · area: cross
- **Where:** `pkg/block/engine/offline.go:381`
- **What:** offlineReadinessOf combines a fresh ColdExtents scan with shortfall(ctx,reporter) served from shortfallMemo.get, which returns the memoized (bytes,ranges) verbatim while time.Since(m.at) < shortfallInterval (=1 minute, :356), no re-run. A manifest row the local index doesn't describe, introduced after the memo last recorded "no shortfall," is invisible for up to a minute — offlineReadinessOf falls through to Known:true, RemoteOnlyBytes from stale data, and Safe() (Known && RemoteOnlyBytes==0) can report true.
- **Why it matters:** Sole prod caller Runtime.ShareOffline documents this explicitly as "taken live rather than recorded by a scheduled pass... an operator running `dfsctl share warm` wants to watch the number fall, not learn about it tomorrow" — directly contradicted by the memo. An operator/decommission workflow polling before wiping local storage can see Safe():true and destroy the only copy of bytes a concurrent event just made remote-only-and-unconfirmed. The existing ponytail comment only defends the persistent-shortfall direction, not a zero-memo going stale as a new loss appears.
- **Fix:** Either make Known false with a Reason whenever the shortfall answer came from a memo hit near the interval edge, or add a Store.OfflineReadinessFresh(ctx) that bypasses the memo for ShareOffline specifically while pollers/dashboards keep the cheap path. At minimum document the up-to-60s lag on Safe().
- **Verified:** CONFIRMED. Contract quote verified verbatim at runtime/offline.go:12-16. Downgraded HIGH→MED: window only opens if an interval is genuinely lost mid-process (itself an anomaly), bounded to 60s, fresh process starts with empty memo.

### [MED] Manifest cross-check reads EVERY share's payloads, not just this one's — permanently breaks OfflineReadiness for shares sharing a metadata store · `bugs` · area: offline-readiness
- **Where:** `pkg/block/engine/offline.go:202`
- **What:** manifestShortfall walks chunks.EnumeratePayloads with no share filter — bs.fileChunkStore is the raw per-name metadata.Store (blockstore_config.go:437), and EnumeratePayloads on every backend iterates the whole keyspace, no shareName prefix filter, though payload IDs are `{shareName}/{uuid}`. For a share sharing a metadata store with another (documented pattern, configuration.md:1188), the cross-check for share A also walks share B's payloads; A's local index has zero entries for B's payloadIDs, so every one of B's rows shows up as 100% shortfall against A.
- **Why it matters:** /archive is literally the docs' worked example for OfflineReadiness (configuration.md:664-670) AND for shared metadata (:1188) — under the documented topology, OfflineReadiness for /archive always takes the missing>0 branch and returns Known:false, permanently, no matter how much data is warmed and pinned. Not silent-zeros (fails toward indeterminate) but a complete, permanent break of the residency signal on a first-party documented deployment shape. No test exercises two shares against one fileChunkStore.
- **Fix:** Thread shareName/ShareID into engine.BlockStoreConfig, filter manifestShortfall's EnumeratePayloads callback to the `shareName+"/"` prefix before calling ListFileChunks/DataExtents, or add a share-scoped EnumeratePayloads to metadata.Store.
- **Verified:** CONFIRMED end to end (EnumeratePayloads has no share filter on any backend; journal DataExtents returns nil,nil for an unseen id → full-gap subtraction → Known:false). Two corrections: not unconditional — seedColdIfNeeded/SeedColdFromManifest walks the same unfiltered enumeration and instead inflates RemoteOnlyBytes with foreign bytes at seed time (Safe() still never true); payloads created by the sibling share AFTER that one-shot seed produce the permanent Known:false as described. Either way the signal is cross-share contaminated.

### [MED] GetStats() N+1 per-payload walk silently truncated by a fixed 5s timeout, producing inconsistent counts with no error surfaced · `perf` · area: composition-lifecycle
- **Where:** `pkg/block/engine/stats.go:194`
- **What:** populateBlockCounts does one EnumeratePayloads + one ListFileChunks round trip PER payload, under a hardcoded `context.WithTimeout(context.Background(), 5*time.Second)`. Per-payload error (including deadline-exceeded once the budget is spent) does `return nil // skip this payload, keep going` (202-203) — BlocksTotal/Dirty/Local/Remote silently undercount for every payload after the deadline trips, while stats.FileCount is still set from the full enumeration. GetStats() has no error return.
- **Why it matters:** N+1 scales linearly with payload count; a share with hundreds of thousands of files (measured elsewhere at 1.2M) can blow the 5s budget on one admin/metrics-triggered call. Failure mode is silent partial-count corruption, not a bounded error.
- **Fix:** Return an error / Truncated bool from populateBlockCounts and surface it, or thread a real ctx instead of the hardcoded 5s (already ponytail-noted at stats.go:181), or memoize like offline.go's shortfallMemo does for the same walk.
- **Verified:** CONFIRMED as described. Reachable: shares/blockstore_ops.go:269,283 (REST stats), non-test. One correction: if the deadline trips inside EnumeratePayloads itself, FileCount can be left at 0 rather than "correct" — skew varies by backend, but silent partial counts either way.

### [MED] carvePass dispatches every resident file through a goroutine+semaphore+shard-lock every tick, not just dirty ones · `perf` · area: write-path-carve
- **Where:** `pkg/block/engine/carve_dispatch.go:74`
- **What:** carvePass calls m.local.ListFiles(ctx) — walks every shard's full index, ALL resident files regardless of dirty state — then for EVERY entry does uploadLimiter.Acquire + spawns a goroutine calling m.local.Carve. Each per-file Carve takes sh.carveMu + sh.mu (journal/carve.go:238-269) just to discover most files have zero dirty bytes and bail. journal.Carve already has a cheap empty-FileID path that filters candidates per-shard internally — carve_dispatch.go never uses it.
- **Why it matters:** On a share where only a small fraction of resident files is dirty at any tick, this is O(total_resident_files) goroutine spawns + semaphore round-trips + shard-mutex pairs every UploadInterval (2s default) — recurring overhead scaling with total file count, not dirty file count, competing for scheduler time with the actual upload work.
- **Fix:** Add a cheap dirty-candidate accessor to journal.Store (factor carveCandidates' per-shard filtering out into something exported) and dispatch goroutines only over that filtered set instead of ListFiles' full population.
- **Verified:** CONFIRMED as written. Caveat on the fix: calling Carve with an empty FileID would serialize every shard behind one sequential loop, destroying the per-file upload concurrency the dispatcher exists for (doc at :59-71) — right fix is a dirty-file set (journal already tracks fi.firstDirtyNanos) exposed as ListDirtyFiles, feeding the same concurrent loop. Reachable: carveDispatcher launched from startPeriodicUploader (syncer.go:723-727).

### [MED] GetStats never sets BlocksLocal; local-only-share committed chunks misreported as BlocksRemote · `structure` · area: composition-lifecycle
- **Where:** `pkg/block/engine/stats.go:224`
- **What:** classifyBlocks only ever increments BlocksDirty or BlocksRemote. BlockStoreStats.BlocksLocal (stats.go:15) is declared and cross-share merged (blockstore_ops.go:298) but never assigned anywhere — permanently 0. For a local-only share (remote==nil), manifestRows() writes every committed row State=Pending with a real hash and never transitions it; classifyBlocks's Pending+nonzero-hash branch counts it as BlocksRemote with comment "the remote copy is authoritative" — false when there is no remote.
- **Why it matters:** Same class this audit targets: GetStats() is the engine's own reporting surface disagreeing with actual byte placement. An operator reading `dfsctl store block stats` for a local-only share sees non-zero "Blocks Remote" and zero "Blocks Local" — exactly backwards — could conclude the local copy is disposable when it's the only copy. Confirmed not to gate any automated eviction/reclaim decision — reporting-only.
- **Fix:** In classifyBlocks, branch on bs.remote before classifying a hashed Pending row: bs.remote==nil (or hash not remote-confirmed) → BlocksLocal++, not BlocksRemote++.
- **Verified:** CONFIRMED. engine.go:34-36 claims GetStats() "populates BlocksLocal/BlocksRemote/BlocksTotal" — false, zero assignments outside cross-share merge and dfsctl display. Reachable: GetStats → API /status + dfsctl store block stats. Reporting-only, MED stands.

### [MED] Flush's doc comment claims a metadata quiesce the early-return path never performs · `structure` · area: write-path-carve
- **Where:** `pkg/block/engine/flush.go:39`
- **What:** Comment (39-41) says the early-return branch (LocalDurable() && !RequireDurableCommit() — the DEFAULT policy on any share with the standard fs local store) "Still perform[s] the per-payload FileChunk metadata quiesce that syncer.Flush would... only the remote carve drain is skipped." The branch's only action before returning is bs.local.Commit(28) = journal Store.Commit → shard.groupCommit, a pure fsync, nothing metadata-related. The real quiesce (FileChunk rows / FileAttr.Blocks projection) only happens inside Carve→CommitBlock, reached exclusively via bs.syncer.Flush(46) or the explicit DrainRollups/DrainAllUploads helpers — all after the early return.
- **Why it matters:** Same idiom this audit already flagged live in the package (legacy_migration.go:28 precedent). Not cosmetic: an operator reasoning about why FileChunk manifest / FileAttr.Blocks lags a successful COMMIT (clone/snapshot/restart-recovery reasoning) is told by this comment the manifest is already quiesced when it structurally is not — only the next background carveDispatcher tick closes the gap.
- **Fix:** Rewrite the comment: nothing beyond the local fsync runs on this branch, no FileChunk rows written here; manifest catches up only via background carveDispatcher (bounded by UploadInterval) or an explicit DrainRollups/Flush(force) call. Drop the false "still perform the quiesce" claim.
- **Verified:** CONFIRMED verbatim. Branch is the default path (LocalDurable && !RequireDurableCommit) — production common case, not an edge case.

### [LOW] compaction: ErrChunkNotFound branch deletes block record with zero liveness verification and zero logging on the happy path · `bugs` · area: gc-mark-sweep-compaction
- **Where:** `pkg/block/engine/compaction.go:196`
- **What:** compactOneBlock: when rbs.GetBlock fails with block.ErrChunkNotFound, code assumes the only cause is "prior compaction's DeleteBlock landed, DeleteBlockRecord did not" and calls v.DeleteBlockRecord(blockID) unconditionally — no liveness check (can't, object is gone). Unlike the object-present path below (parses block, checks GetLocator per record first), success path has no log line at all; only DeleteBlockRecord failure logs a Warn. compaction_test.go has no test on this branch.
- **Why it matters:** ErrChunkNotFound is also exactly what real remote object loss/corruption/accidental-deletion looks like — indistinguishable here from crash-recovery, same action either way. Per-hash synced-locator index untouched in this branch, so hashes still pointing at blockID keep believing they're synced/live forever — permanent hard failure with no repair path, never re-surfaced as orphans by later sweeps since the marker still says synced. Matches the #2084 shape: no log on the common path, no operator visibility.
- **Fix:** Log unconditionally (slog.Warn) on branch entry before deleting — should be rare (crash-recovery only), a steady background rate is itself a signal. Add a CompactReport counter (e.g. HuskRecordsWithoutObject). Stronger: record a short-lived tombstone/moved-to marker in the old record before a successful compaction's final DeleteBlockRecord, so crash-recovered husks are distinguishable from genuinely lost objects.
- **Verified:** CONFIRMED as written, reachable via CompactBlocks ← blockgc.go:596 (non-test, DryRun false). Harm narrower than claimed: deleting the husk record does not CAUSE the stale-locator condition — locators pointing at a lost blockID are already dead either way. What's genuinely defective is observability: a real remote object-loss event is indistinguishable from routine cleanup and passes with zero log output. Downgraded to LOW, reclassified observability. Fix: one slog.Warn naming block_id + rec.ChunkCount before the delete.

## Implementation gaps vs reference

| Expected behavior (reference) | Our behavior | Gap | Source |
|---|---|---|---|
| fsync/close must block till durable, or protocol layer says async | PunchHole zero-fill rides journal's deferred-fsync WRITE path, NFSv4.2 DEALLOCATE handler ACKs `NFS4_OK` w/ no Flush/Commit after | Reap (manifest gone) durable before zero-write durable — power-loss window, pre-punch bytes reappear, no verifier lets client detect it (unlike WRITE/COMMIT) | readwrite.go:309, deallocate.go:91 · https://juicefs.com/docs/community/internals/io_processing/ |
| write-back ("ack before upload") must be opt-in, named risk | default async policy (`LocalDurable && !RequireDurableCommit`) early-returns from Flush; comment claims a metadata quiesce still runs on that branch | Comment false — no FileChunk/manifest work happens, only a bare fsync; risk window undocumented, not tunable/named like rclone's `--vfs-write-back` | flush.go:39 · https://rclone.org/commands/rclone_mount/ |
| sequential vs random-write carve strategy differs, no read-modify-write of remote object | not exercised by this audit — no finding either way | n/a | — |
| local read-cache and write buffer logically distinct (evicting one can't drop not-yet-uploaded data) | `Cache` (read side) fully wired (NewCache, 4 workers/share) but `Get` has zero callers, `loadByHash` is a permanent no-op miss — read path uses `scheduleReadahead` instead | Not a conflation bug — worse, read cache is dead code costing memory/goroutines for zero effect; conflation risk moot only because the cache never holds anything | cache.go:205 · https://juicefs.com/docs/cloud/guide/cache/ |
| fault-in fetches by need (range), not whole object | not exercised by this audit — no finding either way | n/a | — |
| concurrent fault-in of same cold range deduplicated (singleflight) | `fetchResolvedBlock`/`inlineFetchOrWait` do share a waiter path, but their `ErrChunkNotFound` wrap-and-log is hand-copied between the two, comment admits waiter must see identical wrapped error or it diverges | Drift risk on the exact seam that keeps waiter/direct-caller errors consistent — no compile-time enforcement | fetch.go:649 |
| partial/failed fault-in must not poison cache with partial/zeroed block | none directly — but same failure *class*: remote sweep can reclaim a hash a concurrent dedup write just re-referenced, no re-verify between mark snapshot and delete | Live row survives, block gone underneath it — reader gets unresolvable hash / silent zeros, same shape as #2084/#1888/#1850 (project memory) | gc_sweep_index.go:69 (HIGH) |
| GC must never delete on existence-check alone — mark must be refreshed, not just observed | `sweepFromSyncedIndex` decides orphan-vs-live from a mark-phase snapshot (`gcs.Has`) taken before sweep; nothing revalidates before delete+`DeleteSynced` | Dedup hit (`engineDeduper.IsChunkDurable`) never refreshes `syncedAt` — classic CAS-GC resurrection TOCTOU, checklist's exact #8 scenario, live in this codebase | gc_sweep_index.go:69 (HIGH) · https://github.com/restic/restic/pull/2718 |
| mark and sweep separated in time / serialized vs concurrent writers | `gcRootLock` only serializes GC-vs-GC, never GC-vs-writer/repairer; `manifest_repair.go`'s recreate path checks `IsSynced` as a plain boolean w/ zero coordination with GC's DeleteBlock→DeleteSynced ordering | Repair can fabricate a manifest row pointing at bytes GC already deleted in the marker-clear window — Lost range written back as falsely Synced | manifest_repair.go:155 (MED) · https://restic.readthedocs.io/en/stable/060_forget.html |
| deletion deferred/delayed, not synchronous with logical delete (grace window absorbs racing readers/writers) | Truncate and PunchHole both reap manifest rows + decrement refcounts BEFORE the physical local truncate/zero-write, with no rollback if the physical op then fails | Late local failure leaves reclaimed metadata pointing at un-clipped/un-zeroed bytes — stale content served with no error, no compensating grace window like Ceph RGW's deferred purge | readwrite.go:223 (MED), readwrite.go:309 (MED) · https://docs.ceph.com/en/reef/radosgw/config-ref/ |
| orphaned multipart uploads cleaned up on their own path | not exercised by this audit — no finding either way | n/a | — |
| don't assume S3 listing is strongly consistent for scan-then-act reconciliation | `Reconcile()` builds the live-set (`EnumerateSynced`) BEFORE walking records (`WalkBlockRecords`) — opposite of `ReclaimRecords()`'s deliberately TOCTOU-safe order, which has an 18-line comment + regression test for the identical race | A commit landing between Reconcile's two scans is invisible to the live-set but visible to the record walk → false leaked-block classification (report-only, doesn't delete) | reconcile.go:169 (LOW, duplicated of the HIGH-class race) |
| never derive object identity/dedup from ETag | dedup keyed by blake3 content hash (`engineDeduper.IsChunkDurable`), not S3 ETag | none — compliant | gc_sweep_index.go:69 narrative · https://docs.aws.amazon.com/AmazonS3/latest/API/API_CompleteMultipartUpload.html |
| conditional writes (If-Match/If-None-Match) for manifest updates that must not clobber a concurrent writer | not exercised by this audit — no finding either way | n/a | — |
| NFSv3 UNSTABLE write is a real not-yet-durable state; COMMIT is the only thing that makes it durable | PunchHole's zero-overwrite rides the same never-fsyncs journal `appendRecord` path as an ordinary WRITE, but the DEALLOCATE handler that triggers it returns success with no COMMIT/Flush step at all | Reap-then-write ordering means the "durable" signal (manifest updated) precedes actual durability of the write it depends on — inverts the RFC 1813 contract | readwrite.go:309, journal/segment.go:256 · https://www.rfc-editor.org/rfc/rfc1813.html |
| fsync/COMMIT success must mean safe against the failure domains the engine claims to survive, not just "in the write-back buffer" | Flush's default-policy branch does a bare local fsync and returns; doc comment claims a metadata quiesce also ran | Operator reasoning about "why does manifest lag a successful COMMIT" is told the quiesce happened when structurally it never does — same caveat ZeroFS documents explicitly for its own NFS mount, undocumented here | flush.go:39 (MED) · https://github.com/Barre/ZeroFS |
| SMB2/3 durable/persistent handles gated on CA + lease, reconnect must re-assert at correct epoch | not exercised by this audit — engine package has no SMB session/lease code, out of scope | n/a | — |
| grace period/generation number, not wall-clock, gates reclaim of anything an in-flight reader might still touch | `sweepFromSyncedIndex`'s orphan decision is a pure wall-clock `syncedAt` vs `gracePeriod` cutoff, no generation/touch-on-reference — a dedup hit never bumps `syncedAt` | Root cause of the HIGH finding above: wall-clock-only grace period is exactly the gap this checklist item warns about — a generation/touch mechanism on synced markers would close it | gc_sweep_index.go:69 (HIGH), gc.go:680 |
