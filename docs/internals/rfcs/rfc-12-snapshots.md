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

- A **snapshot** is a share at one instant: a read-only copy of its tree plus one
  counted ref set per file ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). The count is all that keeps its blocks.
- Every snapshot is taken at a **cut**, a short freeze in which everything written
  before it is offloaded and committed, so it never names journal content.
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
document composes those from what the set has: counted ref sets
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
| **cut** | the instant a snapshot describes ([§2.3](#2.3%20The%20cut%20is%20a%20drained%2C%20frozen%20instant)) |
| **export** | a self-describing stream of metadata records ([§5](#5.%20The%20export%20format)): kind `backup` (one snapshot) or `move` (a whole namespace) |
| **import** | building records from an export, staged and published at once ([§5.2](#5.2%20Import%20is%20staged%20and%20published%20atomically)) |

## 2. Snapshots

### 2.1 A namespace is the unit that moves

A block may be counted by every share of its namespace ([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)), so
whatever changes who counts a block acts on a namespace: a clone ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)) or
restore ([§3.3](#3.3%20Restore)) creates its share **in the namespace that holds the blocks**, and a
migration ([§4](#4.%20Moving%20a%20namespace%20between%20installations)) moves a whole namespace with every share, snapshot and pending
deletion in it. A new share **SHOULD** get a namespace of its own, so that it can
be moved alone; shares configured into one namespace deduplicate against each
other and move together.

### 2.2 A snapshot is counted content and a frozen tree

A snapshot has two parts:

- **the tree**: a copy of the share's inodes and entries at the cut
  ([RFC 7 §2.1](rfc-7-namespace-metadata.md#2.1%20Inode)), keyed by the snapshot. It holds no remote content and is not counted;
- **the content**: per regular file, its shape, holes and one ref set
  ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). A file unchanged since the previous snapshot names the same ref
  set, raising only its holder count.

Nothing else — no manifest, hold list or extra GC root — keeps a snapshot's
blocks alive ([RFC 9 §2.1](rfc-9-gc.md#2.1%20The%20count%20is%20the%20only%20authority), [RFC 7 §4.4](rfc-7-namespace-metadata.md#4.4%20There%20is%20no%20third%20holder)).

> ponytail: the tree is a full copy, O(files + entries) records per snapshot,
> while content is shared through ref sets. Upgrade to layered trees, where a
> snapshot records only what changed since its predecessor, when a scheduled
> snapshot of a large share costs more than its interval ([§8.3](#8.3%20Benchmarks%20and%20targets)).

A snapshot is `capturing`, `complete` or `failed`; only a complete one is
browsed, cloned, backed up or restored. A failed one's records are removed as a
deletion removes them ([§2.8](#2.8%20Deleting)).

### 2.3 The cut is a drained, frozen instant

The journal keeps no superseded version of a byte ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)), so content
dirty at the cut could be overwritten before offload and then exist nowhere. The
cut is therefore taken on a drained share:

1. **Pre-drain.** With writes flowing, offload the share ([RFC 8 §4.2](rfc-8-engine.md#4.2%20Offload%20is%20scheduled%20here)) until
   its dirty bytes fall below the configured freeze bound.
2. **Freeze.** Hold every new mutating operation on the share — at every owner,
   where the share has several units ([RFC 11 §2](rfc-11-ownership.md#2.%20Ownership%20units)) — and let those in flight
   finish. Reads continue. Held operations wait; they are not failed.
3. **Drain.** Offload and commit everything dirty on the share.
4. **Cut.** In one transaction, create the snapshot record in state `capturing`.
   Every later transaction on the share sees it.
5. **Thaw.** Release the held operations.

The freeze **MUST** be bounded by a configured timeout; a drain that cannot finish
within it **MUST** abort the snapshot and thaw, never cut over dirty content. A share with no remote tier carves nothing
([RFC 8 §4.6](rfc-8-engine.md#4.6%20A%20share%20with%20no%20remote%20tier%20never%20reports%20durability)), so a snapshot of it **MUST** be refused.

> decision: every snapshot drains, not only exportable ones. A snapshot that held
> journal content would need the journal to keep superseded versions, which it
> does not. Withdraw this if the journal ever retains versions for a cut.

### 2.4 Capture is copy on first write

Writing a snapshot's records costs time proportional to the share, so it is not
done inside the freeze. After the thaw a walker captures files and directories,
and **every transaction that changes a share record not yet captured MUST first
capture that record's state, in the same transaction**: offload and existence
commits, removals, releases ([RFC 6 §6.4](rfc-6-block-metadata.md#6.4%20Delete)), attribute and entry changes. Nothing
changed a record between the cut and its first later transaction, so what is
captured is exactly the cut, and a file released after the cut is counted by its
ref set before the release decrements its chunks.

A record captured by a write carries a marker the walker skips. Markers are
removed when the snapshot completes or fails ([RFC 0 §9.1](rfc-0-data-lifecycle.md#9.1%20Records%20and%20their%20reclamation)). A restart resumes
the walker; when it finishes, the snapshot becomes `complete` in one transaction.

### 2.5 Browsing a snapshot

A complete snapshot is reachable read-only under a virtual directory at the
share's root, configurable and hidden from listings. What protocols rely on:

- a handle into a snapshot names the share, the snapshot and the inode
  ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20an%20inode%2C%20never%20a%20path)), so it never resolves to a live inode; once the snapshot is
  deleted it resolves stale ([RFC 7 §6.3](rfc-7-namespace-metadata.md#6.3%20Staleness%20is%20reported%2C%20never%20guessed));
- a read resolves through the file's ref set by the covering lookup
  ([RFC 6 §8.1](rfc-6-block-metadata.md#8.1%20Covering%20lookup)) and fetches from the remote tier;
- permissions are evaluated against the captured inode, as it was at the cut;
- every mutating operation fails with the read-only error;
- a snapshot inode's numeric file id differs from the live inode's
  ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20derived%2C%20and%20collisions%20are%20its%20problem)), so tools do not take the two for one file.

> ponytail: snapshot reads bypass the journal; add a fill keyed by the ref set
> when browsing throughput matters.

### 2.6 A writable clone is a new share in the same namespace

A clone creates a new share from a complete snapshot, in the snapshot's
namespace, as one staged import ([§5.2](#5.2%20Import%20is%20staged%20and%20published%20atomically)): the tree is copied with **new FileIDs**
and generations, mapped so that hard links stay links, and each ref set's refs
become the new file's refs, raising each chunk's refcount. That is
[RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)'s restore, conditional on each chunk existing ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence)), and it
cannot fail while the snapshot holds its ref sets.

The clone then shares **chunks and nothing else** with its source. Deleting the
source share, the snapshot or the clone releases only that one's own refs, and
changes nothing the other two read. The clone moves with its namespace ([§2.1](#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)).

> ponytail: a clone copies every ref, O(refs) records. Upgrade to files that
> read through their source's ref set until first written, when clone latency on
> large shares matters.

### 2.7 Scheduled snapshots and retention

A share **MAY** carry one snapshot policy: a schedule, and a retention of *keep N
per interval* over any of `hourly`, `daily`, `weekly` and `monthly`. A policy
snapshot is kept while it is the newest snapshot of one of the N most recent
periods of any interval; otherwise pruning deletes it ([§2.8](#2.8%20Deleting)).

- Pruning deletes only snapshots the policy made, never manual ones.
- A tick that finds a snapshot in progress is skipped, not queued; ticks missed
  while down produce at most one snapshot on start.
- A policy **MAY** name a backup location and retention; each policy snapshot is
  then backed up ([§3](#3.%20Backups)).
- The scheduler runs once per share, wherever the share's control plane runs.

### 2.8 Deleting

Deleting a snapshot **MUST** be refused while an unexpired backup holds it
([§3.2](#3.2%20A%20backup%20holds%20its%20snapshot)); pruning defers such a snapshot until its last backup expires. A
deletion drops each ref set it names ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)), deletes the tree, and removes
the snapshot record last, marked in progress so a restart resumes. Deleting
a share with snapshots **MUST** be refused unless the request deletes or detaches
them; a detached snapshot belongs to the namespace, stays restorable, and is
deleted only explicitly.

## 3. Backups

### 3.1 A backup is an export of one snapshot's metadata

A backup writes an export of kind `backup` ([§5](#5.%20The%20export%20format)) to a configured **backup
location**: a directory or an object-store prefix outside every block namespace
([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)), never inside the metadata store it protects. It carries the
snapshot's tree and content records; the chunk and block records of every chunk
they name, as **location hints** only ([§3.3](#3.3%20Restore)); the material IDs in those blocks'
census ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)); and the namespace's identity, prefix and key scope. Its
state — `writing`, `complete`, `expired` — is kept in the location, listable
without the installation that wrote it.

### 3.2 A backup holds its snapshot

Until a complete backup expires, its snapshot cannot be deleted ([§2.8](#2.8%20Deleting)), so
the snapshot's ref sets keep every block the backup names counted. The backup
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
    headers, and a chunk found nowhere fails the import;
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
Ownership is in the owner's configuration and in a **claim object** at a fixed
location beside the namespace's blocks, like the health object
([RFC 4 §4.7](rfc-4-remote-tier.md#4.7%20Health%20is%20one%20probe%20call)): installation, claim epoch, and state `owned` or `released` (with
the digest of the move export). Installations that share only the bucket check
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
2. **Freeze and drain at A**, as for a cut ([§2.3](#2.3%20The%20cut%20is%20a%20drained%2C%20frozen%20instant)), for every share in the
   namespace. The shares stay frozen: held operations are failed as unavailable
   once the move is committed to, and clients reconnect to B.
3. **Stop A's writers and GC** for the namespace, and join them: offload loops,
   relocation, sweep, collection ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)). Release A's GC lease.
4. **Export** the namespace, kind `move`: every share, snapshot, ref set, chunk,
   block and pending-deletion record ([RFC 9 §3.2](rfc-9-gc.md#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded)). A's records are current
   and quiescent, so the move carries them as they are.
5. **Release**: A writes the claim `released` with the export's digest.
6. **Import at B** ([§5.2](#5.2%20Import%20is%20staged%20and%20published%20atomically)), then claim `owned`, then start the shares.
7. **Drop at A**: delete the namespace's records from A's metadata store
   **without releasing them**. No refcount is decremented, no candidate or pending
   deletion is written, nothing is swept. A deletes nothing in the remote store.

Before step 5, A **MAY** abort: restart GC and thaw. After it, A **MUST NOT**
re-claim, thaw or write the namespace unless the operator states that B has not
published the import, which A cannot learn itself. A move keeps share identities
and FileIDs ([§4.5](#4.5%20Versions%20and%20FileIDs%20on%20import)), so clients' handles **SHOULD** stay valid at B.

### 4.3 GC across installations on one bucket

No namespace is ever served by two GC services:

- **Sweep** retires only blocks its own store records at `live` zero
  ([RFC 9 §2.3](rfc-9-gc.md#2.3%20The%20absence%20of%20a%20record%20proves%20nothing)). A's GC is joined before the export and A records nothing after
  step 7; B counts every ref, since every share and snapshot moved.
- **The deletion fence** ([RFC 9 §3.4](rfc-9-gc.md#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete)) moves too: A's pending deletions are
  imported, so B's writers do not put those names until B's GC has deleted them.
- **Collection** ([RFC 9 §5.3](rfc-9-gc.md#5.3%20It%20runs%20only%20where%20the%20namespace%20is%20proven)) runs only where claim and configuration agree;
  objects A put and never committed are B's to collect.
- A **stale process of A** that puts after the release only leaks: its commit
  finds no records. One that deletes is the hazard [RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace) already has for
  two passes, bounded by step 3's join and the claim check ([§10](#10.%20Open%20questions), question 3).

A namespace **MUST NOT** be split between installations. A share that shares its
namespace with others moves only with them ([§2.1](#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)).

### 4.4 Key scope and material

A namespace's key scope is fixed when it is created and travels in every export.
B **MUST** use it for the namespace, and **MUST** refuse an import whose scope or
prefix differs from its configuration: names derived under another scope would
never match the imported records ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)).

The export lists every material ID in the census of the blocks it names, and B
**MUST** find each held by its provider before staging a record
([RFC 5 §2.7](rfc-5-transforms.md#2.7%20Failures)): material unavailable now refuses the import as retryable; unknown
or destroyed material refuses it naming the IDs. An import **MUST NOT** proceed and
leave reads to fail as corrupt. B needs no particular *current* material: its
own chain writes new blocks ([RFC 5 §2.8](rfc-5-transforms.md#2.8%20The%20chain%20ID)).

### 4.5 Versions and FileIDs on import

**Versions.** A version B's journal assigns to an imported file **MUST** exceed
every version imported for it, or a new write loses precedence to older content
and is dropped at its commit ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)). The header records the highest epoch
in the export, and every import — move, recovery or clone — starts the target
share's owner epoch ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20A%20token%20is%20a%20lease%2C%20fenced%20by%20an%20epoch)) above it before the share serves. Versions
compare epoch first, so this holds whatever B's counters are; journals opened
later include the imported records in their floor ([RFC 6 §8.3](rfc-6-block-metadata.md#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor)).

**FileIDs.** A FileID is unique across a store and keys content in every journal
([RFC 0 §3](rfc-0-data-lifecycle.md#3.%20Identity)):

- a **move** keeps FileIDs, generations and the share's identity, and **MUST** be
  refused if any imported FileID already exists at B;
- a **clone** or **restore** assigns new FileIDs and a new share identity, since
  the same snapshot may be restored many times.

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
           material IDs ‖ record counts per section ‖ header digest
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
- **Streamable.** Sections come in dependency order — namespace, tree, content,
  chunks and blocks, pending deletions — with records in key order, so an importer
  holds one frame and writes as it reads.

Records are the logical records of [RFC 6](rfc-6-block-metadata.md) and [RFC 7](rfc-7-namespace-metadata.md), never a backend's dump,
so an export moves between metadata backends.

### 5.2 Import is staged and published atomically

An export is larger than one transaction. An import therefore writes into a
**staging area** of the target store, under an import identity nothing serves or
counts: no share, no GC service and no dedup lookup sees it. When the trailer
verifies and every check has passed — material, scope, FileIDs, hints, counts
recomputed, audit clean — one transaction **publishes** it: the namespace or
share record becomes active and the staging identity is retired.

A torn, corrupt or refused import publishes nothing; its staging records are
removed by its failure path or, if interrupted, at the next start
([RFC 0 §9.1](rfc-0-data-lifecycle.md#9.1%20Records%20and%20their%20reclamation)). An import is never resumed from a partial staging area. A clone
stages the same way, and publishes only if every conditional adoption held.

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

Errors are a closed set: `ErrHeld`, `ErrNoRemoteTier`, `ErrFreezeTimeout`,
`ErrNotOwner`, `ErrMaterialMissing` (retryable when only unavailable),
`ErrScopeMismatch`, `ErrFileIDExists` and `ErrExportCorrupt`.

Block metadata gains what these need, beside [RFC 6](rfc-6-block-metadata.md)'s `Snapshot` and
`DropSnapshot`: covering lookup over a ref set, capture of a file into a snapshot
inside a caller's transaction, and staged, publishable imports.

### 6.2 Configuration

```yaml
snapshots:
  directory: .snapshot        # the browse directory (§2.5)
  freeze:
    dirty_bound: 256MiB       # pre-drain target before freezing (§2.3)
    timeout: 30s
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
| S1 | A snapshot's blocks are kept alive only by its ref sets' counts: no manifest, hold list or extra root. |
| S2 | A snapshot names only content committed before its cut; no cut is taken over dirty content. |
| S3 | A complete snapshot reads back exactly the share's bytes, attributes and tree at the cut, whatever the share does after it. |
| S4 | A freeze is bounded; one that cannot drain in time aborts the snapshot and thaws. |
| S5 | A clone and its source share chunks only; deleting either, or the snapshot, never changes what the others read. |
| S6 | A snapshot held by an unexpired backup cannot be deleted; an expired backup is never restored. |
| S7 | A namespace has one owner. Only the owner puts, sweeps, relocates or collects in it, and only the owner's store holds its records. |
| S8 | A move never releases refs at the old owner and never deletes a remote object on its behalf. |
| S9 | An import publishes all of its records or none, and nothing reads, counts or collects a staged record. |
| S10 | An import proceeds only with every material ID held and the namespace's scope and prefix configured. |
| S11 | Every version assigned to an imported file exceeds every version imported for it. |
| S12 | A move keeps FileIDs and refuses a collision; a clone or restore assigns new ones. |
| S13 | Counts after an import are recomputed from imported refs, never read from the export. |

## 8. Test plan and benchmarks

The set-wide rules and tiers are in [the RFC index](rfc-index.md#Test%20tiers).
Every check here runs against a real metadata backend and the remote-tier
emulator; GC runs as in production, not stubbed.

### 8.1 Group A — lost or wrong content

| Invariant | Check |
| --- | --- |
| S1 | Snapshot a share, delete every file, run sweep and collection to completion. Every block the snapshot names survives, and every snapshot file reads back by hash. Repeat with a hold list injected and removed: same result. |
| S2 | Write continuously while a snapshot is taken; each write carries its own sequence number. Every file in the snapshot holds a prefix of the writer's sequence, and nothing the snapshot names is journal-only. |
| S3 | Model-based: random writes, truncates, deallocates, renames, links, unlinks and releases racing capture. At completion, the snapshot equals a model copy taken at the cut, byte for byte and attribute for attribute. |
| S3 | Kill the process at every capture step; restart. The snapshot completes and equals the model. |
| S4 | Make the remote tier unavailable during the drain. The snapshot fails within the timeout, held operations proceed, and no snapshot record remains in `capturing`. |
| S5 | Clone, then write to both and delete the source, then the snapshot. Each share reads its own bytes; the audit reports no mismatch. |
| S6 | Delete a backed-up snapshot: `ErrHeld`. Crash expiry after each step: a restore never succeeds from an expired backup, and never reads a swept block. |
| S7 | Two installations on one bucket, each owning one namespace, both running GC, relocation and collection for a day-tier run. Neither deletes an object the other's records name. Configure both to own one namespace: GC stops on the claim check. |
| S8 | Move a namespace while A's GC has candidates and pending deletions. After the drop, no delete is issued by A; B resumes the pending deletions under the fence. |
| S9 | Corrupt one byte in each frame position, truncate the stream at every frame boundary, kill the importer at each step. Nothing publishes; staging is empty after restart. |
| S10 | Import with one material ID removed from B's provider, and with a different scope: refused before any record is staged. |
| S11 | Import a file whose refs carry a high epoch into an installation whose counter is low; write to it. The write is committed and read back. Revert the epoch raise: the check fails on the read. |
| S12 | Move a namespace to B, back to A, and to B again: FileIDs preserved. Restore one backup twice: two shares, disjoint FileIDs. |
| S13 | Plant wrong counts in an export: the import's counts are correct and the audit is clean. |
| [§3.3](#3.3%20Restore) | Relocate every block a backup names, delete the owner's metadata, run a recovery import. Every stale hint is resolved from block headers and every file reads back. A second unexpired backup of the namespace survives GC after the import. |

### 8.2 Group B — cost and wedging

| Concern | Counted check |
| --- | --- |
| [§2.2](#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree) sharing | A second snapshot of an unchanged share writes no ref-set refs: only holder counts and the tree. |
| [§2.4](#2.4%20Capture%20is%20copy%20on%20first%20write) overhead | A write to a captured file during capture performs no capture read beyond one marker lookup. |
| [§5.1](#5.1%20Layout) streaming | Import of a 10^6-file export holds at most one frame plus bounded batches in memory. |
| [§2.7](#2.7%20Scheduled%20snapshots%20and%20retention) retention | A simulated year of hourly ticks under the example policy: never more than 47 policy snapshots, and manual snapshots untouched. |

### 8.3 Benchmarks and targets

Recorded on the reference box ([the RFC index](rfc-index.md#Test%20tiers)); the 10^7-file rows run daily.

| Benchmark | Measures | Target |
| --- | --- | --- |
| Freeze duration under a sustained writer | seconds held | at most `dirty_bound` over the measured offload rate, plus 1 s |
| Snapshot of a 10^6-file share, first and unchanged | wall time, records written | report; the unchanged run writes no refs |
| Write latency during capture | p99 against no capture | within 20% |
| Export and import of a 10^6-file namespace | records/s | at least half the backend's own scan and batch-write rates |
| Clone of a 10^6-file snapshot | wall time | report, against the ponytail of [§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace) |
| Recovery import, all hints stale | header gets per block | one ranged get per block in the namespace |
| Move of a 10^6-file namespace, freeze to B serving | seconds | report; no block transferred |

## 9. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| snapshot outcomes, labelled `result` = `complete`, `freeze_timeout`, `failed` | `dittofs_snapshot_total` | counter |
| freeze duration | `dittofs_snapshot_freeze_seconds` | histogram |
| capture progress: records captured by walker and by write | `dittofs_snapshot_captured_total` | counter |
| policy ticks, labelled `result` = `taken`, `skipped`, `pruned` | `dittofs_snapshot_policy_total` | counter |
| age of the newest complete backup per share; the alert for a policy that stopped | `dittofs_backup_newest_age_seconds` | gauge |
| export and import records and bytes, labelled `kind` | `dittofs_export_records_total`, `dittofs_import_records_total` | counter |
| import refusals, labelled `reason` = `corrupt`, `material`, `scope`, `fileid`, `owner`, `hint` | `dittofs_import_refused_total` | counter |
| namespace claim state per namespace (1 on `owned` by this installation) | `dittofs_namespace_owned` | gauge |
| GC passes stopped by the claim check; any nonzero value is an alert | `dittofs_gc_claim_refusals_total` | counter |

Logs: each cut, move step and publish logs at `Info` with share, namespace and
snapshot or export digest. A freeze timeout, an import refusal and a claim
mismatch log at `Warn`, naming the missing material, the mismatching scope or the
claiming installation.

## 10. Open questions

1. **Moving one share out of a shared namespace** needs its chunks re-homed, a
   full data copy. Specify it when an operator needs it.
2. **Reading another installation's snapshot without a move** would count blocks
   the owner can sweep; it needs replication ([RFC 11](rfc-11-ownership.md)) or a byte copy.
3. **A paused GC process deleting after its lease.** A delete already issued
   cannot be fenced by a transaction; the claim check narrows the window without
   closing it. The same gap exists for two GC processes in one installation
   ([RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace)), and should be closed there.
4. **Snapshots without a drain**, for shares whose backlog makes the freeze long:
   needs the journal to retain the cut's versions ([§2.3](#2.3%20The%20cut%20is%20a%20drained%2C%20frozen%20instant)).
5. **Handles across a recovery import.** It assigns new FileIDs, so clients
   remount; keeping them would need proof that no other restore of the namespace
   used them.

---

## Appendix A — where the current code differs

Descriptive, for the refactor.

| # | This document says | The code today |
| --- | --- | --- |
| D1 | Snapshot blocks are held by counted ref sets (S1) | a manifest file of block hashes on local disk is an extra GC root; a hold list |
| D2 | A snapshot is logical records ([§5.1](#5.1%20Layout)) | a backend-native dump of the whole metadata store in the local store directory |
| D3 | No cut over dirty content (S2) | a snapshot may be created without the durability check, and restored with a force flag |
| D4 | Restore makes a new share ([§3.3](#3.3%20Restore)) | restore overwrites the share in place, behind a safety snapshot and a restore-in-progress marker |
| D5 | Writable clones ([§2.6](#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)) | no clone, and no restore into another share |
| D6 | Browsable snapshots ([§2.5](#2.5%20Browsing%20a%20snapshot)) | snapshots are not visible to clients |
| D7 | Retention keeps N per interval ([§2.7](#2.7%20Scheduled%20snapshots%20and%20retention)) | keep-last and a maximum age |
| D8 | Backups to a location outside the metadata store ([§3](#3.%20Backups)) | none; snapshots cannot be exported |
| D9 | Namespaces move between installations ([§4](#4.%20Moving%20a%20namespace%20between%20installations)) | none; no claim, no export format |
| D10 | Capture runs after a bounded freeze ([§2.4](#2.4%20Capture%20is%20copy%20on%20first%20write)) | one consistent read view of the whole store, held for the dump |
