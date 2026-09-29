---
rfc: 12
title: "RFC 12 — snapshots, backups and share migration"
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
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-9-gc]]"
  - "[[rfc-11-ownership]]"
aliases:
  - RFC 12
tags:
  - rfc
---
# RFC 12 — snapshots, backups and share migration

**Status:** draft. [§10](#10.%20Open%20questions) lists what is undecided.
**Audience:** anyone implementing snapshots, backup and restore, or moving a share
between installations that share one remote store. Conventions and test tiers are
in [the RFC index](rfc-index.md).

This document specifies behaviour, not the current code.
[Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists where the code differs.

---

## In short

- A **snapshot** is a share at one instant, read-only, recorded as one **cut
  number**. Its tree and content are the records and refs committed before that
  cut: still live, or moved to **history** by the first change after it and kept
  ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)). Taking one writes one record, whatever the share's size; the count of
  its refs is all that keeps its blocks.
- Every snapshot is taken at a **cut**: one transaction behind a gate held only
  while in-flight transactions finish. Content still dirty at the cut is pinned
  in the journal until offloaded, and the snapshot completes then
  ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)). Nothing is drained and writes never freeze.
- A **writable clone** is a new share built from a snapshot in the same
  namespace, sharing chunks with its source and nothing else.
- A **backup** exports one snapshot's metadata outside the metadata store, never
  blocks, and holds its snapshot until it expires. **Restore** makes a new share.
- A **namespace** — one remote prefix, one counting domain — has one owning
  installation. **Migration** moves it to another installation on the same bucket:
  drain, freeze, release, import, claim. No block is copied.

## 1. Purpose

Operators need to recover a share as it was, to survive losing the metadata
store, and to move a share between installations without copying data. This
document composes those from what the set has: counted history refs
([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)), clone by adoption ([RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy)), restore by adoption
([RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)), and GC as a service of one namespace ([RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace)).

### 1.1 Non-goals

This document **MUST NOT** be read as specifying:

- a copy of the block store: the remote store is the only copy of content, and
  protecting the bucket is outside this set;
