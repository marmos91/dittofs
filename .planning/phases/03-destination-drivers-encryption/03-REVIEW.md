---
phase: 3-destination-drivers-encryption
reviewer: code-reviewer
date: 2026-04-16
branch: feat/v0.13.0-phase-3-destination-drivers-encryption
status: findings-with-applied-fixes
---

# Phase 3 Code Review

## Summary

| Severity | Count | Action |
|----------|-------|--------|
| CRITICAL | 1     | FALSE POSITIVE — rejected |
| HIGH     | 1     | FIX APPLIED |
| MEDIUM   | 2     | FIX APPLIED (M-1), FIX APPLIED (M-2) |

## Findings

### C-1 (rejected): `testKeyHex` alleged to be 60 chars

**Reviewer claim:** `testKeyHex` in `destinationtest/roundtrip.go` is 60 characters, making every encrypted conformance test fail with `ErrInvalidKeyMaterial`.

**Verification:** `awk 'BEGIN{print length("abababababababababababababababababababababababababababababababab")}'` returns `64`. The string is `ab` × 32 = 64 characters. Reviewer miscounted as `ab × 30`. Tests pass precisely because the key is valid.

**Action:** None.

### H-1: `fs/verifyReadCloser.Close()` does not drain before hash check

**File:** `pkg/backup/destination/fs/store.go:448-467`

The fs driver's `Close()` calls `v.vr.Mismatch()` without draining remaining bytes. If Phase 5 closes the reader early (context cancellation, engine error), the SHA-256 hash is computed over partial ciphertext, producing a false `ErrSHA256Mismatch` on a valid backup. The S3 driver correctly drains via `io.Copy(io.Discard, v.vr)`.

D-11 contract: "Reader verifies SHA-256 as it streams and returns ErrSHA256Mismatch on close if the digest differs." The fs driver only satisfies this when the caller fully drains — a behavioral correctness gap.

**Fix applied:** Added `io.Copy(io.Discard, v.r)` before checking `Mismatch()` (drains through the full reader chain so the verifyReader observes complete ciphertext). Matches the S3 driver pattern.

### M-1: Dead branch `strings.HasPrefix(code, "5")` in `classifyS3Error`

**File:** `pkg/backup/destination/s3/errors.go:72`

AWS SDK v2 API error codes are named strings (`"InternalError"`, `"ServiceUnavailable"`), never HTTP status codes starting with `"5"`. The branch is dead — but harmless because the downstream `re.Response.StatusCode >= 500` fallback catches real 5xx responses.

**Fix applied:** Replaced the dead prefix check with explicit named codes (`InternalError`, `ServiceUnavailable`, `RequestTimeout`) that AWS actually returns.

### M-2: Inconsistent empty-SHA256 handling between fs and s3 verifiers

**Files:** `pkg/backup/destination/fs/store.go:431-434`, `pkg/backup/destination/s3/hash.go:77-84`

When `manifest.SHA256 == ""`: the S3 driver silently skips integrity checks, while the fs driver returns a false `ErrSHA256Mismatch` for valid data. For a correctly-written backup this is unreachable (driver always populates SHA256), but a hand-crafted or pre-Phase-3 manifest would split behavior.

**Fix applied:** Aligned both drivers — empty `expected` now always produces `ErrSHA256Mismatch` (fail-closed). This keeps operators safe from silently restoring unverified data and surfaces the missing-hash condition loudly.

## Items Verified Correct

- AES-256-GCM wire format (D-05): frame counter + `data`/`final` tag in AAD; final-tagged truncation detection works
- Key zeroing (D-09): both drivers zero key material after `cipher.NewGCM`
- Nonce freshness: fresh `crypto/rand.Read` per frame
- S3 two-phase commit (D-02): payload multipart → manifest PutObject; `upErr`/`producerErr` priority correct, no goroutine leak
- D-13 prefix collision: reads `cfg["prefix"]` matching `pkg/controlplane/runtime/shares/service.go:1013`; empty-prefix catastrophic case correctly rejected
- Atomic publish (D-03): `os.Rename` is the single publish event, `cleanupTmp = false` set atomically
- 0600/0700 perms (D-14): explicit `Chmod` after `Create`/`Mkdir` (umask-safe)
- Orphan sweep (D-06): bounded by timeout, non-fatal errors
- Error sentinel identity: all 11 via `errors.New`, wrapped with `%w`, `errors.Is`-checkable
- `builtins.RegisterBuiltins` has no `init()`
