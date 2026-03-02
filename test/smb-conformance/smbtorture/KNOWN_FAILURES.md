# smbtorture Known Failures

Last updated: 2026-03-02 (Phase 40-01 baseline re-measurement, post phases 33-39)

Tests listed here are expected to fail and will NOT cause CI to report failure.
Only NEW failures (not in this list) will cause CI to fail.

The `parse-results.sh` script reads test names from the first column of the
table below. Lines starting with `#`, `|---`, empty lines, and the header row
(`Test Name`) are ignored.

Every entry has been individually verified against the smbtorture baseline run
of 2026-03-02 (commit 52f84ecd). Only tests that fail due to genuinely
unimplemented features are listed. Tests for implemented features (sessions,
leases, durable handles) that still fail are tracked as fix candidates in
`baseline-results.md`, NOT here.

## Expected Failures

### Multi-Channel (Not Implemented)

Multi-channel support requires establishing multiple TCP connections to the same
session, which DittoFS does not implement.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.multichannel.bugs.bug_15346 | Multi-channel | Multi-channel not implemented | - |
| smb2.multichannel.generic.num_channels | Multi-channel | Multi-channel not implemented | - |

### ACLs and Security Descriptors (Not Implemented)

DittoFS uses POSIX permission model. Windows ACL/DACL/SACL semantics, security
descriptors, and owner rights are not implemented.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.acls.ACCESSBASED | ACLs | Windows ACL semantics not implemented | - |
| smb2.acls.CREATOR | ACLs | Creator SID semantics not implemented | - |
| smb2.acls.DENY1 | ACLs | ACL deny semantics not implemented | - |
| smb2.acls.DYNAMIC | ACLs | Dynamic access checks not implemented | - |
| smb2.acls.GENERIC | ACLs | Generic ACL mapping not implemented | - |
| smb2.acls.INHERITANCE | ACLs | ACL inheritance not implemented | - |
| smb2.acls.INHERITFLAGS | ACLs | ACL inherit flags not implemented | - |
| smb2.acls.MXAC-NOT-GRANTED | ACLs | Maximum access not-granted not implemented | - |
| smb2.acls.OVERWRITE_READ_ONLY_FILE | ACLs | ACL overwrite read-only not implemented | - |
| smb2.acls.OWNER | ACLs | Owner SID semantics not implemented | - |
| smb2.acls.OWNER-RIGHTS | ACLs | Owner rights not implemented | - |
| smb2.acls.OWNER-RIGHTS-DENY | ACLs | Owner rights deny not implemented | - |
| smb2.acls.OWNER-RIGHTS-DENY1 | ACLs | Owner rights deny not implemented | - |
| smb2.acls.SDFLAGSVSCHOWN | ACLs | SD flags vs chown not implemented | - |
| smb2.acls_non_canonical.flags | ACLs | Non-canonical ACL ordering not implemented | - |
| smb2.sdread | Security descriptors | Security descriptor read not implemented | - |
| smb2.secleak | Security descriptors | Security descriptor leak test not implemented | - |

### IOCTL/FSCTL Operations (Not Implemented)

