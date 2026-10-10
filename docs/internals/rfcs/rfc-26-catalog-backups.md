---
rfc: 26
title: "RFC 26 — catalog backups and the export format"
component: backups
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-1-journal]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-3-syncer]]"
  - "[[rfc-4-remote-tier]]"
  - "[[rfc-5-transforms]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-9-gc]]"
  - "[[rfc-12-snapshots]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-16-metadata-store]]"
  - "[[rfc-27-namespace-migration]]"
aliases:
  - RFC 26
tags:
  - rfc
---

# RFC 26 — catalog backups and the export format

**Status:** draft. [§8](#8.%20Open%20questions) lists what is undecided.
**Audience:** anyone implementing catalog or copying backups, restore from one,
or the export format that a backup and a namespace move both write. Snapshots,
which every backup reads, are [RFC 12](rfc-12-snapshots.md); the move is
[RFC 27](rfc-27-namespace-migration.md). Conventions and test tiers are in
[the RFC index](rfc-index.md).

This document specifies behaviour, not the current code. [Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists
where the code differs.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

**What this is.** A backup copies what one snapshot sees to a place outside the
installation's metadata store, so the files can be brought back when that store
is lost. A **catalog backup** copies the metadata only: the tree, and the refs
naming chunks already in the bucket. A **copying backup** also copies the blocks
those refs name, into a folder of its own at a location outside the namespace's
bucket, so it survives losing the bucket too. Both are written in one format,
the **export**, which a namespace move ([RFC 27](rfc-27-namespace-migration.md))
writes as well; this RFC owns it.

**The problem, with one example.** Installation A holds namespace `ns-photos`
and its share `photos`; its blocks are in the bucket `dfs-data`, and snapshot 12
of `photos` is `complete` ([RFC 12](rfc-12-snapshots.md)).

1. **The backup.** A writes a catalog backup of snapshot 12 to the location
   `same-bucket`, a folder beside the namespaces. A **use record** on the
   snapshot keeps it from being deleted while the backup lives, so every block
   the backup names stays counted. The export is sealed under the namespace's
   export key, and marked `complete` in the location.
2. **The loss.** A's metadata store is destroyed. The bucket is intact, but A
   serves nothing.
3. **The restore.** On installation B the operator restores the backup, stating
   that A is gone. B takes the namespace's claim, waits until a partitioned A
   would have stopped deleting, and imports the export into a **staging area**:
   each location hint is checked against the block it names, every count is
   recomputed from the imported refs, and the audit runs. Then one transaction
   publishes the new share `photos-restored`. No block is copied: the refs name
   chunks still in `dfs-data`.
4. **Had the bucket been lost too**, a catalog backup could not help. A copying
   backup of snapshot 12 to `vault`, another bucket, would have copied the blocks
   into its block folder there, and the restore would re-encode them into a new
   namespace.

```text
 snapshot 12 ──use record──► backup ──► location: export (sealed metadata)
                                │
                                └── copying backup only ──► location: block folder
                                                            (blocks its base lacks)

 restore: export ──► staged import ──► one publish ──► new share
          blocks read from the namespace's bucket, or from the block folder if it is gone
```

**The words you need.**

- **Export**, **import** — a sealed, self-describing stream of metadata
  records, and building records from one, staged and published at once
  ([§3](#3.%20The%20export%20format)).
- **Catalog backup / copying backup** — an export of one snapshot's metadata
  to a location outside the metadata store / the same plus a copy of every block
  it names, outside the share's bucket ([§2](#2.%20Catalog%20backups), [§2.4](#2.4%20Copying%20backups)).
- **Use record** — a mark on a snapshot while a backup reads it; the snapshot
  cannot be deleted meanwhile ([§2.2](#2.2%20A%20backup%20holds%20its%20snapshot)).
- **Location** — where backups are written, `mutable` or `immutable`
  ([§2](#2.%20Catalog%20backups)).
- **Recovery import** — a restore from a backup alone, when the metadata store
  holding the namespace is lost ([§2.3](#2.3%20Restore)).

**What this RFC promises.**

- A catalog backup survives losing the metadata store but not the bucket; a
  copying backup survives losing the bucket too, and at an immutable location
  survives a stolen installation credential as well. No backup survives losing
  the master key that wraps its namespace's export key — every namespace has
  one, encrypting or not — so that master key lives off the host.
- Every export is sealed and authenticated, and an import publishes all of its
  records or none.
- A restore always makes a new share; from a copying backup, in a new namespace.

**How the rest is organised.** §1 lists what backups are for and what they are
not. §2 covers catalog backups and restore; §2.4 on copying backups is long and
self-contained. §3 is the export format and how an import is staged. §4 is the
API and configuration, §5 the invariants, §6–§7 tests and metrics, §8 the open
questions.

## In short

- **A catalog backup** copies one snapshot's metadata out of the metadata store.
  It protects against losing the metadata store. It does not protect against
  losing the bucket, because it holds no data.
- **A copying backup** is a catalog backup plus a copy of the blocks it names, in
  a location outside the namespace's bucket. Each copy puts only the blocks its
  previous one does not already hold, and it survives losing the bucket.
- **A backup holds its snapshot** with a use record, so the refs that snapshot
  sees keep every block the backup names counted; it adds no second liveness
  mechanism.
- **A restore** makes a new share: a clone when the snapshot still exists, a
  recovery import when the metadata store is lost.
- **The export** is the one format every backup and every namespace move
  writes: logical records, sealed under the namespace's export key, read front to
  back, and imported into a staging area that is published at once.

## 1. Purpose

Operators ask a file service's data protection to **survive losing the metadata
store.** The bucket is a replicated, durable service, but the metadata store is
software the operator runs, and a bad upgrade or a lost cluster can destroy it.
Metadata is a small fraction of the data, so copying it elsewhere is cheap; a
catalog backup ([§2](#2.%20Catalog%20backups)) is that copy, and restoring it brings
the files back from the blocks already in the bucket. When the bucket itself must
not be a single point of loss, a copying backup ([§2.4](#2.4%20Copying%20backups))
also copies the blocks to a location of its own.

A backup reads a snapshot ([RFC 12](rfc-12-snapshots.md)) and is composed from
what the set already has: counted history refs
([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)) and restore by
adoption ([RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)). The export format
([§3](#3.%20The%20export%20format)) is specified here because a backup is its
first writer; a namespace move ([RFC 27](rfc-27-namespace-migration.md)) writes
the same format, with kinds of its own.

### 1.1 Non-goals

This document **MUST NOT** be read as specifying:

- a replica of the block store kept in step with it: a copying backup
  ([§2.4](#2.4%20Copying%20backups)) copies one snapshot's blocks when it runs. A namespace's own bucket
  runs with versioning and object lock off, which a namespace store requires
  ([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)), and so with no service-side replication that depends on
  versioning; this set's protection against losing that bucket, or against a
  holder of its credential, is a copying backup at an immutable location
  ([§2](#2.%20Catalog%20backups));
- a backup of master keys: an export carries the namespace's own keys only
  wrapped under a master key, and never a master key
  ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys));
- restoring in place: a restore makes a new share ([§2.3](#2.3%20Restore)), and swapping it
  in is a rename in the control plane.

### 1.2 Terms

Share, namespace, installation, shard, journal, ref, chunk, block, cut and
snapshot are defined once, in [RFC 0's glossary](rfc-0-data-lifecycle.md#Glossary);
cut number, history, use record and detached snapshot are
[RFC 12 §1.2](rfc-12-snapshots.md#1.2%20Terms)'s; claim and re-home are
[RFC 27 §1.2](rfc-27-namespace-migration.md#1.2%20Terms)'s. This document adds:

| Term | Means |
| --- | --- |
| **export**, **import** | a self-describing stream of metadata records ([§3](#3.%20The%20export%20format)), and building records from one, staged and published at once |
| **copying backup** | a catalog backup that also copies the blocks it names into a **block folder** at its location; its **manifest** lists the folder blocks it needs ([§2.4](#2.4%20Copying%20backups)) |
| **sweep** | the deletion, at a block folder, of every block no retained manifest lists ([§2.4.4](#2.4.4%20Expiry%20and%20the%20sweep)) |
| **location record** | a backup location's configuration: where it is, its **mode** (`mutable` or `immutable`), its credential reference and its lifecycle age ([§4.2](#4.2%20Configuration)) |

## 2. Catalog backups

A **catalog backup** is a copy of one snapshot's metadata outside the metadata
store: the tree and the refs, and where the blocks they name are. It holds no
data. It survives the loss of the metadata store, and nothing else: it does not
survive the loss of the bucket or of the namespace's folder, the destruction of
key material, or a bucket-level attack. Use a copying backup ([§2.4](#2.4%20Copying%20backups)), which also
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
names, in its clear part ([§3.1](#3.1%20Layout)), the ID of every master key that wraps a
key record it carries, and the backup's state object repeats them. The material
provider **MUST** refuse to destroy a master key that any retained export of any
namespace names — a backup not `expired`, at any configured location — with
`ErrMaterialInUse`, naming the backups. Rotating a master key re-wraps the key
records in the metadata store only ([RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)); every retained
export keeps the old wrapping, so the old key is destroyed only once the last
export naming it has expired, and immutable backups are not made unrestorable by
the usual compliance rotation.

**A backup location has a mode**, stated in its location record
([§4.2](#4.2%20Configuration)) and enforced by the store that opens it
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
  by the lifecycle rule once its lock has ended ([§2.4.4](#2.4.4%20Expiry%20and%20the%20sweep)).

An immutable location's bucket holds no namespace's blocks, since a namespace's
own store refuses versioning and locks ([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)); turning either on for the
namespace's bucket is refused, so it cannot stand in for a backup.

### 2.1 A backup is an export of one snapshot's metadata

A backup writes an export of kind `backup` ([§3](#3.%20The%20export%20format)) to a configured **backup
location**: a directory, or a folder in a bucket — the block store's bucket
included — outside every namespace's prefix
([RFC 4 §4.2](rfc-4-remote-tier.md#4.2%20Names%20in%2C%20locations%20kept%20inside)), never inside the metadata store it protects. It
carries:

- the records the snapshot sees — files with their FileData fields, entries,
  ACLs, xattrs, stream links and holes, with directory deltas folded in — as plain
  records, and per file the refs the snapshot sees, as plain refs;
- the chunk and block records of every chunk they name, as **location hints**
  only ([§2.3](#2.3%20Restore));
- the material in those blocks' census, each as (material ID, fingerprint)
  ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)), and the namespace's key records, each held as
  [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)'s table says for its kind — wrapped under a master key,
  never unwrapped, or in the clear for a non-encrypting namespace's chunk-ID key
  — with the IDs of the master keys that wrap them ([§2](#2.%20Catalog%20backups), [RFC 27 §2.4](rfc-27-namespace-migration.md#2.4%20Key%20scope%20and%20material));
- the namespace's identity, prefix and key scope, and every other bound setting
  of the namespace ([§3.1](#3.1%20Layout));
- per share, its fold rule and the hash key its entries' digests are keyed under
  ([RFC 7 §3.4](rfc-7-namespace-metadata.md#3.4%20Enumeration)), both fixed when the share was created. Every entry key and
  every directory position is derived from them, so an import that drew its own
  would find no entry by lookup and resume no listing where a client's cookie
  points.

Like every export, it is sealed and authenticated under the namespace's export
key ([§3.1](#3.1%20Layout)): in an encrypting namespace, a reader of the location who
lacks the master key learns neither the tree nor the chunk IDs, and cannot alter
a byte unnoticed.

Its state — `writing`, `complete`, `failed`, `expired` — is kept in the location,
listable without the installation that wrote it.

### 2.2 A backup holds its snapshot

A **use record** in the metadata store marks a snapshot as being read by a clone,
a restore, a backup or a move. It is written in the transaction that starts the
reader, which requires the snapshot to be `complete` and conflicts with a
deletion's first transaction on the snapshot record. For a clone or restore it is
deleted at publish or on failure; for a backup, when the backup expires; for a
copying backup, when it completes, since it then holds its own blocks
([§2.4](#2.4%20Copying%20backups)); for a move, when B has published or A has aborted ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)). A deletion checks
the use record, not the backup location, so an unreachable location makes deletion
fail closed.

So while a backup is being written, and until it expires, its snapshot cannot be
deleted ([RFC 12 §2.8](rfc-12-snapshots.md#2.8%20Deleting)), and the refs the snapshot sees keep every block the backup
names counted. The backup adds no second liveness mechanism
([RFC 9 §2.1](rfc-9-gc.md#2.1%20References%20are%20the%20only%20authority)).

Expiry marks the backup `expired` in its location, then deletes the use record,
then removes the export. An expired backup **MUST NOT** be restored; the reverse
order lets a crash leave a restorable backup whose blocks were swept.

**An abandoned reader releases its snapshot.** A use record whose reader is still
running — a backup `writing`, a clone or restore staging — carries a deadline its
reader renews while it makes progress. The snapshot service **MUST** treat a use
record past its deadline as abandoned: it marks the backup `failed` in its
location if it is still `writing` — never one already `complete` — or drops the clone's or restore's staging ([§3.2](#3.2%20Import%20is%20staged%20and%20published%20atomically)), and only then
deletes the use record, in the same order and for the same reason as expiry. A
`failed` backup **MUST NOT** be restored. A crashed reader thus never leaves its
snapshot undeletable. A move's use record ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)) has no deadline: the move
itself deletes it, at B or on abort.

### 2.3 Restore

Restore creates a new share. There are two sources:

- **From a snapshot**, or from a backup whose snapshot still exists: a clone
  ([RFC 12 §2.6](rfc-12-snapshots.md#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)), whose records are current.
- **From a backup alone**, when the metadata store holding the namespace is lost: a
  **recovery import**, which takes the namespace's claim ([RFC 27 §2.1](rfc-27-namespace-migration.md#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)) on the
  operator's statement that the installation holding it is gone, and then:
  - waits out the old installation's claim checks ([RFC 27 §2.1](rfc-27-namespace-migration.md#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)) before it
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
    ([RFC 27 §2.4](rfc-27-namespace-migration.md#2.4%20Key%20scope%20and%20material));
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
    import naming the IDs, as [RFC 27 §2.4](rfc-27-namespace-migration.md#2.4%20Key%20scope%20and%20material) requires;
  - recomputes every count from the imported refs ([RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)) and ends
    with the audit ([RFC 6 §7.5](rfc-6-block-metadata.md#7.5%20Audit)); a retired block the backup records whose
    `not_before` has passed is moved to deleted before anything is served, since
    the old installation may already have deleted it ([RFC 9 §3.2](rfc-9-gc.md#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded));
  - imports **every other unexpired backup of the namespace** found in every
    configured backup location as a detached, held snapshot ([RFC 12 §2.8](rfc-12-snapshots.md#2.8%20Deleting)), each
    under a share identity and FileIDs of its own, before GC runs, or GC would
    sweep what only those backups name. A complete copying backup ([§2.4](#2.4%20Copying%20backups)) is
    exempt: it holds its own blocks, and the namespace's GC can sweep nothing it
    needs;
  - **publishes with the namespace's GC pause record written** ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)), and
    leaves it until the operator states that every share of the namespace has
    been recovered or given up. A namespace can hold shares with no backup at
    all, and clones whose refs count chunks in blocks the recovered shares also
    name; their refs are in no export, so the recomputed counts are too low for
    the blocks they share, and the unrecorded objects of a listing include their
    blocks. While paused, the namespace deletes, relocates and collects
    nothing: an import of one share never deletes a block
    another share needs. The pause costs the space those deletes would have
    freed, and is reported ([RFC 27 §6](rfc-27-namespace-migration.md#6.%20Observability), `move_gc_paused`).

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
GC would sweep what the import counts ([RFC 27 §2.3](rfc-27-namespace-migration.md#2.3%20GC%20across%20installations%20on%20one%20bucket)).

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

### 2.4 Copying backups

A **copying backup** is a catalog backup ([§2.1](#2.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)) that also copies every block
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

A copying backup is also a catalog backup. A recovery import ([§2.3](#2.3%20Restore)) from it
resolves its location hints in the namespace, like any backup's, when the bucket
survived and only the metadata store was lost. The copied blocks are for when the
namespace's folder is gone. Restoring from them always makes a new namespace
([§2.4.5](#2.4.5%20Restore%20into%20a%20new%20namespace)).

#### 2.4.1 Layout at the location

```
<location>/
  control/health                           the location's health object, one per location
  exports/<namespace>/<backup>/export      the export
  exports/<namespace>/<backup>/state       its state object
  progress/<namespace>/<backup>/<batch>    blocks a running copy has stored
  keys/<namespace>/keys                    the namespace's key records (`ObjKeys`, RFC 4)
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
  leaving namespace's blocks its copies name ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)). Each name is put only by copies of that one block, always with the same
  bytes.
- **One health object per location**, not per folder. It is written when the
  location record is created ([RFC 13 §2.5](rfc-13-configuration.md#2.5%20A%20backup%20location%20is%20its%20own%20record)), before any namespace copies
  there, and the version it was stored at is kept in the location record, so the
  first namespace to use an immutable location finds it and opens. A credential
  rotation proves the new credential reaches the same location by reading that
  recorded version (immutable) or a nonce stored in the object (mutable), not by
  any namespace's claim.
- **Each backup's state object** holds `writing`, `complete`, `damaged`, `failed`
  or `expired`, as [§2.1](#2.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata) keeps it, and can be listed without the installation.
- **The folder record** `NS‖ns‖bk‖location`, in the namespace's metadata, names the
  copy or sweep running at the folder, with a deadline its holder renews and a
  **lease number** raised at every take. It keeps the folder's **census**: every
  (material ID, fingerprint) in the census of a block that some retained
  manifest lists. Being under the namespace's prefix, it moves with a migration
  ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)).
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
  cover — the ceiling the claim's decision names ([RFC 27 §2.1](rfc-27-namespace-migration.md#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)).

#### 2.4.2 What is copied

The export carries everything [§2.1](#2.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata) lists, with two differences:

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
([§3.1](#3.1%20Layout)). For each chunk:

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
  ([§2.4.3](#2.4.3%20Writing%20one%2C%20step%20by%20step)).
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
> refs whose change sequence is above the base snapshot's ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)) when
> planning time shows beside the copy's transfer time.

#### 2.4.3 Writing one, step by step

Copies and sweeps of one folder run one at a time, from the installation whose
claim names it `owned` for the namespace ([RFC 27 §2.1](rfc-27-namespace-migration.md#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)). The folder record is what
serialises them.

1. **Start.** One transaction requires the snapshot `complete`. It writes a use
   record of kind `copy`, with a deadline ([§2.2](#2.2%20A%20backup%20holds%20its%20snapshot)). It takes the folder record,
   which must be free or past its deadline. The holder runs the folder store's
   `Recheck` ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) and refuses to start on any drift it reports. The
   copier then writes the backup's state object `writing` at the location.
2. **Plan**, as [§2.4.2](#2.4.2%20What%20is%20copied) describes.
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
   ([§2.4.4](#2.4.4%20Expiry%20and%20the%20sweep)). It then marks the state object `complete`. Then one
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
- **Past the deadline**, the backup is abandoned ([§2.2](#2.2%20A%20backup%20holds%20its%20snapshot)): the state object is
  marked `failed` if it is still `writing`, then the folder record is released,
  then the use record is deleted. Its progress objects stay: the share's next
  copy takes the blocks they list as base ([§2.4.2](#2.4.2%20What%20is%20copied)), and the sweep keeps them
  until a copy of the share completes, then collects every block of the failed
  copies that no retained manifest lists.
- **`max_copy_time` bounds one attempt**, from its start, not the whole chain of
  attempts that a large first copy takes. A copy still running at
  `max_copy_time` is marked `failed` as above, and its successor resumes from
  its progress at the next policy tick.
- **A location that stays unreachable, or a namespace bucket that is lost
  mid-copy,** ends in the same `failed`. Earlier backups are untouched.

#### 2.4.4 Expiry and the sweep

The folder has no metadata store behind it: the manifests are what say which
blocks are in use. Expiry follows [§2.2](#2.2%20A%20backup%20holds%20its%20snapshot)'s order. First the state object
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
   ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)), so no backup names a folder of another namespace. The mark set is the union of the
   manifests of every `complete` and `damaged` backup, plus the progress objects
   of every backup still `writing`, and of every `failed` one whose share has
   completed no copy at the location since (its blocks are a successor's base,
   [§2.4.2](#2.4.2%20What%20is%20copied)).
   An export or state object that cannot be read or verified **MUST** stop the
   sweep with no delete. An incomplete mark set would delete a live backup's
   blocks.
2. **Sweep.** List the folder ([RFC 4 §4.6](rfc-4-remote-tier.md#4.6%20List%20is%20a%20complete%2C%20resumable%20walk)) and delete, in batches
   ([RFC 4 §4.5](rfc-4-remote-tier.md#4.5%20Delete%20is%20batched%20and%20idempotent)), every block in no mark set. Before each batch, read the claim
   under the same fence GC uses ([RFC 27 §2.1](rfc-27-namespace-migration.md#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)).
3. **Settle.** Recompute the folder record's census from the mark set. Remove the
   exports and progress objects of `expired` backups, and those of a `failed`
   backup only once its share has completed a copy at the location since: until
   then they are a successor's base ([§2.4.2](#2.4.2%20What%20is%20copied)), and removing them would
   restart a first copy too large for one attempt from zero.

Serialisation makes the mark exact. No copy runs while the sweep holds the folder
record, so no block is added or reused between the mark and the delete. The one
writer is the claim holder, so no other installation puts into the folder.

> decision: expiry does not wait for a restore that is reading the backup. A
> restore that loses its blocks fails at the next read, and publishes nothing
> ([§3.2](#3.2%20Import%20is%20staged%20and%20published%20atomically)). Expiry is the operator's retention choice, and the rule above
> never takes the last good backup. Add a restore mark that the sweep honours if
> retention ever races restores in practice.

**At an immutable location** ([§2](#2.%20Catalog%20backups), [RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) DittoFS deletes
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
  *T* + *R*. At step 5 ([§2.4.3](#2.4.3%20Writing%20one%2C%20step%20by%20step)), before the state object is marked `complete`,
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
  as elsewhere ([§2.4.2](#2.4.2%20What%20is%20copied)): a block the base holds is not put again, and
  the new manifest names the base's version, whose retention step 5 extends. No
  full copy is needed, and no block is put twice.
- **The lifecycle age bounds the copy, and the policy is checked against it.**
  The location record's lifecycle age **MUST** be at least the longest retention
  of any policy writing there plus `backups.max_copy_time`, and the bucket's
  current-version rule no younger, as its open checks
  ([RFC 4 §4.14](rfc-4-remote-tier.md#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)). An attempt still running at `max_copy_time` is failed, and its
  successor builds on it ([§2.4.3](#2.4.3%20Writing%20one%2C%20step%20by%20step)). A policy whose period is shorter than its
  estimated **incremental** copy — the bytes the share wrote over its last
  period, from its usage counters, over `backups.copy_rate` — **MUST** be refused
  when set, and its next tick skipped, with `ErrPolicyPeriod`: its increments
  could never keep up, and the location would only fill. The first copy is not
  held to the period, since it completes across attempts.
- **Expiry** marks the state object `expired` (a new version of it) and then
  deletes the use record, as [§2.2](#2.2%20A%20backup%20holds%20its%20snapshot) orders, so an expired backup is never
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

#### 2.4.5 Restore into a new namespace

A copying backup restores into a **new namespace**, on any installation that holds
a master key wrapping the source namespace's keys, which the export carries
([§2.1](#2.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)).
The source namespace need not exist. So restore works when the bucket is gone, and
for a copy to another site.

1. **Check.** Read the header. Every material ID must be held with its
   fingerprint ([RFC 27 §2.4](rfc-27-namespace-migration.md#2.4%20Key%20scope%20and%20material)), or the restore is refused with `ErrMaterialMissing`.
   The state, its version of highest authenticated sequence ([§2.4.4](#2.4.4%20Expiry%20and%20the%20sweep)), must be
   `complete`: `damaged`, `failed` and `expired` are refused.
2. **Create.** Create the target namespace with a new ID, a prefix of its own, its
   own key scope and keys, and its claim written `owned`. It has no previous holder,
   so there is no fence to wait out.
3. **Re-put the content, staged** ([§3.2](#3.2%20Import%20is%20staged%20and%20published%20atomically)). For each manifest block, read from the
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
   `born` 0, as a clone does ([RFC 27 §2.5](rfc-27-namespace-migration.md#2.5%20Versions%20and%20FileIDs%20on%20import)). Refs name chunk IDs
   ([RFC 6 §2.5](rfc-6-block-metadata.md#2.5%20Refs%20name%20hashes%2C%20never%20blocks)); each is rewritten to the target ID step 3 recorded and adopts
   the chunk re-put there ([RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)). Recompute the counts, run the audit,
   map principals by ID ([§3.1](#3.1%20Layout)), raise the file-number allocator above the
   highest `Number` ([RFC 27 §2.5](rfc-27-namespace-migration.md#2.5%20Versions%20and%20FileIDs%20on%20import)), then publish.

A chunk missing from the folder, or failing its hash, fails the restore, and
nothing publishes. `Verify` finds that damage before a restore needs the backup.

> decision: restore always re-encodes into a new namespace. A folder block's name
> is derived under the source namespace's ID ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)), so it cannot verify
> in another namespace. Restoring under the old ID would be a recovery import that
> races any holder that still has it. The cost is a decode and an encode of every
> restored byte, and new writes chunk under the new namespace's chunking key, so
> they deduplicate less against restored content. Add an as-is copy back under the
> old namespace ID, behind the recovery statement of [§2.3](#2.3%20Restore), if restore time
> after a lost bucket becomes the complaint.

#### 2.4.6 Cost and pacing

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

## 3. The export format

### 3.1 Layout

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
section* = type (1 byte, RFC 5 Appendix B.1) ‖ frame* ‖ section MAC ‖ record count
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
([§2.4.2](#2.4.2%20What%20is%20copied)), after chunks and blocks, and its chunk records name folder
blocks.

- **Versioned.** A reader **MUST** refuse an unknown format version and read every
  version it once wrote; each record carries its own version.
- **Self-describing.** The clear part and the header alone suffice for
  pre-flight ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)), to any installation holding the master key.
- **Verifiable.** Each frame's AEAD finds corruption within 4 MiB; section MACs
  and the trailer's MAC cover order and completeness. The trailer's MAC is the
  export's identity, its **digest**; a move's claim names the digest over its
  base's and its delta's ([RFC 27 §2.1](rfc-27-namespace-migration.md#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)).
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

### 3.2 Import is staged and published atomically

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

## 4. API surface

### 4.1 Interfaces

Signatures are indicative; the obligations above are normative.

```go
type Backups interface {
	Backup(ctx context.Context, id SnapshotID, loc string, retain time.Duration) (BackupID, error)
	// Copy writes a copying backup, incremental against its base (§2.4).
	Copy(ctx context.Context, id SnapshotID, loc string, retain time.Duration) (BackupID, error)
	Verify(ctx context.Context, loc string, id BackupID) (VerifyReport, error) // marks it damaged on failure
	List(ctx context.Context, loc string, ns NamespaceID) ([]BackupInfo, error)
	Expire(ctx context.Context, loc string, id BackupID) error // then sweeps a copying backup's folder
	// Restore clones, or runs a recovery import if holderGone (§2.3). For a
	// copying backup, spec.Namespace names the new namespace it re-encodes into
	// (§2.4.5), and holderGone is ignored.
	Restore(ctx context.Context, loc string, id BackupID, spec ShareSpec, holderGone bool) (ShareID, error)
}
```

A snapshot policy names a backup location, kind, retention and verify period
([RFC 12 §3.1](rfc-12-snapshots.md#3.1%20Interfaces), `Policy`).

Errors join RFC 12's closed set: `ErrPrincipalCollision`, `ErrMaterialMissing`
(retryable when only unavailable), `ErrScopeMismatch`, `ErrExportCorrupt`,
`ErrLocation`, `ErrBackupDamaged`, `ErrPolicyPeriod` and `ErrNetgroupCollision`.
`ErrScopeMismatch` covers every bound setting
([RFC 27 §2.4](rfc-27-namespace-migration.md#2.4%20Key%20scope%20and%20material)).

What other components gain:

- **Copying backups** ([§2.4](#2.4%20Copying%20backups)): the folder record with its census; material
  removal checking every folder census ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)); a use record of kind
  `copy`; the block folder opened as a remote store of its own.
- **The journal at import** ([RFC 1 §9.1](rfc-1-journal.md#9.1%20Rebuilding)): a raise of its version counter above a
  share's version floor before it serves a share it did not open with.

### 4.2 Configuration

Backup locations and the copy settings are control-plane records, set through
the API ([RFC 13 §2.1](rfc-13-configuration.md#2.1%20The%20control%20plane%20is%20the%20source)) like every other record, with the scope and
class [RFC 13 Appendix B](rfc-13-configuration.md#Appendix%20B%20%E2%80%94%20the%20settings) gives each: `backups.copy_rate`
and `backups.max_copy_time` per installation; each backup location its own
installation-scoped record ([RFC 13 §2.5](rfc-13-configuration.md#2.5%20A%20backup%20location%20is%20its%20own%20record)), which a snapshot policy names
([RFC 12 §3.2](rfc-12-snapshots.md#3.2%20Configuration)). The shape below
is how a provisioning file declares them
([RFC 13 §2.4](rfc-13-configuration.md#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file)).

```yaml
backups:
  copy_rate: 200MiB/s           # copying backups' transfer rate, per installation (§2.4.6)
  max_copy_time: 48h            # one copy attempt still running then is failed; its successor builds on it (§2.4.3)
  locations:                    # each a location record (§2)
    same-bucket:                # beside the namespaces: survives losing the metadata store only
      { type: s3, bucket: dfs-data, prefix: backups/, mode: mutable, credential: dfs-data-rw }
    vault:                      # another bucket, locked: may hold copying backups
      { type: s3, bucket: dfs-vault, prefix: backups/, mode: immutable,
        credential: dfs-vault-put-only, lifecycle_age: 2208h,       # ≥ longest retain + max_copy_time
        generation: 168h,                                           # extension generation G (§2.4.4)
        retention_cap: 2376h }      # ≥ longest retain + G + max_copy_time; enforced by bucket policy
```

## 5. Invariants

Invariant numbers are shared by RFC 12, RFC 26 and RFC 27, and each invariant is
stated in the one that owns its subject; the numbers missing here are theirs.

| # | Invariant |
| --- | --- |
| S14 | An import publishes all of its records or none; a staged ref is counted and indexed, is never served or found by dedup, and is dropped if the import fails. |
| S15 | An import proceeds only with every material ID held and the namespace's scope and prefix configured. |
| S16 | Every version assigned to an imported file exceeds every version imported for it: a journal raises its counter above a share's version floor before it serves a share it did not serve when it opened. Every file number issued after an import exceeds every number imported. |
| S18 | Counts after an import are recomputed from imported refs, never read from the export. |
| S19 | An import maps principals by opaque ID, never by protocol ID, refuses a collision of principal ID, protocol ID, principal name, share name, share path or netgroup, and carries no secret. |
| S23 | Every chunk a copying backup's export names lies in a folder block its manifest lists, and that block is byte for byte the namespace's block of that name. A copy reuses only its base's folder blocks, and never one whose census names material being retired. |
| S24 | At an immutable location DittoFS deletes nothing, every object a backup names is read by its recorded version, and each is locked until at least the backup's completion plus its retention ([§2.4.4](#2.4.4%20Expiry%20and%20the%20sweep)). Elsewhere, a folder block is deleted only by a sweep that runs alone at its folder, from the namespace's claim holder, and finds the block in no manifest of a `complete` or `damaged` backup and in no progress of a `writing` one. Any unreadable export stops the sweep. Material named by a folder's census is never removed. |
| S25 | A copying backup releases its snapshot only once `complete`. Retention never expires a share's newest complete copying backup at a location. A restore from one re-encodes into a new namespace, checks every chunk against its plaintext hash, and publishes all or nothing. |
| S29 | An export is sealed and authenticated under its namespace's export key by RFC 5's export cryptography, names in its clear part the export-key and master-key IDs it needs, carries each key record as RFC 5's key table holds it, carries every bound setting, each share's fold rule and entry-digest key, and every netgroup its policy names with its members, and an import refuses any mismatch. |
| S30 | A recovery import leaves its namespace's GC paused until the operator states every share recovered or given up. |
| S31 | A folder record is a fenced lease: a holder acts only under its own lease number, and issues no put or delete once *L* / (1 + ρ) − σ has passed since the read that showed *L* left. Every wait on another process's or installation's timeout is *T* × (1 + ρ) + σ. |
| S36 | No backup survives losing the master key that wraps its namespace's keys; every export names the master keys it needs, and a master key any retained export names is never destroyed. |
| S37 | A failed copy is the base of its successor; `max_copy_time` bounds one attempt; a policy is held to its incremental copy, not its first. At an immutable location every put carries its own recorded retain-until within the location's cap, extensions come once per generation, backups are found by listing versions, every state and progress version is authenticated and the one of highest authenticated sequence is read, the location's one health object is written at location creation, and it and the last good backup never age out. |

## 6. Test plan and benchmarks

The set-wide rules and tiers are in [the RFC index](rfc-index.md#Test%20tiers).
Every check runs against a real metadata backend and the remote-tier emulator; GC
runs as in production, not stubbed.

### 6.1 How it is tested

These checks run on [RFC 12 §5.1](rfc-12-snapshots.md#5.1%20How%20it%20is%20tested)'s
model checker, deterministic simulator and model-based run.

**A model.** A folder's copies and sweeps ([§2.4.3](#2.4.3%20Writing%20one%2C%20step%20by%20step),
[§2.4.4](#2.4.4%20Expiry%20and%20the%20sweep)) **MUST** be modelled in a model
checker before they are implemented, with S24 as a property, and kept in step
with this document.

**Coverage.** Seed search **MUST** also reach each of these or fail: an abandoned
use record released; a copy reusing a base block, and one refusing a block with
retiring material; a copy resumed from its progress; a sweep stopped by an
unreadable export; a relocated block re-resolved mid-copy; a copy built on a
failed predecessor's progress.

### 6.2 Scenario catalogue

| ID | Scenario |
| --- | --- |
| `S-snap-catalog-restore` | [§2.3](#2.3%20Restore)'s example: a catalog backup, loss of the metadata store, recovery import |
| `S-snap-backup-abandoned` | a backup crashes while `writing`; at its use record's deadline it is marked `failed` and the snapshot becomes deletable; the failed backup is refused by restore |
| `S-snap-copy-incremental` | [§2.4](#2.4%20Copying%20backups)'s first example: incremental copies across a relocation, expiry, sweep and verify |
| `S-snap-copy-restore` | [§2.4](#2.4%20Copying%20backups)'s second example: the bucket is lost; a restore re-encodes into a new namespace on another installation, and the failing policy keeps the last good backup |
| `S-snap-copy-crash-<step>` | the copier or the sweep crashes after each step; a restart before the deadline resumes, one after it marks `failed`, and the next sweep leaves exactly the retained manifests' blocks |
| `S-snap-copy-relocated` | compaction relocates and deletes a block while a copy is about to read it; the copy re-resolves and copies the new block |
| `S-snap-copy-retire-material` | material is being retired while backups list folder blocks sealed with it; copies stop reusing them, removal waits for the folder census, and it proceeds after they expire |
| `S-snap-copy-unreadable-export` | one export at the location is corrupt; the sweep deletes nothing |
| `S-snap-copy-damaged` | a folder block is corrupted; `Verify` marks the backup `damaged`; the next copy takes no base from it; restore refuses it |
| `S-snap-import-version-floor` | a share imported into an installation whose journals are already open is written at once; the journal raises its counter above the share's floor first |
| `S-snap-copy-immutable` | copies to an immutable location under a stolen put credential, a long copy, and a chain of failing copies; restores read recorded versions |
| `S-snap-recovery-shared` | a recovery import of one share of a namespace whose clone has no backup; GC stays paused until the operator's statement |
| `S-snap-folder-lease` | a sweep paused past its folder lease resumes after a copy took the folder |

### 6.3 Group A — lost or wrong content

| Invariant | Check |
| --- | --- |
| S14 | Corrupt one byte in each frame position, truncate the stream at every frame boundary, kill the importer at each step: nothing publishes, staging is empty after restart. During a clone's staging, run two audit walks and the deleter: no chunk a staged ref names is retired. |
| S15 | Import with one material ID removed from B's provider, and with a different scope: refused before any record is staged. |
| S16 | `S-snap-import-version-floor`: import a share whose files carry versions far above the counter of a journal already open at B, and write to one file at once, then from a per-child shard created afterwards. Each write's version exceeds the imported ones, commits and reads back. Skip the raise: the first write is dropped at its commit and the read returns the imported bytes. Restore a share whose highest `Number` is 10⁶ and create a file: its number is above 10⁶; skip the allocator raise and two files report one id. |
| S18 | Plant wrong counts in an export: the import's counts are correct and the audit is clean. |
| S19 | Import an export whose principal ID B holds for another user, and one whose UID B maps to another principal: both refused, naming the principal. Import a share named `Photos` into a B holding `photos`, and one whose path lies inside another share's: both refused, naming the collision; with a new name given in the request, accepted. Import one whose principals B lacks: adopted by ID, with no secret. |
| S23 | `S-snap-copy-incremental` and `S-snap-copy-relocated`: every folder block's name recomputes from its header, and every chunk decodes to its hash. `S-snap-copy-retire-material`: reuse a block with retiring material, and removal finds it still named. |
| S24 | `S-snap-copy-crash-<step>` and `S-snap-copy-unreadable-export`: after each, the folder holds exactly the union of the retained manifests plus writing progress, and every retained backup restores. Let a copy and a sweep run at once: a sweep deletes a block the copy reused. |
| S25 | `S-snap-copy-restore`, `S-snap-copy-damaged`. Delete the snapshot right after a copy completes, run GC to completion, and restore from the copy: every file reads back by hash. |
| [§2.3](#2.3%20Restore) | `S-snap-catalog-restore` with every block the backup names relocated first: every stale hint is resolved from block headers and every file reads back; the older backup survives GC. |
| S24 | `S-snap-copy-immutable`: copies to an immutable location — every get by recorded version; a put under an existing name by a stolen credential, then a restore: the restore reads the recorded versions and every file reads back. A copy that took 40 h: every version its manifest names is locked to its completion plus retention, including blocks put in its first hour. Copies keep failing for twice the retention: the last good backup's versions are still locked and it restores. A policy whose period is below its estimated incremental copy is refused `ErrPolicyPeriod`; one below its full copy but above its increment is accepted. A first copy of 40 TiB at 200 MiB/s with `max_copy_time` 48 h: it fails once, its successor builds on its progress, and the share has a complete backup within three attempts; restart each attempt from zero and none completes. Run past the lifecycle age with the health object and the last good backup untouched: the location still opens, and a restore lists versions and finds the backup. Put a newer `expired` state version with a stolen credential: the restore ignores it, since it does not authenticate. Re-put the backup's own authentic `writing` version: the restore reads `complete`, the higher sequence. Copy the first namespace ever to a fresh immutable location: it opens, finding the location's health object by its recorded version. Keep a process up past the lifecycle age plus two generations: the location still opens, and the health object's lock never exceeds the cap. A failed first copy is settled before its successor runs: its progress objects stay and the successor builds on them. Answer one extension in a burst with a throttling reply: the backup completes. |
| S29 | Flip one byte of an export's clear part, header, a frame and the trailer, and move one frame to another position: each is refused `ErrExportCorrupt`. Read an export without the master key: no file name, chunk ID or principal appears in its bytes. Import with a chunking target, an encrypt flag or a key ID that differs from B's: refused `ErrScopeMismatch` naming it. Import a policy naming netgroup `ops` into a B whose `ops` has other members: refused `ErrNetgroupCollision`; into a B without `ops`: adopted with its members, and a client of the deny list is still denied. |
| S29 | Restore a catalog backup of a case-insensitive share whose listings clients were paging: every name is found by lookup, and a listing resumed from a cookie taken before the backup continues where it stopped. Draw a fresh digest key or fold rule at import: lookups miss and cookies resume elsewhere. Check the export's frame nonces: no two frames of one export repeat one. |
| S30 | `S-snap-recovery-shared`: a namespace holds a share with a catalog backup and its clone with none, sharing blocks. Recover the share alone: GC deletes, relocates and collects nothing in the namespace, and every block the clone needs survives; after the operator's statement GC runs and the audit is clean. Start GC at publish instead: the clone's blocks are deleted. |
| S31 | `S-snap-folder-lease`: a sweep pauses past its deadline after its mark; a copy takes the folder and reuses a block the mark left unlisted; the sweep resumes: it deletes nothing, by its lease number and its own clock. Run the sweep's clock 5% slow: it stops by *L* / 1.05 − σ; fence by the deadline alone, without the clock bound, and the sweep deletes the reused block. A recovery import whose clock runs 5% fast against a partitioned old holder's waits two `Recheck` periods × 1.05 + σ by the slow clock; wait two periods plus a fixed margin and both delete for a while. |
| S36 | Back up an encrypting namespace, rotate its master key, and destroy the old one: refused `ErrMaterialInUse`, naming the backup; expire the backup and the destroy succeeds. Restore the backup on an installation that holds no master key: refused `ErrMaterialMissing`. Back up a non-encrypting namespace, lose the host's disk, and restore from the bucket and the backup with the escrowed master key: every file reads back; without it, the restore is refused `ErrMaterialMissing` though the chunk-ID key is in the bucket. |
| S30 | Delete the embedded metadata store with the node stopped and start it: the node serves nothing and GC deletes nothing, naming the store; a recovery import from the newest catalog backup serves the files as of that backup, and the blocks written since stay until the operator's statement. |

### 6.4 Group B — cost and wedging

| Concern | Counted check |
| --- | --- |
| [§3.1](#3.1%20Layout) streaming | Import of a 10⁶-file export holds at most one frame plus bounded batches in memory. |
| [§2.4](#2.4%20Copying%20backups) increment | A second copy of an unchanged share puts no block. After one changed file, it puts only the blocks holding its new chunks. A sweep lists the folder once. |

### 6.5 Benchmarks and targets

Recorded on the reference box ([the RFC index](rfc-index.md#Test%20tiers)).

| Benchmark | Measures | Target |
| --- | --- | --- |
| Export and import of a 10⁶-file namespace | records/s | at least half the backend's own scan and batch-write rates |
| Recovery import, all hints stale | header gets per block | one ranged get per block in the namespace |
| Daily copying backup of a 1 TiB share, 1% changed | bytes put; wall time | at most the blocks holding the changed chunks; at `backups.copy_rate` |
| Restore of a 1 TiB copying backup into a new namespace | MB/s | report, against the codec's decode plus encode throughput |

## 7. Observability

Metric names are shown without the deployment's prefix, which the exporter adds.

| Answers | Metric | Type |
| --- | --- | --- |
| age of the newest complete backup per share, labelled `kind`; the alert for a policy that stopped | `backup_newest_age_seconds` | gauge |
| copying backups' blocks, labelled `result` = `copied`, `reused`, `reresolved`; bytes put | `backup_copy_blocks_total`, `backup_copy_bytes_total` | counter, counter |
| blocks and bytes held per block folder; blocks swept; sweeps stopped by an unreadable export, an alert | `backup_folder_blocks`, `backup_folder_bytes`, `backup_swept_total`, `backup_sweep_refused_total` | gauge, gauge, counter, counter |
| verifications, labelled `result` = `ok`, `damaged` | `backup_verify_total` | counter |
| export and import records and bytes, labelled `kind` | `export_records_total`, `import_records_total` | counter |
| import refusals, labelled `reason` = `corrupt`, `material`, `scope`, `fileid`, `claim`, `hint`, `principal` | `import_refused_total` | counter |
| folder-lease actions refused by a lease number or a holder's own clock | `backup_lease_fenced_total` | counter |
| immutable versions extended at copy completion or for the last good backup | `backup_retention_extended_total` | counter |

Logs: each publish of an import logs at `Info` with share, namespace and export
digest. An import refusal logs at `Warn`, naming the missing material or the
mismatching scope. Each copy, sweep and verify logs at `Info` with its counts; a
copy marked `failed`, a backup marked `damaged` and a stopped sweep log at `Warn`.

## 8. Open questions

1. **Handles across a recovery import.** It assigns new FileIDs, so clients remount;
   keeping them would need proof that no other restore of the namespace used them.

---

## Appendix A — where the current code differs

Descriptive, for the refactor.

| # | This document says | The code today |
| --- | --- | --- |
| D4 | Restore makes a new share ([§2.3](#2.3%20Restore)) | restore overwrites the share in place, behind a safety snapshot and a restore-in-progress marker |
| D8 | Catalog backups to a location outside the metadata store ([§2](#2.%20Catalog%20backups)) | none; snapshots cannot be exported |
| D15 | Copying backups copy blocks to a folder of their own, incrementally, and restore into a new namespace ([§2.4](#2.4%20Copying%20backups)) | none |
