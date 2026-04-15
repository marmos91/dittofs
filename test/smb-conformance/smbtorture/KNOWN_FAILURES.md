# smbtorture Known Failures

Last updated: 2026-03-31 (Compression inheritance fix, stale compression known failures cleanup)

Tests listed here are expected to fail and will NOT cause CI to report failure.
Only NEW failures (not in this list) will cause CI to fail.

The `parse-results.sh` script reads test names from the first column of the
table below. Lines starting with `#`, `|---`, empty lines, and the header row
(`Test Name`) are ignored.

Every entry has been individually verified against the smbtorture baseline run
of 2026-03-02 (commit 52f84ecd). Tests that fail due to genuinely unimplemented
features are listed, along with fix-candidate tests for partially-implemented
features (sessions, leases, durable handles, locks) that still need work.

## Expected Failures

### Multi-Channel (Not Implemented)

Multi-channel support requires establishing multiple TCP connections to the same
session, which DittoFS does not implement.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.multichannel.bugs.bug_15346 | Multi-channel | Multi-channel not implemented | - |
| smb2.multichannel.generic.num_channels | Multi-channel | Multi-channel not implemented | - |
| smb2.multichannel.leases.test2 | Multi-channel | Multi-channel lease coordination not implemented | - |
| smb2.multichannel.oplocks.test1 | Multi-channel | Multi-channel oplock coordination not implemented | - |
| smb2.multichannel.oplocks.test2 | Multi-channel | Multi-channel oplock coordination not implemented | - |
| smb2.multichannel.oplocks.test3_specification | Multi-channel | Multi-channel oplock coordination not implemented | - |
| smb2.multichannel.leases.test1 | Multi-channel | Multi-channel lease coordination not implemented | - |

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

Server-side copy (SRV_COPYCHUNK), sparse file operations, and most FSCTL operations
are not implemented. Compression state tracking (FSCTL_GET/SET_COMPRESSION),
FILE_ATTRIBUTE_COMPRESSED, compression inheritance (parent dir to child), and
FILE_NO_COMPRESSION create option are supported. Compression permission checks
(SEC_FILE_WRITE_DATA for SET_COMPRESSION) are not yet implemented.
Duplicate extents (block refcounting) tests skip automatically because
FILE_SUPPORTS_BLOCK_REFCOUNTING is not advertised. The compress_notsup_get/set
tests correctly SKIP because FILE_FILE_COMPRESSION is advertised.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.ioctl.bug14769 | IOCTL | IOCTL edge case not implemented | - |
| smb2.ioctl.compress_perms | IOCTL | FSCTL_SET_COMPRESSION requires SEC_FILE_WRITE_DATA check (not implemented) | - |
| smb2.ioctl.dup_extents_simple | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_len_beyond_dest | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_len_beyond_src | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_len_zero | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_compressed_src | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_sparse_dest | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_sparse_src | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_bad_handle | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_sparse_both | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_src_is_dest | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_src_is_dest_overlap | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_compressed_dest | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_src_lock | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.dup_extents_dest_lock | IOCTL | Duplicate extents not implemented (may state-poison in CI) | - |
| smb2.ioctl.copy_chunk_sparse_dest | IOCTL | Sparse file semantics not implemented for server-side copy | - |
| smb2.ioctl.bug14788.NETWORK_INTERFACE | IOCTL | Network interface enumeration not implemented | - |
| smb2.ioctl.network_interface_info | IOCTL | Flaky in CI (network interface query race) | - |
| smb2.ioctl.sparse_compressed | IOCTL | Sparse + compression not implemented | - |
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
| smb2.streams.sharemodes | Streams | ADS share mode enforcement edge cases (newly reachable) | - |
| smb2.streams.zero-byte | Streams | ADS zero-byte handling not implemented | - |
| smb2.create_no_streams.no_stream | Streams | No-streams create context not implemented | - |

### Change Notify (Remaining)

Phase 73 Plan 03 completed async ChangeNotify infrastructure: basedir, close,
dir, double, mask, mask-change, rec, rmdir1-4, tree, logoff, tdis,
tdis1, tcp, tcon now pass. Remaining tests require features not yet implemented.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.notify.handle-permissions | Change Notify | Notify per-handle permission enforcement not tested | - |
| smb2.notify.invalid-reauth | Change Notify | Notify re-auth interaction edge case | - |
| smb2.notify.overflow | Change Notify | Notify buffer overflow edge case | - |
| smb2.notify.session-reconnect | Change Notify | Depends on session reconnect (not re-auth) | - |
| smb2.notify.valid-req | Change Notify | CompletionFilter validation rejects previously-accepted requests | - |
| smb2.notify.tcon | Change Notify | Full-suite flaky (tree disconnect notify race) | - |
| smb2.notify.dir | Change Notify | Full-suite flaky (directory notify race) | - |
| smb2.change_notify_disabled.notfiy_disabled | Change Notify | Change notify disabled mode test | - |

