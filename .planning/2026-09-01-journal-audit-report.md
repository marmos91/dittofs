# Audit Report — dittofs (ff14b24cb)
Stack: Go (1.x, stdlib-first). Package `pkg/block/journal` inside the single repo-root Go module (no nested go.mod under this path). Test-only deps: standard `testing` + in-package fakes (no testify/mock framework observed in headers scanned). Non-test external imports: internal/logger (repo-internal), pkg/block, pkg/block/chunker (sibling repo packages), lukechampine.com/blake3 (1 call site), golang.org/x/sys (statfs_unix.go/statfs_windows.go, build-tag split). No web framework, no ORM, no CLI framework here — this is a pure data-plane library package, not a service entrypoint. Build via `go build`/`go test` at repo root; no package-local Makefile/Dockerfile. · Areas: 6 · Findings: 6 HIGH / 8 MED / 45 LOW

## Summary by dimension

| Dimension | HIGH | MED | LOW |
|---|---|---|---|
| bugs | 3 | 1 | 6 |
| slop | 0 | 1 | 4 |
| perf | 0 | 4 | 9 |
| structure | 0 | 1 | 15 |
| bloat | 0 | 1 | 6 |
| comments | 0 | 0 | 3 |
| gaps | 3 | 0 | 2 |

## Summary by area

| Area | Findings |
|---|---|
| append-index-record | 10 |
| carve | 9 |
| cold-tier | 5 |
| cross | 3 |
| reclaim | 6 |
| recovery | 10 |
| store-api | 16 |

## Findings

### [LOW] insert() exact-overwrite fast path still heap-allocs a 1-elem dead slice on every warm overwrite · `perf` · area: append-index-record
- **Where:** `pkg/block/journal/index.go:151`
- **What:** Case (b) exact-range overwrite avoids the 3 general-path allocs + sort.Slice, but `dead = []segDead{{seg: e.loc.SegmentID, bytes: e.length}}` still allocs a fresh slice every non-cold overwrite.
- **Why it matters:** This is the steady-state random-4K-write shape per func's own comment, called once per appendRecord under sh.mu — avoidable alloc right next to two paths already hardened against it.
- **Fix:** Return scalar (deadSeg segDead, hasDead bool) or caller-owned scratch array instead of a 1-elem slice.
- **Verified:** CONFIRMED. Named return escapes to heap. Single small alloc next to a WriteAt already doing 3 pwrites — MED demoted to LOW.

### [LOW] appendRecord: 3 separate heap allocs per record on the write hot path, no pooling · `perf` · area: append-index-record
- **Where:** `pkg/block/journal/segment.go:291`
- **What:** `fileID := []byte(id)` (291), encodeHeader's make() (record.go:95), idxEntry.encode's make() (index.go:491) — 3 allocs/record, no reuse.
- **Why:** Shared hot-path primitive behind both WriteAt and Hydrate; package already has recordScratchPool for reads but nothing analogous for writes.
- **Fix:** Copy id bytes directly into the header buf being built (skip standalone []byte(id)); give idxEntry.encode a caller-supplied buf or per-shard scratch instead of make()-ing 40B/record.
- **Verified:** CONFIRMED segment.go:291/373/457/511, record.go:94, index.go:490. ~110B allocs/record behind 3 pwrites + group fsync — LOW.

### [LOW] readRecord (carve's read path) never reuses recordScratchPool, unlike verifiedRead · `perf` · area: append-index-record
- **Where:** `pkg/block/journal/segment.go:588`
- **What:** `readVerifiedRecord(seg.fd, recOff, s.cfg.SegmentSize, id, nil)` — nil scratch, fresh alloc per call. verifiedRead (index.go:386-412) pools the identical shape.
- **Why:** readRecord drives carve's sequential hot loop (runReader.Read, carve.go:970) — once per record boundary per pack pass, unpooled while its sibling is pooled.
- **Fix:** Thread a reusable scratch buffer through runReader (single-threaded/sequential) into readRecord, or have it borrow from recordScratchPool.
- **Verified:** CONFIRMED. Not drop-in: rr.rec is retained by caller (carve.go:955-957), so a shared pool needs release semantics — simpler fix is per-runReader scratch reused across records. LOW.

### [LOW] loadCold slurps the whole cold.log into one buffer instead of streaming · `perf` · area: cold-tier
- **Where:** `/Users/marmos91/dittofs-worktrees/audit-journal/pkg/block/journal/cold.go:193`
- **What:** `os.ReadFile(...)` pulls entire cold.log before decoding one entry; decode loop (200-210) appends with no cap hint; compaction gate (recovery.go:397) only evaluated after the full load.
- **Why:** Breaks the package's own bounded-read convention (record.go streams via fixed hdrBuf/ReaderAt); store-open memory becomes O(total historical cold churn), not O(live cold intervals).
- **Fix:** Decode via bufio.Reader / fixed scratch buf, same pattern as readRecordAt; size out's initial cap from fileSize/(coldHeaderSize+typicalIDLen).
- **Verified:** CONFIRMED os.ReadFile at cold.go:193, called every open (recovery.go:359) before compaction gate can act. Downgraded LOW: decoded []coldEntry retained either way; growth already ponytail-marked recovery.go:394-395 as bounded.

### [LOW] FileID(rec.fileID) map-key conversion in replayRecords allocates per record, bypasses inline-index elision · `perf` · area: recovery
- **Where:** `pkg/block/journal/recovery.go:318`
- **What:** `fid := FileID(rec.fileID); fi := idxMap[fid]` via local var defeats compiler's no-copy special case for `m[string(b)]` used directly as index. Same shape at 304, 311.
- **Why:** Extra heap alloc per replayed record in recover()'s hottest loop, every boot.
- **Fix:** Index map directly inline (`idxMap[FileID(rec.fileID)]`) on the lookup; materialize a real string only on insert.
- **Verified:** CONFIRMED but smaller than claimed — only the map-hit READ is elidable; tombstone/truncate/insert branches store into the map and need a real string regardless. One small alloc/record, once per boot. LOW.

### [LOW] WriteVersion and JournalVersion are byte-identical implementations exposed as two exported names · `structure` · area: store-api
- **Where:** `pkg/block/journal/store.go:468`
- **What:** WriteVersion (468) and JournalVersion (1046) both `return s.version.Load()`; different callers use each (engine/fetch.go:103, warm.go:94 vs flush.go:287, snapshot.go:561/1535).
- **Why it matters:** Two public names for one concept — violates minimize-interface-surface, invites drift if one is changed and not the other.
- **Fix:** Keep JournalVersion (matches PinVersion/SetPinVersion vocab), delete WriteVersion or alias it with a doc comment.
- **Verified:** CONFIRMED identical bodies. Partial mitigation: WriteVersion is interface-mandated (local.Store, MemoryStore); collapsing needs an interface change, not just a delete. LOW.

### [LOW] RestoreToVersion is a 170-line, gocyclo-37 function despite its own doc naming two independent phases · `structure` · area: store-api
- **Where:** `pkg/block/journal/store.go:1079`
- **What:** Doc comment (1065-1078) names "phase 1: ceiling replay" / "phase 2: re-materialize", body has matching markers (1087, 1169) but both live in one function mixing scan, dispatch, bookkeeping, I/O.
- **Why:** SRP smell the code's own comments already point the way out of; hard to review or unit-test in isolation.
- **Fix:** Extract replayCeiling(...) and rematerializeVersion(...) at the documented split; RestoreToVersion becomes a ~10-line orchestrator.
- **Verified:** CONFIRMED 141 lines, ~38 decision points. Maintainability only — no gocyclo/funlen lint gate configured in .golangci.yml. LOW.

