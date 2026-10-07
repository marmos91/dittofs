---
rfc: 14
title: "RFC 14 — open state and locks"
component: open state
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
aliases:
  - RFC 14
tags:
  - rfc
---
# RFC 14 — open state and locks

**Status:** draft. [§15](#15.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone changing how opens, byte-range locks, deny modes, caching
grants or change watches are granted, conflicted, recalled, recovered or
persisted — and anyone writing a protocol adapter that exposes them.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

File protocols do more than name files and move bytes. A client opens a file
and may forbid others to open it while it does; it locks byte ranges; it caches
reads and writes under a promise from the server that nobody else is using the
file; it asks to hear when a directory changes. This RFC owns all of that
state: who holds each file open, what each holder may do, what the server has
promised, and what happens to it when a client, a server or a node goes away.

In outline: clients reach DittoFS over NFS (Network File System) or SMB (Server
Message Block); writes land in a local journal and are later uploaded to an
S3 (Simple Storage Service) bucket ([RFC 0](rfc-0-data-lifecycle.md)). Every
file has one node that serves it, its **primary** — on a single-node install,
N1. Open state lives there, in one table per file, beside the file's I/O.

### The problem, in one example

alice-pc holds `profiles/alice/ODFC_alice.vhdx` open all day. In this example
its open asks for read and write, denies other writers, and carries an SMB
lease — a caching grant — for read, write and handle caching under a lease key
alice-pc chose.

1. **08:30, sign-in.** N1 records the open, its deny mode and the lease in the
   file's table, in memory. An open of a file that has a name is not written to
   the metadata store. Under the lease, alice-pc caches reads, buffers writes and
   takes byte-range locks locally.
2. **10:15, a network blip.** alice-pc's connection drops. A plain open would
   close; this one was opened durable, so it stays, with its deny mode and its
   lease, for the timeout its create negotiated. alice-pc reconnects within it.
   The reconnect must match on client, user, the create's ID where it has one,
   and the lease key; anything else is refused as if the open did not exist.
3. **14:00, a second sign-in.** alice signs in on a second desktop, which opens
   the same file for writing. The deny mode held by alice-pc refuses it with a
   sharing violation. It is never downgraded to a read-only open the client did
   not ask for.
4. **The take-over case.** Profile software re-attaching a disk from another
   machine sends the same *app instance* ID as the first open. Then alice-pc's
   open is closed first — its locks, deny mode and lease released — and the new
   one granted, unless its app-instance version is not higher.
5. **Had the first open shared write access**, the second open would still
   conflict with alice-pc's write lease. The server breaks the lease: alice-pc
   sends its buffered writes as ordinary writes and acknowledges. Meanwhile the
   second desktop is told to retry rather than holding a server thread, and if
   alice-pc never answers, the lease is revoked at a deadline.
6. **N1 restarts.** The in-memory table is gone. Without a rule, the second
   desktop could open the file before alice-pc comes back, and both would think
   they hold it. Instead N1 runs a **grace period**: for at least one lease
   period it refuses every new open, lock or grant, and lets clients its durable
   records name reclaim what they held. alice-pc reclaims its durable open;
   grace then ends on its own, early once every recorded client has reclaimed.

Without locks, a deny mode or a lease, two clients writing one file get exactly
this and nothing more: each write request is applied whole; where two overlap,
the one that reached the primary later wins, byte by byte; a caching client
sees the other's writes only through NFS close-to-open checks and SMB lease
breaks.

```text
 alice-pc (SMB)          second desktop (SMB)          build01 (NFS)
 open: read+write,       open: write, same file        READ, WRITE, LOCK
 deny write, lease L1         │                            │
      └──────────────────────►│◄───────────────────────────┘
                              ▼
              filesystem service routes to the file's primary
                              ▼
 ┌──── N1, primary: the table for ODFC_alice.vhdx (in memory) ────┐
 │ client alice-pc     open o1: read+write, deny write, durable   │
 │ grant  L1: read+write+handle, epoch 3          locks: none     │
 │ second desktop's open vs o1's deny mode ──► sharing violation  │
 │ same app instance? ──► close o1 and its lease, then grant      │
 │ the same process runs the file's I/O: nothing lands between    │
 │ a conflict check and the write it admits                       │
 └────────────────────────────────────────────────────────────────┘
   restart ──► grace: only reclaims until holders are back
```

### The words you need

- **Open** and **deny mode**: one open of one file, with the access it was
  granted and the access it denies to later opens; a deny mode is checked once,
  when an open is granted ([§2.2](#2.2%20Open), [§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open)).
- **Byte-range lock**: a lock on a range of a file, held by a lock owner.
  SMB locks are mandatory, NFS locks advisory ([§2.3](#2.3%20Lock)).
- **Caching grant**: the server's promise that nobody else is using the file —
  an NFS delegation, an SMB oplock or lease — taken back by a recall within a
  deadline ([§5](#5.%20Caching%20grants)).
- **Client lease**: how long the server keeps a silent client's state. "Lease"
  also names SMB's caching grant and a node's lease
  ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch));
  the three are unrelated ([§4.1](#4.1%20A%20client%20lease)).
- **Grace period** and **reclaim**: after a primary loses open state, the
  window in which only former holders may take state, by reclaiming it
  ([§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe)).
- **Durable** and **persistent open**: an SMB open kept across a disconnect for
  its timeout; a persistent one also survives a failover, because it is written
  down ([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)).
- **Delete pending**: SMB's delete on close. The name stays until the last
  open closes; new opens are refused meanwhile ([§9.4](#9.4%20Delete%20on%20close)).

### What this RFC promises

- One table per file, at the file's primary, seen by both protocols: an SMB
  deny mode or lock refuses an NFS request it forbids, and the reverse where
  [§7](#7.%20Conflicts%20across%20protocols) says so.
- No client waits forever on another: a recall ends at its deadline, by
  acknowledgement or revocation, and a client silent for its whole lease loses
  all its state at once.
- After open state is lost, nobody takes what another client held: grace admits
  only reclaims, and grace ends without an operator.
- An unlinked file that is still open stays readable until its last close, and
  a crash does not release it early. Opening and closing a file that has a name
  writes no open record, unless the open is persistent.
- Writers that do not coordinate get whole writes, ordered byte by byte by
  arrival at the primary, and nothing more; applications that need more lock.

### How the rest is organised

[§2](#2.%20The%20entities) defines the entities: client, open, lock, caching
grant, watch, layout and copy. [§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary)
is the one rule everything rests on: one table per file, at one primary.
[§4](#4.%20Client%20leases%2C%20grace%20and%20reclaim) covers client leases,
grace and reclaim, NLM (Network Lock Manager) locks included;
[§5](#5.%20Caching%20grants) caching grants; [§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open)
deny modes; [§7](#7.%20Conflicts%20across%20protocols) conflicts across
protocols and writers that do not coordinate. [§8](#8.%20What%20is%20durable)
says what is written down; [§9](#9.%20Open%20state%20and%20the%20life%20of%20a%20file)
how open state keeps a file alive and how delete on close works;
[§10](#10.%20Shard%20placement) what happens when a file's primary changes.
[§12](#12.%20Invariants) lists the invariants. On a first read, skip §2.5–§2.7,
§4.4–§4.5, §10, §11 and §13 onward.

## 1. Purpose

A file protocol does not only name files and move bytes. Clients open files,
deny others access while they hold them, lock byte ranges, cache under promises
the server made, and ask to be told when a directory changes. This document owns
that state and answers:

> **Who holds this file open, what may they do with it, what have we promised
> them, and what happens to all of it when a client, a server or a primary goes
> away?**

It is one component because every one of these kinds of state is consulted by
the others: an open is refused by a deny mode, a lock is released by its open's
close, a grant is broken by an open, a delete waits for the last close, and a
client's lease ending releases them all.

### 1.1 Non-goals

This component **MUST NOT**:

- decide names, permissions or when a file stops existing — [RFC 7](rfc-7-namespace-metadata.md)'s. It
  tells RFC 7 that a file is open ([§9](#9.%20Open%20state%20and%20the%20life%20of%20a%20file)); RFC 7 decides the rest;
- hold content or know where it is. A lock is a statement about who may act on
  a file, not about where its bytes are ([§9.3](#9.3%20Locks%20do%20not%20pin%20bytes));
- define shard records, node leases or primary epochs — [RFC 11](rfc-11-ownership.md)'s. Client
  state and shard placement share no records ([RFC 11 §7](rfc-11-ownership.md#7.%20Protocol%20state));
- encode a wire protocol: how a stateid, an SMB FileId, a lease key or a reply
  is spelled is the adapters' ([RFC 17](rfc-17-vfs.md)). A protocol value that must outlive a
  connection, a primary or a restart — a lease key, a create GUID, an app
  instance, a lock sequence — is an opaque field here, compared and never
  interpreted, because an adapter cannot carry it across a failover.

## 2. The entities

The entities below live in the metadata package ([RFC 16](rfc-16-metadata-store.md)). Signatures are
indicative; the rules around them are normative.

### 2.1 Client

```go
// Client is one protocol client instance and its lease. When the lease
// expires, every Open, Lock, CachingGrant and Watch it holds is released in
// one step (§4.3).
type Client struct {
	ID       ClientID
	Protocol protocol.Kind
	Expires  time.Time // zero for an NLM host, which has no lease (§4.5)
	Shards   []ShardID // shards it has held state in; bounded (§8)
	Notify   []byte    // NLM only: where its restart notification goes (§4.5); opaque here
}
```

`ClientID` names one client instance as its protocol defines it: NFSv4's client
ID from `EXCHANGE_ID` or `SETCLIENTID` (a machine plus a boot verifier), SMB's
`ClientGuid`, and for NFSv3 locks the NLM host name with the host's NSM state
number. A client that reboots is a new `ClientID`, which is how its stale state
is recognised and released.

**The server's identity is the installation's.** Every `protocol` node of an
installation reaches the same state at the same primaries ([§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary)), so to a client
they are one server. The NFSv4.1 server owner's major ID and the server scope
**MUST** be the installation's identity ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)) on every node, and the
minor ID **MUST** be the node's own: a client may then use one client ID through
several nodes, and does not try to share a session between nodes, whose session
state each node holds alone. Client IDs are minted unique across the
installation and never reused, and a client record is durable ([§8](#8.%20What%20is%20durable)), so a
client ID stays valid across a `protocol` node's restart; what a loss
invalidates is the state in the shards that lost it ([§4.4](#4.4%20Grace%20is%20per%20shard)), never the client
ID itself.

`Shards` lists every shard the client has held state in. A reclaim in a
shard the list does not name is refused ([§4.4](#4.4%20Grace%20is%20per%20shard)): a client that held state
only elsewhere cannot take state in this shard first.

`protocol.Kind` comes from one small package that the adapters and the metadata
layer both import. It **MUST NOT** come from an adapter package: the metadata
layer would then import an adapter.

### 2.2 Open

```go
// Open is one open of one file. An open of an unlinked file keeps it alive (§9).
type Open struct {
	ID            OpenID // unique across the installation, never reused
	Client        ClientID
	Principal     Principal // who opened it; a reconnect by anyone else is refused (§8.1)
	File          FileID
	Access        Access     // granted: read, write, delete
	Deny          Access     // deny mode: SMB share access; NFSv4 deny, which has no delete bit
	Durability    Durability // none, durable, persistent (§8)
	DeleteOnClose bool       // its close makes the file delete pending (§9.4)
	Grant         GrantID    // the SMB lease or oplock covering it; zero for NFS (§2.4)
	Suspended     TimeMask   // times a write through this open leaves alone (§2.2); zero: none
	Via           FileID     // the directory it was opened through (§9.5)

	// SMB durable, resilient and persistent opens (§8.1); zero for NFS.
	CreateGUID  [16]byte      // identifies the CREATE: replay and reconnect matching
	AppInstance [16]byte      // zero: none
	AppVersion  [2]uint64     // the app instance version, high and low; zero: none
	Timeout     time.Duration // how long it is kept once its client disconnects
	LockSeq     [64]uint8     // lock sequence per index: a valid bit and a 4-bit sequence
}
```

The name is both protocols' own: NFS's `OPEN` and its open stateid, the
"Open" of the SMB2 specification with its FileId. "Handle" is not used: in this
set it means the file handle ([RFC 7 §6](rfc-7-namespace-metadata.md#6.%20Handles)). SMB's durable and persistent handles
are properties of an open, so they are a field here, not an entity. NFSv3 has no
opens; its I/O runs against an anonymous open that holds no deny mode and keeps
nothing alive.

`OpenID` is unique across the installation, so an SMB adapter can carry it as
the persistent half of the SMB FileId and find the open again after a
reconnect to another node.

**A time can be suspended per open.** SMB lets a client set a time, in a
set-information request on an open, to a sentinel: -1 suspends the automatic
update of that time for I/O through this open, -2 resumes it. `Suspended` holds
the times so suspended — `Modify`, `Change`, `Access`. A write or read through
an open **MUST NOT** advance a time its `Suspended` names; the same I/O through
any other open, and every explicit set, still does. The filesystem service
reads `Suspended` when it admits the write and passes the times to leave alone
with it, so the existence commit that records the write leaves them unchanged
([RFC 17 §5.1](rfc-17-vfs.md#5.1%20Write)). `Suspended` is volatile with the open, and durable only
when the open is persistent ([§8](#8.%20What%20is%20durable)); a lost suspension costs only a time
the client asked not to move.

### 2.3 Lock

```go
// Lock is one byte-range lock, held against the file (never a handle or a name).
type Lock struct {
	Owner     LockOwner
	Open      OpenID // zero for an NLM lock: NFSv3 has no opens
	Range     ByteRange
	Exclusive bool
}

// LockOwner is who holds a lock. For NFSv4 and NLM, one owner's locks never
// conflict with each other: a lock over its own range replaces, merges or
// splits it. SMB locks stack, and an exclusive SMB lock conflicts with any
// overlapping lock, its own open's included.
type LockOwner struct {
	Client ClientID
	Owner  []byte // opaque; see below
}
```

The owner is each protocol's own:

| Protocol | Owner | So |
| --- | --- | --- |
| NFSv4 | the client ID and the lock-owner the client names | two lock-owners of one client conflict |
| NLM | the host's `ClientID` and the `svid` it sends | two processes on one host conflict; one process's locks merge |
| SMB | the open: `Owner` is the `OpenID` | two opens of one client conflict, as SMB requires |

An NLM lock names no open: it is held by its owner alone and released by
unlock, by its host's restart ([§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)) or by revocation. Every other lock
names the open it was taken under, and closing that open releases it.

### 2.4 CachingGrant

```go
// CachingGrant is what NFS calls a delegation and SMB an oplock or a lease: a
// promise that no other client is using the file, recalled on conflict within
// a bounded time (§5).
type CachingGrant struct {
	ID        GrantID
	Client    ClientID
	File      FileID
	Kind      GrantKind // read, write, handle, in any combination
	Key       LeaseKey  // SMB lease key, 128 bits, chosen by the client; zero for an oplock or a delegation
	ParentKey LeaseKey  // SMB lease v2: the lease key of the client's lease on the parent directory; zero: none
	Epoch     uint16    // SMB lease v2: raised on every change of Kind; zero otherwise
	Breaking  GrantKind // the kind a break in flight goes to; meaningful while Recall is set
	Recall    time.Time // zero unless a recall is in flight
}
```

**One grant per (client, lease key, file).** An SMB lease is named by its key,
which the client chooses and sends with every open it wants covered. Every open
of one file by one client under one key is covered by the same grant, and
those opens never break it: that is what a lease is for ([§5.1](#5.1%20What%20a%20grant%20is)). The same
client under a different key holds a different grant, which conflicts as
another client's would. A key already naming a grant on another file is
refused. An oplock covers exactly the one open that asked for it. An NFS
delegation is held by the client and covers none of its opens: it ends by
`DELEGRETURN` or recall, not by close.

**The epoch orders a lease's changes.** Every change of a v2 lease's `Kind` —
a break, an upgrade on a later open — **MUST** raise `Epoch`, and the new value
is sent with the change, so a client that receives two breaks out of order
discards the older. A v1 lease and an oplock have none.

**What is persisted.** A grant is volatile ([§8](#8.%20What%20is%20durable)). A lease covering a
persistent open is written with it, key, parent key, kind and epoch, so the
reconnected open finds its lease as it left it.

### 2.5 Watch

```go
// Watch is a client's request to hear about changes under a directory: SMB
// CHANGE_NOTIFY and NFSv4.1 directory notifications. It ends with its
// client's lease and is never reclaimed (§8).
// A change is delivered only if the watch's client may traverse to it
// (RFC 17's callbacks).
type Watch struct {
	ID        WatchID
	Client    ClientID
	Dir       FileID
	Recursive bool
	Filter    ChangeMask // names, attributes, size, times, security, streams
}
```

### 2.6 Layout

A **layout** is a pNFS client's grant to send I/O for a byte range of a file
straight to the data servers it names ([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS)). It is open state like
the others: held by the client under an open, in the file's table, released
with the client's lease, recalled on conflict. It also records the (shard,
epoch) it was granted under, and is recalled when that shard changes primary or
the file moves to another shard ([§10](#10.%20Shard%20placement)).

### 2.7 Copy

```go
// Copy is one server-side copy running after its request was answered: an
// NFSv4.2 COPY with an asynchronous reply. Held in the destination file's
// table. An SMB copychunk completes within its request and has none.
type Copy struct {
	ID             CopyID // what the copy stateid names
	Client         ClientID
	Src, Dst       FileID
	SrcOff, DstOff int64
	Length         int64
	Done           int64     // bytes copied so far: OFFLOAD_STATUS's answer
	State          CopyState // running, done, failed, cancelled
}
```

The copy reports its end to the client by `CB_OFFLOAD` and is forgotten once the
client has heard it or its lease has expired. A cancel (`OFFLOAD_CANCEL`) stops
it at the next batch; the bytes already copied stay. A copy is volatile, and a
copy that is lost is unknown, never done: a status or cancel for it is answered
`ErrNoCopy`, and the client runs the copy again. Copying the same bytes to the
same place twice is harmless, so a partial copy left behind needs no cleanup.

> decision: an asynchronous copy is not persisted. Persisting it costs a write
> at start and at every progress mark, to spare a client re-running a copy only
> after a failover or restart that landed mid-copy; the client already recovers
> from it. Persist it, with its progress, if copies in a workload run long
> enough that failovers routinely land inside them.

### 2.8 NFSv4.0 owner sequences

```go
// OwnerSeq is the next sequence number an NFSv4.0 open-owner or lock-owner
// must send. NFSv4.1 and later use sessions and have none.
type OwnerSeq struct {
	Owner LockOwner // client ID and the opaque owner the client names
	Kind  OwnerKind // open-owner or lock-owner
	Next  uint32    // the seqid the owner's next sequenced request must carry
}
```

An NFSv4.0 owner numbers its sequenced requests — `OPEN`, `OPEN_CONFIRM`,
`OPEN_DOWNGRADE`, `CLOSE` for an open-owner; `LOCK` and `LOCKU` for a
lock-owner — and the server refuses one out of sequence. The primary that holds
the owner's `OwnerSeq` checks and advances it in the step that applies the
request, so a sequence check and the state change it admits never separate.

**Where it is held.** An owner's `OwnerSeq` is held at the primary of its
**home shard**: the shard of the file of its first sequenced request. A
sequenced request on a file in another shard checks and advances the sequence
at the home primary first, as one call, and only then runs at its file's
primary. A lock-owner's locks are almost always on one file, so its home shard
is that file's and the extra call does not arise.

**What is the adapter's.** The reply to the owner's last request, which
NFSv4.0 replays when the same seqid arrives again, is cached by the adapter
that answered it, not here. It is best-effort: after the loss of that protocol
node a replayed request finds no cached reply and is answered
`NFS4ERR_BAD_SEQID` if it does not advance the sequence.

`OwnerSeq` is volatile, like the opens and locks it sequences. A home primary
that is lost loses it; in the grace that follows, an owner with no `OwnerSeq`
is new, and its first sequenced request sets `Next`, as for any new owner.

## 3. One table per file, at one primary

| State | Granted by | Conflicts with | Lifetime |
| --- | --- | --- | --- |
| **open** | an open | a deny mode ([§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open)) | until close |
| **deny mode** | an open | an open asking for denied access | until close |
| **byte-range lock** | a lock request | an overlapping lock of another owner ([§2.3](#2.3%20Lock)); I/O across protocols ([§7](#7.%20Conflicts%20across%20protocols)) | until unlock, close, or lease expiry; an NLM lock until unlock or its host's restart |
| **delete pending** | an open's close with `DeleteOnClose`, or a set-disposition | every new open; a rename of the file ([§9.4](#9.4%20Delete%20on%20close)) | until cleared, or the last close removes the name |
| **copy** | an asynchronous copy request | nothing: its writes are ordinary writes | until reported, cancelled, or lease expiry |
| **caching grant** | the server, on an open | any conflicting access by another client ([§5](#5.%20Caching%20grants)) | until recalled, revoked, returned or expired |
| **watch** | a watch request | nothing | until cancelled or lease expiry |
| **layout** | a layout request, under an open | a conflicting open, lock or deny mode; a primary change of its shard, or a move of its file | until returned, recalled, revoked or lease expiry |

All of it is held against a **file**, never a name or a handle. A rename does
not disturb it, and two hard links to one file are one lockable object. Keying
by handle is the subtler error: one client may hold several handles to one file,
and a lock one handle can see and another cannot is a lock the same client can
take twice.

All of it is held **in one table per file, at the primary of the file's shard**
([RFC 15 §3](rfc-15-topology.md#3.%20One%20primary%20per%20shard)). A file is never split across shards. That
primary also runs the file's I/O, so a conflict check and the I/O it admits run
in one process under one epoch, and no grant lands between them. An implementation **MUST NOT** hold any of it in an adapter,
where the other adapter cannot see it: a lock one protocol grants and the other
does not observe is not a lock. Every adapter reaches the table through the
filesystem service ([RFC 17](rfc-17-vfs.md)), which routes to the primary.

## 4. Client leases, grace and reclaim

### 4.1 A client lease

A **client lease** is how long the server keeps a client's state while the
client is silent. Every request from the client renews it (NFSv4 `SEQUENCE` or
`RENEW`); a client with nothing to do sends an empty renewal. A client silent
for a whole lease period is treated as gone. Without that, a laptop that sleeps
holding a lock blocks every other client forever. SMB's durable-handle timeout
plays the same part for SMB clients.

The lease period is the protocol's, fixed, not a setting ([RFC 13](rfc-13-configuration.md)): NFSv4 clients
are given a 90 s lease, advertised in the `lease_time` attribute; an SMB durable
handle is kept for the timeout its create request negotiates, within the bounds
the protocol sets (MS-SMB2 3.3.5.9.10).

"Lease" names two unrelated things in this set: the client lease, here, and the
**node lease** of [RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch), which, with the shard record, decides which node may serve a shard. SMB
also calls a caching grant a "lease" ([§5](#5.%20Caching%20grants)). The three share no records.

### 4.2 Grace makes volatile state safe

Open state other than what [§8](#8.%20What%20is%20durable) makes durable **MAY** be held in memory and lost
on restart or primary change. It **MUST NOT** be the reason an operation blocks on
a holder that no longer exists.

A primary that has lost open state **MUST** then run a **grace period** of at
least one lease period. During it, it **MUST** refuse every request for open
state that is not a reclaim — a new lock, a new open, a deny mode, a grant, a
layout — whether or not it appears to conflict: the state it would be checked
against is what was lost, so a conflict cannot be decided. For the same reason
it **MUST** refuse, with `ErrGrace`, every read or write through an anonymous
or unreclaimed open that a lost mandatory lock or deny mode might have
forbidden; without that, an NFSv3 write lands in a range an SMB client is about
to reclaim a lock on. A client reclaiming what it held
then finds it available, and a client that did not hold it cannot take it first.
Without that window, two clients that were correctly serialised before the
restart are both granted the same lock after it, and neither is told.
Open-state leases are extended by the grace period, so a client is not expired
for time the server spent restarting.

A reclaim is accepted only from a client the durable client record ([§8](#8.%20What%20is%20durable))
shows held state before the loss. Without that record, every reclaim **MUST**
be refused.

The window **MUST** end on its own ([RFC 0 §10.2](rfc-0-data-lifecycle.md#10.2%20No%20state%20requires%20intervention%20to%20leave)). A server that will not leave a
grace period until an operator acts has replaced one wedge with another. It
**SHOULD** end early, once every client whose record names the shard has
finished reclaiming — sent `RECLAIM_COMPLETE`, or for SMB reconnected its durable
opens — or has expired: nothing is left that a new request could take first.

### 4.3 An expired lease releases everything it held, everywhere

When a client's lease expires or its state is revoked, every open, deny mode,
lock, grant and watch it held **MUST** be released, in every view that records
it, in the same step. State one protocol dropped and another still counts
refuses conflicting requests against a holder that no longer exists, until a
restart.

### 4.4 Grace is per shard

Every open-state operation reaches the primary of the file's shard
([§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary)), so a grace period is scoped to the shards whose primary lost state, and
the rest of the cluster keeps granting. Only a failover loses state: a handover
or a batch move of files hands the table over and runs no grace ([§10](#10.%20Shard%20placement)).

**A client must be told to reclaim.** A client whose session survives a failover
— its `protocol` node did not change — sees no server restart and would never
reclaim. When a shard fails over, the primary **MUST** therefore signal every client
whose record names the shard:

- **NFSv4.1:** set `SEQ4_STATUS_RECALLABLE_STATE_REVOKED` or
  `SEQ4_STATUS_ADMIN_STATE_REVOKED` on the client's next `SEQUENCE` reply, or
  force the loss of its session so it reclaims everything; grace then covers
  every shard the client's record names.
- **NFSv4.0:** return `NFS4ERR_STALE_STATEID` for its stateids in the shard, which
  makes it recover them.
- **SMB:** break the connection, so the client reconnects and reclaims its
  durable opens; volatile opens are lost, as after any server failure.
- **NLM:** notify it of a restart, so it reclaims its locks ([§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)).

A reclaim is accepted only in a shard the client's record names ([§2.1](#2.1%20Client)).

**A former primary fences itself.** Grants are replies to clients, which no
receiver can refuse by epoch, so a primary **MUST** stop serving open state at its
node lease expiry less the drift bound, and a new primary starts grace only after the
old primary's node lease has lapsed plus the drift bound ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).

### 4.5 NLM locks and restart notification

NFSv3 locks through NLM, and NLM has no lease: a host renews nothing, so its
locks are held until it unlocks them or restarts. Restarts are told both ways by
NSM, and both directions are this component's:

- **The host restarted.** Every NLM request carries the host's NSM state number,
  part of its `ClientID` ([§2.1](#2.1%20Client)). A restart notification from the host, or a
  request with a new state number, expires the old `ClientID`, releasing every
  lock it held ([§4.3](#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)).
- **The server lost locks.** The installation holds one durable **NSM state
  number**. A failover of a shard in which any NLM host held locks **MUST**
  raise it, durably, before the shard's grace begins, and then send a restart
  notification carrying it to every NLM host whose client record names the
  shard, at the record's `Notify`. The durable NLM client records are the
  monitor list; there is no second one. The host then reclaims its locks, which
  grace admits as it admits every reclaim ([§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe)), and a non-reclaim lock
  in grace is refused with `ErrGrace`.

> decision: an NLM lock has no lease, so a host that dies and never comes back
> holds its locks until an administrator revokes its client. This is the NLM
> protocol's own contract, and every NLM server has the same ceiling; a
> server-side timeout would release locks a slow but live host still relies
> on. Add a timeout only if a deployment's NLM hosts are known to be disposable.

## 5. Caching grants

### 5.1 What a grant is

Without a grant, a client must ask the server before trusting anything it has
cached: reads revalidate, writes go out before they are acknowledged, and every
lock is a round trip. When only one client uses a file, the server can promise
it that nobody else is touching the file, and the client then serves reads from
its cache, buffers writes and takes locks locally. NFS calls the promise a
**delegation**; SMB an **oplock** and, since SMB 2.1, a **lease**, keyed by file
rather than by open so that two opens by one client do not break each other.

| Kind | The holder may | Several clients may hold it |
| --- | --- | --- |
| read | serve reads from its cache | yes |
| write | also buffer writes and take locks locally | no |
| handle | keep the file open after the application closes it, so a reopen is local | yes |

An NFS read delegation is a read grant; an NFS write delegation is read and
write. Neither is ever a handle grant, which only SMB offers: a delegation
already lets its holder open the file locally, and it ends by recall or
return, not by close. SMB leases combine the three freely.

### 5.2 A recall, and why nothing is merged

When another client opens the file in a conflicting way, the server **recalls**
(NFS) or **breaks** (SMB) the grant. The holder sends its buffered writes to the
server as ordinary writes and locks as ordinary lock requests, drops or
downgrades its cache, and acknowledges (NFS `DELEGRETURN`, SMB a break
acknowledgement). The conflicting request **MUST NOT** hold a worker while it
waits: it is answered with `ErrDelay` — NFS `DELAY` or `JUKEBOX`, an SMB pending
reply completed later — and proceeds once the acknowledgement arrives or the
recall is revoked.

Nothing is merged: a write grant means the holder was the only writer, so there
is no second version to reconcile. The second client sees the first one's data
exactly as if it had never been cached.

### 5.3 A recall MUST end within a bounded time

A grant is only safe if it can be taken back. A recall **MUST** have a deadline,
and a holder that has not acknowledged by it **MUST** have the grant revoked;
writes it sends under the revoked grant are refused. Waiting indefinitely for a
client that has stopped answering blocks every other client of that file, on
nothing but a promise the server made unprompted. A revoked grant costs one
client its cache; an unbounded recall costs every other client the file. The
deadline is generous for that reason.

A client that let one recall be revoked **MUST NOT** be offered grants again for
the rest of its lease, and its other grants **SHOULD** be recalled: it has shown
it does not answer. SMB writes carry an open, not a lease, so revoking an SMB
lease **MUST** also invalidate the opens it covered; otherwise the holder's
stale buffered writes arrive through them after the second client's.

### 5.4 How a grant is obtained, and where it pays

No application asks for a grant: the operating system's client does, on every
open, and the server decides.

- **SMB:** the client puts a requested oplock level, or a lease request naming a
  lease key and the read, write and handle bits it wants, in `CREATE`; the reply
  says what was granted.
- **NFSv4:** the server may offer a delegation in the `OPEN` reply when it sees
  no conflict and the client has a working callback channel; NFSv4.1 lets the
  client state a preference. NFSv3 has neither.

A server **SHOULD** offer a grant only when no other client holds the file open
in a conflicting way, and **MUST NOT** offer one to a client without a working
callback path, since a grant that cannot be recalled cannot be revoked in time.

Grants pay for files one user works on: document editors (open, save, reopen),
directory browsing (the handle grant keeps handles across the many small opens a
browse does), builds and version control in a home directory, single-user
shares. They are never offered for files many clients share, and a client that
never gets one still works.

## 6. A deny mode is checked at open

A deny mode is evaluated once, when an open is granted, against the opens
already held. It **MUST NOT** be re-evaluated per read or per write of a granted
open: the open that was granted was granted, and a later open cannot
retroactively forbid it.

An anonymous open — NFSv3 I/O, and NFSv4 I/O under the anonymous stateid — was
never granted, so nothing was checked for it. Its reads and writes **MUST** be
checked against the deny modes held, per operation, and refused with
`ErrShareViolation` when one forbids them.

An open that conflicts is refused. It **MUST NOT** be downgraded silently to
weaker access than the client asked for — a client that asked for write and got
read discovers it on the first write, having already decided the file was
writable.

## 7. Conflicts across protocols

One table ([§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary)) makes every conflict decidable; these rules decide it.

| Held | Requested | Rule |
| --- | --- | --- |
| an SMB deny mode | an NFS open, read or write the deny mode forbids | refused |
| an NFSv4 deny mode | an SMB open it forbids | refused |
| an SMB byte-range lock | an NFS `READ` or `WRITE` across the range it forbids | refused: SMB locks are mandatory |
| an NFS byte-range lock | an SMB byte-range lock that overlaps it | refused |
| an NFS byte-range lock | SMB or NFS I/O across its range | allowed: NFS locks are advisory and gate lock requests only |
| a caching grant of either protocol | a conflicting open, write, set-attribute, rename or unlink from another client, through either protocol | the grant is recalled within its deadline ([§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)) before the request proceeds |
| a write grant | a read from another client | recalled to read |
| an SMB open whose deny mode includes delete | an NFS `REMOVE` of a name of the file, or a `RENAME` of it or over it | refused, `ErrShareViolation`; checked per operation, as anonymous I/O is ([§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open)) |
| SMB opens that all share delete | an NFS `REMOVE` of the file's name | the name goes now; the opens keep the file alive until the last close ([§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive)) |
| a delete-pending file | an open, through either protocol | refused, `ErrDeletePending` ([§9.4](#9.4%20Delete%20on%20close)) |

A caching grant is recalled by the server in the holder's own protocol, whatever
protocol caused the conflict ([RFC 17](rfc-17-vfs.md)'s callbacks).

A layout is recalled when a mandatory lock or deny mode is granted over its
range, so pNFS I/O, which bypasses the primary, never crosses one.

### 7.1 Writers that do not coordinate

Two clients that write one file without a lock, a deny mode or a grant are
given exactly this, whatever their protocols, and nothing more:

- **Each write request is applied whole.** One `WRITE` is one engine write
  ([RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more)) and one journal version ([RFC 1 §3.1](rfc-1-journal.md#3.1%20Write)), so its bytes are never
  interleaved with another write's within its range.
- **Overlap resolves by arrival at the primary.** Every write to a file runs at
  the file's primary ([RFC 17 §4.8](rfc-17-vfs.md#4.8%20One%20primary%20per%20file)), which assigns versions in one serialised
  order; where two writes overlap, the later one's bytes win, byte by byte, and
  survive every later stability point and offload.
- **A write is visible once acknowledged.** A read that reaches the primary
  after a write was acknowledged returns that write's bytes or newer ones.

Nothing orders two clients' writes beyond that. An application write the
client splits into several requests (NFS `wsize`, SMB `MaxWriteSize`) is several
writes and may interleave with another client's. A read concurrent with an
overlapping write **MAY** return some bytes from before it and some from after.
A caching client sees another's writes only through its protocol's own
mechanisms — NFS close-to-open consistency, revalidating by the change attribute
at open; SMB lease and oplock breaks ([§5.2](#5.2%20A%20recall%2C%20and%20why%20nothing%20is%20merged)) — and applications that need more
**MUST** lock.

## 8. What is durable

Each entity has one rule. Durable state is written to the metadata store under
the keys [RFC 16](rfc-16-metadata-store.md) lists; volatile state never is.

| Entity | Rule | Why |
| --- | --- | --- |
| **Client** | **durable**, holding only what reclaim needs: the client's identity, the shards it has held state in, and whether its state was revoked or its reclaim is incomplete | without it every reclaim after a loss must be refused ([§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe)); one record per client, not per open |
| **Open** | **volatile**, reclaimed in grace — except **durable** when it keeps an unlinked file alive ([§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive)) or `Durability` is persistent, and then every field, the SMB identity and lock sequences included ([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)) | a durable record per open costs a write per open; only these two cases lose data or a promise without one |
| **Lock** | **volatile**, reclaimed in grace — **durable** only when its open is persistent | reclaim restores it |
| **CachingGrant** | **volatile**, never reclaimed; an SMB lease survives only inside a persistent open, with its key, parent key, kind and epoch ([§2.4](#2.4%20CachingGrant)) | a lost grant costs a client its cache, never correctness |
| **delete pending** | **volatile** with the opens — **durable** while the file has a persistent open ([§9.4](#9.4%20Delete%20on%20close)) | a pending delete is a promise to the client that set it, which only a persistent open carries across a failover |
| **Watch** | **volatile**, never reclaimed | the client re-registers; the NFSv4.1 specification does not allow reclaiming directory notifications |
| **Layout** | **volatile**, reclaimed in grace; recalled, never handed over, when its shard's primary changes or its file moves | a lost layout costs a `LAYOUTGET`; a stale one is refused by epoch |
| **Copy** | **volatile**, never reclaimed ([§2.7](#2.7%20Copy)) | the client runs a lost copy again |
| **NSM state number** | **durable**, one per installation ([§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)) | an NLM host recognises a server restart only by a higher number |

Client records are held globally, not per shard, because one client's state spans
many shards; the shard list in each is what scopes its reclaims. The list is
written when the client first takes state in a shard, not per open, and is
bounded: a client past the bound is recorded as holding state in every shard of
the share, which widens its grace and never loses a reclaim.

### 8.1 SMB durable and persistent opens

An SMB client that loses its connection expects to reconnect and find its opens.
What the server keeps, and for how long, is set per open by its create request:

| Kind | Survives | Held as |
| --- | --- | --- |
| volatile | nothing: a disconnect closes it | the primary's table |
| durable (v1, v2) and resilient | a disconnect, for `Timeout`; a handover | the primary's table; after a failover, reclaimed by the reconnect in grace like any volatile open ([§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe)) |
| persistent | a disconnect, for `Timeout`; a handover; a failover | a durable record ([§8](#8.%20What%20is%20durable)) with every field of the open, its locks and its lease |

**A disconnect.** When an SMB client's connection ends, its volatile opens
close. Its durable, resilient and persistent opens stay, with every lock, deny
mode and lease they hold, and conflict as before; each is closed when `Timeout`
has passed since the disconnect without a reconnect. A close by timeout is a
close: it releases what the open held and can run a pending delete
([§9.4](#9.4%20Delete%20on%20close)). A new primary counts every persistent open as disconnected from
the moment it began serving, so the disconnect time is never written.

**A reconnect matches an open only when all of these do:** the client
(`ClientGuid`), the principal, the open's `CreateGUID` for a v2 request, and the
lease key when the open has a lease. Anything else is refused as if the open did
not exist. Without the principal, a client that learned another's FileId takes
over its open; without the lease key, a reconnect attaches an open to a lease of
another file.

**Replays are answered, never applied twice.** A `CREATE` marked as a replay
whose `CreateGUID` names an existing open of the same client returns that open.
A lock request on a resilient, durable v2 or persistent open names an index
and a sequence; if `LockSeq` at that index already holds that sequence, the
request is a replay and is answered success without being applied, and
otherwise the sequence is stored with the lock's result. For a persistent open,
`LockSeq` is written in the transaction that writes the lock, so the answer
survives a failover. Without it, a lock re-sent after a lost reply conflicts
with itself.

**An app instance replaces its predecessor.** A create carrying an
`AppInstance` that an open of the same file already holds, from any client,
closes that open first, releasing its locks, deny mode and lease, and then
proceeds — unless both carry an `AppVersion` and the new one is not higher,
when the create is refused. This is how a failover cluster, or a desktop session
re-attaching a profile container from another machine, takes over a file its
earlier instance still holds open. The check runs in the open's own step at the
file's primary, so no other open lands between the close and the grant.

## 9. Open state and the life of a file

### 9.1 An open keeps a file alive

An open is the second holder of a file ([RFC 7 §4.2](rfc-7-namespace-metadata.md#4.2%20Open%20state%20is%20the%20second%20holder)). An unlink that leaves an
open file with no entry **MUST**, in the transaction that removes the entry, make
the open durable; the file's pending release ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)) is written in the
same transaction and names no holders — the durable open records are the only
list of them. The last close releases the file. Held only in
one process past that point, another node could release a file a client still
has open, drop its refs, and let sweep delete its content.

Opening and closing a linked file **MUST NOT** write a record for the open
itself unless the open is persistent ([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)). The only other write an open may
cause is the client record's shard list, once, when the client first takes state
in that shard ([§8](#8.%20What%20is%20durable)); an open of a linked file by a client already listed
for its shard writes nothing.

### 9.2 A new primary releases nothing before grace ends

Volatile opens held by a failed primary are gone with it. Until its
grace period ends, a new primary **MUST** treat every file of the shard as possibly
open: an unlink that drops `nlink` to zero writes the pending release, and the
release waits for grace and honours the opens clients reclaimed.

### 9.3 Locks do not pin bytes

Holding an open, a lock, a deny mode or a grant **MUST NOT** make an extent
ineligible for eviction ([RFC 0 §8.1](rfc-0-data-lifecycle.md#8.1%20Evict)) or reclamation ([RFC 0 §8.2](rfc-0-data-lifecycle.md#8.2%20Reclaim)). All of it is
about who may act on a file, not where its bytes are. Coupling them is how a
local tier fills up with content that is durable remotely and cannot be released
because a client left a file open.

### 9.4 Delete on close

SMB deletes a file by marking it, not by removing its name. A file becomes
**delete pending** when an open with `DeleteOnClose` closes (SMB's
`FILE_DELETE_ON_CLOSE` at create), or at once when an open holding delete
access sets the disposition (`FileDispositionInformation`); a set-disposition
of false, through any such open, clears it. Delete pending is held in the
file's table, together with the entry — parent and name — the marking open
was opened through.

While a file is delete pending:

- every new open of it, through either protocol, **MUST** be refused with
  `ErrDeletePending`;
- its name still resolves, so a lookup, a listing and `GETATTR` see it;
- a rename of it is refused with `ErrDeletePending`; an unlink of its name is
  allowed.

**The last close removes the name.** When the file's last open closes — its
last of any protocol, a close by timeout ([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)) included — the recorded
entry is unlinked, if it still names the file, through the ordinary unlink
([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)): its transaction writes the pending release, no open remains,
and the release runs. Only that one name goes; another hard link keeps the file.
A delete with POSIX semantics (`FILE_DISPOSITION_POSIX_SEMANTICS`) is not
delete pending: it unlinks the name at once, and the opens keep the file alive
([§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive)), exactly as an NFS `REMOVE` does. Delete pending is not a
holder ([RFC 7 §4.4](rfc-7-namespace-metadata.md#4.4%20There%20is%20no%20third%20holder)): it keeps nothing alive; it is an unlink waiting for the
opens to end.

**Durability.** Delete pending is volatile with the opens that cause it. While
the file has a persistent open it **MUST** be durable: written, with its
recorded entry, in the transaction of the close or set-disposition that makes
it, and deleted by the unlink's transaction or by the clear.

> decision: without a persistent open, a failover loses a pending delete with
> the opens, and the file keeps its name — the outcome a crash before the last
> close has on a local volume, which SMB clients already tolerate. Persisting
> it for durable opens too would cost a write per delete of a file that was
> open elsewhere. Persist it for every open if a workload depends on a delete
> outliving a failover without persistent opens.

### 9.5 Open children refuse an SMB rename or delete of their directory

SMB refuses to rename a directory, or to delete it, while a file below it is
open. A rename or delete of a directory that comes from SMB therefore checks the
opens whose `Via` — the directory the open was made through — is that
directory. Each primary keeps, in memory, a count of its opens per `Via`,
changed by every open and close; the check is one lookup. A child that starts
its own shard ([RFC 11 §2](rfc-11-ownership.md#2.%20Shards)) has its count at that shard's primary, which the
rename's prepare already asks ([RFC 11 §8.1](rfc-11-ownership.md#8.1%20Operations%20across%20shards)). A nonzero count refuses the
request with `ErrShareViolation`. An NFS rename or remove is not refused: POSIX
allows both.

> decision: only direct children are counted, not every descendant. Counting
> descendants means walking an open's ancestors on every open and close, across
> shards wherever a subtree starts one, to serve a check whose only failure is
> letting a rename succeed that Windows would refuse: the open keeps working,
> since opens hold files, not names ([§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary)). A delete needs no more, because a
> directory with any child is not empty and is refused anyway. An open counts
> only under the directory it was opened through, not under the file's other
> links. Count every ancestor, per shard, if a workload is shown to depend on
> the refusal of a rename above an open grandchild.

## 10. Shard placement

Open state is held by the **primary of the file's shard** ([RFC 15 §3](rfc-15-topology.md#3.%20One%20primary%20per%20shard)) and
moves with it. It is fenced by that primary's epoch: an open-state change carried
by a superseded primary is refused, and every open-state call carries the
`ClientID` it acts for, which the primary checks owns the open, lock, grant, watch
or layout named.

- **A handover** of a shard's primary hands the shard's volatile table, with its
  dedup table, to the new primary under the new epoch, after the old primary
  stops granting and before the new one serves ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)). No state
  is lost, so no grace runs and no client is told.
- **A batch move of files** to another shard hands over only those files' entries,
  with their dedup entries, frozen with the batch. The receiving primary installs
  them after the batch commits and adds its shard to each client record they
  name ([§2.1](#2.1%20Client)), so a later failover of that shard asks those clients to
  reclaim. No grace runs.
- **A failover** loses the volatile table, and starts grace for that shard
  ([§4.4](#4.4%20Grace%20is%20per%20shard)).
- **A layout** is bound to the (shard, epoch) it was granted under. When that
  shard changes primary, by handover or failover, or the file moves to another
  shard, the layout **MUST** be recalled, and
  revoked at the recall deadline; a `LAYOUTCOMMIT` for it then fails with
  `NFS4ERR_BADLAYOUT` ([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS)).

When a shard's primary changes, the NFS write verifier **MUST** change, or clients
never resend writes they sent unstable to the old one ([RFC 11 §7](rfc-11-ownership.md#7.%20Protocol%20state)).

## 11. Interface

Signatures are indicative; the obligations above are normative. The filesystem
service ([RFC 17](rfc-17-vfs.md)) is this interface's only caller.

```go
type OpenState interface {
	// Clients.
	Connect(ctx context.Context, c Client) (ClientID, error)
	Renew(ctx context.Context, c ClientID) error
	Expire(ctx context.Context, c ClientID) error // releases everything (§4.3)

	// Opens and deny modes (§6). Every call names its client, and the primary
	// refuses one that names another client's state (§10).
	// OpenRequest carries access, deny, durability, timeout, delete-on-close,
	// the lease key and kind wanted, create GUID, app instance and reclaim.
	Open(ctx context.Context, c ClientID, file FileID, r OpenRequest) (Open, *CachingGrant, error)
	Reconnect(ctx context.Context, c ClientID, id Identity, o OpenID, m ReconnectMatch) (Open, error) // §8.1
	Disconnect(ctx context.Context, c ClientID) error                                                // closes volatile opens, times the rest (§8.1)
	SetDisposition(ctx context.Context, c ClientID, o OpenID, delete bool) error                   // §9.4
	SuspendTimes(ctx context.Context, c ClientID, o OpenID, suspend, resume TimeMask) error         // §2.2
	Close(ctx context.Context, c ClientID, o OpenID) error // the last close may unlink (§9.4) and release (§9.1)

	// Byte-range locks. o is zero for an NLM lock (§2.3); seq is SMB's lock
	// sequence, nil when the request carries none (§8.1). An NFSv4.0 request's
	// owner seqid travels in OpenRequest and in Lock, Unlock and Close options,
	// and is checked at the owner's home primary (§2.8): ErrBadSeqID.
	Lock(ctx context.Context, c ClientID, file FileID, owner LockOwner, o OpenID, r ByteRange, exclusive, reclaim bool, seq *LockSequence) error
	TestLock(ctx context.Context, c ClientID, file FileID, owner LockOwner, r ByteRange, exclusive bool) (*Lock, error)
	Unlock(ctx context.Context, c ClientID, file FileID, owner LockOwner, r ByteRange) error

	// Restart notification from an NLM host (§4.5): expires its old ClientID.
	HostRestarted(ctx context.Context, host []byte, state uint32) error

	// Asynchronous copies (§2.7). The filesystem service runs the copy and
	// reports progress; Status and Cancel answer ErrNoCopy for a lost copy.
	CopyBegin(ctx context.Context, c ClientID, cp Copy) (CopyID, error)
	CopyProgress(ctx context.Context, id CopyID, done int64, s CopyState) error
	CopyStatus(ctx context.Context, c ClientID, id CopyID) (Copy, error)
	CopyCancel(ctx context.Context, c ClientID, id CopyID) error

	// Caching grants (§5): recalls go out through the adapter's callbacks.
	Return(ctx context.Context, c ClientID, g GrantID) error
	AckBreak(ctx context.Context, c ClientID, g GrantID, to GrantKind) error

	// Layouts (§2.6).
	LayoutGet(ctx context.Context, c ClientID, o OpenID, r ByteRange, write, reclaim bool) (Layout, error)
	LayoutCommit(ctx context.Context, c ClientID, l LayoutID, end int64, mtime time.Time) error // ErrBadLayout on a stale epoch
	LayoutReturn(ctx context.Context, c ClientID, l LayoutID) error

	// Watches.
	Watch(ctx context.Context, c ClientID, dir FileID, recursive bool, f ChangeMask) (WatchID, error)
	Unwatch(ctx context.Context, c ClientID, w WatchID) error

	// Grace (§4.2): the client has reclaimed everything it will.
	ReclaimComplete(ctx context.Context, c ClientID) error

	// Checks run at the primary, in the process that runs the I/O (§3, §7).
	// ErrDelay while a recall is outstanding; ErrGrace in grace.
	CheckIO(ctx context.Context, o OpenRef, r ByteRange, write bool) error
	// CheckChange recalls grants and never waits. On a remove or rename it also
	// refuses with ErrShareViolation against a deny-delete, and a rename of a
	// delete-pending file with ErrDeletePending (§7, §9.4). On an SMB rename or
	// delete of a directory it refuses with ErrShareViolation while any open was
	// made through that directory (§9.5).
	CheckChange(ctx context.Context, file FileID, by ClientID, what ChangeMask) error
}

var (
	ErrGrace          = errors.New("openstate: in grace period")
	ErrShareViolation = errors.New("openstate: deny mode conflict")
	ErrLocked         = errors.New("openstate: range locked")
	ErrNoReclaim      = errors.New("openstate: nothing to reclaim")
	ErrStaleClient    = errors.New("openstate: client expired or revoked")
	ErrDelay          = errors.New("openstate: recall outstanding, retry")
	ErrBadLayout      = errors.New("openstate: layout stale or revoked")
	ErrNotYours       = errors.New("openstate: state held by another client")
	ErrDeletePending  = errors.New("openstate: file is delete pending")
	ErrNoCopy         = errors.New("openstate: copy unknown or lost")
	ErrBadSeqID       = errors.New("openstate: owner sequence out of order")
)
```

## 12. Invariants

| # | Invariant |
| --- | --- |
| L1 | All open state is keyed by file, held in one table per file at the primary of the file's shard, and visible to every adapter; that primary also runs the file's I/O. |
| L2 | No operation blocks on a holder that no longer exists; after a loss of open state, every non-reclaim request and every I/O a lost lock or deny mode might forbid is refused for that shard until grace ends, and grace ends on its own — early once every recorded client has finished reclaiming. |
| L3 | A reclaim is accepted only from a client whose durable record names the shard, and every client so named is told to reclaim. |
| L4 | An expired or revoked client's state is released in every view in one step. |
| L5 | A recall ends within its deadline, by acknowledgement or revocation, and no worker waits on it. |
| L6 | A deny mode is checked once, at open, for a granted open, and per operation for an anonymous one; a conflicting open is refused, never downgraded. |
| L7 | An open that keeps an unlinked file alive is durable by the time the unlink commits. Opening and closing a linked file writes no open record unless the open is persistent, and otherwise writes at most the client record's shard list, once per client and shard. |
| L8 | After a failover, nothing is released before grace ends; a handover or a batch move of files hands the state over and runs no grace. |
| L9 | Open state never makes an extent ineligible for eviction or reclamation. |
| L10 | Open state and shard placement share no records. |
| L11 | A primary serves no open state past its node lease expiry less the drift bound. |
| L12 | A layout is bound to the (shard, epoch) it was granted under and is recalled when that shard changes primary or the file moves. |
| L13 | Every open-state call names its client, and names only that client's state. |
| L14 | A delete-pending file refuses every new open and every rename, keeps its name until its last open closes, and then loses that name through the ordinary unlink; it is durable while the file has a persistent open. |
| L15 | A reconnect matches an open only on client, principal, create GUID (v2) and lease key; a replayed create or lock returns its first result; a create with a held app instance closes the earlier open first. A persistent open's record holds all of it. |
| L16 | One grant per (client, lease key, file); opens under one key never break their own lease; a v2 lease's epoch rises with every change of its kind; an NFS delegation is never a handle grant. |
| L17 | A lock names an owner, and an NFSv4 or NLM owner's locks never conflict with each other; an NLM lock needs no open. A loss of NLM locks raises the durable NSM state number and notifies every recorded host before grace. |
| L18 | A lost asynchronous copy is reported unknown, never done. |
| L19 | The NFSv4.1 server owner's major ID and server scope are the installation's identity on every node; the minor ID is the node's. |
| L20 | Each write request is applied whole under one version at the file's primary; overlapping writes resolve byte by byte in the primary's arrival order. |
| L21 | A write or read through an open never advances a time that open has suspended; through any other open it does. |
| L22 | An NFSv4.0 owner's sequence is checked and advanced at its home primary in the step that applies the request; no out-of-sequence request changes state. |
| L23 | An SMB rename or delete of a directory is refused while an open made through that directory exists; an NFS one is not. |

## 13. Conformance

Every check runs through the filesystem service, against every backend, in the
index's tiers.

### 13.1 Group A — wrong holder, lost state, stuck client

| Requirement | Check |
| --- | --- |
| [§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary) one table | Take a lock and a deny mode through one adapter, reach the same file through the other. Assert the other observes both. A single-adapter rig cannot fail this. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) grace | Grant a lock, restart, have a different client request the conflicting lock immediately. Assert refusal for the lease period. Request a lock on a file nobody held; assert it is refused too, and that a reclaim is granted. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) reclaim needs a record | Lose the client records, restart, reclaim. Assert refused. |
| [§4.3](#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere) lease expiry | Grant a grant through one adapter, let the lease expire, open the file conflictingly through the other. Assert the open is granted without a restart. |
| [§4.4](#4.4%20Grace%20is%20per%20shard) grace per shard | Fail one shard over. Assert only that shard refuses new state, and that a client whose session survived is told to reclaim and does. |
| [§4.4](#4.4%20Grace%20is%20per%20shard) reclaim scoped | Client C holds state only in shard U1, D holds a lock in U2; fail U2 over. Assert C's reclaim of D's lock is refused. |
| [§4.4](#4.4%20Grace%20is%20per%20shard) self-fence | Pause a primary past its node lease, let a new primary finish grace, resume the old one. Assert it grants nothing. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) I/O in grace | Hold an SMB mandatory lock, fail over, write the range through NFSv3 before the reclaim. Assert `ErrGrace`. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) early end | Fail over with two recorded clients; both reclaim and complete. Assert grace ends before the lease period. |
| [§10](#10.%20Shard%20placement) planned move | Hold opens, locks and a grant, move the shard. Assert no grace, and every holding intact at the new primary. |
| [§10](#10.%20Shard%20placement) stale layout | Write through a layout, fail its data server's shard over, `LAYOUTCOMMIT`. Assert `NFS4ERR_BADLAYOUT`. |
| [§5.2](#5.2%20A%20recall%2C%20and%20why%20nothing%20is%20merged) no blocked worker | Hold grants on 10^4 files with a client that never answers; send conflicting opens from another. Assert each gets `ErrDelay` at once and unrelated operations keep their latency. |
| [§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open) anonymous I/O | Hold an SMB deny-write, write through NFSv3. Assert refused. |
| [§11](#11.%20Interface) client check | Close, unlock and return a grant naming another client's state. Assert `ErrNotYours`. |
| [§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time) recall deadline | Grant a grant to a client that never answers recalls, open conflictingly. Assert the open proceeds after the deadline and the silent client's later writes are refused. |
| [§5.2](#5.2%20A%20recall%2C%20and%20why%20nothing%20is%20merged) flush on recall | Buffer writes under a write grant, open from another client. Assert the second client reads the first's data. |
| [§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open) no downgrade | Request write against a deny-write. Assert refusal, never a read-only open. |
| [§7](#7.%20Conflicts%20across%20protocols) cross-protocol | For every row of the table, assert the stated outcome with one protocol holding and the other requesting. |
| [§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive) open-unlinked | Open, unlink, crash. Assert the content survives until grace ends, and is released after it unless the open was reclaimed. |
| [§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive) lazy | From one client, open and close a linked file 10^4 times with volatile opens. Assert exactly one write, the client record's shard list on the first open, and none after. Repeat with persistent opens; assert each open writes its record. |
| [§9.2](#9.2%20A%20new%20primary%20releases%20nothing%20before%20grace%20ends) new primary | Open a file on one primary, move the shard, unlink through the new primary. Assert no release before grace ends. |
| [§9.4](#9.4%20Delete%20on%20close) delete pending | Open a file twice over SMB, the first with delete-on-close; close the first. Assert the name still resolves, a third open through each protocol and a rename are refused `ErrDeletePending`; close the second and assert the name is gone and the file released. Repeat with a second hard link: assert only the opened name goes. |
| [§9.4](#9.4%20Delete%20on%20close) persistent | Set delete pending on a file held by a persistent open, fail the shard over, reconnect. Assert a new open is refused and the last close removes the name. |
| [§7](#7.%20Conflicts%20across%20protocols) remove against deny-delete | Hold an SMB open without share-delete; `REMOVE` the name over NFS. Assert `ErrShareViolation`. Reopen sharing delete, `REMOVE` again: assert the name is gone, the SMB open still reads, and its close releases the file. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) reconnect matching | Disconnect a durable v2 open; reconnect with its FileId but each of another client GUID, another principal, another create GUID, another lease key. Assert each refused, and the exact match accepted. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) timeout | Disconnect a durable open holding a deny-write; open for write from another client before and after `Timeout`. Assert refused, then granted. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) create replay | Send a `CREATE` with a create GUID, drop the reply, replay it. Assert one open exists and the replay returns its `OpenID`. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) lock replay | On a persistent open, take an exclusive lock with a sequence, drop the reply, fail the shard over, replay. Assert success, not `ErrLocked`, and one lock held. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) app instance | Client A opens a file with an app instance and deny-all; client B opens it with the same app instance. Assert A's open, locks and lease are gone and B is granted; repeat with B's version not higher, assert B refused and A intact. |
| [§2.4](#2.4%20CachingGrant) lease key | Open a file twice from one client under one key, then under a second key. Assert the second open does not break the first's read-write-handle lease, the third does, and each break carries a higher epoch. Assert an NFS write delegation is never offered with the handle kind. |
| [§2.3](#2.3%20Lock) owners | Two NFSv4 lock-owners of one client lock one range exclusively: assert the second refused. One owner locks overlapping ranges: assert they merge, not conflict. Take an NLM lock: assert it is held with no open and refuses an overlapping NFSv4 lock. |
| [§4.5](#4.5%20NLM%20locks%20and%20restart%20notification) NLM restart | Hold NLM locks from two hosts, fail the shard over. Assert the NSM state number rose before grace, both hosts were notified, their reclaims are granted and a non-reclaim lock is refused `ErrGrace`. Then send a restart notification from one host: assert its locks are released at once. |
| [§2.7](#2.7%20Copy) lost copy | Start an asynchronous copy, fail the destination's shard over mid-copy, ask its status. Assert `ErrNoCopy`, never done. |
| [§2.1](#2.1%20Client) server owner | Send `EXCHANGE_ID` through two `protocol` nodes. Assert one major ID and one scope, two minor IDs, and that the client ID from one is accepted by the other. |
| [§7.1](#7.1%20Writers%20that%20do%20not%20coordinate) unlocked writers | Two clients, one per protocol, write overlapping 1 MiB ranges of distinct patterns concurrently, 10⁴ times, with commits and offloads between. Assert after each round the overlap holds exactly one writer's pattern, whole, and that it is the one the primary acknowledged last. A design that splits one request across versions fails this. |
| [§2.2](#2.2%20Open) suspended time | Open a file twice over SMB; set `Modify` to -1 on the first. Write through the first; assert `Modify` unchanged after the existence commit. Write through the second; assert it advanced. Set -2 on the first, write through it; assert it advanced. Repeat on a persistent open across a failover. |
| [§2.8](#2.8%20NFSv4.0%20owner%20sequences) owner sequence | Over NFSv4.0, one open-owner opens files in two shards with consecutive seqids; assert both accepted. Send a stale seqid to the second shard's file; assert `ErrBadSeqID` and no state change. Fail the home shard over; assert the owner's next sequenced request is accepted in grace as a new owner's. |
| [§9.5](#9.5%20Open%20children%20refuse%20an%20SMB%20rename%20or%20delete%20of%20their%20directory) open children | Open a file in directory D over SMB; rename D over SMB: assert `ErrShareViolation`. Rename D over NFS: assert it succeeds and the open still reads. Open a file two levels below D; assert an SMB rename of D succeeds. |

### 13.2 What must not stand in

- **A single-client rig MUST NOT stand in for [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe).** The failure needs two clients on one object.
- **A single-adapter rig MUST NOT stand in for [§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary) or [§7](#7.%20Conflicts%20across%20protocols).** State only one protocol can see passes every test that only speaks that protocol.
- **A test that never revokes MUST NOT stand in for [§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time).** A recall that is always answered never exercises the deadline.

### 13.3 Benchmarks

| Benchmark | Measures | Target |
| --- | --- | --- |
| Open and close a linked file | p99 latency, records written | records written 0 once the client is listed for the shard, volatile opens |
| Grace refusal and reclaim of 10^4 locks | time to leave grace | once every recorded client completes; one lease period at most |
| Planned move of a shard holding 10^4 opens | time new opens are refused | 0: no grace |
| Recall with a responsive holder | time from conflicting open to its grant | report |
| 10^5 clients renewing | renewals/s at the primary | report |

## 14. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| state held, by kind | `dittofs_openstate_held{kind=client\|open\|lock\|grant\|watch\|layout\|copy\|delete_pending}` | gauge |
| grants offered and declined | `dittofs_openstate_grants_total{result}` | counter |
| recall time | `dittofs_openstate_recall_seconds` | histogram |
| recalls revoked at the deadline | `dittofs_openstate_recalls_revoked_total` | counter |
| conflicts refused, by rule | `dittofs_openstate_conflicts_total{rule}` | counter |
| shards in grace | `dittofs_openstate_grace_shards` | gauge |
| grace periods ended, by `reason` = `complete` or `timeout` | `dittofs_openstate_grace_ended_total{reason}` | counter |
| requests answered `ErrDelay` while a recall is outstanding | `dittofs_openstate_delays_total` | counter |
| layouts recalled on a primary change | `dittofs_openstate_layout_recalls_total` | counter |
| reclaims accepted and refused | `dittofs_openstate_reclaims_total{result}` | counter |
| leases expired | `dittofs_openstate_expired_total` | counter |

Recall, grant, grace and open-state metrics are defined here only; other RFCs
link to this table. No share or client label. A revoked recall and an expired lease log at `Warn`
with the client and file.

## 15. Open questions

1. **Recall deadline values**, per protocol, and whether they are settings
   ([RFC 13](rfc-13-configuration.md)). The lease period is decided ([§4.1](#4.1%20A%20client%20lease)).
2. **Persistent opens** for continuously available shares: which shares allow
   them, and the cost of a synchronous write per open, grant change and close.
3. **Byte-range lock splitting** across protocols with different range
   semantics (SMB's 64-bit ranges against NFS's end-of-file locks).
4. **Directory grants** (NFSv4.1 directory delegations, SMB directory leases):
   offered at all, and what breaks them.

## Appendix A — prior art

| System | Takes from it |
| --- | --- |
| The NFSv4.1 specification (client records on stable storage, grace, reclaim) | the minimum durable state for safe reclaim; delegations not reclaimed across a loss |
| Linux kernel NFS server and its client-tracking daemon | client records durable, opens and locks rebuilt in grace |
| NFS-Ganesha clustered recovery | a shared grace record; grace scoped to what lost state |
| The SMB2 specification (opens, durable and persistent handles, leases) | durability as a property of an open; lease keys per file |
| Multiprotocol NAS (a lock manager per volume on its serving node; a distributed lock manager with protocol-shared lock domains) | one table per file; SMB deny modes and locks mandatory against NFS I/O |
| Distributed file systems with client capabilities | caching grants recalled on conflict, rebuilt from clients on reconnect |
