---
phase: 24-directory-delegations
verified: 2026-02-22T22:45:00Z
status: passed
score: 28/28 must-haves verified
re_verification: false
---

# Phase 24: Directory Delegations Verification Report

**Phase Goal:** Server can grant directory delegations and notify clients of directory changes via backchannel
**Verified:** 2026-02-22T22:45:00Z
**Status:** passed
**Re-verification:** No — initial verification

## Goal Achievement

### Observable Truths

**Plan 01 (State Model):**

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 1 | DelegationState can represent both file and directory delegations with IsDirectory flag | ✓ VERIFIED | `IsDirectory bool` field exists at delegation.go:63, used in ReturnDelegation and metrics |
| 2 | Directory delegations track a notification bitmask and pending notifications | ✓ VERIFIED | `NotificationMask uint32`, `PendingNotifs []DirNotification` at delegation.go:65-74 |
| 3 | NotifyDirChange appends to pending notifications and manages batch timers | ✓ VERIFIED | dir_delegation.go:128-161 implements batching with NotifMu lock, resetBatchTimer call |
| 4 | Batch timer flush sends accumulated notifications to BackchannelSender | ✓ VERIFIED | flushDirNotifications at dir_delegation.go:171-208 drains PendingNotifs, calls Enqueue on BackchannelSender |
| 5 | Lock ordering (sm.mu before NotifMu) prevents deadlocks | ✓ VERIFIED | Comments at delegation.go:76-77, NotifyDirChange acquires sm.mu RLock first (dir_delegation.go:129), then NotifMu (dir_delegation.go:145) |
| 6 | DESTROY_CLIENTID auto-revokes all directory delegations for that client | ✓ VERIFIED | v41_client.go purgeV41Client iterates delegByOther, calls cleanupDirDelegation for directory delegations |

**Plan 02 (Handler and Config):**

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 7 | GET_DIR_DELEGATION grants a directory delegation with notification bitmask to clients with valid lease | ✓ VERIFIED | get_dir_delegation_handler.go:86 calls GrantDirDelegation with notifMask; lease check at dir_delegation.go:47-55 |
| 8 | GET_DIR_DELEGATION returns GDD4_UNAVAIL when delegation limit reached or delegations disabled | ✓ VERIFIED | Handler at get_dir_delegation_handler.go:95-107 returns GDD4_UNAVAIL for limit/disabled errors |
| 9 | GET_DIR_DELEGATION returns GDD4_OK with stateid, cookie verifier, and notification types on success | ✓ VERIFIED | get_dir_delegation_handler.go:109-138 encodes GDD4_OK with deleg.Stateid, deleg.CookieVerf, notifMask |
| 10 | DELEGRETURN for directory delegations flushes pending notifications before acknowledging | ✓ VERIFIED | delegation.go:242-256 checks IsDirectory, stops timer, flushes notifications before removal |
| 11 | MaxDelegations and DirDelegBatchWindowMs are configurable via adapter settings API and CLI | ✓ VERIFIED | Fields in models/adapter_settings.go:65-66, API handlers at api/handlers/adapter_settings.go:47-48,73-74,127-128, CLI flags at cmd/dfsctl/commands/adapter/settings.go |
| 12 | Settings watcher propagates MaxDelegations and batch window to StateManager at runtime | ✓ VERIFIED | nfs_adapter_settings.go:42-47 calls SetMaxDelegations and SetDirDelegBatchWindow |

**Plan 03 (Mutation Hooks):**

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 13 | CREATE handler triggers NOTIFY4_ADD_ENTRY notification for parent directory | ✓ VERIFIED | create.go:385 calls NotifyDirChange with NOTIFY4_ADD_ENTRY |
| 14 | REMOVE handler triggers NOTIFY4_REMOVE_ENTRY notification for parent directory | ✓ VERIFIED | remove.go:167 calls NotifyDirChange with NOTIFY4_REMOVE_ENTRY |
| 15 | RENAME handler triggers NOTIFY4_RENAME_ENTRY for both source and destination directories | ✓ VERIFIED | rename.go has dual NotifyDirChange calls for source (RENAME_ENTRY) and destination (ADD_ENTRY for cross-dir renames) |
| 16 | LINK handler triggers NOTIFY4_ADD_ENTRY notification for target directory | ✓ VERIFIED | link.go has NotifyDirChange with NOTIFY4_ADD_ENTRY |
| 17 | OPEN with CREATE triggers NOTIFY4_ADD_ENTRY notification when new file is created | ✓ VERIFIED | open.go has NotifyDirChange when OPEN4_CREATE used and file created |
| 18 | SETATTR on directory triggers NOTIFY4_CHANGE_DIR_ATTRS notification | ✓ VERIFIED | setattr.go has NotifyDirChange with isSignificantAttrChange filtering |
| 19 | Notifications only sent for subscribed types (filtered by notification mask) | ✓ VERIFIED | dir_delegation.go:138-141 checks `deleg.NotificationMask & (1 << notif.Type) != 0` |
| 20 | Directory delegation recall triggered on conflicting modification from different client | ✓ VERIFIED | dir_delegation.go:149-159 checks OriginClientID != deleg.ClientID and calls RecallDirDelegation |
| 21 | Prometheus metrics shared with file delegations using type label | ✓ VERIFIED | delegation_metrics.go uses type label "file"/"directory" in grant/recall/return counters |

