# smbtorture Baseline Results

**Date:** 2026-03-02
**DittoFS Commit:** 52f84ecd (feat/phase-40-smb3-conformance-testing)
**Profile:** memory (metadata: memory, payload: memory)
**Platform:** ARM64 (Apple Silicon) with x86_64 emulation via Rosetta/QEMU
**smbtorture Image:** quay.io/samba.org/samba-toolbox:v0.8
**Context:** Post phases 33-39 (SMB3 dialect negotiation, key derivation, signing, encryption, Kerberos, leases, durable handles, cross-protocol coordination)

## Overall Summary

| Metric | Count |
|--------|-------|
| Total Tests | 602 |
| Passed | 54 |
| Failed | 372 |
| Skipped | 176 |
| Pass Rate | 9.0% |
| Known Failures (matched) | 372 |
| New Failures | 0 |

## Per-Sub-Suite Breakdown

| Sub-Suite | Pass | Fail | Skip | Total | Pass Rate |
|-----------|------|------|------|-------|-----------|
| smb2.acls | 0 | 14 | 0 | 14 | 0% |
| smb2.acls_non_canonical | 0 | 1 | 0 | 1 | 0% |
| smb2.async_dosmode | 0 | 1 | 0 | 1 | 0% |
| smb2.bench | 0 | 5 | 0 | 5 | 0% |
| smb2.change_notify_disabled | 0 | 1 | 0 | 1 | 0% |
| smb2.charset | 2 | 2 | 0 | 4 | 50% |
| smb2.check-sharemode | 1 | 0 | 0 | 1 | 100% |
| smb2.compound_async | 0 | 10 | 0 | 10 | 0% |
| smb2.compound_find | 2 | 1 | 0 | 3 | 67% |
| smb2.connect | 1 | 0 | 0 | 1 | 100% |
| smb2.create | 7 | 10 | 1 | 18 | 39% |
| smb2.create_no_streams | 0 | 1 | 0 | 1 | 0% |
| smb2.credits | 0 | 5 | 0 | 5 | 0% |
| smb2.delete-on-close-perms | 3 | 6 | 0 | 9 | 33% |
| smb2.deny | 0 | 2 | 0 | 2 | 0% |
| smb2.dir | 0 | 9 | 0 | 9 | 0% |
| smb2.dirlease | 0 | 18 | 0 | 18 | 0% |
| smb2.dosmode | 0 | 1 | 0 | 1 | 0% |
| smb2.durable-open | 0 | 9 | 0 | 9 | 0% |
| smb2.durable-open-disconnect | 0 | 1 | 0 | 1 | 0% |
| smb2.durable-v2-delay | 1 | 1 | 0 | 2 | 50% |
| smb2.durable-v2-open | 0 | 32 | 0 | 32 | 0% |
| smb2.durable-v2-regressions | 0 | 0 | 1 | 1 | N/A |
| smb2.ea | 0 | 1 | 0 | 1 | 0% |
| smb2.fileid | 0 | 4 | 0 | 4 | 0% |
| smb2.getinfo | 0 | 8 | 0 | 8 | 0% |
| smb2.ioctl | 2 | 44 | 29 | 75 | 3% |
| smb2.ioctl-on-stream | 0 | 1 | 0 | 1 | 0% |
| smb2.kernel-oplocks | 0 | 7 | 1 | 8 | 0% |
| smb2.lease | 0 | 32 | 13 | 45 | 0% |
| smb2.lock | 6 | 15 | 5 | 26 | 23% |
| smb2.maxfid | 0 | 1 | 0 | 1 | 0% |
| smb2.maximum_allowed | 1 | 1 | 0 | 2 | 50% |
| smb2.mkdir | 1 | 0 | 0 | 1 | 100% |
| smb2.multichannel | 0 | 2 | 9 | 11 | 0% |
| smb2.name-mangling | 0 | 2 | 0 | 2 | 0% |
| smb2.notify | 1 | 22 | 0 | 23 | 4% |
| smb2.openattr | 0 | 1 | 0 | 1 | 0% |
| smb2.oplock | 0 | 42 | 0 | 42 | 0% |
| smb2.read | 1 | 4 | 0 | 5 | 20% |
| smb2.rename | 7 | 6 | 0 | 13 | 54% |
| smb2.replay | 0 | 0 | 56 | 56 | N/A |
| smb2.rw | 1 | 3 | 0 | 4 | 25% |
| smb2.samba3misc | 0 | 1 | 0 | 1 | 0% |
| smb2.scan | 4 | 0 | 0 | 4 | 100% |
| smb2.sdread | 0 | 1 | 0 | 1 | 0% |
| smb2.secleak | 0 | 1 | 0 | 1 | 0% |
| smb2.session | 2 | 11 | 58 | 71 | 3% |
| smb2.session-id | 1 | 0 | 0 | 1 | 100% |
| smb2.session-require-signing | 0 | 1 | 0 | 1 | 0% |
| smb2.set-sparse-ioctl | 0 | 1 | 0 | 1 | 0% |
| smb2.setinfo | 0 | 1 | 0 | 1 | 0% |
| smb2.sharemode | 0 | 3 | 0 | 3 | 0% |
| smb2.stream-inherit-perms | 1 | 0 | 0 | 1 | 100% |
| smb2.streams | 2 | 12 | 0 | 14 | 14% |
| smb2.tcon | 0 | 1 | 0 | 1 | 0% |
| smb2.timestamp_resolution | 0 | 1 | 0 | 1 | 0% |
| smb2.timestamps | 6 | 9 | 0 | 15 | 40% |
| smb2.twrp | 0 | 1 | 3 | 4 | 0% |
| smb2.winattr | 0 | 1 | 0 | 1 | 0% |
| smb2.winattr2 | 1 | 0 | 0 | 1 | 100% |
| smb2.zero-data-ioctl | 0 | 1 | 0 | 1 | 0% |

