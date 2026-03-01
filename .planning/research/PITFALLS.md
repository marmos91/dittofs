# Domain Pitfalls: SMB3 Protocol Upgrade (3.0/3.0.2/3.1.1)

**Domain:** Adding SMB3 features (encryption, signing, leases, Kerberos, durable handles, cross-protocol integration) to existing SMB2.0.2/2.1 implementation
**Researched:** 2026-02-28
**Confidence:** HIGH for pitfalls 1-7 (verified against MS-SMB2 spec, Microsoft blog test vectors, and DittoFS source code); MEDIUM for pitfalls 8-12 (WebSearch + Samba bug reports + community experience); MEDIUM for pitfall 13-15 (cross-protocol coordination based on existing DittoFS architecture analysis)

---

## Critical Pitfalls

Mistakes that cause Windows clients to refuse to connect, encryption/signing failures, data corruption, or fundamental protocol violations that block all SMB3 functionality.

### Pitfall 1: Preauth Integrity Hash Chain Computed Over Wrong Bytes

**What goes wrong:** The SMB 3.1.1 preauth integrity hash chain must be computed over the *complete wire bytes* of each NEGOTIATE and SESSION_SETUP message (request and response), including the SMB2 header. A common mistake is hashing only the body, only the first command of a compound, or using the parsed/reconstructed message rather than the exact bytes received on the wire.

**Why it happens:** The hash chain feeds into key derivation. Developers often store parsed request structures and reconstruct them for hashing, introducing byte differences (padding, alignment, field ordering). The spec says the hash is over "the full message, starting from the beginning of the SMB2 Header to the end of the message" -- meaning the raw TCP payload *excluding* the 4-byte NetBIOS header but *including* the 64-byte SMB2 header.

**Consequences:** Session keys derived from a wrong preauth integrity hash will be incorrect. All subsequent signing verification fails. Windows clients receive STATUS_ACCESS_DENIED on the first signed request after SESSION_SETUP completes. The failure is silent and hard to diagnose because the session setup itself succeeds -- keys are derived after the final SESSION_SETUP response.

**Prevention:**
1. Store the exact raw bytes of every NEGOTIATE req/resp and SESSION_SETUP req/resp before any parsing
2. Hash = SHA-512(PreviousHash || MessageBytes) for each message in sequence
3. For SESSION_SETUP responses with STATUS_MORE_PROCESSING_REQUIRED, the response signature field is zeroed (unsigned) -- hash the bytes as-is including the zero signature
4. For the final SESSION_SETUP response (STATUS_SUCCESS), hash the bytes *before* computing the signature (the signature itself is computed with the derived signing key)
5. Write test vectors matching Microsoft's published examples (see Sources) and validate byte-for-byte

**Detection:** Windows client connects, SESSION_SETUP succeeds, but the very first subsequent command (TREE_CONNECT) fails with STATUS_ACCESS_DENIED. The server log shows signature verification failure.

**Phase:** Must be implemented and validated in the negotiate/preauth phase (Phase 1 of SMB3 milestone).

---

### Pitfall 2: Key Derivation Label/Context Strings Differ Between 3.0 and 3.1.1

**What goes wrong:** SMB 3.0/3.0.2 and SMB 3.1.1 use different label strings and context values for the SP800-108 KDF. Using the wrong labels for a given dialect produces incorrect keys. All four key types (SigningKey, EncryptionKey, DecryptionKey, ApplicationKey) have different labels between dialect families.

**Why it happens:** The spec defines the labels in different sections, and the change between 3.0.x and 3.1.1 is subtle. The label strings are:

**SMB 3.0 / 3.0.2:**
- SigningKey: Label="SMB2AESCMAC\0", Context="SmbSign\0"
- EncryptionKey: Label="SMB2AESCCM\0", Context="ServerIn \0" (note trailing space)
- DecryptionKey: Label="SMB2AESCCM\0", Context="ServerOut\0"
- ApplicationKey: Label="SMB2APP\0", Context="SmbRpc\0"

**SMB 3.1.1:**
- SigningKey: Label="SMBSigningKey\0", Context=PreauthIntegrityHashValue (64 bytes)
- EncryptionKey: Label="SMBC2SCipherKey\0", Context=PreauthIntegrityHashValue
- DecryptionKey: Label="SMBS2CCipherKey\0", Context=PreauthIntegrityHashValue
- ApplicationKey: Label="SMBAppKey\0", Context=PreauthIntegrityHashValue

**Consequences:** Signing and encryption fail completely for the affected dialect. If the server uses 3.1.1 labels when the negotiated dialect is 3.0, Windows clients disconnect immediately after session setup.

