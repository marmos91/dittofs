---
rfc: 10
title: "RFC 10 — journal replication"
component: journal replication
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-8-engine]]"
aliases:
  - RFC 10
tags:
  - rfc
---
# RFC 10 — journal replication

**Status:** draft — first write-up of the 2026-09-28 design discussion, not yet
reviewed. Every section is open to change; [§13](#13.%20Open%20questions) lists what is known to be
undecided.
**Depends on:** [RFC 0](rfc-0-data-lifecycle.md) for the terms and invariants; [RFC 1](rfc-1-journal.md) for the journal operations
this layer drives (`WriteAt`, `Apply`, `Settle`, `Export`, `SetEpoch`, `Discard`,
`MarkDurable`); [RFC 6](rfc-6-block-metadata.md) for the commits it fences; [RFC 8](rfc-8-engine.md) for the engine that
composes it. Ownership of files and ranges — who writes what — is RFC 11's; this
document states only what it assumes of it ([§3](#3.%20What%20it%20assumes%20of%20ownership)).
**Audience:** anyone designing or changing how an acknowledged write survives the
loss of the node that accepted it.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT** and **MAY** are
to be interpreted as in RFC 2119.

---

## 1. Purpose

A write is acknowledged once the journal holds it ([RFC 8 §4.1](rfc-8-engine.md#4.1%20The%20facade%20orders%20a%20write%3B%20adapters%20do%20not)), and until it is
offloaded the journal of the node that accepted it is the only copy. On one node
that is the design: the journal is on durable storage, and losing the device
loses what it held since the last offload. With several nodes serving one share,
losing a node must not lose acknowledged writes, and nodes other than the writer
should be able to serve recently written bytes.

This layer makes both true:

> **An acknowledged write is held by every journal in its replica set, and any of
> them can serve it once it is known to be current.**

It sits between the engine and the journal. The journal ([RFC 1](rfc-1-journal.md)) stays a local
store that knows nothing of peers; this layer decides which journals hold a file,
moves operations between them, and fences the ones that must no longer count.

### 1.1 Goals

1. **Durability across node loss.** An acknowledged write survives the loss of
   any number of nodes smaller than its replica set.
2. **Reads from replicas.** A member of a file's replica set **MAY** serve that
   file's recently written bytes from its own journal, under [§8](#8.%20Reads%20from%20replicas)'s rule.
3. **No consensus among block services.** Consensus is needed only to decide who
   owns and who replicates, and it is obtained from the configuration store
   ([§2.2](#2.2%20Roles)), which already runs it. No block service runs a consensus protocol.
4. **One code path.** A single node is a replica set of one ([§10](#10.%20A%20single%20node)); the engine
   **MUST** go through this layer in every deployment.

### 1.2 Non-goals

This layer **MUST NOT**:

- decide who owns a file or range, or move ownership — RFC 11;
- order writes from more than one writer to the same bytes — one writer per
  ownership unit is assumed ([§3](#3.%20What%20it%20assumes%20of%20ownership));
- offload, carve, sync to the remote tier, or evict — the owner's engine does,
  exactly as on one node;
- replicate metadata — the metadata store has its own durability;
- erasure-code journal content ([§12](#12.%20Alternatives%20considered)).

## 2. Model

### 2.1 Terms

| Term | Means |
| --- | --- |
| **ownership unit** | the set of bytes one owner writes: a share, a subtree, a file or a byte range, as RFC 11 decides |
| **owner** | the one block service that assigns versions for a unit and accepts its writes |
| **replica** | a block service whose journal holds a copy of the unit's un-offloaded operations |
| **replica set** | the owner and its replicas; the owner is a member |
| **configuration** | `{unit, epoch, owner, replicas, committed}`, held in the configuration store |
| **epoch** | a number raised by every change of configuration; the high half of every version the owner assigns ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)) |
| **committed point** | the newest version below which every operation of the unit is held by every member |
| **learner** | a block service receiving the unit's operations while it catches up, before it is a member |

### 2.2 Roles

A deployment has three roles. They **MAY** run in one process — a single node is
all three — or be split across machines.

| Role | Holds | Consensus |
| --- | --- | --- |
| protocol front-end | client sessions; forwards each operation to the owner of what it touches | none |
| metadata service | namespace and block metadata ([RFC 6](rfc-6-block-metadata.md), [RFC 7](rfc-7-namespace-metadata.md)); ownership and configurations | its store's own |
| block service | a journal per share, the engine, carver and syncer; this layer | none |

The **configuration store** is the metadata service's store. It **MUST** provide
linearizable compare-and-swap on a configuration, and **MUST** be reachable from
every block service. Nothing in this layer depends on which store provides it.

### 2.3 What a replica holds

A replica's journal holds the unit's operations at the versions the owner
assigned, applied through `Apply` ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Operations%20versioned%20elsewhere)) in whatever order they arrive.
Its segments are laid out in its own order; two members' journals are never
byte-identical and need not be. What they agree on is, for every byte, the newest
version applied — and highest-version-wins makes that independent of arrival order.

## 3. What it assumes of ownership

RFC 11 is not written yet. This layer relies on it for exactly these, and on
nothing else:

1. **One owner per unit at a time**, recorded in its configuration.
2. **Every configuration change is a compare-and-swap that raises the epoch.**
   An owner whose configuration is no longer current can learn it only by
   reading the store or by being refused ([§6](#6.%20Fencing)).
3. **The owner's lease.** The owner renews a lease in the configuration store;
   when it lapses, another member may take over ([§9](#9.%20Failover)). An owner **MUST** stop
   serving and acknowledging when its lease runs out by its own clock, reckoned
   pessimistically.
4. **Ownership follows the writer.** A unit usually moves to the block service
   writing it, by handover rather than failover: the old owner hands its un-offloaded
   operations to the new one through `Export`, then the configuration moves ([§9.4](#9.4%20Handover)).
5. **Commits are fenced.** Every existence and offload commit carries the owner
   epoch and is refused when it is not current ([RFC 6 §4.1](rfc-6-block-metadata.md#4.1%20What%20one%20commit%20records)).

Decisions already taken for RFC 11, recorded here so they are not lost: write
tokens are held in the configuration store and revoked on conflict; a token
starts wide and splits when another writer asks for an overlapping range; reads
of cached content on non-owners are validated against the current ref's version,
with read tokens a later step; the ownership unit is a concept whose granularity
— share, subtree, file, range — is a policy, fixed per unit when it is created.

## 4. The write path

The owner handles every write to the unit. For a write, deallocate, truncate or
delete:

1. **Assign.** The owner stages the operation in its own journal, which assigns
   its version under the current epoch ([RFC 1 §3.1](rfc-1-journal.md#3.1%20Write)).
2. **Replicate.** The owner sends the operation, with its version and the
   configuration epoch, to **every** replica in the current configuration, in
   parallel.
3. **Apply.** Each replica checks the epoch ([§6](#6.%20Fencing)), applies the operation with
   `Apply`, makes it durable, and answers.
4. **Existence.** Once every member holds the operation durably, the owner records
   existence ([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal)) in a commit fenced by its epoch.
5. **Acknowledge.** Only then is the client acknowledged.

**Every member, not a quorum.** An operation is acknowledged only when every
member of the current configuration holds it. That is what lets any single
member take over with every acknowledged write already in hand ([§9](#9.%20Failover)), and what
lets a replica serve reads ([§8](#8.%20Reads%20from%20replicas)). A member that cannot keep up is removed from the
configuration ([§7](#7.%20Membership)); the owner **MUST NOT** acknowledge around it while it is
still a member.

**Order against existence.** Step 4 **MUST NOT** precede step 3 on every member.
Recorded first, a crash of the owner leaves a range metadata says exists and no
surviving journal holds, which resolves **Lost** for a write never acknowledged —
and stays Lost.

**A client's flush** ([RFC 8 §9.4](rfc-8-engine.md#9.4%20Commit%20is%20answered%20by%20the%20journal)) is answered once every member has synced
the file's operations. On one node that is the journal's `Sync`.

**Batching.** The owner **SHOULD** batch operations to a replica and group their
syncs, exactly as the journal groups its own ([RFC 1 §6.2](rfc-1-journal.md#6.2%20Sync%20policy)). The unit of
acknowledgement stays the operation.

## 5. Offload and release

Only the owner offloads, as on one node, and its commits carry its epoch
([§3](#3.%20What%20it%20assumes%20of%20ownership)). Replicas never offload.

**Settling.** The owner sends each replica its committed point with every batch.
A replica settles the unit's files to it ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Operations%20versioned%20elsewhere)), which drops removal
markers below it. The owner's own content is settled as it assigns it.

**Replicas release by being told.** After an offload commit lands, the owner
sends the members the extents it covered with the commit's `oldest` and `newest`.
Each calls `MarkDurable` ([RFC 1 §9.2](rfc-1-journal.md#9.2%20Offload%20state%20after%20recovery)) and then evicts under its own capacity
policy. The rule that protects the owner protects a replica: an extent is marked
only where nothing newer is held there. A notice carries the epoch and is
refused, like any operation, when it is stale. A replica **MAY** also derive the
same marks from metadata directly, which is how it catches up on notices it
missed.

**Pressure.** A replica cannot offload to make room. When its journal nears
capacity it **MUST** say so to the owner, and the owner **MUST** treat a member's
pressure as its own ([RFC 8 §7.2](rfc-8-engine.md#7.2%20A%20capacity%20refusal%20comes%20back%20here)): offload sooner, then refuse writes. A replica
whose journal refuses an operation is lagging, and [§7](#7.%20Membership) applies.

## 6. Fencing

Every message this layer sends — an operation, a committed point, a durability
notice, a seal — carries the configuration epoch it was sent under. **The receiver
enforces it:**

- a replica **MUST** refuse a message whose epoch is lower than the one it has
  installed for the unit;
- a replica that receives a higher epoch than it knows **MUST** refuse the
  message, read the configuration and install it before accepting anything more;
- the metadata store refuses a commit whose owner epoch is not current
  ([§3](#3.%20What%20it%20assumes%20of%20ownership)).

A check made only by the sender is not a fence. An owner that pauses past its
lease, then resumes, still believes it owns the unit; what stops it is that every
receiver has installed the new epoch before the new owner accepts a write ([§9](#9.%20Failover)).

Versions carry the epoch in their high half ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)), so anything a
superseded owner manages to write is older than everything its successor writes.

## 7. Membership

**Replica count.** Each unit has a configured replica count and a floor. The
owner requests replicas up to the count; below the floor it **MUST** refuse
writes, and report the unit as under-replicated through share health rather than
only in a log ([RFC 0 §10.2](rfc-0-data-lifecycle.md#10.2%20No%20state%20requires%20intervention%20to%20leave)). A floor of one keeps writes available with no
redundancy; that is a deployment's choice, stated in its configuration.

**Placement.** Members of one unit **SHOULD** be in different failure domains.
What a domain is — host, rack, zone — is configuration.

**Removal.** A replica that does not answer within a configured bound, or whose
lag exceeds one, is removed: the owner writes a configuration without it at the
next epoch, installs the epoch on the remaining members, and continues. Waiting
for a slow member instead stalls every write of the unit. Before a configuration
that removes a replica accepts writes, that replica's read lease ([§8](#8.%20Reads%20from%20replicas)) **MUST**
have expired.

**Joining.** A new or returning block service joins as a learner:

1. it `Discard`s whatever it holds of the unit's files ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Operations%20versioned%20elsewhere)) — a returning
   copy may hold operations its configuration never acknowledged;
2. the owner streams it `Export` of every file of the unit, and meanwhile sends it
   live operations as it would a member, without waiting for its answer;
3. when it has applied everything up to the owner's committed point, the owner
   adds it at the next epoch.

A learner is never counted for acknowledgement and never serves reads.

## 8. Reads from replicas

The owner serves reads of the unit from its journal, as on one node. A replica
**MAY** serve a read from its own journal when both hold:

- **it holds a read lease** on the current configuration, granted by the owner
  and shorter than the time a configuration change waits for it ([§7](#7.%20Membership));
- **the range is clean**: every version it holds there is at or below the latest
  committed point it has received, and it holds no operation above that point
  that touches the range.

A range that is not clean is **dirty**: the replica asks the owner for the
newest version of the range and serves its own bytes only if they carry it;
otherwise it forwards the read to the owner. This is chain replication's
apportioned-read rule (CRAQ) applied per range.

Why the lease: a replica removed from the configuration stops receiving
operations, and without the lease it would go on answering reads with content
that is no longer current. The lease is what makes removal safe for readers; it
assumes clocks whose drift is bounded by configuration.

A block service that is not a member never serves the unit's un-offloaded bytes
from its journal. It forwards to a member, or reads offloaded content through
metadata and the remote tier, filling its own journal ([RFC 0 §6.2](rfc-0-data-lifecycle.md#6.2%20Fill)); that
content is validated against the current ref's version before it is served
(RFC 11).

## 9. Failover

### 9.1 Why one member suffices

Every acknowledged operation is held by every member ([§4](#4.%20The%20write%20path)). So any one member
that was in the configuration when the owner failed holds every acknowledged
operation, and can take over with no need to gather a quorum. What it may lack,
or others may hold beyond it, are operations never acknowledged.

### 9.2 Takeover

When the owner's lease lapses, a member takes over:

1. **Claim.** It writes a configuration naming itself owner at the next epoch,
   by compare-and-swap. Members it cannot reach are dropped from the new
   configuration and must rejoin as learners ([§7](#7.%20Membership)). A member that restarted
   uncleanly **MAY** claim only once it has reopened its journal, which is durable
   and complete for everything it acknowledged.
2. **Install.** It installs the new epoch on every member it kept. From here the
   old owner can reach no member, and nothing it sends counts.
3. **Seal.** It asks each kept member which ranges it holds operations for above
   the committed point the new owner last received, and adds its own. Those are
   the **uncertain ranges**: content that may or may not have been acknowledged.
   For each, the new owner re-issues, under the new epoch, the content it holds
   there — its own bytes, or a journal removal where it holds nothing — and
   replicates it like any operation. The new epoch outranks every old one, so
   every member converges on the new owner's view of the uncertain ranges.
4. **Trim.** It truncates each file's journal content to the file's size in
   metadata, under the new epoch, so no unacknowledged content beyond the end of
   a file is ever offloaded.
5. **Record.** It records in the configuration store that the old epoch is
   sealed, and only then accepts writes.

A seal not recorded is a seal that did not happen: if the claimant fails before
step 5, the next claimant repeats the procedure from its own state.

### 9.3 After takeover

The new owner offloads what it holds, which includes everything the old owner
had not. Nothing was lost as long as one member of the old configuration
survived. The old owner, if it returns, is not a member: it rejoins as a learner
and discards what it held.

### 9.4 Handover

A planned move — ownership following the writer — does not wait for a lease:

1. the new owner joins the unit's replica set as a learner, if it is not a member;
2. the old owner stops accepting writes for the unit and drains what is in flight;
3. it writes the configuration naming the new owner at the next epoch, by
   compare-and-swap;
4. the new owner installs the epoch and accepts writes.

No seal is needed: the old owner acknowledged nothing it had not replicated to
every member, and stopped before the move.

## 10. A single node

On one node the unit's replica set is the node itself. Replication is a local
call, the epoch never changes, the committed point is whatever the journal has
synced, reads are always the owner's, and failover cannot happen. The engine
**MUST** still compose this layer, not the journal directly: two code paths means
every rule in this document is exercised by one of them and not the other, and the
untested one is the one that ships to the deployment that needs it.

## 11. Prior art

Every system below that keeps consensus off the data path does it the same way:
one writer orders each unit, a service that already runs consensus holds the
configuration and an epoch, and every message is fenced by it. This design is
that pattern.

| System | What this design takes from it |
| --- | --- |
| PacificA (Lin et al., MSR-TR-2008-25) | configuration kept apart from data replication; commit needs every replica; a new primary reconciles before serving |
| Windows Azure Storage stream layer (Calder et al., SOSP 2011) | acknowledgement from all replicas; a failed writer is sealed rather than repaired, and appends continue under a new configuration |
| Chain replication (van Renesse, Schneider, OSDI 2004); CRAQ (Terrace, Freedman, USENIX ATC 2009) | reads from replicas: clean ranges served locally, dirty ones checked with the writer |
| Kafka, KIP-101, KIP-279, KIP-966 | truncation by epoch, never by a local watermark; a replica that restarted uncleanly is not a takeover target until caught up |
| Aurora (Verbitski et al., SIGMOD 2017, 2018) | a single writer's versions make replica acknowledgements consistent; a recovery's truncation recorded durably under its epoch |
| GFS (SOSP 2003), HDFS lease recovery | the configuration version bumped before a new writer writes; stale replicas detected by version |
| Ceph peering | a new primary records that it is active before serving, or a short-lived primary can acknowledge writes nobody finds |
| Frangipani (SOSP 1997) | a lease checked only by the sender is not a fence: storage must reject a stale writer |
| Assise (OSDI 2020) | a local log replicated before acknowledgement and published asynchronously — the same shape as journal plus offload |
| FoundationDB (SIGMOD 2021), TigerBeetle | deterministic simulation as the primary test method |

## 12. Alternatives considered

| Alternative | Why not |
| --- | --- |
| Every block service writes, and journals order operations differently | Two writers of one range leave replicas holding different bytes with no version comparable across them. Ordering needs one writer per unit or consensus per write. |
| Consensus per write through the configuration store's timestamp service | Moves consensus from rare events to every write. Reads of recent data then need a quorum or a metadata lookup, several nodes offload overlapping content, and `size` becomes contended. It buys nothing over forwarding to the owner, which costs the same hop the replication already pays. |
| Acknowledge at a quorum of the replica set | Hides a slow replica, but a replica can then miss acknowledged writes, so none can serve reads, and failover needs a read quorum and a durable truncation record. |
| Replicas read another's writes only after offload | Metadata shows a new size at once; reading the unoffloaded range on another node must then wait for offload or fetch from the owner anyway. It also breaks close-to-open visibility unless close waits for offload. |
| Erasure-coded journal content | Journal content is small, overwritten and short-lived: stripes need read-modify-write on partial writes, failover must reconstruct, and no replica holds whole data to serve reads. It saves capacity only on the un-offloaded working set. Erasure coding belongs at rest, where blocks are sealed. |
| A shared journal on storage every node can reach, or a replicated log service | Either requires shared-access storage or another service to deploy and keep healthy. Replication among block services needs neither. |
| Journal in a key-value store or an in-memory cache | Weaker durability or capacity, write amplification on large values, and the journal's semantics rebuilt on top. |

## 13. Open questions

1. **Ownership unit granularity** (RFC 11). This document works for any unit; the
   cost of a seal grows with the files in a unit.
2. **Several owners of one file.** With byte-range units, every range owner
   writes the file's `size` ([RFC 6 §2.4](rfc-6-block-metadata.md#2.4%20Shape%20and%20holes)); truncate must revoke every range first.
   Unspecified.
3. **The seal's cost.** Asking every member for its uncertain ranges is
   proportional to operations above the committed point; the bound on that — the
   committed point's lag — is unmeasured.
4. **Clock assumptions.** The read lease and the owner's lease assume bounded
   drift. The bound and what happens when it is exceeded are unspecified.
5. **Whole-cluster restart.** Every lease has lapsed; every unit needs a takeover
   before it serves. Whether that is acceptable at thousands of units is
   unmeasured.
6. **Protocol state** — client locks, opens, delegations, the NFS write verifier —
   belongs to the protocol RFCs, but the write verifier **MUST** change whenever a
   unit's owner does, or clients never resend writes the old owner held unsynced.
7. **Journal capacity** is multiplied by the replica count for un-offloaded
   content. Whether offload keeps that bounded under sustained writes is
   unmeasured.

## 14. Conformance

This layer is a distributed protocol, and no amount of review establishes one.
Conformance rests on three kinds of check, and all three are required.

**A model.** The configuration, fencing, takeover and seal of [§6](#6.%20Fencing)–[§9](#9.%20Failover) **MUST**
be modelled in a model checker before they are implemented, with the invariants
below as properties, and the model **MUST** be kept in step with this document.

**Deterministic simulation.** The layer reaches the network, the journals' storage
seam ([RFC 1 §1.3](rfc-1-journal.md#1.3%20It%20is%20testable%20on%20its%20own)), time and randomness only through interfaces. A simulator
drives a cluster of instances in one process from a seed, injecting message loss,
delay, reordering and duplication, partitions, process pauses, crashes that
discard unsynced writes, and clock drift within the configured bound. A failing
seed **MUST** reproduce the failure exactly.

**Whole-system fault injection** against real processes, checking the histories
clients observe for linearizability of acknowledged writes.

Properties every one of them checks:

| Property | Violated by |
| --- | --- |
| An acknowledged write is held by some journal of the current configuration, or durable remotely, at every moment | a quorum acknowledgement, a seal that drops content, a learner counted as a member |
| No member's journal serves, for a range, content older than the newest acknowledged write there, while it holds a read lease | a removed replica still serving, a dirty range served as clean |
| Removed content is never served again | a late operation below a removal marker, a rejoining copy that did not discard |
| Nothing a superseded owner sends after the epoch is installed takes effect anywhere | a sender-side lease check standing in for receiver fencing |
| A unit with a member of the last configuration alive becomes writable again without operator action | a takeover that waits for a quorum |