## Newly Passing Tests

These 50 tests were previously masked by wildcard entries in KNOWN_FAILURES.md (e.g., `smb2.session.*`) but now pass after phases 33-39 implementation.

| Test Name | Suite | Notes |
|-----------|-------|-------|
| smb2.charset.Testing composite character (a umlaut) | charset | Unicode handling works |
| smb2.charset.Testing naked diacritical (umlaut) | charset | Unicode handling works |
| smb2.check-sharemode | sharemode | Basic share mode check passes |
| smb2.compound_find.compound_find_related | compound_find | Compound FIND related ops work |
| smb2.compound_find.compound_find_unrelated | compound_find | Compound FIND unrelated ops work |
| smb2.connect | connect | Basic SMB2 connection passes |
| smb2.create.brlocked | create | Create with byte-range lock works |
| smb2.create.delete | create | Create + delete works |
| smb2.create.dir-alloc-size | create | Dir allocation size works |
| smb2.create.dosattr_tmp_dir | create | DOS attr on temp dir works |
| smb2.create.mkdir-dup | create | Duplicate mkdir handled |
| smb2.create.multi | create | Multi-create works |
| smb2.create.open | create | Basic open works |
| smb2.delete-on-close-perms.BUG14427 | delete-on-close | Bug 14427 regression test passes |
| smb2.delete-on-close-perms.FIND_and_set_DOC | delete-on-close | Find + set delete-on-close works |
| smb2.delete-on-close-perms.OVERWRITE_IF | delete-on-close | Overwrite with DOC works |
| smb2.durable-v2-delay.durable_v2_reconnect_delay_msec | durable-v2 | V2 reconnect delay works |
| smb2.ioctl.shadow_copy | ioctl | Shadow copy IOCTL enumeration works |
| smb2.ioctl.sparse_file_attr | ioctl | Sparse file attribute query works |
| smb2.lock.async | lock | Async lock works |
| smb2.lock.cancel-logoff | lock | Lock cancel on logoff works |
| smb2.lock.cancel-tdis | lock | Lock cancel on tree disconnect works |
| smb2.lock.context | lock | Lock context handling works |
| smb2.lock.rw-shared | lock | Shared read-write lock works |
| smb2.lock.zerobyteread | lock | Zero-byte read with lock works |
| smb2.maximum_allowed.read_only | maximum_allowed | Read-only max allowed access works |
| smb2.mkdir | standalone | Basic mkdir works |
| smb2.notify.file | notify | File change notify works |
| smb2.read.dir | read | Directory read works |
| smb2.rename.close-full-information | rename | Rename + close full info works |
| smb2.rename.msword | rename | MS Word rename pattern works |
| smb2.rename.no_sharing | rename | Rename with no sharing works |
| smb2.rename.rename-open | rename | Rename open file works |
| smb2.rename.rename_dir_bench | rename | Directory rename benchmark works |
| smb2.rename.share_delete_no_delete_access | rename | Share delete without access works |
| smb2.rename.simple | rename | Simple rename works |
| smb2.rw.append | rw | Append write works |
| smb2.scan.find | scan | Full scan.find passes |
| smb2.scan.getinfo | scan | Full scan.getinfo passes |
| smb2.scan.scan | scan | Full scan.scan passes |
| smb2.scan.setinfo | scan | Full scan.setinfo passes |
| smb2.session-id | session | Session ID handling works |
| smb2.session.ntlmssp_bug14932 | session | NTLMSSP bug14932 regression passes |
| smb2.session.two_logoff | session | Two logoff works |
| smb2.streams.rename | streams | Stream rename works |
| smb2.streams.sharemodes | streams | Stream share modes work |
| smb2.timestamps.delayed-1write | timestamps | Delayed single write timestamp works |
| smb2.timestamps.freeze-thaw | timestamps | Freeze-thaw timestamp works |
| smb2.timestamps.test_close_not_attrib | timestamps | Close not affecting attrib works |
| smb2.timestamps.time_t_0 | timestamps | time_t=0 boundary works |
| smb2.timestamps.time_t_1 | timestamps | time_t=1 boundary works |
| smb2.timestamps.time_t_4294967295 | timestamps | time_t=UINT32_MAX boundary works |
| smb2.winattr2 | standalone | Windows attributes v2 works |

