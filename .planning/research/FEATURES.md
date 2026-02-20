# Feature Landscape: NFSv4.1 Session Infrastructure

**Domain:** NFSv4.1 session features for existing NFSv4.0 server
**Researched:** 2026-02-20
**Confidence:** HIGH (RFC 8881 specifications, Linux kernel source, nfs4j reference)

## Table Stakes

Features that NFSv4.1 clients expect. Missing any of these means clients cannot use v4.1 at all.

| Feature | Why Expected | Complexity | Notes |
|---------|--------------|------------|-------|
| EXCHANGE_ID (op 42) | Replaces SETCLIENTID for v4.1 | Medium | New client registration path, extends ClientRecord |
| CREATE_SESSION (op 43) | Creates session with slot tables | High | Core new abstraction, channel attrs negotiation |
| DESTROY_SESSION (op 44) | Graceful session teardown | Low | Drain active slots, cleanup maps |
| SEQUENCE (op 53) | First op in every v4.1 COMPOUND | High | Drives EOS, lease renewal, flow control, status flags |
| COMPOUND bifurcation | Minor version 1 dispatch path | High | Split ProcessCompound into v4.0/v4.1 paths |
| Slot table with EOS | Exactly-once semantics | High | Per-slot sequence validation + full reply cache |
| BIND_CONN_TO_SESSION (op 41) | Associate connection with session | Medium | Enables reconnection and trunking |
| RECLAIM_COMPLETE (op 58) | Signal end of reclaim phase | Low | Required by Linux client after session recovery |
| DESTROY_CLIENTID (op 57) | Graceful client cleanup | Low | Remove client and all sessions |
| Backchannel on fore connection | NAT-friendly callbacks | High | Replace separate TCP callback with multiplexed I/O |
| CB_SEQUENCE (cb op 11) | First op in every v4.1 CB_COMPOUND | Medium | Back-channel slot table sequencing |
| Owner-seqid bypass for v4.1 | Skip open-owner seqid validation | Medium | Session slots replace per-owner seqids |
| OPEN_CONFIRM suppression | Not used in v4.1 | Low | Return NFS4ERR_NOTSUPP for minorversion 1 |

## Differentiators

Features that set DittoFS apart. Not strictly required by all clients, but add significant value.

| Feature | Value Proposition | Complexity | Notes |
|---------|-------------------|------------|-------|
| Directory delegations (op 46) | Cache directory listings client-side | High | GET_DIR_DELEGATION + CB_NOTIFY notifications |
| Session trunking (nconnect) | Multiple connections per session for throughput | Medium | BIND_CONN_TO_SESSION with session-level routing |
| FREE_STATEID (op 45) | Clean up abandoned stateids | Low | Allows client to release stateids without CLOSE |
| TEST_STATEID (op 55) | Batch stateid validation | Low | Client checks multiple stateids in one call |
| BACKCHANNEL_CTL (op 40) | Update backchannel security | Low | Allows GSS context refresh on backchannel |
| CB_RECALL_ANY (cb op 8) | Recall any recallable state | Low | Server-driven resource reclamation |
| CB_RECALL_SLOT (cb op 10) | Dynamic slot table resize | Low | Server tells client to use fewer slots |
| SECINFO_NO_NAME (op 52) | Security negotiation without name | Low | Easier security flavor discovery |
| sr_status_flags in SEQUENCE | Rich server-to-client status | Medium | CB_PATH_DOWN, revoked state, lease info |

## Anti-Features

Features to explicitly NOT build in v3.0.

| Anti-Feature | Why Avoid | What to Do Instead |
|--------------|-----------|-------------------|
| pNFS (ops 47-51) | Layout/device operations for parallel I/O | Return NFS4ERR_NOTSUPP; pNFS is out of scope per PROJECT.md |
| SET_SSV (op 54) | SSV-based security for session | Return NFS4ERR_NOTSUPP; rely on RPCSEC_GSS |
| WANT_DELEGATION (op 56) | Client-driven delegation requests | Return NFS4ERR_NOTSUPP; server-driven grants only |
| Persistent sessions | Surviving session across server restart | Not needed for single-instance; volatile sessions only |
| Multi-instance trunking | Trunking across DittoFS instances | Out of scope; single-instance only |

## Feature Dependencies

```
EXCHANGE_ID ──> CREATE_SESSION ──> SEQUENCE ──> [all v4.1 ops]
                     |                |
                     |                |──> Owner-seqid bypass
                     |                |──> OPEN_CONFIRM suppression
                     |                └──> Implicit lease renewal
                     |
                     |──> BIND_CONN_TO_SESSION ──> Session trunking
                     |                              └──> Backchannel binding
                     |
                     |──> DESTROY_SESSION
                     |
                     └──> Backchannel on fore connection
                              |
                              |──> CB_SEQUENCE
                              |──> CB_RECALL (modified from v4.0)
                              |──> CB_NOTIFY ──> Directory delegations
                              └──> CB_RECALL_ANY

DESTROY_CLIENTID ──> (requires all sessions destroyed first)
RECLAIM_COMPLETE ──> (requires session established)
FREE_STATEID ──> (requires session established)
TEST_STATEID ──> (requires session established)
GET_DIR_DELEGATION ──> (requires backchannel for CB_NOTIFY)
```

## MVP Recommendation

Prioritize these features for a working NFSv4.1 that Linux clients can mount with `vers=4.1`:

1. **Types and constants foundation** — operation numbers, error codes, structures
2. **EXCHANGE_ID + CREATE_SESSION + DESTROY_SESSION** — session lifecycle
3. **SEQUENCE with EOS** — request processing with exactly-once semantics
4. **COMPOUND bifurcation** — minorversion=1 dispatch path
5. **Owner-seqid bypass** — skip v4.0 seqid validation in session context
6. **BIND_CONN_TO_SESSION** — connection management and reconnection
7. **RECLAIM_COMPLETE** — required by Linux client after mount
8. **Backchannel multiplexing + CB_SEQUENCE** — NAT-friendly callbacks

Defer:
- **Directory delegations:** Complex, not required for basic v4.1 mounts. Add after core sessions work.
- **FREE_STATEID, TEST_STATEID:** Useful but not blocking for basic operation.
- **BACKCHANNEL_CTL:** Needed only when GSS contexts on backchannel must be refreshed.
- **pNFS operations:** Explicitly out of scope.

## Sources

- [RFC 8881: NFSv4.1 Protocol](https://www.rfc-editor.org/rfc/rfc8881.html) — HIGH confidence
- [Linux nfsd NFSv4.1 Implementation Status](https://docs.kernel.org/filesystems/nfs/nfs41-server.html) — HIGH confidence
- [Linux NFS Server 4.0/4.1 Issues](http://linux-nfs.org/wiki/index.php?title=Server_4.0_and_4.1_issues) — MEDIUM confidence
- [nfs4j NFSv4.1 Operations](https://github.com/dCache/nfs4j) — MEDIUM confidence
