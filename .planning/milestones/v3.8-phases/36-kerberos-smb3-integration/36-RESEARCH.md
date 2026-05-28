# Phase 36: Kerberos SMB3 Integration - Research

**Researched:** 2026-03-02
**Domain:** Kerberos/SPNEGO authentication for SMB3, shared Kerberos service layer, NFS GSS migration
**Confidence:** HIGH

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Feed Kerberos session key into the existing `configureSessionSigningWithKey()` pipeline (same path as NTLM)
- Truncate/pad session key to 16 bytes regardless of Kerberos encryption type (AES128=16B, AES256=32B, DES=8B) -- standard SMB2 key normalization
- Prefer authenticator subkey over ticket session key per MS-SMB2 3.3.5.5.3
- Reuse Phase 35's supported encryption algorithm list for validation -- no separate config
- Handle mixed encryption types (authenticator subkey vs ticket) transparently via gokrb5
- Store Kerberos-derived keys identically to NTLM (via `sess.SetCryptoState()` / `sess.SetSigningKey()`) -- add metadata field for auth mechanism in logging only
- Auto-derive CIFS SPN from NFS principal (`nfs/host@REALM` -> `cifs/host@REALM`) with optional `smb_service_principal` config override
- Single keytab file containing both `nfs/` and `cifs/` service principal entries -- document this requirement
- Always generate a real AP-REP token (not nil) and include it in the SPNEGO accept-complete response
- Match the client's Kerberos OID in the SPNEGO response (MS OID if client used MS OID, standard OID if client used standard)
- Include SPNEGO mechListMIC for downgrade protection -- bidirectional: verify client MIC if present, then generate server MIC
- Use gokrb5's AP-REP builder if available; fall back to manual construction if server-side API is incomplete
- Trust gokrb5 for ticket lifetime validation -- no additional near-expiry checks
- Implement replay cache for Kerberos authenticators in the shared service (`internal/auth/kerberos/`)
- On Kerberos failure: return SPNEGO NegState=reject; client retries with new SESSION_SETUP (fresh session ID) using NTLM
- NTLM enable/disable configurable as adapter-level setting in the control plane (not global config file)
- When Kerberos provider is not configured (no keytab): only advertise NTLM in NEGOTIATE response SPNEGO hints
- Kerberos-only mechList failure: return STATUS_LOGON_FAILURE directly (no SPNEGO wrapping)
- Fresh SESSION_SETUP on fallback (SessionId=0) -- clean state, no stale Kerberos context leaking into NTLM
- Log failed Kerberos attempts at INFO level with context (principal, error reason, client IP)
- Adapter-level `guest_enabled: true/false` config in control plane (default: true)
- Valid Kerberos ticket but unknown principal (no matching DittoFS user): hard failure (STATUS_LOGON_FAILURE)
- Expired Kerberos ticket: hard failure (STATUS_LOGON_FAILURE) -- client must renew with KDC
- Guest access controlled by share permissions -- guest identity checked against share permission model
- Reject guest sessions when server requires signing (`signing.required: true`) -- can't sign without session key
- Anonymous (empty security buffer) and failed-auth both treated as guest -- same `SMB2_SESSION_FLAG_IS_GUEST` behavior
- Log INFO hint about Windows 11 24H2 insecure guest logon policy when guest session is created
- **`pkg/auth/kerberos/`** -- Provider (keytab, config, SPN) + Kerberos identity mapping (principal-to-user resolution with strip-realm default + configurable explicit mapping table)
- **`internal/auth/kerberos/`** -- NEW shared KerberosService (broader service object pattern): `Authenticate(apReq) -> AuthResult`, `BuildMutualAuth(creds) -> apRep`, built-in replay cache, session key extraction with subkey preference and size normalization
- **`internal/adapter/nfs/rpc/gss/`** -- Refactored to call `internal/auth/kerberos/` for AP-REQ verification and key extraction; keeps NFS-specific RPCSEC_GSS wiring (MIC, integrity, privacy, context store, sequence window)
- **`internal/adapter/smb/v2/handlers/kerberos_auth.go`** -- NEW dedicated file for SMB Kerberos auth (extracted from session_setup.go); calls shared KerberosService, handles SPNEGO wrapping
- **`internal/adapter/smb/auth/spnego.go`** -- Extended with MIC computation helpers (GetMIC/VerifyMIC)
- **`internal/auth/kerberos/replay.go`** -- Replay cache (in-memory, TTL-based, keyed by principal+ctime+cusec)
- Migrate NFS GSS code to use shared KerberosService in this phase (not deferred)

