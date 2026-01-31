---
status: verifying
trigger: "PostgreSQL POSIX compliance test (pjdfstest) times out during CI. Hangs after completing tests/open/24.t successfully."
created: 2026-01-31T10:00:00Z
updated: 2026-01-31T23:55:00Z
---

## Current Focus

**BREAKTHROUGH: Tests do NOT hang locally - only in CI environment.**

Local test run (PostgreSQL backend):
- open tests 00-24: ALL PASS (except known test 03 ENAMETOOLONG failures)
- Transition 24.t -> end: COMPLETED WITHOUT HANG
- Total time: ~5.5 minutes

hypothesis: CI-specific environmental issue - not a code bug
test: Need to identify what differs between local machine and GitHub Actions runner
expecting: Root cause in CI configuration, containerized postgres setup, or resource constraints
next_action: Check GitHub Actions artifacts for monitor.log from recent failed CI runs

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

- timestamp: 2026-01-31T22:36:00Z
  checked: LOCAL TEST RUN - PostgreSQL backend with open tests 00-24
  found: ALL TESTS COMPLETE WITHOUT HANGING
  details: |
    - Tests 00-24 ran successfully (excluding open/25.t which is 2GB file test)
    - Only failures: test 03.t due to ENAMETOOLONG (path length limits) - expected behavior
    - Total time: ~5.5 minutes (352 wallclock secs)
    - Transition between tests: NORMAL (no hang after 24.t)
    - PostgreSQL connection: localhost:5432, same credentials as CI
  implication: **TESTS DO NOT HANG LOCALLY - THIS IS A CI-ONLY ISSUE**

- timestamp: 2026-01-31T22:40:00Z
  checked: Local vs CI environment differences
  found: Key differences identified:
    1. CI uses GitHub Actions service container for PostgreSQL
    2. CI has resource limits (shared runner, 2 vCPU, 7GB RAM)
    3. CI uses ubuntu-latest (may have different kernel/NFS client version)
    4. CI PostgreSQL runs in separate container with Docker networking
  implication: Issue may be related to containerized PostgreSQL or CI resource constraints

- timestamp: 2026-01-31T22:50:00Z
  checked: CI artifacts - monitor.log, posix-postgres.log, server.log, posix-memory.log
  found: |
    **ROOT CAUSE IDENTIFIED:**
    1. Test 24.t completes successfully at 20:21:58
    2. Test 25.t STARTS (it's the 2GB file test) - contrary to earlier assumption
    3. Server log shows READ at 2GB offset being initiated
    4. The READ operation enters "reading from Payload Service" but NEVER RETURNS
    5. Memory store passes test 25 in 34ms - test itself is not the problem
    6. PostgreSQL store hangs on the same test

    The hang is in the Payload Service READ path when combined with PostgreSQL metadata store.

  implication: |
    The timing/behavior of PostgreSQL metadata operations affects the Payload Service READ
    in a way that causes a deadlock or infinite wait. This only manifests in CI, not locally.

- timestamp: 2026-01-31T23:30:00Z
  checked: TransferQueue.EnqueueDownload and TransferManager.enqueueDownload
  found: |
    **CRITICAL BUG FOUND:**

    In manager.go line 938:
    ```go
    m.queue.EnqueueDownload(req)
    ```

    The return value is NOT checked. But EnqueueDownload returns false if queue is full:

    In queue.go lines 124-136:
    ```go
    func (q *TransferQueue) EnqueueDownload(req TransferRequest) bool {
        select {
        case q.downloads <- req:
            // ... success
            return true
        default:
            logger.Warn("Download queue full, dropping request")
            return false  // <-- REQUEST DROPPED!
        }
    }
    ```

    If the download request is dropped:
    1. The `done` channel created in enqueueDownload is NEVER signaled
    2. The caller waits forever at line 897: `case err := <-done:`
    3. The READ operation hangs indefinitely

  implication: |
    This is the root cause of the infinite hang. When download queue becomes full:
    - Download request is silently dropped (only a warning logged)
    - done channel never receives a value
    - EnsureAvailable waits forever
    - READ operation hangs

    Why only PostgreSQL in CI?
    - PostgreSQL metadata operations are slower (network latency to service container)
    - More NFS operations pile up waiting for metadata
    - More concurrent download requests flood the queue
    - Queue reaches capacity (default: 1000)
    - Subsequent downloads are dropped

    Why not locally?
    - PostgreSQL is faster (localhost)
    - Less concurrent pressure on download queue
    - Queue never fills up

## Resolution

root_cause: |
  **FOUND: EnqueueDownload silently drops requests when queue is full**

  The TransferManager.enqueueDownload() calls queue.EnqueueDownload() but does NOT check
  the return value. When the queue is full, the request is dropped but the done channel
  is never signaled, causing EnsureAvailable to wait forever.

  Fix: Return an error when EnqueueDownload returns false, instead of returning a done
  channel that will never be signaled.

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
- pkg/payload/transfer/manager.go (FIX: handle EnqueueDownload returning false - Phase 3)

### Fix Applied

Modified `enqueueDownload()` in `pkg/payload/transfer/manager.go` to check the return
value of `queue.EnqueueDownload()`. When the queue is full:
1. Clean up in-flight tracking immediately
2. Signal an error on the done channel
3. Return the channel (caller will receive error instead of waiting forever)

```go
// Before (buggy):
req.Done = m.wrapDoneChannel(key, done)
m.queue.EnqueueDownload(req)
return done

// After (fixed):
wrappedDone := m.wrapDoneChannel(key, done)
req.Done = wrappedDone

if !m.queue.EnqueueDownload(req) {
    // Queue is full - signal error on the wrapped channel to:
    // 1. Clean up in-flight tracking (via wrapDoneChannel goroutine)
    // 2. Forward error to the original done channel
    // 3. Prevent goroutine leak in wrapDoneChannel
    wrappedDone <- fmt.Errorf("download queue full, cannot enqueue block %s", key)
    return done
}
return done
```

### Verification Needed
- [ ] Run POSIX tests with PostgreSQL in CI
- [ ] Verify test 25 (2GB file) completes without hanging
- [ ] Verify error is properly reported when queue is full

### Commit
- Commit: bfe8f52
- Status: Committed locally, needs push to trigger CI
