# Phase 36: Kerberos SMB3 Integration - Context

**Gathered:** 2026-03-02
**Status:** Ready for planning

<domain>
## Phase Boundary

Domain-joined Windows clients authenticate via Kerberos/SPNEGO with proper SMB3 key derivation, with NTLM and guest fallback. This phase builds the shared Kerberos service layer (used by both NFS and SMB), integrates it into the SMB SESSION_SETUP handler, and migrates the existing NFS GSS code to use the shared service.

</domain>

<decisions>
## Implementation Decisions

### Session Key Extraction
- Feed Kerberos session key into the existing `configureSessionSigningWithKey()` pipeline (same path as NTLM)
- Truncate/pad session key to 16 bytes regardless of Kerberos encryption type (AES128=16B, AES256=32B, DES=8B) — standard SMB2 key normalization
- Prefer authenticator subkey over ticket session key per MS-SMB2 3.3.5.5.3
- Reuse Phase 35's supported encryption algorithm list for validation — no separate config
- Handle mixed encryption types (authenticator subkey vs ticket) transparently via gokrb5
- Store Kerberos-derived keys identically to NTLM (via `sess.SetCryptoState()` / `sess.SetSigningKey()`) — add metadata field for auth mechanism in logging only
- Auto-derive CIFS SPN from NFS principal (`nfs/host@REALM` -> `cifs/host@REALM`) with optional `smb_service_principal` config override
- Single keytab file containing both `nfs/` and `cifs/` service principal entries — document this requirement

### Mutual Authentication (AP-REP)
- Always generate a real AP-REP token (not nil) and include it in the SPNEGO accept-complete response
- Match the client's Kerberos OID in the SPNEGO response (MS OID if client used MS OID, standard OID if client used standard)
- Include SPNEGO mechListMIC for downgrade protection — bidirectional: verify client MIC if present, then generate server MIC
- Use gokrb5's AP-REP builder if available; fall back to manual construction if server-side API is incomplete
- Trust gokrb5 for ticket lifetime validation — no additional near-expiry checks
- Implement replay cache for Kerberos authenticators in the shared service (`internal/auth/kerberos/`)

### NTLM Fallback Strategy
- On Kerberos failure: return SPNEGO NegState=reject; client retries with new SESSION_SETUP (fresh session ID) using NTLM
- NTLM enable/disable configurable as adapter-level setting in the control plane (not global config file)
- When Kerberos provider is not configured (no keytab): only advertise NTLM in NEGOTIATE response SPNEGO hints
- Kerberos-only mechList failure: return STATUS_LOGON_FAILURE directly (no SPNEGO wrapping)
- Fresh SESSION_SETUP on fallback (SessionId=0) — clean state, no stale Kerberos context leaking into NTLM
- Log failed Kerberos attempts at INFO level with context (principal, error reason, client IP)

### Guest Session Behavior
- Adapter-level `guest_enabled: true/false` config in control plane (default: true)
- Valid Kerberos ticket but unknown principal (no matching DittoFS user): hard failure (STATUS_LOGON_FAILURE)
- Expired Kerberos ticket: hard failure (STATUS_LOGON_FAILURE) — client must renew with KDC
- Guest access controlled by share permissions — guest identity checked against share permission model
- Reject guest sessions when server requires signing (`signing.required: true`) — can't sign without session key
- Anonymous (empty security buffer) and failed-auth both treated as guest — same `SMB2_SESSION_FLAG_IS_GUEST` behavior
- Log INFO hint about Windows 11 24H2 insecure guest logon policy when guest session is created

### Code Structure and NFS Co-existence (ARCH-03)
- **`pkg/auth/kerberos/`** — Provider (keytab, config, SPN) + Kerberos identity mapping (principal-to-user resolution with strip-realm default + configurable explicit mapping table)
- **`internal/auth/kerberos/`** — NEW shared KerberosService (broader service object pattern): `Authenticate(apReq) -> AuthResult`, `BuildMutualAuth(creds) -> apRep`, built-in replay cache, session key extraction with subkey preference and size normalization
- **`internal/adapter/nfs/rpc/gss/`** — Refactored to call `internal/auth/kerberos/` for AP-REQ verification and key extraction; keeps NFS-specific RPCSEC_GSS wiring (MIC, integrity, privacy, context store, sequence window)
- **`internal/adapter/smb/v2/handlers/kerberos_auth.go`** — NEW dedicated file for SMB Kerberos auth (extracted from session_setup.go); calls shared KerberosService, handles SPNEGO wrapping
- **`internal/adapter/smb/auth/spnego.go`** — Extended with MIC computation helpers (GetMIC/VerifyMIC)
- **`internal/auth/kerberos/replay.go`** — Replay cache (in-memory, TTL-based, keyed by principal+ctime+cusec)
- Migrate NFS GSS code to use shared KerberosService in this phase (not deferred)

