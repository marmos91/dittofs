<!-- Data-plane audit of origin/develop @ 7eeec0da, scoped to the metadata <-> block store hot write/read path. -->

# Status annotations

The audit itself was run against the tree recorded below (`origin/develop @ 7eeec0da`) and has
not been rerun. What follows is a status layer over those findings: each one that has since been
actioned carries a `> STATUS:` line (`FIXED`, `TRACKED`, `PARTIAL`, or `REFUTED`). Those markers
were last re-verified on 2026-08-20 against develop `b4ff70af`. All 8 HIGH findings shipped; see
closed umbrella #1900.

| Finding | Issue | Outcome |
|---|---|---|
| sqlite `GetLinkCount` swallows errors | #1897 | FIXED #1901 (postgres too) |
| `Identity.gidSet` unsynchronized | #1895 | FIXED #1902 |
| Cold-fetch skips chunks after a hole | #1894 | FIXED #1903 |
| `WRITE_THROUGH` decoded but ignored | #1899 | FIXED #1904 (hardware-verified) |
| Per-READ synchronous atime write | #1892 | FIXED #1905 |
| `ProjectManifestToBlocks` O(N^2) | #1896 | FIXED #1906 |
| Netgroup unenforced on NFSv4 | #1898 | FIXED #1907 |
| Process-wide SMB write mutex | #1893 | FIXED #1908 |
| Truncate/PunchHole hydrate clamp (MED x2) | #1911 | FIXED #1933 |
| Carve/repack bypass CRC (MED x2) | #1912 | FIXED #1925 |
| Recovery panics on empty sealed segment | #1913 | FIXED #1921 |
| `evictSegment` busy-claim leak | #1914 | FIXED #1922 (+ torn-tail rollback the fix required) |
| badger durable-handle index key | #1915 | FIXED #1926 (+ postgres UNIQUE index drop) |
| `cas/` purge cross-share race | #1916 | FIXED #1928 |
| S3 SSRF guard config-time only | #1917 | FIXED #1927 |
| COMMIT authz / delete ceiling / anon ACL | #1918 | FIXED #1932 |
| Committed size past the durable extent | — | FIXED #1929 |
| Lock-manager MED batch (8 findings) | — | FIXED #1935 |

Every issue escalated out of this audit is closed. The unescalated MED tail was then
swept in five per-package batches:

| Batch | PR | Covers |
|---|---|---|
| runtime | #1943 | adapter restart/stop lifecycle, dead client-tracking surface, callback + path-derivation dedup |
| journal + local store | #1948 | reclaim/recovery correctness, fault-in seam on `FileSize`/`DataExtents`, group-commit, local-store cleanups |
| SQL stores | #1949 | `GetFileByPayloadID` manifest aggregation + scoped diff, snapshot/reset block-record state, transaction-convention and retry gaps |
| decorators | #1946 | shared `remote.Passthrough`, dead `BlockStoreAppend` contract, truthful rotation docs |
| block engine | #1959 | whole-block readahead, cold re-read failing closed, per-walk manifest snapshot, shutdown joins |

Also landed since: #1947 (memory-restore reseed via `quota.Delta`), #1950 (badger share-cache
purge on `Reset`/`RestoreSnapshot`), #1951 (master-key rotation, the feature #1946's docs said
did not exist), #1953 (overlapping manifest rows).

**Two findings are only half closed** — both marked `PARTIAL` inline:

- **badger caches on `Reset`/`RestoreSnapshot`** (#1961). #1950 flushes `shareCache`. `readCache`,
  `parentCache` and `direntCache` are still not flushed, so a restore can keep serving
  pre-restore file attrs and dirent hits. Same reachability as the original finding.
- **`/api/v1/clients` misreports NFSv4** (#1962). #1943 dropped the eight never-populated fields, but
  `NfsDetails.Version` is still hardcoded `"3"` at `pkg/adapter/nfs/connection.go:109` while the
  same connection dispatches both versions.

**The MED tail is now accounted for.** Of the 34 findings that carried no status:

- **2 are in flight** — the SMB `OpenFile` lock races, closed by #1957.
- **15 are structural debt, deliberately deferred** and marked `DEFERRED` inline. Seven are the
  pool-path-vs-transaction-path CRUD duplication, the three copy-pasted generation-guarded caches,
  and the duplicated config-reconciliation body across `badger`/`postgres`/`sqlite`; #1828 subsumes
  all of them. The other eight — `recover()` in `pkg/block/journal/recovery.go`, the lock-manager
  and lease god-functions, `runtime/shares/service.go`, and the triplicated ACL evaluation context
  — were tracked as #1967: #2015 (`recover()` → 54 lines over named phases), #2016 (the two lease
  god-functions → 120 and 86 lines), #2017 (`manager.go` split by the concerns its own section
  comments named), #2022 (`shares/service.go` 3300 → 581 lines) and #2014 (`walkDACL` as the single
  DACL walk, and two of the three `EvaluateContext` builders collapsed). #2014 deliberately left
  `buildAttrEvalContext` alone because #2020 was rewriting the same file; #2030 closes that residue
  and with it #1967. None had a behavioural defect attached; each was a refactor whose value is
  drift resistance, not a fix, and none of the six PRs changed behaviour.

  Two findings carried `DEFERRED to #1967` markers that #1967's own body never listed — the
  `pkg/metadata/service.go` god object and the share-wide `lm.mu` that serializes every lock op.
  Closing #1967 would have dropped them silently, so they moved to #2029 rather than riding along.
  #2017 declined the `Manager` *type* split for the same reason #2029 exists: re-homing that state
  is a lock-ownership redesign, not a file move.
- **17 are correctness or cost findings**, now batched into four PRs by area (metadata write path,
  lock-manager index use, per-op round-trips, adapters and dead state). The headline entries:

| Finding | Where | Dimension |
|---|---|---|
| Per-block sequential metadata round-trips on the Truncate/PunchHole/Delete reap loops | `pkg/block/engine/readwrite.go:178` | perf |
| Per-chunk `DeleteSynced`+`MarkSynced` loop issues 2N unbatched round-trips | `pkg/metadata/block_record_store.go:159` | perf |
| `checkFilePermissionsFile` does an uncached `GetShareOptions` round trip per check | `pkg/metadata/auth_permissions.go:267` | perf |
| GETXATTR scans the parent directory instead of resolving in O(1) | `pkg/metadata/xattr.go:121` | perf |

