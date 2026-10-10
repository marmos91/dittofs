---
rfc: 12
title: "RFC 12 — snapshots and clones"
component: snapshots
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-4-remote-tier]]"
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
# RFC 12 — snapshots and clones

**Status:** draft. [§7](#7.%20Open%20questions) lists what is undecided.
**Audience:** anyone implementing snapshots or clones. Catalog backups and the
export format are [RFC 26](rfc-26-catalog-backups.md), and moving a namespace
between installations is [RFC 27](rfc-27-namespace-migration.md); both read
snapshots. Conventions and test tiers are in [the RFC index](rfc-index.md).

This document specifies behaviour, not the current code. [Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists
where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

**What this is.** A snapshot is a read-only view of a share as it was at one
instant, kept as long as it is wanted and browsable from the client. This RFC
also specifies writable clones, made from a snapshot. The other ways of
protecting and moving data build on snapshots too: backups of the metadata (and
optionally of the data) and restore into a new share are
[RFC 26](rfc-26-catalog-backups.md), and moving a whole tenant to another DittoFS
installation that uses the same bucket is [RFC 27](rfc-27-namespace-migration.md).

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
  reads it; the snapshot cannot be deleted meanwhile ([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot)).
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

**How the rest is organised.** §1 lists the questions operators ask of
snapshots. §2 is the core: §2.2–§2.4 how a cut, history and the hold work (read
these), then browsing, clones, schedules and locks, deleting, space reporting,
and subtree snapshots (§2.10, skippable at first). §3 is the API and
configuration, §4 the invariants, §5–§6 tests and metrics, §7 the open
questions. Catalog backups, restore and the export format are
[RFC 26](rfc-26-catalog-backups.md); moving a namespace between installations
and re-homing a share out of a shared one are
[RFC 27](rfc-27-namespace-migration.md).

## In short

- **A snapshot** is a read-only view of a share as it was at one moment. Taking
  one writes a few records whatever the share's size, and copies nothing.
- **After a snapshot, nothing it can see is overwritten in place.** The first
  change to a record or to an extent of content keeps the old version as
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
- **A subtree snapshot** cuts only the shards of one subtree, and closes only
  their gates. It coexists with share snapshots.
- **A namespace** is a folder in a bucket, and each share gets its own by
  default.
- **Backups and moves read snapshots.** A catalog or copying backup
  ([RFC 26](rfc-26-catalog-backups.md)) and a namespace move
  ([RFC 27](rfc-27-namespace-migration.md)) each hold the snapshot they read with a
  use record, so it cannot be deleted under them.

## 1. Purpose

Operators ask four things of a file service's data protection. This document
answers the first two:

- **Undo a mistake.** A user deletes a folder, a script truncates a database
  dump, ransomware encrypts a share. The operator, or the user from their own
  client, needs the files as they were an hour or a day ago, without a restore
  ticket. Snapshots taken on a schedule, browsable in place over NFS and through
  SMB's Previous Versions ([§2.5](#2.5%20Browsing%20a%20snapshot)), and locked against deletion
  ([§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)), answer this.
- **Copy production for development, test or analytics without paying for it
  twice.** A clone ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)) is a writable share that starts as a snapshot's
  exact contents and stores only what it changes.

The other two — surviving the loss of the metadata store, and moving tenants
between installations — are answered by catalog backups
([RFC 26](rfc-26-catalog-backups.md)) and namespace migration
([RFC 27](rfc-27-namespace-migration.md)). Both read a snapshot under a use record
([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot)) and write the export format of
[RFC 26 §3](rfc-26-catalog-backups.md#3.%20The%20export%20format); a rule changed in one of the three RFCs is
checked against the other two.

Both answers here are composed from what the set already has: counted history
refs ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)), clone by adoption
([RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy)) and replicated journals
([RFC 10](rfc-10-journal-replication.md)).

### 1.1 Non-goals

This document **MUST NOT** be read as specifying:

