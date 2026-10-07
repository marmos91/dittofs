---
rfc: 21
title: "RFC 21 — NFS"
component: nfs
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
aliases:
  - RFC 21
  - NFS
tags:
  - rfc
---
# RFC 21 — NFS

**Status:** draft. [§14](#14.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone changing the NFS adapter — its versions, sessions, state on
the wire, attributes or locking side protocols — and anyone deciding what an NFS
client sees when a node goes away.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

**What this is.** NFS (Network File System) is the protocol Linux and Unix
machines use to mount a remote directory as if it were a local disk. The client
sends remote procedure calls over TCP — look up a name, open, read, write,
lock, close — and the server answers each one. This RFC is the NFS adapter: the
part of DittoFS that speaks those calls and translates each into a call on the
filesystem service ([RFC 17](rfc-17-vfs.md)). It makes every choice the NFS
specifications leave to the server — versions, session sizes, lease time, what
survives a node failure — and says which record elsewhere each NFS object stands
for. It owns no durable state of its own: names, file handles and attributes
are [RFC 7](rfc-7-namespace-metadata.md)'s; client records, opens, locks and
delegations are [RFC 14](rfc-14-open-state.md)'s; which shares exist, who may
mount them and the NFSv4 pseudo-filesystem (the read-only tree of directories
that leads a client to each share) are [RFC 17](rfc-17-vfs.md)'s. Behind the
service, writes land in a local journal and move to an object store in the
background ([RFC 0](rfc-0-data-lifecycle.md)); NFS sees none of that.

**How a client talks to the server, and what goes wrong.** Take the cluster:
`protocol` nodes **P1** and **P2** hold client connections, and a storage node
**S1** is the primary for `builds` — the one node that orders every change to
its files. The Linux build server **build01** mounts `builds` over NFSv4.1 and
untars a source tree of 40,000 small files.

1. **Mount.** build01 connects to the cluster's floating address, which P1
   holds. It identifies itself (`EXCHANGE_ID`), gets a **client ID**, and opens a
   **session** (`CREATE_SESSION`) with a table of up to 64 **slots**: each
   request goes in a slot with a sequence number, and the server keeps the last
   reply per slot so a resent request gets the stored answer instead of running
   twice. It walks the pseudo-filesystem to `builds` and receives the share's
   root **file handle**, an opaque name for the directory.
2. **Untar.** Every file is an `OPEN` that creates it, then `WRITE`s of up to
   1 MiB each, then `CLOSE`. P1 forwards each call to S1, which records the open
   and its state. Each request renews build01's 90 s **lease**; a client silent for
   90 s loses everything it held.
3. **P1 dies.** Requests for three files are in flight; their replies never
   reach build01. P2 takes over the floating address and resets build01's
   connection, so build01 reconnects at once — now to P2.
4. **What build01 sees.** P2 does not know the session, which lived in P1's
   memory: it answers `NFS4ERR_BADSESSION`. build01 opens a new session under
   the same client ID, which every node accepts because the client record is
   stored, and resends the three requests in new slots. Its opens, locks and
   delegations are untouched: they were at S1, not P1, so no grace period runs.
   But the reply cache went with P1, so the resent requests run again. A
   rewrite of the same 1 MiB is harmless; an exclusive create succeeds, because
   the stored create verifier matches; a `MKDIR` answers "already exists" for a
   directory build01 did in fact make.

Two other designs were possible. If the session held the opens and locks, P1's
loss would lose them too, and every client would have to reclaim its state in a
grace period of at least 90 s, during which no new open or lock is granted. If the reply
cache were stored and replicated, the retry would be exact, at the cost of one
replicated write per create and rename — about 40,000 extra for this untar —
to cover an event whose visible result matches an NFSv3 server reboot, which
clients already tolerate. This RFC takes neither: state at the primary, reply
cache in memory.

```text
 build01 (Linux, NFSv4.1)  ── mounts dittofs:/builds via a floating address
     │
     ▼
 ┌──────────── P1 ────────────┐  dies   ┌──────────── P2 ────────────┐
 │ session, 64 slots          │ ──────► │ session 2, same client ID  │
 │ reply cache (memory only)  │ address │ reply cache starts empty   │
 └─────────────┬──────────────┘  moves  └─────────────┬──────────────┘
               │      every call is forwarded to the file's primary
               ▼                                      ▼
 ┌────────────────────── S1, primary of builds ──────────────────────┐
 │ RFC 14: client record, opens, locks, delegations, stateids        │
 │ RFC 7:  names, file handles, attributes, directory cookies        │
 └───────────────────────────────────────────────────────────────────┘
```

**The words you need.**

- **`protocol` node / primary** — a node that holds client connections / the one
  storage node that orders a shard's writes ([RFC 0 glossary](rfc-0-data-lifecycle.md#Glossary)).
- **File handle** — an opaque name for a file, the same on every node and after
  restart ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)).
- **Session, slot, reply cache** — NFSv4.1's way to run each request exactly
  once: a resend in the same slot gets the stored reply.
- **Client ID and lease** — who the client is, and how long its state is kept
  while it is silent: 90 s ([RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)).
