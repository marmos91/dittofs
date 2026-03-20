# Project Research Summary

**Project:** DittoFS v0.10.0 - Production Hardening + SMB Protocol Fixes
**Domain:** Production filesystem server hardening and SMB3 protocol completeness
**Researched:** 2026-03-20
**Confidence:** HIGH

## Executive Summary

v0.10.0 represents a production hardening milestone that requires **zero new external dependencies**. All eight features (SMB credit flow control, multi-channel session binding, share quotas, payload stats, client tracking, trash/soft-delete, macOS signing fix, and WPTS conformance fixes) can be implemented using Go's standard library and the existing dependency set. The project already has the foundational infrastructure (session management, crypto state, metadata store interfaces, control plane models, WPTS Docker infrastructure) and this milestone is about hardening, fixing edge cases, and filling protocol gaps.

The features span three architectural domains: (1) SMB protocol completeness (credits, multi-channel, signing fix, WPTS), (2) metadata-layer features (quotas, payload stats, trash), and (3) operational features (client tracking). The **MetadataService** is the natural hub for quotas, trash, and payload stats, while the **SMB session/connection pipeline** absorbs credit flow and multi-channel. Client tracking requires a new cross-cutting service in the runtime. Eight features, three dependency chains, no circular dependencies.

The critical risk is **multi-channel session binding**, which requires fundamental architectural changes to the connection-per-session model and has documented data corruption risks from Samba's implementation history. The macOS signing fix is the highest-priority standalone item as it unblocks an entire platform. Credit flow control is low-effort protocol compliance that improves the baseline for all subsequent WPTS work. The recommended build order prioritizes features that unblock others while deferring the highest-risk items (multi-channel, trash) to later phases after solid test coverage exists.

## Key Findings

### Recommended Stack

**Core finding:** Zero new dependencies required. Every feature uses existing Go stdlib packages and the current dependency set (Go 1.25, BadgerDB 4.5.2, GORM 1.31.1, Cobra 1.8.1). Total estimated new code: ~2500-3500 LOC across 8 features.

**Core technologies:**
- **Go stdlib crypto** (SHA-512, AES-CMAC/GMAC): Already used in SMB signing infrastructure; macOS fix requires debugging existing hash chain, not new primitives
- **GORM**: Auto-migrate handles new quota/trash columns on Share model; no schema migration framework needed
- **Existing session/connection infrastructure**: Credit flow and multi-channel extend existing `session.Manager` and `Connection` types
- **Existing metadata service**: Quotas, trash, and payload stats are pure extensions of the metadata layer interface

**Critical anti-recommendations:**
- No external quota library (Go syscall.Statfs already used; quotas are metadata-layer)
- No fsnotify for ChangeNotify (DittoFS metadata is in-memory/BadgerDB, not a real filesystem)
- No new testing framework for WPTS (Docker-based infrastructure already exists from v3.6/v3.8)
- No SMB compression libraries (69 WPTS tests skipped require compression; defer to v4.5 BlockStore compression)

### Expected Features

