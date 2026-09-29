---
rfc: 9
title: "RFC 9 — GC: sweep, trash, compaction and remote deletion"
component: GC
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-3-syncer]]"
  - "[[rfc-4-remote-tier]]"
  - "[[rfc-5-transforms]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-10-journal-replication]]"
aliases:
  - RFC 9
tags:
  - rfc
---
# RFC 9 — GC: sweep, trash, compaction and remote deletion

**Status:** draft.
**Audience:** anyone changing sweep, the deleter, the compactor or collection, a
metadata backend's block and chunk records, or adding a way to keep content alive.

This document specifies behaviour, not the current code. [Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists
where the code differs.

---

## 1. Purpose

GC answers one question about the remote tier:

> **Which remote objects may be deleted, and when is deleting one safe?**

It is the only component in the set that destroys the last copy of content
([RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep)). Every other failure in the system can lose a local copy, serve a
stale answer or stop accepting writes. A GC defect deletes data that a file still
names, and nothing downstream can recover it once the object is gone.

GC is one of three reclaimers. Each owns one kind of space, and none reclaims
another's:

| Space | Reclaimed by | When | Destroys |
| --- | --- | --- | --- |
| **Local journal space** | the journal's release and repack ([RFC 1 §8.1](rfc-1-journal.md#8.1%20Releasing%20storage), [§8.2](rfc-1-journal.md#8.2%20Repack)) | when the engine's `EvictionPolicy` and `CapacityGovernor` decide ([RFC 8 §10](rfc-8-engine.md#10.%20Local%20space)) | only local copies of content already durable remotely; nothing else is evictable ([RFC 8 §10.4](rfc-8-engine.md#10.4%20Nothing%20but%20durability%20makes%20an%20extent%20unevictable)) |
| **Metadata records** | removals, releases and snapshot deletion ([RFC 6 §6](rfc-6-block-metadata.md#6.%20Reference%20counting)) | when a file is truncated, released or a snapshot deleted, in batches of at most K refs | refs and counts; never an object |
| **Remote objects** | this RFC | when a block's count is zero and its trash delay has passed | the object |

GC runs four operations, all on the remote tier:

| Operation | What it destroys | Section |
| --- | --- | --- |
| **sweep** and the **deleter** | a block whose `live` count reached zero: sweep retires its records into the trash, and the deleter deletes the object once the trash delay has passed | [§3](#3.%20Sweep) |
| **compactor** | nothing — it rewrites the live chunks of mostly dead blocks into a new block, so the old ones reach zero and sweep takes them | [§4](#4.%20Compactor) |
| **collection** | an object no record names, or one whose put intent was abandoned | [§5](#5.%20Unrecorded%20objects) |
| **audit** | nothing — it recomputes counts, repairs them, and reports | [§6](#6.%20Audit) |

**From a deleted file to a deleted object.** Nothing deletes a block record
directly; every step follows from a count reaching zero:

1. The file is released ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)). Block metadata drops its refs in batches of at
   most K ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)); a snapshot that still sees them keeps them as history refs.
2. Each dropped ref decrements its chunk's refcount in the same transaction. A
   refcount that reaches zero decrements the `live` of the block its chunk record
   names.
3. The transaction that leaves a block's `live` at zero writes the block's **zero
   index key** ([§3.5](#3.5%20Finding%20candidates%20costs%20what%20is%20retirable)): the block is now a candidate.
4. Sweep reads the zero index and **retires** the block in a conditional
   transaction: its chunk records go, and the block record moves to state
   `retired` with `not_before` = now + the trash retention ([§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object)).
5. After `not_before`, the **deleter** moves the record to `deleted`, deletes the
   object in a batch, and prunes the record ([§3.2](#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded)).

> [!important] Pending review — three reclaimers, and the walk to a delete
> GC is stated as the remote tier's reclaimer only, beside the journal's and
> block metadata's, with the chain from a released file to a deleted object.

### 1.1 Non-goals

GC **MUST NOT**:

- recover local space. Eviction and reclamation are the journal's ([RFC 0 §8.1](rfc-0-data-lifecycle.md#8.1%20Evict),
  [RFC 0 §8.2](rfc-0-data-lifecycle.md#8.2%20Reclaim); [RFC 1](rfc-1-journal.md)). GC never reads, writes or unlinks a segment;
- decide what is referenced. Refcounts are block metadata's ([RFC 6 §6](rfc-6-block-metadata.md#6.%20Reference%20counting)), and a
  file's release is the namespace's ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)). GC reads the count; it does not
  keep one of its own;
- keep content alive by any means other than the count ([§2.1](#2.1%20The%20count%20is%20the%20only%20authority));
- restate what a put, a read or a delete means. That is [RFC 4](rfc-4-remote-tier.md)'s ([RFC 4 §1.2](rfc-4-remote-tier.md#1.2%20A%20contract%2C%20not%20a%20component));
- name a block by any means other than [RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block), or frame, seal or parse an
  object ([RFC 4 §3](rfc-4-remote-tier.md#3.%20The%20block%20format));
- import another component in this set ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)).

### 1.2 Words this document uses, and two it does not

[RFC 0 §1.1](rfc-0-data-lifecycle.md#1.1%20The%20component%20set) assigns this component "mark/sweep". **Sweep** keeps its meaning:
retiring a block that nothing references ([RFC 0 §2.3](rfc-0-data-lifecycle.md#2.3%20Operations)). **Mark** survives
only as the audit of [§6](#6.%20Audit), which recomputes counts and never permits a delete
([§2.4](#2.4%20A%20snapshot%20holds%20its%20blocks%20without%20a%20pin)).

**Relocation** is [RFC 6 §7.3](rfc-6-block-metadata.md#7.3%20Relocation)'s word for the record change that moves chunks into
a new block. The **compactor** ([§4](#4.%20Compactor)) is the GC component that decides when to
relocate and performs the transfers. Compaction here discards no referenced
content: it moves live chunks and leaves the dead ones for sweep.

**Trash** is the interval between a block's retirement and its object's delete
([§3.7](#3.7%20Trash)). "Grace period" is not used: trash postpones a delete that the count
already permitted, and never permits one.

**Collection** is [§5](#5.%20Unrecorded%20objects)'s deletion of objects no live record names.

## 2. What is safe to delete

### 2.1 The count is the only authority

A block **MAY** be deleted only when its `live` count is zero ([RFC 6 §2.3](rfc-6-block-metadata.md#2.3%20Block)), and
only after the conditional retirement of [§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object). Nothing else permits a delete, and
nothing but the count prevents one.

In particular, an implementation **MUST NOT** consult, in deciding whether a
delete is permitted:

- a hold set, a pin list, or an extra root for a snapshot, an open file or any
  other holder. Every holder of content holds counted references ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref);
  [RFC 7 §4.4](rfc-7-namespace-metadata.md#4.4%20There%20is%20no%20third%20holder));
- elapsed time. Elapsed time **MUST NOT** permit a delete, and **MUST NOT**
  substitute for conditional retirement or conditional adoption ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence)).
  It **MAY** postpone one: the trash of [§3.7](#3.7%20Trash) does exactly that;
- any state held in process memory, a lock or a lease. Two passes in two
  processes **MUST** be as safe as two in one.

A second liveness mechanism fails open. Every path that decides liveness has to
remember it, the one that forgets deletes held content, and the audit reports
nothing wrong because by the counts nothing was.

> [!note] Prior art (non-normative)
> Most systems that delete shared content guard the delete with time, a trace,
> or both:
>
> - **Time as the guard.** Git prunes unreachable loose objects only once older than
>   `gc.pruneExpire`, so a concurrent writer's not-yet-referenced objects survive
>   ([git-gc](https://git-scm.com/docs/git-gc)). Delta Lake's `VACUUM` removes
>   unreferenced files only past a retention threshold, and warns that shortening it
>   can break concurrent readers and writers ([Delta utility](https://docs.delta.io/latest/delta-utility.html)).
>   Ceph RGW holds a deleted object's tail for a minimum wait before its garbage
>   collector removes it, so in-flight reads finish ([RGW config](https://docs.ceph.com/en/latest/radosgw/config-ref/)).
>   Kopia deletes unreferenced blobs only after safety margins sized for
>   concurrent snapshot writers ([Kopia maintenance](https://kopia.io/docs/advanced/maintenance/)).
> - **Trace as the guard.** restic `prune` builds the used set from every snapshot,
>   writes repacked packs and the new index, and deletes old packs only after
>   both, under an exclusive lock ([restic references](https://restic.readthedocs.io/en/latest/100_references.html)).
>   Guo and Efstathopoulos chose grouped mark-and-sweep over reference counts
>   because counts updated outside a transaction drift under crashes and lost
>   updates ([ATC '11](https://www.usenix.org/events/atc11/tech/final_files/GuoEfstathopoulos.pdf)).
>   Windows Server Data Deduplication pairs its regular garbage collection with a
>   periodic full collection that reclaims what the regular one misses
>   ([advanced settings](https://learn.microsoft.com/en-us/windows-server/storage/data-deduplication/advanced-settings)).
> - **Counts plus trash.** JuiceFS counts slice references, holds deleted files in a
>   trash for a configurable number of days, and offers a separate `gc` that
>   compares a bucket listing with metadata to find leaks
>   ([internals](https://juicefs.com/docs/community/internals/), [trash](https://juicefs.com/docs/community/security/trash/)).
>
> This RFC keeps the count as the sole authority because every count changes in
> the same transaction as the refs that change it ([RFC 6 §6.1](rfc-6-block-metadata.md#6.1%20A%20refcount%20is%20exactly%20its%20refs)), which removes
> the drift the ATC '11 authors measured; the adoption race that the time-guarded
> systems close with a grace window is closed here by transactions ([§3.3](#3.3%20The%20race%20with%20adoption%20is%20closed%20by%20transactions%2C%20not%20by%20time)),
> which a stalled writer cannot outlast. From the others it takes a trash that
> postpones deletes ([§3.7](#3.7%20Trash)), a periodic full audit ([§6](#6.%20Audit)), and a listing
> backstop for leaks ([§5](#5.%20Unrecorded%20objects)) — none of which permits a delete.

> [!important] Pending review — time may postpone, never permit
> The rule is kept and reworded: elapsed time never permits a delete and never
> replaces a conditional transaction, but may postpone one. Prior art is cited.

### 2.2 Zero is a candidate, not a verdict

`live` is read at one instant and the retirement happens at another. Between them
an offload commit can adopt one of the block's chunks ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence)), a clone can
copy a ref to one, and a relocation can move one in or out. So a zero read outside a
transaction **MUST** be treated as a candidate only. The verdict is taken inside
the retirement transaction, which re-reads `live` and refuses if it is nonzero
([RFC 6 §7.1](rfc-6-block-metadata.md#7.1%20Conditional%20retirement)).

A sweep therefore has no beginning that matters: each retirement is its own
decision, taken on the state at its own commit, which is how [RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep) holds.

### 2.3 The absence of a record proves nothing

A block with no record in this store is not thereby unreferenced ([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)).
Another store, another process or another deployment writing into the same key
namespace can reference it. Sweep therefore acts only on blocks this store
records. Objects no store records are [§5](#5.%20Unrecorded%20objects)'s, and [§5](#5.%20Unrecorded%20objects) runs only where the namespace
is proven to belong to the stores it enumerated.

The namespace is fixed by the key scope of [RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope). A deployment **MUST**
meet [RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count) — one keyspace partition per namespace — before any GC operation
runs. The shares of one namespace are one counting domain, their refs counted
in its one partition, and a GC pass reads only that partition.

![Two timelines. Above, a sweep reads the live set, a carve adopts h and commits a ref, and the sweep deletes h's block. Below, an adoption and a conditional retirement in both orders, each ending safely](img/rfc7-sweep-race.svg)

### 2.4 A snapshot holds its blocks without a pin

A snapshot needs no pin, hold or extra root, because it already holds counted
refs. When a live ref that a snapshot sees is superseded or released, it moves to
history in the same transaction instead of being dropped, and a history ref
counts exactly as a live one ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). So every chunk a live snapshot can
read has refcount ≥ 1, the block its record names has `live` > 0, and sweep
never retires it. Deleting the snapshot drops the history refs no other live
snapshot sees; only then can their counts reach zero. A backup is an export of a
snapshot ([RFC 12 §3.1](rfc-12-snapshots.md#3.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata)) and is held the same way while the snapshot lives.

**Tracing is not the delete authority.** A mark reads the live set at one instant
and deletes at a later one; a ref committed in between names a chunk the mark did
not see. Making that safe needs a second mechanism for adoptions made after the
mark began, which [§2.1](#2.1%20The%20count%20is%20the%20only%20authority) forbids. The trace survives as the audit ([§6](#6.%20Audit)), which
checks the counts and never deletes.

> [!important] Pending review — snapshots need no pin
> §2.4 now states why snapshots and backups hold their blocks through counted
> history refs, and cuts the mark discussion to why tracing never deletes.

## 3. Sweep

### 3.1 Retire the records, then delete the object

A block record carries its own GC state, and that state is the state machine
([RFC 6 §2.3](rfc-6-block-metadata.md#2.3%20Block)):

    live ──retire──▶ retired{not_before} ──due──▶ deleted ──object deleted──▶ (pruned)
      ▲                    │
      └──────restore───────┘   (trash recovery, §3.7)

| State | Meaning | Index key written with it | Left by |
| --- | --- | --- | --- |
| `live` | the object may hold referenced chunks | the **zero index** key `BZ‖ns‖name`, exactly while `live` = 0 | retirement |
| `retired` | its chunk records are gone; the object is in the trash until `not_before` | the **retired index** key `BR‖ns‖not_before‖name` | the deleter, or a restore |
| `deleted` | the object is being deleted; nothing can name it again | the **deleted index** key `BD‖ns‖name` | pruning, once the delete succeeded |

Every transition is one transaction, and each writes the index key of the state
it enters and deletes the one of the state it leaves. The index keys are derived
from the block records and can be rebuilt from them ([§7.4](#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt)).

1. **Retire** ([RFC 6 §7.1](rfc-6-block-metadata.md#7.1%20Conditional%20retirement)). Sweep takes names from the zero index and retires
   them in batches bounded by K records written ([RFC 6 §5.2](rfc-6-block-metadata.md#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed)). For each block in
   the batch, inside the transaction: if the record is `live` with `live` = 0,
   delete every chunk record whose `block` names it, set the state to `retired`
   with `not_before` = now + the trash retention ([§3.7](#3.7%20Trash)), delete its zero key and
   write its retired key; otherwise leave it and drop it from the batch. One
   block's refusal does not fail the batch.
2. **Mark deleted.** The deleter reads the retired index in `not_before` order up
   to now, and in one transaction per batch moves each record still `retired`
   with `not_before` ≤ now to `deleted`, swapping its retired key for its deleted
   key.
3. **Delete** the objects through the remote store's multi-object delete
   ([RFC 4 §4.5](rfc-4-remote-tier.md#4.5%20Delete%20is%20batched%20and%20idempotent)), up to `gc.delete_batch` names per call (default 1,000, the
   common service limit; the store splits further if its service needs it).
   Each name's result is its own.
4. **Prune.** For each name whose result is success, one transaction deletes the
   block record, conditional on state `deleted`, and its deleted key. A name that
   failed stays `deleted` and is retried ([§3.6](#3.6%20Failures%20resolve%20on%20their%20own)).

The order is not a preference. Retiring first means a crash between the steps
leaves an object that a `retired` or `deleted` record still names, which the
deleter resumes. Deleting first means a crash, or a failed metadata write, leaves
records that name an object that no longer exists. Every read of those chunks then
fails, and every adoption of them succeeds, so new files acquire refs to content
that is gone. That is **Lost** for content that was durable.

`deleted` is final. A restore acts only on `retired` ([§3.7](#3.7%20Trash)), and step 2 is
conditional on `retired`, so the two conflict on the block record and at most one
commits. A delete is issued only for a name whose record is already `deleted`.

**Deletes go directly to the remote store**, not through the syncer ([RFC 3 §5](rfc-3-syncer.md#5.%20What%20belongs%20elsewhere)).
A delete is not a transfer: it **MUST NOT** occupy the syncer's pool or wait on
its fairness, and a store the syncer reports unhealthy only makes deletes fail,
which [§3.6](#3.6%20Failures%20resolve%20on%20their%20own) handles. The compactor's reads and puts are transfers and do go
through the syncer ([§4.2](#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)).

**Scheduling is the shared work scheduler's** ([RFC 8 §6.1.1](rfc-8-engine.md#6.1.1%20The%20shared%20work%20scheduler)). Sweep, the
deleter, the compactor and the audit are its sources. The scheduler batches,
retries with backoff and jitter, bounds concurrency, shares fairly between
namespaces and applies rate limits; each source owns its durability, and GC's is
the index keys. A restart resumes from them — `BD` first, then `BR` up to now,
then `BZ` — with no scan of block records.

> [!important] Pending review — GC is a state machine on the block record
> Replaces the pending-deletion and candidate records with block states
> (`live` → `retired{not_before}` → `deleted` → pruned) and three derived index
> keys, a batched deleter using multi-object delete, and the shared scheduler.

### 3.2 A retirement not yet deleted is durably recorded

A retired block keeps its block record until its object is gone: in state
`retired` through the trash, then `deleted` until the delete succeeds. Abandoning
an intent and collection write a record in state `deleted` for their name
([§5.4](#5.4%20Age%20is%20not%20the%20guard)). A restart **MUST** resume every `deleted` record and every `retired`
one whose `not_before` has passed, and finds them through the index keys.

This is what removes enumeration from sweep's correctness. Without the record, a
crash between [§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object)'s steps leaves an object that can only be found by listing the
store, and nothing's correctness may depend on a listing ([RFC 4 §4.6](rfc-4-remote-tier.md#4.6%20List%20is%20a%20complete%2C%20resumable%20walk)). With it,
the object is named by a record until the moment it is gone.

A `retired` or `deleted` record gates no put or commit. No writer reads it: a name
it holds can never be put or committed again ([§3.4](#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete)), and adoption reads the
chunk record, which retirement already removed.

### 3.3 The race with adoption is closed by transactions, not by time

Adoption is an offload commit referencing a chunk it did not carry ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence)).
Retirement and adoption are the only two operations that can disagree about
whether a chunk is alive, and both touch the same two records:

| Order | Adoption | Retirement | Outcome |
| --- | --- | --- | --- |
| adopt, then retire | refcount 0→1, `live` 0→1, zero key deleted | reads `live` = 1, refuses | block survives with the new ref |
| retire, then adopt | finds no chunk record, fails | deletes the chunk records, block to `retired` | pass retries carrying the chunk's bytes |

The table is correct only if the store serializes the two. An implementation
**MUST** guarantee that, of two transactions writing the same record, at most one
commits on a pre-state the other changed. Serializable and snapshot isolation
both provide this for a record both transactions write. Under a weaker level —
a read-committed default, for one — the conditions **MUST** be part of the write
statements themselves:

- retirement's "`live` is zero" **MUST** be the predicate of the state change, not
  a prior read;
- adoption **MUST** detect a missing chunk record from the write it issues — the
  count of rows it updated — and not from a read made earlier in the transaction.

A retried adoption carries the chunk's bytes, which the journal still holds: the
extent was offered because it is **Dirty** ([RFC 0 §5.2](rfc-0-data-lifecycle.md#5.2%20Offload)), and nothing reports it
durable until a commit succeeds ([RFC 6 §4.3](rfc-6-block-metadata.md#4.3%20The%20commit%20is%20the%20report%27s%20return%20edge)). So losing this race costs one
upload and never a client-visible error. The trash does not change this: a
retired block's chunks cannot be adopted, whatever its `not_before`.

### 3.4 A retired key is not re-created underneath its delete

The danger this section rules out: GC deletes object *K*, and at the same moment,
or later, something puts a new object under the same name *K* and commits a block
record for it. The delete, landing late, would remove content a record names.

Block metadata rules it out by construction, not by a fence ([RFC 6 §7.6](rfc-6-block-metadata.md#7.6%20Put%20intents)):

- **A name is minted once, for one put attempt**, from a fresh nonce
  ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)). No two attempts, and no two writers, ever put one name, and a
  retry within the attempt writes the same bytes.
- **Before any put** — an offload's or a compaction's — the writer durably records
  a **put intent** for the name, carrying its owner epoch.
- **The commit that creates the block record consumes the intent** in the same
  transaction, and fails if it is absent.

A name's state only moves forward:

    absent → intended → recorded (live → retired → deleted) | abandoned (deleted)

GC deletes an object only when its record is `deleted` and no intent names it.
Nothing can put or commit that name again. Four timelines show why each case
holds. *W* is an offload writer under owner epoch 7.

**A normal life.**

| t | Event | State of *K* |
| --- | --- | --- |
| 1 | *W* mints *K* and records `I(K, 7)` | intended |
| 2 | *W* puts object *K* | intended, object present |
| 3 | *W*'s commit consumes `I(K)` and creates `B(K)` with `live` = 3 | recorded, `live` |
| 50 | the file is released; `live` reaches 0; `BZ‖K` written | `live`, candidate |
| 51 | sweep retires *K*: chunk records deleted, `not_before` = 51 + 48 h | `retired` |
| 51 + 48 h | the deleter marks *K* `deleted`, deletes the object, prunes `B(K)` | gone |

No step after t = 3 can recreate `I(K)`, so no commit can create `B(K)` again.

**A late delete.** The deleter's batch holding *K* times out at t = 60 and the
process restarts. The same content is written again and offloaded; the new
attempt mints *K′* ≠ *K*. The retried delete of *K* lands at t = 61, after *K′*
is committed. It removes *K* only. *K′* is untouched because a name is never
minted twice.

**A commit racing an abandonment.** *W* puts *K* and stalls before its commit;
ownership moves to epoch 8.

| t | *W* (epoch 7) | GC | Result |
| --- | --- | --- | --- |
| 5 | — | abandons `I(K)`: reads and deletes it, writes `B(K)` = `deleted` | *K* is final |
| 6 | commit reads `I(K)`: absent | — | commit fails; *W* re-offers under a new name |

In the other order, *W*'s commit consumes `I(K)` first and the abandonment finds
no intent and does nothing. Both transactions read and delete the intent key, so
they conflict and at most one commits ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit)). A plain read under
snapshot isolation is not enough: the read **MUST** conflict with the other's
delete, by conflict tracking or a lock on the key.

**A put that lands after its delete.** *W*'s put of *K* is slow. At t = 5 GC
abandons `I(K)`; at t = 6 the deleter deletes *K* and prunes `B(K)`; at t = 7 the
put lands. Object *K* now exists with neither intent nor record. It can never be
committed — its intent is gone for good — so it is a leak, not a risk, and the
listing backstop of [§5](#5.%20Unrecorded%20objects) collects it.

So:

- a delete needs no fence and no delay;
- a delete that lands late — resumed after a crash, retried after a timeout —
  can reach no committed block;
- a writer whose commit fails for want of its intent re-offers its content, as
  any failed commit does; the object it put is already scheduled for deletion.

### 3.5 Finding candidates costs what is retirable

A sweep pass **MUST** cost O(blocks with `live` = 0), not O(blocks recorded). The
zero index delivers this: every transaction that moves a `live` block's count to
zero writes its `BZ‖ns‖name` key, and every transaction that moves it off zero
deletes it. Sweep reads only that prefix. The index is a hint and never an
authority: retirement re-reads `live` whatever it says. `live` crosses zero far
less often than a refcount changes ([RFC 6 §5.3](rfc-6-block-metadata.md#5.3%20Hot%20records%20that%20are%20not%20per-file)), so the key does not add a
hot record.

**Example.** A namespace records 10⁸ blocks. Block *K1* carries chunks *a* and
*b*, block *K2* carries *c*; one file references all three.

| t | Transaction | Counts | Zero index |
| --- | --- | --- | --- |
| 1 | release, phase-2 batch: drops the ref to *a* | *a* 1→0; `live`(K1) 2→1 | — |
| 2 | next batch: drops the refs to *b* and *c* | *b* 1→0, `live`(K1) 1→0; *c* 1→0, `live`(K2) 1→0 | `BZ‖K1`, `BZ‖K2` written |
| 3 | another file's commit adopts *c* | *c* 0→1, `live`(K2) 0→1 | `BZ‖K2` deleted |
| 4 | sweep reads the `BZ` prefix | — | finds *K1* only |
| 5 | retirement of *K1* re-reads `live` = 0 | — | `BZ‖K1` deleted, `BR‖…‖K1` written |

The pass read one index key and one block record, not 10⁸. Had the adoption at
t = 3 landed after sweep read the index but before its retirement, the retirement
would re-read `live`(K2) = 1 and refuse.

**A block can be born dead.** The commit that creates a block record can leave
its `live` at zero, and then no later event ever moves it from one to zero:

- two writers carry the same chunk list [*x*, *y*] and mint two names. *W1*
  commits *Ka* first, `live` = 2. *W2* commits *Kb*: both chunk records already
  name *Ka*, so *W2* adopts them, and *Kb*'s copies are dead weight. *Kb* is
  created with `live` = 0;
- content that repeats one chunk puts it in two blocks in flight, and the second
  to commit adopts it;
- every ref a pass carried was dropped because its file was released or truncated
  while the pass was in flight.

*Kb* holds no chunk record, so no refcount change can ever reach it. A rule
written as "a transaction that takes `live` from one to zero" never names it. The
creating commit is a transaction that leaves `live` at zero, and it **MUST** write
the zero key.

An implementation **MAY** instead scan the block records. The scan is correct and
proportional to the store, and on a large store it is the reason a pass does not
finish. One that scans **MUST** state that its pass costs O(blocks recorded).

> [!important] Pending review — §3.4 and §3.5 by example
> Both sections are rewritten around worked timelines on the new block states.

### 3.6 Failures resolve on their own

| Condition | Behaviour |
| --- | --- |
| Delete reports the object absent | Success. A delete is idempotent ([RFC 4 §4.5](rfc-4-remote-tier.md#4.5%20Delete%20is%20batched%20and%20idempotent)), and a deleter resuming after a crash will see this. |
| Some names in a batch fail | Each name's result is its own: the successes are pruned, the failures stay `deleted` and are retried by the scheduler with backoff. |
| Delete refused or throttled | The records stay `deleted`; the scheduler backs off, with jitter, and lowers the rate for that store. |
| Remote tier unavailable | Retirements continue, `deleted` and due `retired` records accumulate, and nothing is lost. A backlog that does not drain **MUST** be reported as a health condition, not only logged ([RFC 0 §10.2](rfc-0-data-lifecycle.md#10.2%20No%20state%20requires%20intervention%20to%20leave)). |
| Metadata unwritable | Nothing retires or moves to `deleted`, so nothing is deleted. |
| Retirement conflicts | Retried under I8: bounded by the pass's deadline, not an attempt count, with randomised backoff ([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries)). The retry re-reads `live`; it **MUST NOT** re-propose a decision taken on the pre-conflict state. |
| Crash anywhere | Every step is either inside a transaction or recorded by a block state and its index key, so a restart resumes and does not re-decide. |

A failure in one block **MUST NOT** stop the pass for others. No failure in this
table requires an operator to clear it.

### 3.7 Trash

Retirement sets `not_before` = now + `gc.trash_retention` (default **48 h**,
[RFC 13](rfc-13-configuration.md)), and the deleter leaves the object until then. The trash exists
for one purpose: a count defect that retired a block still referenced can be
repaired before the object is gone. It is not a safety input — the count and the
conditional transactions are ([§2.1](#2.1%20The%20count%20is%20the%20only%20authority)) — and a deployment that sets it to zero
loses only this recovery.

`not_before` is taken from the retiring node's clock. Skew moves an object's delete
earlier or later by the skew and never makes it unsafe.

**Recovery.** A `retired` block can be restored to `live`:

1. Read the object's header through a syncer flow ([RFC 4 §3.4](rfc-4-remote-tier.md#3.4%20Every%20read%20is%20verified%20by%20the%20codec)). It lists every
   chunk the block carries with its position and length. This needs the object
   present (it is, until the deleter marks it `deleted`) and the material that
   sealed the header ([RFC 5](rfc-5-transforms.md)).
2. In one transaction, conditional on the record still being `retired`: for each
   chunk in the header whose hash has no chunk record, create one naming this
   block, with its refcount from a targeted recount of the refs naming it
   ([RFC 6 §6.3](rfc-6-block-metadata.md#6.3%20Underflow%20is%20corruption%2C%20not%20a%20boundary)); leave alone a hash whose record names another block. Set
   `live` from the created records with a nonzero refcount, set the state to
   `live`, delete the retired key, and write the zero key if `live` is still zero.

It applies when the audit finds a ref whose hash has no chunk record ([§6.3](#6.3%20A%20ref%20with%20no%20chunk%20record%20is%20Lost%2C%20and%20trash%20is%20searched)), and
when an operator recovers from a known count defect within the retention. It
cannot apply once the record is `deleted`: the restore and the deleter's
transition conflict on the block record ([§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object)).

**What skips the trash.** An object whose intent was abandoned, and one found only
by listing, never held committed content: no chunk record ever named it. Both are
written straight to `deleted` and deleted at once ([§5](#5.%20Unrecorded%20objects)).

**What it costs.** Trash holds, at steady state, the bytes retired over one
retention period: about churn × retention. At 1% of stored bytes retired per day,
each day of retention costs about 1% of stored bytes in extra remote space and one
block record per retired block. The default 48 h costs about 2%.

> [!important] Pending review — trash with recovery
> Retired objects wait 48 h (a setting) before deletion; a retired block can be
> restored from its header; abandoned and listed orphans skip the trash.

## 4. Compactor

![Compaction: three mostly dead source blocks, ranged reads of their live chunks only, one put of a new block, one transaction that moves the chunk records, and the sources swept through the trash](img/rfc9-compaction.svg)

Sweep deletes safely; the compactor rewrites for space. Sweep is required for
correctness of accounting — a block at zero must go — while compaction is an
optimisation: it is rate-limited, may be turned off, and a namespace without it
only holds more dead bytes.

> [!important] Pending review — relocation becomes the compactor
> Relocation is now a GC component of its own, with its policy, state machine,
> bounds and tests, and its transfers stated: ranged reads of live chunks only,
> then one put per new block.

### 4.1 A block that is mostly dead pins its dead bytes

A block is deleted only when every chunk it carries is unreferenced. A block with
one referenced chunk and forty dead ones keeps all forty-one on the remote tier
indefinitely. The compactor copies the referenced chunks into a new block so that
the old one reaches `live` = 0 and sweep can take it. It is also the only way a
chunk is re-encoded ([RFC 5 §5.2](rfc-5-transforms.md#5.2%20Relocation%20re-encodes)): there is no in-place re-encode.

The compactor destroys nothing. It **MUST NOT** delete an object, and **MUST NOT**
retire a record. It moves chunk records and counts ([RFC 6 §7.3](rfc-6-block-metadata.md#7.3%20Relocation)), and the old
blocks are swept by [§3](#3.%20Sweep) exactly like any other block that reached zero,
trash included.

### 4.2 Read verified, mint, put, then move

The compactor opens a syncer flow on the store ([RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface)). Its reads and puts
are transfers, so the pool's memory bound, retries and health refusal apply to
them ([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)), and they share the pool fairly with offload ([RFC 3 §2.9](rfc-3-syncer.md#2.9%20Workers%20are%20shared%20fairly%20across%20flows)).

**Transfers.** The compactor reads only live chunks, each by a ranged read of its
body at the position its chunk record gives ([RFC 4 §4.1](rfc-4-remote-tier.md#4.1%20Interface)), and writes each
target with one whole-block put. It uses no multipart upload and no server-side
copy: a block is small enough for one put ([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20durable%20on%20success)), and a copy would carry
the source's encoding and header, which the target must not reuse. Dead bytes are
never read.

**Example.** Three 4 MiB blocks, *B1*, *B2* and *B3*, of twelve chunks each, hold
two, two and three live chunks: about 1.7 MiB live of 12 MiB stored.

1. **Plan.** The compactor reads the compaction index ([§4.4](#4.4%20When%20to%20compact%20is%20policy)) and groups
   *B1*–*B3*, whose live bytes together fit one target. For each, `LiveChunks`
   yields the seven chunk records that name it with a nonzero refcount. Nothing
   is written; a crash here costs nothing.
2. **Read.** Seven ranged reads, one per live chunk, through the flow, which
   decodes each body and verifies each chunk ([RFC 4 §3.4](rfc-4-remote-tier.md#3.4%20Every%20read%20is%20verified%20by%20the%20codec)). The compactor
   **MUST NOT** parse a block or verify it itself. It reads 1.7 MiB, not 12.
3. **Assemble** the seven chunks into target *T* under [RFC 2 §5](rfc-2-carver.md#5.%20The%20block%20assembler): whole chunks
   only (P1), the target size (P2), and only the chunks whose bytes it carries (P3).
4. **Mint** *T*'s name by [RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block), from a fresh nonce, and record its put
   intent under GC's epoch ([§3.4](#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete), [RFC 6 §7.6](rfc-6-block-metadata.md#7.6%20Put%20intents)). A nonce never equals a
   source's name, so no check against the sources is needed.
5. **Put** *T* in one put, encoded under the store's current transform chain
   ([RFC 5 §5.2](rfc-5-transforms.md#5.2%20Relocation%20re-encodes)). A retry within this attempt reuses the name and the plan.
6. **Move**, after the put is reported durable, in one transaction
   ([RFC 6 §7.3](rfc-6-block-metadata.md#7.3%20Relocation)) that consumes *T*'s intent and fails if it is absent, points each
   moved chunk record that still names a source at *T*, creates *T*'s block
   record, and moves `live` from each source to *T* by the number of its moved
   chunk records whose refcount is nonzero, counted inside the transaction. Here
   `live`(T) = 7 and *B1*–*B3* reach 0, so the same transaction writes their zero
   keys.
7. **Sweep** retires *B1*–*B3* ([§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object)); after the trash retention the deleter
   deletes them. Remote bytes fall from 12 MiB to 1.7 MiB.

A reader that resolved a chunk to *B2* before step 6 still finds *B2* for the
trash retention, and after it re-resolves ([§4.3](#4.3%20A%20reader%20can%20hold%20the%20old%20location)).

**State machine.** A compaction's durable state is the put intent and the chunk
records; nothing else is written.

| Crash after | State | Outcome |
| --- | --- | --- |
| 1–3 | nothing written | no effect; the index still lists the sources |
| 4 | an intent, no object | a re-run mints a new name; the old intent is abandoned once GC's epoch moves on, and its record goes straight to `deleted` ([§5](#5.%20Unrecorded%20objects)) |
| 5 | an intent and an object no record names | a re-run mints a new name; the orphan is found by its abandoned intent and deleted ([§5](#5.%20Unrecorded%20objects)) |
| 6 | chunks moved, every source's `live` at zero | the sources are ordinary sweep candidates |

A chunk whose refcount is zero is not moved. Its record stays pointing at its
source until the source retires. If it is adopted in between, the source's `live`
becomes nonzero again, the source survives, and the next compaction moves it.
That is a leak for one pass, not a loss.

### 4.3 A reader can hold the old location

A reader that resolved a chunk to *B* before step 6 can issue its read after *B*
is deleted, and find the object absent. Refs name hashes ([RFC 6 §2.5](rfc-6-block-metadata.md#2.5%20Refs%20name%20hashes%2C%20never%20blocks)), so the
chunk is still reachable, at its new block, and can move again while a slow read
is in flight.

When the remote store reports a block absent, the read path **MUST** resolve the
chunk again and retry **while the resolved block name keeps changing**, bounded
by the read's deadline, and **MUST** report **Lost** only when the same block name
misses twice or no chunk record for the chunk exists ([RFC 8](rfc-8-engine.md)). A fixed retry
count turns two compactions in a row into a spurious error; retrying a name that
did not change turns a lost chunk into a hung read.

A ranged read that fails verification is **corrupt**: one name always holds the
same bytes ([RFC 5](rfc-5-transforms.md)).

Compaction is safe only while these rules hold.

### 4.4 When to compact is policy

Compaction is **on by default**. A block is a candidate when its **dead-byte
ratio** — the bytes of its chunks whose refcount is zero, plus the bytes it
carries for chunks another block owns, over its bytes — exceeds
`gc.compaction.dead_ratio`. GC **MUST** select on bytes, not chunks, because chunk
sizes vary several-fold ([RFC 2 §3](rfc-2-carver.md#3.%20The%20boundary%20function)). A deployment **MAY** turn compaction off,
stated in its configuration; such a namespace keeps every block with one live
chunk indefinitely.

GC **MUST NOT** compact a block whose chunks are all referenced, with one
exception: retiring material or a transform ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)) compacts every block
whose bodies use it, fully live or not. The target's minted name ([§4.2](#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move) step 4)
differs from the source's even though its chunk list is unchanged.

**Finding compaction candidates MUST NOT cost a scan of every block per pass.**
A block's dead bytes change only when a refcount of one of its chunks crosses
zero, which the transaction doing so already writes. That transaction **MUST**
maintain the block record's `dead` byte count ([RFC 6 §2.3](rfc-6-block-metadata.md#2.3%20Block)) and write the
**compaction index** key `BC‖ns‖name` when the ratio crosses the threshold, and
delete it when it falls back, as [§3.5](#3.5%20Finding%20candidates%20costs%20what%20is%20retirable) does for `live` = 0. One that instead
scans block records **MUST** state that the pass costs O(blocks recorded).

**Bounds.** The compactor **MUST** be rate-limited in encoded bytes per second
(`gc.compaction.rate`), runs its transfers on the syncer's fair share
([§7.1](#7.1%20GC%20bounds%20its%20own%20work)), and **MUST** yield to offload: under capacity pressure on any journal
feeding the store it pauses. It holds one target in memory per worker.

The default threshold is open ([§12](#12.%20Open%20questions)).

### 4.5 What compaction races

| Concurrent operation | Why it is safe |
| --- | --- |
| Adoption of a moved chunk | Both transactions read and write that chunk's record, so they serialize. The adoption counts in whichever block the record names when it applies ([RFC 6 §7.3](rfc-6-block-metadata.md#7.3%20Relocation)). |
| Sweep of a source | Retirement reads the source's `live` inside its transaction. It is zero only once the move has committed. |
| Snapshot or restore | Refs name hashes, so a snapshot's refs survive the move. A restore takes locations from the live store, never from its copy ([RFC 6 §7.4](rfc-6-block-metadata.md#7.4%20Restore)). |
| A second compaction of the same sources | Each mints its own target and puts it. A chunk record the first already moved names another block, and the second leaves it alone; its target's `live` counts only what it moved, and a target that moved nothing is born dead ([§3.5](#3.5%20Finding%20candidates%20costs%20what%20is%20retirable)). |
| Trash restore of a source | A source is `live` until sweep retires it, and a restore acts only on `retired`; a restored block is compacted like any other. |

## 5. Unrecorded objects

### 5.1 How they arise

An object no live block record names comes from one of:

- a put whose commit never ran — a crash, or a pass abandoned after the put
  ([RFC 6 §4.2](rfc-6-block-metadata.md#4.2%20Only%20after%20durability)). Its intent still names it;
- a compaction that put and did not commit ([§4.2](#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)). Its intent still names it;
- a put that landed after its intent was abandoned and its delete ran
  ([§3.4](#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete)). Neither intent nor record names it;
- anything written into the namespace by a process this store does not know
  about.

A retirement does not produce one, because [§3.2](#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded) keeps its record until the
delete completes. A re-offer after a restart mints new names, so it never
re-derives one of these ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)).

### 5.2 Collection is housekeeping

An unrecorded object is a leak of storage, not a threat to content: no record
names it, so no read reaches it. Collecting it is worth running but not required
for correctness ([RFC 4 §4.6](rfc-4-remote-tier.md#4.6%20List%20is%20a%20complete%2C%20resumable%20walk)). An implementation **MUST NOT** depend on
collection for correctness, and a deployment that never runs it **MUST** only
leak.

Collection has two sources:

- **Abandoned intents — the primary source.** An intent whose epoch is
  superseded — its writer lost ownership, or its process restarted under a new
  epoch — names its object exactly. GC abandons it ([RFC 6 §7.6](rfc-6-block-metadata.md#7.6%20Put%20intents)): one
  transaction deletes the intent and writes the name's block record in state
  `deleted`, and the deleter deletes the object at once, with no trash
  ([§3.7](#3.7%20Trash)). A live owner that gives up an attempt abandons that attempt's intent
  itself, at once, through the same transaction ([RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline), O4), so a
  long-lived owner leaves no intent for GC to wait on. This needs no listing and
  no proof of the namespace: the intent is this store's own record of the name. A
  crash never leaks an object this way.
- **A listing — a rare backstop** for the last two cases of [§5.1](#5.1%20How%20they%20arise), which no
  record names. It runs only under [§5.3](#5.3%20It%20runs%20only%20where%20the%20namespace%20is%20proven).

### 5.3 It runs only where the namespace is proven

The listing backstop deletes an object because no record names it, which is
exactly the inference [§2.3](#2.3%20The%20absence%20of%20a%20record%20proves%20nothing) forbids unless every store that could name it has
been read. GC **MUST NOT** delete an object found only by listing unless:

- the deployment meets [RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count) — one keyspace partition per namespace, and key
  derivation that includes the namespace ID — and GC can verify it from
  configuration, not assume it;
- every store in the counting domain ([§2.3](#2.3%20The%20absence%20of%20a%20record%20proves%20nothing)) was enumerated completely in this
  pass. A store that failed or was skipped **MUST** stop the backstop for the
  whole namespace.

### 5.4 Age is not the guard

An object may be the put half of a commit still in flight. What protects it is
its intent, not its age:

- **An object named by an intent** is deleted only after the intent is abandoned,
  in a transaction that conflicts with the commit consuming it ([§3.4](#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete)). GC
  abandons only an intent whose epoch is superseded; a live writer's intent is
  never touched by GC, however old. The live writer **MAY** abandon its own
  given-up attempt's intent ([RFC 6 §7.6](rfc-6-block-metadata.md#7.6%20Put%20intents)).
- **An object named by neither an intent nor a block record** is in the final
  state ([§3.4](#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete)): no put of its name can ever commit. The backstop writes its
  block record in state `deleted` in a transaction that finds neither record nor
  intent for the name, then deletes it.

A listed object's modification time is not consulted, not even as an efficiency
filter: a put is always preceded by a durable intent, so an object found with
neither intent nor record can never be committed, however long a stalled writer
takes ([RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model)), and one whose commit is about to land is still named by its
intent. The listing is only the backstop for what no intent names.

## 6. Audit

Counts are sweep's only authority ([§2.1](#2.1%20The%20count%20is%20the%20only%20authority)), so they are checked by
[RFC 6 §7.5](rfc-6-block-metadata.md#7.5%20Audit)'s audit, which GC runs. The audit needs no consistent read of the
whole store: it walks refs and history in bounded reads over ranges of chunk
hashes, and corrects each block's `live` in one transaction.

> [!important] Pending review — audit coverage, two-walk lowering, Lost search
> The audit now has a coverage period and reports it, lowers a count only on two
> agreeing walks, and turns a ref with no chunk record into a trash search.

### 6.1 Coverage

The audit **MUST** cover every chunk record at least once per
`gc.audit.period` (default 7 days), rate-limited by `gc.audit.rate` so that it
stays a background load. It **MUST** report the time since each hash range was
last covered, and a range not covered within the period **MUST** raise a health
condition. A pass cursor lets a restarted walk resume its range ([§7.2](#7.2%20Every%20record%20GC%20stores%20names%20its%20reclamation)).

Each walk also compares the index keys with the block records it reads, as the
check mode of [§7.4](#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt) does, and reports any mismatch.

### 6.2 Corrections

- **Raising a count MAY happen at once**, in any transaction. Raising a count that
  was right only leaks.
- **Lowering a count requires two agreeing walks.** A count is lowered only when
  two consecutive walks computed the same lower value, and the correcting
  transaction finds the chunk record's `stamp` unchanged since before the second
  walk counted it ([RFC 6 §7.5](rfc-6-block-metadata.md#7.5%20Audit)). A changed stamp means re-walk that chunk.
- **A block whose `live`, or any of whose chunks' refcounts, the audit found low
  MUST NOT be retired** until the count is repaired. Suspending the named block
  costs one leaked block per mismatch; retiring on a low count costs the content.

A count underflow met by any transaction schedules a targeted recount of that
record ([RFC 6 §6.3](rfc-6-block-metadata.md#6.3%20Underflow%20is%20corruption%2C%20not%20a%20boundary)), which GC runs like an audit of one record, with the same
suspension until it commits.

### 6.3 A ref with no chunk record is Lost, and trash is searched

A live or history ref whose hash has no chunk record **MUST** be reported as
**Lost**, naming the file and offset. The report **MUST** start a search of the
trash: GC pauses the deleter for the namespace, reads the headers of the `retired`
blocks through the retired index, and restores ([§3.7](#3.7%20Trash)) each block that carries
the hash. The deleter resumes when the search completes. The search costs one
header read per retired block, bounded by the trash's size, and runs only on a
Lost report. A hash found in no retired block stays **Lost**.

The audit decides nothing else, and its scratch state obeys [§7.2](#7.2%20Every%20record%20GC%20stores%20names%20its%20reclamation).

## 7. Bounds, records and scheduling

### 7.1 GC bounds its own work

GC schedules through the shared work scheduler ([RFC 8 §6.1.1](rfc-8-engine.md#6.1.1%20The%20shared%20work%20scheduler)), one instance
per process, with one source each for sweep, the deleter, the compactor and the
audit of every namespace whose lease it holds. The scheduler supplies the
bounds; GC **MUST** configure and state them:

- deletes in flight per process, and the names per delete call;
- the memory held for them;
- the compactor's bytes per second and the audit's refs per second;
- fairness between namespaces, so one namespace's deletion storm does not starve
  another's sweep.

The compactor's transfers are bounded by the syncer as well: they run on GC's
flow ([§4.2](#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)), so the pool's memory bound applies, and fairness, counted in encoded
bytes, keeps compaction from starving the offloads that make content evictable.

The memory a pass holds **MUST NOT** grow with the size of the store. A pass that
needs a set of every referenced hash, in memory or on disk, has become a mark
([§2.4](#2.4%20A%20snapshot%20holds%20its%20blocks%20without%20a%20pin)).

**Deferred work runs one batch at a time per namespace.** The batched drops GC
runs — snapshot history drops ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)), intent abandonment, sweep's retirements,
the deleter's state transitions — and the phase-2 drops of large releases **MUST**
keep at most one sub-transaction in flight per namespace. Each sub-transaction is
already bounded by *K* ([RFC 6 §5.2](rfc-6-block-metadata.md#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed)), so a deletion storm occupies at most one
transaction's worth of the metadata store at a time, and space is reclaimed as
fast as that allows. A user's quota is not what waits: usage drops when the file
is released, not when its chunks are.

> ponytail: one in-flight batch per namespace, paced only by the scheduler's
> fixed rate limit. A batch whose chunks are all hot can still hold contended keys
> for as long as it runs. Add a budget of transaction time per interval that
> yields to client operations when the deletion-storm benchmark (§11.8) shows
> client latency moving past its bound.

### 7.2 Every record GC stores names its reclamation

I7 applies to GC's records as to any other ([RFC 0 §9.1](rfc-0-data-lifecycle.md#9.1%20Records%20and%20their%20reclamation)).

Every row is a key of [RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed), inside the namespace's partition; the
key column is that table's, repeated so a reader of this one need not switch.
**Derived** rows can be dropped and rebuilt from the authoritative ones
([§7.4](#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt)).

| Record | Key | Kind | Maximum size | Reclaimed by |
| --- | --- | --- | --- | --- |
| block record, any state ([§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object)) | `B‖ns‖name` | authoritative | one per recorded block, plus one per block in the trash or awaiting its delete | pruning, after the object's successful delete |
| put intent ([§3.4](#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete)) | `I‖ns‖name` | authoritative | one per put in flight, plus one per put abandoned since the last pass | the commit that consumes it, or its abandonment ([§5.2](#5.2%20Collection%20is%20housekeeping)) |
| zero index ([§3.5](#3.5%20Finding%20candidates%20costs%20what%20is%20retirable)) | `BZ‖ns‖name` | derived | one per `live` block at `live` = 0 | the transaction that retires the block or moves `live` off zero |
| retired index ([§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object)) | `BR‖ns‖not_before‖name` | derived | one per block in the trash | the deleter's move to `deleted`, or a restore |
| deleted index ([§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object)) | `BD‖ns‖name` | derived | one per name awaiting its delete | pruning |
| compaction index ([§4.4](#4.4%20When%20to%20compact%20is%20policy)) | `BC‖ns‖name` | derived | one per block past the dead-byte threshold | the move that empties the block, a ratio falling back, or its retirement |
| GC lease ([§7.3](#7.3%20GC%20is%20one%20service%20per%20namespace)) | `NS‖ns‖gc‖lease` | authoritative | one per namespace | overwritten by each holder |
| per-pass summary | `NS‖ns‖gc‖summary` | derived | one per namespace, overwritten | the next pass |
| pass cursor | `NS‖ns‖gc‖cursor‖walk` | derived | one per namespace per kind of walk (audit, index rebuild, compaction scan): the last key done | overwritten as the walk advances; deleted when the walk completes. A restarted walk resumes from it instead of starting over |
| audit scratch ([§6](#6.%20Audit)) | `NS‖ns‖gc‖scratch‖walk‖hash` | derived | the stamps and counts of one walk's range of chunk hashes | the end of the walk, on success or failure; after a crash, the restarted walk deletes its walk's scratch before redoing the range from the cursor. A walk runs every pass, so its restart is guaranteed |

A record not in this table **MUST NOT** be added without a row. Where a pass
leaves scratch state on disk, what removes it after a crash is part of the row,
and "the next pass" is an answer only if a pass is guaranteed to run.

### 7.3 GC is one service per namespace

GC is a service of the remote namespace ([§2.3](#2.3%20The%20absence%20of%20a%20record%20proves%20nothing)), not a policy of any share's
engine: one namespace can hold the blocks of many shares, and a pass covers all
of them. One instance runs per namespace, the holder of the namespace's **GC
lease**, a record in the metadata store (`NS‖ns‖gc‖lease`), held by a node with
the `storage` role ([RFC 15](rfc-15-topology.md)); on one node the lease is local. Cadence, triggers,
the trash retention and the compaction threshold are GC's own configuration
([RFC 13](rfc-13-configuration.md)). GC **SHOULD** run a pass periodically, and sooner when the zero
index ([§3.5](#3.5%20Finding%20candidates%20costs%20what%20is%20retirable)) exceeds a configured count.

**A pass starts with `Recheck`** on every store of the namespace
([RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)), which re-reads the service settings that can drift after
open — versioning, lifecycle rules. On drift GC stops the pass, and the syncer
refuses puts and deletes to that store until a later `Recheck` passes.

The lease is for efficiency, not safety. A holder can pause past its lease while
a successor runs, so GC **MUST** be correct with two passes over one namespace at
once, from one process or two ([§2.1](#2.1%20The%20count%20is%20the%20only%20authority)). Removing the lease **MUST NOT** make a
pass unsafe.

A pass **MUST NOT** require quiescence. Writes, offloads, clones, snapshots and
reads continue during it, and [§3.3](#3.3%20The%20race%20with%20adoption%20is%20closed%20by%20transactions%2C%20not%20by%20time), [§3.4](#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete) and [§4.5](#4.5%20What%20compaction%20races) are what make that safe.

### 7.4 The index can be dropped and rebuilt

Every derived row of [§7.2](#7.2%20Every%20record%20GC%20stores%20names%20its%20reclamation) is a function of authoritative ones: the zero,
retired, deleted and compaction keys of the block records' state, `live`,
`not_before`, `dead` and size; the cursors, summary and scratch of nothing that
must survive. Block records, chunk records, refs and intents are authoritative
and **MUST NOT** be dropped by any GC operation. So a corrupted index — a key
missing, extra, or in the wrong state — is repaired by rebuilding it:

1. **Pause** sweep, the deleter and the compactor for the namespace, and wait for
   their in-flight transactions and deletes to finish. Commits, removals and
   reads continue; each keeps maintaining the index as it always does.
2. **Drop** the derived keys by prefix: `BZ‖ns`, `BR‖ns`, `BD‖ns`, `BC‖ns` and
   `NS‖ns‖gc‖` except the lease.
3. **Rebuild** in one pass of bounded reads over `B‖ns‖`, writing per batch, in
   the transaction that re-reads each record: the zero key for a `live` block at
   `live` = 0; the retired key, with the record's own `not_before`, for a
   `retired` one; the deleted key for a `deleted` one; the compaction key for one
   past the threshold. A cursor records progress, so an interrupted rebuild
   resumes from it.
4. **Resume** GC once the pass completes.

A key written by a concurrent commit during the rebuild is the one the rebuild
would write, so the two agree. `not_before` is read from the record, never
recomputed, so no retired block is deleted before its trash expires, during or
after a rebuild; the deleter is paused throughout. The rebuild costs O(blocks
recorded) and is an operator action, not routine: nothing in normal operation
needs it.

**Check mode** runs the same walk without writing and reports every key that is
missing, extra or in the wrong state. The audit runs it as part of every walk
([§6.1](#6.1%20Coverage)).

Both are exposed through the management API ([RFC 23](rfc-index.md)) and its client, for
example `dfsctl gc rebuild-index --namespace <ns>` and
`dfsctl gc rebuild-index --namespace <ns> --check`. A mismatch found by either
increments `dittofs_gc_index_mismatches_total` and emits an event naming the key.

> [!important] Pending review — the index is derived and rebuildable
> Index keys, cursors, summary and scratch are declared derived; an operator can
> drop and rebuild them from block records, or check them without writing.

## 8. API surface

Signatures are indicative; the obligations above are normative. GC declares an
interface for each dependency, named for the need ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)).

```go
// Blocks is what GC needs from block metadata (RFC 6 §7). GC holds one per
// namespace, and every call reads and writes only that namespace's partition.
type Blocks interface {
	// Zero yields blocks the zero index lists. A hint, never an authority (§3.5).
	Zero(ctx context.Context) iter.Seq2[BlockName, error]
	// Retire, per block in one transaction and only if live is zero, deletes the
	// chunk records naming it and moves it to retired with notBefore. One result
	// per name: nil, or ErrLive.
	Retire(ctx context.Context, bs []BlockName, notBefore time.Time) []error
	// Due yields retired blocks whose notBefore is at or before now, in order.
	Due(ctx context.Context, now time.Time) iter.Seq2[BlockName, error]
	// MarkDeleted moves each still-retired, due block to deleted (§3.1 step 2).
	MarkDeleted(ctx context.Context, bs []BlockName, now time.Time) ([]BlockName, error)
	// Deleting yields every block in state deleted.
	Deleting(ctx context.Context) iter.Seq2[BlockName, error]
	// Prune deletes the records of deleted blocks whose object delete succeeded.
	Prune(ctx context.Context, bs []BlockName) error
	// Restore returns a retired block to live from its header's chunk list (§3.7).
	// ErrNotRetired if it is no longer retired.
	Restore(ctx context.Context, b BlockName, chunks []ChunkLoc) error
	// Intend durably records a put intent for a freshly minted target (§4.2).
	Intend(ctx context.Context, name BlockName, epoch uint64) error
	// AbandonedIntents yields intents whose epoch is superseded (§5.2).
	AbandonedIntents(ctx context.Context) iter.Seq2[BlockName, error]
	// Abandon deletes a superseded intent and writes the name's record as deleted,
	// in a transaction that conflicts with a commit consuming it (§3.4).
	Abandon(ctx context.Context, name BlockName) error
	// Unrecorded writes a deleted record for a listed name only if neither a
	// block record nor an intent names it (§5.4).
	Unrecorded(ctx context.Context, name BlockName) error
	// LiveChunks yields b's chunks whose refcount is nonzero.
	LiveChunks(ctx context.Context, b BlockName) iter.Seq2[ChunkLoc, error]
	// CompactionCandidates yields blocks the compaction index lists (§4.4).
	CompactionCandidates(ctx context.Context) iter.Seq2[BlockName, error]
	// Relocate consumes dst's intent, moves chunk records from srcs to dst and
	// moves live, in one transaction (§4.2). ErrNoIntent if the intent is gone.
	Relocate(ctx context.Context, srcs []BlockName, dst NewBlock) error
	// Audit walks refs in bounded reads and yields every mismatch (§6).
	Audit(ctx context.Context) iter.Seq2[Mismatch, error]
	// Recount repairs one chunk's count and its block's live (§6).
	Recount(ctx context.Context, hash Hash) error
	// RebuildIndex drops and rebuilds the derived keys, resuming from its
	// cursor; with check set it writes nothing and yields mismatches (§7.4).
	RebuildIndex(ctx context.Context, check bool) iter.Seq2[Mismatch, error]
}

// Remote is the part of the remote store GC calls directly (RFC 4 §4.1).
type Remote interface {
	Delete(ctx context.Context, names []BlockName) []error // one result per name
	List(ctx context.Context, after BlockName) iter.Seq2[Info, error] // collection only
}

// Transfers is the syncer flow GC opens for the compactor and trash recovery (RFC 3 §1.3).
type Transfers interface {
	Fetch(ctx context.Context, name BlockName, want []ChunkRange) iter.Seq2[Chunk, error]
	Header(ctx context.Context, name BlockName) ([]ChunkLoc, error)
	Upload(ctx context.Context, name BlockName, size int64, src func() iter.Seq2[Chunk, error]) (Stored, error)
}

// Lease is the namespace's GC lease, a metadata-store record (§7.3).
type Lease interface {
	Hold(ctx context.Context, ns NamespaceID, d time.Duration) (bool, error)
}

// Config is GC's own configuration (§7.3, RFC 13).
type Config struct {
	DeleteBatch      int           // names per delete call; default 1,000
	DeletesInFlight  int           // concurrent delete calls per process
	Interval         time.Duration // between passes
	ZeroTrigger      int           // zero-index size that starts a pass early
	TrashRetention   time.Duration // default 48 h (§3.7)
	CompactDeadRatio float64       // dead-byte ratio that makes a block a candidate; 0 turns compaction off (§4.4)
	CompactRate      int64         // encoded bytes per second
	AuditPeriod      time.Duration // default 7 days (§6.1)
	AuditRate        int           // refs per second
	LeaseDuration    time.Duration
}

func New(b Blocks, r Remote, t Transfers, l Lease, s *sched.Scheduler, cfg Config) (*GC, error)

func (g *GC) Sweep(ctx context.Context) (SweepReport, error)
func (g *GC) Drain(ctx context.Context) (DeleteReport, error)
// Compact merges each group of sources into one target (§4.2).
func (g *GC) Compact(ctx context.Context, groups iter.Seq[[]BlockName], reason Reason) (CompactReport, error)
func (g *GC) Restore(ctx context.Context, b BlockName) error
func (g *GC) Collect(ctx context.Context, domain []StoreID) (CollectReport, error)
func (g *GC) Audit(ctx context.Context) (AuditReport, error)
func (g *GC) RebuildIndex(ctx context.Context, check bool) (IndexReport, error)
```

## 9. Invariants

| # | Invariant |
| --- | --- |
| G1 | A remote object is deleted only once its block record is `deleted`: reached from `retired` after a transaction that re-read `live` as zero and after `not_before`, or written by the abandonment of its superseded intent, or, for a listed object, by a transaction that found neither record nor intent. |
| G2 | Nothing but the count keeps content alive or permits a delete: no hold list, no state in process memory, no lease. Elapsed time only postpones a permitted delete. |
| G3 | An object is deleted only when its record is `deleted` and no intent names it, which is final: every name is minted once, every put follows a durable intent, and a commit creates a block record only by consuming that intent. |
| G4 | Every retirement or collection not yet followed by a successful delete is durably recorded in its block record's state, and a restart resumes it from the index. |
| G5 | The compactor deletes nothing. It moves chunk records and counts, and sweep and the deleter delete. |
| G6 | A block GC writes is named like any other: minted once from a fresh nonce, with its intent recorded before the put. |
| G7 | Collection takes abandoned intents first; its listing backstop deletes only objects with neither intent nor record, only in a namespace whose every store was enumerated completely; correctness never depends on either. |
| G8 | A conflict is retried under I8, and a failed delete never loses its `deleted` record. |
| G9 | Every record GC stores has a named reclamation path at its maximum size. |
| G10 | Deferred work — batched drops, abandonment, retirement, the deleter's transitions — keeps at most one sub-transaction in flight per namespace. |
| G11 | A `retired` block can be restored until the deleter marks it `deleted`, and not after; the two transitions conflict on the block record. |
| G12 | Every index key is derived from block records; dropping and rebuilding the index changes no block record and deletes nothing early. |
| G13 | The audit covers every chunk record within its period; it raises counts at once, lowers them only on two agreeing walks, and reports a ref with no chunk record as Lost. |

## 10. Observability

Every metric is labelled by namespace. Per-block outcomes are metrics, not log
lines.

| Answers | Metric | Type |
| --- | --- | --- |
| retirements, labelled `result` = `retired` or `refused` | `dittofs_gc_retirements_total` | counter |
| blocks in the trash, and their bytes | `dittofs_gc_trash_blocks`, `dittofs_gc_trash_bytes` | gauge |
| blocks `deleted` and not yet pruned; with the next row, whether the backlog drains | `dittofs_gc_pending_deletions` | gauge |
| age past `not_before` of the oldest due or `deleted` block | `dittofs_gc_pending_deletion_oldest_seconds` | gauge |
| delete results per name, labelled `result` = `ok` or the error of [RFC 4 §4.8](rfc-4-remote-tier.md#4.8%20Errors%20are%20a%20closed%20set) | `dittofs_gc_deletes_total` | counter |
| sweep lag: time from `live` reaching zero to retirement | `dittofs_gc_sweep_lag_seconds` | histogram |
| trash restores, labelled `cause` = `audit` or `operator` and `result` | `dittofs_gc_restores_total` | counter |
| compactions, labelled `reason` = `dead_bytes` or `retirement` and `result` = `moved`, `born_dead` or `no_intent` | `dittofs_gc_compactions_total` | counter |
| encoded bytes read and written by the compactor | `dittofs_gc_compacted_bytes_total` | counter |
| space amplification: remote bytes over referenced bytes | `dittofs_gc_space_amplification_ratio` | gauge |
| audit mismatches, labelled `record` = `chunk` or `block` and `direction` = `high`, `low` or `lost`. Any `low` or `lost` is an alert | `dittofs_gc_audit_mismatches_total` | counter |
| time since each hash range was last audited; its maximum against the period | `dittofs_gc_audit_coverage_age_seconds` | gauge |
| index keys missing, extra or in the wrong state, labelled `source` = `audit`, `check` or `rebuild` | `dittofs_gc_index_mismatches_total` | counter |
| collection outcomes, labelled `source` = `intent` or `listing` and `result` = `deleted` or `refused` | `dittofs_gc_collection_total` | counter |
| put intents older than one pass interval; one that only grows means abandonment stopped | `dittofs_gc_intents_stale` | gauge |
| work waiting in the scheduler, by `source` | `dittofs_gc_deferred_backlog` | gauge |
| retirement, move and restore conflicts retried | `dittofs_gc_conflicts_total` | counter |
| whether this process holds the namespace's GC lease | `dittofs_gc_lease_held` | gauge |
| pass duration, labelled `op` = `sweep`, `delete`, `compact`, `collect`, `audit` or `rebuild` | `dittofs_gc_pass_seconds` | histogram |

Logs: a low count or a Lost ref logs the block or file at `Error`. A deletion
backlog that stops draining raises a health condition and logs once at `Warn` on
entry and on exit. A collection refused because a store was not enumerated logs
that store at `Warn`. Taking or losing the GC lease, a restore and an index
rebuild log at `Info`.

## 11. Test plan and benchmarks

The set's test rules apply ([Test tiers](rfc-index.md#Test%20tiers)). Every check
runs against every metadata backend, and every Group A check runs against a
remote backend that can fail a delete after performing it ([RFC 4 §7.1](rfc-4-remote-tier.md#7.1%20Conformance%20suite)).

> [!important] Pending review — expanded test plan
> Adds a model-based group, crash at every state transition of sweep, deleter
> and compactor, store fault injection, index rebuild checks, and scale and soak
> benchmarks.

### 11.1 Group A — deleting referenced content

| Requirement | Check |
| --- | --- |
| [§3.3](#3.3%20The%20race%20with%20adoption%20is%20closed%20by%20transactions%2C%20not%20by%20time) adoption race | Interleave `Retire` and an adopting commit at every step boundary, in both orders. Assert that either the block survives with the new ref, or the commit fails and the retry uploads. Assert that no ref ever names a chunk whose object is gone. |
| [§3.3](#3.3%20The%20race%20with%20adoption%20is%20closed%20by%20transactions%2C%20not%20by%20time) isolation | Run the interleaving on each backend at its configured isolation level, with the conditions forced into separate statements. Assert the check fails, so the rig can see the defect it guards. |
| [§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object) order | Fail the metadata write after the delete, then adopt the chunk. Assert the adoption fails. A rig that only crashes between steps misses the failure that needs no crash. |
| [§3.4](#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete) intents | For each writer (an offload, a compaction) and each deleter (a resumed deletion, an abandonment, the listing backstop), run the intent write, the put, the abandonment, the delete and the commit in every order. Assert no `live` block record ever names a deleted object, a commit whose intent was abandoned fails, and a put landing after its delete leaves an object with neither intent nor record. |
| [§3.4](#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete) isolation | Make the commit's and the abandonment's reads of the intent plain snapshot reads. Assert the check fails, so the rig can see the race it guards. |
| [§3.4](#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete) late delete | Delay a delete past a restart and a re-offer of the same content. Assert the re-offer's new name is untouched. |
| [§2.1](#2.1%20The%20count%20is%20the%20only%20authority) no hold | Snapshot a file, delete the file, sweep with no hold provider configured. Assert every block the snapshot names survives. Repeat with the sweep running between the snapshot's metadata capture and its completion. |
| [§2.1](#2.1%20The%20count%20is%20the%20only%20authority) no process state | Run two sweeps against one store from two processes that both believe they hold the GC lease, with an adopting offload in a third. Assert no referenced block is deleted. |
| [§2.1](#2.1%20The%20count%20is%20the%20only%20authority) time never permits | Set the trash retention to zero and stall an adopting commit past it. Assert the retirement still refuses or the adoption still fails; the clock changes nothing but when the object goes. |
| [§3.7](#3.7%20Trash) restore against deleter | Restore a retired block while the deleter marks it due, at every step boundary. Assert either the restore commits and no delete is issued for the name, or the deleter wins and the restore fails `ErrNotRetired`. |
| [§4.2](#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move) name | Compact a fully live block for a retirement; assert the target name is fresh, its intent was recorded before the put and consumed by the move. |
| [§4.2](#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move) transfers | Compact blocks 10% live. Assert the bytes read equal the live chunks' encoded bytes, one put per target, and no multipart or copy request reaches the store. |
| [§4.2](#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move) crash | Crash after the put and before the move; re-run. Assert the re-run mints a new name, the first target is collected from its abandoned intent, and every read resolves. Abandon the intent while the move is in flight; assert the move fails with `ErrNoIntent`. |
| [§4.2](#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move) counts | Drop a moved chunk's refcount to zero between steps 1 and 6. Assert every block's `live` counts only nonzero refcounts. |
| [§4.3](#4.3%20A%20reader%20can%20hold%20the%20old%20location) re-resolution | Compact a chunk twice while a read holds its first location; assert the read returns the right bytes. Make one name miss twice; assert **Lost**. Delete the chunk record; assert **Lost**. |
| [§4.5](#4.5%20What%20compaction%20races) compaction race | Compact a block while adopting one of its chunks and reading another. Assert every read returns the right bytes. Run two compactions of one block at once; assert the same. |
| [§5.3](#5.3%20It%20runs%20only%20where%20the%20namespace%20is%20proven) namespace | Point two stores at one bucket and prefix, run collection from one. Assert it refuses. |
| [§7.3](#7.3%20GC%20is%20one%20service%20per%20namespace) drift | Turn versioning on for the store's bucket between two passes. Assert the next pass stops at its `Recheck` and issues no delete, and puts and deletes resume only after a later `Recheck` passes. |
| [§5.4](#5.4%20Age%20is%20not%20the%20guard) in-flight commit | Put a block, stall its commit for longer than a pass interval, run collection with the writer's epoch still current. Assert the object and its intent survive and the commit succeeds. Supersede the epoch; assert the intent is abandoned, the commit fails and the content is re-offered. |
| [§6.2](#6.2%20Corrections) lowering | Plant a high count and change a ref under the first walk. Assert the count is not lowered until two walks agree with the stamp unchanged. |
| [§6.3](#6.3%20A%20ref%20with%20no%20chunk%20record%20is%20Lost%2C%20and%20trash%20is%20searched) Lost search | Plant an uncounted ref, let sweep retire its block, run the audit inside the retention. Assert **Lost** is reported, the deleter pauses, the block is restored and the ref reads back. Repeat past the retention: assert **Lost** stays reported. |

### 11.2 Group B — leaks and stalls

| Requirement | Check |
| --- | --- |
| [§3.2](#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded) resume | Crash after retirement and before the delete, restart, disable listing. Assert the object is deleted once due. |
| [§5.2](#5.2%20Collection%20is%20housekeeping) intents collect crashes | Crash offloads and compactions after their puts, many times, with listing disabled. Assert every orphan is deleted from its abandoned intent and remote bytes return to what the records name. |
| [§3.5](#3.5%20Finding%20candidates%20costs%20what%20is%20retirable) cost | Grow the store with no garbage, sweep. Assert records read per pass do not grow with the store. |
| [§3.5](#3.5%20Finding%20candidates%20costs%20what%20is%20retirable) born dead | Through the real carver, not hand-built records: write content that repeats one chunk, so that two blocks in flight carry it and one adopts; write the same file from two writers so two attempts carry one chunk list; separately, release a file while its pass is in flight. Delete everything and sweep from the zero index only. Assert every block is deleted and remote bytes return to zero. |
| [§3.6](#3.6%20Failures%20resolve%20on%20their%20own) no intervention | Fail every delete until the backlog is reported, then restore the remote. Assert the backlog drains and the health condition clears with no operator action. |
| [§3.6](#3.6%20Failures%20resolve%20on%20their%20own) I8 | Drive retirements and adoptions at one shared chunk. Assert conflicts occur and that none reaches a caller as an error. |
| [§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object) deletes direct | Saturate the syncer's pool with offloads. Assert deletes still complete. Mark the store unhealthy: assert compaction transfers are refused by the flow, and failed deletes stay `deleted`. |
| [§3.7](#3.7%20Trash) retention | Retire blocks at a known time. Assert no delete is issued before `not_before`, every one is issued within one pass after, and abandoned intents are deleted with no delay. |
| [§4.4](#4.4%20When%20to%20compact%20is%20policy) default | With default configuration, churn files until blocks cross the threshold. Assert they are compacted and swept with no configuration change. |
| [§4.4](#4.4%20When%20to%20compact%20is%20policy) candidate cost | Grow the store with no dead bytes, run the compactor. Assert records read per pass do not grow with the store. |
| [§6.1](#6.1%20Coverage) coverage | Run the audit at its rate on a store sized so one period covers it. Assert every chunk record is covered within the period and the coverage gauge says so; halve the rate and assert the health condition. |
| [§7.1](#7.1%20GC%20bounds%20its%20own%20work) deletion storm | Delete a snapshot of a heavily rewritten share and release a 10⁷-ref file while clients run. Assert client p99 latency stays within the budget's bound and the drops still finish. |
| [§7.1](#7.1%20GC%20bounds%20its%20own%20work) bound | Stall the remote at full GC concurrency. Assert in-flight deletes and memory stay within the stated bound, and offload keeps its fair share during compaction. |
| [§7.2](#7.2%20Every%20record%20GC%20stores%20names%20its%20reclamation) I7 | Run many passes with retirements, compactions, restores and failed deletes. Assert each record kind stays within its table row. |
| [§7.3](#7.3%20GC%20is%20one%20service%20per%20namespace) lease | Stop the lease holder mid-pass. Assert another instance takes the lease and drains the due and `deleted` blocks. |

### 11.3 Group C — model-based

A reference model holds, per namespace, every ref, every chunk's refcount, every
block's `live`, `dead` and state, and the set of objects in the store. A driver
generates random sequences of writes, overwrites, truncates, releases, clones,
snapshot cuts and deletions, offload commits including adoptions and born-dead
blocks, sweeps, deleter drains, compactions, restores, audits, index rebuilds,
lease changes and clock advances, and applies each to the system and the model.

| Invariant, checked after every step | Check |
| --- | --- |
| no loss | Every ref, live or history, resolves to a chunk record whose block's object exists and returns the ref's bytes. |
| no leak | After the sequence ends and a full drain past the retention, the store holds exactly the objects of `live` blocks, and no `retired` or `deleted` record remains. |
| counts equal recount | Every refcount equals the refs naming it; every `live` equals its nonzero chunk records; every `dead` equals its dead bytes. |
| index equals derivation | The zero, retired, deleted and compaction keys equal those computed from the block records. |
| state machine | Every block record's state changed only along `live` → `retired` → `deleted`, or `retired` → `live` by a restore. |

Shrink every failing sequence to a minimal one and keep it as a regression case.

### 11.4 Group D — crash at every state transition

Kill the process between every pair of steps below, restart, let GC resume, and
assert the Group C invariants. A rig that injects a crash only at a sleep point
misses the ones inside a batch: each crash point is a hook the implementation
exposes.

| Machine | Crash points |
| --- | --- |
| sweep | before the retirement transaction; after it, before the zero keys of the rest of the batch are read again |
| deleter | after the move to `deleted`, before the delete call; after the delete returns, before pruning; between pruning two names of one batch |
| compactor | after planning; after the intent; after the put; after the move; before sweep takes the sources |
| collection | after abandoning an intent, before its delete; after a listing installs a `deleted` record, before its delete |
| restore | after the header read, before the transaction; after the transaction |
| index rebuild | after the drop; mid-walk at every batch; after the walk, before GC resumes |

### 11.5 Group E — store fault injection

The emulator of [RFC 4 §7.1](rfc-4-remote-tier.md#7.1%20Conformance%20suite) injects each fault while the model-based driver runs.

| Fault | Assert |
| --- | --- |
| a multi-object delete that fails some names and not others | the successes are pruned, the failures stay `deleted` and are retried until they succeed |
| throttling on every delete for a period | the scheduler backs off with jitter, no name is lost, the backlog drains after |
| a delete that succeeds but whose reply is lost | the retry reports absent, which is success; the record is pruned once |
| a listing that lags recent puts and deletes | the backstop deletes nothing a record or intent names; a lagged object is collected on a later pass |
| a bucket setting that drifts mid-pass (versioning, a lifecycle rule) | the pass stops at the next `Recheck`, and no delete is issued until one passes |
| a delete landing after a trash restore | impossible by construction: assert no delete call ever carries a name whose record is `live` |
| a ranged read returning the wrong bytes during compaction | verification fails, no move commits, the sources stay |

### 11.6 Group F — the index

| Requirement | Check |
| --- | --- |
| [§7.4](#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt) repair | Snapshot the index. Corrupt it: delete keys, add keys for absent or `live` blocks, move keys to the wrong state, alter a `not_before`. Rebuild. Assert the index equals the snapshot and no delete was issued before any block's `not_before`. |
| [§7.4](#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt) resume | Crash mid-rebuild at several cursors. Assert the restarted rebuild completes from its cursor and the result equals the snapshot. |
| [§7.4](#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt) concurrent writes | Rebuild while commits, releases and adoptions run. Assert the result equals the derivation after they finish. |
| [§7.4](#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt) check | Run check mode on each corruption above. Assert every mismatch is reported, the metric and event fire, and nothing is written. |

### 11.7 What must not stand in

- **A single-process rig MUST NOT stand in for [§2.1](#2.1%20The%20count%20is%20the%20only%20authority).** A guard held in memory
  passes every check run in the process that holds it.
- **Hand-built block and chunk records MUST NOT be the only input to a sweep
  check.** Synthetic records encode the author's model of what the carver
  produces; a leak that exists only for repeated or all-zero content, or only
  when two blocks race, passes every such check. At least one check per group
  carves real content, including repeated and all-zero data.
- **A crash-only rig MUST NOT stand in for [§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object).** The ordering defect needs a
  write that fails and a process that keeps running.
- **A correctness check on surviving data MUST NOT stand in for [§3.3](#3.3%20The%20race%20with%20adoption%20is%20closed%20by%20transactions%2C%20not%20by%20time) or
  [§3.4](#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete).** A run in which the race never interleaves passes with no guard at
  all. Assert that the interleaving happened.
- **A trash long enough to hide a bug MUST NOT stand in for a correct count.**
  Group A runs with the retention at zero as well as at its default.

### 11.8 Benchmarks, scale and soak

Run on the reference box ([Test tiers](rfc-index.md#Test%20tiers)), against a
local emulator of the remote service and the reference metadata backend, except
where a row names the scale tier.

| Benchmark | Measures | Target |
| --- | --- | --- |
| Sweep throughput | blocks retired per second | ≥ 1,000/s |
| Deleter throughput | objects deleted and pruned per second, batches of 1,000 names | ≥ 1,000/s |
| Sweep cost against store size | records read per pass, fixed garbage, 10⁵ to 10⁸ recorded blocks | flat within 10% |
| Sweep lag | p99 of `dittofs_gc_sweep_lag_seconds`, remote healthy | ≤ one pass interval + 60 s |
| Backlog drain | time to empty 10⁶ due deletions after the remote returns | ≤ 10⁶ / deleter throughput, with no operator action |
| Pass memory | peak memory, 10⁵ to 10⁸ recorded blocks | flat within 10% |
| Deletion storm | client p99 while 10⁶ files are released and their blocks swept and deleted, against the same load idle | within 20% of idle |
| Born-dead puts | extra puts and sweeps per 10⁶ commits from duplicate attempts over one chunk list | report |
| Compaction throughput | encoded MB/s read and written | report, against the previous run |
| Compaction under load | offload MB/s and client p99 with the compactor at its rate, against neither running | within 10% of offload's fair share; p99 within 20% |
| Space amplification | remote bytes over referenced bytes under a churn workload, per threshold | report; feeds [§12](#12.%20Open%20questions) item 1 |
| Audit throughput | refs per second at the default rate | report |
| Audit coverage at scale | projected time to cover 2 PB at 4 MiB blocks, from measured throughput | ≤ `gc.audit.period` |
| Index rebuild | seconds per 10⁶ block records | report |
| Soak | 72 h of mixed writes, releases, snapshots, compaction and audit, with Group C's invariants checked hourly and fault injection on | zero invariant failures; memory and backlog flat |

## 12. Open questions

1. **The compactor's default threshold** ([§4.4](#4.4%20When%20to%20compact%20is%20policy)). What settles it is the space
   amplification of a churning workload under each threshold, against the
   transfer cost each spends.
2. **How fast to abandon intents** ([§5.2](#5.2%20Collection%20is%20housekeeping)). An intent is abandonable as soon as
   its epoch is superseded; how long after an epoch change GC waits, to avoid
   abandoning intents a slow former owner is about to fail on anyway, is a
   cost question, not a safety one.
3. **Whether a low count should halt sweep** ([§6](#6.%20Audit)). Suspending only the named
   block is the rule. Whether one low count is evidence enough to distrust the
   rest of the store wants a decision once the audit has run on a real store.
4. **The counting domain under one key scope** ([§2.3](#2.3%20The%20absence%20of%20a%20record%20proves%20nothing)). With one scope for several
   shares, the stores of those shares are one counting domain, which [RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)
   forbids unless they are one store. Which of its two options a deployment takes
   is [RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)'s to settle, and GC's scope follows from it.
5. **Trash retention per namespace or per store** ([§3.7](#3.7%20Trash)). 48 h is proposed for
   every namespace; whether a namespace with a costly store wants less, traded
   against the recovery window, waits for the first measured count defect.

---

## Appendix A — where the current code differs

Descriptive, for the refactor. D1–D4 can delete content a file names; the
migration is not scheduled here, and a difference **MUST NOT** be closed by
amending the requirement.

| # | This document says | The code today |
| --- | --- | --- |
| D1 | Retire, then delete ([§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object)) | deletes the object first, then the block record and the durable marker in separate steps; one transient metadata error leaves records naming a deleted object, which later adoptions reference. Data loss |
| D2 | No state in memory, no time ([§2.1](#2.1%20The%20count%20is%20the%20only%20authority), [§3.3](#3.3%20The%20race%20with%20adoption%20is%20closed%20by%20transactions%2C%20not%20by%20time)) | adoptions after the mark are protected by a process-wide in-memory table, for one hour only |
| D3 | Holders are counted ([§2.4](#2.4%20A%20snapshot%20holds%20its%20blocks%20without%20a%20pin)) | a snapshot is held only once its manifest exists, written after the backup it summarises; a sweep in between can delete its content |
| D4 | Proven namespace ([§5.3](#5.3%20It%20runs%20only%20where%20the%20namespace%20is%20proven)) | orphan reclaim deletes any object no record under one remote configuration names, once older than a grace window; a second server on the same bucket and prefix loses its objects |
| D5 | The count is the authority ([§2.1](#2.1%20The%20count%20is%20the%20only%20authority)) | sweep is decided by a mark over every ref; snapshots and open-unlinked files are extra roots |
| D6 | Conditional retirement ([§3.1](#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object)) | the count is read, decided on and decremented in separate steps under a process-local lock |
| D7 | Underflow fails ([RFC 6 §6.3](rfc-6-block-metadata.md#6.3%20Underflow%20is%20corruption%2C%20not%20a%20boundary)) | the `live` decrement clamps at zero, and the last-chunk path relies on it |
| D8 | Block states and index keys ([§3.2](#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded)) | no block state and no index; an object left by a crash is found only by listing |
| D9 | Intents, not age, guard collection ([§5.4](#5.4%20Age%20is%20not%20the%20guard)) | orphan reclaim is guarded only by age; no intent is recorded before a put |
| D10 | The compactor deletes nothing ([§4.1](#4.1%20A%20block%20that%20is%20mostly%20dead%20pins%20its%20dead%20bytes)) | relocation deletes the old object and its record itself |
| D11 | Mint a name with its nonce in the header, and record its intent ([§4.2](#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)) | relocation generates a random name, carries no nonce in the header and records nothing before the put |
| D12 | Ranged reads of live chunks through a flow, verified by the codec ([§4.2](#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)) | relocation fetches whole objects directly, verifies them and parses the format itself |
| D13 | No lock or lease is a safety input ([§7.3](#7.3%20GC%20is%20one%20service%20per%20namespace)) | the run lock and the per-remote lock are process-local; multi-server operation is unsafe |
| D14 | Declared, not asserted ([§8](#8.%20API%20surface)) | GC imports the metadata layer, takes the remote store's full interface, and finds its dependencies by type assertion |
| D15 | I8 ([§3.6](#3.6%20Failures%20resolve%20on%20their%20own)) | the `live` retry is bounded by an attempt count, with jitter derived from the attempt number |
| D16 | Every block with `live` zero is found ([§3.5](#3.5%20Finding%20candidates%20costs%20what%20is%20retirable)) | the carver can pack one hash into several blocks in flight; the chunk locator is written last-wins and sweep decrements only the block it names, so the other blocks keep a nonzero count with no locator. Only an operator-run reconcile finds them. Leak |
| D17 | Reclamation is reported ([§10](#10.%20Observability)) | a hash held by the in-memory adoption guard is skipped silently; a pass reports nothing swept and no reason, and a dry run counts the hash as freeable |
| D18 | Audit covers every chunk per period and recomputes counts in bounded reads ([§6](#6.%20Audit)) | the audit checks only that every ref has a chunk record |
| D19 | Deferred work, one batch in flight per namespace ([§7.1](#7.1%20GC%20bounds%20its%20own%20work)) | no bound; a pass runs until done |
| D20 | Trash with restore ([§3.7](#3.7%20Trash)) | none; a retired object is deleted in the same pass |
| D21 | Rebuildable index ([§7.4](#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt)) | none |
