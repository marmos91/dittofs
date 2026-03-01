# Project Research Summary

**Project:** DittoFS SMB3 Protocol Upgrade (v3.8)
**Domain:** SMB3 protocol implementation (3.0/3.0.2/3.1.1 dialects with encryption, signing, leases, Kerberos, durable handles)
**Researched:** 2026-02-28
**Confidence:** HIGH

## Executive Summary

The SMB3 upgrade is an additive enhancement to DittoFS's existing SMB2.0.2/2.1 adapter. The codebase is remarkably well-positioned: dialect constants for 3.0/3.0.2/3.1.1 already exist, Kerberos infrastructure is shared and functional, SPNEGO parsing is complete, and the lease system (OplockManager) already supports V2 leases with epoch tracking. The primary work is implementing five cryptographic layers: (1) SP800-108 key derivation (KDF), (2) AES-CCM/GCM encryption, (3) AES-CMAC/GMAC signing, (4) SHA-512 preauth integrity hash chain, and (5) transform header framing. These integrate into existing connection/session management without requiring architectural changes.

The recommended approach is to build in dependency order: negotiate + key derivation + signing first (everything else needs correct keys), then encryption (depends on KDF), then leases/Kerberos (extends existing systems), then durable handles (most complex, depends on everything). Only one new external dependency is needed: `github.com/aead/cmac` for AES-CMAC signing. AES-CCM encryption should be implemented internally (~200 lines) rather than importing a library. All other crypto uses Go stdlib.

The critical risk is the preauth integrity hash chain (SMB 3.1.1): it must be computed over exact wire bytes, not parsed/reconstructed messages. A single byte difference in hashing causes all keys to derive incorrectly, leading to signature verification failures that are silent and hard to diagnose. Microsoft's published test vectors are essential for validation. The second risk is durable handle persistence complexity: 14+ validation conditions during reconnect, each addressing a specific attack vector, with Samba having had multiple bugs in this area.

## Key Findings

### Recommended Stack

The SMB3 stack is lean and leverages existing infrastructure. Only one new dependency (`aead/cmac`) is needed. Go stdlib provides AES-GCM encryption and SHA-512 hashing with hardware acceleration. AES-CCM encryption should be implemented internally using NIST SP 800-38C (~200 lines) rather than pulling in the entire pion/dtls library. The KDF (SP800-108) is a single HMAC-SHA256 call (~30 lines). SPNEGO/Kerberos reuses the existing `jcmturner/gokrb5/v8` library and `pkg/auth/kerberos/` provider.

**Core technologies:**
- **Go stdlib crypto (`crypto/aes`, `crypto/cipher`):** AES-128-GCM encryption (preferred cipher for 3.1.1) and SHA-512 preauth hashing — hardware-accelerated on amd64/arm64
- **Internal AES-CCM implementation:** For SMB 3.0/3.0.2 backward compatibility (~200 lines following NIST SP 800-38C) — avoids pulling pion/dtls dependency for a single algorithm
- **`github.com/aead/cmac`:** AES-128-CMAC signing for SMB 3.0+ (replaces HMAC-SHA256 used in SMB 2.x) — implements `hash.Hash` interface, zero transitive deps
- **Internal SP800-108 KDF:** Key derivation function (~30 lines) using `crypto/hmac` + `crypto/sha256` — too simple to justify external dependency
- **Existing `jcmturner/gokrb5/v8`:** Kerberos authentication shared with NFS adapter — no new dependency, just needs session key extraction

**Critical versions:**
- AES-128-GCM is the primary target (Windows 10/11 prefer this)
- AES-128-CCM is secondary (Windows 8/8.1 compatibility)
- AES-256 variants deferred (specialized compliance scenarios)

### Expected Features

SMB3 features split cleanly into protocol-level requirements (Windows 10/11 clients expect these) and advanced features (enterprise/HA scenarios that can be deferred).

**Must have (table stakes):**
- **SMB 3.0/3.0.2/3.1.1 dialect negotiation with negotiate contexts** — Windows 10+ clients advertise 3.1.1 as highest dialect
- **Preauth integrity hash chain (SHA-512)** — Mandatory for 3.1.1, protects negotiate/session-setup from MITM tampering
- **AES encryption (CCM/GCM) with transform header** — Per-session and per-share encryption control
- **SP800-108 KDF for key derivation** — All SMB3 keys (signing, encryption, decryption) derived from session key via KDF
- **AES-CMAC signing** — Replaces HMAC-SHA256 for SMB 3.x dialects
- **Kerberos session key extraction** — Current code creates Kerberos sessions without deriving keys; this blocks SMB3 signing/encryption for Kerberos auth
- **SMB3 Lease V2 with ParentLeaseKey** — Windows expects lease epoch tracking and directory lease support
- **Secure dialect validation IOCTL** — FSCTL_VALIDATE_NEGOTIATE_INFO for SMB 3.0/3.0.2 clients to verify negotiate wasn't tampered with

