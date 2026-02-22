# Phase 24: Directory Delegations - Research

**Researched:** 2026-02-22
**Domain:** NFSv4.1 directory delegations (GET_DIR_DELEGATION, CB_NOTIFY) per RFC 8881 Sections 10.9, 18.39, 20.4
**Confidence:** HIGH

## Summary

Phase 24 implements NFSv4.1 directory delegations, allowing the server to grant directory change tracking to clients via GET_DIR_DELEGATION (operation 46) and notify them of directory mutations via CB_NOTIFY (callback operation 6) over the backchannel. This extends the existing file delegation pattern (StateManager, DelegationState, CB_RECALL, BackchannelSender) to directories with subscription-based notification support.

The existing codebase provides excellent foundations: (1) DelegationState struct with stateid, file handle tracking, recall timers, and revocation; (2) BackchannelSender goroutine with queue, retry logic, and exponential backoff; (3) XDR types already defined for both CbNotifyArgs/Res and GetDirDelegationArgs/Res (created in Phase 16 as stubs); (4) NOTIFY4_* constants already defined (ADD_ENTRY=3, REMOVE_ENTRY=2, RENAME_ENTRY=4, CHANGE_CHILD_ATTRS=0, CHANGE_DIR_ATTRS=1, CHANGE_COOKIE_VERIFIER=5); (5) v41DispatchTable with a stub for OP_GET_DIR_DELEGATION already consuming XDR args. The primary work is: extending DelegationState with directory-specific fields (notification bitmask, IsDirectory flag, pending notifications), implementing notification batching in StateManager, adding NotifyDirChange() hooks to directory-mutating handlers, implementing the GET_DIR_DELEGATION handler, and encoding CB_NOTIFY operations for BackchannelSender delivery.

**Primary recommendation:** Extend the existing DelegationState struct with directory-specific fields rather than creating a separate DirDelegationState. The delegation lifecycle (grant, recall, revoke, return, free) follows the exact same pattern as file delegations. Directory mutations (CREATE, REMOVE, RENAME, LINK, OPEN with CREATE, SETATTR) call StateManager.NotifyDirChange() which batches notifications and flushes them via BackchannelSender. Build in three plans: (1) state model + notification types + batching, (2) GET_DIR_DELEGATION handler + recall integration, (3) CB_NOTIFY delivery + handler hooks + metrics.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Support full notification set from day one: ADD, REMOVE, RENAME, ATTR_CHANGE
- Honor client's requested notification bitmask -- only send notifications for subscribed types
- Include full entry info in CB_NOTIFY (name, cookie, attributes) so clients can update cache without READDIR
- Batch notifications within a configurable time window (exposed in NFS adapter config section)
- Cross-directory renames: notify both source and destination directory delegation holders
- Include readdir cookie for affected entries in notifications
- All directories eligible for delegation (including export root)
- Configurable server-wide limit on total outstanding directory delegations
- Delegation lives until recalled or returned -- no TTL auto-expiry (matches standard lease model)
- Require valid lease before granting -- refuse if client has expired lease
- DESTROY_CLIENTID must auto-revoke all directory delegations for that client
- Use lease timeout as the grace period after sending recall before forced revocation
- Flush any batched notifications before acknowledging DELEGRETURN -- client gets complete picture
- Track recall reason (conflict, resource pressure, admin action) for metrics and logging
- Extend existing DelegationState struct with notification-specific fields (notification bitmask, pending notifications, dir flag)
- Index by file handle -- same lookup pattern as file delegations
- Ephemeral state (lost on restart) -- consistent with current file delegation model
- Prometheus metrics: shared with file delegations using a 'type' label (file vs directory) -- no separate metric names
- TEST_STATEID and FREE_STATEID: ensure directory delegation stateids are handled properly
- Notification batching logic lives in StateManager
- Directory mutation handlers (CREATE, REMOVE, RENAME, etc.) call StateManager.NotifyDirChange() directly after successful mutation
- GET_DIR_DELEGATION implemented as standalone handler registered in dispatch table
- CB_NOTIFY reuses existing BackchannelSender goroutine from phase 22
- XDR types for CB_NOTIFY and GET_DIR_DELEGATION added to existing xdr package
- Update docs/NFS.md with directory delegation section
- Extra resilience: defensive handling for delegation on deleted dir, double-grant, stale handle edge cases