Server-side copy (SRV_COPYCHUNK), sparse file operations, compression, and most
FSCTL operations are not implemented. Only shadow_copy enumeration and
sparse_file_attr query work.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.ioctl.bug14769 | IOCTL | IOCTL edge case not implemented | - |
| smb2.ioctl.compress_notsup_get | IOCTL | Compression not implemented | - |
| smb2.ioctl.compress_notsup_set | IOCTL | Compression not implemented | - |
| smb2.ioctl.copy_chunk_across_shares | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_across_shares2 | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_across_shares3 | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_append | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_bad_access | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_bad_key | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_bug15644 | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_dest_lock | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_limits | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_max_output_sz | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_multi | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_overwrite | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_simple | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_sparse_dest | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_src_exceed | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_src_exceed_multi | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_src_is_dest | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_src_is_dest_overlap | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_src_lock | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_tiny | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_write_access | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy_chunk_zero_length | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.copy-chunk | IOCTL | Server-side copy not implemented | - |
| smb2.ioctl.req_resume_key | IOCTL | Resume key for server-side copy not implemented | - |
| smb2.ioctl.req_two_resume_keys | IOCTL | Resume key for server-side copy not implemented | - |
| smb2.ioctl.sparse_copy_chunk | IOCTL | Sparse + server-side copy not implemented | - |
| smb2.ioctl.sparse_dir_flag | IOCTL | Sparse file semantics not implemented | - |
| smb2.ioctl.sparse_file_flag | IOCTL | Sparse file semantics not implemented | - |
| smb2.ioctl.sparse_hole_dealloc | IOCTL | Sparse file hole deallocation not implemented | - |
| smb2.ioctl.sparse_lock | IOCTL | Sparse file locking not implemented | - |
| smb2.ioctl.sparse_perms | IOCTL | Sparse file permissions not implemented | - |
| smb2.ioctl.sparse_punch | IOCTL | Sparse file hole punching not implemented | - |
| smb2.ioctl.sparse_punch_invalid | IOCTL | Sparse file hole punching not implemented | - |
| smb2.ioctl.sparse_qar | IOCTL | Sparse query allocated ranges not implemented | - |
| smb2.ioctl.sparse_qar_malformed | IOCTL | Sparse query allocated ranges not implemented | - |
| smb2.ioctl.sparse_qar_multi | IOCTL | Sparse query allocated ranges not implemented | - |
| smb2.ioctl.sparse_qar_ob1 | IOCTL | Sparse query allocated ranges not implemented | - |
| smb2.ioctl.sparse_qar_overflow | IOCTL | Sparse query allocated ranges not implemented | - |
| smb2.ioctl.sparse_qar_truncated | IOCTL | Sparse query allocated ranges not implemented | - |
| smb2.ioctl.sparse_set_nobuf | IOCTL | Sparse file set not implemented | - |
| smb2.ioctl.sparse_set_oversize | IOCTL | Sparse file set not implemented | - |
| smb2.ioctl-on-stream | IOCTL | IOCTL on ADS not implemented | - |
| smb2.set-sparse-ioctl | IOCTL | Sparse file IOCTL not implemented | - |
| smb2.zero-data-ioctl | IOCTL | Zero data IOCTL not implemented | - |

### Alternate Data Streams (Not Implemented)

ADS (Alternate Data Streams / named streams) are a Windows NTFS feature not
applicable to DittoFS's virtual filesystem. Only basic stream rename and
share modes pass due to the stub implementation.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.streams.attributes1 | Streams | ADS attributes not implemented | - |
| smb2.streams.attributes2 | Streams | ADS attributes not implemented | - |
| smb2.streams.basefile-rename-with-open-stream | Streams | ADS rename semantics not implemented | - |
| smb2.streams.create-disposition | Streams | ADS create disposition not implemented | - |
| smb2.streams.delete | Streams | ADS delete not implemented | - |
| smb2.streams.dir | Streams | ADS directory listing not implemented | - |
| smb2.streams.io | Streams | ADS I/O not implemented | - |
| smb2.streams.names | Streams | ADS name enumeration not implemented | - |
| smb2.streams.names2 | Streams | ADS name enumeration not implemented | - |
| smb2.streams.names3 | Streams | ADS name enumeration not implemented | - |
| smb2.streams.rename2 | Streams | ADS rename semantics not implemented | - |
| smb2.streams.zero-byte | Streams | ADS zero-byte handling not implemented | - |
| smb2.create_no_streams.no_stream | Streams | No-streams create context not implemented | - |

### Change Notify (Not Fully Implemented)

