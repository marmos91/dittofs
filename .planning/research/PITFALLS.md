# Domain Pitfalls: NT Security Descriptors, SMB2 Conformance, and Windows 11 Compatibility

**Domain:** Adding NT Security Descriptors to existing SMB2 server, fixing SMB2 conformance (smbtorture/WPTS), Windows 11 client compatibility
**Researched:** 2026-02-26
**Confidence:** HIGH for pitfalls 1-7 (verified against MS-DTYP/MS-SMB2 specs, Samba source, and existing DittoFS code); MEDIUM for pitfalls 8-14 (WebSearch + community reports); LOW for pitfall 15 (Windows 11 25H2 specific, limited public documentation)

---

## Critical Pitfalls

Mistakes that cause Windows clients to refuse to connect, icacls to crash/show garbage, or smbtorture/WPTS tests to fail systematically.

### Pitfall 1: Security Descriptor Field Ordering Mismatch (MS-DTYP Section 2.4.6)

**What goes wrong:** The self-relative Security Descriptor encodes Owner SID, Group SID, SACL, and DACL in a fixed order (e.g., Owner-Group-DACL), but Windows Server 2003/2008+ actually transmits them in a different order: SACL-DACL-Owner-Group. The offsets are correct, but the byte layout differs from what the spec text implies. A server that always emits Owner-Group-DACL will work for basic queries, but smbtorture tests that perform byte-level comparison against a reference SD will flag mismatches, and some Windows security tools may behave unexpectedly.

**Why it happens:** MS-DTYP Section 2.4.6 says "the order of appearance of pointer target fields is not required to be in any particular order." Developers read the struct definition top-to-bottom and assume Owner comes first. But Windows implementations actually emit SACL before DACL before Owner/Group, and smbtorture's reference comparisons expect the Windows order.

**Consequences:** smbtorture `smb2.acls` tests may fail with binary comparison errors even when the parsed SD is logically correct. `smbcacls` may report unexpected layouts. Windows Explorer security tab works (it uses offsets) but conformance tests flag the difference.

**Prevention:** Follow the Windows-observed field ordering: after the 20-byte SD header, emit SACL (if present), then DACL, then Owner SID, then Group SID. Or better: test empirically against Windows by capturing a reference SD from a real Windows server and matching the exact byte layout. The current DittoFS implementation in `security.go` emits Owner-Group-DACL, which is functional but may need reordering for strict conformance.

**Detection:** Run `smbtorture smb2.acls` and check for "SD mismatch" errors. Capture Wireshark traces comparing DittoFS SD bytes against a Windows server's response.

**DittoFS-specific:** Current `BuildSecurityDescriptor()` in `internal/adapter/smb/v2/handlers/security.go` emits Owner, Group, DACL in that order. The offsets are computed correctly, so Windows clients parse it fine. Conformance tests may require reordering to match Windows behavior.

---

### Pitfall 2: Missing SE_DACL_AUTO_INHERITED and SE_DACL_PROTECTED Control Flags