### Claude's Discretion
- Rename notification encoding: single event vs decomposed remove+add (RFC 8881 notify4 semantics)
- CB_NOTIFY delivery failure handling: retry policy before revocation (match existing CB_RECALL failure handling)
- Notification batcher architecture: per-directory vs global (fit existing goroutine model)
- Batch count limit vs time-only flushing (consider CB_NOTIFY message size)
- Attr change notification scope: which attribute changes are "significant" enough to trigger notify
- Delegation granting heuristics (always grant vs heuristic-based, per RFC guidance)
- Multiple clients holding directory delegations simultaneously (RFC semantics)
- Behavior at delegation limit: refuse vs recall LRU
- Proactive delegation offering on READDIR vs explicit GET_DIR_DELEGATION only
- Conflicting operation blocking: block until delegation returned vs proceed immediately after recall
- CB_RECALL_ANY for directory delegations under memory pressure
- Recall on directory deletion: recall first vs revoke immediately
- Cascade recall: whether recalling dir delegation also recalls file delegations within
- FREE_STATEID scope for directory delegation stateids
- Per-client shared vs separate limits for file and directory delegations
- Test depth: appropriate testing patterns based on previous phases

### Deferred Ideas (OUT OF SCOPE)
None -- discussion stayed within phase scope
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| DDELEG-01 | Server handles GET_DIR_DELEGATION to grant directory delegations with notification bitmask | XDR types already exist (GetDirDelegationArgs/Res in types package); stub in v41DispatchTable at OP_GET_DIR_DELEGATION=46; DelegationState needs IsDirectory flag + NotificationMask field; uses existing GrantDelegation pattern with directory-specific extensions |
| DDELEG-02 | Server sends CB_NOTIFY when directory entries change (add/remove/rename/attr change) | CbNotifyArgs/Res XDR types already exist; NOTIFY4_* constants defined; BackchannelSender infrastructure ready; need notify sub-type encoders (notify_add4, notify_remove4, notify_rename4), notification batching timer, and hooks in 6+ mutation handlers |
| DDELEG-03 | Directory delegation state tracked in StateManager with recall and revocation support | Existing DelegationState + delegByOther/delegByFile maps handle lifecycle; need extension for notification fields; ReturnDelegation, RevokeDelegation, FreeStateid already handle deleg type 0x03; purgeV41Client needs directory delegation cleanup |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `internal/protocol/nfs/v4/state` | Phase 9-23 | StateManager, DelegationState, BackchannelSender, recall/revocation | All NFSv4 state lives here; single RWMutex pattern |
| `internal/protocol/nfs/v4/handlers` | Phase 7-23 | V41OpHandler for GET_DIR_DELEGATION, mutation handler hooks | All handlers follow established patterns |
| `internal/protocol/nfs/v4/types` | Phase 6-23 | XDR types (CbNotifyArgs, GetDirDelegationArgs, NOTIFY4_* constants, Bitmap4) | Already defined in Phase 16, need sub-type encoders |
| `internal/protocol/xdr` | existing | XDR encode/decode primitives | Used by all NFS handlers |
| `pkg/controlplane/models` | Phase 14 | NFSAdapterSettings for batch window config | Existing adapter settings pattern |
| Go stdlib `sync`, `time`, `context`, `bytes` | N/A | Mutex, timers for batching, buffer for encoding | Standard patterns |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `internal/protocol/nfs/v4/state/callback_common.go` | Phase 22 | EncodeCBRecallOp pattern for new EncodeCBNotifyOp | Shared wire-format helpers |
| `internal/protocol/nfs/v4/state/backchannel.go` | Phase 22 | BackchannelSender.Enqueue, encodeCBCompoundV41 | CB_NOTIFY delivery via v4.1 backchannel |
| `internal/protocol/nfs/v4/state/callback.go` | Phase 11 | v4.0 dial-out path reference pattern | CB_NOTIFY for v4.0 clients (if supported) |
| `internal/protocol/nfs/v4/attrs` | Phase 13 | Attribute encoding for CB_NOTIFY entry info | Include child/dir attrs in notifications |