### Claude's Discretion
- Whether to fail auth entirely or succeed without AP-REP when AP-REP generation fails
- Server subkey in AP-REP: follow Samba's behavior (likely no server subkey)
- Handling of 3-leg SPNEGO continuation tokens from clients
- Mixed encryption type handling details (delegated to gokrb5 transparently)
- Credential delegation: skip for now (niche scenario)
- Per-client auth failure rate limiting: defer (existing server-level rate limiter covers this)

### Deferred Ideas (OUT OF SCOPE)
- Prometheus metrics for auth mechanism counters (smb_auth_kerberos_total, smb_auth_ntlm_total, etc.) -- no metrics infrastructure yet
- Kerberos credential delegation (forwarded TGT, constrained delegation, S4U2Proxy) -- niche scenario, future phase
- Per-client auth failure rate limiting -- existing server-level rate limiter covers this for now
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| AUTH-01 | Server completes SPNEGO/Kerberos session setup with session key extraction via shared Kerberos layer | Shared KerberosService in `internal/auth/kerberos/` with `Authenticate()` method; session key extraction with subkey preference; 16-byte normalization for SMB3 KDF |
| AUTH-02 | Server generates AP-REP token for mutual authentication in SPNEGO accept-complete | AP-REP construction via `BuildMutualAuth()` using existing `buildAPRep()` pattern from NFS GSS; GSS-API wrapped token with OID matching |
| AUTH-03 | Server falls back from Kerberos to NTLM within SPNEGO when Kerberos fails | SPNEGO reject response on Kerberos failure; client retries with fresh SESSION_SETUP; NTLM enable/disable in control plane settings; conditional NEGOTIATE hints |
| AUTH-04 | Guest sessions bypass encryption and signing (no session key) | Guest sessions set `SMB2_SESSION_FLAG_IS_GUEST`; reject when `signing.required: true`; adapter-level `guest_enabled` config; no `SetCryptoState()` call for guests |
| KDF-04 | Server extracts Kerberos session key from AP-REQ for SMB3 key derivation | Session key from `apReq.Ticket.DecryptedEncPart.Key` or authenticator subkey; truncate/pad to 16 bytes; feed into `configureSessionSigningWithKey()` |
| ARCH-03 | SMB3 features reuse NFSv4 infrastructure where possible (delegations, state management, Kerberos) | Shared KerberosService used by both NFS GSS and SMB handlers; shared Provider for keytab management; NFS GSS refactored to call shared service |
</phase_requirements>

## Summary

Phase 36 creates a shared Kerberos authentication service layer and integrates it into the SMB SESSION_SETUP handler to enable domain-joined Windows clients to authenticate via SPNEGO/Kerberos. The phase also migrates the existing NFS RPCSEC_GSS code to use the shared service, establishing a unified Kerberos infrastructure across both protocols.

