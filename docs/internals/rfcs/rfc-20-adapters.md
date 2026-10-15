---
rfc: 20
title: "RFC 20 — the adapter model"
component: adapter
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-17-vfs]]"
  - "[[rfc-18-identity]]"
  - "[[rfc-19-authorization]]"
  - "[[rfc-21-nfs]]"
  - "[[rfc-22-smb]]"
  - "[[rfc-23-control-plane]]"
  - "[[rfc-24-resources]]"
  - "[[rfc-25-observability]]"
  - "[[rfc-27-namespace-migration]]"
  - "[[rfc-29-test-harness]]"
aliases:
  - RFC 20
  - adapter model
tags:
  - rfc
---
∞# RFC 20 — the adapter model

**Status:** draft. [§11](#11.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone writing or changing a protocol adapter, and anyone deciding
whether a piece of logic belongs to a protocol or to the filesystem.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code;
[Appendix B](#Appendix%20B%20%E2%80%94%20where%20the%20current%20code%20differs) says where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

An **adapter** is the code for one wire protocol. It accepts connections,
reads bytes, turns them into calls on the filesystem service of
[RFC 17](rfc-17-vfs.md), and turns the answers back into bytes. It decides
nothing about files: who may open one, whether a lock conflicts, how much quota
is left. Those are the service's, written once for every protocol.

[RFC 17](rfc-17-vfs.md) fixes what the service offers and what stays in
adapters. [RFC 21](rfc-21-nfs.md) and [RFC 22](rfc-22-smb.md) fix the choices
each protocol's standards leave open. This RFC is the part between: the shape
every adapter has — how it starts and stops, how a request travels through it,
how it encodes and decodes, how it handles many clients at once — and how we
prove an adapter only translates, and translates correctly.

**The problem, with one example.** alice-pc opens
`profiles/alice/ODFC_alice.vhdx` over SMB with deny-write. build01 writes to
the same file over NFSv3. RFC 17 decides, in one place, that build01's write is
a sharing violation. That decision is worth only what the adapters let through.
If the NFSv3 handler, written for a protocol with no opens, checked permissions
itself and called the content engine directly "because NFSv3 is simple", the
service's check would never run. The rule would exist and be bypassed, and
nothing would be red: the NFS suite passes, the SMB suite passes, and the
service suite tests a path the NFS adapter does not take.

With this RFC:

1. The NFS adapter's packages import the service and their own wire and session
   code, and nothing else. A handler that reached for the engine would not
   build, and a test says so (§2.3).
2. The NFSv3 `WRITE` handler decodes the arguments, builds an anonymous open
   from the caller's identity, makes **one** call, `Write`, and maps what comes
   back (§4.3).
3. The service refuses the write with `ErrShareViolation`; the adapter's error
   table turns it into `NFS3ERR_ACCES`
   ([RFC 21 §5.6](rfc-21-nfs.md#5.6%20Open-state%20and%20capacity%20errors%20on%20the%20wire)), and the table's own test proves every service error has a row (§4.5).
4. A nightly run sends the same NFSv3 and SMB sequence to a reference server and
   to DittoFS, captures both, and compares them field by field after removing
   what legitimately differs (§8). A reply that differs, and that no recorded
   decision explains, is red.

**The words you need.**

- **adapter** — the code for one wire protocol, from the listening socket to
  the service call ([RFC 17 §4.2](rfc-17-vfs.md#4.2%20Translation%20stays%20in%20adapters)).
- **codec** — the pure functions that turn bytes into a request's fields and a
  reply's fields into bytes; no I/O, no service (§5).
- **handler** — the function for one wire operation: fields in, one service
  call, fields out (§4.3).
- **connection state** — what the protocol keeps per connection or session:
  slot tables, reply caches, credits, security contexts, ID tables. The adapter
  owns it, in memory only (§2.2).
- **client-held name** — anything a client can present again after its
  connection is gone: a file handle, a stateid, a durable handle, a create
  verifier. Never the adapter's (§2.1).
- **reference server** — a widely deployed implementation of the protocol whose
  wire behaviour this one is compared against (§8).

![The two adapters, each holding only wire, session, security and ID state in memory, calling the filesystem service; the service alone reaches the metadata store, open state, the content engine and routing; an adapter's direct import of any of those fails the import test](img/rfc20-boundary.svg)

**What this RFC promises.**

- Every adapter has the same lifecycle, the same request pipeline and the same
  drain on stop, so a fix to one stage is a fix for both protocols.
- An adapter holds no filesystem logic, and the build proves it: its imports
  are checked, its handlers make at most one mutating service call, and every
  service error has a row in its error table.
- A slow or silent client costs its own connection, never a worker another
  client needs.
- An adapter's tests test translation; the service's tests test semantics; and a
  wire comparison against a reference server catches what neither can.

**How the rest is organised.** §1 gives the purpose and non-goals. §2 is the
boundary: what an adapter may hold, and the two questions that decide it. §3 is
the lifecycle, §4 the dispatch pipeline, §5 encoding and decoding, §6
concurrency, the network, what makes an adapter fast, and TCP. §7 and §8 are
the two kinds of test this RFC owns — the boundary tests and the wire tests. §9 and §10 are invariants and
metrics; §11 lists open questions. Appendix A is prior art, Appendix B where the
current code differs, Appendix C the reference profile for wire tests.

### Scenarios

**A handler is tempted to compose.** An SMB `CREATE` with
`FILE_OPEN_IF` and deny-write arrives.

1. The handler could look the name up, create it if absent, then open it — three
   calls, and two clients could interleave between them.
2. It may not: a handler makes at most one mutating call (§4.3). It builds one
   `OpenRequest` carrying the disposition, the access, the deny mode and the
   lease wanted, and calls `Open` once
   ([RFC 17 §4.1](rfc-17-vfs.md#4.1%20The%20operation%20set%20is%20the%20union%2C%20not%20the%20intersection)).
3. If the service could not express the operation atomically, the fix is a
   service operation, never a sequence in the adapter.

Outcome: the atomicity SMB promises is the service's to keep, for both
protocols.

**The node is told to stop while build01 is mid-untar.**

1. The adapter stops accepting connections and stops reading new requests from
   the ones it has (§3.2).
2. Requests already handed to the service run to their answer, bounded by the
   drain deadline; their replies are sent.
3. Each connection is then closed. NFS clients, on hard mounts, retry against
   whichever node next answers the address; SMB clients reconnect and reclaim
   their durable opens ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)).
4. Nothing the adapter held is written anywhere: slot tables, reply caches and
   sessions are gone, as RFC 21 §3.2 and RFC 22 §4.3 already accept.

Outcome: a stop loses only what a crash would, and loses no acknowledged write.

**A Windows client sends a reply-size edge the spec leaves vague.**

1. The nightly wire run sends the same `QUERY_DIRECTORY` with a 1 KiB output
   buffer to the reference SMB server and to DittoFS.
2. The reference answers `STATUS_BUFFER_OVERFLOW` with one partial entry;
   DittoFS answers `STATUS_INVALID_PARAMETER`.
3. After normalisation (§8.3) the two replies differ in status, and no entry in
   the divergence list names a decision for it. The run is red, with both
   captures attached.

Outcome: a difference no unit test would have looked for is found before a
Windows user hits it.

## In short

- An adapter is wire, sessions and encoding. It holds only connection state, in
  memory, and calls only the service ([RFC 17](rfc-17-vfs.md)).
- Two questions place every piece of logic: could another protocol observe it,
  and can the client present it back after its connection is gone? Yes to
  either and it is not the adapter's.
- Every adapter has one lifecycle — configured, serving, draining, stopped — and
  one pipeline: frame, decode, admit, replay check, resolve, call, map, encode,
  send.
- A handler makes at most one mutating service call. A compound is a sequence
  of handlers, run by the adapter's compound runner, never one handler doing
  several things.
- Codecs are pure, bounded and fuzzed, and anchored by golden vectors taken from
  the specifications and from reference captures, not by round trips alone.
- One reader and one writer per connection; bounded work in flight per
  connection, set by the protocol's own flow control; no worker waits on a
  client.
- Speed comes from what the adapter does not do: no serialising of a
  connection's requests, no payload copies, no shared lock on the request path.
  Buffers are pooled by size class, replies are sent in gathered writes, and
  Nagle's algorithm is off.
- Boundary tests prove the adapter only translates; wire tests compare it with a
  reference server, field by field, with every accepted difference recorded.

---

## 1. Purpose

Two adapters written separately grow the same parts twice — a listener, a
connection loop, a drain, a request budget, an error table — and grow them
differently. The second copy is where a check goes missing: a deadline one
protocol enforces and the other does not, a drain that waits on one and drops
the other. This RFC fixes the shape both share, so it is written once, and
fixes how we show that an adapter does nothing else.

### 1.1 Non-goals

This RFC **MUST NOT** be read as specifying:

- the service's operations, their order or their errors — those are
  [RFC 17](rfc-17-vfs.md)'s;
- a protocol's own choices — versions, dialects, transfer sizes, slot and credit
  counts, attributes, names — those are [RFC 21](rfc-21-nfs.md)'s and
  [RFC 22](rfc-22-smb.md)'s;
- how a principal is authenticated against a directory, or how it is mapped to
  numeric IDs and SIDs — that is [RFC 18](rfc-18-identity.md)'s. This RFC fixes only that the
  adapter runs the protocol's handshake and hands the service what it proved
  (§4.4);
- memory budgets and buffer pool sizes — [RFC 24 §2](rfc-24-resources.md#2.%20The%20node%20memory%20budget)'s. This RFC fixes where
  the bounds apply, not their values (§6);
- the test runner, tiers and grading — [RFC 29](rfc-29-test-harness.md)'s. This
  RFC states the checks.

## 2. The boundary

### 2.1 Two questions place every piece of logic

A piece of behaviour belongs to the service, not the adapter, when either of
these is true:

1. **Another protocol could observe it.** A deny mode, a lease, a lock, a
   pending delete, a stream, a DOS attribute, a timestamp held suspended
   through an open: the other protocol must see it, so it is decided where both
   are visible. That is the purpose of the whole design — the protocols are
   interoperable wherever the semantics allow — and an adapter that decides one
   of these alone makes it invisible to the other.
2. **The client can present it back after its connection is gone.** A file
   handle, a stateid, a client ID, a durable or persistent handle, an exclusive
   create verifier, an NLM grant: the client keeps the name and sends it again
   after a reconnect — to this node after a restart, or to another after an
   address takeover. Whatever the name refers to **MUST** outlive the
   connection, so it is [RFC 7](rfc-7-namespace-metadata.md)'s or
   [RFC 14](rfc-14-open-state.md)'s, never the adapter's memory.

Everything else is the adapter's: what the protocol's specification alone
defines, kept per connection or session, and gone when they are.

The second question is not "must it survive a node failure". Much of what an
adapter holds is lost on a restart by decision — NFSv4.1 reply caches
([RFC 21 §3.2](rfc-21-nfs.md#3.2%20The%20reply%20cache%20is%20per%20session%2C%20in%20memory)), SMB sessions
([RFC 22 §4.3](rfc-22-smb.md#4.3%20Reconnect%20after%20a%20node%20is%20lost)) — and
that is right, because the protocol already tells the client to rebuild them:
`NFS4ERR_BADSESSION`, a new SMB session setup. The test is whether the client
holds a name for the thing. A client that holds a name it can never rebuild
must find the thing still there.

> decision: placement is decided by observability and by client-held names,
> not by which protocol uses a feature. A feature only SMB triggers — a deny
> mode, delete on close, an alternate data stream — is still the service's,
> because NFS must see its effect. Move a feature into an adapter only when both
> questions answer no, and say so in that protocol's RFC.

**Worked example.** Six details, placed by the two questions:

| Detail | Could the other protocol observe it? | Does the client present it back? | Lives in |
| --- | --- | --- | --- |
| alice-pc's deny-write on her container | yes — build01's NFSv3 write must be refused | — | the service ([RFC 14](rfc-14-open-state.md)) |
| a named stream `report.docx:summary` | yes — removing the file over NFS removes its streams | — | the service ([RFC 7](rfc-7-namespace-metadata.md)) |
| build01's exclusive create verifier | no | yes — the `OPEN` is resent after a reconnect | the service ([RFC 7 §2.10](rfc-7-namespace-metadata.md#2.10%20Exclusive%20create)) |
| the cached reply in build01's slot 7 | no | no — after node loss the client is told `NFS4ERR_BADSESSION` and opens a new session | the adapter, in memory |
| the 512 credits granted to alice-pc's connection | no | no — credits end with the connection | the adapter, in memory |
| the name `a:b` shown to SMB as `a` U+F022 `b` | no — NFS sees the stored name | no — the mapping is computed, nothing is held | the adapter's codec |

### 2.2 What an adapter may hold

An adapter **MAY** hold, in the memory of the `protocol` node serving the
connection, and nothing else:

| Kind | Examples |
| --- | --- |
| Transport | listeners, connections, framing state, TLS or transport-encryption state |
| Security contexts | an RPCSEC_GSS context, an SMB session's keys, signing and encryption state, a handshake in progress |
| Flow control | NFSv4.1 slot tables, SMB credits, per-connection in-flight counts |
| Exactly-once state | the NFSv4.1 reply cache, SMB message IDs and channel sequence |
| ID encodings | the wire form of a stateid, an SMB `FileId`, a tree ID, a session ID, a `READDIR` cookie and its verifier — each a translation to and from a service value |
| Compound state | NFSv4 current and saved filehandles, SMB related-request chaining |
| Requests in progress | an SMB request answered `STATUS_PENDING` and re-driven ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)), a waiting lock's wire identity |

None of it is written to the metadata store, and none is replicated. Every ID
encoding **MUST** be reversible from the connection's own tables to the service
value it stands for, or refused with the protocol's stale or bad-ID error: an
adapter never guesses which open a wire ID meant.

An adapter **MUST NOT** hold a cache of anything the service returns — an
attribute, a permission decision, a lookup result, a directory listing — beyond
the request that read it. A cached attribute is the most common way one
protocol's client sees another's change late; the client's own caching rules
([RFC 21 §9.2](rfc-21-nfs.md#9.2%20Close-to-open%2C%20and%20what%20the%20server%20guarantees), RFC 22's leases) are the
only caching there is.

> ponytail: no adapter-side attribute cache. Every `GETATTR` and every reply's
> post-operation attributes reach the service. Add one, keyed by the file's
> `Version` and invalidated by the service's change stream, only when a profile
> on the reference box shows attribute reads at the primary limiting a
> metadata-heavy workload.

### 2.3 What an adapter may import

An adapter's packages **MUST** import only:

- the service's interface and value types ([RFC 17 §3](rfc-17-vfs.md#3.%20Interface)),
  and the neutral types it names;
- the adapter's own codecs, session and connection code;
- the shared adapter runtime of this RFC — listener, connection loop, drain,
  pipeline, metrics (§3, §4, §6);
- the standard library and wire-format libraries that do no file I/O.

They **MUST NOT** import a metadata store, the open-state tables, the content
engine, the topology or routing code, the configuration store, or another
adapter. Two adapters that need the same helper — the ACL translation between a
security descriptor or an NFSv4 ACL and the neutral ACL
([RFC 19 §5.1](rfc-19-authorization.md#5.1%20One%20translation%2C%20two%20spellings)),
a time conversion — take it from a shared translation package that itself
imports only neutral types.

A test **MUST** enforce this list from the import graph, and fail naming the
package and the forbidden import (§7.1). A rule nobody checks drifts the first
week someone is in a hurry.

### 2.4 Protocol details that stay in the adapter

What both questions answer no to, listed so the line is not re-argued per
feature:

| Detail | Why it is the adapter's |
| --- | --- |
| Name presentation: names SMB cannot carry, invalid UTF-8 hidden ([RFC 22 §12.3](rfc-22-smb.md#12.3%20Names%20SMB%20cannot%20carry)) | a reversible mapping of a stored name; the stored name is the service's |
| NFSv4 owner strings `user@domain` | the encoding of an identity; the mapping is [RFC 18 §4.5](rfc-18-identity.md#4.5%20NFSv4%20owner%20strings)'s |
| Security descriptors and NFSv4 ACLs | spelled by the one shared translation ([RFC 19 §5.1](rfc-19-authorization.md#5.1%20One%20translation%2C%20two%20spellings)); what an ACL means, and what a set of one may hold, are RFC 19's |
| Attribute bitmaps, SMB information classes, `FSINFO` and volume replies | formatting what the service returned |
| `READDIR` and `QUERY_DIRECTORY` reply trimming to the client's buffer | the service resumes from a cursor ([RFC 17 §4.7](rfc-17-vfs.md#4.7%20Callable%20across%20the%20network)); fitting a reply is the protocol's |
| SMB control codes | each translated into one service call — copychunk to `Copy`, zero data to `Deallocate`, allocated ranges to `Seek` ([RFC 22 §10](rfc-22-smb.md#10.%20Control%20codes)) |
| IPC$ and its RPC services | an RPC endpoint calling `ListShares` and identity lookups; it reaches no file ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)) |
| Port mapper, MOUNT's empty `DUMP` | side programs with no file state ([RFC 21 §6.3](rfc-21-nfs.md#6.3%20MOUNT)) |

And what looks protocol-only but is not:

| Detail | Why it is the service's |
| --- | --- |
| Exclusive create verifier | the client presents it again after a reconnect ([RFC 7 §2.10](rfc-7-namespace-metadata.md#2.10%20Exclusive%20create)) |
| `SETATTR` guarded by ctime | a compare-and-set: checked in the adapter it races the write it guards; the service takes the guard as an argument |
| Timestamp suspension (−1 / −2) | held with the open at the primary ([RFC 22](rfc-22-smb.md)) |
| Delete on close, streams, DOS attributes | NFS observes them |
| A blocked lock's wait | the waiter is open state; only its wire answer is the adapter's ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)) |

## 3. Common lifecycle

### 3.1 The adapter's states

Every adapter runs through the same four states, driven by the control plane
([RFC 23 §2.3](rfc-23-control-plane.md#2.3%20The%20runtime%20reconciles%3B%20it%20does%20not%20take%20orders)):

![The adapter's states — configured, serving, draining, stopped, with reconfigure looping on serving — and below them one connection's life: accept, negotiate, authenticate, serve with the pipeline per request, close](img/rfc20-lifecycle.svg)

- **configured** — settings validated ([RFC 13](rfc-13-configuration.md)), no
  socket open. A setting the adapter cannot apply **MUST** fail here, not at the
  first request.
- **serving** — listeners open, connections accepted. Start succeeds only once
  every configured listener is bound; a partial bind is a failed start, and
  closes what it opened.
- **draining** — no new connection is accepted and no new request is read from
  an open one. Requests already handed to the service run to their answer, and
  their replies are sent. Draining ends when none is left or at the drain
  deadline, whichever is first.
- **stopped** — every connection closed, every connection's state dropped,
  `Disconnect` called for each SMB session that ended
  ([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations)).

A request still running at the drain deadline is abandoned: its context is
cancelled, its reply is not sent, and the client retries as after a lost reply.
The adapter **MUST NOT** answer it with an error the client would treat as
final, since the service may still apply it; a retry meets the service's
dedup ([RFC 17 §4.7](rfc-17-vfs.md#4.7%20Callable%20across%20the%20network)) or the protocol's own replay
rules.

**Reconfigure** adds or removes listeners without a restart. Removing an
address closes its listener and every connection accepted on it — exactly what
a namespace move's release asks of an adapter
([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks), [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)) — and
leaves the others serving.

**Side programs are part of the adapter.** A protocol's auxiliary listeners —
the port mapper, MOUNT, NLM and NSM for NFS, a service-discovery announcement,
the RPC endpoints behind SMB's `IPC$` — start with the adapter, all or none, and
stop after its own listeners close, in reverse order. A side program that fails
to start fails the adapter's start. A datagram side program, such as a port
mapper answering on UDP, reads into a bounded queue and drops a datagram that
arrives at a full one: the client retries.

**Health.** An adapter is healthy when it is serving with every listener and
side program bound. Draining reports not ready
([RFC 25 §3.6](rfc-25-observability.md#3.6%20Readiness)), so that whatever steers new
clients — an address takeover ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)), a load balancer — stops sending
them before the drain ends.

**A setting is live or restart-only.** Each adapter setting is marked as one or
the other in [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)'s table. A live setting applies to connections and sessions set
up after the change; one already set up keeps what it negotiated — its dialect,
signing, encryption and sizes — until it ends. A restart-only setting is applied
by a drain and a start, never by changing a serving connection under its client.

### 3.2 A connection's lifecycle

The lower half of the drawing above:

1. **accept** — the connection is charged its fixed cost to the node's
   connection-state pool and counted against the node's connection limit and
   its address's quarter of it (§6.4,
   [RFC 24 §4.4](rfc-24-resources.md#4.4%20Connections)); past any of them, the
   connection is closed at once, before any byte is read.
2. **negotiate** — the protocol's version or dialect exchange. Requests other
   than the negotiation are refused as the protocol says until it completes.
3. **authenticate** — the protocol's security handshake (§4.4). NFS's
   per-request credentials make this a per-request step for AUTH_SYS and a
   context setup for RPCSEC_GSS; SMB's is per session.
4. **serve** — the request pipeline of §4.
5. **close** — by the client, by an idle timeout, by a protocol violation, by a
   drain, or by a `Revoked` callback naming the address
   ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). Closing cancels the context of
   every request still in progress on the connection, drops the connection's
   state, and, for the last channel of an SMB session, calls `Disconnect`.

The service learns of a client at the protocol's own moment
([RFC 17 §3.1](rfc-17-vfs.md#3.1%20Operations) `Connect`) — not at accept, since
an NFS client's identity spans connections and an SMB client's spans sessions.
A connection is the adapter's; a client is the service's.

**Cleanup comes before reconnect, and blocks only the session it is for.** A
closing connection's cleanup — `Disconnect` for each session whose last channel
it was, its ID tables dropped — completes before a reconnect of the same session
is served on another connection, so the reconnect never meets half-ended state.
The wait is keyed by the session: one client's slow cleanup never delays another
client's setup.

### 3.3 Callbacks are the adapter's to deliver

The `Callbacks` an adapter registers at `Connect`
([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)) are implemented once per protocol
on the shared connection runtime: they find the connection holding the client's
back channel, encode the protocol's callback — `CB_RECALL`, a lease break,
`NLM_GRANTED`, a change notification — and send it on that connection's writer
(§6.2). A callback **MUST** return within its context's deadline whether or not
the client answers; a client that has no live connection is a callback that
failed, and the service handles it as the client not answering.

`Revoked` is the adapter's prompt to answer what it holds pending on the
revoked state — a waiting lock, an outstanding notification, a held request —
with the protocol's error, and, for a moved share's address, to close the
listener and its connections (§3.1).

## 4. Dispatch

### 4.1 One pipeline

Every request, in either protocol, passes the same stages in this order:

![The pipeline: frame, decode header, admit, replay check, decode args and resolve IDs, each with its protocol-only refusal; then the one stage that calls the service; then map error, encode and send; a compound's next operation and a held SMB request both re-enter at resolve IDs](img/rfc20-pipeline.svg)

| Stage | Does | Fails as |
| --- | --- | --- |
| frame | reads one record (RPC record marking, NetBIOS session header), bounded by the protocol's largest request ([RFC 21 §2.3](rfc-21-nfs.md#2.3%20Transfer%20sizes), [RFC 22 §3.3](rfc-22-smb.md#3.3%20Sizes%20and%20credits)); reserves its receive bytes from the length in its header before reading the body ([RFC 24 §3.2](rfc-24-resources.md#3.2%20Receive%3A%20charged%20at%20frame%2C%20and%20the%20reader%20waits)) | a length past the cap: connection closed; no room in the receive pool: the record is not read until room frees |
| decode header | the RPC header and credential, or the SMB2 header; signature check | the protocol's garbage-args or invalid-parameter answer; a bad signature drops the request |
| admit | charges the request to the connection's flow control (§6.3) | the request is not read until room frees |
| replay check | the NFSv4.1 slot and sequence, SMB replay and channel sequence | the cached reply, or the protocol's sequence error |
| decode args | the operation's arguments, through its codec (§5) | the protocol's garbage-args or invalid-parameter answer |
| resolve IDs | wire IDs to service values: stateid or `FileId` to an `OpenRef`, tree ID to its root handle | the protocol's bad- or stale-ID answer |
| call service | the handler (§4.3) | — |
| map error | the service's error to the protocol's status (§4.5) | — |
| encode | the reply, through its codec, into the reply buffer (§5.3) | — |
| send | hands the encoded reply to the connection's writer (§6.2) | connection closed |

Stages before **call service** touch no file and call no service operation.
A request refused before that stage is refused for protocol reasons only, and
leaves no trace at the primary.

The pipeline is one implementation, parameterised per protocol by its framer,
header codec, flow control, replay check and error table. Two adapters **MUST
NOT** each write their own: a stage missing in one is the drift §1 describes.

### 4.2 The operation table

Each protocol registers one table from operation code to handler. An operation
the table lacks is answered the protocol's not-supported status, by the
pipeline, before argument decoding. Version and dialect gating — an NFSv4.2
operation in a 4.1 compound, an SMB 3.x command on a 2.1 connection — is a
column of the table, not a check in the handler.

Each row also carries the preconditions the pipeline checks before the handler
runs, so that no handler repeats them:

| Column | Meaning | Example |
| --- | --- | --- |
| allowed before | whether it may arrive before negotiation or authentication completes | `NEGOTIATE`, `SESSION_SETUP`, `EXCHANGE_ID` |
| needs | session, tree connect, current filehandle — refused with the protocol's status when absent | SMB `READ` needs a session and a tree; NFSv4 `PUTROOTFH` needs no filehandle |
| versions | the first and last version or dialect that serve it | NFSv4.2 `SEEK` from minor version 2 |
| mutates | whether the handler may make its one mutating call (§4.3) | `WRITE` yes, `GETATTR` no |
| cacheable | whether its reply goes in the replay cache | NFSv4.1 `CREATE`, `REMOVE`, `RENAME` |
| reply bound | the largest reply it can produce for its arguments, before running it | `READ`: the count asked plus its header |

A test asserts every row fills every column.

Cross-cutting work — tracing, metrics, deadlines, panic recovery — wraps every
handler the same way, in the pipeline, in this fixed order outside in:
recovery, trace span, metrics, deadline. A handler never opens its own span or
records its own latency ([RFC 17 §7](rfc-17-vfs.md#7.%20Observability) owns
operation latency).

A handler that panics **MUST** be answered with the protocol's server-fault
status, logged at error with the operation and the stack, and **MUST NOT** take
down the connection's other requests.

### 4.3 A handler makes at most one mutating call

A handler is the function for one wire operation:

```go
// Handler is indicative; the obligations around it are normative.
type Handler[Req, Rep any] func(ctx context.Context, c *Conn, req *Req) (*Rep, error)
```

- It **MAY** make zero service calls, for operations that are the protocol's
  own: `NULL`, `ECHO`, `NEGOTIATE`, `SEQUENCE`, `CANCEL`, session setup.
- It **MUST** make at most one service call that changes state. A wire
  operation the service cannot express in one call is a missing service
  operation ([RFC 17 §4.1](rfc-17-vfs.md#4.1%20The%20operation%20set%20is%20the%20union%2C%20not%20the%20intersection)), never a sequence in the handler.
- It **MAY** make further read-only calls only to fill fields the reply
  carries, and only where the service's mutating call does not already return
  them. Where it does — pre- and post-operation attributes, the change
  attribute, `MaximalAccess` — the handler uses what it got.
- It **MUST NOT** branch on a file's attributes, mode, ACL or type to decide
  whether the operation is allowed. It may branch on them to format the reply.

**Example.** The NFSv3 `WRITE` handler, whole:

```go
func write3(ctx context.Context, c *Conn, a *WRITE3args) (*WRITE3res, error) {
	o := vfs.Anonymous(c.Identity(ctx), a.File) // NFSv3 has no opens (RFC 17 §4.1)
	n, v, err := c.Svc.Write(ctx, o, int64(a.Offset), a.Data, stability(a.Stable))
	if err != nil {
		return nil, err // mapped once, by the table (§4.5)
	}
	after, _ := c.Svc.GetAttr(ctx, c.Identity(ctx), a.File) // read-only, for the reply's attributes
	return &WRITE3res{Count: uint32(n), Committed: a.Stable, Verf: v, After: after}, nil
}
```

It looks up no share, checks no permission, tests no lock and calls no engine.
A version that did any of them fails the import test (§7.1) or the recording
service's expectations (§7.2).

**A compound is a sequence of handlers.** NFSv4 `COMPOUND` and SMB compounded
requests are run by the adapter's compound runner, one handler per operation,
carrying the compound state of §2.2 between them. The runner stops at the first
failure as NFSv4 requires, and applies SMB's related-request rule. It holds no
state between operations beyond that compound state: no lock is held across
two operations of one compound, and two operations of one compound are two
service calls, admitted separately
([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)).

- A compound longer than the protocol's operation cap is refused before any of
  its operations runs; the cap is the protocol RFC's.
- **The reply budget is checked before each operation, not after.** The runner
  compares the operation's reply bound (§4.2) with what is left of the reply the
  client allows — NFSv4.1's `ca_maxresponsesize`, SMB's output buffer — and
  refuses an operation that would not fit before it runs (`NFS4ERR_REP_TOO_BIG`).
  A mutation applied whose reply then cannot be sent is a mutation the client
  will send again.

**Example: an SMB compound.** Explorer reading a file's properties sends
`CREATE`, `QUERY_INFO` and `CLOSE`, related. The runner calls `Open`, keeps the
returned open as the compound's current file, answers `QUERY_INFO` from
`GetAttr` through it, then calls `Close`: three handlers, three service calls,
each admitted. If `Open` fails, the related `QUERY_INFO` and `CLOSE` are answered
with the same status, as SMB requires, and reach no service call.

> ponytail: compounds run as sequences of single calls. A `LOOKUP`-`GETATTR`-`OPEN`
> chain is three round trips to the primary in a split deployment. Batch them
> into one service call only when RFC 17 §9's open question is answered by a
> measurement showing compound round trips limiting a split deployment.

### 4.4 Identity

The adapter runs the protocol's security handshake — AUTH_SYS credential
decoding, the RPCSEC_GSS context exchange, SMB's NTLM or Kerberos session setup
— and hands the service an `Identity` holding what the handshake proved, and
nothing more ([RFC 17 §3](rfc-17-vfs.md#3.%20Interface)): the protocol, the
flavour and level, the principal or numeric IDs the credential carried, the
client's address.

The adapter **MUST NOT** map a principal to numeric IDs or SIDs, apply
squashing, or decide that a principal is privileged. Those are the service's,
per share, by [RFC 18 §6](rfc-18-identity.md#6.%20Resolution%20per%20share)'s
resolution. Where the handshake itself needs a directory — verifying an NTLM
response, a Kerberos ticket against a keytab — the adapter calls the
authentication interface RFC 18 defines
([RFC 18 §3.1](rfc-18-identity.md#3.1%20The%20authentication%20interface)), and
holds only the result in its security context. A handshake that fails is
answered as the protocol's logon failure, and never becomes an anonymous or
guest session.

An `Identity` **MUST** be built afresh for each request from the connection's
security context. Any cache keyed on identity carries every field the
`Identity` has, so two principals that share numeric IDs never share an entry.

### 4.5 One error table per protocol

Each adapter maps the service's neutral errors
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) through one
table, keyed by error and, where the protocol needs it, by operation and
version:

- every error the service can return **MUST** have a row in every adapter's
  table, checked by a test that enumerates the service's errors (§7.3);
- an error with no row — a bug in the service or the table — is answered the
  protocol's server-fault status and logged at error;
- an `ErrDelay` is mapped by the cause it carries
  ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)), never by the operation;
- expected refusals — `ErrNotFound`, `ErrExist`, `ErrAccess` and the like — are
  logged at debug, unexpected ones at error.

Who writes which rows: a protocol RFC states the rows whose answer is a
protocol choice — which status makes a client retry, reclaim or give up
([RFC 21 §5.6](rfc-21-nfs.md#5.6%20Open-state%20and%20capacity%20errors%20on%20the%20wire),
[RFC 22 §6.6](rfc-22-smb.md#6.6%20Service%20errors%20on%20the%20wire)). The rest follow
from the protocol's own specification, are not restated in any RFC, and live
only in the adapter's table. This RFC fixes that there is exactly one table per
protocol and that it is total (§7.3).

*Illustrative rows; the authoritative ones are in the protocol RFCs:*

| Service error | NFSv3 | NFSv4 | SMB |
| --- | --- | --- | --- |
| `ErrNotFound` | `NFS3ERR_NOENT` | `NFS4ERR_NOENT` | `STATUS_OBJECT_NAME_NOT_FOUND` |
| `ErrExist` | `NFS3ERR_EXIST` | `NFS4ERR_EXIST` | `STATUS_OBJECT_NAME_COLLISION` |
| `ErrAccess` | `NFS3ERR_ACCES` | `NFS4ERR_ACCESS` | `STATUS_ACCESS_DENIED` |
| `ErrDelay` with a conflict cause | `NFS3ERR_JUKEBOX` | `NFS4ERR_DELAY` | held, then by cause (§4.6) |
| no row | `NFS3ERR_SERVERFAULT` | `NFS4ERR_SERVERFAULT` | `STATUS_INTERNAL_ERROR` |

### 4.6 Held requests

Where the protocol has no retry signal, as for SMB, the adapter holds a request
answered `ErrDelay`, sends the interim response, and re-drives the call
([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). The re-drive is a pipeline
mechanism, not handler code:

- a held request holds no worker between attempts; it is a timer and the
  decoded request;
- each attempt re-runs **call service** onward with the same decoded arguments
  and the same wire IDs re-resolved, so an open closed meanwhile is seen;
- how long a request is held, per cause, is RFC 17 §3.2's table, applied by the
  pipeline from the cause the `ErrDelay` carries; a handler never chooses;
- a client `CANCEL` or a closed connection ends the hold, and for a waiting lock
  calls `CancelLock` before answering ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)).

## 5. Encoding and decoding

### 5.1 Codecs are pure

A codec is a pair of functions per message: decode bytes into fields, encode
fields into bytes. A codec **MUST NOT** do I/O, call the service, read a clock
or consult a setting; what it needs — the negotiated dialect, the minor version,
the connection's maximum sizes — is passed in. Pure codecs are what make §5.4's
golden vectors and fuzzing possible.

Codecs **MAY** be generated from the protocol's machine-readable definitions —
XDR specifications for the NFS family — or written by hand where none exists,
as for SMB2. Either way the constants — operation codes, status codes, flag
bits, structure sizes — are taken from the specification once, in one place per
protocol, and every use names them.

### 5.2 Decoding is bounded

A decoder **MUST** check every length, count and offset against both the bytes
remaining and the protocol's cap before it allocates or reads: an array count
of four billion in a 200-byte request is refused, not allocated. Caps are the
protocol RFCs' sizes ([RFC 21 §2.3](rfc-21-nfs.md#2.3%20Transfer%20sizes),
[RFC 22 §3.3](rfc-22-smb.md#3.3%20Sizes%20and%20credits)). A decoder refuses
trailing bytes the protocol forbids and accepts the ones it allows, as the
specification says for each message — SMB's 8-byte alignment padding, XDR's
4-byte padding.

A malformed request is refused with the protocol's garbage-arguments or
invalid-parameter status. A framing error the protocol cannot recover from —
a record length past the cap, a broken NetBIOS header — closes the connection.

**Example.** An NFSv4 `GETATTR` arrives in a 200-byte request claiming a
bitmap of 300 words. The decoder needs 1,200 bytes for it, finds 160 left, and
refuses the request `NFS4ERR_BADXDR` before allocating anything. A `READDIR`
asking for a 4 GiB reply is not refused: the size asked is clamped to the
session's reply size, since a client may ask for more than it gets.

### 5.3 No copies on the data path

`READ` and `WRITE` payloads are not copied by the adapter
([RFC 17 §4.5](rfc-17-vfs.md#4.5%20No%20copies%20on%20the%20data%20path)):

- a `WRITE`'s data is a slice of the receive buffer, lent to the service for the
  call and released when the call returns;
- a `READ`'s reply is encoded as a header followed by the buffer the service
  filled, sent as one gathered write, with the protocol's padding appended, not
  copied in.

Receive and reply buffers come from pools whose sizes are
[RFC 24 §2.3](rfc-24-resources.md#2.3%20The%20pools)'s. A buffer
lent to the service is never reused before the call returns, and a buffer
holding a reply is never reused before the writer has sent it.

Encryption and signing, where a protocol wraps the whole message, necessarily
read every byte; they are the one copy the data path makes, and they write into
the reply buffer rather than a further one.

### 5.4 How codecs are tested

- **Golden vectors.** Each message kind has fixed bytes for fixed fields:
  decoding them gives the fields, encoding the fields gives the bytes. Vectors
  come from the specifications' own examples where they have them, and from
  captures of the reference server (§8) for the rest — never only from the
  codec under test, which would certify its own mistakes.
- **Round trips.** `decode(encode(x)) = x` for generated values. A round trip
  shows only that the two halves agree with each other; it is a check on the
  codec's consistency, never on its correctness, and never stands in for a
  golden vector.
- **Fuzzing.** Every decoder has a fuzz target seeded with its golden vectors
  and the reference captures. A decoder that panics, allocates past the cap, or
  accepts what it then cannot encode, fails.

## 6. Concurrency and the network

### 6.1 One reader, workers, one writer

![One connection: a reader framing requests, up to the flow-control bound of requests running at once, a bounded writer queue sending replies in any order; when the client stops reading, the queue fills, the reader stops reading that connection, and past the write deadline the connection closes](img/rfc20-connection.svg)

Each connection has one reader, which frames and decodes headers, and one
writer, which serialises replies onto the socket. Requests decoded by the
reader run concurrently, up to the connection's flow-control bound (§6.3).
Both protocols allow replies out of order, and the adapter **MAY** send them so;
NFSv4.1 slots and SMB message IDs say which request each reply answers.

The pipeline's work for one request runs on a worker. A worker **MUST NOT**
wait on a client — not for a callback's answer, not for a socket a slow client
does not drain:

- callbacks are sent on the client's connection writer with a deadline, and a
  missed deadline is the client not answering ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks));
- a reply that cannot be written because the client stopped reading waits in
  that connection's writer, not in the worker; once the writer's queue is full,
  the reader stops reading that connection (§6.3), and past the write deadline
  the connection is closed.

A slow client therefore costs its own connection and nothing else.

### 6.2 The writer

The writer owns the socket's write side. It sends whole messages, never
interleaves two, and applies per-message signing or encryption in send order
where the protocol requires an order. It has a bounded queue and a write
deadline (§6.4). A failed write closes the connection.

**Order where the protocol needs it.** When answering a request makes the
service send a callback on the same connection — an SMB lease break to the very
client whose request caused it — the callback is queued ahead of that request's
reply, so the client never sees an answer before the break it depends on. That
is the only order the writer keeps between messages; every other reply goes out
as it completes.

### 6.3 Flow control is the protocol's own

The bound on requests in flight per connection is the protocol's own mechanism
wherever it has one — NFSv4.1 slots ([RFC 21 §3.1](rfc-21-nfs.md#3.1%20Slot%20tables)),
SMB credits ([RFC 22 §3.3](rfc-22-smb.md#3.3%20Sizes%20and%20credits)). Where it has
none — NFSv3, NFSv4.0, the side programs — the adapter imposes a fixed
per-connection cap of **64** requests in flight
([RFC 24 §3.4](rfc-24-resources.md#3.4%20The%20per-connection%20cap%20for%20protocols%20without%20flow%20control)),
and a connection at its cap is simply not read until a request completes, so
TCP's own window pushes back on the client.

Node-wide bounds — bytes held in request and reply buffers, held requests
([RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)), open state — are
[RFC 24](rfc-24-resources.md)'s, and none is an unbounded queue. Receive bytes are charged
at **frame** (§4.1), before a request's body is read: a reader that cannot
reserve them waits, its connection is not read, and no answer is owed
([RFC 24 §3.2](rfc-24-resources.md#3.2%20Receive%3A%20charged%20at%20frame%2C%20and%20the%20reader%20waits)).
Reply bytes are reserved before the service is called
([RFC 24 §3.3](rfc-24-resources.md#3.3%20Reply%3A%20charged%20before%20the%20call%2C%20and%20the%20call%20waits)).
The busy answers are the service's, never the adapter's: a call refused for a
node memory pool — the share cap, open state, a reply reservation's deadline —
returns `ErrDelay` with `DelayResources`, and one past its hold budget
`DelayHoldBudget` ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)).
The error table answers both at once and holds neither
([RFC 21 §5.6](rfc-21-nfs.md#5.6%20Open-state%20and%20capacity%20errors%20on%20the%20wire),
[RFC 22 §6.6](rfc-22-smb.md#6.6%20Service%20errors%20on%20the%20wire)):
`DelayResources` is `NFS4ERR_DELAY`, `NFS3ERR_JUKEBOX`, `NLM4_DENIED_NOLOCKS`
or `STATUS_INSUFFICIENT_RESOURCES`.

> ponytail: a fixed cap of 64 per connection for protocols without flow control.
> One NFSv3 client on one connection is limited to the cap however idle the node
> is. Raise it, or size it from the receive pool, when a single-client NFSv3
> benchmark on the reference box is limited by it.

### 6.4 Deadlines and limits

| Limit | Applies to | On expiry |
| --- | --- | --- |
| request deadline | every service call, carried in its context ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) | the service's answer at the deadline is mapped as any other |
| idle timeout | a connection with no request and no held state | closed |
| write deadline | the writer, per message | connection closed |
| handshake deadline | negotiate and authenticate | connection closed |
| connection limit | per node, `connection_limit`, and a quarter of it per address ([RFC 24 §4.4](rfc-24-resources.md#4.4%20Connections)) | new connections closed at accept |
| drain deadline | draining (§3.1) | remaining requests abandoned |

Each is a setting with a default in [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings),
the same name for every protocol, overridable per protocol where the protocol
RFC says why. A connection holding state the client relies on — an NFSv4.0
callback path, an SMB session with durable opens — is not idle while that
state lives.

### 6.5 Cancellation

A client cancel (SMB `CANCEL`), a closed connection or a drain cancels the
request's context. The service decides what a cancelled context means for a
call already in progress; the adapter only stops waiting for it and drops its
reply. A mutation that the service applied after the cancel is applied: the
adapter **MUST NOT** report to the client that it was not.

### 6.6 Throughput and latency

What makes an adapter fast is mostly what it does not do. In the order of what
each buys:

1. **Many requests per connection.** Both protocols multiplex: requests in
   flight on one connection are matched to their replies by XID, slot or
   message ID. The adapter runs them concurrently up to the flow-control bound
   (§6.3) and replies as each completes. It **MUST NOT** serialise a connection's
   requests beyond what the protocol requires — SMB related operations of one
   compound, and one NFSv4.1 slot's sequence. Order between requests on one file
   is the service's ([RFC 17 §4.8](rfc-17-vfs.md#4.8%20One%20primary%20per%20file)).
2. **Many connections per client.** Clients open several connections for
   bandwidth — NFS clients with several connections per mount, NFSv4.1 trunking
   ([RFC 21 §3.3](rfc-21-nfs.md#3.3%20Trunking%2C%20server%20owner%20and%20scope)), SMB multichannel
   ([RFC 22 §4.2](rfc-22-smb.md#4.2%20Multichannel%20only%20within%20one%20node)). Each connection has its own reader and
   writer, and a session's state shared across its channels is never locked
   across a service call, so a client's connections run on different cores.
3. **No copies of payload** (§5.3). A `READ` reply is the header and the buffer
   the service filled, sent in one gathered write; a `WRITE`'s data is decoded in
   place in the receive buffer.
4. **Buffers pooled by size class.** Receive and reply buffers come from pools
   in four classes, each with the protocol's headers
   ([RFC 24 §3.1](rfc-24-resources.md#3.1%20Buffer%20classes)) — small, 4 KiB, a
   header with attributes; directory, 64 KiB, a directory page; transfer,
   1 MiB; large, 8 MiB, SMB past 1 MiB — so the transfer and large classes are
   drawn only for `READ` and `WRITE`. A buffer goes back to its pool when its
   last borrower, the writer or the service, releases it, never before; a buffer
   larger than the largest class its protocol uses is not pooled. How much
   memory each pool may hold is
   [RFC 24 §2.3](rfc-24-resources.md#2.3%20The%20pools)'s.
5. **Coalesced sends.** When several replies are queued, the writer sends them
   in one gathered write, so a burst of small replies costs one system call. This
   is why Nagle's algorithm is off (§6.7): batching is the writer's, where it
   adds no delay.
6. **No shared lock on the request path.** ID tables are per connection or per
   session, so two connections never contend on one. Node-wide tables — the
   session table, the connection registry — are read without a lock held across
   the service call.
7. **Identity from the security context.** `Identity` is built per request from
   what the handshake established (§4.4), with no directory call per request.

> ponytail: one goroutine per request, bounded by the connection's flow control
> and [RFC 24](rfc-24-resources.md#6.1%20Goroutines)'s node-wide budget; no worker pool. A pool adds a queue and a
> handoff to every request and bounds nothing the budgets do not already bound.
> Add one only when a profile on the reference box shows per-request scheduling
> or allocation limiting the small-operation rate.

> ponytail: one accept loop per listener. Accepting is not a limit at a file
> server's connection counts. Spread accepts over several loops sharing the port
> only if a reconnect storm after a failover is shown queueing in the listen
> backlog.

> decision: an adapter keeps no connection pool. A file server is the passive
> end, and clients choose how many connections to open. The only connection an
> adapter opens is an NFSv4.0 callback path to a client, which it keeps, one per
> client, and reuses. Connections between `protocol` and `storage` nodes belong
> to the shard service ([RFC 15](rfc-15-topology.md)), not to an adapter.

The ceilings above are measured on the reference box
([index](rfc-index.md#Test%20tiers)) by four benchmarks, each per protocol:
small operations (`GETATTR`, `LOOKUP`, SMB `QUERY_INFO`) per second on one
connection and on the node; 1 MiB sequential read and write throughput on one
connection and on four; and the p99 latency of a 4 KiB write with the node at
half its request budget.

### 6.7 TCP

| Option | Setting | Why |
| --- | --- | --- |
| Nagle's algorithm | **off** (`TCP_NODELAY`) on every connection, **MUST** | a request-reply protocol: Nagle holding a small reply until the client's delayed acknowledgement arrives adds up to that timer, tens to hundreds of milliseconds, to a `GETATTR`; batching is the writer's instead (§6.6) |
| keepalive | on, with a probe period shorter than the shortest lease or session timeout it must detect | finds a peer that vanished without closing, so its connection and any back channel it held are closed; the protocol's own lease still decides what happens to state |
| send and receive buffers | left to the kernel's autotuning; **MUST NOT** be fixed by default | fixing them caps the window and switches autotuning off on common kernels; a 1 MiB transfer over a long round trip needs more window than any fixed default |
| user timeout | where the platform offers it, equal to the write deadline (§6.4) | unacknowledged data then fails the connection when the writer would have |
| listen backlog | the system's maximum | a reconnect storm after a failover arrives at once ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)) |
| address reuse | on for listeners | a restarted node binds its port while old connections sit in their closing wait |

Each is a setting in [RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings) with the default above, except Nagle's algorithm,
which is fixed. NFS is served over TCP only
([RFC 21 §2.2](rfc-21-nfs.md#2.2%20TCP%20only)); RDMA, QUIC and SMB Direct are not offered
([RFC 22 §3.1](rfc-22-smb.md#3.1%20Dialects)).

## 7. Boundary tests

These checks prove the adapter only translates. They run per change.

### 7.1 Imports

A test walks the import graph of every adapter package and fails, naming the
package and the import, on anything §2.3 forbids. It is seeded with one
deliberate violation in its own test data and must report it, so a check that
silently matches nothing is caught.

### 7.2 Handlers against a recording service

Each handler is tested against a fake `vfs.Service` that records every call
and returns scripted answers. A handler test asserts three things:

1. the decoded request produced exactly the expected service calls, with the
   expected arguments — at most one that mutates (§4.3);
2. for each scripted answer, including each error, the encoded reply is the
   expected bytes or golden fields;
3. no other call was made.

The recording fake is generated from the service interface, so a new service
method is recorded without hand edits. A handler test **MUST NOT** use a real
store or engine: a handler that works only because a real store did the
checking is exactly the leak this RFC forbids. Semantics are tested once, in
the service's suite ([RFC 17 §8](rfc-17-vfs.md#8.%20Conformance)).

**Example.**

```go
func TestWrite3(t *testing.T) {
	svc := vfstest.Record(t)                          // generated from the interface
	svc.Expect("Write").Return(4096, verf, nil)
	svc.Expect("GetAttr").Return(attrs, nil)          // read-only, for the reply
	rep := runGolden(t, svc, "write3-4k.call")        // golden request bytes
	svc.AssertDone()                                  // exactly these calls, nothing else
	assertGolden(t, rep, "write3-4k.reply")

	svc.Expect("Write").Return(0, nil, vfs.ErrShareViolation)
	rep = runGolden(t, svc, "write3-4k.call")
	assertStatus(t, rep, NFS3ERR_ACCES)               // the table's row, not a guess
}
```

### 7.3 The error tables are total

A test enumerates every error the service declares and asserts each adapter's
table has a row, for every operation and version the table distinguishes. A new
service error fails this test until each protocol says what it means on its
wire.

### 7.4 One mutating call

The recording fake of §7.2 counts mutating calls per handler invocation, and the
handler suite fails any handler that makes more than one. Each handler's test
names the service call it expects, so a handler that silently makes none —
answering from adapter state — is caught too.

### 7.5 Cross-protocol, through both adapters

The cross-protocol rows of RFC 17 §8 are driven once more through both adapters
on the composed node ([index](rfc-index.md#Test%20harness)): an NFS write against
an SMB deny mode, an NFS lock against an SMB lease, a rename over NFS of a
directory marked delete on close over SMB. The service suite proves the rule;
these prove neither adapter routes around it.

## 8. Wire tests

### 8.1 What they are for

Unit tests, golden vectors and the standard conformance suites check what their
authors thought to check. Clients depend on more: the exact status of an edge
case, the order of fields a dissector never looks at, a flag the specification
calls optional and every client sends. The way to find those is to put this
implementation beside a reference server and compare what each says to the same
client.

### 8.2 Differential runs

A wire test is a **script** — a sequence of requests, written against a scripted
client that can send exactly what it is told — run twice: once against a
reference server (Appendix C), once against DittoFS, each on a fresh share with
the same starting tree. Both runs are captured on the wire, decoded by a
protocol dissector into fields, normalised (§8.3), and compared field by field,
request by request.

![A wire test: one script sent to a reference server and to DittoFS, both captured, dissected into fields, normalised, and compared; equal is green, a difference with a recorded entry citing an RFC rule is green, any other difference is red with both captures kept](img/rfc20-wire-diff.svg)

- Scripts drive the protocol, not a kernel client. A kernel client's caching,
  retries and timing differ between runs and hide the reply under test. The
  kernel clients are exercised by the protocol suites
  ([RFC 21 §12](rfc-21-nfs.md#12.%20Conformance), [RFC 22 §17](rfc-22-smb.md#17.%20Conformance)).
- A script may also be a capture of a real client's session, re-sent: allowed
  only for traffic with no security context, since a signed or encrypted
  session cannot be replayed into a new session's keys. Authenticated scripts
  are run live against both servers instead.
- Encryption and signing are switched off where the protocol allows, so the
  dissector sees the messages; one script per protocol runs them on, to compare
  the security exchange itself.

### 8.3 Normalisation

Two correct servers differ in values that mean nothing to a client. Before
comparison each is replaced by a stable token, consistently within a run, so
that "the handle this `LOOKUP` returned is the one that `READ` sent" still
compares:

- times, nonces, verifiers, GUIDs, session and tree IDs, client IDs;
- file handles, `FileId`s, stateids, `READDIR` cookies;
- numeric file IDs and `fsid`s;
- signatures, encrypted payloads, and the lengths of fields that carry only
  these;
- server names, domain names and scopes.

Everything else is compared: statuses, flags, which optional fields are
present, attribute sets and values, sizes, counts and offsets, credit grants,
the order of entries a reply lists. A normalisation rule **MUST** name a field,
never a message: masking a whole reply to make it match is masking the test.

**Example.** The same SMB `CREATE` reply, from each server:

```text
                        reference                    DittoFS                       normalised
Status                  SUCCESS                      SUCCESS                       compared: equal
FileId                  0x00000a3c:0x00007f11        0x00000001:0x0000001d         FILE#1 = FILE#1
CreationTime            2026-10-15T10:03:11.402Z     2026-10-15T10:03:11.981Z      TIME#1 = TIME#1
MaximalAccess           0x001f01ff                   0x001f01ff                    compared: equal
Lease state             RWH                          RW                            compared: DIFFERS
```

The IDs and times become tokens and compare equal. The lease state is
compared and differs: the run is red, unless the divergence list has an entry
for this script and field citing the rule that grants `RW` here.

### 8.4 Divergences are recorded decisions

A difference that survives normalisation is one of two things:

1. **a bug**, and the run is red; or
2. **a recorded divergence** — an entry naming the script, the field, the two
   values, and the protocol RFC rule that makes this implementation differ: no
   short names generated, no UDP, no courtesy clients, no referrals. An entry
   with no rule to cite is not accepted.

The divergence list follows the known-failure rules of the
[index](rfc-index.md#Test%20tiers): an entry excuses one named field in one named
script, a difference it does not name still fails, and an entry that no longer
differs fails until it is removed.

Where the reference server itself departs from the specification, the
specification wins and the entry says so; where two reference servers disagree
(Appendix C), the one clients are known to depend on wins, and the entry names
the client.

### 8.5 What wire tests cover, and when they run

Each protocol keeps a script per area its RFC owns — negotiation and security
setup, opens and caching grants, locks, directory listing at buffer edges,
attribute and information queries, errors for every status its error table
produces, and the operations it answers not supported. A new status in an error
table needs a script that provokes it.

Wire tests run nightly, against the reference servers of Appendix C, and on
demand against a single script. A red run keeps both captures and the
normalised diff as artifacts; a run that could not reach a reference server is
a failure, never a skip.

The reference captures are also the source of §5.4's golden vectors: a field
the reference sends that a codec decodes wrongly is found by the codec's own
tests, per change, without waiting for the nightly run.

## 9. Invariants

| # | Invariant |
| --- | --- |
| A1 | An adapter imports only what §2.3 lists, and holds no metadata view, open state, engine or routing. |
| A2 | Nothing an adapter holds is written to the metadata store or replicated; everything a client can present back after its connection ends is held below the service. |
| A3 | An adapter caches nothing the service returned beyond the request that read it. |
| A4 | Every request passes the pipeline's stages in §4.1's order, through one implementation shared by every adapter. |
| A5 | A handler makes at most one service call that changes state, and decides nothing on a file's attributes, mode or ACL. |
| A6 | A compound runs as a sequence of handlers, each admitted separately, with no lock held across them. |
| A7 | Every service error has a row in every adapter's error table; an error with no row is answered server fault and logged at error. |
| A8 | Decoders allocate nothing a length field claims before checking it against the bytes remaining and the protocol's cap. |
| A9 | The adapter does not copy `READ` or `WRITE` payloads, apart from encryption and signing. |
| A10 | No worker waits on a client; a slow client blocks only its own connection, and a connection at its flow-control bound is not read. |
| A11 | A drained or cancelled request is never answered with a final error the service may contradict. |
| A12 | The adapter maps no identity, applies no squash and grants no privilege; it hands the service what the handshake proved. |
| A13 | Every wire divergence from the reference server that survives normalisation is a red run or a recorded entry citing a protocol RFC rule. |
| A14 | Nagle's algorithm is off on every connection, and the socket buffers are left to the kernel unless a setting fixes them. |
| A15 | Every operation-table row fills every column, and no handler checks a precondition the table carries. |

## 10. Observability

Metric names follow [RFC 25 §2](rfc-25-observability.md#2.%20Metrics) and are
shown without the `dittofs_` prefix the exporter adds
([RFC 25 §2.1](rfc-25-observability.md#2.1%20Names)). Operation latency is
the service's ([RFC 17 §7](rfc-17-vfs.md#7.%20Observability)); the adapter
measures only what happens before and after the service call.

| Answers | Metric | Type |
| --- | --- | --- |
| How many connections, per protocol and state? | `adapter_connections{protocol, state}` | gauge |
| How often are connections refused or closed, and why? | `adapter_connection_closes_total{protocol, reason}` | counter |
| How much is in flight per protocol? | `adapter_requests_in_flight{protocol}` | gauge |
| How often is a request refused before the service? | `adapter_refusals_total{protocol, stage}` | counter |
| How often are replays answered from the cache? | `adapter_replay_hits_total{protocol}` | counter |
| How long are requests on the wire around the service call? | `adapter_wire_seconds{protocol, stage}` | histogram |
| How often does a handler fault? | `adapter_handler_panics_total{protocol, op}` | counter |
| How often does an error have no row? | `adapter_unmapped_errors_total{protocol}` | counter |
| Bytes in and out | `adapter_bytes_total{protocol, direction}` | counter |

`adapter_wire_seconds` takes the `request` bucket set
([RFC 25 §2.6](rfc-25-observability.md#2.6%20Histogram%20buckets)). The
`reason` of `adapter_connection_closes_total` is a closed set that includes
[RFC 24 §10](rfc-24-resources.md#10.%20Observability)'s `pool`, `limit` and
`per_address` for connections closed at accept. No metric carries a client,
principal, share or path label.

`adapter_unmapped_errors_total` and `adapter_handler_panics_total` are zero in
a healthy node; any increase is a bug, and alerts as one.

## 11. Open questions

1. **Generated SMB codecs.** SMB2 has no machine-readable definition. Whether to
   write one, from MS-SMB2's structure tables, and generate the codecs from it
   is unmeasured against the cost of hand-written codecs with golden vectors.
2. **A reference SMB server in nightly runs.** The reference whose behaviour
   Windows clients depend on is a licensed server; whether the nightly tier can
   host one, or runs the second reference of Appendix C and a weekly run against
   the first, is open.
3. **Where the compound runner meets batching.** If RFC 17 §9 adds a batch call,
   the compound runner is where it is used; whether the runner then still
   admits each operation separately (A6) is open with it.

## Appendix A — prior art

*Non-normative.*

- **Samba** — the SMB protocol layer (`smbd`'s request processing) calls a VFS
  layer for every file operation, and keeps sessions, tree connects and
  `FileId` tables in its own memory; its `smbtorture` suite is a long-lived
  example of protocol-level tests separate from file-system tests.
- **NFS-Ganesha** — one dispatcher for every NFS program and version, request
  queues per transport with a fixed worker pool, the protocol layer calling the
  FSAL and nothing else.
- **The Linux NFS server** — the `svc` layer handles transport, record marking
  and the duplicate request cache, and dispatches to per-version procedure
  tables. Its NFSv4 compound processing reads a per-operation table of flags —
  allowed without a filehandle, modifies state, cacheable, handles wrong
  security — and a per-operation reply-size estimate, checked before each
  operation runs against the session's reply limit: the model for §4.2's columns
  and §4.3's reply budget. A transport is served by one thread at a time, so
  requests on one connection are read in order and run in parallel. Its newer
  XDR codecs are generated from the specifications' XDR.
- **Samba's SMB2 server** — one dispatch table whose rows say whether a command
  needs a session, a tree connect or a file ID, and whether it changes state;
  checked before the command's handler runs. Interim responses and re-driven
  requests are a mechanism of the request layer, not of each command.
- **Wireshark and its dissectors** — the de facto field-level description of
  both protocols, and what makes the differential runs of §8 cheap to build.

## Appendix B — where the current code differs

*Non-normative.* The code on the branch this RFC was written against, by section.

| § | This RFC | The code today |
| --- | --- | --- |
| §2.3, §7.1 | adapters import only the service | handlers reach the metadata service directly at about 70 call sites, the lock manager in 15 files, the block store in about 30, through runtime interfaces that return the concrete metadata service; no import check exists for adapters (the block packages have one, the pattern to copy) |
| §2.4, RFC 17 §5.1 | the service orders a write | the prepare-write, block-store write, commit-write, flush sequence is repeated in the NFSv3, NFSv4 and SMB write handlers and in SMB copychunk and sparse handlers, through shared write helpers |
| §2.4 | conflicts are the service's | SMB handlers check locks for I/O in six places and lock directly; NFSv3 handlers break caching grants themselves before write, read, remove and rename; NFSv4 locks through its own state manager |
| §2.4, RFC 17 §4.9 | admission is the service's | share admission runs in four places — NFSv3 dispatch, the NFSv4 auth-context builder, MOUNT, SMB tree connect — and share-permission policy is written twice |
| §2.3 | one shared translation | ACL translation, access-mask conversion and attribute conversion are each written per protocol, access masks twice within NFS |
| §3.1 | one lifecycle | one accept-and-drain shell is shared; each protocol's stop sequence and side programs are hand-ordered per adapter |
| §4.1 | one pipeline | two dispatch stacks with nothing shared above the socket; the connection loops are near-identical copies |
| §4.2 | precondition columns | SMB's table carries needs-session and needs-tree; NFSv4 looks operations up in three per-version maps with a per-share block list; NFSv3 gates in its dispatcher |
| §4.3 | reply budget before each operation | the NFSv4.1 compound enforces the reply size; the per-compound cap is 128 operations |
| §4.4 | no identity mapping in adapters | squashing and identity mapping run in the NFS adapter; SMB resolves SIDs through its own paths |
| §4.5 | one total table per protocol | three status tables (NFSv3, NFSv4, SMB), no totality test |
| §5.1 | codecs from one place | NFS codecs are hand-written, with a reflection-based XDR library still used for the RPC envelope and MOUNT; SMB uses one encoder in most handlers and raw byte order in a few |
| §5.3, §6.6 | no payload copy, gathered writes | replies are copied into a fresh buffer (NFS) or a pooled frame (SMB); no gathered writes; a three-class buffer pool exists |
| §5.4 | golden vectors and fuzzing | one fuzz target (RPC auth); no golden vectors for handler messages |
| §6.1, §6.3 | flow control is the protocol's | one goroutine per connection and one per request, bounded by a per-connection cap of 100 for both protocols; slots (64) and credits exist; no node-wide in-flight bound |
| §6.7 | TCP options | Nagle off and keepalive at 15 s already; socket buffers untouched |
| §7.2 | handlers against a recording service | handler tests run on real in-memory stores and engines; four fakes in total |
| §8 | differential wire tests | a capture corpus with a normaliser exists for Kerberos, LDAP, NFSv4 ACLs and SMB security descriptors only; standard suites run nightly; the interop playbook is manual |
| RFC 21 §2.2 | every NFS-family program over TCP only | NFS itself is TCP only, but MOUNT, NLM and NSM are also served over UDP |

## Appendix C — the reference profile for wire tests

*Non-normative; a profile.*

| Protocol | First reference | Second reference | Why |
| --- | --- | --- | --- |
| SMB | Windows Server, current long-term release | Samba, current release | Windows clients are written against Windows Server; Samba is what a nightly run can host freely, and departs from Windows in places the divergence list then names |
| NFS | the Linux kernel NFS server | NFS-Ganesha | the kernel server is what Linux and macOS clients are tested against; Ganesha is a second userspace implementation |

Capture with `tcpdump`, decode with `tshark`'s dissectors into fields (its JSON
output), normalise and compare with a small matcher; on a non-standard port
`tshark` needs the port hinted to its dissector. The interop playbook in the
debugging guide is the manual form of the same run.
