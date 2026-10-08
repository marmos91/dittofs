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

A rule or record marked **(cluster)** binds only once a second node can serve a
share; what the first release, one node, follows instead is
[RFC 0's single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile).

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
   file's table, in memory, and — because the open is persistent — in one record
   in the metadata store. An ordinary open of a file that has a name is not
   written there at all. Under the lease, alice-pc caches reads, buffers writes
   and takes byte-range locks locally.
2. **10:15, a network blip.** alice-pc's connection drops. A plain open would
   close; this one was opened persistent, on a share offering continuous
   availability, so it stays, with its deny mode and its lease, for the timeout
   its create negotiated. alice-pc reconnects within it. Every write through it
   is stable: synced before it is acknowledged.
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
   alice-pc never answers, the lease is downgraded to none at a 35 s deadline;
   its open stays, as on Windows.
6. **N1 restarts.** The in-memory table is gone. Without a rule, the second
   desktop could open the file before alice-pc comes back, and both would think
   they hold it. Instead N1, whose start raised the shard's incarnation, runs a
   new **grace period**: for one lease period it refuses every new open, lock or grant, and lets clients its durable
   records name reclaim what they held. alice-pc reclaims its persistent open,
   whose record survived the restart, with every write it was told was done;
   grace then ends on its own, early once every recorded client has reclaimed.
   Had the open been merely durable, it would not survive the restart: the
   reconnect is answered as if the open did not exist, and alice-pc opens the
   file again.

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
 │ client alice-pc   open o1: read+write, deny write, persistent  │
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
  its timeout; a persistent one also survives a restart or failover of its
  primary, because it is written down and its writes are stable; a durable one
  does not ([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)).
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
grant, watch, layout, copy, NFSv4.0 owner sequences and unconfirmed clients, and lock waiters. [§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary)
is the one rule everything rests on: one table per file, at one primary.
[§4](#4.%20Client%20leases%2C%20grace%20and%20reclaim) covers client leases,
grace and reclaim, NLM (Network Lock Manager) locks included;
[§5](#5.%20Caching%20grants) caching grants; [§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open)
deny modes; [§7](#7.%20Conflicts%20across%20protocols) conflicts across
protocols and writers that do not coordinate. [§8](#8.%20What%20is%20durable)
says what is written down; [§9](#9.%20Open%20state%20and%20the%20life%20of%20a%20file)
how open state keeps a file alive and how delete on close works;
[§10](#10.%20Shard%20placement) what happens when a file's primary changes.
[§12](#12.%20Invariants) lists the invariants. On a first read, skip §2.5–§2.10,
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
	ID         ClientID
	Protocol   protocol.Kind
	Owner      []byte    // the client's own name for itself: NFSv4 co_ownerid or SETCLIENTID id, SMB ClientGuid, NLM host name; indexed (§2.1)
	Verifier   [8]byte   // NFSv4 boot verifier; a new one under the same Owner is a reboot
	Principal  Principal // NFSv4: who established the client ID; the same Owner from another principal is refused. Zero for SMB and NLM (§2.1)
	MachCred   MachCred  // NFSv4.1 state protection: SP4_MACH_CRED's principal and protected operations, or none
	CSSeq      uint32    // NFSv4.1: the sequence ID of the last CREATE_SESSION applied (RFC 8881 §18.36.4)
	CSReply    []byte    // NFSv4.1: that CREATE_SESSION's reply, opaque here; answers its replay
	Expires    time.Time // volatile, held by the lease owner (§8); zero for an NLM host, which has no lease (§4.5)
	LeaseOwner NodeID    // (cluster) the one node that renews and expires this client (§4.1)
	Callback   NodeID    // (cluster) the protocol node holding its back channel; zero: none
	PathDown   bool      // its back channel is down; signalled to the client until it rebinds (§4.1)
	Sessions   []NodeID  // (cluster) protocol nodes holding a session of this client
	Shards     []ShardID // shards it has held state in; bounded (§8)
	Notify     []byte    // NLM only: where its restart notification goes (§4.5); opaque here
}
```

`ClientID` names one client instance as its protocol defines it: NFSv4's client
ID from `EXCHANGE_ID` or `SETCLIENTID` (a machine plus a boot verifier), SMB's
`ClientGuid`, and for NFSv3 locks the NLM host name with the host's NSM state
number. A client that reboots is a new `ClientID`, which is how its stale state
is recognised and released.

**A client record is found by its owner.** `EXCHANGE_ID`, `SETCLIENTID` and an
SMB negotiate name the client by its own string, not by an ID the server gave;
the record is therefore indexed by (protocol, owner) as well as by ID
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)). A request naming a known owner with the
same verifier and principal returns the existing client ID; with a new verifier it
is a reboot, and expires the old ID once the new one is confirmed (§2.9); from
another principal it is refused with `ErrClientInUse` (NFSv4 `NFS4ERR_CLID_INUSE`),
so a client cannot take over another's owner string. The state-protection mode the
client negotiated (`MachCred`) is kept with the record, because it decides which
principal may later destroy the client or bind a connection to it, on any node
and after a restart. Every field above except `Expires` is durable with the record
(§8).

**An SMB client record names a machine, not a user.** One Windows machine sends
one `ClientGuid` on every connection, and a multi-session host carries many
users' sessions under it. The SMB record is therefore keyed by `ClientGuid`
alone and holds no `Principal`: a negotiate naming a known `ClientGuid` finds
its record whoever authenticates, and is never refused for its principal.
Principals bind where SMB binds them, per session and per open: every open keeps
its opener's principal (§2.2) and a reconnect must match it
([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)). An NLM record has no
principal either; its host is named by its NSM identity (§4.5).

**`CREATE_SESSION` is sequenced in the record.** An NFSv4.1 client numbers its
`CREATE_SESSION` calls from the sequence ID `EXCHANGE_ID` answered. A
`CREATE_SESSION` whose sequence ID equals `CSSeq` is a replay and is answered
`CSReply`; one that is `CSSeq` + 1 is new; any other is refused as misordered
(`NFS4ERR_SEQ_MISORDERED`). `CSSeq` and `CSReply` are written in the
transaction that applies the `CREATE_SESSION`, once per call, so a replay that
reaches another node, or arrives after a restart, gets the first reply rather
than a second session.

**A bearer ID with room is unguessable.** An ID a client presents with nothing
else to prove it is the presenter's — an NFSv4 stateid, whose 12-byte `other`
names an open (`Open.Nonce`), a lock state, a delegation (`GrantID`), a layout
(`LayoutID`) or a copy (`CopyID`), and an NFSv4.0 confirm verifier (§2.9) —
carries at least 64 random bits; a client that sees one cannot derive
another's. **A stateid is found without a file handle.** `TEST_STATEID` and
`FREE_STATEID` name stateids with no current file handle (RFC 8881 §18.48,
§18.38), so a stateid's non-random part carries the number of the shard that
holds its record, the random bits are unique within that shard, and each primary
keeps an index from random bits to the record (open, lock state, grant, layout or
copy) and its file. A stateid is resolved by shard number, then by that index;
a request whose file handle names another file than the record's is refused as a
bad stateid. An ID with no room for them is unique and never reused but not
random: `ClientID`, which `clientid4` carries in 8 bytes, and `OpenID`, which
fills SMB's 8-byte persistent FileId together with the shard. Neither is honoured
on its own: a `ClientID` is accepted only under its principal or session, and an
`OpenID` only within the session and for the principal it was opened by (§2.2).
Every call naming any of them is also checked against its holder and the
caller's principal (§10).

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
	DeleteBy      Principal  // who asked for delete on close or set the disposition; the deferred unlink runs as it (§9.4)
	Grant         GrantID    // the SMB lease or oplock covering it; zero for NFS (§2.4)
	Suspended     TimeMask   // Modify, Access: times a write through this open leaves alone (§2.2); zero: none
	Via           FileID     // the directory it was opened through (§9.5)
	StateSeq      uint32     // NFSv4: the seqid of the open's stateid, raised by every OPEN upgrade, OPEN_DOWNGRADE and CLOSE
	Nonce         uint64     // NFSv4: random bits the open's stateid carries beside its ID (§2.1)
	LossSeen      uint64     // SMB: the file's loss sequence this open last reported or sampled (§8.1)

	// SMB durable, resilient and persistent opens (§8.1); zero for NFS.
	CreateGUID  [16]byte      // identifies the CREATE: replay and reconnect matching
	AppInstance [16]byte      // zero: none
	AppVersion  [2]uint64     // the app instance version, high and low; zero: none
	Timeout     time.Duration // how long it is kept once its client disconnects
	LockSeq     [64]uint8     // lock sequence per index: a valid bit and a 4-bit sequence
	ChannelSeq  uint16        // the open's channel sequence: replays carrying an older one are refused (§8.1)
	Outstanding [2]uint32     // requests in flight under ChannelSeq and under the one before it (MS-SMB2 3.3.5.2.10)
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

**An open is used only by its opener.** `Principal` is kept for the open's whole
life, and every call that uses the open — I/O through its stateid or FileId, a
lock under it, a downgrade, a close — **MUST** be refused with `ErrNotYours`
unless the caller's principal is the open's, or, for an NFSv4.1 client under
machine-credential state protection, the client's machine principal for the
operations it protects. An NFSv4.0 stateid crosses the wire in clear and binds
no session, so without the per-call check anyone who sees one writes with the
opener's access. The same holds for a lock state and a layout.

**I/O under a delegation stateid is the client's, and authorised afresh.** An
NFSv4 client may use its delegation's stateid for I/O by any of its open-owners,
under any principal (RFC 8881 §9.1.3), so the opener-only rule does not apply to
it. Such a call **MUST** be refused with `ErrNotYours` unless the delegation is
held by the call's client — for NFSv4.1 the client of the session it arrives on,
for NFSv4.0 the client the stateid names — and is not revoked, and **MUST** then
be authorised as anonymous I/O is: the caller's own principal against the file's
current ACL and mode and the share grant, and against the deny modes held
([§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open)). The delegation lends
no access: a principal that could not read the file on its own does not read it
through the delegation.

**The stateid's seqid is state.** NFSv4 numbers the changes to one open's stateid
— an `OPEN` that upgrades it, an `OPEN_DOWNGRADE`, a `CLOSE` — and to one lock
state's; a request naming an older seqid than the current one is refused as old,
a newer one as bad. `StateSeq` and `LockState.Seq` hold them, and change in the
step that applies the request.

**A time can be suspended per open.** SMB lets a client set a time, in a
set-information request on an open, to a sentinel: -1 suspends the automatic
update of that time for I/O through this open, -2 resumes it. `Suspended` holds
the times so suspended — `Modify` and `Access`, never the change time. A
suspension of the change time is accepted and not applied: NFS clients revalidate
their caches by the change attribute, and a frozen one would hide SMB writes from
them ([RFC 17 §5.7](rfc-17-vfs.md#5.7%20GetAttr)). A write or read through
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

// LockState is what an NFSv4 lock stateid names: one lock-owner's locks under
// one open of one file. Its seqid rises with every LOCK and LOCKU that changes
// them; the locks themselves stay Lock records.
type LockState struct {
	ID    LockStateID // at least 64 random bits (§2.1)
	Owner LockOwner
	Open  OpenID
	Seq   uint32
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

A watch is held at the primary of its directory's shard, which sees every change
in that shard. **A recursive watch whose subtree crosses into another shard
(cluster)** also leaves a marker, volatile like the watch, at the primary of each
descendant shard whose root lies under it: a change there that the filter
matches is reported to the watch's primary, which completes the watch with
*enumerate directory* — SMB's `STATUS_NOTIFY_ENUM_DIR`, NFSv4.1's equivalent
notification that the directory must be read again — rather than a change list,
since the change's names are another primary's. The client rescans; no change
under a descendant shard is lost, though its detail is. A shard created or moved
under the watched subtree after registration gets its marker when it starts
serving, from the watches its parent's primary holds. On one node every shard has
the same primary, and changes are delivered with their detail.

### 2.6 Layout

A **layout** (cluster) is a pNFS client's grant to send I/O for a byte range of a file
straight to the data servers it names ([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS)). It is open state like
the others: held by the client under an open, in the file's table, released
with the client's lease, recalled on conflict. It also records the (shard,
epoch) it was granted under, and is recalled when that shard changes primary or
the file moves to another shard ([§10](#10.%20Shard%20placement)).

```go
// Layout is what an NFSv4.1 layout stateid names: one client's layouts of one
// file, granted under one open.
type Layout struct {
	ID     LayoutID // at least 64 random bits (§2.1)
	Client ClientID
	Open   OpenID
	Ranges []LayoutRange // byte ranges held, each read or read-write
	Shard  ShardID
	Epoch  uint64 // the shard's primary epoch it was granted under (§10)
	Seq    uint32 // the layout stateid's seqid
}
```

**The layout stateid's seqid is state.** `Seq` rises by one with every
`LAYOUTGET` that grants, every `LAYOUTRETURN` and every layout recall, in the
step that applies it, and the new value goes out with the reply or the recall.
A recall carries it so the client can tell whether a `LAYOUTGET` reply it holds
was sent before or after the recall, and so never uses a range the recall took
back. A request naming a seqid above the current one is refused as bad. Like an
open's, the layout is used only by its opener's principal (§2.2).

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
	Owner     LockOwner // client ID and the opaque owner the client names
	Kind      OwnerKind // open-owner or lock-owner
	Next      uint32    // the seqid the owner's next sequenced request must carry
	Confirmed bool      // open-owner only: OPEN_CONFIRM has been received; until then only OPEN_CONFIRM is accepted
}
```

An NFSv4.0 owner numbers its sequenced requests — `OPEN`, `OPEN_CONFIRM`,
`OPEN_DOWNGRADE`, `CLOSE` for an open-owner; `LOCK` and `LOCKU` for a
lock-owner — and the server refuses one out of sequence. The primary that holds
the owner's `OwnerSeq` checks and advances it in the step that answers the
request, so a sequence check and the state change it admits never separate.

**Every in-sequence reply advances the sequence, refusals included.** RFC 7530
§9.1.7 advances the seqid on every reply to an in-sequence request except
`NFS4ERR_STALE_CLIENTID`, `NFS4ERR_STALE_STATEID`, `NFS4ERR_BAD_STATEID`,
`NFS4ERR_BAD_SEQID`, `NFS4ERR_BADXDR`, `NFS4ERR_RESOURCE`,
`NFS4ERR_NOFILEHANDLE` and `NFS4ERR_MOVED`, and clients count on it: a request
refused `NFS4ERR_DELAY`, `NFS4ERR_GRACE` or `NFS4ERR_DENIED` still advances
`Next`, or the client's next request is answered `NFS4ERR_BAD_SEQID`. A
refusal outside that list is a reply like any other: it advances the sequence
and is cached for replay below. The home primary advances the sequence even when
the request is then refused at another shard's primary, since the client counts
that refusal as a reply.

**Where it is held.** An owner's `OwnerSeq` is held at the primary of its
**home shard**: the shard of the file of its first sequenced request. A
sequenced request on a file in another shard (cluster) checks and advances the sequence
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

### 2.9 NFSv4.0 unconfirmed clients

```go
// Unconfirmed is an NFSv4.0 SETCLIENTID, or an NFSv4.1 EXCHANGE_ID, not yet
// confirmed. Volatile, held by the protocol node that answered it; never
// written to the metadata store.
type Unconfirmed struct {
	Owner     []byte    // the SETCLIENTID id string or EXCHANGE_ID co_ownerid
	Verifier  [8]byte   // the client's boot verifier
	Principal Principal // who sent it; only it may confirm
	Callback  []byte    // NFSv4.0: the callback address and program it named; opaque here
	ClientID  ClientID  // the ID answered: an existing client's, or a new one
	Confirm   [8]byte   // NFSv4.0: the confirm verifier answered, 64 random bits (§2.1)
	Sequence  uint32    // NFSv4.1: the sequence ID EXCHANGE_ID answered; the first CREATE_SESSION carries it
	Expires   time.Time // one lease period after the request
}
```

An NFSv4.0 client establishes itself in two steps: `SETCLIENTID` proposes an
owner, verifier and callback, and `SETCLIENTID_CONFIRM`, naming the answered
client ID and confirm verifier, makes them current. Until the confirm, nothing
of the proposal is state:

- the `protocol` node that answered `SETCLIENTID` holds the `Unconfirmed`
  record in memory, at most one per owner — a second `SETCLIENTID` for the same
  owner replaces it — and drops it at `Expires`;
- no `Client` record is written and no existing client's state changes: a
  proposal with a new verifier under a known owner does not yet expire the old
  client ID, and a proposal of a new callback does not yet rewrite `Callback`;
- the confirm is accepted only from the proposal's principal, with its client
  ID and confirm verifier, before `Expires`. It then does, in one step, what
  `Connect` does for any client ([§2.1](#2.1%20Client)) — writes the record,
  expires the old client ID on a reboot, records the callback ([§4.1](#4.1%20A%20client%20lease))
  — and drops the `Unconfirmed` record;
- a confirm that finds no matching record — expired, replaced, sent to another
  node, or after that node's restart — is answered `NFS4ERR_STALE_CLIENTID`,
  and the client sends `SETCLIENTID` again.

**NFSv4.1 confirms at `CREATE_SESSION`.** An `EXCHANGE_ID` that names a new
owner, or a known owner with a new verifier, makes an `Unconfirmed` record under
the same rules, and changes no state: the old client ID of a rebooted client
keeps its state until the confirm. The client's first `CREATE_SESSION` naming
the answered client ID and `Sequence`, from the same principal, is the confirm
(RFC 8881 §18.35.4): it calls `Connect`, writes `CSSeq` and `CSReply` in the
same transaction ([§2.1](#2.1%20Client)), and drops the `Unconfirmed` record. One
that finds no matching record is answered `NFS4ERR_STALE_CLIENTID`, and the
client sends `EXCHANGE_ID` again. An `EXCHANGE_ID` with a confirmed owner's own
verifier and principal is not a proposal: it returns the existing client ID at
once. Any `EXCHANGE_ID` or `SETCLIENTID` whose owner a confirmed record holds
under another principal is refused with `ErrClientInUse` and leaves no
`Unconfirmed` record.

> decision: an unconfirmed client is volatile and held only by the node that
> answered `SETCLIENTID` or `EXCHANGE_ID`. Writing it would cost a metadata write per mount
> attempt, including the attempts that never confirm, to spare a client one
> repeated `SETCLIENTID` or `EXCHANGE_ID` when a node restarts or its address
> moves between the two calls; both protocols' clients already repeat it on
> `NFS4ERR_STALE_CLIENTID`.
> Write it, or route the confirm to the answering node, if mounts are seen
> failing because confirms routinely land on another node.

### 2.10 Lock waiters

```go
// LockWaiter is a lock request that conflicted and asked to wait. Volatile,
// held in the file's table at its primary; never reclaimed (§8).
type LockWaiter struct {
	Owner     LockOwner
	Open      OpenID // zero for NLM
	Range     ByteRange
	Exclusive bool
	Arrived   time.Time // orders the waiters (FIFO)
	Deadline  time.Time // NFSv4 only: one lease period after the owner's last poll; zero otherwise
	Reserved  bool      // NFSv4 only: the range is free and held for this owner until Deadline
}
```

A lock request that asks to wait and conflicts **MUST NOT** hold a worker
([§5.2](#5.2%20A%20recall%2C%20and%20why%20nothing%20is%20merged)). The
primary records a `LockWaiter` and answers at once: `ErrLocked` for NFSv4, whose
client polls by sending the request again, and `ErrBlocked` for NLM and SMB,
whose client is answered later through the adapter's callback. A request
without `wait` that conflicts is answered `ErrLocked` and leaves no waiter. One
owner holds at most one waiter per file: its next request replaces it. A repeat
of the same request — same range and same lock type, as an NFSv4 poll or a
re-sent blocking `NLM_LOCK` is — keeps the waiter's `Arrived`, so a client that
re-sends while it waits keeps its place; only a request for a different range
or type takes a new arrival time.

**A lock that must wait for a recall waits as a waiter.** A lock request on a
file whose caching grant another client holds starts the recall
([§7](#7.%20Conflicts%20across%20protocols)) and is decided only once the recall
ends. With `wait`, from NLM or SMB, the primary records a `LockWaiter` and
answers `ErrBlocked`; when the recall ends the waiter is considered like any
other, granted if nothing then conflicts, and otherwise left waiting on the lock
that does. Every other such request — NFSv4 with or without `wait`, NLM or SMB
without it — is answered `ErrDelay` and leaves no waiter: an NFSv4 client
retries, NLM answers a non-blocking request `NLM4_DENIED_GRACE_PERIOD`, which
its clients retry, and SMB holds the request pending
([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)). A recall is not a conflict: an
answer of `ErrLocked` or `NLM4_DENIED` would tell the client the range is held
when it may be free, and an `NLM_BLOCKED` with no waiter would never be granted.

How each protocol waits:

- **NFSv4:** a blocking lock type (`READW_LT`, `WRITEW_LT`) asks to wait. The
  client polls; the server never grants on the client's behalf. When the range
  frees, the waiter is `Reserved`: a conflicting request from anyone else is
  refused until the owner's next poll takes the lock or `Deadline` passes. An
  NFSv4.1 client that asked to be notified is sent `CB_NOTIFY_LOCK`; the
  notification is optional and the poll decides.
- **NLM:** a blocking `NLM_LOCK` asks to wait, answered `NLM_BLOCKED`. When the
  range frees, the primary grants the lock in the step that frees it and sends
  `NLM_GRANTED`. If the host does not acknowledge by the callback's deadline,
  the lock is released and the next waiter considered.
- **SMB:** a lock request without the fail-immediately flag asks to wait; with
  it, a conflict is refused at once. The waiter is answered pending, and when
  the range frees the primary grants it in the step that frees it and completes
  the request.

**First in, first out per range.** When an unlock, close, expiry or revocation
frees a range, the primary considers the waiters whose ranges overlap it in
`Arrived` order and grants, or for NFSv4 reserves, the earliest whose request no
longer conflicts, then the next, until one conflicts. While a waiter waits, no
later waiter and no new request from another owner is granted a lock that
conflicts with the waiter's request, so a stream of shared locks cannot starve
an exclusive waiter. Waiters whose ranges do not overlap are independent.

**A waiter has one lifetime, this one.** It is dropped when it is granted; when
its owner cancels it (`NLM_CANCEL`, SMB `CANCEL`) or, for NFSv4, has not polled
by `Deadline`; when its open closes; for SMB when its connection ends; and with
everything else its client holds when the client's lease expires or its state is
revoked
([§4.3](#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)),
or, for NLM, when its host restarts ([§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)).
Nothing else ends it: an adapter **MUST NOT** impose a timeout of its own, and a
waiting request is exempt from the per-call deadline
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)), since it
holds no worker. A waiter is lost with its primary's table; the client's own
retry, reclaim or reconnect replaces it, and in grace a waiting request is
refused like any new lock.

**No adapter answers a waiting request before the waiter is gone.** An adapter
that answers a request it was told `ErrBlocked` for with anything but the grant
`LockWaitOver` reports — a cancel, a connection that ended, a reply it must send
for its own reasons — **MUST** first call `CancelLock` and answer only once it
returns. `CancelLock` is ordered with the grant at the primary: it either drops
the waiter, so no lock is granted for it afterwards, or reports that the lock was
granted first, and the adapter then answers the request as granted. Without the
order, a lock is granted after its request was answered as failed: a lock no
client knows it holds, which refuses every other client until its owner's state
ends.

> decision: an NLM or SMB waiter has no time limit of its own; it ends on cancel,
> close, restart, disconnect or expiry, because both protocols' clients wait for
> a blocking lock indefinitely and the waiter holds no worker, only a record in
> one file's table, bounded at one per owner per file. Add a limit, here and
> nowhere else, if waiter counts are seen growing without bound.

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
| **layout** (cluster) | a layout request, under an open | a conflicting open, lock or deny mode; a primary change of its shard, or a move of its file | until returned, recalled, revoked or lease expiry |

All of it is held against a **file**, never a name or a handle. A named stream
belongs to its base file here: an open of a stream counts as an open of the base
file for deny modes, for delete and delete pending, and for keeping the file alive
(§9.1), as SMB treats a stream as part of its file. A rename does
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

**One lease owner per client (cluster).** A client's requests arrive at whichever
`protocol` nodes hold its sessions, and its state sits at many primaries; none of
them alone sees all its activity. So exactly one node, the client record's
`LeaseOwner`, renews and expires the client: each node that serves the client
reports activity to it at most once per renewal interval, never per request, and
no primary expires a client on its own. When the lease owner expires the client,
it releases the client's state at every shard its record names (§4.3). The lease
owner is chosen when the record is created, among the nodes holding the client's
sessions, and moves only by a compare-and-swap on the record when that node is
lost; the new owner extends the lease by one period before it may expire it.
Without one owner, a primary that sees nothing from a client busy elsewhere
expires it and revokes state the client is still using.

**The back channel has a home (cluster).** Recalls and notifications go out over
the client's back channel, which one `protocol` node holds: the record's
`Callback`. A primary that must recall routes the callback there. When that node
is lost, the record's `PathDown` is set and every node serving the client
signals it — NFSv4.1 `SEQ4_STATUS_CB_PATH_DOWN` — until the client binds a new
back channel (`BIND_CONN_TO_SESSION`, `CREATE_SESSION`, or for NFSv4.0 a new
`SETCLIENTID` callback), which rewrites `Callback` and clears the flag. While the
path is down no grant is offered (§5.4) and a recall that cannot be sent is
revoked at its deadline. `DESTROY_CLIENTID` reaches the lease owner, which
releases everything as at expiry, and is refused while `Sessions` still lists a
session. An SMB client record names a machine whose sessions belong to many
users (§2.1), so an SMB logoff closes only its own session's opens (MS-SMB2
§3.3.5.6) and releases nothing else; the SMB record is released once it holds no
open and no durable open's `Timeout` is still running.

### 4.2 Grace makes volatile state safe

Open state other than what [§8](#8.%20What%20is%20durable) makes durable **MAY** be held in memory and lost
on restart or primary change. It **MUST NOT** be the reason an operation blocks on
a holder that no longer exists.

A primary that has lost open state **MUST** then run a **grace period** of one
lease period from when it begins serving, ending earlier only as below. During it, it **MUST** refuse every request for open
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
be refused. A reclaim that reaches a shard that did not lose state — the
client was told to reclaim everywhere, but only another shard failed over —
returns the state that shard still holds for the client, and is not refused:
the client is reclaiming what it has. That includes a delegation: a reclaim of an
open that names a delegation the shard still holds returns the open with that
delegation. Only a shard in grace, which lost its grants
([§8](#8.%20What%20is%20durable)), answers such a reclaim without one.

**A reclaim never grants more than the client held.** It **MUST** restore only
the access and deny modes — and the locks and grants — the client claims to
have held, and only after authorising them like a new open against the file as
it is now: its current ACL and mode, and the caller's current share grant. A
reclaim that asks for more access than its claimed open had, or whose principal
may no longer open the file that way, is refused. Grace suspends conflict
checks against lost state; it does not suspend permission checks.

The window **MUST** end on its own ([RFC 0 §10.2](rfc-0-data-lifecycle.md#10.2%20No%20state%20requires%20intervention%20to%20leave)). A server that will not leave a
grace period until an operator acts has replaced one wedge with another. It
**SHOULD** end early, once every client whose record names the shard has
finished reclaiming or has expired: nothing is left that a new request could
take first. A client has **finished reclaiming** when it has:

- **NFSv4.1:** sent `RECLAIM_COMPLETE` for this grace instance (§4.4);
- **SMB:** reconnected every persistent open the shard's durable records hold for
  its `ClientGuid`, or let each one's `Timeout` pass;
- **NFSv4.0 or NLM:** never. Neither protocol has a completion signal, so a shard
  whose records name such a client runs its grace for the whole lease period.

> decision: an NFSv4.0 client or NLM host never counts as finished reclaiming,
> because neither protocol says when a client is done, and any rule that guesses
> — a quiet interval, a first non-reclaim request — lets grace end while a slow
> client still has locks to reclaim, which another client then takes. The cost is
> one lease period of refused new state after a loss, in a shard such a client
> held state in. Overturned if a measured client population shows a signal that
> reliably ends its reclaims, and then for that population only.

A request refused with `ErrGrace` is a retry-later, not a failure: an adapter
that holds such a request pending, as SMB holds an open, **MUST** let it outlive
the grace period — its cap is the remaining grace plus a break deadline, 35 s
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) — so that an
open made during grace is granted when grace ends, rather than failed while
grace still refuses it.

### 4.3 An expired lease releases everything it held, everywhere

When a client's lease expires or its state is revoked, every open, deny mode,
lock, lock waiter ([§2.10](#2.10%20Lock%20waiters)), grant, layout and watch it held **MUST** be released, in every view that records
it, in the same step. State one protocol dropped and another still counts
refuses conflicting requests against a holder that no longer exists, until a
restart.

### 4.4 Grace is per shard

Every open-state operation reaches the primary of the file's shard
([§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary)), so a grace period is scoped to the shards whose primary lost state, and
the rest of the cluster keeps granting. Only a failover loses state: a handover
or a batch move of files hands the table over and runs no grace ([§10](#10.%20Shard%20placement)).

**A grace period is an instance.** Each loss of a shard's open state — a restart
of its primary, a failover — opens a new **grace instance**, named by the shard
incarnation the new primary serves under ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).
Every start of a node, the single node's included, **MUST** raise the
incarnation of every shard it serves in the transaction that starts it, before
any open-state request is answered, so a grace instance never repeats
([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)). Were
it to repeat, the `RECLAIM_COMPLETE` a client sent at mount would end the new
instance at once, its reclaims would be refused as out of grace, and another
client could take its lock. A client's
`RECLAIM_COMPLETE` is tracked per (client, shard, grace instance), durably in the
client record's entry for the shard, so a `RECLAIM_COMPLETE` the client sent at
mount, or after an earlier loss, never ends a later instance's reclaim phase, and
a reclaim in the new instance is accepted from a client that completed an old
one.

**A client must be told to reclaim.** A client whose session survives a failover
— its `protocol` node did not change — sees no server restart and would never
reclaim. When a shard loses its open state, the primary **MUST** therefore signal
every client whose record names the shard:

- **NFSv4.1:** set `SEQ4_STATUS_RESTART_RECLAIM_NEEDED` on the client's next
  `SEQUENCE` reply. That opens a new reclaim phase for the client: it reclaims
  its state, a reclaim reaching a shard that lost nothing is answered with the
  state that shard holds (§4.2), and it sends `RECLAIM_COMPLETE` again, which
  ends its part of this grace instance.
- **NFSv4.0:** answer `NFS4ERR_STALE_CLIENTID` (`ErrStaleClient`) to its
  `RENEW` and to every operation naming its client ID, until it repeats
  `SETCLIENTID` and `SETCLIENTID_CONFIRM`. That is the signal NFSv4.0 clients
  start reboot recovery on; `NFS4ERR_STALE_STATEID` with the client ID still
  valid is answered by a `RENEW`, which succeeds, and the client loops. A
  stateid of the lost shard is still answered `NFS4ERR_STALE_STATEID`: the
  client's `RENEW` that follows is what meets `NFS4ERR_STALE_CLIENTID`, so it
  recovers rather than loops. The loss
  marks the record's entry for the shard, durably, and the signal lasts while
  any entry is marked. The repeated `SETCLIENTID` carries the same owner and
  verifier, so it is not a reboot (§2.1): its confirm keeps the client ID and
  clears the marks. The client then reclaims, and a shard that lost nothing
  returns what it holds (§4.2).
- **SMB:** break the connection, so the client reconnects and reclaims its
  persistent opens. Durable v1 and v2 opens, like volatile ones, are not
  reclaimed across a restart or failover of their primary: a reconnect to one is
  answered `STATUS_OBJECT_NAME_NOT_FOUND`, as MS-SMB2 answers a durable handle the
  server no longer holds ([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)).
- **NLM:** notify it of a restart, so it reclaims its locks ([§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)).

A reclaim is accepted only in a shard the client's record names ([§2.1](#2.1%20Client)).

**A former primary fences itself (cluster).** Grants are replies to clients, which no
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
deadline is generous for that reason: 35 s for an SMB lease or oplock break, the
break timeout MS-SMB2 sets, and one client lease period for an NFS delegation.

A client that let one recall be revoked, or one SMB break time out, **MUST NOT**
be offered grants for one lease period, 90 s, after it, and its other grants
**SHOULD** be recalled: it has shown it does not answer. Each further revocation
or timed-out break restarts the period. The fence is bounded in time, not tied to
the client's lease: an active NFSv4 client renews its lease on every request and
an SMB client has none, so either tie would keep it ungranted for as long as its
record exists, after one missed break.

**An SMB break that times out downgrades; it does not close.** As MS-SMB2
3.3.6.5 does, a lease or oplock break not acknowledged by its deadline sets the
grant to none and proceeds; the opens it covered stay open, with their locks and
deny modes, and the holder may keep writing through them. NFS revocation is
unchanged: a revoked delegation's later writes are refused.

> decision: a timed-out SMB break keeps the holder's opens, as Windows does,
> rather than invalidating them. Invalidating them would close a stalled VM's
> profile container mid-session, which Windows never does. The cost is the one
> Windows accepts: writes the holder buffered under its write caching can still
> arrive through its open after the second client's, ordered by arrival, with no
> error to either. It is bounded to holders that missed a 35 s deadline and then
> resumed. Overturned if a workload shows such late writes corrupting shared
> files that only SMB writers touch; then the opens are invalidated as for NFS,
> and the divergence from Windows is stated to clients' administrators.

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

### 5.5 A client's grants are bounded

Every grant is a table entry at a primary and a recall the primary may one day
have to send. Each primary therefore counts the caching grants each client holds
on its files and keeps the count within a **grant budget** of 4096 per client:

- a client at its budget **MUST NOT** be offered another grant; its opens are
  answered without one ([§5.4](#5.4%20How%20a%20grant%20is%20obtained%2C%20and%20where%20it%20pays));
- when a client reaches its budget, the primary **MUST** ask it to return grants
  down to half the budget, 2048: an NFSv4.1 client by `CB_RECALL_ANY` naming
  2048 as the number to keep, of the kinds it holds; an NFSv4.0 or SMB client,
  whose protocol has no such request, by recalling or breaking its least recently
  used grants one by one;
- an NFSv4.1 client still above 2048 one lease period after `CB_RECALL_ANY` has
  its least recently used grants recalled one by one down to 2048. Each of these recalls ends
  by [§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)'s
  deadline like any other.

A grant is **used** by every open, I/O or lock the primary admits under it; the
primary orders each client's grants by last use, in memory. Recalling the oldest
grant instead would take back the profile container or mailbox a session opened
at sign-in and has used ever since, and keep the files it touched once.

The count is the primary's alone and volatile: a client may hold up to the
budget at each primary it reaches, and a new primary starts every count at zero.

> decision: the budget is 4096 grants per client per primary, and a client at it
> is asked to keep 2048. Grants pay on files one user works on (§5.4), and a
> working set beyond a few thousand files is served nearly as well without them,
> while every grant held is memory at the primary and a recall a conflicting
> client may wait on. Halving, rather than returning one, leaves room for a
> working set to turn over before the next request. Counting per primary needs
> no traffic between nodes. Make the values settings
> ([RFC 13](rfc-13-configuration.md)) if a workload is shown to lose throughput
> at the budget, or a primary's memory is shown to be pressed below it.

## 6. A deny mode is checked at open

A deny mode is evaluated once, when an open is granted, against the opens
already held. It **MUST NOT** be re-evaluated per read or per write of a granted
open: the open that was granted was granted, and a later open cannot
retroactively forbid it.

An anonymous open — NFSv3 I/O, and NFSv4 I/O under the anonymous stateid — was
never granted, so nothing was checked for it. Its reads and writes **MUST** be
checked against the deny modes held, per operation, and refused with
`ErrShareViolation` when one forbids them.

**Handle caching is broken before a sharing violation.** Before refusing any
request for a deny-mode conflict — an open of either protocol, an anonymous read
or write, an NFS `REMOVE` or `RENAME` against a deny-delete — the primary
**MUST** break every handle-caching grant held over an open that causes the
conflict, to the same grant without handle caching, and answer the request
`ErrDelay` ([§5.2](#5.2%20A%20recall%2C%20and%20why%20nothing%20is%20merged)).
Once each break is acknowledged or has timed out, the request's retry checks the
deny modes again: a holder that kept the file open only in its handle cache
closes it on the break, and the request then proceeds. Only a conflict that
remains is refused with `ErrShareViolation`. This is the order MS-SMB2 and
Windows follow; refusing first fails every open, and every NFS write, that
Windows Explorer's own cached handle would block — a deny mode no application
still holds.

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
| a caching grant held by another client — an NFS delegation, an SMB oplock or lease with read or write caching | a byte-range lock request of either protocol, NLM included | the grant is recalled to none, as MS-FSA 2.1.5.7 breaks it, and the lock is decided only after the recall ends; a blocking NLM or SMB request waits as a waiter answered `ErrBlocked`, any other is answered `ErrDelay` ([§2.10](#2.10%20Lock%20waiters)). Without it, an NLM lock and the local lock a write delegation's holder took are both exclusive over one range |
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
| **Client** | **durable**, holding what reclaim and identity need: every field of `Client` ([§2.1](#2.1%20Client)) but `Expires` — its owner, verifier, principal and state protection, its `CREATE_SESSION` sequence and cached reply, its lease owner and back-channel node — the shards it has held state in, with the grace instance of its last `RECLAIM_COMPLETE` in each and, for NFSv4.0, whether a loss there is still to be signalled (§4.4), and whether its state was revoked. `Expires` is **volatile**, held by the lease owner, which extends it by one lease period when it takes the client over or restarts (§4.1) | without it every reclaim after a loss must be refused ([§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe)); one record per client, not per open, rewritten only when one of those fields changes. A durable `Expires` would make every renewal a metadata write: 10⁵ clients renewing is 10⁵ writes per lease period for no reclaim it enables |
| **Open** | **volatile**, reclaimed in grace — except **durable** when it keeps an unlinked file alive ([§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive)) or `Durability` is persistent, and then every field, the SMB identity and lock sequences included ([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)) | a durable record per open costs a write per open; only these two cases lose data or a promise without one |
| **Lock**, **LockState** | **volatile**, reclaimed in grace — **durable** only when its open is persistent | reclaim restores it |
| **CachingGrant** | **volatile**, never reclaimed; an SMB lease survives only inside a persistent open, with its key, parent key, kind and epoch ([§2.4](#2.4%20CachingGrant)) | a lost grant costs a client its cache, never correctness |
| **delete pending** | **volatile** with the opens — **durable** while the file has a persistent open ([§9.4](#9.4%20Delete%20on%20close)) | a pending delete is a promise to the client that set it, which only a persistent open carries across a failover |
| **Watch** | **volatile**, never reclaimed | the client re-registers; the NFSv4.1 specification does not allow reclaiming directory notifications |
| **Layout** (cluster) | **volatile**, reclaimed in grace; recalled, never handed over, when its shard's primary changes or its file moves | a lost layout costs a `LAYOUTGET`; a stale one is refused by epoch |
| **Copy** | **volatile**, never reclaimed ([§2.7](#2.7%20Copy)) | the client runs a lost copy again |
| **Unconfirmed** | **volatile**, at the `protocol` node that answered `SETCLIENTID`, for one lease period ([§2.9](#2.9%20NFSv4.0%20unconfirmed%20clients)) | a lost one costs the client a repeated `SETCLIENTID` |
| **LockWaiter** | **volatile**, never reclaimed ([§2.10](#2.10%20Lock%20waiters)) | the client's poll, reclaim or reconnect asks again |
| **grant budget count** | **volatile**, per primary, starting at zero ([§5.5](#5.5%20A%20client%27s%20grants%20are%20bounded)) | grants are volatile too; the count is rebuilt as they are offered |
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
| durable (v1, v2) and resilient | a disconnect, for `Timeout`; a handover. Not a restart or failover of its primary: a reconnect after one is answered `STATUS_OBJECT_NAME_NOT_FOUND`, as MS-SMB2 answers a durable handle the server no longer holds | the primary's table |
| persistent | a disconnect, for `Timeout`; a handover; a restart or failover of its primary | a durable record ([§8](#8.%20What%20is%20durable)) with every field of the open, its locks and its lease |

**Writes that survive a restart are stable.** Every write through a persistent
open, and every write through any open on a share that offers continuous
availability, **MUST** be stable: synced to the journal before it is acknowledged
([RFC 1 §6.2](rfc-1-journal.md#6.2%20Sync%20policy)), as if the client had asked for write-through. SMB has no
write verifier, so a handle that survives the loss of acknowledged unstable
writes would reconnect over the gap and the client would never resend them: a
profile container would be silently corrupt. A durable v1 or v2 open does not
survive its primary's restart, so its unstable writes need no such rule — the
client finds the open gone and knows its cached state is lost.

**A loss without a restart reaches the SMB opens that wrote it.** The journal
also loses acknowledged unstable writes while its process keeps running: a record
dropped as corrupt, a failed sync window, a re-attached device
([RFC 1 §3.8](rfc-1-journal.md#3.8%20Loss%20events)). An NFS client learns from
the write verifier; nothing on the SMB wire changes, and a durable open carries
on over the gap. So every SMB open keeps `LossSeen`, the file's **loss
sequence** — which the engine raises each time a loss event drops an
acknowledged, unoffloaded write of the file — as sampled when the open was made
and at its last flush. A flush (SMB `FLUSH`) through an open whose `LossSeen` is
below the file's current loss sequence **MUST** fail with `ErrLost`, and set
`LossSeen` to the current value, so each open that was open across the loss
learns of it once, at its next flush, and later flushes succeed. `LossSeen` is
volatile with the open, durable only with a persistent one. The loss sequence
lives for one process and starts again at 0
([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)),
so a persistent open reinstated after its primary's restart or failover has
`LossSeen` reset to 0: a value carried over from the previous process would sit
above the new sequence and let a flush succeed over the new process's loss.

> decision: an SMB open learns of a running-process loss at its next flush, as a
> Linux file description learns of a writeback error at its next `fsync`, rather
> than having the open invalidated. Invalidating every open of the file would
> close a profile container mid-session for the loss of writes another open made,
> and a writer that never flushes has asked for no stability. Overturned if SMB
> clients are shown to depend on unflushed writes surviving, such as an
> application that closes without flushing and treats the close as durable; then
> the opens whose writes the loss record names are invalidated instead.

> decision: durable v1 and v2 opens are not reclaimable across a primary restart
> or failover, only across a disconnect, because their writes are unstable and SMB
> gives the client no signal that unstable writes were lost; reclaiming the handle
> would hide the loss. MS-SMB2 permits the server to lose them. Overturned if
> durable opens' writes are made stable too, at the price of a sync per write on
> every share, in which case they could be reclaimed in grace like persistent
> ones.

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

Every other request on an open is matched by the open's channel sequence, as
MS-SMB2 3.3.5.2.10 does: a write, set-information or control request carrying a
channel sequence older than `ChannelSeq` is refused, and a newer one becomes
`ChannelSeq` only once no request under the value before the current one is
still `Outstanding`. `ChannelSeq` and `Outstanding` are held in the
open at its primary, so a replay reaching the primary through another
`protocol` node is checked against the same state, and are durable with a
persistent open, written in the transaction that applies the request, so a stale
replayed write is still refused after a failover.

**An app instance replaces its predecessor.** A create carrying an
`AppInstance` that an open of the same file already holds closes that open
first, releasing its locks, deny mode and lease, and then proceeds — unless
both carry an `AppVersion` and the new one is not higher, when the create is
refused. This is how a failover cluster, or a desktop session
re-attaching a profile container from another machine, takes over a file its
earlier instance still holds open. The check runs in the open's own step at the
file's primary, so no other open lands between the close and the grant.

The matching open is closed only if it belongs to a **different client
identity** and the caller's maximal access to the file being created includes
read; otherwise the create proceeds as if no instance matched, and meets the
existing open's deny mode like any other create. Without the access condition,
any principal that learns an app instance ID can close another's open; without
the client condition, a client closes its own open by reusing its own ID.

## 9. Open state and the life of a file

### 9.1 An open keeps a file alive

An open is the second holder of a file ([RFC 7 §4.2](rfc-7-namespace-metadata.md#4.2%20Open%20state%20is%20the%20second%20holder)). An unlink that leaves an
open file with no entry **MUST**, in the transaction that removes the entry, make
the open durable; the file's pending release ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)) is written in the
same transaction and names no holders — the durable open records are the only
list of them. When the entry is in another shard, the file's primary makes the
opens durable while it prepares that unlink ([RFC 15 §4.2](rfc-15-topology.md#4.2%20Calls%20that%20touch%20two%20primaries)). An open of a file
that already has no entry, by handle, writes its durable open record in its own
transaction and guards the File record there
([RFC 7 §4.5](rfc-7-namespace-metadata.md#4.5%20A%20release%20re-checks%20its%20holders%20inside%20its%20own%20transaction)). The last close of a file with no entry deletes its durable
open record and reports the file to the filesystem service, which runs the
release ([RFC 17 §5.3](rfc-17-vfs.md#5.3%20Open%20and%20close)); open state never releases a file itself. Held
only in one process past the unlink, an open would let another node release a
file a client still has open, drop its refs, and let sweep delete its content.

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
local tier fills up with content that is synced to the remote tier and cannot be released
because a client left a file open.

### 9.4 Delete on close

SMB deletes a file by marking it, not by removing its name. A file becomes
**delete pending** when an open with `DeleteOnClose` closes (SMB's
`FILE_DELETE_ON_CLOSE` at create), or at once when an open holding delete
access sets the disposition (`FileDispositionInformation`); a set-disposition
of false, through any such open, clears it. Delete pending is held in the
file's table, together with the entry — parent and name — the marking open
was opened through, and the principal that marked it (`DeleteBy`): the one that
asked for delete on close at create, or that set the disposition. That
principal's delete access was checked when it marked the file, and the deferred
unlink runs as it — whoever closes last, a close by timeout included — never as
the last closer, who may hold no delete access at all.

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
recorded entry and its marking principal, in the transaction of the close or
set-disposition that makes it, and deleted by the unlink's transaction or by
the clear.

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
its own shard ([RFC 11 §2](rfc-11-ownership.md#2.%20Shards)) has its count at that shard's primary (cluster), which the
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

Every rule of this section is a cluster rule (cluster) except the last
paragraph's, which binds a single node's restart too
([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)).

- **A handover** of a shard's primary hands the shard's volatile table, with its
  dedup table, to the new primary under the new epoch, after the old primary
  stops granting and before the new one serves ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)). No state
  is lost, so no grace runs and no client is told.
- **A batch move of files** to another shard hands over only those files' entries,
  with their dedup entries, frozen with the batch. The batch's commit transaction
  itself adds the receiving shard to each client record the entries name
  ([§2.1](#2.1%20Client)), and only then does the receiving primary install them and
  serve the files ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)).
  A failover of the receiving shard at any moment after the commit therefore asks
  those clients to reclaim; added after the commit, a failover between the two
  would lose their state with no client told. No grace runs.
- **A failover** loses the volatile table, and starts grace for that shard
  ([§4.4](#4.4%20Grace%20is%20per%20shard)).
- **A layout** is bound to the (shard, epoch) it was granted under. When that
  shard changes primary, by handover or failover, or the file moves to another
  shard, the layout **MUST** be recalled, and
  revoked at the recall deadline; a `LAYOUTCOMMIT` for it then fails with
  `NFS4ERR_BADLAYOUT` ([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS)).

When a shard's primary changes, or any node serving it starts — every start,
in the start transaction (§4.4) — the shard incarnation rises and the NFS write
verifier **MUST** change with it, or clients never resend
writes they sent unstable to the old one ([RFC 11 §7](rfc-11-ownership.md#7.%20Protocol%20state)); the same incarnation
names the new grace instance (§4.4).

## 11. Interface

Signatures are indicative; the obligations above are normative. The filesystem
service ([RFC 17](rfc-17-vfs.md)) is this interface's only caller.

```go
type OpenState interface {
	// Clients.
	Connect(ctx context.Context, c Client) (ClientID, error) // found by (protocol, owner): same verifier returns the ID, a new one is a reboot, another NFSv4 principal is ErrClientInUse (§2.1); called at SETCLIENTID_CONFIRM or the confirming CREATE_SESSION, never at SETCLIENTID or EXCHANGE_ID (§2.9)
	CreateSession(ctx context.Context, c ClientID, seq uint32, reply []byte) ([]byte, error) // checks seq against CSSeq: a replay returns the cached reply, seq+1 records this one, any other is refused (§2.1)
	Renew(ctx context.Context, c ClientID) error             // reaches the lease owner (§4.1)
	Expire(ctx context.Context, c ClientID) error            // releases everything (§4.3)
	Destroy(ctx context.Context, c ClientID, by Principal) error // DESTROY_CLIENTID; refused while a session remains or by a principal state protection excludes
	Rebind(ctx context.Context, c ClientID, node NodeID) error   // a new back channel at node; clears PathDown (§4.1)

	// Opens and deny modes (§6). Every call names its client, and the primary
	// refuses one that names another client's state (§10).
	// OpenRequest carries access, deny, durability, timeout, delete-on-close,
	// the lease key and kind wanted, create GUID, app instance and reclaim.
	Open(ctx context.Context, c ClientID, file FileID, r OpenRequest) (Open, *CachingGrant, error)
	Reconnect(ctx context.Context, c ClientID, id Identity, o OpenID, m ReconnectMatch) (Open, error) // §8.1
	Disconnect(ctx context.Context, c ClientID) error                                                // closes volatile opens, times the rest (§8.1)
	SetDisposition(ctx context.Context, c ClientID, o OpenID, delete bool) error                   // §9.4
	SuspendTimes(ctx context.Context, c ClientID, o OpenID, suspend, resume TimeMask) error         // §2.2
	Close(ctx context.Context, c ClientID, o OpenID) (orphan bool, err error) // the last close may unlink (§9.4); orphan reports a file left with no entry and no open, for the filesystem service to release (§9.1)
	Flushed(ctx context.Context, c ClientID, o OpenID, loss uint64) error // SMB FLUSH: ErrLost once when loss, the file's loss sequence, is above the open's LossSeen (§8.1)

	// Byte-range locks. o is zero for an NLM lock (§2.3); seq is SMB's lock
	// sequence, nil when the request carries none (§8.1). An NFSv4.0 request's
	// owner seqid travels in OpenRequest and in Lock, Unlock and Close options,
	// and is checked at the owner's home primary (§2.8): ErrBadSeqID.
	// wait: the request may wait (§2.10); a conflict then records a waiter and
	// answers ErrLocked (NFSv4) or ErrBlocked (NLM, SMB). A recall of another
	// client's grant answers ErrBlocked with a waiter (NLM, SMB with wait) or
	// ErrDelay (every other request).
	Lock(ctx context.Context, c ClientID, file FileID, owner LockOwner, o OpenID, r ByteRange, exclusive, wait, reclaim bool, seq *LockSequence) error
	// CancelLock is ordered with the grant: it drops the waiter, or reports
	// granted when the lock was granted first. An adapter calls it before any
	// answer to a waiting request other than the grant (§2.10).
	CancelLock(ctx context.Context, c ClientID, file FileID, owner LockOwner, r ByteRange) (granted bool, err error)
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
	ErrBlocked        = errors.New("openstate: lock waits; granted later through the callback")
	ErrClientInUse    = errors.New("openstate: owner held by another principal")
	ErrSeqMisordered  = errors.New("openstate: create-session sequence out of order")
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
| L11 | (cluster) A primary serves no open state past its node lease expiry less the drift bound. |
| L12 | (cluster) A layout is bound to the (shard, epoch) it was granted under and is recalled when that shard changes primary or the file moves. |
| L13 | Every open-state call names its client, and names only that client's state. |
| L14 | A delete-pending file refuses every new open and every rename, keeps its name until its last open closes, and then loses that name through the ordinary unlink; it is durable while the file has a persistent open. |
| L15 | A reconnect matches an open only on client, principal, create GUID (v2) and lease key; a replayed create or lock returns its first result; a create with a held app instance closes the earlier open first only when that open is another client's and the caller may read the file. A persistent open's record holds all of it. |
| L16 | One grant per (client, lease key, file); opens under one key never break their own lease; a v2 lease's epoch rises with every change of its kind; an NFS delegation is never a handle grant. |
| L17 | A lock names an owner, and an NFSv4 or NLM owner's locks never conflict with each other; an NLM lock needs no open. A loss of NLM locks raises the durable NSM state number and notifies every recorded host before grace. |
| L18 | A lost asynchronous copy is reported unknown, never done. |
| L19 | The NFSv4.1 server owner's major ID and server scope are the installation's identity on every node; the minor ID is the node's. |
| L20 | Each write request is applied whole under one version at the file's primary; overlapping writes resolve byte by byte in the primary's arrival order. |
| L21 | A write or read through an open never advances a time that open has suspended; through any other open it does. |
| L22 | An NFSv4.0 owner's sequence is checked and advanced at its home primary in the step that applies the request; no out-of-sequence request changes state. |
| L23 | An SMB rename or delete of a directory is refused while an open made through that directory exists; an NFS one is not. |
| L24 | A reclaim restores only modes, locks and grants the client held, authorised like a new open against the file as it is now; it never grants more. |
| L25 | A client record is found by (protocol, owner); for NFSv4 the same owner from another principal is refused `ErrClientInUse`, and the record keeps its verifier, principal and state protection durably. An SMB record is keyed by `ClientGuid` alone and holds no principal; principals bind per session and per open. |
| L26 | Every call using an open, lock state or layout is refused unless its caller is the opener's principal or a machine principal state protection names; every bearer ID with room — an NFSv4 stateid's `other`, a confirm verifier — carries at least 64 random bits, and an ID without room is honoured only with its principal and session. |
| L27 | An NFSv4 stateid's seqid and an open-owner's confirmation are held as state and change in the step that applies the request. |
| L28 | (cluster) One node, the lease owner, renews and expires each client; no primary expires a client on its own. Recalls route to the client's back-channel node, and a lost one is signalled until the client rebinds. |
| L29 | `RECLAIM_COMPLETE` is tracked per (client, shard, grace instance); every node start raises every shard's incarnation in its start transaction, so no instance repeats; a loss of a shard's state opens a new instance and signals `SEQ4_STATUS_RESTART_RECLAIM_NEEDED` to NFSv4.1 clients and `NFS4ERR_STALE_CLIENTID` to NFSv4.0 clients until they confirm again; a reclaim at a shard that lost nothing returns what it holds, delegations included. |
| L30 | Only a persistent open survives its primary's restart or failover, and every write through one, or through any open on a continuously available share, is stable before it is acknowledged. |
| L31 | Before a sharing violation is returned to any open, anonymous I/O, remove or rename, handle caching over the conflicting opens is broken and the deny modes checked again. |
| L32 | A byte-range lock request recalls every caching grant another client holds on the file before it is decided. |
| L33 | An SMB break that times out downgrades the grant to none and keeps the opens; a revoked NFS delegation's writes are refused. |
| L34 | A deferred delete runs as the principal that marked the file, never as the last closer. |
| L35 | (cluster) A recursive watch whose subtree crosses into another shard is completed with enumerate-directory when a change there matches it. |
| L36 | An open of a named stream is an open of its base file for deny modes, delete and keeping the file alive; no open suspends the change time. |
| L37 | (cluster) A layout stateid's seqid rises with every grant, return and recall of the layout, in the step that applies it, and a recall carries it. |
| L38 | An NFSv4.0 `SETCLIENTID` changes no state until its `SETCLIENTID_CONFIRM`, and an NFSv4.1 `EXCHANGE_ID` none until its first `CREATE_SESSION`; the unconfirmed proposal is volatile at the answering node and confirmed only by its principal within one lease period. |
| L39 | No worker waits on a blocked lock: a waiting request leaves a volatile waiter, waiters overlapping one range are served first in, first out, and every waiter ends on grant, cancel, close, disconnect, its client's expiry, its host's restart, or for NFSv4 a missed poll, and on nothing else; no adapter answers a waiting request other than as granted before `CancelLock` has returned. |
| L40 | Each primary holds at most 4096 caching grants per client; a client at the budget is offered none and is asked to return down to 2048, its least recently used grants first. |
| L41 | A client record's `Expires` is volatile; its `CREATE_SESSION` sequence and cached reply are durable and written once per `CREATE_SESSION`, so a replay anywhere gets the first reply. |
| L42 | I/O under a delegation stateid is served only to the client holding the delegation, and authorised as anonymous I/O is, for the caller's own principal. |
| L43 | Every SMB open that was open across a loss of an acknowledged, unoffloaded write of its file fails its next flush with `ErrLost`, once. |
| L44 | A lock request that meets another client's caching grant is never answered as a conflict while the recall runs: a blocking NLM or SMB request becomes a waiter, any other is answered `ErrDelay`. |
| L45 | A client that let a recall be revoked or a break time out is offered no grant for one lease period after it; the fence ends on its own. |
| L46 | A batch move's commit adds the receiving shard to every client record its open-state entries name; the entries are installed only after it. |
| L47 | Grace lasts one lease period and ends earlier only when every client its records name has finished reclaiming; an NFSv4.0 client or NLM host never has. A request refused `ErrGrace` and held pending by an adapter outlives the grace period. |

## 13. Conformance

Every check runs through the filesystem service, against every backend, in the
index's tiers.

### 13.1 Group A — wrong holder, lost state, stuck client

| Requirement | Check |
| --- | --- |
| [§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary) one table | Take a lock and a deny mode through one adapter, reach the same file through the other. Assert the other observes both. A single-adapter rig cannot fail this. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) grace | Grant a lock, restart, have a different client request the conflicting lock immediately. Assert refusal for the lease period. Request a lock on a file nobody held; assert it is refused too, and that a reclaim is granted. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) reclaim needs a record | Lose the client records, restart, reclaim. Assert refused. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) reclaim grants no more | Open a file read-only with deny-none; fail over. Reclaim it read-write with deny-write: assert refused. Remove the principal's read access, fail over, reclaim the original open: assert refused. Reclaim exactly what was held with access intact: assert granted. A reclaim that trusts the claimed modes passes the third and fails neither of the first two. |
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
| [§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time) recall deadline | Grant an NFS write delegation to a client that never answers recalls, open conflictingly. Assert the open proceeds after the deadline and the silent client's later writes are refused. Repeat with an SMB read-write-handle lease: assert the conflicting open proceeds at 35 s, the lease is none, and the holder's open still exists and its next write is accepted. A design that closes the SMB holder's opens, or that lets the NFS holder write on, fails one half. |
| [§5.2](#5.2%20A%20recall%2C%20and%20why%20nothing%20is%20merged) flush on recall | Buffer writes under a write grant, open from another client. Assert the second client reads the first's data. |
| [§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open) no downgrade | Request write against a deny-write. Assert refusal, never a read-only open. |
| [§7](#7.%20Conflicts%20across%20protocols) cross-protocol | For every row of the table, assert the stated outcome with one protocol holding and the other requesting. |
| [§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive) open-unlinked | Open, unlink, crash. Assert the content survives until grace ends, and is released after it unless the open was reclaimed. |
| [§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive) open-unlinked across protocols | Open a file over NFSv4, unlink it over SMB, evict its content from the journal, read through the NFSv4 handle. Assert the exact bytes, fetched from the remote. Close; assert the release runs and the refs are dropped. A single-protocol rig, or one that never evicts, cannot fail this. |
| [§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive) lazy | From one client, open and close a linked file 10^4 times with volatile opens. Assert exactly one write, the client record's shard list on the first open, and none after. Repeat with persistent opens; assert each open writes its record. |
| [§9.2](#9.2%20A%20new%20primary%20releases%20nothing%20before%20grace%20ends) new primary | Open a file on one primary, move the shard, unlink through the new primary. Assert no release before grace ends. |
| [§9.4](#9.4%20Delete%20on%20close) delete pending | Open a file twice over SMB, the first with delete-on-close; close the first. Assert the name still resolves, a third open through each protocol and a rename are refused `ErrDeletePending`; close the second and assert the name is gone and the file released. Repeat with a second hard link: assert only the opened name goes. |
| [§9.4](#9.4%20Delete%20on%20close) persistent | Set delete pending on a file held by a persistent open, fail the shard over, reconnect. Assert a new open is refused and the last close removes the name. |
| [§7](#7.%20Conflicts%20across%20protocols) remove against deny-delete | Hold an SMB open without share-delete; `REMOVE` the name over NFS. Assert `ErrShareViolation`. Reopen sharing delete, `REMOVE` again: assert the name is gone, the SMB open still reads, and after its close the file is released. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) reconnect matching | Disconnect a durable v2 open; reconnect with its FileId but each of another client GUID, another principal, another create GUID, another lease key. Assert each refused, and the exact match accepted. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) timeout | Disconnect a durable open holding a deny-write; open for write from another client before and after `Timeout`. Assert refused, then granted. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) create replay | Send a `CREATE` with a create GUID, drop the reply, replay it. Assert one open exists and the replay returns its `OpenID`. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) lock replay | On a persistent open, take an exclusive lock with a sequence, drop the reply, fail the shard over, replay. Assert success, not `ErrLocked`, and one lock held. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) app instance | Client A opens a file with an app instance and deny-all; client B opens it with the same app instance. Assert A's open, locks and lease are gone and B is granted; repeat with B's version not higher, assert B refused and A intact. A principal without read access to the file sends the same app instance; assert A's open is untouched and the create meets A's deny mode. A sends a second create with its own app instance; assert its first open is not closed. |
| [§2.4](#2.4%20CachingGrant) lease key | Open a file twice from one client under one key, then under a second key. Assert the second open does not break the first's read-write-handle lease, the third does, and each break carries a higher epoch. Assert an NFS write delegation is never offered with the handle kind. |
| [§2.3](#2.3%20Lock) owners | Two NFSv4 lock-owners of one client lock one range exclusively: assert the second refused. One owner locks overlapping ranges: assert they merge, not conflict. Take an NLM lock: assert it is held with no open and refuses an overlapping NFSv4 lock. |
| [§4.5](#4.5%20NLM%20locks%20and%20restart%20notification) NLM restart | Hold NLM locks from two hosts, fail the shard over. Assert the NSM state number rose before grace, both hosts were notified, their reclaims are granted and a non-reclaim lock is refused `ErrGrace`. Then send a restart notification from one host: assert its locks are released at once. |
| [§2.7](#2.7%20Copy) lost copy | Start an asynchronous copy, fail the destination's shard over mid-copy, ask its status. Assert `ErrNoCopy`, never done. |
| [§2.1](#2.1%20Client) server owner | Send `EXCHANGE_ID` through two `protocol` nodes. Assert one major ID and one scope, two minor IDs, and that the client ID from one is accepted by the other. |
| [§7.1](#7.1%20Writers%20that%20do%20not%20coordinate) unlocked writers | Two clients, one per protocol, write overlapping 1 MiB ranges of distinct patterns concurrently, 10⁴ times, with commits and offloads between. Assert after each round the overlap holds exactly one writer's pattern, whole, and that it is the one the primary acknowledged last. A design that splits one request across versions fails this. |
| [§2.2](#2.2%20Open) suspended time | Open a file twice over SMB; set `Modify` to -1 on the first. Write through the first; assert `Modify` unchanged after the existence commit. Write through the second; assert it advanced. Set -2 on the first, write through it; assert it advanced. Repeat on a persistent open across a failover. |
| [§2.8](#2.8%20NFSv4.0%20owner%20sequences) owner sequence | Over NFSv4.0, one open-owner opens files in two shards with consecutive seqids; assert both accepted. Send a stale seqid to the second shard's file; assert `ErrBadSeqID` and no state change. Refuse an `OPEN` with `ErrDelay`, and a `LOCK` at the second shard's primary with `ErrLocked`; assert each advanced its owner's sequence and that owner's next seqid is accepted. Fail the home shard over; assert the owner's next sequenced request is accepted in grace as a new owner's. |
| [§2.1](#2.1%20Client) owner index | `EXCHANGE_ID` with an owner string, then again with the same verifier from another node: assert the same client ID. Again with a new verifier: assert a new ID and the old one's state released. Again from another principal: assert refused. Restart the node and repeat the first: assert the same ID, found by the index. |
| [§2.2](#2.2%20Open) opener only | Over NFSv4.0, client A opens a file for write; client B, as another principal, writes and closes using A's stateid. Assert both refused `ErrNotYours` and A's open intact. Guess a stateid by changing one bit of A's; assert it names nothing. |
| [§2.2](#2.2%20Open) stateid seqid | Upgrade an open, then send `OPEN_DOWNGRADE` with the pre-upgrade seqid: assert refused as old. Send an `OPEN` from a new NFSv4.0 open-owner and then a `READ` before `OPEN_CONFIRM`: assert refused; confirm, then assert the read accepted. |
| [§4.1](#4.1%20A%20client%20lease) one lease owner | A client holds state in two shards at two primaries and talks only to the first for two lease periods. Assert the second primary does not expire it. Kill the client; assert the lease owner expires it once and both shards release. |
| [§4.1](#4.1%20A%20client%20lease) back channel | Kill the `protocol` node holding a client's back channel; assert `SEQ4_STATUS_CB_PATH_DOWN` on its next `SEQUENCE` through another node, no grant offered until it rebinds, and a recall delivered to the new node after `BIND_CONN_TO_SESSION`. |
| [§4.4](#4.4%20Grace%20is%20per%20shard) reclaim instance | An NFSv4.1 client mounts and sends `RECLAIM_COMPLETE`; fail its shard over. Assert the next `SEQUENCE` carries `SEQ4_STATUS_RESTART_RECLAIM_NEEDED`, its reclaim is accepted, and grace for that shard does not end until it sends `RECLAIM_COMPLETE` again. A design that keeps one completion per client refuses the reclaim. Reclaim the same client's state in a second shard that did not fail over: assert the state is returned, not refused. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) durable after restart | Open a file durable v2, write unstable, restart the primary, reconnect. Assert `STATUS_OBJECT_NAME_NOT_FOUND`. Repeat with a persistent open on a continuously available share: assert the reconnect succeeds and every acknowledged write reads back. A design that reclaims durable opens, or acknowledges persistent writes before sync, fails one half. |
| [§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open) handle break first | Client A opens a file with deny-write and holds a read-handle lease; its application has closed the file. Client B opens for write over SMB. Assert B is answered pending, A gets a handle break, A closes its cached open, and B is granted. Repeat with B an NFSv4 `OPEN` for write, and again with B an NFSv3 `WRITE`: assert each is answered `ErrDelay`, A gets the break, and B's retry succeeds. A server that refuses at once, or breaks only for SMB opens, fails one of the three. |
| [§7](#7.%20Conflicts%20across%20protocols) lock recalls grant | Grant an NFSv4 write delegation; send a blocking NLM lock over the same range from another host. Assert the delegation is recalled, the NLM request is answered `NLM_BLOCKED` with a waiter recorded, and after the holder returns the delegation without a lock on the range the host receives `NLM_GRANTED`. Repeat with the holder flushing a conflicting lock: assert the waiter stays and is granted at its unlock. Repeat non-blocking: assert `NLM4_DENIED_GRACE_PERIOD` while the recall runs and the lock granted on a retry after it. A design that answers `ErrDelay` to the blocking request never grants it; one that answers `ErrLocked` denies a free range. |
| [§9.4](#9.4%20Delete%20on%20close) delete as marker | Principal P with delete access opens a file delete-on-close; principal Q without delete access opens it too; P closes, then Q. Assert the name is removed. Reverse it — Q cannot set delete pending — and assert refused. |
| [§2.5](#2.5%20Watch) recursive across shards | Register a recursive SMB watch on a directory with a per-child shard below it on another primary; create a file in the child. Assert the watch completes with `STATUS_NOTIFY_ENUM_DIR`. A watch held only at its own primary never fires. |
| [§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary) streams | Open a named stream with deny-write over SMB; open the base file for write over NFS. Assert refused. Unlink the base file's name; assert the stream open keeps the file alive. |
| [§2.2](#2.2%20Open) change time never frozen | Set ChangeTime to -1 on an SMB open, write through it, `GETATTR` over NFS. Assert the change attribute advanced. |
| [§9.5](#9.5%20Open%20children%20refuse%20an%20SMB%20rename%20or%20delete%20of%20their%20directory) open children | Open a file in directory D over SMB; rename D over SMB: assert `ErrShareViolation`. Rename D over NFS: assert it succeeds and the open still reads. Open a file two levels below D; assert an SMB rename of D succeeds. |
| [§2.6](#2.6%20Layout) layout seqid | `LAYOUTGET` a range, then recall the layout while a second `LAYOUTGET` reply is in flight. Assert the recall's seqid is above the first reply's and below or equal to the second's, so the client can order them, and that a `LAYOUTRETURN` naming a seqid above the current one is refused. A layout without a seqid gives the client nothing to order by. Guess a layout ID by changing one bit; assert it names nothing. |
| [§2.9](#2.9%20NFSv4.0%20unconfirmed%20clients) unconfirmed client | A confirmed NFSv4.0 client holds a lock. Send `SETCLIENTID` with its owner and a new verifier; assert the lock still held and no client record written. Confirm from another principal, with a wrong confirm verifier, and after one lease period: assert each `NFS4ERR_STALE_CLIENTID`. Repeat and confirm correctly within the period: assert the old client ID expired and its lock released. Restart the answering node between the two calls: assert the confirm is `NFS4ERR_STALE_CLIENTID` and a new `SETCLIENTID` succeeds. A design that acts at `SETCLIENTID` releases the lock at the first step. |
| [§2.10](#2.10%20Lock%20waiters) lock waiters | Hold an exclusive lock; from three owners request overlapping blocking locks in order exclusive A, shared B, exclusive C, while a fourth owner keeps taking and releasing non-waiting shared locks. Release the holder. Assert A is granted (NLM, SMB) or reserved (NFSv4) first, B only after A, C last, that the fourth owner is refused while A waits, and that no worker was held during the wait. Over NFSv4, stop polling for A: assert its reservation ends after one lease period and B proceeds. Expire a waiting client's lease: assert its waiter is gone. Over NLM, assert `NLM_BLOCKED` then `NLM_GRANTED`; over SMB with the fail-immediately flag, assert refusal at once. |
| [§5.5](#5.5%20A%20client%27s%20grants%20are%20bounded) grant budget | One NFSv4.1 client opens 5000 files at one primary with no other client. Assert grants stop at 4096, a `CB_RECALL_ANY` names 2048 to keep, and a client that ignores it is recalled one by one down to 2048 within one lease period plus the recall deadline. Repeat over SMB, writing every few seconds through the first lease granted: assert breaks of the least recently used leases down to 2048, and that the first lease is not broken. A design with no budget grants all 5000; one that evicts oldest first breaks the first lease. |
| [§2.1](#2.1%20Client) SMB machine record | Two principals negotiate with one `ClientGuid` on two connections, as a multi-session host does. Assert both are admitted to one client record that holds no principal. Each opens a file; assert the second principal presenting the first's FileId is refused, and a reconnect of the first's open by the second is refused. A record that binds the `ClientGuid` to its first principal refuses the second negotiate. |
| [§2.1](#2.1%20Client) create-session sequence | Confirm an NFSv4.1 client with `CREATE_SESSION` at the answered sequence; drop the reply; restart the node and replay it through another. Assert the cached reply is returned and no second session or record write. Send sequence + 2: assert `NFS4ERR_SEQ_MISORDERED`. A design without `CSSeq` in the record creates a second session. |
| [§2.1](#2.1%20Client) renewal writes nothing | 10⁵ NFSv4.1 clients renew for two lease periods. Assert no client-record write. Restart the lease owner; assert no client is expired within one lease period of the restart. A durable `Expires` writes once per renewal. |
| [§2.2](#2.2%20Open) delegation stateid I/O | Client C holds a write delegation on a file. Under principal P2, which may write the file, C writes with the delegation stateid: assert accepted. Under P3, which may not: assert `ErrAccess`. Client D presents C's delegation stateid: assert `ErrNotYours`. A design that applies the opener-only rule refuses the first; one that trusts the delegation accepts the second. |
| [§2.9](#2.9%20NFSv4.0%20unconfirmed%20clients) NFSv4.1 unconfirmed | A confirmed NFSv4.1 client holds a lock. `EXCHANGE_ID` with its owner and a new verifier: assert the lock held and no record written; `CREATE_SESSION` from another principal: assert refused; from the same: assert the old client ID expired and its lock released. `EXCHANGE_ID` with the owner from a third principal: assert `NFS4ERR_CLID_INUSE` and no unconfirmed record. |
| [§2.10](#2.10%20Lock%20waiters) one waiter lifetime | Hold an exclusive lock; an SMB client waits on it for five minutes. Assert the request is still pending and is granted on unlock. Race a cancel against an unlock 10⁴ times: assert after each that either the request was answered granted and the lock is held, or it was answered cancelled and no lock is held for it. A design with an adapter timeout fails the first; one that answers before `CancelLock` returns leaves an orphan lock in the second. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) grace length | Restart with only NFSv4.1 clients recorded, all completing: assert grace ends early. Restart with one NFSv4.0 client or NLM host recorded besides: assert grace lasts the full lease period. During that grace, an SMB client opens a new file: assert the open is held pending and granted when grace ends, not failed at 60 s. |
| [§4.4](#4.4%20Grace%20is%20per%20shard) incarnation every start | On a single node, an NFSv4.1 client mounts and sends `RECLAIM_COMPLETE`; restart twice. Assert the incarnation rose at each start, the write verifier changed each time, and after each restart the client's reclaim is accepted and grace does not end before its new `RECLAIM_COMPLETE`. A start that keeps the incarnation ends the second grace at once. |
| [§4.4](#4.4%20Grace%20is%20per%20shard) NFSv4.0 signal | An NFSv4.0 client holds locks in two shards; fail one over. Assert its `RENEW` is answered `NFS4ERR_STALE_CLIENTID` until it repeats `SETCLIENTID` and confirms, that the confirm keeps its client ID, that its reclaim in the failed shard is granted and in the other returns the lock it holds, and that `RENEW` then succeeds. Restart the lease owner before the confirm: assert the signal persists. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) delegation reclaim | An NFSv4.1 client holds delegations in two shards; fail one over. Assert its reclaim in the shard that did not fail over returns the open with its delegation, and in the failed shard the open without one. |
| [§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time) fence ends | An SMB client lets one lease break time out and keeps its connection. Assert it is offered no lease for 90 s and is offered one after. A fence tied to the client's lease never lifts. |
| [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) loss reaches SMB opens | Open a file twice over SMB durable, write unstable through the first, drop the write's journal record as corrupt in the running process. Flush through each open: assert `ErrLost` once each, then success on the next flush. Disconnect and reconnect the first before flushing: assert the reconnected open's flush still fails. A design that relies on the verifier, or on a restart, passes every flush. |
| [§10](#10.%20Shard%20placement) batch move then failover | Move a batch of files holding opens of a client that held nothing in the receiving shard, and fail the receiving shard over right after the batch commits, before it installs the entries. Assert the client is told to reclaim and its reclaim there is accepted. A design that adds the shard after the commit refuses it. |

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
| 10^5 clients renewing | renewals/s at the primary, client-record writes | renewals/s report; writes 0 |

## 14. Observability

Metric names are shown without the deployment's prefix.

| Answers | Metric | Type |
| --- | --- | --- |
| state held, by kind | `openstate_held{kind=client\|open\|lock\|grant\|watch\|layout\|copy\|delete_pending}` | gauge |
| grants offered and declined | `openstate_grants_total{result}` | counter |
| recall time | `openstate_recall_seconds` | histogram |
| recalls revoked at the deadline | `openstate_recalls_revoked_total` | counter |
| conflicts refused, by rule | `openstate_conflicts_total{rule}` | counter |
| shards in grace | `openstate_grace_shards` | gauge |
| grace periods ended, by `reason` = `complete` or `timeout` | `openstate_grace_ended_total{reason}` | counter |
| requests answered `ErrDelay` while a recall is outstanding | `openstate_delays_total` | counter |
| layouts recalled on a primary change | `openstate_layout_recalls_total` | counter |
| reclaims accepted and refused | `openstate_reclaims_total{result}` | counter |
| leases expired | `openstate_expired_total` | counter |

Recall, grant, grace and open-state metrics are defined here only; other RFCs
link to this table. No share or client label. A revoked recall and an expired lease log at `Warn`
with the client and file.

## 15. Open questions

1. **Recall deadlines as settings.** The values are decided — 35 s for an SMB
   break, one lease period for an NFS delegation ([§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)) — whether they become settings
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
