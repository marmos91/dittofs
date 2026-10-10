---
rfc: 11
title: "RFC 11 — shards"
component: shards
status: deferred
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-10-journal-replication]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-28-journal-format]]"
aliases:
  - RFC 11
tags:
  - rfc
---
# RFC 11 — shards

**Status:** deferred. [§15](#15.%20Open%20questions) lists what is known to be undecided.
**Audience:** anyone designing how several nodes serve one share.

> [!note] Built after the single-node release
> This design is not shelved. The first release is a single node scaled
> vertically; horizontal scaling is the phase after it, needed for large
> contracts, for large shares spread over several nodes and, later, for
> NFSv4.2 and pNFS. Every rule here is a cluster rule except the parts marked
> **Release 1**, which bind the single node now and which
> [RFC 0's single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)
> cites: one journal device per shard ([§2.1](#2.1%20One%20primary%20per%20shard)) and the
> fence records with their point-record conflict rule ([§8](#8.%20Metadata%20consistency)).
> The profile says what replaces the rest on one node.
> Until the cluster is built only these hooks are implemented:
>
> - the 128-bit content version, with its epoch half held at zero
>   ([RFC 1 §4.3](rfc-1-journal.md#4.3%20Versions));
> - the node epoch and the shard incarnation carried in the NFS write verifier
>   ([§7](#7.%20Protocol%20state), rule 3);
> - one binary, with roles chosen by configuration ([RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20binary%2C%20roles%20chosen%20at%20deployment)).
>
> Adding nodes migrates each journal's format one way, behind a gate
> ([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)).

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

**What this is.** In a cluster, every file has exactly one server allowed to
change it at any moment. This RFC groups files into shards, says which server is
in charge of each shard, how a request arriving at any server reaches it, and
how that charge moves: after a crash, in a rebalance, or to follow the client
doing the writing.

**The pieces.**

- A **node** is one running DittoFS server process, with one or both of two
  roles. A `protocol` node holds client connections (SMB, NFS): it translates
  each call and, when it is not the file's primary, forwards it — for that call
  it is the **front-end** — and keeps no file state of its own. A `storage`
  node holds **journals** — logs of recent writes on a local disk — and does
  the work. A single-node install runs both roles in one process.
- A **shard** is a set of files that move together. By default a whole share is
  one shard. A directory can be marked so that each child directory created in
  it gets its own: with `profiles/` marked, `profiles/alice/` is shard A and
  `profiles/bob/` is shard B.
- The **primary** of a shard is the one storage node that owns it right now. It
  orders every write and holds every open, lock and lease (caching grant) on the
  shard's files; reads go to it, or are checked with it first ([§6](#6.%20Reads%20on%20other%20nodes)).
  The shard's replicas hold copies of its journal, ready to take over
  ([RFC 10](rfc-10-journal-replication.md)).

**One write, followed.**

```text
   alice-pc (SMB)                      build01 (NFS)
        │                                   │
   ┌────▼────┐                         ┌────▼────┐   protocol nodes: hold
   │   P1    │                         │   P2    │   connections, translate,
   └────┬────┘                         └─────────┘   forward
        │ 1  ODFC_alice.vhdx → shard A → primary S1
        ▼
   ┌─────────────┐   ┌─────────────┐   ┌─────────────┐   storage nodes:
   │     S1      │   │     S2      │   │     S3      │   hold journals,
   │ primary   A │   │ primary   B │   │ primary   C │   do the work
   │ replica B,C │   │ replica A,C │   │ replica A,B │
   └──────┬──────┘   └──────▲──────┘   └──────▲──────┘
          │ 2  journal it, copy to A's replicas │
          └─────────────────┴───────────────────┘
          3  S2 and S3 hold it → OK → P1 → alice-pc

   shard A = profiles/alice/   shard B = profiles/bob/   shard C = builds
```

1. alice-pc is connected to P1 and writes 64 KiB into
   `profiles/alice/ODFC_alice.vhdx`.
2. P1 looks up the file's shard (A) and A's primary (S1) in its cache of shard
   records, and forwards the write, stamped with the shard and epoch it expects.
3. S1 checks it is still A's primary at that epoch, checks alice's open and the
   locks others hold, stages the write in its journal and copies it to S2 and
   S3 ([RFC 10](rfc-10-journal-replication.md)).
4. When all three hold it, S1 answers OK, and P1 passes it to alice-pc.

Had P1's cache been stale — A moved to S2 a moment ago — S1 would refuse,
naming the primary it knows, and P1 would re-read the record and retry. A
stale cache costs a retry, never a write in the wrong place.

**When the primary changes: the epoch.** Each shard's record in the metadata
store carries an **epoch**, a fencing number raised by a change of primary or
replicas and by the raise before a move
([RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms) states the rule).

- *S1 crashes.* S1 renews one lease for all its shards every 3 s, valid for
  10 s (the proposed defaults). Once it has lapsed, plus 0.5 s allowed for clock drift, S2 — a replica —
  takes A over: one metadata-store transaction names S2 primary and raises A's
  epoch from 5 to 6 and its incarnation. Writes to A stall for up to the lease,
  the drift allowance and the takeover's own work — about 10.5 s plus a few
  seconds — then resume at S2. The
  node serving A changed, so the NFS **write verifier** for A's files changes —
  the signal that makes an NFS client resend writes it had not yet flushed — and
  clients reclaim their opens and locks during a grace period; alice-pc
  reclaims its handle on the container. If S1 had only paused and then wakes up,
  everything it sends carries epoch 5: the replicas refuse it, and the metadata
  store refuses its commits, because the takeover marked its lease lapsed.
- *A move started and called off.* Before a batch of files moves into shard A,
  A's epoch is raised above that of the shard they come from — say from 6 to 7.
  If the move then goes no further, no node changed: S2 still serves A. The
  write verifier is derived from the serving node and the shard's incarnation,
  which only a change of serving primary raises, not from the epoch, so it stays
  the same and no client resends anything.

**Moving a shard moves no bytes in the remote tier.** Uploaded content lives in
the bucket and is described in the metadata store, and every storage node
reaches both. A handover ships only the shard's journal content not yet
uploaded, with its open-state table; a rebalance moves primaries and replicas,
never files.

**How a very big share scales.** A share is one shard by default, so one node,
its primary, orders all its writes, opens and locks: any number of clients can
write through any protocol node, but the share gets at most one node's
throughput — and, since a shard's journal content lives on one journal device of
that node, at most one device's write bandwidth, on a single node too — and
replicas add durability, not write throughput
([§1.1](#1.1%20What%20scaling%20is%20being%20designed%20for), [§2.1](#2.1%20One%20primary%20per%20shard)). Marking a directory
gives each child directory created in it its own shard, and moves existing
children over in batches; each such shard hashes to one of 4096 fixed slots,
and a slot table assigns slots to storage nodes in proportion to their
capacity, so alice's and bob's shards land on different nodes
([§2.2](#2.2%20Automatic%20per-child%20shards)). An operator can also split chosen subtrees by hand.
One shard stays the unit of parallelism: a directory's entries are never split
across shards and neither is a file, so one huge flat directory or one hot file
— alice's 30 GB container — is still bounded by one node; per-file and range
shards are deferred ([Appendix C](#Appendix%20C%20%E2%80%94%20later%3A%20per-file%20and%20range%20shards)). Reads spread further:
any storage node may serve an extent, from its journal or by fetching from the
bucket, after one round trip asking the primary for the newest version, which
carries no bytes ([§6](#6.%20Reads%20on%20other%20nodes)). Placing whole shares on separate nodes
behind a balancer, without shards that move, is an open question, and its
proposed size cap does not fit profile shares ([§15](#15.%20Open%20questions)).

**The words you need.**

- **node**, **role** — one server process / what it runs, `protocol` or
  `storage` ([glossary](rfc-0-data-lifecycle.md#Glossary)).
- **shard** — a set of files with one primary at a time; a share by default,
  a subtree, or a child of a marked directory ([§2](#2.%20Shards)).
- **primary** — the storage node that runs a shard's writes, open state and
  locks now ([§3](#3.%20The%20primary)).
- **replica** — a storage node holding a copy of the shard's journal content,
  ready to take over ([RFC 10](rfc-10-journal-replication.md)).
- **epoch** — the shard record's fencing number; what raises it is
  [RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms)'s, and every receiver refuses an older
  one ([§3.1](#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).
- **shard incarnation** — the record's second number, raised whenever a node
  begins serving the shard as primary; the write verifier follows it
  ([§3.1](#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).
- **node lease** — the one lease each storage node renews for all its shards; a
  node whose lease lapsed is primary of nothing ([§3.1](#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).
- **slot table** — the record that assigns each of a fixed number of slots to
  nodes, placing per-child shards ([§2.2](#2.2%20Automatic%20per-child%20shards)).

**What this RFC promises.**

- At most one node's writes to any byte count at a time: a replaced primary's
  writes and commits are refused, and it stops granting opens and locks by
  itself before its lease runs out.
- A file's shard is set when it is created and never changes on rename; only a
  batched move changes it, and between batches every file is in exactly one
  shard.
- A stale route costs a refusal and a retry, never a wrong write, and a retried
  write is applied once.
- A failover runs a grace period for open state; a planned handover or move
  hands open state over and runs none. The write verifier changes when the node
  serving a shard changes, and not when only the epoch is raised.
- No planned change moves a shard's primary sooner than the dwell time
  (`shard.dwell`, proposal: 30 min) after the last one, so two writers cannot bounce it back
  and forth.

**How the rest is organised.** §1 states the three shapes of scaling and the
non-goals. §2 defines shards and their policies, §3 the primary, its lease and
when it follows the writer, §4 how primaries and files move. §5 is routing, §6
reads on other nodes, §7 protocol state. §8 is how the metadata store fences
commits, with operations spanning shards in §8.1; §5.2 (pNFS) and the backend
notes in §8 can be skipped on a first read. §9 tabulates failures and §10 walks
through them step by step. §11–§15 are API, invariants, metrics, tests and open
questions.

## 1. Purpose

A **node** is one DittoFS server process, whatever roles it runs ([RFC 15 §2](rfc-15-topology.md#2.%20Roles)).
Only nodes with the `storage` role hold journals, so only they can be a primary
or a replica. There are no metadata-only DittoFS nodes: the metadata store is
either embedded in a storage node or an external replicated store
([RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20binary%2C%20roles%20chosen%20at%20deployment)).

On one node, every write to a share goes through one engine and one journal, and
the order of writes is the order that journal appends them. A write is
acknowledged once the journal holds it, and until it is offloaded the journals
of its replica set are the only copies ([RFC 10 §1](rfc-10-journal-replication.md#1.%20Purpose)). So with several nodes serving one share, one node
must order the writes to any given bytes, and every other node must know which
one. Designs that run stateless servers over a shared transactional store have
no such node, because they acknowledge nothing that the store does not already
hold. This design has one, and this document says how it is chosen, found and
replaced:

> **Which node may write these bytes now, how does a request reach it, and how
> does anyone else read them?**

**Terms.** *Share*, *node*, *shard*, *primary*, *replica* and *epoch* are
defined once for every RFC in the [RFC 0 glossary](rfc-0-data-lifecycle.md#Glossary). Read in this order
they define one another without a loop: a **shard** is a set of files the shard
policy groups ([§2](#2.%20Shards)), with no reference to who serves it; its **shard record**
is one store record per shard; the **primary** is the storage node that record
names; the **epoch** and **shard incarnation** are numbers in that record; a
**replica** is a storage node the record lists besides the primary. This
document adds:

| Term | Means |
| --- | --- |
| **shard record** | a shard's entry in the metadata store, one per shard: the node it names as primary, as (node, node epoch, journal identity, join incarnation); its epoch and its shard incarnation; its replicas, replica count and floor ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities)). It changes only by compare-and-swap |
| **fence records** | two records per file, `F_x` and `F_o`, each holding the (shard, epoch) the file's commits must carry ([§8](#8.%20Metadata%20consistency)) |
| **front-end** | the node a client's call arrives at; when it is not the primary, it forwards the call there ([§5.1](#5.1%20Front-ends%20forward%20to%20the%20primary)), as the [RFC 0 glossary](rfc-0-data-lifecycle.md#Glossary) defines it |
| **handover** | a planned change of a shard's primary, with no lease wait and no grace period ([RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover)) |
| **move** | a change of the shard some files belong to, run in batches ([§4](#4.%20Moving%20files%20and%20primaries)) |
| **re-placement delay** | how long a shard stays on the replica that took it over before it is moved to the node its slot names ([§2.2](#2.2%20Automatic%20per-child%20shards)) |

*Learner*, *node lease*, *drift bound* and *committed point* are [RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms)'s.
A primary is always a node, never a user. Where this document says *owner* it
means a file's owner — its uid or SID — and nothing else.

### 1.1 What scaling is being designed for

A deployment in the target range holds petabytes across tens of thousands of
shares and hundreds of thousands of users. Three shapes of multi-node system were
considered:

| | A. active-passive | B. shares spread across nodes | C. many nodes per share |
| --- | --- | --- | --- |
| Gives | survival of node loss | aggregate throughput across shares | throughput of one share beyond one node |
| Primary of a share | one node; a replica takes over | one node per share | one per shard, finer than a share |
| Limit | one node's throughput in total | one node per share | one node per shard |

A share is **one shard by default**, which gives B, and B with failover gives A.
Subtree and per-child shards give C for a share one node cannot carry
([§2](#2.%20Shards)). One node is any of them with one storage node. The same mechanism
serves all of them, so no deployment runs a code path another does not.

### 1.2 Non-goals

This document **MUST NOT**:

- replicate journal content or define how a shard's content survives failover —
  [RFC 10](rfc-10-journal-replication.md);
- define client-visible open state and locking — [RFC 14](rfc-14-open-state.md). Shard records are
  internal and **MUST NOT** be exposed as, or stored with, client state
  ([§7](#7.%20Protocol%20state));
- decide which nodes may be primaries — [RFC 15](rfc-15-topology.md)'s roles;
- let more than one node write the same bytes at once
  ([RFC 10 Appendix B](rfc-10-journal-replication.md#Appendix%20B%20%E2%80%94%20alternatives%20considered));
- split one file across shards, or give one file its own shard —
  [Appendix C](#Appendix%20C%20%E2%80%94%20later%3A%20per-file%20and%20range%20shards) sketches both for later;
- split one directory's entries across shards;
- split the metadata store into partitions with no transaction spanning them.
  The store is **one logical store with transactions across any keys**; a
  backend scales it horizontally underneath, and existence, release, batched
  moves and cross-shard operations each commit one transaction over records of
  several shards ([§4](#4.%20Moving%20files%20and%20primaries), [§8.1](#8.1%20Operations%20across%20shards), [Appendix B](#Appendix%20B%20%E2%80%94%20alternatives%20considered)).

> ponytail: one directory's entries live in one shard, so a single directory
> takes creates and lookups-with-open-state at one node's rate. Upgrade to a
> policy that hashes one directory's names across several shards when a
> measured workload — a flat ingest directory of 10^7 files — is capped by it.

## 2. Shards

A **shard** is a set of files with one primary at a time. Every file belongs to
one shard, recorded with the file ([RFC 7 §2.1](rfc-7-namespace-metadata.md#2.1%20File)), and the shard record names
its primary, epoch and replicas.

| Policy | A file's shard is | When |
| --- | --- | --- |
| per share | the share's single shard | the default |
| subtree | inherited from its parent at create; directories chosen by an operator start a new shard | a share one node cannot carry, split by hand |
| per child | each child directory created in a marked directory starts its own shard, placed by slot ([§2.2](#2.2%20Automatic%20per-child%20shards)) | a share of many independent trees, such as home directories |

![A share split into shards](img/rfc11-shards.svg)

**Shard records MUST NOT be per file.** The metadata store holds one shard record
per share, per chosen subtree and per child of a marked directory. Shard records
therefore grow with shares and directories, never with files.

**A shard belongs to the file, not the path.** A file's shard is set when it is
created and changes only by a move ([§4](#4.%20Moving%20files%20and%20primaries)). Rename **MUST NOT** change it,
and hard links do not split it. A change of shard changes which journals hold the
file's un-offloaded content, so it is never a side effect of rename; a policy that
wants a renamed tree to follow its new parent does so by a move.

### 2.1 One primary per shard

A shard has **one primary**. The primary serialises everything that acts on the
shard's files: namespace writes, open state ([RFC 14](rfc-14-open-state.md)), layouts, journal
appends, existence, offload, removals, releases and clone. Its epoch fences every
one of them ([§8](#8.%20Metadata%20consistency)). Because one primary holds both the open-state table and
the journal, a conflict check and the I/O it admits run in one process under one
epoch: no check goes stale between them, and no I/O pays a round trip to be
checked.

**Many writers, one shard.** One primary does not mean one client. Any number of
clients write to a share at once, each through whichever node it is connected to.
Every write to a shard is forwarded to its primary ([§5.1](#5.1%20Front-ends%20forward%20to%20the%20primary)), which orders and
journals them all. What one primary caps is throughput: a shard gets at most one
node's worth. A share that needs more is split into subtree or per-child shards,
each with its own primary on its own node. [§10](#10.%20Worked%20examples) (a) walks through both.

**One shard, one journal device.** A member holds a shard in exactly one of its
journals, and a node keeps one journal per device ([RFC 10 §2.2](rfc-10-journal-replication.md#2.2%20One%20journal%20carries%20many%20shards)), so a
shard's write bandwidth and un-offloaded capacity are capped at one device of its
primary, not at the node. **Release 1:** this binds the single node too, and
[RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) cites it: a share that is one
shard writes at one journal device's rate however many devices the node has. A
node with several devices spreads load only by placing different shards on
different journals; a share that needs more than one device is split into
shards as above.

> ponytail: a shard lives in one journal per member, so one shard's writes are
> capped at one device's bandwidth and its un-offloaded content at one device's
> capacity. Upgrade to striping a shard's files across a member's journals,
> each file still in one journal, when one shard on a multi-device node is
> measured device-bound while its other devices idle.

> ponytail: one primary per shard caps a shard's metadata operations and its data
> bandwidth at one node together. The upgrade is two primaries per shard — one for
> the namespace, one for data — each with its own epoch, and handoffs for release,
> truncate, the size overlay, `LAYOUTCOMMIT` and the I/O conflict check. Take it
> when measurement shows metadata and data work contending on one primary.

### 2.2 Automatic per-child shards

A directory **MAY** be marked so that every child directory **created** in it
starts a new shard. Files created directly in the marked directory stay in its
shard, and a directory renamed into it keeps its own.

- **Placement.** A per-child shard's ID hashes into one of a fixed number of
  **slots** — the per-child shard slot count setting (proposal: 4096), fixed when
  the installation is created and never changed after. The **slot table**, a record in the metadata store
  ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), assigns each slot an ordered list of nodes from the
  **placement set** — the storage nodes that joined and were not decommissioned,
  less any absent for longer than the re-placement delay — the first as primary,
  the rest as replicas. Each node receives slots in proportion to its
  **capacity**, a node setting ([RFC 13 §3](rfc-13-configuration.md#3.%20Scopes)). **A slot's nodes MUST lie in
  distinct failure domains** — host, rack or zone, each node's domain being a node
  setting — whenever the placement set spans as many domains as the replica
  count; the assignment weights by capacity only among placements that satisfy
  that. With fewer domains than the count, the table places as many distinct
  domains as exist and reports the slots that share one as a health condition.
  Without the rule, a capacity-weighted table puts a slot's primary and its
  replicas behind one rack switch, and one rack loss loses every copy. A node that merely misses a lease
  renewal stays in the placement set, and the table is changed only by
  compare-and-swap. The slot's nodes are a proposal: they are written into the
  shard record by the same compare-and-swap as any other change, so fencing
  never depends on the table.
- **Failover now, re-placement later.** When a primary's node lease lapses, a
  replica takes the shard over at once ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover)). Only after the
  **re-placement delay** — `shard.replace_delay` (proposal: 10 min) — is the shard handed
  over ([§4](#4.%20Moving%20files%20and%20primaries)) to the node its slot names, and only once the dwell time has
  passed since its last planned change of primary ([§3.3](#3.3%20The%20primary%20follows%20the%20writer)). A node that returns
  within the delay therefore costs one failover and nothing more.
- **One placement.** The slot table is the only placement of a per-child shard's
  replicas. The repair scheduler ([RFC 10 §7.4](rfc-10-journal-replication.md#7.4%20Repair)), restoring a shard's
  count after a node is lost, **MUST** join the node the table would name in the
  lost node's place were it to leave the placement set — the slot's next node by
  the same capacity- and domain-weighted assignment — and a recomputation that
  removes the lost node **MUST** name, in its place, a node already serving the
  slot's shards wherever the domain rule allows. Without it the repair and the
  later recomputation each pick a node, and a lost node costs every shard two
  joins and two copies of its tail.
- **Rebalance.** When the placement set or a node's capacity changes, the slot
  table is recomputed to move as few slots as the new weights allow, and written
  by one compare-and-swap. Only the shards of slots whose nodes changed are
  handed over ([§4](#4.%20Moving%20files%20and%20primaries)), each under the ordinary handover rules, at a configured
  rate, under the same dwell, and — for a node that left — after the
  re-placement delay. A shard's slot never changes, so a rebalance moves primaries
  and replicas, never files.
- **Marking an existing directory** moves each existing child tree into a new
  shard by a batched move ([§4](#4.%20Moving%20files%20and%20primaries)). Before walking a child tree, the move writes
  the new shard onto that tree's root, so files created in it during the walk are
  born in the new shard. The walk stops at nested shard boundaries, and a batch
  skips any file whose recorded shard is no longer the giving one, so a file
  hard-linked into two trees moves once. Each (giving, receiving) pair has its own
  move record, so child trees move in parallel. A move costs about three store
  writes per file: its recorded shard and its two fence records ([§4](#4.%20Moving%20files%20and%20primaries) step 3). Marking a directory in a shard that a live subtree snapshot
  covers **MUST** be refused with `ErrSubtreeSnapshot` until those snapshots are
  deleted, because it moves files ([§4](#4.%20Moving%20files%20and%20primaries)).
- **Cost.** One shard record per child of a marked directory, and one slot
  table per installation, whose size is fixed by the slot count, not by nodes or
  shards. A rebalance touches the shards of the moved slots only.

**No client operation writes a per-share key.** With 10^5 per-child shards in one share:

- usage is written as per-shard deltas and folded into per-shard totals,
  summed when read, so no key is written from every shard
  ([RFC 16 §4.4](rfc-16-metadata-store.md#4.4%20Counters%20that%20many%20writers%20change));
- a path walk crosses the share's root, but lookups and listings that need no
  primary-held state are served from the store by any node
  ([RFC 15 §4](rfc-15-topology.md#4.%20Where%20each%20call%20runs)). The root's own primary sees only the root's namespace writes,
  which are as few as its entries.

## 3. The primary

### 3.1 The primary is fenced by an epoch

A node is primary of a shard while the shard record names it — as (node, node
epoch) — and its **node lease** for that node epoch is live. There is one lease
per node, held in its node record and renewed once for every shard it is
primary of, never per shard ([RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement)). A `protocol`-only node holds one too, so
it can be fenced before its client addresses are taken over
([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)).

| Installation setting ([RFC 13 §3](rfc-13-configuration.md#3.%20Scopes)) | Proposed default | Used by |
| --- | --- | --- |
| node lease | 10 s | the expiry each renewal sets; a takeover waits it out plus the drift bound, then gathers and seals, so a lost primary's shards stall for the lease, plus the drift bound, plus the gather interval, plus the seal — about 10.5 s plus a few seconds, and more when much was in flight ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover)) |
| renewal period | 3 s | how often a node renews; a failed renewal is retried at once, after a random jitter of up to a tenth of the period, then at the period, with no fixed count, until it succeeds. The self-fence falls 5 s — half the lease — after the node last reached the store and stops the node serving, not retrying: it keeps renewing at the period past the self-fence and past its lease's expiry, and resumes or is refused as the store-stall row of [§9](#9.%20Failure) says. So the first renewal after a store stall lands within a period of the store's return, not later |
| drift bound | 500 ms | a margin in time, not a rate: the self-fence margin before expiry, the clock check (a node whose clock is off the store's by more than 250 ms fences), and the wait a successor adds |
| clock-rate bound ρ | [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) | a rate: how much slower than store time a node's monotonic clock may run. A lease of `L` measured by a node's clock lasts at most `L × (1 + ρ)` in store time, and every wait this document sizes is stated that way ([RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement) item 3) |

> decision: the defaults above are proposals sized against one another — the
> self-fence on an unreachable store at half the lease, three renewals per
> lease, a retry jitter of a tenth of the period — not measured. The ratios are
> fixed: half the lease leaves a node that lost the store as long to stop as the
> successor still waits, and a tenth of the period spreads a cluster's retries
> after a store stall without delaying any past the period. Overturned by the
> takeover-time benchmark ([RFC 10 §15](rfc-10-journal-replication.md#15.%20Test%20plan%20and%20benchmarks)) or a measured rate of
> self-fences on store hiccups shorter than the lease.

- **Renew fails on a node record a takeover has marked lapsed**, and on a lease
  that lapsed in store time; the latter alone is resumed rather than replaced
  (below). Renewals write only the lease's expiry record and, in the same
  transaction, each of the node's journal generations, swapped from `g` to `g + 1`
  after the node has written `g + 1` to the journal's `format` and synced it; a
  journal whose swap loses is amnesiac and serves nothing more ([RFC 10 §2.2](rfc-10-journal-replication.md#2.2%20One%20journal%20carries%20many%20shards)).
  They never write the node record, so the commits that guard the node record ([§8](#8.%20Metadata%20consistency)) never contend with
  them. A node whose node record a takeover marked acquires a new lease at a higher node epoch,
  no sooner than the old expiry plus the drift bound in store time, and is then
  primary of nothing: the records still name its old node epoch. It regains a
  shard only by taking it over ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover)), which raises the shard's epoch and
  runs grace; the old node epoch counts as lapsed, so it can take over its own
  shards at once.
- **A change of primary is a compare-and-swap** on the shard record. Which
  changes raise the epoch is [RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms)'s rule, stated
  there once: a takeover and a handover raise it; a re-claim of a shard with no
  replica and a journal attach on one node do not. Every one of them raises the
  **shard incarnation**, which rises every time a node or journal begins serving
  the shard as primary and on nothing else: not on a raise before a move, not on
  a change of replicas. It fences nothing; it feeds the write
  verifier ([§7](#7.%20Protocol%20state)) and names the shard's grace instance
  ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)), so a primary that serves a shard again after another
  did never repeats a verifier it gave before.
- **An epoch never decreases for a file.** A shard's epoch **MUST** exceed that of
  every shard that previously held any of its files. A shard that receives files
  is therefore raised above every epoch they leave, before the first batch of a
  move ([§4](#4.%20Moving%20files%20and%20primaries)); a new shard starts one above its parent's. A file created in a
  shard starts at the shard's epoch.

**A primary fences itself.** It **MUST** stop acknowledging writes, serving open
state, serving reads and answering version queries ([§6](#6.%20Reads%20on%20other%20nodes)) once its node lease
is within the drift bound of expiry by its own clock, or once it has not reached
the store for half its lease. Each renewal returns the store's time, and a node
whose clock differs from it by more than half the drift bound, after allowing
half the renewal's round trip, fences itself as if its lease had lapsed. A successor **MUST NOT** serve before the old lease has
lapsed plus the drift bound, in store time ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover)).

**Store time read on two nodes.** On a store with one time source, `Now` is the
same clock for every node. On a store whose `Now` is each node's own clock,
clamped by its clock record ([RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend)), the old primary's expiry was
taken from its `Now` and the successor compares it with its own, which may read
up to the clock-offset bound σ ahead ([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)). There
every wait that compares a time one node stored with another node's `Now` —
a takeover's wait for a lapsed lease, and the mark that precedes an address
takeover — **MUST** add σ: the successor waits until its `Now` passes the
expiry plus the drift bound plus σ. So `(L − δ) × (1 + ρ) < L + δ`
([RFC 10 §3](rfc-10-journal-replication.md#3.%20What%20it%20assumes%20of%20shard%20placement) item 3) still holds in the old primary's own store
time. Every other lease and hold comparison here is of a time a node stored
against that node's own `Now` — a renewal, a resume, a node's new lease after a
mark, a participant's hold deadline ([§8.1](#8.1%20Operations%20across%20shards)) — and adds nothing.

> decision: on a store whose `Now` is each node's clock, the clock check above
> compares a node's clock with that same clock clamped, so it catches only a
> clock stepped back under its ceiling; the offset between two nodes is σ, an
> assumption no node can check alone, as on one node
> ([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)). Overturned by a store that
> exposes one time source to every node, which makes σ zero here and the check
> meaningful, or by a measured offset between two nodes above σ.

**Health is more than reaching the store.** A node that renews while it cannot
do its shards' work would keep them forever. So a node **MUST** stop renewing —
and its shards fail over — while any of these holds for longer than the
**stall bound**, an installation setting (proposal: 5 s): a journal sync it has
issued has not completed; its serving loop has made no progress on a queued
call. Each is checked by a watchdog inside the process, apart from the paths it
watches. A node that merely runs slowly but within the bound keeps its shards.
Replication is not watched node-wide: one replica that cannot keep up is
removed, and a primary that hears from no replica of a shard relinquishes that
shard alone, removing none, so its other shards keep serving
([RFC 10 §7.2](rfc-10-journal-replication.md#7.2%20Removal)).

**A store stall does not take every shard over.** When the store stalls for
longer than a lease, every node self-fences at half its lease and every lease
lapses in store time, though no node failed. A node whose lease lapsed but whose
node record no takeover has marked **resumes** its lease at the same node epoch,
by a transaction that writes the node record unmarked and so conflicts with any
claim that would mark it: exactly one commits. The resume transaction also
swaps each of the node's journal generations, as a renewal does, and the node
**MUST** complete those swaps before it serves or acknowledges anything again
([RFC 10 §2.2](rfc-10-journal-replication.md#2.2%20One%20journal%20carries%20many%20shards)); a journal whose swap loses is amnesiac.
Without it, a VM imaged while it held its lease and restored after the lease
lapsed, with no takeover between, resumes at its old node epoch and serves its
rolled-back journal under its old verifier, and the writes acknowledged after
the image are gone with no client told. A resumed node keeps its shards,
its open state, its write verifier, and runs no grace. Only the process that
held the lease resumes, its memory intact; a process that restarted acquires a
new node epoch and takes its shards over as before.

**The claim hold.** To let resumption win the race it is meant to win, a node **MUST NOT**
mark another's node record lapsed — by claiming its shards, or by the
mark that precedes an address takeover
([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)) — when that node's lease
lapsed while the marking node itself could not reach the store, until the
marking node has reached the store again for at least **two renewal periods**
plus the drift bound. A stalled node keeps retrying its renewal at the period through
the stall, past its self-fence, so its resume lands inside the first period after the store returns; the second covers a resume
that lost its first attempt. A node that is really gone is then marked that much
later, and a stall costs a pause, not a failover of the installation. On a single node the stall is a local
fault under the profile's own threshold ([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)).

The lease alone protects nothing. A primary can pause past its lease and then
act; a check it makes before sending is already stale. What makes it safe is that
every receiver refuses a stale epoch — replicas ([RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing)) and the
metadata store ([§8](#8.%20Metadata%20consistency)). The shard record decides who **should** write; the
epoch decides whose writes **count**. Open-state grants are replies to clients
that no receiver can refuse, so for them the self-fence is the only fence.

**Every fenced commit takes a shared guard on its primary's node record**, and a
takeover marks that record lapsed in the transaction that claims the shard
([§8](#8.%20Metadata%20consistency)). A commit a paused primary has in flight aborts on its guard if the
claim commits first; if it commits first, it is ordered before the claim, under
a fence that still held. One it starts later reads the mark and is refused —
before the successor has written a single fence record. The guard is
on the node record, not on the lease's expiry record or the journal generations
a renewal writes, so renewals never conflict with commits.

### 3.2 The primary stays until another node needs it

- A shard that has never had a primary, or whose last primary released it
  cleanly after offloading everything, is **claimed** by the first storage node
  that needs it, with one compare-and-swap.
- A shard whose record names a primary with a lapsed lease is **not** claimed
  that way. It is taken over by a replica that is not a learner
  ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover)); a plain claim by any other node would leave the replicas
  outside the record, and they would discard every acknowledged write.
- Nothing is exchanged per write or per file while the primary stays; its node
  renews one lease for every shard it holds.
- Any other node that receives a write forwards it to the primary
  ([§5.1](#5.1%20Front-ends%20forward%20to%20the%20primary)), or, under [§3.3](#3.3%20The%20primary%20follows%20the%20writer), asks for the primary to move.

A single writer therefore pays one metadata-store transaction per shard it
claims, not per file or per write.

### 3.3 The primary follows the writer

Forwarding is the default. A shard's primary is handed over to another node
([§4](#4.%20Moving%20files%20and%20primaries)) only when all of these hold:

1. **One node sends most of the writes.** Over the last **window**, at least
   **share** of the shard's written bytes arrived forwarded from one node.
2. **The dwell time has passed** since the shard's last planned change of
   primary.
3. **The new node is a replica that is not a learner**, or becomes one first
   ([RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover)).

Only the primary decides, from the forwarded writes it counts itself; front-ends
never request a move. A handover whose un-offloaded tail does not shrink while it
is shipped — the writer is outrunning the copy — is abandoned, and the dwell time
restarts. Two nodes writing one shard by turns therefore never pull it back and
forth faster than once per dwell time, and two writing at once never meet
condition 1.

**The dwell binds every planned change of primary**: follow-the-writer,
re-placement and rebalance each wait until the dwell time has passed since the
shard's last one. A failover is not gated and does not restart the dwell, and
neither is the handover away from a primary the front-ends cannot reach
([§5.1](#5.1%20Front-ends%20forward%20to%20the%20primary)): both answer a primary that is lost to its clients, which no dwell may
keep in place. Without the one dwell, a follow-the-writer move and a
re-placement back to the slot's node could alternate, shipping the tail each time.

| Setting | Proposed default | Meaning |
| --- | --- | --- |
| `shard.follow_writer` | on | off: the primary never moves on its own; only an operator or rebalance moves it |
| `shard.follow_writer.window` | 5 min | how far back condition 1 looks |
| `shard.follow_writer.share` | 0.9 | the fraction of written bytes one node must have sent |
| `shard.dwell` | 30 min | how long after a planned change of a shard's primary before any planned change may move it again |
| `shard.replace_delay` | 10 min | the re-placement delay ([§2.2](#2.2%20Automatic%20per-child%20shards)) |

## 4. Moving files and primaries

Two things move, and both are handovers: the primary of a whole shard, and a
batch of files from one shard to another.

**A shard's primary moves** by [RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover): the new primary is first a replica
that is not a learner; the old one stops writes, grants and reads, drains, and
names the new one at the next epoch; it hands over the open-state table and the
dedup table; the new primary re-applies existence and serves with no grace period.
A handover whose old primary is lost before it completes becomes a failover.

**Files move between shards in batches.** A subtree can hold more files than one
transaction may write. A move from giving shard G to receiving shard R keeps its
cursor in a **move record** of its own, keyed by the pair (G, R), so advancing it
changes neither shard record and several moves out of G run at once.

**A move into or out of a shard that a live subtree cut covers MUST be refused**
with `ErrSubtreeSnapshot` ([RFC 12 §2.10](rfc-12-snapshots.md#2.10%20Subtree%20snapshots)). A file's history is kept by
testing its shard's coverage, so a file moved out would drop history the
snapshot reads, and a file moved in would bring records the cut never ordered. A
handover of a covered shard's primary moves no files and is allowed.

> ponytail: coverage is per shard, so one live subtree snapshot pins every file
> of its covered shards in place until it is deleted. Upgrade to per-file
> coverage, the cut recorded on each file it covers, when refused moves under
> long-kept subtree snapshots block rebalancing or directory marking.

Before the first batch, if R's epoch is not above G's, R's primary raises it to
one above G's by an ordinary change of R's record and installs it on R's replicas
([RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing)). Then each batch runs these steps:

1. **Freeze.** G's primary stops acknowledging writes, granting open state and
   serving reads for the batch's files, recalls their layouts, and drains what is
   in flight for them. Calls for them wait; nothing else in G pauses.
2. **Ship.** G's primary sends R's primary each file's un-offloaded operations
   (`Export`, [RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)) — held superseded versions and their snapshot
   hold marks included ([RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)) — with the files' open-state
   entries and dedup entries, tagged with the move, the batch and G's epoch. R's
   primary accepts a ship only for a batch it has open under that epoch, and
   **re-versions** it: each operation gets a new version under R's epoch, in the
   order exported, so each file's versions keep their order; a stamp and a hold
   mark follow the operation they were on to its new version. Each is replicated
   to R's replicas as an ordinary write
   ([RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover)).
3. **Commit.** Once the re-versioned operations are durable on R's primary and
   every one of R's replicas, R's primary commits one transaction that:
   - reads both shard records with conflict tracking, and commits only if G's
     epoch is still the one the batch was frozen under and R's is still the one
     R installed, above it;
   - for each file whose recorded shard is still G, rewrites it to R and writes
     its fence records as (R, R's epoch);
   - writes R's hold record for every cut whose held versions the batch shipped
     ([RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)), so the snapshot waits for R's offload of them;
   - adds R to the shard list of every client record the batch's open-state
     entries name ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)), so a failover of R the moment after the commit
     already tells those clients to reclaim. Added after the commit, a failover
     between the two would lose their state in R with no client told;
   - advances the move record's cursor.

   If either epoch has changed, nothing commits: R's primary `Discard`s the
   batch's files under R on itself and every replica, and the batch is frozen and
   shipped again under the new epochs.
4. **Serve.** R's primary re-applies existence for the batch's files, as after a
   takeover ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover) step 5), installs the open-state entries, whose
   client records already name R, and serves the files. G's
   primary refuses any later call for them with a routing error, because their
   recorded shard is no longer G, and front-ends re-route.
5. **Discard.** G's primary sends its replicas a committed point covering the
   batch, then tells every node of G's replica set, itself included, to `Discard`
   the files under G. A node in R's replica set too keeps its entries under R:
   `Discard` names one shard ([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)), whenever the node joined R. A late G
   operation for a moved file is below G's committed point and is refused.

![Moving a batch of files between shards](img/rfc11-move.svg)

Every file is in exactly one shard between batches, so a crash leaves nothing to
repair: the move resumes from its cursor. If G fails over mid-move, its new
primary reads the move record and resumes from step 1. A takeover of either shard
writes fence records only for files recorded in its own shard; a takeover of R
discards its entries of files still recorded in G, which a batch shipped but did
not commit.

**Snapshots.** A share's cut is one transaction over all its shards, behind each
shard's cut gate ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)). While a snapshot record of the share in state
`cutting` exists, the primary of a new shard — a per-child shard just created
([§2.2](#2.2%20Automatic%20per-child%20shards)) — and a primary that starts serving a shard, or files moved into
one, after a takeover, a handover or a move **MUST** start the shard's gate
closed, so no transaction it admits commits after the cut under the old cut
number. The holds the moved files carry follow their re-versioned content, and
R's hold records keep the snapshot waiting until R has offloaded them. For a
subtree cut, which closes only its covered shards' gates
([RFC 12 §2.10](rfc-12-snapshots.md#2.10%20Subtree%20snapshots)), the rule applies to the covered shards only.

## 5. Routing

### 5.1 Front-ends forward to the primary

File protocols carry operations on many files over one connection and cannot
redirect a client per file, so the front-end routes each operation:

- it looks up the shard of what the operation touches, and the shard's primary,
  in a cache of shard records read from the metadata store ([RFC 15 §6](rfc-15-topology.md#6.%20Learning%20primaries));
- it forwards the operation to that primary. Every routed call is callable across
  a network ([RFC 15 §4.1](rfc-15-topology.md#4.1%20Every%20call%20is%20safe%20to%20route)) and carries the route envelope
  ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)): a request ID, so a retry is answered once; the shard and
  epoch the sender expects; the sending node and its node epoch; and a **hop
  count**;
- the receiver refuses the call if the epoch is not its current one, if the
  file's recorded shard is not the one named, or if the sender's node epoch has
  been fenced; the front-end re-reads the shard record and retries with backoff
  until the call's deadline ([RFC 0 §10.3](rfc-0-data-lifecycle.md#10.3%20Every%20wait%20on%20a%20request%20ends%20at%20a%20deadline)), never for a fixed count, so a
  failover's stall is waited out rather than turned into an error;
- only the front-end forwards. A node that receives a call it is not primary
  for refuses it, naming the primary it knows, and **MUST NOT** forward a call
  whose hop count is not zero, so a call never cycles between stale caches.

![Routing and a stale route](img/rfc11-routing.svg)

The routing cache **MAY** be stale. A stale entry costs a refusal and a retry,
never a wrong write, because the check is at the receiver.

**Retries are answered once.** The primary keeps a dedup table of recent
mutations keyed by request ID alone, which is unique across the cluster
([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)) — not by shard or epoch, so a retry that straddles an epoch raise,
or is re-routed to the shard a file moved to, is still recognised — holding each
result for at least the sender's retry window. The table travels with every
handover and, entry by entry, with the files of every batch. **It also survives a
takeover (cluster):** a journal operation's record keeps its request ID and
result, and the pair outlives the operation's eviction for the retry window
([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)); every namespace mutation writes a **request
record** — its request ID and result, with an expiry of the retry window — in its
own transaction ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)). A new primary rebuilds the table from both
before it serves, and deletes expired request records as it folds. A retry
after a takeover is therefore answered from the operation's own durable record,
never applied a second time. The request ID is keyed on the protocol's own
retry identity, not on the connection, because an NFSv4 client retries only on a
new connection (RFC 7530 §3.1.1) and an NFSv3 one may: for NFSv3 and NFSv4.0 it
is (client address, XID, procedure, checksum of the arguments), as a server's
reply cache keys a retry; for NFSv4.1 it is (session, slot, sequence ID). A
retry carrying the same identity, on any connection and through any front-end
that holds the client's address, maps to the same request ID
([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)). An NFSv4.1 retry after its session was lost
comes under a new session and is a new request, and an SMB replay is matched by
the primary from its open's own state.

**A primary the front-ends cannot reach** while it still reaches the store would
keep its lease forever. Front-ends report failed forwards to the store, which
records a health condition on the node. If it persists for the re-placement
delay, each of the node's shards is handed over to a replica the front-ends can
reach; a primary that does not complete a handover within a bound is refused its
next renewal, which turns the handover into a failover.

On one node the front-end and the primary are one process, and routing is a
function call.

### 5.2 Clients that can route themselves

A protocol that lets the server direct a client's I/O per file — parallel NFS
layouts — **MAY** point the client at the primary directly, so its reads and
writes skip the front-end hop. A layout is another holder of the routing
decision: it records the (shard, epoch) it was granted under, the primary as data
server checks it on every `READ`, `WRITE` and `COMMIT`, and it **MUST** be recalled
or revoked when the shard's primary changes or its file moves
([RFC 15 §5.1](rfc-15-topology.md#5.1%20pNFS)). The flex-files layout's own fence — the metadata server
changing a synthetic uid — is not a substitute: it fences every client at once
and is not tied to an epoch. Clients without layouts use the front-end path;
nothing in this document depends on layouts.

## 6. Reads on other nodes

| Served by | When |
| --- | --- |
| the primary | content the whole replica set holds ([RFC 10 §8](rfc-10-journal-replication.md#8.%20Reads)) |
| any other storage node, replicas included | only bytes carrying, at every extent within the read, the version the primary names for it |
| a learner | never |

A node other than the primary **MAY** serve an extent, from its own journal or by
filling it from metadata and the remote tier ([RFC 0 §6.2](rfc-0-data-lifecycle.md#6.2%20Fill)), only after asking
the primary for the newest version the whole replica set holds at each extent the
read covers — one version per extent, since a read can span bytes written at
many versions — and only bytes that carry exactly the version named for their
extent, under the checks [RFC 10 §8](rfc-10-journal-replication.md#8.%20Reads) makes of the
answer's epoch and lease — its own checks, not the primary's. **Otherwise it MUST
forward the read.** Metadata
alone cannot tell it: the primary acknowledges a write before offloading it, so
the current ref ([RFC 6 §2.1](rfc-6-block-metadata.md#2.1%20ChunkRef)) can be older than an acknowledged write, and a
replica's copy can be older than one the primary holds. A cached copy older than
the named version is dropped ([RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)) and filled again. The question costs
one round trip and no bytes.

**Attributes follow the same line.** A file's size and times after an
acknowledged but unflushed write live only at its primary. A `GETATTR`, or any
call whose answer must reflect every acknowledged write, goes to the file's
primary ([RFC 15 §4](rfc-15-topology.md#4.%20Where%20each%20call%20runs)). A directory listing with attributes (`READDIRPLUS`,
SMB query-directory) **MAY** take the attributes of children in other shards from
the store, which lags the primary by at most the existence commit's bounded age
([RFC 0 §5.1](rfc-0-data-lifecycle.md#5.1%20Write)): a flush commits the existence of writes that
overwrite committed content, but an append or hole fill reaches the store only
with the next group commit, so a child's size and times there can be that much
behind a flushed write. Close-to-open consistency holds regardless, because the
open that follows a close revalidates with a `GETATTR` at the file's primary; a
listing **MUST NOT** present store attributes as fresher than that bound.

## 7. Protocol state

Client-visible state is [RFC 14](rfc-14-open-state.md)'s: held by the primary of the file's shard,
fenced by its epoch, moved with it, and recovered through grace
([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable), [RFC 14 §9](rfc-14-open-state.md#9.%20Open%20state%20and%20the%20life%20of%20a%20file)). Four rules follow from this document and bind it:

1. **A failover loses volatile open state.** The new primary runs grace for the
   shard and releases nothing before it ends
   ([RFC 14 §9.2](rfc-14-open-state.md#9.2%20A%20new%20primary%20releases%20nothing%20before%20grace%20ends)).
2. **A handover and a batch move do not.** The old primary hands the files' state
   to the new one ([§4](#4.%20Moving%20files%20and%20primaries)), and no grace runs.
3. **The NFS write verifier MUST change whenever the shard's serving primary
   changes or its journal loses unstable writes, and SHOULD NOT change
   otherwise.** It is derived from five inputs ([RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more)): the primary's node,
   its node epoch, its process instance, the **shard incarnation**
   ([§3.1](#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) and the journal's **loss generation**. The incarnation
   rises every time a node begins serving the shard, so a primary that serves a
   shard again after another did — a handover there and back, with no restart —
   never repeats the verifier it gave before. The loss generation is monotonic per
   process across journal reopens, and rises only when the journal loses extents
   not yet offloaded or fails a sync window ([RFC 1 §5.3](rfc-1-journal.md#5.3%20A%20failed%20sync)), so a loss
   that costs nothing does not make every client resend. The shard epoch is not an
   input, so the raise before a move does not make every client resend its
   unstable writes.
4. **Client state is not a shard record.** It has its own semantics, grace and
   recovery, and **MUST NOT** share records with shard records, though both **MAY**
   be served by one metadata store.

## 8. Metadata consistency

**Every fenced commit carries (shard, epoch, node, node epoch)** and is refused
unless:

- for each file it touches, the file's fence record holds exactly that (shard,
  epoch) — *current* means equal, not merely not above, and a fence from another
  shard never matches whatever its number; and
- the primary's node record, which the commit guards ([RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend)), holds
  that node epoch and is not marked lapsed.

The fence record refuses a paused primary's commit once a successor has written
it. The node record refuses it before then: a takeover marks the old primary's
node record lapsed in the transaction that claims the shard ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover)), so a
commit in flight under the old node epoch aborts on its guard if the claim
commits first, is ordered before the claim if it commits first, and one that
starts later reads the mark. The guard is a conflict, not a time:
no check of store time against the lease's expiry fences a commit, because a
store whose commit path chooses the commit timestamp after the check could land
the commit past the expiry. Store time decides only when a lease has lapsed
([§3.1](#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)). Every commit of a node guards one record, and guards are shared, so
they never contend with each other; renewals write another record.

Fenced commits are existence ([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal)), offload ([RFC 6 §4.1](rfc-6-block-metadata.md#4.1%20What%20one%20commit%20records)),
truncate, deallocate, clone, release, the pruning of removal records, and every
namespace transaction — create, link, unlink, rename, set-attribute, ACL and
xattr changes, the pending release. A group commit across files checks each file.

> [!important] Release 1
> The two fence records per file, their readers and writers in the table below,
> and the rule that the check is a conflict on point records bind the single
> node: every commit there still reads and writes `F_x` and `F_o` as stated, and
> only the comparison of epochs is skipped
> ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit)).
> [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) cites this.

**The check MUST abort its commit if the fence changed after the commit's
snapshot**, whatever the store's isolation level: a guard, a read the store tracks
for conflicts, or an explicit lock on the key. A plain read under snapshot
isolation is not a fence, and neither is a scan over a key range. A change of the
fence that commits after the guarded commit needs no conflict with it: the
guarded commit is then ordered before the change, under a fence that was still
current, and the change — a takeover, a move's batch commit, a removal — is made
by a transaction that sees it. No rule here relies on a guard failing a writer
that commits after it ([RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend)).

**Two fence records per file**, one per commit path ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit)):

| Record | Read by | Written by |
| --- | --- | --- |
| `F_x(file)` | existence commits and namespace transactions, with conflict tracking | a new primary; a move; removals and releases |
| `F_o(file)` | offload commits and removal pruning | a new primary; a move; removals and releases; every offload commit ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit)) |

- A primary **MUST** rewrite both, as (its shard, its current epoch), inside
  the first fenced transaction that touches the object under that epoch — a
  file's existence or offload commit, or a namespace transaction on a directory
  — and only over fences that name its own shard at an older epoch; a fence
  naming another shard, or its own at its current or a newer epoch, is checked,
  never rewritten. That binds a new primary, and equally a primary that stays
  through a raise of its own shard's epoch ([RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms)), whose fences still hold the epoch
  before the raise and would refuse every commit it makes under the new one. The
  rewrite is lazy, per object, and rides in a transaction the primary was
  running anyway, so a raise costs nothing for objects it never commits for and
  no separate write is ordered against the journal. A commit the primary began
  under the old epoch that meets its own rewritten fence is refused and re-run
  under the new epoch. A create writes the new object's fence records.
- A removal or release reads and writes both, so it conflicts with an offload
  commit and with an existence commit of the same file in both directions.
- A fence that forces every commit of a shard through one record **MUST NOT** be
  used: it serialises every file of the shard on one key.
- A fenced commit covers only operations durable on the whole replica set
  ([RFC 10 §4](rfc-10-journal-replication.md#4.%20The%20write%20path)).
- **Release is fenced like a write.** It deletes the file's content record and
  records a removal of the whole file ([RFC 6 §6.4](rfc-6-block-metadata.md#6.4%20Delete)); a superseded primary's
  release would destroy a file its successor is writing.
- **Removal records are pruned only by the file's primary**, under its epoch.
  Only the primary knows which of its offload offers are still in flight.
- A **put intent** ([RFC 6 §7.6](rfc-6-block-metadata.md#7.6%20Put%20intents)) carries the shard and the
  (node, node epoch) it was written under. It is superseded once the shard
  record no longer names that (node, node epoch) as primary — after a takeover,
  a handover, or a re-claim after a restart, which raises no epoch but names a
  new node epoch — and not by a raise of the epoch alone: a join, a removal, a
  learner clear or the raise before a move leaves the primary in place, its
  puts in flight, and an intent superseded then would let GC remove an object
  the live primary is about to commit. The primary commits such an intent under
  its current epoch. A primary that hands a shard over ends its own put attempts
  for it — commits or abandons each intent — while it drains
  ([RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover)), and one that relinquishes a shard does the same before it
  writes the relinquished record ([RFC 10 §7.2](rfc-10-journal-replication.md#7.2%20Removal)). A superseded intent can no longer commit — its
  commit fails the fence or the node-record guard — and GC **MAY** remove it
  ([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)). On a single node the node abandons them all at
  start ([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)).

*Backend notes (non-normative).* On the backend that tracks point reads, a
conflict-tracked read is a get inside an update transaction with conflict
detection on; a blind write detects nothing, so a guarded write also gets the key.
On the snapshot-isolation backend it is an explicit key lock, taken on several
files in file-identity order. Transaction size limits set the batch size of moves
and removals, and no transaction spans an upload.

### 8.1 Operations across shards

A rename, link or unlink can touch files in several shards: the source
directory, the target directory, the file itself — whose shard need not be its
directory's ([§2](#2.%20Shards)) — and a target it replaces. Each check such an operation
needs lives at a different primary: a deny mode that forbids delete, a delegation
to recall, a directory watch. So:

1. **One primary runs it**: the source directory's, or, for a link, the target
   directory's. It is the **coordinator**.
2. **Prepare.** The coordinator asks the primary of every other shard involved
   to check the operation against its open state — deny modes, conflicting
   opens — to recall the caching grants it breaks, and to **hold** the files:
   refuse new opens and grants that would conflict, until an outcome arrives or a
   deadline passes. The deadline, in store time, is at most the participant's own
   node lease expiry less the drift bound, so no hold outlives the lease that
   protects it. Before it answers, each participant commits a durable **hold
   record** for the operation in its shard ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), keyed by shard
   and operation and distinct from a snapshot's hold record, a fenced write
   carrying the deadline. Each answers yes with its (shard, epoch), its
   (node, node epoch) and the hold's deadline, or no with a reason.
3. **Commit.** On every yes, the coordinator commits **one** metadata transaction
   that guards the fence records of every file and directory it changes, at the
   (shard, epoch) each primary answered with; guards each participant's shard
   record, and commits only if its epoch is still the one answered; guards each
   participant's node record as [§8](#8.%20Metadata%20consistency) does; and guards each participant's hold
   record, and commits only if every one still exists. A participant that failed
   over since it answered therefore refuses the commit even for a file its
   successor has not touched, and one that released its hold refuses it too. A
   directory rename runs the loop check inside this transaction
   ([RFC 7 §5.2](rfc-7-namespace-metadata.md#5.2%20The%20loop%20check%20is%20inside%20the%20transaction)).
4. **Release.** The coordinator sends the outcome, and each primary drops its
   hold and deletes its hold record. A file whose last entry was removed gets a
   pending release, which its own primary decides ([RFC 15 §4.2](rfc-15-topology.md#4.2%20Calls%20that%20touch%20two%20primaries)).

On any no, the coordinator sends abort and returns the protocol's
sharing-violation or file-open error. A participant releases its hold on its own
once the deadline has passed in store time, by a transaction that deletes its
hold record, and grants nothing the hold refused until that transaction has
committed. A lost coordinator therefore blocks nobody for longer than the
deadline, and a late commit aborts on its guard of the hold record, or finds
the record gone, rather than override a grant made after the hold ended; one
that commits before the release is ordered before every grant the hold refused. No time check
decides the commit: a store that chooses the commit timestamp after such a check
could land the commit past the deadline. A new primary deletes the hold records
its shard's earlier epochs left, which no commit can use since each guards the
shard record at the epoch answered.

**Snapshots.** A cross-shard transaction writes versioned records in every shard it
touches, so it **MUST** be admitted at all their cut gates or at none
([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)): the coordinator takes the gates in shard-ID order and,
on meeting a closed one, releases those it took, waits for it and starts again.
A prepared transaction stays admitted until it commits or its hold deadline ends
it, which bounds how long it keeps a gate from closing.

![A rename across shards](img/rfc11-cross-shard.svg)

## 9. Failure

| Failure | Outcome |
| --- | --- |
| a storage node is lost | its shards fail over to replicas as soon as its lease lapses ([RFC 10 §9](rfc-10-journal-replication.md#9.%20Failover)); after the re-placement delay they are handed to the nodes their slots name ([§2.2](#2.2%20Automatic%20per-child%20shards)); front-ends re-route on the first refusal |
| a node pauses past its lease | on resuming, everything it sends is refused: by replicas by epoch, by the store by its node record or fence ([§8](#8.%20Metadata%20consistency)); its renewal fails and it is primary of nothing ([§3.1](#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) |
| the store stalls for longer than a lease | every node self-fences at half its lease and its lease lapses in store time; each node keeps retrying its renewal at the renewal period past its self-fence ([§3.1](#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)); when the store returns, its next retry, within a period, resumes its lease at the same node epoch, completing its journals' generation swaps first, and keeps its shards, open state and verifier with no grace; every node holds its claims and address takeovers for two renewal periods plus the drift bound so resumption wins ([§3.1](#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) |
| a node reaches the store but its journal device or serving loop stalls | it stops renewing once the stall passes the stall bound, and its shards fail over ([§3.1](#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) |
| a primary reaches the store but none of a shard's replicas | it removes none of them and relinquishes that shard, which a replica takes over at once; its other shards are unaffected ([RFC 10 §7.2](rfc-10-journal-replication.md#7.2%20Removal)) |
| a front-end is lost | another front-end takes over its client addresses ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)); clients reconnect and lose no open state, which primaries hold |
| a primary is lost | its shards fail over; clients reclaim their state in them in grace ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)); layouts naming it are revoked |
| the store is unreachable from a node | it can neither renew nor commit; it fences itself after half its lease, and its shards fail over wherever the store is reachable |
| a partition separates nodes but not the store | who is primary does not change, since the store decides it; replication across the partition fails and [RFC 10 §7.2](rfc-10-journal-replication.md#7.2%20Removal) removes the unreachable replicas while one still answers; a primary cut off from every replica removes none and relinquishes the shard to them |
| front-ends cannot reach a primary that reaches the store | a health condition; handover after the re-placement delay ([§5.1](#5.1%20Front-ends%20forward%20to%20the%20primary)) |
| a coordinator is lost mid-prepare | holds end at their deadline; nothing was committed ([§8.1](#8.1%20Operations%20across%20shards)) |

## 10. Worked examples

Notation: nodes A–E; `S(e5)` is shard S at epoch 5; `f`, `g` are files. Each
example is a scenario in the simulator's catalogue ([§14](#14.%20Test%20plan%20and%20benchmarks)), named by the ID that
replays it exactly.

**(a) Many clients writing one share, and a per-child split** —
`S-shard-many-writers`. Share `/data` is one shard S, primary A, replicas B and C.
Thirty clients mount through A, B and C.

| t | node A | nodes B, C | metadata store | client sees |
| --- | --- | --- | --- | --- |
| 0 | primary of S(e5); journals writes from its own 10 clients | front-ends forward their 20 clients' writes to A | S: e5, A; B, C | all 30 write |
| 1 | orders and journals all 30 streams; network or disk at 100% | replicate A's operations as replicas | existence commits at e5 | throughput flat at one node's |
| 2 | operator marks `/data/users` per-child | — | mark on `/data/users` | — |
| 3 | moves each `/data/users/u*` tree into its own shard, in batches (as (d)) | — | shards U1…U30 placed by their slots: U1 → B, U2 → C, U3 → A, … | brief waits on files in a frozen batch |
| 4 | primary of S and U3, U6, … | B primary of U1, U4, …; C of U2, U5, … | — | each client's writes go to one of three primaries; throughput ≈ 3 nodes' |

Nothing a client does changes: every node still forwards what it is not primary
for. What changed is that there are now thirty primaries' worth of ordering to go
around.

**(b) The primary follows the writer, and does not ping-pong** —
`S-shard-follow-writer`. Shard P, primary A, replicas B and C; window 5 min,
share 0.9, dwell 30 min.

| t | node A | node B | metadata store | client sees |
| --- | --- | --- | --- | --- |
| 0:00 | primary of P(e3); a batch job on B's client writes P | forwards every write to A | P: e3, A; B, C | writes pay one hop |
| 0:05 | counts 97% of P's bytes from B over 5 min; dwell has passed; hands over (§4) | becomes primary | P: e4, B; A, C | writes pause for the drain, then no hop |
| 0:10 | a client on A starts writing P; forwards to B | counts A's share at 60% | — | A's client pays one hop |
| 0:20 | — | A's share reaches 95%, but only 15 min since the last move: stays | — | — |
| 0:35 | — | dwell passed, A still at 95%: hands over | P: e5, A; B, C | — |
| 0:36 | both clients now write at once | 50% / 50%: no node reaches 0.9 | — | P stays at A; B's client forwards |

Without the dwell, t 0:20 would have moved P back at once, and every move ships P's
un-offloaded tail.

**(c) A node crashes: failover now, re-placement later** —
`S-shard-crash-replace`. Per-child shard U7, primary A, replicas B and C; the
the slot U7 hashes into names A first. Re-placement delay 10 min.

| t | node A | nodes B, C | metadata store | client sees |
| --- | --- | --- | --- | --- |
| 0 | crashes | — | U7: e9, A; B, C | writes to U7 stall |
| 0 + lease + drift | — | B has the highest committed point: takes U7 over ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover)) | U7: e10, B; C | writes resume at B; grace for U7's open state |
| 4 min | restarts; acquires a lease at a new node epoch and is primary of nothing; rejoins U7 as a learner ([RFC 10 §7.3](rfc-10-journal-replication.md#7.3%20Joining)) | B covers A's tail | U7: e11, B; C, A (learner) | — |
| 6 min | tail covered | B clears the learner flag | U7: e12, B; C, A | — |
| 10 min | — | the re-placement delay has passed since the failover, the dwell since U7's last planned change long ago, and U7's slot still names A: B hands U7 over ([§4](#4.%20Moving%20files%20and%20primaries)) | U7: e13, A; B, C | a brief drain; no grace |

Had A stayed down past 10 min, it would have left the placement set, the slot
table would have given A's slots to other nodes by capacity, and U7 would have
been handed to its slot's new first node, after that node joined as a replica. A
node that flaps within the delay never changes the slot table.

**(d) Moving a batch of files between shards** — `S-shard-batch-move`. Files
`f1`–`f3` move from G(e7) — primary A, replicas B, C — to R — primary B,
replicas C, D.

| t | node A (G's primary) | node B (R's primary) | metadata store | client sees |
| --- | --- | --- | --- | --- |
| 0 | — | raises R to e8, installs it on C and D | R: e8, B; C, D; move record (G, R) cursor 0 | — |
| 1 | freezes `f1`–`f3`: holds new writes, recalls a layout on `f2`, drains | — | — | writes to `f1`–`f3` wait |
| 2 | exports their un-offloaded operations `v(7,…)`, open-state and dedup entries to B, tagged (G, e7, batch 1) | re-versions them as `v(8,1)`–`v(8,9)` under R; replicates to C, D; all durable | — | — |
| 3 | — | — | one txn: G still e7, R still e8 > 7; `f1`–`f3` → R; fences (R, e8); cursor 3 | — |
| 4 | refuses `f1`–`f3` with a routing error | re-applies existence at e8; installs open state; serves | existence at (R, e8) | waiting writes retry at B, found in the moved dedup entries or applied once |
| 5 | sends G's replicas a committed point covering the batch; tells A, B, C to `Discard` `f1`–`f3` under G | B and C drop their G entries and keep their R ones | — | — |

Had G failed over to C at e8 after t3, C's takeover would find `f1`'s fence
holding (R, e8): same number, other shard, so C's re-applied existence for `f1` is
refused and C drops that content. Had it failed over before t3, the commit would
have been refused, B would have discarded `f1`–`f3` under R on B, C and D, and
the batch would have been shipped again from G's new primary.

**(g) A delayed copy of a move is refused** — `S-shard-move-delayed-copy`. As
(d), but a duplicate of t2's ship is delayed in the network.

| t | node A (G's primary) | node B (R's primary) | metadata store | client sees |
| --- | --- | --- | --- | --- |
| 0 | — | batch 1 commits as in (d) | `f1` → R, fences (R, e8) | — |
| 1 | — | a client truncates `f1` at `v(8,20)`; `cp` passes it; C and D settle | truncate committed at (R, e8) | `f1` truncated |
| 2 | — | the duplicate ship tagged (G, e7, batch 1) arrives: batch 1 is not open, refused | — | — |
| 3 | a duplicate G operation for `f1` reaches C | C: at or below G's committed point, and its G entry is gone: refused | — | `f1` stays truncated |

Had B accepted the duplicate, it would have re-versioned the truncated bytes above
`v(8,20)` and brought them back.

**(e) A rename across shards, refused then allowed** —
`S-shard-cross-rename`. `d1` is in S1 (primary A), `d2` in S3 (primary C), and
`f`, once renamed into `d1` from another tree, is still in S2 (primary B). A
Windows client has `d1/f` open without delete sharing.

| t | node A (coordinator) | node B (f's primary) | metadata store | client sees |
| --- | --- | --- | --- | --- |
| 0 | an NFS rename `d1/f` → `d2/g` arrives; prepare to B and C | the open denies delete: answers no | — | — |
| 1 | aborts; C drops its hold and deletes its hold record | — | nothing committed | NFS client: file open error |
| 2 | the Windows client closes `f`; the rename is retried; prepare again | no conflicting open: holds `f`, commits its hold record, answers yes (S2, e4), (B, 5), deadline T no later than its lease expiry less the drift bound | hold record of S2, deadline T | — |
| 3 | C recalls a directory delegation on `d2`, commits its hold record, answers yes (S3, e2) | — | hold record of S3 | — |
| 4 | commits one txn: fences `d1` (S1, e6), `d2` (S3, e2), `f` (S2, e4); shard records S2 at e4 and S3 at e2; node records of A, B and C; hold records of S2 and S3, both present | — | entries moved; `f` still in S2 | rename done |
| 5 | sends the outcome | drops its hold, deletes its hold record | hold records deleted | — |

Had B died after t2 and C′ taken S2 over before the commit, S2's record would be
at e5 and B's node record marked lapsed: the commit is refused, and no open C′
grants in its grace can be overridden by it — `S-shard-cross-hold-failover`. Had
B instead passed T and released `f` before t4, its release would have deleted
S2's hold record before granting a deny-delete open, and the commit would have
conflicted with that deletion or found the record gone, whatever timestamp the
store chose for it — `S-shard-cross-hold-release`.

**(f) A paused old primary** — `S-shard-paused-primary`. Shard S, primary A at
node epoch 3, lease expiry E; replicas B and C.

| t | node A | node B | metadata store | client sees |
| --- | --- | --- | --- | --- |
| 0 | S(e5); begins commits for `f` and `g`, then freezes | — | S: e5, (A, 3); fences `f`, `g` (S, e5) | — |
| 1 | — | after E + drift in store time, takes S over | one txn: A's node record marked lapsed at 3; S: e6, (B, 5); C | writes resume at B |
| 2 | — | writes `f`, writing its fences first | fence `f` (S, e6); `g` untouched | — |
| 3 | resumes; its commit for `g` arrives, guarding A's node record | — | refused: the record is marked lapsed, though fence `g` is still (S, e5) | `g` intact |
| 4 | its commit for `f` arrives | — | refused: (S, e5) ≠ (S, e6) | `f` has B's write |
| 5 | renewal fails; acquires a new lease at node epoch 4 | — | record names (B, 5): A is primary of nothing | — |

Had A's commit for `g` been in flight when B's claim committed, it would have
conflicted on A's node record and aborted — `S-shard-stale-commit-node-guard`.

## 11. API surface

Signatures are indicative; the obligations above are normative.

```go
// Shards is what storage nodes and front-ends ask of the metadata store.
type Shards interface {
	// ShardOf returns the shard a file belongs to, recorded with it (§2).
	ShardOf(ctx context.Context, file FileID) (ShardID, error)
	// Record returns a shard's record (§1, RFC 16 §2.3).
	Record(ctx context.Context, s ShardID) (metadata.Shard, error)
	// Claim names the caller primary of a shard that has none, or whose last
	// primary released it cleanly (§3.2). ErrHeld names the live primary;
	// ErrNeedsTakeover means the record names a lapsed one (RFC 10 §9.2).
	Claim(ctx context.Context, s ShardID) (Claim, error)
	// Renew extends the caller's node lease — writing its expiry record and
	// swapping each of its journal generations, never the node record — and
	// returns the expiry, the store's time and the journals whose swap lost.
	// gens holds, per journal, the g + 1 already synced to its format file
	// (RFC 10 §2.2). It fails with ErrLeaseExpired once the lease has lapsed or a
	// takeover marked it (§3.1); an unmarked lapse is then resumed, not replaced.
	Renew(ctx context.Context, node NodeID, nodeEpoch uint64, gens map[JournalID]uint64) (expires, storeNow time.Time, lost []JournalID, err error)
	// Resume revives a lease that lapsed in store time while no takeover marked
	// the node record, at the same node epoch, by writing the record unmarked
	// and, in the same transaction, swapping each journal generation as Renew
	// does; it conflicts with any claim marking it (§3.1). The node serves
	// nothing from a journal in lost. ErrLeaseExpired once marked.
	Resume(ctx context.Context, node NodeID, nodeEpoch uint64, gens map[JournalID]uint64) (expires time.Time, lost []JournalID, err error)
	// Move moves files from one shard to another in batches, resuming from the
	// move record of the pair (§4).
	Move(ctx context.Context, from, to ShardID, files iter.Seq[FileID]) error
}

// Claim is what a primary carries on every fenced call (§8): the fence
// records must hold (Shard, Epoch), and the node record, which the commit
// guards, must hold NodeEpoch unmarked.
type Claim struct {
	Shard     ShardID
	Epoch     uint64
	Node      NodeID
	NodeEpoch uint64
}

// Router is a front-end's routing cache (§5.1, RFC 15 §6).
type Router interface {
	Route(ctx context.Context, file FileID) (NodeID, Claim, error)
	Invalidate(s ShardID) // after a refusal
}

var (
	ErrHeld          = errors.New("shard: primary is live elsewhere")
	ErrNeedsTakeover = errors.New("shard: primary lapsed; take over through a replica")
	ErrLeaseExpired  = errors.New("shard: node lease lapsed; acquire a new one")
	ErrWrongShard    = errors.New("shard: file is not in the shard named")
)
```

## 12. Invariants

| # | Invariant |
| --- | --- |
| O1 | At most one node's writes to any byte take effect under any epoch, because every receiver refuses a stale one. |
| O2 | A file's epoch never decreases: the shard's epoch rises as [RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms) states, and a batch commits only at a receiving epoch above the one the batch was frozen under, installed on every receiving replica first. |
| O3 | A shard record names its primary as (node, node epoch); a node whose lease lapsed is primary of nothing until it takes a shard over. |
| O4 | A file's shard is set at create and changes only by a batched move; every file is in exactly one shard between batches. |
| O5 | A node other than the primary, a replica included, serves only bytes carrying the version the primary names; a learner serves none. |
| O6 | A stale route costs a refusal and a retry, never a wrong write; a retry is answered from the dedup table, keyed by request ID alone and handed over with every handover and batch. |
| O7 | A failover starts grace for the shard and releases nothing before it ends; a handover or batch move hands open state over and starts none. |
| O8 | The write verifier changes whenever the shard incarnation or the journal's loss generation changes, and is derived from no shard epoch; the shard incarnation rises every time a node begins serving the shard as primary, and on nothing else. |
| O9 | Shard records and client state share no records. |
| O10 | Every fenced commit is refused unless each file's fence record equals the (shard, epoch) it carries and the primary's node record, which it guards, holds the node epoch it carries unmarked; no fence is one record per shard. |
| O11 | Shard records are never per file. |
| O12 | Removal records are pruned only by the file's primary, under its epoch. |
| O13 | A primary acknowledges no write, grants no open state, serves no read and answers no version query once its lease is within the drift bound of expiry, or its clock differs from the store's by more than half the drift bound. A successor serves no sooner than the old expiry plus the drift bound by its own `Now`, plus σ on a store whose `Now` is each node's clock. |
| O14 | During a batch move, the giving primary acknowledges no write to the batch's files from freeze until it refuses them after the commit; the receiving primary re-versions what it accepts under its own epoch, accepts ships only for an open batch, and discards its entries of a batch that did not commit; `Discard` names one shard. |
| O15 | An operation across shards commits in one transaction, only after every other primary involved has checked and held its files and committed a hold record, only while each is still at the (shard, epoch) and node epoch it answered with, and only while every participant's hold record, which the commit guards, still exists; a participant grants nothing its hold refused until the deletion of its hold record has committed. |
| O16 | No shard's primary changes by a planned move sooner than the dwell time after its previous planned change. |
| O17 | A per-child shard's slot is fixed by its ID and the slot count, which never changes after installation creation; the slot table changes only by compare-and-swap, a rebalance hands over only the shards of slots whose nodes changed, and no shard record depends on the table for fencing. |
| O18 | A slot's nodes lie in distinct failure domains whenever the placement set spans enough of them. |
| O19 | A node renews only while its journals sync and its serving loop progresses, each within the stall bound; a shard whose primary hears from none of its replicas is relinquished, not renewed through. |
| O20 | A lease that lapsed with its node record unmarked is resumed at the same node epoch by the process that held it, with no takeover, grace or verifier change, and serves nothing from a journal before that journal's generation swap in the resume has won; a resume and a claim on one node record never both commit; no node marks another lapsed within two renewal periods plus the drift bound of its own return to the store. |
| O21 | A batch move's commit writes the receiving shard into every client record its open-state entries name. |
| O22 | A retried mutation is answered from a durable record of its first result — the journal operation's record or the mutation's request record — across a takeover, a handover and a move. |
| O23 | A put intent is superseded only once the shard record no longer names its (node, node epoch) as primary, so a re-claim after a restart supersedes the intents the earlier process left and a raise of the epoch under a live primary supersedes none. |
| O25 | A primary rewrites a file's or directory's fence records at its current epoch inside its first fenced transaction on the object under that epoch, whether it is new or stayed through a raise, and only over fences naming its own shard at an older epoch. |
| O26 | A per-child shard's replacement replica after a node loss is the node its slot would name, so a lost node costs each shard one join and one tail copy. |
| O24 | A shard is held in one journal per member, so its write bandwidth is one device's. |

## 13. Observability

Metric names are shown without the deployment's prefix.

| Answers | Metric | Type |
| --- | --- | --- |
| claims, labelled `result` = `granted`, `held` or `takeover` | `shard_claims_total` | counter |
| changes of primary, labelled `reason` = `follow_writer`, `rebalance`, `replace`, `operator` or `failover` | `shard_primary_changes_total` | counter |
| handover time, freeze to new primary serving | `shard_handover_seconds` | histogram |
| batches moved, and files in them | `shard_move_batches_total`, `shard_moved_files_total` | counter |
| reads on a node other than the primary, labelled `result` = `served`, `refilled` or `forwarded` | `shard_other_node_reads_total` | counter |
| shards this node is primary of, and replica of | `shard_primaries`, `shard_replicas` | gauge |
| shard records in the store; they follow shares and directories, not files | `shard_records` | gauge |
| planned moves back to a primary the shard left within the dwell time; nonzero is a bug | `shard_bounces_total` | counter |
| self-fences, labelled `reason` = `lease` or `clock` | `shard_self_fences_total` | counter |
| cross-shard operations, labelled `result` = `committed`, `refused` or `timed_out` | `shard_cross_ops_total` | counter |
| shards away from their placed node, waiting out the re-placement delay | `shard_displaced` | gauge |
| slots per node, and the node's share of capacity, so a skew is visible | `shard_slots`, `shard_capacity_share` | gauge |
| slots whose nodes a rebalance changed, and slots whose shards are still being handed over | `shard_slot_moves_total`, `shard_slots_pending` | counter, gauge |

Forwarding and refusals by epoch are counted once, in [RFC 15 §10](rfc-15-topology.md#10.%20Observability).

Logs: a change of primary and a move batch at `Info`, with shard, old and new
primary, epoch and reason. A node that fences itself on its lease or its clock
logs at `Error`.

## 14. Test plan and benchmarks

Claims, moves, cross-shard operations and routing are modelled together with
[RFC 10](rfc-10-journal-replication.md)'s shard-record protocol in one model checked before implementation
([RFC 10 §15](rfc-10-journal-replication.md#15.%20Test%20plan%20and%20benchmarks)), and exercised by the same deterministic simulator: one
process, every network, clock, store and journal reached through an interface,
everything driven from a seed. **A failing seed MUST reproduce its failure
exactly**, and every scenario below replays from its seed.

**Coverage.** Every run reports the branches it reached, and seed search **MUST**
reach each of these "sometimes" assertions or fail: a batch was re-frozen after an
epoch changed; a batch skipped a file recorded in another shard; a `Discard`
under G reached a node in both replica sets and left its R entries; a stale route was refused by epoch, and
by recorded shard; a call was refused at the hop limit; a retry was answered from
a dedup entry that crossed a handover; a follow-the-writer move was suppressed by
the dwell time, and a handover abandoned for a tail that did not shrink; a
re-placement waited out the delay; a prepare was refused, timed out, and its late
commit refused by the participant's deleted hold record; a commit in flight
aborted by a concurrent release; a commit was refused by the node-record guard,
aborted in flight by it, and refused by fence; a ship was refused as not open; a
re-placement waited out the dwell; a slot-table compare-and-swap lost to a
concurrent one and was recomputed; a rebalance handed over a slot's shards at the
rate limit;
`Renew` returned `ErrLeaseExpired`; `Claim` returned `ErrNeedsTakeover`; a lease
was resumed after a store stall and a claim lost to the resume; a node stopped
renewing on a stalled journal; a retry after a takeover was answered from a
request record; a slot table placed a slot across domains; a live primary
rewrote a fence after a raise of its own epoch; a resume's generation swap lost;
a claim or address mark waited out the claim hold.

**Scenario catalogue.**

| ID | Scenario |
| --- | --- |
| `S-shard-many-writers` | [§10](#10.%20Worked%20examples) (a) |
| `S-shard-follow-writer` | [§10](#10.%20Worked%20examples) (b) |
| `S-shard-crash-replace` | [§10](#10.%20Worked%20examples) (c) |
| `S-shard-batch-move` | [§10](#10.%20Worked%20examples) (d) |
| `S-shard-cross-rename` | [§10](#10.%20Worked%20examples) (e) |
| `S-shard-paused-primary` | [§10](#10.%20Worked%20examples) (f) |
| `S-shard-move-delayed-copy` | [§10](#10.%20Worked%20examples) (g) |
| `S-shard-cross-hold-failover` | a participant fails over after answering a prepare, and its successor grants a deny-delete open; the coordinator's commit is refused by the participant's shard and node records ([§10](#10.%20Worked%20examples) (e)) |
| `S-shard-stale-commit-node-guard` | a paused primary's commit is in flight while the claim commits; it aborts on the node record ([§10](#10.%20Worked%20examples) (f)) |
| `S-shard-move-abort-discard` | a batch fails to commit after R re-versioned it; R discards its entries under R, nodes in both sets keep G's, and the re-ship holds no stale extent |
| `S-shard-move-retry-rerouted` | a write's reply is lost during a batch; its retry, re-routed to R, is answered from the moved dedup entry |
| `S-shard-replace-dwell` | follow-the-writer moves a shard, then its re-placement comes due within the dwell: it waits |
| `S-shard-move-stale-route` | a write routed to the giving primary after a batch commits is refused and applied once at the receiver |
| `S-shard-move-giving-failover` | the giving shard fails over between freeze and commit; R discards the batch under R, and the batch re-freezes under the new epoch |
| `S-shard-move-receiving-crash` | the receiving primary is lost right after a batch commits; a receiving replica takes over with every moved write |
| `S-shard-move-crash-every-batch` | a move of 10^6 files crashed after every batch resumes, each file in one shard |
| `S-shard-mark-existing` | marking a directory per-child while files are created and hard-linked inside it |
| `S-shard-zombie-renew` | a node paused past its lease renews: refused; it serves no shard it held |
| `S-shard-takeover-clock-offset` | on a store whose `Now` is each node's clock, set the replica's clock σ ahead of the primary's and ρ fast, then partition the primary from the store; assert the replica serves no sooner than the primary's self-fence by the primary's own clock. A takeover that waits only the expiry plus the drift bound serves while the old primary still acknowledges |
| `S-shard-plain-claim-lapsed` | a non-replica tries to claim a shard whose primary lapsed: `ErrNeedsTakeover` |
| `S-shard-flapping-node` | a node misses renewals repeatedly within the delay: failovers, no slot-table change |
| `S-shard-slot-rebalance` | a node joins with twice the others' capacity: it gains about twice their slots, only those slots' shards are handed over, at the configured rate and under the dwell; no file changes shard |
| `S-shard-slot-cas-race` | two nodes recompute the slot table at once: one compare-and-swap wins, the other recomputes from it, and no slot's shards are handed over twice |
| `S-shard-unreachable-primary` | front-ends cannot reach a primary that reaches the store: handover after the delay |
| `S-shard-hop-loop` | two front-ends with crossed stale caches: refused at the hop limit, then routed |
| `S-shard-cross-timeout` | a coordinator dies after prepare: holds end at their deadline, each deleting its hold record; a late commit is refused |
| `S-shard-cross-hold-release` | a participant releases an expired hold while the coordinator's commit is in flight, the release committing first, on a store that chooses the commit timestamp after the commit's reads; the commit conflicts with the deletion of the hold record and aborts, and the deny-delete open the participant then grants stands |
| `S-shard-cross-dir-loop` | two cross-shard directory renames that together would make a cycle: one commits |
| `S-shard-readdirplus-foreign` | a listing shows children in other shards at their flushed size, never older |
| `S-shard-store-stall-resume` | the store stalls for 15 s with every node healthy; assert no shard fails over, no grace runs, and every write verifier is unchanged afterwards. A design where every lapsed lease is replaced fails over every shard |
| `S-shard-resume-vs-claim` | a primary's resume and a replica's claim race on one node record; assert exactly one commits, and a resumed primary serves only if the claim lost |
| `S-shard-journal-stall` | a primary's journal device stops completing syncs while the node still reaches the store; assert it stops renewing after the stall bound and a replica takes over. A health check by store reachability alone keeps the shard stuck |
| `S-shard-move-client-records` | fail R over immediately after a batch commits; assert every client whose open moved is told to reclaim in R. A client record updated after the commit misses them |
| `S-shard-retry-after-takeover` | a namespace mutation and a write each lose their reply; the primary fails over after the write's content was offloaded and evicted; the client retries through the same front-end on the same connection; assert each is answered with its first result and applied once. A request entry dropped with the evicted content applies the write twice |
| `S-shard-raise-live-primary` | a replica joins, then a learner is cleared, under a live primary with existence commits and puts in flight; assert every commit lands at the new epoch after the primary rewrites the touched files' fences, GC removes none of its intents, and no ref names a removed object. A design where only a new primary writes fences refuses every commit after the raise; one that supersedes intents by epoch leaks a dangling ref |
| `S-shard-resume-rollback` | image a primary's VM while it holds its lease, let the lease lapse with no takeover, restore the image; assert its resume's generation swap loses, it serves nothing from that journal, and the shard fails over with every acknowledged write. A resume that skips the swap serves the rolled-back journal under the old verifier |
| `S-shard-stall-renew-phase` | stall the store for 15 s with every node's renewal timer just past its last attempt; assert every node's first renewal after the stall lands within the jitter, no node marks another lapsed within the hold, and no shard fails over. A renewal retried only at the period, or a hold of one period, fails over shards |
| `S-shard-repair-one-join` | lose the first replica of a per-child shard's slot; let repair restore the count, then let the node leave the placement set; assert the recomputed slot names the node repair joined, and the shard saw one join and one tail copy |
| `S-shard-verifier-roundtrip` | hand a shard A → B → A with no restart; assert the write verifier A gives the second time differs from the first. A verifier from node, node epoch and process instance alone repeats |
| `S-shard-slot-domains` | four nodes in two racks, replica count 2; assert every slot's nodes lie in different racks, and that a rack loss leaves every shard a replica |
| `S-shard-intent-reclaim` | a primary of a shard with no replica crashes between an intent and its put, restarts and re-claims; assert the intent is superseded by the node epoch and collected |

**Properties** every run asserts:

| Property | Violated by |
| --- | --- |
| At most one node's writes to any byte take effect under any epoch (O1) | a sender-side check standing in for receiver fencing |
| A file's epoch never falls across a move (O2) | a new shard numbered from its own history; a batch committed at an epoch R's replicas have not installed; a batch committed after G's epoch changed |
| A moved file's acknowledged writes survive the loss of any one receiving node (O14) | a batch committed with content on the receiving primary alone; a `Discard` under G that dropped a node's entries under R; a write acknowledged by G after freeze |
| A commit from another shard with the same epoch number is refused (O10) | a fence holding only the epoch |
| A paused former primary's writes, commits, grants and reads are refused or withheld (O3, O10, O13) | `Renew` succeeding on an expired or marked lease; a commit fenced by store time against the expiry instead of the node-record guard; a self-fence that omits reads |
| A deny mode at another primary is honoured by a cross-shard rename (O15) | a coordinator that commits without preparing; a commit that does not guard every participant's hold record; a release that grants before the deletion of its hold record commits; a commit decided by a check of `Now` against the deadline; a hold outliving its participant's lease or failover |
| Moved content is never resurrected (O14) | a ship accepted outside its open batch; an aborted batch's copy left on R; `Discard` across shards |
| No shard's primary moves twice within the dwell time (O16) | follow-the-writer from a single sample; a re-placement not gated by the dwell |
| Placement changes move only the shards of slots whose nodes changed, in proportion to capacity (O17) | a placement recomputed per shard; a slot count that follows the node count; a slot table written without compare-and-swap |
| The store's shard-record count follows directories, not files (O11) | a per-file record or a file-to-shard map kept as shard records |
| A shard's writes resume after a handover, move or failover without operator action | a move that waits on a primary that is gone |

**Benchmarks**, on three storage nodes on one local network:

| Benchmark | Measures | Target |
| --- | --- | --- |
| Claims per file | store transactions per file written, one writer, shard already claimed | 0 |
| Handover | a shard with 64 MiB un-offloaded | ≤ 2 s at 10 Gb/s |
| Forwarding overhead | p50 latency one front-end hop adds | ≤ one network round trip + 100 µs |
| Stale route | operations after a move | each refused at most once, then applied once |
| Batch move | files per second moved, 10^6 small files | report |
| One share, many writers | throughput of 30 writers, one shard vs 30 per-child shards | per-child ≥ 2.5× one shard |
| Two alternating writers | changes of primary per hour | ≤ 60 / dwell minutes |

> decision: the handover, forwarding and many-writers targets are proposals
> stated before any measurement: a handover ships its tail at line rate plus a
> drain, a hop costs one round trip plus dispatch, and 30 per-child shards over
> three nodes should come within a sixth of three nodes' throughput. The first
> run of each benchmark replaces its target with the measured value and a
> margin; the claims-per-file and alternating-writer targets are derived and stay.

## 15. Open questions

1. **Follow-the-writer defaults.** The window, share and dwell in [§3.3](#3.3%20The%20primary%20follows%20the%20writer) are
   proposals, to be tuned from measured workloads.
2. **Read grants** ([§6](#6.%20Reads%20on%20other%20nodes)): a shared grant per extent, revoked before a write is
   accepted over it, if the per-read round trip shows in measurement.
3. **A per-share balancer as an interim step.** Before shards move between
   nodes, a balancer in front of several single-node installations could place
   whole shares on nodes — shape B of [§1.1](#1.1%20What%20scaling%20is%20being%20designed%20for) without shard moves. It needs a
   cap on share size (50 GB has been proposed) so that no share outgrows its
   node. That cap does not fit per-user profile containers
   ([Reference workloads](rfc-index.md#Reference%20workloads)): every user's container on one share passes
   50 GB at two to five users. It fits only if profile shares are split per user
   group, which would make that split a deployment requirement. Whether to build
   the balancer, and with which cap, is open.

---

## Appendix A — prior art

| System | Takes from it |
| --- | --- |
| RADOS placement-group primary (Ceph) | the closest relative: a primary orders writes, replicas hold copies, an epoch fences a superseded primary; and a fixed number of placement groups per pool, mapped to devices weighted by capacity, so a change moves whole groups — the model of [§2.2](#2.2%20Automatic%20per-child%20shards)'s slots |
| CephFS subtree pins and distributed pins | the policy ladder: one metadata server per share, subtrees by hand, children spread by hash (`ceph.dir.pin.distributed`); an automatic balancer that is off by default because it thrashed — why [§3.3](#3.3%20The%20primary%20follows%20the%20writer) needs hysteresis |
| Lustre DNE | remote directories placed by hand, and striped directories that hash one directory's names across servers — the upgrade [§1.2](#1.2%20Non-goals) defers |
| WEKA | a fixed number of hashed buckets, each with a leader that moves on failure — [§2.2](#2.2%20Automatic%20per-child%20shards)'s slots, whose whole-slot handovers bound what a node change moves |
| HDFS router-based federation | stateless routers that proxy to the owning namespace; a cross-namespace rename it refuses, which [§8.1](#8.1%20Operations%20across%20shards) does not |
| 3FS, JuiceFS, VAST | the contrast: stateless servers over a transactional store or shared media need no primary, because they acknowledge nothing the store does not hold |
| Frangipani, GPFS | tokens held by the node using them and revoked on demand; a peer recovers a failed node's state only after fencing it |
| Parallel NFS, flex-files layouts | the client routed to the data's server by a layout the server recalls; the synthetic-uid fence [§5.2](#5.2%20Clients%20that%20can%20route%20themselves) does not rely on |
| Fencing tokens (Kleppmann, 2016) | a lock is safe only if the resource rejects the stale holder |

## Appendix B — alternatives considered

Alternatives to one writer per shard are in [RFC 10 Appendix B](rfc-10-journal-replication.md#Appendix%20B%20%E2%80%94%20alternatives%20considered).

| Alternative | Why not |
| --- | --- |
| A separate lock service beside the metadata store | a second source of truth for who may write; shard records in the store are fenced by the same transactions that use them |
| Leases held in memory by a lease manager | faster, but needs its own recovery; with shards as coarse as a share, claims are too rare for the store's latency to matter |
| Two primaries per shard, one for the namespace and one for data | separate scaling, at the price of five cross-primary handoffs and a check-then-I/O race; kept as the upgrade of [§2.1](#2.1%20One%20primary%20per%20shard) |
| Cross-shard rename and link refused with `EXDEV` | simplest, but a share is one filesystem to its clients, and applications that rename across directories would break wherever a split happened to fall |
| A two-phase commit across primaries with a durable coordinator log | unnecessary: one store transaction commits the change; the prepare holds open state, bounded by a deadline and by a hold record the commit guards |
| Consistent hash of each shard ID directly over the nodes | no single record says where shards belong, so a rebalance must evaluate every shard to find the ones that move, and weighting by capacity needs virtual nodes whose count changes as nodes join; a fixed slot count makes placement one small table, changed by one compare-and-swap, and a rebalance a list of whole slots |
| A metadata store split into partitions with no transaction spanning them | existence, release, batched moves and cross-shard operations each commit records of several shards, or of a shard and the node records it guards, in one transaction; without it each needs a cross-partition protocol of its own. A backend that scales horizontally while keeping transactions across keys gives the capacity without the protocol |
| Placement hashed over nodes holding a live lease | one missed renewal would re-hash every shard the node held, and its return would move them all back |
| Follow-the-writer on any write from a new node | two writers taking turns ping-pong the shard, shipping its tail every time |

## Appendix C — later: per-file and range shards

Deferred. Nothing above depends on them, and no store record, fence or invariant
for them exists until this appendix is promoted.

> ponytail: a file's writes are ordered by one primary, so one file's
> contention and bandwidth are capped at one node. Add per-file and range shards
> when one file's measured load exceeds one node — pNFS striping of a large file
> across data servers is the expected first case.

- **Per-file shard.** A file split from its enclosing shard into its own, recorded
  with the file, so shard records still do not grow per file. For a file whose
  contention — many writers, a hot lock — no coarser shard absorbs.
- **Range shard.** A contiguous extent of one file split off with its own
  primary, epoch, replicas and fence records keyed by (file, range start). The
  file's own shard — its **base shard** — keeps its namespace record, open state
  and every range not split off. A pNFS layout then names one data server per
  range.

What the sketch already knows it must solve: size, change attribute and charged
bytes derived across ranges without two primaries writing one record, and folded
monotonically into the base when a range merges back; a commit that finds no
range fence record after a merge is refused; layouts at range primaries must be
dropped when the base shard's epoch changes; I/O from clients without layouts
funnels through the base primary; and a range shard's entries in a journal are
keyed by (range shard, file), as moves already key them by shard
([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)).
