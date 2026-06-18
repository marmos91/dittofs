# DittoFS — Active Directory / LDAP Enterprise Integration

## Context

A filesystems expert told us DittoFS must be LDAP / Active Directory compatible to be
enterprise-friendly, especially on Windows. This plan validates that and turns it into an
executable roadmap.

**Why it matters.** The enterprise NAS bar (NetApp ONTAP, Dell PowerScale, Qumulo) is *full AD
domain membership* + *multi-protocol identity unification* (the same user, accessing via NFS
with a UID/GID and via SMB with a SID, sees the same files with the same permissions). The
modern cloud-native competitors DittoFS resembles — JuiceFS, CephFS, MinIO — do **not** solve
clean multi-protocol AD. That gap is a differentiator DittoFS can own.

**Where we are (from a full codebase audit).** DittoFS is *not* greenfield here. The
foundation is deliberately laid:

- `pkg/identity/` — pluggable `IdentityProvider` chain + resolver cache + singleflight. A
  Kerberos provider exists (`pkg/identity/kerberos/provider.go`). No LDAP/AD provider yet.
- `pkg/metadata/auth_identity.go` — `AuthContext.Identity` is already dual-stack: Unix
  (`UID/GID/GIDs`) **and** Windows (`SID/GroupSIDs`).
- `pkg/metadata/acl/` — a genuinely good, protocol-agnostic ACL core. `acl.Evaluate()` /
  `acl.EvaluateGranted()` feed one `EvaluateContext` that understands both Unix and Windows
  principals with no protocol branching. NFSv4 ACL ↔ Windows DACL ↔ POSIX mode all
  canonicalize to one in-memory `acl.ACL`.
- `pkg/auth/sid/mapper.go` — algorithmic SID↔UID/GID mapping (machine SID + RID formula).
- **ACL persistence is already solid** (verified): `acl.ACL` persists durably on postgres
  (`acl JSONB`, migration `000004`) and badger (File JSON blob), read back as-is on GetFile (not
  re-synthesized from mode), proven by `storetest/acl_aliasing.go`. Memory backend is volatile
  by design (test-only). So no ACL-persistence work needed — the durability gap is the
  *identity/idmap* layer below, not the ACL.
- SMB NTLMv2 + Kerberos AP-REQ verification (gokrb5) both work. Real-KDC integration tests
  already exist (`test/integration/kerberos/`, `test/e2e/{smb,nfsv4}_kerberos_test.go`).