**No new external dependencies required.** All packages already exist in the codebase.

## Architecture Patterns

### Recommended Project Structure
```
internal/protocol/nfs/v4/
├── state/
│   ├── delegation.go          # EXTEND: DelegationState with dir fields, GrantDirDelegation, NotifyDirChange
│   ├── dir_delegation.go      # NEW: notification batching, flush logic, CB_NOTIFY encoding
│   ├── dir_delegation_test.go # NEW: tests for directory delegation + notification
│   ├── callback_common.go     # EXTEND: add EncodeCBNotifyOp helper
│   ├── manager.go             # EXTEND: new maps/fields for dir deleg tracking
│   └── stateid.go             # VERIFY: FreeStateid handles dir deleg stateids correctly
├── handlers/
│   ├── get_dir_delegation_handler.go      # NEW: GET_DIR_DELEGATION handler
│   ├── get_dir_delegation_handler_test.go # NEW: handler tests
│   ├── handler.go             # MODIFY: replace stub with real handler in v41DispatchTable
│   ├── create.go              # MODIFY: add NotifyDirChange hook after successful creation
│   ├── remove.go              # MODIFY: add NotifyDirChange hook after successful removal
│   ├── rename.go              # MODIFY: add NotifyDirChange hook for both src and dst dirs
│   ├── link.go                # MODIFY: add NotifyDirChange hook after successful link
│   ├── open.go                # MODIFY: add NotifyDirChange hook when OPEN4_CREATE creates new file
│   └── setattr.go             # MODIFY: add NotifyDirChange hook for attr changes
└── types/
    ├── cb_notify.go           # EXTEND: add notify sub-type structs (NotifyAdd4, NotifyRemove4, etc.)
    └── get_dir_delegation.go  # EXISTING: already complete from Phase 16
```

### Pattern 1: Extended DelegationState for Directory Delegations
**What:** Add directory-specific fields to the existing DelegationState struct rather than creating a new type.
**When to use:** Always -- directory delegations share the same lifecycle (grant, recall, return, revoke, free) as file delegations.
**Example:**
```go
// Source: existing DelegationState in delegation.go, extended
type DelegationState struct {
    // ... existing fields (Stateid, ClientID, FileHandle, DelegType, RecallSent, etc.)

    // Directory delegation fields (zero values for file delegations)
    IsDirectory      bool           // true for directory delegations
    NotificationMask uint32         // NOTIFY4_* bitmask from GET_DIR_DELEGATION
    CookieVerf       [8]byte        // cookie verifier for directory delegation
    PendingNotifs    []DirNotification // batched notifications awaiting flush
    NotifMu          sync.Mutex     // protects PendingNotifs (separate from sm.mu)
    BatchTimer       *time.Timer    // notification batch flush timer
    RecallReason     string         // "conflict", "resource_pressure", "admin" for metrics
}

type DirNotification struct {
    Type       uint32  // NOTIFY4_ADD_ENTRY, NOTIFY4_REMOVE_ENTRY, etc.
    EntryName  string  // name of affected entry
    Cookie     uint64  // readdir cookie for the entry
    Attrs      []byte  // pre-encoded fattr4 (optional, for attr change notifications)
    NewName    string  // for RENAME: new name (EntryName is old name)
    NewDirFH   []byte  // for cross-dir RENAME: destination dir handle
}
```