- a backup of material: an export names material IDs, never material
  ([RFC 5 §2.5](rfc-5-transforms.md#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material));
- two installations serving one share at once. That is replication and ownership
  ([RFC 10](rfc-10-journal-replication.md), [RFC 11](rfc-11-ownership.md)); [§4.6](#4.6%20After%20replication) says how the two relate;
- how a protocol presents a snapshot; [§2.5](#2.5%20Browsing%20a%20snapshot) states only what protocols rely on;
- restoring in place: a restore makes a new share ([§3.3](#3.3%20Restore)), and swapping it in
  is a rename in the control plane.

### 1.2 Terms

| Term | Means |
| --- | --- |
| **namespace** | one remote-store prefix ([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)) with one key scope ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)): the counting domain of [RFC 9 §2.3](rfc-9-gc.md#2.3%20The%20absence%20of%20a%20record%20proves%20nothing) and the unit GC serves. It holds one or more shares |
| **installation** | one control plane with its metadata store, serving some namespaces |
| **owner** | the one installation that may put, sweep, relocate or collect in a namespace |
| **cut** | the instant a snapshot describes ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)); its **cut number** *k* is the share's count of cuts, raised by one at each cut. Its type is `SnapshotCut` ([RFC 6](rfc-6-block-metadata.md)) |
| **snapshot** | the user-visible record of one cut: share, `SnapshotCut`, name, creation time, expiry — the `Snapshot` entity of [RFC 16](rfc-16-metadata-store.md), kept under the share's key prefix |
| **klatest** | the cut number of the share's newest live snapshot, 0 when there is none, recorded with the share |
| **born**, **died** | the share's cut number when a ref or a versioned record was committed, and when it was superseded ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)) |
| **versioned record** | a namespace record a snapshot reads — File, entry, ACL, xattr, stream link, hole, directory delta — carrying `born` ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)) |
| **history** | a ref or versioned record the share replaced while a snapshot could still see it, kept with its `died`; a history ref is counted ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)) |
| **pin** | journal content a cut sees that no offload had committed at the cut, kept by the journal until offloaded ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)) |
| **export** | a self-describing stream of metadata records ([§5](#5.%20The%20export%20format)): kind `backup` (one snapshot) or `move` (a whole namespace) |
| **import** | building records from an export, staged and published at once ([§5.2](#5.2%20Import%20is%20staged%20and%20published%20atomically)) |

## 2. Snapshots

### 2.1 A namespace is the unit that moves

A block may be counted by every share of its namespace ([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)), so
whatever changes who counts a block acts on a namespace: a clone ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)) or
restore ([§3.3](#3.3%20Restore)) creates its share **in the namespace that holds the blocks**, and a
migration ([§4](#4.%20Moving%20a%20namespace%20between%20installations)) moves a whole namespace with every share, snapshot, put intent
and retired or deleted block in it. A new share **SHOULD** get a namespace of its own, so that it can
be moved alone; shares configured into one namespace deduplicate against each
other and move together.

### 2.2 A snapshot is counted content and a frozen tree

A snapshot is its **cut number** *k*, recorded in one `LiveCut` record, and
nothing else is written when it is taken. What it sees is read from records the
share already keeps, each stamped with the cut it was written under:

- **the tree**: the share's namespace records ([RFC 7 §2](rfc-7-namespace-metadata.md#2.%20The%20entities)) — files, with the
  FileData fields they carry, entries, ACLs, xattrs, the links from a file to
  its named streams — and each regular file's holes ([RFC 6](rfc-6-block-metadata.md)) and each
  directory's unfolded time deltas ([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps)). These are **versioned
  records**: each carries `born`, and a superseded version a live snapshot can
  still see is kept in history with its `died` ([§2.4](#2.4%20Capture%20is%20copy%20on%20first%20write)). They hold no remote
  content and are not counted;
- **the content**: the refs committed before the cut, live or history. The
  share's record `Cut(share)` holds its latest cut number, and every transaction
  that writes a ref or a versioned record reads it and stamps `born` with it; the
  share's cut gate ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)) orders every such transaction before or after
  the cut ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)), so every record committed before cut *k* has
  `born < k` and every later one `born ≥ k`;
- **the pins**: content whose existence committed before the cut but that no
  offload had yet committed is kept by the journal, pinned to the cut, until an
  offload commits it ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)).

**Superseded records move to history.** The share records *klatest*, the newest
live snapshot's cut number. Any transaction that drops or replaces a ref or a
versioned record with `born < klatest` — an offload commit over a ref, a
removal's batch, a release, a clone's destination, an attribute change, a
rename, an ACL or xattr change, a fold of directory deltas — **MUST**, in the
same transaction, move the old value to history with `died` set to the cut
number that transaction read ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). A history ref counts like a live
one: a chunk's refcount is its live refs plus its history refs
([RFC 9 §2.1](rfc-9-gc.md#2.1%20The%20count%20is%20the%20only%20authority)). A record with `born ≥ klatest` no live snapshot can see, and is
replaced or dropped as before.

**What a snapshot sees.** Snapshot *k* sees, for each ref and each versioned
record, the version with `born < k ≤ died`, where a live version has `died = ∞`.

For example: a file is written and offloaded while the share's cut number is 0,
so its ref and its File have `born` 0. Snapshot 1 is taken; `klatest` is 1. The
file is then `chmod`ed, overwritten and offloaded. The `chmod` reads cut number
1, finds the File's `born` 0 below *klatest* 1, and moves the old File to history
with `died` 1 as it writes the new one with `born` 1; the offload commit does
the same with the ref. Snapshot 1 reads the old File and the old ref, since
0 < 1 ≤ 1, and not the new ones, since 1 < 1 is false; the live share reads the
new ones. Had no snapshot existed, both commits would have replaced in place,
and the offload would have decremented the old chunk.

**Directory deltas need no special case.** A delta is a versioned record, born
at the cut it was written after. The fold that consumes it deletes it and
rewrites the directory's File, and both moves send the old values to history
when a live cut sees them. Snapshot *k* then reads the directory version it sees
plus the deltas it sees: a delta folded by a fold before *k* is inside that
version and has `died < k`; a delta folded at or after *k* is not, and has
`born < k ≤ died`. No delta is applied twice or missed.

Journal versions do not appear here. Each journal numbers its own files
([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)), and a share's files may sit in several nodes' journals, so no
version orders a share; the cut number, one record every commit reads, does.

Nothing else — no manifest, hold list or extra GC root — keeps a snapshot's
blocks alive ([RFC 9 §2.1](rfc-9-gc.md#2.1%20The%20count%20is%20the%20only%20authority), [RFC 7 §4.4](rfc-7-namespace-metadata.md#4.4%20There%20is%20no%20third%20holder)). A pin keeps journal bytes, never
a block.

> ponytail: a snapshot read scans a record's or a file's history at each key or
> offset it resolves, O(history versions of it). Upgrade to an interval index
> over `(born, died]` when snapshot reads of a much-changed file or directory
> show in a profile.

A snapshot is `pinned`, `complete`, `failed` or `deleting`; only a complete one is
browsed, cloned, backed up or restored. A failed one's records are removed as a
deletion removes them ([§2.8](#2.8%20Deleting)).

> [!important] Pending review — snapshots are versioned records
> Namespace records carry `born`/`died` like refs; a snapshot is one cut record;
> no tree is copied. Replaces the whole-tree walker and its O(files) cost.

### 2.3 The cut is one transaction behind a brief gate

Taking snapshot *k* is:

1. **Gate.** Close the share's cut gate ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)) at every owner of its units
   ([RFC 11 §2](rfc-11-ownership.md#2.%20Ownership%20units)): new transactions that write a ref or a versioned record wait
   at the gate, and those admitted finish. Reads continue. Nothing is offloaded
   or drained.
2. **Cut.** In one transaction, raise the share's cut number to *k*, write
   `LiveCut(share, k)`, set *klatest* to *k*, and create the snapshot record in
   state `pinned`. Each owner records, with the cut, a **pin mark** per journal
   holding the share's files: for each file, the journal version its existence
   has committed up to (its `applied`, [RFC 6 §2.4](rfc-6-block-metadata.md#2.4%20FileData%20and%20holes)).
3. **Open.** Reopen the gate. Every later transaction reads the new cut number.

The gate is held for one transaction and the transactions already in flight,
never for an offload; it **MUST** be bounded by the transaction deadline, and a
cut whose gate cannot close in time aborts and reopens it. Every share has a
remote tier, so every share can be snapshotted.

**Dirty content is pinned, not drained.** Content at or below a pin mark that
no offload has yet committed is what the snapshot sees and the remote tier does
not yet hold. The journal **MUST** keep every such version — whatever
supersedes it afterwards: an overwrite, a truncate, a deallocate or a release —
until an offload has committed it ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)). The offload of a superseded
pinned version writes its ref straight to history, with `born` the cut its
existence committed under and `died` the cut of the transaction that superseded
it; the offload of a version still live writes an ordinary ref. An offload's
capture step offers both: the journal hands the pinned superseded versions to
the same pass, flagged as superseded, and the pipeline carves, puts and commits
them like any other content ([RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline), [RFC 1 §3.11](rfc-1-journal.md#3.11%20Snapshot%20pins)). When no pin of
cut *k* remains, one transaction marks the snapshot `complete`. Pins are in the
journal's durable state, so a restart resumes them.

> decision: a snapshot becomes browsable only once its pins are offloaded, so
> a snapshot read, a clone and a backup never name journal content and never
> reach another node's journal. The cost is a delay between the cut and
> `complete`, bounded by the offload of the share's dirty bytes at the cut.
> Serve pinned content from the journal if that delay is ever the complaint.

**Pins are bounded.** A share's pinned bytes count against its journal's
capacity ([RFC 1](rfc-1-journal.md)). A cut **MUST** be refused, with `ErrPinBacklog`, while the
share's pinned bytes not yet offloaded exceed the configured `pin_bound`: with
the remote tier down, snapshots stop before the journal fills, and writes never
stop for a snapshot.

A removal whose phase 1 committed before the cut but whose batches are not done
([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)) masks for a snapshot as it does for the share: the FileData and
holes the snapshot sees already record the removal, and its later batches move
the refs they drop to history with `died` ≥ *k*, which the snapshot would see
but its existence excludes. A removal whose phase 1 commits after the cut masks
nothing for the snapshot, which reads the refs it has not yet dropped, or their
history.

> [!important] Pending review — no drain, no freeze
> The cut is one transaction behind the cut gate. Dirty journal content is
> pinned to the cut until offloaded, and the snapshot completes when no pin
> remains. Heading renamed from "The cut is a drained, frozen instant".

### 2.4 Capture is copy on first write

Nothing is copied when a snapshot is taken. The first transaction after the cut
that supersedes a record the cut can see copies its old value to history, in
that transaction ([§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)); later transactions on the same record find
`born ≥ klatest` and write in place. So a snapshot costs one record to take, and
each record changed afterwards costs one history write, once per live cut it
crosses.

History versions live under the share's history prefix, separate from the
snapshot records and from live records, keyed by the record's own key and then
`died` ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)): a snapshot read of one record is a live read and, when
the live version is too new, one seek into history; a snapshot listing merges
the directory's live entries with its history entries in key order. Refs keep
their own `History(file, died, offset)` records ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)).

**Every record a snapshot read consults is versioned** — the File and its
FileData fields, entries, holes, directory deltas, the ACL, xattrs and stream
links. A record left unversioned would leak: an ACL replaced or released after
the cut would judge the snapshot's content by the new rules, or fall back to
the ACL the mode implies, and let a denied principal read it.

A file released after the cut keeps its namespace records and its content
through the history its release wrote.

> ponytail: deleting a snapshot scans the share's whole history prefix for
> versions with `died` in `[k, kn)`, O(history records of the share). Upgrade
> to a second index ordered by `died`, written with each history version, when
> snapshot deletion time shows in the retention pruner's runtime.

### 2.5 Browsing a snapshot

A complete snapshot is reachable read-only under a virtual directory at the
share's root, configurable and hidden from listings. What protocols rely on:

- a handle into a snapshot names the share, the snapshot and the file
  ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)), so it never resolves to a live file; once the snapshot is
  deleted it resolves stale ([RFC 7 §6.3](rfc-7-namespace-metadata.md#6.3%20Staleness%20is%20reported%2C%20never%20guessed));
- a read resolves by the covering lookup ([RFC 6 §8.1](rfc-6-block-metadata.md#8.1%20Covering%20lookup)) at the cut: the file's
  refs with `born < k ≤ died`, live and history, against the FileData and holes
  the cut sees, which take precedence over any ref, and fetches from the remote
  tier;
- permissions are evaluated against the file, ACL and share grant as the cut
  sees them — the ACL versioned with the file ([§2.4](#2.4%20Capture%20is%20copy%20on%20first%20write)), the grant as it is
  now, so a principal removed from the share reaches none of its snapshots;
- every mutating operation fails with the read-only error;
- a snapshot file's numeric file id differs from the live file's
  ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20derived%2C%20and%20collisions%20are%20its%20problem)), so tools do not take the two for one file.

> ponytail: snapshot reads bypass the journal and run at the remote tier's
> latency, which bounds a mass restore ([§8.3](#8.3%20Benchmarks%20and%20targets)). Add a fill keyed by the
> snapshot when the restore-throughput benchmark falls short of its target.

### 2.6 A writable clone is a new share in the same namespace

A clone creates a new share from a complete snapshot, in the snapshot's
namespace, as one staged import ([§5.2](#5.2%20Import%20is%20staged%20and%20published%20atomically)): the tree the cut sees is copied with **new FileIDs**,
mapped so that hard links stay links, and the refs the
snapshot sees become the new file's refs, raising each chunk's refcount. That is
[RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)'s restore, conditional on each chunk existing ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence)), written in
bounded batches ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)), and it cannot fail while the snapshot holds
its refs.

The clone then shares **chunks and nothing else** with its source. Deleting the
source share, the snapshot or the clone releases only that one's own refs, and
changes nothing the other two read. The clone moves with its namespace ([§2.1](#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)).

> ponytail: a clone copies every ref, O(refs) records. Upgrade to files that
> read through their source snapshot until first written, when clone latency on
> large shares matters.

### 2.7 Scheduled snapshots and retention

A share **MAY** carry one snapshot policy: a schedule, and a retention of *keep N
per interval* over any of `hourly`, `daily`, `weekly` and `monthly`. A policy
snapshot is kept while it is the newest snapshot of one of the N most recent
periods of any interval; otherwise pruning deletes it ([§2.8](#2.8%20Deleting)).

- Pruning deletes only snapshots the policy made, never manual ones.
- A tick refused by the pin bound ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)) is skipped, not queued, and counted;
  ticks missed while down produce at most one snapshot on start. A cut costs one
  record, so a schedule never falls behind a large share.
- A policy **MAY** name a backup location and retention; each policy snapshot is
  then backed up ([§3](#3.%20Backups)).
- The scheduler runs once per share, wherever the share's control plane runs.

### 2.8 Deleting

Deleting a snapshot **MUST** be refused while an unexpired backup holds it
([§3.2](#3.2%20A%20backup%20holds%20its%20snapshot)); pruning defers such a snapshot until its last backup expires.

A deletion drops the history — refs and versioned records — that no other live
snapshot sees. With the
nearest live cuts *kp* < *k* < *kn* — *kp* absent reads as 0, *kn* absent as ∞ —
those are the versions with

    kp ≤ born < k ≤ died < kn

Snapshot *k* sees them, since `born < k ≤ died`; *kp* does not, since
`born ≥ kp`; *kn* does not, since `died < kn`; and no live cut further out can
see a version that neither neighbour sees, because the cuts a version is visible
to are consecutive ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)).

1. One transaction marks the snapshot `deleting`, so nothing browses, clones or
   backs it up, and removes its cut with RFC 6's `DropCut`, through the share's
   cut gate — ref-writing transactions pause only while it commits: the live-cut record
   goes, and, if it was the newest, *klatest* becomes *kp*. No history the
   deletion must drop is created after it. The journal releases the snapshot's
   pins not yet offloaded ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)) that no other live cut holds.
2. Its history refs are dropped per file by the batching pattern of
   [RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation), each decrementing its chunk, and its history versions of
   namespace records by the same pattern; a restart resumes.
3. The snapshot record is removed last.

For example, with live cuts 5, 9 and 14, deleting 9 drops the history refs with
`5 ≤ born < 9 ≤ died < 14`. A ref with `born` 3 and `died` 12 stays: snapshot 5
sees it. A ref with `born` 6 and `died` 14 stays: snapshot 14 sees it.

Deleting
a share with snapshots **MUST** be refused unless the request deletes or detaches
them; a detached snapshot belongs to the namespace, stays restorable, and is
deleted only explicitly.

## 3. Backups

### 3.1 A backup is an export of one snapshot's metadata

A backup writes an export of kind `backup` ([§5](#5.%20The%20export%20format)) to a configured **backup
location**: a directory or an object-store prefix outside every block namespace
([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)), never inside the metadata store it protects. It carries the
namespace records the snapshot sees — files with their FileData fields, entries,
ACLs, xattrs, stream links and holes, as plain records with directory deltas
folded in — and per file the refs the snapshot sees, written as plain refs at
their versions; the chunk and block records of every
chunk they name, as **location hints** only ([§3.3](#3.3%20Restore)); the material in those
blocks' census, each as (material ID, fingerprint) ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)); the
(material ID, fingerprint) of each of the namespace's wrapped keys — its data
keys, header key and chunking key — never the keys
([§4.4](#4.4%20Key%20scope%20and%20material)); and the namespace's identity, prefix and key scope. Its
state — `writing`, `complete`, `expired` — is kept in the location, listable
without the installation that wrote it.

### 3.2 A backup holds its snapshot

Until a complete backup expires, its snapshot cannot be deleted ([§2.8](#2.8%20Deleting)), so
the refs the snapshot sees keep every block the backup names counted. The backup
adds no second liveness mechanism ([RFC 9 §2.1](rfc-9-gc.md#2.1%20The%20count%20is%20the%20only%20authority)).

Expiry marks the backup `expired` in its location, then releases the hold, then
removes the export. An expired backup **MUST NOT** be restored; the reverse order
lets a crash leave a restorable backup whose blocks were swept.

### 3.3 Restore

Restore creates a new share. There are two sources:

- **From a snapshot**, or a backup whose snapshot still exists: a clone
  ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)), whose records are current.
- **From a backup alone**, when the owner's metadata store is lost: a **recovery
  import**, which takes the namespace over ([§4.1](#4.1%20One%20owner%20per%20namespace%2C%20proven%20by%20a%20claim)) and:
  - checks each location hint against the named block's header, which lists its
    chunk hashes ([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout)). Relocation makes hints stale
    ([RFC 9 §4.3](rfc-9-gc.md#4.3%20A%20reader%20can%20hold%20the%20old%20location)); a stale chunk is found by listing the namespace and reading
    headers, and a chunk found nowhere fails the import. When the namespace
    encrypts, header hashes are keyed ([RFC 5 Appendix B](rfc-5-transforms.md#Appendix%20B%20%E2%80%94%20encryption)), so the importer
    compares under the namespace's header key, which it must hold
    ([§4.4](#4.4%20Key%20scope%20and%20material));
  - recomputes every count from the imported refs ([RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)) and ends with
    the audit ([RFC 6 §7.5](rfc-6-block-metadata.md#7.5%20Audit));
  - imports **every other unexpired backup of the namespace** as a detached, held
    snapshot before GC runs, or GC would sweep what only those backups name.

A backup of a namespace that another installation owns and has not released
**MUST NOT** be imported: its owner's GC would sweep what the import counts
([§4.3](#4.3%20GC%20across%20installations%20on%20one%20bucket)).

## 4. Moving a namespace between installations

### 4.1 One owner per namespace, proven by a claim

Each namespace has exactly one owner: only it puts, sweeps, relocates or
collects there, and only its metadata store holds the namespace's records.
Ownership is in the owner's configuration and in a **claim object**: the control
object of role `claim` ([RFC 4 §4.13](rfc-4-remote-tier.md#4.13%20Control%20objects)), put and got by that fixed role
beside the namespace's blocks and never listed as a block, like the health
object ([RFC 4 §4.7](rfc-4-remote-tier.md#4.7%20Health%20is%20one%20probe%20call)). It holds the installation, the claim epoch, and state
`owned` or `released` (with the digest of the move export). Reading it is a step
of the store's capability check ([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)). Installations that share only the bucket check
each other through it:

- a GC pass **MUST** read the claim at its start and before each batch of deletes,
  and **MUST** stop if the claim does not name its installation as `owned`;
- an installation **MUST NOT** open a namespace for writing whose claim names
  another installation as `owned`;
- an import **MUST** take ownership only from a claim that is `released` with the
  digest of the export being imported, or, for a recovery import ([§3.3](#3.3%20Restore)), on
  the operator's explicit statement that the previous owner is gone. It then writes
  the claim `owned`, at the next epoch.

> decision: the claim is a check, not a lock: the remote contract has no
> conditional put, so two installations that both believe they own a namespace
> can both write it. The order of [§4.2](#4.2%20The%20move%2C%20step%20by%20step) prevents that; the claim catches the
> configuration error that breaks the order. Make it a lock if the contract gains
> a conditional write.

### 4.2 The move, step by step

Until replication exists, a migration is a move. From owner A to installation B:

1. **Pre-flight.** A writes the export's header alone, without freezing; B
   checks prefix, key scope and material against it ([§4.4](#4.4%20Key%20scope%20and%20material)).
2. **Freeze and drain at A**, for every share in the namespace: close each
   share's cut gate ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)) and keep it closed, pause removal batches
   ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)), then offload and commit everything dirty, every pin
   included, since B cannot read A's journals. Held operations wait; they are
   not failed. The freeze **MUST** be bounded by `freeze_timeout`, and a drain
   that cannot finish within it aborts the move and reopens the gates. Once the
   move is committed to, held operations are failed as unavailable, and clients
   reconnect to B.
3. **Stop A's writers and GC** for the namespace, and join them: offload loops,
   relocation, sweep, collection ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)). Release A's GC lease.
4. **Export** the namespace, kind `move`: every share, snapshot, live and
   history ref, live and history namespace record, removal, chunk and block
   record in every GC state, and every put intent ([RFC 9 §3.2](rfc-9-gc.md#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded)). A's records are current and
   quiescent, so the move carries them as they are. No writer at B is fenced:
   B mints its own names, and never puts one A minted ([§4.3](#4.3%20GC%20across%20installations%20on%20one%20bucket)).
5. **Release**: A writes the claim `released` with the export's digest.
6. **Import at B** ([§5.2](#5.2%20Import%20is%20staged%20and%20published%20atomically)), then claim `owned`, then start the shares.
7. **Drop at A**: delete the namespace's records from A's metadata store
   **without releasing them**. No refcount is decremented, no GC index key
   is written, nothing is swept. A deletes nothing in the remote store.

Before step 5, A **MAY** abort: restart GC and reopen the gates. After it, A **MUST NOT**
re-claim, reopen or write the namespace unless the operator states that B has not
published the import, which A cannot learn itself. A move keeps share identities
and FileIDs ([§4.5](#4.5%20Versions%20and%20FileIDs%20on%20import)), so clients' handles **SHOULD** stay valid at B.

### 4.3 GC across installations on one bucket

No namespace is ever served by two GC services:

- **Sweep** retires only blocks its own store records at `live` zero
  ([RFC 9 §2.3](rfc-9-gc.md#2.3%20The%20absence%20of%20a%20record%20proves%20nothing)). A's GC is joined before the export and A records nothing after
  step 7; B counts every ref, since every share and snapshot moved.
- **Retired and deleted blocks, and intents,** move too. B rebuilds the GC index
  from the block records ([RFC 9 §7.4](rfc-9-gc.md#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt)) and resumes them as its own trash and
  delete backlog, keeping each `not_before` ([RFC 9 §3.2](rfc-9-gc.md#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded)). A's intents carry A's
  epochs, which the import supersedes ([§4.5](#4.5%20Versions%20and%20FileIDs%20on%20import)), so B collects each with its
  object, if any ([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)).
- **Collection** ([RFC 9 §5.3](rfc-9-gc.md#5.3%20It%20runs%20only%20where%20the%20namespace%20is%20proven)) runs only where claim and configuration agree;
  objects A put and never committed are B's to collect.
- A **stale process of A** that puts after the release only leaks: its commit
  finds no intent, and the object, named by neither an intent nor a record at B,
  is left to the listing backstop. One that deletes can reach no committed block:
  it deletes only a name A's store held neither record nor intent for, a state
  that is final and that the export carried to B unchanged.

A namespace **MUST NOT** be split between installations. A share that shares its
namespace with others moves only with them ([§2.1](#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)).

### 4.4 Key scope and material

A namespace's key scope is fixed when it is created and travels in every export.
B **MUST** use it for the namespace, and **MUST** refuse an import whose scope or
prefix differs from its configuration: names derived under another scope would
never match the imported records ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)).

The export lists every material ID in the census of the blocks it names, each
with its fingerprint, and B **MUST** find each held by its provider, with the
same fingerprint, before staging a record
([RFC 5 §2.7](rfc-5-transforms.md#2.7%20Failures)): material unavailable now refuses the import as retryable; unknown
or destroyed material refuses it naming the IDs. An import **MUST NOT** proceed and
leave reads to fail as corrupt. B needs no particular *current* material: its
own chain writes new blocks ([RFC 5 §2.8](rfc-5-transforms.md#2.8%20The%20chain%20ID)).

The namespace's **data keys**, **header key** and **chunking key** are wrapped
namespace keys ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys), [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)). The export names
each as (material ID, fingerprint) and never carries it. B **MUST** hold the same keys before staging: without the header
key it cannot verify A's block headers, and without the chunking key its new
writes chunk differently from every block A wrote.

### 4.5 Versions and FileIDs on import

**Versions.** A version B's journal assigns to an imported file **MUST** exceed
every version imported for it, or a new write loses precedence to older content
and is dropped at its commit ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)). The header records the highest epoch
in the export, and every import — move, recovery or clone — starts the target
share's owner epoch ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20A%20token%20is%20a%20lease%2C%20fenced%20by%20an%20epoch)) above it before the share serves. Versions
compare epoch first, so this holds whatever B's counters are; journals opened
later include the imported records in their floor ([RFC 6 §8.3](rfc-6-block-metadata.md#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor)). A move
keeps each ref's and each versioned record's `born` and `died`, each snapshot's
cut number and the share's `Cut` record unchanged; they are share-local and
involve no journal version. A clone or restore has no snapshots and no history,
writes every record with `born` 0, and starts at cut number 0.

**FileIDs.** A FileID is unique across a store and keys content in every journal
([RFC 0 §3](rfc-0-data-lifecycle.md#3.%20Identity)):

- a **move** keeps FileIDs and the share's identity, and **MUST** be refused if
  any imported FileID already exists at B;
- a **clone** or **restore** assigns new FileIDs and a new share identity, since
  the same snapshot may be restored many times.

**Handles cannot alias across shares.** Every per-file key carries its share's
identity ([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)), and a handle names the share and the file
([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)). A clone or restore gets a new share identity, so no handle to the
source can resolve into it, and no per-file generation is needed to tell them
apart. An in-place rollback revives the same files, and their old handles
rightly resolve again.

### 4.6 After replication

Once replication and ownership are implemented, two installations **MAY** serve
one namespace under [RFC 10](rfc-10-journal-replication.md) and [RFC 11](rfc-11-ownership.md)'s rules — one metadata store, owners
fenced by epoch, handover ([RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover)) in place of a move. This document does
not specify that; the move of [§4.2](#4.2%20The%20move%2C%20step%20by%20step) remains the way to change which
metadata store holds a namespace.

## 5. The export format

### 5.1 Layout

An export is one stream, written and read front to back, never seeked:

```
header   = magic ‖ format version ‖ kind ‖ namespace id, prefix, key scope ‖
           source installation ‖ share and snapshot ids ‖ cut ‖ highest epoch ‖
           (material ID, fingerprint) of every census material and of the
           namespace's data, header and chunking keys ‖ record counts per
           section ‖ header digest
section* = type ‖ frame* ‖ section digest ‖ record count
frame    = length ‖ records ‖ frame checksum        (frames of at most 4 MiB)
trailer  = end marker ‖ digest over the header and every section digest
```

- **Versioned.** A reader **MUST** refuse an unknown format version and read every
  version it once wrote; each record carries its own version.
- **Self-describing.** The header alone suffices for pre-flight ([§4.2](#4.2%20The%20move%2C%20step%20by%20step)).
- **Verifiable.** Frame checksums find corruption within 4 MiB; section digests
  and the trailer's digest, BLAKE3 like names ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)), cover order and
  completeness. The trailer's digest is the export's identity ([§4.1](#4.1%20One%20owner%20per%20namespace%2C%20proven%20by%20a%20claim)).
- **Streamable.** Sections come in dependency order — namespace, tree, content
  (live and history refs, removals), chunks and blocks, put intents —
  with records in key order, so an importer holds one frame and
  writes as it reads.

Records are the logical records of [RFC 6](rfc-6-block-metadata.md) and [RFC 7](rfc-7-namespace-metadata.md), never a backend's dump,
so an export moves between metadata backends.

**What an export holds.** Everything under the share's prefix and the per-file
prefixes of that share, its history included; for a move, also everything under
the namespace's content-addressed prefixes, which are scoped by namespace
([RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)); plus the server-wide records its files and grants name — the
users and groups whose principals appear in ownership, ACLs, grants and quotas,
with their protocol-ID index rows. **`Secret` records are never exported**: a
user arrives at B without credentials and is given new ones there. Nothing else
server-wide is copied.

**Principals are imported by ID.** A principal is an opaque ID, minted once and
never reissued ([RFC 7 §2.1](rfc-7-namespace-metadata.md#2.1%20File)). An import adopts a principal whose ID B does
not hold, reuses one B holds with an identical record — a principal moving
back — and **MUST** be refused,
naming it, when B holds that ID for someone else or when an imported protocol ID
— a UID, GID or SID — already maps to a different principal at B. It **MUST
NOT** match principals by protocol ID: `u:1000` at A and at B are unrelated
until an operator maps them.

> [!important] Pending review — export scope and principals
> A move exports the namespace-scoped content-addressed records and all history;
> principals import by opaque ID and refuse on collision; secrets stay behind.

### 5.2 Import is staged and published atomically

An export is larger than one transaction. An import therefore writes into a
**staging area** of the target store, under an import identity nothing serves or
reads: no share, no GC service and no dedup lookup sees it. When the trailer
verifies and every check has passed — material, scope, FileIDs, hints, counts
recomputed, audit clean — one transaction **publishes** it: the namespace or
share record becomes active and the staging identity is retired.

A torn, corrupt or refused import publishes nothing; its staging records are
removed by its failure path or, if interrupted, at the next start
([RFC 0 §9.1](rfc-0-data-lifecycle.md#9.1%20Records%20and%20their%20reclamation)). An import is never resumed from a partial staging area. A clone
stages the same way, and publishes only if every conditional adoption held.

**A staged ref into a live namespace is counted.** A clone or restore writes refs
naming chunks its namespace's GC serves, so each staged ref raises its chunk's
count as it is written, or sweep could retire the chunk before the publish. A
failed or interrupted clone or restore drops its staged refs by the batching
pattern of [RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation), decrementing each chunk. A move's or recovery's
namespace has no GC running until it publishes, and its counts are recomputed.

## 6. API surface

### 6.1 Interfaces

Signatures are indicative; the obligations above are normative.

```go
// Snapshots is the per-share surface.
type Snapshots interface {
	Create(ctx context.Context, share ShareID, name string) (SnapshotID, error)
	List(ctx context.Context, share ShareID) ([]SnapshotInfo, error)
	Delete(ctx context.Context, id SnapshotID) error
	Clone(ctx context.Context, id SnapshotID, spec ShareSpec) (ShareID, error)
	SetPolicy(ctx context.Context, share ShareID, p *Policy) error // nil removes it
}

type Backups interface {
	Backup(ctx context.Context, id SnapshotID, loc string, retain time.Duration) (BackupID, error)
	List(ctx context.Context, loc string, ns NamespaceID) ([]BackupInfo, error)
	Expire(ctx context.Context, loc string, id BackupID) error
	// Restore clones, or runs a recovery import if ownerGone (§3.3).
	Restore(ctx context.Context, loc string, id BackupID, spec ShareSpec, ownerGone bool) (ShareID, error)
}

// Migration moves a namespace (§4.2). Export runs steps 2–5 at the owner.
type Migration interface {
	Export(ctx context.Context, ns NamespaceID, w io.Writer) (Digest, error)
	Import(ctx context.Context, r io.Reader) (ImportReport, error)
	Drop(ctx context.Context, ns NamespaceID, imported Digest) error       // step 7
	Abort(ctx context.Context, ns NamespaceID, notImported bool) error // before step 6 only
}

type Policy struct {
	Every          time.Duration
	Keep           map[Interval]int // hourly, daily, weekly, monthly → N
	BackupLocation string           // empty: no backups
	BackupRetain   time.Duration
}
```

Errors are a closed set: `ErrHeld`, `ErrPinBacklog`, `ErrFreezeTimeout` (a move
only), `ErrNotOwner`, `ErrPrincipalCollision`, `ErrMaterialMissing` (retryable when only unavailable),
`ErrScopeMismatch`, `ErrFileIDExists` and `ErrExportCorrupt`.

Block metadata gains what these need ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)): a `Cut` record per share
and `born` on every ref; `Cut` and `DropCut`; the move to history inside every
transaction that replaces a ref with `born < klatest`; a covering lookup at a
cut number over live and history refs; the batched history drop of
[§2.8](#2.8%20Deleting); and staged, publishable imports. Namespace metadata gains the same
for its versioned records: `born` on each, the move to history inside every
transaction that supersedes one with `born < klatest`, reads and listings at a
cut, and the batched history drop. The journal gains pins: a pin mark per cut,
superseded versions kept until offloaded, and their release ([RFC 1](rfc-1-journal.md)). The
offload gains the commit of a pinned superseded version straight to history
([RFC 8](rfc-8-engine.md)).

### 6.2 Configuration

```yaml
snapshots:
  directory: .snapshot        # the browse directory (§2.5)
  pin_bound: 64GiB            # pinned bytes per share before a cut is refused (§2.3)
migration:
  freeze_timeout: 30s         # the move's drain (§4.2)
backups:
  locations:
    offsite: { type: s3, bucket: dfs-backups, prefix: prod/ }
shares:
  photos:
    namespace: photos         # its own, by default (§2.1)
    snapshot_policy:
      every: 1h
      keep: { hourly: 24, daily: 7, weekly: 4, monthly: 12 }
      backup: { location: offsite, retain: 720h }
```

## 7. Invariants

| # | Invariant |
| --- | --- |
| S1 | A snapshot's blocks are kept alive only by the counts of the refs it sees, live and history: no manifest, hold list or extra root. |
| S2 | A snapshot names only content whose existence committed before its cut. Content dirty at the cut is pinned in the journal until offloaded, and a snapshot is complete only when none of its pins remains. |
| S3 | A complete snapshot reads back exactly the share's bytes, attributes, ACLs, xattrs, streams and tree at the cut, whatever the share does after it. |
| S4 | Taking a snapshot writes a constant number of records and holds the cut gate only for in-flight transactions; no write waits on an offload for a snapshot. A cut is refused while the share's pins exceed their bound. |
| S5 | A clone and its source share chunks only; deleting either, or the snapshot, never changes what the others read. |
| S6 | A snapshot held by an unexpired backup cannot be deleted; an expired backup is never restored. |
| S7 | A namespace has one owner. Only the owner puts, sweeps, relocates or collects in it, and only the owner's store holds its records. |
| S8 | A move never releases refs at the old owner and never deletes a remote object on its behalf. |
| S9 | An import publishes all of its records or none, and nothing reads or collects a staged record; a staged ref is counted, and dropped if the import fails. |
| S10 | An import proceeds only with every material ID held and the namespace's scope and prefix configured. |
| S11 | Every version assigned to an imported file exceeds every version imported for it. |
| S12 | A move keeps FileIDs and refuses a collision; a clone or restore assigns new ones. |
| S13 | Counts after an import are recomputed from imported refs, never read from the export. |
| S14 | Every transaction that drops or replaces a ref or a versioned record with `born < klatest` moves it to history in the same transaction, and no chunk's count changes by the move. |
| S15 | Deleting a snapshot drops exactly the history no other live snapshot sees: the refs and versions with `kp ≤ born < k ≤ died < kn`. |
| S17 | Snapshot *k* reads exactly the refs committed before cut *k* and not superseded before it, whatever journals the share's files live in. |
| S16 | A move exports put intents and retired and deleted block records and fences no writer; a delete at either installation reaches no committed block. |
| S18 | An import maps principals by opaque ID, never by protocol ID, refuses a collision, and carries no secret. |

## 8. Test plan and benchmarks

The set-wide rules and tiers are in [the RFC index](rfc-index.md#Test%20tiers).
Every check here runs against a real metadata backend and the remote-tier
emulator; GC runs as in production, not stubbed.

### 8.1 Group A — lost or wrong content

| Invariant | Check |
| --- | --- |
| S1 | Snapshot a share, delete every file, run sweep and collection to completion. Every block the snapshot names survives, and every snapshot file reads back by hash. Repeat with a hold list injected and removed: same result. |
| S2 | Write continuously while a snapshot is taken; each write carries its own sequence number. Every file in the snapshot holds a prefix of the writer's sequence, and nothing the complete snapshot names is journal-only. Overwrite, truncate and release files dirty at the cut before any offload: the snapshot reads back their bytes at the cut. Disable the journal's pin: the check fails on a read. |
| S3 | Model-based: random writes, truncates, deallocates, renames, links, unlinks, releases, `chmod`s, ACL and xattr changes, stream writes and directory-delta folds, racing the cut and each other. At completion, the snapshot equals a model copy taken at the cut, byte for byte, attribute for attribute, and ACL for ACL. |
| S3 | Kill the process at every step between the cut and `complete`, and inside every transaction that moves a record to history; restart. The snapshot completes and equals the model. |
| S3 | Give a file mode 0644 and an ACL denying principal X; snapshot; replace the ACL, then release the file. Browsing the snapshot as X is refused both times. Leave the ACL unversioned: the check fails. |
| S3 | Create in a directory, snapshot before any fold, create more, fold. The snapshot's directory `Modify`, `Change` and `Version` equal the model at the cut, and the live directory's equal the model now. |
| S4 | Make the remote tier unavailable under a sustained writer and take snapshots. Each cut commits within the gate target, writes never stall, and each snapshot stays `pinned`. At the pin bound the next cut is refused with `ErrPinBacklog` and writes proceed. Restore the tier: every snapshot completes. |
| S5 | Clone, then write to both and delete the source, then the snapshot. Each share reads its own bytes; the audit reports no mismatch. |
| S6 | Delete a backed-up snapshot: `ErrHeld`. Crash expiry after each step: a restore never succeeds from an expired backup, and never reads a swept block. |
| S7 | Two installations on one bucket, each owning one namespace, both running GC, relocation and collection for a day-tier run. Neither deletes an object the other's records name. Configure both to own one namespace: GC stops on the claim check. |
| S8 | Move a namespace while A's GC has zero-index blocks, retired blocks in the trash and deleted blocks awaiting their delete. After the drop, no delete is issued by A; B resumes them, and deletes no retired block before its `not_before`. |
| S9 | Corrupt one byte in each frame position, truncate the stream at every frame boundary, kill the importer at each step. Nothing publishes; staging is empty after restart. |
| S10 | Import with one material ID removed from B's provider, and with a different scope: refused before any record is staged. |
| S11 | Import a file whose refs carry a high epoch into an installation whose counter is low; write to it. The write is committed and read back. Revert the epoch raise: the check fails on the read. |
| S12 | Move a namespace to B, back to A, and to B again: FileIDs preserved. Restore one backup twice: two shares, disjoint FileIDs. |
| S13 | Plant wrong counts in an export: the import's counts are correct and the audit is clean. |
| S14 | Model-based: random writes, overwrites, truncates, deallocates, releases and clones racing snapshot creation and deletion. After every transaction, each chunk's count equals its live plus history refs, and every live snapshot reads back its model copy. Remove the move to history from the offload commit: the check fails on a snapshot read. |
| S15 | Take snapshots 1, 2 and 3 over a file overwritten between each; delete 2, then 1, then 3, killing the process between batches. After each, exactly the refs no remaining snapshot sees are gone, the others read back, and the audit is clean. |
| S17 | Place one share's files in two journals, and advance one journal's versions far past the other's. Overwrite files in both between snapshots. Every snapshot reads back its model copy. Replace the cut-number comparison with a comparison of journal `newest` against a cut version: the check fails. |
| S18 | Import an export whose principal ID B holds for another user, and one whose UID B maps to another principal: both refused, naming the principal. Import one whose principals B lacks: adopted by ID, with no secret. |
| S16 | Move a namespace with A's intents outstanding, some with objects and some without. B collects each once its epoch is superseded; a delayed delete from A of a collected name lands after B commits new blocks, and every B block reads back. |
| [§3.3](#3.3%20Restore) | Relocate every block a backup names, delete the owner's metadata, run a recovery import. Every stale hint is resolved from block headers and every file reads back. A second unexpired backup of the namespace survives GC after the import. |

### 8.2 Group B — cost and wedging

| Concern | Counted check |
| --- | --- |
| [§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree) cost to take | Taking a snapshot of a 10^2- and a 10^7-file share writes the same number of records. |
| [§2.4](#2.4%20Capture%20is%20copy%20on%20first%20write) cost to change | After a cut, the first change to a record writes one history version, and a second change to it writes none; with no live snapshot no change writes history. |
| [§2.8](#2.8%20Deleting) deletion | Deleting a snapshot visits only the share's history; it writes no transaction larger than the batching bound. |
| [§5.1](#5.1%20Layout) streaming | Import of a 10^6-file export holds at most one frame plus bounded batches in memory. |
| [§2.7](#2.7%20Scheduled%20snapshots%20and%20retention) retention | A simulated year of hourly ticks under the example policy: never more than 47 policy snapshots, and manual snapshots untouched. |

### 8.3 Benchmarks and targets

Recorded on the reference box ([the RFC index](rfc-index.md#Test%20tiers)); the 10^7-file rows run daily.

| Benchmark | Measures | Target |
| --- | --- | --- |
| Cut under a sustained writer, 64 clients | gate held; p99 write latency across the cut | gate ≤ 50 ms; p99 within 2× of no cut |
| Snapshot of a 10^7-file share | wall time, records written | same as a 10^2-file share; writes no refs and no tree |
| Cut to `complete`, 10 GiB dirty at the cut | seconds | at most the dirty bytes over the measured offload rate, plus 1 s |
| Live write throughput with hourly snapshots, 24 kept | ops/s against none | within 10% |
| Snapshot read of a file overwritten 10^3 times since | p99 against a live read | report, against the ponytail of [§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree) |
| Write latency in the hour after a cut, every record changed once | p99 against no live snapshot | within 20% |
| Export and import of a 10^6-file namespace | records/s | at least half the backend's own scan and batch-write rates |
| Clone of a 10^6-file snapshot | wall time | report, against the ponytail of [§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace) |
| Restore throughput: copy every file of a 10^6-file, 1 TiB snapshot out through the browse path, 64 readers, cold | MB/s and files/s against a live cold read | report, against the ponytail of [§2.5](#2.5%20Browsing%20a%20snapshot); at least half the live cold read |
| Recovery import, all hints stale | header gets per block | one ranged get per block in the namespace |
| Move of a 10^6-file namespace, freeze to B serving | seconds | report; no block transferred |

## 9. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| snapshot outcomes, labelled `result` = `complete`, `pin_backlog`, `failed` | `dittofs_snapshot_total` | counter |
| cut gate held | `dittofs_snapshot_gate_seconds` | histogram |
| pinned bytes not yet offloaded, and time from cut to `complete` | `dittofs_snapshot_pinned_bytes`, `dittofs_snapshot_complete_seconds` | gauge, histogram |
| history refs and history record versions, and those dropped by snapshot deletion | `dittofs_snapshot_history_refs`, `dittofs_snapshot_history_records`, `dittofs_snapshot_history_dropped_total` | gauge, gauge, counter |
| policy ticks, labelled `result` = `taken`, `skipped`, `pruned` | `dittofs_snapshot_policy_total` | counter |
| age of the newest complete backup per share; the alert for a policy that stopped | `dittofs_backup_newest_age_seconds` | gauge |
| export and import records and bytes, labelled `kind` | `dittofs_export_records_total`, `dittofs_import_records_total` | counter |
| import refusals, labelled `reason` = `corrupt`, `material`, `scope`, `fileid`, `owner`, `hint` | `dittofs_import_refused_total` | counter |
| namespace claim state per namespace (1 on `owned` by this installation) | `dittofs_namespace_owned` | gauge |
| GC passes stopped by the claim check; any nonzero value is an alert | `dittofs_gc_claim_refusals_total` | counter |

Logs: each cut, completion, move step and publish logs at `Info` with share,
namespace and snapshot or export digest. A refused cut, a move's freeze timeout,
an import refusal and a claim
mismatch log at `Warn`, naming the missing material, the mismatching scope or the
claiming installation.

## 10. Open questions

1. **Moving one share out of a shared namespace** needs its chunks re-homed, a
   full data copy. Specify it when an operator needs it.
2. **Reading another installation's snapshot without a move** would count blocks
   the owner can sweep; it needs replication ([RFC 11](rfc-11-ownership.md)) or a byte copy.
3. ~~**A paused GC process deleting after its lease.**~~ Closed: a name is put
   by one attempt only, and deleted only once neither a record nor an intent
   names it, a final state, so a late delete reaches no committed block
   ([RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep)).
4. ~~**Snapshots without a drain.**~~ Closed: the journal pins the cut's
   versions ([§2.3](#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)).
5. **Handles across a recovery import.** It assigns new FileIDs, so clients
   remount; keeping them would need proof that no other restore of the namespace
   used them.

---

## Appendix A — where the current code differs

Descriptive, for the refactor.

| # | This document says | The code today |
| --- | --- | --- |
| D1 | Snapshot blocks are held by counted live and history refs (S1) | a manifest file of block hashes on local disk is an extra GC root; a hold list |
| D2 | A snapshot is logical records ([§5.1](#5.1%20Layout)) | a backend-native dump of the whole metadata store in the local store directory |
| D3 | Dirty content at the cut is pinned until offloaded (S2) | a snapshot may be created without the durability check, and restored with a force flag |
| D4 | Restore makes a new share ([§3.3](#3.3%20Restore)) | restore overwrites the share in place, behind a safety snapshot and a restore-in-progress marker |
| D5 | Writable clones ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)) | no clone, and no restore into another share |
| D6 | Browsable snapshots ([§2.5](#2.5%20Browsing%20a%20snapshot)) | snapshots are not visible to clients |
| D7 | Retention keeps N per interval ([§2.7](#2.7%20Scheduled%20snapshots%20and%20retention)) | keep-last and a maximum age |
| D8 | Backups to a location outside the metadata store ([§3](#3.%20Backups)) | none; snapshots cannot be exported |
| D9 | Namespaces move between installations ([§4](#4.%20Moving%20a%20namespace%20between%20installations)) | none; no claim, no export format |
| D10 | A snapshot is one cut record; a superseded namespace record moves to history ([§2.4](#2.4%20Capture%20is%20copy%20on%20first%20write)) | one consistent read view of the whole store, held for the dump |
| D11 | Replaced refs a snapshot sees move to history (S14) | no history; overwritten content is kept only by the manifest |
| D12 | A move exports intents and retired and deleted block records (S16) | none |