The codebase is well-prepared for this work. The NFS GSS implementation in `internal/adapter/nfs/rpc/gss/framework.go` already contains mature AP-REQ verification, AP-REP construction, authenticator subkey handling, and session key extraction. The SMB handler in `session_setup.go` already has a working `handleKerberosAuth()` skeleton that validates AP-REQ and maps principals -- it just needs session key extraction, AP-REP generation, and signing key derivation. The shared Kerberos Provider in `pkg/auth/kerberos/provider.go` already provides keytab management and hot-reload. The primary work is: (1) extracting the core Kerberos AP-REQ verification and AP-REP construction from the NFS GSS code into a shared service, (2) wiring it into the SMB handler with session key normalization and KDF integration, (3) implementing proper SPNEGO negotiation with NTLM fallback and MIC support, and (4) adding control plane settings for NTLM/guest configuration.

**Primary recommendation:** Create `internal/auth/kerberos/` as the shared service layer, extracting verification logic from `internal/adapter/nfs/rpc/gss/framework.go`, then wire it into both adapters.

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| gokrb5/v8 | v8.4.4 | Kerberos protocol (AP-REQ verify, AP-REP build, crypto) | Already used throughout project; provides `service.VerifyAPREQ()`, `messages.APReq`, `crypto.GetEncryptedData()` |
| gokrb5/v8/spnego | v8.4.4 | SPNEGO NegTokenInit/Resp marshal/unmarshal | Already used; `spnego.UnmarshalNegToken()`, `NegTokenResp.Marshal()` |
| gofork/encoding/asn1 | (gokrb5 dep) | ASN.1 encoding for SPNEGO and Kerberos tokens | Already used; gokrb5's ASN.1 fork with proper tagging |
| gokrb5/v8/asn1tools | v8.4.4 | ASN.1 APPLICATION tag wrapping | Already used; `asn1tools.AddASNAppTag()` for AP-REP construction |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| gokrb5/v8/gssapi | v8.4.4 | MIC token computation | MechListMIC for SPNEGO downgrade protection |
| gokrb5/v8/crypto | v8.4.4 | Encryption for AP-REP EncAPRepPart | `crypto.GetEncryptedData()` with key usage 12 |
| gokrb5/v8/service | v8.4.4 | AP-REQ verification and replay cache | `service.VerifyAPREQ()`, `service.GetReplayCache()` |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| gokrb5 built-in replay cache | Custom replay cache | gokrb5's `service.Cache` is a singleton with global state; custom cache gives per-service isolation and explicit TTL control. **Recommendation: use custom replay cache** in `internal/auth/kerberos/replay.go` as specified by user |
| Manual AP-REP construction | gokrb5 higher-level API | gokrb5 doesn't provide a server-side AP-REP builder; manual construction is necessary (already proven in NFS GSS code) |

## Architecture Patterns

### Recommended Project Structure
```
internal/auth/kerberos/          # NEW - shared Kerberos service layer
├── service.go                   # KerberosService: Authenticate(), BuildMutualAuth(), ExtractSessionKey()
├── replay.go                    # ReplayCache: in-memory, TTL-based, keyed by principal+ctime+cusec
├── replay_test.go
├── service_test.go
└── doc.go

pkg/auth/kerberos/               # EXISTING - enhanced
├── provider.go                  # Provider: keytab, config, SPN (unchanged)
├── keytab.go                    # KeytabManager: hot-reload (unchanged)
├── identity.go                  # NEW: principal-to-user resolution
└── doc.go                       # Updated doc

internal/adapter/smb/v2/handlers/
├── kerberos_auth.go             # NEW: extracted from session_setup.go
├── session_setup.go             # Modified: delegates Kerberos to kerberos_auth.go
└── ...

internal/adapter/smb/auth/
├── spnego.go                    # MODIFIED: add MIC helpers (GetMIC/VerifyMIC), BuildNegHints()
└── ...

internal/adapter/nfs/rpc/gss/
├── framework.go                 # MODIFIED: Krb5Verifier delegates to shared KerberosService
└── ...
```

