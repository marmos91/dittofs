# Phase 40: SMB3 Conformance Testing - Context

**Gathered:** 2026-03-02
**Status:** Ready for planning

<domain>
## Phase Boundary

Validate the complete SMB2/SMB3 implementation (phases 33-39) against industry conformance suites, native Go client library, real OS clients (Windows/macOS/Linux), and cross-protocol coordination. All SMB2 and SMB3 tests must pass — iterate and fix until conformance is achieved. Only genuinely inapplicable tests (unimplemented features like ADS, multi-channel) are acceptable as known failures after investigation.

</domain>

<decisions>
## Implementation Decisions

### smbtorture SMB3 Scope
- Target ALL SMB3 suites: durable_v2, lease, replay, session, encryption — plus all existing SMB2 suites
- Both SMB2 and SMB3 tests must pass — no selective skipping
- Remove wildcard entries from KNOWN_FAILURES.md, enumerate individual test names instead
- Every failure must be investigated before being added to KNOWN_FAILURES — only truly inapplicable tests (unimplemented features) are acceptable
- smbtorture runs in CI alongside existing WPTS BVT as separate CI jobs — both must pass
- smbtorture must also run with Kerberos authentication (--use-kerberos), not just NTLM
- Pass rate: all tests pass except documented known failures for unimplemented features or interactive tests

### WPTS BVT Suite
- Same iterate-and-fix approach as smbtorture — all BVT tests must pass
- Re-measure baseline before starting Phase 40 work (phases 30-35 improved pass rate since last measurement)
- Explore additional WPTS categories beyond FileServer BVT (Auth, RSVD) and enable any relevant ones
- Add test filtering capability (e.g., --filter encryption) for faster iteration during fix cycles
- Memory profile for iteration speed, all 5 profiles as final gate before phase completion
- Fix ordering: by impact — fix failures that unblock the most other tests first (cascade approach)
- Multiple small plans per fix iteration batch, each with atomic commits and re-run verification
- Before committing any test to KNOWN_FAILURES, verify it's genuinely a known failure for an unimplemented feature — otherwise fix it

### Go Integration Tests (go-smb2)
- Use hirochachacha/go-smb2 library — confirmed supports SMB 3.0, 3.0.2, 3.1.1, AES-128-CCM/GCM encryption, signing, SHA-512 preauth integrity
- Full SMB3 feature matrix: encryption, signing, dialect negotiation, session setup, file ops, directory ops
- Tests live in test/e2e/ alongside existing SMB tests (E2E with go-smb2 as client)
- Reuses existing E2E framework (server process, helpers)

### Client Compatibility Matrix
- Automated smbclient tests (Go tests that exec smbclient) covering full SMB3 feature set: file CRUD, directory ops, permissions, encryption, signing, different dialects
- Automated multi-OS native client testing in CI (public repo = free GitHub Actions minutes):
  - **Windows**: `windows-latest` runner, DittoFS cross-compiled, `net use` mount, file operations, verify SMB 3.1.1 dialect
  - **macOS**: `macos-latest` runner, DittoFS compiled, `mount_smbfs`, file operations, verify SMB 3.0.2 dialect
  - **Linux**: `ubuntu-latest` runner, DittoFS compiled, `mount.cifs`, file operations, verify SMB 3.1.1 dialect
- Standard file ops + dialect verification per platform (not full feature matrix per OS)
- Manual deep testing of Windows/macOS remains in Phase 40.5

### Kerberos SMB Testing
- Full Kerberos matrix: encryption + Kerberos, signing + Kerberos, leases + Kerberos, durable handles + Kerberos
- Test NTLM fallback and guest sessions
- Cross-protocol Kerberos: same principal accesses same file via both NFS (RPCSEC_GSS) and SMB (SPNEGO)
- Kerberos testing against AD as KDC (not full AD domain join — just AD as Kerberos realm)
- Both smbtorture (--use-kerberos) and Go E2E tests cover Kerberos scenarios

