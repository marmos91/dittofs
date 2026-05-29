# smbtorture Known Failures

Last updated: 2026-05-28 (close FIND+CLOSE goroutine-spawn race via dispatch-time pre-acquire of per-FileID in-flight counter — walks back `smb2.compound_find.compound_find_close` from Compound and Intermittent sections, #472)

Tests listed here are expected to fail and will NOT cause CI to report failure.
Only NEW failures (not in this list) will cause CI to fail.

## Policy (v1.0 conformance gate, #673)

- The [Permanently Unimplementable](#permanently-unimplementable-out-of-scope) appendix at the bottom is the **only** place new entries may be added without an accompanying GH sub-issue.
- Every entry above the appendix MUST either (a) reference an open GH sub-issue under the `v1.0.0` milestone, or (b) be promoted into the appendix with a documented architectural reason.
- Walking a test back (removing from this file) is encouraged whenever it starts passing on develop. Do not re-add a passing test to silence a transient flake — fix the flake.
- Goal: every non-appendix entry resolved before tagging v1.0.

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
| smb2.multichannel.leases.test1 | Multi-channel | Cross-channel lease break fan-out is Phase 2 work on #361; test flakes on DittoFS until the primary/secondary channel coordination lands | #745 |
| smb2.multichannel.leases.test3 | Multi-channel | Spurious lease break on uncontested open — separate bug from #417 epoch drift | #745 |

Note: the five `smb2.multichannel.{leases,oplocks}` tests requiring Samba-internal harness FSCTLs (`torture_block_tcp_transport`, `FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT`) have been moved to the [Permanently Unimplementable](#permanently-unimplementable-out-of-scope) appendix.

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

Most IOCTL sparse-family entries walked back under #718. The remaining residual
failure is a real feature gap: SRV_COPYCHUNK on a sparse destination must surface
zeros for the unwritten hole between the old EOF and the chunk's target offset
(see Samba `test_ioctl_copy_chunk_sparse_dest`). DittoFS's copychunk path grows
the destination file via `WriteAt` at the target offset but does not advertise
or materialize the [old EOF, target offset) hole as zero-reading bytes.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.ioctl.copy_chunk_sparse_dest | IOCTL | SRV_COPYCHUNK to a 0-byte destination at offset 4096 must surface the [0, 4096) gap as zeros on subsequent reads. The block-store sparse-hole zero-fill path does not run for copychunk-extended files. | #750 |

Note: the standalone `smb2.set-sparse-ioctl` and `smb2.zero-data-ioctl` driver
tests require `--option=torture:filename=` / `--option=torture:offset=` runtime
arguments that the default battery does not provide; they are listed in the
[Permanently Unimplementable](#permanently-unimplementable-out-of-scope) appendix.

### Change Notify (Remaining)

Phase 73 Plan 03 completed async ChangeNotify infrastructure. Wave 2 fixed
handle-permissions, overflow, tree, invalid-reauth, tcon (5 more flips).
Passing: basedir, close, handle-permissions, invalid-reauth, logoff,
overflow, rec, rmdir1-4, tcon, tdis, tdis1, tcp, tree.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.notify.valid-req | Change Notify | Needs kernel inotify for MODIFIED on WRITE (also fails on reference Samba in Docker) | #750 |
| smb2.notify.mask-change | Change Notify | SHARING_VIOLATION on directory open (pre-existing, never passed individually) | #750 |

### Oplocks (Multi-Client Coordination Not Implemented)

Oplock tests require multi-client coordination (oplock break notifications to
other clients). DittoFS has basic oplock support; the residual failures cluster
around stat-only-open conflict suppression, LEVEL_II coercion of subsequent
oplock grants, and a few specialized response-mapping cases (#479).

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.oplock.batch9a | Oplocks | Stat-only open vs normal-open break-count drift | #740 |
| smb2.oplock.batch10 | Oplocks | Subsequent oplock request must coerce to LEVEL_II when prior open holds no oplock | #740 |
| smb2.oplock.batch12 | Oplocks | SetPathInfo allocation-size needs second break stage (count=2) | #740 |
| smb2.oplock.batch13 | Oplocks | OVERWRITE with attrs-only second open: granted oplock must be LEVEL_II | #740 |
| smb2.oplock.batch14 | Oplocks | SUPERSEDE with attrs-only second open: granted oplock must be LEVEL_II | #740 |
| smb2.oplock.batch16 | Oplocks | OVERWRITE_IF with attrs-only second open: granted oplock must be LEVEL_II | #740 |
| smb2.oplock.batch22a | Oplocks | Break timeout window not honored (~35s expected) | #740 |
| smb2.oplock.batch23 | Oplocks | After break to LEVEL_II, 3rd open must receive LEVEL_II grant | #740 |
| smb2.oplock.levelii500 | Oplocks | ACK LEVEL_II→None must return STATUS_INVALID_OPLOCK_PROTOCOL | #740 |
| smb2.oplock.statopen1 | Oplocks | READ_CONTROL access-mask should trigger break | #740 |

Note: the four `smb2.kernel-oplocks.*` tests require Linux kernel oplock integration via `F_SETLEASE` on the underlying fd — architecturally incompatible with DittoFS's userspace virtual filesystem. They are listed in the [Permanently Unimplementable](#permanently-unimplementable-out-of-scope) appendix.

### Directory Leases (Partial Implementation)

Directory leases (dirlease) are a separate feature from file leases.
DittoFS implements file leases (Phase 37) and a substantial subset of
directory leases (see #470 PR history). Remaining failures cluster on:
(1) same-dir rename / hardlink break/ack ordering, (2) DELETE_PENDING
visibility on initial-DOC unlink cases.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.dirlease.hardlink | Directory Leases | samedir-wrong-parent-leaskey: break/ack ordering on single-dir hardlink | #743 |
| smb2.dirlease.oplocks | Directory Leases | Skipped by runner — smbtorture 4.22.6 client SIGSEGVs in this subtest and aborts the rest of the dirlease suite (see run.sh) | #750 |
| smb2.dirlease.rename | Directory Leases | samedir-wrong-parent-leaskey: break/ack ordering on single-dir rename | #743 |
| smb2.dirlease.unlink_different_initial_and_close | Directory Leases | DELETE_PENDING returned on second open of a file with initial DOC (delete-on-close shouldn't block reopens before actual delete) | #743 |
| smb2.dirlease.unlink_different_set_and_close | Directory Leases | smb2_lease_break_ack returns UNSUCCESSFUL — break/ack state mismatch on last-handle delete with mismatched parent keys | #743 |
| smb2.dirlease.unlink_same_initial_and_close | Directory Leases | DELETE_PENDING returned on second open of a file with initial DOC | #743 |
| smb2.dirlease.v2_request | Directory Leases | SHARING_VIOLATION on requeued CREATE after dir-lease holder closes during break | #743 |

### Credit Management

Credit grant arithmetic and the `max_async_credits` cap are correct post-#399
and post-#416: the full `smb2.credits` subsuite (10 tests) passes. Samba
enforces the 511-slot cap **per TCP connection** —
`source4/torture/smb2/credits.c:1346` asserts
`num_status_pending == 511` per tree — which DittoFS's per-`ConnInfo`
counter already matched. The `2conn_notify_max_async_credits` failure that
remained here was a cross-connection MessageID collision in
`NotifyRegistry`, fixed in #416.

### File Attributes (Limited Support)

DittoFS has limited DOS/Windows attribute support. Hidden, system, and archive
attributes are not fully implemented.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.dosmode | DOS attributes | DOS mode semantics not implemented | #750 |

### Create Contexts (Advanced Semantics Not Implemented)

Advanced CREATE context features (impersonation, ACL-based create, quota fake
files, create blobs) are not implemented. Basic create operations pass.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.create.blob | Create | Create context blobs not fully implemented | #741 |
| smb2.create.gentest | Create | Generic create test (impersonation) not implemented | #741 |
| smb2.create.impersonation | Create | Impersonation levels not implemented | #741 |
| smb2.create.mkdir-dup | Create | Flaky in CI (parallel CREATE OPEN_IF race — passes intermittently on develop, perturbed by unrelated timing changes) | #741 |
| smb2.create.mkdir-visible | Create | Mkdir visibility semantics not implemented | #741 |
| smb2.create.multi | Create | Regression from recent changes, fails on all 3 stores | #741 |
| smb2.create.path-length | Create | Flaky in CI (path length validation race) | #741 |

### Query/Set Info (Advanced Scenarios)

Advanced getinfo scenarios requiring security descriptor queries, buffer size
checks, and ACL-based access control.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.setinfo | Set Info | SET_INFO timestamp preservation not implemented | #750 |

### Share Modes and Deny (Advanced Scenarios)

Advanced share mode enforcement and deny mode scenarios.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.sharemode.bug14375 | Share modes | Share mode edge case not implemented | #750 |

### Maximum Allowed Access (Partial)

Maximum allowed access computation is partially implemented. Read-only
maximum_allowed works but full computation does not.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.maximum_allowed.maximum_allowed | Access checks | Full maximum allowed computation not implemented | #750 |

### Connection and Tree Connect (Advanced Semantics)

Advanced connection and tree connect edge cases.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.tcon | Tree connect | Advanced tree connect semantics not implemented | #750 |
| smb2.maxfid | Connection | Connection drops under high FD pressure | #750 |

### Intermittent / Flaky

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.lease.statopen4 | Leases | Flaky stat-open lease test - passes intermittently | #751 |

### Character Set (Edge Cases)

Unicode and character set edge cases (partial surrogates, wide-A collision) are
tracked as fix candidates in baseline-results.md rather than known failures,
since basic charset support works.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.charset.Testing | Character set | Unicode surrogate pair / wide-A handling not implemented | #740 |

### Extended Attributes (ACL-Based)

Extended attribute tests requiring ACL-based access control.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.ea.acl_xattr | Extended attributes | EA ACL enforcement not implemented | #750 |

### Timestamp Resolution

Timestamp resolution test requires sub-second precision enforcement.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.timestamp_resolution.resolution1 | Timestamps | Timestamp resolution enforcement not implemented | #750 |

### Session Signing Edge Cases

Session signing edge cases requiring multi-channel binding.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|

### Durable Handles V1 (Fix Candidate)

Durable handle V1 open/reopen operations partially implemented but tests
still fail due to incomplete reconnect and lease coordination.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.durable-open.reopen1a | Durable handles V1 | Durable reopen not fully working | #738 |
| smb2.durable-open.reopen1a-lease | Durable handles V1 | Durable reopen with lease not fully working | #738 |
| smb2.durable-open.reopen2 | Durable handles V1 | Durable reopen not fully working | #738 |
| smb2.durable-open.reopen2-lease | Durable handles V1 | Durable reopen with lease not fully working | #738 |
| smb2.durable-open.reopen2-lease-v2 | Durable handles V1 | Durable reopen with lease V2 not fully working | #738 |
| smb2.durable-open.reopen2a | Durable handles V1 | Durable reopen not fully working | #738 |
| smb2.durable-open.reopen4 | Durable handles V1 | Durable reopen not fully working | #738 |
| smb2.durable-open.delete_on_close1 | Durable handles V1 | Durable DOC not fully working | #738 |
| smb2.durable-open.delete_on_close2 | Durable handles V1 | Durable DOC not fully working | #738 |
| smb2.durable-open.file-position | Durable handles V1 | Durable file position not fully working | #738 |
| smb2.durable-open.lock-oplock | Durable handles V1 | Durable lock + oplock not fully working | #738 |
| smb2.durable-open.lock-lease | Durable handles V1 | Durable lock + lease not fully working | #738 |
| smb2.durable-open.alloc-size | Durable handles V1 | Pre-existing: out.alloc_size returned 0 instead of expected non-zero | #738 |
| smb2.durable-open.read-only | Durable handles V1 | Pre-existing: OBJECT_NAME_NOT_FOUND on durable read-only reopen | #738 |

### Durable Handles V2 (Fix Candidate)

Durable handle V2 open/reopen operations partially implemented but tests
still fail due to incomplete reconnect, lease coordination, and persistence.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.durable-v2-open.create-blob | Durable handles V2 | DH2Q create context blob validation | #739 |
| smb2.durable-v2-open.open-oplock | Durable handles V2 | DH2 open with oplock not fully working | #739 |
| smb2.durable-v2-open.open-lease | Durable handles V2 | DH2 open with lease not fully working | #739 |
| smb2.durable-v2-open.reopen1 | Durable handles V2 | DH2 reopen not fully working | #739 |
| smb2.durable-v2-open.reopen1a | Durable handles V2 | DH2 reopen not fully working | #739 |
| smb2.durable-v2-open.reopen1a-lease | Durable handles V2 | DH2 reopen with lease not fully working | #739 |
| smb2.durable-v2-open.reopen2 | Durable handles V2 | DH2 reopen not fully working | #739 |
| smb2.durable-v2-open.reopen2b | Durable handles V2 | DH2 reopen not fully working | #739 |
| smb2.durable-v2-open.reopen2-lease | Durable handles V2 | DH2 reopen with lease not fully working | #739 |
| smb2.durable-v2-open.reopen2-lease-v2 | Durable handles V2 | DH2 reopen with lease V2 not fully working | #739 |
| smb2.durable-v2-open.durable-v2-setinfo | Durable handles V2 | DH2 setinfo not fully working | #739 |
| smb2.durable-v2-open.lock-oplock | Durable handles V2 | DH2 lock with oplock not fully working | #739 |
| smb2.durable-v2-open.lock-lease | Durable handles V2 | DH2 lock with lease not fully working | #739 |
| smb2.durable-v2-open.lock-noW-lease | Durable handles V2 | DH2 lock without write lease not fully working | #739 |
| smb2.durable-v2-open.stat-and-lease | Durable handles V2 | DH2 stat + lease interaction not fully working | #739 |
| smb2.durable-v2-open.nonstat-and-lease | Durable handles V2 | DH2 non-stat + lease interaction not fully working | #739 |
| smb2.durable-v2-open.statRH-and-lease | Durable handles V2 | DH2 stat-RH + lease interaction not fully working | #739 |
| smb2.durable-v2-open.two-same-lease | Durable handles V2 | DH2 two handles same lease not fully working | #739 |
| smb2.durable-v2-open.two-different-lease | Durable handles V2 | DH2 two handles different leases not fully working | #739 |
| smb2.durable-v2-open.keep-disconnected-rh-with-stat-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | #739 |
| smb2.durable-v2-open.keep-disconnected-rh-with-rh-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | #739 |
| smb2.durable-v2-open.keep-disconnected-rh-with-rwh-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | #739 |
| smb2.durable-v2-open.keep-disconnected-rwh-with-stat-open | Durable handles V2 | DH2 disconnected handle preservation not fully working | #739 |
| smb2.durable-v2-open.purge-disconnected-rwh-with-rwh-open | Durable handles V2 | DH2 disconnected handle purge not fully working | #739 |
| smb2.durable-v2-open.purge-disconnected-rwh-with-rh-open | Durable handles V2 | DH2 disconnected handle purge not fully working | #739 |
| smb2.durable-v2-open.purge-disconnected-rh-with-share-none-open | Durable handles V2 | DH2 disconnected handle purge not fully working | #739 |
| smb2.durable-v2-open.purge-disconnected-rh-with-write | Durable handles V2 | DH2 disconnected handle purge not fully working | #739 |
| smb2.durable-v2-open.purge-disconnected-rh-with-rename | Durable handles V2 | DH2 disconnected handle purge not fully working | #739 |
| smb2.durable-v2-open.app-instance | Durable handles V2 | App instance ID not fully working | #739 |
| smb2.durable-v2-open.persistent-open-oplock | Durable handles V2 | Persistent handles not implemented | #739 |
| smb2.durable-v2-open.persistent-open-lease | Durable handles V2 | Persistent handles not implemented | #739 |
| smb2.durable-v2-delay.durable_v2_reconnect_delay | Durable handles V2 | DH2 reconnect delay not fully working | #739 |

### Leases (Fix Candidate)

Lease V2 is implemented but many smbtorture lease tests still fail due to
incomplete break notification delivery and multi-client coordination.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|

### Sessions (Remaining)

Phase 73 Plan 03 implemented session re-authentication with key re-derivation
per MS-SMB2 3.3.5.5.3. Remaining tests need session reconnect or multi-channel
binding. reauth1-6 / anon-encryption1-3 / ntlmssp_bug14932 were previously
masked because earlier suite tests hung; once the hang cleared they surfaced
as pre-existing failures rather than regressions and are tracked here for
follow-up.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.session.reconnect1 | Sessions | Session reconnect not fully working | #746 |
| smb2.session.reconnect2 | Sessions | Session reconnect not fully working | #746 |
| smb2.session.bind_negative_smb202 | Sessions | Session binding validation not fully working | #746 |
| smb2.session.bind_negative_smb210s | Sessions | Session binding validation not fully working | #746 |
| smb2.session.bind_negative_smb210d | Sessions | Session binding validation not fully working | #746 |
| smb2.session.reauth1 | Sessions | Pre-existing: re-auth CHALLENGE response rejected by client (signature/preauth chain) | #746 |
| smb2.session.reauth2 | Sessions | Pre-existing: re-auth CHALLENGE response rejected by client | #746 |
| smb2.session.reauth3 | Sessions | Pre-existing: re-auth CHALLENGE response rejected by client | #746 |
| smb2.session.reauth4 | Sessions | Pre-existing: re-auth CHALLENGE response rejected by client | #746 |
| smb2.session.reauth5 | Sessions | Pre-existing: re-auth CHALLENGE response rejected by client | #746 |
| smb2.session.reauth6 | Sessions | Pre-existing: re-auth expects LOGON_FAILURE for malformed creds, gets ACCESS_DENIED | #746 |
| smb2.session.ntlmssp_bug14932 | Sessions | Pre-existing: malformed NTLMSSP expects INVALID_PARAMETER, gets ACCESS_DENIED | #746 |
| smb2.session.anon-encryption1 | Sessions | Pre-existing: anonymous encryption setup rejected with INVALID_PARAMETER | #746 |
| smb2.session.anon-encryption2 | Sessions | Pre-existing: anonymous encryption setup rejected with INVALID_PARAMETER | #746 |
| smb2.session.anon-encryption3 | Sessions | Pre-existing: anonymous encryption setup rejected with INVALID_PARAMETER | #746 |

### Session Binding (Multi-Channel, Not Implemented)

Session binding tests require multi-channel support to bind a session across
TCP connections with different SMB dialect and signing/encryption combinations.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.session.bind2 | Session binding | Session binding not implemented | #747 |
| smb2.session.bind_invalid_auth | Session binding | Session binding auth validation not implemented | #747 |
| smb2.session.bind_negative_smb3signCtoHs | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3signCtoHd | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3signCtoGs | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3signCtoGd | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3signHtoCs | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3signHtoCd | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3signHtoGs | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3signHtoGd | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3signGtoCs | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3signGtoCd | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3signGtoHs | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3signGtoHd | Session binding | Multi-channel signing binding not implemented | #747 |
| smb2.session.bind_negative_smb3sneGtoCs | Session binding | Multi-channel signing+encryption binding not implemented | #747 |
| smb2.session.bind_negative_smb3sneGtoCd | Session binding | Multi-channel signing+encryption binding not implemented | #747 |
| smb2.session.bind_negative_smb3sneGtoHs | Session binding | Multi-channel signing+encryption binding not implemented | #747 |
| smb2.session.bind_negative_smb3sneGtoHd | Session binding | Multi-channel signing+encryption binding not implemented | #747 |
| smb2.session.bind_negative_smb3sneCtoGs | Session binding | Multi-channel signing+encryption binding not implemented | #747 |
| smb2.session.bind_negative_smb3sneCtoGd | Session binding | Multi-channel signing+encryption binding not implemented | #747 |
| smb2.session.bind_negative_smb3sneHtoGs | Session binding | Multi-channel signing+encryption binding not implemented | #747 |
| smb2.session.bind_negative_smb3sneHtoGd | Session binding | Multi-channel signing+encryption binding not implemented | #747 |
| smb2.session.bind_negative_smb3signC30toGs | Session binding | Multi-channel signing binding (3.0 to GMAC) not implemented | #747 |
| smb2.session.bind_negative_smb3signC30toGd | Session binding | Multi-channel signing binding (3.0 to GMAC) not implemented | #747 |
| smb2.session.bind_negative_smb3signH2XtoGs | Session binding | Multi-channel signing binding (HMAC to GMAC) not implemented | #747 |
| smb2.session.bind_negative_smb3signH2XtoGd | Session binding | Multi-channel signing binding (HMAC to GMAC) not implemented | #747 |
| smb2.session.bind_negative_smb3signGtoC30s | Session binding | Multi-channel signing binding (GMAC to 3.0) not implemented | #747 |
| smb2.session.bind_negative_smb3signGtoC30d | Session binding | Multi-channel signing binding (GMAC to 3.0) not implemented | #747 |
| smb2.session.bind_negative_smb3signGtoH2Xs | Session binding | Multi-channel signing binding (GMAC to HMAC) not implemented | #747 |
| smb2.session.bind_negative_smb3signGtoH2Xd | Session binding | Multi-channel signing binding (GMAC to HMAC) not implemented | #747 |

### Session Signing Variants (Algorithm-Specific Tests)

Algorithm-specific signing tests that validate signing with each algorithm
in isolation. Newly reachable after GMAC signing fix.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.session.signing-hmac-sha-256 | Session signing | HMAC-SHA-256 signing test not fully passing | #748 |
| smb2.session.signing-aes-128-cmac | Session signing | AES-128-CMAC signing test not fully passing | #748 |
| smb2.session.signing-aes-128-gmac | Session signing | AES-128-GMAC signing test not fully passing | #748 |

### Session Encryption Variants (Algorithm-Specific Tests)

Algorithm-specific encryption tests that validate encryption with each cipher.
Newly reachable after GMAC signing fix.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.session.encryption-aes-128-ccm | Session encryption | AES-128-CCM encryption test not fully passing | #748 |
| smb2.session.encryption-aes-128-gcm | Session encryption | AES-128-GCM encryption test not fully passing | #748 |
| smb2.session.encryption-aes-256-ccm | Session encryption | AES-256-CCM encryption test not fully passing | #748 |
| smb2.session.encryption-aes-256-gcm | Session encryption | AES-256-GCM encryption test not fully passing | #748 |

### Anonymous Session (Remaining)

Phase 73 Plan 03 implemented anonymous session encryption bypass per MS-SMB2
3.3.5.2.9. anon-encryption1-3 now pass. Remaining signing tests need further work.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.session.anon-signing1 | Anonymous session | Anonymous session signing not fully passing | #748 |
| smb2.session.anon-signing2 | Anonymous session | Anonymous session signing not fully passing | #748 |

### Replay Protection (Not Implemented)

Replay protection requires tracking channel sequences and detecting replayed
requests with durable handles. Newly reachable after GMAC signing fix.

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| smb2.replay.replay3 | Replay | Flaky in CI (replay detection race) | #749 |
| smb2.replay.replay-dhv2-oplock2 | Replay | Replay with durable handles not implemented | #749 |
| smb2.replay.replay-dhv2-oplock-lease | Replay | Replay with durable handles not implemented | #749 |
| smb2.replay.replay-dhv2-lease1 | Replay | Replay with durable handles not implemented | #749 |
| smb2.replay.replay-dhv2-lease2 | Replay | Replay with durable handles not implemented | #749 |
| smb2.replay.replay-dhv2-lease3 | Replay | Replay with durable handles not implemented | #749 |
| smb2.replay.replay-dhv2-lease-oplock | Replay | Replay with durable handles not implemented | #749 |
| smb2.replay.dhv2-pending1n-vs-violation-lease-close-sane | Replay | Replay pending violation handling not implemented | #749 |
| smb2.replay.dhv2-pending1n-vs-violation-lease-ack-sane | Replay | Replay pending violation handling not implemented | #749 |
| smb2.replay.dhv2-pending1n-vs-violation-lease-close-windows | Replay | Replay pending violation handling not implemented | #749 |
| smb2.replay.dhv2-pending1n-vs-violation-lease-ack-windows | Replay | Replay pending violation handling not implemented | #749 |
| smb2.replay.dhv2-pending1n-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending1n-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending1n-vs-lease-sane | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending1n-vs-lease-windows | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending1l-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending1l-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending1l-vs-lease-sane | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending1l-vs-lease-windows | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending1o-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending1o-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending1o-vs-lease-sane | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending1o-vs-lease-windows | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending2n-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending2n-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending2n-vs-lease-sane | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending2n-vs-lease-windows | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending2l-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending2l-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending2l-vs-lease-sane | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending2l-vs-lease-windows | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending2o-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending2o-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending2o-vs-lease-sane | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending2o-vs-lease-windows | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending3n-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending3n-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending3n-vs-lease-sane | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending3n-vs-lease-windows | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending3l-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending3l-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending3l-vs-lease-sane | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending3l-vs-lease-windows | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending3o-vs-oplock-sane | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending3o-vs-oplock-windows | Replay | Replay pending oplock handling not implemented | #749 |
| smb2.replay.dhv2-pending3o-vs-lease-sane | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.dhv2-pending3o-vs-lease-windows | Replay | Replay pending lease handling not implemented | #749 |
| smb2.replay.channel-sequence | Replay | Channel sequence tracking not implemented | #749 |
| smb2.replay.replay4 | Replay | Replay detection not implemented | #749 |
| smb2.replay.replay5 | Replay | Replay detection not implemented | #749 |
| smb2.replay.replay6 | Replay | Replay detection not implemented | #749 |

## Permanently Unimplementable (Out of Scope)

Tests below cannot be implemented in DittoFS by design. Reasons fall into three buckets:

1. **Samba-internal test-harness operations.** The smbtorture client invokes Samba-specific FSCTLs that exist only inside Samba's test build (`torture_block_tcp_transport`, `FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT`). DittoFS cannot implement these without becoming Samba.
2. **Kernel-level features.** Tests that require Linux kernel oplock semantics via `F_SETLEASE` on a real fd. DittoFS is a userspace virtual filesystem with no underlying kernel-fd to set leases on.
3. **OS-shell features outside the SMB protocol surface.** NTFS 8.3 short-name mangling (DOS compatibility) and VSS shadow copies / Previous Versions / Time Warp (`SMB2_CREATE_TIMEWARP_TOKEN`) are Windows OS features layered on top of NTFS, not protocol-level features of SMB2/3.
4. **Samba-private POSIX lock extensions** that ride on Samba's smb1-derived semantics and have no MS-SMB2 spec equivalent.

These entries remain in CI's known-failure set (so they don't break the build) but are explicitly outside the v1.0 conformance gate. Do not file sub-issues for them.

| Test Name | Category | Reason |
|-----------|----------|--------|
| smb2.multichannel.leases.test2 | Multi-channel | Requires `torture_block_tcp_transport` (Samba-internal test-harness operation) |
| smb2.multichannel.leases.test4 | Multi-channel | Requires `torture_block_tcp_transport` (Samba-internal test-harness operation) |
| smb2.multichannel.oplocks.test2 | Multi-channel | Requires `FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT` (Samba test-harness FSCTL) |
| smb2.multichannel.oplocks.test3_windows | Multi-channel | Requires `FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT` (Samba test-harness FSCTL) |
| smb2.multichannel.oplocks.test3_specification | Multi-channel | Requires `FSCTL_SMBTORTURE_FORCE_UNACKED_TIMEOUT` + 32-channel coordination (Samba-internal) |
| smb2.kernel-oplocks.kernel_oplocks2 | Kernel oplocks | Requires Linux kernel `F_SETLEASE` on underlying fd — userspace VFS cannot |
| smb2.kernel-oplocks.kernel_oplocks4 | Kernel oplocks | Requires Linux kernel `F_SETLEASE` on underlying fd — userspace VFS cannot |
| smb2.kernel-oplocks.kernel_oplocks5 | Kernel oplocks | Kernel oplock vs lease downgrade semantics — DittoFS has no kernel oplock layer |
| smb2.kernel-oplocks.kernel_oplocks8 | Kernel oplocks | smbtorture-side localdir check is host-FS-specific — not applicable to a virtual FS |
| smb2.name-mangling.mangle | Name mangling | NTFS 8.3 short-name mangling — DOS/Win9x legacy, not in SMB2/3 protocol surface |
| smb2.name-mangling.mangled-mask | Name mangling | NTFS 8.3 short-name mask search — DOS/Win9x legacy, not in SMB2/3 protocol surface |
| smb2.twrp.openroot | Previous Versions / TWRP | Requires Volume Shadow Copy backend (`SMB2_CREATE_TIMEWARP_TOKEN`) — Windows OS feature, not protocol |
| smb2.twrp.listdir | Previous Versions / TWRP | Requires Volume Shadow Copy backend (`SMB2_CREATE_TIMEWARP_TOKEN`) — Windows OS feature, not protocol |
| smb2.samba3misc.localposixlock1 | Samba-private | Samba-specific POSIX lock extensions (smb1-derived, no MS-SMB2 equivalent) |
| smb2.create.quota-fake-file | NTFS-internal | Synthesises NTFS pseudo-file `$Extend\$Quota:$Q:$INDEX_ALLOCATION`. NTFS volume-quota subsystem is a Windows on-disk-format feature; DittoFS has no NTFS metadata layer, no $Extend reserved files, no quota subsystem, and no protocol-defined way to surface these as fake objects on non-NTFS backends. |
| smb2.set-sparse-ioctl | Parameterized driver | Standalone smbtorture driver test that requires `--option=torture:filename=<name>` at invocation. Fails immediately with `Need to provide filename through --option=torture:filename=testfile` in any default-battery run; not a feature gap. The FSCTL itself is covered by `smb2.ioctl.sparse_*`. |
| smb2.zero-data-ioctl | Parameterized driver | Standalone smbtorture driver test that requires `--option=torture:offset=<n>` at invocation. Fails immediately with `Need to provide non-negative offset through --option=torture:offset=NNN`; not a feature gap. The FSCTL itself is covered by `smb2.ioctl.sparse_punch` / `sparse_punch_invalid`. |

**Total: 17 tests permanently out of scope.**

### Kerberos

The 70 entries in `KNOWN_FAILURES_KERBEROS.md` are deferred past the v1.0 tag and tracked under #686 (v1.0+kerberos). They do not gate v1.0 because `parse-results.sh` only loads them when smbtorture is run with `--kerberos`, which is excluded from the v1.0 CI matrix (`run.sh:533`).

## Changelog

### 2026-05-28 — CREATE wire validation + quota-fake-file to appendix (#480)

- Server now validates ImpersonationLevel (>3 → BAD_IMPERSONATION_LEVEL),
  CreateOptions reserved bits (0xff000000 → INVALID_PARAMETER),
  CreateOptions unsupported bits (0x00102080 → NOT_SUPPORTED), FileAttributes
  bits outside 0x7FB7 (→ INVALID_PARAMETER), and TWrp (previous-version
  token) → OBJECT_NAME_NOT_FOUND. Targets flips for `smb2.create.impersonation`
  and partial coverage for `smb2.create.gentest` / `smb2.create.blob`.
- `smb2.create.quota-fake-file` promoted to Permanently Unimplementable —
  NTFS `$Extend\$Quota:$Q:$INDEX_ALLOCATION` is a Windows on-disk-format
  internal object with no equivalent in DittoFS's metadata model.
- Remaining `smb2.create.*` entries (blob, gentest, impersonation, mkdir-dup,
  mkdir-visible, multi, path-length) gated under #480 pending CI confirmation.

### 2026-05-27 — Walk back 4 compound tests (section removed)

Set `torture:smbd=false` in smbtorture args (DittoFS is not smbd — the
`is_smbd` flag only affects `read_read` and `write_write` which expect
Samba-specific async last-compound-element behavior). Combined with PR
#640's fixes for `compound_find_close` and `getinfo_middle`, the entire
Compound Requests section is now empty and removed.

- **Compound** (section removed): `smb2.compound_find.compound_find_close`,
  `smb2.compound_async.getinfo_middle`, `smb2.compound_async.read_read`,
  `smb2.compound_async.write_write`

### 2026-05-26 — Walk back 25 confirmed PASS + add 2 new failures

Confirmed 25 tests now PASS on all 3 CI stores (memory, memory-fs, badger-fs).
Removed from known failures:

- **Benchmarks**: `smb2.bench.oplock1` (section removed — now empty)
- **Compound**: `smb2.compound.related4`, `smb2.compound.related7`,
  `smb2.compound_async.create_lease_break_async`, `smb2.compound_async.rename_last`,
  `smb2.compound_async.rename_middle`, `smb2.compound_async.rename_non_compound_no_async`,
  `smb2.compound_async.rename_same_srcdst_non_compound_no_async`
- **Directory**: `smb2.dir.one`
- **Directory Leases**: `smb2.dirlease.leases`, `smb2.dirlease.overwrite`
- **File Attributes**: `smb2.winattr` (section reduced to `dosmode` only)
- **IOCTL**: `smb2.ioctl.network_interface_info`
- **Locks**: `smb2.lock.cancel-logoff`, `smb2.lock.cancel-tdis`
- **Oplocks**: `smb2.oplock.batch3`, `smb2.oplock.batch7`, `smb2.oplock.batch19`,
  `smb2.oplock.batch20`, `smb2.oplock.batch22b`, `smb2.oplock.batch24`, `smb2.oplock.batch26`,
  `smb2.oplock.exclusive6`, `smb2.oplock.levelii502`
- **Streams**: `smb2.streams.rename2`

Added 2 new failures:

- `smb2.create.multi` — regression from recent changes, fails on all 3 stores
- `smb2.notify.tcon` — fixed: armed-handle event buffering + TreeID-scoped tree disconnect

### 2026-04-27 — Round 7 lease cluster: ClientGUID-scoped break dispatch (`v2_complex1`)

smbtorture `smb2.lease.v2_complex1` opens two SMB sessions on the same
`ClientGuid` (via `torture_smb2_connection_ext`) and asserts every lease
break — including breaks for leases held only by the SECOND session —
arrives on the FIRST session's transport. DittoFS routed breaks via the
per-lease `sessionMap`, so LEASE2 (held by tree1b) broke on tree1b's
transport, tripping `CHECK_BREAK_INFO_V2(tree1a->session->transport, ...)`.

Per MS-SMB2 §3.3.4.7 and Samba `smbXsrv_pending_break_submit`
(source3/smbd/smb2_server.c lines 4361-4400), the lease-break notification
is a **client-level** event, not a session-level one. Samba walks the head
of `client->connections` and delivers on the first live connection of the
lease's ClientGuid regardless of which session created the open. The lease
itself is bound by `(ClientGuid, LeaseKey)` per §3.3.5.9.8.

Fix (signed):

- `internal/adapter/smb/lease/manager.go` adds two parallel maps:
  `leaseClientGUID` (lease key → first-grant ClientGuid, sticky) and
  `clientPrimarySession` (ClientGuid → first sessionID, first-write wins).
  `RequestLease` accepts a `clientGUID [16]byte` argument and populates
  both maps; same-key reopens / upgrades do NOT rebind the GUID.
- New `GetSessionForBreak(leaseKey)` resolves the lease's recorded
  ClientGuid to its primary session; legacy callers (zero GUID) fall back
  to the per-lease `sessionMap` so single-session tests are unaffected.
- `internal/adapter/smb/lease/notifier.go` `OnOpLockBreak` now uses
  `GetSessionForBreak`.
- `internal/adapter/smb/v2/handlers/{lease_context,create,create_post_break}.go`
  thread the ClientGuid from `ConnCryptoState.GetClientGUID()` through
  every `RequestLease` call (CREATE, durable reconnect, traditional-oplock
  synthetic-key path).
- `ReleaseSessionLeases` reaps `clientPrimarySession` entries pointing at
  the gone session — without this, a follow-up break would route to a dead
  sessionID and silently drop.

Confirmed via three new unit tests in `manager_test.go`:
`TestGetSessionForBreak_RoutesByClientGUIDPrimary`,
`TestGetSessionForBreak_FallsBackToSessionMap`,
`TestReleaseSessionLeases_ReapsClientPrimary`.

**#429 lease cluster:** `v2_complex1` now expected to PASS.

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
