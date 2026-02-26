# Requirements: DittoFS v3.6 Windows Compatibility

**Defined:** 2026-02-26
**Core Value:** Full Windows SMB compatibility with proper ACL support, bug fixes, and conformance testing

## v3.6 Requirements

### Bug Fixes

- [ ] **BUG-01**: Sparse file READ returns zeros for unwritten blocks instead of errors (#180)
- [ ] **BUG-02**: Renamed directory children reflect updated paths in QUERY_DIRECTORY (#181)

### Security Descriptors

- [ ] **SD-01**: Default DACL synthesized from POSIX mode bits (owner/group/other) when no ACL exists
- [ ] **SD-02**: ACEs ordered in canonical Windows order (deny before allow)
- [ ] **SD-03**: Well-known SIDs included in default DACL (NT AUTHORITY\SYSTEM, BUILTIN\Administrators)
- [ ] **SD-04**: ACE flag translation corrected (NFSv4 INHERITED_ACE 0x80 -> Windows 0x10)
- [ ] **SD-05**: Inheritance flags (CONTAINER_INHERIT, OBJECT_INHERIT) set on directory ACEs
- [ ] **SD-06**: SE_DACL_AUTO_INHERITED control flag set when ACEs have INHERITED flag
- [ ] **SD-07**: SID user/group collision fixed (different RID ranges for users vs groups)
- [ ] **SD-08**: SACL query returns valid empty SACL structure (not omitted)

### Windows 11 Compatibility

- [ ] **WIN-01**: CREATE response context wire encoding fixed (lease responses actually sent)
- [ ] **WIN-02**: MxAc (Maximal Access) create context response returned to clients
- [ ] **WIN-03**: QFid (Query on Disk ID) create context response returned to clients
- [ ] **WIN-04**: SMB signing validated and enforced for all authenticated sessions
- [ ] **WIN-05**: Missing FileInfoClass handlers added (FileCompressionInformation, FileAttributeTagInformation, FilePositionInformation, FileModeInformation)
- [ ] **WIN-06**: Guest access signing negotiation handled for Windows 11 24H2

### Conformance Testing

- [ ] **TEST-01**: smbtorture SMB2 test suite run against DittoFS, failures triaged
- [ ] **TEST-02**: Newly-revealed conformance failures fixed iteratively
- [ ] **TEST-03**: KNOWN_FAILURES.md updated with current pass/fail status

## Future Requirements (v3.7+)

### Deferred

- **Short name (8.3) generation** — Low priority, only affects legacy apps
- **FILE_ATTRIBUTE_ARCHIVE flag** — Improves backup tool compat, not critical
- **CHANGE_NOTIFY cleanup on disconnect** — Quality improvement, not blocking
- **FileNormalizedNameInformation** — Helps path normalization, not required
- **Full SACL enforcement** — Requires audit logging infrastructure
- **Domain-aware SID construction** — Stable pseudo-domain SID from ServerGUID (nice-to-have)

## Out of Scope

| Feature | Reason |
|---------|--------|
| SMB 3.0/3.0.2/3.1.1 dialect negotiation | v3.8 milestone |
| AES encryption (SMB3) | v3.8 milestone |
| Durable handles (DHnQ/DH2Q) | v3.8 milestone |
| Multi-channel / RDMA | v3.8 milestone |
| Server-side copy (FSCTL_SRV_COPYCHUNK) | v3.8 or v4.0 |
| Extended Attributes (EA) | v4.0 with NFSv4.2 xattrs |
| POSIX Extensions for SMB | Not needed for Windows compat |
| Raw SD byte caching for round-trip fidelity | Complexity vs benefit tradeoff |

## Traceability

| Requirement | Phase | Status |
|-------------|-------|--------|
| BUG-01 | Phase 30 | Pending |
| BUG-02 | Phase 30 | Pending |
| SD-01 | Phase 31 | Pending |
| SD-02 | Phase 31 | Pending |
| SD-03 | Phase 31 | Pending |
| SD-04 | Phase 31 | Pending |
| SD-05 | Phase 31 | Pending |
| SD-06 | Phase 31 | Pending |
| SD-07 | Phase 31 | Pending |
| SD-08 | Phase 31 | Pending |
| WIN-01 | Phase 32 | Pending |
| WIN-02 | Phase 32 | Pending |
| WIN-03 | Phase 32 | Pending |
| WIN-04 | Phase 32 | Pending |
| WIN-05 | Phase 32 | Pending |
| WIN-06 | Phase 32 | Pending |
| TEST-01 | Phase 32 | Pending |
| TEST-02 | Phase 32 | Pending |
| TEST-03 | Phase 32 | Pending |

**Coverage:**
- v3.6 requirements: 19 total
- Mapped to phases: 19 ✓
- Unmapped: 0 ✓

---
*Requirements defined: 2026-02-26*
*Last updated: 2026-02-26 after roadmap refinement*
