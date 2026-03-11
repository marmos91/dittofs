# Phase 12: Kerberos Authentication - Research

**Researched:** 2026-02-15
**Domain:** RPCSEC_GSS / Kerberos v5 / NFS authentication
**Confidence:** HIGH

## Summary

Phase 12 implements RPCSEC_GSS (RFC 2203) with Kerberos v5 (RFC 4121) for DittoFS's NFS server. This is a mandatory requirement for NFSv4 compliance per RFC 7530, which states that both client and server MUST support krb5, krb5i, and krb5p pseudo-flavors. The implementation requires building an RPCSEC_GSS framework at the RPC layer, integrating Kerberos v5 via the gokrb5 library (already a dependency at v8.4.4), and implementing three security services: authentication-only (krb5), integrity protection (krb5i), and privacy/encryption (krb5p).

The key architectural insight is that RPCSEC_GSS operates at the RPC layer, not the NFS layer. It replaces AUTH_UNIX/AUTH_NULL as the authentication mechanism in the RPC credential/verifier fields. The credential structure carries a context handle and sequence number rather than UID/GID. After GSS context establishment, the server maps the Kerberos principal to a local UID/GID for filesystem access control, which integrates with the existing `AuthContext` and `Identity` types in `pkg/metadata/authentication.go`.

**Primary recommendation:** Build `pkg/auth/kerberos` as a shared Kerberos layer (keytab management, principal resolution, identity mapping) and `internal/protocol/nfs/rpc/gss` as the RPCSEC_GSS wire protocol implementation. The gokrb5 v8 library provides all necessary primitives: keytab parsing, AP-REQ verification, MICToken for integrity, and WrapToken for privacy. No additional Kerberos libraries are needed.

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| github.com/jcmturner/gokrb5/v8 | v8.4.4 | Kerberos v5 client/service, GSSAPI tokens, keytab | Already in go.mod; pure Go, tested against MIT KDC + AD |
| github.com/jcmturner/gokrb5/v8/keytab | (part of above) | Keytab file parsing | MIT keytab format support |
| github.com/jcmturner/gokrb5/v8/service | (part of above) | Server-side AP-REQ verification | VerifyAPREQ with replay cache |
| github.com/jcmturner/gokrb5/v8/gssapi | (part of above) | MICToken, WrapToken for krb5i/krb5p | RFC 4121 compliant |
| github.com/jcmturner/gokrb5/v8/crypto | (part of above) | Encryption/checksum operations | AES-128/256, RC4-HMAC support |
| github.com/jcmturner/gokrb5/v8/config | (part of above) | krb5.conf parsing | Standard KRB5 config format |
| github.com/jcmturner/gokrb5/v8/messages | (part of above) | AP-REQ/AP-REP message types | Core Kerberos message handling |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| github.com/jcmturner/gokrb5/v8/spnego | (part of above) | SPNEGO token handling | If SPNEGO wrapper needed for NFSv4 GSS tokens |
| github.com/jcmturner/gokrb5/v8/credentials | (part of above) | Credential extraction | Getting principal from verified ticket |
| internal/auth/spnego (existing) | n/a | SPNEGO parsing/building | Already exists, wraps gokrb5 for SMB/NFSv4 |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| gokrb5 pure Go | CGo binding to libkrb5 | Would break pure-Go requirement, add build complexity |
| golang-auth/go-gssapi/v2 | Different GSSAPI interface | Less mature than gokrb5; gokrb5 already in project |
| jake-scott/go-gssapi/v2 | Another GSSAPI wrapper | Uses gokrb5 internally anyway |

**Installation:**
```bash
# Already in go.mod -- no new dependencies needed
# gokrb5 v8.4.4 and all its subpackages are already available
```

## Architecture Patterns

### Recommended Project Structure
```
pkg/auth/kerberos/                  # Shared Kerberos layer (KRB-01)
    kerberos.go                     # KerberosProvider interface + implementation
    keytab.go                       # Keytab management, hot-reload, validation
    principal.go                    # Service principal resolution
    identity.go                     # Kerberos principal -> metadata.Identity mapping
    config.go                       # Kerberos configuration types

internal/protocol/nfs/rpc/gss/      # RPCSEC_GSS wire protocol (KRB-02)
    types.go                        # rpc_gss_cred_t, rpc_gss_init_res XDR types
    context.go                      # GSS context state machine + context store
    framework.go                    # RPCSEC_GSS processor (intercepts auth flavor 6)
    integrity.go                    # krb5i: rpc_gss_integ_data encode/decode (KRB-04)
    privacy.go                      # krb5p: rpc_gss_priv_data encode/decode (KRB-05)
    sequence.go                     # Sequence number window management
    verifier.go                     # Reply verifier computation

pkg/config/                         # Configuration extensions (KRB-08, KRB-09)
    config.go                       # Add KerberosConfig to main Config
```

### Pattern 1: RPCSEC_GSS at RPC Layer (Interceptor Pattern)
**What:** RPCSEC_GSS processing happens before procedure dispatch, at the RPC level.
**When to use:** All RPC calls with auth flavor 6 (RPCSEC_GSS).
**Why:** The protocol defines credential/verifier handling at the RPC transport layer, not at the NFS application layer. Context creation doesn't even reach NFS handlers.

```go
// In nfs_connection.go handleRPCCall(), BEFORE dispatch to program handlers:
//
// 1. Check auth flavor
// 2. If AUTH_RPCSEC_GSS (6):
//    a. Decode rpc_gss_cred_t from credential body
//    b. Switch on gss_proc:
//       - RPCSEC_GSS_INIT/CONTINUE_INIT: Context creation (no NFS dispatch)
//       - RPCSEC_GSS_DATA: Verify/unwrap, then dispatch normally
//       - RPCSEC_GSS_DESTROY: Destroy context (no NFS dispatch)
// 3. For DATA requests, extract identity from GSS context
// 4. Build reply with GSS verifier

type GSSProcessor struct {
    contexts    *ContextStore       // Thread-safe context store
    provider    *kerberos.Provider  // Keytab + ticket validation
    seqWindows  map[string]*SeqWindow // Per-context sequence tracking
}

// Process intercepts RPCSEC_GSS calls at the RPC level
func (g *GSSProcessor) Process(call *rpc.RPCCallMessage, data []byte) (
    processedData []byte,     // Unwrapped procedure args (for DATA)
    identity *metadata.Identity, // Resolved identity (for DATA)
    gssReply []byte,          // GSS-specific reply (for INIT/DESTROY)
    isControl bool,           // True if INIT/CONTINUE_INIT/DESTROY
    err error,
)
```

### Pattern 2: GSS Context State Machine
**What:** Each client creates a GSS security context through a multi-round handshake.
**When to use:** Context creation (RPCSEC_GSS_INIT, RPCSEC_GSS_CONTINUE_INIT).

```
Client                          Server
  |                               |
  |-- RPCSEC_GSS_INIT ----------->|  (GSS token from Init_sec_context)
  |                               |  Server: Accept_sec_context()
  |<-- rpc_gss_init_res ----------|  (handle + token + window)
  |                               |
  | [if CONTINUE_NEEDED]          |
  |-- RPCSEC_GSS_CONTINUE_INIT -->|  (next GSS token)
  |<-- rpc_gss_init_res ----------|  (updated token)
  |                               |
  | [context established]         |
  |-- RPCSEC_GSS_DATA ----------->|  (normal NFS ops with seq_num)
  |<-- reply with GSS verifier ---|
  |                               |
  |-- RPCSEC_GSS_DESTROY -------->|  (cleanup)
  |<-- reply --------------------|
```