### Oplocks (Multi-Client Coordination Not Implemented)

Oplock tests require multi-client coordination (oplock break notifications to
other clients). DittoFS has basic oplock support but the smbtorture oplock
tests use two connections with coordinated break callbacks that require full
oplock break notification delivery.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.oplock.batch1 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch3 | Oplocks | Multi-client oplock break coordination | - |
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
| smb2.oplock.batch16 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch19 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch20 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch22a | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch22b | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch23 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch24 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.batch26 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.exclusive4 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.exclusive6 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.exclusive9 | Oplocks | Multi-client oplock break coordination | - |
| smb2.oplock.levelii500 | Oplocks | Level II oplock notification not implemented | - |
| smb2.oplock.levelii502 | Oplocks | Level II oplock notification not implemented | - |
| smb2.oplock.brl1 | Oplocks | Byte-range lock + oplock interaction | - |
| smb2.oplock.brl2 | Oplocks | Byte-range lock + oplock interaction | - |
| smb2.oplock.brl3 | Oplocks | Byte-range lock + oplock interaction | - |
| smb2.oplock.doc | Oplocks | Delete-on-close + oplock interaction | - |
| smb2.oplock.statopen1 | Oplocks | Stat open + oplock interaction | - |
| smb2.oplock.stream1 | Oplocks | Stream + oplock interaction | - |
| smb2.kernel-oplocks.kernel_oplocks2 | Kernel Oplocks | Kernel oplock break coordination (newly reachable) | - |
| smb2.kernel-oplocks.kernel_oplocks4 | Kernel Oplocks | Kernel oplock break coordination (newly reachable) | - |
| smb2.kernel-oplocks.kernel_oplocks8 | Kernel Oplocks | Flaky in CI (skip/fail race on localdir check) | - |
| smb2.kernel-oplocks.kernel_oplocks5 | Kernel Oplocks | Lease downgrade grants Level II where kernel oplock expects None (kernel oplock semantics not implemented) | - |

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
| smb2.credits.2conn_ipc_max_async_credits | Credits | Multi-connection IPC async credit management not implemented | - |
| smb2.credits.multichannel_ipc_max_async_credits | Credits | Multi-channel IPC async credit management not implemented | - |
| smb2.credits.1conn_notify_max_async_credits | Credits | Change notification async credit management not implemented | - |
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
| smb2.create.mkdir-dup | Create | Flaky in CI (parallel CREATE OPEN_IF race — passes intermittently on develop, perturbed by unrelated timing changes) | - |
| smb2.create.mkdir-visible | Create | Mkdir visibility semantics not implemented | - |
| smb2.create.nulldacl | Create | Null DACL create not implemented | - |
| smb2.create.path-length | Create | Flaky in CI (path length validation race) | - |
| smb2.create.quota-fake-file | Create | Quota fake file not implemented | - |

### Read/Write Operations (Advanced Semantics)

Advanced read/write scenarios requiring access check enforcement or protocol
edge cases.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.read.access | Read | Read access enforcement not fully implemented (needs DesiredAccess from CREATE) | - |
| smb2.read.position | Read | Read position tracking not implemented | - |

### Query/Set Info (Advanced Scenarios)

Advanced getinfo scenarios requiring security descriptor queries, buffer size
checks, and ACL-based access control.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.getinfo.complex | Query Info | Complex getinfo not implemented | - |
| smb2.getinfo.getinfo_access | Query Info | Access-based getinfo not implemented | - |
| smb2.getinfo.granted | Query Info | Granted access info not implemented | - |
| smb2.getinfo.normalized | Query Info | Normalized name info not implemented | - |
| smb2.getinfo.qfile_buffercheck | Query Info | Buffer check validation not implemented | - |
| smb2.getinfo.qfs_buffercheck | Query Info | FS buffer check not implemented | - |
| smb2.getinfo.qsec_buffercheck | Query Info | Security buffer check not implemented | - |
| smb2.setinfo | Set Info | SET_INFO timestamp preservation not implemented | - |