**Should have (competitive):**
- **Durable Handles V1/V2** — Connection resilience (client reconnects after brief network interruption without losing open files)
- **AES-GMAC signing** — Performance optimization for SMB 3.1.1 (faster than CMAC)
- **Directory leases** — SMB 3.0+ allows leases on directories for caching directory listings
- **Cross-protocol lease/delegation coordination** — SMB3 leases must coordinate with NFS delegations bidirectionally

**Defer (v2+):**
- **Compression (LZ77, LZNT1)** — Optional, complex, low ROI for initial release
- **Multichannel** — Requires session binding across multiple TCP connections; not needed for single-node deployment
- **Persistent handles (CA shares)** — Requires clustered storage and replicated handle state
- **RDMA Direct / QUIC transport** — Hardware-dependent or major new transport layer

### Architecture Approach

The SMB3 upgrade maps cleanly onto existing architecture with no fundamental changes. Encryption sits in the framing layer (`framing.go`) because the SMB2_TRANSFORM_HEADER wraps entire messages before they reach the dispatch layer. Signing becomes algorithm-aware (dispatch based on negotiated dialect). Key derivation happens once during session setup, storing concrete `Signer` and `Encryptor` objects on the session to avoid dialect checks in hot paths. Durable handle state persists in the control plane GORM store (not metadata store) because handles are session-level state, not file metadata.

**Major components:**
1. **`internal/adapter/smb/crypto/`** (NEW) — AES-CCM/GCM encryption, SP800-108 KDF, transform header parsing/building
2. **`internal/adapter/smb/signing/`** (REFACTOR) — Abstract `Signer` interface with HMAC-SHA256, AES-CMAC, and AES-GMAC implementations
3. **`internal/adapter/smb/framing.go`** (MODIFY) — Transform header detection, decrypt before dispatch, encrypt after signing
4. **`internal/adapter/smb/v2/handlers/negotiate.go`** (MODIFY) — Negotiate context parsing (preauth integrity, encryption capabilities), dialect selection up to 3.1.1
5. **`internal/adapter/smb/v2/handlers/session_setup.go`** (MODIFY) — Preauth hash update, KDF key derivation, Kerberos session key extraction
6. **`internal/adapter/smb/durable/`** (NEW) — Durable handle persistence in control plane store, reconnect validation, timeout management
7. **`pkg/controlplane/store/`** (EXTEND) — Add `DurableHandleStore` sub-interface for open state persistence

**Key patterns:**
- **Algorithm dispatch via dialect:** Select crypto algorithms at session creation time based on negotiated dialect, store concrete implementations on session (never check dialect in hot path)
- **Connection-level negotiated state:** Store dialect, cipher, preauth hash on Connection (not globally) since different connections can negotiate different dialects
- **Preauth hash as pipeline state:** Thread SHA-512 hash value through negotiate/session-setup, updating with raw wire bytes before parsing
- **Durable handles as CREATE context extension:** Parse DHnQ/DH2C contexts alongside existing create contexts using same infrastructure

### Critical Pitfalls

Top 5 pitfalls from research, ordered by impact:

1. **Preauth integrity hash computed over wrong bytes (SMB 3.1.1)** — Must hash exact wire bytes including SMB2 header, not parsed/reconstructed messages. A single byte difference causes all derived keys to be wrong, leading to signature verification failures that are silent and hard to diagnose. Prevention: store raw message bytes before parsing, validate against Microsoft's published test vectors.

2. **Key derivation label/context strings differ between 3.0 and 3.1.1** — SMB 3.0/3.0.2 uses constant ASCII strings as KDF context (e.g., "SmbSign\0"), but SMB 3.1.1 uses the 64-byte preauth hash value. Using wrong labels produces incorrect keys. Prevention: parameterize KDF on dialect, test with both 3.0 and 3.1.1 clients independently.

