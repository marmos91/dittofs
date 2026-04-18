---
phase: 07-testing-hardening
reviewed: 2026-04-18T00:00:00Z
depth: standard
files_reviewed: 7
files_reviewed_list:
  - pkg/backup/destination/corruption_test.go
  - test/e2e/helpers/backup_metadata.go
  - pkg/backup/restore/version_gate_restore_test.go
  - test/e2e/backup_matrix_test.go
  - test/e2e/backup_chaos_test.go
  - test/e2e/backup_restore_mounted_test.go
  - pkg/backup/concurrent_write_backup_restore_test.go
findings:
  critical: 0
  warning: 3
  info: 4
  total: 7
status: issues_found
---

# Phase 7: Code Review Report

**Reviewed:** 2026-04-18T00:00:00Z
**Depth:** standard
**Files Reviewed:** 7
**Status:** issues_found

## Summary

Seven files covering the Phase 7 testing and hardening suite were reviewed: corruption-vector integration tests, a version-gate restore unit test, an E2E matrix test (3 engines × 2 destinations), two chaos tests (kill-mid-backup and kill-mid-restore), a mounted-restore precondition test, and a concurrent-write backup/restore integration test.

The code is generally well-structured. Sentinels are checked with `errors.Is`, helper composition is sound, and the test isolation strategy (shared Localstack container per binary, unique bucket names per subtest) follows project conventions. Three warnings were identified that could cause test unreliability or silently mask failures; four informational issues were noted for future polish.

## Warnings

### WR-01: `deleteCorruptionBucket` only fetches the first 1000 S3 objects

**File:** `pkg/backup/destination/corruption_test.go:158`

**Issue:** `ListObjectsV2` returns at most 1000 objects per page. If a test bucket accumulates more than 1000 objects (not expected today but possible if a test is re-run or a previous run leaked state), `DeleteBucket` will fail with `BucketNotEmpty` and the cleanup swallows the error silently. The bucket then leaks in Localstack for the duration of the test binary run.

**Fix:**
```go
// Use a paginator to drain all objects unconditionally.
paginator := awss3.NewListObjectsV2Paginator(corruptionLocalstack.client, &awss3.ListObjectsV2Input{
    Bucket: aws.String(name),
})
for paginator.HasMorePages() {
    page, err := paginator.NextPage(context.Background())
    if err != nil {
        break
    }
    for _, o := range page.Contents {
        _, _ = corruptionLocalstack.client.DeleteObject(context.Background(), &awss3.DeleteObjectInput{
            Bucket: aws.String(name),
            Key:    o.Key,
        })
    }
}
```

---

### WR-02: `runCorruptionCase` GetBackup path silently passes when `wantErr` is nil

**File:** `pkg/backup/destination/corruption_test.go:435-440`

**Issue:** When a `corruptionCase` reaches the `GetBackup` branch (i.e. `checkManifestOnly == false`) but has `wantErr == nil`, `require.ErrorIs(t, closeErr, nil)` passes whether `closeErr` is nil OR non-nil. `errors.Is(anyErr, nil)` returns true only when `anyErr == nil`, so in practice this is safe today — but there is no `default: t.Fatalf(...)` guard analogous to the one in the `GetManifestOnly` branch (lines 429-431). A future case with `checkManifestOnly: false` and no `wantErr` set would silently pass regardless of the outcome.

**Fix:** Add a configuration-completeness guard before the GetBackup call, mirroring the `GetManifestOnly` branch:
```go
// Validate case is fully configured before running.
if !tc.checkManifestOnly && tc.wantErr == nil {
    t.Fatalf("corruptionCase %q: GetBackup path requires wantErr to be set", tc.name)
}
```

---

### WR-03: `TestBackupChaos_KillMidRestore` accepts `"succeeded"` as a valid terminal state without failing the test

**File:** `test/e2e/backup_chaos_test.go:204-210`