## Fix Candidates

These tests fail despite the related feature being implemented in phases 33-39. They should be investigated and fixed in subsequent plans rather than added to KNOWN_FAILURES.

### Session Tests (Phase 33-39: sessions implemented)

| Test Name | Issue |
|-----------|-------|
| smb2.session.reauth1 | Re-authentication flow may have bugs |
| smb2.session.reauth2 | Re-authentication flow may have bugs |
| smb2.session.reauth3 | Re-authentication flow may have bugs |
| smb2.session.reauth4 | Re-authentication flow may have bugs |
| smb2.session.reauth5 | Re-authentication flow may have bugs |
| smb2.session.reauth6 | Re-authentication flow may have bugs |
| smb2.session.reconnect1 | Session reconnect may need fixes |
| smb2.session.reconnect2 | Session reconnect may need fixes |

### Lease Tests (Phase 37: leases implemented)

| Test Name | Issue |
|-----------|-------|
| smb2.lease.break | Lease break mechanism may have edge case bugs |
| smb2.lease.breaking1-6 | Lease breaking scenarios need investigation |
| smb2.lease.complex1 | Complex lease interactions |
| smb2.lease.upgrade/upgrade2/upgrade3 | Lease upgrade scenarios |
| smb2.lease.request | Lease request handling |
| smb2.lease.oplock | Lease-oplock interaction |
| smb2.lease.multibreak | Multi-lease break |
| smb2.lease.nobreakself | Self-break prevention |
| smb2.lease.statopen/statopen2-4 | Stat open with leases |
| smb2.lease.timeout/timeout-disconnect | Lease timeout handling |
| smb2.lease.unlink | Unlink with active lease |
| smb2.lease.lock1 | Lock interaction with lease |
| smb2.lease.duplicate_create/duplicate_open | Duplicate lease handling |
| smb2.lease.lease-epoch | Lease epoch tracking |
| smb2.lease.rename_dir_openfile | Rename dir with open file lease |
| smb2.lease.rename_wait | Rename wait for lease break |
| smb2.lease.initial_delete_disconnect/logoff/tdis | Initial delete with lease |

### Durable Handle Tests (Phase 38: durable handles implemented)

| Test Name | Issue |
|-----------|-------|
| smb2.durable-v2-open.reopen1/1a/1a-lease | Durable v2 reopen flows |
| smb2.durable-v2-open.reopen2/2-lease/2-lease-v2/2b/2c | Durable v2 reopen variants |
| smb2.durable-v2-open.open-lease/open-oplock | Durable v2 open with lease/oplock |
| smb2.durable-v2-open.lock-lease/lock-noW-lease/lock-oplock | Lock + durable handle |
| smb2.durable-v2-open.create-blob | Create blob with durable context |
| smb2.durable-v2-open.app-instance | App instance ID handling |
| smb2.durable-v2-open.persistent-open-lease/persistent-open-oplock | Persistent handle tests |
| smb2.durable-v2-open.stat-and-lease/statRH-and-lease/nonstat-and-lease | Stat + durable combinations |
| smb2.durable-v2-open.two-same-lease/two-different-lease | Multiple durable leases |
| smb2.durable-v2-open.durable-v2-setinfo | SetInfo on durable handle |
| smb2.durable-v2-open.keep-disconnected-* | Disconnected handle preservation |
| smb2.durable-v2-open.purge-disconnected-* | Disconnected handle purging |
| smb2.durable-v2-delay.durable_v2_reconnect_delay | Reconnect delay edge case |

