# smbtorture Known Failures

Last updated: 2026-04-24 (Handle-scoped lease release — #429 cluster 33→31 after 2-run confirmation)

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

### Multi-Channel (Partial — Phase 1 of #361)

Phase 1 of #361 lands the session-binding architecture: `Channel` struct
+ `Session.channels` registry, `DeriveChannelSigningKey`, SMB 3.0 / 3.0.2
and SMB 3.1.1 session-bind auth-completion with per-channel preauth hash
chaining, and per-channel sign/verify routing through dispatch. DittoFS
advertises `SMB2_GLOBAL_CAP_MULTI_CHANNEL` in NEGOTIATE so conformant
clients now exercise the multi-channel test surface.

Phase 2 landed break fan-out (#408). Phase 2.3 landed the per-session
32-channel cap and fixed a concurrent-bind race on the PendingAuth slot
(Samba bug 15346 class) — `bug_15346` and `generic.num_channels` now pass.
The remaining known failures are pre-existing lease-break `new_epoch`
drift, Samba-internal test-harness FSCTL requirements, and cross-channel
async credit coordination.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.multichannel.leases.test1 | Multi-channel | Cross-channel lease break fan-out is Phase 2 work on #361; test flakes on DittoFS until the primary/secondary channel coordination lands | #361 |
| smb2.multichannel.leases.test2 | Multi-channel | Requires torture_block_tcp_transport (Samba-internal test-harness operation) to simulate a blocked channel — not implementable | - |
| smb2.multichannel.leases.test3 | Multi-channel | Spurious lease break on uncontested open — separate bug from #417 epoch drift | #436 |
| smb2.multichannel.leases.test4 | Multi-channel | Requires torture_block_tcp_transport (Samba-internal test-harness operation) — not implementable | - |
| smb2.multichannel.oplocks.test2 | Multi-channel | Requires FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT (Samba test-harness FSCTL) to simulate connection failure — not implementable | - |
| smb2.multichannel.oplocks.test3_windows | Multi-channel | Requires FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT to block TCP transport — not implementable | - |
| smb2.multichannel.oplocks.test3_specification | Multi-channel | Requires FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT + 32-channel coordination — not implementable | - |

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
All `smb2.ioctl.dup_extents_*` tests skip automatically (verified in
smbtorture-2026-03-25 results) because `FILE_SUPPORTS_BLOCK_REFCOUNTING` is
not advertised — they consume no failure slots and are not listed below.
The compress_notsup_get/set tests correctly SKIP because FILE_FILE_COMPRESSION
is advertised.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.ioctl.bug14769 | IOCTL | IOCTL edge case not implemented | - |
| smb2.ioctl.compress_perms | IOCTL | FSCTL_SET_COMPRESSION requires SEC_FILE_WRITE_DATA check (not implemented) | - |
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

### Credit Management

Credit grant arithmetic and the `max_async_credits` cap are correct post-#399
and post-#416: the full `smb2.credits` subsuite (10 tests) passes. Samba
enforces the 511-slot cap **per TCP connection** —
`source4/torture/smb2/credits.c:1346` asserts
`num_status_pending == 511` per tree — which DittoFS's per-`ConnInfo`
counter already matched. The `2conn_notify_max_async_credits` failure that
remained here was a cross-connection MessageID collision in
`NotifyRegistry`, fixed in #416.

### Directory Operations (Advanced Queries Not Implemented)

Advanced directory query features (file index, sorted results, large directory
handling) are not fully implemented.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.dir.1kfiles_rename | Directory | Large directory rename not implemented | - |
| smb2.dir.fixed | Directory | Fixed-size directory entries not implemented | - |
| smb2.dir.modify | Directory | Directory modify during enumeration not implemented | - |
| smb2.dir.one | Directory | Single-entry directory query not implemented | - |

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
| smb2.compound_find.compound_find_close | Compound | Compound FIND + CLOSE ordering — directory enumeration state handoff across compound subcommands not implemented | - |
| smb2.compound_async.create_lease_break_async | Compound | Async lease break in compound not implemented | - |
| smb2.compound_async.getinfo_middle | Compound | Async getinfo in compound middle position | - |
| smb2.compound_async.read_read | Compound | Async read+read compound not implemented | - |
| smb2.compound_async.rename_last | Compound | Async rename in compound last position | - |
| smb2.compound_async.rename_middle | Compound | Async rename in compound middle position | - |
| smb2.compound_async.rename_non_compound_no_async | Compound | Non-compound rename async check | - |
| smb2.compound_async.rename_same_srcdst_non_compound_no_async | Compound | Same src/dst rename async check | - |
| smb2.compound_async.write_write | Compound | Async write+write compound not implemented | - |

### Share Modes and Deny (Advanced Scenarios)

Advanced share mode enforcement and deny mode scenarios.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.sharemode.bug14375 | Share modes | Share mode edge case not implemented | - |

### Delete-on-Close (Advanced Semantics)

Advanced delete-on-close permission checks and edge cases. Basic DOC works
(3 tests pass) but permission-restricted scenarios do not.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.delete-on-close-perms.CREATE | Delete on close | DOC permission check not implemented | - |
| smb2.delete-on-close-perms.CREATE_IF | Delete on close | DOC permission check not implemented | - |
| smb2.delete-on-close-perms.OVERWRITE_IF | Delete on close | OVERWRITE_IF returns OBJECT_NAME_COLLISION instead of ACCESS_DENIED for DOC permission check | - |
| smb2.delete-on-close-perms.READONLY | Delete on close | DOC on read-only files not implemented | - |
| smb2.delete-on-close-perms.FIND_and_set_DOC | Delete on close | Cascade from the CREATE/CREATE_IF/OVERWRITE_IF Existing DOC failures above: those subtests leak `test_dir/test_create.dat` that `smb2_deltree` can't recover from, so `torture_smb2_testdir` reopens a non-empty `test_dir` and the final CLOSE correctly returns DIRECTORY_NOT_EMPTY. Pre-#388 the unlink error was silently swallowed by CLOSE so the test "passed" by accident. Will self-resolve once the upstream DOC permission checks are implemented. | - |

### File IDs (Different Handle Scheme)

DittoFS uses a different file handle scheme than Windows NTFS file IDs.
Stable file ID tracking across renames and uniqueness guarantees are not
implemented.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.fileid.fileid | File IDs | Stable file ID not implemented | - |
| smb2.fileid.fileid-dir | File IDs | Stable directory file ID not implemented | - |

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
| smb2.bench.oplock1 | Benchmarks | Multi-client oplock benchmark | - |

### Character Set (Edge Cases)

Unicode and character set edge cases (partial surrogates, wide-A collision) are
tracked as fix candidates in baseline-results.md rather than known failures,
since basic charset support works.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.charset.Testing | Character set | Unicode surrogate pair / wide-A handling not implemented | #435 |

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

### Durable Handles V1 (Fix Candidate)

Durable handle V1 open/reopen operations partially implemented but tests
still fail due to incomplete reconnect and lease coordination.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.durable-open.reopen1a | Durable handles V1 | Durable reopen not fully working | #431 |
| smb2.durable-open.reopen1a-lease | Durable handles V1 | Durable reopen with lease not fully working | #431 |
| smb2.durable-open.reopen2 | Durable handles V1 | Durable reopen not fully working | #431 |
| smb2.durable-open.reopen2-lease | Durable handles V1 | Durable reopen with lease not fully working | #431 |
| smb2.durable-open.reopen2-lease-v2 | Durable handles V1 | Durable reopen with lease V2 not fully working | #431 |
| smb2.durable-open.reopen2a | Durable handles V1 | Durable reopen not fully working | #431 |
| smb2.durable-open.reopen4 | Durable handles V1 | Durable reopen not fully working | #431 |
| smb2.durable-open.delete_on_close1 | Durable handles V1 | Durable DOC not fully working | #431 |
| smb2.durable-open.delete_on_close2 | Durable handles V1 | Durable DOC not fully working | #431 |
| smb2.durable-open.file-position | Durable handles V1 | Durable file position not fully working | #431 |
| smb2.durable-open.lock-oplock | Durable handles V1 | Durable lock + oplock not fully working | #431 |
| smb2.durable-open.lock-lease | Durable handles V1 | Durable lock + lease not fully working | #431 |

### Durable Handles V2 (Fix Candidate)

Durable handle V2 open/reopen operations partially implemented but tests
still fail due to incomplete reconnect, lease coordination, and persistence.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.durable-v2-open.create-blob | Durable handles V2 | DH2Q create context blob validation | #432 |
| smb2.durable-v2-open.open-oplock | Durable handles V2 | DH2 open with oplock not fully working | #432 |
| smb2.durable-v2-open.open-lease | Durable handles V2 | DH2 open with lease not fully working | #432 |
| smb2.durable-v2-open.reopen1 | Durable handles V2 | DH2 reopen not fully working | #432 |
| smb2.durable-v2-open.reopen1a | Durable handles V2 | DH2 reopen not fully working | #432 |
| smb2.durable-v2-open.reopen1a-lease | Durable handles V2 | DH2 reopen with lease not fully working | #432 |
| smb2.durable-v2-open.reopen2 | Durable handles V2 | DH2 reopen not fully working | #432 |
| smb2.durable-v2-open.reopen2b | Durable handles V2 | DH2 reopen not fully working | #432 |
| smb2.durable-v2-open.reopen2-lease | Durable handles V2 | DH2 reopen with lease not fully working | #432 |
| smb2.durable-v2-open.reopen2-lease-v2 | Durable handles V2 | DH2 reopen with lease V2 not fully working | #432 |
| smb2.durable-v2-open.durable-v2-setinfo | Durable handles V2 | DH2 setinfo not fully working | #432 |
| smb2.durable-v2-open.lock-oplock | Durable handles V2 | DH2 lock with oplock not fully working | #432 |
| smb2.durable-v2-open.lock-lease | Durable handles V2 | DH2 lock with lease not fully working | #432 |
| smb2.durable-v2-open.lock-noW-lease | Durable handles V2 | DH2 lock without write lease not fully working | #432 |
| smb2.durable-v2-open.stat-and-lease | Durable handles V2 | DH2 stat + lease interaction not fully working | #432 |
| smb2.durable-v2-open.nonstat-and-lease | Durable handles V2 | DH2 non-stat + lease interaction not fully working | #432 |
| smb2.durable-v2-open.statRH-and-lease | Durable handles V2 | DH2 stat-RH + lease interaction not fully working | #432 |
| smb2.durable-v2-open.two-same-lease | Durable handles V2 | DH2 two handles same lease not fully working | #432 |
| smb2.durable-v2-open.two-different-lease | Durable handles V2 | DH2 two handles different leases not fully working | #432 |
| smb2.durable-v2-open.keep-disconnected-rh-with-stat-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | #432 |
| smb2.durable-v2-open.keep-disconnected-rh-with-rh-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | #432 |
| smb2.durable-v2-open.keep-disconnected-rh-with-rwh-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | #432 |
| smb2.durable-v2-open.keep-disconnected-rwh-with-stat-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | #432 |
| smb2.durable-v2-open.purge-disconnected-rwh-with-rwh-open | Durable handles V2 | DH2 disconnected handle purge not fully working | #432 |
| smb2.durable-v2-open.purge-disconnected-rwh-with-rh-open | Durable handles V2 | DH2 disconnected handle purge not fully working | #432 |
| smb2.durable-v2-open.purge-disconnected-rh-with-share-none-open | Durable handles V2 | DH2 disconnected handle purge not fully working | #432 |
| smb2.durable-v2-open.purge-disconnected-rh-with-write | Durable handles V2 | DH2 disconnected handle purge not fully working | #432 |
| smb2.durable-v2-open.purge-disconnected-rh-with-rename | Durable handles V2 | DH2 disconnected handle purge not fully working | #432 |
| smb2.durable-v2-open.app-instance | Durable handles V2 | App instance ID not fully working | #432 |
| smb2.durable-v2-open.persistent-open-oplock | Durable handles V2 | Persistent handles not implemented | #432 |
| smb2.durable-v2-open.persistent-open-lease | Durable handles V2 | Persistent handles not implemented | #432 |
| smb2.durable-v2-delay.durable_v2_reconnect_delay | Durable handles V2 | DH2 reconnect delay not fully working | #432 |

### Leases (Fix Candidate)

Lease V2 is implemented but many smbtorture lease tests still fail due to
incomplete break notification delivery and multi-client coordination.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.lease.request | Leases | Lease request handling not fully working | #429 |
| smb2.lease.statopen | Leases | Lease + stat open interaction not fully working | #429 |
| smb2.lease.statopen4 | Leases | Lease + stat open interaction not fully working | #429 |
| smb2.lease.oplock | Leases | Lease + oplock interaction not fully working | #429 |
| smb2.lease.multibreak | Leases | Multi-client lease break not fully working | #429 |
| smb2.lease.breaking1 | Leases | Lease breaking state handling not fully working | #429 |
| smb2.lease.breaking6 | Leases | Lease breaking state handling not fully working | #429 |
| smb2.lease.lock1 | Leases | Lease + lock interaction not fully working | #429 |
| smb2.lease.timeout | Leases | Lease timeout handling not fully working | #429 |
| smb2.lease.unlink | Leases | Lease + unlink interaction not fully working | #429 |
| smb2.lease.rename_wait | Leases | Lease + rename wait not fully working | #429 |
| smb2.lease.duplicate_create | Leases | Duplicate lease create not fully working | #429 |
| smb2.lease.duplicate_open | Leases | Duplicate lease open not fully working | #429 |
| smb2.lease.rename_dir_openfile | Leases | Lease + directory rename with open file not fully working | #429 |
| smb2.lease.lease-epoch | Leases | Lease epoch tracking not fully working | #429 |
| smb2.lease.v2_flags_parentkey | Leases V2 | Lease V2 parent key flags not fully working | #429 |
| smb2.lease.v2_epoch2 | Leases V2 | Lease V2 epoch tracking not fully working | #429 |
| smb2.lease.v2_epoch3 | Leases V2 | Lease V2 epoch tracking not fully working | #429 |
| smb2.lease.v2_complex1 | Leases V2 | Lease V2 complex scenario not fully working | #429 |
| smb2.lease.v2_rename | Leases V2 | Lease V2 rename interaction not fully working | #429 |
| smb2.lease.v2_rename_target_overwrite | Leases V2 | Lease V2 rename target overwrite not fully working | #429 |

### Byte-Range Locks (Fix Candidate)

Byte-range locking is partially implemented but smbtorture lock tests still
fail due to incomplete lock contention and async lock handling.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.lock.valid-request | Locks | Lock request validation not fully working | #430 |
| smb2.lock.auto-unlock | Locks | Auto-unlock on close not fully working | #430 |
| smb2.lock.lock | Locks | Basic lock operation not fully working | #430 |
| smb2.lock.cancel | Locks | Lock cancel not fully working | #430 |
| smb2.lock.errorcode | Locks | Lock error codes not fully working | #430 |
| smb2.lock.zerobytelength | Locks | Zero-length lock not fully working | #430 |
| smb2.lock.unlock | Locks | Unlock operation not fully working | #430 |
| smb2.lock.multiple-unlock | Locks | Multiple unlock not fully working | #430 |
| smb2.lock.stacking | Locks | Lock stacking not fully working | #430 |
| smb2.lock.range | Locks | Lock range validation not fully working | #430 |
| smb2.lock.overlap | Locks | Overlapping locks not fully working | #430 |
| smb2.lock.replay_broken_windows | Locks | Lock replay not fully working | #430 |
| smb2.lock.replay_smb3_specification_durable | Locks | Lock replay with durable handles not fully working | #430 |
| smb2.lock.replay_smb3_specification_multi | Locks | Lock replay with multi-channel not fully working | #430 |
| smb2.lock.cancel-logoff | Locks | Lock cancel on logoff not fully working | #430 |
| smb2.lock.cancel-tdis | Locks | Blocking lock deadlocks dispatch goroutine (needs async LOCK with interim response) | #430 |
| smb2.lock.async | Locks | Blocking lock deadlocks dispatch goroutine (needs async LOCK with interim response) | #430 |
| smb2.lock.open-brlock-deadlock | Locks | Open + byte-range lock deadlock detection not working | #430 |
| smb2.lock.ctdb-delrec-deadlock | Locks | CTDB delete record deadlock not working | #430 |

### Rename (Fix Candidate)

Rename operations partially implemented but tests fail due to incomplete
share mode enforcement during rename.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.rename.share_delete_and_delete_access | Rename | Share delete + delete access rename not working | #433 |
| smb2.rename.no_share_delete_but_delete_access | Rename | Rename share mode enforcement not working | #433 |
| smb2.rename.no_share_delete_no_delete_access | Rename | Rename share mode enforcement not working | #433 |
| smb2.rename.rename_dir_openfile | Rename | Rename directory with open file not working | #433 |

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
| smb2.replay.dhv2-pending2l-vs-lease-sane | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending2l-vs-lease-windows | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending2o-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending2o-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending2o-vs-lease-sane | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending2o-vs-lease-windows | Replay | Replay pending lease handling not implemented | - |
| smb2.replay.dhv2-pending3n-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | - |
| smb2.replay.dhv2-pending3n-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | - |
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
| smb2.timestamps.delayed-2write | Timestamps | Delayed write timestamp update not working | #434 |
| smb2.timestamps.delayed-write-vs-flush | Timestamps | Delayed write vs flush timestamp not working | #434 |
| smb2.timestamps.delayed-write-vs-setbasic | Timestamps | Delayed write vs setbasic timestamp not working | #434 |
| smb2.timestamps.delayed-write-vs-seteof | Timestamps | Delayed write vs seteof timestamp not working | #434 |
| smb2.timestamps.freeze-thaw | Timestamps | CreationTime freeze/unfreeze not fully working | #434 |

## Changelog

### 2026-04-24 — Handle-scoped lease release fixes stale-record accumulation

smbtorture reuses fixed `LEASE1`/`LEASE2` constants across every test in the
`smb2.lease` subsuite. When a test closed its last handle, DittoFS's
`ReleaseLease(leaseKey)` removed every record matching the key across all
handleKey buckets — including records for opens on OTHER files that happened
to share the same constant. Worse, the `hasOtherOpen` check gating the
release compared by FileID alone, so any concurrent open anywhere with the
same key skipped the release entirely, leaving the current handle's record
orphaned in `unifiedLocks`.

The orphaned records accumulated across tests. By the time `break_twice`
ran, three `LEASE1` records sat in the same file's bucket (two from prior
tests where cleanup was skipped, one freshly granted). Every cross-key break
therefore dispatched three times, and `findLeaseByKey`-based lookups
(`SetLeaseEpoch`, `AcknowledgeLeaseBreak`) routinely returned the wrong
record — producing the `new_epoch got 0x2 should 0x13` and
`acknowledged state RW exceeds break-to state RH` signatures.

Fix:

- `pkg/metadata/lock` gains `ReleaseLeaseForHandle(ctx, handleKey, leaseKey)`
  that removes only records in one bucket, leaving other buckets intact.
- `SetLeaseEpoch` now iterates every record matching the key and updates
  each one, so V2 grant-epoch tracking works even when stale records
  briefly coexist.
- `internal/adapter/smb/lease` adds a corresponding `ReleaseLeaseForHandle`
  that only tears down the session/share mapping once the last record for
  the key is actually gone.
- `internal/adapter/smb/v2/handlers/close.go` scopes `hasOtherOpen` to opens
  on the SAME file (matches `MetadataHandle`, not just `FileID`) and always
  releases this handle's record — other files keep theirs.

Confirmed 2× stable — 2 additional tests now pass:

- `smb2.lease.break_twice`
- `smb2.lease.complex1`

**#429 lease cluster: 33 → 31 tests remaining.**

### 2026-04-24 — #429 Phase 2 matrix + delete-pending file-lease break

`fix(smb): compute lease break-to by sharing-violation — #429`
(commit `5c781938`) collapsed `BreakHandleLeasesForSMBOpen` +
`BreakWriteOnHandleLeasesForSMBOpen` into
`BreakLeasesOnOpenConflict(handleKey, excludeOwner, hasSharingViolation)`,
selecting the strip mask per MS-SMB2 3.3.4.7 and Samba
`source3/smbd/open.c::delay_for_oplock_fn` (violation → strip Handle;
no violation → strip Write). Matrix now passes `break_twice`'s
RWH→RW acks and `v2_complex2`'s RWH→RH, though both still fail on
downstream assertions tracked below.

A follow-up commit wired the file's own Handle-strip break into
`handleDeleteOnClose` (the teardown path that runs for
TDIS/LOGOFF/DISCONNECT-triggered deletes) and into
`BreakFileHandleLeasesOnDelete` on the lease manager. The closing
session is passed as `excludeOwner` so the break only fires against
OTHER holders — self-breaks were leaking into the next test's
`lease_break_info.count` and regressing `v1_bug15148`.

Confirmed 2× stable:

- `smb2.lease.initial_delete_tdis`
- `smb2.lease.initial_delete_logoff`
- `smb2.lease.initial_delete_disconnect`

**#429 lease cluster: 36 → 33 tests remaining.**

### 2026-04-24 — Lease subsuite unblocked + 6 #429 collapses

`fix(smb): bound Handle lease break wait on CREATE — #429`
(commit `931ed6f1`) added a 5 s timeout to `BreakHandleLeasesOnOpen`'s
wait, mirroring the existing `parentLeaseBreakWaitTimeout`. Without it,
`WaitForBreakCompletion` inherited the auth context (which only cancels
on session disconnect), so any non-acking client hung the conflicting
CREATE indefinitely. `lease.break_twice` alone hung 57 minutes,
consuming the entire suite-level smbtorture timeout and leaving the rest
of the lease subsuite untested.

With the bound, the lease subsuite now runs end-to-end in ~14 minutes.
Surfaced 6 lease tests as stably passing across 2 confirmation runs:

- `smb2.lease.nobreakself`
- `smb2.lease.v2_flags_breaking`
- `smb2.lease.v2_epoch1`
- `smb2.lease.v2_complex2`
- `smb2.lease.v1_bug15148`
- `smb2.lease.v2_bug15148`

Most were already correct post-#418 but masked by the unrunnable suite.
**#429 lease cluster: 42 → 36 tests remaining.**

A 3rd confirmation run is queued; if any test flips back, it will be
re-added to KNOWN_FAILURES with annotation.

### 2026-04-24 — Prune 20 collapsed entries after post-#418 baseline

Full smbtorture suite baseline against current `develop` (run
`smbtorture-2026-04-23_224339`) confirmed 22 previously-known failures now
pass. Pruned 20 of them (kept `smb2.create.mkdir-dup` and
`smb2.ioctl.network_interface_info` since their own reason text flags them
as flaky — single-run greens are insufficient evidence to remove).

Pruned entries:

- **Benchmarks**: `bench.echo`, `bench.path-contention-shared`, `bench.read`
- **Compound**: `compound_find.compound_find_close`
- **Create**: `create.bench-path-contention-shared`
- **Delete-on-Close**: `delete-on-close-perms.OVERWRITE_IF`
- **Deny modes**: `deny.deny1`, `deny.deny2`
- **Directory**: `dir.file-index`, `dir.large-files`, `dir.many`,
  `dir.sorted`
- **Directory leases**: `dirlease.v2_request_parent`
- **Durable V1** (chips #431): `durable-open.open-lease`
- **File IDs**: `fileid.unique`, `fileid.unique-dir`
- **Query Info**: `getinfo.granted`
- **Share modes**: `sharemode.access-sharemode`,
  `sharemode.sharemode-access`

Two empty fix-candidate section headers are removed:

- **Charset Edge Cases (Fix Candidate)**: only entry was `charset.Testing`.
  **Closes #435.**
- **Delete-on-Close OVERWRITE_IF (Fix Candidate)**: a placeholder header
  whose table was already empty (no entries had ever been filed under it).

Stats vs prior baseline (`smbtorture-2026-04-22_162101`, pre-#418):
160 PASS / 240 KNOWN / 0 NEW → 168 PASS / 233 KNOWN / 0 NEW.

Note: the `smb2.lease.*` subsuite hit the smbtorture per-suite timeout in
this run because `lease.break_twice` alone took 57 minutes (DittoFS
hangs the conflicting open instead of returning `STATUS_SHARING_VIOLATION`).
This is the next target for #429 work; baseline data for the lease cluster
is incomplete until that bug is resolved.

### 2026-04-23 — File tracking issues for fix-candidate clusters

Previously all "Fix Candidate" sections had their `Issue` column set to `-`
because no GH issue was tracking them. Filed eight issues so each fixable
test cluster has a home to land work against:

- **#429** — Leases (umbrella, 42 tests): break delivery + multi-client
  coordination + V2 epoch edge cases that remain after #417.
- **#430** — Byte-Range Locks (19 tests): async LOCK with interim response,
  contention + deadlock detection, replay.
- **#431** — Durable Handles V1 (13 tests): reconnect + lease coordination.
- **#432** — Durable Handles V2 (33 tests): reopen, disconnected-handle
  preservation/purge, app-instance, persistent-open flagged as separate
  feature work.
- **#433** — Rename (4 tests): share-mode enforcement during rename.
- **#434** — Timestamps (5 tests): delayed-write + freeze/thaw.
- **#435** — Charset (1 test): unicode surrogate pair handling.
- **#436** — `multichannel.leases.test3` spurious lease break on uncontested
  open (split out of #417 / PR #418 follow-up).

No test reclassifications or pass/fail transitions — pure issue tracking.

### 2026-04-17 — Reconcile credits subsuite after #378 grant fix (close #397)
The #378 credit-grant cap (commit `191e683e`) resolved both arms of #397: the
off-by-15 overgrant at `credits.c:460` (`granted 529, expected 514`) is gone
on every `*_ipc_max_async_credits` variant, and the follow-on smbtorture
talloc panic no longer fires — the whole `smb2.credits` subsuite now runs to
completion.

- Removed 3 entries that now pass against current HEAD:
  `smb2.credits.session_setup_credits_granted`,
  `smb2.credits.single_req_credits_granted`,
  `smb2.credits.skipped_mid`.
- Reclassified the 3 previously "unreachable" tests plus
  `1conn_ipc_max_async_credits` with their real new blockers. Every
  `*_ipc_max_async_credits` variant now fails at `credits.c:401` because
  named-pipe async READ returns `STATUS_SUCCESS` on an empty pipe instead of
  going async with `STATUS_PENDING` (Samba does this in
  `source3/smbd/smb2_read.c`). `1conn_notify_max_async_credits` fails at
  `credits.c:1281` because the server does not cap async operations at
  `max_async_credits=512` — all 514 reads pend instead of 511 pending + 3
  `STATUS_INSUFFICIENT_RESOURCES` (MS-SMB2 3.3.5.2.5).
- Linked the two multi-channel credits tests to #361.

Remaining IPC async work (named-pipe pending reads + `max_async_credits`
enforcement) is a separate feature area, not a credit-accounting bug.

### 2026-04-17 — Prune stale #268 entries
Removed 7 stale entries added in #268 as "newly reachable" failures after the
GMAC/read/write fixes in 27b2b8d0:

- Now passing reliably across full-suite runs:
  `smb2.scan.scan`, `smb2.delete-on-close-perms.BUG14427`
- Now skipping correctly via feature-flag guards (never consume a failure
  slot): `smb2.ioctl.dup_extents_len_beyond_dest`,
  `smb2.ioctl.dup_extents_len_zero`,
  `smb2.ioctl.dup_extents_compressed_src`,
  `smb2.multichannel.oplocks.test3_specification`,
  `smb2.multichannel.leases.test1`

Re-annotated 3 credits entries (also from #268) as *unreachable* rather than
failing: `credits.2conn_ipc_max_async_credits`, `multichannel_ipc_max_async_credits`,
`1conn_notify_max_async_credits`. These never run because the preceding
`credits.1conn_ipc_max_async_credits` failure (credit grant off-by-15) triggers
an smbtorture client-side talloc panic in the next tcase setup. Fixing the
grant arithmetic is tracked separately.

Dropped the now-empty "Scan" section.

### 2026-04-16 — Tier 1 cleanup after #362 signing fixes
Removed `smb2.scan.find` and `smb2.scan.setinfo` from known failures.
QUERY_DIRECTORY now rejects unsupported FileInformationClass values with
STATUS_INVALID_INFO_CLASS (MS-SMB2 3.3.5.18) instead of silently returning
FileBothDirectoryInformation, and the generic dispatch pipeline now always
emits the MS-SMB2 2.2.2 ERROR Response body for error statuses. Combined
with the #362 signing race fixes, these tests are now deterministic locally
across 5/3 consecutive runs.

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