### Cross-Protocol Scenarios
- Bidirectional break matrix: SMB3 file lease → NFS write triggers break, NFS delegation → SMB open triggers recall, SMB directory lease → NFS create/delete/rename triggers break
- Moderate concurrency: 5-10 goroutines doing simultaneous NFS + SMB operations
- Verify both mechanism (break/recall fires) AND data consistency (content matches after conflict resolution)
- Include Kerberos cross-protocol: same Kerberos principal accesses via both protocols, identity mapper resolves consistently

### CI Refactoring
- Audit and refactor GitHub Actions workflows to avoid overcrowding, especially PR checks
- Organize workflows sensibly: what runs on PR (fast/essential), what runs on push (comprehensive), what runs weekly (full matrix)
- Document CI workflows in docs/CONTRIBUTING.md

### Documentation
- Update test/smb-conformance/README.md with SMB3 suite details
- Add documentation for go-smb2 tests, smbclient tests, cross-protocol tests, multi-OS CI
- One comprehensive testing guide covering all suites
- Add CI section to docs/CONTRIBUTING.md covering all workflows, triggers, result interpretation, manual dispatch

### Claude's Discretion
- WPTS Scenario tests: evaluate whether to add beyond BVT based on available categories and value
- Exact smbtorture suite list per Kerberos run
- Test infrastructure implementation details (how to set up cross-compiled binaries in CI)
- smbclient output parsing approach
- Fix ordering within impact-based batches

</decisions>

<specifics>
## Specific Ideas

- "Before committing any test to KNOWN_FAILURES, we want to make sure they are known failures. Otherwise we want to fix them in this phase"
- Iterate-and-fix approach: re-measure baseline → fix by impact (cascade) → re-run → repeat until all tests pass
- Multiple small plans per iteration — each fix batch gets its own plan with atomic commits and verification
- Public repo means all three OS CI runners (Windows, macOS, Linux) are free
- GitHub Actions refactoring needed alongside new workflows to keep PR checks clean and fast

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `test/smb-conformance/` — Complete WPTS BVT infrastructure: Docker Compose, 5 storage profiles, known failures tracking, result parsing
- `test/smb-conformance/smbtorture/` — smbtorture test runner with known failures and result parsing
- `.github/workflows/smb-conformance.yml` — Tiered CI pipeline (memory on PR, all profiles on push/weekly)
- `test/e2e/file_operations_smb_test.go` — Basic SMB file operations E2E pattern
- `test/e2e/smb_kerberos_test.go` — Kerberos SMB test pattern
- `test/e2e/cross_protocol_test.go` — Cross-protocol NFS↔SMB test pattern
- `test/e2e/framework/` — Container management (Localstack, PostgreSQL, Kerberos KDC)
- `test/e2e/helpers/` — Server process, CLI, mount, unique naming, adapter management

### Established Patterns
- E2E tests use `//go:build e2e` tag, started via `StartServerProcess(t, "")` with `t.Cleanup(sp.ForceKill)`
- Known failures tracked in markdown tables, parsed by shell scripts for CI pass/fail
- Docker Compose for external service dependencies (WPTS container, Localstack, PostgreSQL)
- Tiered CI: fast checks on PR, comprehensive on push/weekly

### Integration Points
- go-smb2 tests connect to DittoFS server process via TCP (same as smbclient)
- Multi-OS tests need DittoFS cross-compilation (GOOS=windows/darwin/linux)
- Kerberos tests reuse existing KDC container setup from `test/e2e/framework/kerberos.go`
- Cross-protocol tests reuse shared metadata/payload store pattern from `cross_protocol_test.go`

</code_context>

<deferred>
## Deferred Ideas

- **Full Active Directory domain join** — DittoFS joins AD as member server with machine account, SPN registration, group policy support. New capability, needs its own phase. **Action: Create GitHub issue + add as new phase.**
- **WPTS Scenario tests** — May be evaluated during Phase 40 (Claude's discretion) but full expansion is a separate effort if warranted.

</deferred>

---

*Phase: 40-smb3-conformance-testing*
*Context gathered: 2026-03-02*