### Compound Requests (Remaining)

Phase 73.1 fixed compound related/unrelated chaining, error propagation,
interim responses, padding, compound find, and async flush. Remaining
failures require DACL enforcement or full async I/O support.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.compound.related4 | Compound | Access control enforcement in compound CREATE (DACL) | - |
| smb2.compound.related7 | Compound | Access control enforcement in compound CREATE (DACL) | - |
| smb2.compound_async.create_lease_break_async | Compound | Async lease break in compound not implemented | - |
| smb2.compound_async.getinfo_middle | Compound | Async getinfo in compound middle position | - |
| smb2.compound_async.read_read | Compound | Async read+read compound not implemented | - |
| smb2.compound_async.rename_last | Compound | Async rename in compound last position | - |
| smb2.compound_async.rename_middle | Compound | Async rename in compound middle position | - |
| smb2.compound_async.rename_non_compound_no_async | Compound | Non-compound rename async check | - |
| smb2.compound_async.rename_same_srcdst_non_compound_no_async | Compound | Same src/dst rename async check | - |
| smb2.compound_async.write_write | Compound | Async write+write compound not implemented | - |
| smb2.compound_find.compound_find_close | Compound | Compound find+close sequence | - |

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
| smb2.delete-on-close-perms.BUG14427 | Delete on close | Flaky SMB2 signing failure during connection setup | - |
| smb2.delete-on-close-perms.CREATE | Delete on close | DOC permission check not implemented | - |
| smb2.delete-on-close-perms.CREATE_IF | Delete on close | DOC permission check not implemented | - |
| smb2.delete-on-close-perms.READONLY | Delete on close | DOC on read-only files not implemented | - |
| smb2.delete-on-close-perms.OVERWRITE_IF | Delete on close | OVERWRITE_IF returns OBJECT_NAME_COLLISION instead of ACCESS_DENIED for DOC permission check | - |

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
| smb2.twrp.openroot | Previous versions | Time-warp not implemented | - |
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

### Character Set Edge Cases (Fix Candidate)

Unicode and character set edge cases (partial surrogates, wide-A collision).
Newly reachable after compound and protocol improvements.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.charset.Testing | Character set | Unicode surrogate pair handling not implemented | - |

### Delete-on-Close OVERWRITE_IF (Fix Candidate)

Delete-on-close with OVERWRITE_IF disposition needs additional enforcement.
Newly reachable after access control improvements.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|

### Durable Handles V1 (Fix Candidate)

Durable handle V1 open/reopen operations partially implemented but tests
still fail due to incomplete reconnect and lease coordination.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.durable-open.open-lease | Durable handles V1 | Durable open with lease not fully working | - |
| smb2.durable-open.reopen1a | Durable handles V1 | Durable reopen not fully working | - |
| smb2.durable-open.reopen1a-lease | Durable handles V1 | Durable reopen with lease not fully working | - |
| smb2.durable-open.reopen2 | Durable handles V1 | Durable reopen not fully working | - |
| smb2.durable-open.reopen2-lease | Durable handles V1 | Durable reopen with lease not fully working | - |
| smb2.durable-open.reopen2-lease-v2 | Durable handles V1 | Durable reopen with lease V2 not fully working | - |
| smb2.durable-open.reopen2a | Durable handles V1 | Durable reopen not fully working | - |
| smb2.durable-open.reopen4 | Durable handles V1 | Durable reopen not fully working | - |
| smb2.durable-open.delete_on_close1 | Durable handles V1 | Durable DOC not fully working | - |
| smb2.durable-open.delete_on_close2 | Durable handles V1 | Durable DOC not fully working | - |
| smb2.durable-open.file-position | Durable handles V1 | Durable file position not fully working | - |
| smb2.durable-open.lock-oplock | Durable handles V1 | Durable lock + oplock not fully working | - |
| smb2.durable-open.lock-lease | Durable handles V1 | Durable lock + lease not fully working | - |

### Durable Handles V2 (Fix Candidate)

