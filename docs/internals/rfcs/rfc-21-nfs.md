---
rfc: 21
title: "RFC 21 — NFS"
component: nfs
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-22-smb]]"
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

**How the rest is organised.** Each numbered section opens with a plain
paragraph — what the mechanism is for and what was decided — before its rules.
§2 sets versions, transport and transfer sizes. §3 (sessions) and §5 (lease,
stateids, delegations, grace) carry the failure story above; read them first.
§4 covers NFSv4.0 clients, §6 NFSv3 locking (NLM, the Network Lock Manager, and
NSM, its status monitor) and MOUNT. §7 fits handles, directory cookies and names
on the wire; §8 summarises attributes and §9 data operations, which a first read
can skim. §10 covers Kerberos on the wire; §11–§13 hold the invariants, the
conformance checks and the metrics. Appendix B lists every attribute and
Appendix C every NFSv4.2 operation.

---

## In short

- NFSv3, NFSv4.0, NFSv4.1 and a subset of NFSv4.2 are served, over TCP only.
- The adapter only translates. Every record it maps — clients, opens, lock
  owners, delegations, layouts, NLM and NSM state, copy state — is
  [RFC 14](rfc-14-open-state.md)'s; every name, handle and attribute is
  [RFC 7](rfc-7-namespace-metadata.md)'s; shares, the pseudo-filesystem and
  `SECINFO` answers are [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)'s.
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

The NFS standards fix the bytes on the wire and leave choices to the server:
which versions and transports to serve, how many requests a client may have in
flight, whether recent replies survive a crash, how long a silent client keeps
its state, which attributes exist, what a directory-listing position means, how
large one read or write may be. Left to each handler's author, they come out
different in each version. This document makes each once, says why, and names
the record elsewhere in the set that each NFS object stands for.

It answers:

> **For every choice the NFS RFCs leave open, what does this server do, and
> which record in the rest of the set does each wire object stand for?**

### 1.1 Non-goals

This document **MUST NOT**:

- define a record. Clients, opens, locks, lock owners, delegations, layouts,
  the records of NLM and NSM (NFSv3's lock and crash-notification protocols,
  §6), exclusive-create verifiers and async copy state are
  [RFC 14](rfc-14-open-state.md)'s and [RFC 7](rfc-7-namespace-metadata.md)'s;
  the server owner and scope (the identity every node reports, §3.3) are
  [RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)'s;
- decide admission, squashing, the pseudo-filesystem or `SECINFO` (the call
  that asks which security flavours a share accepts) — [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees);
- decide authentication, principals or the `owner@domain` mapping — RFC 18
  (planned); or ACL (access control list) semantics — RFC 19 (planned);
- restate the XDR (External Data Representation, the wire encoding) of RFC 1813,
  RFC 7530, RFC 8881 or RFC 7862, or the error mapping table, which RFC 20
  (planned) owns with [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values),
  beyond the open-state and capacity errors of §5.6, whose retry meaning is a
  protocol choice.

## 2. Versions, transport and sizes

Before any file is touched, client and server agree on a protocol version, a
transport (TCP or UDP) and the largest read or write; the server chooses what to
offer. This one offers NFSv3, NFSv4.0, NFSv4.1 and part of NFSv4.2, over TCP
only, with one fixed 1 MiB transfer size.

### 2.1 Versions served

NFS is a family of RPC (remote procedure call) programs, each versioned on its
own: NFS; MOUNT, which hands an NFSv3 client a share's root handle; NLM and NSM,
NFSv3's lock and crash-notification side protocols (§6). NFSv4 needs no side
program; it sends operations in batches (`COMPOUND`s), each naming its minor
version.

| Program, version | Served | Notes |
| --- | --- | --- |
| NFS v2 | no | `PROG_MISMATCH` naming 3–4 |
| NFS v3 (RFC 1813), MOUNT v3, NLM v4, NSM v1 | yes | §6 |
| NFS v4.0 (RFC 7530) | yes | §4 |
| NFS v4.1 (RFC 8881) | yes | sessions §3; pNFS (parallel NFS, clients reading data servers directly) per [RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS) |
| NFS v4.2 (RFC 7862, RFC 8276) | the subset of §9.3 | an operation outside it returns `NFS4ERR_NOTSUPP` |

- A `COMPOUND` whose minor version is above 2 **MUST** be answered
  `NFS4ERR_MINOR_VERS_MISMATCH` with no operation run.
- The minor version is the client's choice per `COMPOUND`; the server
  **MUST NOT** remember one per client, so a client that falls back from 4.2
  to 4.1 needs no new client ID.

> decision: NFSv4.0 is served although NFSv4.1 does everything it does, because
> some clients and appliances are pinned to it (`vers=4.0`). The cost: it is the
> version that recovers worst when a `protocol` node dies (§4.2). Withdraw it
> when no supported client needs it; nothing else in this document depends on it.

### 2.2 TCP only

- Every NFS program — NFS, MOUNT, NLM, NSM — **MUST** be served over TCP and
  **MUST NOT** be served over UDP.
- The port mapper (the service a client asks which port each program listens
  on), where the server runs one, answers on UDP and TCP, because clients probe
  it there before choosing a transport. It reaches no file.
- `SM_NOTIFY`, NSM's "I restarted" message, is sent the way NSM peers listen
  for it (§6.2).

> decision: no UDP. NFS over UDP has no congestion control and reorders. At the
> transfer sizes of §2.3 every request is split into many IP fragments, and when
> the fragment ID counter wraps, pieces of different requests can be
> reassembled together: data is corrupted and nothing reports it. RFC 7530 and
> RFC 8881 already require a congestion-controlled transport for NFSv4, and an
> NFSv3 client that defaults to UDP falls back to TCP when the port mapper
> offers only TCP. Revisit only for a client population that cannot speak TCP,
> which no supported client is. RDMA (remote direct memory access) transports
> are not offered either; see §14.

### 2.3 Transfer sizes

The client learns at mount the most data one read or write may carry. Every
such limit derives from one fixed value: **1 MiB** of data, plus the RPC and
compound overhead for the limits that include headers. That covers NFSv4's
`maxread` and `maxwrite`, NFSv3 `FSINFO`'s `rtmax`, `wtmax`, `rtpref` and
`wtpref`, and a session's largest request and reply (`ca_maxrequestsize`,
`ca_maxresponsesize`). The preferred directory-read
size (`dtpref`) is 64 KiB; the largest file (`maxfilesize`) is 2^63 − 1.

The size is fixed, not a setting ([RFC 13](rfc-13-configuration.md)). A client
negotiates down, never up, so a value that differs between nodes breaks a client
moved between them: build01, told 1 MiB by P1, would go on sending 1 MiB writes
to P2 after an address takeover.

> ponytail: one transfer size for every client and every share. 1 MiB already
> makes per-request overhead small on a fast link, and each request in flight
> holds one buffer of that size (RFC 24, planned), so a larger size costs
> memory. Raise it, or make it per share, when a benchmark on the reference box
> shows large sequential I/O limited by request count rather than by the journal.

## 3. NFSv4.1 sessions

A client that hears no reply sends the request again. For a read that is
harmless; for "create if absent" or "remove this name" the second run answers
differently. NFSv4.1's **session** fixes this: each request goes in one of a
fixed number of **slots** with a sequence number, and the server keeps the last
reply per slot — the **reply cache** — so a resend gets the stored answer. The
server chooses how many slots, how much reply to keep, and whether the cache
survives a crash. This one grants up to 64 slots and keeps the cache in the
memory of the `protocol` node holding the connection, accepting that a node loss
turns resends of in-flight requests into second runs.

### 3.1 Slot tables

A session has two directions: the **fore channel**, for client requests, and the
**back channel**, for the server's calls to the client (§3.4). The slot count of
a channel is how many requests may be in flight on it at once.

- The fore channel offers at most **64 slots**, the back channel at most **16**.
  The call that opens a session (`CREATE_SESSION`) grants the lesser of what the
  client asks and these.
- Under memory pressure the server **MAY** ask the client to use fewer slots (by
  lowering `sr_target_highest_slotid` in its replies), and **MUST** honour a
  client that then retires slots. It **MUST NOT** shrink the table
  (`sr_highest_slotid`) below a slot with a request in flight.
- The largest reply the server will cache (`ca_maxresponsesize_cached`) is the
  lesser of what the client asks and **8 KiB**, so one session's reply cache
  never exceeds 64 slots × 8 KiB = 512 KiB on the fore channel, whatever the
  client asks.
- A slot keeps a reply only when the client asked for that request to be cached
  (`sa_cachethis`), and then keeps the whole reply. A request asking to be
  cached whose reply exceeds the cached size is answered
  `NFS4ERR_REP_TOO_BIG_TO_CACHE` and not run (RFC 8881 §2.10.6.4).
- A request not asked to be cached leaves only its sequence ID in the slot. A
  retransmission of it — same slot, same sequence ID — **MUST** be answered
  `NFS4ERR_RETRY_UNCACHED_REP`, never run again and never answered from a
  partial reply: a part of a reply returned as if it were the whole tells the
  client operations failed or succeeded that did neither.

