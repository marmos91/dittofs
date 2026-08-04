# SMB2/3 orchestration perf — create/write path

Goal: close the SMB-vs-NFS ~10x metadata/create gap and cut per-op SMB adapter overhead.
Data-driven: profile first, cut what the profile indicts.

## Root cause (established)

SMB CLOSE issues **2 extra durable fsync barriers per create** that NFS CREATE never does:
- `close.go:223` `common.CommitBlockStore(...)` — fsync/F_FULLFSYNC on the journal append-log fd
  (every regular file gets a PayloadID, so even a zero-byte create pays this)
- `close.go:250` `metaSvc.FlushPendingWriteForFile(..., true)` — STRICT inline metadata fsync
  (durable=true; deliberately NOT relaxed by #1687 because #1267 treats CLOSE as a persistence guarantee)

NFS CREATE = 1 metadata commit, no block-store touch. Barriers serialize:
P distinct fds = P F_FULLFSYNC barriers. Server is ~97% idle at rig load → the wall is
fsync latency, not CPU. Group-commit/fsync-coalescing already refuted (#1742/#1747).

## Audit findings (agent acdf2dcedef47f060, ranked)

### Tier 1 — safe, pure-metadata (CPU cleanup; unlikely to move fsync-bound throughput, but real redundancy)
1. CREATE reloads parent inode 3–4×: `create.go:1192` (CheckParentCreateAccess re-fetches),
   `1210` (lookupCaseInsensitive lists children), `1992` (compression-attr GetFile),
   + again inside CreateFile. Seam already exists: `auth_permissions.go:662`
   `CheckParentCreateAccessFile` (file-passing, added by #1737) — handler just doesn't use it.
   Fix: load parent File once, thread into CheckParentCreateAccessFile + createNewFile.
2. `create.go:1992` compression GetFile — subset of #1, cuttable alone (NFS #1579 analog).

### Tier 2 — real latency win, correctness-flagged
3. CLOSE double fsync (close.go:223 + 250) — the dominant latency on create+WRITE.
   Two backends/fds, both #1267-mandated durable. CANNOT naively drop either (silent-truncation
   regression). Needs a single cross-backend durability-barrier seam, or skip block Commit for
   pure creates via a CLEAN dirty flag (NOT SmbWriteTriggered — that's timestamp machinery,
   gating durability on it risks #1267). Pure-metadata closes already skip block Commit
   (close.go:212 guards PayloadID != "").
4. QUERY_INFO re-fetches attrs CREATE already computed (`query_info.go:292`). Clean win for
   create+query+close (no WRITE); needs WRITE-invalidation of the cache otherwise stale size.
   Same pattern: close.go POSTQUERY at ~311.

### Tier 3 — broad, small (both workloads)
5. `response.go:31` strings.ToLower(op) per request — precompute lowercased label in dispatch table.
6. compound.go sendCompoundResponses — per-response realloc churn; size once, allocate once.
7. MakeErrorBody() allocates fixed 9-byte body each call — package-level immutable slice.
8. GetMetadataService() re-fetched ~7×/CREATE — fetch once, pass down (cosmetic unless non-trivial).

### NOT cuttable
- Per-op auth frozen at CREATE (OpenFile.GrantedAccess); re-eval = MS-FSA protocol bug.
- Per-compound credit/sequence accounting already coalesced (computed once from first header).
- Per-command signing spec-mandated (MS-SMB2 3.3.4.1.1).

## Credit fix #1815 (in flight, separate from perf)
Conserve-credits redesign: consume every compound sub-command charge + grant >= charge +
move middle-response grants to last. Credit smbtorture tests GREEN
(single_req_credits_granted, session_setup_credits_granted). CI red was smb2.rename flake
(rename_dir_bench, close-full-information) on badger-fs only — orthogonal to the diff. Rerunning.

## Execution order (user-directed)
1. Complete benchmark matrix → fill docs/BENCHMARKS.md SMB3 section. Close related issues.
2. Profile: DITTOFS_CONTROLPLANE_PPROF=true, capture CPU + block/off-CPU during a metadata cell,
   produce flamegraph. Adjudicates Tier 1 (CPU) vs Tier 2 (fsync).
3. Tier 1 — separate PR (parent-inode dedup via CheckParentCreateAccessFile).
4. Tier 2 — CLOSE barrier / QUERY_INFO, informed by profile + clean dirty flag.
5. Profile again — measure Tier 1+2 impact.
6. Benchmark again — measure throughput delta.
7. Teardown bench VM (verify NOT coder 22a493cf).

## Profiling verdict (2026-07-20, local, pkg/metadata create bench, RELAXED durability)
- BenchmarkCreateFileOneDir 17.5us/op vs SeparateDir 16.2us/op (~7% apart) → same-parent
  CONTENTION is NOT the wall. 213 allocs/op, 11KB/op.
- CPU mostly scheduler noise (fast op x RunParallel). Off-CPU BLOCK profile: badger dominates —
  DB.Update 12.4%, NewTransaction 9.6%, Txn.Commit 7.2%, flushMemtable. i.e. ONE badger
  commit per create is the metadata-side cost.
- createEntry: parent read served WARM from GetFileForCreate parentCache (~20ms cum, cheap) →
  Tier 1 parent-dedup is a CPU/alloc trim, NOT a throughput lever (confirmed twice now).
- Relaxed bench has NO fsync. Production SMB create wall = this ~16us metadata-txn cost + the
  CLOSE fsync barriers (block Commit + strict metadata flush) that this bench does not exercise.
  The barriers are the real ms-scale gap; a CPU profile can't see them (they're off-CPU Sync()).
- A separate BenchmarkCreateFile shows 422us/op, ~5MB/op — a much heavier path; identify it.

## STRATEGIC GATE (decide Tier 2 scope from benchmark data)
Question the overnight matrix answers: is the competitor gap in WRITEBACK mode (dittofs-*-writeback,
durability deferred — gap would be per-op orchestration, fixable by Tier 1/2/3) or ONLY in DURABLE
mode (dittofs-s3 / -remote — gap is inherent fsync barriers competitors' durable modes also pay)?
- If dittofs-writeback ~= juicefs-default/rclone → NO writeback gap; durable gap is fair/inherent →
  Tier 2 = coalesce the 2 CLOSE barriers into 1 (safe) rather than a risky write-coordinator.
- If dittofs-writeback still >>slower than juicefs-default → real orchestration gap → invest in
  write-coordinator (batch mutations -> fewer badger commits + defer/coalesce durability).
DO NOT ship a risky #1267-touching durability change unattended overnight; design it, gate impl on data.

## *** PIVOTAL: gate answered by preview data (2026-07-20, medium, single-run) ***
Pulled the completed s3+competitor cells. dittofs vs competitors (WRITEBACK mode → durability NOT the excuse):
- metadata ops/s: dittofs 71 | juicefs 53 | rclone 122 → dittofs BEATS juicefs, ~0.6x rclone. COMPETITIVE.
- rand-write-4k IOPS: dittofs 7,351 | juicefs 1,459 | rclone 9,715 → dittofs BEATS juicefs 5x. COMPETITIVE.
- seq-write MB/s: dittofs 395 (writeback) / 320 (durable) | juicefs 2,931 | rclone 2,266 → dittofs ~7x SLOWER. REAL GAP.
- rand-read-4k IOPS: dittofs 298 | juicefs 20,645 | rclone 10,099 → dittofs ~30-70x SLOWER. REAL GAP (check rig confounds — memory: bench rig lies about read IOPS).
CONCLUSION: the competitor gap is NOT create/metadata (dittofs competitive there) — it is SEQ-WRITE
throughput and RAND-READ IOPS, in writeback mode, so NOT a durability/fsync-barrier issue.
=> Tier 1/2/3 (CREATE path) are legit clean-code wins but do NOT close the competitor gap. Ship them
   (small metadata headroom vs rclone), but PIVOT the real perf investigation to:
   (A) seq-write pipeline: FastCDC chunk/hash + block-store write path throughput (7x, both modes).
   (B) rand-read per-read overhead: NFS/SMB+metadata per-read cost (memory: warm fastpath shipped
       #1648/#1651 fixed block-store amp; remaining gap is per-read metadata/protocol). VERIFY rig
       confounds first (single-run preview; server may be ~97% idle => rig-bound not server-bound).
CAVEAT: single-run medium preview; confirm with full matrix + >=6 runs before BENCHMARKS.md claims.

## *** REFINED gap diagnosis from cpu%/diskwr/latency (2026-07-20) ***
Per-cell counters (harness records cpu_pct, disk_wr_mbps, latency p50/p99):
- seq-write: dittofs 395MBps @ diskwr=408 (ACTUALLY persisting) vs juicefs 2931MBps @ diskwr=1
  (RAM/page-cache buffered, not hitting disk in window). => the "7x" is largely a durability/buffering
  artifact (apples-to-apples, memory project_bench_fairness). dittofs is disk-BW-limited. REAL issue =
  seq-write p99 = 270ms tail (journal-backpressure stalls), NOT raw throughput.
- rand-read-4k: dittofs 298 IOPS @ p50=270ms, cpu53%, diskwr=39 DURING a READ workload => reads MISS
  local cache and do S3-fetch + cache-fill nearly every op. "warm" cells are NOT warm. REAL 30x gap.
  Root suspects: #1767 (cold-read barrier broken, drain stalls + evict no-op <1GiB), #1595. The bench
  warm-up isn't populating the read cache, OR read path isn't serving from cache. NEEDS profiling on a
  controlled setup to split (a) bench warm-up bug vs (b) real read-path cache miss/refetch.
- seq-read: dittofs 89MBps BEATS juicefs 2 / rclone 6. FINE.
- metadata 71 (beats juicefs 53) & rand-write 7351 (beats juicefs 1459). FINE/competitive.
REAL WORK (post-merge, needs VM/profiling, gate on full matrix + >=6 runs):
  P0 rand-read 270ms: is "warm" actually warm? instrument cache hit-rate; likely #1767 warm-up barrier.
  P1 seq-write tail: journal backpressure 270ms p99 (the wedge we saw); bound the local-journal fill.
  Tier1/2/3 CREATE PRs: ship as clean wins (small metadata headroom vs rclone 122 vs 71), NOT gap-closers.

## *** ROOT CAUSE of rand-read 30x gap = BENCHMARK HARNESS BUG (2026-07-20) ***
Verdict (c) both, HEAVILY (a) harness artifact:
- (a) PRIMARY: internal/dfsbench/run/managed.go:137->141 has NO quiesce/drain barrier between
  layoutReadTarget (writes 1GiB read target) and the warm timed runPass. The warm rand-read races
  the async carve+upload+rollup of the just-written file => 39MB/s disk-writes-during-read + CPU/IO
  contention => p50 270ms, cpu53%. Block-store code proves a SETTLED <=1GiB warm read serves locally,
  zero S3, zero diskwr (below 32GiB MaxLocalBytes nothing evicts, read_internal.go:45-47 cold=false).
  dittofs OWN NFS warm rand-read = 7400-24800 IOPS on same block store => SMB3 298 is 25-80x below
  dittofs itself => dominated by missing settle, NOT a read-path bug. Consistent w/ #1595, #1767.
- (b) SECONDARY/real: genuine per-SMB-READ cost (metadata covering-chunk lookup + interval assembly
  per 4k). Only measurable AFTER fixing (a). If post-fix SMB3 rand-read still <<NFS, chase (b).
FIX (dfsbench, ~15-30 LOC): add sync-idle quiesce between layoutReadTarget and warm runPass —
  poll block-store UnsyncedBytes==0 (pkg/block/journal/store.go:120, surfaced via engine stats).
  NOT dfsctl drain-uploads (stalls #1767). dittofs-specific settle; competitors no-op.
IMPLICATION: the overnight matrix warm-READ cells are CONTAMINATED (measured mid-digestion). Metadata
  + write + competitor cells are fine. After harness fix + rebuild + re-run warm-reads => fair numbers.
  This is THE key action for "results similar to competitors" on rand-read.
CAVEAT: couldn't confirm which tier produced 298-IOPS cells; verify -remote tier keeps bytes local
  after CLOSE-sync (same 32GiB cap, should).

## *** VALIDATED: settle fix collapses the rand-read "gap" (2026-07-21) ***
Re-ran dittofs warm reads with settle-fixed dfsbench (#1818). HONEST medium rand-read IOPS:
- dittofs-s3-writeback (badger): 20,548 (was 298 contaminated) — MATCHES juicefs 20,645, 2x rclone 10,099.
- dittofs-s3 (badger): 9,156. dittofs-s3-remote: 8,620. postgres-wb: 3,486. sqlite-wb: 818.
- seq-read: dittofs 364-444 MBps >> juicefs 2 / rclone 6.
CONCLUSION: the 30-70x rand-read "gap" was ENTIRELY the harness measuring mid-digestion. dittofs
(badger) is competitive-to-winning across the board once measured fairly:
  rand-read 20.5k (=juicefs, >rclone) | seq-read 444 (>>both) | rand-write 7.3k (>juicefs, ~rclone) |
  metadata 71 (>juicefs 53, <rclone 122) | seq-write 395 disk-limited (durability/buffering artifact).
sqlite still slow on reads too (818, p50 147ms) — same single-writer/per-op cost as #1819. badger wins.
Next: #1819 sqlite write-amp fix (agent running) -> merge -> rebuild dfs SERVER -> restart VM ->
re-run sqlite rand-write cells -> measure. Then fill docs/BENCHMARKS.md with the honest matrix.

## *** Manifest delta (#1824) re-bench result (2026-07-21, single-run) ***
Rebuilt dfs SERVER with #1820+#1824, re-ran sqlite/postgres rand-write. IOPS pre->post:
- sqlite-s3: med 380->407 (+7%), lg 362->395 (+9%). sqlite-wb: med 390->424 (+9%), lg 345->397 (+15%).
- postgres-s3: med 1765->1732 (noise), lg 1288->1441 (+12%). postgres-wb: med 1730->1800 (+4%), lg 1422->1527 (+7%).
- sqlite-remote med 338->246 = single-run S3 variance (delta only removes work, can't regress).
VERDICT: modest real win, MORE on large files (O(N)->O(changed) as predicted), + prevents catastrophic
O(N) amplification on huge manifests. Does NOT change ranking — SQL backends still fundamentally behind
badger (single-writer/round-trips dominate). badger = write-heavy recommendation. Not updating
BENCHMARKS.md for a single-run ~10% (below >=6-run bar); documented story unchanged.

## Discoveries log
- 2026-07-20 PRs: #1817 Tier3 allocs MERGED. #1816 Tier1 create-dedup (review clean, CI pending).
  #1818 dfsbench warm-read settle barrier: reviewer caught a CRITICAL bug — settle window (500ms)
  was SHORTER than the carve dispatcher tick (UploadInterval 2s), so UnsyncedBytes is flat between
  ticks and the loop false-settles in an inter-tick lull → warm read still races the next carve pass.
  Also pending_uploads is DEAD (hardcoded 0 in engine stats.go:135). FIXED: stability window now 4s
  (>1 tick), tracked by last-change time; dropped pending_uploads reliance (unsynced-stability only).
  Pushed; CI re-running. Merge on green, then rebuild dfsbench + re-run warm-reads for fair numbers.
- 2026-07-20: #1815 CI red = smb2.rename.rename_dir_bench + smb2.rename.close-full-information
  FAIL on badger-fs ONLY (memory backend green). Credit conformance tests green.
  HYPOTHESIS (not yet confirmed): the redesign removed develop's +512 credit top-up in
  grantConnectionCredits, dropping to echo(requested) floored at charge. Fast backend replenishes
  before draining (no effect); slow badger-fs (~10x create latency) keeps more ops outstanding per
  replenish cycle → client credit balance can run low → rename bench does fewer iters / stalls.
  Prior rename-flake history: commit bd71f940 (SHARING_VIOLATION flake), #1652/#1658.
  DISCRIMINATOR = rerun: pass→timing flake; fail-again-deterministic→reduced credit ceiling starves
  slow backend → need a sane credit headroom floor (NOT blind +512; enough to avoid starvation).
- 2026-07-20 UPDATE: hypothesis FALSIFIED by the actual committed diff (origin/develop...HEAD).
  Net credit change vs develop is NON-NEGATIVE: response.go adds only a floor (credits<charge→charge,
  raises multi-credit grants, never lowers); compound.go consume-all corrects server sequence-window
  accounting (client's real balance unchanged); move-to-last redistributes the SAME total across a
  compound's responses. No +512-top-up removal in the final commit (that was a reverted intermediate).
  Client-visible credits under this branch are >= develop → starvation impossible. Conclusion:
  rename_dir_bench + close-full-information are the pre-existing badger-fs rename flake family
  (bd71f940, #1652/#1658), NOT a regression. Rerun expected green. If it deterministically fails
  again, root-cause fresh (do NOT retry-mask) — but the credit path is exonerated.

## #1776 metadata-store dedup — COMPLETE (2026-07-21)

All five chunks landed; issue closed. Chunks: quota (#1787), statfs `internal/sqlstat`
(#1826), sqlcodec (done), badger error mapper (#1825), SQL tx backoff `internal/txretry`
(#1827).

Chunk-5 scope call (the delicate one): only the **byte-identical** SQL backoff was unified —
sqlite and postgres shared the same `txBackoff` + constants + deadline computation, now
`internal/txretry.{Deadline,Backoff}`. Each backend keeps its own retryable-error
classification (sqlite BUSY/LOCKED vs postgres 40001/40P01) and commit loop. **Badger was
deliberately NOT unified**: its loop is closure-based `db.Update` with a fixed attempt count
(no time budget) plus post-commit cache invalidation, so folding it in would *change* retry
semantics for ~zero dedup win — a contention-correctness risk not worth taking. Reviewer
confirmed the SQL extraction is behavior-preserving.
