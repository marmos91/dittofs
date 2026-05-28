# Phase 12: Kerberos Authentication - Context

**Gathered:** 2026-02-15
**Status:** Ready for planning

<domain>
## Phase Boundary

Implement RPCSEC_GSS framework with Kerberos v5 support (krb5/krb5i/krb5p) for NFSv4, SPNEGO for SMB SESSION_SETUP and REST API, per-share security flavor policy, identity mapping with auto-provisioning, and service principal/keytab management. Uses pure Go (gokrb5) with no cgo dependency.

This phase delivers Kerberos authentication across all three DittoFS protocols (NFS, SMB, REST API) via a shared Kerberos layer in `pkg/auth/kerberos/`.

</domain>

<decisions>
## Implementation Decisions

### KDC Integration Model
- External KDC only (MIT, Heimdal, Active Directory) — no embedded test KDC
- Pure Go implementation using jcmturner/gokrb5 library — no cgo, no libgssapi
- Read system krb5.conf for realm/KDC discovery (standard /etc/krb5.conf or KRB5_CONFIG env)
- Cross-realm trust supported for multi-domain AD environments
- Mutual authentication required (server proves identity to client) per RFC 7530
- GSSAPI security contexts cached after initial auth for performance
- Configurable clock skew tolerance (default 5 minutes, matches MIT Kerberos)
- Keytab hot-reload via file watching (supports AD automated key rotation)
- AES encryption types only (AES128-CTS-HMAC-SHA1, AES256-CTS-HMAC-SHA1) — no legacy DES/RC4
- GSS context lifetime matches Kerberos ticket lifetime (no independent TTL)
- Full Prometheus metrics: auth attempts, successes, failures by flavor, context cache size, keytab reload events
- Log all successful Kerberos authentications at INFO level for audit trail (principal, realm, client IP, security flavor)

