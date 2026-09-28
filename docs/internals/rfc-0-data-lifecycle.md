---
rfc: 0
title: "RFC 0 — the data lifecycle"
component: data lifecycle
status: draft
aliases:
  - RFC 0
tags:
  - rfc
---
# RFC 0 — the data lifecycle

**Status:** draft.
**Audience:** anyone implementing or reviewing a storage component.

This is the root of the RFC set. It defines the terms, the data model, the
residency function, the operations and the invariants that no single component
can enforce alone. Every other RFC in the set inherits these and does not
redefine them.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT** and **MAY** are
to be interpreted as in RFC 2119.

---

## 1. Scope

DittoFS stores file content in two tiers. The **journal** is the local tier: it
holds bytes on this machine. The **remote tier** holds them durably elsewhere.
"Journal" names the component throughout this set; where a sentence contrasts
the two sides, "remote tier" is its counterpart.

This document specifies how content moves between them, how its location is
known, and what must remain true throughout.

It does not specify wire protocols, authentication, or the filesystem namespace
beyond its relationship to content.

### 1.1 The component set

The components, what each owns, and the order to read them in are listed in
[the RFC index](rfc-index.md). This document owns the terms, the data model,
residency, the lifecycle, the cross-component invariants and the failure model;
it owns no component's internals.

### 1.2 Component autonomy

A component **MUST NOT** import another component in this set.

Where a component requires a capability it does not own, it **MUST** declare an
interface for that capability in its own package, named for the need rather than
for the provider. The engine supplies an implementation at composition time.
Each component **MUST** build and pass its tests with every such interface
stubbed.

A capability **MUST NOT** be negotiated by type assertion on an interface the
provider does not declare it satisfies. A capability that is absent **MUST**
produce a build failure, not a silent fallback.

> [!note]
> The type-assertion prohibition is specific: an assertion that fails
> yields a working program with silently degraded behaviour — an unindexed
> lookup, a disabled guard — and no test observes it. A declared parameter
> cannot fail this way.

Conformance is checked by a per-component import-graph test.

## 2. Terminology

### 2.1 Entities

**File** — a namespace entry with an identity, attributes, and an ordered list
of chunk references.

**FileAttr** — a file's size, ownership, mode, timestamps and identifiers.

**Chunk** — a run of bytes identified by the BLAKE3-256 hash of its content.
Chunks are content-addressed, reference-counted, and shared: one chunk MAY be
referenced by many files. Boundaries are content-defined (FastCDC), so inserting
bytes early in a file re-cuts only the chunks around the insertion.

Chunk boundaries are sought within a configured size range. The range bounds
where a boundary may be *placed*; it does not bound chunk size absolutely. The
final chunk of a file is emitted whole at whatever size remains, so a file
smaller than the extent minimum is exactly one chunk. **Content MUST NOT be
padded to a chunk size.**

**ChunkRef** — one file's use of one chunk at one offset. A file's content is
fully described by its ordered list of chunk refs. Many refs MAY name one chunk.