### Pattern 3: Identity Mapping (Kerberos Principal to Unix UID/GID)
**What:** After GSS authentication, map the Kerberos principal to a local identity.
**When to use:** Every RPCSEC_GSS_DATA request.

```go
// The identity mapper converts a Kerberos principal to metadata.Identity
// This integrates with the existing CheckShareAccess and permission system.
//
// Mapping strategies (configurable per share):
// 1. Static mapping: "user@REALM" -> UID/GID (config file)
// 2. Local passwd lookup: getpwnam(principal_name)
// 3. AD integration: LDAP lookup for UID/GID attributes
// 4. PAC-based: Extract UID/GID from Microsoft PAC data in ticket

type IdentityMapper interface {
    MapPrincipal(principal string, realm string) (*metadata.Identity, error)
}
```

### Pattern 4: Integrity and Privacy Wrapping
**What:** For krb5i, wrap procedure args/results with MIC checksum. For krb5p, encrypt them.
**When to use:** DATA phase when service is integrity (2) or privacy (3).

```go
// krb5i (integrity):
// Request:  rpc_gss_integ_data { databody: XDR(seq_num + args), checksum: GetMIC(databody) }
// Response: rpc_gss_integ_data { databody: XDR(seq_num + results), checksum: GetMIC(databody) }
//
// krb5p (privacy):
// Request:  rpc_gss_priv_data { databody: Wrap(XDR(seq_num + args)) }
// Response: rpc_gss_priv_data { databody: Wrap(XDR(seq_num + results)) }

// Using gokrb5 GSSAPI tokens:
// - MICToken.Verify() / MICToken.SetChecksum() for krb5i
// - WrapToken.Unmarshal() / NewInitiatorWrapToken() for krb5p
```

### Anti-Patterns to Avoid
- **Kerberos in NFS handlers:** RPCSEC_GSS is an RPC-level concern. NFS handlers should receive the same `AuthContext`/`Identity` regardless of whether AUTH_UNIX or RPCSEC_GSS was used. The GSS processing must happen before dispatch.
- **Rolling own ASN.1/DER:** gokrb5 handles all ASN.1 encoding for Kerberos messages. Use the library types (APReq, MICToken, WrapToken), not manual encoding.
- **Single global keytab:** Keytabs should be hot-reloadable (watch file for changes) to allow key rotation without server restart.
- **Storing session keys in memory forever:** GSS contexts must be cleaned up via LRU/TTL to prevent memory leaks from abandoned connections.
- **Blocking on KDC:** All KDC communication happens during context creation. The DATA path only uses cached session keys. Do not make KDC calls on the hot path.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| Kerberos ticket validation | Custom ASN.1 parsing | gokrb5 `service.VerifyAPREQ()` | Replay detection, clock skew, encryption type negotiation |
| MIC computation (krb5i) | Custom HMAC | gokrb5 `gssapi.MICToken` | Key usage numbers, padding, RFC 4121 compliance |
| Encryption (krb5p) | Custom AES-CBC | gokrb5 `gssapi.WrapToken` | Rotation, padding, checksum integration |
| Keytab parsing | Custom binary parser | gokrb5 `keytab.Parse()` | MIT keytab format is complex with multiple versions |
| KRB5 config parsing | Custom ini parser | gokrb5 `config.Load()` | Realm mappings, KDC discovery, encryption policies |
| Sequence number window | Custom bitmap | Custom but simple sliding window | RFC 2203 specifies exact semantics; not complex but must be precise |
| XDR encoding for GSS types | Separate XDR library | Existing `internal/protocol/xdr` package | Already have XDR encode/decode helpers |

**Key insight:** The gokrb5 library provides all the cryptographic primitives. The custom code needed is: (1) the RPCSEC_GSS wire protocol framing (XDR encode/decode of rpc_gss_cred_t, rpc_gss_integ_data, etc.), (2) the context state machine, and (3) integration with the existing RPC dispatch.

## Common Pitfalls

### Pitfall 1: Wrong Key Usage Numbers
**What goes wrong:** MIC verification or WrapToken decryption fails with "checksum mismatch" errors.
**Why it happens:** RFC 4121 defines specific key usage numbers for different operations. Using the wrong key usage number produces valid-looking tokens that fail verification.
**How to avoid:** Use these exact key usage numbers from RFC 4121:
- Initiator MIC: 23
- Acceptor MIC: 25
- Initiator Wrap: 24
- Acceptor Wrap: 26
**Warning signs:** Tests pass with self-generated tokens but fail against Linux NFS clients.

### Pitfall 2: Reply Verifier Computation
**What goes wrong:** NFS clients reject server replies, connection drops.
**Why it happens:** RPCSEC_GSS replies must include a verifier computed as `GetMIC(seq_num)` over the sequence number from the corresponding request. The verifier flavor must be RPCSEC_GSS (6), not AUTH_NULL (0).
**How to avoid:** In `MakeSuccessReply`, when the original request used RPCSEC_GSS, compute verifier as MIC of the XDR-encoded sequence number using the context's session key.
**Warning signs:** Context creation works but first DATA request gets "authentication error" from client.

### Pitfall 3: XDR Encoding of rpc_gss_cred_t
**What goes wrong:** Credential parsing fails; server can't extract GSS context handle.
**Why it happens:** The `rpc_gss_cred_t` is a UNION type with a version discriminator. The credential body in `OpaqueAuth.Body` is the XDR encoding of this union, not a direct struct encoding.
**How to avoid:** Decode the credential body as: version (uint32) -> then the struct fields (gss_proc, seq_num, service, handle). Don't try to use `xdr.Unmarshal` with a flat struct.
**Warning signs:** Parsing works for INIT but fails for DATA, or handle appears corrupted.

### Pitfall 4: Sequence Number Overflow and Window
**What goes wrong:** Server starts rejecting valid requests from the client.
**Why it happens:** Sequence numbers wrap or the window size is too small for concurrent requests. MAXSEQ is 0x80000000 (not 0xFFFFFFFF). When seq_num reaches MAXSEQ, the context must be destroyed and re-created.
**How to avoid:** Implement sliding window per RFC 2203 Section 5.3.3.1. Use the `seq_window` from context creation response. Track seen sequence numbers in a bitmap.
**Warning signs:** Intermittent authentication failures under load; works fine with single-threaded client.

### Pitfall 5: Context Expiration and Cleanup
**What goes wrong:** Memory grows unboundedly; stale contexts accumulate.
**Why it happens:** Clients disconnect without sending RPCSEC_GSS_DESTROY. Each context holds a session key and sequence window.
**How to avoid:** Implement LRU eviction or TTL-based cleanup for the context store. A reasonable default is 2x the Kerberos ticket lifetime (typically 10 hours, so 20 hours TTL). Log context evictions for debugging.
**Warning signs:** Memory usage grows linearly with number of unique clients over days.

