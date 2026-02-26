# Known Failures - SMB Conformance (WPTS BVT)

Tests listed here are expected to fail. CI will pass (exit 0) as long as
all failures are in this list. New failures not listed here will cause CI to fail.

The `parse-results.sh` script reads test names from the first column of the
table below. Lines starting with `#`, `|---`, empty lines, and the header
row (`Test Name`) are ignored.

## Expected Failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| BVT_Negotiate_SMB2002 | Negotiate | SMB 2.0.2 negotiation edge case | - |
| BVT_Negotiate_SMB21 | Negotiate | SMB 2.1 negotiate test expects features not implemented | - |
| BVT_Negotiate_SMB30 | Negotiate | SMB 3.0 not implemented | Phase 39 |
| BVT_Negotiate_SMB302 | Negotiate | SMB 3.0.2 not implemented | Phase 39 |
| BVT_Negotiate_SMB311 | Negotiate | SMB 3.1.1 not implemented | Phase 39 |
| BVT_Negotiate_SMB311_Preauthentication | Negotiate | SMB 3.1.1 preauthentication not implemented | Phase 39 |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_CCM | Negotiate | SMB 3.1.1 encryption (AES-128-CCM) not implemented | Phase 39 |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_GCM | Negotiate | SMB 3.1.1 encryption (AES-128-GCM) not implemented | Phase 39 |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_AES_256_CCM | Negotiate | SMB 3.1.1 encryption (AES-256-CCM) not implemented | Phase 39 |
| BVT_Negotiate_SMB311_Preauthentication_Encryption_AES_256_GCM | Negotiate | SMB 3.1.1 encryption (AES-256-GCM) not implemented | Phase 39 |
| BVT_Negotiate_SMB311_CompressionEnabled | Negotiate | SMB 3.1.1 compression not implemented | - |
| BVT_Negotiate_SMB311_Compression_IsChainedCompressionSupported | Negotiate | SMB 3.1.1 chained compression not implemented | - |
| BVT_Negotiate_SMB311_RdmaTransformCapabilities_IsSupported | Negotiate | RDMA transform not supported | - |
| BVT_Negotiate_Compatible_2002 | Negotiate | Multi-protocol negotiation not fully compatible | - |
| BVT_Negotiate_Compatible_Wildcard | Negotiate | Wildcard dialect negotiation not supported | - |
| BVT_Negotiate_SigningEnabled | Negotiate | Signing negotiation not fully conformant | - |
| Negotiate_SMB311_IsServerToClientNotificationsSupported | Negotiate | Server-to-client notifications not supported | - |
| Negotiate_SMB311_IsTransportCapabilitiesSupported | Negotiate | Transport capabilities context not supported | - |
| BVT_Signing | Signing | Message signing not fully conformant | - |
| BVT_Encryption_SMB311 | Encryption | SMB 3.1.1 encryption not implemented | Phase 39 |
| BVT_Encryption_SMB311_CCM | Encryption | AES-128-CCM encryption not implemented | Phase 39 |
| BVT_Encryption_SMB311_GCM | Encryption | AES-128-GCM encryption not implemented | Phase 39 |
| BVT_Encryption_SMB311_AES_256_CCM | Encryption | AES-256-CCM encryption not implemented | Phase 39 |
| BVT_Encryption_SMB311_AES_256_GCM | Encryption | AES-256-GCM encryption not implemented | Phase 39 |
| BVT_Encryption_GlobalEncryptionEnabled | Encryption | Global encryption not implemented | Phase 39 |
| BVT_Encryption_PerShareEncryptionEnabled | Encryption | Per-share encryption not implemented | Phase 39 |
| BVT_DirectoryLeasing_LeaseBreakOnMultiClients | Leasing | Directory leasing not supported | - |
| BVT_DCReferralV3ToDC | DFS | DFS referrals not implemented | - |
| BVT_RootReferralV4ToDC | DFS | DFS referrals not implemented | - |
| BVT_DomainReferralV3ToDC | DFS | DFS referrals not implemented | - |
| BVT_SysvolReferralv4ToDCSysvolPath | DFS | DFS SYSVOL referrals not implemented | - |
| BVT_SysvolReferralV3ToDCNetlogonPath | DFS | DFS Netlogon referrals not implemented | - |
| BVT_RootAndLinkReferralStandaloneV4ToDFSServer | DFS | DFS standalone referrals not implemented | - |
| BVT_RootAndLinkReferralDomainV4ToDFSServer | DFS | DFS domain referrals not implemented | - |
| BVT_SWN_CheckProtocolVersion | SWN | Service Witness Protocol not implemented | - |
| BVT_SWNGetInterfaceList_ClusterSingleNode | SWN | Service Witness Protocol not implemented | - |
| BVT_SWNGetInterfaceList_ScaleOutSingleNode | SWN | Service Witness Protocol not implemented | - |
| BVT_WitnessrRegister_SWNAsyncNotification_ClientMove | SWN | Service Witness Protocol not implemented | - |
| BVT_WitnessrRegisterEx_SWNAsyncNotification_ClientMove | SWN | Service Witness Protocol not implemented | - |
| BVT_WitnessrRegisterEx_SWNAsyncNotification_IPChange | SWN | Service Witness Protocol not implemented | - |
| BVT_VSSOperateShadowCopySet_WritableSnapshot_GeneralFileServer | VSS | Volume Shadow Copy not implemented | - |
| BVT_VSSOperateShadowCopySet_WritableSnapshot_ScaleoutFileServer | VSS | Volume Shadow Copy not implemented | - |
| BVT_VSSOperateShadowCopySet_ClusterSharePath_OwnerNode | VSS | Volume Shadow Copy not implemented | - |
| BVT_VSSOperateShadowCopySet_ClusterSharePath_NonOwnerNode | VSS | Volume Shadow Copy not implemented | - |

## How to Add New Entries

After running the test suite, `parse-results.sh` will report new failures not
in this table. To add them:

1. Copy the exact test name from the output
2. Determine the failure category and reason
3. Add a row to the table above
4. Reference the relevant GitHub issue or future phase

Format:
```
| ExactTestName | Category | Reason for expected failure | #issue or Phase N |
```