> ponytail: 8 KiB per cached reply covers every non-idempotent operation's
> reply with its attributes, and bounds a session at 512 KiB however many
> clients ask for more. A client that wants a cached `READ` reply is told it is
> too big and asks again uncached. Raise the cap when a supported client is
> shown asking to cache replies larger than 8 KiB for operations that are not
> idempotent.

> ponytail: 64 slots caps one client at 64 requests in flight per session. A
> client that needs more opens a second session or uses several nodes at once
> (§3.3). Raise the cap, or size it from a memory budget, when a single-client
> benchmark is limited by slots.

### 3.2 The reply cache is per session, in memory

- The reply cache — the last reply per slot, by sequence ID — **MUST** be held
  in the memory of the `protocol` node that holds the session, and **MUST NOT**
  be written to the metadata store or replicated.

When that node dies, its sessions go with it. The client reconnects to a
surviving node through the floating address
([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)) and is answered
`NFS4ERR_BADSESSION`. It opens a new session (`CREATE_SESSION`) under its
existing client ID, which every node accepts because the client record is
durable ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)), and carries on. Its opens, locks and
delegations are untouched: they are held by the primaries, not by the
`protocol` node ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)), and no grace runs.

What the client loses is exactly-once for the requests that were in flight. It
resends them on the new session, in new slots, and they run again. For build01
when P1 dies mid-untar:

| Request in flight | Seen by the client on retry |
| --- | --- |
| `READ`, `GETATTR`, `LOOKUP`, `READDIR` | the same answer, or a newer one |
| `WRITE` at the same offset | the same bytes written again: harmless unless another client wrote the range in between |
| `CREATE` / `OPEN` with `EXCLUSIVE4_1` | success: the stored verifier matches ([RFC 7 §2.10](rfc-7-namespace-metadata.md#2.10%20Exclusive%20create)) |
| `CREATE` / `OPEN` `GUARDED`, `MKDIR`, `LINK`, `SYMLINK` | `NFS4ERR_EXIST`, for a directory build01 did make |
| `REMOVE` | `NFS4ERR_NOENT`, for a file build01 did remove |
| `RENAME` | `NFS4ERR_NOENT`, or success if the target name is back |
| `LOCK`, `OPEN` upgrade | `NFS4ERR_OLD_STATEID` or `NFS4ERR_BAD_STATEID` on the stale sequence number; the client re-reads its state |

The duplicate filter on forwarded calls (the route envelope's dedup table,
[RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope),
[RFC 17 §4.7](rfc-17-vfs.md#4.7%20Callable%20across%20the%20network)) does
**not** help here. It recognises a request by an ID the forwarding node mints,
and build01's resend arrives through P2, which mints a new one. It catches
retries between a `protocol` node and a primary, not between a client and a
`protocol` node.

> decision: the reply cache is not durable. Storing it would cost one
> replicated write per non-idempotent request — every create and rename, about
> 40,000 extra writes for build01's untar — to protect against one event, a
> `protocol` node dying with requests in flight, whose visible result is the
> same as an NFSv3 server reboot, which every client already tolerates. Revisit
> when users report client-visible errors after a `protocol` node loss; the fix
> then is to key the primary's dedup table by (client ID, session, slot,
> sequence ID) carried in the envelope, which survives a `protocol` node but not
> a session.

### 3.3 Trunking, server owner and scope

**Trunking** is one client using several connections, even to several
addresses, at once. The client decides what it may combine from what each
address reports when it identifies itself (`EXCHANGE_ID`): a **server scope**
(`server_scope`) and a **server owner** whose major ID (`so_major_id`) says "one
client ID is valid across these" and minor ID (`so_minor_id`) "one session is".
All `protocol` nodes here are one server for client IDs and separate servers for
sessions, because a session's slots live in one node's memory (§3.2).

- Every `protocol` node of an installation **MUST** return the same
  `server_scope` and the same `so_major_id`, and **MUST** return a
  `so_minor_id` unique to the node: the major ID and scope are the
  installation's identity
  ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)),
  the minor ID the node's own
  ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)).
- **Client ID trunking** across nodes is allowed: one client ID, sessions on
  several nodes, each node's connections bound to its own sessions.
- **Session trunking** — one session with connections to several nodes — is
  refused: binding a connection to a session (`BIND_CONN_TO_SESSION`), or a
  `SEQUENCE` naming a session, that this node does not hold is answered
  `NFS4ERR_BADSESSION`. Session trunking across connections to one node is
  allowed.

`EXCHANGE_ID` also tells the client which roles a node plays: every node sets
`EXCHGID4_FLAG_USE_NON_PNFS`, and also `EXCHGID4_FLAG_USE_PNFS_MDS` where the
deployment has a data server role; data servers return
`EXCHGID4_FLAG_USE_PNFS_DS` ([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS)).
For state protection — whether only the machine that created a client ID may
act on it — `SP4_NONE` (no check) and `SP4_MACH_CRED` (the machine's
credential is checked) are accepted; `SP4_SSV` (a secret negotiated in-band) is
refused with `NFS4ERR_ENCR_ALG_UNSUPP`.

**What the client record carries for NFS.** Every node answers `EXCHANGE_ID`,
`SETCLIENTID` and `DESTROY_CLIENTID` for any client, so everything those calls
decide **MUST** be in RFC 14's durable client record
([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)), not in one node's memory:

| NFS state | Held in the client record as | Used by |
| --- | --- | --- |
| the owner string (`co_ownerid`, `nfs_client_id4`) | `Owner`, indexed by (protocol, owner) | `EXCHANGE_ID` / `SETCLIENTID` from a client that rebooted or reconnected through another node finds its record, and its verifier tells which |
| the boot verifier | `Verifier` | a new verifier under the same owner is a new client instance |
| the NFSv4.0 confirm verifier | nothing: it is a keyed digest of the client ID and `Verifier` under the installation secret, which every node recomputes | `SETCLIENTID_CONFIRM` on any node |
| the principal that created the client ID | `Principal` | an `EXCHANGE_ID` or `SETCLIENTID` for the same owner from another principal is `NFS4ERR_CLID_INUSE` (RFC 8881 §18.35.5) |
| `SP4_MACH_CRED` | `MachCred`: the machine principal and the operations it protects | each node enforces state protection the same way |
| the back-channel node and whether the path is down | `Callback`, `PathDown` (§3.4) | callbacks, `SEQ4_STATUS_CB_PATH_DOWN` |
| the nodes holding a session of this client | `Sessions`: one entry per node, added at `CREATE_SESSION`, removed at the node's last `DESTROY_SESSION` or with the node's loss | `DESTROY_CLIENTID` is `NFS4ERR_CLIENTID_BUSY` while any entry remains (§5.1) |
| the shards it has held state in, with the grace instance of its last `RECLAIM_COMPLETE` in each | `Shards` and the record's per-shard entry ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)) | §5.4 |

The last row is cluster-only: on one node the list is local. Without it, a
node asked to destroy a client would see none of the client's sessions on other
nodes and release state they still use.

### 3.4 The back channel and where callbacks go

Sometimes the server must call the client: to take back a delegation
(`CB_RECALL`) or a pNFS layout (`CB_LAYOUTRECALL`), to report a finished copy
(`CB_OFFLOAD`), or to ask for some delegations back when it holds too many
(`CB_RECALL_ANY`). These **callbacks** start at the primary of the file's shard,
but the client's back channel is held by a `protocol` node, so the primary goes
through the `Callbacks` the client registered ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). The
registration names the `protocol` node holding the client's back channel, and
the call travels primary → that node → the client's connection.

- The registration **MUST** be held in the durable client record, as the node
  that holds the client's back channel (`Callback`,
  [RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)), so
  every primary routes a callback the same way whichever node it last heard the
  client through. A node's in-memory list is not enough: a primary on another
  node would never see a rebind (cluster; the
  [single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)
  has one node and one list).
- A client's registration **MUST** be replaced, not added to, when it binds a
  back channel on another node: the latest wins. After a `protocol` node loss,
  the client's new session with a back channel (`CREATE_SESSION` with
  `CDFC4_BACK`) or `BIND_CONN_TO_SESSION` redirects every later callback,
  through the service's `Rebind`
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)), which writes the new node into
  the record's `Callback`, and clears `PathDown`, before the reply.
- Until it does, a callback has nowhere to go and fails. The primary treats that
  as the client not answering, and the recall is revoked at its deadline
  ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).