### Pattern 1: Shared KerberosService
**What:** A service object in `internal/auth/kerberos/` that encapsulates AP-REQ verification, session key extraction (with subkey preference), AP-REP construction, and replay detection.
**When to use:** Both NFS GSS and SMB session setup need the same core Kerberos operations.
**Example:**
```go
// internal/auth/kerberos/service.go
type AuthResult struct {
    Principal    string
    Realm        string
    SessionKey   types.EncryptionKey  // Subkey preferred over ticket key
    APRepToken   []byte               // GSS-API wrapped AP-REP
    APReq        *messages.APReq      // Needed by caller for context
}

type KerberosService struct {
    provider    *kerberos.Provider
    replayCache *ReplayCache
}

func (s *KerberosService) Authenticate(apReqBytes []byte, servicePrincipal string) (*AuthResult, error) {
    // 1. Unmarshal AP-REQ
    // 2. Build service.Settings with provider's keytab
    // 3. VerifyAPREQ
    // 4. Check replay cache
    // 5. Decrypt authenticator, extract subkey/session key
    // 6. Return AuthResult with preferred key
}

func (s *KerberosService) BuildMutualAuth(apReq *messages.APReq, sessionKey types.EncryptionKey) ([]byte, error) {
    // 1. Build EncAPRepPart with ctime/cusec from authenticator
    // 2. Include subkey if present
    // 3. Encrypt with session key (key usage 12)
    // 4. Wrap in GSS-API token with KRB5 OID + AP-REP token ID
    // 5. Return GSS-wrapped AP-REP bytes
}
```

### Pattern 2: Session Key Normalization for SMB3
**What:** Kerberos session keys vary in size (8-32 bytes). SMB3 KDF expects exactly 16 bytes. Normalize by truncating (>16) or zero-padding (<16).
**When to use:** After extracting the session key from Kerberos, before feeding into `configureSessionSigningWithKey()`.
**Example:**
```go
// internal/adapter/smb/v2/handlers/kerberos_auth.go
func normalizeSessionKey(key []byte) []byte {
    normalized := make([]byte, 16)
    copy(normalized, key) // Truncates if >16, zero-pads if <16
    return normalized
}
```
This matches [MS-SMB2] Section 3.3.5.5.3 and Samba's behavior (`cli_session_setup_kerberos()` in `source3/libsmb/cliconnect.c`).

### Pattern 3: SPNEGO Negotiate Hints
**What:** The NEGOTIATE response SecurityBuffer should contain a SPNEGO NegTokenInit2 (NegHints) advertising available mechanisms. When Kerberos is configured, include both Kerberos OIDs and NTLM; when not configured, only NTLM.
**When to use:** During NEGOTIATE response building, when the negotiated dialect supports SPNEGO.
**Example:**
```go
// internal/adapter/smb/auth/spnego.go
func BuildNegHints(kerberosEnabled, ntlmEnabled bool) ([]byte, error) {
    var mechTypes []asn1.ObjectIdentifier
    if kerberosEnabled {
        mechTypes = append(mechTypes, OIDMSKerberosV5, OIDKerberosV5)
    }
    if ntlmEnabled {
        mechTypes = append(mechTypes, OIDNTLMSSP)
    }
    init := spnego.NegTokenInit{MechTypes: mechTypes}
    return init.Marshal() // Returns NegTokenInit with just MechTypes (no token)
}
```

### Pattern 4: Kerberos OID Matching in SPNEGO Response
**What:** When building the SPNEGO accept-complete response, use the same Kerberos OID that the client used in its NegTokenInit.
**When to use:** In the Kerberos auth handler, after successful verification.
**Example:**
```go
// Track which Kerberos OID the client used
var clientKerberosOID asn1.ObjectIdentifier
if parsed.HasMechanism(auth.OIDMSKerberosV5) {
    clientKerberosOID = auth.OIDMSKerberosV5
} else {
    clientKerberosOID = auth.OIDKerberosV5
}
// Use same OID in response
spnegoResp, _ := auth.BuildAcceptComplete(clientKerberosOID, apRepToken)
```

