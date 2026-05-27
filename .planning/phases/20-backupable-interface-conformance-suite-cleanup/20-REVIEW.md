---
phase: 20-backupable-interface-conformance-suite-cleanup
reviewed: 2026-05-27T14:30:00Z
depth: standard
files_reviewed: 7
files_reviewed_list:
  - pkg/blockstore/hashset.go
  - pkg/blockstore/hashset_test.go
  - pkg/metadata/backupable.go
  - pkg/metadata/backupable_test.go
  - pkg/metadata/backup/envelope.go
  - pkg/metadata/backup/envelope_test.go
  - pkg/metadata/storetest/backup_conformance.go
findings:
  critical: 2
  warning: 3
  info: 2
  total: 7
status: issues_found
---

# Phase 20: Code Review Report

**Reviewed:** 2026-05-27T14:30:00Z
**Depth:** standard
**Files Reviewed:** 7
**Status:** issues_found

## Summary

The backup interface, envelope format, HashSet data structure, and conformance suite are generally well-structured with clear separation of concerns. However, there are two critical issues: the envelope Writer accumulates CRC before confirming the underlying write succeeded (data integrity risk on short writes), and the conformance suite calls `t.Fatal` from a non-test goroutine (undefined behavior per Go testing contract). There are also several warnings around dead test code, a weak concurrency test, and unchecked errors in tests.

## Critical Issues

### CR-01: Envelope Writer.Write accumulates CRC before confirming write success

**File:** `pkg/metadata/backup/envelope.go:110-115`
**Issue:** `Writer.Write` unconditionally feeds all of `p` into the CRC accumulator before writing to the underlying `io.Writer`. If the underlying writer performs a short write (returns `n < len(p)` with an error), the CRC has accumulated bytes that were never written to the output. Any retry writing the remaining bytes (`p[n:]`) will cause those bytes to be CRC'd twice. The resulting CRC will not match the actual written stream, producing a silently corrupt backup that fails CRC verification on restore.

This passes tests only because `bytes.Buffer.Write` never short-writes. Any real-world writer (network socket, file under disk pressure, `io.Writer` wrappers with buffering) can produce short writes.

**Fix:**
```go
func (ew *Writer) Write(p []byte) (int, error) {
    n, err := ew.w.Write(p)
    // Only accumulate bytes that were actually written.
    if n > 0 {
        ew.crc.Write(p[:n])
    }
    return n, err
}
```

### CR-02: t.Fatal called from non-test goroutine in ConcurrentWriter conformance test

**File:** `pkg/metadata/storetest/backup_conformance.go:247-256`
**Issue:** `createTestFile(t, ...)` is called inside a goroutine at line 255. `createTestFile` (in `suite.go:115-164`) calls `t.Fatalf` on any failure. The Go testing documentation explicitly states that `Fatal`, `Fatalf`, `FailNow` "must be called only from the goroutine running the test or benchmark function." Calling them from a spawned goroutine invokes `runtime.Goexit()` in the wrong goroutine, which can cause panics, silent goroutine death, or test framework corruption. The `wg.Done()` deferred call may still fire, allowing the test to continue with corrupt state.

**Fix:** Replace the `t.Fatal`-using `createTestFile` with error-returning operations and handle errors locally:
```go
go func() {
    defer wg.Done()
    rootHandle, err := store.GetRootHandle(ctx, shareName)
    if err != nil {
        return // Best effort -- backup may hold a lock.
    }
    // Use direct store operations with error returns instead of
    // createTestFile which calls t.Fatal internally.
    handle, err := store.GenerateHandle(ctx, shareName, "/concurrent-new.bin")
    if err != nil {
        return
    }
    _, id, err := metadata.DecodeFileHandle(handle)
    if err != nil {
        return
    }
    file := &metadata.File{
        ShareName: shareName,
        FileAttr:  metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644, UID: 1000, GID: 1000},
    }
    file.ID = id
    _ = store.PutFile(ctx, file)
    _ = store.SetParent(ctx, handle, rootHandle)
    _ = store.SetChild(ctx, rootHandle, "concurrent-new.bin", handle)
}()
```

## Warnings

### WR-01: ConcurrentWriter test does not actually verify snapshot isolation

