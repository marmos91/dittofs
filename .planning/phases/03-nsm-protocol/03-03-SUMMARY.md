# Phase 3 Plan 3: NSM Crash Recovery Summary

## Metadata

| Field | Value |
|-------|-------|
| Phase | 03-nsm-protocol |
| Plan | 03 |
| Subsystem | crash-recovery |
| Tags | nsm, callback, notifier, crash-detection, lock-cleanup |
| Duration | 6 min |
| Completed | 2026-02-05 |

## One-liner

SM_NOTIFY callback client with 5s timeout, parallel Notifier, FREE_ALL handler, NSM metrics, and adapter startup integration.

## What Was Built

### SM_NOTIFY Callback Client (Task 1)
- `internal/protocol/nsm/callback/client.go`: TCP callback client with 5s total timeout
- `internal/protocol/nsm/callback/notify.go`: High-level SendNotify function
- Fresh TCP connection per callback (no connection caching per CONTEXT.md)
- XDR encoding for NSM Status structure (mon_name, state, priv)
- RPC record marking for proper wire format

### Notifier for Parallel SM_NOTIFY (Task 2)
- `internal/protocol/nsm/notifier.go`: Parallel notification orchestration
- NotifyAllClients sends SM_NOTIFY to all registered clients using goroutines
- Failed notifications trigger OnClientCrash callback for lock cleanup
- LoadRegistrationsFromStore restores registrations on startup
- DetectCrash handles external crash detection

### NSM Prometheus Metrics (Task 4 - included with Task 2)
- `internal/protocol/nsm/metrics.go`: Full metrics suite
- nsm_requests_total, nsm_request_duration_seconds
- nsm_clients_registered gauge
- nsm_notifications_total by result (started, success, failed)
- nsm_crashes_detected_total, nsm_crash_cleanups_total
- nsm_locks_cleaned_on_crash_total
- Nil receiver pattern for zero overhead when disabled

### NLM FREE_ALL Handler (Task 3)
- `internal/protocol/nlm/handlers/free_all.go`: Bulk lock release handler
- Added Data field to NLMHandlerContext for raw request access
- Decode nlm_notify request (name, state fields)
- Added FREE_ALL to NLM dispatch table (procedure 23)
- Best effort cleanup with waiter processing

### NFS Adapter Integration (Task 5)
- Added nsmNotifier, nsmMetrics, nsmClientStore fields
- handleClientCrash callback releases locks across all shares
- performNSMStartup loads registrations, increments state, sends notifications
- SM_NOTIFY sent in background goroutine (non-blocking)

## Key Files

### Created
| File | Purpose |
|------|---------|
| internal/protocol/nsm/callback/client.go | TCP callback client with 5s timeout |
| internal/protocol/nsm/callback/notify.go | SendNotify high-level function |
| internal/protocol/nsm/notifier.go | Parallel notification orchestration |
| internal/protocol/nsm/metrics.go | NSM Prometheus metrics |
| internal/protocol/nlm/handlers/free_all.go | FREE_ALL handler (procedure 23) |

### Modified
| File | Changes |
|------|---------|
| internal/protocol/nlm/handlers/context.go | Added Data []byte field |
| internal/protocol/nlm/dispatch.go | Added FREE_ALL to dispatch table |
| pkg/adapter/nfs/nfs_adapter.go | NSM notifier integration, startup logic |

## Commits

| Hash | Description |
|------|-------------|
| 91e3212 | feat(03-03): add SM_NOTIFY callback client with 5s timeout |
| c272bae | feat(03-03): add NSM Notifier for parallel SM_NOTIFY on restart |
| 2445417 | feat(03-03): implement NLM FREE_ALL handler for bulk lock release |
| 90fce42 | feat(03-03): integrate NSM Notifier with NFS adapter startup |

## Decisions Made

| Decision | Rationale |
|----------|-----------|
| Fresh TCP connection per callback | Per CONTEXT.md - no connection caching for callbacks |
| Parallel SM_NOTIFY using goroutines | Fastest recovery - notify all clients simultaneously |
| FREE_ALL returns void | Per NLM spec - no response body |
| Background notification goroutine | Don't block accept loop during startup |
| Best effort lock cleanup | Log errors but continue, don't fail crash handling |

## Deviations from Plan

None - plan executed exactly as written.

## Technical Details

### SM_NOTIFY Callback Flow
```
Server Restart
    |
    v
performNSMStartup()
    |
    +-> LoadRegistrationsFromStore()
    +-> IncrementServerState()
    +-> NotifyAllClients() [background goroutine]
            |
            +-> For each client (parallel):
                    |
                    +-> SendNotify(ctx, client, ...)
                    |       |
                    |       v
                    |   Client.Send(addr, status, proc, prog, vers)
                    |       |
                    |       +-> TCP dial (with 5s timeout)
                    |       +-> Build RPC CALL message
                    |       +-> Send with record marking
                    |       +-> Read response (discard)
                    |
                    +-> If error: OnClientCrash(clientID)
                            |
                            v
                    handleClientCrash() -> release locks
```

### NLMHandlerContext.Data Field
The Data field was added to pass raw request bytes to handlers that need direct XDR decoding (like FREE_ALL). This avoids pre-decoding structures that aren't needed by all handlers.

## Verification

```bash
# Build verification
go build ./internal/protocol/nsm/...   # OK
go build ./internal/protocol/nlm/...   # OK
go build ./pkg/adapter/nfs/...         # OK

# Test verification
go test ./internal/protocol/nsm/...    # [no test files]
go test ./internal/protocol/nlm/...    # [no test files]
```

## Next Phase Readiness

Phase 3 (NSM Protocol) is now COMPLETE. All three plans executed:
- 03-01: NSM types and foundation
- 03-02: NSM handlers and dispatch
- 03-03: NSM crash recovery (this plan)

Ready for Phase 4 (NFSv4 Foundation) or other phases per roadmap.