### Pitfall 6: DATA Body Location Differs by Service Level
**What goes wrong:** Cannot decode NFS procedure arguments from krb5i/krb5p requests.
**Why it happens:** For `rpc_gss_svc_none`, procedure args follow the credential/verifier normally. For `rpc_gss_svc_integrity`, the procedure args are inside `rpc_gss_integ_data.databody_integ` (XDR encoded with prepended seq_num). For `rpc_gss_svc_privacy`, they are inside the encrypted `rpc_gss_priv_data.databody_priv`.
**How to avoid:** The GSSProcessor must unwrap the data BEFORE passing it to the NFS handler. The handler should never see integrity/privacy wrapping.
**Warning signs:** NULL procedure works but GETATTR/LOOKUP fail with "bad XDR" errors when using krb5i.

### Pitfall 7: AUTH_SYS Fallback Must Be Explicit
**What goes wrong:** Clients expecting AUTH_SYS access get NFS4ERR_WRONGSEC errors.
**Why it happens:** When Kerberos is enabled, SECINFO must advertise which security flavors are available per share. If AUTH_SYS is not included, clients won't try it.
**How to avoid:** Make AUTH_SYS fallback configurable per share (KRB-06). Update the SECINFO handler to return all configured flavors including RPCSEC_GSS pseudo-flavors (390003, 390004, 390005) and optionally AUTH_SYS (1).
**Warning signs:** Existing AUTH_SYS clients break after enabling Kerberos on any share.

## Code Examples

### Example 1: rpc_gss_cred_t XDR Decode
```go
// Source: RFC 2203 Section 5
// Wire format of RPCSEC_GSS credential body

type RPCGSSCredV1 struct {
    GSSProc   uint32   // RPCSEC_GSS_DATA=0, INIT=1, CONTINUE_INIT=2, DESTROY=3
    SeqNum    uint32   // Sequence number for this request
    Service   uint32   // rpc_gss_svc_none=1, integrity=2, privacy=3
    Handle    []byte   // Context handle (opaque)
}

const (
    RPCGSSVers1        = 1
    AuthRPCSECGSS      = 6  // Auth flavor number

    RPCGSSData         = 0
    RPCGSSInit         = 1
    RPCGSSContinueInit = 2
    RPCGSSDestroy      = 3

    RPCGSSSvcNone      = 1
    RPCGSSSvcIntegrity = 2
    RPCGSSSvcPrivacy   = 3
)

func DecodeGSSCred(body []byte) (*RPCGSSCredV1, error) {
    reader := bytes.NewReader(body)

    version, err := xdr.DecodeUint32(reader)
    if err != nil {
        return nil, fmt.Errorf("decode version: %w", err)
    }
    if version != RPCGSSVers1 {
        return nil, fmt.Errorf("unsupported RPCSEC_GSS version: %d", version)
    }

    cred := &RPCGSSCredV1{}
    cred.GSSProc, _ = xdr.DecodeUint32(reader)
    cred.SeqNum, _ = xdr.DecodeUint32(reader)
    cred.Service, _ = xdr.DecodeUint32(reader)
    cred.Handle, _ = xdr.DecodeOpaque(reader)

    return cred, nil
}
```

### Example 2: GSS Context Creation (Server-Side)
```go
// Source: gokrb5 service.VerifyAPREQ + RFC 2203 Section 5.2.2
// Context creation handles RPCSEC_GSS_INIT / CONTINUE_INIT

func (g *GSSProcessor) handleInit(cred *RPCGSSCredV1, gssToken []byte) (*RPCGSSInitRes, error) {
    // Parse the KRB5 AP-REQ from the GSS token
    var apReq messages.APReq
    // GSS token wraps AP-REQ in an application tag (OID + AP-REQ)
    if err := apReq.Unmarshal(extractAPREQ(gssToken)); err != nil {
        return nil, fmt.Errorf("unmarshal AP-REQ: %w", err)
    }

    // Verify using keytab (handles decryption, replay check, clock skew)
    settings := service.NewSettings(
        g.provider.Keytab(),
        service.RequireHostAddr(false),
        service.DecodePAC(true),
        service.MaxClockSkew(5 * time.Minute),
        service.SName(g.provider.ServicePrincipal()),
    )

    valid, creds, err := service.VerifyAPREQ(&apReq, settings)
    if err != nil || !valid {
        return &RPCGSSInitRes{
            GSSMajor: gssErrDefectiveCredential,
        }, nil
    }

    // Extract session key from decrypted ticket
    sessionKey := apReq.Ticket.DecryptedEncPart.Key

    // Create context with unique handle
    ctx := &GSSContext{
        Handle:     generateHandle(),
        Principal:  creds.CName().String(),
        Realm:      creds.Realm,
        SessionKey: sessionKey,
        SeqWindow:  128, // Recommended default
        CreatedAt:  time.Now(),
    }
    g.contexts.Store(ctx)

    // Build AP-REP token for mutual authentication
    apRep := buildAPRep(apReq, sessionKey)

    return &RPCGSSInitRes{
        Handle:    ctx.Handle,
        GSSMajor:  gssComplete,
        GSSMinor:  0,
        SeqWindow: ctx.SeqWindow,
        GSSToken:  apRep,
    }, nil
}
```

### Example 3: Integrity Verification (krb5i)
```go
// Source: RFC 2203 Section 5.3.3.4.2 + gokrb5 gssapi.MICToken

func (g *GSSProcessor) unwrapIntegrity(
    ctx *GSSContext,
    cred *RPCGSSCredV1,
    requestBody []byte,
) ([]byte, error) {
    // Request body is: rpc_gss_integ_data { databody_integ<>, checksum<> }
    reader := bytes.NewReader(requestBody)

    databody, err := xdr.DecodeOpaque(reader)  // XDR(seq_num + args)
    if err != nil {
        return nil, fmt.Errorf("decode databody: %w", err)
    }

    checksum, err := xdr.DecodeOpaque(reader)  // MIC of databody
    if err != nil {
        return nil, fmt.Errorf("decode checksum: %w", err)
    }

    // Verify MIC using gokrb5 MICToken
    mic := &gssapi.MICToken{
        Payload:  databody,
        Checksum: checksum,
    }

    valid, err := mic.Verify(ctx.SessionKey, gssKeyUsageInitiatorMIC)
    if err != nil || !valid {
        return nil, fmt.Errorf("MIC verification failed")
    }

    // Extract seq_num and procedure args from databody
    dbReader := bytes.NewReader(databody)
    seqNum, _ := xdr.DecodeUint32(dbReader)

    // Validate sequence number
    if !ctx.SeqWindow.Accept(seqNum) {
        return nil, fmt.Errorf("sequence number rejected: %d", seqNum)
    }

    // Remaining bytes are the procedure arguments
    args := make([]byte, dbReader.Len())
    dbReader.Read(args)

    return args, nil
}
```

### Example 4: SECINFO with Kerberos Flavors
```go
// Source: RFC 7530 Section 16.31
// Update the existing SECINFO handler to return RPCSEC_GSS flavors

const (
    authNoneFlavor  = 0
    authSysFlavor   = 1
    authRPCSECGSS   = 6

    // Pseudo-flavors for NFSv4 per RFC 7530 Section 3.2.1
    PseudoFlavorKrb5  = 390003
    PseudoFlavorKrb5i = 390004
    PseudoFlavorKrb5p = 390005
)

// SECINFO response entry for RPCSEC_GSS includes additional info:
// flavor (6) + OID + QOP + service
// For krb5:  flavor=6, OID=1.2.840.113554.1.2.2, qop=0, service=rpc_gss_svc_none
// For krb5i: flavor=6, OID=1.2.840.113554.1.2.2, qop=0, service=rpc_gss_svc_integrity
// For krb5p: flavor=6, OID=1.2.840.113554.1.2.2, qop=0, service=rpc_gss_svc_privacy
```

