---
rfc: 6
title: RFC 6 — block metadata
component: block metadata
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-3-syncer]]"
  - "[[rfc-4-remote-tier]]"
  - "[[rfc-5-transforms]]"
aliases:
  - RFC 6
tags:
  - rfc
---
# RFC 6 — block metadata

**Status:** draft.
**Audience:** anyone changing a metadata backend's content records, or anything
that reads them — the read path, offload, sweep.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code;
[Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists where the code differs, as defects to fix, never
rules to build around.

---

## 1. Purpose

Block metadata is the source of truth for file content: which chunks make up each
file, which block holds each chunk, which blocks exist remotely, and what state
each of them is in. It is the second oracle of [RFC 0 §4.1](rfc-0-data-lifecycle.md#4.1%20The%20two%20oracles), and it answers, for any
offset of any file:

> **Does content exist here, which chunk holds it, which block holds that chunk,
> and is that block durable?**

It also counts references, so that sweep can tell what is safe to delete
([RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep)).

It is a **ledger**, not an observer. Every fact in it was observed by another
component and delivered by the engine — existence by the write path, chunks by
the carver, durability by the syncer. Its job is to record those facts atomically
and answer from them without distortion.

The engine is its caller on the content path: write, offload, read, truncate,
clone. Two other components hold narrow views, wired by whoever composes them
([RFC 8 §2.1](rfc-8-engine.md#2.1%20The%20engine%20is%20the%20composition%20root%2C%20and%20the%20only%20one)): the namespace reads `size`, write-time attributes and allocation
([RFC 7 §2.5](rfc-7-namespace-metadata.md#2.5%20Where%20%60size%60%20lives)), and GC retires blocks, relocates and audits ([§7](#7.%20What%20sweep%20needs%20from%20this%20component)). Releasing an
inode reaches this component only through the engine's `Release`, which drops the
refs and the journal's copy together ([RFC 8 §9.1](rfc-8-engine.md#9.1%20One%20facade%2C%20shaped%20like%20content), [RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)).

### 1.1 Non-goals

Block metadata **MUST NOT**:

- record where bytes sit on local disk, or whether they are local at all — that
  is the journal's question, and residency is computed, not stored
  ([RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function)). It knows which content has a durable remote copy (a carved
  offset, [§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class)); it **MUST NOT** try to know whether that content is also local;
- observe durability — it records reports ([RFC 0 §4.3](rfc-0-data-lifecycle.md#4.3%20Reporting), [RFC 3 §2.7](rfc-3-syncer.md#2.7%20It%20reports%3B%20it%20does%20not%20persist));
- own names, directories, attributes, handles, permissions or locks — those are
  [RFC 7](rfc-7-namespace-metadata.md)'s;
- decide what to offload, evict or sweep — it supplies the atomic operations those
  decisions need and nothing more;
- import another component in this set ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)).

### 1.2 Why it is a separate RFC from the namespace

RFC 7 and this document are one database and two sets of records. Neither writes
the other's records. They are specified separately because their write patterns
differ in kind:

| | Namespace metadata ([RFC 7](rfc-7-namespace-metadata.md)) | Block metadata (this RFC) |
| --- | --- | --- |
| Written by | client operations | client writes *and* background offload |
| Unit | one entry | one extent, one chunk, one block |
| Rate | per operation | per stability point, and per chunk at offload |
| Grows with | directory size | file size |

Both **MUST** live in one physical database, because two rules need a
transaction or a consistent read that spans them: `GETATTR` joins the inode with
the shape record ([RFC 7 §9.1](rfc-7-namespace-metadata.md#9.1%20Attributes%20are%20answers%2C%20not%20caches)), and an unlink that ends an inode's life records its
pending release in the transaction that removes the last entry ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)).
Across two databases each needs a cross-store protocol whose failure modes are the
torn states this document forbids. One database does not require one keyspace: a
backend **MAY** give each side its own tables or key prefix. Splitting them onto
separate servers is open ([§12](#12.%20Open%20questions)).

## 2. The records

Block metadata holds three things — a file's **existence**, its **content map**,
and the **blocks** the content lives in — in six kinds of content record, plus
the records sweep and snapshots need. Each record is keyed by exactly one thing,
answers exactly one question, and none holds a list that grows with its file.

![Three concepts in six records: existence (shape, holes and removals, written by the write path and removals), the content map (refs pointing at chunks by hash, written by the offload commit), and blocks (a live count per remote object, retired by sweep), with the direction each one points](img/rfc4-records.svg)

| Concept | Record | Keyed by | Holds | Answers |
| --- | --- | --- | --- | --- |
| Existence ([§3](#3.%20Existence)) | **Shape** | `FileID` | size, `applied` version, write-time `mtime` and `ctime` | how long is the file, when was it written, and up to which journal version is that recorded? |
| | **Hole** | `(FileID, start)` | end | was this range never written? |
| | **Removal** | `(FileID, version)` | removed range | what did a truncate, deallocate, release or clone remove, and at which version? |
| Content map ([§2.1](#2.1%20Ref)) | **Ref** | `(FileID, offset)` or `(ref set, offset)` | chunk hash, skip, length, content versions | which bytes of which chunk are these? |
| | **Chunk** | chunk hash | block name, position in block, refcount | where is this chunk, and who uses it? |
| Blocks ([§7](#7.%20What%20sweep%20needs%20from%20this%20component)) | **Block** | block name | live-chunk count, encoding generation, encodings | may this remote object be deleted, and how was it written? |
| Snapshots ([§6.5](#6.5%20Who%20owns%20a%20ref)) | **Ref set** | ref set ID | holder count | how many snapshots name this frozen set of refs? |
| Sweep ([§7.6](#7.6%20The%20deletion%20fence)) | **Pending deletion** | block name | — | which retired names still await their remote delete? |
| | **Deletion generation** | slot, a hash of the block name | counter | has a delete of this name completed since a writer checked it? |

Who writes each record:

| | Shape, Hole, Removal | Ref | Chunk | Block | Pending deletion, deletion generation |
| --- | --- | --- | --- | --- | --- |
| Existence commit | writes Shape, Hole | — | — | — | — |
| Offload commit | reads Removal | writes | creates, counts | creates, counts | reads |
| Truncate, deallocate, clone, release | writes | writes | counts | counts | — |
| Retirement, relocation, delete ([§7](#7.%20What%20sweep%20needs%20from%20this%20component)) | — | — | moves, deletes | creates, deletes | writes |

The offload commit writes three records in one transaction because it records one
event — a block became durable — and those are its three consequences ([§4.1](#4.1%20What%20one%20commit%20records)).
Each merge that would remove a record kind moves a cost somewhere this document
forbids: a list that grows with the file (I7), a keyspace the write path and the
offload share ([§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths)), or a location rewritten in every ref on relocation ([§2.5](#2.5%20Refs%20name%20hashes%2C%20never%20blocks)).
The block's `live` is derivable from chunk records; it is kept materialised
because it is the one key retirement and adoption conflict on ([§7.1](#7.1%20Conditional%20retirement)).

### 2.1 Ref

A ref says: *these bytes of this file are that range of that chunk.* It is
[RFC 0](rfc-0-data-lifecycle.md)'s **chunk ref**:

    Ref(file, offset) = { hash, skip, length, oldest, newest }

- `file` is the inode that owns the ref, never a name ([§6.5](#6.5%20Who%20owns%20a%20ref)). A snapshot's
  ref set owns refs the same way, keyed by the ref set in place of the file.
- `offset` is where in the file the ref's bytes begin.
- `hash` names the chunk, never a block ([§2.5](#2.5%20Refs%20name%20hashes%2C%20never%20blocks)). A **zero ref** names no chunk:
  its bytes are all zeros, and it reads as zeros without a fetch ([§3.5](#3.5%20Operations%20that%20make%20holes)).
- `skip` and `length` select `[skip, skip + length)` of the chunk's bytes.
- `oldest` and `newest` bound the journal content versions of what the ref
  describes (below).

To read offset *x*, take the ref with the greatest `offset` ≤ *x*. If
*x* < `offset + length`, the byte at *x* is byte `skip + (x − offset)` of chunk
`hash`. Otherwise no ref covers *x*, and existence says whether it is a hole or
uncarved ([§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class)).

![Refs of two files over three chunks: file f tiled by refs to A, B and C, the ref to B split in two by a deallocation, and file g adopting A, so that A's refcount is 2 and B's is 2](img/rfc4-refs.svg)

A ref **MUST** be able to name a strict sub-range of its chunk, because truncate
narrows the tail of one and deallocating the middle of a chunk leaves two refs to
it with different `skip`. Refs of one file **MUST NOT** overlap: in offset order
they tile the parts of the file that have been carved and committed.

**Why a ref carries content versions.** Every operation the journal stages gets a
version higher than any before it for that file ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)). An offload pass is
offered extents whose versions lie in `[Oldest, Newest]` ([RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload)), and the
commit records that range on every ref the pass writes. It has three uses:
ordering commits ([§4.4](#4.4%20Commits%20for%20one%20file%20apply%20in%20order)), dropping refs a removal covers ([§6.2](#6.2%20Truncation%20and%20deallocation)), and checking
the journal's offloaded bits after a crash ([RFC 1 §9.2](rfc-1-journal.md#9.2%20Offload%20state%20after%20recovery)). The last is why position
alone is not enough:

1. Write version 1 over `[0, 4M)` and offload it. The ref `(f, 0)` records
   `newest = 1`.
2. Write version 2 over `[0, 1M)`. The journal holds it; it is not offloaded yet.
3. Crash and recover. The journal holds version 2 at `[0, 1M)`, and the ref still
   covers `[0, 4M)`.
4. Judged by position, the ref covers `[0, 1M)`, so it is durable, and eviction
   may drop version 2. The next read fetches version 1: an acknowledged write is
   gone, and nothing reports it.

With the versions, `MarkDurable(f, [0, 4M), 1, 1)` leaves `[0, 1M)` unmarked,
because the journal holds version 2 there.

**A ref is its own record.** An implementation **MUST NOT** store a file's refs as
one value, one document or one row holding the list. A list costs a rewrite of
the whole list per commit — O(*N*²) to write a file of *N* chunks — and makes it
a key every offload and writer contend on ([§5](#5.%20Write%20sets)). A backend that holds a list at
all **MUST** satisfy I7 ([RFC 0 §9.1](rfc-0-data-lifecycle.md#9.1%20Records%20and%20their%20reclamation)) at the list's worst-case encoded size.

### 2.2 Chunk

    Chunk(hash) = { block, position, length, refcount }

A chunk is keyed by its content hash and by nothing else ([RFC 2 §4](rfc-2-carver.md#4.%20Identity)). There is
**one chunk record per hash** in a namespace ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)), however many files and
offsets use it.

`block` and `position` locate the chunk's bytes: the block that carries it, and
the offset and length of its body in the encoded block, as the block's header
records them ([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout)). They are recorded from the encoder's output at commit,
never computed from local offsets, because transforms change body lengths.
Encoding is deterministic, so every put of one block name writes the same bytes
([RFC 5](rfc-5-transforms.md)); a recorded position is therefore never stale, and nothing rewrites it
except relocation ([§7.3](#7.3%20Relocation)). A ranged read that fails verification is corrupt.

### 2.3 Block

    Block(name) = { live, generation, encodings }

`live` is the number of chunk records that name this block and whose refcount is
nonzero. A chunk this block carries whose record names another block is dead
weight here and is not counted ([§4.1](#4.1%20What%20one%20commit%20records)). A block is sweepable when, and only when,
`live` is zero ([RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep)).

`generation` is the block's **encoding generation**, a `uint64` input to its name
([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)): 0 for a block written by offload, and one more than the highest
source generation for a block a relocation wrote ([§7.3](#7.3%20Relocation)). It is recorded so a
relocation can derive the next one, and a re-run after a crash derives the same
name.

`encodings` lists the transform IDs, versions and material IDs the block's bodies
use, as the store reported them at the put ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)). The transform census
reads it; the read path never does.

A block record has **no durability flag**: by [§4.2](#4.2%20Only%20after%20durability) it exists only once its block
is durable.

### 2.4 Shape and holes

    Shape(file)            = { size, applied, mtime, ctime }
    Hole(file, start)      = { end }
    Removal(file, version) = { start, end }

A hole record is one extent `[start, end)` below `size` that was never written,
or was deallocated. A file's holes never overlap or touch — adjacent holes merge
— and a dense file has none. A hole is its own record for the reason a ref is.

`mtime` and `ctime` are the times of the last write, stored here so that a write
touches one record ([RFC 7 §2.5](rfc-7-namespace-metadata.md#2.5%20Where%20%60size%60%20lives)).

`applied` is the newest journal version whose existence change this shape
records. A commit of pending existence advances it, which makes the commit
idempotent and tells recovery where to resume ([§3.4](#3.4%20Ordering%20against%20the%20journal)).

A removal record names a range removed by a truncate (`[new size, ∞)`), a
deallocate, a release (`[0, ∞)`) or a clone's destination, at the **journal
version of that operation** ([§6.2](#6.2%20Truncation%20and%20deallocation)). An offload commit drops any of its refs
whose `newest` is below the version of an overlapping removal: that content was
offered before the removal and is gone. A version, not a clock, because two
operations in one tick look identical; and not the file's latest version, which
every write advances, so every offload would conflict with every write — the
livelock [§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths) forbids.

### 2.5 Refs name hashes, never blocks

A ref **MUST NOT** carry a block key or a position in a block. The indirection is
what lets a chunk move: relocating it rewrites one chunk record ([§7.3](#7.3%20Relocation)). If refs
carried the location, the same move would rewrite every ref of every file sharing
the chunk, and any copy of the refs taken earlier would name a block that no
longer exists.

### 2.6 The scope of a count

A refcount is only as good as the set of refs it counts. If a ref exists that the
count does not include, the count is low, and sweep deletes referenced content.

The records of [§2](#2.%20The%20records) **MUST** therefore be held in one store per remote key
namespace ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)). A deployment **MUST** do one of two things:

- **One store per namespace.** Every share whose blocks can share a remote key
  records its refs in the store that counts them.
- **One namespace per store.** Key derivation includes the store's identity, so
  two stores can never name one object.

Two stores that can name one key, each keeping its own count, are forbidden.

**The absence of a record here MUST NOT be taken as evidence that a remote object
is unreferenced.** It shows only that this store does not reference it. How an
unrecorded object is collected is [RFC 9](rfc-9-gc.md)'s problem.

### 2.7 A file's life, record by record

One file `f`, from create to delete. Block positions ignore framing.

**t0 — create.** `Shape(f)`: size 0, applied 0.

**t1 — write 4 MiB at 0, version v1.** The journal stages the bytes and the
client is acknowledged. No record changes yet: until the stability point the
journal is the authority for this write, and the engine answers `size` from it
([§3.4](#3.4%20Ordering%20against%20the%20journal)).

**t2 — write 1 MiB at 10M, v2, then `COMMIT`.** The stability point commits both
writes' existence in one transaction. The gap before 10M becomes a hole.

| Record | Value |
| --- | --- |
| Shape(f) | size **11M**, applied **2**, mtime and ctime of v2 |
| Hole(f, 4M) | end 10M |

`[0, 4M)` and `[10M, 11M)` are **uncarved**: below `size`, not a hole, no ref
([§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class)). Only the journal holds the bytes, and `size` is the only record that
says they exist. If the journal loses them, a read fails as **Lost**; it does not
return zeros.

**t3 — offload.** The journal offers both extents, `Oldest` v1 and `Newest` v2.
The carver cuts chunk A (4 MiB) and chunk B (1 MiB), the engine packs both into
block K1, finds no record for K1, puts it, and one commit writes:

| Record | Value |
| --- | --- |
| Ref(f, 0) | A, skip 0, length 4M, oldest 1, newest 2 |
| Ref(f, 10M) | B, skip 0, length 1M, oldest 1, newest 2 |
| Chunk(A) | block K1, position `[0, 4M)`, refcount 1 |
| Chunk(B) | block K1, position `[4M, 5M)`, refcount 1 |
| Block(K1) | live 2, generation 0 |

The commit read the file's removals and found none. The journal marks both
extents durable; they are **Resident** and evictable.

**t4 — overwrite 1 MiB at 1M, v3, and `COMMIT`.** Shape's `applied` becomes 3 and
`size` is unchanged. No ref changes. The journal holds v3 at `[1M, 2M)`, newer
than the ref's `newest`, so that extent is **Dirty** again.

**t5 — offload the overwrite.** The carver cuts chunk C into block K2, and the
commit splits the ref around it:

| Record | Value |
| --- | --- |
| Ref(f, 0) | A, skip 0, length **1M**, 1–2 |
| Ref(f, 1M) | **C**, skip 0, length 1M, 3–3 |
| Ref(f, 2M) | A, skip **2M**, length 2M, 1–2 |
| Ref(f, 10M) | B, skip 0, length 1M, 1–2 |
| Chunk(A) | K1, `[0, 4M)`, refcount **2** |
| Chunk(C) | K2, `[0, 1M)`, refcount 1 |
| Block(K2) | live 1 |

A was not rewritten. Its middle MiB is dead weight in K1 that no ref uses.

**t6 — truncate to 3 MiB, v4.** The journal truncates and assigns v4; then one
transaction ([§6.2](#6.2%20Truncation%20and%20deallocation)):

| Record | Change |
| --- | --- |
| Shape(f) | size **3M**, applied **4** |
| Removal(f, 4) | `[3M, ∞)` |
| Hole(f, 4M) | deleted — past the new end of file |
| Ref(f, 2M) | narrowed to A, skip 2M, length **1M** |
| Ref(f, 10M) | deleted, so B's refcount goes 1 → **0** |
| Block(K1) | live 2 → **1** |

A pass that had been offered the 11 MiB file at versions up to 3 and commits now
finds `Removal(f, 4)` and drops its refs that overlap `[3M, ∞)`.

**t7 — delete.** The namespace releases the inode through the engine's `Release`
([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)): the shape is deleted, `Removal(f, 5) = [0, ∞)` is written, and the
three refs are dropped. K1's and K2's `live` reach 0. Sweep's `Retire(K1)` checks
`live == 0` and deletes Block(K1), Chunk(A) and Chunk(B) in one transaction,
records the pending deletion of K1, and only then deletes the object ([§7.1](#7.1%20Conditional%20retirement)).

## 3. Existence

### 3.1 The gap this closes

[RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function) resolves an extent that no chunk covers to **Absent**, and reads it as
zeros. That is right for a hole and wrong for content that was written but not yet
carved. If the journal loses such an extent — a corrupt record, a lost device —
metadata has no chunk, and the system serves zeros for acknowledged data: I1's
violation. **Existence is therefore recorded independently of carving.**

### 3.2 Every offset is in exactly one class

For an offset below `size`, block metadata answers with one of three classes,
disjoint and exhaustive. These are [RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function)'s metadata inputs:

| Class | Meaning | Residency with journal present | Residency with journal absent |
| --- | --- | --- | --- |
| **hole** | in the hole set | — | **Absent** |
| **uncarved** | not a hole, no ref covers it | **Dirty** | **Lost** |
| **carved** | a ref covers it | **Resident** | **Remote** |

An offset at or beyond `size` is past end of file. A zero ref is carved and reads
as zeros without a fetch.

![A file drawn as a strip below its size: a hole from a write beyond EOF, a carved run of refs, an uncarved run written since the last offload, and the three questions metadata answers for each](img/rfc4-offset-classes.svg)

An implementation **MUST** distinguish *hole* from *uncarved*. Deriving holes as
"gaps between refs" classes every uncarved byte as a hole, and every such byte
the journal loses reads back as zeros; it also gives `SEEK_HOLE` wrong answers.

### 3.3 Holes, not written extents

Existence records **holes**, not written extents: an overwrite or an append — almost
every write — then changes nothing but `size`, and a dense file has an empty hole
set however it was written. Holes are still routine: clients send one file's
writes as many concurrent requests that arrive in any order, so a sequential copy
creates and fills holes continuously, and the per-write cost bound of [§5.2](#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed)
applies to them.

### 3.4 Ordering against the journal

An extent leaving the hole set, or `size` growing past it, is a claim that its
bytes exist. That claim becomes durable at the protocol's **stability point** —
`COMMIT`, `fsync`, a stable write, a close where the protocol requires one — not
at every write:

1. The namespace authorises the write ([RFC 7](rfc-7-namespace-metadata.md)).
2. The journal stages the bytes and assigns the write its version ([RFC 1 §3.1](rfc-1-journal.md#3.1%20Write)).
3. The write is acknowledged. Until its existence is committed, **the journal is
   the authority for it**: the engine answers reads, `size`, times and allocation
   by applying the journal's uncommitted operations of the file over these
   records ([RFC 8 §4.1](rfc-8-engine.md#4.1%20The%20facade%20orders%20a%20write%3B%20adapters%20do%20not)).
4. At the stability point, existence is committed — `size` grown, holes shrunk,
   times set, `applied` advanced to the newest version covered — **group-committed
   across files**: one transaction per journal for every file with pending
   existence, not one per write. The stability reply waits for it.

The rules:

- Existence **MUST NOT** be committed for a version the journal does not yet hold
  durably, and a stability reply **MUST NOT** precede the commit that covers it.
- **Recovery re-applies existence from the journal.** Before serving a file after a
  crash, the engine applies to existence every operation the journal holds for it
  above `applied` ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)). This is the only place existence is derived
  from the journal, and it covers only what was never committed. Content whose
  existence was committed and that the journal then loses resolves **Lost** —
  which is what existence is recorded for.
- A write acknowledged but not yet stable whose journal copy is lost disappears
  without trace. That is what an unstable write permits; the server's write
  verifier changes on restart, so clients resend.
- **Truncate, deallocate, release and clone are synchronous**: each commits the
  file's pending existence and its own change in one transaction before it
  returns ([§6.2](#6.2%20Truncation%20and%20deallocation)).
- **An offer covers only committed existence**: the engine commits a file's
  pending existence before capturing an offer of it ([RFC 8 §4.3](rfc-8-engine.md#4.3%20The%20offload%20guard%20is%20narrow)), so no ref
  ever lies outside recorded existence.

### 3.5 Operations that make holes

| Operation | Effect on existence |
| --- | --- |
| Write starting past `size` | `size` grows; `[old size, write offset)` becomes a hole |
| Truncate up | `size` grows; `[old size, new size)` becomes a hole |
| Truncate down | `size` shrinks; holes past it are dropped; a removal is recorded |
| Deallocate | the range becomes a hole; refs over it are dropped or narrowed ([§6](#6.%20Reference%20counting)); a removal is recorded |
| Allocate | none — see below |

**Allocate MUST NOT remove a hole** unless the zeros it promises are staged in
the journal first. Removing a hole claims bytes exist; with nothing staged the
range becomes uncarved and journal-absent — **Lost**. Reporting allocation to
`SEEK_DATA` is RFC 7's, and **MUST NOT** be done by editing existence.

**All-zero chunks are not stored.** The engine commits a zero ref for a chunk whose
bytes are all zeros ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Blocks%20are%20assembled%20here%2C%20as%20a%20fold%20over%20the%20carver%27s%20output)): it names no chunk, carries versions like any
ref, is ordered and dropped by the same rules, and is reported to `SEEK_HOLE` as a
hole. No zero chunk is stored or counted, so the commonest chunk of any deployment
is never a hot refcount ([§5.3](#5.3%20Hot%20records%20that%20are%20not%20per-file)). A zero ref is written by the offload commit,
not the write path, so it keeps [§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths)'s write sets disjoint.

## 4. The offload commit

### 4.1 What one commit records

An offload pass ([RFC 0 §5.2](rfc-0-data-lifecycle.md#5.2%20Offload)) ends in one commit per block. A commit records, in
**one transaction**:

- the block record, with its generation and encodings ([§2.3](#2.3%20Block));
- a chunk record for each chunk the block carries **that has none yet**, naming
  this block;
- the refs for the extents the pass carved, replacing what they overlap;
- the refcount and `live` changes those refs imply ([§6](#6.%20Reference%20counting)), including for chunks
  the pass adopted from earlier blocks.

Partial application **MUST NOT** be observable: a ref without its chunk, a
refcount without its ref, or a chunk without its block each turns a later read or
sweep into a guess.

**The first block to commit a chunk owns its record.** A later commit that carries
the same chunk adopts it: it adds its refs and their counts, and leaves `block`
and `position` as they are. The copy the later block carries is dead weight, not
counted in its `live`.

**A commit is idempotent per block name.** A block record may already exist: two
passes in flight carried identical chunk lists, or the writer's pre-put check
found the block and skipped the put ([§7.6](#7.6%20The%20deletion%20fence)). A commit that finds the block
record present **MUST NOT** overwrite `live`: it adds to `live` only for the chunk
records it creates, and applies its refs by the rules below.

**The commit checks, per file and inside its transaction:**

- **the owner epoch.** Each file's share of the commit carries the owner epoch the
  pass ran under; the commit **MUST** fail for that file if the epoch is no longer
  current, read so that it conflicts with a concurrent epoch change under the
  backend's isolation level ([RFC 11 §8](rfc-11-ownership.md#8.%20Metadata%20consistency)). Existence commits and removals carry
  and check it the same way;
- **the file's removals**: a ref overlapping a removal of higher version than the
  ref's `newest` is dropped ([§6.2](#6.2%20Truncation%20and%20deallocation)); a file with no shape — released — has all
  its refs dropped ([§6.4](#6.4%20Delete));
- **the deletion fence** for the block name ([§7.6](#7.6%20The%20deletion%20fence)): the commit fails if a
  pending deletion exists for the name or its deletion generation changed since
  the writer's pre-put check.

A dropped ref leaves the rest of the commit to apply. The block is durable either
way; a chunk it carries only for dropped refs is dead weight.

### 4.2 Only after durability

A commit **MUST NOT** run before the syncer has reported the block durable
([RFC 3 §2.6](rfc-3-syncer.md#2.6%20Durability%20is%20observed%2C%20never%20inferred)), or the pre-put check found it recorded. Chunk and block records are
therefore records of durable content, and no record says "this chunk exists but
its block might not". Recording refs before the put would let a crash leave refs
to a block never written. A share with no remote tier never commits: its content
stays uncarved and **Dirty** for its lifetime ([RFC 0 §8.1](rfc-0-data-lifecycle.md#8.1%20Evict)).

### 4.3 The commit is the report's return edge

The extents a commit covers are the extents the offload callback returns as
durable ([RFC 0 §5.2](rfc-0-data-lifecycle.md#5.2%20Offload)). The callback **MUST NOT** return an extent whose commit has not
succeeded, so an offloaded bit is never set for content that metadata does not
hold ([RFC 1 §9.2](rfc-1-journal.md#9.2%20Offload%20state%20after%20recovery)).

### 4.4 Commits for one file apply in order

A commit **MUST NOT** replace refs written by a commit of content offered later.

Two passes of one file can be in flight at once — the engine's guard is held
only while capturing an offer and while committing ([RFC 8 §4.3](rfc-8-engine.md#4.3%20The%20offload%20guard%20is%20narrow)) — and they can
commit in either order. If an older pass's refs overwrote a newer pass's, the
journal, having marked the newer bytes durable, may release them, and a later read
fetches the older content. For each ref it would replace, a commit compares the
new ref's `newest` with the existing ref's range:

| New `newest` | The commit |
| --- | --- |
| above the existing `newest` | replaces the ref |
| inside the existing `[oldest, newest]` | treats the content as already committed: writes nothing for that ref and reports its extent durable |
| below the existing `oldest` | refuses that ref, and applies the rest |

Only strictly older content is refused. This suffices: content at an offset that
differs between two passes was written after the earlier pass was offered, so its
version, and the later pass's `newest`, exceeds every version the earlier pass
holds.

## 5. Write sets

### 5.1 No record is written by both paths

The write path — existence commits — and the offload commit **MUST NOT** write a
common record. [§2](#2.%20The%20records) tabulates who writes what.

![Which paths write which records, and the one shared per-file key the rule removes: a writer streaming appends and an offload committing chunks, retrying against each other on one record](img/rfc4-write-sets.svg)

A record written by both is a conflict between a client stream and a background
pass on every offload. Under optimistic concurrency the pass retries, re-reads the
record the client is still writing, conflicts again, and never commits. Nothing
becomes evictable, the journal fills, and writes are refused. That is livelock,
and backoff does not fix it, because the collision comes from the structure.

The offload commit *reads* the file's removals and shape existence. Removals'
only writers are the removal operations, so that read conflicts only with the
operations it must conflict with. `size` and `mtime` belong to the write path;
the offload commit **MUST NOT** touch them.

### 5.2 Cost per commit is bounded by what changed

An offload commit **MUST** write O(refs replaced + chunks committed + blocks
committed) records, and **MUST** read no more than O(log *n*) records per ref it
replaces, where *n* is the number of refs in the file. An existence commit
**MUST** write O(holes changed) records per file it covers. No per-commit cost may
grow with the size of the file: a commit that is O(file) makes a file of *N*
chunks O(*N*²) to write.

### 5.3 Hot records that are not per-file

[§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths) removes the per-file hot record. Others remain:

- **A popular chunk's refcount.** Every file that references a common chunk
  increments one record. Zero chunks are not counted ([§3.5](#3.5%20Operations%20that%20make%20holes)), which removes
  the worst case; snapshots count once per ref set ([§6.5](#6.5%20Who%20owns%20a%20ref)).
- **A popular block's `live`.** Moves only when a refcount crosses zero.
- **Usage accounting.** A per-owner byte or quota counter is one record shared by
  all of that owner's files. It belongs to the write path and RFC 7, and the
  offload commit **MUST NOT** update it.

An implementation **SHOULD** measure the first under a deduplicating workload
before shipping.

## 6. Reference counting

### 6.1 A refcount is exactly its refs

A chunk's refcount **MUST** equal the number of file refs naming it plus the
number of ref sets naming it ([§6.5](#6.5%20Who%20owns%20a%20ref)), at every commit point. It **MUST** change
in the same transaction as the refs that change it — never in a second
transaction a crash can separate from the first. A block's `live` **MUST** move in
the same transaction as every refcount crossing between zero and nonzero, for a
chunk that block carries.

A refcount that drifts high leaks a chunk forever. One that drifts low lets sweep
delete a chunk that is still referenced — I3's violation, and the only failure in
this system that destroys the last copy of content ([RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep)).

### 6.2 Truncation and deallocation

Truncate down, deallocate, release ([§6.4](#6.4%20Delete)) and a clone's destination ([§6.6](#6.6%20Clone%20and%20server-side%20copy))
are **removals**. Under the file's guard ([RFC 8 §4.3](rfc-8-engine.md#4.3%20The%20offload%20guard%20is%20narrow)), the journal removes the
range first and assigns the removal its version *v* ([RFC 1 §3.6](rfc-1-journal.md#3.6%20Truncate%2C%20deallocate%20and%20delete)); then one
transaction:

- commits the file's pending existence ([§3.4](#3.4%20Ordering%20against%20the%20journal));
- drops the refs wholly inside the removed range, and decrements their chunks;
- narrows a ref that straddles its edge, by adjusting `skip` or `length`;
- splits a ref that spans a deallocated range into two refs to the same chunk,
  and increments that chunk;
- updates existence ([§3.5](#3.5%20Operations%20that%20make%20holes)), sets `applied` to *v*, and writes
  `Removal(file, v)` for the range.

A crash between the journal step and the transaction is recovered like any
uncommitted operation: the journal's record of the removal ([RFC 1 §3.6](rfc-1-journal.md#3.6%20Truncate%2C%20deallocate%20and%20delete)) lies above
`applied`, and recovery re-applies it ([§3.4](#3.4%20Ordering%20against%20the%20journal)).

**A transfer survives a removal under it.** A pass offered before the removal
keeps uploading from the bytes it was offered. Its commit drops each ref whose
`newest` is below an overlapping removal's version and applies the rest. Without
that check, a pass that carved `[0, 10 MiB)` commits refs after a concurrent
truncate to 5 MiB, and a later truncate up turns them back into readable content
where the user was promised zeros. A dropped extent is not reported durable, and
what the journal still holds there is offered again.

**Removal records are pruned by the file's owner.** A removal matters only while
a pass offered before it can still commit, so the owner deletes a file's removals
at or below the file's **durable floor**: the lowest `Newest` of its passes in
flight, or every removal when none is in flight, as at startup ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)). No other
process prunes them: only the owner can see its passes, and a stale owner's
commits fail on the epoch ([§4.1](#4.1%20What%20one%20commit%20records)).

### 6.3 Underflow is corruption, not a boundary

A decrement that would take a refcount, a holder count or `live` below zero
**MUST** fail the transaction and **MUST** be reported as a consistency error
naming the record. It **MUST NOT** clamp at zero: a count about to go negative was
already too low, so something else still references the chunk.

### 6.4 Delete

Releasing a file deletes its shape, writes `Removal(file, v) = [0, ∞)` at the
journal's delete version, and drops all its refs, decrementing their chunks
([RFC 0 §7](rfc-0-data-lifecycle.md#7.%20Mutation%20and%20removal)). It carries and checks the owner epoch like any commit. An offload
commit for a file with no shape drops that file's refs.

Dropping the refs **MAY** be deferred past the release, and **MUST** then leak
rather than lose:

- the removal record is the durable record of the pending drop: a restart finds
  shapeless files by it and resumes, without a full scan. It is pruned only once
  the file has no refs left and no pass in flight;
- a refcount decremented before its ref is removed is a sweep hazard, and **MUST
  NOT** happen.

### 6.5 Who owns a ref

Refs belong to an **inode**, not to a name. Hard links, renames, trash and a file
unlinked while open are namespace states of one inode, and none changes a ref.
Block metadata learns of a deletion only when [RFC 7](rfc-7-namespace-metadata.md) releases the inode through
the engine's `Release`.

A **snapshot** that can be restored holds counted content, but it is counted
**once per ref set**, not once per ref per snapshot:

- Taking a snapshot of a file freezes its refs as a **ref set**: refs keyed by
  `(ref set, offset)`, holding one count on each distinct chunk they name.
- A snapshot names ref sets, and a ref set's holder count is the number of
  snapshots naming it. A later snapshot of a file whose refs have not changed
  since **MAY** name the same ref set, incrementing only its holder count.
- When the holder count reaches zero, the ref set is released as a file is
  ([§6.4](#6.4%20Delete)).

Nothing else keeps content alive: a hold list or pin set consulted beside the
count fails open ([RFC 9 §2.1](rfc-9-gc.md#2.1%20The%20count%20is%20the%20only%20authority)). Any future holder of content holds refs and is
counted.

### 6.6 Clone and server-side copy

Cloning carved content copies refs, not bytes. A clone is a **removal of the
destination range and an adoption**, under the destination's guard: the journal
removes the destination range at version *v*, then one transaction

- removes the destination range as [§6.2](#6.2%20Truncation%20and%20deallocation) does, writing `Removal(dst, v)`;
- writes the cloned refs **re-versioned** with `oldest = newest = v`, and
  increments their chunks — failing if any chunk has been retired ([§7.2](#7.2%20Adoption%20is%20conditional%20on%20existence));
- records the destination's existence over the range.

Re-versioning makes the cloned refs outrank anything a destination pass in flight
carries, and the removal drops what that pass would have committed there.

**Uncarved content cannot be cloned by reference**, because no chunk covers it
yet. For the source's uncarved extents, a clone **MUST** either offload the source
first or copy the bytes through the destination's write path. It **MUST NOT**
record the destination range as existing unless its bytes are staged or its refs
written.

## 7. What sweep needs from this component

Sweep and relocation are [RFC 9](rfc-9-gc.md)'s. They need the atomic operations below,
and cannot be made safe without them.

### 7.1 Conditional retirement

    Retire(block) — if live == 0: delete the block record and every chunk record
                    that names it, and write the block's pending deletion; else refuse

A chunk record that names another block is not this block's to delete ([§4.1](#4.1%20What%20one%20commit%20records)).
This **MUST** be one transaction, and the condition **MUST** be evaluated inside
it: a sweep that reads `live`, decides, and deletes in a separate step deletes a
block a commit re-adopted in between.

The pending-deletion record makes the remote delete resumable ([RFC 9 §3.2](rfc-9-gc.md#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded)).
Retiring the records before deleting the object means a crash between the two
leaves an unreferenced object recorded for deletion; the reverse order leaves
records naming an object that no longer exists, which is **Lost** for content
that was durable.

### 7.2 Adoption is conditional on existence

An offload commit that references a chunk it did not carry **MUST** fail if that
chunk's record no longer exists when the commit applies, and **MUST NOT**
recreate it. With [§7.1](#7.1%20Conditional%20retirement) this closes the race without a clock: an adoption before
retirement makes `live` nonzero and retirement refuses; an adoption after it
fails, and the pass retries carrying the chunk. An implementation **MUST NOT**
substitute a grace period for either operation ([RFC 0 §4.3](rfc-0-data-lifecycle.md#4.3%20Reporting)).

### 7.3 Relocation

Relocation rewrites the surviving chunks of one or more blocks into a new block,
so the sources can be retired. When to do it is RFC 9's; this section covers the
record change.

GC reads and puts through a syncer flow of its own ([RFC 9 §4.2](rfc-9-gc.md#4.2%20Read%20verified%2C%20name%20by%20content%2C%20put%2C%20then%20move)). The new
block's generation is one more than the highest source generation, so it never
derives a source's name; a relocation **MUST** be refused if the target name
equals any source name. Before the put, it runs the pre-put check of [§7.6](#7.6%20The%20deletion%20fence).

After the new block is durable, one transaction:

- points each moved chunk record that still names a source at the new block and
  position;
- creates the new block record, with its generation and encodings, or adds to
  `live` if it exists ([§4.1](#4.1%20What%20one%20commit%20records));
- counts, inside the transaction, the moved chunks whose refcount is nonzero,
  adds that to the new block's `live` and subtracts each source's share from it;
- checks the deletion fence for the new name ([§7.6](#7.6%20The%20deletion%20fence)).

Refs are untouched ([§2.5](#2.5%20Refs%20name%20hashes%2C%20never%20blocks)). A source whose `live` reaches zero is retirable. A
chunk adopted by a racing commit is counted in whichever block its record names
when the adoption applies; both transactions read and write that record, so they
cannot interleave.

### 7.4 Restore

Restoring block metadata from a copy — a snapshot, a backup — is a batch of
adoptions, not a write of old records:

- every chunk a restored ref names **MUST** exist when the restore applies ([§7.2](#7.2%20Adoption%20is%20conditional%20on%20existence));
  a restore that would write a ref to a retired chunk **MUST** fail;
- chunk and block records **MUST** be taken from the live store, never from the
  copy, whose locations may predate a relocation and whose counts describe a
  store that no longer exists;
- refcounts and `live` **MUST** change by the refs the restore adds and removes.

A restore from a counted snapshot cannot fail the first check, because the
snapshot's ref set held its chunks.

### 7.5 Audit

Refcounts are sweep's only authority, so there **MUST** be a way to check them. An
audit recomputes, from one consistent read of the store, at two levels:

- each chunk's refcount from the file refs and ref sets naming it, each ref set
  counted once;
- each ref set's holder count from the snapshots naming it;

and each block's `live` from its chunks, and reports every mismatch.

- A correction **MAY** raise a count to its recomputed value. Raising a count that was
  right only leaks.
- A correction **MUST NOT** lower a count unless the recomputation came from a single
  consistent read and no commit, clone or restore ran between that read and the
  write. A walk that spans concurrent commits can come out low.

### 7.6 The deletion fence

A block name is derived from its content ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)), so an offload or a
relocation can put a name whose earlier object is being deleted. A delete of a
name **MUST NOT** run concurrently with a put of that name whose commit can
succeed ([RFC 9 §3.4](rfc-9-gc.md#3.4%20A%20retired%20key%20is%20not%20re-created%20underneath%20its%20delete)). Two records provide that:

- a **pending-deletion** record per retired name, written by `Retire` or by
  `Unrecorded` (collection of an object no block record names), removed by
  `Deleted` once the remote delete succeeded;
- a fixed array of **deletion generations**, indexed by a hash of the name.
  `Deleted` increments the name's generation in the transaction that removes its
  pending-deletion record.

Every writer runs a **pre-put check** before any put:

    Precheck(name) → recorded | pending deletion | absent, and the name's deletion generation

- **recorded:** the block exists; the writer skips the put and commits, adopting
  it ([§4.1](#4.1%20What%20one%20commit%20records)).
- **pending deletion:** the writer does not commit now; it retries the content in
  a later pass.
- **absent:** the writer puts, then commits carrying the generation it read.

The commit fails if a pending deletion exists for the name or its generation
changed since the check, and the writer retries from the check. A writer whose put
preceded a delete therefore fails its commit and puts again; one that checked
after the increment put after the delete, and is safe. Two writers that miss each
other's check both put the same name; the encoding is deterministic, so they write
the same bytes, and the second put is harmless.

The generation array is read by every commit and written only by `Deleted`, so it
adds no hot record to the offload path.

## 8. Queries

### 8.1 Covering lookup

    Covering(file, offset, n) → the refs, holes and uncarved runs over [offset, offset + n), in order

This is the read path's question, asked per extent the journal does not hold
([RFC 0 §6.1](rfc-0-data-lifecycle.md#6.1%20Resolution)). It **MUST** cost O(log *n* + results) in the number of refs and
holes of that file, and **MUST** be a method on the declared interface
([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)).

### 8.2 Deduplication lookup

    Durable(hash) → chunk | none

Returns the chunk record for `hash`, if any. By [§4.2](#4.2%20Only%20after%20durability) a record implies a durable
block, so this is the complete answer to "may this chunk be referenced rather than
uploaded" ([RFC 2 §5](rfc-2-carver.md#5.%20Packing%3A%20three%20rules%2C%20not%20a%20component)). It **MUST NOT** consult anything that knows about a block
not yet committed. The answer is advisory: [§7.2](#7.2%20Adoption%20is%20conditional%20on%20existence) makes a stale one safe, and it
**MUST NOT** be treated as a reservation.

### 8.3 A file's refs and the version floor

    Refs(file)           → refs in offset order
    VersionFloor(shares) → version

`Refs` lets the engine check the journal's offloaded bits against the refs after
a restart ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)), for the files the journal holds and no others. It
**MUST** cost O(log *n* + results) for that file.

`VersionFloor` is the highest version this store records for any file of the
given shares — every ref's `newest` and every shape's `applied`. The journal is
opened with it, over every share it may serve, so that a journal restored from an
old copy cannot reissue a version metadata already holds ([RFC 1 §9.1](rfc-1-journal.md#9.1%20Rebuilding)). It
**MUST** be answered from an index ordered by version, in one lookup per share,
and **MUST NOT** be a counter record that every commit rewrites, which would be
the hottest record in the store.

## 9. Invariants

| # | Invariant |
| --- | --- |
| M1 | Existence is committed at the stability point, never for bytes the journal does not hold durably, and a hole is distinguishable from uncarved content. |
| M2 | Existence is derived from the journal only at recovery, and only above `applied`. |
| M3 | A chunk or block record exists only for content reported durable or found recorded. |
| M4 | A chunk's refcount equals the file refs and ref sets naming it, and `live` equals the referenced chunk records naming the block, at every commit point. |
| M5 | A count that would go negative fails the transaction. |
| M6 | A block is retired only by a conditional operation that evaluates `live` inside its own transaction and records its pending deletion; retirement deletes only chunk records naming that block. |
| M7 | Adoption of a chunk fails if its record is gone, and never recreates it. |
| M8 | No record is written by both an existence commit and an offload commit. |
| M9 | A commit's cost is bounded by what it changed, not by the file. |
| M10 | A commit never replaces a ref with strictly older content, never applies a ref below an overlapping removal's version, and never applies under a stale owner epoch. |
| M11 | Block metadata records nothing about local placement. |
| M12 | Every holder of content — file, ref set — holds counted refs. Nothing keeps content alive outside the count. |
| M13 | No two stores that can name one remote key keep separate counts, and the absence of a record is never evidence that an object is unreferenced. |
| M14 | Refs name hashes, never blocks. |
| M15 | A restore or clone is an adoption, and never copies a count or a location. |
| M16 | A commit is idempotent per block name, fails under the deletion fence, and a relocation never targets a source's name. |
| M17 | No zero chunk is stored or counted. |

## 10. API surface and observability

### 10.1 Interface

Signatures are indicative; the obligations are normative. Each caller holds only
the view it declares: the engine `Existence` and `Content`, the namespace `Size`,
`Times` and `Allocation`, GC `Blocks` ([RFC 9 §8](rfc-9-gc.md#8.%20API%20surface)).

```go
type Ref struct {
    Offset         int64
    Hash           Hash // zero value: a zero ref (§3.5)
    Skip, Length   int64
    Oldest, Newest Version
}

// FileCommit is one file's share of a block commit.
type FileCommit struct {
    File  FileID
    Epoch uint64 // owner epoch the pass ran under (§4.1)
    Refs  []Ref
}

type BlockCommit struct {
    Name       BlockName
    Generation uint64
    Fence      uint64     // deletion generation read by Precheck (§7.6)
    Encodings  []Encoding // as the store reported them (RFC 5 §5.3)
    Chunks     []ChunkAt  // hash and position of every chunk the block carries
    Files      []FileCommit
}

// CommitResult says, per file, which extents are now durable: applied refs and
// refs found already committed (§4.4). Dropped and refused refs are absent.
type CommitResult map[FileID][]Extent

type Existence interface {
    // Commit applies pending existence for many files in one transaction (§3.4).
    Commit(ctx context.Context, pending []PendingExistence) error
    // Remove applies a truncate, deallocate or release at the journal's version (§6.2, §6.4).
    Remove(ctx context.Context, file FileID, epoch uint64, r Removal, pending PendingExistence) error
    Size(ctx context.Context, file FileID) (int64, error)
    Times(ctx context.Context, file FileID) (mtime, ctime time.Time, err error)
    Allocation(ctx context.Context, file FileID, off int64) (Span, error) // SEEK_DATA / SEEK_HOLE
    Applied(ctx context.Context, file FileID) (Version, error)             // §3.4 recovery
}

type Content interface {
    Precheck(ctx context.Context, name BlockName) (BlockState, uint64, error)      // §7.6
    Commit(ctx context.Context, c BlockCommit) (CommitResult, error)              // §4.1
    PruneRemovals(ctx context.Context, file FileID, atOrBelow Version) error      // §6.2
    Covering(ctx context.Context, file FileID, off, n int64) iter.Seq2[Span, error] // §8.1
    Durable(ctx context.Context, hash Hash) (ChunkAt, bool, error)                // §8.2
    Clone(ctx context.Context, src, dst FileID, epoch uint64, srcOff, dstOff, n int64, v Version) error // §6.6
    Snapshot(ctx context.Context, file FileID) (RefSetID, error)                  // §6.5
    DropSnapshot(ctx context.Context, set RefSetID) error                         // §6.5
    Refs(ctx context.Context, file FileID) iter.Seq2[Ref, error]                  // §8.3
    VersionFloor(ctx context.Context, shares []ShareID) (Version, error)          // §8.3
}

// Blocks is RFC 9 §8's view, satisfied here.
type Blocks interface {
    Candidates(ctx context.Context) iter.Seq2[BlockName, error]                   // live may be zero
    Retire(ctx context.Context, b BlockName) error                                // §7.1
    PendingDeletions(ctx context.Context) iter.Seq2[BlockName, error]             // §7.6
    Deleted(ctx context.Context, names []BlockName) error                         // §7.6
    LiveChunks(ctx context.Context, b BlockName) (gen uint64, chunks iter.Seq2[ChunkLoc, error])
    Relocate(ctx context.Context, src []BlockName, dst NewBlock) error            // §7.3
    Audit(ctx context.Context) iter.Seq2[Mismatch, error]                         // §7.5
    Unrecorded(ctx context.Context, name BlockName) error                         // §7.6
    Census(ctx context.Context) iter.Seq2[EncodingCount, error]                   // RFC 5 §5.3
}

var (
    ErrLive         = errors.New("blockmeta: block still live")       // Retire refused
    ErrChunkRetired = errors.New("blockmeta: adopted chunk retired")  // §7.2
    ErrFenced       = errors.New("blockmeta: name deleted under put") // §7.6
    ErrStaleEpoch   = errors.New("blockmeta: owner epoch not current") // §4.1
    ErrSameName     = errors.New("blockmeta: relocation onto source") // §7.3
    ErrInconsistent = errors.New("blockmeta: count underflow")        // §6.3
)
```

A serialisation conflict is retried inside the call under the caller's deadline
and never returned ([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries)).

### 10.2 Observability

| Answers | Metric | Type |
| --- | --- | --- |
| commits, labelled `result` = `ok`, `adoption_refused`, `fenced`, `stale_epoch` or `error` | `dittofs_blockmeta_commits_total` | counter |
| refs a commit did not apply, labelled `reason` = `removed`, `older`, `released` or `already_committed` | `dittofs_blockmeta_refs_skipped_total` | counter |
| existence commits and the files each covered | `dittofs_blockmeta_existence_commits_total`, `dittofs_blockmeta_existence_files_per_commit` | counter, histogram |
| time per commit, lookup and existence commit, by `op` | `dittofs_blockmeta_op_seconds` | histogram |
| pre-put checks, labelled `result` = `recorded`, `pending` or `absent` | `dittofs_blockmeta_prechecks_total` | counter |
| retirements, labelled `result` = `ok` or `live` | `dittofs_blockmeta_retire_total` | counter |
| underflows ([§6.3](#6.3%20Underflow%20is%20corruption%2C%20not%20a%20boundary)); any nonzero value is an alert | `dittofs_blockmeta_underflow_total` | counter |
| audit mismatches, labelled `kind` = `refcount`, `holders` or `live` and `direction` = `high` or `low` | `dittofs_blockmeta_audit_mismatches_total` | counter |
| conflicts retried, by `op` | `dittofs_blockmeta_conflict_retries_total` | counter |
| removal records held; a value that only grows means pruning stopped | `dittofs_blockmeta_removals` | gauge |

An underflow logs the record at `Error`. An audit mismatch logs the record, its
stored and recomputed counts, at `Error` when low (sweep hazard) and `Warn` when
high (leak). A skipped, refused or fenced commit logs at `Debug`: the counter
says how routine it is.

## 11. Conformance

Every check runs against every backend through one shared conformance suite, in
the tiers and under the rules of the [index](rfc-index.md).

### 11.1 Group A — wrong content, lost content

| Requirement | Check |
| --- | --- |
| [§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class) hole vs uncarved | Write past EOF, commit existence, do not offload, drop the journal's extent. Assert the gap reads zeros and the written range **fails**. A rig that only checks the zeros passes the build that serves zeros for both. |
| [§3.4](#3.4%20Ordering%20against%20the%20journal) recovery replay | Write, crash before the stability point. Assert recovery re-applies existence from the journal and the write reads back. Then commit, crash, drop the journal extent. Assert the read fails as **Lost**. |
| [§3.4](#3.4%20Ordering%20against%20the%20journal) group commit | Write to 64 files, then `COMMIT` one. Assert one transaction covered every file with pending existence. |
| [§3.5](#3.5%20Operations%20that%20make%20holes) allocate | Allocate a range with nothing staged. Assert it reads zeros, not a failure. |
| [§3.5](#3.5%20Operations%20that%20make%20holes) zero chunks | Write and offload an all-zero region. Assert zero refs, no chunk record, no refcount change, `SEEK_HOLE` reports a hole, and reads return zeros with no fetch. |
| [§4.4](#4.4%20Commits%20for%20one%20file%20apply%20in%20order) commit order | Commit B, then A over the same offsets with a lower `newest`. Assert B's refs survive. Commit again with a `newest` inside B's range. Assert nothing changes and the extent is reported durable. |
| [§4.1](#4.1%20What%20one%20commit%20records) idempotent commit | Commit one block name twice with identical chunks, from two files. Assert `live` equals the chunk records naming the block, and both files' refs apply. |
| [§4.1](#4.1%20What%20one%20commit%20records) owner epoch | Commit with an epoch below the file's current one. Assert `ErrStaleEpoch` and no record changed; repeat for an existence commit and a removal. |
| [§6.1](#6.1%20A%20refcount%20is%20exactly%20its%20refs) refcount | Over random interleavings of commit, truncate, deallocate, clone, snapshot and delete, assert after every transaction that each refcount equals its file refs plus ref sets. |
| [§6.2](#6.2%20Truncation%20and%20deallocation) versioned removal | Offer at versions ≤ 3, truncate at 4, commit. Assert refs past the new size are dropped, others apply, and no ref lies past `size`. Then offer, deallocate a range the pass did not carve, commit. Assert every ref applies. |
| [§6.2](#6.2%20Truncation%20and%20deallocation) crash inside a removal | Crash between the journal step and the transaction. Assert recovery applies the removal. |
| [§6.2](#6.2%20Truncation%20and%20deallocation) pruning | With a pass in flight at `Newest` 5, prune. Assert removals above 5 survive and those at or below it are gone. |
| [§6.3](#6.3%20Underflow%20is%20corruption%2C%20not%20a%20boundary) underflow | Force a double decrement. Assert the transaction fails and the count is unchanged. |
| [§6.4](#6.4%20Delete) release | Release a file with a pass in flight, then commit the pass. Assert the pass's refs are dropped, and that a restart resumes a deferred drop. |
| [§6.5](#6.5%20Who%20owns%20a%20ref) snapshot counted once | Snapshot a file twice without change. Assert one ref set, holder count 2, and each chunk's refcount raised by exactly one. Delete the file. Assert the chunks' blocks are not retirable. |
| [§6.6](#6.6%20Clone%20and%20server-side%20copy) clone | Clone over a destination range with a pass in flight. Assert the pass's refs there are dropped and the cloned refs, versioned at the clone's version, survive. Clone uncarved content, drop the source's journal extent. Assert the destination reads the written bytes. |
| [§7.1](#7.1%20Conditional%20retirement), [§7.2](#7.2%20Adoption%20is%20conditional%20on%20existence) sweep race | Interleave `Retire` and an adopting commit in every order. Assert that either the block survives with the new ref, or the commit fails, and never a ref to a retired chunk. |
| [§7.1](#7.1%20Conditional%20retirement) retire only its own | Commit a chunk in K1, commit the same chunk carried in K2, retire K2. Assert the chunk record survives, naming K1. |
| [§7.3](#7.3%20Relocation) relocation | Relocate two blocks into one. Assert no ref changed, every read resolves, the new generation is the higher source's plus one, and the sources are retirable. Relocate onto a source's name. Assert `ErrSameName`. |
| [§7.4](#7.4%20Restore) restore after retire | Take an uncounted copy, retire one of its chunks, restore. Assert the restore fails and wrote nothing. |
| [§7.5](#7.5%20Audit) two-level audit | Corrupt a ref set's holder count, then a chunk's refcount. Assert the audit reports each at its level. |
| [§7.6](#7.6%20The%20deletion%20fence) fence | Precheck, retire and delete the name, then commit. Assert `ErrFenced`. Precheck a pending name. Assert the writer does not commit. Precheck a recorded name. Assert no put and the commit adopts. |
| [§2.6](#2.6%20The%20scope%20of%20a%20count) two stores | Point two stores at one remote namespace. Assert the configuration is refused, or that keys differ. |
| [§8.3](#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor) floor | Commit refs up to version 7 and existence up to 9. Assert `VersionFloor` returns 9. |

### 11.2 Group B — cost

| Requirement | Check |
| --- | --- |
| [§5.2](#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed) amplification | Write a file of *N* chunks for several *N*. Assert records **written** per commit are constant in *N*. A correctness assertion on the refs passes a quadratic implementation. |
| [§3.3](#3.3%20Holes%2C%20not%20written%20extents) out-of-order writes | Write a file of *N* MiB as shuffled 1 MiB writes, for several *N*. Assert no hole records remain and records written per write are constant in *N*. |
| [§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths) write sets | Stream appends to one file while its offload commits. Assert no commit retries because of the writer. A single-writer rig cannot fail this. |
| [§8.1](#8.1%20Covering%20lookup) lookup | Assert records **read** per covering lookup grow at most logarithmically in *N*, counting index iterator steps as well as row loads. |
| [§8.3](#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor) floor index | Assert `VersionFloor` reads O(1) records per share, and that no commit writes a per-share record. |
| [RFC 0 §9.1](rfc-0-data-lifecycle.md#9.1%20Records%20and%20their%20reclamation) (I7) | Store a file's refs across a range of sizes. Assert no stored value reaches the storage engine's inline threshold at the refs' worst-case encoding, not a fixture's. |

### 11.3 Benchmarks and targets

| Benchmark | Measures | Target |
| --- | --- | --- |
| Covering lookup, files of 10^3 to 10^7 refs | p50, p99 latency | p99 ≤ 100 µs at 10^7 refs, within 2× of the 10^3 figure |
| Offload commit of one 64-chunk block, into files of 10^3 to 10^7 refs | records written, p99 latency | records written constant; p99 ≤ 5 ms |
| Existence group commit over 64 files | p99 latency | ≤ 2 ms |
| Appends to one file during its offload | commit retries caused by the writer | zero |
| Snapshot of an unchanged 10^6-ref file | records written | one |
| Consistency-check walk ([§8.3](#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor)) | time per ref | ≤ 2 µs per ref |
| Audit ([§7.5](#7.5%20Audit)) of 10^8 refs | refs/s | ≥ 10^6 refs/s |

## 12. Open questions

1. **Group-commit window** ([§3.4](#3.4%20Ordering%20against%20the%20journal)). Stability points bound the window; how
   much further grouping across a busy journal pays is unmeasured.
2. **Deferred ref drops** ([§6.4](#6.4%20Delete)). Whether a release is ever slow enough to
   justify deferring is unmeasured.
3. **Separate metadata servers.** [§1.2](#1.2%20Why%20it%20is%20a%20separate%20RFC%20from%20the%20namespace) keeps both sides in one database. High
   availability, or data servers apart from a metadata server, may want them on
   separate machines; that needs an answer for the two operations [§1.2](#1.2%20Why%20it%20is%20a%20separate%20RFC%20from%20the%20namespace) names,
   across this document, [RFC 7](rfc-7-namespace-metadata.md) and [RFC 8](rfc-8-engine.md).

## Appendix A — where the current code differs

One line per requirement.

| Requirement | Code today |
| --- | --- |
| [§2.1](#2.1%20Ref) refs are records | a per-file list: segmented on one backend, loaded whole on another |
| [§2.1](#2.1%20Ref), [§4.4](#4.4%20Commits%20for%20one%20file%20apply%20in%20order) refs carry versions | no content versions |
| [§2.2](#2.2%20Chunk) one chunk per hash | chunk rows keyed by file and offset carry the refcount |
| [§2.3](#2.3%20Block) generation and encodings | not recorded; block names are random |
| [§2.6](#2.6%20The%20scope%20of%20a%20count) scope of a count | shares on one remote config share an unnamespaced key space |
| [§3](#3.%20Existence) existence | not recorded: size grown from the journal at startup, holes derived from gaps |
| [§3.5](#3.5%20Operations%20that%20make%20holes) zero chunks | stored and counted like any chunk |
| [§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths) disjoint write sets | every offload commit rewrites the per-file record |
| [§6.1](#6.1%20A%20refcount%20is%20exactly%20its%20refs) refcount is its refs | never incremented; decrements in separate transactions; `live` set once |
| [§6.2](#6.2%20Truncation%20and%20deallocation) versioned removals | none |
| [§6.3](#6.3%20Underflow%20is%20corruption%2C%20not%20a%20boundary) underflow fails | clamped at zero |
| [§6.5](#6.5%20Who%20owns%20a%20ref) the count is the authority | a mark phase with hold lists for snapshots and open files |
| [§6.6](#6.6%20Clone%20and%20server-side%20copy) clone | the refcount increment is missing; local-only clone copies bytes |
| [§7.1](#7.1%20Conditional%20retirement) conditional retirement | read, decide, delete the object, then the record |
| [§7.2](#7.2%20Adoption%20is%20conditional%20on%20existence) no grace period | a grace window and an in-process adoption guard |
| [§7.4](#7.4%20Restore) restore adopts | restore writes the copy's records, counts and locations included |
| [§7.5](#7.5%20Audit) audit | checks only that every ref has a chunk row |
| [§7.6](#7.6%20The%20deletion%20fence) deletion fence | none; unrecorded objects are deleted by age |
| [§8.1](#8.1%20Covering%20lookup) declared, O(log n) | reached by type assertion with a scan fallback |
| [§8.3](#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor) refs check and floor | neither exists |