**The gaps to close.** No LDAP client; no AD-sourced identity; Kerberos PAC decoding is
explicitly **off** (`internal/auth/kerberos/service.go` — `DecodePAC(false)`), so AD group SIDs
never flow; SID/GroupSIDs are not persisted; no domain join; SID↔UID/GID is algorithmic only
(doesn't match the UID/GID an AD shop has already provisioned).

**Cross-protocol correctness problems the audit surfaced (must fix first).** AD is worthless if
the two protocols enforce its identities differently:
1. **NFSv4 `ACCESS` bypasses the core.** `internal/adapter/nfs/v4/handlers/access.go`
   reimplements a Unix-mode-only `checkAccessBits()` — it ignores ACLs, DENY ACEs, and all
   SID-based grants. SMB and NFSv3 route through the core; NFSv4 does not. Same file → different
   answer per protocol.
2. **Two entry points** — `CheckPermissions()` (NFS, generic flags) vs `CheckFileAccess()`
   (SMB, MS-DTYP bits). Both reach `acl.Evaluate()` but via separate adapters → drift risk.
3. **Adapters call `acl.Evaluate()` directly** (SMB `query_directory.go`), violating the
   "handlers do protocol only" invariant.
4. **Per-op gates lean on cached `GrantedAccess`** for SMB READ/WRITE/DELETE/traverse rather
   than re-evaluating through one checker.

**Intended outcome.** A thin, protocol-agnostic permission/identity core that every NFS
(v3/v4.0/v4.1) and SMB (2.0.2–3.1.1) operation funnels through, plus an AD/LDAP identity
provider feeding it — so a Leonardo-class shop can point DittoFS at their AD and have Windows
*and* Linux users see one consistent, correct filesystem.

## Decisions (locked with user)

| Decision | Choice |
|----------|--------|
| First shippable tier | **Tier 2 + idmap** — Kerberos/NTLM auth vs AD + LDAP lookup + SID↔UID/GID unification (not just auth) |
| idmap strategy | **RFC2307 from AD (read `uidNumber`/`gidNumber` via LDAP), RID algorithmic fallback** for objects lacking Unix attrs |
| Domain credential | **Offline keytab import now** (admin pre-creates computer account); **online `net ads join` (MS-RPC) later** |
| Delivery | **Sequenced PRs**, each gated by a dockerized Samba AD-DC CI fixture |
| Testing | **Docker Samba AD-DC for every-PR CI**; **temporary Scaleway VM + real Windows client + AD for milestone acceptance** (mirrors the proven TLS validation playbook), then terminate VM |

## Design — the thin common layer

Principle (already a repo invariant): **protocol handlers do protocol only; all permission and
identity decisions live in `pkg/metadata` + the stores.** The audit shows the core mostly
exists — this is consolidation + one bug fix, not a rewrite.

```
NFS v3/v4.0/v4.1 handlers          SMB 2.x/3.x handlers
        |  (translate protocol bits only)  |
        v                                   v
   metadata.FileAccessChecker  <-- single entry point -->
        |
        v
   acl.Evaluate / acl.EvaluateGranted   (one EvaluateContext: UID/GID/GIDs + SID/GroupSIDs)
        ^
        |
   identity.Resolver  ->  [ Kerberos provider | NEW: AD/LDAP provider ]
        ^
        |
   AuthContext.Identity  (UID/GID/GIDs + SID/GroupSIDs, populated per protocol + from AD/PAC)
```

- **One checker interface** in `pkg/metadata` (consolidate `CheckPermissions` and
  `CheckFileAccess` behind it): `CheckFileAccess(file, authCtx, requestedMask) (granted, err)`,
  `CheckDeleteAccess`, `CheckExecuteAccess`. Every adapter op calls it; adapters only translate
  their wire access-bits ↔ the canonical MS-DTYP/RFC7530 mask vocabulary (which already aligns).
- **Identity stays canonical** in `AuthContext.Identity`. Adapters never make identity
  decisions; they populate from their wire creds, and the resolver enriches from AD.
- **AD provider** implements the existing `identity.IdentityProvider` interface and slots into
  the resolver chain — zero new plumbing in handlers.

## Standards / references to follow

- **RFC 4120** (Kerberos v5), **RFC 4178** (SPNEGO), **RFC 2307 / 2307bis** (POSIX LDAP schema:
  `uidNumber`, `gidNumber`), **RFC 7530 §6 / RFC 8881** (NFSv4 ACL ACE4 model — already our
  canonical form), **MS-DTYP** (SID structure, security descriptors, owner-implicit rights),
  **MS-SMB2** (SD query/set access gating), **MS-PAC** (Kerberos PAC group SIDs).
- **Reference implementation: Samba/winbind** — study `idmap_ad` (RFC2307), `idmap_rid`, machine
  password rotation, name-mapping. It is the only mature open-source AD domain member; mirror its
  semantics for interop.
- **Go libraries:** `gokrb5` (already used; AP-REQ verify, keytab, PAC decode — *enable it*),
  `go-ldap/ldap/v3` (LDAPS + RFC2307 queries). No mature Go MS-RPC lib → online join deferred.

## Delivery plan (sequenced PRs)

Each PR is independently testable and gated by the AD-DC docker fixture.

### PR-0 — Permission-core consolidation + NFSv4 ACCESS fix (PREREQUISITE)
*Cross-protocol correctness before any AD work.*
- Fix NFSv4 `ACCESS`: delete `checkAccessBits()`; route through the central checker like NFSv3
  does (`internal/adapter/nfs/v4/handlers/access.go`).
- Introduce the single `FileAccessChecker` interface in `pkg/metadata`; fold `CheckPermissions`
  + `CheckFileAccess` behind it. Move SMB's direct `acl.Evaluate()` call
  (`smb/handlers/query_directory.go`) behind it.
- Audit + route per-op SMB gates (READ/WRITE/DELETE/traverse) through the checker (or document
  why the cached `GrantedAccess` snapshot is spec-correct per MS-SMB2 — keep what the spec
  mandates, centralize the rest).
- **New cross-protocol conformance suite**: a matrix test (memory/badger/postgres × NFSv3 /
  NFSv4.0 / NFSv4.1 / SMB) asserting the *same* file + ACL (incl. DENY ACEs and SID-based
  grants) yields the *same* allow/deny on every protocol. This is the regression net for
  everything after.

### PR-1 — AD-DC test fixture + enable Kerberos PAC
- Dockerized **Samba AD-DC** (KDC + LDAP + AD schema) as a reusable CI fixture; wire into the
  event-tiered CI (postsubmit/nightly to start; presubmit if fast enough).
- Flip `DecodePAC(true)` in `internal/auth/kerberos/service.go`; extract PAC group SIDs into the
  Kerberos `AuthResult` → `AuthContext.Identity.GroupSIDs`. (Biggest single unlock — AD nested
  groups resolved by the DC, delivered in the ticket; no LDAP group-walk at scale.)
- Tests: real AD-issued ticket → PAC groups appear in the identity.

### PR-2 — LDAP/AD identity provider
- `pkg/identity/ldap/` (or `ad/`) implementing `identity.IdentityProvider`: connect over
  **LDAPS / signed LDAP** (defense shops reject plaintext), bind via machine keytab or service
  account, resolve user/group, read **RFC2307** `uidNumber`/`gidNumber`, resolve (nested) group
  membership. Slot into the resolver chain.
- Wire `LinkStore` persistence (control-plane GORM table: `(provider, external_id) → user`).
- Config surface: `dfsctl`/REST + operator CRD for LDAP URL, base DN, bind creds, TLS, idmap
  mode. Regenerate `docs/CLI.md` via `cmd/gendocs`.

### PR-3 — Core durable identity + idmap layer (provider-agnostic)
*Not LDAP-specific. A single core concern consumed by every identity provider — Kerberos-PAC,
LDAP/AD, local users, future OIDC — so identity resolution never drifts per provider.*

Two SID classes, different persistence needs:
- **Local / algorithmic SIDs** (machine SID + RID derived from UID; local users + RID fallback).
  Deterministic → reproducible from UID. Persistence optional. **Contract:** the algorithm must
  stay stable across **restart AND across cluster nodes** (the Samba tdb portability trap — if
  two nodes derive different UIDs for the same SID, a persisted ACL means different things on
  each node). Lock the machine SID + RID formula as a durable, node-shared invariant.
- **Foreign / AD domain SIDs** (`S-1-5-21-<AD-domain>-<RID>` from PAC groups or LDAP). NOT
  derivable from a local UID — binding is external (RFC2307) or allocated. **MUST be persisted
  durably.** Otherwise after restart a persisted ACL's foreign SID either can't resolve, or a
  re-allocation maps it to a *different* UID → the ACL silently points at the wrong user
  (data-exposure bug).

Work:
- idmap resolver in the core: RFC2307 attrs first; algorithmic RID fallback (`pkg/auth/sid/`).
- Persist `SID`/`GroupSIDs` on the user model (the `user.go` "future" columns) + a durable
  SID↔UID/GID cache table (3-backend, with a `storetest` conformance case proving round-trip +
  restart stability for foreign SIDs).
- Populate the canonical `AuthContext.Identity` / `EvaluateContext` identically regardless of
  which provider produced it and which protocol (NFS UID/GID or SMB SID) the request arrived on
  — the unification guarantee. **Coupled to PR-1:** PAC-delivered foreign group SIDs depend on
  this durable idmap the moment `DecodePAC` is enabled.

### PR-4 — End-to-end keytab Kerberos (SMB + NFS) against AD + acceptance
- Offline keytab config for the SMB server principal + NFS RPCSEC_GSS; SPNEGO Kerberos as the
  primary path, NTLM fallback. Domain-aware SMB session (replace hardcoded `WORKGROUP`).
- **Scaleway VM acceptance**: real Windows client joined to the docker/VM AD — mount over SMB,
  open the Explorer Security tab, `whoami /groups`, set/inspect ACLs; Linux NFSv4.1 + Kerberos
  mount of the same share; assert the same user sees consistent ownership/permissions
  cross-protocol. Terminate VM after.

### Later (out of scope for this milestone)
- Online `net ads join` via MS-RPC (lsarpc/samr) + machine-password rotation — the NetApp-grade
  UX. Revisit when a pilot demands automated join.
- LSA name lookup (SID→name) so Explorer shows `DOMAIN\user` instead of raw SIDs.

## Critical files

- `internal/auth/kerberos/service.go` — `DecodePAC(false)` → `true`; extract PAC group SIDs.
- `internal/adapter/nfs/v4/handlers/access.go` — remove `checkAccessBits()`, route to core.
- `pkg/metadata/auth_permissions.go` — unify `CheckPermissions`/`CheckFileAccess` behind one
  `FileAccessChecker`.
- `pkg/metadata/acl/{evaluate.go,types.go}` — keep as-is (canonical); reused, not changed.
- `pkg/identity/{identity.go,resolver.go,store.go}` + new `pkg/identity/ldap/` — AD provider.
- `pkg/controlplane/models/user.go` — persist `SID`/`GroupSIDs` (the "future" columns) + idmap.
- `pkg/auth/sid/mapper.go` — RID fallback inside the RFC2307-first idmap.
- `internal/adapter/smb/handlers/{session_setup.go,auth_helper.go}` — domain-aware session.
- `cmd/dfsctl` + REST + operator CRD + `cmd/gendocs` — config surface + docs.

## Execution model — GitHub issues + parallel worktree agents

Track as an **umbrella issue** + one sub-issue per PR. Implementation happens later via multiple
agents on isolated git worktrees, parallel where dependencies allow.

**Umbrella issue:** "Active Directory / LDAP enterprise integration (Tier 2 + multi-protocol
idmap)" — links the design (this plan), the decisions table, the competitive context, and all
sub-issues with the dependency graph below.