3. **Transform header vs plain SMB2 header confusion** — Encrypted messages use 0xFD protocol ID, plain messages use 0xFE. The framing layer must detect transform headers before parsing SMB2 headers. Signing must be skipped for encrypted messages (signature verification is embedded in AEAD). Prevention: check first 4 bytes, decrypt in framing layer, never sign AND encrypt.

4. **Signing algorithm change (HMAC-SHA256 → AES-CMAC)** — Existing code uses HMAC-SHA256 for all signing. SMB 3.x requires AES-128-CMAC. If server negotiates 3.x but continues using HMAC-SHA256, every signed message fails verification. Prevention: refactor signing into algorithm-aware interface, dispatch based on dialect.

5. **Durable Handle V2 reconnect validation complexity** — 14+ validation conditions during reconnect (CreateGuid match, lease key match, security context match, etc.), each addressing a specific attack vector. Missing any check creates security vulnerabilities. Samba has had multiple bugs here. Prevention: implement checks as sequential validation pipeline, test each condition independently with smbtorture.

Additional cross-protocol pitfall: **SMB3 directory lease breaks during NFS directory operations** — NFS CREATE/REMOVE/RENAME in a directory must break SMB3 directory leases (new in SMB3). Existing cross-protocol code only handles file-level leases. Prevention: add `CheckAndBreakForDirectoryChange` method, ensure epoch tracking for NFS-triggered breaks.

## Implications for Roadmap

Based on research, suggested phase structure follows dependency chains. Each phase builds on previous work and delivers testable functionality.

### Phase 1: SMB3 Dialect Negotiation & Preauth Integrity
**Rationale:** Everything depends on correct dialect negotiation. The preauth integrity hash chain is foundational for SMB 3.1.1 key derivation. This phase establishes the connection-level state needed by all subsequent phases.

**Delivers:**
- Dialect selection for 3.0/3.0.2/3.1.1
- Negotiate context parsing/encoding (preauth integrity, encryption capabilities, signing capabilities)
- SHA-512 preauth integrity hash chain on Connection and Session
- Updated capabilities advertisement (CapDirectoryLeasing, CapEncryption)

**Addresses:** Must-have features (negotiate contexts, preauth integrity)

**Avoids:** Pitfall 1 (preauth hash over wrong bytes), Pitfall 6 (negotiate context ordering/padding)

**Research flags:** Standard patterns. Microsoft spec (MS-SMB2) is comprehensive with detailed examples. Skip phase-specific research.

---

### Phase 2: SP800-108 KDF & AES-CMAC Signing
**Rationale:** All SMB3 cryptographic operations depend on correct key derivation. Signing must work before encryption can be tested (encryption key derivation shares the same KDF infrastructure). This phase is pure crypto implementation with published test vectors.

**Delivers:**
- SP800-108 KDF implementation (~30 lines)
- SMB 3.0/3.0.2 key derivation (constant label/context strings)
- SMB 3.1.1 key derivation (preauth hash as context)
- AES-CMAC signer implementation
- Signing algorithm abstraction (HMAC-SHA256 for 2.x, AES-CMAC for 3.x)
- Session key derivation for NTLM and Kerberos

**Uses:** Go stdlib (`crypto/hmac`, `crypto/sha256`), `github.com/aead/cmac`

**Avoids:** Pitfall 2 (wrong KDF labels), Pitfall 5 (signing algorithm), Pitfall 7 (key derivation timing)

**Research flags:** Standard patterns. NIST SP800-108 spec + Microsoft blog test vectors provide complete validation. Skip phase-specific research.

---

### Phase 3: AES Encryption & Transform Header
**Rationale:** Depends on KDF from Phase 2 for EncryptionKey/DecryptionKey. Depends on negotiate from Phase 1 for cipher selection. This phase implements the framing layer changes needed to wrap/unwrap encrypted messages.

**Delivers:**
- AES-128-GCM encryption/decryption (Go stdlib)
- AES-128-CCM encryption/decryption (internal implementation)
- Transform header parsing/building
- Framing layer integration (detect 0xFD, decrypt before dispatch, encrypt after signing)
- Per-session encryption (Session.EncryptData flag)
- Per-share encryption (Share.EncryptData flag)

**Implements:** Encryption framing layer from architecture

**Avoids:** Pitfall 3 (transform header confusion), Pitfall 4 (AES-256 key length), Pitfall 18 (per-share encryption validation)

**Research flags:** Moderate complexity. AES-CCM implementation needs careful validation against NIST test vectors. Consider `/gsd:research-phase` for CCM-specific details if implementation issues arise.