- The client's back channel is **down** when the node its record names is lost,
  or that node reports the channel's connection gone. The record carries that
  state, so whichever node answers the client's next `SEQUENCE` **MUST** set
  `SEQ4_STATUS_CB_PATH_DOWN` in it, and no delegation is offered to the client
  ([RFC 14 §5.4](rfc-14-open-state.md#5.4%20How%20a%20grant%20is%20obtained%2C%20and%20where%20it%20pays))
  until a rebind clears it.
- The back channel's security is `AUTH_NONE` or `AUTH_SYS` (no credential, or
  plain user and group IDs), as the client asked in `CREATE_SESSION`
  (`csa_sec_parms`); Kerberos (`RPCSEC_GSS`, §10) on the back channel is not
  offered.

Example: build01 holds a delegation on `builds/Makefile` from S1, and P1 dies.
Until build01 opens a session with a back channel on P2, S1's recall cannot reach
it; afterwards the registration names P2 and the recall arrives there.

## 4. NFSv4.0 clients

NFSv4.0 has no sessions. The client identifies itself with `SETCLIENTID`, which
also names an address where the server could call it back; and instead of slots, each
**open owner** and **lock owner** (an identity on the client holding opens or
locks) numbers its requests, and the server replays its last reply to a resend.
All of it maps onto the same records as NFSv4.1. The replay lives in a
`protocol` node's memory like the reply cache, and fares worse on a node loss.

### 4.1 SETCLIENTID and the callback path

- `SETCLIENTID` and `SETCLIENTID_CONFIRM` map onto the client record as
  `EXCHANGE_ID` and `CREATE_SESSION` do: the client's `nfs_client_id4` is its
  `Owner`, its verifier tells a reboot
  ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)), and the confirm verifier
  is derived from the client ID and verifier (§3.3), so any node can confirm.
- The callback address (`r_addr`) is accepted and not recorded, and no callback
  connection is opened. The NFSv4.0 callback program carries only delegation
  calls (`CB_GETATTR`, `CB_RECALL`), and delegations are not offered to
  NFSv4.0 clients (§5.3), so the server never has a call to make. `RENEW`
  therefore never answers `NFS4ERR_CB_PATH_DOWN`.

### 4.2 Owner sequence IDs

NFSv4.0 numbers `OPEN`, `OPEN_CONFIRM`, `OPEN_DOWNGRADE` and `CLOSE` per open
owner, and `LOCK` and `LOCKU` per lock owner, and replays the last reply to a
retransmission.

- The **next expected sequence ID** of each owner **MUST** be held in RFC 14's
  owner sequence record (`OwnerSeq.Next`, at the owner's home primary,
  [RFC 14 §2.8](rfc-14-open-state.md#2.8%20NFSv4.0%20owner%20sequences)), and
  advanced in the step that applies the operation, so every node agrees on it.
- Whether a new open owner has been confirmed is held in the same record: the
  first `OPEN` of an owner creates its `OwnerSeq` with `Confirmed` false, and
  only `OPEN_CONFIRM` sets it
  ([RFC 14 §2.8](rfc-14-open-state.md#2.8%20NFSv4.0%20owner%20sequences)).
  Until then only `OPEN_CONFIRM` is accepted from the owner.
- The **last reply** per owner is held only in the `protocol` node that sent
  it, as the session reply cache is (§3.2).

After a `protocol` node loss, a retransmission of an owner's last operation
finds its sequence ID already used and no stored reply. The server answers
`NFS4ERR_BAD_SEQID`, and the client recovers that owner's state.

> decision: NFSv4.0 owner replay is best-effort across a `protocol` node loss,
> for the reason of §3.2. It is worse than NFSv4.1's, because one owner's error
> costs the client every open under that owner, not one request. NFSv4.1 is the
> supported path for clients that need to ride out a node failure; withdraw
> this when §2.1 withdraws NFSv4.0.

`OPEN_CONFIRM` is required for a new open owner, as RFC 7530 §16.18 specifies;
`RELEASE_LOCKOWNER` releases the lock owner's record at the primary.

## 5. Client state on the wire

What a client holds — client ID, opens, locks, delegations, layouts — is
[RFC 14](rfc-14-open-state.md)'s records, kept at each file's primary. The
server chooses how long a silent client keeps them, how their tokens are built,
which delegations to offer and how recovery is signalled. Here: a 90 s lease
with no courtesy, tokens any node resolves, file delegations but no directory
ones, grace per shard.

### 5.1 Client ID, lease time, expiry

A **lease** is how long a client's state is kept while it is silent.

- `lease_time` is **90 s**, fixed ([RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)). Every `SEQUENCE`, and
  every NFSv4.0 `RENEW` or stateful operation, renews it.
- The client ID is minted from the client record and is valid on every node.
- Ending a client (`DESTROY_CLIENTID`) maps to `Disconnect` ([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), RFC 14's
`Destroy`, and is refused with
  `NFS4ERR_CLIENTID_BUSY` while the client still holds a session on any node,
  as RFC 8881 §18.50 requires; the answering node reads that from the client
  record (§3.3), not from its own sessions. `DESTROY_SESSION` drops only the
  node's session.

**No courtesy clients.** A client whose lease expires has every open, lock,
delegation and layout released at once, in every view
([RFC 14 §4.3](rfc-14-open-state.md#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)), whether or not another client wants them. Its next request is
answered `NFS4ERR_EXPIRED` (for a stateid) or `NFS4ERR_STALE_CLIENTID` /
`NFS4ERR_BADSESSION` (for a client ID or session), and it starts over.

If build01 is paused for two minutes while holding a lock on `builds/.lock`,
the lock is gone when it resumes, even though nobody else asked for it.

> decision: no courtesy clients. Keeping an expired client's state until
> someone else wants it would let a laptop that slept through its lease wake up
> with its locks intact. It costs a second, conditional release path — state
> that is both expired and still held — at every primary and in the
> cross-protocol checks of RFC 14 §7, where an SMB open would have to expire
> NFS state on conflict. The ceiling: a client silent for 90 s loses its locks
> even when nobody wanted them. Revisit if that shows up as lost locks on
> suspended clients.

### 5.2 Stateids

A **stateid** is the 16-byte token the server returns for each open, lock,
delegation, layout or copy; the client quotes it on every later read, write,
lock or close. It has two parts: `other`, 12 bytes saying *which* state, and
`seqid`, a counter that moves each time that state changes. The encoding is the
adapter's ([RFC 17 §4.2](rfc-17-vfs.md#4.2%20Translation%20stays%20in%20adapters)), under five rules:

1. **`other` names a record, not a table slot.** It **MUST** be a type tag and
   the RFC 14 identifier it stands for — open, a lock owner's lock state,
   delegation, layout or copy — so any `protocol` node resolves it without a
   table of its own, and a stateid survives a `protocol` node loss.
2. **`other` cannot be guessed.** At least **64 bits** of it **MUST** come from
   a random source, so a client that sees its own stateids learns nothing that
   names another's. The identifiers RFC 14 mints for these records carry those
   bits ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)); a counter or a
   timestamp in their place makes every neighbouring stateid reachable by
   arithmetic.
3. **`seqid` is held with the state at the primary**, as a field of the record
   the stateid names, changed in the step that applies the request
   ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)): an open's `StateSeq`,
   raised by every `OPEN` upgrade, `OPEN_DOWNGRADE` and `CLOSE`; a lock state's
   `LockState.Seq`, raised by every `LOCK` and `LOCKU` that changes the locks of
   one (open, lock owner) pair; and the layout's, raised by `LAYOUTGET` and
   `LAYOUTRETURN`. A delegation is never changed in place, so its stateid keeps
   the `seqid` it was granted with. A request with an older `seqid` is
   `NFS4ERR_OLD_STATEID`, a newer one `NFS4ERR_BAD_STATEID`. NFSv4.1's `seqid` 0
   means "current" (RFC 8881 §8.2.2).
4. **A stateid is checked against its client** wherever the request names one
   — the client ID of the session, or for NFSv4.0 the client ID in the open or
   lock owner of a sequenced operation (`OPEN`, `CLOSE`, `OPEN_DOWNGRADE`,
   `LOCK`, `LOCKU`) — and one belonging to another client is
   `NFS4ERR_BAD_STATEID` (`ErrNotYours`).
5. **Over NFSv4.0, I/O is checked against the open's principal.** An NFSv4.0
   `READ`, `WRITE` or `SETATTR` carries no client ID and no session, so rule 4
   cannot run there. Instead the request's credential **MUST** name the
   principal that made the open the stateid names (`Open.Principal`,
   [RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)); the service refuses one
   naming any other principal with `ErrNotYours`, which on these three
   operations is answered `NFS4ERR_ACCESS` (§5.6), and the stateid grants it
   nothing. A lock
   stateid is checked against the principal of the open it was taken under.
   The special stateids below name no open and are checked per operation as
   anonymous I/O is.

> decision: rule 5 trusts the credential the request carries. Under Kerberos
> that is an authenticated principal and the check closes the hole: a stateid
> quoted by another user is refused. Under `AUTH_SYS` the user and group IDs
> are asserted by the client and not proven, so a host that can send packets
> as the opener's uid still reaches the opener's access; it could equally
> reach that user's files with no stateid at all, since `AUTH_SYS` admission
> trusts the same assertion ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)).
> The check binds a stateid to one principal, never to one machine: a client
> that reconnects from a new address keeps its opens. Withdraw the `AUTH_SYS`
> ceiling only by requiring Kerberos on the share, which is the share's
> setting, not this rule's.

Three stateid values are special:

- **all zeros** is an anonymous open — I/O with no open, as NFSv3 does
  ([RFC 17 §4.1](rfc-17-vfs.md#4.1%20The%20operation%20set%20is%20the%20union%2C%20not%20the%20intersection)) — checked per operation against deny modes;
- **all ones** bypasses no check: it is treated as anonymous for `READ` only and
  refused for `WRITE`;
- the **current stateid** (NFSv4.1) — "the one the previous operation in this
  `COMPOUND` returned" — is resolved inside the compound.

Asking whether stateids are still valid (`TEST_STATEID`) answers per stateid
without changing state. `FREE_STATEID` releases a lock state with no locks held,
or a revoked delegation or layout the client acknowledges, and is refused with
`NFS4ERR_LOCKS_HELD` otherwise. `RECLAIM_COMPLETE` maps to `ReclaimComplete`
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)).

### 5.3 Delegations

A **delegation** is the server's promise that no other client is using a file,
so the client may cache it without asking. Delegations are RFC 14's caching
grants: a read delegation is a read grant, a write delegation a read and write
grant, and neither is ever a handle grant, which only SMB offers
([RFC 14 §5.1](rfc-14-open-state.md#5.1%20What%20a%20grant%20is)). Offering
them at all is the server's choice; this one offers both file kinds to NFSv4.1
and later, none to NFSv4.0, and no directory ones.

- Both are offered to NFSv4.1 and NFSv4.2 clients, under [RFC 14 §5.4](rfc-14-open-state.md#5.4%20How%20a%20grant%20is%20obtained%2C%20and%20where%20it%20pays),
  and the client's stated wishes (`OPEN4_SHARE_ACCESS_WANT_*` flags) are honoured.
- **NFSv4.0 clients are offered none**: every v4.0 `OPEN` returns
  `OPEN_DELEGATE_NONE`.
- `CB_RECALL` is the recall, `DELEGRETURN` the client handing it back. A recall
  not answered by its deadline, one client lease period
  ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)),
  is revoked; the client is told by `SEQ4_STATUS_RECALLABLE_STATE_REVOKED`, its
  delegation stateid is `NFS4ERR_DELEG_REVOKED` until freed, and writes it sends
  under the revoked delegation are refused. That client is offered no delegation
  for the rest of its lease, and its other delegations **SHOULD** be recalled.
- `CB_RECALL_ANY` is sent when the primary's grant table is over its budget.
- **A lock request recalls a delegation.** A byte-range lock request from any
  other client — NFSv4 `LOCK`, an NLM lock (§6.1), an SMB lock — on a file under
  a read or write delegation **MUST** recall it to none before the lock is
  decided, as RFC 14 requires of every grant with read or write caching
  ([RFC 14 §7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols); the
  SMB form is MS-FSA 2.1.5.7). A delegation lets its holder take locks locally;
  without the recall, the holder's local lock and the other client's lock are
  both granted over the same range, and neither is told.

> decision: no delegations for NFSv4.0 clients. A v4.0 recall needs the
> separate callback connection of §4.1, opened from the node that last heard
> from the client to an address the client chose, with none of §3.4's
> rebinding after a node loss; and a v4.0 stateid is checked against a
> principal only, not a client (§5.2), so a write delegation would be the
> widest grant such a stateid could carry. The cost is local caching for
> clients pinned to v4.0. Revisit if §2.1's pinned clients show the extra round
> trips in a benchmark.

Example: build01 holds a write delegation on `builds/Makefile` and edits it
locally. A second NFS client opens the same file for writing; S1 recalls the
delegation, build01 writes back its changes and returns it, and only then is
the second open granted.

**Directory delegations are not offered.** A request for one
(`GET_DIR_DELEGATION`) returns `GDD4_UNAVAIL` with `will_signal_deleg_avail`
false, so NFSv4.1 directory notifications, which ride on them, are never sent.
RFC 14's Watches (change notifications on a directory) reach NFS clients through
nothing.

> decision: no directory delegations. Linux and other common clients do not
> request them, and each one costs a watch plus a grant broken by every create
> in the directory — for `builds`, every file of an untar. Revisit when a
> client that uses them is supported; the record is already RFC 14's Watch.

### 5.4 Grace on the wire

After a primary fails over, its shard enters a **grace period**: clients
reclaim what they held, and nothing new is granted meanwhile. Grace is per shard
([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)), so a
failover of `builds`' primary leaves every other shard serving normally. On the
wire:

| Situation | Answer |
| --- | --- |
| a non-reclaim request refused during grace (`ErrGrace`) | `NFS4ERR_GRACE` (NFSv4), `NLM4_DENIED_GRACE_PERIOD` (NLM) |
| a reclaim outside grace, or by a client the record does not name for the shard | `NFS4ERR_NO_GRACE` |
| a reclaim in grace of state the client did not hold (`ErrNoReclaim`) | `NFS4ERR_RECLAIM_BAD` |
| a reclaim sent after that same client's `RECLAIM_COMPLETE` for the same shard and the same grace instance (RFC 8881 §18.51.3) | `NFS4ERR_NO_GRACE` |
| reclaim an open | `OPEN` with `CLAIM_PREVIOUS` |
| reclaim a lock | `LOCK` with `reclaim` set |
| reclaim a delegation (`CLAIM_PREVIOUS` with a delegation type) | answered without one: a delegation is not reclaimed ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)) |

