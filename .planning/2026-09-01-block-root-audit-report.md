# Audit Report — audit-engine (4ec814bc2)
Stack: Go (module github.com/... single go.mod at repo root, no separate module under pkg/block). Package `block` (doc.go: "package blockstore" historically, now `block`) at pkg/block root — 14 non-test .go files, 2,038 LOC. Stdlib-only within this scope except `lukechampine.com/blake3` (objectid.go). No frameworks, no codegen, no build tooling beyond `go build`/`go test`. Consumed by sibling packages engine/, journal/, local/, remote/, compression/, encryption/, blockcodec/, blockstoretest/ (all import "pkg/block" for the shared types) — those are OUT OF SCOPE for this audit (separately audited) and exist here only as grep targets for reachability/callers/pinning tests. · Areas: 3 · Findings: 0 HIGH / 2 MED / 20 LOW

## Summary by dimension

| Dimension | HIGH | MED | LOW |
|---|---|---|---|
| bugs | 0 | 2 | 5 |
| security | 0 | 0 | 0 |
| slop | 0 | 0 | 4 |
| perf | 0 | 0 | 2 |
| structure | 0 | 0 | 4 |
| bloat | 0 | 0 | 1 |
| comments | 0 | 0 | 4 |

## Summary by area

| Area | Findings |
|---|---|
| cross | 3 |
| manifest-identity-types | 5 |
| store-contract-policy | 13 |
| structures-and-legacy | 1 |

## Findings

### [LOW] doc.go Sub-packages list names 3 directories that don't exist under pkg/block · `structure` · area: store-contract-policy
- **Where:** `pkg/block/doc.go:101`
- **What:** Lines 101-121 "Sub-packages" list 8 entries incl migrate, gc, storetest. migrate/ doesn't exist anywhere. gc/ isn't a pkg/block dir (gc.go/gc_block.go are files inside engine/). storetest/ isn't under pkg/block (that's pkg/metadata/storetest, different pkg).
- **Why it matters:** Repo has demonstrated pattern (3 prior confirmed instances) of doc.go overclaiming sub-packages. Contributor grepping pkg/block/migrate or pkg/block/gc wastes a round trip; misdirects on where GC/migration logic lives (embedded in engine/, already audited). List also omits real sub-packages journal, compression, encryption, blockcodec.
- **Fix:** Drop migrate and gc bullets (no such packages; GC/migration live in engine/, documented there). Fix storetest bullet to point at pkg/metadata/storetest or delete. Add missing journal, compression, encryption, blockcodec entries.
- **Verified:** CONFIRMED. Actual pkg/block dirs: blockcodec, blockstoretest, chunker, compression, encryption, engine, journal, local, remote. No migrate/, gc/, storetest/ under pkg/block.

### [LOW] Doc comments reference nonexistent type "BlockStore" — the interface is named Store · `comments` · area: store-contract-policy
- **Where:** `pkg/block/blockstore.go:1`
- **What:** blockstore.go:1,19-20,27,93; doc.go:43,58,112; errors.go:201 all say "BlockStore." (e.g. "BlockStore.Head and BlockStore.Walk", "BlockStore.Get") but no such type exists — the declared interface is `type Store`.
- **Why it matters:** Stale name from pre-rename state; godoc-style refs resolve to nothing, misleads a reader grepping for the type by name.
- **Fix:** Replace "BlockStore." with "Store." throughout blockstore.go, doc.go, errors.go.
- **Verified:** CONFIRMED. grep "type BlockStore" in pkg/block finds nothing; interface is `type Store`. All 8 cited refs present verbatim.

### [LOW] ErrUnknownHash doc cites a file that doesn't exist and the wrong package for AddRef · `comments` · area: store-contract-policy
- **Where:** `pkg/block/errors.go:145`
- **What:** Comment says "The LRU hit path (Opt 1 — see pkg/block/local/fs/rollup.go)". No rollup.go exists anywhere. AddRef lives in pkg/metadata/store/{badger,sqlite,postgres,memory}/objects.go and store/sql/chunks.go, not pkg/block/local/fs.
- **Why it matters:** Stale/misleading pointer; sends reader chasing a nonexistent file. Sentinel is prod-live (badger/objects.go:335, memory/objects.go:421/425).
- **Fix:** Drop the "see pkg/block/local/fs/rollup.go" pointer, or repoint at the actual AddRef sites under pkg/metadata/store/.
- **Verified:** CONFIRMED. find -name 'rollup*.go' empty; local/fs holds only fs.go/legacy*.go. Comment-only, severity LOW.

