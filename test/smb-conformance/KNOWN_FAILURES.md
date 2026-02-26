# Known Failures - SMB Conformance (WPTS BVT)

Tests listed here are expected to fail. CI will pass (exit 0) as long as
all failures are in this list. New failures not listed here will cause CI to fail.

The `parse-results.sh` script reads test names from the first column of the
table below. Lines starting with `#`, `|---`, empty lines, and the header
row (`Test Name`) are ignored.

<!-- Populate this table after the first WPTS run. Use `./run.sh --dry-run`
     to verify configuration, then run the full suite and check the TRX output
     for exact test names. The names below are representative patterns based
     on WPTS BVT test suite analysis. Update with actual test names after the
     first successful run. -->

## Expected Failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| BVT_SMB2Negotiate_SMB302 | Negotiate | DittoFS supports SMB 2.1 only; SMB 3.0.2 negotiation not implemented | Phase 39 |
| BVT_SMB2Negotiate_SMB311 | Negotiate | DittoFS supports SMB 2.1 only; SMB 3.1.1 negotiation not implemented | Phase 39 |
| BVT_SMB2Negotiate_SMB300 | Negotiate | DittoFS supports SMB 2.1 only; SMB 3.0 negotiation not implemented | Phase 39 |
| BVT_SMBEncryption_SMB302 | Encryption | SMB3 encryption not implemented | Phase 39 |
| BVT_SMBEncryption_SMB311 | Encryption | SMB3 encryption not implemented | Phase 39 |
| BVT_MultiChannel | MultiChannel | Multi-channel not supported | - |
| BVT_DurableHandleV2_Reconnect | DurableHandle | Durable handles v2 not implemented | Phase 42 |
| BVT_DurableHandleV2_Persistent | DurableHandle | Persistent handles not implemented | Phase 42 |
| BVT_SMB2Negotiate_QUIC | Transport | QUIC transport not supported | - |

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