### [LOW] errClosed is unexported even though it's the sentinel every gated Store method returns · `structure` · area: store-api
- **Where:** `pkg/block/journal/store.go:26`
- **What:** `var errClosed = errors.New("journal: store closed")` unexported, returned by 14+ exported methods; ErrLocalStoreFull (reclaim.go:29) exported in same package.
- **Why:** Inconsistent sentinel-export convention; no external caller can `errors.Is(err, journal.ErrClosed)`.
- **Fix:** Export as `ErrClosed` matching ErrLocalStoreFull's naming.
- **Verified:** CONFIRMED inconsistency, but motivating "workaround" evidence (fs.go/engine's own closed sentinels) REFUTED — different layer/purpose, no demonstrated consumer need. MED demoted LOW; one-line fix.

### [LOW] appendRecord's closed-check breaks the package's error-sentinel convention · `structure` · area: append-index-record
- **Where:** `pkg/block/journal/segment.go:276`
- **What:** Returns `fmt.Errorf("journal: closed")` instead of the errClosed sentinel used at all 13 other closed-check sites; sole exception is the shared primitive behind WriteAt/Hydrate.
- **Why it matters:** Breaks errors.Is(err, errClosed) for exactly the write path that most needs to distinguish clean shutdown from real failure.
- **Fix:** Replace with `errClosed` at segment.go:276.
- **Verified:** CONFIRMED sole non-sentinel site. Present impact nil (errClosed itself unexported; only in-package errors.Is check is gcLoop, not write path) — combinatorial with the finding above. One-line fix, LOW.

### [LOW] packRuns is a god-function: 246-line orchestration loop with shared mutable closure state · `structure` · area: carve
- **Where:** `pkg/block/journal/carve.go:388`
- **What:** 246 lines (388-634), gocyclo ~35, 6 shared locals (pending/arenap/arena/arenaOff/batchBytes/blockFirstRun) mutated across closures + outer loop.
- **Why:** Block-builder state has nothing to do with the outer per-run widening/clobber-guard prologue; hard to reason locally about who mutates what.
- **Fix:** Extract blockBuilder struct with ensureArena()/flush() methods; extract per-run prologue (extendRunToRowEnd + clobberGuard) into its own helper.
- **Verified:** CONFIRMED 246 lines, locals match. Claim overstated closure count (3 actual, not 6). Concurrency already extracted to carveDispatcher — the natural seam this asks for. MED demoted LOW, no defect.

### [LOW] repackSegment is a god-function mixing 5+ responsibility layers · `structure` · area: reclaim
- **Where:** `pkg/block/journal/reclaim.go:729`
- **What:** ~170 lines: snapshot victim under lock, tombstone scan, target segment create+write+seal, diskBytes accounting, index repoint (findMove/movesByVersion), victim retire, caller live-bytes mutation, delta compute — all inline.
- **Why:** Hard to unit-test in isolation; file already shows the extraction discipline elsewhere (movesByVersion:689, findMove:701).
- **Fix:** Extract snapshotVictim(), writeSurvivors(), repointIndex() per the func's own implied phases.
- **Verified:** CONFIRMED 172 lines (729-901), all named concerns present. Reachable via GC→gcShard→pickVictim, production path. No correctness issue — MED demoted LOW.

### [LOW] loadSegment is a god function mixing 6 unrelated concerns · `structure` · area: recovery
- **Where:** `pkg/block/journal/recovery.go:187`
- **What:** ~100 lines, 7+ early returns: fd open, header decode+orphan classify, sealed-but-empty damage, torn-tail truncate+fsync, shard resolve, two-actives conflict resolve, idx rebuild dispatch, replay dispatch — one flat function.
- **Why:** Breaks recover()'s own clean phase-method decomposition (scanSegments/applyColdLog/compactColdLog/assignActiveSegments).
- **Fix:** Extract classifySegment(), truncateTornTail(), resolveActiveConflict() as recoveryState methods.
- **Verified:** CONFIRMED, matches described flow exactly (recovery.go:187-288). Style/SRP only, no behavioral defect. LOW.

### [LOW] Three parallel slices kept in lockstep by an implicit shared index · `structure` · area: recovery
- **Where:** `pkg/block/journal/recovery.go:122`
- **What:** actives/sealedByShard/indexByShard — 3 independent slices/maps indexed by the same shard int across scanSegments, loadSegment, assignActiveSegments, reconcileByteCounters.
- **Why:** Struct-of-arrays anti-pattern; each future per-shard field is one more slice threaded by the same index.
- **Fix:** Bundle into `shardRecoveryData struct{active, sealed, index}`; single `[]shardRecoveryData`.
- **Verified:** PARTLY REFUTED — "nothing enforces same length" is false: newRecoveryState (138-153) is the sole alloc site, all three sized n=cfg.ShardCount, never appended to elsewhere; length invariant structurally guaranteed. Style preference only — MED demoted LOW.

### [LOW] doc.go "See gc.go" points at a file that doesn't exist · `bloat` · area: store-api
- **Where:** `pkg/block/journal/doc.go:23`
- **What:** "...must not drive one concurrently. See gc.go." — no gc.go in the package; GC lives in reclaim.go:516 (`func (s *Store) GC`).
- **Why:** Stale reference in the package doc comment; reader following it hits a missing file.
- **Fix:** `s/gc.go/reclaim.go/`.
- **Verified:** CONFIRMED, isolated stale ref (adjacent cross-refs elsewhere correct, e.g. store.go:200, segment.go:85). Doc-comment only, zero runtime effect. LOW.

### [LOW] appendTombstone/appendTruncateMarker and writeTombstoneRecord/writeTruncateRecord are near-duplicate bodies · `bloat` · area: append-index-record
- **Where:** `pkg/block/journal/segment.go:426`
- **What:** appendTombstone (426-474) and appendTruncateMarker (481-526) duplicate ctx check / maxFileIDLen / fail-injection / recLen / lock / seal-if-full / version stamp / write*Record / noteMinVersion / idxEntry / unlock / groupCommit / err-wrap; differ only in fail-field name, flag, FileOffset. writeTombstoneRecord (554-572) / writeTruncateRecord (531-550) likewise duplicate the encode/WriteAt/advance-tail sequence.
- **Why it matters:** ~90 lines of copy-paste in the hottest append-path file; any future change to marker framing has to be made and re-verified twice.
- **Fix:** Collapse to one `writeMarkerRecord(seg, id, version, flags, fileOffset)` and one `appendMarker(ctx, id, flags, fileOffset, failInject, kind)` that both current functions become thin wrappers over.
- **Verified:** CONFIRMED, deltas exactly as described. Reachable via Delete (store.go:773) / Truncate (store.go:976) + reclaim repack carry-forward. Pure duplication, no drift present today. LOW.

### [LOW] doc.go's dependency claim is contradicted by carve.go's three optional capabilities · `bloat` · area: carve
- **Where:** `pkg/block/journal/doc.go:6`
- **What:** doc.go:6-10 claims journal "depends only on stdlib plus a pair of narrow injected interfaces (RemoteStore, Clock)" and "knows nothing about namespaces, protocols, permissions, or the metadata store." carve.go:108-162 defines 3 more optional BlockSink capabilities — supersededReaper.ReapSupersededManifest, manifestRowEnder.ManifestRowEndAfter, clobberGuard.PreserveClobberedRow — doc comments reason in manifest-row/CAS vocabulary ("the commit that writes a row is an upsert", "a punched hole must read as zeros", "gap-free, overlap-free tiling of [0,size)"). Metadata-store domain knowledge baked into journal's own interface contracts.
- **Why it matters:** doc.go is the package's stated contract, primary rubric readers trust. Claim is false: only crash-safety commit-before-flip is metadata-agnostic, packing/reap correctness is not.
- **Fix:** Narrow doc.go to name these 3 as an explicit extension point for CAS-manifest-shaped sinks, or move the manifest-row reasoning into pkg/block/engine/blocksink.go where concrete sinks live; keep journal-side interface docs generic ("reports how far downstream coverage of a range reaches").
- **Verified:** CONFIRMED, stronger than claimed. doc.go:6-10 false twice: (a) non-test imports include internal/logger, pkg/block, pkg/block/chunker, lukechampine.com/blake3; (b) 2 more mandatory interfaces (Deduper, BlockSink) injected via SetCarveTargets (carve.go:170), plus the 3 optional ones. REACHABLE: engine/syncer.go:820,831 wire real sinks; engine/blocksink.go:167-235 implements all three. Doc-accuracy only.

### [LOW] coldstats.go split from cold.go not justified — one method, no reuse boundary · `bloat` · area: cold-tier
- **Where:** `pkg/block/journal/coldstats.go:29`
- **What:** coldstats.go is 53 lines, one exported method ColdExtents (~20 lines code, rest doc comment). No test-only/build-tag reason to separate from cold.go (encode/decode, appendCold, loadCold, rewriteCold, liveColdEntries, ColdSeeded/MarkColdSeeded). ColdExtents duplicates liveColdEntries's (cold.go:268-281) walk-shards-filter-iv.cold shape.
- **Why it matters:** CLAUDE.md flags unjustified file fragmentation (carve.go/carve_dispatch.go/carve_pack.go, reclaim.go precedent). Single-method file, no distinct lifecycle/locking/audience from cold.go — cost a 2nd file + doc preamble for zero cohesion gain.
- **Fix:** Fold ColdExtents (+ coldstats_test.go) into cold.go / cold_test.go. Split into real cold/ subpackage only if cold-tier surface later grows enough to justify it.
- **Verified:** CONFIRMED: 53 lines, one symbol at :29, ~20 lines code under 24-line doc comment, no build tag, same sh.mu-per-shard walk as cold.go. cold.go 324 lines already holds every other cold concern. REACHABLE: engine/offline.go:60 declares interface, offline.go:145 calls it. File-layout smell only.

### [LOW] writeDataRecord duplicates appendRecord's frame+write+CRC sequence instead of being shared · `bloat` · area: reclaim
- **Where:** `pkg/block/journal/reclaim.go:971`
- **What:** reclaim.go:971-999 writeDataRecord (used only by repackSegment) re-implements the header-encode + 3x WriteAt (header, payload, CRC) + tail-advance sequence segment.go:328-353 does inline in appendRecord (live write path). ~20 near-identical lines, independent error strings ("write record header" vs "repack write header").
- **Why it matters:** Package already extracted writeTombstoneRecord/writeTruncateRecord (segment.go:531-573) explicitly as helpers "shared by the live path and repack's marker carry-forward," called from reclaim.go. writeDataRecord is the same shape for data records but never wired back into appendRecord — duplicate logic that can drift.
- **Fix:** Have appendRecord call writeDataRecord(seg, id, offset, version, synced, data) instead of inlining, mirroring the marker-record delegation. writeDataRecord already returns payloadOff for direct use.
- **Verified:** CONFIRMED reclaim.go:967-999 writeDataRecord same sequence as segment.go ~328-353, differing only error strings. Established shared-helper pattern exists (segment.go:531-572, explicit doc comment) for marker records; data-record case is the odd one out. REACHABLE: writeDataRecord called reclaim.go:803 from repackSegment (GC path); appendRecord is live write path. No behavioral divergence today (reclaim.go:837 correctly notes deliberate diskBytes difference).

### [LOW] doc.go points to a file that doesn't exist · `comments` · area: store-api
- **Where:** `pkg/block/journal/doc.go:23`
- **What:** "...journal must not drive one concurrently. See gc.go." — no gc.go in package. GC/reclaim lives in reclaim.go (confirmed by store.go:200 "(reclaim.go)" and segment.go:85 "reclaim.go" pointing correctly).
- **Why it matters:** Stale cross-reference in the package's own doc contract, the primary rubric this audit treats as verifiable — a reader following it hits a 404.
- **Fix:** Change "See gc.go." to "See reclaim.go."
- **Verified:** DUPLICATE of doc.go:6 finding, same evidence: doc.go:23 'See gc.go.', no gc.go in package, GC is reclaim.go:516. Correct sibling refs at store.go:200 and segment.go:85 confirm intended target. Doc-only.

### [LOW] Free-disk-space (statfs) is queried once at Open to size a static cap; the live eviction gate never re-polls it · `gaps` · area: store-api
- **Where:** `pkg/block/journal/store.go:276`
- **What:** diskFreeBytes (statfs_unix.go:10, statfs_windows.go:10) called exactly once, in Open (store.go:276-283), to derive static cfg.MaxLocalBytes = free*0.8 when unset. ensureSpace (reclaim.go:328-335) gates purely on s.diskBytes.Load()+needed > s.cfg.MaxLocalBytes — an in-memory counter never cross-checked against real disk free space again.
- **Why it matters:** Reference write-back caches (rclone --vfs-cache-max-size/--vfs-cache-min-free-space, JuiceFS --free-space-ratio) combine a configured quota with a live statfs check because internal accounting can diverge from reality (other shares/host consuming the same volume after Open). Here the cap is a one-time startup snapshot, never refreshed.
- **Fix:** Have ensureSpace (or a periodic check) re-probe diskFreeBytes(s.dir) and treat low live headroom as a second, independent eviction trigger alongside the MaxLocalBytes/diskBytes comparison.
- **Verified:** CONFIRMED. Repo-wide diskFreeBytes call sites: statfs_unix.go:10/statfs_windows.go:10 (defs), store.go:277 (sole production call, inside `if cfg.MaxLocalBytes <= 0`), bounded_growth_test.go:17 (test). ensureSpace never re-polls statfs. NOT correctness-breaking: reclaim.go:319-327 documents MaxLocalBytes as "a soft pressure threshold, not a hard byte quota," store only over-fills its own accounting, never corrupts. Self-limiting: stale snapshot only exists when MaxLocalBytes left unset. Real gap vs rclone/JuiceFS but MED overstates it.

### [LOW] Delete unconditionally wipes truncVer even when the tombstone loses the race, reopening the truncate-staleness hole for the file's surviving incarnation · `bugs` · area: cross
- **Where:** `pkg/block/journal/store.go:807`
- **What:** Store.Delete (store.go:759-813) appends a tombstone at tombVer, keeps intervals with `iv.version > tombVer` (store.go:785-786, "raced past the delete: survives"). Regardless of survivors, `delete(sh.truncVer, id)` (store.go:806-807) runs unconditionally under "The file is gone, so its truncate bound has nothing left to guard" — false exactly when intervals survived. truncVer is the only thing staleAfterTruncate (store.go:471-474) checks to refuse a stale Hydrate write-back predating the file's last Truncate.
- **Why it matters:** A Hydrate in flight (notAfter sampled pre-truncate) that acquires sh.mu after this unconditional delete finds truncVer[id] absent, staleAfterTruncate returns false, fileIndex.hydratable (index.go:509-544) treats every gap as ordinary and hydratable — stale remote bytes get written back into the surviving file. The silent-stale-write-back class truncVer/doc.go D4 exist to prevent. Narrow (needs Truncate, then Delete racing a write, then a stale in-flight Hydrate on same file) but real, currently-unguarded, code path explicitly written and commented.
- **Fix:** Gate `delete(sh.truncVer, id)` on the same `len(kept)==0` condition already computed for the index (mirrors the already-conditional `delete(sh.index, id)` at store.go:797-800). Leave truncVer entry in place when tombstone loses the race.
- **Verified:** CONFIRMED store.go:806-807 unconditional, outside the `len(kept)==0` branch (785-786). truncVer sole reader staleAfterTruncate (store.go:471-474), sole writer Truncate (store.go:968). hydratable (index.go:509-544) fences only cold intervals by mark, plain holes always returned. REACHABLE non-test: Delete <- FSStore.Delete <- engine/readwrite.go:367 (NFS REMOVE/SMB delete) + flush.go:308; Hydrate <- engine/fetch.go:201 (cold-read write-back). Fix is one line. Severity lowered MED->LOW: narrow window, Delete records no version fence of its own anyway.

### [LOW] rewriteCold leaves cold.log.tmp orphaned on every error path · `bugs` · area: cold-tier
- **Where:** `pkg/block/journal/cold.go:234`
- **What:** None of the failure returns in rewriteCold (Write error ~245, Sync error ~249, Close error ~252, and Rename error ~255) remove the temp file `path+".tmp"` already created/written. Stray file persists in the journal directory after a compaction failure.
- **Why it matters:** Concrete resource leak on an otherwise carefully crash-safe path (temp+rename+fsyncDir); harmless to correctness since loadCold only reads cold.log, but leaves debris in a directory this package's crash-recovery model reasons carefully about.
- **Fix:** `defer func(){ if err != nil { _ = os.Remove(tmp) } }()` at the top of rewriteCold with a named return, or explicit `_ = os.Remove(tmp)` on each error return.
- **Verified:** CONFIRMED cold.go:217-262: tmp opened O_CREATE|O_TRUNC; Write/Sync/Close/Rename failure returns all leave tmp on disk (Rename failure return at ~255 also missed by the original claim). REACHABLE non-test: recovery.go:400 `r.s.rewriteCold(live)` on Open/recovery path. Trivial in practice: next rewrite reopens same path with O_TRUNC, at most one stale file.

### [LOW] Wire-framing structs split between exported (PascalCase) and unexported (lowercase) field casing with no reflection/marshaling reason for either · `structure` · area: append-index-record
- **Where:** `pkg/block/journal/record.go:77`
- **What:** recordHeader (record.go:77-83) and idxEntry (index.go:479-486) use exported PascalCase fields as layout-table documentation only — both hand-encoded (encodeHeader/decodeHeader, idxEntry.encode()), never via reflection/encoding-json. interval (index.go:29-46), piece (index.go:235-246), segmentMeta (segment.go:47-82) — structurally identical in-memory mirrors — use lowercase unexported fields.
- **Why it matters:** No functional difference, but unresolved style split within the same two files for the identical kind of struct, making exported casing look like it signals something (serialization, external API) when it doesn't.
- **Fix:** Pick one convention for internal wire-framing structs (lowercase, none need reflection); lowercase recordHeader's and idxEntry's fields to match interval/piece/segmentMeta. Bundle with an unrelated touch to these files, not standalone.
- **Verified:** CONFIRMED: recordHeader and idxEntry both hand-encoded, no reflection/struct tags anywhere. interval/piece/segmentMeta lowercase for identical kind of mirror. Both types used from non-test code. Pure style split, no functional effect.

### [LOW] journal/carve_dispatch.go and engine/carve_dispatch.go share a filename across sibling packages for unrelated responsibilities · `structure` · area: carve
- **Where:** `pkg/block/journal/carve_dispatch.go:1`
- **What:** pkg/block/journal/carve_dispatch.go (209 lines) is the per-file commit/flip ordering pipeline (carveDispatcher, commit-before-flip invariant, sem-bounded worker pool). pkg/block/engine/carve_dispatch.go (140 lines) is the background ticker loop calling journal.Carve across a share's files (Syncer.carveDispatcher, carvePass, newBlockID). Complements, not duplicates — confirmed by diff against source, not correctness finding.
- **Why it matters:** Identical basenames in sibling packages named around "carve dispatch" is a discoverability trap: grep -l, fuzzy-file-open, `git log --follow -- '**/carve_dispatch.go'` land on the wrong one without full path in view; a reader skimming names alone reasonably guesses these are one concept split across layers.
- **Fix:** Rename journal/carve_dispatch.go to carve_pipeline.go (it's a per-block commit->flip pipeline, not a scheduler) — frees "dispatch" for the engine-side scheduling loop that actually dispatches carve work.
- **Verified:** CONFIRMED both files production code: journal side = carveDispatcher (acquire/submit/commitAndFlip/discard/wait, commit-before-flip ordering); engine side = Syncer.carveDispatcher/carvePass/carveBlockSize/newBlockID (background ticker). Different responsibilities, identical basename in sibling packages. Discoverability nit only.

### [LOW] ColdExtents breaks the package's established named-result-struct convention for tally APIs · `structure` · area: cold-tier
- **Where:** `pkg/block/journal/coldstats.go:29`
- **What:** ColdExtents returns bare `(bytes int64, extents int64, err error)`. The package's other multi-field tally report, Evict (reclaim.go:56), returns named EvictResult{SegmentsEvicted, BytesFreed} instead of positional ints. Shape already leaked: engine/offline.go:60 hand-declares `ColdExtents(ctx) (bytes int64, extents int64, err error)` as its own interface method to consume it.
- **Why it matters:** Go idiom favors a named result struct over same-typed positional returns once there's more than one value: documents which int is which at call sites, can grow a field later without changing every signature. Package already applies this idiom for EvictResult; ColdExtents is the odd one out by omission.
- **Fix:** If signature is still cheap to change (check offline.go's Store type + two test fakes mirroring the tuple), introduce `type ColdStats struct{ Bytes, Extents int64 }`, return `(ColdStats, error)`; otherwise bundle with next signature touch, don't do standalone.
- **Verified:** CONFIRMED coldstats.go:29 tuple; EvictResult at reclaim.go:38-41 returned by Evict (reclaim.go:56). Shape leaked to engine/offline.go:60 coldRangeReporter, consumed at offline.go:145. Two same-typed positional returns are mis-orderable.

### [LOW] evict's allowActiveSeal is an undocumented boolean-trap parameter · `structure` · area: reclaim
- **Where:** `pkg/block/journal/reclaim.go:343`
- **What:** s.evict(ctx, targetBytes, allowActiveSeal bool) called as s.evict(ctx, overage, false) at reclaim.go:343 (ensureSpace) and s.evict(ctx, targetBytes, true) at reclaim.go:57 (Evict). Bare true/false carries load-bearing meaning (whether the write-path capacity gate may force-seal its own active segment) invisible at either call site.
- **Why it matters:** Classic boolean trap — reader of ensureSpace sees `false` and must jump to evict's doc to learn what it disables. Both call sites already carry multi-line comments compensating for the bool's opacity.
- **Fix:** Name the constant at each call site (`const dontSealActive = false` / `const allowSealActive = true`), or two thin wrappers evictForce/evictWritePath, so the call reads without a doc jump.
- **Verified:** CONFIRMED reclaim.go:65 signature, call sites reclaim.go:57 (true) and reclaim.go:343 (false), both with multi-line explanatory comments exactly as the finding names. Live non-test path (ensureSpace is the write-path capacity gate). Style-only.

### [LOW] Idx-sidecar open flags/perm tuple duplicated verbatim in 3 places · `structure` · area: recovery
- **Where:** `pkg/block/journal/recovery.go:258`
- **What:** `os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)` for the .idx sidecar appears identically at recovery.go:258, recovery.go:462, and segment.go:177 — three independent copy-pasted literals.
- **Why it matters:** DRY violation on a correctness-sensitive tuple (flags+perm for a durability-adjacent file). A future change (e.g. adding O_SYNC, tightening perms) applied to one site and missed on the others silently reintroduces divergent idx-write behavior between fresh-create, recovery-reopen, and pool-reuse paths.
- **Fix:** Extract a single openIdxAppend(path string) (*os.File, error) helper in segment.go, call it from all three sites.
- **Verified:** CONFIRMED byte-identical at recovery.go:258 (recovery-reopen), recovery.go:462 (pool-reuse), segment.go:177 (fresh-create). All three non-test paths; segment.go:177 via createSegment, both recovery sites via Store.recover. recovery.go:561 uses O_TRUNC, correctly excluded. Fix: one func (s *Store) openIdx(id uint64) *os.File; three sites collapse to it.

### [LOW] Comment references an issue number, banned by repo convention · `comments` · area: store-api
- **Where:** `pkg/block/journal/commit_bench_test.go:10`
- **What:** "BenchmarkConcurrentCommit reproduces the fio rand-write-4k shape that #1736 regressed on" — embeds issue number #1736 in a source comment.
- **Why it matters:** CLAUDE.md "Code comments": comments describe behaviour, never reference issue/PR numbers — those belong in commit messages/PR descriptions. No ponytail: prefix here (the sole sanctioned exception).
- **Fix:** Drop the issue number, describe the shape directly (e.g. "reproduces a fio rand-write-4k burst: many concurrent 4 KiB writes each followed by a durability Commit..."); move #1736 pointer to a commit message if needed.
- **Verified:** CONFIRMED at commit_bench_test.go:10. Rule not scoped to non-test files — this is committed, compiled source, not an unreachable path.

### [LOW] Comment references an issue number, banned by repo convention · `comments` · area: store-api
- **Where:** `pkg/block/journal/groupcommit_durability_test.go:18`
- **What:** "...which is what proves they aren't hollow (see #1736 durability review)."
- **Why it matters:** Same CLAUDE.md rule as above — no issue/PR references in source comments, no ponytail: prefix.
- **Fix:** Drop "(see #1736 durability review)"; the preceding sentence already explains why the seam exists without needing the pointer.
- **Verified:** CONFIRMED at groupcommit_durability_test.go:18. Only two such refs in the whole package (grep for #1xxx/#2xxx), these two. Fix: delete the parenthetical.

### [LOW] appendCold permanently skips directory-fsync retry after first failure, defeating D4's durability guarantee · `bugs` · area: cold-tier
- **Where:** `/Users/marmos91/dittofs-worktrees/audit-journal/pkg/block/journal/cold.go:145`
- **What:** First appendCold call: OpenFile(O_CREATE) makes cold.log even if fsyncDir(s.dir) then fails; error returned, file not removed. coldFD nil, next call os.Stat finds file exists, created=false, fsyncDir skipped forever after.
- **Why it matters:** cold.go's own doc says this fsync exists so a crash right after first eviction can't come back to no cold.log. Losing the dir entry loses every cold marker → ranges read as POSIX holes → silent zeros (#2084-class). Fails open, untested.
- **Fix:** On fsyncDir failure, `os.Remove(s.coldPath())` before returning so next call retries: `if derr := fsyncDir(s.dir); derr != nil { _ = fd.Close(); _ = os.Remove(s.coldPath()); return fmt.Errorf(...) }`.
- **Verified:** CONFIRMED cold.go:129-150. Reachable via reclaim.go:280 (evictSegment), store.go:531 (SeedCold), store.go:625 (SeedColdBatch), store.go:1290 (Invalidate). Needs dir-fsync error + later crash before writeback → LOW not HIGH.

### [LOW] Sealed-segment corruption past first record silently dropped: no warning, no truncation, no disk-byte correction · `bugs` · area: recovery
- **Where:** `pkg/block/journal/recovery.go:214`
- **What:** loadSegment warns only when sealed segment scans as ZERO valid records. N good records then a torn record N+1 → recs short, validUpTo < fileSize, but truncate branch at line 243 is gated `!sealed` so nothing truncates AND nothing logs. m.tail.Store(validUpTo) under-reports segment size; diskBytes undercounts.
- **Why it matters:** A sealed segment is trusted as fully durable. Any record dropping from the index via mid-stream corruption is indistinguishable from never-written data → HOLE/WARM conflation (D1 cardinal sin), zero diagnostic trace. TestSealedSegmentWithNoValidRecords only covers len(recs)==0, not the prefix-then-corrupt case.
- **Fix:** After scanValidRecords, for sealed branch also check `validUpTo < fileSize(fd)` and logger.Warn naming segment id + dropped range. Use fileSize(fd) not validUpTo when summing `disk` in recover(). Add regression test with valid records ahead of corrupted tail.
- **Verified:** CONFIRMED recovery.go:214-228 warns only on `sealed && len(recs)==0`; torn-tail branch at 243 gated `!sealed`. Reachable: loadSegment called from recover() on every open. Data past tear already untrustworthy regardless — actual defect is missing diagnostic + undercount, not new data loss → LOW.

### [LOW] doc.go's stdlib-only dependency claim is contradicted by the package's own import block · `slop` · area: store-api
- **Where:** `pkg/block/journal/doc.go:7`
- **What:** doc.go claims journal "depends only on the standard library plus a pair of narrow injected interfaces (RemoteStore, Clock)". store.go:14-15 imports internal/logger and pkg/block/chunker directly (11 logger.Warn sites; chunker.Params/DefaultParams/Validate in Config.withDefaults). format.go imports pkg/block for block.ErrFutureFormat.
- **Why it matters:** This is the doc's central testable D5 claim and it's false trivially — a grep of the package's own imports refutes it. internal/logger is an internal/ import, a hard Go-compiler blocker for the "stand alone as a library" extraction doc.go asserts is already true.
- **Fix:** Either sever the deps (inject a narrow Logger interface instead of internal/logger; chunker/block deps harder to remove) or rewrite the doc claim to state the real dependency set.
- **Verified:** CONFIRMED: doc.go:7-9 vs internal/logger (store.go:14-15), pkg/block/chunker (store.go:15), pkg/block (format.go). internal/ path makes extraction outright impossible. Documentation-only, no runtime effect → LOW.

### [LOW] fileIndex.insert: tie-break on equal Version diverges between fast-path and general-path branches · `slop` · area: append-index-record
- **Where:** `pkg/block/journal/index.go:166`
- **What:** Exact-range fast path (line 147): `iv.version <= e.version` → existing wins tie. General multi-fragment path (line 166): `e.version > iv.version` (strict) → tie falls to else, new wins. Same rule, opposite outcome on equality.
- **Why it matters:** Ties are real: recovery.go's applyColdLog (line 377) replays cold-log entries at their *original* persisted Version. A crash window (whole-range warm record replayed, then partial overwrite splits it, then cold entry re-inserted at original Version) produces a tie against a still-valid warm fragment, forced onto the general path, which now demotes an intact warm fragment to cold and charges it dead — needless remote fetch, skewed GC dead-byte signal. Breaks order-independence the fast path (and TestInsertNewestWinsByVersion) assumes. No test covers a Version tie in either branch.
- **Fix:** Change line 166 to `if e.version >= iv.version {` to match fast path. Add tie regression test (exact match + multi-fragment overlap) in index_test.go.
- **Verified:** CONFIRMED drift, index.go:147 vs :166. Ties producible via applyColdLog (recovery.go:377) reusing persisted Version, and repackSegment moves forward with m.version. Impact = needless remote fetch + skewed GC signal, not corruption (range still remote-durable via evictable() gate), needs narrow crash window → LOW not HIGH.

### [LOW] syncedRanges does unbounded O(intervals) scan per dirty run instead of binary-search bound like its sibling anySyncedFrom · `perf` · area: carve
- **Where:** `pkg/block/journal/carve.go:749`
- **What:** syncedRanges(sh, id, from, to) walks `for k := range fi.ivs` from index 0 over every live interval, under sh.mu.Lock(). Called once per dirty run from packRuns (carve.go:500-507) whenever sink implements clobberGuard — which both production sinks do. Sibling anySyncedFrom (20 lines above) already bounds start via `sort.Search(len(fi.ivs), func(i int) bool { return fi.ivs[i].end() > off })` (carve.go:729) specifically to stay off hot paths.
- **Why it matters:** O(runs × total_intervals) per file under a lock that blocks concurrent appends, on a path the same file already documents as needing this exact optimization.
- **Fix:** `k := sort.Search(len(fi.ivs), func(i int) bool { return fi.ivs[i].end() > from })`, start walk from k, keep existing `iv.fileOff >= to` break.
- **Verified:** CONFIRMED carve.go:759 loop from 0 vs sibling's sort.Search bound at :729. Reachable per dirty run via both production sinks (blocksink.go:171,:175 implement PreserveClobberedRow). LOW not HIGH: same per-run pass already pays a whole-manifest read in extendRunToRowEnd (ponytail-marked dominant cost), so this in-memory walk isn't the bottleneck.

### [LOW] rebuildIdx: N unbuffered write() syscalls instead of one batched write · `perf` · area: recovery
- **Where:** `pkg/block/journal/recovery.go:565`
- **What:** `for _, rec := range recs { fd.Write(idxEntry{...}.encode()) }` — one raw fd.Write per record (40 bytes), no bufio, single Sync at end. fnv1a(string(rec.fileID)) allocates a fresh string per iteration.
- **Why it matters:** Triggered whenever .idx sidecar is missing — exactly the failure recovery must handle promptly. Thousands of records = thousands of syscalls serially blocking Open().
- **Fix:** Wrap fd in bufio.NewWriter (or accumulate into one []byte), single/few Write calls, Flush before Sync.
- **Verified:** CONFIRMED recovery.go:565-578, fnv1a alloc at :568, Sync at :579. Reachable via loadSegment idxMissing→rebuildIdx (:279-283); repack targets ALWAYS lack a sidecar so every GC-repacked segment rebuilds on next Open. LOW not HIGH: nothing ever reads this path back — correct fix is deleting rebuildIdx, not bufio-wrapping it.

### [LOW] Optional BlockSink capability interfaces are unexported — engine can never compile-check against them, so a method-name typo silently no-ops a correctness-critical hook · `structure` · area: carve
- **Where:** `pkg/block/journal/carve.go:118`
- **What:** supersededReaper (:118), manifestRowEnder (:133), clobberGuard (:160) unexported, detected via `s.sink.(supersededReaper)` etc. (carve.go:339/500/675). engine/blocksink.go implements all three by name only, zero `var _` guards anywhere.
- **Why it matters:** A typo in ReapSupersededManifest/ManifestRowEndAfter/PreserveClobberedRow produces zero build error, zero panic — assertion returns ok=false, capability silently skipped. Stale manifest rows never reaped → stale row can win over fresh and serve old bytes on cold read (carve.go:321-324) — the package's own cardinal sin, reached via D5 seam instead of HOLE/COLD seam.
- **Fix:** Export the three interfaces, add `var _ journal.SupersededReaper = engineBlockSink{}` etc. next to engineBlockSink/localBlockSink in blocksink.go.
- **Verified:** DUPLICATE of the `gaps`-dimension finding below (same evidence). Premise "none CAN exist" REFUTED — engine can assert today via anonymous interface literal, no exported journal type needed. No present defect: all six methods currently match. Downgraded to LOW; report once, fix is 3 one-line assertions.

### [LOW] D5 is false at the semantic level: the three optional capabilities bake DittoFS's CAS-manifest model into a package that claims to know nothing about the metadata store · `structure` · area: carve
- **Where:** `pkg/block/journal/carve.go:108`
- **What:** doc.go:6-10 claims journal "knows nothing about ... the metadata store". carve.go:108-162 (optional capabilities) reasons entirely in metadata-store vocabulary: "manifest row" (:110,124,144), "the commit that writes a row is an upsert" (:139), "gap-free, overlap-free tiling of [0,size)" (:111), "a punched hole must read as zeros" (:151). BlockSink/Deduper are generic; these three are not.
- **Why it matters:** A third party extracting this as a standalone library gets eviction/carve hooks meaningless without reimplementing DittoFS's specific manifest semantics — falsifies the "standalone reusable cache" framing. Coupling is baked into contract vocabulary, not the import graph.
- **Fix:** Reframe as fully generic append-cache terms, e.g. single `PostCommit(ctx, id, committedSpans, owedSpans) error` capability with zero "row"/"manifest"/"upsert" mention. Move manifest-specific reasoning into blocksink.go's doc comments.
- **Verified:** CONFIRMED as stated but weak — design-taste judgement, coupling is opt-in (sink omitting methods just loses the reap), journal genuinely imports no metadata package. Report as doc-honesty note, not defect.

### [LOW] doc.go's 'stdlib plus narrow interfaces' dependency claim is false — package imports internal/logger · `bloat` · area: store-api
- **Where:** `pkg/block/journal/doc.go:7`
- **What:** doc.go: "depends only on the standard library plus a pair of narrow injected interfaces (RemoteStore, Clock)." Actual: internal/logger imported by store.go:14, cold.go, reclaim.go, recovery.go (logger.Warn e.g. store.go:280,386,419); pkg/block/chunker imported by store.go:15/carve.go/index.go (Config.ChunkParams chunker.Params); pkg/block imported by format.go:25 (block.ErrFutureFormat).
- **Why it matters:** internal/ import is the exact compiler-hard blocker the library-extractability rubric calls out — makes the package impossible to use outside this module regardless of interface narrowness. Doc isn't just imprecise, it's actively wrong about a hard constraint.
- **Fix:** Drop internal/logger for an injected Logger interface and correct doc.go's chunker/block description, or rewrite doc.go to state the real dependency set.
- **Verified:** CONFIRMED. doc.go:6-8 vs internal/logger (store.go:14, cold.go:43, reclaim.go:11, recovery.go:11), pkg/block/chunker (store.go:15, carve.go:15, index.go:11), pkg/block (format.go:25). Doc comment falsity, not runtime defect → LOW.

### [LOW] Optional BlockSink capabilities (supersededReaper/manifestRowEnder/clobberGuard) are unexported interfaces detected only via runtime type-assertion, with zero compile-time conformance check anywhere in the repo · `gaps` · area: carve
- **Where:** `pkg/block/journal/carve.go:118`
- **What:** supersededReaper (:118), manifestRowEnder (:133), clobberGuard (:160) unexported. Detected via `s.sink.(supersededReaper)` (:339), `.(manifestRowEnder)` (:675), `.(clobberGuard)` (:500) — failed assertion is a silent no-op, no log/error. engine's engineBlockSink/localBlockSink (blocksink.go:171-237) implement all three by name only; unexported identifiers can't be referenced from engine for a `var _` guard. Repo-wide grep: zero conformance guards for any of the three.
- **Why it matters:** Index/manifest must never silently diverge from the append-only log as a second source of truth. carveFile's own comments (carve.go:313-338) say a single missed reap leaves stale rows alive forever and can serve old bytes on cold read under greatest-start overlap. If any of the three methods silently stops being invoked (rename during refactor, new sink impl, copy-paste drop), manifest diverges from ground truth with no error, no log, no test failure — silent data corruption.
- **Fix:** Export the three interfaces (SupersededReaper, ManifestRowEnder, ClobberGuard); add `var _ journal.SupersededReaper = engineBlockSink{}` (+ other two, + localBlockSink) in blocksink.go. Keep runtime ok-gated assertion in carve.go for legitimate fake-sink opt-out.
- **Verified:** CONFIRMED as stated; keep this one, drop the `structure`-dimension duplicate above. Claim itself concedes "no live bug today". Stated impossibility REFUTED — engine can assert structurally via anonymous interface literal, no exported journal type required. Downgraded HIGH→LOW: 3-line zero-risk fix, guards a rename-during-refactor risk that hasn't occurred.

### [LOW] fileIndex.insert breaks version ties in opposite directions on the fast vs. slow overlap path · `bugs` · area: append-index-record
- **Where:** `pkg/block/journal/index.go:147`
- **What:** Fast path (:141-158): `if iv.version <= e.version { return }` — existing wins tie. Slow path (:164-193): `if e.version > iv.version {...} else {...}` — tie falls to else, new wins. Doc comment (:85-88) says version breaks ties but not which way; the two paths disagree with each other.
- **Why it matters:** Whether a duplicate-version reinsert is idempotent depends on incidental overlap shape (exact boundary vs partial) rather than a uniform intentional policy.
- **Fix:** Change `if e.version > iv.version` to `if e.version >= iv.version` at index.go:166 so duplicate-version insert is idempotent regardless of overlap shape.
- **Verified:** CONFIRMED asymmetry, no wrong-bytes outcome found. Both paths reachable (insert called from appendRecord segment.go:381, SeedColdBatch store.go:631). Production duplicate-version case (crash between appendCold and segment unlink) is an EXACT range match → hits fast path correctly. Partial-overlap variant needs cold entry to straddle a live warm fragment → spuriously-cold range, bytes provably remote-resident (evict only evicts synced segments) → redundant re-fetch, not corruption. Severity dropped MED→LOW.

### [LOW] doc.go points at a file that does not exist ("See gc.go") · `slop` · area: store-api
- **Where:** `pkg/block/journal/doc.go:23`
- **What:** Package doc's closing sentence: "journal must not drive one concurrently. See gc.go." No gc.go exists; GC lives in reclaim.go alongside Evict.
- **Why it matters:** Stale file pointer in the one file meant to be the package's authoritative contract — misleads a reader trying to find the GC implementation.
- **Fix:** Change "See gc.go." to "See reclaim.go." (or name the function, e.g. "See Store.GC in reclaim.go.").
- **Verified:** CONFIRMED. doc.go:23; directory listing has only gc_test.go, real impl (pickVictim, repackSegment, Evict) in reclaim.go.

### [LOW] MaxLocalBytes free-space probe silently leaves the store uncapped when disk is full, contradicting its own comment · `slop` · area: store-api
- **Where:** `pkg/block/journal/store.go:276`
- **What:** Config.MaxLocalBytes doc (:63) and inline comment (:271-275) say uncapped only when the probe itself fails. Code: `if free, ferr := diskFreeBytes(dir); ferr == nil && free > 0 {...} else if ferr != nil { logger.Warn(...) }` — the ferr==nil && free==0 branch is unhandled: MaxLocalBytes stays 0 (unset/uncapped), no warning logged.
- **Why it matters:** Plausible-but-wrong logic, silently swallows disk-full case with no log line, contradicting the adjacent comment. It's exactly the disk-full edge case the cap mechanism exists to guard against.
- **Fix:** Fold free==0 into the warning branch (`else` instead of `else if ferr != nil`); clamp computed cap to at least 1 byte, e.g. `cfg.MaxLocalBytes = max(1, int64(float64(free)*defaultMaxLocalBytesFreeFraction))`.
- **Verified:** CONFIRMED. store.go:276-282. Reachable: Open is the package's sole constructor. Needs already-full volume at Open → MED→LOW. Fix: drop `&& free > 0` guard or make else unconditional.

### [LOW] SeedColdBatch re-locks the same per-shard mutex once per cold entry instead of grouping by shard · `perf` · area: store-api
- **Where:** `/Users/marmos91/dittofs-worktrees/audit-journal/pkg/block/journal/store.go:628`
- **What:** After appendCold, insert loop does `for _, e := range entries { sh := s.shardFor(e.id); sh.mu.Lock(); sh.indexFor(e.id).insert(...); sh.mu.Unlock() }`. entries built by iterating seeds in order, so every extent of one file is contiguous → back-to-back Lock/Unlock of the same shard mutex.
- **Why it matters:** SeedColdBatch is documented as a bulk primitive seeding "whole manifest at once" for share-add/snapshot-restore, scaling with file count × extents-per-file. Re-acquiring the same mutex per entry adds needless lock/unlock churn proportional to N instead of distinct shards touched.
- **Fix:** Group entries by shard before the insert pass (bucket by s.shardFor(e.id)), take each shard's lock once, insert all its entries under one critical section.
- **Verified:** CONFIRMED. store.go:628-639. Reachable from engine/flush.go:135-143 (share add/snapshot restore). Sized honestly: ~20ns uncontended mutex round trip, whole point of the call is replacing per-file fsync with one batch fsync — ~2ms lock churn at 100k entries vs seconds saved. Real but immaterial → MED→LOW.

### [LOW] ReadAt hot path allocates pieces+segs slices every call, no pooling · `perf` · area: append-index-record
- **Where:** `pkg/block/journal/index.go:251`
- **What:** fi.plan() (:251-280) builds `pieces []piece` via repeated append from nil; ReadAt (:324) does `segs := make([]*segmentMeta, len(pieces))` every call — fresh heap allocs even for the common single-interval warm read.
- **Why it matters:** ReadAt is the named hot random-read data-plane path (randwrite_bench_test.go). Same file already pools the record-scratch buffer for verifiedRead (recordScratchPool, :376) — same class of per-request buffer left unpooled right next to the one that got pooled.
- **Fix:** Pool a small piece/seg scratch (sync.Pool, reset+reuse like recordScratchPool), or fast-path len==1 without slicing when the whole read lands in one interval.
- **Verified:** CONFIRMED. index.go:254 `var pieces []piece` grown by append; :324 `make([]*segmentMeta, len(pieces))`. Reachable from engine/read_internal.go. Downgraded MED→LOW: both small, pooling fiddly (segs held past sh.mu release through deferred releaseGuards, must be returned on all 4 error returns at :333/356/361). Fix if profile justifies: one sync.Pool holding struct{pieces []piece; segs []*segmentMeta}.

### [HIGH] RestoreToVersion is blind to cold.log — deletes remote-durable files and zero-fills cold ranges instead of preserving them · `bugs` · area: store-api
- **Where:** `pkg/block/journal/store.go:1087`
- **What:** RestoreToVersion phase-1 ceiling replay (store.go:1087-1139) scans only scanValidRecords; never loadCold/applyColdLog like recover() (recovery.go:358). Cold intervals (Evict, SeedCold/SeedColdBatch — store.go:520,607) live only in cold.log+mem index, no seg record (cold.go:129 appendCold). Fully-cold file at V: no vIndex entry, falls into "head not in vIndex" bucket (store.go:1217-1223), unconditionally Delete'd though durable remotely. Mixed warm/cold file: cold sub-ranges absent from `exts`, become POSIX holes after Delete+WriteAt, reads return zeros.
- **Why it matters:** doc.go names HOLE-vs-COLD conflation the module's cardinal sin; recover()'s applyColdLog exists for the identical reason. Only caller runtime/snapshot.go:1539 RestoreToVersion + shares.SeedColdFromManifest (:1598) is gated `if remoteVerify` (:1595) — but RestoreToVersion only runs in the !remoteVerify local-only branch (:1341), so the ONLY production path gets zero compensation. Cold intervals ARE reachable local-only: blockstore_config.go:370 disables eviction for remoteStore==nil but engine.go:271 `SetEvictionEnabled(bs.syncer.IsRemoteHealthy())` re-enables it, syncer.go:309-312 returns true when healthMonitor==nil, Start runs after :370.
- **Fix:** fold loadCold(s.dir) into phase 1 like applyColdLog (insert cold:true/synced:true at version<=V, respect tombstone/truncate clip). Phase 2: re-assert cold vIndex entries via appendCold+insert instead of reading a source record.
- **Verified:** CONFIRMED — zero loadCold/coldEntry refs in whole function; caller's mitigation only fires on the branch RestoreToVersion never takes.

### [HIGH] repackSegment never sets target.records — synced-gate can misclassify a still-dirty repacked segment as evictable · `bugs` · area: reclaim
- **Where:** `pkg/block/journal/reclaim.go:834`
- **What:** repackSegment sets target.liveBytes (:834) and target.syncedRecords (:835), never target.records. records (segment.go:54, "eviction synced-gate denominator") incremented only by live append (segment.go:359) and recovery replay (recovery.go:340); createSegment leaves it 0. evictable() (reclaim.go:207-210) gates on syncedRecords==records. pickVictim (reclaim.go:621-672) picks by dead-byte ratio only, no synced check — an all-dirty victim gives syncedCount==0, so 0==0 reads evictable though holding never-synced data. evictSegment can then cold-mark+unlink it, losing the sole copy. (Mixed victim: opposite bug — permanently unevictable via Evict, lesser leak.)
- **Why it matters:** violates D2 synced-gate ("Evict frees only sealed segments with NO unsynced record"); real data-loss path. TestGCForcedRepackPreservesData only checks syncedRecords, never records, never evicts post-repack.
- **Fix:** add `target.records.Store(int64(len(moves)))` beside the existing Stores at :834-835; exclude tombstone/truncate markers, matching live-path semantics.
- **Verified:** CONFIRMED via grep — only two non-test writers of records package-wide.

### [HIGH] RestoreToVersion re-materializes the V-view via WriteAt (never fsyncs) while its own doc promises durable reconstruction — crash window can erase the file entirely · `gaps` · area: store-api
- **Where:** `pkg/block/journal/store.go:1079`
- **What:** Phase 2 (store.go:1169-1224): s.Delete fsyncs a tombstone via groupCommit (segment.go:426-474), then re-appends V-view bytes with s.WriteAt (:1211) which only buffers, never fsyncs (documented :424-425). No Commit/groupCommit call anywhere after.
- **Why it matters:** doc (store.go:1057-1059) claims "re-materializes that view durably at the log head, so a crash-reopen reconstructs V" — false. Bound by DirtyExpiry (~30s) or next explicit Commit. Crash in that window: tombstone survives, new data doesn't — file reads empty. Each subsequent Delete's groupCommit fsyncs its own shard, so only the last restored file per shard is exposed, but that window is real on a local-only share where the journal is the only copy.
- **Fix:** call sh.groupCommit()/s.Commit(ctx,id) at end of RestoreToVersion (per shard touched) before returning.
- **Verified:** CONFIRMED — zero groupCommit/Commit refs in function; caller snapshot.go:1539-1546 only does DrainRollups after.

### [HIGH] repackSegment never sets target.records — dirty-only repacked segment misclassifies as evictable, breaking write-back cache dirty/clean invariant · `gaps` · area: reclaim
- **Where:** `pkg/block/journal/reclaim.go:834`
- **What:** Same root defect as the bugs-dimension finding above, framed against write-back cache correctness: writeDataRecord (reclaim.go:971-999) never touches seg.records; target.records stays 0 after repack. pickVictim never checks synced state of a victim's live records. All-dirty victim -> syncedCount==0 -> evictable() reads 0==0 true -> Evict/ensureSpace cold-marks+unlinks a segment holding exclusively-dirty (never-carved) data.
- **Why it matters:** reference write-back model (rclone/JuiceFS) requires dirty vs clean gate eviction; D2 contract is "Evict frees only sealed segments with NO unsynced record". Coincidental zero==zero defeats that gate — cold read later fetches from remote bytes never pushed there: silent data loss, not staleness. No test combines GC-repack of a dirty victim with subsequent Evict/ensureSpace.
- **Fix:** `target.records.Store(int64(len(moves)+len(markers)))` right after :834-835, excluding tombstone/truncate consistent with live append semantics.
- **Verified:** CONFIRMED, worst finding in report — evictSegment cold-marks every interval of the segment regardless of iv.synced, then unlinks; defeats Evict's own stated contract (reclaim.go:44-47).

### [HIGH] Torn cold.log tail is never physically truncated at recovery — a crash silently poisons every future append · `gaps` · area: recovery
- **Where:** `pkg/block/journal/recovery.go:358`
- **What:** applyColdLog (recovery.go:358-386) calls loadCold (cold.go:192-212) for the intact prefix but never truncates cold.log on disk. loadSegment does this for .seg (fd.Truncate(validUpTo)+Sync, recovery.go:243-250); no equivalent for cold.log. appendCold reopens O_APPEND (cold.go:138), so new entries land after old garbage tail. Next restart's loadCold walks from 0, parses pre-crash intact entries, hits the still-present garbage, stops (cold.go:203-207) — discards every entry the intervening process appended, permanently.
- **Why it matters:** compactColdLog (recovery.go:395-405), the only rewriteCold caller, gated by `coldLoaded <= 2*len(live)+coldCompactFloor(1024)` — never fires for realistic small/medium logs, so the torn tail is never repaired. Lost cold entry = silent zero-fill instead of remote fetch, per cold.go's own doc.
- **Fix:** have loadCold return the intact byte offset; truncate+fsync cold.log to that offset in applyColdLog before any later append, mirroring loadSegment's active-tail repair.
- **Verified:** CONFIRMED — no wholesale cold.log rewrite outside compactColdLog; reachable via Evict (reclaim.go:280) and SeedCold/SeedColdBatch (store.go:531/625/1290), all production paths.

### [HIGH] Hydrate has no delete-fence, so a racing cold-read resurrects a deleted file · `bugs` · area: cross
- **Where:** `pkg/block/journal/store.go:456`
- **What:** Hydrate (store.go:449-467) gates only via staleAfterTruncate/truncVer. No tombVer/delete-fence map exists (shard.go). Delete (store.go:759-816) removes sh.index[id] outright, leaving no trace. With sh.index[id] nil, fileIndex.hydratable's nil-receiver case (index.go:512-514) fills the whole requested range unconditionally. Race: cold read samples notAfter, starts remote fetch; concurrently Delete tombstones+clears index; fetch completes; Hydrate appendRecord's writes stale pre-delete bytes back, recreating the deleted file.
- **Why it matters:** Delete's own doc promises "a crash can never resurrect a file whose tombstone is already durable" — here no crash needed, plain concurrency resurrects it silently, no error, no log line. Top-severity silent-corruption class (cf. #1850/#1888/#2084).
- **Fix:** add per-shard tombVer[id] map set on Delete; gate Hydrate/hydratable (and appendRecord's re-check under sh.mu) on notAfter <= tombVer the same way truncVer gates.
- **Verified:** CONFIRMED — Delete deletes sh.truncVer[id] too (store.go:807), erasing the only existing fence; reachable via engine/readwrite.go:367 (Delete before manifest reap) racing engine/fetch.go:201 Hydrate whose `at` was sampled at fetch.go:103, window = whole remote fetch (hundreds of ms).

### [MED] Double-active-segment recovery: pointer swap misattributes byte/record counters to the wrong segmentMeta · `slop` · area: recovery
- **Where:** `pkg/block/journal/recovery.go:266`
- **What:** In loadSegment's two-unsealed-actives branch (:257-274), `r.actives[sh], m = m, r.actives[sh]` swaps which struct `m` points at. Demotion bookkeeping (sealed, idxFD close, sealedByShard) correctly applies to demoted `m`. But :286 `r.replayRecords(m, sh, id, recs)` runs AFTER the swap, still using swapped `m` — now the OLD demoted segmentMeta, not the one matching just-scanned id/recs. replayRecords' noteMinVersion/liveBytes.Add/records.Add/syncedRecords.Add credit the wrong segment. loc.SegmentID is set independently to `id` so reads still resolve correctly — only the counters are wrong.
- **Why it matters:** minVersion=0 is the "unset" sentinel (segment.go:57); liveBytes/records/syncedRecords feed GC victim selection and the eviction synced-gate. New active segment looks falsely empty, demoted segment looks falsely more dead. Untested: no recovery_test.go/segment_test.go path exercises two-active-segments. Reachable via repackSegment's createSegment-write-then-seal crashing mid-repack (reclaim.go:~800-832) — "should never happen" comment notwithstanding.
- **Fix:** bind `m` to id/recs throughout; use a separate var for the demoted segment, e.g. capture `cur := m` before the swap and pass `cur` to replayRecords.
- **Verified:** CONFIRMED at recovery.go:265-267/286.

### [MED] Recovery always full-rescans+CRC-verifies .seg payload, .idx sidecar never read · `perf` · area: recovery
- **Where:** `pkg/block/journal/recovery.go:212`
- **What:** loadSegment always calls scanValidRecords(fd, SegmentSize, SegmentSize) — full read+CRC of every record's payload, sealed or not. idxEntry has encode() only, no decode anywhere; nothing in the package ever reads .idx content back. index.go:468-469 docs the sidecar as enabling a re-scan skip; never realized.
- **Why it matters:** O(total journal bytes) read+CRC on every boot instead of O(records*40B) via sidecars; sealed/immutable segments already CRC-verified once at write time get re-verified every restart.
- **Fix:** severity capped at MED not HIGH because idxEntry stores only FileIDHash, not FileID (index.go:479-487) — cannot rebuild the FileID-keyed index without a format change. Correct fix: delete the sidecar and its two write paths (pure write amplification today), not add a decoder.
- **Verified:** CONFIRMED — grep shows idxEntry.decode does not exist; reachable via every Open() -> recover() -> loadSegment.

### [MED] SetCarveTargets writes unsynchronized fields that Carve reads without a lock, contradicting Store's own concurrency contract · `structure` · area: store-api
- **Where:** `pkg/block/journal/carve.go:170`
- **What:** SetCarveTargets does `s.deduper = d; s.sink = sink` on plain non-atomic fields (store.go:181-182). Carve reads them unlocked (carve.go:202). store.go:168-170 documents all exported methods safe for concurrent use, with no carve-out. Sibling one-shot setter SetVerifyReads correctly uses atomic.Bool (store.go:219,249).
- **Why it matters:** data race under the Go memory model. engine/syncer.go wireCarveTargets is explicitly one-shot-with-retry (syncer.go:806-831), returns early when blockCommitter/syncedHashStore not yet set — a started syncer's carve tick can read s.sink concurrently with a later SetRemoteStore attach writing it.
- **Fix:** atomic.Value/atomic.Pointer for deduper+sink (mirror SetVerifyReads), or wire them via Open()/constructor so a returned *Store is always fully formed and SetCarveTargets disappears.
- **Verified:** CONFIRMED; MED not HIGH — production wiring usually completes before Start, no corruption observed.

### [MED] Truncate's truncVer stamp uses the pre-marker peek version, not the marker's real fenced version, leaving a version-range gap where Hydrate un-truncates a file · `bugs` · area: cross
- **Where:** `pkg/block/journal/store.go:968`
- **What:** Truncate stamps sh.truncVer[id] = s.version.Load() at peek time (:968), before appendTruncateMarker (:976) assigns the marker's real Version via s.nextVersion() (segment.go:481-522) under a separate lock. truncVer is never raised to the real marker version afterward — the clip section only uses a local var. staleAfterTruncate (store.go:473-475) fences only notAfter <= Vpeek, not Vmarker. Any unrelated write between peek and marker-fsync lands `at` in (Vpeek,Vmarker], passing the fence for a Hydrate racing the clip window; hydratable treats the not-yet-clipped span as an unversioned hole and fills it, and the resulting interval's fresh version can exceed truncVer, surviving the clip's own `iv.version > truncVer` test — persistent post-clip resurrection, not just transient.
- **Why it matters:** Truncate's own doc says "a crash after the marker can never resurrect the truncated bytes" — this resurrects without any crash, via ordinary concurrent cold-read traffic.
- **Fix:** after appendTruncateMarker returns its real version, raise sh.truncVer[id] = max(existing, truncVer) in the same locked clip section (~:981).
- **Verified:** CONFIRMED; MED not HIGH — narrower window than the Hydrate delete-fence finding, needs `at` in a 1-version band plus a manifest row still covering past newSize.

### [MED] recordHasDirtyFragment rescans the whole file's interval list per touched physical record on every block flip · `perf` · area: carve
- **Where:** `pkg/block/journal/carve.go:888`
- **What:** flipUpTo (:844) builds `touched` records past watermark; for each, recordHasDirtyFragment does `for k := range fi.ivs` over the ENTIRE file's interval slice, though fi.ivs is sorted by fileOff (index.go:85-88) and a record's live fragments can only fall in that record's own [fileOff,fileOff+len) window. Runs once per touched record per block, under sh.mu.
- **Why it matters:** O(touched_records x total_file_intervals) per carve pass for heavily-overwritten/fragmented files, under the shard lock.
- **Fix:** bound scan via sort.Search on fi.ivs (same idiom as findRecord), or maintain a live dirty-fragment refcount per physical record incrementally instead of rescanning.
- **Verified:** CONFIRMED, carve.go:886-898.

### [MED] repackSegment re-walks entire shard index a 2nd time; evictSegment's twin already avoids this exact walk · `perf` · area: reclaim
- **Where:** `pkg/block/journal/reclaim.go:846`
- **What:** Step 1a (:731-746) scans sh.index building `moves`. Step 4 (:843-865) scans sh.index again from scratch to repoint, though only files already in `moves` can match. gcShard loops repackSegment per victim (:554-579): O(V x total shard files) instead of O(V x touched files).
- **Why it matters:** evictSegment (:245-310) already solved this with a `backed` map from its first scan (:273-276), narrow second pass (:296-308), plus a ponytail comment naming the remaining walk as accepted debt. repackSegment has neither.
- **Fix:** build `movedFiles := map[FileID]struct{}` alongside `moves` in step 1a; iterate only those in step 4. Note: narrows the `remaining` catch-all's visibility (:851-856), same tradeoff evictSegment already accepted.
- **Verified:** CONFIRMED, reclaim.go:731-746 / 843-865 vs evictSegment's 273-308.

### [MED] Full per-record payload read into memory during scan is never used by recovery · `perf` · area: recovery
- **Where:** `pkg/block/journal/recovery.go:212`
- **What:** scanValidRecords/readRecordAt(scratch=nil) allocates a fresh buffer holding header+fileID+payload+CRC per record; recs[] keeps them all alive until loadSegment returns. replayRecords/rebuildIdx/RestoreToVersion phase1/victimMarkers only read header fields, segOff, fileID — grep confirms no .payload touch on the scan-only paths.
- **Why it matters:** record.go's own comment justifying no-scratch ("recovery keeps every payload it scans") is stale for these callers — pure GC churn per segment, up to SegmentSize (256MB default) held live per segment, twice over (boot + RestoreToVersion).
- **Fix:** bundled with the .idx-sidecar finding above if that lands (most segments skip scan entirely). Otherwise thread a reusable scratch buffer through scan callers that don't need payload — but copy fileID out first (aliases scratch, <=maxFileIDLen) before reuse.
- **Verified:** CONFIRMED; disk read itself is mandatory for CRC verification, only the payload-retention/no-scratch part is waste.

### [MED] Optional BlockSink capabilities are wired by unexported type assertion with no compile-time or structural conformance check · `bloat` · area: carve
- **Where:** `pkg/block/journal/carve.go:118`
- **What:** supersededReaper (:118), manifestRowEnder (:133), clobberGuard (:160) are unexported interfaces sniffed via type assertion at :339, :500, :675. Unexported means no other package can write a `var _ supersededReaper = (*engineBlockSink)(nil)` guard. Contrast: BlockSink itself is compiler-checked at every SetCarveTargets call because that param is the exported journal.BlockSink. grep of engine/blocksink.go for `var _ ` guards: none.
- **Why it matters:** a method-name/signature typo on either side compiles clean and silently disables the capability — indistinguishable from the documented "test fakes don't implement it" fallback (:117/126/158). Silent-manifest-drift class this package has shipped bugs in before (stale superseded rows, lost clobbered-row ranges, unwidened runs).
- **Fix:** add a compile-time guard in pkg/block/engine (blocksink.go or a _test.go) asserting engineBlockSink/localBlockSink satisfy the full three-method set via a local interface literal, keeping journal-side interfaces unexported.
- **Verified:** CONFIRMED, no `var _ ` conformance guard anywhere for these three.

### [LOW] appendTombstone/appendTruncateMarker never advance sh.lastVersion · `bugs` · area: append-index-record
- **Where:** `pkg/block/journal/segment.go:426`
- **What:** appendTombstone (:426-474) and appendTruncateMarker (:481-526) mint version via s.nextVersion() (:450,503), write, unlock (:465,519), never set sh.lastVersion = version. appendRecord does, at :357, under the same lock. recovery.go:478 restores it correctly from r.maxVersion on restart.
- **Why it matters:** shard.go:41-46 documents lastVersion as "highest record Version appended to this shard"; groupCommit's fsync ceiling (store.go:700), dirty()'s auto-commit gate (shard.go:121-129), sealSegment's markSynced all key off it. Currently masked because both ops self-fsync via groupCommit before returning — but the field stays permanently understated until another write lands on the shard, a landmine for any future consumer trusting it (skip-if-clean optimization, metric, durability check).
- **Fix:** add `sh.lastVersion = version` under sh.mu in both functions, mirroring appendRecord, before the Unlock at :465/:519.
- **Verified:** CONFIRMED; understated only, never overstated — no reachable incorrect behavior today, hence LOW not HIGH.

## Implementation gaps vs reference

| Expected behavior (checklist) | Our behavior | Gap | Source |
|---|---|---|---|
| Data write durable before index/visible state flips — never index-before-data (#3) | `RestoreToVersion` phase 2 (store.go:1199-1214): `Delete` fsyncs tombstone via groupCommit, then `WriteAt` re-materializes V-view but only buffers — no `Commit`/groupCommit call anywhere after. Doc (store.go:1057-1059) claims "durably" regardless. | Crash in DirtyExpiry window (~30s) after restore: durable tombstone survives, re-materialized data doesn't → file reads empty, not pre- or post-restore state. Fix: one `s.Commit(ctx,id)` / per-shard `groupCommit()` at end of `RestoreToVersion`. | Bitcask write-then-update-KeyDir ordering — https://riak.com/assets/bitcask-intro.pdf |
| Dirty (local-only) vs clean (synced-to-remote) must gate eviction; only clean data reclaimable (#7) | `repackSegment` (reclaim.go:833-835) sets `target.liveBytes`/`target.syncedRecords` but never `target.records`. Fresh `segmentMeta.records` defaults 0; only live-append (segment.go:359) and recovery replay (recovery.go:340) increment it. `pickVictim` (reclaim.go:621-670) never checks synced state, so an all-dirty victim can be repacked. Result: `syncedRecords(0)==records(0)` → `evictable()` (reclaim.go:207-210) reads a dirty-only repacked segment as fully synced. | GC repack of an unsynced-only victim, then `Evict`/`ensureSpace` cold-marks and unlinks it → data never carved to remote, later cold read gets zeros. Silent data loss, no test combines repack-of-dirty-victim with subsequent evict. Fix: `target.records.Store(int64(len(moves)))` beside reclaim.go:834-835. | rclone `--vfs-write-back` (no cleanup until backend write completes) — https://rclone.org/commands/rclone_mount/; JuiceFS cache_management (loss before upload = permanent) — https://juicefs.com/docs/community/cache_management/ |
| Torn trailing record after crash detected via checksum, log truncated at that point — not silently skipped (#4) | `applyColdLog` (recovery.go:358-386) calls `loadCold` which stops at first torn entry and returns only the intact prefix — but never truncates `cold.log` on disk. `appendCold` reopens O_APPEND, so new entries land after the old garbage tail. `compactColdLog`'s only rewrite path is gated by a threshold (`coldCompactFloor=1024`) that never fires on a small/medium log. | Next restart's `loadCold` walks from offset 0, hits the same old tear, stops — discarding every well-formed entry appended since the first crash, permanently. Contrast: `loadSegment` (recovery.go:243-250) does `Truncate(validUpTo)+Sync` on a torn active `.seg`; cold.log gets no equivalent. Fix: have `loadCold` return intact byte length, truncate+fsync cold.log in `applyColdLog` before any append can extend it. | LevelDB log format resync-after-truncation — https://github.com/google/leveldb/blob/main/doc/log_format.md; SQLite WAL chained-checksum torn-tail detection — https://www.sqlite.org/walformat.html |
| Index/manifest fully rebuildable from the append-only log alone — never a second source of truth that can silently diverge (#5) | `supersededReaper`/`manifestRowEnder`/`clobberGuard` (carve.go:118,133,160) are unexported; detected only by runtime type-assertion (carve.go:339,500,675) with silent no-op on failed assertion. Zero `var _ <iface> = engineBlockSink{}` compile-time guard anywhere in repo — can't exist across the package boundary since the interfaces aren't exported. | If a production sink's method is renamed/dropped in a refactor, manifest silently stops being reaped/ended — no build error, no log, no test failure — and can serve stale bytes on cold read (carve.go's own doc, 313-338). No live bug today; LOW since risk hasn't materialized. Fix: export the 3 interfaces + add compile-time `var _` guards in engine/blocksink.go. | Bitcask KeyDir/hint-file rebuild — https://riak.com/assets/bitcask-intro.pdf; Haystack index rebuild / orphan-needle detection — https://www.usenix.org/conference/osdi10/finding-needle-haystack-facebooks-photo-storage |
| Two independent eviction triggers combine: configured quota + live free-disk headroom via `statfs`, not just internal byte accounting (#8) | `diskFreeBytes` (statfs_unix.go:10) called exactly once, in `Open` (store.go:276-283), to derive a static `cfg.MaxLocalBytes = free*0.8`. `ensureSpace` (reclaim.go:328-335) gates purely on `s.diskBytes.Load()` vs that static cap — never re-polls statfs. | External disk pressure (another share, another process on same volume) after `Open` produces no backpressure until the stale cap happens to still hold. Doc itself calls `MaxLocalBytes` "a soft pressure threshold" so not correctness-breaking — LOW. Fix: re-probe `diskFreeBytes(s.dir)` periodically in/near `ensureSpace`, treat low live headroom as a second independent trigger. | rclone `--vfs-cache-max-size`/`--vfs-cache-min-free-space` combo — https://rclone.org/commands/rclone_mount/; JuiceFS `--free-space-ratio` via statfs — https://juicefs.com/docs/community/cache_management/ |