### [LOW] chunker sub-package bullet contradicts doc.go's own removed-migration-tool statement · `comments` · area: store-contract-policy
- **Where:** `pkg/block/doc.go:114`
- **What:** Line 114-115: "chunker: FastCDC chunker used by both writes and by the migration tool." Contradicts doc.go:75-77: "dfs migrate-to-cas shipped through v0.21 and has been removed."
- **Why it matters:** Self-contradictory within same doc block. Bonus: doc.go:116-117 lists a `migrate` sub-package with MigrateShareToCAS — dir doesn't exist, symbol not found by grep.
- **Fix:** Cut "and by the migration tool" from chunker bullet; delete the migrate bullet entirely.
- **Verified:** CONFIRMED. Real chunker importers: journal/{carve,index,store}.go, local/fs/fs.go, runtime/shares/blockstore_config.go, internal/dfsbench. No migration tool consumes it.

### [LOW] ChunksForPayload silently accepts offsets above the package's own MaxInt64 ceiling, disagreeing with ParseChunkOffset/ChunkOffsetFor on the same ID string · `bugs` · area: cross
- **Where:** `pkg/block/ids.go:125`
- **What:** ChunksForPayload parses trailing numeric component via bare `strconv.ParseUint(suffix, 10, 64)` (ids.go:122), no `> math.MaxInt64` reject, unlike ParseChunkOffset (:28-29) and ChunkOffsetFor (:68-69). Doc comment (:16-21) says the guard "keeps the conversion total for all callers" — this caller skips it.
- **Why it matters:** Reachable via memory/objects.go:510 and store/sql/chunks.go:199 (backs ListFileChunks for memory/sqlite/postgres). But keeping unplaceable rows is deliberate/documented (ids.go:105-107, "dropping it would hide the damage"), so membership divergence isn't a bug. Real delta: such a row sorts LAST at a huge key instead of the documented "keys as 0" (ids.go:114) — an ordering/doc mismatch, corrupted-row-only.
- **Fix:** Route through ChunkOffsetFor(r.ID, payloadID), default off=0 when !ok, so all offset-placement decisions share one guarded impl.
- **Verified:** CONFIRMED but narrower than claimed. Downgraded MED→LOW: doc-mismatch on ordering of corrupted rows, not a membership/correctness bug.

### [LOW] FileChunk struct has inconsistent JSON field tagging · `structure` · area: manifest-identity-types
- **Where:** `pkg/block/types.go:242`
- **What:** Only LastSyncAttemptAt (:282, `json:"last_sync_attempt_at,omitempty"`) and State (:289, `json:"state"`) tagged; ID/Hash/DataSize/StartOffset/RefCount/LastAccess/CreatedAt untagged, serialize as bare Go names. ChunkRef in same file is fully tagged snake_case.
- **Why it matters:** FileChunk IS json-marshaled in prod — badger's on-disk value (store/badger/objects.go:347 `json.Marshal(&block)`, read back :449 `json.Unmarshal(val, &fc)`). Six on-disk key names are unpinned Go field names: a field rename silently changes the badger record layout, old rows decode with zero values — same class as this repo's recurring silent-zeros bugs.
- **Fix:** Tag all 8 fields snake_case matching ChunkRef — but it's a format change, needs a format-version bump or dual-read, not a blind retag.
- **Verified:** CONFIRMED, more load-bearing than the claim assumed (on-disk encoding, not just a hypothetical debug dump).

### [LOW] doc.go documents a TRANSITIONAL-marker grep convention with zero adoption anywhere in the repo · `structure` · area: store-contract-policy
- **Where:** `pkg/block/doc.go:123`
- **What:** Lines 123-146: 24 lines specifying TRANSITIONAL-PHASE-N / TRANSITIONAL-NEXT-MILESTONE marker convention + grep recipe. Repo-wide grep for TRANSITIONAL- hits only doc.go itself (:128/132/144) — zero usages.
- **Why it matters:** Dead process doc; the documented grep returns empty, indistinguishable from "convention abandoned." Repo already has a live, adopted marker convention (`ponytail:`) doing this job.
- **Fix:** Delete the 24 lines.
- **Verified:** CONFIRMED. Zero adopters repo-wide.

