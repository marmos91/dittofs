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
  - "[[rfc-12-snapshots]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-18-identity]]"
  - "[[rfc-19-authorization]]"
  - "[[rfc-20-adapters]]"
  - "[[rfc-22-smb]]"
  - "[[rfc-24-resources]]"
  - "[[rfc-25-observability]]"
  - "[[rfc-27-namespace-migration]]"
  - "[[rfc-28-journal-format]]"
aliases:
  - RFC 21
  - NFS
tags:
  - rfc
---
# RFC 21 — NFS

**Status:** draft. [§15](#15.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone changing the NFS adapter — its versions, sessions, state on
the wire, attributes or locking side protocols — and anyone deciding what an NFS
client sees when a node goes away.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

NFS (Network File System) is the protocol Linux and Unix machines use to mount
a remote directory as if it were a local disk. The client sends remote procedure
calls over TCP — look up a name, open, read, write, lock, close — and the server
answers each one. It is like ordering by phone from a warehouse: the caller asks
one thing at a time, and the clerk on the line only takes the order and passes
it to the stock room.

This RFC is the NFS adapter, that clerk: the part of DittoFS that speaks those
calls and translates each into a call on the filesystem service
([RFC 17](rfc-17-vfs.md)). It makes every choice the NFS specifications leave
to the server — versions, session sizes, lease time, what survives a node
failure — and says which record elsewhere each NFS object stands for. It owns
no durable record of its own — only the opaque `Adapter` bytes it keeps in
RFC 14's client records, through the service (§11.4): names, file handles and attributes are
[RFC 7](rfc-7-namespace-metadata.md)'s; client records, opens, locks and
delegations are [RFC 14](rfc-14-open-state.md)'s; which shares exist, who may
mount them and the NFSv4 pseudo-filesystem (the read-only tree of directories
that leads a client to each share) are [RFC 17](rfc-17-vfs.md)'s. Behind the
service, writes land in a local journal and move to an object store in the
background ([RFC 0](rfc-0-data-lifecycle.md)); NFS sees none of that.

**The problem, with one example.** How a client talks to the server, and what
goes wrong. Take a cluster of nodes **P1**, **P2** and **S1**. Every node
holds client connections and is the primary of some shards; **S1** is the
primary for `builds`, the one node that orders every change to its files. The Linux build server **build01**
mounts `builds` over NFSv4.1 and untars a source tree of 40,000 small files.

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
grace period of at least 90 s, during which no new open or lock is granted. If
the reply cache were stored and replicated, the retry would be exact, at the
cost of one replicated write per create and rename — about 40,000 extra for
this untar — to cover an event whose visible result matches an NFSv3 server
reboot, which clients already tolerate. This RFC takes neither: state at the
primary, reply cache in memory.

![build01 mounts builds through a floating address held by P1, its gateway node, whose memory holds session 1 with up to 64 slots and its reply cache. P1 dies; the address moves to P2, which answers NFS4ERR_BADSESSION, and build01 opens session 2 under the same client ID with an empty reply cache. Both nodes forward every call to S1, the primary of builds, which holds RFC 14's client record, opens, locks, delegations and stateids and RFC 7's names, file handles, attributes and directory cookies, untouched by the loss, so no grace runs. What is lost is exactly-once for the three requests in flight](img/rfc21-node-loss.svg)

**The words you need.**

- **gateway node / primary** — the node a client's call arrives at, which
  forwards it to the primary when it is not the primary itself
  ([RFC 17 §2](rfc-17-vfs.md#2.%20Gateway%20and%20server)) / the one node that
  orders a shard's writes ([RFC 0 glossary](rfc-0-data-lifecycle.md#Glossary)).
- **File handle** — an opaque name for a file, the same on every node and after
  restart ([§7.1](#7.1%20File%20handles),
  [RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)).
- **Session, slot, reply cache** — NFSv4.1's way to run each request exactly
  once: a resend in the same slot gets the stored reply
  ([§3.1](#3.1%20Slot%20tables), [§3.2](#3.2%20The%20reply%20cache%20is%20per%20session%2C%20in%20memory)).
- **Client ID and lease** — who the client is, and how long its state is kept
  while it is silent: 90 s ([§5.1](#5.1%20Client%20ID%2C%20lease%20time%2C%20expiry),
  [RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)).
- **Stateid** — the token naming an open, a lock or a delegation on the wire
  ([§5.2](#5.2%20Stateids)).
- **Delegation** — a promise that no other client is using a file, so the client
  may cache it; recalled on conflict ([§5.3](#5.3%20Delegations),
  [RFC 14 §5.1](rfc-14-open-state.md#5.1%20What%20a%20grant%20is)).
- **Grace period** — a window after a primary loses state in which only
  reclaims of held state are accepted ([§5.4](#5.4%20Grace%20on%20the%20wire),
  [RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)).
- **NLM, NSM** — NFSv3's side protocols: the Network Lock Manager takes locks,
  and the Network Status Monitor tells peers of a restart
  ([§6](#6.%20NFSv3%20locking%3A%20NLM%20and%20NSM)).

**What this RFC promises.**

- Losing a client's gateway node loses none of the opens, locks or delegations
  held at other primaries and starts no grace period there; the client opens a
  new session and carries on. The shards the lost node was primary of fail
  over with their own grace (§5.4).
- It does lose exactly-once for the requests in flight: a resent create, remove
  or rename may answer "exists" or "not found" for work that was done.
- A file handle, a stateid and a directory listing position are valid on every
  node, so a client moved between nodes keeps using them.
- A file's `change` attribute moves on every write, and never on an operation
  that changed nothing, so when one client closes a file and another opens it
  through a different node, the second sees the new data.
- A client silent for 90 s loses all its state at once; nothing is kept for it.
  Every NFS program is served over TCP only.

### Scenarios

**build01 is paused for two minutes while holding a lock on `builds/.lock`.**

1. Its lease is 90 s, renewed by each request; for two minutes it sends none
   ([§5.1](#5.1%20Client%20ID%2C%20lease%20time%2C%20expiry)).
2. The lease expires, and every open, lock and delegation it held is released
   at once, whether or not another client wants them: there are no courtesy
   clients ([§5.1](#5.1%20Client%20ID%2C%20lease%20time%2C%20expiry)).
3. When it resumes, its next request is answered `NFS4ERR_BADSESSION` for its
   session, or `NFS4ERR_BAD_STATEID` for a stateid, and it starts over.

Outcome: the lock is gone when build01 resumes, even though nobody else asked
for it.

**build01 holds a delegation on `builds/Makefile` from S1, and P1 dies.**

1. The delegation is held at S1, so it survives; the recall path does not. S1
   reaches build01 through the gateway node the client record names for its
   back channel, which was P1 ([§3.4](#3.4%20The%20back%20channel%20and%20where%20callbacks%20go)).
2. A second client's conflicting request makes S1 send `CB_RECALL`. Until
   build01 opens a session with a back channel on P2, the recall has nowhere to
   go and fails.
3. Once it does, the record names P2 and the recall arrives there. Had it never
   done so, the delegation would have been revoked at the recall's deadline.

Outcome: a callback follows the client to whichever node it last bound its back
channel on.

**build01 mounts `builds` over NFSv3 instead and takes an NLM lock; then S1 fails over.**

1. The NLM lock is an RFC 14 lock under an anonymous open, so it conflicts with
   NFSv4 and SMB locks like any other ([§6.1](#6.1%20NLM)).
2. The shard's new primary raises the installation's NSM state number, durably,
   before the shard's grace begins, and has the gateway node holding the
   floating address send `SM_NOTIFY` to build01 ([§6.2](#6.2%20NSM)).
3. build01's statd reclaims the lock in grace. NLM never says it has finished
   reclaiming, so the shard runs its whole grace period.
4. Had only P1 died, no lock would have been lost, and no `SM_NOTIFY` would be
   sent.

Outcome: an NFSv3 client is asked to reclaim only when state was actually lost.

**How the rest is organised.** Each numbered section opens with a plain
paragraph — what the mechanism is for and what was decided — before its rules.
§2 sets versions, transport and transfer sizes. §3 (sessions) and §5 (lease,
stateids, delegations, grace) carry the failure story above; read them first.
§4 covers NFSv4.0 clients, §6 NFSv3 locking (NLM, the Network Lock Manager, and
NSM, its status monitor) and MOUNT. §7 fits handles, directory cookies and names
on the wire; §8 summarises attributes and §9 data operations, which a first read
can skim. §10 covers Kerberos on the wire; §11 gathers every translation between NFS
and the core, with its tests and metrics; §12–§14 hold the invariants, the
conformance checks and the other metrics. Appendix B lists every attribute and
Appendix C every NFSv4.2 operation.

---

## In short

- NFSv3, NFSv4.0, NFSv4.1 and a subset of NFSv4.2 are served, over TCP only.
- The adapter only translates. Every record it maps — clients, opens, lock
  owners, delegations, layouts, NLM and NSM state, copy state — is
  [RFC 14](rfc-14-open-state.md)'s; every name, handle and attribute is
  [RFC 7](rfc-7-namespace-metadata.md)'s; shares, the pseudo-filesystem and
  `SECINFO` answers are [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)'s.
  It is built as [RFC 20](rfc-20-adapters.md) describes every adapter: its
  lifecycle, request pipeline, codecs and connection handling.
- NFSv4.1 sessions, their slot tables and reply caches live in the memory of
  the gateway node holding the connection. They are not durable: when that
  node dies, exactly-once execution is lost for the requests in flight, and a
  client that retries a non-idempotent operation on its new session may run it
  twice.
- Callbacks run from the file's primary to the gateway node holding the
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
- decide authentication, principals or the `owner@domain` mapping —
  [RFC 18](rfc-18-identity.md#4.5%20NFSv4%20owner%20strings); or ACL (access
  control list) semantics — [RFC 19](rfc-19-authorization.md#3.%20Evaluation);
  their NFSv4 spelling is this RFC's (§11.3);
- restate the XDR (External Data Representation, the wire encoding) of RFC 1813,
  RFC 7530, RFC 8881 or RFC 7862, or the rows of the error table that follow
  from those specifications, which the adapter holds and
  [RFC 20 §4.5](rfc-20-adapters.md#4.5%20One%20error%20table%20per%20protocol) keeps total over
  [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)'s errors,
  beyond the open-state and capacity errors of §11.5, whose retry meaning is a
  protocol choice.

## 2. Versions, transport and sizes

Before any file is touched, client and server agree on a protocol version, a
transport (TCP or UDP) and the largest read or write; the server chooses what to
offer. This one offers NFSv3, NFSv4.0, NFSv4.1 and part of NFSv4.2, over TCP
only, with one fixed 1 MiB transfer size.

### 2.1 Versions served

NFS is a family of RPC (remote procedure call) programs, each versioned on its
own: NFS; MOUNT, which hands an NFSv3 client a share's root handle; NLM and NSM,
NFSv3's lock and crash-notification side protocols (§6). NFSv4 needs no
auxiliary service ([RFC 20 §3.1](rfc-20-adapters.md#3.1%20The%20adapter%27s%20states)); it sends operations in batches (`COMPOUND`s), each naming its minor
version.

![The NFS program family as served: on the NFSv3 side NFS v3, MOUNT v3 for a share's root handle, NLM v4 for byte-range locks and NSM v1 for crash notification are served; on the NFSv4 side v4.0 and v4.1 are served, v4.2 as the subset of section 9.3, and a minor version of 3 or above is answered NFS4ERR_MINOR_VERS_MISMATCH with nothing run. NFS v2 gets PROG_MISMATCH naming 3 to 4, and no program is served over UDP. All over TCP with one fixed 1 MiB transfer size; the port mapper answers on UDP and TCP and SM_NOTIFY is the only UDP the server sends](img/rfc21-programs.svg)

| Program, version | Served | Notes |
| --- | --- | --- |
| NFS v2 | no | `PROG_MISMATCH` naming 3–4 |
| NFS v3 (RFC 1813), MOUNT v3, NLM v4, NSM v1 | yes | §6 |
| NFS v4.0 (RFC 7530) | yes | §4 |
| NFS v4.1 (RFC 8881) | yes | sessions §3; pNFS (parallel NFS, clients reading data servers directly) only in a cluster, per [RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS): a single node advertises no metadata server and answers every layout operation `NFS4ERR_NOTSUPP` (§3.3) |
| NFS v4.2 (RFC 7862, RFC 8276) | the subset of §9.3 | an operation outside it returns `NFS4ERR_NOTSUPP` |

- A `COMPOUND` whose minor version is above 2 **MUST** be answered
  `NFS4ERR_MINOR_VERS_MISMATCH` with no operation run.
- The minor version is the client's choice per `COMPOUND`; the server
  **MUST NOT** remember one per client, so a client that falls back from 4.2
  to 4.1 needs no new client ID.

> decision: NFSv4.0 is served although NFSv4.1 does everything it does, because
> some clients and appliances are pinned to it (`vers=4.0`). The cost: it is the
> version that recovers worst from a lost reply, since a retransmission past the
> dedup table's retry window loses the owner's state (§4.2). Withdraw it when
> no supported client needs it; nothing else in this document depends on it.

### 2.2 TCP only

- Every NFS program — NFS, MOUNT, NLM, NSM — **MUST** be served over TCP and
  **MUST NOT** be served over UDP, with one exception: NLM and NSM are served
  over UDP too while [RFC 20 §11](rfc-20-adapters.md#11.%20Open%20questions)'s
  UDP question stands, for parity with today, since some clients send lock
  requests only over UDP.
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
> are not offered either; see §15.

**NFS over TLS** (RFC 9289) is served. A client sends a `NULL` call with the
`AUTH_TLS` flavour; the server answers and upgrades the same TCP connection to
TLS before any other request. With `mode: opportunistic` a client that does not
probe is served in plaintext; with `mode: require` any call before the upgrade
but the probe is refused and the connection closed. The settings are
`adapters.nfs.tls`
([RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)); where
the upgrade sits in the adapter, and why a client certificate authenticates the
host and not the caller, is
[RFC 20 §5.5](rfc-20-adapters.md#5.5%20Message%20transforms)'s.

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
> holds one buffer of that size ([RFC 24 §3.5](rfc-24-resources.md#3.5%20What%20a%20transfer%20size%20costs)), so a larger size costs
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
memory of the gateway node holding the connection, accepting that a node loss
turns resends of in-flight requests into second runs.

### 3.1 Slot tables

A session has two directions: the **fore channel**, for client requests, and the
**back channel**, for the server's calls to the client (§3.4). The slot count of
a channel is how many requests may be in flight on it at once.

- The fore channel offers at most **64 slots**, the back channel at most **16**.
  The call that opens a session (`CREATE_SESSION`) grants the lesser of what the
  client asks and these, and fewer where the node cannot charge the session's
  reply cache: the slots it can charge, at least one; where it cannot charge
  one, `CREATE_SESSION` is answered `NFS4ERR_DELAY`
  ([RFC 24 §4.5](rfc-24-resources.md#4.5%20Sessions%20and%20reply%20caches)).
- Under memory pressure the server **MAY** ask the client to use fewer slots (by
  lowering `sr_target_highest_slotid` in its replies; RFC 24 §4.5 says when),
  and **MUST** honour a client that then retires slots. It **MUST NOT** shrink the table
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
  in the memory of the gateway node that holds the session, and **MUST NOT**
  be written to the metadata store or replicated.

When that node dies, its sessions go with it. The client reconnects to a
surviving node through the floating address
([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)) and is answered
`NFS4ERR_BADSESSION`. It opens a new session (`CREATE_SESSION`) under its
existing client ID, which every node accepts because the client record is
durable ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)), and carries on. Its opens, locks and
delegations are untouched: they are held by the primaries, not by the
gateway node ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)), and
no grace runs but in the shards the lost node was primary of.

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
**not** help here. It recognises a request by the protocol's own retry
identity — for NFSv4.1 the gateway node's ID and the node epoch its
session was created under, with the request's session, slot and sequence ID
([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)) — and
build01's resend arrives through P2 on a new session, in a new slot, so it
names another. It catches retries on one session, not a resend on a new one.

> decision: the reply cache is not durable. Storing it would cost one
> replicated write per non-idempotent request — every create and rename, about
> 40,000 extra writes for build01's untar — to protect against one event, a
> gateway node dying with requests in flight, whose visible result is the
> same as an NFSv3 server reboot, which every client already tolerates. Revisit
> when users report client-visible errors after a gateway node loss. Keying
> the primary's dedup table by (client ID, session, slot,
> sequence ID) is no fix: an NFSv4.1 session lives in one node's memory and dies
> with it, so the resend arrives on a new session, in a new slot, and names
> neither. The request ID a forwarded call carries is built from that same
> retry identity, gateway node and the session's node epoch included ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)),
> so it too names the old session, and nothing in it lets a primary match a
> resend on the new one. The
> fix then is persistent sessions: the slot table and its cached replies stored
> with the client record, at the replicated write per request this decision
> declines.

### 3.3 Trunking, server owner and scope

**Trunking** is one client using several connections, even to several
addresses, at once. The client decides what it may combine from what each
address reports when it identifies itself (`EXCHANGE_ID`): a **server scope**
(`server_scope`) and a **server owner** whose major ID (`so_major_id`) says "one
client ID is valid across these" and minor ID (`so_minor_id`) "one session is".
All gateway nodes here are one server for client IDs and separate servers for
sessions, because a session's slots live in one node's memory (§3.2).

- Every gateway node of an installation **MUST** return the same
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
`EXCHGID4_FLAG_USE_NON_PNFS`.

- **A single node serves no pNFS.** It **MUST NOT** set
  `EXCHGID4_FLAG_USE_PNFS_MDS` or `EXCHGID4_FLAG_USE_PNFS_DS`, **MUST NOT** list
  `fs_layout_type` or any `layout_*` attribute in `supported_attrs`, and
  **MUST** answer `GETDEVICEINFO`, `GETDEVICELIST`, `LAYOUTGET`, `LAYOUTCOMMIT`
  and `LAYOUTRETURN` with `NFS4ERR_NOTSUPP`
  ([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS),
  [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)). A
  layout needs a data server apart from the metadata server's own I/O path,
  and one node has none; a client told it is a metadata server would ask for
  layouts no rule of the first release defines.
- **(cluster)** A node also sets `EXCHGID4_FLAG_USE_PNFS_MDS` where the
  deployment has a data server role, and data servers return
  `EXCHGID4_FLAG_USE_PNFS_DS`, under [RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS).

For state protection — whether only the machine that created a client ID may
act on it — `SP4_NONE` (no check) and `SP4_MACH_CRED` (the machine's
credential is checked) are accepted; `SP4_SSV` (a secret negotiated in-band) is
refused with `NFS4ERR_ENCR_ALG_UNSUPP`.

**What the client record carries for NFS.** Every node answers `EXCHANGE_ID`,
`SETCLIENTID` and `DESTROY_CLIENTID` for any client, so everything those calls
decide **MUST** be in RFC 14's durable client record
([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)), not in one node's memory.
§11.4 maps each field from the wire and lists what the adapter keeps in its
`Adapter` bytes.


### 3.4 The back channel and where callbacks go

Sometimes the server must call the client: to take back a delegation
(`CB_RECALL`) or a pNFS layout (`CB_LAYOUTRECALL`), to report a finished copy
(`CB_OFFLOAD`), or to ask for some delegations back when it holds too many
(`CB_RECALL_ANY`). These **callbacks** start at the primary of the file's shard,
but the client's back channel is held by a gateway node, so the primary goes
through the `Callbacks` the client registered ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). The
registration names the gateway node holding the client's back channel, and
the call travels primary → that node → the client's connection.

- The registration **MUST** be held in the durable client record, as the node
  that holds the client's back channel (`Reach.Callback`,
  [RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease)), so
  every primary routes a callback the same way whichever node it last heard the
  client through. A node's in-memory list is not enough: a primary on another
  node would never see a rebind (cluster; the
  [single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)
  has one node and one list).
- A client's registration **MUST** be replaced, not added to, when it binds a
  back channel on another node: the latest wins. After a gateway node loss,
  the client's new session with a back channel (`CREATE_SESSION` with
  `CREATE_SESSION4_FLAG_CONN_BACK_CHAN`) or `BIND_CONN_TO_SESSION` redirects every later callback,
  through the service's `Connect`, made again from the new node, which runs
  RFC 14's `Rebind` ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)) and writes the new node into
  the record's `Reach.Callback`, and clears `Reach.Down`, before the reply.
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

### 3.5 Confirming a client

A client ID is not state until the client confirms it. NFSv4.0 confirms with
`SETCLIENTID_CONFIRM`, NFSv4.1 with its first `CREATE_SESSION`. Both follow
[RFC 14 §2.9](rfc-14-open-state.md#2.9%20Clients%20pending%20confirmation); on
the wire:

- An `EXCHANGE_ID` or `SETCLIENTID` that names a new owner, or a known owner
  with a new verifier, **MUST** change no state: the answering node holds an
  `Pending` proposal in memory, and a rebooted client's old client ID keeps
  its opens and locks until the confirm. An `EXCHANGE_ID` that names a
  confirmed owner with that owner's verifier and principal returns the
  existing client ID at once (RFC 8881 §18.35.5). A `SETCLIENTID` that does so
  still leaves a `Pending` proposal, for the existing client ID under a new
  confirm verifier, and changes no state until its `SETCLIENTID_CONFIRM`,
  which keeps the client ID and its state and takes only the new callback
  (RFC 7530 §16.33.5). An NFSv4.0 client always confirms; with no record to
  confirm, its confirm would be `NFS4ERR_STALE_CLIENTID` every time it
  repeated `SETCLIENTID`, a loop after every restart.
- One whose owner a confirmed record holds under another principal **MUST** be
  `NFS4ERR_CLID_INUSE` (`ErrState` (`ClientInUse`)), whatever its verifier, and leaves no
  `Pending` proposal (RFC 8881 §18.35.5).
- The confirm is accepted only at the node that answered, from the same
  principal, naming the answered client ID and, for NFSv4.0, the confirm
  verifier, for NFSv4.1 the sequence ID, before one lease period has passed. It
  then writes the client record, expires the old client ID of a rebooted
  client, and drops the `Pending` proposal. A confirm that finds no such
  record — sent to another node, after that node restarted, or late — is
  `NFS4ERR_STALE_CLIENTID`, and the client sends `SETCLIENTID` or
  `EXCHANGE_ID` again.
- **One record kind for both minor versions.** NFSv4.0 and NFSv4.1 clients are
  one protocol kind ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)), and the
  minor version that confirmed a record is kept in its `Adapter` bytes. A
  `SETCLIENTID` for a name confirmed by `EXCHANGE_ID`, or the reverse, is a new
  `Boot`: a reboot, confirmed as any is, which expires the old client ID and its
  state, never the same instance (RFC 8881 §2.4.1). A client's state is never
  shared across minor versions. A confirm that finds the record but comes from another
  principal is `NFS4ERR_CLID_INUSE` (`ErrState` (`ClientInUse`); RFC 7530 §16.34.5,
  RFC 8881 §18.36.4) and leaves the record in place for its own principal.
- **`CREATE_SESSION` is sequenced by the adapter's record.** The NFS adapter
  keeps a one-slot replay in the client record's `Adapter` bytes (§3.3): the
  `csa_sequence` of the last `CREATE_SESSION` applied and its reply. It reads
  the record (`Record`) and decides, on whichever node the call reaches:
  - a `csa_sequence` **equal** to the stored one is a replay, answered with the
    stored reply, and writes nothing;
  - one equal to the stored one **+ 1** is new: the adapter builds the session
    and its reply, and calls `AddSession` with the bytes it read and the new
    sequence and reply, which records the session and writes the new bytes in
    one step ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)); for the
    confirming `CREATE_SESSION` the sequence compared is the `Pending`
    proposal's (§3.3);
  - if `AddSession` answers `ErrRecordChanged`, another node applied a
    `CREATE_SESSION` first: the adapter drops the session it built, rereads
    the record it was handed, and decides again, so a racing replay is
    answered with the winner's reply;
  - **any other** is `NFS4ERR_SEQ_MISORDERED` (RFC 8881 §18.36.4), the
    adapter's own answer: open state has no error for it.

  Without the stored reply, a replay that reaches another node after an
  address takeover, or arrives after a restart, creates a second session.

> decision: the confirm verifier is random and the unconfirmed proposal lives at
> one node, as RFC 14 §2.9 decides, rather than a digest any node recomputes. A
> digest would let a client confirm through another node, but it is computable
> by anyone who holds the installation secret and binds nothing the proposal's
> principal did not already prove; the cost of the volatile record is one
> repeated `SETCLIENTID` or `EXCHANGE_ID` when the address moves between the two
> calls, which clients already do on `NFS4ERR_STALE_CLIENTID`. Revisit with RFC
> 14 §2.9's own trigger: mounts seen failing because confirms land elsewhere.

## 4. NFSv4.0 clients

NFSv4.0 has no sessions. The client identifies itself with `SETCLIENTID`, which
also names an address where the server could call it back; and instead of slots, each
**open owner** and **lock owner** (an identity on the client holding opens or
locks) numbers its requests, and the server replays its last reply to a resend.
All of it maps onto the same records as NFSv4.1. The replay lives in a
gateway node's memory like the reply cache, and fares worse on a node loss.

### 4.1 SETCLIENTID and the callback path

- `SETCLIENTID` and `SETCLIENTID_CONFIRM` map onto the client record as
  `EXCHANGE_ID` and `CREATE_SESSION` do: the client's `nfs_client_id4` id is
  its `Name`, its verifier its `Boot`, which tells a reboot
  ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)), and the proposal is
  confirmed as §3.5 says: at the node that answered `SETCLIENTID`, under a
  random confirm verifier, and `NFS4ERR_STALE_CLIENTID` anywhere else.
- The callback address (`r_addr`) and program are kept in the adapter's own
  bytes, as
  [RFC 14 §2.9](rfc-14-open-state.md#2.9%20Clients%20pending%20confirmation)
  keeps them: in the `Pending` proposal's `Adapter`, and taken into the
  record's `Adapter` only by the confirm (§3.3), so a `SETCLIENTID` that only names a new callback for a
  confirmed client changes nothing until `SETCLIENTID_CONFIRM`. No callback
  connection is ever opened to it. The NFSv4.0 callback program carries only
  delegation calls (`CB_GETATTR`, `CB_RECALL`), and delegations are not offered
  to NFSv4.0 clients (§5.3), so the server never has a call to make. `RENEW`
  therefore never answers `NFS4ERR_CB_PATH_DOWN`.

### 4.2 Owner sequence IDs

NFSv4.0 numbers `OPEN`, `OPEN_CONFIRM`, `OPEN_DOWNGRADE` and `CLOSE` per open
owner. A `LOCK` that introduces a lock owner carries the open owner's seqid
beside the new lock owner's first one (`open_to_lock_owner4`), and is
sequenced and advanced against the open owner; every later `LOCK` of that lock
owner, and every `LOCKU`, is numbered per lock owner
([RFC 14 §2.8](rfc-14-open-state.md#2.8%20Owner%20sequences)). The
server replays the last reply to a retransmission.

- The **next expected sequence ID** of each owner **MUST** be held in RFC 14's
  owner sequence record (`Sequence.Next`, at the owner's home primary,
  [RFC 14 §2.8](rfc-14-open-state.md#2.8%20Owner%20sequences)), and
  advanced in the step that answers the operation, so every node agrees on it.
- **Every in-sequence reply advances it, refusals included.** As RFC 7530
  §9.1.7 requires, an in-sequence request answered `NFS4ERR_DELAY`,
  `NFS4ERR_GRACE`, `NFS4ERR_DENIED` or any other error advances `Next`, except
  `NFS4ERR_STALE_CLIENTID`, `NFS4ERR_STALE_STATEID`, `NFS4ERR_BAD_STATEID`,
  `NFS4ERR_BAD_SEQID`, `NFS4ERR_BADXDR`, `NFS4ERR_RESOURCE`,
  `NFS4ERR_NOFILEHANDLE` and `NFS4ERR_MOVED`. **(cluster)** When another
  shard's primary answers the request, the home primary commits the advance
  only once that answer is known and outside this list, and the answer is the
  reply kept for replay; a `NFS4ERR_STALE_STATEID` or `NFS4ERR_BAD_STATEID`
  from the file's primary advances nothing, as the client does not count it.
  Linux counts every other reply; a sequence left behind, or one run ahead,
  answers the owner's next request `NFS4ERR_BAD_SEQID`.
- Whether a new open owner has been confirmed is held in the same record: the
  first granted `OPEN` of an owner creates its `Sequence` with `Confirmed`
  false, a refused one creates nothing, and only `OPEN_CONFIRM` sets it
  ([RFC 14 §2.8](rfc-14-open-state.md#2.8%20Owner%20sequences)). Until then
  an `OPEN` from the owner replaces the unconfirmed open, releasing it, whatever
  seqid it carries; every other request naming its state, `READ` and `WRITE`
  included, is `NFS4ERR_BAD_STATEID` and advances nothing; and an
  `OPEN_CONFIRM` of an owner already confirmed is `NFS4ERR_BAD_STATEID`
  (RFC 7530 §16.18.5). `OPEN4_RESULT_CONFIRM` is never set on a
  `CLAIM_PREVIOUS` reclaim or on an `OPEN` granted a delegation
  (RFC 7530 §9.1.11).
- **A seqid wraps to 0.** An owner's next seqid after `0xFFFFFFFF` is 0, as
  clients in use send; a stateid's `seqid` wraps to 1, since 0 means
  current (RFC 14 §2.8's `decision:`).
- The **last reply** per owner is held only in the gateway node that sent
  it, as the session reply cache is (§3.2).

**(cluster)** After a gateway node loss, a retransmission of an owner's last
operation is still answered once. An NFSv4.0 request ID is the client's
address, XID, procedure and argument checksum, never the connection, so the
retransmission reaches the primary through the node that took the address over
under the same request ID, and the primary answers it from its dedup table,
the stored result of the first send
([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope), T10), without
advancing the owner's sequence again. Only a retransmission the table no longer
holds — sent past the retry window — finds its sequence ID used and no stored
reply; it is answered `NFS4ERR_BAD_SEQID`, and the client recovers that owner's
state.

> decision: NFSv4.0 owner replay past the dedup table's retry window is
> best-effort, for the reason of §3.2: the per-owner last reply is in one
> node's memory. It is worse than NFSv4.1's, because one owner's error costs the
> client every open under that owner, not one request. A retransmission within
> the window is answered from the primary's dedup table, on whichever node it
> arrives; withdraw this when §2.1 withdraws NFSv4.0.

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
  every NFSv4.0 `RENEW` or stateful operation, renews it, in the lease owner's
  memory and never by a write (§3.3).
- The client ID is minted from the client record and is valid on every node.
  It is unique and never reused, but not random: `clientid4` is 8 bytes, with no
  room beside what makes it unique, so it is honoured only together with the
  principal or session that holds it
  ([RFC 14 §2.11](rfc-14-open-state.md#2.11%20State%20references)).
- Ending a client (`DESTROY_CLIENTID`) maps to the service's `Destroy`
  ([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), RFC 14's `Destroy`, never
  to `Disconnect`, which is an SMB session's last channel ending; a refusal is
  `ErrState` (`ClientBusy`). It ends a client that holds
  nothing: it is refused with `NFS4ERR_CLIENTID_BUSY` while the client still
  holds a session on any node or any open, lock, delegation or layout in any
  shard, as RFC 8881 §18.50.3 requires, a revoked stateid not yet freed being
  none of these — the answering node reads that from the
  client record (§3.3), not from its own sessions, and never releases state to
  make the call succeed — and with `NFS4ERR_WRONG_CRED`,
  as Linux answers, when sent by a principal the client's `SP4_MACH_CRED` state protection
  excludes. `DESTROY_SESSION` drops only the
  node's session.

**No courtesy clients.** A client whose lease expires has every open, lock,
delegation and layout released at once, in every view
([RFC 14 §4.3](rfc-14-open-state.md#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)), whether or not another client wants them. Its next request is
answered `NFS4ERR_STALE_CLIENTID` (`ErrState` (`StaleClient`),
[RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) for an
operation naming its client ID, `NFS4ERR_BADSESSION` for a session, and
`NFS4ERR_BAD_STATEID` for a stateid, whose record was released with the rest
(§11.5); it starts over.

If build01 is paused for two minutes while holding a lock on `builds/.lock`,
the lock is gone when it resumes, even though nobody else asked for it.

**An administrator's revoke keeps the client.** It is not an expiry. Revoking
a client by administrator (`RevokedByAdmin`,
[RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)) ends its opens, locks,
delegations and layouts but keeps its client record and its sessions, as RFC
8881 §18.46.3 expects. Each revoked stateid enters the client's
revoked set
([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time))
and is answered by the cause of its revocation, as RFC 8881 §15.1 defines the
two errors: `NFS4ERR_ADMIN_REVOKED` for any state an administrator revoked —
an open, a lock state, a delegation or a layout — and `NFS4ERR_DELEG_REVOKED`
only for a delegation or layout revoked at its recall deadline (§5.3). Until
`FREE_STATEID` frees each one, every `SEQUENCE` reply carries
`SEQ4_STATUS_ADMIN_STATE_REVOKED` while any state an administrator revoked is
in the set, and `SEQ4_STATUS_RECALLABLE_STATE_REVOKED` while a grant revoked at
its recall deadline is; the adapter reads which from the causes `Renew`'s
`ClientStatus` reports, so any node answering the `SEQUENCE` sets the right
flag. The session stays usable, so the client frees what was revoked and opens
again under the same client ID. An NFSv4.0 client has neither the flag nor
`FREE_STATEID`: its stateid is answered `NFS4ERR_ADMIN_REVOKED` once, and the
adapter then calls `FreeState` for it on the client's behalf, so a later use is
`NFS4ERR_BAD_STATEID` and the client reopens.

**A revoke outlives a restart.** The revoked set is volatile, but the revoke's
transaction also empties the client's shard list durably
([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).
A later reclaim names a shard the record does not list and is refused
`ErrState` (`NoReclaim`) (§11.5) — the client held nothing there it may take
back — until the client next takes state there by an ordinary, non-reclaim
request, which lists the shard again; RFC 8881 §8.4.3 asks a server to refuse
the reclaim of state revoked before its restart. Without it, a node restarting
before a stuck client saw its revoke would let that client reclaim in grace the
lock another client took since. An NLM host has no revoked set and no flag to be
told by, so an administrator's revoke of an NLM host is an expiry
([RFC 14 §4.3](rfc-14-open-state.md#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)):
it releases the host's locks and deletes its record, so a later restart sends
it no `SM_NOTIFY` and it reclaims nothing. A reclaim it sends anyway names a
client no record holds — `ErrState` (`NoReclaim`) — and its next ordinary lock request
creates a new client record (§6.1).

> decision: an administrator's revoke follows RFC 8881 rather than purging
> the client like an expiry. A purge destroys the sessions, and a client
> whose session survived it, or that comes back with the same owner, meets
> `NFS4ERR_STALE_CLIENTID` on every reopen and loops. Keeping the record costs
> the revoked set until the client frees it, which a live client does at its
> next `SEQUENCE`, and an emptied shard list, refilled as it takes state again. Revisit if revoked sets are seen held by clients
> that never free them; the bound is then the lease, since such a client stops
> renewing.

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
`seqid`, a counter that moves each time that state changes. The codec is
`pkg/vfs`'s, shared by every adapter that names one
([RFC 17 §3.4](rfc-17-vfs.md#3.4%20IDs%20and%20their%20encodings)), under five rules:

1. **`other` names a record, not a table slot.** It **MUST** be a type tag and
   the RFC 14 identifier it stands for — open, a lock owner's lock state,
   delegation, layout or copy — so any gateway node resolves it without a
   table of its own, and a stateid survives a gateway node loss. Its
   non-random part carries the number of the shard that holds the record, and
   its random bits are unique within that shard, whose primary keeps an index
   from them to the record
   ([RFC 14 §2.11](rfc-14-open-state.md#2.11%20State%20references)): a stateid is resolved
   by shard, then index, with no file handle. The 12 bytes are laid out as:
   byte 0 the type tag (open, lock state, delegation, layout, copy) in its low
   3 bits and the low 5 bits of the shard's grace instance the stateid was
   minted under (§5.4) in its high 5 bits; bytes 1–3 the shard number, big-endian; bytes 4–11 the
   record's 64 random bits. Nothing else fits without giving up random bits.

   **A miss says whether state was lost.** The adapter passes the grace-instance
   bits with the lookup, and a stateid whose index entry is gone is answered
   as [RFC 14 §2.11](rfc-14-open-state.md#2.11%20State%20references) decides: `ErrState` (`StaleState`)
   (`NFS4ERR_STALE_STATEID`) when its bits name an earlier grace instance than
   the shard's current one, or for any miss while the shard is in grace;
   `ErrState` (`BadState`) (`NFS4ERR_BAD_STATEID`) otherwise — a stateid minted under the
   current grace instance outside grace, or one naming a shard number no shard
   holds. A stateid whose record never left its shard is judged by that
   shard's `GraceInstance`. **(cluster)** One a batch move carried from shard G
   to shard R keeps G's shard number and bits, and is judged where its record
   now lives: G's forwarding entry records R's grace instance at the batch's
   commit and carries it on the re-route, and R answers a miss stale while R
   is in grace or R's instance has moved past the recorded one, and bad
   otherwise; G deletes its forwarding entry on a bad answer, or by RFC 11
   §4's sweep once R, out of grace, still answers stale
   ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries),
   [RFC 14 §2.11](rfc-14-open-state.md#2.11%20State%20references)). So a moved stateid
   lost in R's failover is stale and reclaimed in R's grace, and one moved and
   later freed is bad. A grace instance advances
   only when the shard's open state is lost, at a restart or a failover that
   loses it; a handover that ships the state does not advance it, nor does the
   grace a moved namespace's shares are held in at the new installation
   (§7.4), so the bits
   of a live stateid stay current. Without the bits, a stateid lost in a restart
   would miss exactly as a released one does, and answered `NFS4ERR_BAD_STATEID`
   it makes Linux skip that file's locks in its reboot reclaim (§5.4).

   > ponytail: 24 bits of shard number name 16.7 million shards, about 160
   > times the per-child shard count RFC 11 plans for. Widen the field, at the
   > cost of random bits rule 2 forbids giving up, only if an installation's
   > shard numbers are shown to approach it. The 5 grace-instance bits are the
   > low bits of the incarnation that set the instance, and the incarnation
   > rises at every start, failover and handover, so they repeat after 32
   > raises of the incarnation, not after 32 losses: a stateid minted before a
   > loss reads as current, and is `NFS4ERR_BAD_STATEID`, only when the
   > incarnation rose by an exact multiple of 32 between its instance and the
   > current one. Its client's lease, 90 s, ends long before 32 raises
   > normally pass. Take bits from the shard number if a shard's incarnation is
   > ever seen to rise 32 times within one lease.
2. **`other` cannot be guessed.** At least **64 bits** of it **MUST** come from
   a random source, so a client that sees its own stateids learns nothing that
   names another's. `other` has the room: RFC 14 names an NFS open by its
   `Token`, which `other` carries, and takes no `OpenID` for it, and gives a
   lock state, delegation, layout or copy an ID that carries those bits
   ([RFC 14 §2.11](rfc-14-open-state.md#2.11%20State%20references)); a counter or a
   timestamp in their place makes every neighbouring stateid reachable by
   arithmetic. The rule binds stateids, which are bearer tokens with room for
   it; the 8-byte client ID is not one (§5.1).
3. **`seqid` is held with the state at the primary**, as a field of the record
   the stateid names, changed in the step that applies the request
   ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)): an open's `Seq`,
   raised by every `OPEN` upgrade, `OPEN_DOWNGRADE` and `CLOSE`; a lock state's
   `LockState.Seq`, raised by every `LOCK` and `LOCKU` that changes the locks of
   one (open, lock owner) pair; and the layout's `Seq`, raised by every `LAYOUTGET`
   it grants, every `LAYOUTRETURN` and every layout recall, which carries the
   new value so the client can order a `LAYOUTGET` reply against the recall
   ([RFC 14 §2.6](rfc-14-open-state.md#2.6%20Layout)). A delegation is never changed in place, so its stateid keeps
   the `seqid` it was granted with. A request with an older `seqid` is
   `NFS4ERR_OLD_STATEID`, a newer one `NFS4ERR_BAD_STATEID`. NFSv4.1's `seqid` 0
   means "current" (RFC 8881 §8.2.2).
4. **A stateid is checked against its client** wherever the request names one
   — the client ID of the session, or for NFSv4.0 the client ID in the open or
   lock owner of a sequenced operation (`OPEN`, `CLOSE`, `OPEN_DOWNGRADE`,
   `LOCK`, `LOCKU`) — and one belonging to another client is
   `NFS4ERR_BAD_STATEID` (`ErrState` (`NotYours`)).
5. **Over NFSv4.0, I/O is checked against the open's principal.** An NFSv4.0
   `READ`, `WRITE` or `SETATTR` carries no client ID and no session, so rule 4
   cannot run there. Instead the request's credential **MUST** name the
   principal that made the open the stateid names (`Open.Principal`,
   [RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)); the service refuses one
   naming any other principal with `ErrState` (`NotYours`), which on these three
   operations is answered `NFS4ERR_ACCESS` (§11.5), and the stateid grants it
   nothing. A lock
   stateid is checked against the principal of the open it was taken under.
   The special stateids below name no open and are checked per operation as
   anonymous I/O is. Over NFSv4.1 the same principal check runs beside rule 4,
   as [RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open) requires of every use of
   an open, except for the machine principal of a client under `SP4_MACH_CRED`,
   which RFC 14 admits as the record's `Owner` for the calls its `Protect`
   names (§3.3).
6. **I/O under a delegation stateid belongs to the client, not to a
   principal.** RFC 8881 §9.1.3 lets a client use a delegation's stateid for
   `READ`, `WRITE` and `SETATTR` by any of its open owners, under any principal,
   so rule 5 does not apply to it. Such a request **MUST** be refused
   `NFS4ERR_BAD_STATEID` (`ErrState` (`NotYours`)) unless the delegation is held,
   unrevoked, by the client of the session it arrives on, and **MUST** then be
   authorised afresh as anonymous I/O is — the file's ACL and mode against the
   request's credential, and every deny mode and mandatory lock
   ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)). The delegation proves the
   client may cache; it grants no user access the user does not have.

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
without changing state. `TEST_STATEID` and `FREE_STATEID` carry no file handle
(RFC 8881 §18.48, §18.38), and clients send them without `PUTFH` while
recovering from a revocation, so each stateid is resolved by rule 1's shard
number and index, never through the current file handle; a stateid quoted with
a file handle that names another file than its record's is
`NFS4ERR_BAD_STATEID`. A stateid of a removed or retired share is
`NFS4ERR_BAD_STATEID` and is never flagged revoked (§11.5): share removal
deletes the records that could list it, so nothing could ever free it, and a
client told its state was revoked would test every stateid it holds on every
`SEQUENCE`. `FREE_STATEID` frees any stateid in the client's revoked set —
an open, lock state, delegation or layout, revoked by an administrator or at a
recall deadline — answering `NFS4_OK` through the service's `FreeState`
([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)),
and releases a lock state with no locks held; a lock state still holding
locks, or a live open, delegation or layout, is refused with
`NFS4ERR_LOCKS_HELD` (`ErrState` (`LocksHeld`)), and a stateid that resolves to nothing is answered by
rule 1 — `NFS4ERR_STALE_STATEID` or `NFS4ERR_BAD_STATEID` — never success. `RECLAIM_COMPLETE` maps to `ReclaimComplete`
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), naming the (shard, instance)
pairs the session's `SEQUENCE` replies have shown in `ClientStatus.Reclaim`,
which the adapter keeps per session
([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)).

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
  under the revoked delegation are refused. The adapter learns of it only
  through the service: `Revoked` with reason `RevokedGrant` and its
  `GrantID`, after which `Renew` reports a grant revoked at its recall
  deadline in `ClientStatus` and the
  answering node sets the flag on the client's next `SEQUENCE` until the client
  frees the stateid, and `ErrState` (`Revoked`) on a later use of the stateid, which is answered
  `NFS4ERR_DELEG_REVOKED`, never the `NFS4ERR_BAD_STATEID` of `ErrState` (`NotYours`)
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). That client is offered no delegation
  for 90 s after the revocation by that primary, each further revocation
  restarting the period; its other delegations end by their own recalls
  ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).
  The fence is bounded in time, not tied to the lease: an active client renews
  its lease on every request, so a lease-long fence would never lift.
- `CB_RECALL_ANY` is sent when a client reaches its grant budget at one
  primary, 4096 grants, naming 2048 to keep, through `Callbacks.RecallAny`
  when the table raises `Sink.RecallAny`; a client at the budget is offered
  no delegation until its count falls below the budget
  ([RFC 14 §5.5](rfc-14-open-state.md#5.5%20A%20client%27s%20grants%20are%20bounded)).
- **A lock request recalls a delegation.** A byte-range lock request from any
  other client — NFSv4 `LOCK`, an NLM lock (§6.1), an SMB lock — on a file under
  a read or write delegation **MUST** recall it to none before the lock is
  decided, as RFC 14 requires of every grant with read or write caching
  ([RFC 14 §7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols); the
  SMB form is MS-FSA 2.1.5.7). A delegation lets its holder take locks locally;
  without the recall, the holder's local lock and the other client's lock are
  both granted over the same range, and neither is told.
- **While the recall runs, the request is told to retry, never refused.** An
  NFSv4 `LOCK` or `LOCKT` is `NFS4ERR_DELAY` (`ErrDelay`) and leaves no
  waiter; a blocking NLM lock is `NLM4_BLOCKED` (`ErrBlocked`) with a waiter,
  granted by `NLMPROC4_GRANTED` once the recall ends and nothing then conflicts; a
  non-blocking NLM lock or `NLMPROC4_TEST` is `NLM4_DENIED_GRACE_PERIOD`, which NLM
  clients retry
  ([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)). `NFS4ERR_DENIED`
  or `NLM4_DENIED` would tell the client the range is held when it may be free,
  and an `NLM4_BLOCKED` with no waiter behind it would never be granted.
- **An SMB handle-cached open is broken before a deny mode refuses NFS.** An
  NFSv4 `OPEN`, or an anonymous or delegation-stateid `READ` or `WRITE` (NFSv3's
  included), that would be refused only by the deny mode of an SMB open whose
  grant caches handles **MUST NOT** be refused at once: the handle caching is
  broken first, the request answered `NFS4ERR_DELAY` / `NFS3ERR_JUKEBOX`
  meanwhile, and the deny mode checked again once the break ends
  ([RFC 14 §6](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open);
  [RFC 22 §6.3](rfc-22-smb.md#6.3%20Breaks)). The cached open may be one the
  Windows application already closed; only an open still in use after the break
  refuses the request, with `NFS4ERR_SHARE_DENIED`, `NFS4ERR_LOCKED` or
  `NFS3ERR_ACCES` (§11.5).

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
| a reclaim in grace of state the client did not hold (`ErrState` (`NoReclaim`)) | `NFS4ERR_RECLAIM_BAD` |
| a reclaim sent after that same client's `RECLAIM_COMPLETE` for the same shard and the same grace instance (RFC 8881 §18.51.3) | `NFS4ERR_NO_GRACE` |
| reclaim an open | `OPEN` with `CLAIM_PREVIOUS` |
| reclaim a lock | `LOCK` with `reclaim` set |
| reclaim a delegation (`CLAIM_PREVIOUS` with a delegation type), in a shard in grace | the open, without a delegation: the shard lost its grants, which are not durable ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)) |
| reclaim a delegation in a shard not in grace that still holds it for the client | the open with the delegation as it stands, granting nothing new ([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)) |

![When the shard of builds fails over or its primary restarts, a new grace instance starts in which only reclaims are accepted. An NFSv4.1 client is told by SEQ4_STATUS_RESTART_RECLAIM_NEEDED on every SEQUENCE reply and reclaims in every shard, its RECLAIM_COMPLETE closing reclaim for that instance only; an NFSv4.0 client gets NFS4ERR_STALE_CLIENTID on RENEW and NFS4ERR_STALE_STATEID for the lost shard's stateids, repeats SETCLIENTID and the confirm and reclaims; an NLM host is sent SM_NOTIFY by UDP from the floating address after the NSM state number is raised, and the shard runs its whole grace. A gateway node's loss alone loses nothing, runs no grace and sends no SM_NOTIFY](img/rfc21-recovery-signals.svg)

**Reclaim is tracked per shard and per grace.** Clients send
`RECLAIM_COMPLETE` once at mount, when there is nothing to reclaim, so a rule
that counted one `RECLAIM_COMPLETE` for the client's lifetime would refuse
every reclaim after the first failover. Instead:

- Every grace a shard enters is a new **grace instance**. A `RECLAIM_COMPLETE`
  **MUST** be recorded per (client, shard, grace instance), for every shard
  the client's record names that is in grace when it arrives
  ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)); a
  grace instance is named by the shard record's `GraceInstance`, which a start
  or failover that loses the shard's open state sets to the incarnation it
  serves under, and which a handover that ships the table keeps — unless the
  old primary is lost before the new one holds the table, when the new
  primary runs grace and sets it too
  ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).
  It closes reclaim for those instances
  only. A later failover starts a new instance, in which the same client may
  reclaim again.
- **A grace instance never repeats, on one node too.** Every start of a node
  raises the incarnation of every shard it serves, in the transaction that
  starts it and before any open-state request is answered
  ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard),
  [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)), so
  a single-node restart opens a new instance. Were the instance to repeat, the
  `RECLAIM_COMPLETE` a client sent at mount would end the new grace for it at
  once, its reclaims would be `NFS4ERR_NO_GRACE`, and another client could take
  its lock.
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
  failover would lose its opens in every healthy shard. That includes a
  delegation: a reclaim there of an open that holds one returns the open with
  its delegation, and only a shard in grace answers without one (the table
  above).
- **NFSv4.0 is told by its client ID.** NFSv4.0 has no session flag. While the
  client record's `Done` for any shard is below that shard's grace instance,
  which a loss makes it, the client's `RENEW` and every operation naming its client ID
  **MUST** be `NFS4ERR_STALE_CLIENTID` (`ErrState` (`StaleClient`)), and a stateid in the
  lost shard `NFS4ERR_STALE_STATEID` (`ErrState` (`StaleState`)), until the client repeats `SETCLIENTID`
  and `SETCLIENTID_CONFIRM`
  ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)). The
  repeat carries the same owner and verifier, so it is not a reboot: the
  confirm keeps the client ID, sets every `Done` to the current instance, and the client reclaims, a
  shard that lost nothing returning what it holds. `NFS4ERR_STALE_STATEID`
  alone is not the signal: NFSv4.0 clients answer it with `RENEW`, which would
  succeed while the client ID is valid, and start reboot recovery only on
  `NFS4ERR_STALE_CLIENTID`, so they would loop.
- **A stateid the loss took is stale, never bad.** On both minor versions a
  stateid minted before the shard's last loss, or quoted while the shard is in
  grace, whose record is gone is `NFS4ERR_STALE_STATEID` by its grace-instance
  bits (§5.2 rule 1). Linux marks an open answered `NFS4ERR_BAD_STATEID` for
  recovery without grace and skips its locks in the reboot reclaim, so a
  client writing a locked file at the restart would lose the lock while the
  shard was in grace and would have accepted its reclaim.

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

**`LOCK`, `LOCKU`, `NLMPROC4_LOCK` and `NLMPROC4_UNLOCK` each name one range**, passed
to the service as a one-element list
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)). A `LOCKU` of a range the owner
holds only part of, or none of, releases what it holds there and succeeds, as a
POSIX unlock does; `NFS4ERR_LOCK_RANGE`, which RFC 8881 §18.12.4 allows there,
is never answered (§11.5).

**A blocking NFSv4 lock is polled, and its waiter reserved.** A `LOCK` of type
`READW_LT` or `WRITEW_LT` that conflicts is `NFS4ERR_DENIED` (`ErrLocked`) and
leaves a waiter; the client polls by sending it again, and a repeat of the same
range and type keeps the waiter's place. When the range frees, it is held for
that owner until its next poll takes it or one lease period passes, and a
client that asked for notification is sent `CB_NOTIFY_LOCK`, which is optional;
the poll decides ([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)).

### 5.6 Open-state and capacity errors on the wire

Which status each open-state and capacity error is answered with, and so
whether a client retries, reclaims or gives up, is §11.5's one table.

### 5.7 Translation to open state

The client, stateid, open, delegation and lock forms of this chapter map onto
RFC 14's records as §11.4 states, and the identity a call carries as §11.1
states.

## 6. NFSv3 locking: NLM and NSM

NFSv3 holds no state, so locking lives in two side protocols. **NLM** (Network
Lock Manager) takes and releases byte-range locks. **NSM** (Network Status
Monitor) handles crashes: each side's status daemon (`statd`) records whom it
holds locks with and, after a restart, sends them `SM_NOTIFY` so they reclaim.
Here NLM locks are RFC 14 locks, conflicting with NFSv4 and SMB locks, and
`SM_NOTIFY` is sent when a shard fails over or its primary restarts, not when a
client's gateway node dies.

### 6.1 NLM

NLM v4 maps onto RFC 14: the NLM host — its name (`caller_name`) **and the
source address it sends from** — is the client, an `(svid, oh)` pair — process
ID and owner handle — the lock owner, and each lock an RFC 14 lock under an
anonymous open of the file. The records, including what NSM needs to notify a
client, are RFC 14's.

- **A host is bound to its address.** NLM carries no credential that proves
  which host sent a request: `caller_name` is whatever the sender writes. The
  client record of an NLM host **MUST** be keyed by `caller_name` together with
  the source address of the request that created it: both are its `Name`
  ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)), its `ClientID` is minted
  as any client's is, and the host's NSM state number is its `Boot` (§3.3). A request naming that
  `caller_name` from another address is another client, and can neither test,
  unlock nor free the first host's locks. `NLMPROC4_FREE_ALL` and NSM's
  `SM_NOTIFY` for a host release its locks only when they arrive from the
  address its record holds.
- **A lock is a permission decision.** An NLM lock is a lock outside any open,
  so it needs read access or ownership of the file, and a write lock asks no
  more than a read lock
  ([RFC 19 §3.8](rfc-19-authorization.md#3.8%20Locks%20outside%20an%20open)). A
  store error resolving the caller fails the lock, never authorises it as
  anonymous ([RFC 18 §5.4](rfc-18-identity.md#5.4%20Errors%20never%20become%20answers)).
- **NLM is admitted at `Claimed`.** `LOCK`, `TEST`, `UNLOCK`, `CANCEL`,
  `SHARE`, `UNSHARE`, `FREE_ALL` and their `*_MSG` forms are admitted on a share
  whose `MinProof` is higher than `Claimed`; the claimed caller is still
  resolved and gated as above.
- **Replies leave from the address the client used.** Callbacks to the host —
  `NLMPROC4_GRANTED`, `NLMPROC4_GRANTED_MSG`, the `NLMPROC4_*_RES` replies — **MUST** be sent
  from the floating address the host's requests arrived on, never from a
  node's own address, because clients match a callback against the server
  they sent the lock to and drop one from any other address.
- **Callbacks are calls the server makes, on its own connection.** A callback
  is an RPC to the host's NLM service, not a reply on the connection that
  carried the request, which may have closed by the time a lock is granted.
  The adapter finds the service's port through the host's port mapper
  (`GETPORT`, NLM version 4, TCP) and connects to it from the floating address
  ([RFC 20 §3.3](rfc-20-adapters.md#3.3%20Callbacks%20are%20the%20adapter%27s%20to%20deliver)).
  A host whose port mapper or NLM service cannot be reached is a callback that
  failed, and the grant is resent as below.

> decision: binding a host to its source address stops one host from
> releasing another's locks by naming it, which is all NLM's design allows.
> It does not stop a sender that forges the source address on the same
> network; NLM has no other identity to check, and every NLM server shares
> that ceiling. A host whose address changes — a DHCP renewal — is a new
> client, and its old locks stay until revoked, as after any NLM host loss
> ([RFC 14 §4.5](rfc-14-open-state.md#4.5%20NLM%20locks%20and%20restart%20notification)).
> Withdraw only for a deployment whose NLM hosts authenticate, which NLM
> cannot express.

- **Blocking locks.** A lock request that may wait (`NLMPROC4_LOCK` with `block`
  set) and conflicts, or meets a recall (§5.3), returns `NLM4_BLOCKED`
  (`ErrBlocked`). The primary keeps the waiter, and on grant the gateway
  node holding the client's registration sends `NLMPROC4_GRANTED_MSG` /
  `NLMPROC4_GRANTED`. No worker waits
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)).
- **A waiter has RFC 14's one lifetime.** It ends when granted, on
  `NLMPROC4_CANCEL`, when the host's restart notification or `NLMPROC4_FREE_ALL`
  expires the client, when the client is revoked, or when `DropShare` ends it
  for the share's removal or its namespace's move (§7.4)
  ([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)). The adapter
  **MUST NOT** drop a waiter on a timer of its own, and the request is exempt
  from the per-call deadline
  ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)): NLM
  clients wait for a blocking lock indefinitely, and a waiter dropped behind
  their back is never granted, since they do not resend a request answered
  `NLM4_BLOCKED`. A waiter ended by a share's removal or move is reported
  through `Revoked` with the share-removed or moved reason
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)); NLM has no message that
  withdraws a `NLM4_BLOCKED`, so the adapter sends nothing, and the host's next
  request on the share's handles is `NLM4_STALE_FH` (§11.5).
- **A cancel is answered only after the waiter is gone.** `NLMPROC4_CANCEL` **MUST**
  call `CancelLock` and answer only once it returns. If it reports that the lock
  was granted first, the lock stands and `NLMPROC4_GRANTED` is sent as for any
  grant; otherwise no lock is granted for that request afterwards
  ([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)). Answering first
  would leave a lock granted after its request was cancelled, held by no client
  that knows it.
- **The grant is resent until the host answers.** `NLMPROC4_GRANTED` (or
  `NLMPROC4_GRANTED_MSG` and its `NLMPROC4_GRANTED_RES`) is sent, and resent on a 1 s
  backoff growing to 30 s, until the host acknowledges it, refuses it
  (`NLM4_DENIED` or another error in the reply), cancels the lock or restarts,
  or the lock is released by an administrator's revoke or by `DropShare`
  ([RFC 14 §2.10](rfc-14-open-state.md#2.10%20Lock%20waiters)). Each send is one
  `LockWaitOver` call, which returns the host's answer — `refused` false for an
  acknowledgement, true for a refusal — or an error when the host said nothing by the call's deadline,
  after which the service calls again and the lock stays granted
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). A refusal releases the lock
  as an unlock would, and the next waiter is considered. The lock is never
  released for a silence: the host may already be writing under a grant whose
  acknowledgement alone was lost. The resending is a timer and holds no worker.
- **Grace.** NLM shares RFC 14's per-shard grace with NFSv4: during it a
  non-reclaim lock is `NLM4_DENIED_GRACE_PERIOD`, and a request with `reclaim`
  set is a reclaim. An NLM host never counts as finished reclaiming, since NLM
  has no completion signal, so a shard in which any NLM host held locks runs
  its whole grace period
  ([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)).
- **Share reservations** (`NLMPROC4_SHARE`), which DOS-era clients use, map to an
  open with a deny mode, so they conflict with SMB and NFSv4 deny modes like
  any other.
- **Who holds a conflict.** An NLM lock refusing an NFSv4 `LOCK` or `LOCKT`
  is reported with client ID 0 and an empty owner (§5.5). In the other
  direction, an `NLMPROC4_TEST` refused by an NFSv4 or SMB lock reports a holder
  with `svid` 0 and an empty `oh`, for the same reason: an internal owner
  never reaches the wire.
- **A host whose record just went is connected afresh.** RFC 14 deletes an
  NLM host's record with its last unlock, and refuses a lock racing that step
  `ErrState` (`StaleClient`)
  ([RFC 14 §4.5](rfc-14-open-state.md#4.5%20NLM%20locks%20and%20restart%20notification)).
  The adapter then calls `Connect` for the host and sends the request once
  more; a `StaleClient` never reaches the host as `NLM4_DENIED`, and one met
  again on the retry is answered `NLM4_DENIED_GRACE_PERIOD`, which the host
  resends.
- `NLMPROC4_FREE_ALL` from the host's own address expires the client.
- The asynchronous procedures (`*_MSG` / `*_RES`) are accepted over TCP and
  answered on the client's NLM callback service, from the floating address.
- An NLM lock request from another host on a file under a delegation recalls
  the delegation first, and is answered meanwhile as §5.3 says: `NLM4_BLOCKED`
  with a waiter if blocking, `NLM4_DENIED_GRACE_PERIOD` if not, never
  `NLM4_DENIED`.

> decision: NLM calls are admitted at `Claimed` on a share that requires more,
> because a Linux client's `lockd` and `statd` send `AUTH_UNIX` even for files on
> a `sec=krb5` mount, and every NFSv3 `fcntl` and `flock` on a Kerberos-only share
> would otherwise fail. The exemption covers lock calls only, never data, so the
> ceiling is that an admitted host can hold locks on the files its claimed uid
> can read. Make it a per-share opt-in when a lock-holding denial of service from
> an admitted host is reported, or a client is shown to send NLM over
> `RPCSEC_GSS`.

### 6.2 NSM

The server monitors clients as its peers expect: a host's first NLM call
registers it for restart notification, its `mon_name` and `priv` cookie kept
in the client record's `Adapter` bytes (§3.3); `SM_MON` and `SM_UNMON` are calls a server makes to its
own monitor, never requests a peer sends it
([RFC 14 §4.5](rfc-14-open-state.md#4.5%20NLM%20locks%20and%20restart%20notification)).

- After a shard in which any NLM host held locks fails over or its primary
  restarts, the installation's NSM state number is raised, durably, before the
  shard's grace begins; for a moved namespace it is raised when its address
  arrives at the new installation, inside the moved shares' grace there
  (§7.4) ([RFC 14 §4.5](rfc-14-open-state.md#4.5%20NLM%20locks%20and%20restart%20notification)),
  and the new primary asks the gateway node currently holding each address
  to send `SM_NOTIFY` to every NLM host whose record names the shard, at the
  address its `Adapter` bytes hold, which the `Revoked` report carries, naming the server name the client monitored — the floating
  address's name, not a node's — with the new state number, so the client's
  statd reclaims locks.
- `SM_NOTIFY` is sent by UDP, as statd peers listen for it. Apart from the port
  mapper's answers and the NLM and NSM replies served over UDP for parity
  (§2.2), it is the only UDP the server sends, and it carries no file operation.
- Losing a client's gateway node is not a restart for NLM: no lock held at
  another primary was lost, so no `SM_NOTIFY` is sent for it ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)).

### 6.3 MOUNT

MOUNT v3 is the service's share calls
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)): `MNT` is `Root` with the path,
which resolves it and returns the root handle and the share's flavours; `DUMP`
is empty, and `UMNT` and `UMNTALL` change nothing; `EXPORT` is `ListShares`, the
shares [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)
lets the caller see.

`MNT` and `EXPORT` are admitted on the share's state and its client rules only,
without the `MinProof` and `MinProtection` step, so a Kerberos-only share can
return its flavours to a client whose mount daemon sends `MNT` as `AUTH_UNIX`.
The handle `MNT` returns is gated like any other on every later call.

> decision: `MNT` and `EXPORT` skip the proof and protection minimums, because a
> Linux client sends `MNT` as `AUTH_UNIX` even for `sec=krb5`, and every NFSv3
> Kerberos mount of a Kerberos-only share would otherwise be refused before the
> client learns the share's flavours. The ceiling is what an `AUTH_SYS` `MNT`
> learns: the share's flavours and its root handle, which is a bearer token over
> NFSv3, but every call made with it meets `MinProof` and is refused. Overturned
> if a supported client is shown to send `MNT` over `RPCSEC_GSS`, or a root handle
> is shown to reach anything without a later admitted call.

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
  `NFS4ERR_INVAL` or `NFS4ERR_BADNAME`. On a share under the `windows` name
  rule that includes a Windows reserved device name, from the one list
  [RFC 7 §3.8](rfc-7-namespace-metadata.md#3.8%20Names%20every%20protocol%20of%20a%20share%20can%20use)
  keeps and [RFC 22 §12.3](rfc-22-smb.md#12.3%20Names%20SMB%20cannot%20carry)
  cites; under the `posix` rule NFS creates it.

- NFSv4.1's character-set capability (`fs_charset_cap`) follows the share's
  name rule ([RFC 7 §3.8](rfc-7-namespace-metadata.md#3.8%20Names%20every%20protocol%20of%20a%20share%20can%20use)):
  on a case-sensitive `posix` share, where NFSv3 clients can create names that
  are not UTF-8, it sets `FSCHARSET_CAP4_CONTAINS_NON_UTF8` and clears
  `FSCHARSET_CAP4_ALLOWS_ONLY_UTF8`; on a share that refuses such names — the
  `windows` rule, or any case-insensitive share — it sets
  `FSCHARSET_CAP4_ALLOWS_ONLY_UTF8` and clears the other, since no stored name
  there can be anything else. How SMB shows such names is [RFC 22 §12.3](rfc-22-smb.md#12.3%20Names%20SMB%20cannot%20carry)'s.
- An NFS `RENAME` of a directory, or `REMOVE` of an empty one, is not refused
  because a file below it is open, over SMB or NFS: POSIX allows both, and only
  an SMB rename or delete checks the directory's open children
  ([RFC 14 §9.5](rfc-14-open-state.md#9.5%20Open%20children%20refuse%20an%20SMB%20rename%20or%20delete%20of%20their%20directory)).
  The open keeps working, since it holds the file, not its name.

### 7.4 Pseudo-filesystem, SECINFO and locations

That the pseudo-filesystem is the service's, built from the share list on
every node, and what a pseudo directory lists, is
[RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)'s. What it
reports is this section's, and the service computes it; the adapter encodes it
and adds no export of its own:

- a pseudo directory's handle is minted by the handle codec as its own kind
  ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)),
  from a digest of the installation's identity and the directory's path, and
  nothing else; so is its numeric file id. Every node and every restart mints
  the same. A pseudo handle whose path no longer leads to a share is `ErrStale`;
- its `fsid` is one reserved value per installation, never a share's; its
  change attribute is `ShareList.Version`, so adding, removing, renaming or
  re-pathing a share is seen by every client's cache;
- a share's root reports the share's own `fsid`, and `mounted_on_fileid` the
  numeric id of the pseudo directory it sits in, so a client sees a mount
  crossing there;
- `SECINFO` on a pseudo directory answers every flavour some share admits; on
  a share's path, the share's `Flavors`.

`fs_locations` — the attribute telling a client where else a filesystem can be
reached — is returned only for a drain
([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)):

- a draining gateway node answers operations on a share with
  `NFS4ERR_MOVED`, and `fs_locations` names another gateway node of the same
  installation and the same path;
- every other time the attribute lists no location;
- a referral to another installation is never returned
  ([RFC 17 §1.1](rfc-17-vfs.md#1.1%20Non-goals)), and `fs_locations_info` is not
  supported.

**A namespace move carries no NFS state.** When a namespace moves to another
installation
([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)),
NFS has no way to carry a mount across, since the one mechanism, a referral,
is refused above. During the freeze, calls are answered `NFS4ERR_DELAY` /
`NFS3ERR_JUKEBOX`. No open, lock or delegation is carried: B is a different
server, with its own server owner and scope (§3.3). Share identities and
FileIDs survive the move, so a handle taken at A resolves at B
([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)).
What a client sees depends on whether the move carries the address it mounts:

- **The move names A's address, and the operator moves it to B.** The address
  is named at the move's freeze, and the move is refused if it serves anything
  but the moved namespace's shares: on one node, A's address serves every share
  A has, and moving it would send the clients of A's other namespaces to B,
  where every handle is `ESTALE`. **From A's release until the address reaches
  B, A stops serving it entirely**: the control-plane runtime's reconfigure
  removes the address, closing its listener and every connection on it
  ([RFC 20 §3.3](rfc-20-adapters.md#3.3%20Callbacks%20are%20the%20adapter%27s%20to%20deliver)),
  so no NFS call there is answered, and never
  `NFS4ERR_STALE` / `NFS3ERR_STALE`; a hard mount keeps retrying, as through
  a server restart. A stale answer would be worse than none: Linux marks the
  inode stale and answers `ESTALE` itself afterwards, without asking, so every
  file in use in the interval would stay dead after the address reached B.
  **B holds the moved shares in an NFS-only grace from its publish**: it
  refuses non-reclaim NLM locks and NFSv4 opens and locks there and accepts
  their reclaims, while SMB on those shares is unaffected; it ends at the
  earlier of `migration.reconnect_wait` and one lease period after the address
  arrives, and an address arriving after that bound opens a fresh grace before
  B notifies
  ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)
  owns the rule and its decision), so no lock is granted at B before the
  hosts that held it at A can reclaim it, unless the address outlasts the
  bound. The address's arrival (`AddressArrived`) is step 10 of
  [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step),
  not B's publish: when B takes the address, it raises its NSM state number
  above the larger of A's, carried in the move's delta, and its own, and only
  then **MUST** send `SM_NOTIFY` (§6.2), from that address, to every NLM host
  monitored for those shares — the monitor records travel in the delta with
  the namespace's client records — so each host's statd reclaims its locks in
  that grace. That grace advances no grace instance and signals no NFSv4
  client. Sent at publish, no node of B holds the address yet, a notification
  from B's own address sends hosts to reclaim at A, and Linux ignores a later
  notification whose state number it has already seen. An NFSv3 client's
  handles keep working at B. An NFSv4 client sees a new server owner and scope
  and its client ID unknown, and B holds no client record for it: a Linux
  NFSv4.1 client, told of a new scope, sends no reclaim, and any client's
  reclaim is `NFS4ERR_NO_GRACE`. Its opens, locks and delegations are lost, a
  lock's holder sees `EIO`, and its new opens are `NFS4ERR_GRACE` until B's
  grace ends.
- **The move names no address.** A answers every handle into the moved shares
  `NFS4ERR_STALE` / `NFS3ERR_STALE` from its release, as for a removed share
  (§11.5), and clients remount against B's name, with no state there.

> decision: a move carries no NFS state, because carrying it would need a
> referral to another installation and client state B does not hold. When the
> address moves, NFSv3 clients reclaim their NLM locks in B's grace and NFSv4
> clients lose all their state, locks included, and open again after that
> grace; when it does not, every client remounts. The cost is lost NFSv4 locks
> with `EIO` to their holders, or a remount, and, while the address is in
> transit, calls that go unanswered until it arrives, which a soft mount turns
> into `EIO` at its timeout. Revisit with a referral, or with client records
> carried to B, when a move without lost locks is required of NFS clients.

## 8. Attributes

NFSv4 attributes are a numbered list: some required, some recommended; the
client asks which are supported (`supported_attrs`). Each one served maps to a
field of RFC 7's File, the share's capabilities, or a fixed value.

### 8.1 Supported

Every REQUIRED attribute of RFC 8881 is supported. The recommended and NFSv4.2
attributes, and the field each is read from, are listed in
[Appendix B](#Appendix%20B%20%E2%80%94%20attributes). The ones that carry a
decision: `change` is the file's version including writes not yet committed
(§9.2); `change_attr_type` is `NFS4_CHANGE_TYPE_IS_UNDEFINED` (§15);
`maxread`, `maxwrite` and `maxfilesize` are §2.3's.

**ACL attributes.** `acl`, `dacl`, `sacl` and `aclsupport`, who may read and
write each, and how their entries are spelled are §11.3's.

- `mode` and `dacl` are unaffected: the file's owner may set them through its
  implicit rights to read and write the ACL, unless the ACL holds an
  owner-rights entry, in which case the walk decides
  ([RFC 19 §3.1](rfc-19-authorization.md#3.1%20One%20request%2C%20in%20order)).
  Setting `owner` to another principal needs the privileged flag; setting it
  to the caller itself needs the **change the owner** right, which an ACL can
  grant; `owner_group` needs the owner, or a caller with that right, naming a
  group it belongs to, or the privileged flag
  ([RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved));
  anything else is `ErrPerm`, answered `NFS4ERR_PERM` / `NFS3ERR_PERM`. Root on
  an export that leaves it unsquashed is privileged, so `chown` and `tar x` as
  root work as on a local disk.
- **Which NFS callers are privileged** is
  [RFC 18 §6.5](rfc-18-identity.md#6.5%20What%20the%20proof%20proved%3A%20authenticated%2C%20admin%2C%20privileged)'s;
  on the wire it means: over `AUTH_NONE` a caller is never privileged, being the
  export's anonymous principal; over `AUTH_SYS` uid 0 is the well-known root
  principal ([RFC 18 §2.1](rfc-18-identity.md#2.1%20Kinds%20and%20well-known%20principals)),
  privileged only on an export that leaves root unsquashed, and a share grant
  naming a principal `admin` makes no `AUTH_SYS` caller privileged, whatever uid
  it sends. Over `RPCSEC_GSS`, an `admin` grant makes the authenticated
  principal privileged (§10), and a Kerberos name linked to root is privileged
  exactly when the export leaves root unsquashed, as `AUTH_SYS` uid 0 is:
  root on a client running `tar xpf` on a `sec=krb5` export with
  `no_root_squash` restores owners as on a local disk, and the flavour gives
  root no more than the export's squash setting does.

> decision: an `admin` grant counts only over an authenticated flavour, as
> RFC 7 §7.4 decides for every protocol; the NFS reasoning is this. Over
> `AUTH_SYS` the uid is the client's assertion: with an `admin` grant to uid
> 1500, any client root could send uid 1500, read every file and plant a
> setuid-root binary, which is exactly what `root_squash` exists to stop, and
> a Linux server drops every capability for a non-zero fsuid. The cost is that
> an operator who wants a non-root administrator over NFS must use Kerberos.
> Revisit only for an export whose clients are all trusted, which is the
> export's setting (no squashing), not this rule's.

**`ACCESS` answers the granted set.** NFSv3 and NFSv4 `ACCESS` map to the
service's `Access`, which intersects the asked bits with `Files.Granted`, the
granted set of
[RFC 19 §3.4](rfc-19-authorization.md#3.4%20Maximal%20access%20and%20an%20open%27s%20grant)
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)), so the bits reported are
exactly those a later operation is authorised for, owner's implicit rights and
the privileged flag included. Each bit reports its rights, the same values in
both versions:

| `ACCESS` bit | On a file | On a directory |
| --- | --- | --- |
| `READ` | read data | list |
| `LOOKUP` | — | traverse |
| `MODIFY` | write data | delete a child |
| `EXTEND` | append, or write data | add a file, or add a subdirectory |
| `DELETE` | — | delete a child |
| `EXECUTE` | execute | — |
| `XAREAD`, `XALIST` | read extended attributes | read extended attributes |
| `XAWRITE` | write extended attributes | write extended attributes |

A bit with no right on that file type is never reported. `XAREAD`, `XAWRITE`
and `XALIST` exist only in NFSv4.2
([RFC 8276](https://www.rfc-editor.org/rfc/rfc8276.html)) and are never reported
on an earlier version.

**`time_metadata` always moves.** The file's ctime moves on every change,
whatever an SMB client has suspended on its own open
([RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward),
[RFC 22 §9.3](rfc-22-smb.md#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files)):
NFSv3 clients revalidate their caches by ctime, and one that froze would serve
stale data after an SMB write.

### 8.2 Not supported

Labeled NFS (`sec_label`, RFC 7204), named attributes (`OPENATTR`; the
REQUIRED `named_attr` attribute itself is supported and reads false on every
object) and a set of attributes no record holds are absent;
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

> decision: no named attributes. `OPENATTR` opens a hidden directory of
> attribute files. Solaris and illumos clients use it; macOS uses it for
> extended attributes and resource forks only when the server advertises
> `named_attr`, and otherwise keeps them in AppleDouble `._` files beside each
> file; Linux never sends it. A Linux home directory's extended attributes (desktop
> metadata, `user.*` tags) travel as RFC 8276 xattrs, which are served
> ([§9.3](#9.3%20NFSv4.2%20operations)). Revisit for a deployment whose clients are Solaris
> or illumos, or macOS clients whose `._` files are not acceptable, and depend
> on named attributes, mapping them onto the named
> streams SMB already uses ([RFC 7 §2.7](rfc-7-namespace-metadata.md#2.7%20Extended%20attributes%20and%20named%20streams)).

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
  `FILE_SYNC`.
- **What a stability reply waits for.** A stable write, and a `COMMIT`, **MUST**
  be answered once the journal has synced every record in its range, which
  carries the data and the modification time the write sets
  ([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)), and once the existence commit
  of every pending write in its range that overwrites committed content — the
  writes that need an overwrite record — has committed. An append or a hole
  fill does not wait for its existence commit, which follows lazily within the
  group commit's bounded age; a waiting reply joins the next group commit
  rather than issuing a transaction of its own
  ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)).
  So an untar or a log append costs one journal sync per stability point, and
  only an overwrite of committed bytes costs a share of one group commit too.
  There is no cheaper level to offer: `DATA_SYNC` waits for what `FILE_SYNC`
  waits for.
- **A stalled store delays only the replies that wait on it.** While the
  metadata store does not commit, a stability reply that covers an overwrite is
  `ErrDelay` at the caller's deadline, never success, and is answered
  `NFS4ERR_DELAY` / `NFS3ERR_JUKEBOX`, which the client retries
  ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal));
  one that covers only appends and hole fills is answered after the sync.
- The verifier in every `WRITE` and `COMMIT` reply — which changes when the
  server may have lost unstable writes, telling the client to resend them — is
  RFC 17's ([RFC 17 §5.8](rfc-17-vfs.md#5.8%20The%20write%20verifier)), passed
  through.
- **A loss reaches NFS as the verifier.** When the service reports to a
  `Commit` that unstable writes of the file were lost (`ErrLost`, reported once
  per loss), the `COMMIT` reply **MUST** carry the verifier sampled after the
  loss, which differs from the one those writes were answered with, and
  succeed: the client then resends every write since its last matching
  verifier (RFC 1813 §3.3.21, RFC 8881 §18.3.4), which recovers the data, where
  an error would lose it. A `COMMIT` is never answered with a verifier sampled
  before a loss it was told of.
- **A stable write whose own sync fails is an I/O error.** A `DATA_SYNC` or
  `FILE_SYNC` `WRITE` whose sync failed (`ErrLost` on the `Write`,
  [RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal))
  **MUST** be answered `NFS4ERR_IO` / `NFS3ERR_IO`, never success: the client
  was promised nothing for it, and a verifier cannot carry the loss of a write
  that was never answered.
- A write refused for capacity is answered as §11.5 says: "retry"
  (`NFS4ERR_DELAY` / `NFS3ERR_JUKEBOX`) for the journal's capacity or the
  share's journal limit while offload or repack can drain it, for as long as
  that lasts and never turned into "no space" by a deadline, and "no space", at
  once, only for a permanent cause
  [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)
  names.

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
- **`GETATTR` is answered at the primary**, where the engine holds the
  uncommitted writes, so no node answers from a committed record that lags
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
  every `wcc_data`, `before` is absent for a directory, always. For a file it
  is present only when the primary read it from the File and its overlay —
  committed attributes plus every write staged and not yet committed
  ([RFC 17 §5.7](rfc-17-vfs.md#5.7%20GetAttr)) — and accepted the operation in
  one hold of the file's **accept lock**, the per-file lock every write,
  truncate and attribute change takes before it draws its `Version`
  ([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps),
  [§9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)):
  no write from any client, through any node, is accepted between the capture
  and the operation, and `after` is read in the same hold once the operation is
  staged. The commit serialisation is not enough, since it orders commits while
  another write is accepted in between. Where the lock cannot be taken — a
  `WRITE` that waits on a recall, a forwarded operation, a lock contended past
  its bound — `before` is absent and the client revalidates. A `before`
  that another client's staged write fell behind would let the client patch a
  stale cache over it.

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
`LAYOUTERROR` and `LAYOUTSTATS`, and on a single node every layout operation
(§3.3).

**`COPY` and `CLONE` are one mechanism, two atomicities.** Both are the
service's `Copy`, the engine's clone
([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone),
[RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)).
RFC 7862 requires atomicity of `CLONE` only, and `COPY` reports the bytes it
copied. On the wire:

- **`CLONE` is atomic.** The whole range is one clone, resolved at one point of
  the source, and the reply is sent only when the clone is done. A `CLONE`
  longer than `clone_max_len`
  ([RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings),
  1 GiB by default) is `NFS4ERR_INVAL` (`ErrInvalid`) and changes nothing; the
  client falls back to copying.
- **`COPY` is not atomic.** It runs as bounded chunks (64 MiB) in offset order,
  each one small atomic clone, so only the current chunk's source range is
  frozen and a writer of the source waits at most one chunk; nothing freezes the
  whole source for the copy. The reply reports the bytes copied: at the
  caller's deadline the copy stops after its current chunk and answers a short
  `wr_count`, which RFC 7862 §15.2 allows, and the client continues from there.
  Its `wr_committed` is `UNSTABLE4` with the write verifier of §9.1, since the
  copied ranges are journal writes like any other, and a `COMMIT` of the
  destination makes them stable.
- a `COPY` or `CLONE` whose source and destination are one file and whose
  ranges overlap **MUST** be `NFS4ERR_INVAL` (`ErrInvalid`) and change nothing,
  as RFC 7862 §15.2.3 and §15.13.3 require and Linux answers `EINVAL`;
- a `COPY` or `CLONE` between shares in different namespaces **MUST** be
  `NFS4ERR_XDEV` (`ErrCross` (`CrossNamespace`)) and change nothing, so the client falls
  back to reading and writing: no chunk can be adopted across namespaces;
- a call that would change a running clone's source or destination range — a
  `COPY` chunk's included — is held at the service until that clone or chunk
  ends and then runs, and is answered `NFS4ERR_DELAY` / `NFS3ERR_JUKEBOX` only
  if its own deadline passes first
  ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)), so each clone reads
  one consistent source and an NFSv3 writer does not pay its client's fixed
  retry-later pause for a chunk that ends far sooner. The calls held this way
  are `WRITE`, `DEALLOCATE`, a `SETATTR` of `size`, a `COPY` or `CLONE`
  whose destination is such a range, and a `REMOVE` or last `CLOSE` that would
  release a running clone's destination
  ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone));
- **the hold budget is per session, and the adapter's.** A held NFSv4.1 call
  keeps its slot, so the adapter counts the session's calls waiting behind a
  clone and lets at most the larger of 1 and a quarter of the session's
  fore-channel slots wait at once: it passes `vfs.WithHold` with the call's
  remaining deadline while the session is under that count, and zero past it,
  so a further call meets `ErrDelay` with `DelayClone` at once and is answered
  `NFS4ERR_DELAY` (§11.5) while the rest of the session keeps moving
  ([RFC 20 §4.6](rfc-20-adapters.md#4.6%20Held%20requests),
  [RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)). NFSv3 and NFSv4.0
  have no session and no slots to exhaust, and pass their remaining deadline:
  every held call is bounded by its own deadline;
- a source range the engine reads as Lost is carried to the destination as
  Lost, never as zeros; a clone refused at admission leaves the destination's
  prior content, and one begun is resumed after a crash, never undone.

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

Not offered: every `COPY` is answered synchronously, `ca_synchronous` false
included, which RFC 7862 §15.2.3 lets a server do, with the short count of §9.3
at the caller's deadline, from which the client continues. No copy stateid is
issued, so `OFFLOAD_STATUS` and `OFFLOAD_CANCEL` name nothing and are answered
`NFS4ERR_BAD_STATEID` by the adapter, with no service call, and `CB_OFFLOAD` is
never sent ([RFC 17 §9](rfc-17-vfs.md#9.%20Migration)).

> decision: copies stay synchronous, as copychunk and NFSv4.2 `COPY` are today.
> An asynchronous copy needs RFC 14's copy state at the destination's primary
> and a callback that may be lost with it; a short count gives a client the
> same progress without either. Offer it when a client is shown to stall on a
> long synchronous `COPY` that a short count does not unblock.

## 10. RPCSEC_GSS

`RPCSEC_GSS` is how NFS carries Kerberos: client and server set up a GSS
(Generic Security Services) **context** — a handle, a session key and a window
of accepted sequence numbers — and every later call is checked against it.
Authentication and principal mapping are
[RFC 18 §3.3](rfc-18-identity.md#3.3%20Kerberos)'s. On the wire:

- a GSS context **MUST** be held in the memory of the gateway node that
  created it, and is not shared or stored. After a gateway node loss, the
  client's next call on the old handle is answered `RPCSEC_GSS_CREDPROBLEM`,
  and it establishes a new context;
- the sequence window is 128. A call whose sequence number is below the
  window, or already seen within it, is dropped unanswered (RFC 2203
  §5.3.3.1); one above it advances the window. One whose verifier checksum
  fails is rejected with `RPCSEC_GSS_CREDPROBLEM` (RFC 2203 §5.3.3.4.2).
  Neither dropped nor rejected call runs;
- `krb5` (authentication), `krb5i` (plus integrity) and `krb5p` (plus privacy,
  i.e. encryption) are served; the share's minimum level is
  [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)'s
  admission, answered `NFS4ERR_WRONGSEC` over NFSv4 and, over NFSv3, by
  rejecting the RPC with the `auth_stat` `AUTH_TOOWEAK` (RFC 5531), which
  RFC 2623 §2.4 allows beside an NFS error, as
  [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values) maps
  `ErrWrongSecurity` (§11.5);
- a context is keyed by its handle, not by the TCP connection that created it,
  so a reconnect or a trunked connection keeps using it;
- the context table is bounded: at most 10 000 contexts per gateway node,
  charged to [RFC 24](rfc-24-resources.md#5.1%20No%20queue%20is%20unbounded)'s
  connection-state pool. When it is full, the same principal's least recently
  used context is evicted first, then the least recently used overall, so one
  principal looping `INIT` evicts only its own. A context idle 8 hours is
  evicted; only a call whose header MIC verified touches its idle time;
- `RPCSEC_GSS_DESTROY` needs a valid header MIC and an in-window sequence
  number, since handles travel in the clear;
- a call on a context past its ticket's end time, or with a sequence number at
  or above `MAXSEQ`, deletes the context and is answered
  `RPCSEC_GSS_CTXPROBLEM`, so the client fetches a new ticket rather than
  retrying a dead handle. Both are checked only after the header MIC verifies.

> ponytail: the context cap and the idle timeout are constants. Make them
> settings when a deployment's client count, or its clients' idle pattern, is
> shown to need others.

> decision: an NFSv3 call with a flavour the share refuses is rejected at the
> RPC layer with `AUTH_TOOWEAK`, not answered `NFS3ERR_ACCES`, in agreement
> with RFC 17 §4.3's mapping. NFSv3 has no wrong-security status, and RFC 2623
> §2.4 leaves the server either an NFS error or an RPC authentication error;
> RFC 5531's `auth_stat` names `AUTH_TOOWEAK`. knfsd answers `NFS3ERR_ACCES`
> instead. Applications see `EACCES` either way, since Linux turns
> `AUTH_TOOWEAK` into `EACCES`; the difference is the line Linux logs, which
> names the flavour as the cause rather than the file's permissions, and that
> diagnostic is the whole reason for the choice. Revisit if a supported NFSv3
> client is shown to handle `AUTH_TOOWEAK` worse than `NFS3ERR_ACCES`.

## 11. Translation

The adapter translates and the core decides
([RFC 20 §2.1](rfc-20-adapters.md#2.1%20Two%20questions%20place%20every%20piece%20of%20logic)).
This chapter is every translation the NFS adapter makes, in one order: who is
calling (§11.1), which retry a call is (§11.2), the namespace (§11.3), open
state (§11.4) and the errors that come back (§11.5); then how the translations
are tested (§11.6) and watched (§11.7).

The adapter reaches the core only through `vfs.Service`
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)): `SignIn`, `Spell` and `Find`
for identities and names, and the file and state calls for everything else. It
imports no identity, metadata or open-state package. What it must keep beyond a
connection it keeps in the client record's opaque `Adapter` bytes, which the
core stores and never reads (§11.4).

Every table here has one shape: the core's field, call or error first, the NFS
field or status after it, and, where it matters, a column for what is kept in
`Adapter` bytes rather than in a core field. A request reads a row right to
left, a result left to right. Each mapping is a pure function of the request
and, where named, the record's `Adapter` bytes, and each is total: a wire value
maps to one core value or to a named refusal, never to a default (N26).

### 11.1 Identity

Each credential becomes RFC 17's neutral `Identity`
([RFC 17 §3.0](rfc-17-vfs.md#3.0%20Value%20types)), which names no flavour and
no Kerberos level:

| `Proof` | `Protection` | `Evidence.Principal` | `Claims` | ← NFS credential |
| --- | --- | --- | --- | --- |
| `None` | `None` | zero | none | `AUTH_NONE` |
| `Claimed` | `None` | zero | the uid, gid and groups it carried | `AUTH_SYS` |
| `Ticket` | `None` | the ticket's principal, from `Service.SignIn` | none | `RPCSEC_GSS` `krb5` |
| `Ticket` | `Integrity` | as `krb5` | none | `RPCSEC_GSS` `krb5i` |
| `Ticket` | `Privacy` | as `krb5` | none | `RPCSEC_GSS` `krb5p` |

The identity also carries the connection's source address, which client rules
are checked against.

- **Refused credentials.** A credential that does not decode, an `AUTH_SYS`
  credential over RFC 5531's limits — more than 16 gids, a machine name over
  255 bytes — and every flavour other than `AUTH_NONE`, `AUTH_SYS` and
  `RPCSEC_GSS` **MUST** be rejected `AUTH_BADCRED` and run nothing. None is ever
  treated as anonymous: that would turn a malformed credential into the share's
  everyone grant.
- **Root.** A claimed uid 0 maps to RFC 18's root principal
  ([RFC 18 §2.1](rfc-18-identity.md#2.1%20Kinds%20and%20well-known%20principals)).
- **Export flavour policy.** "Requires Kerberos" is the share's `MinProof`
  `Ticket`, and a minimum Kerberos level is the matching `MinProtection`;
  admission compares both with the identity
  ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)), and a
  refusal is `ErrWrongSecurity` (§11.5), answered on the wire as §10 says.
- **Names.** `owner` and `owner_group` strings, uids and gids are spelled
  through `Service.Spell` and parsed through `Service.Find`
  ([RFC 18 §4.3](rfc-18-identity.md#4.3%20uid%20and%20gid),
  [§4.5](rfc-18-identity.md#4.5%20NFSv4%20owner%20strings)); the adapter keeps
  no map of its own.
- **The credential's group.** An `AUTH_SYS` credential's `gid` is the group
  RFC 7 gives a new file
  ([RFC 7 §2.4](rfc-7-namespace-metadata.md#2.4%20Attributes%2C%20and%20who%20writes%20them)),
  resolved through `Find`; a gid no principal holds counts as none.
  `AUTH_NONE` and `RPCSEC_GSS` carry no group on the wire, so the principal's
  primary group applies.
- **What a claim reaches.** A `Claimed` or `None` caller is never
  `authenticated` and never `admin`
  ([RFC 18 §6.5](rfc-18-identity.md#6.5%20What%20the%20proof%20proved%3A%20authenticated%2C%20admin%2C%20privileged));
  the adapter only reports the proof, and what that means for an ACL is
  [RFC 19 §2.3](rfc-19-authorization.md#2.3%20Principals)'s. Which NFS callers
  are privileged is §8.1.
- **GSS contexts** stay on the gateway node (§10); nothing of them reaches the
  core but the `Identity` `SignIn` returned.

### 11.2 Request identity

The request ID a forwarded call carries
([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)), which the
service's dedup keys on, is derived from the NFS retry identity, never from
the connection, because an NFSv4 client retries only on a new connection
(RFC 7530 §3.1.1) and an NFSv3 one may:

| Request ID | ← NFS retry identity |
| --- | --- |
| tagged NFSv3 or NFSv4.0 | (client address, XID, procedure, checksum of the arguments), as a server's reply cache keys a retry |
| tagged NFSv4.1 | (gateway node's ID, the node epoch the session was created under, session, slot, sequence ID) |

Each gateway node mints its own session IDs and nothing makes them unique
across gateway nodes, hence the node in the NFSv4.1 row. The epoch is the
session's, not the gateway node's current one: a gateway node marked lapsed
re-acquires a higher node epoch without restarting, its sessions survive, and a
retry on the same session, slot and sequence must still carry the ID its first
send did. So an NFSv3 or NFSv4.0 retry carries the same ID on any connection
and through any gateway node that holds the client's address, and an NFSv4.1
retry on any connection of its session, which lives at one gateway node
([RFC 11 §5.1](rfc-11-ownership.md#5.1%20Gateways%20forward%20to%20the%20primary)).

> decision: an NFSv4.1 retry after its session was lost is a new request. The
> session — its slot table and replay cache — lives at the gateway node and dies
> with it: the client finds the session gone, creates a new one, and resends
> under a new slot and so a new request ID. A non-idempotent call whose reply was
> lost can then be applied twice, as after a restart of a server without
> persistent sessions, which NFS clients already tolerate; an exclusive create
> stays safe by its stored verifier
> ([RFC 7 §2.10](rfc-7-namespace-metadata.md#2.10%20Exclusive%20create)). NFSv3 and NFSv4.0 retries keep their identity
> across a gateway node loss, since it names no session. Overturned if re-applied
> non-idempotent calls across a gateway node loss show up in client-visible errors;
> then NFSv4.1 sessions are persisted in the store with their replay caches.

### 11.3 Namespace

[RFC 7](rfc-7-namespace-metadata.md) is protocol-neutral: its records, rules and
errors name no NFS field. The NFSv4 attribute list is §8 and
[Appendix B](#Appendix%20B%20%E2%80%94%20attributes); the table below adds what
NFSv3 carries, and the rules after it bind both versions. Its errors are
§11.5's.

| RFC 7 field | NFSv3 (`fattr3`, `sattr3`) | NFSv4 |
| --- | --- | --- |
| `Type` | `ftype3`; a `Stream` is never reached (§8.2) | `type`, likewise |
| `Mode` | `mode` | `mode`, `mode_set_masked` |
| `Owner`, `Group` | `uid`, `gid`, through [RFC 18 §4.3](rfc-18-identity.md#4.3%20uid%20and%20gid) | `owner`, `owner_group`, through [RFC 18 §4.5](rfc-18-identity.md#4.5%20NFSv4%20owner%20strings) |
| `Nlink`; a directory's `Subdirs` | `nlink`: a file's `Nlink`, a directory's 2 + `Subdirs` and never below 2 | `numlinks`, likewise |
| `Size`, `Charged` | `size`, `used` | `size`, `space_used` |
| `Device` | `rdev` | `rawdev` |
| `Number` | `fileid` | `fileid` |
| the share's filesystem identity | `fsid`, derived from the `ShareID` | `fsid`, likewise |
| `Access`, `Modify`, `Change` | `atime`, `mtime`, `ctime` | `time_access`, `time_modify`, `time_metadata` |
| `Birth` | none | `time_create` |
| `Version` | none: NFSv3 clients validate by `ctime` and `mtime` | `change` |
| the stored markers hidden, system, archive | none | `hidden`, `system`, `archive` |
| the stored markers read-only, sparse | none | none |
| `CreateVerifier` | `createverf3` of an `EXCLUSIVE` `CREATE` | `verifier4` of an `EXCLUSIVE4` or `EXCLUSIVE4_1` `OPEN` |

- **Times.** `SET_TO_SERVER_TIME` and `SET_TO_SERVER_TIME4` set a server-chosen
  time, clamped as [RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward) says. `SET_TO_CLIENT_TIME` and
  `SET_TO_CLIENT_TIME4` are a client-given time, stored as given. NFS sets
  times on no open, so a client-given time needs ownership, an ACL's write
  attributes or privilege
  ([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)).
- **The read-only marker has no NFS spelling.** NFSv3 has no attribute for it
  and NFSv4 defines none beside `archive`, `hidden` and `system`, so an NFS
  client can neither see the marker nor clear it. It is still enforced
  ([RFC 7 §7.9](rfc-7-namespace-metadata.md#7.9%20The%20read-only%20marker%20refuses%20writes%20and%20deletes)):
  a `WRITE`, a `SETATTR` of `size`, an `OPEN` for write and a `REMOVE` or a
  `RENAME` over a file an SMB client marked read-only are `NFS4ERR_ACCESS` or
  `NFS3ERR_ACCES`, and `ACCESS` reports no `MODIFY` or `EXTEND` for it.
- **Exclusive create.** The verifier of an NFSv3 `EXCLUSIVE` create and of an
  NFSv4 `EXCLUSIVE4` or `EXCLUSIVE4_1` open is RFC 7's create verifier
  ([RFC 7 §2.10](rfc-7-namespace-metadata.md#2.10%20Exclusive%20create)), stored in `CreateVerifier` and never in `atime` or
  `mtime`. The `SETATTR` a client sends after an `EXCLUSIVE` or `EXCLUSIVE4`
  create is the first `SetAttrs`, which clears it. The attributes an
  `EXCLUSIVE4_1` open carries (`suppattr_exclcreat`, Appendix B) are applied by
  the create's own transaction, not by a `SetAttrs`, and leave the verifier in
  place.
- **umask.** NFSv4.2's `mode_umask` ([RFC 8275](https://www.rfc-editor.org/rfc/rfc8275.html)) is the umask RFC 7 says a
  create carries apart from the mode: ignored when the parent holds
  inheritable entries, applied to the mode when it holds none (Appendix B).
- **The ACL rules RFC 19 states are RFC 8881's.** The ordered walk is
  [RFC 8881 §6.2.1](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.2.1)'s
  ([RFC 19 §3](rfc-19-authorization.md#3.%20Evaluation)); removing an entry with
  delete on the file or delete a child on its directory is §6.2.1.3.2's; mode and
  ACL agree as §6.4 requires, the `chmod` merge is §6.4.1.1's, and inheritance at
  create is §6.4.3's ([RFC 19 §4](rfc-19-authorization.md#4.%20Mode%20and%20ACL)).

**ACLs.** `acl`, `dacl` and `sacl` spell RFC 7's one stored ACL through the
translation both adapters share
([RFC 19 §5.1](rfc-19-authorization.md#5.1%20One%20translation%2C%20two%20spellings)),
which changes spellings and bit positions only.

| Attribute | Versions | Holds | To read | To write |
| --- | --- | --- | --- | --- |
| `acl` | 4.0, 4.1, 4.2 | the access list, then the audit list; no flags | read the ACL, and the system-security right for the audit entries | write the ACL, and the system-security right for the audit entries |
| `dacl` | 4.1, 4.2 | the access list and its flags | read the ACL | write the ACL |
| `sacl` | 4.1, 4.2 | the audit list and its flags | the system-security right | the system-security right, and write the ACL in the call's ceiling |
| `aclsupport` | all | allow, deny, audit and alarm | — | — |

The system-security right is held only by a caller whose `admin` flag is set
([RFC 19 §3.5](rfc-19-authorization.md#3.5%20Privilege%20and%20the%20system-security%20right));
reading the audit entries tells a user what is watched, and changing them turns
auditing off, so both protocols gate them alike
([RFC 22 §11](rfc-22-smb.md#11.%20Security%20descriptors)).

- **Audit entries.** A `GETATTR` or `SETATTR` of `sacl` without the
  system-security right is `NFS4ERR_ACCESS`. A `GETATTR` of `acl` without it
  returns the access list alone; a `SETATTR` of `acl` without it that holds an
  audit or alarm entry is `NFS4ERR_ACCESS`, and one that holds none leaves the
  stored audit list as it was.
- **Flags.** `dacl` and `sacl` carry each list's flags in `aclflag4`: protected
  `ACL4_PROTECTED`, auto-inherited `ACL4_AUTO_INHERIT`, defaulted
  `ACL4_DEFAULTED`. A `SETATTR` of `acl` leaves both lists' flags as stored,
  since `acl` carries none. A `SETATTR` naming `acl` with `dacl` or `sacl` is
  refused: two values for one list.
- **At create.** An `acl` or `dacl` in an `OPEN` or `CREATE`'s attributes sets
  the new file's access list only; the audit list is the inherited one unless
  the caller holds the system-security right and sends `sacl` or audit entries
  ([RFC 19 §4.4](rfc-19-authorization.md#4.4%20Inheritance)).
- **With `mode`.** A `SETATTR` naming `mode` and `acl` (or `dacl`) sets both: the
  ACL is stored as sent and only setuid, setgid and sticky are taken from the
  mode ([RFC 19 §4.3](rfc-19-authorization.md#4.3%20Setting%20mode%20and%20ACL)),
  since merging a mode into the ACL the client sent would change it.
  `mode_set_masked` changes only the bits its mask names, as a `chmod` of those
  bits.
- **Entries.** `ACE4_ACCESS_ALLOWED_ACE_TYPE`, `ACE4_ACCESS_DENIED_ACE_TYPE`,
  `ACE4_SYSTEM_AUDIT_ACE_TYPE` and `ACE4_SYSTEM_ALARM_ACE_TYPE` are RFC 19's four
  kinds; any other type is refused. Each right is the NFSv4 right of the same
  bit: `READ_DATA` (`LIST_DIRECTORY`), `WRITE_DATA` (`ADD_FILE`), `APPEND_DATA`
  (`ADD_SUBDIRECTORY`), `READ_NAMED_ATTRS`, `WRITE_NAMED_ATTRS`, `EXECUTE`,
  `DELETE_CHILD`, `READ_ATTRIBUTES`, `WRITE_ATTRIBUTES`, `DELETE`, `READ_ACL`,
  `WRITE_ACL`, `WRITE_OWNER`, `SYNCHRONIZE`. Order is kept as sent
  ([RFC 19 §2.7](rfc-19-authorization.md#2.7%20Order)).
- **Entry flags** map by name
  ([RFC 19 §2.5](rfc-19-authorization.md#2.5%20Entry%20flags)), never byte for
  byte, since three sit at bits SMB uses for others:

  | Flag | NFSv4 bit |
  | --- | --- |
  | file inherit | `0x01` |
  | directory inherit | `0x02` |
  | no propagate | `0x04` |
  | inherit only | `0x08` |
  | successful access | `0x10` |
  | failed access | `0x20` |
  | inherited | `0x80` |

- **The group flag** (`ACE4_IDENTIFIER_GROUP`, `0x40`) is not a flag of the
  model: it is how NFSv4 spells the principal's kind. It is set on read exactly
  when the principal is a group or `GROUP@`, and on write selects the namespace a
  `who` is resolved in. It is not stored.
- **Owner pairs.** An effective `OWNER@` or `GROUP@` entry with no inherit flags
  directly followed by the CREATOR OWNER or CREATOR GROUP inherit-only entry that
  RFC 22 §11 would fold with it is folded as RFC 22 folds it, so no stored ACL
  holds an unfolded pair whichever protocol wrote it.

**`who`.** Every principal an entry can name has a spelling
([RFC 19 §5.2](rfc-19-authorization.md#5.2%20Principals%20on%20the%20wire)):

| Principal | `who` |
| --- | --- |
| owner, group, everyone | `OWNER@`, `GROUP@`, `EVERYONE@` |
| authenticated users, network, anonymous | `AUTHENTICATED@`, `NETWORK@`, `ANONYMOUS@` |
| root | `0` on every call: root has no record name for an owner string |
| owner rights, built-in users, administrators, system, creator owner, creator group | the SID's string form at the domain of [RFC 18 §4.5](rfc-18-identity.md#4.5%20NFSv4%20owner%20strings), such as `S-1-3-4@example.com` |
| a user or group | RFC 18 §4.5's owner string for the call's proof kind — `name@domain` on a proven caller, the decimal uid or gid on a claimed one — with the group flag for a group; a principal that has a SID and no owner string is spelled by its SID, as above |
| a deleted principal | the null SID, `S-1-0-0@domain` |

On a set, a `who` of the form `S-…@domain` with a well-formed SID resolves by
that SID; any other through RFC 18 §4.5's rules, in the user or group namespace
the group flag says. NFSv4's other special names — `INTERACTIVE@`, `BATCH@`,
`DIALUP@`, `SERVICE@` — are well-known principals with no row in
[RFC 18 §2.1](rfc-18-identity.md#2.1%20Kinds%20and%20well-known%20principals)'s
table, which RFC 19 §2.8 decides.

**Refusals.** A set RFC 19 refuses is refused whole and nothing is stored
([RFC 19 §2.8](rfc-19-authorization.md#2.8%20What%20a%20stored%20ACL%20may%20hold)):
a `who` no principal holds is `NFS4ERR_BADOWNER`; every other refusal — an entry
type other than the four, a list over the limit, a deny naming a well-known
principal that matches no caller, `acl` with `dacl` or `sacl` — is
`NFS4ERR_INVAL`.

> decision: a principal NFS has no owner string for is spelled by its SID rather
> than left out. Leaving it out makes every read-modify-write from an NFS client
> a silent delete of that entry, a deny included. A `who` that is a SID is legal
> UTF-8 an NFS tool passes through unread. Overturned if a widely used NFS client
> is shown to refuse or mangle such a `who`; the fallback then is to refuse the
> NFS ACL write of a file holding an unspellable entry, never to drop it.

- **Listings.** `READDIR` is `Entries`, `READDIRPLUS` and an NFSv4 `READDIR`
  asking for attributes are `EntriesPlus`; cookies and the verifier are §7.2.
- **Change info and weak cache consistency.** `change_info4`'s `before` and
  `after` are the directory's `Version` read just before and just after the
  operation, and `atomic` is always false; NFSv3 `wcc_data` omits `before` for
  a directory always, and for a file unless it was captured under the file's
  accept lock
  ([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps)).
- **Delete pending.** NFS cannot set it, but meets it: an `OPEN` of a
  delete-pending file, and a `CREATE`, `LINK` or `RENAME` into a delete-pending
  directory, are `ErrDeletePending`
  ([RFC 7 §8.2](rfc-7-namespace-metadata.md#8.2%20A%20delete%20on%20close%20is%20an%20ordinary%20unlink%2C%20later)), answered as an access error (§11.5): NFS has no
  delete-pending status, and a client told "no such entry" would create again.

### 11.4 Open state

Open state takes only neutral requests and returns neutral results and errors
([RFC 14 §1.1](rfc-14-open-state.md#1.1%20Non-goals)). Every NFS → neutral
mapping it needs is here.

**Clients.** Every node answers `EXCHANGE_ID`, `SETCLIENTID` and
`DESTROY_CLIENTID` for any client (§3.3), so each field those calls decide is
in RFC 14's durable client record
([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)):

| RFC 14 field | ← NFS field | Used by |
| --- | --- | --- |
| `Name`, indexed by (protocol, name) | the owner string: `co_ownerid` (`EXCHANGE_ID`), the `nfs_client_id4` id (`SETCLIENTID`); for an NLM host its `caller_name` with the source address it sends from (§6.1) | an `EXCHANGE_ID` / `SETCLIENTID` from a client that rebooted or reconnected through another node finds its record, and `Boot` tells which |
| `Boot` | the boot verifier; for an NLM host its NSM state number, big-endian in the low four bytes, so the bytes order as the numbers do | a new `Boot` under the same name is a new client instance |
| `Pending` | an unconfirmed `SETCLIENTID` or `EXCHANGE_ID`: nothing durable, held at the node that answered, `Confirm` sent as the NFSv4.0 confirm verifier or, low 32 bits, as the NFSv4.1 `eir_sequenceid` ([RFC 14 §2.9](rfc-14-open-state.md#2.9%20Clients%20pending%20confirmation)) | `SETCLIENTID_CONFIRM`, or NFSv4.1's first `CREATE_SESSION`, at that node (§3.5) |
| `Owner` | the principal that created the client ID, mapped before squashing; zero for an NLM host | an `EXCHANGE_ID` or `SETCLIENTID` for the same name from another principal is `NFS4ERR_CLID_INUSE` (RFC 8881 §18.35.5) and leaves no `Pending` |
| `Protect` | set for `SP4_MACH_CRED`, false for `SP4_NONE`; `spo_must_allow` stays in the adapter's bytes, and the adapter refuses a machine-credential call outside it before calling the service | each node admits the machine principal the same way |
| `Reach.Callback`, `Reach.Down` | the node holding the back channel, and whether it is lost (§3.4) | callbacks, `SEQ4_STATUS_CB_PATH_DOWN` |
| `Reach.Sessions` | one entry per node holding a session, added at `CREATE_SESSION` through `AddSession`, removed at the node's last `DESTROY_SESSION` or with the node's loss | `DESTROY_CLIENTID` is `NFS4ERR_CLIENTID_BUSY` while any entry remains (§5.1) |
| `Lease.Node` | the lease owner: the node holding the client's sessions | renewal by `SEQUENCE` and `RENEW` |
| `Shards` and the record's per-shard `Done` | the shards it holds state in, and in each the grace instance it last finished reclaiming in; a loss awaits the client's recovery while that is below the shard's grace instance ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)) | §5.4 |

**What the NFS adapter keeps in `Adapter`.** The record's `Adapter` bytes are
the NFS adapter's own; open state stores them and never reads them. They start
with a version byte, and hold:

- the minor version that confirmed the record, 0 or 1, which a proposal under
  the other one treats as a reboot (§3.5);
- the one-slot `CREATE_SESSION` replay: the `csa_sequence` of the last one
  applied and its reply (§3.5);
- `SP4_MACH_CRED`'s operation sets as sent, `spo_must_enforce` and
  `spo_must_allow`, which the adapter checks before it calls the service: an
  operation in `spo_must_enforce` from any principal but `Owner` is
  `NFS4ERR_ACCESS`;
- for NFSv4.0, the callback address and program (`cb_client4`,
  `callback_ident`), taken from the `Pending` proposal's `Adapter` by the
  confirm (§4.1);
- for an NLM host, where its restart notice goes: `mon_name` and the `priv`
  cookie (§6.2).

A caching grant carries no `Adapter` bytes for NFS.

The `Reach.Sessions` row is cluster-only: on one node the list is local. Without it,
a node asked to destroy a client would see none of the client's sessions on
other nodes and release state they still use. The `Shards` row binds on one
node too, and **MUST** be durable there: every restart opens a new grace
instance (§5.4), and the record of which instance a client completed is what
keeps a `RECLAIM_COMPLETE` sent before the restart from ending the grace after
it.

The lease's expiry time (`Client.Expires`) is not among these: it is volatile, held by
the client's lease owner ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)),
so a renewal — every `SEQUENCE`, every NFSv4.0 `RENEW` — writes no record.

**Opens, grants, locks and their calls.**

| RFC 14 field or call | ← NFS | Kept in `Adapter` bytes | Where |
| --- | --- | --- | --- |
| `Client.Pending`, `Pending.Confirm` | `SETCLIENTID` / `EXCHANGE_ID` before its confirm; the confirm verifier, the `eir_sequenceid` | the NFSv4.0 callback address, until the confirm takes it | §3.5, §4.1 |
| `AddSession` | `CREATE_SESSION` | its sequence and last reply, which answer a replay | §3.5 |
| `StateRef{Type, Shard, Instance, Token, Seq}` | stateid: `other` (shard, grace-instance bits, token), `seqid`; `seqid` 0 is `Seq` 0, current | — | §5.2 |
| `OpenMode.Access` / `Deny` | `share_access` / `share_deny`: read and write each way; NFSv4 has no delete bit, so an NFS open never denies delete | — | below |
| an open with `Via` zero: counted under no directory, no delete through a parent ([RFC 17 §4.6](rfc-17-vfs.md#4.6%20One%20chokepoint)); `Reclaim` for `CLAIM_PREVIOUS` | `OPEN` claim types `CLAIM_FH`, `CLAIM_PREVIOUS`, `CLAIM_DELEG_CUR_FH`, `CLAIM_DELEG_PREV_FH` | — | below |
| grant kind read; read + write; never handle | read delegation, write delegation | — | §5.3 |
| `LockReq` of one range; `Wait` poll for `READW_LT` and `WRITEW_LT`, callback for a blocking NLM lock | `LOCK` / `LOCKU` / `LOCKT`, NLM `LOCK` / `UNLOCK` / `TEST` | — | §5.5, §6.1 |
| `LockOwner.Name`; `LockOwner.Stack` false | the `lock_owner4`, or the NLM `svid` + `oh`; NFS locks merge and split | — | §5.5, §6.1 |
| `Seqs` on the call, held as RFC 14's `Sequence`; which replies advance it | NFSv4.0 open-owner and lock-owner seqids | — | §4.2 |
| `AckRecall` to none | `DELEGRETURN` | — | Appendix D |
| `ReclaimComplete`: the client has finished reclaiming | `RECLAIM_COMPLETE` | — | §5.4 |
| `Reach.Down` | `SEQ4_STATUS_CB_PATH_DOWN` on the next `SEQUENCE` | — | §3.4 |
| `ClientStatus` flags `ReclaimNeeded`, `AdminRevoked`, `RecallableRevoked` | `SEQ4_STATUS_RESTART_RECLAIM_NEEDED`, `SEQ4_STATUS_ADMIN_STATE_REVOKED`, `SEQ4_STATUS_RECALLABLE_STATE_REVOKED`, each until `RECLAIM_COMPLETE` or `FREE_STATEID` clears its cause; for NFSv4.0, a needed reclaim is `NFS4ERR_STALE_CLIENTID` to `RENEW` and to every operation naming the client ID until `SETCLIENTID` and its confirm, while the lost shard's stateids are `NFS4ERR_STALE_STATEID` | — | §5.1, §5.4 |
| `ErrBlocked`, then `LockWaitOver` | a lock that waits, granted later: `NLMPROC4_GRANTED` resent until the host answers; an NFSv4.1 waiter may be sent `CB_NOTIFY_LOCK`, which is optional | the NLM host's restart-notice cookie | §5.5, §6.1 |
| every neutral error | its NFS status | — | §11.5 |

**Access and deny.** `OPEN4_SHARE_ACCESS_READ` and `_WRITE` are `Access` read
and write data (with the attribute reads and writes RFC 7 §7.7 implies);
`OPEN4_SHARE_DENY_READ` and `_WRITE` are `Deny` of the same bits. The
`OPEN4_SHARE_ACCESS_WANT_*` flags are not access: they ask for a grant (§5.3).
An access or deny value outside the protocol's set is `NFS4ERR_INVAL`, never
truncated to the bits that fit.

**Topology on the wire.** A `LAYOUTCOMMIT` on a layout whose (shard, epoch) is
stale ([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS)) is `NFS4ERR_BADLAYOUT`; a
single node answers every layout operation with `NFS4ERR_NOTSUPP` (§3.3). A
drained node answers with `fs_locations` naming another gateway node (§7.4).

**What crosses between nodes** is [RFC 17 §2.1](rfc-17-vfs.md#2.1%20What%20runs%20where)'s:
neutral calls and the opaque `Adapter` bytes. Slot tables, reply caches and GSS
contexts stay volatile on the gateway (§3.2, §10). The bytes' first byte is the
adapter's own version, so a node that cannot read a version refuses the request
rather than misreading it; the adapter reads the record through the service on
every decision that needs it and never answers from a copy it holds.

### 11.5 Errors

[RFC 20 §4.5](rfc-20-adapters.md#4.5%20One%20error%20table%20per%20protocol)
requires one total table per protocol; this is NFS's, for every neutral error
the service returns — RFC 7's
([RFC 7 §11.3](rfc-7-namespace-metadata.md#11.3%20Neutral%20errors)), RFC 14's
and RFC 17's ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)).
Which answer makes a client retry, reclaim or give up is the protocol's choice,
so the rows say it. NLM codes are for NFSv3 locking (§6).

| Service error | NFSv4 | NFSv3 / NLM |
| --- | --- | --- |
| `ErrNotFound` | `NFS4ERR_NOENT` | `NFS3ERR_NOENT` |
| `ErrExist` | `NFS4ERR_EXIST` | `NFS3ERR_EXIST` |
| `ErrNotEmpty` | `NFS4ERR_NOTEMPTY` | `NFS3ERR_NOTEMPTY` |
| `ErrNotDir`, `ErrIsDir` | `NFS4ERR_NOTDIR`, `NFS4ERR_ISDIR` | `NFS3ERR_NOTDIR`, `NFS3ERR_ISDIR` |
| `ErrAccess`, the read-only marker's refusals included, a delete's `ReadOnly` cause too ([RFC 7 §7.9](rfc-7-namespace-metadata.md#7.9%20The%20read-only%20marker%20refuses%20writes%20and%20deletes)) | `NFS4ERR_ACCESS` | `NFS3ERR_ACCES` |
| `ErrWrongShare` — a handle of another share than the one asked | `NFS4ERR_ACCESS` | `NFS3ERR_ACCES` |
| `ErrCross` (`CrossShare`) — a `RENAME` or `LINK` between shares | `NFS4ERR_XDEV` | `NFS3ERR_XDEV` |
| `ErrDelay` — a recall or break in progress, a write held behind a clone past its own deadline or past what its session lets wait (`DelayClone`, §9.3), a share frozen for a move (§7.4), any transient refusal [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s one table classes as retry-later, a stability reply waiting on a stalled store (§9.1), or a store conflict still unresolved at the caller's deadline (`DelayConflict`, [RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `NFS4ERR_DELAY`, for as long as the cause lasts | `NFS3ERR_JUKEBOX`; NLM: `NLM4_DENIED_GRACE_PERIOD` |
| `ErrDelay` with `DelayResources` — a node memory pool spent: the share's cap on the request buffers, open state, or a reply reservation's deadline ([RFC 24 §4.2](rfc-24-resources.md#4.2%20Across%20shares%3A%20the%20share%20cap%20at%20admission)) | `NFS4ERR_DELAY`, at once; never held | `NFS3ERR_JUKEBOX`; NLM: `NLM4_DENIED_GRACE_PERIOD`, which the Linux lock manager waits on and resends, where `NLM4_DENIED_NOLOCKS` reaches the application as `ENOLCK` with no retry |
| `ErrBlocked` — a blocking NLM lock left waiting (§6.1) | — | NLM: `NLM4_BLOCKED` |
| `ErrStale` — a released file's handle | `NFS4ERR_STALE` | `NFS3ERR_STALE` |
| `ErrStale` — the share is being removed or retired ([RFC 16 §2.3.2](rfc-16-metadata-store.md#2.3.2%20Removing%20a%20share)) | `NFS4ERR_STALE` for every handle into it; a pseudo-filesystem `LOOKUP` of its path, gone from the pseudo-filesystem, is `NFS4ERR_NOENT`; a stateid of the share in an operation with no handle (`TEST_STATEID`, `FREE_STATEID`) is `NFS4ERR_BAD_STATEID`, and `SEQUENCE` sets no revoked-state flag for it | `NFS3ERR_STALE`; `MOUNT` of its path is `MNT3ERR_NOENT`; NLM: `NLM4_STALE_FH` |
| `ErrNoSpace` — a permanent capacity refusal, as [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s one table classes it | `NFS4ERR_NOSPC`, at once | `NFS3ERR_NOSPC` |
| `ErrIncomplete` — a read, through a snapshot or a copy made from one, of a file the snapshot's cut lists as incomplete ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)), or a `COPY` or `CLONE` from such a file, refused before anything changes ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)) | `NFS4ERR_IO` | `NFS3ERR_IO` |
| `ErrQuota` — a logical quota, its scope named ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `NFS4ERR_NOSPC` for a share-wide or project quota, `NFS4ERR_DQUOT` for a user or group one | `NFS3ERR_NOSPC` and `NFS3ERR_DQUOT` likewise |
| `ErrInvalid` — a name RFC 7 refuses (§7.3); a rename into its own descendant; a `COPY` or `CLONE` of overlapping ranges of one file, or a `CLONE` longer than `clone_max_len` (§9.3) | `NFS4ERR_BADNAME` for the name; `NFS4ERR_INVAL` otherwise | `NFS3ERR_INVAL` |
| `ErrReadOnly` — a mutation where only reading is admitted ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `NFS4ERR_ROFS` | `NFS3ERR_ROFS` |
| `ErrNameTooLong` — a name past the share's length limit | `NFS4ERR_NAMETOOLONG` | `NFS3ERR_NAMETOOLONG` |
| `ErrCross` (`CrossNamespace`) — a `COPY` or `CLONE` between shares in different namespaces (§9.3) | `NFS4ERR_XDEV` | — |
| `ErrCross` (`OtherShard`) — **(cluster)** a `CLONE` whose source and destination are in different shards, whatever their primaries, refused at admission ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `COPY`: never reaches the adapter; the service copies the bytes server-side, reading at the source and writing at the destination, and the adapter answers success with the count ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)). `CLONE`: `NFS4ERR_XDEV`, and the client copies. Never `NFS4ERR_NOTSUPP`, which Linux records as the operation missing for the whole mount | — |
| `ErrInconsistent` — a store invariant the server's own state breaks, such as a constraint or a file's link count below zero, never a directory's subdirectory count, which RFC 7 recounts and reads as 0 meanwhile (App. B) ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)); the service raises a health condition | `NFS4ERR_IO` | `NFS3ERR_IO` |
| `ErrCross` (`CrossProject`) — a `RENAME` or `LINK` across tree quotas ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `NFS4ERR_XDEV`; `mv` copies and deletes instead | `NFS3ERR_XDEV` |
| `ErrState` (`ClientInUse`) | `NFS4ERR_CLID_INUSE` | — |
| `ErrState` (`RecordChanged`) — another node wrote the client record's adapter bytes first | never sent: the adapter rereads the record and decides again (§3.5); a `CREATE_SESSION` it then finds misordered is its own `NFS4ERR_SEQ_MISORDERED` | — |
| `ErrLost` on `Commit` — a loss of unstable writes the client was answered for | the `COMMIT` reply, with the new verifier (§9.1) | the same |
| `ErrLost` on a stable `Write` — the sync that write waited on failed (§9.1) | `NFS4ERR_IO` | `NFS3ERR_IO` |
| `ErrGrace` | `NFS4ERR_GRACE` | `NFS3ERR_JUKEBOX`; NLM: `NLM4_DENIED_GRACE_PERIOD` |
| `ErrState` (`NoReclaim`) | `NFS4ERR_RECLAIM_BAD` in grace, `NFS4ERR_NO_GRACE` outside it | NLM: `NLM4_DENIED_GRACE_PERIOD` in grace, else `NLM4_DENIED` |
| `ErrState` (`ReclaimConflict`) — a reclaim that conflicts with state already reclaimed in the same grace ([RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe)) | `NFS4ERR_RECLAIM_CONFLICT` | NLM: `NLM4_DENIED` |
| `ErrState` (`StaleClient`) — a client that expired, or is marked by a shard's loss ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `NFS4ERR_STALE_CLIENTID` for an operation naming a client ID, `RENEW` included, and for an NFSv4.0 client a loss awaits (§5.4). A stateid of an expired client is never answered `NFS4ERR_STALE_CLIENTID`, which is not a legal answer to `READ`, `WRITE` and the other stateid operations of RFC 8881: RFC 14 released its record with the client's other state, so it resolves to nothing and is `ErrState` (`BadState`), below. `NFS4ERR_EXPIRED` is never answered, since no expired client's record is kept (§5.1). A stateid of a live client whose state was lost is `ErrState` (`StaleState`), below | NLM: never `NLM4_DENIED`: the adapter connects the host afresh and retries once, then answers `NLM4_DENIED_GRACE_PERIOD` (§6.1) |
| `ErrState` (`Revoked`, `Admin` naming the cause) — a stateid in its client's revoked set, not yet freed, its client still live: a grant revoked at its recall deadline, or any state an administrator revoked (§5.1, [RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)); state lost with a shard stays `ErrState` (`StaleState`) | by the revocation's cause: `NFS4ERR_ADMIN_REVOKED` for any stateid an administrator revoked (§5.1); `NFS4ERR_DELEG_REVOKED` for a delegation's or layout's revoked at its recall deadline (§5.3) | — |
| `ErrState` (`BadState`) — a stateid whose record is gone, minted under the shard's current grace instance outside grace: an expired client's, one freed, a removed share's, or one never issued (§5.2 rule 1) | `NFS4ERR_BAD_STATEID` | — |
| `ErrState` (`ClientBusy`) — `Destroy` found the client still holding state (§5.1) | `NFS4ERR_CLIENTID_BUSY` | — |
| `ErrState` (`StaleState`) — a stateid whose state was lost with its shard's open state ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)): its record is gone and its bits name an earlier grace instance, or its shard is in grace (§5.2 rule 1) | `NFS4ERR_STALE_STATEID` (§5.4) | — |
| `ErrPerm` — an immutable or append-only file, or a change of owner, group or flags the caller may not make ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | `NFS4ERR_PERM` | `NFS3ERR_PERM` |
| `ErrUnavailable`, `ErrCorrupt`, `ErrLost` on a read ([RFC 17 §5.2](rfc-17-vfs.md#5.2%20Read)) | a short read of the verified bytes, then `NFS4ERR_DELAY` for `ErrUnavailable` and `NFS4ERR_IO` for the others | `NFS3ERR_JUKEBOX` for `ErrUnavailable`, `NFS3ERR_IO` for the others |
| `ErrState` (`NotYours`) | `NFS4ERR_ACCESS` for NFSv4.0 `READ`, `WRITE` and `SETATTR`, where only the principal is checked (§5.2); `NFS4ERR_BAD_STATEID` otherwise | — |
| `ErrWrongSecurity` — the share's policy does not admit the call's flavour ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)) | `NFS4ERR_WRONGSEC` | the RPC is rejected `AUTH_TOOWEAK` (§10) |
| `ErrState` (`BadSeqID`) | `NFS4ERR_BAD_SEQID` | — |
| `ErrState` (`LocksHeld`) — `RELEASE_LOCKOWNER` of an owner still holding locks, `FREE_STATEID` of a lock state holding locks or of a live open or delegation; nothing changed | `NFS4ERR_LOCKS_HELD` | — |
| `ErrLocked` | `NFS4ERR_DENIED` for `LOCK` / `LOCKT`; `NFS4ERR_LOCKED` for I/O | `NFS3ERR_ACCES`; NLM: `NLM4_DENIED` |
| `ErrRangeNotLocked` | never answered: an NFSv4 lock-owner's locks merge and split ([RFC 14 §2.3](rfc-14-open-state.md#2.3%20Lock)), so a `LOCKU` releases what the owner holds of its range and succeeds on a range it holds none of, as a POSIX unlock does. RFC 8881 §18.12.4 and RFC 7530 §16.12.5 let a server answer `NFS4ERR_LOCK_RANGE` there; this one does not, so a client never has to split its unlock into the ranges it holds | NLM: never answered; `NLMPROC4_UNLOCK` likewise succeeds, `NLMPROC4_GRANTED` |
| `ErrShareViolation` | `NFS4ERR_SHARE_DENIED` for `OPEN`; `NFS4ERR_LOCKED` for I/O; `NFS4ERR_FILE_OPEN` for a `REMOVE` or `RENAME` of a file an SMB open denies delete to (RFC 8881 §18.25.3) | `NFS3ERR_ACCES`, for I/O, `REMOVE` and `RENAME` alike; NLM: `NLM4_DENIED` for `NLMPROC4_SHARE` |
| `ErrDeletePending` | `NFS4ERR_ACCESS` | `NFS3ERR_ACCES` |
| `ErrState` (`OldState`) — a stateid seqid older than the state's ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)) | `NFS4ERR_OLD_STATEID` | — |

The adapter answers the capacity error the service returns and decides none of
them. A write refused by the journal's capacity or by the share's journal limit
is `ErrDelay` while offload or repack can drain it, and which refusals become
`ErrNoSpace`, and when, is [RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)'s
one table ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)).
**A transient refusal never becomes "no space" because a deadline ran out.** A
write still `ErrDelay` at the caller's deadline is answered `NFS4ERR_DELAY` /
`NFS3ERR_JUKEBOX`, the protocol's native retry-later, and the client retries for
as long as the outage lasts; only a permanent cause is `NFS4ERR_NOSPC`, at
once. Linux treats `ENOSPC` on a write as fatal and reports it at `fsync` or
close, so converting at the deadline would turn a two-minute remote outage into
lost writes.
A share's journal limit is a bound on local space, not a quota: answering it
"no space" at once would fail applications with `ENOSPC` on a share with room
in the remote tier, while offload clears the limit in seconds. Only a logical
quota (`ErrQuota`) is answered at once. A share-wide or project quota is
answered as no space: it is the volume the client mounted, `statfs` reports
its room ([RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem)), and an application meets it as a full disk, as
it meets an XFS project quota. A user or group quota is `DQUOT`, what
`rquota` reports against.

### 11.6 Testing

The translations are tested as RFC 20 §7 says, as pure functions with no
service, before any wire suite runs:

- **Golden vectors per table.** One golden file per table of this chapter —
  credentials (§11.1), retry identities (§11.2), attributes (§11.3), client
  fields, stateids, `share_access`/`share_deny` pairs, claim types, delegation
  types, `LOCK` types, `lock_owner4` and NLM `svid`/`oh` (§11.4), and every
  row of §11.5 for each of NFSv3, NFSv4.0, NFSv4.1 and NLM — asserts NFS →
  neutral → NFS gives the golden neutral value and back the golden wire value.
- **Every mapping is total.** Property tests over generated inputs: every NFS
  input maps to a neutral value or a named refusal, never a default; every
  neutral error in `vfs.Errors()` maps to an NFS status for each version
  (RFC 20 §7.3); where a mapping is invertible, the round trip is the identity.
- **Every decoder is fuzzed.** Go native fuzzing (`testing.F`) of the stateid ↔
  `StateRef` codec, `share_access`/`share_deny` decoding, `LOCK` and NLM
  lock-request decoding, owner-string parsing and the `Adapter`-bytes decoder,
  seeded with truncated, corrupt, older- and newer-version bytes: no panic, and
  either a valid neutral request or a refusal, never a request that decodes to
  other state.

| Requirement | Check | Fails without the rule |
| --- | --- | --- |
| §11.1–§11.5 golden vectors *(1 node)* | The golden files above, run as unit tests. | a field dropped or mapped differently by two handlers |
| §11.1–§11.5 total mapping *(1 node)* | The property tests above. | an input mapped to a default, or a neutral error with no NFS status |
| §11.4 decoders fuzzed *(1 node)* | The fuzz targets above, each with its seed corpus. | a decoder that panics, or reads another version's bytes as its own |
| §6.1 NLM against SMB *(1 node)* | An NLM lock and an SMB byte-range lock over one range of one file, in each order: assert the second refused, and an SMB write into the NLM range admitted (NFS locks are advisory) while an NFSv3 write into the SMB range is refused. | |
| §11.4 SMB deny-write against an NFS write open *(1 node)* | An SMB client opens a file with `FILE_SHARE_READ` only and no lease; an NFSv4 client `OPEN`s it with `OPEN4_SHARE_ACCESS_WRITE`, and an NFSv3 client `WRITE`s it: assert `NFS4ERR_SHARE_DENIED` and `NFS3ERR_ACCES`. Close the SMB open: assert both then succeed. Reverse it: an NFSv4 `OPEN` with `OPEN4_SHARE_DENY_WRITE`, then an SMB `CREATE` for write: assert `STATUS_SHARING_VIOLATION`. | a deny mode enforced within one protocol only |
| e2e | pynfs, cthon04 and nfstest through dt ([RFC 29](rfc-29-test-harness.md)) are the end-to-end layer over these rows and §13's. | |

### 11.7 Monitoring

The adapter's metrics and request span are
[RFC 20 §10](rfc-20-adapters.md#10.%20Observability)'s, recorded in `core`
under [RFC 25](rfc-25-observability.md)'s conventions; these are their NFS
rows. `protocol` is `nfs3` or `nfs4`, with NLM, NSM and MOUNT under `nfs3`
([RFC 25 §2.4](rfc-25-observability.md#2.4%20Labels)). No share, client,
owner, file or principal label.

| Answers, for NFS | Metric | Type |
| --- | --- | --- |
| wire inputs refused because they map to no neutral value, by `reason`: the table that refused it — `identity`, `attribute`, `client`, `stateid`, `open`, `lock` | `adapter_translation_refused_total{protocol, reason}` | counter |
| neutral errors answered, by the neutral error, from the service's closed error set (§11.5) | `adapter_status_mapped_total{protocol, error}` | counter |
| neutral errors with no row in §11.5; any nonzero is a bug | `adapter_unmapped_errors_total{protocol}` | counter |
| `Adapter` bytes that failed to decode, by `reason` (`truncated`, `corrupt`, `unknown_version`); any nonzero is an alert | `adapter_record_decode_failures_total{protocol, reason}` | counter |
| `CREATE_SESSION` replays answered from the client record | `adapter_record_replays_total{protocol="nfs4"}` | counter |
| reply-cache hits: genuine retransmissions answered from a session's slot | `adapter_replay_hits_total{protocol="nfs4"}` | counter |

**The request span.** One per RPC, named `adapter.nfs3.<PROCEDURE>` or
`adapter.nfs4.COMPOUND`, with one child per operation of a compound
(`adapter.nfs4.<OP>`) and the service's span below it
([RFC 25 §6.1](rfc-25-observability.md#6.1%20Spans)). Its attributes are
closed sets: protocol, minor version, operation, NFS status, the neutral error
or result, request and reply bytes, and the stage that refused. Never a path,
a name, an owner string, a principal or a client address
([RFC 25 §5.4](rfc-25-observability.md#5.4%20What%20is%20never%20logged)).

## 12. Invariants

| # | Invariant |
| --- | --- |
| N1 | No NFS program is served over UDP, except NLM and NSM while RFC 20 §11's UDP question stands (§2.2). |
| N2 | Every gateway node returns the same `server_scope` and `so_major_id`, and its own `so_minor_id`. |
| N3 | Sessions, reply caches and GSS contexts live on one gateway node and are never stored; a session is never bound to connections on two nodes. |
| N4 | A stateid's `other` names an RFC 14 record, resolvable on any node; its `seqid` is held with the state at the primary. |
| N5 | An expired client's state is released at expiry; nothing is kept as a courtesy. |
| N6 | A client's latest back-channel registration receives every later callback. |
| N7 | `change` is `Version`: it moves on every write the primary accepts, never on an operation that changed nothing, and never backward. |
| N8 | A READDIR cookie is RFC 7's 63-bit digest of the entry, valid on every node; a resume returns the entries after it; no reply ends inside a collision chain; the cookie verifier is a constant of the share. |
| N9 | No referral or `fs_locations` names a server outside the installation. |
| N10 | Every NFS object the adapter maps names a record another RFC owns; the adapter keeps no durable state but the opaque `Adapter` bytes of those records, reached through the service. |
| N11 | A replayed request is answered from a whole cached reply or `NFS4ERR_RETRY_UNCACHED_REP`, never run twice on one session and never from part of a reply; a session caches at most 64 × 8 KiB. |
| N12 | A stateid's `other` holds at least 64 random bits; I/O under an open or lock stateid is served only to the open's principal; I/O under a delegation stateid to any principal of the holding client, authorised afresh. |
| N13 | A `RECLAIM_COMPLETE` closes reclaim for one (client, shard, grace instance); every failover or restart, a single node's included, opens a new instance, never a repeated one, and is signalled by `SEQ4_STATUS_RESTART_RECLAIM_NEEDED`, or to an NFSv4.0 client by `NFS4ERR_STALE_CLIENTID`. |
| N14 | A lock request from another client recalls a read or write delegation before the lock is decided, and meanwhile is answered retry or blocked, never denied; a delegation is never a handle grant. |
| N15 | An NLM host is its name and source address; NLM callbacks are calls to the host's NLM service, on a connection the server opens from the floating address the host used. |
| N16 | Audit entries are read or changed only by an administrator, on every minor version; the ctime NFS reports moves on every change. |
| N17 | A stability reply waits for the journal sync of its range and for the existence commit of every pending overwrite of committed content in it, never for an append's or hole fill's; a capacity refusal is answered as RFC 8 §10.2 classifies it, a transient one retry-later for as long as it lasts, never "no space" because a deadline passed; only a logical quota or a permanent cause is answered at once. |
| N18 | The owner string (`Name`), verifier (`Boot`), creating principal (`Owner`), state protection (`Protect`), back-channel node, session holders and, in the adapter's own bytes, the last `CREATE_SESSION` sequence and reply of a client are in its durable record; its lease expiry is not; an unconfirmed client is a `Pending` proposal, volatile at the node that answered; an NFSv4.0 owner's sequence and confirmation are in its `Sequence`. |
| N19 | A directory's `change_info4` is never reported atomic, and NFSv3 pre-operation attributes are returned only when read from the File and its overlay in the hold of the file's accept lock that accepts the operation, never for a directory. |
| N20 | A single node advertises no pNFS role and answers every layout operation `NFS4ERR_NOTSUPP`. |
| N21 | An NLM waiter ends only as RFC 14's lifetime ends it; no adapter timer drops it, and no cancel is answered before `CancelLock` returns; an `NLMPROC4_GRANTED` is resent until the host answers, and a granted lock is never released for a silence. |
| N22 | A `COPY` or `CLONE` of overlapping ranges of one file, or a `CLONE` over `clone_max_len`, is `NFS4ERR_INVAL` and changes nothing; one across namespaces is `NFS4ERR_XDEV`; a `CLONE` is atomic and answered when done; a `COPY` runs in bounded chunks and may answer a short count; I/O into a running clone's or chunk's ranges is held until it ends, and `NFS4ERR_DELAY` only past its own deadline. |
| N23 | A loss of unstable writes reaches the next `COMMIT` as a changed verifier, never as a success under the old one. |
| N24 | A stateid whose record is gone is `NFS4ERR_STALE_STATEID` when its bits name an earlier grace instance or its shard is in grace, and `NFS4ERR_BAD_STATEID` otherwise; an administrator's revoke keeps the client and its sessions, answers each revoked stateid `NFS4ERR_ADMIN_REVOKED` until it is freed, and outlives a restart, so the revoked state is never reclaimed. |
| N25 | A moved namespace's named address is served by nothing between A's release and its arrival at B, and the moved shares are in an NFS-only grace at B from publish as RFC 27 §2.2 bounds it. |
| N26 | Every NFS → neutral translation is the adapter's, total and pure: each wire value maps to one neutral value or a named refusal, every neutral error maps to one NFS status, and nothing the adapter cannot decode is acted on. Open state never reads a protocol value (§11). |
| N27 | The NFS adapter's durable state — the `CREATE_SESSION` replay, the callback and NSM addresses, the state-protection operation sets — lives only in the client record's versioned `Adapter` bytes, written in the step of the change it belongs to and over the bytes the adapter read; per-connection state is volatile on the gateway (§3.3, §3.5, §11.4). |


## 13. Conformance

Every check runs over the wire against one single node composed as in
production, as the index's protocol-suite tiers run
([Test harness](rfc-index.md#Test%20harness)); a check that names two nodes,
P1 and P2, a gateway node's loss or a shard failover between nodes is a
cluster check, deferred with the cluster
([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)), and
on a single node a failover is the node's restart. **Deferring a row defers
only its cross-node steps.** Every rule that binds a single node keeps a
single-node wire check: the rows marked *(1 node)* below run in the first
release, and a row's steps that name a second node run with the cluster. The protocol suites test
translation, and the service suite
([RFC 17 §8](rfc-17-vfs.md#8.%20Conformance)) tests behaviour. The
translation's own rows, cross-protocol ones included, are §11.6's, and run
with these.

The wire tests of [RFC 20 §8](rfc-20-adapters.md#8.%20Wire%20tests) compare this
adapter with a reference NFS server, field by field. This RFC owns the NFS
divergence list: each entry cites the rule here that makes DittoFS differ — no
UDP (§2.2), no courtesy clients (§5.1), no referrals (§7.4).

| Requirement | Check | Fails without the rule |
| --- | --- | --- |
| §2.2 TCP only | Probe NFS and MOUNT by UDP. Assert no reply; mount with `proto=udp` falls back or fails. While NLM and NSM are served over UDP (§2.2), assert they answer there. | a UDP listener left enabled |
| §3.2 reply cache | pynfs `nfs4.1` session and `SEQUENCE` replay tests; then kill the gateway node with a `REMOVE` in flight and assert the client sees `NFS4ERR_NOENT` on retry and recovers its session with no grace. | a session assumed durable, or a grace started on gateway node loss |
| §3.3 trunking | `EXCHANGE_ID` on two nodes: same `so_major_id` and scope, different `so_minor_id`; `BIND_CONN_TO_SESSION` across nodes is `NFS4ERR_BADSESSION`. | one scope per node, or a session bound across nodes |
| §3.4 callback routing | Hold a delegation, kill the client's gateway node, reconnect through another, conflict from a second client. Assert `CB_RECALL` arrives on the new connection. | callbacks registered once at first `Connect` |
| §3.4 path down | Block the back channel. Assert `SEQ4_STATUS_CB_PATH_DOWN` and no delegation offered. | |
| §4.2 v4.0 owners | pynfs `nfs4.0` open-owner and lock replay tests; then move the client to another node mid-sequence and assert the next sequenced `OPEN` is accepted. Kill the client's gateway node with a `CLOSE` in flight whose reply is lost; assert the retransmission through the node that took the address over is answered with the first result, the owner's next `OPEN` accepted, and no `NFS4ERR_BAD_SEQID`. Open with a new owner and, before `OPEN_CONFIRM`, send `READ` and `CLOSE` under it through another node; assert both refused and `OPEN_CONFIRM` then accepted. | seqids or confirmation held in the gateway node |
| §3.5 confirm | `SETCLIENTID` on P1, `SETCLIENTID_CONFIRM` on P2: assert `NFS4ERR_STALE_CLIENTID`; repeat both on P2 and assert confirmed. `SETCLIENTID` on P1 then the confirm on P1 from another principal: assert `NFS4ERR_CLID_INUSE`, and the confirm from the first principal then accepted. A client holding a lock reboots and sends `EXCHANGE_ID` with a new verifier but no `CREATE_SESSION`: assert its old client ID still holds the lock. `EXCHANGE_ID` for a confirmed owner from another principal: assert `NFS4ERR_CLID_INUSE`. Collect 10^4 confirm verifiers for one client ID and verifier: assert no two equal. | a confirm value derived from the client ID and verifier, or a client ID that acts before it is confirmed |
| §3.5 `CREATE_SESSION` sequence | Send `CREATE_SESSION`, drop the reply, move the address to P2 and resend it: assert the first reply and one session. Send one with `csa_sequence` two ahead: assert `NFS4ERR_SEQ_MISORDERED`. Restart the node between a `CREATE_SESSION` and its replay; assert the replay answered from the record. | `CREATE_SESSION`'s sequence held in one node's memory |
| §3.3 renewal writes nothing | Count metadata store writes while one client sends 10^4 `SEQUENCE`s with no other operation; assert none. | a durable `Expires` |
| §3.3 no pNFS on one node | On a single node: `EXCHANGE_ID` reply has `EXCHGID4_FLAG_USE_NON_PNFS` and neither pNFS flag; `supported_attrs` lists no `fs_layout_type`; `GETDEVICELIST`, `GETDEVICEINFO`, `LAYOUTGET`, `LAYOUTCOMMIT` and `LAYOUTRETURN` are each `NFS4ERR_NOTSUPP`. | a single node advertising itself a metadata server |
| §5.1 no courtesy | Let a client's lease expire with no conflict; assert its lock is gone and a new client takes it. | |
| §5.2 stateids | pynfs `TEST_STATEID`, `FREE_STATEID`, bad- and old-stateid tests; present another client's stateid and assert `NFS4ERR_BAD_STATEID`. Open, upgrade the open, then `READ` with the first stateid's `seqid` through another node; assert `NFS4ERR_OLD_STATEID`. | |
| §5.2 v4.0 stateids | Over NFSv4.0, `LOCK` with an open stateid of another client's open owner; assert `NFS4ERR_BAD_STATEID`. Under Kerberos, open a file as alice, then send `READ`, `WRITE` and `SETATTR` quoting her open stateid with mallory's credential; assert `NFS4ERR_ACCESS` for each and the file unchanged. | v4.0 I/O authorised by the stateid alone |
| §5.2 delegation stateid | Over NFSv4.1 under Kerberos, alice opens a file mode 0644 and gets a read delegation; from the same client, bob `READ`s under the delegation stateid: assert served. bob `WRITE`s under it: assert `NFS4ERR_ACCESS`. A second client quotes the stateid: assert `NFS4ERR_BAD_STATEID`. | delegation I/O refused for another principal of the holder, or served without authorising the caller |
| §5.2 unguessable `other` | Open 10^4 files from one client; assert no two `other` values share their random bits and that no field of them increases with open order. | `other` built from a counter |
| §3.1 cached reply size | `CREATE_SESSION` asking for a 1 MiB cached reply; assert 8 KiB granted. Send a `REMOVE` with `sa_cachethis` false, drop the reply, resend in the same slot and sequence; assert `NFS4ERR_RETRY_UNCACHED_REP` and the name removed once. | a partial reply replayed as complete, or an unbounded cache |
| §3.3 client record | Two nodes: `EXCHANGE_ID` on P1, then the same owner string with a new verifier on P2, then from another principal on P2; assert P2 finds the record, treats the first as a reboot and refuses the second `NFS4ERR_CLID_INUSE`. Hold a session on P1 and send `DESTROY_CLIENTID` to P2; assert `NFS4ERR_CLIENTID_BUSY`. | client state kept per node |
| §5.3 no v4.0 delegations | An NFSv4.0 client with a reachable callback address opens a file no one else uses, read-only and then for write; assert `OPEN_DELEGATE_NONE` both times and no connection to `r_addr`. | delegations offered over the v4.0 callback path |
| §5.4 grace | (cluster) pynfs reboot/reclaim tests against a shard failover, not a whole-server restart; the single-node restart is the *(1 node)* reboot row. | grace tested only by restarting everything |
| §5.4 reclaim per grace | A client mounts (sending `RECLAIM_COMPLETE`), opens files in two shards, then one shard fails over while its session survives. Assert `SEQ4_STATUS_RESTART_RECLAIM_NEEDED` on the next `SEQUENCE`, its reclaim in the failed shard granted without a delegation, its reclaim in the healthy shard of an open holding a delegation answered with the open and that delegation, and a second failover reclaimed the same way. | `RECLAIM_COMPLETE` counted once per client, a revocation flag as the signal, or a healthy shard dropping a delegation it holds |
| §5.4 single-node restart | On one node: a client mounts, sending `RECLAIM_COMPLETE`, and takes a lock; restart the node. Assert the shard's incarnation rose, the client's reclaim of the lock is granted rather than `NFS4ERR_NO_GRACE`, and a second client's conflicting lock is refused until the reclaim. | an incarnation raised only when a journal begins serving, so the grace instance repeats |
| §5.4 NFSv4.0 signal | An NFSv4.0 client holds an open in a shard that fails over. Assert its `RENEW` is `NFS4ERR_STALE_CLIENTID`, a `READ` under the open's stateid `NFS4ERR_STALE_STATEID`, and after it repeats `SETCLIENTID` and the confirm, the client ID unchanged and its reclaim granted. A Linux client completes recovery without looping. | `NFS4ERR_STALE_STATEID` with a valid client ID as the only signal |
| §11.5 full journal | Fill the journal with offload stalled and the remote reachable; assert a write is answered `NFS4ERR_DELAY` (v4) and `NFS3ERR_JUKEBOX` (v3), not `NFS4ERR_NOSPC`, and succeeds on retry once offload resumes. Fill one share's journal limit the same way: assert `NFS4ERR_DELAY` too, and success once offload drains it. Fill the journal with the store unreachable for 2 minutes, past the caller's deadline: assert every retry is `NFS4ERR_DELAY` / `NFS3ERR_JUKEBOX`, never `NOSPC`, and the writes succeed once the store returns. Make the store deny puts; assert `NFS4ERR_NOSPC` at once. Exceed a user quota; assert `NFS4ERR_DQUOT` at once. Exceed a project quota, then a share-wide one; assert `NFS4ERR_NOSPC` at once for each. | a share's journal limit answered "no space" at once, a transient refusal turned into "no space" at its deadline, or a quota answered "retry" |
| §5.3 lock recall | Client A holds a write delegation; client B sends an NLM lock and, separately, an NFSv4 `LOCK` over a range A locked locally. Assert `CB_RECALL` before B's answer and A's lock sent to the server; once the recall ends, B's NFSv4 `LOCK` is `NFS4ERR_DENIED`, a non-blocking NLM lock `NLM4_DENIED`, and a blocking NLM lock stays `NLM4_BLOCKED` behind A's lock and is sent `NLMPROC4_GRANTED` when A unlocks. Repeat with A holding a read delegation and no local lock; assert `CB_RECALL` before B is granted. While A delays its `DELEGRETURN`, assert B's NFSv4 `LOCK` answered `NFS4ERR_DELAY`, a blocking NLM lock `NLM4_BLOCKED` and then `NLMPROC4_GRANTED` once A returns, and a non-blocking one `NLM4_DENIED_GRACE_PERIOD` — never `NFS4ERR_DENIED` or `NLM4_DENIED`. | a lock request that does not recall, recalls write delegations only, or is answered "held" or "blocked" with no waiter while the recall runs |
| §5.3 handle-cached deny mode | An SMB client opens with deny-write under an RWH lease and closes its handle locally. An NFSv4 client opens for write, and an NFSv3 client writes: assert each `NFS4ERR_DELAY` / `NFS3ERR_JUKEBOX` while the break runs, then succeeds. Repeat with the SMB handle still in use: assert `NFS4ERR_SHARE_DENIED` and `NFS3ERR_ACCES` only after the break. | an NFS request refused by a deny mode only a handle cache held |
| §6 NLM | cthon04 lock tests over NFSv3; an NLM lock against an SMB byte-range lock and an SMB deny mode against `NLMPROC4_SHARE`. Fail a shard over and assert `SM_NOTIFY` reaches the client's statd and it reclaims. | NLM state held in the adapter |
| §6.1 host binding | Host A locks a range. From another address, send `NLMPROC4_FREE_ALL` and `NLMPROC4_UNLOCK` naming A's `caller_name`; assert A's lock still held. Block a lock of A's, release it, and capture `NLMPROC4_GRANTED`: assert its source is the floating address A used. Block a lock of A's again, close the connection that carried it, then release: assert `NLMPROC4_GRANTED` still arrives, on a connection the server opened to A's NLM port. | a host identified by its name alone, callbacks from a node address, or a grant sent only on the request's connection |
| §6.1 waiter lifetime | Host B blocks on host A's lock; hold A's lock for 5 minutes, then release; assert B is sent `NLMPROC4_GRANTED`. Block again and send `NLMPROC4_CANCEL` racing A's unlock, 10^3 times; assert every time either the cancel answered with no lock held by B afterwards, or B granted and told. Drop B's acknowledgements of `NLMPROC4_GRANTED` for 5 minutes; assert it is resent throughout, B still holds the lock and host C's conflicting lock stays `NLM4_BLOCKED`; then let an acknowledgement through and assert the resending stops. Have B refuse the grant; assert the lock released and C granted. Remove the share while C waits; assert C's waiter gone and its next request `NLM4_STALE_FH`. | a waiter dropped on an adapter timer, a cancel answered before the waiter is gone, or a grant released for a silence |
| §6.1 NLM grace | Fail over a shard in which one NLM host and one NFSv4.1 client held locks; the NFSv4.1 client sends `RECLAIM_COMPLETE`. Assert grace runs its whole lease period. | an NLM host counted finished by a guess |
| §5.5, §6.1 conflict holder | Hold a range by NLM, then by an SMB byte-range lock; from an NFSv4 client send `LOCK` and `LOCKT` over it. Decode the reply bytes: `NFS4ERR_DENIED`, client ID 0, owner length 0, for all four. Then `NLMPROC4_TEST` against an NFSv4 lock: `svid` 0, `oh` length 0. | an internal owner identity encoded as the holder |
| §7.2 cookies | List a 10^5-entry directory from one node, resume each page on another node, while a third client creates and removes. Assert every untouched entry exactly once, and the same cookie verifier on every page. With the share's hash key fixed so two names collide, list with a `maxcount` that ends a page between them; assert the page ends before the pair and the next holds both. | positional or node-local cookies, a verifier that changes on mutation, or a page ending inside a chain |
| §8 attributes | pynfs attribute tests (`supported_attrs`, `GETATTR`, `SETATTR`); assert every REQUIRED attribute in `supported_attrs`, `named_attr` among them and false, `sec_label` absent, and `fs_charset_cap` set by the share's name rule. | a REQUIRED attribute dropped from `supported_attrs` |
| App. B directory link count *(1 node)* | In a new directory, assert `numlinks` 2 over NFSv4 and `nlink` 2 over NFSv3; `MKDIR` two subdirectories and create a file: assert 4; `RENAME` one subdirectory into the other: assert the parent 3 and the other subdirectory 3; `RMDIR` the moved one: assert its new parent back to 2 and the top directory unchanged at 3. The share's root reports 2 plus its subdirectories. `nfstest_cache`'s directory checks pass. | a directory reporting 1, or a count that moves on a file's create |
| App. B `mode_umask` *(1 node)* | Over NFSv4.2 with umask 022, create a file mode 0666 in a directory with no inheritable entries: assert 0644. In a directory whose ACL holds an inheritable entry granting a group modify, create the same: assert the group's inherited entry intact, the umask ignored, and no entry the inherited ACL lacked. | the umask applied to an inherited ACL, or an entry added by the create mode |
| §11.3 audit entries | As a file's owner who is not an administrator, over NFSv4.0 and NFSv4.1: `GETATTR` and `SETATTR` of `sacl` (NFSv4.1) are `NFS4ERR_ACCESS`; `GETATTR` of `acl` shows no audit entry; `SETATTR` of `acl` with one is `NFS4ERR_ACCESS`, and without one keeps the stored audit entries. As an administrator, all succeed. | the SACL readable or writable by the owner, or the `acl` gate applied to one minor version |
| §11.3 ACL attributes | `SETATTR` of `acl` and `dacl` together: `NFS4ERR_INVAL`, nothing stored. `SETATTR` of `mode` 04750 with an `acl`: the ACL stored as sent, setuid set, and `Mode` the ACL's own bits. A `who` naming a group with the group flag clear: resolved in the user namespace, `NFS4ERR_BADOWNER` when no user holds it. | flag bits copied byte for byte; a mode merged into a sent ACL |
| §11.3 read-only marker | Over SMB set `FILE_ATTRIBUTE_READONLY` on a file and on an empty directory. Over NFSv3 and NFSv4: `WRITE`, `SETATTR` of `size` and `REMOVE` of the file are `NFS3ERR_ACCES` / `NFS4ERR_ACCESS`, an `OPEN` for write is `NFS4ERR_ACCESS`, `ACCESS` reports `READ` without `MODIFY` or `EXTEND`, `READ` and `RENAME` of it succeed; a create in the directory succeeds and its `RMDIR` is refused. Clear the flag over SMB: assert each now succeeds. | the marker enforced for SMB only, or answered as a read-only filesystem |
| §11.1 refused credentials *(1 node)* | Send a truncated `AUTH_SYS` credential, one with 17 gids, one with a 256-byte machine name, and an `AUTH_DH` and an unknown flavour, to a share with an everyone grant: each rejected `AUTH_BADCRED`, nothing created. | garbage credentials served as anonymous |
| §6.3 `MNT` on a Kerberos-only share *(1 node)* | `MNT` over `AUTH_UNIX` of a share whose `MinProof` is `Ticket`: the flavours and the root handle returned; a `GETATTR` with that handle over `AUTH_SYS`: refused `AUTH_TOOWEAK`; over `sec=krb5`: answered. | NFSv3 Kerberos mounts refused; or a handle that skips the minimum |
| §6.1 NLM on a Kerberos-only share *(1 node)* | On a share whose `MinProof` is `Ticket`, an NLM `LOCK` over `AUTH_SYS` as a uid that can read the file: granted; as a uid that can neither read nor own a 0600 file: refused. A store error resolving the caller: the lock refused, never granted as anonymous. | every NFSv3 lock on a Kerberos share failing; locks on files the caller cannot read |
| §10 context table *(1 node)* | `RPCSEC_GSS_DESTROY` with a corrupted header MIC: refused, the context still usable. A call at sequence `MAXSEQ`: `RPCSEC_GSS_CTXPROBLEM`, and the context gone. With the table's share sized to four contexts, one principal opening a fifth evicts its own oldest, never another principal's. A reconnect on a new TCP connection keeps its context. | an unauthenticated teardown; one principal evicting everyone |
| §8.1 ctime | An SMB client sets −1 on all four times of an open and writes through it; assert an NFSv3 `GETATTR` shows `ctime` moved. | SMB suspension freezing the NFS ctime |
| §9.1 stability | Write `DATA_SYNC`, crash the primary, read back. Assert the data and its `size` and `mtime`. Append 10^4 `FILE_SYNC` writes to a new file with the metadata store stalled; assert every reply arrives. Overwrite committed bytes `FILE_SYNC` with the store stalled; assert no success reply until the store commits — `NFS4ERR_DELAY` within the deadline — and that the reply follows that write's existence commit. Same with unstable overwrites and `COMMIT`. | an append waiting on a metadata transaction, or an overwrite answered stable before its existence commit |
| §9.1 loss verifier | Write unstable, then fail the sync window holding the writes; `COMMIT`. Assert the `COMMIT` succeeds with a verifier different from the writes', the client resends, and the data reads back. | a loss reported as a `COMMIT` error, or a `COMMIT` answered under the old verifier |
| §9.2 close-to-open | Two clients on two nodes: one writes and closes, the other opens and reads. Assert the new data, for writes staged and not yet committed. Two writes with no commit between them change `change` twice. Write, `GETATTR`, fail the shard over to another node, `GETATTR` again; assert `change` not lower. | `change` from the committed record only, or from a counter that restarts with the primary |
| §9.2 change info | Run 64 parallel creates in one directory over NFSv4 and NFSv3; assert every `change_info4` has `atomic` false and no NFSv3 `wcc_data` for the directory carries `before`. Two NFSv3 clients on two nodes write one file in parallel, 10^4 writes each; for every reply that carries `before`, assert its `size` and `mtime` equal the previous write's `after` in the file's write order. | a change-info pair reported exact that another create fell between, or a file `before` read outside the accept-lock hold that accepts the write |
| §9.3 NFSv4.2 | xfstests over NFSv4.2 (`SEEK_HOLE`/`SEEK_DATA`, `copy_file_range`, `FICLONE`, punch-hole, xattrs); `READ_PLUS` over a file with holes reads zeros where holes are. `ALLOCATE` is `NFS4ERR_NOTSUPP`. | an `ALLOCATE` answered success with nothing reserved |
| §9.3 copy and clone | `CLONE` and `COPY` of one file onto an overlapping range of itself: assert `NFS4ERR_INVAL` and the file unchanged. `CLONE` one byte over `clone_max_len`: assert `NFS4ERR_INVAL`. `COPY` and `CLONE` between shares in different namespaces: assert `NFS4ERR_XDEV`, and that Linux `cp` falls back and copies the bytes. During a 512 MiB `CLONE`, `WRITE` into its source range: assert the write is answered success only after the `CLONE` is answered, never `NFS4ERR_DELAY` within its deadline, and the destination equal to the source before the write. `COPY` a 4 GiB file while writing its source continuously: assert no source write waits longer than one 64 MiB chunk, and that a `COPY` cut by its deadline answers a short `wr_count` the client continues from. Clone a source with a Lost run: assert the destination reads that run as Lost, not zeros. Crash the primary mid-clone: assert after restart the destination equals the source at the clone's point, never zeros or a mix. | overlapping or over-length clones run, a cross-namespace copy attempted, a `COPY` freezing the whole source, a clone reading a source that changed under it, or a Lost run copied as zeros |
| §9.3 hold budget *(1 node)* | An NFSv4.1 session of 16 fore-channel slots writes into a running 512 MiB `CLONE`'s source from 8 slots at once: assert at most 4 calls passed a non-zero `WithHold` and held, the others `NFS4ERR_DELAY` at once, and a `READ` of another file on the session served meanwhile. Repeat on a session of 2 slots: assert one call held. Over NFSv3, write into the source: assert the write held and answered success after the clone, not `NFS3ERR_JUKEBOX`. | a budget that rounds to 0, or an NFSv3 write refused at once for want of a session |
| §7.4 address move | Two single-node installations A and B; a namespace whose address serves only its shares moves from A to B, naming the address, while a Linux NFSv3 client with an NLM lock and a Linux NFSv4.1 client with a lock have files open and in use there, both hard mounts. Hold the address at A for 5 minutes after B publishes, within `migration.reconnect_wait`. Assert A sends no reply on the address after its release, never `NFS3ERR_STALE` or `NFS4ERR_STALE`, and accepts no new connection there; a third NLM host's lock on the first host's range is `NLM4_DENIED_GRACE_PERIOD` at B throughout; no `SM_NOTIFY` before B takes the address, and after, `SM_NOTIFY` from that address with a state number above A's and B's, and the NLM host's reclaim granted in B's grace. Assert every file the NFSv3 client had in use reads and writes at B with no `ESTALE`. Assert the NFSv4.1 client sends no reclaim, its lock holder sees `EIO`, and its new `OPEN` is `NFS4ERR_GRACE` until B's grace ends and then granted. Start a move naming an address that also serves another of A's namespaces: assert the move refused at its freeze. | an address answering stale while it moves, so in-use files stay dead at B; a lock granted at B before the NLM host could reclaim; notification at B's publish or from B's own address; an NSM number not above A's; or an address move that strands A's other namespaces |
| §9.4 copies are synchronous | A 1 GiB `COPY` with `ca_synchronous` false: assert a synchronous reply, no copy stateid, and a short `wr_count` at the deadline from which a resent `COPY` finishes; `OFFLOAD_STATUS` of any stateid is `NFS4ERR_BAD_STATEID`. | an asynchronous copy offered with no copy state behind it |
| §3.1, §3.2 sessions *(1 node)* | pynfs `nfs4.1` session, slot and `SEQUENCE` replay tests, on one node, no node killed. | a replay run twice on one session |
| §4.2 v4.0 owners *(1 node)* | pynfs `nfs4.0` open-owner and lock-owner replay tests; a first `LOCK` of a new lock owner advances its open owner's seqid, and the open owner's next `CLOSE` is accepted. | a new lock owner's `LOCK` sequenced against the lock owner only |
| §3.5 confirm *(1 node)* | `SETCLIENTID` then the confirm from another principal: `NFS4ERR_CLID_INUSE`; `EXCHANGE_ID` then `CREATE_SESSION` from another principal: `NFS4ERR_CLID_INUSE`. `EXCHANGE_ID` for a confirmed owner from another principal: `NFS4ERR_CLID_INUSE`. A confirmed NFSv4.0 client repeats `SETCLIENTID` with its own verifier and a new callback, then `SETCLIENTID_CONFIRM`: assert the confirm accepted, the client ID and its locks unchanged, and the new callback taken. | a same-verifier `SETCLIENTID` answered with no record to confirm |
| §5.1 destroy *(1 node)* | A client holding an open sends `DESTROY_SESSION` then `DESTROY_CLIENTID`: assert `NFS4ERR_CLIENTID_BUSY` and the open intact; close it and assert the destroy succeeds. | state released to make `DESTROY_CLIENTID` succeed |
| §5.2 stateids *(1 node)* | pynfs `TEST_STATEID`, `FREE_STATEID`, bad- and old-stateid tests. Let a client's lease expire and `READ` under its stateid: assert `NFS4ERR_BAD_STATEID`, never `NFS4ERR_STALE_CLIENTID`. Quote a stateid never issued, under the shard's current grace instance: assert `NFS4ERR_BAD_STATEID`. Time out a recall and `TEST_STATEID` the delegation's stateid: assert `NFS4ERR_DELEG_REVOKED`, and success for its `FREE_STATEID`. Remove a share while a client holds opens in it: assert `TEST_STATEID` and `FREE_STATEID` of them answer `NFS4ERR_BAD_STATEID`, `SEQUENCE` sets no revoked-state flag, and the client's next `SEQUENCE` is not followed by a `TEST_STATEID` loop. | a removed share's stateids flagged until freed, which nothing can free |
| §5.1 administrator's revoke *(1 node)* | An NFSv4.1 client holds an open, a lock and a delegation; revoke it by administrator. Assert its next `SEQUENCE` succeeds on the same session with `SEQ4_STATUS_ADMIN_STATE_REVOKED` and without `SEQ4_STATUS_RECALLABLE_STATE_REVOKED`; `READ` under the open's stateid and `TEST_STATEID` of the lock's and the delegation's are each `NFS4ERR_ADMIN_REVOKED`; after `FREE_STATEID` of each the flag clears and a new `OPEN` under the same client ID succeeds, with no `NFS4ERR_STALE_CLIENTID` and no retry loop. Over NFSv4.0: a `READ` under the revoked open's stateid is `NFS4ERR_ADMIN_REVOKED` once, then `NFS4ERR_BAD_STATEID`, and the client reopens. | an administrator's revoke purging the client, so its reopen loops on `NFS4ERR_STALE_CLIENTID`, or an admin-revoked delegation answered as a missed recall |
| §5.1 revoke across a restart *(1 node)* | An NFSv4.1 client X holds an open and a lock on `db.lock`, and an NLM host H a lock on another file; revoke both by administrator, let a second client Y take X's lock and a third H's, then restart the node before X sends a `SEQUENCE`. Assert X's reclaims of the open and the lock are refused (`ErrState` (`NoReclaim`), the shard no longer listed), Y's reclaim granted, and X's new `OPEN` after grace granted, which lists the shard again so a later restart lets X reclaim it. Assert H is sent no `SM_NOTIFY` after the restart. From a harness NLM client using H's name and address (Linux sends no reclaim unprompted), send a reclaim of H's old lock: assert `NLM4_DENIED_GRACE_PERIOD` in grace and `NLM4_DENIED` after; then an ordinary lock from H on a free range: assert granted under a new client record. | a revoke forgotten at a restart, so the revoked client reclaims a lock another holds |
| §5.3 revoked delegation *(1 node)* | Let a recall time out; assert `SEQ4_STATUS_RECALLABLE_STATE_REVOKED` on the next `SEQUENCE` and `NFS4ERR_DELEG_REVOKED`, not `NFS4ERR_BAD_STATEID`, for a `WRITE` under the delegation stateid. | a revoked delegation the adapter is never told of |
| §5.4 reboot *(1 node)* | pynfs reboot and reclaim tests against a restart of the single node. | reboot recovery tested nowhere in the first release |
| §5.2, §5.4 lock across a restart *(1 node)* | An NFSv4.0 Linux client holds a byte-range lock on a file and writes under it continuously; restart the node. Assert the first `WRITE` after the restart is `NFS4ERR_STALE_STATEID`, never `NFS4ERR_BAD_STATEID`, `RENEW` is `NFS4ERR_STALE_CLIENTID`, and after `SETCLIENTID` and the confirm the lock's reclaim is granted in grace; a second client's conflicting lock is refused throughout, and the writer's I/O resumes without an error. Repeat over NFSv4.1 with the client's `SEQUENCE` carrying `SEQ4_STATUS_RESTART_RECLAIM_NEEDED`. | a stateid lost in a restart answered bad, so the client skips the lock in its reclaim |
| §7.2 cookies *(1 node)* | With the share's hash key fixed so two names collide, list with a `maxcount` that ends a page between them; assert the page ends before the pair and the next holds both. | a page ending inside a chain |
| §9.1 failed stable write *(1 node)* | Fail the journal sync under a `FILE_SYNC` `WRITE`; assert `NFS4ERR_IO` / `NFS3ERR_IO`. | a stable write answered success over a failed sync |
| §9.2 close-to-open and WCC *(1 node)* | Two clients on the one node: one writes and closes, the other opens and reads the new data. Two NFSv3 clients write one file in parallel, 10^4 writes each; for every reply that carries `before`, assert it equals the previous write's `after`. | `change` or `before` read outside the accept-lock hold |
| §8.1 privilege *(1 node)* | An export with `Squash=root` admitting `AUTH_SYS`, and a share grant naming uid 1500 `admin`: over `AUTH_SYS` as uid 1500, `chown` another user's file and read a mode-0600 file of another user; assert `NFS4ERR_PERM` and `NFS4ERR_ACCESS`. As root: assert squashed. Over Kerberos as the `admin` principal: assert both succeed. On a `sec=krb5` export, a principal the ID mapping maps to uid 0 runs `tar xpf` of an archive with other owners: with `no_root_squash` assert the owners restored; with `root_squash` assert squashed and `NFS4ERR_PERM`. | an `admin` grant honoured on an asserted uid, or root over Kerberos treated otherwise than root over `AUTH_SYS` |
| §3.5 replay through another node | `CREATE_SESSION` on P1, drop the reply, restart P1, replay the same `csa_sequence` through P2: assert the stored reply from `Adapter` and one session; `csa_sequence` + 2: `NFS4ERR_SEQ_MISORDERED`. Race two `CREATE_SESSION`s of one sequence on P1 and P2: one session, both answered with its reply. | |
| §3.3 one name, two principals *(1 node)* | Two principals send `EXCHANGE_ID` with one `co_ownerid`: the second is `NFS4ERR_CLID_INUSE` and leaves no `Pending`; over NFSv4.0 the same with `SETCLIENTID`. | |
| §3.3 reboot racing renew *(1 node)* | An NFSv4.0 client holds a lock; send `SETCLIENTID` with a new verifier and, before its confirm, `RENEW` under the old client ID: assert the renewal succeeds and the lock is held; confirm: the old ID's state released and its `RENEW` `NFS4ERR_STALE_CLIENTID`. | |

**Tests expected to fail.** The suites' tests that the decisions of this RFC
fail by design. A single-node run passes when exactly these fail; one that
starts passing means the decision changed and this list is stale. A cluster run
expects these and the one cluster entry below the table.

| Test | Fails because |
| --- | --- |
| pynfs `nfs4.0` delegation tests (`DELEG*`) | no delegations to NFSv4.0 clients (§5.3) |
| pynfs and cthon tests of `OPENATTR` and named attributes | no named attributes (§8.2) |
| pynfs courtesy-client tests: each test that lets a client's lease expire with no conflicting client and then uses its open or lock, listed by its pynfs code in the harness's known-failure list | no courtesy clients (§5.1) |
| pynfs directory-delegation and `CB_NOTIFY` tests (`GET_DIR_DELEGATION`) | no directory delegations (§5.3) |
| pynfs `SP4_SSV` state-protection tests | `SP4_SSV` refused (§3.3) |
| pynfs and xfstests `ALLOCATE` / `fallocate` reservation tests | `ALLOCATE` not served (§9.3) |
| pynfs labeled-NFS tests (`sec_label`) | no labeled NFS (§8.2) |

No suite in the list runs over UDP; §2.2's own row checks that no UDP
listener answers. A cluster run also expects pynfs's session-trunking tests across nodes to fail,
since session trunking across nodes is refused (§3.3); a single node has no
second node to trunk to, so they are not in its list.

**What must not stand in.** A single-node run cannot fail the cross-node steps
of §3.2–§3.5, §4.2 or §7.2: each is about state surviving a move between nodes,
and needs two. Their *(1 node)* rows test only what binds one node — replay on
one session, owner sequences, confirms and cookies on one node — and do not
stand in for those steps. A cluster run cannot stand in for §3.3's single-node
pNFS row or §5.4's single-node restart, which bind only the single-node
profile.

## 14. Observability

Metric names follow [RFC 25 §2](rfc-25-observability.md#2.%20Metrics) and are
shown without the `dittofs_` prefix the exporter adds.

| Answers | Metric | Type |
| --- | --- | --- |
| sessions held per node | `nfs_sessions` | gauge |
| slot-table saturation | `nfs_slots_in_use{channel}` | gauge |
| `NFS4ERR_BADSESSION` answered — clients recovering a session lost with a node | `nfs_badsession_total` | counter |
| callbacks with no live back channel | `nfs_cb_path_down_total` | counter |
| GSS contexts held per node | `nfs_gss_contexts` | gauge |
| NLM blocked waiters | `nfs_nlm_waiters` | gauge |

Operation latency is RFC 17's; grants, recalls and grace are RFC 14's; the
adapter's own rows for NFS, and its request span, are §11.7's. No share,
client, owner, file or principal label.

## 15. Open questions

1. **`change_attr_type`.** Reporting `NFS4_CHANGE_TYPE_IS_VERSION_COUNTER` lets
   clients skip invalidations. [RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)
   keeps `change` from moving backward across restarts, moves and failovers, but
   its version floor covers only versions a File records: a journal lost with
   writes whose versions only its overlay had reported can let a later write
   draw one of those versions again. Until that case is closed, `UNDEFINED`.
2. **RDMA.** NFS over RDMA (RFC 8166) is not offered. Whether it pays for
   itself depends on a deployment with RDMA fabrics and on
   [RFC 24 §3](rfc-24-resources.md#3.%20Request%20and%20reply%20buffers)'s buffer model.
3. **Directory delegations and notifications** (§5.3), with RFC 14's open
   question 4.
4. **Session trunking across nodes** (§3.3) would need slots held at a shared
   place; nothing asks for it yet.
5. **A duplicate-request cache for NFSv3 and NFSv4.0.** The adapter keeps one
   in memory today, and [RFC 20 §11](rfc-20-adapters.md#11.%20Open%20questions)
   keeps it for parity (its question 2). The service's dedup covers only a
   forwarded call ([RFC 17 §4.7](rfc-17-vfs.md#4.7%20Callable%20across%20the%20network)).
   This RFC decides whether the service's dedup should cover a single node too,
   so a retransmitted `REMOVE` or `CREATE` is answered its first result without
   an adapter cache.

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
| `change_attr_type` | `NFS4_CHANGE_TYPE_IS_UNDEFINED` (§15) |
| `size`, `space_used` | `Size`; `Charged` ([RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem)) |
| `mode`, `owner`, `owner_group`, `mode_set_masked` | the File; owner strings by [RFC 18 §4.5](rfc-18-identity.md#4.5%20NFSv4%20owner%20strings) |
| `mode_umask` (RFC 8275) | settable at create only: the mode is masked by the umask when the parent holds no inheritable ACL entry, and the umask is ignored when it does (RFC 8275 §3), so an inherited ACL is narrowed only by the create mode, as an AND that never adds an entry ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)). Linux clients send it on NFSv4.2 instead of applying the umask themselves, which would cut an inherited grant to the mode's bits |
| `acl`, `dacl`, `aclsupport` | RFC 7's ACL, spelled as §11.3 says; `aclsupport` reports allow, deny, audit and alarm |
| `sacl` | the ACL's audit entries ([RFC 7 §2.6](rfc-7-namespace-metadata.md#2.6%20ACL%2C%20and%20how%20it%20agrees%20with%20the%20mode)); administrators only (§11.3) |
| `archive`, `hidden`, `system` | `Flags` |
| `time_access`, `time_modify`, `time_metadata`, `time_create`, `*_set` | `Access`, `Modify`, `Change`, `Birth` |
| `time_delta` | `Capabilities.TimeGranularity` |
| `fileid`, `mounted_on_fileid`, `rawdev` | RFC 7; pseudo-filesystem per [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees) |
| `numlinks`, and NFSv3's `nlink` | a file's `Nlink`; a directory's 2 plus its `Subdirs`, a signed count, with its unfolded deltas added as for its times, and never less than 2: a count below zero, while its recount runs, reads as 0 and never fails the read ([RFC 7 §4.1](rfc-7-namespace-metadata.md#4.1%20%60nlink%60%20is%20exactly%20its%20entries)); a share's root the same. Clients and `find` expect 2 plus the subdirectory count, and a cache check that a directory changed when its count moved needs it to move |
| `fsid`, `unique_handles`, `homogeneous`, `fh_expire_type` | per share; true; true; persistent |
| `case_insensitive`, `case_preserving`, `maxname`, `maxlink`, `no_trunc`, `chown_restricted`, `cansettime` | `Capabilities`; the share's case rule ([RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case)) |
| `files_*`, `space_avail`, `space_free`, `space_total` | `StatFS` |
| `quota_avail_hard`, `quota_avail_soft`, `quota_used` | `Quota` for the caller's principal |
| `maxread`, `maxwrite`, `maxfilesize` | §2.3 |
| `fs_layout_type`, `layout_*`, `layout_alignment` | (cluster) pNFS ([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS)); not in `supported_attrs` on a single node (§3.3) |
| `fs_locations` | §7.4 |
| `fs_charset_cap` | the share's name rule (§7.3) |
| `suppattr_exclcreat` | `mode`, `mode_umask`, `owner`, `owner_group`, `acl`, `time_*_set` |
| `xattr_support` | true where `Capabilities.Xattrs` |
| `clone_blksize` | 0: no alignment required, since `Copy` shares content at any offset |

Not supported:

| Feature | Answer | Why |
| --- | --- | --- |
| Labeled NFS (`sec_label`, RFC 7204) | not in `supported_attrs` | no mandatory-access-control policy exists to label for |
| Named attributes (`OPENATTR`) | `NFS4ERR_NOTSUPP`; `named_attr`, a REQUIRED attribute, stays in `supported_attrs` and reads false | named streams exist for SMB; NFS clients use RFC 8276 xattrs, and Linux never uses `OPENATTR`, and macOS falls back to `._` files without it |
| `retention_*`, `retentevt_*`, `mimetype`, `time_backup`, `dir_notif_delay`, `dirent_notif_delay`, `fs_status`, `fs_locations_info`, `mdsthreshold`, `space_freed` | not in `supported_attrs` | no record holds them; `space_freed` cannot be computed cheaply where content is shared |

## Appendix C — NFSv4.2 operations

*Normative: the operation mapping of [§9.3](#9.3%20NFSv4.2%20operations).*

| Operation | Maps to | Notes |
| --- | --- | --- |
| `ALLOCATE` | `NFS4ERR_NOTSUPP` | no reservation exists below the adapter (§9.3) |
| `DEALLOCATE` | `Deallocate` | holes are RFC 6's |
| `SEEK` | `Seek` | `NFS4_CONTENT_DATA` / `NFS4_CONTENT_HOLE`, from the engine's allocation answer ([RFC 17 §5.2](rfc-17-vfs.md#5.2%20Read)); a zero ref under a newer overwrite record is data |
| `READ_PLUS` | `Read` and `Seek` | holes returned as `NFS4_CONTENT_HOLE` segments only where RFC 6 records one, never derived from reads; zeros written as data stay data, and so does a zero ref under a newer overwrite record |
| `COPY` (intra-server) | `Copy` | bounded chunks, not atomic, may answer a short `wr_count`; always synchronous, §9.4; overlapping ranges of one file `NFS4ERR_INVAL`; across namespaces `NFS4ERR_XDEV` (§9.3) |
| `OFFLOAD_STATUS`, `OFFLOAD_CANCEL`, `CB_OFFLOAD` | no service call: no copy stateid is issued (§9.4) | answered `NFS4ERR_BAD_STATEID`; `CB_OFFLOAD` never sent |
| `CLONE` | `Clone`, one atomic clone | the engine's clone at one point of the source ([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)), answered only when done; over `clone_max_len` or overlapping ranges of one file `NFS4ERR_INVAL`; across namespaces `NFS4ERR_XDEV`; I/O into its ranges held while it runs, `NFS4ERR_DELAY` only past that I/O's deadline (§9.3) |
| `GETXATTR`, `SETXATTR`, `LISTXATTRS`, `REMOVEXATTR` (RFC 8276) | `Xattrs`, `SetXattr`, `RemoveXattr` | the user namespace only; the same records SMB extended attributes reach |
| `COPY_NOTIFY`, inter-server `COPY` | `NFS4ERR_NOTSUPP` | one installation only ([RFC 17 §1.1](rfc-17-vfs.md#1.1%20Non-goals)) |
| `IO_ADVISE`, `WRITE_SAME`, `LAYOUTERROR`, `LAYOUTSTATS` | `NFS4ERR_NOTSUPP` | |
| `GETDEVICEINFO`, `GETDEVICELIST`, `LAYOUTGET`, `LAYOUTCOMMIT`, `LAYOUTRETURN` (NFSv4.1) | `NFS4ERR_NOTSUPP` on a single node; (cluster) [RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS) | §3.3 |

## Appendix D — requests and the service calls they make

Every request the NFS adapter serves, the [RFC 17](rfc-17-vfs.md) service
method it becomes, and what the adapter answers without the service. What each
method then calls below is
[RFC 17 §3.5](rfc-17-vfs.md#3.5%20What%20each%20method%20calls)'s.

**NFSv3 and its auxiliary services.**

| Request | Method | Notes |
| --- | --- | --- |
| `NULL` | none (adapter only) | |
| `GETATTR` | `GetAttr` | |
| `SETATTR` | `SetAttr` | the ctime guard is `AttrChange.Guard`; returns `wcc_data` |
| `LOOKUP` | `Lookup` | `..` at a share root answers the root ([RFC 17 §5.10](rfc-17-vfs.md#5.10%20Lookup%20and%20create)) |
| `ACCESS` | `Access` | |
| `READLINK` | `GetAttr` | the stored target, `File.Target` |
| `READ` | `Read` on `vfs.Anonymous(h)` | |
| `WRITE` | `Write` on `vfs.Anonymous(h)` | `WriteResult` gives count, verifier and `wcc_data` |
| `CREATE`, `MKDIR`, `SYMLINK`, `MKNOD` | `Create` | `EXCLUSIVE` carries `CreateArgs.Verifier` |
| `REMOVE` | `Unlink` | |
| `RMDIR` | `Rmdir` | |
| `RENAME` | `Rename` | |
| `LINK` | `Link` | |
| `READDIR`, `READDIRPLUS` | `ReadDir` | `want` asks for attributes and handles for `READDIRPLUS` |
| `FSSTAT` | `StatFS` | |
| `FSINFO`, `PATHCONF` | none (adapter only) | constants of the adapter and the share ([§2.3](#2.3%20Transfer%20sizes)) |
| `COMMIT` | `Commit` | |
| MOUNT `MNT` | `Root` with the path | |
| MOUNT `EXPORT` | `ListShares` | |
| MOUNT `UMNT`, `UMNTALL`, `DUMP` | none (adapter only) | no mount table ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)) |
| NLM `TEST` | `TestLock` on an anonymous open | |
| NLM `LOCK` | `Lock` | `Wait` callback for a blocking lock; `Reclaim` in grace ([RFC 17 §2.2](rfc-17-vfs.md#2.2%20Who%20owns%20open%20state%20and%20locks)) |
| NLM `CANCEL` | `CancelLock` | answered only once it returns |
| NLM `UNLOCK` | `Unlock` | |
| NLM `GRANTED` (to the host) | the `LockWaitOver` callback | |
| NLM `SHARE`, `UNSHARE` | `Open`, `Close` of an anonymous-owner open with a deny mode | |
| NLM `FREE_ALL` | `HostRestarted` | from the host's own address only |
| NLM `*_MSG`, `*_RES` | as their synchronous forms | answered on the host's callback service |
| NSM `SM_NOTIFY` (received) | `HostRestarted` | |
| NSM `SM_MON`, `SM_UNMON` | none (adapter only) | sent by the server to its own monitor, never received from a peer; a host is registered for restart notification by its first NLM call, which is `Connect` with its `caller_name` as monitor name (§6.2) |
| rquota `GETQUOTA` | `Quota` | |

**NFSv4.0, 4.1 and 4.2.** `COMPOUND` runs its operations as sequences of these.

| Operation | Method | Notes |
| --- | --- | --- |
| `SETCLIENTID`, `EXCHANGE_ID` | `Propose` | an unconfirmed proposal, no state ([§3.5](#3.5%20Confirming%20a%20client)) |
| `SETCLIENTID_CONFIRM` | `Connect`, naming the `Proposal` | |
| `CREATE_SESSION` | `Record`; the first `Connect`, naming the `Proposal`, then `AddSession` (§3.5) | a replay is answered from the record's adapter bytes and calls nothing more |
| `SEQUENCE` | `Renew`, at most once per renewal interval | the slot table is the adapter's |
| `RENEW` | `Renew` | |
| `DESTROY_SESSION` | none (adapter only) | |
| `DESTROY_CLIENTID` | `Destroy` | |
| `BIND_CONN_TO_SESSION`, `BACKCHANNEL_CTL` | `Connect` when the back channel moves | |
| `RECLAIM_COMPLETE` | `ReclaimComplete` | |
| `PUTROOTFH`, `PUTPUBFH` | `Root` with the zero `ShareRef` | |
| `PUTFH`, `GETFH`, `SAVEFH`, `RESTOREFH` | none (adapter only) | the next operation's call admits the handle |
| `LOOKUP`, `LOOKUPP` | `Lookup` (`..` for `LOOKUPP`) | |
| `GETATTR`, `VERIFY`, `NVERIFY` | `GetAttr` (and `GetACL` for the `acl` attribute) | the comparison is the adapter's |
| `SETATTR` | `SetAttr` (and `SetACL` for `acl`) | |
| `ACCESS` | `Access` | |
| `CREATE` | `Create` | |
| `LINK`, `REMOVE`, `RENAME` | `Link`, `Unlink`, `Rename` | `change_info4` from the `WCC` |
| `READDIR` | `ReadDir` | |
| `READLINK` | `GetAttr` | |
| `OPEN` | `Open` | create, claim types and reclaim are `OpenRequest` fields ([RFC 17 §2.2](rfc-17-vfs.md#2.2%20Who%20owns%20open%20state%20and%20locks)); `OPEN4_RESULT_CONFIRM` is `OpenResult.Confirm` |
| `OPEN_CONFIRM` | `ConfirmOpen` | NFSv4.0; the reply's stateid carries the seqid it returns |
| `OPEN_DOWNGRADE` | `Downgrade` | the reply's stateid carries the seqid it returns |
| `CLOSE` | `Close` | |
| `READ`, `WRITE`, `COMMIT` | `Read`, `Write`, `Commit` | |
| `LOCK`, `LOCKT`, `LOCKU` | `Lock`, `TestLock`, `Unlock` | ([RFC 17 §2.2](rfc-17-vfs.md#2.2%20Who%20owns%20open%20state%20and%20locks)); `LOCK` answers the lock stateid `Lock` returns, `LOCKU` the seqid `Unlock` returns |
| `RELEASE_LOCKOWNER` | `ReleaseLockOwner` | |
| `DELEGRETURN` | `AckRecall` to `GrantNone` | |
| `DELEGPURGE`, `WANT_DELEGATION`, `GET_DIR_DELEGATION`, `OPENATTR`, `SET_SSV` | not supported, answered as this RFC says of each | |
| `TEST_STATEID` | `TestState` | |
| `FREE_STATEID` | `FreeState`, for a revoked stateid or a lock stateid holding no locks | the lock stateid is freed alone, never its owner's other lock state (RFC 8881 §18.38) |
| `SECINFO`, `SECINFO_NO_NAME` | `SecInfo` | |
| `ALLOCATE` | not supported ([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)) | |
| `DEALLOCATE`, `SEEK` | `Deallocate`, `Seek` | |
| `READ_PLUS` | `Read` and `Seek` | |
| `COPY` | `Copy` | always answered synchronously, short at the deadline ([§9.4](#9.4%20Async%20COPY)) |
| `OFFLOAD_STATUS`, `OFFLOAD_CANCEL` | none (adapter only) | no copy stateid is ever issued; answered as one naming nothing |
| `CLONE` | `Clone` | |
| `GETXATTR`, `LISTXATTRS`, `SETXATTR`, `REMOVEXATTR` | `Xattrs`, `SetXattr`, `RemoveXattr` | |
| `COPY_NOTIFY`, `IO_ADVISE`, `WRITE_SAME`, `LAYOUT*`, `GETDEVICE*` | not supported ([Appendix C](#Appendix%20C%20%E2%80%94%20NFSv4.2%20operations)) | |