- how a protocol presents a snapshot beyond what [§2.5](#2.5%20Browsing%20a%20snapshot) requires;
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
| **use record** | a durable record that a clone, restore, catalog backup or move is reading a snapshot, which blocks its deletion ([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot)) |
| **lock** | an expiry time before which a snapshot cannot be deleted by anyone ([§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)) |
| **detached snapshot** | a complete snapshot whose share was deleted: the share is kept **retired**, serving nothing, so that its snapshots stay restorable ([§2.8](#2.8%20Deleting)) |

## 2. Snapshots

### 2.1 A namespace is the unit that moves

A namespace is a folder — one prefix — inside a bucket of the remote tier, with
one key scope ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)). Several namespaces **MAY** share a bucket, and a
catalog backup location ([RFC 26 §2.1](rfc-26-catalog-backups.md#2.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)) **MAY** be another folder of that same
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
block acts on the whole namespace: a migration ([RFC 27 §2](rfc-27-namespace-migration.md#2.%20Moving%20a%20namespace%20between%20installations)) moves every share,
snapshot, retired block and put intent in it. A share leaves a namespace it
shares with others only by a re-home ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), which copies its content into a
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
versions of one extent both look alive at one cut ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history) shows how).

**What a snapshot sees.** Snapshot *k* sees, for each key or extent, the version
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
record that justifies the move — `klatest`'s, or `SubCut`'s newest covering
live cut, or one found in the interval — with conflict tracking, retesting against the live cuts if it is
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
([RFC 1 §4.3](rfc-1-journal.md#4.3%20Versions)), and a share's files sit in several nodes' journals; the cut
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
is seen within three periods.

> decision: three skipped ticks is fixed, not measured: one skip is a transient
> worth only a counter, and three is the fewest that separates a stopped policy
> from a busy hour while still alerting within three periods. Lower it if
> operators report missing snapshots the condition did not name; raise it if
> the condition fires on shares whose next tick succeeds.

`Cut(share).k` and `klatest` rise only at a cut, behind the gate, so the
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
  ([RFC 1 §4.3](rfc-1-journal.md#4.3%20Versions)): the offload would ship only the newer bytes, and the
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
file extent, one shard.

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
versions of one extent visible to snapshot 1. The ordering rule below forbids it.

**Worked example: a successor appended before the cut** —
`S-snap-hold-uncommitted-successor`. One file extent, one shard.

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
  same extent, so that each version's successor is the one that really replaced
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
  ([RFC 1 §6](rfc-1-journal.md#6.%20Capacity)), exceeds `snapshots.hold_journal_fraction` of its capacity.
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
> latency, which bounds a mass restore ([§5.5](#5.5%20Benchmarks%20and%20targets)). Add a fill keyed by the
> snapshot when the restore-throughput benchmark falls short of its target.

### 2.6 A writable clone is a new share in the same namespace

A clone creates a new share from a complete snapshot, in the snapshot's
namespace, as one staged import ([RFC 26 §3.2](rfc-26-catalog-backups.md#3.2%20Import%20is%20staged%20and%20published%20atomically)):

- the transaction that starts it writes a use record on the snapshot
  ([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot)), so the snapshot cannot be deleted while it is copied;
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
  highest `Number` imported ([RFC 27 §2.5](rfc-27-namespace-migration.md#2.5%20Versions%20and%20FileIDs%20on%20import)), publishes the share and deletes the use
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
| 3 | prod overwrites the extent | r → history (`born` 2, `died` 7); new ref on c2 | 2 |
| 4 | prod is deleted with its snapshots: 7's history dropped, prod's live refs released | r' | 1 |
| 5 | dev reads the extent | r' resolves c | 1 |

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
  policy snapshot ([RFC 26 §2](rfc-26-catalog-backups.md#2.%20Catalog%20backups), [RFC 26 §2.4](rfc-26-catalog-backups.md#2.4%20Copying%20backups)).
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
([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)).

> decision: a lock refuses deletion through this system's API only, and a
> governance lock yields to one separately granted right. An attacker
> holding the namespace bucket's credentials can still delete the objects; the
> namespace's bucket cannot be versioned or locked ([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)), so the
> protection against that attacker is a copying backup at an immutable location
> ([RFC 26 §2.4](rfc-26-catalog-backups.md#2.4%20Copying%20backups), [RFC 26 §1.1](rfc-26-catalog-backups.md#1.1%20Non-goals)). The lock's value is that one
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
([§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)) and with `ErrHeld` while a use record names it ([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot));
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
   ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)). The cut gate orders only reads of `Cut(share)`; it does not
   order a history move against a deletion — the tracked `LiveCut` read does.
   Each primary releases its journal hold for *k*, keeping what
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
snapshot moves with its namespace ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)). A recovery import's detached
snapshots ([RFC 26 §2.3](rfc-26-catalog-backups.md#2.3%20Restore)) are snapshots of a retired share created for them.

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

## 3. API surface

### 3.1 Interfaces

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

type Policy struct {
	Every          time.Duration
	Keep           map[Interval]int // hourly, daily, weekly, monthly → N
	TimeZone       *time.Location   // periods of Keep; nil is UTC (§2.7)
	Lock           time.Duration    // 0: policy snapshots are not locked
	LockMode       LockMode         // compliance or governance
	BackupLocation string           // empty: no backups (RFC 26)
	BackupKind     BackupKind       // catalog or copy
	BackupRetain   time.Duration    // never expires the newest complete copy (RFC 26 §2.4.4)
	VerifyEvery    time.Duration    // 0: copying backups are not verified on a period
}
```

The backup and migration surfaces are [RFC 26 §4.1](rfc-26-catalog-backups.md#4.1%20Interfaces)
and [RFC 27 §3.1](rfc-27-namespace-migration.md#3.1%20Interfaces).

Errors are a closed set across the three: this document's are `ErrHeld`,
`ErrLocked`, `ErrHoldBacklog`, `ErrSnapshotReserve`, `ErrNotShardRoot`,
`ErrSubtreeSnapshot`, `ErrLockTooLong` and `ErrSnapshotLimit`; RFC 26 and RFC 27
list theirs.

What other components gain:

- **Block and namespace metadata** ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref), [RFC 7](rfc-7-namespace-metadata.md)): `born` on
  every versioned record, stamped at the committing transaction; `died` from the
  successor; the move to history, or the drop, tested against the live cuts; a
  covering lookup and listings at a cut; the died index and the batched history
  drop of [§2.8](#2.8%20Deleting); history moves that read their `LiveCut` with conflict
  tracking; the change sequence a scan returns with each key, the commit
  timestamp ([RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend)); use records with
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
- **The offload** ([RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline)): held versions committed straight to history and
  before newer versions of their extent; runs split at live hold marks.
- **Subtree snapshots** ([§2.10](#2.10%20Subtree%20snapshots)): `LiveCut` records a cut's kind and covered
  set; `SubCut(share, shard)`; the history test by coverage; the died index
  carrying each version's shard; moves of files refused on a covered shard
  ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)).
- **The metadata store** ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)): namespace key records; the died index
  keyed by shard first; each snapshot's ordinal in `LiveCut`; the folder
  record's lease number; a retired share; the allocator raise at publish.
- **The engine at a cut** ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)): cut points per journal taken after the
  gate closes, the pre-cut existence commit up to them behind that gate, and no
  existence commit of any kind above them while the gate is closed.

### 3.2 Configuration

Snapshot policies and locks are control-plane records, set through the API
([RFC 13 §2.1](rfc-13-configuration.md#2.1%20The%20control%20plane%20is%20the%20source)) like every other record, with the scope and
class [RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings) gives each: `snapshots.directory`, `snapshots.hold_bound`
and `snapshots.reserve` per share; `snapshots.hold_journal_fraction`,
`snapshots.gate_max`, `snapshots.cut_deadline` and `snapshots.lock_max` per
installation. A policy names a backup location ([RFC 26 §4.2](rfc-26-catalog-backups.md#4.2%20Configuration));
moves and re-homes are configured as [RFC 27 §3.2](rfc-27-namespace-migration.md#3.2%20Configuration)
says. The shape below is how a provisioning file declares them
([RFC 13 §2.4](rfc-13-configuration.md#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).
Every number in it is a proposal, not a measurement: `gate_max` and
`cut_deadline` are settled by the cut benchmark of [§5.5](#5.5%20Benchmarks%20and%20targets) (the gate's
p99 under sustained offload, and the announce-to-commit time over many shards),
`hold_bound` and `hold_journal_fraction` by the dirty bytes a journal carries
at a cut in the cut-to-`complete` benchmark, `reserve_fraction` by the history
growth of [§5.4](#5.4%20Group%20B%20%E2%80%94%20cost%20and%20wedging)'s reserve check, and `lock_max` by the longest retention a
compliance policy asks for (proposal: one year).

```yaml
snapshots:
  hold_journal_fraction: 0.25   # held share of a journal's capacity, all shares (§2.4)
  gate_max: 1s                  # longest a gate stays closed, whatever the coordinator does (§2.3)
  cut_deadline: 5s              # a cut not committed this long after its announce is aborted (§2.3)
  lock_max: 8760h               # longest lock a snapshot may carry (§2.7)
  reserve_fraction: 1.0         # default reserve: history bytes per byte charged live (§2.9)
shares:
  photos:
    namespace: photos           # its own, by default (§2.1)
    snapshots:
      directory: .snapshot      # the browse directory (§2.5)
      hold_bound: 64GiB         # held bytes of this share before a cut is refused (§2.4)
      reserve: 2TiB             # history bytes before new cuts are refused (§2.9); default reserve_fraction × live bytes
      policy:
        every: 1h
        keep: { hourly: 24, daily: 7, weekly: 4, monthly: 12 }
        lock: 168h
        backup: { location: vault, kind: copy, retain: 2160h, verify_every: 720h }   # RFC 26
```

## 4. Invariants

Invariant numbers are shared by RFC 12, RFC 26 and RFC 27, and each invariant is
stated in the one that owns its subject; the numbers missing here are theirs.

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
| S21 | Quota charges live logical bytes only; history bytes are reported per share and per snapshot. |
| S22 | A subtree cut closes only its covered shards' gates and draws its number from the share's sequence. A version moves to history exactly when a live cut covering its shard sees it. A subtree snapshot shows only files of its covered shards, reached through covered directories, and no file moves into or out of a covered shard while the cut lives. |
| S27 | A snapshot contains exactly the writes whose existence committed before its cut, never one whose existence committed after. Each primary closes its gate first, then takes its final cut points and commits the shard's pending existence up to them behind the closed gate, so every write acknowledged before the snapshot was requested is in it, within one shard no write is in it while one acknowledged before it is missing, and no namespace change that follows a write acknowledged after the cut points is in it. |
| S28 | No write waits at a cut gate longer than `gate_max`; a closed gate waits for admitted transactions up to that bound and refuses the cut only past it. Three consecutive skipped policy ticks raise a health condition. |
| S33 | A detached snapshot's share is retired: it serves nothing, accepts no write, and keeps only what its snapshots see. |
| S34 | A policy tick refused by the snapshot reserve counts as a period holding a snapshot and prunes the oldest unlocked policy snapshot until the cut fits, so history ages out and cuts resume; a locked or manual snapshot is never pruned for it. |
| S35 | An ordinal is unique among the snapshots visible at any one path and is not reassigned within 24 h of its snapshot's deletion; a cut time is unique among the snapshots visible at any one path, and a subtree cut's runs ahead of the store's time only by the cuts on its own path. |
| S40 | The cut takes each shard's cut points after its gate closes and commits the shard's pending existence up to them behind that gate — a primary's own pre-cut and recovery commits pass its own closed gate — and no existence commit of any kind (group commit, removal's first phase, offer capture, explicit time set) lands above them until the gate reopens. |

## 5. Test plan and benchmarks

The set-wide rules and tiers are in [the RFC index](rfc-index.md#Test%20tiers).
Every check runs against a real metadata backend and the remote-tier emulator; GC
runs as in production, not stubbed. [RFC 26](rfc-26-catalog-backups.md) and
[RFC 27](rfc-27-namespace-migration.md) run their checks on the model checker and
simulator described here.

### 5.1 How it is tested

**A model.** The cut ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)), the hold's lifecycle ([§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)), the
deletion ([§2.8](#2.8%20Deleting)) and subtree coverage ([§2.10](#2.10%20Subtree%20snapshots)) **MUST** be
modelled in a model checker before they are implemented, with S4, S5, S7, S9 and
S22 as properties, and kept in step with this document. RFC 26 and RFC 27 add
their own models to the same checker.

**Deterministic simulation.** The snapshot service, the gate, the journal's holds
and the move reach the network, the journals, the metadata store, the remote
store, time and randomness only through interfaces. A simulator drives a cluster
of installations, nodes and one bucket in one process from a seed; a failing seed
**MUST** reproduce the failure exactly. It runs the scenario catalogues of this
document ([§5.2](#5.2%20Scenario%20catalogue)), [RFC 26](rfc-26-catalog-backups.md#6.2%20Scenario%20catalogue)
and [RFC 27](rfc-27-namespace-migration.md#5.2%20Scenario%20catalogue), scripted and
replayed exactly, and a randomized seed search over every fault at once: node
crashes discarding unsynced writes, lease lapses and takeovers mid-cut,
coordinator crashes, metadata-store stalls and unknown commit outcomes,
remote-store outages, clock drift within and beyond the bound, and process pauses
between a claim check and a delete.

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
refused by each bound; a
cross-shard transaction releasing its admissions at a closed gate; a history move
retried on a deletion's `LiveCut`; a stamp rebuilt after takeover; a hold record
written by a move between shards; an uncovered shard's transaction stamped below a
subtree cut it committed after; a move of files refused on a covered shard; a
pre-cut existence commit carrying an unflushed write into a snapshot; a
reserve-refused tick that pruned. RFC 26 and RFC 27 list the branches their
scenarios add.

### 5.2 Scenario catalogue

| ID | Scenario |
| --- | --- |
| `S-snap-hold-overwrite` | [§2.4](#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)'s example: dirty content overwritten after the cut is held and offloaded to history |
| `S-snap-cut-failover` | [§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)'s example: a cut across two shards while one fails over |
| `S-snap-delete-middle` | [§2.8](#2.8%20Deleting)'s example: deleting the middle of three snapshots visits only its interval |
| `S-snap-clone-delete-source` | [§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)'s example: clone, then delete the source and its snapshot |
| `S-snap-locked-delete` | [§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)'s example: a locked snapshot refuses deletion, shortening and pruning |
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
| `S-snap-smb-token` | two cuts requested within one second, and a cut at B after a move from an A whose clock ran ahead, get distinct Previous Versions tokens |
| `S-snap-subtree-rename` | [§2.10](#2.10%20Subtree%20snapshots)'s example: a subtree snapshot closes one shard's gate; a rename out of the subtree and a write to the renamed file keep its history; an uncovered shard writes none; deletion drops it all |
| `S-snap-subtree-coexist` | share cuts and subtree cuts of two shards interleave; deleting each in turn keeps exactly the history the remaining ones see, per shard, and `klatest` and every `SubCut` end at the greatest live covering cut |
| `S-snap-subtree-stale-stamp` | a transaction on an uncovered shard reads the cut number before a subtree cut and commits after it; a later share cut reads it correctly |
| `S-snap-subtree-nested` | a subtree cut of a shard with per-child shards beneath it covers them; an entry naming a file of an uncovered shard is left out of the browse |
| `S-snap-subtree-move-refused` | marking a directory for per-child shards inside a covered shard, and a move into it, are refused `ErrSubtreeSnapshot`; a handover of its primary runs; after the deletion the move proceeds |
| `S-snap-retention-dst` | a year of hourly ticks in a zone with daylight saving, with a two-day outage: kept snapshots match the period rule exactly, and the outage prunes nothing |
| `S-snap-subtree-scale` | 10⁴ per-child shards, each with an hourly subtree snapshot kept 24: each subtree deletion reads only its own shard's history, the deletion queue drains, and no cut is refused `ErrSnapshotLimit`; no two snapshots visible at one path share an ordinal, and no ordinal is reassigned within 24 h of its snapshot's deletion; every cut time stays within a few seconds of the store's time, and Previous Versions at each path lists distinct tokens. Draw cut times from one share-wide sequence: they run hours ahead within a day |
| `S-snap-gate-bounds` | a slow admitted transaction and a stopped coordinator meet the gate's drain and ceiling bounds |
| `S-snap-detach` | a share is deleted with its snapshots detached, and they are restored and deleted in turn |

### 5.3 Group A — lost or wrong content

| Invariant | Check |
| --- | --- |
| S1 | Snapshot a share, delete every file, run sweep and collection to completion. Every block the snapshot names survives, and every snapshot file reads back by hash. |
| S2 | Place one share's files in two journals and two shards, and advance one journal's versions far past the other's. Overwrite files in both between snapshots. Every snapshot reads back its model copy. Replace the cut-number comparison with a comparison of journal versions: the check fails. |
| S3 | Model-based run above; then kill the process at every step between the cut and `complete`, and inside every transaction that moves a record to history; restart. Each snapshot completes and equals the model. |
| S3 | Give a file mode 0644 and an ACL denying principal X; snapshot; replace the ACL, then release the file. Browsing the snapshot as X is refused both times. Leave the ACL unversioned: the check fails. |
| S4 | `S-snap-cut-failover` and `S-snap-shard-move-mid-cut`. Remove the epoch check from the cut transaction: a post-cut `chmod` appears in the snapshot. `S-snap-cross-shard-gate`: admit per gate instead of all-or-nothing, and cuts abort at their deadline. |
| S5 | Acknowledge a write after the cut points, before the gate reopens: its existence commits after the cut and the snapshot does not show it. `S-snap-carve-straddle`: remove the split, and the snapshot reads post-cut bytes. `S-snap-stamp-takeover`: leave stamps unreplicated, and more than one version per file needs a rebuilt stamp. Two overwrites of one extent between snapshots, then a truncate over it (a model-test seed): the superseded piece dies at the lower overwrite's `born`, no history key collides, and the truncate commits. |
| S6 | `S-snap-hold-overwrite` with v3 committed first: the check fails on two versions visible at cut 1. `S-snap-offload-after-drop`: remove the live-cut test, and the audit reports an orphan history ref. `S-snap-delete-vs-history`: read `LiveCut` untracked, and the audit reports an orphan history ref. |
| S7 | `S-snap-hold-takeover` and `S-snap-hold-lost`. Keep holds unreplicated: the first reads the wrong bytes. `S-snap-hold-uncommitted-successor`: let the uncommitted successor supersede, and the snapshot reads r1. `S-snap-hold-shard-move`: leave the receiving hold record out of the move's commit, and the snapshot completes reading the wrong bytes. |
| S8 | `S-snap-remote-down` and `S-snap-journal-bound`: each cut commits within the gate target, writes never stall, snapshots stay `holding`, refused cuts return `ErrHoldBacklog`; restore the tier and every snapshot completes. `S-snap-hold-bound-dirty`: count held bytes only, and the journal fills. |
| S9 | `S-snap-delete-middle`, `S-snap-delete-concurrent` and `S-snap-delete-serialised`, killing the process between batches. After each, exactly the history no remaining snapshot sees is gone, `klatest` is the greatest live cut, and the audit is clean. Let two deletions run at once: `S-snap-delete-serialised` ends with `klatest` naming a deleted cut. |
| S10 | `S-snap-clone-delete-source`; then write to both and read each back. |
| S11 | `S-snap-locked-delete`, `S-snap-clone-vs-delete`, `S-snap-backup-abandoned` (RFC 26). Crash expiry and abandonment after each step: a restore never succeeds from an expired or failed backup, and never reads a swept block. |
| S21 | Rewrite a 10 GiB file hourly under a quota with hourly snapshots: charged usage stays at 10 GiB, `history_bytes` grows, and bytes freed if deleted for the oldest snapshot matches what its deletion frees. |
| S22 | `S-snap-subtree-rename`, `S-snap-subtree-coexist` and `S-snap-subtree-nested` under the model-based run: each live snapshot of either kind reads back its model copy. `S-snap-subtree-stale-stamp`. Test coverage by klatest alone, ignoring `SubCut`: the subtree snapshot reads post-cut bytes. Allow the move of `S-snap-subtree-move-refused`: a later write drops history the subtree snapshot reads. |
| S27 | Write with `UNSTABLE` and acknowledge, write a stable overwrite with no `COMMIT`, and take a snapshot at once, before any group commit is due: the snapshot has both writes. Write w1 then w2 to two files of one shard with the group commit stalled, and cut between them while the stall lasts: the snapshot holds w1 whenever it holds w2. Remove the pre-cut commit: the snapshot lacks both writes of the first case. Stall the store past `gate_max`: the cut is aborted, the gate reopens and writes keep being acknowledged. Write and flush `~tmp1` after the cut points and rename it over `report.docx` while the gate is closed: the snapshot shows the old `report.docx`, never a zero-length one. `S-snap-cut-failover`: a primary that starts closed after a takeover commits its recovery and pre-cut existence behind its own gate and replies; the snapshot holds every write acknowledged before the request. |
| S28 | `S-snap-gate-bounds`: under sustained offload, with a group commit of 256 files and an offload commit filling the key budget K admitted at every close and taking 200 ms each, every hourly cut for a day commits. Cap the drain at 50 ms: every cut is refused. A prepared cross-shard transaction holds one shard past `gate_max`: the gate reopens by `gate_max`, the cut is refused and retried, and no write waited longer. Stop the coordinator after every gate closed: every gate reopens by `gate_max`. Refuse three ticks in a row: the health condition names the share. |
| S33 | `S-snap-detach`: delete a share detaching its two snapshots; the share serves nothing and refuses writes; each snapshot restores; content no snapshot saw is released; deleting the second snapshot removes the share's records. |
| S11 | Set a lock past `lock_max`: refused `ErrLockTooLong`. Shorten a governance lock with the override right: allowed, logged and counted; without it, and on a compliance lock: `ErrLocked`. |

### 5.4 Group B — cost and wedging

| Concern | Counted check |
| --- | --- |
| [§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate) cost to take | Taking a snapshot of a 10²- and a 10⁷-file share writes the same number of records. |
| [§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree) cost to change | After a cut, the first change to a record writes one history version and a second writes none; with no live snapshot no change writes history. |
| [§2.8](#2.8%20Deleting) deletion | Deleting a snapshot reads only history in its interval, whatever the share's total history, and writes no transaction larger than the batching bound. |
| [§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks) retention | A simulated year of hourly ticks under the example policy: never more than 47 policy snapshots, manual snapshots untouched, locked ones kept to expiry. |
| [§2.10](#2.10%20Subtree%20snapshots) cut | A subtree cut of one per-child shard of a 10⁴-shard share closes one gate, and reads one shard record. |
| [§2.10](#2.10%20Subtree%20snapshots) subtree deletion | `S-snap-subtree-scale`: keys read by a subtree deletion are proportional to its own shard's history in its interval, whatever the number of shards. |
| [§2.9](#2.9%20Space%20is%20reported%2C%20not%20charged) reserve | A share with no reserve set and hourly snapshots of a file rewritten hourly: cuts are refused `ErrSnapshotReserve` once history reaches its live bytes. Keep running for 48 h under `keep: { hourly: 24 }`: each refused tick still prunes the oldest policy snapshot, history falls, and cuts are taken again within a day. Skip refused periods as empty: no cut is taken after the first refusal. Under `keep: { hourly: 24, daily: 7, weekly: 4, monthly: 12 }` with 5% daily churn: refused ticks prune the oldest unlocked policy snapshot, weekly and monthly ones included, and a cut is taken within a day; a locked snapshot survives. |

### 5.5 Benchmarks and targets

Recorded on the reference box ([the RFC index](rfc-index.md#Test%20tiers)); the
10⁷-file rows run daily.

| Benchmark | Measures | Proposed target |
| --- | --- | --- |
| Cut under a sustained writer, 64 clients, one shard and 64 shards | gate held; p99 write latency across the cut | gate ≤ 50 ms; p99 within 2× of no cut |
| Snapshot of a 10⁷-file share | wall time, records written | same as a 10²-file share |
| Cut to `complete`, 10 GiB dirty at the cut | seconds | at most the dirty bytes over the measured offload rate, plus 1 s |
| Live write throughput with hourly snapshots, 24 kept | ops/s against none | within 10% |
| Snapshot read of a file overwritten 10³ times since | p99 against a live read | report, against the ponytail of [§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree) |
| Deleting one hourly snapshot of a share with 2.6×10⁵ history refs | keys read | proportional to the history in its interval |
| Clone of a 10⁶-file snapshot | wall time | report, against the ponytail of [§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace) |
| Restore throughput: copy every file of a 10⁶-file, 1 TiB snapshot out through the browse path, 64 readers, cold | MB/s and files/s against a live cold read | at least half the live cold read |
| Subtree cut of one of 64 shards under a sustained writer on every shard | p99 write latency on the other 63 across the cut | unchanged against no cut |

## 6. Observability

Metric names are shown without the deployment's prefix, which the exporter adds.

| Answers | Metric | Type |
| --- | --- | --- |
| snapshot outcomes, labelled `result` = `complete`, `hold_backlog`, `reserve`, `aborted`, `failed`, and `kind` = `share`, `subtree` | `snapshot_total` | counter |
| cut gate held, per shard | `snapshot_gate_seconds` | histogram |
| held bytes not yet offloaded, per share and per journal, and time from cut to `complete` | `snapshot_held_bytes`, `snapshot_complete_seconds` | gauge, histogram |
| history bytes per share; history refs and records; those dropped by deletion | `snapshot_history_bytes`, `snapshot_history_refs`, `snapshot_history_records`, `snapshot_history_dropped_total` | gauge, gauge, gauge, counter |
| policy ticks, labelled `result` = `taken`, `skipped`, `pruned`, `locked` | `snapshot_policy_total` | counter |
| deletions refused or deferred, labelled `reason` = `locked`, `held`, `busy` (another deletion of the share running), `moving` | `snapshot_delete_refused_total` | counter |
| use records released at their deadline, labelled `reader` = `backup`, `clone`, `restore` | `snapshot_use_abandoned_total` | counter |
| cross-shard transactions that released their admissions at a closed gate | `snapshot_gate_readmissions_total` | counter |
| history moves retried on a conflict with a deletion's `LiveCut` | `snapshot_history_conflicts_total` | counter |
| moves of files refused because a subtree snapshot covers the shard | `snapshot_subtree_move_refused_total` | counter |
| cuts refused by the gate's ceiling; consecutive skipped ticks per policy, a health condition at 3 | `snapshot_gate_refused_total`, `snapshot_policy_skipped_consecutive` | counter, gauge |
| governance-lock overrides, labelled by principal; any value is an audit event | `snapshot_lock_overrides_total` | counter |

Logs: each cut, completion and abort logs at `Info` with share, namespace and
snapshot. A refused cut and a failed snapshot log at `Warn`, naming the bound; a
refused move of files logs at `Warn`.

## 7. Open questions

1. **Immutability at the filesystem layer.** Backups can be made immutable at
   the location's bucket ([RFC 26 §2](rfc-26-catalog-backups.md#2.%20Catalog%20backups)). Whether regulated data needs immutability
   inside the filesystem — a file, snapshot or share that cannot be changed or
   deleted before a retention date, enforced by DittoFS — is being looked at
   ([RFC 4 §8](rfc-4-remote-tier.md#8.%20Decisions%20and%20open%20questions), item 8).
2. **Does a restore need resurrection?** Closed: yes. A clone, a restore and a
   re-home can each adopt a chunk whose block compaction retired since the cut,
   so adoption resurrects the block on all three paths, whatever is deferred
   with deduplication ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace), [RFC 9 §3.3](rfc-9-gc.md#3.3%20Adoption%20resurrects%20a%20retired%20block)).

---

## Appendix A — where the current code differs

Descriptive, for the refactor.

| # | This document says | The code today |
| --- | --- | --- |
| D1 | Snapshot blocks are held by counted live and history refs (S1) | a manifest file of block hashes on local disk is an extra GC root; a hold list |
| D2 | A snapshot is logical records ([RFC 26 §3.1](rfc-26-catalog-backups.md#3.1%20Layout)) | a backend-native dump of the whole metadata store in the local store directory |
| D3 | Dirty content at the cut is held until offloaded (S7) | a snapshot may be created without the durability check, and restored with a force flag |
| D5 | Writable clones ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)) | no clone, and no restore into another share |
| D6 | Browsable snapshots and Previous Versions ([§2.5](#2.5%20Browsing%20a%20snapshot)) | snapshots are not visible to clients |
| D7 | Retention keeps N per interval; locks ([§2.7](#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)) | keep-last and a maximum age; no locks |
| D10 | A snapshot is one cut record; a superseded record moves to history ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)) | one consistent read view of the whole store, held for the dump |
| D11 | Replaced refs a snapshot sees move to history (S6) | no history; overwritten content is kept only by the manifest |
| D13 | History is reported, not charged (S21) | snapshot space is not reported |
| D14 | Subtree snapshots cut one shard and those beneath it ([§2.10](#2.10%20Subtree%20snapshots)) | none |