### Pattern 5: Control Plane Settings Propagation
**What:** Add `NtlmEnabled` and `GuestEnabled` fields to `SMBAdapterSettings` model, with live reload via the existing settings watcher.
**When to use:** NTLM and guest access are adapter-level policy decisions managed at runtime via dfsctl.
**Example:**
```go
// pkg/controlplane/models/adapter_settings.go
type SMBAdapterSettings struct {
    // ... existing fields ...
    NtlmEnabled  bool `gorm:"default:true" json:"ntlm_enabled"`
    GuestEnabled bool `gorm:"default:true" json:"guest_enabled"`
    SMBServicePrincipal string `gorm:"size:256" json:"smb_service_principal"` // Override for auto-derived CIFS SPN
}
```

### Anti-Patterns to Avoid
- **Duplicating AP-REQ verification logic in SMB handler:** Both NFS and SMB need the same verification. Extract to shared service.
- **Coupling RPCSEC_GSS wiring to the shared service:** The shared service handles ONLY Kerberos-level operations (AP-REQ verify, AP-REP build). NFS-specific RPCSEC_GSS framing (context store, sequence window, MIC, integrity/privacy wrapping) stays in `gss/`.
- **Using gokrb5's singleton replay cache for SMB:** The `service.GetReplayCache()` is a process-global singleton. Since `service.VerifyAPREQ()` already calls it, we get basic replay protection. For the shared service, add an additional explicit cache for cross-protocol replay detection.
- **Modifying session key bytes in place:** Always copy keys before normalization -- never modify the original `EncryptionKey.KeyValue` slice.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| AP-REQ ticket decryption | Custom decryption | `service.VerifyAPREQ()` + `apReq.DecryptAuthenticator()` | Handles all encryption types, clock skew, replay detection |
| SPNEGO token parsing | Custom ASN.1 parser | `spnego.UnmarshalNegToken()` | Handles NegTokenInit/Resp, all tag formats |
| AP-REP EncAPRepPart encryption | Custom cipher | `crypto.GetEncryptedData(data, key, 12, 0)` | Key usage 12 for AP-REP, handles all etypes |
| MIC token computation | Custom checksum | `gssapi.MICToken.SetChecksum()` | RFC 4121 compliant, handles all Kerberos etypes |
| GSS-API token wrapping | Custom builder | Reuse `wrapGSSToken()` from existing GSS code | Already handles OID encoding, length encoding, token IDs |
| Session key derivation (SMB3) | Custom KDF | Existing `session.DeriveAllKeys()` + `configureSessionSigningWithKey()` | Already implements SP800-108 with dialect-specific labels |

**Key insight:** The NFS GSS implementation already solved the hard Kerberos problems (AP-REQ verification, AP-REP construction, authenticator decryption, subkey handling). The work here is extraction and integration, not greenfield development.

## Common Pitfalls

### Pitfall 1: Session Key Size Mismatch
**What goes wrong:** Kerberos AES-256 keys are 32 bytes but SMB3 KDF expects 16-byte input. Passing 32 bytes produces incorrect derived keys.
**Why it happens:** Different Kerberos encryption types produce different key sizes (DES=8, AES128=16, AES256=32).
**How to avoid:** Always normalize to exactly 16 bytes: `copy(normalized[:16], key)`. This matches [MS-SMB2] 3.3.5.5.3 and Samba's behavior.
**Warning signs:** Windows client reports STATUS_LOGON_FAILURE after Kerberos auth apparently succeeds; signing verification fails on first signed message.

### Pitfall 2: Wrong Kerberos OID in SPNEGO Response
**What goes wrong:** Client uses MS Kerberos OID (1.2.840.48018.1.2.2) but server responds with standard OID (1.2.840.113554.1.2.2). Windows SSPI may reject the response.
**Why it happens:** Windows uses the MS Kerberos OID by default; other clients use the standard OID. Server must match.
**How to avoid:** Track which OID the client used in the NegTokenInit and echo it back in the NegTokenResp's SupportedMech field.
**Warning signs:** Windows client shows "Authentication mechanism negotiation failure" error.