**What goes wrong:** The Security Descriptor Control field only sets `SE_SELF_RELATIVE (0x8000)` and `SE_DACL_PRESENT (0x0004)`. Windows clients and tools (icacls, Explorer's Security tab) expect `SE_DACL_AUTO_INHERITED (0x0400)` to indicate that the DACL supports automatic inheritance propagation. Without it, icacls shows all ACEs as "not inherited" even when they have the `INHERITED_ACE` flag, and attempting to modify inheritance via Explorer produces confusing results.

**Why it happens:** The existing code sets the bare minimum control flags. The developer does not realize that SE_DACL_AUTO_INHERITED is separate from the per-ACE INHERITED_ACE flag -- the former is a whole-SD flag that tells Windows "this SD was produced by an inheritance-aware system."

**Consequences:**
- icacls shows `(I)` inheritance markers incorrectly or not at all
- Windows Explorer's "Advanced Security Settings" shows inheritance as broken
- smbtorture `smb2.acls.inheritance` tests fail
- SET_INFO calls from Windows that modify inheritance (Protected DACL toggle) produce unexpected results because the server does not preserve or update these flags

**Prevention:** When building the Security Descriptor:
1. Set `SE_DACL_AUTO_INHERITED (0x0400)` if ANY ACE in the DACL has the `INHERITED_ACE` flag
2. Set `SE_DACL_PROTECTED (0x1000)` if the DACL explicitly blocks inheritance propagation
3. When parsing SET_INFO security descriptors from clients, preserve these control flags and propagate them to metadata storage
4. Round-trip these flags: if a client sends SE_DACL_PROTECTED, store it, and return it on subsequent QUERY_INFO

**Detection:** Run `icacls \\server\share\file` on Windows. If all ACEs show as explicitly set (no `(I)` prefix) despite being inherited, this flag is missing.

**DittoFS-specific:** The current `control` variable in `BuildSecurityDescriptor()` only uses `seSelfRelative | seDACLPresent`. Add constants for the auto-inherited and protected flags and set them based on ACE analysis.

---

### Pitfall 3: Non-Canonical ACE Ordering in DACL

**What goes wrong:** The DACL emits ACEs in the same order as the NFSv4 ACL (which is user-defined order per RFC 7530). Windows requires a specific "canonical" ordering: explicit deny ACEs first, then explicit allow ACEs, then inherited deny ACEs, then inherited allow ACEs. If the order is wrong, Windows security tools reorder them silently, icacls output looks inconsistent with what was set, and the effective permissions change unexpectedly.

**Why it happens:** NFSv4 ACLs are evaluated in order (first match wins), so the order is significant and user-defined. Windows DACLs are also evaluated in order, but Windows tools expect and enforce canonical ordering. When translating NFSv4 ACLs to Windows DACLs, preserving the NFSv4 order breaks Windows assumptions.

**Consequences:**
- icacls displays reordered ACEs vs. what the server sent, causing confusion
- Windows Explorer's Security tab reorders ACEs when saving, triggering unnecessary SET_INFO calls
- smbtorture tests that check ACE ordering will fail
- Security-sensitive applications that rely on ACE evaluation order may grant/deny access incorrectly

**Prevention:** When building the DACL from NFSv4 ACEs, sort them into canonical Windows order:
1. Explicit deny ACEs (no INHERITED_ACE flag, type = ACCESS_DENIED)
2. Explicit allow ACEs (no INHERITED_ACE flag, type = ACCESS_ALLOWED)
3. Inherited deny ACEs (INHERITED_ACE flag set, type = ACCESS_DENIED)
4. Inherited allow ACEs (INHERITED_ACE flag set, type = ACCESS_ALLOWED)

**Important caveat:** This reordering changes NFSv4 ACL semantics. The correct approach is to sort for Windows display/conformance but document that the server uses NFSv4 evaluation order internally. For smbtorture conformance, the DACL returned via QUERY_INFO must be in canonical order.

**Detection:** Set a DACL with a DENY ACE after an ALLOW ACE. Query with icacls. If the order is preserved (ALLOW before DENY), the canonical ordering is not applied.

**DittoFS-specific:** The current `buildDACL()` in `security.go` iterates `file.ACL.ACEs` in order, preserving NFSv4 ordering. Add a sort step before encoding.

---

### Pitfall 4: SID Domain Identifier Collision Between Users and Groups

**What goes wrong:** Both `makeDittoFSUserSID()` and `makeDittoFSGroupSID()` produce identical SIDs for the same numeric ID: `S-1-5-21-0-0-0-{id}`. If a user has UID 1000 and a group has GID 1000, they map to the same SID. Windows interprets the Owner SID and Group SID as the same principal, which causes `icacls` to display the group as the owner (or vice versa) and confuses ACL editors.

**Why it happens:** The current code has `makeDittoFSGroupSID` calling `makeDittoFSUserSID` directly -- the comment says "Same format, GID used as RID." This is technically correct for simple cases where UIDs and GIDs don't overlap, but Unix systems commonly reuse numeric IDs across the user/group namespace (e.g., user 1000 and group 1000 are different entities).

**Consequences:**
- icacls shows the file owner and group as the same account
- Windows ACL editor cannot distinguish between "User 1000" and "Group 1000"
- SET_INFO operations that change owner vs. group may have no visible effect
- smbtorture tests that verify Owner SID != Group SID will fail for files where UID == GID

**Prevention:** Use different SID structures for users and groups:
- Users: `S-1-5-21-{domain-id-1}-{domain-id-2}-{domain-id-3}-{uid + 1000}` (RID offset for users)
- Groups: `S-1-5-21-{domain-id-1}-{domain-id-2}-{domain-id-3}-{gid + 10000}` (different RID offset for groups)

Alternatively, use Samba's convention of placing unmapped Unix users in `S-1-22-1-{uid}` and unmapped Unix groups in `S-1-22-2-{gid}`. This is a recognized convention that Windows tools can be taught to display correctly.

**Detection:** Create a file with UID=1000, GID=1000. Run `icacls \\server\share\file`. If Owner and Group show the same SID string, this collision exists.

**DittoFS-specific:** This is present in the current code. `makeDittoFSGroupSID(gid)` is literally `makeDittoFSUserSID(gid)`. Fix requires introducing a distinguishing factor (different domain sub-authority or RID offset).

---

### Pitfall 5: Hash-Based SID for Named Principals Is Non-Deterministic Across Restarts

**What goes wrong:** When `PrincipalToSID()` encounters a named principal like "alice@EXAMPLE.COM" that is not a numeric UID, it falls back to a hash-based RID using a simple `rid = rid*31 + uint32(c)` polynomial hash. This hash is deterministic for the same string, but the RID has no stable reverse mapping. If a client caches the SID (as Windows aggressively does in its SID-to-name cache), and the server restarts or the principal format changes slightly, the SID changes, breaking cached permissions.

**Why it happens:** The hash fallback was a reasonable placeholder, but 32-bit polynomial hashes have collision potential, and there is no way for `SIDToPrincipal()` to reverse the hash back to the original principal name -- it falls through to returning the raw SID string.

**Consequences:**
- Windows client SID caches become stale after server restart or principal name format change
- ACLs set via icacls reference a SID that cannot be resolved back to the original user
- Two different principals may hash to the same RID (collision), granting unintended access
- `SIDToPrincipal()` cannot reverse the mapping, so SET_INFO round-trips lose the principal identity

**Prevention:**
1. Store a persistent `principal <-> RID` mapping table in the control plane database
2. Allocate RIDs sequentially (e.g., starting at 10000) for named principals
3. On `PrincipalToSID()`, look up the table; on miss, allocate a new RID and persist it
4. On `SIDToPrincipal()`, look up the table to get back the original principal string
5. For the v3.6 scope (not full AD integration), a simple in-memory cache seeded from the control plane's User table may suffice

**Detection:** Set an ACL via icacls using a user account. Query it back. If the principal shows as a raw SID string (S-1-5-21-...) instead of the username, the reverse mapping is broken.

**DittoFS-specific:** The hash fallback in `PrincipalToSID()` at line ~276 in `security.go` is the problematic code path. The control plane already has a User model with IDs -- integrate SID allocation with it.

---

### Pitfall 6: STATUS_BUFFER_OVERFLOW Handling for Security Descriptor Queries

**What goes wrong:** When the client requests a security descriptor with `OutputBufferLength` smaller than the actual SD size, the server should return `STATUS_BUFFER_OVERFLOW (0x80000005)` with the truncated data. But the current code returns `STATUS_SUCCESS` with truncated data instead, based on a comment about Linux kernel CIFS treating STATUS_BUFFER_OVERFLOW as an error.

**Why it happens:** An earlier fix for Linux CIFS compatibility changed the behavior for all clients. Linux CIFS clients do treat BUFFER_OVERFLOW as a hard error for some info classes. But Windows clients *expect* BUFFER_OVERFLOW for security descriptor queries -- it is part of the two-phase query pattern where the client first sends a small buffer to learn the required size, then sends a second query with the correct size.

**Consequences:**
- Windows Explorer's Security tab may show incomplete ACLs without retrying
- icacls may silently truncate ACEs
- smbtorture tests checking for STATUS_BUFFER_OVERFLOW will fail
- WPTS BVT tests for QUERY_INFO security info will fail

**Prevention:** Implement client-aware response logic:
1. For InfoType=Security (type 3): Always return STATUS_BUFFER_OVERFLOW when truncating, because Windows clients handle it correctly for security queries
2. For InfoType=File (type 1): Return STATUS_SUCCESS with truncated data for Linux CIFS compatibility
3. Alternatively, check the negotiated dialect or client fingerprint to determine behavior

**Detection:** Use Wireshark to capture a security descriptor query from Windows Explorer where the initial OutputBufferLength is small (often 0 or 1024). Verify the response status is STATUS_BUFFER_OVERFLOW, not STATUS_SUCCESS.

**DittoFS-specific:** The truncation logic in `QueryInfo()` at line ~276 in `query_info.go` always returns STATUS_SUCCESS. Needs a conditional branch for InfoType=Security.

---

### Pitfall 7: Missing SACL_SECURITY_INFORMATION Handling Returns Wrong Error

**What goes wrong:** When a client requests `SACL_SECURITY_INFORMATION (0x08)` in the AdditionalInfo field, the server must either return the SACL or return `STATUS_ACCESS_DENIED` (because SACL access requires `ACCESS_SYSTEM_SECURITY` privilege). Instead, the current code silently ignores the SACL request and returns an SD without it, which makes the client think the file has no SACL rather than that access was denied.

**Why it happens:** The SACL handling is not implemented, so it is simply not included in the response. The developer treats "not implemented" as "not present."

**Consequences:**
- smbtorture `smb2.acls` tests that request SACL information fail
- Windows audit policy tools receive incorrect information
- WPTS tests checking SACL access control will report false results

**Prevention:**
1. If `SACL_SECURITY_INFORMATION` is requested in `additionalSecInfo`, return `STATUS_PRIVILEGE_NOT_HELD (0xC0000061)` or `STATUS_ACCESS_DENIED`
2. Do NOT silently omit the SACL -- the client must know the request was denied, not that the SACL is empty
3. For future work: implement SACL with system audit ACEs

**Detection:** Run `icacls \\server\share\file /t /q` with audit options. If no error is returned but no audit entries appear, SACL handling is missing.

---

## Moderate Pitfalls

### Pitfall 8: Windows 11 24H2+ Mandatory SMB Signing Breaks Guest/Anonymous Access

**What goes wrong:** Windows 11 24H2 and Windows Server 2025 require SMB signing by default on all connections. SMB signing cannot succeed with guest credentials (null session key). DittoFS's anonymous/guest authentication path negotiates signing-enabled but cannot complete the signing handshake, causing Windows 11 24H2 clients to refuse connection with "The specified network password is not correct" or "System error 53."

**Why it happens:** Microsoft hardened SMB security in 24H2, making signing mandatory. Guest sessions have no session key to derive signing keys from. The server advertises `SMB2_NEGOTIATE_SIGNING_ENABLED` in the NEGOTIATE response, and Windows 11 24H2 interprets this as "signing will happen" and rejects the connection when signing fails.

**Consequences:**
- Windows 11 24H2 users cannot connect to DittoFS at all in guest mode
- Users must modify Group Policy (`AllowInsecureGuestAuth`) to connect, which is a poor experience
- Manual Windows 11 testing fails immediately

**Prevention:**
1. Ensure that authenticated sessions (NTLM with actual credentials) produce valid signing keys
2. For guest access: clearly document that Windows 11 24H2+ requires `AllowInsecureGuestAuth` group policy change
3. Long-term (v3.8): implement SMB 3.x signing (AES-CMAC/GMAC) for proper signing support
4. Short-term: ensure the NEGOTIATE response security mode matches what the server can actually deliver
5. Consider not advertising `SMB2_NEGOTIATE_SIGNING_ENABLED` when the server cannot enforce it

**Detection:** Attempt to connect from a fresh Windows 11 24H2 installation. If connection fails immediately, this is the issue.

---

### Pitfall 9: Compound CREATE+QUERY_INFO+CLOSE Security Descriptor Pattern

**What goes wrong:** Windows Explorer and icacls use a compound (related) request pattern: CREATE + QUERY_INFO(Security) + CLOSE in a single compound. The QUERY_INFO uses the FileID from the preceding CREATE via the `SMB2_FLAGS_RELATED_OPERATIONS` mechanism. If the compound handler does not correctly propagate the FileID from the CREATE response to the QUERY_INFO request, the server returns STATUS_INVALID_HANDLE.

**Why it happens:** The compound handler in `compound.go` correctly propagates FileID for operations listed in `InjectFileID()`. But the FileID injection for QUERY_INFO uses offset 24, which must exactly match the CREATE response's FileID field position in the QUERY_INFO request body. If any intermediate operation fails or the response encoding changes the FileID format, propagation breaks.

**Consequences:**
- Windows Explorer's Security tab shows "Unable to display current owner" or "Access denied"
- icacls returns STATUS_INVALID_HANDLE
- First manual test on Windows fails, creating panic

**Prevention:**
1. Add explicit E2E tests for compound CREATE+QUERY_INFO(Security)+CLOSE
2. Verify that `InjectFileID` for QUERY_INFO correctly places the 16-byte FileID at offset 24
3. Test with both related and unrelated compound variants
4. Verify that the CREATE response FileID extraction in `ProcessRequestWithFileID` correctly returns the FileID for the compound handler to propagate

**Detection:** Mount from Windows, right-click a file, select Properties > Security tab. If it fails, the compound security query is broken. Wireshark shows the compound request pattern clearly.

**DittoFS-specific:** The `InjectFileID` function in `compound.go` handles QUERY_INFO at offset 24 and SET_INFO at offset 16. Verify these offsets match the actual request body layout including the StructureSize field.

---

### Pitfall 10: ACE Flag Truncation During NFSv4-to-Windows Translation

**What goes wrong:** NFSv4 ACE flags are 32-bit (`uint32`), but Windows ACE flags are 8-bit (`uint8`). The current code truncates with `uint8(ace.Flag & 0xFF)`, which preserves the low byte. However, the NFSv4 `INHERITED_ACE` flag is `0x00000080`, which fits in 8 bits, but `SUCCESSFUL_ACCESS_ACE_FLAG (0x10)` and `FAILED_ACCESS_ACE_FLAG (0x20)` also fit. The problem is that the NFSv4 flag bit positions are intentionally aligned with Windows positions (per RFC 7530), so the truncation is correct for standard flags -- but if DittoFS ever stores non-standard flags in bits 8-31, they will be silently lost.

**Why it happens:** The truncation is technically correct today, but fragile. Future code that adds custom flags in the upper bits will break silently.

**Consequences:**
- Currently none (standard flags fit in 8 bits)
- Future risk: custom ACE flags added for DittoFS-specific purposes will be silently dropped on the Windows side

**Prevention:**
1. Validate that all stored ACE flags fit in 8 bits before truncation
2. Log a warning if `ace.Flag & 0xFFFFFF00 != 0` (upper bits set but will be lost)
3. Document the 8-bit flag constraint for future developers

**Detection:** Unit test: create an ACE with flags > 0xFF, build SD, parse it back, verify the flags were truncated (and that a warning was logged).

---

### Pitfall 11: Missing FILE_PERSISTENT_ACLS in FileFsAttributeInformation

**What goes wrong:** The FileFsAttributeInformation response advertises filesystem attributes including `FILE_PERSISTENT_ACLS (0x00000008)`. This flag tells Windows that the filesystem supports ACLs. If this flag is missing, Windows Explorer will not show the Security tab at all, and icacls will return "The command line is too long."

**Why it happens:** The developer adds ACL support in QUERY_INFO/SET_INFO but forgets to update the filesystem attributes response. Windows checks the filesystem capabilities before showing ACL-related UI.

**Consequences:**
- No Security tab in Windows Explorer
- icacls cannot read or set ACLs
- All ACL testing on Windows fails immediately

**Prevention:** Verify that the `FileFsAttributeInformation` response (case 5 in `buildFilesystemInfo()`) includes `FILE_PERSISTENT_ACLS (0x00000008)` in the capabilities bitmask.

**Detection:** Right-click a file in Windows Explorer, select Properties. If there is no Security tab, check the filesystem attributes.

**DittoFS-specific:** The current value is `0x0000008F` which includes bits: `FILE_CASE_SENSITIVE_SEARCH (0x01)`, `FILE_CASE_PRESERVED_NAMES (0x02)`, `FILE_UNICODE_ON_DISK (0x04)`, `FILE_PERSISTENT_ACLS (0x08)`, `FILE_SUPPORTS_REPARSE_POINTS (0x80)`. The `FILE_PERSISTENT_ACLS` bit IS already set. Verify this is not accidentally removed during refactoring.

---

### Pitfall 12: Sparse File READ Returns Error Instead of Zeros for Unwritten Regions

**What goes wrong:** When reading from an unwritten region of a file (offset beyond what has been written but within the file size), the server returns an error or short read instead of zero-filled data. Windows clients expect sparse file regions to read as zeros.

**Why it happens:** The payload store does not have data for unwritten blocks. The read operation either returns an error (block not found) or a short read (fewer bytes than requested). SMB clients interpret errors as I/O failures and short reads as EOF.

**Consequences:**
- Files created with FILE_OPEN_IF + WRITE at offset 1MB will fail to read at offset 0
- Applications that create sparse files (common on Windows) will see data corruption
- This is existing issue #180 in the project tracker

**Prevention:**
1. When `PayloadService.ReadAt()` encounters an unwritten block/region, return zero-filled bytes
2. The cache layer should handle this transparently: cache miss for an unwritten region should produce zeros, not an error
3. Test with: create file, write at offset 1MB, read at offset 0 -- must get zeros

**Detection:** Create a file, write 1 byte at offset 1048576. Read from offset 0. If the read fails or returns non-zero data, this bug is present.

**DittoFS-specific:** This is tracked as issue #180. The fix belongs in the payload service's read path, not in the SMB handler.

---

### Pitfall 13: Renamed Directory Children Show Stale Path in QUERY_DIRECTORY

**What goes wrong:** After renaming a directory via SET_INFO(FileRenameInformation), the children of that directory still report the old path in their metadata. QUERY_DIRECTORY lists the correct filenames (they are stored relative to the parent), but any operation that uses the child's stored `Path` field (like CHANGE_NOTIFY) uses the stale path.

**Why it happens:** The metadata store's `Move()` operation updates the directory entry itself but does not recursively update the `Path` field of all children. The `Path` field is denormalized (stored for convenience) and becomes stale.

**Consequences:**
- CHANGE_NOTIFY watchers on renamed directories stop receiving notifications
- Compound operations that reference the old path fail with STATUS_OBJECT_PATH_NOT_FOUND
- This is existing issue #181 in the project tracker

**Prevention:**
1. When `Move()` renames a directory, recursively update the `Path` field of all descendants
2. Alternatively, stop storing the full path in `OpenFile` and always derive it from the handle hierarchy
3. The `StoreOpenFile()` call in SET_INFO rename updates the current file but not any open handles to children

**Detection:** Create directory `/test`, create file `/test/foo.txt`, rename directory to `/renamed`, list `/renamed` -- the files should appear. But CHANGE_NOTIFY on `/renamed` may not fire.

**DittoFS-specific:** This is tracked as issue #181. The fix is in the metadata service's `Move` operation.

---

## Minor Pitfalls

### Pitfall 14: QUERY_INFO OutputBufferOffset Incorrect for Security Info

**What goes wrong:** The QUERY_INFO response sets `OutputBufferOffset` to a fixed value (header size + struct size). But for security descriptor responses, some Windows clients use this offset to locate the SD data within the response. If the offset does not match the actual data position (e.g., due to padding differences), the client reads garbage.

**Prevention:** Ensure `OutputBufferOffset` in `QueryInfoResponse.Encode()` correctly points to where the Data field starts. The current formula `64+9 = 73` should be correct for SMB2 header (64) + QUERY_INFO response fixed part (9 bytes before data). Verify with Wireshark.

**Detection:** Wireshark packet analysis: check that the OutputBufferOffset field matches the actual position of the security descriptor bytes in the response.

---

### Pitfall 15: Windows 11 25H2 SMBv1 Fallback Breaking Discovery

**What goes wrong:** Windows 11 25H2 completely removes SMBv1 support for NetBIOS-based connections. If DittoFS is discovered via NetBIOS name resolution, Windows 11 25H2 clients cannot find the server at all.

**Prevention:** Ensure DittoFS is accessible via DNS name or IP address directly on port 445 (TCP), not relying on NetBIOS for name resolution. Document this requirement for Windows 11 25H2 users.

**Detection:** Test from Windows 11 25H2 using both `\\server-name\share` and `\\ip-address\share`. If name-based access fails but IP-based works, NetBIOS discovery is the issue.

---

### Pitfall 16: Negotiate Capabilities Not Advertising Leasing Support

**What goes wrong:** The NEGOTIATE response sets Capabilities to 0 (no capabilities). Windows 11 clients may send lease requests via create contexts regardless, but some conformance tests verify that the server advertises leasing support before testing lease behavior.

**Prevention:** Set `CapLeasing (0x02)` in the NEGOTIATE response Capabilities field since DittoFS already supports leases. This is a minor issue but can cause WPTS test classification problems.

**DittoFS-specific:** The NEGOTIATE handler in `negotiate.go` sets Capabilities to 0. Update to advertise supported capabilities.

---

## Integration Pitfalls: Security Descriptors Meeting Existing ACL Model

These pitfalls are specific to how the new NT Security Descriptor code interacts with DittoFS's existing NFSv4 ACL model.

### Integration Pitfall A: ACL Round-Trip Fidelity Loss

**What goes wrong:** A Windows client reads a Security Descriptor (QUERY_INFO), makes no changes, and writes it back (SET_INFO). But the round-trip through NFSv4 ACL format loses information:
1. Windows ACE types not in NFSv4 (e.g., SYSTEM_MANDATORY_LABEL_ACE) are silently dropped
2. SID-to-principal-to-SID conversion is lossy for non-DittoFS SIDs
3. ACE canonical ordering on output differs from stored NFSv4 order

The net effect is that a no-op SET_INFO modifies the stored ACL, which triggers CHANGE_NOTIFY events and confuses clients that compare SDs for equality.

**Prevention:**
1. Cache the raw SD bytes alongside the NFSv4 ACL. On QUERY_INFO, if the cached SD is present and the ACL has not been modified via NFS, return the cached bytes directly
2. On SET_INFO, store both the NFSv4 ACL translation AND the raw SD bytes
3. Invalidate the cached SD when the ACL is modified via NFS or the control plane

**Detection:** Use Wireshark to capture QUERY_INFO Security, then immediately SET_INFO Security with the same bytes. Capture QUERY_INFO Security again. The bytes should be identical.

---

### Integration Pitfall B: NFS and SMB ACL Modification Conflict

**What goes wrong:** An NFS client modifies file permissions (via SETATTR mode bits or NFSv4 ACLs). The mode-to-ACL synchronization in `pkg/metadata/acl/mode.go` produces a new ACL. But the cached raw SD bytes (from Integration Pitfall A's fix) are now stale. The next SMB client QUERY_INFO returns the old SD.

**Prevention:**
1. When any ACL modification occurs (from any protocol), invalidate the cached SD
2. Use a version counter on the ACL: increment on every modification, check version on SD cache hit
3. The metadata service's `SetFileAttributes` already updates timestamps -- add SD cache invalidation at the same point

**Detection:** Modify file permissions via NFS (`chmod`), then check permissions from Windows (`icacls`). If the Windows view does not reflect the NFS change, the cache is stale.

---

### Integration Pitfall C: Cross-Protocol Owner/Group Identity Mismatch

**What goes wrong:** An SMB client sets the Owner SID to `S-1-5-21-0-0-0-2000` via SET_INFO Security. This maps to UID 2000 via `sidToUID()`. An NFS client then reads the file's owner as UID 2000 via GETATTR. But if DittoFS's identity mapper (used for NFS) maps UID 2000 to "bob@EXAMPLE.COM" while the SMB SID was originally set by "alice" (whose Windows SID happened to map to RID 2000), there is an identity mismatch.

**Prevention:**
1. Ensure the control plane User model has both UID and SID fields
2. SID allocation should be derived from the User model, not computed on-the-fly
3. When SET_INFO changes the owner SID, resolve it through the control plane to get the canonical UID
4. When GETATTR returns the UID, ensure it was set through the same identity resolution path

**Detection:** Create a file from Windows as user "alice." Check the owner from NFS. If the owner name does not resolve to "alice," the identity mapping is inconsistent.

---

## Phase-Specific Warnings

| Phase Topic | Likely Pitfall | Mitigation |
|-------------|---------------|------------|
| Security Descriptor encoding | Pitfall 1 (field ordering) | Match Windows byte layout, test with Wireshark |
| SD control flags | Pitfall 2 (missing auto-inherited) | Set SE_DACL_AUTO_INHERITED when ACEs have INHERITED flag |
| DACL construction | Pitfall 3 (ACE ordering) | Sort into canonical Windows order before encoding |
| SID mapping | Pitfall 4 (user/group collision) | Use different RID ranges or S-1-22-1/S-1-22-2 convention |
| Named principal SIDs | Pitfall 5 (hash instability) | Persistent principal-to-RID mapping table |
| QUERY_INFO truncation | Pitfall 6 (wrong status code) | Return STATUS_BUFFER_OVERFLOW for security queries |
| SACL handling | Pitfall 7 (silent omission) | Return STATUS_PRIVILEGE_NOT_HELD |
| Windows 11 24H2 testing | Pitfall 8 (signing required) | Ensure NTLM produces valid signing keys |
| Compound requests | Pitfall 9 (FileID propagation) | E2E test compound CREATE+QUERY_INFO(Security)+CLOSE |
| Filesystem attributes | Pitfall 11 (missing PERSISTENT_ACLS) | Verify bit 0x08 in FileFsAttributeInformation |
| Sparse file read | Pitfall 12 (zeros not returned) | Fix issue #180 in payload service |
| Directory rename path | Pitfall 13 (stale paths) | Fix issue #181 in metadata Move |
| ACL round-trip | Integration A (fidelity loss) | Cache raw SD bytes alongside NFSv4 ACL |
| Cross-protocol ACL | Integration B (stale cache) | Invalidate SD cache on any ACL modification |
| Identity mapping | Integration C (owner mismatch) | Derive SIDs from control plane User model |

## Sources

- [MS-DTYP: Security Descriptor (Section 2.4.6)](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/7d4dac05-9cef-4563-a058-f108abecce1d) -- HIGH confidence
- [MS-SMB2: QUERY_INFO Request (Section 2.2.37)](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/d623b2f7-a5cd-4639-8cc9-71fa7d9f9ba9) -- HIGH confidence
- [MS-SMB2: Handling Compounded Related Requests](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/46dd4182-62d3-4e30-9fe5-e2ec124edca1) -- HIGH confidence
- [Order of ACEs in a DACL (Microsoft)](https://learn.microsoft.com/en-us/windows/win32/secauthz/order-of-aces-in-a-dacl) -- HIGH confidence
- [Samba SID Mapping Bug Discussion](https://lists.samba.org/archive/samba-technical/2017-January/118369.html) -- MEDIUM confidence
- [Samba SD Field Ordering Discussion](https://lists.samba.org/archive/cifs-protocol/2009-November/001104.html) -- MEDIUM confidence
- [Windows 11 24H2 SMB Signing Changes (Microsoft)](https://techcommunity.microsoft.com/blog/filecab/smb-security-hardening-in-windows-server-2025--windows-11/4226591) -- HIGH confidence
- [Windows 11 24H2 NAS Compatibility (Microsoft)](https://techcommunity.microsoft.com/blog/filecab/accessing-a-third-party-nas-with-smb-in-windows-11-24h2-may-fail/4154300) -- HIGH confidence
- [Enable Insecure Guest Logons (Microsoft)](https://learn.microsoft.com/en-us/windows-server/storage/file-server/enable-insecure-guest-logons-smb2-and-smb3) -- HIGH confidence
- [Samba smbtorture Lease Test Failures](https://samba-technical.samba.narkive.com/suMaLLor/broken-smbtorture-lease-cases) -- MEDIUM confidence
- [Microsoft WindowsProtocolTestSuites (GitHub)](https://github.com/microsoft/WindowsProtocolTestSuites) -- HIGH confidence
- DittoFS source code: `internal/adapter/smb/v2/handlers/security.go`, `query_info.go`, `set_info.go`, `compound.go`, `negotiate.go`, `converters.go` -- verified by direct reading