Async change notification requires background notification infrastructure.
Only basic file-level notify works.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.notify.basedir | Change Notify | Async directory notify not implemented | - |
| smb2.notify.close | Change Notify | Notify on close not implemented | - |
| smb2.notify.dir | Change Notify | Directory notify not implemented | - |
| smb2.notify.double | Change Notify | Double notify not implemented | - |
| smb2.notify.handle-permissions | Change Notify | Notify permission checks not implemented | - |
| smb2.notify.invalid-reauth | Change Notify | Notify reauth handling not implemented | - |
| smb2.notify.logoff | Change Notify | Notify on logoff not implemented | - |
| smb2.notify.mask | Change Notify | Notify mask filtering not implemented | - |
| smb2.notify.mask-change | Change Notify | Notify mask change not implemented | - |
| smb2.notify.overflow | Change Notify | Notify buffer overflow not implemented | - |
| smb2.notify.rec | Change Notify | Recursive notify not implemented | - |
| smb2.notify.rmdir1 | Change Notify | Notify on rmdir not implemented | - |
| smb2.notify.rmdir2 | Change Notify | Notify on rmdir not implemented | - |
| smb2.notify.rmdir3 | Change Notify | Notify on rmdir not implemented | - |
| smb2.notify.rmdir4 | Change Notify | Notify on rmdir not implemented | - |
| smb2.notify.session-reconnect | Change Notify | Notify session reconnect not implemented | - |
| smb2.notify.tcon | Change Notify | Notify on tree connect not implemented | - |
| smb2.notify.tcp | Change Notify | Notify over TCP not implemented | - |
| smb2.notify.tdis | Change Notify | Notify on tree disconnect not implemented | - |
| smb2.notify.tdis1 | Change Notify | Notify on tree disconnect not implemented | - |
| smb2.notify.tree | Change Notify | Tree-level notify not implemented | - |
| smb2.notify.valid-req | Change Notify | Notify valid request checks not implemented | - |
| smb2.change_notify_disabled.notfiy_disabled | Change Notify | Change notify disabled test not implemented | - |

### Oplocks (Multi-Client Coordination Not Implemented)

Oplock tests require multi-client coordination (oplock break notifications to
other clients). DittoFS has basic oplock support but the smbtorture oplock
tests use two connections with coordinated break callbacks that require full
oplock break notification delivery.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.oplock.batch1 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch2 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch3 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch4 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch5 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch6 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch7 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch8 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch9 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch9a | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch10 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch11 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch12 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch13 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch14 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch15 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch16 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch19 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch20 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch21 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch22a | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch22b | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch23 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch24 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch25 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch26 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.exclusive1 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.exclusive2 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.exclusive3 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.exclusive4 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.exclusive5 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.exclusive6 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.exclusive9 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.levelii500 | Oplocks | Level II oplock notification not implemented | - |
| smb2.oplock.levelii501 | Oplocks | Level II oplock notification not implemented | - |
| smb2.oplock.levelii502 | Oplocks | Level II oplock notification not implemented | - |
| smb2.oplock.brl1 | Oplocks | Byte-range lock + oplock interaction | - |
| smb2.oplock.brl2 | Oplocks | Byte-range lock + oplock interaction | - |
| smb2.oplock.brl3 | Oplocks | Byte-range lock + oplock interaction | - |
| smb2.oplock.doc | Oplocks | Delete-on-close + oplock interaction | - |
| smb2.oplock.statopen1 | Oplocks | Stat open + oplock interaction | - |
| smb2.oplock.stream1 | Oplocks | Stream + oplock interaction | - |
| smb2.kernel-oplocks.kernel_oplocks1 | Kernel Oplocks | Kernel oplock break not implemented | - |
| smb2.kernel-oplocks.kernel_oplocks2 | Kernel Oplocks | Kernel oplock break not implemented | - |
| smb2.kernel-oplocks.kernel_oplocks3 | Kernel Oplocks | Kernel oplock break not implemented | - |
| smb2.kernel-oplocks.kernel_oplocks4 | Kernel Oplocks | Kernel oplock break not implemented | - |
| smb2.kernel-oplocks.kernel_oplocks5 | Kernel Oplocks | Kernel oplock break not implemented | - |
| smb2.kernel-oplocks.kernel_oplocks6 | Kernel Oplocks | Kernel oplock break not implemented | - |
| smb2.kernel-oplocks.kernel_oplocks7 | Kernel Oplocks | Kernel oplock break not implemented | - |