### Pitfall 3: Missing AP-REP in SPNEGO Accept-Complete
**What goes wrong:** Current code sends `nil` AP-REP token. Domain-joined Windows clients expect a real AP-REP for mutual authentication and may warn or fail.
**Why it happens:** The existing skeleton was a minimal implementation that passed `nil` to `BuildAcceptComplete()`.
**How to avoid:** Always build a real AP-REP using `buildAPRep()` (proven in NFS GSS code) and include it as the ResponseToken in the SPNEGO NegTokenResp.
**Warning signs:** Windows Event Viewer shows Kerberos mutual authentication warnings; some Windows security policies reject non-mutual Kerberos.

### Pitfall 4: SPNEGO MechListMIC Computation Scope
**What goes wrong:** MIC is computed over wrong data. Per RFC 4178, the MIC protects the original mechList from the NegTokenInit (the DER-encoded sequence of OIDs), not the entire token.
**Why it happens:** Confusion about what data the MIC covers.
**How to avoid:** When computing server MIC: serialize only the `MechTypes` field (the OID list) from the original NegTokenInit, then compute GSS GetMIC over those bytes. When verifying client MIC: same data.
**Warning signs:** SPNEGO negotiation fails with "MIC verification failed" on clients that implement MIC checking (newer Windows).

### Pitfall 5: Replay Cache Key Collision
**What goes wrong:** Same authenticator accepted by both NFS and SMB services.
**Why it happens:** gokrb5's `service.VerifyAPREQ()` uses a global singleton replay cache that doesn't distinguish between service principals.
**How to avoid:** The custom replay cache in `internal/auth/kerberos/replay.go` should key on `(principal, ctime, cusec, service_principal)`. Since `VerifyAPREQ()` also checks its internal cache, cross-protocol replay is already prevented for the same SPN. But NFS (`nfs/host`) and SMB (`cifs/host`) use different SPNs, so the same authenticator could theoretically be accepted by both -- the custom cache adds defense-in-depth.
**Warning signs:** Security audit finding; practically low risk since tickets are SPN-specific.

### Pitfall 6: Stale Kerberos Context Leaking into NTLM Fallback
**What goes wrong:** On Kerberos failure, if the server responds with STATUS_MORE_PROCESSING_REQUIRED instead of rejecting, the client may try to continue the same session. Any Kerberos-related state in that session leaks into the NTLM flow.
**Why it happens:** Incorrect error handling in the SPNEGO dispatch.
**How to avoid:** On Kerberos failure, return SPNEGO NegState=reject. Client creates a fresh SESSION_SETUP (SessionId=0) for NTLM retry. No session state from the failed Kerberos attempt persists.
**Warning signs:** NTLM authentication succeeds but signing fails (wrong key material); session appears to have Kerberos metadata but NTLM credentials.

## Code Examples

### Session Key Extraction from AP-REQ (from existing NFS GSS code)
```go
// Source: internal/adapter/nfs/rpc/gss/framework.go (Krb5Verifier.VerifyToken)
// This is the proven pattern to extract and use in the shared service

// Extract session key from the decrypted ticket
sessionKey := apReq.Ticket.DecryptedEncPart.Key

// Decrypt the authenticator to access subkey and timestamps
if err := apReq.DecryptAuthenticator(sessionKey); err != nil {
    return nil, fmt.Errorf("decrypt authenticator: %w", err)
}

// Per RFC 4120, prefer authenticator subkey over ticket session key
contextKey := sessionKey
if apReq.Authenticator.SubKey.KeyType != 0 && len(apReq.Authenticator.SubKey.KeyValue) > 0 {
    contextKey = apReq.Authenticator.SubKey
}
```