**Score:** 21/21 truths verified

### Required Artifacts

**Plan 01:**

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `internal/protocol/nfs/v4/state/delegation.go` | Extended DelegationState with directory fields | ✓ VERIFIED | IsDirectory, NotificationMask, CookieVerf, PendingNotifs, NotifMu, BatchTimer, RecallReason fields exist (lines 62-86) |
| `internal/protocol/nfs/v4/state/dir_delegation.go` | GrantDirDelegation, NotifyDirChange, flushDirNotifications, resetBatchTimer, DirNotification type | ✓ VERIFIED | 339 lines (min 150), all methods present |
| `internal/protocol/nfs/v4/state/dir_delegation_test.go` | Tests for directory delegation grant, notification batching, flush, recall, revocation | ✓ VERIFIED | 571 lines (min 200), 14 tests cover all scenarios |
| `internal/protocol/nfs/v4/types/cb_notify.go` | NotifyAdd4, NotifyRemove4, NotifyRename4, NotifyAttrChange4 sub-type encoders | ✓ VERIFIED | All 4 types with Encode methods exist (lines 194-340) |
| `internal/protocol/nfs/v4/state/callback_common.go` | EncodeCBNotifyOp helper for building CB_NOTIFY wire format | ✓ VERIFIED | EncodeCBNotifyOp defined at line 145 |
| `pkg/controlplane/models/adapter_settings.go` | MaxDelegations and DirDelegBatchWindowMs config fields | ✓ VERIFIED | Both fields at lines 65-66 with defaults 10000 and 50ms |

**Plan 02:**

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `internal/protocol/nfs/v4/handlers/get_dir_delegation_handler.go` | handleGetDirDelegation V41OpHandler implementation | ✓ VERIFIED | 178 lines (min 60), complete handler with GDD4_OK/GDD4_UNAVAIL responses |
| `internal/protocol/nfs/v4/handlers/get_dir_delegation_handler_test.go` | Tests for GET_DIR_DELEGATION handler (grant success, unavail, no FH, bad session) | ✓ VERIFIED | 404 lines (min 100), 7 tests cover all cases including delegreturn flush |
| `internal/protocol/nfs/v4/handlers/handler.go` | OP_GET_DIR_DELEGATION registered with real handler in v41DispatchTable | ✓ VERIFIED | Line 213: `h.v41DispatchTable[types.OP_GET_DIR_DELEGATION] = h.handleGetDirDelegation` |

**Plan 03:**

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `internal/protocol/nfs/v4/handlers/create.go` | NotifyDirChange hook after successful creation | ✓ VERIFIED | Line 385 has NotifyDirChange with NOTIFY4_ADD_ENTRY |
| `internal/protocol/nfs/v4/handlers/remove.go` | NotifyDirChange hook after successful removal + recall for deleted directories | ✓ VERIFIED | Lines 167-172 have NotifyDirChange and RecallDirDelegation for deleted dirs |
| `internal/protocol/nfs/v4/handlers/rename.go` | NotifyDirChange hooks for both source and destination directories | ✓ VERIFIED | Dual NotifyDirChange calls for source and dest (RENAME_ENTRY + ADD_ENTRY) |
| `internal/protocol/nfs/v4/handlers/link.go` | NotifyDirChange hook after successful link | ✓ VERIFIED | NotifyDirChange with NOTIFY4_ADD_ENTRY present |
| `internal/protocol/nfs/v4/state/dir_delegation_hooks_test.go` | Integration tests verifying mutation hooks trigger correct notification types | ✓ VERIFIED | 529 lines (min 150), 11 integration tests for all notification hooks |
| `docs/NFS.md` | Directory delegation documentation section | ✓ VERIFIED | "Directory Delegations" section at line 260 |

### Key Link Verification

**Plan 01:**

| From | To | Via | Status | Details |
|------|----|----|--------|---------|
| dir_delegation.go | callback_common.go | EncodeCBNotifyOp call in flushDirNotifications | ✓ WIRED | Line 193: `EncodeCBNotifyOp(&deleg.Stateid, deleg.FileHandle, pending, deleg.NotificationMask)` |
| dir_delegation.go | backchannel.go | BackchannelSender.Enqueue in flushDirNotifications | ✓ WIRED | Line 202: `sender.Enqueue(req)` |
| dir_delegation.go | delegation.go | DelegationState.IsDirectory and PendingNotifs fields | ✓ WIRED | Fields defined and used in ReturnDelegation (delegation.go:242) |

**Plan 02:**