**Issue:** The test explicitly returns without a failure when the restore job completes before the kill signal arrives (the 300 ms sleep window). This means the SAFETY-02 invariant for the restore path is untested on any run where the restore is fast. On CI with low payload (50 users, local S3), this race is likely to trigger frequently, causing the test to provide no safety coverage at all. The comment at line 186 acknowledges this but treats it as acceptable.

This is a test reliability/coverage issue: a consistently "succeeded" run silently drops the SAFETY-02 restore-path assertion without any CI signal.

**Fix:** The most reliable approach is to increase the seed payload to a size that reliably exceeds the kill window on CI (e.g. 500 users with a 1-second sleep), or to use a mechanism that pauses the restore mid-flight before killing (e.g. a named pipe or a temporary network disruption). As a minimum, convert the silent `return` to a `t.Skip` so the CI log shows the test was not exercised:
```go
if finalJob.Status == "succeeded" {
    t.Skipf("restore completed before kill (timing race); increase seed size or sleep. Actual status: %s", finalJob.Status)
}
```

---

## Info

### IN-01: `TestManifestVersionGate_RestoreSentinel` is a tautological self-reference

**File:** `pkg/backup/destination/corruption_test.go:529-533`

**Issue:** `errors.Is(restore.ErrManifestVersionUnsupported, restore.ErrManifestVersionUnsupported)` is always true for any non-nil sentinel; it tests no real logic. The meaningful assertion is that the sentinel is non-nil and carries the expected text, which the other two `require` calls already cover. The `errors.Is` self-reference adds noise without value.

**Fix:** Remove the tautological line:
```go
func TestManifestVersionGate_RestoreSentinel(t *testing.T) {
    require.NotNil(t, restore.ErrManifestVersionUnsupported)
    require.Contains(t, restore.ErrManifestVersionUnsupported.Error(), "manifest version")
}
```

---

### IN-02: `hashDirTreeExcluding` hashes file paths using the OS-absolute path

**File:** `pkg/backup/restore/version_gate_restore_test.go:182-205`

**Issue:** `h.Write([]byte(p))` writes the full absolute OS path (e.g. `/tmp/TestXxx123/001/...`) into the hash. This means two identical directory trees rooted at different temp paths produce different hashes. The current usage (pre-tamper vs post-tamper comparison on the same root) is not affected, but the function is misleadingly documented as computing a content hash of "path || fileBytes". Any future caller that tries to compare trees across roots will get a false mismatch.

**Fix:** Write the path relative to root rather than the absolute path:
```go
rel, _ := filepath.Rel(root, p)
_, _ = h.Write([]byte(rel))
```

---

### IN-03: `s3SafeBucketName` and `chaosS3BucketName` are near-duplicates across two E2E files

**File:** `test/e2e/backup_matrix_test.go:164` and `test/e2e/backup_chaos_test.go:26`

**Issue:** Both files define their own local bucket-sanitisation helper with slightly different rules (`chaosS3BucketName` uses a regex; `s3SafeBucketName` does manual replacements). The duplication means a fix to one is not automatically applied to the other, and the two helpers diverge in edge-case handling (e.g. dot removal is only in `s3SafeBucketName`).

**Fix:** Consolidate into a single exported helper in `test/e2e/helpers/` (e.g. `helpers.S3SafeBucketName`) and reference it from both files.

---

### IN-04: `writerErrs` counter is logged but not asserted in `TestConcurrentWriteBackupRestore`

**File:** `pkg/backup/concurrent_write_backup_restore_test.go:149`

**Issue:** The log message at line 149 says "expected: 0" for `writerErrs`, but no `require` or `assert` enforces this. If the memory store returns errors during concurrent writes (e.g. a race in `SetChild` or `SetLinkCount`), the test continues silently and the PayloadIDSet invariants still pass because concurrent files that failed to commit are simply absent from both the backup and the restored store. A non-zero `writerErrs` would indicate a regression in the memory store's concurrent-write contract but would go undetected.

**Fix:**
```go
require.Zero(t, writerErrs.Load(),
    "writer goroutine must not see errors from the memory store during concurrent writes")
```

---

_Reviewed: 2026-04-18T00:00:00Z_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: standard_