### AP-REP Construction (from existing NFS GSS code)
```go
// Source: internal/adapter/nfs/rpc/gss/framework.go (buildAPRep)
// This is the proven pattern to extract into the shared service

encAPRepPart := messages.EncAPRepPart{
    CTime: apReq.Authenticator.CTime,
    Cusec: apReq.Authenticator.Cusec,
}
if hasSubkey(apReq) {
    encAPRepPart.Subkey = apReq.Authenticator.SubKey
}

// Marshal inner -> add APPLICATION 27 tag -> encrypt with session key (usage 12)
inner, _ := asn1.Marshal(encAPRepPart)
tagged := asn1tools.AddASNAppTag(inner, 27)
encrypted, _ := crypto.GetEncryptedData(tagged, sessionKey, 12, 0)

// Build AP-REP message
apRep := messages.APRep{PVNO: 5, MsgType: 15, EncPart: encrypted}
repInner, _ := asn1.Marshal(apRep)
repBytes := asn1tools.AddASNAppTag(repInner, 15)

// For SMB: don't wrap in GSS-API token (SPNEGO handles framing)
// For NFS: wrap with GSS-API header (0x60 + OID + 0x0200)
```

### SMB Session Key Normalization and KDF Integration
```go
// internal/adapter/smb/v2/handlers/kerberos_auth.go (new)

// normalizeSessionKey truncates or zero-pads a Kerberos session key to 16 bytes.
// Per [MS-SMB2] 3.3.5.5.3, the session key for SMB3 key derivation is always 16 bytes.
func normalizeSessionKey(key []byte) []byte {
    normalized := make([]byte, 16)
    copy(normalized, key) // copy min(16, len(key)) bytes; rest is zero
    return normalized
}

// After successful Kerberos auth:
sessionKey := normalizeSessionKey(authResult.SessionKey.KeyValue)
h.configureSessionSigningWithKey(sess, sessionKey, ctx)
```

### SPNEGO MIC Computation
```go
// internal/adapter/smb/auth/spnego.go (new helper)

// ComputeMechListMIC computes the MIC over the SPNEGO mechList for downgrade protection.
// The mechList is the DER-encoded SEQUENCE OF OIDs from the original NegTokenInit.
// The MIC is a GSS-API GetMIC token using the session key.
func ComputeMechListMIC(sessionKey types.EncryptionKey, mechListBytes []byte) ([]byte, error) {
    micToken := gssapi.MICToken{
        Flags:     gssapi.MICTokenFlagSentByAcceptor,
        SndSeqNum: 0,
        Payload:   mechListBytes,
    }
    if err := micToken.SetChecksum(sessionKey, 25); err != nil { // KeyUsageAcceptorSign
        return nil, err
    }
    return micToken.Marshal()
}
```

