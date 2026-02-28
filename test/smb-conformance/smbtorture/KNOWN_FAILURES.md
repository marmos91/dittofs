# smbtorture Known Failures

Last updated: 2026-02-28 (Phase 32 initial baseline)

Tests listed here are expected to fail and will NOT cause CI to report failure.
Only NEW failures (not in this list) will cause CI to fail.

The `parse-results.sh` script reads test names from the first column of the
table below. Wildcard patterns (ending with `.*`) match any test with that
prefix. Lines starting with `#`, `|---`, empty lines, and the header row
(`Test Name`) are ignored.

## Expected Failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.durable-open.* | Durable handles | Not implemented (v3.8 Phase 42) | - |
| smb2.durable-v2-open.* | Durable handles v2 | Not implemented (v3.8 Phase 42) | - |
| smb2.multichannel.* | Multi-channel | Not implemented (v3.8) | - |
| smb2.replay.* | Replay detection | Not implemented (v3.8) | - |
| smb2.notify.* | Change Notify | Not implemented (v3.8 Phase 40.5) | - |
| smb2.compound.* | Compound requests | Limited support | - |
| smb2.session.* | Session management | SMB3 session features not implemented | - |
| smb2.lease.* | Leasing | SMB3 leasing not implemented (v3.8 Phase 40) | - |
| smb2.credits.* | Credit management | SMB3 credit sequences not implemented | - |
| smb2.ioctl.* | IOCTL/FSCTL | Most FSCTL operations not implemented | - |
| smb2.streams.* | Alternate Data Streams | ADS not implemented (v3.8 Phase 43) | - |
| smb2.rename.* | Rename | Advanced rename scenarios may fail | - |
| smb2.delete-on-close.* | Delete on close | Complex delete-on-close semantics | - |
| smb2.dir.* | Directory operations | Advanced directory queries may fail | - |
| smb2.dosmode | DOS attributes | FILE_ATTRIBUTE_HIDDEN not supported | - |
| smb2.async_dosmode | DOS attributes | FILE_ATTRIBUTE_HIDDEN not supported | - |
| smb2.maxfid | File descriptors | Connection drops under high FD pressure | - |
| smb2.hold-sharemode | Hold test | Interactive test, blocks indefinitely | - |
| smb2.hold-oplock | Hold test | Interactive test with 5-min timeout | - |
| smb2.connect | Connection | Advanced connection/session negotiation | - |
| smb2.setinfo | Set info | Advanced set-info operations not implemented | - |
| smb2.set-sparse-ioctl | Sparse files | Sparse file IOCTL not implemented | - |
| smb2.zero-data-ioctl | Zero data | Zero data IOCTL not implemented | - |

## Notes

- smbtorture image: quay.io/samba.org/samba-toolbox:v0.8
- DittoFS implements SMB 2.0.2/2.1 (not SMB 3.x yet)
- Many tests may fail due to missing SMB3 features
- Fixing failures is deferred to v3.8 milestone
- Update this file after first smbtorture run with actual test names

## How to Add New Entries

After running the test suite, `parse-results.sh` will report new failures not
in this table. To add them:

1. Copy the exact test name from the output
2. Determine the failure category and reason
3. Add a row to the table above (use `.*` suffix for category-wide patterns)
4. Reference the relevant GitHub issue or future phase

Format:
```
| ExactTestName | Category | Reason for expected failure | #issue or Phase N |
```
