---
rfc: 1
title: "RFC 1 — the journal"
component: journal
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-28-journal-format]]"
aliases:
  - RFC 1
tags:
  - rfc
---
# RFC 1 — the journal

**Status:** draft.
**Audience:** anyone implementing or reviewing the journal. Its on-disk format
and its recovery after a crash are [RFC 28](rfc-28-journal-format.md).

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

DittoFS is a file server. Clients write to it over NFS (Network File System) or
SMB (Server Message Block); each write lands first in a fast local **journal**
and is acknowledged from there. In the background the bytes are cut into
chunks, packed into blocks and uploaded to a remote object store (an S3
bucket). Once uploaded, local copies can be evicted, and a read of evicted data
fetches it back. [RFC 0](rfc-0-data-lifecycle.md) tells the whole story.

The journal is the local half. It keeps recent writes on a local disk so a
client is answered at disk speed, keeps them through a crash, and serves reads
of whatever it still holds. There is one journal per disk, shared by every share
placed on it. It knows nothing about chunks, blocks or the remote store: it holds
extents of files, and it says exactly which ones.

### The problem, in one example

The installation runs on one node, N1, with its journal on an NVMe disk and the
remote tier in the S3 bucket `dfs-data`. Alice's Windows desktop, alice-pc,
keeps her profile in `profiles/alice/ODFC_alice.vhdx`, a 30 GB virtual disk
with one handle open all day.

1. **09:00.** alice-pc writes 64 KiB at offset 2 GiB. The journal appends it as
   one record at the end of its current segment, a 256 MiB file on the NVMe
   disk, gives it content version v41, and the write is acknowledged. When
   alice-pc flushes, the journal forces the segment to the device (fsync)
   before the flush is answered.
2. **09:01.** The engine, which drives the journal and the upload path, asks for
   an **offer**: a frozen view of the file's dirty bytes, here
   `[2 GiB, +64 KiB)` at v41. It cuts them into chunks, uploads a block and
   records it in metadata.
3. While that block uploads, alice-pc overwrites the same 64 KiB. The journal
   now serves v42 for that extent; the offer still reads v41, because the upload
   must send the bytes that were hashed.
4. The block commits and the engine reports `[2 GiB, +64 KiB)` offloaded.

A careless journal loses data here. Marking the extent offloaded by position would
let eviction drop v42, the only copy anywhere: the bucket holds v41. The journal
marks only content no newer than what it offered, so v42 stays dirty, the next
offer picks it up, and a release of it is refused until then.

5. **Overnight** the disk fills and the engine evicts the offloaded parts of the
   file. **Tuesday** alice signs in and Windows reads offset 0. The journal no
   longer holds it, and says so: the read reports the extent as **missing** and
   leaves the caller's buffer untouched. Zeros would hand Windows a corrupt disk
   that looks valid. The engine finds the extent in metadata, fetches it from
   `dfs-data`, and gives it back with a **fill**, which writes only where the
   journal still holds nothing, so it can never cover a newer write.

```text
   alice-pc                                  S3 bucket dfs-data
      │ SMB write, read                          ▲          │
      ▼                                          │ upload   │ fetch
   ┌─────────────────────── engine ─────────────────────────────┐
   │  carve, pack, upload (RFC 2, RFC 3)                        │
   └──┬──────────┬────────────┬─────────────┬─────────────┬─────┘
      │ write    │ read:      │ offload:    │ report:     │ release,
      │          │ bytes, or  │ frozen      │ extents now │ fill
      ▼          │ "missing"  │ offer       │ offloaded   ▼
   ┌───────────── journal, N1's NVMe disk ──────────────────────┐
   │ index in memory, ODFC_alice.vhdx:                          │
   │   [0, 2 GiB)          v17   offloaded                      │
   │   [2 GiB, +64 KiB)    v42   dirty                          │
   │ segments on disk, append only:                             │
   │   0041.seg sealed    0042.seg active  ◄── new records      │
   └────────────────────────────────────────────────────────────┘
```

### The words you need