- **Stateid** — the token naming an open, a lock or a delegation on the wire.
- **Delegation** — a promise that no other client is using a file, so the client
  may cache it; recalled on conflict ([RFC 14 §5.1](rfc-14-open-state.md#5.1%20What%20a%20grant%20is)).
- **Grace period** — a window after a primary loses state in which only
  reclaims of held state are accepted ([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)).

**What this RFC promises.**

- Losing a `protocol` node loses no open, lock or delegation and starts no grace
  period; the client opens a new session and carries on.
- It does lose exactly-once for the requests in flight: a resent create, remove
  or rename may answer "exists" or "not found" for work that was done.
- A file handle, a stateid and a directory listing position are valid on every
  node, so a client moved between nodes keeps using them.
- A file's `change` attribute moves on every write, and never on an operation
  that changed nothing, so when one client closes a file and another opens it
  through a different node, the second sees the new data.
- A client silent for 90 s loses all its state at once; nothing is kept for it.
  Every NFS program is served over TCP only.

**How the rest is organised.** §2 sets versions, transport and transfer sizes.
§3 (sessions) and §5 (lease, stateids, delegations, grace) carry the failure
story above; read them first. §4 covers NFSv4.0 clients, §6 NFSv3 locking
(NLM, the Network Lock Manager, and NSM, its status monitor) and MOUNT. §7 fits
handles, directory cookies and names on the wire; §8 lists attributes and §9
data operations, which a first read can skim. §10 covers Kerberos on the wire;
§11–§13 hold the invariants, the conformance checks and the metrics.

---

## In short

- NFSv3, NFSv4.0, NFSv4.1 and a subset of NFSv4.2 are served, over TCP only.
- The adapter only translates. Every record it maps — clients, opens, lock
  owners, delegations, layouts, NLM and NSM state, copy state — is
  [RFC 14](rfc-14-open-state.md)'s; every name, handle and attribute is
  [RFC 7](rfc-7-namespace-metadata.md)'s; shares, the pseudo-filesystem and
  `SECINFO` answers are [RFC 17 §4.9](rfc-17-vfs.md)'s.
- NFSv4.1 sessions, their slot tables and reply caches live in the memory of
  the `protocol` node holding the connection. They are not durable: when that
  node dies, exactly-once execution is lost for the requests in flight, and a
  client that retries a non-idempotent operation on its new session may run it
  twice.
- Callbacks run from the file's primary to the `protocol` node holding the
  client's back channel, through the callbacks the client registered last.
- No courtesy clients: a lease that expires releases everything at once.
- Labeled NFS, named attributes (`OPENATTR`), inter-server copy and referrals to
  another installation are not offered.

---

## 1. Purpose

The NFS specifications fix the wire and leave a set of choices to the server:
which versions and transports to serve, how large a slot table is, whether the
reply cache survives a crash, how long a lease is, whether an expired client is
kept as a courtesy, which attributes exist, what a cookie verifier means, how
large a transfer may be. A server that leaves them to whoever writes the handler
makes them differently in each version. This document makes each once, states
why, and maps each NFS concept onto the record that owns it elsewhere in the
set.

It answers:

> **For every choice the NFS RFCs leave open, what does this server do, and
> which record in the rest of the set does each wire object stand for?**

### 1.1 Non-goals

This document **MUST NOT**:

- define a record. Clients, opens, locks, lock owners, delegations, layouts,
  NLM and NSM records, exclusive-create verifiers and async copy state are
  [RFC 14](rfc-14-open-state.md)'s and [RFC 7](rfc-7-namespace-metadata.md)'s; the
  server owner and scope are [RFC 16](rfc-16-metadata-store.md)'s;
- decide admission, squashing, the pseudo-filesystem or `SECINFO` — [RFC 17 §4.9](rfc-17-vfs.md);
- decide authentication, principals or the `owner@domain` mapping — RFC 18
  (planned); or ACL semantics — RFC 19 (planned);
- restate the XDR of RFC 1813, RFC 7530, RFC 8881 or RFC 7862, or the error
  mapping table, which RFC 20 (planned) owns with [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values).

## 2. Versions, transport and sizes

### 2.1 Versions served

| Program, version | Served | Notes |
| --- | --- | --- |
| NFS v2 | no | `PROG_MISMATCH` naming 3–4 |
| NFS v3 (RFC 1813), MOUNT v3, NLM v4, NSM v1 | yes | §6 |
| NFS v4.0 (RFC 7530) | yes | §4 |
| NFS v4.1 (RFC 8881) | yes | sessions §3, pNFS per [RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS) |
| NFS v4.2 (RFC 7862, RFC 8276) | the subset of §9.3 | an operation outside it returns `NFS4ERR_NOTSUPP` |

A `COMPOUND` whose `minorversion` is above 2 **MUST** be answered
`NFS4ERR_MINOR_VERS_MISMATCH` with no operation run. The minor version is the
client's choice per `COMPOUND`; the server **MUST NOT** remember one per client,
so a client that falls back from 4.2 to 4.1 needs no new client ID.

> decision: NFSv4.0 is served although NFSv4.1 replaces everything it does,
> because clients and appliances still pin `vers=4.0`. It is the version that
> recovers worst from a `protocol` node loss (§4.2). Withdraw it when no
> supported client needs it; nothing else in this document depends on it.

### 2.2 TCP only

Every NFS program — NFS, MOUNT, NLM, NSM — **MUST** be served over TCP and
**MUST NOT** be served over UDP. The port mapper, where the server runs one,
answers on UDP and TCP, because clients probe it there before choosing a
transport; it reaches no file. `SM_NOTIFY` is sent the way NSM peers listen
for it (§6.2).

> decision: no UDP. NFS over UDP has no congestion control, reorders, and
> corrupts silently when IP fragment IDs wrap at the transfer sizes of §2.3;
> RFC 7530 and RFC 8881 already require a congestion-controlled transport for
> NFSv4. NFSv3 clients that default to UDP fall back to TCP when the port mapper
> offers only TCP. Revisit only for a client population that cannot speak TCP,
> which no supported client is. RDMA transports are not offered either; see
> §14.

### 2.3 Transfer sizes

`maxread` and `maxwrite` (NFSv4), `rtmax`, `wtmax`, `rtpref` and `wtpref`
(NFSv3 `FSINFO`), and a session's `ca_maxrequestsize` and
`ca_maxresponsesize` are derived from one fixed value: **1 MiB** of data, plus
the RPC and compound overhead for the sizes that include headers. `dtpref` is
64 KiB. `maxfilesize` is 2^63 − 1.

The size is fixed, not a setting ([RFC 13](rfc-13-configuration.md)): a client
negotiates down, never up, and a value that differs between nodes breaks a
client moved between them by an address takeover.

> ponytail: one transfer size for every client and every share. 1 MiB already
> amortises the RPC on a fast link, and each outstanding request pins one such
> buffer (RFC 24, planned). Raise it, or make it per share, when a benchmark on
> the reference box shows large sequential I/O limited by request count rather
> than by the journal.

## 3. NFSv4.1 sessions

### 3.1 Slot tables

A session's fore channel offers at most **64 slots**, and its back channel at
most **16**. `CREATE_SESSION` grants the lesser of what the client asks and
these. The server **MAY** lower `sr_target_highest_slotid` under memory pressure
and **MUST** honour a client that then retires slots; it **MUST NOT** shrink
`sr_highest_slotid` below a slot with a request in flight.

`ca_maxresponsesize_cached` is granted as asked up to the transfer-size bound of
§2.3, but the reply kept per slot is the one the client asked to cache
(`sa_cachethis`); a non-cached reply keeps only its status and the operations
before the first that returns data, as RFC 8881 §2.10.6.1.3 allows.

> ponytail: 64 slots per session bounds one client's parallelism per session to
> 64 requests. A client that needs more opens a second session or trunks
> (§3.3). Raise the bound, or make it dynamic by memory budget, when a
> single-client benchmark is slot-bound.

### 3.2 The reply cache is per session, in memory

The reply cache — the last reply per slot, by sequence ID — **MUST** be held in
the memory of the `protocol` node that holds the session, and **MUST NOT** be
written to the metadata store or replicated.

When that node dies, its sessions go with it. The client reconnects to a
surviving node — through a floating address ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)) — and is
answered `NFS4ERR_BADSESSION`; it runs `CREATE_SESSION` against its existing
client ID, which every node accepts because the client record is durable
([RFC 14](rfc-14-open-state.md)), and resumes. Its opens, locks and
delegations are unaffected: they are held by the primaries, not by the
`protocol` node ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)), and no grace runs.

