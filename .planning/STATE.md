---
gsd_state_version: 1.0
milestone: v3.8
milestone_name: SMB3 Protocol Upgrade
status: phase-complete
last_updated: "2026-03-02T14:42:17.761Z"
progress:
  total_phases: 39
  completed_phases: 39
  total_plans: 134
  completed_plans: 134
---

# Project State

## Project Reference

See: .planning/PROJECT.md (updated 2026-02-28)

**Core value:** Enterprise-grade multi-protocol file access with unified locking, Kerberos authentication, and session reliability
**Current focus:** v3.8 SMB3 Protocol Upgrade — Phase 38 (Durable Handles)

## Current Position

Phase: 38 of 40 (Durable Handles)
Plan: 3 of 3 complete
Status: Phase 38 Complete
Last activity: 2026-03-02 — Completed 38-02 (Durable handle CREATE context processing)

Progress: [##########] 100%

## Completed Milestones

| Milestone | Phases | Plans | Duration | Shipped |
|-----------|--------|-------|----------|---------|
| v1.0 NLM + Unified Locking | 1-5 | 19 | Feb 1-7, 2026 | 2026-02-07 |
| v2.0 NFSv4.0 + Kerberos | 6-15 | 42 | Feb 7-20, 2026 | 2026-02-20 |
| v3.0 NFSv4.1 Sessions | 16-25 | 25 | Feb 20-25, 2026 | 2026-02-25 |
| v3.5 Adapter + Core Refactoring | 26-29.4 | 22 | Feb 25-26, 2026 | 2026-02-26 |
| v3.6 Windows Compatibility | 29.8-32 | 12 | Feb 26-28, 2026 | 2026-02-28 |

## Performance Metrics

**Velocity:**
- Total plans completed: 136 (19 v1.0 + 42 v2.0 + 25 v3.0 + 22 v3.5 + 12 v3.6 + 4 inserted + 12 v3.8)
- 5 milestones in 29 days
- Average: ~4.7 plans/day

| Phase | Plan | Duration | Tasks | Files |
|-------|------|----------|-------|-------|
| 33    | 01   | 9min     | 2     | 12    |
| 33    | 02   | 13min    | 2     | 10    |
| 33    | 03   | 45min    | 2     | 29    |
| 34    | 01   | 13min    | 2     | 13    |
| 34    | 02   | 10min    | 2     | 16    |
| 35    | 01   | 7min     | 2     | 11    |
| 35    | 02   | 9min     | 2     | 12    |
| 35    | 03   | 12min    | 2     | 9     |
| 36    | 01   | 7min     | 2     | 8     |
| 36    | 02   | 10min    | 2     | 8     |
| 36    | 03   | 8min     | 2     | 7     |
| 37    | 01   | 9min     | 2     | 10    |
| 37    | 02   | 11min    | 2     | 9     |
| 37    | 03   | 8min     | 2     | 7     |
| 38    | 01   | 7min     | 2     | 11    |
| 38    | 02   | 16min    | 1     | 4     |
| 38    | 03   | 10min    | 2     | 6     |

## Accumulated Context

### Decisions

- v3.8: Business logic (leases, durable handles, state) in metadata service layer, not SMB internal package
- v3.8: SMB internal package = protocol encoding/decoding/framing only
- v3.8: Reuse NFSv4 infrastructure (delegations, state management, Kerberos) for SMB3
- v3.8: Shared Kerberos layer for SMB3 via existing RPCSEC_GSS infrastructure
- v3.8: Dependency order — negotiate -> KDF/signing -> encryption -> Kerberos -> leases -> durable handles -> cross-protocol -> testing
- 4 TODO(plan-03) cross-protocol oplock break markers from v3.5 (to resolve in v3.8)
- REF-01.8/REF-01.9 adapter translation layers deferred to v3.8
- 33-01: smbenc uses buffer-based pattern with error accumulation (not streaming io.Reader)
- 33-01: ConnectionCryptoState placed in internal/adapter/smb to avoid circular imports
- 33-01: CryptoState created eagerly for all connections (minimal overhead, simpler code path)
- 33-02: CryptoState interface in handlers/ to break circular imports with smb/ package
- 33-02: Dispatch hooks pattern (before/after per command) for cross-cutting concerns like preauth hash
- 33-02: Server cipher preference: AES-128-GCM > AES-128-CCM > AES-256-GCM > AES-256-CCM
- 33-02: DropConnection on HandlerResult for fatal protocol violations requiring TCP close
- 33-03: Map-based IOCTL dispatch table (IOCTLHandler func type) mirrors command dispatch pattern
- 33-03: VALIDATE_NEGOTIATE_INFO reads all 4 fields from CryptoState, never re-computes
- 33-03: 3.1.1 connections drop TCP on VNEG per MS-SMB2 3.3.5.15.12
- 33-03: All SMB handler binary encoding goes through smbenc codec (ARCH-02 enforced)
- [Phase 34]: CMAC in signing/ package (not standalone cmac/) for cohesion with other signers
- [Phase 34]: SessionSigningState kept temporarily with Signer + legacy SigningKey for minimal blast radius
- [Phase 34]: Signer interface pattern: Sign([16]byte) + Verify(bool) for polymorphic SMB signing
- [Phase 34-02]: SessionCryptoState holds all 4 keys upfront (even encryption/decryption for Phase 35)
- [Phase 34-02]: DeriveAllKeys dispatches by dialect: <3.0 direct HMAC, >=3.0 full KDF
- [Phase 34-02]: Default signing preference: GMAC > CMAC > HMAC-SHA256 (configurable via adapter settings)
- [Phase 34-02]: 3.1.1 clients omitting SIGNING_CAPABILITIES default to AES-128-CMAC per spec
- [Phase 35-01]: Vendored CCM from pion/dtls (MIT) to avoid 50+ transitive dependencies
- [Phase 35-01]: Encryptor interface: Encrypt(plaintext, aad) -> (nonce, ciphertext, err) mirrors Signer pattern
- [Phase 35-01]: Default cipher preference: AES-256-GCM > AES-256-CCM > AES-128-GCM > AES-128-CCM (256-bit first)
- [Phase 35-02]: EncryptWithNonce added to Encryptor interface (nonce-before-AAD requirement)
- [Phase 35-02]: EncryptableSession interface decouples middleware from session.Session (no circular imports)
- [Phase 35-02]: Encrypted sessions bypass signing entirely per MS-SMB2 3.3.4.1.1 (AEAD provides integrity)
- [Phase 35-02]: Consecutive decryption failures tracked per-connection (5 failures = disconnect)
- [Phase 35-03]: EncryptionConfig struct duplicated in handlers/ to avoid circular imports (mirrors SigningConfig pattern)
- [Phase 35-03]: shouldRejectUnencryptedTreeConnect only enforces in required mode (preferred allows mixed)
- [Phase 35-03]: Live SMB settings can upgrade encryption_mode from disabled to preferred at runtime
- [Phase 35-03]: buildAuthenticatedResponse takes encryptData bool for SessionFlagEncryptData
- [Phase 36-01]: BuildMutualAuth returns raw AP-REP (APPLICATION 15), not GSS-wrapped; protocol adapters add their own framing
- [Phase 36-01]: ReplayCache keyed by 4-tuple (principal, ctime, cusec, servicePrincipal) for cross-protocol dedup
- [Phase 36-01]: HasSubkey exported as package-level function for reuse by NFS GSS and SMB auth
- [Phase 36-01]: Shared auth service pattern: protocol-agnostic core in internal/auth/, protocol framing in adapter packages
- [Phase 36-02]: Session key normalized to 16 bytes via copy() (truncate >16, zero-pad <16) per MS-SMB2 3.3.5.5.3
- [Phase 36-02]: MIC computation uses key usage 23 (acceptor sign); verification uses 25 (initiator sign) per RFC 4121
- [Phase 36-02]: Client Kerberos OID echoed in SPNEGO response (MS OID preferred for Windows SSPI)
- [Phase 36-02]: Valid Kerberos ticket from unknown principal = hard failure (not guest), security decision
- [Phase 36-02]: Server mechListMIC uses full session key (not normalized 16-byte key) per RFC 4178
- [Phase 36-03]: Kerberos failure returns SPNEGO reject (NegState=reject) so client retries with fresh SessionId=0 for NTLM
- [Phase 36-03]: Guest sessions gated by GuestEnabled AND signing.required (no session key = no signing)
- [Phase 36-03]: NEGOTIATE SecurityBuffer contains SPNEGO NegTokenInit advertising available auth mechanisms
- [Phase 36-03]: NTLM disable check early in SessionSetup, before message type dispatch
- [Phase 36-03]: SetKerberosProvider auto-creates KerberosService and IdentityConfig (strip-realm default)
- [Phase 37-01]: advanceEpoch helper centralizes all epoch increments for monotonicity
- [Phase 37-01]: Recently-broken cache uses 5s TTL to prevent directory lease grant-break storms
- [Phase 37-01]: Cross-key conflicts break to LeaseStateNone (simplest correct behavior per MS-SMB2)
- [Phase 37-01]: Lease upgrade whitelist: R->RW, R->RH, R->RWH, RH->RWH, RW->RWH
- [Phase 37-02]: LockManagerResolver interface pattern for per-share LockManager resolution at request time
- [Phase 37-02]: metadataServiceResolver bridges MetadataService to lease package (uses DecodeFileHandle)
- [Phase 37-02]: Surviving oplock wire-format types moved to oplock_constants.go (CREATE response uses OplockLevel)
- [Phase 37-02]: Traditional oplock code paths fully removed (not just disabled)
- [Phase 37-03]: Auto-wire LockManager as DirChangeNotifier in RegisterStoreForShare
- [Phase 37-03]: ctx.ClientAddr used as originClientID (AuthContext has no Identity.ClientID)
- [Phase 37-03]: setattr retains direct NotifyDirChange (NFS4-specific, not in DirChangeType enum)
- [Phase 37-03]: NFS4 delegation recall for removed dirs kept as direct StateManager call (cleanup, not notification)
- [Phase 38-01]: DurableHandleStore follows ClientRegistrationStore sub-interface pattern exactly
- [Phase 38-01]: Memory store uses linear scans for secondary lookups (acceptable for low handle counts)
- [Phase 38-01]: BadgerDB uses hex-encoded composite keys for multi-value indices (dh:appid:{hex}:{id})
- [Phase 38-01]: PostgreSQL uses SQL interval arithmetic for server-side expired handle cleanup
- [Phase 38-01]: Optional [16]byte fields stored as NULL in PostgreSQL when zero-value
- [Phase 38-02]: V2 (DH2Q) takes precedence over V1 (DHnQ) when both present per MS-SMB2
- [Phase 38-02]: V1 requires batch oplock for grant; V2 has no oplock requirement
- [Phase 38-02]: Reconnect early-exit at Step 4b in CREATE handler avoids unnecessary file operations
- [Phase 38-02]: Session key hash = SHA-256 of session signing key for reconnect security
- [Phase 38-02]: DurableTimeoutMs defaults to 60000ms (60 seconds) in handler constructor
- [Phase 38-02]: IsDurable NOT set on restored handle -- client must re-request after reconnect
- [Phase 38-03]: Scavenger iterates all handles client-side (not bulk delete) to perform cleanup before deletion
- [Phase 38-03]: Local durableHandleStoreProvider interface avoids importing storetest from production code
- [Phase 38-03]: Scavenger lifecycle tied to Serve context -- stops automatically on adapter shutdown

### Pending Todos

None.

### Blockers/Concerns

None.

## Session Continuity

Last session: 2026-03-02
Stopped at: Completed 38-02-PLAN.md (Durable handle CREATE context processing — Phase 38 complete)
Resume file: None