### Directory Leases (Not Implemented)

Directory leases (dirlease) are a separate feature from file leases.
DittoFS implements file leases (Phase 37) but not directory leases.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.dirlease.hardlink | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.leases | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.oplocks | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.overwrite | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.rename | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.rename_dst_parent | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.setatime | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.setbtime | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.setctime | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.setdos | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.seteof | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.setmtime | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.unlink_different_initial_and_close | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.unlink_different_set_and_close | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.unlink_same_initial_and_close | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.unlink_same_set_and_close | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.v2_request | Directory Leases | Directory leases not implemented | - |
| smb2.dirlease.v2_request_parent | Directory Leases | Directory leases not implemented | - |

### Credit Management (Not Fully Implemented)

SMB3 credit management (credit grants, async credits, IPC credits) is not
fully implemented. DittoFS grants a fixed credit count.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.credits.1conn_ipc_max_async_credits | Credits | IPC async credit management not implemented | - |
| smb2.credits.ipc_max_data_zero | Credits | IPC credit management not implemented | - |
| smb2.credits.session_setup_credits_granted | Credits | Dynamic credit granting not implemented | - |
| smb2.credits.single_req_credits_granted | Credits | Dynamic credit granting not implemented | - |
| smb2.credits.skipped_mid | Credits | Skipped message ID tracking not implemented | - |

### Directory Operations (Advanced Queries Not Implemented)

Advanced directory query features (file index, sorted results, large directory
handling) are not fully implemented.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.dir.1kfiles_rename | Directory | Large directory rename not implemented | - |
| smb2.dir.file-index | Directory | File index tracking not implemented | - |
| smb2.dir.find | Directory | Advanced find semantics not implemented | - |
| smb2.dir.fixed | Directory | Fixed-size directory entries not implemented | - |
| smb2.dir.large-files | Directory | Large directory operations not implemented | - |
| smb2.dir.many | Directory | Large directory operations not implemented | - |
| smb2.dir.modify | Directory | Directory modify during enumeration not implemented | - |
| smb2.dir.one | Directory | Single-entry directory query not implemented | - |
| smb2.dir.sorted | Directory | Sorted directory results not implemented | - |

### File Attributes (Limited Support)

DittoFS has limited DOS/Windows attribute support. Hidden, system, and archive
attributes are not fully implemented.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.dosmode | DOS attributes | DOS mode semantics not implemented | - |
| smb2.async_dosmode | DOS attributes | Async DOS mode not implemented | - |
| smb2.openattr | File attributes | Open with attribute validation not implemented | - |
| smb2.winattr | Windows attributes | Windows-specific attributes not implemented | - |

### Create Contexts (Advanced Semantics Not Implemented)

Advanced CREATE context features (impersonation, ACL-based create, quota fake
files, create blobs) are not implemented. Basic create operations pass.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.create.acldir | Create | ACL-based directory create not implemented | - |
| smb2.create.aclfile | Create | ACL-based file create not implemented | - |
| smb2.create.bench-path-contention-shared | Create | Path contention benchmark not implemented | - |
| smb2.create.blob | Create | Create context blobs not fully implemented | - |
| smb2.create.gentest | Create | Generic create test (impersonation) not implemented | - |
| smb2.create.impersonation | Create | Impersonation levels not implemented | - |
| smb2.create.leading-slash | Create | Leading slash path handling not implemented | - |
| smb2.create.mkdir-visible | Create | Mkdir visibility semantics not implemented | - |
| smb2.create.nulldacl | Create | Null DACL create not implemented | - |
| smb2.create.quota-fake-file | Create | Quota fake file not implemented | - |

### Read/Write Operations (Advanced Semantics)

