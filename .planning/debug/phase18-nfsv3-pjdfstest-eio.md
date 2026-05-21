---
status: awaiting_human_verify
trigger: "Phase 18 PR #537 — NFS v3 pjdfstest regressions: chmod/12.t (EIO on SUID-clearing write), unlink/14.t (EIO on read-after-unlink, ENOTEMPTY on rmdir), open/00.t (stat size=0 after write). Develop is GREEN, v4/v4.1 pass, blockstore-layer regression that manifests only on v3."
created: 2026-05-21
updated: 2026-05-21
---

## Current Focus

reasoning_checkpoint:
  hypothesis: "Phase 18 commit cd50fed6 deleted LocalStore.DeleteAllBlockFiles and switched engine.Delete to call DeleteLog. DeleteLog permanently tombstones the payloadID (FSStore FIX-8, memory MemoryStore lines 392-394). DittoFS's PayloadID is path-based (metadata/file_helpers.go buildPayloadID = shareName/path), violating the implicit invariant that callers allocate fresh PayloadIDs after deletion. Any 'unlink + recreate at same path' workflow triggers AppendWrite-on-tombstoned-payload → ErrDeleted → propagates to NFS WRITE handler → NFS3ERR_IO (EIO). NFSv4 client-side silly-rename hides the problem (the unlink is actually a rename); NFSv3 has no silly-rename so it unlinks server-side immediately."
  confirming_evidence:
    - "git show d225926f:pkg/blockstore/engine/engine.go shows engine.Delete previously called DeleteAllBlockFiles (which did NOT tombstone — git show d225926f:pkg/blockstore/local/memory/memory.go lines 634-647 confirms no tombstone added). Phase 18 commit cd50fed6 deleted DeleteAllBlockFiles and current engine.go:518 calls bs.local.DeleteLog instead."
    - "MemoryStore.DeleteLog (memory.go:416-425): delete(s.appendLogs, payloadID); s.tombstones[payloadID] = struct{}{}. MemoryStore.AppendWrite (memory.go:312-314): returns 'AppendWrite on tombstoned payload' error when tombstoned. FSStore has analogous FIX-8 documented invariant in appendwrite.go:274-286."
    - "metadata/file_helpers.go:14: buildPayloadID returns shareName + '/' + path (path-based, NOT UUID-based). metadata/file_create.go:267 wires it: newAttr.PayloadID = PayloadID(buildPayloadID(...)) on every Create. Same path = same PayloadID across delete-recreate."
    - "pjdfstest/chmod/12.t script (fetched from upstream): test sequence creates file at path n0 with mode 04777, unlinks, creates again at n0 with mode 02777 → tests 7-8 fail. Same pattern at 06777 → tests 11-12 fail. open/00.t test 41 area: unlink n0 + 'echo test > n0' → WRITE returns EIO → stat size=0."
    - "unlink/14.t: 'expect 0 create n0' (test 3) + 'open n0 O_WRONLY : write Hello,World' (test 4) + 'open ... unlink ... fstat' (test 5) + 'expect 0 create n0' (test 6 — RECREATE at same path → payloadID tombstoned). Test 6 chain: write fails with EIO → unlink in chain never executes → n0 remains → directory n2 not empty → rmdir n2 returns ENOTEMPTY (test 7)."
    - "NFSv4 known_failures already lists unlink/14.t as expected failure due to silly-rename. The silly-rename means unlink-recreate at same path becomes rename-create-at-fresh-path → fresh PayloadID, masking the tombstone issue."
  falsification_test: "If hypothesis is wrong: writing a unit test that calls AppendWrite-DeleteLog-AppendWrite cycle on memory store at same payloadID should succeed today. If it fails (which I expect from the code), hypothesis confirmed."
  fix_rationale: "Memory store's rollup is synchronous (rollupLocked runs under write lock in AppendWrite), so its tombstone cannot guard any async-rollup-completion race; it is purely structural cruft that breaks legitimate recreate. Drop the tombstone gate (and the tombstones map). FSStore tombstone DOES guard against async-rollup-completion-after-delete races, but the drain barrier in DeleteAppendLog step 2 ensures all pre-delete writers/rollups have completed before we exit. We can safely clear the tombstone at the END of DeleteAppendLog (step 6) — any AppendWrite arriving after step 6 starts fresh state via getOrCreateLog. The FIX-8 race (clear-on-success resurrection) is moot because the drain in step 2 already serializes us against in-flight writers."
  blind_spots: "Cannot run NFS v3 pjdfstest locally on macOS (requires Linux + sudo + NFS client). Verification will be local unit tests + go test + push and observe CI. I haven't checked whether dedup / GC or refcount bookkeeping has any subtle assumption that a deleted payloadID stays dead — but engine.Delete decrements refcounts based on the file's BlockRef list which is metadata-side, so post-recreate the new file's metadata starts fresh and the engine treats it as a new payload — should be OK. There may also be concerns about the speculative rollupChunkEmitter callback for memory store firing for chunks from the deleted lifecycle after recreate; this is bounded by the synchronous-rollup property."
