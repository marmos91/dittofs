# WPTS BVT Baseline Results

Last updated: 2026-03-02 (Phase 40, pre-measurement)

## Summary

| Metric | Phase 29.8 (v3.6) | Phase 40 (v3.8) | Delta |
|--------|-------------------|-----------------|-------|
| **Total BVT tests** | 240 | TBD | - |
| **Passed** | 133 | TBD | - |
| **Failed** | ~107 | TBD | - |
| **Pass rate** | 55.4% | TBD | - |

> **Note:** The Phase 40 baseline has not yet been measured. This document was prepared
> with the Phase 29.8 baseline and expected improvements from Phases 30-39.
> Run `./run.sh --profile memory --verbose` on an x86_64 Linux host (the WPTS container
> is linux/amd64 only) to capture the actual baseline. Then update this document with
> the real numbers.

**DittoFS commit:** 52f84ecd (feat/phase-40-smb3-conformance-testing branch)

## How to Re-measure

```bash
# On x86_64 Linux (CI or similar):
cd test/smb-conformance
./run.sh --profile memory --verbose

# Parse the TRX file to extract individual test outcomes:
./parse-results.sh results/<timestamp>/<file>.trx KNOWN_FAILURES.md
```

After running, update this document with actual counts and individual test outcomes.

## Expected Improvements from Phases 30-39

The following phases made changes that are expected to improve the WPTS BVT pass rate
beyond the Phase 29.8 baseline of 133/240:

### Phase 30: Bug Fixes
- **BUG-01 (Sparse file READ):** Zero-fill for unwritten blocks. May fix tests reading sparse regions.
- **BUG-02 (Renamed directory listing):** Path updated before persistence on Move. May fix QueryDirectory tests after rename.
- **BUG-03 (Parent dir navigation):** Multi-component `..` path resolution. May fix path traversal tests.
- **BUG-04 (Oplock break wiring):** NFS ops trigger oplock break for SMB clients.
- **BUG-05 (NumberOfLinks):** FileStandardInfo.NumberOfLinks reads actual link count. May fix FileStandardInformation tests.
- **BUG-06 (Pipe share list caching):** Share list cached for pipe CREATE. May fix named pipe connection tests.

### Phase 31: Windows ACL Support
- **SD-01 through SD-08 (Security Descriptors):** Full DACL synthesis from POSIX mode bits with owner, group, well-known SIDs, canonical ACE ordering, inheritance flags, SACL stub.
- May fix: OWNER_SECURITY_INFORMATION, DACL_SECURITY_INFORMATION, SACL_SECURITY_INFORMATION, SET_INFO tests.

### Phase 32: Protocol Compatibility
- **MxAc create context:** Returns maximal access mask from POSIX permissions.
- **QFid create context:** Returns on-disk file ID with volume ID.
- **FileCompressionInformation (class 28):** Returns valid fixed-size buffer.
- **FileAttributeTagInformation (class 35):** Returns valid fixed-size buffer.
- **Updated capability flags:** FILE_SUPPORTS_SPARSE_FILES added to FileFsAttributeInformation.

### Phase 33: SMB3 Encryption
- AES-128-CCM, AES-128-GCM, AES-256-CCM, AES-256-GCM ciphers implemented.
- Full transform header encoding/decoding.
- Connection-level encrypt/decrypt state.
- Preauth integrity hash tracking (SHA-512).
- NEGOTIATE context support (SMB2_ENCRYPTION_CAPABILITIES).
- VALIDATE_NEGOTIATE_INFO IOCTL.

### Phase 34: SMB3 Signing
- AES-128-CMAC (per MS-SMB2 3.0+), AES-128-GMAC (3.1.1), HMAC-SHA256 (2.x).
- Session key derivation via SP800-108 KDF.
- SIGNING_CAPABILITIES negotiate context.
- Signature verification on signed requests.

### Phase 35-37: SMB3 Features (Leasing, Sessions, Kerberos)
- Lease V2 support with parent key and epoch tracking.
- Session binding, reconnect, re-authentication.
- Kerberos authentication via SPNEGO/GSSAPI.