Durable handle V2 open/reopen operations partially implemented but tests
still fail due to incomplete reconnect, lease coordination, and persistence.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.durable-v2-open.create-blob | Durable handles V2 | DH2Q create context blob validation | - |
| smb2.durable-v2-open.open-oplock | Durable handles V2 | DH2 open with oplock not fully working | - |
| smb2.durable-v2-open.open-lease | Durable handles V2 | DH2 open with lease not fully working | - |
| smb2.durable-v2-open.reopen1 | Durable handles V2 | DH2 reopen not fully working | - |
| smb2.durable-v2-open.reopen1a | Durable handles V2 | DH2 reopen not fully working | - |
| smb2.durable-v2-open.reopen1a-lease | Durable handles V2 | DH2 reopen with lease not fully working | - |
| smb2.durable-v2-open.reopen2 | Durable handles V2 | DH2 reopen not fully working | - |
| smb2.durable-v2-open.reopen2b | Durable handles V2 | DH2 reopen not fully working | - |
| smb2.durable-v2-open.reopen2-lease | Durable handles V2 | DH2 reopen with lease not fully working | - |
| smb2.durable-v2-open.reopen2-lease-v2 | Durable handles V2 | DH2 reopen with lease V2 not fully working | - |
| smb2.durable-v2-open.durable-v2-setinfo | Durable handles V2 | DH2 setinfo not fully working | - |
| smb2.durable-v2-open.lock-oplock | Durable handles V2 | DH2 lock with oplock not fully working | - |
| smb2.durable-v2-open.lock-lease | Durable handles V2 | DH2 lock with lease not fully working | - |
| smb2.durable-v2-open.lock-noW-lease | Durable handles V2 | DH2 lock without write lease not fully working | - |
| smb2.durable-v2-open.stat-and-lease | Durable handles V2 | DH2 stat + lease interaction not fully working | - |
| smb2.durable-v2-open.nonstat-and-lease | Durable handles V2 | DH2 non-stat + lease interaction not fully working | - |
| smb2.durable-v2-open.statRH-and-lease | Durable handles V2 | DH2 stat-RH + lease interaction not fully working | - |
| smb2.durable-v2-open.two-same-lease | Durable handles V2 | DH2 two handles same lease not fully working | - |
| smb2.durable-v2-open.two-different-lease | Durable handles V2 | DH2 two handles different leases not fully working | - |
| smb2.durable-v2-open.keep-disconnected-rh-with-stat-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | - |
| smb2.durable-v2-open.keep-disconnected-rh-with-rh-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | - |
| smb2.durable-v2-open.keep-disconnected-rh-with-rwh-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | - |
| smb2.durable-v2-open.keep-disconnected-rwh-with-stat-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | - |
| smb2.durable-v2-open.purge-disconnected-rwh-with-rwh-open | Durable handles V2 | DH2 disconnected handle purge not fully working | - |
| smb2.durable-v2-open.purge-disconnected-rwh-with-rh-open | Durable handles V2 | DH2 disconnected handle purge not fully working | - |
| smb2.durable-v2-open.purge-disconnected-rh-with-share-none-open | Durable handles V2 | DH2 disconnected handle purge not fully working | - |
| smb2.durable-v2-open.purge-disconnected-rh-with-write | Durable handles V2 | DH2 disconnected handle purge not fully working | - |
| smb2.durable-v2-open.purge-disconnected-rh-with-rename | Durable handles V2 | DH2 disconnected handle purge not fully working | - |
| smb2.durable-v2-open.app-instance | Durable handles V2 | App instance ID not fully working | - |
| smb2.durable-v2-open.persistent-open-oplock | Durable handles V2 | Persistent handles not implemented | - |
| smb2.durable-v2-open.persistent-open-lease | Durable handles V2 | Persistent handles not implemented | - |
| smb2.durable-v2-delay.durable_v2_reconnect_delay | Durable handles V2 | DH2 reconnect delay not fully working | - |

### Leases (Fix Candidate)