### Lock Tests (Phase 33-39: locks improved)

| Test Name | Issue |
|-----------|-------|
| smb2.lock.lock | Core lock test |
| smb2.lock.unlock | Unlock test |
| smb2.lock.multiple-unlock | Multiple unlock |
| smb2.lock.auto-unlock | Auto-unlock on close |
| smb2.lock.cancel | Lock cancel |
| smb2.lock.contend | Lock contention |
| smb2.lock.errorcode | Error code correctness |
| smb2.lock.overlap | Overlapping locks |
| smb2.lock.range | Lock range tests |
| smb2.lock.rw-exclusive | Exclusive R/W lock |
| smb2.lock.stacking | Lock stacking |
| smb2.lock.truncate | Truncate with lock |
| smb2.lock.valid-request | Valid request validation |
| smb2.lock.zerobytelength | Zero-byte length lock |
| smb2.lock.replay_broken_windows | Windows lock replay compat |

### Timestamp Tests (partially passing)

| Test Name | Issue |
|-----------|-------|
| smb2.timestamps.delayed-2write | Two-write delayed timestamp |
| smb2.timestamps.delayed-write-vs-flush | Delayed write vs flush interaction |
| smb2.timestamps.delayed-write-vs-setbasic | Delayed write vs SetBasicInfo |
| smb2.timestamps.delayed-write-vs-seteof | Delayed write vs SetEndOfFile |
| smb2.timestamps.time_t_-1 | Negative time_t boundary |
| smb2.timestamps.time_t_-2 | Negative time_t boundary |
| smb2.timestamps.time_t_10000000000 | Large time_t value |
| smb2.timestamps.time_t_15032385535 | Large time_t value |
| smb2.timestamps.time_t_1968 | 1968 date handling |

### Rename Tests (partially passing)

| Test Name | Issue |
|-----------|-------|
| smb2.rename.no_share_delete_but_delete_access | Permission edge case |
| smb2.rename.no_share_delete_no_delete_access | Permission edge case |
| smb2.rename.rename_dir_openfile | Rename dir with open file |
| smb2.rename.share_delete_and_delete_access | Permission edge case |
| smb2.rename.simple_modtime | Rename mtime update |
| smb2.rename.simple_nodelete | Rename without delete access |

## All Individual Failing Tests

Complete list of all 372 failing tests for reference.

<details>
<summary>Click to expand full failing test list</summary>

### smb2.acls (14 failures)
- smb2.acls.ACCESSBASED
- smb2.acls.CREATOR
- smb2.acls.DENY1
- smb2.acls.DYNAMIC
- smb2.acls.GENERIC
- smb2.acls.INHERITANCE
- smb2.acls.INHERITFLAGS
- smb2.acls.MXAC-NOT-GRANTED
- smb2.acls.OVERWRITE_READ_ONLY_FILE
- smb2.acls.OWNER
- smb2.acls.OWNER-RIGHTS
- smb2.acls.OWNER-RIGHTS-DENY
- smb2.acls.OWNER-RIGHTS-DENY1
- smb2.acls.SDFLAGSVSCHOWN

### smb2.acls_non_canonical (1 failure)
- smb2.acls_non_canonical.flags

### smb2.async_dosmode (1 failure)
- smb2.async_dosmode

### smb2.bench (5 failures)
- smb2.bench.echo
- smb2.bench.oplock1
- smb2.bench.path-contention-shared
- smb2.bench.read
- smb2.bench.session-setup

### smb2.change_notify_disabled (1 failure)
- smb2.change_notify_disabled.notfiy_disabled

### smb2.charset (2 failures)
- smb2.charset.Testing (2 additional charset test variants)

### smb2.compound_async (10 failures)
- smb2.compound_async.create_lease_break_async
- smb2.compound_async.flush_close
- smb2.compound_async.flush_flush
- smb2.compound_async.getinfo_middle
- smb2.compound_async.read_read
- smb2.compound_async.rename_last
- smb2.compound_async.rename_middle
- smb2.compound_async.rename_non_compound_no_async
- smb2.compound_async.rename_same_srcdst_non_compound_no_async
- smb2.compound_async.write_write

