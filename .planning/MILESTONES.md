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

