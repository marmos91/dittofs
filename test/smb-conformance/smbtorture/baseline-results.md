# smbtorture Baseline Results

> Written by `dt known baseline` from one cell's results in a run. Don't edit it by hand:
> write it again, and commit it through a pull request. It grades nothing: a run is graded
> against the suite's `known.yaml`.

**Date:** 2026-10-20
**DittoFS Commit:** 1a8bb680
**Cell:** smbtorture/badger-s3
**Platform:** linux
**smbtorture:** smbtorture 4.22.6

## Overall Summary

| Metric | Count |
|--------|-------|
| Total Tests | 633 |
| Passed | 550 |
| Failed | 43 |
| Known failures | 43 |
| New failures | 0 |
| Skipped | 40 |
| Pass Rate | 86.9% |

## Per-Sub-Suite Breakdown

| Sub-Suite | Pass | Fail | Skip | Total | Pass Rate |
|-----------|------|------|------|-------|-----------|
| smb2.acls | 14 | 0 | 0 | 14 | 100% |
| smb2.acls_non_canonical | 1 | 0 | 0 | 1 | 100% |
| smb2.async_dosmode | 1 | 0 | 0 | 1 | 100% |
| smb2.change_notify_disabled | 1 | 0 | 0 | 1 | 100% |
| smb2.charset | 3 | 1 | 0 | 4 | 75% |
| smb2.check-sharemode | 1 | 0 | 0 | 1 | 100% |
| smb2.compound | 20 | 0 | 0 | 20 | 100% |
| smb2.compound_async | 10 | 0 | 0 | 10 | 100% |
| smb2.compound_find | 3 | 0 | 0 | 3 | 100% |
| smb2.connect | 1 | 0 | 0 | 1 | 100% |
| smb2.create | 14 | 2 | 1 | 17 | 88% |
| smb2.create_no_streams | 1 | 0 | 0 | 1 | 100% |
| smb2.credits | 10 | 0 | 0 | 10 | 100% |
| smb2.delete-on-close-perms | 9 | 0 | 0 | 9 | 100% |
| smb2.deny | 2 | 0 | 0 | 2 | 100% |
| smb2.dir | 9 | 0 | 0 | 9 | 100% |
| smb2.dirlease | 17 | 0 | 0 | 17 | 100% |
| smb2.dosmode | 0 | 1 | 0 | 1 | 0% |
| smb2.durable-open | 23 | 1 | 0 | 24 | 96% |
| smb2.durable-open-disconnect | 1 | 0 | 0 | 1 | 100% |
| smb2.durable-v2-delay | 2 | 0 | 0 | 2 | 100% |
| smb2.durable-v2-open | 32 | 0 | 0 | 32 | 100% |
| smb2.durable-v2-regressions | 0 | 0 | 1 | 1 | N/A |
| smb2.ea | 1 | 0 | 0 | 1 | 100% |
| smb2.fileid | 4 | 0 | 0 | 4 | 100% |
| smb2.getinfo | 8 | 0 | 0 | 8 | 100% |
| smb2.ioctl | 55 | 0 | 20 | 75 | 100% |
| smb2.ioctl-on-stream | 1 | 0 | 0 | 1 | 100% |
| smb2.kernel-oplocks | 5 | 2 | 1 | 8 | 71% |
| smb2.lease | 44 | 0 | 1 | 45 | 100% |
| smb2.lock | 22 | 0 | 4 | 26 | 100% |
| smb2.maxfid | 1 | 0 | 0 | 1 | 100% |
| smb2.maximum_allowed | 2 | 0 | 0 | 2 | 100% |
| smb2.mkdir | 1 | 0 | 0 | 1 | 100% |
| smb2.multichannel | 6 | 5 | 0 | 11 | 55% |
| smb2.name-mangling | 0 | 2 | 0 | 2 | 0% |
| smb2.notify | 20 | 1 | 0 | 21 | 95% |
| smb2.notify-inotify | 1 | 0 | 0 | 1 | 100% |
| smb2.openattr | 1 | 0 | 0 | 1 | 100% |
| smb2.oplock | 41 | 1 | 0 | 42 | 98% |
| smb2.read | 4 | 0 | 1 | 5 | 100% |
| smb2.rename | 13 | 0 | 0 | 13 | 100% |
| smb2.replay | 34 | 21 | 1 | 56 | 62% |
| smb2.rw | 4 | 0 | 0 | 4 | 100% |
| smb2.samba3misc | 0 | 1 | 0 | 1 | 0% |
| smb2.scan | 3 | 0 | 0 | 3 | 100% |
| smb2.sdread | 1 | 0 | 0 | 1 | 100% |
| smb2.secleak | 1 | 0 | 0 | 1 | 100% |
| smb2.session | 63 | 1 | 7 | 71 | 98% |
| smb2.session-id | 1 | 0 | 0 | 1 | 100% |
| smb2.session-require-signing | 1 | 0 | 0 | 1 | 100% |
| smb2.set-sparse-ioctl | 0 | 1 | 0 | 1 | 0% |
| smb2.setinfo | 1 | 0 | 0 | 1 | 100% |
| smb2.sharemode | 3 | 0 | 0 | 3 | 100% |
| smb2.stream-inherit-perms | 1 | 0 | 0 | 1 | 100% |
| smb2.streams | 14 | 0 | 0 | 14 | 100% |
| smb2.tcon | 1 | 0 | 0 | 1 | 100% |
| smb2.timestamp_resolution | 0 | 1 | 0 | 1 | 0% |
| smb2.timestamps | 15 | 0 | 0 | 15 | 100% |
| smb2.twrp | 0 | 1 | 3 | 4 | 0% |
| smb2.winattr | 1 | 0 | 0 | 1 | 100% |
| smb2.winattr2 | 1 | 0 | 0 | 1 | 100% |
| smb2.zero-data-ioctl | 0 | 1 | 0 | 1 | 0% |

