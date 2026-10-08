---
rfc: 12
title: "RFC 12 — snapshots, clones, catalog backups and namespace migration"
component: snapshots
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-4-remote-tier]]"
  - "[[rfc-5-transforms]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-9-gc]]"
  - "[[rfc-10-journal-replication]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-16-metadata-store]]"
aliases:
  - RFC 12
tags:
  - rfc
---
# RFC 12 — snapshots, clones, catalog backups and namespace migration

**Status:** draft. [§10](#10.%20Open%20questions) lists what is undecided.
**Audience:** anyone implementing snapshots, clones, catalog backup and restore, or
moving a namespace between installations that share one bucket. Conventions and
test tiers are in [the RFC index](rfc-index.md).

This document specifies behaviour, not the current code. [Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists
where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

**What this is.** A snapshot is a read-only view of a share as it was at one
instant, kept as long as it is wanted and browsable from the client. This RFC
also builds on snapshots the other ways of protecting and moving data: writable
clones, backups of the metadata (and optionally of the data), restore into a new
share, and moving a whole tenant to another DittoFS installation that uses the
same bucket.

**The problem, with one example.** Single-node install N1; clients write to a
fast local journal, and in the background the bytes are cut into **chunks**
(~256 KiB, named by the hash of their content), packed into **blocks** (~4 MiB
objects) and uploaded to the S3 bucket `dfs-data`. The metadata store keeps a
**ref** for each use of a chunk by a file, and a block is deleted by GC
(garbage collection) once no ref reaches its chunks.

The `profiles` share takes a snapshot every hour. Four have been taken, so the
share's **cut number** is 4. In alice's 30 GB profile disk
`profiles/alice/ODFC_alice.vhdx`, offset 2 GiB holds chunk `c` through ref `r`,
committed while the cut number was 3 (`born` 3).

1. **10:00 — the cut.** The snapshot service briefly closes a gate in front of
   the share's metadata writes, raises the cut number to 5 in one transaction,
   and reopens it; the gate's target is 50 ms. Nothing is copied, whatever the
   share's size.
2. **10:20 — the overwrite.** Malware on alice-pc encrypts the disk in place,
   64 KiB at a time. When the new bytes at 2 GiB are uploaded, the commit finds
   that `r` was born before cut 5. Instead of replacing `r`, it moves it to
   **history** with `died` 5 and writes the new ref with `born` 5. History refs
   count like live ones, so `c` keeps its count and GC keeps its block.
   Without the snapshot, `r` would simply be replaced, `c`'s count would reach
   zero, and its block would be deleted once GC's 48-hour trash ran out.
3. **Dirty content at the cut.** At 09:59:58 alice-pc wrote 64 KiB at 3 GiB and
   flushed it; at 10:00 it was still only in the journal. At 10:00:02 the
   malware overwrote it. Normally the journal would keep only the newer bytes
   and upload those. The **snapshot hold** makes it keep the 09:59:58 version
   until it is uploaded, straight into history. Snapshot 5 stays `holding` until
   that upload lands, then becomes `complete` and browsable. Before its gate
   closes, the cut commits the share's pending existence — the size and times
   of writes the journal holds but metadata does not yet record — so every
   write acknowledged before 10:00 is in snapshot 5, flushed or not.
4. **10:45 — the undo.** alice opens Previous Versions in Explorer, picks
   10:00, and reads her disk as it was: every ref with `born` < 5 ≤ `died`. The
   operator can also clone snapshot 5 into a new writable share.

```text
               cut 5 (10:00)
                     │
 r → c      ─────────┼──── live until 10:20 ─┐ moved to history,
 (born 3)            │                       │ died 5; c still counted
                     │                       ▼
 r' → c'             │  written 10:20 ──────────► live, born 5
                     │
 snapshot 5 reads every ref with born < 5 ≤ died  →  r → c
 the live share reads the current refs            →  r' → c'

 journal: v@09:59:58 (flushed, not yet uploaded) ─ held at 10:00 ─┐
          v@10:00:02 overwrites it                                │
          upload carries v@09:59:58 straight into history ◄───────┘
          then snapshot 5: holding → complete
```

**The words you need.**

- **Cut number** — a per-share counter raised by one at each snapshot;
  snapshot *k* is the share as of the instant the counter became *k*
  ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree), [RFC 0 glossary](rfc-0-data-lifecycle.md#Glossary)).
- **`born`, `died`** — the cut number a version was committed under, and the one
  its replacement was committed under; snapshot *k* sees a version when
  `born` < *k* ≤ `died` ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)).
- **History** — versions a live snapshot can still see after the share replaced
  them; their refs are counted, which is all that keeps their blocks.
- **Snapshot hold** — the journal keeping a flushed but not yet uploaded version
  that a cut sees until it is uploaded ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)).