**Prevention:**
1. Implement a single KDF function that takes label and context as parameters
2. Select labels/contexts based on `Connection.Dialect` at key derivation time
3. Null-terminate labels correctly (the `\0` is part of the label, not a C string artifact)
4. For 3.0.x, context is a fixed string; for 3.1.1, context is the 64-byte SHA-512 hash value
5. Test with both SMB 3.0 and 3.1.1 clients independently
6. Validate against Microsoft's published test vectors (SMB 3.0 multichannel and SMB 3.1.1 multichannel examples in the MS blog)

**Detection:** smbtorture signing tests fail. Windows client shows "Access Denied" after session setup.

**Phase:** Must be correct in the cryptographic key derivation phase. This is the most common cause of "works in testing, breaks with Windows" failures.

---

### Pitfall 3: Encryption Transform Header vs Plain SMB2 Header Confusion

**What goes wrong:** When encryption is active, SMB3 messages are wrapped in an SMB2_TRANSFORM_HEADER (ProtocolId 0xFD534D42 = 0xFD 'S' 'M' 'B') instead of the normal SMB2 header (0xFE 'S' 'M' 'B'). The framing layer must detect the transform header, decrypt the payload, and then dispatch the inner plain SMB2 message. Common mistakes:

1. **Trying to parse the transform header as a regular SMB2 header** -- the first 4 bytes differ (0xFD vs 0xFE)
2. **Signing the inner message when encryption is active** -- MS-SMB2 explicitly states that if decryption succeeds, signature verification MUST be skipped
3. **Using wrong nonce size** -- AES-128-CCM uses 11 bytes of the 16-byte Nonce field; AES-128-GCM uses 12 bytes
4. **Encrypting responses that should be plain** -- NEGOTIATE and the first SESSION_SETUP leg cannot be encrypted (no keys yet)

**Why it happens:** The existing DittoFS framing layer (`internal/adapter/smb/framing.go`) reads the NetBIOS header then checks for `types.SMB2ProtocolID` (0x424D53FE). Adding transform header support requires intercepting before this check. The transform header has a completely different layout: Signature(16) + Nonce(16) + OriginalMessageSize(4) + Reserved(2) + Flags(2) + SessionId(8).

**Consequences:** Encrypted messages are rejected as malformed. Windows clients that require encryption (per-share or per-server) cannot connect. If the server accidentally signs AND encrypts, the client may reject the inner message.

**Prevention:**
1. In `ReadRequest`, check the first 4 bytes: 0xFE = plain SMB2, 0xFD = transform header, 0xFF = SMB1
2. For transform headers, read the full transform header (52 bytes), look up the session's decryption key by SessionId, decrypt the payload, then parse the inner SMB2 message
3. After successful decryption, skip signature verification entirely (per MS-SMB2 3.3.5.2.1.1)
4. For AES-CCM: nonce is bytes [0:11] of the Nonce field. For AES-GCM: nonce is bytes [0:12]. The remaining bytes must be zero
5. Never encrypt NEGOTIATE or first SESSION_SETUP (check if session has encryption keys before encrypting)
6. Set the transform header Flags field: 0x0001 = encrypted (SMB 3.0+)

**Detection:** Windows client with encryption enabled gets "The specified network name is no longer available" error. tcpdump shows transform headers being sent but server responds with STATUS_INVALID_PARAMETER.

**Phase:** Must be implemented alongside the negotiate context phase (encryption capability negotiation) and wired into the framing layer early.

---

### Pitfall 4: AES-256 Key Length Mismatch (SMB 3.1.1 with AES-256-CCM/GCM)

**What goes wrong:** SMB 3.1.1 supports AES-256-CCM and AES-256-GCM in addition to AES-128 variants. When AES-256 is negotiated, the 'L' parameter in the KDF must be 256 (not 128), producing 32-byte keys. Implementations that hardcode L=128 generate 16-byte keys that silently truncate to the wrong length for AES-256 operations.

**Why it happens:** SMB 3.0 only supported AES-128-CCM, so early implementations hardcode key length to 16 bytes. The MS-SMB2 spec states: "If Connection.CipherId is AES-128-CCM or AES-128-GCM, 'L' value is initialized to 128. If Connection.CipherId is AES-256-CCM or AES-256-GCM, 'L' value is initialized to 256."

**Consequences:** AES-256 encryption/decryption produces garbage. The client receives corrupted data and disconnects. This only manifests when connecting to Windows Server 2022+ or other servers that prefer AES-256.

**Prevention:**
1. Store `Connection.CipherId` after negotiate
2. Parameterize KDF on cipher ID: L=128 for AES-128-*, L=256 for AES-256-*
3. EncryptionKey and DecryptionKey buffer size must match: 16 bytes for AES-128, 32 bytes for AES-256
4. SigningKey remains 16 bytes regardless of cipher (CMAC uses 128-bit keys)