## Passing Tests (550)

<details><summary>Show 550 test(s)</summary>

### smb2.acls (14)
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

### smb2.acls_non_canonical (1)
- smb2.acls_non_canonical.flags

### smb2.async_dosmode (1)
- smb2.async_dosmode

### smb2.change_notify_disabled (1)
- smb2.change_notify_disabled.notfiy_disabled

### smb2.charset (3)
- smb2.charset.Testing
- smb2.charset.Testing
- smb2.charset.Testing

### smb2.check-sharemode (1)
- smb2.check-sharemode

### smb2.compound (20)
- smb2.compound.compound-break
- smb2.compound.compound-padding
- smb2.compound.create-write-close
- smb2.compound.interim1
- smb2.compound.interim2
- smb2.compound.interim3
- smb2.compound.invalid1
- smb2.compound.invalid2
- smb2.compound.invalid3
- smb2.compound.invalid4
- smb2.compound.related1
- smb2.compound.related2
- smb2.compound.related3
- smb2.compound.related4
- smb2.compound.related5
- smb2.compound.related6
- smb2.compound.related7
- smb2.compound.related8
- smb2.compound.related9
- smb2.compound.unrelated1

### smb2.compound_async (10)
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

### smb2.compound_find (3)
- smb2.compound_find.compound_find_close
- smb2.compound_find.compound_find_related
- smb2.compound_find.compound_find_unrelated

### smb2.connect (1)
- smb2.connect

### smb2.create (14)
- smb2.create.acldir
- smb2.create.aclfile
- smb2.create.blob
- smb2.create.brlocked
- smb2.create.delete
- smb2.create.dir-alloc-size
- smb2.create.dosattr_tmp_dir
- smb2.create.impersonation
- smb2.create.leading-slash
- smb2.create.mkdir-dup
- smb2.create.mkdir-visible
- smb2.create.multi
- smb2.create.nulldacl
- smb2.create.open

### smb2.create_no_streams (1)
- smb2.create_no_streams.no_stream

### smb2.credits (10)
- smb2.credits.1conn_ipc_max_async_credits
- smb2.credits.1conn_notify_max_async_credits
- smb2.credits.2conn_ipc_max_async_credits
- smb2.credits.2conn_notify_max_async_credits
- smb2.credits.ipc_max_data_zero
- smb2.credits.multichannel_ipc_max_async_credits
- smb2.credits.multichannel_max_async_credits
- smb2.credits.session_setup_credits_granted
- smb2.credits.single_req_credits_granted
- smb2.credits.skipped_mid

### smb2.delete-on-close-perms (9)
- smb2.delete-on-close-perms.BUG14427
- smb2.delete-on-close-perms.CREATE
- smb2.delete-on-close-perms.CREATE
- smb2.delete-on-close-perms.CREATE_IF
- smb2.delete-on-close-perms.CREATE_IF
- smb2.delete-on-close-perms.FIND_and_set_DOC
- smb2.delete-on-close-perms.OVERWRITE_IF
- smb2.delete-on-close-perms.OVERWRITE_IF
- smb2.delete-on-close-perms.READONLY

