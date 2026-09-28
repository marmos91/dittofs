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
**Depends on:** [RFC 0](rfc-0-data-lifecycle.md), for the terms, the residency function and the invariants.
[RFC 2](rfc-2-carver.md) supplies the chunks this component records and [RFC 3](rfc-3-syncer.md) the durability reports.
Nothing here redefines them.
**Audience:** anyone changing a metadata backend's content records, or anything
that reads them — the read path, offload, sweep.

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT** and **MAY** are
to be interpreted as in RFC 2119.

This document specifies behaviour, not the current code. Where the code differs,
[Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists it for the refactor. A difference is a defect to be fixed or
migrated, never a rule for an implementer to build around.

---

## 1. Purpose

Block metadata is the source of truth for file content: which chunks make up each
file, which block holds each chunk, which blocks exist remotely, and what state
each of them is in. It is the second oracle of [RFC 0 §4.1](rfc-0-data-lifecycle.md#4.1%20The%20two%20oracles), and it answers, for any
offset of any file:

> **Does content exist here, which chunk holds it, which block holds that chunk,
> and is that block durable?**

The one fact about content it does not hold is local placement ([§1.1](#1.1%20Non-goals)).

It also counts references, so that sweep can tell what is safe to delete
([RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep)).

It is a **ledger**, not an observer. Every fact in it was observed by another
component and delivered by the engine — existence by the write path, chunks by
the carver, durability by the syncer. Block metadata imports none of them and
observes nothing itself. Its job is to record those facts atomically and answer
from them without distortion.

The engine is its caller on the content path: write, offload, read, truncate,
clone. Two other components hold narrow views of it, which the engine wires at
construction ([RFC 8 §2.1](rfc-8-engine.md#2.1%20The%20engine%20is%20the%20composition%20root%2C%20and%20the%20only%20one)) and which they then call directly: the namespace reads
`size`, write-time attributes and allocation ([RFC 7 §2.5](rfc-7-namespace-metadata.md#2.5%20Where%20%60size%60%20lives)), and sweep retires
blocks and audits counts ([§7](#7.%20What%20sweep%20needs%20from%20this%20component)). Neither holds more than its view. Releasing an
inode's refs is not in the namespace's view: it reaches this component only
through the engine's `Release`, which drops the refs and the journal's copy
together ([RFC 8 §9.1](rfc-8-engine.md#9.1%20One%20facade%2C%20shaped%20like%20content), [RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)).

### 1.1 Non-goals

Block metadata **MUST NOT**:

- record where bytes sit on local disk, or whether they are local at all
  ([RFC 0 §4.1](rfc-0-data-lifecycle.md#4.1%20The%20two%20oracles)) — that is the journal's question, and residency is computed, not
  stored ([RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function)). It does know which content has a durable remote copy: a
  carved offset has one, and an uncarved offset has only the journal ([§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class)).
  What it cannot say is whether a carved offset is *also* held locally —
  **Resident** or **Remote** — and it **MUST NOT** try;
- observe durability — it records reports, and a record with no report behind it
  is a claim nothing verified ([RFC 0 §4.3](rfc-0-data-lifecycle.md#4.3%20Reporting), [RFC 3 §2.7](rfc-3-syncer.md#2.7%20It%20reports%3B%20it%20does%20not%20persist));
- own names, directories, attributes, handles, permissions or locks — the
  filesystem model that the NFS and SMB adapters both translate into, which is
  [RFC 7](rfc-7-namespace-metadata.md)'s. It is not per-adapter state: both protocols share one namespace, and its
  rules are enforced once, below the adapters;
- decide what to offload, evict or sweep — it supplies the atomic operations those
  decisions need ([§7](#7.%20What%20sweep%20needs%20from%20this%20component)) and nothing more;
- import another component in this set ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)).

### 1.2 Why it is a separate RFC from the namespace

RFC 7 and this document are one database and two sets of records. The separation
is logical: each side has its own records and its own interface, and neither
writes the other's records. They are specified separately because their write
patterns are different in kind:

| | Namespace metadata ([RFC 7](rfc-7-namespace-metadata.md)): the filesystem model the adapters speak | Block metadata (this RFC): file content |
| --- | --- | --- |
| Written by | client operations | client writes *and* background offload |
| Unit | one entry | one extent, one chunk, one block |
| Rate | per operation | per write, and per chunk at offload |
| Grows with | directory size | file size |

A design that stores both in one record per file puts a background process and
the client on the same key, and [§5](#5.%20Write%20sets) forbids that.

Both **MUST** live in one physical database, because two rules need a
transaction or a consistent read that spans them. `GETATTR` joins the inode with
the shape record ([RFC 7 §9.1](rfc-7-namespace-metadata.md#9.1%20Attributes%20are%20answers%2C%20not%20caches)), and an unlink that ends an inode's life records its
pending release in the transaction that removes the last entry, so the refs are
dropped later ([§6.4](#6.4%20Delete), [RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)). Across two databases, each of those needs a
cross-store protocol whose failure modes are exactly the torn states this
document forbids. What one
database does not require is one keyspace: a backend **MAY** give each side its
own tables or key prefix, so that offload-commit churn is not reclaimed together
with namespace records. Splitting them onto separate servers is open ([§13](#13.%20Open%20questions)).

## 2. The records

Block metadata holds three things — a file's **existence**, its **content map**,
and the **blocks** the content lives in — in six kinds of record. Each record is
keyed by exactly one thing, answers exactly one question, and none holds a list
that grows with its file.

![Three concepts in five records: existence (shape and holes, written by the write path), the content map (refs pointing at chunks by hash, written by the offload commit), and blocks (a live count per remote object, retired by sweep), with the direction each one points](img/rfc4-records.svg)

| Concept                                                             | Record    | Keyed by           | Holds                                      | Answers                                                |
| ------------------------------------------------------------------- | --------- | ------------------ | ------------------------------------------ | ------------------------------------------------------ |
| Existence ([§3](#3.%20Existence))                                   | **Shape**   | `FileID`           | size, truncation stamp, write-time `mtime` and `ctime` | how long is the file, and when was it written? |
|                                                                     | **Hole**    | `(FileID, start)`  | end                                        | was this range never written?                          |
|                                                                     | **Removal** | `(FileID, stamp)`  | removed range                              | what did a truncate or deallocate remove after an offer? |
| Content map ([§2.1](#2.1%20Ref))                                    | **Ref**     | `(FileID, offset)` | chunk hash, skip, length, content versions | which bytes of which chunk are these?                  |
|                                                                     | **Chunk**   | chunk hash         | block name, position in block, refcount    | where is this chunk, and who uses it?                  |
| Blocks ([§7](#7.%20What%20sweep%20needs%20from%20this%20component)) | **Block**   | block name         | live-chunk count, encoding generation, encodings | may this remote object be deleted, and how was it written? |

Who writes each record follows from the concept, with one exception:

| | Shape, Hole, Removal | Ref | Chunk | Block |
| --- | --- | --- | --- | --- |
| Write path | writes Shape, Hole | — | — | — |
| Offload commit | reads | writes | creates, counts | creates, counts |
| Truncate, deallocate, clone, delete | writes | writes | counts | counts |
| Relocation, retirement ([§7](#7.%20What%20sweep%20needs%20from%20this%20component)) | — | — | moves, deletes | creates, deletes |

The exception is the offload commit, which writes three records. That is one
transaction recording one event — a block became durable — and the three records
are its three consequences: new content at these offsets, these chunks now
exist, this object now holds them ([§4.1](#4.1%20What%20one%20commit%20records)). The write path and the offload commit
share no record ([§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths)).

**Why not fewer.** Each merge that would remove a record kind moves a cost
somewhere this document forbids:

| Merge | What breaks |
| --- | --- |
| Holes into Shape, as a list | the list grows with the file's sparseness, and every write into a hole rewrites it (I7) |
| Holes into the ref keyspace, as refs with no chunk | the write path and the offload commit then write one keyspace, and a write into a hole races a commit over the same offsets ([§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths)) |
| Chunk into Ref: refs name `(block, position)` | relocating a block rewrites every ref naming its chunks, across every file; deduplication needs a by-hash lookup, which is the chunk record again; the refcount has nowhere to live ([§2.5](#2.5%20Refs%20name%20hashes%2C%20never%20blocks)) |
| Block into Chunk: `live` computed by scanning a block's chunks | retirement's condition becomes a predicate over a range, and closing the race with adoption then needs serialisable range reads that not every backend has ([§7.1](#7.1%20Conditional%20retirement), [§7.2](#7.2%20Adoption%20is%20conditional%20on%20existence)); sweep finds dead blocks only by scanning every chunk |
| Shape into the namespace inode | `size` and the holes would move in two records, and every write would rewrite the inode `chmod` writes ([RFC 7 §2.5](rfc-7-namespace-metadata.md#2.5%20Where%20%60size%60%20lives)) |

The block record is the one that holds nothing new: `live` is derivable from the
chunk records. It is kept as a materialised count because it is the single key
retirement and adoption conflict on, and the index sweep reads to find what to
retire. A backend that could give both — serialisable range predicates and a
cheap query for blocks with no live chunk — could derive it instead, and
[§7.5](#7.5%20Audit)'s audit already recomputes it that way.

### 2.1 Ref

A ref says: *these bytes of this file are that range of that chunk.* It is
[RFC 0](rfc-0-data-lifecycle.md)'s **chunk ref** — one file's use of one chunk at one offset:

    Ref(file, offset) = { hash, skip, length, oldest, newest }

- `file` is the inode that owns the ref, never a name ([§6.5](#6.5%20Who%20owns%20a%20ref)). A snapshot's copy
  of a file owns refs too, keyed by `(snapshot, file)` in place of the file.
- `offset` is where in the file the ref's bytes begin.
- `hash` names the chunk. A ref never names a block ([§2.5](#2.5%20Refs%20name%20hashes%2C%20never%20blocks)).
- `skip` and `length` select which bytes of the chunk the ref uses.
- `oldest` and `newest` bound the journal content versions of what the ref
  describes (below).

To read offset *x*, take the ref with the greatest `offset` ≤ *x*. If
*x* < `offset + length`, the byte at *x* is byte `skip + (x − offset)` of chunk
`hash`. Otherwise no ref covers *x*, and existence says whether it is a hole or
uncarved ([§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class)).

![Refs of two files over three chunks: file f tiled by refs to A, B and C, the ref to B split in two by a deallocation, and file g adopting A, so that A's refcount is 2 and B's is 2](img/rfc4-refs.svg)

**An example.** File `f` is 10 MiB and was carved into three chunks: A (4 MiB),
B (3 MiB) and C (3 MiB). Its refs tile it:

| Ref | hash | skip | length | covers |
| --- | --- | --- | --- | --- |
| `(f, 0)` | A | 0 | 4 MiB | `[0, 4M)` |
| `(f, 4M)` | B | 0 | 3 MiB | `[4M, 7M)` |
| `(f, 7M)` | C | 0 | 3 MiB | `[7M, 10M)` |

- **Deallocate `[5M, 6M)`.** The ref over B splits into `(f, 4M) → B, skip 0,
  length 1M` and `(f, 6M) → B, skip 2M, length 1M`, and `[5M, 6M)` becomes a
  hole. B's refcount goes from 1 to 2: one chunk, two uses. No byte moved and B
  was not rewritten.
- **Truncate to 9 MiB.** The last ref narrows to `(f, 7M) → C, skip 0, length 2M`.
  C keeps all 3 MiB in its block; the ref stops using the tail.
- **File `g` writes the same 4 MiB as A.** Its offload finds A's chunk record and
  adopts it: `(g, 0) → A, skip 0, length 4M`, and A's refcount goes to 2. That is
  all deduplication is — two refs naming one hash.

[§2.7](#2.7%20A%20file%27s%20life%2C%20record%20by%20record) follows one file through every record, from create to delete.

**Why a ref carries content versions.** Every write the journal stages gets a
content version, higher than any before it for that file ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)). An offload
pass is offered extents whose versions lie in `[Oldest, Newest]` ([RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload)),
and the commit records that range on every ref the pass writes. A range, not a
version per byte, is enough for both of its uses.

The first use is ordering commits: of two passes over overlapping offsets, the
one with the lower `newest` carries older content and must not overwrite the
other ([§4.4](#4.4%20Commits%20for%20one%20file%20apply%20in%20order)).

The second is reseeding after a crash ([RFC 1 §9.2](rfc-1-journal.md#9.2%20Offload%20state%20after%20recovery)). A crash forgets which journal
extents were already offloaded, and the engine rebuilds that from the refs: what a
ref covers is durable remotely, so the journal may evict it. Position alone is
not enough to say so:

1. Write version 1 over `[0, 4M)` and offload it. The ref `(f, 0)` records
   `newest = 1`.
2. Write version 2 over `[0, 1M)`. The journal holds it; it is not offloaded yet.
3. Crash and recover. The journal still holds version 2 at `[0, 1M)`, and the ref
   still covers `[0, 4M)`.
4. Judged by position, the ref covers `[0, 1M)`, so reseed marks it durable and
   eviction may drop version 2. The next read fetches version 1. An
   acknowledged write is gone, and nothing reports it.

With the versions, reseed passes the ref's `oldest` and `newest` to
`MarkDurable`. The journal sees version 2 > `newest` at `[0, 1M)`, leaves that
extent unmarked, and it is offloaded again. Held content *older* than `oldest` can
only have come back from a restored segment, and the journal drops it as stale.

`skip` and `length` select `[skip, skip + length)` of the chunk's bytes. A ref
that uses a whole chunk has `skip = 0` and `length` equal to the chunk's length.
A ref **MUST** be able to name a strict sub-range of its chunk, because truncate
narrows the tail of one ([RFC 0 §7](rfc-0-data-lifecycle.md#7.%20Mutation%20and%20removal)) and deallocating the middle of a chunk leaves
two refs to it with different `skip`.

Refs of one file **MUST NOT** overlap. A file's refs, in offset order, tile the
parts of the file that have been carved and committed; everything else is a hole
or uncarved ([§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class)).

**A ref is its own record.** An implementation **MUST NOT** store a file's refs as
one value, one document or one row holding the list. A single list costs a rewrite
of the whole list per commit, so writing a file of *N* chunks costs O(*N*²), and
it makes the list a key every offload and every writer contend on ([§5](#5.%20Write%20sets)). A value
that grows with its file can also outgrow what the storage engine reclaims by its
usual mechanism, which is [RFC 0 §9.1](rfc-0-data-lifecycle.md#9.1%20Records%20and%20their%20reclamation)'s obligation (I7). A backend that holds a
list at all **MUST** satisfy I7 for it at the list's worst-case encoded size;
one record per ref satisfies both rules at once.

### 2.2 Chunk

    Chunk(hash) = { block, position, length, refcount }

A chunk is keyed by its content hash and by nothing else ([RFC 2 §4](rfc-2-carver.md#4.%20Identity)). There is
**one chunk record per hash** in a namespace ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)), however many files and
offsets use it. A record keyed by `(file, offset)` that also carries a refcount
is a ref wearing a chunk's name, and the refcount on it counts nothing.

`block` and `position` locate the chunk's bytes: the name of the block that
carries it, and `position`, the offset and length of the chunk's body in the
encoded block, as the block's header records them ([RFC 4 §3.2](rfc-4-remote-tier.md#3.2%20Layout)). This is what a
ranged read asks for. It is recorded from the encoder's output when the block is
committed, never computed from local offsets, because transforms change body
lengths.

`position` is the one field a read may rewrite. Two passes in flight that carry
identical chunk lists write one block name, possibly with different layouts
([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20durable%20on%20success)), so a recorded position can be stale. The engine's store repairs
that inside its read, from the block's header, and yields the chunk with the range
it was actually read from ([RFC 4 §3.4](rfc-4-remote-tier.md#3.4%20Every%20read%20is%20verified%20by%20the%20codec)); when that range differs from the record,
the engine rewrites `position` ([RFC 8 §6.7](rfc-8-engine.md#6.7%20An%20absent%20object%20is%20re-resolved%20exactly%20once)). The rewrite is conditional on the
record still naming the same block, so it never races relocation.

### 2.3 Block

    Block(name) = { live, generation, encodings }

`live` is the number of chunk records that name this block and whose refcount is
nonzero. A chunk this block carries whose record names another block is dead
weight here and is not counted ([§4.1](#4.1%20What%20one%20commit%20records)). A block is sweepable when, and only when,
`live` is zero ([RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep)).

`generation` is the block's **encoding generation**, an input to its name
([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)): 0 for a block written by offload, and the source block's
generation plus one for a block a relocation re-encoded ([§7.3](#7.3%20Relocation)). It is recorded
so that a relocation can derive the next one, and a re-run after a crash derives
the same name.

`encodings` lists the transform IDs, versions and material IDs the block's bodies
use, as the store reported them when the block was put ([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)). It is
what the transform census reads; nothing on the read path consults it.

A block record has **no durability flag**, because it has no non-durable state:
by [§4.2](#4.2%20Only%20after%20durability) a block record exists only once its block is durable. What a block record
records is that the block exists remotely and how much of it is still wanted.

### 2.4 Shape and holes

    Shape(file)          = { size, stamp, mtime, ctime }
    Hole(file, start)    = { end }
    Removal(file, stamp) = { start, end }

A hole record is one extent `[start, end)` below `size` that was never written,
or was deallocated. A file's holes never overlap or touch — adjacent holes merge
— so they are exactly its unwritten ranges, ordered by `start`, and a dense file
has none. A hole is its own record for the reason a ref is ([§2.1](#2.1%20Ref)): a hole list
on the shape record would grow with the file's sparseness, and every write into
a hole would rewrite it.

`mtime` and `ctime` are the times of the last write, written by the write path in
the transaction that records existence. They are [RFC 7](rfc-7-namespace-metadata.md)'s attributes, stored here
so that a write touches one record ([RFC 7 §2.5](rfc-7-namespace-metadata.md#2.5%20Where%20%60size%60%20lives)).

`stamp`, the **truncation stamp**, is a counter, not a clock. It starts at zero,
and only truncate-down and deallocate advance it ([§6.2](#6.2%20Truncation%20and%20deallocation)). Each advance writes one
removal record naming the range it removed — `[new size, ∞)` for a truncate — at
the new stamp. Together they let an offload commit see exactly what was removed
under it:

1. The journal offers `[0, 10M)` of `f`. The engine reads `stamp = 3` and
   carries it with the pass ([RFC 8 §4.4](rfc-8-engine.md#4.4%20The%20truncation%20stamp%20is%20captured%20at%20offer%20and%20checked%20at%20commit)).
2. A client truncates `f` to 5 MiB: `stamp` becomes 4 and `Removal(f, 4) = [5M, ∞)`.
3. The pass commits, finds `stamp = 4`, reads the removals after 3, and drops the
   refs of `f` that overlap `[5M, ∞)`; the rest of the commit applies. The
   dropped extents are not reported durable, and the journal, already truncated,
   has nothing left to offer there.

Without the check, the commit would leave refs past the new end of file ([§6.2](#6.2%20Truncation%20and%20deallocation)).
Dropping only what overlaps a removal keeps a file that punches holes in a loop
from dropping every commit. A timestamp cannot do this job: two operations in one
tick look identical, and clocks step backwards. Nor can the content version,
which every write advances, so every offload would conflict with every write —
the livelock [§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths) forbids.

A removal record is needed only while a pass captured before it can still commit.
The engine deletes a file's removal records at or below the lowest stamp any of
its passes in flight captured, and all of them at startup, before offload runs
([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)). That is the reclamation path I7 asks every record to name.

[§3](#3.%20Existence) is entirely about why these records exist.

### 2.5 Refs name hashes, never blocks

A ref carries a chunk hash, and the chunk record says where that chunk lives. A ref
**MUST NOT** carry a block key or a position in a block.

The indirection is what lets a chunk move. Relocating a chunk into a new block
([§7.3](#7.3%20Relocation)) then rewrites one chunk record. If refs carried the location, the same move
would rewrite every ref of every file sharing the chunk, and any copy of the refs
taken earlier — a snapshot, a backup — would name a block that no longer exists.

### 2.6 The scope of a count

A refcount is only as good as the set of refs it counts. If a ref exists that the
count does not include, the count is low, and sweep deletes referenced content.

The records of [§2](#2.%20The%20records) **MUST** therefore be held in one store per remote key
namespace ([RFC 2 §4.3](rfc-2-carver.md#4.3%20Key%20scope)). A deployment **MUST** do one of two things:

- **One store per namespace.** Every share whose blocks can share a remote key
  records its refs in the store that counts them.
- **One namespace per store.** Key derivation includes the store's identity, so
  two stores can never name one object.

Two stores that can name one key, each keeping its own count, are forbidden. Each
count is then correct about its own store and too low about the object, and
neither store can tell.

The same reasoning bounds what absence proves. **The absence of a record here
MUST NOT be taken as evidence that a remote object is unreferenced.** It shows
only that this store does not reference the object. Another store, another
process or another deployment writing to the same bucket may reference it. How an
unrecorded object is collected is [RFC 9](rfc-9-gc.md)'s problem. This document only forbids
treating a missing record as proof that nothing references the object.

### 2.7 A file's life, record by record

One file `f`, from create to delete, with every record that exists after each
step. Block positions ignore framing.

**t0 — create.**

| Record | Value |
| --- | --- |
| Shape(f) | size 0, stamp 0 |

**t1 — write 4 MiB at 0, content version v1.** The journal stages the bytes, the
write path grows `size`, the client is acknowledged.

| Record | Value |
| --- | --- |
| Shape(f) | size **4M**, stamp 0, mtime and ctime of v1 |

`[0, 4M)` is **uncarved**: below `size`, not a hole, no ref ([§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class)). Only the
journal holds the bytes, and `size` is the only record that says they exist. If
the journal loses them, a read fails as **Lost**; it does not return zeros.

**t2 — write 1 MiB at 10M, v2.** The write starts past the end of the file, so
the gap becomes a hole.

| Record | Value |
| --- | --- |
| Shape(f) | size **11M**, stamp 0 |
| Hole(f, 4M) | end 10M |

`[0, 4M)` and `[10M, 11M)` are uncarved; `[4M, 10M)` is a hole and reads as
zeros. Without the hole record the two kinds of range are indistinguishable.
Had the write started at 4M instead — an append — no hole would exist.

**t3 — offload.** The journal offers both extents, with `Oldest` v1 and `Newest`
v2. The carver cuts chunk A (4 MiB) and chunk B (1 MiB), the engine packs both
into block K1, the syncer puts it and reports it durable, and one commit writes:

| Record | Value |
| --- | --- |
| Shape(f) | size 11M, stamp 0 — read, not written: the stamp is unchanged |
| Hole(f, 4M) | end 10M |
| Ref(f, 0) | A, skip 0, length 4M, oldest 1, newest 2 |
| Ref(f, 10M) | B, skip 0, length 1M, oldest 1, newest 2 |
| Chunk(A) | block K1, position `[0, 4M)`, refcount 1 |
| Chunk(B) | block K1, position `[4M, 5M)`, refcount 1 |
| Block(K1) | live 2, generation 0 |

The journal marks both extents durable. They are **Resident** and evictable.

**t4 — overwrite 1 MiB at 1M, v3.** The write path touches only Shape, and
`size` does not change. No ref changes: `Ref(f, 0)` still covers `[0, 4M)`. The
journal holds v3 at `[1M, 2M)`, newer than the ref's `newest`, so that extent is
**Dirty** again and reads are served from the journal.

A crash here is the case [§2.1](#2.1%20Ref)'s versions exist for. Reseed calls
`MarkDurable([0, 4M), oldest 1, newest 2)`; the journal finds v3 > 2 at
`[1M, 2M)` and leaves it unmarked, so v3 is offloaded again rather than evicted.

**t5 — offload the overwrite.** The journal offers `[1M, 2M)` at v3. The carver
cuts chunk C into block K2, and the commit splits the ref around it:

| Record | Value |
| --- | --- |
| Ref(f, 0) | A, skip 0, length **1M**, 1–2 |
| Ref(f, 1M) | **C**, skip 0, length 1M, 3–3 |
| Ref(f, 2M) | A, skip **2M**, length 2M, 1–2 |
| Ref(f, 10M) | B, skip 0, length 1M, 1–2 |
| Chunk(A) | K1, `[0, 4M)`, refcount **2** — two refs name it |
| Chunk(B) | K1, `[4M, 5M)`, refcount 1 |
| Chunk(C) | K2, `[0, 1M)`, refcount 1 |
| Block(K1) | live 2 |
| Block(K2) | live 1 |

A was not rewritten. Its middle MiB, the v1 bytes, is dead weight in K1 that no
ref uses.

**t6 — truncate to 3 MiB.** One transaction ([§6.2](#6.2%20Truncation%20and%20deallocation)):

| Record | Change |
| --- | --- |
| Shape(f) | size **3M**, stamp **1** |
| Removal(f, 1) | `[3M, ∞)` |
| Hole(f, 4M) | deleted — past the new end of file |
| Ref(f, 2M) | narrowed to A, skip 2M, length **1M** |
| Ref(f, 10M) | deleted, so B's refcount goes 1 → **0** |
| Block(K1) | live 2 → **1** — A is still referenced, B is not |

An offload that had been offered the 11 MiB file and commits now finds stamp 1,
not 0, and drops those of `f`'s refs that overlap `[3M, ∞)` ([§2.4](#2.4%20Shape%20and%20holes)).

**t7 — delete.** The namespace releases the inode through the engine's `Release`
([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)), and its three refs are dropped. A's refcount goes 2 → 0 and C's 1 → 0, so K1's `live` and
K2's `live` both reach 0. Sweep's `Retire(K1)` checks `live == 0` and deletes
Block(K1), Chunk(A) and Chunk(B) in one transaction, and only then deletes the
object ([§7.1](#7.1%20Conditional%20retirement)). K2 goes the same way.

| Record | Written when | Its job above |
| --- | --- | --- |
| **Shape** | a write; truncate | `size` was the only record of the uncarved bytes (t1); `stamp` told offload a truncate had happened (t6) |
| **Removal** | truncate; deallocate | told offload which range the truncate removed (t6) |
| **Hole** | a write past EOF; truncate; deallocate | told the zeros at `[4M, 10M)` from the uncarved bytes (t2) |
| **Ref** | offload commit; truncate; deallocate; delete | mapped file bytes to chunk bytes and split on overwrite (t5); its versions kept reseed from releasing v3 (t4) |
| **Chunk** | offload commit; retirement | located A, B and C in their blocks and counted their refs |
| **Block** | offload commit; retirement | counted live chunks, so sweep knew when K1 could go (t7) |

The write path wrote only Shape and Hole (t1, t2, t4). Offload wrote only Ref,
Chunk and Block, and read `stamp` (t3, t5).

## 3. Existence

### 3.1 The gap this closes

[RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function) resolves an extent that no chunk covers to **Absent**, and reads it as
zeros. That is right for a hole and wrong for content that was written but not yet
carved: a client write is acknowledged long before its first offload ([RFC 0 §5.1](rfc-0-data-lifecycle.md#5.1%20Write)),
and until then no chunk covers it.

If the journal loses such an extent — a corrupt record ([RFC 1 §9.3](rfc-1-journal.md#9.3%20Torn%20and%20corrupt%20records)), a lost
device — metadata has no chunk, so the extent resolves to **Absent**, and the
system serves zeros for acknowledged data. That is I1's violation, reached through
the one window where only one oracle knew the content existed.

The fix is the same as [RFC 0](rfc-0-data-lifecycle.md)'s own: stop asking one source a question it cannot
answer. **Existence is recorded at write time, independently of carving.**

### 3.2 Every offset is in exactly one class

For an offset below `size`, block metadata answers with one of three classes,
and the classes are disjoint and exhaustive:

| Class | Meaning | Residency with journal present | Residency with journal absent |
| --- | --- | --- | --- |
| **hole** | in the hole set | — | **Absent** |
| **uncarved** | not a hole, no ref covers it | **Dirty** | **Lost** |
| **carved** | a ref covers it | **Resident** | **Remote** |

An offset at or beyond `size` is past end of file and is not a residency question.

![A file drawn as a strip below its size: a hole from a write beyond EOF, a carved run of refs, an uncarved run written since the last offload, and the three questions metadata answers for each](img/rfc4-offset-classes.svg)

An implementation **MUST** distinguish *hole* from *uncarved*. Deriving holes as
"gaps between refs" is the error this section exists to forbid: it classes every
uncarved byte as a hole, and every such byte the journal loses reads back as
zeros. The same derivation gives `SEEK_HOLE` wrong answers for content written
since the last offload.

This table **amends [RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function)** ([§10](#10.%20Consequences%20for%20RFC%200)). Its metadata column is these three
classes rather than "chunk exists" and "block durable", and the row
"chunk exists, block not durable" is unreachable, because a chunk record exists
only once its block is durable ([§4.2](#4.2%20Only%20after%20durability)).

### 3.3 Why holes, not written extents

Existence could be recorded either way round — the set of extents that were
written, or the set that were not. This document records **holes**, because of
what each costs the write path:

| | Record written extents | Record holes |
| --- | --- | --- |
| Overwrite inside the file | insert or merge an extent | nothing beyond size |
| Append at EOF | extend an extent | nothing beyond size |
| Write starting past EOF | insert an extent | add one hole for the gap |
| Write into a hole | insert an extent | shrink or split a hole |
| Grows with | every distinct write range | sparseness only |

Almost every write is an overwrite or an append, and those already update `size`
([RFC 0 §5.1](rfc-0-data-lifecycle.md#5.1%20Write)). Recording holes adds nothing to them. A dense file has an empty hole
set however it was written.

**Holes are routine even on a share with no sparse files.** NFS and SMB clients
send one file's writes as many concurrent requests — RPC slots, multi-credit
writes — and the server receives them in any order. A sequential copy that
arrives as `@2M, @0, @3M, @1M` creates a hole at the first request and fills it
over the next three: write past EOF, write into a hole, append, write into a
hole. Hole creation and removal are therefore on the write path's hot path, and
the per-write cost bound below applies to them as much as to appends. Deliberate
sparseness — disk images, preallocated database files, out-of-order downloads,
punched ranges — adds holes that last; out-of-order arrival adds holes that last
microseconds.

The chosen representation is not otherwise normative. An implementation **MAY**
record written extents instead if it meets [§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class) and [§3.4](#3.4%20Ordering%20against%20the%20journal), and **MUST** then show
that its per-write cost does not grow with the number of prior writes.

### 3.4 Ordering against the journal

An extent leaves the hole set, or `size` grows past it, as a claim that its bytes
exist. The claim **MUST NOT** precede the bytes:

1. The namespace layer authorises the write ([RFC 7](rfc-7-namespace-metadata.md)).
2. The journal stages the bytes ([RFC 1 §3.1](rfc-1-journal.md#3.1%20Write)).
3. Existence is recorded: `size` grown, holes shrunk.
4. The client is acknowledged.

Each crash point then resolves to a truthful answer:

| Crash after | Journal | Existence | Resolves to | For an acknowledged write? |
| --- | --- | --- | --- | --- |
| 2 | holds bytes | hole, or past `size` | **Absent**, or past EOF | no — never acknowledged, so zeros are correct |
| 3 | holds bytes | uncarved | **Dirty** | no — and the write survived anyway |

Reversing 2 and 3 makes the first crash resolve to **Lost** for a write that was
never acknowledged — a loud failure for data no client was promised.

Existence **MUST** be at least as durable at step 4 as the journal's
record is under the configured policy ([RFC 1 §6.2](rfc-1-journal.md#6.2%20Sync%20policy)). An acknowledged write whose
bytes survive and whose existence does not is invisible, which is I1's violation
by the other route.

An implementation **MAY** group-commit existence across many writes, and
**SHOULD**, because appends change `size` on every write. It **MUST NOT**
acknowledge any write in the group before the group's commit.

**Existence MUST NOT be reconstructed from the journal.** Growing `size` after a
crash to cover what the journal holds makes the journal the oracle for existence
again, which [RFC 0 §4.1](rfc-0-data-lifecycle.md#4.1%20The%20two%20oracles) forbids — and it fails in the direction that matters: it
cannot recover what the journal lost, which is the only case existence is recorded
for.

### 3.5 Operations that make holes

| Operation | Effect on existence |
| --- | --- |
| Write starting past `size` | `size` grows; `[old size, write offset)` becomes a hole |
| Truncate up | `size` grows; `[old size, new size)` becomes a hole |
| Truncate down | `size` shrinks; holes past it are dropped; `stamp` advances and a removal is recorded |
| Deallocate | the range becomes a hole; refs over it are dropped or narrowed ([§6](#6.%20Reference%20counting)); `stamp` advances and a removal is recorded |
| Allocate | none — see below |

**Allocate MUST NOT remove a hole** unless the zeros it promises are staged in
the journal first, by [§3.4](#3.4%20Ordering%20against%20the%20journal). Removing a hole claims bytes exist. With nothing
staged the range becomes uncarved and journal-absent — **Lost** — so a
preallocated file would fail every read of a range it never wrote. Reporting
allocation to `SEEK_DATA` is RFC 7's, and it **MUST NOT** be done by editing
existence.

## 4. The offload commit

### 4.1 What one commit records

An offload pass ([RFC 0 §5.2](rfc-0-data-lifecycle.md#5.2%20Offload)) ends in one commit per block, or one commit for several.
A commit records, in **one transaction**:

- the block record, with its generation and encodings ([§2.3](#2.3%20Block));
- a chunk record for each chunk the block carries **that has none yet**, naming
  this block;
- the refs for the extents the pass carved, replacing what they overlap;
- the refcount changes those refs imply ([§6](#6.%20Reference%20counting)), including for chunks the pass
  adopted from earlier blocks rather than carrying;
- the `live` changes those refcount changes imply.

Partial application of that list **MUST NOT** be observable: a ref without its
chunk, a refcount without its ref, or a chunk without its block each turns a
later read or a later sweep into a guess.

**The first block to commit a chunk owns its record.** A later commit that carries
the same chunk finds the record present and adopts it: it adds its refs and their
counts, and leaves `block` and `position` as they are. The copy the later block
carries is dead weight, not counted in its `live`. Replacing the record would
reset its count and point it at a block whose `live` does not count it, so a
sweep could delete the block the count's refs still need.

**A commit is idempotent per block name.** Two passes in flight can carry
identical chunk lists and so commit one name twice ([RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20durable%20on%20success)). A commit that
finds the block record already present **MUST NOT** overwrite `live`: it adds to
`live` only for the chunk records it creates, and applies its refs by the rules
below like any other commit.

**The truncation check is per file and per range.** A block may carry chunks of
several files ([RFC 8 §5.7](rfc-8-engine.md#5.7%20A%20block%20packs%20chunks%2C%20whichever%20files%20they%20came%20from)). For each file, the commit compares the stamp captured
at offer with the current one and drops only the refs that overlap a removal
recorded since ([§6.2](#6.2%20Truncation%20and%20deallocation)); every other ref, of that file and of the others, applies.
The block is durable either way; a chunk it carries only for dropped refs is
dead weight.

### 4.2 Only after durability

A commit **MUST NOT** run before the syncer has reported the block durable
([RFC 3 §2.6](rfc-3-syncer.md#2.6%20Durability%20is%20observed%2C%20never%20inferred)). Chunk and block records are therefore records of durable content, and
nothing else.

This is the rule that collapses [RFC 0](rfc-0-data-lifecycle.md)'s five metadata states into [§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class)'s three. No
record says "this chunk exists but its block might not", so no reader has to
decide what that means.

> [!note]
> Recording refs before the put would let a crash leave refs to a block
> that was never written. With content-derived keys ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)) that is not a
> leak, but it is a ref to nothing, and every reader would have to check
> durability on every ref. Committing after the report costs a window in which an
> uploaded block is recorded nowhere. That window is safe, because the key is its
> content and the retry targets the same object.

A share with no remote tier therefore never commits: its content stays uncarved
and **Dirty** for its lifetime, which is exactly what [RFC 0 §8.1](rfc-0-data-lifecycle.md#8.1%20Evict) requires of
content with no remote copy.

### 4.3 The commit is the report's return edge

The extents a commit covers are the extents the offload callback returns as durable
([RFC 0 §5.2](rfc-0-data-lifecycle.md#5.2%20Offload)). The callback **MUST NOT** return an extent whose commit has not
succeeded. That ordering is what lets a reseeding pass after a crash trust
metadata over the journal's unset offloaded bits ([RFC 1 §9.2](rfc-1-journal.md#9.2%20Offload%20state%20after%20recovery)): an offloaded bit is never
set for content that metadata does not hold.

### 4.4 Commits for one file apply in order

A commit **MUST NOT** replace refs written by a commit of content offered later.

Two passes can carve overlapping extents of one file — B offered after a write
superseded what A was offered. If A commits after B, A's refs overwrite B's. The
journal has marked B's bytes durable, so it may release them, and a later read
fetches A's content for the offsets B wrote. That is silent corruption, and no
single component observes it.

Every ref carries the content versions of its offer ([§2.1](#2.1%20Ref)), because reseed needs
them, and the commit orders refs by them. For each ref it would replace, a
commit compares the new ref's `newest` with the existing ref's range:

| New `newest` | The commit |
| --- | --- |
| above the existing `newest` | replaces the ref |
| inside the existing `[oldest, newest]` | treats the content as already committed: writes nothing for that ref and reports its extent durable |
| below the existing `oldest` | refuses that ref, and applies the rest |

Only strictly older content is refused. This is sufficient: content at an offset
that differs between two passes was written after the earlier pass was offered,
so its version, and the later pass's `newest`, exceeds every version the earlier
pass holds. The engine also serialises passes per file ([RFC 8 §4.3](rfc-8-engine.md#4.3%20Commits%20for%20one%20file%20are%20serialised%20here)); that keeps
the check from firing in one process, and does not stand in for it.

## 5. Write sets

### 5.1 No record is written by both paths

The write path and the offload commit **MUST NOT** write a common record. [§2](#2.%20The%20records)
tabulates who writes what; the write path's column is existence alone.

![Which paths write which records, and the one shared per-file key the rule removes: a writer streaming appends and an offload committing chunks, retrying against each other on one record](img/rfc4-write-sets.svg)

A record written by both is a conflict between a client stream and a background
pass on every offload. Under optimistic concurrency the pass retries. The retry
re-reads the record the client is still writing, so it conflicts again, and the
pass never commits. Nothing is offloaded, nothing becomes evictable, the journal
fills, and writes are refused ([RFC 0 §10](rfc-0-data-lifecycle.md#10.%20Failure%20model)). That is livelock, not contention.
Backoff does not fix it, because the collision comes from the structure, not the
timing.

The offload commit *reads* `stamp` and the file's removal records ([§6.2](#6.2%20Truncation%20and%20deallocation)).
Truncation and deallocation are their only writers, so the read conflicts only
with the operations it must conflict with.

`size` and `mtime` changes belong to the write path and are [RFC 7](rfc-7-namespace-metadata.md)'s attributes.
The offload commit **MUST NOT** touch them. Offloading changes where content is, not
what it is.

### 5.2 Cost per commit is bounded by what changed

An offload commit **MUST** write O(refs replaced + chunks committed + blocks
committed) records, and **MUST** read no more than O(log *n*) records per ref it
replaces, where *n* is the number of refs in the file.

No per-commit cost may grow with the size of the file. A commit that is O(file)
makes a file of *N* chunks O(*N*²) to write, and makes the largest files, which
are the ones under the heaviest write load, the slowest to offload.

### 5.3 Hot records that are not per-file

[§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths) removes the per-file hot record. It does not remove every hot record:

- **A popular chunk's refcount.** Every file that references a common chunk
  increments one record. The commonest chunk by far is all zeros, which a
  content-defined chunker cuts at `Max` over any zero run ([RFC 2 §3](rfc-2-carver.md#3.%20The%20boundary%20function)). Every
  preallocated or zero-filled region of every file then contends on it.
- **A popular block's `live`.** Moves only when a chunk's refcount crosses zero,
  so it is far colder than the refcount.
- **Usage accounting.** A per-owner byte or quota counter updated on every write
  is one record shared by all of that owner's files. It belongs to the write path
  and RFC 7, and by [§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths) the offload commit **MUST NOT** update it.

An implementation **SHOULD** measure the first two under a zero-heavy workload before
shipping. Remedies — a sharded counter, or not carving known-zero chunks and
recording them as holes — are open ([§13](#13.%20Open%20questions)).

## 6. Reference counting

### 6.1 A refcount is exactly its refs

A chunk's refcount **MUST** equal the number of refs naming it, at every commit
point. It **MUST** change in the same transaction as the refs that change it —
never before, never after, never in a second transaction a crash can separate
from the first.

A refcount that drifts high leaks a chunk forever. A refcount that drifts low lets
sweep delete a chunk that is still referenced. That is I3's violation, and the
only failure in this system that destroys the last copy of content ([RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep)).

A block's `live` **MUST** move in the same transaction as every refcount crossing
between zero and nonzero, for a chunk that block carries.

### 6.2 Truncation and deallocation

Truncate down and deallocate change refs outside an offload, and **MUST** in one
transaction:

- drop the refs wholly inside the removed range, and decrement their chunks;
- narrow a ref that straddles its edge, by adjusting `skip` or `length`, so that
  no ref describes content outside the file's remaining extents ([RFC 0 §7](rfc-0-data-lifecycle.md#7.%20Mutation%20and%20removal));
- split a ref that spans a deallocated range into two refs to the same chunk, and
  increment that chunk;
- update existence ([§3.5](#3.5%20Operations%20that%20make%20holes)), advance `stamp`, and write the removal record for the
  range at the new stamp ([§2.4](#2.4%20Shape%20and%20holes)).

An offload commit **MUST NOT** apply a ref that overlaps a range removed after the
file's stamp was captured at offer; it drops that ref and applies the others
([§4.1](#4.1%20What%20one%20commit%20records)). Without that check a pass that carved `[0, 10 MiB)` commits refs after a
concurrent truncate to 5 MiB. The file then holds refs past its end, and a later
truncate up turns them back into readable content where the user was promised
zeros. A dropped extent is not reported durable, and what the journal still
holds there is offered again ([RFC 1 §3.6](rfc-1-journal.md#3.6%20Truncate%2C%20deallocate%20and%20delete)).

The engine holds the file's offload guard across both the metadata step and the
journal's own truncate or deallocate ([RFC 8 §4.3](rfc-8-engine.md#4.3%20Commits%20for%20one%20file%20are%20serialised%20here)), so no offer falls between
them; the stamp check is what keeps a commit correct without relying on that
guard.

`stamp` is advanced only by these two operations. An append **MUST NOT** advance
it. If it did, the check would conflict with every write, and [§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths) would be undone
by the back door.

### 6.3 Underflow is corruption, not a boundary

A decrement that would take a refcount or `live` below zero **MUST** fail the
transaction and **MUST** be reported as a consistency error naming the chunk or
block.

It **MUST NOT** clamp at zero. Clamping hides the defect that caused the
underflow, usually a double decrement. And the clamped value is itself wrong in
the dangerous direction: a count that was about to go negative was already too
low, so something else still references the chunk.

### 6.4 Delete

Deleting a file drops all its refs and decrements their chunks ([RFC 0 §7](rfc-0-data-lifecycle.md#7.%20Mutation%20and%20removal)). This
**MAY** be deferred past the namespace removal, and **MUST** then leak rather
than lose:

- refs outliving their file keep chunks alive. That is a leak, and it **MUST** be
  collectable without a full scan — by a durable record of the pending deletion
  that a restart resumes;
- a refcount decremented before its ref is removed is a sweep hazard, and **MUST
  NOT** happen.

### 6.5 Who owns a ref

Refs belong to an **inode**, not to a name. Hard links, a rename over an existing
file, moving a file to trash, and a file unlinked while still open are all
namespace states of one inode. None of them changes a ref. Block metadata **MUST
NOT** be told about names. It learns of a deletion only when [RFC 7](rfc-7-namespace-metadata.md) releases the
inode through the engine's `Release`, and that release is the delete of [§6.4](#6.4%20Delete).

A **snapshot** that can be restored holds its content exactly as a file does, so
its refs **MUST** be counted exactly as a file's are. A snapshot's refs are owned
by `(snapshot, file)` rather than by a file, and creating a snapshot increments
every chunk it names.

The content a snapshot holds **MUST NOT** be protected by a separate list that
sweep consults instead of the count: a hold set, a pin list, or an extra root for
a mark phase. That is a second way of keeping content alive, and it fails open.
Every path that decides liveness has to remember to consult it. The path that
forgets deletes snapshotted content, and any check that reads the refcounts
reports that nothing is wrong. The same applies to any future holder of content:
if it can keep a chunk alive, it holds refs and is counted.

The cost is O(refs) increments per snapshot, paid when the snapshot is taken
([§13](#13.%20Open%20questions)).

### 6.6 Clone and server-side copy

Cloning carved content copies refs, not bytes. Writing the destination's refs and
incrementing their chunks **MUST** happen in one transaction, and that transaction
is an adoption ([§7.2](#7.2%20Adoption%20is%20conditional%20on%20existence)): it **MUST** fail if any chunk it names has been retired. Any
refs the destination had over the cloned range are replaced, and their chunks are
decremented in the same transaction.

**Uncarved content cannot be cloned by reference**, because no chunk covers it
yet. For the uncarved extents of the source, a clone **MUST** either offload the
source first, or copy the bytes through the destination's write path ([§3.4](#3.4%20Ordering%20against%20the%20journal)). It
**MUST NOT** record the destination range as existing, whether carved or
uncarved, unless its bytes are staged or its refs are written. Doing so claims
bytes that exist nowhere under the destination, and a read resolves to **Lost**.

## 7. What sweep needs from this component

Sweep's protocol is [RFC 9](rfc-9-gc.md)'s. It needs two atomic operations from this document,
and it cannot be made safe without them.

### 7.1 Conditional retirement

    Retire(block) — if live == 0: delete the block record and every chunk record
                    that names it; else: refuse

A chunk record that names another block is not this block's to delete, even when
this block carries a copy of the chunk ([§4.1](#4.1%20What%20one%20commit%20records)).

This **MUST** be one transaction, and the condition **MUST** be evaluated inside
it, not read beforehand. A sweep that reads `live`, decides, and deletes in a
separate step deletes a block that a commit re-adopted between the read and the
delete.

Retiring the records before deleting the remote object means a crash between the
two leaves an object nothing references. With content-derived keys ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block))
that object is findable by its content, and it is RFC 9's to collect. The reverse
order leaves records naming an object that no longer exists, so every read of
those chunks fails — which is **Lost** for content that was durable.

### 7.2 Adoption is conditional on existence

An offload commit that references a chunk it did not carry — deduplication — **MUST**
fail if that chunk's record no longer exists when the commit applies. It **MUST
NOT** recreate the record.

With [§7.1](#7.1%20Conditional%20retirement) this closes the race without a clock. A commit that adopts before
retirement increments the refcount, `live` goes nonzero, and retirement refuses.
A commit that adopts after retirement finds no record and fails, and the pass
retries, carrying the chunk's bytes this time.

An implementation **MUST NOT** substitute a grace period for either operation.
[RFC 0 §4.3](rfc-0-data-lifecycle.md#4.3%20Reporting) forbids inferring durability from elapsed time. Inferring that a block
is safe to delete because it has been unreferenced "for long enough" is the same
inference with worse consequences.

### 7.3 Relocation

Rewriting a block's surviving chunks into a new block, so that a mostly
unreferenced block can be retired, changes where chunks live. Deciding when to do
it belongs to RFC 9. This section covers only the record change.

GC reads the source and puts the new block through a syncer flow of its own
([RFC 9 §4.2](rfc-9-gc.md#4.2%20Read%20verified%2C%20name%20by%20content%2C%20put%2C%20then%20move)). The new block's name is derived like any other ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)); a
relocation that re-encodes its chunks — retiring material or a transform
([RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census)) — uses the source's generation plus one, so it never derives the
source's own name. A relocation **MUST** be refused if the target name equals the
source name: nothing re-encodes a block in place.

After the syncer reports the new block durable, one transaction:

- points each moved chunk record that still names the old block at the new block
  and position;
- creates the new block record, with its generation and encodings, or adds to
  `live` if it already exists ([§4.1](#4.1%20What%20one%20commit%20records));
- counts, inside the transaction, the moved chunks whose refcount is nonzero,
  adds that count to the new block's `live` and subtracts it from the old
  block's.

Refs are untouched, by [§2.5](#2.5%20Refs%20name%20hashes%2C%20never%20blocks). If the old block's `live` reaches zero it becomes
retirable ([§7.1](#7.1%20Conditional%20retirement)). A chunk adopted by a commit that races the relocation is
counted in whichever block its record names when the adoption applies. That is
correct in both orders, because both transactions read and write that chunk
record and so cannot interleave.

### 7.4 Restore

Restoring block metadata from a copy — a snapshot, a backup — is not a write of
old records. It is a batch of adoptions.

- Every ref the restore writes names a chunk, and that chunk **MUST** exist when
  the restore applies ([§7.2](#7.2%20Adoption%20is%20conditional%20on%20existence)). A restore that would write a ref to a retired chunk
  **MUST** fail. Writing the ref would produce content that is **Lost** on first
  read, and nobody would know until then.
- Chunk and block records **MUST** be taken from the live store, never from the
  copy. The copy's locations may predate a relocation ([§7.3](#7.3%20Relocation)). The copy's counts
  describe a store that no longer exists.
- Refcounts and `live` **MUST** change by the refs the restore adds and removes,
  as for any commit ([§6.1](#6.1%20A%20refcount%20is%20exactly%20its%20refs)).

A restore from a counted snapshot ([§6.5](#6.5%20Who%20owns%20a%20ref)) cannot fail the first check, because the
snapshot held its chunks. A restore from an uncounted copy can, and failing is
the correct outcome for it. Probing the remote tier after the restore does not
substitute for the first check: it runs after the records are written, and races
every sweep that runs in between.

### 7.5 Audit

Refcounts are sweep's only authority, so there **MUST** be a way to check them. An
audit recomputes each chunk's refcount from the refs that name it, and each
block's `live` from its chunks, and reports every mismatch.

A recomputation is valid only if it reads one consistent state of the store. A
walk that spans concurrent commits recounts a mixture of states and can come out
low.

- A repair **MAY** raise a count to its recomputed value. Raising a count that
  was right only leaks.
- A repair **MUST NOT** lower a count unless the recomputation came from a single
  consistent read and no commit, clone or restore ran between that read and the
  write. Lowering a count on the strength of a walk made while commits were
  running makes sweep unsafe.

## 8. Queries

### 8.1 Covering lookup

    Covering(file, offset) → ref | hole | uncarved | past EOF

This is the read path's question, asked once per extent the journal does not hold
([RFC 0 §6.1](rfc-0-data-lifecycle.md#6.1%20Resolution)). It **MUST** be answered in O(log *n*) in the number of refs and holes
of that file, and **MUST** be a method on the declared interface.

It **MUST NOT** be reached by a type assertion that falls back to a scan. [RFC 0](rfc-0-data-lifecycle.md)
[§1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy) explains why in general. Here specifically: the fallback is a read path that
still returns correct bytes, only linearly slower per read, so a backend that
loses the method by a rename degrades every cold read and no correctness test
notices.

A range form — the refs, holes and uncarved runs covering `[offset, offset + n)`,
in order — **SHOULD** exist, and **MUST** cost O(log *n* + results).

### 8.2 Deduplication lookup

    Durable(hash) → chunk | none

Returns the chunk record for `hash` if there is one. By [§4.2](#4.2%20Only%20after%20durability) a record implies a
durable block, so this is the complete answer to "may this chunk be referenced
rather than uploaded" ([RFC 2 §5](rfc-2-carver.md#5.%20Packing%3A%20three%20rules%2C%20not%20a%20component)). It **MUST NOT** consult anything that could
know about a block not yet committed.

The answer is advisory. A chunk it returns can be retired before the adopting
commit applies, and [§7.2](#7.2%20Adoption%20is%20conditional%20on%20existence) is what makes that safe. The query **MUST NOT** be
treated as a reservation.

### 8.3 Whole-file identity

`ObjectID` ([RFC 0 §3](rfc-0-data-lifecycle.md#3.%20Identity)) is a function of a file's refs. An implementation **MAY**
store it. If it does, it **MUST** update it in the transaction that changes the
refs, and **MUST NOT** do so at a cost proportional to the file ([§5.2](#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed)). A stored
`ObjectID` that is not updated on every ref change is worse than none, because
whole-file deduplication would then reference the wrong content.

The alternative — computing it on demand, when whole-file deduplication asks — is
permitted and is the default. Whole-file deduplication is an accelerator
([RFC 0 §3.1](rfc-0-data-lifecycle.md#3.1%20Deduplication)), and it is cheaper to pay for it when it is used than on every
offload.

### 8.4 Reseed: a file's refs and its durable floor

    Refs(file)         → refs in offset order
    DurableFloor(file) → version

After a restart the engine reseeds the journal's offloaded bits from the refs
([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)), for the files the journal holds and no others. `Refs` **MUST** cost
O(log *n* + results) for that file, so reseed costs what the journal holds, not
what the store holds.

`DurableFloor` is the highest `newest` over the file's refs: no journal content of
that file at or below it can be newer than what is committed. The journal opens
that file with it as its version floor ([RFC 1 §9.1](rfc-1-journal.md#9.1%20Rebuilding)), so a journal restored
from an old copy cannot reissue a version metadata already holds. An
implementation **MAY** compute it in the same walk as `Refs`. It **MUST NOT**
store it on the shape record, which the offload commit does not write ([§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths)).

## 9. Invariants

| # | Invariant |
| --- | --- |
| M1 | Existence is recorded before a write is acknowledged, and a hole is distinguishable from uncarved content. |
| M2 | Existence is never reconstructed from the journal. |
| M3 | A chunk or block record exists only for content reported durable. |
| M4 | A chunk's refcount equals its refs, and `live` equals the referenced chunk records naming the block, at every commit point. |
| M5 | A refcount or `live` that would go negative fails the transaction. |
| M6 | A block record is retired only by a conditional operation that evaluates `live` inside its own transaction, and retirement deletes only chunk records naming that block. |
| M7 | Adoption of a chunk fails if its record is gone, and never recreates it. |
| M8 | No record is written by both the write path and the offload commit. |
| M9 | An offload commit's cost is bounded by what it changed, not by the file. |
| M10 | A commit never replaces a ref with strictly older content, and never applies a ref over a range removed after its offer. |
| M11 | Block metadata records nothing about local placement. |
| M12 | Every holder of content — file, snapshot — holds counted refs. Nothing keeps content alive outside the count. |
| M13 | No two stores that can name one remote key keep separate counts, and the absence of a record is never evidence that an object is unreferenced. |
| M14 | Refs name hashes, never blocks. |
| M15 | A restore or clone is an adoption, and never copies a count or a location. |
| M16 | A commit is idempotent per block name, and a relocation never targets its source's name. |

M1, M3, M4, M10 and M16 are the ones whose violation loses data or serves wrong
content. M6, M7, M12, M13 and M15 are sweep's safety, without which I3 cannot
hold. M8 and M9 are the ones whose violation stops the system.

## 10. Consequences for RFC 0

This document changes two statements in [RFC 0](rfc-0-data-lifecycle.md). Both need amending there, so that
the set does not carry two answers.

1. **[RFC 0 §4.2](rfc-0-data-lifecycle.md#4.2%20The%20residency%20function), the residency function.** The metadata column is [§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class)'s
   three classes — hole, uncarved, carved. The row "chunk exists, block not
   durable" is unreachable by [§4.2](#4.2%20Only%20after%20durability). The outcomes are unchanged, and I1 now holds
   across the window before the first offload.
2. **[RFC 0 §5.1](rfc-0-data-lifecycle.md#5.1%20Write), the write path.** Step 1's "updates size and mtime" moves after
   the journal stages the bytes, per [§3.4](#3.4%20Ordering%20against%20the%20journal). Authorisation stays first.

## 11. API surface and observability

### 11.1 Interface

Signatures are indicative; the obligations are normative. Each caller holds only
the view it declares ([§1](#1.%20Purpose)): the engine all of it, the namespace `Size`,
`Times` and `Allocation`, sweep the last group.

```go
type Stamp uint64

type Ref struct {
    File           FileID
    Offset         int64
    Hash           Hash
    Skip, Length   int64
    Oldest, Newest Version
}

// FileCommit is one file's share of a block commit.
type FileCommit struct {
    File  FileID
    Stamp Stamp // captured at offer (§2.4)
    Refs  []Ref
}

type BlockCommit struct {
    Name       BlockName
    Generation uint32
    Encodings  []Encoding  // as the store reported them (RFC 5 §5.3)
    Chunks     []ChunkAt   // hash and position of every chunk the block carries
    Files      []FileCommit
}

// CommitResult says, per file, which extents are now durable: applied refs and
// refs found already committed (§4.4). Dropped and refused refs are absent.
type CommitResult map[FileID][]Extent

type Existence interface {
    RecordWrite(ctx context.Context, file FileID, off, n int64, at time.Time) error // §3.4
    Truncate(ctx context.Context, file FileID, size int64) error                   // §6.2
    Deallocate(ctx context.Context, file FileID, off, n int64) error               // §6.2
    Size(ctx context.Context, file FileID) (int64, error)
    Times(ctx context.Context, file FileID) (mtime, ctime time.Time, err error)
    Allocation(ctx context.Context, file FileID, off int64) (Span, error)          // SEEK_DATA / SEEK_HOLE
}

type Content interface {
    Stamp(ctx context.Context, file FileID) (Stamp, error)
    Commit(ctx context.Context, c BlockCommit) (CommitResult, error)                // §4.1
    PruneRemovals(ctx context.Context, file FileID, upTo Stamp) error               // §2.4
    Covering(ctx context.Context, file FileID, off, n int64) iter.Seq2[Span, error] // §8.1
    Durable(ctx context.Context, hash Hash) (ChunkAt, bool, error)                  // §8.2
    RewritePosition(ctx context.Context, hash Hash, block BlockName, r Range) error // §2.2
    Clone(ctx context.Context, src, dst FileID, srcOff, dstOff, n int64) error      // §6.6
    Release(ctx context.Context, file FileID) error                                 // §6.4
    Refs(ctx context.Context, file FileID) iter.Seq2[Ref, error]                    // §8.4
    DurableFloor(ctx context.Context, file FileID) (Version, error)                 // §8.4
}

type Sweep interface {
    DeadBlocks(ctx context.Context, after BlockName) iter.Seq2[BlockName, error]     // live == 0
    Retire(ctx context.Context, block BlockName) error                               // §7.1
    Relocate(ctx context.Context, from, to BlockName, gen uint32, enc []Encoding, moved []ChunkAt) error // §7.3
    Census(ctx context.Context) iter.Seq2[EncodingCount, error]                      // RFC 5 §5.3
    Audit(ctx context.Context) iter.Seq2[Mismatch, error]                            // §7.5
}

var (
    ErrLive          = errors.New("blockmeta: block still live")        // Retire refused
    ErrChunkRetired  = errors.New("blockmeta: adopted chunk retired")   // §7.2
    ErrSameName      = errors.New("blockmeta: relocation onto source")  // §7.3
    ErrInconsistent  = errors.New("blockmeta: count underflow")         // §6.3
)
```

A serialisation conflict is retried inside the call under the caller's deadline
and never returned ([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries)).

### 11.2 Observability

| Answers | Metric | Type |
| --- | --- | --- |
| commits, labelled `result` = `ok`, `adoption_refused` or `error` | `dittofs_blockmeta_commits_total` | counter |
| refs a commit did not apply, labelled `reason` = `removed` (stamp), `older` (version) or `already_committed` | `dittofs_blockmeta_refs_skipped_total` | counter |
| time per commit, per covering lookup, per existence write, by `op` | `dittofs_blockmeta_op_seconds` | histogram |
| retirements, labelled `result` = `ok` or `live` | `dittofs_blockmeta_retire_total` | counter |
| relocations refused onto their source's name | `dittofs_blockmeta_relocate_same_name_total` | counter |
| underflows ([§6.3](#6.3%20Underflow%20is%20corruption%2C%20not%20a%20boundary)); any nonzero value is an alert | `dittofs_blockmeta_underflow_total` | counter |
| audit mismatches, labelled `kind` = `refcount` or `live` and `direction` = `high` or `low` | `dittofs_blockmeta_audit_mismatches_total` | counter |
| conflicts retried, by `op` | `dittofs_blockmeta_conflict_retries_total` | counter |
| removal records held; a value that only grows means pruning stopped | `dittofs_blockmeta_removals` | gauge |

An underflow logs the chunk or block at `Error`. An audit mismatch logs the
record, its stored and recomputed counts at `Error` when low (sweep hazard) and
at `Warn` when high (leak). A refused or skipped ref logs at `Debug` only: it is
routine under concurrency, and the counter says how routine.

## 12. Conformance

[RFC 1 §11](rfc-1-journal.md#11.%20Conformance) applies unchanged: conformance is every **MUST** holding, and a check is
validated by reverting the code and watching it fail on its own assertion.

Every check runs against every backend through one shared conformance suite. A
property that holds on one backend and not another is the category of defect this
document was written after.

### 12.1 Group A — wrong content, lost content

| Requirement | Check |
| --- | --- |
| [§3.2](#3.2%20Every%20offset%20is%20in%20exactly%20one%20class) hole vs uncarved | Write past EOF, do not offload, drop the journal's extent. Assert the gap reads zeros and the written range **fails**. A rig that only checks the zeros passes the build that serves zeros for both. |
| [§3.4](#3.4%20Ordering%20against%20the%20journal) no reconstruction | Crash with journal bytes past recorded `size`. Assert `size` is not grown on restart. |
| [§3.5](#3.5%20Operations%20that%20make%20holes) allocate | Allocate a range with nothing staged. Assert it reads zeros, not a failure. |
| [§4.4](#4.4%20Commits%20for%20one%20file%20apply%20in%20order) commit order | Commit B, then commit A over the same offsets with a lower `newest`. Assert B's refs survive. Commit again with a `newest` inside B's range. Assert nothing changes and the extent is reported durable. |
| [§4.1](#4.1%20What%20one%20commit%20records) idempotent commit | Commit one block name twice with identical chunks, from two files. Assert `live` equals the chunk records naming the block, and both files' refs apply. |
| [§6.1](#6.1%20A%20refcount%20is%20exactly%20its%20refs) refcount | Over random interleavings of commit, truncate, deallocate and delete, assert after every transaction that each refcount equals a count of refs naming it. |
| [§6.2](#6.2%20Truncation%20and%20deallocation) stamp | Offer, truncate, commit. Assert the refs past the new size are dropped, every other ref applies, and no ref lies past `size`. Then offer, deallocate a range the pass did not carve, commit. Assert every ref applies. |
| [§6.3](#6.3%20Underflow%20is%20corruption%2C%20not%20a%20boundary) underflow | Force a double decrement. Assert the transaction fails and the count is unchanged. |
| [§7.1](#7.1%20Conditional%20retirement), [§7.2](#7.2%20Adoption%20is%20conditional%20on%20existence) sweep race | Interleave `Retire` and an adopting commit in every order. Assert that either the block survives with the new ref, or the commit fails, and never a ref to a retired chunk. |
| [§7.1](#7.1%20Conditional%20retirement) retire only its own | Commit a chunk in block K1, commit the same chunk carried in K2, retire K2. Assert the chunk record survives, naming K1. |
| [§6.5](#6.5%20Who%20owns%20a%20ref) snapshot counted | Snapshot a file, delete the file. Assert every chunk's refcount is still nonzero and its block is not retirable, with no other liveness input configured. |
| [§6.6](#6.6%20Clone%20and%20server-side%20copy) clone uncarved | Write a source without offloading, clone it, drop the source's journal extent. Assert the destination reads the written bytes, not a failure and not zeros. |
| [§7.3](#7.3%20Relocation) relocation | Relocate a block's chunks. Assert no ref changed, every read still resolves, and the old block is retirable. Relocate onto the source's own name. Assert `ErrSameName` and no record changed. |
| [§7.4](#7.4%20Restore) restore after retire | Take an uncounted copy, retire one of its chunks, restore. Assert the restore fails and wrote nothing. |
| [§2.6](#2.6%20The%20scope%20of%20a%20count) two stores | Point two stores at one remote namespace. Assert the configuration is refused, or that keys differ. |
| [§8.4](#8.4%20Reseed%3A%20a%20file%27s%20refs%20and%20its%20durable%20floor) floor | Commit refs up to version 7, then `DurableFloor`. Assert 7. |

### 12.2 Group B — cost

| Requirement | Check |
| --- | --- |
| [§5.2](#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed) amplification | Write a file of *N* chunks for several *N*. Assert records **written** per commit are constant in *N*. A correctness assertion on the resulting refs passes a quadratic implementation. |
| [§3.3](#3.3%20Why%20holes%2C%20not%20written%20extents) out-of-order writes | Write a file of *N* MiB as 1 MiB writes in shuffled order, for several *N*. Assert the file ends with no hole records, and that records written per write are constant in *N*. |
| [§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths) write sets | Stream appends to one file while its offload commits. Assert every commit succeeds without a retry caused by the writer. |
| [§8.1](#8.1%20Covering%20lookup) lookup | Assert records **read** per covering lookup grow at most logarithmically in *N*, counting index iterator steps as well as row loads: a scan restarted per candidate is visible only in keys scanned. |
| [§8.1](#8.1%20Covering%20lookup) declared | Build every backend against the interface with the lookup method. A backend that lacks it **MUST** fail to compile. |
| [RFC 0 §9.1](rfc-0-data-lifecycle.md#9.1%20Records%20and%20their%20reclamation) (I7) | Store a file's refs across a range of sizes. Assert no single stored value reaches the storage engine's inline threshold, sizing the refs at their worst-case encoding rather than a fixture's. Then append past the old threshold and assert storage reclaimed by the *other* mechanism does not grow. |

### 12.3 What must not stand in

- **A correctness assertion MUST NOT stand in for [§5.2](#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed) or [RFC 0 §9.1](rfc-0-data-lifecycle.md#9.1%20Records%20and%20their%20reclamation).** The quadratic
  implementation returns the right refs, and so does the one filling an
  unreclaimable log. Only a count of writes, or of bytes landing on the far side
  of the threshold, observes either.
- **A fixture MUST NOT set the threshold margin.** Refs whose offsets are small
  encode a third shorter than the worst case, so a bound checked only against
  such a fixture is not the bound the rule asks for.
- **An in-memory backend MUST NOT be the only backend for Group B.** Its costs are
  not any durable backend's.
- **A single-writer rig MUST NOT stand in for [§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths).** The failure needs a client
  stream and an offload on the same file at once.

### 12.4 Benchmarks and targets

Run on the reference box of [RFC 1 §12](rfc-1-journal.md#12.%20Test%20plan%20and%20performance%20targets), against every durable backend, on every
merge. A benchmark reports; the Group B checks above are what fail a build.

| Benchmark | Measures | Target |
| --- | --- | --- |
| Covering lookup, files of 10^3 to 10^7 refs | p50, p99 latency | p99 ≤ 100 µs at 10^7 refs, and within 2× of the 10^3 figure |
| Offload commit of one 64-chunk block, into files of 10^3 to 10^7 refs | records written, p99 latency | records written constant; p99 ≤ 5 ms |
| Existence write, grouped by 64 | commits per write, p99 latency | one commit per group; p99 ≤ 2 ms |
| Appends to one file during its offload | commit retries caused by the writer | zero |
| 64 writers of all-zero data ([§5.3](#5.3%20Hot%20records%20that%20are%20not%20per-file)) | commits/s against distinct data | report; the input to [§13](#13.%20Open%20questions) question 1 |
| Reseed walk ([§8.4](#8.4%20Reseed%3A%20a%20file%27s%20refs%20and%20its%20durable%20floor)) | time per ref | ≤ 2 µs per ref |
| Audit ([§7.5](#7.5%20Audit)) of 10^8 refs | refs/s | ≥ 10^6 refs/s |

A regression of more than 10% is reported and does not block a merge.

## 13. Open questions

1. **The zero chunk** ([§5.3](#5.3%20Hot%20records%20that%20are%20not%20per-file)). Its refcount is the hottest record in any
   deployment with sparse or preallocated files. Recording zero runs as holes
   instead of carving them removes the record entirely. It also means the carver,
   or the engine before it, recognises zero runs, which [RFC 2](rfc-2-carver.md) does not yet specify.
2. **Group-commit window** ([§3.4](#3.4%20Ordering%20against%20the%20journal)). Existence is committed per acknowledged write,
   or per group. What group size keeps a streaming write from paying one metadata
   commit per write is unmeasured.
3. **Existence under a relaxed sync policy.** When the journal acknowledges before
   its record is on disk ([RFC 1 §6.2](rfc-1-journal.md#6.2%20Sync%20policy)), a crash can lose bytes whose existence was
   recorded, and they then resolve to **Lost**. That is truthful, and it is what the
   policy traded away. Whether it should instead resolve to **Absent**, which
   requires existence to be exactly as durable as the journal and no more, wants
   deciding with [RFC 1](rfc-1-journal.md)'s policy table.
4. **Refs of a deleted file** ([§6.4](#6.4%20Delete)). Deferring the decrement keeps delete fast.
   The durable record of pending deletions is one more thing a restart resumes.
   Whether deletion is ever slow enough to justify it is unmeasured.
5. **Snapshot cost** ([§6.5](#6.5%20Who%20owns%20a%20ref)). Counting a snapshot's refs costs O(refs) increments
   when it is taken, and those increments land on the hottest counters ([§5.3](#5.3%20Hot%20records%20that%20are%20not%20per-file)).
   A copy-on-write snapshot, which counts a whole ref set once and copies it only
   when a file diverges, avoids that. But it makes "which refs name this chunk" a
   two-level question, which [§7.5](#7.5%20Audit)'s audit must then answer too.
6. **Mark-sweep instead of counts.** [RFC 0 §8.3](rfc-0-data-lifecycle.md#8.3%20Sweep) chose reference counting, and this
   document specifies it. Mark-sweep has no hot counters and cannot drift. It
   costs a walk of every ref per sweep, and it needs its own answer to adoption
   during the walk. If [§5.3](#5.3%20Hot%20records%20that%20are%20not%20per-file) or [§6.5](#6.5%20Who%20owns%20a%20ref) turn out too costly under measurement, the
   choice belongs in [RFC 0](rfc-0-data-lifecycle.md), not in a second mechanism added beside the first.
7. **Separate metadata servers.** [§1.2](#1.2%20Why%20it%20is%20a%20separate%20RFC%20from%20the%20namespace) keeps both sides in one database. High
   availability, or parallel data servers apart from a metadata server, may want
   them on separate machines. That needs an answer for the two operations
   [§1.2](#1.2%20Why%20it%20is%20a%20separate%20RFC%20from%20the%20namespace) names, and it spans this document, [RFC 7](rfc-7-namespace-metadata.md) and [RFC 8](rfc-8-engine.md).

## Appendix A — where the current code differs

Descriptive, for the refactor. None is a rule to build around, and a difference
**MUST NOT** be closed by amending the requirement.

| Requirement | Code today |
| --- | --- |
| [§2.1](#2.1%20Ref) refs are records | refs are stored as a per-file list: segmented on one backend, loaded whole to commit on another |
| [§2.1](#2.1%20Ref), [§4.4](#4.4%20Commits%20for%20one%20file%20apply%20in%20order) refs carry versions | refs carry no content versions |
| [§2.2](#2.2%20Chunk) one chunk per hash | chunk rows are keyed by file and offset and carry the refcount; durability per hash is a separate marker |
| [§2.3](#2.3%20Block) generation and encodings | not recorded; block names are random |
| [§2.6](#2.6%20The%20scope%20of%20a%20count) scope of a count | shares on one remote config share an unnamespaced key space, and unrecorded objects are deleted by age |
| [§3](#3.%20Existence) existence | not recorded: size is grown from the journal at startup, and holes are derived from gaps between refs |
| [§5.1](#5.1%20No%20record%20is%20written%20by%20both%20paths) disjoint write sets | every offload commit rewrites the per-file record, serialised by a per-file lock |
| [§6.1](#6.1%20A%20refcount%20is%20exactly%20its%20refs) refcount is its refs | refcounts are never incremented, decrements run in separate transactions, `live` is set once |
| [§6.2](#6.2%20Truncation%20and%20deallocation) stamp and removals | no truncation stamp or removal record |
| [§6.3](#6.3%20Underflow%20is%20corruption%2C%20not%20a%20boundary) underflow fails | clamped at zero |
| [§6.5](#6.5%20Who%20owns%20a%20ref) the count is the authority | sweep is a mark phase, and snapshots and open files are protected by hold lists |
| [§6.6](#6.6%20Clone%20and%20server-side%20copy) clone | the refcount increment is missing; local-only clone and server-side copy copy bytes |
| [§7.1](#7.1%20Conditional%20retirement) conditional retirement | read, decide, delete the object, then delete the record, in separate steps |
| [§7.2](#7.2%20Adoption%20is%20conditional%20on%20existence) no grace period | a grace window and an in-process adoption guard |
| [§7.4](#7.4%20Restore) restore adopts | restore writes the copy's records, counts and locations included |
| [§7.5](#7.5%20Audit) audit | checks only that every ref has a chunk row |
| [§8.1](#8.1%20Covering%20lookup) declared, O(log n) | reached by type assertion with a scan fallback, quadratic on one backend |
| [§8.3](#8.3%20Whole-file%20identity) stored identity current | updated only on shrink and punch |
| [§8.4](#8.4%20Reseed%3A%20a%20file%27s%20refs%20and%20its%20durable%20floor) reseed | no reseed and no floor query |
| [RFC 0 §3](rfc-0-data-lifecycle.md#3.%20Identity) one identity | a second content identity with its own index |