### Example 5: Kerberos Configuration
```yaml
# Configuration for DittoFS Kerberos support
kerberos:
  enabled: true
  keytab_path: "/etc/dittofs/dittofs.keytab"
  service_principal: "nfs/dittofs.example.com@EXAMPLE.COM"
  # Optional: krb5.conf path (defaults to /etc/krb5.conf)
  krb5_conf: "/etc/krb5.conf"
  # Clock skew tolerance (default: 5 minutes)
  max_clock_skew: 5m
  # Context expiration (default: 8 hours)
  context_ttl: 8h
  # Maximum concurrent GSS contexts (default: 10000)
  max_contexts: 10000
  # Identity mapping strategy
  identity_mapping:
    # "static" | "passwd" | "ldap"
    strategy: "static"
    # Static mappings: principal -> UID/GID
    static_map:
      "admin@EXAMPLE.COM": { uid: 0, gid: 0 }
      "alice@EXAMPLE.COM": { uid: 1000, gid: 1000 }
    # Default UID/GID for authenticated but unmapped users
    default_uid: 65534
    default_gid: 65534

# Per-share auth configuration (already in share options)
# AllowedAuthMethods: ["unix", "kerberos"]
# AUTH_SYS fallback is controlled by including "unix" in AllowedAuthMethods
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| AUTH_UNIX only (current DittoFS) | RPCSEC_GSS mandatory for NFSv4 | RFC 7530 (2015) | Must implement krb5/krb5i/krb5p |
| DES encryption (AUTH_DES) | AES-128/256 via Kerberos 5 | RFC 3962 (2005) | DES no longer acceptable |
| GSS-API v1 (RFC 2078) | GSS-API v2 (RFC 2743) | 2000 | Current gokrb5 implements v2 |
| RPCSEC_GSS v1 (RFC 2203) | RPCSEC_GSS v3 (RFC 7861) | 2016 | v1 is sufficient for initial impl |
| Manual SPN registration | AD/IPA automated provisioning | Recent tooling | Still need manual keytab for custom server |

**Not needed for initial implementation:**
- RPCSEC_GSS v3 (RFC 7861): Adds multi-principal auth and channel bindings. v1 is sufficient and universally supported.
- SPNEGO wrapping for NFS: NFS uses raw KRB5 mechanism OID, not SPNEGO. SPNEGO is for SMB.
- Kerberos v4: Obsolete. Only v5 matters.
- User2User authentication: Advanced feature not required for standard NFS.

## Integration Points

### 1. RPC Layer (nfs_connection.go)
The `handleRPCCall` method is the primary integration point. Before dispatching to program-specific handlers, check `call.GetAuthFlavor()` for `AuthRPCSECGSS` (6) and route through the `GSSProcessor`.

**Current flow:**
```
ReadCall -> ReadData -> handleRPCCall -> handleNFSProcedure/handleNFSv4Procedure
```

**New flow:**
```
ReadCall -> ReadData -> handleRPCCall -> GSSProcessor.Process() -> handleNFSProcedure/v4
                                            |
                                            +-> INIT/CONTINUE/DESTROY (return directly)
                                            +-> DATA: unwrap, extract identity, proceed
