# Requirements: DittoFS SMB3 Protocol Upgrade

**Defined:** 2026-02-28
**Core Value:** Enterprise-grade multi-protocol file access with unified locking, Kerberos authentication, and session reliability

## v3.8 Requirements

Requirements for SMB3 protocol upgrade. Each maps to roadmap phases.

### Negotiation

- [x] **NEG-01**: Server negotiates SMB 3.0/3.0.2/3.1.1 dialects selecting highest mutually supported
- [x] **NEG-02**: Server parses and responds with negotiate contexts (preauth integrity, encryption, signing capabilities)
- [x] **NEG-03**: Server advertises CapDirectoryLeasing and CapEncryption capabilities for 3.0+
- [x] **NEG-04**: Server computes SHA-512 preauth integrity hash chain over raw wire bytes on Connection and Session

### Key Derivation

- [x] **KDF-01**: Server derives signing/encryption/decryption/application keys via SP800-108 Counter Mode KDF
- [x] **KDF-02**: Server uses constant label/context strings for SMB 3.0/3.0.2 key derivation
- [x] **KDF-03**: Server uses preauth integrity hash as KDF context for SMB 3.1.1 key derivation
- [ ] **KDF-04**: Server extracts Kerberos session key from AP-REQ for SMB3 key derivation

### Signing

- [x] **SIGN-01**: Server signs messages with AES-128-CMAC for SMB 3.x sessions (replacing HMAC-SHA256)
- [x] **SIGN-02**: Server supports AES-128-GMAC signing for SMB 3.1.1 via signing capabilities negotiate context
- [x] **SIGN-03**: Signing algorithm abstraction dispatches by negotiated dialect (HMAC-SHA256 for 2.x, CMAC/GMAC for 3.x)

### Encryption

- [ ] **ENC-01**: Server encrypts/decrypts messages using AES-128-GCM with transform header framing
- [ ] **ENC-02**: Server encrypts/decrypts messages using AES-128-CCM for 3.0/3.0.2 compatibility
- [ ] **ENC-03**: Server supports AES-256-GCM and AES-256-CCM cipher variants
- [ ] **ENC-04**: Server enforces per-session encryption via Session.EncryptData flag
- [ ] **ENC-05**: Server enforces per-share encryption via Share.EncryptData configuration
- [ ] **ENC-06**: Framing layer detects transform header (0xFD) and decrypts before dispatch

### Authentication

- [ ] **AUTH-01**: Server completes SPNEGO/Kerberos session setup with session key extraction via shared Kerberos layer
- [ ] **AUTH-02**: Server generates AP-REP token for mutual authentication in SPNEGO accept-complete
- [ ] **AUTH-03**: Server falls back from Kerberos to NTLM within SPNEGO when Kerberos fails
- [ ] **AUTH-04**: Guest sessions bypass encryption and signing (no session key)

### Leases

- [ ] **LEASE-01**: Server grants Lease V2 with ParentLeaseKey and epoch tracking in CREATE responses
- [ ] **LEASE-02**: Server grants directory leases (Read-caching) for SMB 3.0+ clients
- [ ] **LEASE-03**: Server breaks directory leases when directory contents change (file create/delete/rename)
- [ ] **LEASE-04**: Lease management logic lives in metadata service layer, not SMB internal package

### Durable Handles

- [ ] **DH-01**: Server grants durable handles V1 (DHnQ) and reconnects via DHnC with timeout
- [ ] **DH-02**: Server grants durable handles V2 (DH2Q) with CreateGuid for idempotent reconnection
- [ ] **DH-03**: Durable handle state persists in control plane store surviving disconnects
- [ ] **DH-04**: Server validates all reconnect conditions (14+ checks per MS-SMB2 spec)
- [ ] **DH-05**: Durable handle management logic lives in metadata service layer, reusing NFSv4 state patterns

### Secure Dialect

- [x] **SDIAL-01**: Server handles FSCTL_VALIDATE_NEGOTIATE_INFO IOCTL for SMB 3.0/3.0.2 clients

### Cross-Protocol

- [ ] **XPROT-01**: SMB3 lease breaks coordinate bidirectionally with NFS delegations via Unified Lock Manager
- [ ] **XPROT-02**: NFS directory operations trigger SMB3 directory lease breaks
- [ ] **XPROT-03**: Cross-protocol coordination logic lives in metadata service (shared abstract layer)

### Architecture

- [ ] **ARCH-01**: Business logic (leases, durable handles, state) lives in metadata service layer following NFS v3/v4 pattern
- [x] **ARCH-02**: SMB internal package contains only protocol encoding/decoding/framing — no business logic
- [ ] **ARCH-03**: SMB3 features reuse NFSv4 infrastructure where possible (delegations, state management, Kerberos)

