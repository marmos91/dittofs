---
status: investigating
trigger: "PostgreSQL POSIX compliance test (pjdfstest) times out during CI. Hangs after completing tests/open/24.t successfully."
created: 2026-01-31T10:00:00Z
updated: 2026-01-31T18:30:00Z
---

## Current Focus

**CRITICAL INSIGHT: All PostgreSQL operations have 10-second timeouts, but NONE are firing during the 1+ hour hang.**

This definitively proves the hang is NOT in any PostgreSQL/database operation.

hypothesis: The hang is in a non-database operation that differs between PostgreSQL and other stores
test: Job 62089600222 is currently running (started 15:47:43, still in_progress as of 17:30)
expecting: Either CI times out (60min limit) or we identify a non-database blocking point
next_action: Once CI completes/times out, examine logs to identify exact blocking location

## Symptoms

expected: PostgreSQL POSIX compliance tests complete successfully like memory and badger stores
actual: Test hangs indefinitely after tests/open/24.t completes (shows "ok" with timing, then just "." and hangs)
errors: Timeout in CI - no explicit error message, just hangs
reproduction: Run POSIX compliance tests with PostgreSQL metadata store in CI (PR #109)
started: Currently failing on PR https://github.com/marmos91/dittofs/pull/109

## Eliminated

- hypothesis: CreateRootDirectory (shares.go:266) causes hang via pool.Begin without timeout
  evidence: CreateRootDirectory is only called during share loading at startup, not during test execution
  timestamp: 2026-01-31T10:30:00Z

- hypothesis: Pool exhaustion causing indefinite blocking on connection acquire
  evidence: Applied 10-second connection acquire timeouts to ALL pool operations via pool_helpers.go. Also fixed config parsing to use max_conns=50. Job 62084606023 STILL hung at exact same spot after open/24.t (13:01:13Z). If it were pool exhaustion, we'd see timeout errors, not silent hang.
  timestamp: 2026-01-31T16:00:00Z

## Evidence

- timestamp: 2026-01-31T10:05:00Z
  checked: GitHub Actions workflow run 21520984330 logs
  found: open/24.t completes at 15:32:16, then no output until cancellation at 16:28:16 (nearly 1 hour hang)
  implication: Hang occurs after open/24.t completes, before open/25.t can start

- timestamp: 2026-01-31T10:08:00Z
  checked: Memory and BadgerDB test logs
  found: Both proceed immediately from open/24.t to open/25.t (25ms, 30ms respectively)
  implication: PostgreSQL-specific issue, not test harness or NFS protocol

- timestamp: 2026-01-31T10:10:00Z
  checked: shares.go CreateRootDirectory function
  found: Line 266 uses s.pool.Begin(ctx) WITHOUT connection acquire timeout
  implication: Unlike WithTransaction (which has 10s timeout), this can block indefinitely

- timestamp: 2026-01-31T10:12:00Z
  checked: WithTransaction in transaction.go
  found: Uses connectionAcquireTimeout (10s) with context.WithTimeout before pool.Begin
  implication: Inconsistent timeout handling between different pool operations

- timestamp: 2026-01-31T10:14:00Z
  checked: All direct pool operations
  found: Many direct s.pool.QueryRow/Query/Exec calls without explicit timeout
  implication: Any of these could block if pool is exhausted

- timestamp: 2026-01-31T10:25:00Z
  checked: init.go CreateMetadataStore for postgres case
  found: Only parses host, port, database, user, password, sslmode - does NOT parse max_conns, min_conns, query_timeout
  implication: Setup script passes max_conns:50 but store uses default MaxConns=10

- timestamp: 2026-01-31T10:28:00Z
  checked: Default PostgreSQL config values
  found: MaxConns=10, QueryTimeout=30s (but statement_timeout only affects query execution, not connection acquisition)
  implication: With only 10 connections and high concurrency, pool exhaustion is more likely

- timestamp: 2026-01-31T10:32:00Z
  checked: All rows.Close() calls after Query operations
  found: All Query operations have proper defer rows.Close()
  implication: No obvious connection leak from Query operations

- timestamp: 2026-01-31T10:40:00Z
  checked: NFS adapter context creation (nfs_adapter.go:278)
  found: shutdownCtx is context.WithCancel(context.Background()) - NO TIMEOUT
  implication: Context passed to handlers and store operations has no deadline

- timestamp: 2026-01-31T10:45:00Z
  checked: pgxpool acquire timeout support (GitHub discussions)
  found: pgxpool has NO built-in acquire timeout - must use context timeout for each operation
  implication: WithTransaction pattern is correct; direct pool operations need same treatment

- timestamp: 2026-01-31T10:50:00Z
  checked: WithTransaction implementation
  found: Creates ctx with 10s timeout before pool.Begin, returns "connection acquire timeout" error
  implication: This is the correct pattern - need to apply to all pool operations

- timestamp: 2026-01-31T17:00:00Z
  checked: All poolRows.Close() patterns in postgres store
  found: All Query operations have proper defer rows.Close() and poolRows wrapper releases connection
  implication: No connection leak from Query operations

- timestamp: 2026-01-31T17:05:00Z
  checked: poolRow.Scan() releases connection
  found: All queryRow usages either call .Scan() directly or pass to fileRowToFileWithNlink which calls .Scan()
  implication: No connection leak from QueryRow operations

- timestamp: 2026-01-31T17:10:00Z
  checked: sync.Cond.Wait() in transfer manager (ioCond, downloadsPending)
  found: downloadsPending is never modified (always 0), so waitForDownloads() loop exits immediately
  implication: NOT a condition variable deadlock

- timestamp: 2026-01-31T17:15:00Z
  checked: WaitGroup patterns in NFS adapter
  found: c.wg.Add(1) at line 112, c.wg.Done() in handleRequestPanic deferred at line 114
  implication: If processRequest blocks forever, wg.Done() never called, but this would happen for all stores

- timestamp: 2026-01-31T17:20:00Z
  checked: FOR UPDATE/SHARE/LOCK statements in PostgreSQL store
  found: None - no explicit row-level locking in any queries
  implication: NOT a PostgreSQL lock contention issue

- timestamp: 2026-01-31T17:25:00Z
  checked: COMMIT handler (commit.go) flush operations
  found: payloadSvc.Flush() is non-blocking for large files (spawns goroutine with context.Background())
  implication: COMMIT shouldn't block - flush is async

- timestamp: 2026-01-31T17:30:00Z
  checked: CRITICAL INSIGHT - 10s pool timeout did NOT fire
  found: Pool timeout only fires on pool.Acquire/pool.Begin operations
  implication: Code is NOT blocked waiting for pool connection. Must be blocked on something else entirely.

- timestamp: 2026-01-31T17:45:00Z
  checked: Transaction methods in objects.go (lines 411-440)
  found: Transaction methods like GetBlock, PutBlock, etc. call tx.store.* methods which acquire NEW pool connections instead of using the transaction's connection (tx.tx)
  implication: POTENTIAL ISSUE - Within a transaction, object operations acquire separate connections, which could cause:
    1. Connection starvation (transaction holds 1 conn, operations need more)
    2. Data inconsistency (reads outside transaction may see uncommitted data)
    3. With 50 connections and many concurrent ops, pool could still exhaust
  note: This is inconsistent with file operations (GetFile, PutFile) which use tx.tx directly

- timestamp: 2026-01-31T17:50:00Z
  checked: Current CI run (job 62089600222)
  found: "Run POSIX tests" step still in_progress after ~20 minutes
  implication: Either tests are slow with PostgreSQL backend OR hung at same point. Need to wait for completion/timeout to confirm.

- timestamp: 2026-01-31T18:30:00Z
  checked: CI job 62089600222 status (run 21546881400)
  found: Still in_progress after 2+ hours (started 15:47:43). "Run POSIX tests" step is the current step.
  implication: Tests are DEFINITELY hanging. Commit/Rollback timeout fix did NOT resolve the issue. The hang is NOT in any PostgreSQL operation since all have 10s timeouts.

- timestamp: 2026-01-31T18:30:00Z
  checked: open/24.t test content
  found: Tests opening UNIX domain socket files (bind, open with O_RDONLY/WRONLY/RDWR, unlink). Creates socket-type file via MKNOD.
  implication: Socket file metadata operations are involved, but test completes successfully before hang.

- timestamp: 2026-01-31T18:35:00Z
  checked: PayloadService.Flush() implementation
  found: Flush is non-blocking - spawns goroutine with context.Background() for uploads. invokeFinalizationCallback is called but callback is nil (never registered).
  implication: Flush operations cannot be causing the hang.

- timestamp: 2026-01-31T18:35:00Z
  checked: NFS request handling flow
  found: requestSem properly acquired/released via handleRequestPanic. wg.Add/Done properly paired. Context cancellation checks throughout.
  implication: No obvious blocking in NFS layer.

## Resolution

root_cause: **UNKNOWN - Hang is NOT in any PostgreSQL operation**

### Eliminated Root Causes

1. **Pool exhaustion** - ELIMINATED
   - Fix applied: pool_helpers.go with 10-second acquire timeouts
   - Result: Tests STILL hang - no timeout errors observed

2. **Commit/Rollback blocking** - ELIMINATED
   - Fix applied: 10-second timeouts for tx.Commit() and tx.Rollback()
   - Result: Tests STILL hang - no timeout errors observed

3. **Connection leaks** - ELIMINATED
   - Verified: All rows.Close() properly called
   - Verified: All QueryRow.Scan() properly called

4. **Row-level locking** - ELIMINATED
   - Checked: No FOR UPDATE/SHARE/LOCK statements in PostgreSQL store

5. **Condition variable deadlock** - ELIMINATED
   - Checked: downloadsPending never modified, waitForDownloads() exits immediately

### Key Insight

**All PostgreSQL operations have 10-second timeouts. The 1+ hour hang means NO PostgreSQL operation is blocking.**

The hang MUST be in:
- NFS layer (but same code for all stores?)
- Kernel NFS client behavior
- Test harness (prove) behavior
- Race condition that only manifests with PostgreSQL timing
- Memory/resource accumulation specific to PostgreSQL

### Current Status

- CI job 62089600222 was cancelled after 2+ hours
- Logs confirmed hang happens AFTER open/24.t completes (15:51:49), BEFORE open/25.t starts
- 44 minutes of complete silence in logs between test completion and cancellation
- Memory store: 24.t→25.t transition in ~39ms
- BadgerDB store: 24.t→25.t transition in ~39ms
- PostgreSQL store: 24.t→25.t transition NEVER happens (hangs indefinitely)

### Timing Analysis

```
PostgreSQL:
15:51:49.643 - open/24.t completes (49ms, all 5 pass)
              ...44 minutes of nothing...
16:35:58.492 - "The operation was canceled" (manual cancellation)

Memory:
15:50:51.166 - open/24.t completes (27ms)
15:50:51.205 - open/25.t starts (~39ms later)

BadgerDB:
15:50:54.247 - open/24.t completes (26ms)
15:50:54.286 - open/25.t starts (~39ms later)
```

### Next Steps

1. Add debugging to CI workflow:
   - Background monitoring script (nfsstat, ps, server activity)
   - strace on prove (with timeout)
   - Server request logging with timestamps
2. Run specific tests (open/24.t and open/25.t only) to isolate issue
3. Check if hang is in `prove` (Perl test harness) or NFS kernel client

### files_changed
- pkg/metadata/store/postgres/pool_helpers.go (CREATED - Phase 1)
- pkg/metadata/store/postgres/files.go (updated pool operations - Phase 1)
- pkg/metadata/store/postgres/shares.go (updated pool operations - Phase 1)
- pkg/metadata/store/postgres/server.go (updated pool operations - Phase 1)
- pkg/metadata/store/postgres/objects.go (updated pool operations - Phase 1)
- pkg/metadata/store/postgres/transaction.go (added Commit/Rollback timeouts - Phase 2)
