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
  - "[[rfc-11-ownership]]"
  - "[[rfc-16-metadata-store]]"
aliases:
  - RFC 10
tags:
  - rfc
---
# RFC 10 — journal replication

**Status:** draft. [§16](#16.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone designing or changing how an acknowledged write survives the
loss of the node that accepted it.

---

## 1. Purpose

A write is acknowledged once the journal holds it ([RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more)), and until it is
offloaded the journal of the node that accepted it is the only copy. On one node
that is the design. With several nodes serving one share, losing a node must not
lose acknowledged writes. This layer makes that true:

> **An acknowledged write is held by the journal of every node in its replica
> set until it is offloaded.**

It sits between the engine and the journal. The journal ([RFC 1](rfc-1-journal.md)) stays a local
store that knows nothing of peers; this layer decides which journals hold a shard,
moves operations between them, and fences the ones that must no longer count.

### 1.1 Goals

1. **Durability across node loss.** An acknowledged write survives the loss of
   every node of its replica set but one that is not a learner.
2. **Reads anywhere.** Any storage node **MAY** serve recently written bytes it
   holds, after one round trip to the primary that carries no bytes ([§8](#8.%20Reads)).
3. **No consensus among storage nodes.** The metadata store is the only consensus
   ([§3](#3.%20What%20it%20assumes%20of%20shard%20placement)); it is consulted when the replica set changes, never per write.
4. **A new node takes writes at once**, with no pause in the shard's writes ([§7.3](#7.3%20Joining)).
5. **One code path** on one node as on many ([§10](#10.%20A%20single%20node)).

### 1.2 Non-goals

This layer **MUST NOT**:

- decide which node is a shard's primary, or move it — [RFC 11](rfc-11-ownership.md);
- order writes from more than one writer to the same bytes — one writer per shard
  is assumed ([§3](#3.%20What%20it%20assumes%20of%20shard%20placement));
- offload, carve, sync to the remote tier, or evict — the primary's engine does,
  exactly as on one node;
- replicate metadata — the metadata store has its own durability;
- erasure-code journal content ([Appendix B](#Appendix%20B%20%E2%80%94%20alternatives%20considered)).

Which process runs which role is [RFC 15](rfc-15-topology.md#2.1%20One%20binary%2C%20roles%20chosen%20at%20deployment)'s. This document speaks only of
**storage nodes** — nodes with the `storage` role, each with a journal per device
— and of the metadata store.

## 2. Model

### 2.1 Terms

**Shard**, **primary**, **shard record** and **node lease** are the
[glossary](rfc-0-data-lifecycle.md#Glossary)'s. Every rule here is per shard. The
primary also holds the shard's namespace writes and open state, which this layer
does not replicate.

| Term | Means |
| --- | --- |
| **replica** | a storage node whose journal receives a copy of every operation the primary assigns for the shard |
| **replica set** | the primary and its replicas |
| **learner** | a replica that joined at a **join point** and has not yet been given the shard's older content ([§7.3](#7.3%20Joining)) |
| **epoch** | a number in the shard record, raised by every change to it and carried by every version and message ([§6](#6.%20Fencing)) |
| **committed point** | per shard, the newest version at or below which the whole replica set durably holds every operation and metadata records every removal. The primary sends it to its replicas, and each keeps it durably |
| **drift bound** | the bound on clock drift with which every node reckons its node lease ([§3](#3.%20What%20it%20assumes%20of%20shard%20placement)) |

![Primary, replicas and the shard record](img/rfc10-replica-set.svg)

**What the epoch buys.** A version is an epoch and a counter, compared as one
number ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)): after a takeover to epoch 4, `v(4,1)` outranks `v(3,90)`, and a
late `v(3,95)` from the old primary loses to it in every journal it reaches.

**What the replicas agree on.** Journals are not byte-identical on disk, but **at
or below the committed point every journal of the replica set returns the same
bytes at the same version for any file and offset**. Above it they can differ;
a takeover resolves those ranges ([§9.2](#9.2%20Takeover)).

### 2.2 One journal carries many shards

A storage node keeps one journal per device ([RFC 1](rfc-1-journal.md)), holding files of every shard
the node is primary or replica of. Nothing in this layer is per journal:

- every journal call and every procedure acts per file, and only on the files the
  journal holds content for — never on every file of a shard, which can hold
  millions;
- the node keeps, per shard, an **install record** — the installed epoch, the
  committed point and its own join incarnation — durably and on the device of the
  journal it describes, so losing the device loses both;
- capacity is the journal's, shared under its per-share limits ([RFC 1 §7](rfc-1-journal.md#7.%20Capacity));
  [§5](#5.%20Offload%20and%20release) says how pressure reaches primaries;
- a replica is identified by its node, the **journal identity** of the journal
  holding the shard ([RFC 1 §4.1](rfc-1-journal.md#4.1%20Layout)) and a join incarnation, and so is the primary,
  whose entry in the shard record adds its node epoch. A lost device therefore
  loses the node's place in every shard that journal held;
- each journal has a **generation**, kept in its `format` file and in the
  metadata store under its journal identity ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)). At every open of
  the journal, every acquisition of its node's lease and every renewal of it
  (every 3 s), the node reads the generation `g` in `format`, writes `g + 1`
  there and syncs it, then raises the store's to `g + 1` by compare-and-swap,
  conditional on the store's being at most `g` — a crash, or a failed renewal,
  between the sync and the swap leaves the store one behind. At a renewal the
  swaps ride in the renewal's own transaction, beside the lease's new expiry; it
  writes the lease's expiry record and the journals' generation records, never
  the node record, so it still does not conflict with fenced commits ([§3](#3.%20What%20it%20assumes%20of%20shard%20placement)
  item 3). A swap that loses does not fail the renewal; it fails only its
  journal. A node **MUST** complete the swap at open and at acquisition before it
  serves or acknowledges anything from the journal. A journal whose swap loses
  was rolled back — restored from an older copy, or from a VM image taken before
  its node's last successful swap — and is amnesiac for every shard it holds
  ([§6](#6.%20Fencing)): the node stops serving from it, durably drops its install records,
  then swaps again from the store's generation, so the amnesia outlives a crash
  and the next open does not lose again. Any image older than one renewal
  interval therefore loses. The cost is one sync of `format` per journal per
  renewal; the compare-and-swaps add no round trip.

### 2.3 The journal extension

The journal of [RFC 1](rfc-1-journal.md) assigns every version itself. Replication needs a journal
that also takes versions assigned elsewhere, raises a file's epoch, and forgets a
file on command. That is a **later journal format version**, added as
[RFC 1 §4.3](rfc-1-journal.md#4.3%20Records) prescribes: a binary that knows it opens a journal of RFC 1's
version and upgrades it on its first extension record; a binary that does not
refuses the newer journal.

```go
Apply(s ShardID, id FileID, op Op) error                         // write, deallocate, truncate or delete, with its version and digest
Export(s ShardID, id FileID, from Version) iter.Seq2[Op, error]  // un-offloaded content and removal markers, with bytes
SetEpoch(s ShardID, id FileID, e uint64) error
Discard(s ShardID, id FileID) error
```

It adds two record kinds, **epoch** and **discard**, lifts RFC 1's rule that a
version's epoch half is zero ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)), and gives `Offload` a **ceiling**:
an offer includes only content at or below the version the caller passes. This
layer passes the shard's committed point ([§5](#5.%20Offload%20and%20release)).

**Entries are per shard.** The extension keys a file's entry by (shard, file),
and its epoch record names the shard as well as the epoch. One journal can
therefore hold a file under two shards while a move runs ([§9.4](#9.4%20Handover)), and every
call names the shard and touches that entry only. A node serves a file only from
its entry under the shard whose primary it asked ([§8](#8.%20Reads)). An entry with no epoch
record, as every entry of RFC 1's format is, belongs to the shard metadata
records its file in.

**`Apply`** takes an operation whose version another journal assigned and applies
it by version: at each byte it takes effect only where it is newer than what the
journal holds there, content and removal markers alike. An equal version is a
repetition and changes nothing when its **digest** matches — a hash of the
operation's request ID, kind, range and bytes, which every operation carries and
its record keeps — and is refused with `ErrDivergent` when it does not. Applied
operations are durable, reserve capacity, advance the file's change sequence
([RFC 1 §3.4](rfc-1-journal.md#3.4%20Fill)) and are recorded exactly as assigned ones are. The caller **MUST
NOT** apply two different operations under one version of one file. A journal
that applied a set of operations in any order therefore holds exactly what one
that applied them in version order holds.

**`SetEpoch`** raises the epoch under which the journal assigns versions for
`id` under `s`. `e` **MUST** exceed the file's current epoch, and the epoch record
**MUST** be durable before the first version under it is assigned. The primary
raises a file's epoch to the shard's just before assigning the file's first
version under it, so a new epoch costs nothing for files it never writes.

**`Export`** is RFC 1's `Since` with each write's bytes, **skipping offloaded
extents**: it yields, in version order, operations that reproduce every
un-offloaded extent and removal marker of `id` at or above `from`, each at its own
version.

**`Discard`** forgets the entry of `id` under `s` entirely — held extents
whatever their offloaded bit, removal markers and epoch — as if the journal had
never held it, and leaves the file's entry under any other shard alone. Its record
covers every record of the file with a lower sequence number whatever its
version, the one exception to precedence; it drops the file's entry by raising
the floor sequence ([RFC 1 §3.4](rfc-1-journal.md#3.4%20Fill)), is durable on return, and recovery **MUST NOT**
bring back anything it discarded. **Discarding dirty content destroys it unless
another journal holds it**; the caller **MUST** know that one does.

**Retention.** A file's newest epoch record is live, and carried forward by
repack ([RFC 1 §8.2](rfc-1-journal.md#8.2%20Repack)), only while the file has a held extent or a removal marker. A
journal that has forgotten a file's epoch treats it as having none, and the
primary's `SetEpoch` before the next assignment re-establishes it. A discard record
is live while it still covers a record of its file in another segment.

**Snapshot holds and stamps are replicated operations.** `Hold`, `Unhold` and
`Stamp` ([RFC 1 §3.11](rfc-1-journal.md#3.11%20Snapshot%20holds)) are sent by the primary to every replica and applied
there like any operation, and a `Hold` returns only once its marks are durable on
every replica of the shard ([RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)). A replica's `Hold`
acknowledgement returns its journal's held bytes and dirty bytes not yet
offloaded, summed over every share it carries, so the primary evaluates the hold
bound for every journal of the replica set. `Export` yields, beside un-offloaded
content, every held superseded version with the hold marks that keep it and every
version's stamp, so whatever an export feeds — a join, a reconcile, a move between
shards — carries the holds and the stamps too.

**Settling** needs no new record. A node calls RFC 1's `Settle` on a shard's files
at the committed point it has persisted, which drops removal markers at or below
it ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Settle%20and%20Since)), and settles again after a restart. No marker is needed for what
settling drops or offload released: [§6](#6.%20Fencing)'s refusal at the committed point keeps
every late operation below it out.

## 3. What it assumes of shard placement

[RFC 11](rfc-11-ownership.md) provides these; this layer relies on nothing else:

1. **One primary per shard, named in its shard record** as (node, node epoch,
   journal identity, incarnation). The metadata store is the only consensus. It
   **MUST** provide linearizable compare-and-swap on a shard record and be
   reachable from every storage node, and every change to a record raises its
   epoch but one ([§10](#10.%20A%20single%20node)). Records change only when the primary or the replica
   set changes, never on lease renewal.
2. **A file's epoch never decreases**, including when it moves between shards
   ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).
3. **One node lease per storage node**, with a node epoch. The lease lasts 10 s
   and is renewed every 3 s, and the drift bound is 500 ms, all installation
   settings ([RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings)). The node record holds the node epoch and
   whether a takeover has marked it lapsed; renewals write only the lease's own
   expiry record and its journals' generations ([§2.2](#2.2%20One%20journal%20carries%20many%20shards)), never the node record ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)). Losing the lease fences all of the node's shards.
   A node **MUST** stop acknowledging and serving — open state included — when its
   lease runs out by its own clock less the drift bound (9.5 s of the 10 s), and
   when it has not reached the store for half its lease (5 s). A renewal returns
   the store's time ([RFC 9 §1.2](rfc-9-gc.md#1.2%20Words%20this%20document%20uses)); a node whose clock differs from it by more than half
   the drift bound, less half the renewal's round trip, self-fences. A drift
   beyond the bound can break open-state exclusivity, which only the lease
   protects, but not durability, which receivers and the store fence.
   **Renewal fails once the lease has lapsed** — in store time, or because a
   takeover marked the node record: the node acquires a new lease at a higher node
   epoch, no sooner than the old expiry plus the drift bound in store time, and is
   then primary of nothing until it re-claims ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).
4. **A shard whose primary's lease lapsed is taken over, never plainly claimed**,
   and only by a replica that is not a learner ([§9.2](#9.2%20Takeover)); a claim by any other
   node would leave the replicas outside the record ([RFC 11 §3.2](rfc-11-ownership.md#3.2%20The%20primary%20stays%20until%20another%20node%20needs%20it)).
5. **The primary follows the writer** by handover, not failover ([§9.4](#9.4%20Handover)).
6. **Commits are fenced at the store.** Every metadata commit acting for a shard
   carries its (shard, epoch) and its primary's (node, node epoch). It is refused
   unless the file's fence records hold exactly that (shard, epoch) — *current*
   means equal, and a fence from another shard never matches — and unless the
   primary's node record, which the commit guards, holds that node epoch
   unmarked ([RFC 11 §8](rfc-11-ownership.md#8.%20Metadata%20consistency)). The fence records order epochs per file; the guard
   fences the files a new primary has not yet touched, because a takeover marks
   the old primary's node record lapsed as it claims ([§9.2](#9.2%20Takeover)), and every commit
   still in flight under the old node epoch then conflicts and aborts.

**An unknown compare-and-swap outcome** — a claim, a join, a removal, clearing a
learner, a handover — is resolved by re-reading the shard record and acting on
what is there, before anything else.

## 4. The write path

The primary handles every operation on the shard. For a write, deallocate, truncate
or delete:

1. **Assign.** The primary stages the operation in its own journal, which assigns
   its version under the file's epoch. It carries the request ID of the routed
   call ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)), so a new primary that holds it answers a retry instead
   of applying it again.
2. **Replicate.** The primary sends it, with its epoch, to every replica in the
   shard record, learners included, in parallel.
3. **Apply.** Each replica checks it against [§6](#6.%20Fencing), applies it with `Apply`,
   makes it durable and answers.
4. **Acknowledge.** Once the primary and every replica hold it durably, the client
   is acknowledged.

![The write path](img/rfc10-write-path.svg)

**The whole replica set, not a quorum.** An operation **MUST NOT** be acknowledged
before step 4; that is what makes one replica enough for a takeover ([§9.1](#9.1%20Why%20one%20replica%20suffices)).
A replica that cannot keep up is removed ([§7.2](#7.2%20Removal)); the primary **MUST NOT**
acknowledge around it while it is listed.

**Metadata follows the replica set.** Until a write's existence is committed, the
journal is its authority ([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal)). The primary group-commits existence at the
stability point and **MUST** include only operations the whole replica set holds.
A truncate, deallocate, release or clone is a synchronous metadata operation: its
journal operation **MUST** be held by the whole replica set before its metadata
commit, and the client is acknowledged after that commit. Recorded first, a crash
of the primary leaves metadata describing an operation no surviving journal holds.

**A client's flush** ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)) is answered once every node of the replica set
has synced the file's operations and their existence is committed.

**The committed point** ([§2.1](#2.1%20Terms)) rides on every batch. A replica persists it
in its install record, never lowers it, and then settles to it ([§2.3](#2.3%20The%20journal%20extension)).

**Batching.** The primary **SHOULD** batch operations to a replica and group their
syncs, as the journal groups its own ([RFC 1 §6.2](rfc-1-journal.md#6.2%20Sync%20policy)). The unit of
acknowledgement stays the operation.

## 5. Offload and release

Only the primary offloads, as on one node, and **only content at or below the
committed point** — it passes the point as the offer's ceiling ([§2.3](#2.3%20The%20journal%20extension)). A newer
ref committed ahead of an older operation still in flight would make that
operation's commit be refused as older ([RFC 6 §4.4](rfc-6-block-metadata.md#4.4%20Commits%20for%20one%20file%20apply%20in%20order)), and it could never become
durable. Replicas never offload.

**Replicas release by being told.** After an offload commit lands, the primary
sends each replica the extents it covered with the commit's `oldest` and
`newest`. The replica calls `MarkDurable` ([RFC 1 §9.2](rfc-1-journal.md#9.2%20Offload%20state%20after%20recovery)) and evicts under its own
capacity policy. The notice also raises the replica's committed point to at least
`newest`, since the primary offloaded nothing above its own point: a replica that
evicted content therefore never reports a point below it ([§9.2](#9.2%20Takeover)). A replica
**MAY** derive the same marks from metadata, which is how it catches up on notices
it missed.

**Pressure.** A replica cannot offload to make room, and its journal is shared
with every shard it holds. When the journal nears capacity it **MUST** tell the
primary of each shard holding un-offloaded content in it, and each primary **MUST**
treat that as its own pressure ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)): offload sooner, then pace and
refuse writes. While the replica still acknowledges, backpressure is the answer,
not removal. A replica whose journal refuses an operation within its share's
limit is lagging for that shard alone, and [§7.2](#7.2%20Removal) applies — rate-limited, so
one full device does not remove itself from every shard at once.

**Under-replication means offloading eagerly.** While a shard is under-replicated
([§7.1](#7.1%20Count%2C%20floor%20and%20placement)), its primary offloads as soon as content is committed, so the content
only its survivors hold, and the tail a new replica must copy, shrink ([§7.3](#7.3%20Joining)).

## 6. Fencing

Every message this layer sends — an operation batch, a durability notice, an
install, a seal request, a newest-version query — carries the epoch it was sent
under. **The receiver enforces it:**

- it **MUST** refuse a message whose epoch is lower than the one it has installed
  for the shard;
- on a higher epoch it **MUST** refuse the message, read the shard record and
  install it before accepting anything more. Installing **MUST** be durable before
  it answers anything under the new epoch, so a restart never accepts what it
  refused before;
- when it installs a record that does not list it — by node, journal identity and
  incarnation — it **MUST** stop serving the shard and `Discard` its entries of
  the shard's files, and only those. It may return only by joining again ([§7.3](#7.3%20Joining));
- when a record lists it but it has no install record for the shard — its device
  was replaced or wiped — or its journal lost the generation swap ([§2.2](#2.2%20One%20journal%20carries%20many%20shards)), it
  **MUST** refuse with `ErrAmnesiac` and rejoin as a learner; listed as primary, it
  serves nothing. A record naming a dead node's ID is therefore safe to reuse: the
  new journal matches nothing listed;
- it **MUST** refuse every operation at or below its committed point. It refuses
  with `ErrCommitted` if it holds that version of the file with the same digest
  ([§2.3](#2.3%20The%20journal%20extension)) — a repetition of an operation the whole replica set already held, which
  the primary counts as held — and with `ErrDivergent` otherwise. A learner's
  copied tail is not refused ([§7.3](#7.3%20Joining)).

**`ErrDivergent` means the primary was rolled back.** A primary never waits on an
operation at or below a replica's committed point, since it sends that point only
once the whole replica set holds everything below it; only a primary restored from
a VM image younger than one renewal interval, which the generation swap cannot
catch ([§2.2](#2.2%20One%20journal%20carries%20many%20shards)), assigns a version a second time. A
primary that meets `ErrDivergent` for an operation it is waiting on **MUST**
acknowledge nothing more, stop renewing its lease, raise the store's generation of
each of its journals without writing it to the journal — so each reopens amnesiac
— and restart. Its replicas take the shard over holding every operation it
acknowledged before the rollback. A refusal of a late duplicate reaches a primary
that is not waiting on it, and is ignored.

A check made only by the sender is not a fence. A primary that pauses past its
lease still believes it is the shard's primary when it resumes; what stops it is
that every receiver has installed the new epoch before the new primary accepts a
write, and that the claim marked its node record lapsed ([§3](#3.%20What%20it%20assumes%20of%20shard%20placement) item 6).

A replica removed while cut off from the primary hears no refusal. It **MUST**
re-read, on a configured period, the shard record of every shard it holds content
for, and discard those that no longer list it.

## 7. Changing the replica set

### 7.1 Count, floor and placement

Each shard has a **replica count** and a **floor**, both in its shard record and
both counting the acknowledgement set — the primary and every replica. The default
is a count of 3, the primary and two replicas, and a floor of 2; a single node is
1 and 1. A floor above one **MUST** count at least one replica that is not a
learner, since a learner cannot take over. Below the floor the primary **MUST**
refuse writes and report the shard through share health rather than only in a log
([RFC 0 §10.2](rfc-0-data-lifecycle.md#10.2%20No%20state%20requires%20intervention%20to%20leave)). A shard is **under-replicated** while its replica set has fewer
members that are not learners than the count; it keeps writing, and its primary
offloads eagerly ([§5](#5.%20Offload%20and%20release)).

A floor of one keeps writes available with no redundancy; that is a deployment's
choice, stated in its settings.

Replicas of one shard **SHOULD** be in different failure domains. What a domain is
— host, rack, zone — is a setting.

### 7.2 Removal

The primary removes a replica that does not answer within a configured bound,
that lags by more than a configured amount of bytes or time — a slow node that
still answers is removed like a silent one — whose journal refuses within its
share's limit ([§5](#5.%20Offload%20and%20release)), or that refuses as amnesiac ([§6](#6.%20Fencing)). It writes the record without it at the next epoch by
compare-and-swap and installs that epoch on the remaining replicas; operations
the removed replica had not acknowledged are then acknowledged without it.

A primary remembers the highest committed point each replica has acknowledged,
and persists these marks in a side record of the shard ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)) on a
configured period and before every record change it makes. The side record is not
the shard record, so writing it raises no epoch, and a mark is never lowered. A
replica that reports a point below its mark has lost state it acknowledged and is
removed the same way; a claimant gathering points reads the marks too ([§9.2](#9.2%20Takeover)).

`ponytail:` a mark persisted on a period misses a rollback to a point between the
persisted and the live mark while the primary is also restarted. The generation
swap ([§2.2](#2.2%20One%20journal%20carries%20many%20shards)) catches a restore from any image older than one renewal interval,
so the window is open only to a torn disk, which loses synced writes without
rolling the generation back — an image younger than one renewal is the same case.
Persist the mark with every committed point when one shows up in the fault record.

### 7.3 Joining

A new or returning node joins as a learner, and counts for new writes at once:

1. It `Discard`s its entries of the shard's files: a returning copy can hold
   operations never acknowledged.
2. The primary writes the record adding it at the next epoch by compare-and-swap,
   as a learner with a new incarnation and a **join point** `J`: the first version
   the primary will assign under the new epoch. It installs the epoch on every
   replica, the new one included.
3. From `J` on, the primary sends it every operation and waits for its answer like
   any replica's. It counts for acknowledgement of every operation from `J`.
4. The **tail** — the shard's un-offloaded content below `J` with its stamps, and
   every held superseded version with its hold marks ([§2.3](#2.3%20The%20journal%20extension)) — is covered by the
   primary, file by file, either by sending it `Export` or by offloading it; a
   held version is covered only once it is copied or offloaded under its hold, not
   by offloading the file's current content. The live stream began at step 3,
   before any `Export` snapshot is taken, so no operation falls between the two.
   The primary offloads eagerly meanwhile ([§5](#5.%20Offload%20and%20release)), so the tail shrinks on its own.
5. When the primary has seen every file's `Export` acknowledged or the file's tail
   offloaded, it clears the learner flag by compare-and-swap at the next epoch.

![A new node joins](img/rfc10-join.svg)

Writes never pause. A learner's copied tail is applied whatever its committed
point, since the copy reflects the primary's current state, and it does not settle
until its flag is cleared: a copied operation can still arrive under a removal
whose marker settling would drop. A node accepts a copy only while it is a
learner of the shard; once its flag is cleared, a late copy is refused like any
late operation. A learner **MUST NOT** take over, serves no reads, and its
committed point is never compared with others' ([§9.2](#9.2%20Takeover)). It is safe to count at
once because every acknowledged operation below `J` is held by the primary and
every replica that is not a learner, or is offloaded. A takeover before the tail
is covered drops it, and it joins again under the new primary.

`ponytail:` a returning node discards the shard's files and receives the whole
un-offloaded tail again. Rejoin by delta — each file's divergence point, from
which only newer operations are discarded and copied — when flapping nodes make
catch-up show up in repair time.

### 7.4 Repair

Restoring the count after a node is lost is paced by the **repair scheduler**.
It runs under a lease as GC does ([RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix)): partitioned by a hash of the
shard ID, each partition held by one node's lease record with an epoch
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)); when a holder's lease lapses another node takes the
partition, which is the scheduler's only failover. The limits below are the
cluster's, divided evenly among the partitions. It **MUST**:

- order shards by fewest replicas that are not learners first, then by most
  un-offloaded bytes;
- place each new replica in a failure domain the shard does not use;
- limit how many shards join one node at once, and how many joins run in the
  cluster at once;
- be disabled cluster-wide by one setting, for maintenance that would otherwise
  start a storm of joins.

The scheduler only chooses; each join is the primary's procedure of [§7.3](#7.3%20Joining).

## 8. Reads

**The primary** serves reads of the shard from its journal, but only content the
whole replica set holds: a read overlapping an operation still being replicated
waits for it. Otherwise a failover could remove content a client had already
read.

**Every other storage node**, replica or not, follows [RFC 11 §6](rfc-11-ownership.md#6.%20Reads%20on%20other%20nodes): it asks the
primary the newest version of the range the whole replica set holds, and the
answer carries the primary's epoch and node lease expiry. It serves its own bytes
only if they carry exactly that version, the answer's epoch is the one it has
installed for the shard, it has not fenced itself, and its own clock reads before
that expiry less the drift bound; otherwise it forwards the read. The checks are
the asking node's own, since a primary that pauses between checking its lease and
answering sends an answer that is already stale. A learner serves none. A node
never serves un-offloaded content it does not hold.

`ponytail:` a read on a node other than the primary costs one round trip to the
primary. Add read leases when that round trip shows in a replica-read profile
([Appendix B](#Appendix%20B%20%E2%80%94%20alternatives%20considered)).

## 9. Failover

### 9.1 Why one replica suffices

Every acknowledged operation is held by the whole replica set ([§4](#4.%20The%20write%20path)), and every
replica that is not a learner also holds the shard's un-offloaded content from
before it joined. So any one of them holds every acknowledged operation that is
not offloaded, and can take over with no quorum to gather. What it may lack, or
others may hold beyond it, are operations never acknowledged.

### 9.2 Takeover

When the primary's node lease lapses, a replica takes over:

1. **Claim.** The claimant asks the shard's replicas that are not learners for
   their committed points for a configured **gather interval**. A point below the
   mark the primary persisted for that replica ([§7.2](#7.2%20Removal)) is not counted, and its
   replica is dropped; of the rest, the one with the highest point claims. The
   claim is one transaction. It marks the old primary's node record lapsed at the
   node epoch the shard record names, unless that node epoch is already
   superseded, and writes the shard record naming the claimant primary — as
   (node, node epoch, journal identity, incarnation) — at the next epoch, with
   every learner removed. It is conditional on the record being the one it read;
   on listing the claimant — node, journal identity and incarnation — as primary
   or as a replica that is not a learner; and on the old primary's lease having
   lapsed: store time past its expiry plus the drift bound, or its node epoch
   superseded, which implies it ([§3](#3.%20What%20it%20assumes%20of%20shard%20placement) item 3). Replicas it could not reach
   within the interval are dropped too. Every dropped node and every learner
   rejoins as a learner ([§7.3](#7.3%20Joining)). Two simultaneous claimants conflict on the
   record, and one wins.
2. **Install.** It installs the new epoch durably on itself, then on every kept
   replica; each answers with its committed point. From here nothing the old
   primary sends counts. If a kept replica reports a point above the claimant's
   own, the claimant does not seal: it names that replica as primary at the next
   epoch by compare-and-swap, conditional on it being listed and not a learner, and
   that replica runs this procedure from step 2.
3. **Reconcile.** The **baseline** is the highest committed point any kept
   replica reports. The claimant pulls every kept replica's operations above the
   baseline and applies them with `Apply`: with one writer, the highest version
   at each byte is the right one. It sends each kept replica whose point is below
   the baseline the un-offloaded operations it holds above that replica's point.
4. **Re-issue.** Where any operation above the baseline exists — the ranges that
   may or may not have been acknowledged — the claimant re-issues what it now
   holds there under the new epoch and replicates it like any operation: its
   bytes, or a removal where a removal marker is the newest thing it holds. A range
   metadata already covers at or above the uncertain version is never re-issued as
   a removal. Every kept replica then converges on the claimant's view.
5. **Existence.** It re-applies existence for the shard's files it holds —
   `Since(id, applied)` for each ([RFC 1 §3.10](rfc-1-journal.md#3.10%20Settle%20and%20Since)) — writing each file's fence records at
   the new epoch first, in commits that wait until every kept replica durably
   holds the step 4 re-issues. Metadata can lag acknowledged writes ([§4](#4.%20The%20write%20path)), so the
   journal decides, not the recorded size.
6. **Set the committed point.** Once every kept replica has durably acknowledged
   every re-issue — one that has not is removed by compare-and-swap first — and
   step 5's commits have landed, it sets the committed point to the newest version
   it re-issued, or to the baseline if it re-issued nothing, and sends it to every
   replica. Only then does it serve or accept writes, and it starts the shard's
   grace period for open state ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)).

![Takeover](img/rfc10-takeover.svg)

**Holds are inherited.** Every kept replica holds the hold marks durably, and
steps 3 and 4 carry held superseded versions and their marks like un-offloaded
operations, so the new primary offloads them under the hold and the snapshot still
completes ([RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)). While a snapshot record in state `cutting`
exists, the new primary starts the shard's cut gate closed ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)).

A claimant that fails before step 6 leaves nothing to repair: the next claimant
repeats the procedure at a higher epoch, which outranks every partial re-issue,
and its union includes them.

### 9.3 After takeover

The new primary offloads what it holds, which includes everything the old primary
had not; the learners the claim dropped join again under it ([§7.3](#7.3%20Joining)). The old
primary, if it returns, is primary of nothing ([§3](#3.%20What%20it%20assumes%20of%20shard%20placement) item 3) until it claims each shard
again under [§9.2](#9.2%20Takeover), or [§10](#10.%20A%20single%20node) for a shard with no replica, and is fenced by [§6](#6.%20Fencing)
everywhere else.

### 9.4 Handover

A planned move — the primary following the writer, a rebalance or a re-placement —
waits for no lease:

1. the new primary becomes a replica that is not a learner, joining under [§7.3](#7.3%20Joining)
   if it is not one;
2. the old primary stops accepting writes, granting open state and serving primary
   reads for the shard, drains what is in flight, and sends every replica the
   committed point covering it;
3. it writes the record naming the new primary at the next epoch, by
   compare-and-swap, and sends the new primary the shard's open-state table and the
   recent entries of its dedup table ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)). On an unknown outcome it
   re-reads the record before doing anything else, so it never resumes writes
   under a record that has moved;
4. the new primary installs the epoch on every replica, re-applies existence as
   [§9.2](#9.2%20Takeover) step 5 does, sets the committed point to the drained one, installs the
   open-state table, and only then accepts writes and serves open state, with no
   grace period ([RFC 14 §10](rfc-14-open-state.md#10.%20Shard%20placement)). It inherits the shard's holds and gate as
   after a takeover.

No seal is needed: the old primary acknowledged nothing it had not replicated to
the whole replica set, and stopped before the move. It stays a replica unless
removed. If it is lost after step 3 and before the new primary holds the table, the
new primary runs grace as after a failover.

**Moving files between shards** is a handover run in batches ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)). The
receiving shard's primary first raises its own epoch above the giving shard's by an
ordinary record change and installs it on its replicas. The giving primary ships
each batch's un-offloaded operations with `Export` — held superseded versions and
their hold marks included — with the files' open-state and dedup entries, tagged
with the move, the batch and the giving shard's epoch. The receiving primary
accepts a ship only for a batch it has open under that epoch, so a delayed or
duplicated ship is refused. It **re-versions** what it accepts: in the order
exported, it assigns each operation a new version under its own epoch, in the
file's entry under the receiving shard, and replicates it like any write, hold
marks following their versions. Moved content therefore obeys every rule of this
document, and the files' refs follow in metadata, since the receiving epoch
outranks every version the giving shard assigned. A shipped removal marker becomes
a marker at its new version; the giving primary committed that removal before the
freeze drained, so none is committed again. Each batch commits only if both
shards' epochs are still the ones the batch ran under, and only once the
re-versioned content is durable on the receiving primary and every one of its
replicas. If it does not commit, the receiving primary `Discard`s the batch's
files under the receiving shard on itself and every replica before the batch is
shipped again; a replica that does not confirm is removed ([§7.2](#7.2%20Removal)). After a
batch commits, the giving primary sends its replicas a committed point at or above
the batch's newest operation, then tells every node of its replica set, itself
included, to `Discard` the files under the giving shard. A node in both replica
sets, or one that joins the receiving set later, keeps its entry under the
receiving shard ([§2.3](#2.3%20The%20journal%20extension)). A late operation of the giving shard for a moved file is
at or below that committed point, and is refused ([§6](#6.%20Fencing)).

### 9.5 Whole-cluster restart

Each node acquires a new lease once its journals are open, and each shard then
needs one claim ([§9.2](#9.2%20Takeover)), normally by its old primary, whose old node epoch is
now superseded. A node **MAY** claim many shards in one metadata transaction. A
shard with nothing above its baseline re-issues nothing, so its takeover is a
claim, an install and an existence pass; the repair scheduler restores what did
not return within the gather interval ([§7.4](#7.4%20Repair)).

## 10. A single node

On one node a shard's replica set is the primary alone. Replication is a local call,
the committed point is whatever the journal has synced with its removals
recorded, reads are always the primary's, and failover cannot happen. The engine
**MUST** still compose this layer, not the journal directly: with two code paths,
every rule here is exercised by one and not the other, and the untested one ships
to the deployment that needs it.

**A shard with no replica never changes epoch**, including across a restart that
lets its node lease lapse. Nothing else can claim it, so its node, holding a new
lease, re-claims it by a compare-and-swap that changes only the node epoch the
record names — the one record change that raises no epoch. No receiver needs
fencing, and the old process's commits are refused by the guard on its node
record ([§3](#3.%20What%20it%20assumes%20of%20shard%20placement) item 6). Nothing then calls `Apply`, `SetEpoch` or `Discard` until
files move between shards ([§9.4](#9.4%20Handover)), and the journal stays at RFC 1's format
version. It moves to the extension's version on its first extension record, and
an older binary cannot open it afterwards. In a cluster, a shard configured with
no replica is unavailable while its primary is down.

## 11. Worked examples

Notation: nodes A, B, C, N; `v(e,n)` is a version at epoch `e`; `cp` is a
committed point. Each example is a scenario of the simulator's catalogue
([Appendix C](#Appendix%20C%20%E2%80%94%20test%20catalogue)), named with its ID, which replays it exactly.

**(a) A node crashes; a replica takes over; no acknowledged write is lost** —
`S-takeover-basic`. Shard U at epoch 5, primary A, replicas B and C.

| t | primary | replicas | metadata store | client-visible |
| --- | --- | --- | --- | --- |
| 0 | A assigns `v(5,100)` to `f[0,4K)`, sends it with `cp = v(5,99)` | B, C apply, persist `cp 99`, ack | U: e5, A; B, C | — |
| 1 | A holds all acks | — | — | write acknowledged |
| 2 | A crashes | — | A's node lease runs out | writes to U stall |
| 3 | — | B, C gather: both report `cp 99`; B wins the tie by node ID | store time passes A's expiry + drift | — |
| 4 | B claims | — | one txn: A's node record marked lapsed; U e6, primary B; C | — |
| 5 | B installs e6 on itself, then C | C installs e6, reports `cp 99` | — | — |
| 6 | baseline 99; B pulls `v(5,100)` from C (it holds it already) and re-issues it as `v(6,1)` | C applies `v(6,1)`, acks | — | — |
| 7 | B re-applies existence for `f`, sets `cp v(6,1)` | C persists `cp v(6,1)` | existence commit, fenced at e6 | U writable; grace starts; a read of `f` returns t0's bytes |

U is now under-replicated: count 3, two members, at its floor of 2. B offloads
eagerly, and (b) follows.

**(b) A new node replaces it and counts for writes at once** — `S-join-replace`.

| t | primary | replicas | metadata store | client-visible |
| --- | --- | --- | --- | --- |
| 0 | B is primary of U at e6, offloading eagerly | C | U: e6, B; C | writes continue |
| 1 | repair scheduler picks N | N discards anything of U | — | — |
| 2 | B adds N | — | CAS: U e7, B; C, N (learner, `J = v(7,1)`) | — |
| 3 | B installs e7 on C and N | C, N install | — | — |
| 4 | B assigns `v(7,1)` onward, waits for C and N | C, N ack | — | writes acknowledged with N counted |
| 5 | B exports `f`'s un-offloaded tail to N; `g`'s tail is offloaded meanwhile | N applies the copy under its point | offload commits for `g` | — |
| 6 | B has every file covered | — | CAS: U e8, N no longer a learner | U back at full count |

**(c) A paused old primary resumes** — `S-zombie-primary`.

| t | primary | replicas | metadata store | client-visible |
| --- | --- | --- | --- | --- |
| 0 | A (e5, lease expiry `E`) sends `v(5,200)`, begins unlinking `g`, then freezes | B, C have not received `v(5,200)` | `F_x(g)` at e5 | write pending |
| 1 | — | B claims and seals as in (a) | one txn: A's node record marked lapsed at A's node epoch; U e6, primary B; C | — |
| 2 | B serves; a client stats and opens `g` | — | no fence written for `g` (a read) | U writable |
| 3 | A resumes: `v(5,200)` arrives at B and C | both refuse: e5 < e6 | — | — |
| 4 | A's unlink commit for `g` arrives, guarding A's node record | — | refused: the record is marked lapsed, although `F_x(g)` is still e5 | `g` intact |
| 5 | A installs e6, is not listed, discards U's files | — | — | the client's retry reaches B; its request ID was never applied |

**(d) Operations reach a replica out of order** — `S-reorder-replica`. A
assigns `v(5,5)` to `f` at 1 MiB and `v(5,7)` to `f` at 0; `cp` is `v(5,4)`.

| t | primary | replicas | metadata store | client-visible |
| --- | --- | --- | --- | --- |
| 0 | A sends both | B receives `v(5,7)` first and applies it; C has both | — | — |
| 1 | A crashes | B still lacks `v(5,5)` | — | neither acknowledged |
| 2 | — | B claims; baseline `v(5,4)` | CAS: U e6, B; C | — |
| 3 | B's point is still `v(5,4)`, so its ceiling keeps `v(5,7)` out of any offer | — | — | — |
| 4 | B pulls `v(5,5)` from C, then re-issues both | — | — | — |

**(e) A seal over uncertain ranges** — `S-seal-uncertain`. Two ranges of `f`.
`X`: `v(5,110)` acknowledged, offloaded, then evicted by B. `Y`: `v(5,130)` never
acknowledged, held by C only.

| t | primary | replicas | metadata store | client-visible |
| --- | --- | --- | --- | --- |
| 0 | A offloads `v(5,110)` at `X`, sends `MarkDurable(newest = v(5,110))` | B raises its `cp` to `v(5,110)`, evicts `X`; C's notice is lost, `cp 100` | `X` refs `v(5,110)` | `X` acknowledged |
| 1 | A sends `v(5,130)` at `Y`, then crashes | only C receives it | — | `Y` not acknowledged |
| 2 | — | B claims; B reports 110, C reports 100 | CAS: U e6, B; C | — |
| 3 | baseline 110 = max; `X` is not above it, so nothing is re-issued there | B sends C nothing for `X` it no longer holds | `X` still refs `v(5,110)` | `X` intact |
| 4 | B pulls `v(5,130)` from C, re-issues it as `v(6,1)` | C acks | existence for `Y` at e6 | `Y` kept; a retry is answered by its request ID |

**(f) A replica read on a range being written** — `S-replica-read`.

| t | primary | replicas | metadata store | client-visible |
| --- | --- | --- | --- | --- |
| 0 | A holds `f[0,4K)` at `v(5,250)` everywhere | B, C hold `v(5,250)` | — | — |
| 1 | a read of `f[0,4K)` arrives at B; A answers "newest held by all: `v(5,250)`" | B's bytes carry it; B serves | — | reads `v(5,250)` |
| 2 | A assigns `v(5,300)` there | B applies it; C has not yet | — | write pending |
| 3 | a read arrives at B; A answers `v(5,250)` | B holds `v(5,300)`, not 250; forwards | — | A serves `v(5,250)` |
| 4 | C acks; A acknowledges | — | — | write acknowledged |
| 5 | a read arrives at B; A answers `v(5,300)` | B serves | — | reads `v(5,300)` |

**(g) Handover following the writer** — `S-handover`. Writes to U now arrive only
at B, a replica that is not a learner.

| t | primary | replicas | metadata store | client-visible |
| --- | --- | --- | --- | --- |
| 0 | A stops writes, open-state grants and primary reads for U; drains | B, C ack the in-flight operations | — | writes to U pause |
| 1 | A sends `cp v(5,400)` | B, C persist it | — | — |
| 2 | A names B primary; sends open-state table and dedup entries | — | CAS: U e6, primary B; A, C | — |
| 3 | B installs e6 on A and C, re-applies existence, sets `cp v(5,400)`, installs the table | A, C install e6 | existence commits at e6 | writes resume at B; no grace |

**(h) A node rejoins with an empty disk under the same node ID** —
`S-amnesiac-rejoin`. C's device is replaced; it restarts with journal identity
`J2`; U's record lists C with `J1`.

| t | primary | replicas | metadata store | client-visible |
| --- | --- | --- | --- | --- |
| 0 | A sends a batch at e5 | C has no install record for U: refuses `ErrAmnesiac` | U: e5, A; B, C(`J1`) | writes to U stall briefly |
| 1 | A removes C | — | CAS: U e6, A; B | writes resume |
| 2 | the scheduler assigns C | C discards, joins | CAS: U e7, A; B, C(`J2`, learner) | — |
| 3 | — | had A died at t0, C could not have claimed: `J2` matches nothing listed | — | — |

**(i) A primary restored from a VM image** — `S-primary-vm-restore`. Shard U at
epoch 5, primary A at node epoch 3, replicas B and C. A's VM is imaged, memory and
disk, while its journal's generation is 7.

| t | primary | replicas | metadata store | client-visible |
| --- | --- | --- | --- | --- |
| 0 | A restarts: swaps generation 7 → 8 at open, acquires node epoch 4 (swapping 8 → 9), re-claims U | B, C install e6 | generation 9; U e6, A; B, C | — |
| 1 | A assigns `v(6,1)`–`v(6,50)`, sends `cp v(6,45)`; three renewals swap 9 → 12 | B, C hold them, persist `cp 45` | generation 12 | fifty writes acknowledged |
| 2 | A's VM is restored to its image: node epoch 3, generation 7, a lease run out by its own clock | — | — | writes to U stall; A serves nothing |
| 3 | A's renewal is refused, node epoch 3 being superseded; it acquires node epoch 5 and swaps from 7: the store holds 12, the swap loses | — | A's node record at node epoch 5 | — |
| 4 | A is amnesiac for U and serves nothing | B claims: A's node epoch 4 is superseded | one txn: U e7, primary B; C | U writable; the fifty writes intact |
| 5 | — | A discards its entries of U's files and joins | U e8, B; C, A (learner) | — |

Had the image been taken after A's last renewal, its generation would match the store's and its
swap would win. Restored once its lease had lapsed in store time, A would be
primary of nothing and lose the gather to B's and C's higher points ([§9.2](#9.2%20Takeover));
restored within its lease, it would run on until a version it assigned a second
time met `ErrDivergent` ([§6](#6.%20Fencing)).

**(j) A learner is excluded from a takeover** — `S-learner-excluded-takeover`.
Shard U at epoch 7, primary B, replicas C and D, and N, a learner joined at
`J = v(7,1)`.

| t | primary | replicas | metadata store | client-visible |
| --- | --- | --- | --- | --- |
| 0 | B offloads `X` up to `v(7,40)`, sends `MarkDurable` | N and D raise `cp` to 40; C's notice is lost, `cp 30` | — | — |
| 1 | B crashes | — | — | writes to U stall |
| 2 | — | C and D gather; N's point is not asked for. D reports 40 and claims | one txn: B's node record marked lapsed; U e8, primary D; C | — |
| 3 | — | N installs e8, is not listed, discards its entries of U's files | — | — |
| 4 | D seals and serves as in (a) | C converges | existence at e8 | U writable |
| 5 | D adds N again | N discards, joins | U e9, D; C, N (learner, `J = v(9,1)`) | — |


## 12. API surface

Signatures are indicative; the obligations above are normative.

```go
// Replicated is what the engine calls in place of the journal (§10).
type Replicated interface {
	WriteAt(ctx context.Context, file FileID, off int64, p []byte) (Version, error) // §4
	Deallocate(ctx context.Context, file FileID, off, n int64) (Version, error)
	Truncate(ctx context.Context, file FileID, size int64) (Version, error)
	Delete(ctx context.Context, file FileID) (Version, error)
	Sync(ctx context.Context, file FileID) error                               // a client's flush, §4
	ReadAt(ctx context.Context, file FileID, p []byte, off int64) (int, error) // §8
	Durable(ctx context.Context, file FileID, ext []Extent, oldest, newest Version) error // §5
	Hold(ctx context.Context, share ShareID, cut SnapshotCut, marks map[FileID]Version) error // §2.3
	Unhold(ctx context.Context, share ShareID, cut SnapshotCut) error
}

// Peer is one replica as the primary sees it, or the primary as a replica asks
// it. Every call carries the shard and its epoch and is fenced by §6.
type Peer interface {
	Apply(ctx context.Context, s ShardID, e Epoch, ops []Op, committed Version) error // §4
	Copy(ctx context.Context, s ShardID, e Epoch, ops []Op) error                     // §7.3
	MarkDurable(ctx context.Context, s ShardID, e Epoch, file FileID, ext []Extent, oldest, newest Version) error // §5
	Install(ctx context.Context, s ShardID, e Epoch) (committed Version, err error) // §6
	Above(ctx context.Context, s ShardID, e Epoch, v Version) iter.Seq2[Op, error]   // §9.2
	Newest(ctx context.Context, s ShardID, e Epoch, file FileID, r Range) (v Version, epoch Epoch, leaseExpires time.Time, err error) // §8
}

// The shard record is RFC 16's metadata.Shard; each member is a metadata.Replica (§3).

var (
	ErrStaleEpoch      = errors.New("replication: stale epoch")
	ErrNotReplica      = errors.New("replication: not listed")
	ErrAmnesiac        = errors.New("replication: listed, but no install record or a lost generation swap")
	ErrCommitted       = errors.New("replication: at or below the committed point, same digest")
	ErrDivergent       = errors.New("replication: another operation holds this version")
	ErrUnderReplicated = errors.New("replication: below the floor")
	ErrForward         = errors.New("replication: forward to the primary")
)
```

## 13. Invariants

Each invariant is stated where its rule lives; the model checks them as properties
([§15](#15.%20Test%20plan%20and%20benchmarks)).

| # | Invariant | Rule |
| --- | --- | --- |
| R1 | An acknowledged operation is held by the whole replica set until offloaded | [§4](#4.%20The%20write%20path), [§7.3](#7.3%20Joining), [§9.1](#9.1%20Why%20one%20replica%20suffices) |
| R2 | Metadata commits and acknowledgements follow the whole replica set | [§4](#4.%20The%20write%20path) |
| R3 | Receivers refuse older epochs and install newer ones durably first | [§6](#6.%20Fencing) |
| R4 | The committed point never decreases, bounds every refusal and every offer | [§5](#5.%20Offload%20and%20release), [§6](#6.%20Fencing) |
| R5 | A learner counts but never takes over, is compared, serves or settles | [§7.3](#7.3%20Joining) |
| R6 | Reads serve only what the whole replica set holds | [§8](#8.%20Reads) |
| R7 | A claim is one conditional compare-and-swap that also marks the old node record | [§9.2](#9.2%20Takeover) step 1 |
| R8 | A seal re-issues the union above the baseline, removals only where newest | [§9.2](#9.2%20Takeover) steps 3–4 |
| R9 | A new primary serves nothing before its re-issues are held and its point set | [§9.2](#9.2%20Takeover) step 6 |
| R10 | A node not listed discards; one listed but amnesiac refuses | [§6](#6.%20Fencing) |
| R11 | Every commit is fenced by file epoch and by the primary's node record | [§3](#3.%20What%20it%20assumes%20of%20shard%20placement) item 6 |
| R12 | Shards sharing a journal are independent; moved content is re-versioned | [§2.3](#2.3%20The%20journal%20extension), [§9.4](#9.4%20Handover) |
| R13 | The engine reaches the journal only through this layer | [§10](#10.%20A%20single%20node) |
| R14 | Nothing is served or acknowledged from a journal before its generation swap, or after a swap of it loses | [§2.2](#2.2%20One%20journal%20carries%20many%20shards) |

## 14. Observability

Every metric is labelled by shard where it is per shard, and by share.

| Answers | Metric | Type |
| --- | --- | --- |
| committed-point lag: time between the newest assigned version and the committed point | `dittofs_replication_committed_lag_seconds` | gauge |
| time replication adds to an acknowledgement | `dittofs_replication_ack_seconds` | histogram |
| replicas per shard, labelled `learner`; below the floor raises a health condition | `dittofs_replication_replicas` | gauge |
| under-replicated shards, and shards waiting for the repair scheduler | `dittofs_replication_under_replicated`, `dittofs_replication_repair_queue` | gauge |
| refusals, labelled `reason` = `stale_epoch`, `committed`, `divergent`, `not_listed`, `amnesiac` or `pressure` | `dittofs_replication_refusals_total` | counter |
| bytes taken by `Apply`, labelled `outcome` = `applied` or `older` | `dittofs_journal_applied_bytes_total` | counter |
| record changes, labelled `kind` = `takeover`, `handover`, `remove`, `join` or `covered`, and `outcome` = `ok`, `stale` or `unknown` | `dittofs_replication_record_changes_total` | counter |
| takeover time, lease expiry to writable | `dittofs_replication_takeover_seconds` | histogram |
| seal duration, and the bytes it re-issued | `dittofs_replication_seal_seconds`, `dittofs_replication_seal_bytes_total` | histogram, counter |
| join to learner flag cleared, and the tail bytes, labelled `by` = `copy` or `offload` | `dittofs_replication_tail_seconds`, `dittofs_replication_tail_bytes_total` | histogram, counter |
| reads on nodes other than the primary, labelled `result` = `served` or `forwarded` | `dittofs_replication_remote_reads_total` | counter |
| node clock offset from store time; self-fences, labelled `reason` = `lease`, `unreachable` or `clock` | `dittofs_node_clock_offset_seconds`, `dittofs_node_self_fences_total` | gauge, counter |

Logs: every record change at `Info`, with shard, old and new epoch and reason. A
node that self-fences logs at `Error`. Stale-epoch refusals log at `Warn`,
rate-limited per sender.

## 15. Test plan and benchmarks

This layer is a distributed protocol, and no amount of review establishes one.
Conformance rests on three kinds of check, all deterministic or replayable, and
all three are required. The scenarios, coverage assertions, properties and
journal extension checks they run are [Appendix C](#Appendix%20C%20%E2%80%94%20test%20catalogue).

**A model.** Shard records, fencing, joining, takeover and the seal **MUST** be
modelled in a model checker before they are implemented, with the invariants of
[§13](#13.%20Invariants) as properties, and kept in step with this document. **Before
implementation the model MUST confirm two simplifications this document makes**:
that one durable committed point per shard replaces a per-file settled point, and
that no release marker is needed because the committed-point refusal covers every
late operation below a released version.

**Deterministic simulation.** The layer reaches the network, the journals'
storage seam ([RFC 1 §1.3](rfc-1-journal.md#1.3%20It%20is%20testable%20on%20its%20own)), the metadata store, time and randomness only through
interfaces. A simulator drives a cluster in one process from a seed. A failing
seed **MUST** reproduce the failure exactly. It runs in two modes:

- **the scenario catalogue**: scripted, seeded scenarios replayed exactly, one per
  edge case;
- **randomized seed search** over every fault kind at once: message loss, delay,
  duplication and reordering; one-way links; swizzle-clogging (clogging a random
  subset of links one at a time and unclogging them in a different order);
  process pauses; crashes that discard unsynced writes; fsync errors; torn and
  misdirected writes; device loss; a full journal; slow nodes; clock drift within
  and beyond the bound; VM image restores; and a metadata store that stalls, is
  unavailable, or answers a compare-and-swap with an unknown outcome. Seed search
  **MUST** reach every coverage assertion or fail.

**Whole-system fault injection** against real processes, checking the histories
clients observe for linearizability of acknowledged writes.

**Benchmarks**, on three storage nodes on one local network, each the reference
box ([Test tiers](rfc-index.md#Test%20tiers)):

| Benchmark | Measures | Target |
| --- | --- | --- |
| Committed-point lag | p99 under sustained 4 KiB writes, three nodes | ≤ one sync interval + 2 ms |
| Acknowledgement overhead | p50 and p99 write latency against a single node | ≤ one network round trip + one replica sync |
| p99 with one degraded replica | write p99 while one replica's disk is 10× slower, until it is removed | ≤ the lag trigger, then back to the row above |
| Write stall on replica loss | longest acknowledgement gap when a replica dies | ≤ the removal bound + one compare-and-swap |
| Takeover time | lease expiry to writable, 64 MiB uncertain, 10³ files | ≤ drift bound + gather interval + 1 s |
| Seal duration | seconds per uncertain GiB | report; ≤ 1 s for 64 MiB |
| Handover time | shard with 64 MiB un-offloaded | ≤ 2 s at 10 Gb/s |
| Join time | join to counting for writes; join to learner flag cleared | ≤ one metadata transaction + one round trip; report, tail ≥ 80% of the link |
| Whole-cluster restart | all node leases lapsed to 10⁴ shards writable | report ([§16](#16.%20Open%20questions) item 3) |

## 16. Open questions

1. **The seal's cost.** Pulling the kept replicas' operations above the baseline
   is proportional to the committed point's lag, which is unmeasured.
2. **Journal capacity** is multiplied by the replica count for un-offloaded
   content, on journals shared by many shards. Whether offload keeps that bounded
   under sustained writes, and during repair, is unmeasured.
3. **Whole-cluster restart** now costs one lease per node, but still one claim per
   shard. Whether batched claims make 10⁴ shards writable fast enough is unmeasured.
4. **Repair pacing defaults** — how many joins per node and per cluster, the
   gather interval, the lag trigger — have no measured values yet.

Moves between shards and protocol state are [RFC 11](rfc-11-ownership.md)'s; per-file and range shards
are deferred ([RFC 11 Appendix C](rfc-11-ownership.md#Appendix%20C%20%E2%80%94%20later%3A%20per-file%20and%20range%20shards)).

---

## Appendix A — prior art

Every system below that keeps consensus off the data path does it this way: one
writer orders each shard, a service that already runs consensus holds the replica
set and an epoch, and every message is fenced by it.

| System | What this design takes from it |
| --- | --- |
| PacificA (MSR-TR-2008-25) | replica set kept apart from data; commit at every replica; reconcile before serving |
| Azure Storage stream layer (SOSP 2011) | all-replica acknowledgement; seal a failed writer, continue under a new set, repair in the background |
| BookKeeper ensemble change | swap the failed node and continue from the first unacknowledged entry — a replica counting from its join point |
| Kafka KIP-101, KIP-405, KIP-966 | truncation by epoch; copy only the local tail; takeover eligibility apart from acknowledgement, as learners are |
| 3FS | a syncing target takes writes before reads; stop after half a lease without the manager |
| Chain replication (OSDI 2004), CRAQ (ATC 2009) | a read away from the writer confirmed against its newest committed version |
| Aurora (SIGMOD 2017, 2018) | one writer's versions make replica acknowledgements consistent |
| GFS (SOSP 2003), HDFS lease recovery | replica-set version bumped before a new writer writes; stale replicas detected by version |
| Ceph peering | reconcile and record before serving; recover the most degraded first |
| Frangipani (SOSP 1997) | a lease checked only by the sender is not a fence |
| Assise (OSDI 2020) | a local log replicated before acknowledgement, published asynchronously |
| FoundationDB (SIGMOD 2021), TigerBeetle, Antithesis | deterministic simulation; swizzle-clogging; a storage fault model; "sometimes" assertions |

## Appendix B — alternatives considered

| Alternative | Why not |
| --- | --- |
| Every storage node writes, and journals order operations differently | Two writers of one range leave journals holding different bytes with no version comparable across them. Ordering needs one writer per shard or consensus per write. |
| Consensus per write through the metadata store's timestamp service | Moves consensus from rare events to every write. Reads of recent data then need a quorum or a metadata lookup, several nodes offload overlapping content, and `size` becomes contended. It buys nothing over forwarding to the primary, which costs the same hop replication already pays. |
| Acknowledge at a quorum of the replica set | Hides a slow replica, but a replica can then miss acknowledged writes, so failover needs a read quorum and a durable truncation record. A slow replica is removed on lag instead ([§7.2](#7.2%20Removal)). |
| Read leases for replicas | Saves one round trip per read on a node other than the primary, but costs a lease type, clean/dirty range tracking, a wait of the longest read lease on every removal and takeover, and a second clock assumption that a drifting clock breaks. Removed; the round trip needs no range tracking and no wait at a takeover ([§8](#8.%20Reads)). |
| Promote a joining node only after it catches up | Needs a barrier that stops the shard's writes while the joining node confirms everything, and leaves a shard at its floor unwritable for the whole catch-up. Counting from a join point needs no pause, and offload covers the tail ([§7.3](#7.3%20Joining)). |
| A lease per shard | 10⁴ renewals where one per node does, and 10⁴ lapsed leases after a whole-cluster restart. One node lease fences all of a node's shards at once. |
| Rejoin by delta | A returning node could keep what it holds up to each file's divergence point and receive only newer operations. Deferred: it needs per-file epoch history ([§7.3](#7.3%20Joining)). |
| A seal from the claimant's view alone | Re-issues a removal wherever the claimant holds nothing, including content it evicted after offload: an acknowledged write is destroyed. The seal takes the union above the highest committed point ([§9.2](#9.2%20Takeover)). |
| Replicas read another's writes only after offload | Metadata shows a new size at once; reading the un-offloaded range elsewhere must then wait for offload or ask the primary anyway. It also breaks close-to-open visibility unless close waits for offload. |
| Erasure-coded journal content | Journal content is small, overwritten and short-lived: stripes need read-modify-write on partial writes, failover must reconstruct, and no replica holds whole data to serve reads. Erasure coding belongs at rest, where blocks are sealed. |
| A shared journal on storage every node can reach, or a replicated log service | Either requires shared-access storage or another service to deploy and keep healthy. Replication among storage nodes needs neither. |
| Journal in a key-value store or an in-memory cache | Weaker durability or capacity, write amplification on large values, and the journal's semantics rebuilt on top. |

## Appendix C — test catalogue

**Scenarios.** `S-takeover-basic`, `S-join-replace`, `S-zombie-primary`,
`S-reorder-replica`, `S-seal-uncertain`, `S-replica-read`, `S-handover`,
`S-amnesiac-rejoin`, `S-primary-vm-restore` and `S-learner-excluded-takeover` are
[§11](#11.%20Worked%20examples) (a)–(j). The rest:

| ID | Scenario |
| --- | --- |
| `S-stale-commit-node-guard` | a paused primary's existence commit for a file the new primary never touched is in flight while the claim commits; it aborts on the node record, and one started after the claim is refused |
| `S-restart-own-shard` | a primary restarts and acquires a new node epoch before any replica claims; it claims its own shard at once |
| `S-seal-of-seal` | a claimant dies mid-seal; the next claimant's union includes its partial re-issues |
| `S-claimant-crash-<step>` | the claimant crashes after each of [§9.2](#9.2%20Takeover)'s six steps, and after each replica's install |
| `S-simultaneous-claims` | two replicas claim at once; one wins, the other installs and stays a replica |
| `S-cas-unknown-<kind>` | a claim, join, removal, learner clear and handover each get an unknown outcome, once landed and once not |
| `S-partition-primary-store` | the primary reaches its replicas but not the store: it self-fences at half its lease; a replica claims |
| `S-partition-primary-replica` | the primary reaches the store but not a replica: it removes the replica; the replica cannot claim |
| `S-removal-vs-takeover` | a primary's removal and a replica's claim race on one record |
| `S-learner-orphaned` | the primary dies before a learner's tail is covered; the claim drops the learner, which discards and joins again |
| `S-join-during-takeover` | a join and a claim race; the loser re-reads |
| `S-move-during-failover` | files moving between shards when either shard's primary fails mid-batch |
| `S-replica-journal-full` | one replica's shared journal fills: backpressure first, then rate-limited removals across its shards |
| `S-gray-replica` | a replica answers but slowly; it is removed on lag, and acknowledgement latency recovers |
| `S-fsync-error` | a replica's sync fails; it must not acknowledge ([RFC 1 §6.3](rfc-1-journal.md#6.3%20A%20failed%20sync)) |
| `S-torn-write` | a torn record on a replica is found at recovery; the replica reports a lower point and is removed |
| `S-disk-loss` | a replica's device is lost while it is primary of some shards and replica of others |
| `S-rollback-replica` | a replica's journal is restored from an older copy with the same identity: amnesiac by its lost generation swap at open, lease acquisition or renewal; a torn one is removed by its mark |
| `S-clock-drift-beyond-bound` | one node's clock drifts past the bound: it self-fences on renewal; durability holds regardless |
| `S-store-stall` | the metadata store stalls for longer than half a lease, then returns |
| `S-store-unavailable` | the metadata store is down: every write stops, nothing acknowledged is lost |
| `S-restart-10k-staggered` | every node restarts, returning over minutes, with 10⁴ shards |
| `S-zombie-node` | a node paused past its lease, whose shards were all claimed, resumes and sends to every one |
| `S-dup-reorder-epochs` | duplicated and reordered operations from two epochs reach one replica |
| `S-release-late-duplicate` | an operation below a released version arrives late after eviction |

**Coverage.** Every run reports which branches it reached; seed search **MUST**
reach each of: a seal re-issued a range; a union pulled an operation the claimant
lacked; a claim was handed to a replica with a higher point; a replica was removed
for silence, for lag and for pressure; pressure was answered by backpressure; a
node rejoined as a learner; a tail was covered by copy and by offload;
`ErrAmnesiac` was returned for a missing install record and for a generation swap
lost at open, at lease acquisition and at renewal; `ErrDivergent` was returned to a waiting
primary; a learner was dropped by a claim; a commit in flight was aborted by the
node-record guard; an operation was refused at or below the committed point; a
replica was dropped from a gather by its persisted mark; an unknown
compare-and-swap outcome was resolved by re-reading.

**Properties** every check asserts:

| Property | Invariants | Violated by |
| --- | --- | --- |
| An acknowledged write is held by a journal of the current replica set, or durable remotely, at every moment | R1, R8 | a quorum acknowledgement, a seal from the claimant's view alone, a learner claiming |
| No read returns content older than the newest acknowledged write, or content a seal later removed | R6, R9, R14 | a node serving without asking the primary, a primary serving unreplicated content, a restored primary serving its image |
| Removed content is never served again | R4, R10 | a late duplicate below the committed point, a rejoining copy that did not discard |
| Nothing a superseded primary sends or commits takes effect after the claim | R3, R11 | a sender-side lease check standing in for receiver fencing, a stale commit on a file the new primary never touched, a commit fenced by a store time compared before the store fixes when the commit lands |
| A node without the shard's data never claims it or counts for it | R5, R7, R10, R14 | a claim by node ID alone, a wiped or restored device rejoining silently, a learner compared in a gather, a refusal counted as held without its digest |
| A shard with one replica that is not a learner alive becomes writable without operator action | R7, R9 | a takeover that waits for a quorum |
| One shard's install, discard, lag or pressure never changes or refuses another shard's files | R12 | a discard by journal, one share's backlog refusing another's writes |
| Nothing is offered above the committed point | R4 | an offer ahead of an operation still in flight |

**Journal extension checks**, run against the journal alone as [RFC 1 §11](rfc-1-journal.md#11.%20Conformance) runs
its own:

| Checks | How |
| --- | --- |
| order does not matter | Apply one random set of writes, deallocates, truncates and a delete with distinct versions to fresh journals in many orders, with repetitions; assert identical bytes and versions, before and after a crash and reopen, and identical to version order. |
| no resurrection | Apply a write at v2, a deallocate at v3 over it, then a write at v1; assert the range reads `missing`, and still does after reopen. |
| ceiling holds | Apply v2 at A and v4 at B; offer with ceiling v3; assert only A is offered. Raise it to v4; assert both are. |
| refusal below the point | Persist a committed point of v5, settle, crash and reopen; apply a write at v4 through this layer; assert `ErrCommitted` and that the journal is unchanged. |
| export skips offloaded | Export a file with offloaded extents, dirty content and removal markers into a fresh journal; assert it holds the dirty content and markers at their versions, and none of the offloaded extents. |
| epoch outranks | Assign, raise the epoch, apply an old-epoch operation with a larger counter; assert it loses. Crash between the epoch record and the first assignment; assert the next version is under the raised epoch. |
| discard is final | Apply content, `Discard`, crash and reopen; assert nothing is held and a `Fill` begun before the discard is refused. Repeat during an offer. |
| epoch records retire | Write, offload, release and settle every extent of a file; repack every segment; assert its epoch record is gone and the next write after `SetEpoch` is under the raised epoch. |
| format upgrade | Open a journal of RFC 1's version, write, then `Apply`; assert it reopens under the extension's version and an RFC 1 binary refuses it. |
| generation swap | Open, crash between the `format` sync and the swap, reopen; assert the journal is not amnesiac. Copy the journal aside, open and close it again, restore the copy; assert its swap loses and it refuses as amnesiac. Copy a serving journal aside, let one renewal pass, restore the copy under the running node; assert the next renewal still extends the lease, that swap loses, and the node stops serving from that journal alone. |