**Table stakes (cannot ship without):**
- **Share quotas with FSSTAT/FSINFO/SMB reporting**: Every production NAS enforces storage limits. Without quotas, one share consumes all storage. `df` on NFS and Explorer free space on SMB must show quota-limited values, not raw disk capacity.
- **Payload stats (UsedSize)**: Currently `UsedSize` in BlockStore.Stats is zero or inaccurate. Quota enforcement and filesystem statistics depend on accurate storage consumption tracking.
- **SMB credit flow control**: MS-SMB2 requires credit charge validation and sequence number tracking. Without enforcement, clients can exhaust server resources (DoS vector).
- **SMB 3.1.1 signing on macOS**: Known bug (#252) where macOS clients reject signatures due to preauth integrity hash mismatch. Blocks an entire platform.
- **Client tracking**: Admins need unified view of connected NFS/SMB clients. `dfsctl client list` must show all protocols in a single table.
- **WPTS conformance fixes**: 73 known failures need reduction. Primary target is ChangeNotify (20 failures) where infrastructure exists but dispatch handler returns NOT_IMPLEMENTED.

**Should have (differentiators):**
- **Trash/soft-delete with configurable retention**: Server-side trash protects against accidental deletion. Unlike client-side recycle bins, works across all protocols. Most NAS vendors offer this (Synology @Recycle, QNAP Network Recycle Bin).
- **SMB multi-channel (session binding)**: Allows single session across multiple TCP connections for aggregate bandwidth and fault tolerance. Complex feature with data corruption risks; consider experimental/opt-in.

**Defer (anti-features for v0.10.0):**
- User/group quotas (per-user limits): Massively complex, requires tracking per-user usage across shares. Share quotas cover 90% of use cases.
- Client-side recycle bin integration: Windows `$RECYCLE.BIN` requires SID management. Server-side trash is simpler and protocol-agnostic.
- SMB compression (LZ77, LZNT1): 69 WPTS tests skipped due to compression. Large effort for marginal benefit on modern networks.
- SMB persistent handles: Different from durable handles (already implemented). Requires serialization to disk and recovery on restart.
- Full SMB multi-channel in v0.10.0: Session binding requires architectural changes. Implement IOCTL interface advertisement only; full multi-channel in future milestone.

### Architecture Approach

Features integrate into existing architecture without redesign. The **MetadataService** becomes the hub for quota enforcement (PrepareWrite checks quota before allowing writes), trash logic (RemoveFile checks share config and routes to soft-delete or hard-delete), and payload stats (GetFilesystemStatistics returns quota-adjusted values). The **SMB session/connection pipeline** absorbs credit flow control (pre-dispatch validation in ProcessSingleRequest) and multi-channel (session binding via SMB2_SESSION_FLAG_BINDING in SESSION_SETUP handler). Client tracking requires a new **ClientRegistry** service in the runtime that aggregates NFS mount tracking and SMB session tracking into protocol-agnostic ClientRecord objects.

**Major components:**
1. **MetadataService** (`pkg/metadata/service.go`) — Quota enforcement in PrepareWrite/CreateFile, trash redirect in RemoveFile, quota-aware GetFilesystemStatistics
2. **SMB session pipeline** (`internal/adapter/smb/`) — Credit charge validation in dispatch layer, session binding in session_setup.go, per-channel signing keys
3. **ClientRegistry** (NEW: `pkg/controlplane/runtime/clients/`) — Protocol-agnostic client tracking aggregating NFS mounts and SMB sessions
4. **Control plane models** (`pkg/controlplane/models/share.go`) — Add QuotaBytes, QuotaFiles, TrashEnabled, TrashRetentionDays columns
5. **BlockStore engine** (`pkg/blockstore/engine/`) — Fix UsedSize tracking (currently returns 0)

**Critical patterns:**
- **Quota enforcement at service boundary**: Enforce in MetadataService, not protocol handlers. Handlers translate domain errors to protocol-specific codes (NFS3ERR_NOSPC, STATUS_DISK_FULL).
- **Trash as metadata-only operation**: Trash moves files in metadata namespace only. Block data stays in place. Trash expiry triggers real deletion which eventually triggers block GC.
- **Dispatch hook for credit validation**: Add credit charge validation as pre-dispatch step in ProcessSingleRequest, before handler is called. Mirrors existing signing verification hook pattern.
- **Multi-channel via session binding flag**: Detect multi-channel in SESSION_SETUP via `Flags & 0x01`. New connection joins existing session rather than creating new one.

### Critical Pitfalls

1. **SMB credit grant drops to zero — client deadlock**: MS-SMB2 states "server MUST ensure credits held by client never reduced to zero." A server that grants zero credits permanently deadlocks that client session. Current adaptive algorithm could theoretically drive result below MinGrant. **Prevention**: Never return 0 from any credit grant path; add final safety floor `if grant == 0 { grant = 1 }`.

2. **Credit charge validation missing — allows resource exhaustion**: DittoFS calculates `CalculateCreditCharge` but never validates incoming requests. Without enforcement, malicious clients can send unlimited concurrent requests with `CreditCharge=0`, exhausting server resources. **Prevention**: Validate CreditCharge against payload before dispatch; verify `session.Outstanding >= creditCharge` before consuming credits.

3. **Multi-channel session binding race with session state**: Session state (tree connects, open files, leases, signing keys) must be shared across all connections. If session binding and concurrent operation on original connection race, server observes partially initialized state. Per Samba 4.4.0 release notes: "corner cases in treatment of channel failures that may result in data corruption when race conditions hit." **Prevention**: Session-level mutex for binding; session-connection registry becomes multi-value map; connection failure isolation (one channel fails ≠ tear down session).

4. **macOS preauth integrity hash mismatch — platform-specific byte ordering**: Existing preauth hash chain validated against Windows 11. macOS SMB client may produce different NEGOTIATE bytes due to different context ordering. If server modifies or reserializes bytes between receiving NEGOTIATE and hashing it, hash diverges. **Prevention**: Hash ORIGINAL wire bytes, not reconstructed bytes; modify ReadRequest to return raw message bytes alongside parsed header/body.

5. **WPTS conformance fixes regress existing passing tests**: Fixing one known failure can cause previously passing test to fail. Current 193 passing tests share server state (session table, tree connects, leases). **Prevention**: Run FULL WPTS suite after every change; categorize known failures by root cause, not test name; use git bisect for regressions; isolate server state between test categories if feasible.

## Implications for Roadmap

Based on research, suggested phase structure with dependency-aware ordering:

### Phase 1: SMB Protocol Foundation
**Rationale:** macOS signing fix unblocks entire platform (highest priority standalone item). Credit flow control improves protocol compliance baseline for all subsequent WPTS work. Both are foundational for later SMB features.

**Delivers:**
- macOS SMB 3.1.1 signing support (fix preauth integrity hash)
- SMB credit charge validation and enforcement
- Foundation for WPTS conformance improvements

**Addresses:**
- Issue #252 (macOS signing)
- Credit flow control (MS-SMB2 3.3.5.2.3 compliance)

**Avoids:**
- Pitfall 1 (credit grant zero)
- Pitfall 2 (credit validation missing)
- Pitfall 4 (macOS hash mismatch)

**Estimated complexity:** Medium (debugging preauth hash + protocol compliance wiring)

**Research flags:** Standard SMB protocol patterns. No additional research needed.

---

### Phase 2: Storage Management
**Rationale:** Quotas modify GetFilesystemStatistics, which payload stats also touches. Do quotas first to establish the pattern. Payload stats are quick win with existing BlockStore infrastructure. Both needed before trash (trash must count against quota).

**Delivers:**
- Per-share quota configuration and enforcement
- FSSTAT/FSINFO quota-aware reporting (NFS)
- FileFsSizeInformation quota reporting (SMB)
- Accurate UsedSize in BlockStore.Stats()
- `dfsctl share` quota management

**Addresses:**
- Share quotas (#232)
- Payload stats (#216)

**Avoids:**
- Pitfall 6 (TOCTOU race — use atomic counter)
- Pitfall 7 (scanning performance — cached stats)
- Pitfall 13 (NFS/SMB reporting inconsistency)

**Estimated complexity:** Medium (DB migration + service logic + cross-protocol consistency)

**Research flags:** Standard quota patterns. No additional research needed.

---

### Phase 3: Operational Visibility
**Rationale:** Client tracking is fully independent. Can be built in parallel with Phase 1-2 or immediately after. Provides operational visibility into active connections before tackling complex WPTS fixes.

**Delivers:**
- Protocol-agnostic ClientRecord model
- ClientRegistry runtime sub-service
- `dfsctl client list` command
- REST API `/api/clients` endpoint
- TTL-based expiry to prevent memory leak

**Addresses:**
- Client tracking (#157)

**Avoids:**
- Pitfall 8 (memory leak from stale entries — TTL expiry)
- Pitfall 14 (double-counting multi-protocol clients — IP-based keying)

**Estimated complexity:** Medium (new service + API + CLI following established patterns)

**Research flags:** Standard registry patterns. No additional research needed.

---

### Phase 4: WPTS Conformance Push
**Rationale:** Credit flow is stable (Phase 1), so WPTS baseline is solid. Primary target is ChangeNotify (20 failures) where infrastructure exists but needs wiring. Iterative approach with full regression testing after each fix.

**Delivers:**
- CHANGE_NOTIFY full implementation (wire NotifyRegistry into metadata operations)
- Negotiate/Encryption fixes (5 tests, same preauth issues as macOS fix)
- Leasing/DurableHandle edge case fixes (6 tests)
- Timestamp algorithm fixes (3 tests)
- Target: 73 known → ~40-45 known (reduce by ~30)

**Addresses:**
- WPTS conformance (reduce 73 known failures)

**Avoids:**
- Pitfall 5 (regressions — full suite runs after every change)
- Pitfall 16 (Docker staleness — pin WPTS version)

**Estimated complexity:** High (20+ individual fixes, iterative process)

**Research flags:** Each fix may need MS-SMB2 spec deep dive. Budget time for root-cause analysis.

---

### Phase 5: Advanced Storage Features
**Rationale:** Trash depends on quota tracking (trash must count against quota) and GC coordination (trashed files are still "live" for GC). Highest complexity and least urgent — defer to end after solid test coverage exists.

**Delivers:**
- Server-side trash with per-share config
- Configurable retention (1-180 days)
- Admin restore via API/CLI (`dfsctl trash list/restore/purge`)
- Hidden `.trash/` directory filtered from READDIR/QueryDirectory
- Background TrashScavenger for expired items
- GC coordination (trashed files still referenced)

**Addresses:**
- Trash/soft-delete (#190)

**Avoids:**
- Pitfall 9 (GC interaction — trash files still "live" for GC)
- Pitfall 10 (cross-protocol visibility — hide .trash from listings)
- Pitfall 15 (sync race — purge order: remote blocks, local blocks, metadata)

**Estimated complexity:** High (new subsystem, scavenger, cross-protocol semantics, GC integration)

**Research flags:** Trash/GC coordination may need phase-specific research for edge cases.

---

### Phase 6: Multi-Channel (Experimental)
**Rationale:** Most complex feature with highest risk due to connection lifecycle changes and documented data corruption risks from Samba history. Implement after all other features are stable with full test coverage. Consider shipping as experimental/opt-in.

**Delivers:**
- IOCTL interface advertisement (FSCTL_QUERY_NETWORK_INTERFACE_INFO)
- SMB2_SESSION_FLAG_BINDING in SESSION_SETUP
- Per-channel signing keys (channel-specific preauth hash)
- Session-connection registry multi-value support
- Lease break fan-out to all channels
- Feature flag for opt-in enablement

**Addresses:**
- SMB multi-channel session binding

**Avoids:**
- Pitfall 3 (session binding race — session-level mutex)
- Pitfall 12 (lease break fan-out failure — try all channels)
- Pitfall 19 (architecture coupling — ChannelSet abstraction)

**Estimated complexity:** Very High (architectural changes, connection lifecycle, concurrent access patterns)

**Research flags:** Multi-channel edge cases may need deeper research during implementation. Consider phase-specific research for lease break replay logic.

---

### Phase Ordering Rationale

**Dependency chains:**
```
Phase 1 (SMB Protocol Foundation)
    |
    v
Phase 4 (WPTS Conformance) — depends on Phase 1 credit flow baseline
    |
    v
Phase 6 (Multi-Channel) — depends on Phase 1 + Phase 4 protocol stability

Phase 2 (Storage Management)
    |
    v
Phase 5 (Trash) — depends on Phase 2 quota tracking and GC coordination

Phase 3 (Operational Visibility) — independent, can run parallel
```

**Build order prioritizes:**
1. **Unblock platforms first**: macOS signing fix (Phase 1) is highest priority standalone item
2. **Foundation before features**: Credit flow (Phase 1) before WPTS (Phase 4) ensures protocol compliance baseline
3. **Data before behavior**: Quotas/stats (Phase 2) before trash (Phase 5) establishes tracking infrastructure
4. **Stable before risky**: All other features before multi-channel (Phase 6) provides solid test coverage
5. **Visibility early**: Client tracking (Phase 3) provides operational insight before tackling complex conformance work

**Risk mitigation:**
- Multi-channel (highest risk) is last phase with feature flag for opt-in enablement
- WPTS (high iteration risk) comes after protocol foundation is solid
- Trash (complex GC interaction) deferred until quota/stats infrastructure proven

### Research Flags

**Phases with standard patterns (skip research-phase):**
- **Phase 1 (SMB Protocol Foundation)**: Well-documented MS-SMB2 spec, existing infrastructure
- **Phase 2 (Storage Management)**: Standard quota patterns, existing metadata service interfaces
- **Phase 3 (Operational Visibility)**: Established runtime sub-service patterns (mounts, stores)

**Phases likely needing deeper research:**
- **Phase 4 (WPTS Conformance)**: Each fix may need MS-SMB2 spec deep dive for specific info classes, lease break states, negotiate contexts. Budget time for root-cause analysis per test category (20 ChangeNotify, 5 negotiate, 6 leasing, etc.).
- **Phase 5 (Trash)**: Trash/GC coordination edge cases may need phase-specific research. Investigate: trash purge ordering with BlockStore sync, cross-protocol delete semantics (DELETE_ON_CLOSE vs REMOVE).
- **Phase 6 (Multi-Channel)**: Lease break replay logic, channel failure isolation, per-channel signing key derivation with SMB 3.1.1 preauth hash. Consider phase-specific research for production-grade implementation.

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | **HIGH** | Zero new dependencies verified against existing go.mod. All features use stdlib or existing packages. |
| Features | **HIGH** (table stakes), **MEDIUM** (differentiators) | Table stakes validated against NetApp, ONTAP, Samba, Windows Server behavior. Multi-channel complexity based on Samba implementation history. |
| Architecture | **HIGH** | Direct source code analysis of integration points. MetadataService as hub for quotas/trash/stats is natural extension. Credit flow fits existing session pipeline. |
| Pitfalls | **HIGH** (1-5), **MEDIUM** (6-15) | Critical pitfalls verified against MS-SMB2 spec and Samba release notes. Moderate pitfalls based on TOCTOU patterns, GC interaction analysis, and codebase architecture review. |

**Overall confidence:** HIGH

### Gaps to Address

**Minimal gaps — most patterns are established:**

1. **Multi-channel lease break replay logic**: Samba bug #11897 documents "missing oplock/lease break request replay" as a known multi-channel issue. The exact replay algorithm when a channel reconnects is not fully specified in MS-SMB2. **Handling**: Phase 6 may need targeted research or Samba source code review for production-grade implementation.

2. **Trash/GC coordination timing**: The exact ordering of trash purge (delete metadata first vs blocks first) when BlockStore syncer is mid-upload is not fully explored. **Handling**: Phase 5 should include E2E test: create file → write data → delete (trash) → trigger sync → trigger GC → restore from trash → verify data intact.

3. **macOS-specific NEGOTIATE differences**: The exact byte differences between macOS and Windows NEGOTIATE requests (context ordering, salt values, capability flags) are inferred but not packet-capture verified. **Handling**: Phase 1 must include macOS packet capture comparison with Windows to identify exact mismatch bytes.

4. **WPTS test interdependencies**: Which of the 73 known failures share root causes vs which are independent bugs is not fully categorized. **Handling**: Phase 4 should begin with root-cause grouping exercise before attempting fixes.

All gaps are addressable during implementation phases with standard debugging techniques (packet captures, source code review, E2E testing). No fundamental unknowns that block planning.

## Sources

### Primary (HIGH confidence)

**Official Microsoft specifications:**
- [MS-SMB2: Verifying Credit Charge and Payload Size](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/fba3123b-f566-4d8f-9715-0f529e856d25)
- [MS-SMB2: Algorithm for Granting Credits](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/2e366edb-b006-47e7-aa94-ef6f71043ced)
- [MS-SMB2: Granting Credits to Client](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/46256e72-b361-4d73-ac7d-d47c04b32e4b)
- [MS-SMB2: Session Binding](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/9a697646-6085-4597-808c-765bb2280c6e)
- [MS-SMB2: Per Session State](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/8174c219-2224-4009-b96a-06d84eccb3ae)
- [SMB 3.1.1 Pre-authentication Integrity in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10)

**DittoFS codebase analysis:**
- `internal/adapter/smb/session/`, `internal/adapter/smb/crypto_state.go`, `internal/adapter/smb/hooks.go`
- `pkg/metadata/service.go`, `pkg/controlplane/runtime/`, `pkg/blockstore/engine/`
- `test/smb-conformance/KNOWN_FAILURES.md`: 193 pass / 73 known / 69 skipped

### Secondary (MEDIUM confidence)

**Implementation references:**
- [Samba Wiki: SMB2 Credits](https://wiki.samba.org/index.php/SMB2_Credits)
- [Samba 4.4.0 Release Notes](https://www.samba.org/samba/history/samba-4.4.0.html) — Multi-channel data corruption documentation
- [NetApp SMB 3.0 Multichannel Technical Report](https://www.netapp.com/media/17136-tr4740.pdf)
- [NetApp: Quota Display with NFS Clients](https://kb.netapp.com/Advice_and_Troubleshooting/Data_Storage_Software/ONTAP_OS/How_Quotas_display_with_NFS_clients_df_output)
- [Samba Multi-Channel SNIA Presentation](https://www.snia.org/educational-library/samba-multi-channel-iouring-status-update-2020)

**NAS vendor patterns:**
- [Synology DSM Recycle Bin](https://mariushosting.com/synology-how-to-empty-all-recycle-bins-on-dsm-7/)
- [Seagate NAS OS: Network Recycle Bin](https://www.seagate.com/support/kb/nas-os-4x-network-recycle-bin-nrb-006005en/)

### Tertiary (LOW confidence)

**General patterns:**
- [Filesystem Quota Management in Go (ANEXIA Blog)](https://anexia.com/blog/en/filesystem-quota-management-in-go/) — Go quotactl wrapper approach (not used, pattern reference only)
- [github.com/nao1215/trash](https://pkg.go.dev/github.com/nao1215/trash/go-trash) — Go FreeDesktop trash library (not used, pattern reference only)

---

**Research completed:** 2026-03-20
**Ready for roadmap:** Yes

**Total estimated effort:** ~2500-3500 LOC across 8 features, 6 phases, zero new dependencies