### Security Flavor Policy
- Per-share configurable flavor list via control plane API (e.g., sec=[krb5p, krb5i, krb5, sys])
- Default to AUTH_SYS for new shares — admin opts shares into Kerberos explicitly
- Server-level `kerberos.enabled` toggle + keytab path + realm config in config.yaml
- Ordered flavor list with negotiation — client picks strongest it supports, server prefers first in list
- Same permissions regardless of auth flavor — flavor controls transport security, not authorization level
- Reject non-allowed flavor with NFS4ERR_WRONGSEC + SECINFO hint (standard NFSv4 behavior)
- No auth rate limiting — rely on KDC lockout policy (Kerberos ticket validation doesn't expose credentials)
- Dedicated `dittofsctl share security set` CLI command for managing share security flavors
- SMB: SPNEGO always offered at session level when Kerberos enabled; share flavor enforcement at TREE_CONNECT
- Share enable/disable toggle — separate from security flavors (deferred to Phase 14)
- Upgrade existing SECINFO handler (Phase 8) to return actual share security flavors

### Identity Mapping
- Auto-map by default (strip realm from principal), with explicit override table for specific principals
- Auto-provision DittoFS user on first Kerberos authentication — no pre-registration required
- Auto-provisioned users join a configurable default group (e.g., `kerberos-users`) — admin pre-configures group permissions
- PAC extraction for UID/GID mapping (full AD integration) — included in Phase 12
- Auto-map AD group memberships from PAC to DittoFS groups — realm-prefixed names (e.g., `CORP.COM/finance-team`)
- Principal with instance preserved: admin/instance@REALM → DittoFS user 'admin/instance'
- User lifecycle: Disable blocks access permanently (no re-provision); Delete removes user, re-provisioned with default group on next login
- Auto-provisioned users tagged with source='kerberos' — visible in `dittofsctl user list`
- Log auto-provisioning events at INFO level (no webhook)

### Service Principal Setup
- Three SPNs in a single keytab: nfs/hostname@REALM, cifs/hostname@REALM, HTTP/hostname@REALM
- Hostname: default to os.Hostname(), overridable via kerberos.hostname config
- Default keytab path: XDG config dir (~/.config/dittofs/dittofs.keytab), overridable
- Keytab also loadable from DITTOFS_KERBEROS_KEYTAB_BASE64 env var (base64-encoded) for container deployments
- Startup validation: warn on missing SPNs but start anyway; fail only if keytab file unreadable
- `dittofsctl kerberos init` CLI helper to generate keytab with all three SPNs (local operation, no auth required)
- `dittofsctl kerberos test` to validate setup end-to-end (requires auth)
- `GET /api/v1/kerberos/status` REST endpoint for monitoring (keytab loaded, SPNs found, realm, active contexts, last auth)

### Code Structure
- Shared Kerberos layer: `pkg/auth/kerberos/` (public — shared by server, CLI, API client)
- RPCSEC_GSS implementation: `internal/protocol/nfs/rpcgss/` (NFS-specific, under NFS tree)
- REST API SPNEGO middleware: `pkg/controlplane/api/middleware/` (alongside existing JWT middleware)
- SMB SPNEGO handler: `internal/protocol/smb/spnego/` (internal, alongside SMB protocol code)
- Extend existing `metadata.AuthContext` with Kerberos fields (Principal, Realm, SecurityFlavor, GSSContext)
- Extend existing RPC dispatch.go with RPCSEC_GSS as new auth flavor (not a pre-processing layer)
- Full RPCSEC_GSS lifecycle: RPCSEC_GSS_INIT, RPCSEC_GSS_CONTINUE_INIT, RPCSEC_GSS_DESTROY

### Testing
- Docker-based testing: Samba AD DC container for realistic AD environment (PAC, groups, SPNEGO)
- testcontainers in Go tests (shared helper) + docker-compose for manual testing
- Integration tests co-located with code using build tags (not separate test/integration/ folder)
- E2E tests (mount-based with Kerberos) in test/e2e/ with -tags=e2e
- `dittofsctl login --kerberos` for SSO login to REST API

### Documentation
- Include basic docs/KERBEROS.md with setup guide, configuration reference, and troubleshooting in Phase 12

### Claude's Discretion
- Offline keytab validation vs online KDC verification for ticket validation
- RPCSEC_GSS window size configuration
- Multiple hostname support in keytab (gokrb5 natural handling)
- SECINFO AUTH_NONE policy for pseudo-fs browsing (per RFC 7530 Section 2.6)
- Group membership refresh strategy (on-auth vs cached with TTL)
- Write encryption policy (follow share policy vs enforce krb5p)
- Debug command necessity (kerberos test + status endpoint may suffice)
- Kerberos login token mode (JWT exchange vs SPNEGO per request)
- Startup config validation strategy (strict vs lazy)

</decisions>

<specifics>
## Specific Ideas

- "DittoFS should be easy to use for amatorial use cases" — Kerberos is opt-in, AUTH_SYS remains the default. Non-Kerberos setups have zero overhead.
- Three-protocol SSO: single keytab serves NFS (RPCSEC_GSS), SMB (SPNEGO at SESSION_SETUP), and REST API (SPNEGO via HTTP Negotiate)
- SMB SPNEGO wired without requiring SMB3 — works with existing SMB2 SESSION_SETUP
- K8s operator should manage keytab secrets (deferred to operator project)
- Config structure: server-level `kerberos:` block for infrastructure, per-share `sec:` list for flavor policy via control plane API
- User model extends existing `Enabled` bool field — no new concepts for disable/enable

</specifics>

<deferred>
## Deferred Ideas

- **Share enable/disable toggle** — General share management feature, belongs in Phase 14 (Control Plane v2.0)
- **K8s operator keytab management** — Operator auto-creates keytab secrets from DittoServer CR spec. Belongs in operator project.
- **Webhook notifications for user provisioning** — Complete webhook flow is a separate future project
- **Auth rate limiting** — May add per-IP rate limiting for failed auth attempts in a future security hardening phase

</deferred>

---

*Phase: 12-kerberos-authentication*
*Context gathered: 2026-02-15*
