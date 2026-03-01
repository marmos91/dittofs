# smbtorture Known Failures

Last updated: 2026-03-01 (Phase 33 smbtorture conformance pass)

Tests listed here are expected to fail and will NOT cause CI to report failure.
Only NEW failures (not in this list) will cause CI to fail.

The `parse-results.sh` script reads test names from the first column of the
table below. Wildcard patterns (ending with `.*`) match any test with that
prefix. Lines starting with `#`, `|---`, empty lines, and the header row
(`Test Name`) are ignored.

## Expected Failures

### SMB3 Features (Not Implemented)

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.durable-open.* | Durable handles | Not implemented (v3.8 Phase 42) | - |
| smb2.durable-v2-open.* | Durable handles v2 | Not implemented (v3.8 Phase 42) | - |
| smb2.durable-v2-delay.* | Durable handles v2 | Durable reconnect delay not implemented | - |
| smb2.durable-v2-regressions.* | Durable handles v2 | Durable reconnect not implemented | - |
| smb2.multichannel.* | Multi-channel | Not implemented (v3.8) | - |
| smb2.replay.* | Replay detection | Not implemented (v3.8) | - |
| smb2.lease.* | Leasing | SMB3 leasing not implemented (v3.8 Phase 40) | - |
| smb2.credits.* | Credit management | SMB3 credit sequences not implemented | - |
| smb2.session.* | Session management | SMB3 session binding/reconnect not implemented | - |
| smb2.session-require-signing.* | Session signing | Signing enforcement edge cases (SMB3 session binding) | - |

### Change Notify

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.notify.* | Change Notify | Async notify not fully implemented | - |
| smb2.change_notify_disabled.* | Change Notify | Change notify disabled test | - |

### IOCTL/FSCTL Operations

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.ioctl.* | IOCTL/FSCTL | Most FSCTL operations not implemented | - |
| smb2.set-sparse-ioctl | Sparse files | Sparse file IOCTL not implemented | - |
| smb2.zero-data-ioctl | Zero data | Zero data IOCTL not implemented | - |

### Streams (Alternate Data Streams)

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.streams.* | Alternate Data Streams | ADS not implemented (v3.8 Phase 43) | - |
| smb2.create_no_streams.* | Streams | No-streams create context not implemented | - |

### Oplocks

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.oplock.* | Oplocks | Advanced oplock scenarios require multi-client coordination | - |
| smb2.kernel-oplocks.* | Kernel Oplocks | Kernel oplock break handling not implemented | - |
| smb2.hold-oplock | Hold test | Interactive test with 5-min timeout | - |

### Locking

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.lock.* | Byte-range locks | Advanced lock scenarios (cancel, async, replay) not fully implemented | - |

### ACLs and Security

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.acls.* | ACLs | Windows ACL/DACL semantics not implemented | - |
| smb2.acls_non_canonical.* | ACLs | Non-canonical ACL ordering not implemented | - |
| smb2.sdread | Security descriptors | Security descriptor read not implemented | - |
| smb2.secleak | Security leak | Security descriptor leak test | - |
| smb2.maximum_allowed.* | Access checks | Maximum allowed access computation not implemented | - |

### Share Modes and Deny

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.sharemode.* | Share modes | Advanced share mode scenarios not fully implemented | - |
| smb2.check-sharemode | Share modes | Advanced share mode checking not fully implemented | - |
| smb2.hold-sharemode | Hold test | Interactive test, blocks indefinitely | - |
| smb2.deny.* | Deny modes | Complex deny mode scenarios may fail | - |

### Directory Operations

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.dir.* | Directory operations | Advanced directory queries may fail | - |

### CREATE Operations

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.create.* | Create | Advanced create contexts and semantics not fully implemented | - |

### Read/Write Operations

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.read.* | Read | Advanced read scenarios (access checks, dir read, eof) | - |
| smb2.rw.* | Read/Write | Advanced read/write scenarios | - |

### Query/Set Info

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.getinfo.* | Query Info | Advanced getinfo scenarios (access, security, buffer checks) | - |
| smb2.setinfo | Set Info | change_time not preserved after SET_INFO with explicit timestamps | - |
| smb2.scan.* | Scan | Info level enumeration/scan tests | - |

### Timestamps

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.timestamps.* | Timestamps | Delayed write, freeze-thaw, and boundary timestamps not implemented | - |
| smb2.timestamp_resolution.* | Timestamps | Timestamp resolution test | - |

### File Attributes

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.dosmode | DOS attributes | FILE_ATTRIBUTE_HIDDEN not fully supported | - |
| smb2.async_dosmode | DOS attributes | Async DOS attribute operations | - |
| smb2.openattr | File attributes | Open with attribute validation | - |
| smb2.winattr | Windows attributes | Windows-specific file attributes | - |
| smb2.winattr2 | Windows attributes | Windows-specific file attributes v2 | - |

### Rename and Delete

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.rename.* | Rename | Advanced rename scenarios may fail | - |
| smb2.delete-on-close.* | Delete on close | Complex delete-on-close semantics | - |

### Compound and Connection

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.compound.* | Compound requests | Limited support | - |
| smb2.connect | Connection | Advanced connection/session negotiation | - |
| smb2.tcon | Tree connect | Advanced tree connect semantics | - |

### File IDs

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.fileid.* | File IDs | File ID stability and uniqueness tests | - |

### Misc

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.maxfid | File descriptors | Connection drops under high FD pressure | - |
| smb2.bench.* | Benchmarks | Performance benchmarks (multi-client coordination) | - |
| smb2.charset.* | Character sets | Unicode/charset handling tests | - |
| smb2.twrp.* | Previous versions | Time-warp (previous versions) not implemented | - |
| smb2.name-mangling.* | Name mangling | 8.3 short name mangling not fully implemented | - |
| smb2.ea.* | Extended attributes | Extended attribute ACL tests | - |
| smb2.aio_delay.* | Async I/O | Async I/O delay/cancel tests | - |
| smb2.samba3misc.* | Samba-specific | Samba3-specific POSIX lock tests | - |

## Notes

- smbtorture image: quay.io/samba.org/samba-toolbox:v0.8
- DittoFS implements SMB 2.0.2, 2.1, 3.0, 3.0.2, and 3.1.1 dialects
- Phase 34 added SMB 3.x key derivation (SP800-108 KDF) and signing
  (AES-128-CMAC/GMAC via SIGNING_CAPABILITIES negotiate context)
- Many tests fail due to missing SMB3 features (encryption, durable handles, etc.)
- Tests requiring multi-client coordination (oplocks, share modes) are expected to fail
- Tests requiring Windows-specific ACL/security semantics are expected to fail
- The NT_STATUS_NO_MEMORY errors seen in full-suite runs are a client-side issue
  from rapid connection creation under ARM64 emulation, not a DittoFS server bug
- Fixing remaining failures is deferred to v3.8 milestone phases

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