### smb2.deny (2)
- smb2.deny.deny1
- smb2.deny.deny2

### smb2.dir (9)
- smb2.dir.1kfiles_rename
- smb2.dir.file-index
- smb2.dir.find
- smb2.dir.fixed
- smb2.dir.large-files
- smb2.dir.many
- smb2.dir.modify
- smb2.dir.one
- smb2.dir.sorted

### smb2.dirlease (17)
- smb2.dirlease.hardlink
- smb2.dirlease.leases
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

### smb2.durable-open (23)
- smb2.durable-open.alloc-size
- smb2.durable-open.delete_on_close1
- smb2.durable-open.file-position
- smb2.durable-open.lease
- smb2.durable-open.lock-lease
- smb2.durable-open.lock-noW-lease
- smb2.durable-open.lock-oplock
- smb2.durable-open.open-lease
- smb2.durable-open.open-oplock
- smb2.durable-open.open2-lease
- smb2.durable-open.open2-oplock
- smb2.durable-open.oplock
- smb2.durable-open.read-only
- smb2.durable-open.reopen1
- smb2.durable-open.reopen1a
- smb2.durable-open.reopen1a-lease
- smb2.durable-open.reopen2
- smb2.durable-open.reopen2-lease
- smb2.durable-open.reopen2-lease-v2
- smb2.durable-open.reopen2a
- smb2.durable-open.reopen3
- smb2.durable-open.reopen4
- smb2.durable-open.stat-open

### smb2.durable-open-disconnect (1)
- smb2.durable-open-disconnect.open-oplock-disconnect

### smb2.durable-v2-delay (2)
- smb2.durable-v2-delay.durable_v2_reconnect_delay
- smb2.durable-v2-delay.durable_v2_reconnect_delay_msec

### smb2.durable-v2-open (32)
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

### smb2.ea (1)
- smb2.ea.acl_xattr

### smb2.fileid (4)
- smb2.fileid.fileid
- smb2.fileid.fileid-dir
- smb2.fileid.unique
- smb2.fileid.unique-dir

### smb2.getinfo (8)
- smb2.getinfo.complex
- smb2.getinfo.fsinfo
- smb2.getinfo.getinfo_access
- smb2.getinfo.granted
- smb2.getinfo.normalized
- smb2.getinfo.qfile_buffercheck
- smb2.getinfo.qfs_buffercheck
- smb2.getinfo.qsec_buffercheck

### smb2.ioctl (55)
- smb2.ioctl.bug14769
- smb2.ioctl.compress_create_with_attr
- smb2.ioctl.compress_dir_inherit
- smb2.ioctl.compress_file_flag
- smb2.ioctl.compress_inherit_disable
- smb2.ioctl.compress_invalid_buf
- smb2.ioctl.compress_invalid_format
- smb2.ioctl.compress_perms
- smb2.ioctl.compress_query_file_attr
- smb2.ioctl.compress_set_file_attr
- smb2.ioctl.copy-chunk
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
- smb2.ioctl.network_interface_info
- smb2.ioctl.req_resume_key
- smb2.ioctl.req_two_resume_keys
- smb2.ioctl.shadow_copy
- smb2.ioctl.sparse_compressed
- smb2.ioctl.sparse_copy_chunk
- smb2.ioctl.sparse_dir_flag
- smb2.ioctl.sparse_file_attr
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

### smb2.ioctl-on-stream (1)
- smb2.ioctl-on-stream

### smb2.kernel-oplocks (5)
- smb2.kernel-oplocks.kernel_oplocks1
- smb2.kernel-oplocks.kernel_oplocks3
- smb2.kernel-oplocks.kernel_oplocks4
- smb2.kernel-oplocks.kernel_oplocks6
- smb2.kernel-oplocks.kernel_oplocks7