### Control Plane Settings Extension
```go
// pkg/controlplane/models/adapter_settings.go (modification)
type SMBAdapterSettings struct {
    // ... existing fields ...

    // Authentication settings
    NtlmEnabled         bool   `gorm:"default:true" json:"ntlm_enabled"`
    GuestEnabled        bool   `gorm:"default:true" json:"guest_enabled"`
    SMBServicePrincipal string `gorm:"size:256" json:"smb_service_principal"` // Override CIFS SPN
}
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| NFS-only Kerberos (gss/framework.go) | Shared KerberosService (`internal/auth/kerberos/`) | Phase 36 | Both NFS and SMB use same verification/AP-REP logic |
| Nil AP-REP in SMB Kerberos | Real AP-REP with mutual auth | Phase 36 | Domain-joined Windows clients get proper mutual authentication |
| Empty NEGOTIATE SecurityBuffer | SPNEGO NegHints with mech list | Phase 36 | Clients know which auth mechanisms are available before SESSION_SETUP |
| Hardcoded NTLM-only auth | Configurable Kerberos + NTLM + guest | Phase 36 | Runtime control of auth policy via control plane |

**Deprecated/outdated:**
- The `handleKerberosAuth()` skeleton in `session_setup.go` will be extracted to `kerberos_auth.go` and substantially rewritten
- The nil AP-REP approach in `handleKerberosAuth()` is replaced by real mutual auth

## Open Questions

1. **gokrb5 server-side AP-REP builder availability**
   - What we know: gokrb5 has `messages.APRep` struct and `messages.EncAPRepPart` struct. The NFS GSS code already builds AP-REP manually using `asn1.Marshal()` + `asn1tools.AddASNAppTag()` + `crypto.GetEncryptedData()`.
   - What's unclear: Whether gokrb5 has a higher-level builder we missed (unlikely based on code review).
   - Recommendation: Use the proven manual construction from NFS GSS code. HIGH confidence this works.

2. **SPNEGO mechListMIC: which GSS key usage?**
   - What we know: RFC 4178 says the MIC is computed using the GSS-API getMIC operation on the mechList bytes. For krb5, this means a MIC token with key usage 25 (acceptor sign) per RFC 4121.
   - What's unclear: Whether Windows clients actually verify the server's mechListMIC (many implementations skip it).
   - Recommendation: Implement full bidirectional MIC (verify client MIC if present, generate server MIC). Even if clients don't verify, it's correct per spec and needed for compliance.

3. **3-leg SPNEGO continuation tokens**
   - What we know: Normal Kerberos over SPNEGO is a single round trip (NegTokenInit with AP-REQ -> NegTokenResp with AP-REP). Some edge cases (e.g., mutual auth with certain KDC configurations) could theoretically produce a 3-leg exchange.
   - What's unclear: Whether any real-world Windows client sends continuation tokens for Kerberos SPNEGO.
   - Recommendation: Log and reject 3-leg tokens with STATUS_LOGON_FAILURE. Add handling in a future phase if real-world issues emerge.

## Sources

### Primary (HIGH confidence)
- Codebase analysis of `internal/adapter/nfs/rpc/gss/framework.go` -- proven AP-REQ verification and AP-REP construction
- Codebase analysis of `internal/adapter/smb/v2/handlers/session_setup.go` -- existing Kerberos skeleton and NTLM flow
- Codebase analysis of `internal/adapter/smb/session/crypto_state.go` -- `DeriveAllKeys()` and signing pipeline
- Codebase analysis of `internal/adapter/smb/kdf/kdf.go` -- SP800-108 KDF implementation
- Codebase analysis of `vendor/github.com/jcmturner/gokrb5/v8/` -- AP-REQ verify, SPNEGO types, MIC tokens
- [MS-SMB2] Section 3.3.5.5.3 -- Session key extraction and signing setup for Kerberos

### Secondary (MEDIUM confidence)
- [MS-SMB2] Section 3.3.5.5 -- SESSION_SETUP processing model (referenced from code comments)
- RFC 4178 -- SPNEGO specification (mechListMIC computation)
- RFC 4121 -- Kerberos V5 GSS-API mechanism (MIC token format, key usages)
- RFC 4120 Section 5.5.2 -- AP-REP format and EncAPRepPart structure

### Tertiary (LOW confidence)
- Samba source code behavior for AP-REP server subkey handling (referenced in CONTEXT.md; not directly verified)

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all libraries already vendored and used in the project
- Architecture: HIGH -- patterns directly derived from existing NFS GSS implementation and user decisions
- Pitfalls: HIGH -- identified from codebase analysis and protocol specification knowledge
- Session key normalization: HIGH -- matches [MS-SMB2] spec and Samba behavior
- SPNEGO MIC computation: MEDIUM -- RFC 4178 is clear but real-world interop not tested

**Research date:** 2026-03-02
**Valid until:** 2026-04-01 (stable domain; gokrb5 v8 is mature)
