---
phase: 02-nlm-protocol
verified: 2026-02-05T11:30:00Z
status: gaps_found
score: 3.5/4 must-haves verified
gaps:
  - truth: "Prometheus metrics track NLM operations"
    status: partial
    reason: "Metrics struct defined but not wired into adapter or handlers"
    artifacts:
      - path: "internal/protocol/nlm/metrics.go"
        issue: "Metrics created but not yet instantiated or passed to handlers; follow-up tracked as future work"
    missing:
      - "Instantiate nlm.Metrics in NFS adapter SetRuntime()"
      - "Pass metrics to Handler via NewHandler()"
      - "Record metrics in dispatch.go or handlers"
---

# Phase 2: NLM Protocol Verification Report

**Phase Goal:** Implement the Network Lock Manager protocol (RPC 100021) for NFSv3 locking
**Verified:** 2026-02-05T11:30:00Z
**Status:** gaps_found
**Re-verification:** No — initial verification

## Goal Achievement

### Observable Truths

| #   | Truth                                                                        | Status       | Evidence                                                                                      |
| --- | ---------------------------------------------------------------------------- | ------------ | --------------------------------------------------------------------------------------------- |
| 1   | NFSv3 client can acquire and release byte-range locks via fcntl()           | ✓ VERIFIED   | LockFileNLM/UnlockFileNLM methods call lock manager, handlers route to metadata service      |
| 2   | Blocking lock requests queue and notify when lock becomes available         | ✓ VERIFIED   | BlockingQueue.Enqueue() in lock.go, processNLMWaiters() in adapter, callback via granted.go  |
| 3   | NLM_TEST correctly reports lock conflicts with owner information             | ✓ VERIFIED   | TestLockNLM returns conflict with holder info, test.go handler returns NLM4Denied with holder |
| 4   | Lock cancellation stops pending blocking locks                              | ✓ VERIFIED   | BlockingQueue.Cancel() in queue.go, handler calls it in cancel.go                            |
| 5   | Prometheus metrics track NLM operations                                      | ⚠️ PARTIAL   | Metrics struct exists in metrics.go but never instantiated or wired to handlers               |

**Score:** 3.5/4 truths verified (4 fully verified, 1 partial)

### Required Artifacts

#### Plan 02-01: XDR Utilities and NLM Types

| Artifact                                       | Expected                                              | Status     | Details                                                                   |
| ---------------------------------------------- | ----------------------------------------------------- | ---------- | ------------------------------------------------------------------------- |
| `internal/protocol/xdr/decode.go`              | Shared DecodeOpaque, DecodeString functions           | ✓ VERIFIED | 160 lines, exports all expected functions, no stubs                       |
| `internal/protocol/xdr/encode.go`              | Shared WriteXDROpaque, WriteXDRString, WriteXDRPadding| ✓ VERIFIED | 198 lines, exports all expected functions, no stubs                       |
| `internal/protocol/nlm/types/constants.go`     | NLM program number, procedure numbers, status codes   | ✓ VERIFIED | 241 lines, ProgramNLM=100021, all procedures defined                      |
| `internal/protocol/nlm/types/types.go`         | NLM4Lock, NLM4Holder, request/response args           | ✓ VERIFIED | 384 lines (exceeds 100-line minimum), comprehensive types                 |
| `internal/protocol/nlm/xdr/decode.go`          | NLM request decoders                                  | ✓ VERIFIED | 486 lines, exports all expected decode functions                          |
| `internal/protocol/nlm/xdr/encode.go`          | NLM response encoders                                 | ✓ VERIFIED | 375 lines, exports all expected encode functions                          |

#### Plan 02-02: NLM Dispatcher and Handlers

| Artifact                                          | Expected                                          | Status     | Details                                                                   |
| ------------------------------------------------- | ------------------------------------------------- | ---------- | ------------------------------------------------------------------------- |
| `internal/protocol/nlm/handlers/handler.go`       | NLM Handler struct with MetadataService reference | ✓ VERIFIED | 46 lines, NewHandler() takes metadataService, substantive                 |
| `internal/protocol/nlm/dispatch.go`               | NLM dispatch table mapping procedures to handlers | ✓ VERIFIED | 247 lines, NLMDispatchTable maps all procedures                           |
| `pkg/adapter/nfs/nfs_connection.go`               | NLM program routing in handleRPCCall              | ✓ VERIFIED | case rpc.ProgramNLM at line 363, calls handleNLMProcedure                 |
| `pkg/metadata/service.go`                         | NLM-specific lock methods                         | ✓ VERIFIED | LockFileNLM, UnlockFileNLM, TestLockNLM, CancelBlockingLock methods exist|

#### Plan 02-03: Blocking Queue and Callbacks

| Artifact                                          | Expected                                  | Status     | Details                                                                   |
| ------------------------------------------------- | ----------------------------------------- | ---------- | ------------------------------------------------------------------------- |
| `internal/protocol/nlm/blocking/queue.go`         | Per-file blocking lock queue              | ✓ VERIFIED | 185 lines, Enqueue/Cancel methods, per-file queues                        |
| `internal/protocol/nlm/blocking/waiter.go`        | Waiter entry with callback info           | ✓ VERIFIED | Waiter struct with callback address, thread-safe cancellation             |
| `internal/protocol/nlm/callback/client.go`        | TCP callback client for NLM_GRANTED       | ✓ VERIFIED | 232 lines, SendGrantedCallback with 5s timeout                            |
| `internal/protocol/nlm/metrics.go`                | NLM Prometheus metrics                    | ⚠️ ORPHANED| 151 lines, defines metrics but never instantiated or passed to handlers   |

