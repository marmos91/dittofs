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

**Status:** draft — first write-up of the 2026-09-28 design discussion, not yet
reviewed. [§12](#12.%20Open%20questions) lists what is known to be undecided.
**Depends on:** [RFC 10](rfc-10-journal-replication.md), which assumes what this document provides ([RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20ownership)), and
on [RFC 0](rfc-0-data-lifecycle.md), [RFC 1](rfc-1-journal.md), [RFC 6](rfc-6-block-metadata.md), [RFC 7](rfc-7-namespace-metadata.md) and [RFC 8](rfc-8-engine.md) for the components whose operations it routes.
**Audience:** anyone designing how several block services serve one share.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT** and **MAY** are
to be interpreted as in RFC 2119.

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

This document designs for **C, with the ownership unit as a policy** ([§2](#2.%20Ownership%20units)): a
unit of one share gives B, B with failover gives A, and one node is C with one
block service. The same mechanism serves all of them, so no deployment runs a
code path another does not.

### 1.2 Non-goals

This document **MUST NOT**:

- replicate journal content or define failover of a unit's content — [RFC 10](rfc-10-journal-replication.md);
- define client-visible locking (NFS `LOCK`, SMB byte-range locks, share modes).
  Write tokens are internal and **MUST NOT** be exposed as, or stored with, client
  locks ([§7](#7.%20Protocol%20state));
- let more than one block service write the same bytes at once ([§11](#11.%20Alternatives%20considered)).

## 2. Ownership units

An **ownership unit** is a set of bytes with one owner at a time. Every file names
the unit it belongs to, and the unit's configuration ([RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms)) names its
owner, epoch and replica set.

**Granularity is a policy, not a mechanism.** A unit **MAY** be a whole share, a
directory subtree, a file, or a byte range of a file:

| Policy | A file's unit is | Resembles |
| --- | --- | --- |
| per share | the share's single unit | B; today's single node |
| per subtree | inherited from its parent at create; chosen directories start a new unit | subtree pinning in distributed metadata servers |
| per file | its own | per-file ownership |
| per range | a range of the file, split on demand ([§3.2](#3.2%20Tokens%20start%20wide%20and%20split)) | byte-range tokens in shared-disk filesystems |

Every commit that must be fenced checks the unit's epoch the same way whatever
the granularity, so a policy **MAY** change later by reassigning files to units —
which moves ownership, not data.

**A unit belongs to the inode, not the path.** A file's unit is fixed when the
file is created and recorded with it. Rename **MUST NOT** change it, and hard
links do not split it. A subtree policy that wants a renamed file to follow its
new parent does so by an explicit move ([§4](#4.%20Moving%20ownership)), never as a side effect of rename,
because a unit change silently changes which journal holds the file's
un-offloaded content.

## 3. Write tokens

### 3.1 A token is a lease, fenced by an epoch

The right to write a unit is a **write token**: a lease recorded in the
configuration store ([RFC 10 §2.2](rfc-10-journal-replication.md#2.2%20Roles)) with the unit's configuration. Acquiring or
moving one is a compare-and-swap that raises the epoch.

A token alone protects nothing. A holder can pause past its lease and then act;
a check it makes before sending is already stale. What makes the token safe is
that every receiver refuses a stale epoch — replicas ([RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing)) and the
metadata store, whose existence and offload commits carry the owner epoch and
are refused when it is not current. The token decides who **should** write; the
epoch decides whose writes **count**.

### 3.2 Tokens start wide and split

A token is held until someone else needs it, not released after each write:

- the first writer of an unowned file is granted its whole unit — for range
  units, `[0, ∞)`;
- nothing is exchanged per write while the holder is the only writer;
- when another block service asks for a range that overlaps a held token, the
  holder is asked to give up only the overlap; the token splits, and each side
  keeps its part.

A single writer therefore pays one configuration-store transaction per file per
writing session, not per write.

### 3.3 Ownership follows the writer

A write arriving at a block service that does not own the unit is either
forwarded to the owner ([§5](#5.%20Routing)) or moves the unit to where it arrived. A unit
**SHOULD** move when its owner has not written it recently and the requester is
now its only writer, and **SHOULD NOT** move back and forth between two writers
of one unit; forwarding is the answer to concurrent writers. The exact policy is
open ([§12](#12.%20Open%20questions)).

## 4. Moving ownership

A move is a handover ([RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover)): the new owner catches up on the unit's
un-offloaded operations through `Export` ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Operations%20versioned%20elsewhere)), the old owner stops and
drains, and the configuration moves at the next epoch. Shipping the dirty
operations — rather than offloading them first — keeps a move a local-network
transfer instead of a remote-tier round-trip.

A lost owner is not moved but failed over ([RFC 10 §9](rfc-10-journal-replication.md#9.%20Failover)), which is when its lease
lapses.

**Truncate and delete** act on the whole file. With per-range units, a truncate
or delete **MUST** first revoke or move every range token of the file to one
owner, then act ([§12](#12.%20Open%20questions)).

## 5. Routing

### 5.1 Front-ends forward to the owner

Protocol front-ends hold client sessions ([RFC 10 §2.2](rfc-10-journal-replication.md#2.2%20Roles)). NFS and SMB carry
operations on many files over one connection and cannot redirect a client per
file, so the front-end routes each operation:

- it looks up the owner of what the operation touches, in a cache of unit
  configurations read from the configuration store;
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

A protocol that lets the server direct a client's I/O per file — NFSv4.1 parallel
NFS layouts — **MAY** point the client at the owner directly, so its reads and
writes skip the front-end hop. A layout is then another holder of the routing
decision, and **MUST** be recalled when the unit moves. Clients without such a
protocol use the front-end path; nothing in this document depends on layouts.

## 6. Reads on non-owners

| The bytes are | Served by |
| --- | --- |
| un-offloaded | the owner, or a member of the unit's replica set under [RFC 10 §8](rfc-10-journal-replication.md#8.%20Reads%20from%20replicas) |
| offloaded | any block service, from metadata and the remote tier, filling its own journal ([RFC 0 §6.2](rfc-0-data-lifecycle.md#6.2%20Fill)) |

**A cached copy is checked before it is served.** A non-owner that filled an
offloaded range may hold content the owner has since overwritten and offloaded
again. Before serving from its journal, it **MUST** compare the version its
journal holds ([RFC 1 §3.2](rfc-1-journal.md#3.2%20Read)) with the version on the current ref ([RFC 6 §2.1](rfc-6-block-metadata.md#2.1%20Ref)),
and on a mismatch drop its copy (`MarkDurable`'s stale rule, [RFC 1 §9.2](rfc-1-journal.md#9.2%20Offload%20state%20after%20recovery)) and
fetch again. The check costs one metadata read per cached read.

**Read tokens are the recorded next step.** A shared read token per range,
revoked when a write token is granted over it, would let a non-owner serve its
cache with no metadata read. It is deferred until the per-read check shows up in
measurement ([§12](#12.%20Open%20questions)).

## 7. Protocol state

Client-visible state is the protocol RFCs', but three rules follow from
ownership and are stated here so they are not lost:

1. **Open state that keeps an inode alive** ([RFC 7 §4.2](rfc-7-namespace-metadata.md#4.2%20Open%20state%20is%20the%20second%20holder)) **MUST** be a record in
   the metadata store naming its holder and carrying a lease. Held in one
   process, another block service can release an unlinked inode a client still
   has open elsewhere, drop its refs, and let sweep delete the content (I3).
2. **The NFS write verifier MUST change whenever a unit's owner changes**, or
   clients never resend writes they sent unstable to the old owner.
3. **Client locks are not write tokens.** They have their own semantics, grace
   period and recovery, and **MUST NOT** share records with tokens, though they
   **MAY** be served by the same metadata service.

## 8. Metadata consistency

Every commit that acts for a unit — existence ([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal)), offload
([RFC 6 §4.1](rfc-6-block-metadata.md#4.1%20What%20one%20commit%20records)), truncate and deallocate — carries the unit's owner epoch and is
refused, for that file, when the epoch is not current. The check **MUST** conflict
with any concurrent change of the epoch whatever the store's isolation level: a
read the store tracks for conflicts, or an explicit lock on the key. A plain read
under snapshot isolation is not a fence.

A file's `size` ([RFC 6 §2.4](rfc-6-block-metadata.md#2.4%20Shape%20and%20holes)) is written by whoever owns the file's writes. With
per-range units several owners write it; how they do so without contending on
every append is open ([§12](#12.%20Open%20questions)).

## 9. Failure

| Failure | Outcome |
| --- | --- |
| a block service is lost | its units fail over to members of their replica sets ([RFC 10 §9](rfc-10-journal-replication.md#9.%20Failover)) once its leases lapse; front-ends re-route on the first refusal |
| a front-end is lost | its clients reconnect to another front-end and reclaim their state through the protocol's grace period; no acknowledged write is lost, because the owner's replica set holds it |
| the configuration store is unreachable from a block service | the block service can neither renew its leases nor commit; it stops acknowledging when its leases run out, and its units fail over wherever the store is reachable |
| a partition separates block services but not the store | nothing changes for ownership, which is decided in the store; replication across the partition fails and [RFC 10 §7](rfc-10-journal-replication.md#7.%20Membership) removes the unreachable members |

## 10. Prior art

| System | Takes from it |
| --- | --- |
| Frangipani (SOSP 1997), GPFS (FAST 2002) | tokens held by the node using them and revoked on demand; byte-range tokens that split; a peer recovers a failed node's state only after fencing it |
| xFS (SOSP 1995) | a small reassignable map from file to managing node |
| Distributed metadata servers with subtree ownership, and client capabilities | ownership per subtree, with pinning favoured over automatic balancing in practice; revocable client grants for caching and buffering |
| Parallel NFS | the client routed to the data's owner by a layout the server recalls |
| Shared-everything designs over a fabric | the contrast: stateless front-ends reaching shared persistent media need no owner routing, at the price of storage every node can address |
| Fencing tokens (Kleppmann, 2016) | a lock is safe only if the resource rejects the stale holder |

## 11. Alternatives considered

| Alternative | Why not |
| --- | --- |
| Several writers of the same bytes, ordered by a global timestamp from the store | consensus per write; recent reads then need a quorum; offload of one file splits across nodes; nothing gained over forwarding, which costs the hop replication already pays |
| A read on another node sees a write only after it is offloaded | metadata shows the new size at once, so the range must be fetched from the owner anyway; close-to-open visibility would need close to wait for offload |
| A separate lock service beside the metadata store | a second source of truth for who may write; tokens in the store are fenced by the same transactions that use them |
| Tokens held in memory by a token manager | faster, but needs its own recovery; the store is used first, with a cache in front of it if it proves slow ([§12](#12.%20Open%20questions)) |
| Ownership only per share | caps one share at one node; kept as the per-share policy, not as the design |

## 12. Open questions

1. **Default granularity, and when to move a unit.** Per file is the likely
   default; the move policy that avoids ping-pong between two writers is
   unspecified.
2. **A cache in front of the configuration store** for token acquisition, if one
   transaction per file per session shows up in measurement.
3. **Read tokens** ([§6](#6.%20Reads%20on%20non-owners)), if per-read version checks show up in measurement.
4. **Several owners of one file**: `size` and existence commits written by every
   range owner; truncate and delete revoking every range first.
5. **Where client lock and delegation state lives** — in the metadata service, or
   with the owner — belongs to the protocol RFCs; [§7](#7.%20Protocol%20state)'s three rules bind it.
6. **Whether the metadata service itself is split** into shards with no
   transaction spanning them, and what that does to existence and release.

## 13. Conformance

Tokens, moves and routing are modelled with RFC 10's configuration protocol in
one model ([RFC 10 §14](rfc-10-journal-replication.md#14.%20Conformance)), and exercised by the same deterministic simulation. Properties:

| Property | Violated by |
| --- | --- |
| At most one block service's writes to any byte take effect under any epoch | a sender-side token check standing in for receiver fencing |
| A write forwarded on a stale route is refused, then applied once at the owner | a front-end trusting its cache, or a non-idempotent retry |
| A non-owner never serves cached content older than the current ref | a skipped version check |
| An open file's content survives its unlink on another block service | open state held in one process |
| A unit's writes resume after a move or a failover without operator action | a move that waits on a holder that is gone |