**Reclaim is tracked per shard and per grace.** Clients send
`RECLAIM_COMPLETE` once at mount, when there is nothing to reclaim, so a rule
that counted one `RECLAIM_COMPLETE` for the client's lifetime would refuse
every reclaim after the first failover. Instead:

- Every grace a shard enters is a new **grace instance**. A `RECLAIM_COMPLETE`
  **MUST** be recorded per (client, shard, grace instance), for every shard
  the client's record names that is in grace when it arrives
  ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)); a
  grace instance is named by the shard incarnation the new primary serves
  under. It closes reclaim for those instances
  only. A later failover starts a new instance, in which the same client may
  reclaim again.
- When a shard the client's record names fails over, or the server restarts,
  every `SEQUENCE` reply to that client **MUST** carry
  `SEQ4_STATUS_RESTART_RECLAIM_NEEDED` until the client sends
  `RECLAIM_COMPLETE` for the new instance. That flag, not a revocation flag,
  is what tells an NFSv4.1 client whose session survived to reclaim: a client
  told its state was revoked frees it instead.
- The client then reclaims everything it holds, in every shard, since NFSv4.1
  reclaims per client, not per shard. A reclaim reaching a shard not in grace,
  of state that shard still holds for that client, **MUST** be answered with
  that state as it stands, granting nothing new; any other reclaim there is
  `NFS4ERR_NO_GRACE`. Otherwise a client asked to reclaim by one shard's
  failover would lose its opens in every healthy shard.
- NFSv4.0 has no session flag: a failover answers that client's stateids in
  the shard `NFS4ERR_STALE_STATEID`, as
  [RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard) says, and
  the client reclaims.

The other protocols' signals are RFC 14 §4.4's too.

### 5.5 Lock conflicts on the wire

A refused NFSv4 `LOCK` or `LOCKT` is `NFS4ERR_DENIED` with a `LOCK4denied`
naming the conflicting range, its type and its holder: a client ID and an
opaque owner. The holder is RFC 14's lock owner, whose owner bytes are each
protocol's own ([RFC 14 §2.3](rfc-14-open-state.md#2.3%20Lock)).

- When the holder is an NFSv4 lock owner, `LOCK4denied` carries its client ID
  and the owner bytes that client sent.