**Version** — the content version of a file's bytes: a number that orders every
write and removal of one file, assigned when the operation is staged
([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)). Where two operations cover the same byte, the higher version
wins, whatever order they arrived in. A chunk ref records the versions of the
content it was committed from ([RFC 6 §4.4](rfc-6-block-metadata.md#4.4%20Commits%20for%20one%20file%20apply%20in%20order)).

**Block** — the unit of remote storage: a whole number of chunks in one object,
addressed by one remote key and written by one put. A get retrieves chunks of a
block, or the whole block. A block targets a configured size and MAY exceed it by
at most one chunk; the last block of an offload pass MAY fall short
([RFC 2 §5](rfc-2-carver.md#5.%20Packing%3A%20three%20rules%2C%20not%20a%20component), P2), because **a block boundary is always a chunk
boundary**. A chunk **MUST NOT** span two blocks ([§2.2](#2.2%20How%20a%20file%20relates%20to%20its%20chunks)).

**Segment** — a local append-only file of records, capped at a configured size,
sealed when full and immutable thereafter.

**Record** — one framed run of bytes within a segment, carrying a header with
its identity, length and checksum.

**Extent** — a contiguous `(offset, length)` region of one file's bytes. This is
the only term for that concept: "range", "region" and "span" are not separate
terms and **MUST NOT** be introduced as though they were. An extent is a value,
not a thing that is stored; where a component keeps state about one, it keeps it
*against* the extent.

A **block** and a **segment** are unrelated groupings. A block groups chunks for
remote transfer; a segment groups records for local storage. They are formed by
different processes, at different times, from different inputs. A chunk's block
says nothing about which segment holds its bytes, and a segment's contents say
nothing about any block.

### 2.2 How a file relates to its chunks

A file does not contain its content. It contains an ordered list of references to
chunks, and the chunks are shared.

![A file's ordered chunk refs pointing into a shared, reference-counted chunk pool; two versions of a file share the chunks either side of an edit](img/rfc0-files-and-chunks.svg)

The refs are ordered and belong to the file. The chunks are content-addressed and
belong to nobody: the same chunk is named by every ref whose content hashes to
it, and its refcount is how many refs those are. This is the whole of
deduplication — there is no copying step and no second representation, only a
second ref naming a chunk that already exists.

Because boundaries are content-defined, an edit disturbs only the chunks it
falls inside. Editing the middle of a file re-cuts the middle chunk; the chunks
either side keep their hashes and stay shared. Under fixed-size chunking the
edit would shift every boundary after it, so every subsequent chunk would hash
differently and nothing downstream of the edit would dedup.

#### A chunk is never split across blocks

Chunks are grouped into blocks for transfer. The grouping accumulates chunks
until the running total reaches the configured target, and then **ends at that
chunk's boundary** — so a block is usually a little larger than the target,
never a little different in composition. Only the last block of an offload pass may
be smaller.

![Chunks accumulating to an 8 MiB target: the crossing chunk is included whole, making the block 9.6 MiB, versus the forbidden alternative of cutting that chunk at exactly 8 MiB](img/rfc0-block-packing.svg)

Overshoot is the price of a rule that holds everywhere else. If a chunk could be
cut to make a block an exact size, then:

- its hash would name content living in two blocks, so **one hash would no
  longer be one locator** — and a chunk's hash being its address is what makes
  it findable, shareable and countable at all;
- reading that chunk would require two transfers, and reading it during a
  partial fetch would require knowing it was split;
- a refcount would have to describe parts of a chunk rather than a chunk, so
  "is this chunk still referenced" — the question sweep depends on ([§8.3](#8.3%20Sweep)) — would
  stop having a single answer.

The rule costs at most one chunk of overshoot per block. Breaking it costs the
identity property the entire content-addressed model rests on.

#### Blocks and segments group the same chunks differently

A block is a remote grouping of chunks. A segment is a local file of records.
They are built by different processes, at different times, from different
inputs, and neither can be derived from the other.

![The same four chunks shown twice: boxed into two blocks for remote transfer, and stored in two segments locally in a different order, one segment also holding an unrelated file's bytes](img/rfc0-blocks-vs-segments.svg)

A chunk's block says nothing about which segment holds its bytes, and a
segment's contents say nothing about any block. This is why evicting a block
frees no segment, and why dropping a segment affects chunks belonging to many
blocks and many files.

### 2.3 Operations

| Term | Means |
| --- | --- |
| **write** | stage bytes in the journal and acknowledge the client |
| **sync** | make durable in the journal |
| **offload** | the journal's pass: offer dirty extents, accept durability reports. Not a client's flush (NFS `COMMIT`, SMB `FLUSH`, `fsync`), which only syncs the journal ([RFC 8 §9.4](rfc-8-engine.md#9.4%20Commit%20is%20answered%20by%20the%20journal)) |
| **chunk** | place one content-defined boundary |
| **box** | group chunks into one block |
| **put** / **get** | make durable in the remote tier / retrieve from it |
| **fill** | place retrieved remote bytes into the journal |
| **demand** fetch | a get a reader is waiting on |
| **speculate** | get a block no reader has asked for yet: **read-ahead** (following an observed access pattern) or **pre-warm** (an explicit request over a share or subtree) |
| **evict** | release local bytes that are durable remotely ([§8.1](#8.1%20Evict)) |
| **reclaim** | recover local space without losing content ([§8.2](#8.2%20Reclaim)) |
| **sweep** | delete a remote block that nothing references ([§8.3](#8.3%20Sweep)) |

These words are disjoint and **MUST NOT** be used interchangeably. In
particular, *evict*, *reclaim* and *sweep* differ in what they may destroy:
eviction destroys a local copy, reclamation destroys nothing, and sweep destroys
the last copy.

## 3. Identity

A file carries two identifiers.

| Identifier | Type | Lifetime | Purpose |
| --- | --- | --- | --- |
| `ID` | UUID | for the life of the file | identity. Survives rename, relink, and rewriting of content. The journal keys content by this value. |
| `ObjectID` | 32 bytes | changes with every content change | BLAKE3 Merkle root over the file's chunk hashes in offset order. Whole-file content identity. |

`ID` **MUST** be stable across every namespace operation. `ObjectID` **MUST**
change whenever the file's chunk list changes.

The journal's `FileID` **MUST** be a distinct type constructible only from a
file `ID`, so that a value of another kind cannot reach it by conversion.

`ObjectID` describes content that is complete and stable. It **MUST NOT** be
read as a residency or durability signal.

### 3.1 Deduplication

Two mechanisms operate, at different granularities and for different reasons.

**Chunk deduplication** is the mechanism that avoids storing or transferring
duplicate bytes. A chunk whose hash is already known is referenced rather than
stored again; its refcount increases. It catches partial overlap between files.

**Whole-file deduplication** is an optimisation over the same result. Given a
file's `ObjectID`, an implementation MAY determine that some existing file has
exactly the same chunks in the same order, and reference them without carving,
hashing or resolving chunks individually. Chunk deduplication reaches the same
stored state; whole-file deduplication reaches it with less work.

An implementation **MUST NOT** rely on whole-file deduplication for
correctness. It is an accelerator, and a system that skips it is correct and
slower.

## 4. Residency

### 4.1 The two oracles

Content location is described by two independent sources, each authoritative for
exactly one question.

> The **journal** answers: *do I hold these bytes on this disk, and at what
> offset?*
>
> The **metadata store** answers: *does this extent exist, which chunk covers it,
> which block holds it, and is that block durable remotely?*

The journal **MUST NOT** record whether content exists or whether it was ever
written, and **MUST NOT** be consulted about remote durability — it is not
authoritative for it. It **MAY** record that it has been *told* an extent is
durable, for the two internal purposes [RFC 1 §2](rfc-1-journal.md#2.%20The%20model%20it%20presents) permits: selecting offload
candidates, and refusing an unsafe release. That record is never an answer.

The metadata store **MUST NOT** record where bytes sit on local disk.

**Neither oracle consults the other.** The journal requires no interface from the
metadata store and **MUST NOT** acquire one. An extent it does not hold is reported
as absent, with no judgement about why; resolving that absence is the engine's,
in [§6.1](#6.1%20Resolution). A design in which the journal asks what exists reintroduces the single
oracle this section exists to remove.

### 4.2 The residency function

An extent's residency is not stored. It is computed from the two answers:

| metadata | journal | residency | read behaviour |
| --- | --- | --- | --- |
| no chunk covers the offset | — | **Absent** | zeros |
| chunk exists, block not durable | present | **Dirty** | serve locally |
| chunk exists, block durable | present | **Resident** | serve locally |
| chunk exists, block durable | absent | **Remote** | get, fill, serve |
| chunk exists, block not durable | absent | **Lost** | fail |

![The two oracles and the five states their answers imply, with journal silence shown as the ambiguity a single source cannot resolve](img/rfc0-residency-join.svg)

A block counts as durable only while it can be decoded. A block whose material
is lost for good ([RFC 5 §2.7](rfc-5-transforms.md#2.7%20Failures)) is not durable, so an extent it covers that the
journal no longer holds is **Lost**. Material that is only unavailable is a
failure of the remote tier, not of residency ([§10](#10.%20Failure%20model)).

An implementation **MUST NOT** persist the resolved residency of an extent, and
**MUST NOT** maintain a cache of it that can outlive either input.

The function is total: every combination of the two answers yields exactly one
residency. **Absent** and **Lost** are distinct, and an implementation **MUST**
distinguish them — returning zeros for **Lost** is data loss reported as data.

> [!note]
> The distinction is the reason for the split. A single source cannot
> tell "never written" from "no longer held", because both are the absence of a
> local record. Two sources can, because only one of them is responsible for
> knowing what exists.

### 4.3 Reporting

An extent becomes **Resident** only when the journal is told that the
corresponding block is durable remotely. An implementation **MUST NOT** infer
remote durability from the completion of a transfer, the absence of an error, or
elapsed time. Durability is reported by the component that observed it, to the
component that records it.

The journal's offloaded bit is set in exactly two ways: by a durability report,
from an offload ([§5.2](#5.2%20Offload)) or from reseeding after a restart, and by fill
([§6.2](#6.2%20Fill)), whose bytes came from the remote tier and are therefore durable there
by construction ([RFC 1 §3.4](rfc-1-journal.md#3.4%20Fill)).

## 5. The write path

### 5.1 Write

1. The namespace layer authorises the write and updates size and mtime.
2. The journal stages the bytes and appends a record.
3. The client is acknowledged.

The client **MUST NOT** be acknowledged before the bytes are recoverable from
the journal under the configured durability policy. Content **MUST NOT** be
chunked, hashed or transferred on this path.

The resulting extent is **Dirty**.

### 5.2 Offload

Offload is initiated by policy and driven by the journal, which offers its dirty
extents and accepts a report of what became durable:

```
journal.Offload / OffloadMany(ids, fn)
    │  offers dirty extents, of one file or several
    └──► fn:  carver  — cut chunks
              engine  — group chunks into blocks, across files
              syncer  — put blocks
              metadata — record chunks, refs, blocks, durability
    ◄──── returns the extents now durable remotely
journal marks exactly those extents
```

![The write path: client acknowledged from the journal, then a later offload whose callback carves, puts and records durability, returning the durable extents along the edge that is the only path to Resident](img/rfc0-lifecycle.svg)

The callback returns the extents that became durable. The journal **MUST** mark
exactly those, and **MUST NOT** mark an extent for which no report was received.

> [!note]
> The callback shape exists because the acknowledgement has nowhere to
> live in a linear `write → carve → put` pipeline. The return edge is the only
> path by which an extent becomes **Resident**.

An offload that fails leaves every affected extent **Dirty**, and **MUST** be
retryable without loss. Chunking is deterministic ([RFC 2](rfc-2-carver.md)), so a retry that
offers the same stretch of bytes converges on the same chunk identities. A retry
whose stretch starts elsewhere — an offer cut at a limit, or a partial report that
moved the start of what remains dirty — cuts its first chunks differently until
its boundaries rejoin the earlier ones. Those few chunks are stored again under
new hashes and the earlier ones are left to sweep: a cost in space and transfer,
never in correctness.

## 6. The read path

### 6.1 Resolution

1. The journal is asked for the extent. It returns the bytes it holds, and the
   extents it does not.
2. For each extent it does not hold, metadata is consulted for the covering
   chunk and its block.
3. The residency function ([§4.2](#4.2%20The%20residency%20function)) determines the outcome:
   - **Absent** — the extent is a hole. Return zeros.
   - **Remote** — get the chunks it needs, or the whole block; serve the verified
     bytes; fill ([§6.2](#6.2%20Fill)) if policy says so, without the reply waiting on it.
   - **Lost** — fail. An implementation **MUST NOT** return zeros.

### 6.2 Fill

Fill is the only operation that makes an extent locally present without a client
write. It transitions **Remote → Resident**.

Fill **MUST NOT** overwrite an extent that the journal holds. Retrieved content
reflects a past state, and a concurrent write to the same extent is newer by
definition; the write wins.

Filling is discretionary. An implementation **MAY** decline to fill a retrieved
extent, serving it without retaining it. Declining is appropriate when retaining
would displace content more likely to be read again, including under local
capacity pressure and during large sequential reads.

Whether to fill is policy and belongs to the engine ([RFC 8](rfc-8-engine.md)). The mechanism and
its concurrency safety belong to the journal ([RFC 1](rfc-1-journal.md)).

## 7. Mutation and removal

**Overwrite.** New bytes are staged at the same offsets and become new chunks at
the next offload. Superseded chunk refs are dropped from the file's list. The
chunks themselves persist until their refcounts reach zero.

**Truncate.** Chunk refs beyond the new size are dropped; a ref straddling the
boundary is narrowed. A narrowed ref **MUST NOT** continue to describe content
past the new size.

**Delete.** The namespace entry and all the file's chunk refs are removed, and
each named chunk's refcount is decremented. Deletion **MUST NOT** remove content
from the remote tier; that is sweep ([§8.3](#8.3%20Sweep)), and it is asynchronous.

## 8. Reclamation

Three operations recover space. They differ in what they may destroy, and
**MUST** be kept distinct in both code and discussion.

![Evict, reclaim and sweep side by side, keyed by what each may destroy: one local copy, nothing, and the last copy](img/rfc0-reclamation-scopes.svg)

### 8.1 Evict

Evict releases local bytes whose content is durable in the remote tier,
transitioning **Resident → Remote**.

Evicting an extent that is not durable remotely produces **Lost** and is data
loss. An implementation **MUST NOT** evict a **Dirty** extent. This is the
definition of the operation, not a check applied to it.

Eviction **MUST NOT** modify the remote tier.

Eviction writes nothing to metadata ([RFC 8 §7.1](rfc-8-engine.md#7.1%20Eviction%20is%20chosen%20here%2C%20and%20needs%20no%20new%20record)): residency is computed, so
once the journal stops holding durable content, it resolves as **Remote**. Its
one ordering rule is that local bytes **MUST** be released only after the offload
commit that made them durable is itself durable. A release that precedes it can
leave, after a crash, content whose only copy is gone: **Lost**.

### 8.2 Reclaim

Reclaim recovers local space without changing what content exists. A reclaim pass
**MUST** be content-preserving — it changes where bytes are, never whether they
are.

Reclaim is the umbrella; the mechanisms under it are **repack** (copying live
records out of a sparse segment and unlinking it), retiring a segment that holds
nothing, and removing an unattachable file. [RFC 1 §8.2](rfc-1-journal.md#8.2%20Repack) specifies them.

**"Compaction" is deliberately not used for any of these.** In an LSM that word
names an operation that merges runs and *drops* superseded entries; in a
log-structured queue it names one that keeps only the latest value per key. Both
discard content. Reclaim never does.

### 8.3 Sweep

Sweep deletes blocks from the remote tier. It is the only operation in this
system that deletes content anywhere, and the only one after which content
cannot be recovered.

Safety rests on reference counting, specified in [RFC 6](rfc-6-block-metadata.md):

- a chunk carries the number of chunk refs naming it
- a block carries the number of its chunks whose refcount is nonzero
- a block is sweepable only when that number is zero

A block **MUST NOT** be deleted while any chunk it contains is referenced.

Reference counts are read at different instants from the state they describe. An
implementation **MUST** ensure that content created or referenced after a sweep
began cannot be deleted by that sweep, even when every individual observation
was correct when made. [RFC 9](rfc-9-gc.md) specifies the protocol.

## 9. Invariants

These hold across components. No component can enforce any of them alone.

| # | Invariant |
| --- | --- |
| **I1** | An extent that exists is never read as zeros. Zeros are returned only for **Absent**. |
| **I2** | A **Dirty** extent is never evicted. |
| **I3** | A remote block is never deleted while any chunk it contains is referenced. |
| **I4** | Fill never overwrites content the journal holds. |
| **I5** | Remote durability is reported, never inferred. |
| **I6** | No component imports another component in this set. |
| **I7** | Every stored record has a named reclamation path that holds at the record's maximum size. |
| **I8** | A serialization conflict is retried within the caller's deadline, never surfaced as an I/O error. |

An implementation is conformant when all eight hold under concurrent operation,
across crash and restart, and in every condition in [§10](#10.%20Failure%20model).

Each invariant **MUST** be tested at the component that consumes the data, not
the one that produces it. A test placed beside a producer can pass while the
value is discarded downstream.

### 9.1 Records and their reclamation

I7 binds every component that persists anything, because components sharing a
storage engine share its thresholds and triggers: one component's unbounded
record fills the store another component's records live in. An implementation
**MUST** be able to name what reclaims a stored record's superseded copies, and
the answer **MUST** hold at the record's maximum size, computed from its
worst-case encoding. A record that could cross the engine's threshold for moving
values into a separately reclaimed store **MUST** be bounded below it. Each
metadata RFC applies this to its own records ([RFC 6](rfc-6-block-metadata.md), [RFC 7](rfc-7-namespace-metadata.md)).

The placement index of [RFC 1 §5.2](rfc-1-journal.md#5.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes) is held in memory and never stored, so I7
does not reach it; that RFC bounds it for a different reason.

### 9.2 Conflicts and their retries

I8 binds every component whose store detects write-write conflicts
optimistically. A conflict is the store doing its job, so it **MUST** be retried
and **MUST NOT** reach the caller as an I/O error. The retry **MUST** be bounded
by the caller's deadline, not by a fixed attempt count, and the backoff between
attempts **MUST** be randomised: a backoff computed from the attempt number alone
makes every loser of one conflict collide again on the next attempt.

Conformance drives concurrent writers at one deliberately shared key and asserts
two things together: conflicts **occur**, and **none** reaches the caller.

### 9.3 Where each invariant is tested and observed

An invariant spans components, so its test lives with the component that
consumes the data ([§9](#9.%20Invariants)), and its signal with the component that can see
it break.

| # | Test owned by | Signal owned by |
| --- | --- | --- |
| **I1** | [RFC 8](rfc-8-engine.md): reads of **Lost** and **Remote** extents | RFC 8: reads failed as **Lost**, reads failed because the remote tier is unavailable |
| **I2** | [RFC 1](rfc-1-journal.md): `Release` refuses unmarked extents; RFC 8: eviction choice | RFC 1: dirty bytes against held bytes |
| **I3** | [RFC 9](rfc-9-gc.md): sweep against concurrent reference | RFC 9: blocks swept, deletions refused; [RFC 6](rfc-6-block-metadata.md): count audit mismatches |
| **I4** | RFC 1: `Fill` against a concurrent write | RFC 1: fills refused as older than the file |
| **I5** | RFC 1: marking follows reports only; RFC 8: reports follow durable commits | RFC 1: bytes offered against bytes marked durable |
| **I6** | every RFC: its own import test | the build |
| **I7** | RFC 6, RFC 7, RFC 9: each stored record at its maximum size | the owning RFC: store size under rewrite |
| **I8** | RFC 6, RFC 7: concurrent writers on one key | the owning RFC: conflicts retried, and conflicts surfaced (which must stay zero) |

## 10. Failure model

Every condition below has exactly one specified behaviour.

| Condition | Behaviour |
| --- | --- |
| **Remote tier unavailable** | Writes continue into the journal while capacity allows. No extent becomes **Resident**, so no extent becomes evictable. Reads of **Remote** extents fail; they **MUST NOT** return zeros. |
| **Journal at capacity, remote available** | Evict ([§8.1](#8.1%20Evict)); if nothing is evictable, reclaim ([§8.2](#8.2%20Reclaim)); if everything is **Dirty**, offload it and then evict it. The write is accepted once space is free, or refused at its deadline. |
| **Journal at capacity, remote unavailable** | Refuse the write. Every local extent is **Dirty**, and I2 forbids evicting it, so refusal is the only behaviour that does not lose data. |
| **Metadata unwritable** | A write fails at its namespace update ([§5.1](#5.1%20Write)) and is not acknowledged. Offload fails, so extents stay **Dirty** and the journal fills until [§10.1](#10.1%20Capacity%20is%20a%20bound%2C%20not%20a%20target) refuses writes. Reads continue while metadata is readable. |
| **Crash** | On restart the journal rebuilds its placement index from its segments; a torn tail is truncated to the last record that verifies. Metadata recovers by its backend's own durability. The two recover independently and **MAY** disagree; [§10.2](#10.2%20No%20state%20requires%20intervention%20to%20leave) applies. |
| **Local content corrupt** | The journal drops only the extents backed by records that fail verification ([RFC 1 §9.3](rfc-1-journal.md#9.3%20Torn%20and%20corrupt%20records)). A dropped extent that was durable resolves as **Remote** and is fetched again; one that was **Dirty** resolves as **Lost**. The rest of the segment stays usable and reclaimable. |
| **Material unavailable** | The material provider cannot supply a key ([RFC 5 §2.7](rfc-5-transforms.md#2.7%20Failures)). Behaves as **Remote tier unavailable**: offload cannot encode and reads of **Remote** extents cannot decode. Material lost for good is not this row: its blocks are not durable ([§4.2](#4.2%20The%20residency%20function)). |

### 10.1 Capacity is a bound, not a target

The journal's capacity limit **MUST** be enforced by reservation taken before
a write is accepted. An implementation that tests a counter without reserving
against it permits any number of concurrent writers to pass a single test, and
the limit bounds nothing.

Refusal at the limit is a specified outcome, not a failure of the design. The
alternative — accepting content that cannot be made durable and cannot be
released — has no exit.

### 10.2 No state requires intervention to leave

Every condition in this section **MUST** resolve on its own once the underlying
cause is removed. An implementation **MUST NOT** have a state reachable by
normal operation from which it cannot return without operator action.

In particular: sustained inability to offload **MUST** be reported as a health
condition of the share, and **MUST NOT** be represented only as log output.

Recovery after a crash is required to be *truthful*, not to make the two oracles
agree. A chunk that metadata knows about and whose bytes did not survive is
**Lost**, and reads of it fail. Reconciling the two by assuming agreement
reintroduces the failure this model exists to prevent.

## 11. Open questions

1. **Whole-file deduplication's cost** ([§3.1](#3.1%20Deduplication)) — it requires an index and a
   Merkle computation per file to save work that chunk deduplication performs
   anyway. Whether it earns that on real workloads is unmeasured.
2. **Fill policy** ([§6.2](#6.2%20Fill)) — this document specifies that filling is
   discretionary and names the conditions under which declining is appropriate.
   It does not specify a policy. [RFC 8](rfc-8-engine.md) must, and the right one is unmeasured.
3. **Eviction granularity** ([§8.1](#8.1%20Evict)) — this document constrains eviction by
   durability, not by unit; the unit is [RFC 1](rfc-1-journal.md)'s to choose. The right segment size
   is unmeasured, as is whether per-extent hole punching degrades at scale
   ([RFC 1 §13](rfc-1-journal.md#13.%20Open%20questions)).
4. **What a retried transaction may close over** ([§9.2](#9.2%20Conflicts%20and%20their%20retries)) — a retried closure
   re-runs against state that changed since it was first called. Whether it may
   close over values read before the transaction opened, or must re-read every
   row it modifies, is not settled. One that closes over pre-read state can
   re-propose the decision the conflict was raised to prevent, which delays a
   lost update rather than preventing it.