What the client loses is exactly-once for the requests that were in flight. It
resends them on the new session, with new slots, and they run again:

| Request in flight | Seen by the client on retry |
| --- | --- |
| `READ`, `GETATTR`, `LOOKUP`, `READDIR` | the same answer, or a newer one |
| `WRITE` at the same offset | the same bytes written again: harmless unless another client wrote the range in between |
| `CREATE` / `OPEN` with `EXCLUSIVE4_1` | success: the stored verifier matches ([RFC 7](rfc-7-namespace-metadata.md)) |
| `CREATE` / `OPEN` `GUARDED`, `MKDIR`, `LINK`, `SYMLINK` | `NFS4ERR_EXIST` |
| `REMOVE` | `NFS4ERR_NOENT` |
| `RENAME` | `NFS4ERR_NOENT`, or success if the target name is back |
| `LOCK`, `OPEN` upgrade | `NFS4ERR_OLD_STATEID` or `NFS4ERR_BAD_STATEID` on the stale seqid; the client re-reads its state |

The route envelope's dedup table ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope),
[RFC 17 §4.7](rfc-17-vfs.md#4.7%20Callable%20across%20the%20network)) does **not**
mitigate this: its request ID is minted by the forwarding node, and a retry
arriving through a different `protocol` node carries a new one. It catches
retries between a `protocol` node and a primary, not between a client and a
`protocol` node.

> decision: the reply cache is not durable. A durable reply cache costs a
> replicated write per non-idempotent request, on the path every create and
> rename takes, to protect against one event — a `protocol` node dying with
> requests in flight — whose visible result is the same as an NFSv3 server
> reboot, which every client already tolerates. Revisit when client-visible
> errors after a `protocol` node loss are reported by users, and then key the
> primary's dedup table by (client ID, session, slot, sequence ID) carried in
> the envelope, which survives a `protocol` node but not a session.

### 3.3 Trunking, server owner and scope

Every `protocol` node of an installation **MUST** return the same `server_scope`
and the same `so_major_id` in `EXCHANGE_ID`, and **MUST** return a
`so_minor_id` unique to the node. The values are the installation's
([RFC 16](rfc-16-metadata-store.md)). So:

- **client ID trunking** across nodes is allowed: one client ID, sessions on
  several nodes, each node's connections bound to its own sessions;
- **session trunking** — one session with connections to several nodes — is
  refused: `BIND_CONN_TO_SESSION` or a `SEQUENCE` naming a session this node does
  not hold is answered `NFS4ERR_BADSESSION`, because a session's slots live on
  one node (§3.2). Session trunking across connections to one node is allowed.

`EXCHANGE_ID` sets `EXCHGID4_FLAG_USE_NON_PNFS` and, where the deployment has a
data server role, `EXCHGID4_FLAG_USE_PNFS_MDS`; data servers return
`EXCHGID4_FLAG_USE_PNFS_DS` ([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS)).
State protection: `SP4_NONE` and `SP4_MACH_CRED` are accepted; `SP4_SSV` is
refused with `NFS4ERR_ENCR_ALG_UNSUPP`.

### 3.4 The back channel and where callbacks go

Callbacks — `CB_RECALL`, `CB_LAYOUTRECALL`, `CB_OFFLOAD`,
`CB_RECALL_ANY` — are issued by the primary of the file's shard through the
`Callbacks` the client registered ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)).
The registration names the `protocol` node holding the client's back channel,
and the call travels primary → that node → the client's connection.

