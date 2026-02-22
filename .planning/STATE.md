# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-20)

**Core value:** Enterprise-grade multi-protocol file access with unified locking and Kerberos authentication
**Current focus:** v3.0 NFSv4.1 Sessions — Phase 17 complete, ready for Phase 18

## Current Position

Phase: 24 of 25 (Directory Delegations)
Plan: 3 of 3 in current phase
Status: Phase Complete
Last activity: 2026-02-22 -- Completed 24-03-PLAN.md

Progress: [########################################] 100% (85/85 plans complete)

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |

## Performance Metrics

**Velocity:**
- Total plans completed: 61 (19 v1.0 + 42 v2.0)
- Average duration: ~7 min
- Total execution time: ~7 hours

**By Phase:**

| Phase | Plans | Total | Avg/Plan | Status |
|-------|-------|-------|----------|--------|
| 01-locking-infrastructure | 4 | 75 min | 18.75 min | COMPLETE |
| 02-nlm-protocol | 3 | 25 min | 8.3 min | COMPLETE |
| 03-nsm-protocol | 3 | 19 min | 6.3 min | COMPLETE |
| 04-smb-leases | 3 | 29 min | 9.7 min | COMPLETE |
| 05-cross-protocol-integration | 6 | 37 min | 6.2 min | COMPLETE |
| 06-nfsv4-protocol-foundation | 3 | 30 min | 10.0 min | COMPLETE |
| 07-nfsv4-file-operations | 3 | 35 min | 11.7 min | COMPLETE |
| 08-nfsv4-advanced-operations | 3 | 18 min | 6.0 min | COMPLETE |
| 09-state-management | 4 | 33 min | 8.3 min | COMPLETE |
| 10-nfsv4-locking | 3 | 33 min | 11.0 min | COMPLETE |
| 11-delegations | 4 | 41 min | 10.3 min | COMPLETE |
| 12-kerberos-authentication | 5 | 48 min | 9.6 min | COMPLETE |
| 13-nfsv4-acls | 5 | 43 min | 8.6 min | COMPLETE |
| 14-control-plane-v2-0 | 7 | 48 min | 6.9 min | COMPLETE |
| 15-v2-0-testing | 5 | 24 min | 4.8 min | COMPLETE |
| Phase 16 P01 | 7min | 2 tasks | 9 files |
| Phase 16 P02 | 5min | 2 tasks | 12 files |
| Phase 16 P03 | 7min | 2 tasks | 26 files |
| Phase 16 P04 | 6min | 2 tasks | 18 files |
| Phase 16 P05 | 6min | 2 tasks | 4 files |
| Phase 17 P01 | 3min | 2 tasks | 2 files |
| Phase 17 P02 | 2min | 2 tasks | 2 files |
| Phase 18 P01 | 18min | 2 tasks | 7 files |
| Phase 18 P02 | 8min | 2 tasks | 12 files |
| Phase 19 P01 | 23min | 3 tasks | 20 files |
| Phase 20 P01 | 25min | 2 tasks | 7 files |
| Phase 20 P02 | 14min | 2 tasks | 11 files |
| Phase 21 P01 | 10min | 2 tasks | 14 files |
| Phase 21 P02 | 18min | 2 tasks | 12 files |
| Phase 22 P01 | 35min | 2 tasks | 13 files |
| Phase 22 P02 | 25min | 2 tasks | 8 files |
| Phase 23 P01 | 12min | 2 tasks | 7 files |
| Phase 23 P02 | 8min | 2 tasks | 12 files |
| Phase 23 P03 | 6min | 2 tasks | 9 files |
| Phase 24 P01 | 7min | 2 tasks | 8 files |
| Phase 24 P02 | 12min | 2 tasks | 7 files |
| Phase 24 P03 | 12min | 2 tasks | 13 files |

## Quick Tasks Completed

| # | Description | Branch | PR | Date |
|---|------------|--------|----|------|
| 1 | NFS adapter refactor (issue #148): split 3 oversized files, extract XDR decoder, fix metrics double-decode, add 32 tests | refactor/148-nfs-adapter-cleanup | - | 2026-02-19 |
| 2 | K8s operator: expose NFS portmapper port (Service 111->10111, NetworkPolicy, best-effort enablement) | feat/k8s-nfs-portmapper | #155 | 2026-02-20 |

## Accumulated Context

### Decisions

Decisions are logged in PROJECT.md Key Decisions table.
Recent decisions affecting current work:

- [v3.0 roadmap]: 10 phases derived from 32 requirements, ordered by dependency chain (types -> slot table -> EXCHANGE_ID -> CREATE_SESSION -> SEQUENCE -> connections -> backchannel -> dir delegations)
- [v3.0 roadmap]: SMB Kerberos (SMBKRB-01, SMBKRB-02) placed in Phase 25 (testing) since it reuses shared Kerberos layer from v2.0
- [Phase 16]: SessionId4 encoded as raw 16 bytes (no length prefix) per RFC 4506 fixed-size opaque
- [Phase 16-02]: Response types use status-gated encoding -- if Status != NFS4_OK only status is encoded/decoded
- [Phase 16-03]: LAYOUTCOMMIT uses bool-gated conditional unions for newoffset/time_modify/layout_update fields
- [Phase 16-03]: DeviceId4 encoded as fixed 16 bytes (no length prefix) per RFC 8881 Section 3.3.14
- [Phase 16-04]: CB_NOTIFY entries stored as raw opaque deferring sub-type parsing to Phase 24
- [Phase 16-04]: CB_NOTIFY_DEVICEID uses conditional encoding (Immediate only for CHANGE, not DELETE)
- [Phase 16-05]: v41StubHandler uses typed decoder closures to validate XDR args and prevent stream desync
- [Phase 16-05]: v4.0 ops accessible from v4.1 compounds via fallback to opDispatchTable (per RFC 8881)
- [Phase 17-01]: Per-SlotTable mutex instead of global StateManager.mu for SEQUENCE hot path
- [Phase 17-01]: SequenceValidation is separate type from v4.0 SeqIDValidation (v4.1 seqid wraps through 0)
- [Phase 17-01]: CachedReply stores full XDR bytes for complete replay detection
- [Phase 17-02]: Session struct is independent of StateManager -- registration is Phase 19's job
- [Phase 17-02]: crypto/rand session ID with deterministic fallback (clientID + nanotime)
- [Phase 17-02]: Back channel slot table only allocated when CONN_BACK_CHAN flag is set
- [Phase 18-01]: V41ClientRecord is separate struct from v4.0 ClientRecord (different registration flow)
- [Phase 18-01]: SP4_MACH_CRED/SP4_SSV rejected with NFS4ERR_ENCR_ALG_UNSUPP before state allocation (matches Linux nfsd)
- [Phase 18-01]: ServerIdentity singleton with os.Hostname() for server_owner, consistent across all EXCHANGE_ID calls
- [Phase 18-02]: NFSClientProvider stored as any on Runtime to avoid pkg/ -> internal/ import cycle
- [Phase 18-02]: EvictV40Client with full cleanup (open states, lock states, delegations, lease timers)
- [Phase 19-01]: CREATE_SESSION replay: handler caches encoded XDR bytes via CacheCreateSessionResponse(), StateManager owns multi-case seqid algorithm
- [Phase 19-01]: Channel negotiation clamps to server limits (64 fore slots, 8 back slots, 1MB sizes), no RDMA, MaxOperations=0
- [Phase 19-01]: Session reaper sweeps every 30s, 2x lease duration for unconfirmed client timeout
- [Phase 19-01]: V4MaxSessionSlots/V4MaxSessionsPerClient config fields exist but not yet wired to StateManager
- [Phase 20-01]: seqid=0 sentinel for v4.1 bypass of per-owner seqid validation (safe because v4.0 seqids never use 0)
- [Phase 20-01]: Replay cache at COMPOUND level -- full XDR bytes cached in slot, returned byte-identical on duplicate
- [Phase 20-01]: GetStatusFlags reports CB_PATH_DOWN/BACKCHANNEL_FAULT until backchannel is bound (Phase 22)
- [Phase 20-02]: SequenceMetrics follows exact SessionMetrics nil-safe receiver pattern
- [Phase 20-02]: Minor version range defaults to 0-1 (both v4.0 and v4.1 enabled)
- [Phase 20-02]: Version range check placed before minorversion switch in ProcessCompound
- [Phase 21-01]: Separate connMu RWMutex for connection state (not global sm.mu) to reduce contention
- [Phase 21-01]: Generous direction negotiation: FORE_OR_BOTH -> BOTH, BACK_OR_BOTH -> BOTH
- [Phase 21-01]: Auto-bind on CREATE_SESSION is best-effort (errors logged, do not fail CREATE_SESSION)
- [Phase 21-01]: Lock ordering: sm.mu before connMu (enforced in destroySessionLocked)
- [Phase 21-01]: Connection limit default 16/session with NFS4ERR_RESOURCE enforcement
- [Phase 21-02]: ConnectionMetrics follows nil-safe receiver pattern (registerOrReuse for Prometheus re-registration)
- [Phase 21-02]: unbindConnectionLocked accepts reason parameter for accurate metrics labeling
- [Phase 21-02]: Session API includes ConnectionInfo list and ConnectionSummary in same response
- [Phase 22-01]: Shared wire-format helpers exported to callback_common.go, reused by both v4.0 and v4.1 paths
- [Phase 22-01]: ConnWriter registered lazily after COMPOUND (maybeRegisterBackchannel), not during BIND_CONN_TO_SESSION handler
- [Phase 22-01]: Read-loop demux checks msg_type (bytes 4-7) before RPC parsing to route REPLY messages to PendingCBReplies
- [Phase 22-01]: Callback routing via getBackchannelSender(clientID) -- v4.1 gets BackchannelSender, v4.0 uses dial-out
- [Phase 22-01]: BackchannelSender uses exponential backoff (5s/10s/20s) with 3 retry attempts
- [Phase 22-02]: BackchannelMetrics follows nil-safe receiver + registerOrReuse pattern (same as SessionMetrics/ConnectionMetrics/SequenceMetrics)
- [Phase 22-02]: net.Pipe() for backchannel integration tests (no real TCP listeners, no port conflicts)
- [Phase 22-02]: Metrics wired into sendCallbackWithRetry() for accurate per-attempt tracking
- [Phase 23-01]: DestroyV41ClientID rejects NFS4ERR_CLIENTID_BUSY when sessions remain (strict RFC 8881)
- [Phase 23-01]: FreeStateid uses type byte from Other[0] to route to correct cleanup path (0x01=open, 0x02=lock, 0x03=deleg)
- [Phase 23-01]: TestStateids uses RLock only -- no lease renewal side effects per RFC 8881
- [Phase 23-01]: ReclaimComplete returns NFS4_OK outside grace period (not an error per RFC 8881)
- [Phase 23-01]: GraceStatusInfo exposes RemainingSeconds for API/CLI countdown display
- [Phase 23-02]: DESTROY_CLIENTID is session-exempt per RFC 8881 Section 18.50.3
- [Phase 23-02]: v4.0-only ops (5 ops) rejected with NFS4ERR_NOTSUPP in v4.1 COMPOUNDs via consumeV40OnlyArgs
- [Phase 23-02]: TEST_STATEID returns NFS4_OK overall with per-stateid error codes array (not fail-on-first)
- [Phase 23-03]: Grace status endpoint unauthenticated (like health probes) for K8s and monitoring access
- [Phase 23-03]: Grace period info only shown in dfs status when active (clean output by default)
- [Phase 24-01]: Separate NotifMu per delegation (not global sm.mu) to avoid holding global lock during backchannel sends
- [Phase 24-01]: Directory delegations reuse StateTypeDeleg (0x03) type byte -- same as file delegations
- [Phase 24-01]: Count-based flush at maxBatchSize=100 + timer-based flush at configurable window (default 50ms)
- [Phase 24-01]: directory_deleted recall triggers immediate revocation (no CB_RECALL needed)
- [Phase 24-01]: purgeV41Client now cleans up all delegations (file+directory) for destroyed clients
- [Phase 24-03]: OriginClientID on DirNotification enables conflict recall without separate conflict-checking API
- [Phase 24-03]: isSignificantAttrChange filters atime/ctime-only SETATTR notifications (only mode/owner/group/size)
- [Phase 24-03]: DelegationMetrics uses shared counters with type label (file/directory) following nil-safe receiver pattern
- [Phase 24-03]: REMOVE handler does pre-removal lookup to get child handle for directory delegation revocation
- [Phase 24-02]: GDD4_UNAVAIL is non-fatal per RFC 8881 -- does not fail COMPOUND, just signals no delegation available
- [Phase 24-02]: Two-phase DELEGRETURN for directory delegations: flush with lock released, re-acquire for removal (avoids backchannel deadlock)

### Pending Todos

None.

### Blockers/Concerns

- Phase 20 (SEQUENCE + COMPOUND bifurcation) is highest risk — touches every v4.1 request path
- Phase 22 (backchannel) requires new bidirectional I/O pattern on TCP connections

## Session Continuity

Last session: 2026-02-22
Stopped at: Completed 24-03-PLAN.md (Phase 24 complete)
Resume file: `/gsd:execute-phase 25`