```

### 2. Reply Construction (rpc/parser.go)
`MakeSuccessReply` currently hardcodes AUTH_NULL verifier. For RPCSEC_GSS, the reply verifier must be `GetMIC(seq_num)` with the context's session key. Need a `MakeGSSSuccessReply(xid, data, gssVerifier)` variant.

For krb5i/krb5p, the reply body itself needs wrapping:
- krb5i: body = `rpc_gss_integ_data { XDR(seq_num + results), GetMIC(databody) }`
- krb5p: body = `rpc_gss_priv_data { Wrap(XDR(seq_num + results)) }`

### 3. Auth Context Creation (dispatch.go)
`ExtractHandlerContext` and `ExtractV4HandlerContext` currently only handle AUTH_UNIX. For RPCSEC_GSS, the GSSProcessor provides the `metadata.Identity` from the Kerberos principal mapping. The handler context creation needs an additional path:

```go
if authFlavor == AuthRPCSECGSS {
    // Identity already resolved by GSSProcessor
    handlerCtx.UID = gssIdentity.UID
    handlerCtx.GID = gssIdentity.GID
    handlerCtx.GIDs = gssIdentity.GIDs
    handlerCtx.AuthFlavor = AuthRPCSECGSS
}
```

### 4. SECINFO Handler (v4/handlers/secinfo.go)
Currently returns only AUTH_SYS (1) and AUTH_NONE (0). Must be updated to include RPCSEC_GSS entries with KRB5 OID and service levels when Kerberos is configured.

### 5. Configuration (pkg/config/)
Add `KerberosConfig` to the main `Config` struct with keytab path, service principal, identity mapping strategy.

### 6. Share Options (pkg/metadata/types.go)
`AllowedAuthMethods` already supports `"kerberos"`. The per-share security flavor list needs to include specific krb5/krb5i/krb5p levels.

## Open Questions

1. **Identity Mapping Strategy**
   - What we know: Kerberos provides a principal name (e.g., "alice@EXAMPLE.COM"). NFS needs UID/GID.
   - What's unclear: Whether to support only static mapping initially or also include passwd/LDAP lookup.
   - Recommendation: Start with static mapping in config file. Add LDAP/passwd in a future phase. The `IdentityMapper` interface allows future extension.

2. **NFSv3 + Kerberos**
   - What we know: RFC 2623 defines RPCSEC_GSS usage with NFSv3. It works at the RPC level, same as v4.
   - What's unclear: Whether DittoFS should support krb5 for v3 connections or only v4.
   - Recommendation: Implement at the RPC layer (before version dispatch), so both v3 and v4 benefit automatically. The GSSProcessor does not need to know NFS version.

3. **Mutual Authentication**
   - What we know: NFS clients typically request mutual authentication (server proves identity to client).
   - What's unclear: Whether the AP-REP response during context init is sufficient or if additional steps are needed.
   - Recommendation: gokrb5's `service.VerifyAPREQ` handles the AP-REQ side. Build the AP-REP from the authenticated session key. This is standard Kerberos mutual auth.

4. **Keytab Hot-Reload**
   - What we know: Production deployments rotate keytab files periodically.
   - What's unclear: Whether fsnotify-based reload (already in go.mod) or periodic polling is better.
   - Recommendation: Use fsnotify (already a dependency) to watch the keytab file. On change, parse new keytab and atomically swap. Old contexts continue with their established session keys.

## Relevant RFCs

| RFC | Title | Relevance |
|-----|-------|-----------|
| RFC 2203 | RPCSEC_GSS Protocol Specification | Core protocol: credential format, context lifecycle, integrity/privacy |
| RFC 4121 | Kerberos V GSS-API Mechanism | Token formats (MIC, Wrap), key usage numbers |
| RFC 7530 | NFSv4 Protocol | Mandates RPCSEC_GSS + KRB5, defines pseudo-flavors, SECINFO op |
| RFC 2623 | NFS v2/v3 + RPCSEC_GSS | Usage of RPCSEC_GSS with NFSv3 |
| RFC 4120 | Kerberos V5 | Core Kerberos protocol (AP-REQ/REP, ticket format) |
| RFC 7861 | RPCSEC_GSS v3 | Future: channel bindings, multi-principal (not needed initially) |

## Sources

### Primary (HIGH confidence)
- [RFC 2203 - RPCSEC_GSS Protocol Specification](https://datatracker.ietf.org/doc/html/rfc2203) -- Wire format, context lifecycle, all XDR types
- [RFC 7530 - NFSv4 Protocol](https://datatracker.ietf.org/doc/html/rfc7530) -- Mandatory krb5/krb5i/krb5p, pseudo-flavors, SECINFO
- [gokrb5/v8 Go Package](https://pkg.go.dev/github.com/jcmturner/gokrb5/v8) -- Library API, subpackages
- [gokrb5 gssapi Package](https://pkg.go.dev/github.com/jcmturner/gokrb5/v8/gssapi) -- MICToken, WrapToken types and methods
- [gokrb5 service Package](https://pkg.go.dev/github.com/jcmturner/gokrb5/v8/service) -- VerifyAPREQ, Settings, replay cache
- [gokrb5 messages Package](https://pkg.go.dev/github.com/jcmturner/gokrb5/v8/messages) -- APReq, APRep structures
- Existing codebase: `internal/protocol/nfs/rpc/` -- Current RPC layer implementation
- Existing codebase: `internal/auth/spnego/` -- SPNEGO already using gokrb5
- Existing codebase: `pkg/metadata/authentication.go` -- AuthContext, Identity, CheckShareAccess

### Secondary (MEDIUM confidence)
- [NFS-Ganesha RPCSEC_GSS Wiki](https://github.com/nfs-ganesha/nfs-ganesha/wiki/RPCSEC_GSS) -- Reference C implementation patterns
- [Debian NFS/Kerberos Wiki](https://wiki.debian.org/NFS/Kerberos) -- Keytab and principal configuration
- [Red Hat NFS Security Guide](https://access.redhat.com/documentation/en-us/red_hat_enterprise_linux/7/html/storage_administration_guide/s1-nfs-security) -- krb5/krb5i/krb5p operational details
- [Linux kernel rpc-server-gss](https://docs.kernel.org/filesystems/nfs/rpc-server-gss.html) -- Reference kernel implementation

### Tertiary (LOW confidence)
- [golang-auth/go-gssapi/v2](https://pkg.go.dev/github.com/golang-auth/go-gssapi/v2) -- Alternative GSSAPI interface (not recommended, gokrb5 is sufficient)
- [NFSv4 Kerberos with Active Directory](https://tbellembois.github.io/kerberos.html) -- Community guide for AD integration

## Competitive Analysis

**Researched:** 2026-02-15
**Confidence:** HIGH (kernel and NFS-Ganesha), MEDIUM (MinIO, Samba), LOW (JuiceFS, go-nfs)

This section analyzes how existing projects implement Kerberos/RPCSEC_GSS authentication. The goal is to identify architecture patterns, solved problems, and pitfalls that DittoFS can learn from rather than re-discovering.

### 1. Linux Kernel NFS Server (`net/sunrpc/auth_gss/`)

**Source:** [github.com/torvalds/linux/tree/master/net/sunrpc/auth_gss](https://github.com/torvalds/linux/tree/master/net/sunrpc/auth_gss), [kernel docs](https://docs.kernel.org/filesystems/nfs/rpc-server-gss.html)

**Architecture:**
The kernel splits RPCSEC_GSS into two distinct phases with a clear boundary:
- **Context establishment** is delegated to userspace (via rpc.svcgssd or gssproxy)
- **Per-packet integrity/privacy** is handled entirely in kernel space

This split exists because context creation requires complex GSSAPI library interactions (ASN.1 parsing, KDC communication) that are impractical in kernel space, while per-packet operations are performance-critical and must avoid user-kernel transitions.

**Key files and their responsibilities:**
| File | Purpose |
|------|---------|
| `svcauth_gss.c` | Server-side GSS auth -- the main orchestrator |
| `gss_krb5_mech.c` | Kerberos 5 mechanism registration |
| `gss_krb5_crypto.c` | AES/DES crypto operations for MIC/Wrap |
| `gss_krb5_seal.c` / `gss_krb5_unseal.c` | Message sealing (integrity) |
| `gss_krb5_wrap.c` | Message wrapping (privacy) |
| `gss_rpc_upcall.c` | Kernel-to-userspace communication |
| `gss_mech_switch.c` | Pluggable mechanism selection |

**Two upcall mechanisms (important lesson):**
1. **Legacy (rpc.svcgssd):** Text-based protocol with hard 2KiB token limit and 4KiB response buffer. Breaks with large Kerberos tokens (common in AD environments with many group memberships) and users in thousands of groups.
2. **Modern (gssproxy):** RPC-over-unix-socket with no size restrictions, supporting tokens up to 64KiB+. Replaces the legacy daemon.

**Three-cache system:**
- `rsi_cache` (init cache): Maps RPCSEC_GSS init request tokens to responses during context negotiation
- `rsc_cache` (context cache): Stores established contexts with session keys and sequence windows
- Both use hash-based lookup with XDR serialization for cache entries

**What to copy:**
- The clear separation between context creation (complex, rare) and data path (fast, frequent). DittoFS should ensure no KDC calls happen on the DATA path.
- Pluggable mechanism selection via `gss_mech_switch.c`. Even though DittoFS only needs KRB5 initially, building the GSSProcessor with a mechanism interface allows future extensibility.
- The sequence window is a bitmap tracking the highest seen sequence number plus a window. The `gss_check_seq_num()` function validates incoming sequences efficiently.

**What NOT to copy:**
- The upcall mechanism. The kernel needs upcalls because it cannot run userspace GSSAPI libraries. DittoFS is entirely userspace, so we handle everything in-process using gokrb5. This is a massive simplification.
- The legacy 2KiB/4KiB buffer limitations. By using gokrb5 directly, DittoFS has no artificial token size limits.

**Key lesson:** The kernel implementation proves that the DATA path (MIC verification, Wrap/Unwrap) is simple once context creation is complete. The complexity is in context establishment. Since DittoFS uses gokrb5 in-process, the hardest part of the kernel's implementation (upcalls) disappears entirely.

### 2. NFS-Ganesha (Userspace NFS Server)

**Source:** [github.com/nfs-ganesha/nfs-ganesha](https://github.com/nfs-ganesha/nfs-ganesha), [RPCSEC_GSS wiki](https://github.com/nfs-ganesha/nfs-ganesha/wiki/RPCSEC_GSS), [ntirpc auth_gss.c](https://github.com/nfs-ganesha/ntirpc/blob/305482a4b84658f8d667cb1a85c78f6c2c133e65/src/auth_gss.c)

**Architecture:**
NFS-Ganesha is the most relevant reference because it is a userspace NFS server -- exactly what DittoFS is. Their key decisions:
- Uses `libgssrpc` from the MIT Kerberos distribution for GSSAPI operations
- Handles ALL RPCSEC_GSS processing in-process ("No rpc.gssd or rpc.svcgssd or rpc.ipmad is required")
- RPCSEC_GSS is built into their `ntirpc` (New TI-RPC) library, which is the RPC transport layer

**RPC Abstraction Layer (RPCAL) structure:**
```
src/RPCAL/
    connection_manager.c        # TCP connection lifecycle
    connection_manager_metrics.c # Performance monitoring
    gss_credcache.c             # GSS credential caching
    gss_extra.c                 # Extended GSS functionality
    rpc_tools.c                 # RPC utilities
    nfs_dupreq.c                # Duplicate request detection
