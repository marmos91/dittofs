---
phase: 21-per-engine-backup-drivers
reviewed: 2026-05-27T00:00:00Z
depth: standard
files_reviewed: 7
files_reviewed_list:
  - pkg/metadata/store/memory/backup.go
  - pkg/metadata/store/memory/memory_conformance_test.go
  - pkg/metadata/store/badger/backup.go
  - pkg/metadata/store/badger/badger_conformance_test.go
  - pkg/metadata/store/postgres/backup.go
  - pkg/metadata/store/postgres/postgres_conformance_test.go
  - pkg/metadata/storetest/backup_conformance.go
findings:
  critical: 4
  warning: 4
  info: 3
  total: 11
status: issues_found
---

# Phase 21: Code Review Report

**Reviewed:** 2026-05-27
**Depth:** standard
**Files Reviewed:** 7
**Status:** issues_found

## Summary

Three backup driver implementations (memory, badger, postgres) were reviewed alongside the shared conformance suite. The envelope protocol (`pkg/metadata/backup`) is structurally sound: magic bytes, versioning, engine tag routing, and trailing CRC32 are all implemented correctly in isolation. The bugs are in how the drivers sequence CRC verification against persistent writes, in a data race in the memory driver, in OOM-inducing unbounded allocations on untrusted length fields, and in a snapshot-ordering problem that makes the badger `ConcurrentWriter` conformance test unreliable.

---

## Critical Issues

### CR-01: Badger and Postgres restore commit data before verifying CRC

**Files:**
- `pkg/metadata/store/badger/backup.go:232-240`
- `pkg/metadata/store/postgres/backup.go:301-307`

**Issue:** Both drivers flush/commit all restored data to durable storage before calling `backup.VerifyCRC`. If the CRC check then fails, the store is left in a partially-restored or fully-restored-but-corrupt state. The next `Restore` call returns `ErrRestoreDestinationNotEmpty` because the non-empty-check sees the already-written data, making the store permanently unrecoverable without manual intervention. The `ROLLBACK` deferred in the postgres driver has no effect once `COMMIT` has succeeded (line 301).

**Badger sequence:**
```
wb.Flush()       // line 232 — data durably written to Badger
backup.VerifyCRC // line 238 — too late: can't undo the flush
```

**Postgres sequence:**
```
pgRaw.Exec("COMMIT") // line 301 — transaction committed
backup.VerifyCRC     // line 306 — too late: ROLLBACK defer is a no-op after COMMIT
```

**Fix — Badger:** Buffer all KV entries into an in-memory slice, verify CRC before starting any Badger write, then replay the entries:
```go
// After the read loop and before the final Flush:
if err := backup.VerifyCRC(r, acc); err != nil {
    wb.Cancel()
    return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
}
// Flush after CRC is verified.
if err := wb.Flush(); err != nil {
    return fmt.Errorf("%w: flush final batch: %v", metadata.ErrRestoreCorrupt, err)
}
```
For large databases this requires reading the full stream into a staging area (e.g., a temporary Badger DB or an in-memory slice of KV pairs) before committing. Alternatively, read and verify CRC of the byte stream first, then replay from a seekable buffer.

**Fix — Postgres:** Verify CRC before issuing `COMMIT`. Since the payload reader is a `TeeReader` over the original reader, all payload bytes are already accumulated in `acc` by the time the table loop finishes. The 4-byte CRC trailer can be read before committing:
```go
// Verify CRC BEFORE committing.
if err := backup.VerifyCRC(r, acc); err != nil {
    return fmt.Errorf("%w: %v", metadata.ErrRestoreCorrupt, err)
}
// Safe to commit now.
if _, err := pgRaw.Exec(ctx, "COMMIT").ReadAll(); err != nil {
    return fmt.Errorf("restore: commit: %w", err)
}
```

---

### CR-02: Memory backup races on `rollupOffsets` and `synced` maps

**File:** `pkg/metadata/store/memory/backup.go:112-115`

**Issue:** `Backup` holds `s.mu.RLock()` but reads `s.rollupOffsets` (line 113) and `s.synced` (line 114) directly. Both fields are governed by *separate* mutexes (`rollupMu` and `syncedMu` respectively, as documented in `store.go:241-259`). A concurrent `SetRollupOffset` call acquires only `rollupMu` — not `s.mu` — so it can write to `s.rollupOffsets` while `Backup` is reading it under `s.mu.RLock()`. This is an unsynchronized concurrent map access; the Go race detector will flag it.

**Fix:** Acquire both secondary mutexes before reading those fields, while already holding `s.mu.RLock()`:
```go
s.rollupMu.RLock()
snap.RollupOffsets = s.rollupOffsets
s.rollupMu.RUnlock()

s.syncedMu.RLock()
snap.Synced = s.synced
s.syncedMu.RUnlock()
```
These reads must happen inside the `s.mu.RLock()` section (to keep the snapshot consistent with the rest of the store state) but with the secondary locks also held.

---

### CR-03: Unbounded allocation from untrusted length fields