**File:** `pkg/metadata/storetest/backup_conformance.go:277-293`
**Issue:** The test claims to verify snapshot isolation ("files created after the backup begins must NOT appear in the restored state"), but the actual assertions (lines 287-292) only check that the two initial files exist. The comment at lines 277-283 explicitly acknowledges the test does not verify the core contract: "Depending on timing, the backup may or may not have captured it." The `hashes` variable is discarded with `_ = hashes` (line 284). This means the test cannot detect a Backup implementation that violates snapshot isolation -- it would pass even if `concurrent-new.bin` appeared in every backup.

A conformance suite that cannot detect the behavior it claims to test provides false confidence.

**Fix:** Either (a) use a synchronization mechanism (channel or barrier) to ensure the concurrent write happens after Backup begins but before it completes, then assert `concurrent-new.bin` is absent; or (b) remove the "ConcurrentWriter" claim and rename the test to reflect what it actually verifies (basic backup/restore stability under concurrent access). At minimum, document this as a known limitation of the suite.

### WR-02: Dead code in TestEnvelope_RoundTrip (abandoned first attempt)

**File:** `pkg/metadata/backup/envelope_test.go:27-84`
**Issue:** Lines 27-58 contain an entire abandoned first read attempt that is acknowledged in comments as incorrect ("We need a different approach: re-read from scratch with proper separation"). The variable `crc` from the first attempt is silenced with `_ = crc` at line 84. This dead code path tests nothing (it never calls VerifyCRC), clutters the test, and -- more importantly -- demonstrates a real API footgun: `ReadHeader` returns a tee reader that will feed CRC bytes into the accumulator if the caller reads past the payload boundary. The test itself fell into this trap.

This footgun suggests the `ReadHeader`/`VerifyCRC` API needs either (a) documentation clarifying that the caller must know the exact payload length, or (b) a redesigned API that handles payload/CRC boundary internally (e.g., `ReadHeader` accepts total stream length, or returns a `LimitedReader` over the payload).

**Fix:** Remove lines 27-84 entirely (the "first attempt"). The second attempt (lines 61-82) correctly demonstrates the API. If the footgun API is intentional, add a doc comment on `ReadHeader` warning that `io.ReadAll(payloadReader)` will corrupt the CRC accumulator.

### WR-03: Unchecked errors from Write and Finish in multiple envelope tests

**File:** `pkg/metadata/backup/envelope_test.go:93-94, 111-112, 130-131`
**Issue:** In `TestEnvelope_BadMagic`, `TestEnvelope_BadVersion`, and `TestEnvelope_Truncated`, calls to `ew.Write([]byte("data"))` and `ew.Finish()` discard both the `(int, error)` and `error` returns respectively. While these are writing to a `bytes.Buffer` and won't fail in practice, this sets a poor example in conformance test code that will be copied by store implementers. The Go vet `errcheck` linter would flag these.

**Fix:**
```go
if _, err := ew.Write([]byte("data")); err != nil {
    t.Fatalf("Write: %v", err)
}
if err := ew.Finish(); err != nil {
    t.Fatalf("Finish: %v", err)
}
```

## Info

### IN-01: HashSet.Hashes() exposes internal map, no production callers

**File:** `pkg/blockstore/hashset.go:66-70`
**Issue:** `Hashes()` returns the internal map by reference, meaning any external mutation is visible to the HashSet. The doc comment warns about this, but a `grep` across the codebase shows zero production callers -- it is only called in `hashset_test.go:123`. Since `ForEach`, `Sorted`, `Contains`, and `Len` cover all read patterns, `Hashes()` is dead API surface that increases the risk of accidental mutation.

**Fix:** Consider removing `Hashes()` entirely, or making it return a copy if external iteration over the raw map is ever needed.

### IN-02: No validation for empty engine tag in envelope NewWriter

**File:** `pkg/metadata/backup/envelope.go:76-78`
**Issue:** `NewWriter` validates that `engineTag` is not longer than 65535 bytes but does not reject an empty string. An empty engine tag produces a valid envelope with `engine_len=0` and zero engine tag bytes. This is technically parseable but semantically meaningless -- every store engine should have a non-empty identifier. A `Restore` receiving an empty tag would fail to match any engine, producing a confusing `ErrEngineMismatch` rather than a clear "empty tag" error.

**Fix:** Add a guard: `if len(engineTag) == 0 { return nil, errors.New("backup: engine tag must not be empty") }`

---

_Reviewed: 2026-05-27T14:30:00Z_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: standard_