Lease V2 is implemented but many smbtorture lease tests still fail due to
incomplete break notification delivery and multi-client coordination.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.lease.request | Leases | Lease request handling not fully working | - |
| smb2.lease.nobreakself | Leases | Lease self-break suppression not fully working | - |
| smb2.lease.statopen | Leases | Lease + stat open interaction not fully working | - |
| smb2.lease.statopen4 | Leases | Lease + stat open interaction not fully working | - |
| smb2.lease.upgrade | Leases | Lease upgrade not fully working | - |
| smb2.lease.upgrade2 | Leases | Lease upgrade not fully working | - |
| smb2.lease.upgrade3 | Leases | Lease upgrade not fully working | - |
| smb2.lease.break | Leases | Lease break notification not fully working | - |
| smb2.lease.oplock | Leases | Lease + oplock interaction not fully working | - |
| smb2.lease.multibreak | Leases | Multi-client lease break not fully working | - |
| smb2.lease.breaking1 | Leases | Lease breaking state handling not fully working | - |
| smb2.lease.breaking2 | Leases | Lease breaking state handling not fully working | - |
| smb2.lease.breaking3 | Leases | Lease breaking state handling not fully working | - |
| smb2.lease.breaking4 | Leases | Lease breaking state handling not fully working | - |
| smb2.lease.breaking5 | Leases | Lease breaking state handling not fully working | - |
| smb2.lease.breaking6 | Leases | Lease breaking state handling not fully working | - |
| smb2.lease.lock1 | Leases | Lease + lock interaction not fully working | - |
| smb2.lease.complex1 | Leases | Complex lease scenario not fully working | - |
| smb2.lease.timeout | Leases | Lease timeout handling not fully working | - |
| smb2.lease.unlink | Leases | Lease + unlink interaction not fully working | - |
| smb2.lease.timeout-disconnect | Leases | Lease timeout on disconnect not fully working | - |
| smb2.lease.rename_wait | Leases | Lease + rename wait not fully working | - |
| smb2.lease.duplicate_create | Leases | Duplicate lease create not fully working | - |
| smb2.lease.duplicate_open | Leases | Duplicate lease open not fully working | - |
| smb2.lease.v1_bug15148 | Leases | Lease V1 edge case not fully working | - |
| smb2.lease.initial_delete_tdis | Leases | Lease + delete on tree disconnect not fully working | - |
| smb2.lease.initial_delete_logoff | Leases | Lease + delete on logoff not fully working | - |
| smb2.lease.initial_delete_disconnect | Leases | Lease + delete on disconnect not fully working | - |
| smb2.lease.rename_dir_openfile | Leases | Lease + directory rename with open file not fully working | - |
| smb2.lease.lease-epoch | Leases | Lease epoch tracking not fully working | - |
| smb2.lease.break_twice | Leases | Double lease break not fully working | - |
| smb2.lease.v2_breaking3 | Leases V2 | Lease V2 breaking state handling not fully working | - |
| smb2.lease.v2_flags_breaking | Leases V2 | Lease V2 flags during break not fully working | - |
| smb2.lease.v2_flags_parentkey | Leases V2 | Lease V2 parent key flags not fully working | - |
| smb2.lease.v2_epoch1 | Leases V2 | Lease V2 epoch tracking not fully working | - |
| smb2.lease.v2_epoch2 | Leases V2 | Lease V2 epoch tracking not fully working | - |
| smb2.lease.v2_epoch3 | Leases V2 | Lease V2 epoch tracking not fully working | - |
| smb2.lease.v2_complex1 | Leases V2 | Lease V2 complex scenario not fully working | - |
| smb2.lease.v2_complex2 | Leases V2 | Lease V2 complex scenario not fully working | - |
| smb2.lease.v2_rename | Leases V2 | Lease V2 rename interaction not fully working | - |
| smb2.lease.v2_bug15148 | Leases V2 | Lease V2 edge case not fully working | - |
| smb2.lease.v2_rename_target_overwrite | Leases V2 | Lease V2 rename target overwrite not fully working | - |

### Byte-Range Locks (Fix Candidate)

Byte-range locking is partially implemented but smbtorture lock tests still
fail due to incomplete lock contention and async lock handling.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.lock.valid-request | Locks | Lock request validation not fully working | - |
| smb2.lock.auto-unlock | Locks | Auto-unlock on close not fully working | - |
| smb2.lock.lock | Locks | Basic lock operation not fully working | - |
| smb2.lock.cancel | Locks | Lock cancel not fully working | - |
| smb2.lock.errorcode | Locks | Lock error codes not fully working | - |
| smb2.lock.zerobytelength | Locks | Zero-length lock not fully working | - |
| smb2.lock.unlock | Locks | Unlock operation not fully working | - |
| smb2.lock.multiple-unlock | Locks | Multiple unlock not fully working | - |
| smb2.lock.stacking | Locks | Lock stacking not fully working | - |
| smb2.lock.range | Locks | Lock range validation not fully working | - |
| smb2.lock.overlap | Locks | Overlapping locks not fully working | - |
| smb2.lock.replay_broken_windows | Locks | Lock replay not fully working | - |
| smb2.lock.replay_smb3_specification_durable | Locks | Lock replay with durable handles not fully working | - |
| smb2.lock.replay_smb3_specification_multi | Locks | Lock replay with multi-channel not fully working | - |
| smb2.lock.cancel-logoff | Locks | Lock cancel on logoff not fully working | - |
| smb2.lock.cancel-tdis | Locks | Blocking lock deadlocks dispatch goroutine (needs async LOCK with interim response) | - |
| smb2.lock.async | Locks | Blocking lock deadlocks dispatch goroutine (needs async LOCK with interim response) | - |
| smb2.lock.open-brlock-deadlock | Locks | Open + byte-range lock deadlock detection not working | - |
| smb2.lock.ctdb-delrec-deadlock | Locks | CTDB delete record deadlock not working | - |

