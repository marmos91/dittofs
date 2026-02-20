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