### smb2.lease (44)
- smb2.lease.break
- smb2.lease.break_twice
- smb2.lease.breaking1
- smb2.lease.breaking2
- smb2.lease.breaking3
- smb2.lease.breaking4
- smb2.lease.breaking5
- smb2.lease.breaking6
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
- smb2.lease.v2_breaking3
- smb2.lease.v2_bug15148
- smb2.lease.v2_complex1
- smb2.lease.v2_complex2
- smb2.lease.v2_epoch1
- smb2.lease.v2_epoch2
- smb2.lease.v2_epoch3
- smb2.lease.v2_flags_breaking
- smb2.lease.v2_flags_parentkey
- smb2.lease.v2_rename
- smb2.lease.v2_rename_target_overwrite

### smb2.lock (22)
- smb2.lock.async
- smb2.lock.auto-unlock
- smb2.lock.cancel
- smb2.lock.cancel-logoff
- smb2.lock.cancel-tdis
- smb2.lock.contend
- smb2.lock.context
- smb2.lock.errorcode
- smb2.lock.lock
- smb2.lock.multiple-unlock
- smb2.lock.overlap
- smb2.lock.range
- smb2.lock.replay_smb3_specification_durable
- smb2.lock.replay_smb3_specification_multi
- smb2.lock.rw-exclusive
- smb2.lock.rw-shared
- smb2.lock.stacking
- smb2.lock.truncate
- smb2.lock.unlock
- smb2.lock.valid-request
- smb2.lock.zerobytelength
- smb2.lock.zerobyteread

### smb2.maxfid (1)
- smb2.maxfid

### smb2.maximum_allowed (2)
- smb2.maximum_allowed.maximum_allowed
- smb2.maximum_allowed.read_only

### smb2.mkdir (1)
- smb2.mkdir

### smb2.multichannel (6)
- smb2.multichannel.bugs.bug_15346
- smb2.multichannel.generic.interface_info
- smb2.multichannel.generic.num_channels
- smb2.multichannel.leases.test1
- smb2.multichannel.leases.test3
- smb2.multichannel.oplocks.test1

### smb2.notify (20)
- smb2.notify.basedir
- smb2.notify.close
- smb2.notify.dir
- smb2.notify.double
- smb2.notify.file
- smb2.notify.invalid-reauth
- smb2.notify.logoff
- smb2.notify.mask
- smb2.notify.overflow
- smb2.notify.rmdir1
- smb2.notify.rmdir2
- smb2.notify.rmdir3
- smb2.notify.rmdir4
- smb2.notify.session-reconnect
- smb2.notify.tcon
- smb2.notify.tcp
- smb2.notify.tdis
- smb2.notify.tdis1
- smb2.notify.tree
- smb2.notify.valid-req

### smb2.notify-inotify (1)
- smb2.notify-inotify.inotify-rename

### smb2.openattr (1)
- smb2.openattr

### smb2.oplock (41)
- smb2.oplock.batch1
- smb2.oplock.batch10
- smb2.oplock.batch11
- smb2.oplock.batch12
- smb2.oplock.batch13
- smb2.oplock.batch14
- smb2.oplock.batch15
- smb2.oplock.batch16
- smb2.oplock.batch19
- smb2.oplock.batch2
- smb2.oplock.batch20
- smb2.oplock.batch21
- smb2.oplock.batch22a
- smb2.oplock.batch23
- smb2.oplock.batch24
- smb2.oplock.batch25
- smb2.oplock.batch26
- smb2.oplock.batch3
- smb2.oplock.batch4
- smb2.oplock.batch5
- smb2.oplock.batch6
- smb2.oplock.batch7
- smb2.oplock.batch8
- smb2.oplock.batch9
- smb2.oplock.batch9a
- smb2.oplock.brl1
- smb2.oplock.brl2
- smb2.oplock.brl3
- smb2.oplock.doc
- smb2.oplock.exclusive1
- smb2.oplock.exclusive2
- smb2.oplock.exclusive3
- smb2.oplock.exclusive4
- smb2.oplock.exclusive5
- smb2.oplock.exclusive6
- smb2.oplock.exclusive9
- smb2.oplock.levelii500
- smb2.oplock.levelii501
- smb2.oplock.levelii502
- smb2.oplock.statopen1
- smb2.oplock.stream1

### smb2.read (4)
- smb2.read.access
- smb2.read.dir
- smb2.read.eof
- smb2.read.position

### smb2.rename (13)
- smb2.rename.close-full-information
- smb2.rename.msword
- smb2.rename.no_share_delete_but_delete_access
- smb2.rename.no_share_delete_no_delete_access
- smb2.rename.no_sharing
- smb2.rename.rename-open
- smb2.rename.rename_dir_bench
- smb2.rename.rename_dir_openfile
- smb2.rename.share_delete_and_delete_access
- smb2.rename.share_delete_no_delete_access
- smb2.rename.simple
- smb2.rename.simple_modtime
- smb2.rename.simple_nodelete

