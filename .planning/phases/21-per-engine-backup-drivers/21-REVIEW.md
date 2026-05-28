---
phase: 21-per-engine-backup-drivers
reviewed: 2026-05-27T00:00:00Z
depth: standard
files_reviewed: 7
files_reviewed_list:
  - pkg/metadata/store/memory/backup.go
  - pkg/metadata/store/memory/memory_conformance_test.go
  - pkg/metadata/storetest/backup_conformance.go
  - pkg/metadata/store/badger/backup.go
  - pkg/metadata/store/badger/badger_conformance_test.go
  - pkg/metadata/store/postgres/backup.go
  - pkg/metadata/store/postgres/postgres_conformance_test.go
findings:
  critical: 3
  warning: 3
  info: 1
  total: 7
status: issues_found
---

# Phase 21: Code Review Report

**Reviewed:** 2026-05-27T00:00:00Z
**Depth:** standard
**Files Reviewed:** 7
**Status:** issues_found

## Summary

Phase 21 introduces per-engine backup drivers for the memory, Badger, and Postgres metadata
stores, plus a shared conformance suite in `pkg/metadata/storetest/backup_conformance.go`.
The envelope format, hash extraction logic, and error sentinel usage are generally sound.
Three correctness defects stand out: a check-then-act (TOCTOU) race in the memory Restore,
a partial-commit window in the Badger batch-flush loop that contradicts the documented
atomicity guarantee, and a fundamental test-isolation failure in the Postgres conformance
wiring that makes the backup tests functionally untestable.

---

## Critical Issues

### CR-01: TOCTOU race in `MemoryMetadataStore.Restore` — empty check and write are not atomic

**File:** `pkg/metadata/store/memory/backup.go:247-317`

**Issue:** The emptiness check is performed under a brief read lock (lines 247-252), which is
released before gob decoding begins. The write lock is not acquired until line 317, after all
I/O and decoding has completed. In the window between the read-unlock (line 249) and the
write-lock (line 317), another goroutine can legally call `CreateShare` or a second concurrent
`Restore` and populate the store. When the write lock is finally acquired, the in-flight
Restore overwrites that data silently, or a second concurrent Restore can both pass the empty
check and then race to write, producing an indeterminate merged state.

The Badger and Postgres drivers do not have this problem because their empty checks and writes
are either both within the same transaction or the storage engine enforces atomicity externally.

**Fix:** Acquire the write lock once and re-validate emptiness under it before populating:

```go
s.mu.Lock()
defer s.mu.Unlock()

// Re-check emptiness now that the write lock is held.
if len(s.shares) > 0 {
    return metadata.ErrRestoreDestinationNotEmpty
}
// ... populate store fields ...
```

For a non-blocking Restore (avoids holding the write lock during stream I/O), decode the
stream with no lock held, then acquire the write lock, re-check empty, and atomically swap in
the decoded snapshot.

---

### CR-02: Badger `Restore` partial-commit corrupts store on mid-batch `WriteBatch.Flush` failure

**File:** `pkg/metadata/store/badger/backup.go:261-280`

**Issue:** The implementation collects all KV entries in memory and verifies the CRC before
writing (correct). It then writes to Badger in batches of `restoreBatchSize` (10 000) entries
using a `WriteBatch`. When the intermediate `wb.Flush()` at line 270 succeeds, those entries
are durably committed to Badger. If a subsequent `wb.SetEntry()` call fails (e.g., disk full,
Badger internal error), `wb.Cancel()` is called and the function returns an error — but the
previously flushed batch is already committed. The store is left with partial data: non-empty
(`isStoreEmpty()` returns false) but missing later entries.

The function-level comment at lines 163-168 explicitly promises:

> "A corrupt stream leaves the store empty and retryable."

This guarantee is violated the moment any intermediate `Flush()` succeeds and the next
`SetEntry` or `Flush` fails.

**Fix:** Use a single Badger read-write transaction (`db.Update`) instead of `WriteBatch`.
`WriteBatch` documents that it does not provide transactional semantics. For metadata stores
measured in megabytes, a single transaction is acceptable:

```go
err := s.db.Update(func(txn *badgerdb.Txn) error {
    for _, e := range entries {
        if err := txn.Set(e.Key, e.Value); err != nil {
            return fmt.Errorf("set entry: %w", err)
        }
    }
    return nil
})
if err != nil {
    return fmt.Errorf("%w: write entries: %v", metadata.ErrRestoreCorrupt, err)
}
```

If `WriteBatch` must be kept for large stores, remove the "retryable" claim from the comment
and add explicit per-batch rollback/cleanup logic.

---

### CR-03: Postgres conformance test factory ignores `DITTOFS_TEST_POSTGRES_DSN`; `srcStore` and `dstStore` share the same database, making backup tests non-functional

**File:** `pkg/metadata/store/postgres/postgres_conformance_test.go:18-55`

**Issue:** Two separate bugs combine to make the Postgres backup conformance tests
non-functional:

**Bug A — Dead env var.** `TestConformance` and `TestBackupConformance` read
`DITTOFS_TEST_POSTGRES_DSN` only to decide whether to skip. `newPostgresStoreFactory` never
uses this value; it always connects to `localhost:5432 / dittofs_test / postgres / postgres`.
If the env var points to a different host, the tests silently connect to the wrong server.