### Key Link Verification

| From                                          | To                            | Via                           | Status     | Details                                                                   |
| --------------------------------------------- | ----------------------------- | ----------------------------- | ---------- | ------------------------------------------------------------------------- |
| `internal/protocol/nlm/xdr/decode.go`         | `internal/protocol/xdr`       | import for shared utilities   | ✓ WIRED    | Import found, DecodeOpaque/DecodeString called                            |
| `internal/protocol/nfs/xdr/decode.go`         | `internal/protocol/xdr`       | import for shared utilities   | ✓ WIRED    | Import found, delegates to shared package                                 |
| `internal/protocol/nlm/handlers/handler.go`   | `pkg/metadata`                | MetadataService reference     | ✓ WIRED    | metadataService field, type *metadata.MetadataService                     |
| `pkg/adapter/nfs/nfs_adapter.go`              | `internal/protocol/nlm/handlers` | nlm.NewHandler initialization| ✓ WIRED   | nlmHandler field, initialized in SetRuntime() line 331                    |
| `internal/protocol/nlm/handlers/lock.go`      | `internal/protocol/nlm/blocking` | queue waiter on conflict   | ✓ WIRED    | h.blockingQueue.Enqueue() at line 180                                     |
| `pkg/adapter/nfs/nfs_adapter.go`              | `internal/protocol/nlm/callback` | process waiters async      | ✓ WIRED    | SetNLMUnlockCallback() at line 334, processNLMWaiters() at line 355      |
| `internal/protocol/nlm/handlers/*`            | `internal/protocol/nlm/metrics`  | record metrics             | ✗ NOT_WIRED| Metrics never passed to handlers, no metrics calls in handlers            |

### Requirements Coverage

| Requirement | Status       | Blocking Issue                                                    |
| ----------- | ------------ | ----------------------------------------------------------------- |
| NLM-01      | ✓ SATISFIED  | NLM protocol implemented (RPC 100021)                             |
| NLM-02      | ✓ SATISFIED  | NLM_TEST operation implemented                                    |
| NLM-03      | ✓ SATISFIED  | NLM_LOCK operation implemented                                    |
| NLM-04      | ✓ SATISFIED  | NLM_UNLOCK operation implemented                                  |
| NLM-05      | ✓ SATISFIED  | NLM_CANCEL operation implemented                                  |
| NLM-06      | ✓ SATISFIED  | Byte-range locking support (64-bit offsets/lengths)               |
| NLM-07      | ✓ SATISFIED  | Blocking lock support with callbacks                              |
| NLM-08      | ✓ SATISFIED  | Non-blocking lock support                                         |
| NLM-09      | ✓ SATISFIED  | NLM handlers in internal/protocol/nlm/                            |

### Anti-Patterns Found

| File                                      | Line | Pattern                   | Severity   | Impact                                                        |
| ----------------------------------------- | ---- | ------------------------- | ---------- | ------------------------------------------------------------- |
| `internal/protocol/nlm/metrics.go`        | N/A  | Defined but unused        | ⚠️ Warning | Observability gap - metrics exist but never collected         |

### Human Verification Required

#### 1. Real NFSv3 Client Lock Acquisition

**Test:** Mount DittoFS via NFS and use fcntl() to acquire a lock from a C program or Python script
**Expected:** Lock acquired successfully, fcntl(F_SETLK) returns 0
**Why human:** Requires real NFS client with kernel NFS implementation

#### 2. Blocking Lock Callback

**Test:** Acquire exclusive lock from client A, attempt blocking lock from client B, release lock from client A
**Expected:** Client B receives NLM_GRANTED callback and acquires lock
**Why human:** Requires network callback testing with real NFS clients

#### 3. Lock Conflict Detection

**Test:** Acquire exclusive lock, attempt TEST (F_GETLK) from another owner
**Expected:** F_GETLK returns conflicting lock info with owner details
**Why human:** Requires real NFS client to verify fcntl() returns correct conflict info

#### 4. Lock Cancellation

**Test:** Queue blocking lock, send NLM_CANCEL before lock becomes available
**Expected:** Blocking request removed from queue, no callback sent
**Why human:** Requires precise timing and network protocol inspection

### Gaps Summary

**Metrics Infrastructure Not Wired**

The NLM metrics infrastructure (`internal/protocol/nlm/metrics.go`) is complete and well-designed but never instantiated or connected to the handlers. The code creates Prometheus metrics for:
- Request counts by procedure and status
- Request duration histograms
- Blocking queue size gauge
- Callback counts and duration

However:
1. Metrics are never created in the NFS adapter (no `nlm.NewMetrics()` call)
2. Handler doesn't accept metrics parameter in `NewHandler()`
3. No metrics recording in dispatch.go or individual handlers
4. processNLMWaiters() doesn't track callback metrics

This is a **minor observability gap** that doesn't affect functionality but prevents monitoring NLM operations in production. The metrics struct is well-designed and ready to use - it just needs wiring.

**Impact:** Low - core functionality works, but operators can't monitor NLM lock operations via Prometheus.

---

_Verified: 2026-02-05T11:30:00Z_
_Verifier: Claude (gsd-verifier)_