### Rename (Fix Candidate)

Rename operations partially implemented but tests fail due to incomplete
share mode enforcement during rename.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.rename.share_delete_and_delete_access | Rename | Share delete + delete access rename not working | - |
| smb2.rename.no_share_delete_but_delete_access | Rename | Rename share mode enforcement not working | - |
| smb2.rename.no_share_delete_no_delete_access | Rename | Rename share mode enforcement not working | - |
| smb2.rename.rename_dir_openfile | Rename | Rename directory with open file not working | - |

### Sessions (Remaining)

Phase 73 Plan 03 implemented session re-authentication with key re-derivation
per MS-SMB2 3.3.5.5.3. reauth2-6 now pass. Remaining tests need session
reconnect or multi-channel binding.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.session.reconnect1 | Sessions | Session reconnect not fully working | - |
| smb2.session.reconnect2 | Sessions | Session reconnect not fully working | - |
| smb2.session.bind_negative_smb202 | Sessions | Session binding validation not fully working | - |
| smb2.session.bind_negative_smb210s | Sessions | Session binding validation not fully working | - |
| smb2.session.bind_negative_smb210d | Sessions | Session binding validation not fully working | - |

### Session Binding (Multi-Channel, Not Implemented)

Session binding tests require multi-channel support to bind a session across
TCP connections with different SMB dialect and signing/encryption combinations.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.session.bind2 | Session binding | Session binding not implemented | - |
| smb2.session.bind_invalid_auth | Session binding | Session binding auth validation not implemented | - |
| smb2.session.bind_negative_smb2to3s | Session binding | Multi-channel session binding not implemented | - |
| smb2.session.bind_negative_smb2to3d | Session binding | Multi-channel session binding not implemented | - |
| smb2.session.bind_negative_smb3to2s | Session binding | Multi-channel session binding not implemented | - |
| smb2.session.bind_negative_smb3to2d | Session binding | Multi-channel session binding not implemented | - |
| smb2.session.bind_negative_smb3to3s | Session binding | Multi-channel session binding not implemented | - |
| smb2.session.bind_negative_smb3to3d | Session binding | Multi-channel session binding not implemented | - |
| smb2.session.bind_negative_smb3encGtoCs | Session binding | Multi-channel encryption binding not implemented | - |
| smb2.session.bind_negative_smb3encGtoCd | Session binding | Multi-channel encryption binding not implemented | - |
| smb2.session.bind_negative_smb3signCtoHs | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3signCtoHd | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3signCtoGs | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3signCtoGd | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3signHtoCs | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3signHtoCd | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3signHtoGs | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3signHtoGd | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3signGtoCs | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3signGtoCd | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3signGtoHs | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3signGtoHd | Session binding | Multi-channel signing binding not implemented | - |
| smb2.session.bind_negative_smb3sneGtoCs | Session binding | Multi-channel signing+encryption binding not implemented | - |
| smb2.session.bind_negative_smb3sneGtoCd | Session binding | Multi-channel signing+encryption binding not implemented | - |
| smb2.session.bind_negative_smb3sneGtoHs | Session binding | Multi-channel signing+encryption binding not implemented | - |
| smb2.session.bind_negative_smb3sneGtoHd | Session binding | Multi-channel signing+encryption binding not implemented | - |
| smb2.session.bind_negative_smb3sneCtoGs | Session binding | Multi-channel signing+encryption binding not implemented | - |
| smb2.session.bind_negative_smb3sneCtoGd | Session binding | Multi-channel signing+encryption binding not implemented | - |
| smb2.session.bind_negative_smb3sneHtoGs | Session binding | Multi-channel signing+encryption binding not implemented | - |
| smb2.session.bind_negative_smb3sneHtoGd | Session binding | Multi-channel signing+encryption binding not implemented | - |
| smb2.session.bind_negative_smb3signC30toGs | Session binding | Multi-channel signing binding (3.0 to GMAC) not implemented | - |
| smb2.session.bind_negative_smb3signC30toGd | Session binding | Multi-channel signing binding (3.0 to GMAC) not implemented | - |
| smb2.session.bind_negative_smb3signH2XtoGs | Session binding | Multi-channel signing binding (HMAC to GMAC) not implemented | - |
| smb2.session.bind_negative_smb3signH2XtoGd | Session binding | Multi-channel signing binding (HMAC to GMAC) not implemented | - |
| smb2.session.bind_negative_smb3signGtoC30s | Session binding | Multi-channel signing binding (GMAC to 3.0) not implemented | - |
| smb2.session.bind_negative_smb3signGtoC30d | Session binding | Multi-channel signing binding (GMAC to 3.0) not implemented | - |
| smb2.session.bind_negative_smb3signGtoH2Xs | Session binding | Multi-channel signing binding (GMAC to HMAC) not implemented | - |
| smb2.session.bind_negative_smb3signGtoH2Xd | Session binding | Multi-channel signing binding (GMAC to HMAC) not implemented | - |

