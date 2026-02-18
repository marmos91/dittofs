# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-04)

**Core value:** Enterprise-grade multi-protocol file access with unified locking and Kerberos authentication
**Current focus:** v2.0 Testing Phase 15 in progress

## Current Position

Phase: 15 of 28 (v2.0 Testing)
Plan: 5 of 5 complete
Status: Phase 15 COMPLETE. All v2.0 testing plans executed.
Last activity: 2026-02-17 - Completed Plan 15-05 (POSIX NFSv4, Control Plane Mount-Level, Stress Tests)

Progress: [#####################################---] 88% (59/67 plans complete)

## Performance Metrics

**Velocity:**
- Total plans completed: 48
- Average duration: 8.3 min
- Total execution time: 5.8 hours

**By Phase:**

| Phase | Plans | Total | Avg/Plan | Status |
|-------|-------|-------|----------|--------|
| 01-locking-infrastructure | 4 | 75 min | 18.75 min | COMPLETE |
| 02-nlm-protocol | 3 | 25 min | 8.3 min | COMPLETE |
| 03-nsm-protocol | 3 | 19 min | 6.3 min | COMPLETE |
| 04-smb-leases | 3 | 29 min | 9.7 min | COMPLETE |
| 05-cross-protocol-integration | 6 | 37 min | 6.2 min | COMPLETE |
| 06-nfsv4-protocol-foundation | 3/3 | 30 min | 10.0 min | COMPLETE |
| 07-nfsv4-file-operations | 3/3 | 35 min | 11.7 min | COMPLETE |
| 08-nfsv4-advanced-operations | 3/3 | 18 min | 6.0 min | COMPLETE |

| 09-state-management | 4/4 | 33 min | 8.3 min | COMPLETE |
| 10-nfsv4-locking | 3/3 | 33 min | 11.0 min | COMPLETE |
| 11-delegations | 4/4 | 41 min | 10.3 min | COMPLETE |
| 12-kerberos-authentication | 5/5 | 48 min | 9.6 min | COMPLETE |
| 13-nfsv4-acls | 5/5 | 43 min | 8.6 min | COMPLETE |

| 14-control-plane-v2-0 | 7/7 | 48 min | 6.9 min | COMPLETE |
| 15-v2-0-testing | 5/5 | 24 min | 4.8 min | COMPLETE |

**Recent Trend:**
- Last 5 plans: 15-01 (5 min), 15-02 (4 min), 15-03 (3 min), 15-04 (4 min), 15-05 (7 min)
- Trend: Phase 15 COMPLETE, all 5 plans avg 4.8 min

*Updated after each plan completion*
| Phase 15 P02 | 4 | 2 tasks | 2 files |

## Phase 01 Accomplishments

### Plan 01-01: Lock Manager Enhancements
- EnhancedLock type with protocol-agnostic ownership model
- POSIX lock splitting (SplitLock, MergeLocks)
- Atomic lock upgrade (shared to exclusive)
- Wait-For Graph deadlock detection
- Lock configuration and limits tracking

### Plan 01-02: Lock Persistence
- LockStore interface for all metadata store backends
- Memory, BadgerDB, PostgreSQL implementations
- Server epoch tracking for split-brain detection
- Transaction integration for atomic operations

### Plan 01-03: Grace Period and Metrics
- Grace period state machine for lock reclaim
- Connection tracker with adapter-controlled TTL
- Full Prometheus metrics suite
- Early grace period exit optimization

### Plan 01-04: Package Reorganization - COMPLETE
- Created `pkg/metadata/errors/` package (leaf, no deps)
- Created `pkg/metadata/lock/` package for all lock code
- Import graph: errors <- lock <- metadata <- stores
- No circular dependencies
- Backward compatibility via type aliases

## Phase 02 Accomplishments

### Plan 02-01: XDR Utilities and NLM Types - COMPLETE
- Shared XDR package at internal/protocol/xdr/ (no DittoFS dependencies)
- NFS XDR refactored to delegate to shared utilities
- NLM v4 constants (program 100021, procedures, status codes)
- NLM v4 types (NLM4Lock, NLM4Holder, request/response structures)
- NLM XDR encode/decode functions for all message types

### Plan 02-02: NLM Dispatcher and Synchronous Operations - COMPLETE
- NLM procedure handlers (NULL, TEST, LOCK, UNLOCK, CANCEL)
- NLM dispatch table mapping procedures to handlers
- MetadataService NLM methods (LockFileNLM, TestLockNLM, UnlockFileNLM, CancelBlockingLock)
- NLM program routing in NFS adapter (same port 12049)
- Package restructure to avoid import cycles (nlm/types subpackage)

### Plan 02-03: Blocking Lock Queue and GRANTED Callback - COMPLETE
- Per-file blocking lock queue with configurable limit (100 per file)
- NLM_GRANTED callback client with 5s TOTAL timeout
- Queue integration with lock/unlock handlers
- SetNLMUnlockCallback for async waiter notification
- NLM Prometheus metrics (nlm_* prefix)

## Phase 03 Accomplishments

### Plan 03-01: NSM Types and Foundation - COMPLETE
- NSM types package at internal/protocol/nsm/types/
- NSM XDR encode/decode at internal/protocol/nsm/xdr/
- Extended ClientRegistration with NSM fields (MonName, Priv, SMState, CallbackInfo)
- NSMCallback struct for RPC callback details
- ClientRegistrationStore interface for persistence
- Conversion functions for persistence (To/From PersistedClientRegistration)

### Plan 03-02: NSM Handlers and Dispatch - COMPLETE
- NSM handler struct with ConnectionTracker, ClientRegistrationStore, server state
- NSM dispatch table mapping procedures to handlers
- SM_NULL, SM_STAT, SM_MON, SM_UNMON, SM_UNMON_ALL, SM_NOTIFY handlers
- Client registration storage in Memory, BadgerDB, PostgreSQL
- PostgreSQL migration 000003_clients for nsm_client_registrations table
- NSM program (100024) routing in NFS adapter

### Plan 03-03: NSM Crash Recovery - COMPLETE
- SM_NOTIFY callback client with 5s total timeout
- Notifier for parallel SM_NOTIFY on server restart
- NLM FREE_ALL handler (procedure 23) for bulk lock release
- NSM Prometheus metrics (nsm_* prefix)
- NFS adapter integration for startup notification

## Phase 04 Accomplishments

### Plan 04-01: SMB Lease Types - COMPLETE
- LeaseInfo struct with R/W/H state flags matching MS-SMB2 spec
- Lease state constants (0x01=R, 0x02=W, 0x04=H)
- EnhancedLock.Lease field for unified lock manager integration
- PersistedLock lease fields (LeaseKey, LeaseState, LeaseEpoch, BreakToState, Breaking)
- Lease conflict detection in IsEnhancedLockConflicting
- LockQuery.IsLease filter for listing leases vs byte-range locks
- Full round-trip conversion (ToPersistedLock/FromPersistedLock)

### Plan 04-02: OplockManager Refactoring - COMPLETE
- OplockManager refactored with LockStore dependency for lease persistence
- RequestLease/AcknowledgeLeaseBreak/ReleaseLease methods
- LeaseBreakScanner with 35s default timeout
- Cross-protocol break triggers (CheckAndBreakForWrite/Read)
- Backward compatible with existing oplock API

### Plan 04-03: Cross-Protocol Breaks and CREATE Context - COMPLETE
- ErrLeaseBreakPending error for signaling pending breaks
- CheckAndBreakForWrite/Read return ErrLeaseBreakPending for W leases
- OplockChecker interface in MetadataService for cross-protocol visibility
- waitForLeaseBreak helper in NFS handlers with 35s timeout
- NFS WRITE/READ handlers call CheckAndBreakLeasesFor{Write,Read}
- Lease create context (RqLs/RsLs) parsing and encoding per MS-SMB2
- SMB CREATE processes lease contexts when OplockLevel=0xFF
- CREATE response includes granted lease state in RsLs context

## Phase 05 Accomplishments

### Plan 05-01: Unified Lock View Foundation - COMPLETE
- UnifiedLockView struct in pkg/metadata/ for cross-protocol lock visibility
- FileLocksInfo separates ByteRangeLocks and Leases for easy processing
- GetAllLocksOnFile, HasConflictingLocks, GetLeaseByKey, GetWriteLeases, GetHandleLeases
- NLMHolderInfo and TranslateToNLMHolder for NLM4_DENIED responses
- Cross-protocol Prometheus metrics (cross_protocol_conflict_total, cross_protocol_break_duration_seconds)
- MetadataService owns UnifiedLockView per share

### Plan 05-02: NLM-SMB Integration - COMPLETE
- Configurable lease break timeout (default 35s, CI=5s via DITTOFS_LOCK_LEASE_BREAK_TIMEOUT)
- NLM cross_protocol.go with waitForLeaseBreak, buildDeniedResponseFromSMBLease helpers
- NLM LOCK checks for SMB leases before acquiring, waits for lease break
- OplockChecker.CheckAndBreakForDelete for Handle lease breaks
- NFS REMOVE/RENAME break Handle leases before deletion
- SMB adapter registers OplockManager with MetadataService.SetOplockChecker

### Plan 05-03: SMB-to-NFS Integration - COMPLETE
- SMB handlers check NLM byte-range locks before granting leases
- Write lease denied when ANY NLM lock exists on file
- Read lease denied when exclusive NLM lock exists
- STATUS_LOCK_NOT_GRANTED (0xC0000054) for NLM conflicts
- Cross-protocol conflicts logged at INFO level
- CREATE succeeds even when lease denied (only caching affected)

### Plan 05-04: Cross-Protocol Locking E2E Tests - COMPLETE
- File locking helpers (LockFile, TryLockFile, LockFileRange) using flock/fcntl
- TestCrossProtocolLocking with XPRO-01 to XPRO-04 subtests
- Grace period recovery tests (TestGracePeriodRecovery, TestCrossProtocolReclaim)
- Byte-range specific locking tests for fine-grained conflict detection
- Platform-specific notes logging for macOS vs Linux

### Plan 05-05: SMB Lease Grace Period Reclaim - COMPLETE [GAP CLOSURE]
- Added `Reclaim bool` field to LeaseInfo for tracking grace period reclaims
- Added `ReclaimLease` method to LockStore interface
- Implemented `ReclaimLeaseSMB` in MetadataService for SMB lease recovery
- Added `LeaseReclaimer` interface and `RequestLeaseWithReclaim` to OplockManager
- Added `OnReconnect` hook to SMB adapter for session reconnection handling

### Plan 05-06: E2E SMB Mount Support - COMPLETE [GAP CLOSURE]
- Added `IsNativeSMBAvailable()` to check platform-specific SMB mount capability
- Added `SkipIfNoSMBMount(t)` helper for graceful test skipping
- Cross-protocol tests skip gracefully when SMB mount unavailable
- Platform support: Windows (native), macOS (mount_smbfs), Linux (cifs-utils)
- Note: Docker fallback approach removed per user feedback (proven unreliable)

## Phase 06 Accomplishments

### Plan 06-01: NFSv4 Types, Constants, and Attribute Helpers - COMPLETE
- All 40 NFSv4 operation numbers defined per RFC 7530 (OP_ACCESS=3 through OP_ILLEGAL=10044)
- All 48+ NFSv4 error codes with exact values (NFS4_OK=0 through NFS4ERR_CB_PATH_DOWN=10048)
- CompoundContext, Compound4Args, Compound4Response structs for COMPOUND dispatch
- MapMetadataErrorToNFS4 mapping 20+ internal errors to NFS4 status codes
- ValidateUTF8Filename per RFC 7530 Section 12.7
- Bitmap4 encode/decode/SetBit/IsBitSet/ClearBit/Intersect helpers
- FATTR4 mandatory + recommended attribute bit numbers
- EncodePseudoFSAttrs with PseudoFSAttrSource interface

### Plan 06-02: COMPOUND Dispatcher and Version Routing - COMPLETE
- PseudoFS virtual namespace tree with handle generation, dynamic rebuilds, and handle stability
- COMPOUND dispatcher with sequential op execution, stop-on-error, tag echo, minor version validation
- NFSv4 version routing: v3 and v4 operate simultaneously on the same port
- V4 Handler struct with extensible op dispatch table (NFS4ERR_NOTSUPP for unimplemented ops)
- NFSv4 NULL procedure handler (RPC procedure 0)
- ExtractV4HandlerContext for AUTH_UNIX credential extraction
- Removed macOS kernel workaround (v4 now supported)
- 27 new tests (16 pseudofs + 11 compound) all passing with race detection

### Plan 06-03: NFSv4 Operation Handlers - COMPLETE
- 14 operation handlers (PUTFH, PUTROOTFH, PUTPUBFH, GETFH, SAVEFH, RESTOREFH, LOOKUP, LOOKUPP, GETATTR, READDIR, ACCESS, ILLEGAL, SETCLIENTID, SETCLIENTID_CONFIRM)
- All handlers registered in COMPOUND dispatch table
- LOOKUP traverses pseudo-fs tree with export junction crossing support
- GETATTR encodes requested attributes using bitmap intersection
- READDIR lists pseudo-fs children with cookie pagination
- SETCLIENTID/SETCLIENTID_CONFIRM stubs for Phase 9 state management
- 27 new tests covering all handlers, error cases, and end-to-end pseudo-fs browsing

## Phase 07 Accomplishments

### Plan 07-01: NFSv4 File Operation Handlers - COMPLETE
- buildV4AuthContext helper for identity mapping, squashing, and permission resolution
- EncodeRealFileAttrs encodes all fattr4 attributes from real metadata.File
- MapFileTypeToNFS4 type conversion utility
- PseudoFS.FindJunction for LOOKUPP cross-back to pseudo-fs namespace
- LOOKUP resolves real directory children via MetadataService.Lookup
- LOOKUPP navigates to parent with pseudo-fs cross-back at share root
- GETATTR returns real file attributes (type, size, mode, timestamps)
- READDIR lists real directory entries with per-entry fattr4 attributes
- ACCESS checks Unix permissions (owner/group/other) with root bypass
- READLINK handler returns symlink target paths
- 20 new real-FS tests plus 5 pseudo-fs regression tests

### Plan 07-02: NFSv4 CREATE/REMOVE Operations - COMPLETE
- CREATE handler for NF4DIR (directories) and NF4LNK (symlinks) via MetadataService
- REMOVE handler with auto-detect file vs directory (try RemoveFile, fallback to RemoveDirectory)
- change_info4 encoding for parent directory cache coherency
- createtype4, createmode4, OPEN share/claim/delegation, write stability constants
- Block/char devices, sockets, FIFOs return NFS4ERR_NOTSUPP
- Regular file creation returns NFS4ERR_BADTYPE (files use OPEN)
- 15 unit tests with race detection

### Plan 07-03: NFSv4 File I/O Handlers - COMPLETE
- Stateid4 type with RFC 7530-compliant encode/decode and special-stateid detection
- OPEN handler (CLAIM_NULL with UNCHECKED4/GUARDED4/EXCLUSIVE4 create modes)
- OPEN_CONFIRM handler (placeholder for Phase 9 state management)
- CLOSE handler (accepts any stateid, returns zeroed stateid)
- READ handler via PayloadService.ReadAt with EOF detection and COW support
- WRITE handler using two-phase pattern (PrepareWrite/WriteAt/CommitWrite)
- COMMIT handler via PayloadService.Flush with server boot verifier
- 37+ comprehensive tests covering I/O lifecycle, roundtrips, and edge cases

## Phase 08 Accomplishments

### Plan 08-01: NFSv4 LINK and RENAME Operations - COMPLETE
- LINK handler using SavedFH (source file) + CurrentFH (target directory) two-filehandle pattern
- RENAME handler with dual change_info4 for source and target directories
- Cross-share XDEV detection via DecodeFileHandle share name comparison
- Pseudo-fs read-only rejection (LINK checks CurrentFH, RENAME checks both handles)
- Both operations registered in COMPOUND dispatch table
- 21 tests covering success paths, error conditions, and compound sequences

### Plan 08-02: SETATTR Handler and fattr4 Decode Infrastructure - COMPLETE
- fattr4 decode infrastructure (DecodeFattr4ToSetAttrs) supporting all 6 writable attributes
- Owner/group string parsing (ParseOwnerString, ParseGroupString) with numeric@domain, bare numeric, well-known names
- SETATTR handler: stateid decode, fattr4 decode, MetadataService.SetFileAttributes, attrsset bitmap response
- NFS4StatusError interface for typed decode errors (ATTRNOTSUPP, INVAL, BADOWNER)
- WritableAttrs bitmap, time_how4 constants (SET_TO_SERVER_TIME4, SET_TO_CLIENT_TIME4)
- 29 tests (15 decode + 14 handler) all passing with -race

### Plan 08-03: VERIFY/NVERIFY + SECINFO Upgrade + Stubs - COMPLETE
- VERIFY handler: byte-exact XDR comparison of client-provided fattr4 against server attrs
- NVERIFY handler: inverse of VERIFY (succeeds when attrs differ)
- Shared verifyAttributes helper + encodeAttrValsOnly for DRY comparison
- SECINFO upgraded from 1 flavor (AUTH_SYS) to 2 flavors (AUTH_SYS + AUTH_NONE)
- OPENATTR stub (NFS4ERR_NOTSUPP), OPEN_DOWNGRADE stub (NFS4ERR_NOTSUPP)
- RELEASE_LOCKOWNER stub (NFS4_OK no-op) to prevent client cleanup errors
- 16 tests (11 VERIFY/NVERIFY + 5 stubs/SECINFO) all passing with -race

## Phase 09 Accomplishments

### Plan 09-01: Client ID Management (SETCLIENTID, SETCLIENTID_CONFIRM) - COMPLETE
- StateManager central coordinator at internal/protocol/nfs/v4/state/
- ClientRecord with five-case SETCLIENTID algorithm per RFC 7530 Section 9.1.1
- Boot epoch + atomic counter client ID generation (unique across server restarts)
- crypto/rand confirm verifier generation (not timestamps)
- Handler.StateManager field with backward-compatible variadic constructor
- Removed global nextClientID atomic counter from setclientid.go
- V4ClientState extended with ClientID field
- 18 unit tests covering all SETCLIENTID cases with race detection

### Plan 09-02: Stateid and Open-State Tracking - COMPLETE
- Stateid generation with type-tagged other field (type(1) + epoch(3) + counter(8))
- OpenOwner tracking with seqid validation (OK/Replay/Bad) and replay caching
- OpenState lifecycle: OpenFile, ConfirmOpen, CloseFile, DowngradeOpen via StateManager
- Stateid validation for READ/WRITE (rejects bad/old/stale, allows special stateids)
- RENEW handler validates client ID and updates LastRenewal timestamp
- OPEN_DOWNGRADE with share mode subset validation
- Full integration test: SETCLIENTID -> CONFIRM -> OPEN -> CONFIRM -> WRITE -> READ -> RENEW -> CLOSE

### Plan 09-03: Lease Management (RENEW, Expiration) - COMPLETE
- LeaseState with timer-based expiration, Renew, Stop, IsExpired methods
- ConfirmClientID creates lease timer for newly confirmed clients
- onLeaseExpired cleans up all client state (open states, owners, records)
- ValidateStateid checks lease expiry and renews implicitly (Pitfall 3)
- WRITE handler checks ShareAccess, returns NFS4ERR_OPENMODE for read-only opens
- GETATTR returns configured lease_time from StateManager
- StateManager.Shutdown stops all active lease timers
- 14 lease tests covering expiration, renewal, cleanup, concurrent access

### Plan 09-04: Grace Period Handling - COMPLETE
- GracePeriodState with timer-based auto-expiry and early exit on all-reclaimed
- OPEN with CLAIM_NULL blocked during grace (NFS4ERR_GRACE)
- OPEN with CLAIM_PREVIOUS allowed during grace for state reclaim
- CLAIM_PREVIOUS outside grace returns NFS4ERR_NO_GRACE
- SaveClientState/GetConfirmedClientIDs for shutdown persistence
- CLAIM_DELEGATE_CUR/CLAIM_DELEGATE_PREV return NFS4ERR_NOTSUPP
- 9 grace period tests passing with race detection

## Phase 10 Accomplishments

### Plan 10-01: LOCK Operation with Lock-Owner State Management - COMPLETE
- Lock type constants (READ_LT, WRITE_LT, READW_LT, WRITEW_LT) per RFC 7530
- LockOwner, LockState, LockResult, LOCK4denied data model
- StateManager.LockNew() for open-to-lock-owner transition (locker4 new path)
- StateManager.LockExisting() for existing lock stateid path (locker4 exist path)
- acquireLock bridge to unified lock.Manager with cross-protocol OwnerID
- LOCK handler with full locker4 union XDR decoding
- validateOpenModeForLock for NFS4ERR_OPENMODE checks
- 28 tests (22 state-level + 6 handler-level) passing with race detection

### Plan 10-02: LOCKT and LOCKU Operations - COMPLETE
- LOCKT handler for stateless lock conflict testing (no state created)
- StateManager.TestLock queries lock manager via ListEnhancedLocks + IsEnhancedLockConflicting
- LOCKU handler for byte-range lock release with POSIX split semantics
- StateManager.UnlockFile validates stateid/seqid and calls RemoveEnhancedLock
- parseConflictOwner extracts clientID/ownerData from conflict OwnerID strings
- 12 new handler-level tests covering LOCKT and LOCKU scenarios

### Plan 10-03: RELEASE_LOCKOWNER and Integration - COMPLETE
- RELEASE_LOCKOWNER real implementation replacing no-op stub
- NFS4ERR_LOCKS_HELD enforcement in CloseFile (before state removal)
- ReleaseLockOwner checks active locks via lock manager before cleanup
- Lease expiry cascading cleanup: lock states -> lock-owners -> lock manager
- OpenState.LockStates typed as []*LockState (was []interface{})
- Full lock lifecycle E2E test: OPEN -> LOCK -> LOCKT -> LOCKU -> RELEASE_LOCKOWNER -> CLOSE

## Phase 11 Accomplishments

### Plan 11-01: Delegation State Tracking and DELEGRETURN - COMPLETE
- DelegationState struct with stateid (type tag 0x03), client tracking, recall/revoke flags
- delegByOther and delegByFile dual-map indexing in StateManager
- GrantDelegation, ReturnDelegation (idempotent), GetDelegationsForFile, countOpensOnFile
- onLeaseExpired delegation cleanup cascade
- DELEGRETURN handler registered in COMPOUND dispatch table
- CB_RECALL, CB_GETATTR, ACE4, space limit constants
- 20 tests (14 state + 6 handler) all passing with -race

### Plan 11-02: NFSv4 Callback Client - COMPLETE
- ParseUniversalAddr for IPv4/IPv6 uaddr to host:port conversion per RFC 5665
- CB_COMPOUND and CB_RECALL XDR encoding per RFC 7530 wire format
- SendCBRecall creates TCP connection, sends framed RPC CALL with CB_COMPOUND, validates NFS4 reply
- SendCBNull verifies callback path with lightweight CB_NULL RPC call
- RPC message building and record marking follows NLM callback pattern
- 28 tests (12 address parsing + 2 encoding + 4 RPC + 10 integration) all passing with -race

### Plan 11-03: OPEN Delegation Integration - COMPLETE
- ShouldGrantDelegation policy: callback check, exclusive access, no existing delegations
- CheckDelegationConflict with async CB_RECALL via goroutine (no lock during TCP)
- EncodeDelegation for full open_delegation4 wire format (READ/WRITE/NONE with ACE)
- ValidateDelegationStateid for CLAIM_DELEGATE_CUR support
- OPEN handler: conflict check -> NFS4ERR_DELAY, grant decision after OpenFile
- CLAIM_DELEGATE_CUR handler validates delegation stateid and opens file
- DELEGPURGE handler returns NFS4ERR_NOTSUPP (no CLAIM_DELEGATE_PREV support)
- 18 new tests covering all policy branches, conflict scenarios, encoding, validation

### Plan 11-04: Recall Timeout, Revocation, and Anti-Storm Protection - COMPLETE
- RecallTimer on DelegationState with StartRecallTimer/StopRecallTimer methods
- RevokeDelegation marks delegation revoked, removes from delegByFile, keeps in delegByOther
- CB_NULL async verification on SETCLIENTID_CONFIRM sets CBPathUp per client
- CBPathUp=false prevents delegation grants in ShouldGrantDelegation
- Recently-recalled cache (30s TTL) prevents grant-recall-grant storms
- ReturnDelegation stops recall timer, handles revoked delegation return (NFS4_OK)
- ValidateDelegationStateid returns NFS4ERR_BAD_STATEID for revoked delegations
- Shutdown stops all recall timers to prevent timer goroutines firing after shutdown
- 16 new tests covering all revocation, callback path, and anti-storm scenarios

## Phase 12 Accomplishments

### Plan 12-01: Foundation Types and Configuration - COMPLETE
- RPCSEC_GSS XDR types: RPCGSSCredV1 decode/encode, RPCGSSInitRes encode
- Sliding window sequence number tracker with bitmap-based replay detection
- RFC 2203 constants (AuthRPCSECGSS=6, gss_proc, service levels, MAXSEQ)
- KRB5 OID, pseudo-flavors (krb5/krb5i/krb5p), RFC 4121 key usage constants
- KerberosProvider with keytab/krb5.conf loading and hot-reload
- StaticMapper for principal@REALM to metadata.Identity conversion
- KerberosConfig in pkg/config with defaults and env var overrides
- 20 tests passing with -race

### Plan 12-02: GSS Context State Machine - COMPLETE
- GSSContext struct with handle, principal, realm, session key, sequence window
- ContextStore with sync.Map O(1) lookup, TTL-based expiration, LRU eviction
- GSSProcessor orchestrating RPCSEC_GSS INIT/DESTROY lifecycle
- Verifier interface for mockable AP-REQ verification (Krb5Verifier for production)
- extractAPReq strips GSS-API token wrapper to extract raw AP-REQ
- Store-before-reply ordering enforced (NFS-Ganesha bug prevention)
- 43 tests passing with -race (12 context + 22 framework + 9 existing)

### Plan 12-03: RPC Integration - COMPLETE
- handleData validates sequence numbers, checks MAXSEQ, maps principal to Identity via IdentityMapper
- Reply verifier computes MIC of XDR-encoded seq_num using gokrb5 MICToken (KeyUsageAcceptorSign=25)
- GSSProcessor wired into handleRPCCall: INIT/DESTROY control path, DATA dispatch path, silent discard
- GSS identity via context.Value to both NFSv3 ExtractHandlerContext and NFSv4 ExtractV4HandlerContext
- MakeGSSSuccessReply and MakeAuthErrorReply (CREDPROBLEM/CTXPROBLEM) in rpc/parser.go
- SetKerberosConfig method on NFSAdapter for pre-SetRuntime Kerberos configuration

### Plan 12-04: SECINFO Upgrade - COMPLETE
- krb5i integrity: UnwrapIntegrity/WrapIntegrity for rpc_gss_integ_data (MIC verification)
- krb5p privacy: UnwrapPrivacy/WrapPrivacy for rpc_gss_priv_data (WrapToken verification)
- Dual sequence number validation (credential + body) for both krb5i and krb5p
- Reply body wrapping in NFS connection handler for krb5i/krb5p service levels
- SECINFO returns RPCSEC_GSS entries (krb5p, krb5i, krb5) with KRB5 OID when Kerberos enabled
- SECINFO entry order: krb5p > krb5i > krb5 > AUTH_SYS > AUTH_NONE (most secure first)

### Plan 12-05: Keytab Hot-Reload, Metrics, and Lifecycle Test - COMPLETE
- KeytabManager with 60s polling for file changes, atomic reload on modification
- resolveKeytabPath/resolveServicePrincipal for DITTOFS_KERBEROS_KEYTAB and DITTOFS_KERBEROS_PRINCIPAL env vars
- GSSMetrics struct with dittofs_gss_ prefix: context creations/destructions, active gauge, auth failures, data requests, duration histograms
- WithMetrics functional option for GSSProcessor (zero overhead when nil)
- Full RPCSEC_GSS lifecycle integration test: INIT -> DATA -> duplicate rejection -> DESTROY -> stale handle
- 15 new tests (12 keytab + 3 lifecycle/metrics) all passing with -race

## Phase 13 Accomplishments

### Plan 13-01: ACL Types and Evaluation Engine - COMPLETE
- ACE/ACL types with all RFC 7530 Section 6 constants (4 types, 7 flags, 16 mask bits, 3 special identifiers)
- Process-first-match ACL evaluation engine with INHERIT_ONLY skipping and dynamic OWNER@/GROUP@/EVERYONE@ resolution
- Canonical ordering validation (strict Windows order: explicit DENY > ALLOW > inherited DENY > ALLOW)
- DeriveMode extracts rwx from OWNER@/GROUP@/EVERYONE@ ALLOW ACEs
- AdjustACLForMode modifies only special identifiers, preserves explicit user/group ACEs
- ComputeInheritedACL handles FILE_INHERIT, DIRECTORY_INHERIT, NO_PROPAGATE, INHERIT_ONLY
- PropagateACL replaces inherited ACEs while preserving explicit ACEs
- Protocol-agnostic package with zero external imports

### Plan 13-02: Identity Mapper Package - COMPLETE
- IdentityMapper interface with Resolve(ctx, principal) -> (*ResolvedIdentity, error)
- ConventionMapper: user@REALM resolution with case-insensitive domain matching
- ConventionMapper: numeric UID support for AUTH_SYS interop
- TableMapper: explicit mapping table from MappingStore with userLookup callback
- CachedMapper: TTL-based caching with double-check locking, error caching, invalidation, stats
- StaticMapper migrated from pkg/auth/kerberos with backward-compat wrapper
- GroupResolver interface for group@domain ACE evaluation
- MappingStore interface for explicit mapping CRUD
- ParsePrincipal helper and NobodyIdentity utility
- pkg/identity has zero external imports (stdlib only)
- 34 tests passing with -race including concurrent cache access

### Plan 13-03: ACL Metadata Integration - COMPLETE
- FileAttr.ACL field (nil=Unix mode, non-nil=ACL evaluation, empty ACEs=deny all)
- calculatePermissions branches on ACL presence with evaluateACLPermissions
- evaluateWithACL maps Permission flags to NFSv4 ACE mask bits per operation
- createEntry inherits ACL from parent via acl.ComputeInheritedACL
- chmod adjusts OWNER@/GROUP@/EVERYONE@ ACEs via acl.AdjustACLForMode
- SetAttrs supports ACL with validation, CopyFileAttr deep-copies ACL
- PostgreSQL migration 000004: ACL JSONB column with partial index
- PostgreSQL read/write updated for ACL JSONB serialization
- IdentityMapping GORM model with controlplane store CRUD (4 methods)
- AllModels() includes IdentityMapping for auto-migration

### Plan 13-04: NFSv4 ACL Wire Format and Handler Integration - COMPLETE
- EncodeACLAttr/DecodeACLAttr for full nfsace4 wire format per RFC 7531 with round-trip fidelity
- EncodeACLSupportAttr reporting all 4 ACE type support flags (0x0F)
- DecodeACLAttr rejects >128 ACEs to prevent resource exhaustion
- FATTR4_ACL (bit 12) and FATTR4_ACLSUPPORT (bit 13) in SupportedAttrs bitmap
- FATTR4_ACL in WritableAttrs bitmap for SETATTR support
- GETATTR encodes ACL for both pseudo-fs (empty) and real files
- SETATTR decodes and validates ACL from XDR with proper NFS4 error codes
- IdentityMapper field on Handler struct for FATTR4_OWNER reverse resolution
- Package-level SetIdentityMapper for runtime configuration without signature changes

## Phase 15 Accomplishments

### Plan 15-01: NFSv4 E2E Framework and Basic Operations - COMPLETE
- MountNFSWithVersion/MountNFSExportWithVersion helpers for v3/v4.0 parameterized mounts
- Version field on Mount struct for NFS version tracking
- SkipIfDarwin, SkipIfNoNFS4ACLTools, SkipIfNFSv4Unsupported platform skip helpers
- CleanupStaleMounts updated for dittofs-e2e-nfsv4-* patterns
- 8 E2E test functions: basic ops, advanced ops, OPEN modes, pseudo-FS, READDIR pagination, golden path, stale handle, backward compat
- All existing v1.0 E2E tests compile without changes (backward compat verified)

### Plan 15-02: NFSv4 Locking and Delegation E2E Tests - COMPLETE
- TestNFSv4Locking with 6 subtests (ReadWriteLocks, ExclusiveLock, OverlappingRanges, LockUpgrade, LockUnlock, CrossClientConflict) parameterized for v3 and v4.0
- TestNFSv4BlockingLock for v4-only blocking lock semantics (F_SETLKW with goroutine monitoring)
- TestNFSv4DelegationBasicLifecycle with server log verification for delegation grants
- TestNFSv4DelegationRecall with multi-client data consistency and CB_RECALL log scraping
- TestNFSv4DelegationRevocation for unresponsive client revocation scenario
- TestNFSv4NoDelegationConflict for concurrent read access without conflicts
- Log-based delegation observability helpers (readLogFile, extractNewLogs)

### Plan 15-03: NFSv4 ACL and Kerberos E2E Tests - COMPLETE
- NFSv4 ACL lifecycle E2E tests: set/read ACLs via nfs4_setfacl/nfs4_getfacl, deny ACE enforcement
- ACL inheritance tests with FILE_INHERIT/DIRECTORY_INHERIT flags
- ACL access enforcement test (restrictive ACL, chmod interop)
- Cross-protocol ACL interop test (NFSv4 -> SMB, skips when unavailable)
- NFSv4 Kerberos extended E2E tests with explicit vers=4.0: all 3 flavors (krb5/krb5i/krb5p)
- Authorization denial for unmapped users, file ownership mapping, AUTH_SYS fallback
- Concurrent Kerberos users test with cross-visibility verification
- Helper functions: nfs4SetACL, nfs4AddACE, nfs4GetACL, krbV4Mount

### Plan 15-04: Store Matrix, Recovery, and Concurrency Tests - COMPLETE
- TestStoreMatrixV4: 18 subtests (2 versions x 9 backends) with file I/O verification
- TestFileSizeMatrix: v3+v4 x 500KB/1MB/10MB/100MB with SHA-256 checksum verification
- TestMultiShareConcurrent: two shares mounted simultaneously with isolation verification
- TestMultiClientConcurrency: two mounts to same share with goroutine-based concurrent writes
- TestServerRestartRecovery: BadgerDB+filesystem persistent backends survive graceful restart
- TestStaleNFSHandle: memory backend restart returns ENOENT after unmount+remount
- TestSquashBehavior: root_squash/all_squash ownership verification (requires root)
- TestClientReconnection: adapter disable/re-enable with 30s reconnection timeout

### Plan 15-05: POSIX NFSv4, Control Plane Mount-Level, and Stress Tests - COMPLETE
- setup-posix.sh --nfs-version parameter for NFSv3/NFSv4 POSIX compliance testing
- known_failures_v4.txt documenting NFSv4-specific pjdfstest expected failures
- TestNFSv4ControlPlaneBlockedOps: block/unblock WRITE via API, verify mount behavior
- TestNFSv4ControlPlaneNetgroup: netgroup access control via NFS mount
- TestNFSv4ControlPlaneSettingsHotReload: delegation policy and blocked ops hot-reload
- TestStressLargeDirectory/ConcurrentDelegations/ConcurrentFileCreation behind -tags=stress
- run-e2e.sh with --coverage, --stress, --s3, --nfs-version, --race flags

## Accumulated Context

### Decisions

Decisions are logged in PROJECT.md Key Decisions table.
Recent decisions affecting current work:

- [Init]: NLM before NFSv4 - Build locking foundation first
- [Init]: Unified Lock Manager - Single lock model for NFS+SMB
- [Init]: Lock state in metadata store - Atomic with file operations
- [01-01]: OwnerID as opaque string - Lock manager does not parse protocol prefix
- [01-01]: Enhanced locks stored per-LockManager instance (not global)
- [01-01]: Atomic upgrade returns ErrLockConflict when other readers exist
- [01-02]: LockStore embedded in Transaction for atomic operations
- [01-02]: PersistedLock uses string FileID for storage efficiency
- [01-03]: Grace period blocks new locks, allows reclaims and tests
- [01-03]: Connection TTL controlled by adapter (NFS=0, SMB may have grace)
- [01-04]: Import graph: errors <- lock <- metadata <- stores
- [01-04]: Backward compatibility via type aliases in pkg/metadata
- [02-01]: Shared XDR package at internal/protocol/xdr/ for NFS+NLM reuse
- [02-01]: NLM v4 only (64-bit offsets/lengths), not v1-3
- [02-02]: NLM types moved to nlm/types subpackage to avoid import cycle
- [02-02]: NLM handler initialized with MetadataService from runtime
- [02-02]: Owner ID format: nlm:{caller_name}:{svid}:{oh_hex}
- [02-03]: 5 second TOTAL timeout for NLM_GRANTED callbacks
- [02-03]: Fresh TCP connection per callback (no caching)
- [02-03]: Release lock immediately on callback failure
- [02-03]: Unlock callback pattern for async waiter notification
- [03-01]: NSM types package mirrors NLM structure
- [03-01]: priv field as [16]byte fixed array (XDR opaque[16])
- [03-01]: ClientRegistrationStore interface for persistence
- [03-01]: Extend existing ClientRegistration vs new type
- [03-02]: HandlerResult in handlers package (close to handlers)
- [03-02]: Client ID format: nsm:{client_addr}:{callback_host}
- [03-02]: NSM v1 only (standard version)
- [03-03]: Parallel SM_NOTIFY using goroutines for fastest recovery
- [03-03]: Failed notification = client crashed, cleanup locks immediately
- [03-03]: FREE_ALL returns void per NLM spec
- [03-03]: Background notification goroutine (non-blocking)
- [04-01]: Lease state constants match MS-SMB2 2.2.13.2.8 spec values
- [04-01]: LeaseInfo embedded in EnhancedLock via pointer (nil for byte-range locks)
- [04-01]: Centralized MatchesLock method in LockQuery for consistent filtering
- [04-01]: BreakStarted is runtime-only, not persisted
- [04-02]: LockStore dependency injected via NewOplockManagerWithStore
- [04-02]: Break timeout 35 seconds (Windows default per MS-SMB2)
- [04-02]: Scan interval 1 second for balance of responsiveness and efficiency
- [04-02]: Session tracking map for break notification routing
- [04-03]: 35-second lease break timeout matches Windows MS-SMB2 default
- [04-03]: Polling-based lease break wait with 100ms interval
- [04-03]: OplockChecker interface in MetadataService for clean cross-protocol visibility
- [05-01]: UnifiedLockView in pkg/metadata/ (not pkg/metadata/lock/) per CONTEXT.md
- [05-01]: Per-share UnifiedLockView ownership matches LockManager pattern
- [05-01]: NLM OH field = first 8 bytes of 16-byte LeaseKey
- [05-01]: Cross-protocol metrics use descriptive label constants
- [05-02]: Handle lease break proceeds even on timeout (Windows behavior)
- [05-02]: Break ALL leases to None for delete operations
- [05-02]: OplockChecker registered via SMB adapter SetRuntime
- [05-03]: NLM locks checked BEFORE SMB leases (explicit wins over opportunistic)
- [05-03]: STATUS_LOCK_NOT_GRANTED for byte-range conflicts, not STATUS_SHARING_VIOLATION
- [05-03]: CREATE succeeds even when lease denied (file opens, caching disabled)
- [05-03]: Handle-only leases do not conflict with NLM locks
- [05-04]: fcntl for byte-range locks (NLM), flock for whole-file advisory locks
- [05-04]: Platform-specific notes logging for macOS vs Linux lock behavior
- [05-04]: Grace period tests simulate behavior (full testing requires persistent stores)
- [06-01]: Tag stored as []byte to echo non-UTF-8 content faithfully per RFC 7530
- [06-01]: bitmap4 decode rejects >8 words to prevent memory exhaustion
- [06-01]: FH4_PERSISTENT for pseudo-fs handles (no expiration)
- [06-01]: DefaultLeaseTime 90 seconds matching Linux nfsd
- [06-01]: PseudoFSAttrSource interface decouples attrs from pseudo-fs implementation
- [06-02]: Streaming XDR decode via io.Reader cursor (no pre-parsing all COMPOUND ops)
- [06-02]: Unknown opcodes outside valid range (3-39) return OP_ILLEGAL, valid but unimplemented return NOTSUPP
- [06-02]: handleUnsupportedVersion takes low/high version range for PROG_MISMATCH
- [06-02]: PseudoFS handle format: pseudofs:path (SHA-256 hashed if > 128 bytes)
- [06-02]: First-use INFO logging with sync.Once for both v3 and v4 versions
- [06-03]: Copy-on-set for all FH assignments to prevent CurrentFH/SavedFH aliasing
- [06-03]: PUTPUBFH identical to PUTROOTFH per locked decision
- [06-03]: Export junction crossing uses runtime.GetRootHandle when registry available
- [06-03]: SETCLIENTID uses atomic counter for client ID (Phase 9 replaces)
- [06-03]: READDIR uses child index+1 as cookie values for pseudo-fs
- [07-01]: runtime.Runtime used directly in Handler (consistent with v3 pattern, no interface extraction)
- [07-01]: LOOKUPP cross-back via PseudoFS.FindJunction(shareName) at share root
- [07-01]: ACCESS uses Unix permission triad (owner/group/other) with root UID 0 bypass
- [07-01]: Real file FSID = (major=1, minor=SHA256(shareName)) to distinguish from pseudo-fs FSID (0,1)
- [07-01]: buildV4AuthContext is centralized auth context builder for all real-FS handlers
- [07-02]: Regular file creation via CREATE returns NFS4ERR_BADTYPE (files use OPEN per RFC 7530)
- [07-02]: Block/char/socket/FIFO return NFS4ERR_NOTSUPP (not in metadata layer)
- [07-02]: REMOVE tries RemoveFile first, falls back to RemoveDirectory on ErrIsDirectory
- [07-02]: OPEN/claim/delegation/stability constants added proactively for Plan 07-03
- [07-03]: Placeholder stateids for Phase 7: OPEN returns random stateid, all handlers accept any stateid
- [07-03]: WRITE always returns UNSTABLE4 stability to leverage cache+WAL for performance
- [07-03]: Server boot verifier uses time.Now().UnixNano() encoded as uint64 in 8 bytes
- [07-03]: EXCLUSIVE4 create mode treated as GUARDED4 in Phase 7 (consumes verifier from wire)
- [07-03]: OPEN always sets OPEN4_RESULT_CONFIRM flag (Phase 9 adds proper state tracking)
- [08-01]: Cross-share check uses DecodeFileHandle to compare share names before MetadataService call
- [08-01]: LINK checks only CurrentFH for pseudo-fs; RENAME checks both SavedFH and CurrentFH
- [08-01]: Auth context built from CurrentFH (target directory) for both LINK and RENAME
- [08-02]: NFS4StatusError interface for typed decode errors (ATTRNOTSUPP, INVAL, BADOWNER)
- [08-02]: Accept any special stateid in Phase 8 (Phase 9 validates)
- [08-02]: attrsset bitmap echoes requested bitmap on success (all-or-nothing semantics)
- [08-02]: Owner string parsing supports numeric@domain, bare numeric, and well-known names
- [08-03]: Byte-exact comparison via encode-then-compare for VERIFY/NVERIFY
- [08-03]: encodeAttrValsOnly reuses existing encode functions, strips bitmap to extract opaque data
- [08-03]: SECINFO returns AUTH_SYS first (strongest) then AUTH_NONE per RFC convention
- [08-03]: RELEASE_LOCKOWNER returns NFS4_OK (no-op) to prevent client errors during cleanup
- [08-03]: All stub handlers consume XDR args fully to prevent COMPOUND stream desync
- [09-01]: Variadic StateManager in NewHandler for backward compatibility with 50+ test call sites
- [09-01]: Single RWMutex for all StateManager state (avoids deadlocks per research)
- [09-01]: crypto/rand for confirm verifiers (not timestamps, per Pitfall 6)
- [09-01]: Case 5 (re-SETCLIENTID) creates new unconfirmed record with same client ID
- [09-01]: OpenOwner placeholder struct in state/client.go for Plan 09-02
- [09-02]: Stateid other field: type(1) + epoch_low24(3) + counter(8) for uniqueness across types and restarts
- [09-02]: Special stateids (all-zeros, all-ones) bypass validation and CloseFile for backward compatibility
- [09-02]: OpenOwner keyed by composite string (clientID:hex(ownerData)) for map efficiency
- [09-02]: SeqID wrap-around at 0xFFFFFFFF goes to 1 (not 0) per RFC 7530 since 0 is reserved
- [09-02]: NFS4StateError carries NFS4 status code for direct handler mapping
- [09-03]: LeaseState.mu separate from StateManager.mu to prevent timer callback deadlock
- [09-03]: Implicit lease renewal in ValidateStateid prevents READ-only client expiry
- [09-03]: attrs.SetLeaseTime package-level setter for dynamic FATTR4_LEASE_TIME encoding
- [09-03]: NFS4ERR_OPENMODE returned for WRITE on read-only open state
- [09-03]: onLeaseExpired cascading cleanup: openStates -> openOwners -> client record
- [09-04]: Grace period check before sm.mu acquisition to avoid holding main lock during grace check
- [09-04]: CLAIM_PREVIOUS uses currentFH as the file being reclaimed (no filename decode needed)
- [09-04]: Empty expectedClientIDs skips grace period entirely (no timer started)
- [09-04]: ClientSnapshot struct for shutdown serialization (in-memory persistence for Phase 9)
- [09-04]: CLAIM_DELEGATE_CUR/PREV consume XDR args to prevent COMPOUND stream desync
- [10-01]: Lock stateid seqid and open-owner seqid only advance on success (denied does not consume)
- [10-01]: Lock-owner OwnerID format nfs4:{clientid}:{owner_hex} for cross-protocol detection
- [10-01]: READW_LT/WRITEW_LT are non-blocking hints; server returns NFS4ERR_DENIED immediately
- [10-01]: One LockState per (LockOwner, OpenState) pair, referenced by stateid other field
- [10-02]: LOCKT is purely stateless: no lock-owners, stateids, or maps modified
- [10-02]: LOCKU treats lock-not-found from lock manager as success (idempotent)
- [10-02]: Lock state persists after LOCKU; RELEASE_LOCKOWNER handles cleanup
- [10-02]: parseConflictOwner parses nfs4:{clientid}:{owner_hex} with graceful fallback
- [10-03]: LockStates typed as []*LockState for compile-time safety (was []interface{})
- [10-03]: CLOSE requires LOCKU + RELEASE_LOCKOWNER before accepting (NFS4ERR_LOCKS_HELD)
- [10-03]: Lease expiry iterates OpenState.LockStates for cascading lock cleanup
- [10-03]: RELEASE_LOCKOWNER unknown owner is no-op (NFS4_OK) per RFC 7530
- [11-01]: Idempotent DELEGRETURN: current-epoch not-found returns NFS4_OK per Pitfall 3
- [11-01]: delegByFile keyed by string(fileHandle) for O(1) conflict lookup
- [11-01]: Delegation cleanup after lock/open cleanup in onLeaseExpired for consistent ordering
- [11-01]: countOpensOnFile scans openStateByOther (adequate for current scale)
- [11-02]: CB_NULL uses readAndDiscardCBReply; CB_RECALL uses readAndValidateCBReply with NFS4 status check
- [11-02]: Universal address parsing splits from the right using LastIndex for IPv6 safety
- [11-02]: 5-second total timeout (CBCallbackTimeout) covers both dial and I/O, matching NLM pattern
- [11-02]: callback_ident in CB_COMPOUND set to 0 (client identifies via program number)
- [11-03]: Simple delegation policy: grant only when exclusive access, callback, no existing delegations
- [11-03]: Async CB_RECALL via goroutine; RLock to read callback info, release before TCP call
- [11-03]: NFS4ERR_DELAY on delegation conflict per RFC 7530 recommendation (client retries)
- [11-03]: CLAIM_DELEGATE_PREV returns NFS4ERR_NOTSUPP (requires persistent delegation state)
- [11-03]: DELEGPURGE returns NFS4ERR_NOTSUPP (no CLAIM_DELEGATE_PREV support)
- [11-03]: Variadic deleg param in encodeOpenResult for backward compatibility
- [11-04]: Recall timer fires after lease duration since CB_RECALL per RFC 7530 Section 10.4.6
- [11-04]: Short 5s revocation timer when CB_RECALL fails (callback path known-down)
- [11-04]: CBPathUp verified via CB_NULL replaces simple callback address check
- [11-04]: Recently-recalled TTL = 30s (~1/3 lease duration) prevents grant-recall storms
- [11-04]: Revoked delegations kept in delegByOther for NFS4ERR_BAD_STATEID detection
- [11-04]: DELEGRETURN of revoked delegation returns NFS4_OK (graceful cleanup)
- [12-01]: KerberosConfig in pkg/config (not pkg/auth/kerberos) to avoid circular imports
- [12-01]: StaticMapper as initial identity mapping strategy with DefaultUID/GID=65534
- [12-01]: Env var overrides: DITTOFS_KERBEROS_KEYTAB_PATH, SERVICE_PRINCIPAL, KRB5CONF
- [12-01]: SeqWindow uses bitmap ([]uint64) for O(1) duplicate detection
- [12-01]: Sequence number 0 rejected (not valid in RPCSEC_GSS)
- [12-02]: Verifier interface abstracts AP-REQ verification for testability (mock in tests, gokrb5 in production)
- [12-02]: Store-before-reply ordering enforced: context stored BEFORE INIT reply is built
- [12-02]: sync.Map for context store: O(1) lookup optimized for high-read/low-write pattern
- [12-02]: Background cleanup every 5 minutes with configurable TTL for idle context expiration
- [12-02]: AP-REP token left empty (gokrb5 does not expose AP-REP building); documented limitation
- [12-02]: GSSProcessResult carries both control replies and data results in single type
- [12-02]: DATA handling stubbed with explicit error for Plan 03
- [12-03]: context.Value pattern for GSS identity threading (no handler signature changes)
- [12-03]: GSSSessionInfo carries session key + seq_num for reply verifier via context.Value
- [12-03]: SetKerberosConfig as pre-SetRuntime method (avoids changing NFSAdapter constructor)
- [12-03]: AUTH_NULL verifier for INIT/DESTROY; GSS MIC verifier for DATA replies
- [12-03]: Silent discard returns nil from handleRPCCall (no reply written to connection)

- [12-04]: gokrb5 WrapToken provides integrity (HMAC) only, not actual encryption; documented limitation
- [12-04]: Separate key usage for initiator (23/24) vs acceptor (25/26) per RFC 4121
- [12-04]: KerberosEnabled bool on v4 Handler struct (simplest, no config dependency)
- [12-04]: KRB5 OID as full DER (tag+length+value) in sec_oid4 per RFC 7530
- [12-04]: SECINFO order: krb5p > krb5i > krb5 > AUTH_SYS > AUTH_NONE (most secure first)

- [12-05]: Polling (60s) over fsnotify for keytab hot-reload (more reliable for atomic file replacement)
- [12-05]: DITTOFS_KERBEROS_KEYTAB and DITTOFS_KERBEROS_PRINCIPAL as primary env vars (legacy also supported)
- [12-05]: WithMetrics functional option pattern for GSSProcessor (avoids breaking existing constructor calls)
- [12-05]: GSSMetrics nil-safe methods for zero-overhead when metrics disabled

- [13-01]: Zero requestedMask returns true (vacuously allowed) before nil/empty ACL check
- [13-01]: AUDIT/ALARM ACEs stored only, skipped during evaluation per project decision
- [13-01]: Canonical ordering uses four-bucket system with AUDIT/ALARM anywhere
- [13-01]: DeriveMode considers only ALLOW ACEs of special identifiers
- [13-01]: AdjustACLForMode preserves non-rwx mask bits (READ_ACL, WRITE_ACL, DELETE, SYNCHRONIZE)
- [13-01]: PropagateACL replaces inherited ACEs, preserves explicit in canonical order

- [13-02]: StaticMapper always returns Found=true (falls back to defaults for unknown principals)
- [13-02]: CachedMapper caches errors to prevent thundering herd on infrastructure failures
- [13-02]: pkg/identity has zero external imports (stdlib only) -- no circular dependency risk
- [13-02]: ParsePrincipal splits on last @ for user@host@REALM safety
- [13-02]: ConventionMapper numeric UID uses same value for default GID
- [13-02]: Backward compat via wrapper delegation, not type alias

- [13-03]: ACL field nil=Unix mode, non-nil=ACL evaluation, empty=deny all
- [13-03]: ACL evaluation takes precedence over Unix mode in calculatePermissions
- [13-03]: Root (UID 0) bypasses ACL checks, matching Unix permission model
- [13-03]: PostgreSQL stores ACL as JSONB with partial index for presence queries
- [13-03]: Memory/BadgerDB get ACL support automatically via JSON serialization
- [13-03]: IdentityMapping uses GORM model with principal uniqueIndex

- [13-04]: FATTR4_ACL/ACLSUPPORT constants in attrs package alongside other FATTR4 constants
- [13-04]: Pseudo-fs nodes encode 0 ACEs (no ACL on virtual namespace)
- [13-04]: SetIdentityMapper uses package-level variable (same pattern as SetLeaseTime)
- [13-04]: Group reverse resolution not implemented (only owner uses mapper; group falls back to numeric)
- [13-04]: ACL validation at XDR decode time via acl.ValidateACL before reaching MetadataService
- [13-04]: badXDRError (NFS4ERR_BADXDR) and invalidACLError (NFS4ERR_INVAL) as NFS4StatusError types
- [Phase 15]: Log approach for delegation observability since Prometheus metrics not yet instrumented
- [Phase 15]: Two mount points for cross-client NFS conflict simulation (different open-owners)

### Plan 13-05: SMB Security Descriptor and Control Plane Integration - COMPLETE
- SMB Security Descriptor encoding/decoding per MS-DTYP with self-relative format
- SID types with encode/decode, FormatSID/ParseSIDString
- PrincipalToSID/SIDToPrincipal bidirectional identity translation
- Well-known SID mapping (EVERYONE@, OWNER@, GROUP@)
- QUERY_INFO returns real Security Descriptor with Owner/Group/DACL from file ACL
- SET_INFO parses Security Descriptor and applies ACL changes
- Identity mapping REST API handlers (List, Create, Delete) with admin auth
- API client methods for identity mapping CRUD
- dittofsctl idmap add/list/remove CLI commands
- ACL Prometheus metrics (5 metrics, nil-safe, sync.Once singleton)

- [13-05]: DittoFS user SID format S-1-5-21-0-0-0-{UID/GID} for local identity mapping
- [13-05]: NFSv4 ACE mask bits identical to Windows ACCESS_MASK (no translation needed per RFC 7530)
- [13-05]: Well-known SID bidirectional mapping (EVERYONE@ <-> S-1-1-0, OWNER@ <-> S-1-3-0, GROUP@ <-> S-1-3-1)
- [13-05]: Security Descriptor always self-relative format with SE_SELF_RELATIVE|SE_DACL_PRESENT control flags
- [13-05]: Identity mapping API at /identity-mappings with RequireAdmin middleware
- [13-05]: ACL metrics nil-safe singleton pattern matching GSSMetrics from Phase 12
- [13-05]: additionalSecInfo bitmask controls SD section inclusion (OWNER/GROUP/DACL)

- [14-01]: Version counter (monotonic int) for settings change detection instead of timestamps
- [14-01]: Post-migrate SQL UPDATE to fix GORM zero-value boolean trap for existing shares
- [14-01]: EnsureAdapterSettings called in New() to auto-populate defaults for existing adapters
- [14-01]: Netgroup deletion checks share FK reference count (ErrNetgroupInUse)
- [14-01]: Share security fields added directly to Share struct (not separate table)

- [14-02]: ValidationErrorResponse with per-field errors map follows RFC 7807 (status 422)
- [14-02]: force=true bypasses range validation with logger.Warn audit trail
- [14-02]: dry_run=true validates and returns would-be result without persisting
- [14-02]: Per-field reset via ?setting= query param on POST .../reset endpoint
- [14-02]: Blocked operations validated against known NFS/SMB operation name lists
- [14-02]: SettingsOption functional pattern for API client (WithForce, WithDryRun)

- [14-03]: SettingsWatcher non-fatal on LoadInitial (adapters may not exist at startup)
- [14-03]: DNS cache piggybacked cleanup on lookups (no separate goroutine)
- [14-03]: NetgroupID stored as name in runtime Share (resolved from DB ID during loading)
- [14-03]: Empty netgroup allowlist = allow all; netgroup with no members = deny all
- [14-03]: Settings watcher stops before adapters in shutdown sequence

- [14-04]: Operation blocklist returns NFS4ERR_NOTSUPP (not NFS4ERR_PERM) per locked decision
- [14-04]: Security policy checked at mount handler for NFSv3; COMPOUND for NFSv4 after PUTFH
- [14-04]: Netgroup check is fail-closed: on error, deny access
- [14-04]: SMB operation blocklist is advisory-only with logging (SMB lacks per-op COMPOUND granularity)
- [14-04]: SMB encryption is a stub that logs warning per locked decision
- [14-04]: Delegation policy check is Check 0 in ShouldGrantDelegation for early short-circuit

- [14-05]: Settings command uses nfs/smb as cobra subcommands rather than positional arg
- [14-05]: Separate flag variables for NFS and SMB update commands to avoid cobra duplicate flag errors
- [14-05]: Client-side IP/CIDR/hostname validation before API call for fast feedback
- [14-05]: PersistentPreRunE on settings adapter subcommands to propagate global flags

- [14-06]: Test hostname matching by pre-populating DNS cache (no net.LookupAddr mock needed)
- [14-06]: Runtime tests inject shares directly via addShareDirect to isolate netgroup access logic

- [14-07]: E2E tests use apiclient directly rather than CLI runner for precise API-level assertions
- [14-07]: NFS mount verification not duplicated in lifecycle test (covered by store_matrix_test.go)
- [14-07]: Delegation grant/deny not observable from client side; test covers API persistence only

- [15-01]: MountNFSExportWithVersion as core; MountNFSWithVersion delegates with "/export" default
- [15-01]: NFSv4 mount uses vers=4.0 without mountport or nolock (stateful protocol)
- [15-01]: setupNFSv4TestServer helper encapsulates server lifecycle for DRY test setup
- [15-01]: Version-parameterized subtests for ver in ["3", "4.0"] with SkipIfNFSv4Unsupported guard
- [15-01]: Pseudo-FS browsing test mounts "/" root to verify share junctions

- [15-03]: Local krbV4Mount type instead of framework.KerberosMount to avoid unexported field access
- [15-03]: All Kerberos v4 mounts use vers=4.0 explicitly (never vers=4) per locked decision #5
- [15-03]: Cross-protocol ACL test uses MountSMBWithError for graceful skip on SMB unavailability
- [15-03]: nfs4SetACL/nfs4AddACE/nfs4GetACL CLI wrappers for E2E ACL manipulation

- [15-04]: Reuse storeMatrix variable from store_matrix_test.go (same package, no duplication)
- [15-04]: isNFSv4SkippedPlatform() helper to programmatically build version lists
- [15-04]: TestStaleNFSHandle expects ENOENT (not ESTALE) because unmount+remount = fresh LOOKUP
- [15-04]: TestClientReconnection tolerates ENOENT for memory backends (adapter restart loses state)
- [15-04]: Concurrent write test uses sync.WaitGroup + goroutines with SHA-256 checksums

- [15-02]: Log approach (fallback) for delegation observability since Prometheus metrics not yet instrumented
- [15-02]: Two mount points per test for cross-client NFS conflict simulation (different open-owners)
- [15-02]: POSIX fcntl locks (F_SETLK/F_SETLKW) work for both v3 (NLM) and v4 (integrated locking)
- [15-02]: Graceful handling of same-process POSIX lock semantics (per-process, not per-fd)
- [15-02]: Delegation tests NFSv4-only; locking tests parameterized for v3+v4

- [15-05]: NFSv4 mount uses vers=4.0 without mountport or nolock for POSIX tests
- [15-05]: known_failures_v4.txt inherits common v3 failures, removes locking (NFSv4 has integrated)
- [15-05]: Stress tests use //go:build e2e && stress dual tag for CI-excluded execution
- [15-05]: run-e2e.sh uses -coverprofile=coverage-e2e.out for E2E coverage profiling
- [15-05]: Netgroup E2E test handles both per-operation and mount-time-only access patterns

### Pending Todos

None.

### Blockers/Concerns

None.

## Next Steps

**Phase 15 — COMPLETE (5/5 plans)**
- Plan 15-01 COMPLETE: NFSv4 E2E framework (mount helpers, platform skips, 8 basic operation test functions)
- Plan 15-02 COMPLETE: NFSv4 locking E2E tests
- Plan 15-03 COMPLETE: NFSv4 delegations E2E tests
- Plan 15-04 COMPLETE: Store matrix, recovery, and concurrency E2E tests
- Plan 15-05 COMPLETE: POSIX NFSv4, control plane mount-level, stress tests, E2E coverage

**Ready for Phase 16**

## Session Continuity

Last session: 2026-02-17
Stopped at: Completed 15-05-PLAN.md (Phase 15 COMPLETE)
Resume file: Phase 16 planning