- When the holder is anything else — an NLM host (§6.1) or an SMB open
  ([RFC 22 §7](rfc-22-smb.md#7.%20Byte-range%20locks)) — `LOCK4denied`
  **MUST** carry client ID 0 and an empty owner.
- An internal owner identity **MUST NOT** reach the wire. It is not an NFSv4
  owner, can exceed the protocol's 1024-byte owner limit (`NFS4_OPAQUE_LIMIT`),
  and a reply carrying an over-long owner fails to decode, so the client fails
  the call instead of seeing a conflict.

### 5.6 Open-state and capacity errors on the wire

RFC 20 (planned) owns the full error table; these are mapped here because
which answer makes a client retry, reclaim or give up is a protocol choice.
NLM codes are for NFSv3 locking (§6).

| Service error | NFSv4 | NFSv3 / NLM |
| --- | --- | --- |
| `ErrDelay` — a recall in progress, or the journal full until offload or repack frees space | `NFS4ERR_DELAY` | `NFS3ERR_JUKEBOX`; NLM: `NLM4_BLOCKED` for a blocking lock, else `NLM4_DENIED` |
| `ErrNoSpace` — nothing will free space without an operator (the held bytes all dirty and the store refusing puts, a retention pin, the device full), or the share's limit | `NFS4ERR_NOSPC` | `NFS3ERR_NOSPC` |
| `ErrQuota` | `NFS4ERR_DQUOT` | `NFS3ERR_DQUOT` |
| `ErrGrace` | `NFS4ERR_GRACE` | `NFS3ERR_JUKEBOX`; NLM: `NLM4_DENIED_GRACE_PERIOD` |
| `ErrNoReclaim` | `NFS4ERR_RECLAIM_BAD` in grace, `NFS4ERR_NO_GRACE` outside it | NLM: `NLM4_DENIED_GRACE_PERIOD` in grace, else `NLM4_DENIED` |
| `ErrStaleClient` | `NFS4ERR_EXPIRED` for a stateid; `NFS4ERR_STALE_CLIENTID` for a client ID | NLM: `NLM4_DENIED` |
| `ErrNotYours` | `NFS4ERR_ACCESS` for NFSv4.0 `READ`, `WRITE` and `SETATTR`, where only the principal is checked (§5.2); `NFS4ERR_BAD_STATEID` otherwise | — |
| `ErrBadSeqID` | `NFS4ERR_BAD_SEQID` | — |
| `ErrLocked` | `NFS4ERR_DENIED` for `LOCK` / `LOCKT`; `NFS4ERR_LOCKED` for I/O | `NFS3ERR_ACCES`; NLM: `NLM4_DENIED` |
| `ErrShareViolation` | `NFS4ERR_SHARE_DENIED` for `OPEN`; `NFS4ERR_LOCKED` for I/O | `NFS3ERR_ACCES`; NLM: `NLM4_DENIED` for `NLM4_SHARE` |
| `ErrDeletePending` | `NFS4ERR_ACCESS` | `NFS3ERR_ACCES` |
| `ErrBadLayout` | `NFS4ERR_BADLAYOUT` | — |
| `ErrNoCopy` | `NFS4ERR_BAD_STATEID` (§9.4) | — |

A full journal is the one capacity condition that clears by itself, so it is
the only one answered "retry", and it is answered "retry" for as long as offload
or repack can still free space, never "no space" because a deadline ran out: a
refusal that would outlast the caller's deadline is `ErrDelay` at once
([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)).
A share limit or quota is answered at once and never as `ErrDelay`.

## 6. NFSv3 locking: NLM and NSM

NFSv3 holds no state, so locking lives in two side protocols. **NLM** (Network
Lock Manager) takes and releases byte-range locks. **NSM** (Network Status
Monitor) handles crashes: each side's status daemon (`statd`) records whom it
holds locks with and, after a restart, sends them `SM_NOTIFY` so they reclaim.
Here NLM locks are RFC 14 locks, conflicting with NFSv4 and SMB locks, and
`SM_NOTIFY` is sent when a shard fails over, not when a `protocol` node dies.

### 6.1 NLM

NLM v4 maps onto RFC 14: the NLM host — its name (`caller_name`) **and the
source address it sends from** — is the client, an `(svid, oh)` pair — process
ID and owner handle — the lock owner, and each lock an RFC 14 lock under an
anonymous open of the file. The records, including what NSM needs to notify a
client, are RFC 14's.

- **A host is bound to its address.** NLM carries no credential that proves
  which host sent a request: `caller_name` is whatever the sender writes. The
  client record of an NLM host **MUST** be keyed by `caller_name` together with
  the source address of the request that created it: both are its `Owner`
  ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)), and the host's NSM state
  number completes its `ClientID`. A request naming that
  `caller_name` from another address is another client, and can neither test,
  unlock nor free the first host's locks. `NLM4_FREE_ALL` and NSM's
  `SM_NOTIFY` for a host release its locks only when they arrive from the
  address its record holds.
- **Replies leave from the address the client used.** Callbacks to the host —
  `NLM4_GRANTED`, `NLM4_GRANTED_MSG`, the `*_RES` replies — **MUST** be sent
  from the floating address the host's requests arrived on, never from a
  node's own address, because clients match a callback against the server
  they sent the lock to and drop one from any other address.

> decision: binding a host to its source address stops one host from
> releasing another's locks by naming it, which is all NLM's design allows.
> It does not stop a sender that forges the source address on the same
> network; NLM has no other identity to check, and every NLM server shares
> that ceiling. A host whose address changes — a DHCP renewal — is a new
> client, and its old locks stay until revoked, as after any NLM host loss
> ([RFC 14 §4.5](rfc-14-open-state.md#4.5%20NLM%20locks%20and%20restart%20notification)).
> Withdraw only for a deployment whose NLM hosts authenticate, which NLM
> cannot express.

- **Blocking locks.** A lock request that may wait (`NLM4_LOCK` with `block`
  set) and conflicts returns `NLM4_BLOCKED`. The primary keeps the waiter, and
  on release the `protocol` node holding the client's registration sends
  `NLM4_GRANTED_MSG` / `NLM4_GRANTED`. A waiter not granted within one lease
  period is dropped, and the client's own retry re-queues it. No worker waits
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)).
- **Grace.** NLM shares RFC 14's per-shard grace with NFSv4: during it a
  non-reclaim lock is `NLM4_DENIED_GRACE_PERIOD`, and a request with `reclaim`
  set is a reclaim.
- **Share reservations** (`NLM4_SHARE`), which DOS-era clients use, map to an
  open with a deny mode, so they conflict with SMB and NFSv4 deny modes like
  any other.
- **Who holds a conflict.** An NLM lock refusing an NFSv4 `LOCK` or `LOCKT`
  is reported with client ID 0 and an empty owner (§5.5). In the other
  direction, an `NLM4_TEST` refused by an NFSv4 or SMB lock reports a holder
  with `svid` 0 and an empty `oh`, for the same reason: an internal owner
  never reaches the wire.
- `NLM4_FREE_ALL` from the host's own address expires the client.
- The asynchronous procedures (`*_MSG` / `*_RES`) are accepted over TCP and
  answered on the client's NLM callback service, from the floating address.
- An NLM lock request from another host on a file under a delegation recalls
  the delegation first (§5.3).

### 6.2 NSM

The server monitors clients as its peers expect: a client statd's request to be
monitored (`SM_MON`) is recorded with the client, as its `Notify`
([RFC 14 §4.5](rfc-14-open-state.md#4.5%20NLM%20locks%20and%20restart%20notification)).

- After a failover of a shard in which any NLM host held locks, the
  installation's NSM state number is raised, durably, before the shard's grace
  begins ([RFC 14 §4.5](rfc-14-open-state.md#4.5%20NLM%20locks%20and%20restart%20notification)),
  and the new primary asks the `protocol` node currently holding each address
  to send `SM_NOTIFY` to every NLM host whose record names the shard, at its
  `Notify`, naming the server name the client monitored — the floating
  address's name, not a node's — with the new state number, so the client's
  statd reclaims locks.
- `SM_NOTIFY` is sent by UDP, as statd peers listen for it. This is the one UDP
  datagram the server emits, and it carries no file operation.
- A `protocol` node loss alone is not a restart for NLM: no lock was lost, so no
  `SM_NOTIFY` is sent ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)).

### 6.3 MOUNT

MOUNT v3 is the service's `Shares` group
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)): `MNT` is `Mount`, which
resolves the path and returns the root handle and the share's flavours; `DUMP`
is empty, and `UMNT` and `UMNTALL` change nothing; `EXPORT` is `ListShares`, the
shares [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)
lets the caller see.

## 7. Names, handles and directories

A client names a file by a **file handle**, pages through a directory by
**cookies** (resume positions), and sends names as bytes. All three are
[RFC 7](rfc-7-namespace-metadata.md)'s; this section fits them into NFS's fields
so each stays valid on every node and across restarts.

### 7.1 File handles

- A file handle is RFC 7's opaque handle, returned unchanged
  ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)).
  It fits NFSv3's 64 bytes and NFSv4's 128.