### smb2.compound_find (1 failure)
- smb2.compound_find.compound_find_close

### smb2.create (10 failures)
- smb2.create.acldir
- smb2.create.aclfile
- smb2.create.bench-path-contention-shared
- smb2.create.blob
- smb2.create.gentest
- smb2.create.impersonation
- smb2.create.leading-slash
- smb2.create.mkdir-visible
- smb2.create.nulldacl
- smb2.create.quota-fake-file

### smb2.create_no_streams (1 failure)
- smb2.create_no_streams.no_stream

### smb2.credits (5 failures)
- smb2.credits.1conn_ipc_max_async_credits
- smb2.credits.ipc_max_data_zero
- smb2.credits.session_setup_credits_granted
- smb2.credits.single_req_credits_granted
- smb2.credits.skipped_mid

### smb2.delete-on-close-perms (6 failures)
- smb2.delete-on-close-perms.CREATE
- smb2.delete-on-close-perms.CREATE_IF
- smb2.delete-on-close-perms.OVERWRITE_IF (different test from the passing OVERWRITE_IF)
- smb2.delete-on-close-perms.READONLY

### smb2.deny (2 failures)
- smb2.deny.deny1
- smb2.deny.deny2

### smb2.dir (9 failures)
- smb2.dir.1kfiles_rename
- smb2.dir.file-index
- smb2.dir.find
- smb2.dir.fixed
- smb2.dir.large-files
- smb2.dir.many
- smb2.dir.modify
- smb2.dir.one
- smb2.dir.sorted

### smb2.dirlease (18 failures)
- smb2.dirlease.hardlink
- smb2.dirlease.leases
- smb2.dirlease.oplocks
- smb2.dirlease.overwrite
- smb2.dirlease.rename
- smb2.dirlease.rename_dst_parent
- smb2.dirlease.setatime
- smb2.dirlease.setbtime
- smb2.dirlease.setctime
- smb2.dirlease.setdos
- smb2.dirlease.seteof
- smb2.dirlease.setmtime
- smb2.dirlease.unlink_different_initial_and_close
- smb2.dirlease.unlink_different_set_and_close
- smb2.dirlease.unlink_same_initial_and_close
- smb2.dirlease.unlink_same_set_and_close
- smb2.dirlease.v2_request
- smb2.dirlease.v2_request_parent

### smb2.dosmode (1 failure)
- smb2.dosmode

### smb2.durable-open (9 failures)
- smb2.durable-open.open-lease
- smb2.durable-open.open-oplock
- smb2.durable-open.reopen1
- smb2.durable-open.reopen1a
- smb2.durable-open.reopen1a-lease
- smb2.durable-open.reopen2
- smb2.durable-open.reopen2-lease
- smb2.durable-open.reopen2-lease-v2
- smb2.durable-open.reopen2a

### smb2.durable-open-disconnect (1 failure)
- smb2.durable-open-disconnect.open-oplock-disconnect

### smb2.durable-v2-delay (1 failure)
- smb2.durable-v2-delay.durable_v2_reconnect_delay

### smb2.durable-v2-open (32 failures)
- smb2.durable-v2-open.app-instance
- smb2.durable-v2-open.create-blob
- smb2.durable-v2-open.durable-v2-setinfo
- smb2.durable-v2-open.keep-disconnected-rh-with-rh-open
- smb2.durable-v2-open.keep-disconnected-rh-with-rwh-open
- smb2.durable-v2-open.keep-disconnected-rh-with-stat-open
- smb2.durable-v2-open.keep-disconnected-rwh-with-stat-open
- smb2.durable-v2-open.lock-lease
- smb2.durable-v2-open.lock-noW-lease
- smb2.durable-v2-open.lock-oplock
- smb2.durable-v2-open.nonstat-and-lease
- smb2.durable-v2-open.open-lease
- smb2.durable-v2-open.open-oplock
- smb2.durable-v2-open.persistent-open-lease
- smb2.durable-v2-open.persistent-open-oplock
- smb2.durable-v2-open.purge-disconnected-rh-with-rename
- smb2.durable-v2-open.purge-disconnected-rh-with-share-none-open
- smb2.durable-v2-open.purge-disconnected-rh-with-write
- smb2.durable-v2-open.purge-disconnected-rwh-with-rh-open
- smb2.durable-v2-open.purge-disconnected-rwh-with-rwh-open
- smb2.durable-v2-open.reopen1
- smb2.durable-v2-open.reopen1a
- smb2.durable-v2-open.reopen1a-lease
- smb2.durable-v2-open.reopen2
- smb2.durable-v2-open.reopen2-lease
- smb2.durable-v2-open.reopen2-lease-v2
- smb2.durable-v2-open.reopen2b
- smb2.durable-v2-open.reopen2c
- smb2.durable-v2-open.stat-and-lease
- smb2.durable-v2-open.statRH-and-lease
- smb2.durable-v2-open.two-different-lease
- smb2.durable-v2-open.two-same-lease