### smb2.replay (34)
- smb2.replay.channel-sequence
- smb2.replay.dhv2-pending1l-vs-lease-sane
- smb2.replay.dhv2-pending1l-vs-oplock-sane
- smb2.replay.dhv2-pending1n-vs-lease-sane
- smb2.replay.dhv2-pending1n-vs-oplock-sane
- smb2.replay.dhv2-pending1n-vs-violation-lease-ack-sane
- smb2.replay.dhv2-pending1n-vs-violation-lease-close-sane
- smb2.replay.dhv2-pending1o-vs-lease-sane
- smb2.replay.dhv2-pending1o-vs-oplock-sane
- smb2.replay.dhv2-pending2l-vs-lease-sane
- smb2.replay.dhv2-pending2l-vs-oplock-sane
- smb2.replay.dhv2-pending2n-vs-lease-sane
- smb2.replay.dhv2-pending2n-vs-oplock-sane
- smb2.replay.dhv2-pending2o-vs-lease-sane
- smb2.replay.dhv2-pending2o-vs-oplock-sane
- smb2.replay.dhv2-pending3l-vs-lease-sane
- smb2.replay.dhv2-pending3l-vs-oplock-sane
- smb2.replay.dhv2-pending3n-vs-lease-sane
- smb2.replay.dhv2-pending3n-vs-oplock-sane
- smb2.replay.dhv2-pending3o-vs-lease-sane
- smb2.replay.dhv2-pending3o-vs-oplock-sane
- smb2.replay.replay-commands
- smb2.replay.replay-dhv2-lease-oplock
- smb2.replay.replay-dhv2-lease1
- smb2.replay.replay-dhv2-lease2
- smb2.replay.replay-dhv2-lease3
- smb2.replay.replay-dhv2-oplock-lease
- smb2.replay.replay-dhv2-oplock1
- smb2.replay.replay-dhv2-oplock2
- smb2.replay.replay-dhv2-oplock3
- smb2.replay.replay-regular
- smb2.replay.replay3
- smb2.replay.replay4
- smb2.replay.replay7

### smb2.rw (4)
- smb2.rw.append
- smb2.rw.invalid
- smb2.rw.rw1
- smb2.rw.rw2

### smb2.scan (3)
- smb2.scan.find
- smb2.scan.getinfo
- smb2.scan.setinfo

### smb2.sdread (1)
- smb2.sdread

### smb2.secleak (1)
- smb2.secleak

### smb2.session (63)
- smb2.session.anon-encryption1
- smb2.session.anon-encryption2
- smb2.session.anon-encryption3
- smb2.session.anon-signing1
- smb2.session.anon-signing2
- smb2.session.bind1
- smb2.session.bind2
- smb2.session.bind_invalid_auth
- smb2.session.bind_negative_smb202
- smb2.session.bind_negative_smb210d
- smb2.session.bind_negative_smb210s
- smb2.session.bind_negative_smb2to3d
- smb2.session.bind_negative_smb2to3s
- smb2.session.bind_negative_smb3encGtoCd
- smb2.session.bind_negative_smb3encGtoCs
- smb2.session.bind_negative_smb3signC30toGd
- smb2.session.bind_negative_smb3signC30toGs
- smb2.session.bind_negative_smb3signCtoGd
- smb2.session.bind_negative_smb3signCtoGs
- smb2.session.bind_negative_smb3signCtoHd
- smb2.session.bind_negative_smb3signCtoHs
- smb2.session.bind_negative_smb3signGtoC30d
- smb2.session.bind_negative_smb3signGtoC30s
- smb2.session.bind_negative_smb3signGtoCd
- smb2.session.bind_negative_smb3signGtoCs
- smb2.session.bind_negative_smb3signGtoH2Xd
- smb2.session.bind_negative_smb3signGtoH2Xs
- smb2.session.bind_negative_smb3signGtoHd
- smb2.session.bind_negative_smb3signGtoHs
- smb2.session.bind_negative_smb3signH2XtoGd
- smb2.session.bind_negative_smb3signH2XtoGs
- smb2.session.bind_negative_smb3signHtoCd
- smb2.session.bind_negative_smb3signHtoCs
- smb2.session.bind_negative_smb3signHtoGd
- smb2.session.bind_negative_smb3signHtoGs
- smb2.session.bind_negative_smb3sneCtoGd
- smb2.session.bind_negative_smb3sneCtoGs
- smb2.session.bind_negative_smb3sneGtoCd
- smb2.session.bind_negative_smb3sneGtoCs
- smb2.session.bind_negative_smb3sneGtoHd
- smb2.session.bind_negative_smb3sneGtoHs
- smb2.session.bind_negative_smb3sneHtoGd
- smb2.session.bind_negative_smb3sneHtoGs
- smb2.session.bind_negative_smb3to2d
- smb2.session.bind_negative_smb3to2s
- smb2.session.bind_negative_smb3to3d
- smb2.session.bind_negative_smb3to3s
- smb2.session.encryption-aes-128-ccm
- smb2.session.encryption-aes-128-gcm
- smb2.session.encryption-aes-256-ccm
- smb2.session.encryption-aes-256-gcm
- smb2.session.ntlmssp_bug14932
- smb2.session.reauth1
- smb2.session.reauth2
- smb2.session.reauth3
- smb2.session.reauth4
- smb2.session.reauth6
- smb2.session.reconnect1
- smb2.session.reconnect2
- smb2.session.signing-aes-128-cmac
- smb2.session.signing-aes-128-gmac
- smb2.session.signing-hmac-sha-256
- smb2.session.two_logoff