---

### Phase 4: SPNEGO/Kerberos SMB3 Integration
**Rationale:** Depends on key derivation (Phase 2) for deriving keys from Kerberos session key. Current Kerberos implementation is a critical gap: it creates sessions without signing keys, blocking SMB3 Kerberos authentication entirely.

**Delivers:**
- Kerberos session key extraction from AP-REQ validation
- SMB3 key derivation for Kerberos sessions
- AP-REP token generation for mutual authentication
- SPNEGO fallback (Kerberos → NTLM)

**Uses:** Existing `pkg/auth/kerberos/` provider, `gokrb5` library

**Avoids:** Pitfall 9 (multi-leg Kerberos, AP-REP, fallback)

**Research flags:** Standard patterns. `gokrb5` API for session key extraction is well-documented. Skip phase-specific research.

---

### Phase 5: SMB3 Leases & Directory Leasing
**Rationale:** The existing lease implementation (OplockManager) already supports V2 leases with epoch tracking. This phase is about advertising capabilities, wiring up ParentLeaseKey, and integrating directory lease breaks with metadata layer.

**Delivers:**
- Directory leasing capability advertisement
- ParentLeaseKey handling in CREATE
- Epoch validation in lease break acknowledgment
- Directory lease breaks on metadata changes (file create/delete/rename within directory)

**Addresses:** Must-have features (lease V2, directory leases)

**Avoids:** Pitfall 8 (lease V1/V2 response mismatch)

**Research flags:** Standard patterns. Existing lease code provides foundation. Skip phase-specific research.

---

### Phase 6: Durable Handles V1/V2
**Rationale:** Most complex feature. Depends on leases (Phase 5) because durable handles are tightly coupled with lease state. Depends on encryption (Phase 3) because persistent handles require encryption. The 14+ reconnect validation conditions and state persistence across disconnects make this the highest-complexity phase.

**Delivers:**
- Durable Handle V1 (DHnQ/DHnC create contexts)
- Durable Handle V2 (DH2Q/DH2C with CreateGuid for idempotent reconnection)
- DurableHandleStore persistence in control plane GORM store
- Reconnect validation (all 14+ conditions from MS-SMB2 spec)
- Timeout management and cleanup

**Implements:** Durable handle persistence from architecture

**Avoids:** Pitfall 10 (reconnect validation), Pitfall 11 (state persistence), Pitfall 17 (ClientGuid tracking)

**Research flags:** HIGH complexity. The reconnect validation is extremely detailed with subtle edge cases. Recommend `/gsd:research-phase` for this phase to analyze Samba's implementation and identify additional validation pitfalls not in MS-SMB2 spec.

---

### Phase 7: Secure Dialect Validation (IOCTL)
**Rationale:** Required for SMB 3.0/3.0.2 clients (Windows 8/8.1/Server 2012). Simple IOCTL handler that validates negotiate parameters. Can be implemented alongside other features or as standalone phase.

**Delivers:**
- FSCTL_VALIDATE_NEGOTIATE_INFO IOCTL handler
- Negotiate parameter validation (Capabilities, Guid, SecurityMode, Dialects)

**Avoids:** Pitfall 12 (missing IOCTL for 3.0.x clients)

**Research flags:** Standard patterns. MS-SMB2 spec provides complete IOCTL structure. Skip phase-specific research.

---

### Phase 8: Cross-Protocol Integration & Testing
**Rationale:** Final integration phase. Validates SMB3 features coordinate correctly with NFS delegations and byte-range locks. Runs conformance test suites (smbtorture, WPTS FileServer) and client compatibility testing.

**Delivers:**
- SMB3 lease ↔ NFS delegation bidirectional coordination
- Directory lease breaks on NFS directory operations
- Handle lease blocking for NFS delete operations
- smbtorture SMB3 tests (durable_v2, lease, replay, session, encryption)
- WPTS FileServer SMB3 BVT tests
- Windows 10/11/macOS/Linux client validation

**Addresses:** Cross-protocol integration features

**Avoids:** Pitfall 13 (directory lease breaks), Pitfall 14 (Handle lease blocking), Pitfall 15 (ACL consistency)

**Research flags:** Standard patterns for testing. Conformance suites are well-documented. Skip phase-specific research.

---

### Phase Ordering Rationale

