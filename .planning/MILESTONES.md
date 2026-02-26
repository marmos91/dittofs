# Project Milestones: DittoFS NFS Protocol Evolution

## v1.0 NLM + Unified Lock Manager (Shipped: 2026-02-07)

**Delivered:** Unified cross-protocol locking foundation with NLM, NSM, SMB leases, and cross-protocol lock coordination.

**Phases completed:** 1-5 (19 plans total)

**Key accomplishments:**

- Unified Lock Manager embedded in metadata service with protocol-agnostic ownership model
- NLM protocol (RPC 100021) with blocking lock queue and GRANTED callbacks
- NSM protocol (RPC 100024) with crash recovery and parallel SM_NOTIFY
- SMB2/3 lease support (Read/Write/Handle) with 35s break timeout
- Cross-protocol lock coordination: NLM locks visible to SMB, SMB leases visible to NLM
- E2E tests for NLM locking, SMB leases, cross-protocol conflicts, and grace period recovery

**Stats:**

- 5 phases, 19 plans
- Feb 1 - Feb 7, 2026

**Archive:** [v1.0-ROADMAP.md](milestones/v1.0-ROADMAP.md) | [v1.0-REQUIREMENTS.md](milestones/v1.0-REQUIREMENTS.md)

---

## v2.0 NFSv4.0 + Kerberos (Shipped: 2026-02-20)

**Delivered:** Full NFSv4.0 stateful protocol implementation with RPCSEC_GSS Kerberos authentication, delegations, ACLs, identity mapping, and comprehensive E2E test suite.

**Phases completed:** 6-15 (42 plans total)

**Key accomplishments:**

- NFSv4.0 COMPOUND dispatcher with pseudo-filesystem and 33+ operation handlers
- Stateful NFSv4 protocol: client IDs, stateids, open/lock-owners, lease management, grace period
- Read/write delegations with CB_RECALL, recall timers, revocation, and anti-storm protection
- RPCSEC_GSS Kerberos authentication (krb5/krb5i/krb5p) with keytab hot-reload
- NFSv4 ACLs with identity mapping, SMB Security Descriptor interop, and control plane integration
- Comprehensive E2E test suite: 50+ NFSv4 tests covering locking, delegations, Kerberos, ACLs, POSIX compliance

**Stats:**

- 10 phases, 42 plans
- 224,306 LOC Go
- Feb 7 - Feb 20, 2026 (13 days)

**Known tech debt:**

- ACL evaluation not yet integrated into metadata CheckAccess (wire format works)
- Delegation Prometheus metrics not instrumented (log scraping workaround)
- Netgroup mount enforcement not implemented (CRUD works via API)

**Archive:** [v2.0-ROADMAP.md](milestones/v2.0-ROADMAP.md) | [v2.0-REQUIREMENTS.md](milestones/v2.0-REQUIREMENTS.md) | [v2.0-MILESTONE-AUDIT.md](milestones/v2.0-MILESTONE-AUDIT.md)

---


## v3.0 NFSv4.1 Sessions (Shipped: 2026-02-25)

**Delivered:** NFSv4.1 session infrastructure with exactly-once semantics, backchannel multiplexing, directory delegations, trunking, and SMB Kerberos authentication.

**Phases completed:** 16-25 (25 plans total)

**Key accomplishments:**

- NFSv4.1 XDR types and constants: 19 forward ops, 10 callback ops, 40+ error codes, full encode/decode
- Session infrastructure: EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION with slot table allocation and channel negotiation
- Exactly-once semantics via per-slot replay cache with SEQUENCE validation on every v4.1 COMPOUND
- Backchannel multiplexing: CB_SEQUENCE over fore-channel TCP connection (NAT-friendly, no separate dial-out)
- Connection management and trunking: BIND_CONN_TO_SESSION, multi-connection sessions, server_owner consistency
- Client lifecycle: DESTROY_CLIENTID, FREE_STATEID, TEST_STATEID, RECLAIM_COMPLETE, grace period API
- Directory delegations: GET_DIR_DELEGATION, CB_NOTIFY with batched notifications and conflict recall
- SMB Kerberos: SPNEGO/Kerberos in SESSION_SETUP with shared Kerberos layer and identity mapping
- v4.0/v4.1 coexistence: minorversion routing, independent state, simultaneous mounts
- E2E tests: session lifecycle, EOS replay, backchannel delegation recall, directory notifications, disconnect robustness

**Stats:**

- 10 phases, 25 plans
- 256,842 LOC Go
- 336 files changed, +61,004 / -5,037 lines
- Feb 20 - Feb 25, 2026 (5 days)

**Known tech debt:**

- ACL enforcement in CheckAccess (carried from v2.0)
- Delegation Prometheus metrics (carried from v2.0)
- Netgroup mount enforcement (carried from v2.0)
- LIFE-01 through LIFE-04 traceability entries stale (work complete, table not updated)

**Archive:** [v3.0-ROADMAP.md](milestones/v3.0-ROADMAP.md) | [v3.0-REQUIREMENTS.md](milestones/v3.0-REQUIREMENTS.md)

---


## v3.5 Adapter + Core Refactoring (Shipped: 2026-02-26)

**Delivered:** Clean separation of protocol-specific code from generic layers, unified lock model, restructured NFS/SMB adapters with shared infrastructure, and decomposed core objects for maintainability.

**Phases completed:** 26-29.4 (5 phases, 22 plans)

**Key accomplishments:**

- Unified lock model (OpLock/AccessMode/UnifiedLock) shared by NFS, SMB, and NLM with centralized conflict detection
- Protocol leak purge: removed ~15 protocol-specific types/methods from generic metadata, controlplane, and lock layers
- NFS adapter restructured: `internal/protocol/` -> `internal/adapter/nfs/`, v4/v4.1 hierarchy split, consolidated dispatch
- SMB adapter restructured: BaseAdapter shared with NFS, Authenticator interface, framing/signing/dispatch extracted
- Core decomposed: Store interface split into 9 sub-interfaces, Runtime into 6 sub-services, Offloader renamed and split into 8 files
- Error and boilerplate reduction: PayloadError type, generic GORM/API helpers, centralized API error mapping, metadata file splits

**Stats:**

- 5 phases, 22 plans
- 244 files changed, +23,305 / -10,771 lines
- Feb 25 - Feb 26, 2026 (2 days)

**Known tech debt:**

- REF-01.8/REF-01.9 adapter translation layers deferred to v3.8
- 4 TODO(plan-03) cross-protocol oplock break markers (requires v3.8)
- PayloadError defined but not yet wired into production error paths

**Archive:** [v3.5-ROADMAP.md](milestones/v3.5-ROADMAP.md) | [v3.5-REQUIREMENTS.md](milestones/v3.5-REQUIREMENTS.md) | [v3.5-MILESTONE-AUDIT.md](milestones/v3.5-MILESTONE-AUDIT.md)

---