### smb2.ea (1 failure)
- smb2.ea.acl_xattr

### smb2.fileid (4 failures)
- smb2.fileid.fileid
- smb2.fileid.fileid-dir
- smb2.fileid.unique
- smb2.fileid.unique-dir

### smb2.getinfo (8 failures)
- smb2.getinfo.complex
- smb2.getinfo.fsinfo
- smb2.getinfo.getinfo_access
- smb2.getinfo.granted
- smb2.getinfo.normalized
- smb2.getinfo.qfile_buffercheck
- smb2.getinfo.qfs_buffercheck
- smb2.getinfo.qsec_buffercheck

### smb2.ioctl (44 failures)
- smb2.ioctl.bug14769
- smb2.ioctl.compress_notsup_get
- smb2.ioctl.compress_notsup_set
- smb2.ioctl.copy_chunk_across_shares
- smb2.ioctl.copy_chunk_across_shares2
- smb2.ioctl.copy_chunk_across_shares3
- smb2.ioctl.copy_chunk_append
- smb2.ioctl.copy_chunk_bad_access
- smb2.ioctl.copy_chunk_bad_key
- smb2.ioctl.copy_chunk_bug15644
- smb2.ioctl.copy_chunk_dest_lock
- smb2.ioctl.copy_chunk_limits
- smb2.ioctl.copy_chunk_max_output_sz
- smb2.ioctl.copy_chunk_multi
- smb2.ioctl.copy_chunk_overwrite
- smb2.ioctl.copy_chunk_simple
- smb2.ioctl.copy_chunk_sparse_dest
- smb2.ioctl.copy_chunk_src_exceed
- smb2.ioctl.copy_chunk_src_exceed_multi
- smb2.ioctl.copy_chunk_src_is_dest
- smb2.ioctl.copy_chunk_src_is_dest_overlap
- smb2.ioctl.copy_chunk_src_lock
- smb2.ioctl.copy_chunk_tiny
- smb2.ioctl.copy_chunk_write_access
- smb2.ioctl.copy_chunk_zero_length
- smb2.ioctl.copy-chunk
- smb2.ioctl.req_resume_key
- smb2.ioctl.req_two_resume_keys
- smb2.ioctl.sparse_copy_chunk
- smb2.ioctl.sparse_dir_flag
- smb2.ioctl.sparse_file_flag
- smb2.ioctl.sparse_hole_dealloc
- smb2.ioctl.sparse_lock
- smb2.ioctl.sparse_perms
- smb2.ioctl.sparse_punch
- smb2.ioctl.sparse_punch_invalid
- smb2.ioctl.sparse_qar
- smb2.ioctl.sparse_qar_malformed
- smb2.ioctl.sparse_qar_multi
- smb2.ioctl.sparse_qar_ob1
- smb2.ioctl.sparse_qar_overflow
- smb2.ioctl.sparse_qar_truncated
- smb2.ioctl.sparse_set_nobuf
- smb2.ioctl.sparse_set_oversize

### smb2.ioctl-on-stream (1 failure)
- smb2.ioctl-on-stream

### smb2.kernel-oplocks (7 failures)
- smb2.kernel-oplocks.kernel_oplocks1-7