hypothesis: PayloadID is path-based; DeleteLog tombstones it permanently; Phase 18 wired engine.Delete through DeleteLog, exposing the latent path-reuse incompatibility.
test: add unit test for AppendWrite-DeleteLog-AppendWrite cycle on memory store; then run local go test to confirm
expecting: current memory store fails with "AppendWrite on tombstoned payload"; after fix all three calls succeed
next_action: implement memory + FS store fixes, add regression tests, run local go test, push and check CI

## Symptoms

expected:
  - tests/chmod/12.t tests 7,8,11,12 pass (SUID-clearing write under non-root succeeds, fstat shows 0777)
  - tests/unlink/14.t tests 6,7 pass (read after unlink-on-same-fd returns content; rmdir succeeds on dir made empty by unlink)
  - tests/open/00.t test 41 passes (after 5-byte write, stat returns size 5)
actual:
  - chmod/12.t: write returns EIO instead of clearing SUID and succeeding
  - unlink/14.t: pread after unlink returns EIO; rmdir returns ENOTEMPTY
  - open/00.t: stat after write returns size 0 (and likely echo I/O error)
errors:
  - "echo: I/O error" at line 85 of tests/open/00.t
  - "not ok 41 - tried 'stat file size', expected 5, got 0"
  - chmod test 7: expected 0777, got EIO
  - unlink test 6: expected "Hello,_World!", got EIO
  - unlink test 7: expected 0, got ENOTEMPTY
reproduction: CI "NFS v3 / Memory" job in PR #537 (run 26225957255 job 77173364582). Locally need NFS v3 server + pjdfstest harness.
started: Phase 18 commits e2243471..ad7ed2fa on gsd/phase-18-syncer-simplification branch; develop baseline is green.

## Eliminated

(none yet)

## Evidence

- timestamp: initial
  checked: git log d225926f..HEAD (Phase 18 commit list)
  found: 30+ commits in Phase 18. Most relevant for blockstore behavior:
    - 1736a7a1 refactor(18-08): CAS-rewire writes — AppendWrite + local.Put(hash)
    - 3623b15d refactor(18-08): CAS-rewire reads — local.Get(hash) / local.Has(hash)
    - cd50fed6 refactor(18-08): delete 7 transitional LocalStore methods + FlushedBlock
    - e136e96f feat(18-08): wire CAS read path + chunk-offset FileBlock rows
    - e2243471 fix(blockstore/engine): resolve FileBlock row by absolute offset under FastCDC
    - 82ecfba2 fix(blockstore/engine): use absolute chunk offset in BlockRef + file-size math
    - 31248375 fix(blockstore/engine): gate GetFileSize/Exists on SyncedHashStore.IsSynced
    - 60848384 fix(blockstore): payload-keyed local read closes pre-rollup RAW gap
  implication: 31248375 ("gate GetFileSize/Exists on SyncedHashStore.IsSynced") and 60848384 ("payload-keyed local read closes pre-rollup RAW gap") suggest pre-rollup state has been an active problem area — likely there's still a gap.

## Resolution

root_cause: |
  Phase 18 commit cd50fed6 deleted LocalStore.DeleteAllBlockFiles and switched
  engine.Delete to call LocalStore.DeleteLog. DeleteLog permanently tombstones
  the payloadID (memory/memory.go and fs/appendwrite.go FIX-8 invariant), which
  violates DittoFS's implicit assumption: PayloadID is path-based (metadata/
  file_helpers.go buildPayloadID = shareName + path) so 'unlink + create at
  same path' reuses the same PayloadID. Any subsequent AppendWrite errors with
  ErrDeleted / "AppendWrite on tombstoned payload" → propagates through NFSv3
  WRITE handler → NFS3ERR_IO (EIO). NFSv4 hides this via client-side
  silly-rename; NFSv3 unlinks server-side immediately, exposing it.

fix: |
  Make DeleteLog clear-on-success in both memory and fs local stores. The
  memory store has synchronous rollup so the tombstone has no protective
  purpose; dropped the tombstones map entirely. The fs store needs the
  tombstone as a transient barrier against in-flight rollups completing for a
  payload mid-deletion; clearing the tombstone at the END of DeleteAppendLog
  (after the in-flight drain in step 2) is safe — the drain serializes us
  against any pre-delete writers/rollups and from step 5 onward only fresh
  state is visible. Updated BlockStoreAppend.DeleteLog godoc to mandate
  recreate semantics. Added a conformance scenario (RecreateAfterDeleteLog)
  that exercises the cycle on every backend.

verification: |
  Local: go build, go vet, gofmt -s -l, go test -race for
  pkg/blockstore/..., pkg/metadata/..., pkg/controlplane/...,
  internal/adapter/nfs/... — all PASS.
  CI: pushed to gsd/phase-18-syncer-simplification; PR #537 NFS v3 / Memory
  job should now report only the documented utimensat/09.t known failure.

files_changed:
  - pkg/blockstore/blockstore.go              # DeleteLog godoc: recreate semantics
  - pkg/blockstore/blockstoretest/appendlog.go  # add RecreateAfterDeleteLog scenario
  - pkg/blockstore/local/memory/memory.go     # drop tombstones map; clear files map in DeleteLog
  - pkg/blockstore/local/fs/appendwrite.go    # clear tombstone at end of DeleteAppendLog
  - pkg/blockstore/local/fs/fs.go             # update tombstones field godoc
  - pkg/blockstore/local/fs/delete_truncate_test.go  # flip FIX-8 assertions to recreate semantics