Advanced read/write scenarios requiring access check enforcement or protocol
edge cases.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.read.access | Read | Read access enforcement not fully implemented | - |
| smb2.read.bug14607 | Read | Read edge case (bug 14607) not implemented | - |
| smb2.read.eof | Read | EOF handling semantics not implemented | - |
| smb2.read.position | Read | Read position tracking not implemented | - |
| smb2.rw.invalid | Read/Write | Invalid R/W request handling not implemented | - |
| smb2.rw.rw1 | Read/Write | Read/write interop test not implemented | - |
| smb2.rw.rw2 | Read/Write | Read/write interop test not implemented | - |

### Query/Set Info (Advanced Scenarios)

Advanced getinfo scenarios requiring security descriptor queries, buffer size
checks, and ACL-based access control.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.getinfo.complex | Query Info | Complex getinfo not implemented | - |
| smb2.getinfo.fsinfo | Query Info | Filesystem info not fully implemented | - |
| smb2.getinfo.getinfo_access | Query Info | Access-based getinfo not implemented | - |
| smb2.getinfo.granted | Query Info | Granted access info not implemented | - |
| smb2.getinfo.normalized | Query Info | Normalized name info not implemented | - |
| smb2.getinfo.qfile_buffercheck | Query Info | Buffer check validation not implemented | - |
| smb2.getinfo.qfs_buffercheck | Query Info | FS buffer check not implemented | - |
| smb2.getinfo.qsec_buffercheck | Query Info | Security buffer check not implemented | - |
| smb2.setinfo | Set Info | SET_INFO timestamp preservation not implemented | - |

### Compound Requests (Async Not Implemented)

Compound async request handling is not implemented. Related compound find
operations work but async compounds do not.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.compound_async.create_lease_break_async | Compound | Async compound not implemented | - |
| smb2.compound_async.flush_close | Compound | Async compound not implemented | - |
| smb2.compound_async.flush_flush | Compound | Async compound not implemented | - |
| smb2.compound_async.getinfo_middle | Compound | Async compound not implemented | - |
| smb2.compound_async.read_read | Compound | Async compound not implemented | - |
| smb2.compound_async.rename_last | Compound | Async compound not implemented | - |
| smb2.compound_async.rename_middle | Compound | Async compound not implemented | - |
| smb2.compound_async.rename_non_compound_no_async | Compound | Async compound not implemented | - |
| smb2.compound_async.rename_same_srcdst_non_compound_no_async | Compound | Async compound not implemented | - |
| smb2.compound_async.write_write | Compound | Async compound not implemented | - |
| smb2.compound_find.compound_find_close | Compound | Compound find close not implemented | - |

### Share Modes and Deny (Advanced Scenarios)

Advanced share mode enforcement and deny mode scenarios.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.sharemode.access-sharemode | Share modes | Advanced share mode enforcement not implemented | - |
| smb2.sharemode.bug14375 | Share modes | Share mode edge case not implemented | - |
| smb2.sharemode.sharemode-access | Share modes | Share mode access check not implemented | - |
| smb2.deny.deny1 | Deny modes | Deny mode enforcement not implemented | - |
| smb2.deny.deny2 | Deny modes | Deny mode enforcement not implemented | - |

### Delete-on-Close (Advanced Semantics)

Advanced delete-on-close permission checks and edge cases. Basic DOC works
(3 tests pass) but permission-restricted scenarios do not.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.delete-on-close-perms.CREATE | Delete on close | DOC permission check not implemented | - |
| smb2.delete-on-close-perms.CREATE_IF | Delete on close | DOC permission check not implemented | - |
| smb2.delete-on-close-perms.READONLY | Delete on close | DOC on read-only files not implemented | - |

### File IDs (Different Handle Scheme)

DittoFS uses a different file handle scheme than Windows NTFS file IDs.
Stable file ID tracking across renames and uniqueness guarantees are not
implemented.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.fileid.fileid | File IDs | Stable file ID not implemented | - |
| smb2.fileid.fileid-dir | File IDs | Stable directory file ID not implemented | - |
| smb2.fileid.unique | File IDs | Unique file ID guarantee not implemented | - |
| smb2.fileid.unique-dir | File IDs | Unique directory file ID not implemented | - |

### Maximum Allowed Access (Partial)