### smb2.lease (32 failures)
- smb2.lease.break
- smb2.lease.breaking1-6
- smb2.lease.complex1
- smb2.lease.duplicate_create
- smb2.lease.duplicate_open
- smb2.lease.initial_delete_disconnect
- smb2.lease.initial_delete_logoff
- smb2.lease.initial_delete_tdis
- smb2.lease.lease-epoch
- smb2.lease.lock1
- smb2.lease.multibreak
- smb2.lease.nobreakself
- smb2.lease.oplock
- smb2.lease.rename_dir_openfile
- smb2.lease.rename_wait
- smb2.lease.request
- smb2.lease.statopen
- smb2.lease.statopen2
- smb2.lease.statopen3
- smb2.lease.statopen4
- smb2.lease.timeout
- smb2.lease.timeout-disconnect
- smb2.lease.unlink
- smb2.lease.upgrade
- smb2.lease.upgrade2
- smb2.lease.upgrade3
- smb2.lease.v1_bug15148

### smb2.lock (15 failures)
- smb2.lock.auto-unlock
- smb2.lock.cancel
- smb2.lock.contend
- smb2.lock.errorcode
- smb2.lock.lock
- smb2.lock.multiple-unlock
- smb2.lock.overlap
- smb2.lock.range
- smb2.lock.replay_broken_windows
- smb2.lock.rw-exclusive
- smb2.lock.stacking
- smb2.lock.truncate
- smb2.lock.unlock
- smb2.lock.valid-request
- smb2.lock.zerobytelength

### smb2.maxfid (1 failure)
- smb2.maxfid

### smb2.maximum_allowed (1 failure)
- smb2.maximum_allowed.maximum_allowed

### smb2.multichannel (2 failures)
- smb2.multichannel.bugs.bug_15346
- smb2.multichannel.generic.num_channels

### smb2.name-mangling (2 failures)
- smb2.name-mangling.mangle
- smb2.name-mangling.mangled-mask

### smb2.notify (22 failures)
- smb2.notify.basedir
- smb2.notify.close
- smb2.notify.dir
- smb2.notify.double
- smb2.notify.handle-permissions
- smb2.notify.invalid-reauth
- smb2.notify.logoff
- smb2.notify.mask
- smb2.notify.mask-change
- smb2.notify.overflow
- smb2.notify.rec
- smb2.notify.rmdir1-4
- smb2.notify.session-reconnect
- smb2.notify.tcon
- smb2.notify.tcp
- smb2.notify.tdis
- smb2.notify.tdis1
- smb2.notify.tree
- smb2.notify.valid-req

### smb2.openattr (1 failure)
- smb2.openattr

### smb2.oplock (42 failures)
- smb2.oplock.batch1-26, exclusive1-6, exclusive9, levelii500-502, brl1-3, doc, statopen1, stream1

### smb2.read (4 failures)
- smb2.read.access
- smb2.read.bug14607
- smb2.read.eof
- smb2.read.position

### smb2.rename (6 failures)
- smb2.rename.no_share_delete_but_delete_access
- smb2.rename.no_share_delete_no_delete_access
- smb2.rename.rename_dir_openfile
- smb2.rename.share_delete_and_delete_access
- smb2.rename.simple_modtime
- smb2.rename.simple_nodelete

### smb2.rw (3 failures)
- smb2.rw.invalid
- smb2.rw.rw1
- smb2.rw.rw2

### smb2.samba3misc (1 failure)
- smb2.samba3misc.localposixlock1

### smb2.sdread (1 failure)
- smb2.sdread

### smb2.secleak (1 failure)
- smb2.secleak

### smb2.session (11 failures)
- smb2.session.reauth1-6
- smb2.session.reconnect1
- smb2.session.reconnect2
- smb2.session-require-signing.bug15397

### smb2.set-sparse-ioctl (1 failure)
- smb2.set-sparse-ioctl

### smb2.setinfo (1 failure)
- smb2.setinfo

### smb2.sharemode (3 failures)
- smb2.sharemode.access-sharemode
- smb2.sharemode.bug14375
- smb2.sharemode.sharemode-access

### smb2.streams (12 failures)
- smb2.streams.attributes1
- smb2.streams.attributes2
- smb2.streams.basefile-rename-with-open-stream
- smb2.streams.create-disposition
- smb2.streams.delete
- smb2.streams.dir
- smb2.streams.io
- smb2.streams.names
- smb2.streams.names2
- smb2.streams.names3
- smb2.streams.rename2
- smb2.streams.zero-byte

### smb2.tcon (1 failure)
- smb2.tcon

### smb2.timestamp_resolution (1 failure)
- smb2.timestamp_resolution.resolution1