### Pattern 2: NotifyDirChange Hook in Mutation Handlers
**What:** After a successful directory mutation, call `StateManager.NotifyDirChange()` with the parent directory handle and notification details.
**When to use:** In every handler that mutates directory contents (CREATE, REMOVE, RENAME, LINK, OPEN with CREATE).
**Example:**
```go
// In create.go, after successful creation:
if createErr == nil {
    h.StateManager.NotifyDirChange(ctx.CurrentFH, state.DirNotification{
        Type:      types.NOTIFY4_ADD_ENTRY,
        EntryName: objName,
        // Cookie and Attrs populated by StateManager from metadata
    })
}
```

### Pattern 3: Notification Batching in StateManager
**What:** NotifyDirChange appends to DelegationState.PendingNotifs and resets/starts a batch timer. On timer expiry (or when flush is forced), all pending notifications are encoded as CB_NOTIFY and enqueued to BackchannelSender.
**When to use:** All directory notifications go through batching. Timer-based flush with configurable window.
**Example:**
```go
func (sm *StateManager) NotifyDirChange(dirFH []byte, notif DirNotification) {
    fhKey := string(dirFH)

    sm.mu.RLock()
    delegs := sm.delegByFile[fhKey]
    sm.mu.RUnlock()

    for _, deleg := range delegs {
        if !deleg.IsDirectory || deleg.Revoked || deleg.RecallSent {
            continue
        }
        // Check if client subscribed to this notification type
        if deleg.NotificationMask & (1 << notif.Type) == 0 {
            continue
        }
        deleg.NotifMu.Lock()
        deleg.PendingNotifs = append(deleg.PendingNotifs, notif)
        sm.resetBatchTimer(deleg) // reset or start batch timer
        deleg.NotifMu.Unlock()
    }
}
```

### Pattern 4: CB_NOTIFY via BackchannelSender
**What:** Encode batched notifications into a CB_NOTIFY operation payload and enqueue as CallbackRequest to the existing BackchannelSender.
**When to use:** On batch timer expiry or forced flush (before DELEGRETURN acknowledgment).
**Example:**
```go
func (sm *StateManager) flushDirNotifications(deleg *DelegationState) {
    deleg.NotifMu.Lock()
    notifs := deleg.PendingNotifs
    deleg.PendingNotifs = nil
    deleg.NotifMu.Unlock()

    if len(notifs) == 0 {
        return
    }

    payload := EncodeCBNotifyOp(&deleg.Stateid, deleg.FileHandle, notifs, deleg.NotificationMask)

    sender := sm.getBackchannelSender(deleg.ClientID)
    if sender != nil {
        req := CallbackRequest{
            OpCode:  types.OP_CB_NOTIFY,
            Payload: payload,
        }
        sender.Enqueue(req) // fire-and-forget for notifications
    }
}
```

### Anti-Patterns to Avoid
- **Separate DirDelegationState type:** Do NOT create a separate struct; extend DelegationState with directory fields. All lifecycle operations (grant, recall, return, revoke, free) are shared.
- **Holding sm.mu during notification delivery:** NEVER hold the main StateManager lock during backchannel I/O. Use the per-delegation NotifMu for pending notification access, then enqueue asynchronously.
- **Blocking mutation handlers on notification delivery:** NotifyDirChange must be non-blocking. Append to pending list and let batch timer handle delivery.
- **Sending per-operation notifications:** Always batch. Individual CB_NOTIFY per mutation causes excessive backchannel traffic during bulk operations (e.g., `rm -rf`).

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Callback delivery | Custom TCP send logic | BackchannelSender.Enqueue() | Already handles retry, backoff, write serialization, connection selection |
| XDR encoding | Raw byte manipulation | xdr.WriteUint32, xdr.WriteXDROpaque, types.EncodeStateid4 | Established encoding helpers with padding/alignment |
| Stateid generation | Manual byte assembly | sm.generateStateidOther(StateTypeDeleg) | Existing generator handles boot epoch, uniqueness |
| Timer management | Manual goroutine + sleep | time.AfterFunc / time.Timer.Reset | Standard Go patterns, matches existing recall timer |
| Notification encoding | Ad-hoc wire format | CbNotifyArgs.Encode() + Notify4.Encode() | Already defined in Phase 16, just need sub-type encoders |