- **Use record** — a mark on a snapshot while a clone, restore, backup or move
  reads it; the snapshot cannot be deleted meanwhile ([§3.2](#3.2%20A%20backup%20holds%20its%20snapshot)).
- **Catalog backup / copying backup** — an export of one snapshot's metadata
  to a location outside the metadata store / the same plus a copy of every block
  it names, outside the share's bucket ([§3](#3.%20Catalog%20backups), [§3.4](#3.4%20Copying%20backups)).
- **Namespace** — the folder in the bucket a share's blocks live in; chunks are
  counted within one namespace, and a namespace is what moves between
  installations ([§2.1](#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)).

**What this RFC promises.**

- Taking a snapshot copies nothing and writes a few records, whatever the
  share's size; writes wait only behind a brief per-shard gate.
- A complete snapshot reads back exactly the share's bytes, attributes, ACLs and
  extended attributes as committed at its cut, whatever the share does
  afterwards. Every write acknowledged before the snapshot was requested is in
  it, and within one shard it is crash-consistent: it never holds a write while
  missing one acknowledged before it.
- A snapshot keeps its content alive only through counted refs, and deleting it
  drops only the history no other live snapshot sees. A snapshot under a
  compliance lock cannot be deleted through DittoFS, by anyone, until the lock
  expires.
- Writes are never refused because of a snapshot; when too much content is not
  yet uploaded, the new snapshot is refused instead.
- A catalog backup survives losing the metadata store but not the bucket; a
  copying backup survives losing the bucket too, and at an immutable location
  survives a stolen installation credential as well. No backup survives losing
  the master key that wraps its namespace's export key — every namespace has
  one, encrypting or not — so that master key lives off the host.
  Every export is sealed and authenticated. Moving a namespace to another installation on the same bucket
  copies no block.

**How the rest is organised.** §1 lists the four questions operators ask. §2 is
the core: §2.2–§2.4 how a cut, history and the hold work (read these), then
browsing, clones, schedules and locks, deleting, space reporting, and subtree
snapshots (§2.10, skippable at first). §3 covers catalog backups and restore;
§3.4 on copying backups is long and self-contained. §4 moves a namespace
between installations and re-homes a share out of a shared one. §5 is the
export format, §6 the API and configuration, §7 the invariants, §8–§9 tests and
metrics, §10 the open questions.

## In short

- **A snapshot** is a read-only view of a share as it was at one moment. Taking
  one writes a few records whatever the share's size, and copies nothing.
- **After a snapshot, nothing it can see is overwritten in place.** The first
  change to a record or to a range of content keeps the old version as
  **history**. A history ref counts its chunk in the remote store like a live
  ref, so GC keeps that content for as long as a snapshot can read it.
- **Content still in the journal at the snapshot needs one extra step.** If it
  is overwritten before the journal has offloaded it, the journal keeps the old
  version, under a **snapshot hold**, until it is offloaded straight into
  history. The hold lasts seconds to minutes. The protection that lasts is the
  counted history ref in the remote store.
- **A clone** is a new writable share made from a snapshot. It shares chunks
  with its source in the remote store, and nothing else: either can be deleted
  without affecting the other.
- **A catalog backup** copies one snapshot's metadata out of the metadata store.
  It protects against losing the metadata store. It does not protect against
  losing the bucket, because it holds no data.
- **A copying backup** is a catalog backup plus a copy of the blocks it names, in
  a location outside the namespace's bucket. Each copy puts only the blocks its
  previous one does not already hold, and it survives losing the bucket.
- **A subtree snapshot** cuts only the shards of one subtree, and closes only
  their gates. It coexists with share snapshots.
- **A namespace** is a folder in a bucket, and each share gets its own by
  default. **Migration** hands a whole namespace to another installation on the
  same bucket without copying a block: the target is seeded while the source keeps
  serving, and a short freeze carries only what changed since. **A re-home**
  copies one share's content out of a namespace it shares with others, into a new
  namespace of its own, while the share keeps serving.

## 1. Purpose

Operators ask four things of a file service's data protection, and this document
answers each one:

- **Undo a mistake.** A user deletes a folder, a script truncates a database
  dump, ransomware encrypts a share. The operator, or the user from their own
  client, needs the files as they were an hour or a day ago, without a restore
  ticket. Snapshots taken on a schedule, browsable in place over NFS and through
  SMB's Previous Versions ([§2.5](#2.5%20Browsing%20a%20snapshot)), and locked against deletion
  ([§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)), answer this.
- **Copy production for development, test or analytics without paying for it
  twice.** A clone ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)) is a writable share that starts as a snapshot's
  exact contents and stores only what it changes.
- **Survive losing the metadata store.** The bucket is a replicated, durable
  service, but the metadata store is software the operator runs, and a bad
  upgrade or a lost cluster can destroy it. Metadata is a small fraction of the
  data, so copying it elsewhere is cheap; a catalog backup ([§3](#3.%20Catalog%20backups)) is that
  copy, and restoring it brings the files back from the blocks already in the
  bucket. When the bucket itself must not be a single point of loss, a copying
  backup ([§3.4](#3.4%20Copying%20backups)) also copies the blocks to a location of its own.
- **Move tenants between installations.** Hardware refresh, rebalancing, or
  outgrowing a single-node installation all mean moving shares that can hold
  petabytes. Because both installations can reach the same bucket, a move
  ([§4](#4.%20Moving%20a%20namespace%20between%20installations)) transfers the metadata and the right to collect garbage, and never
  the data. A share that shares its namespace with others is first re-homed into
  one of its own ([§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), which does copy its data.

> decision: snapshots, backups and moves stay one document, at its length,
> because they share one set of records — cuts, use records, exports, claims —
> and a rule changed in one place is checked against the others where they sit
> side by side. Split [§3.4](#3.4%20Copying%20backups) and [§4](#4.%20Moving%20a%20namespace%20between%20installations) into documents of their own when one
> of them gains an implementer who does not need the rest.

All four are composed from what the set already has: counted history refs
([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)), clone by adoption
([RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy)), restore by adoption
([RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)), replicated journals
([RFC 10](rfc-10-journal-replication.md)) and GC as one service per namespace
([RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix)).

### 1.1 Non-goals

This document **MUST NOT** be read as specifying:

- a replica of the block store kept in step with it: a copying backup
  ([§3.4](#3.4%20Copying%20backups)) copies one snapshot's blocks when it runs. A namespace's own bucket
  runs with versioning and object lock off, which a namespace store requires
  ([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)), and so with no service-side replication that depends on
  versioning; this set's protection against losing that bucket, or against a
  holder of its credential, is a copying backup at an immutable location
  ([§3](#3.%20Catalog%20backups));
- a backup of master keys: an export carries the namespace's own keys only
  wrapped under a master key, and never a master key
  ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys));
- two installations serving one namespace at once. That is replication and
  sharding ([RFC 10](rfc-10-journal-replication.md), [RFC 11](rfc-11-ownership.md)); [§4.6](#4.6%20After%20replication)
  says how the two relate;
- how a protocol presents a snapshot beyond what [§2.5](#2.5%20Browsing%20a%20snapshot) requires;
- restoring in place: a restore makes a new share ([§3.3](#3.3%20Restore)), and swapping it
  in is a rename in the control plane;
- snapshots of a directory that is not a shard's root: a subtree snapshot
  ([§2.10](#2.10%20Subtree%20snapshots)) cuts whole shards.

### 1.2 Terms

Share, namespace, installation, shard, primary, replica, node lease, epoch,
journal, offload, ref, chunk, block, cut and snapshot are defined once, in
[RFC 0's glossary](rfc-0-data-lifecycle.md#Glossary). This document adds:

| Term | Means |
| --- | --- |
| **cut number** *k* | a share's count of snapshots taken, share and subtree alike, raised by one at each cut ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)). `Cut(share)` holds the latest *k* and *klatest*, the newest cut a live share snapshot still holds (0 when none does); a subtree snapshot's is kept per shard, in `SubCut` ([§2.10](#2.10%20Subtree%20snapshots)) |
| **covers** | a cut covers a shard when a version of that shard's files can be seen by it: a share cut covers every shard of the share, a subtree cut its **covered set** ([§2.10](#2.10%20Subtree%20snapshots)) |
| **born**, **died** | the cut number a version was committed under, and the born of the version that superseded it ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)) |
| **versioned record** | a record a snapshot reads — File with its FileData, entry, ACL, xattr, stream link, hole, directory delta, ref — carrying `born` |
| **history** | versions that a live snapshot can still see after the share replaced them, kept with their `died` |
| **cut gate** | the per-shard gate at its primary that orders every transaction writing a versioned record before or after a cut ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)) |
| **snapshot hold** | the journal keeping content the cut sees until it is offloaded; its **hold mark** is, per file, the version the file's existence had committed up to at the cut ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)) |
| **hold record** | the metadata-store record, one per shard, saying that a shard's journals still hold content of cut *k* not yet offloaded ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)) |
| **use record** | a durable record that a clone, restore, catalog backup or move is reading a snapshot, which blocks its deletion ([§3.2](#3.2%20A%20backup%20holds%20its%20snapshot)) |
| **change sequence** | a value every transaction stamps on each record it writes under a share's prefixes, or its namespace's content-addressed ones, ordered like the commits; a move's delta is the records stamped above the base's ([§4.2](#4.2%20The%20move%2C%20step%20by%20step)) |
| **lock** | an expiry time before which a snapshot cannot be deleted by anyone ([§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)) |
| **export**, **import** | a self-describing stream of metadata records ([§5](#5.%20The%20export%20format)), and building records from one, staged and published at once |
| **claim** | the control object in a namespace's folder naming the one installation that may write and collect there ([§4.1](#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)) |
| **copying backup** | a catalog backup that also copies the blocks it names into a **block folder** at its location; its **manifest** lists the folder blocks it needs ([§3.4](#3.4%20Copying%20backups)) |
| **sweep** | the deletion, at a block folder, of every block no retained manifest lists ([§3.4.4](#3.4.4%20Expiry%20and%20the%20sweep)) |
| **re-home** | copying one share's content from the namespace it shares into a new namespace of its own, while it serves; a ref names its namespace by **generation** during one ([§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |
| **detached snapshot** | a complete snapshot whose share was deleted: the share is kept **retired**, serving nothing, so that its snapshots stay restorable ([§2.8](#2.8%20Deleting)) |
| **location record** | a backup location's configuration: where it is, its **mode** (`mutable` or `immutable`), its credential reference and its lifecycle age ([§6.2](#6.2%20Configuration)) |

## 2. Snapshots

### 2.1 A namespace is the unit that moves

A namespace is a folder — one prefix — inside a bucket of the remote tier, with
one key scope ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)). Several namespaces **MAY** share a bucket, and a
catalog backup location ([§3.1](#3.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)) **MAY** be another folder of that same
bucket: each has its own prefix, so their object names never collide. Within a
namespace chunks are shared by refs and counted; across two, never
([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)), and GC runs one service per namespace.

Which shares share a namespace:

- **By default, each share is created with a namespace of its own.** It then
  deduplicates only against itself, and can be moved, backed up and collected
  alone.
- **A clone or a restore joins its source's namespace.** That is what lets it
  count its source's chunks instead of copying them ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)).
- **Creating a share into an existing namespace is deferred with
  deduplication.** Its only purpose is to deduplicate against the shares already
  there, which needs the dedup lookup that is deferred
  ([RFC 0 §3.1](rfc-0-data-lifecycle.md#3.1%20Deduplication)); until it returns, an operator cannot do it, and the only
  shares that share a namespace are clones and restores of its shares.

Grouping trades mobility for sharing chunks. A chunk counted by two shares must be
counted in one metadata store, so whatever changes which installation counts a
block acts on the whole namespace: a migration ([§4](#4.%20Moving%20a%20namespace%20between%20installations)) moves every share,
snapshot, retired block and put intent in it. A share leaves a namespace it
shares with others only by a re-home ([§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), which copies its content into a
namespace of its own; otherwise a share's binding to its namespace is fixed while
it holds content ([RFC 13 §5.1](rfc-13-configuration.md#5.1%20A%20bound%20setting%20refuses%20change)).

### 2.2 A snapshot is counted content and a frozen tree

A snapshot is its **cut number** *k*. Each share keeps one `Cut(share) = { k,
klatest, cut time, deleting }` record — its cut time the newest share cut's
([§2.5](#2.5%20Browsing%20a%20snapshot)) — and one `LiveCut(share, k)` per live snapshot
([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). Nothing else is written when a snapshot is taken: what it
sees is read from records the share already keeps, each stamped with a cut.

**`born` is the cut its version was committed under.** Every transaction that
writes a versioned record reads `Cut(share)` behind the cut gate
([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)) and stamps what it writes with `born = k`. For content that
arrives through the journal, the committing transaction is the **existence
commit** that makes the version part of the file
([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal)), not the append: a version acknowledged before
cut *k* whose existence commits after it is not in snapshot *k*, as a size change
committed after the cut is not. The journal keeps each existence commit's cut
with the versions it covered — the **stamp**, a replicated operation that travels
with its versions ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)) — and the offload commit copies it into the ref it
writes. Every version committed before cut *k* therefore has `born < k`, and
every later one `born ≥ k`.

**An acknowledged write is in a snapshot only if its existence committed before
the cut.** Snapshot *k* **MUST NOT** contain, and **MUST NOT** be described as
containing, a write whose existence commit came after cut *k*.

**The gate closes first, then the cut points are taken, then pending existence
commits behind the closed gate, so every write acknowledged before the cut points
is in the snapshot and nothing after them is.** When a primary is asked to close
for cut *k* ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate), step 2), it first closes the shard's gate and waits for
the transactions already admitted to finish. Only then does it take its final
**cut point** in every journal holding the shard's files — the position below
which lies every append it has acknowledged — at one instant of its
acknowledgement order, holding acknowledgements for the few reads that takes. It
then commits the shard's pending existence up to those points, in journal order
([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)), syncing each file first, as group commits that join each
journal's next group commit rather than running beside it, all within
`snapshots.gate_max`. That covers an NFS `UNSTABLE` write not yet committed, an
SMB write not yet flushed, and a stable write whose size and times waited for
their existence commit ([RFC 6 §3.4](rfc-6-block-metadata.md#3.4%20Ordering%20against%20the%20journal)). These pre-cut commits, and the
recovery commits of a primary that started with its gate closed
([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate), step 1), are the primary's own and **pass its own closed gate**;
they read cut *k* − 1, since `Cut(share).k` rises only at step 3, so their
versions take `born < k`.

From the cut point until the gate reopens, **no existence commit of any kind**
lands above the cut point for the shard's files: not a group commit, not a
removal's first phase ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)), not an offer capture, not an explicit
time set. Every one of them except the group commit is a transaction that waits
at the closed gate anyway; the group commit is held by the primary. So snapshot
*k* holds exactly the shard's writes acknowledged before its cut points: a write
acknowledged later is never in it, and an earlier one never missing. Because the
gate closed before the cut points were taken, no namespace transaction can slip
between a write after the cut point and the cut: an application that writes and
flushes `~tmp1` and then renames it over `report.docx` either finished both
before the gate closed, and the snapshot shows the new `report.docx` with its
bytes, or has its rename waiting at the gate, and the snapshot shows the old
`report.docx`. Never a zero-length one, which no crash produces.

- **Within a shard the snapshot is crash-consistent:** it is the shard as a
  crash at the cut points would have left it, recovered. On a single node a
  share is one shard ([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)), so the whole snapshot is.
- **(cluster)** Across shards each primary closes and takes its cut points when
  its own close request arrives, so a write acknowledged on one shard can be in
  the snapshot while a write another shard acknowledged slightly earlier, after
  its own points, is not. Every write acknowledged before the snapshot was
  requested is still in it.
- **What waits, and for how long.** Data writes never wait at the gate: an append
  is acknowledged from the journal and commits no transaction. A namespace
  operation, or any other transaction writing a versioned record, waits at the
  closed gate for about one group commit and one sync per journal, plus step 3's
  transaction. A stability reply for an overwrite above the cut point
  ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)) waits until the gate reopens. Both are bounded by the cut's
  deadline and by `snapshots.gate_max` ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)).
- **A stalled store refuses the cut.** A pre-cut commit that cannot commit within
  `gate_max` aborts the cut ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)), the gate reopens, and the policy skips
  that tick ([§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)); writes are not held for it beyond the bound.

> decision: the gate closes before the cut points are taken and the pre-cut
> commit runs behind it, because a pre-cut commit run with the gate open admits
> namespace transactions, removals, offer captures and time sets that read cut
> *k* − 1 against content committed after the cut point — a state no crash
> produces. The cost is that namespace operations wait at the gate for one group
> commit and one sync per journal; data writes never do. Across shards the
> snapshot is consistent per shard only; take every shard's cut points behind one
> coordinator barrier if a multi-shard application needs cross-shard crash
> consistency.

> ponytail: the whole pre-cut commit runs behind the closed gate. If gate-close
> time measures too long, upgrade to two steps: with the gate still open,
> bulk-commit pending existence up to provisional points; then close the gate,
> take the final cut points, and commit only the remainder between the
> provisional and final points behind it.

**`died` is the successor's `born`.** When a transaction supersedes a version —
an overwrite's offload commit, a truncate or deallocate, a release, a rename, a
`chmod`, an ACL or xattr change, a fold of directory deltas — the old version's
`died` is the new version's `born`. For a removal it is the cut the removal's
first phase read ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)), whichever later batch drops the ref — **except**
a part of a ref that an overwrite record above the ref's `newest` covers: that
part was superseded when the overwrite's existence committed, so it dies at the
`born` of the lowest-version such overwrite, history records included, and is
moved to history as its own piece (RFC 6 §6.2's exception). A held version
offloaded straight to history is dated by the same rule. Stamping
`died` from whichever transaction happens to write it instead would let two
versions of one range both look alive at one cut ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history) shows how).

**What a snapshot sees.** Snapshot *k* sees, for each key or range, the version
with `born < k ≤ died`, where a live version has `died = ∞`.

**A superseded version is kept only if a live cut sees it.** The transaction that
supersedes a version **MUST**, in that transaction, either:

- move it to history, when some live cut *c* has `born < c ≤ died`. A history ref
  counts its chunk like a live one: a chunk's refcount is its live refs plus its
  history refs ([RFC 9 §2.1](rfc-9-gc.md#2.1%20References%20are%20the%20only%20authority)), so the move changes no count; or
- drop it as the share would with no snapshot, decrementing a ref's chunk.

Once subtree snapshots exist, "live cut" in this rule and in [§2.8](#2.8%20Deleting) means a
live cut that covers the version's shard ([§2.10](#2.10%20Subtree%20snapshots)); every share cut covers
every shard, so a share with no subtree snapshot reads the rule as written.

In the common case `died` is the current *k*, and the test is `born < klatest`.
When `died` is older — a held version offloaded late ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)), a removal
batch — the transaction reads the `LiveCut` records in `(born, died]`. Either
way a transaction that moves a version to history **MUST** read the `LiveCut`
record that justifies the move — `klatest`'s, or the one it found in the
interval — with conflict tracking, retesting against the live cuts if it is
already gone, so a deletion that removes that cut
([§2.8](#2.8%20Deleting)) conflicts with it and one of the two retries: a history version
never lands behind a deletion's walk with no live cut to see it. The in-place
path needs no such read: a deletion only lowers `klatest`, which leaves a version
with `born ≥ klatest` unseen by every live cut. Two versions of one
key never share a `died`: if they did, the older one's successor would be the
newer one, born at that same cut, and the newer one would be visible to no cut
and dropped. History keys therefore never collide.

![Taking a snapshot across two shards](img/rfc12-cut.svg)

For example: a file is written and offloaded while the share's cut number is 0,
so its ref and its File have `born` 0. Snapshot 1 is taken; `klatest` is 1. The
file is then `chmod`ed, overwritten and offloaded. The `chmod` reads cut 1,
finds the File's `born` 0 below `klatest`, and moves the old File to history with
`died` 1 as it writes the new one with `born` 1; the offload commit does the same
with the ref. Snapshot 1 reads the old File and ref, since 0 < 1 ≤ 1, and not
the new ones, since 1 < 1 is false. Had no snapshot existed, both commits would
have replaced in place, and the offload would have decremented the old chunk.

**Capture is copy on first write.** Nothing is copied when a snapshot is taken.
The first transaction after the cut that supersedes a version the cut sees moves
it to history; later transactions on the same key find `born ≥ klatest` and
write in place. So a snapshot costs one record to take, and each record changed
afterwards costs one history write, once per live cut it crosses.

**Where history lives.** A versioned record's history sits under its file's
history prefix, keyed by the record's key and then `died`
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)); refs keep their own `History(file, died, offset)` records
([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). A snapshot read of one record is a live read and, when
the live version is too new, one seek into history; a snapshot listing merges a
directory's live entries with its history entries in key order. Each history
version also writes one empty key in the share's **died index**, ordered by the
shard the version was superseded in and then by `died`, so that deleting a
snapshot visits only the history that died in its interval, in the shards its
cut covers ([§2.8](#2.8%20Deleting), [§2.10](#2.10%20Subtree%20snapshots)).

**Every record a snapshot read consults is versioned** — the File and its
FileData fields, entries, holes, directory deltas, the ACL, xattrs and stream
links. A record left unversioned would leak: an ACL replaced after the cut would
judge the snapshot's content by the new rules and let a denied principal read
it. A file released after the cut keeps its records and its content through the
history its release wrote.

**Directory deltas need no special case.** A delta is a versioned record, born at
the cut it was written under ([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps)). The fold that consumes it deletes it and
rewrites the directory's File, and both send the old values to history when a
live cut sees them. Snapshot *k* reads the directory version it sees plus the
deltas it sees: a delta folded before *k* is inside that version and has
`died < k`; one folded at or after *k* is not, and has `born < k ≤ died`. No delta
is applied twice or missed.

**Journal versions do not order a share.** Each journal numbers its own files
([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)), and a share's files sit in several nodes' journals; the cut
number, one record every committing transaction reads, orders them.

**Nothing else keeps a snapshot's blocks alive** — no manifest, hold list or extra
GC root ([RFC 9 §2.4](rfc-9-gc.md#2.4%20A%20snapshot%20holds%20its%20blocks%20without%20a%20pin), [RFC 7 §4.4](rfc-7-namespace-metadata.md#4.4%20There%20is%20no%20third%20holder)). A snapshot hold keeps
journal bytes, never a block; a use record or a lock keeps a snapshot from being
deleted, and the snapshot's counted refs do the rest.

> ponytail: a snapshot read scans a record's or a file's history at each key or
> offset it resolves, O(history versions of it). Upgrade to an interval index
> over `(born, died]` when snapshot reads of a much-changed file or directory
> show in a profile.

A snapshot is in one of five states: `cutting` while its cut is being taken,
`holding` while hold records remain, `complete`, `failed` or `deleting`. Only a
complete one is browsed, cloned, backed up or restored. A failed one's records are
removed as a deletion removes them ([§2.8](#2.8%20Deleting)).

### 2.3 The cut is one transaction behind a brief gate

A share can span several shards, each with its own primary on its own node
([RFC 11 §2](rfc-11-ownership.md#2.%20Shards)). On a single node a share is one shard and the node is its primary
([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)), so the four steps below run with one gate and no remote round
trip; a rule or example marked **(cluster)** in this document binds only where a
second storage node can serve a share. A cut must split every one of them at the same point: every
transaction on every shard is either before it, and reads the old cut number, or
after it, and reads the new one. Each primary therefore keeps a **cut gate** for
each of its shards. A transaction that writes a versioned record **MUST** pass
the gate before it takes its read snapshot or timestamp. Reads never wait at a gate.

**(cluster)** **A transaction that spans shards** ([RFC 11 §8.1](rfc-11-ownership.md#8.1%20Operations%20across%20shards)) **MUST** be admitted at
all its shards' gates or at none: it takes them in shard-ID order, and on meeting
a closed one releases those it already took, waits at the closed one, and starts
again from the first. A closing gate therefore waits only for transactions
admitted everywhere they write, which wait on no gate, and a cut never waits on a
transaction that waits on the cut. A prepared cross-shard transaction stays
admitted until it commits or its prepare's hold deadline ends it
([RFC 11 §8.1](rfc-11-ownership.md#8.1%20Operations%20across%20shards)), which bounds how long it keeps a gate from closing.

The **coordinator** is the control plane's snapshot service. Taking snapshot *k*
is four steps:

1. **Announce.** The coordinator commits the snapshot record in state `cutting`,
   with *k* and a deadline in store time, `snapshots.cut_deadline` after it.
   While a `cutting` record exists, any primary that starts serving one of the
   share's shards — after a takeover, a handover, a move of files or the
   creation of a shard — **MUST** start with that shard's gate closed. Its own
   recovery commits, and the pre-cut commit of step 2 when it is asked to close,
   pass that closed gate ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)); it answers step 2 only once both have
   committed, so a write acknowledged before the request is never missing.
2. **Close and hold.** The coordinator asks the primary of every shard of the
   share, as the shard records name them, to close. Each primary first closes
   its gate: new transactions wait at it, and those already admitted finish. It
   then takes its final cut points and commits the shard's pending existence up
   to them behind the closed gate, within `snapshots.gate_max`
   ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)). It then
   records the shard's snapshot hold for *k* ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)) as a replicated
   journal operation, durable on every replica of the shard
   ([RFC 10](rfc-10-journal-replication.md)), and replies with the shard's epoch, whether the
   hold covers any content not yet offloaded, and the shard's held and dirty
   bytes — or refuses with `ErrHoldBacklog` under the hold bounds
   ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)), releasing the hold.
3. **Cut.** One transaction reads every shard record of the share with conflict
   tracking and requires each epoch to be the one its primary replied with; raises
   `Cut(share).k` to *k* and `klatest` to *k*; writes `LiveCut(share, k)` with the
   snapshot's ordinal ([§2.5](#2.5%20Browsing%20a%20snapshot)) and one hold record per shard that reported
   content not yet offloaded; stamps the
   snapshot's **cut time**, the later of the store's time and one second past the
   latest cut time of any snapshot it will be visible beside — for a share cut,
   the share's previous cut time, which `Cut(share)` keeps, and every covered
   shard's `SubCut` cut time ([§2.10](#2.10%20Subtree%20snapshots)); and moves the snapshot to
   `holding`, or to `complete` if there is no hold record. It commits no existence
   itself — step 2 did that — and writes no per-file record.
4. **Open.** The coordinator tells every primary to reopen. Every later
   transaction reads cut *k*.

**Aborting.** A cut that cannot commit by its deadline — a primary unreachable, a
gate slow to drain, an epoch that changed — is aborted: the coordinator deletes
the `cutting` record, reopens the gates, and each primary releases its hold for
*k*. If the coordinator itself fails, each primary reopens its gate at the
deadline, but only by committing the abort: a transaction deleting the `cutting`
record, which conflicts with the cut transaction, so exactly one of the two
commits. A primary whose abort fails rereads `Cut(share)`: `k` at *k* means the
cut committed, and it opens as after step 4; otherwise another primary's abort
won, and it releases its hold for *k* and opens. A journal hold
for a cut that has neither a `LiveCut` nor a `cutting` record is released at
start and by a periodic check.

**Why each rule.** Without the epoch check, a shard whose primary changed after
step 2 would have a new primary with an open gate and a cached cut number; a
transaction it admitted could commit after the cut stamped with the old one, and
snapshot *k* would show a post-cut change. Without the `cutting` record, a new
primary would not know to start closed. Without the hold replicated before the
cut, a takeover right after the cut would lose it ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)).

**The gate is bounded, not just short.** It is held for the transactions already
in flight, the pre-cut commit — about one group commit and one sync per journal —
step 3's one transaction and the round trips around them, never for an
offload. One bound makes that a ceiling rather than a hope:

- **drain.** A primary that closes its gate stops admitting and waits for its
  admitted transactions to finish, for as long as the ceiling below allows. A
  group commit of up to `G` files and an offload commit within the key budget K ([RFC 6 §5.2](rfc-6-block-metadata.md#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed)) are
  admitted transactions too, and under sustained offload one of them is nearly
  always in flight; a drain bound shorter than such a commit would refuse every
  cut. A transaction still running when the ceiling arrives — a prepared
  cross-shard transaction near its hold deadline, a stalled commit — ends the cut
  by the ceiling's abort; the coordinator retries after a backoff, counted;
- **ceiling.** A primary whose gate has been closed for `snapshots.gate_max`
  reopens it by committing the abort, as when the coordinator fails, whatever the
  coordinator is doing. So no write waits at a gate longer than `gate_max`,
  whatever the number of shards, round trips, slow transactions or retries.

A cut that cannot fit inside the bound is refused, never stretched; the
snapshot policy skips that tick ([§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)), and three consecutive skipped ticks of one
policy, for any reason, raise a health condition naming the share and the
reason of the last refusal, so a policy that silently stopped taking snapshots
is seen within three periods. `Cut(share).k` and `klatest` rise only at a cut, behind the gate, so the
transactions that read them need no conflict tracking on them; a deletion lowers
`klatest`, which the history path guards against by its tracked `LiveCut` read
([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)).
Every share has a remote tier, so every share can be snapshotted.

> ponytail: the cut transaction reads every shard record of the share, O(shards).
> A share of 10⁴ per-child shards makes that one large transaction and 10⁴ gate
> round trips. Upgrade to a per-share epoch summary that every shard-record change
> also writes, when a cut of a share with many shards misses the gate target.

**A removal in flight at the cut** ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)) needs no special case. If its
first phase committed before the cut, the FileData and holes the snapshot sees
already record the removal, and its later batches date the refs they drop at
the removal's cut, or at an earlier overwrite's `born` under RFC 6's exception
([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)), below *k* either way: the snapshot does not see them. If
its first phase commits after the cut, the snapshot reads the refs it has not yet
dropped, or their history.

**(cluster)** **Worked example: a cut across two shards while one fails over** —
`S-snap-cut-failover`. Share `data` has shards S1 (primary A, epoch 6) and S2
(primary B, epoch 7, replicas C and D). `Cut(data)` is `{k 3, klatest 3}`.

| t | coordinator | S1 at A | S2 | metadata store |
| --- | --- | --- | --- | --- |
| 0 | announce snapshot 4 | — | — | snapshot 4 `cutting`, deadline T+5 s |
| 1 | ask A, B to close | closes gate; cut points; pre-cut commit behind it; Hold(4) on A's replicas; replies e6 | B closes gate; cut points; pre-cut commit behind it; Hold(4) durable on B, C, D | S1, S2 pending existence committed, `born` 3 |
| 2 | — | holds gate | B's node lease lapses before it replies | — |
| 3 | — | — | C takes over at e8 ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover)); sees `cutting`, starts closed; has Hold(4); its recovery commits pass its own closed gate | S2: e8, primary C |
| 4 | asks C | — | C takes final cut points, commits pending existence behind its own gate, replies e8 | S2 remainder committed, `born` 3 |
| 5 | cut transaction | — | — | reads S1 e6, S2 e8: match; k 4, LiveCut 4, hold records S1, S2; `holding` |
| 6 | open | opens | C opens | — |

Had the coordinator sent step 5 with B's epoch 7, the transaction would have
failed on S2's record and been retried; had C started with an open gate, a
`chmod` it admitted at t3 could have committed at t5 with `born` 3 over a File
of `born` 3, replacing it in place, and snapshot 4 would have shown it. A
transaction B admitted before its lease lapsed cannot commit after t3 either:
every fenced commit takes a shared guard on its primary's node lease record, and
C's takeover writes that record first ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)), so B's late commit
conflicts and aborts. On a single node no takeover exists, and that guard is not
taken ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit)); the per-file fence records still order every commit. Had C's
closed gate also held its own recovery and pre-cut commits, C could only have
refused the close, or replied with writes B acknowledged before the request
missing from snapshot 4.

### 2.4 A snapshot hold bridges dirty content to history

At the cut, some of the share's content is dirty: acknowledged, part of the file
by an existence commit before the cut, but not yet offloaded. Snapshot *k* must
show it. Two cases:

- **It is not overwritten before its offload.** Nothing special happens. The
  offload writes an ordinary live ref with `born < k`, and from then on that ref
  counts its chunk in the remote store; if a later change supersedes it, it moves
  to history like any ref. The journal releases its copy as usual.
- **It is overwritten, truncated, deallocated or released after the cut, before
  its offload.** Now the journal alone holds the bytes snapshot *k* needs, and by
  its own precedence rule it would let the newer version replace them
  ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)): the offload would ship only the newer bytes, and the
  snapshot's would exist nowhere. The **snapshot hold** prevents that. The
  journal keeps every version at or below the cut's hold mark until an offload
  has committed it, and the offload writes a superseded one **straight to
  history**, with its own `born` and, as `died`, its successor's `born`.

**The mark is committed existence, and so is superseding.** The cut transaction
commits no existence ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)); the pre-cut commit before it carried every
write below the cut points ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)). So what snapshot *k* shows of a file is
what its existence had committed up to when the cut committed, and the hold mark
is exactly that version — the file's `applied` at the cut, not the newest
version the journal holds. A version acknowledged after the cut points, whose
existence commits after the cut, lies above the mark, takes `born ≥ k`, and is
not in the snapshot. For the
version below it to still be in the journal when `Hold` runs, the journal
**MUST** treat an existence-committed version as superseded — for release,
compaction and the offload's superseded flag — only once its successor's
existence has committed ([RFC 1 §3.11](rfc-1-journal.md#3.11%20Snapshot%20holds)). Until then it offloads as a live
version, and the successor's own offload later moves it to history with the
successor's `born` as `died`. Marking the journal's newest version instead would
not help: a successor appended before the cut would already have replaced the
committed version, and the hold would keep bytes no snapshot shows.

So the lasting protection of a snapshot's content is always a counted ref in the
remote store. The hold only bridges the time between the cut and the offload,
which the offload's normal pace bounds.

![A snapshot hold](img/rfc12-hold.svg)

**Worked example: an overwrite after the cut** — `S-snap-hold-overwrite`. One
file range, one shard.

| t | journal (primary and replicas) | metadata store | remote store |
| --- | --- | --- | --- |
| 0 | v1 offloaded and released | r1 live, `born` 0 → c1 | c1, count 1 |
| 1 | v2 written; its existence commits under cut 0: `born` 0 | r1 live | c1 |
| 2 | cut 1: Hold(1) marks v2, on every replica | `Cut {1, 1}`, `LiveCut 1`, hold record; snapshot 1 `holding` | c1 |
| 3 | v3 overwrites v2; v2 superseded, kept by the hold | v3's existence commits under cut 1: `born` 1 | c1 |
| 4 | offload carries v2 and v3 | v2 committed first: r1 → `died` 0, seen by no cut, dropped; h2 history `born` 0 `died` 1 → c2. Then v3: r3 live `born` 1 → c3 | c1 count 0, left to GC; c2, c3 count 1 |
| 5 | v2 and v3 released; hold record deleted | snapshot 1 `complete` | — |

Snapshot 1 reads h2 (0 < 1 ≤ 1); the share reads r3. Had v3 committed first, r1
would have taken `died` 1 from it and been kept in history beside h2 — two
versions of one range visible to snapshot 1. The ordering rule below forbids it.

**Worked example: a successor appended before the cut** —
`S-snap-hold-uncommitted-successor`. One file range, one shard.

| t | journal (primary and replicas) | metadata store | remote store |
| --- | --- | --- | --- |
| 0 | v1 offloaded and released | r1 live, `born` 0 → c1 | c1, count 1 |
| 1 | v2 written; its existence commits under cut 0: `born` 0, `applied` v2 | r1 live | c1 |
| 2 | close for cut 1: gate closed, then cut point taken above v2; pre-cut commit has nothing left to carry; v3 written over v2 and acknowledged above the cut point, so its existence waits for the gate to reopen and v2 is not superseded | r1 live | c1 |
| 3 | cut 1: Hold(1) marks v2, the file's `applied`, on every replica | `Cut {1, 1}`, `LiveCut 1`, hold record; snapshot 1 `holding` | c1 |
| 4 | gate open; v3's existence commits under cut 1: `born` 1; v2 now superseded, kept by the hold | `applied` v3 | c1 |
| 5 | offload carries v2, then v3 | r1 → `died` 0, dropped; h2 history `born` 0 `died` 1 → c2; r3 live `born` 1 → c3 | c1 count 0; c2, c3 count 1 |

Snapshot 1 reads h2: v2 was the file's content at the cut point. Had v3 been
acknowledged before the cut point, the pre-cut commit would have committed it
under cut 0 and snapshot 1 would read v3. Had the journal let v3 replace v2 at
t2, the hold at t3 would have found nothing below its mark and snapshot 1 would
have read r1, content the share had already replaced before the cut.

**Rules.**

- **A hold is replicated and inherited.** The hold for *k* **MUST** be durable on
  every replica of the shard before its primary replies to step 2, and a
  superseded version it keeps **MUST** stay on every replica until offloaded.
  Stamps ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)) are replicated operations too, so a version's `born`
  survives with it. Takeover and handover ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover), [RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover))
  therefore inherit holds and stamps. A stamp its primary had not yet replicated
  when it failed is rebuilt from metadata ([RFC 1 §3.11](rfc-1-journal.md#3.11%20Snapshot%20holds)); that is at most
  the file's latest, since a file's existence commits are serialised, and the File
  versions the rebuild reads are never dropped from under it: a drop means no live
  cut lies between the two candidate cuts, so either gives the same visibility.
- **(cluster)** **A hold follows its files between shards.** A move of files between shards
  ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) **MUST** ship held versions with their stamps and hold marks.
  The receiving primary re-versions moved content on arrival under its own epoch
  ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)), held superseded versions included, keeping each file's version
  order; each held version keeps its stamp, and its hold mark follows it to its
  new version. The transaction that commits a move batch **MUST** write the
  receiving shard's hold record, `S‖id‖hr‖cut‖shard`
  ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), for every cut whose held versions it ships, so a hold record
  names the shard now holding the content before the giving shard's journal lets
  it go; the giving primary then deletes its own
  record only by the completion rule below.
- **A held version commits first.** An offload **MUST** commit a held superseded
  version no later than, and in version order before, any newer version over the
  same range, so that each version's successor is the one that really replaced
  it.
- **A ref never straddles a live cut.** An offload **MUST NOT** place, in one
  ref, versions on both sides of the hold mark of a live cut; the carver's run
  is split there ([RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline)). Between two consecutive live marks a ref takes
  the highest `born` of its versions, which no live cut can tell apart.
- **A late version tests the live cuts.** A held version that commits after its
  snapshot was deleted finds no live cut in `(born, died]` and is dropped
  ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)): its chunks are left to GC like any chunk no ref names
  ([RFC 9 §2.2](rfc-9-gc.md#2.2%20Retirement%20is%20decided%20where%20the%20count%20reaches%20zero)).
- **Completion is decided in metadata.** A shard's primary deletes its hold record
  for *k* once its journal holds no version at or below *k*'s mark that is not
  offloaded. When no hold record of *k* remains, one transaction marks the snapshot
  `complete`. A primary that finds a hold record for its shard but no hold for *k*
  in its journal — every copy was lost — **MUST** mark the snapshot `failed`. A
  snapshot is never `complete` with bytes missing.
- **Holds are bounded, counting what the cut itself will hold.** The new hold can
  keep every dirty byte at the cut, so each bound counts held bytes plus dirty
  bytes not yet offloaded. A cut **MUST** be refused with `ErrHoldBacklog` when
  that sum for the share exceeds `snapshots.hold_bound`, or for any journal
  holding the share's files, summed over every share the journal carries
  ([RFC 1 §7](rfc-1-journal.md#7.%20Capacity)), exceeds `snapshots.hold_journal_fraction` of its capacity.
  Each primary evaluates the journal bound at step 2 for its own journal and for
  each replica's, from the held and dirty bytes every replica returns when it
  acknowledges `Hold` ([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)); the coordinator sums the share
  bound from the primaries' replies before step 3. With the remote tier down,
  snapshots stop before the journal fills; writes never stop for a snapshot.

> decision: a snapshot becomes browsable only once its holds are drained, so a
> snapshot read, a clone and a backup never name journal content and never reach
> another node's journal. The cost is a delay between the cut and `complete`,
> bounded by the offload of the share's dirty bytes at the cut. Serve held content
> from the journal if that delay is ever the complaint.

### 2.5 Browsing a snapshot

A complete snapshot is reachable read-only under a virtual directory at the
share's root, configurable and hidden from listings. What protocols rely on:

- a handle into a snapshot names the share, the snapshot and the file
  ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)), so it never resolves to a live file; once the snapshot is
  deleted it resolves stale ([RFC 7 §6.3](rfc-7-namespace-metadata.md#6.3%20Staleness%20is%20reported%2C%20never%20guessed));
- a read resolves by the covering lookup ([RFC 6 §8.1](rfc-6-block-metadata.md#8.1%20Covering%20lookup)) at the cut: the
  file's refs with `born < k ≤ died`, live and history, against the FileData and
  holes the cut sees, which take precedence over any ref, and fetches from the
  remote tier;
- permissions are evaluated against the file, ACL and share grant as the cut sees
  them — the ACL versioned with the file ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)), the grant as it is now,
  so a principal removed from the share reaches none of its snapshots;
- every mutating operation fails with the read-only error;
- over NFS, each snapshot is its own filesystem: the browse directory's entry
  for a snapshot reports an `fsid` derived from the share and the cut, and the
  `mounted_on_fileid` of the browse directory, so `find -xdev`, `du` and backup
  tools that stay on one filesystem do not descend into every snapshot. Over
  SMB each snapshot reports its own volume serial, derived from the share and
  the snapshot's ordinal, so a client that keys its caches or hard-link
  detection on volume and file id never takes a file in one snapshot for the
  same file in another, or in the live share;
- each snapshot carries an **ordinal**, a value below 2¹⁵ assigned by the cut
  transaction ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)). It **MUST** be unique among the snapshots visible at
  any one path: every live share snapshot, and every live subtree snapshot whose
  root lies on the same chain of ancestors as the new one's ([§2.10](#2.10%20Subtree%20snapshots)).
  Two subtree snapshots of disjoint subtrees never show the same file, since a
  file's `Number` is unique in its share, so they **MAY** share an ordinal. The
  cut takes the smallest such value that is not **quarantined**: an ordinal
  stays unusable for 24 h after the snapshot holding it is deleted, so a client
  that cached a deleted snapshot's volume serial or file ids does not meet them
  again on a new snapshot. A snapshot file's 64-bit file id carries the ordinal
  and the file's `Number`, and its 128-bit id the share and the cut
  ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)): they differ from the live file's and from the same file's in
  every other snapshot visible beside it, so tools never take two of them for
  one file. A cut that finds every eligible ordinal held or quarantined is
  refused with `ErrSnapshotLimit`. So 10⁴ home directories with 24 hourly
  subtree snapshots each need about 48 ordinals per path, not 240,000 per share;
- **SMB Previous Versions** names each complete snapshot by the token
  `@GMT-YYYY.MM.DD-HH.MM.SS` of its cut time in UTC. The cut transaction sets
  each cut time at least one second past that of every snapshot visible beside
  it ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)), from records it already reads, so at each path each token
  names exactly one snapshot — also after a move to an installation whose clock
  is behind.

> decision: the ordinal quarantine is fixed at 24 h, the horizon past which a
> client is assumed to have dropped a snapshot's cached identifiers, since SMB
> clients key caches on volume serial and file id with no expiry this set can
> observe. Raise it, or make it a setting, if a client is found reusing a cached
> id across a longer gap.

> ponytail: snapshot reads bypass the journal and run at the remote tier's
> latency, which bounds a mass restore ([§8.5](#8.5%20Benchmarks%20and%20targets)). Add a fill keyed by the
> snapshot when the restore-throughput benchmark falls short of its target.

### 2.6 A writable clone is a new share in the same namespace

A clone creates a new share from a complete snapshot, in the snapshot's
namespace, as one staged import ([§5.2](#5.2%20Import%20is%20staged%20and%20published%20atomically)):

- the transaction that starts it writes a use record on the snapshot
  ([§3.2](#3.2%20A%20backup%20holds%20its%20snapshot)), so the snapshot cannot be deleted while it is copied;
- the tree the cut sees is copied with **new FileIDs**, mapped so that hard links
  stay links, each file keeping its `Number`; every record is written with
  `born` 0, the cut the new share's first transaction reads, since the new share
  starts at cut 0 ([RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore));
- the refs the snapshot sees become the new files' refs, each raising its chunk's
  count and writing its reverse-index key as it is staged. That is
  [RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)'s restore, conditional on each chunk existing
  ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence)), in bounded batches. A chunk whose block was retired since the
  cut is adopted all the same, and the adoption resurrects the block
  ([RFC 9 §3.3](rfc-9-gc.md#3.3%20Adoption%20resurrects%20a%20retired%20block)). Resurrection stays normative on this path, and on restore and
  re-home, whatever is deferred with deduplication: a snapshot's history refs
  keep its chunks counted, but a chunk the clone names can still sit in a block
  that compaction relocated and retired;
- one transaction raises the new share's file-number allocator above the
  highest `Number` imported ([§4.5](#4.5%20Versions%20and%20FileIDs%20on%20import)), publishes the share and deletes the use
  record.

The clone then shares **chunks in the remote store, and nothing else**, with its
source. Deleting the source share, the snapshot or the clone releases only that
one's own refs and changes nothing the other two read. The clone moves with its
namespace ([§2.1](#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)).

**Worked example: clone, then delete the source** — `S-snap-clone-delete-source`.
Share `prod` holds one ref r on chunk c.

| t | action | refs on c | c's count |
| --- | --- | --- | --- |
| 0 | — | r (prod, live, `born` 2) | 1 |
| 1 | snapshot 7 of prod | r | 1 |
| 2 | clone `dev` from 7: use record; r' staged (dev, `born` 0) with its reverse key; publish; use record deleted | r, r' | 2 |
| 3 | prod overwrites the range | r → history (`born` 2, `died` 7); new ref on c2 | 2 |
| 4 | prod is deleted with its snapshots: 7's history dropped, prod's live refs released | r' | 1 |
| 5 | dev reads the range | r' resolves c | 1 |

> ponytail: a clone copies every ref, O(refs) records. Upgrade to files that
> read through their source snapshot until first written, when clone latency on
> large shares matters.

### 2.7 Scheduled snapshots, retention and locks

A share **MAY** carry one snapshot policy: a schedule, a retention of *keep N per
interval* over any of `hourly`, `daily`, `weekly` and `monthly`, a time zone, and
an optional lock duration and backup, catalog or copying.

**Retention, exactly.** Periods are calendar periods in the policy's time zone,
UTC by default: an hour on the clock, a calendar day, an ISO week starting
Monday, a calendar month. A snapshot belongs to the period holding its cut time.
For each interval with keep N, the policy keeps the newest `complete` policy
snapshot of each of the N most recent periods **that hold one**; periods with no
snapshot — the service was down, every tick was refused by the hold bounds or the
gate — are skipped, not counted, so an outage never prunes history to make room
for empty periods. **A period whose tick the snapshot reserve refused
([§2.9](#2.9%20Space%20is%20reported%2C%20not%20charged)) is the exception: it counts as holding one.** Pruning then runs as
if that snapshot had been taken, so the oldest policy snapshots age out, their
history is dropped, and cuts are admitted again once `history_bytes` falls below
the reserve. Skipping those periods too would leave a churny share refused for
ever: nothing would age out, and no cut would ever be taken again. **Under
reserve pressure that is not enough**: on a share whose history outgrows the
reserve for weeks, the coarse intervals keep their snapshots well after the
fine ones have aged out. So while a tick is refused by the reserve, the policy
also prunes its **oldest unlocked policy snapshot**, one at a time, until
`history_bytes` falls below the reserve and the cut fits; locked and manual
snapshots are never pruned for it, and each such prune is counted and logged.
A snapshot kept by any interval is otherwise kept; one kept by none is pruned
([§2.8](#2.8%20Deleting)). A `cutting` or `holding` snapshot is neither counted nor pruned
until it is `complete` or `failed`; a `failed` one is deleted. A wall-clock hour
that occurs twice when the zone leaves daylight saving is two periods, and one
that is skipped is none.

- Pruning deletes only snapshots the policy made, never manual ones.
- A tick refused by the hold bound or the snapshot reserve ([§2.9](#2.9%20Space%20is%20reported%2C%20not%20charged)) is
  skipped, not queued, and counted; ticks missed while down produce at most one
  snapshot on start. A cut costs one record, so a schedule never falls behind a
  large share.
- A policy that names a backup location, kind and retention backs up each
  policy snapshot ([§3](#3.%20Catalog%20backups), [§3.4](#3.4%20Copying%20backups)).
- The scheduler runs once per share, in the installation holding the share's
  namespace claim.

**Locks.** A snapshot **MAY** carry a lock: an expiry time before which it cannot
be deleted, and a mode.

- **`compliance`**: the lock **MUST** refuse every deletion of the snapshot with
  `ErrLocked` — manual, by pruning, or with its share — until it expires,
  whoever asks. It can be set and extended, never shortened or removed.
- **`governance`**: the same, except that a principal holding the separate
  `snapshot-lock-override` right may shorten or remove it. Each override is
  logged at `Warn` and counted, naming the principal. The right is granted to no
  one by default, and an administrator cannot grant it to themselves.

A lock's expiry **MUST** be refused with `ErrLockTooLong` when it lies more than
`snapshots.lock_max` past the time it is set or extended. A mistyped expiry —
years instead of hours — would otherwise pin the snapshot, its history and its
share past any recovery, since a compliance lock cannot be shortened. A share
with a locked snapshot cannot be deleted. A move carries locks and their modes
([§4.2](#4.2%20The%20move%2C%20step%20by%20step)).

> decision: a lock refuses deletion through this system's API only, and a
> governance lock yields to one separately granted right. An attacker
> holding the namespace bucket's credentials can still delete the objects; the
> namespace's bucket cannot be versioned or locked ([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)), so the
> protection against that attacker is a copying backup at an immutable location
> ([§3.4](#3.4%20Copying%20backups), [§1.1](#1.1%20Non-goals)). The lock's value is that one
> stolen administrator credential cannot erase every snapshot and, through the use
> records, every backup.

**Worked example: a locked snapshot refuses deletion** — `S-snap-locked-delete`.

| t | action | result |
| --- | --- | --- |
| 0 | policy takes snapshot 30 with a 7-day lock | 30 `complete`, locked until T+7 d |
| 1 | an administrator deletes 30 | `ErrLocked`; nothing changes |
| 2 | the administrator sets its lock to T+1 h | `ErrLocked`: a lock cannot be shortened |
| 3 | a pruning tick finds 30 outside its retention | skipped, counted `locked` |
| 4 | T+7 d: the lock expires; next pruning tick | 30 deleted as in [§2.8](#2.8%20Deleting) |

### 2.8 Deleting

Deleting a snapshot **MUST** be refused with `ErrLocked` while it is locked
([§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)) and with `ErrHeld` while a use record names it ([§3.2](#3.2%20A%20backup%20holds%20its%20snapshot));
pruning defers such a snapshot.

A deletion drops the history that no other live snapshot sees. With the nearest
live cuts *kp* < *k* < *kn* — *kp* absent reads as 0, *kn* absent as ∞ — those
are the versions with

    kp ≤ born < k ≤ died < kn

Snapshot *k* sees them, since `born < k ≤ died`; *kp* does not, since
`born ≥ kp`; *kn* does not, since `died < kn`; and no live cut further out sees a
version neither neighbour sees, because the cuts a version is visible to are
consecutive. Every such version has `died` in `[k, kn)`, so the died index
([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)) finds them without reading anything else.

**A share's deletions run one at a time**, under the `deleting` field of
`Cut(share)` ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)), for share and subtree snapshots alike, whoever
requests them — an operator, pruning, a share's deletion, a failed snapshot's
cleanup; there is no second, concurrent path. Two at once can each leave `klatest`
naming the other's cut, and history moved against a cut that is gone lands where
no walk visits: live cuts {5, 9} deleted together, the deletion of 9 sets
`klatest` to 5 while the deletion of 5, not the newest, leaves it alone.

1. One transaction checks the lock and use records, requires `Cut(share).deleting`
   to be empty and sets it to *k*, marks the snapshot `deleting`, deletes
   `LiveCut(share, k)`, and sets `klatest` to the greatest remaining live cut, 0
   when none remains ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). It writes `Cut(share)` whatever
   `klatest` was, so two deletions starting at once conflict on it; the loser
   finds `deleting` set and waits, and pruning defers its snapshot. A use record
   taken concurrently conflicts with it on the snapshot record, and a transaction
   moving a version to history against *k* conflicts with it on `LiveCut(share, k)`
   ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)). Each primary releases its journal hold for *k*, keeping what
   another live cut's hold still covers.
2. Batches walk the died index from *k*, in each shard the cut covers. Each batch, in its transaction, reads the
   live cuts, stops at the next one above its cursor, and drops a version only if
   no live cut *c* has `born < c ≤ died`, decrementing a ref's chunk. Reading the
   live cuts in every batch, rather than once, makes a resumed deletion correct
   whatever cuts were taken while it was stopped.
3. The final transaction removes the snapshot record, sets `klatest` again to the
   greatest remaining live cut and clears `deleting`, so no deletion leaves
   `Cut(share)` naming a cut that is gone.

**Worked example: two deletions requested at once** —
`S-snap-delete-serialised`. Live cuts 5 and 9, `Cut {9, 9}`.

| t | deletion of 9 | deletion of 5 | `Cut(share)` |
| --- | --- | --- | --- |
| 0 | step 1: `deleting` 9, `LiveCut 9` gone, `klatest` 5 | step 1: `klatest` 9, `deleting` 5 | — |
| 1 | commits | conflicts on `Cut`, retries: finds `deleting` 9, waits | `{9, 5, deleting 9}` |
| 2 | walks `died` in [9, ∞); final transaction | — | `{9, 5}` |
| 3 | — | step 1: `LiveCut 5` gone, `klatest` 0 | `{9, 0, deleting 5}` |
| 4 | — | walks `died` in [5, ∞); final transaction | `{9, 0}` |

Afterwards a first overwrite of a version with `born` below 5 compares against
`klatest` 0, writes in place, and leaves no history that no walk would visit.

**Worked example: deleting the middle snapshot** — `S-snap-delete-middle`. Live
cuts 5, 9 and 14; delete 9.

| History ref | `born` | `died` | Seen by | Visited? | Outcome |
| --- | --- | --- | --- | --- | --- |
| h1 | 3 | 12 | 5, 9 | yes, `died` in [9, 14) | kept: 5 sees it |
| h2 | 6 | 11 | 9 | yes | dropped; its chunk decremented |
| h3 | 6 | 14 | 9, 14 | no, `died` ≥ 14 | kept: 14 sees it |
| h4 | 1 | 7 | 5 | no, `died` < 9 | kept: 5 sees it |

The deletion reads two history entries, whatever the share's total history.

Deleting a share with snapshots **MUST** be refused unless the request deletes or
detaches them.

**Detaching.** Deleting a share with its snapshots detached does three things:

1. **Retire the share.** One transaction marks the share `retired`: it is
   removed from every export list and mount, serves no client, and refuses
   every write, new cut, clone into it and policy tick. Its records, identity,
   FileIDs, `Cut` record and snapshots stay where they are.
2. **Drop what no snapshot sees.** Every file is removed as if unlinked and
   released after a final cut no snapshot holds: each live version a live cut
   sees moves to history, and every other is dropped, by the ordinary
   supersede rule ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)) in the batching of [RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation). The share's live
   content is then charged nowhere, and its snapshots keep exactly their own.
3. **Keep the snapshots.** Each remaining snapshot is a **detached snapshot**:
   it can be listed per namespace, browsed by an administrator, cloned, backed
   up, restored, locked and deleted, and nothing else. Its locks and use records
   apply unchanged.

When the last detached snapshot of a retired share is deleted, the share's
remaining records are removed and its identity is never reused. A detached
snapshot moves with its namespace ([§4.2](#4.2%20The%20move%2C%20step%20by%20step)). A recovery import's detached
snapshots ([§3.3](#3.3%20Restore)) are snapshots of a retired share created for them.

### 2.9 Space is reported, not charged

Quotas charge a share's live logical bytes only
([RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota)). History is reported beside them:

- each share keeps `history_bytes`, a counter
  ([RFC 16 §4.4](rfc-16-metadata-store.md#4.4%20Counters%20that%20many%20writers%20change)) raised by every move to history and lowered by
  every drop;
- each snapshot reports **bytes freed if deleted**: the bytes of chunks whose every
  remaining ref is a history ref in its interval, computed on request by the same
  died-index walk a deletion makes;
- every share has a **reserve**: `snapshots.reserve` when set, and otherwise
  `snapshots.reserve_fraction` times its live charged bytes at the time of the
  check. While `history_bytes` exceeds it, a new cut is refused with
  `ErrSnapshotReserve`. Writes are never refused for history. An unbounded
  reserve exists only when an operator sets one explicitly; history that nothing
  bounds would grow until the metadata store or the bucket is full, and that is
  found only when something else fails.

> ponytail: bytes freed if deleted is computed on request, O(history in the
> snapshot's interval), and changes as neighbours are deleted. Keep it as a
> maintained per-snapshot counter when operators poll it often enough to show in
> a profile.

### 2.10 Subtree snapshots

**(cluster)** A **subtree snapshot** is a snapshot of one subtree shard: a shard rooted at a
directory an operator chose, or created in a directory marked for per-child shards
([RFC 11 §2](rfc-11-ownership.md#2.%20Shards), [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards)). It is browsed under that directory. A
home directory on its own per-child shard is the usual case. The mechanism is
§2.2–§2.8's, narrowed to a set of shards.

**A cut covers a set of shards.** A share cut covers every shard of the share,
including shards created after it, which hold nothing it could see. A subtree cut
covers its **covered set**, fixed when the cut is announced: the shard whose root
is the chosen directory, and every shard whose root lies beneath that directory.
`LiveCut(share, k)` records the kind of cut and, for a subtree cut, its covered set.
A subtree cut of a directory that is not a shard's root **MUST** be refused with
`ErrNotShardRoot`.

**Subtree cuts draw from the share's sequence.** A subtree cut takes the next
number from `Cut(share).k`, as a share cut does, so cut numbers stay unique
across both kinds and a `born` means the same thing whichever shard its file
sits in. **Its cut time is unique only where it is seen**: the later of the
store's time and one second past the newest cut time among the share's share
cuts and the `SubCut` of each covered shard, which records the cut time of the
newest subtree cut covering it. Every snapshot visible under the new one's root
is a share cut or a subtree cut covering one of its shards, so the Previous
Versions tokens of [§2.5](#2.5%20Browsing%20a%20snapshot) stay distinct at every path. Drawing every cut
time from one share-wide sequence instead would push 10⁴ hourly subtree cuts
about 10⁴ s ahead of the clock each hour; under this rule each runs at most one
second ahead of the store's time per cut on its own path.

**Only the covered shards' gates close.** [§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)'s four steps run over the covered set
only. The coordinator announces the cut, closes and holds at each covered shard's
primary, and commits a cut transaction that reads only the covered shards' records.
That transaction raises `Cut(share).k`. It writes `LiveCut`, the covered shards'
hold records and each covered shard's `SubCut`, then reopens. Every other shard
keeps serving with its gate open. The `cutting` record's rule that a primary starts
closed applies only to covered shards.

A transaction on an uncovered shard T can read the cut number before the cut and
commit after it, stamping `born` *k* − 1. That is safe for two reasons. No live cut
covering T sits at *k*, so no snapshot ever reads T at *k*. The next share cut
closes T's gate like any other. `born` also never decreases along one key. A
transaction that reads a version reads `Cut(share)` in the same read snapshot, so
it sees a cut number at least as new as the one that version was stamped with.

**Coverage decides history.** A version moves to history exactly when a live cut
that covers its shard sees it ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)). Each shard in some live subtree cut's
covered set keeps `SubCut(share, shard) = { klatest, cut time }`: the newest live
subtree cut covering it, and the cut time of the newest subtree cut that ever
covered it. `Cut(share).klatest` stays the newest live share cut. So:

- the common-case test is `born < max(Cut.klatest, SubCut(shard).klatest)`;
- the interval test reads the `LiveCut` records in `(born, died]` with conflict
  tracking and keeps those that cover the version's shard.

A subtree cut makes no version of an uncovered shard go to history.
`SubCut` is raised only by a cut, behind its shard's gate, and lowered only by a
deletion, like `klatest` ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)). Each died-index key records the shard its
version was superseded in, so a deletion tests coverage without reading the file.
That shard is always the right one, because a covered shard's files cannot move
while its cut lives (below).

**What it shows.** Snapshot *k* is reachable read-only under the virtual directory
([§2.5](#2.5%20Browsing%20a%20snapshot)) at the root of its subtree, not at the share's root. It shows the
covered shards' files as the cut saw them, reached from that root through the
covered shards' directories. Two things are left out:

- **an entry naming a file of an uncovered shard.** Such a file was renamed into
  the subtree across shards before the cut and kept its own shard
  ([RFC 11 §2](rfc-11-ownership.md#2.%20Shards)). The cut never ordered that file's records, so reading it at *k*
  could show a later version;
- **a file of a covered shard whose only entry at the cut is in an uncovered
  directory.** Nothing within the subtree reaches it.

Every other rule of [§2.5](#2.5%20Browsing%20a%20snapshot) holds. SMB Previous Versions for a path lists every
complete share snapshot, and every complete subtree snapshot whose root is the path
or an ancestor of it. A subtree snapshot can be cloned, backed up by either kind,
restored and locked like a share snapshot, over what it shows. A clone's root is
the subtree's root. Holds ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)) are written and bounded for the covered shards
only.

**Renames after the cut.** A rename moves entries and never changes a file's shard
([RFC 11 §2](rfc-11-ownership.md#2.%20Shards)). Every entry is a versioned record of its directory's shard. So:

- **a file of the subtree renamed out** supersedes its entry in a covered
  directory, and that entry moves to history. The snapshot still shows the file
  where it was. The file stays in its covered shard, so later writes to it still
  keep history for the cut;
- **a file renamed in** from an uncovered shard gets an entry with `born ≥ k`,
  which the snapshot does not see;
- **a rename spanning a covered and an uncovered shard** is admitted at both
  gates or at neither ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)), so it is ordered against the cut.

**Moves of files between shards.** A move of files ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) into or out of a
shard that a live subtree cut covers **MUST** be refused with
`ErrSubtreeSnapshot`. After a move out, later writes to the moved file would test
coverage against its new shard and drop history that the subtree snapshot reads.
A file moved in would bring records that the cut never ordered. A handover of a
shard's primary moves no files, and is allowed. Marking a directory inside a
covered shard for per-child shards moves its child trees, so it waits until the
subtree snapshots are deleted. The cut itself is unaffected by a move that is
already running: [§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)'s rules for moves mid-cut stand, and the refusal
applies to batches not yet frozen once the cut commits.

> ponytail: coverage is by the shard a file is in now, so a covered shard refuses
> moves of files for as long as a subtree snapshot of it lives. Upgrade to
> coverage recorded per file at the move, so that a moved file keeps the cuts that
> covered it, when operators need to split or re-mark a shard that carries
> subtree snapshots.

**Deleting.** [§2.8](#2.8%20Deleting) applies with coverage. For a version, the nearest live cuts
*kp* and *kn* are the nearest ones that cover its shard. A subtree deletion walks
the died index of its covered shards only, each from *k* up to the next live cut
covering that shard; the died index is keyed by shard first ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)), so no
other shard's history is read. For each version it drops the version if no live
cut covering the version's shard sees it. The final transaction recomputes
`klatest` for a share cut, or each covered shard's `SubCut` for a subtree cut.
Deletions of either kind run one at a time under `Cut(share).deleting`.

**Subtree snapshots at scale.** The case they exist for is a share of 10⁴ home
directories on per-child shards, each snapshotted hourly. Each subtree cut reads
and closes one shard, writes one `LiveCut` and one `SubCut`, and raises
`Cut(share).k`, one small record write per cut; each deletion reads only its own
shard's history. Nothing in a subtree cut or deletion grows with the number of
other shards.

> ponytail: a share's deletions are serialised, so 10⁴ hourly subtree snapshots
> mean about three deletions a second through one `deleting` field, each a few
> small transactions. Upgrade to serialising subtree deletions per shard, with
> share-cut deletions taking every shard, when a share's deletion queue grows
> instead of draining.

**Worked example: a subtree snapshot, a rename out of it, and its deletion** —
`S-snap-subtree-rename`. Share `home` has per-child shards S1 for `/alice` and S2
for `/bob`. The share's cut number is 6, and no snapshot is live, so `Cut` is
`{k 6, klatest 0}`.

| t | action | S1 (`/alice`) | S2 (`/bob`) | records |
| --- | --- | --- | --- | --- |
| 0 | subtree snapshot 7 of `/alice` | gate closed for the cut transaction; Hold(7) | serving, gate open | `Cut {7, 0}`, `LiveCut 7 {S1}`, `SubCut(S1) 7`; 7 `complete` once S1's hold drains |
| 1 | bob overwrites `/bob/y` (`born` 4) | — | 4 < max(0, 0) is false: replaced in place, new `born` 7 | no history |
| 2 | alice overwrites `/alice/x` (`born` 4) | 4 < max(0, 7): old File and ref to history, `died` 7 | — | 2 history versions, shard S1 |
| 3 | bob renames `/alice/f` (a file of S1) to `/bob/f`: admitted at both gates | entry `alice/f` (`born` 2) to history, `died` 7 | new entry `bob/f`, `born` 7 | `f` stays in S1 |
| 4 | bob writes to `/bob/f` | `f`'s File and ref (`born` 3) to history, `died` 7: S1 is covered | — | — |
| 5 | an operator marks `/alice/proj` for per-child shards | move refused `ErrSubtreeSnapshot` | — | — |
| 6 | browse `/alice/.snapshot/7` | shows `x` and `f` as they were; nothing under `/bob` | — | — |
| 7 | delete 7: walk `died` in [7, ∞) | 5 versions, all S1; no live cut covers S1: dropped | — | `SubCut(S1)` 0; `Cut {7, 0}` |

Had the cut closed S2's gate too, the write at t1 would have waited on the gate.
Had it been a share cut, the write would also have written history nobody asked
for.

## 3. Catalog backups

A **catalog backup** is a copy of one snapshot's metadata outside the metadata
store: the tree and the refs, and where the blocks they name are. It holds no
data. It survives the loss of the metadata store, and nothing else: it does not
survive the loss of the bucket or of the namespace's folder, the destruction of
key material, or a bucket-level attack. Use a copying backup ([§3.4](#3.4%20Copying%20backups)), which also
copies the blocks, against the first and last of those.

**No backup survives losing the master key.** Every export, of any namespace, is
sealed under the namespace's export key, which is held only wrapped under a
master key ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)), so a backup of either kind, at any
location, mutable or immutable, opens only where that master key is held. In an
encrypting namespace the chunk-ID, header and data keys are wrapped the same way. The
master key therefore **MUST** live off the host, in a key service or in a key
file whose off-host escrow was verified at setup, and no key falls back to the
host's own wrapping key; [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)'s table is the one statement of
which keys exist per namespace kind, what wraps each and what must live off the
host. A non-encrypting namespace keeps only its chunk-ID key in the clear beside
its blocks — its key record is not wrapped — so a bucket reader can verify its
chunks; its export key is still wrapped, and restoring its backup after losing
the host still needs the escrowed master key.

**A backup records the master keys it needs, and they outlive it.** Each export
names, in its clear part ([§5.1](#5.1%20Layout)), the ID of every master key that wraps a
key record it carries, and the backup's state object repeats them. The material
provider **MUST** refuse to destroy a master key that any retained export of any
namespace names — a backup not `expired`, at any configured location — with
`ErrMaterialInUse`, naming the backups. Rotating a master key re-wraps the key
records in the metadata store only ([RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)); every retained
export keeps the old wrapping, so the old key is destroyed only once the last
export naming it has expired, and immutable backups are not made unrestorable by
the usual compliance rotation.

**A backup location has a mode**, stated in its location record
([§6.2](#6.2%20Configuration)) and enforced by the store that opens it
([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)):

- **`mutable`**: no versioning, no lock, and DittoFS deletes what expires. It
  protects against losing the metadata store and, for a copying backup, the
  namespace's bucket; it does not protect against a holder of the installation's
  credentials.
- **`immutable`**: versioning and a compliance-mode lock with retention at least
  every backup's written there, a noncurrent-version expiry rule, and a
  credential that cannot delete. Every object is written and read by version, so
  a later put under the same name — by anyone — adds a version and changes
  nothing a backup recorded. Nothing is deleted by DittoFS; what expires, expires
  by the lifecycle rule once its lock has ended ([§3.4.4](#3.4.4%20Expiry%20and%20the%20sweep)).

An immutable location's bucket holds no namespace's blocks, since a namespace's
own store refuses versioning and locks ([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)); turning either on for the
namespace's bucket is refused, so it cannot stand in for a backup.

### 3.1 A backup is an export of one snapshot's metadata

A backup writes an export of kind `backup` ([§5](#5.%20The%20export%20format)) to a configured **backup
location**: a directory, or a folder in a bucket — the block store's bucket
included — outside every namespace's prefix
([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)), never inside the metadata store it protects. It
carries:

- the records the snapshot sees — files with their FileData fields, entries,
  ACLs, xattrs, stream links and holes, with directory deltas folded in — as plain
  records, and per file the refs the snapshot sees, as plain refs;
- the chunk and block records of every chunk they name, as **location hints**
  only ([§3.3](#3.3%20Restore));
- the material in those blocks' census, each as (material ID, fingerprint)
  ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)), and the namespace's key records, each held as
  [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)'s table says for its kind — wrapped under a master key,
  never unwrapped, or in the clear for a non-encrypting namespace's chunk-ID key
  — with the IDs of the master keys that wrap them ([§3](#3.%20Catalog%20backups), [§4.4](#4.4%20Key%20scope%20and%20material));
- the namespace's identity, prefix and key scope, and every other bound setting
  of the namespace ([§5.1](#5.1%20Layout));
- per share, its fold rule and the hash key its entries' digests are keyed under
  ([RFC 7 §3.4](rfc-7-namespace-metadata.md#3.4%20Enumeration)), both fixed when the share was created. Every entry key and
  every directory position is derived from them, so an import that drew its own
  would find no entry by lookup and resume no listing where a client's cookie
  points.

Like every export, it is sealed and authenticated under the namespace's export
key ([§5.1](#5.1%20Layout)): in an encrypting namespace, a reader of the location who
lacks the master key learns neither the tree nor the chunk IDs, and cannot alter
a byte unnoticed.

Its state — `writing`, `complete`, `failed`, `expired` — is kept in the location,
listable without the installation that wrote it.

### 3.2 A backup holds its snapshot

A **use record** in the metadata store marks a snapshot as being read by a clone,
a restore, a backup or a move. It is written in the transaction that starts the
reader, which requires the snapshot to be `complete` and conflicts with a
deletion's first transaction on the snapshot record. For a clone or restore it is
deleted at publish or on failure; for a backup, when the backup expires; for a
copying backup, when it completes, since it then holds its own blocks
([§3.4](#3.4%20Copying%20backups)); for a move, when B has published or A has aborted ([§4.2](#4.2%20The%20move%2C%20step%20by%20step)). A deletion checks
the use record, not the backup location, so an unreachable location makes deletion
fail closed.

So while a backup is being written, and until it expires, its snapshot cannot be
deleted ([§2.8](#2.8%20Deleting)), and the refs the snapshot sees keep every block the backup
names counted. The backup adds no second liveness mechanism
([RFC 9 §2.1](rfc-9-gc.md#2.1%20References%20are%20the%20only%20authority)).

Expiry marks the backup `expired` in its location, then deletes the use record,
then removes the export. An expired backup **MUST NOT** be restored; the reverse
order lets a crash leave a restorable backup whose blocks were swept.

**An abandoned reader releases its snapshot.** A use record whose reader is still
running — a backup `writing`, a clone or restore staging — carries a deadline its
reader renews while it makes progress. The snapshot service **MUST** treat a use
record past its deadline as abandoned: it marks the backup `failed` in its
location if it is still `writing` — never one already `complete` — or drops the clone's or restore's staging ([§5.2](#5.2%20Import%20is%20staged%20and%20published%20atomically)), and only then
deletes the use record, in the same order and for the same reason as expiry. A
`failed` backup **MUST NOT** be restored. A crashed reader thus never leaves its
snapshot undeletable. A move's use record ([§4.2](#4.2%20The%20move%2C%20step%20by%20step)) has no deadline: the move
itself deletes it, at B or on abort.

### 3.3 Restore

Restore creates a new share. There are two sources:

- **From a snapshot**, or from a backup whose snapshot still exists: a clone
  ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)), whose records are current.
- **From a backup alone**, when the metadata store holding the namespace is lost: a
  **recovery import**, which takes the namespace's claim ([§4.1](#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)) on the
  operator's statement that the installation holding it is gone, and then:
  - waits out the old installation's claim checks ([§4.1](#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)) before it
    resolves a hint, puts, adopts or deletes anything, so an old installation that
    is partitioned rather than gone has stopped relocating and deleting;
  - checks each location hint against the named block's header, which lists its
    chunk IDs ([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout)). Relocation makes hints stale
    ([RFC 9 §4.3](rfc-9-gc.md#4.3%20A%20reader%20can%20hold%20the%20old%20location)); a stale chunk is found by listing the namespace and reading
    headers, and a chunk found nowhere fails the import. A block's header says
    itself whether its index is sealed and under which header key, in its seal
    field, so the importer compares each block under the key that block names —
    never under whatever the namespace's configuration says now, which a chain
    change or a header-key rotation since the block was written would make
    wrong ([RFC 5 Appendix B.5](rfc-5-transforms.md#B.5%20What%20a%20bucket%20reader%20still%20learns)). It must hold every header key a seal names
    ([§4.4](#4.4%20Key%20scope%20and%20material));
  - loads the namespace's key records from the `keys` control object in the
    namespace's bucket ([RFC 4 §4.13](rfc-4-remote-tier.md#4.13%20Control%20objects)) and from every configured backup
    location, besides those the export carries, and uses the newest of each
    that unwraps and matches its fingerprint. Every new key record — wrapped,
    never a secret — is written to both before it becomes current
    ([RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)), so a key rotated after the last backup, under
    which relocation has since re-encoded that backup's chunks, is still found;
  - re-derives the material census from the headers it resolved rather than
    trusting the export's: a block re-sealed after the backup can carry material
    the export never listed. Material missing from its provider refuses the
    import naming the IDs, as [§4.4](#4.4%20Key%20scope%20and%20material) requires;
  - recomputes every count from the imported refs ([RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)) and ends
    with the audit ([RFC 6 §7.5](rfc-6-block-metadata.md#7.5%20Audit)); a retired block the backup records whose
    `not_before` has passed is moved to deleted before anything is served, since
    the old installation may already have deleted it ([RFC 9 §3.2](rfc-9-gc.md#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded));
  - imports **every other unexpired backup of the namespace** found in every
    configured backup location as a detached, held snapshot ([§2.8](#2.8%20Deleting)), each
    under a share identity and FileIDs of its own, before GC runs, or GC would
    sweep what only those backups name. A complete copying backup ([§3.4](#3.4%20Copying%20backups)) is
    exempt: it holds its own blocks, and the namespace's GC can sweep nothing it
    needs;
  - **publishes with the namespace's GC pause record written** ([§4.2](#4.2%20The%20move%2C%20step%20by%20step)), and
    leaves it until the operator states that every share of the namespace has
    been recovered or given up. A namespace can hold shares with no backup at
    all, and clones whose refs count chunks in blocks the recovered shares also
    name; their refs are in no export, so the recomputed counts are too low for
    the blocks they share, and the unrecorded objects of a listing include their
    blocks. While paused, the namespace deletes, relocates and collects
    nothing: an import of one share never deletes a block
    another share needs. The pause costs the space those deletes would have
    freed, and is reported ([§9](#9.%20Observability), `dittofs_move_gc_paused`).

**Losing the metadata store is recovered this way, and only this way.** On a
single node the embedded metadata store goes with its host
([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile), [RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model)). A node that starts with its store missing or
unopenable **MUST** refuse to serve, naming the store, and its GC deletes
nothing; an operator starts a recovery import from the newest catalog or copying
backup of each namespace. Each recovered share's files read as they were at that
backup's snapshot: content written after it, still in the bucket or the
journals, is not served as current, and its blocks wait, unreferenced, under the
GC pause above until the operator's statement. Taking a catalog backup on a
period is what bounds that loss. The journals' content for the lost store's
shares sits under tags no recovered share claims, since recovery gives each
share a new identity: before it serves, the node **MUST**, for every such tag,
either export its held extents for salvage and then `Forget` it, or `Forget` it
at once, and report each tag with its held and dirty bytes
([RFC 1 §3](rfc-1-journal.md#3.%20Interface)). A tag left alone would hold dirty extents that are never offered
and never evictable.

> decision: a recovery import finds other backups only in the backup locations
> the recovering installation is configured with, and the operator states that
> the list is complete. A backup in a location nobody lists is swept like any
> unreferenced content. Record each backup in the namespace's folder if operators
> ever lose track of their locations.

A backup of a namespace whose claim another installation holds and has not
released **MUST NOT** be imported except by that recovery statement: the holder's
GC would sweep what the import counts ([§4.3](#4.3%20GC%20across%20installations%20on%20one%20bucket)).

**Worked example: catalog backup, then restore** — `S-snap-catalog-restore`.
Installation A holds namespace `ns-photos`, share `photos`.

| t | action | state |
| --- | --- | --- |
| 0 | snapshot 12 `complete` | — |
| 1 | backup of 12 to `same-bucket` | use record on 12; export `writing` |
| 2 | export done | `complete`; use record stays |
| 3 | A's metadata store is lost | A serves nothing; the bucket is intact |
| 4 | operator on installation B: restore, previous holder gone | B lists `same-bucket`, finds the backup and one older one |
| 5 | B writes the claim `owned` at the next epoch, waits out A's claim checks | a partitioned A has stopped deleting |
| 6 | stage: resolve hints from headers, recompute counts, audit; stage the older backup as a detached snapshot | nothing served, nothing collected |
| 7 | publish share `photos-restored` with new FileIDs, the GC pause record in place | clients mount the new share |
| 8 | operator states every share of `ns-photos` recovered; the pause record is deleted and GC starts | — |

### 3.4 Copying backups

A **copying backup** is a catalog backup ([§3.1](#3.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)) that also copies every block
holding a chunk its snapshot names into a **block folder** at the backup's location.
It is an export of kind `backup-copy`. It survives losing the metadata store, the
namespace's folder and the namespace's bucket. It does not survive losing its
backup location, or the destruction of the key material that sealed its blocks,
which this set leaves to the material provider ([§1.1](#1.1%20Non-goals)).

A copying backup **MUST** be refused with `ErrLocation` when its location is in
the namespace's own bucket.

> decision: independence is judged by bucket only. A location in another bucket
> of the same service is accepted, although it shares that service's failures.
> Whether that is independent enough is the operator's call, and this set cannot
> tell two services from one. Refuse same-service locations too if operators
> mistake one for a real second copy.

A copying backup is also a catalog backup. A recovery import ([§3.3](#3.3%20Restore)) from it
resolves its location hints in the namespace, like any backup's, when the bucket
survived and only the metadata store was lost. The copied blocks are for when the
namespace's folder is gone. Restoring from them always makes a new namespace
([§3.4.5](#3.4.5%20Restore%20into%20a%20new%20namespace)).

#### 3.4.1 Layout at the location

```
<location>/
  control/health                           the location's health object, one per location
  exports/<namespace>/<backup>/export      the export
  exports/<namespace>/<backup>/state       its state object
  progress/<namespace>/<backup>/<batch>    blocks a running copy has stored
  blocks/<namespace>/                      the block folder: one store, this its prefix
    blocks/<block name>                    a block, byte for byte as the namespace stored it
```

- **One block folder per source namespace and location.** The backups of every
  share of a namespace share it, and a block is stored there once. A folder is
  opened as a remote store of its own, with its own prefix, and holds only blocks
  ([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)). Exports and progress objects sit beside it, never inside.
- **A folder block keeps its name.** Names are unique by their nonce
  ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)), across namespaces too, so names do not collide; a folder
  holds its namespace's blocks and, during a re-home into that namespace, the
  leaving namespace's blocks its copies name ([§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)). Each name is put only by copies of that one block, always with the same
  bytes.
- **One health object per location**, not per folder. It is written when the
  location record is created ([RFC 13 §2.5](rfc-13-configuration.md#2.5%20A%20backup%20location%20is%20its%20own%20record)), before any namespace copies
  there, and the version it was stored at is kept in the location record, so the
  first namespace to use an immutable location finds it and opens. A credential
  rotation proves the new credential reaches the same location by reading that
  recorded version (immutable) or a nonce stored in the object (mutable), not by
  any namespace's claim.
- **Each backup's state object** holds `writing`, `complete`, `damaged`, `failed`
  or `expired`, as [§3.1](#3.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata) keeps it, and can be listed without the installation.
- **The folder record** `NS‖ns‖bk‖location`, in the namespace's metadata, names the
  copy or sweep running at the folder, with a deadline its holder renews and a
  **lease number** raised at every take. It keeps the folder's **census**: every
  (material ID, fingerprint) in the census of a block that some retained
  manifest lists. Being under the namespace's prefix, it moves with a migration
  ([§4.2](#4.2%20The%20move%2C%20step%20by%20step)).
- **The folder record is a fenced lease.** A holder paused past its deadline
  must not act after another has taken the record. So every transaction a copy
  or sweep commits — census additions, the release — reads the folder record
  with conflict tracking and requires its own lease number, and a sweep reads it
  before each delete batch. Deletes and puts are not transactions, so a holder
  also fences itself by its own clock: when the read that last confirmed its
  lease showed *L* left before the deadline, it issues no put or delete once
  *L* / (1 + ρ) − σ has passed on its own monotonic clock since it sent that read,
  with ρ and σ [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)'s clock bounds. A new holder takes the record only
  past the old deadline. A sweep's delete then never lands after a copy has taken
  the folder and reused the block, short of a pause the clock bound does not
  cover — the ceiling the claim's decision names ([§4.1](#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)).

#### 3.4.2 What is copied

The export carries everything [§3.1](#3.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata) lists, with two differences:

- each chunk record names the folder block that holds the chunk, not the
  namespace's block;
- a `blocks` section, the **manifest**, lists every folder block that any of those
  chunk records names, with its size, its census and, at an immutable location,
  the version the folder stored ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)).

Each manifest is complete. A backup needs only the blocks its own manifest lists,
never another backup's export, so backups expire independently. The bytes are
incremental: a copy puts only the blocks its **base** does not already hold. The
base is the newest `complete` copying backup of the same share at the same
location, **plus the blocks listed by the progress objects of every `failed`
copy of the share made since it**. A share with no complete backup starts from
its failed copies' progress alone, and from nothing when it has none; a share
whose newest complete backup is `damaged` takes no base from it. So a failed copy
is the base of its successor: a first copy larger than one `max_copy_time` at
`backups.copy_rate` completes across several attempts, each putting only what the
ones before it did not, instead of restarting from zero and failing again. A
progress-listed block was put with its transfer checksum and is reused as a
base block is; at an immutable location only while its recorded version's
retain-until lies at least `max_copy_time` ahead, and otherwise it is copied
afresh.

The copier plans in chunk-hash order. It builds the set of chunks the snapshot's
refs name, live and history, sorted by hash, and merge-joins that set with the
base's chunk section, which the export already keeps in key order
([§5.1](#5.1%20Layout)). For each chunk:

- **The base maps it to a folder block** whose census names no material being
  retired ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)): the new export maps the chunk to that same block, and
  nothing is copied.
- **Otherwise**, the copier reads the block that the namespace's chunk record names
  now, and copies it whole. Every chunk of that block that the snapshot names is
  mapped to the copy.

A block relocated in the namespace since the base ([RFC 9 §4.2](rfc-9-gc.md#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)) is therefore not
copied again: its chunks are found at their old folder block, by hash. A chunk
whose folder block carries retiring material is copied afresh, so that the
material leaves the folder as older backups expire.

**Blocks are copied as sealed bytes, never re-sealed.** A copy is a raw
transfer: a whole-block get from the namespace and a put of the same bytes, under
the same name, into the folder, through the syncer's raw transfers, which carry
a block byte for byte and return the stored version
([RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface)). Nothing is decoded or encoded.

- **The copier needs no material, and never holds plaintext.** A block's
  structure is readable from its own bytes, and its name can be recomputed from
  its header and the namespace's key scope without material ([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout), R1).
  The copier recomputes the name from the header's nonce, chain ID and index
  under the namespace's scope ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)) before it streams the bodies, and the
  put carries the transfer checksum ([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)). That proves the copy is the
  block of that name, byte for byte; it does not prove the chunks inside decode
  to their IDs, which needs the material, and which `Verify` checks
  ([§3.4.3](#3.4.3%20Writing%20one%2C%20step%20by%20step)).
- **Re-sealing would buy one thing, at a price.** Decoding every copied body and
  encoding it again under target material costs CPU on every copied byte. It needs
  a second set of keys to hold, new names minted under a scope the folder would
  need, and a verification that uses both key sets. The one thing it buys is
  independence from the source's material. The census rule below gives that
  independence where it matters: retiring material ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)).
- **The cost of sealed copies is that material stays in use.** [RFC 5 §5.2](rfc-5-transforms.md#5.2%20Relocation%20re-encodes)'s
  reason for re-encoding on relocation applies here. A folder block keeps the
  material that sealed it for as long as a manifest lists it. So removing material
  **MUST** also find it in no folder record's census of the namespace, and a copy
  never reuses a folder block whose census names material being retired. Once
  every backup that lists such a block has expired, the folder's census no longer
  names the material, and removal proceeds.

> ponytail: a folder block is copied whole, dead chunks included, and is kept
> whole for as long as any retained manifest lists it: the folder never
> compacts. Its dead bytes are bounded by the namespace's compaction threshold at
> copy time and by retention. Upgrade to compaction at the folder, which needs
> material and put intents there, when the folder's dead ratio shows in its cost.

> ponytail: planning reads the snapshot's every ref and the base's whole chunk
> section, O(chunks) per backup, however little changed. Upgrade to selecting the
> refs whose change sequence is above the base snapshot's ([§4.2](#4.2%20The%20move%2C%20step%20by%20step)) when
> planning time shows beside the copy's transfer time.

#### 3.4.3 Writing one, step by step

Copies and sweeps of one folder run one at a time, from the installation whose
claim names it `owned` for the namespace ([§4.1](#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)). The folder record is what
serialises them.

1. **Start.** One transaction requires the snapshot `complete`. It writes a use
   record of kind `copy`, with a deadline ([§3.2](#3.2%20A%20backup%20holds%20its%20snapshot)). It takes the folder record,
   which must be free or past its deadline. The holder runs the folder store's
   `Recheck` ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) and refuses to start on any drift it reports. The
   copier then writes the backup's state object `writing` at the location.
2. **Plan**, as [§3.4.2](#3.4.2%20What%20is%20copied) describes.
3. **Copy, in batches.** For each batch of blocks to copy:
   - one transaction adds the batch's census to the folder record, so the census
     never misses material a folder block uses;
   - the blocks are copied through background flows of the syncer, one on the
     namespace's store and one on the folder's
     ([RFC 3 §2.9](rfc-3-syncer.md#2.9%20Workers%20are%20shared%20fairly%20across%20flows)). The copier resolves each chunk from the namespace's
     live chunk record, not from an old hint. When a block is absent because
     relocation moved its chunks, it re-resolves them and copies their new block
     ([RFC 9 §4.3](rfc-9-gc.md#4.3%20A%20reader%20can%20hold%20the%20old%20location)). The snapshot's counted refs keep every chunk it names
     somewhere in the namespace;
   - once the batch's puts have succeeded, a progress object lists its block names, and at
     an immutable location the version of each.

   Every other call the copier, the sweep, expiry and restore make at the
   location — state, export and progress puts and gets, listings, retention
   extensions — goes through the syncer as a small-object transfer
   ([RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface)), never straight to the store: it is retried with backoff
   within a deadline, its concurrency is bounded, and it is counted in the
   client's connection sum ([RFC 4 §4.10](rfc-4-remote-tier.md#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers)). One throttling reply in an
   extension burst then delays the backup rather than failing it.
4. **Export.** The copier writes the `backup-copy` export, manifest last. Every
   manifest block was either put by this copy or taken from the base's manifest.
5. **Complete.** At an immutable location the copier first extends every
   version the backup names to the completion time plus its retention
   ([§3.4.4](#3.4.4%20Expiry%20and%20the%20sweep)). It then marks the state object `complete`. Then one
   transaction releases the folder record and deletes the use record. Then the
   progress objects are deleted, where the location deletes anything.

From step 5 on, the snapshot can be deleted: the backup needs nothing in the
namespace.

> decision: a copying backup releases its snapshot when it completes, not when it
> expires, because its blocks are then its own. The cost is that the snapshot's
> own retention alone decides how long the namespace keeps those blocks. Keep the
> use record to expiry if restoring from the namespace, which is faster than from
> the folder, is ever wanted for as long as the backup lives.

**Verify.** `Verify` reads every block of a backup's manifest from the folder,
by the version the manifest names where it names one.
It decodes each chunk the export names and checks the chunk against its plaintext
hash ([RFC 5 §2.6](rfc-5-transforms.md#2.6%20The%20plaintext%20hash%20is%20the%20final%20check)). It needs the namespace's material. A missing block, or a
chunk that fails the check, marks the backup `damaged`. A damaged backup is never
a base. A restore from it **MUST** be refused with `ErrBackupDamaged`. A policy
**MAY** verify on a period. Verify only reads, so it takes no folder record.

**Failure and resume.** The copy renews its use record's and folder record's
deadlines while it makes progress.

- **A copier restarted before its deadline** resumes the same backup. It plans
  again, and skips the blocks its progress objects list. A block it put without
  listing it is put again, with the same bytes.
- **Past the deadline**, the backup is abandoned ([§3.2](#3.2%20A%20backup%20holds%20its%20snapshot)): the state object is
  marked `failed` if it is still `writing`, then the folder record is released,
  then the use record is deleted. Its progress objects stay: the share's next
  copy takes the blocks they list as base ([§3.4.2](#3.4.2%20What%20is%20copied)), and the sweep keeps them
  until a copy of the share completes, then collects every block of the failed
  copies that no retained manifest lists.
- **`max_copy_time` bounds one attempt**, from its start, not the whole chain of
  attempts that a large first copy takes. A copy still running at
  `max_copy_time` is marked `failed` as above, and its successor resumes from
  its progress at the next policy tick.
- **A location that stays unreachable, or a namespace bucket that is lost
  mid-copy,** ends in the same `failed`. Earlier backups are untouched.

#### 3.4.4 Expiry and the sweep

The folder has no metadata store behind it: the manifests are what say which
blocks are in use. Expiry follows [§3.2](#3.2%20A%20backup%20holds%20its%20snapshot)'s order. First the state object
is marked `expired`, then the folder is swept, then the export is removed. A crash
anywhere in that sequence leaves a backup that cannot be restored, and blocks that
the next sweep collects.

**Retention expires complete backups only, and never the last one.** A policy's
expiry **MUST NOT** expire a share's newest complete copying backup at a location.
So a chain of copies that keep failing — the namespace's bucket gone, say — never
expires the backups that are still good.

**The sweep** runs after an expiry, after a `failed` backup, or on a period. It
holds the folder record, runs only while the claim names its installation
`owned`, and starts only after the folder store's `Recheck` passes. The folder
record's holder also runs that `Recheck` on GC's period
([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)), so a location whose settings drift is reported while no copy runs:

1. **Mark.** Read the state object of every backup at the location whose state
   object names this folder, which are the backups filed under the folder's own namespace — a backup
   taken mid-re-home copies every block into the new namespace's folder
   ([§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), so no backup names a folder of another namespace. The mark set is the union of the
   manifests of every `complete` and `damaged` backup, plus the progress objects
   of every backup still `writing`, and of every `failed` one whose share has
   completed no copy at the location since (its blocks are a successor's base,
   [§3.4.2](#3.4.2%20What%20is%20copied)).
   An export or state object that cannot be read or verified **MUST** stop the
   sweep with no delete. An incomplete mark set would delete a live backup's
   blocks.
2. **Sweep.** List the folder ([RFC 4 §4.6](rfc-4-remote-tier.md#4.6%20List%20is%20a%20complete%2C%20resumable%20walk)) and delete, in batches
   ([RFC 4 §4.5](rfc-4-remote-tier.md#4.5%20Delete%20is%20batched%20and%20idempotent)), every block in no mark set. Before each batch, read the claim
   under the same fence GC uses ([§4.1](#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)).
3. **Settle.** Recompute the folder record's census from the mark set. Remove the
   exports and progress objects of `expired` backups, and those of a `failed`
   backup only once its share has completed a copy at the location since: until
   then they are a successor's base ([§3.4.2](#3.4.2%20What%20is%20copied)), and removing them would
   restart a first copy too large for one attempt from zero.

Serialisation makes the mark exact. No copy runs while the sweep holds the folder
record, so no block is added or reused between the mark and the delete. The one
writer is the claim holder, so no other installation puts into the folder.

> decision: expiry does not wait for a restore that is reading the backup. A
> restore that loses its blocks fails at the next read, and publishes nothing
> ([§5.2](#5.2%20Import%20is%20staged%20and%20published%20atomically)). Expiry is the operator's retention choice, and the rule above
> never takes the last good backup. Add a restore mark that the sweep honours if
> retention ever races restores in practice.

**At an immutable location** ([§3](#3.%20Catalog%20backups), [RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) DittoFS deletes
nothing, and every object is pinned by version and lock rather than by DittoFS's
own bookkeeping:

- **Everything is written and read by version.** Each block put, export, state
  object and progress object records the version the folder returned. A
  manifest names each block by name and version, and restore and `Verify` read
  exactly those versions. A put under the same name later — a retry, a
  misbehaving installation, a stolen credential — adds a version and changes
  nothing any backup reads.
- **Backups are found by version, and state objects are authenticated.** The
  current-version rule puts a delete marker on every object once it reaches the
  lifecycle age counted from its writing — the health object, and the export and
  state object of a last good backup included — so a listing of current objects
  stops showing them. Restore, the sweep and `List` therefore list object
  **versions** ([RFC 4 §4.15](rfc-4-remote-tier.md#4.15%20Versioned%20objects%20at%20a%20backup%20location)), never current objects, and a delete marker
  hides nothing. Every export, state-object and progress-object version is
  authenticated under the namespace's export key — a state object by the state
  MAC, a progress object by the progress MAC, each carrying a sequence the writer
  raises at every write ([RFC 5 Appendix B.1](rfc-5-transforms.md#B.1%20How%20a%20chunk%20is%20encrypted)) — and for each backup the reader
  takes, of the versions that authenticate, the one with the **highest
  authenticated sequence**, never the newest by the store's order. State
  transitions are monotone — `writing`, then `complete`, then `damaged` or
  `expired`; `writing`, then `failed` — so a stolen put credential's forged
  version fails to authenticate, and a re-put of an older authentic one
  (a backup's own `writing`, an old `complete` over a `damaged`) loses to the
  higher sequence rather than ending or misleading a restore.
- **The health object stays locked.** Every open and every `Recheck` extends the
  location health object's recorded version to the single target
  [RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes) states, within the retention cap, so the location still opens
  after the lifecycle age, however long a process stays up; extending writes no
  object.
- **Every put carries its own retain-until, and every retain-until is
  recorded.** The location has no bucket default retention to rely on: a
  default at least the longest retention would lock every hourly block as long
  as a monthly one. Each put sets its version's retain-until explicitly — the
  put time plus the backup's retention *R*, rounded up as below — and the copier
  records that value beside the version, in the progress object and then the
  manifest. Later decisions read the recorded value, or the store's retention
  read ([RFC 4 §4.15](rfc-4-remote-tier.md#4.15%20Versioned%20objects%20at%20a%20backup%20location)) where no record survives, and an extension that the
  service refuses because it would shorten the lock is treated as done.
- **Retention starts when a copy completes.** A backup with retention *R* that
  completes at *T* needs every version its manifest and export name kept until
  *T* + *R*. At step 5 ([§3.4.3](#3.4.3%20Writing%20one%2C%20step%20by%20step)), before the state object is marked `complete`,
  the copier extends every version the backup names whose recorded retain-until
  is below *T* + *R*, and the export's, and records the new values. A block put
  early in a long copy is therefore kept as long as one put at the end.
- **Extensions come in generations.** Every retain-until the copier sets is
  extended one **generation** `G` past the time it needs, `G` being the location
  record's ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes); 7 days by default), and a version whose
  recorded retain-until already covers the need is not touched. Each name has
  its own phase: its generation boundaries are the multiples of `G` offset by
  `hash(name) mod G`, so versions reused on one day fall due spread across the
  generation, and a background pass extends about one `G`-th of them a day
  rather than all at one boundary. A block that daily backups keep reusing is
  then extended about once per `G`, not once a day: about *N* ÷ `G` calls a day
  for *N* reused blocks — at 1 PiB of 4 MiB blocks with `G` of 7 days about
  3.8×10⁷ a day, 440/s, spread evenly — against one call per block per backup,
  or 2.7×10⁸ in one burst with a shared phase. The cost is that a version is
  held up to `G` past its need.
- **What a credential may lock is capped.** The location's bucket policy **MUST**
  cap the remaining retention a put may set at the location record's retention
  cap — the longest retention of any policy writing there, plus one generation
  and `backups.max_copy_time` — and the open checks verify that cap
  ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)), so a stolen put credential cannot lock objects for a century
  and hold their storage cost to ransom.
- **A copy reuses its base's versions, extending them.** Incremental copies work
  as elsewhere ([§3.4.2](#3.4.2%20What%20is%20copied)): a block the base holds is not put again, and
  the new manifest names the base's version, whose retention step 5 extends. No
  full copy is needed, and no block is put twice.
- **The lifecycle age bounds the copy, and the policy is checked against it.**
  The location record's lifecycle age **MUST** be at least the longest retention
  of any policy writing there plus `backups.max_copy_time`, and the bucket's
  current-version rule no younger, as its open checks
  ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)). An attempt still running at `max_copy_time` is failed, and its
  successor builds on it ([§3.4.3](#3.4.3%20Writing%20one%2C%20step%20by%20step)). A policy whose period is shorter than its
  estimated **incremental** copy — the bytes the share wrote over its last
  period, from its usage counters, over `backups.copy_rate` — **MUST** be refused
  when set, and its next tick skipped, with `ErrPolicyPeriod`: its increments
  could never keep up, and the location would only fill. The first copy is not
  held to the period, since it completes across attempts.
- **Expiry** marks the state object `expired` (a new version of it) and then
  deletes the use record, as [§3.2](#3.2%20A%20backup%20holds%20its%20snapshot) orders, so an expired backup is never
  restored. Removing anything is the lifecycle rules': the current-version rule
  places a delete marker once an object reaches the lifecycle age, and the
  noncurrent-version rule removes a version once its lock has ended.
- **The sweep** marks and settles and skips step 2: it lists nothing and
  deletes nothing. The folder's census is still recomputed from the mark set, so
  material leaves it as backups expire.

- **The last good backup stays locked.** Policy never expires a share's newest
  complete backup (the rule above), but a lifecycle rule would remove its
  versions once their lock ended. So while no newer backup has completed, the
  policy extends the newest one's versions — its blocks, its export and every
  version of its state object — by its retention again whenever their
  retain-until comes within one policy period. A chain of failing copies
  then keeps the last good backup, at an immutable location as at a mutable one.

> ponytail: copies and sweeps of one folder run one at a time, so the backups of
> a namespace's shares to one location are copied one after another. Upgrade to
> concurrent copies with an exclusive sweep, since two copies of one name put the
> same bytes, when a namespace's backup window no longer fits.

#### 3.4.5 Restore into a new namespace

A copying backup restores into a **new namespace**, on any installation that holds
a master key wrapping the source namespace's keys, which the export carries
([§3.1](#3.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)).
The source namespace need not exist. So restore works when the bucket is gone, and
for a copy to another site.

1. **Check.** Read the header. Every material ID must be held with its
   fingerprint ([§4.4](#4.4%20Key%20scope%20and%20material)), or the restore is refused with `ErrMaterialMissing`.
   The state, its version of highest authenticated sequence ([§3.4.4](#3.4.4%20Expiry%20and%20the%20sweep)), must be
   `complete`: `damaged`, `failed` and `expired` are refused.
2. **Create.** Create the target namespace with a new ID, a prefix of its own, its
   own key scope and keys, and its claim written `owned`. It has no previous holder,
   so there is no fence to wait out.
3. **Re-put the content, staged** ([§5.2](#5.2%20Import%20is%20staged%20and%20published%20atomically)). For each manifest block, read from the
   folder the bodies of the chunks the export names, by the version the
   manifest names where it names one. Use ranged gets
   ([RFC 4 §4.4](rfc-4-remote-tier.md#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly)), or one whole get when most of the block is named. Decode each
   body with the source material, and check it against its source chunk ID.
   Compute its chunk ID afresh under the target's chunk-ID key
   ([RFC 2 §4.1](rfc-2-carver.md#4.1%20A%20chunk)) — IDs are keyed per namespace, so no source ID is valid in
   the target — and record the mapping. A chunk longer than the target's chunk
   maximum ([RFC 2 §3.2](rfc-2-carver.md#3.2%20One%20setting%2C%20and%20the%20bounds%20derived%20from%20it)) is re-cut under the target's settings, and its ref
   becomes the refs of its pieces. Hand the chunks to the target namespace's
   block assembler ([RFC 2 §5](rfc-2-carver.md#5.%20The%20block%20assembler)), which mints names under the target's scope
   and puts each block under a put intent. That is
   relocation's read, mint, put and commit ([RFC 9 §4.2](rfc-9-gc.md#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)), with the target in
   another namespace. Only the chunks the export names are carried, so dead chunks
   stay behind.
4. **Import** the tree with new FileIDs and a new share identity, every record
   `born` 0, as a clone does ([§4.5](#4.5%20Versions%20and%20FileIDs%20on%20import)). Refs name chunk IDs
   ([RFC 6 §2.5](rfc-6-block-metadata.md#2.5%20Refs%20name%20hashes%2C%20never%20blocks)); each is rewritten to the target ID step 3 recorded and adopts
   the chunk re-put there ([RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)). Recompute the counts, run the audit,
   map principals by ID ([§5.1](#5.1%20Layout)), raise the file-number allocator above the
   highest `Number` ([§4.5](#4.5%20Versions%20and%20FileIDs%20on%20import)), then publish.

A chunk missing from the folder, or failing its hash, fails the restore, and
nothing publishes. `Verify` finds that damage before a restore needs the backup.

> decision: restore always re-encodes into a new namespace. A folder block's name
> is derived under the source namespace's ID ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)), so it cannot verify
> in another namespace. Restoring under the old ID would be a recovery import that
> races any holder that still has it. The cost is a decode and an encode of every
> restored byte, and new writes chunk under the new namespace's chunking key, so
> they deduplicate less against restored content. Add an as-is copy back under the
> old namespace ID, behind the recovery statement of [§3.3](#3.3%20Restore), if restore time
> after a lost bucket becomes the complaint.

#### 3.4.6 Cost and pacing

- **Transfer.** Each copy gets and puts only the blocks holding chunks that its
  base's folder blocks do not already hold, dead chunks included. That is one get
  and one put per block, plus one progress object per batch. A share that changes
  1% a day copies about the blocks that 1% landed in.
- **Metadata.** Planning reads the snapshot's refs and the base's chunk section
  once (the ponytail above). A sweep lists the folder once, and reads every
  retained manifest.
- **Pacing.** Copies run in the syncer's background class, behind every reader's
  demand ([RFC 3 §2.9](rfc-3-syncer.md#2.9%20Workers%20are%20shared%20fairly%20across%20flows)), and under `backups.copy_rate`, a byte rate per
  installation, so that a full first copy of a large share does not take a
  service's egress budget in one night.
- **Restore** costs a ranged get per named chunk from the folder, plus the decode,
  encode and put of every restored byte.

**Worked example: incremental copies, relocation and expiry** —
`S-snap-copy-incremental`. Share `photos` is in namespace `ns-photos`. Location
`vault` is in another bucket. Namespace blocks are `b…`. The folder is
`vault/blocks/ns-photos`.

| t | namespace | copy | folder | manifests |
| --- | --- | --- | --- | --- |
| 0 | snapshot 10 sees c1, c2 in b1, and c3 in b2 | K1, no base: copies b1, b2 whole; use record dropped at `complete` | b1, b2 | K1: b1, b2 |
| 1 | c3 overwritten by c5, in b3; compaction moves c1, c2 from b1 into b4, and b1 is deleted | — | b1, b2 | K1 |
| 2 | snapshot 11 sees c1, c2 (in b4) and c5 (in b3) | K2, base K1: c1, c2 found in folder b1, reused; b3 copied | b1, b2, b3 | K1; K2: b1, b3 |
| 3 | K1 expires | `expired`; sweep marks b1, b3 from K2, lists b1, b2, b3, deletes b2 | b1, b3 | K2 |
| 4 | — | `Verify` K2: reads b1 and b3, decodes c1, c2, c5 against their hashes: ok | b1, b3 | K2 |

At t2 the copy put one block, not two: b4 is not copied, because its chunks were
already in the folder under b1's name. At t3, b2's chunk c3 is named by no retained
manifest.

**Worked example: restore after the bucket is lost** — `S-snap-copy-restore`.
`ns-photos`'s bucket is lost. Installation C holds `ns-photos`'s material and
header key.

| t | C | `vault` | target namespace `ns-photos-r` |
| --- | --- | --- | --- |
| 0 | lists `vault`: K2 `complete`; header material held | — | — |
| 1 | creates `ns-photos-r` with a new ID, scope and keys; claim `owned` | — | empty |
| 2 | stages: ranged gets of c1, c2 from b1 and c5 from b3; decodes, checks hashes; carves b9 under the new scope; puts it under an intent | read | b9 {c1, c2, c5}, staged |
| 3 | imports K2's tree with new FileIDs; refs adopt c1, c2, c5; counts recomputed; audit clean | — | staged |
| 4 | publishes share `photos-r` | — | served |

Meanwhile the installation that held `ns-photos` keeps failing its copies,
because the namespace's blocks are gone. Its policy never expires K2, the newest
complete copy.

## 4. Moving a namespace between installations

### 4.1 One installation per namespace, proven by a claim

Exactly one installation holds a namespace: only it puts, sweeps, relocates or
collects there, and only its metadata store holds the namespace's records. Which
one is in its configuration and in the **claim**: the control object of role
`claim` ([RFC 4 §4.13](rfc-4-remote-tier.md#4.13%20Control%20objects)) in the namespace's folder, never listed as a block,
like the health object ([RFC 4 §4.7](rfc-4-remote-tier.md#4.7%20Health%20is%20one%20probe%20call)). It holds the installation's identity,
the claim epoch, the holder's **instance nonce**, and state `owned` or
`released` (with the digest of the move export).

**An installation's identity changes when it is copied.** The identity minted
when the installation is created ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)) is half of it; the other half
is an **instance** part, recorded with it, that a copy of the installation's
disks does not keep. At every start the installation reads the platform's
machine-generation identifier, where the platform offers one that changes when
a virtual machine is cloned or restored from an image, and compares it with the
one it recorded. A difference means this process runs on a copy: it mints a new
instance part, and holds no namespace — it does not write, collect or delete in
any — until an operator states which of the copies is the installation. A copy
is never silently a second holder. A copy may hold a metadata store older than
the one last served — a VM restored from an image — and nothing records that
it is older, so once the operator confirms a copy, or the nonce check
(below) has stopped one, the confirmed installation runs
[RFC 9 §3.7](rfc-9-gc.md#3.7%20Trash)'s settle of retired blocks before it serves, adopts, clones or
collects, as for any store opened at an older state.

**The claim catches a copy the platform does not report.** At every start, and
at every `Recheck`, the holder rewrites its claim with a fresh instance nonce, in
this order, each step done before the next:

1. **read** the claim. If it names this installation's identity with a nonce
   other than the two this installation recorded — the last it wrote and the one
   it intended to write next — the reader stops (below), and writes nothing;
2. **record** the new nonce as the intended one, committed in its own metadata
   store, beside the instance part ([RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities));
3. **put** the claim with that nonce, then record it as the last written.

A claim read that names this installation's identity but carries a nonce it did
not record means another process with the same identity is writing it: the
reader **MUST** stop writing to the namespace, and its GC **MUST** issue no
further delete, at once, and alert. Reading before rewriting is what lets the
original win against a stale image: a disk image restored from yesterday records
yesterday's nonces, finds the original's newer one at its first start, and stops
before its own put; rewriting first would let the stale copy overwrite the
claim and fence the original instead, after which its GC deletes blocks only the
original's newer records name. Recording before the put is what lets a holder
that crashed between the two recognise its own claim at restart. Two copies
started at once from one image fence each other within one `Recheck` period: the
one that reads the other's nonce stops, and only one keeps writing.

> decision: which copy survives is decided by timing, not by which is the
> original, and the alert names both so an operator can swap them. The cost is
> one claim put per namespace per `Recheck` period. Make the claim a conditional
> write ([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)) if a fleet's claim puts show in its request costs.
Reading it is a step of the store's capability check
([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)). Installations that share only the bucket check each other
through it:

- GC **MUST** read the claim at every `Recheck` and before each batch of deletes,
  and **MUST** issue no delete while the claim does not name its installation as
  `owned` ([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)). A GC process **MUST NOT** issue a delete more than
  two `Recheck` periods after it issued its last read of a claim naming it, by its
  own clock — from when the read was sent, not when it returned, so a slow read
  cannot stretch the window.
- An installation **MUST** open a namespace for writing only while the claim names
  it `owned` and its own record of the namespace is `serving`, and **MUST** stop
  writing when a claim read on the `Recheck` period says otherwise.
- An import **MUST** take the claim only from a claim that is `released` with the
  digest of the export being imported, or, for a recovery import ([§3.3](#3.3%20Restore)), on
  the operator's statement that the installation holding it is gone. It writes the
  claim `owned` at the next epoch. After a recovery import it then waits
  *T* × (1 + ρ) + σ, with *T* two `Recheck` periods and ρ and σ the clock-rate
  and clock-offset bounds of [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile), before it puts, adopts, resolves a
  hint or deletes, so that a partitioned old installation's GC, counting its two
  periods on a clock that may run slow, has fenced itself.

**Every claim put records its nonce first.** Every path that writes a claim — a
start, a `Recheck`, a move's or a recovery's import, a re-home's new namespace
([§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), a restore into a new namespace ([§3.4.5](#3.4.5%20Restore%20into%20a%20new%20namespace)) — records the nonce it
intends to write in its own metadata store before the put, and records it as the
last written after, as steps 2–3 above do. The record of a namespace's claim
nonces (`NS‖ns‖claim`, [RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)) is installation-local and **MUST
NOT** be exported: an import that carried the old holder's nonces would find its
own claim naming a nonce it never recorded at its first `Recheck`, and fence
itself.

> decision: the claim is a check, not a lock: the remote contract has no
> conditional put, so two installations that both believe they hold a namespace
> can both write it. The order of [§4.2](#4.2%20The%20move%2C%20step%20by%20step) prevents that, and the
> self-fence bounds a partitioned one by clocks within [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)'s clock
> bound, which rests on the hosts' time synchronisation. Make the claim a lock if
> the contract gains a conditional write.

### 4.2 The move, step by step

Until replication spans installations, a migration is a move, from installation A
to installation B. The bulk of the metadata is copied while A keeps serving; the
freeze carries only what changed since.

![Moving a namespace](img/rfc12-migration.svg)

1. **Pre-flight.** A writes the export's header alone; B checks prefix, key scope
   and material against it ([§4.4](#4.4%20Key%20scope%20and%20material)), and that it configures every backup
   location a use record or a folder record ([§3.4.1](#3.4.1%20Layout%20at%20the%20location)) of the namespace
   names, whose expiry and sweeps it will run.
2. **Pre-seed, while A serves.** A takes a **base cut** of every share in the
   namespace: an ordinary snapshot held by a use record of kind `move`
   ([§3.2](#3.2%20A%20backup%20holds%20its%20snapshot)), not by a lock, so the move can delete it. A then writes the
   namespace's **GC pause** record, which GC reads before every pass and every
   batch and which survives a restart: a paused namespace gets no relocation,
   delete or collection. Every relocation commit, every transaction that marks a
   block deleted, and every retirement of an unrecorded object a listing found
   reads the pause record with conflict tracking, so writing it aborts each one
   not yet committed; one already committed is in the records the export reads,
   its old block's delete waiting with every other delete to become B's
   backlog. A cooperative move therefore needs no clock: the claim's self-fence
   ([§4.1](#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)) is only the backstop for a process that does not see the pause. Retirement is not
   paused: it is decided where a count reaches zero
   ([RFC 9 §2.2](rfc-9-gc.md#2.2%20Retirement%20is%20decided%20where%20the%20count%20reaches%20zero)), an adoption undoes it, and the delta carries the
   records it changes. Once every base cut is `complete`, so that no offload can
   still write a ref a base cut sees, A writes an export of kind `move-base`: every
   record the base cuts see, all history, every chunk and block record, and the
   principals they name. B stages it, rebuilds its derived indexes — reverse ref
   keys, the died index, the version-floor index, the GC index — and recomputes
   its counts as it stages, and publishes nothing. From here until the
   move ends, the namespace refuses share creation, share deletion, clones,
   backups of either kind and re-homes ([§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) with `ErrMoving`, and defers snapshot deletion and pruning; a deletion,
   clone or backup already running finishes first.
3. **Pre-drain.** A expedites offload until the namespace's dirty and held bytes
   would drain within half of `migration.freeze_timeout` at the measured offload
   rate.
4. **Freeze, durably.** A records the namespace as `moving`, which keeps it closed
   across a restart; closes every shard's cut gate on every share and keeps it
   closed; pauses removal batches; and offloads everything dirty or held, since B
   cannot read A's journals. An operation that arrives during the freeze waits
   at most `migration.hold_reply`, then is answered with the retry-later error
   `ErrDelay` ([RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model)), which NFS clients receive as `NFS4ERR_DELAY` or
   `NFS3ERR_JUKEBOX` and retry. An SMB request is kept pending with an interim
   response instead, under the same hold an adapter keeps for a request refused
   `ErrGrace` ([RFC 17 §3.2](rfc-17-vfs.md#3.2%20Callbacks)), bounded by `migration.freeze_timeout` plus
   35 s; if the namespace has moved when the freeze ends, the connection is
   dropped so durable handles reconnect at the new owner. No NFS call is held for
   the whole freeze, and none is failed outright. The freeze **MUST** be bounded by
   `migration.freeze_timeout`; one that cannot finish in time aborts the move.
5. **Stop A's writers and GC** for the namespace, and join them: offload loops,
   relocation, the deleter, collection ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)).
6. **Delta.** A writes an export of kind `move-delta`, naming the base's digest:
   - every record whose change sequence is above its base cut's — live or
     history, per-file or per-share, and every chunk and block record above the
     namespace's. `born` cannot select them: an offload that commits late writes a
     live ref with `born` below the base, and a narrowed ref keeps its `born`
     ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). Nothing the base saw was deleted meanwhile: a live base cut
     turns every drop of it into a move to history, and the paused GC deletes no
     chunk or block record;
   - every record under each share's prefix — snapshots, cuts, grants, quotas,
     usage, locks and use records — and every per-file record that carries no
     `born` — removals, pending releases, durable opens — which B takes in place
     of the base's, so a deletion among them carries too;
   - every put intent ([RFC 9 §3.2](rfc-9-gc.md#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded)), and the principals the delta names.
7. **Ready at B, then release.** B stages the delta over the base
   ([§5.2](#5.2%20Import%20is%20staged%20and%20published%20atomically)), applying its changes to the indexes and counts it built from
   the base, verifies it, and reports **ready**: everything but the publish is
   done. Only then does A write the claim `released` with the digest over base
   and delta, and record the namespace `released`. Everything up to ready runs
   inside `migration.freeze_timeout`, and a failure or a timeout before it
   aborts the move as below; no unbounded or unabortable step follows the
   release.
8. **Claim at B.** B publishes the staged import — one transaction — writes the
   claim `owned` at the next epoch, and starts the shares. Clients reconnect to
   B.
9. **Drop at A.** A first calls `Forget` for each of the namespace's shares'
   tags in every journal that holds one ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)), and makes no per-file
   `Delete`: one header-only record per tag that drops every extent, removal
   marker and hold mark of its files — every record of the tag below the forget
   record's sequence number, whatever its version — which no caller settles and
   `Since` never yields. So no extent or marker of a
   moved file stays in A's journals, and a later move back finds none to replay
   as a removal. Step 4 offloaded all of
   them, so this discards nothing B lacks. A then deletes the namespace's records
   from its metadata store **without releasing them**: no refcount is
   decremented, no GC index key is written, nothing is swept, and nothing in the
   remote store is deleted. A crash between the two resumes the `Forget` calls
   at A's next start, before anything is served, from the namespace's
   `released` record.

B drops the GC pause record at publish, then deletes each move use record and its
base cut as an ordinary snapshot. The freeze lasts the drain plus the delta. The
delta exports only the changes since the base, but selecting them is one
sequential scan of the namespace's keys filtered by change sequence: metadata
reads only, no remote I/O.

> ponytail: the delta is found by a filtered scan, O(the namespace's keys), and
> sent in O(changes). Upgrade to a per-namespace change log written with each
> commit when the freeze's scan time shows in a move benchmark.

The pause from step 2 grows the namespace by what relocation and deletes would have reclaimed during the
pre-seed.

**Aborting.** Before A writes the claim `released` in step 7, A **MAY** abort: it records the namespace `serving`,
deletes the GC pause record, deletes each move use record and then its base cut,
and reopens the gates; B drops its staging.
After that write, A **MUST NOT** re-claim, reopen or write the namespace unless the
operator states that B has not published the import, which A cannot learn itself;
it then writes the claim `owned` at a higher epoch. A move keeps share identities
and FileIDs ([§4.5](#4.5%20Versions%20and%20FileIDs%20on%20import)), so clients' handles **SHOULD** stay valid at B.

**Worked example: a pre-seeded move** — `S-snap-move-preseed`. Namespace
`ns-photos` holds share `photos`, 10⁷ files, one shard, at cut 19.

| t | A | B | clients |
| --- | --- | --- | --- |
| 0 | pre-flight header | checks scope, prefix, material: ok | writing to A |
| 1 | base cut 20, held by a move use record; GC pause record written, one relocation in flight aborts; 20 `complete` after 40 s; `move-base` export, 10⁷ files, 3 h | stages it | writing to A throughout |
| 2 | pre-drain: 40 GiB dirty down to 2 GiB | — | writing to A, slower |
| 3 | `moving`; gates closed; drains 2 GiB in 20 s; joins writers and GC | — | calls answered retry-later after 1 s, and retried |
| 4 | `move-delta`: 3×10⁴ records with change sequence above cut 20's — among them the kept part of a ref a truncate narrowed at t1, still `born` 12 — all share-prefix records, intents; 5 s | stages it over the base, applies it to the indexes and counts built at t1, verifies; ready | calls wait |
| 5 | claim `released` with the digest | — | calls retried |
| 6 | — | publishes; claim `owned` at epoch 4; serves | reconnect to B: 40 s after t3 |
| 7 | drops its records | drops the pause record; deletes the move use record, then base cut 20 | — |

### 4.3 GC across installations on one bucket

No namespace is ever served by two GC services:

- **The deleter** acts only on blocks its own store records, and only after
  checking its own reverse ref index ([RFC 9 §2.3](rfc-9-gc.md#2.3%20The%20absence%20of%20a%20record%20proves%20nothing),
  [RFC 9 §3.5](rfc-9-gc.md#3.5%20The%20deleter%20verifies%20before%20it%20deletes)). In a move A's relocation and deletes are paused by the
  durable pause record from the pre-seed and its GC joined before release, and A records nothing after step 9; B counts every ref and writes
  its reverse keys, since every share and snapshot moved.
- **Retired and deleted blocks, and intents,** move too. B rebuilds the GC index
  from the block records ([RFC 9 §7.4](rfc-9-gc.md#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt)) and resumes them as its own trash and
  delete backlog, keeping each `not_before` ([RFC 9 §3.2](rfc-9-gc.md#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded)); a retired block's
  chunk records move with it, so an adoption at B still resurrects it. Every
  imported intent was written by A, whose writers stopped and were joined at
  step 5, so none can still be followed by a commit: B treats each as abandoned
  ([RFC 9 §3.4](rfc-9-gc.md#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete)) and collects it with its object, if any
  ([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)). No epoch comparison is involved.
- **Collection** ([RFC 9 §5.3](rfc-9-gc.md#5.3%20It%20runs%20only%20where%20the%20namespace%20is%20proven)) runs only where claim and configuration agree;
  objects A put and never committed are B's to collect.
- **A stale process of A** that puts after the release only leaks: its commit
  finds no intent, and the object is left to B's listing backstop. One that
  deletes is stopped by the claim check before each batch and by its own fence
  two `Recheck` periods after its last good claim read ([§4.1](#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)). A process
  that escapes both — paused past the clock bound between its check and its
  delete — can delete a block B resurrected or minted; that is the ceiling the
  claim's decision states.

A namespace **MUST NOT** be split between installations ([§2.1](#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)).

### 4.4 Key scope and material

A namespace's key scope, and every other bound setting of it
([RFC 13 §5.1](rfc-13-configuration.md#5.1%20A%20bound%20setting%20refuses%20change)), is fixed when it is created and travels in every export's
header ([§5.1](#5.1%20Layout)): the prefix, the key scope, the chunking target and
bounds, whether it encrypts, the IDs of its chunk-ID and chunking keys, and the
block format version it writes. B **MUST** use them for the namespace, and
**MUST** refuse with `ErrScopeMismatch`, naming the setting, an import any of
whose bound settings differs from its configuration: names derived under
another scope would never match the imported records ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)), another
chunking target would re-cut every file B writes against content cut the old
way, and a namespace that encrypts at A must not take plaintext writes at B.
Each share's fold rule and entry-digest hash key travel the same way, and B
**MUST** create the share with them, never draw its own: every entry key and
every client's listing cookie was derived under them
([RFC 7 §3.4](rfc-7-namespace-metadata.md#3.4%20Enumeration)).

The export lists every material ID in the census of the blocks it names, each
with its fingerprint, and B **MUST** find each held by its provider, with the same
fingerprint, before staging a record ([RFC 5 §2.7](rfc-5-transforms.md#2.7%20Failures)): material unavailable
now, or unknown to B's provider, refuses the import as retryable, naming the IDs
— loading the keys ends it; destroyed material refuses it for good, naming the
IDs. An import **MUST NOT** proceed and leave reads to fail as corrupt.
B needs no particular *current* material: its own chain writes new blocks
([RFC 5 §2.8](rfc-5-transforms.md#2.8%20The%20chain%20ID)).

The namespace's **data keys**, **header keys**, **chunk-ID key**, **chunking
key** and **export keys** are kept as key records, each held as
[RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)'s table says for the namespace's kind: wrapped under a
master key, except a non-encrypting namespace's chunk-ID key, kept in the clear
beside its blocks. The export carries every key record so held, and the IDs of
the master keys that wrap them, and never a master key. B **MUST** hold every
master key the export names, and each key record **MUST** unwrap and match its
fingerprint, before staging: without the
export key it cannot read the export, without the chunk-ID key it cannot verify a
chunk, without a header key it cannot verify A's block headers, and without the
chunking key its new writes chunk differently from every block A wrote.

**B re-wraps what it imports; A keeps what it exported under.** At publish, B
**MUST** re-wrap every imported key record under its own current master key
([RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)), in the publishing transaction, so after a move no key record
of B's depends on a master key A may rotate away and destroy. A **MUST**, when
it writes a move's export, record a durable **export pin** naming each master
key the export's wrappings use, and its material provider **MUST** refuse to
destroy a pinned master key with `ErrMaterialInUse` until an operator releases
the pin: A cannot see B's re-wrap, and B's retained exports from before its
re-wrap still name it. The destroy check runs per installation, against that
installation's own key records, retained exports and pins.

### 4.5 Versions and FileIDs on import

**A journal attaching an imported share holds none of its extents.** A share
moved back to an installation that once held it — A to B and back to A — arrives
with FileIDs A's journals may have held. Attaching a share whose tag the journal
already knows as an import ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)) **MUST** first `Forget` that tag —
dropping its extents and markers through one header-only record — or be refused
naming the tag; otherwise A would serve its stale,
clean extents keyed by those FileIDs over what B wrote since. Step 9's `Forget`
makes this the rare case of a crash, not the rule.

**Versions.** A version B's journal assigns to an imported file **MUST** exceed
every version imported for it, or a new write loses precedence to older content
and is dropped at its commit ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)). The journals at B were opened
before the share existed there, so the floor they opened with does not cover it.
So before a journal serves a share it did not serve when it opened — an imported
share of a move, a recovery, a clone or a restore — it **MUST** raise its version
counter above that share's version floor: the highest version B's metadata now
records for any file of the share, every ref's `newest` and every FileData's
`applied` ([RFC 6 §8.3](rfc-6-block-metadata.md#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor)). The publish makes that floor readable; the
share's first write waits for the raise. Journals opened later include the
share in their floor at open, as for any share ([RFC 1 §9.1](rfc-1-journal.md#9.1%20Rebuilding)). The export
needs no version field of its own: the floor is computed from the records it
imported. A move keeps every `born` and `died`, each snapshot's cut number and
each share's `Cut` record: they are share-local and involve no journal version.
A clone or restore has no snapshots and no history, writes every record with
`born` 0, and starts at cut 0.

**File numbers.** A move, a clone and a restore keep each file's `Number`
([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)). The transaction that publishes an import **MUST** raise the
share's file-number allocator above the highest `Number` it imported, before any
create in the share: otherwise B's next create is issued a number a restored
file holds, and two files report one id. A move carries the allocator record
itself, and the raise then changes nothing.

**FileIDs.** A FileID is unique across a store and keys content in every journal
([RFC 0 §3](rfc-0-data-lifecycle.md#3.%20Identity)):

- a **move** keeps FileIDs and share identities, and **MUST** be refused if any
  imported FileID already exists at B;
- a **clone** or **restore**, and each detached snapshot a recovery imports, gets
  new FileIDs and a new share identity, since one snapshot may be restored many
  times.

**Handles cannot alias across shares.** Every per-file key carries its share's
identity ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), and a handle names the share and the file
([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)). A clone or restore gets a new share identity, so no handle to
the source can resolve into it.

### 4.6 After replication

**(cluster)** Once replication and sharding span installations, two installations **MAY** serve
one namespace under [RFC 10](rfc-10-journal-replication.md) and [RFC 11](rfc-11-ownership.md)'s rules — one
metadata store, primaries fenced by epoch, handover
([RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover)) in place of a move. This document does not specify that;
the move of [§4.2](#4.2%20The%20move%2C%20step%20by%20step) remains the way to change which metadata store holds a
namespace.

### 4.7 Moving one share out of a shared namespace

A share in a namespace shared with other shares — a clone with its source, while
deduplication is deferred ([§2.1](#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)) — cannot be moved alone. Its chunks are
counted together with theirs. A **re-home** fixes that: it copies the share's
content into a new namespace of its own while the share keeps serving. After
that, the share moves alone ([§4.2](#4.2%20The%20move%2C%20step%20by%20step)). A re-home is the migration
[RFC 13 §5.1](rfc-13-configuration.md#5.1%20A%20bound%20setting%20refuses%20change) names as the only way a share's namespace binding changes while the
share holds content, and so also the only way to change a bound setting — the
chunking target, the keys, whether the namespace encrypts — for content already
written: the new namespace is created with the new settings. A share that is its
namespace's only share is re-homed for that reason alone.

> decision: shares created into an existing namespace are deferred with
> deduplication, but re-home is not: it is how a clone leaves its source's
> namespace and how any share changes a bound setting. Multi-share namespaces
> built for deduplication return, with what re-home must do for them, when the
> dedup lookup does.

A re-home is a data copy. Every chunk the share's refs name, live and history, is
read from the old namespace N and written into new blocks in the new namespace N'.
Deduplication against N's other shares is lost by design. A chunk they also hold
ends up stored in both namespaces. Within the share, deduplication is kept: N'
stores each of its chunks once.

**A ref names its namespace by generation.** A share's record lists its namespaces
by **generation**: one entry outside a re-home, and two during one, N at *g* and N'
at *g* + 1. Every ref carries the generation of the namespace its chunk is counted
in. A read, an adoption or a drop acts on the chunk record in that namespace. The
share keeps `old_refs`, a counter ([RFC 16 §4.4](rfc-16-metadata-store.md#4.4%20Counters%20that%20many%20writers%20change)) of its refs at *g*, which every
transaction that writes or drops such a ref changes. A clone of a ref by reference
within the re-homing share adopts in the namespace that ref names. A clone or
server-side copy between two shares in different namespaces is refused with
`ErrCrossNamespace` (NFS `NFS4ERR_XDEV`, SMB `STATUS_NOT_SUPPORTED`), as it is
between any two namespaces ([RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy)), and the client falls back to reading
and writing the bytes.

**Steps.**

1. **Start.** A re-home is refused:
   - with `ErrMoving` during a move of N;
   - with `ErrRehoming` while another re-home of the share runs.

   An unexpired catalog backup of the share does not refuse a re-home, although
   its hints and its recovery import name chunks in N. Its blocks are kept by
   **parking** instead (step 3): the share's refs leave N's live set but stay
   counted in N until the last catalog backup taken before the re-home finished
   has expired. A copying backup needs no parking: it holds its own blocks.

   A share that is N's only share is re-homed like any other; N is left empty
   and is deleted once its GC has collected what the switches retired.

   One transaction then creates N', with a new ID, prefix, key scope and keys and
   its claim written `owned` by this installation. The same transaction adds N' to
   the share's record at *g* + 1 as its **write namespace**, and writes the
   **re-home record** `S‖id‖rh`, holding the cursor and the pass number. Until the
   re-home finishes, the share refuses clones of its snapshots and new catalog
   backups with `ErrRehoming`, and a move of N or of N' is refused with
   `ErrRehoming` too. Snapshots and copying backups go on ([§3.4](#3.4%20Copying%20backups)). A clone or
   catalog backup that is already running finishes first.
2. **Switch writes.** Each primary of the share's shards is told of the change. It
   carves every new offer under N', and adopts — once deduplication is added —
   only from N'. Chunk IDs are fixed when an offer is carved, before any put
   attempt, so **an offer captures its namespace and that namespace's keys when it
   is carved** ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope), [RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline)), and every later step of it — the plan,
   the names it mints, the puts, the commit — uses the captured namespace, not
   the write namespace current when that step runs. The primary joins every offer
   captured under N, through its commit or its abandonment, before it
   acknowledges the switch. The put-intent step **MUST** refuse, with
   `ErrScopeMismatch`, a plan whose chunk IDs are keyed under a namespace other
   than the one it mints names into, so an offer carved under N can never put
   N-keyed IDs into N'. From then on, offload commits write refs at *g* + 1.
3. **Copy and switch, in batches.** The re-home walks the share's ref keys, live
   and history, in key order from its cursor. It takes refs at *g* per batch, as
   many as the key budget K allows ([RFC 6 §5.2](rfc-6-block-metadata.md#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed)).
   - **Copy.** Chunk IDs are keyed per namespace ([RFC 2 §4.1](rfc-2-carver.md#4.1%20A%20chunk)), so a chunk
     has one ID in N and another in N'. For each chunk the batch names, the
     re-home reads it from N — a ranged get of its body
     ([RFC 4 §4.4](rfc-4-remote-tier.md#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly)), decoded and checked against its N ID ([RFC 5 §2.6](rfc-5-transforms.md#2.6%20The%20plaintext%20hash%20is%20the%20final%20check)) — and
     computes its N' ID. Where N' already has a live chunk record for that ID,
     nothing is put. Where N''s chunking settings differ from N's, or a chunk
     exceeds N''s chunk maximum, the re-home reads the bytes each ref covers
     and cuts them under N''s settings, and each old ref becomes the refs of its
     pieces. The chunks go to N''s block assembler, which puts new blocks under
     N''s scope and chain. Each put has a put intent
     and runs through a background flow of the syncer. This is relocation's read,
     mint and put ([RFC 9 §4.2](rfc-9-gc.md#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)), with the target in another namespace.
   - **Switch.** Once those puts have succeeded, one transaction:
     - consumes their intents and creates their block and chunk records in N';
     - reads each ref of the batch with conflict tracking, and requires it
       unchanged;
     - adopts each ref's chunk in N', by its N' ID, conditional on existence and
       resurrecting a retired block ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence), [RFC 9 §3.3](rfc-9-gc.md#3.3%20Adoption%20resurrects%20a%20retired%20block)) — normative here
       whatever is deferred with deduplication — and writes its reverse key there;
     - drops the ref from N: it decrements the chunk, removes the reverse key, and
       retires any block it leaves at zero ([RFC 9 §2.2](rfc-9-gc.md#2.2%20Retirement%20is%20decided%20where%20the%20count%20reaches%20zero)) — or, while an
       unexpired catalog backup of the share taken before the re-home finished
       exists, **parks** it: the ref moves to N's own parked prefix
       (`NS‖N‖pk‖…`, [RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), keyed by the share, keeping its count
       and reverse key, where no read, snapshot or listing sees it and only N's
       GC counts it. It is filed under N, not under the share's prefix, and owned
       by N's installation, so moving the share out later carries none of it and
       leaves none of N's counts behind;
     - rewrites each ref at *g* + 1, naming its N' chunk ID, with the same range,
       version, `born` and `died`; a ref that was re-cut becomes its pieces,
       each with those same values;
     - lowers `old_refs` and advances the cursor.

     A ref that changed since it was read makes the batch retry.
4. **Catch-up passes.** Refs at *g* can appear behind the cursor. A server-side
   copy by reference from another share of N is one source. A version moved to
   history under a key the cursor already passed is another. When a pass ends with
   `old_refs` above zero, the next pass scans only the share's records whose change
   sequence ([§4.2](#4.2%20The%20move%2C%20step%20by%20step)) is above the previous pass's start, and switches what it
   finds.
5. **Close, then finish.** When `old_refs` reads near zero after a pass, one
   transaction marks the re-home `closing`. From then on nothing can raise
   `old_refs`: a server-side copy by reference from N into the share copies
   bytes instead, as between any two namespaces, and every other transaction that touches a
   ref at *g* only moves it to history, which keeps its count, or drops it,
   which lowers it. The re-home then runs catch-up passes until a fold of the
   counter ([RFC 16 §4.4](rfc-16-metadata-store.md#4.4%20Counters%20that%20many%20writers%20change)) reads zero. Since the counter can only fall
   once `closing` is set, a folded zero stays zero, so the finish transaction
   reads the folded record and the `closing` flag — two keys, never the
   counter's unfolded deltas — makes N' the share's only namespace, deletes the
   re-home record, and lifts the refusals. A finish that had to read every
   unfolded delta would scan a range every writer of the share appends to, and
   conflict with each of them.

N's GC reclaims each chunk the share alone held, as its count reaches zero. Chunks
the other shares hold stay in N at their counts. Nothing in N is deleted on the
re-home's behalf.

**Parked refs outlive the backups that need them.** When the last catalog
backup of the share taken before the re-home finished expires, the parked refs
are dropped from N in batches ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)), decrementing each chunk, and N
can then empty. Until then a recovery import of such a backup resolves its hints
in N as it would before the re-home, and finds every block they name. N is not
deleted while it holds parked refs; a move of N carries them, being under N's
prefix, and a move of the re-homed share carries none. The parking
costs the space of the share's content in both namespaces for one catalog
retention.

**The switch is not a new version.** It rewrites which namespace counts a ref's
chunk, and nothing a snapshot reads. So it passes no cut gate, stamps nothing and
writes no history. Every snapshot and every live read returns the same bytes
before and after it. Snapshots of the share keep working throughout: every ref,
at either generation, counts its chunk in the namespace it names. Cuts, holds and
deletions run as usual. A deletion that drops a ref drops it at whichever
generation that ref names.

**Backups.** A re-home of a large share runs for weeks at its paced rate, and the
share's backups do not stop for it. Copying backups go on throughout: the copier
resolves each ref's chunk in the namespace its generation names, and copies that
namespace's block, whichever it is, into **N′'s folder** ([§3.4.1](#3.4.1%20Layout%20at%20the%20location)), under N′'s
folder lease alone. So a backup taken mid-re-home lists blocks of one folder,
each with the namespace that sealed it, and its census lists both namespaces'
material. **Its export carries both namespaces**: both IDs, both sets of key
records with the master-key IDs that wrap them, and both scopes, each chunk
record naming its namespace; it is filed under N′, and its state object names
N′'s folder only. N's sweep never sees it and needs not: no block it lists is
in N's folder, so a listing of one folder finds every backup its sweep must
honour. Its base is matched in N′'s folder, so an N block is copied there once
and reused by later copies until no ref names it. A restore from it decodes each chunk with
the material of the namespace it came from ([§3.4.5](#3.4.5%20Restore%20into%20a%20new%20namespace)). Catalog backups,
which hold blocks only through the namespace's counts, are refused while the
re-home runs and resume at its end naming N'. A policy that takes catalog
backups skips them during a re-home and counts each skip; a share that needs
backups through a long re-home is given a copying policy first.

**Progress, failure and pacing.**

- **The cursor is durable.** The re-home record keeps the cursor and the pass,
  advanced by each switch transaction. A restart resumes at the cursor.
- **A crash between the puts and the switch** leaves blocks in N' named only by
  their intents. N''s GC collects them ([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)), and the retried batch puts
  again.
- **Pacing.** The copy runs in the syncer's background class, under
  `rehome.rate`. A rate of 0 pauses it.
- **Cost.** Each byte the share references is read once and written once. There
  is one ranged get per chunk, and two chunk-record updates and one ref rewrite
  per ref.

> decision: a re-home cannot be cancelled once it has started, only paused. Half
> done, the share's content is split across two namespaces, and both count it
> correctly. Stopping there leaves the share serving correctly, and finishing is
> the only way back to one namespace. A reverse re-home into N would bring
> deduplication back, but it is not specified. Add it if operators start re-homes
> they regret.

> ponytail: a re-home reads each chunk with its own ranged get, even when most
> of a block's chunks belong to the share. Upgrade to one whole-block get when
> most of a block is named, as restore does ([§3.4.5](#3.4.5%20Restore%20into%20a%20new%20namespace)), when re-home request
> counts show in its cost.

**Worked example: re-homing one share while it serves** — `S-snap-rehome`. Shares
`vm1` and `vm2` share namespace `ns-vms`. `vm2`'s file `f` has live refs r1 → c1,
which `vm1` also holds, and r2 → c2. It also has a history ref h3 → c3, which
snapshot 4 of `vm2` sees.

| t | action | `ns-vms` counts | `ns-vm2` | `old_refs` |
| --- | --- | --- | --- | --- |
| 0 | start: `ns-vm2` created, generation 2 added to `vm2`; clones and backups of `vm2` refused | c1 2, c2 1, c3 1 | empty | 3 |
| 1 | `vm2`'s primary joins its attempts under `ns-vms` and acknowledges | — | — | 3 |
| 2 | a client writes `f`; its offload carves c4 into `ns-vm2`, ref r4 at generation 2 | — | c4 1 | 3 |
| 3 | batch 1: ranged gets of c1, c2, c3 from `ns-vms`; b9 put in `ns-vm2` under an intent | — | b9 stored, unrecorded | 3 |
| 4 | switch: b9 recorded; r1, r2, h3 adopt in `ns-vm2`, drop from `ns-vms`, rewritten at generation 2 with their `born` and `died` | c1 1, c2 0, c3 0: their blocks retire if nothing else is live in them | c1, c2, c3, c4 1 | 0 |
| 5 | `vm1` server-side copies a file into `vm2` by reference: ref r5 → c5 at generation 1 | c5 +1 | — | 1 |
| 6 | pass 2 scans records above pass 1's change sequence and switches r5 | c5 −1 | c5 1 | 0 |
| 7 | finish: `ns-vm2` is `vm2`'s only namespace | — | — | — |
| 8 | a read of snapshot 4 resolves h3 at generation 2 in `ns-vm2` | — | — | — |

Chunk names in the `ns-vm2` column stand for the same bytes under `ns-vm2`'s
own chunk IDs. `vm1` still reads c1 from `ns-vms`. `vm2` can now be moved to
another installation alone.

## 5. The export format

### 5.1 Layout

An export is one stream, written and read front to back, never seeked:

```
clear    = magic ‖ format version ‖ kind ‖ namespace id(s) ‖ export salt ‖
           sealing export-key ID ‖ current and retired export-key IDs ‖
           IDs of every master key whose wrapping it carries ‖
           the namespace's key records, each as RFC 5 Appendix B.2 holds it ‖ clear MAC
header   = (sealed) prefix, key scope and every other bound setting ‖
           per share: fold rule and entry-digest hash key ‖
           source installation ‖ share and snapshot ids ‖ cuts ‖ base digest
           (move-delta only) ‖ (material ID, fingerprint) of every census
           material ‖ record counts per section ‖ header MAC
section* = type ‖ frame* ‖ section MAC ‖ record count
frame    = length ‖ sealed records ‖ frame index      (frames of at most 4 MiB)
trailer  = end marker ‖ trailer MAC
```

**Sealed and authenticated.** Everything after the clear part is sealed under
keys derived from the namespace's export key and the export's own random salt,
exactly as [RFC 5 Appendix B.1](rfc-5-transforms.md#B.1%20How%20a%20chunk%20is%20encrypted) specifies, with golden vectors: AES-256-GCM-SIV
for each frame, its nonce the section index and the frame index, so no two frames
of one export share a nonce, and its associated data the export's salt, section
and frame index, so a frame moved, dropped, repeated or reordered fails;
HMAC-SHA256 for the clear, header, section and trailer MACs; HKDF-SHA256 with
32-byte outputs for every derived key. Each key record is wrapped as RFC 5
specifies, with associated data namespace ‖ kind ‖ key ID, so a record moved to
another namespace or kind fails to unwrap. The clear part says only what an
importer needs to find the keys: the namespace, the export-key IDs current and
retired, the master-key IDs, and the key records. The export key rotates
([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)); a retired one stays until no retained export names it. In an
encrypting namespace a reader of the location without the master key learns the
namespace ID and the export's size, and nothing of the tree, the names, the chunk
IDs or the principals; a writer without it cannot change a byte unnoticed,
including in the clear part, which the clear MAC covers once the export key is
unwrapped. A non-encrypting namespace's export is sealed and authenticated the
same way: its export key is wrapped under a master key like every namespace's
([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)), so reading or forging one needs that master key. Only its
chunk-ID key record is held unwrapped, in the clear, which lets a bucket reader
verify chunks and nothing more.

Kinds are `backup` and `backup-copy` (one snapshot), and `move-base` and
`move-delta` (a namespace). A `backup-copy` adds a `blocks` section, its manifest
([§3.4.2](#3.4.2%20What%20is%20copied)), after chunks and blocks, and its chunk records name folder
blocks.

- **Versioned.** A reader **MUST** refuse an unknown format version and read every
  version it once wrote; each record carries its own version.
- **Self-describing.** The clear part and the header alone suffice for
  pre-flight ([§4.2](#4.2%20The%20move%2C%20step%20by%20step)), to any installation holding the master key.
- **Verifiable.** Each frame's AEAD finds corruption within 4 MiB; section MACs
  and the trailer's MAC cover order and completeness. The trailer's MAC is the
  export's identity, its **digest**; a move's claim names the digest over its
  base's and its delta's ([§4.1](#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)).
- **Streamable.** Sections come in dependency order — namespace, tree, content
  (live and history refs, removals), chunks and blocks, put intents — with
  records in key order, so an importer holds one frame and writes as it reads.

Records are the logical records of [RFC 6](rfc-6-block-metadata.md) and [RFC 7](rfc-7-namespace-metadata.md), never a
backend's dump, so an export moves between metadata backends. Derived indexes are
never exported; the importer rebuilds them.

**What an export holds.** Everything under the share's prefix and the per-file
prefixes of that share, its history included; for a move, also everything under
the namespace's content-addressed prefixes, which are scoped by namespace
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)); plus the server-wide records its files and grants name — the
users and groups whose principals appear in ownership, ACLs, grants and quotas,
with their protocol-ID index rows, and every netgroup the share's export policy
names, **with its members**. **`Secret` records are never exported**: a user
arrives at B without credentials and is given new ones there. Nothing else
server-wide is copied.

**Netgroups are imported by name, with their members.** A client rule that names
a netgroup B lacks would match nothing at B, and a deny rule that matches
nothing turns into an allow for everyone it was meant to stop. So an import
adopts a netgroup B does not hold, reuses one B holds with the same members, and
**MUST** be refused with `ErrNetgroupCollision`, naming it, when B holds that
name with different members. The operator then renames one side or makes them
agree; nothing is merged silently.

**Principals are imported by ID.** A principal is an opaque ID, minted once and
never reissued ([RFC 7 §2.1](rfc-7-namespace-metadata.md#2.1%20File)). An import adopts a principal whose ID B does
not hold, reuses one B holds with an identical record — a principal moving back —
and **MUST** be refused, naming it, when B holds that ID for someone else or when
an imported protocol ID — a UID, GID or SID — already maps to a different
principal at B. It **MUST NOT** match principals by protocol ID: `u:1000` at A
and at B are unrelated until an operator maps them.

**Names and paths collide the same way.** An import is a create for the
control plane's naming rules ([RFC 16 §2.3.1](rfc-16-metadata-store.md#2.3.1%20Share%20names%2C%20paths%20and%20state)): it **MUST** be refused, before it
publishes anything and naming the collision, when a share's name collides under
the name fold with one B holds, when its path equals, contains or lies inside
another share's, or when a principal's name already names a different principal
at B. The import request **MAY** carry a new share name, path or principal name,
applied in the staged copy; nothing is renamed silently.

### 5.2 Import is staged and published atomically

An export is larger than one transaction. An import therefore writes into a
**staging area** of the target store, under an import identity no share serves
and no dedup lookup reads. When the trailer verifies and every check has passed —
the export's MACs, material, bound settings, FileIDs, principals, netgroups,
hints, counts recomputed, audit clean — one transaction
**publishes** it: the namespace or share record becomes active and the staging
identity is retired. A `move-base` stays staged until its delta arrives and is
published with it.

A torn, corrupt or refused import publishes nothing; its staging records are
removed by its failure path or, if interrupted, at the next start. An import is
never resumed from a partial staging area.

**A staged ref into a live namespace is counted and indexed.** A clone or restore
writes refs naming chunks its namespace's GC serves. Each staged ref raises its
chunk's count and writes its reverse-index key as it is written, so the deleter's
check and the audit ([RFC 9 §3.5](rfc-9-gc.md#3.5%20The%20deleter%20verifies%20before%20it%20deletes)) see it, and no GC pass can retire a
chunk before the publish. Only serving and dedup lookups skip staging. A failed
or interrupted clone or restore drops its staged refs and their keys by the
batching pattern of [RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation), decrementing each chunk. A move's or
recovery's namespace has no GC running at B until it publishes, and its counts are
recomputed.

## 6. API surface

### 6.1 Interfaces

Signatures are indicative; the obligations above are normative.

```go
// Snapshots is the per-share surface.
type Snapshots interface {
	Create(ctx context.Context, share ShareID, name string) (SnapshotID, error)
	// CreateSubtree cuts the shard rooted at root and the shards beneath it (§2.10).
	CreateSubtree(ctx context.Context, share ShareID, root FileID, name string) (SnapshotID, error)
	List(ctx context.Context, share ShareID) ([]SnapshotInfo, error) // with bytes freed if deleted, on request
	Lock(ctx context.Context, id SnapshotID, until time.Time, mode LockMode) error // extends only, up to lock_max (§2.7)
	// Override shortens or removes a governance lock; needs the override right (§2.7).
	Override(ctx context.Context, id SnapshotID, until time.Time) error
	Delete(ctx context.Context, id SnapshotID) error
	Clone(ctx context.Context, id SnapshotID, spec ShareSpec) (ShareID, error)
	SetPolicy(ctx context.Context, share ShareID, p *Policy) error // nil removes it
}

type Backups interface {
	Backup(ctx context.Context, id SnapshotID, loc string, retain time.Duration) (BackupID, error)
	// Copy writes a copying backup, incremental against its base (§3.4).
	Copy(ctx context.Context, id SnapshotID, loc string, retain time.Duration) (BackupID, error)
	Verify(ctx context.Context, loc string, id BackupID) (VerifyReport, error) // marks it damaged on failure
	List(ctx context.Context, loc string, ns NamespaceID) ([]BackupInfo, error)
	Expire(ctx context.Context, loc string, id BackupID) error // then sweeps a copying backup's folder
	// Restore clones, or runs a recovery import if holderGone (§3.3). For a
	// copying backup, spec.Namespace names the new namespace it re-encodes into
	// (§3.4.5), and holderGone is ignored.
	Restore(ctx context.Context, loc string, id BackupID, spec ShareSpec, holderGone bool) (ShareID, error)
}

// Rehome copies one share into a new namespace of its own (§4.7).
type Rehome interface {
	Start(ctx context.Context, share ShareID, ns NamespaceSpec) error
	Status(ctx context.Context, share ShareID) (RehomeStatus, error) // pass, cursor, old_refs, bytes copied
	SetRate(ctx context.Context, share ShareID, bytesPerSec int64) error // 0 pauses; there is no cancel
}

// Migration moves a namespace (§4.2).
type Migration interface {
	PreSeed(ctx context.Context, ns NamespaceID, w io.Writer) (Digest, error)          // steps 1–2
	Freeze(ctx context.Context, ns NamespaceID, base Digest, w io.Writer) (Digest, error) // steps 3–7
	Import(ctx context.Context, r io.Reader) (ImportReport, error)                     // base, then delta
	Drop(ctx context.Context, ns NamespaceID, imported Digest) error                   // step 9
	Abort(ctx context.Context, ns NamespaceID, notPublished bool) error                // after step 7 only with notPublished
}

type Policy struct {
	Every          time.Duration
	Keep           map[Interval]int // hourly, daily, weekly, monthly → N
	TimeZone       *time.Location   // periods of Keep; nil is UTC (§2.7)
	Lock           time.Duration    // 0: policy snapshots are not locked
	LockMode       LockMode         // compliance or governance
	BackupLocation string           // empty: no backups
	BackupKind     BackupKind       // catalog or copy
	BackupRetain   time.Duration    // never expires the newest complete copy (§3.4.4)
	VerifyEvery    time.Duration    // 0: copying backups are not verified on a period
}
```

Errors are a closed set: `ErrHeld`, `ErrLocked`, `ErrHoldBacklog`,
`ErrSnapshotReserve`, `ErrMoving`, `ErrFreezeTimeout`, `ErrNotClaimed`,
`ErrPrincipalCollision`, `ErrMaterialMissing` (retryable when only unavailable),
`ErrScopeMismatch`, `ErrFileIDExists`, `ErrExportCorrupt`, `ErrNotShardRoot`,
`ErrSubtreeSnapshot`, `ErrLocation`, `ErrBackupDamaged`, `ErrRehoming`,
`ErrLockTooLong`, `ErrSnapshotLimit`, `ErrPolicyPeriod` and
`ErrNetgroupCollision`. `ErrScopeMismatch` covers every bound setting
([§4.4](#4.4%20Key%20scope%20and%20material)).

What other components gain:

- **Block and namespace metadata** ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref), [RFC 7](rfc-7-namespace-metadata.md)): `born` on
  every versioned record, stamped at the committing transaction; `died` from the
  successor; the move to history, or the drop, tested against the live cuts; a
  covering lookup and listings at a cut; the died index and the batched history
  drop of [§2.8](#2.8%20Deleting); history moves that read their `LiveCut` with conflict
  tracking; the per-share and per-namespace change sequence; use records with
  deadlines, and locks; staged, publishable imports.
- **The journal** ([RFC 1 §3.11](rfc-1-journal.md#3.11%20Snapshot%20holds)): holds, their marks and their release;
  the cut of each existence commit kept with the versions it covered; a version
  superseded only once its successor's existence commits.
- **Replication** ([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)): the hold and the stamp as replicated
  operations, each replica's held and dirty bytes returned with its `Hold`
  acknowledgement, and held superseded versions with their stamps shipped by
  takeover, handover and moves between shards.
- **Ownership** ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries), [RFC 11 §8.1](rfc-11-ownership.md#8.1%20Operations%20across%20shards)): a move's commit writes the
  receiving shard's hold records; a cross-shard transaction admitted at all its
  shards' gates or at none.
- **GC** ([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)): the namespace's pause record, read before every pass
  and batch, and by every relocation commit with conflict tracking.
- **The offload** ([RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline)): held versions committed straight to history and
  before newer versions of their range; runs split at live hold marks.
- **Subtree snapshots** ([§2.10](#2.10%20Subtree%20snapshots)): `LiveCut` records a cut's kind and covered
  set; `SubCut(share, shard)`; the history test by coverage; the died index
  carrying each version's shard; moves of files refused on a covered shard
  ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)).
- **Copying backups** ([§3.4](#3.4%20Copying%20backups)): the folder record with its census; material
  removal checking every folder census ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)); a use record of kind
  `copy`; the block folder opened as a remote store of its own.
- **The journal at import** ([RFC 1 §9.1](rfc-1-journal.md#9.1%20Rebuilding)): a raise of its version counter above a
  share's version floor before it serves a share it did not open with.
- **The metadata store** ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)): namespace key records; the died index
  keyed by shard first; each snapshot's ordinal in `LiveCut`; the folder
  record's lease number; a retired share; the allocator raise at publish.
- **Re-homes** ([§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)): a share's namespaces by generation, and each ref's
  generation, which reads, adoptions, drops and the dedup oracle resolve in; the
  `old_refs` counter; the re-home record; a write namespace that each offer
  captures, with its keys, when it is carved, and an intent step that refuses a
  plan keyed under another namespace ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)); the share's parked refs
  under the old namespace's own prefix.
- **The journal at a move** ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)): the drop calls `Forget` on the moved
  shares' tags in every journal; attaching an imported share's tag `Forget`s,
  or refuses, any extent of it the journal still holds.
- **The engine at a cut** ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)): cut points per journal taken after the
  gate closes, the pre-cut existence commit up to them behind that gate, and no
  existence commit of any kind above them while the gate is closed.

### 6.2 Configuration

Snapshot policies, locks, backup locations and moves are control-plane records,
set through the API ([RFC 13 §2.1](rfc-13-configuration.md#2.1%20The%20control%20plane%20is%20the%20source)) like every other record, with the scope and
class [RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings) gives each: `snapshots.hold_bound` and
`snapshots.reserve` per share; `snapshots.hold_journal_fraction`,
`snapshots.gate_max`, `snapshots.cut_deadline`, `snapshots.lock_max`, `migration.*`, `backups.copy_rate`
and `backups.max_copy_time` per installation; each backup location its own
installation-scoped record ([RFC 13 §2.5](rfc-13-configuration.md#2.5%20A%20backup%20location%20is%20its%20own%20record)), which a policy names. The shape below
is how a provisioning file declares them
([RFC 13 §2.4](rfc-13-configuration.md#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).

```yaml
snapshots:
  directory: .snapshot          # the browse directory (§2.5)
  hold_bound: 64GiB             # held bytes per share before a cut is refused (§2.4)
  hold_journal_fraction: 0.25   # held share of a journal's capacity, all shares (§2.4)
  gate_max: 1s                  # longest a gate stays closed, whatever the coordinator does (§2.3)
  cut_deadline: 5s              # a cut not committed this long after its announce is aborted (§2.3)
  lock_max: 8760h               # longest lock a snapshot may carry (§2.7)
  reserve_fraction: 1.0         # default reserve: history bytes per byte charged live (§2.9)
migration:
  freeze_timeout: 5m            # bound on a move's freeze, through B's ready (§4.2)
  hold_reply: 1s                # longest an NFS call waits in the freeze before ErrDelay; SMB is held pending
                                # up to freeze_timeout + 35 s, as for ErrGrace (§4.2, RFC 17 §3.2)
backups:
  copy_rate: 200MiB/s           # copying backups' transfer rate, per installation (§3.4.6)
  max_copy_time: 48h            # one copy attempt still running then is failed; its successor builds on it (§3.4.3)
  locations:                    # each a location record (§3)
    same-bucket:                # beside the namespaces: survives losing the metadata store only
      { type: s3, bucket: dfs-data, prefix: backups/, mode: mutable, credential: dfs-data-rw }
    vault:                      # another bucket, locked: may hold copying backups
      { type: s3, bucket: dfs-vault, prefix: backups/, mode: immutable,
        credential: dfs-vault-put-only, lifecycle_age: 2208h,       # ≥ longest retain + max_copy_time
        generation: 168h,                                           # extension generation G (§3.4.4)
        retention_cap: 2376h }      # ≥ longest retain + G + max_copy_time; enforced by bucket policy
rehome:
  rate: 100MiB/s                # a re-home's copy rate; 0 pauses it (§4.7)
shares:
  photos:
    namespace: photos           # its own, by default (§2.1)
    snapshots:
      reserve: 2TiB             # history bytes before new cuts are refused (§2.9); default reserve_fraction × live bytes
      policy:
        every: 1h
        keep: { hourly: 24, daily: 7, weekly: 4, monthly: 12 }
        lock: 168h
        backup: { location: vault, kind: copy, retain: 2160h, verify_every: 720h }
```

## 7. Invariants

| # | Invariant |
| --- | --- |
| S1 | A snapshot's blocks are kept alive only by the counts of the refs it sees, live and history: no manifest, hold list or extra root. |
| S2 | Snapshot *k* reads exactly the versions committed before cut *k* and not superseded before it, whatever journals and shards the share's files live in and whichever shard they move to. |
| S3 | A complete snapshot reads back exactly the share's bytes, attributes, ACLs, xattrs, streams and tree at the cut, whatever the share does after it. |
| S4 | A cut is atomic across shards: no transaction on any shard of the share commits after the cut stamped with the previous cut number; a transaction spanning shards is admitted at all their gates or at none, so a cut never waits on a transaction that waits on it. |
| S5 | `born` is the cut the committing transaction read, and a version's stamp is replicated and moves with it; `died` is the successor's `born`; no ref holds versions from both sides of a live cut's hold mark. |
| S6 | Every transaction that supersedes a version moves it to history, with no change to any count, exactly when a live cut *c* has `born < c ≤ died`, and otherwise drops it; a move to history conflicts with the deletion of the cut it relies on. |
| S7 | A snapshot hold marks the version each file's existence had committed up to at the cut, which the journal keeps until its successor's existence commits. It is durable on every replica before its cut commits and is inherited by takeover, handover and moves between shards, a move writing the receiving shard's hold record in its commit. A snapshot is `complete` only when no hold record remains, and `failed`, never `complete`, when a held version is lost. |
| S8 | Taking a snapshot writes a number of records independent of the share's size and holds the gate only for in-flight transactions; no write waits on an offload for a snapshot. A cut is refused over the hold bounds, counting the dirty bytes it would hold, or over the reserve. |
| S9 | Deleting a snapshot drops exactly the history no remaining live snapshot sees, and visits only history with `died` in its interval. A share's deletions run one at a time, and each leaves `klatest` at the greatest remaining live cut. |
| S10 | A clone and its source share chunks only; deleting either, or the snapshot, never changes what the others read. |
| S11 | A snapshot with a use record or an unexpired lock cannot be deleted; a compliance lock is never shortened, a governance lock only by the separate override right, and no lock is set past `lock_max`; an abandoned reader's use record is released at its deadline; an expired or failed backup is never restored. |
| S12 | One installation holds a namespace's claim. Only it puts, sweeps, relocates or collects there, only its store holds the namespace's records, and it writes only while the claim names it `owned` and its record says `serving`. A namespace being moved has no relocation, delete or collection from the pre-seed on, across restarts. |
| S13 | A move never releases refs at the old installation and never deletes a remote object on its behalf. |
| S14 | An import publishes all of its records or none; a staged ref is counted and indexed, is never served or found by dedup, and is dropped if the import fails. |
| S15 | An import proceeds only with every material ID held and the namespace's scope and prefix configured. |
| S16 | Every version assigned to an imported file exceeds every version imported for it: a journal raises its counter above a share's version floor before it serves a share it did not serve when it opened. Every file number issued after an import exceeds every number imported. |
| S17 | A move keeps FileIDs and refuses a collision; a clone, a restore and a detached snapshot get new ones. A move's drop `Forget`s the namespace's shares' tags in every journal at the old installation — every record of each tag below the forget record's sequence number, by sequence number, not by version — leaving no extent or removal marker of them, and attaching an imported share's tag finds none of its extents. |
| S18 | Counts after an import are recomputed from imported refs, never read from the export. |
| S19 | An import maps principals by opaque ID, never by protocol ID, refuses a collision of principal ID, protocol ID, principal name, share name, share path or netgroup, and carries no secret. |
| S20 | After a move, the base plus the delta equal the source's records at the freeze; the base is exported only once its cuts are `complete`, and the delta is every record whose change sequence is above the base's. |
| S21 | Quota charges live logical bytes only; history bytes are reported per share and per snapshot. |
| S22 | A subtree cut closes only its covered shards' gates and draws its number from the share's sequence. A version moves to history exactly when a live cut covering its shard sees it. A subtree snapshot shows only files of its covered shards, reached through covered directories, and no file moves into or out of a covered shard while the cut lives. |
| S23 | Every chunk a copying backup's export names lies in a folder block its manifest lists, and that block is byte for byte the namespace's block of that name. A copy reuses only its base's folder blocks, and never one whose census names material being retired. |
| S24 | At an immutable location DittoFS deletes nothing, every object a backup names is read by its recorded version, and each is locked until at least the backup's completion plus its retention ([§3.4.4](#3.4.4%20Expiry%20and%20the%20sweep)). Elsewhere, a folder block is deleted only by a sweep that runs alone at its folder, from the namespace's claim holder, and finds the block in no manifest of a `complete` or `damaged` backup and in no progress of a `writing` one. Any unreadable export stops the sweep. Material named by a folder's census is never removed. |
| S25 | A copying backup releases its snapshot only once `complete`. Retention never expires a share's newest complete copying backup at a location. A restore from one re-encodes into a new namespace, checks every chunk against its plaintext hash, and publishes all or nothing. |
| S26 | During a re-home every ref of the share names the namespace its chunk is counted in, by that namespace's chunk ID. A switch adopts in the new namespace and drops from the old in one transaction, keeping range, version, `born` and `died`, so no snapshot's or live read's bytes change. The re-home finishes only when no ref names the old namespace, decided from a folded counter that cannot rise once the re-home is closing. |
| S27 | A snapshot contains exactly the writes whose existence committed before its cut, never one whose existence committed after. Each primary closes its gate first, then takes its final cut points and commits the shard's pending existence up to them behind the closed gate, so every write acknowledged before the snapshot was requested is in it, within one shard no write is in it while one acknowledged before it is missing, and no namespace change that follows a write acknowledged after the cut points is in it. |
| S28 | No write waits at a cut gate longer than `gate_max`; a closed gate waits for admitted transactions up to that bound and refuses the cut only past it. Three consecutive skipped policy ticks raise a health condition. |
| S29 | An export is sealed and authenticated under its namespace's export key by RFC 5's export cryptography, names in its clear part the export-key and master-key IDs it needs, carries each key record as RFC 5's key table holds it, carries every bound setting, each share's fold rule and entry-digest key, and every netgroup its policy names with its members, and an import refuses any mismatch. |
| S30 | A recovery import leaves its namespace's GC paused until the operator states every share recovered or given up. |
| S31 | A folder record is a fenced lease: a holder acts only under its own lease number, and issues no put or delete once *L* / (1 + ρ) − σ has passed since the read that showed *L* left. Every wait on another process's or installation's timeout is *T* × (1 + ρ) + σ. |
| S32 | A claim carries the holder's instance nonce; a holder reads the claim before it rewrites it and records the new nonce before it puts it; a process that reads its own identity with a nonce it did not record stops writing and deleting at once, and an installation started on a copy holds no namespace until an operator states which copy it is. |
| S33 | A detached snapshot's share is retired: it serves nothing, accepts no write, and keeps only what its snapshots see. |
| S34 | A policy tick refused by the snapshot reserve counts as a period holding a snapshot and prunes the oldest unlocked policy snapshot until the cut fits, so history ages out and cuts resume; a locked or manual snapshot is never pruned for it. |
| S35 | An ordinal is unique among the snapshots visible at any one path and is not reassigned within 24 h of its snapshot's deletion; a cut time is unique among the snapshots visible at any one path, and a subtree cut's runs ahead of the store's time only by the cuts on its own path. |
| S36 | No backup survives losing the master key that wraps its namespace's keys; every export names the master keys it needs, and a master key any retained export names is never destroyed. |
| S37 | A failed copy is the base of its successor; `max_copy_time` bounds one attempt; a policy is held to its incremental copy, not its first. At an immutable location every put carries its own recorded retain-until within the location's cap, extensions come once per generation, backups are found by listing versions, every state and progress version is authenticated and the one of highest authenticated sequence is read, the location's one health object is written at location creation, and it and the last good backup never age out. |
| S38 | An offer uses the namespace and keys it captured when carved; no put lands N-keyed chunk IDs in another namespace. A copy taken mid-re-home carries both namespaces, puts every block into the new namespace's folder, and is honoured by that folder's sweep; a ref the re-home drops from N stays counted there while a catalog backup taken before the re-home finished is unexpired. |
| S39 | After a move's drop, no journal of the old installation holds an extent or removal marker of the moved shares, and a journal attaching an imported share holds none of its tag's extents before it serves. B's work after A's release is one publish and the claim. |
| S40 | The cut takes each shard's cut points after its gate closes and commits the shard's pending existence up to them behind that gate — a primary's own pre-cut and recovery commits pass its own closed gate — and no existence commit of any kind (group commit, removal's first phase, offer capture, explicit time set) lands above them until the gate reopens. |

## 8. Test plan and benchmarks

The set-wide rules and tiers are in [the RFC index](rfc-index.md#Test%20tiers).
Every check runs against a real metadata backend and the remote-tier emulator; GC
runs as in production, not stubbed.

### 8.1 How it is tested

**A model.** The cut ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)), the hold's lifecycle ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)), the
deletion ([§2.8](#2.8%20Deleting)), the move's claim, pause and freeze ([§4.1](#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim), [§4.2](#4.2%20The%20move%2C%20step%20by%20step)), subtree
coverage ([§2.10](#2.10%20Subtree%20snapshots)), a folder's copies and sweeps ([§3.4.3](#3.4.3%20Writing%20one%2C%20step%20by%20step),
[§3.4.4](#3.4.4%20Expiry%20and%20the%20sweep)) and the re-home's switch and finish ([§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) **MUST** be modelled
in a model checker before they are implemented, with S4, S5, S7, S9, S12, S13,
S20, S22, S24 and S26 as properties, and kept in step with this document.

**Deterministic simulation.** The snapshot service, the gate, the journal's holds
and the move reach the network, the journals, the metadata store, the remote
store, time and randomness only through interfaces. A simulator drives a cluster
of installations, nodes and one bucket in one process from a seed; a failing seed
**MUST** reproduce the failure exactly. It runs the scenario catalogue
([§8.2](#8.2%20Scenario%20catalogue)), scripted and replayed exactly, and a randomized seed search over
every fault at once: node crashes discarding unsynced writes, lease lapses and
takeovers mid-cut, coordinator crashes, metadata-store stalls and unknown commit
outcomes, remote-store outages, clock drift within and beyond the bound, and
process pauses between a claim check and a delete.

**Model-based checking.** Random writes, overwrites, truncates, deallocates,
renames, links, unlinks, releases, `chmod`s, ACL and xattr changes, stream writes,
directory-delta folds, clones and snapshot creation and deletion race each other.
After every transaction, each chunk's count equals its live plus history refs, and
every live snapshot reads back a model copy taken at its cut.

**Coverage.** Every run reports which branches it reached, and seed search **MUST**
reach each of these or fail: a cut aborted on a changed epoch and retried; a
primary started with its gate closed on a `cutting` record; a coordinator's abort
committed by a primary at the deadline; a held superseded version offloaded
straight to history; a held version dropped because its snapshot was deleted
first; a ref split at a hold mark; a snapshot marked `failed`; two deletions of one
share requested at once, the second waiting; a deletion refused by a use record and by a lock; a cut
refused by each bound; a stale GC process stopped by its claim fence; a
cross-shard transaction releasing its admissions at a closed gate; a history move
retried on a deletion's `LiveCut`; a stamp rebuilt after takeover; a hold record
written by a move between shards; an abandoned use record released; a relocation
aborted by the GC pause record; an uncovered shard's transaction stamped below a
subtree cut it committed after; a move of files refused on a covered shard; a
copy reusing a base block, and one refusing a block with retiring material; a
copy resumed from its progress; a sweep stopped by an unreadable export; a
relocated block re-resolved mid-copy; a re-home batch retried on a changed ref;
a catch-up pass switching a ref written behind the cursor; a pre-cut existence
commit carrying an unflushed write into a snapshot; a reserve-refused tick that
pruned; a copy built on a failed predecessor's progress; a parked ref dropped
when the last pre-re-home catalog backup expired; a claim read that stopped a
stale image before its put.

### 8.2 Scenario catalogue

| ID | Scenario |
| --- | --- |
| `S-snap-hold-overwrite` | [§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)'s example: dirty content overwritten after the cut is held and offloaded to history |
| `S-snap-cut-failover` | [§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)'s example: a cut across two shards while one fails over |
| `S-snap-delete-middle` | [§2.8](#2.8%20Deleting)'s example: deleting the middle of three snapshots visits only its interval |
| `S-snap-clone-delete-source` | [§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)'s example: clone, then delete the source and its snapshot |
| `S-snap-catalog-restore` | [§3.3](#3.3%20Restore)'s example: a catalog backup, loss of the metadata store, recovery import |
| `S-snap-locked-delete` | [§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)'s example: a locked snapshot refuses deletion, shortening and pruning |
| `S-snap-move-preseed` | [§4.2](#4.2%20The%20move%2C%20step%20by%20step)'s example: a pre-seeded move with a short freeze |
| `S-snap-coordinator-crash-<step>` | the coordinator crashes after each of [§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)'s steps; gates reopen by the deadline, and no hold outlives an aborted cut |
| `S-snap-shard-move-mid-cut` | files move between shards, and a new per-child shard is created, between announce and cut |
| `S-snap-hold-takeover` | the primary dies after the cut with held superseded versions; the new primary offloads them and the snapshot completes |
| `S-snap-hold-lost` | every copy of a held version is lost; the snapshot goes `failed` |
| `S-snap-carve-straddle` | one carve run spans a live hold mark; it is split and the snapshot reads its bytes |
| `S-snap-delete-concurrent` | a deletion is stopped between batches, cuts are taken meanwhile, and it resumes correctly |
| `S-snap-delete-serialised` | [§2.8](#2.8%20Deleting)'s example: two deletions of one share requested at once run one after the other, and `klatest` ends at the greatest remaining live cut |
| `S-snap-delete-vs-history` | a primary takes over mid-deletion with its gate open; a transaction that read `LiveCut(k)` before the deletion's first transaction moves a version to history against *k*; one of the two conflicts and retries, and no orphan history remains |
| `S-snap-hold-uncommitted-successor` | [§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)'s example: a successor acknowledged above the cut point, whose existence commits after the cut; the snapshot reads the committed version |
| `S-snap-hold-shard-move` | files with held superseded versions move between shards, and are re-versioned on arrival, before their offload; the receiving shard's hold record is written in the move's commit, and the snapshot completes with the held bytes |
| `S-snap-stamp-takeover` | the primary fails after an existence commit, before its stamp replicates; the new primary rebuilds that stamp from metadata, and every other stamp arrived by replication |
| `S-snap-cross-shard-gate` | a cut closes the gates of shards S1 and S2 while cross-shard renames run, some prepared; each rename is admitted at both gates or at neither, and the cut commits within its deadline |
| `S-snap-hold-bound-dirty` | a journal at 24% held carries 30% dirty; the cut is refused `ErrHoldBacklog` by the journal bound a replica reported |
| `S-snap-offload-after-drop` | a held version's offload commits after its snapshot was deleted; it is dropped, and the audit is clean |
| `S-snap-clone-vs-delete` | a clone or backup starts while its snapshot's deletion runs; exactly one wins |
| `S-snap-remote-down` | the remote tier is down under a sustained writer; cuts are refused at the bounds and writes proceed |
| `S-snap-journal-bound` | many shares' held bytes share one journal; the per-journal bound refuses cuts before the journal fills |
| `S-snap-move-crash-<step>` | A or B crashes after each of [§4.2](#4.2%20The%20move%2C%20step%20by%20step)'s steps; a restart during `moving` stays closed |
| `S-snap-move-stale-gc` | A is partitioned, not dead, during a recovery import; its GC stops on its claim fence |
| `S-snap-smb-token` | two cuts requested within one second, and a cut at B after a move from an A whose clock ran ahead, get distinct Previous Versions tokens |
| `S-snap-backup-abandoned` | a backup crashes while `writing`; at its use record's deadline it is marked `failed` and the snapshot becomes deletable; the failed backup is refused by restore |
| `S-snap-move-delta-seq` | during the pre-seed an offload lands late with `born` below the base, a truncate narrows a ref keeping its `born`, and a count reaches zero and retires a block; the delta carries all three by change sequence and B reads back A's bytes |
| `S-snap-move-gc-pause` | a relocation is in flight when the pause record is written; it aborts, A restarts during the pre-seed, and no relocation, delete or collection runs until B publishes |
| `S-snap-move-refuses` | a clone, a backup, a share creation and a share deletion requested during a move are refused `ErrMoving`; one already running finishes first |
| `S-snap-subtree-rename` | [§2.10](#2.10%20Subtree%20snapshots)'s example: a subtree snapshot closes one shard's gate; a rename out of the subtree and a write to the renamed file keep its history; an uncovered shard writes none; deletion drops it all |
| `S-snap-subtree-coexist` | share cuts and subtree cuts of two shards interleave; deleting each in turn keeps exactly the history the remaining ones see, per shard, and `klatest` and every `SubCut` end at the greatest live covering cut |
| `S-snap-subtree-stale-stamp` | a transaction on an uncovered shard reads the cut number before a subtree cut and commits after it; a later share cut reads it correctly |
| `S-snap-subtree-nested` | a subtree cut of a shard with per-child shards beneath it covers them; an entry naming a file of an uncovered shard is left out of the browse |
| `S-snap-subtree-move-refused` | marking a directory for per-child shards inside a covered shard, and a move into it, are refused `ErrSubtreeSnapshot`; a handover of its primary runs; after the deletion the move proceeds |
| `S-snap-copy-incremental` | [§3.4](#3.4%20Copying%20backups)'s first example: incremental copies across a relocation, expiry, sweep and verify |
| `S-snap-copy-restore` | [§3.4](#3.4%20Copying%20backups)'s second example: the bucket is lost; a restore re-encodes into a new namespace on another installation, and the failing policy keeps the last good backup |
| `S-snap-copy-crash-<step>` | the copier or the sweep crashes after each step; a restart before the deadline resumes, one after it marks `failed`, and the next sweep leaves exactly the retained manifests' blocks |
| `S-snap-copy-relocated` | compaction relocates and deletes a block while a copy is about to read it; the copy re-resolves and copies the new block |
| `S-snap-copy-retire-material` | material is being retired while backups list folder blocks sealed with it; copies stop reusing them, removal waits for the folder census, and it proceeds after they expire |
| `S-snap-copy-unreadable-export` | one export at the location is corrupt; the sweep deletes nothing |
| `S-snap-copy-damaged` | a folder block is corrupted; `Verify` marks the backup `damaged`; the next copy takes no base from it; restore refuses it |
| `S-snap-rehome` | [§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)'s example: a re-home under writes, a cross-share copy caught by a second pass, and snapshot reads before and after |
| `S-snap-rehome-crash-<step>` | the re-home crashes after each step and between a batch's puts and its switch; it resumes at its cursor, and N''s GC collects the orphaned puts |
| `S-snap-rehome-straddle` | an offload carved under N mints names and puts after the write namespace changed: it puts into N with N-keyed IDs, the primary's join makes it commit before the acknowledgement, and every read of it verifies; hand the intent step a plan keyed under N for a put into N': refused `ErrScopeMismatch` |
| `S-snap-rehome-sole` | the only share of its namespace is re-homed into a namespace with a different chunking target and encryption on; it serves throughout, every snapshot reads back, its refs are re-cut, and the old namespace ends empty |
| `S-snap-rehome-backup` | copying backups run every day of a long re-home; each lists blocks of N′'s folder only, sealed in either namespace, and carries both namespaces' IDs and key records, a restore from one taken mid-way reads back, and N′'s sweep keeps every block it lists while N's sweep deletes none of N′'s; new catalog backups are refused and counted. A share with a 90-day catalog backup is re-homed at once: its refs are parked in N, a recovery import of that backup reads back after the re-home finished, and N empties only once the backup expires |
| `S-snap-rehome-finish-load` | the finish runs while 64 writers write the share: it commits on the folded counter alone, without a range read; a server-side copy by reference from N after `closing` copies bytes |
| `S-snap-retention-dst` | a year of hourly ticks in a zone with daylight saving, with a two-day outage: kept snapshots match the period rule exactly, and the outage prunes nothing |
| `S-snap-subtree-scale` | 10⁴ per-child shards, each with an hourly subtree snapshot kept 24: each subtree deletion reads only its own shard's history, the deletion queue drains, and no cut is refused `ErrSnapshotLimit`; no two snapshots visible at one path share an ordinal, and no ordinal is reassigned within 24 h of its snapshot's deletion; every cut time stays within a few seconds of the store's time, and Previous Versions at each path lists distinct tokens. Draw cut times from one share-wide sequence: they run hours ahead within a day |
| `S-snap-import-version-floor` | a share imported into an installation whose journals are already open is written at once; the journal raises its counter above the share's floor first |
| `S-snap-copy-immutable` | copies to an immutable location under a stolen put credential, a long copy, and a chain of failing copies; restores read recorded versions |
| `S-snap-gate-bounds` | a slow admitted transaction and a stopped coordinator meet the gate's drain and ceiling bounds |
| `S-snap-recovery-shared` | a recovery import of one share of a namespace whose clone has no backup; GC stays paused until the operator's statement |
| `S-snap-folder-lease` | a sweep paused past its folder lease resumes after a copy took the folder |
| `S-snap-cloned-vm` | two copies of one installation start from one disk image |
| `S-snap-detach` | a share is deleted with its snapshots detached, and they are restored and deleted in turn |
| `S-snap-move-freeze-reply` | calls during a move's freeze are answered retry-later after `hold_reply` over NFS, kept pending over SMB within `freeze_timeout` plus 35 s, never answered `STATUS_DISK_FULL`, the SMB connection dropped at the move so durable handles reconnect at B, and all complete at B |

### 8.3 Group A — lost or wrong content

| Invariant | Check |
| --- | --- |
| S1 | Snapshot a share, delete every file, run sweep and collection to completion. Every block the snapshot names survives, and every snapshot file reads back by hash. |
| S2 | Place one share's files in two journals and two shards, and advance one journal's versions far past the other's. Overwrite files in both between snapshots. Every snapshot reads back its model copy. Replace the cut-number comparison with a comparison of journal versions: the check fails. |
| S3 | Model-based run above; then kill the process at every step between the cut and `complete`, and inside every transaction that moves a record to history; restart. Each snapshot completes and equals the model. |
| S3 | Give a file mode 0644 and an ACL denying principal X; snapshot; replace the ACL, then release the file. Browsing the snapshot as X is refused both times. Leave the ACL unversioned: the check fails. |
| S4 | `S-snap-cut-failover` and `S-snap-shard-move-mid-cut`. Remove the epoch check from the cut transaction: a post-cut `chmod` appears in the snapshot. `S-snap-cross-shard-gate`: admit per gate instead of all-or-nothing, and cuts abort at their deadline. |
| S5 | Acknowledge a write after the cut points, before the gate reopens: its existence commits after the cut and the snapshot does not show it. `S-snap-carve-straddle`: remove the split, and the snapshot reads post-cut bytes. `S-snap-stamp-takeover`: leave stamps unreplicated, and more than one version per file needs a rebuilt stamp. Two overwrites of one range between snapshots, then a truncate over it (a model-test seed): the superseded piece dies at the lower overwrite's `born`, no history key collides, and the truncate commits. |
| S6 | `S-snap-hold-overwrite` with v3 committed first: the check fails on two versions visible at cut 1. `S-snap-offload-after-drop`: remove the live-cut test, and the audit reports an orphan history ref. `S-snap-delete-vs-history`: read `LiveCut` untracked, and the audit reports an orphan history ref. |
| S7 | `S-snap-hold-takeover` and `S-snap-hold-lost`. Keep holds unreplicated: the first reads the wrong bytes. `S-snap-hold-uncommitted-successor`: let the uncommitted successor supersede, and the snapshot reads r1. `S-snap-hold-shard-move`: leave the receiving hold record out of the move's commit, and the snapshot completes reading the wrong bytes. |
| S8 | `S-snap-remote-down` and `S-snap-journal-bound`: each cut commits within the gate target, writes never stall, snapshots stay `holding`, refused cuts return `ErrHoldBacklog`; restore the tier and every snapshot completes. `S-snap-hold-bound-dirty`: count held bytes only, and the journal fills. |
| S9 | `S-snap-delete-middle`, `S-snap-delete-concurrent` and `S-snap-delete-serialised`, killing the process between batches. After each, exactly the history no remaining snapshot sees is gone, `klatest` is the greatest live cut, and the audit is clean. Let two deletions run at once: `S-snap-delete-serialised` ends with `klatest` naming a deleted cut. |
| S10 | `S-snap-clone-delete-source`; then write to both and read each back. |
| S11 | `S-snap-locked-delete`, `S-snap-clone-vs-delete`, `S-snap-backup-abandoned`. Crash expiry and abandonment after each step: a restore never succeeds from an expired or failed backup, and never reads a swept block. |
| S12 | Two installations on one bucket, each holding one namespace, both running GC, relocation and collection for a day-tier run: neither deletes an object the other's records name. Configure both to hold one namespace: GC stops on the claim check. `S-snap-move-stale-gc`. |
| S13 | Move a namespace while A's GC has retired blocks in the trash, deleted blocks awaiting their delete, and intents in flight. After the drop, A issues no delete; B resumes them, and deletes no retired block before its `not_before`. |
| S14 | Corrupt one byte in each frame position, truncate the stream at every frame boundary, kill the importer at each step: nothing publishes, staging is empty after restart. During a clone's staging, run two audit walks and the deleter: no chunk a staged ref names is retired. |
| S15 | Import with one material ID removed from B's provider, and with a different scope: refused before any record is staged. |
| S16 | `S-snap-import-version-floor`: import a share whose files carry versions far above the counter of a journal already open at B, and write to one file at once, then from a per-child shard created afterwards. Each write's version exceeds the imported ones, commits and reads back. Skip the raise: the first write is dropped at its commit and the read returns the imported bytes. Restore a share whose highest `Number` is 10⁶ and create a file: its number is above 10⁶; skip the allocator raise and two files report one id. |
| S17 | Move a namespace to B, back to A, and to B again: FileIDs preserved. Restore one backup twice: two shares, disjoint FileIDs. Move A→B, overwrite a file at B, move back: A reads B's bytes, and A's journals held no extent of the share between the drop and the return. Skip step 9's `Forget`: A serves its stale extent. Give an old record of the tag a version above the forget's: it is still dropped, since `Forget` covers by sequence number. Replace it with a per-file `Delete`: after A→B→A a restart's `Since` yields the markers and an untouched file loses its content. Crash A between the `Forget` calls and the record drop, and plant a stale extent of the tag: the attach `Forget`s it or refuses, and never serves it. |
| S18 | Plant wrong counts in an export: the import's counts are correct and the audit is clean. |
| S19 | Import an export whose principal ID B holds for another user, and one whose UID B maps to another principal: both refused, naming the principal. Import a share named `Photos` into a B holding `photos`, and one whose path lies inside another share's: both refused, naming the collision; with a new name given in the request, accepted. Import one whose principals B lacks: adopted by ID, with no secret. |
| S20 | `S-snap-move-preseed` under the model-based run on A during the pre-seed: after the move B's records equal A's at the freeze, and the delta holds no record unchanged since the base. `S-snap-move-delta-seq`: select the delta by `born` and `died`, and B misses the narrowed ref. `S-snap-move-gc-pause`: pause in memory only, and B maps a relocated chunk to its deleted block. |
| S21 | Rewrite a 10 GiB file hourly under a quota with hourly snapshots: charged usage stays at 10 GiB, `history_bytes` grows, and bytes freed if deleted for the oldest snapshot matches what its deletion frees. |
| S22 | `S-snap-subtree-rename`, `S-snap-subtree-coexist` and `S-snap-subtree-nested` under the model-based run: each live snapshot of either kind reads back its model copy. `S-snap-subtree-stale-stamp`. Test coverage by klatest alone, ignoring `SubCut`: the subtree snapshot reads post-cut bytes. Allow the move of `S-snap-subtree-move-refused`: a later write drops history the subtree snapshot reads. |
| S23 | `S-snap-copy-incremental` and `S-snap-copy-relocated`: every folder block's name recomputes from its header, and every chunk decodes to its hash. `S-snap-copy-retire-material`: reuse a block with retiring material, and removal finds it still named. |
| S24 | `S-snap-copy-crash-<step>` and `S-snap-copy-unreadable-export`: after each, the folder holds exactly the union of the retained manifests plus writing progress, and every retained backup restores. Let a copy and a sweep run at once: a sweep deletes a block the copy reused. |
| S25 | `S-snap-copy-restore`, `S-snap-copy-damaged`. Delete the snapshot right after a copy completes, run GC to completion, and restore from the copy: every file reads back by hash. |
| S26 | `S-snap-rehome` and `S-snap-rehome-crash-<step>` under the model-based run: every snapshot and the live share read back their model copies at every step, and after the finish no ref names N and the audit of both namespaces is clean. Skip the catch-up passes: finish refuses, since `old_refs` is not zero. |
| [§3.3](#3.3%20Restore) | `S-snap-catalog-restore` with every block the backup names relocated first: every stale hint is resolved from block headers and every file reads back; the older backup survives GC. |
| S24 | `S-snap-copy-immutable`: copies to an immutable location — every get by recorded version; a put under an existing name by a stolen credential, then a restore: the restore reads the recorded versions and every file reads back. A copy that took 40 h: every version its manifest names is locked to its completion plus retention, including blocks put in its first hour. Copies keep failing for twice the retention: the last good backup's versions are still locked and it restores. A policy whose period is below its estimated incremental copy is refused `ErrPolicyPeriod`; one below its full copy but above its increment is accepted. A first copy of 40 TiB at 200 MiB/s with `max_copy_time` 48 h: it fails once, its successor builds on its progress, and the share has a complete backup within three attempts; restart each attempt from zero and none completes. Run past the lifecycle age with the health object and the last good backup untouched: the location still opens, and a restore lists versions and finds the backup. Put a newer `expired` state version with a stolen credential: the restore ignores it, since it does not authenticate. Re-put the backup's own authentic `writing` version: the restore reads `complete`, the higher sequence. Copy the first namespace ever to a fresh immutable location: it opens, finding the location's health object by its recorded version. Keep a process up past the lifecycle age plus two generations: the location still opens, and the health object's lock never exceeds the cap. A failed first copy is settled before its successor runs: its progress objects stay and the successor builds on them. Answer one extension in a burst with a throttling reply: the backup completes. |
| S27 | Write with `UNSTABLE` and acknowledge, write a stable overwrite with no `COMMIT`, and take a snapshot at once, before any group commit is due: the snapshot has both writes. Write w1 then w2 to two files of one shard with the group commit stalled, and cut between them while the stall lasts: the snapshot holds w1 whenever it holds w2. Remove the pre-cut commit: the snapshot lacks both writes of the first case. Stall the store past `gate_max`: the cut is aborted, the gate reopens and writes keep being acknowledged. Write and flush `~tmp1` after the cut points and rename it over `report.docx` while the gate is closed: the snapshot shows the old `report.docx`, never a zero-length one. `S-snap-cut-failover`: a primary that starts closed after a takeover commits its recovery and pre-cut existence behind its own gate and replies; the snapshot holds every write acknowledged before the request. |
| S28 | `S-snap-gate-bounds`: under sustained offload, with a group commit of 256 files and an offload commit filling the key budget K admitted at every close and taking 200 ms each, every hourly cut for a day commits. Cap the drain at 50 ms: every cut is refused. A prepared cross-shard transaction holds one shard past `gate_max`: the gate reopens by `gate_max`, the cut is refused and retried, and no write waited longer. Stop the coordinator after every gate closed: every gate reopens by `gate_max`. Refuse three ticks in a row: the health condition names the share. |
| S29 | Flip one byte of an export's clear part, header, a frame and the trailer, and move one frame to another position: each is refused `ErrExportCorrupt`. Read an export without the master key: no file name, chunk ID or principal appears in its bytes. Import with a chunking target, an encrypt flag or a key ID that differs from B's: refused `ErrScopeMismatch` naming it. Import a policy naming netgroup `ops` into a B whose `ops` has other members: refused `ErrNetgroupCollision`; into a B without `ops`: adopted with its members, and a client of the deny list is still denied. || S29 | Restore a catalog backup of a case-insensitive share whose listings clients were paging: every name is found by lookup, and a listing resumed from a cookie taken before the backup continues where it stopped. Draw a fresh digest key or fold rule at import: lookups miss and cookies resume elsewhere. Check the export's frame nonces: no two frames of one export repeat one. |
| S30 | `S-snap-recovery-shared`: a namespace holds a share with a catalog backup and its clone with none, sharing blocks. Recover the share alone: GC deletes, relocates and collects nothing in the namespace, and every block the clone needs survives; after the operator's statement GC runs and the audit is clean. Start GC at publish instead: the clone's blocks are deleted. |
| S31 | `S-snap-folder-lease`: a sweep pauses past its deadline after its mark; a copy takes the folder and reuses a block the mark left unlisted; the sweep resumes: it deletes nothing, by its lease number and its own clock. Run the sweep's clock 5% slow: it stops by *L* / 1.05 − σ; fence by the deadline alone, without the clock bound, and the sweep deletes the reused block. A recovery import whose clock runs 5% fast against a partitioned old holder's waits two `Recheck` periods × 1.05 + σ by the slow clock; wait two periods plus a fixed margin and both delete for a while. |
| S32 | `S-snap-cloned-vm`: start two copies of one installation's disk, on a platform that reports a clone and on one that does not. With the report, the copy holds no namespace. Without it, within one `Recheck` period exactly one copy writes and deletes, and the other has alerted. Restore a day-old image of a running installation, on a platform that reports nothing: it stops at its first start, before any put, and the original keeps writing; rewrite the claim before reading it, and the original is the one fenced. Crash the holder between its put and recording the nonce: at restart it recognises its own claim. |
| S33 | `S-snap-detach`: delete a share detaching its two snapshots; the share serves nothing and refuses writes; each snapshot restores; content no snapshot saw is released; deleting the second snapshot removes the share's records. |
| S36 | Back up an encrypting namespace, rotate its master key, and destroy the old one: refused `ErrMaterialInUse`, naming the backup; expire the backup and the destroy succeeds. Restore the backup on an installation that holds no master key: refused `ErrMaterialMissing`. Back up a non-encrypting namespace, lose the host's disk, and restore from the bucket and the backup with the escrowed master key: every file reads back; without it, the restore is refused `ErrMaterialMissing` though the chunk-ID key is in the bucket. |
| S30 | Delete the embedded metadata store with the node stopped and start it: the node serves nothing and GC deletes nothing, naming the store; a recovery import from the newest catalog backup serves the files as of that backup, and the blocks written since stay until the operator's statement. |
| S11 | Set a lock past `lock_max`: refused `ErrLockTooLong`. Shorten a governance lock with the override right: allowed, logged and counted; without it, and on a compliance lock: `ErrLocked`. |

### 8.4 Group B — cost and wedging

| Concern | Counted check |
| --- | --- |
| [§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate) cost to take | Taking a snapshot of a 10²- and a 10⁷-file share writes the same number of records. |
| [§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree) cost to change | After a cut, the first change to a record writes one history version and a second writes none; with no live snapshot no change writes history. |
| [§2.8](#2.8%20Deleting) deletion | Deleting a snapshot reads only history in its interval, whatever the share's total history, and writes no transaction larger than the batching bound. |
| [§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks) retention | A simulated year of hourly ticks under the example policy: never more than 47 policy snapshots, manual snapshots untouched, locked ones kept to expiry. |
| [§4.2](#4.2%20The%20move%2C%20step%20by%20step) freeze | The delta exported holds exactly the records changed since the base; the freeze time is reported against the namespace's key count (the scan's ceiling). B's work after A's release is one publish transaction and the claim put, whatever the namespace's size; kill B mid-rebuild, or delay it past `freeze_timeout`: the move aborts and A serves. Rebuild indexes after the release instead: the freeze ends while B still rebuilds for minutes, and an abort is no longer possible. |
| [§5.1](#5.1%20Layout) streaming | Import of a 10⁶-file export holds at most one frame plus bounded batches in memory. |
| [§2.10](#2.10%20Subtree%20snapshots) cut | A subtree cut of one per-child shard of a 10⁴-shard share closes one gate, and reads one shard record. |
| [§3.4](#3.4%20Copying%20backups) increment | A second copy of an unchanged share puts no block. After one changed file, it puts only the blocks holding its new chunks. A sweep lists the folder once. |
| [§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace) re-home | Each chunk is read from N and put into N' once. A re-home with no concurrent writes finishes in one pass. |
| [§2.10](#2.10%20Subtree%20snapshots) subtree deletion | `S-snap-subtree-scale`: keys read by a subtree deletion are proportional to its own shard's history in its interval, whatever the number of shards. |
| [§2.9](#2.9%20Space%20is%20reported%2C%20not%20charged) reserve | A share with no reserve set and hourly snapshots of a file rewritten hourly: cuts are refused `ErrSnapshotReserve` once history reaches its live bytes. Keep running for 48 h under `keep: { hourly: 24 }`: each refused tick still prunes the oldest policy snapshot, history falls, and cuts are taken again within a day. Skip refused periods as empty: no cut is taken after the first refusal. Under `keep: { hourly: 24, daily: 7, weekly: 4, monthly: 12 }` with 5% daily churn: refused ticks prune the oldest unlocked policy snapshot, weekly and monthly ones included, and a cut is taken within a day; a locked snapshot survives. |

### 8.5 Benchmarks and targets

Recorded on the reference box ([the RFC index](rfc-index.md#Test%20tiers)); the
10⁷-file rows run daily.

| Benchmark | Measures | Target |
| --- | --- | --- |
| Cut under a sustained writer, 64 clients, one shard and 64 shards | gate held; p99 write latency across the cut | gate ≤ 50 ms; p99 within 2× of no cut |
| Snapshot of a 10⁷-file share | wall time, records written | same as a 10²-file share |
| Cut to `complete`, 10 GiB dirty at the cut | seconds | at most the dirty bytes over the measured offload rate, plus 1 s |
| Live write throughput with hourly snapshots, 24 kept | ops/s against none | within 10% |
| Snapshot read of a file overwritten 10³ times since | p99 against a live read | report, against the ponytail of [§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree) |
| Deleting one hourly snapshot of a share with 2.6×10⁵ history refs | keys read | proportional to the history in its interval |
| Export and import of a 10⁶-file namespace | records/s | at least half the backend's own scan and batch-write rates |
| Clone of a 10⁶-file snapshot | wall time | report, against the ponytail of [§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace) |
| Restore throughput: copy every file of a 10⁶-file, 1 TiB snapshot out through the browse path, 64 readers, cold | MB/s and files/s against a live cold read | at least half the live cold read |
| Recovery import, all hints stale | header gets per block | one ranged get per block in the namespace |
| Move of a 10⁷-file namespace with 1% changed during the pre-seed | freeze to B serving | under `freeze_timeout`; no block transferred |
| Subtree cut of one of 64 shards under a sustained writer on every shard | p99 write latency on the other 63 across the cut | unchanged against no cut |
| Daily copying backup of a 1 TiB share, 1% changed | bytes put; wall time | at most the blocks holding the changed chunks; at `backups.copy_rate` |
| Restore of a 1 TiB copying backup into a new namespace | MB/s | report, against the codec's decode plus encode throughput |
| Re-home of a 10⁶-file share under 64 writers | MB/s against `rehome.rate`; p99 write latency | within 10% of the rate; p99 within 2× of no re-home |

## 9. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| snapshot outcomes, labelled `result` = `complete`, `hold_backlog`, `reserve`, `aborted`, `failed`, and `kind` = `share`, `subtree` | `dittofs_snapshot_total` | counter |
| cut gate held, per shard | `dittofs_snapshot_gate_seconds` | histogram |
| held bytes not yet offloaded, per share and per journal, and time from cut to `complete` | `dittofs_snapshot_held_bytes`, `dittofs_snapshot_complete_seconds` | gauge, histogram |
| history bytes per share; history refs and records; those dropped by deletion | `dittofs_snapshot_history_bytes`, `dittofs_snapshot_history_refs`, `dittofs_snapshot_history_records`, `dittofs_snapshot_history_dropped_total` | gauge, gauge, gauge, counter |
| policy ticks, labelled `result` = `taken`, `skipped`, `pruned`, `locked` | `dittofs_snapshot_policy_total` | counter |
| deletions refused or deferred, labelled `reason` = `locked`, `held`, `busy` (another deletion of the share running), `moving` | `dittofs_snapshot_delete_refused_total` | counter |
| use records released at their deadline, labelled `reader` = `backup`, `clone`, `restore` | `dittofs_snapshot_use_abandoned_total` | counter |
| cross-shard transactions that released their admissions at a closed gate | `dittofs_snapshot_gate_readmissions_total` | counter |
| history moves retried on a conflict with a deletion's `LiveCut` | `dittofs_snapshot_history_conflicts_total` | counter |
| age of the newest complete backup per share, labelled `kind`; the alert for a policy that stopped | `dittofs_backup_newest_age_seconds` | gauge |
| moves of files refused because a subtree snapshot covers the shard | `dittofs_snapshot_subtree_move_refused_total` | counter |
| copying backups' blocks, labelled `result` = `copied`, `reused`, `reresolved`; bytes put | `dittofs_backup_copy_blocks_total`, `dittofs_backup_copy_bytes_total` | counter, counter |
| blocks and bytes held per block folder; blocks swept; sweeps stopped by an unreadable export, an alert | `dittofs_backup_folder_blocks`, `dittofs_backup_folder_bytes`, `dittofs_backup_swept_total`, `dittofs_backup_sweep_refused_total` | gauge, gauge, counter, counter |
| verifications, labelled `result` = `ok`, `damaged` | `dittofs_backup_verify_total` | counter |
| re-home progress per share: refs left at the old generation, bytes copied, passes | `dittofs_rehome_old_refs`, `dittofs_rehome_bytes_total`, `dittofs_rehome_passes_total` | gauge, counter, counter |
| export and import records and bytes, labelled `kind` | `dittofs_export_records_total`, `dittofs_import_records_total` | counter |
| import refusals, labelled `reason` = `corrupt`, `material`, `scope`, `fileid`, `claim`, `hint`, `principal` | `dittofs_import_refused_total` | counter |
| move phase per namespace (`preseed`, `freeze`, `released`, none), freeze duration, and whether the GC pause record is present | `dittofs_move_phase`, `dittofs_move_freeze_seconds`, `dittofs_move_gc_paused` | gauge, histogram, gauge |
| namespace claim state (1 when `owned` by this installation) | `dittofs_namespace_owned` | gauge |
| cuts refused by the gate's ceiling; consecutive skipped ticks per policy, a health condition at 3 | `dittofs_snapshot_gate_refused_total`, `dittofs_snapshot_policy_skipped_consecutive` | counter, gauge |
| governance-lock overrides, labelled by principal; any value is an audit event | `dittofs_snapshot_lock_overrides_total` | counter |
| claim reads naming this installation with a nonce it did not write, and starts on a copied disk; any value is an alert | `dittofs_namespace_claim_copy_total` | counter |
| folder-lease actions refused by a lease number or a holder's own clock | `dittofs_backup_lease_fenced_total` | counter |
| immutable versions extended at copy completion or for the last good backup | `dittofs_backup_retention_extended_total` | counter |
| GC passes stopped by the claim check or its fence; any nonzero value is an alert | `dittofs_gc_claim_refusals_total` | counter |

Logs: each cut, completion, abort, move step and publish logs at `Info` with share,
namespace and snapshot or export digest. A refused cut, a failed snapshot, a move's
freeze timeout, an import refusal and a claim mismatch log at `Warn`, naming the
bound, the missing material, the mismatching scope or the claiming installation.
Each copy, sweep, verify and re-home pass logs at `Info` with its counts; a copy
marked `failed`, a backup marked `damaged`, a stopped sweep and a refused move of
files log at `Warn`.

## 10. Open questions

1. **Reading another installation's snapshot without a move** would count blocks
   the holder can sweep; it needs replication across installations or a byte copy.
2. **Handles across a recovery import.** It assigns new FileIDs, so clients remount;
   keeping them would need proof that no other restore of the namespace used them.
3. **Immutability at the filesystem layer.** Backups can be made immutable at
   the location's bucket ([§3](#3.%20Catalog%20backups)). Whether regulated data needs immutability
   inside the filesystem — a file, snapshot or share that cannot be changed or
   deleted before a retention date, enforced by DittoFS — is being looked at
   ([RFC 4 §8](rfc-4-remote-tier.md#8.%20Decisions%20and%20open%20questions), item 8).
4. **Does a restore need resurrection?** Closed: yes. A clone, a restore and a
   re-home can each adopt a chunk whose block compaction retired since the cut,
   so adoption resurrects the block on all three paths, whatever is deferred
   with deduplication ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace), [RFC 9 §3.3](rfc-9-gc.md#3.3%20Adoption%20resurrects%20a%20retired%20block)).

---

## Appendix A — where the current code differs

Descriptive, for the refactor.

| # | This document says | The code today |
| --- | --- | --- |
| D1 | Snapshot blocks are held by counted live and history refs (S1) | a manifest file of block hashes on local disk is an extra GC root; a hold list |
| D2 | A snapshot is logical records ([§5.1](#5.1%20Layout)) | a backend-native dump of the whole metadata store in the local store directory |
| D3 | Dirty content at the cut is held until offloaded (S7) | a snapshot may be created without the durability check, and restored with a force flag |
| D4 | Restore makes a new share ([§3.3](#3.3%20Restore)) | restore overwrites the share in place, behind a safety snapshot and a restore-in-progress marker |
| D5 | Writable clones ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)) | no clone, and no restore into another share |
| D6 | Browsable snapshots and Previous Versions ([§2.5](#2.5%20Browsing%20a%20snapshot)) | snapshots are not visible to clients |
| D7 | Retention keeps N per interval; locks ([§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)) | keep-last and a maximum age; no locks |
| D8 | Catalog backups to a location outside the metadata store ([§3](#3.%20Catalog%20backups)) | none; snapshots cannot be exported |
| D9 | Namespaces move between installations, pre-seeded ([§4](#4.%20Moving%20a%20namespace%20between%20installations)) | none; no claim, no export format |
| D10 | A snapshot is one cut record; a superseded record moves to history ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)) | one consistent read view of the whole store, held for the dump |
| D11 | Replaced refs a snapshot sees move to history (S6) | no history; overwritten content is kept only by the manifest |
| D12 | A move exports intents and retired and deleted block records ([§4.3](#4.3%20GC%20across%20installations%20on%20one%20bucket)) | none |
| D13 | History is reported, not charged (S21) | snapshot space is not reported |
| D14 | Subtree snapshots cut one shard and those beneath it ([§2.10](#2.10%20Subtree%20snapshots)) | none |
| D15 | Copying backups copy blocks to a folder of their own, incrementally, and restore into a new namespace ([§3.4](#3.4%20Copying%20backups)) | none |
| D16 | A share is re-homed into a namespace of its own while it serves ([§4.7](#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) | none; every share has its own store |