```

**Context caching pattern:**
NFS-Ganesha stores GSS contexts in a hash-based cache (`authgss_ctx_hash_get()` / `authgss_ctx_hash_set()`). Context handles are used as hash keys for O(1) lookup during DATA phase.

**Critical bug found and fixed (race condition):**
NFS-Ganesha had a documented race condition where the server sent the RPCSEC_GSS_INIT reply to the client BEFORE inserting the new context into the cache. When the client immediately sent a DATA request (e.g., EXCHANGE_ID), a different worker thread could try to look up the context before it was cached, causing RPCSEC_GSS_CREDPROBLEM errors.

**What to copy:**
- The "everything in-process" model. DittoFS should do exactly what NFS-Ganesha does: use a Kerberos library directly (gokrb5 instead of libgssrpc) with no external daemons.
- The dedicated credential cache module (`gss_credcache.c`). DittoFS should have a dedicated `ContextStore` with thread-safe hash-based lookup.
- Configuring via an `NFS_KRB5` block with principal name, keytab path, and enabled flag -- our `kerberos:` config block follows this pattern.

**What NOT to copy:**
- Using a C GSSAPI library. NFS-Ganesha depends on `libgssrpc` (from MIT Kerberos), which would require CGo. DittoFS uses pure Go via gokrb5.
- Their `--enable-gssrpc` compile flag approach. DittoFS should always compile with Kerberos support; the feature is toggled at runtime via config.

**Critical lesson to internalize:** Store the GSS context in the cache BEFORE sending the init reply to the client. This avoids the race condition that NFS-Ganesha discovered. In DittoFS's `handleInit()`, the `g.contexts.Store(ctx)` call must happen before the reply is written to the TCP connection.

### 3. JuiceFS (Go Distributed Filesystem)

**Source:** [juicefs.com/docs/cloud/hadoop/for-kerberos](https://juicefs.com/docs/cloud/hadoop/for-kerberos/), [github.com/juicedata/juicefs issue #3283](https://github.com/juicedata/juicefs/issues/3283)

**Architecture:**
JuiceFS takes a fundamentally different approach to Kerberos than what DittoFS needs:
- Kerberos support is in the **Hadoop Java SDK** (not the Go client)
- Authenticates at the **application layer** (Hadoop security framework), not the RPC/wire protocol layer
- The Go community edition had no Kerberos support until recently; it only supported "authenticated usernames" without identity verification
- Enterprise edition adds Kerberos, Ranger, POSIX ACL, and access tokens

**Keytab evolution (relevant lesson):**
JuiceFS initially only supported credential cache files (`KRB5CCNAME`), not keytab files. This caused significant operational pain in Kubernetes environments because:
- Credential caches expire (ticket lifetime)
- Users had to repeatedly `kinit` and refresh Kubernetes secrets
- No automatic renewal was possible

This was fixed in [PR #3517](https://github.com/juicedata/juicefs/issues/3283) by adding `KRB5KEYTAB` and `KRB5PRINCIPAL` environment variables for keytab-based authentication.

**What to copy:**
- Support both keytab files (primary) AND environment variable overrides for keytab/principal paths (`KRB5KEYTAB`, `KRB5PRINCIPAL`). This is essential for Kubernetes/container deployments.
- The identity mapping configuration through `core-site.xml` maps to our `identity_mapping.static_map` concept.

**What NOT to copy:**
- JuiceFS's application-layer authentication model. DittoFS needs wire-level RPCSEC_GSS, not application-level auth.
- Starting with credential cache only. DittoFS should support keytab from day one (avoiding JuiceFS's initial mistake).

**Key lesson:** Container/Kubernetes deployments demand keytab-based auth with environment variable paths. Credential cache files are operationally painful. DittoFS's config should support both `keytab_path` in config and `DITTOFS_KERBEROS_KEYTAB` / `DITTOFS_KERBEROS_PRINCIPAL` env vars from the start.

### 4. MinIO (Go Object Storage)

**Source:** [pkg.go.dev/github.com/minio/gokrb5/v7](https://pkg.go.dev/github.com/minio/gokrb5/v7), [MinIO LDAP docs](https://min.io/docs/minio/kubernetes/upstream/operations/external-iam/configure-ad-ldap-external-identity-management.html)

**Architecture:**
MinIO implements Kerberos authentication as **HTTP SPNEGO middleware**, not at the RPC level:
- Maintains a fork of gokrb5 (v7, older than the v8 DittoFS uses)
- Uses the `spnego.SPNEGOKRB5Authenticate()` HTTP handler wrapper
- Authentication happens at the HTTP layer, wrapping regular handlers
- Supports AD keytab with `KeytabPrincipal` option for service account mapping
- Processes Microsoft PAC authorization data from tickets

**Identity flow:**
1. Client sends HTTP request with `Authorization: Negotiate <SPNEGO-token>` header
2. MinIO's SPNEGO middleware validates the token using keytab
3. Authenticated principal is placed in the HTTP request context
4. MinIO then uses STS (Security Token Service) `AssumeRoleWithLDAPIdentity` to map the principal to IAM policies
5. LDAP group memberships are synced periodically for policy updates

**LDAP auto-sync pattern (relevant):**
MinIO periodically queries the LDAP server to detect account removals and group membership changes. Removed accounts have their STS credentials purged automatically. This prevents stale access after user deprovisioning.

**What to copy:**
- The middleware/interceptor pattern for authentication. MinIO wraps HTTP handlers; DittoFS wraps RPC dispatch. Same concept, different transport.
- PAC data extraction from Kerberos tickets (`service.DecodePAC(true)` in gokrb5 settings). This can provide group memberships directly from the ticket without a separate LDAP lookup.
- The idea of periodic background sync for identity mapping staleness. Even with static mapping, DittoFS could periodically reload the mapping file.

**What NOT to copy:**
- MinIO's HTTP/SPNEGO approach. NFS uses raw KRB5 mechanism OID in RPCSEC_GSS, not SPNEGO. SPNEGO is only for SMB in DittoFS (already handled by `internal/auth/spnego/`).
- MinIO's v7 gokrb5 fork. DittoFS already has the newer v8.

**Key lesson:** The gokrb5 SPNEGO handler proves that pure-Go Kerberos authentication works well in production Go services. MinIO validates DittoFS's choice of gokrb5 as the Kerberos library.

### 5. Samba (Official SMB Implementation)

**Source:** [wiki.samba.org/index.php/Samba_Security_Documentation](https://wiki.samba.org/index.php/Samba_Security_Documentation), [samba-team/samba kerberos-notes.txt](https://github.com/samba-team/samba/blob/master/source4/auth/kerberos/kerberos-notes.txt)

**Architecture -- GENSEC (Generic Security Subsystem):**
Samba's most important architectural contribution is GENSEC, a unified security abstraction that handles all authentication regardless of protocol. GENSEC sits at layers 3-6 and provides:

```
Application Protocol (SMB, LDAP, DCE/RPC)
         |
      GENSEC  <-- Single programming interface
         |
    +---------+---------+---------+
    |         |         |         |
  SPNEGO   GSSAPI    NTLM    SCHANNEL
    |         |
  KRB5     KRB5