**Key insight:** The BackchannelSender infrastructure from Phase 22 was explicitly designed to be extensible for CB_NOTIFY. The comment in Phase 22 CONTEXT.md reads: "Build an extensible dispatch table for callback operations -- only CB_RECALL implemented now, but CB_NOTIFY (Phase 24) should be trivial to add."

## Common Pitfalls

### Pitfall 1: Notification Storm During Bulk Operations
**What goes wrong:** `rm -rf` on a large directory triggers thousands of NOTIFY4_REMOVE_ENTRY notifications, overwhelming the backchannel.
**Why it happens:** Each file removal calls NotifyDirChange individually.
**How to avoid:** Batch notifications with a configurable time window (e.g., 50ms default). The batch timer accumulates notifications and sends one CB_NOTIFY per flush with all entries. Also enforce a max batch size (e.g., 100 entries) to bound CB_NOTIFY message size.
**Warning signs:** BackchannelSender queue full, backchannel callback failures spiking.

### Pitfall 2: Deadlock Between NotifMu and sm.mu
**What goes wrong:** Lock ordering violation if notification flush acquires sm.mu while holding NotifMu, and another path acquires NotifMu while holding sm.mu.
**Why it happens:** Multiple lock levels with unclear ordering.
**How to avoid:** Strict lock ordering: sm.mu before NotifMu (same as existing sm.mu before connMu). Never hold NotifMu when calling any StateManager method that acquires sm.mu. In flushDirNotifications: acquire NotifMu to drain pending list, release it, then acquire sm.mu if needed for BackchannelSender lookup.
**Warning signs:** Test deadlocks, goroutine dump showing lock contention.

### Pitfall 3: Race Between DELEGRETURN and Pending Notifications
**What goes wrong:** Client sends DELEGRETURN for a directory delegation while notifications are still pending. If DELEGRETURN completes before flush, client misses notifications.
**Why it happens:** DELEGRETURN is synchronous but notification flush is async.
**How to avoid:** Per the locked decision: "Flush any batched notifications before acknowledging DELEGRETURN." In ReturnDelegation, check if the delegation is a directory delegation with pending notifications, flush them synchronously before removing the delegation state.
**Warning signs:** Client cache becomes stale after returning delegation.

### Pitfall 4: Directory Delegation on Deleted Directory
**What goes wrong:** Client holds a directory delegation, another client deletes the directory. Server still has the delegation state referencing a non-existent directory.
**Why it happens:** Directory deletion doesn't automatically revoke delegations.
**How to avoid:** When a directory is removed (REMOVE/RMDIR handler succeeds on a directory), check if any directory delegations exist for the removed directory's handle and recall them. Treat this as a conflict recall with reason "directory_deleted".
**Warning signs:** Stale file handle errors when trying to send notifications to deleted directory's delegation holders.

### Pitfall 5: FreeStateid/TestStateid for Directory Delegation Stateids
**What goes wrong:** FreeStateid or TestStateid doesn't properly handle directory delegation stateids, causing cleanup failures.
**Why it happens:** Directory delegations use the same StateTypeDeleg (0x03) byte tag as file delegations. The existing freeDelegStateidLocked already handles this correctly since it looks up by Other field in delegByOther map.
**How to avoid:** Verify that the existing FreeStateid and TestStateid code paths work unchanged for directory delegations. No separate type tag is needed since directory delegations are tracked in the same delegByOther map. Add test coverage to confirm.
**Warning signs:** NFS4ERR_BAD_STATEID when freeing directory delegation stateids.

### Pitfall 6: Concurrent Notification Append During Flush
**What goes wrong:** A mutation handler appends to PendingNotifs while flushDirNotifications is draining the slice.
**Why it happens:** PendingNotifs accessed from multiple goroutines without synchronization.
**How to avoid:** Use deleg.NotifMu to protect PendingNotifs. Flush pattern: lock, swap slice with nil, unlock, then encode and send the swapped slice. New notifications that arrive during send go to the fresh nil slice and will be flushed by the next timer.
**Warning signs:** Data races detected by `go test -race`.

## Code Examples