### Documentation

- [ ] **DOC-01**: Update docs/ with comprehensive SMB3 protocol documentation (configuration, capabilities, security)

### Testing

- [ ] **TEST-01**: smbtorture SMB3 tests pass (durable_v2, lease, replay, session, encryption suites)
- [ ] **TEST-02**: Microsoft WPTS FileServer SMB3 BVT tests pass
- [ ] **TEST-03**: Go integration tests (go-smb2) validate native client-server SMB3 interop
- [ ] **TEST-04**: Cross-protocol integration tests validate SMB3 leases vs NFS delegations
- [ ] **TEST-05**: Windows 10/11, macOS, and Linux client compatibility validated
- [ ] **TEST-06**: E2E tests for SMB3 encryption, signing, leases, Kerberos, and durable handle scenarios

## Future Requirements

Deferred to post-v3.8. Tracked but not in current roadmap.

### Compression

- **COMP-01**: SMB 3.1.1 message compression (LZ77, LZNT1, LZ77+Huffman, Pattern_V1)

### Multichannel

- **MULTI-01**: Multiple TCP connections per session with session binding and channel-specific signing keys

### Transport

- **TRANS-01**: SMB over QUIC (VPN-less remote file access)

## Out of Scope

Explicitly excluded. Documented to prevent scope creep.

| Feature | Reason |
|---------|--------|
| Compression (LZ77/LZNT1) | Optional, complex, low ROI for initial SMB3 release |
| Multichannel | Single-node architecture; requires session binding and interface discovery |
| Persistent handles (CA shares) | Requires clustered storage which DittoFS does not support |
| RDMA Direct | Hardware-dependent, kernel-level, irrelevant for userspace Go |
| QUIC transport | Major new transport layer, separate from protocol upgrade |
| SMB1 negotiate compatibility | SMB1 is deprecated, DittoFS never supported it |
| 8.3 short name generation | Low priority, only affects legacy apps |
| Full SACL enforcement | Requires audit infrastructure |
| Extended Attributes over SMB | Requires xattr metadata layer from v4.0 |

## Traceability

Which phases cover which requirements. Updated during roadmap creation.

| Requirement | Phase | Status |
|-------------|-------|--------|
| NEG-01 | Phase 33 | Complete |
| NEG-02 | Phase 33 | Complete |
| NEG-03 | Phase 33 | Complete |
| NEG-04 | Phase 33 | Complete |
| KDF-01 | Phase 34 | Complete |
| KDF-02 | Phase 34 | Complete |
| KDF-03 | Phase 34 | Complete |
| KDF-04 | Phase 36 | Pending |
| SIGN-01 | Phase 34 | Complete |
| SIGN-02 | Phase 34 | Complete |
| SIGN-03 | Phase 34 | Complete |
| ENC-01 | Phase 35 | Pending |
| ENC-02 | Phase 35 | Pending |
| ENC-03 | Phase 35 | Pending |
| ENC-04 | Phase 35 | Pending |
| ENC-05 | Phase 35 | Pending |
| ENC-06 | Phase 35 | Pending |
| AUTH-01 | Phase 36 | Pending |
| AUTH-02 | Phase 36 | Pending |
| AUTH-03 | Phase 36 | Pending |
| AUTH-04 | Phase 36 | Pending |
| LEASE-01 | Phase 37 | Pending |
| LEASE-02 | Phase 37 | Pending |
| LEASE-03 | Phase 37 | Pending |
| LEASE-04 | Phase 37 | Pending |
| DH-01 | Phase 38 | Pending |
| DH-02 | Phase 38 | Pending |
| DH-03 | Phase 38 | Pending |
| DH-04 | Phase 38 | Pending |
| DH-05 | Phase 38 | Pending |
| SDIAL-01 | Phase 33 | Complete |
| XPROT-01 | Phase 39 | Pending |
| XPROT-02 | Phase 39 | Pending |
| XPROT-03 | Phase 39 | Pending |
| ARCH-01 | Phase 37 | Pending |
| ARCH-02 | Phase 33 | Complete |
| ARCH-03 | Phase 36 | Pending |
| DOC-01 | Phase 39 | Pending |
| TEST-01 | Phase 40 | Pending |
| TEST-02 | Phase 40 | Pending |
| TEST-03 | Phase 40 | Pending |
| TEST-04 | Phase 40 | Pending |
| TEST-05 | Phase 40 | Pending |
| TEST-06 | Phase 40 | Pending |

**Coverage:**
- v3.8 requirements: 44 total
- Mapped to phases: 44
- Unmapped: 0

---
*Requirements defined: 2026-02-28*
*Last updated: 2026-02-28 after roadmap creation*