```

**Key GENSEC design decisions:**
1. **Opaque blob pattern:** All security operations produce/consume opaque byte buffers. The application protocol never needs to understand Kerberos, NTLM, or any mechanism details.
2. **Plugin-based backends:** NTLMSSP, GSSAPI(KRB5), and SCHANNEL are interchangeable plugins. The higher-level code just calls `gensec_update()` with opaque tokens.
3. **Negotiation handling:** SPNEGO negotiation is a GENSEC plugin itself, not a special case. It wraps the actual mechanism plugin.
4. **State machine safety:** All GENSEC operations are non-blocking and context-based (no globals). This enables event-driven server architectures.

**Kerberos-specific patterns from `kerberos-notes.txt`:**
- **SPN aliasing:** `HOST/` principal aliases allow `CIFS/`, `HTTP/`, etc. to share the same key. Reduces keytab management overhead.
- **Account authorization abstraction:** Beyond just "is this a valid Kerberos ticket?", Samba adds authorization checks: "Is this account permitted to access this service, at this time, from this workstation?" via three plugin callbacks: `pac_generate`, `pac_verify`, `client_access`.
- **Legacy client compatibility:** Old Samba3 clients send incorrectly checksummed GSSAPI tokens. Rather than strict RFC compliance, Samba chose pragmatic compatibility. Lesson: real-world clients may not be perfectly RFC-compliant.
- **`send_to_kdc()` hook:** Allows the server to intercept and route KDC communication through custom channels, avoiding blocking the main event loop.

**What to copy:**
- The opaque blob / interceptor pattern. DittoFS's `GSSProcessor` should operate on opaque bytes at the RPC level. NFS handlers never see authentication details -- they get an `Identity` / `AuthContext` the same way regardless of auth method.
- The authorization abstraction. Beyond verifying the ticket, DittoFS should check share-level access (`CheckShareAccess`) using the resolved identity. This is already planned in the architecture.
- State machine safety. All GSS context operations should be context-based with no global state, matching DittoFS's goroutine-per-connection model.
- SPN aliasing awareness. DittoFS should accept both `nfs/hostname@REALM` and potentially `host/hostname@REALM` service principals to handle environments where administrators use HOST/ aliases.

**What NOT to copy:**
- The full GENSEC abstraction layer. DittoFS only needs RPCSEC_GSS for NFS and SPNEGO for SMB. A full generic security subsystem is over-engineering for our scope.
- Heimdal/MIT KDC integration. Samba can run its own KDC as a domain controller. DittoFS is a service, not a KDC.
- Legacy client workarounds for broken GSSAPI checksums. Linux NFS clients are well-tested; we should be strict on RFC compliance.

**Key lesson:** Samba's GENSEC validates the architecture of having a shared auth layer (`pkg/auth/kerberos/`) that is protocol-agnostic, with protocol-specific integration points (`internal/protocol/nfs/rpc/gss/` for NFS, `internal/auth/spnego/` for SMB). Both protocols ultimately resolve to the same `Identity` type.

### 6. go-nfs (Go NFS Library by willscott)

**Source:** [github.com/willscott/go-nfs](https://github.com/willscott/go-nfs)

**Architecture:**
go-nfs does NOT support RPCSEC_GSS or Kerberos authentication. The library uses `NewNullAuthHandler` (AUTH_NONE) as its authentication mechanism. There is no RPCSEC_GSS implementation in the Go NFS ecosystem.

**What this means for DittoFS:**
- DittoFS will be the first pure-Go NFS server with RPCSEC_GSS support (to our knowledge)
- There is no Go library to import for RPCSEC_GSS framing -- we must implement it ourselves
- The custom code needed is modest: XDR types for `rpc_gss_cred_t`, `rpc_gss_init_res`, `rpc_gss_integ_data`, `rpc_gss_priv_data` plus the context state machine. All crypto is handled by gokrb5.

**Key lesson:** No shortcuts exist in the Go ecosystem. The RPCSEC_GSS wire protocol framing must be built from scratch, guided by RFC 2203 and validated against the Linux kernel NFS client.

### 7. RFC Implementation Guidance

**Source:** [RFC 2203](https://www.rfc-editor.org/rfc/rfc2203), [RFC 2623](https://www.rfc-editor.org/rfc/rfc2623)

**RFC 2203 Section 5 -- Critical implementation notes:**

1. **Disable GSS-API replay/sequence detection:** When calling the equivalent of `GSS_Init_sec_context()`, the RFC mandates that `replay_det_req_flag` and `sequence_req_flag` must be turned OFF. Reason: ONC RPC operates over unreliable transports, multi-threaded servers process messages out of order, and the RPCSEC_GSS protocol has its own sequence window mechanism. Using gokrb5's `service.VerifyAPREQ()`, this means not relying on the library's built-in replay detection for the ongoing session -- only for the initial AP-REQ verification.

2. **Sequence window sizing:** The server should select `seq_window` based on expected concurrent requests. For DittoFS's goroutine-per-connection model, 128 is reasonable. The RFC explicitly states "there are no known security issues with selecting a large window."

3. **Silent discard for out-of-range sequences:** The server MUST silently discard (no error reply) requests with sequence numbers outside the window or duplicates. This allows clients to recover via timeout. Do NOT send error replies for sequence violations.

4. **Sequence number increment on retransmit:** Clients MUST increment the sequence number even when retransmitting the same RPC (same XID). This is different from how XID-based duplicate detection works. The sequence number and XID are independent.

5. **Dual sequence validation for integrity/privacy:** For krb5i and krb5p, the sequence number appears in BOTH the credential AND the wrapped body. The RFC requires the server to reject requests where these two sequence numbers differ.

6. **QOP consistency:** The same QOP value used for the header checksum MUST be used for the data (integrity or privacy). Mixing QOP values within a single request is invalid.

7. **Context cleanup obligation:** "An LRU mechanism or an aging mechanism should be employed by the server to clean up" orphaned contexts. The RFC acknowledges clients may not send RPCSEC_GSS_DESTROY.

**RFC 2203 Section 7 -- Security considerations:**

8. **Header privacy limitation:** RPC program number, version, and procedure number are NEVER encrypted, even with krb5p. An attacker can see which NFS operations are being performed. The RFC acknowledges this as a known limitation.

9. **DoS via sequence window exhaustion:** An attacker monitoring traffic could send spoofed requests with high sequence numbers, causing the server to waste CPU on checksum verification. Mitigation: skip header checksum verification if sequence number is below the current window (cheap integer comparison before expensive crypto).

**RFC 2623 -- NFSv3 + RPCSEC_GSS specifics:**

10. **Service principal format:** The NFS server principal MUST be `nfs@hostname` (hostbased service format). This is different from CIFS (`cifs/hostname@REALM`).

11. **MOUNT protocol returns security flavors:** The MOUNT v3 response includes a list of supported auth flavors. Clients SHOULD use the first flavor they support. DittoFS should present krb5p first, then krb5i, then krb5, then AUTH_SYS (if allowed), matching the most-secure-first convention.

12. **NULL procedure must accept AUTH_NONE:** The NULL procedure is used for pinging and RPCSEC_GSS context establishment. It MUST accept AUTH_NONE regardless of per-share security settings.

13. **Mount-time operations can be unauthenticated:** GETATTR, STATFS (v2), FSINFO (v3) may use AUTH_NONE/AUTH_SYS to support unattended automounter operations. This is configurable -- strict deployments may require Kerberos for all operations.

### Competitive Summary Table

| Implementation | Language | RPCSEC_GSS | Approach | Key Pattern for DittoFS |
|---------------|----------|------------|----------|------------------------|
| Linux Kernel | C | Full (v1+v2) | Split: userspace init, kernel data path | Separation of context creation vs data path |
| NFS-Ganesha | C | Full (v1) | In-process via libgssrpc | Everything in-process; cache-before-reply |
| JuiceFS | Go/Java | No (app-layer only) | Hadoop SDK Kerberos | Keytab > credential cache for containers |
| MinIO | Go | No (HTTP SPNEGO) | gokrb5 SPNEGO middleware | gokrb5 works well in production Go |
| Samba | C | N/A (uses SPNEGO/GSSAPI) | GENSEC plugin abstraction | Shared auth layer, opaque blob pattern |
| go-nfs | Go | None | AUTH_NONE only | No Go RPCSEC_GSS exists; must build |
| XetHub NFS | Rust | None | AUTH_NONE/AUTH_SYS | Even new NFS servers skip GSS initially |

### Actionable Takeaways for DittoFS

**Architecture decisions validated by competition:**

1. **In-process Kerberos (from NFS-Ganesha):** Handle RPCSEC_GSS entirely within the DittoFS process using gokrb5. No external daemons needed. This is the proven approach for userspace NFS servers.

2. **RPC-level interceptor (from Linux kernel + Samba GENSEC):** GSS processing at the RPC layer before NFS dispatch. NFS handlers receive a resolved `Identity`, never raw Kerberos tokens. Both the kernel and Samba validate this as the correct layer boundary.

3. **Shared auth provider (from Samba GENSEC):** `pkg/auth/kerberos/` serves both NFS (RPCSEC_GSS) and SMB (SPNEGO) with the same keytab management, principal resolution, and identity mapping. Protocol-specific wrappers translate to/from wire formats.

4. **Cache context before reply (from NFS-Ganesha bug):** When handling RPCSEC_GSS_INIT, store the new context in the ContextStore BEFORE writing the reply to the client. This prevents the race condition where a subsequent DATA request arrives before the context is cached.

5. **Keytab-first, env-var-supported (from JuiceFS lesson):** Support keytab files as the primary authentication mechanism. Add `DITTOFS_KERBEROS_KEYTAB` and `DITTOFS_KERBEROS_PRINCIPAL` environment variables for Kubernetes deployments. Never rely on credential cache files alone.

6. **Sequence window as bitmap (from Linux kernel):** Track seen sequence numbers using the kernel's approach: store the highest seen sequence number and a bitmap of size `seq_window` tracking which recent numbers have been seen. Default window of 128 is sufficient for goroutine-per-connection.

7. **Silent discard for sequence violations (from RFC 2203):** Never send error replies for out-of-window or duplicate sequence numbers. Let the client timeout and retry. This is mandatory per RFC.

8. **Accept HOST/ SPN aliases (from Samba):** When verifying the AP-REQ, accept both `nfs/hostname@REALM` and `host/hostname@REALM` principals. Some environments configure HOST/ aliases that should work for NFS.

**Implementation risks identified by competition:**

1. **No Go RPCSEC_GSS library exists** (go-nfs gap). We must build the wire protocol from scratch. Mitigated by: RFC 2203 is well-specified, the XDR types are simple, and all crypto is in gokrb5.

2. **Context cache race condition** (NFS-Ganesha bug). Mitigated by: explicit cache-before-reply ordering in our implementation.

3. **Large Kerberos tokens in AD environments** (kernel legacy limit). Mitigated by: gokrb5 has no artificial token size limits. Pure Go avoids the kernel's historical buffer constraints.

4. **Stale identity mappings** (MinIO LDAP sync). Static mapping avoids this initially. When LDAP mapping is added later, implement periodic sync similar to MinIO's approach.

### Competitive Analysis Sources

- [Linux kernel net/sunrpc/auth_gss/](https://github.com/torvalds/linux/tree/master/net/sunrpc/auth_gss) -- Source code for kernel RPCSEC_GSS
- [Linux kernel rpc-server-gss docs](https://docs.kernel.org/filesystems/nfs/rpc-server-gss.html) -- Architecture documentation
- [NFS-Ganesha RPCSEC_GSS wiki](https://github.com/nfs-ganesha/nfs-ganesha/wiki/RPCSEC_GSS) -- Userspace NFS GSS patterns
- [NFS-Ganesha ntirpc auth_gss.c](https://github.com/nfs-ganesha/ntirpc/blob/305482a4b84658f8d667cb1a85c78f6c2c133e65/src/auth_gss.c) -- RPCSEC_GSS wire protocol in C
- [NFS-Ganesha GSS context race condition](https://lists.nfs-ganesha.org/archives/list/devel@lists.nfs-ganesha.org/thread/RP5M2PSJTN3KTXFBNJ57SQE4BXDEK2T6/) -- Critical bug report
- [JuiceFS Kerberos docs](https://juicefs.com/docs/cloud/hadoop/for-kerberos/) -- Hadoop-layer Kerberos
- [JuiceFS keytab support issue #3283](https://github.com/juicedata/juicefs/issues/3283) -- Keytab vs credential cache
- [MinIO gokrb5 v7 fork](https://pkg.go.dev/github.com/minio/gokrb5/v7) -- Go Kerberos in production
- [Samba Security Documentation](https://wiki.samba.org/index.php/Samba_Security_Documentation) -- GENSEC architecture
- [Samba kerberos-notes.txt](https://github.com/samba-team/samba/blob/master/source4/auth/kerberos/kerberos-notes.txt) -- Kerberos design decisions
- [go-nfs by willscott](https://github.com/willscott/go-nfs) -- No RPCSEC_GSS support confirmed
- [RFC 2203 -- RPCSEC_GSS](https://www.rfc-editor.org/rfc/rfc2203) -- Implementation guidance sections
- [RFC 2623 -- NFSv3 + RPCSEC_GSS](https://www.rfc-editor.org/rfc/rfc2623) -- NFS-specific security requirements
- [Linux NFS ID Mapper docs](https://docs.kernel.org/admin-guide/nfs/nfs-idmapper.html) -- Principal-to-UID/GID mapping

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- gokrb5 already in go.mod, API verified via pkg.go.dev docs
- Architecture: HIGH -- RPCSEC_GSS is well-specified by RFC 2203, integration points clearly identified in codebase
- Wire protocol: HIGH -- All XDR types from RFC 2203, token formats from RFC 4121
- Pitfalls: HIGH -- Based on RFC specification + NFS-Ganesha implementation experience
- Identity mapping: MEDIUM -- Strategy choices depend on deployment, but interface is clear
- krb5p encryption details: MEDIUM -- gokrb5 WrapToken handles it, but integration with RPC reply framing needs validation
- Competitive analysis: HIGH -- Linux kernel and NFS-Ganesha patterns verified from source; MinIO/Samba from official docs

**Research date:** 2026-02-15
**Valid until:** 2026-04-15 (stable domain, RFCs don't change)