**Sub-issues (1:1 with PRs):**
- `#AD-0` Permission-core consolidation + NFSv4 ACCESS fix + cross-protocol conformance matrix
- `#AD-1` Dockerized Samba AD-DC CI fixture + enable Kerberos PAC (group SIDs)
- `#AD-2` LDAP/AD identity provider (LDAPS, RFC2307, nested groups) + LinkStore persistence
- `#AD-3` Core durable identity + idmap layer (provider-agnostic; foreign-SID persistence)
- `#AD-4` End-to-end keytab Kerberos (SMB+NFS) + domain-aware session + Windows acceptance
- `#AD-pcap` (cross-cutting) Reference pcap capture corpus + diff harness (see below)

**Dependency graph / waves (for parallel agents):**
```
Wave 1 (parallel — non-overlapping areas):
  AD-0  (pkg/metadata permission core + nfs/smb handlers)
  AD-1  (internal/auth/kerberos + CI fixture)        ── independent of AD-0
Wave 2 (parallel, after AD-1 fixture exists):
  AD-2  (pkg/identity/ldap)        ┐ light coupling at resolver chain —
  AD-3  (pkg/auth/sid + user model)┘ split files cleanly or sequence the resolver wiring
        (AD-3 unblocks AD-1's PAC group SIDs end-to-end)
Wave 3 (after all):
  AD-4  (e2e wiring + acceptance)
AD-pcap: runs alongside every wave as the correctness oracle.
```
Worktree isolation per agent (the established pattern); each sub-issue's PR is independently
CI-gated by the AD-DC fixture. AD-0 must merge before AD-4; AD-1+AD-3 must both land before
PAC groups work end-to-end.