**Files:**
- `pkg/metadata/store/memory/backup.go:254-258` — `payloadLen` is `uint64`; `make([]byte, payloadLen)` will OOM for any crafted stream with `payloadLen > available RAM`.
- `pkg/metadata/store/badger/backup.go:197,211` — `keyLen` and `valLen` are `uint32`; `make([]byte, keyLen)` and `make([]byte, valLen)` each allow up to 4 GiB per allocation per KV entry.

**Issue:** A crafted or truncated backup stream can specify an arbitrarily large length field, causing the process to exhaust memory before any decoding error is detected. In the badger case a single key+value pair costs up to 8 GiB. In the memory case, `payloadLen = 2^63` (maximum `int64` value when cast) will pass the `make` call and the subsequent `io.ReadFull` will fail, but only after the kernel rejects the allocation or the OOM killer fires.

**Fix:** Enforce sane upper bounds before allocation. For memory:
```go
const maxGobPayload = 1 << 30 // 1 GiB, adjust to real-world max
if payloadLen > maxGobPayload {
    return fmt.Errorf("%w: payload too large (%d bytes)", metadata.ErrRestoreCorrupt, payloadLen)
}
```
For badger, Badger's own key size limit is 65,535 bytes; values are bounded by `options.ValueLogFileSize` (default 1 GiB). Reject anything beyond those bounds:
```go
const maxBadgerKeyLen = 65535
const maxBadgerValLen = 1 << 30
if keyLen > maxBadgerKeyLen {
    wb.Cancel()
    return fmt.Errorf("%w: key too large (%d)", metadata.ErrRestoreCorrupt, keyLen)
}
```

---

### CR-04: Badger `ConcurrentWriter` conformance test has a non-deterministic snapshot window

**File:** `pkg/metadata/store/badger/backup.go:51-66`

**Issue:** The `ConcurrentWriter` test uses a `signalWriter` that fires (closes `started`) on the first `Write` call. For the memory driver, the first write happens inside `NewWriter` *after* `s.mu.RLock()` is already held, so the snapshot is guaranteed to be established before the concurrent goroutine runs. For the badger driver, `backup.NewWriter` (which triggers the `signalWriter` signal) is called at **line 51**, before `s.db.View()` (the actual MVCC snapshot) at **line 66**. There is a window between the signal and the snapshot where the concurrent goroutine may write and commit a new file, which then appears inside the `db.View()` snapshot — making the test an incorrect assertion that can both fail (if the concurrent file lands in the snapshot) and spuriously pass.

The postgres driver correctly calls `BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ` before `NewWriter`, so it does not have this problem.

**Fix:** In `badger/backup.go`, move `db.View` to begin *before* writing anything to the envelope writer, and call `NewWriter` only after the read transaction is open:
```go
hs := blockstore.NewHashSet(0)
err = s.db.View(func(txn *badgerdb.Txn) error {
    // Now the MVCC snapshot is established. Open the envelope writer.
    envW, err := backup.NewWriter(w, badgerEngineTag)
    if err != nil { ... }
    // Write schema version, iterate keys, write sentinel ...
    return nil
})
```
The `envW` closure variable must be threaded into the View callback, or the envelope writer must be opened after the transaction is guaranteed to exist.

---

## Warnings

### WR-01: Memory and badger restore TOCTOU on the empty-destination check

**Files:**
- `pkg/metadata/store/memory/backup.go:219-227`
- `pkg/metadata/store/badger/backup.go:143-149`

**Issue:** Both drivers check "is the store empty?" under a short-lived read lock (memory) or a separate `db.View` transaction (badger), then release it and proceed with the envelope read and write lock. A concurrent `CreateShare` call that succeeds between the empty check and the write lock will have its data **silently overwritten** by the restore without an `ErrRestoreDestinationNotEmpty` being returned. The postgres driver has the same problem (line 231: `QueryRow` outside the restore transaction).

**Fix (memory):** Hold `s.mu.Lock()` (write lock) for the entire Restore operation — check emptiness under the write lock so no concurrent mutation can sneak in:
```go
s.mu.Lock()
defer s.mu.Unlock()
if len(s.shares) > 0 {
    return metadata.ErrRestoreDestinationNotEmpty
}
// ... proceed to read envelope and restore state ...
```
Note: for the memory driver, holding the write lock for the full operation is consistent with the current Backup behavior (RLock held for full operation).

**Fix (badger/postgres):** Perform the emptiness check inside the write/restore transaction rather than in a separate read.

---

### WR-02: Postgres restore does not validate `tableCount` against expected schema

**File:** `pkg/metadata/store/postgres/backup.go:263-299`

**Issue:** `tableCount` (a `uint32` from the stream) is used as the loop bound without any cap or comparison against `len(backupTables)`. A stream with an enormous `tableCount` (e.g., `1 << 30`) and all-zero `dataLen` sections for known table names will loop `tableCount` times, successfully processing each known table name and then failing only on unknown names. Even ignoring malicious streams, a schema mismatch (e.g., a backup produced with 10 tables restored against a build expecting 15) silently restores only the 10 tables, leaving the other 5 empty — which the schema version check was supposed to prevent.