**Detection:** Connections work with AES-128-GCM but fail with AES-256-GCM. smbtorture encryption tests with AES-256 fail.

**Phase:** Key derivation and encryption implementation phase.

---

### Pitfall 5: Signing Algorithm Change -- HMAC-SHA256 vs AES-128-CMAC

**What goes wrong:** The existing DittoFS signing implementation (`internal/adapter/smb/signing/signing.go`) uses HMAC-SHA256 exclusively, which is correct for SMB 2.0.2/2.1. SMB 3.0+ requires AES-128-CMAC for signing. If the server negotiates a 3.x dialect but continues using HMAC-SHA256, every signed message will fail verification on the client.

**Why it happens:** The current `SigningKey.Sign()` method hardcodes `hmac.New(sha256.New, sk.key[:])`. When upgrading to support 3.x dialects, developers may forget to switch the algorithm because the signing key size (16 bytes) and signature size (16 bytes) are identical for both algorithms.

**Consequences:** All commands after SESSION_SETUP fail signature verification. Windows clients disconnect with "The cryptographic signature is invalid."

**Prevention:**
1. Add a `SigningAlgorithm` field to `SigningKey` or `SessionSigningState` (HMAC-SHA256 for 2.x, AES-128-CMAC for 3.x)
2. The signing key for 3.x is derived via KDF (not directly from session key as in 2.x)
3. For 3.x signing: use `crypto/aes` + CMAC implementation (Go stdlib does not include CMAC; use a library or implement per RFC 4493)
4. Keep backward compatibility: if negotiated dialect is 2.0.2 or 2.1, use existing HMAC-SHA256 path
5. SMB 3.1.1 additionally supports AES-128-GMAC signing (negotiated via SMB2_SIGNING_CAPABILITIES context) -- implement as a third option

**Detection:** Windows client connects with SMB 3.x, SESSION_SETUP succeeds, first subsequent command fails.

**Phase:** Must be implemented alongside key derivation. The signing module needs refactoring before any 3.x dialect can work.

---

### Pitfall 6: Negotiate Context Ordering and Padding Requirements

**What goes wrong:** SMB 3.1.1 negotiate contexts must appear in a specific order and each context must be 8-byte aligned. Common mistakes:
1. Missing PREAUTH_INTEGRITY_CAPABILITIES (mandatory) or placing it after ENCRYPTION_CAPABILITIES
2. Incorrect padding between contexts (each must start at an 8-byte aligned offset relative to the start of the negotiate context list)
3. Setting HashAlgorithmCount > 1 in the response (must be exactly 1)
4. Not echoing back contexts that the client sent -- if client sends ENCRYPTION_CAPABILITIES, server MUST respond with ENCRYPTION_CAPABILITIES even if no common cipher exists

**Why it happens:** The existing negotiate handler (`internal/adapter/smb/v2/handlers/negotiate.go`) has no negotiate context parsing at all -- fields like `negotiateContextOffset`, `negotiateContextCount` are commented out. Adding context support requires careful binary layout work.

**Consequences:** Windows 10+ clients that send SMB 3.1.1 in the dialect list either fall back to 2.1 (losing all 3.x features) or fail the connection entirely if the client requires 3.1.1.