### smb2.session-id (1)
- smb2.session-id

### smb2.session-require-signing (1)
- smb2.session-require-signing.bug15397

### smb2.setinfo (1)
- smb2.setinfo

### smb2.sharemode (3)
- smb2.sharemode.access-sharemode
- smb2.sharemode.bug14375
- smb2.sharemode.sharemode-access

### smb2.stream-inherit-perms (1)
- smb2.stream-inherit-perms

### smb2.streams (14)
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
- smb2.streams.rename
- smb2.streams.rename2
- smb2.streams.sharemodes
- smb2.streams.zero-byte

### smb2.tcon (1)
- smb2.tcon

### smb2.timestamps (15)
- smb2.timestamps.delayed-1write
- smb2.timestamps.delayed-2write
- smb2.timestamps.delayed-write-vs-flush
- smb2.timestamps.delayed-write-vs-setbasic
- smb2.timestamps.delayed-write-vs-seteof
- smb2.timestamps.freeze-thaw
- smb2.timestamps.test_close_not_attrib
- smb2.timestamps.time_t_-1
- smb2.timestamps.time_t_-2
- smb2.timestamps.time_t_0
- smb2.timestamps.time_t_1
- smb2.timestamps.time_t_10000000000
- smb2.timestamps.time_t_15032385535
- smb2.timestamps.time_t_1968
- smb2.timestamps.time_t_4294967295

### smb2.winattr (1)
- smb2.winattr

### smb2.winattr2 (1)
- smb2.winattr2

</details>

## Failing Tests (43)

<details><summary>Show 43 test(s)</summary>

### smb2.charset (1)
- smb2.charset.Testing _(known)_

### smb2.create (2)
- smb2.create.gentest _(known)_
- smb2.create.quota-fake-file _(known)_

### smb2.dosmode (1)
- smb2.dosmode _(known)_

### smb2.durable-open (1)
- smb2.durable-open.delete_on_close2 _(known)_

### smb2.kernel-oplocks (2)
- smb2.kernel-oplocks.kernel_oplocks2 _(known)_
- smb2.kernel-oplocks.kernel_oplocks5 _(known)_

### smb2.multichannel (5)
- smb2.multichannel.leases.test2 _(known)_
- smb2.multichannel.leases.test4 _(known)_
- smb2.multichannel.oplocks.test2 _(known)_
- smb2.multichannel.oplocks.test3_specification _(known)_
- smb2.multichannel.oplocks.test3_windows _(known)_

### smb2.name-mangling (2)
- smb2.name-mangling.mangle _(known)_
- smb2.name-mangling.mangled-mask _(known)_

### smb2.notify (1)
- smb2.notify.rec _(known)_

### smb2.oplock (1)
- smb2.oplock.batch22b _(known)_