| From | To | Via | Status | Details |
|------|----|----|--------|---------|
| get_dir_delegation_handler.go | dir_delegation.go | StateManager.GrantDirDelegation call | ✓ WIRED | Line 86: `h.StateManager.GrantDirDelegation(session.ClientID, ctx.CurrentFH, notifMask)` |
| handler.go | get_dir_delegation_handler.go | v41DispatchTable registration replacing stub | ✓ WIRED | Line 213 registers handleGetDirDelegation |
| delegreturn.go | dir_delegation.go | flushDirNotifications before ReturnDelegation for directory delegations | ✓ WIRED | delegation.go:242-256 checks IsDirectory and calls flushDirNotifications |
| nfs_adapter_settings.go | dir_delegation.go | SetMaxDelegations and SetDirDelegBatchWindow calls | ✓ WIRED | Lines 42-47 call both setters |

**Plan 03:**

| From | To | Via | Status | Details |
|------|----|----|--------|---------|
| create.go | dir_delegation.go | h.StateManager.NotifyDirChange call | ✓ WIRED | Line 385: `h.StateManager.NotifyDirChange(...)` |
| remove.go | dir_delegation.go | h.StateManager.NotifyDirChange + RecallDirDelegation for deleted dirs | ✓ WIRED | Lines 167-172 have both calls |
| rename.go | dir_delegation.go | Two NotifyDirChange calls (source + dest dir) | ✓ WIRED | Dual NotifyDirChange calls present |

### Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|------------|-------------|--------|----------|
| DDELEG-01 | 24-02 | Server handles GET_DIR_DELEGATION to grant directory delegations with notification bitmask | ✓ SATISFIED | GET_DIR_DELEGATION handler at get_dir_delegation_handler.go grants delegations with notification mask from args.NotificationTypes bitmap |
| DDELEG-02 | 24-03 | Server sends CB_NOTIFY when directory entries change (add/remove/rename/attr change) | ✓ SATISFIED | All 6 mutation handlers (CREATE, REMOVE, RENAME, LINK, OPEN, SETATTR) call NotifyDirChange; flushDirNotifications sends CB_NOTIFY via BackchannelSender |
| DDELEG-03 | 24-01 | Directory delegation state tracked in StateManager with recall and revocation support | ✓ SATISFIED | DelegationState extended with IsDirectory, NotificationMask, PendingNotifs; RecallDirDelegation and RevokeDelegation support directory delegations; purgeV41Client cleans up directory delegations |

**Orphaned requirements:** None (all 3 requirements from ROADMAP.md are claimed by plans)

### Anti-Patterns Found

None. No TODO, FIXME, XXX, HACK, or PLACEHOLDER comments in key files. No stub implementations. All handlers substantively implemented.

### Human Verification Required

#### 1. Client Notification Receipt

**Test:** Mount NFSv4.1 export with directory delegation support, request directory delegation via GET_DIR_DELEGATION, then modify directory (add/remove/rename file), observe CB_NOTIFY arrival on client.

**Expected:** Client receives CB_NOTIFY with correct notification type and entry name within the batch window (default 50ms or earlier if 100 notifications reached).

**Why human:** Requires NFSv4.1 client that supports GET_DIR_DELEGATION and CB_NOTIFY (Linux 5.10+ or higher may support this; behavior varies by kernel version). Cannot be verified programmatically without real NFS client.

#### 2. Conflict Recall Behavior

**Test:** Client A holds directory delegation, client B modifies same directory from different mount.

**Expected:** Client A receives CB_RECALL for the directory delegation, returns it, then client B's operation completes.

**Why human:** Requires multi-client scenario with real NFS mounts and observable callback timing. Cannot verify programmatically in unit tests.

#### 3. Settings Configuration Persistence

**Test:** Use `dfsctl adapter settings patch --max-delegations 5000 --dir-deleg-batch-window-ms 100`, restart server, verify settings retained.

**Expected:** Settings persist across restarts and are visible in `dfsctl adapter settings show`.

**Why human:** Requires full server lifecycle test with database persistence. Integration tests verify API layer but not persistence across restarts.

### Overall Status Summary

Phase 24 successfully implements directory delegations with all must-haves verified:

**State Model (Plan 01):**
- DelegationState extended with 7 directory-specific fields
- Notification batching with time (50ms default) and count (100 max) flush triggers
- Lock ordering (sm.mu before NotifMu) prevents deadlocks
- CB_NOTIFY sub-type encoders for 4 notification types
- MaxDelegations and DirDelegBatchWindowMs config with sensible defaults

**Handler and Config (Plan 02):**
- GET_DIR_DELEGATION handler grants delegations with GDD4_OK/GDD4_UNAVAIL responses
- DELEGRETURN flushes pending notifications before acknowledging
- Config full stack: model → store → API → apiclient → CLI → settings watcher
- All tests pass with race detection

**Mutation Hooks (Plan 03):**
- 6 mutation handlers wire NotifyDirChange correctly
- Conflict-based recall via OriginClientID field
- Directory deletion triggers immediate revocation
- Prometheus metrics with type label (file/directory)
- docs/NFS.md has comprehensive directory delegation documentation

All automated verification passed. Phase goal achieved. Ready for human testing with NFSv4.1 clients.

---

_Verified: 2026-02-22T22:45:00Z_
_Verifier: Claude (gsd-verifier)_
