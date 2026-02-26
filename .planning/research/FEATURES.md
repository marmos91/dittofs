# Feature Landscape: SMB2 Conformance Fixes and Windows Compatibility

**Domain:** SMB2 protocol conformance, NT Security Descriptors, Windows 11 client compatibility
**Researched:** 2026-02-26
**Confidence:** HIGH for bug fixes and security descriptors (codebase analysis + MS-SMB2 spec), MEDIUM for conformance test specifics (web research + Samba documentation)

## Table Stakes

Features/fixes users and test suites expect. Missing = Windows clients fail, conformance tests error out, or icacls returns garbage.

### Bug Fixes (Known Issues)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Sparse file READ zero-fill (#180) | READ at offsets beyond written data must return zeros, not error or garbage | Low | `internal/adapter/smb/v2/handlers/read.go`, `pkg/payload/` | Current code reads from PayloadService which may return short reads or errors for unwritten ranges. Must detect gaps between file metadata size and actual written blocks, and return zero-filled buffers for unwritten regions. SMB2 READ spec requires this implicitly -- clients assume contiguous file data. |
| Renamed directory listing (#181) | After SET_INFO rename, QUERY_DIRECTORY on renamed dir must reflect new Path | Low | `internal/adapter/smb/v2/handlers/set_info.go`, `pkg/metadata/` Move operation | The Move operation in MetadataService likely does not update the stored Path field of child entries. After renaming `/old` to `/new`, ReadDirectory on `/new` returns entries with stale paths rooted at `/old`. Fix is in the metadata Move operation, not SMB handlers. |

### Security Descriptor Improvements (#182)

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Proper default DACL from POSIX permissions | Files without explicit ACL must synthesize a DACL from UID/GID/mode instead of granting Everyone full access | Medium | `security.go` buildDACL function, `pkg/metadata/` | Current code falls back to a single ACE granting S-1-1-0 (Everyone) FILE_ALL_ACCESS when no ACL exists. This makes `icacls` show overly permissive ACLs. Must synthesize owner/group/other ACEs from Unix mode bits (similar to Samba's `acl_xattr:default acl style = posix`). |
| ACE ordering (deny before allow) | Windows requires ACCESS_DENIED_ACE before ACCESS_ALLOWED_ACE in DACL | Low | `security.go` buildDACL function | MS-DTYP 2.4.5 requires canonical ACE ordering: explicit deny, explicit allow, inherited deny, inherited allow. Current code preserves NFSv4 ACL order which may violate this. Windows Security tab and icacls will warn about "non-canonical" ACLs. |
| Inheritance flags on ACEs | Parent directory ACEs need CONTAINER_INHERIT and OBJECT_INHERIT flags | Medium | `security.go`, `pkg/metadata/acl/` | Without inheritance flags, new files/dirs in a directory do not inherit parent ACL. Windows Explorer "Security" tab will show inherited entries as empty. Need CI (0x02) and OI (0x01) flags on directory ACEs. |
| SACL support (audit ACEs) | SACL query must return valid empty SACL, not omit it | Low | `security.go` BuildSecurityDescriptor | Some tools query SACL_SECURITY_INFORMATION. Returning no SACL offset is technically valid but some conformance tests expect SE_SACL_PRESENT flag when SACL is requested. Return empty SACL structure. |
| SE_DACL_AUTO_INHERITED control flag | Security descriptor should set auto-inherited flags | Low | `security.go` BuildSecurityDescriptor | Windows clients check SE_DACL_AUTO_INHERITED (0x0400) to determine if ACL was propagated. Without it, Security tab shows "Inherited permissions are disabled" warning. |

### Well-Known SID Completeness

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| BUILTIN\Administrators SID (S-1-5-32-544) | icacls and Security tab expect admin group in default ACL | Low | `security.go` well-known SIDs | Current well-known SID map has only 3 entries (Everyone, CREATOR OWNER, CREATOR GROUP). Need BUILTIN\Administrators, BUILTIN\Users, NT AUTHORITY\SYSTEM, Authenticated Users. |
| NT AUTHORITY\SYSTEM SID (S-1-5-18) | Windows expects SYSTEM in default DACL | Low | `security.go` default DACL | Samba always includes NT AUTHORITY\SYSTEM with full control in synthesized DACLs. Omitting it makes Windows tools show warnings. |
| Domain-aware SID construction | SIDs should use a consistent machine domain RID (not S-1-5-21-0-0-0) | Medium | `security.go` makeDittoFSUserSID, config | Current implementation uses S-1-5-21-0-0-0-{uid} which is technically a valid local SID but looks fake. Should use a stable pseudo-domain SID derived from ServerGUID (e.g., hash to 3 sub-authorities) so SIDs persist across restarts and look legitimate to Windows tools. |

### CREATE Response Context Encoding

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| MxAc (Maximal Access) response context | Windows Explorer sends MxAc in every CREATE; expects response with maximal access mask | Medium | `create.go`, `encoding.go` | Current CREATE handler does not parse or respond to MxAc create contexts. Windows clients use MxAc to determine what context menus to show (rename, delete, properties). Without it, Explorer may show limited options or fall back to querying access separately. Response is 8 bytes: QueryStatus(4) + MaximalAccess(4). |
| QFid (Query on Disk ID) response context | Some clients request QFid; server must return 32-byte opaque blob | Low | `create.go` | Current code ignores QFid requests. Response is 32 bytes of opaque file identifier. Use metadata FileHandle bytes padded to 32. |
| Create context wire encoding in CREATE response | Current Encode() method hardcodes CreateContextsOffset=0, Length=0 | Medium | `create.go` Encode() method | Even when lease response context is populated, the current CREATE response encoder does not serialize create contexts to wire format. Lease responses (RqLs) and MxAc responses are silently dropped. Must implement proper create context chain encoding per MS-SMB2 2.2.14.2. |

### SMB Signing for Windows 11 24H2

| Feature | Why Expected | Complexity | Dependencies | Notes |
|---------|--------------|------------|--------------|-------|
| Functional SMB signing on all responses | Windows 11 24H2 requires signing by default; unsigned responses cause STATUS_ACCESS_DENIED | High | `signing/signing.go`, negotiate handler, session setup | DittoFS has signing infrastructure but must ensure it is active for all authenticated sessions. Windows 11 24H2 rejects connections where signing negotiation fails. This is the most critical Windows 11 compatibility issue based on community reports. |
| Guest access signing exemption handling | Guest sessions cannot use signing (no session key); must handle gracefully | Medium | `session_setup.go`, signing layer | Windows 11 24H2 blocks insecure guest logons by default. DittoFS must either: (a) not offer guest access, or (b) correctly handle the signing-vs-guest negotiation so Windows does not reject the session. |

## Differentiators

Features that improve Windows experience beyond basic compliance. Not required for conformance but valued by real-world users.

| Feature | Value Proposition | Complexity | Dependencies | Notes |
|---------|-------------------|------------|--------------|-------|
| Proper FILE_ATTRIBUTE_ARCHIVE flag | Windows uses Archive bit for backup tracking; setting it on modified files matches NTFS behavior | Low | `converters.go` fileAttrToSMBAttributesInternal | Most SMB servers set Archive on modified files. DittoFS currently never sets it. Low effort, improves Windows backup tool compatibility. |
| Short name (8.3) generation in directory listings | Some older Windows apps and conformance tests expect ShortName field populated | Medium | `query_directory.go` directory entry builders | Current code leaves ShortName as zeros. Not critical for modern Windows but some smbtorture tests check it. Generate from long name using standard algorithm. |
| FilePositionInformation support | Some clients use FilePositionInformation for seeking | Low | `query_info.go` buildFileInfoFromStore | Currently returns NOT_SUPPORTED. Should return current offset (0 is acceptable default). |
| FileModeInformation support | Queried by some conformance tests | Low | `query_info.go` | Return FILE_SYNCHRONOUS_IO_NONALERT (0x20) which is the standard mode for network files. |
| FileCompressionInformation support | Windows Explorer queries this for Properties dialog | Low | `query_info.go` | Return zeros (no compression). Avoids "cannot retrieve properties" error in Explorer. |
| Proper STATUS_BUFFER_OVERFLOW for security queries | Return STATUS_BUFFER_OVERFLOW instead of STATUS_SUCCESS when SD is truncated | Low | `query_info.go` QueryInfo handler | Current code truncates and returns SUCCESS. The MS-SMB2 spec says security info queries should return STATUS_BUFFER_OVERFLOW when OutputBufferLength is too small. Note the comment about Linux CIFS -- this needs to only apply to security queries, not file info queries. |
| CHANGE_NOTIFY completion on session close | Clean up pending CHANGE_NOTIFY when session disconnects | Low | `stub_handlers.go`, connection management | Currently watches leak until directory handle closes. Windows Explorer holds many CHANGE_NOTIFY requests; they should complete with STATUS_CANCELLED on disconnect. |
| FileAttributeTagInformation (class 35) | Queried by Windows for reparse point type identification | Low | `query_info.go` | Needed for proper symlink display in Windows Explorer. Return FileAttributes + ReparseTag (0 for non-reparse, IO_REPARSE_TAG_SYMLINK for symlinks). |
| FileNormalizedNameInformation (class 48) | Windows 10/11 queries this for path normalization | Low | `query_info.go` | Return the full normalized path from share root. Helps Windows resolve case differences. |

## Anti-Features

Features to explicitly NOT build in v3.6. These belong in v3.8 (SMB3 Protocol Upgrade) or later.

| Anti-Feature | Why Avoid | What to Do Instead |
|--------------|-----------|-------------------|
| SMB 3.0/3.0.2/3.1.1 dialect negotiation | Full SMB3 is a separate milestone (v3.8) with encryption, new signing, preauth integrity | Return STATUS_NOT_SUPPORTED for SMB3 dialects; stick to 2.0.2/2.1 for v3.6 |
| AES encryption (SMB3) | Requires SMB3 session setup, key derivation, per-packet encrypt/decrypt | Defer to v3.8; not needed for conformance at SMB2.1 level |
| Durable handles (DHnQ/DH2Q) | Complex reconnection semantics, persistent state | Return no durable handle context in CREATE response; Windows falls back gracefully |
| Multi-channel / RDMA | SMB3 feature for multiple network paths | Defer to v3.8; return NOT_SUPPORTED for FSCTL_QUERY_NETWORK_INTERFACE_INFO (already done) |
| Server-side copy (FSCTL_SRV_COPYCHUNK) | Requires chunk-based copy coordination | Return NOT_SUPPORTED (already done); implement in v3.8 or v4.0 |
| Extended Attributes (EA) support | Requires xattr metadata layer not yet built | Return EaSize=0 in all queries (already done); EA support comes in v4.0 with NFSv4.2 xattrs |
| POSIX Extensions for SMB | Linux-specific POSIX create context; not needed for Windows compatibility | Defer indefinitely; DittoFS handles POSIX via NFS path |
| Full SACL enforcement (audit logging) | Requires Windows event log integration and privilege checks | Return empty SACL when queried; do not enforce audit entries |

## Feature Dependencies

```
Bug Fixes (independent):
  #180 sparse READ ── payload layer zero-fill
  #181 renamed dir listing ── metadata Move path update

Security Descriptors (#182):
  Default DACL from POSIX ──> ACE ordering ──> Inheritance flags
                          |
                          └──> Well-known SIDs (SYSTEM, Administrators)
                                    |
                                    └──> Domain-aware SID construction
                                              |
                                              └──> SE_DACL_AUTO_INHERITED flag

CREATE Response Contexts:
  Wire encoding fix ──> MxAc response ──> QFid response
       |
       └──> Lease response actually sent (currently silently dropped)

Windows 11 Signing:
  Signing enforcement ──> Guest access handling
```

## MVP Recommendation

### Phase 1 (Must Fix -- Bugs and Broken Behavior)

1. **Sparse file READ zero-fill (#180)** -- Blocks basic file operations on Windows when files have gaps
2. **Renamed directory listing (#181)** -- Blocks Explorer navigation after rename
3. **CREATE response context wire encoding** -- Lease responses and future MxAc responses are silently dropped without this

### Phase 2 (Must Fix -- Windows Compatibility)

4. **Default DACL from POSIX permissions** -- icacls returns meaningless results without this
5. **ACE ordering (deny before allow)** -- Windows warns about non-canonical ACLs
6. **Well-known SIDs (SYSTEM, Administrators)** -- Default DACL must include these
7. **MxAc create context response** -- Windows Explorer relies on this for UI decisions
8. **SMB signing enforcement** -- Windows 11 24H2 rejects unsigned sessions

### Phase 3 (Should Fix -- Conformance)

9. **Inheritance flags on ACEs** -- Required for proper directory ACL propagation
10. **Domain-aware SID construction** -- SIDs look fake without stable domain RID
11. **SE_DACL_AUTO_INHERITED flag** -- Removes "inheritance disabled" warning
12. **QFid create context response** -- Some conformance tests check this
13. **Missing FileInfoClass handlers** -- FileAttributeTagInformation, FileCompressionInformation, etc.
14. **Guest access signing negotiation** -- Handle Windows 11 24H2 guest restrictions

Defer:
- **Short name (8.3) generation:** Low priority, only affects legacy apps and some smbtorture tests
- **Full SACL support:** Return empty SACL; actual audit logging is out of scope
- **SMB3 features (encryption, durable handles, multi-channel):** v3.8 milestone

## Conformance Test Landscape

### smbtorture Test Categories (Expected Failure Areas)

| Test Category | What It Tests | Likely DittoFS Failures | Severity |
|---------------|---------------|------------------------|----------|
| smb2.connect | Basic connection and negotiate | Should pass if signing works | Low |
| smb2.session | Session setup, re-auth, binding | May fail on signing-required scenarios | Medium |
| smb2.create.gentest | All CREATE disposition/option combinations | May fail on edge cases (invalid attrs, specific access masks) | Medium |
| smb2.create.contexts | Create context parsing and response | Will fail -- MxAc, QFid not returned, context encoding broken | High |
| smb2.read | READ at various offsets, zero-length, past EOF | Will fail on sparse file reads (#180) | High |
| smb2.write | WRITE with various sizes, at EOF, past EOF | Should mostly pass | Low |
| smb2.lock | Byte-range locking, shared/exclusive, cancel | Should mostly pass (Unified Lock Manager) | Low |
| smb2.setinfo | SET_INFO for rename, disposition, timestamps | May fail on rename edge cases (#181) | Medium |
| smb2.getinfo | QUERY_INFO for all info classes | Will fail for unsupported classes (compression, position, mode) | Medium |
| smb2.getinfo.security | Security descriptor query/set | Will fail -- default DACL wrong, ACE order, missing SIDs | High |
| smb2.lease | Lease grant, break, epoch tracking | May partially fail if context encoding is broken | Medium |
| smb2.notify | CHANGE_NOTIFY operations | Should mostly pass (already implemented) | Low |
| smb2.oplock | Oplock grant and break | Should mostly pass | Low |
| smb2.dir | Directory enumeration, patterns | Should mostly pass | Low |

### Microsoft WPTS (WindowsProtocolTestSuites) BVT

| Test Area | What It Tests | Expected Result |
|-----------|---------------|-----------------|
| Negotiate BVT | Dialect selection, capabilities | Should pass for SMB2.1 |
| Session BVT | Authentication, signing setup | Depends on signing correctness |
| TreeConnect BVT | Share connection, IPC$ | Should pass |
| Create BVT | Basic file open/create | May fail on MxAc context |
| QueryInfo BVT | FileBasicInfo, FileStandardInfo | Should pass |
| SetInfo BVT | Rename, timestamps, disposition | May fail on rename edge cases |
| Security BVT | SD query and set | Will fail -- DACL issues |
| Lock BVT | Byte-range locking | Should pass |
| IOCTL BVT | ValidateNegotiateInfo | Should pass |

## Windows 11 Client Quirks (Verified from Community Reports)

| Quirk | Impact | Mitigation |
|-------|--------|------------|
| SMB signing required by default (24H2) | Connection fails without proper signing | Ensure signing works for authenticated sessions |
| Guest logon blocked by default (24H2 Pro) | Guest/anonymous access rejected | Support proper NTLM auth; document that guest access requires client-side GPO change |
| Username with domain prefix | Win11 sends `domain\user` or `user@domain` instead of bare username | NTLM auth handler must strip domain prefix before control plane lookup |
| Credential caching behavior | Win11 caches and retries with Microsoft Account credentials | Proper STATUS_LOGON_FAILURE on bad creds forces credential dialog |
| SMB2.1 lease required by default | Win11 requests leases on every CREATE | Must respond with lease context (currently broken due to encoding) |
| FileFsAttributeInformation check | Win11 checks FILE_PERSISTENT_ACLS flag | Already set in current code (0x0000008F); must keep |
| Previous Versions tab | Explorer queries FSCTL_SRV_ENUMERATE_SNAPSHOTS | Already returns empty snapshot list; working |
| Properties dialog | Queries FileCompressionInformation, FileAttributeTagInformation | Must add these info classes to avoid error dialogs |

## Sources

- [MS-SMB2 2.2.13.2: SMB2_CREATE_CONTEXT Request Values](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/75364667-3a93-4e2c-b771-592d8d5e876d) -- HIGH confidence
- [MS-SMB2 2.2.14.2: SMB2_CREATE_CONTEXT Response Values](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/893bff02-5815-4bc1-9693-669ed6e85307) -- HIGH confidence
- [MS-DTYP 2.4.6: Security Descriptor Structure](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/) -- HIGH confidence
- [Windows 11 24H2 SMB Signing Changes](https://learn.microsoft.com/en-us/windows-server/storage/file-server/smb-signing) -- HIGH confidence
- [Windows 11 24H2 Third-Party NAS Compatibility](https://techcommunity.microsoft.com/blog/filecab/accessing-a-third-party-nas-with-smb-in-windows-11-24h2-may-fail/4154300) -- HIGH confidence
- [Enable Insecure Guest Logons](https://learn.microsoft.com/en-us/windows-server/storage/file-server/enable-insecure-guest-logons-smb2-and-smb3) -- HIGH confidence
- [Samba vfs_acl_xattr: POSIX-to-NT ACL Translation](https://www.samba.org/samba/docs/current/man-html/vfs_acl_xattr.8.html) -- MEDIUM confidence
- [Samba idmap: Unix-to-SID Mapping Architecture](https://samba.tranquil.it/doc/en/samba_fundamentals/about_winbindd.html) -- MEDIUM confidence
- [smbtorture SMB2 Test Framework](https://wiki.samba.org/index.php/Writing_Torture_Tests) -- MEDIUM confidence
- [Microsoft WindowsProtocolTestSuites](https://github.com/microsoft/WindowsProtocolTestSuites) -- MEDIUM confidence
- [Notes on Sparse Files and File Sharing](https://learn.microsoft.com/en-us/archive/blogs/openspecification/notes-on-sparse-files-and-file-sharing) -- MEDIUM confidence
- [libfwnt Security Descriptor Documentation](https://github.com/libyal/libfwnt/blob/main/documentation/Security%20Descriptor.asciidoc) -- MEDIUM confidence
- DittoFS codebase analysis (internal/adapter/smb/) -- HIGH confidence