- A client's registration **MUST** be replaced, not added to, when it binds a
  back channel on another node: the latest wins, so after a `protocol` node loss
  the client's new `CREATE_SESSION` (with `CDFC4_BACK`) or `BIND_CONN_TO_SESSION`
  redirects every later callback.
- Until it does, a callback has nowhere to go and fails; the primary treats
  that as the client not answering, and the recall is revoked at its deadline
  ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).
- While a client has no usable back channel, every `SEQUENCE` reply to it **MUST**
  carry `SEQ4_STATUS_CB_PATH_DOWN`, and no delegation is offered to it
  ([RFC 14 §5.4](rfc-14-open-state.md#5.4%20How%20a%20grant%20is%20obtained%2C%20and%20where%20it%20pays)).
- The back channel's security is `AUTH_NONE` or `AUTH_SYS`, per
  `CREATE_SESSION`'s `csa_sec_parms`; `RPCSEC_GSS` on the back channel is not
  offered.

## 4. NFSv4.0 clients

### 4.1 SETCLIENTID and the callback path

`SETCLIENTID` and `SETCLIENTID_CONFIRM` map onto the client record as
`EXCHANGE_ID` and `CREATE_SESSION` do: the client's `nfs_client_id4` is its
identity, its verifier tells a reboot, and the confirm verifier is held with the
client record so that any node can confirm ([RFC 14](rfc-14-open-state.md)).

The NFSv4.0 callback is a separate connection the server opens to the client's
`r_addr`. It is opened by the `protocol` node that confirmed the client, or
that most recently received a `RENEW` or stateful operation from it after a
loss, and that node registers itself for the client's callbacks (§3.4). A
callback connection that cannot be opened makes the client's callback path
down: `RENEW` answers `NFS4ERR_CB_PATH_DOWN`, and no delegation is offered.

### 4.2 Owner sequence IDs

NFSv4.0 sequences `OPEN`, `CLOSE`, `OPEN_CONFIRM`, `OPEN_DOWNGRADE` and `LOCK`
per open owner or lock owner, and replays the last reply to a retransmission.

- The **next expected sequence ID** of each owner **MUST** be held with the owner at
  the primary ([RFC 14](rfc-14-open-state.md)), and advanced in the step that
  applies the operation, so every node agrees on it.
- The **last reply** per owner is held only in the `protocol` node that sent
  it, as the session reply cache is (§3.2).

After a `protocol` node loss, a retransmission of an owner's last operation
finds its sequence ID already consumed and no cached reply. The server answers
`NFS4ERR_BAD_SEQID`, and the client recovers that owner's state.

> decision: NFSv4.0 owner replay is best-effort across a `protocol` node loss,
> for the reason of §3.2, and is worse than NFSv4.1's because an owner's error
> costs the client every open under it. NFSv4.1 is the supported path for
> clients that need to ride a node failure; withdraw this when §2.1 withdraws
> NFSv4.0.

`OPEN_CONFIRM` is required for a new open owner, as RFC 7530 §16.18 specifies;
`RELEASE_LOCKOWNER` releases the lock owner's record at the primary.

## 5. Client state on the wire

### 5.1 Client ID, lease time, expiry

- `lease_time` is **90 s**, fixed ([RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)). Every `SEQUENCE`, and
  every NFSv4.0 `RENEW` or stateful operation, renews it.
- The client ID is minted from the client record and is valid on every node.
- `DESTROY_CLIENTID` maps to `Disconnect` ([RFC 17](rfc-17-vfs.md#3.1%20Operations)) and is refused with
  `NFS4ERR_CLIENTID_BUSY` while the client still holds a session, as RFC 8881
  §18.50 requires; `DESTROY_SESSION` drops only the node's session.

**No courtesy clients.** A client whose lease expires has every open, lock,
delegation and layout released at once, in every view
([RFC 14 §4.3](rfc-14-open-state.md#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)), whether or not another client wants them. Its next request is
answered `NFS4ERR_EXPIRED` (stateids) or `NFS4ERR_STALE_CLIENTID` /
`NFS4ERR_BADSESSION` (client ID, session), and it starts over.

> decision: no courtesy clients. Keeping an expired client's state until a
> conflict arrives lets a laptop that slept through its lease resume without
> losing locks, but costs a second, conditional release path — state that is
> both expired and held — at every primary and in the cross-protocol checks of
> RFC 14 §7, where an SMB open would have to expire NFS state on conflict. The
> ceiling: a client silent for 90 s loses its locks even when nobody wanted
> them. Revisit if that shows up as lost locks on suspended clients.

### 5.2 Stateids

Stateid encoding is the adapter's ([RFC 17 §4.2](rfc-17-vfs.md#4.2%20Translation%20stays%20in%20adapters)), under three rules:

1. **`other` names a record, not a table slot.** The 12-byte `other` field
   **MUST** be derived from the RFC 14 identifier it stands for — open, lock
   owner's lock state, delegation, layout or copy — and a type tag, so any
   `protocol` node resolves it without a table of its own and a stateid survives
   a `protocol` node loss.
2. **`seqid` is held with the state at the primary**, advanced in the step that
   changes it (`OPEN` upgrade, `OPEN_DOWNGRADE`, `LOCK`, `LOCKU`, `LAYOUTGET`).
   A request with an older `seqid` is `NFS4ERR_OLD_STATEID`; a newer one
   `NFS4ERR_BAD_STATEID`. NFSv4.1's `seqid` 0 means "current" (RFC 8881 §8.2.2).
3. **A stateid is checked against its client** — the client ID of the session,
   or for NFSv4.0 the owner's client — and one belonging to another client is
   `NFS4ERR_BAD_STATEID` (`ErrNotYours`).

The special stateids: all-zeros is an anonymous open
([RFC 17 §4.1](rfc-17-vfs.md#4.1%20The%20operation%20set%20is%20the%20union%2C%20not%20the%20intersection)), checked per operation against deny modes;
all-ones bypasses no check, and is treated as anonymous for `READ` only and
refused for `WRITE`; the current stateid (4.1) is resolved inside the compound.

`TEST_STATEID` answers per stateid without changing state. `FREE_STATEID`
releases a lock state with no locks held, or a revoked delegation or layout the
client acknowledges, and is refused with `NFS4ERR_LOCKS_HELD` otherwise.
`RECLAIM_COMPLETE` maps to `ReclaimComplete`.

### 5.3 Delegations

Delegations are RFC 14's caching grants: a read delegation is a read grant, a
write delegation read, write and handle ([RFC 14 §5.1](rfc-14-open-state.md#5.1%20What%20a%20grant%20is)). Both are offered, under
RFC 14 §5.4, and the client's `OPEN4_SHARE_ACCESS_WANT_*` flags are honoured.
`CB_RECALL` is the recall, `DELEGRETURN` the acknowledgement; a recall not
answered by its deadline is revoked, the client is told by
`SEQ4_STATUS_RECALLABLE_STATE_REVOKED`, and its delegation stateid is
`NFS4ERR_DELEG_REVOKED` until freed. `CB_RECALL_ANY` is sent when the primary's
grant table is over its budget.

**Directory delegations are not offered.** `GET_DIR_DELEGATION` returns
`GDD4_UNAVAIL` with `will_signal_deleg_avail` false, and NFSv4.1 directory
notifications, which ride on them, are therefore never sent. RFC 14 Watches
reach NFS clients through nothing.

> decision: no directory delegations. Linux and other common clients do not
> request them, and each one is a watch plus a grant broken by every create in
> the directory. Revisit when a client that uses them is supported; the record
> is already RFC 14's Watch.

### 5.4 Grace on the wire

Grace is per shard ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)). `ErrGrace` is `NFS4ERR_GRACE` (v4) and
`NLM4_DENIED_GRACE_PERIOD` (NLM). A reclaim outside grace, or by a client the
record does not name for the shard, is `NFS4ERR_NO_GRACE`; a reclaim after
`RECLAIM_COMPLETE` is `NFS4ERR_NO_GRACE` too. `CLAIM_PREVIOUS` reclaims an
open, `reclaim` set on `LOCK` a lock. A delegation is not reclaimed
([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)): `CLAIM_PREVIOUS`
with a delegation type is answered without one. A client told to reclaim by a
failover of one shard reclaims through the same calls; the signals are RFC 14
§4.4's.

## 6. NFSv3 locking: NLM and NSM

### 6.1 NLM

NLM v4 maps onto RFC 14: the NLM host name (`caller_name`) is the client, an
`(svid, oh)` pair the lock owner, and each lock an RFC 14 lock under an
anonymous open of the file. The records, including what NSM needs to notify a
client, are RFC 14's.

- **Blocking locks.** `NLM4_LOCK` with `block` set that conflicts returns
  `NLM4_BLOCKED`; the primary keeps the waiter, and on release the `protocol`
  node holding the client's registration sends `NLM4_GRANTED_MSG` /
  `NLM4_GRANTED`. A waiter not granted within one lease period is dropped, and
  the client's own retry re-queues it. No worker waits ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)).
- **Grace.** NLM shares RFC 14's per-shard grace with NFSv4: during it a
  non-reclaim lock is `NLM4_DENIED_GRACE_PERIOD`, and `reclaim` set is a
  reclaim.
- **Share reservations** (`NLM4_SHARE`), which DOS-era clients use, map to an
  open with a deny mode, so they conflict with SMB and NFSv4 deny modes like
  any other.
- `NLM4_FREE_ALL` expires the client.
- The `*_MSG` / `*_RES` asynchronous procedures are accepted over TCP and
  answered on the client's NLM callback service.

### 6.2 NSM

The server monitors clients as its peers expect: `SM_MON` from the client's
statd is recorded with the client ([RFC 14](rfc-14-open-state.md)). After a failover
of a shard, the new primary asks the `protocol` node currently holding each
affected client's address to send `SM_NOTIFY` naming the server name the client
monitored — the floating address's name, not a node's — with a state number
the installation raises for each failover, so the client's statd reclaims
locks. `SM_NOTIFY` is sent by UDP, as statd peers listen for it; this is the
one UDP datagram the server emits, and it carries no file operation.

A `protocol` node loss alone is not a restart for NLM: no lock was lost, so no
`SM_NOTIFY` is sent ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)).

### 6.3 MOUNT

MOUNT v3 is `Root` ([RFC 17 §4.9](rfc-17-vfs.md)): `MNT` resolves the path,
returns the root handle and the share's flavours; `DUMP` is empty, `UMNT` and
`UMNTALL` change nothing; `EXPORT` lists what [RFC 17 §4.9](rfc-17-vfs.md) lets
the caller see.

## 7. Names, handles and directories

### 7.1 File handles

A file handle is RFC 7's opaque handle, returned unchanged
([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)). It fits NFSv3's 64 bytes and NFSv4's 128. `fh_expire_type`
is `FH4_PERSISTENT`, true by [RFC 7 §6.2](rfc-7-namespace-metadata.md#6.2%20A%20handle%20is%20stable%20across%20restart); a released file's handle is
`NFS3ERR_STALE` / `NFS4ERR_STALE`. `fileid` is RFC 7's derived numeric id,
and a named stream is never reachable over NFS (§8.2).

### 7.2 READDIR cookies and verifier

RFC 7 owns stability ([RFC 7 §3.5](rfc-7-namespace-metadata.md#3.5%20A%20cookie%20survives%20concurrent%20mutation)); this section only fits it on the wire.

- The 64-bit cookie **MUST** be a pure function of the last entry's ordering
  key, so any node resumes a listing another node began. Cookies 0, 1 and 2
  are reserved (start, and the `.` and `..` NFSv3 synthesises).
- Where the key does not fit 64 bits, the cookie is a 63-bit digest of it and
  the resume finds the first key at or after the entry whose digest it is; a
  digest that matches no entry resumes after the nearest key, as RFC 7 §3.5
  allows for an entry removed during the listing.
- The cookie verifier is RFC 7's ordering generation. A verifier that no longer
  matches is `NFS3ERR_BAD_COOKIE` / `NFS4ERR_NOT_SAME`; a zero verifier with a
  non-zero cookie is accepted, as Linux clients send it.

### 7.3 Names

A name is RFC 7's bytes ([RFC 7 §3.2](rfc-7-namespace-metadata.md#3.2%20A%20name%20is%20bytes%2C%20and%20it%20is%20validated%20at%20the%20boundary)). The NFS adapter passes it through
unvalidated and unconverted, on every version; RFC 7 refuses what it refuses,
mapped to `NFS3ERR_INVAL` / `NFS4ERR_INVAL` or `NFS4ERR_BADNAME`. NFSv4's
`fs_charset_cap` sets `FSCHARSET_CAP4_CONTAINS_NON_UTF8` and clears
`FSCHARSET_CAP4_ALLOWS_ONLY_UTF8`, since NFSv3 clients can create such names. How SMB shows such names is [RFC 22](rfc-22-smb.md)'s.

### 7.4 Pseudo-filesystem, SECINFO and locations

The pseudo-filesystem, its handles, `fsid` and change attribute, and
`SECINFO` / `SECINFO_NO_NAME` answers are [RFC 17 §4.9](rfc-17-vfs.md)'s. The
adapter encodes them; it adds no export of its own.

`fs_locations` is returned only for a drain ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)): a draining
`protocol` node answers operations on a share with `NFS4ERR_MOVED`, and
`fs_locations` names another `protocol` node of the same installation and the
same path. Every other time the attribute lists no location. A referral to
another installation is never returned ([RFC 17 §1.1](rfc-17-vfs.md#1.1%20Non-goals)), and
`fs_locations_info` is not supported.

## 8. Attributes

### 8.1 Supported

Every REQUIRED attribute of RFC 8881 is supported. Of the RECOMMENDED and NFSv4.2
attributes:

| Attribute | Source |
| --- | --- |
| `change` | `File.Version` with the engine's overlay ([RFC 17 §5.7](rfc-17-vfs.md#5.7%20GetAttr)); §9.2 |
| `change_attr_type` | `NFS4_CHANGE_TYPE_IS_UNDEFINED` (§14) |
| `size`, `space_used` | `Size`; `Charged` ([RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem)) |
| `mode`, `owner`, `owner_group`, `mode_set_masked` | the File; owner strings by RFC 18 (planned) |
| `acl`, `dacl`, `aclsupport` | RFC 7's ACL, mapped by RFC 19 (planned) |
| `sacl` | the ACL's audit entries ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)) |
| `archive`, `hidden`, `system` | `Flags` |
| `time_access`, `time_modify`, `time_metadata`, `time_create`, `*_set` | `Access`, `Modify`, `Change`, `Birth` |
| `time_delta` | `Capabilities.TimeGranularity` |
| `fileid`, `mounted_on_fileid`, `numlinks`, `rawdev` | RFC 7; pseudo-filesystem per [RFC 17 §4.9](rfc-17-vfs.md) |
| `fsid`, `unique_handles`, `homogeneous`, `fh_expire_type` | per share; true; true; persistent |
| `case_insensitive`, `case_preserving`, `maxname`, `maxlink`, `no_trunc`, `chown_restricted`, `cansettime` | `Capabilities`; the share's case rule ([RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case)) |
| `files_*`, `space_avail`, `space_free`, `space_total` | `StatFS` |
| `quota_avail_hard`, `quota_avail_soft`, `quota_used` | `Quota` for the caller's principal |
| `maxread`, `maxwrite`, `maxfilesize` | §2.3 |
| `fs_layout_type`, `layout_*`, `layout_alignment` | pNFS ([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS)) |
| `fs_locations` | §7.4 |
| `suppattr_exclcreat` | `mode`, `owner`, `owner_group`, `acl`, `time_*_set` |
| `xattr_support` | true where `Capabilities.Xattrs` |
| `clone_blksize` | 0: no alignment required, since `Copy` shares content at any offset |

### 8.2 Not supported

| Feature | Answer | Why |
| --- | --- | --- |
| Labeled NFS (`sec_label`, RFC 7204) | not in `supported_attrs` | no mandatory-access-control policy exists to label for |
| Named attributes (`OPENATTR`, `named_attr` false) | `NFS4ERR_NOTSUPP` | named streams exist for SMB; NFS clients use RFC 8276 xattrs, and no common client uses `OPENATTR` |
| `retention_*`, `retentevt_*`, `mimetype`, `time_backup`, `dir_notif_delay`, `dirent_notif_delay`, `fs_status`, `fs_locations_info`, `mdsthreshold`, `space_freed` | not in `supported_attrs` | no record holds them; `space_freed` cannot be computed cheaply where content is shared |

> decision: no labeled NFS. A label would be an xattr in the security
> namespace ([RFC 7 §2.7](rfc-7-namespace-metadata.md#2.7%20Extended%20attributes%20and%20named%20streams)) with no other record, so adding it is cheap
> — but serving it means taking part in a client's MAC policy, which is a
> security claim this server makes nowhere else. Revisit for a customer running
> SELinux-labeled home directories, storing the label as an xattr.

## 9. Data

### 9.1 Write stability

`UNSTABLE4` / `UNSTABLE` writes are `Write` with unstable stability, answered
from the journal ([RFC 0 §5.1](rfc-0-data-lifecycle.md)); `COMMIT` is
`Commit`. `DATA_SYNC` and `FILE_SYNC` are both a stable write, and the reply
says `FILE_SYNC`: a stability point commits size and `mtime` with the data
([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)), so there is no cheaper level to offer. The verifier in every
`WRITE` and `COMMIT` reply is RFC 17's ([RFC 17 §5.8](rfc-17-vfs.md#5.8%20The%20write%20verifier)), passed through.

### 9.2 Close-to-open, and what the server guarantees

Clients implement close-to-open: flush and `GETATTR` at close, `GETATTR` at open,
and drop the cache when `change` moved. The server guarantees what that needs:

- **`change` moves on every write the primary applies**, not only at an existence
  commit. The overlay's `Version` ([RFC 17 §5.7](rfc-17-vfs.md#5.7%20GetAttr)) **MUST** advance per
  staged write, so two `GETATTR`s with a write between them never return the
  same value, whichever node answers.
- **`GETATTR` is answered at the primary** when the engine holds uncommitted
  writes, so a `protocol` node never answers from a committed record that lags
  them.
- A `GETATTR` after a client's `CLOSE` or `COMMIT` returns a `size` and `change`
  covering every write that client was answered for.

`change` never moves for an operation that changed nothing
([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps)): an offload, eviction or fill does not make every client
drop its cache.

### 9.3 NFSv4.2 operations

| Operation | Maps to | Notes |
| --- | --- | --- |
| `ALLOCATE` | `Allocate` | |
| `DEALLOCATE` | `Deallocate` | holes are RFC 6's |
| `SEEK` | `Seek` | `NFS4_CONTENT_DATA` / `NFS4_CONTENT_HOLE` |
| `READ_PLUS` | `Read` and `Seek` | holes returned as `NFS4_CONTENT_HOLE` segments only where RFC 6 records one; zeros written as data stay data |
| `COPY` (intra-server) | `Copy` | sync or async, §9.4 |
| `OFFLOAD_STATUS`, `OFFLOAD_CANCEL`, `CB_OFFLOAD` | async copy state ([RFC 14](rfc-14-open-state.md)) | |
| `CLONE` | `Copy` | content shared, never re-staged; atomic as RFC 7862 §15.13 asks |
| `GETXATTR`, `SETXATTR`, `LISTXATTRS`, `REMOVEXATTR` (RFC 8276) | `Xattrs`, `SetXattr`, `RemoveXattr` | the user namespace only; the same records SMB extended attributes reach |
| `COPY_NOTIFY`, inter-server `COPY` | `NFS4ERR_NOTSUPP` | one installation only ([RFC 17 §1.1](rfc-17-vfs.md#1.1%20Non-goals)) |
| `IO_ADVISE`, `WRITE_SAME`, `LAYOUTERROR`, `LAYOUTSTATS` | `NFS4ERR_NOTSUPP` | |

### 9.4 Async COPY

A `COPY` whose length is at most 64 MiB, or which shares content without moving
bytes, runs synchronously. A longer one with `ca_synchronous` false returns a
copy stateid at once and runs at the destination file's primary, which holds the
copy state ([RFC 14](rfc-14-open-state.md)). Completion is a `CB_OFFLOAD`
through the client's current callback registration (§3.4); a `CB_OFFLOAD` that
cannot be delivered is dropped, and the client learns the result from
`OFFLOAD_STATUS`, which any node answers from the copy state. A copy whose
primary fails over is lost with the volatile table: `OFFLOAD_STATUS` answers
`NFS4ERR_BAD_STATEID`, and the client falls back to reading and writing.

## 10. RPCSEC_GSS

Authentication and principal mapping are RFC 18's (planned). On the wire:

- a GSS context — handle, sequence window, session key — **MUST** be held in the
  memory of the `protocol` node that created it, and is not shared or stored.
  After a `protocol` node loss, the client's next call on the old handle is
  answered `RPCSEC_GSS_CREDPROBLEM` and it establishes a new context;
- the sequence window is 128;
- `krb5`, `krb5i` and `krb5p` are served; the share's minimum level is
  [RFC 17 §4.9](rfc-17-vfs.md)'s admission, answered `NFS4ERR_WRONGSEC` or
  `AUTH_TOOWEAK`.

## 11. Invariants

| # | Invariant |
| --- | --- |
| N1 | No NFS program is served over UDP. |
| N2 | Every `protocol` node returns the same `server_scope` and `so_major_id`, and its own `so_minor_id`. |
| N3 | Sessions, reply caches and GSS contexts live on one `protocol` node and are never stored; a session is never bound to connections on two nodes. |
| N4 | A stateid's `other` names an RFC 14 record, resolvable on any node; its `seqid` is held with the state at the primary. |
| N5 | An expired client's state is released at expiry; nothing is kept as a courtesy. |
| N6 | A client's latest back-channel registration receives every later callback. |
| N7 | `change` moves on every write the primary applies and never on an operation that changed nothing. |
| N8 | A READDIR cookie is a function of the entry's ordering key, valid on every node. |
| N9 | No referral or `fs_locations` names a server outside the installation. |
| N10 | Every NFS object the adapter maps names a record another RFC owns; the adapter keeps no durable state. |

## 12. Conformance

Every check runs over the wire against a multi-node deployment unless it says
otherwise; the protocol suites test translation, and the service suite
([RFC 17 §8](rfc-17-vfs.md#8.%20Conformance)) tests behaviour.

| Requirement | Check | Fails without the rule |
| --- | --- | --- |
| §2.2 TCP only | Probe NFS, MOUNT and NLM by UDP. Assert no reply; mount with `proto=udp` falls back or fails. | a UDP listener left enabled |
| §3.2 reply cache | pynfs `nfs4.1` session and `SEQUENCE` replay tests; then kill the `protocol` node with a `REMOVE` in flight and assert the client sees `NFS4ERR_NOENT` on retry and recovers its session with no grace. | a session assumed durable, or a grace started on `protocol` node loss |
| §3.3 trunking | `EXCHANGE_ID` on two nodes: same `so_major_id` and scope, different `so_minor_id`; `BIND_CONN_TO_SESSION` across nodes is `NFS4ERR_BADSESSION`. | one scope per node, or a session bound across nodes |
| §3.4 callback routing | Hold a delegation, kill the client's `protocol` node, reconnect through another, conflict from a second client. Assert `CB_RECALL` arrives on the new connection. | callbacks registered once at first `Connect` |
| §3.4 path down | Block the back channel. Assert `SEQ4_STATUS_CB_PATH_DOWN` and no delegation offered. | |
| §4.2 v4.0 owners | pynfs `nfs4.0` open-owner and lock replay tests; then move the client to another node mid-sequence and assert the next sequenced `OPEN` is accepted. | seqids held in the `protocol` node |
| §5.1 no courtesy | Let a client's lease expire with no conflict; assert its lock is gone and a new client takes it. | |
| §5.2 stateids | pynfs `TEST_STATEID`, `FREE_STATEID`, bad- and old-stateid tests; present another client's stateid and assert `NFS4ERR_BAD_STATEID`. | |
| §5.4 grace | pynfs reboot/reclaim tests against a shard failover, not a whole-server restart. | grace tested only by restarting everything |
| §6 NLM | cthon04 lock tests over NFSv3; an NLM lock against an SMB byte-range lock and an SMB deny mode against `NLM4_SHARE`. Fail a shard over and assert `SM_NOTIFY` reaches the client's statd and it reclaims. | NLM state held in the adapter |
| §7.2 cookies | List a 10^5-entry directory from one node, resume each page on another node, while a third client creates and removes. Assert every untouched entry exactly once. | positional or node-local cookies |
| §8 attributes | pynfs attribute tests (`supported_attrs`, `GETATTR`, `SETATTR`); assert `sec_label` and `named_attr` absent. | |
| §9.1 stability | Write `DATA_SYNC`, crash the primary, read back. Assert the data and its `size` and `mtime`. | |
| §9.2 close-to-open | Two clients on two nodes: one writes and closes, the other opens and reads. Assert the new data, for writes staged and not yet committed. Two writes with no commit between them change `change` twice. | `change` from the committed record only |
| §9.3 NFSv4.2 | xfstests over NFSv4.2 (`fallocate`, `SEEK_HOLE`/`SEEK_DATA`, `copy_file_range`, `FICLONE`, xattrs); `READ_PLUS` over a file with holes reads zeros where holes are. | |
| §9.4 async copy | A 1 GiB `COPY` async; kill the client's `protocol` node before completion; assert `OFFLOAD_STATUS` from another node reports it. | copy state held in the adapter |

**What must not stand in.** A single-node run cannot fail §3.2–§3.4, §4.2 or
§7.2: each is about state surviving a move between nodes, and needs two.

## 13. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| sessions held per node | `dittofs_nfs_sessions` | gauge |
| slot-table saturation | `dittofs_nfs_slots_in_use{channel}` | gauge |
| reply-cache hits (genuine retransmissions) | `dittofs_nfs_replay_hits_total` | counter |
| `NFS4ERR_BADSESSION` answered — clients recovering a session lost with a node | `dittofs_nfs_badsession_total` | counter |
| callbacks with no live back channel | `dittofs_nfs_cb_path_down_total` | counter |
| GSS contexts held per node | `dittofs_nfs_gss_contexts` | gauge |
| NLM blocked waiters | `dittofs_nfs_nlm_waiters` | gauge |

Operation latency is RFC 17's; grants, recalls and grace are RFC 14's. No share
or client label.

## 14. Open questions

1. **`change_attr_type`.** Reporting `NFS4_CHANGE_TYPE_IS_VERSION_COUNTER` lets
   clients skip invalidations, but needs `change` never to go backwards across a
   failover that loses overlay versions. Until that is shown, `UNDEFINED`.
2. **RDMA.** NFS over RDMA (RFC 8166) is not offered. Whether it pays for
   itself depends on a deployment with RDMA fabrics and on RFC 24's buffer model.
3. **Directory delegations and notifications** (§5.3), with RFC 14's open
   question 4.
4. **Session trunking across nodes** (§3.3) would need slots held at a shared
   place; nothing asks for it yet.

## Appendix A — prior art

*Non-normative.*

| Server | Choice | Taken or not |
| --- | --- | --- |
| Linux knfsd | reply cache in memory; client records on stable storage via a tracking daemon; courtesy clients kept since 5.x; UDP disabled by default for NFSv4 | records taken; courtesy clients not |
| NFS-Ganesha | sessions and reply cache in memory per node; clustered recovery with a shared grace record; `fs_locations` for migration | grace record and migration-only locations taken |
| Windows Server NFS | NFSv4.1 only for v4; no delegations by default; per-node session state in failover clusters | per-node session state taken |
| Clustered NAS (one session per node, client-ID trunking across nodes) | server owner shared by nodes, server scope per installation | taken (§3.3) |
