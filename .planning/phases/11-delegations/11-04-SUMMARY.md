---
phase: 11-delegations
plan: 04
status: complete
started: 2026-02-14
completed: 2026-02-14
---

# Plan 11-04: Recall Timeout, Revocation, and Anti-Storm Protection

## What Was Built

Completed the delegation lifecycle with failure handling: recall timer fires RevokeDelegation after lease period if client doesn't respond to CB_RECALL, revoked delegations return NFS4ERR_BAD_STATEID, CB_NULL verifies callback path on SETCLIENTID_CONFIRM, CBPathUp tracking controls delegation grants, recently-recalled cache prevents grant-recall-grant storms.

## Key Artifacts

| File | What It Provides |
|------|-----------------|
| `internal/protocol/nfs/v4/state/delegation.go` | StartRecallTimer, StopRecallTimer, addRecentlyRecalled, isRecentlyRecalled, RecentlyRecalledTTL (30s) |
| `internal/protocol/nfs/v4/state/manager.go` | RevokeDelegation, recentlyRecalled map, CB_NULL async on ConfirmClientID, Shutdown stops recall timers |
| `internal/protocol/nfs/v4/state/client.go` | CBPathUp field for callback path health tracking |
| `internal/protocol/nfs/v4/state/delegation_test.go` | 16 new tests (recall timer, revocation, callback path, recently-recalled, shutdown) |

## Key Decisions

1. **Recall timer = lease duration**: Per RFC 7530 Section 10.4.6, server MUST NOT revoke before lease period expires since recall attempt
2. **Short timer on CB_RECALL failure**: 5s instead of full lease when callback path is known-down
3. **CBPathUp replaces simple callback-addr check**: Verified via CB_NULL, more reliable than just checking address is non-empty
4. **Recently-recalled TTL = 30s**: ~1/3 of default lease duration, prevents rapid grant-recall storms
5. **Revoked delegations kept in delegByOther**: Enables NFS4ERR_BAD_STATEID on stale stateid validation
6. **DELEGRETURN of revoked delegation returns NFS4_OK**: Graceful cleanup, client may not know delegation was revoked

## Commits

- `ad771fe` feat(11-04): add recall timer, revocation, callback path tracking, and recently-recalled cache
- `22aa6b8` test(11-04): add recall timer, revocation, callback path, and recently-recalled tests

## Test Results

All tests pass with `-race`:
- 16 new tests for recall timer, revocation, callback path, recently-recalled cache, shutdown
- Full v4 suite green: state, handlers, attrs, types, pseudofs