### Session Signing Variants (Algorithm-Specific Tests)

Algorithm-specific signing tests that validate signing with each algorithm
in isolation. Newly reachable after GMAC signing fix.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.session.signing-hmac-sha-256 | Session signing | HMAC-SHA-256 signing test not fully passing | - |
| smb2.session.signing-aes-128-cmac | Session signing | AES-128-CMAC signing test not fully passing | - |
| smb2.session.signing-aes-128-gmac | Session signing | AES-128-GMAC signing test not fully passing | - |

### Session Encryption Variants (Algorithm-Specific Tests)

Algorithm-specific encryption tests that validate encryption with each cipher.
Newly reachable after GMAC signing fix.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.session.encryption-aes-128-ccm | Session encryption | AES-128-CCM encryption test not fully passing | - |
| smb2.session.encryption-aes-128-gcm | Session encryption | AES-128-GCM encryption test not fully passing | - |
| smb2.session.encryption-aes-256-ccm | Session encryption | AES-256-CCM encryption test not fully passing | - |
| smb2.session.encryption-aes-256-gcm | Session encryption | AES-256-GCM encryption test not fully passing | - |

### Anonymous Session (Remaining)

Phase 73 Plan 03 implemented anonymous session encryption bypass per MS-SMB2
3.3.5.2.9. anon-encryption1-3 now pass. Remaining signing tests need further work.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.session.anon-signing1 | Anonymous session | Anonymous session signing not fully passing | - |
| smb2.session.anon-signing2 | Anonymous session | Anonymous session signing not fully passing | - |

### Replay Protection (Not Implemented)

Replay protection requires tracking channel sequences and detecting replayed
requests with durable handles. Newly reachable after GMAC signing fix.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.replay.replay3 | Replay | Flaky in CI (replay detection race) | - |
| smb2.replay.replay-commands | Replay | Replay detection not implemented | - |
| smb2.replay.replay-dhv2-oplock1 | Replay | Replay with durable handles not implemented | - |
| smb2.replay.replay-dhv2-oplock2 | Replay | Replay with durable handles not implemented | - |
| smb2.replay.replay-dhv2-oplock3 | Replay | Replay with durable handles not implemented | - |
| smb2.replay.replay-dhv2-oplock-lease | Replay | Replay with durable handles not implemented | - |
| smb2.replay.replay-dhv2-lease1 | Replay | Replay with durable handles not implemented | - |
| smb2.replay.replay-dhv2-lease2 | Replay | Replay with durable handles not implemented | - |
| smb2.replay.replay-dhv2-lease3 | Replay | Replay with durable handles not implemented | - |
| smb2.replay.replay-dhv2-lease-oplock | Replay | Replay with durable handles not implemented | - |
| smb2.replay.dhv2-pending1n-vs-violation-lease-close-sane | Replay | Replay pending violation handling not implemented | - |
| smb2.replay.dhv2-pending1n-vs-violation-lease-ack-sane | Replay | Replay pending violation handling not implemented | - |
| smb2.replay.dhv2-pending1n-vs-violation-lease-close-windows | Replay | Replay pending violation handling not implemented | - |
| smb2.replay.dhv2-pending1n-vs-violation-lease-ack-windows | Replay | Replay pending violation handling not implemented | - |
| smb2.replay.dhv2-pending1n-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending1n-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending1n-vs-lease-sane | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending1n-vs-lease-windows | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending1l-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending1l-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending1l-vs-lease-sane | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending1l-vs-lease-windows | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending1o-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending1o-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending1o-vs-lease-sane | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending1o-vs-lease-windows | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending2n-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending2n-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending2n-vs-lease-sane | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending2n-vs-lease-windows | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending2l-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending2l-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending2o-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending2o-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending2o-vs-lease-sane | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending2o-vs-lease-windows | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending3n-vs-lease-sane | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending3n-vs-lease-windows | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending3l-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending3l-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending3l-vs-lease-sane | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending3l-vs-lease-windows | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending3o-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending3o-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending3o-vs-lease-sane | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending3o-vs-lease-windows | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.channel-sequence | Replay | Channel sequence tracking not implemented | - |
| smb2.replay.replay4 | Replay | Replay detection not implemented | - |
| smb2.replay.replay5 | Replay | Replay detection not implemented | - |
| smb2.replay.replay6 | Replay | Replay detection not implemented | - |