**Prevention:**
1. Parse negotiate contexts from the request (they start at the offset specified in the negotiate request header)
2. In the response, emit contexts in order: PREAUTH_INTEGRITY first, then ENCRYPTION
3. Pad each context to 8-byte boundary (add zero bytes after the context data)
4. PREAUTH_INTEGRITY response: exactly 1 hash algorithm (SHA-512, ID=0x0001), plus a server-generated 32-byte salt
5. ENCRYPTION response: exactly 1 cipher (the preferred one from the client's list)
6. Additional optional contexts: SMB2_SIGNING_CAPABILITIES (cipher 0x0001=AES-CMAC, 0x0002=AES-GMAC), SMB2_COMPRESSION_CAPABILITIES, SMB2_RDMA_TRANSFORM_CAPABILITIES

**Detection:** Windows client silently negotiates SMB 2.1 instead of 3.1.1 (check negotiated dialect in server logs).

**Phase:** First phase of SMB3 implementation -- negotiate handling.

---

### Pitfall 7: Session Key Derivation Timing -- Keys Before Signature Verification

**What goes wrong:** In the final SESSION_SETUP exchange, the server must:
1. Complete GSS authentication (get the session key)
2. Update the preauth integrity hash with the final SESSION_SETUP request
3. Derive all cryptographic keys using the preauth hash as context (for 3.1.1)
4. Sign the SESSION_SETUP response with the newly derived signing key
5. Send the response

If steps 2-4 happen in the wrong order, the derived keys are wrong. Specifically:
- The hash MUST be updated with the final request BEFORE key derivation
- The hash must NOT include the final response (the response is signed, not hashed)
- For STATUS_MORE_PROCESSING_REQUIRED responses, hash is updated with both request AND response

**Why it happens:** The multi-leg NTLM/SPNEGO exchange involves multiple SESSION_SETUP round-trips. Each leg updates the hash. The asymmetry (hash the final request but not the final response) is easy to get wrong.

**Consequences:** Derived keys are incorrect. The server signs the final SESSION_SETUP response with a wrong key. Windows client verifies the signature, fails, and disconnects.

**Prevention:**
1. Maintain a `PreauthIntegrityHashValue` per-connection (initialized from negotiate) and per-session (forked at first SESSION_SETUP)
2. For each SESSION_SETUP round-trip:
   - Hash the request: `H = SHA-512(H_prev || request_bytes)`
   - If response status is MORE_PROCESSING_REQUIRED: `H = SHA-512(H || response_bytes)`
   - If response status is SUCCESS: derive keys using H as context, then sign the response with derived SigningKey
3. The response to the final leg is NOT included in the hash used for key derivation
4. Test with both single-leg (Kerberos) and multi-leg (NTLM) authentication

**Detection:** Kerberos auth works (single leg) but NTLM fails (multi-leg), or vice versa.

**Phase:** Authentication and key derivation phase.

---

## Moderate Pitfalls

### Pitfall 8: Lease vs Oplock Backward Compatibility During Dialect Transition

**What goes wrong:** The existing DittoFS implementation supports both traditional oplocks (SMB 2.0.2/2.1) and leases (SMB 2.1+ via the Unified Lock Manager). When a client negotiates SMB 3.0+, it will exclusively use Lease V2 contexts (SMB2_CREATE_REQUEST_LEASE_V2 with 52 bytes including ParentLeaseKey and Epoch). If the server still processes these as Lease V1 (32 bytes), the ParentLeaseKey (used for directory leases) and Epoch (used for break sequencing) are silently lost.

**Why it happens:** The existing `DecodeLeaseCreateContext` in `lease.go` already handles both V1 and V2 formats based on data length, which is good. However, the risk is in the *response*: SMB 3.0+ clients that sent V2 contexts expect V2 responses. Sending a V1 response (32 bytes) to a V2 request causes the client to ignore the lease context entirely.

**Prevention:**
1. Track the lease version negotiated per-connection (V1 for dialect <= 2.1, V2 for dialect >= 3.0)
2. Always respond with the same version the client requested
3. For directory leases (new in SMB 3.0+): handle the ParentLeaseKey correctly -- it links a file lease to its parent directory lease for hierarchical break notifications
4. Epoch must be tracked and incremented correctly for V2 -- the existing implementation increments `lease.Lease.Epoch` which is correct, but must ensure it flows through to the response context

**Detection:** Windows Explorer directory refresh is slow (directory leases not working). File save from Office apps takes extra time (lease upgrade fails).

**Phase:** Lease integration phase.

---

### Pitfall 9: SPNEGO Multi-Leg Kerberos with MIC Requirement

**What goes wrong:** The existing SPNEGO/Kerberos implementation in `session_setup.go` handles single-leg Kerberos (AP-REQ -> accept-complete). However, some Kerberos exchanges can be multi-leg (e.g., when mutual authentication is required or when the client requests a MIC). The server must handle:
1. NegTokenInit with Kerberos AP-REQ -> NegTokenResp with accept-complete + AP-REP (single leg)
2. NegTokenInit with Kerberos AP-REQ -> NegTokenResp with accept-incomplete + AP-REP -> NegTokenResp with MIC (multi-leg)

Additionally, SPNEGO fallback from Kerberos to NTLM must be handled: if Kerberos fails (wrong SPN, expired ticket), the server should send reject with a hint to try NTLM, not just STATUS_LOGON_FAILURE.

**Why it happens:** The current code (`handleKerberosAuth`) validates AP-REQ and immediately returns accept-complete. It does not build an AP-REP token. While Windows clients accept this for basic scenarios, strict SPNEGO implementations (like MIT Kerberos-based Linux clients) may require the AP-REP for mutual authentication. Samba bug #14106 documents similar SPNEGO fallback issues.

**Prevention:**
1. Build the AP-REP from the service context and include it in the NegTokenResp
2. Handle NegStateRequestMIC in subsequent SESSION_SETUP legs
3. When Kerberos fails, respond with NegStateReject but include NTLM as the supported mech, allowing SPNEGO fallback
4. Track SPNEGO negotiation state per-session (not just NTLM pending auth state)
5. The preauth integrity hash must be updated for every SESSION_SETUP leg, including Kerberos multi-leg

**Detection:** Linux `smbclient` with Kerberos fails while Windows client works. MIT Kerberos-based clients report "mutual authentication failed."

**Phase:** Authentication phase -- when integrating shared Kerberos layer.

---

### Pitfall 10: Durable Handle V2 Reconnect Validation Complexity

**What goes wrong:** Durable Handle V2 reconnect (SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2) has an extraordinarily long list of validation conditions (per MS-SMB2 section 3.3.5.9.12). Missing any single check creates a security vulnerability or protocol violation. The complete list includes:

1. Look up Open by FileId.Persistent in GlobalOpenTable
2. If not found and persistent flag set, look up by CreateGuid
3. Validate Open.Lease is not NULL and Open.ClientGuid matches Connection.ClientGuid
4. Validate Open.CreateGuid matches the request's CreateGuid
5. Validate Open.IsDurable or Open.IsResilient is TRUE
6. Validate Open.Session is NULL (previous session was cleaned up)
7. If Open.Lease is NULL, lease context must NOT be present in request
8. If Open.Lease is NOT NULL, matching lease context MUST be present
9. Lease key must match between Open.Lease.LeaseKey and the create context
10. Lease version must match (V1 context for V1 lease, V2 for V2)
11. Open.Lease.LeaseState must contain Handle caching for durable reconnect
12. Open.DurableOwner must match Session.SecurityContext (same user)
13. File name must match Open.Lease.FileName (if lease is not null and FileDeleteOnClose is false)
14. Cannot combine with DurableHandleRequest, DurableReconnect V1, or DurableHandleRequest V2

**Why it happens:** Durable handles interact with sessions, leases, security contexts, and file state simultaneously. Each condition addresses a specific attack vector or protocol invariant. Samba's implementation had multiple bugs (#13318, #13535, #11897, #11898) related to these checks.

**Prevention:**
1. Implement checks as a sequential validation pipeline with early return on first failure
2. Return STATUS_OBJECT_NAME_NOT_FOUND for most validation failures (as spec requires)
3. Return STATUS_INVALID_PARAMETER for conflicting create contexts
4. Return STATUS_ACCESS_DENIED for security context mismatch
5. Store CreateGuid, LeaseKey, DurableOwner, and FileName with each Open for validation
6. Write a smbtorture `durable_v2_reconnect` test for each validation condition individually

**Detection:** smbtorture durable_v2 tests fail. Windows Excel "recovered unsaved changes" feature does not work after network interruption.

**Phase:** Durable handles phase -- one of the later phases due to dependency on leases and session management.

---

### Pitfall 11: Durable Handle State Persistence Across Server Restart

**What goes wrong:** Durable handles survive connection drops but must also survive server restarts for "persistent handles." The server must persist:
- Open state (FileId, CreateGuid, DurableOwner)
- Lease state (LeaseKey, LeaseState, Epoch)
- File path and metadata handle

Without persistence, all durable handles are lost on restart, making the feature useless for connection resilience.

**Why it happens:** DittoFS already has lock state persistence in the metadata store (per-share), which the existing lease implementation uses. However, durable handle state (the Open object with its FileId, CreateGuid, and DurableOwner) is separate from lease state and must be stored in a new table/structure. The Unified Lock Manager stores lease info but not the full SMB Open state.

**Prevention:**
1. Design a DurableOpenStore interface (like LockStore but for Open objects)
2. Store: FileId.Persistent, CreateGuid, LeaseKey, DurableOwner (security context), FilePath, CreateOptions, DesiredAccess
3. On reconnect, validate stored state against the reconnect request
4. Set a durable handle timeout (default 60 seconds) -- if no reconnect within timeout, clean up
5. For DittoFS, implement in the control plane store (GORM-based) alongside session/share data
6. Grace period on restart: allow reconnects within a configurable window (similar to NFS grace period already implemented)

**Detection:** Server restart causes all SMB clients to show "network path not found" instead of transparently reconnecting.

**Phase:** Durable handles persistence phase.

---

### Pitfall 12: Secure Dialect Negotiation (SMB 3.0.x) vs Preauth Integrity (3.1.1)

**What goes wrong:** SMB 3.0 and 3.0.2 use a post-authentication FSCTL_VALIDATE_NEGOTIATE_INFO IOCTL to verify the negotiate exchange was not tampered with. SMB 3.1.1 replaced this with preauth integrity (hash chain). If the server negotiates 3.0.x but does not implement FSCTL_VALIDATE_NEGOTIATE_INFO, Windows clients will fail the connection after tree connect.

**Why it happens:** The IOCTL handler (`internal/adapter/smb/v2/handlers/stub_handlers.go`) likely stubs IOCTL. FSCTL_VALIDATE_NEGOTIATE_INFO is a specific IOCTL (CtlCode=0x00140204) that the server must handle by validating the client's negotiate information matches the server's state.

**Prevention:**
1. Implement FSCTL_VALIDATE_NEGOTIATE_INFO handler in IOCTL dispatch
2. Compare: Capabilities, Guid, SecurityMode, Dialects from the request against the values from the original negotiate
3. If any mismatch, return STATUS_ACCESS_DENIED (potential MITM)
4. The IOCTL must be signed (per spec requirement)
5. For 3.1.1, this IOCTL is not needed (preauth integrity supersedes it), but the server should still handle it gracefully if a client sends it

**Detection:** Windows 8/8.1/Server 2012 clients (which negotiate 3.0/3.0.2) disconnect immediately after tree connect. Windows 10+ clients work fine (negotiate 3.1.1 which does not use FSCTL_VALIDATE_NEGOTIATE_INFO).

**Phase:** Early phase -- must be part of negotiate/IOCTL handling.

---

## Cross-Protocol Pitfalls

Mistakes specific to DittoFS's multi-protocol (NFS + SMB) architecture. These are particularly important because DittoFS's competitive advantage includes cross-protocol consistency.

### Pitfall 13: SMB3 Lease Break During NFS Delegation Recall Race

**What goes wrong:** DittoFS already has cross-protocol oplock break coordination (`CheckAndBreakForWrite`, `CheckAndBreakForRead`, `CheckAndBreakForDelete` in `oplock.go`). When upgrading to SMB3 leases (which are more granular than SMB2 oplocks), the break logic must account for:

1. **Directory leases (new in SMB3)**: NFS directory operations (CREATE, REMOVE, RENAME in a directory) must break SMB3 directory leases. The existing cross-protocol code only handles file-level leases.
2. **Epoch-based sequencing**: SMB3 lease breaks include an epoch counter. NFS-triggered breaks must correctly increment the epoch. Stale epoch values cause clients to ignore the break.
3. **Simultaneous breaks**: An NFS write can trigger an SMB3 Write lease break while an NFS delegation recall is already in progress for the same file. The Unified Lock Manager must handle concurrent breaks without deadlock.

**Why it happens:** The existing OplockManager uses a single `sync.RWMutex` (`mu`) for all lease operations. When an NFS handler calls `CheckAndBreakForWrite` (which takes `mu`), and the break notification goroutine tries to send the break (which may eventually need `mu` for updating state), there is a potential for contention. The existing code avoids deadlock by sending breaks asynchronously, but the epoch tracking requires careful coordination.

**Prevention:**
1. Add directory lease awareness to cross-protocol break methods (new method: `CheckAndBreakForDirectoryChange`)
2. Ensure epoch is atomically incremented and persisted before the break notification is sent
3. Test: NFS CREATE in directory while SMB3 directory lease active -> directory lease break + epoch increment
4. Test: NFS WRITE while SMB3 R+W+H lease active -> break W to R, increment epoch, keep R+H
5. Add integration test: simultaneous NFS delegation recall + SMB3 lease break on same file

**Detection:** SMB3 clients show stale directory listings after NFS modifications. Files created via NFS not visible in Windows Explorer until manual refresh.

**Phase:** Cross-protocol integration phase (late in milestone).

---

### Pitfall 14: Cross-Protocol Lock/Lease Semantics Mismatch with Handle Leases

**What goes wrong:** SMB3 Handle (H) leases protect against "surprise deletion" -- the client gets notified before a file is deleted or renamed, allowing it to close gracefully. NFS has no equivalent concept. When an NFS client issues REMOVE or RENAME, DittoFS must break the Handle lease BEFORE completing the operation. The existing `CheckAndBreakForDelete` does this, but:

1. The break is asynchronous (goroutine) -- the NFS handler may complete the delete before the SMB client acknowledges the break
2. The method returns `ErrLeaseBreakPending` but the NFS handler may not wait for acknowledgment
3. If the NFS handler proceeds without waiting, the SMB client loses data (cached writes not flushed)

**Why it happens:** The current design returns `ErrLeaseBreakPending` to signal the caller should wait, but NFS v3/v4 handlers may not have retry logic for this error. NFSv4 delegations have a recall mechanism with a timeout, but the NFS handler dispatching is separate from SMB lease break dispatching.

**Prevention:**
1. NFS handlers that trigger lease breaks MUST block until break is acknowledged or times out (the existing `OpLockBreakScanner` handles timeout)
2. Implement a `WaitForBreakAcknowledgment(leaseKey, timeout)` method on OplockManager
3. NFS REMOVE/RENAME flow: `CheckAndBreakForDelete -> if pending, WaitForBreakAcknowledgment(30s) -> complete delete`
4. If timeout expires, the lease is force-revoked (existing scanner behavior) and the NFS operation proceeds
5. Test: SMB3 client has R+W+H lease, NFS client removes file -> SMB3 gets break notification, flushes writes, acknowledges, NFS delete completes

**Detection:** NFS delete succeeds but SMB client loses cached (unflushed) writes. Data loss scenario.

**Phase:** Cross-protocol integration phase. This is a data safety issue and must be thoroughly tested.

---

### Pitfall 15: ACL Consistency Between SMB3 Encrypted Sessions and NFS Kerberos

**What goes wrong:** SMB3 encrypted sessions authenticate via SPNEGO/Kerberos or NTLM, producing a Windows security context (SID-based). NFS Kerberos produces a Unix identity (UID/GID-based). The existing DittoFS SID mapping (Samba-style RID allocation: uid*2+1000, gid*2+1001) must produce consistent mappings regardless of access path. When SMB3 encryption is enabled, the security context may carry additional claims or group memberships that differ from the NFS Kerberos path.

**Why it happens:** The SMB session maps Kerberos principal -> control plane user -> SID (via RID allocation). The NFS adapter maps Kerberos principal -> control plane user -> UID/GID. Both should resolve to the same control plane user, but edge cases exist:
1. Username normalization differs (SMB strips realm, NFS may not)
2. Group membership queries differ (SMB uses Windows token groups, NFS uses supplementary GIDs from AUTH_SYS or Kerberos principal groups)
3. The POSIX-to-DACL synthesis produces Security Descriptors from mode bits, but mode bits are computed from the Unix identity. If the Unix identity differs between SMB and NFS paths, the generated DACL differs.

**Prevention:**
1. Ensure principal-to-username normalization is identical in both `handleKerberosAuth` (SMB) and the NFS RPCSEC_GSS handler
2. Use the same control plane user lookup for both paths (already done -- both use `userStore.GetUser`)
3. When generating Security Descriptors, always use the file's stored UID/GID (from metadata), not the accessing user's identity
4. Test: create file via NFS Kerberos, query Security Descriptor via SMB3 -> Owner SID should match the NFS user's mapped SID

**Detection:** `icacls` shows different owners for files created via NFS vs SMB. Permission denied errors when accessing NFS-created files via SMB or vice versa.

**Phase:** Cross-protocol integration phase.

---

## Minor Pitfalls

### Pitfall 16: MaxTransactSize / MaxReadSize / MaxWriteSize Negotiation for 3.x

**What goes wrong:** SMB 3.x introduces multi-credit operations where reads/writes can be larger than 64KB. The existing negotiate response sets max sizes but may not coordinate with credit charges. For SMB 3.x, a single operation can use multiple credits (1 credit per 64KB), and the CreditCharge header field indicates how many credits the operation consumes.

**Prevention:** Ensure credit charge validation in the dispatch layer accepts multi-credit operations. The existing credit system tracks outstanding credits per-session but may not validate CreditCharge > 1.

**Phase:** Negotiate phase.

---

### Pitfall 17: Connection.ClientGuid Tracking for Durable Handles

**What goes wrong:** SMB 3.x uses ClientGuid (sent in NEGOTIATE) to identify client instances across connections. Durable handle reconnect validates that the reconnecting connection's ClientGuid matches the original. If DittoFS does not track ClientGuid per-connection, durable reconnect validation fails.

**Prevention:** Store ClientGuid from the NEGOTIATE request in Connection state. Validate during durable reconnect. This also enables multi-channel detection (same ClientGuid on different connections = same client).

**Phase:** Negotiate phase (store) and durable handles phase (validate).

---

### Pitfall 18: TREE_CONNECT Encryption Flag Per-Share

**What goes wrong:** SMB 3.x supports per-share encryption via the SMB2_SHAREFLAG_ENCRYPT_DATA flag in TREE_CONNECT response. If the share has encryption enabled and the client does not support encryption, the server must reject the tree connect. The existing tree connect handler does not check encryption capabilities.

**Prevention:** Add encryption configuration per-share in the control plane. In TREE_CONNECT handler, check if share requires encryption and if the session supports it. Return STATUS_ACCESS_DENIED if the client cannot encrypt.

**Phase:** Encryption phase.

---

## Phase-Specific Warnings

| Phase Topic | Likely Pitfall | Mitigation |
|---|---|---|
| Negotiate Contexts (3.1.1) | Pitfalls 1, 6 - Hash chain and context ordering | Store raw message bytes; parse contexts in order; 8-byte align; validate with test vectors |
| Key Derivation | Pitfalls 2, 4, 7 - Wrong labels/lengths/timing | Dialect-aware KDF; parameterized L value; correct hash-before-derive ordering |
| Signing Upgrade | Pitfall 5 - Wrong algorithm | AES-128-CMAC for 3.x; keep HMAC-SHA256 for 2.x; add algorithm field to signing state |
| Encryption | Pitfalls 3, 4, 18 - Transform header, nonce, per-share | New framing layer for 0xFD; correct nonce sizes; per-share encryption config |
| Leases | Pitfall 8 - V1 vs V2 response mismatch | Track lease version per-connection; match response to request version |
| SPNEGO/Kerberos | Pitfall 9 - Multi-leg, AP-REP, fallback | Build AP-REP; handle MIC; NTLM fallback path |
| Durable Handles | Pitfalls 10, 11, 17 - Validation, persistence, ClientGuid | Implement all 14+ validation checks; persist Open state; track ClientGuid |
| Secure Dialect (3.0.x) | Pitfall 12 - Missing IOCTL | Implement FSCTL_VALIDATE_NEGOTIATE_INFO for 3.0/3.0.2 clients |
| Cross-Protocol | Pitfalls 13, 14, 15 - Directory leases, Handle lease blocking, ACL consistency | Directory lease breaks; blocking wait for Handle lease; unified principal normalization |

---

## Sources

### Microsoft Official Documentation (HIGH confidence)
- [MS-SMB2: Generating Cryptographic Keys](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/da4e579e-02ce-4e27-bbce-3fc816a3ff92)
- [MS-SMB2: Handling SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/62ba68d0-8806-4aef-a229-eefb5827160f)
- [MS-SMB2: SMB2_PREAUTH_INTEGRITY_CAPABILITIES](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5a07bd66-4734-4af8-abcf-5a44ff7ee0e5)
- [MS-SMB2: Receiving an SMB2 NEGOTIATE Request](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/b39f253e-4963-40df-8dff-2f9040ebbeb1)
- [MS-SMB2: Receiving an SMB2 SESSION_SETUP Request](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/e545352b-9f2b-4c5e-9350-db46e4f6755e)
- [SMB 2 and SMB 3 Security: Anatomy of Signing and Cryptographic Keys](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-2-and-smb-3-security-in-windows-10-the-anatomy-of-signing-and-cryptographic-keys) -- includes complete test vectors for SMB 3.0 and 3.1.1 key derivation
- [SMB 3.1.1 Pre-authentication Integrity in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10)
- [SMB 3.1.1 Encryption in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-encryption-in-windows-10)
- [Encryption in SMB 3.0: A Protocol Perspective](https://learn.microsoft.com/en-us/archive/blogs/openspecification/encryption-in-smb-3-0-a-protocol-perspective)

### Samba Implementation Experience (MEDIUM confidence)
- [Implementing Persistent Handles in Samba (SNIA SDC 2018)](https://www.snia.org/sites/default/files/SDC/2018/presentations/SMB/Bohme_Ralph_Implementing_Persistent_Handles_Samba.pdf)
- [Samba Bug #14106: Fix spnego fallback from kerberos to ntlmssp](https://bugzilla.samba.org/show_bug.cgi?id=14106)
- [Samba Bug #13722: Enabling SMB3 encryption breaks macOS clients](https://www.illumos.org/issues/13722)
- [Samba's Way Toward SMB 3.0 (USENIX ;login:)](https://www.usenix.org/system/files/login/articles/03adam_016-025_online.pdf)

### Protocol Specification References
- [SP800-108: Recommendation for Key Derivation Using Pseudorandom Functions](https://nvlpubs.nist.gov/nistpubs/Legacy/SP/nistspecialpublication800-108.pdf)
- [RFC 4493: The AES-CMAC Algorithm](https://www.ietf.org/rfc/rfc4493.txt)
- [RFC 4178: SPNEGO](https://datatracker.ietf.org/doc/html/rfc4178)

### DittoFS Codebase Analysis (HIGH confidence)
- `internal/adapter/smb/signing/signing.go` -- HMAC-SHA256 only, needs AES-CMAC for 3.x
- `internal/adapter/smb/v2/handlers/negotiate.go` -- Negotiate contexts commented out, max dialect 2.1
- `internal/adapter/smb/framing.go` -- Only handles 0xFE protocol ID, no transform header (0xFD)
- `internal/adapter/smb/v2/handlers/session_setup.go` -- Kerberos single-leg only, no AP-REP
- `internal/adapter/smb/v2/handlers/lease.go` -- V1/V2 decode exists, cross-protocol breaks exist
- `internal/adapter/smb/v2/handlers/oplock.go` -- Cross-protocol integration exists, needs directory lease support
- `internal/adapter/smb/session/session.go` -- No preauth hash state, no durable handle tracking
- `internal/adapter/smb/types/constants.go` -- Dialect constants and capabilities defined for 3.x already
