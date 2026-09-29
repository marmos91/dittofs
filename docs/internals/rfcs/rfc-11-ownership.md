---
rfc: 11
title: "RFC 11 — ownership"
component: ownership
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-10-journal-replication]]"
aliases:
  - RFC 11
tags:
  - rfc
---
# RFC 11 — ownership

**Status:** draft. [§14](#14.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone designing how several block services serve one share.

---

## 1. Purpose

On one node every write to a share goes through one engine and one journal, and
the order of writes is the order that journal appends them. With several block
services serving one share, something must decide which of them orders the
writes to any given bytes. This document answers:

> **Which block service may write these bytes now, how does a request reach it,
> and how does anyone else read them?**

### 1.1 What scaling is being designed for

A deployment in the target range holds petabytes across tens of thousands of
shares and hundreds of thousands of users. Three shapes of multi-node system
were considered:

| | A. active-passive | B. shares spread across nodes | C. many nodes per share |
| --- | --- | --- | --- |
| Gives | survival of node loss | aggregate throughput across shares | throughput of one share beyond one node |
| Owner of a share | one node, a standby takes over | one node per share | per unit, finer than a share |
| Limit | one node's throughput in total | one node per share | none in principle |

The ownership unit is a policy ([§2](#2.%20Ownership%20units)), and its default is **a share**, which
gives B; B with failover gives A. Subtree units give C for a share one node
cannot carry, and per-file units are enabled only where measurement shows
contention a coarser unit cannot absorb. One node is any of them with one block
service. The same mechanism serves all of them, so no deployment runs a code
path another does not.

### 1.2 Non-goals

This document **MUST NOT**:

- replicate journal content or define failover of a unit's content — [RFC 10](rfc-10-journal-replication.md);
- define client-visible locking (NFS `LOCK`, SMB byte-range locks, share modes).
  Write tokens are internal and **MUST NOT** be exposed as, or stored with, client
  locks ([§7](#7.%20Protocol%20state));
- let more than one block service write the same bytes at once
  ([RFC 10 Appendix B](rfc-10-journal-replication.md#Appendix%20B%20%E2%80%94%20alternatives%20considered)).

## 2. Ownership units

An **ownership unit** is a set of files with one owner at a time. Every file
belongs to one unit, and the unit's configuration ([RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms)) names its
owner, epoch and replica set.

| Policy | A file's unit is | When |
| --- | --- | --- |
| per share | the share's single unit | the default |
| per subtree | inherited from its parent at create; chosen directories start a new unit | a share one node cannot carry |
| per file | its own, split from its enclosing unit | only under measured contention |

**The configuration store MUST NOT hold per-file state.** It holds one
configuration per share or subtree unit. Which unit a file belongs to is
recorded with its inode ([RFC 7 §2.1](rfc-7-namespace-metadata.md#2.1%20Inode)), and a per-file unit's owner and epoch are
recorded there too, with the enclosing unit's members. The configuration store
therefore grows with shares and subtrees, never with files.

Every commit that must be fenced checks the unit's epoch the same way whatever
the policy, so a policy **MAY** change later by moving files between units —
which moves ownership, not data.

**A unit belongs to the inode, not the path.** A file's unit is fixed when the
file is created and recorded with it. Rename **MUST NOT** change it, and hard
links do not split it. A subtree policy that wants a renamed file to follow its
new parent does so by an explicit move ([§4](#4.%20Moving%20ownership)), never as a side effect of rename,
because a unit change silently changes which journal holds the file's
un-offloaded content.

## 3. Write tokens

### 3.1 A token is a lease, fenced by an epoch

The right to write a unit is a **write token**: a lease recorded with the unit's
configuration ([RFC 10 §2.2](rfc-10-journal-replication.md#2.2%20Roles)). It is the owner lease of [RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms) — one lease,
not two. Acquiring or moving one is a compare-and-swap that raises the epoch.

**An epoch never decreases for a file.** A unit's new epoch **MUST** exceed that of
every unit that previously held any of its files. Every change that moves files
between units — a new subtree unit, a per-file unit split off or merged back, a
move — therefore sets every unit it touches to one more than the highest of their
epochs, in the same transaction. A file created in a unit starts at the unit's
epoch.

A token alone protects nothing. A holder can pause past its lease and then act;
a check it makes before sending is already stale. What makes the token safe is
that every receiver refuses a stale epoch — replicas ([RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing)) and the
metadata store ([§8](#8.%20Metadata%20consistency)). The token decides who **should** write; the epoch decides
whose writes **count**.

### 3.2 A token is held until another writer needs it

- The first writer to a unit with no live owner acquires its token, with one
  compare-and-swap.
- Nothing is exchanged per write, or per file, while the holder is the only
  writer; it renews the lease.
- Another block service that wants to write forwards to the holder ([§5.1](#5.1%20Front-ends%20forward%20to%20the%20owner)), or,
  under [§3.3](#3.3%20Ownership%20follows%20the%20writer), asks for the unit to move.

A single writer therefore pays one configuration-store transaction per unit it
acquires, not per file or per write.

### 3.3 Ownership follows the writer

A write arriving at a block service that does not own the unit is either
forwarded to the owner ([§5](#5.%20Routing)) or moves the unit to where it arrived. A unit
**SHOULD** move when its owner has not written it recently and the requester is
now its only writer, and **SHOULD NOT** move back and forth between two writers
of one unit; forwarding is the answer to concurrent writers. Moving a whole share
ships all its un-offloaded content, so the bar for a move is high. The exact
policy is open ([§14](#14.%20Open%20questions)).

## 4. Moving ownership

A move is a handover ([RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover)): the new owner catches up through `Export`
([RFC 10 §2.5](rfc-10-journal-replication.md#2.5%20The%20journal%20extension)), which yields each operation with its bytes, and applies them
with `Apply`, on the un-offloaded operations of the unit's files the old
owner's journal holds, the old owner stops and drains, the configuration moves at
the next epoch, and the new owner settles those files to the drained point before
it serves. Shipping the dirty operations, rather than offloading them first,
keeps a move a local-network transfer instead of a remote-tier round trip.
Moving files from one unit to another is the same handover, with one
compare-and-swap over both units ([§3.1](#3.1%20A%20token%20is%20a%20lease%2C%20fenced%20by%20an%20epoch)).

> [!important] Pending review — handover catches up through Export
> `Since` yields only versions and extents, not bytes; a new owner needs the
> operations with their bytes, which RFC 10's `Export` gives and `Apply` replays.

A lost owner is not moved but failed over ([RFC 10 §9](rfc-10-journal-replication.md#9.%20Failover)), once its lease lapses.

## 5. Routing

### 5.1 Front-ends forward to the owner

Protocol front-ends hold client sessions ([RFC 10 §2.2](rfc-10-journal-replication.md#2.2%20Roles)). File protocols carry
operations on many files over one connection and cannot redirect a client per
file, so the front-end routes each operation:

- it looks up the unit of what the operation touches, and the unit's owner in a
  cache of configurations read from the configuration store;
- it forwards the operation to that block service through the engine's facade
  ([RFC 8 §9](rfc-8-engine.md#9.%20The%20facade)), which **MUST** therefore be callable across a network — streamed
  bodies, no callbacks across it, and every operation safe to retry;
- a block service that is no longer the owner refuses the operation by epoch; the
  front-end re-reads the configuration and retries.

The routing cache **MAY** be stale. A stale entry costs a refusal and a retry,
never a wrong write, because the epoch check is at the receiver.

On one node the front-end, the owner and the facade are the same process, and
routing is a function call.

### 5.2 Clients that can route themselves

A protocol that lets the server direct a client's I/O per file — parallel NFS
layouts — **MAY** point the client at the owner directly, so its reads and
writes skip the front-end hop. A layout is then another holder of the routing
decision, and **MUST** be recalled when the unit moves. Clients without such a
protocol use the front-end path; nothing in this document depends on layouts.

## 6. Reads on non-owners

| Served by | When |
| --- | --- |
| the owner, or a member of the unit's replica set | always, under [RFC 10 §8](rfc-10-journal-replication.md#8.%20Reads%20from%20replicas) |
| any other block service | only for a range it knows holds no un-offloaded content at the owner newer than what it serves |

A block service that is not a member **MAY** serve a range — from its own
journal, or by filling it from metadata and the remote tier ([RFC 0 §6.2](rfc-0-data-lifecycle.md#6.2%20Fill)) —
only if it knows the owner holds no un-offloaded content there newer than the
version it would serve. **Otherwise it MUST forward the read** to the owner or a
member. Metadata alone cannot tell it: the owner acknowledges a write before
offloading it, so the current ref ([RFC 6 §2.1](rfc-6-block-metadata.md#2.1%20Ref)) can be older than an
acknowledged write. It learns it from the owner, by asking the newest version of
the range and serving only bytes that carry it. A cached copy older than that
version is dropped ([RFC 1 §9.2](rfc-1-journal.md#9.2%20Offload%20state%20after%20recovery)) and filled again.

The check costs one round trip to the owner per read, but no bytes when the copy
is current. Read tokens would remove the round trip ([§14](#14.%20Open%20questions)).

## 7. Protocol state

Client-visible state is the protocol RFCs', but these rules follow from
ownership and bind them:

1. **Open state that keeps an inode alive** ([RFC 7 §4.2](rfc-7-namespace-metadata.md#4.2%20Open%20state%20is%20the%20second%20holder)) **MUST** be a durable
   record in the metadata store naming its holder and carrying a lease. Opens
   reach the unit's owner like any operation ([§5.1](#5.1%20Front-ends%20forward%20to%20the%20owner)), so the owner **MAY** write
   the record lazily, in the transaction of the unlink that drops `nlink` to zero
   while the file is open ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)). Held only in one process past that
   point, another block service can release an unlinked inode a client still has
   open, drop its refs, and let sweep delete the content (I3).
2. **A grace period extends open-state leases.** While a protocol's grace period
   runs, the leases of the open state it lets clients reclaim **MUST NOT** expire,
   so an inode a client is about to reclaim is not released under it.
3. **A new owner releases nothing before grace ends.** Opens held lazily by the
   previous owner are gone with it, so after a takeover or a move, until the
   grace period ends, the new owner **MUST** treat every file of the unit as
   possibly open: an unlink that drops `nlink` to zero writes the pending-release
   record, and the release waits for grace and honours the opens clients
   reclaimed.
4. **The NFS write verifier MUST change whenever a unit's owner changes**, or
   clients never resend writes they sent unstable to the old owner.
5. **Client locks are not write tokens.** They have their own semantics, grace
   period and recovery, and **MUST NOT** share records with tokens, though they
   **MAY** be served by the same metadata service.

## 8. Metadata consistency

Every metadata commit that acts for a unit carries the unit's owner epoch and is
refused, for that file, when the epoch is not current: existence
([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal)), offload ([RFC 6 §4.1](rfc-6-block-metadata.md#4.1%20What%20one%20commit%20records)), truncate, deallocate, release, clone, and
the pruning of removal records. A group commit across files checks each file's
unit epoch. The check **MUST** conflict with any concurrent change of the epoch
whatever the store's isolation level: a read the store tracks for conflicts, or
an explicit lock on the key. A plain read under snapshot isolation is not a
fence, and neither is a scan over a key range: a read whose result gates a
commit **MUST** conflict with every concurrent write that would change it.

**The epoch is held in two fence records per file**, one per commit path, as
[RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit) specifies them:

| Record | Read by | Written by |
| --- | --- | --- |
| `F_x(file)` | existence commits, with conflict tracking | a new owner; removals and releases |
| `F_o(file)` | offload commits and removal pruning | a new owner; removals and releases |

- A new owner **MUST** write both, at its epoch, before its first operation on
  the file under that epoch. An epoch change therefore conflicts with every
  commit on either path.
- A removal or release reads and writes both, so it conflicts with an offload
  commit and with an existence commit of the same file in both directions,
  without a range lock the store does not offer.
- A check that forces every commit of a unit through one record **MUST NOT** be
  used: it serialises every file of the unit on one key.
- A fenced commit covers only operations durable on the unit's replica set
  ([RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing)).
- A **put intent** ([RFC 6 §7.6](rfc-6-block-metadata.md#7.6%20Put%20intents)) carries the owner epoch it was
  written under. An intent whose epoch is superseded names an attempt that can no
  longer commit, and GC **MAY** remove it ([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)).

> [!important] Pending review — per-file fence records
> The two metadata backends differ: one tracks point reads but not range scans,
> the other validates no reads at all. Two point records per file make the epoch
> check a write-write or tracked-read conflict on both, without one hot record
> per unit. Intents carry the epoch so abandoned ones are recognisable.

- **Release is fenced like a write.** It deletes the file's shape and records a
  removal of the whole file ([RFC 6](rfc-6-block-metadata.md)); a superseded owner's release would destroy
  a file its successor is writing.
- **Removal records are pruned only by the file's owner**, under its epoch, once
  they fall below the file's durable floor ([RFC 6](rfc-6-block-metadata.md)). Only the owner knows which
  of its offers are still in flight, and a removal pruned under one lets that
  offer's commit bring removed content back.
- The owner epoch is separate from the removal versions a commit also checks
  ([RFC 6](rfc-6-block-metadata.md)).
- A file's `size` ([RFC 6 §2.4](rfc-6-block-metadata.md#2.4%20Shape%20and%20holes)) is written only by the owner of its unit.

*Backend notes (non-normative).* On the backend that tracks point reads, a
conflict-tracked read is a get inside an update transaction with conflict
detection on; a blind write detects nothing, so a guarded write also gets the
key. On the snapshot-isolation backend it is an explicit key lock (optimistic,
or pessimistic for contended keys), taken on several files in file-identity
order. Transaction size limits set the batch size of removals
([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)), and no transaction spans an upload, which would hold back the
store's garbage-collection safe point.

## 9. Failure

| Failure | Outcome |
| --- | --- |
| a block service is lost | its units fail over to members of their replica sets ([RFC 10 §9](rfc-10-journal-replication.md#9.%20Failover)) once its leases lapse; front-ends re-route on the first refusal |
| a block service pauses past its lease | on resuming, everything it sends is refused by epoch ([RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing)) |
| a front-end is lost | its clients reconnect to another front-end and reclaim their state through the protocol's grace period ([§7](#7.%20Protocol%20state)); no acknowledged write is lost, because the owner's replica set holds it |
| the configuration store is unreachable from a block service | the block service can neither renew its leases nor commit; it stops acknowledging when its leases run out, and its units fail over wherever the store is reachable |
| a partition separates block services but not the store | nothing changes for ownership, which is decided in the store; replication across the partition fails and [RFC 10 §7](rfc-10-journal-replication.md#7.%20Membership) removes the unreachable members |

## 10. API surface

Signatures are indicative; the obligations above are normative.

```go
// Ownership is what block services and front-ends ask of the configuration store.
type Ownership interface {
	// Unit returns the unit a file belongs to, recorded with its inode (§2).
	Unit(ctx context.Context, file FileID) (UnitID, error)
	// Config returns a unit's configuration (RFC 10 §2.1).
	Config(ctx context.Context, u UnitID) (Configuration, error)
	// Acquire grants the caller the unit's write token (§3.2).
	// ErrHeld names the holder to forward to.
	Acquire(ctx context.Context, u UnitID) (Token, error)
	// Renew extends the token's lease, which is the owner lease.
	Renew(ctx context.Context, t Token) (time.Time, error)
	// Move hands files from one unit to another: one compare-and-swap over both
	// units, each set to one more than the higher of their epochs (§3.1).
	Move(ctx context.Context, from, to UnitID, files iter.Seq[FileID]) error
}

type Token struct {
	Unit    UnitID
	Epoch   Epoch
	Expires time.Time
}

// Router is a front-end's routing cache (§5.1).
type Router interface {
	Route(ctx context.Context, file FileID) (NodeID, Epoch, error)
	Invalidate(u UnitID) // after a refusal by epoch
}

// OpenState is the leased open record of §7.
type OpenState interface {
	Open(ctx context.Context, file FileID, holder HolderID) (Lease, error)
	Renew(ctx context.Context, l Lease) error
	Close(ctx context.Context, l Lease) error
	// Grace extends every lease reclaimable during a protocol grace period.
	Grace(ctx context.Context, until time.Time) error
}

var ErrHeld = errors.New("ownership: token held elsewhere")
```

## 11. Invariants

| # | Invariant |
| --- | --- |
| O1 | At most one block service's writes to any byte take effect under any epoch, because every receiver refuses a stale one. |
| O2 | A file's owner epoch never decreases: every configuration change raises it, and every move of files between units raises every unit it touches above all of their previous epochs, in one transaction. |
| O3 | The write token is the owner lease: one lease per unit. |
| O4 | A file's unit is fixed at create and changes only by an explicit move. |
| O5 | A block service that is not a member serves a range only when it knows the owner holds no newer un-offloaded content there; otherwise it forwards. |
| O6 | A stale route costs a refusal and a retry, never a wrong write; every facade operation is safe to retry. |
| O7 | Open state that keeps an inode alive is a durable leased record by the time an unlink would release the inode; a grace period extends its lease; after an owner change nothing is released before grace ends. |
| O8 | The write verifier changes whenever a unit's owner changes. |
| O9 | Write tokens and client locks share no records. |
| O10 | Every fenced commit — existence, offload, truncate, deallocate, release, clone and removal pruning — conflicts with a concurrent epoch change under the store's isolation level, through the file's fence records and never through one record per unit. |
| O11 | The configuration store holds no per-file state. |
| O12 | Removal records are pruned only by the file's owner, under its epoch. |

## 12. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| token acquisitions, labelled `result` = `granted` or `held` | `dittofs_ownership_acquisitions_total` | counter |
| moves, labelled `reason` = `follow_writer`, `policy` or `failover` | `dittofs_ownership_moves_total` | counter |
| handover time, drain to new owner writable | `dittofs_ownership_handover_seconds` | histogram |
| forwarded operations, labelled `result` = `ok` or `stale_route` | `dittofs_ownership_forwarded_total` | counter |
| non-member reads, labelled `result` = `served`, `refilled` or `forwarded` | `dittofs_ownership_nonmember_reads_total` | counter |
| units owned by this block service, labelled by policy | `dittofs_ownership_units` | gauge |
| configurations in the configuration store; it follows shares and subtrees, not files | `dittofs_ownership_configurations` | gauge |
| moves of one unit back to an owner it left within the last minute; a steady rate is ping-pong | `dittofs_ownership_bounces_total` | counter |
| open-state leases held, and those extended by grace | `dittofs_ownership_open_leases` | gauge |

Logs: a move at `Info` with unit, old and new owner, epoch and reason. A block
service that stops on its own lease expiry logs at `Error`.

## 13. Test plan and benchmarks

Tokens, moves and routing are modelled with [RFC 10](rfc-10-journal-replication.md)'s configuration protocol in
one model ([RFC 10 §14](rfc-10-journal-replication.md#14.%20Test%20plan%20and%20benchmarks)), and exercised by the same deterministic simulation.
Properties:

| Property | Violated by |
| --- | --- |
| At most one block service's writes to any byte take effect under any epoch (O1) | a sender-side token check standing in for receiver fencing |
| A file's epoch never falls across a move between units (O2) | a new unit numbered from its own history, or two compare-and-swaps where one is needed |
| A write forwarded on a stale route is refused, then applied once at the owner (O6) | a front-end trusting its cache, or a non-idempotent retry |
| A non-member never serves content older than an acknowledged write (O5) | a check against the ref alone, a skipped check on fill |
| An open file's content survives its unlink on another block service, through a grace period, and through an owner change (O7) | open state held in one process, a lease that lapses during grace, a new owner releasing on unlink before grace ends |
| A release, a truncate or a removal pruning by a superseded owner is refused (O10, O12) | a release checked against existence only, pruning by whoever holds the record |
| An epoch change concurrent with an existence commit, an offload commit and a removal of one file is detected on each backend's isolation level (O10) | an epoch check by range scan, a blind write to a fence record, a unit-wide fence record |
| The configuration store's record count follows units, not files (O11) | a per-file token or a file-to-unit map kept as configuration |
| A unit's writes resume after a move or a failover without operator action | a move that waits on a holder that is gone |

**Benchmarks**, on three block services on one local network:

| Benchmark | Measures | Target |
| --- | --- | --- |
| Token acquisitions | configuration-store transactions per file written, one writer, unit already owned | 0 |
| Handover time | a unit with 64 MiB un-offloaded | ≤ 2 s at 10 Gb/s |
| Forwarding overhead | p50 latency a front-end hop adds | ≤ one network round trip + 100 µs |
| Stale route | operations after a move | each refused at most once, then applied once |
| Non-member read | p50 latency of a current cached read against the owner's | report |
| Two alternating writers | moves per minute of one unit | report, against the move policy ([§14](#14.%20Open%20questions) item 1) |

## 14. Open questions

1. **When to move a unit.** The move policy that avoids ping-pong between two
   writers is unspecified.
2. **When contention justifies a per-file unit**, measured how, and when it is
   merged back.
3. **Read tokens** ([§6](#6.%20Reads%20on%20non-owners)): a shared token per range, revoked before a write is
   accepted over it, if the per-read round trip shows up in measurement.
4. **Per-range units and token splitting.** A byte range of a file as a unit,
   split on demand when another writer asks for an overlap. It needs several
   owners of one file to write `size` and existence, and truncate and delete to
   gather every range first. Not specified, and not enabled.
5. **Where client lock and delegation state lives** — in the metadata service, or
   with the owner — belongs to the protocol RFCs; [§7](#7.%20Protocol%20state)'s rules bind it.
6. **Whether the metadata service itself is split** into shards with no
   transaction spanning them, and what that does to existence, release and the
   two-unit compare-and-swap of [§3.1](#3.1%20A%20token%20is%20a%20lease%2C%20fenced%20by%20an%20epoch).

---

## Appendix A — prior art

| System | Takes from it |
| --- | --- |
| Frangipani (SOSP 1997), GPFS (FAST 2002) | tokens held by the node using them and revoked on demand; byte-range tokens that split; a peer recovers a failed node's state only after fencing it |
| xFS (SOSP 1995) | a small reassignable map from file to managing node |
| Distributed metadata servers with subtree ownership, and client capabilities | ownership per subtree, with pinning favoured over automatic balancing in practice; revocable client grants for caching and buffering |
| Parallel NFS | the client routed to the data's owner by a layout the server recalls |
| Shared-everything designs over a fabric | the contrast: stateless front-ends reaching shared persistent media need no owner routing, at the price of storage every node can address |
| Fencing tokens (Kleppmann, 2016) | a lock is safe only if the resource rejects the stale holder |

## Appendix B — alternatives considered

Alternatives to one writer per unit — several writers ordered by a timestamp, or
reads that see a write only after offload — are in [RFC 10 Appendix B](rfc-10-journal-replication.md#Appendix%20B%20%E2%80%94%20alternatives%20considered).

| Alternative | Why not |
| --- | --- |
| A separate lock service beside the metadata store | a second source of truth for who may write; tokens in the store are fenced by the same transactions that use them |
| Tokens held in memory by a token manager | faster, but needs its own recovery; with units as coarse as a share, acquisitions are too rare for the store's latency to matter |
| Per-file units by default | a unit per file and a token transaction per file per writing session, at billions of files; kept as an option under measured contention ([§2](#2.%20Ownership%20units)) |