### Timestamps (Fix Candidate)

Timestamp update semantics partially implemented but tests fail due to
incomplete delayed-write and timestamp freeze/unfreeze logic.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.timestamps.delayed-2write | Timestamps | Delayed write timestamp update not working | - |
| smb2.timestamps.delayed-write-vs-flush | Timestamps | Delayed write vs flush timestamp not working | - |
| smb2.timestamps.delayed-write-vs-setbasic | Timestamps | Delayed write vs setbasic timestamp not working | - |
| smb2.timestamps.delayed-write-vs-seteof | Timestamps | Delayed write vs seteof timestamp not working | - |
| smb2.timestamps.freeze-thaw | Timestamps | CreationTime freeze/unfreeze not fully working | - |

### Scan (Full Operation Enumeration)

The scan tests enumerate all supported operations and fail on unimplemented ones.
smb2.scan.setinfo iterates all SET_INFO information classes; smb2.scan.find
iterates all QUERY_DIRECTORY information classes. Both hit unimplemented classes.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.scan.scan | Scan | Full operation scan hits unimplemented info classes | - |
| smb2.scan.setinfo | Scan | Flaky in CI (rare connection-disconnect signature mismatch on brute-force level enumeration) | - |
| smb2.scan.find | Scan | Hits unimplemented FileInfoClass values in iteration | - |

## Changelog

### Phase 73 (2026-03-24)
Removed ~24 tests (ChangeNotify, session re-auth, anonymous encryption).
Re-added ~28 tests that were prematurely removed (durable handles, leases,
notify valid-req, freeze-thaw). Fixed rw.invalid and kernel_oplocks5 regressions.
Reverted post-conflict lease granting (caused kernel_oplocks5 regression).

## Notes

- smbtorture image: quay.io/samba.org/samba-toolbox:v0.8
- DittoFS implements SMB 2.0.2, 2.1, 3.0, 3.0.2, and 3.1.1 dialects
- Phases 33-39 added: SMB3 dialect negotiation, key derivation (SP800-108 KDF),
  signing (HMAC-SHA256/AES-128-CMAC/AES-128-GMAC), encryption (AES-128-CCM/GCM,
  AES-256-CCM/GCM), Kerberos authentication, leases, durable handles V2, and
  cross-protocol coordination
- 50 tests newly pass after phases 33-39 (see baseline-results.md)
- Fix-candidate tests (leases, durable handles, sessions, locks, etc.) are
  listed here with "(Fix Candidate)" annotations and also tracked in
  baseline-results.md for prioritization
- The NT_STATUS_NO_MEMORY errors seen in full-suite runs are a client-side issue
  from rapid connection creation under ARM64 emulation, not a DittoFS server bug
- Interactive hold tests (smb2.hold-oplock, smb2.hold-sharemode) are skipped by
  run.sh and not listed here

## How to Add New Entries

After running the test suite, `parse-results.sh` will report new failures not
in this table. To add them:

1. Copy the exact test name from the output
2. Investigate the failure -- determine whether the feature is implemented
3. Add the test to this list with the appropriate category and reason
4. Mark fix candidates with "(Fix Candidate)" in the section header

Format:
```
| smb2.exact.test.name | Category | Specific reason for expected failure | #issue or Phase N |
```
