---
status: resolved
trigger: "GitHub Issue #130 - SMB E2E tests, cross-protocol tests, and related tests failing"
created: 2026-02-18T12:00:00Z
updated: 2026-02-18T14:15:00Z
---

## Symptoms

expected: All E2E tests pass (SMB, cross-protocol, backup, settings, file sizes)
actual: Multiple test suites were hidden behind t.Skip() calls; settings version tracking failed; 100MB file checksum mismatch
errors: Version counter reset to 1 on settings reset; "cache full" during large file writes; data corruption on read-back
reproduction: Run `sudo go test -tags=e2e -v ./test/e2e/...`
started: Tests were skipped/failing since the test suites were added

## Evidence

- timestamp: 2026-02-18T12:10:00Z
  checked: SMB file operations (SMB-01 through SMB-06)
  found: All 6 tests PASS when t.Skip removed
  implication: SMB tests were incorrectly skipped

- timestamp: 2026-02-18T12:15:00Z
  checked: Cross-protocol interop (XPR-01 through XPR-06)
  found: All 6 tests PASS when t.Skip removed
  implication: Cross-protocol tests were incorrectly skipped

- timestamp: 2026-02-18T12:20:00Z
  checked: Cross-protocol locking, grace period, backup tests
  found: All PASS or properly skip on macOS
  implication: Tests were incorrectly hidden behind blanket t.Skip

- timestamp: 2026-02-18T12:30:00Z
  checked: Settings version tracking test
  found: Version progression 1->2->3->1 (reset goes back to 1)
  implication: ResetNFSAdapterSettings uses delete+create, losing version counter

- timestamp: 2026-02-18T13:00:00Z
  checked: 100MB file checksum on NFS read-back
  found: 20-22 of 25 4MB chunks corrupted (zero-filled regions)
  implication: Data loss during write/read cycle

- timestamp: 2026-02-18T13:20:00Z
  checked: Server logs during 100MB write with DEBUG level
  found: No WRITE errors, no READ errors, no cache-full errors. Only 3 eager uploads (first 12MB). Flush uploads partial blocks with sizes 256KB-1.8MB instead of 4MB.
  implication: COMMIT during active write triggers Flush that detaches partial blocks

- timestamp: 2026-02-18T13:30:00Z
  checked: DetachBlockForUpload behavior in uploadRemainingBlocks
  found: DetachBlockForUpload sets blk.data=nil. Subsequent writes to same block re-allocate buffer with fresh (empty) coverage bitmap, losing previously-written data. ReadAt returns zeros for the lost region.
  implication: ROOT CAUSE of 100MB corruption found

## Eliminated

- hypothesis: Cache-full backpressure causing WRITE errors
  evidence: DEBUG logs showed zero WRITE errors and zero cache-full errors
  timestamp: 2026-02-18T13:20:00Z

- hypothesis: NFS client ignoring WRITE errors
  evidence: All 3200 WRITE operations returned NFS3OK
  timestamp: 2026-02-18T13:20:00Z

- hypothesis: Read path returning wrong data from cache
  evidence: ReadAt correctly copies from block buffers; the buffers themselves contain zeros
  timestamp: 2026-02-18T13:25:00Z

## Resolution

### Bug 1: Tests Hidden Behind t.Skip

root_cause: SMB, cross-protocol, backup, and other tests were blanket-skipped during development but actually pass
fix: Removed 7 t.Skip calls across 6 test files, replaced with informational comments
verification: All tests pass when skip is removed

### Bug 2: Settings Version Counter Reset

root_cause: ResetNFSAdapterSettings and ResetSMBAdapterSettings used delete+create pattern. NewDefaultNFSSettings hardcodes Version:1, so reset always went back to version 1 instead of incrementing.
fix: Changed to update-in-place pattern using gorm.Expr("version + 1") to maintain monotonic version counter
verification: Version progression now correctly 1->2->3->4
files_changed:
  - pkg/controlplane/store/adapter_settings.go

### Bug 3: 100MB File Data Corruption (PRIMARY)

root_cause: During NFS COMMIT, Flush() calls uploadRemainingBlocks() which used DetachBlockForUpload() to detach block buffers for zero-copy upload. When COMMIT fires during an active 100MB write, partial blocks (e.g., 256KB of 4MB filled) are detached (data=nil). Subsequent NFS WRITE RPCs for the same block call getOrCreateBlock() which re-allocates a fresh buffer with an empty coverage bitmap. The previously-written data (0-256KB) exists only in the block store upload, but the cache now has a fresh buffer that only contains data from 256KB onwards. On read-back, ReadAt copies from the block buffer which has zeros in the 0-256KB region.
fix: Changed uploadRemainingBlocks to use MarkBlockUploading + copy instead of DetachBlockForUpload. Data stays in cache for concurrent reads. Also changed writeToBlock to allow writes to Uploading blocks (reverting to Pending) since flush now uses copied data.
verification: 100MB test passes 3 consecutive times. All E2E tests pass. All unit tests pass.
files_changed:
  - pkg/payload/transfer/manager.go (flush upload: copy instead of detach)
  - pkg/cache/write.go (allow writes to Uploading blocks)
  - pkg/payload/service.go (backpressure retry for cache-full)
  - test/e2e/backup_test.go (removed t.Skip)
  - test/e2e/controlplane_v2_test.go (removed t.Skip)
  - test/e2e/cross_protocol_lock_test.go (removed t.Skip)
  - test/e2e/cross_protocol_test.go (removed t.Skip)
  - test/e2e/file_operations_smb_test.go (removed t.Skip)
  - test/e2e/grace_period_test.go (removed t.Skip)
  - test/e2e/nfsv4_store_matrix_test.go (removed t.Skip, enabled 100MB)
  - pkg/controlplane/store/adapter_settings.go (version tracking fix)