### Claude's Discretion
- Whether to fail auth entirely or succeed without AP-REP when AP-REP generation fails
- Server subkey in AP-REP: follow Samba's behavior (likely no server subkey)
- Handling of 3-leg SPNEGO continuation tokens from clients
- Mixed encryption type handling details (delegated to gokrb5 transparently)
- Credential delegation: skip for now (niche scenario)
- Per-client auth failure rate limiting: defer (existing server-level rate limiter covers this)

</decisions>

<specifics>
## Specific Ideas

- "Internal protocol logic should still reside in internal. pkg should only contain logic that makes sense to expose to the public"
- "In the public pkg it should also exist the connection between Kerberos and DittoFS authentication and permission logic"
- Phase 35 (on branch `feat/v3.8-smb3-encryption-transform-header`) has the supported encryption algorithm list that Phase 36 should reuse
- Principal-to-username mapping: strip realm as default (`alice@REALM` -> `alice`), plus optional explicit mapping table in config for non-standard setups

</specifics>

<code_context>
## Existing Code Insights

### Reusable Assets
- `pkg/auth/kerberos/Provider` — Shared keytab management, hot-reload, SPNEGO/AP-REQ detection, service principal config
- `internal/adapter/smb/auth/spnego.go` — Full SPNEGO parser (NegTokenInit/Resp, Kerberos/NTLM OID detection, BuildAcceptComplete/BuildReject)
- `internal/adapter/smb/kdf/` — SP800-108 KDF already implements all 4 key derivations (signing, encryption, decryption, application)
- `internal/adapter/smb/signing/` — HMAC-SHA256, AES-CMAC, AES-GMAC signers fully implemented
- `internal/adapter/smb/v2/handlers/session_setup.go:configureSessionSigningWithKey()` — Full SMB 2.x/3.x signing pipeline (KDF branching, CryptoState setup)
- `internal/adapter/smb/v2/handlers/session_setup.go:handleKerberosAuth()` — Existing skeleton that validates AP-REQ, maps principal, creates session (needs session key extraction + AP-REP + signing)
- `internal/adapter/nfs/rpc/gss/` — Full RPCSEC_GSS implementation (context management, MIC computation, replay detection via SeqWindow, integrity/privacy wrapping)
- `pkg/auth/identity.go` — Protocol-neutral Identity model with Principal field and IdentityMapper interface

### Established Patterns
- gokrb5 v8 used throughout: `service.VerifyAPREQ()`, `messages.APReq`, `keytab.Keytab`, `spnego.UnmarshalNegToken()`
- Auth flow: parse SPNEGO -> detect mechanism -> delegate to mechanism handler -> map to DittoFS user -> create session
- Session key storage: `session.Session.SetCryptoState()` for SMB 3.x, `sess.SetSigningKey()` for SMB 2.x
- Handler pattern: `Handler` struct with registry, signing config, session management; methods return `*HandlerResult`

### Integration Points
- `handleKerberosAuth()` in session_setup.go — primary integration point for SMB (already routing Kerberos tokens)
- `pkg/auth/kerberos/Provider` — shared across NFS and SMB adapters
- `gss/framework.go` — NFS RPCSEC_GSS INIT handler uses same AP-REQ verification flow that will be shared
- `session.DeriveAllKeys()` — Called from `configureSessionSigningWithKey()` for SMB 3.x key derivation
- Control plane adapter config — NTLM enable/disable and guest enable/disable settings

</code_context>

<deferred>
## Deferred Ideas

- Prometheus metrics for auth mechanism counters (smb_auth_kerberos_total, smb_auth_ntlm_total, etc.) — no metrics infrastructure yet
- Kerberos credential delegation (forwarded TGT, constrained delegation, S4U2Proxy) — niche scenario, future phase
- Per-client auth failure rate limiting — existing server-level rate limiter covers this for now

</deferred>

---

*Phase: 36-kerberos-smb3-integration*
*Context gathered: 2026-03-02*