### Phase 38: Durable Handles
- Durable handle V1 (DHnQ/DHnC) and V2 (DH2Q/DH2C).
- Reconnect with session key verification.
- Handle scavenger for expired durable handles.

### Phase 39: Cross-Protocol Integration
- Unified caching model (SMB leases + NFS delegations).
- Bidirectional break/recall across protocols.
- Cross-protocol identity mapping.

## Expected Impact on Known Failures

Based on the implementations above, these categories of known failures may now pass:

| Category | Known Failure Count | Expected After Phase 40 |
|----------|-------------------|------------------------|
| DurableHandle (V1 reconnect) | 2 | May now pass (Phase 38) |
| Leasing (V1 file lease) | 1 | May now pass (Phase 35-37) |
| OpLock (break notification) | 1 | May now pass (Phase 30 BUG-04) |
| FsInfo (Encryption flag) | 1 | May now pass (Phase 33 encryption) |
| Timestamp | 5 | Possibly improved (Phase 30-32) |
| ChangeNotify | 17 | Unlikely (not implemented) |
| ADS | 9 | Unlikely (not implemented) |
| VHD/RSVD | 24 | No change (permanently out of scope) |
| SWN | 5 | No change (permanently out of scope) |
| SQoS | 3 | No change (permanently out of scope) |
| DFS | 2 | No change (permanently out of scope) |
| NTFS-FsCtl | 11 | No change (permanently out of scope) |
| FsInfo (others) | 2 | No change (permanently out of scope) |
| NamedPipe | 2 | Possibly improved |

**Optimistic estimate:** 133 + 5 (newly passing) = ~138 passing (57.5%)
**Conservative estimate:** 133 + 2 (leasing + durable) = ~135 passing (56.3%)

## Additional WPTS Categories

### Categories to Explore

Run these on x86_64 Linux to assess relevance:

```bash
# Auth category (may test NTLM/Kerberos/guest scenarios)
./run.sh --category Auth --verbose

# Model category (SMB2 model tests)
./run.sh --category Model --verbose

# Specific feature filters (for investigation during fix cycles)
./run.sh --filter "FullyQualifiedName~Encryption" --verbose
./run.sh --filter "FullyQualifiedName~Lease" --verbose
./run.sh --filter "FullyQualifiedName~DurableHandle" --verbose
```

### Known Category Sizes (from research)

| Category | Approximate Count | CI Suitability |
|----------|------------------|----------------|
| BVT | ~101-240 | Yes (fast, core verification) |
| SMB2 Feature Test | ~2,664 | No (too large for CI; cherry-pick) |
| Server Failover | ~48 | No (requires clustering) |
| RSVD | ~29 | No (VHD not supported) |
| DFSC | ~41 | No (DFS not implemented) |
| Auth | Varies | Investigate (may test NTLM/Kerberos) |

## Individual Test Results

> **Pending:** This section will be populated after running the WPTS BVT baseline
> on x86_64 Linux. The `parse-results.sh` script will produce per-test outcomes.

### Template (update after running baseline):

```
| Test Name | Outcome | Previous (Phase 29.8) | Notes |
|-----------|---------|----------------------|-------|
| (populated after run) | | | |
```

### Newly Passing (tests in KNOWN_FAILURES.md that now pass)

> Pending baseline measurement.

### Fix Candidates (tests NOT in KNOWN_FAILURES that fail and appear fixable)

> Pending baseline measurement.

## Changelog

- **Phase 67 (2026-03-20):** SMB 3.1.1 preauth integrity hash chain verified correct via conformance tests (7 tests including MS-SMB2 test vectors). All 4 pitfalls from issue #252 confirmed correctly handled. Negotiate response wire format validated (context alignment, security buffer offsets). 5 BVT_Negotiate_SMB311 tests remain in KNOWN_FAILURES.md pending WPTS verbose log diagnosis to identify root cause (not a preauth hash computation issue).
- **Phase 40 (2026-03-02):** Created baseline-results.md template with Phase 29.8 reference, expected improvements from Phases 30-39, and exploration guide for additional categories.
