# Security Model (Internals)

This document describes the threat model and internal mechanism design of DittoFS security
features. It is intended for contributors working on or auditing the security layer.

For operator/user-facing configuration — auth setup, TLS config, Kerberos keytab runbook, and
the production hardening checklist — see [`../guide/security.md`](../guide/security.md).

## Table of Contents

- [Threat model](#threat-model)
- [RPCSEC_GSS internals (NFS/Kerberos)](#rpcsec_gss-internals-nfskerberos)
- [SPNEGO mechanics (SMB/Kerberos)](#spnego-mechanics-smbkerberos)
- [SMB3 cipher and transform design](#smb3-cipher-and-transform-design)
  - [Encryption](#encryption)
  - [Signing](#signing)
  - [Preauth integrity (downgrade protection)](#preauth-integrity-downgrade-protection)
  - [Key derivation](#key-derivation)
  - [Guest sessions and NTLM fallback](#guest-sessions-and-ntlm-fallback)
- [Transport security comparison](#transport-security-comparison)
- [References](#references)

---

## Threat model

DittoFS is a userspace network filesystem server. The threat model covers the following attack
surfaces:

**In scope:**

- **Network eavesdropping:** an attacker on the same network segment reading plaintext RPC or
  SMB traffic. Mitigations: SMB3 in-protocol AEAD encryption (`encryption_mode: required`),
  NFS-over-TLS (RFC 9289), Kerberos krb5p privacy (`sec=krb5p`), VPN/IPsec.
- **Credential spoofing (NFS):** a client sending arbitrary UID/GID values in AUTH_UNIX.
  Mitigation: RPCSEC_GSS (Kerberos) replaces trust-based UID with cryptographic identity;
  root squash limits privilege escalation when AUTH_UNIX is unavoidable.
- **Session downgrade attacks (SMB):** a man-in-the-middle stripping security capabilities
  from the negotiate exchange. Mitigations: SMB 3.1.1 preauth integrity hash binds keys to the
  exact negotiate exchange; VALIDATE_NEGOTIATE_INFO on SMB 3.0/3.0.2; SMB signing rejection of
  tampered messages.
- **Message tampering:** an attacker modifying in-flight SMB or NFS messages.
  Mitigations: SMB signing (AES-CMAC/GMAC); AEAD authentication tag in SMB3 encryption;
  Kerberos GSS wrap tokens with checksum.
- **Control plane credential exposure:** admin credentials sent to the REST API in cleartext.
  Mitigation: loopback-only bind by default; native TLS when exposed to the network.
- **Keytab/machine-secret exfiltration:** unauthorized read of the keytab or machine account
  secret. Mitigation: filesystem permissions (`chmod 600`); API write-only redaction for
  machine secret.

**Out of scope (no current mitigation):**

- Encryption at rest (S3 server-side encryption can be enabled independently)
- Audit logging for file operations
- Denial-of-service attacks
- Side-channel attacks against cryptographic primitives

---

## RPCSEC_GSS internals (NFS/Kerberos)

DittoFS implements RPCSEC_GSS per RFC 2203 in `pkg/auth/kerberos` and the NFS protocol layer.
The shared Kerberos layer also serves the SMB SPNEGO path (see below).

**Security context establishment:**

1. Client sends a `NULL` procedure call with a `RPCSEC_GSS_INIT` credential containing a GSS
   token (Kerberos AP-REQ).
2. Server calls `gss_accept_sec_context` against the AP-REQ using the keytab. The keytab is
   hot-loaded and re-read on change — new contexts after a keytab rotation use the new key
   material immediately.
3. Server returns an `RPCSEC_GSS_INIT` reply with a GSS token (AP-REP) proving its identity to
   the client (mutual authentication). The context handle is stored per-connection.
4. Subsequent RPCs carry the context handle; the server maps the established principal to
   UID/GID and builds an `*metadata.AuthContext` for the operation.

**Security services:** DittoFS negotiates the GSS service requested by the client mount:

- `krb5` — authentication only; data is plaintext.
- `krb5i` — authentication + per-message integrity (GSS MIC token on each RPC).
- `krb5p` — authentication + per-message privacy (GSS wrap with encryption + integrity).

**Clock skew:** the maximum allowed difference between client and server clocks is configurable
(`max_clock_skew`, default `5m`). Kerberos tickets carry a validity window; requests outside the
window are rejected.

**Context TTL:** `context_ttl` (default `8h`) is the maximum lifetime of an RPCSEC_GSS security
context. After expiry the client must re-authenticate.

---

## SPNEGO mechanics (SMB/Kerberos)

SMB Kerberos authentication follows the SPNEGO protocol (RFC 4178) during `SESSION_SETUP`.

**Negotiation flow:**

1. Server sends a `NEGOTIATE` response with a SPNEGO `NegTokenInit` advertising both the
   Kerberos OID (`1.2.840.113554.1.2.2`) and NTLM as supported mechanisms.
2. A client with a valid TGT obtains a service ticket for the `cifs/` SPN and sends an
   `AP-REQ` inside a SPNEGO `InitialContextToken`.
3. Server validates the `AP-REQ` against the shared keytab (same keytab as the NFS adapter).
4. Server sends an `AP-REP` (`NegTokenResp` with `accept-complete`) proving its identity to the
   client — **mutual authentication**. NTLM does not provide this property.
5. The session key from the Kerberos ticket is used as input to the SP800-108 KDF to derive
   SMB3 per-purpose keys (see [Key derivation](#key-derivation)).

**Principal mapping:** the SMB adapter automatically derives the `cifs/` SPN from the configured
`nfs/` service principal. The client Kerberos principal (without realm) is mapped to a DittoFS
control plane user for authorization.

**NTLM fallback:** when Kerberos is unavailable, the server falls back to NTLMv2. For local
DittoFS users, the NTLMv2 response is validated against the stored NT hash. For AD domain users,
the challenge/response is forwarded to the DC over a sealed NETLOGON secure channel (MS-NRPC
sign+seal AES, post-ZeroLogon); the DC-returned SID is resolved through the LDAP/idmap pipeline,
yielding the same UID/GID as Kerberos.

---

## SMB3 cipher and transform design

### Encryption

SMB3 provides encryption using AEAD (Authenticated Encryption with Associated Data) ciphers,
delivering both confidentiality and integrity for all messages on an encrypted session. The
encryption is in-protocol — not TLS or QUIC.

**Wire format:** each encrypted SMB message is wrapped in a `TRANSFORM_HEADER`:

```
Signature      [16]byte  — AEAD authentication tag
Nonce          [16]byte  — 128-bit or 96-bit, depending on cipher
OriginalMsgSz  uint32    — size of the plaintext
Reserved       uint16
Flags          uint16    — 0x0001 = encrypted
SessionID      uint64
```

The `Flags` + `SessionID` of the transform header are the AEAD additional data. This binds the
ciphertext to the session and prevents cross-session replay.

**Cipher selection:** for SMB 3.1.1, the cipher is negotiated via the
`SMB2_ENCRYPTION_CAPABILITIES` negotiate context. DittoFS prefers AES-128-GCM (fastest, hardware
accelerated) but accepts AES-256-GCM, AES-128-CCM, and AES-256-CCM in that order. For SMB
3.0/3.0.2, AES-128-CCM is mandatory.

**Encryption modes:**

| Mode | Behavior |
|------|----------|
| `disabled` | No encryption; startup WARN if not on loopback |
| `preferred` (default) | SMB 3.x sessions encrypted; SMB 2.x accepted |
| `required` | Only encrypted SMB 3.x clients accepted; SMB 2.x rejected at NEGOTIATE |

**Per-share enforcement:** `checkEncryptionRequired` rejects unencrypted requests on a share
configured with `encrypt_data`, returning `STATUS_ACCESS_DENIED`, regardless of the global mode.
`SMB2_SESSION_FLAG_ENCRYPT_DATA` is set at `SESSION_SETUP` in `required` mode; in `preferred`
mode AEAD keys are derived but the flag is not set.

### Signing

SMB3 introduces AES-based signing algorithms:

| Algorithm | Dialect | Notes |
|-----------|---------|-------|
| HMAC-SHA256 | SMB 2.x | Uses session key directly; legacy |
| AES-128-CMAC | SMB 3.0+ | Uses SP800-108 derived `SigningKey` |
| AES-128-GMAC | SMB 3.1.1 | Preferred; leverages GCM hardware |

**Algorithm negotiation (3.1.1):** the `SMB2_SIGNING_CAPABILITIES` negotiate context lets client
and server agree on a signing algorithm. DittoFS prefers GMAC > CMAC. Clients omitting the
signing capability context default to AES-128-CMAC.

**Signed-when-encrypted:** after `SESSION_SETUP` completes (non-guest sessions), all messages
are signed. Signing is redundant when AEAD encryption is active, but DittoFS signs encrypted
messages to match Windows Server behavior and maintain broad client compatibility.

### Preauth integrity (downgrade protection)

SMB 3.1.1 maintains a running SHA-512 hash (the "preauth integrity hash") over the raw bytes of
every `NEGOTIATE` and `SESSION_SETUP` message:

```
PreauthHash = SHA-512(PreauthHash || message_bytes)
```

This hash is used as the **KDF context** when deriving per-session keys. Any MITM modification
of negotiate messages (e.g., stripping encryption or signing capabilities) produces a different
hash, which produces different derived keys, causing `SESSION_SETUP` to fail with a MAC mismatch.

For SMB 3.0/3.0.2 (which predate preauth integrity), `FSCTL_VALIDATE_NEGOTIATE_INFO` provides
downgrade detection: the client sends its original negotiate parameters, and if the server's
stored state differs, the connection is dropped.

### Key derivation

SMB3 derives per-purpose keys from the session key using NIST SP800-108 Counter Mode KDF with
HMAC-SHA256 (`pkg/smb/crypto`):

| Key | Purpose |
|-----|---------|
| `SigningKey` | AES-CMAC/GMAC message signing |
| `EncryptionKey` | Server-to-client AES-GCM/CCM encryption |
| `DecryptionKey` | Client-to-server AES-GCM/CCM decryption |
| `ApplicationKey` | Application-level cryptographic operations |

**Dialect-specific label/context strings:**

- **SMB 3.0/3.0.2:** fixed label strings (e.g., `"SmbSign\0"`, `"ServerIn \0"`).
- **SMB 3.1.1:** the preauth integrity hash is used as the KDF context, binding all four derived
  keys to the exact negotiate/session-setup exchange.

The session key itself is the Kerberos ticket session key (when using Kerberos/SPNEGO), or the
NTLMv2 session key (when falling back to NTLM).

### Guest sessions and NTLM fallback

**Guest sessions:** no session key is available, so no signing and no encryption are possible.
`SMB2_SESSION_FLAG_IS_GUEST` is set. In `required` encryption mode, a guest session is rejected
rather than accepted unencrypted. Guest sessions should be restricted to read-only public shares.

**NTLM session key weakness:** the NTLMv2 session key is derived from the NT password hash,
which is weaker than a Kerberos session key. Signing uses HMAC-SHA256 rather than AES-CMAC/GMAC.
NTLM does not provide mutual authentication, making the client unable to verify server identity
and leaving the connection vulnerable to relay attacks without SMB channel binding.

---

## Transport security comparison

| Property | SMB3 encryption | NFS Kerberos (krb5p) |
|----------|-----------------|---------------------|
| Confidentiality | AES-128-GCM / AES-128-CCM | AES-256 wrap (RFC 3962) |
| Integrity | AEAD tag (built into cipher) | Kerberos checksum |
| Key derivation | SP800-108 from session key | Kerberos sub-session key |
| Per-message overhead | 52 bytes (transform header) | ~28 bytes (GSS wrap token) |
| Downgrade protection | Preauth integrity hash (3.1.1) | GSS-API mechanism negotiation |
| Mutual auth | Via Kerberos AP-REP | Built into RPCSEC_GSS |
| Hardware acceleration | AES-NI + CLMUL (GCM) | Depends on krb5 library |

Both protocols provide strong transport security when properly configured. SMB3 encryption is
built into the protocol (no external infrastructure required beyond the DittoFS config). NFS
krb5p requires a functioning Kerberos infrastructure but provides the same security properties.

---

## References

- [RFC 2203 — RPCSEC_GSS Protocol Specification](https://www.rfc-editor.org/rfc/rfc2203)
- [RFC 2743 — Generic Security Service API (GSS-API)](https://www.rfc-editor.org/rfc/rfc2743)
- [RFC 4120 — The Kerberos Network Authentication Service (V5)](https://www.rfc-editor.org/rfc/rfc4120)
- [RFC 4121 — The Kerberos Version 5 GSS-API Mechanism](https://www.rfc-editor.org/rfc/rfc4121)
- [RFC 4178 — SPNEGO (GSS-API Negotiation Mechanism)](https://www.rfc-editor.org/rfc/rfc4178)
- [RFC 7530 — NFS Version 4 Protocol](https://www.rfc-editor.org/rfc/rfc7530)
- [RFC 9289 — NFS-over-TLS](https://www.rfc-editor.org/rfc/rfc9289)
- [MS-SMB2 — Server Message Block Protocol Versions 2 and 3](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/)
- [MS-NLMP — NTLM Authentication Protocol](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-nlmp/)
- [MS-NRPC — Netlogon Remote Protocol](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-nrpc/)
- [MS-DTYP — SID, ACL, and security descriptor formats](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-dtyp/)
- [NIST SP800-108 — KDF in Counter Mode](https://csrc.nist.gov/publications/detail/sp/800-108/rev-1/final)
- [Glossary](../guide/glossary.md) — plain-language definitions of terms above
