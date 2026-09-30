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

## 1. Purpose

A file protocol does not only name files and move bytes. Clients open files,
deny others access while they hold them, lock byte ranges, cache under promises
the server made, and ask to be told when a directory changes. This document owns
that state and answers:

> **Who holds this file open, what may they do with it, what have we promised
> them, and what happens to all of it when a client, a server or an owner goes
> away?**

It is one component because every one of these kinds of state is consulted by
the others: an open is refused by a deny mode, a lock needs an open, a grant is
broken by an open, and a client's lease ending releases them all.

### 1.1 Non-goals

This component **MUST NOT**:

- decide names, permissions or when a file stops existing — [RFC 7](rfc-7-namespace-metadata.md)'s. It
  tells RFC 7 that a file is open ([§9](#9.%20Open%20state%20and%20the%20life%20of%20a%20file)); RFC 7 decides the rest;
- hold content or know where it is. A lock is a statement about who may act on
  a file, not about where its bytes are ([§9.3](#9.3%20Locks%20do%20not%20pin%20bytes));
- define write tokens, ownership leases or owner epochs — [RFC 11](rfc-11-ownership.md)'s. Client
  state and ownership share no records ([RFC 11 §7](rfc-11-ownership.md#7.%20Protocol%20state));
- encode a wire protocol: stateids, SMB FileIds, lease keys and replay caches
  are the adapters' ([RFC 17](rfc-17-vfs.md)).

## 2. The entities

Five entities, in the metadata package ([RFC 16](rfc-16-metadata-store.md)). Signatures are
indicative; the rules around them are normative.

### 2.1 Client

```go
// Client is one protocol client instance and its lease. When the lease
// expires, every Open, Lock, CachingGrant and Watch it holds is released in
// one step (§4.3).
type Client struct {
	ID       ClientID
	Protocol protocol.Kind
	Expires  time.Time
	Units    []OwnershipUnitID // units it has held state in; bounded (§8)
}
```

`ClientID` names one client instance as its protocol defines it: NFSv4's client
ID from `EXCHANGE_ID` or `SETCLIENTID` (a machine plus a boot verifier), SMB's
`ClientGuid`, the NLM host name for NFSv3 locks. A client that reboots is a new
`ClientID`, which is how its stale state is recognised and released.

`Units` lists every ownership unit the client has held state in. A reclaim in a
unit the list does not name is refused ([§4.4](#4.4%20Grace%20is%20per%20ownership%20unit)): a client that held state
only elsewhere cannot take state in this unit first.

`protocol.Kind` comes from one small package that the adapters and the metadata
layer both import. It **MUST NOT** come from an adapter package: the metadata
layer would then import an adapter.

### 2.2 Open

```go
// Open is one open of one file. An open of an unlinked file keeps it alive (§9).
type Open struct {
	ID         OpenID
	Client     ClientID
	File       FileID
	Access     Access     // granted
	Deny       Access     // deny mode: SMB share access, NFSv4 deny
	Durability Durability // none, durable, persistent (§8)
}
```

The name is both protocols' own: NFS's `OPEN` and its open stateid, the
"Open" of the SMB2 specification with its FileId. "Handle" is not used: in this
set it means the file handle ([RFC 7 §6](rfc-7-namespace-metadata.md#6.%20Handles)). SMB's durable and persistent handles
are properties of an open, so they are a field here, not an entity. NFSv3 has no
opens; its I/O runs against an anonymous open that holds no deny mode and keeps
nothing alive.

### 2.3 Lock

```go
// Lock is one byte-range lock, held against the file (never a handle or a name).
type Lock struct {
	Open      OpenID
	Range     ByteRange
	Exclusive bool
}
```

### 2.4 CachingGrant

```go
// CachingGrant is what NFS calls a delegation and SMB an oplock or a lease: a
// promise that no other client is using the file, recalled on conflict within
// a bounded time (§5).
type CachingGrant struct {
	ID     GrantID
	Client ClientID
	File   FileID
	Kind   GrantKind // read, write, handle
	Recall time.Time // zero unless a recall is in flight
}
```

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
with the client's lease, recalled on conflict. It also records the epoch of
every unit it names, and is recalled when any of them changes owner
([§10](#10.%20Ownership)).

## 3. One table per file, at one owner

| State | Granted by | Conflicts with | Lifetime |
| --- | --- | --- | --- |
| **open** | an open | a deny mode ([§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open)) | until close |
| **deny mode** | an open | an open asking for denied access | until close |
| **byte-range lock** | a lock request | an overlapping lock on the same file; I/O across protocols ([§7](#7.%20Conflicts%20across%20protocols)) | until unlock, close, or lease expiry |
| **caching grant** | the server, on an open | any conflicting access by another client ([§5](#5.%20Caching%20grants)) | until recalled, revoked, returned or expired |
| **watch** | a watch request | nothing | until cancelled or lease expiry |
| **layout** | a layout request, under an open | a conflicting open, lock or deny mode; an owner change of a unit it names | until returned, recalled, revoked or lease expiry |

All of it is held against a **file**, never a name or a handle. A rename does
not disturb it, and two hard links to one file are one lockable object. Keying
by handle is the subtler error: one client may hold several handles to one file,
and a lock one handle can see and another cannot is a lock the same client can
take twice.

All of it is held **in one table per file, at the owner of the file's unit**
([RFC 15 §3](rfc-15-topology.md#3.%20One%20owner%20per%20unit)) — the base unit, for a file striped into range units. That
owner also runs the file's I/O, so a conflict check and the I/O it admits run
in one process under one epoch, and no grant lands between them. An implementation **MUST NOT** hold any of it in an adapter,
where the other adapter cannot see it: a lock one protocol grants and the other
does not observe is not a lock. Every adapter reaches the table through the
filesystem service ([RFC 17](rfc-17-vfs.md)), which routes to the owner.

## 4. Client leases, grace and reclaim

### 4.1 A client lease

A **client lease** is how long the server keeps a client's state while the
client is silent. Every request from the client renews it (NFSv4 `SEQUENCE` or
`RENEW`); a client with nothing to do sends an empty renewal. A client silent
for a whole lease period is treated as gone. Without that, a laptop that sleeps
holding a lock blocks every other client forever. SMB's durable-handle timeout
plays the same part for SMB clients.

"Lease" names two unrelated things in this set: the client lease, here, and the
**ownership lease** of [RFC 11 §3.1](rfc-11-ownership.md#3.1%20A%20token%20is%20a%20lease%2C%20fenced%20by%20an%20epoch), which decides which node may serve a unit. SMB
also calls a caching grant a "lease" ([§5](#5.%20Caching%20grants)). The three share no records.

### 4.2 Grace makes volatile state safe

Open state other than what [§8](#8.%20What%20is%20durable) makes durable **MAY** be held in memory and lost
on restart or owner change. It **MUST NOT** be the reason an operation blocks on
a holder that no longer exists.

An owner that has lost open state **MUST** then run a **grace period** of at
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
**SHOULD** end early, once every client whose record names the unit has
finished reclaiming — sent `RECLAIM_COMPLETE`, or for SMB reconnected its durable
opens — or has expired: nothing is left that a new request could take first.

### 4.3 An expired lease releases everything it held, everywhere

When a client's lease expires or its state is revoked, every open, deny mode,
lock, grant and watch it held **MUST** be released, in every view that records
it, in the same step. State one protocol dropped and another still counts
refuses conflicting requests against a holder that no longer exists, until a
restart.

### 4.4 Grace is per ownership unit

Every open-state operation reaches the owner of the file's unit
([§3](#3.%20One%20table%20per%20file%2C%20at%20one%20owner)), so a grace period is scoped to the units whose owner lost state, and
the rest of the cluster keeps granting. Only a failover loses state: a planned
move hands the table over and runs no grace ([§10](#10.%20Ownership)).

**A client must be told to reclaim.** A client whose session survives a failover
— its `protocol` node did not change — sees no server restart and would never
reclaim. When a unit fails over, the owner **MUST** therefore signal every client
whose record names the unit:

- **NFSv4.1:** set `SEQ4_STATUS_RECALLABLE_STATE_REVOKED` or
  `SEQ4_STATUS_ADMIN_STATE_REVOKED` on the client's next `SEQUENCE` reply, or
  force the loss of its session so it reclaims everything; grace then covers
  every unit the client's record names.
- **NFSv4.0:** return `NFS4ERR_STALE_STATEID` for its stateids in the unit, which
  makes it recover them.
- **SMB:** break the connection, so the client reconnects and reclaims its
  durable opens; volatile opens are lost, as after any server failure.
- **NLM:** notify it of a restart, so it reclaims its locks.

A reclaim is accepted only in a unit the client's record names ([§2.1](#2.1%20Client)).

**A former owner fences itself.** Grants are replies to clients, which no
receiver can refuse by epoch, so an owner **MUST** stop serving open state at its
lease expiry less the drift bound, and a new owner starts grace only after the
old lease has lapsed plus the drift bound ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20A%20token%20is%20a%20lease%2C%20fenced%20by%20an%20epoch)).

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

An NFS read delegation is a read grant; an NFS write delegation is read, write
and handle together. SMB leases combine the three freely.

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

One table ([§3](#3.%20One%20table%20per%20file%2C%20at%20one%20owner)) makes every conflict decidable; these rules decide it.

| Held | Requested | Rule |
| --- | --- | --- |
| an SMB deny mode | an NFS open, read or write the deny mode forbids | refused |
| an NFSv4 deny mode | an SMB open it forbids | refused |
| an SMB byte-range lock | an NFS `READ` or `WRITE` across the range it forbids | refused: SMB locks are mandatory |
| an NFS byte-range lock | an SMB byte-range lock that overlaps it | refused |
| an NFS byte-range lock | SMB or NFS I/O across its range | allowed: NFS locks are advisory and gate lock requests only |
| a caching grant of either protocol | a conflicting open, write, set-attribute, rename or unlink from another client, through either protocol | the grant is recalled within its deadline ([§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)) before the request proceeds |
| a write grant | a read from another client | recalled to read |

A caching grant is recalled by the server in the holder's own protocol, whatever
protocol caused the conflict ([RFC 17](rfc-17-vfs.md)'s callbacks).

A layout is recalled when a mandatory lock or deny mode is granted over its
range, so pNFS I/O, which bypasses the owner, never crosses one.

## 8. What is durable

Each entity has one rule. Durable state is written to the metadata store under
the keys [RFC 16](rfc-16-metadata-store.md) lists; volatile state never is.

| Entity | Rule | Why |
| --- | --- | --- |
| **Client** | **durable**, holding only what reclaim needs: the client's identity, the units it has held state in, and whether its state was revoked or its reclaim is incomplete | without it every reclaim after a loss must be refused ([§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe)); one record per client, not per open |
| **Open** | **volatile**, reclaimed in grace — except **durable** when it keeps an unlinked file alive ([§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive)) or `Durability` is persistent | a durable record per open costs a write per open; only these two cases lose data or a promise without one |
| **Lock** | **volatile**, reclaimed in grace — **durable** only when its open is persistent | reclaim restores it |
| **CachingGrant** | **volatile**, never reclaimed; an SMB lease survives only inside a persistent open | a lost grant costs a client its cache, never correctness |
| **Watch** | **volatile**, never reclaimed | the client re-registers; the NFSv4.1 specification does not allow reclaiming directory notifications |
| **Layout** | **volatile**, reclaimed in grace; a planned move hands it over only if every unit it names kept its owner | a lost layout costs a `LAYOUTGET`; a stale one is refused by epoch |

Client records are held globally, not per unit, because one client's state spans
many units; the unit list in each is what scopes its reclaims. The list is
written when the client first takes state in a unit, not per open, and is
bounded: a client past the bound is recorded as holding state in every unit of
the share, which widens its grace and never loses a reclaim.

## 9. Open state and the life of a file

### 9.1 An open keeps a file alive

An open is the second holder of a file ([RFC 7 §4.2](rfc-7-namespace-metadata.md#4.2%20Open%20state%20is%20the%20second%20holder)). An unlink that leaves an
open file with no entry **MUST**, in the transaction that removes the entry, make
the open durable; the file's pending release ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)) is written in the
same transaction and names no holders — the durable open records are the only
list of them. The last close releases the file. Held only in
one process past that point, another node could release a file a client still
has open, drop its refs, and let sweep delete its content.

Opening and closing a linked file **MUST NOT** write any record.

### 9.2 A new owner releases nothing before grace ends

Volatile opens held by a failed owner are gone with it. Until its
grace period ends, a new owner **MUST** treat every file of the unit as possibly
open: an unlink that drops `nlink` to zero writes the pending release, and the
release waits for grace and honours the opens clients reclaimed.

### 9.3 Locks do not pin bytes

Holding an open, a lock, a deny mode or a grant **MUST NOT** make an extent
ineligible for eviction ([RFC 0 §8.1](rfc-0-data-lifecycle.md#8.1%20Evict)) or reclamation ([RFC 0 §8.2](rfc-0-data-lifecycle.md#8.2%20Reclaim)). All of it is
about who may act on a file, not where its bytes are. Coupling them is how a
local tier fills up with content that is durable remotely and cannot be released
because a client left a file open.

## 10. Ownership

Open state is held by the **owner of the file's unit** ([RFC 15 §3](rfc-15-topology.md#3.%20One%20owner%20per%20unit)) and
moves with it. It is fenced by that owner's epoch: an open-state change carried
by a superseded owner is refused, and every open-state call carries the
`ClientID` it acts for, which the owner checks owns the open, lock, grant, watch
or layout named.

- **A planned move** hands the unit's volatile table to the new owner under the
  new epoch, after the old owner stops granting and before the new one serves
  ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20ownership)). No state is lost, so no grace runs and no client is told.
- **A failover** loses the volatile table, and starts grace for that unit
  ([§4.4](#4.4%20Grace%20is%20per%20ownership%20unit)).
- **A layout** is bound to the epoch of every unit it names. When any of them
  changes owner, by move or failover, the layout **MUST** be recalled, and
  revoked at the recall deadline; a `LAYOUTCOMMIT` for it then fails with
  `NFS4ERR_BADLAYOUT` ([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS)).

When a unit's owner changes, the NFS write verifier **MUST** change, or clients
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

	// Opens and deny modes (§6). Every call names its client, and the owner
	// refuses one that names another client's state (§10).
	Open(ctx context.Context, c ClientID, file FileID, want, deny Access, d Durability, reclaim bool) (Open, *CachingGrant, error)
	Close(ctx context.Context, c ClientID, o OpenID) error // the last close may release (§9.1)

	// Byte-range locks.
	Lock(ctx context.Context, c ClientID, o OpenID, r ByteRange, exclusive, reclaim bool) error
	TestLock(ctx context.Context, c ClientID, o OpenID, r ByteRange, exclusive bool) (*Lock, error)
	Unlock(ctx context.Context, c ClientID, o OpenID, r ByteRange) error

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

	// Checks run at the owner, in the process that runs the I/O (§3, §7).
	// ErrDelay while a recall is outstanding; ErrGrace in grace.
	CheckIO(ctx context.Context, o OpenRef, r ByteRange, write bool) error
	CheckChange(ctx context.Context, file FileID, by ClientID, what ChangeMask) error // recalls grants, never waits
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
)
```

## 12. Invariants

| # | Invariant |
| --- | --- |
| L1 | All open state is keyed by file, held in one table per file at the owner of the file's unit, and visible to every adapter; that owner also runs the file's I/O. |
| L2 | No operation blocks on a holder that no longer exists; after a loss of open state, every non-reclaim request and every I/O a lost lock or deny mode might forbid is refused for that unit until grace ends, and grace ends on its own — early once every recorded client has finished reclaiming. |
| L3 | A reclaim is accepted only from a client whose durable record names the unit, and every client so named is told to reclaim. |
| L4 | An expired or revoked client's state is released in every view in one step. |
| L5 | A recall ends within its deadline, by acknowledgement or revocation, and no worker waits on it. |
| L6 | A deny mode is checked once, at open, for a granted open, and per operation for an anonymous one; a conflicting open is refused, never downgraded. |
| L7 | An open that keeps an unlinked file alive is durable by the time the unlink commits; opening and closing a linked file writes nothing. |
| L8 | After a failover, nothing is released before grace ends; a planned move hands the table over and runs no grace. |
| L9 | Open state never makes an extent ineligible for eviction or reclamation. |
| L10 | Open state and ownership share no records. |
| L11 | An owner serves no open state past its lease expiry less the drift bound. |
| L12 | A layout is bound to the epoch of every unit it names and is recalled when any changes owner. |
| L13 | Every open-state call names its client, and names only that client's state. |

## 13. Conformance

Every check runs through the filesystem service, against every backend, in the
index's tiers.

### 13.1 Group A — wrong holder, lost state, stuck client

| Requirement | Check |
| --- | --- |
| [§3](#3.%20One%20table%20per%20file%2C%20at%20one%20owner) one table | Take a lock and a deny mode through one adapter, reach the same file through the other. Assert the other observes both. A single-adapter rig cannot fail this. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) grace | Grant a lock, restart, have a different client request the conflicting lock immediately. Assert refusal for the lease period. Request a lock on a file nobody held; assert it is refused too, and that a reclaim is granted. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) reclaim needs a record | Lose the client records, restart, reclaim. Assert refused. |
| [§4.3](#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere) lease expiry | Grant a grant through one adapter, let the lease expire, open the file conflictingly through the other. Assert the open is granted without a restart. |
| [§4.4](#4.4%20Grace%20is%20per%20ownership%20unit) grace per unit | Fail one unit over. Assert only that unit refuses new state, and that a client whose session survived is told to reclaim and does. |
| [§4.4](#4.4%20Grace%20is%20per%20ownership%20unit) reclaim scoped | Client C holds state only in unit U1, D holds a lock in U2; fail U2 over. Assert C's reclaim of D's lock is refused. |
| [§4.4](#4.4%20Grace%20is%20per%20ownership%20unit) self-fence | Pause an owner past its lease, let a new owner finish grace, resume the old one. Assert it grants nothing. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) I/O in grace | Hold an SMB mandatory lock, fail over, write the range through NFSv3 before the reclaim. Assert `ErrGrace`. |
| [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe) early end | Fail over with two recorded clients; both reclaim and complete. Assert grace ends before the lease period. |
| [§10](#10.%20Ownership) planned move | Hold opens, locks and a grant, move the unit. Assert no grace, and every holding intact at the new owner. |
| [§10](#10.%20Ownership) stale layout | Write through a layout, fail its data server's unit over, `LAYOUTCOMMIT`. Assert `NFS4ERR_BADLAYOUT`. |
| [§5.2](#5.2%20A%20recall%2C%20and%20why%20nothing%20is%20merged) no blocked worker | Hold grants on 10^4 files with a client that never answers; send conflicting opens from another. Assert each gets `ErrDelay` at once and unrelated operations keep their latency. |
| [§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open) anonymous I/O | Hold an SMB deny-write, write through NFSv3. Assert refused. |
| [§11](#11.%20Interface) client check | Close, unlock and return a grant naming another client's state. Assert `ErrNotYours`. |
| [§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time) recall deadline | Grant a grant to a client that never answers recalls, open conflictingly. Assert the open proceeds after the deadline and the silent client's later writes are refused. |
| [§5.2](#5.2%20A%20recall%2C%20and%20why%20nothing%20is%20merged) flush on recall | Buffer writes under a write grant, open from another client. Assert the second client reads the first's data. |
| [§6](#6.%20A%20deny%20mode%20is%20checked%20at%20open) no downgrade | Request write against a deny-write. Assert refusal, never a read-only open. |
| [§7](#7.%20Conflicts%20across%20protocols) cross-protocol | For every row of the table, assert the stated outcome with one protocol holding and the other requesting. |
| [§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive) open-unlinked | Open, unlink, crash. Assert the content survives until grace ends, and is released after it unless the open was reclaimed. |
| [§9.1](#9.1%20An%20open%20keeps%20a%20file%20alive) lazy | Open and close a linked file 10^4 times. Assert no record was written. |
| [§9.2](#9.2%20A%20new%20owner%20releases%20nothing%20before%20grace%20ends) new owner | Open a file on one owner, move the unit, unlink through the new owner. Assert no release before grace ends. |

### 13.2 What must not stand in

- **A single-client rig MUST NOT stand in for [§4.2](#4.2%20Grace%20makes%20volatile%20state%20safe).** The failure needs two clients on one object.
- **A single-adapter rig MUST NOT stand in for [§3](#3.%20One%20table%20per%20file%2C%20at%20one%20owner) or [§7](#7.%20Conflicts%20across%20protocols).** State only one protocol can see passes every test that only speaks that protocol.
- **A test that never revokes MUST NOT stand in for [§5.3](#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time).** A recall that is always answered never exercises the deadline.

### 13.3 Benchmarks

| Benchmark | Measures | Target |
| --- | --- | --- |
| Open and close a linked file | p99 latency, records written | records written 0 |
| Grace refusal and reclaim of 10^4 locks | time to leave grace | once every recorded client completes; one lease period at most |
| Planned move of a unit holding 10^4 opens | time new opens are refused | 0: no grace |
| Recall with a responsive holder | time from conflicting open to its grant | report |
| 10^5 clients renewing | renewals/s at the owner | report |

## 14. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| state held, by kind | `dittofs_openstate_held{kind=client\|open\|lock\|grant\|watch\|layout}` | gauge |
| grants offered and declined | `dittofs_openstate_grants_total{result}` | counter |
| recall time | `dittofs_openstate_recall_seconds` | histogram |
| recalls revoked at the deadline | `dittofs_openstate_recalls_revoked_total` | counter |
| conflicts refused, by rule | `dittofs_openstate_conflicts_total{rule}` | counter |
| units in grace | `dittofs_openstate_grace_units` | gauge |
| grace periods ended, by `reason` = `complete` or `timeout` | `dittofs_openstate_grace_ended_total{reason}` | counter |
| requests answered `ErrDelay` while a recall is outstanding | `dittofs_openstate_delays_total` | counter |
| layouts recalled on an owner change | `dittofs_openstate_layout_recalls_total` | counter |
| reclaims accepted and refused | `dittofs_openstate_reclaims_total{result}` | counter |
| leases expired | `dittofs_openstate_expired_total` | counter |

Recall, grant, grace and open-state metrics are defined here only; other RFCs
link to this table. No share or client label. A revoked recall and an expired lease log at `Warn`
with the client and file.

## 15. Open questions

1. **Lease period and recall deadline values**, per protocol, and whether they
   are settings ([RFC 13](rfc-13-configuration.md)).
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
| Multiprotocol NAS (a lock manager per volume owner; a distributed lock manager with protocol-shared lock domains) | one table per file; SMB deny modes and locks mandatory against NFS I/O |
| Distributed file systems with client capabilities | caching grants recalled on conflict, rebuilt from clients on reconnect |
