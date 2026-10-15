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
  - "[[rfc-17-vfs]]"
  - "[[rfc-24-resources]]"
  - "[[rfc-27-namespace-migration]]"
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

### In plain words

Every file has one desk where all promises about it are kept: who has it open,
who locked which bytes, who may cache it. The desk sits at the file's primary,
and NFS and SMB requests both come to it. So an SMB lock refuses an NFS write it
forbids, and the check and the write happen in one place. Most of the desk lives
in memory, not on paper. If its node fails or restarts, the one serving it next runs a grace period: for
a while it only gives back what clients say they held. Three things here share
the word "lease", and nothing else:

- a **node lease**: is this server still the primary of its files?
- a **client lease**: is this client still alive? 90 s for NFSv4;
- an **SMB lease**: a promise that the client may cache the file.

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
   new **grace period**: for one lease period it refuses every new open, lock, deny mode, grant or layout, and every read or write a lost lock or deny mode might have forbidden, and lets clients its durable
   records name reclaim what they held. alice-pc reclaims its persistent open,
   whose record survived the restart, with every write it was told was done;
   grace then ends on its own, early once every recorded client has reclaimed.
   Had the open been merely SMB durable, it would not survive the restart: the
   reconnect finds no open, and alice-pc opens the file again.

Without locks, a deny mode or a lease, two clients writing one file get exactly
this and nothing more: each write request is applied whole; where two overlap,
the one that reached the primary later wins, byte by byte; a caching client
sees the other's writes only through NFS close-to-open checks and SMB lease
breaks.