### CB_NOTIFY Operation Encoding
```go
// Source: follows EncodeCBRecallOp pattern from callback_common.go
func EncodeCBNotifyOp(stateid *types.Stateid4, dirFH []byte, notifs []DirNotification, mask uint32) []byte {
    var buf bytes.Buffer

    // argop: OP_CB_NOTIFY = 6
    _ = xdr.WriteUint32(&buf, types.OP_CB_NOTIFY)

    // CB_NOTIFY4args: stateid4
    types.EncodeStateid4(&buf, stateid)

    // CB_NOTIFY4args: nfs_fh4
    _ = xdr.WriteXDROpaque(&buf, dirFH)

    // CB_NOTIFY4args: notify4 changes<>
    // Group notifications by type into notify4 entries
    changes := groupNotifsByType(notifs, mask)
    _ = xdr.WriteUint32(&buf, uint32(len(changes)))
    for _, change := range changes {
        _ = change.Encode(&buf)
    }

    return buf.Bytes()
}
```

### GET_DIR_DELEGATION Handler
```go
// Source: follows handleFreeStateid/handleTestStateid pattern from Phase 23
func (h *Handler) handleGetDirDelegation(
    ctx *types.CompoundContext,
    v41ctx *types.V41RequestContext,
    reader io.Reader,
) *types.CompoundResult {
    // Require current filehandle (the directory)
    if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
        return &types.CompoundResult{Status: status, OpCode: types.OP_GET_DIR_DELEGATION, Data: encodeStatusOnly(status)}
    }

    // Decode args
    var args types.GetDirDelegationArgs
    if err := args.Decode(reader); err != nil {
        return &types.CompoundResult{Status: types.NFS4ERR_BADXDR, OpCode: types.OP_GET_DIR_DELEGATION, Data: encodeStatusOnly(types.NFS4ERR_BADXDR)}
    }

    // Get clientID from session
    session := h.StateManager.GetSession(v41ctx.SessionID)
    if session == nil {
        return &types.CompoundResult{Status: types.NFS4ERR_BADSESSION, OpCode: types.OP_GET_DIR_DELEGATION, Data: encodeStatusOnly(types.NFS4ERR_BADSESSION)}
    }

    // Try to grant
    deleg, err := h.StateManager.GrantDirDelegation(session.ClientID, ctx.CurrentFH, args.NotificationTypes)
    // ... encode response
}
```

### Notify Sub-Type XDR: notify_add4
```go
// RFC 8881 notify_add4 structure
// Per Phase 16 decision: CB_NOTIFY entries stored as raw opaque, sub-type parsing in Phase 24
type NotifyAdd4 struct {
    // notify_entry4 fields
    EntryName string   // component4 (entry name)
    Cookie    uint64   // nfs_cookie4 (readdir cookie)
    Attrs     []byte   // fattr4 (pre-encoded attributes)
    // prev_entry4 (optional previous entry for ordering)
    HasPrev   bool
    PrevName  string   // previous entry name if HasPrev
    PrevCookie uint64  // previous entry cookie if HasPrev
}

func (n *NotifyAdd4) Encode(buf *bytes.Buffer) error {
    // Encode as opaque within NotifyEntry4
    var inner bytes.Buffer
    _ = xdr.WriteXDRString(&inner, n.EntryName)
    _ = xdr.WriteUint64(&inner, n.Cookie)
    _, _ = inner.Write(n.Attrs)  // pre-encoded fattr4
    _ = xdr.WriteBool(&inner, n.HasPrev)
    if n.HasPrev {
        _ = xdr.WriteXDRString(&inner, n.PrevName)
        _ = xdr.WriteUint64(&inner, n.PrevCookie)
    }
    return xdr.WriteXDROpaque(buf, inner.Bytes())
}
```