Maximum allowed access computation is partially implemented. Read-only
maximum_allowed works but full computation does not.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.maximum_allowed.maximum_allowed | Access checks | Full maximum allowed computation not implemented | - |

### Connection and Tree Connect (Advanced Semantics)

Advanced connection and tree connect edge cases.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.tcon | Tree connect | Advanced tree connect semantics not implemented | - |
| smb2.maxfid | Connection | Connection drops under high FD pressure | - |

### Previous Versions / Time Warp (Not Implemented)

Previous versions (shadow copies / TWRP) are a Windows Volume Shadow Copy
feature not applicable to DittoFS.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.twrp.listdir | Previous versions | Time-warp not implemented | - |

### Benchmarks (Multi-Client Coordination)

Benchmark tests require multi-client coordination and stress scenarios.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.bench.echo | Benchmarks | Multi-client echo benchmark | - |
| smb2.bench.oplock1 | Benchmarks | Multi-client oplock benchmark | - |
| smb2.bench.path-contention-shared | Benchmarks | Multi-client path contention | - |
| smb2.bench.read | Benchmarks | Multi-client read benchmark | - |
| smb2.bench.session-setup | Benchmarks | Multi-client session setup benchmark | - |

### Character Set (Edge Cases)

Unicode and character set edge cases (partial surrogates, wide-A collision) are
tracked as fix candidates in baseline-results.md rather than known failures,
since basic charset support works.

### Name Mangling (Not Implemented)

8.3 short name mangling (DOS compatibility) is not implemented.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.name-mangling.mangle | Name mangling | 8.3 short name mangling not implemented | - |
| smb2.name-mangling.mangled-mask | Name mangling | Mangled name mask search not implemented | - |

### Extended Attributes (ACL-Based)

Extended attribute tests requiring ACL-based access control.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.ea.acl_xattr | Extended attributes | EA ACL enforcement not implemented | - |

### Timestamp Resolution

Timestamp resolution test requires sub-second precision enforcement.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.timestamp_resolution.resolution1 | Timestamps | Timestamp resolution enforcement not implemented | - |

### Samba-Specific Tests

Samba3-specific POSIX lock extensions not implemented.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.samba3misc.localposixlock1 | Samba-specific | POSIX lock extensions not implemented | - |

### Session Signing Edge Cases

Session signing edge cases requiring multi-channel binding.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.session-require-signing.bug15397 | Session signing | Signing enforcement with binding not implemented | - |

## Notes

- smbtorture image: quay.io/samba.org/samba-toolbox:v0.8
- DittoFS implements SMB 2.0.2, 2.1, 3.0, 3.0.2, and 3.1.1 dialects
- Phases 33-39 added: SMB3 dialect negotiation, key derivation (SP800-108 KDF),
  signing (HMAC-SHA256/AES-128-CMAC/AES-128-GMAC), encryption (AES-128-CCM/GCM,
  AES-256-CCM/GCM), Kerberos authentication, leases, durable handles V2, and
  cross-protocol coordination
- 50 tests newly pass after phases 33-39 (see baseline-results.md)
- Tests for implemented features (sessions, leases, durable handles) that still
  fail are tracked as fix candidates in baseline-results.md, NOT here
- The NT_STATUS_NO_MEMORY errors seen in full-suite runs are a client-side issue
  from rapid connection creation under ARM64 emulation, not a DittoFS server bug
- Interactive hold tests (smb2.hold-oplock, smb2.hold-sharemode) are skipped by
  run.sh and not listed here

## How to Add New Entries

After running the test suite, `parse-results.sh` will report new failures not
in this table. To add them:

1. Copy the exact test name from the output
2. Investigate the failure -- determine whether the feature is implemented
3. If the feature IS implemented, track as a fix candidate (do NOT add here)
4. If the feature is genuinely NOT implemented, add a row with:
   - Exact test name (no wildcard patterns)
   - Category
   - Specific reason (which feature is missing)
   - GitHub issue or future phase reference

Format:
```
| smb2.exact.test.name | Category | Specific reason for expected failure | #issue or Phase N |
```