- Handle lifetime (`fh_expire_type`) is `FH4_PERSISTENT`, true by
  [RFC 7 §6.2](rfc-7-namespace-metadata.md#6.2%20A%20handle%20is%20stable%20across%20restart);
  a released file's handle is `NFS3ERR_STALE` / `NFS4ERR_STALE`.
- The file number (`fileid`) is RFC 7's stored numeric id (`Number`,
  [RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)),
  or for a file seen through a snapshot the snapshot's id built from its
  ordinal and `Number`; never the `FileID` the handle carries, and a named
  stream is never reachable over
  NFS (§8.2). The handle's `FileID` is a secret for NFSv3, where a handle is a
  bearer token ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)),
  so no other field of any protocol carries it ([RFC 22 §9.1](rfc-22-smb.md#9.1%20What%20is%20reported)).
- A snapshot is reached read-only under the share's browse directory, and over
  NFS each snapshot is its own filesystem: its `fsid` is derived from the share
  and the cut, and its root reports the browse directory's `mounted_on_fileid`
  ([RFC 12 §2.5](rfc-12-snapshots.md#2.5%20Browsing%20a%20snapshot)). A
  mutation there is `NFS3ERR_ROFS` / `NFS4ERR_ROFS`.

### 7.2 READDIR cookies and verifier

Each listed entry carries a 64-bit cookie; the client asks for the next page
"after cookie X". A cookie verifier lets the server say "start again". RFC 7
owns stability ([RFC 7 §3.5](rfc-7-namespace-metadata.md#3.5%20A%20cookie%20survives%20concurrent%20mutation));
on the wire:

- The cookie **MUST** be the digest RFC 7 orders the last entry returned by — a
  63-bit keyed hash of the entry's key under the share's hash key — unchanged,
  so any node resumes a listing another node began. RFC 7 orders a directory's
  listing by that digest, so a resume returns the entries whose digest is
  greater than the cookie, and needs no table mapping cookies back to keys; a
  cookie whose entry was removed during the listing still resumes at the right
  place. Cookies 0, 1 and 2 are reserved (start, and the `.` and `..` NFSv3
  synthesises), and RFC 7 raises a digest below 3 to 3.
- The top bit of the cookie is always clear, because some clients pass it to
  applications as a signed 64-bit directory offset.
- A reply **MUST NOT** end inside a collision chain, the entries sharing one
  digest: it ends before the chain or includes all of it, so the cookie alone
  says where to resume. A chain that does not fit in the client's `maxcount` at
  all is answered `NFS3ERR_TOOSMALL` / `NFS4ERR_TOOSMALL`. At 63 bits a chain of
  two is likely only past about 4·10⁹ entries in one directory.
- The cookie verifier is a constant of the share, fixed with its ordering when
  the share is created, and never changes on a mutation. A verifier other than
  the share's is `NFS3ERR_BAD_COOKIE` / `NFS4ERR_NOT_SAME`; a zero verifier with
  a non-zero cookie is accepted, since common clients send one.

So if P1 dies while build01 lists `builds/src/`, P2 resumes at the same entry.

### 7.3 Names

- A name is RFC 7's bytes ([RFC 7 §3.2](rfc-7-namespace-metadata.md#3.2%20A%20name%20is%20bytes%2C%20and%20it%20is%20validated%20at%20the%20boundary)).
  The NFS adapter passes it through unvalidated and unconverted, on every
  version; RFC 7 refuses what it refuses, mapped to `NFS3ERR_INVAL` /
  `NFS4ERR_INVAL` or `NFS4ERR_BADNAME`.
- NFSv4's character-set capability (`fs_charset_cap`) sets
  `FSCHARSET_CAP4_CONTAINS_NON_UTF8` and clears
  `FSCHARSET_CAP4_ALLOWS_ONLY_UTF8`, since NFSv3 clients can create names that
  are not UTF-8. How SMB shows such names is [RFC 22 §12.3](rfc-22-smb.md#12.3%20Names%20SMB%20cannot%20carry)'s.

### 7.4 Pseudo-filesystem, SECINFO and locations

The pseudo-filesystem, its handles, `fsid` and change attribute, and the
`SECINFO` / `SECINFO_NO_NAME` answers are
[RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)'s. The adapter
encodes them; it adds no export of its own.

`fs_locations` — the attribute telling a client where else a filesystem can be
reached — is returned only for a drain
([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)):

- a draining `protocol` node answers operations on a share with
  `NFS4ERR_MOVED`, and `fs_locations` names another `protocol` node of the same
  installation and the same path;
- every other time the attribute lists no location;
- a referral to another installation is never returned
  ([RFC 17 §1.1](rfc-17-vfs.md#1.1%20Non-goals)), and `fs_locations_info` is not
  supported.

## 8. Attributes

NFSv4 attributes are a numbered list: some required, some recommended; the
client asks which are supported (`supported_attrs`). Each one served maps to a
field of RFC 7's File, the share's capabilities, or a fixed value.

### 8.1 Supported

Every REQUIRED attribute of RFC 8881 is supported. The recommended and NFSv4.2
attributes, and the field each is read from, are listed in
[Appendix B](#Appendix%20B%20%E2%80%94%20attributes). The ones that carry a
decision: `change` is the file's version including writes not yet committed
(§9.2); `change_attr_type` is `NFS4_CHANGE_TYPE_IS_UNDEFINED` (§14);
`maxread`, `maxwrite` and `maxfilesize` are §2.3's.

**Audit entries are an administrator's.** The ACL's audit and alarm entries
say which accesses are recorded; reading them tells a user what is watched, and
changing them turns auditing off. Both protocols gate them alike
([RFC 22 §11](rfc-22-smb.md#11.%20Security%20descriptors)):

- `GETATTR` or `SETATTR` of `sacl` by a principal that is not an administrator
  of the share (RFC 18, planned) **MUST** be `NFS4ERR_ACCESS`.
- The NFSv4.0 `acl` attribute can carry audit and alarm entries too. A `GETATTR`
  of `acl` by a non-administrator **MUST** omit them; a `SETATTR` of `acl` by a
  non-administrator that contains any is `NFS4ERR_ACCESS`, and one that
  contains none leaves the stored audit entries as they were.
- `mode`, `owner` and `dacl` are unaffected: the owner still controls them.

**`time_metadata` always moves.** The file's ctime moves on every change,
whatever an SMB client has suspended on its own open
([RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward),
[RFC 22 §9.3](rfc-22-smb.md#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files)):
NFSv3 clients revalidate their caches by ctime, and one that froze would serve
stale data after an SMB write.

### 8.2 Not supported

Labeled NFS (`sec_label`, RFC 7204), named attributes (`OPENATTR`,
`named_attr` false) and a set of attributes no record holds are absent;
[Appendix B](#Appendix%20B%20%E2%80%94%20attributes) lists them with the answer
each gets and why.

> decision: no labeled NFS. Labeled NFS carries a mandatory-access-control (MAC)
> label, such as an SELinux context, on every file. Storing one is cheap: it
> would be an xattr in the security namespace
> ([RFC 7 §2.7](rfc-7-namespace-metadata.md#2.7%20Extended%20attributes%20and%20named%20streams))
> with no other record. But serving it means taking part in a client's MAC
> policy, which is a security claim this server makes nowhere else. Revisit for
> a customer running SELinux-labeled home directories, storing the label as an
> xattr.

## 9. Data

Reads and writes pass to the filesystem service almost unchanged. The choices
here: what "stable" means for a write, what a client needs to see another
client's data, and which NFSv4.2 operations are served.

### 9.1 Write stability

An NFS write says how durable it must be before the reply: **unstable**
(`UNSTABLE4` / `UNSTABLE`, the client will send a `COMMIT` later), or stable —
data and what is needed to read it back (`DATA_SYNC`), or data and all
metadata (`FILE_SYNC`).

- An unstable write is `Write` with unstable stability, answered from the
  journal ([RFC 0 §5.1](rfc-0-data-lifecycle.md#5.1%20Write)); `COMMIT` is `Commit`.
- `DATA_SYNC` and `FILE_SYNC` are both a stable write, and the reply says
  `FILE_SYNC`. A stable write **MUST** be answered once the journal has synced
  the write's record, which carries the data and the modification time the
  write sets ([RFC 1 §4.3](rfc-1-journal.md#4.3%20Records)), and **MUST NOT**
  wait for the metadata store to commit them: the synced record alone makes the
  write, its size and its `mtime` recoverable after a crash, and the existence
  commit follows lazily
  ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)).
  So there is no cheaper level to offer, and a stable write costs one journal
  sync, not a metadata transaction as well.
- The verifier in every `WRITE` and `COMMIT` reply — which changes when the
  server may have lost unstable writes, telling the client to resend them — is
  RFC 17's ([RFC 17 §5.8](rfc-17-vfs.md#5.8%20The%20write%20verifier)), passed
  through.
- A write refused because the journal is full is answered "retry"
  (`NFS4ERR_DELAY` / `NFS3ERR_JUKEBOX`) while offload or repack can still free
  space, and "no space" only once nothing will without an operator (§5.6).

### 9.2 Close-to-open, and what the server guarantees

NFS clients cache file data and promise only **close-to-open** consistency: at
close they flush and read the attributes; at open they read the attributes
again, and drop their cache if the `change` attribute moved. The server
guarantees what that needs:

- **`change` moves on every write the primary accepts**, not only when the
  write is committed. The overlay's `Version` ([RFC 17 §5.7](rfc-17-vfs.md#5.7%20GetAttr))
  **MUST** advance per staged write, so two `GETATTR`s with a write between them
  never return the same value, whichever node answers.
- **`change` never moves backward.** It is `Version`, drawn from the version
  counter of the journal at the file's primary, which only rises and which a
  journal raises above the share's version floor before it serves a share it
  did not serve when it opened; the existence commit never leaves the stored
  value below one the overlay reported
  ([RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)).
  Nothing reports a value derived from the clock alone.
- **`GETATTR` is answered at the primary** when the engine holds uncommitted
  writes, so a `protocol` node never answers from a committed record that lags
  them.
- A `GETATTR` after a client's `CLOSE` or `COMMIT` returns a `size` and `change`
  covering every write that client was answered for.
- `change` never moves for an operation that changed nothing
  ([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps)): an upload to
  the object store, an eviction or a fill does not make every client drop its
  cache.
- **A directory's change info is never atomic.** The `change_info4` that
  `CREATE`, `REMOVE`, `RENAME`, `LINK` and a creating `OPEN` return carries the
  directory's `Version` read just before and just after the transaction, with
  `atomic` false, so the client revalidates the directory instead of patching
  its cache ([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps)).
- **NFSv3 pre-operation attributes are omitted unless captured atomically.** In
  every `wcc_data`, `before` is absent for a directory, always, and for a file
  unless the primary captured it under the file's commit serialisation in the
  same step as the operation
  ([RFC 17 §5.7](rfc-17-vfs.md#5.7%20GetAttr)); the client then revalidates.
  A `before` that another change fell behind would let the client patch a
  stale cache.

Example: build01 writes `builds/out/app` through P1 and closes it; another
client opens it through P2 before the writes are committed. P2 asks S1, whose
`change` has moved, so that client drops its cache and reads the new binary.

### 9.3 NFSv4.2 operations

NFSv4.2 adds space management (`ALLOCATE`, `DEALLOCATE`), hole finding (`SEEK`,
`READ_PLUS`), server-side copy and clone (`COPY`, `CLONE`) and extended
attributes (RFC 8276 `GETXATTR` and friends, the user namespace only, reaching
the same records SMB extended attributes reach). Each maps to a service call or
to RFC 14's copy state; the per-operation table is [Appendix C](#Appendix%20C%20%E2%80%94%20NFSv4.2%20operations).
Not served, answered `NFS4ERR_NOTSUPP`: `ALLOCATE`, copy between installations
(`COPY_NOTIFY`, inter-server `COPY`, since there is one installation only —
[RFC 17 §1.1](rfc-17-vfs.md#1.1%20Non-goals)), `IO_ADVISE`, `WRITE_SAME`,
`LAYOUTERROR` and `LAYOUTSTATS`.

> decision: `ALLOCATE` is not served. It promises that later writes to the
> range will not fail for space, and nothing below this adapter reserves
> space: the service offers no `Allocate`
> ([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), and the journal admits
> writes against its own capacity and the share's limit as they arrive
> ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)),
> so a reservation would be a promise no layer keeps. Answering success without one is worse than
> refusing: an application that preallocated would still meet `NFS4ERR_NOSPC`
> mid-write. Clients fall back on `NFS4ERR_NOTSUPP` (an `fallocate` that only
> reserves fails with "not supported"; one that writes zeros writes them).
> Serve it when a lower RFC defines a per-file reservation charged against the
> share's limit.

### 9.4 Async COPY

A long server-side copy may answer at once and report completion later.

- A `COPY` whose length is at most 64 MiB, or which shares content without
  moving bytes, runs synchronously.
- A longer one with `ca_synchronous` false returns a copy stateid at once and
  runs at the destination file's primary, which holds the copy state
  ([RFC 14 §2.7](rfc-14-open-state.md#2.7%20Copy)).
- Completion is a `CB_OFFLOAD` through the client's current callback
  registration (§3.4). A `CB_OFFLOAD` that cannot be delivered is dropped, and
  the client learns the result by asking (`OFFLOAD_STATUS`), which any node
  answers from the copy state.
- A copy whose primary fails over is lost with the volatile table, and is
  reported unknown, never done: `OFFLOAD_STATUS` and `OFFLOAD_CANCEL` meet
  `ErrNoCopy`, answered `NFS4ERR_BAD_STATEID`, and the client runs the copy
  again or falls back to reading and writing.

## 10. RPCSEC_GSS

`RPCSEC_GSS` is how NFS carries Kerberos: client and server set up a GSS
(Generic Security Services) **context** — a handle, a session key and a window
of accepted sequence numbers — and every later call is checked against it.
Authentication and principal mapping are RFC 18's (planned). On the wire:

- a GSS context **MUST** be held in the memory of the `protocol` node that
  created it, and is not shared or stored. After a `protocol` node loss, the
  client's next call on the old handle is answered `RPCSEC_GSS_CREDPROBLEM`,
  and it establishes a new context;
- the sequence window is 128;
- `krb5` (authentication), `krb5i` (plus integrity) and `krb5p` (plus privacy,
  i.e. encryption) are served; the share's minimum level is
  [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)'s
  admission, answered `NFS4ERR_WRONGSEC` or `AUTH_TOOWEAK`.

## 11. Invariants

| # | Invariant |
| --- | --- |
| N1 | No NFS program is served over UDP. |
| N2 | Every `protocol` node returns the same `server_scope` and `so_major_id`, and its own `so_minor_id`. |
| N3 | Sessions, reply caches and GSS contexts live on one `protocol` node and are never stored; a session is never bound to connections on two nodes. |
| N4 | A stateid's `other` names an RFC 14 record, resolvable on any node; its `seqid` is held with the state at the primary. |
| N5 | An expired client's state is released at expiry; nothing is kept as a courtesy. |
| N6 | A client's latest back-channel registration receives every later callback. |
| N7 | `change` is `Version`: it moves on every write the primary accepts, never on an operation that changed nothing, and never backward. |
| N8 | A READDIR cookie is RFC 7's 63-bit digest of the entry, valid on every node; a resume returns the entries after it; no reply ends inside a collision chain; the cookie verifier is a constant of the share. |
| N9 | No referral or `fs_locations` names a server outside the installation. |
| N10 | Every NFS object the adapter maps names a record another RFC owns; the adapter keeps no durable state. |
| N11 | A replayed request is answered from a whole cached reply or `NFS4ERR_RETRY_UNCACHED_REP`, never run twice on one session and never from part of a reply; a session caches at most 64 × 8 KiB. |
| N12 | A stateid's `other` holds at least 64 random bits; NFSv4.0 I/O under a stateid is served only to the open's principal. |
| N13 | A `RECLAIM_COMPLETE` closes reclaim for one (client, shard, grace instance); every failover or restart opens a new instance and is signalled by `SEQ4_STATUS_RESTART_RECLAIM_NEEDED`. |
| N14 | A lock request from another client recalls a read or write delegation before the lock is decided; a delegation is never a handle grant. |
| N15 | An NLM host is its name and source address; NLM callbacks leave from the floating address the host used. |
| N16 | Audit entries are read or changed only by an administrator; the ctime NFS reports moves on every change. |
| N17 | A stable write is answered after the journal sync, never after a metadata transaction; a full journal answers "retry" while offload or repack can free space, never "no space" for a deadline. |
| N18 | The owner string, verifier, creating principal, state protection, back-channel node and session holders of a client are in its durable record; an NFSv4.0 owner's sequence and confirmation are in its `OwnerSeq`. |
| N19 | A directory's `change_info4` is never reported atomic, and NFSv3 pre-operation attributes are returned only when captured with the operation, never for a directory. |

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
| §4.2 v4.0 owners | pynfs `nfs4.0` open-owner and lock replay tests; then move the client to another node mid-sequence and assert the next sequenced `OPEN` is accepted. Open with a new owner and, before `OPEN_CONFIRM`, send `READ` and `CLOSE` under it through another node; assert both refused and `OPEN_CONFIRM` then accepted. `SETCLIENTID` on P1, `SETCLIENTID_CONFIRM` on P2: assert confirmed. | seqids or confirmation held in the `protocol` node |
| §5.1 no courtesy | Let a client's lease expire with no conflict; assert its lock is gone and a new client takes it. | |
| §5.2 stateids | pynfs `TEST_STATEID`, `FREE_STATEID`, bad- and old-stateid tests; present another client's stateid and assert `NFS4ERR_BAD_STATEID`. Open, upgrade the open, then `READ` with the first stateid's `seqid` through another node; assert `NFS4ERR_OLD_STATEID`. | |
| §5.2 v4.0 stateids | Over NFSv4.0, `LOCK` with an open stateid of another client's open owner; assert `NFS4ERR_BAD_STATEID`. Under Kerberos, open a file as alice, then send `READ`, `WRITE` and `SETATTR` quoting her open stateid with mallory's credential; assert `NFS4ERR_ACCESS` for each and the file unchanged. | v4.0 I/O authorised by the stateid alone |
| §5.2 unguessable `other` | Open 10^4 files from one client; assert no two `other` values share their random bits and that no field of them increases with open order. | `other` built from a counter |
| §3.1 cached reply size | `CREATE_SESSION` asking for a 1 MiB cached reply; assert 8 KiB granted. Send a `REMOVE` with `sa_cachethis` false, drop the reply, resend in the same slot and sequence; assert `NFS4ERR_RETRY_UNCACHED_REP` and the name removed once. | a partial reply replayed as complete, or an unbounded cache |
| §3.3 client record | Two nodes: `EXCHANGE_ID` on P1, then the same owner string with a new verifier on P2, then from another principal on P2; assert P2 finds the record, treats the first as a reboot and refuses the second `NFS4ERR_CLID_INUSE`. Hold a session on P1 and send `DESTROY_CLIENTID` to P2; assert `NFS4ERR_CLIENTID_BUSY`. | client state kept per node |
| §5.3 no v4.0 delegations | An NFSv4.0 client with a reachable callback address opens a file no one else uses, read-only and then for write; assert `OPEN_DELEGATE_NONE` both times and no connection to `r_addr`. | delegations offered over the v4.0 callback path |
| §5.4 grace | pynfs reboot/reclaim tests against a shard failover, not a whole-server restart. | grace tested only by restarting everything |
| §5.4 reclaim per grace | A client mounts (sending `RECLAIM_COMPLETE`), opens files in two shards, then one shard fails over while its session survives. Assert `SEQ4_STATUS_RESTART_RECLAIM_NEEDED` on the next `SEQUENCE`, its reclaim in the failed shard granted, its reclaim in the healthy shard answered with the state it holds, and a second failover reclaimed the same way. | `RECLAIM_COMPLETE` counted once per client, or a revocation flag as the signal |
| §5.6 full journal | Fill the journal with offload stalled and the remote reachable; assert a write is answered `NFS4ERR_DELAY` (v4) and `NFS3ERR_JUKEBOX` (v3) for longer than the caller's deadline, never `NFS4ERR_NOSPC`, and succeeds on retry once offload resumes. Make the store deny puts; assert `NFS4ERR_NOSPC`. Fill a share's limit instead; assert `NFS4ERR_NOSPC` at once. | a full journal answered "no space" when its deadline runs out, or a quota answered "retry" |
| §5.3 lock recall | Client A holds a write delegation; client B sends an NLM lock and, separately, an NFSv4 `LOCK` over a range A locked locally. Assert `CB_RECALL` before B's answer, A's lock sent to the server, and B refused. Repeat with A holding a read delegation and no local lock; assert `CB_RECALL` before B is granted. | a lock request that does not recall, or recalls write delegations only |
| §6 NLM | cthon04 lock tests over NFSv3; an NLM lock against an SMB byte-range lock and an SMB deny mode against `NLM4_SHARE`. Fail a shard over and assert `SM_NOTIFY` reaches the client's statd and it reclaims. | NLM state held in the adapter |
| §6.1 host binding | Host A locks a range. From another address, send `NLM4_FREE_ALL` and `NLM4_UNLOCK` naming A's `caller_name`; assert A's lock still held. Block a lock of A's, release it, and capture `NLM4_GRANTED`: assert its source is the floating address A used. | a host identified by its name alone, or callbacks from a node address |
| §5.5, §6.1 conflict holder | Hold a range by NLM, then by an SMB byte-range lock; from an NFSv4 client send `LOCK` and `LOCKT` over it. Decode the reply bytes: `NFS4ERR_DENIED`, client ID 0, owner length 0, for all four. Then `NLM4_TEST` against an NFSv4 lock: `svid` 0, `oh` length 0. | an internal owner identity encoded as the holder |
| §7.2 cookies | List a 10^5-entry directory from one node, resume each page on another node, while a third client creates and removes. Assert every untouched entry exactly once, and the same cookie verifier on every page. With the share's hash key fixed so two names collide, list with a `maxcount` that ends a page between them; assert the page ends before the pair and the next holds both. | positional or node-local cookies, a verifier that changes on mutation, or a page ending inside a chain |
| §8 attributes | pynfs attribute tests (`supported_attrs`, `GETATTR`, `SETATTR`); assert `sec_label` and `named_attr` absent. | |
| §8.1 audit entries | As a file's owner who is not an administrator: `GETATTR` and `SETATTR` of `sacl` are `NFS4ERR_ACCESS`; `GETATTR` of `acl` shows no audit entry; `SETATTR` of `acl` without one keeps the stored audit entries. As an administrator, all succeed. | the SACL readable or writable by the owner |
| §8.1 ctime | An SMB client sets −1 on all four times of an open and writes through it; assert an NFSv3 `GETATTR` shows `ctime` moved. | SMB suspension freezing the NFS ctime |
| §9.1 stability | Write `DATA_SYNC`, crash the primary, read back. Assert the data and its `size` and `mtime`. Count metadata store commits during 10^4 `FILE_SYNC` writes; assert the replies did not wait on them. | a stable write answered only after a metadata transaction |
| §9.2 close-to-open | Two clients on two nodes: one writes and closes, the other opens and reads. Assert the new data, for writes staged and not yet committed. Two writes with no commit between them change `change` twice. Write, `GETATTR`, fail the shard over to another node, `GETATTR` again; assert `change` not lower. | `change` from the committed record only, or from a counter that restarts with the primary |
| §9.2 change info | Run 64 parallel creates in one directory over NFSv4 and NFSv3; assert every `change_info4` has `atomic` false and no NFSv3 `wcc_data` for the directory carries `before`. | a change-info pair reported exact that another create fell between |
| §9.3 NFSv4.2 | xfstests over NFSv4.2 (`SEEK_HOLE`/`SEEK_DATA`, `copy_file_range`, `FICLONE`, punch-hole, xattrs); `READ_PLUS` over a file with holes reads zeros where holes are. `ALLOCATE` is `NFS4ERR_NOTSUPP`. | an `ALLOCATE` answered success with nothing reserved |
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
   clients skip invalidations. [RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)
   keeps `change` from moving backward across restarts, moves and failovers, but
   its version floor covers only versions a File records: a journal lost with
   writes whose versions only its overlay had reported can let a later write
   draw one of those versions again. Until that case is closed, `UNDEFINED`.
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

## Appendix B — attributes

*Normative: the attribute support of [§8](#8.%20Attributes).*

Every REQUIRED attribute of RFC 8881 is supported. The RECOMMENDED and NFSv4.2
attributes that are supported, and where each is read from:

| Attribute | Source |
| --- | --- |
| `change` | `File.Version` with the engine's overlay ([RFC 17 §5.7](rfc-17-vfs.md#5.7%20GetAttr)); §9.2 |
| `change_attr_type` | `NFS4_CHANGE_TYPE_IS_UNDEFINED` (§14) |
| `size`, `space_used` | `Size`; `Charged` ([RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem)) |
| `mode`, `owner`, `owner_group`, `mode_set_masked` | the File; owner strings by RFC 18 (planned) |
| `acl`, `dacl`, `aclsupport` | RFC 7's ACL, mapped by RFC 19 (planned) |
| `sacl` | the ACL's audit entries ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)); administrators only (§8.1) |
| `archive`, `hidden`, `system` | `Flags` |
| `time_access`, `time_modify`, `time_metadata`, `time_create`, `*_set` | `Access`, `Modify`, `Change`, `Birth` |
| `time_delta` | `Capabilities.TimeGranularity` |
| `fileid`, `mounted_on_fileid`, `numlinks`, `rawdev` | RFC 7; pseudo-filesystem per [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees) |
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

Not supported:

| Feature | Answer | Why |
| --- | --- | --- |
| Labeled NFS (`sec_label`, RFC 7204) | not in `supported_attrs` | no mandatory-access-control policy exists to label for |
| Named attributes (`OPENATTR`, `named_attr` false) | `NFS4ERR_NOTSUPP` | named streams exist for SMB; NFS clients use RFC 8276 xattrs, and no common client uses `OPENATTR` |
| `retention_*`, `retentevt_*`, `mimetype`, `time_backup`, `dir_notif_delay`, `dirent_notif_delay`, `fs_status`, `fs_locations_info`, `mdsthreshold`, `space_freed` | not in `supported_attrs` | no record holds them; `space_freed` cannot be computed cheaply where content is shared |

## Appendix C — NFSv4.2 operations

*Normative: the operation mapping of [§9.3](#9.3%20NFSv4.2%20operations).*

| Operation | Maps to | Notes |
| --- | --- | --- |
| `ALLOCATE` | `NFS4ERR_NOTSUPP` | no reservation exists below the adapter (§9.3) |
| `DEALLOCATE` | `Deallocate` | holes are RFC 6's |
| `SEEK` | `Seek` | `NFS4_CONTENT_DATA` / `NFS4_CONTENT_HOLE`, from the engine's allocation answer ([RFC 17 §5.2](rfc-17-vfs.md#5.2%20Read)); a zero ref under a newer overwrite record is data |
| `READ_PLUS` | `Read` and `Seek` | holes returned as `NFS4_CONTENT_HOLE` segments only where RFC 6 records one, never derived from reads; zeros written as data stay data, and so does a zero ref under a newer overwrite record |
| `COPY` (intra-server) | `Copy` | sync or async, §9.4 |
| `OFFLOAD_STATUS`, `OFFLOAD_CANCEL`, `CB_OFFLOAD` | async copy state ([RFC 14 §2.7](rfc-14-open-state.md#2.7%20Copy)) | |
| `CLONE` | `Copy` | content shared, never re-staged; atomic as RFC 7862 §15.13 asks |
| `GETXATTR`, `SETXATTR`, `LISTXATTRS`, `REMOVEXATTR` (RFC 8276) | `Xattrs`, `SetXattr`, `RemoveXattr` | the user namespace only; the same records SMB extended attributes reach |
| `COPY_NOTIFY`, inter-server `COPY` | `NFS4ERR_NOTSUPP` | one installation only ([RFC 17 §1.1](rfc-17-vfs.md#1.1%20Non-goals)) |
| `IO_ADVISE`, `WRITE_SAME`, `LAYOUTERROR`, `LAYOUTSTATS` | `NFS4ERR_NOTSUPP` | |