### [MED] ComputeObjectID's Merkle root hashes only ChunkRef.Hash, ignoring Size/StartOffset — two files with genuinely different byte content can collide on ObjectID and be conflated by the file-level dedup short-circuit · `bugs` · area: cross
- **Where:** `pkg/block/objectid.go:36`
- **What:** ComputeObjectID folds only `blocks[i].Hash` into BLAKE3 Merkle root (line 39-40), never Size/StartOffset. Hash = hash of FULL underlying chunk (types.go:249-271, 146-165); Size/StartOffset pick the sub-range a row actually claims, narrows on truncate/carve (types.go:264-270) while Hash stays same. Two ChunkRefs, same Hash H, diff Size/StartOffset (e.g. full 8MiB chunk vs same chunk narrowed to 2nd half 4MiB) → genuinely diff bytes, diff file size, same ComputeObjectID.
- **Why it matters:** ObjectID persisted every quiesce: file_modify.go:458, sparse.go:91. Doc's own dedup-short-circuit story (coordinator FindByObjectID/PersistFileChunks/ErrObjectIDConflict) implied silent content conflation across files — refuted on verify: `applyFileLevelDedupHit`/`TrySpeculativeFileLevelDedup` = 0 Go hits, no `engine/dedup.go`, `FindByObjectID` has no non-test caller, `ErrObjectIDConflict` declared and never consumed. Real consequence: UNIQUE index is live (postgres `000013_object_id.up.sql`, sqlite `000001` `inodes_object_id_idx ... WHERE object_id IS NOT NULL`) → colliding 2nd file hits unhandled 23505 on SetManifest → spurious write failure, not silent corruption. objectid_test.go's `TestComputeObjectID_MutationDiff` only flips a Hash byte, never varies Size/StartOffset — zero coverage either way.
- **Fix:** Fold Size + StartOffset into BLAKE3 input alongside Hash (`h.Write(blocks[i].Hash[:]); binary.Write(h, binary.BigEndian, blocks[i].StartOffset); binary.Write(h, binary.BigEndian, blocks[i].Size)`). Wire-format change — bump `objectIDDomainPrefix` to v2 per its own doc comment, needs migration note.
- **Verified:** DEFECT CONFIRMED, stated dedup-conflation harm REFUTED — no consumer wired. Real effect narrower: unhandled unique-constraint violation on collision, not silent conflation. Reachable in prod (truncate keeps straddling ref intact per PruneChunkRefsToSize, ObjectID recomputed unchanged over it — collision needs no exotic setup).

### [MED] DEALLOCATE's two-phase punch has no rollback: metadata-manifest prune commits durably before the block-store punch that actually zeroes bytes, and a phase-2 failure is reported to the client with phase-1 damage standing · `bugs` · area: cross
- **Where:** `pkg/metadata/sparse.go:99`
- **What:** `metadata.Service.PunchHole` prunes via `block.PunchHole` (sparse.go:84) and commits with `store.SetManifest` at sparse.go:99 — removes every fully-contained ChunkRef from manifest, so SEEK_HOLE/READ_PLUS now report hole there. Caller `internal/adapter/nfs/v4/handlers/deallocate.go:67-91` calls `metaSvc.PunchHole` FIRST (commits prune), then `blockStore.PunchHole` (engine) to zero bytes + reap refcounts. Phase-2 fail (I/O err, ctx cancel) → handler returns NFS4ERR_IO at :95, no compensating write, no re-add of pruned refs.
- **Why it matters:** Handler's own comment (:73-80): read path resolves via dual-read shim off empty ChunkRef list, so metadata prune alone does NOT guarantee zero reads — only the block-store zero-overwrite does. Failed phase 2 leaves exactly that gap permanently: manifest says hole, refcounts never decremented (orphan leak blocking GC), bytes never zeroed — a plain READ can surface old pre-punch bytes while SEEK_HOLE disagrees. Client told op failed, no reason to retry, nothing drives convergence. Same residency-truth-failure class as RestoreToVersion/Truncate, one layer up from engine.Store.PunchHole itself.
- **Fix:** Either reorder (engine zero+reap first, SetManifest after — physical-before-metadata, matches rule used elsewhere) or, if metadata-first order must stay for FlushPendingWriteForFile serialization, repair the manifest (re-add res.PreOpBlocks for still-physically-present chunks, or route through manifest_repair.go) when blockStore.PunchHole fails.
- **Verified:** CONFIRMED. sparse.go:99 commits durably; deallocate.go:70→91→95 no compensating write on phase-2 error. Reachable via live NFSv4.2 DEALLOCATE (non-test, wired through h.Registry/common.ResolveForWrite). Severity HIGH→MED: needs phase-2 failure to fire (I/O error, ctx cancel), not every call.