**The 220 LOW findings** were never triaged past the verify gate — unlike the HIGH and MED
entries, none carries a `Verified:` paragraph, so each is a claim rather than an established fact.
Any sweep must re-check them against the tree before acting: several MED findings above turned out
to have been closed already by unrelated work (#1906, #1932, #1939) and had gone unnoticed until
this refresh, and at least one MED premise was outright false. They break down as 64 `structure`,
60 `comments`, 42 `bloat`, 21 `perf`, 11 `gaps`, 10 `bugs`, 10 `slop`, 2 `security` — so roughly
two thirds are comment, dead-code and refactor entries, and the 44 `bugs`/`security`/`gaps`/`perf`
ones are where the value is. Those tranches have since been swept — #1996 (`comments` + `slop`),
#1997 (the Dockerfile `EXPOSE` drift split out of it), #1999 (`structure`, TRIVIAL grade), #2000
(`bloat`), #1971 (`bugs` + `security`), #1975 (`gaps`) and #1978 (`perf`) — and every LOW finding
now carries a marker. The verdict behind each marker, including the ones that turned out *not* to
be closed by the sweep that should have covered them, is in `low-tranche-verdicts.md` next to this
file. The structure debt the sweeps could not absorb is tracked as #1994, which is still open.

Also opened from this work: #1909 (crash-ordering silent zeros, still open),
#1910 (smbtorture grading non-determinism, fixed in #1919).

**Findings surfaced while fixing, not by the audit:**

- #1923 — `smb2.replay.channel-sequence` fails intermittently (byte-identical binary passes and fails). Surfaced while investigating a CI verdict flip; the audit did not look at test stability.
- The #1914 fix would have introduced a fresh silent-zeros bug: releasing the busy claim enables a retry that appends behind a torn cold-log tail, which `loadCold` then drops forever. Fixed in the same PR.
- #1910's original diagnosis (grading folds timeouts into pass/fail) was **wrong** — grading was correct; the real hole was that timed-out and never-reached tests vanished silently from the report. Recorded so the wrong theory is not re-derived.
- #1866 is **not a flaky test**. It was filed as "failed once in CI, not reproducible"; chasing it off a red PR run produced a deterministic repro on clean develop in 0.11s. `WaitForSnapshot` returns the orchestration error only while a live `doneCh` is in the registry, but `unregisterSnap` deletes that entry the moment the goroutine finishes — so when orchestration completes before the caller waits, the wrapped sentinel is dropped and a **failed** snapshot is returned with a **nil** error. The REST layer maps that sentinel to HTTP 500, so the wait endpoint reports success for a snapshot that failed, and the faster a snapshot fails the likelier it is reported fine. `cancelAndWaitInFlightSnaps` already works around the identical hazard in one place. A faithful fix needs the failure *kind* persisted on the row, which currently stores only the message string.
- #1936 — develop's E2E job has been red since 2026-07-28 and nothing tracked it. Two unrelated failures: `TestBlocksFlipLifecycle_NFS/_SMB` regressed at `1f95b389` (#1890, which intentionally redefined `disk_used` as the physical segment footprint, so the test's `require.Zero` is only reachable if GC also retires the emptied segment file), and `TestNLMAxisInterop`, older, where nlockmgr never registers in the test namespace's rpcbind. E2E is push-only on develop and gates no PR, which is why it survived 9+ days.
- #1930 (disabled-share purge gate), #1931 (`Get`/`Consume` naming different handles), #1934 (`resolveCovering` order-dependence) — all raised by fixes. #1934 shipped as #1953; the other two still await a decision rather than a patch.
- #1934's premise — that overlapping manifest rows are latent and cannot occur today — was **false**. A randomized mutation soak hit overlap in 3 of 24 seeds: `Truncate` narrows a straddling row, a later write re-carves from an earlier chunk boundary, and nothing reaps the narrowed row's tail. Erroring on overlap turned correct cold reads into hard failures, so #1953 ships greatest-start (matching badger) instead.
- #1956 — the same soak found a pre-existing bug the audit missed: after `PunchHole`, a cold read of the punched range does not read back as zeros (RFC 7862 DEALLOCATE contract). 5 of 24 seeds. Same silent-wrong-data family as #1879, #1888, #1894, #1909, #1911.

**On the review gates:** Copilot's findings ran 4 real / 2 refuted across this work, and the sub-agent reviewer missed every real one. The two real findings on #1933 shared a shape worth naming — *a sentinel value silently disabling a guard* (`DataSize == 0` skipping the clamp; a 32-bit `int()` wrap doing the same). Both would have failed open, which is the same failure direction as the silent-zeros family the audit exists to close. Copilot is not redundant with the reviewer sub-agent and must be checked immediately before merge, not once when the PR opens — it re-reviews each new commit.

**Severity calibration note:** the carve/repack CRC-bypass findings (#1912) were graded MED here but are arguably HIGH — they do not corrupt data, they promote already-corrupt data to trusted, defeating downstream checksums.


## What is still open before this audit can be called closed

Everything escalated out of the audit as its own issue is closed: all 8 HIGH (#1900), every
escalated MED, and the four that were only half closed at the last refresh (#1961, #1962, #1974,
plus #1909's device-loss verification). What remains is the debt the audit deliberately did *not*
escalate, and three things that surfaced while fixing.

| Finding | Tracking | State |
|---|---|---|
| God-functions and duplicated algorithms | #1967 | Closes when #2030 merges. Six PRs landed; #2030 is the last `EvaluateContext` copy, which #2014 could not touch while #2020 held the same file. |
| Two structure findings #1967's body never listed | #2029 | Open. `pkg/metadata/service.go` god object and the share-wide `lm.mu`. Split out so closing #1967 does not drop them. |
| LOW structure debt outside the store and lock packages | #1994 | Open, roughly 20 items. Four closed today (#2018 x2, #2019, #2020); `errors.go`/`lock_exports.go` reframed rather than fixed by #2025. |
| Store-family duplication (pool vs transaction CRUD, generation-guarded caches, config reconciliation) | #1828 | Open by design. Seven MED and several LOW findings are not fixable as isolated patches. |
| `PunchHole` cold read does not read back as zeros | #1956 | Open. Found by the mutation soak the audit fixes prompted, not by the audit. |
| Memory store durable-handle map guarded by two locks | #2023 | Open. Surfaced by #2018 while removing the *dead* shadow mutexes next to it. |
| `WaitForSnapshot` drops the orchestration error | #1866 | Open. Needs the failure kind persisted on the row, which currently stores only a message string. |

Both journal findings in #1994 that carried only a recommendation are now ruled. `segment.go:279`
is **WONTFIX**: `sh.lastVersion` is a scalar watermark that `groupCommit` and `DurableExtent` read
as a durability ceiling, and it is sound only while record completion order equals Version order —
which is what the lock buys. Releasing it acks a version as durable while its bytes are still in
flight, in either arrangement of the version stamp; the traced interleavings are in the finding.
`reclaim.go:131` is **WONTFIX, accepted latency**: the busy-claim asymmetry holds and is in fact
stronger than stated (the append path never consults the claim at all, so an active segment cannot
be excluded by one), and the exposure is four fsyncs on one shard at a time, once per
operator-triggered drain, on a path that never runs during normal writes — the same seal already
runs under the same lock on every segment rotation. Both now carry a `ponytail:` marker naming the
ceiling and the upgrade path, so the next reader does not re-propose the fix.

**One caveat on the LOW tranches.** They were swept, not verified. Unlike HIGH and MED, no LOW
finding ever carried a `Verified:` paragraph, so a sweep closing one is a claim about a claim.
`low-tranche-verdicts.md` records where that mattered: #1999's body described collapsing sqlite's
9-column client scanner, but its diff touched `sqlite/durable_handles.go` and `memory/clients.go`
instead, and both copies are still written out verbatim on develop. Marking that FIXED on the
strength of the PR description would have hidden it. Treat a swept LOW as swept, not as proven.

---

# Audit Report — DittoFS

Tree: `origin/develop` @ **7eeec0da** (detached worktree, untouched for the run)  
Scope: **data plane only** — the hot write/read path between metadata stores and block stores  
Raw findings judged: **565** across two runs · confirmed after adversarial verify: **330**  
**8 HIGH · 102 MED · 220 LOW**

All 8 lenses ran. 31 findings were reported independently by both runs and collapsed; those are marked *re-confirmed* — two separate verify agents reached the same conclusion, which is corroborating evidence.

Every finding passed three gates: confirmed from source, reachable from a non-test caller anywhere in the tree, and not spec-correct-per-RFC. Severity is blast radius on the data plane, not finder enthusiasm.

## Summary by dimension

| Dimension | HIGH | MED | LOW |
|---|---|---|---|
| bloat | 0 | 17 | 42 |
| bugs | 1 | 16 | 10 |
| comments | 0 | 7 | 60 |
| gaps | 3 | 12 | 11 |
| perf | 3 | 22 | 21 |
| security | 0 | 4 | 2 |
| slop | 0 | 3 | 10 |
| structure | 1 | 21 | 64 |
| **total** | **8** | **102** | **220** |

## Summary by area (top 20)

| Area | Findings |
|---|---|
| block-engine-syncer-drain | 13 |
| block-engine-readwrite-core | 10 |
| metadata-auth-permissions | 9 |
| metadata-store-memory | 9 |
| block-engine-read-fetch | 8 |
| block-journal-replay-gc | 8 |
| block-local | 8 |
| metadata-badger-write-txn | 8 |
| metadata-postgres-support | 8 |
| block-engine-carve-flip-gc | 8 |
| metadata-store-sqlite-write | 7 |
| block-journal-replay-recovery | 7 |
| metadata-store-badger-write-txn | 7 |
| metadata-store-sqlite-support | 7 |
| nfsv4-read-write-commit-readplus | 7 |
| runtime-core-blockstore-routing | 7 |
| adapter-smb-rw | 6 |
| metadata-core-contract | 6 |
| runtime-mounts | 6 |
| block-engine-readwrite | 6 |

---

## HIGH findings

### [HIGH] READ unconditionally does a synchronous metadata write (atime bump) on every read call

- **Where:** `internal/adapter/smb/handlers/read.go:440` · `perf` · area: adapter-smb-rw · *re-confirmed by both runs*
> STATUS: FIXED in #1905
- **Verified:** Confirmed: read.go:440-443 calls metaSvc.SetFileAttributes(Atime) on every successful READ unless IsAtimeFrozen, with no throttle. Service.SetFileAttributes (file_modify.go:217+) does GetFile + permission checks + a persisting store write; the dirTimes coalescing path applies only to directories (dirTimeSet gate at 236), so file reads take the full read-modify-write. One metadata write per READ RPC on the SMB data plane — classic atime amplification, and the codebase already debounces the mtime-on-write analogue.
- **Fix:** Debounce/batch atime updates the same way mtime writes are already debounced (e.g. only bump atime once per N seconds per open, mirroring smbDelayedWriteWindow), or defer via the existing FlushPendingWrite-style async path instead of a synchronous per-READ SetFileAttributes call.

### [HIGH] Every WRITE takes a process-wide mutex + durable-store lookup, serializing all writers

- **Where:** `internal/adapter/smb/handlers/write.go:376` · `perf` · area: adapter-smb-rw · *re-confirmed by both runs*
> STATUS: FIXED in #1908
- **Verified:** Confirmed: write.go:376-387 calls purgeConflictingDisconnectedHandlesForDataChange(..., breakToBelowHandle=true) on every WRITE when DurableStore != nil; the callee (disconnected_state_machine.go:306-311) passes the early-return guard, takes h.durablePurgeMu (handler.go:198, one process-wide sync.Mutex) and does GetDurableHandlesByFileHandle before releasing. DurableStore is set for the normal single-metadata-store deployment (adapter.go:871-896). Global serialization + a store round trip per WRITE is a server-wide data-plane throughput ceiling; handler.go's own comment claiming these paths are off the steady-state hot path is wrong.
- **Fix:** Fast-path skip the lock+lookup when there are no disconnected durable handles for the file (e.g., track a per-file or per-share disconnected-handle count/flag so the common case — no disconnected handles — avoids the lock and DB call entirely), or move the mutex to per-file-handle granularity.

### [HIGH] Cold-fetch loop skips chunks after a hole instead of the actual next chunk

- **Where:** `pkg/block/engine/fetch.go:372` · `gaps` · area: block-engine-read-fetch
> STATUS: FIXED in #1903
- **What:** EnsureAvailableAndRead's per-window fetch loop: when resolveCovering(cur) finds no chunk at cur (fb==nil), it jumps cur straight to the NEXT 8MiB block boundary (`cur = (cur/BlockSize + 1) * BlockSize`) instead of to the next actual chunk start. Comment justifies this with 'real files are fully written (no holes)' — but sparse holes ARE a supported, documented case elsewhere in this same package (read_internal.go:27-28 'A genuinely sparse hole ... stays zero-filled', range.go:13-15 'Sparse holes ... are NOT skipped'). BlockSize=8MiB (pkg/block/types.go:17), FastCDC avg chunk ~4MiB (read_internal.go:103-104), so a block routinely holds 2+ chunks. If a hole precedes a real chunk that starts and ends before the next block boundary, that whole chunk is skipped from the fetch list — never dispatched to dispatchRemoteFetch/hydrateChunk.
- **Why it matters:** Caller re-reads via local.ReadAt after this returns and trusts the window is now warm. The skipped chunk's bytes were never hydrated, so the post-fetch read either re-reports cold (silently swallowed — see next finding) or, worse, gets zero-filled as if it were a genuine hole, corrupting a read of a sparse file that has real data after a hole within the same 8MiB block.
- **Verified:** CONFIRMED at fetch.go:372-378. On fb==nil the loop does `cur = (cur/BlockSize + 1) * BlockSize` — jumps to the next 8 MiB boundary, not the next chunk start. resolveCovering (read_internal.go:154-183) returns (nil,0,nil) for any offset no row covers, so one uncovered byte discards every covering row up to the boundary. BlockSize=8MiB vs FastCDC ~4MiB avg (read_internal.go:103-104) ⇒ multiple chunks per block routinely. Reachable in production: readAtInternal (read_internal.go:34-54) sees cold=true → ensureAndReadFromLocal:81 → syncer.EnsureAvailableAndRead; also healCorruptWarmRead:69. Repro: payload with hole [0,1MiB) and evicted chunk [1MiB,3MiB); read(0,4MiB) → cur=0 nil → cur=8MiB → loop exits → toFetch empty → `return false, nil` (line 392) → caller re-reads, chunk still cold, zeros served as success. Not spec-correct: the package explicitly supports sparse holes (read_internal.go:27-28, range.go) and NFSv4.2 sparse is a shipped feature, so the in-code justification 'real files are fully written (no holes)' is contradicted by its own package. Same silent-zero class as #1879/#1888. Fix: one ListFileChunks snapshot for the payload (listFileChunksSnapshot already exists, warm.go:84) and iterate rows overlapping [offset,end) — or advance cur to the next row's absOffset instead of the block boundary.
- **Fix:** On a miss at cur, resolve the next actual chunk start (e.g. have resolveCovering/store return the next row's absOffset, or scan forward chunk-by-chunk) instead of assuming the rest of the current block is hole; only jump past block boundary when no chunk exists anywhere in [cur, nextBlockBoundary).

### [HIGH] Identity.gidSet lazy-init cache mutated without lock, but Identity is shared across concurrent goroutines via session-level cache

- **Where:** `pkg/metadata/auth_identity.go:214` · `structure` · area: metadata-auth-permissions · *re-confirmed by both runs*
> STATUS: FIXED in #1902
- **Verified:** CONFIRMED: HasGID (auth_identity.go:214-229) writes i.gidSet unguarded on first call; doc at :206-207 says NOT thread-safe / don't share across goroutines. Sharing is real: session.CachedAuthIdentity/SetCachedAuthIdentity (session/session.go:246-263) memoize ONE *metadata.Identity per session and handlers/auth_helper.go:230/240 hand that same pointer to every request. Concurrency on one session is real: parked-CREATE resume runs completeCreateAfterBreak in `go func()` (create_post_break.go:1574) concurrently with the connection loop, and auth_helper.go's own comment notes other goroutines reassign Session.User. Both goroutines reach HasGID via file_access_checker.go:147 / auth_permissions.go:372 / file_create.go:335 / file_modify.go:339,403. Concurrent map write (or read-during-write) = fatal runtime panic, not just a race-detector hit. Only real caller violates the type's own contract.
- **Fix:** Build gidSet eagerly when the Identity is constructed/memoized so HasGID is read-only, or guard the lazy init with sync.Once / a mutex on Identity.

### [HIGH] ProjectManifestToBlocks re-lists and re-diffs the entire file manifest on every block-object commit, not once per carve run

- **Where:** `pkg/metadata/block_record_store.go:174` · `perf` · area: metadata-core-contract · *re-confirmed by both runs*
> STATUS: FIXED in #1906
- **Verified:** CONFIRMED: DefaultCommitBlock (L137-176) ends with ProjectManifestToBlocks(ctx,tx,payloadIDFromChunks(fileChunks)) on EVERY call; that does tx.ListFileChunks(full per-file manifest) + ManifestToChunkRefs sort.Slice + PutFile{BlocksDirty:true}. REACHABLE: engine.CommitBlock (pkg/block/engine/blocksink.go:256) invokes it once per carved block object, serialized per-FileID by commitLocks — so a file carved across K block objects re-lists/re-sorts/re-writes the whole growing manifest K times, O(N^2/K). Contrast confirmed: sibling ReapSupersededManifest is explicitly run once at run end (L188-190). Material: for a multi-GB file this is thousands of full-manifest list+sort+File-row rewrites on the drain/write path. HIGH stands.
- **Fix:** Make the projection incremental — merge just-committed fileChunks into the already-sorted File.Blocks (insert at sorted position) instead of tx.ListFileChunks + full re-sort + BlocksDirty rewrite on every batch. Deferring wholesale to run end conflicts with the documented same-txn coherence invariant (block_record_store.go:36-39), so incremental is the correct shape.

### [HIGH] GetLinkCount swallows all query errors as "count=0", defeating callers' error-fallback safety net on the unlink/hardlink write path

- **Where:** `pkg/metadata/store/sqlite/files.go:183` · `bugs` · area: metadata-store-sqlite-write · *re-confirmed by both runs*
> STATUS: FIXED in #1901
- **Verified:** Confirmed verbatim at files.go:182-187 and transaction.go:764-769: `if err != nil { // Not found means count is 0; return 0, nil }` — any Scan error, not just sql.ErrNoRows. Callers confirmed: file_remove.go:171-175 has `linkCount, lcErr := tx.GetLinkCount(...); if lcErr != nil { linkCount = 1 }` — an explicit do-not-free-payload fallback rendered unreachable on sqlite, so a transient fault reads as nlink=0 and takes the last-link branch that marks content deletable; file_create.go:179 `linkCount, _ := tx.GetLinkCount(...)` then SetLinkCount(count+1) propagates a bogus count. sqlite is a flagship backend; swallowed error on a delete/hardlink write path = data-loss class. HIGH stands.
- **Fix:** In both files.go:183 and transaction.go:765, branch on errors.Is(err, sql.ErrNoRows) -> (0, nil) and return mapDBError(err, "GetLinkCount", "") for every other error, matching GetChild/GetParent/SetLinkCount.

### [HIGH] Netgroup export access check enforced only for NFSv3 MOUNT — SMB and NFSv4 bypass it entirely

- **Where:** `pkg/controlplane/runtime/netgroups.go:169` · `gaps` · area: runtime-mounts
> STATUS: FIXED in #1907
- **What:** Runtime.CheckNetgroupAccess is the sole export-level IP/netgroup allowlist gate in the codebase. Grepping all callers shows exactly one call site: internal/adapter/nfs/mount/handlers/mount.go:163 (NFSv3 MOUNT RPC). internal/adapter/smb/handlers/tree_connect.go has no reference to Netgroup/CheckNetgroupAccess at all, and internal/adapter/nfs/v4/handlers/helpers.go (buildV4AuthContext, the NFSv4 per-op auth/permission entry point) also never calls it. NFSv4 has no separate MOUNT protocol — client access to an exported filesystem happens via PUTFH/PUTROOTFH/LOOKUP, and this codebase never runs the netgroup check on that path either.
- **Why it matters:** A share configured with a restrictive NetgroupName (client allowlist by IP/CIDR/hostname) is fully enforced only for NFSv3 mounts. An SMB client, or an NFSv4.0/4.1 client, can reach the same share with zero netgroup enforcement — the CLAUDE.md invariant 'export-level squashing/access is applied during mount' silently only covers one of three supported protocol paths. This is a real authorization bypass, not a cosmetic gap: an admin who locks a share down to a netgroup for security reasons gets no actual protection against SMB or NFSv4 clients.
- **Verified:** Confirmed: CheckNetgroupAccess (runtime/netgroups.go:155) has exactly ONE non-test caller repo-wide — internal/adapter/nfs/mount/handlers/mount.go:163 (NFSv3 MOUNT). grep over internal/adapter/nfs/v4 and internal/adapter/smb for Netgroup returns nothing; v4 helpers.go buildV4AuthContext has no check. NFSv4 has no MOUNT protocol, so `mount -o vers=4.1` reaches the identical share via PUTROOTFH/LOOKUP with no IP allowlist evaluated. Docs (docs/guide/cli.md:3048-3053) describe netgroups generically as 'IP-based share access control ... restrict which network endpoints can access a share' with no v3-only caveat — an admin-facing security control silently unenforced on a supported protocol version. SMB half of claim is weaker (NetgroupID lives under nfsOpts, runtime/init.go:312, so SMB was never in scope), but the NFSv4 bypass alone is real. Fix: run CheckNetgroupAccess in the v4 per-compound auth path (helpers.go buildV4AuthContext / PUTFH share resolution).
- **Fix:** Call r.CheckNetgroupAccess (or an equivalent wrapper) from SMB TreeConnect (internal/adapter/smb/handlers/tree_connect.go) using the session's peer IP, and from NFSv4's buildV4AuthContext / first access to a share's root filehandle, mirroring the fail-closed pattern already used in mount.go (deny on parse error, deny on lookup error, deny on no match).

### [HIGH] WRITE_THROUGH/WRITE_UNBUFFERED request flag decoded but never honored

- **Where:** `internal/adapter/smb/handlers/write.go:97` · `gaps` · area: smb-read-write · *re-confirmed by both runs*
> STATUS: FIXED in #1904
- **What:** DecodeWriteRequest parses req.Flags (write.go:97) into WriteRequest.Flags, whose doc comment (write.go:47-52) names bit 0x1 write-through and 0x2 unbuffered. Write() never reads req.Flags again anywhere in the function body — it always runs the same PrepareWrite -> WriteToBlockStore -> CommitWrite -> deferred-flush path (write.go:412-451). WriteToBlockStore (internal/adapter/common/write_payload.go) is a bare passthrough to engine.WriteAt with no sync flag, and the metadata flush is explicitly deferred (write.go:442-448, 'Relaxed (#1687): ... we can defer the metadata db.Sync off the per-WRITE ack path'). handlePipeWrite (write.go:528) also never looks at req.Flags.
- **Why it matters:** Per MS-SMB2 3.3.5.13, SMB2_WRITEFLAG_WRITE_THROUGH tells the server the write must be durable at the underlying object store before the response is sent, and WRITE_THROUGH/WRITE_UNBUFFERED on a named-pipe write in the 3.x dialect family MUST be rejected with STATUS_INVALID_PARAMETER. Here a client that sets write-through (databases, Hyper-V-over-SMB, FILE_FLAG_WRITE_THROUGH opens) gets ack'd exactly like a normal cached write, with data/metadata durability deferred to CLOSE/FLUSH — silently dropping the durability guarantee the client explicitly requested and depends on for crash correctness. The named-pipe rejection case is also completely unimplemented.
- **Verified:** Confirmed: write.go:97 decodes req.Flags; grep for 'Flags' in write.go returns ONLY :47/:52 (doc) and :97 (decode) — never read in Write() or handlePipeWrite. Path is unconditional PrepareWrite→common.WriteToBlockStore→CommitWrite→FlushPendingWriteForFile(...,false) (write.go:412-451). Durability actually is dropped, verified down-stack: engine WriteAt (readwrite.go:89) → local.WriteAt → pkg/block/journal/store.go:358 'WriteAt buffers a dirty client write. It never fsyncs; durability is a Commit thing', and metadata sync is explicitly deferred (io.go:386 durable=false). So a client setting SMB2_WRITEFLAG_WRITE_THROUGH is ACKed with data only buffered — contradicts MS-SMB2 3.3.5.13 (write-through must reach persistent storage before the response). Named-pipe WRITE_THROUGH/UNBUFFERED → STATUS_INVALID_PARAMETER (3.x family) also unimplemented. Fix: on Flags&0x1, call the durable flush (FlushPendingWriteForFile durable=true) + journal Commit before responding; reject the pipe case in handlePipeWrite.
- **Fix:** In Write(), branch on req.Flags & SMB2_WRITEFLAG_WRITE_THROUGH (0x1, and WRITE_UNBUFFERED 0x2 for 3.x) and force a synchronous durability path for that request — flush the block store and call metaSvc.FlushPendingWriteForFile(..., forceSync=true) before returning success, instead of the deferred flush used on the default path. In handlePipeWrite, reject with STATUS_INVALID_PARAMETER when Flags has WRITE_THROUGH/WRITE_UNBUFFERED set.


---

## MED findings

### [MED] COMMIT handler has no authorization check — forces flush on any handle

- **Where:** `internal/adapter/nfs/v3/handlers/commit.go:115` · `security` · area: adapter-nfs-v3-rw-commit · *re-confirmed*
> STATUS: TRACKED as #1918
- **Verified:** Confirmed: Commit() (commit.go:115 GetFileCached with a bare context.Context, :171 ResolveForWrite, :194 CommitBlockStore) runs no permission gate; READ gates explicitly at read.go:164-197 and WRITE via PrepareWrite. Linux knfsd does fh_verify(..., NFSD_MAY_WRITE) for COMMIT, so this is a real deviation. Downgraded HIGH->MED: the info-disclosure half is refuted — GETATTR in this same package performs no permission check either (grep for Permission/GetCachedAuthContext in getattr.go: zero hits), which is POSIX-correct (stat needs no read bit), so the WCC attrs leak nothing GETATTR does not already give. Residual real effect is an unauthorized forced stable-storage flush on any traversable handle.
- **Fix:** Call h.GetCachedAuthContext(ctx) before GetFileCached and gate on the existing permission check (e.g. metaSvc.CheckReadPermissionFile / write gate) before ResolveForWrite + CommitBlockStore, denying with the mapped status like READ does.

### [MED] READ has no rtmax clamp — client-controlled Count drives a non-pooled allocation up to 1GB per request

- **Where:** `internal/adapter/nfs/v3/handlers/read.go:245` · `perf` · area: adapter-nfs-v3-rw-commit
> STATUS: FIXED in #1939 — READ clamps Count to caps.MaxReadSize
- **Verified:** Asymmetry confirmed: cachedMaxWriteSize is referenced only in write.go:166; read.go:245-251 computes actualLength = min(offset+Count, file.Size)-offset with no rtmax reference, validation caps Count at 1GB (read_validation.go:53-61), and common.ReadFromBlockStore does pool.Get(count) which falls to a bare make() above largeSize (bufpool.go:158-161), with Put refusing non-class capacities (bufpool.go:186-203). So an oversized-but-valid READ against a large file allocates up to ~1GB unpooled per RPC. Downgraded to MED: conforming clients honour the advertised rtmax, so this is a malicious/misbehaving-client memory-DoS hardening gap, not steady-state hot-path cost.
- **Fix:** Clamp actualLength to the cached/advertised MaxReadSize (mirror cachedMaxWriteSize()/setMaxWriteSize() from write.go, e.g. cachedMaxReadSize()) before calling common.ReadFromBlockStore, short-reading (RFC 1813 explicitly permits returning fewer bytes than requested) instead of allocating up to the full 1GB validation ceiling.

### [MED] buildReadPlusContents always computes block.Segments() even though it's discarded in the common (registry-configured) path

- **Where:** `internal/adapter/nfs/v4/handlers/read_plus.go:158` · `perf` · area: adapter-nfs-v4-rw-commit
> STATUS: FIXED in #1939 — block.Segments is computed only on the fallback path
- **Verified:** CONFIRMED: read_plus.go:158 `segs := block.Segments(file.Blocks, file.Size)` runs unconditionally, then :163 `segs = block.SegmentsExtents(ext, file.Size)` overwrites it whenever Registry resolves and DataExtents succeeds — the expected path. Discarded work is real: block.Segments -> normalizedExtents (holemap.go:46) allocates a [][2]uint64 of len(refs), runs sort.Slice (line 71), merges, then SegmentsExtents allocates a []Segment of len*2+1. That is an O(n log n) sort + two slice allocs per READ_PLUS, thrown away. For a large file (e.g. 1 GiB at 128 KiB chunks = ~8k refs) this is a millisecond-class waste on the data plane; the fix is a pure code-motion into the fallback branch.
- **Fix:** Compute segs lazily: try engine DataExtents first, fall back to block.Segments(file.Blocks, file.Size) only when Registry is nil or DataExtents errors.

### [MED] WRITE mutates OpenFile.PayloadID without lock, contradicting its own "immutable, safe without mutex" doc and racing unguarded readers

- **Where:** `internal/adapter/smb/handlers/write.go:493` · `bugs` · area: adapter-smb-rw
> STATUS: TRACKED in #1940 (PR #1957) — the OpenFile name triple is published as one atomic value
- **Verified:** Confirmed: write.go:493 `openFile.PayloadID = writeOp.PayloadID` with no lock; handler.go:401-402 lists PayloadID among 'immutable fields ... safe to access without the mutex' — factually wrong. Unguarded readers confirmed at close.go:212 (`openFile.PayloadID != ""` gating CommitBlockStore flush) and durable_context.go:1092 (persisted durable-handle snapshot). handler.go:392-395 itself documents that clients pipeline ops on one FileID (incl. multi-channel), and OpenFile lives in a sync.Map shared across connections, so concurrency is reachable. Downgraded HIGH->MED: window is the one-time ''->id transition on first write, and a CLOSE concurrent with an in-flight WRITE is already client-undefined; still a genuine data race (string header, -race-flaggable) that can drop the CLOSE flush.
- **Fix:** Guard the write.go:493 assignment (plus set_reparse_point.go:256 / ioctl_copychunk.go:555) and the read sites (close.go:212, durable_context.go:1092) with openFile.mu, or drop the cached field and re-fetch PayloadID from metadata at CLOSE/durable-snapshot time. At minimum fix the handler.go struct comment.

### [MED] WRITE reads OpenFile.FileName without lock while SET_INFO rename mutates it concurrently

- **Where:** `internal/adapter/smb/handlers/write.go:465` · `bugs` · area: adapter-smb-rw
> STATUS: TRACKED in #1940 (PR #1957)
- **Verified:** CONFIRMED: write.go:465 strings.Index(openFile.FileName, ":") with no lock; set_info.go:784 and :1184 assign openFile.FileName with no mu held (the only mu.Lock regions in that file are 401-481 and 632-637, unrelated). handler.go:401-402 lists the immutable set as FileID/TreeID/SessionID/Path/MetadataHandle/PayloadID/CreateOptions — FileName is not in it, and handler.go:393 states WRITE can be dispatched concurrently with other ops on the same handle (multi-channel). Unsynchronized string field = genuine data race (torn ptr/len read is memory-unsafe in Go), plus wrong ADS classification / notify stream name at 465-473 and 495-509. Keeping MED: a real race, but the reachable damage is limited to change-notify naming and ADS timestamp propagation.
- **Fix:** Read openFile.FileName under openFile.mu.RLock() in write.go and take openFile.mu.Lock() around the FileName/Path mutation in set_info.go:784 and :1184.

### [MED] WRITE issues two extra unbatched SetFileAttributes round trips (file atime + parent atime) per call

- **Where:** `internal/adapter/smb/handlers/write.go:481` · `perf` · area: adapter-smb-rw
> STATUS: FIXED in #1938 — the noteSmbAccess/noteSmbParentAccess per-handle coalescing window collapses the file and parent atime bumps, which is the debounce the finding asked for.
- **Verified:** CONFIRMED verbatim: after CommitWrite (line 434) and the deferred FlushPendingWriteForFile (449), write.go:482 does metaSvc.SetFileAttributes(file, Atime) and :485 does SetFileAttributes(openFile.ParentHandle, Atime), followed by restoreParentDirFrozenTimestamps(:489). Reachable: Handler.Write is the SMB2 WRITE entrypoint. Three+ synchronous metadata round trips per WRITE, and the parent-directory bump serializes every WRITE to any file in a directory on the same parent row — the same RMW-contention shape the codebase fixed elsewhere for mkdir nlink, and directly contradicting the deferral rationale in its own comment at 445-448.
- **Fix:** Fold file+parent atime into the same CommitWrite transaction, or defer/debounce it the way FlushPendingWriteForFile was relaxed, instead of two extra blocking store round trips per WRITE.

### [MED] Package doc + godoc advertise BlockStoreAppendConformance and appendlog.go that don't exist; FSStore never wired to any conformance suite

- **Where:** `pkg/block/blockstoretest/doc.go:8` · `structure` · area: block-conformance-suite · *re-confirmed*
> STATUS: FIXED in #1946 — block.BlockStoreAppend is deleted, along with the contract doc that claimed FSStore implemented it
- **Verified:** CONFIRMED on this tree: repo-wide grep for `func BlockStoreAppendConformance` returns nothing (only BlockStoreConformance conformance.go:65 and RemoteBlockStoreConformance remoteblock.go:50). Package dir has only conformance.go/remoteblock.go/doc.go — no appendlog.go. doc.go also cites pkg/block/local/fs/appendlog_internals_test.go which does not exist (fs dir has only legacy*/disk_used/format tests). blockstoretest.* callers are compression, encryption, s3, memory — fs is NOT among them, contradicting doc.go:8-9 ("The fs, s3, and memory backends all call this entrypoint") and conformance.go:63-64. Real coverage hole hidden behind confident godoc. MED not HIGH: no runtime defect, docs + missing test wiring.
- **Fix:** Either implement BlockStoreAppendConformance (and appendlog.go) and wire pkg/block/local/fs/*_test.go to call blockstoretest.BlockStoreConformance + BlockStoreAppendConformance, or strip the false claims from doc.go/conformance.go godoc until they exist.

### [MED] EncryptedRemote and compression.Decorator are ~90% duplicate boilerplate — no shared base

- **Where:** `pkg/block/encryption/decorator.go:283` · `structure` · area: block-crypto-compression
> STATUS: FIXED in #1946 — shared remote.Passthrough
- **Verified:** Confirmed by side-by-side read: encryption/decorator.go:283-357 vs compression/decorator.go:315-389 are line-for-line identical modulo the receiver type and comment wording (blockInner/casInner/PutBlock/GetBlock/GetBlockRange/DeleteBlock/WalkBlocks). legacy_cas_migration.go: a type-normalized diff of the two 53/54-line files yields only comment-text differences — structurally byte-identical. Both packages are live (block.Store/remote.RemoteStore assertions). ~300 duplicated lines across two live decorators is real drift surface.
- **Fix:** Extract the passthrough scaffolding into pkg/block/remote (e.g. remote/passthrough.go: type Passthrough struct{ inner RemoteStore } with casInner/blockInner/Close/HealthCheck/Healthcheck/Durable/PutBlock/GetBlock/GetBlockRange/DeleteBlock/WalkBlocks/Has/GetRange/Walk taking a decode func). EncryptedRemote and compression.Decorator embed it and supply only Put/Get/SealChunk/ReadChunk plus the encode/decode hook. Same for the LegacyCASStore forwards.

### [MED] ~150 lines of identical passthrough plumbing duplicated verbatim between the two decorators

- **Where:** `pkg/block/compression/decorator.go:325` · `bloat` · area: block-encryption-compression
> STATUS: FIXED in #1946 — shared remote.Passthrough
- **Verified:** CONFIRMED. pkg/block/compression/decorator.go:325-389 (blockInner/casInner/PutBlock/GetBlock/GetBlockRange/DeleteBlock/WalkBlocks/HealthCheck/Healthcheck/Durable) vs pkg/block/encryption/decorator.go:293-357 — bodies byte-identical modulo receiver name; GetRange/Has/Delete same. Even the '--- remote.RemoteBlockStore passthrough' banner comment is duplicated near-verbatim. REACHABLE: both constructed in prod at pkg/controlplane/runtime/shares/service.go:1564 (encryption.NewRemote) and :1592 (compression.NewRemote). Fix: one embedded `passthrough{inner remote.RemoteStore}` struct in a shared pkg, embed in both decorators; each keeps only Get/Head/SealChunk/ReadChunk/Close.
- **Fix:** Extract a small embeddable helper (e.g. `remote.BlockPassthrough{ inner remote.RemoteStore }`) implementing blockInner/casInner/PutBlock/GetBlock/GetBlockRange/DeleteBlock/WalkBlocks/HealthCheck/Healthcheck/Durable once; embed it in both Decorator and EncryptedRemote. Cuts ~150 duplicated lines to one shared implementation.

### [MED] Master-key rotation is documented/promised but not implemented — old blocks become permanently unreadable after rotation

- **Where:** `pkg/block/encryption/keyprovider/provider.go:26` · `gaps` · area: block-encryption-compression
> STATUS: PARTIAL — #1946 made the docs truthful by removing the rotation procedure the provider could not honour. The retired-key set that makes rotation actually work is #1951, still open
- **Verified:** CONFIRMED. Both providers embed a single aesGCMKEK (local.go:68-98, kmip.go:37-67) holding one masterKey/masterKeyID (local.go:227-231); Unwrap (local.go:253-256) returns ErrWrongMasterKey whenever the frame's id != the single held id — no keyring of retired keys anywhere in the package. Interface doc (provider.go:24-29) promises routing 'to the right master key after a future rotation' and kmip.go:29-31 documents rotation as 'write a new key to the HSM and restart the daemon'; doc.go:1-6 claims parity with SSE-KMS/KES/Vault Transit which keep old versions decryptable. Following the documented procedure orphans every previously wrapped block. Reachable: keyprovider.NewProvider + encryption.NewRemote at shares/service.go:1560-1564 (non-test). Fix: either accept a list of retired keys (id→key map) in Config and route Unwrap by id, or delete the rotation promise from the docs until implemented.
- **Fix:** Either (a) make Config/KeyProvider support a list of master keys (current + N retired), so Unwrap can look up by masterKeyID across the set while Wrap always uses CurrentMasterKeyID — mirrors how Vault Transit/SSE-KMS keep old key versions live for decrypt-only; or (b) if only one key is ever supported, remove the 'route to the right master key after a future rotation' claim from provider.go and the 'operators rotate...and restart' claim from kmip.go, and document that rotation requires a full re-encrypt (rewrap every block) before decommissioning the old key, e.g. via a rewrap tool that reads with the old provider and writes with the new one.

### [MED] walkAuditShareFiles: N+1 GetFile per directory entry, ignoring already-populated DirEntry.Attr

- **Where:** `pkg/block/engine/audit_state.go:218` · `perf` · area: block-engine-carve-gc
> STATUS: REFUTED in #1959 — the per-entry GetFile is load-bearing (the READDIRPLUS projection leaves Blocks/PayloadID unloaded on the relational backends); documented in place
- **Verified:** CONFIRMED: walkAuditShareFiles' inner loop unconditionally does `child, err := store.GetFile(ctx, e.Handle)` for every entry returned by ListChildren, then switches on child.Type. metadata.DirEntry.Attr (validation.go:39-42) exists precisely so callers 'can avoid per-entry GetFile() calls', and it is populated on the store-level path: sqlite files.go:356 sets `Attr: attr`, and badger's store-level ListChildren (files.go:395-409) delegates straight to badgerTransaction.ListChildren which fills Attr — so the fix pays off on the default backend too. REACHABLE from non-test entrypoints: dfsctl store block audit (cmd/dfsctl/commands/store/block/audit.go:44) → API handler blockstore_audit.go:72 → Runtime.AuditRefcounts. Downgraded HIGH→MED: this is an operator-triggered offline audit walk, not the data plane — cost is a slower audit run, not client-visible latency.
- **Fix:** Use e.Attr when non-nil (recurse on e.Attr.Type; build the *metadata.File from e.Handle+e.Attr for regular files) and fall back to GetFile(ctx, e.Handle) only when e.Attr is nil.

### [MED] cas/ legacy-namespace purge races across shares sharing a remote store, deleting standalone chunks another share hasn't migrated yet

- **Where:** `pkg/block/engine/legacy_migration.go:135` · `bugs` · area: block-engine-carve-gc
> STATUS: TRACKED as #1916
- **Verified:** Confirmed: Phase P (legacy_migration.go:141-159) walks and deletes the ENTIRE cas/ namespace (s3 legacy_cas_migration.go casPrefix="cas/" under s.fullKey, i.e. bucket+configured prefix, no per-share scoping), runs per share from engine.go:232 in an unsynchronized background goroutine, and repackStandaloneChunks treats ErrChunkNotFound as pre-existing loss (drops the synced marker, lines 232-245). No lock analogous to Runtime.remoteGCLock (blockgc.go:637-644) exists on this path — grep shows migrateLegacyCAS has no serialization at all. Note the defect is broader than a race: even fully serialized, share A's blanket purge destroys share B's still-standalone chunks, so the scoping fix is the correct one. MED not HIGH: requires >=2 shares on one remote config with pre-#1493 standalone data, on a one-time upgrade path.
- **Fix:** Serialize Phase R+P per remote identity the same way gc.go/Runtime do: acquire a process-wide lock keyed by the remote's stable identity (e.g. configID, or the same key Runtime's remoteGCLock uses) before running migrateLegacyCASRemote, and hold it across both Phase R and Phase P. Alternatively, scope Phase P's purge to only the hash keys this share actually repacked (or verified dead) instead of blanket-deleting the whole cas/ namespace, so one share's purge can never race another share's still-in-flight read.

### [MED] WarmAll claims to skip already-local blocks and report them but never does — BlocksAlreadyLocal is dead, every warm re-fetches everything

- **Where:** `pkg/block/engine/warm.go:88` · `gaps` · area: block-engine-cold-eviction-seed
> STATUS: FIXED in #1959 — BlocksAlreadyLocal dropped; the skip it advertised cannot be implemented over an offset-keyed local tier
- **Verified:** CONFIRMED at warm.go:76-108. `alreadyLocal` declared line 78, never incremented (only read at 107/170/177) ⇒ WarmResult.BlocksAlreadyLocal is always 0 despite its godoc at :16 and the API json tag `blocks_already_local`. Loop 88-99 appends every parseable row unconditionally; the ponytail comment at 95-98 concedes there is no local-presence probe, directly contradicting the function godoc at :36-37 ('skips any block already present locally') and :45-46 ('total is the number of NOT-already-local blocks'). fetchResolvedBlock (fetch.go:288-330) has no local short-circuit — it always dispatchRemoteFetch then hydrateChunk, so every warm run does pay full remote GETs. Reachable in production: shares/service.go:2804 → engine.Store.WarmAll (engine.go:541) → Syncer.WarmAll. Downgraded to MED: no correctness/data loss, and the ponytail comment shows the re-fetch is a known accepted cost (journal is offset-keyed, not hash-keyed) — the defect is the godoc + the always-zero field in an operator-visible JSON result. Fix: either drop BlocksAlreadyLocal and correct the two godoc paragraphs, or probe residency via local.DataExtents(payloadID) once per payload and skip rows fully covered.
- **Fix:** Either add a cheap local-presence check (e.g. probe bs.local.DataExtents/FileSize coverage for the chunk's offset range before adding it to targets) and increment alreadyLocal on skip, or fix the docs/API to stop promising skip-already-local behavior and stop reporting a field that's structurally always zero.

### [MED] Readahead worker fetch only resolves the first chunk of a block, silently drops trailing chunks

- **Where:** `pkg/block/engine/fetch.go:57` · `gaps` · area: block-engine-read-fetch
> STATUS: FIXED in #1959
- **Verified:** CONFIRMED. fetch.go:56-59 resolveFileChunk does ONE resolveCovering probe at blockIdx*BlockSize (BlockSize=8MiB, pkg/block/types.go:17); fetchBlock (fetch.go:262) uses it and nothing else. Contrast EnsureAvailableAndRead (fetch.go:366-388) which loops resolveCovering advancing by fb.DataSize, with an in-repo comment stating 'FastCDC chunks are typically smaller than BlockSize, so a block holds several chunks' and that the old single-probe loop left everything past a block's first chunk unfetched. Reachable non-test: readwrite.go:32 Store.ReadAt -> scheduleReadahead -> EnqueuePrefetch -> sync_queue.go:300 q.manager.fetchBlock. Effect is perf only (demand path still correct), so MED not HIGH. Fix: give fetchBlock the same covering-chunk walk over [blockIdx*BlockSize,(blockIdx+1)*BlockSize) and fetch each row.
- **Fix:** Either resolve and enqueue all chunks covering the block's byte range in resolveFileChunk/fetchBlock, or document explicitly that prefetch is best-effort single-chunk-per-block and rely on demand fetch for the rest.

### [MED] Post-hydrate re-read discards its own cold flag

- **Where:** `pkg/block/engine/read_internal.go:84` · `gaps` · area: block-engine-read-fetch
> STATUS: FIXED in #1959
- **Verified:** CONFIRMED at read_internal.go:84: `if _, _, err := bs.local.ReadAt(ctx, payloadID, int64(offset), dest); err != nil` — n and cold both discarded. readAtInternal:51-54 then returns len(data), nil unconditionally; healCorruptWarmRead:69-72 same. Production path (NFS/SMB read → Store.ReadAt). Real given idx 6 makes leftover-cold actually happen, and independently under evict-racing-hydrate. Silent zeros instead of EIO — same failure mode #1850/#1879 were opened for. Not a spec question. Fix: `n, cold, err := ...; if cold { return fmt.Errorf("window still cold after hydrate for %s at %d: %w", payloadID, offset, block.ErrChunkNotFound) }`.
- **Fix:** Check the returned cold flag from the post-hydrate ReadAt; if still cold, treat as a hydrate failure (error out) rather than returning success.

### [MED] N+1 full-manifest scan per covering-chunk in cold-read fetch loop (non-indexed backends)

- **Where:** `pkg/block/engine/fetch.go:367` · `perf` · area: block-engine-readwrite
> STATUS: FIXED in #1959
- **Verified:** CONFIRMED: EnsureAvailableAndRead's window loop (fetch.go:366-388) calls resolveCovering once per covering chunk (`for cur := offset; cur < end;` … `cur = next` advancing by fb.DataSize). resolveCovering (read_internal.go:154-184) takes the indexed GetFileChunkAtOffset path only when the store implements chunkAtOffsetResolver, else falls through to `store.ListFileChunks(ctx, payloadID)` + findRowCoveringOffset — a full per-payload manifest fetch and O(N) scan per offset. So K covering chunks ⇒ K full manifest fetches, O(K·N). REACHABLE: sqlite/postgres/memory are selectable metadata backends and this is the demand-fetch cold-read path. Downgraded HIGH→MED: the function's own doc already flags the fallback as 'not the profiled hot path', badger (the default) has the index, and a cold read is dominated by the remote object fetch that follows — but the fix is trivial and the O(K·N) is real.
- **Fix:** In EnsureAvailableAndRead (and resolveFileChunk's blockIdx path), when the store does not implement chunkAtOffsetResolver, call listFileChunksSnapshot(ctx, payloadID) ONCE for the whole window, then resolve every covering chunk in-memory via findRowCoveringOffset against that single snapshot (mirroring DataExtents), instead of letting resolveCovering re-issue ListFileChunks per offset.

### [MED] Truncate keeps a ChunkRef whose byte range extends past newSize; cold-read hydrate can resurrect truncated-away bytes / grow local storage past the new size

- **Where:** `pkg/block/engine/readwrite.go:151` · `bugs` · area: block-engine-readwrite
> STATUS: FIXED in #1933 (#1911) — hydrate now writes only the bytes the row still claims, taking the second option below (narrow the row) rather than dropping it. Copilot found two fail-open holes in the first cut: the clamp was skipped when `DataSize == 0`, which is unreachable via the demand cold read but reachable via `WarmAll` (it walks enumerated rows with no size filter), and `int(fb.DataSize)` could wrap on a 32-bit platform and skip the clamp the same way. Both would have re-opened exactly this finding through a zero rather than a wrong length.
- **Verified:** Confirmed: drop predicate is only b.Offset >= newSize (readwrite.go:151-160); straddling tail row keeps original Hash/Size. journal Store.Truncate (store.go:708-730) only CLIPS the local interval to newSize and does NOT re-mark it dirty, so nothing re-carves the straddling manifest row — unlike the PunchHole case, no dirty bytes are involved, so the tail chunk's segment is already fully synced and immediately evictable (reclaim.go:190). A cold read at any offset inside that chunk resolves the un-narrowed row and hydrateChunk (fetch.go:83) writes the FULL fb.DataSize at the chunk's absolute offset with no size clamp; the hydrate record carries a higher version than the truncate marker (store.go:683-685 explicitly does not fence later versions), so bytes past newSize come back into the local index. Re-extending the file then serves those stale bytes where a zero hole is required. Downgraded to MED: needs eviction + cold read + re-extend, and metadata Size still clamps ordinary reads.
- **Fix:** For the kept partial-tail block, don't keep the ChunkRef verbatim: either drop/supersede its FileChunk manifest row too (same reap path as the fully-past blocks) so a subsequent read of that offset falls through to a hole/re-carve, or truncate the returned ChunkRef's Size to newSize-b.Offset and persist a correspondingly truncated manifest row before returning.

### [MED] Per-block sequential metadata round-trips on Truncate/PunchHole/Delete reap loops

- **Where:** `pkg/block/engine/readwrite.go:178` · `perf` · area: block-engine-readwrite
- **Verified:** CONFIRMED in all three sites: Truncate reaps `for _, b := range dropped { ... bs.coordinator.DecrementRefCountAndReap(ctx, payloadID, b.Offset) }` (readwrite.go ~:178-197), PunchHole the same (~:273-291), Delete the same over `blocks` (:392-419, verified verbatim). One coordinator call per dropped chunk, strictly sequential. Reachable from prod unlink (Delete) and SETATTR-size (Truncate) / DEALLOCATE (PunchHole). Materially significant, not micro-opt: at the recommended 128 KiB FastCDC chunk a 4 GiB file is ~32k rows, i.e. ~32k sequential metadata round-trips on a postgres backend before the client op returns. Each site also does a ReprojectBlocks pass after. MED stands.
- **Fix:** Add a batched coordinator entrypoint (DecrementRefCountAndReapMany(ctx, payloadID, offsets []uint64)) implemented as one multi-row DELETE/UPDATE per backend, and call it once per Truncate/PunchHole/Delete instead of per dropped block; keep exact payloadID/offset semantics.

### [MED] blocksReprojector: one-impl capability interface probed twice, only to dodge test fakes

- **Where:** `pkg/block/engine/readwrite.go:126` · `structure` · area: block-engine-readwrite · *re-confirmed*
> STATUS: FIXED in #1959 — ReprojectBlocks is now a MetadataCoordinator method
- **Verified:** CONFIRMED: interface declared readwrite.go:126-128, identical `rp, ok := bs.coordinator.(blocksReprojector)` guards at :193 (Truncate) and :287 (PunchHole). Grep shows exactly ONE implementation repo-wide: shares/coordinator.go:168 (*metadataCoordinator). Doc comment itself states the only non-implementers are unit-test fakes. Violates repo's explicit convention (no one-impl capability interfaces), and the ok=false branch silently skips a correctness-relevant Blocks re-projection — a future second coordinator that forgets the method gets over-counted snapshots with no compile error.
- **Fix:** Add ReprojectBlocks to MetadataCoordinator and call bs.coordinator.ReprojectBlocks unconditionally in Truncate and PunchHole; test fakes implement a no-op.

### [MED] PersistFileChunks doc describes a rollup-completion callback that engine.go says no longer exists

- **Where:** `pkg/block/engine/coordinator.go:67` · `comments` · area: block-engine-readwrite-core
> STATUS: FIXED in #1959 (doc)
- **Verified:** Confirmed: coordinator.go:67-71 says 'Engine invokes this from the local store's rollup-completion callback (the ObjectIDPersister wired in engine.New)'. engine.go:193-197 says the opposite ('Chunk-lifecycle hooks are gone with the journal switchover ... There is no rollup-completion persister, no write-side cache warm hook, and no per-chunk emitter') and New (engine.go:163-205) wires no callback. grep: ObjectIDPersister has zero declarations tree-wide, comment-only (engine.go:100, readwrite.go:504/553/566/572, fetch.go:43, 2 tests). Engine also never calls PersistFileChunks outside coordinator_test.go:245 — ErrPersistFileChunksNotWired exists for that. Reachable: MetadataCoordinator impl is production (pkg/controlplane/runtime/shares/coordinator.go:195, injected per shares/service.go). Fix: replace the callback sentence with 'invoked by the runtime coordinator wrapper' or drop it; MED not HIGH — comment rot, no runtime effect.
- **Fix:** Update coordinator.go's PersistFileChunks doc to say who actually calls it now (per engine.go: the carve BlockSink's commit txn), drop the ObjectIDPersister/engine.New claim.

### [MED] Background goroutines (carveDispatcher, runUploadController, HealthMonitor.monitorLoop) are not joined on shutdown

- **Where:** `pkg/block/engine/syncer.go:710` · `structure` · area: block-engine-syncer-drain
> STATUS: FIXED in #1959 — HealthMonitor.Stop and Syncer.Close now join under a bounded wait
- **Verified:** CONFIRMED: `go m.carveDispatcher(ctx)` (:710) and `go m.runUploadController(ctx, ...)` (:717) launched with no WaitGroup; Syncer.Close() (:915-943) closes stopCh, calls healthMonitor.Stop(), DrainAllUploads, queue.Stop(timeout) and returns without joining either. HealthMonitor.Stop() (sync_health.go:97-101) closes stopCh under stopOnce, never joins monitorLoop. Contrast SyncQueue.Stop which does join. Materially reachable: engine.go:376-386 Close() calls syncer.Close() then immediately bs.local.Close() / bs.remote.Close(), while a carvePass in flight is still calling m.local.ListFiles/Carve and monitorLoop may still be running probeFunc against the remote — use-after-close/goroutine-leak race. Partially mitigated (carvePass derives a stopCh-cancelled ctx) but the join is genuinely absent.
- **Fix:** Add a sync.WaitGroup to Syncer and HealthMonitor covering carveDispatcher/runUploadController/monitorLoop; Add(1) before each `go`, Done() on return, Wait() with SyncQueue.Stop's timeout pattern inside Close()/Stop().

### [MED] Truncate: misleading summary line contradicts documented no-op behavior

- **Where:** `pkg/block/engine/syncer.go:584` · `comments` · area: block-engine-syncer-drain
> STATUS: FIXED in #1959 (doc)
- **Verified:** Confirmed at syncer.go:584-593. Godoc summary line 584 'Truncate removes blocks beyond the new size from the remote store' contradicts the body (595-609: checkReady, nil-remote skip, health-gate warn, then bare 'return nil'). Line 590 is genuinely truncated mid-sentence: '…becomes a no-op at the remote-side after' followed by 'kept as a stable seam for callers'. Reachable: engine.Truncate invokes m.syncer.Truncate unconditionally on the production truncate path. Fix: summary → 'Truncate is a no-op on the remote side; CAS objects are reclaimed by GC.' and repair the dangling clause. MED — first line of godoc states the opposite of the behaviour.
- **Fix:** Reword summary to e.g. "Truncate is a no-op at the remote layer; per-file cleanup is handled by RefCount decrement + GC." and fix the truncated sentence at line 590.

### [MED] Garbled/incomplete comment on fileChunkStore field

- **Where:** `pkg/block/engine/syncer.go:42` · `comments` · area: block-engine-syncer-drain
> STATUS: FIXED in #1959 (doc)
- **Verified:** CONFIRMED syncer.go:42-46. Two defects, not one: (a) field comment opens 'the syncer is one of the engine-internal callers' with no 'fileChunkStore is...' lead; (b) sentence 'surface (GetFileChunk for dual-read resolve, ListFileChunks for GetFileSize/Exists). routes reads through FileAttr.Blocks and lets us drop the wider interface.' has no subject and is unparseable. Field type IS still block.EngineFileChunkStore (line 47), so 'lets us drop the wider interface' describes a state that never arrived — actively misleading. Production: NewSyncer syncer.go:168. Fix: rewrite as one sentence naming fileChunkStore + why the wide surface is still needed.
- **Fix:** Rewrite as a complete sentence, e.g. "Other engine internals route reads through FileAttr.Blocks and no longer need the wider interface; the syncer is one of the few callers left that still reaches into it."

### [MED] Delete: summary and body claim don't match the actual no-op implementation

- **Where:** `pkg/block/engine/syncer.go:612` · `comments` · area: block-engine-syncer-drain
> STATUS: FIXED in #1959 (doc)
- **Verified:** CONFIRMED syncer.go:612-634. Doc says 'Delete removes all blocks for a file from the remote store' and 'Delete now records the deletion intent'. Body: checkReady → nil-remote early return → IsRemoteHealthy warn return → 'return nil' (line 633). Nothing removed, nothing recorded — pure no-op. Real deletion is refcount decrement in engine.Delete. Reachable: readwrite.go:423 'delErr := bs.syncer.Delete(ctx, payloadID)'. Fix: retitle 'Delete is a no-op on the remote side; engine.Delete drives refcount+GC' or delete the method and its call site.
- **Fix:** Reword to state this Syncer.Delete is a no-op retained as a stable seam; the refcount/GC-driven deletion happens in engine.Delete, not here.

### [MED] Verified warm read allocates a fresh whole-record buffer per read, no pooling

- **Where:** `pkg/block/journal/record.go:182` · `perf` · area: block-journal-core
> STATUS: FIXED in #1948 — the record read takes a caller-supplied buffer
- **Verified:** CONFIRMED: record.go:182 `buf := make([]byte, body)` where body = FileIDLen + full PayloadLen + CRC; index.go:341-345 calls verifiedRead per warm piece of every ReadAt when verifyReads is set, and verifiedRead (363-386) calls readRecordAt(seg.fd, p.recOff, SegmentSize) then copies only the sub-range. REACHABLE and ON by default for durable shares: shares/service.go:1170-1171 SetVerifyReads(!writeback). Cost is per-read whole-record alloc + read + CRC on the data-plane read path — more than a micro-opt (the alloc is sized to the record, not the request).
- **Fix:** Pool the readRecordAt scratch buffer (sync.Pool sized to SegmentSize/record ceiling, or per-shard reusable buffer under an appropriate lock) instead of make()-ing a fresh slice every call; reset before reuse per the pooling convention already used elsewhere in the block-store hot path.

### [MED] Delete/Truncate fsync bypasses shard group-commit coalescing

- **Where:** `pkg/block/journal/segment.go:424` · `perf` · area: block-journal-core · *re-confirmed*
> STATUS: FIXED in #1948 — tombstone durability goes through the shard commit leader
- **Verified:** CONFIRMED: appendTombstone releases sh.mu then does `if err := fd.Sync(); err != nil { … 'journal: fsync tombstone' }`, and appendTruncateMarker does the identical raw `fd.Sync()` ('journal: fsync truncate marker'). Neither calls sh.groupCommit(); grep shows groupCommit is invoked only from Store.Commit (store.go:451). groupCommit (store.go:464-510) is a general per-shard leader/follower fsync coalescer that re-reads sh.active fresh and is safe for these callers (its own comment covers the rotation case). So a burst of concurrent Delete/Truncate hashing to one shard pays one full disk barrier each, exactly what #1736 built groupCommit to eliminate. Kept MED rather than HIGH: bulk unlink also pays a metadata-store durable txn per op, so removing this halves rather than transforms the cost, and delete/truncate is not the profiled data-plane hot path.
- **Fix:** Route appendTombstone/appendTruncateMarker's durability step through sh.groupCommit() (store.go:464) instead of calling fd.Sync() directly, so concurrent deletes/truncates on one shard piggyback on a single fsync the way concurrent Commit callers already do.

### [MED] evictSegment leaks a segment's busy claim forever when appendCold fails, permanently blocking reclamation

- **Where:** `pkg/block/journal/reclaim.go:207` · `bugs` · area: block-journal-replay-gc
> STATUS: FIXED in #1914 / PR #1922
- **Verified:** Confirmed: evictSegment's appendCold branch returns `0, err` with no busy reset, and evict() (reclaim.go:94-97) just propagates. Sibling paths do reset — grep for busy.Store(false) hits only reclaim.go:406 (reclaimEmptied error branch) and 495 (gcShard after repack); the claim CAS sites are 182/402/491. A stuck busy permanently excludes the segment from evictable() (190) and pickVictim, so a single appendCold I/O/ENOSPC failure strands the segment (and its bytes) for the process lifetime — precisely under the disk-pressure condition eviction serves. MED: needs an appendCold failure, and impact is a space leak, not data loss.
- **Fix:** Reset the claim on failure, mirroring gcShard/reclaimEmptied: in evictSegment's appendCold error branch (and/or in evict()'s caller), add `seg.busy.Store(false)` before returning the error so a future eviction/GC pass can retry the segment.

### [MED] pickVictim rebuilds full shard live-byte map every call, GC pass becomes O(victims × intervals)

- **Where:** `pkg/block/journal/reclaim.go:530` · `perf` · area: block-journal-replay-gc
> STATUS: FIXED in #1948 — the live-byte map is summed once per GC pass
- **Verified:** CONFIRMED: pickVictim (reclaim.go:528-546) takes sh.mu and walks every fi in sh.index and every iv to rebuild `live` on each call; gcShard (475-505) loops pickVictim->repackSegment until nil, so cost is O(V×I) per pass with sh.mu held (same lock ReadAt uses). REACHABLE: background GC goroutine store.go:349 `s.GC(ctx, GCOptions{})` on defaultGCInterval ticker, plus API POST /shares/{name}/blockstore/gc (block_gc.go:144). Only the repacked segment's counts change between iterations, so the rescan is genuinely redundant.
- **Fix:** Compute live-byte-per-segment once per gcShard call (or maintain incrementally, subtracting relocated bytes from the source segment and not rescanning others), reuse across the victim-picking loop; only the just-repacked segment's counts need updating between iterations.

### [MED] findMove is a linear scan inside repack's index-repoint loop → O(moves²) per segment repack

- **Where:** `pkg/block/journal/reclaim.go:726` · `perf` · area: block-journal-replay-gc
> STATUS: FIXED in #1948 — moves are bucketed by version
- **Verified:** CONFIRMED: findMove (605-613) is a plain linear scan over `moves`; repackSegment step 4 (718-737, under sh.mu) calls it once per index interval still pointing at the victim, and that set is the same order as len(moves) (both derived from the victim's live records). A heavily-overwritten victim with thousands of small live fragments makes the repoint O(moves²) while holding sh.mu. REACHABLE via the same background-GC path (store.go:349) and the GC API handler.
- **Fix:** Build a map[key]int (key = fileOff+version, or index moves by original record so a direct index/key lookup works) once before the step-4 loop instead of re-scanning the slice per interval.

### [MED] evictSegment scans the whole shard index once per evicted segment

- **Where:** `pkg/block/journal/reclaim.go:207` · `perf` · area: block-journal-replay-gc
> STATUS: FIXED in #1914 / PR #1922
- **Verified:** CONFIRMED: evictSegment (207-240) walks all of sh.index twice (collect coldEntry, then flip cold) filtering loc.SegmentID==seg.id, each pass under sh.mu; evict() (64-104) loops evictSegment until targetBytes is met. No reverse segment->intervals index exists, so N evicted segments cost O(N×I). REACHABLE from two non-test entrypoints: Evict (shares DrainLocalSynced admin drain, which evicts many segments) and ensureSpace, the write-path capacity gate.
- **Fix:** When targetBytes requires evicting multiple segments, snapshot per-segment interval buckets once (single pass over sh.index) up front and consume from that snapshot across the evict loop, instead of re-scanning the full index per segment.

### [MED] Sealed segment with zero valid records panics recovery on recs[0]

- **Where:** `pkg/block/journal/recovery.go:152` · `bugs` · area: block-journal-replay-gc
> STATUS: FIXED in #1913 / PR #1921
- **Verified:** Confirmed at recovery.go:152: both preceding guards are `!sealed`-qualified (134 empty-active → emptyPool, 140 torn tail → truncate), then `sh := s.shardIndex(FileID(recs[0].fileID))` runs unconditionally. A sealed segment whose first record's header/payload fails scanValidRecords yields len(recs)==0 → index out of range panic during journal Open, i.e. hard startup crash for the share instead of a contained error/orphan. Reachable from the production open path (not test-only). MED: needs on-disk corruption of a sealed segment, but the failure mode (panic, no recovery) is disproportionate.
- **Fix:** Add a `len(recs) == 0` check that applies regardless of `sealed` (or explicitly for the sealed case) before computing `sh`: either treat it like the orphan case (`orphans = append(orphans, id); continue` after closing/dropping m) or return a wrapped error identifying the corrupt segment, instead of falling through to `recs[0]`.

### [MED] recover() is a ~375-line god-function mixing 8+ unrelated concerns

- **Where:** `pkg/block/journal/recovery.go:60` · `structure` · area: block-journal-replay-gc
> STATUS: FIXED #2015 — `recover()` is now a 54-line orchestrator over named phase helpers.
- **Verified:** Confirmed: func boundaries show recover() spans 60-436 (next func idxMissing at 437) = ~375 lines; single body threads actives/sealedByShard/indexByShard/emptyPool/orphans/tombstones/truncations/unsynced/missingIdx locals across segment scan, torn-tail truncate, idx rebuild, cold-log replay+compaction, counter reconcile, orphan sweep. Only helpers are idxMissing/rebuildIdx/sweepOrphans/fileSize. Reachable from Store open (non-test). Genuine maintainability/testability issue on the replay-correctness path.
- **Fix:** Extract named Store methods per phase, e.g. scanAndClassifySegments, replayRecords(indexByShard) tombstones/truncations, applyColdLog(indexByShard), reconcileByteCounters(indexByShard, sealedByShard, actives), assignActiveSegments(actives, emptyPool). Keep recover() as the orchestrator that calls them in order.

### [MED] Carve reads dirty local bytes for remote upload with an unverified raw pread — corrupted bytes get hashed and committed to the remote store as genuine content

- **Where:** `pkg/block/journal/carve.go:571` · `gaps` · area: block-journal-replay-recovery
> STATUS: TRACKED as #1912
- **Verified:** CONFIRMED. runReader.Read (carve.go:558-576) calls s.readPayload(rr.sh, iv.loc, rr.off, ...); readPayload (segment.go:~542) is a bare seg.fd.ReadAt with no CRC and no verifyReads gate — its own doc even notes ReadAt has the resolve-and-guard/verify path it skips. Carve hashes exactly those bytes (BLAKE3) and commits/uploads them, so latent local bit rot becomes permanent content-addressed remote data instead of a CorruptRangeError. Reachable: carveDispatcher→local.Carve (carve_dispatch.go:118) in prod, plus Flush/ManualSync. Fix: route runReader through the CRC-verifying record read (or verify record CRC before hashing) so carve fails closed.
- **Fix:** Gate runReader.Read (or readPayload itself, for this call site) on the store's verifyReads path — read via readRecordAt/verifiedRead and return an error instead of raw bytes when the record's CRC does not match, so carveRun aborts that run instead of uploading unverified/corrupted content.

### [MED] GC repack copies victim payload with raw ReadAt, bypassing the store's own CRC verification — silently launders bit-rot into the new segment

- **Where:** `pkg/block/journal/reclaim.go:675` · `gaps` · area: block-journal-replay-recovery
> STATUS: TRACKED as #1912
- **Verified:** CONFIRMED. reclaim.go:674-678 `victim.fd.ReadAt(data, m.srcOff)` raw pread, no CRC; writeDataRecord (reclaim.go:831-848) stamps a FRESH crc over those bytes. readRecordAt (record.go:190-193) is where the CRC check lives and repack never uses it; store has verifiedRead (index.go:341-363) + CorruptRangeError and verifyReads IS enabled in prod (shares/service.go:1170-1171 SetVerifyReads(!writeback)), so read path fails closed while repack launders. victimMarkers (reclaim.go:808-809) → scanValidRecords (record.go:206-216) `break`s on first torn/CRC-bad record, silently dropping later tombstone/truncate markers. Reachable: repackSegment ← GC (reclaim.go:494) ← startBackgroundGC ticker (store.go:287,333-352), non-test. Fix: read survivors via readRecordAt (verify CRC, abort repack + surface CorruptRangeError) and make victimMarkers scan the whole segment / fail the repack when validUpTo < segSize.
- **Fix:** In repackSegment, read each moved record through readRecordAt (the same verified path index.go's verifiedRead uses) instead of a raw victim.fd.ReadAt, and abort/quarantine the segment (return an error, keep the victim) on CRC mismatch instead of copying it forward. Apply the same to victimMarkers' scanValidRecords call — surface/report a truncated scan instead of silently accepting whatever prefix parsed.

### [MED] FSStore.FileSize and DataExtents skip the legacy fault-in seam, so SEEK/READ_PLUS and size queries see holes/zero-size for un-drained pre-journal payloads

- **Where:** `pkg/block/local/fs/fs.go:225` · `bugs` · area: block-local · *re-confirmed*
> STATUS: FIXED in #1948
- **Verified:** Confirmed asymmetry in source: WriteAt/ReadAt/Delete/Truncate each call s.materializeLegacy first; FileSize and DataExtents delegate straight to s.Store. Reachable in production — shares/service.go:2937 sets fsOpts.MigrateLegacyLocalOnly, and legacy_migrate.go's own doc states the fault-in exists so 'a read never observes zeros'. engine.DataExtents (dataextents.go:31) is called by NFSv4.2 SEEK/READ_PLUS with no prior ReadAt, so an un-drained payload's log-only (never rolled-up, hence no FileChunk row to union in) bytes report as a hole → RFC 7862 sparse-copy loss. MED not HIGH: bounded to the one-time async drain window, and rolled-up ranges are still covered by the manifest union in dataextents.go.
- **Fix:** Call s.materializeLegacy(payloadID) at the top of FSStore.FileSize and FSStore.DataExtents (mirroring WriteAt/ReadAt/Delete/Truncate), same as the other four data-plane shims. FileSize currently has no error return to propagate a materialize failure through — either add one or fall back to logging+treating as not-found consistent with existing error handling in the package.

### [MED] FSStore.Stats() does a full O(files) journal scan + slice alloc just to get a count

- **Where:** `pkg/block/local/fs/fs.go:276` · `perf` · area: block-local
> STATUS: FIXED in #1948 — Stats reads journal.Store.FileCount
- **Verified:** CONFIRMED fs.go:276 `FileCount: len(s.Store.ListFiles(...))`; journal Store.ListFiles (store.go ~641) locks every shard and appends every FileID to an unpreallocated slice. REACHABLE non-test: engine/stats.go:79 (Store.Stats -> runtime.GetShareUsage:706 -> api/handlers/shares.go:1626) and engine/stats.go:125/244 (getStats -> shares/service.go:2639 GetStatsLite, :2801 warm start). Aggravating: GetStatsLite's own doc says it is 'O(1)-ish and safe to call on a hot path such as a metrics scrape' — false, it walks every shard; engine.Stats() additionally calls local.ListFiles a SECOND time (stats.go:80). runtime/metrics.go:21 already comments 'Avoid GetShareUsage here' because of this cost, i.e. the cost is known real. Shard locks contend with WriteAt/ReadAt.
- **Fix:** Add a cheap Count() on journal.Store that sums shard index sizes under lock without building/returning a slice, and call that from Stats() instead of ListFiles().

### [MED] SetRetentionPolicy is a no-op in every LocalStore implementation but stays a required interface method that live callers invoke with real config

- **Where:** `pkg/block/local/local.go:111` · `structure` · area: block-local · *re-confirmed*
> STATUS: FIXED in #1948 — SetRetentionPolicy removed from the interface
- **Verified:** CONFIRMED: local.go:108-111 declares it as a 'compatibility no-op'; fs.go:250 and memory.go:286 are empty bodies; engine/health.go:87 delegates; live calls at shares/service.go:1067 and :1758. Knob is user-settable over REST (handlers/shares.go:357-370 ParseRetentionPolicy/ValidateRetentionPolicy) so RetentionTTL is validated then silently discarded. Only RetentionPin survives, and via a separate SetEvictionEnabled call (service.go:1064), not this method.
- **Fix:** Remove SetRetentionPolicy from the LocalStore interface and its two no-op implementations; delete (or explicitly no-op with a comment at the call site, not the interface) the calls in shares/service.go. If retention tuning is ever reintroduced, add it back as a capability interface asserted only against backends that support it.

### [MED] MemoryStore.writeLocked: exact-size realloc+full-copy on every growing write — O(n^2) total

- **Where:** `pkg/block/local/memory/memory.go:60` · `perf` · area: block-local
> STATUS: FIXED in #1948 — the grow path appends instead of exact-size reallocating
- **Verified:** CONFIRMED: writeLocked does `if int64(len(f.buf)) < end { grown := make([]byte, end); copy(grown, f.buf); f.buf = grown }` with zero capacity slack — every extending WriteAt reallocates and copies the whole buffer, so N sequential appends copy O(n^2) bytes. REACHABLE from a non-test entrypoint: pkg/controlplane/runtime/shares/service.go:3036 `case "memory": store := localmemory.New()` is a selectable local_store type in the share config, reached through the same factory as the fs backend. Downgraded HIGH→MED: the memory backend is not the default or recommended production local store (fs/journal is), so the O(n^2) is real but confined to an opt-in configuration.
- **Fix:** Grow via append() so Go's amortized geometric growth applies: `if int64(len(f.buf)) < end { f.buf = append(f.buf, make([]byte, end-int64(len(f.buf)))...) }`. Same zero-fill semantics, amortized O(1) instead of a full copy per growing write.

### [MED] Package doc comment describes a superseded design (stale/misleading)

- **Where:** `pkg/block/remote/doc.go:1` · `comments` · area: block-remote · *re-confirmed*
> STATUS: FIXED in #1946
- **Verified:** CONFIRMED pkg/block/remote/doc.go (whole file, 9 lines): claims backends '(S3, filesystem, memory)' and 'Key format: "{payloadID}/block-{blockIdx}"'. Both refuted in-package: remote.go:1-16 + :66-73 state the production surface is opaque blockID with on-wire key block.FormatBlockKey(blockID) = 'blocks/<blockID>'; ls pkg/block/remote shows only memory/ and s3/ subpackages — no filesystem backend exists. Package is production (remote.RemoteStore consumed at engine/syncer.go:36,168). Two competing 'Package remote' doc comments in one package, the stale one wins alphabetically in godoc. Fix: delete doc.go — remote.go already carries the authoritative package comment.
- **Fix:** Delete doc.go's stale content and fold a short package comment into remote.go (which already carries the real, current doc), or rewrite doc.go to match remote.go's description (blocks/<blockID>, s3 + memory backends only).

### [MED] SSRF: S3 endpoint safety validated once at config-time, never enforced on live HTTP connections (redirect-follow + DNS-rebind bypass)

- **Where:** `pkg/block/remote/s3/store.go:155` · `security` · area: block-remote · *re-confirmed*
> STATUS: TRACKED as #1917
- **Verified:** Confirmed: transport at store.go:154-173 uses a bare net.Dialer, http.Client at 175-179 sets only Transport+Timeout (no CheckRedirect, so Go's default 10-redirect follow applies). ValidateEndpoint (line 285) resolves + checkEndpointIP once and is only called from the config path (blockstore_init.go:91); nothing re-checks per dial. Both bypasses stand: a 3xx from the validated endpoint to 169.254.169.254 is followed, and the real dialer re-resolves the hostname on every later request (rebind). Downgraded HIGH->MED: exploitation requires the privileged S3-endpoint-config capability plus attacker control of that endpoint/DNS — but it does defeat the exact control this code implements.
- **Fix:** Wrap DialContext (store.go:162-165) to run checkEndpointIP on every resolved IP before connect, and set httpClient.CheckRedirect (line 176-180) to refuse redirects (or re-check the target IP before following).

### [MED] SSRF endpoint guard is config-time-only; never re-enforced on the actual dial (DNS-rebind + redirect-follow bypass)

- **Where:** `pkg/block/remote/s3/store.go:162` · `gaps` · area: block-remote
> STATUS: TRACKED as #1917
- **Verified:** CONFIRMED. ValidateEndpoint (store.go:263-328) resolves + checks IPs once at config-create (only caller: runtime/blockstore_init.go:91, non-test). NewFromConfig builds http.Transport with a stock net.Dialer DialContext (store.go:161-165) — no per-connection IP recheck/pin — and http.Client (store.go:176-180) leaves CheckRedirect nil, i.e. default follow-10. So DNS rebinding at connect time and a 3xx Location to an internal host both escape the guard; the doc comment's 'refused before S3 SDK ever issues a request' only holds for the literal configured hostname. Not spec-mandated behavior anywhere — guard's stated purpose is exactly this threat. Mitigating: endpoint is admin-configured control-plane input, so attack needs a hostile/compromised endpoint operator or attacker-controlled DNS → MED not HIGH. Fix: custom DialContext that re-resolves and runs checkEndpointIP on the dialed IP, plus CheckRedirect that re-validates (or refuses) redirect targets.
- **Fix:** Wire IP pinning into DialContext: after ValidateEndpoint resolves+approves an IP, have the http.Transport's DialContext re-validate (or reuse) that resolved IP for every connection (e.g. wrap net.Dialer.DialContext to call checkEndpointIP on the address actually being dialed, using the same allowPrivate policy), and set httpClient.CheckRedirect to either refuse redirects outright (S3 PUT/GET/DELETE never legitimately need them) or re-run checkEndpointIP against the redirect Location before following.

### [MED] Anonymous requester zero-value UID/GID collides with explicit "0@localdomain" ACEs and GROUP@ — missing authz guard for unauthenticated identity

- **Where:** `pkg/metadata/acl/evaluate.go:393` · `security` · area: metadata-acl-errors-backup · *re-confirmed*
> STATUS: TRACKED as #1918
- **Verified:** Confirmed from source: SpecialGroup arm compares raw evalCtx.GID/GIDs against FileOwnerGID, and the default arm does `id == evalCtx.UID` / `== evalCtx.GID`; only FileOwnerUID gets the AnonymousFileOwnerUID sentinel. Both anonymous builders (auth_permissions.go:424-427, file_access_checker.go:101-104) leave UID/GID at 0 and set only FileOwnerUID — and auth_permissions.go:421-423 admits the residual verbatim ('GROUP@ and the "<n>@localdomain" form may still match on UID/GID-0 owned files'). Reachable from live NFS/SMB permission checks whenever Identity==nil or Identity.UID==nil. MED not HIGH: it is a documented known residual and requires a root-group-owned file with a GROUP@/0@localdomain ACE plus an export that admits identity-less sessions.
- **Fix:** Add an explicit no-identity signal to EvaluateContext (HasIdentity bool or Anonymous UID/GID sentinels) and gate the SpecialGroup arm (lines 393-402) and the parseLocalDomainID id==UID/GID comparisons (lines 425-433) on it, mirroring the OWNER@ treatment.

### [MED] Evaluate() and EvaluateGranted() duplicate the entire DACL-walk algorithm

- **Where:** `pkg/metadata/acl/evaluate.go:170` · `structure` · area: metadata-acl-errors-backup · *re-confirmed*
> STATUS: FIXED #2014 — `walkDACL` is the single walk; `Evaluate` and `EvaluateGranted` are thin wrappers over it.
- **Verified:** CONFIRMED line-by-line: owner-rights pre-scan (195-212 vs 302-319), ACE accumulation switch with identical `ace.AccessMask &^ (allowedBits|deniedBits)` first-match-wins (217-250 vs 324-351), and owner-implicit tail incl. RequesterHasTakeOwnership (263-268 vs 353-358) are byte-equivalent; only the early-term mask and return type differ. Both reachable from non-test code: Evaluate at auth_permissions.go:493/722/1067 and file_access_checker.go:83; EvaluateGranted at auth_permissions.go:959/998/1107. MED stands — duplicated security-critical access-check algorithm with no mechanism keeping the copies in sync.
- **Fix:** Extract a private walkDACL(a *ACL, evalCtx *EvaluateContext, mask uint32) (allowed, denied uint32) that runs the owner-rights pre-scan + ACE loop + owner-implicit tail once. Evaluate becomes allowed,_ := walkDACL(...); return allowed&requestedMask == requestedMask; EvaluateGranted becomes allowed,_ := walkDACL(...); return allowed & probeMask.

### [MED] acl.EvaluateContext construction duplicated 3x (inline + 2 near-identical builders)

- **Where:** `pkg/metadata/auth_permissions.go:441` · `bloat` · area: metadata-auth
> STATUS: PARTIAL #2014 — the inline construction and `buildFileAccessEvalContext` now both route through one `buildEvalContext`. The third copy, `buildAttrEvalContext` in `file_access_checker.go`, was left in place because that file was being rewritten concurrently by #2020; #2030 collapses it now that #2020 has merged.
- **Verified:** CONFIRMED 3x copy. Identical ~25-line block (anonymous AnonymousFileOwnerUID sentinel branch, then UID/GIDs/GID/Who switch/SID/GroupSIDs/RequesterHasTakeOwnership) at auth_permissions.go:415-467 (inline in evaluateACLPermissions), auth_permissions.go:1159-1198 (buildFileAccessEvalContext), file_access_checker.go:95-134 (buildAttrEvalContext). Only differences: owner source (file.UID/GID vs attr.UID/GID) and the root-bypass that sits before construction in evaluateACLPermissions. Both comment headers self-admit the mirroring. All three on prod paths (NFS CheckPermissions, SMB CheckFileAccess, SMB ABE). Fix: one buildEvalContext(ownerUID, ownerGID uint32, authCtx) and have all three call it; evaluateACLPermissions keeps only its root/ReadOnly branch.
- **Fix:** Extract one helper, e.g. buildEvalContext(identity *Identity, ownerUID, ownerGID uint32) *acl.EvaluateContext, and call it from evaluateACLPermissions, buildFileAccessEvalContext, and buildAttrEvalContext (the latter two just pass file.UID/file.GID or attr.UID/attr.GID).

### [MED] Identity.HasGID lazily mutates shared cache without synchronization, racing across concurrent requests on the same SMB session

- **Where:** `pkg/metadata/auth_identity.go:214` · `bugs` · area: metadata-auth-permissions
> STATUS: FIXED in #1902
- **Verified:** Confirmed both halves: HasGID (auth_identity.go:214-228) lazily assigns i.gidSet and fills it with no lock, under a doc that says 'NOT thread-safe'; BuildAuthContextFromUser (auth_helper.go:~152-158) reuses one memoized *Identity per session via ctx.cachedIdentity/maybeCacheIdentity, and HasGID is called from the hot permission path (auth_permissions.go:372 etc.). Concurrency is reachable: SMB async/deferred paths run handlers in goroutines against the same session (handlers/lock_async.go:109 resumePendingLock, handler.go:1662/1805-1817, create_post_break.go:1574), plus multichannel. Two concurrent first-touches → Go fatal 'concurrent map writes'. MED not HIGH: the race window is only the one-time lazy build per identity, after which the map is read-only.
- **Fix:** Guard gidSet construction with a sync.Once / sync.RWMutex on Identity, or pre-build gidSet once at buildIdentity time (before the pointer is published to the session cache) instead of lazily inside HasGID, so the cached object is genuinely immutable once shared.

### [MED] checkDeletePermission's HasDeleteAccess bypass skips the store-level read-only ceiling

- **Where:** `pkg/metadata/auth_permissions.go:751` · `security` · area: metadata-auth-permissions · *re-confirmed*
> STATUS: TRACKED as #1918
- **Verified:** Code confirmed: line 751 `if ctx.HasDeleteAccess && !ctx.ShareReadOnly { return nil }` — no store-level check, while the doc at line 739-740 claims 'Read-only shares still block this path'. shareForbidsWrites (line 566) exists and is used by siblings. Callers RemoveFile (file_remove.go:76) and RemoveDirectory (directory.go:175) add no earlier store-level gate. Downgraded HIGH->MED: the seed's primary scenario is refuted — tree_connect.go:145-149 clamps a ReadWrite/Admin grant to PermissionRead when share.ReadOnly, and auth_helper.go:92/177/419 derive ShareReadOnly from that, so a read-only share yields ShareReadOnly=true at connect time. The live gap is the toggle-after-connect window (share flipped read-only while a tree/handle is held) — exactly the case checkFilePermissionsFile's comment at lines 293-300 explicitly defends against and this path does not.
- **Fix:** Use the existing helper: `if ctx.HasDeleteAccess && !s.shareForbidsWrites(ctx, parentHandle) { return nil }` so both ceilings gate the fast path, matching checkFilePermissionsFile (line 305) and CheckParentCreateAccessFile (line ~690).

### [MED] checkDeletePermission Rule 1 skips store-level share ReadOnly check (ceiling asymmetry vs. rest of file)

- **Where:** `pkg/metadata/auth_permissions.go:749` · `slop` · area: metadata-auth-permissions
> STATUS: FIXED in #1932 — Rule 1 goes through shareForbidsWrites
- **Verified:** CONFIRMED at auth_permissions.go:751 `if ctx.HasDeleteAccess && !ctx.ShareReadOnly { return nil }` with no store-level lookup. Asymmetry is real: line 305 (checkFilePermissionsFile) gates on `!ctx.ShareReadOnly && !shareReadOnly` after a live GetShareOptions at line 267; line 690 (CheckParentCreateAccess) gates on `ctx.ShareReadOnly || storeReadOnly` after GetShareOptions at 678; helper s.shareIsReadOnly (line 579) already exists and is used at 538/567. REACHABLE: HasDeleteAccess is set on exactly one non-test path, internal/adapter/smb/handlers/close.go:493, which then calls RemoveFile (file_remove.go:76) / RemoveDirectory (directory.go:175) → checkDeletePermission; neither caller does its own share-readonly gate. ctx.ShareReadOnly is derived per-op from ctx.Permission (auth_helper.go:92/177/419), but ctx.Permission is frozen at tree-connect, so an UpdateShareOptions ReadOnly flip is not seen by an already-connected tree. Downgraded HIGH→MED: exploit window needs an admin toggling a share read-only while a DELETE_ON_CLOSE handle is already open — narrow, not remotely triggerable.
- **Fix:** In checkDeletePermission, before the Rule 1 short-circuit, also fetch the live share option (via s.shareIsReadOnly(ctx, parentHandle)) and require it be false, mirroring checkFilePermissionsFile: `if ctx.HasDeleteAccess && !ctx.ShareReadOnly && !s.shareIsReadOnly(ctx, parentHandle) { return nil }`.

### [MED] ACL EvaluateContext construction triplicated (self-described as 'mirrors X' three times) instead of one shared builder

- **Where:** `pkg/metadata/auth_permissions.go:1159` · `structure` · area: metadata-auth-permissions
> STATUS: PARTIAL #2014 — same collapse as the sibling finding at `auth_permissions.go:441`: two of the three self-described “mirrors” are one builder, and #2030 folds in the third.
- **Verified:** CONFIRMED three copies, near-identical incl. comments: inline in evaluateACLPermissions (auth_permissions.go:~416-466, incl. the AnonymousFileOwnerUID sentinel branch), buildFileAccessEvalContext (:1159-1198, doc literally says 'mirrors evaluateACLPermissions'), buildAttrEvalContext (file_access_checker.go:95-134, doc says 'mirrors buildFileAccessEvalContext'). File embeds FileAttr (file_types.go: `FileAttr` embedded), so buildFileAccessEvalContext(file,ctx) == buildAttrEvalContext(&file.FileAttr,ctx) with zero behavior change; buildFileAccessEvalContext has 4 prod call sites (auth_permissions.go:715/958/1066/1100). Security-relevant drift risk is genuine (#540 sentinel had to be replicated). MED not HIGH: no current divergence between the copies.
- **Fix:** Delete buildFileAccessEvalContext and the inline block in evaluateACLPermissions; make buildAttrEvalContext(attr *FileAttr, authCtx *AuthContext) the single builder and call it everywhere via &file.FileAttr.

### [MED] checkFilePermissionsFile does an uncached store.GetShareOptions round trip on every single permission check

- **Where:** `pkg/metadata/auth_permissions.go:267` · `perf` · area: metadata-auth-permissions
- **Verified:** CONFIRMED auth_permissions.go:266-267: unconditional store.GetShareOptions(ctx, file.ShareName) inside checkFilePermissionsFile, the funnel every read/write/create/setattr routes through (checkFilePermissions:245 delegates to it). sqlite/shares.go:50 CONFIRMED as an uncached `SELECT options FROM shares WHERE share_name = ?1` + json.Unmarshal (postgres identical), and sqlite runs a single-connection pool so it also serializes. badger/shares.go:61-71 CONFIRMED to have a shareCache with generation-based invalidation — proving the call site was already recognized as hot, with the fix landing in only one of four backends. Share options are near-static, so this is per-op redundant recomputation on the data plane.
- **Fix:** Add the same per-store ShareOptions cache to sqlite/postgres, or cache *ShareOptions at the Service call site keyed by share name with generation-based invalidation on share update.

### [MED] fileReadCache and direntCache duplicate the entire generation-guarded store/invalidate/prune logic

- **Where:** `pkg/metadata/store/badger/create_cache.go:78` · `bloat` · area: metadata-badger-cache-index
> STATUS: DEFERRED to #1828 — subsumed by the metadata-store unification (SQL family + KV family behind a shared base); not fixable as an isolated patch.
- **Verified:** CONFIRMED. create_cache.go direntCache (struct m sync.Map/n atomic.Int64/gen atomic.Uint64/prune atomic.Bool; get/store/invalidate/pruneToHalf) is a line-for-line twin of read_cache.go fileReadCache incl. the identical gen-recheck-after-Swap comment, the CompareAndDelete-only-if-not-superseded step, the gen-bump-BEFORE-LoadAndDelete ordering note, and the CompareAndSwap-guarded halve loop; only the value type (direntEntry vs *metadata.File) and the cap const differ. No shared generic helper exists in pkg/. Reachable: both caches live on BadgerMetadataStore, the default production store. Fix: one generic `type genCache[V comparable]` with get/store/invalidate/pruneToHalf(cap int) — both value types are comparable so sync.Map CompareAndDelete works unchanged.
- **Fix:** Extract a generic genCache[V any] type (parametrized on the stored value) shared by fileReadCache and direntCache; direntCache becomes genCache[direntEntry], fileReadCache becomes genCache[*metadata.File]. shareReadCache can stay separate since it deliberately omits cap/prune (shares are few).

### [MED] Reset()/RestoreSnapshot wipe BadgerDB but never invalidate the four in-process caches

- **Where:** `pkg/metadata/store/badger/reset.go:17` · `gaps` · area: metadata-badger-cache-index · *re-confirmed*
> STATUS: PARTIAL — #1950 flushes shareCache on Reset and RestoreSnapshot. readCache, parentCache and direntCache are still not flushed, so a restore can keep serving pre-restore file attrs and dirent hits. Tracked in #1961
- **Verified:** CONFIRMED. badger/reset.go:17-25 Reset = ctx check + s.db.DropAll(), nothing else; snapshot_store.go:181+ RestoreSnapshot streams via WriteBatch (never through badgerTransaction, so no dirtyFiles/dirtyDirents invalidation in transaction.go:157-172) and only calls initUsedBytesAndPayloadIndex at :309. read_cache.go has NO TTL — entries die only on invalidate() or pruneToHalf at 8192 cap. Reachable non-test: runtime/snapshot.go:1549 resetable.Reset(ctx) and :1567 snapshotable.RestoreSnapshot on the store from GetMetadataStoreForShare (:1438) = the live instance that served the share. GetFileForRead (files.go:44-58) / GetShareOptions return on cache hit without touching badger, and a restore of the same share reuses the same fileID UUIDs + shareName, so warm keys keep serving pre-restore attrs. Mitigated only by the share staying disabled post-restore (snapshot.go:1646 comment) — no cache flush anywhere. Fix: add a store-wide flush (bump gen + clear sync.Maps on all four caches) at the end of Reset.
- **Fix:** Add a clear() method to each cache type (fileReadCache, direntCache, shareReadCache) that bumps gen, calls m.Clear() (Go 1.23+ sync.Map.Clear, safe for concurrent readers) and resets the counter, then call s.readCache.clear(); s.parentCache.clear(); s.direntCache.clear(); s.shareCache.clear() in Reset() right after a successful DropAll.

### [MED] Struct doc claims a single mutex that doesn't exist

- **Where:** `pkg/metadata/store/badger/store.go:38` · `comments` · area: metadata-badger-support
> STATUS: FIXED in #1970 — no mu field exists; the doc now describes MVCC serialization with per-subsystem locks. The identical false claim in the memory backend was corrected with it.
- **Verified:** CONFIRMED badger/store.go:37-41 'All operations are protected by a single read-write mutex (mu)'. BadgerMetadataStore has NO `mu` field; grep shows only capsMu(103), lockStoreMu(134), clientStoreMu(138), durableStoreMu(142), recoveryStoreMu(146), quotaMu(159) + a nested statsCache mu(129); concurrency is badger MVCC txns. Reachable: NewBadgerMetadataStore ← WithDefaults/WithDefaultsAndCaches ← runtime/init.go:97 and runtime/stores/service.go:192 (default backend). MED: a reader reasoning about locking/deadlocks from this is actively misled.
- **Fix:** Rewrite to describe the actual model: badger MVCC transactions for the data path + separate fine-grained mutexes per subsystem, or delete the paragraph.

### [MED] "File Handle Strategy" doc block contradicts the UUID-handle design described later in the same file

- **Where:** `pkg/metadata/store/badger/store.go:51` · `comments` · area: metadata-badger-support
> STATUS: FIXED in #1970 — GenerateHandle ignores its path argument and mints a UUID; the doc now says so.
- **Verified:** CONFIRMED badger/store.go:51-60 'File handles are generated from filesystem paths ... reversible ... paths exceeding 64 bytes converted to hash-based format with reverse mapping'. Actual impl badger/shares.go:20-25 GenerateHandle IGNORES its `path` arg and returns metadata.GenerateNewHandle(shareName); there is no hash-reverse-mapping keyspace. Contradicting 'UUID-Based File Identification' section is in encoding.go:25-31, not 'the same file' as the claim states — minor misattribution, defect itself stands. Reachable: same default-backend constructors as idx 7.
- **Fix:** Delete the stale "File Handle Strategy" paragraph and the "Path-based file handles for import/export capability" bullet under Key Features; keep only the UUID-based section that matches current code.

### [MED] BlockRecordStore CRUD fully duplicated between store-level and tx-level instead of sharing a *Txn helper

- **Where:** `pkg/metadata/store/badger/block_record_store.go:50` · `bloat` · area: metadata-badger-write-txn
> STATUS: DEFERRED to #1828 — subsumed by the metadata-store unification (SQL family + KV family behind a shared base); not fixable as an isolated patch.
- **Verified:** CONFIRMED. block_record_store.go:50-162 (tx methods) and :168-312 (store methods) each implement encode/Set, Get+decode, Delete-with-ErrKeyNotFound-swallow, prefix iterate+decode+collect, and the floor-at-0 RMW; store level only adds ctx.Err() guards, db.Update/db.View wrappers, and updateWithConflictRetry on DecrLiveChunkCount. Established sibling pattern confirmed: objects.go:52 reapBlockTxn, objects.go:679 listFileChunksTxn factor the txn-scoped body. Reachable: production callers of the store-level surface at pkg/controlplane/runtime/blockgc_reconcile_reclaim.go:63, pkg/block/engine/gc_block.go:138, compaction.go:155, reconcile.go:179; tx-level via pkg/metadata/block_record_store.go:145. Fix: extract putBlockRecordTxn/getBlockRecordTxn/deleteBlockRecordTxn/walkBlockRecordsTxn/decrLiveChunkTxn(*badger.Txn,...) and call from both layers.
- **Fix:** Extract putBlockRecordTx/getBlockRecordTx/deleteBlockRecordTx/walkBlockRecordsTx(txn *badgerdb.Txn, ...) helpers; have both tx.XxxBlockRecord and BadgerMetadataStore.XxxBlockRecord call them, matching the pattern already used elsewhere in the package.

### [MED] Link-count-with-type-default lookup reinvented 6 times instead of reusing fileLinkCountTxn

- **Where:** `pkg/metadata/store/badger/transaction.go:267` · `bloat` · area: metadata-badger-write-txn
> STATUS: FIXED in #1972 — the six open-coded blocks call fileLinkCountTxn, and the hidden bug is closed: the tx-scoped GetFileByPayloadID legacy scan returned without loadManifest, so unindexed rows came back with empty Blocks. Both fallbacks route through loadEnrichedFileByID. A separate divergence found here is tracked as #1973.
- **Verified:** CONFIRMED, including the divergence. The Get(keyLinkCount)->switch nil/ErrKeyNotFound->default 2 (dir) / 1 block is present verbatim at transaction.go:267-284, 802-824, 1228-1241 (root variant, defaults 2), 1541-1553 (seed-then-override phrasing), 1636-1654, and files.go:194-212. objects.go:837 fileLinkCountTxn implements exactly this fallback and is called only from objects.go:585, objects.go:821 and snapshot_store.go:143 — none of the six. Divergence confirmed by reading tx GetFileByPayloadID (transaction.go:1603): the fast path returns loadEnrichedFileByID (which calls loadManifest at :1557) but the legacy full-scan branch sets Nlink + derivePath and returns WITHOUT loadManifest, while the store-level twin does call it (files.go:217) — so a pre-#1435 unindexed row resolved inside a txn comes back with empty Blocks. Reachable: tx.GetFileByPayloadID called from pkg/controlplane/runtime/shares/service.go:802, shares/coordinator.go:197, pkg/metadata/block_record_store.go:51. Fix: one applyLinkCount(txn,*File) helper (fileLinkCountTxn setter form) at all six sites, and add the missing loadManifest(tx.txn, file) in the legacy-scan branch.
- **Fix:** Replace all six blocks with file.Nlink = fileLinkCountTxn(tx.txn, file) (or txn directly at store level), and add the missing loadManifest call to transaction.go's GetFileByPayloadID scan branch, or better, route it through loadEnrichedFileByID entirely.

### [MED] Per-chunk DeleteSynced+MarkSynced loop issues 2N unbatched store round-trips per block commit

- **Where:** `pkg/metadata/block_record_store.go:159` · `perf` · area: metadata-core-contract
- **Verified:** CONFIRMED: block_record_store.go:159-169 loops tx.DeleteSynced then tx.MarkSynced per chunk; postgres/synced_hash_store.go:214 and :253 each do a single tx.tx.Exec, so 2*len(chunks) sequential network round-trips inside one txn (plus a tx.Put per fileChunk just above). Batching precedent CONFIRMED in the same write path: postgres/file_block_refs.go:55 builds a pgx.Batch and :72 SendBatch. REACHABLE non-test: engine/blocksink.go:256 (carve/upload commit) and legacy_migration.go:381, compaction.go:315; every SQL backend's CommitBlock delegates here (sqlite/block_record_store.go:130, postgres:164, badger:322). With block objects packing hundreds of FastCDC chunks this is hundreds of serialized round-trips per commit on the write path.
- **Fix:** Batch the per-chunk synced-marker writes the same way putFileChunkRefs batches file_block_refs: one pgx.Batch (or one multi-row INSERT ... ON CONFLICT DO UPDATE) covering all chunks' locator overwrites, instead of 2*N sequential calls.

### [MED] CreateHardLink discards GetLinkCount error and can clobber an existing file's true link count down to 1

- **Where:** `pkg/metadata/file_create.go:180` · `bugs` · area: metadata-file-io-write · *re-confirmed*
> STATUS: FIXED in #1901 (error now propagates)
- **Verified:** Confirmed at file_create.go:180: `linkCount, _ := tx.GetLinkCount(...)` followed immediately by SetLinkCount(linkCount+1), so a read failure persists nlink=1 for an already multiply-linked target. Only error-swallowing GetLinkCount call site besides file_remove.go:171 — directory.go:255, file_create.go:470 and file_modify.go:834/884/894 all propagate. Reachable from the NFS LINK / SMB hardlink path. Under-counted nlink then lets a later unlink take RemoveFile's last-link branch and reclaim content other names still reference. MED: requires a backend read failure.
- **Fix:** Check the error and abort the transaction: `linkCount, err := tx.GetLinkCount(ctx.Context, targetHandle); if err != nil { return err }` before calling SetLinkCount.

### [MED] createEntry silently skips the parent directory's link-count bump on mkdir when GetLinkCount errors

- **Where:** `pkg/metadata/file_create.go:470` · `bugs` · area: metadata-file-io-write
> STATUS: FIXED in #1972 — the `if err == nil` guard discarded a failed parent GetLinkCount while the tx still committed the new directory, leaving parent nlink permanently one short. The read is now tx-critical and propagates, matching how Move and RemoveDirectory already treat it.
- **Verified:** Confirmed verbatim at file_create.go:469-476: `parentLinkCount, err := tx.GetLinkCount(...); if err == nil { SetLinkCount(parentLinkCount+1) }` — non-nil err drops the '..' nlink bump while the tx still commits the new dir + child edge, and the error is discarded with no log. Reachable: core mkdir path. Contrast confirmed at file_modify.go:831-834 ("The read is tx-critical: a failed GetLinkCount must roll the...") and :884/:894, which DO propagate — same read, opposite treatment in the same package. Compounding factor: on postgres/sqlite tx.GetLinkCount itself swallows errors into (0,nil) (postgres/transaction.go:823, sqlite/transaction.go:~770), so on those backends the failure mode is worse — parent nlink is silently SET TO 1, not skipped.
- **Fix:** Either propagate the error to abort the whole create (matching the RemoveDirectory / Move treatment of the same read), or at minimum log at Error level when skipped so it isn't silently lost.

### [MED] RemoveFile silently assumes nlink=1 on GetLinkCount failure — can delete content still referenced by surviving hard links

- **Where:** `pkg/metadata/file_remove.go:171` · `bugs` · area: metadata-file-io-write
> STATUS: FIXED in #1975 — an earlier revision of this file marked it fixed by #1901; that was wrong. #1901 only made the SQL backends propagate GetLinkCount errors, which makes the `linkCount = 1` guess MORE reachable, not less. The fallback survived on develop verbatim. #1975 returns the error instead, which is safe because backends report a missing count as (0, nil) rather than an error.
- **Verified:** Confirmed verbatim at file_remove.go:171-174 (`if lcErr != nil { linkCount = 1 }`), inside the same tx whose own comment (163-170) calls this read tx-critical for exactly the nlink→0 decision. Inconsistent with the sibling handling in file_modify.go:831-834 ('The read is tx-critical: a failed GetLinkCount must roll the...') and 884/894, which return the error. Reachable: RemoveFile is the NFS/SMB unlink path. On a transient backend read error a multiply-linked file takes the last-link branch, leaves returnFile.PayloadID populated and the caller reaps content still referenced by surviving names. MED: needs a GetLinkCount failure, but the outcome is silent data loss.
- **Fix:** Return lcErr instead of defaulting: `if lcErr != nil { return lcErr }` — abort the transaction (and the whole RemoveFile call) on a failed link-count read, consistent with how Move.go and RemoveDirectory already treat GetLinkCount errors as tx-critical.

### [MED] Service struct is a god object mixing store routing, NLM locking, quota policy, dir-notification wiring, and hot-path write coordination

- **Where:** `pkg/metadata/service.go:40` · `structure` · area: metadata-file-io-write
> STATUS: OPEN in #2029 — validated, stays a redesign. Verdict PARTIAL: the concern mix and the 1583 lines are real, but the "128 receivers sharing s.mu" framing is wrong. 128 `Service` receivers exist across pkg/metadata, of which only 20 touch s.mu; the store-routing hot path already bypasses it via the lock-free storeCache, and the write hot path takes it once (shareWriteback) at a measured ~77 ns/op at 8 cores vs ~45 ns for a sync.Map control — ~30 ns on a microsecond-scale op, i.e. no evidence s.mu costs anything. s.mu guards exactly stores/lockManagers/unifiedViews/dirChangeNotifiers/writebackShares/quotas/removeGen/graceDuration/graceCoordinator/byteRangeReleaseHook. Any split must preserve two things: (1) RegisterStoreForShare snapshots removeGen[share] under s.mu, recovers the lock manager OUTSIDE s.mu, then re-checks the generation under the SAME acquisition that publishes store+lockManager+dirChangeNotifier together — so the decision-to-publish and the publish are one atomic step and a share is never observable store-visible/manager-absent; (2) the grace coordinator must be signalled exactly once per grace window across three asymmetric paths (normal timer end, lost-publish race → no OnLockGraceEnd, removed-mid-flight → exactly one OnLockGraceEnd, RemoveStoreForShare → capture IsInGracePeriod BEFORE AbortGracePeriod). Extracting QuotaManager or LockManagerRegistry moves state across both invariants at once, so no bounded subset was found.
- **Verified:** CONFIRMED from source: Service (service.go:40+) carries stores/storeCache/lockManagers/unifiedViews/dirChangeNotifiers/pendingWrites/dirTimes/writebackShares/deferredCommit + parentLinkShards + createNameShards + cookies + quotas + identityQuotas + quotaGracePersist + removeGen + graceDuration + graceCoordinator + byteRangeReleaseHook; 126 `func (s *Service)` receivers across pkg/metadata. Orthogonal concerns (quota policy, lock-manager lifecycle, dir-change notify) share the struct that also holds write hot-path state — materially inflates blast radius. Contrasts with runtime's own sub-service split per CLAUDE.md.
- **Fix:** Extract QuotaManager (quota get/set/enforce methods + identity quota state) and a LockManagerRegistry (lockManagers/unifiedViews/grace-period wiring) into their own types that Service composes, mirroring runtime's sub-service split. Leaves Service holding store routing + pendingWrites/dirTimes (the actual write-path state).

### [MED] GETXATTR does a full paginated directory scan instead of an O(1) exact child lookup

- **Where:** `pkg/metadata/xattr.go:121` · `perf` · area: metadata-file-io-write
- **Verified:** CONFIRMED xattr.go:124-146: findStreamChild pages the ENTIRE parent directory via ListChildren(cursor, 0) filtering with streamNameFromChild+EqualFold; ResolveGetXattr:170 calls it before any exact lookup, so every GETXATTR on a file costs O(siblings) and multiple store round-trips. Files interface DOES expose the O(1) alternative (store.go:54 GetChild), so an exact-match fast path is available without widening any interface. REACHABLE non-test: xattr_service.go:38 (Service.GetXattr) plus all four backends' GetXattr (sqlite/xattr.go:16, postgres:16, badger:15, memory:19). Textbook O(n)-where-O(1)-exists on a per-file-access metadata op (SMB ADS probes, macOS xattr reads, NFSv4.2 GETXATTR).
- **Fix:** In ResolveGetXattr try files.GetChild(ctx, parent, streamChildPrefix(baseName)+name) first (exact, indexed); fall back to the findStreamChild scan only on not-found, to preserve case-insensitive matching (mirrors the Lookup/LookupCaseInsensitive fast-path-then-scan pattern).

### [MED] Single share-wide mutex serializes ALL lock ops on synchronous per-op store I/O

- **Where:** `pkg/metadata/lock/manager.go:702` · `perf` · area: metadata-lock-core-manager
> STATUS: OPEN in #2029 — validated and now measured; stays a redesign, but the blocker is narrower and the cost is bigger than stated. Verdict CONFIRMED on the serialization, PARTIAL on the reason. Measured (Apple M1 Max, RunParallel over DISTINCT handle keys, so zero logical conflict): with no lock store, Lock+Unlock is 547 ns/op at 1 core and 788 ns/op at 8 — negative scaling but sub-microsecond, ~1.3M ops/s, not a ceiling. With a lock store at 100 us/call: 265 us/op at 1 core, 258 us/op at 8. At 1 ms/call: 2.294 ms at 1 core, 2.296 ms at 8. Identical at every core count — the share does ~436 lock ops/s on a 1 ms-RTT store no matter how many clients or cores. So the throughput ceiling is the SYNCHRONOUS lockStore round-trip inside lm.mu.Lock (putLockLocked/deletePersistedLocked), not map contention, and it is on every byte-range lock/unlock AND every SMB lease grant/break/release; all four backends implement lock.LockStore, so it is on by default. The striping blocker is smaller than listed: of the named state, gracePeriod and recentlyBroken are never written after construction (recentlyBroken has its own mutex), breakWaitChans is keyed by handleKey at all 7 sites and would stripe cleanly, and epoch/lockStore/shareName/handleChecker/clientRecoveryStore/onByteRangeRelease/delegationRecallTimeout are each written exactly once at wiring. The genuinely manager-global mutable state is only leaseKeyIndex, clientHandleIndex and breakCallbacks. Cross-bucket traversals are 11, not ~12: 7 whole-map ranges and 4 reverse-index fan-outs — and 4 of the 7 were in Manager.GetStats, which had zero production callers and is now deleted, leaving 7. The remaining ones (RemoveClientLocks, ReleaseByOwnerPrefix, CollectExpiredDelegationRecalls, hasLeaseKeyOnOtherFile, clearBreakingSiblingsLocked, releaseLeaseImpl, SetLeaseEpoch) each need atomicity across N buckets plus the two indexes, which is the lock-ordering redesign #2017 declined. Moving the persist out of the critical section is NOT the shortcut: putLockLocked's contract is mutex order == store order, which is what eliminates the reorder/resurrection class.
- **Verified:** CONFIRMED: Manager has one sync.RWMutex for the whole share (L702); putLockLocked/deletePersistedLocked (L1276, L1296) make a synchronous lockStore round-trip bounded by persistTimeout=3s (L754) and their doc comments require the caller to hold lm.mu. So one file's store latency blocks every other file's lock/lease op in the share. REACHABLE: every NFS NLM lock and SMB lease grant/release. Severity MED not HIGH: the serialization is a deliberate ordering fix (comment cites the reorder/resurrection bug class it eliminates), persistence is best-effort with a 3s cap, and lock ops are far lower-rate than read/write — but under lease-heavy SMB workloads the share-wide queueing is a genuine throughput ceiling.
- **Fix:** Shard the critical section per handleKey (striped mutex) so lock ops on different files don't queue behind each other's store RTT. Do NOT simply move the persist outside mu — putLockLocked's contract (manager.go:1265-1276) is that mutex order == store order; any fix must preserve per-handle ordering (e.g. per-handle serialized queue).

### [MED] SetLeaseEpoch scans every handleKey bucket instead of using leaseKeyIndex

- **Where:** `pkg/metadata/lock/manager.go:1493` · `perf` · area: metadata-lock-core-manager
> STATUS: FIXED in #1935 — SetLeaseEpoch iterates leaseKeyIndex[leaseKey] and scans only those buckets, keeping the update-every-matching-record collection and two-pass max-epoch convergence. Equivalence test: TestSetLeaseEpoch_IndexMatchesFullScan.
- **Verified:** CONFIRMED: L1493-1503 ranges the whole lm.unifiedLocks map (every handleKey bucket, every lock) to collect leaseKey matches. leaseKeyIndex exists and — per its own doc — ref-counts EVERY bucket holding a key, so it is a complete candidate set and using it preserves the 'update every matching record' requirement that the comment justifies (findLeaseByKey's first-match-only is why the author avoided it, but the index itself is not first-match). REACHABLE from SMB CREATE: handlers/create.go:950 and handlers/lease_context.go:510 → smb/lease/manager.go:1219 → this. Cost is O(all locks in the share) per lease-granting/upgrading open. MED not HIGH: constant is small per record and lease opens are not the highest-rate op, but it degrades with total outstanding locks rather than with actual contention.
- **Fix:** Iterate lm.leaseKeyIndex[leaseKey] for candidate handleKeys and scan only lm.unifiedLocks[handleKey] in those buckets (same pattern as findLeaseByKey, leases.go:129-141), keeping the existing two-pass max-epoch convergence semantics.

### [MED] Manager is a god object spanning 8+ unrelated responsibilities

- **Where:** `pkg/metadata/lock/manager.go:701` · `structure` · area: metadata-lock-core-manager
> STATUS: PARTIAL #2017 — manager.go was split into byterange.go, break.go, connection.go, grace.go and delegation.go along the concerns its own section comments already named. The `Manager` *type* still carries every responsibility behind one mutex; #2017 declined the type split deliberately, and that residue is tracked as #2029.
- **Verified:** CONFIRMED. type Manager at manager.go:701 holds, under ONE lm.mu: locks (legacy byte-range), unifiedLocks, breakCallbacks, leaseKeyIndex/clientHandleIndex, gracePeriod, handleChecker, lockStore, clientRecoveryStore, epoch, recentlyBroken, breakWaitChans, delegationRecallTimeout, onByteRangeRelease. 121 *Manager methods across the package; manager.go alone is 131 KB and the concerns are split by filename only (leases.go 54 K, oplock.go, delegation.go, grace.go, reclaim.go, directory.go) with the single type reassembled across them. Production type (implements LockManager, one per share). Seed's method count (~95) understated; MED stands — no wrong behavior, but genuine cross-concern serialization and review burden.
- **Fix:** Extract cohesive sub-components with narrow ownership (ByteRangeLockTable, LeaseBreaker, DelegationTable, GracePeriodController), each with its own mutex, composed inside Manager which becomes a thin facade delegating to them.

### [MED] hasLeaseKeyOnOtherFile full-manager scan bypasses existing leaseKeyIndex

- **Where:** `pkg/metadata/lock/leases.go:162` · `perf` · area: metadata-lock-lease-oplock
> STATUS: FIXED in #1935 — hasLeaseKeyOnOtherFile ranges leaseKeyIndex[leaseKey], skips excludeHandleKey and checks Owner.ClientID per bucket. Equivalence test: TestHasLeaseKeyOnOtherFile_IndexMatchesFullScan.
- **Verified:** Confirmed: leases.go:162-179 ranges all of lm.unifiedLocks; called at leases.go:387 inside lm.mu.Lock() on every RequestLease with requestedState != None, i.e. every lease-granting SMB CREATE (non-test path via adapter lease manager). leaseKeyIndex (indexes.go) is derived state reconciled on every mutation and already used by findLeaseByKey/clearBreakingSiblingsLocked for exactly this lookup, so the bounded fix is available and semantically equivalent. Real, but per-entry work is a pointer deref + compare, so HIGH is overstated — MED (scales with total open-file lock records under the global write mutex).
- **Fix:** Use lm.leaseKeyIndex[leaseKey] to get the candidate handleKey buckets (same pattern as findLeaseByKey), skip excludeHandleKey, and scan only lm.unifiedLocks[handleKey] for those buckets checking Owner.ClientID — bounded by the (small) number of files actually holding that lease key instead of every file on the server.

### [MED] requestLeaseImplWithMode: 440-line god function, mixes 8+ concerns

- **Where:** `pkg/metadata/lock/leases.go:259` · `structure` · area: metadata-lock-lease-oplock
> STATUS: FIXED #2016 — `requestLeaseImplWithMode` is now `requestLeaseImpl` at 120 lines over named helpers.
- **Verified:** Confirmed: func at leases.go:259, next func at 723 => ~464 lines; signature carries 11 params incl 3 trailing bools (isDirectory, isTraditionalOplock, suppressConflictBreak). Reachable via lease request path (SMB create/lease). Pure structure/maintainability though — no wrong behavior demonstrated, so HIGH is overstated; MED given the area's break-ordering bug history (#1701/#1806).
- **Fix:** Extract named steps: normalizeRequest, checkCrossFileUniqueness, checkDelegationConflict, handleSameKeyLease, resolveCrossKeyConflict, grant. Replace trailing bools with a small LeaseRequest struct.

### [MED] acknowledgeLeaseBreakImpl: 200-line function mixing ack validation, tombstone/timeout classification, progressive multi-stage dispatch

- **Where:** `pkg/metadata/lock/leases.go:902` · `structure` · area: metadata-lock-lease-oplock
> STATUS: FIXED #2016 — `acknowledgeLeaseBreakImpl` is down to 86 lines; ack validation, classification and progressive dispatch are separate.
- **Verified:** CONFIRMED: func starts at 902 and closes at 1099 (~198 lines) under a single lm.mu held by defer, covering not-found, !Breaking late-ACK/BrokenViaTimeout tombstone classification, epoch validation, subset check, ack-to-None cleanup, and progressive-stage recompute with unlock/relock via closure. Reachable via the exported AcknowledgeLeaseBreak on the SMB lease-break path. MED stands: this is the exact seam that produced the #1701 double-break bug, so the density has demonstrated correctness cost — though it is still a refactor, not a live defect.
- **Fix:** Split into: classifyLateAck(lock) (handles not-breaking cases), applyAckToNone(lock), applyProgressiveAck(lock, acknowledgedState) — each independently testable.

### [MED] WaitForGraph exposes check-then-act pair; concurrent LOCK requests can build an undetected cycle

- **Where:** `pkg/metadata/lock/deadlock.go:60` · `structure` · area: metadata-lock-nlm-deadlock-grace
> STATUS: FIXED in #1935 — WouldCauseCycle + AddWaiter collapsed into TryAddWaiter under one write lock; the sole caller (smb/handlers/lock_async.go:55) uses it.
- **Verified:** Confirmed: WouldCauseCycle (deadlock.go:60, RLock/RUnlock) and AddWaiter (deadlock.go:82, separate Lock) are separate; real caller parkLockOnConflict does WouldCauseCycle at lock_async.go:52 then TryReserveAsync/generateAsyncId/context.WithTimeout/PendingLockRegistry.Register before AddWaiter at :106 — no lock held across the gap, so two concurrent parks can both pass the check and close a cycle. Graph built in production (handler.go:904 LockWaitGraph: lock.NewWaitForGraph()). Downgraded from HIGH: the missed cycle is not a hang — every parked lock is bounded by asyncBlockingLockTimeout (waitCtx at lock_async.go:73), so the undetected deadlock resolves as a late LOCK_NOT_GRANTED instead of a permanent wedge, and the window is narrow.
- **Fix:** Collapse into one atomic method, e.g. TryAddWaiter(waiter string, owners []string) bool that holds a single Lock for both the cycle check and the edge insert, returning false (no edges added) if it would cycle. Delete/deprecate the separate WouldCauseCycle+AddWaiter public pair or keep them only as building blocks under one lock.

### [MED] pendingWrites map is fully dead state

- **Where:** `pkg/metadata/store/memory/store.go:196` · `bloat` · area: metadata-memory-backend
> STATUS: FIXED in #1970 — pendingWrites had no producer or consumer; the field and its alloc/reset/snapshot plumbing are gone.
- **Verified:** CONFIRMED dead. Exhaustive grep of pkg/metadata/store/memory for `pendingWrites` yields only: doc comment store.go:108, decl store.go:189-196, alloc store.go:354, reset.go:34, and snapshot round-trip snapshot_store.go:124/337/368-369. No PrepareWrite/CommitWrite/read of the map anywhere in the package or its transaction (transaction.go has zero hits). Distinct from the live pkg/metadata/service.go pendingWrites *PendingWritesTracker (different type, real users). Reachable pkg: NewMemoryMetadataStoreWithDefaults called from pkg/controlplane/runtime/init.go:65 and stores/service.go:176. Fix: delete field + its alloc/reset/snapshot plumbing (and the Snapshot.PendingWrites gob field).
- **Fix:** Delete the field and its init/reset/snapshot plumbing, or implement PrepareWrite/CommitWrite if two-phase writes are actually needed.

### [MED] deviceNumbers map + deviceNumber struct are fully dead

- **Where:** `pkg/metadata/store/memory/store.go:187` · `bloat` · area: metadata-memory-backend
> STATUS: FIXED in #1970 — deviceNumbers had zero insertion sites; device major/minor ride on FileAttr.Rdev. Map, struct and plumbing removed, gob decode-compatibility verified both directions.
- **Verified:** CONFIRMED dead. `grep -rn 'deviceNumbers\[' pkg/` returns ZERO hits — no insertion exists anywhere; the only accesses are maps.Clone (transaction.go:98), snapshot restore (transaction.go:137, snapshot_store.go:336/365-366), two deletes (transaction.go:405, 761), alloc (store.go:353), reset (reset.go:33), gob field (snapshot_store.go:123). Since nothing ever writes a key, a restored snapshot can only ever be empty too, so the map is unreachable state by construction. deviceNumber struct (store.go:49-52) has no other user; device major/minor rides on metadata.File fields instead. Reachable pkg (init.go:65, stores/service.go:176). Fix: delete the map, the deviceNumber struct, the two deletes, the clone/restore lines and the Snapshot.DeviceNumbers field.
- **Fix:** Remove deviceNumber/deviceNumbers entirely, or wire it up in PutFile for FileTypeBlockDevice/FileTypeCharDevice if device files need it.

### [MED] SetFilesystemCapabilities duplicates the exact upsert query from initializeFilesystemCapabilities

- **Where:** `pkg/metadata/store/postgres/server.go:74` · `bloat` · area: metadata-postgres-support
> STATUS: FIXED in #1949 — one filesystem_capabilities upsert per backend
- **Verified:** CONFIRMED. server.go:74-98 and store.go:333-368 carry the same 15-column `INSERT INTO filesystem_capabilities (...) VALUES (1,$1..$14) ON CONFLICT (id) DO UPDATE SET ...` with the same 14-arg list; only the executor differs (s.exec + Warn-on-error vs pool.Exec + returned error). initializeFilesystemCapabilities is called from NewPostgresMetadataStore (production, mirrored by sqlite/store.go:140). Caveat on criterion 2: store-level SetFilesystemCapabilities currently has no non-test caller (only storetest/store_surface.go:580) but is a mandatory metadata.Store interface method (pkg/metadata/store.go:236) compiled into the binary, so it is not deletable dead code. Fix: one package-level const upsertCapabilitiesSQL + capsArgs(caps) helper used by both.
- **Fix:** Factor the query + arg-binding into one shared helper (e.g. upsertFilesystemCapabilities(ctx, execer, caps)) taking anything with Exec(ctx, sql, args...), called from both the constructor and SetFilesystemCapabilities.

### [MED] ListChildren duplicated near-verbatim between pool path and tx path

- **Where:** `pkg/metadata/store/postgres/transaction.go:624` · `bloat` · area: metadata-postgres-write
> STATUS: DEFERRED to #1828 — subsumed by the metadata-store unification (SQL family + KV family behind a shared base); not fixable as an isolated patch.
- **Verified:** CONFIRMED by direct diff. Diffed transaction.go:624-777 against files.go:200-376: only 57 diff lines, and every one is a comment-wording difference except two — the receiver/signature line and tx.tx.Query(ctx,...) vs s.query(ctx,...). Query text, scan loop, ACL hydration (the '#532' block appears in both), ObjectID sentinel handling, recycle-bin deleted_at decode and lenient ACL-unmarshal are byte-identical logic. Both are live interface methods (Transaction.ListChildren / MetadataStore.ListChildren). Fix: one helper taking the query executor, both call it.
- **Fix:** Extract the query+scan body into a shared helper taking a minimal query interface (QueryRow/Query — pgx.Tx and the pool wrapper already satisfy the same shape), called from both the pool method and the tx method.

### [MED] GetChild/GetParent/GetLinkCount/GetFilesystemMeta/GetFile duplicated pool-vs-tx

- **Where:** `pkg/metadata/store/postgres/transaction.go:529` · `bloat` · area: metadata-postgres-write
> STATUS: DEFERRED to #1828 — subsumed by the metadata-store unification (SQL family + KV family behind a shared base); not fixable as an isolated patch.
- **Verified:** CONFIRMED. Pairs verified byte-for-byte: GetChild tx transaction.go:529-561 vs files.go (`SELECT dc.child_id FROM parent_child_map dc WHERE dc.parent_id=$1 AND dc.child_name=$2`, only tx.tx.QueryRow vs s.queryRow + an extra Debug log differ); GetParent (`SELECT parent_id FROM parent_child_map WHERE child_id=$1 LIMIT 1`) identical; GetLinkCount (`SELECT nlink FROM inodes WHERE id=$1`, same swallow-error->0 behavior) identical; GetFilesystemMeta (`SELECT meta FROM filesystem_meta WHERE share_name=$1`, same default-on-miss with tx.store.capabilities vs s.capabilities) identical; GetFile identical 24-column SELECT incl. inodePathExpr + blockRefsAggExpr + FileRowToFileWithNlinkAndBlocks. Reachable: NewPostgresMetadataStore called at pkg/controlplane/runtime/init.go:168. Fix: hoist each query to a package-level const and share a scan helper over a pgx Row-source (queryRow func or a rowQuerier iface the pool+tx both satisfy) — files.go methods become one-liners.
- **Fix:** Same fix as ListChildren: one implementation per operation parameterized over pgx.Row/pgx.Rows-returning interface, called from both PostgresMetadataStore and postgresTransaction.

### [MED] QueryTimeout config field is parsed and defaulted but never applied to any query

- **Where:** `pkg/metadata/store/sqlite/config.go:24` · `bloat` · area: metadata-sqlite-support
> STATUS: FIXED in #1949 — the unread QueryTimeout knob is gone
- **Verified:** CONFIRMED. grep QueryTimeout in pkg/metadata/store/sqlite/ returns exactly 4 hits: config.go:24 (doc 'bounds an individual statement'), :25 (field), :41-42 (default 30s). Never in DSN() pragmas, never a context.WithTimeout, never touched by pool_helpers.go. Contrast postgres/connection.go:35-36 which maps it to statement_timeout. Config is operator-facing and live-constructed in prod (runtime/init.go:183, runtime/stores/service.go:218) via mapstructure key query_timeout — operator sets it, silently gets nothing. Fix: apply it (context.WithTimeout in query/queryRow/exec) or delete the field.
- **Fix:** Either wire QueryTimeout into a per-statement context.WithTimeout in pool_helpers.go's execer (query/exec paths), or delete the field and its mapstructure tag/doc comment from SQLiteMetadataStoreConfig.

### [MED] filesystem_capabilities upsert SQL duplicated verbatim between store.go and server.go

- **Where:** `pkg/metadata/store/sqlite/store.go:304` · `bloat` · area: metadata-sqlite-support
> STATUS: FIXED in #1949
- **Verified:** CONFIRMED. store.go:304-359 initializeFilesystemCapabilities and server.go:79-133 SetFilesystemCapabilities issue the same 14-column INSERT INTO filesystem_capabilities ... ON CONFLICT(id) DO UPDATE with the same 14-column excluded-list; differ only in ? vs ?1 placeholders and db.ExecContext vs s.exec. Both prod: store.go:140 calls the free function from NewSQLiteMetadataStore; SetFilesystemCapabilities is an interface method (mirrored again at transaction.go:1285). Column change must be made twice. Fix: keep one statement (const) and have startup call it.
- **Fix:** Construct the SQLiteMetadataStore struct earlier in NewSQLiteMetadataStore (before initUsedBytesCounter) and call store.SetFilesystemCapabilities(capabilities) instead of the separate initializeFilesystemCapabilities(ctx, db, capabilities) free function; delete the duplicate.

### [MED] GetLinkCount silently converts any query error into "link count 0" instead of propagating it

- **Where:** `pkg/metadata/store/sqlite/transaction.go:764` · `gaps` · area: metadata-sqlite-write
> STATUS: FIXED in #1901
- **Verified:** CONFIRMED sqlite/transaction.go:764-768 `err = tx.tx.QueryRow(...).Scan(&count); if err != nil { return 0, nil }` — catch-all, comment only true for ErrNoRows. Identical in sqlite/files.go:183 AND postgres/files.go:~183 + postgres/transaction.go:809. Reachable: file_remove.go:171 (tx.GetLinkCount, lcErr fallback now dead), file_modify.go:834/884/894 whose own comments say 'a failed GetLinkCount must roll the tx back' — that contract is unenforceable on sqlite/postgres. badger/files.go:441 propagates (correct reference). Severity lowered HIGH->MED: a broken conn also fails the subsequent tx.SetLinkCount/DeleteChild/PutFile in the same tx, so the whole tx rolls back; silent content-free requires a read-only transient error, and the common ErrNoRows->0 case is intended. Fix: `if errors.Is(err, sql.ErrNoRows) { return 0, nil }; return 0, mapDBError(err, "GetLinkCount", "")` in all four spots.
- **Fix:** In both files.go and transaction.go, discriminate: `if errors.Is(err, sql.ErrNoRows) { return 0, nil }; if err != nil { return 0, mapDBError(err, "GetLinkCount", "") }`. Mirror the badger/postgres-correct pattern already used elsewhere in this file (e.g. ApplyDataWrite in data_write.go:39-44 does the errors.Is(sql.ErrNoRows) split correctly) so a real DB error surfaces and the file_remove.go/file_create.go defensive fallbacks actually get to run.

### [MED] Three copy-pasted generation-guarded cache implementations instead of one generic type

- **Where:** `pkg/metadata/store/badger/read_cache.go:31` · `structure` · area: metadata-store-badger-cache-index
> STATUS: DEFERRED to #1828 — subsumed by the metadata-store unification (SQL family + KV family behind a shared base); not fixable as an isolated patch.
- **Verified:** CONFIRMED. fileReadCache (read_cache.go:31-110) and direntCache (create_cache.go:42-124) are field-for-field identical (m/n/gen/prune) with identical generation()/get()/store() gen-guard + CompareAndDelete-on-race /invalidate() bump-then-delete, and pruneToHalf (95-110 vs 109-124) differs only in the cap const name. shareReadCache (share_read_cache.go:30-75) repeats the same gen/store/invalidate discipline minus cap. Three hand-maintained copies of a concurrency-critical invalidation invariant whose own comments call it correctness/permission-critical; a fix applied to one and missed in another reintroduces a stale hit. Go 1.25 generics + comparable V works for *metadata.File, direntEntry (deliberately made comparable, see create_cache.go:51), *ShareOptions. Reachable: all three on the badger read/create hot paths.
- **Fix:** Collapse to one genCache[V comparable] {m sync.Map; n atomic.Int64; gen atomic.Uint64; prune atomic.Bool; cap int64} with get/store/invalidate/pruneToHalf written once; cap=0 = unbounded (shareReadCache). fileReadCache/direntCache/shareReadCache become thin wrappers only where key shape (direntKey) or clone-on-read (cloneShareOptions) differs.

### [MED] FileID durable-handle index is a single-value key, not scoped by handle ID -- deleting one handle can orphan a different live handle for the same file

- **Where:** `pkg/metadata/store/badger/durable_handles.go:133` · `bugs` · area: metadata-store-badger-support
> STATUS: TRACKED as #1915
- **Verified:** Confirmed: prefixDHFileID = "dh:fid:" (line 27); putDurableHandleTx:99 writes fidKey unscoped by ID (unlike appid/fh/share which append ":"+handle.ID); deleteIndicesTx:134 blind-Deletes the same recomputed key. Two persisted durable handles on the same FileID (FileID is stored with volatile half zeroed per durable_context.go:1077, i.e. per-file not per-open) collide; deleting the superseded one wipes the live one's index while its dh:id record survives. Reachable: internal/adapter/smb/handlers/durable_context.go:516/552/614/683 call GetDurableHandleByFileID / ConsumeDurableHandleByFileID on the DHnC/DH2C reconnect path; delete path via DeleteDurableHandle / DeleteExpiredDurableHandles. Downgraded HIGH->MED: failure mode is a failed durable reconnect (client falls back to a fresh open), not data loss.
- **Fix:** Either scope prefixDHFileID as `dh:fid:{hex}:{id}` and scan-and-pick in Get/ConsumeDurableHandleByFileID (mirroring getHandlesByPrefix), or minimally in deleteIndicesTx re-Get fidKey and only Delete when its value == handle.ID.

### [MED] Share-lifecycle writes bypass the store's own SSI-conflict retry contract; comment claims a retry that doesn't exist

- **Where:** `pkg/metadata/store/badger/shares.go:194` · `slop` · area: metadata-store-badger-write-txn
> STATUS: FIXED in #1972 — CreateShare, UpdateShareOptions, DeleteShare and CreateRootDirectory routed through updateWithConflictRetry, so ErrConflict no longer escapes unmapped. Worst on DeleteShare, which reads every f: row into the read set.
- **Verified:** CONFIRMED: shares.go uses raw `s.db.Update(` at lines 113 (CreateShare), 151 (UpdateShareOptions), 194 (DeleteShare), 436 (CreateRootDirectory) — grep shows updateWithConflictRetry is referenced only from objects.go:157/182/210/245/301 and block_record_store.go:280, never from shares.go; withTransaction's retry loop (transaction.go:110-116, maxTransactionRetries) is likewise not on this path. So a single badgerdb.ErrConflict aborts the op and escapes unmapped (mapBadgerError is applied only to the inner txn.Get). deleteShareFiles' doc comment asserting the 'pool-path applies it after db.Update returns nil — so conflict/serialization retry re-runs the enclosing Update' describes a retry that does not exist on DeleteShare. REACHABLE via the runtime AddShare/RemoveShare/UpdateShareOptions control-plane paths. Downgraded HIGH→MED: share lifecycle ops are rare admin operations with low SSI-conflict probability; blast radius is a spurious error return + unwrapped sentinel, not data loss.
- **Fix:** Route CreateShare, UpdateShareOptions, DeleteShare and CreateRootDirectory through s.updateWithConflictRetry(ctx, fn) (objects.go:157) instead of a bare s.db.Update(fn), and fix or delete deleteShareFiles' false retry claim.

### [MED] Share CRUD duplicated between pool-path (no retry) and transaction-path (retried), already caused one bug that had to be fixed twice

- **Where:** `pkg/metadata/store/badger/shares.go:108` · `structure` · area: metadata-store-badger-write-txn
> STATUS: DEFERRED to #1828 — subsumed by the metadata-store unification (SQL family + KV family behind a shared base); not fixable as an isolated patch.
- **Verified:** CONFIRMED: shares.go:108-212 (CreateShare/UpdateShareOptions/DeleteShare) call s.db.Update(...) directly with ZERO conflict retry, and duplicate badgerTransaction.CreateShare/UpdateShareOptions/DeleteShare (transaction.go:1045-1148) body-for-body (same keyShare/shareData/encode logic, same StoreError). DeleteShare drives deleteShareFiles which reads every f: row into the txn read set — the highest-conflict-probability op, unretried, and a raw badgerdb.ErrConflict escapes to the caller unmapped. Reachable from dfsctl share create/delete via runtime. MED not HIGH: needs a concurrent write to the same share to bite.
- **Fix:** Make BadgerMetadataStore.CreateShare/UpdateShareOptions/DeleteShare thin wrappers over s.WithTransaction delegating to the badgerTransaction implementations, eliminating the second copy and giving pool-path share mutations the same bounded retry and StoreError wrapping.

### [MED] Reset() leaves per-identity quota cache stale — diverges from sqlite's Reset contract

- **Where:** `pkg/metadata/store/memory/reset.go:20` · `bugs` · area: metadata-store-memory
> STATUS: FIXED in #1949
- **Verified:** Confirmed: memory/reset.go zeroes s.usedBytes (line 44) and every map but never touches s.quota; sqlite/reset.go:47-49 does `s.quotaMu.Lock(); s.quota.Reset(); s.quotaMu.Unlock()`. Memory store has a real quota.Cache (store.go:253) read by GetQuotaUsage (store.go:445-449). Reachable from prod: pkg/controlplane/runtime/snapshot.go:1442 type-asserts metadata.Resetable and calls resetable.Reset(ctx) at :1549 on the snapshot-restore path, reached from internal/controlplane/api/handlers/snapshot.go. MED not HIGH: memory backend, quota reporting/enforcement skew only.
- **Fix:** Add `s.quotaMu.Lock(); s.quota.Reset(); s.quotaMu.Unlock()` inside Reset(), mirroring sqlite/reset.go:47-49.

### [MED] RestoreSnapshot() recomputes usedBytes but never reseeds the per-identity quota cache

- **Where:** `pkg/metadata/store/memory/snapshot_store.go:412` · `bugs` · area: metadata-store-memory
> STATUS: FIXED in #1949
- **Verified:** Confirmed: memory/snapshot_store.go:411-419 recomputes only s.usedBytes; no s.quota touch. sqlite/snapshot_store.go:296 calls initUsedBytesCounter, which (store.go:184-203) both stores usedBytes AND s.quota.Seed(userUsage, groupUsage). Reachable: runtime/snapshot.go:1567 snapshotable.RestoreSnapshot on the API restore path. Combined with idx 2 the cache keeps pre-restore values (the seed's 'reads all zeros' detail is off, but the missing reseed is real). MED: quota skew after restore, memory backend only.
- **Fix:** While summing totalBytes over s.files, also accumulate per-uid/per-gid UsageStat maps and call s.quotaMu.Lock(); s.quota.Seed(userUsage, groupUsage); s.quotaMu.Unlock(), matching sqlite's post-restore initUsedBytesCounter.

### [MED] PrepareStatements config flag is declared and logged but never wired into the pgxpool connection config

- **Where:** `pkg/metadata/store/postgres/connection.go:27` · `perf` · area: metadata-store-postgres-support
> STATUS: FIXED in #1949 — renamed DisablePreparedStatements and wired into the pool config
- **Verified:** CONFIRMED: createConnectionPool (connection.go:12-68) sets MaxConns/MinConns/lifetimes/statement_timeout only; no DefaultQueryExecMode, no StatementCacheCapacity, no reference to cfg.PrepareStatements anywhere in the pool build. Only other use is the log line at store.go:170 ("prepare_statements", cfg.PrepareStatements). Field is declared config.go:32 with documented default true. Reachable: createConnectionPool is the sole pool constructor for the postgres store. Dead knob — setting prepare_statements=false (the PgBouncer transaction-pooling escape hatch) has zero effect.
- **Fix:** Wire PrepareStatements into poolConfig.ConnConfig.DefaultQueryExecMode (pgx.QueryExecModeCacheStatement when true, QueryExecModeSimpleProtocol/DescribeExec when false) in createConnectionPool.

### [MED] durable_handles.go / clients.go / client_recovery.go bypass the pool acquire-timeout wrapper pool_helpers.go exists specifically to enforce

- **Where:** `pkg/metadata/store/postgres/durable_handles.go:236` · `structure` · area: metadata-store-postgres-support
> STATUS: FIXED in #1949
- **Verified:** CONFIRMED: postgresDurableStore{pool *pgxpool.Pool} (durable_handles.go:14-20), postgresClientStore (clients.go:28-34), postgresRecoveryStore (client_recovery.go:23-29) each hold the raw pool; direct s.pool.Exec/Query/QueryRow counts 12 / 6 / 4, plus schema_ops.go:60,112. All three are built from the live store (durable_handles.go:380, clients.go:199, client_recovery.go:152), so non-test reachable. pool_helpers.go:20-32 states verbatim that pgxpool has no built-in acquire timeout and 'without these helpers, any pool operation can hang forever under high concurrent load'; poolConnectionAcquireTimeout=10s never applies on these paths. Genuine fail-fast gap on SMB durable-handle reconnect / NFSv4 client recovery.
- **Fix:** Route postgresDurableStore, postgresClientStore, postgresRecoveryStore through PostgresMetadataStore's query/exec/queryRow helpers instead of holding a raw *pgxpool.Pool, or give them a small execer interface implemented by a thin wrapper that applies the same acquire-timeout. Do the same for schema_ops.go's ListSchemasByPrefix/DropSchema.

### [MED] fileChunkRefsDelta re-reads and diffs the entire stored manifest on every BlocksDirty PutFile, cost scales with total file chunk count not with the changed range

- **Where:** `pkg/metadata/store/postgres/file_block_refs.go:98` · `perf` · area: metadata-store-postgres-write
> STATUS: FIXED in #1949 — BlocksDirtyOffsets scopes the manifest diff to the touched range
- **Verified:** Confirmed: fileChunkRefsDelta (98+) does `SELECT "offset", size, hash FROM file_block_refs WHERE file_id = $1` into a map whenever hasPriorRefs, then builds a second O(N) incoming map to compute upserts/deletes. Reachable: transaction.go:465 `putFileChunkRefs(ctx, tx.tx, file.ID, file.Blocks, updated)` runs on every PutFile with BlocksDirty (i.e. every carve/commit), and file.Blocks is the whole projected manifest. So each commit re-reads and re-compares all N stored rows regardless of how few offsets changed — O(N) per write, ~O(N^2) over a large file's lifetime (a multi-GB file at 128KiB chunks is 10k+ rows re-read per commit, plus the hash bytes over the wire). Kept MED: unbounded growth with file size on the write path, not a micro-opt.
- **Fix:** Carry the changed-offset set (or a modified-range hint) from where the manifest projection is built into PutFile so putFileChunkRefs can target its UPSERT/DELETE without a full-table diff read per commit.

### [MED] CreateRootDirectory bypasses the package's own WithTransaction convention, losing conflict retry

- **Where:** `pkg/metadata/store/postgres/shares.go:269` · `structure` · area: metadata-store-postgres-write
> STATUS: FIXED in #1949 — runs through WithTransaction
- **Verified:** CONFIRMED: the needsUpdate branch issues a bare pool s.exec(ctx, updateQuery,...) (shares.go:269-285) with no transaction; the create branch hand-rolls s.beginTx / defer Rollback / tx.Commit (shares.go:299-368). Neither goes through s.WithTransaction, whose retry loop on 40001/40P01 exists at transaction.go:52-59/88/148/163 and is used by every sibling write. So a transient serialization failure/deadlock fails the call instead of retrying. Reachable from prod: shares/service.go:903 calls metadataStore.CreateRootDirectory on share create/bootstrap. MED not HIGH: this path runs at share creation, so real conflicts are rare.
- **Fix:** Route both branches through s.WithTransaction(ctx, func(tx metadata.Transaction) error {...}) like every other mutation in the package, or wrap the bespoke tx with the same isRetryableError/backoff loop transaction.go already provides.

### [MED] ~180 lines of config-reconciliation business logic duplicated verbatim between postgres and sqlite backends

- **Where:** `pkg/metadata/store/postgres/shares.go:212` · `structure` · area: metadata-store-postgres-write
> STATUS: DEFERRED to #1828 — subsumed by the metadata-store unification (SQL family + KV family behind a shared base); not fixable as an isolated patch.
- **Verified:** CONFIRMED by direct diff of postgres/shares.go:200-400 vs sqlite/shares.go:200-400: the entire CreateRootDirectory body — idempotency check, mode/uid/gid needsUpdate diff with identical log lines, insert, and metadata.File construction — is identical apart from receiver type, $n vs ?n placeholders, mapPgError vs mapDBError, and the tx begin/rollback idiom. Real business logic (config reconciliation), not boilerplate, maintained in two places. MED not HIGH: drift risk, no current behavioral divergence; matches the repo's own sqlite/postgres unification target.
- **Fix:** Hoist the idempotency check + per-field diff + File construction into a shared SQL-family/dialect helper, leaving only the parameterized INSERT/UPDATE SQL per backend.

### [MED] PutFile costs 2 round-trips for every file CREATE (no-op UPDATE, then INSERT)

- **Where:** `pkg/metadata/store/postgres/transaction.go:262` · `perf` · area: metadata-store-postgres-write
> STATUS: FIXED in #1949 — the create path carries NewInode so PutFile skips the probe that can only miss
- **Verified:** Confirmed: updateQuery (CTE `old` + FOR UPDATE + UPDATE ... RETURNING old.size) issued unconditionally at 262; `if !updated` INSERT fallback at ~409-425. Reachable — pkg/metadata/file_create.go:188/446 call tx.PutFile for freshly created inodes, so the UPDATE matches zero rows on every CREATE/MKDIR/symlink by construction. That is a guaranteed extra client-server round-trip (not pipelined; pgx Exec then Exec) inside the txn on the create path, which is the documented write wall for this tree. Kept MED: real per-create latency in the 0.1-1ms range over a network link, though the seed's one-statement fix is not directly implementable on current PG.
- **Fix:** Pass an is-new hint from the create path (file_create.go) so PutFile skips the guaranteed-miss UPDATE and goes straight to INSERT. Note: the seed's 'INSERT ... ON CONFLICT DO UPDATE RETURNING old.*' shape does not work pre-PG18 (OLD in RETURNING is PG18+), and the CTE exists precisely to return the pre-update size/uid/gid under FOR UPDATE — so collapsing to one statement requires either the is-new hint or keeping the CTE for the update case only.

### [MED] backupTables omits block_records — snapshot/restore/reset silently diverge from postgres backend and drop block-record state

- **Where:** `pkg/metadata/store/sqlite/snapshot_store.go:36` · `slop` · area: metadata-store-sqlite-support
> STATUS: FIXED in #1949 — block_records is in backupTables, pinned by conformance, sqlite snapshot schema v5
- **Verified:** CONFIRMED: sqlite backupTables (snapshot_store.go:35-50) lists 14 tables — inodes, shares, filesystem_meta, parent_child_map, file_blocks, file_block_refs, locks, nsm_client_registrations, durable_handles, v4_client_recovery, synced_hashes, server_config, server_epoch, filesystem_capabilities — and does NOT include block_records. Postgres's equivalent (postgres/snapshot_store.go:58-73) ends with "block_records". The table is real and live on sqlite: pkg/metadata/store/sqlite/block_record_store.go issues INSERT/SELECT/DELETE/walk against block_records (lines 41/63/77/90/143). reset.go:13 confirms Reset reuses the same backupTables slice, so Reset also skips it despite its 'empty every metadata table' contract, and RestoreSnapshot's truncate leaves stale rows behind. REACHABLE via dfsctl snapshot/restore/reset. Downgraded HIGH→MED: admin-triggered path, and block_records is partly rebuildable by the reconcile/reclaim walk — but stale live_chunk_count after restore is a genuine GC hazard.
- **Fix:** Add "block_records" to backupTables in pkg/metadata/store/sqlite/snapshot_store.go (FK-safe position, matching postgres), bump sqliteSchemaVersion with a comment documenting the addition, and add a WriteSnapshot/RestoreSnapshot round-trip test asserting block_records rows survive.

### [MED] objects.go duplicates FileChunkStore CRUD SQL between pool and tx paths

- **Where:** `pkg/metadata/store/sqlite/objects.go:73` · `structure` · area: metadata-store-sqlite-write
> STATUS: FIXED in #1949
- **Verified:** CONFIRMED. Pool Put(73-111)/Delete/Increment/Decrement/AddRef(229-251)/GetByHash(258-271) vs tx copies (Put~545, Delete, Increment, AddRef 637-656, GetByHash 658-673) carry byte-identical query literals ('UPDATE file_blocks SET ref_count = ref_count + 1 WHERE hash = ?1 AND state = 2', same 8-col INSERT..ON CONFLICT). Both surfaces already funnel through the same `execer` shim (pool_helpers.go), and decrementAndReapTx(ctx, execer, id) proves the extraction works in-file (called with execer{e:rawTx} at 164 and tx.tx at 631). ~100 lines duplicated on the chunk ref-count write path. Reachable via block/engine coordinator.
- **Fix:** Extract execer-parameterized bodies (putFileChunkTx, deleteFileChunkTx, incrementRefCountTx, decrementRefCountTx, addRefTx, getByHashTx) alongside the existing decrementAndReapTx, and have both *SQLiteMetadataStore and *sqliteTransaction call them.

### [MED] CreateRootDirectory and DecrementRefCountAndReap bypass the package's own busy/backoff retry wrapper

- **Where:** `pkg/metadata/store/sqlite/shares.go:300` · `structure` · area: metadata-store-sqlite-write
> STATUS: FIXED in #1949 — runs through WithTransaction
- **Verified:** CONFIRMED. shares.go:300 `rawTx, err := s.db.BeginTx(ctx, nil)` + hand-rolled commit/rollback + execer{e:rawTx,op:"CreateRootDirectory"}; objects.go:164 same shape for DecrementRefCountAndReap. Neither touches txretry.Deadline/Backoff, which transaction.go:59-133 implements as the documented package-wide backpressure policy (comment at 21-26). Both reachable non-test: CreateRootDirectory <- pkg/controlplane/runtime/shares/service.go:903; DecrementRefCountAndReap <- pkg/block/engine/readwrite.go:185/280/414 and runtime/blockgc_reconcile.go:168. Mitigating: DSN sets _pragma=busy_timeout (config.go:98, default 5s) and both txns open with a write as first statement, so plain BUSY is driver-absorbed — the gap is only when busy_timeout expires, where every other mutation would keep retrying to the txretry budget and these return EIO. Real inconsistency, MED not HIGH.
- **Fix:** Rewrite both to run inside s.WithTransaction(ctx, func(tx metadata.Transaction) error {...}), calling the existing tx-level counterparts (tx.CreateRootDirectory, decrementAndReapTx via tx.tx) instead of opening a raw *sql.Tx.

### [MED] Move() has no in-transaction TOCTOU recheck of source/destination entries

- **Where:** `pkg/metadata/file_modify.go:657` · `gaps` · area: metadata-write-coordination
> STATUS: FIXED in #1972 — both names were resolved only outside the tx and no lock covers a rename, so two renames onto the same destination both SetChild and orphan an inode. Both edges are re-resolved with tx.GetChild inside the tx, which also enters the child keys in the read set so badger SSI can see the conflict.
- **Verified:** CONFIRMED. Move reads srcHandle/srcFile (file_modify.go:657-664) and dstHandle/dstFile (699-705) via store.GetChild/GetFile outside the tx; the withRelaxedTransaction body (801-900+) re-reads only the two DIRECTORY inodes (tx.GetFile) and then acts on the stale dstFile/srcHandle — no tx.GetChild(fromDir,fromName) or tx.GetChild(toDir,toName) recheck anywhere. Contrast CreateHardLink (file_create.go:163-171: 'Re-check existence inside the transaction to close the TOCTOU race ... same pattern as createEntry') and createEntry (file_create.go:264-268 'OUTER check advisory only'). No lock covers it either: lockCreateName is used only by the create paths (file_create.go:148,431) and lockParentLinks (file_modify.go:792) only fires for cross-parent DIRECTORY renames, so concurrent file renames onto the same dest both SetChild → last writer wins, first inode orphaned, and badger SSI can't see the conflict because the child key was never read through the tx. Reachable: NFSv3 rename.go:258, NFSv4 rename.go:152, SMB set_info.go:746/1116. Fix: re-read both entries with tx.GetChild inside the tx and re-run the type/not-empty/dst checks against that.
- **Fix:** Inside the withRelaxedTransaction closure, re-resolve srcHandle via tx.GetChild(fromDir, fromName) and dstHandle via tx.GetChild(toDir, toName), and abort (return the appropriate StoreError) if either no longer matches what was read outside the transaction, mirroring the recheck already done in createEntry/CreateHardLink.

### [MED] write_codec.go reimplements xdr.EncodeWccData/encodeWccAttr inline instead of reusing it

- **Where:** `internal/adapter/nfs/v3/handlers/write_codec.go:192` · `bloat` · area: nfsv3-read-write-commit
> STATUS: FIXED in #1939 — WriteResponse.Encode calls xdr.EncodeWccData. Byte-identity with the old inline routine was checked against all six pre-op/post-op/status combinations before the claim was accepted.
- **Verified:** CONFIRMED. write_codec.go:196-232 hand-writes the pre-op present flag + Size/Mtime.Seconds/Mtime.Nseconds/Ctime.Seconds/Ctime.Nseconds (else branch writes uint32(0)), then calls xdr.EncodeOptionalFileAttr for post-op at :231 — which is exactly xdr.EncodeWccData (internal/adapter/nfs/xdr/encode.go:113-135: present flag -> encodeWccAttr(:157) -> EncodeOptionalFileAttr). Byte-identical output. Every other mutating v3 codec already reuses it: commit_codec.go:125, setattr_codec.go:150, create_codec.go:134, mkdir_codec.go:109, rmdir_codec.go:133, remove_codec.go:125, rename_codec.go:146/152, link_codec.go:141, symlink_codec.go:182, mknod_codec.go:182 — WRITE is the lone holdout. REACHABLE: WriteResponse.Encode is the live NFSv3 WRITE reply encoder. Fix: replace :195-233 with `xdr.EncodeWccData(&buf, resp.AttrBefore, resp.AttrAfter)`.
- **Fix:** Replace write_codec.go lines 192-233 with a single `if err := xdr.EncodeWccData(&buf, resp.AttrBefore, resp.AttrAfter); err != nil { return nil, fmt.Errorf("failed to encode wcc data: %w", err) }`, matching commit_codec.go.

### [MED] READ on non-regular, non-directory filehandle (symlink/device/socket/FIFO) wrongly returns NFS4ERR_ISDIR instead of NFS4ERR_INVAL

- **Where:** `internal/adapter/nfs/v4/handlers/read.go:146` · `gaps` · area: nfsv4-read-write-commit-readplus
> STATUS: FIXED in #1939 — readTypeError returns SYMLINK for links and INVAL for the remaining types
- **Verified:** CONFIRMED: internal/adapter/nfs/v4/handlers/read.go:146-152 returns NFS4ERR_ISDIR for ANY file.Type != FileTypeRegular (FileTypeSymlink/Block/Char/Socket/FIFO all exist in pkg/metadata/file_types.go). REACHABLE: handleRead registered at register_v40.go:48 (h.v40DispatchTable[types.OP_READ]); special stateids are accepted (read.go:70-74, openState==nil bypasses share-access), so a plain PUTFH/LOOKUP(symlink)+READ with the anonymous stateid reaches line 146. NOT spec-correct: RFC 7530 §16.23.4 — dir → NFS4ERR_ISDIR, symlink → NFS4ERR_SYMLINK, otherwise → NFS4ERR_INVAL (RFC 5661 §18.22.3 uses WRONG_TYPE for the 'otherwise' case). No existing test pins the wrong mapping: io_test.go:900/917 only assert ISDIR for a directory and the pseudo-fs. Fix: switch on file.Type — Directory→ISDIR, Symlink→NFS4ERR_SYMLINK, default→NFS4ERR_INVAL.
- **Fix:** Split the check: `if file.Type == metadata.FileTypeDirectory { return NFS4ERR_ISDIR }` then `else if file.Type != metadata.FileTypeRegular { return NFS4ERR_INVAL }` (mirror pkg/metadata/io.go's PrepareWrite logic).

### [MED] READ_PLUS same ISDIR-for-everything bug as READ

- **Where:** `internal/adapter/nfs/v4/handlers/read_plus.go:95` · `gaps` · area: nfsv4-read-write-commit-readplus
> STATUS: FIXED in #1939 — shares readTypeError with READ
- **Verified:** CONFIRMED: read_plus.go:95-97 identical collapse (file.Type != FileTypeRegular → readPlusErr(NFS4ERR_ISDIR)). REACHABLE: handleReadPlus registered at register_v42.go:19 (h.v42DispatchTable[types.OP_READ_PLUS]); same special-stateid bypass at read_plus.go:74-79. Same RFC 7530 §16.23.4 / RFC 7862 §15.10 violation as idx 7 (symlink→NFS4ERR_SYMLINK, other non-regular→INVAL/WRONG_TYPE). No conformance test pins ISDIR for non-dir types. Fix both call sites with one shared helper.
- **Fix:** Same split as read.go: NFS4ERR_ISDIR only for metadata.FileTypeDirectory, NFS4ERR_INVAL for every other non-regular type.

### [MED] UpdateAdapter swallows startAdapter failure after persisting new config

- **Where:** `pkg/controlplane/runtime/adapters/service.go:152` · `bugs` · area: runtime-adapters
> STATUS: FIXED in #1943
- **Verified:** Confirmed at service.go:152-159: after `_ = s.stopAdapter(cfg.Type)`, a startAdapter failure is only logger.Warn'd and UpdateAdapter returns nil, with the new Enabled config already committed at :138. Inconsistent with CreateAdapter (:96-109, rolls back and returns the error) and EnableAdapter (:213-215, returns it). Reachable: runtime.go:339 -> internal/controlplane/api/handlers/adapters.go:275 -> dfsctl adapter edit/enable/disable. Result: API 200 while the adapter is down and s.entries has no entry. MED not HIGH: control-plane state divergence, self-heals on restart, no data path impact.
- **Fix:** Return the error instead of only logging: `if err := s.startAdapter(cfg); err != nil { return fmt.Errorf("failed to restart adapter after update: %w", err) }`.

### [MED] stopAdapter removes the map entry before the adapter is confirmed stopped

- **Where:** `pkg/controlplane/runtime/adapters/service.go:263` · `bugs` · area: runtime-adapters
> STATUS: FIXED in #1943 — the entry is held until Stop returns
- **Verified:** Code confirmed verbatim: line 263 `delete(s.entries, adapterType)` runs under the lock BEFORE adapter.Stop/cancel/errCh wait; the ctx.Done() branch returns an error with the entry already gone. Reachable from 4 non-test callers (service.go:116 DeleteAdapter, :152 UpdateAdapter, :226 DisableAdapter, :296 StopAllAdapters). startAdapter's only duplicate guard is `s.entries[cfg.Type]` (line 239), so post-timeout a re-enable spawns a second Serve goroutine on the same port. Downgraded HIGH->MED: only reachable when Stop exceeds shutdownTimeout, i.e. a wedged adapter, not a normal path.
- **Fix:** Only delete(s.entries, adapterType) after the success branch (`<-entry.errCh`); on the timeout branch, leave the entry in the map (or mark it as still-stopping) so a subsequent start is rejected instead of racing a still-live adapter.

### [MED] Client share-tracking API built but never wired from either production registration path

- **Where:** `pkg/controlplane/runtime/clients/service.go:140` · `bloat` · area: runtime-identity-clients
> STATUS: FIXED in #1943 — the unwired share-tracking API is gone
- **Verified:** CONFIRMED and worse than bloat: Registry.AddShare (clients/service.go:140) / RemoveShare (:155) have zero non-test callers; both production Register sites — pkg/adapter/nfs/connection.go (ClientRecord{ClientID,Protocol,Address,NFS}) and pkg/adapter/smb/connection.go (…,SMB) — omit Shares entirely, and nothing else writes e.Shares. So ClientRecord.Shares is always nil, and the LIVE API path internal/controlplane/api/handlers/clients.go:51 registry.ListByShare(share) always returns [] for any ?share= query. Reachable=true (user-visible endpoint silently empty), so this is a functional gap, not pure dead weight.
- **Fix:** Either wire AddShare/RemoveShare into the mount/share-attach path (e.g. call from wherever a client's export/share association is established) or delete Shares/AddShare/RemoveShare/ListByShare + the API share filter.

### [MED] ClientRecord/NfsDetails/SmbDetails fields declared but never populated by any production caller

- **Where:** `pkg/controlplane/runtime/clients/service.go:22` · `bloat` · area: runtime-identity-clients
> STATUS: PARTIAL — #1943 dropped the eight never-populated fields. The v4 sub-claim is still open: NfsDetails.Version is hardcoded "3" at pkg/adapter/nfs/connection.go:109, so /api/v1/clients misreports every v4 client. Tracked in #1962
- **Verified:** CONFIRMED, including the v4 sub-claim. Only two prod constructors exist: pkg/adapter/nfs/connection.go:105-110 sets ClientID/Protocol/Address + NfsDetails{Version:"3"}, and pkg/adapter/smb/connection.go:93-98 sets ClientID/Protocol/Address + SmbDetails{SessionID}. Registry has no field-mutating method (only Register/Deregister/Get/UpdateActivity/Add|RemoveShare/List*). Grep for User/AuthFlavor/Domain/Signed/Encrypted/Dialect assignments hits ONLY clients/service_test.go:15,19,272,276-278,289. So ClientRecord.User(22), NfsDetails.AuthFlavor/UID/GID(33-35), SmbDetails.Dialect(41)/Domain/Signed/Encrypted(42-44) are always zero in the API response — 8 fields, not 6. v4 sub-claim CONFIRMED: the same NFSConnection dispatches both versions (connection.go:177-183, `case rpc.NFSVersion3` / `case rpc.NFSVersion4`) yet Version is hardcoded "3", so /api/v1/clients misreports every v4 client. Fix: drop the never-filled fields, or populate them (Version from call.Version, the auth flavor plus UID and GID from the AuthContext, and the dialect along with the signed and encrypted flags from the SMB session).
- **Fix:** Either thread real values through at registration (auth flavor, UID and GID from the AuthContext; the negotiated dialect and message-integrity state held by the SMB session; the real negotiated NFS version) or drop the unused fields until there's a caller for them.

### [MED] Identical callback-registry logic duplicated for two unrelated notification channels

- **Where:** `pkg/controlplane/runtime/runtime.go:1113` · `bloat` · area: runtime-lifecycle-core
> STATUS: FIXED in #1943 — both channels share one callbackList
- **Verified:** CONFIRMED verbatim duplication. runtime.go:1113-1138 vs 1144-1170 are identical modulo the field name (identityChangeCallbacks:164 vs identityProviderChangeCallbacks:170): append-under-Lock, captured-index unsubscribe that nils the slot, Notify = RLock+copy+iterate+nil-check. REACHABLE both: OnIdentityMappingChange from pkg/adapter/nfs/nlm.go:738, pkg/adapter/smb/adapter.go:782, pkg/adapter/nfs/adapter.go:622; Notify from pkg/controlplane/api/router.go:446,460. OnIdentityProviderConfigChange from smb/adapter.go:693,790,841 and nfs/nlm.go:705; Notify from internal/controlplane/api/handlers/identity_providers.go:221,382. Fix: one `type callbackList struct{ mu sync.Mutex; fns []func() }` with Add/Notify, two fields of that type.
- **Fix:** Extract a small unexported type, e.g. `type callbackList struct { mu sync.Mutex; cbs []func() }` with `Add(fn func()) func()` and `Notify()` methods, and give Runtime two `callbackList` fields instead of two `[]func()` fields plus four hand-written methods. The four public On*/Notify* methods on Runtime become one-line delegations.

### [MED] deriveGCStateRoot and deriveLocalStoreDir are near-verbatim duplicates

- **Where:** `pkg/controlplane/runtime/shares/service.go:2825` · `bloat` · area: runtime-shares
> STATUS: FIXED in #1943 — deriveGCStateRoot calls deriveLocalStoreDir
- **Verified:** CONFIRMED. shares/service.go:2825-2847 and 2861-2883 are line-for-line identical (nil guard, GetConfig(), cfg["path"].(string) ok/empty guard, pathutil.ExpandPath, filepath.IsAbs guard); only the terminal Join differs (extra "gc-state" segment). REACHABLE: both called from prod at service.go:1254 (share.gcStateRoot) and :1258 (share.localStoreDir) inside share creation. Fix: deriveGCStateRoot = { d := deriveLocalStoreDir(cfg,name); if d=="" {return ""}; return filepath.Join(d,"gc-state") }.
- **Fix:** Extract a shared helper, e.g. `func deriveShareBaseDir(cfg interface{ GetConfig() (map[string]any, error) }, shareName string) (string, bool)` returning the expanded `<base>/shares/<sanitized>` dir, then have deriveGCStateRoot do `base, ok := deriveShareBaseDir(...); if !ok { return "" }; return filepath.Join(base, "gc-state")` and deriveLocalStoreDir just return `base`.

### [MED] Service is a god object — 49 methods / ~3300 LOC mixing unrelated responsibilities

- **Where:** `pkg/controlplane/runtime/shares/service.go:471` · `structure` · area: runtime-shares-lifecycle
> STATUS: FIXED #2022 — `shares/service.go` is 581 lines; the rest moved to blockstore_config.go, blockstore_ops.go, lifecycle.go and share_config.go.
- **Verified:** Confirmed and if anything understated: file is 3310 lines, 54 `func (s *Service)` methods. Struct at :471 carries registry+reservations+remoteStores map+two callback registries+metricsRec etc. remoteStores ref-counting (acquireRemoteStore :1449, releaseRemoteStore :1597, plus rebind paths :1372-1433, :2446-2546) is a self-contained sub-concern still inline. Precedent for the extraction exists in-package (warm.go/warmRegistry). Reachable — this is the live share service.
- **Fix:** Extract remote-store ref-counting into its own type (mirroring warmRegistry: e.g. `remoteStorePool` with its own mutex, `acquire`/`release`), and extract the per-field config setters (UpdateShare, SetShareSquash, SetShareTrashConfig, SetShareNetgroup, kerberos setters) into a separate share-config file/type. Service keeps registry CRUD + composition of these sub-components.


---

## LOW findings

### [LOW] Dead EOF-detection branch: engine.Store.ReadAt never returns io.EOF/io.ErrUnexpectedEOF
- **Where:** `internal/adapter/common/read_payload.go:64` · `slop`
- **Fix:** Delete the dead branch (lines 64-66) since the engine contract never short-reads; EOF is correctly derived by callers from file.Size instead. If keeping it as defensive future-proofing for a hypothetical backend, at minimum use errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF).

### [LOW] Issue-number reference embedded in doc comment (ErrNotDurableYet)
- **Where:** `internal/adapter/common/write_payload.go:29` · `comments`
- **Fix:** Drop "See #1274." — the preceding two sentences already state the behavior fully; nothing is lost.

### [LOW] Issue-number reference embedded in doc comment (CommitBlockStore hard-error note)
- **Where:** `internal/adapter/common/write_payload.go:74` · `comments`
- **Fix:** Reword to "ALWAYS surfaced unchanged — engine.Flush returning a non-nil error propagates regardless of policy.", drop the parenthetical.

### [LOW] Issue-number reference embedded in inline comment (Observability log line)
- **Where:** `internal/adapter/common/write_payload.go:116` · `comments`
- **Fix:** Reword to "// Observability: record the per-store durability decision at the ack point..." — drop "(#1245)".

### [LOW] COMMIT silently skips metadata flush and logs nothing when auth-context build fails
- **Where:** `internal/adapter/nfs/v3/handlers/commit.go:211` · `bugs`
> STATUS: FIXED in #1932 — logAuthCtxError + authDenialStatus; COMMIT now logs and fails the RPC, the silent-continue path is gone.
- **Fix:** Log the authErr (e.g. logAuthCtxError(ctx.Context, authErr, "COMMIT", ...)) even though COMMIT proceeds without failing the RPC, so operators can see the flush was skipped.

### [LOW] CommitResponse.WriteVerifier doc comment describes an unimplemented placeholder that the code already implements correctly
- **Where:** `internal/adapter/nfs/v3/handlers/commit.go:69` · `slop`
- **Fix:** Delete the 'In this implementation / A production implementation should...' block; replace with: WriteVerifier is the server boot time (serverBootTime), so it changes only across restart per RFC 1813.

### [LOW] Unconditional fmt.Sprintf hex-formatting of file handle on every READ/WRITE/COMMIT call, not gated by debug level
- **Where:** `internal/adapter/nfs/v3/handlers/read.go:116` · `perf`
> STATUS: FIXED in #1978 — the handle was hex-formatted on every READ/WRITE/COMMIT regardless of log level; now an xdr.LazyHandle slog.LogValuer.
- **Fix:** Wrap read.go:116/250/279-280, write.go:127, commit.go:89 in `if logger.IsDebugEnabled() { ... }`, matching internal/adapter/common/write_payload.go:131.

### [LOW] CompoundResult error-literal repeated ~9x per file instead of using sibling's helper pattern
- **Where:** `internal/adapter/nfs/v4/handlers/read.go:24` · `structure`
> STATUS: FIXED in #2000 — readErr (read.go:204), writeErr (write.go:231) and commitErr (commit.go:128) now mirror readPlusErr; no inline CompoundResult status literal remains in the three handlers.
- **Fix:** Add `readErr(status)`, `writeErr(status)`, `commitErr(status)` (or one generic `opErr(op, status uint32) *types.CompoundResult`) per handler mirroring readPlusErr; replace all inline literals.

### [LOW] nil-Registry guard + block-store resolve boilerplate duplicated across all 4 handlers
- **Where:** `internal/adapter/nfs/v4/handlers/read.go:174` · `structure`
> STATUS: FIXED in #1999 — h.resolveBlockStore (helpers.go:250) is the single resolve path; read.go:128 and the sibling handlers call it.
- **Fix:** Add one `h.resolveBlockStore(ctx, fh, forWrite bool)` helper in this package that read/write/commit call (read_plus keeps its error-return shape). Do NOT push into common.ResolveFor* — the in-code comment deliberately keeps the NFSv4 concern out of common.

### [LOW] READ_PLUS response buffer not pre-sized, unlike sibling READ handler
- **Where:** `internal/adapter/nfs/v4/handlers/read_plus.go:216` · `perf`
> STATUS: FIXED in #1978 — reply buffer pre-sized to the exact wire size, matching encodeRead4resok.
- **Fix:** Compute total wire size up front (12 + per-segment: hole 20B, data 12+len(Data)+pad) and preallocate like encodeRead4resok does.

### [LOW] fmt.Sprintf hex-formats FileID on every READ/WRITE call regardless of log level
- **Where:** `internal/adapter/smb/handlers/write.go:161` · `perf`
> STATUS: FIXED in #1978 — SMB READ/WRITE entry logs go through a lazyFileID LogValuer instead of formatting unconditionally.
- **Fix:** Pass req.FileID as a raw []byte/[16]byte structured field (LogValuer/fmt.Stringer, formatted only when the record is emitted) instead of eager fmt.Sprintf at the call site.

### [LOW] BlockStoreConformance godoc references a nonexistent BlockStoreAppendConformance call
- **Where:** `pkg/block/blockstoretest/conformance.go:63` · `comments`
- **Fix:** Remove this sentence (or replace with the actual current mechanism, if append-conformance was folded elsewhere).

### [LOW] BlockStoreConformance never exercises Head/Has/GetRange on a missing hash, despite the interface pinning ErrChunkNotFound/(false,nil) for exactly that case
- **Where:** `pkg/block/blockstoretest/conformance.go:65` · `gaps`
> STATUS: CLOSED in #1975 — Has_NotFound / Head_NotFound / GetRange_NotFound added to the shared block-store conformance suite. Has_NotFound stores an unrelated object first so a listing-backed backend must still report a definitive miss.
- **Fix:** Add Has_NotFound (assert (false, nil) on an unstored hash), Head_NotFound (assert errors.Is(err, block.ErrChunkNotFound)), and GetRange_NotFound (assert errors.Is(err, block.ErrChunkNotFound) on an absent hash) subtests to BlockStoreConformance, mirroring the existing Get_NotFound pattern (conformance.go:136-159).

### [LOW] doc.go describes a BlockStoreAppendConformance entrypoint and appendlog files that don't exist
- **Where:** `pkg/block/blockstoretest/doc.go:10` · `comments`
- **Fix:** Delete the BlockStoreAppendConformance bullet and the appendlog_internals_test.go reference, or update them to describe whatever superseded the append-log-specific scenarios (if anything). If BlockStoreAppend conformance no longer exists at all, remove every trace of it from this doc.

### [LOW] DeduceDefaults duplicates ClampToInt64's own clamp logic inline
- **Where:** `pkg/block/defaults.go:67` · `structure` · *re-confirmed*
- **Fix:** readBufferSize := ClampToInt64(rbRaw); drop the inline branch.

### [LOW] ParseChunkOffset reinvents strconv.ParseUint, drops overflow protection
- **Where:** `pkg/block/ids.go:7` · `structure`
- **Fix:** Replace with `i := strings.LastIndexByte(id, '/'); if i < 0 || i == len(id)-1 { return 0, false }; v, err := strconv.ParseUint(id[i+1:], 10, 64); return v, err == nil`.

### [LOW] Stale "audit:" paragraph references files that don't exist and miscounts markers
- **Where:** `pkg/block/doc.go:149` · `comments`
- **Fix:** Delete the entire 'audit:' paragraph (lines 149-163). If the TRANSITIONAL-marker convention still needs an example, point at the one real site (engine/cache.go) or drop the example entirely — the preceding paragraphs already document the convention generically.

### [LOW] Dated "Collision check" narration baked into permanent interface doc
- **Where:** `pkg/block/filechunk.go:70` · `comments`
- **Fix:** Delete the dated collision-check note entirely. Drop the "Renamed from ..." clauses from the Delete/Put/GetByHash doc comments (that history belongs in commit messages, not the interface contract).

### [LOW] FileChunk.IsRemote and IsFinalized are identical methods
- **Where:** `pkg/block/types.go:269` · `bloat`
- **Fix:** Keep one (IsRemote reads more naturally given BlockState's naming), delete the other, update the ~6 call sites.

### [LOW] Broken/incomplete sentence — stale edit artifact
- **Where:** `pkg/block/types.go:95` · `comments`
- **Fix:** Reword to "MarshalJSON exists to drive ChunkRef JSON serialization; ..." or simply delete the fragment and keep the following "Without this, encoding/json would default to base64..." sentence.

### [LOW] Leftover internal phase reference in UnmarshalJSON doc
- **Where:** `pkg/block/types.go:107` · `comments`
- **Fix:** Replace with a self-contained description, e.g. "the legacy default base64 form (encoding/json's default for [32]byte arrays before this type had a custom MarshalJSON)" — drop "pre-Phase-12".

### [LOW] Compressed write path double-allocates and double-copies the compressed body
- **Where:** `pkg/block/compression/frame.go:45` · `perf`
> STATUS: FIXED in #1978 — header reserved in the buffer the codec streams into. Measured 20.6 MB to 14.7 MB allocated per 4 MiB semi-compressible Put; the frame-vs-plaintext decision is numerically unchanged.
- **Fix:** Presize compressed (bytes.NewBuffer(make([]byte,0,len(data)))) to cut growth reallocations, and/or reserve FrameHeaderFixedSize+maxOrigSizeVarint up front and stream the codec output after it so encodeFrame's separate allocate-and-copy is unnecessary.

### [LOW] Encrypted write path double-allocates and double-copies the ciphertext
- **Where:** `pkg/block/encryption/frame.go:107` · `perf`
> STATUS: FIXED in #1978 — aead.Seal appends onto the header buffer, dropping the standalone ciphertext allocation and copy. The reservation is exact, so Seal never reallocates.
- **Fix:** Build frame header into `out` first, then `out = aead.Seal(out, nonce, data, hash[:])` to append ciphertext in place; drop the separate ciphertext alloc in sealLayer and the append-copy in encodeFrame.

### [LOW] AES-GCM cipher rebuilt from scratch on every block-key Wrap/Unwrap call
- **Where:** `pkg/block/encryption/keyprovider/local.go:239` · `perf`
> STATUS: WONTFIX — real, but dwarfed by the per-chunk AEAD seal, and a cached AEAD would outlive Close()'s key zeroization.
- **Fix:** Cache the cipher.AEAD once on aesGCMKEK (lazily on first use or in newLocalProvider/newKMIPProvider) and have Wrap/Unwrap reuse it; a new provider instance already covers key rotation.

### [LOW] Legacy-only block.Store CAS surface left in the primary decorator file instead of legacy_cas_migration.go
- **Where:** `pkg/block/encryption/decorator.go:306` · `bloat`
- **Fix:** Move Put/Get/GetRange/Has/Head/Walk/Delete/plaintextSizeFor/casInner into legacy_cas_migration.go in both packages, under the same 'migration-only, delete when retired' banner already used there.

### [LOW] Package doc comment carries issue/PR numbers
- **Where:** `pkg/block/engine/compaction.go:1` · `comments`
- **Fix:** Drop "(#1487)" — "// Package engine — GC compaction of partially-dead blocks."

### [LOW] Phase-ID reference in comment — explicit repo-rule violation
- **Where:** `pkg/block/engine/gc.go:67` · `comments`
- **Fix:** Drop "Phase-11"/"multi-process phase" wording. State it plainly: "sufficient for single-server; cross-process safety needs an OS-level flock (not yet implemented)."

### [LOW] Issue-number references scattered through GC comments
- **Where:** `pkg/block/engine/gc.go:150` · `comments`
- **Fix:** Reword each to state the invariant/behaviour without the issue number, e.g. "...rows whose owning inode was already gone (the pre-fix leak)." — drop "(#1433)" throughout.

### [LOW] GCStats.MaxOrphansPerShare is a dead knob — declared, never read, never set
- **Where:** `pkg/block/engine/gc.go:182` · `bloat`
- **Fix:** Delete the MaxOrphansPerShare field from Options entirely; nothing references it.

### [LOW] GCStats.SharesScanned / BlocksScanned — dead fields explicitly documented as always-zero
- **Where:** `pkg/block/engine/gc.go:161` · `bloat`
- **Fix:** Remove SharesScanned and BlocksScanned from GCStats (and the corresponding JSON wire keys / dfsctl display code if any), or if REST clients truly still parse them, replace with a short migration note instead of two permanently-zero int fields plus a paragraph justifying their uselessness.

### [LOW] Package doc comment carries issue/PR numbers
- **Where:** `pkg/block/engine/gc_block.go:1` · `comments`
- **Fix:** Rewrite to describe behaviour: "Package engine — block-aware GC reclaim for packed-block objects." Drop #1414/PR3/#1493/#1637 refs from body comments too.

### [LOW] Package doc comment carries issue/PR numbers
- **Where:** `pkg/block/engine/reclaim.go:1` · `comments`
- **Fix:** "// Package engine — orphan-storage reclaim, the deleting stages after the read-only reporter (reconcile.go)." Strip issue/PR tokens from the body comments too (class 1/2 descriptions stand fine without them).

### [LOW] Package doc comment carries issue/PR numbers
- **Where:** `pkg/block/engine/reconcile.go:1` · `comments`
- **Fix:** "// Package engine — read-only orphan-storage reporter. Enumerates and classifies orphaned block storage WITHOUT mutating anything... an operator reviews the report before the later delete stages act." Drop PR5a/PR5b/5c/#1493/#1525/#1554.

### [LOW] compactOneBlock re-fetches per-chunk locators via GetLocator instead of reusing the already-scanned EnumerateSynced result
- **Where:** `pkg/block/engine/compaction.go:237` · `perf`
> STATUS: WONTFIX — background GC pass, not a request path; a locator snapshot would also change move-decision freshness.
- **Fix:** In CompactBlocks' EnumerateSynced callback (line 143), also build a hash->locator map alongside liveBytes and pass it into compactOneBlock instead of calling v.GetLocator per record.

### [LOW] CollectGarbage doc comment contradicts markPhase's actual fail-closed hard-error behavior
- **Where:** `pkg/block/engine/gc.go:347` · `slop`
- **Fix:** Update the CollectGarbage doc (lines 347-351) to state that a reconciler reporting zero shares aborts the run via markPhase's fail-closed guard, instead of the superseded 'empty live set, sweep everything' description.

### [LOW] Cache stores context in a struct field that is never read
- **Where:** `pkg/block/engine/cache.go:90` · `structure`
- **Fix:** Drop the `ctx context.Context` field (cache.go:90) and the `ctx: ctx` initializer in NewCache; keep only `cancel`, which is the sole thing Close() uses.

### [LOW] Unconditional raState allocation on every read, even on map hit
- **Where:** `pkg/block/engine/readahead.go:48` · `perf`
> STATUS: FIXED in #1978 — Load before LoadOrStore, so a readahead hit no longer builds a throwaway raState on the per-read path.
- **Fix:** Load first, only allocate on miss: `v, loaded := m.readahead.Load(payloadID); if !loaded { v, loaded = m.readahead.LoadOrStore(payloadID, &raState{}) }`.

### [LOW] WarmAll's BlocksAlreadyLocal stat is dead code — always 0, contradicts its own doc comment
- **Where:** `pkg/block/engine/warm.go:78` · `slop` · *re-confirmed*
> STATUS: FIXED in #1959
- **Fix:** Delete the dead alreadyLocal var + BlocksAlreadyLocal field and the stale 'split into already-local/to-fetch' doc (warm.go:16, 20-21, 60-61), and simplify the shares/warm.go:174 check to use the enumerated-target count.

### [LOW] WarmAll always re-downloads every chunk from remote, even chunks already resident locally
- **Where:** `pkg/block/engine/warm.go:95` · `perf`
> STATUS: REFUTED — the code already carries a comment explaining the probe is impossible (the journal is not hash-keyed), and WarmAll is an operator one-shot rather than a hot path.
- **Fix:** Probe local residency (e.g. via the journal/local store) per FileChunk before dispatching the remote fetch, skip already-resident chunks, and increment the existing (currently dead) alreadyLocal counter instead of always going remote.

### [LOW] WarmResult.BlocksAlreadyLocal / alreadyLocal counter declared but never incremented
- **Where:** `pkg/block/engine/warm.go:78` · `structure` · *re-confirmed*
> STATUS: FIXED in #1959
- **Fix:** Delete alreadyLocal + WarmResult.BlocksAlreadyLocal (and the shares/warm.go:174 term, service.go:2808 field) and fix the :61 doc comment; or implement the local-presence skip it promises.

### [LOW] Unnecessary pre-1.22 loop-variable-capture idiom
- **Where:** `pkg/block/engine/warm.go:147` · `structure`
- **Fix:** Delete the `t := t` line at warm.go:147; range var is per-iteration under go 1.25.0.

### [LOW] AuditRefcountsResult.Delta duplicates DanglingRefs with no added information
- **Where:** `pkg/block/engine/audit_state.go:92` · `bloat`
- **Fix:** Drop the `Delta` field and have callers (dfsctl audit.go, blockstore_audit.go handler, runtime/blockaudit.go) key off `DanglingRefs != 0` directly, or if a separate signed 'violation' name is wanted for API stability, at least stop duplicating the doc/tests around two names for one value.

### [LOW] hasLegacy bool threaded alongside legacy interface value — pure duplicate of a nil check
- **Where:** `pkg/block/engine/legacy_migration.go:98` · `bloat`
- **Fix:** Drop the `hasLegacy` parameter from `repackStandaloneChunks` and `migrationChunkBytes`; replace `if hasLegacy` / `if !hasLegacy` with `if legacy != nil` / `if legacy == nil`.

### [LOW] Stale forward-looking comment cites a milestone version 12 releases in the past
- **Where:** `pkg/block/engine/cache.go:46` · `comments`
- **Fix:** Delete the comment (or replace with a plain behavioural note that the cache is populated lazily on first-read only, no proactive warm). Track any real 'add cold-start warming' intent in an issue, not in source.

### [LOW] Internal ticket/requirement IDs (CACHE-0x, T-12-2x) baked into source comments
- **Where:** `pkg/block/engine/cache.go:27` · `comments`
- **Fix:** Strip the ticket/requirement codes, keep the behavioural sentence that follows each (e.g. 'bounded channel capacity; submit is non-blocking and drops when full' instead of '(T-12-24 mitigation: ...)').

### [LOW] EnsureAvailableAndRead's bool return value is always false — dead return, needless indirection
- **Where:** `pkg/block/engine/fetch.go:340` · `bloat`
- **Fix:** Change signature to `func (m *Syncer) EnsureAvailableAndRead(...) error` and drop the bool from every return statement and the doc comment.

### [LOW] "Post-Phase-17/18" migration-phase references in read-path comments
- **Where:** `pkg/block/engine/fetch.go:43` · `comments`
- **Fix:** Rewrite each as a plain statement of the current invariant, e.g. 'fb.Hash MUST be non-zero for any reachable block; a zero hash is migration drift, not a legacy format' — drop the 'Post-Phase-N' framing entirely.

### [LOW] Issue-number reference (#1487) embedded in fetch-retry comments
- **Where:** `pkg/block/engine/fetch.go:123` · `comments`
- **Fix:** Drop '(#1487)' from both comments; the surrounding prose already explains the compaction/migration race without needing the issue number.

### [LOW] EnsureAvailableAndRead: docstring claims it fills dest directly; it never does — bool return is permanently dead
- **Where:** `pkg/block/engine/fetch.go:337` · `slop`
- **Fix:** Drop the dead `bool` from the signature (→ `error`) and rewrite the docstring to say it only hydrates the local tier — the caller must always re-read locally. (Or restore direct-fill and update callers.)

### [LOW] Dead pre-1.22 loop-variable capture idiom
- **Where:** `pkg/block/engine/fetch.go:436` · `structure`
- **Fix:** Delete the `p := p` line at fetch.go:436; range var is per-iteration under go 1.25.0.

### [LOW] MetadataCoordinator.DecrementRefCount is a dead interface method — engine never calls it
- **Where:** `pkg/block/engine/coordinator.go:37` · `bloat`
- **Fix:** Remove DecrementRefCount from the MetadataCoordinator interface (and its shares/coordinator.go implementation) if nothing outside tests needs plain-decrement semantics; keep DecrementRefCountAndReap as the sole reap-path primitive.

### [LOW] Truncated sentence fragment: stray '. Per-metadata-store scope...' continuation line
- **Where:** `pkg/block/engine/coordinator.go:95` · `comments`
- **Fix:** Merge into one clause, e.g. 'Used by the file-level dedup short-circuit. Per-metadata-store scope, not per-share.'

### [LOW] loadByHash doc comment describes behavior the function no longer has
- **Where:** `pkg/block/engine/engine.go:288` · `comments`
- **Fix:** Delete/rewrite the 288-301 doc block to just say loadByHash is a permanent no-op miss (matching the inline comment at 302-307), or delete the inline comment and keep only the doc block updated to state the no-op behavior once.

### [LOW] syncedHashStore field comment references removed ObjectIDPersister wiring path
- **Where:** `pkg/block/engine/engine.go:96` · `comments`
- **Fix:** Drop the FSStore/ObjectIDPersister clause; comment should just say it's threaded into the Syncer.

### [LOW] LocalForTest() and Local() are byte-identical accessors for the same field
- **Where:** `pkg/block/engine/engine.go:444` · `bloat`
- **Fix:** Delete LocalForTest and repoint its 2 test call sites (api_accessors_test.go:55, commit_durability_test.go:225) at Local().

### [LOW] ReadAt's `blocks []block.ChunkRef` param is dead across the whole call chain
- **Where:** `pkg/block/engine/readwrite.go:22` · `bloat`
- **Fix:** Drop `blocks` from block.Store.ReadAt (pkg/block/filechunk.go:179) and from Store.ReadAt's signature; update the handful of call sites (already passing nil, so it's a mechanical removal).

### [LOW] Truncated/garbled sentence fragment: 'semantics post-).'
- **Where:** `pkg/block/engine/readwrite.go:210` · `comments`
- **Fix:** Rewrite as '(matches engine.Delete semantics)' or finish the intended clause.

### [LOW] Truncated sentence fragment: 'are removed in; the legacy adapter call sites...'
- **Where:** `pkg/block/engine/readwrite.go:489` · `comments`
- **Fix:** Rewrite as 'legacy CopyPayload data-copy semantics have been removed; legacy adapter call sites...' (drop the dangling 'in').

### [LOW] CopyPayload's srcPayloadID unused-parameter escape hatch is dead code, not a real need
- **Where:** `pkg/block/engine/readwrite.go:610` · `bloat`
- **Fix:** Delete line 610-612 (`_ = srcPayloadID` and its comment) — unused function parameters compile fine in Go without a blank assignment; the param itself stays for interface signature symmetry with dstPayloadID.

### [LOW] Stats/GetStats/GetStatsLite/LocalStats drop context propagation, manufacturing context.Background() for DB reads
> STATUS: WONTFIX, premise partly corrected -- there is exactly ONE `context.Background()` in the file (`stats.go:190`), not the several implied, and it is already bounded by a 5s timeout. Threading a real ctx means adding one to `local.LocalStore.Stats()` and every backend implementing it, i.e. churning the same interface the finding two entries down says not to churn. Marked with a `ponytail:` at the call site.
- **Where:** `pkg/block/engine/stats.go:74` · `structure`
- **Fix:** Thread ctx through GetStats(ctx)/GetStatsLite(ctx)/Stats(ctx)/LocalStats(ctx) from the callers (they already have one), replacing both context.Background() call sites.

### [LOW] BlockStoreStats.PendingSyncs and PendingUploads are duplicate always-zero fields
- **Where:** `pkg/block/engine/stats.go:135` · `bloat`
- **Fix:** Collapse to one field (or drop both, documenting UnsyncedBytes as the real backpressure signal per the existing comment) instead of keeping two identical always-zero fields.

### [LOW] Eager remote health probe runs synchronously while Syncer.mu is held, stalling every read/flush call during share startup
- **Where:** `pkg/block/engine/syncer.go:664` · `bugs`
> STATUS: FIXED in #1971 — Start/SetRemoteStore split into a locked half so the eager health probe runs with m.mu released. Kept synchronous deliberately: engine.go:269 reconciles local eviction from IsRemoteHealthy() right after Start returns, so deferring the probe would leave eviction enabled through the first probe RTT with the remote down.
- **Fix:** Release m.mu before calling hm.Start(ctx), or make HealthMonitor.Start's eager probe itself asynchronous (spawn the probe+state-transition in the goroutine instead of running it inline before go hm.monitorLoop(ctx)). At minimum bound the eager probe with a short context.WithTimeout so the Syncer.Start() critical section has a hard ceiling.

### [LOW] Dead computed variable in startPeriodicUploader — copy-paste drift
- **Where:** `pkg/block/engine/syncer.go:702` · `slop`
> STATUS: FIXED in #1971 — dead UploadInterval computation deleted; carve_dispatch.go:35 owns that read.
- **Fix:** Delete lines 702-706 (interval computation + `_ = interval`) from startPeriodicUploader; carveDispatcher already owns and defaults its own interval independently.

### [LOW] Syncer is a god object bundling fetch-dedup, readahead, health, adaptive upload control, carve wiring and queue ownership
> STATUS: WONTFIX -- every field named here is read on the fetch/carve/close lock-ordering path, whose failure mode is silent zeros. Giving carve wiring its own lock introduces a second lock order for legibility only. Marked with a `ponytail:` at the struct; revisit when a hardware rig can prove a split preserves the ordering.
- **Where:** `pkg/block/engine/syncer.go:34` · `structure`
- **Fix:** Extract carve wiring (remoteBlockStore/chunkSealer/blockCommitter/carveActive/carveTargetsWired/wireCarveTargets) into its own collaborator with its own lock so it stops sharing m.mu with health/close.

### [LOW] Dead UploadInterval computation in startPeriodicUploader — discarded via blank identifier
- **Where:** `pkg/block/engine/syncer.go:702` · `bugs`
> STATUS: FIXED in #1971 — dead UploadInterval computation deleted; carve_dispatch.go:35 owns that read.
- **Fix:** Delete lines 702-706 (the interval/default/blank-assignment) from startPeriodicUploader; leave carveDispatcher as the single owner of that config read.

### [LOW] Dead code: interval computed then discarded via `_ = interval`
- **Where:** `pkg/block/engine/syncer.go:702` · `structure` · *re-confirmed*
> STATUS: FIXED in #1971 — dead UploadInterval computation deleted; carve_dispatch.go:35 owns that read.
- **Fix:** Delete syncer.go:702-706 (`interval := m.config.UploadInterval`, the `<= 0` default, and `_ = interval`); carve_dispatch.go:35 already reads m.config.UploadInterval and applies its own default.

### [LOW] Stale comment references removed addPendingHash/pendingMu symbols
- **Where:** `pkg/block/engine/syncer.go:37` · `comments`
- **Fix:** Rewrite to describe the actual current caller(s) that read hasRemote lock-free, or delete the pendingMu/addPendingHash clause entirely.

### [LOW] Syncer.Close() / HealthMonitor.Stop() don't join background goroutines before returning — use-after-close race on local/remote teardown
- **Where:** `pkg/block/engine/syncer.go:914` · `gaps`
> STATUS: FIXED in #1959 — the bgWG/waitBounded and HealthMonitor joins landed after the audit tree. #1975 adds the two coverage tests (both verified failing against the pre-fix build) plus a stop-over-queued-tick guard so the join cannot wait on one more probe.
- **Fix:** Add a sync.WaitGroup to Syncer covering carveDispatcher and runUploadController (Add(1) before each `go`, Done() on return) and one to HealthMonitor covering monitorLoop; Wait() on it inside Close()/Stop() using the same timeout pattern SyncQueue.Stop already uses, before returning.

### [LOW] Record checksum never covers the FileID bytes — silent misattribution on bit rot
- **Where:** `pkg/block/journal/record.go:85` · `gaps`
> STATUS: TRACKED as #1976 — the FileID bytes genuinely fall outside the CRC'd span, but every fix changes the on-disk record layout and cannot read existing journals. Needs a format version and a migration, not a sweep patch.
- **Fix:** Extend the CRC coverage to include the FileID bytes: either widen headerCRCCovers-style checksum to cover Magic..Version+FileID (recomputed on every append, which is cheap since FileID is short) and updated at the same one-byte Flags-flip site as today (Flags stays excluded, everything else including FileID included), or fold the FileID into the existing payload CRC computation (crc(fileID) combined with crc(payload), e.g. running the crc32 update sequentially over fileID then payload) so readRecordAt's validation catches a corrupted FileID the same way it catches a corrupted payload.

### [LOW] appendTombstone / appendTruncateMarker are near-duplicate ~45-line methods
> STATUS: DIFFED, no drift. WONTFIX. `segment.go:410` vs `:465` diff to: the signature, the fail-injection seam name, the record-writer call, the idx `FileOffset`/`Flags` fields, and comment text. Everything load-bearing matches -- both take `sh.mu` right after `shardFor`, unlock explicitly on each error path and once before `sh.groupCommit()` (neither uses `defer`, neither leaks the lock), and both run rollover -> `sh.active` -> `nextVersion` -> write record -> `noteMinVersion` -> `idxFD.Write` -> unlock -> groupCommit in that order. Truncate's extra `FileOffset` is required, not drift. Worth recording because it is NOT the #1909 class: NEITHER marker path stamps `sh.lastVersion`, so `groupCommit`'s ceiling never covers a marker's version. That errs conservative -- the fsync physically covers the bytes and the watermark merely under-claims, so `DurableExtent` and `shard.dirty()` can only under-report durability, never over-report. Collapsing two 45-line methods whose every line sits on that path is not worth the risk.
- **Where:** `pkg/block/journal/segment.go:383` · `bloat`
- **Fix:** Collapse to one Store.appendMarker(ctx, id FileID, flags uint8, fileOffset uint64, testFail FileID) (uint64, error) that does the shared work and calls a single writeMarkerRecord(seg, id, version, flags, fileOffset) (recordHeader already has both fields). appendTombstone becomes appendMarker(ctx, id, flagTombstone, 0, testFailTombstone) and appendTruncateMarker becomes appendMarker(ctx, id, flagTruncate, uint64(newSize), testFailTruncate); one testFailMarker var (keyed by id) can replace the two separate globals.

### [LOW] appendRecord holds shard mutex across multiple disk-write syscalls (mutex held across I/O)
- **Where:** `pkg/block/journal/segment.go:279` · `structure`
> STATUS: WONTFIX — the observation is real, the fix is unsound. `sh.lastVersion` is a *scalar* watermark, and both `groupCommit` (`store.go:556-568`) and `DurableExtent` (`store.go:715`) read it as "every Version at or below this is durable". That only holds while record completion order equals Version order, which is exactly what holding `sh.mu` across the writes buys. Traced interleaving, stamping the version inside the reservation: writer W reserves `segOff`, takes v=7, stamps `lastVersion=7`, unlocks, and is preempted before its first `pwrite`; a concurrent NFS COMMIT leads `groupCommit`, reads `upTo=7`, fsyncs (flushing a page cache that does not yet contain W's bytes), gets nil, and calls `markSynced(7)`. The client is told its write is durable, `DurableExtent` publishes the range as the committed file size through `SetDurableExtentResolver` (`shares/lifecycle.go:131-177`), and `shard.dirty()` reports the shard clean so the periodic dirty-age commit skips it. On power loss recovery's scan stops at the unwritten record and the acked range returns as a hole of zeros — #1909 / #1929 verbatim. Stamping the version *after* the unlocked writes fails the same way one version lower: appenders v=7 and v=8 can complete in either order, so v=8 finishing first raises the ceiling over v=7's in-flight bytes. Releasing the lock therefore requires replacing the watermark with an in-flight version set (ceiling = lowest in-flight Version minus one) plus a rotation barrier — `sealSegment` runs inside this same lock and calls `markSynced(sh.lastVersion)` on the identical premise. Recorded as a `ponytail:` marker on `appendRecord` naming that upgrade path, since nothing tests it: `groupcommit_durability_test.go` and `durable_extent_test.go` are single-goroutine and would stay green through the broken refactor. One correction to the triage — the parenthetical about `idxFD.Write` ordering is not load-bearing: the `.idx` sidecar is write-only today (nothing decodes `idxEntry`; recovery always rebuilds from the `.seg` scan) and is opened `O_APPEND`, so entries cannot tear. The version watermark is what kills the fix.
- **Fix:** Reserve segOff/version/tail (and do segment rotation) under sh.mu, release the lock, do the three WriteAt calls unlocked against the reserved offset window, then reacquire sh.mu only to update seg counters, the fileIndex and the idx sidecar entry — mirroring readPayload's snapshot-then-unlocked-I/O shape. Note the reservation must preserve contiguous segment layout for the recovery scan.

### [LOW] Package-global mutable test-seam vars bypass the injectable-seam pattern already established in the same file
- **Where:** `pkg/block/journal/segment.go:433` · `structure`
- **Fix:** Replace both with per-Store injectable hooks (`failTombstone func(FileID) error` / `failTruncate func(FileID) error`, nil in production) matching the shard.segSync seam shape.

### [LOW] Two competing logging idioms coexist in the same file/package instead of one canonical logger
- **Where:** `pkg/block/journal/reclaim.go:746` · `structure`
- **Fix:** Standardize on internal/logger.Warn for every operator-facing warning in journal (recovery.go:241, recovery.go:344, cold.go:182, reclaim.go:746) and delete the `logf` package var, or make it a thin adapter that calls logger.Warn with the message as the single field. Pick one.

### [LOW] Background reclaim seals the shard's active segment (multiple synchronous fsyncs) while holding sh.mu
- **Where:** `pkg/block/journal/reclaim.go:131` · `structure`
> STATUS: WONTFIX, accepted latency — with the exposure measured rather than assumed. The asymmetry the triage claims is real: `seg.busy` is CAS-set only in `claimColdestEvictable` (`reclaim.go:150-175`) and the GC victim claim, both of which scan `sh.sealed` only, so nothing ever claims `sh.active` and `sealableActive`'s `!act.busy.Load()` term is vacuous. But the claim is not the load-bearing half — `appendRecord` never consults `seg.busy` at all, it takes `sh.mu` and appends into whatever `sh.active` is. `evictSegment`/`repackSegment` can unlock because their victim is *sealed*: immutable, unreachable by any appender, with `busy` only excluding the other reclaim passes. Unlocking an active seal would let a record land between `sealInPlace`'s data fsync and its sealed-bit fsync, which is precisely the ordering that comment names ("data is fsynced BEFORE the sealed bit so recovery never trusts a header whose records did not reach disk"), turning a latency finding into a correctness one. Scale: `sealSyncedActives` is reached only from `Store.Evict` (`allowActiveSeal=true`, `reclaim.go:57`); the write-path gate `ensureSpace` passes `false` (`reclaim.go:311`) and no timer drives it, so its only production caller is `engine.DrainLocalSynced` behind the operator's `store block evict` (`shares/blockstore_ops.go:350`). One pass per call (bounded by the `sealedActives` flag), one shard locked at a time out of 16, and the cost per shard is four fsyncs — `sealInPlace`'s two plus `createSegment`'s `fd.Sync` and `fsyncDir`; the audit's "three" undercounts. During those, that one shard's `appendRecord`, `ReadAt` index snapshot, `groupCommit` ceiling read, `DurableExtent` and carve flip block. The identical sequence already runs under the same lock on the hot write path at every rotation (every `SegmentSize`, 256 MiB by default, per shard), so an operator-triggered drain that is already evicting the whole local tier is not where this matters. Recorded as a `ponytail:` marker naming the ceiling and the append-exclusion upgrade path.
- **Fix:** Follow the same pattern evictSegment/repackSegment already use: snapshot what needs sealing under sh.mu, release the lock, do sealSegment's I/O unlocked (sealSegment would need a variant that doesn't assume the lock, or its two sub-steps split so only the in-memory `sh.active = seg` / `sh.sealed[old.id] = old` mutations happen under the lock), then re-acquire briefly to publish the result.

### [LOW] Comment references external PR number, violates repo's no-external-refs rule
- **Where:** `pkg/block/journal/carve.go:113` · `comments`
- **Fix:** Drop the PR reference: "Call once before the first Carve; production wires the real impls, tests pass fakes."

### [LOW] Issue-number references scattered through carve/index comments
- **Where:** `pkg/block/journal/carve.go:100` · `comments`
- **Fix:** Strip the "(#953)", "(#953-A)", "(#1736)" tokens from each comment, keeping the surrounding explanation intact (e.g. "a partial overwrite of a warm ... interval leaves surviving fragments ..." without the ticket prefix).

### [LOW] Duplicated "fully-synced idle active segment" predicate
- **Where:** `pkg/block/journal/reclaim.go:128` · `bloat`
- **Fix:** Extract a shared helper, e.g. `func (s *Store) fullySyncedIdleActive(sh *shard) *segmentMeta` or `func fullySynced(act *segmentMeta) bool`, and call it from both sealSyncedActives and reclaimEmptied.

### [LOW] Multiple issue-number references embedded in behaviour comments
- **Where:** `pkg/block/journal/reclaim.go:167` · `comments`
- **Fix:** Delete the "(#1718)" suffixes; the surrounding prose already fully explains the invariant without needing the ticket pointer.

### [LOW] Issue-number reference in recovery comment
- **Where:** `pkg/block/journal/recovery.go:189` · `comments`
- **Fix:** Drop "(#1718)"; the preceding clause already states the behaviour.

### [LOW] FSStore embeds *journal.Store, leaking its full public API and bypassing the payloadID shim/legacy-migration gate for unshadowed methods
> STATUS: WONTFIX -- converting the embed to a named field means hand-writing pass-throughs for every method the tree already calls through it (SeedCold, JournalVersion, SetPinVersion, PinVersion, RestoreToVersion, SetVerifyReads, FileCount, SetEvictionEnabled, SetCarveTargets, UnsyncedBytes, Close), reached from snapshot.go, shares/service.go and the local.LocalStore interface. ~16 forwarders to gate a hazard the audit itself found unreachable. Marked with a `ponytail:` at the struct.
- **Where:** `pkg/block/local/fs/fs.go:56` · `structure`
- **Fix:** Change `*journal.Store` to an unexported named field (`js *journal.Store`) and add explicit pass-through methods for what FSStore genuinely exposes (Close, Evict, SetEvictionEnabled, SetCarveTargets, Carve, UnsyncedBytes), forcing a conscious shim/no-shim decision whenever journal.Store grows a method.

### [LOW] LocalStore is a 20-method god interface mixing 4 unrelated concerns
> STATUS: WONTFIX -- already sectioned by comment, and every production consumer holds the whole store, so splitting adds named unions without narrowing one dependency. Marked with a `ponytail:` at the interface.
- **Where:** `pkg/block/local/local.go:33` · `structure`
- **Fix:** Split into composable interfaces at point of use (DataPlane, Carver, Evictor, Lifecycle) and have LocalStore = a type alias/union only where a consumer genuinely needs all of them (engine.Store). Callers that need one capability type-assert narrowly, following the pattern already used correctly at pkg/block/engine/flush.go:114-164 for the journal-only capabilities.

### [LOW] MemoryStore.ReadAt: unconditional clear(dst) before copy doubles memory writes on the common fully-covered read
- **Where:** `pkg/block/local/memory/memory.go:114` · `perf`
> STATUS: FIXED in #1978 — only the tail the copy misses is zeroed.
- **Fix:** Only zero the uncovered tail: n := copy(dst, f.buf[offset:]); if n < len(dst) { clear(dst[n:]) } (keep the existing early clear+return for the f == nil || offset >= len(f.buf) branch).

### [LOW] memory.ErrStoreClosed is a redundant same-value alias kept 'for backward compatibility'
- **Where:** `pkg/block/local/memory/memory.go:26` · `bloat`
- **Fix:** Drop the local alias; return block.ErrStoreClosed directly at the three sites and remove the alias + comment.

### [LOW] Redundant double copy on every memory-backend PutBlock
- **Where:** `pkg/block/remote/memory/store.go:69` · `perf`
> STATUS: FIXED in #1978 — redundant copy dropped; io.ReadAll already returns a private buffer and every read path copies out.
- **Fix:** Drop the second copy; store `data` directly: s.blocksByID[blockID] = &memBlock{data: data, lastModified: s.nowFn()}.

### [LOW] RemoteStore capability-interface doc directly contradicts the interface it documents
- **Where:** `pkg/block/remote/remote.go:114` · `structure`
- **Fix:** Pick one: either un-embed ChunkReader/ChunkSealer from RemoteStore and go back to type-assertion (matches doc + LegacyCASStore precedent below), or delete the now-false doc paragraph and describe the interface as it is (mandatory, not optional).

### [LOW] LegacyCASStore (migration-only, slated for deletion) embedded into the permanent RemoteStore contract, though callers already type-assert it
- **Where:** `pkg/block/remote/remote.go:49` · `structure`
- **Fix:** Drop `LegacyCASStore` from the RemoteStore embed; let legacy_migration.go's existing type assertion be the only access path (same pattern already used for ChunkReader/ChunkSealer per the — currently false — doc intent). Shrinks the interface every new backend must satisfy and makes the promised future deletion trivial.

### [LOW] EXPOSE directives have drifted out of sync across the three production Dockerfiles
- **Where:** `Dockerfile.prebuilt:10` · `slop`
- **Fix:** Pick one canonical EXPOSE list (NFS/SMB/API + metrics + mDNS/WSD, matching what the binary can actually serve) and apply it consistently across Dockerfile, Dockerfile.prebuilt, and Dockerfile.goreleaser, carrying the explanatory comment for the discovery ports (and adding one for 9090/metrics).

### [LOW] exec.Executor interface has exactly one implementation and its own tests bypass it
- **Where:** `internal/dfsbench/exec/ssh.go:20` · `bloat`
- **Fix:** Either add the promised test fake and use it in at least one test, or collapse Executor back to the concrete *sshExecutor type and drop the interface until a second transport actually exists.

### [LOW] Admin-SID predicate duplicated across pkg/metadata and pkg/metadata/acl instead of extracted to a shared leaf package
- **Where:** `pkg/metadata/acl/evaluate.go:15` · `structure`
- **Fix:** Move IsAdministratorSID (and the regex) into a leaf package (or pkg/metadata/acl itself, since acl is already imported by pkg/metadata) and have pkg/metadata/auth_identity.go call the shared predicate instead of keeping a parallel copy.

### [LOW] isAdminSID duplicated between acl package and pkg/metadata/auth_identity.go
- **Where:** `pkg/metadata/acl/evaluate.go:13` · `bloat`
- **Fix:** Move the SID literal + regex + predicate into pkg/metadata/acl (already imported by pkg/metadata) and have auth_identity.go's IsAdministratorSID call acl's version instead of keeping a parallel copy.

### [LOW] IsXXXError helpers in errors.go are copy-pasted boilerplate, no shared unwrap helper
- **Where:** `pkg/metadata/errors/errors.go:347` · `structure` · *re-confirmed*
- **Fix:** Add `func hasCode(err error, codes ...ErrorCode) bool { var se *StoreError; if !goerrors.As(err, &se) { return false }; return slices.Contains(codes, se.Code) }` and rewrite each IsXXXError as a one-liner over it.

### [LOW] GenerateCookie allocates a boxed hash.Hash64 + byte-slice copies per directory entry, on every READDIR page
- **Where:** `pkg/metadata/cookies.go:73` · `perf`
> STATUS: FIXED in #1978 — FNV-1a inlined, removing 3 allocations per dirent. A test pins the digest against hash/fnv so cookies stay stable across restarts.
- **Fix:** Inline FNV-1a as a plain loop over the string bytes (no hash.Hash interface, no []byte conversion): XOR-multiply into a local uint64. Drops all 3 allocations per call.

### [LOW] pkg/metadata/errors.go + lock_exports.go are wholesale back-compat re-export shims, doubling the package's public API surface
- **Where:** `pkg/metadata/errors.go:1` · `structure`
> STATUS: WONTFIX in #2025 — see the errors.go:1 `bloat` entry below; the files are the package's public API by design, not a deprecated shim, and the doc comments now say so.
- **Fix:** Grep-replace the ~60 call sites to import github.com/marmos91/dittofs/pkg/metadata/errors and .../lock directly, then delete errors.go's re-export section and lock_exports.go entirely. If genuinely still needed for an external consumer, that consumer should be named in the deprecation comment instead of 'backward compatibility' generically.

### [LOW] Deprecated aliases in lock_exports.go are mutable package-level function vars, not wrapper funcs
- **Where:** `pkg/metadata/lock_exports.go:121` · `structure`
> STATUS: OPEN — #2025 kept the shim deliberately (see the errors.go:1 entry above), so the var-vs-func point survives its premise: metadata.NewLockManager and the seven sibling aliases are still reassignable package-level vars.
- **Fix:** If the shim is kept at all (prefer deleting it), convert each `var X = pkg.X` to a thin `func X(...) ... { return pkg.X(...) }`.

### [LOW] GetQuotaUsage failure silently disables quota enforcement, zero logging
- **Where:** `pkg/metadata/quota_enforce.go:51` · `bugs`
> STATUS: FIXED in #1971 — a GetQuotaUsage failure now logs at Error before degrading to no-quota.
- **Fix:** Log the failure, e.g. `logger.Error("quota: usage lookup failed, allowing write", "share", shareName, "scope", scope, "id", usageID, "error", err)` before returning nil, so a persistently broken quota backend is visible instead of silently degrading enforcement.

### [LOW] errors.go: entire file is a dead-weight deprecated re-export shim over pkg/metadata/errors and pkg/metadata/lock
- **Where:** `pkg/metadata/errors.go:1` · `bloat`
> STATUS: WONTFIX in #2025 — reframed rather than removed. errors.go and lock_exports.go are documented as pkg/metadata's public lock and error surface, not a back-compat shim: the implementations must live in pkg/metadata/lock and pkg/metadata/errors because those packages cannot import metadata, and the aliases keep the API reachable from the package that owns it.
- **Fix:** Do the mechanical rename: update the ~25 call sites to import pkg/metadata/errors (and pkg/metadata/lock where needed) directly and call errors.NewXXXError/lock.NewXXXError, then delete pkg/metadata/errors.go outright.

### [LOW] CheckStickyBitRestriction has line-by-line narration comments restating the following statement
- **Where:** `pkg/metadata/validation.go:391` · `comments`
- **Fix:** Drop the line-by-line narration comments (`// Debug: Log sticky bit check details`, `// Get the effective UID of the caller`, `// Check if the caller owns the file being deleted/renamed`, `// Check if the caller owns the sticky directory`, `// Sticky bit restriction applies - deny the operation`); keep only the function-level doc comment which already explains the sticky-bit semantics.

### [LOW] root_squash / admin-squash leaves Username+Domain unsquashed, letting named-principal ACEs re-grant privileged identity
- **Where:** `pkg/metadata/auth_identity.go:317` · `gaps`
> STATUS: FIXED in #1975 — root/admin squash now clears Username and Domain. ACL evaluation resolves Who from them, so a named-principal ACE re-granted the privilege the squash had just removed.
- **Fix:** In the MapPrivilegedToAnonymous branch of ApplyIdentityMapping (auth_identity.go ~320-330), clear result.Username and result.Domain the same way MapAllToAnonymous does, so no named-principal ACE can match a squashed identity.

### [LOW] Redundant "Check context" comment restates the next line verbatim
- **Where:** `pkg/metadata/auth_permissions.go:235` · `comments`
- **Fix:** Delete both `// Check context` comments (lines 235 and 261); the code is self-explanatory.

### [LOW] "using CRUD operation/method" comments are meaningless filler on plain getter calls
- **Where:** `pkg/metadata/auth_permissions.go:50` · `comments`
- **Fix:** Delete both comments, or if genuinely useful, replace with something that explains why the call is needed here (e.g. read-only ceiling context), not what it obviously does.

### [LOW] CopyFileAttr (generic WCC-attr deep-copy helper) defined in the identity/auth file
- **Where:** `pkg/metadata/auth_identity.go:376` · `structure`
- **Fix:** Move CopyFileAttr to file_types.go next to the FileAttr type definition it copies.

### [LOW] evaluateWithACL walks the whole ACL once per requested-permission bit instead of one single-pass EvaluateGranted call
- **Where:** `pkg/metadata/auth_permissions.go:489` · `perf`
> STATUS: WONTFIX — the loop only evaluates requested bits, normally one. Folding the masks risks DENY and owner-implicit drift for no measurable gain.
- **Fix:** OR the ACE masks of all set Permission bits, call acl.EvaluateGranted(fileACL, evalCtx, combined) once (acl/evaluate.go:292), then map granted mask back through permToACLMask (pm.mask&granted==pm.mask).

### [LOW] FileAccessChecker: single-implementation capability interface declared in the producer package
- **Where:** `pkg/metadata/file_access_checker.go:32` · `structure` · *re-confirmed*
> STATUS: FIXED in #2020 — the interface and its `var _` assertion are gone; `rg FileAccessChecker` over the tree is empty, so there was no test fake keeping it alive either.
- **Fix:** Delete the FileAccessChecker interface and the var _ assertion; have filterByAccess/canEnumerateEntry in query_directory.go take *metadata.Service directly, or declare a small local interface in the smb/handlers package (consumer-side) if a test double is genuinely needed there.

### [LOW] CreateRootDirectory's UID/GID reconciliation on an existing root writes the file record without invalidating readCache/parentCache
- **Where:** `pkg/metadata/store/badger/shares.go:546` · `gaps`
> STATUS: FIXED in #1975 — CreateRootDirectory invalidates readCache and parentCache for the root; a re-attach with changed UID/GID served stale ownership before the fix.
- **Fix:** In CreateRootDirectory, after a successful commit, also invalidate s.readCache and s.parentCache for rootFile.ID.String() unconditionally (cheap even when nothing changed), alongside the existing shareCache.invalidate call.

### [LOW] Lazy-init mutex-guarded singleton wrappers add indirection with zero benefit
- **Where:** `pkg/metadata/store/badger/clients.go:310` · `bloat`
- **Fix:** Initialize clientStore/recoveryStore directly in NewBadgerMetadataStore's struct literal (mirroring how db itself is set) and drop clientStoreMu/recoveryStoreMu plus the getClientStore/getRecoveryStore indirection; call s.clientStore.Xxx(...) directly from the BadgerMetadataStore delegation methods.

### [LOW] Key-namespace doc table is missing 3 of the 12 prefixes defined right below it
- **Where:** `pkg/metadata/store/badger/encoding.go:35` · `comments`
- **Fix:** Add rows for cn: (reverse child-name edge), obj: (ObjectID index), pl: (PayloadID index) matching the const block.

### [LOW] Reset() does not zero usedBytes/quota counters, unlike the sqlite/postgres reference implementations — stale accounting survives a failed restore
- **Where:** `pkg/metadata/store/badger/reset.go:21` · `gaps`
> STATUS: REFUTED — Reset() already zeroes usedBytes and the quota cache, and failRestore is consistent given the empty-store precondition.
- **Fix:** Zero s.usedBytes and reset the quota cache directly inside Reset() (mirroring sqlite/postgres), and additionally do the same in failRestore before returning, so both call sites leave counters consistent with the DB's actual (empty) contents rather than depending on a downstream success path that may never run.

### [LOW] Comment narrates a past bug/fix instead of describing current behavior
- **Where:** `pkg/metadata/store/badger/shares.go:607` · `comments`
- **Fix:** Trim to a behavior statement, e.g. "Preserve existing share Options (e.g. from a prior CreateShare) when materializing the root row."

### [LOW] Mutex-guarded lazy-init singleton for substores that only need s.db, already available at construction
- **Where:** `pkg/metadata/store/badger/locks.go:537` · `bloat`
- **Fix:** Construct s.lockStore = newBadgerLockStore(s.db) and s.durableStore = newBadgerDurableStore(s.db) once in NewBadgerMetadataStore; delete initLockStore/getDurableStore, lockStoreMu/durableStoreMu, and every call site's guard call.

### [LOW] Line-by-line "Store/Index by/Delete" comments restate each Set/Delete call in lock persistence helpers
- **Where:** `pkg/metadata/store/badger/locks.go:95` · `comments`
- **Fix:** Drop the per-line labels; keep the substantive block comments (e.g. the lockIndexPrefix hex-encoding rationale at lines 31-50, or the iterator-close note at 235-236) which explain non-obvious behavior.

### [LOW] Dangling/truncated "Renamed from X" and "lifted from" comments describe past refactors, not current behavior
- **Where:** `pkg/metadata/store/badger/objects.go:124` · `comments`
- **Fix:** Delete the "Renamed from"/"lifted from" fragments; if the rename is worth documenting, put it in the commit message, not in-source.

### [LOW] "(review iteration 1):" comment is a leftover process note, not a behavior description
- **Where:** `pkg/metadata/store/badger/objects.go:887` · `comments`
- **Fix:** Drop the "(review iteration 1):" prefix; keep the substantive rollback-correctness explanation that follows it.

### [LOW] badgerTransaction.GetFileByPayloadID legacy-scan fallback returns File without its block manifest
- **Where:** `pkg/metadata/store/badger/transaction.go:1603` · `gaps`
> STATUS: FIXED in #1972 — the legacy scan branch returned without loading the manifest, so unindexed rows resolved with empty Blocks. #1972 routes both the tx- and store-level fallbacks through loadEnrichedFileByID, closing the drift that caused it.
- **Fix:** Add the same `if err := loadManifest(tx.txn, file); err != nil { return nil, err }` call before `result = file; return errFound` in the legacy-scan branch of badgerTransaction.GetFileByPayloadID (transaction.go, inside the item.Value closure around line 1658), matching files.go:217 and loadEnrichedFileByID's loadManifest call.

### [LOW] Redundant "Decode/Encode handle" comments restate the next line verbatim
- **Where:** `pkg/metadata/store/badger/transaction.go:232` · `comments`
- **Fix:** Delete these one-line comments; keep only comments where the decoded/encoded value's purpose is non-obvious (e.g. "share name reused for X").

### [LOW] Function re-exported via mutable package var instead of wrapper func
- **Where:** `pkg/metadata/object.go:19` · `structure`
- **Fix:** func ParseContentHash(s string) (ContentHash, error) { return block.ParseContentHash(s) } — a thin wrapper func, not a var.

### [LOW] Store interface is a god interface — ISP violation
- **Where:** `pkg/metadata/store.go:377` · `structure`
- **Fix:** Split into role interfaces callers actually consume (e.g. keep Files/Shares/ServerConfig/Transactor as-is, move block-manifest/GC-only methods — FindByObjectID, EnumerateFileChunks, EnumeratePayloads, EnumerateLivePayloadIDs, CommitBlock, GetQuotaUsage — behind a narrower GCStore/AdminStore interface consumers assert into, mirroring Resetable/Snapshotable.

### [LOW] Store hand-duplicates block.EngineFileChunkStore's method set instead of embedding it
- **Where:** `pkg/metadata/store.go:420` · `structure`
- **Fix:** Replace the embedded FileChunkStore in Store with block.EngineFileChunkStore; drop the three duplicated method decls.

### [LOW] testing package imported into production pkg/metadata build
- **Where:** `pkg/metadata/synced_hash_store_suite.go:18` · `structure`
- **Fix:** Move RunSyncedHashStoreSuite/RunSyncedHashEnumeratorSuite into pkg/metadata/storetest (or a _test.go-only file with an exported test-helper build) so testing never links into cmd/dfs.

### [LOW] LockManager interface bundles 30+ unrelated methods; consumers depend on the whole surface instead of interfaces-at-consumer
- **Where:** `pkg/metadata/lock/manager.go:31` · `structure`
- **Fix:** Define narrow interfaces at each consumer package (e.g. a LeaseBreaker interface in internal/adapter/smb/lease listing only the ~6 methods it calls) instead of importing lock.LockManager wholesale; Manager keeps implementing everything, only the declared dependency shrinks.

### [LOW] Exported mutable package-level slice globals
- **Where:** `pkg/metadata/lock/oplock.go:118` · `structure`
- **Fix:** Unexport to validFileLeaseStates/validDirectoryLeaseStates (already only consumed via the Is*Valid* helpers in this package) or convert to a func returning a fresh copy if external packages truly need the list.

### [LOW] Package doc comment restated verbatim in every file
- **Where:** `pkg/metadata/lock/oplock_break.go:1` · `structure`
- **Fix:** Keep one full package doc (e.g. in a doc.go or manager.go), trim the other 7 files to a one-line file-scoped comment describing just that file's contents.

### [LOW] RegisterClient hand-builds a StoreError instead of reusing the existing NewConnectionLimitError factory, losing adapter/limit context
- **Where:** `pkg/metadata/lock/connection.go:163` · `structure`
- **Fix:** Replace the inline struct literal with `return errors.NewConnectionLimitError(adapterType, limit)`.

### [LOW] NewGracePeriodError silently drops remainingSeconds
- **Where:** `pkg/metadata/lock/errors.go:75` · `slop` · *re-confirmed*
- **Fix:** Fold into Message: fmt.Sprintf("grace period active, new locks blocked (%ds remaining)", remainingSeconds), or add a RemainingSeconds field to StoreError.

### [LOW] Error constructors accept diagnostic params (blockedBy/current/max/remainingSeconds) then discard them
- **Where:** `pkg/metadata/lock/errors.go:66` · `structure`
- **Fix:** Either populate StoreError.Message from the params — e.g. fmt.Sprintf("%s lock limit exceeded (%d/%d)", limitType, current, max) and include remainingSeconds in the grace message — or drop the unused params from the signatures. Wire NewDeadlockError into an actual caller or delete it.

### [LOW] Operation is boolean-soup (IsReclaim/IsTest/IsNew) in a file that demonstrates the correct enum idiom two blocks above
- **Where:** `pkg/metadata/lock/grace.go:38` · `structure`
- **Fix:** Make Operation a single enum (OpNew/OpReclaim/OpTest iota, mirroring GraceState) or a NewOperation(kind) constructor; drop IsNew or actually branch on it (e.g. assert exactly one of the three is set).

### [LOW] generateFileHandle reimplements metadata.GenerateNewHandle
- **Where:** `pkg/metadata/store/memory/store.go:648` · `bloat`
- **Fix:** Drop generateFileHandle and call metadata.GenerateNewHandle everywhere in this package (the unused fullPath parameter can be dropped too).

### [LOW] postgresClientStore/postgresRecoveryStore lazy-init wrapper adds no value over the pool already held by PostgresMetadataStore
- **Where:** `pkg/metadata/store/postgres/clients.go:28` · `bloat`
- **Fix:** Construct clientStore/recoveryStore (and the other two, if same shape) directly in NewPostgresMetadataStore and drop the *Mu sync.Mutex + getXStore() lazy-init wrappers; or fold the tiny structs' methods directly onto PostgresMetadataStore and drop the wrapper types entirely.

### [LOW] Stale comment: describes a PrepareStatements default that is never applied anywhere
- **Where:** `pkg/metadata/store/postgres/config.go:82` · `comments`
- **Fix:** Delete the comment, or if the intended default was dropped, either implement `if !c.PrepareStatements { c.PrepareStatements = true }` in ApplyDefaults() or remove the comment entirely.

### [LOW] StatsCacheTTL config field defaulted and logged but never used to cache anything
- **Where:** `pkg/metadata/store/postgres/config.go:34` · `bloat`
- **Fix:** Delete the field (and its default/log line) or actually implement a TTL cache in GetFilesystemStatistics.

### [LOW] Dead narration comment describing indecision, no corresponding code
- **Where:** `pkg/metadata/store/postgres/connection.go:39` · `comments`
- **Fix:** Delete lines 39-41.

### [LOW] Step-by-step narration comments trivially restate the following line
- **Where:** `pkg/metadata/store/postgres/connection.go:13` · `comments`
- **Fix:** Remove these one-line restatement comments; keep only comments that explain *why* (e.g. the acquire-timeout rationale already present elsewhere in the package).

### [LOW] queryRow/query/exec/beginTx duplicate the same acquire-timeout-and-classify-deadline block four times
- **Where:** `pkg/metadata/store/postgres/pool_helpers.go:37` · `bloat`
- **Fix:** Extract a shared acquireConn(ctx, op, sql string) (*pgxpool.Conn, error) helper that does the timeout+classify dance once; queryRow/query/exec call it then do their pgx-specific call, beginTx calls s.pool.Begin(acquireCtx) directly since Begin has no separate acquire step exposed the same way (or extract just the classify-error part).

### [LOW] Redundant step-narration comments in CreateRootDirectory
- **Where:** `pkg/metadata/store/postgres/shares.go:306` · `comments`
- **Fix:** Delete these five restatement comments; keep the ones that explain non-obvious invariants (nlink, idempotent upsert, etc.).

### [LOW] GetBlockRecord/WalkBlockRecords duplicated tx-level vs store-level
> STATUS: DIFFED, no drift -- DEFERRED to #1828. Both copies were extracted and compared line by line before any collapse was considered. postgres `block_record_store.go`: GetBlockRecord `:46` vs `:115` and WalkBlockRecords `:68` vs `:132` are character-identical apart from the `tx.tx.QueryRow`/`s.queryRow` dispatcher -- same `ctx.Err()` guard, same 5-column SELECT, same error prefix in both (no divergent "tx" variant), and both delegate the row loop to the shared `iterBlockRecordRows`. Put/Delete/DecrLiveChunkCount are not duplicates at all: the store forms delegate through `WithTransaction`. Deferred to #1828 with the rest of the pool/tx seam.
- **Where:** `pkg/metadata/store/postgres/block_record_store.go:46` · `bloat`
- **Fix:** Factor the query string + scanBlockRecord/iterBlockRecordRows call into one function parameterized over the minimal query interface, called from both receivers.

### [LOW] Debug-log comment is a bare restatement of the line below it
- **Where:** `pkg/metadata/store/postgres/transaction.go:229` · `comments`
- **Fix:** Delete the comment (or the whole debug log line, since it's unconditional Debug logging on the hot GetFile path with no gate).

### [LOW] Debug-log comment is a bare restatement of the line below it
- **Where:** `pkg/metadata/store/postgres/transaction.go:443` · `comments`
- **Fix:** Delete the comment.

### [LOW] Debug-log comment is a bare restatement of the line below it
- **Where:** `pkg/metadata/store/postgres/transaction.go:553` · `comments`
- **Fix:** Delete the comment.

### [LOW] StatsCacheTTL config field is dead — parsed, defaulted, never read
- **Where:** `pkg/metadata/store/sqlite/config.go:31` · `bloat`
- **Fix:** Drop the field entirely; if statfs caching is ever added to sqlite, reintroduce it then.

### [LOW] scanBlockRecord / scanBlockRecordRow are a hand-duplicated pair that could delegate
- **Where:** `pkg/metadata/store/sqlite/block_record_store.go:274` · `bloat`
- **Fix:** Delete scanBlockRecordRow; in WalkBlockRecords call sites, do `rec, _, err := scanBlockRecord(rows)` directly since rows satisfies scanRow.

### [LOW] Redundant 'Debug logging' comment restates the obvious call beneath it
- **Where:** `pkg/metadata/store/sqlite/transaction.go:174` · `comments`
- **Fix:** Delete the comment (or, if the debug call itself is being kept intentionally for triage, replace with a comment that explains *why* this specific log point exists, not that it 'logs').

### [LOW] Redundant 'Debug logging' comment restates the obvious call beneath it
- **Where:** `pkg/metadata/store/sqlite/transaction.go:386` · `comments`
- **Fix:** Delete the comment.

### [LOW] Redundant 'Debug logging' comment restates the obvious call beneath it
- **Where:** `pkg/metadata/store/sqlite/transaction.go:496` · `comments`
> STATUS: FIXED in #1978 — three ad-hoc Debug logs gated on logger.Enabled; GetChild runs per path component.
- **Fix:** Delete the comment.

### [LOW] Package-level mutable global cache-size config instead of threading through store config
- **Where:** `pkg/metadata/store/badger/cache.go:69` · `structure`
> STATUS: WONTFIX — claim is accurate but the suggested fix is a net loss. The store-open path is the package-level runtime.CreateMetadataStoreFromConfig (init.go:55), whose second caller is the HTTP handler internal/controlplane/api/handlers/metadata_stores.go:87 — a runtime-added store, with no access to cfg.Metadata.Badger. Threading the operator defaults there means either a new parameter on an exported bootstrap function that the API handler cannot fill, or parking the two values on Runtime — the same re-introduce-state-on-Runtime move rejected for the netgroups.go finding. The global is written once in start.go before any store opens and read at open time, which is exactly what a process-wide operator default needs; the mutex and getter are the price.
- **Fix:** Resolve operator config once in cmd/dfs/commands/start.go and pass it through BadgerMetadataStoreConfig / the runtime/stores/service.go store-open path; drops the mutex, the getter, and the test reset boilerplate.

### [LOW] block_record_store.go: tx-level and store-level methods duplicate the same txn body instead of sharing a free function
> STATUS: DIFFED, no drift. badger `block_record_store.go`: Put `:50`/`:168`, Get `:58`/`:184`, Delete `:77`/`:213`, Walk `:88`/`:230`, Decr `:130`/`:275`. Key building, encode/decode, not-found handling, the floor-at-zero arithmetic and the walk order are identical in every pair. The asymmetries are structural, not drift, and uniform across all five: tx-level methods take `_ context.Context` (a tx cannot honour cancellation mid-transaction) while store-level ones add a `ctx.Err()` preflight, and store `DecrLiveChunkCount` wraps in `updateWithConflictRetry` where the tx form cannot, being already inside a txn. FIXED in passing: store `DecrLiveChunkCount` double-prefixed its not-found message (the closure and the outer wrap both prepended `badger DecrLiveChunkCount: `). The retry contract this pair depends on is now pinned by a `ConcurrentDecrLiveChunkCount` case in `pkg/metadata/storetest/block_record_ops.go` -- it loses 2 of 8 decrements if the retry is removed.
- **Where:** `pkg/metadata/store/badger/block_record_store.go:130` · `structure`
- **Fix:** Extract getBlockRecordTxn(txn *badgerdb.Txn, blockID string), deleteBlockRecordTxn(txn, blockID), walkBlockRecordsTxn(txn, fn), decrLiveChunkCountTxn(txn, blockID, delta) — mirror the synced_hash_store.go shape — and have both the badgerTransaction and BadgerMetadataStore methods call them.

### [LOW] ShareName not hex-encoded before building dh:share: index key — same injection class the package already flags/mitigates for MonName
- **Where:** `pkg/metadata/store/badger/durable_handles.go:125` · `security`
> STATUS: FIXED in #1971 — share name hex-encoded through a new shareIndexPrefix on put/delete/list, mirroring lockIndexPrefix. No migration needed.
- **Fix:** Hex-encode ShareName like monNameIndexPrefix does: prefixDHShare + hex.EncodeToString([]byte(handle.ShareName)) + ":" + handle.ID, consistently in putDurableHandleTx:125, deleteIndicesTx:160 and ListDurableHandlesByShare:478.

### [LOW] Redundant Get+Unmarshal of durable handle already decoded in caller
- **Where:** `pkg/metadata/store/badger/durable_handles.go:324` · `perf`
> STATUS: WONTFIX — SMB reconnect and the expiry janitor, not a per-request path.
- **Fix:** Add a deleteDurableHandleTx variant taking the already-decoded *lock.PersistedDurableHandle (straight to deleteIndicesTx + txn.Delete); keep the id-only form as a thin wrapper. Use it from consumeByIndexTx and keep the handle (not just the id) in the DeleteExpiredDurableHandles View pass.

### [LOW] durable_handles.go: secondary-index add/remove hand-duplicated per index type across put and delete
> STATUS: DIFFED, no drift. WONTFIX. `putDurableHandleTx:112-154` vs `deleteIndicesTx:156-209` cover the SAME five index families with the SAME three guards -- FileID and Share unconditional (both through the shared `fileIDIndexKey`/`shareIndexPrefix` helpers, so those two cannot drift at all), CreateGuid and AppInstanceId gated on `!= zeroGUID`, FileHandle gated on `len(MetadataHandle) > 0`. No index is added without a removal counterpart, so there is no stale-entry leak. The remove side's one extra block is the legacy unsuffixed `dh:fid:` cleanup at `:165-181`, deliberate migration handling with an owner check. Recording one latent hazard found while diffing, present identically on both sides and therefore NOT drift: the CreateGuid key is the only one with no `:{id}` suffix, and delete removes it with no owner check, unlike the legacy FileID key directly above it. Two live handles sharing a CreateGuid (client-supplied on SMB2 CREATE; the spec says unique, a buggy client can repeat it) would let deleting A orphan B's lookup -- a failed durable reconnect, not corruption. The owner-check at `:171-179` is the fix template if anyone touches it.
- **Where:** `pkg/metadata/store/badger/durable_handles.go:88` · `structure`
- **Fix:** Extract a single indexKeys(handle) [][]byte builder applying the same guards (CreateGuid != zeroGUID, AppInstanceId != zeroGUID, len(MetadataHandle) > 0) once; have putDurableHandleTx Set and deleteIndicesTx Delete over the same generated list.

### [LOW] Doc comment for SetMaxTransactionRetriesForTest glued onto InlineSyncCountForTest (copy-paste drift)
- **Where:** `pkg/metadata/store/badger/testhooks.go:3` · `slop`
- **Fix:** Move lines 3-8 (the SetMaxTransactionRetriesForTest doc block) down to sit directly above `func SetMaxTransactionRetriesForTest(n int) func()` at line 27, leaving only the InlineSyncCountForTest doc block (lines 9-13) above its function.

### [LOW] Retry-with-backoff on ErrConflict duplicated verbatim between two independent implementations
> STATUS: DIFFED, no drift. WONTFIX on the collapse. `objects.go:156` `updateWithConflictRetry` vs `transaction.go:110` `withTransaction`: max attempts (one shared `maxTransactionRetries` atomic), the backoff expression, the between-attempt `ctx.Err()` re-check, the `== ErrConflict` sentinel test and the `txnConflicts` counter are character-identical. Neither handles `ErrTxnTooBig`. Two apparent divergences were chased down and both are false alarms: (a) the exhaustion return differs in SHAPE -- `withTransaction` guards with `goerrors.Is(lastErr, ErrConflict)` before mapping, `updateWithConflictRetry` maps unguarded -- but `lastErr` can only be nil when the loop never runs (`maxTransactionRetries <= 0`, test-only), and `mapBadgerError(nil, ...)` returns nil, so BOTH return nil in the only reachable case; (b) `updateWithConflictRetry` never calls `syncIfRelaxed()`, which looks like a durability gap until you read `store.go:168-174`, where relaxed mode is DEFINED as deferring namespace-op fsyncs to the background syncer while `WithTransaction` -- the data-paired path -- syncs explicitly. Share/refcount/block-record writes are namespace ops. Documented policy, not drift. FIXED in passing: `transaction.go` claimed "Exponential backoff with jitter... grows exponentially up to ~50ms"; the schedule is linear `(2*attempt+1)ms` and the jitter term is a deterministic function of `attempt`, with no randomness at all. Comment corrected in both copies.
- **Where:** `pkg/metadata/store/badger/objects.go:157` · `structure`
- **Fix:** Extract one retryOnConflict(ctx, body func() error) error helper owning the loop, ctx check, backoff, s.txnConflicts increment and final error wrapping; have both withTransaction and updateWithConflictRetry call it.

### [LOW] Refcount mutators leak the raw badgerdb.ErrConflict sentinel across the store-interface boundary
- **Where:** `pkg/metadata/store/badger/objects.go:176` · `structure`
- **Fix:** In updateWithConflictRetry (or the shared retry helper) wrap the exhausted lastErr through mapBadgerError before returning, matching withTransaction:204-211.

### [LOW] PutFile probes fm: manifest key on every attr-only write forever, even after migration is complete
- **Where:** `pkg/metadata/store/badger/transaction.go:438` · `perf`
> STATUS: WONTFIX — a stale 'manifest externalized' flag would mean a manifest silently not written. Trading a data-loss risk for one point Get is the wrong side of the bargain.
- **Fix:** Track manifest-externalized state on the in-memory File (transient field set by loadManifest when it reads fm:, mirroring BlocksDirty) and skip the existence probe once set.

### [LOW] ListChildren does 2 extra point-Gets per directory entry (N+1)
- **Where:** `pkg/metadata/store/badger/transaction.go:795` · `perf`
> STATUS: DEFERRED to #1828 — a real N+1, but eliminating it needs a new batched store-interface method and a matching conformance-suite change across every backend.
- **Fix:** Split into an attrs-optional variant so the 2N extra Gets are paid only when the caller actually needs per-entry Attr (READDIRPLUS/SMB query-directory), not for plain READDIR, xattr enumeration, or the limit=1 is-empty probes.

### [LOW] withTransaction's retry loop never re-checks context cancellation between attempts
- **Where:** `pkg/metadata/store/badger/transaction.go:116` · `structure`
- **Fix:** Add `if err := ctx.Err(); err != nil { return err }` at the top of the for-loop body in withTransaction, same as updateWithConflictRetry already does.

### [LOW] EncodeFileHandle duplicates EncodeShareHandle's entire body
> STATUS: REFUTED -- there is no duplicated body. `types.go:41` is a one-line delegation: `return EncodeShareHandle(file.ShareName, file.ID)`. Byte-identical output by construction, no magic/version byte in either. A symbol-name similarity false positive.
- **Where:** `pkg/metadata/types.go:41` · `bloat`
- **Fix:** func EncodeFileHandle(file *File) (FileHandle, error) { return EncodeShareHandle(file.ShareName, file.ID) }

### [LOW] PutClientRegistration hand-inlines the exact struct copy cloneClientRegistration already performs
- **Where:** `pkg/metadata/store/memory/clients.go:46` · `structure`
- **Fix:** Replace lines 47-57 with `s.registrations[reg.ClientID] = cloneClientRegistration(reg)`.

### [LOW] GetFileByPayloadID is an unindexed O(n) full-store scan (store and tx variants)
- **Where:** `pkg/metadata/store/memory/files.go:362` · `perf`
> STATUS: WONTFIX — the memory store clones itself entirely on every mutation by design; one O(n) among many changes nothing.
- **Fix:** Add payloadIndex map[metadata.PayloadID]string alongside objectIndex, maintained in tx.PutFile/tx.DeleteFile the same way, and resolve GetFileByPayloadID through it.

### [LOW] Store-level read CRUD methods duplicate transaction-level methods body-for-body instead of sharing a *Locked helper
- **Where:** `pkg/metadata/store/memory/files.go:123` · `structure`
> STATUS: FIXED in #2018 — getFileLocked / getChildLocked / getParentLocked / getLinkCountLocked / listChildrenLocked now hold the single body; the store methods take RLock and the transaction methods (transaction.go:242/402/451/459/477) call the same helpers.
> DRIFT (found on re-check, closed silently by that collapse): the pair was NOT byte-identical. Diffing the two copies at 47449ef3^ shows GetFile/GetChild/GetParent/GetLinkCount differing only in receiver and lock acquisition, but the transaction-level ListChildren omitted the link-count reconciliation the store-level copy performed (`if nlink, ok := store.linkCounts[childKey]` / dir→2 / file→1), so a listing taken inside a transaction reported the raw stored Nlink and lost the multi-link signal. #2018 collapsed onto the store-level copy, which is the correct one, so the drift is gone — but nothing pinned it: the existing conformance case HardLinkListChildrenShowsNlinkGT1 only drives the store-level surface, and the tx surface had no production caller (only storetest/ea_ops.go), which is why it went unnoticed. Added HardLinkTxListChildrenShowsNlinkGT1 to pkg/metadata/storetest so a future re-split cannot regress it; verified it fails when the reconciliation is removed, and passes on memory/badger/sqlite as shipped.
- **Fix:** Factor each duplicated pair into a *Locked helper (getFileLocked, getChildLocked, getParentLocked, ...) called from both the store method (under RLock) and the tx method (lock already held), matching the FileChunkStore *Locked precedent in objects.go.

### [LOW] Three near-identical lazy sub-stores double-lock with a dead inner mutex; a fourth uses the opposite (correct) discipline
- **Where:** `pkg/metadata/store/memory/locks.go:24` · `structure`
> STATUS: FIXED in #2018 — memoryLockStore's duplicate ten-method lock.LockStore set and its shadow mutex are deleted, as are the inner mutexes on memoryClientStore/memoryRecoveryStore. memoryDurableStore keeps its own lock and is documented as the deliberate exception: getDurableStore releases the store-wide mutex before the method runs, so that lock is the sole guard there. NOTE for anyone re-reading this finding: only the sub-store's copy was dead. MemoryMetadataStore's own ten lock methods (locks.go:118-252) are the live lock.LockStore implementation — pinned by the var _ assertion at locks.go:104, embedded in metadata.Transaction (store.go:263), and reached in production from pkg/metadata/service.go:387, which type-asserts the live store to lock.LockStore and wires it into the lock manager. Deleting those is a compile error and would silently disable lock persistence on the memory backend.
- **Fix:** Pick one discipline: drop the redundant inner sync.RWMutex from memoryClientStore/memoryRecoveryStore (never reached without s.mu held) and delete memoryLockStore's uncalled method set + mutex, keeping the store-wide lock as sole guard, mirroring the *Locked helper pattern.

### [LOW] derivePathLocked does O(fanout) child-name scan per ancestor level on every GetFile call
- **Where:** `pkg/metadata/store/memory/store.go:557` · `perf`
> STATUS: WONTFIX — same: the memory store's whole-store clone per mutation dominates.
- **Fix:** Maintain a parentName side index (map handleKey→edge name) updated in SetChild/DeleteChild alongside `parents`, so derivePathLocked does an O(1) lookup per level instead of childNameLocked's full childrenMap scan.

### [LOW] WithTransaction clones the entire store (all maps, including every directory's children) on every single mutating call
- **Where:** `pkg/metadata/store/memory/transaction.go:87` · `perf`
> STATUS: WONTFIX — same: the memory store's whole-store clone per mutation dominates.
- **Fix:** Copy-on-write only the map entries the closure touches (record mutated keys and restore just those), or at minimum stop maps.Clone-ing children[k] for directories the closure never touches (transaction.go:108-110).

### [LOW] Two divergent code paths mint file handles for the same concept depending on tx vs non-tx caller
- **Where:** `pkg/metadata/store/memory/transaction.go:605` · `structure`
- **Fix:** Have tx.GenerateHandle call tx.store.generateFileHandle(shareName, path) (dropping the unused metadata.GenerateNewHandle call), or vice versa — consolidate on one minting function.

### [LOW] PrepareStatements config knob is validated/logged but never wired to the pgx pool
- **Where:** `pkg/metadata/store/postgres/config.go:32` · `bugs` · *re-confirmed*
> STATUS: FIXED in #1949 — renamed DisablePreparedStatements and wired to DefaultQueryExecMode.
- **Fix:** Either wire cfg.PrepareStatements into poolConfig.ConnConfig.DefaultQueryExecMode (e.g. pgx.QueryExecModeSimpleProtocol when false) in createConnectionPool, and set the documented true default in ApplyDefaults; or remove the field/claims if it's not meant to be functional.

### [LOW] Raw-connection-acquire-with-timeout boilerplate duplicated instead of factored into pool_helpers.go
- **Where:** `pkg/metadata/store/postgres/reset.go:23` · `structure`
- **Fix:** Add `acquireConn(ctx) (*pgxpool.Conn, error)` next to query/exec/queryRow/beginTx in pool_helpers.go; call it from reset.go and both snapshot_store.go sites.

### [LOW] GetFile's 20+ column SQL literal duplicated between pool-path and tx-path instead of a shared const
> STATUS: DIFFED, no drift -- DEFERRED to #1828. Both copies were extracted and compared line by line before any collapse was considered. postgres `transaction.go:206` vs `files.go:29`, and the sqlite twin `transaction.go:151` vs `files.go:28`: after normalizing the receiver and the `?N`/`$N` dialect, the only hunks are the func signature and a debug-log block the tx copy carries and the pool copy does not. Same 24 columns in the same order; both interpolate the SAME package-level `inodePathExpr` and `blockRefsAggExpr` constants, so the subqueries cannot drift; both delegate every scan target to `sqlcodec.FileRowToFileWithNlinkAndBlocks(row, true)`, so there is no per-copy scan list to drop a column from. Same `ctx.Err()` guard, same handle decode, same error mapping. sqlite and postgres also agree with each other. The duplication is query TEXT whose risky half was already factored out; collapsing it by hand now would churn the exact files #1828 rewrites into a single `sql` implementation over a Dialect.
- **Where:** `pkg/metadata/store/postgres/transaction.go:210` · `structure`
- **Fix:** Extract the GetFile SELECT to a package-level const (getFileQuery) the way locks.go does for selectByID, and reuse it from PostgresMetadataStore.GetFile and postgresTransaction.GetFile; same for the other pool/tx pairs in files.go/transaction.go.

### [LOW] GetClientRegistration and ListClientRegistrations duplicate the same 9-field Scan block instead of sharing a scan helper
> STATUS: DIFFED, no drift -- DEFERRED to #1828. Both copies were extracted and compared line by line before any collapse was considered. sqlite `clients.go` Get vs the List loop body, and the postgres twin: identical field order, identical `len(privBytes) == 16` guard, identical no-rows handling (`sql.ErrNoRows` / `pgx.ErrNoRows` -> `nil, nil`). No drift. The sibling finding on `scanDurableHandleRows` in the same package was already collapsed onto `scanDurableHandleFields`, so the pattern to imitate exists; deferred to #1828 rather than applied piecemeal to one of four backends.
- **Where:** `pkg/metadata/store/sqlite/clients.go:70` · `structure`
- **Fix:** Extract `scanRegistration(row scanRow) (*lock.PersistedClientRegistration, error)` (mirroring scanDurableHandle) and call it from both Get and the List loop.

### [LOW] scanDurableHandleRows duplicates scanDurableHandle's whole 32-column scan instead of delegating through the shared scanRow interface
- **Where:** `pkg/metadata/store/sqlite/durable_handles.go:93` · `structure` · *re-confirmed*
- **Fix:** Replace the loop body with h, err := scanDurableHandle(rows) (scanRows satisfies scanRow's Scan(dest ...any) error), guarding the nil return; collapses ~58 lines to ~6.

### [LOW] Four structurally identical lazy-init-with-mutex getters for trivial zero-cost sub-store wrappers
- **Where:** `pkg/metadata/store/sqlite/durable_handles.go:401` · `structure`
- **Fix:** Construct all four sub-stores eagerly in NewSQLiteMetadataStore once db is open (deletes 4 mutexes + 4 nil-check getters); or collapse to one generic lazyInit helper if lazy is kept.

### [LOW] Migration comment references hallucinated function names / wrong file
- **Where:** `pkg/metadata/store/sqlite/migrations/000005_file_timestamps_filetime.up.sql:3` · `slop`
- **Fix:** Fix the comment to reference pkg/metadata/store/internal/sqlcodec/sqlcodec.go's TimeToFiletime (:56) / FiletimeToTime (:66).

### [LOW] RestoreSnapshot cell decoder allocates buffer from unvalidated length before reading, unbounded on a corrupt/truncated stream
- **Where:** `pkg/metadata/store/sqlite/snapshot_store.go:476` · `bugs`
> STATUS: FIXED in #1971 — readSized no longer allocates from an unverified u32 length.
- **Fix:** Bound `n` before allocating -- reject if n exceeds a fixed max cell size, or read via io.CopyN into a growable buffer instead of make([]byte, n) up front.

### [LOW] RestoreSnapshot decodes untrusted length-prefixed cell data with no size cap, before CRC verification
- **Where:** `pkg/metadata/store/sqlite/snapshot_store.go:471` · `security`
> STATUS: FIXED in #1971 — same readSized change.
- **Fix:** Cap per-cell text/blob length before allocating (fixed constant), and/or CRC-verify the payload stream before parsing rows.

### [LOW] block_record_store.go duplicates full SQL bodies between pool and tx paths instead of sharing via execer, unlike sibling synced_hash_store.go
> STATUS: DIFFED, no drift -- DEFERRED to #1828. Both copies were extracted and compared line by line before any collapse was considered. sqlite `block_record_store.go`: Put `:36`/`:138`, Get `:57`/`:159`, Delete `:72`/`:174`, Walk `:85`/`:187`. Each pair diffs to exactly one hunk (the signature) after normalizing the receiver, the exec/query dispatcher, and the `"sqlite tx "` vs `"sqlite "` error prefix. Put binds all five args including `rec.Length` in both; both Walk copies share `scanBlockRecord`. One cross-dialect asymmetry, not a drift: both sqlite Walk copies discard the `found` bool (`:98`, `:198`) where postgres's shared iterator skips on `!ok`. Harmless -- `scanBlockRecord` only reports `found=false` on `sql.ErrNoRows`, unreachable inside a `rows.Next()` loop -- and identical between the two sqlite copies. Deferred to #1828.
- **Where:** `pkg/metadata/store/sqlite/block_record_store.go:36` · `structure`
- **Fix:** Factor blockRecordPut(ctx, execer, rec)/blockRecordGet/blockRecordDelete/blockRecordWalk helpers (mirroring syncedMark et al.) and have both the store-level and tx-level methods call them, same as objects.go already does for decrementAndReapTx/scanFileChunk.

### [LOW] SetParent's ctx-cancellation handling diverges between the store-level and tx-level implementations
- **Where:** `pkg/metadata/store/sqlite/files.go:162` · `structure`
- **Fix:** Add `if err := ctx.Err(); err != nil { return err }` to sqliteTransaction.SetParent for consistency with its store-level sibling and the rest of the package.

### [LOW] Unconditional, eagerly-evaluated Debug logging on every transactional GetFile/GetChild call allocates on the hottest metadata path
- **Where:** `pkg/metadata/store/sqlite/transaction.go:496` · `perf`
> STATUS: FIXED in #1978 — three ad-hoc Debug logs gated on logger.Enabled; GetChild runs per path component.
- **Fix:** Drop these ad-hoc debug logs, or gate behind `if tx.store.logger.Enabled(ctx, slog.LevelDebug)` so the String()/boxing cost is paid only when Debug is on.

### [LOW] sqliteTransaction Files/Shares/ServerConfig impls split into one 1344-line transaction.go, unlike every other domain
- **Where:** `pkg/metadata/store/sqlite/transaction.go:185` · `structure`
- **Fix:** Split transaction.go by domain next to its store-level sibling: file/dir tx methods into files.go, share/root-dir into shares.go, server-config/caps into a small config file; keep only WithTransaction + the sqliteTransaction struct in transaction.go.

### [LOW] RemoveFile falls back to linkCount=1 on GetLinkCount error, risking premature content deletion
- **Where:** `pkg/metadata/file_remove.go:171` · `gaps`
> STATUS: FIXED in #1975 — an earlier revision of this file marked it fixed by #1901; that was wrong. #1901 only made the SQL backends propagate GetLinkCount errors, which makes the `linkCount = 1` guess MORE reachable, not less. The fallback survived on develop verbatim. #1975 returns the error instead, which is safe because backends report a missing count as (0, nil) rather than an error.
- **Fix:** Do not fall back to 1 on error; return lcErr (aborting the transaction) so the caller retries instead of risking content being reaped while another hard link remains.

### [LOW] Stale process-artifact text embedded in doc comment
- **Where:** `pkg/metadata/tx_context.go:14` · `comments`
- **Fix:** Drop the "(review iteration 1):" prefix; keep the rest as plain prose, e.g. "Wired by common.CopyPayload (and any future caller that needs to bind engine-level RefCount mutations to its WithTransaction-owned tx)."

### [LOW] READ handler: two near-identical empty-response blocks (empty file vs offset>=EOF)
- **Where:** `internal/adapter/nfs/v3/handlers/read.go:215` · `bloat`
- **Fix:** Merge into one guard: `if file.PayloadID == "" || req.Offset >= file.Size { ... }` with a single log call, dropping the separate 'empty file' branch.

### [LOW] Circular/tautological comment restating its own subject
- **Where:** `internal/adapter/nfs/v3/handlers/read.go:248` · `comments`
- **Fix:** Delete the comment, or replace with something that adds real info, e.g. `// common.ReadFromBlockStore resolves and reads the payload chunk range.` — only if it's not already obvious from the call below.

### [LOW] WRITE offset+length overflow checked twice via two different mechanisms
- **Where:** `internal/adapter/nfs/v3/handlers/write.go:184` · `bloat`
- **Fix:** Drop the redundant safeAdd overflow branch in Write() (or drop the manual check in validateWriteRequest) and rely on a single overflow check, done once after any short-write clamping.

### [LOW] commit.go duplicates CompoundResult boilerplate and even bypasses its own existing helper
- **Where:** `internal/adapter/nfs/v4/handlers/commit.go:22` · `bloat`
- **Fix:** Rename/reuse `encodeCommit4resError` (or add a thin `commitErr(status)` matching the package convention) and route all 10 error returns through it.

### [LOW] read.go re-implements CompoundResult error boilerplate 14x instead of package-convention xxxErr helper
- **Where:** `internal/adapter/nfs/v4/handlers/read.go:24` · `bloat`
> STATUS: FIXED in #2000 — same change as the `structure` duplicate of this finding above.
- **Fix:** Add `func readErr(status uint32) *types.CompoundResult { return &types.CompoundResult{Status: status, OpCode: types.OP_READ, Data: encodeStatusOnly(status)} }` and replace all 14 inline literals with `return readErr(...)`.

### [LOW] Package doc contradicts actual segmentation logic — describes fallback path as the primary one
- **Where:** `internal/adapter/nfs/v4/handlers/read_plus.go:47` · `comments`
- **Fix:** Update lines 43-49 to state the primary source is the block-store engine's DataExtents view, with block.Segments as the fallback when the registry/engine can't be resolved — matching the accurate explanation already present at lines 149-156.

### [LOW] write.go re-implements CompoundResult error boilerplate ~14x instead of package-convention xxxErr helper
- **Where:** `internal/adapter/nfs/v4/handlers/write.go:72` · `bloat`
- **Fix:** Add `func writeErr(status uint32) *types.CompoundResult { return &types.CompoundResult{Status: status, OpCode: types.OP_WRITE, Data: encodeStatusOnly(status)} }` and replace the inline literals with it.

### [LOW] bootVerifierBytes nil-guard is dead defensive code for a state the code guarantees can't happen
- **Where:** `internal/adapter/nfs/v4/handlers/write.go:56` · `bloat`
- **Fix:** Drop the nil check and dereference directly: `return *serverBootVerifier.Load()`. If genuinely worried about init ordering, that's a real bug to fix, not paper over.

### [LOW] EnableAdapter persists Enabled=true before start succeeds, no rollback on failure
- **Where:** `pkg/controlplane/runtime/adapters/service.go:208` · `bugs`
> STATUS: FIXED in #1971 — EnableAdapter rolls back the persisted flag on start failure, using a fresh bounded context.
- **Fix:** On startAdapter failure revert cfg.Enabled=false and persist it best-effort (log rollback failure) before returning the original error, mirroring CreateAdapter's rollback at lines 96-109.

### [LOW] adapterEntry.ctx is a write-only dead field
- **Where:** `pkg/controlplane/runtime/adapters/service.go:46` · `structure` · *re-confirmed*
- **Fix:** Drop the `ctx` field from adapterEntry; keep cancel/errCh.

### [LOW] DefaultShutdownTimeout constant duplicated verbatim in three packages
- **Where:** `pkg/controlplane/runtime/adapters/service.go:17` · `bloat`
- **Fix:** Keep one definition (runtime.DefaultShutdownTimeout is the public one callers use) and have adapters/lifecycle reference it instead of re-declaring, or hoist it to a shared low-level package both import.

### [LOW] Comment embeds issue-tracker reference (#1245 Bug D)
- **Where:** `pkg/controlplane/runtime/stores/service.go:188` · `comments`
- **Fix:** Rewrite in behavioural terms, e.g.: "Temp/snapshot/probe stores inherit the process-wide cache default (SetGlobalBadgerCacheDefaults) and RAM-relative auto-sizing via buildBadgerOptions instead of a per-store override, avoiding cache-size thrashing across short-lived instances." Drop the "#1245 Bug D" citation entirely.

### [LOW] Comment embeds a phase/plan reference (Phase-5)
- **Where:** `pkg/controlplane/runtime/stores/service.go:235` · `comments`
- **Fix:** Drop "Phase-5": "CreatedAt is derived from the ULID suffix embedded in the schema name (temp schemas use the form `<origSchema>_restore_<ulid>`)."

### [LOW] sharePrefix parameter is dead weight — no caller ever passes non-empty
- **Where:** `pkg/controlplane/runtime/blockgc.go:50` · `bloat`
- **Fix:** Drop the sharePrefix parameter from RunBlockGC and runBlockGCSweep, delete the WARN-and-ignore block, and delete the now-pointless blockgc_test.go case that exercises it.

### [LOW] singleShareReconciler duplicates perRemoteReconciler for the single-share case
> STATUS: PREMISE CORRECTED, and a real DRIFT found behind it -- FIXED. No `singleShareReconciler` type exists; `perRemoteReconciler` (`blockgc.go:243`) is constructed at three sites and is not duplicated. The genuinely duplicated bodies are `runBlockGCSweep:74` (server-wide) and `runBlockGCForShare:276` (share-scoped). Diffing those two surfaced one divergence that was NOT intentional: the server-wide sweep folded its remote-tier result with `accumulateGCStats(total, stats, false)` while the share-scoped twin passed `true` -- and so did the local-tier fold inside the SAME function, so one function mixed both conventions. The flag gated `DryRun`, `DryRunCandidates` and `FirstErrors`. `FirstErrors` is not dry-run metadata: it is the curated, class-diversified error sample (`engine/gc.go:737-780`) that is the ONLY cause detail `dfsctl store block gc` and `gc status` render. So on every server-wide run -- the GC scheduler and `dfsctl store block gc --reconcile` -- remote-tier GC failures reported as a bare `errors: N` with the causes discarded one level down, while `ErrorCount` still incremented. `blockgc_reconcile.go:82` folded the sweep result with `true`, clearly expecting the detail to be there. Fixed by propagating unconditionally and DELETING the `includeDryRunMeta` parameter, which now had one value at every call site -- the dead flexibility is what let the two copies disagree. Pinned by `TestRunBlockGC_PropagatesRemoteTierErrorDetail`.
- **Where:** `pkg/controlplane/runtime/blockgc.go:247` · `bloat`
- **Fix:** Delete singleShareReconciler; replace the line-231 use with perRemoteReconciler{rt: r, shares: []string{entry.ShareName}}.

### [LOW] ValidateBlockStoreConfig never shape-checks the "encryption" sub-config, unlike compression/parallel_uploads
- **Where:** `pkg/controlplane/runtime/blockstore_init.go:71` · `gaps`
> STATUS: FIXED in #1975 — validateEncryptionSubconfig reuses encryption.ParsePolicy so shapes and AEAD names cannot drift from the attach path, and checks each provider kind's required fields including the KMIP client cert pair.
- **Fix:** Add a validateEncryptionSubconfig(config map[string]any) error mirroring validateCompressionSubconfig: if config["encryption"] is absent, return nil (opt-in); otherwise require a JSON object, validate an optional "aead" string against the three known values, and validate "key.kind" is "local" (requires "file") or "kmip" (requires "endpoint"/"key_uid"). Call it in the s3 case next to validateCompressionSubconfig/validateParallelUploads (blockstore_init.go line ~94-98).

### [LOW] DiscoveryName takes no ctx and calls context.Background() for its store I/O
- **Where:** `pkg/controlplane/runtime/discovery.go:20` · `structure`
> STATUS: FIXED — DiscoveryName(ctx) threads the caller's context into GetSetting. The call sites had no context to pass, so auxsvc.Group.Reconcile now hands its already-stored base context (the adapter's Serve context) to the sidecar builder, and the NFS/SMB discoveryName helpers take it from there.
- **Fix:** Add a ctx context.Context parameter to DiscoveryName and thread it through to GetSetting; update the (presumably few) call sites.

### [LOW] MetricsSnapshot N+1 DB query: ListSnapshots called per-share every scrape
- **Where:** `pkg/controlplane/runtime/metrics.go:27` · `perf`
> STATUS: WONTFIX — per-scrape with N = shares; batching it needs a new store API.
- **Fix:** Add batch store.ListSnapshotsForShares(ctx, shareNames) (or ListAllSnapshots) and bucket in memory; same batching for the quota-usage loop.

### [LOW] Package-level mutable global DNS cache instead of a Runtime-scoped field
> STATUS: WONTFIX -- the fix would re-introduce exactly the NFS-specific state that the comment two lines above records as deliberately moved off `Runtime`. Netgroup matching is host-keyed and Runtime-independent, so process-wide sharing is harmless. Marked with a `ponytail:`.
- **Where:** `pkg/controlplane/runtime/netgroups.go:24` · `structure`
- **Fix:** Scope the cache to a small netgroup-checking helper struct owned per-Runtime (lazily built with sync.Once as a Runtime field, not a package var), or inject it via New().

### [LOW] Runtime struct is a god object mixing unrelated concerns
- **Where:** `pkg/controlplane/runtime/runtime.go:60` · `structure`
- **Fix:** Move snapInFlight/snapDeleteLocks/restoreLocks/remoteGCLocks into a small snapshot-ops sub-service; move ldapConfig/netlogonCredential/identity callbacks into identity.Service, matching the existing sub-service composition already used for adapters/stores/shares.

### [LOW] RemoveShare has no ctx parameter, fabricates one internally with a hardcoded timeout
> STATUS: WONTFIX -- the detached context is load-bearing, not an oversight. It covers only the best-effort `user_grace` orphan-row purge that runs AFTER the share is already torn down; binding it to an API request context would let a client disconnect abort the purge and leak the rows it exists to prevent, with nothing left to retry it. Rationale now recorded in the source comment.
- **Where:** `pkg/controlplane/runtime/runtime.go:567` · `structure`
- **Fix:** Add ctx context.Context as RemoveShare's first parameter, propagate it to sharesSvc.RemoveShare / PurgeDefaultUserGrace instead of constructing a fresh background context with a hardcoded timeout.

### [LOW] Keyed-mutex-registry pattern copy-pasted 3x with no shared type
- **Where:** `pkg/controlplane/runtime/runtime.go:129` · `structure`
- **Fix:** Extract a small `keyedMutex struct{ mu sync.Mutex; m map[string]*sync.Mutex }` with a `Get(key string) *sync.Mutex` method; use it for all three registries (RWMutex variant needs generics or a second type).

### [LOW] Cross-package magic-string constant duplicated to dodge an import cycle
- **Where:** `pkg/controlplane/runtime/runtime.go:1210` · `structure`
- **Fix:** Extract the provider-key constants into a leaf package with no dependency on either adapter or runtime (e.g. a tiny `adapterkeys` package) and import it from both sides.

### [LOW] GetShareIdentityInfo error swallowed and replaced with fabricated "not found" message
- **Where:** `pkg/controlplane/runtime/identity/service.go:30` · `bugs`
> STATUS: FIXED in #1971 — the underlying error is wrapped instead of replaced with a fabricated message.
- **Fix:** Wrap instead of discard: return nil, fmt.Errorf("share %q: %w", shareName, err).

### [LOW] Error swallows wrapped cause, drops %w
- **Where:** `pkg/controlplane/runtime/identity/service.go:32` · `structure`
- **Fix:** `return nil, fmt.Errorf("share %q not found: %w", shareName, err)`.

### [LOW] Client registry has no update path for per-client protocol/identity fields — permanently stuck at connect-time stub values
- **Where:** `pkg/controlplane/runtime/clients/service.go:62` · `gaps`
> STATUS: TRACKED on #1962 — ClientRecord has no update path at all; entries are write-once apart from the LastActivity atomic. Closing it needs new Registry mutators plus wiring from both adapters, so it widens #1962 rather than being separable.
- **Fix:** Add Registry methods to merge-update an existing entry post-registration, e.g. `UpdateUser(clientID, user string)`, `UpdateNFSDetails(clientID string, fn func(*NfsDetails))`, `UpdateSMBDetails(clientID string, fn func(*SmbDetails))` (same read-lock-and-mutate-in-place pattern as AddShare/RemoveShare), and wire adapters to call them once the real version/dialect/signing/identity is known (NFS: per-call from AuthContext/call.Version; SMB: after NEGOTIATE and SESSION_SETUP).

### [LOW] Long parameter list / positional dependency injection across three methods
- **Where:** `pkg/controlplane/runtime/lifecycle/service.go:232` · `structure`
> STATUS: FIXED in #2021 — Serve now takes a named lifecycle.Deps struct (service.go:229-256) carrying the six shutdown collaborators.
- **Fix:** Collect the six shutdown collaborators into a single `Deps`/`Config` struct passed once to `New()` or `Serve()`, e.g. `type Deps struct { Settings SettingsInitializer; Adapters AdapterLoader; ... }`. Removes positional-arg fragility and makes future additions additive instead of signature-breaking.

### [LOW] First-boot machine-SID persist failure is silently swallowed, breaking the documented restart-stability invariant
- **Where:** `pkg/controlplane/runtime/lifecycle/service.go:202` · `gaps`
> STATUS: TRACKED as #1977 — confirmed swallowed, but making it fatal would stop the server starting for every other protocol on a transient hiccup while registering one adapter. Policy call, deliberately not taken in a sweep.
- **Fix:** Treat a first-boot persist failure as fatal to Serve() (return the error, matching the pinned-SID invalid-pin precedent already in this function) rather than continuing with an in-memory-only SID; or, at minimum, retry/verify the write and refuse to start serving SMB identity mapping until the SID is durably stored.

### [LOW] Deprecated NFSClientProvider shim duplicates the generic adapter-provider API it says to use instead
- **Where:** `pkg/controlplane/runtime/runtime.go:1232` · `bloat`
- **Fix:** Replace the 4 call sites with SetAdapterProvider("nfs", ...)/GetAdapterProvider("nfs") and delete SetNFSClientProvider/NFSClientProvider.

### [LOW] pollNFSSettings and pollSMBSettings duplicate the entire poll/version-check/swap/notify sequence
> STATUS: DIFFED, no drift. WONTFIX. `settings_watcher.go:196` vs `:266`: with the protocol token normalised away the bodies differ only in the `logger.Info` field list, one comment and a blank line. Both do `errors.Is(err, models.ErrAdapterNotFound) -> return nil`, both read the cached version under RLock and compare with `!=` (not `>`), both swap under a separate Lock, both gate the log AND the callback fan-out on `currentVersion > 0`, and both copy the callback slice under RLock before invoking outside the lock. They also share the same benign version-read/swap TOCTOU and the same first-successful-poll-does-not-notify behaviour -- equally, so no drift. Two ~35-line bodies whose only real difference is the protocol they name; a generic collapse would need the settings type parameterised for a ~30-line saving.
- **Where:** `pkg/controlplane/runtime/settings_watcher.go:196` · `bloat`
- **Fix:** Factor the common shape into a generic helper parameterized by adapter name, a getter closure, and a log/notify closure (e.g. `func pollAdapterSettings[T any](ctx, adapterName string, load func() (T, int, error), onChanged func(T))`), leaving each poll* wrapper to supply only its type-specific bits.

### [LOW] Redundant comment restates the line immediately below it (NFS adapter fetch)
- **Where:** `pkg/controlplane/runtime/settings_watcher.go:197` · `comments`
- **Fix:** Delete the comment; the call is self-explanatory.

### [LOW] Redundant comment restates the line immediately below it (SMB adapter fetch)
- **Where:** `pkg/controlplane/runtime/settings_watcher.go:268` · `comments`
- **Fix:** Delete the comment; the call is self-explanatory.

### [LOW] mountKey uses naive colon-join → composite key collisions between distinct (protocol, client, share) tuples
- **Where:** `pkg/controlplane/runtime/mounts/service.go:29` · `bugs`
> STATUS: FIXED in #1971 — map keyed by a struct instead of a concatenated string.
- **Fix:** Use a struct{Protocol, ClientAddr, ShareName string} as the map key directly, or length-prefix the components.

### [LOW] dnsResolver interface — one-impl abstraction kept only for a test fake
> STATUS: WONTFIX -- the suggested replacement (package-level `lookupAddrFn`/`lookupHostFn` vars the tests overwrite) swaps a typed 2-method seam for mutable global state shared across parallel tests. Net loss. Marked with a `ponytail:` at the interface.
- **Where:** `pkg/controlplane/runtime/netgroups.go:48` · `bloat`
- **Fix:** Drop the interface; have matchHostname/forwardConfirms take *dnsCache directly, or swap to package-level func vars (lookupAddrFn/lookupHostFn) that tests override — smaller than a 2-method interface + fake struct.

### [LOW] Redundant step comment restates trivial code
- **Where:** `pkg/controlplane/runtime/netgroups.go:191` · `comments`
- **Fix:** Delete the inline `// 2.` comment; the docstring Algorithm list already documents the step.

### [LOW] Redundant comment restates the function call it precedes
- **Where:** `pkg/controlplane/runtime/netgroups.go:220` · `comments`
- **Fix:** Delete the comment; `ensureDNSCache()` is self-documenting.

### [LOW] Trivial step comment restates the return it labels
- **Where:** `pkg/controlplane/runtime/netgroups.go:246` · `comments`
- **Fix:** Delete; if a comment is wanted, fold it into the loop instead of a standalone line on a bare return.

### [LOW] sharedRemote.configID is a write-only dead field
- **Where:** `pkg/controlplane/runtime/shares/service.go:377` · `bloat`
- **Fix:** Drop the `configID` field from `sharedRemote` and the two assignment sites (or, if it's meant as a self-describing invariant check, add an actual read/assert that uses it — otherwise delete it per the repo's eager dead-code-removal rule).

### [LOW] Two package doc comments in pkg/.../shares — errors.go's competes with doc.go's
- **Where:** `pkg/controlplane/runtime/shares/errors.go:1` · `structure`
- **Fix:** Drop or de-package-ify errors.go:1 (blank line after it, or reword to non-doc), leaving doc.go the sole package comment.

### [LOW] RebindShareBlockStore does not cancel in-flight warm jobs before tearing down the old block store
- **Where:** `pkg/controlplane/runtime/shares/service.go:1348` · `bugs`
> STATUS: FIXED in #1971 — cancelForShare runs before drain/close and after pre-validation.
- **Fix:** Call s.warmJobs.cancelForShare(name) in RebindShareBlockStore before oldBS.DrainAllUploads/Close, mirroring RemoveShare's ordering.

### [LOW] Service mixes generic registry, engine factory, and Postgres-specific schema admin — SRP violation
- **Where:** `pkg/controlplane/runtime/stores/service.go:23` · `structure`
> STATUS: FIXED in #2019 — SwapMetadataStore, OpenMetadataStoreAtPath, ListPostgresRestoreOrphans, DropPostgresSchema, openPostgresAtSchema, sqliteRestoreCapabilities and the PostgresRestoreOrphan type are gone (292 of 379 lines), along with all of pkg/metadata/store/postgres/schema_ops.go. The doc comments naming a restore orchestrator and a startup orphan sweep went with them. Service is now the pure named-store registry its package doc claims, and service_test.go covers the surviving surface.
- **Fix:** Delete rather than split: OpenMetadataStoreAtPath (:162), ListPostgresRestoreOrphans (:265), DropPostgresSchema (:318), openPostgresAtSchema (:348) and sqliteRestoreCapabilities (:361) have no non-test callers — removing them leaves Service as the pure registry its doc comment claims. Only relocate if a restore command is still planned.

### [LOW] ReadRequest.Flags decoded but never consulted — UNBUFFERED semantics documented, never wired
- **Where:** `internal/adapter/smb/handlers/read.go:98` · `bloat`
- **Fix:** Drop the unused field and its documented-but-fictional semantics, or implement the bypass-cache path in common.ReadFromBlockStore for req.Flags&0x01.

### [LOW] Trivial restatement comment before struct-literal return
- **Where:** `internal/adapter/smb/handlers/read.go:523` · `comments`
- **Fix:** Delete the comment.

### [LOW] Trivial restatement comments before named-pipe lookups
- **Where:** `internal/adapter/smb/handlers/read.go:549` · `comments`
- **Fix:** Delete these comments (read.go:549; write.go:533,540).
