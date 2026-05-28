# Phase 14: Control Plane v2.0 - Context

**Gathered:** 2026-02-16
**Status:** Ready for planning

<domain>
## Phase Boundary

Add NFSv4 and SMB adapter configuration to the control plane. Per-share security policies with auth flavor control, identity squashing, and IP-based access restrictions via netgroups. Adapter-level settings for timeouts, version negotiation, transport tuning, operation blocklists, and delegation policy. All exposed via REST API and dittofsctl CLI with hot-reload.

RBAC migration is Phase 14.1 (separate). This phase uses existing admin auth for new endpoints.

</domain>

<decisions>
## Implementation Decisions

### Per-Share Security Policy
- Security policy uses **boolean toggles** (not flavor lists) for cross-adapter compatibility: `allow_auth_sys`, `require_kerberos`, `min_kerberos_level`
- Default posture: **AUTH_SYS allowed** for shares without explicit security config
- Security + squashing (root_squash, all_squash) **grouped together** as the share's "access policy" (extends existing ShareAccessPolicy)
- Kerberos required on a share but no keytab configured: **refuse to start share** (fail-fast)
- Security policy changes take effect **immediately** (hot-reload). New connections use updated policy
- Existing connections are **grandfathered** when policy tightens (no disconnect of active AUTH_SYS sessions)
- **Audit trail** via structured server log entries (INFO level) for all security policy changes — log who/what/when

### Netgroups (IP Access Control)
- Netgroups as a **first-class API resource** with CRUD operations (stored in DB)
- Support **IPs, CIDRs, and DNS hostnames** (including wildcards like *.example.com)
- Hostname matching via **reverse DNS only** (PTR lookup, not FCRDNS)
- Empty allowlist = **allow all** (no IP restrictions). Add entries to restrict
- Security flavors are **share-level only**, not per-netgroup. Netgroups control which IPs can connect; share settings control how they authenticate
- Netgroups are shared resources — one netgroup can be referenced by multiple shares

### NFSv4 Adapter Settings
- Version negotiation: **min/max version range** (min_version, max_version)
- Lease and grace period: **per-adapter (global)**, not per-share
- **Extended timeout set** exposed: lease_time (default 90s), grace_period (default 90s), delegation_recall_timeout (default 90s), callback_timeout (default 5s), lease_break_timeout (default 35s), max_connections
- max_compound_ops: **configurable** (default 50), DoS protection
- max_clients: **configurable** (default 10000), memory protection
- Transport tuning: max_read_size, max_write_size, preferred_transfer_size exposed
- Pseudo-fs root always '/' (not configurable)
- Global log level only (no per-adapter log level override)

### SMB Adapter Settings (Matching NFS Parity)
- SMB gets **equivalent knobs**: max_connections, session_timeout, oplock/lease_break_timeout, max_sessions
- **Min/max dialect** negotiation control (SMB2.0, SMB2.1, SMB3.0, SMB3.1.1)
- **enable_encryption toggle (stub)** — config knob present, logs "not yet implemented". Ready for future SMB3 encryption

### Delegation Policy
- **Configurable** at adapter level: delegations_enabled=true/false (default true)
- Common troubleshooting knob for multi-client workloads

### Operation Blocklist
- Per-adapter **and** per-share operation blocklist
- Adapter sets baseline disabled ops, shares can add to it
- Disabled ops return NFS4ERR_NOTSUPP

### API Design
- Adapter settings: **nested under adapter** — `PUT /adapters/nfs/settings`
- Security policy: **part of share CRUD** body (extends existing share model)
- Netgroups: top-level resource — `GET/POST/DELETE /netgroups`
- Both **PATCH (partial) + PUT (full replace)** for adapter settings
- Validation returns **per-field errors**: `{ "errors": { "lease_time": "must be >= 30s" } }`
- **Strict range validation** with **--force flag** to bypass (logs warning)
- **Defaults endpoint**: `GET /adapters/nfs/settings/defaults` returns all settings with default values and valid ranges
- **Granular permissions**: new `manage-settings` permission (Phase 14.1 will integrate into RBAC)
- Admin users/groups automatically have manage-settings permission

### CLI Design
- Adapter settings: `dittofsctl adapter nfs settings show/update/reset`
- Netgroups: `dittofsctl netgroup create/list/delete/add-member/remove-member` (top-level)
- Settings display: **config-style view** (grouped key-values with sections) for `show`, table for `list`
- Non-default values **marked with '*'** in CLI output
- `dittofsctl adapter nfs settings reset [--setting lease_time]` — reset all or specific settings
- **--dry-run flag** for settings updates (validates without applying)
- No import/export — backup handled by existing `dittofs backup controlplane`

### Default Values & Migration
- Adapter creation via API **automatically creates** default settings record
- Existing adapters: **DB migration auto-populates** default settings
- Existing shares: **migration auto-populates** default security policy (AUTH_SYS allowed, no Kerberos required, no IP restrictions)
- GORM auto-migrate for schema changes
- API is sole source of truth (config file does not define adapters — adapters are API-managed, docs need updating per issue #120)

### Settings Hot-Reload
- **Polling interval: 10 seconds** — adapter checks DB for setting changes
- New connections use updated settings; existing connections grandfathered

### Testing
- Full **E2E tests with real NFS + SMB mounts** for both adapters
- One **full lifecycle test**: create adapter -> set settings -> create share with security policy -> mount -> verify behavior
- Plus **focused scenario tests**: Kerberos required rejects AUTH_SYS, lease_time change takes effect, IP allowlist blocks unauthorized client, etc.
- E2E tests must be **fast enough for CI**

### Code Structure
- Extend existing adapter model in controlplane with richer settings fields
- Extend existing **ShareAccessPolicy** with security fields
- Add **Netgroup** and **NetgroupMember** as new GORM models
- GORM auto-migrate for schema changes

### Claude's Discretion
- Exact API response format and HTTP status codes
- Internal settings storage schema design
- Polling implementation details
- Prometheus metrics for control plane operations (settings changes, RBAC events)
- Settings hot-reload thread safety approach
- E2E test helper design

</decisions>

<specifics>
## Specific Ideas

- CLI command structure: `dittofsctl adapter nfs settings show/update/reset` (adapter type first, then settings action)
- Netgroups like NFS netgroups concept but as API resources, not /etc/netgroup
- Security policy extends existing ShareAccessPolicy model
- SMB encryption toggle is a stub — "not yet implemented" log message when enabled
- Adapters already API-managed (not config file), docs outdated per issue #120

</specifics>

<deferred>
## Deferred Ideas

- **Phase 14.1: RBAC Migration** — Role-based access control (admin/operator/viewer), migrate from is_admin flag and legacy permissions, per-user and per-group roles, manage-settings permission folded into RBAC. 14.1 depends on 14. RBAC controls controlplane operations; share-level permissions control filesystem operations (two separate layers).
- **Documentation update** (issue #120) — Config file no longer defines adapters, docs need updating

</deferred>

---

*Phase: 14-control-plane-v2-0*
*Context gathered: 2026-02-16*