### [LOW] doc.go claims 'migrate' and 'gc' sub-packages that do not exist under pkg/block · `slop` · area: store-contract-policy
- **Where:** `pkg/block/doc.go:116`
- **What:** `# Sub-packages` lists `migrate: ... MigrateShareToCAS` and `gc: Mark-sweep garbage collection ...` as children of pkg/block. Neither dir exists (`ls pkg/block/migrate`, `ls pkg/block/gc` = nothing). `gc.go`/`gc_block.go`/`gcstate.go` actually live in `pkg/block/engine/`. `MigrateShareToCAS` appears NOWHERE else in repo — hallucinated symbol.
- **Why it matters:** Contributor greps `pkg/block/migrate` or `MigrateShareToCAS` per this doc, finds nothing — wasted time, false confidence a dedicated GC/migrate package exists. Same overclaiming pattern flagged 3x elsewhere.
- **Fix:** Delete `migrate:` bullet (tool already removed per doc.go's own "# On-disk format versions" section). Repoint `gc:` bullet at `pkg/block/engine`.
- **Verified:** CONFIRMED. No `pkg/block/migrate`, 0 hits for `package migrate` or `MigrateShareToCAS` outside doc.go. gc lives in `pkg/block/engine/`. Doc-only, zero runtime effect → severity HIGH→LOW. Duplicate finding across idx 0/1/2/7 — count once.

### [LOW] doc.go Sub-packages list names three directories that don't exist under pkg/block · `bloat` · area: store-contract-policy
- **Where:** `pkg/block/doc.go:116`
- **What:** `migrate:` (116-117) and `gc:` (118-119) — no such dirs; gc lives in `pkg/block/engine/`. `storetest:` (120-121) claims "Legacy conformance test suites for higher-level FileChunkStore implementations" as pkg/block child — real pkg is `pkg/metadata/storetest` (per CLAUDE.md: "Store contracts live in pkg/metadata/storetest"), not under pkg/block at all.
- **Why it matters:** 3 of 8 bullets in Sub-packages section false. Contributor greps pkg/block/{migrate,gc,storetest}, finds nothing. Same overclaiming pattern confirmed 3x elsewhere (doc rule: comments must be factually true against actual layout).
- **Fix:** Delete `migrate`, `gc`, `storetest` bullets. If gc location worth noting: "gc: mark-sweep GC lives in pkg/block/engine (gc.go et al.), not a separate package."
- **Verified:** CONFIRMED, same defect as idx 0 plus storetest bullet. Real pkg/block sub-dirs: blockcodec, blockstoretest, chunker, compression, encryption, engine, journal, local, remote. Duplicate of idx 0/2/7 — count once. Doc-only → LOW.

### [LOW] doc.go sub-package list names two directories that don't exist and misattributes a third · `comments` · area: store-contract-policy
- **Where:** `pkg/block/doc.go:116`
- **What:** `migrate:` (116-117), `gc:` (118-119) claimed as pkg/block sub-packages — neither exists (`Glob pkg/block/migrate/**`, `Glob pkg/block/gc/**` = nothing). gc.go lives in `pkg/block/engine/` (gc.go, gc_block.go, gc_sweep_index.go, gcstate.go). No migrate/ anywhere (offline migration tool removed, per doc.go's own 75-77). `storetest:` (120-121) — no `pkg/block/storetest`; description matches `pkg/metadata/storetest`.
- **Why it matters:** Stale doc gives false directory map. 4th/5th/6th instance of same overclaiming pattern in this one list.
- **Fix:** Delete `migrate`+`gc` bullets (or repoint gc at pkg/block/engine); delete or repoint `storetest` at pkg/metadata/storetest.
- **Verified:** CONFIRMED, duplicate of idx 0/1/7. No pkg/block/migrate, no pkg/block/gc, no pkg/block/storetest; gc in engine, storetest in pkg/metadata. Doc-only → LOW, no runtime path.

### [LOW] ContentHash.UnmarshalJSON legacy array form skips length validation — silently accepts a truncated/oversized hash instead of erroring · `bugs` · area: manifest-identity-types
- **Where:** `pkg/block/types.go:115`
- **What:** Legacy branch: `var arr [HashSize]byte; json.Unmarshal(data, &arr)`, accepts on `err == nil`. encoding/json array-into-fixed-array: silently zero-pads short arrays, silently truncates long ones — never errors on length mismatch. `[]` → zero hash, no error. 10 or 40-element array → wrong hash, no error. Sibling branches (hex :125, base64 :133) both gate on `len(...) == HashSize` first — array branch is the ONLY one with no check.
- **Why it matters:** Package's own ErrFutureFormat rationale explicitly rejects "decodes cleanly into ... the wrong thing." ContentHash flows into FileChunk.Hash/ChunkRef.Hash → dedup lookups, read-path chunk resolution.
- **Fix:** Unmarshal into `[]byte` first, check `len(raw) == HashSize`, else return ErrInvalidHash-wrapped error — mirror hex/base64 branches.
- **Verified:** CONFIRMED by execution: `json.Unmarshal([]byte("[1,2,3]"), &[32]byte{})` → err=nil zero-padded; 40-element → err=nil truncated to 32. Reachable: badger persists File/FileChunk as JSON (block_record_store.go:40 et al.), production decode path. Severity MED→LOW: array form only in v0.14.x-era rows (always 32 elements historically) — mismatch needs already-corrupt data; wrong-but-valid hash still fails closed downstream (ErrChunkContentMismatch/ErrChunkNotFound), not silent bad bytes served.

### [LOW] doc.go Sub-packages list names two directories that don't exist under pkg/block (migrate, gc) plus a third pointing at the wrong tree (storetest) · `bugs` · area: store-contract-policy
- **Where:** `pkg/block/doc.go:101`
- **What:** Lines 101-121, 7 'sub-packages' incl. `migrate: ... MigrateShareToCAS`, `gc: Mark-sweep garbage collection ...`, `storetest: Legacy conformance test suites ...`. None exist under pkg/block: gc.go lives in `pkg/block/engine/gc.go`; real storetest is `pkg/metadata/storetest` (referenced from `pkg/metadata/storetest/file_block_ops.go`). Also `chunker` bullet (114-115) says chunker "used by both writes and by the migration tool" while doc.go's own text (75-78) says that tool "has been removed" — self-contradictory.
- **Why it matters:** Every exported doc comment must be factually true against actual layout; demonstrated pattern (3 prior confirmed instances) of doc.go overclaiming. Misdirects on where mark-sweep GC / migration logic actually lives.
- **Fix:** Delete `migrate`+`gc` bullets; fix or drop `storetest` (point at pkg/metadata/storetest); drop "and by the migration tool" clause from chunker bullet.
- **Verified:** CONFIRMED, duplicate of idx 0/1/2 plus one extra true point: doc.go:114-115 vs :75-77 self-contradict on migration tool's existence. Doc-only → LOW.

### [LOW] doc.go package-doc first line names the wrong package ("blockstore" vs actual package "block") · `bugs` · area: store-contract-policy
- **Where:** `pkg/block/doc.go:1`
- **What:** Line 1: "Package blockstore defines the unified content-addressed block storage contract..." — but `blockstore.go:12` declares `package block`. Doc still uses pre-rename name.
- **Why it matters:** Go convention (and go/doc tooling) expects doc comment to start "Package \<actual-name\>"; go doc / pkg.go.dev renders mismatched header. Same overclaiming pattern flagged elsewhere in this file.
- **Fix:** Change line 1 to "Package block defines...".
- **Verified:** CONFIRMED. doc.go:1 says "Package blockstore..."; blockstore.go declares `package block`; doc.go's own last line says `package block`. Cosmetic, no runtime effect → severity MED→LOW.

### [LOW] TRANSITIONAL-marker convention documented in doc.go is used nowhere in the entire repository · `bugs` · area: store-contract-policy
- **Where:** `pkg/block/doc.go:123`
- **What:** Lines 123-146 document `TRANSITIONAL-PHASE-N`/`TRANSITIONAL-NEXT-MILESTONE` as a live grep convention, doc.go:144 literally says `grep -rn 'TRANSITIONAL-' ./pkg/block`. Repo-wide grep for `TRANSITIONAL-` hits only doc.go itself + `.planning/audits/2026-08-05-dataplane/report.md:1088` (a prior audit that already flagged this exact paragraph as dead and suggested pointing at `engine/cache.go` instead — that site has no marker either).
- **Why it matters:** Guard/convention never actually used is unverified, not proven — same class as "a guard that never fired." Documents process that doesn't exist.
- **Fix:** Delete lines 123-146 (prior audit already recommended this, still unused).
- **Verified:** CONFIRMED. Only files containing the string: pkg/block/doc.go itself and the planning report. Zero adopters anywhere, including the fallback site. Doc-only → severity MED→LOW.

### [LOW] ContentHash.UnmarshalJSON legacy array branch skips length validation — malformed input silently decodes to a wrong/zero hash instead of erroring · `slop` · area: manifest-identity-types
- **Where:** `pkg/block/types.go:115`
- **What:** v0.14.x legacy-array branch: `if data[0]=='['{ var arr [HashSize]byte; if json.Unmarshal(data,&arr)==nil { *h=ContentHash(arr); return nil } }`. Array-into-fixed-array: short array zero-padded, long array truncated, no error either way. `[]` → all-zero hash, no error. Sibling hex branch checks `len==HashSize*2`, base64 checks `len==HashSize` — only array branch unchecked.
- **Why it matters:** Answers the "can dual-JSON tolerance produce ambiguous decode" question directly — yes, for array branch. ContentHash zero value is the documented pending-sentinel for FileChunk.Hash, so a corrupted legacy array on a real row decodes cleanly to that sentinel with no error — "malformed record decodes into a different, valid-looking state" shape.
- **Fix:** Decode into `[]byte` first, check `len == HashSize`, return same ErrInvalidHash-wrapped error as sibling branches instead of letting array-fill silently succeed.
- **Verified:** Confirmed at types.go:115-125 (no length check) vs hex :127 / base64 :137 (both gated). Verified by running: `[]`→nil,zero; `[1,2,3]`→nil,padded; 40-elem→nil,truncated to 32. Reachable: badger encoding.go:84 marshal / :456 unmarshal → ChunkRef.Hash → this func. Downgraded MED→LOW: no producer ever emits wrong-length array (encoding/json always wrote 32 for [32]byte); byte-truncated badger value fails JSON parse outright — wrong-decode needs a corrupt/foreign writer, hardening gap not live bug. Duplicate of idx 5 — prefer deleting the array (and base64) branches per idx 8's fix over adding a length check alone.

### [LOW] doc.go claims 'storetest' as a sub-package of pkg/block — it is actually pkg/metadata/storetest · `slop` · area: store-contract-policy
- **Where:** `pkg/block/doc.go:120`
- **What:** `storetest: Legacy conformance test suites for higher-level FileChunkStore implementations` listed under `# Sub-packages` — implies pkg/block/storetest, no such dir. Real package is `pkg/metadata/storetest`, a sibling tree, not a child of pkg/block.
- **Why it matters:** Same overclaiming-doc pattern flagged as primary audit target; misleads on package ownership boundaries.
- **Fix:** Drop bullet or correct path to `pkg/metadata/storetest`, note it's not a child.
- **Verified:** Confirmed: doc.go:120-121. `ls pkg/block/` = blockcodec, blockstoretest, chunker, compression, encryption, engine, journal, local, remote — no storetest. Real pkg is `pkg/metadata/storetest` (file_block_ops.go et al.), sibling tree. Doc-only → LOW not MED.

### [LOW] doc.go's 'Transitional-marker convention' documents markers that are used nowhere in the codebase · `slop` · area: store-contract-policy
- **Where:** `pkg/block/doc.go:123`
- **What:** Describes `TRANSITIONAL-PHASE-N:`/`TRANSITIONAL-NEXT-MILESTONE:` as established grep-able convention. `grep -rn 'TRANSITIONAL-' .` repo-wide returns doc.go as ONLY match — zero call sites use either marker.
- **Why it matters:** Presented present-tense as codebase fact ("the goal is for `grep -rn 'TRANSITIONAL-' ./pkg/block` to enumerate every deferral surface") but never adopted. Contributor following doc gets zero results, wrongly concludes no deferred-deletion debt exists — debt does exist (legacy_cas.go migration-only surface), just unmarked.
- **Fix:** Either apply markers to real transitional surfaces (legacy_cas.go, `remote.LegacyCASStore` legacy path), or delete this aspirational section.
- **Verified:** Confirmed: `grep -rn 'TRANSITIONAL-'` over whole worktree hits only doc.go:128,132,144 (definition + own grep recipe) + `.planning/audits/2026-08-05-dataplane/report.md:1088` (already flagged this exact paragraph dead a month ago; its suggested fallback site `engine/cache.go` carries no marker either). Zero adopters. Doc-only → LOW.

### [LOW] ContentHash.MarshalJSON allocates+discards via CASKey() instead of encoding hex directly into the preallocated buffer · `perf` · area: manifest-identity-types
- **Where:** `pkg/block/types.go:98`
- **What:** MarshalJSON preallocates `out` exact-cap (99) then calls `h.CASKey()` (101), which does `hex.EncodeToString` (2 allocs) + `"blake3:"+...` concat (1 alloc) = 3 throwaway allocs, all copied into `out` then discarded.
- **Why it matters:** Drives ChunkRef JSON serialization for every FileAttr.Blocks write to Postgres/Badger, once per chunk per manifest write/read — 3 extra allocs × N chunks on hot metadata path for large files.
- **Fix:** Write directly into `out`: copy `"blake3:"` then `hex.Encode(out[tail:], h[:])` instead of calling CASKey(). 3 allocs → the 1 already-planned `out` alloc.
- **Verified:** Confirmed from source: types.go:99 prealloc, :101 `append(out, h.CASKey()...)`, CASKey (types.go:33-35) = `"blake3:"+hex.EncodeToString(h[:])`, 3 throwaway allocs/chunk. Reachable: badger encoding.go:84 `json.Marshal(blocks)` on fm: manifest write path. Severity MED→LOW: dwarfed by JSON encoder's own reflection/buffer work on same path — micro-nit not hot-path defect.

### [LOW] PunchHole re-sorts an already-sorted output — redundant O(n log n) every DEALLOCATE · `perf` · area: structures-and-legacy
- **Where:** `pkg/block/holemap.go:230`
- **What:** PunchHole filters refs (drop-only, no split/insert) then unconditionally calls `sortChunkRefsByOffset(out)` at :230. Input `refs` contractually sorted by Offset (holemap.go:9-10, `FileAttr.Blocks` documented sorted; sparse.go passes `file.Blocks` straight through). Drop-only filter preserves order → `out` already sorted when sort runs, every call, not just rare path. `TestPunchHole` (holemap_test.go:209) never exercises unsorted input — defensive branch unverified too.
- **Why it matters:** Unnecessary sort pass (closure/interface overhead) on top of the O(n) filter that already produced correctly-ordered output, on every DEALLOCATE. No reordering step to justify it, unlike neighboring MergeChunkRefsByOffset/PruneChunkRefsToSize pattern.
- **Fix:** Drop trailing `sortChunkRefsByOffset(out)` call — drop-only filter on sorted input preserves order. If defensive re-sort genuinely wanted, mark `ponytail:` naming the ceiling instead of paying cost silently.
- **Verified:** Confirmed: holemap.go:219-231, drop-only filter, order preserved under documented invariant. Reachable: sparse.go:84 `block.PunchHole(file.Blocks,...)` ← Service.PunchHole:48 ← nfs/v4/handlers/deallocate.go:67. Two corrections → severity MED→LOW: (a) NOT O(n log n) — sort.Slice's pdqsort has presorted detection, degenerates ~O(n) here; (b) DEALLOCATE also does a real zero-overwrite in engine/readwrite.go:277, sort cost unmeasurable next to it. Genuine dead work, safe to drop, not a perf-worthy finding at MED.

### [LOW] ContentHash legacy JSON-array decode has no length check — encoding/json silently zero-pads/truncates · `structure` · area: manifest-identity-types
- **Where:** `pkg/block/types.go:115`
- **What:** UnmarshalJSON legacy `[...]` branch: `json.Unmarshal(data, &arr)` into fixed `[HashSize]byte`. Short array → zero-filled tail, success. Long array → excess silently discarded, success. No length check (contrast canonical/base64 paths, both gate `len(...)==HashSize`).
- **Why it matters:** Exact anti-pattern package's own ErrFutureFormat rationale rejects ("decodes successfully into ... the wrong thing"). Blast radius MED not HIGH: FileChunk.Hash doesn't physically locate bytes (ChunkLocator/synced-hash lookups keyed separately, fail closed on genuine miss per locator.go IsStandalone contract) — observable failure is a loud lookup miss, not silent zeros served. Also: this dead-Badger-build compat branch (+ base64 branch below it) is the "legacy surface for a migration nobody runs" pattern flagged elsewhere — first choice is deletion, not a length fix.
- **Fix:** Given "no production stores in the field" decision, delete JSON-array + base64 legacy branches (111-122, 136-142) outright, keep only canonical `blake3:{hex}`/bare-hex forms. If fallback must stay: decode into `[]byte` first, reject `len(b) != HashSize` before copy, matching hex/base64 discipline already in same func.
- **Verified:** Same defect as idx 2 (types.go:115-125 no gate; siblings :127/:137 gated), confirmed identically. Mitigation verified: hash not used to physically locate bytes, so failure mode is a loud lookup miss. Stronger point holds: branch exists only for pre-v0.14.x badger rows, matches repo's "no production stores in field → delete legacy eagerly" convention. Dedup with idx 2; prefer deleting array+base64 branches over adding a length check.

## Implementation gaps vs reference

No dedicated `gaps` findings this pass — table below cross-refs the reference checklist against what this audit actually verified in `pkg/block` root (doc.go/errors.go/types.go/retention.go/locator.go). Rows marked *not audited* fall in sibling pkgs (engine/local/remote/chunker) — out of scope per this report's header, need their own pass.

| Expected (reference) | Our behavior | Gap | Source |
|---|---|---|---|
| Put idempotent on identical-hash content, no error on repeat | `ErrContentExists` sentinel defined in errors.go; doc.go states Put contract idempotent | Backend enforcement lives in engine/local/remote — not audited this pass | https://pkg.go.dev/github.com/containerd/containerd/content |
| Get on missing key returns one centralized not-found sentinel | `ErrContentNotFound`/`ErrChunkNotFound` defined at interface level | Sentinel exists; per-backend translation (badger/S3/etc, sibling pkgs) not audited this pass | https://pkg.go.dev/github.com/ipfs/boxo/blockstore |
| Every Store method takes `context.Context` first param | Not checked this pass — Store interface sig not diffed against convention | Unverified — add to next engine-scope pass | https://github.com/ipfs/go-ipfs-blockstore/blob/master/blockstore.go |
| Walk uses sentinel early-exit (`ErrStopWalk`), not bool/context-cancel | Confirmed: doc.go/errors.go document `ErrStopWalk` mirroring `filepath.SkipDir` | None — matches, but doc comments say "BlockStore.Walk" (stale type name, no such type — see LOW finding above, comments/store-contract-policy) | https://pkg.go.dev/io/fs#SkipAll |
| GetRange follows io.ReaderAt EOF/short-read semantics | Not checked this pass | Unverified — need GetRange impl read, out of scope (sibling pkg) | https://pkg.go.dev/io#ReaderAt |
| Residency ≥3 states (not local/remote bool) | Confirmed: `BlockState` Pending→Syncing→Remote in types.go, matches Lustre EXISTS/ARCHIVED/DIRTY shape | None at type level | https://github.com/LiXi-storage/lustre_manual_markdown/blob/master/26-Hierarchical%20Storage%20Management%20(HSM).md |
| "Synced" and "safe to reclaim" are separate predicates, not one bit | **Violated.** DEALLOCATE two-phase punch (`pkg/metadata/sparse.go:99` commits manifest prune before `blockStore.PunchHole` zeroes bytes) — phase-2 failure leaves manifest saying "reclaimed" with stale refcounts/bytes still standing, same class as #1850/#1888/#2084 | Reorder to physical-before-metadata, or repair manifest on phase-2 fail — see MED finding above (cross/bugs, sparse.go:99) | https://learn.microsoft.com/en-us/windows/win32/api/cfapi/nf-cfupdateplaceholder |
| Confirmed-lost distinct from not-yet-uploaded | Confirmed: `ErrChunkLostBeforeMirror` sentinel, doc comment says hash retained for retry | None found | DittoFS `pkg/block/errors.go:ErrChunkLostBeforeMirror` |
| Pin/retain is async intent, not a blocking guarantee | `RetentionPin` policy shape in retention.go — not exercised against a concurrent-eviction path this pass | Unverified — needs engine-scope eviction-path test | https://learn.microsoft.com/en-us/windows/win32/cfapi/build-a-cloud-file-sync-engine |
| Sentinels are package `var`s, compared via `errors.Is` | Confirmed: `ErrStopWalk`, `ErrFutureFormat` etc documented this way | Doc text itself stale — refers to nonexistent `BlockStore` type and a nonexistent `rollup.go` path (LOW findings, comments/store-contract-policy) | https://pkg.go.dev/errors#Is |
| Forward-incompatible format fails loud, never decodes silently | Confirmed: `ErrFutureFormat` doc states this explicitly, matches intent | ContentHash.UnmarshalJSON legacy array branch violates the same principle one type over: zero-pads/truncates instead of erroring (LOW finding above, types.go:115, bugs/structure/slop area: manifest-identity-types) | DittoFS `pkg/block/errors.go:ErrFutureFormat` |
| "Not yet indexed" has own sentinel distinct from "not found" | Confirmed: `ErrUnknownHash` documents the LRU-hit fallback contract | Doc comment points at nonexistent `pkg/block/local/fs/rollup.go` — real AddRef sites are `pkg/metadata/store/*/objects.go` (LOW finding above, errors.go:145) | DittoFS `pkg/block/errors.go:ErrUnknownHash` |
| Full 32-byte BLAKE3 digest as address, never truncated | Confirmed: `HashSize = 32` in types.go | None on ContentHash itself. Related gap one layer up: `ComputeObjectID`'s Merkle root hashes only `ChunkRef.Hash`, drops Size/StartOffset — two chunks sharing a Hash but different Size/StartOffset collide on ObjectID (MED finding above, objectid.go:36) | https://github.com/BLAKE3-team/BLAKE3-specs |
| Recomputed hash checked against stored hash on read, bad bytes never reach caller | Confirmed: `ErrChunkContentMismatch` doc states this | None found this pass | https://pkg.go.dev/github.com/ipfs/boxo/blockstore |
| BLAKE3 tree-chunking kept separate from storage-layer CDC chunking; chunk-size side channel (restic CVE-shape) considered | Not checked — `chunker/` is sibling pkg, out of scope | Unverified — flag for chunker-scope audit | https://github.com/restic/restic/blob/master/doc/design.rst |
| Chunk logical hash kept separate from physical (block, offset, length) | Confirmed: `ChunkLocator{BlockID, WireOffset, WireLength}` matches restic pack-index / JuiceFS chunk-slice-block split | None found | https://github.com/restic/restic/blob/master/doc/design.rst |
| Legacy locator format refused on live read path, fail-closed | `ChunkLocator.IsStandalone()` doc states this contract — not exercised against an actual legacy row this pass | Unverified — needs journal/migration-scope test | DittoFS `pkg/block` locator.go:IsStandalone |
| GC never deletes an object mid-write; fails closed on zero timestamp | doc.go states GC "fails closed on a zero timestamp" but GC code itself lives in `pkg/block/engine/gc*.go`, out of scope here. Also: doc.go's own Sub-packages list still claims a `gc` sub-package under pkg/block that doesn't exist (multiple LOW findings above, store-contract-policy) | Contract not verified against actual engine/gc*.go this pass; doc's own package map for where to find it is wrong | https://git-scm.com/docs/git-gc |
| Every backend stamps non-zero reliable write timestamp | Not checked — backend write paths are sibling pkgs (local/remote/badger etc), out of scope | Unverified — flag for backend-scope audit | DittoFS `pkg/block/doc.go` (internal invariant, no external source) |

Generated into the audit worktree; committed here as this file.