- **Dependency-driven order:** KDF before encryption, negotiate before keys, leases before durable handles
- **Testability at each phase:** Each phase delivers independently testable functionality (can validate with Windows client or smbtorture subset)
- **Risk management:** Most complex feature (durable handles) comes last when all dependencies are solid
- **Incremental value:** Phases 1-5 already enable Windows 10/11 connectivity with encryption and signing. Durable handles (Phase 6) is a bonus feature for connection resilience.

### Research Flags

Phases likely needing deeper research during planning:
- **Phase 3 (AES Encryption):** AES-CCM implementation details — NIST SP 800-38C provides spec but edge cases (nonce generation, tag verification) may need additional research if issues arise during implementation
- **Phase 6 (Durable Handles):** Reconnect validation — 14+ conditions with subtle security implications. Samba implementation analysis recommended to identify pitfalls not explicit in MS-SMB2 spec

Phases with standard patterns (skip research-phase):
- **Phase 1 (Negotiate):** MS-SMB2 spec is comprehensive with detailed examples
- **Phase 2 (KDF/Signing):** NIST SP800-108 + Microsoft blog test vectors provide complete validation
- **Phase 4 (Kerberos):** `gokrb5` API is well-documented
- **Phase 5 (Leases):** Existing lease code provides foundation
- **Phase 7 (IOCTL):** Simple IOCTL handler, well-specified
- **Phase 8 (Testing):** Conformance suites are well-documented

## Confidence Assessment

| Area | Confidence | Notes |
|------|------------|-------|
| Stack | HIGH | Go stdlib covers most needs (AES-GCM, SHA-512). Only one new dependency (`aead/cmac`). AES-CCM can be implemented internally using NIST spec as reference. |
| Features | HIGH | MS-SMB2 spec is authoritative with complete feature definitions. Microsoft engineering blogs provide test vectors and implementation guidance. |
| Architecture | HIGH | DittoFS codebase analysis shows existing infrastructure (Kerberos provider, lease system, SPNEGO parsing) aligns perfectly with SMB3 requirements. |
| Pitfalls | HIGH for 1-7 | Verified against MS-SMB2 spec + Microsoft blog test vectors + DittoFS source code. MEDIUM for 8-12 (WebSearch + Samba bug reports). MEDIUM for 13-15 (cross-protocol coordination based on architecture analysis). |

**Overall confidence:** HIGH

The combination of authoritative Microsoft documentation (MS-SMB2 specification + engineering blog posts with test vectors), mature reference implementations (Samba, go-smb2 client), and well-positioned existing codebase gives high confidence that the upgrade is feasible and well-understood. The main uncertainty is durable handle state persistence complexity (14+ validation conditions), which is addressed by flagging Phase 6 for deeper research.

### Gaps to Address

Research identified two areas requiring validation during implementation:

- **AES-CCM nonce generation:** NIST SP 800-38C specifies nonce uniqueness requirements. MS-SMB2 specifies nonce structure (11 bytes from transform header Nonce field). The gap is the exact nonce generation strategy (counter-based vs random vs hybrid). Resolution: analyze go-smb2 client implementation and Samba ksmbd for production-tested approaches.

- **Durable handle timeout values:** MS-SMB2 specifies default timeout (60 seconds for V1, negotiable for V2) but does not provide guidance on optimal values for different network conditions. Resolution: follow Samba defaults initially, make configurable for tuning based on real-world testing.

- **Cross-protocol lease/delegation timing windows:** When NFS triggers SMB lease break, existing code uses async goroutines with timeout. The gap is whether timeout should be shorter for Handle leases (to prevent data loss) vs Read/Write leases (performance trade-off). Resolution: analyze NFS delegation recall timeout (already implemented) and align SMB lease break timeout for consistency.

## Sources

### Primary (HIGH confidence)

**Microsoft Official Documentation:**
- [MS-SMB2: Generating Cryptographic Keys](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/da4e579e-02ce-4e27-bbce-3fc816a3ff92) — SP800-108 KDF specification, L values, label/context strings
- [MS-SMB2: SMB2_PREAUTH_INTEGRITY_CAPABILITIES](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5a07bd66-4734-4af8-abcf-5a44ff7ee0e5) — Negotiate context structure, SHA-512 hash chain
- [MS-SMB2: SMB2_ENCRYPTION_CAPABILITIES](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/16693be7-2b27-4d3b-804b-f605bde5bcdd) — Cipher IDs (AES-128/256-CCM/GCM)
- [MS-SMB2: SMB2_TRANSFORM_HEADER](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/d6ce2327-a4c9-4793-be66-7b5bad2175fa) — Encryption framing, nonce formats
- [MS-SMB2: Encrypting the Message](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/24d74c0c-3de1-40d9-a949-d169ad84361d) — Encryption procedure
- [MS-SMB2: SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/5e361a29-81a7-4774-861d-f290ea53a00e) — Durable handle V2 create context
- [MS-SMB2: Handling DURABLE_HANDLE_RECONNECT_V2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/62ba68d0-8806-4aef-a229-eefb5827160f) — 14+ validation conditions

