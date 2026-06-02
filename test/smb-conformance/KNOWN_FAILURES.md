# Known Failures - SMB Conformance (WPTS BVT)

Last updated: 2026-06-02 (#673 — removed BVT_DirectoryLeasing_ReadWriteHandleCaching: it passes deterministically; docs-only)

Tests listed here are expected to fail. CI will pass (exit 0) as long as
all failures are in this list. New failures not listed here will cause CI to fail.

## Final Tally (#673 v1.0 conformance gate)

Every remaining entry is justified and falls into exactly one bucket. No
UNJUSTIFIED entries remain.

- **Permanently Unimplementable / out-of-scope** (appendix) — **39**: VHD/RSVD ×26, SWN ×6, SQoS ×3, DFS ×2, NamedPipe ×2
- **Total: 39**

(Rendered as a list, not a markdown table, so `parse-results.sh` — which ingests every line beginning with `|` — does not mistake these tally lines for known-failure rows.)

No WPTS entry is deferred-with-issue and no entry is "flaky": every fixable BVT
test passes, leaving only the architecturally out-of-scope appendix.

The `parse-results.sh` script reads test names from the first column of the
table below. Lines starting with `#`, `|---`, empty lines, and the header
row (`Test Name`) are ignored.

## Rules

- The [Permanently Unimplementable](#permanently-unimplementable-out-of-scope) appendix at the bottom is the **only** place where entries may be added without a documented fix plan.
- Every entry above the appendix MUST either (a) reference an open GH sub-issue, or (b) be promoted into the appendix with a documented architectural reason.
- Goal: every non-appendix entry resolved before v1.0.

## Baseline Status

- **Initial baseline (Phase 29.8):** 133/240 BVT tests passing
- **Current baseline (#673):** 226/265 PASS, 39 known failures (all permanent / out-of-scope)
- **Target:** All BVT tests pass except genuinely unimplemented features

## Expected Failures

None. Every fixable BVT test passes on develop. New failures reported by
`parse-results.sh` must be investigated and fixed, not added here — see
"How to Add New Entries" below.

## Status Legend

| Status | Meaning |
|--------|---------|
| **Expected** | Known failure, fix planned in a future phase |
| **Permanent** | Feature intentionally not implemented (out of scope) — see appendix |

## Permanently Unimplementable (Out of Scope)

Tests below cannot be implemented in DittoFS by design. Reasons fall into the
following buckets:

1. **Virtual Hard Disk (RSVD).** SMB-over-RSVD shared VHD/VHDX file format
   and SCSI command tunneling. Requires a block-level VHD storage backend;
   DittoFS is a file-level virtual filesystem, not a disk subsystem.
2. **Service Witness Protocol (SWN).** Cluster failover notification protocol.
   Requires a multi-node clustered file server with shared-witness coordination;
   DittoFS is a single-node userspace server.
3. **Storage QoS (SQOS).** Per-VHD IOPS / latency policy enforcement.
   Requires Hyper-V Storage QoS integration on the storage layer.
4. **Distributed File System (DFS) referrals.** DFS namespace referrals
   require AD-integrated namespace coordination, deprecated in favor of
   per-share access in DittoFS.
5. **Named Pipe FSA (WPTS-internal).** The WPTS FSA Adapter Connects to
   the SUT via SSH to drive the FSCC layer directly; the Docker harness does
   not expose SSH to the DittoFS container, so the test cannot run regardless
   of pipe implementation status.

These entries remain in CI's known-failure set (so they don't break the build)
but are explicitly outside the v1.0 conformance gate.

| Test Name | Category | Reason |
|-----------|----------|--------|
| BVT_ApplySnapshot | VHD/RSVD | RSVD shared VHD snapshot apply — block-level VHD storage not implemented |
| BVT_ChangeTracking | VHD/RSVD | RSVD VHD block change tracking — block-level VHD storage not implemented |
| BVT_Convert_VHDFile_to_VHDSetFile | VHD/RSVD | RSVD VHD → VHDSet conversion — block-level VHD storage not implemented |
| BVT_Create_Delete_Checkpoint | VHD/RSVD | RSVD VHD checkpoint lifecycle — block-level VHD storage not implemented |
| BVT_Extract_VHDSet | VHD/RSVD | RSVD VHDSet extraction — block-level VHD storage not implemented |
| BVT_OpenCloseSharedVHD_V1 | VHD/RSVD | RSVD v1 shared-VHD open/close — block-level VHD storage not implemented |
| BVT_OpenCloseSharedVHD_V2 | VHD/RSVD | RSVD v2 shared-VHD open/close — block-level VHD storage not implemented |
| BVT_OpenSharedVHDSetByTargetSpecifier | VHD/RSVD | RSVD VHDSet target-specifier open — block-level VHD storage not implemented |
| BVT_Optimize | VHD/RSVD | RSVD VHD optimize — block-level VHD storage not implemented |
| BVT_QuerySharedVirtualDiskSupport | VHD/RSVD | RSVD shared-VHD support query — block-level VHD storage not implemented |
| BVT_QueryVirtualDiskChanges | VHD/RSVD | RSVD VHD changed-block tracking — block-level VHD storage not implemented |
| BVT_Query_VHDSet_FileInfo_SnapshotEntry | VHD/RSVD | RSVD VHDSet snapshot entry query — block-level VHD storage not implemented |
| BVT_Query_VHDSet_FileInfo_SnapshotList | VHD/RSVD | RSVD VHDSet snapshot list query — block-level VHD storage not implemented |
| BVT_ReadSharedVHD | VHD/RSVD | RSVD shared-VHD read — block-level VHD storage not implemented |
| BVT_Resize | VHD/RSVD | RSVD VHD resize — block-level VHD storage not implemented |
| BVT_TunnelCheckConnectionStatusToSharedVHD | VHD/RSVD | RSVD SCSI tunnel — block-level VHD storage not implemented |
| BVT_TunnelGetDiskInfoToSharedVHD | VHD/RSVD | RSVD SCSI tunnel — block-level VHD storage not implemented |
| BVT_TunnelGetFileInfoToSharedVHD | VHD/RSVD | RSVD SCSI tunnel — block-level VHD storage not implemented |
| BVT_TunnelSCSIPersistentReserve_Preempt | VHD/RSVD | RSVD SCSI persistent reserve — block-level VHD storage not implemented |
| BVT_TunnelSCSIPersistentReserve_RegisterAndReserve | VHD/RSVD | RSVD SCSI persistent reserve — block-level VHD storage not implemented |
| BVT_TunnelSCSIPersistentReserve_ReserveAndRelease | VHD/RSVD | RSVD SCSI persistent reserve — block-level VHD storage not implemented |
| BVT_TunnelSCSIPersistentReserve_ReserveConflict | VHD/RSVD | RSVD SCSI persistent reserve — block-level VHD storage not implemented |
| BVT_TunnelSCSIToSharedVHD | VHD/RSVD | RSVD SCSI tunnel — block-level VHD storage not implemented |
| BVT_TunnelSRBStatusToSharedVHD | VHD/RSVD | RSVD SCSI SRB status tunnel — block-level VHD storage not implemented |
| BVT_TunnelValidateDiskToSharedVHD | VHD/RSVD | RSVD SCSI disk validation tunnel — block-level VHD storage not implemented |
| BVT_WriteSharedVHD | VHD/RSVD | RSVD shared-VHD write — block-level VHD storage not implemented |
| BVT_SWNGetInterfaceList_ClusterSingleNode | SWN | Service Witness Protocol — requires multi-node clustered file server |
| BVT_SWNGetInterfaceList_ScaleOutSingleNode | SWN | Service Witness Protocol — requires multi-node clustered file server |
| BVT_SWN_CheckProtocolVersion | SWN | Service Witness Protocol — requires multi-node clustered file server |
| BVT_WitnessrRegister_SWNAsyncNotification_ClientMove | SWN | Service Witness Protocol — requires multi-node clustered file server |
| BVT_WitnessrRegisterEx_SWNAsyncNotification_ClientMove | SWN | Service Witness Protocol — requires multi-node clustered file server |
| BVT_WitnessrRegisterEx_SWNAsyncNotification_IPChange | SWN | Service Witness Protocol — requires multi-node clustered file server |
| BVT_Sqos_ProbePolicy | SQoS | Storage QoS — requires Hyper-V QoS integration on the storage layer |
| BVT_Sqos_SetPolicy | SQoS | Storage QoS — requires Hyper-V QoS integration on the storage layer |
| BVT_Sqos_UpdateCounters | SQoS | Storage QoS — requires Hyper-V QoS integration on the storage layer |
| BVT_RootAndLinkReferralDomainV4ToDFSServer | DFS | DFS namespace referrals — AD-integrated namespace not implemented |
| BVT_RootAndLinkReferralStandaloneV4ToDFSServer | DFS | DFS namespace referrals — AD-integrated namespace not implemented |
| BVT_FileAccess_OpenNamedPipe | NamedPipe | WPTS FSA adapter requires SSH to SUT — not available in Docker harness |
| BVT_FileAccess_OpenNamedPipe_InvalidPathName | NamedPipe | WPTS FSA adapter requires SSH to SUT — not available in Docker harness |

**Total permanently unimplementable: 39 tests.**

## Out-of-Scope Categories Summary

| Category | Count |
|----------|-------|
| VHD/RSVD | 26 |
| SWN | 6 |
| SQoS | 3 |
| DFS | 2 |
| NamedPipe | 2 |

## Phase 33-39 Improvements

The following SMB3 features were implemented in Phases 33-39:

- **Phase 30:** Bug fixes (sparse READ, renamed dir listing, parent dir navigation, oplock break wiring, NumberOfLinks, pipe share caching).
- **Phase 31:** Windows ACL support (DACL synthesis, SD-01..SD-08).
- **Phase 32:** MxAc / QFid / FileCompressionInformation / FileAttributeTagInformation create contexts.
- **Phase 33:** SMB3 encryption (AES-128/256-CCM/GCM, VALIDATE_NEGOTIATE_INFO).
- **Phase 34:** SMB3 signing (AES-CMAC, AES-GMAC, HMAC-SHA256, SP800-108 KDF).
- **Phase 35-37:** Lease V2, session binding/reconnect, Kerberos via SPNEGO/GSSAPI.
- **Phase 38:** Durable handles V1 + V2 (DHnQ/DHnC, DH2Q/DH2C).
- **Phase 39:** Cross-protocol caching (SMB leases + NFS delegations).

## Phase 72-73 Improvements

- **v0.10.0 Phase 72:** ChangeNotify (async + CANCEL + completion filters), client-preference cipher/signing selection, DH V1 volatile FileID regen, TREE_DISCONNECT signing exemption, lease V1/V2 fixes, timestamp freeze/unfreeze per MS-FSA 2.1.5.14.2.
- **v0.10.0 Phase 73:** ChangeNotify ADS stream notifications, ADS share access + timestamp conformance, ChangeNotify completion, session re-auth, anonymous encryption, DH/lease state machine, per-field CreationTime freeze/unfreeze.

## How to Add New Entries

After running the test suite, `parse-results.sh` will report new failures not
in this table. To add them:

1. Copy the exact test name from the output
2. **Investigate the failure** — determine if the feature is implemented
3. If the feature IS implemented: fix the bug, do NOT add to this list
4. If the feature requires a feature not yet implemented but planned: add to "Expected Failures" with an issue link
5. If the feature is architecturally out of scope: add to the "Permanently Unimplementable" appendix with a one-line reason

Format for Expected Failures:
```
| ExactTestName | Category | Reason for expected failure | Expected | #issue or Phase N |
```

Format for Permanently Unimplementable:
```
| ExactTestName | Category | Architectural reason it cannot be implemented |
```

## Changelog

- **#673 (2026-06-02, dirlease removal):** Removed `BVT_DirectoryLeasing_ReadWriteHandleCaching` from the known-failure list. Verified by running the WPTS FileServer suite against current develop (memory profile) with a byte-level pcap on the SMB wire: the test PASSES deterministically (4/4 — once in the full DirectoryLeasing class, three more in isolation). The "Flaky" classification was refuted. The wire trace is fully MS-SMB2 conformant: the directory lease is granted as Read|Handle (the Write bit is correctly dropped on a directory grant, matching the passing `RWGrantedAsR` / `RWHGrantedAsRH` cases); on a conflicting child create the server sends a Lease Break Notification (Flags=Break Ack Required, Current=0x03 Read|Handle, New=0x00) and processes the client's signed Lease Break Acknowledgment with STATUS_SUCCESS — the canonical §2.2.23.2/§2.2.24.1 handshake. The only error seen in the capture, STATUS_DIRECTORY_NOT_EMPTY (0xc0000101), occurs exclusively on framework cleanup CLOSE operations, which `CommonTestBase.TestCleanup()` wraps in an exception-swallowing `try { } catch { }` and therefore cannot affect the test outcome. Tally: Flaky 1→0, Total 40→39. CI: since `parse-results.sh` only counts `Failed`/`Error` outcomes, a passing test that was previously listed had no gate effect; removing it makes any genuine future regression register as a New Failure.
- **#673 (2026-06-02):** Docs-only rationalization. Added a Final Tally header block. Confirmed every entry is bucketed and justified.
- **Wave 4 (2026-05-28):** Walk back 4 confirmed PASS. Three were already passing on develop: `Algorithm_NotingFileModified_Dir_LastAccessTime`, `FileInfo_Set_FileBasicInformation_Timestamp_MinusTwo_Dir_LastWriteTime`, `BVT_DirectoryLeasing_LeaseBreakOnMultiClients`. The fourth, `FileInfo_Set_FileBasicInformation_Timestamp_MinusOne_Dir_ChangeTime`, was flipped by an ADS-write fix that preserves the base object's frozen ChangeTime — previously, file_modify.go auto-bumped Ctime to NOW whenever `modified=true && attrs.Ctime==nil`, clobbering the freeze even when only the ADS handle's Mtime was unfrozen. Restructure with Permanently Unimplementable appendix mirroring the smbtorture file layout.
- **v0.10.0 Phase 73 (2026-03-24):** SMB Conformance Deep-Dive. Plan 01: ChangeNotify ADS stream notifications wired (5 tests). Plan 02: ADS share access + timestamp conformance (9 ADS + 3 timestamp tests removed). Plan 03: ChangeNotify completion, session re-auth, anonymous encryption (~25 smbtorture tests). Plan 04: DH/lease state machine fixes (~26 smbtorture tests). Plan 05: Per-field CreationTime freeze/unfreeze, ChangeEa reclassified as Permanent. Total: 56 (53 permanent + 3 expected).
- **v0.10.0 Phase 72 (2026-03-23):** ChangeNotify fully implemented with async responses, CANCEL support, and all MS-SMB2 completion filter events (Plan 01, 16 tests fixed). Client-preference cipher/signing selection, DH V1 volatile FileID regeneration, TREE_DISCONNECT signing exemption, lease V1/V2 state transitions fixed (Plan 02, 12 tests fixed). Timestamp freeze/unfreeze per MS-FSA 2.1.5.14.2, parent directory atime on file write (Plan 03, 3 tests fixed). Total removed: 31. New total: 65 (52 permanent + 13 expected).
- **v4.5 Phase 69 (2026-03-20):** Full MS-SMB2 3.3.x signing audit completed. Added spec section references (3.3.5.2.4, 3.3.4.1.1, 3.3.5.2.7.2) to framing.go, response.go, compound.go. Enforced NegSigningRequired for 3.1.1 NEGOTIATE and SigningRequired for 3.1.1 SESSION_SETUP. All signing paths verified compliant.
- **v4.7 Phase 67 (2026-03-20):** SMB 3.1.1 preauth integrity hash chain verified correct via MS-SMB2 test vectors and conformance tests.
- **v3.8 Phase 42 (2026-03-09):** Updated ptfconfig to SMB 3.1.1. Added 14 newly exercised tests.
- **v3.8 Phase 40 (2026-03-02):** Post-SMB3 update. Removed 5 tests whose features are now implemented (durable handles V1, leasing V1, oplock break, encryption capability flag).
- **v3.6 Phase 32 (2026-02-28):** Updated baseline after bug fixes (sparse READ, directory listing, parent dir, oplock break, link count), ACL support, and protocol enhancements.
- **v3.6 Phase 29.8 (2026-02-26):** Initial baseline (133/240 BVT tests passing). Created expected failure list with 90 entries across 14 categories.