**Fix:** Reject counts that don't match the expected value (same version = same count):
```go
if tableCount != uint32(len(backupTables)) {
    return fmt.Errorf("%w: backup has %d tables, expected %d",
        metadata.ErrSchemaVersionMismatch, tableCount, len(backupTables))
}
```

---

### WR-03: `uint64 dataLen` cast to `int64` for `io.LimitReader` silently truncates on overflow

**File:** `pkg/metadata/store/postgres/backup.go:353-356`

**Issue:** `dataLen` is a `uint64` from the stream. The cast `int64(dataLen)` at line 356 wraps to a large negative number for values above `math.MaxInt64`. `io.LimitReader` with a negative `n` returns `EOF` immediately, so `CopyFrom` receives an empty reader. The `dataLen` bytes are not consumed from `payloadR`, silently desynchronizing all subsequent reads. The existing `isKnownTable` check will reject unknown table names found in the now-misaligned stream, but the error message ("unknown table") does not expose the real cause.

**Fix:** Validate before the cast:
```go
const maxTableDataLen = 1 << 40 // 1 TiB; adjust to real-world max
if dataLen > maxTableDataLen {
    return fmt.Errorf("table data too large (%d bytes)", dataLen)
}
dataReader := io.LimitReader(payloadR, int64(dataLen))
```

---

### WR-04: Badger hash extraction silently produces an incomplete `HashSet` on malformed `f:` entries

**File:** `pkg/metadata/store/badger/backup.go:106-115`

**Issue:** When a `f:` (file) key fails JSON unmarshal, the backup logs a warning and continues iteration, excluding that file's block hashes from the returned `HashSet`. The backup stream itself is still written correctly with the raw KV bytes, so restoring from this backup will succeed — but the returned `HashSet` is incomplete. Any GC hold placed using that `HashSet` will miss blocks that belong to the malformed file, making those blocks eligible for garbage collection while the restored store still references them.

Returning an error here rather than continuing is safer in the context of producing a GC hold:

```go
if err := json.Unmarshal(val, &file); err != nil {
    return fmt.Errorf("%w: malformed f: entry %q: %v",
        metadata.ErrBackupAborted, string(key), err)
}
```

If the intent is "best-effort hash collection with partial data", the contract must be documented in the `Backupable` interface and the GC caller must treat a non-nil error as "HashSet is incomplete, do not use for GC hold".

---

## Info

### IN-01: Postgres test reads `DITTOFS_TEST_POSTGRES_DSN` but uses hardcoded connection config

**File:** `pkg/metadata/store/postgres/postgres_conformance_test.go:59-64,69-74`

**Issue:** Both test functions read `connStr := os.Getenv("DITTOFS_TEST_POSTGRES_DSN")` and use it as a presence gate to skip the test when not set. However, `connStr` is never passed to `newPostgresStoreFactory()` and the factory always connects to the hardcoded `localhost:5432/dittofs_test` with username `postgres` / password `postgres`. Setting `DITTOFS_TEST_POSTGRES_DSN` to a non-default DSN has no effect on the actual connection.

**Fix:** Parse the DSN and inject it into the factory config, or at minimum document the skip-only semantics of the env var with a comment explaining that the actual target is always `localhost:5432/dittofs_test`.

---

### IN-02: Postgres conformance tests share a single database — no per-test isolation

**File:** `pkg/metadata/store/postgres/postgres_conformance_test.go:18-55`

**Issue:** `newPostgresStoreFactory` creates a new `*PostgresMetadataStore` for each subtest, but all instances connect to the same database (`dittofs_test`). Share names created by one subtest (e.g., `rt-bkp` from `RoundTrip`) persist and are visible to subsequent subtests that use a different store instance. If subtests run in parallel or if a test fails without cleanup, later subtests may encounter unexpected state.

**Fix:** Each factory call should create an isolated schema (e.g., `CREATE SCHEMA test_<ulid>; SET search_path TO test_<ulid>`) and drop it in `t.Cleanup`. Alternatively, use `t.TempDir()`-style unique database names created per test run.

---

### IN-03: `ctx.Err()` in `Restore` is wrapped with `ErrRestoreCorrupt` rather than a dedicated abort sentinel

**Files:**
- `pkg/metadata/store/memory/backup.go:215-217`
- `pkg/metadata/store/badger/backup.go:179-181` (ctx check in loop)
- `pkg/metadata/store/postgres/backup.go:225-227`

**Issue:** A context cancellation (operator timeout, shutdown signal) is wrapped as `ErrRestoreCorrupt`, which implies the backup stream is bad. Callers that log `ErrRestoreCorrupt` for investigation will receive false alarms. The `Backupable` contract document (`backupable.go`) defines `ErrBackupAborted` for cancelled backups but provides no equivalent for cancelled restores; the result is that context cancellation in `Restore` is indistinguishable from a genuinely corrupt stream. A new sentinel (e.g., `ErrRestoreAborted`) would clarify the distinction, or at minimum the context error should be surfaced in the wrapped message more clearly.

---

_Reviewed: 2026-05-27_
_Reviewer: Claude (gsd-code-reviewer)_
_Depth: standard_