- **held extent** — an extent of one file the journal can produce, with its
  content version and its offloaded bit ([§2](#2.%20The%20model%20it%20presents)); *extent* is defined in
  [RFC 0 §2.1](rfc-0-data-lifecycle.md#2.1%20Entities).
- **record**, **segment** — one write as stored on disk; an append-only file of
  records, sealed when full. Space comes back only when a whole segment is
  deleted ([§7.1](#7.1%20Releasing%20storage)).
- **content version** — the number ordering a file's writes and removals; where
  two cover one byte, the higher wins ([§4.3](#4.3%20Versions)).
- **offloaded bit** — set only when the engine reports the bytes offloaded in the
  remote tier; eviction needs it ([§2](#2.%20The%20model%20it%20presents)).
- **offer**, **report** — the frozen view of dirty bytes the engine uploads, and
  its statement of which of them are now offloaded ([§3.3](#3.3%20Offload)).
- **release**, **fill** — no longer holding offloaded bytes (eviction), whose
  space repack then returns, and placing fetched remote bytes back
  ([§3.5](#3.5%20Release), [§7.2](#7.2%20Repack), [§3.4](#3.4%20Fill)).

### What this RFC promises

- An acknowledged write survives the process dying at once, and a host crash or
  power loss once synced: by the client's flush, or within a fixed time bound
  (1 s proposed).
- A read never returns zeros for an extent the journal does not hold. It names
  that extent as missing, exactly, and leaves the bytes untouched.
- Bytes not reported offloaded are never released: such a release is
  refused, whole.
- A fill never overwrites held bytes, and is refused if the file changed since
  the read that found the gap.
- At its limit a write is refused at once with a named error; it never waits and
  never overruns. Truncates, deletes and releases still go through, from space
  set aside for them. After a crash the journal rebuilds itself from its own
  files alone ([RFC 28 §3](rfc-28-journal-format.md#3.%20Recovery)).

### How the rest is organised

- §1–§2: what the journal is for, and the model of held extents it presents.
  Read these first.
- §3: the interface. §3.1–§3.6 are the core; §3.7–§3.11 (statistics, loss
  events, metrics, settling removals, snapshot holds) can wait.
- §4: the in-memory index and versions; §5: durability and sync; §6: capacity.
- §7: freeing space (release, repack); §8: locking.
- §9: tests and benchmarks; §10: open questions.
- [RFC 28](rfc-28-journal-format.md) is the rest of the journal: its on-disk
  format, its recovery after a crash, their checks, and how sync works on each
  platform.

## 1. Purpose

The journal holds bytes on this machine, durably against
process and machine failure, and it answers exactly one question about them:

> **Do I hold these bytes, at what offset, and which version of them?**

It is the component the client's write is acknowledged against, and the component
a read is served from when the bytes are here. There is one journal per device,
and it holds the files of every share placed on that device.

### 1.1 Non-goals

The journal **MUST NOT**:

- decide whether an extent that it does not hold is a hole, evicted, or lost — it
  reports only that it does not hold it ([§3.2](#3.2%20Read));
- return zero bytes for an extent it does not hold;
- know what a chunk or a block is;
- know which remote tier exists, or speak to one;
- import another component in this set ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)).

The journal has no dependency on the metadata store, the syncer or the carver,
and requires no interface from any of them. Its dependency set is the language's
standard library and the operating system's file interface, and an import test
**MUST** enforce that.

### 1.2 What it knows about content

The journal's unit is an **extent of one file's bytes**, named by a `FileID` and a
byte offset. It knows nothing about the structure of that content.

In particular, a **record is not a chunk**. A record is whatever one write
staged, possibly coalesced with adjacent writes. Chunk boundaries are
content-defined and are found later, by the carver, over the extents the journal
offers at offload. The journal **MUST NOT** assume any relationship between its
records and any chunk, block or remote object.

### 1.3 It is testable on its own

The journal's dependencies are the ones [§1.1](#1.1%20Non-goals) allows, and each is supplied:

| Dependency | In production | In a test |
| --- | --- | --- |
| a directory on a filesystem | the configured journal path | a temporary directory on a real filesystem |
| time | the system clock | a virtual clock the test advances, for the sync timer and every wait |

No metadata store, no carver, no syncer, no block store. A check **MUST** be
writable as: open a journal on a temporary directory, drive the interface of
[§3](#3.%20Interface), assert on what comes back and what is on disk.

**The filesystem is real, never faked.** Torn tails, directory-entry
durability and whether a write survived are properties of real storage.

**Storage loss is simulated beneath the journal, not by killing it.** The journal
reaches its segment files through a narrow, package-internal seam — open,
allocate, write at an offset, read, sync, truncate, close, unlink. A test wraps the real file and remembers every
write since the last sync; a simulated crash discards exactly those and reopens
the journal on the same directory. Killing the process instead leaves unsynced
writes in the page cache, and proves a durability the journal does not have.

**Time is virtual.** The sync timer ([§5.2](#5.2%20Sync%20policy)), write stalls and every bounded wait
run on the virtual clock, so the journal **MUST NOT** read time from anywhere the
virtual clock cannot replace.

## 2. The model it presents

For each `FileID`, the journal maintains a set of **held extents**: disjoint
`(fileOffset, length)` extents whose bytes it can produce, each mapped to a
location in local storage.

For each held extent it also records one bit of **offload state**: whether the
extent has been reported offloaded ([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)). This bit exists for two
purposes and no others — selecting what to offer at offload, and refusing an
unsafe release ([§5.1](#5.1%20Ordering%20rules)). It **MUST NOT** be consulted to answer a read, and the
journal **MUST NOT** expose it as an answer about whether content is offloaded; the journal
is not authoritative for that ([RFC 0 §4.1](rfc-0-data-lifecycle.md#4.1%20The%20two%20oracles)).

The bit can only be wrong in the safe direction: set solely by being told, and
only where no held content is newer than what was reported ([RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)), it can call
an offloaded extent dirty — a redundant offload — but never a dirty one
offloaded.

Every held extent also carries the **content version** of its bytes ([§4.3](#4.3%20Versions)).
Where a truncate, deallocate or delete removed content, the journal remembers the
removal and its version — a **removal marker** ([§3.6](#3.6%20Truncate%2C%20deallocate%20and%20delete)) — until the caller
settles it, so the caller can still learn of a removal its metadata has not yet
recorded ([§3.10](#3.10%20Settle%20and%20Since)). At every byte of a file, **the newest version the journal
has applied wins**.

Held extents are **disjoint and need not be adjacent**: an implementation **MUST
NOT** assume a file's held content is contiguous, nor that it begins at offset
zero.

**What an extent is, by example.** An extent is a region of one file's bytes:
a `FileID`, a start offset and a length, nothing more. When alice-pc writes
64 KiB at offset 1 GiB into `ODFC_alice.vhdx`, that write is the extent
*(alice.vhdx, 1 GiB, 64 KiB)*. An extent says *which bytes of the file*, never
*where on disk* they sit and never *what content* they hold; the record that
stores them says where, and its content version says which bytes these are.

Extents split and shrink as later writes land on them:

```text
 file offset   0        4K       8K       12K
               ├────────┴────────┤                  write v7: extent (0, 8K)
                        ├────────┴────────┤         write v8: extent (4K, 8K)

 held extents  ├── v7 ──┼────── v8 ───────┤
 afterwards    (0, 4K)   (4K, 8K)
```

The first write's extent was (0, 8K). The second overwrote its upper half, so
the journal now holds two extents for the file: (0, 4K) at version 7 and
(4K, 8K) at version 8. The bytes of v7 between 4K and 8K are still inside the
first record on disk, but they are no longer *held*: a read of 4K–8K gets v8.
An extent is not a chunk either — chunk boundaries are cut later from content
([§1.2](#1.2%20What%20it%20knows%20about%20content)) — and not a record: one record
may end up as several held extents, or none.

![One file's held extents with gaps between them, the missing extents a read reports, and the three different causes a gap can have](img/rfc1-file-extents.svg)

The gaps carry no explanation. An extent never written, an extent released by
eviction, and an extent whose bytes were lost are the same observation to the
journal — it does not hold them — and it **MUST** report all three identically.
Resolving which is which is [RFC 0 §6.1](rfc-0-data-lifecycle.md#6.1%20Resolution), and it belongs to the engine.

A gap is a gap in the file, not in storage. The journal never writes into space
freed inside a segment: every record is appended at the end of its stream
([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)), and space goes back to the filesystem only when a whole segment is
unlinked ([§7.1](#7.1%20Releasing%20storage)), after repack has carried its live records forward
([§7.2](#7.2%20Repack)). A write that fills a gap in a file lands wherever its stream
stands, like any other write.

![The same held extents mapped to records scattered across three segments in append order, interleaved with another file](img/rfc1-placement.svg)

Nothing relates an extent's position in the file to its position in storage.
Extents land wherever the append stream stood when they were written, so a
sequential file is stored out of order, interleaved with other files.

`FileID` **MUST** be a distinct type constructible only from a file `ID`
([RFC 0 §3](rfc-0-data-lifecycle.md#3.%20Identity)). File IDs are unique across the store, so files of many shares share one
journal without colliding, and a `FileID` **MUST NOT** encode a share: the share a
file belongs to is given by the handle it is written through ([§3](#3.%20Interface)), never
derived from its identifier.

## 3. Interface

```go
Init(dir string, install InstallationID) (JournalID, error)
Open(dir string, floor Version, expect JournalID) (*Journal, error)
(*Journal) Share(tag ShareTag, limit int64, floor Version, imported bool) (*Handle, error)
(*Journal) Forget(tag ShareTag) error
```

One journal serves several shares. Each share's engine works through its own
`Handle`, and every operation below is a method of it. The handle's tag is
written into every record the share appends ([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)), so recovery rebuilds each
share's accounting ([§6](#6.%20Capacity)); a file belongs to one share for its life. `Open`'s `floor` is
the version floor of [RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding); `expect` is the identity the caller has recorded for
this directory ([RFC 28 §3.4](rfc-28-journal-format.md#3.4%20Unattachable%20files)).

**Only an explicit initialisation step creates a journal.** `Init` creates a new
journal in a directory that holds none, writing into its `format` file
([RFC 28 §2.1](rfc-28-journal-format.md#2.1%20Layout)) a fresh journal identity and the **installation ID** it is given — the
one the same initialisation step writes into the metadata store's bootstrap
([RFC 16](rfc-16-metadata-store.md)) — and returns the identity for the caller to record. `Open` never
creates one ([RFC 28 §3.4](rfc-28-journal-format.md#3.4%20Unattachable%20files)). So a start that finds a journal and no metadata store, or
a metadata store and no journal, can tell which side is missing instead of
taking either for a first start.

**A share attached after open raises the counter first.** `Share`'s `floor` is
the share's own version floor: the highest content version recorded for any file
of the share, imported records included. A share this journal did not serve when
it opened — moved here, recovered onto this device, cloned or restored into it —
can carry versions from another journal far above this one's counter. `Share`
**MUST** raise the journal's version counter above that floor before it returns
the handle, so no write through it can be assigned a version below a version its
files already hold ([§4.3](#4.3%20Versions)). Without it a new write would lose precedence to the
imported content it overwrote, be reported offloaded, and be released while the
older bytes are served.

**A share attached as an import holds none of its old extents.** `imported` is
set when the share arrives from another installation — a move back, a recovery
import ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)). If the journal still holds anything under `tag`, `Share`
**MUST** `Forget` the tag before it returns the handle, or refuse naming the tag.
Otherwise the journal would serve its own stale, clean extents, keyed by FileIDs
the share kept, over what the other installation wrote since.

**`Forget` drops a tag whole, and leaves nothing to settle.** It appends one
header-only **forget record** naming the tag, durable before it returns, and
drops from the index every held and retained extent, removal marker and hold
mark of the tag's files; recovery applies the record to every record of the tag
with a lower sequence number, so none of them is held, retained or rebuilt as a
marker again ([RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)). `Since` never yields it and no caller settles it: unlike
a `Delete` per file, whose markers wait for a metadata transaction ([§3.6](#3.6%20Truncate%2C%20deallocate%20and%20delete)),
`Forget` is used exactly where no such transaction will come. It is the
**detach** of a move's drop ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step), step 9) and of an import's attach
above. `Forget` **MUST** be refused while a handle of the tag is open. The record
draws on the reserved headroom ([§6](#6.%20Capacity)), and is live while any segment holds a
record of the tag below its sequence number ([§7.2](#7.2%20Repack)).

**A tag no share claims is resolved before serving.** After a recovery import,
which gives the shares new identities ([RFC 26 §2.3](rfc-26-catalog-backups.md#2.3%20Restore)), or a share's deletion, the
journal can hold records under a tag no share of the installation claims; its
dirty extents would never be offered and never become evictable. The caller
**MUST**, before it serves, either export each such tag's held extents for
salvage — read through a handle attached for the purpose, then `Forget` — or
`Forget` it at once, and report every such tag with its held and dirty bytes.
`Stats` names every tag that holds a record ([§3.7](#3.7%20State%20introspection)).

### 3.1 Write

```go
WriteAt(id FileID, off int64, p []byte, mtime int64) (v Version, err error) // mtime: ns since the Unix epoch, 0 when the open suspended it (RFC 28 §2.3)
Sync(ids ...FileID) error
```

Stages `p` at `off`, with the modification time the write sets ([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)), and makes it durable within the sync bound
([§5.2](#5.2%20Sync%20policy)). On return with `err == nil`, the extent `(off, len(p))` is held, is
**Dirty**, and **MUST** survive process death.

`WriteAt` **assigns** the write's content version `v` and returns it. The version
**MUST** be assigned inside the file's serialised append ([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)), so that for one
file, versions the journal assigns are appended in the order they were assigned.
Otherwise a lower version can land after a higher one and an offload between them
offers the higher without the lower, breaking [RFC 6 §4.4](rfc-6-block-metadata.md#4.4%20Commits%20for%20one%20file%20apply%20in%20order).

`Sync` returns once every operation on the named files that returned before the
call is durable. It is the journal's part of a client's
flush ([§5.2](#5.2%20Sync%20policy)).

`WriteAt` **MUST** reserve capacity before accepting bytes ([§6](#6.%20Capacity)) and **MUST**
fail rather than exceed the configured limit.

A write overlapping a held extent supersedes it wherever its version is newer,
which for a version `WriteAt` assigns is everywhere. The journal **MUST** serve the
newer bytes thereafter, and **MUST NOT** require the caller to invalidate first.

### 3.2 Read

```go
ReadAt(id FileID, off int64, p []byte) (n int, missing []Extent, held []Held, asOf Seq, err error)

type Held struct {
    Extent  Extent
    Version Version // the content version of the bytes served for this extent
}
```

Fills `p` from held extents. Every sub-extent of `(off, len(p))` that the journal
does not hold **MUST** be reported in `missing`, and the corresponding bytes of
`p` **MUST** be left untouched.

`ReadAt` **MUST NOT** zero-fill an extent it does not hold, and **MUST NOT**
return a short read in place of a `missing` entry. The caller distinguishes
hole from eviction from loss ([RFC 0 §6.1](rfc-0-data-lifecycle.md#6.1%20Resolution)); the journal supplies only the
absence.

`missing` **MUST** be exact: an implementation **MUST NOT** widen it to a whole
segment, record or block for convenience, because the caller pays a remote
transfer per entry.

`held` names the content version of each sub-extent served; whether that is
current anywhere else is the caller's question.

`asOf` is the file's change sequence at the moment of the read: the highest
sequence number of any operation that has changed the file's extents ([§3.4](#3.4%20Fill), [§4.3](#4.3%20Versions)). A caller that fetches
a `missing` extent passes it back to `Fill`.

### 3.3 Offload

```go
Offload(id FileID, limit, widen int64, fn func(o Offer, report func(offloaded []Extent) error) error) error

OffloadMany(ids []FileID, limit, widen int64, fn func(offers []Offer, report func(id FileID, offloaded []Extent) error) error) error

type Offer struct {
    ID         FileID
    Dirty      []Extent
    Neighbours []Extent    // offloaded held extents offered on request (widen); see below
    Superseded []Held      // superseded versions the journal still retains for offload (§3.11)
    Oldest     Version     // the oldest content version among the offered records, neighbours included
    Newest     Version     // the newest
    Offered    io.ReaderAt // the bytes as offered; see below
}
```

Offers held extents of `id` whose offloaded bit is unset, and marks exactly the
extents reported offloaded through `report`.

**Offload is how dirty content becomes evictable.** The journal uploads nothing.
It hands the engine a frozen view of a file's dirty content, the **offer**; the
engine carves, uploads and commits it; and as each block's commit lands, the
engine tells the journal which extents are now offloaded. Those extents get their
offloaded bit, and only then can `Release` evict them ([§3.5](#3.5%20Release)). Offload makes
content evictable; `Release` evicts it. The name is the lifecycle step this call
serves ([RFC 0 §5.2](rfc-0-data-lifecycle.md#5.2%20Offload)).

**What an offer is.** A snapshot of one file's dirty content, taken when the call
starts:

- `Dirty`: the extents offered;
- `Oldest`, `Newest`: the range of their content versions;
- `Offered`: a reader that returns exactly those bytes for as long as `fn` runs.

Frozen means a client write during the pass changes what `ReadAt` serves, but
not what `Offered` returns. The carver hashes the bytes, and the uploader reads
them again later to send them ([RFC 3 §3.2](rfc-3-syncer.md#3.2%20It%20holds%20a%20reference%2C%20not%20a%20copy)); both must see the same bytes, or the
uploaded block would not match its hashes.

> **Example.** The offer covers `[0, 4)` MiB at v10. While its block uploads, a
> client writes `[1, 2)` MiB at v11.
>
> - `ReadAt` on `[1, 2)` returns v11. `Offered` on `[1, 2)` still returns v10: the
>   v10 record stays in its segment until `fn` returns.
> - The block commits, and the engine reports `[0, 4)`. The journal marks
>   `[0, 1)` and `[2, 4)`. `[1, 2)` stays dirty: the remote holds v10, the journal
>   holds v11, and marking it would let eviction drop v11, the only copy.
> - The next pass offers `[1, 2)` at v11.

**Offloaded neighbours are offered on request.** With `widen` above zero, each
offer also carries, in `Neighbours`, held extents whose offloaded bit is set and
that are contiguous with an offered dirty extent, up to `widen` bytes on each
side. They are frozen in `Offered` exactly like the dirty bytes, and counted in
`Oldest` and `Newest`. They are what the engine reads when it widens a run to
re-tile a ref the run partly replaces ([RFC 8 §6.7](rfc-8-engine.md#6.7%20A%20run%20is%20what%20the%20journal%20offers%2C%20widened%20only%20to%20re-tile)); without them it would read
bytes that a concurrent write or release can change under the carver. A report
naming a neighbour changes nothing: it is already marked.

**`report` marks offloaded extents block by block.** One offer is usually committed as
several blocks, and their commits land at different times. The engine calls
`report` once per committed block, naming that block's extents, and each call
marks them at once, so they become evictable without waiting for the rest of the
pass.

> **Example.** A pass offers 64 MiB, carved into 16 blocks of 4 MiB. Block 3
> commits first: `report([8, 12) MiB)` makes those 4 MiB evictable at once. Block
> 9's upload then fails and `fn` returns an error. Everything reported so far
> stays marked; the rest stays dirty and a later call offers it again.

- `report` **MAY** be called any number of times while `fn` runs.
- Once `fn` returns, `report` **MUST** fail: the offer is over, and its records
  may have moved or been superseded. It also returns the error of the offloaded
  record it appends ([RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)) — the exhausted headroom of [§6](#6.%20Capacity), or a failed
  write — and then marks nothing; the extents stay dirty and a later offer
  carries them again.
- The error `fn` returns covers only what was not reported; it takes back nothing.

**A `report` costs what it reports.** Marking *k* extents **MUST** cost
O(*k* log *n*), where *n* is the number of extents the file holds, and **MUST NOT**
scan the file's other extents to decide anything about the reported ones.

> **Example.** A 160 GB file written in 1 MiB writes holds about 150,000 extents.
> Reporting one 4 MiB block touches four of them, and must find those four through
> the index. A `report` that walks all 150,000 does it once per block — about
> 40,000 times over the file — inside the lock the file's writes and reads also
> need. Writes to that file slow as it grows, and stall in the end.

**`limit` bounds the dirty bytes one call offers.** The journal offers extents in
offset order until the next would pass the limit, and offers an extent larger than
what remains as a prefix ending at the limit. Without a bound, one pass over a
large dirty file pins all of it until the last block uploads. The engine chooses
the limit ([RFC 8 §6.2](rfc-8-engine.md#6.2%20When%20a%20file%20is%20offered)). A prefix ends at the limit, not at a boundary the content
chose, so its end is **artificial**: the carver cuts nothing past its last
content-chosen boundary and leaves the tail dirty ([RFC 2 §2.4](rfc-2-carver.md#2.4%20An%20artificial%20end%20leaves%20the%20tail%20uncut)). Nothing in that
tail is reported, so the next call's offer starts at the first dirty byte, which
is that boundary. A limited pass adds no chunk boundary the content did not
choose.

**An offer pins a bounded number of segments.** Every segment holding an offered
record stays on disk, unmoved, until `fn` returns ([§3.3](#3.3%20Offload), [§7.2](#7.2%20Repack)). Small scattered
extents can come from a different segment each: a 64 MiB pass over 64 KiB random
overwrites can touch a thousand segments, none of which repack can retire while
a slow upload runs. So the journal stops adding extents to an offer once the next
would bring the segments it pins above a fixed bound (proposal: 64), as it
stops at `limit`, and offers the rest in a later call. An extent in a segment
the offer already pins never counts against the bound. **Pins are bounded across
offers too**: the journal counts each pinned segment once, however many running
offers pin it, and once that count reaches a journal-wide bound (proposal: 256)
an offer adds no extent from a segment not yet pinned beyond its first. An offer
never takes an extent from a segment a running repack pass has selected
([§7.2](#7.2%20Repack)): it takes that extent from its copy once repack has repointed it, and
otherwise leaves it to a later call, so an offer never pins a segment repack has
started on. The first extent always fits, so an offer is never empty while dirty
extents outside segments under repack remain, and pinned segments are bounded by
the journal-wide bound plus one per running offer.

> [!note] decision
> Both bounds are constants of the implementation, not settings, and neither is
> measured. 64 lets one offer gather a default 4 MiB block of 64 KiB extents
> even when every extent sits in its own segment; 256 lets four such offers run
> at once. Overturned by a journal held at its limit ([§10](#10.%20Open%20questions), question 4)
> where pins keep repack from retiring segments, or where offers end at the
> bound often enough that blocks fall short of their target.

**`Oldest` and `Newest`** bound the content versions of the records in the offer
([§4.3](#4.3%20Versions)). The engine records both on the refs the pass commits ([RFC 6 §4.4](rfc-6-block-metadata.md#4.4%20Commits%20for%20one%20file%20apply%20in%20order)) and
names them again when it reseeds ([RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)); the two bounds are all reseed and the stale rule
need.

`OffloadMany` is the same offer over several files in one callback, so the engine
can pack chunks of several files into one block ([RFC 2 §5.3](rfc-2-carver.md#5.3%20A%20block%20packs%20chunks%2C%20whichever%20files%20they%20came%20from)). Every rule of this section
holds per file within it, and `limit` bounds the total across the files.
`Offload(id, …)` is `OffloadMany` with one file, and an implementation **SHOULD**
build it that way.

The journal **MUST** mark exactly the reported extents and **MUST NOT** mark an
extent for which no report was received, including when `fn` returns an error
after some reports — a partial success **MUST** be honoured.

The journal **MUST NOT** interpret, reorder or subdivide a report except to
intersect it with what it offered. A reported extent that was not offered **MUST**
be rejected as an error, not silently accepted.

Offered extents **MUST** remain readable and **MUST NOT** be moved for the
duration of the call.

`Offered` reads at file offsets, and only inside the offered extents for the
duration of `fn`; a read outside them, or after `fn` returns, **MUST** fail rather
than return other bytes. **`Offered` returns no unverified byte**: before it
returns any byte of a record, it **MUST** have read that record's whole payload
and verified its payload checksum during this offer ([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)). A record that does
not verify is dropped as corrupt ([RFC 28 §3.3](rfc-28-journal-format.md#3.3%20Torn%20and%20corrupt%20records)) and the read fails, so the engine
abandons that part of the pass. Without it, rotted bytes would be hashed, uploaded
under a valid name, committed and marked offloaded: corruption laundered into a
block that verifies. A write that supersedes an offered extent while `fn` runs
is permitted and changes what `ReadAt` returns; it **MUST NOT** change what
`Offered` returns, so the superseded record **MUST** stay in its segment until `fn`
returns. The superseding write's offloaded bit **MUST** remain unset even if `fn`
reports its offset offloaded: the report is about the offered bytes, not the newer
ones.

**A removal does not interrupt an offer.** A `Truncate`, `Deallocate` or `Delete`
while `fn` runs takes effect at once for `ReadAt`, and **MUST NOT**
change what `offered` returns: the offered records stay on disk, unmoved, until
`fn` returns. A report naming an extent removed meanwhile marks nothing, because
nothing is held there. The engine drops the removed refs at commit
([RFC 8](rfc-8-engine.md)).

### 3.4 Fill

```go
Fill(id FileID, off int64, p []byte, asOf Seq, v Version) error
```

Places retrieved remote bytes into local storage. This is the only path by which
an extent becomes held without a client write.

`Fill` **MUST NOT** overwrite any part of an extent the journal already holds; it
**MUST** write only the sub-extents that are currently absent, and it **MUST**
make that determination and the write atomic with respect to concurrent
`WriteAt` on the same file.

**A Fill older than the file is refused.** `asOf` is the sequence `ReadAt` returned
when the caller found the extent missing ([RFC 8 §7.2](rfc-8-engine.md#7.2%20The%20reply%20streams%2C%20one%20verified%20chunk%20at%20a%20time)). The journal keeps, per file, the
highest sequence number of any operation that changed its extents — `WriteAt`, `Release`,
`Truncate`, `Deallocate` and `Delete`. If it
is newer than `asOf`, `Fill` **MUST** write nothing and return a distinct error.
The per-file sequence is bounded by **floor buckets**: a fixed
table of 4,096 sequence numbers, indexed by a hash of the `FileID`. A file's entry
is dropped when the file holds no extent and no removal marker — after a `Delete`,
or once everything it held was released — and dropping it raises the file's
bucket to the entry's sequence number. A file with no entry is compared against
its bucket. At open every bucket starts at the highest sequence number on disk:
no read that began before the open can still fill. Without this, a fetch that
began before a write, or before a truncate down and up, could land after it and
put old bytes back, marked offloaded. The check is deliberately coarse: a needless
refusal costs only a cache miss.

> [!note] decision
> Floor buckets, not one journal-wide floor. With one floor, every delete in the
> journal refuses every in-flight fill of every file that holds nothing, so a
> delete-heavy share — a build tree's temporary files — keeps every other share's
> cold reads from filling. A bucket confines that to about one file in 4,096.
> Overturned by a measured `fill_refused` rate under a delete-heavy load that
> 4,096 buckets do not bring below one refusal per thousand fills.

A filled extent's offloaded bit **MUST** be set, because the content came from the
remote tier and is by construction offloaded. **This makes `Fill` as
safety-critical as `Release`.** The caller **MUST** pass only bytes it retrieved
from the remote tier for that exact extent of that exact file; anything else
becomes content the journal will release while it exists nowhere.

`v` is the content version of the ref the bytes were fetched from: its `newest`
([RFC 6 §2.1](rfc-6-block-metadata.md#2.1%20ChunkRef)). The filled record carries it, so reseed and the stale rule treat
filled content like any other ([RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)). Its sequence number is new, like every
append's, so where it and a record still on disk carry the same version — the
release record of the content it replaces — the fill wins at recovery ([§4.3](#4.3%20Versions)).

#### Fill is not a write

`Fill` and `WriteAt` take nearly the same arguments and are opposites: a write is
newer than what is held, supersedes it and owes an offload; a fill is older, must
not touch held content, and is offloaded already. An implementation **SHOULD** share
their append machinery and **MUST NOT** expose them as one entry point selected by
a parameter: a caller passing the wrong value would discard a client's write or
make content not yet offloaded evictable, and nothing would fail at the time.

### 3.5 Release

```go
Release(id FileID, extents []Extent) error
```

Stops holding `extents`. This is the mechanism of eviction; the policy is the
engine's ([RFC 0 §8.1](rfc-0-data-lifecycle.md#8.1%20Evict)). It frees no storage inside a record: storage returns
only when a segment is unlinked ([§7.1](#7.1%20Releasing%20storage)).

`Release` **MUST** refuse an extent whose offloaded bit is unset, and **MUST** make
no partial progress on a refused call: the journal's enforcement of [RFC 0](rfc-0-data-lifecycle.md)'s
I2, which the caller is also obliged to keep.

The caller **MUST NOT** call `Release` before the offload commit that made the
content offloaded is itself committed ([RFC 0 §8.1](rfc-0-data-lifecycle.md#8.1%20Evict)). Eviction records nothing
elsewhere; the journal cannot verify this ordering and does not try.

**A release returns no storage.** It turns held bytes into **unreclaimed**
bytes: still allocated in their segment, backing nothing. Storage returns only
when a segment is unlinked — by repack ([§7.2](#7.2%20Repack)), or because the segment is left
holding nothing live, which the journal unlinks on its own once the release
records are durable ([§7.1](#7.1%20Releasing%20storage)). `Release` therefore reports nothing freed;
`UnreclaimedBytes` rises by what it released ([§3.7](#3.7%20State%20introspection)), and `UsedBytes` falls
only when an unlink has happened, never earlier. An engine that reads space free
before it is and retries a refused write gets the refusal again ([§6](#6.%20Capacity)).

**A release is recorded.** `Release` appends one **release record** per released
extent, naming the file, the extent and the content version released there, with a
new sequence number ([§4.3](#4.3%20Versions)). Recovery applies it like any record, so an older
record still on disk is not held again after a restart. A release leaves no
removal marker: eviction removes nothing from the file.

The release record **MUST** be durable before the segment holding the released
record is unlinked ([§5.1](#5.1%20Ordering%20rules)). If a crash loses the record, the segment is still
on disk and the released record still whole: held again after recovery, as it
was before the release. A release record draws on the reserved headroom of
[§6](#6.%20Capacity), so a full journal can still release once content writes are refused.

### 3.6 Truncate, deallocate and delete

```go
Truncate(id FileID, size int64) (Version, error)
Deallocate(id FileID, e Extent) (Version, error)
Delete(id FileID) (Version, error)
CloneTarget(id FileID, spec CloneSpec) (Version, error)

type CloneSpec struct {
    Src            FileID
    SrcOff, DstOff int64
    Len            int64
    AsOf           Seq // the source's change sequence the clone resolves at (§3.2)
}
```

`Truncate` stops holding every extent at or beyond `size` and narrows an extent
straddling it. `Deallocate` stops holding `e`, whatever its offloaded bit, and is
how a punched extent stops being served ([RFC 8 §8.2](rfc-8-engine.md#8.2%20Deallocate%20records%20a%20hole%3B%20it%20does%20not%20write%20zeros)). `Delete` stops holding every
extent of `id`. `CloneTarget` stops holding `(spec.DstOff, spec.Len)` of `id` as
`Deallocate` does, and its record and marker carry `spec`: the removal phase 1 of
a clone makes ([RFC 8 §9.1](rfc-8-engine.md#9.1%20Clone%20adopts%20carved%20refs%20and%20copies%20the%20rest%20locally)). `Since` yields that marker with its spec, so a
restart that re-applies it rebuilds the clone and resumes it, rather than applying
a plain removal that would leave the destination reading zeros.

Each is versioned like a write: the journal assigns the version inside the file's
serialised append, appends a record carrying it, and leaves a **removal marker** —
the removed extent and the removal's version, holding no bytes. Content with a
version **below** the marker's is not held inside the extent; content at or above
it is. Markers live in the placement index and cost an entry each.

**A marker lives until the caller settles it.** It exists so the caller can learn
of a removal its metadata has not yet recorded: `Since` yields it ([§3.10](#3.10%20Settle%20and%20Since)).
The engine settles a file after the metadata transaction recording the removal
commits ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)). Recovery rebuilds every marker from the removal records on
disk ([RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)), since settling is not persisted: a marker the caller settled
before a crash comes back and is settled again after it, which costs one
repeated, idempotent metadata step.

> **Example.** Truncate to 1 MiB at v8 leaves a marker over `[1 MiB, ∞)` at v8. The engine
> commits the new size, then calls `Settle(id, 8)`, which drops the marker. Had
> the process crashed between the truncate and the commit, recovery would rebuild
> the marker from the truncate record, and `Since` would hand it to the engine to
> commit.

Each **MUST** be durable on return, and **MUST NOT** require the affected
storage to be reclaimed first — reclamation is asynchronous ([§7](#7.%20Reclamation%20mechanisms)). Their
records draw on the reserved headroom of [§6](#6.%20Capacity), and are never refused by the
journal's own limit; the one refusal they can meet is the exhausted headroom of
[§6](#6.%20Capacity), which only removals of content the journal does not hold can reach.

None of them is gated on the offloaded bit, and an implementation **MUST NOT**
gate one. Discarding content the user removed is not data loss, and an
un-offloaded delete that a crash reverts would resurrect a removed file.

### 3.7 State introspection

```go
Extents(id FileID) ([]Extent, error)      // held extents, offset order
Files() iter.Seq[FileID]                   // files of this share with a held extent or an unsettled removal marker
Stats() Stats                              // journal-wide, with a per-share breakdown
DirtyFiles() iter.Seq2[FileID, time.Time]  // files with an unset offloaded bit, each with its oldest dirty byte's write time
```

`Extents` reports what the journal holds, and is not an answer about what
exists: a caller answering where data or holes lie **MUST** combine it with
metadata, or it will report evicted content as a hole. `Files` and `Since`
([§3.10](#3.10%20Settle%20and%20Since)) together are what the engine re-applies uncommitted existence from
after a crash ([RFC 0 §5.1](rfc-0-data-lifecycle.md#5.1%20Write)).

`DirtyFiles` is what the engine's age tick reads ([RFC 8 §6.1](rfc-8-engine.md#6.1%20The%20work%20queue)), so it
**MUST** be cheap: the journal keeps a per-journal list of dirty files keyed by
their oldest dirty byte's write time, updated when a write sets a file's first
unset offloaded bit and when `Offload` or a removal clears its last, and
rebuilt at recovery. It **MUST NOT** walk the placement index or read a record,
and costs O(dirty files), not O(extents).

`Stats` **MUST** be served from maintained counters: it **MUST NOT** walk the
placement index or the segment set, or block a write. A statistic that only a
walk can produce is omitted.

| Field | Kind | Is |
| --- | --- | --- |
| `MaxBytes` | gauge | the configured capacity ([§6](#6.%20Capacity)) |
| `UsedBytes` | gauge | local storage **allocated**: the segments, each at its accounted size, per [§7.3](#7.3%20Accounting). Clean held bytes, dirty bytes and unreclaimed bytes all count here; it is the measure eviction is triggered on ([§6](#6.%20Capacity)) |
| `ReservedBytes` | gauge | record bytes reserved by in-flight writes inside allocated segments, not yet written |
| `HeldBytes` | gauge | the sum of held extent lengths |
| `DirtyBytes` | gauge | held bytes whose offloaded bit is unset |
| `OldestDirty` | value | when the oldest held extent whose offloaded bit is unset was written; the age of the upload backlog |
| `Files` | gauge | files with at least one held extent |
| `ExtentCount` | gauge | held extents |
| `Segments` / `Sealed` | gauge | segments in total, and sealed |
| `UnreclaimedBytes` | gauge | storage in segments not yet unlinked that backs no held or retained extent and no live record: released or superseded but still on disk; what repack can recover ([§7.2](#7.2%20Repack)) |
| `HeadroomBytes` / `RepackReserveBytes` | gauge | the reserved headroom for records without bytes and the repack reserve, each as allocated now ([§6](#6.%20Capacity)) |
| `PinnedSegments` | gauge | segments held by running offers ([§3.3](#3.3%20Offload)) |
| `OpenDescriptors` / `DescriptorLimit` | gauge | open segment descriptors, and the bound they are held under ([§7.4](#7.4%20Open%20descriptors)) |
| `DamagedSegments` | gauge | segments holding records that do not verify ([RFC 28 §3.3](rfc-28-journal-format.md#3.3%20Torn%20and%20corrupt%20records)) |
| `LastSyncError` | value | the most recent sync failure, and when ([§5.2](#5.2%20Sync%20policy)) |
| `SyncDuration` | histogram | time per sync; the one operation only the journal can time |
| `RemovalMarkers` | gauge | removal markers not yet settled ([§3.6](#3.6%20Truncate%2C%20deallocate%20and%20delete)) |
| `ExtentLimit` | gauge | the configured index entry bound ([§4.2](#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)) |
| `Counters` | counters | the cumulative counts of [§3.9](#3.9%20Metrics), since open |
| `RejectedSegments` | gauge | segments not attached ([RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)) |
| `Recovery` | value | how many segments the last open read by catalog and by scan, and how long it took ([RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)) |
| `Losses` | value | the most recent loss events, each with a sequence number ([§3.8](#3.8%20Loss%20events)) |
| `LossGeneration` | value | raised on every loss of content not yet offloaded and every failed sync window; never lower than any value this process has exposed for this directory ([§3.8](#3.8%20Loss%20events)) |
| `Shares` | per share | every tag that holds a record, attached or not ([§3](#3.%20Interface)), with `Limit`, `UsedBytes` — the record bytes of the share's records still on disk —, `ReservedBytes`, `HeldBytes`, `DirtyBytes` and `UnreclaimedBytes` for each share ([§6](#6.%20Capacity)) |

`DirtyBytes` against `HeldBytes` says whether anything is evictable at all, and
`HeldBytes` against `UsedBytes` whether a full journal is full of content or of
storage no repack has recovered, which `UnreclaimedBytes` names. Since a release
frees nothing by itself ([§3.5](#3.5%20Release)), a capacity trigger that read `DirtyBytes` and
`HeldBytes` alone would see a journal it had just evicted as having room while
every byte stays allocated; and one that read dirty plus unreclaimed bytes alone
would miss clean held bytes, so a device full of clean cached content would fail
writes before eviction began. **The engine triggers eviction on `UsedBytes`, the
allocated occupancy**, against the journal's maximum and each share's limit, and
uses dirty plus unreclaimed bytes only to pace writes; repack, not eviction, is
the step that returns space ([RFC 8 §10](rfc-8-engine.md#10.%20Local%20space)). Unreclaimed bytes are counted to the share whose record
holds them.
`OldestDirty` says how long content has waited to reach the remote tier: a
backlog that grows old is content one device failure away from loss, whatever
its size ([RFC 8 §14](rfc-8-engine.md#14.%20Observability)). The journal keeps it without a walk, from the
dirty extents ordered by write time.

`Stats` **MUST** be safe to call concurrently with every other operation. It is
not a consistent snapshot of the journal, and **MUST NOT** take a lock a write
needs to become one. It guarantees only these inequalities, each kept by the
order in which the counters are updated:

- `DirtyBytes` ≤ `HeldBytes`: a write raises `HeldBytes` before `DirtyBytes`,
  and a removal lowers `DirtyBytes` before `HeldBytes`;
- `UsedBytes` ≥ what the segments allocate: an allocation raises `UsedBytes`
  before it happens, and an unlink lowers it only after it has happened;
- `ReservedBytes` ≥ 0: a reservation is released only after the record it
  covered is written and counted in its share's `UsedBytes`.

Any other relation between two fields **MAY** be momentarily off by one
in-flight operation.

### 3.8 Loss events

A **loss event** is an extent the journal stopped holding without being asked:
dropped as corrupt ([RFC 28 §3.3](rfc-28-journal-format.md#3.3%20Torn%20and%20corrupt%20records)), as stale ([RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)), or because a failed sync window was
failed ([§5.3](#5.3%20A%20failed%20sync)). Each names the share, the file, the extent, the content version,
the reason, and whether its offloaded bit was set. `Stats` keeps the most recent
ones in a bounded ring, each with a sequence number, and counts them all
([§3.9](#3.9%20Metrics)), so a poller sees every new event or knows how many it missed. The
engine logs each one.

**A loss is a record, and it names the exact record lost.** The journal appends
a **loss record** for each record piece a loss event drops ([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)) before it
publishes the event, naming the file, the extent and the version dropped, its
reason, and — in its payload — the **sequence number of the record it drops**,
and whether that record was **synced** when it was dropped. Recovery does not
hold again any record carrying that sequence number — the original, or a copy
re-appended under it ([§4.3](#4.3%20Versions)) — even where it is still on disk and happens to
verify ([RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)). What else the loss record reaches depends on the
flag:

- **unsynced** — a record of a failed sync window ([§5.3](#5.3%20A%20failed%20sync)), or one found corrupt
  before its sync: the loss record reaches nothing else. Every other record,
  older ones of the same extent included, is decided by precedence as if the
  dropped one had never been written, so the content it superseded is held again.
  A loss keyed on "at or below this version inside the extent" would also take an
  older, flushed record the dropped one had superseded, and turn a lost unsynced
  overwrite into the loss of the write before it;
- **synced** — a record found corrupt or stale after its sync: it also outranks,
  like a release record, every record of the extent below its version. The
  dropped write was stable and had legitimately superseded them; bringing one
  back would serve older content in place of a stable write, which RFC 0's I1
  forbids.

**A superseded record is kept until its superseding write is synced.** Content a
write supersedes stays on disk, and in the index as a retained extent
([§3.11](#3.11%20Snapshot%20holds)), until the record that superseded it is synced; release, repack and
unlinking treat it as live meanwhile. So when a window fails ([§5.3](#5.3%20A%20failed%20sync)) and its
records are dropped, the content they superseded is held again — with its own
version, record and offloaded bit — and a read serves the last synced write, not
a gap. Once the superseding record is synced the retained extent leaves the
index, unless a snapshot hold keeps it ([§3.11](#3.11%20Snapshot%20holds)).

At open, every loss record still on disk is placed in the ring, marked as found
at open, so a restart does not hide a loss nobody had read. A loss record is
dropped by the same per-extent test as a removal record ([§7.2](#7.2%20Repack)): once no segment
still on disk holds a record of its file over its extent at or below the pair of
the record it names, neither that record nor a copy of it can come back. Recovery that drops a record as
corrupt appends the loss record once, so the next open applies it and does not
report the same loss again.

**A loss of content not yet offloaded raises the loss generation.** The journal
keeps a **loss generation**, a counter it **MUST** raise by one on every loss
event whose extent's offloaded bit was unset, and on every failed sync window
([§5.3](#5.3%20A%20failed%20sync)), and exposes as `Stats.LossGeneration` ([§3.7](#3.7%20State%20introspection)). It **MUST NOT** rise for
a loss of offloaded content — corrupt or stale content that the remote tier
holds and the next read fetches — because the engine folds the generation into
its write verifier ([RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more)), and a change makes every client of every
share on the journal resend its unstable writes. It is raised before the event
is published and before any `Sync` the failure fails returns. A failed sync
resolved by re-appending loses nothing and **MUST NOT** raise it.

**Each loss event names its file.** The engine raises a per-file **loss
sequence** from the same events: every event that drops an acknowledged write of
a file not yet offloaded, and every failed window, raises that file's sequence
([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)). So each loss event and each failed-window report **MUST** name
the file of every record it drops.

**The generation never repeats within a process.** It is kept in memory, and a
restart already changes the process instance the verifier carries. Within one
process it **MUST** be monotonic across reopens of the journal: the journal draws
it from a counter the process keeps per journal directory, so a journal closed
and opened again in the same process continues above every value it exposed
before, rather than restarting at zero and handing a client a verifier it has
already seen. A caller that pairs the generation with a write reads it **before**
staging the write: a generation read after `WriteAt` returns can already include
a loss that took that write, and the client would never learn of it.

**The generation is reported per journal.** Each journal has its own, and nothing
merges two journals' generations into one. A replica (cluster) that sees its
journal's generation rise records its **loss point** from the loss events behind
the rise, not from the rise itself — the lowest version among the records they
name, for each shard the journal holds a replica of
([RFC 10 §6](rfc-10-journal-replication.md#6.%20Fencing)).

The journal calls out to nothing to report. Everything an operator needs to tell
a wedged journal from a slow one — at capacity, nothing evictable, syncing
failing — **MUST** be readable from one `Stats` call.

### 3.9 Metrics

The journal exports nothing itself: its dependency set excludes any metrics
library ([§1.1](#1.1%20Non-goals)). The engine exports every `Stats` field of [§3.7](#3.7%20State%20introspection), and the
counters below, labelled with the journal, and the per-share fields labelled with
the share too. A metric is listed only if the journal alone knows it; the caller
times `WriteAt` and counts read hits and misses itself. The counters live in
`Stats` and reset on open.

| Counter | Answers |
| --- | --- |
| `written_bytes` | client bytes accepted by `WriteAt` |
| `filled_bytes` | remote bytes placed by `Fill`; with written, the cold-read share of local writes |
| `fill_refused` | fills refused as older than the file ([§3.4](#3.4%20Fill)) |
| `offered_bytes`, `marked_bytes` | bytes offered at offload and bytes marked offloaded; the lag between them is offload falling behind |
| `released_bytes`, `freed_bytes` | bytes released, and storage actually returned by unlinking segments ([§7.1](#7.1%20Releasing%20storage)) |
| `repacked_bytes` | bytes moved by repack; the write amplification reclamation adds |
| `reservation_refused` | writes refused at capacity ([§6](#6.%20Capacity)) |
| `syncs`, `sync_failures` | syncs issued, and failed ([§5.2](#5.2%20Sync%20policy)); syncs against writes shows whether group commit works |
| `sync_reappended_bytes` | bytes re-appended to a new segment after a failed sync ([§5.3](#5.3%20A%20failed%20sync)) |
| `headroom_draws` | records without bytes appended from the reserved headroom while the journal was at its limit ([§6](#6.%20Capacity)) |
| `corrupt_extents` | extents dropped as corrupt ([RFC 28 §3.3](rfc-28-journal-format.md#3.3%20Torn%20and%20corrupt%20records)), labelled `outcome` = `data_loss` (offloaded bit unset) or `repairable` (set). Any `data_loss` is an alert |
| `stale_extents` | extents dropped as older than offloaded content ([RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)) |
| `torn_tails` | torn tails truncated at open; one per crashed stream is normal |
| `scan_resyncs` | scans that found a record by resynchronising past a bad one ([RFC 28 §3.3](rfc-28-journal-format.md#3.3%20Torn%20and%20corrupt%20records)) |
| `index_refusals` | writes refused at the index entry bound ([§4.2](#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)) |
| `segment_reopens` | reads that had to reopen a closed segment ([§7.4](#7.4%20Open%20descriptors)) |

`RemovalMarkers` growing is a caller that never settles ([§3.6](#3.6%20Truncate%2C%20deallocate%20and%20delete)).

### 3.10 Settle and Since

```go
Since(id FileID, from Version) iter.Seq2[Change, error]
Settle(id FileID, v Version)

type Change struct {
    Kind    ChangeKind // write, truncate, deallocate, delete, clone target or loss
    Extent  Extent     // a held extent, or the extent a removal marker covers
    Version Version
    Clone   *CloneSpec // a clone target's spec (§3.6); nil for every other kind
}
```

`Since(id, from)` yields every held extent of `id`, every unsettled removal
marker and every synced loss record ([§3.8](#3.8%20Loss%20events)) whose version is strictly above
`from`, each with its version, in version order. A loss is yielded so the engine
can commit a lost write's existence from it, and the range reads **Lost** rather
than as the file was before the write ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)); an unsynced loss is not
yielded, since its write was never answered as stable. A synced loss record
above the caller's `applied` is therefore live, like an unsettled marker, until
`Settle` covers it. It is how the engine learns, after a crash, what the journal holds that
metadata has not recorded: the engine passes the file's `applied` version
([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)). A held extent is reported as a write, whether it came from `WriteAt`
or `Fill`: a fill carries the version of a ref metadata already has, so it lies
at or below `applied` and is not yielded. `Since` reads a consistent view of the file
and **MUST NOT** block writes to it for longer than a read does.

`Settle(id, v)` is the caller's statement that metadata has recorded every
removal of `id` at or below `v`. The journal drops those markers. `Settle` is
not persisted and appends nothing: recovery rebuilds the markers from the removal
records ([RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)), and the caller settles again ([§3.6](#3.6%20Truncate%2C%20deallocate%20and%20delete)). Settling too early
is the caller's error, not the journal's: a marker dropped before its removal is
recorded is one a crash may leave metadata never learning of, until recovery
rebuilds it.

### 3.11 Snapshot holds

```go
Hold(share ShareID, cut SnapshotCut, marks map[FileID]Version) error // durable before it returns
Unhold(share ShareID, cut SnapshotCut) error
Held(share ShareID) (bytes int64)
Stamp(id FileID, through Version, cut SnapshotCut) error
```

A snapshot cut is taken without draining the journal ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)); the
dirty content at the cut stays here, held for it ([RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)). `Hold`
records a **hold mark** per file for the cut: the version its existence has
committed up to.

`Stamp(id, through, cut)` records the cut an existence commit read, for every
version of `id` it covered up to `through`. It is stored with those versions, not
with each write record: a version's cut is the cut of the commit that made it
exist, not of its append ([RFC 12 §2.2](rfc-12-snapshots.md#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)). `Offload` offers each version with its
stamp, and the offload commit copies it into the ref as `born`
([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). A `Stamp` that cannot append its record — the headroom exhausted
([§6](#6.%20Capacity)), or a failed write — returns the error and records nothing, and the
caller treats it as the lost stamp below. A stamp is a journal record made durable by the file's next `Sync`,
which every existence commit takes first, and where replication is composed it is
a replicated operation like `Hold`. A file's existence commits are serialised, so
at most the latest stamp — one not yet durable, or not yet replicated when its
primary failed — is lost to a crash or missing after a takeover. Recovery rebuilds
it from metadata: the oldest version of the file's record, live or in history,
whose `applied` covers the version carries the same cut as its `born`. A version of
that record the rebuild needs can have been dropped only if no live cut lies
between the two candidate cuts, and then either gives the same visibility
([RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)).

- **An existence-committed version is superseded only once its successor's
  existence commits.** Until then it **MUST NOT** be released, dropped by repack or
  offered as superseded, whatever newer version has been appended over it: it is
  offloaded as the live version, and the successor's own offload later moves it
  to history. So a hold mark, the version existence has committed up to, always
  names content the journal still has ([RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)).
- **A held version is kept until it is offloaded under the cut.** Content at
  or below a hold mark whose offloaded bit is unset **MUST NOT** be released,
  dropped by repack or dropped when an overwrite, a truncate, a deallocate or a
  release supersedes it: it stays readable to `Offload` ([§3.3](#3.3%20Offload)), which offers it
  in `Superseded`, until its commit returns ([§4.3](#4.3%20Versions) precedence still
  governs every other read).
- **What is kept is a retained extent.** Content either rule keeps after a newer
  version superseded it, and content kept until its superseding write is synced
  ([§3.8](#3.8%20Loss%20events)), is no longer held: `ReadAt` never serves it. The
  placement index keeps it as a **retained extent** — its extent, version, record
  location and offloaded bit — beside the file's held extents ([§4](#4.%20The%20placement%20index)), so `Offload`
  can offer it, repack carries its record forward like held content ([§7.2](#7.2%20Repack)), and
  per-segment held bytes count it. A retained extent leaves the index when its
  rule ends: its offload commit is reported, its successor's existence commits
  (by `Stamp`), `Unhold` releases the cut that kept it, or the record that
  superseded it is synced — or it is held again, when that record's window
  fails ([§5.3](#5.3%20A%20failed%20sync)). Recovery rebuilds the
  retained extents from the hold, unhold, stamp and offloaded records on disk with
  the rest of the index ([RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)).
- **Holds are durable and replicated.** A hold mark is a journal record, synced
  before `Hold` returns; recovery rebuilds holds with the index ([RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)), so a
  restart resumes them. Where replication is composed, `Hold`, `Unhold` and
  `Stamp` are replicated operations, and `Hold` returns only once the marks are durable on
  every replica ([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)).
- **Held bytes count against capacity** ([§6](#6.%20Capacity)) and are reported by `Held`,
  from a maintained counter, for the cut's `snapshots.hold_bound` and
  `snapshots.hold_journal_fraction` refusals.
- **A hold is released** for a version when that version's offload commits, and
  for the whole cut by `Unhold` when the cut is aborted or the snapshot deleted,
  keeping what another live cut's hold still covers. Released held content that
  nothing else holds is reclaimable like any superseded record
  ([§7](#7.%20Reclamation%20mechanisms)).

## 4. The placement index

Two artifacts carry index information, and conflating them is the easiest
mistake to make in this document.

| | **placement index** | **segment catalog** |
| --- | --- | --- |
| Where | memory | inside each sealed segment ([RFC 28 §2.4](rfc-28-journal-format.md#2.4%20The%20segment%20catalog)) |
| Scope | the whole journal | one segment |
| Describes | **held extents** — live content | **records** — everything that segment holds, live or superseded |
| Consulted to serve a read | **yes, always** | never |
| Lifetime | only while the journal is open | immutable, for the life of its segment |
| Authoritative | yes, at runtime | no |

The placement index is built from the catalogs (or a scan, [RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)) at recovery,
by applying precedence ([§4.3](#4.3%20Versions)), and never the other way round. So a catalog entry
need not correspond to a placement index entry — its record may be superseded by
one in another segment — and a placement index entry need not correspond to a
catalog entry: an active segment has no catalog until it seals.

The rest of this section specifies the placement index. In memory, the journal
maintains for each `FileID` an offset-ordered set of disjoint held extents, each
mapped to a record location and carrying its content version and offloaded bit,
together with the file's removal markers ([§3.6](#3.6%20Truncate%2C%20deallocate%20and%20delete)), which carry a version and no
location, and its **retained extents** ([§3.11](#3.11%20Snapshot%20holds)): superseded content still kept for
offload, each with its extent, version, record location and offloaded bit. Retained
extents are a separate set, never consulted to serve a read, and may overlap held
extents and each other.

Extents **MUST** be disjoint: a write superseding part of an existing extent
**MUST** split or narrow it, never leave two extents covering one offset. A
lookup **MUST** be unambiguous without consulting sequence numbers — they order
records during recovery, not during a read.

### 4.1 What it must answer

The index is on the read path, so its cost is paid per request. For a file
holding *n* extents, an implementation **MUST** provide:

| Query | Used by | Bound |
| --- | --- | --- |
| the extent covering an offset, or its absence | every read | `O(log n)` |
| the held extents and gaps across a span, in offset order | `ReadAt`, `Fill` | `O(log n + k)` for *k* results |
| insert, split, narrow, remove at an offset | write, truncate, release | `O(log n)` amortised |
| every extent of one file, in offset order | `Extents`, `Offload` | `O(n)` |
| held and retained bytes in a given segment | reclamation | `O(1)` |
| for one file, each segment holding any of its records, with the lowest (content version, sequence number) among them | dropping removal and release records ([§7.2](#7.2%20Repack)) | `O(s)` for *s* such segments |

A linear walk of a file's extents to answer a point lookup **MUST NOT** be used;
at the extent counts of [§4.2](#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes) it is the read path's dominant cost.

#### Representation

Two levels. The outer level maps `FileID` to that file's index and needs no
ordering, so a hash map with `O(1)` expected lookup is sufficient and
**SHOULD** be used.

The inner level is ordered by file offset. An implementation **SHOULD** use an
in-memory **B-tree keyed on each extent's start offset**: extents are disjoint, so
the covering extent is the predecessor of an offset and no interval tree is
needed; a sorted slice inserts in `O(n)`, and pointer-per-node trees cost more
memory and cache misses than a B-tree's packed nodes.

Whatever the structure, an implementation **MUST** meet the bounds above and
**MUST** allow reads of one file to proceed while another file is being written
([§8](#8.%20Concurrency)). A copy-on-write B-tree, giving readers a stable view without a lock,
**MAY** be used to extend that to reads and writes of the *same* file.

**No reverse index is required**: repack reads a segment's records anyway and asks
the forward index whether each is still held or retained there. What reclamation
needs is the last two rows, **held and retained bytes per segment** and, per
file, the segments holding its records with each one's lowest (version,
sequence) pair among them, which an implementation **MUST** maintain
incrementally — on every append, and on every unlink — and **MUST NOT** derive by
walking the index. A segment's pairs are fixed once it seals, since its records
never change; recovery rebuilds them from the catalogs or the scan.

### 4.2 The index is bounded by extent count, not by bytes

An entry costs about 40 bytes, so the index's footprint tracks how many extents
exist and is independent of how much content they describe.

Measured with a 64-bit version, an entry is 40 bytes and 43 in a B-tree layout;
the 128-bit version of [§4.3](#4.3%20Versions) makes it about 51 (estimated, not re-measured):

| Held as | Extents for 1 TiB | Index |
| --- | --- | --- |
| 1 MiB extents | ~1.0 million | ~51 MB |
| 256 KiB extents | ~4.2 million | ~215 MB |
| 64 KiB extents | ~16.8 million | **~820 MB** |

**An entry is one contiguous piece of one record.** Two records never share an
entry: each carries its own header between payloads, and its own content
version, which reseed and the stale rule read per extent ([RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)). So an
implementation **MUST** represent each contiguous held piece of a record as one
entry, never as several, and the entry count of content is the count of record
pieces holding it. A write is one record per segment it spans ([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)), so content
written in large writes is few entries, and content written in small writes is
one entry per write. To coalesce, the producers write fewer records: a `Fill` of
a fetched run is one record, and a repack **MUST** copy adjacent held extents of
one file that share a content version and an offloaded bit as one record
([§7.2](#7.2%20Repack)), which coalesces repacked content to one entry per copy.

> [!note] ponytail
> The index costs about 51 bytes per record piece held, so a journal holding
> 1 TiB written in 64 KiB writes needs about 820 MB of index until that content
> is offloaded and released. Upgrade to entries that span consecutive records of
> one file with a version range, splitting on demand, when the entry bound below
> is reached in production on a workload of small sequential writes.

`Stats.ExtentCount` ([§3.7](#3.7%20State%20introspection)) **MUST** expose the entry count, removal markers and
retained extents included.

**The index is bounded, and the bound refuses.** An implementation **MUST NOT**
silently degrade when the index grows. A configured entry bound (`ExtentLimit`,
sized against a memory budget at about 51 bytes per entry) caps the entries of
all files of the journal together. A `WriteAt` or `Fill` that would raise the
count above the bound is refused at once, with the same named refusal as a
capacity refusal, naming the index as the limit ([§6](#6.%20Capacity)), and counted in
`index_refusals`. Records without bytes are never refused by it, though they can
raise the count: a release or deallocate inside one held extent splits it in two,
and a removal leaves a marker. Each adds at most two entries, and a refused one
would wedge the very calls that lower the count, so the bound caps what content
writes may add and the count may stand above it by what removals and releases
added since; content writes stay refused until it falls back below. **An index
over its bound at open** — rebuilt from records the bound did not refuse, or
opened under a lowered `ExtentLimit` — is no different: the open **MUST**
succeed, and `WriteAt` and `Fill` are refused until the count drops below the
bound. The engine gets out of the refusal as out
of a capacity one: it offloads and releases dirty content of the files holding
the most entries, and repacks to coalesce what stays.

This index is never stored, so [RFC 0 §9.1](rfc-0-data-lifecycle.md#9.1%20Records%20and%20their%20reclamation) (I7) does not reach it; its failure is
exhaustion, which the bound reports. The one stored artifact besides records, a
segment catalog, is reclaimed with its segment ([RFC 28 §2.4](rfc-28-journal-format.md#2.4%20The%20segment%20catalog)).

### 4.3 Versions

A record carries two numbers, because two questions need them and the answers
diverge.

| | Sequence number | Content version |
| --- | --- | --- |
| answers | the order this journal appended records in | which bytes these are, and which is newer |
| scope | this journal | the file, wherever it is held |
| carried by | every record | every record ([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)) |
| a write, truncate, deallocate or delete | a new one | a new one, assigned by the journal |
| a fill | a new one | the version of the ref it was fetched from ([§3.4](#3.4%20Fill)) |
| a release | a new one | the version released ([§3.5](#3.5%20Release)) |
| a repack copy of content | a new one | the original's, unchanged ([§7.2](#7.2%20Repack)) |
| a header-only record carried forward by repack | the original's, unchanged | the original's, unchanged ([§7.2](#7.2%20Repack)) |
| a record re-appended after a failed sync, or at open ([§5.3](#5.3%20A%20failed%20sync), [RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)) | the original's, unchanged | the original's, unchanged |
| used by | precedence among equal versions; `Fill`'s staleness check | precedence; `Offload` offers, `Since`, reseed, the stale rule ([RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)) |

**A content version is 128 bits: an epoch, then a counter,** compared as one
unsigned number. The journal takes the counter from one monotonically increasing
counter per journal. **In this format version the epoch half MUST be zero**, on
every record and in the floor; a record or a floor with a non-zero epoch fails
the open like an unrecognised kind ([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)). The width is fixed now so that a
later format version can raise the epoch ([RFC 10](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)) without changing the
record layout or the metadata that stores versions. Every version the journal
assigns **MUST** exceed every version on disk and the floor supplied at open
([RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)). Once the epoch half is raised, the floor is compared
on the counter half only: epochs are ordered per file by the extension that
raises them, and a floor compared as one 128-bit number would lift every file in
the journal to the highest epoch any file in it has held.

**Precedence.** Where two records cover the same byte, the one with the higher
content version wins; where their versions are equal, the one with the higher
sequence number wins. That is what lets a repack copy succeed its original, a
release record cover the content it released, and a fill succeed the release
record of the content it replaces, while an operation that arrives late with an
older version still loses. Precedence decides reads only: a version whose
existence has committed stays live for release, repack and offload until its
successor's existence commits ([§3.11](#3.11%20Snapshot%20holds)). Equivalently, record A outranks record
B where they overlap exactly when A's (content version, sequence number) pair is
the greater, compared version first.

Sequence numbers **MUST** come from one monotonically increasing counter per
journal, never per file or per segment.

A number of either kind **MUST NOT** be issued twice; a header-only record that
repack carries forward, and a record re-appended after a failed sync or at open,
keeps its original numbers, which is the same record copied, not a number issued
again. A re-append given new numbers would outrank records appended after the
original — a release, a removal, a later write's loss record — and a crash would
then bring back what those had covered. On recovery the next of each **MUST** exceed
every one found on disk, and the next content version the floor too, taken from
the maximum observed during reconstruction, not from a separately persisted
counter. A share attached after open raises the next content version above its
own floor before it is served ([§3](#3.%20Interface)).

Because precedence is decided by the two numbers and not by position, **recovery
MAY process segments in any order, including concurrently.** An implementation
**MUST NOT** depend on ascending segment order for correctness.

### 4.4 Inspection

The journal **MUST** provide a way to read out its index — per file, and in
bulk — and to verify it against the segments, without mutating the store and
without stopping it.

It **MUST** also expose per-segment accounting: for each segment, its id, seal
state, allocated storage, held bytes, and whether it holds records that do not verify. `Stats`
([§3.7](#3.7%20State%20introspection)) is the `O(1)` total; this is the breakdown behind it.

Verification **MUST** report, rather than repair: the extents the index claims
that the segments do not support, the records the segments hold that the index
does not reference, and the footers ([RFC 28 §2.4](rfc-28-journal-format.md#2.4%20The%20segment%20catalog)) that did not verify. Repair is a
separate, explicit action.

### 4.5 Rebuilding one file from its records, by example

*Explanatory: the rules are [§4.3](#4.3%20Versions) and [RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding).*

These are every record of `alice.vhdx` in the journal, in the order they were
appended, with another file's record in between. Offsets are in KiB.

| seq | segment | kind | file offset | length | version | meaning |
| --- | --- | --- | --- | --- | --- | --- |
| 101 | 12 | write | 0 | 8 | 7 | alice-pc writes `AAAAAAAA` |
| 102 | 12 | write | — | — | — | a record of `bob.vhdx`: ignored here |
| 103 | 12 | write | 4 | 8 | 8 | alice-pc overwrites with `BBBBBBBB` from 4 KiB |
| 104 | 12 | offloaded | 0 | 8 | 7 | the engine reports version 7 offloaded |
| 105 | 13 | release | 0 | 4 | 7 | eviction: stop holding 0–4 KiB of version 7 |
| 110 | 13 | write | 20 | 4 | 9 | alice-pc writes `CCCC` at 20 KiB |
| 120 | 14 | fill | 0 | 4 | 7 | a cold read fetched 0–4 KiB back from the remote tier |

**Step 1 — collect.** The placement index, or on recovery a scan of every
segment's catalog, yields the records for this `FileID`. Their order on disk is
irrelevant: segments may be read in any order, even concurrently, because
everything below is decided by the two numbers in each record, not by position.

**Step 2 — decide each byte by precedence.** For every byte, among the records
that cover it, the one with the **higher content version** wins; between equal
versions, the **higher sequence number** wins.

```text
 KiB      0        4        8        12       16       20       24
          ┌────────┬────────┐
 101 v7   │AAAA    │AAAA    │          write
          └────────┼────────┼────────┐
 103 v8            │BBBB    │BBBB    │ write: beats 101 on 4–8 (v8 > v7)
                   └────────┴────────┘
 105 v7   ░release░                    beats 101 on 0–4 (v7 = v7, seq 105 > 101)
 120 v7   │AAAA    │                   fill: beats 105 (v7 = v7, seq 120 > 105)
                                                         ┌────────┐
 110 v9                                                  │CCCC    │ write
                                                         └────────┘
 result   │AAAA    │BBBB    │BBBB    │   (not held)     │CCCC    │
          v7 fill   v8       v8        12–20 missing     v9
          offloaded dirty    dirty                       dirty
```

- **0–4 KiB.** Three records cover it, all version 7: the write (101), the
  release (105) and the fill (120). The fill has the highest sequence number,
  so it wins: the bytes are held again, read from record 120. They are
  offloaded, because the `offloaded` record (104) covers version 7 here.
- **4–8 KiB.** The write at version 7 (101) and the write at version 8 (103).
  Version 8 wins. It is **dirty**: the `offloaded` record names version 7, and
  never marks newer content offloaded ([RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)).
- **8–12 KiB.** Only record 103: version 8, dirty.
- **12–20 KiB.** No record. The journal reports it **missing** and does not say
  why: never written, evicted, or lost look the same here
  ([§2](#2.%20The%20model%20it%20presents)). The engine asks the metadata store.
- **20–24 KiB.** Only record 110: version 9, dirty.

**Step 3 — the result is the held extents.** Adjacent bytes with the same
record, version and offload bit merge into one extent:

| Held extent | Version | Offloaded | Read from |
| --- | --- | --- | --- |
| (0, 4 KiB) | 7 | yes | record 120, segment 14 |
| (4 KiB, 8 KiB) | 8 | no | record 103, segment 12 |
| (20 KiB, 4 KiB) | 9 | no | record 110, segment 13 |

That set is exactly what the placement index holds for the file. A read of
0–24 KiB returns the three held extents and reports 12–20 KiB as missing; the
engine fills that gap from the remote tier or reports it lost.

What happens to the records later ([§7.2](#7.2%20Repack)): record 101 backs no held byte, so
when repack rewrites segment 12 it carries 103 and 104 forward and leaves 101
behind. Record 105 then outranks nothing left on disk and is dropped too.
Record 104 stays as long as the version-7 bytes it marks offloaded are held.

## 5. Durability and ordering

### 5.1 Ordering rules

| Rule | Why |
| --- | --- |
| A record **MUST** be written before `WriteAt` returns, and synced by a `Sync` or within the sync bound ([§5.2](#5.2%20Sync%20policy)). `Sync` returns only once synced. | The acknowledgement is a promise, of exactly the durability the policy states. |
| `Release` **MUST NOT** precede the committed offload transaction the offloaded bit reflects. | Otherwise a crash can leave content whose only copy is gone ([RFC 0 §8.1](rfc-0-data-lifecycle.md#8.1%20Evict)). |
| A release record **MUST** be durable before the segment holding the record it released is unlinked. | Otherwise a crash can lose the record and keep the unlink, and an older record the release covered is held again ([§3.5](#3.5%20Release)). |
| A segment's records **MUST** be durable before the segment is unlinked by reclamation. | A reclaim pass **MUST** be content-preserving ([RFC 0 §8.2](rfc-0-data-lifecycle.md#8.2%20Reclaim)). |
| Marking an offloaded bit **MUST NOT** precede the report that justifies it. | [RFC 0](rfc-0-data-lifecycle.md) invariant I5. |
| A directory sync **MUST** follow the creation of a segment or the `format` file before any `Sync` covering its records returns. | Otherwise power loss can drop a synced segment's directory entry, and the segment with it ([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)). |
| A segment's unlink **MUST** be durable before a removal record whose retention counted that segment is dropped. | Otherwise a crash can bring the segment back without the removal that covered its records ([§7.2](#7.2%20Repack)). |
| A sync that failed **MUST NOT** satisfy, by a later success, a `Sync` for writes made before the failure. | Those writes may be lost already ([§5.3](#5.3%20A%20failed%20sync)). |
| A segment whose sync failed **MUST NOT** be appended to again. | What the device holds past its last successful sync is unknown; a record appended after it could follow bytes a scan cannot cross ([§5.3](#5.3%20A%20failed%20sync)). |

### 5.2 Sync policy

The journal **MUST** be on durable local storage: a synced record survives a
process crash, a host crash and power loss. There is no setting that declares a
journal not durable. The journal's part in a client's flush is `Sync` ([§3.1](#3.1%20Write));
what else a flush waits for is its caller's ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)).

There is one policy. `WriteAt` writes its record before it returns, and the
record is synced by the first of two events: a `Sync` naming its file, or a fixed
time bound after the write. A record written but not yet synced survives process
death, since the operating system holds it, but not host loss. A client that asks
for a stable write gets one because the engine calls `Sync` before replying
([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)); that is cheap, because one sync covers every writer waiting on the
stream (group commit).

> [!note] decision
> The bound is a constant of the implementation (proposal: 1 s), not measured.
> Overturned by a measurement showing that the timer's syncs cost throughput a
> longer bound would recover, or that host loss within the bound is unacceptable
> to a deployment.

Whatever the bound :

- the journal **MUST** be able to state it;
- it is a bound in time, not only in bytes;
- a failure to sync **MUST** be reported to the caller, and **MUST NOT** be
  recorded as success and retried silently.

**The sync must reach the device.** On a platform where the plain file-sync call
does not flush the device's write cache — the Apple filesystem is one — the
journal **MUST** use the call that does (a full sync), or a synced record does
not survive power loss.

### 5.3 A failed sync

A failed sync is not a slow one. After it, the operating system may already have
dropped the dirty pages it could not write and marked them clean, so a second
sync can succeed without having written them, and a read of those pages can
return what the device held before rather than what was written. So after a
failed sync of a segment, **everything written to that segment since its last
successful sync is not durable**, the segment takes no further append — not
even a seal marker: **a failed segment is never sealed** — and the stream
continues in a new segment whose header names no predecessor ([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)). The
failed segment stays accounted at its cap to the journal until it is unlinked
([§7.3](#7.3%20Accounting)), and is a repack candidate at once.
For the window, the journal **MUST** do one of two things:

- **re-append it.** Append those records again to the new segment and sync it;
  the writes' `Sync` calls wait for the new sync. A re-appended record keeps the
  original's sequence number and content version ([§4.3](#4.3%20Versions)): it is the same record
  in a new place. The bytes come from a copy the journal kept in its own memory
  since the write, or from a read of the failed segment that verifies against the
  payload checksum the journal computed when it wrote the record and kept in
  memory, never against the checksum read back with it. The page cache of a
  failed segment is not evidence: a read that does not verify against the kept
  checksum fails that record's part of the window instead. **A header-only
  record is always re-appended, never failed**: the journal keeps every one of a
  window in memory until it is synced, since it is a header and at most a 32-byte
  payload, so a removal, release, offloaded, unmark, loss, hold, unhold, stamp or
  forget record never becomes a loss. Failing one would have no defined meaning —
  a failed truncate or release would bring back what it removed — and nothing
  forces it, because its bytes never depend on the failed segment's pages;
- **fail it.** Drop exactly the window's records from the index, each as a loss
  event with an unsynced loss record in the new segment naming it by sequence
  number ([§3.8](#3.8%20Loss%20events)). Content those records superseded was kept, because a
  superseded record is kept until its superseding write is synced ([§3.8](#3.8%20Loss%20events)), and
  is held again: a read serves the last synced write. Failing a window raises the
  loss generation before any `Sync` it fails returns, and the engine folds it into
  its write verifier ([RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more)), so a client resends the unstable writes
  the window held. Re-appending loses nothing and does not raise it.

**A failed window is reported once to each file that had a write in it.** The
journal records, per file with a write in the failed window, an unreported
failure. The next `Sync` naming that file — whether it was waiting when the sync
failed or arrives later — returns the error and clears the file's mark; the
`Sync` after it reports only what happened since. So a window the sync timer
failed, with no `Sync` waiting, still fails the next flush, commit or stable
write of each of its files, and its writer learns its data is gone, as a file's
next `fsync` reports a failed writeback once on the platforms that track it per
file. A file with no write in the window is not told. The marks live in memory
and cost one entry per file with a write in an unreported window; a restart
loses them, and the restart itself changes the write verifier.

**A failed window ends; it does not wedge the file.** Once the window's records
are dropped, nothing of the file is pending on the failed segment, so after the
one failed `Sync`, a later `Sync` of the file waits only for writes made after the
failure, and succeeds when they are synced in the new segment. A later successful
sync of the failed segment **MUST NOT** satisfy a `Sync` for a write made before
the failure. A window left pending instead — neither re-appended nor dropped —
fails every later flush, commit and offload of the file until a restart.

Until the loss records are durable, a crash can find the window's records whole
on the old segment and hold them again; that serves the client's own
acknowledged bytes and loses nothing. Recovery does not then trust them as
synced: they lie past the last proven sync point, and are verified in place at
open and re-appended only where that fails ([RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)).

## 6. Capacity

The journal has a configured maximum local footprint, shared by its shares. Each
share's handle also carries a limit ([§3](#3.%20Interface)); limits **MAY** sum to more than the
maximum, and a share's limit is what stops one share taking the whole device.

**The two bounds count different things, at different granularities.** Segments
are allocated whole ([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)), so a write into one changes no allocation:

- **the journal's maximum bounds allocated storage, in whole segments.** Creating
  a segment, a spare included, reserves its cap against the maximum, and its
  unlink returns it; the journal's `UsedBytes` moves only then. A segment's
  unwritten tail, seal marker and footer are the journal's, not a share's, and the
  free tail of a sealed segment is returned at seal ([§7.3](#7.3%20Accounting));
- **a share's limit bounds the record bytes of its records on disk** — header,
  payload and padding — counted when a record is written and released when the
  segment holding it is unlinked. Every record byte is accounted to the share
  whose tag it carries — with one named exception: a header-only record, drawn
  from the headroom below, is counted in its share's `UsedBytes` like any other
  record, so the share's usage reports it, but never reserves against or is
  refused by the share's limit. Its storage is the headroom's, which the journal
  sets aside outside every limit.

`WriteAt` and `Fill` **MUST** reserve their record bytes, before accepting them,
against the share's limit and against the free space left in their stream's
allocated segments, taking the next spare when the current segment cannot hold
the record and allocating a new spare against the maximum. `ReservedBytes` is
what these reservations hold and the records have not yet filled. The reservation
**MUST** be atomic with respect to other reservations.
An implementation that reads a counter and then decides **MUST NOT** be
considered conformant: any number of concurrent callers can pass one such test,
and the maximum then bounds nothing.

**A refusal is immediate, and named.** When a reservation cannot be satisfied,
the call **MUST** fail at once with `ErrNoSpace`, carrying which limit refused
it — the share's, the journal's or the index's ([§4.2](#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)) — and how many bytes or
entries were missing. It **MUST NOT** accept the bytes and exceed the maximum,
and it **MUST NOT** wait: space is freed only by the engine's own calls
(`Release`, `Offload`, `Repack`), so a journal that waited for space would be
waiting on its caller. A refusal performs no I/O. The journal's refusal is the
engine's input, not the client's answer: a refusal by the journal's maximum or a
share's limit is **transient** while offload, release and repack can drain it, so
the engine answers retry-later (`ErrDelay`) within the caller's deadline and no
space (`ErrNoSpace`) only after it, by the one table of
[RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here). A full filesystem underneath (`ENOSPC`) is reported as
itself.

**Records without bytes are not refused by the journal's own limit.** Release,
truncate, deallocate, delete, clone target, offloaded, unmark, loss, hold, unhold,
stamp and forget records, seal markers and footers are what frees space or keeps
it accounted; refusing them at the limit would wedge a full journal. They draw on
a **reserved headroom** the journal sets aside at open, outside every share's
limit: at least one seal marker and footer per open stream plus
`journal.headroom_records` header-only records (proposal: 65,536; at 96 bytes
each ([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)), about 6 MiB). `ErrNoSpace` is
never returned for them. Their one refusal is the exhausted headroom below, and
only removals of content the journal does not hold can bring it about.

**The headroom is real storage, allocated in advance.** The journal holds it as
spare segments, created and allocated at their cap ahead of need ([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)), so a
header-only record never needs the filesystem to find space; a filesystem that
fills (`ENOSPC`) cannot take it away — on a filesystem that writes allocated
space in place. A copy-on-write filesystem can allocate anew on the first write
into allocated space, so there the headroom holds only with copy-on-write
disabled for the journal's files, and outside every filesystem snapshot, as
[RFC 28 Appendix A](rfc-28-journal-format.md#Appendix%20A%20%E2%80%94%20platform%20profile) requires. When a stream's segment fills, a spare
becomes its next segment and the journal allocates a new spare; if that
allocation fails, content writes are refused until it succeeds, and header-only
records go on drawing on what is allocated.

**The headroom is finite, and content writes stop first.** Content writes are
refused once the headroom falls below half its size, so what remains serves
the records that return space. Each release, removal or loss record either
stops holding something or records a removal the caller needs, and repack turns
what they stopped holding into free space from its own reserve, below, so the
headroom refills as segments are unlinked. A header-only record that finds the
headroom exhausted fails with the transient refusal rather than overrunning.

> [!note] decision
> Header-only records can exhaust the headroom only when they free nothing:
> removals of content the journal does not hold, issued faster than repack
> returns space. Then those removals, and only they, are refused until it does.
> Overturned by a workload that reaches `headroom_draws` near the headroom's size
> while repack has candidates it is not running.
> The half kept for space-returning records and the default of
> `journal.headroom_records` are unmeasured: overturned by a journal held at its
> limit ([§10](#10.%20Open%20questions), question 4) whose removals reach the refusal, or whose
> headroom sits mostly undrawn.

**Repack has a reserve of its own.** Repack copies live records before it can
unlink their segment ([§7.2](#7.2%20Repack)). Its copies draw on a **repack reserve** at journal
level, outside every share's limit and separate from the headroom, allocated in
advance as spare segments like the headroom and sized to at least one segment's
cap, the most live payload one segment can hold. A share at its limit therefore
blocks repack neither of its own records nor of any other share's: a copy is
never refused by a share's limit. The copy is accounted to its record's share
once the source segment is unlinked; until then the share's usage may stand above
its limit by that copy. A filesystem that is itself full (`ENOSPC`) is a different
failure from every refusal above and is reported as such, never as `ErrNoSpace`.

The journal **MUST NOT** evict to satisfy its own reservation. Eviction requires
knowing what is offloaded, which the journal is not authoritative for.

**Getting out of a refusal is the engine's loop** ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)). The engine paces
writes before the limit, so a refusal is rare ([RFC 8 §10.2.1](rfc-8-engine.md#10.2.1%20Writes%20are%20paced%20before%20the%20limit%2C%20not%20stopped%20at%20it)); on one, it releases
content already offloaded and offloads dirty content so it can be released next —
which frees nothing yet — then repacks, which returns the space, and retries within
the caller's deadline ([RFC 0 §10.3](rfc-0-data-lifecycle.md#10.3%20Every%20wait%20on%20a%20request%20ends%20at%20a%20deadline)). Remote GC frees nothing here. For the
loop to be fast, the journal owes it three things:

- a refusal that costs no I/O, above;
- `Stats` that report each share's `HeldBytes`, `DirtyBytes`, `UnreclaimedBytes`
  and `UsedBytes` from counters ([§3.7](#3.7%20State%20introspection)), so the engine sees pressure, and what
  repack could recover, before it is refused;
- a `Repack` that reports the storage it returned, only once it is returned
  ([§7.2](#7.2%20Repack)), so the engine knows when a retry can succeed without polling.

## 7. Reclamation mechanisms

The journal provides mechanisms. The policy that drives them is the engine's.

### 7.1 Releasing storage

`Release` stops holding extents; it frees no storage itself. **Storage is freed
in whole segments only**: a segment's storage returns to the filesystem when the
segment is unlinked, which happens once it is no longer **held** ([§7.2](#7.2%20Repack)) — because
releases and supersession left it nothing live, or because repack carried its
live records forward. Between the two, released and superseded bytes are
**unreclaimed** ([§3.7](#3.7%20State%20introspection)): the capacity a full journal is short of is usually
here, and only repack returns it.

An implementation **MUST NOT** free, zero or deallocate any byte range inside a
segment while the segment remains, with two exceptions, both at the end of the
file: at recovery, a torn tail — the bytes from the first record of the newest
segment of a stream that is torn, not corrupt — is cut off by truncating the
segment there ([RFC 28 §3.3](rfc-28-journal-format.md#3.3%20Torn%20and%20corrupt%20records)); and at seal, the unwritten space past the trailer is
returned ([§7.3](#7.3%20Accounting)). No verified record lies in either, so nothing held or
retained is touched. One checksum covers a record's whole payload
([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)), so freeing part of a record would leave its remaining bytes
unverifiable: recovery would drop the record ([RFC 28 §3.3](rfc-28-journal-format.md#3.3%20Torn%20and%20corrupt%20records)), and a remainder still dirty
would resolve as **Lost**. A record partly released keeps all its bytes on disk
until repack copies its held part forward, with its content version unchanged
([§4.3](#4.3%20Versions)), and the segment holding the original is unlinked.

`Release` **MUST NOT** silently retain the content in a readable state: a released
extent is absent from reads at once, whatever is still on disk.

> [!note] ponytail
> Released bytes stay allocated until repack retires their segment, so every
> reclaimed byte costs a copy of the live records sharing its segment — about
> u ÷ (1 − u) bytes copied per byte freed for a segment whose live fraction is
> u, so about 5.7 at 85% live — and the journal sits fuller for longer than one
> that frees storage at release. The engine's capacity triggers therefore read
> unreclaimed bytes, not dirty bytes alone ([§3.7](#3.7%20State%20introspection)), and repack prefers the
> segments with the least live payload ([§7.2](#7.2%20Repack)). Measured by J8 ([§9.6](#9.6%20Benchmarks)). If
> repack's write amplification under an eviction-heavy load is too high, the
> upgrade is to free storage at release for **whole records only** — never part
> of one — leaving partly released records to repack as now.

No surviving record moves at release, so a release rewrites no index entry and
copies no bytes.

A segment that is not **held** ([§7.2](#7.2%20Repack)) **MUST** be unlinked: by the journal, on its
own, once the records that left it not held are durable, without waiting for a
repack to select it.

### 7.2 Repack

```go
Repack(budget int64, scope RepackScope) (freed int64, err error)

type RepackScope struct {
    Share    ShareTag    // zero: any share
    Segments []SegmentID // empty: any segment
}
```

Repack is the principal mechanism of *reclaim* ([RFC 0 §8.2](rfc-0-data-lifecycle.md#8.2%20Reclaim)). It copies the
still-live records of a segment into another segment, repoints the
placement index, and unlinks the original. It is local and discards nothing; it
is unrelated to the remote relocation of [RFC 9](rfc-9-gc.md). The engine calls `Repack` with a
budget of bytes to copy and a **scope**; it repacks the best candidates within
both and returns `freed`, the storage returned to the filesystem, once the unlinks
are durable. The scope aims the pass: at the segments holding a share's records,
when that share is the one at its limit, or at a set of segments, when the engine
has just evicted the extents they hold ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)); a zero scope lets the
journal choose from every segment. Segment IDs come from the per-segment
accounting of [§4.4](#4.4%20Inspection). A pass that could only pick journal-wide would spend
its budget on the sparsest segments anywhere, and return nothing to the share or
the segments the refusal was about.
`UsedBytes` falls at the same moment, never earlier.

It is how released storage returns to the filesystem ([§7.1](#7.1%20Releasing%20storage)), retires fragmented segments that still
cost a descriptor ([§7.4](#7.4%20Open%20descriptors)), and coalesces index entries ([§4.2](#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)).

**Selection.** Candidacy is decided from held and retained bytes per segment
against the storage that segment still allocates — both of which an
implementation already maintains ([§4.1](#4.1%20What%20it%20must%20answer), [§7.3](#7.3%20Accounting)). A repack pass **MUST NOT**
need to read a segment to decide whether to repack it, and **SHOULD** take the
segments with the least live payload first, since they free the most per byte
copied. Only segments inside the scope are candidates. A segment pinned by a
running offer ([§3.3](#3.3%20Offload)) is not a candidate, and a segment a pass has selected
takes no new pin until the pass ends with it.

**Content preservation.** Repack **MUST** preserve the set of held extents and of
retained extents ([§3.11](#3.11%20Snapshot%20holds)), the bytes they produce, and their offloaded bits. Resetting an offloaded bit would make
offloaded content look dirty and cause it to be re-uploaded; setting one would make
content not yet offloaded evictable.

**A copy of content gets a new sequence number and keeps its content version**
([§4.3](#4.3%20Versions)). The new sequence number makes the copy the successor if a crash leaves
both on disk; the kept version keeps it recognisable to its ref at reseed.

**A carried header-only record keeps both its numbers.** A release, removal,
loss, offloaded or unmark record carried forward is the same record in a new place.
Given a new sequence number, a release record carried forward would outrank a
fill appended after it at the same version — the fill of the very content it
released — and a crash would then drop the filled bytes ([§4.3](#4.3%20Versions)). So the copy
keeps the original's sequence number and content version, and where both are on
disk after a crash they are equal and either decides the same way.

**The source is verified whole before any part is copied.** Repack **MUST** read
the source record's whole payload and verify its payload checksum before copying
any part of it ([RFC 28 §2.3](rfc-28-journal-format.md#2.3%20Records)). A record that does not verify is dropped as corrupt
([RFC 28 §3.3](rfc-28-journal-format.md#3.3%20Torn%20and%20corrupt%20records)) and nothing of it is copied.

**A partly released record is copied in part.** Repack copies only the held and
retained extents of a record, each as a new record with its own payload checksum,
and never the released part. The original stays whole on disk until its segment
is unlinked ([§7.1](#7.1%20Releasing%20storage)). Adjacent held extents of one file that share a content
version and an offloaded bit are copied as one record, so repacked content
coalesces ([§4.2](#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)).

**Records without content are carried forward.** A release record, or a synced
loss record, is live while it may still outrank a record of its file in another
segment ([§4.3](#4.3%20Versions)), and a synced loss record also while `Settle` has not covered it
([§3.10](#3.10%20Settle%20and%20Since)); an unsynced loss record reaches only the record it names
([§3.8](#3.8%20Loss%20events)), and is live while any segment holds a copy of that record. A
truncate, deallocate, delete or clone target record is live while it may still
outrank a record in another segment **or** its removal marker is unsettled
([§3.6](#3.6%20Truncate%2C%20deallocate%20and%20delete)): recovery rebuilds the marker from it. A forget record is live while
any other segment holds a record of its tag below its sequence number ([§3](#3.%20Interface)).
An offloaded or unmark record is live while it
decides the bit of a held or retained extent ([RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)). Hold, unhold and stamp
records are live while the hold or stamp they record is ([§3.11](#3.11%20Snapshot%20holds)). A segment is
**held** while it backs a held or retained extent or holds a live one of these
records; repack copies such records forward like held content, and a segment that
is not held is unlinked ([§7.1](#7.1%20Releasing%20storage)).

**A removal record is dropped per extent, by version, not by sequence.** A
removal, release or synced loss record outranks a record exactly where their
extents overlap and its (content version, sequence number) pair is the greater
([§4.3](#4.3%20Versions)), so a record appended after it can still be one it outranks: a repack
copy of retained content, or a fill, keeps an old version under a new sequence
number. It can only ever outrank records **of its own file, inside its own
extent**. So a removal, release or synced loss record whose marker, if it has
one, is settled — a synced loss record being settled itself — and below whose pair **no other segment holds a record of the
same file overlapping its extent** ([§4.1](#4.1%20What%20it%20must%20answer)), can outrank nothing on disk, and
repack drops it — once the unlinks it counted on are durable ([§5.1](#5.1%20Ordering%20rules)).
Comparing sequence numbers alone would drop a truncate that still covers a
repacked copy of older retained content, and a crash would then bring that
content back as held. Comparing against every record of the file, whatever its
extent, would pin each release and deallocate of a file behind any older record
of that file anywhere: a disk image whose boot region is refilled at every login
would keep every later removal of the image alive, hundreds of thousands per
eviction cycle. Comparing against the lowest pair journal-wide would never drop
one at all: every fill and every repacked copy of cached content carries an old
version by design, so the headroom drains until the journal wedges.

To answer the test, the journal keeps in memory, per file and segment, the
extents its records on disk cover, each with the lowest pair over it, merged
where adjacent extents share it; it is rebuilt at open with the index and costs
at most one entry per record on disk, usually far fewer, since a file's records
cluster in the segments its stream wrote.

**A pass decides its drops after its own copies.** The test runs once the pass's
copies are appended, and counts them, its target segment included: a record the
pass carries forward is on disk below the removal's pair like any other. A test
run before the copies would drop a settled truncate whose segment it emptied,
then carry forward the older, snapshot-retained version the truncate covered,
and a crash would bring those bytes back as held where zeros belong.

**A copy races the file's writes.** A repack copy is appended under the file's
serialised append ([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)), and only if its source record is still held **or
retained** at that moment. A write, removal or release that superseded the source
after repack read it does not make the source dead: it turns held content into
content retained until that write syncs, or by a hold ([§3.11](#3.11%20Snapshot%20holds)), and the copy is
still made, as retained. Only a source that is neither — released, or covered
by a synced superseding record nothing retains — is skipped, so the segment is
never unlinked while it backs a held or retained extent ([§7.1](#7.1%20Releasing%20storage)).

**Ordering.** Copy the records; make the copies durable; repoint the placement
index; only then unlink the source. A crash before the unlink leaves both copies
on disk, and recovery selects the copy by precedence ([§4.3](#4.3%20Versions)) — the source's records
become dead weight that a later pass reclaims. A crash after the unlink is
indistinguishable from a completed repack. At no point is an extent unreachable.

**Capacity.** Repack consumes space before it releases any. Its copies **MUST**
reserve against the repack reserve of [§6](#6.%20Capacity), never against a share's limit, and
a pass copies at most what the reserve holds before the segments it emptied are
unlinked and the reserve is whole again. A journal at capacity, and a share at its
limit, **MUST** still be able to run one.

**Refusals.** A repack **MUST NOT** run on a segment any record of which cannot
be read ([RFC 28 §3.3](rfc-28-journal-format.md#3.3%20Torn%20and%20corrupt%20records)), because it cannot carry forward what it cannot read. Two passes
**MUST NOT** select the same segment ([§8](#8.%20Concurrency)), and a segment **MUST NOT** be its own
target.

### 7.3 Accounting

Accounted local footprint **MUST** reflect storage the journal's files actually
allocate, not the sum of held extents: a release frees nothing until its
segment is unlinked ([§7.1](#7.1%20Releasing%20storage)), and the limit must see that.

A segment is allocated at its cap when created ([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)) and **MUST** be accounted
at its cap from creation until its unlink; a segment shortened by a torn-tail
truncation ([RFC 28 §3.3](rfc-28-journal-format.md#3.3%20Torn%20and%20corrupt%20records)), or at seal, is accounted at its size after it. **A seal
returns the free tail**: after the seal marker, footer and trailer are durable,
the journal truncates the segment to end just after its trailer and makes that
durable, so a segment sealed early, idle ([RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)), strands no unwritten space. A
segment left after a failed sync is not sealed ([§5.3](#5.3%20A%20failed%20sync)) and keeps its cap until
unlinked. No record lies past the trailer, so nothing
held or retained is touched; a crash between the two steps leaves a segment
whose last bytes are not its trailer, which recovery scans instead
([RFC 28 §2.4](rfc-28-journal-format.md#2.4%20The%20segment%20catalog)). The accounting therefore
does not follow what a filesystem happens to allocate around a growing file, and
the spare segments of the headroom and the repack reserve ([§6](#6.%20Capacity)) are accounted
the same way, to the journal, not to a share. Every file the journal creates
**MUST** be accounted, including each segment's catalog ([RFC 28 §2.4](rfc-28-journal-format.md#2.4%20The%20segment%20catalog)). Storage that is not accounted is storage no limit bounds and
no reclamation targets. `Stats` reports it ([§3.7](#3.7%20State%20introspection)). Every such file **MUST** also be
bounded by live state, not by history: a side log that grows with every change
and is compacted only at open grows without bound for as long as the process
runs, and is compacted by a restart nobody scheduled.

**Retiring a segment is idempotent.** A segment's bytes **MUST** leave the
accounted footprint exactly once, in the step that removes the segment from the
set of segments the journal owns, and only if that step found it there. Two
reclamation paths that can each select a segment — the unlink of a segment a
release left not held, and repack, say —
will sooner or later both retire the same one: a retire that finds the file
already gone, or the segment already out of the set, **MUST** subtract nothing.
A claim taken on a segment **MUST NOT** be released after the segment is retired,
since a released claim on a retired segment is an invitation to retire it again.
A retire that subtracts nothing **MUST** be counted and logged at `Warn`: it is
harmless, but it means two paths selected one segment, which is the race this rule
exists for. The accounted footprint **SHOULD** be checked against the sum of the
segment set's sizes by verification ([§4.4](#4.4%20Inspection)), and a mismatch reported.

### 7.4 Open descriptors

The number of segment descriptors held open **MUST** be bounded by
configuration, independently of the number of segments. The bound is per
process: the journals of one process, one per device, draw on one budget,
because the limit it protects, the open-file limit, is per process. A read of a
segment whose descriptor is not open **MUST** reopen it. A cached descriptor of a
segment **MUST** be closed before that segment is unlinked ([RFC 28 §2.2](rfc-28-journal-format.md#2.2%20Segments)): an unlinked
file held open keeps its storage allocated, and on some platforms cannot be
unlinked at all.

The count, its bound and the reopens **MUST** be reported through `Stats` ([§3.7](#3.7%20State%20introspection),
[§3.9](#3.9%20Metrics)). A bound set too low is invisible in the count, which simply sits at the
limit, and shows up only as a reopen on nearly every read.

## 8. Concurrency

### 8.1 What must not block what

A read of one file never waits on a write to another file, on a sync, or on
reclamation of a segment it does not read. A write waits only for its own file's
append and for capacity. A truncate, deallocate or delete never waits on an
offload pass — its callback or the upload inside it — of its own file or of
another; the pass keeps the bytes it was offered ([§3.3](#3.3%20Offload)), and the storage they
occupy is reclaimed once the pass ends. The rest of this section is how.

### 8.2 Lock domains

An implementation **MUST** be able to name, for every piece of mutable state,
which domain guards it. The domains are:

| Domain | Guards | Scope |
| --- | --- | --- |
| **file append** | one file's serialised append: assigning a version and appending a write, fill, removal, release or repack copy for the file, and `Fill`'s absence check with its write | one `FileID` |
| **capacity** | the reservation counters ([§6](#6.%20Capacity)) | store-wide |
| **file index** | one file's placement index entries | one `FileID` |
| **segment append** | the write position of one active segment | one segment |
| **storage guard** | a segment's bytes against being freed or moved | one segment |

The file append domain is what [§3.1](#3.1%20Write), [§3.4](#3.4%20Fill) and [§7.2](#7.2%20Repack) mean by the file's
serialised append. It is the one domain held across I/O — the record write — and
it guards one file only, so writes to different files still run concurrently.

The file index domain is deliberately per-file, not store-wide: a store-wide
index lock makes every read of every file contend with every write to any file.

### 8.3 Lock ordering, and what may never be held

Acquisition **MUST** follow one total order: **file append → capacity → file
index → segment append → storage guard.** An implementation **MUST NOT**
acquire in any other order, and **MUST NOT** hold two locks of one domain at
once, so no two files' append locks are ever held together: `OffloadMany` and a
repack pass take each file's lock in turn.

Two rules matter more than the order itself, because both have produced real
deadlocks:

![The two-lock cycle between a reader and a reclaimer, and the ordering that breaks it](img/rfc1-lock-cycle.svg)

**The storage guard MUST NOT be acquired while holding the file index.** A
reader naturally holds the file index to resolve an extent, then wants the
storage guard to read it; reclamation naturally holds the storage guard, then
wants the file index to repoint entries. Those two orders deadlock. The reader
**MUST** release the file index — taking a copy of the resolved location — before
acquiring the storage guard. Reclamation **MUST** repoint under the file index
alone and take the storage guard only to unlink, holding no other lock: the
guard is the last lock in the order, so nothing may be acquired after it.

**No lock may be held across the `Offload` callback** ([§3.3](#3.3%20Offload)), which hands control
to code that may take seconds.

**No I/O may be performed while holding the file index.** The index is memory;
resolving an extent is a lookup. An implementation that reads a segment while
holding it converts disk latency into index contention.

### 8.4 What must be atomic

| Operation | Must be atomic with respect to | Why |
| --- | --- | --- |
| append a record, then publish its extents | readers of that file | a reader **MUST NOT** see an extent whose record is not yet written |
| `Fill`'s absence check, then its write | `WriteAt` on the same extent | otherwise a fill overwrites a newer client write ([§3.4](#3.4%20Fill)) |
| `Release`'s offloaded-bit check, then dropping the extent | `WriteAt` and `Offload` on that extent | a write between check and drop would have its bytes released |
| `report`'s and `MarkOffloaded`'s version check, then setting the bit and appending the offloaded record | `WriteAt` and `Fill` on that extent | a write between check and set would be marked offloaded while only older bytes are ([§3.3](#3.3%20Offload), [RFC 28 §3.2](rfc-28-journal-format.md#3.2%20Offload%20state%20after%20recovery)) |
| repack's index repoint | readers of the affected files | a reader **MUST** see either the old location or the new one, never neither |
| a repack copy's held check, then its append | `WriteAt`, removals and `Release` on that file | a copy appended over newer state would reinstate superseded content ([§7.2](#7.2%20Repack)) |

Publishing an extent **MUST** be the last step of a write, after its record is
written, so a read never finds an index entry pointing at bytes not yet in the
segment. It **MUST NOT** wait for the record to be durable: that would put the
sync on every read of freshly written bytes, which is what the sync bound
([§5.2](#5.2%20Sync%20policy)) exists to avoid. A published extent may therefore not survive a host
crash, so everything that turns journal content into a durable claim elsewhere
syncs first: an offer **MUST** sync the records it captures before handing them to
`fn` ([§3.3](#3.3%20Offload)), and the engine **MUST** `Sync` a file before committing its
existence ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)).

### 8.5 Protecting readers from reclamation

Between resolving an extent and reading it, repack may move the record and the
segment holding it may be unlinked, whether by repack or because a release left
it not held ([§7.1](#7.1%20Releasing%20storage)).

An implementation **MUST** guarantee that a read either observes the location it
resolved, intact, or discovers that it must retry or report the extent missing.
It **MUST NOT** be possible for a read to return bytes from storage that has been
freed or reused.

The mechanism is a **shared/exclusive guard per segment**: readers hold it
shared; an unlink takes it exclusively, which waits for in-flight reads of that
segment only, and unlinks only then. An implementation **MAY**
use another mechanism with the same guarantee, provided freeing storage is still
the last step, after no reader can hold the old resolution.

### 8.6 Progress

No operation may starve. In particular:

- a continuous stream of readers **MUST NOT** prevent reclamation from ever
  acquiring a segment exclusively, as a reader-preferring guard would;
- reclamation **MUST NOT** hold a segment exclusively for longer than one unit of
  work, so a large repack **MUST** be divisible rather than holding a segment for
  its whole duration;
- a write blocked on capacity ([§6](#6.%20Capacity)) **MUST** fail rather than wait indefinitely,
  and **MUST NOT** hold the file index while blocked.

### 8.7 Shutdown

On close, the journal **MUST** stop accepting new operations, allow in-flight
ones to complete or fail cleanly, and **MUST NOT** free storage a reader may
still be holding. An operation **MUST NOT** observe a partially closed store: a
read that begins after close **MUST** fail, rather than reading through a
half-dismantled index.

Background work — reclamation passes, sealing idle segments — **MUST** be
stopped and joined before the segments it touches are closed.

## 9. Conformance

The rules for checks, tiers and recorded results are [the index's](rfc-index.md#Test%20tiers); this
section is the journal's plan.

### 9.1 Kinds of test

| Kind | What it covers | How |
| --- | --- | --- |
| Unit | pure logic with no storage: the placement index, coalescing, overlap splitting, the record and catalog codecs | table-driven, in memory; the only place an in-memory stand-in is allowed |
| Conformance | every check in [§9.4](#9.4%20The%20checks) | real filesystem in a temporary directory, virtual time, on every supported platform ([§9.3](#9.3%20Environments%20a%20check%20set%20must%20cover)) |
| Fault injection | lost, torn and reordered writes, flipped bits, outside edits ([RFC 28 §4.4](rfc-28-journal-format.md#4.4%20Corruption%2C%20crashes%20and%20edits%20from%20outside)); `EIO` and `ENOSPC` from sync, write and unlink | the storage seam of [§1.3](#1.3%20It%20is%20testable%20on%20its%20own), which drops or fails exactly the operations named |
| Model-based | sequences no hand-written case thinks of | random sequences of `WriteAt`, `Fill`, `Offload`, `Release`, `Truncate`, `Deallocate`, `Delete`, `Settle`, repack, crash and reopen, compared after every step against a reference model: each file's bytes, each extent's offloaded bit, and the removal markers `Since` yields. A failing sequence is shrunk to the shortest that still fails and kept as a regression case |
| Fuzz | parsers of bytes the journal did not just write: records, segment headers and trailers, catalogs | coverage-guided fuzzing; the property is that any input is either accepted as valid or rejected, never panics and never yields an extent the bytes do not contain |
| Concurrency | [§8](#8.%20Concurrency): lock order, readers against reclamation, progress | data-race detector, at least two files on two streams, virtual time for the waits |
| Structure | the dependency set of [§1.1](#1.1%20Non-goals) | an import test |
| Benchmark | [§9.6](#9.6%20Benchmarks), J1–J9 | real time, real device; after merge and daily, never per change ([§9.7](#9.7%20What%20CI%20checks%20instead%20of%20timing)) |
| Soak | what only appears after hours: index growth, descriptor leaks, footprint drift | mixed write, offload, release and repack load for hours against a capacity smaller than the data written, asserting that the [§4.1](#4.1%20What%20it%20must%20answer) counters, open descriptors and footprint stay flat |

The model-based test finds the interleavings no hand-picked case imagined — a
write during an offload, a fill racing a write, a release after a truncate —
which is where Group A failures come from.

### 9.2 Edge cases

Beyond the named checks, the unit, model and fault tests **MUST** reach these.

The edge cases of the on-disk format and of opening a journal are
[RFC 28 §4.1](rfc-28-journal-format.md#4.1%20Edge%20cases).

**Extent shape**

- a zero-length write, and a write at an offset close to the largest file offset;
- an overwrite that splits one held extent into three, and one that covers several whole extents and parts of two more;
- the same extent overwritten many times, so sequence numbers and index churn grow while held bytes do not;
- a write that crosses a segment boundary, and a record that exactly fills a segment;
- a file whose first held byte is far from offset zero ([§2](#2.%20The%20model%20it%20presents)).

**Lifecycle**

- truncate inside an extent, truncate to zero and write again, truncate down and back up, deallocate inside and across extents ([§3.6](#3.6%20Truncate%2C%20deallocate%20and%20delete));
- `Fill` over an extent that is partly held, partly dirty and partly absent ([§3.4](#3.4%20Fill));
- `Offload` where `fn` reports extents it was not offered, or reports none ([§3.3](#3.3%20Offload));
- `Release` of an extent only part of which carries an offloaded bit ([§3.5](#3.5%20Release));
- `Release` of the middle of one record, of one end, and of every part of it in separate calls ([§7.1](#7.1%20Releasing%20storage));
- a segment in which every record has been released, which must become reclaimable without a read.

**Capacity and storage**

- a reservation equal to exactly the remaining space, and one a byte over, against the share's limit and against the journal's ([§6](#6.%20Capacity));
- `ENOSPC` from the filesystem while the journal is below its own maximum, which **MUST** stay distinguishable from its own refusal;
- a sync that fails and then succeeds, on the same stream ([§5.3](#5.3%20A%20failed%20sync));
- a journal at its maximum asked to truncate, delete, release, mark offloaded and seal ([§6](#6.%20Capacity));
- one segment selected by eviction and by repack at once, retired by one and then by the other; accounted footprint **MUST** fall by the segment's size exactly once and never go below zero ([§7.3](#7.3%20Accounting));


### 9.3 Environments a check set must cover

| Dimension | Requirement |
| --- | --- |
| Filesystems | every filesystem the deployment supports, not one representative: sync and unlink semantics differ by platform ([RFC 28 Appendix A](rfc-28-journal-format.md#Appendix%20A%20%E2%80%94%20platform%20profile)) |
| Files and streams | at least two files across at least two append streams. A single-file rig maps to one stream and **structurally cannot** observe [§8](#8.%20Concurrency) |
| Shares | at least two shares on one journal, so per-share accounting and limits ([§6](#6.%20Capacity)) are exercised |
| Storage loss | by dropping writes at the seam ([§1.3](#1.3%20It%20is%20testable%20on%20its%20own)), never by terminating the process |
| Index scale | at least one check at an extent count where `O(n)` and `O(log n)` are distinguishable ([§4.1](#4.1%20What%20it%20must%20answer)) |

### 9.4 The checks

Grouped by what a failure *costs*, because that decides whether it blocks a
release.

The checks of the on-disk format and of recovery are
[RFC 28 §4.2](rfc-28-journal-format.md#4.2%20The%20checks), in the same groups.

#### Group A — silent data loss

A check here fails by **serving or destroying the wrong bytes without an error**.

| Requirement | Check |
| --- | --- |
| [§3.2](#3.2%20Read) no zero-fill | Fill `p` with a sentinel pattern, then read a never-written extent and an explicitly released extent; both report `missing`, the sentinel is untouched in both, and the two are indistinguishable to the journal. |
| [§3.3](#3.3%20Offload) write during offload | Write to an offered extent mid-callback, report it offloaded, assert the extent's bit stays unset and `Release` refuses it. |
| [§3.4](#3.4%20Fill) fill safety | Hold a `Fill` at the storage seam after it has found the extent absent, `WriteAt` the same extent, then let the fill continue; assert the written bytes survive. |
| [§3.4](#3.4%20Fill) fill sets the bit | Fill an absent extent; assert `Release` then permits it. `WriteAt` the same extent; assert `Release` refuses it again. |
| [§3.5](#3.5%20Release) refusal | Ask to release an extent with no offloaded bit; assert refusal and no partial progress. |
| [§7.1](#7.1%20Releasing%20storage) partial release keeps the record whole | Write one 1 MiB record; report only its first half offloaded; release that half; crash and reopen, once by catalog and once by scan. Assert the second half reads its exact bytes, is still dirty, and no loss event or corruption is reported. A design that frees part of a record fails: the record no longer verifies, and the dirty half resolves as **Lost**. |
| [§7.1](#7.1%20Releasing%20storage) release frees nothing inside a segment | Release extents of every shape against segments that stay held; assert the storage seam sees no operation on those segments but appends, and that their allocated size is unchanged. |
| [§7.2](#7.2%20Repack) repack copies the live part | Release the middle of an offloaded record; repack its segment; reopen. Assert both ends read their exact bytes, `MarkOffloaded` at the original versions marks them, the middle reads `missing`, and the original segment is unlinked. |
| [§7.2](#7.2%20Repack) crash mid-repack | Simulate a crash ([§1.3](#1.3%20It%20is%20testable%20on%20its%20own)) between the copy and the unlink; assert recovery selects the copies, every held extent still reads, and no extent is duplicated in the placement index. |
| [§8.5](#8.5%20Protecting%20readers%20from%20reclamation) unlink under read | Write a non-zero pattern into a segment holding nothing else, start a read of it, release the extent mid-read so the segment is unlinked; assert the read either returns the pattern or reports the extent missing with the sentinel in `p` untouched. |
| [§8.5](#8.5%20Protecting%20readers%20from%20reclamation) repack under read | Same, with a repack copy and unlink instead; assert the read sees the old or the new location, never neither. |
| [§3.10](#3.10%20Settle%20and%20Since) removals survive to `Since` | Write, then truncate, deallocate and delete across three files; crash before any `Settle`; reopen; assert `Since(id, 0)` yields each held extent and each removal marker at its version, and `Since(id, v)` for a removal's own version `v` does not yield it. `Settle` each file at its removal's version; assert `Since` no longer yields the marker and `RemovalMarkers` is zero. Crash and reopen again; assert the markers are back, unsettled. |
| [§3.6](#3.6%20Truncate%2C%20deallocate%20and%20delete) no resurrection | Write at v2, deallocate at v3 over it, then `Fill` the extent with a current `asOf` and version v2; assert the extent reads `missing`, and still does after reopen. Repeat with version v3: assert the fill is held, since only content below the marker's version is excluded. |
| [§7.2](#7.2%20Repack) removal outlives its segment | Write a file, overwrite it, delete it, then reclaim until the segment holding the delete is the only one selected; crash and reopen. Assert the file does not exist. A reclaim gate that counts only content records treats a segment holding just a removal as empty. |
| [§3.3](#3.3%20Offload) removal during offload | Truncate an offered extent while `fn` runs, then read `offered`; assert it returns the offered bytes, `ReadAt` reports the extent missing, and a report naming it marks nothing. Repeat with delete. |
| [§3.3](#3.3%20Offload) neighbours are frozen | Hold offloaded content either side of a dirty extent; offload with `widen`; while `fn` runs, overwrite one neighbour and release the other; assert `Offered` still returns the neighbours' original bytes, `Oldest` covers their versions, and a report naming them changes no bit. |
| [§7.2](#7.2%20Repack) removal records carried forward | Write A, offload; truncate it away; let every segment holding A's records but the oldest be repacked; reopen; assert the extent reads `missing`, not A. Repeat with a delete and a deallocate. Then truncate a file whose older records are all gone, leave the marker unsettled, repack the truncate's segment, reopen; assert `Since` still yields the marker. |
| [§7.2](#7.2%20Repack) copy loses to a newer write | Hold a repack after it has read a record and before it appends the copy; overwrite the extent and `Sync`, with no hold; let the repack continue; assert the new bytes are served, before and after reopen, and no copy of the old record was appended. Repeat with the overwrite unsynced: assert the copy is appended as retained, the source segment is unlinked, and failing the overwrite's window then serves the old bytes, before and after reopen. A pass that skips a source that has just become retained loses the flushed write when the window fails. |
| [§3.8](#3.8%20Loss%20events) loss raises the loss generation | Read `LossGeneration`; drop a corrupt dirty extent and assert it rose by one; drop a corrupt offloaded extent and a stale one and assert it did not move; fail a sync and resolve it by failing its window, and assert it rose again before the failing `Sync` returned; fail another and resolve it by re-appending, and assert it did not move. A design without the generation leaves the write verifier unchanged, and a client never resends the writes the loss took. |
| [§5.3](#5.3%20A%20failed%20sync) failed sync window | Write A and B to one segment; fail the sync at the seam and drop the unsynced writes; let a later sync of the segment succeed; assert `Sync` for A and B either failed or returned only after A and B were synced in another segment, and that after a crash both read back or `Sync` reported the failure. |
| [§3.3](#3.3%20Offload) offered bytes verified | Write a 1 MiB record, corrupt one byte at its end through the seam, offload with `limit` covering only its first 64 KiB; assert `Offered` returns no byte of it, the record is dropped as corrupt, and no report marks it. A design that checks only the bytes it returns hands the carver the first 64 KiB as good and the next pass the rotted rest. |
| [§7.2](#7.2%20Repack) repack verifies the whole source | Release the middle of an offloaded 1 MiB record, then corrupt a byte in the released middle; repack its segment; assert nothing of the record is copied, it is dropped as corrupt, and both ends read `missing` rather than bytes under a fresh checksum. |
| [§3.11](#3.11%20Snapshot%20holds) retained extents survive repack | Hold a cut over dirty v1, overwrite with v2, repack the segment holding v1, crash and reopen; assert `ReadAt` serves v2, the next offer carries v1 in `Superseded`, and after v1's commit is reported it leaves the index. A design that keeps only held extents in the index loses v1 at the repack. |
| [§7.2](#7.2%20Repack) removal dropped by version | Hold a cut over v1, truncate it away at v2, repack v1's segment so its retained copy gets a sequence number above the truncate's; release the hold and settle the marker; repack until the truncate's segment is selected; crash and reopen. Assert the extent reads `missing`. A rule comparing sequence numbers alone drops the truncate and the copy comes back held. |
| [§7.2](#7.2%20Repack) drops decided after the pass's copies | In one segment, put a settled truncate over v1 that a snapshot hold still retains, and nothing else of the file elsewhere; repack that segment; crash and reopen. Assert the extent reads `missing` and the truncate was carried forward with the retained v1. A pass that tests drops before appending its copies drops the truncate, carries v1, and brings it back held. |
| [§7.2](#7.2%20Repack) carried release keeps its sequence | Write A at v7 and offload it; release it; `Fill` the same extent at v7; repack the release record's segment; crash and reopen. Assert the extent reads the filled bytes. A repack that gives the release a new sequence number makes it outrank the fill. |
| [§5.3](#5.3%20A%20failed%20sync) re-append trusts no page cache | Write A, fail the next sync, and make reads of the failed segment return the bytes the device held before; resolve by re-appending; assert A is re-appended only from a copy that verifies against the checksum kept at write time, or its window is failed and A dropped as a loss — never re-appended with the stale bytes. Assert no record is appended to the failed segment afterwards. |
| [§3.8](#3.8%20Loss%20events) loss by exact record | Write v5 and `Sync` it; overwrite the same 64 KiB at v10 with no `Sync`; fail the timer's sync and resolve by failing the window. Assert a read serves v5, before and after a crash and reopen, and the loss record names v10's sequence number. A loss keyed on version inside the extent drops v5 too, and a design that let v5's record go once v10 was appended has nothing to restore. |
| [§3.8](#3.8%20Loss%20events) a synced loss masks what it superseded | Write v5, `Sync`; write v10, `Sync`; corrupt v10's payload; reopen with v5's record still on disk. Assert the extent reads `missing`, never v5. A loss record that names only its record brings the superseded v5 back. |
| [§5.3](#5.3%20A%20failed%20sync) a failed window is reported once | Write to file F with no `Sync` waiting; fail the timer's sync and fail the window. Assert the next `Sync(F)` fails, the one after succeeds, and a `Sync(G)` of a file with no write in the window succeeds throughout. A design that fails only the `Sync` calls waiting at the time lets F's next flush succeed with its data gone. |
| [§4.3](#4.3%20Versions) a re-append keeps its numbers | Write A, fail its sync and resolve by re-appending; truncate over A; crash so the original, its copy and the truncate are all on disk; reopen. Assert the extent reads `missing`. A re-append given a new sequence number outranks the truncate and brings A back. |
| [§3.3](#3.3%20Offload) no pin on a segment under repack | Select a segment for repack and hold the pass after its first copy; offload a file with dirty extents in that segment. Assert the offer pins no segment under repack, takes the copied extent from its new location, leaves the rest to a later call, and the repack completes and unlinks the segment. |

#### Group B — wedging and unbounded resource use

A check here fails by **reaching a state it cannot leave**, or by consuming without bound.

| Requirement | Check |
| --- | --- |
| [§6](#6.%20Capacity) reservation | Drive concurrent writers at the limit; assert the footprint never exceeds it. |
| [§6](#6.%20Capacity) named refusal | Fill a share to its limit. Assert the next `WriteAt` fails with `ErrNoSpace` naming the share's limit and the missing bytes, issues no I/O through the storage seam, and returns without waiting; release and repack that many offloaded bytes and assert the retry succeeds. Repeat against the journal's maximum. |
| [§6](#6.%20Capacity) share limit | Fill one share to its limit; assert its next write is refused naming the share's limit while another share still writes, and that per-share accounting is the same after reopen. |
| [§7.2](#7.2%20Repack) repack at capacity | Fill to capacity, then repack; assert it can run and that the journal recovers space without an external write succeeding first. |
| [§7.4](#7.4%20Open%20descriptors) descriptors | Create more segments than the descriptor bound; assert reads still succeed and open descriptors stay bounded. |
| [§4.2](#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes) coalescing | Offload and release 64 MiB written in 64 KiB writes, then `Fill` it back in one call per 4 MiB at one version; assert one entry per fill record piece, not per original write. Repack a segment holding adjacent held extents of one file at one version; assert the copy is one record and one entry. |
| [§4.2](#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes) index bound refuses | Set `ExtentLimit` to 10^4 and write scattered 4 KiB writes; assert the write that would pass the bound is refused naming the index, at once and with no I/O, `index_refusals` counts it, a truncate and a release still succeed, and after offload, release and repack a write succeeds again. |
| [§6](#6.%20Capacity) repack beside a share at its limit | Fill share A to its limit with offloaded content and share B with released content interleaved in the same segments; repack; assert the copies of A's records are made, B's unreclaimed storage returns, and no copy is refused by A's limit. A design that reserves repack copies against the share's limit returns nothing. |
| [§6](#6.%20Capacity) headroom is allocated | Open a journal, then make the filesystem return `ENOSPC` for every new allocation; assert content writes are refused, while releases, removals and `MarkOffloaded` up to the headroom succeed without allocating, and a repack still returns space. |
| [§3.3](#3.3%20Offload) pinned segments bounded | Scatter 64 KiB dirty extents of one file one per segment across 1,000 segments; offload with a 64 MiB `limit` and stall `fn`; assert `PinnedSegments` never exceeds the bound, the offer ends at it, and repack can retire the unpinned segments meanwhile. |
| [§5.3](#5.3%20A%20failed%20sync) a failed window does not wedge | Write A, fail its sync, resolve by failing the window; write B to the same file and `Sync`; assert the first `Sync` failed, the second succeeds once B is synced in a new segment, A reads `missing` with a loss event, and an offload of the file proceeds. A design that leaves A pending fails every later `Sync` of the file. |
| [§3.4](#3.4%20Fill) floor buckets | Read file X missing, then delete 10^4 other files; `Fill` X with the read's `asOf`; assert it is accepted. Read file Y missing, delete Y and recreate it with new content; `Fill` with the first read's `asOf`; assert it is refused. One journal-wide floor refuses the first. |
| [§4.1](#4.1%20What%20it%20must%20answer) bounds | Build a file index of 10^6 extents; assert point lookup and span query cost does not grow linearly with extent count. |
| [§5.3](#5.3%20A%20failed%20sync) sync failure | Fail a sync; assert the caller sees it and that subsequent writes to the same stream still attempt to sync. |
| [§6](#6.%20Capacity) records without bytes at the limit | Fill the journal to its maximum; assert truncate, deallocate, delete, release, `MarkOffloaded`, a seal and its footer all succeed without `ErrNoSpace`, and `headroom_draws` counts them. Make the filesystem return `ENOSPC` for a content write's new segment; assert that failure is reported as `ENOSPC`, not `ErrNoSpace`. |
| [§3.5](#3.5%20Release) freed means freed | Release the last held extents of a segment whose unlink the seam holds back; assert `UsedBytes` does not move and `UnreclaimedBytes` keeps the segment until the unlink completes. Repack another segment with the unlink held back; assert `Repack` returns `freed` only once it completes. |
| [§7.1](#7.1%20Releasing%20storage) storage returned | Release every held extent of a sealed segment; assert it is unlinked without a `Repack` call and accounted footprint falls by its size. Release part of another; assert footprint is unchanged, `UnreclaimedBytes` grows by the released bytes, and footprint falls only once repack has retired the segment. |
| [§8.6](#8.6%20Progress) reclaim not starved | Hold sustained read load on one segment; assert reclamation still acquires it within a bounded time. |
| [§8.1](#8.1%20What%20must%20not%20block%20what) removals do not wait on offload | Stall an offload pass inside `fn`, mid-upload; delete another file whose records share a segment with the offered ones, then truncate and deallocate the offered file itself. Assert each call returns without waiting for `fn`, and that the deleted file's storage is reclaimed once the pass returns. A design that holds a lock across the pass blocks the delete until the upload ends. |
| [§8.3](#8.3%20Lock%20ordering%2C%20and%20what%20may%20never%20be%20held) no deadlock | Run readers, writers, fills, repack and release concurrently on the same segments under a deadlock detector and a lock-order recorder; assert no cycle, every acquisition follows the total order, no two locks of one domain are held together, the storage guard is taken with no other lock held, and no lock is held across the offload callback. |
| [§8.7](#8.7%20Shutdown) background joined | Close the store with a repack in flight; assert it stops before any segment is closed and no work continues afterwards. |
| [§7.2](#7.2%20Repack) removal records dropped per extent | Keep one segment holding file X's filled content at an old version for the whole run; on file Y, issue 10^5 truncates, deallocates and releases, repacking as it goes. Assert Y's header-only records are dropped once no segment holds an older record of Y, and `HeadroomBytes` stays above half its size. A drop test against the lowest pair journal-wide keeps every one of them behind X's segment and drains the headroom. Then, in one file Z, keep one segment holding Z's filled content over `[0, 1 MiB)` for the whole run, and issue 10^5 releases and deallocates over `[1 GiB, 2 GiB)` of Z; assert they are dropped too. A drop test per file keeps them all behind Z's own old segment. |
| [§7.2](#7.2%20Repack) repack aimed at a share | Fill share A to its limit with records interleaved in segments mostly holding share B's unreclaimed bytes, and leave sparser segments of B alone elsewhere; `Repack` with A's scope. Assert `freed` comes from segments holding A's records and A's `UsedBytes` falls. Repeat with a segment-set scope naming two segments; assert only they are repacked. A journal-wide pass spends its budget on B's sparser segments. |
| [§6](#6.%20Capacity) segment and record accounting | Write records into an allocated segment; assert the journal's `UsedBytes` does not move while the share's `UsedBytes` rises by each record's bytes and `ReservedBytes` returns to zero after each. Let the segment go idle past the seal threshold; assert the seal truncates its unwritten tail and the journal's `UsedBytes` falls by it. |
| [§3.3](#3.3%20Offload) pins bounded across offers | Run eight concurrent offers, each over a file with dirty extents scattered one per segment across 200 segments. Assert `PinnedSegments` never exceeds the journal-wide bound plus eight, and each offer still offers at least one extent. A per-offer cap alone lets eight offers pin 512. |

#### Group C — recovery

A check here fails by **coming back up describing something other than what is on disk**.

| Requirement | Check |
| --- | --- |
| [§7.2](#7.2%20Repack) reseed after repack | Offload an extent, repack its segment, crash; `MarkOffloaded` at the offload's versions; assert the extent is marked and `Release` permits it. |
| [§3.5](#3.5%20Release) release survives restart | Write A, offload; write B over it into another segment, offload and release B; reopen; assert the extent reads `missing`, not A. |
| [§3.5](#3.5%20Release) release record lost | Release the last held extent of a segment and crash before the release record syncs; assert the segment was not unlinked and the extent is held with its original bytes. |
| [§3.4](#3.4%20Fill) fill beats an older write | Write A, offload, write B, offload, release B; `Fill` the extent with B's bytes; crash and reopen; assert the extent reads B, not A. |
| [§3.4](#3.4%20Fill) stale Fill | Read an extent missing; write, offload and release it; then `Fill` with the first read's `asOf`; assert nothing is written. Repeat with a truncate down and up in place of the write. |
| [§3.3](#3.3%20Offload) incremental report | Report one block's extents mid-callback, then fail the callback; assert the reported extents are marked and releasable and the rest are not. |
| [§4.3](#4.3%20Versions) epoch half is zero | Write a record whose version has a non-zero epoch half into a segment; assert the open fails, as for an unknown kind. |
| [§4.3](#4.3%20Versions) monotonicity | Reopen after a crash; assert the next sequence number and the next assigned content version each exceed every one on disk, and that recovery in shuffled segment order yields an identical index. |
| [§3](#3.%20Interface) attached share raises the counter | Open a journal whose counter stands at 10^3; attach a share with a floor of 9×10^8; assert the first write through it is assigned a version above the floor, and that a design attaching without raising assigns one below the share's imported refs. |
| [§3](#3.%20Interface) import attach holds no old extents | Write and offload file F under tag T; attach T again with `imported` set. Assert no read of F serves the old bytes and `Files` lists none of T's old files and `Since` yields no marker of them, or that `Share` failed naming T. A journal that attaches the tag as it stands serves the old clean extents. |
| [§3](#3.%20Interface) forget leaves nothing to settle | Write files F and G under tag T, delete G and leave its marker unsettled; `Forget(T)`; crash and reopen. Assert no extent or marker of T is held, `Since` yields nothing for F or G, `RemovalMarkers` does not count them, and `Forget` was refused while a handle of T was open. Attach T again, write F at a higher version, crash and reopen: assert the new write is held. A drop that deletes per file leaves a marker that a later attach's `Since` yields as a removal. |
| [§3](#3.%20Interface) unclaimed tag reported | Write dirty extents under tag T; reopen and attach no share for T. Assert `Stats().Shares` lists T with its held and dirty bytes, and that after `Forget(T)` it lists T no more. |
| [§3.6](#3.6%20Truncate%2C%20deallocate%20and%20delete) clone target survives a crash | `CloneTarget` file D with a spec; crash before any `Settle`; reopen. Assert `Since(D, 0)` yields the marker with its spec unchanged, and the extent reads `missing`. A marker that keeps only the extent hands the engine a plain removal and the destination reads zeros. |
| [§5.3](#5.3%20A%20failed%20sync) header-only records are never failed | Write A and `Sync`; in one window, truncate A's file and release another offloaded extent; fail the sync and make reads of the failed segment return stale bytes. Assert both records are re-appended, no loss event names either, and both extents read `missing` before and after reopen, and the failed segment carries no seal marker. |
| [§3.8](#3.8%20Loss%20events) a loss survives restart | Drop a corrupt dirty extent; crash before anyone reads `Losses`; reopen; assert the loss is in `Losses`, marked found at open, the dropped record is not held even if the corruption is repaired on disk, and a second reopen does not report it as new. |

#### Group D — observability

A check here fails by **leaving an operator unable to tell which of two opposite situations they are in**.

| Requirement | Check |
| --- | --- |
| [§3.7](#3.7%20State%20introspection) wedge is visible | Drive the journal to capacity with nothing evictable; assert one `Stats` call shows both, and which share holds the dirty bytes. Drive it to capacity with everything released but unrepacked; assert `UnreclaimedBytes` names it, per share. |
| [§7.3](#7.3%20Accounting) accounting follows the cap | On every supported filesystem, including one that allocates speculatively past the end of a growing file ([RFC 28 Appendix A](rfc-28-journal-format.md#Appendix%20A%20%E2%80%94%20platform%20profile)), write records into a segment in small appends; assert `UsedBytes` equals the sum of the segments' accounted sizes — the cap for each unsealed one, its size after seal for each sealed one — throughout, and the filesystem's allocated size never exceeds it. |
| [§3.8](#3.8%20Loss%20events) generation never repeats | Raise the loss generation to 3, close the journal and open it again in the same process; assert it reads at least 3, and that the next dirty loss raises it to 4. |
| [§3.7](#3.7%20State%20introspection) stats are cheap | Poll `Stats` under concurrent write load; assert it takes no lock a write needs and its cost does not grow with held extents. |
| [§3.7](#3.7%20State%20introspection) named inequalities hold | Poll `Stats` in a tight loop under concurrent writes, releases, truncates and repack; assert every sample keeps `DirtyBytes` ≤ `HeldBytes`, `ReservedBytes` ≥ 0 and `UsedBytes` ≥ the storage the seam reports allocated. |
| [§3.7](#3.7%20State%20introspection) backlog age | Write, advance the virtual clock, offload part; assert `OldestDirty` is the write time of the oldest extent still dirty. |
| [§3.7](#3.7%20State%20introspection) clean bytes count as occupancy | Fill the journal to its maximum with filled, offloaded content and nothing dirty or released. Assert `UsedBytes` is at the maximum while `DirtyBytes` and `UnreclaimedBytes` are zero, so a trigger on allocated occupancy fires and one on dirty plus unreclaimed bytes does not. |
| [§3.9](#3.9%20Metrics) metrics are reachable | Drive every condition a metric names — a refused reservation, a failed sync, a corrupt dirty extent, a rejected segment, a torn tail — and assert each moves its metric through `Stats`. |
| [§3.8](#3.8%20Loss%20events) loss events | Drop a corrupt dirty extent and a stale one; assert each appears in `Losses` with its file, extent, reason and bit, and that a poller that missed events can tell how many. |
| [§4.1](#4.1%20What%20it%20must%20answer) segment held-bytes | Write, release and repack across several segments; assert the per-segment counter matches a recomputed walk at every step. |
| [§4.4](#4.4%20Inspection) verification reports | Introduce a deliberate index/segment disagreement; assert verification names it and changes nothing. |

### 9.5 What must not stand in for the real thing

The index's rules apply ([test tiers](rfc-index.md#Test%20tiers)). For the journal in particular, an
in-memory segment implementation **MUST NOT** stand in for the filesystem in
[§7.1](#7.1%20Releasing%20storage), [RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding) or [RFC 28 §3.3](rfc-28-journal-format.md#3.3%20Torn%20and%20corrupt%20records): unlink durability, torn tails and bit corruption are
properties of real storage.

### 9.6 Benchmarks

The journal is on every write's path and every warm read's. Benchmarks run on a
real filesystem on the kind of device a deployment uses — a RAM-backed one makes
`fsync` free and so measures everything except what bounds a write — and in real
time. Each target is a fraction of the filesystem underneath, measured with a raw
I/O benchmark on the same filesystem with the same block size, queue depth and
sync pattern. The targets are proposed, to be confirmed once J1 has run.

| # | Measures | Setup | Proposed target |
| --- | --- | --- | --- |
| J1 | write path | `WriteAt` sequential and random, 4 KiB to 1 MiB, with and without a `Sync` after each write ([§5.2](#5.2%20Sync%20policy)), 16 writers | sequential ≥ 256 KiB: ≥ 95% of the filesystem; random 4 KiB: ≥ 90% of its IOPS with an `fsync` per write; `WriteAt` excluding sync and copy: p99 ≤ 20 µs |
| J2 | write scaling | 1, 4, 16 and 64 files written concurrently | rises to within 5% of the filesystem, then stays there to 64 |
| J3 | read path | `ReadAt` over held extents, index at 10^3 to 10^6 extents ([§4.1](#4.1%20What%20it%20must%20answer)) | point lookup ≤ 1 µs at 10^6; warm 1 MiB `ReadAt` ≥ 2 GB/s single stream |
| J4 | offload offer | `Offload` over files of many small extents and of few large ones, `fn` returning at once | ≤ 1 µs per offered extent; index insertion at 10^6 ≤ 2 µs |
| J5 | reclamation under load | release and repack ([§7](#7.%20Reclamation%20mechanisms)) while J1 runs | J1's p99 ≤ 1.5× J1 alone |
| J8 | repack write amplification | eviction-heavy load: J1 against a capacity well below the data written, the engine releasing offloaded content continuously; report `repacked_bytes` per `released_bytes` and J1's throughput against J1 alone | none yet; the gate of [§7.1](#7.1%20Releasing%20storage)'s marker |
| — | `Stats` | polled under write load | O(1), ≤ 1 µs |

J6, J7 and J9 measure recovery, reseed and the journal's shape, and are
[RFC 28 §4.3](rfc-28-journal-format.md#4.3%20Benchmarks)'s; the setup and method here apply to
them too.

Method: run J1 and J5 long enough to exhaust the device's write cache and report
the rate after it; state each row's queue depth; repeat J1 with more data than
the journal holds, reporting submission and completion latency apart; report
create and overwrite separately. Each result also records the device's raw
figures, the filesystem and mount options, and the sync bound, besides what
[the index](rfc-index.md#Test%20tiers) requires.

### 9.7 What CI checks instead of timing

No timed check runs per change ([tiers](rfc-index.md#Test%20tiers)). The regressions that matter here — a
lost group commit, an index that stopped being `O(log n)` — are counted instead:

- **syncs per write**: 16 concurrent writers on one append stream,
  every writer calling `Sync` after each write ([§5.2](#5.2%20Sync%20policy)); the storage seam counts syncs and holds each
  one until all 16 are waiting, so scheduling cannot decide the result. They
  **MUST** issue at most one sync per four writes. A design without group commit
  issues one per write; one that groups all 16 issues one per 16. The check sits
  a factor of four from each, so neither scheduling nor a leader handing off
  mid-round moves a correct design across it;
- **index cost**: the placement index counts comparisons per lookup and per
  insertion; going from 10^3 to 10^6 extents **MUST** at most double them, the
  ratio log(10^6) / log(10^3) an `O(log n)` structure gives;
- **allocations per operation**: `WriteAt`, `ReadAt` and `Stats` are measured
  in allocations per call, and an increase fails the check;
- **index memory**: bytes per entry at 10^6 extents **MUST** stay at or below 56,
  the estimate of [§4.2](#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes) plus a tenth.
- **report cost**: a pass that offloads a file of 10^3 and of 10^6 extents, written
  sequentially so none merge, reports block by block; comparisons per reported
  extent **MUST** at most double between the two, the same logarithmic ratio
  ([§3.3](#3.3%20Offload)).

## 10. Open questions

The open questions of the on-disk format and of recovery are
[RFC 28 §5](rfc-28-journal-format.md#5.%20Open%20questions).

1. **Repack write amplification** ([§7.1](#7.1%20Releasing%20storage)). Freeing storage only in whole
   segments makes every reclaimed byte cost a copy of its live neighbours; how
   much under an eviction-heavy load is unmeasured (J8), and decides whether
   whole-record freeing at release is worth adding.
2. **Index insertion cost** ([§4.2](#4.2%20The%20index%20is%20bounded%20by%20extent%20count%2C%20not%20by%20bytes)). What the journal does at the entry bound is
   settled: it refuses the write. Unmeasured is **insertion** cost — the
   benchmark covered memory and lookup, and it is insertion, not lookup, where a
   sorted slice actually fails.
3. **The scope of one sync.** A sync covers one append stream ([§5.2](#5.2%20Sync%20policy)), and
   more streams spread writers across more syncs: a create-heavy load split over
   many streams shares fewer syncs, and measured slower as streams were added.
   Whether one leader may sync several streams at once, or streams should be
   fewer than writers' natural grouping suggests, is unmeasured against the
   contention more streams remove.
4. **Headroom, reserves and thresholds** ([§6](#6.%20Capacity), [§3.3](#3.3%20Offload), [RFC 28 §3.1](rfc-28-journal-format.md#3.1%20Rebuilding)). The headroom's
   size (`journal.headroom_records`), the repack reserve of one segment, the bound
   of 64 pinned segments per offer and of 256 across offers, and the 16 MiB
   `journal.idle_seal_bytes` are proposals; none has been measured
   against a journal held at its limit or against recovery time.
