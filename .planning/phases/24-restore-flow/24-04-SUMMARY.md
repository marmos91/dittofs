---
phase: 24-restore-flow
plan: 04
subsystem: controlplane-runtime-tests
tags: [restore, integration-test, memory-fixture, failure-modes]
requires:
  - 24-01-SUMMARY.md
  - 24-02-SUMMARY.md
  - 24-03-SUMMARY.md
provides:
  - "TestRestoreSnapshot_Integration: 9 sub-tests covering REST-01/02/03 + D-24-13 failure-mode taxonomy"
affects:
  - pkg/controlplane/runtime/snapshot_restore_test.go (new)
tech-stack:
  added: []
  patterns:
    - "memory-only orchestration integration fixture (cpstore SQLite + memory metadata + memory remote)"
    - "counter-based head-failure injection scoped to a single hash for failure-mode coverage"
    - "metadata-store-wrapper Resetable failure injection via one-shot flag"
key-files:
  created:
    - pkg/controlplane/runtime/snapshot_restore_test.go
  modified: []
decisions:
  - "failHashAfterCount arms threshold relative to current call count: decouples gate-arm timing from incidental Head() calls already issued during fixture setup (e.g. source CreateSnapshot's own VerifyRemoteDurability)"
  - "Two-file populate + delete-first-file mutation lets post-verify-fail use a hash unique to the source manifest, so pre-verify and safety-snap verify pass while post-verify fails on that hash"
  - "failableResetable wraps the real MemoryMetadataStore and overrides Reset only; everything else (Backup/Restore/MetadataStore methods) is promoted via embedding so the orchestrator's GetMetadataStoreForShare + capability type-assertions resolve transparently"
  - "Fixture creates the share inside the metadata store (CreateShare + CreateRootDirectory) so populateFiles helpers can wire files into a valid root — Phase 23 fixture didn't need this because its synthetic controlled-backup payload didn't depend on real share/file rows"
metrics:
  duration: 4m21s
  completed: 2026-05-28
  tasks_completed: 2
  files_created: 1
  files_modified: 0
  commits:
    - abfe0009
    - 98bfb786
---

# Phase 24 Plan 04: Restore-orchestration integration test Summary

E2E integration test landing `pkg/controlplane/runtime/snapshot_restore_test.go` with 9 sub-tests that exercise `Runtime.RestoreSnapshot` end-to-end against a memory-only fixture, covering REST-01/02/03 plus the full D-24-13 failure-mode taxonomy.

## What landed

| Sub-test | Scenario | Sentinel asserted | REST coverage | D-24-13 mode |
|----------|----------|-------------------|---------------|--------------|
| HappyPath | populate → snapshot → delete a file → restore → file recovered, share stays disabled | (none — nil err) | REST-01 | success |
| EnabledShareRefuses | restore against an Enabled share | `ErrShareEnabled` | precheck | enabled-precondition |
| SnapshotNotFound | restore with bogus snap ID | `ErrSnapshotNotFound` | precheck | unknown-snap |
| SnapshotNotReady | restore against a failed snap | `ErrSnapshotStateConflict` | precheck | state-mismatch |
| NonDurableRefused | restore from a NoSyncGate snap without `AllowNonDurable` | `ErrSnapshotNotDurable` | precheck | non-durable-refuse |
| AllowNonDurable | restore from a NoSyncGate snap WITH `AllowNonDurable` (remote pre-seeded so verify still passes) | nil | REST-01 + REST-03 | non-durable-override |
| PreVerifyFailsFast | head-fail one manifest hash; assert no Reset, no safety snap, metadata unchanged | `ErrRestoreVerifyFailed` | REST-03 (pre half) | pre-verify-miss |
| PostVerifyFails | head-fail a hash unique to source manifest so pre-verify + safety-snap pass but post-verify fails; assert deleted file IS back + safety snap dump on disk | `ErrRestoreVerifyFailed` | REST-03 (post half) | post-verify-miss |
| InterruptedRestore | `failableResetable.failNextReset = true` aborts step 5; recovery via `RestoreSnapshot(safetyID)` replays the safety-snap state | `ErrRestoreAborted` (then nil) | REST-02 | reset-aborted + recovery |

All nine pass under `go test ./pkg/controlplane/runtime/ -run TestRestoreSnapshot_Integration -count=1` and under `-race`.

## Failure-injection mechanisms

### `restoreRemote` (head-failure-by-hash with phase counter)

Wraps `remote.RemoteStore`. Tracks per-hash call counts on every `Head()` and consults a per-hash threshold:

```go
if hasGate && count >= threshold {
    return blockstore.Meta{}, blockstore.ErrBlockNotFound
}
```

Threshold is set by `failHashAfterCount(hash, n)` **relative to the call count observed at arm time**. This is the load-bearing detail: source `CreateSnapshot` already ran `VerifyRemoteDurability` against the same hashes during fixture setup, so the counter starts at >0 when the test reaches `RestoreSnapshot`. Arming threshold=1 absolute would fail the very first pre-verify Head and short-circuit the test before it reaches its target gate. Arming threshold=1 *from now* gives the test author a predictable "pass once, then fail" knob regardless of harness history.

### `failableResetable` (one-shot Reset failure)

Embeds `*memory.MemoryMetadataStore`. Method promotion forwards every `metadata.MetadataStore` + `metadata.Backupable` call; only `Reset` is overridden. When `failNextReset` is true the wrapper returns a synthetic error and consumes the flag (one-shot). This lets the test land `ErrRestoreAborted`, then immediately retry restoration from the safety snap with the underlying Reset working normally.

## Coverage

- **REST-01** (orchestration completes end-to-end): HappyPath + AllowNonDurable
- **REST-02** (safety-snap is the recovery primitive): InterruptedRestore (full failed→safetyID-replay→recovered cycle)
- **REST-03** (pre+post verify gates): PreVerifyFailsFast + PostVerifyFails
- **D-24-01** (share stays disabled across success + failure): asserted in every non-precheck-only sub-test
- **D-24-13** failure-mode taxonomy: every documented mode has at least one passing sub-test (PreVerifyFailsFast, PostVerifyFails, InterruptedRestore, NonDurableRefused, EnabledShareRefuses, SnapshotNotFound, SnapshotNotReady).

## Deviations from Plan

None — plan executed exactly as written. The fixture composition matched the PATTERNS §"Memory-only integration test fixture composition" sketch; the only departure was inlining share-bootstrap (`CreateShare` + `CreateRootDirectory`) into the fixture constructor instead of via the storetest helper package, since the existing Phase 23 fixture in the same file did not yet need it.

## Self-Check: PASSED

- pkg/controlplane/runtime/snapshot_restore_test.go: FOUND
- commit abfe0009: FOUND in `git log`
- commit 98bfb786: FOUND in `git log`
- `go test ./pkg/controlplane/runtime/ -run TestRestoreSnapshot_Integration -count=1`: PASS (9/9 sub-tests)
- `go test ./pkg/controlplane/runtime/ -count=1`: PASS (Phase 23 `TestCreateSnapshot_Integration` still passes — no regression)
- `go vet ./pkg/controlplane/runtime/...`: clean
- `errors.Is` assertions: 6 sentinel families (ErrShareEnabled, ErrSnapshotNotFound, ErrSnapshotStateConflict, ErrSnapshotNotDurable, ErrRestoreVerifyFailed ×2, ErrRestoreAborted)
