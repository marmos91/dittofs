# Phase 30: SMB Bug Fixes - Context

**Gathered:** 2026-02-27
**Status:** Ready for planning

<domain>
## Phase Boundary

Fix 6 known SMB bugs blocking Windows file operations: sparse file READ zero-fill (#180), renamed directory child listing (#181), multi-component `..` path navigation (#214), cross-protocol oplock break wiring (#213), hardcoded NumberOfLinks (#221), and pipe share list caching (#223). No new features — strictly correctness fixes with regression coverage.

</domain>

<decisions>
## Implementation Decisions

### Oplock break coordination
- Oplock break mechanism must be **generic via Runtime** — any adapter can trigger breaks on any other adapter, not hardwired NFS→SMB
- This aligns with DittoFS multi-protocol architecture and future-proofs for additional adapters

### Sparse read behavior
- **Zero-fill gaps** within file size for unwritten blocks — return continuous buffer with zeros where blocks don't exist
- Zero-fill logic lives in the **payload layer** — single point of truth so both NFS and SMB benefit automatically
- **Short read / EOF** for reads past file size — standard POSIX behavior, sparse zero-fill only applies within file boundaries

### Fix approach
- Follow the **4 roadmap plans** as defined (30-01 through 30-04), each independently testable
- **Fix + improve surroundings** — while in the code, clean up related issues (better error messages, defensive checks)
- **Fix root causes** if a deeper architectural issue is revealed during a fix, rather than patching symptoms

### Regression testing
- Bug fix E2E tests **added to existing test files** by feature area, not a separate file
- Sparse READ tests cover **both** Windows Explorer workflow AND cross-protocol (write via NFS with gaps, read via SMB)
- Oplock break tested via **dual-mount E2E** — SMB client holds oplock, NFS client writes, verify break sent and data consistent
- Run **WPTS BVT suite locally** after each fix (not just CI)
- Run **SMB conformance suite** after fixes to verify no regressions

### Claude's Discretion
- Sync vs async oplock break wait strategy (based on MS-SMB2 spec and Samba reference)
- Oplock break timeout behavior on unresponsive SMB clients
- Which NFS v3 operations trigger oplock breaks (mutating only vs all conflicting)
- Sparse region tracking approach (implicit block detection vs explicit bitmap)
- Fix execution order across the 4 plans

</decisions>

<specifics>
## Specific Ideas

- Oplock break should follow Samba's behavior as reference implementation
- Sparse file behavior should match what Windows Explorer expects (standard sparse semantics)
- Cross-protocol testing is critical — bugs were discovered through real Windows usage

</specifics>

<deferred>
## Deferred Ideas

None — discussion stayed within phase scope

</deferred>

---

*Phase: 30-smb-bug-fixes*
*Context gathered: 2026-02-27*