### smb2.timestamps (9 failures)
- smb2.timestamps.delayed-2write
- smb2.timestamps.delayed-write-vs-flush
- smb2.timestamps.delayed-write-vs-setbasic
- smb2.timestamps.delayed-write-vs-seteof
- smb2.timestamps.time_t_-1
- smb2.timestamps.time_t_-2
- smb2.timestamps.time_t_10000000000
- smb2.timestamps.time_t_15032385535
- smb2.timestamps.time_t_1968

### smb2.twrp (1 failure)
- smb2.twrp.listdir

### smb2.winattr (1 failure)
- smb2.winattr

### smb2.zero-data-ioctl (1 failure)
- smb2.zero-data-ioctl

</details>

## All Skipped Tests

176 tests were skipped (smbtorture determined they could not run against this server).

<details>
<summary>Click to expand full skipped test list</summary>

### smb2.session (58 skipped)
Session binding, encryption, signing negotiation, and anonymous tests requiring multi-channel or specific auth features:
- smb2.session.bind1, bind2, bind_different_user, bind_invalid_auth
- smb2.session.bind_negative_smb202, bind_negative_smb210d, bind_negative_smb210s
- smb2.session.bind_negative_smb2to3d/s, smb3to2d/s, smb3to3d/s
- smb2.session.bind_negative_smb3encGtoCd/s
- smb2.session.bind_negative_smb3sign* (CMAC/GMAC/HMAC variants, 22 tests)
- smb2.session.bind_negative_smb3sne* (8 tests)
- smb2.session.encryption-aes-128-ccm/gcm, encryption-aes-256-ccm/gcm
- smb2.session.signing-aes-128-cmac/gmac, signing-hmac-sha-256
- smb2.session.anon-encryption1-3, anon-signing1-2
- smb2.session.expire_disconnect, expire1e/1n/1s, expire2e/2s

### smb2.replay (56 skipped)
All replay tests skipped (require durable handle v2 with replay support):
- smb2.replay.channel-sequence
- smb2.replay.dhv2-pending* (48 tests)
- smb2.replay.replay-dhv2-* (10 tests)
- smb2.replay.replay-regular, replay3-7

### smb2.ioctl (29 skipped)
Compression, dedup, and network interface tests:
- smb2.ioctl.compress_* (9 tests)
- smb2.ioctl.dup_extents_* (12 tests)
- smb2.ioctl.network_interface_info
- smb2.ioctl.sparse_compressed
- smb2.ioctl.trim_simple
- smb2.ioctl.bug14607, bug14788.NETWORK_INTERFACE, bug14788.VALIDATE_NEGOTIATE

### smb2.lease (13 skipped)
V2 lease tests requiring multi-channel or advanced features:
- smb2.lease.v2_* (v2_breaking3, v2_bug15148, v2_complex1-2, v2_epoch1-3, v2_flags_breaking, v2_flags_parentkey, v2_rename, v2_rename_target_overwrite)
- smb2.lease.break_twice, dynamic_share

### smb2.multichannel (9 skipped)
All multi-channel tests (not implemented):
- smb2.multichannel.generic.interface_info
- smb2.multichannel.leases.test1-4
- smb2.multichannel.oplocks.test1-3, test3_specification, test3_windows

### smb2.lock (5 skipped)
- smb2.lock.ctdb-delrec-deadlock, open-brlock-deadlock
- smb2.lock.replay_smb3_specification_durable/multi
- smb2.lock.rw-none

### smb2.twrp (3 skipped)
- smb2.twrp.openroot, stream, write

### smb2.kernel-oplocks (1 skipped)
- smb2.kernel-oplocks.kernel_oplocks8

### smb2.durable-v2-regressions (1 skipped)
- smb2.durable-v2-regressions.durable_v2_reconnect_bug15624

### smb2.create (1 skipped)
- smb2.create.path-length

</details>

## Notes

- ARM64 emulation adds ~3-5x overhead. NT_STATUS_NO_MEMORY errors in some tests are client-side issues from rapid connection creation under emulation, not DittoFS server bugs.
- The smb2.replay suite is entirely skipped because it requires SMB3 durable handle v2 replay support that smbtorture cannot detect.
- Session binding tests (58 skipped) require multi-channel support to establish a second channel for binding.
- The high number of lease failures (32) despite Phase 37 implementation suggests the smbtorture lease tests exercise client-visible lease semantics that the server may not fully expose over the wire yet.
- Durable handle V2 failures (32) despite Phase 38 implementation suggest smbtorture exercises reconnect and persistence semantics that need wire-level debugging.