### smb2.replay (21)
- smb2.replay.dhv2-pending1l-vs-lease-windows _(known)_
- smb2.replay.dhv2-pending1l-vs-oplock-windows _(known)_
- smb2.replay.dhv2-pending1n-vs-lease-windows _(known)_
- smb2.replay.dhv2-pending1n-vs-oplock-windows _(known)_
- smb2.replay.dhv2-pending1n-vs-violation-lease-ack-windows _(known)_
- smb2.replay.dhv2-pending1n-vs-violation-lease-close-windows _(known)_
- smb2.replay.dhv2-pending1o-vs-lease-windows _(known)_
- smb2.replay.dhv2-pending1o-vs-oplock-windows _(known)_
- smb2.replay.dhv2-pending2l-vs-lease-windows _(known)_
- smb2.replay.dhv2-pending2l-vs-oplock-windows _(known)_
- smb2.replay.dhv2-pending2n-vs-lease-windows _(known)_
- smb2.replay.dhv2-pending2n-vs-oplock-windows _(known)_
- smb2.replay.dhv2-pending2o-vs-lease-windows _(known)_
- smb2.replay.dhv2-pending2o-vs-oplock-windows _(known)_
- smb2.replay.dhv2-pending3l-vs-lease-windows _(known)_
- smb2.replay.dhv2-pending3l-vs-oplock-windows _(known)_
- smb2.replay.dhv2-pending3n-vs-lease-windows _(known)_
- smb2.replay.dhv2-pending3n-vs-oplock-windows _(known)_
- smb2.replay.dhv2-pending3o-vs-lease-windows _(known)_
- smb2.replay.dhv2-pending3o-vs-oplock-windows _(known)_
- smb2.replay.replay6 _(known)_

### smb2.samba3misc (1)
- smb2.samba3misc.localposixlock1 _(known)_

### smb2.session (1)
- smb2.session.reauth5 _(known)_

### smb2.set-sparse-ioctl (1)
- smb2.set-sparse-ioctl _(known)_

### smb2.timestamp_resolution (1)
- smb2.timestamp_resolution.resolution1 _(known)_

### smb2.twrp (1)
- smb2.twrp.listdir _(known)_

### smb2.zero-data-ioctl (1)
- smb2.zero-data-ioctl _(known)_

</details>

## Skipped Tests (40)

<details><summary>Show 40 test(s)</summary>

### smb2.create (1)
- smb2.create.path-length

### smb2.durable-v2-regressions (1)
- smb2.durable-v2-regressions.durable_v2_reconnect_bug15624

### smb2.ioctl (20)
- smb2.ioctl.bug14607
- smb2.ioctl.bug14788.NETWORK_INTERFACE
- smb2.ioctl.bug14788.VALIDATE_NEGOTIATE
- smb2.ioctl.compress_notsup_get
- smb2.ioctl.compress_notsup_set
- smb2.ioctl.dup_extents_bad_handle
- smb2.ioctl.dup_extents_compressed_dest
- smb2.ioctl.dup_extents_compressed_src
- smb2.ioctl.dup_extents_dest_lock
- smb2.ioctl.dup_extents_len_beyond_dest
- smb2.ioctl.dup_extents_len_beyond_src
- smb2.ioctl.dup_extents_len_zero
- smb2.ioctl.dup_extents_simple
- smb2.ioctl.dup_extents_sparse_both
- smb2.ioctl.dup_extents_sparse_dest
- smb2.ioctl.dup_extents_sparse_src
- smb2.ioctl.dup_extents_src_is_dest
- smb2.ioctl.dup_extents_src_is_dest_overlap
- smb2.ioctl.dup_extents_src_lock
- smb2.ioctl.trim_simple

### smb2.kernel-oplocks (1)
- smb2.kernel-oplocks.kernel_oplocks8

### smb2.lease (1)
- smb2.lease.dynamic_share

### smb2.lock (4)
- smb2.lock.ctdb-delrec-deadlock
- smb2.lock.open-brlock-deadlock
- smb2.lock.replay_broken_windows
- smb2.lock.rw-none

### smb2.read (1)
- smb2.read.bug14607

### smb2.replay (1)
- smb2.replay.replay5

### smb2.session (7)
- smb2.session.bind_different_user
- smb2.session.expire1e
- smb2.session.expire1n
- smb2.session.expire1s
- smb2.session.expire2e
- smb2.session.expire2s
- smb2.session.expire_disconnect

### smb2.twrp (3)
- smb2.twrp.openroot
- smb2.twrp.stream
- smb2.twrp.write

</details>
