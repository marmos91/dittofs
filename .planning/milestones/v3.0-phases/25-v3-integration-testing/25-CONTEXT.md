# Phase 25: v3.0 Integration Testing - Context

**Gathered:** 2026-02-23
**Status:** Ready for planning

<domain>
## Phase Boundary

Verify all NFSv4.1 functionality end-to-end with real Linux NFS client mounts. Covers session/EOS replay, backchannel delegation recall, directory delegation notifications, v4.0/v4.1 coexistence, and SMB Kerberos integration. All tests use the existing e2e framework extended with v4.1 and cross-protocol capabilities.

</domain>

<decisions>
## Implementation Decisions

### Test Environment
- Run on both macOS and Linux (CI via extended existing GitHub Actions workflow)
- macOS: attempt all mounts, gracefully skip on failure (OS check — macOS skips, Linux treats mount failure as real failure)
- Use existing `e2e` build tag for all tests (no new tags)
- Server runs in-process (existing e2e pattern), with logs piped to t.Log()
- Protocol-specific tests (EOS, delegations, backchannel) use memory-only backends
- File I/O storage matrix runs against ALL existing backend combinations (memory/memory, badger/filesystem, memory/s3, etc.) for NFS v3, v4.0, v4.1, and SMB
- Small file sizes for I/O matrix (1MB, 10MB) — benchmarks are a separate suite later
- No test retries — flakiness must be fixed, not masked
- Functional tests only — no metrics/telemetry assertions
- Reuse existing KDC helper from framework/kerberos.go for SMB Kerberos tests, started once per test suite

### Test Organization
- Extend existing nfsv4_*.go test files (add v4.1 test functions alongside v4.0 tests)
- Parametrize existing file operation tests by protocol version: v3, v4.0, v4.1, and SMB all run the same core file ops suite
- Include SMB in the file operations matrix (both Kerberos and NTLM/simpler auth modes)
- Shared helpers go into existing framework/ and helpers/ directories
- Build custom test assertion helpers (e.g., for EOS replay verification, delegation recall checks)
- No requirement traceability comments in test code — test names and behavior speak for themselves

### EOS & Session Replay
- Use real Linux NFS client only (no custom RPC client)
- Verify replay through server-side observability (internal counters or log signals — Claude's discretion on mechanism)
- Test basic replay scenario + 1-2 edge cases (e.g., stale seqid rejected)
- For backchannel delegation recall (CB_RECALL): Claude picks the triggering approach (file conflict vs server-side force)
- Directory delegation notifications: test ALL notification types (entry added, removed, renamed, attributes changed)
- Verify delegation state is properly cleaned up after recall/revocation (assert removal from server state)

### Coexistence & Cross-Protocol
- v4.0 and v4.1 coexistence test: both versions mounted simultaneously on the SAME share
- Core file ops suite is version-parametrized — same tests run on v3, v4.0, v4.1, and SMB, ensuring transparent operation
- SMB full cross-protocol test: SPNEGO/Kerberos auth + basic file ops + bidirectional visibility (NFS->SMB and SMB->NFS)
- SMB file ops tested with both Kerberos and NTLM/simpler auth modes
- Identity mapping verification covers: UID/GID match, permission enforcement, cross-protocol identity consistency, group membership resolution
- macOS: native SMB mount via `mount_smbfs` for basic ops; advanced features on Linux only (attempt first, skip on failure)

### Test Framework Design
- Unified Mount interface using Go `os` package on mount paths (protocol-transparent, tests real kernel behavior)
- Separate interface levels: base interface (CRUD, attrs, lifecycle) + extended interface (links, ACLs, protocol-specific ops)
- Protocol-specific helpers available alongside unified interface for protocol-level tests
- Shared mountV41() helper for v4.1 mount setup with OS-aware skip logic
- One server per entire test suite (started in TestMain)
- Automatic cleanup of stale mounts and temp directories from failed test runs
- Server log dump to test output on test failure only
- Disconnect/robustness tests: force-close client during large file write, directory listing, and session establishment — verify no resource leaks or corruption

### Claude's Discretion
- Test timeout values per test case
- Mount reuse strategy (per-suite vs per-test)
- Test function naming convention for v4.1 tests within shared files
- EOS replay trigger mechanism (delay injection vs connection disruption)
- EOS replay detection mechanism (log parsing vs internal counters)
- CB_RECALL test approach (multi-client conflict vs server-side force recall)
- Event capture approach for protocol-level test assertions
- Coexistence test scope (pairwise vs all-protocols simultaneous)
- CI runner NFS package installation approach
- SMB client approach for tests (smbclient vs mount.cifs vs Go library)
- Parallel vs sequential execution of protocol version subtests
- Additional disconnect test scenarios beyond the three specified

</decisions>

<specifics>
## Specific Ideas

- "We should parametrize mount helpers to run the same suite on v3, v4 and v4.1. At least the basic suite with file ops. Then each version will test its own nuances separately. But the core operations should be transparent."
- "NFSv3 is stateless, NFSv4 is stateful. Write patterns to payload service may change and should be tested" — motivation for running full storage matrix on v4.0/v4.1, not just memory
- macOS native SMB mount via mount_smbfs for basic testing coverage
- All tests attempt to run everywhere — only skip when the OS genuinely can't support the feature (macOS v4.1 mount failure)

</specifics>

<deferred>
## Deferred Ideas

- Performance benchmarks for v4.1 vs v4.0 vs v3 — separate benchmark suite later
- Metrics/telemetry assertion tests — out of scope for this phase
- Test profiles (quick/full) — just use go test -run patterns instead

</deferred>

---

*Phase: 25-v3-integration-testing*
*Context gathered: 2026-02-23*