**Microsoft Engineering Blogs (with test vectors):**
- [SMB 2 and SMB 3 Security: Anatomy of Signing and Cryptographic Keys](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-2-and-smb-3-security-in-windows-10-the-anatomy-of-signing-and-cryptographic-keys) — Complete key derivation test vectors for 3.0 and 3.1.1
- [SMB 3.1.1 Pre-authentication Integrity in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-pre-authentication-integrity-in-windows-10) — Hash chain computation examples
- [SMB 3.1.1 Encryption in Windows 10](https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-3-1-1-encryption-in-windows-10) — Cipher negotiation examples
- [Encryption in SMB 3.0: A Protocol Perspective](https://learn.microsoft.com/en-us/archive/blogs/openspecification/encryption-in-smb-3-0-a-protocol-perspective) — Transform header details

**Standards:**
- [NIST SP800-108: Key Derivation Using Pseudorandom Functions](https://nvlpubs.nist.gov/nistpubs/Legacy/SP/nistspecialpublication800-108.pdf) — KDF Counter Mode specification
- [NIST SP800-38C: CCM Mode](https://nvlpubs.nist.gov/nistpubs/Legacy/SP/nistspecialpublication800-38c.pdf) — AES-CCM algorithm specification
- [RFC 4493: The AES-CMAC Algorithm](https://www.ietf.org/rfc/rfc4493.txt) — CMAC specification

**Go Standard Library:**
- [Go crypto/cipher package](https://pkg.go.dev/crypto/cipher) — AEAD interface, GCM support
- [Go crypto/sha512 package](https://pkg.go.dev/crypto/sha512) — SHA-512 implementation

### Secondary (MEDIUM confidence)

**Reference Implementations:**
- [hirochachacha/go-smb2 CCM implementation](https://github.com/hirochachacha/go-smb2/blob/master/internal/crypto/ccm/ccm.go) — Reference for AES-CCM (~200 lines, BSD-2 licensed)
- [hirochachacha/go-smb2 KDF implementation](https://github.com/hirochachacha/go-smb2/blob/master/kdf.go) — Confirms label strings and context values
- [pion/dtls CCM package](https://pkg.go.dev/github.com/pion/dtls/v2/pkg/crypto/ccm) — Alternative CCM reference (MIT licensed)
- [aead/cmac package](https://pkg.go.dev/github.com/aead/cmac) — CMAC library documentation

**Samba Experience:**
- [Implementing Persistent Handles in Samba (SNIA SDC 2018)](https://www.snia.org/sites/default/files/SDC/2018/presentations/SMB/Bohme_Ralph_Implementing_Persistent_Handles_Samba.pdf) — Durable handle implementation details
- [Samba Bug #14106: SPNEGO fallback](https://bugzilla.samba.org/show_bug.cgi?id=14106) — Kerberos → NTLM fallback issues
- [Samba's Way Toward SMB 3.0 (USENIX ;login:)](https://www.usenix.org/system/files/login/articles/03adam_016-025_online.pdf) — Implementation challenges

### Tertiary (HIGH confidence - DittoFS codebase analysis)

**Existing DittoFS Infrastructure:**
- `internal/adapter/smb/types/constants.go` — Dialect constants (3.0/3.0.2/3.1.1 already defined)
- `internal/adapter/smb/v2/handlers/lease.go` — V1/V2 lease decode/encode already implemented
- `pkg/auth/kerberos/provider.go` — Shared Kerberos provider with keytab/hot-reload
- `internal/adapter/smb/auth/spnego.go` — Full SPNEGO parsing (NTLM + Kerberos detection)
- `pkg/metadata/lock/oplock.go` — Unified Lock Manager with cross-protocol coordination
- `internal/adapter/smb/signing/signing.go` — HMAC-SHA256 implementation (needs refactor for CMAC)

---
*Research completed: 2026-02-28*
*Ready for roadmap: yes*
