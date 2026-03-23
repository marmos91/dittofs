# Phase 71: Operational Visibility - Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in CONTEXT.md — this log preserves the alternatives considered.

**Date:** 2026-03-22
**Phase:** 71-operational-visibility
**Areas discussed:** Client identity model, Staleness and cleanup, API unification, Disconnect semantics, Code organization and design

---

## Client Identity Model

| Option | Description | Selected |
|--------|-------------|----------|
| One record per session/mount | NFS: one per mount (v3) or stateful client (v4). SMB: one per session. Same IP with both protocols = 2+ records. | ✓ |
| One record per IP, aggregate | Group all NFS+SMB from same IP into single record with sub-entries. | |
| One record per TCP connection | Every TCP socket is a client. Very granular. | |

**User's choice:** One record per session/mount
**Notes:** Recommended as simplest mapping to what each protocol already tracks.

---

## Staleness and Cleanup

| Option | Description | Selected |
|--------|-------------|----------|
| TCP disconnect only | Remove when TCP closes. No TTL. If socket open, client exists. | |
| Inactivity TTL | Mark stale after N min no requests. Risk: idle NFS mounts pruned. | |
| Hybrid: TCP + ghost TTL | TCP disconnect primary. TTL sweeper for ghost entries (unclean disconnect). | ✓ |

**User's choice:** Hybrid approach
**Notes:** User said "3 is the most accurate" and asked for recommendation. Claude confirmed hybrid is best — TCP disconnect as primary signal, TTL (default 5 min) only for ghost entries from unclean disconnects. Idle NFS mounts stay listed.

---

## API Unification

| Option | Description | Selected |
|--------|-------------|----------|
| New /api/v1/clients + keep old | Unified endpoint alongside existing NFS-specific endpoint for backwards compat. | |
| Replace old with unified only | Remove /api/v1/adapters/{type}/clients, replace with /api/v1/clients. Breaking change. | ✓ |
| Unified only, old redirects | New unified endpoint. Old returns 301 redirect. Gradual migration. | |

**User's choice:** Replace old with unified only
**Notes:** Breaking change accepted. Clean API surface preferred over backwards compatibility.

---

## Disconnect Semantics

| Option | Description | Selected |
|--------|-------------|----------|
| Yes, with confirmation prompt | dfsctl client disconnect with interactive confirmation. Emergency admin tool. | |
| Yes, no prompt (--force) | Default requires --force or interactive confirm for scripting. | |
| No, list-only for now | Phase 71 is visibility only. Disconnect is separate concern. | |

**User's choice:** Yes, admin-only
**Notes:** User specified "the permission to perform this action should be limited to admins." Role-based access control enforced at API level.

---

## Code Organization — Registry Location

| Option | Description | Selected |
|--------|-------------|----------|
| pkg/controlplane/runtime/clients/ | New runtime sub-service, same pattern as mounts/service.go. | ✓ |
| pkg/adapter/ shared interface | Define ClientRegistry interface in adapter package. | |
| Embedded in mounts/service.go | Extend MountTracker to also track client metadata. | |

**User's choice:** pkg/controlplane/runtime/clients/
**Notes:** Natural addition to existing runtime sub-service pattern.

---

## Code Organization — Registration Pattern

| Option | Description | Selected |
|--------|-------------|----------|
| Callback interface on Runtime | Runtime exposes RegisterClient/DeregisterClient. Adapters call in accept/close. | ✓ |
| Event bus / observer | Adapters emit events. Registry subscribes. More extensible but heavier. | |
| Polling from registry | Registry queries adapters periodically. No adapter changes but wasteful. | |

**User's choice:** Callback interface on Runtime
**Notes:** Same pattern as mount Record/Remove. Minimal coupling.

---

## Code Organization — Data Model

| Option | Description | Selected |
|--------|-------------|----------|
| Runtime-only in-memory | sync.Map in ClientRegistry. Lost on restart, rebuilt on reconnect. | ✓ |
| Persisted in GORM | GORM model in SQLite/PostgreSQL. Survives restarts. | |

**User's choice:** Runtime-only in-memory
**Notes:** Matches transient nature of connections and MountTracker pattern.

---

## Code Organization — Protocol Details

| Option | Description | Selected |
|--------|-------------|----------|
| Flat struct + Metadata map | Common fields as struct. Protocol-specific in map[string]any. | |
| Protocol-typed sub-structs | Optional NfsDetails and SmbDetails sub-structs. Strongly typed. | ✓ |
| Common fields only | Only protocol-agnostic fields. Simplest but loses debug info. | |

**User's choice:** Protocol-typed sub-structs
**Notes:** User asked "2 would be more secure, no?" Claude confirmed — type safety, json:"-" tags for sensitive fields, self-documenting API schema.

---

## Claude's Discretion

- ClientID format
- Exact NfsDetails/SmbDetails field selection
- TTL sweeper implementation
- CLI table columns and formatting
- REST API response structure

## Deferred Ideas

None — discussion stayed within phase scope.