![One table per file at its primary: alice-pc's persistent SMB open o1 (read+write, deny write) with lease L1, a second desktop's SMB open for write, and build01's NFS READ, WRITE and LOCK all go through vfs.Service to N1, which holds the in-memory table for ODFC_alice.vhdx; the second desktop is refused ErrShareViolation and never downgraded, the same app instance closes o1 first, build01's anonymous NFSv3 WRITE is refused by o1's deny write, and after a restart grace admits only reclaims; only the client record and the persistent open are written to the store](img/rfc14-one-table.svg)

### Terms

- **Primary and shard.** This RFC says *primary* for the node that serves a
  file, and *shard* only for RFC 11's record (the unit whose primary changes).
- **Open-state step.** The per-file serialisation every call below runs under
  at the file's primary ([§11](#11.%20Interface)).
- **Grace period** and **reclaim**: after a primary loses open state, the
  window in which only former holders may take state, by reclaiming it
  ([§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe)). Each loss opens a
  new **grace instance** ([§4.4](#4.4%20Grace%20is%20per%20shard)).
- **Delete pending**: SMB's delete on close. The name stays until the last
  open closes; new opens are refused meanwhile ([§9.4](#9.4%20Delete%20on%20close)).
- **SMB durable and persistent opens**: an SMB durable open survives a
  disconnect; a persistent one also survives its primary's restart or
  failover, because it has a durable record and stable writes
  ([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)).

On a first read, skip §2.5–§2.10, §4.4–§4.5, §10, §11 and §13 onward.

### Scenarios

**An NFS build tool and an SMB editor share one file.**

1. A Windows editor holds an SMB byte-range lock on the first 4 KiB of
   `data.bin`.
2. build01 sends an NFS `WRITE` across that range. It is refused: SMB locks are
   mandatory ([§7](#7.%20Conflicts%20across%20protocols)).
3. build01 takes an NFS lock on another range. SMB I/O there is still allowed:
   NFS locks are advisory and gate only lock requests
   ([§7](#7.%20Conflicts%20across%20protocols)).
4. An SMB client asks for a lock overlapping build01's. It is refused
   ([§7](#7.%20Conflicts%20across%20protocols)).

One table serves both protocols, and each lock means what its own protocol says.

**A caching grant that is never returned.**

1. build01 holds an NFS write delegation on `config.json` and buffers its writes
   locally ([§5.1](#5.1%20What%20a%20grant%20is)).
2. alice-pc opens the file over SMB for writing, so the primary recalls the
   delegation ([§7](#7.%20Conflicts%20across%20protocols)).
3. build01's network has failed and it never answers. At the recall deadline,
   one lease period for NFS, the delegation is revoked ([§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).
4. alice-pc's open proceeds. build01's later writes under the grant are refused,
   and it gets no grants for 90 s ([§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).

A promise the server made is always taken back in bounded time; an SMB break
waits 35 s.

A profile container's day across a blip and a restart is
[RFC 22 §2](rfc-22-smb.md#2.%20A%20profile%20container%2C%20end%20to%20end)'s.

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

The entities below are `pkg/openstate`'s ([§11.1](#11.1%20Package%2C%20dependencies%20and%20wiring)).
Only the pieces [§8](#8.%20What%20is%20durable) makes durable reach the
metadata store, as records [RFC 16](rfc-16-metadata-store.md) keys. Signatures
are indicative; the rules around them are normative.

![The entities and what is written down: Client is durable except Expires; Open and Lock/LockState are durable only for a persistent open or one keeping an unlinked file alive; CachingGrant, Watch, LockWaiter, OwnerSeq at its home primary and the Unconfirmed record held at the open state of the node that answered are volatile; Layout is reserved for the cluster; a client holds opens, locks sit under an open, an SMB lease covers its client's opens, and every entry is keyed by the FileID, never a name or handle, and released in one step when its client expires or is revoked](img/rfc14-entities.svg)

### 2.1 Client

```go
// Client is one protocol client instance and its lease. When the lease
// expires, every Open, Lock, CachingGrant and Watch it holds is released in
// one step (§4.3).
type Client struct {
	ID         ClientID
	Protocol   protocol.Kind
	Owner      []byte    // the client's own name for itself: NFSv4 co_ownerid or SETCLIENTID id, SMB ClientGuid, an NLM host's caller_name with the source address it sends from; indexed (§2.1)
	Verifier   [8]byte   // NFSv4 boot verifier, NLM host's NSM state number; a new one under the same Owner is a reboot
	Principal  Principal // NFSv4: who established the client ID; the same Owner from another principal is refused. Zero for SMB and NLM (§2.1)
	MachCred   MachCred  // NFSv4.1 state protection: SP4_MACH_CRED's principal and protected operations, or none
	CSSeq      uint32    // NFSv4.1: the sequence ID of the last CREATE_SESSION applied (RFC 8881 §18.36.4)
	CSReply    []byte    // NFSv4.1: that CREATE_SESSION's reply, opaque here; answers its replay
	Expires    time.Time // volatile, held by the lease owner (§8); zero for an NLM host, which has no lease (§4.5)
	PathDown   bool      // its back channel is down; signalled to the client until it rebinds (§4.1)
	Shards     []ShardID // shards it has held state in; at most a fixed count, then every shard of the share (§8)
	Notify     []byte    // NLM only: where its restart notification goes (§4.5); opaque here

	// (cluster) set at the client's first contact, never imported (§8)
	LeaseOwner NodeID   // the one node that renews and expires this client (§4.1)
	Callback   NodeID   // the gateway node holding its back channel; zero: none
	Sessions   []NodeID // gateway nodes holding a session of this client
}
```

`ClientID` names one client instance as its protocol defines it: NFSv4's client
ID from `EXCHANGE_ID` or `SETCLIENTID` (a machine plus a boot verifier), SMB's
`ClientGuid`, and for NFSv3 locks an NLM host with its NSM state number. Every
`ClientID` is issued by the server, an NLM host's too: the host's
`caller_name` together with the source address it sends from is its `Owner`,
since NLM carries no credential and a name alone could be written by any host
(RFC 21 §6.1), and its NSM state number is its `Verifier`, so a request with a
new state number is a reboot like any other. A client that reboots is a new `ClientID`, which is how its stale state
is recognised and released.

**A client record is found by its owner.** `EXCHANGE_ID`, `SETCLIENTID` and an
SMB negotiate name the client by its own string, not by an ID the server gave;
the record is therefore indexed by (protocol, owner) as well as by ID
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)). A request naming a known owner with the
same verifier and principal returns the existing client ID — over NFSv4.0
through an `Unconfirmed` record that `SETCLIENTID_CONFIRM` confirms, keeping the
ID (§2.9); with a new verifier it
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
principal either; its host is named by its `Owner`, its `caller_name` with the source address it sends from (above).

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
names an open (`Open.Nonce`), a lock state or a delegation (`GrantID`), and an
NFSv4.0 confirm verifier (§2.9) — carries at least 64 random bits; a client
that sees one cannot derive another's. **A stateid is found without a file
handle.** `TEST_STATEID` and `FREE_STATEID` name stateids with no current file
handle (RFC 8881 §18.48, §18.38).

**A state reference names its shard and grace instance.** Open state mints and
resolves a neutral `StateRef`: `{Type StateType /* open, lock state, grant */;
Shard ShardID; Instance uint8 /* low bits of the grace instance it was minted
under (§4.4) */; Token uint64 /* ≥ 64 random bits, unique within Shard */;
Seq uint32 /* the seqid presented with it; zero: current */}`. `Seq` is judged
against the record's in the step that resolves it (§2.2), so `TestState` answers
an old one as it would be answered in use.
Its wire spelling is `pkg/vfs`'s
([RFC 17 §3.4](rfc-17-vfs.md#3.4%20IDs%20and%20their%20encodings),
[RFC 21 §5.2](rfc-21-nfs.md#5.2%20Stateids)). Each primary keeps an index from
`Token` to the record and its file, so a reference resolves by shard, then
token, with no file handle. A request whose file handle names another file is
refused `ErrBadState`. A reference a batch move carried is judged against the
receiving shard's instance as recorded at the batch's commit
([§10](#10.%20Shard%20placement)). **A `GrantID` and a `WatchID` name their
shard the same way**, so a call naming one with no file handle — `AckBreak`,
`Unwatch`, as `TestState` and `FreeState` name a `StateRef` — is routed to that
shard's primary from the ID alone
([RFC 17 §2.1](rfc-17-vfs.md#2.1%20What%20runs%20where)).

**A stateid that misses is stale or bad.** A stateid whose record the index does
not hold is answered stale (`ErrStaleState`) when it may name state the shard
lost, so the client reclaims, and bad (`ErrBadState`) otherwise:

- **stale** while the shard is in grace, or when its `Instance` names an earlier
  grace instance than the shard's current one: it may name state that loss
  took;
- **bad** for every other miss: an expired client's released state, a revoked
  stateid its client has freed, a removed share's, and a forged one resolve to
  nothing alike, since nothing maps a released stateid back to its client.

A client — Linux's, for one — treats a bad stateid as needing no reclaim and
drops the locks under it after a restart; a stale one sends it into reboot
recovery, so it reclaims them
in grace. Only a stateid in its client's revoked set (§5.3) — a recall that
missed its deadline, or state an administrator revoked — is answered revoked
(`ErrRevoked`), and an expired client's stateid is answered expired only while
that client's record is still kept with its state; neither is answered for a
stateid whose record was released. An ID with no room for them is unique and never reused but not
random: `ClientID`, which `clientid4` carries in 8 bytes, and `OpenID`, which
fills SMB's 8-byte persistent FileId together with the shard. Neither is honoured
on its own: a `ClientID` is accepted only under its principal or session, and an
`OpenID` only within the session and for the principal it was opened by (§2.2).
Every call naming any of them is also checked against its holder and the
caller's principal (§10).

> decision: 64 random bits is the floor because it is what a 12-byte stateid
> `other` holds beside what `StateRef` carries,
> and a guesser answered at a server's request rate expects no hit within an
> installation's life. It is not measured against an attack. Raise it if a
> format with room for more is adopted, or if a reachable guessing rate is shown
> to give a hit within that life.

Every node of an installation is one server to a client: one server
owner major ID and scope, its own minor ID
([RFC 21 §3.3](rfc-21-nfs.md#3.3%20Trunking%2C%20server%20owner%20and%20scope)).
Client IDs are unique across the installation, never reused, and outlive a
node's restart, since the record is durable ([§8](#8.%20What%20is%20durable)).

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
	Access        Access     // granted at open: the full mask of RFC 7 §7.7, WRITE_DAC, WRITE_OWNER and a DELETE granted through the parent included
	Privileged    bool       // opened with backup or restore intent by a caller privileged for it (RFC 7 §7.4); every request through the open keeps it
	Deny          Access     // deny mode: SMB share access; NFSv4 deny, which has no delete bit
	Durability    Durability // none, SMB durable, persistent (§8)
	DeleteOnClose bool       // its close makes the file delete pending (§9.4)
	DeleteBy      Principal  // who asked for delete on close or set the disposition; the deferred unlink runs as it (§9.4)
	Grant         GrantID    // the SMB lease or oplock covering it; zero for NFS (§2.4)
	Suspended     TimeMask   // Modify, Access: times a write through this open leaves alone (§2.2); zero: none
	Via           FileID     // the directory it was opened through (§9.5); zero for an open that names no parent — NFSv4 CLAIM_FH, CLAIM_PREVIOUS, CLAIM_DELEG_CUR_FH, CLAIM_DELEG_PREV_FH, an SMB open by file ID — which is then counted under no directory and grants no delete through a parent (RFC 17 §4.6); a reconnect keeps the Via it had
	StateSeq      uint32     // NFSv4: the seqid of the open's stateid, raised by every OPEN upgrade, OPEN_DOWNGRADE and CLOSE
	Nonce         uint64     // NFSv4: the 64 random bits of the open's stateid, bytes 4–11 of `other`, beside its shard number (§2.1); the OpenID is not in the stateid
	LossSeen      uint64     // SMB: the file's loss sequence this open last reported or sampled (§8.1)
	Session       SessionRef // SMB: (gateway node, the node epoch the session was created under, session ID) of the session that opened or last reconnected it; session IDs are unique only per node and per process, and a restarted node takes a new node epoch. A disconnect acts on the opens of a session whose last channel ended (§8.1)

	// A clone's source open (§9.2): held by no client, written by the clone's
	// first phase. Role is client for every other open, and the two fields
	// below are then zero.
	Role         OpenRole // client, clone source
	CloneDst     FileID   // the clone's destination file, whose Removal record holds the clone spec
	CloneVersion Version  // the 16-byte journal version asOf the source was read at (RFC 6 §6.6)

	// Every SMB 3.x open (§8.1); zero for NFS.
	ChannelSeq  uint16    // the open's channel sequence: replays carrying an older one are refused (§8.1)
	Outstanding [2]uint32 // requests in flight under ChannelSeq and under the sequences before it (MS-SMB2 3.3.5.2.10); volatile, zero at a new primary

	// SMB durable (v1, v2) and persistent opens (§8.1); nil for NFS and for a
	// volatile open. A v1 open leaves CreateGUID and the app instance zero.
	Reconnect *ReconnectKeys
}

// ReconnectKeys is what a reconnect or replay of an SMB open matches on.
type ReconnectKeys struct {
	CreateGUID  [16]byte      // identifies the CREATE: replay and reconnect matching
	AppInstance [16]byte      // zero: none
	AppVersion  [2]uint64     // the app instance version, high and low; zero: none
	Timeout     time.Duration // how long it is kept once its client disconnects
	LockSeq     [64]uint8     // lock sequence per index: a valid bit and a 4-bit sequence
}
```

> ponytail: one pointer per open, to stay within RFC 24 M6's 1 KiB per open.
> Inline the fields again if M6 measures the indirection.

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
opener's access. The same holds for a lock state.

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
state's; a request naming an older seqid than the current one is refused
`ErrOldState`, a newer one `ErrBadState`, and seqid zero means the current one
(RFC 8881 §8.2.2). `StateSeq` and `LockState.Seq` hold them, and change in the
step that applies the request; the presented seqid reaches that step once, as
`StateRef.Seq` or `OpenRef.Seq` (§11.3). `Seqs` carries only owner seqids.

**A time can be suspended per open.** `Suspended` names `Modify` and `Access`
only, never the change time
([RFC 22 §9.3](rfc-22-smb.md#9.3%20Timestamps%2C%20allocation%20and%20sparse%20files)
spells the sentinels). A write or read through that open **MUST NOT** advance a
time it names; I/O through any other open, and every explicit set, still does.
The service passes the times to leave alone with the write
([RFC 17 §5.1](rfc-17-vfs.md#5.1%20Write)). Volatile with the open, durable
only with a persistent one.

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
// CHANGE_NOTIFY; NFSv4.1 directory notifications need a directory delegation,
// which is not offered (RFC 21 §5.3). It ends with its client's lease and is
// never reclaimed (§8).
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

A watch is held at its directory's primary, which sees every change in that
directory's shard. **A recursive watch whose subtree crosses into another shard
(cluster)** also leaves a marker, volatile like the watch, at the primary of each
descendant shard whose root lies under it: a change there that the filter
matches is reported to the watch's primary, which completes the watch with
*enumerate directory* — SMB's `STATUS_NOTIFY_ENUM_DIR` — rather than a change list,
since the change's names are another primary's. The client rescans; no change
under a descendant shard is lost, though its detail is. A shard created or moved
under the watched subtree after registration gets its marker when it starts
serving, from the watches its parent's primary holds. On one node every shard has
the same primary, and changes are delivered with their detail.

### 2.6 Layout

A **layout** (cluster) is reserved: a single node offers none, and every layout
operation is answered not supported
([RFC 21 §2.1](rfc-21-nfs.md#2.1%20Versions%20served)). A pNFS RFC adds it as
open state of the file's table, held under an open, released with the client's
lease, bound to the (shard, epoch) it was granted under, and recalled when that
shard changes primary or the file moves
([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS),
[RFC 17 §10](rfc-17-vfs.md#10.%20Open%20questions) Q2).

### 2.7 Copy

Asynchronous copy is not offered: every copy is answered synchronously and
issues no copy stateid ([RFC 21 §9.4](rfc-21-nfs.md#9.4%20Async%20COPY),
[RFC 17 §5.9](rfc-17-vfs.md#5.9%20Copy%20and%20clone)). A copy holds no open
state.

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

**A `LOCK` that introduces a lock-owner is sequenced by its open-owner.** The
first `LOCK` of a lock-owner carries the open stateid, the open-owner's seqid
and the new lock-owner's first seqid (RFC 7530 §16.10). It is checked and
advanced against the open-owner's `OwnerSeq`, at the open-owner's home primary,
as an `OPEN` is; its reply advances the open-owner's sequence under the rule
below, refusals included, and only a granted lock creates the lock-owner's
`OwnerSeq` with `Next` one past the seqid it carried. Every later `LOCK` and
`LOCKU` of that lock-owner carries the lock-owner's seqid alone.

Every in-sequence reply advances `Next`, refusals included, except those
RFC 7530 §9.1.7 exempts; open state stays out of that list's wire names and
reports `ErrStaleClient`, `ErrStaleState`, `ErrBadState` and `ErrBadSeqID`,
which advance nothing
([RFC 21 §4.2](rfc-21-nfs.md#4.2%20Owner%20sequence%20IDs) maps the rest).

**Where it is held.** An owner's `OwnerSeq` is held at its **home primary**:
the primary of the file of the owner's first sequenced request. **(cluster)** A
sequenced request on a file another primary serves checks the sequence at the
home primary first, runs at its file's primary, and then commits the advance at
the home primary on that primary's answer, never before it. A second request of
the owner arriving between the two waits for the first's advance, since the
owner's requests are serial. A lock-owner's locks are almost always on one
file, so the extra call does not arise.

The last reply, for replay, is the adapter's
([RFC 21 §4.2](rfc-21-nfs.md#4.2%20Owner%20sequence%20IDs)).

`OwnerSeq` is volatile, like the opens and locks it sequences. A home primary
that is lost loses it; in the grace that follows, an owner with no `OwnerSeq`
is new, and its first sequenced request sets `Next`, as for any new owner.

### 2.9 NFSv4.0 unconfirmed clients

```go
// Unconfirmed is an NFSv4.0 SETCLIENTID, or an NFSv4.1 EXCHANGE_ID, not yet
// confirmed. Held by open state's client half (§11) in *vfs.Server on the node
// that answered, never by its gateway, and never written to the store.
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

A client is proposed by `Propose` and made current by `Connect`. Four rules
hold the proposal:

1. a proposal changes no state, and a new verifier does not expire the old
   client ID until its confirm;
2. at most one record per owner, and a later proposal replaces it;
3. it expires one lease period after the request;
4. a confirm is accepted only at the answering node, from the proposal's
   principal, naming its client ID and its confirm verifier or sequence, and
   it runs `Connect` in one step ([§2.1](#2.1%20Client)).

A proposal for an owner a confirmed record holds under another principal is
refused `ErrClientInUse` and leaves no record. Wire statuses and the
NFSv4.0/4.1 walk are
[RFC 21 §3.5](rfc-21-nfs.md#3.5%20Confirming%20a%20client)'s.

> decision: an unconfirmed client is volatile and held only by the node that
> answered `SETCLIENTID` or `EXCHANGE_ID`. Writing it would cost a metadata write per mount
> attempt, including the attempts that never confirm, to spare a client one
> repeated `SETCLIENTID` or `EXCHANGE_ID` when a node restarts or its address
> moves between the two calls; both protocols' clients already repeat it on
> `NFS4ERR_STALE_CLIENTID`.
> Write it, or route the confirm to the answering node, if mounts are seen
> failing because confirms routinely land on another node.

> ponytail: a node holds at most 4,096 `Unconfirmed` records, and 16 per
> source address; past either a proposal is answered `ErrDelay`, charged to
> RFC 24's connection-state pool. `Owner` is at most 1,024 bytes, the
> protocols' own opaque limit. Both counts are proposals. Raise them if mounts
> are refused below them.

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

![A lock waiter's life: a conflicting request that does not wait is answered ErrLocked with no waiter; one that waits records a LockWaiter and is answered at once, ErrLocked for NFSv4 which polls and ErrBlocked for NLM and SMB; a lock meeting another client's grant starts the recall and becomes a waiter only if it is a blocking NLM or SMB request, otherwise ErrDelay; when the range frees, waiters are served first in first out, NLM and SMB granted in the freeing step with NLM_GRANTED resent until answered, NFSv4 reserved until its Deadline; a waiter is dropped only on cancel through CancelLock, close, last channel, client expiry or revoke, host restart, DropShare or a missed NFSv4 poll](img/rfc14-lock-waiter.svg)

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
  `NLM_GRANTED`. The lock stays granted, and the primary **MUST** keep resending
  `NLM_GRANTED` until the host acknowledges it, refuses it, cancels the lock or
  restarts, or the lock is released by an administrator revoking the host's
  client ([§4.3](#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)) or by `DropShare` for the share's removal or move. `LockWaitOver` returns the host's answer
  ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)): on an acknowledgement the
  resending stops; on a refusal, or an `NLM_CANCEL` for the lock, the primary
  releases the lock as an unlock would and considers the next waiter; with no
  answer by the call's deadline it calls again. A restart releases the lock with
  everything else the host held ([§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)).
  The resend schedule is the NLM adapter's; the resending is a timer and holds
  no worker. An unacknowledged grant is never released for a silence
  ([§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)'s `decision:`).
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
by `Deadline`; when its open closes; for SMB when its open's session loses its last channel; and with
everything else its client holds when the client's lease expires or its state is
revoked
([§4.3](#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)),
or, for NLM, when its host restarts ([§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)),
or when `DropShare` ends its share (§4.3).
Nothing else ends it: an adapter **MUST NOT** impose a timeout of its own, and a
waiting request is exempt from the per-call deadline
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)), since it
holds no worker. A waiter is lost with its primary's table; the client's own
retry, reclaim or reconnect replaces it, and in grace a waiting request is
refused like any new lock. A waiter has no time limit of its own, since it
holds no worker, only a record bounded at one per owner per file and by §5.5's
per-client caps; add one, here and nowhere else, if waiter counts are seen
growing without bound.

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

## 3. One table per file, at one primary

| State | Granted by | Conflicts with | Lifetime |
| --- | --- | --- | --- |
| **open** | an open | a deny mode ([§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open)) | until close |
| **deny mode** | an open | an open asking for denied access | until close |
| **byte-range lock** | a lock request | an overlapping lock of another owner ([§2.3](#2.3%20Lock)); I/O across protocols ([§7](#7.%20Conflicts%20across%20protocols)) | until unlock, close, or lease expiry; an NLM lock until unlock or its host's restart |
| **delete pending** | an open's close with `DeleteOnClose`, or a set-disposition | every new open; a rename of the file ([§9.4](#9.4%20Delete%20on%20close)) | until cleared, or the last close removes the name |
| **caching grant** | the server, on an open | any conflicting access by another client ([§5](#5.%20Caching%20grants)) | until recalled, revoked, returned or expired |
| **watch** | a watch request | nothing | until cancelled or lease expiry |
| **layout** (cluster) | reserved ([§2.6](#2.6%20Layout)) | | |

All of it is held against a **file**, never a name or a handle. A named stream
belongs to its base file here: an open of a stream counts as an open of the base
file for deny modes, for delete and delete pending, and for keeping the file alive
(§9.1), as SMB treats a stream as part of its file. A rename does
not disturb it, and two hard links to one file are one lockable object. Keying
by handle is the subtler error: one client may hold several handles to one file,
and a lock one handle can see and another cannot is a lock the same client can
take twice.

All of it is held **in one table per file, at the file's primary**
([RFC 15 §3](rfc-15-topology.md#3.%20One%20primary%20per%20shard)). A file is never split across shards. That
primary also runs the file's I/O, so a conflict check and the I/O it admits run
in one process under one epoch, and no grant lands between them. An implementation **MUST NOT** hold any of it in an adapter,
where the other adapter cannot see it: a lock one protocol grants and the other
does not observe is not a lock. Every adapter reaches the table through the
filesystem service ([RFC 17](rfc-17-vfs.md)), which routes to the primary.

**The table is per file and serialised per file.** A file's entry is created
with its first piece of state and freed with its last, so a file with no state
costs nothing. Each entry has its own mutex: the open-state step. No lock spans
files except each primary's stateid index and per-client index, which are taken
after a file's step and only to add or remove an entry. `CheckIO` on a file
with no entry is one map read and allocates nothing.

> ponytail: a file's byte-range locks are a slice sorted by start, so a
> conflict check and an insert are O(n) in the file's ranges. Replace it with
> an interval tree when a profile shows that scan on a file holding more than
> 1,024 ranges.

**Who owns the table.** The table is the concrete `*openstate.Table` in the
service's `Deps`, built and wired as
[§11.1](#11.1%20Package%2C%20dependencies%20and%20wiring) says, and the only
lock manager
([RFC 17 §2.2](rfc-17-vfs.md#2.2%20Who%20owns%20open%20state%20and%20locks)).
The gateway and the adapters hold none of it: open state lives only in
`*vfs.Server`, its per-file area at the file's primary and its client area at
the client's lease owner (§11.3).

## 4. Client leases, grace and reclaim

### 4.1 A client lease

A **client lease** is how long the server keeps a client's state while the
client is silent. Every request from the client renews it (NFSv4 `SEQUENCE` or
`RENEW`); a client with nothing to do sends an empty renewal. A client silent
for a whole lease period is treated as gone. Without that, a laptop that sleeps
holding a lock blocks every other client forever. SMB's durable-handle timeout
plays the same part for SMB clients.

The lease period is fixed, not a setting ([RFC 13](rfc-13-configuration.md)): NFSv4 clients
are given a 90 s lease, advertised in the `lease_time` attribute; an SMB durable
handle is kept for the timeout its create request negotiates, within the bounds
the protocol sets (MS-SMB2 3.3.5.9.10).

> decision: the NFSv4 lease is 90 s, and every period this document states as
> "one lease period" — grace, the grant fence, an NFSv4 waiter's poll deadline, an
> unconfirmed client's life — is derived from it. NFSv4 leaves the value to the
> server; 90 s is long enough that a client is not expired across a brief network
> or suspend gap, and short enough that a failover's grace stays near a minute and
> a half. It is not measured. Shorten it if failover-to-writable time is measured
> to be dominated by grace; lengthen it if clients are measured being expired by
> gaps they survive elsewhere. Either is a change to one value, not a setting,
> since no operator can name a workload one lease suits and another does not.

The client lease shares no record with RFC 11's
[node lease](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)
or with an SMB lease ([§5](#5.%20Caching%20grants)).

**One lease owner per client (cluster).** A client's requests arrive at whichever
gateway nodes hold its sessions, and its state sits at many primaries; none of
them alone sees all its activity. So exactly one node, the client record's
`LeaseOwner`, renews and expires the client: each node that serves the client
reports activity to it at most once per **renewal interval**, never per request, and
no primary expires a client on its own. When the lease owner expires the client,
it releases the client's state at every shard its record names (§4.3). The lease
owner is chosen when the record is created, among the nodes holding the client's
sessions, and moves only by a compare-and-swap on the record when that node is
lost; the new owner extends the lease by one period before it may expire it.
Without one owner, a primary that sees nothing from a client busy elsewhere
expires it and revokes state the client is still using.

> decision: the renewal interval is 10 s, a ninth of the lease: how often a
> serving node reports a client's activity to its lease owner, and the longest
> a cached `ClientStatus` is stale. It is a proposal, not measured; shorten it
> if revoked-state flags are seen arriving late enough to matter.

**The back channel has a home (cluster).** Recalls and notifications go out over
the client's back channel, which one gateway node holds: the record's
`Callback`. A primary that must recall routes the callback there. When that node
is lost, the record's `PathDown` is set and every node serving the client
signals it — NFSv4.1 `SEQ4_STATUS_CB_PATH_DOWN` — until the client binds a new
back channel (`BIND_CONN_TO_SESSION`, `CREATE_SESSION`, or for NFSv4.0 a new
`SETCLIENTID` callback), which rewrites `Callback` and clears the flag. While the
path is down no grant is offered (§5.4) and a recall that cannot be sent is
revoked at its deadline. `DESTROY_CLIENTID` reaches the lease owner, which
refuses it with `ErrClientBusy` while `Sessions` still lists a session or the
client holds any state, releasing nothing (RFC 8881 §18.50), and otherwise
deletes the record. An SMB client record names a machine whose sessions belong to many
users (§2.1), so an SMB logoff closes only its own session's opens (MS-SMB2
§3.3.5.6) and releases nothing else; the SMB record is released once it holds no
open and no SMB durable or persistent open's `Timeout` is still running.

### 4.2 Grace makes volatile state safe

Open state other than what [§8](#8.%20What%20is%20durable) makes durable **MAY** be held in memory and lost
on restart or primary change. It **MUST NOT** be the reason an operation blocks on
a holder that no longer exists.

A primary that has lost open state **MUST** then run a **grace period** of one
lease period from when it begins serving, ending earlier only as below. During it, it **MUST** refuse every request for open
state that is not a reclaim — a new lock, a new open, a deny mode, a grant —
whether or not it appears to conflict: the state it would be checked
against is what was lost, so a conflict cannot be decided. For the same reason
it **MUST** refuse, with `ErrGrace`, every read or write through an anonymous
or unreclaimed open that a lost mandatory lock or deny mode might have
forbidden; without that, an NFSv3 write lands in an extent an SMB client is about
to reclaim a lock on. A client reclaiming what it held
then finds it available, and a client that did not hold it cannot take it first.
Without that window, two clients that were correctly serialised before the
restart are both granted the same lock after it, and neither is told.
Open-state leases are extended by the grace period, so a client is not expired
for time the server spent restarting.

**The move grace is the one exception.** A namespace move that names an
address holds the moved shares at the receiving installation in a narrower
**move grace**: it refuses with `ErrGrace` only non-reclaim NLM locks and NFSv4
opens and locks on them, admits their reclaims, and ends at the earlier of
`migration.reconnect_wait` and one lease period after the address arrives
([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step),
which states why SMB is not held).

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
the grace period — its cap is the remaining grace plus the SMB break deadline
of §5.3, 35 s
([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) — so that an
open made during grace is granted when grace ends, rather than failed while
grace still refuses it.

### 4.3 An expired lease releases everything it held, everywhere

When a client's lease expires or its state is revoked, every open, deny mode,
lock, lock waiter ([§2.10](#2.10%20Lock%20waiters)), grant and watch it held
**MUST** be released, in every view that records it. The release is a mark,
not a scan: the client is marked ended at each primary under that primary's
client index, and from then on every check treats its state as absent. The
entries are then removed file by file, each under that file's step, from the
per-client index, without blocking requests on other files. No request sees
part of an ended client's state. State one protocol dropped and another still
counts refuses conflicting requests against a holder that no longer exists,
until a restart.

**A share's removal or move ends its state.** `DropShare` ends every open,
lock, lock waiter (NLM ones included), grant and watch under the share,
without a recall or a wait for the holder: for a share being removed
([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)),
and at a namespace move's release for each moved share
([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)),
whose FileIDs a move back would meet again. Each is reported through `Revoked`
as `RevokedShare`, with the move's address for a moved share
([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)), and its records go with it, so
a later stateid of the share resolves to nothing and is answered bad, with no
revoked-state flag left to clear; a blocked lock or a pending change
notification is answered rather than left waiting. It starts no §5.3 no-grant
period for the client, and runs whether or not the shard is in grace.

### 4.4 Grace is per shard

Every open-state operation reaches the file's primary
([§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary)), so a grace
period is scoped to the RFC 11 shards, the unit whose primary changes, that
lost state, and the rest of the cluster keeps granting. Only losing the table
loses state — a restart of the primary, a failover, or a handover whose old
primary is lost before the new one holds the table, as listed below; a handover
or a batch move of files that hands the table over runs no grace ([§10](#10.%20Shard%20placement)).

![(cluster) Grace per shard after a failover: N1 dies and its in-memory table for shard A is lost; after N1's node lease lapses plus the drift bound, N2 serves A under a raised incarnation and a new grace instance, granting only reclaims, while shard B keeps granting; alice-pc's connection is broken and it reclaims its persistent open, build01 sees ReclaimNeeded on its next renewal, reclaims and completes, another client's new lock gets ErrGrace and its retry is granted once grace ends early; grace is capped at 90 s and runs that long if an NFSv4.0 client or NLM host is recorded](img/rfc14-grace.svg)

**A grace period is an instance.** Each loss of a shard's open state — a restart
of its primary, a failover, a handover whose old primary is lost before the new
one holds the table — opens a new **grace instance**: the transaction that
starts serving after the loss copies the shard incarnation the new primary
serves under into the shard record's `GraceInstance`, and only such a loss
does. A handover that ships the table raises the incarnation and keeps
`GraceInstance`; one whose table never arrived has no start or takeover
transaction to copy it, so the new primary, already named, writes
`GraceInstance` = its incarnation by compare-and-swap before it serves
([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).
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
— its gateway node did not change — sees no server restart and would never
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
  stateid of the lost shard is still answered `NFS4ERR_STALE_STATEID` (`ErrStaleState`, by its
  `Instance`, [§2.1](#2.1%20Client)): the
  client's `RENEW` that follows is what meets `NFS4ERR_STALE_CLIENTID`, so it
  recovers rather than loops. The loss
  marks the record's entry for the shard, durably, and the signal lasts while
  any entry is marked. The repeated `SETCLIENTID` carries the same owner and
  verifier, so it is not a reboot (§2.1): its confirm keeps the client ID and
  clears the marks. The client then reclaims, and a shard that lost nothing
  returns what it holds (§4.2).
- **SMB:** break the connection, so the client reconnects and reclaims its
  persistent opens; SMB durable and volatile opens are not reclaimed
  ([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)).
- **NLM:** notify it of a restart, so it reclaims its locks ([§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)).

A reclaim is accepted only in a shard the client's record names ([§2.1](#2.1%20Client)).

**A former primary fences itself (cluster).** Grants are replies to clients, which no
receiver can refuse by epoch, so a primary **MUST** stop serving open state at its
node lease expiry less the drift bound, and a new primary starts grace only after the
old primary's node lease has lapsed plus the drift bound ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).

### 4.5 NLM locks and restart notification

NFSv3 locks through NLM (Network Lock Manager), which has no lease: a host's
locks are held until it unlocks them or restarts. Open state owns the durable
NSM state number and decides whom a loss must notify; the NFS adapter's NSM
side spells and sends the notice
([RFC 21 §6.2](rfc-21-nfs.md#6.2%20NSM)).

- **The host restarted.** A restart notice from the host, or a request with a
  new state number, is a `Connect` under the host's owner with a new verifier:
  a reboot ([§2.1](#2.1%20Client)), which expires the old `ClientID`, releasing
  every lock it held ([§4.3](#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)).
- **The server lost locks.** The installation holds one durable **NSM state
  number**. A failover of a shard in which any NLM host held locks **MUST**
  raise it, durably, before the shard's grace begins, and then report the
  loss, through `Revoked` with reason `RevokedShardState`, to every NLM client
  whose record names the shard; the adapter sends the notice to its `Notify`.
  The durable NLM client records are the monitor list; there is no second one.
  The host then reclaims its locks, which grace admits as it admits every
  reclaim ([§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe)), and a
  non-reclaim lock in grace is refused with `ErrGrace`.
- **An address arrives from another installation.** A namespace move raises
  the number when the move's address arrives, above both installations',
  inside the move grace
  ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)
  step 10).

> decision: an NLM lock has no lease, so a host that dies and never comes back
> holds its locks until an administrator revokes its client. This is the NLM
> protocol's own contract, and every NLM server has the same ceiling; a
> server-side timeout would release locks a slow but live host still relies
> on. Add a timeout only if a deployment's NLM hosts are known to be disposable.
> The `lock_grant_unacknowledged` health condition
> ([§14](#14.%20Observability)) names such a host to the administrator.

### 4.6 What each failure does

Each condition of [RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model) is
decided where this table points; it adds no rule.

| Condition | Decided in |
| --- | --- |
| a client falls silent | [§4.1](#4.1%20A%20client%20lease), [§4.3](#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere) |
| a grant's holder falls silent | [§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time) |
| a client's back channel is lost | [§4.1](#4.1%20A%20client%20lease) |
| a gateway node is lost | [§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens) |
| a primary restarts or fails over | [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe), [§4.4](#4.4%20Grace%20is%20per%20shard) |
| a handover's table never arrives | [§4.4](#4.4%20Grace%20is%20per%20shard) |
| an NLM host never returns | [§4.5](#4.5%20NLM%20locks%20and%20restart%20notification) |
| a revoked set is lost | [§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time) |
| the metadata store is unavailable | the `decision:` below |

> decision: the store unavailable while writing a client record, a persistent
> open, a durable open of an unlinked file or delete pending: the call is
> refused with the store's error and changes no volatile state; a renewal,
> which writes nothing, is unaffected. Proposed, not tested against a store
> outage; revisit if a measured outage shows clients needing a softer answer.

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
waits: it is answered with `ErrDelay`, carrying its cause, `DelayRecall`
([§11](#11.%20Interface)) — NFS `DELAY` or `JUKEBOX`, an SMB pending
reply completed later — and proceeds once the acknowledgement arrives or the
recall is revoked.

Nothing is merged: a write grant means the holder was the only writer, so there
is no second version to reconcile. The second client sees the first one's data
exactly as if it had never been cached.

### 5.3 A recall MUST end within a bounded time

A grant is only safe if it can be taken back. A recall **MUST** have a deadline,
and a holder that has not acknowledged by it **MUST** have the grant revoked;
writes it sends under the revoked grant are refused as revoked (`ErrRevoked`,
not `ErrNotYours`), and the revocation is reported to the adapter through
`Revoked` with a revoked-grant reason naming the `GrantID`, so the NFS adapter
sets `SEQ4_STATUS_RECALLABLE_STATE_REVOKED` on the client's next `SEQUENCE`
(RFC 8881 §18.46.3) until the client frees the stateid, and answers its writes
`DELEG_REVOKED`. The primary keeps the client's **revoked set** — (stateid,
shard) of each piece of its state it revoked — volatile with the client's other
state there; `FREE_STATEID` of a revoked stateid answers success and removes it,
and the flag clears when the client's set is empty. A failover loses the set
with the grants, and grace then signals the loss as a lost shard's state (§4.4).

![A recall that is never answered: alice-pc's SMB CREATE of config.json for write conflicts with build01's NFS write delegation; open state sends RecallGrant, the NFS adapter sends CB_RECALL which is never answered, and alice-pc's open is answered ErrDelay at once and held pending by the SMB adapter; at the one-lease-period deadline the delegation is revoked, the retried open is granted, and build01's later write under the delegation stateid is refused ErrRevoked; build01 is then offered no grant for 90 s and its other grants should be recalled, while an SMB holder would instead have its lease dropped to none at 35 s and keep its opens](img/rfc14-recall-deadline.svg)

**An administrator's revoke follows the same path** (RFC 8881 §8.2.4). It
releases every open, lock and grant the client holds, as an expiry does
([§4.3](#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)),
so other clients may take them at once, but keeps each one's stateid in the
revoked set, marked with its cause, and keeps the client record and its
sessions. A call naming one is answered `ErrRevoked`, which the NFS adapter
answers `ADMIN_REVOKED` for every stateid an administrator revoked, a
delegation's included, and `DELEG_REVOKED` only for a grant revoked
at its recall deadline, as RFC 8881 §15.1 defines the two, with
`SEQ4_STATUS_ADMIN_STATE_REVOKED` set until `FREE_STATEID` has freed each.
`FREE_STATEID` reaches the service as `FreeState` (§11). An NFSv4.0 client
has no `FREE_STATEID` and an SMB client nothing like it, so for them the
adapter calls `FreeState` once it has answered the stateid revoked, and the
stateid is bad after; the SMB record is then released as §4.1 says once it
holds nothing. The client then opens afresh under its own client ID.

**(cluster)** The revoked set lives at each shard's primary, but the flags a
`SEQUENCE` reply carries are the client's: in the step that adds to or frees
from its revoked set, each primary reports to the client's lease owner (§4.1)
whether its set still holds admin-cause and deadline-cause entries, and the
lease owner keeps the OR of those reports per shard. That aggregate is the
`ClientStatus` `Renew` returns (§11), which the answering node caches for at
most one renewal interval.

> decision: a cached `ClientStatus` may be one renewal interval stale, so a
> revoked-state flag can reach the client up to that late. It costs no call
> per `SEQUENCE`. Read the aggregate per reply if a client is seen acting on
> state it was not yet told is revoked.

**A revoke outlives a restart.** The revoked set is volatile, but the revoke is
not: the revoke's transaction marks, durably, each of the client's per-shard
keys ([§8](#8.%20What%20is%20durable)) for the shards whose state it released. A reclaim in a marked
shard is refused with `ErrReclaimRevoked` — `NFS4ERR_NO_GRACE` — whether or
not the shard is in grace, until the
client next takes state there by an ordinary, non-reclaim request, whose
transaction clears the mark. Without it, a restart before the client sees
`SEQ4_STATUS_ADMIN_STATE_REVOKED` forgets the revoke while the per-shard key
remains, and the client reclaims in grace the open and lock another client
took after the revoke. `ErrReclaimRevoked` is checked for NFSv4 reclaims
only: an NLM host's revoke is an expiry that deletes its record
([§4.3](#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)),
and an SMB revoke releases its persistent opens, so a later reclaim or
reconnect meets no record (`ErrNoReclaim`).

Waiting indefinitely for a
client that has stopped answering blocks every other client of that file, on
nothing but a promise the server made unprompted. A revoked grant costs one
client its cache; an unbounded recall costs every other client the file. The
deadline is generous for that reason: 35 s for an SMB lease or oplock break, and
one client lease period for an NFS delegation. MS-SMB2 leaves the break
acknowledgment timer to the implementation; 35 s is the value Windows uses, so
SMB clients built against Windows tolerate it, and it is fixed rather than a
setting ([RFC 13](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)). The NFS deadline is derived, not chosen: it is
§4.1's lease, and the grant fence below is that lease again.

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

**No write grant on a setuid or setgid file.** A server **MUST NOT** offer a
grant with write caching — an NFS write delegation, an SMB oplock or lease with
write — on a file whose mode has the setuid or setgid bit, and **MUST** recall
the write caching of any grant held on a file when a mode change sets either
bit. A write clears both bits, and only the server can clear them: a write its
holder buffers never reaches the server, and the Linux client does not clear
them itself under a write delegation, so the bits would outlive the write.
Read grants still pay for such files, which are executables, read far more
than written.

Grants pay for files one user works on: document editors (open, save, reopen),
directory browsing (the handle grant keeps handles across the many small opens a
browse does), builds and version control in a home directory, single-user
shares. They are never offered for files many clients share, and a client that
never gets one still works.

> decision: a file on which 16 clients already hold read grants is offered no
> further grant, and a recall fan-out is sent from a per-primary queue of at
> most 4,096 outstanding callbacks, never under the file's step. The
> conflicting request is answered `ErrDelay` whether or not its recalls have
> been sent. Both values are proposals; raise them if a measured workload
> loses grants it would have used.

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
primary keeps a last-use time on each grant, written atomically at 1 s
resolution on admission; at the budget it scans the client's grants for the
oldest, once per recall batch. Recalling the oldest
grant instead would take back the profile container or mailbox a session opened
at sign-in and has used ever since, and keep the files it touched once.

> ponytail: a scan of up to 4,096 timestamps per budget hit. Keep a per-client
> LRU list if budget recalls appear in a profile.

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

**Every other per-client count is bounded too.**

> decision: each primary holds, per client, at most 16,384 opens, 65,536 lock
> ranges, 4,096 lock owners and 1,024 waiters, proposed, settled with RFC 24's
> M6. A request past one is refused `ErrDelay` (`DelayResources`) before it
> changes anything; a close, a cancel and an unlock that splits no range never
> is. An NFS or NLM unlock of a range's middle adds a range, so at the range cap
> it is refused as a lock past it would be, and the client unlocks a whole
> range or closes instead; an SMB unlock never splits. A client that only
> retries holds its locks until it closes or its lease ends; give splits a
> reserve above the cap if a real workload is seen refused there. The caps sit under
> [RFC 24 §4.6](rfc-24-resources.md#4.6%20Open%20state)'s pool so one client cannot spend
> another's share. Make them settings if a measured client is refused below
> them.

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
| a caching grant held by another client — an NFS delegation, an SMB oplock or lease with read or write caching | a byte-range lock request of either protocol, NLM included | recalled to none, as MS-FSA 2.1.5.7 breaks it, before the lock is decided ([§2.10](#2.10%20Lock%20waiters)) |
| an SMB open whose deny mode includes delete | an NFS `REMOVE` of a name of the file, or a `RENAME` of it or over it | refused, `ErrShareViolation`; checked per operation, as anonymous I/O is ([§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open)) |
| SMB opens that all share delete | an NFS `REMOVE` of the file's name | the name goes now; the opens keep the file alive until the last close ([§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive)) |
| a delete-pending file | an open, through either protocol | refused, `ErrDeletePending` ([§9.4](#9.4%20Delete%20on%20close)) |

Each protocol's lock semantics — mandatory, stacking, owner-is-the-open — is
one value per protocol; the matrix is a function of the two.

A caching grant is recalled by the server in the holder's own protocol, whatever
protocol caused the conflict ([RFC 17](rfc-17-vfs.md)'s callbacks).

### 7.1 Writers that do not coordinate

Two clients that write one file without a lock, a deny mode or a grant are
given exactly this, whatever their protocols, and nothing more:

- **Each write request is applied whole.** One `WRITE` is one engine write
  ([RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more)) and one journal version ([RFC 1 §3.1](rfc-1-journal.md#3.1%20Write)), so its bytes are never
  interleaved with another write's within its extent.
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
| **Client** | **durable**, holding what reclaim and identity need: every field of `Client` ([§2.1](#2.1%20Client)) but `Expires` — its owner, verifier, principal and state protection, its `CREATE_SESSION` sequence and cached reply, its lease owner and back-channel node — the shards it has held state in, with the grace instance of its last `RECLAIM_COMPLETE` in each and, for NFSv4.0, whether a loss there is still to be signalled (§4.4), and whether an administrator's revoke released its state there, which refuses its reclaims in that shard ([§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)). `Expires` is **volatile**, held by the lease owner, which extends it by one lease period when it takes the client over or restarts (§4.1) | without it every reclaim after a loss must be refused ([§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe)); one record per client, not per open, rewritten only when one of those fields changes. A durable `Expires` would make every renewal a metadata write: 10⁵ clients renewing is 10⁵ writes per lease period for no reclaim it enables |
| **Open** | **volatile**, reclaimed in grace — except **durable** when it keeps an unlinked file alive ([§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive)) or `Durability` is persistent, and then every field, the SMB identity and lock sequences included ([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)) | a durable record per open costs a write per open; only these cases, and a clone's source open ([§9.2](#9.2%20A%20new%20primary%20releases%20nothing%20before%20grace%20ends)), lose data or a promise without one |
| **Lock**, **LockState** | **volatile**, reclaimed in grace — **durable** only when its open is persistent | reclaim restores it |
| **CachingGrant** | **volatile**, never reclaimed; an SMB lease survives only inside a persistent open, with its key, parent key, kind and epoch ([§2.4](#2.4%20CachingGrant)) | a lost grant costs a client its cache, never correctness |
| **delete pending** | **volatile** with the opens — **durable** while the file has a persistent open ([§9.4](#9.4%20Delete%20on%20close)) | a pending delete is a promise to the client that set it, which only a persistent open carries across a failover |
| **Watch** | **volatile**, never reclaimed | the client re-registers; the NFSv4.1 specification does not allow reclaiming directory notifications |
| **Layout** (cluster) | reserved ([§2.6](#2.6%20Layout)) | |
| **Unconfirmed** | **volatile**, in open state on the answering node, one lease period ([§2.9](#2.9%20NFSv4.0%20unconfirmed%20clients)) | a lost one costs the client a repeated `SETCLIENTID` |
| **LockWaiter** | **volatile**, never reclaimed ([§2.10](#2.10%20Lock%20waiters)) | the client's poll, reclaim or reconnect asks again |
| **grant budget count** | **volatile**, per primary, starting at zero ([§5.5](#5.5%20A%20client%27s%20grants%20are%20bounded)) | grants are volatile too; the count is rebuilt as they are offered |
| **NSM state number** | **durable**, one per installation ([§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)) | an NLM host recognises a server restart only by a higher number |

**Every durable open record lives under its file's open prefix.** A durable
Open **MUST** be written at `F‖id‖op‖openID`, where `id` is the file it opens — a
named stream's own ID for an open of a stream — and a durable Lock only beneath
its open, at `F‖id‖op‖openID‖l‖…` ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)).
No other key may record that a file is held open; the durable-open index
`S‖id‖do‖shard‖file‖openID`, written and deleted with each record, only
lists them per shard for a new primary
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)). A release finds holders by
range-guarding `F‖id‖op‖` for the file and each of its streams
([RFC 7 §4.5](rfc-7-namespace-metadata.md#4.5%20A%20release%20re-checks%20its%20holders%20inside%20its%20own%20transaction)),
so a durable open written anywhere else is a holder the release cannot see.

Client records are held globally, not per shard, because one client's state spans
many shards; the shard list in each is what scopes its reclaims. The list is
written when the client first takes state in a shard, not per open.

> ponytail: the shard list is bounded by a fixed count, proposed 256. Past it
> the client is recorded as holding state in every shard of the share, which
> widens its grace and loses no reclaim. Raise the count if grace is measured
> widened by it.

**An imported client record merges by owner.** A namespace move's imported
client record merges by (protocol, owner): a record the receiver holds keeps
its `ClientID` and gains the imported shards, any other is minted there, and
the imported opens and locks are rewritten to that ID. An NLM record merges
the same way: its `Verifier` is the host's NSM state number (§2.1), so a
receiver's record for the same owner under a lower state number is expired as
a restart would expire it
([§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)). Node fields are
never imported
([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)).

### 8.1 SMB durable and persistent opens

An SMB client that loses its connection expects to reconnect and find its opens.
What the server keeps, and for how long, is set per open by its create request:

| Kind | Survives | Held as |
| --- | --- | --- |
| volatile | nothing: a disconnect closes it | the primary's table |
| SMB durable (v1, v2) | a disconnect, for `Timeout`; a handover. Not a restart or failover of its primary: a reconnect after one finds no open ([RFC 22 §6.4](rfc-22-smb.md#6.4%20Durable%20and%20persistent%20handles) spells it) | the primary's table |
| persistent | a disconnect, for `Timeout`; a handover; a restart or failover of its primary | a durable record ([§8](#8.%20What%20is%20durable)) with every field of the open, its locks and its lease |

RFC 22 §6.4 draws what a reconnect finds after each kind of loss.

**Writes that survive a restart are stable.** Every write through a persistent
open, and through any open on a continuously available share, **MUST** be
stable before it is acknowledged
([RFC 1 §5.2](rfc-1-journal.md#5.2%20Sync%20policy); the service's rule,
[RFC 17](rfc-17-vfs.md#6.%20Invariants%2C%20and%20the%20test%20of%20each) V19):
SMB has no write verifier, so only stable writes let an open survive a lost
primary.

> decision: continuous availability, and with it persistent opens, is opt-in per
> share and off by default ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)). A share holding per-user profile
> containers **SHOULD** enable it in a cluster: only a persistent open survives
> its primary's failover ([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)), so on any other share a failover breaks
> every open container. Only the shares that need it pay for stable writes and
> one store record per open. Overturned if measured persistent-open cost is low
> enough to make it the cluster default.

**A loss without a restart reaches the SMB opens that wrote it.** The journal
also loses acknowledged unstable writes while its process keeps running: a record
dropped as corrupt, a failed sync window, a re-attached device
([RFC 1 §3.8](rfc-1-journal.md#3.8%20Loss%20events)). An NFS client learns from
the write verifier; nothing on the SMB wire changes, and an SMB durable open carries
on over the gap. So every SMB open keeps `LossSeen`, the file's **loss
sequence** — which the engine raises each time a loss event drops an
acknowledged, unoffloaded write of the file, and for every file attached to a
device when that device is lost, since a lost device writes no loss record
([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)) — as sampled when the open was made
and at its last flush. A flush (SMB `FLUSH`) through an open whose `LossSeen` is
below the file's current loss sequence **MUST** fail with `ErrLost`, and set
`LossSeen` to the current value, so each open that was open across the loss
learns of it once, at its next flush, and later flushes succeed. `LossSeen` is
volatile with the open, durable only with a persistent one. The loss sequence
lives for one process and starts again at 0
([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)),
so a persistent open reinstated after its primary's restart or failover, or
imported by a namespace move ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)), has `LossSeen` reset to 0: a value carried over from the previous process would sit
above the new sequence and let a flush succeed over the new process's loss.

> decision: an SMB open learns of a running-process loss at its next flush, as a
> Linux file description learns of a writeback error at its next `fsync`, rather
> than having the open invalidated. Invalidating every open of the file would
> close a profile container mid-session for the loss of writes another open made,
> and a writer that never flushes has asked for no stability. Overturned if SMB
> clients are shown to depend on unflushed writes surviving, such as an
> application that closes without flushing and treats the close as durable; then
> the opens whose writes the loss record names are invalidated instead.

> decision: SMB durable v1 and v2 opens are not reclaimable across a primary restart
> or failover, only across a disconnect, because their writes are unstable and SMB
> gives the client no signal that unstable writes were lost; reclaiming the handle
> would hide the loss. MS-SMB2 permits the server to lose them. Overturned if
> SMB durable opens' writes are made stable too, at the price of a sync per write on
> every share, in which case they could be reclaimed in grace like persistent
> ones.

**A lost gateway node (cluster).** A session lives at the gateway node that
holds it, and a lost node calls no `Disconnect`. So the primary treats every
open whose `Session` belongs to a node whose node lease lapsed as
disconnected at the lapse ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)): its `Timeout` starts then, and a
reconnect through another node finds it attached to no live session.

**A disconnect.** When an SMB session's last channel ends, the volatile opens
that session made close; losing one channel of a multichannel session while
another stays up closes nothing (MS-SMB2 3.3.7.1). `ClientGuid` names a machine,
so a disconnect is scoped by the open's `Session`, never by the client record:
one machine's other sessions keep their opens. Its SMB durable and persistent opens stay, with every lock, deny
mode and lease they hold, and conflict as before; each is closed when `Timeout`
has passed since the disconnect without a reconnect. A close by timeout is a
close: it releases what the open held and can run a pending delete
([§9.4](#9.4%20Delete%20on%20close)). A new primary counts every persistent open as disconnected from
the moment it began serving, so the disconnect time is never written. A
persistent open a namespace move carried starts its `Timeout` at its own
client's first tree connect to its share at the receiving installation,
bounded by `migration.reconnect_wait`
([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)).

**A reconnect matches an open only when all of these do:** the client
(`ClientGuid`), the principal, the open's `CreateGUID` for a v2 request, and the
lease key when the open has a lease. Anything else is refused as if the open did
not exist. Without the principal, a client that learned another's FileId takes
over its open; without the lease key, a reconnect attaches an open to a lease of
another file. A reconnect that matches rebinds the open's `Session` to the
reconnecting session, in the step that answers it and, for a persistent open,
durably: a disconnect acts on the opens of the session that ended, so an open
left naming its first session would be timed by no later disconnect and never
closed. `Session` names the gateway node, the node epoch the session was
created under and the session ID, since each node numbers its sessions alone
and a restarted node may number one again.

**Replays are answered, never applied twice.** A `CREATE` marked as a replay
whose `CreateGUID` names an existing open of the same client returns that open.
A lock request on an SMB durable v2 or persistent open names an index
and a sequence; if `LockSeq` at that index already holds that sequence, the
request is a replay and is answered success without being applied, and
otherwise the sequence is stored with the lock's result. For a persistent open,
`LockSeq` is written in the transaction that writes the lock, so the answer
survives a failover. Without it, a lock re-sent after a lost reply conflicts
with itself.

`ChannelSeq` and `Outstanding` are held in the open at its primary and checked
as MS-SMB2 3.3.5.2.10 checks them
([RFC 22 §6.5](rfc-22-smb.md#6.5%20Replay) states the rule), so a replay
reaching the primary through another node is checked against the same state. A
persistent open's `ChannelSeq` is written before the request that raises it
runs, a read included; `Outstanding` is never written, and a new primary
starts both counts at zero.

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
([RFC 7 §4.5](rfc-7-namespace-metadata.md#4.5%20A%20release%20re-checks%20its%20holders%20inside%20its%20own%20transaction)).
That guard aborts the open only when the release commits first; a guard binds
only its own transaction, so an open that commits first is caught by the
release's own range guard over `F‖id‖op‖` (§8), not by the open's guard. The last close of a file with no entry deletes its durable
open record and reports the file to the filesystem service, which runs the
release ([RFC 17 §5.3](rfc-17-vfs.md#5.3%20Open%20and%20close)); open state never releases a file itself. Held
only in one process past the unlink, an open would let another node release a
file a client still has open, drop its refs, and let sweep delete its content.

A share's root has no entry from its creation and is never released by an
open's close: it goes only with its share ([RFC 7 §4.1](rfc-7-namespace-metadata.md#4.1%20%60nlink%60%20is%20exactly%20its%20entries)). This section's "file with no
entry" therefore excludes it. An open of a share's root is treated as an open
of a linked file: it writes no durable open record, and its last close reports
no orphan. A named stream's opens follow its base file ([RFC 7 §4.5](rfc-7-namespace-metadata.md#4.5%20A%20release%20re-checks%20its%20holders%20inside%20its%20own%20transaction)).

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

**Grace's end closes every durable open record nobody reclaimed.** The durable
open records of the shard's files outlive the primary that wrote them, and an
unlink made even a volatile open durable (§9.1). When grace ends, the primary
**MUST** close, as a close would, every open record in the shard that is not a
persistent open, not a clone's source open, and was not reclaimed — found
through the shard's range of the durable-open index, written and deleted with
each record ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), never by a scan of every file of the share —: delete the record and its locks, and
report each file left with no entry and no holder to the filesystem service,
which runs its release. A persistent open not reconnected is closed the same way
once its `Timeout` has passed since the primary began serving (§8.1). A
clone's source open is held by no client, rebuilt by the new primary from the
destination's `Removal` record, kept through grace, and closed only by the
clone's last batch
([RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally),
which also refuses a clone across shards at admission). Swept at grace end, it would let an unlinked source be released
mid-clone and carry `Lost` into the destination. Without this step a record whose open died with its primary is the file's only holder
for good, and its content is never released.

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
the last closer, who may hold no delete access at all. That principal's delete
access is decided at open against the file and the directory it was opened
through
([RFC 7 §7.7](rfc-7-namespace-metadata.md#7.7%20What%20a%20caller%20may%20be%20asked%20for%2C%20and%20what%20removing%20a%20name%20needs)).

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
The unlink runs as `DeleteBy` through the marking open's reference, so that
open's grant, a backup or restore intent's privilege included, authorises it
([RFC 17 §5.3](rfc-17-vfs.md#5.3%20Open%20and%20close)); delete pending
therefore keeps that reference with the entry and the principal, and so does
its durable record. An unlink still refused — the directory gained an entry, or
the file's or directory's rights changed since the mark — is reported to no
client, since the close it rode on has been answered or had none behind it: it
is logged and counted
([RFC 7 §8.2](rfc-7-namespace-metadata.md#8.2%20A%20delete%20on%20close%20is%20an%20ordinary%20unlink%2C%20later),
[RFC 17 §7](rfc-17-vfs.md#7.%20Observability)), and the entry and the file stay.
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

Open state is held by the **file's primary** ([RFC 15 §3](rfc-15-topology.md#3.%20One%20primary%20per%20shard)) and
moves with it. It is fenced by that primary's epoch: an open-state change carried
by a superseded primary is refused, and every open-state call carries the
`ClientID` it acts for, which the primary checks owns the open, lock, grant or
watch named.

Every rule of this section is a cluster rule (cluster) except the last
paragraph's, which binds a single node's restart too
([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)).

- **A handover** of a shard's primary hands the shard's volatile table, with the
  dedup entries of its open-state calls, to the new primary under the new epoch,
  after the old primary stops granting and before the new one serves ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)). No state
  is lost, so no grace runs and no client is told. The open-state entries are
  few next to writes, and without them the retry of an open-state call — an
  SMB `CREATE`, `CLOSE` or `LOCK`, an NFSv4.1 `OPEN`, `OPEN_DOWNGRADE`,
  `CLOSE`, `LOCK` or `LOCKU`, a sequenced NFSv4.0 operation
  ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)) — whose
  reply the handover lost runs again at the new primary: a second open, a lock
  that conflicts with the client's own, a close answered bad stateid, a
  `BAD_SEQID`. Only these are shipped: a namespace mutation's
  request record is durable already
  ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)),
  and a journal operation's dedup entry is not shipped
  ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)).
- **A batch move of files** to another shard hands over only those files' entries,
  with the dedup entries of their open-state calls and of the namespace
  mutations on them, frozen with the batch. The batch's commit re-keys each
  shipped namespace mutation's request record, with its expiry index entry,
  and the files' per-shard index entries from the giving shard to the
  receiving one ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed), [RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)), so a retry after the receiving
  shard's next handover or takeover is still answered, not run again. The batch's commit transaction
  itself adds the receiving shard to each client record the entries name
  ([§2.1](#2.1%20Client)), and only then does the receiving primary install them and
  serve the files ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)).
  A failover of the receiving shard at any moment after the commit therefore asks
  those clients to reclaim; added after the commit, a failover between the two
  would lose their state with no client told. No grace runs. A stateid and an
  SMB persistent `FileId` name the shard that held their record when issued
  (§2.1), so the batch's commit writes at the giving shard a durable
  **forwarding entry** naming the receiving shard and its `GraceInstance` at
  the commit; a call naming one is answered with a routing error, never bad
  stateid, and a miss at the receiving shard is judged as §2.1 says. The entry
  is deleted lazily, never on the close path (L7); its life is
  [RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries) step 4's.
  `TEST_STATEID`, `FREE_STATEID` and a durable reconnect after a move find
  their state there, a failover of the giving shard included.
- **A failover** loses the volatile table, and starts grace for that shard
  ([§4.4](#4.4%20Grace%20is%20per%20shard)).

When a shard's primary changes, or any node serving it starts — every start,
in the start transaction (§4.4) — the shard incarnation rises and the NFS write
verifier **MUST** change with it, or clients never resend
writes they sent unstable to the old one ([RFC 11 §7](rfc-11-ownership.md#7.%20Protocol%20state)). The incarnation names a
new grace instance only when the change lost the shard's open state (§4.4); a
handover that ships the table keeps the instance.

## 11. Interface

Signatures are indicative; the obligations above are normative. The filesystem
service ([RFC 17](rfc-17-vfs.md)) is this interface's only caller.

Every call takes its open-state steps in
[RFC 17 §5.0](rfc-17-vfs.md#5.0%20One%20lock%20order)'s one lock order, and
holds none while the service holds it behind a clone or chunk.

### 11.1 Package, dependencies and wiring

```go
package openstate // pkg/openstate

type Deps struct {
	Store   *metadata.Store   // RFC 16: the OpenRecords view only — client records, durable opens and their locks, delete pending, the NSM number
	Pool    *resources.Pool   // RFC 24 §4.6: the open-state pool
	Clock   clock.Clock       // the only time source
	Metrics *metrics.Registry
	Logger  *slog.Logger
}

// New builds an empty table and starts nothing. *Table is the only
// implementation; the service holds it as a concrete type (RFC 17 §2.3).
func New(d Deps) (*Table, error)

// Start takes the Sink: the service is built after the table and holds it,
// so a Sink in Deps would make the two constructors need each other.
func (t *Table) Start(ctx context.Context, sink Sink) error
func (t *Table) Stop()

// Sink is how open state reaches clients. The service implements it and
// routes each event to the client's Callbacks (RFC 17 §3.2). Every method
// queues and returns; a failed or late delivery of a client's event counts as
// the client not answering.
type Sink interface {
	RecallGrant(c ClientID, g CachingGrant, to GrantKind)
	Notify(c ClientID, w WatchID, ch []Change)
	Revoked(r Revocation)
	LockWaitOver(c ClientID, w LockWaiter, granted bool) // the service forwards it and reports the host's answer through WaitAnswered
	Closed(f FileID, r CloseResult) // a close open state made itself; the service's own, never forwarded (below)
}
```

No method reads the wall clock: every lease, deadline, poll, fence, grace and
`Timeout` here is read from `Deps.Clock`.

**A close the table makes itself reaches the service.** A `Timeout` that
passes, grace's end (§9.2), an expiry or revoke (§4.3) and an app instance's
replacement (§8.1) close opens with no `Close` call to answer. When such a
close leaves an orphan or a pending unlink, the table calls `Closed` once,
after the step commits, and the service runs it as it runs a `Close`'s
`CloseResult` ([§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive),
[§9.4](#9.4%20Delete%20on%20close)). A process lost before it runs loses
nothing durable: the pending release was written with the unlink and runs at
the next start ([RFC 17 §5.3](rfc-17-vfs.md#5.3%20Open%20and%20close)), a
persistent open's delete pending is durable, and a volatile one is lost as a
failover loses it (§9.4).

`Revocation`, `RevokeReason`,
`ClientStatus`, `StateRef` and `LockRange` are declared here and aliased by `pkg/vfs`
([RFC 17 §3.0](rfc-17-vfs.md#3.0%20Value%20types)), since open state cannot
import vfs; `LockReq` is open state's own, and the service copies `pkg/vfs`'s
request's ranges into it whole (§11.3). The import rule — only the filesystem service imports this package
— is held by the architecture test
([RFC 20](rfc-20-adapters.md)), not by the package's location.

The package is laid out as method sets over the per-file entry:

```text
pkg/openstate/
  doc.go deps.go types.go errors.go sink.go
  table.go step.go            per-file entries and the step
  clients.go lease.go         Propose, Connect, Renew, Expire, Revoke; the lease clock
  open.go deny.go reconnect.go delete.go
  lock.go waiters.go grant.go recall.go
  grace.go reclaim.go stateref.go ownerseq.go
  watch.go check.go
  ranges/                     pure leaf
  leasefsm/                   pure leaf, SMB lease states, ported
```

There is no durable-store interface of open state's own and no separate test
package: the store is RFC 16's, faults are injected below it, and the suite
runs in the package ([§12](#12.%20Invariants%2C%20and%20the%20test%20of%20each)).

### 11.2 Lifecycle

The node ([RFC 23 §2.2](rfc-23-control-plane.md#2.2%20Start%20and%20stop)), which `node.New` builds it into, calls `Start(ctx, sink)`
with the built `*vfs.Server` as the `Sink`, after the store and before the service's `Start`
([RFC 17 §2.5](rfc-17-vfs.md#2.5%20Lifecycle)), and `Stop` after the
service's `Close`, before the store's. `Start` reads each served shard's
`GraceInstance` and incarnation, already raised by the node's start
transaction ([§4.4](#4.4%20Grace%20is%20per%20shard)), enters grace for each,
and arms the lease, recall, waiter-deadline, grant-fence and `Timeout` timers
on `Deps.Clock`. A timer firing is a call into the same per-file step. There is
no drain: draining revokes nothing, and open state is left to the next start's
grace ([RFC 17 §2.5](rfc-17-vfs.md#2.5%20Lifecycle)). `Stop` stops the
timers; `Close` is the per-open call of §11.3.

### 11.3 Methods

Methods are grouped by area, and each area names where it runs. Every call
names its client, and the primary refuses one that names another client's
state ([§10](#10.%20Shard%20placement)).

```go
// Clients — in *vfs.Server at the client's lease owner (RFC 17 §2.2).
Propose(ctx, c Client) (Proposal, error)                        // §2.9; changes no state
Connect(ctx, c Client, p *Proposal) (ClientID, error)           // §2.1; nil p for SMB and NLM; a new verifier under a known owner is a reboot, an NLM restart notice included
CreateSession(ctx, c ClientID, seq uint32, reply []byte) ([]byte, error)
Renew(ctx, c ClientID) (ClientStatus, error)
Expire(ctx, c ClientID) error                                   // releases everything; the record goes (§4.3)
Revoke(ctx, c ClientID) error                                   // releases everything; keeps the record, sessions and revoked set (§5.3)
Destroy(ctx, c ClientID, by Principal) error                    // ErrClientBusy while a session or state remains; releases nothing
Rebind(ctx, c ClientID, node NodeID) error                      // clears PathDown; node is (cluster)

// Per-file — at the file's primary.
Open(ctx, c ClientID, f FileID, r OpenRequest) (OpenResult, error)
Reconnect(ctx, c ClientID, id Identity, o OpenID, m ReconnectMatch, s SessionRef) (Open, error)
Downgrade(ctx, c ClientID, o OpenRef, q Seqs, access, deny Access) error
ConfirmOpen(ctx, c ClientID, o OpenID, seq uint32) error
SetDisposition(ctx, c ClientID, o OpenID, delete bool) error
SuspendTimes(ctx, c ClientID, o OpenID, suspend, resume TimeMask) error
Close(ctx, c ClientID, o OpenRef, q Seqs) (CloseResult, error)  // CloseResult{Orphan bool; Unlink *PendingUnlink}: open state never unlinks; the service runs it
Flushed(ctx, c ClientID, o OpenID, loss uint64) error
Disconnect(ctx, c ClientID, s SessionRef) error                 // closes the session's volatile opens; starts the rest's Timeout

Lock(ctx, c ClientID, f FileID, r LockReq) error                // LockReq{Owner; Open OpenRef; Ranges []LockRange; Wait, Reclaim bool; Seq *LockSequence; NFS Seqs}: one step
CancelLock(ctx, c ClientID, f FileID, o LockOwner, r ByteRange) (granted bool, err error)
WaitAnswered(ctx, c ClientID, w LockWaiter, refused bool) error // the NLM host's answer to LockWaitOver
TestLock(ctx, c ClientID, f FileID, o LockOwner, r ByteRange, exclusive bool) (*Lock, error)
Unlock(ctx, c ClientID, f FileID, ref OpenRef, o LockOwner, rs []ByteRange, q Seqs) error
ReleaseLockOwner(ctx, c ClientID, o LockOwner) error

AckBreak(ctx, c ClientID, g GrantID, to GrantKind) error        // to GrantNone: a return (DELEGRETURN, a lease released)
Watch(ctx, c ClientID, dir FileID, recursive bool, m ChangeMask) (WatchID, error)
Unwatch(ctx, c ClientID, w WatchID) error

ReclaimComplete(ctx, c ClientID) error                          // ends the client's part of every grace instance its shards are in
TestState(ctx, c ClientID, s []StateRef) []error                // nothing changed
FreeState(ctx, c ClientID, s StateRef) error                    // a revoked entry, or a lock state holding no locks
DropShare(ctx, share ShareID, why DropReason) error             // §4.3
CheckIO(ctx, o OpenRef, r ByteRange, write bool) error
CheckChange(ctx, f FileID, by ClientID, what ChangeMask) error
```

The `ctx` arguments are `context.Context`; every method is on `*Table`. The
types the methods name:

```go
// OpenRequest is what an open asks for.
type OpenRequest struct {
	Principal     Principal     // the opener, resolved by the service: Open.Principal and the initial DeleteBy
	Session       SessionRef    // SMB: the opening session (Open.Session); zero for NFS
	Privileged    bool          // backup or restore intent the caller is privileged for (Open.Privileged)
	Access, Deny  Access
	Durability    Durability
	Timeout       time.Duration // SMB: the timeout the create negotiated
	DeleteOnClose bool
	Lease         LeaseKey      // SMB: the lease key; zero: none
	Want          GrantKind     // the grant kind asked for
	Reconnect     *ReconnectKeys // SMB durable and persistent: the timeout; v2 also create GUID, app instance
	Reclaim       bool
	OwnerSeq      *uint32       // NFSv4.0: the open-owner's seqid, checked at its home primary (§2.8)
	Via           FileID        // the directory it was opened through (§9.5)
}

// LockRange is one range of a LockReq; an SMB LOCK may mix shared and
// exclusive elements. pkg/vfs aliases it (RFC 17 §3.0).
type LockRange struct {
	ByteRange
	Exclusive bool
}

// Seqs is the owner seqids an NFSv4.0 request presents; zero for NFSv4.1, SMB,
// NLM and NFSv3. The stateid's own seqid is never here: it travels once, as the
// OpenRef's or StateRef's Seq.
type Seqs struct {
	Owner    *uint32 // NFSv4.0: the owner's seqid (§2.8) — the lock-owner's, or the open-owner's for a LOCK introducing its lock-owner, an OPEN_DOWNGRADE or a CLOSE
	NewOwner uint32  // NFSv4.0, a LOCK introducing its lock-owner: that lock-owner's first seqid
}

type OpenResult struct {
	Open     Open
	Grant    *CachingGrant // nil: none offered
	Replayed bool          // a replayed create answered with the open it made
}

// ClientStatus is what a renewal reports beside the lease; RFC 17's
// ClientStatus carries the same three fields to the adapters.
type ClientStatus struct {
	AdminRevoked      bool // a stateid an administrator revoked, of any kind, is in the client's revoked set (§5.3): SEQ4_STATUS_ADMIN_STATE_REVOKED
	RecallableRevoked bool // a grant revoked at its recall deadline is: SEQ4_STATUS_RECALLABLE_STATE_REVOKED
	ReclaimNeeded     bool // a shard of its state started a new grace instance (§4.4)
}
```

`OpenRef` names an open (`OpenID`), a delegation (`GrantID`) or the anonymous
open, with the handle and the seqid presented (`Seq`, zero: current) — for a
`LOCK` or `LOCKU` naming a lock stateid, that stateid's seqid;
`StateRef` ([§2.1](#2.1%20Client)) is its stateid form.
`LockReq` is passed by value; `Owner.Owner` is copied only when a lock or
waiter is stored, so a refused or tested request allocates nothing. A request
naming several ranges ([RFC 17 §5.11](rfc-17-vfs.md#5.11%20Locks)) is taken in
one step, all or none, and only a single range may wait. Its SMB `Seq` is
checked once before the first range and stored once with the whole result
([§8.1](#8.1%20SMB%20durable%20and%20persistent%20opens)), so neither a later
range nor a retry after a refusal reads as a replay. A conflict
with `Wait` records a waiter and answers `ErrLocked` (NFSv4) or `ErrBlocked`
(NLM, SMB); a recall of another client's grant answers `ErrBlocked` with a
waiter (NLM, SMB with `Wait`) or a `DelayError` (every other request)
([§2.10](#2.10%20Lock%20waiters)). An adapter calls `CancelLock` before any
answer to a waiting request other than the grant. `CheckChange` recalls grants
and never waits; on a remove or rename it also refuses with
`ErrShareViolation` against a deny-delete, a rename of a delete-pending file
with `ErrDeletePending`, and an SMB rename or delete of a directory with
`ErrShareViolation` while any open was made through it
([§7](#7.%20Conflicts%20across%20protocols),
[§9.4](#9.4%20Delete%20on%20close),
[§9.5](#9.5%20Open%20children%20refuse%20an%20SMB%20rename%20or%20delete%20of%20their%20directory)).

> decision: one `*Table` with two areas, not two types. The client area runs at
> the lease owner and the per-file area at the file's primary; on one node both
> are one process and the boundary is per method, as RFC 17 §2.1 keeps its own.
> Overturned when the cluster's remote primary lands: the client area becomes
> its own type in `*vfs.Server` at the lease owner.

```go
// DelayError is a retry-later. errors.Is(err, ErrDelay) holds for it, and
// RFC 17 §4.3's Delay maps it one to one.
type DelayError struct{ Cause DelayCause } // DelayRecall: a recall is outstanding; DelayResources: a cap or RFC 24's pool (§5.5)

// The filesystem service returns these to adapters as RFC 17 §4.3's errors:
// the state refusals as ErrState kinds (ErrStaleClient as StateError
// StaleClient, and so on), the rest under their own names.
var (
	ErrGrace          = errors.New("openstate: in grace period")
	ErrShareViolation = errors.New("openstate: deny mode conflict")
	ErrLocked         = errors.New("openstate: range locked")
	ErrNoReclaim      = errors.New("openstate: nothing to reclaim")
	ErrReclaimRevoked = errors.New("openstate: reclaim in a shard where an administrator revoked the client's state")
	ErrStaleClient    = errors.New("openstate: client expired, or marked by a loss of its shard's open state")
	ErrRevoked        = errors.New("openstate: state revoked, by a recall past its deadline or an administrator, and not yet freed")
	ErrBadState       = errors.New("openstate: stateid names no record")
	ErrStaleState     = errors.New("openstate: state lost with its shard's open state")
	ErrOldState       = errors.New("openstate: stateid seqid older than the state's")
	ErrDelay          = errors.New("openstate: retry later")
	ErrNotYours       = errors.New("openstate: state held by another client")
	ErrDeletePending  = errors.New("openstate: file is delete pending")
	ErrBadSeqID       = errors.New("openstate: owner sequence out of order")
	ErrBlocked        = errors.New("openstate: lock waits; granted later through the callback")
	ErrClientInUse    = errors.New("openstate: owner held by another principal")
	ErrClientBusy     = errors.New("openstate: client still holds a session or state")
	ErrSeqMisordered  = errors.New("openstate: client sequence out of order")
)
```

## 12. Invariants, and the test of each

Every test runs in the table's own package suite — `*Table` over the
in-memory store, a recording `Sink`, the fake clock — unless it names another
layer. Every duration is advanced on the fake clock, never waited for; a check
that needs real time is an e2e check and says so. A row asserting a wire value
names the RFC 21 or RFC 22 row that asserts it; here it asserts the neutral
error or `ClientStatus` field. Deleted numbers are not reused.

| # | Invariant | Test |
| --- | --- | --- |
| L1 | All open state is keyed by file, held in one table per file at the file's primary, and visible to every adapter; that primary also runs the file's I/O. | Service layer, cross-protocol: for every row of [§7](#7.%20Conflicts%20across%20protocols)'s table, assert the stated outcome with one protocol holding and the other requesting. A single-adapter rig cannot fail this. |
| L2 | After a loss of open state, every non-reclaim request, and every I/O a lost lock or deny mode might forbid, is refused `ErrGrace` for that shard until grace ends, and grace ends on its own. | Grant a lock, restart, have a different client request the conflicting lock at once: assert refused for the lease period; a lock on a file nobody held is refused too, and a reclaim is granted. Hold an SMB mandatory lock, fail over, write the range anonymously before the reclaim: assert `ErrGrace`. |
| L3 | A reclaim is accepted only from a client whose durable record names the shard, and every client so named is told to reclaim. | Lose the client records, restart, reclaim: assert `ErrNoReclaim`. Client C holds state only in shard U1, D a lock in U2; fail U2 over: assert C's reclaim of D's lock is refused. |
| L5 | A recall ends within its deadline, by acknowledgement or revocation, and no worker waits on it. | Hold grants on 10⁴ files with a client that never answers; send conflicting opens from another. Assert each gets `ErrDelay` at once, unrelated `CheckIO` p99 stays within 2× its idle value, and no goroutine is blocked on a recall (counted). |
| L6 | A deny mode is checked once, at open, for a granted open, and per operation for an anonymous one; a conflicting open is refused, never downgraded. | Hold an SMB deny-write, write anonymously: assert `ErrShareViolation`. Request write against a deny-write: assert refused, never a read-only open. |
| L7 | An open that keeps an unlinked file alive is durable by the time the unlink commits. Opening and closing a linked file writes no open record unless the open is persistent, and otherwise writes at most the client record's shard list, once per client and shard. | Open, unlink, crash: assert the content survives until grace ends and is released after it unless the open was reclaimed. From one client, open and close a linked file 10⁴ times with volatile opens: assert exactly one write, the shard list on the first open; repeat with persistent opens, assert each open writes its record. |
| L8 | After a failover, nothing is released before grace ends, except by a share's removal, whose share answers every reclaim `ErrStale`; a handover or a batch move of files hands the state over and runs no grace. | **(cluster)** Hold opens, locks and a grant, move the shard: assert no grace and every holding intact. Open a file, move the shard, unlink through the new primary: assert no release before grace ends. `DropShare` a share holding a blocked lock and a pending watch: assert both are answered, and a reclaim there is `ErrStale`. |
| L9 | Open state never makes an extent ineligible for eviction or reclamation. | Engine layer: hold an open and a lock on a file, evict and reclaim its extent: assert both proceed. |
| L10 | Open state and shard placement share no records. | L48's store scan: no key an open-state call writes is a shard record's. |
| L11 | (cluster) A primary serves no open state past its node lease expiry less the drift bound. | Pause a primary past its node lease, let a new primary finish grace, resume the old one: assert it grants nothing. |
| L13 | Every open-state call names its client, and names only that client's state. | RFC 17 V8. |
| L14 | A delete-pending file refuses every new open and every rename, keeps its name until its last open closes, and then loses that name through the ordinary unlink; it is durable while the file has a persistent open. | Open a file twice over SMB, the first delete-on-close; close the first. Assert the name resolves, a third open through each protocol and a rename are refused `ErrDeletePending`; close the second, assert the name gone and the file released; with a second hard link, only the opened name goes. Repeat on a persistent open across a failover. |
| L15 | A reconnect matches only on client, principal, create GUID (v2) and lease key, and rebinds the open to the reconnecting session; a replay returns its first result; an app instance closes only another client's open, for a caller that may read the file. | Reconnect an SMB durable v2 open with each of another client GUID, principal, create GUID and lease key: assert each refused, the exact match accepted. Replay a create with a dropped reply: assert one open, `Replayed`. On a persistent open, replay a lock with a sequence after a failover: assert success and one lock; a two-range lock under one sequence takes both ranges, and its retry after a refusal is applied, not answered as a replay. Reconnect in S2, drop S2's last channel: assert `Timeout` starts; equal session IDs on two nodes: disconnecting one leaves the other's opens. App instance: B with A's instance closes A's open; a lower version, a caller without read, and A's own second create do not. |
| L16 | One grant per (client, lease key, file); opens under one key never break their own lease; a v2 lease's epoch rises with every change of its kind; an NFS delegation is never a handle grant. | Open a file twice from one client under one key, then under a second key: assert the second open does not break the first's lease, the third does, each break carries a higher epoch, and no delegation is offered with the handle kind. |
| L17 | A lock names an owner, and an NFSv4 or NLM owner's locks never conflict with each other; an NLM lock needs no open. A loss of NLM locks raises the durable NSM state number and reports it before grace. | Two NFSv4 lock-owners of one client lock one range: the second refused; one owner's overlapping ranges merge. An NLM lock is held with no open and refuses an overlapping NFSv4 lock. Fail over a shard holding two hosts' NLM locks: assert the number rose before grace, both are reported through `Revoked`, their reclaims granted and a non-reclaim lock `ErrGrace`; a restart notice from one releases its locks at once. |
| L20 | Each write request is applied whole under one version at the file's primary; overlapping writes resolve byte by byte in the primary's arrival order. | Service layer: two clients, one per protocol, write overlapping 1 MiB extents of distinct patterns 10⁴ times, with commits and offloads between: assert the overlap holds one writer's pattern, whole, the one acknowledged last. |
| L21 | A write or read through an open never advances a time that open has suspended; through any other open it does. | Suspend `Modify` on the first of two SMB opens; write through each: assert unchanged, then advanced; resume, write: advanced. Repeat on a persistent open across a failover. |
| L22 | An NFSv4.0 owner's sequence is checked and advanced at its home primary as §2.8 says; no out-of-sequence request changes state. | One open-owner opens files on two primaries with consecutive seqids: both accepted; a stale seqid: `ErrBadSeqID`, no change. An `ErrDelay` open and an `ErrLocked` lock each advance the sequence. Fail the second primary over: a sequenced request is `ErrStaleState` and does not advance. Fail the home primary over: the next request is accepted in grace as a new owner's. RFC 21 §12 asserts the wire values. |
| L23 | An SMB rename or delete of a directory is refused while an open made through that directory exists; an NFS one is not. | Open a file in D over SMB: an SMB rename of D is `ErrShareViolation`, an NFS one succeeds and the open still reads; an open two levels below D does not refuse it. |
| L24 | A reclaim restores only modes, locks and grants the client held, authorised like a new open against the file as it is now; it never grants more. | Open read-only with deny-none; fail over. Reclaim read-write with deny-write: refused. Remove the principal's read access, fail over, reclaim the original: refused. Reclaim exactly what was held: granted. |
| L25 | A client record is found by (protocol, owner); for NFSv4 the same owner from another principal is refused `ErrClientInUse`, and the record keeps its verifier, principal and state protection durably. An SMB record is keyed by `ClientGuid` alone and holds no principal. | Connect an owner, again with the same verifier through another node: the same ID; with a new verifier: a new ID and the old state released; from another principal: refused; after a restart: the same ID. Two principals negotiate one `ClientGuid`: one record, no principal; the second presenting or reconnecting the first's open is refused. |
| L26 | Every call using an open or lock state is refused unless its caller is the opener's principal or a machine principal state protection names; every bearer ID with room carries at least 64 random bits, and an ID without room is honoured only with its principal and session. | Mint 2¹⁶ stateids and flip each of the 64 random bits of each: assert no flip names a record. A counter fails at the low bits. The opener-only half is RFC 17 V18. |
| L27 | An NFSv4 stateid's seqid and an open-owner's confirmation are held as state and change in the step that applies the request. | Upgrade an open, then `Downgrade` with the pre-upgrade seqid: `ErrOldState`, and `TestState` with it too. A new NFSv4.0 open-owner's read before `ConfirmOpen`: refused; after it: accepted. |
| L28 | (cluster) One node, the lease owner, renews and expires each client; no primary expires a client on its own. Recalls route to the client's back-channel node, and a lost one is signalled until the client rebinds. | A client holds state at two primaries and talks only to the first for two lease periods: the second does not expire it; kill the client: it is expired once and both release. Kill its back-channel node: `PathDown` is reported through another node, no grant is offered until `Rebind`, and a recall then reaches the new node. |
| L29 | `RECLAIM_COMPLETE` is tracked per (client, shard, grace instance), every node start raises every shard's incarnation, and a loss signals every client its records name to reclaim. | An NFSv4.1 client completes, its shard fails over: `ClientStatus.ReclaimNeeded`, its reclaim accepted, grace not ended before it completes again; a reclaim at a shard that lost nothing returns its state, delegations included. On one node, restart twice: the incarnation rises each time (the verifier is RFC 8 E27) and each grace waits for a new completion. An NFSv4.0 client is answered `ErrStaleClient` until it confirms again, across a lease-owner restart. A handover whose old primary dies before the table arrives: `GraceInstance` = the new incarnation before serving. |
| L30 | Only a persistent open survives its primary's restart or failover, and every write through one, or through any open on a continuously available share, is stable before it is acknowledged. | Open SMB durable v2, write unstable, restart, reconnect: assert no open (`ErrNoReclaim`; RFC 22 §17 asserts the wire status). Repeat persistent on a continuously available share: the reconnect succeeds and every acknowledged write reads back. |
| L31 | Before a sharing violation is returned to any open, anonymous I/O, remove or rename, handle caching over the conflicting opens is broken and the deny modes checked again. | RFC 17 V21. |
| L33 | An SMB break that times out downgrades the grant to none and keeps the opens; a revoked NFS delegation's writes are refused. | A write delegation's holder never answers a recall; open conflictingly: the open proceeds after the deadline and the holder's later writes are `ErrRevoked`. Repeat with an SMB read-write-handle lease: the open proceeds at 35 s, the lease is none, and the holder's open still exists and its next write is accepted. |
| L34 | A deferred delete runs as the principal that marked the file, never as the last closer. | P with delete access opens delete-on-close; Q without opens too; P closes, then Q: the name is removed. Q cannot set delete pending. |
| L35 | (cluster) A recursive watch whose subtree crosses into another shard is completed with enumerate-directory when a change there matches it. | Register a recursive watch above a child shard on another primary; create a file in the child: the watch completes with enumerate-directory. |
| L36 | An open of a named stream is an open of its base file for deny modes, delete and keeping the file alive; no open suspends the change time. | Open a stream deny-write over SMB; open the base for write over NFS: refused; unlink the base's name: the stream open keeps the file alive. Suspend the change time on an SMB open and write: the change attribute advanced. |
| L38 | A proposal changes no state until its confirm; it is volatile in the answering node's open state and confirmed only by its principal within one lease period. | A confirmed client holds a lock; `Propose` its owner with a new verifier: the lock held and no record written; confirm from another principal: refused. RFC 21 §12 holds the wire walk. |
| L39 | No worker waits on a blocked lock: a waiter is volatile, served first in, first out per range, and ends only on what §2.10 lists; an NLM grant is resent, never released for a silence; no adapter answers a waiting request other than as granted before `CancelLock` returns. | Hold an exclusive lock; owners request blocking exclusive A, shared B, exclusive C while a fourth takes non-waiting shared locks; release: A first, B, then C, the fourth refused while A waits, no worker held. An NFSv4 waiter that stops polling loses its reservation after a lease period. An NLM grant whose acknowledgements are dropped for ten minutes is still resent and held; a refusal releases it. An SMB waiter pending five minutes is granted on unlock. Race a cancel against an unlock 10⁴ times: granted and held, or cancelled and none held. |
| L40 | Each primary holds at most 4096 caching grants per client; a client at the budget is offered none and is asked to return down to 2048, least recently used first. | An NFSv4.1 client opens 5000 files at one primary: grants stop at 4096, a recall-any names 2048, and one that ignores it is recalled down to 2048 within a lease period plus the deadline. Over SMB, writing through the first lease every few seconds: breaks reach 2048 and spare the first lease. |
| L41 | A client record's `Expires` is volatile; its `CREATE_SESSION` sequence and cached reply are durable and written once per `CREATE_SESSION`. | Confirm at `CreateSession`, drop the reply, restart and replay through another node: the cached reply and no second session; sequence + 2: `ErrSeqMisordered`. 10⁵ clients renew for two lease periods: no record write; restart the lease owner: none expired within a lease period. |
| L42 | I/O under a delegation stateid is served only to the client holding the delegation, and authorised as anonymous I/O is, for the caller's own principal. | RFC 17 V24. |
| L43 | Every SMB open that was open across a loss of an acknowledged, unoffloaded write of its file fails its next flush with `ErrLost`, once. | Two SMB durable opens, write unstable through the first, drop its journal record as corrupt: each `Flushed` is `ErrLost` once, then succeeds; a reconnect before flushing does not clear it. |
| L44 | A byte-range lock request recalls every caching grant another client holds on the file before it is decided, and is never answered as a conflict while the recall runs: a blocking NLM or SMB request becomes a waiter, any other is answered `ErrDelay`. | Grant a write delegation; a blocking NLM lock from another host: the delegation recalled, `ErrBlocked` with a waiter, and after a return with no lock the waiter granted through `LockWaitOver`; with the holder flushing a conflicting lock, the waiter is granted at its unlock. Non-blocking: `ErrDelay` while the recall runs, granted on a retry after. |
| L45 | A client that let a recall be revoked or a break time out is offered no grant for one lease period after it; the fence ends on its own. | An SMB client lets a break time out and stays connected: no lease for 90 s, one after. |
| L46 | A batch move's commit adds the receiving shard to every client record its open-state entries name; the entries are installed only after it. | **(cluster)** Move a batch holding a client's opens to a shard it held nothing in, and fail that shard over right after the commit: the client is told to reclaim and its reclaim accepted. |
| L47 | Grace lasts one lease period and ends earlier only when every client its records name has finished reclaiming, except a move grace (§4.2); a request refused `ErrGrace` and held pending outlives it. | Fail over with two recorded NFSv4.1 clients that complete: grace ends early. With an NFSv4.0 client or NLM host recorded besides: the full lease period, and an SMB open held pending through it is granted when grace ends. |
| L48 | Every durable open is recorded at `F‖id‖op‖openID` of the file or stream it opens, and every durable lock beneath its open; no other key records that a file is held open. A clone's source open is one, of its own role: grace end never closes it, only the clone's last batch does. | Make every kind of durable open — of an unlinked file, of its named stream, a persistent open with locks; scan the store: every key but the client record lies under `F‖id‖op‖`. A share root's open writes none. Race a release against each durable-open write, both orders: never both commit. A clone source open survives grace end and closes at the clone's last batch. |
| L49 | A state reference carries its type, shard, grace-instance bits and ≥64 random bits; a miss is stale in grace or for an earlier instance, bad otherwise. | A recall misses its deadline: `ErrRevoked` until `FreeState`. An expired client's references are `ErrBadState`, as a forged one. A single-node restart under an NFSv4.0 lock: `ErrStaleState`, and the reclaim granted. **(cluster)** After a batch move, `TestState` and `FreeState` answer as before and a reconnect by the old `FileId` succeeds; fail the receiving shard over: the moved reference is stale, the forwarding entry kept, and works after the reclaim. |
| L50 | An expired or revoked client's state is released in every view; an administrator's revoke keeps its record, sessions and revoked references, each answered revoked until freed, and refuses a reclaim in a shard it released, across a restart, until the client next takes state there; an NLM host's revoke deletes its record; `Destroy` releases nothing. | Grant through one adapter, let the lease expire, open conflictingly through the other: granted without a restart. Revoke a client: its state is released for others at once, each reference `ErrRevoked` with `AdminRevoked` set and `RecallableRevoked` clear until freed. Revoke X, let Y take its lock, restart: X's reclaim is `ErrReclaimRevoked`, its next ordinary open clears it. Revoke an NLM host and restart: no notice, no record, a reclaim `ErrNoReclaim`. `Destroy` with a session or state: `ErrClientBusy`, nothing released. |
| L51 | No grant with write caching is offered on a file whose mode has the setuid or setgid bit, and a mode change that sets either bit recalls the write caching of every grant held on the file. | Open a setuid file for write as a non-owner: no write grant offered, over NFS and SMB, and for setgid. Grant a write delegation, then set setuid from another client: the delegation is recalled before the mode change returns. pjdfstest `chmod/12.t` is the e2e half (§13). |
| L52 | An SMB logoff closes only its own session's opens and releases nothing else. | Two sessions under one `ClientGuid` each hold opens; log one off: only its opens close. |
| L53 | (cluster) The opens of sessions held by a node whose node lease lapsed are timed as disconnected from the lapse. | Kill the node holding a session with SMB durable opens: each `Timeout` starts at the lapse, and a reconnect through another node finds them. |
| L54 | The stateid index, the per-client index, the per-`Via` counts and the grant-budget counts are derived from the files' entries. | Model layer: after any operation sequence, each equals a rebuild from the entries; after `Expire`, the client is in none of them. |

Authorisation, delegation I/O, handle-break order and client ownership are
RFC 17 V8, V18, V21 and V24.

## 13. Conformance

The table is tested in layers, cheapest first, as
[RFC 17 §8](rfc-17-vfs.md#8.%20Conformance) tests the service.

### 13.1 Layers

| Layer | What it covers |
| --- | --- |
| pure units | range algebra, the deny × access matrix, the owner seqid, the SMB lease state machine |
| table suite | §12's rows: `*Table`, the in-memory store, a recording `Sink`, the fake clock |
| fault injection | below the real table, not in place of it: a fault KV under the store, a failing `Sink`, the fake clock |
| model | interleavings of open, lock, recall and expiry under `-race`, against a sequential model |
| fuzz | every client-chosen input, below |
| service, cross-protocol | RFC 17 `vfstest/suite`'s `crossproto.go` (L1, L20) |
| adapter | RFC 21 and RFC 22: the wire spelling of every neutral error and status |
| e2e | RFC 29, below |

**Fuzzing.** Every client-chosen input has a `testing.F` target, seeded with
the suite's values; the stateid codecs are `pkg/vfs`'s, fuzzed by
[RFC 17 §8](rfc-17-vfs.md#8.%20Conformance):

- **range algebra**: lock and unlock sequences over two owners and ranges in
  [0, 2⁶⁴), drawn from the input, agree with a per-byte model over a 256-byte
  window plus the endpoints 2⁶⁴−1 and length 0 = to end of file; no
  offset+length overflow is accepted;
- **owner seqid**: a request stream for one NFSv4.0 owner, including wrap at
  2³² and every exempt reply, agrees with a model of §2.8;
- **SMB lock sequence**: `LockSeq` index and value from arbitrary bytes,
  replayed or not, agree with MS-SMB2 3.3.5.14's model;
- **opaque owners**: an owner of 0 to 2,048 bytes is stored when ≤ 1,024 bytes,
  refused otherwise, and never aliases another;
- **model sequences**: the model layer's operations decoded from the input, so
  a failing interleaving is saved to the corpus.

**End to end.** RFC 29 cells cover what the table suite cannot: pynfs 4.0/4.1
with its lock, delegation and reboot groups (today only `all` runs,
[RFC 29 §1](rfc-29-test-harness.md#1.%20Why), #2996); smbtorture
`smb2.lock`, `smb2.oplock`, `smb2.lease`, `smb2.durable-open`,
`smb2.durable-v2-open`, `smb2.replay`, `smb2.notify`; an NLM blocking-lock and
restart-notify driver; pjdfstest `chmod/12.t` with delegations on. A suite
whose named tests all skip is a failure
([RFC 29 §4.7](rfc-29-test-harness.md#4.7%20Grading%3A%20known%20failures%2C%20expected%20skips%2C%20quarantine%2C%20retries)).
Tiers are RFC 29's.

### 13.2 What must not stand in

- **A single-client rig MUST NOT stand in for [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe).** The failure needs two clients on one object.
- **A single-adapter rig MUST NOT stand in for [§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary) or [§7](#7.%20Conflicts%20across%20protocols).** State only one protocol can see passes every test that only speaks that protocol.
- **A test that never revokes MUST NOT stand in for [§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time).** A recall that is always answered never exercises the deadline.

### 13.3 Benchmarks

| Benchmark | Measures | Target |
| --- | --- | --- |
| `CheckIO`, file with no state | ns, allocs | ≤ 50 ns, 0 allocs |
| `CheckIO`, 1 open, 16 ranges | ns, allocs | ≤ 250 ns, 0 allocs |
| `Lock`+`Unlock`, uncontended | ns, allocs | ≤ 1 µs, ≤ 1 alloc (the stored range) |
| `Open`+`Close`, volatile, linked file | ns, allocs, records written | ≤ 2 µs, ≤ 2 allocs, 0 records once the client is listed for the shard |
| `Open`+`Close`, persistent | records written, p99 | one synchronous store write per open, grant change and close; p99 reported |
| 64 goroutines locking disjoint ranges of 64 files | ops/s vs 1 goroutine | ≥ 0.8 × cores: no global lock |
| 64 goroutines locking disjoint ranges of 1 file | ops/s | report; the per-file step's ceiling |
| expire a client holding 10⁴ opens and locks over 10⁴ files | wall time; other files' `CheckIO` p99 during it | ≤ 50 ms; p99 within 2× idle |
| 10⁵ clients, lease sweep | CPU per second; renew ns | ≤ 1% of a core; renew ≤ 200 ns, 0 allocs |
| recall with a responsive holder | conflicting open to grant, on loopback callbacks | ≤ 1 ms + the holder's reply |
| recall fan-out to 16 read holders | time to the last callback sent | ≤ 1 ms |
| grace with 10⁴ locks reclaimed | CPU time to reclaim | ≤ 1 s; ends at the last completion |
| heap per open, range, grant, waiter at 10⁵ | bytes | RFC 24 M6: ≤ 1 KiB, ≤ 256 B |

> decision: the targets are proposals, set before a baseline; the first
> after-merge run against today's `pkg/metadata/lock.Manager` through a shim
> fixes them, compared with `benchstat` (RFC 17 §8's tiers).

## 14. Observability

Metric names are shown without the deployment's prefix. `protocol` takes
RFC 25's values: `nfs3` (NLM included), `nfs4`, `smb`.

| Answers | Metric | Type |
| --- | --- | --- |
| state held | `openstate_held{kind, protocol}`, `kind` = `client`, `unconfirmed`, `open`, `lock`, `lock_state`, `waiter`, `grant`, `watch`, `delete_pending`, `revoked` | gauge |
| grants offered and declined | `openstate_grants_total{protocol, result}`, `result` = `offered`, `conflict`, `no_callback`, `budget`, `fenced`, `setuid`, `pool`, `shared` | counter |
| recalls, by outcome and cause | `openstate_recalls_total{protocol, outcome, reason}`, `outcome` = `returned`, `revoked`, `downgraded`; `reason` = `conflict`, `budget`, `setuid`, `fence` | counter |
| recall time | `openstate_recall_seconds{protocol, outcome}` | histogram (`request`) |
| conflicts refused | `openstate_conflicts_total{reason}`, `reason` = `deny_mode`, `lock`, `mandatory_io`, `delete_pending`, `open_children`, `grace` | counter |
| lock waits | `openstate_lock_wait_seconds{protocol, outcome}`, `outcome` = `granted`, `cancelled`, `poll_missed`, `ended` | histogram (`background`) |
| grants not yet acknowledged by their lock-waiter's host, and the oldest | `openstate_lock_grants_unacknowledged`, `openstate_lock_grant_unacknowledged_oldest_seconds` | gauge |
| clients fenced from grants ([§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)) | `openstate_grant_fenced_clients` | gauge |
| shards in grace | `openstate_grace_shards` | gauge |
| grace duration | `openstate_grace_seconds{reason}`, `reason` = `complete`, `timeout`, `move` | histogram (`background`) |
| reclaims | `openstate_reclaims_total{protocol, kind, result}`, `kind` = `open`, `lock`, `grant`, `persistent`; `result` = `accepted`, `no_record`, `more_than_held`, `revoked`, `pool` | counter |
| SMB reconnects | `openstate_reconnects_total{result}`, `result` = `matched`, `refused_key`, `refused_principal`, `timed_out` | counter |
| clients ended | `openstate_clients_ended_total{protocol, reason}`, `reason` = `lease`, `reboot`, `admin`, `destroy`, `share` | counter |
| wait for a file's open-state step | `openstate_step_wait_seconds` | histogram (`request`) |

The recall histogram uses the `request` set. A delegation revoked at its 90 s
deadline lands above the last bucket, 60 s, and is counted by
`outcome="revoked"`. Every series is exported from start at zero. Requests
answered retry-later are counted once, in RFC 17's `vfs_delayed_total{cause}`.
Recall, grant, grace and open-state metrics are defined here only; other RFCs
link to this table. No share or client label.

**Spans.** A table call opens no span; it runs inside RFC 17's `vfs.<Op>` span
and adds events to it: `openstate.conflict` (`reason`),
`openstate.recall_started` (holder `protocol`, number of grants),
`openstate.waiter_recorded`, `openstate.grace_refused`, and
`openstate.step_wait` with the time waited. Work that outlives a request opens
a root span that links to the request that started it: `openstate.recall`
(grant kind, `protocol`, `outcome`, deadline), `openstate.grace` per shard
(shard ID, grace instance, recorded clients, `reason`), and
`openstate.client_end` (client ID, `reason`, counts released). A renewal opens
no span; at 10⁵ clients one per lease period would be most of the export
queue. Client, share and file IDs are span attributes, never metric labels.

**Logs.** An expired or revoked client logs one `Warn` line naming the client
and `reason`, with counts of what was released by kind. A recall revoked at its
deadline logs one `Warn` per client per lease period, with the count and the
first FileID. Per-file detail is at `Debug`.

**Health.** One condition, `lock_grant_unacknowledged`: node scope, naming
the client; degraded while a lock granted to a waiting NLM host has gone
unacknowledged for 15 min (proposed), raising a ticket; read from
`openstate_lock_grant_unacknowledged_oldest_seconds`. It is how an
administrator learns of the hold
[§4.5](#4.5%20NLM%20locks%20and%20restart%20notification)'s `decision:`
accepts.

## 15. Open questions

1. **Recall deadlines.** Whether a measured workload needs an NFS recall
   deadline shorter than the lease
   ([§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).
2. **The cost of persistent opens**, unmeasured;
   [§13.3](#13.3%20Benchmarks) measures it.
3. **Byte-range lock splitting** across protocols with different range
   semantics (SMB's 64-bit ranges against NFS's end-of-file locks). Default:
   every range is held as [offset, end] over 64 bits, an NFS length of all
   ones or zero meaning to end of file; conflicts are computed on that form.
   Open: whether a lock past 2⁶³ from an NFS client and an SMB one must ever
   meet.
4. **A file's table through the management API.** Whether it reads a file's
   holders, locks and waiters for an operator asking who holds a file; it adds
   API surface.

## Appendix A — prior art

| System | Takes from it |
| --- | --- |
| The NFSv4.1 specification (client records on stable storage, grace, reclaim) | the minimum durable state for safe reclaim; delegations not reclaimed across a loss |
| Linux kernel NFS server and its client-tracking daemon | client records durable, opens and locks rebuilt in grace |
| NFS-Ganesha clustered recovery | a shared grace record; grace scoped to what lost state |
| The SMB2 specification (opens, durable and persistent handles, leases) | durability as a property of an open; lease keys per file |
| Multiprotocol NAS (a lock manager per volume on its serving node; a distributed lock manager with protocol-shared lock domains) | one table per file; SMB deny modes and locks mandatory against NFS I/O |
| Distributed file systems with client capabilities | caching grants recalled on conflict, rebuilt from clients on reconnect |

## Appendix B — where the current code differs

*Informative.* Each line names a requirement above and what the code does
today. [RFC 17 §9](rfc-17-vfs.md#9.%20Migration) slice 3 closes them, as
`pkg/metadata/lock` becomes `pkg/openstate`.

| Requirement | Today |
| --- | --- |
| no open state in an adapter ([§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary)) | NFSv4 state is held in `internal/adapter/nfs/v4/state`, and SMB leases in the SMB lease manager |
| keyed by file, never a handle ([§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary)) | `handleKey := string(fileHandle)` (`internal/adapter/smb/lease/manager.go:564`, `pkg/metadata/service.go`) |
| locks volatile unless their open is persistent ([§8](#8.%20What%20is%20durable)) | locks are persisted, and restored by `RestoreLocks` (`pkg/metadata/share_registry.go:433`) |
| one grace instance per loss ([§4.4](#4.4%20Grace%20is%20per%20shard)) | two grace machines, and `IncrementServerEpoch` |
| a waiter has no time limit of its own ([§2.10](#2.10%20Lock%20waiters)) | `BlockingTimeout` (60 s) and `MandatoryLocking` are declared in `config.go` and read nowhere else |
| a recall ends by its deadline ([§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)) | `byteRangeLockBreakWaitTimeout = 5s` (`break.go:300`); `lease_break_timeout` is an adapter setting |
| the grant fence is per client ([§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)) | `RecentlyRecalledTTL = 30s`, per file (`delegation.go:20`) |
| per-client bounds ([§5.5](#5.5%20A%20client%27s%20grants%20are%20bounded)) | per-file, per-client and total lock limits, as settings |
| no deadlock detection is specified | `deadlock.go` |
| directory delegations are not offered ([§2.5](#2.5%20Watch)) | `dir_delegation.go` |
| the table is serialised per file ([§3](#3.%20One%20table%20per%20file%2C%20at%20one%20primary)) | one `sync.RWMutex` over `map[string][]*UnifiedLock` (`manager.go:744`) |