## Correctness methodology — pcap-diff against official protocols (cross-cutting)

Builds on the existing `docs/DEBUGGING.md` SMB/NFS pcap-diff playbook (same method that root-caused
the macOS NFSv4.1 panic). For every AD-touching feature, capture the **reference** server's wire
behavior and diff DittoFS against it — bytes don't lie where interop assumptions do.

| Feature | Reference endpoint to capture | Tool |
|---------|------------------------------|------|
| Kerberos AP-REQ / SPNEGO / PAC | Windows client ↔ Windows Server / Samba domain member | tshark, `KRB5_TRACE` |
| LDAP RFC2307 queries | `ldapsearch` / `net ads` ↔ AD | tshark, ldap debug |
| SMB SECURITY_DESCRIPTOR query/set | `icacls`/Explorer ↔ Windows share | smbtorture, tshark |
| NFSv4 fattr4_acl | Linux client ↔ knfsd | tshark, `rpcdebug` |

Endpoints come for free from the test infra: dockerized Samba AD-DC + the scw Windows VM. Capture
once into a corpus (`#AD-pcap`), re-diff in CI/locally during implementation. Divergence from the
real server = the bug.

## Verification

- **Unit/conformance:** `go test -race ./...`; the new cross-protocol permission matrix
  (PR-0) green on memory/badger/postgres for every NFS/SMB version.
- **AD-DC docker fixture (CI, every PR):** spin Samba AD-DC; assert Kerberos auth, PAC groups,
  LDAP/RFC2307 lookup, idmap resolution, SID↔UID/GID round-trip.
- **smbtorture** SD/ACL/maximum_allowed batteries stay green (existing harness).
- **Scaleway VM acceptance (milestone):** real Windows client + Linux NFSv4.1/Kerberos against
  a real AD — same user, both protocols, identical files/owners/ACLs; Explorer Security tab
  works. Terminate VM after.
- **Negative controls:** DENY ACE and SID-only grant must produce identical allow/deny on NFS
  and SMB (the bug class PR-0 closes).