**Bug B — Shared database breaks RoundTrip (and most other subtests).** Every `factory(t)`
call opens a new connection pool to the same physical `dittofs_test` database. There is no
per-store schema isolation. When `testBackup_RoundTrip` executes:

```
populateTestData(srcStore, ...)   // writes shares/files into dittofs_test
dstStore.Restore(ctx, &buf)
// Restore checks: SELECT EXISTS(SELECT 1 FROM shares) → TRUE
//                 → returns ErrRestoreDestinationNotEmpty immediately
```

The Restore always fails because `srcStore` and `dstStore` share the underlying database.
Consequently, `RoundTrip`, `ConcurrentWriter`, and `HashSetCorrectness` all fail. The
`NonEmptyDest` subtest appears to pass but for the wrong reason: it finds the previous
subtest's data rather than its own intentionally populated destination.

**Fix for Bug A:** Use the DSN to construct the config:

```go
dsn := os.Getenv("DITTOFS_TEST_POSTGRES_DSN")
if dsn == "" {
    t.Skip("DITTOFS_TEST_POSTGRES_DSN not set")
}
cfg, err := postgres.ParseDSN(dsn) // or inline the parsing
```

**Fix for Bug B:** Isolate each `factory(t)` call with a unique Postgres schema:

```go
schemaName := "bkp_test_" + strings.ToLower(ulid.Make().String())
// CREATE SCHEMA schemaName; SET search_path = schemaName
t.Cleanup(func() { dropSchema(schemaName) })
```

---

## Warnings

### WR-01: `WriteBatch` not cancelled on intermediate `Flush` error — potential goroutine leak

**File:** `pkg/metadata/store/badger/backup.go:269-272`

**Issue:** When the intermediate `wb.Flush()` call at line 270 returns an error, the function
returns without calling `wb.Cancel()`. In Badger v4, `WriteBatch` has background goroutines
that may be leaked if `Cancel()` is never called. This is a separate concern from CR-02
(partial commit); even after CR-02 is resolved, callers should cancel on all error paths.

**Fix:**
```go
if err := wb.Flush(); err != nil {
    wb.Cancel()
    return fmt.Errorf("%w: flush batch: %v", metadata.ErrRestoreCorrupt, err)
}
```

---

### WR-02: Memory `Backup` holds `s.mu.RLock` across entire gob encoding, blocking all mutations for the duration

**File:** `pkg/metadata/store/memory/backup.go:101-236`

**Issue:** `s.mu.RLock()` is acquired at line 101 and released via `defer` at line 102. It is
held for the entire duration of gob encoding (lines 213-216). For a store with thousands of
files, gob encoding can run for hundreds of milliseconds. During this window every write
operation (`PutFile`, `SetChild`, `CreateShare`, etc.) that needs `s.mu.Lock()` is blocked.
Backup effectively serialises with all writes.

The `ConcurrentWriter` conformance test passes because of this blocking, not because the
backup maintains a true snapshot: the concurrent write is simply prevented from running, not
observed and excluded. If the locking strategy were ever changed, the test would no longer
verify the stated isolation property.

**Fix:** Shallow-copy all live maps into snapshot-owned maps while the lock is held, then
release the lock before gob encoding:

```go
s.mu.RLock()
snap := shallowCopySnapshot(s)  // copies all maps under the read lock
s.mu.RUnlock()                  // release before any I/O

var gobBuf bytes.Buffer
if err := gob.NewEncoder(&gobBuf).Encode(&snap); err != nil { ... }
```

The shallow copy is safe because map values in these stores are either immutable value types
or pointer types where the pointed-to objects are not mutated in-place after insertion.

---

### WR-03: Badger `Restore` atomicity comment is factually incorrect

**File:** `pkg/metadata/store/badger/backup.go:163-168`

**Issue:** The block comment states "A corrupt stream leaves the store empty and retryable."
As described in CR-02, this is false after any intermediate `WriteBatch.Flush()` succeeds.
Incorrect documentation misleads future maintainers about recovery semantics and makes it
harder to diagnose partial-restore failures in production.

**Fix:** Until CR-02 is resolved, replace the comment with an accurate description:

```
// Note: phase-3 writes use WriteBatch which does not provide transactional
// atomicity. If a mid-batch Flush succeeds and a later write fails, the store
// will contain partial data. A full transactional write path is tracked in
// the phase-21 follow-up issue.
```

---

## Info

### IN-01: Misplaced godoc comment on `TestBackupConformance` in Badger conformance test

**File:** `pkg/metadata/store/badger/badger_conformance_test.go:34-37`

**Issue:** Lines 34-36 read:

```go
// TestBadgerStore_PutGetFile_BlocksRoundTrip verifies FileAttr.Blocks
// (Phase 12 META-01) round-trips through the public Badger backend API
// — i.e. through PutFile/GetFile end-to-end, not just the JSON encoder.
func TestBackupConformance(t *testing.T) {
```

The comment describes `TestBadgerStore_PutGetFile_BlocksRoundTrip` (defined at line 51) but
is attached as the godoc for `TestBackupConformance`. This appears to be a copy-paste from the
wrong function.

**Fix:** Remove lines 34-36 (or relocate the comment to line 51 where
`TestBadgerStore_PutGetFile_BlocksRoundTrip` is defined).

---

_Reviewed: 2026-05-27T00:00:00Z_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: standard_