### Mutation Handler Hook Pattern
```go
// In remove.go, after successful removal (line ~146):
logger.Debug("NFSv4 REMOVE successful", "target", target, "client", ctx.ClientAddr)

// Directory delegation notification (non-blocking)
if h.StateManager != nil {
    h.StateManager.NotifyDirChange(ctx.CurrentFH, state.DirNotification{
        Type:      types.NOTIFY4_REMOVE_ENTRY,
        EntryName: target,
    })
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| No directory delegations (NFSv4.0) | GET_DIR_DELEGATION + CB_NOTIFY (NFSv4.1) | RFC 8881 (Aug 2020) | Eliminates READDIR polling for directory change detection |
| CB_NOTIFY entries as raw opaque | Full sub-type parsing (notify_add4, notify_remove4, notify_rename4) | Phase 24 (this phase) | Previously deferred in Phase 16-04 per decision "CB_NOTIFY entries stored as raw opaque deferring sub-type parsing to Phase 24" |
| File delegations only | Unified delegation (file + directory) with type label | Phase 24 (this phase) | Single DelegationState struct, shared metrics with type label |

**RFC 8881 Guidance on Directory Delegations (Section 10.9):**
- Directory delegations are "OPTIONAL" -- server may always refuse with GDD4_UNAVAIL
- Server MUST send CB_NOTIFY only for notification types the client requested via NotificationTypes bitmap
- Multiple clients can simultaneously hold directory delegations on the same directory (unlike write delegations which are exclusive)
- Directory delegation does not conflict with file delegations within the directory
- Server should recall directory delegation when it cannot maintain notification guarantees
- CB_NOTIFY delivery failure should eventually lead to delegation revocation

## Discretionary Recommendations

Based on codebase patterns and RFC guidance, these are recommendations for areas left to Claude's discretion:

### Rename Notification: Single NOTIFY4_RENAME_ENTRY Event
Encode renames as a single NOTIFY4_RENAME_ENTRY with old name + new name, not decomposed into remove+add. This matches the RFC 8881 notify4 bitmap which has a dedicated NOTIFY4_RENAME_ENTRY=4 constant. For cross-directory renames, send NOTIFY4_RENAME_ENTRY to both source and destination directory delegation holders.

### CB_NOTIFY Failure Handling: Match CB_RECALL Pattern
On CB_NOTIFY delivery failure, follow the same retry policy as CB_RECALL in sendRecallV41: 3 retries with exponential backoff (5s/10s/20s), then mark backchannel fault. Unlike CB_RECALL, CB_NOTIFY failure should NOT trigger immediate revocation -- instead, mark the delegation for recall on the next notification failure and revoke if recall also fails. Notifications are informational; missing one is acceptable.

### Notification Batcher: Per-Delegation Timer
Use per-delegation batch timers rather than a global batcher. Each DelegationState owns its PendingNotifs and BatchTimer. This avoids a central goroutine bottleneck and naturally aligns with the per-delegation notification mask filtering. When a notification arrives, if no timer is running, start one; if already running, do nothing (notifications accumulate until timer fires).

### Batch Limits: Time + Count
Flush notifications on either: (a) batch window timer expiry (configurable, default 50ms), OR (b) count exceeding max batch size (default 100 entries). This prevents unbounded CB_NOTIFY messages from large bulk operations while keeping latency low for interactive use.

### Attr Change Scope: Size, Mode, Owner Changes Only
Trigger NOTIFY4_CHANGE_CHILD_ATTRS only for "significant" attribute changes: size, mode, uid, gid. Ignore atime-only changes (would trigger on every READ) and ctime changes (always change, too noisy). The ChildAttrDelay from GET_DIR_DELEGATION args provides a server-side throttle.

### Delegation Granting: Always Grant When Possible
Grant directory delegations to any client that requests them via GET_DIR_DELEGATION, as long as: (1) client has valid lease, (2) server-wide delegation limit not exceeded, (3) backchannel is operational. Do not use heuristics. RFC 8881 allows servers to always refuse, so always-grant is the most useful policy.

### Multiple Clients: Allow Simultaneous Directory Delegations
Multiple clients can hold directory delegations on the same directory simultaneously. All holders receive CB_NOTIFY for subscribed events. This differs from write file delegations (exclusive). Directory delegations are read-only cache hints, not locks.

### Behavior at Delegation Limit: Refuse with GDD4_UNAVAIL
When the server-wide directory delegation limit is reached, refuse new delegations with GDD4_UNAVAIL + will_signal=false. Do NOT recall existing delegations to make room (too aggressive). The limit is a safety valve, not a resource management policy.

### No Proactive Offering
Only grant directory delegations via explicit GET_DIR_DELEGATION request. Do not proactively offer on READDIR. This keeps the implementation simple and avoids sending unsolicited delegations to clients that may not support directory delegation callbacks.

### Conflicting Operation: Proceed After Recall
When a conflicting client modifies a directory (e.g., client B creates a file in a directory delegated to client A), proceed with the operation immediately after sending recall to client A. Do NOT block client B waiting for DELEGRETURN. Client A will learn about the change via CB_RECALL and must re-validate its cache.

### Recall on Directory Deletion: Revoke Immediately
When a directory is deleted while delegated, revoke the delegation immediately (do not send recall and wait). The directory no longer exists, so the client cannot meaningfully return the delegation. Add to recentlyRecalled cache to prevent re-grant storms.

### No Cascade Recall
Recalling a directory delegation does NOT cascade to file delegations within the directory. File and directory delegations are independent per RFC 8881. A directory delegation tracks directory entry changes; file delegations track file content/metadata.

### FREE_STATEID: Same as File Delegations
The existing freeDelegStateidLocked already handles directory delegation stateids correctly since they use the same StateTypeDeleg (0x03) type byte and are stored in the same delegByOther map.

### Shared Limits: Combined File + Directory
Use a single configurable delegation limit that includes both file and directory delegations. Simpler to reason about and configure. Add the limit to NFSAdapterSettings alongside existing DelegationsEnabled field.

## Open Questions

1. **Linux NFS client directory delegation support**
   - What we know: Linux kernel NFS client has been working on directory delegation support since 2024 (LKML patches)
   - What's unclear: Whether the mainline Linux NFS client in common distributions sends GET_DIR_DELEGATION
   - Recommendation: Implement per spec; even without client support, the code exercises the state management infrastructure. Test with synthetic COMPOUND requests.

2. **Notification attribute encoding complexity**
   - What we know: Full fattr4 encoding for each entry is expensive and complex
   - What's unclear: Which attributes clients actually use from CB_NOTIFY vs re-fetching via GETATTR
   - Recommendation: Start with a minimal attribute set in notifications (fileid, type, size, mtime). The GetDirDelegationArgs.ChildAttrs bitmap from the client tells us what they want, but we can return a subset.

## Sources

### Primary (HIGH confidence)
- **RFC 8881 Sections 10.9, 18.39, 20.4** - Directory delegations, GET_DIR_DELEGATION, CB_NOTIFY specification
- **Existing codebase** - DelegationState (delegation.go), BackchannelSender (backchannel.go), callback_common.go, CbNotifyArgs/GetDirDelegationArgs (types/cb_notify.go, types/get_dir_delegation.go), NOTIFY4_* constants (types/constants.go)
- **Phase 11 RESEARCH.md** - File delegation implementation patterns, recall/revocation lifecycle
- **Phase 22 RESEARCH.md** - BackchannelSender design, CB_NOTIFY extensibility

### Secondary (MEDIUM confidence)
- **Phase 16 decisions** - "CB_NOTIFY entries stored as raw opaque deferring sub-type parsing to Phase 24" (STATE.md)
- **Linux nfsd source** - Reference implementation for directory delegation semantics

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH - all packages exist, patterns established in Phase 11/22
- Architecture: HIGH - extends proven patterns (DelegationState, BackchannelSender, handler hooks)
- Pitfalls: HIGH - identified from codebase analysis (lock ordering, batching, race conditions)
- XDR wire format: MEDIUM - notify sub-type encoding needs verification against RFC 8881 XDR appendix

**Research date:** 2026-02-22
**Valid until:** 2026-03-22 (stable domain, no fast-moving dependencies)
