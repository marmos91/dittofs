---
rfc: 4
title: "RFC 4 — the remote block store: block format and store contract"
component: remote tier
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-2-carver]]"
  - "[[rfc-3-syncer]]"
  - "[[rfc-5-transforms]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-9-gc]]"
  - "[[rfc-12-snapshots]]"
  - "[[rfc-13-configuration]]"
aliases:
  - RFC 4
tags:
  - rfc
---
# RFC 4 — the remote block store: block format and store contract

**Status:** draft.
**Audience:** anyone implementing a remote block store, or reading or writing
blocks. Conventions and test tiers are in [the RFC index](rfc-index.md).

This document specifies behaviour, not the current code. Where the code differs,
[Appendix A](#Appendix%20A%20%E2%80%94%20where%20the%20current%20code%20differs) lists it for the refactor.

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

DittoFS keeps recent writes on a fast local disk and copies them, in the
background, to an object store such as an S3 (Amazon Simple Storage Service)
compatible bucket, which is the lasting home of all file content. This RFC
specifies the two things on either side of that copy: the **block**, the one
kind of object DittoFS stores there, and the **remote block store**, the thin
adapter that puts, gets, lists and deletes blocks on one particular service.

In outline: clients write over NFS (Network File System) or SMB (Server Message
Block); writes land in a local journal and are acknowledged from there; later
the bytes are cut into chunks of about 256 KiB, packed into blocks of about
4 MiB, and uploaded; once the upload is recorded, the local copy may be dropped,
and a later read fetches it back ([RFC 0](rfc-0-data-lifecycle.md)).

### The problem, in one example

The installation runs on node N1, and its remote tier is the bucket `dfs-data`.
alice's profile disk, `profiles/alice/ODFC_alice.vhdx` (a 30 GB virtual hard
disk file), receives 64 KiB overwrites all day.

1. **N1 starts.** Before it serves anything, the store checks the real service:
   it puts a small object with a deliberately wrong checksum and expects the
   service to refuse it, reads the bucket's versioning and expiry settings, and
   lists what it just wrote. Services that speak the same protocol differ here:
   some accept a wrong checksum and store the bytes anyway. If the service
   neither checks a checksum nor reports a trustworthy digest of what it stored,
   the store refuses to open, naming what is missing.
2. **The syncer uploads block `K1`**: 4 MiB holding 16 chunks of alice's disk.
   The store sends one put: the block's size and checksum first, then the
   header, then each chunk body, encoded as it is read from the journal. One bit
   flips on the way.
3. **Without the in-transit check**, the service stores the damaged block and
   answers success. The journal, told the block is stored, lets its copy go.
   Days later alice signs in, her disk is read back, the chunk fails its hash
   check, and the only good copy is gone.
4. **With it**, the service refuses the put (or its reported digest does not
   match), and the store returns "corrupt in transit". The store does not
   retry: it remembers nothing between calls. The syncer retries, streaming the
   same bytes from the journal under the same name. The second put succeeds,
   and success means the block survives the loss of N1.
5. **The next morning** alice signs in and her disk's header is no longer local.
   The engine asks for one chunk by the offset and length metadata recorded for
   it. The store returns exactly those bytes or an error, never fewer; the block
   codec undoes compression and encryption and checks the chunk's hash before
   any byte reaches alice.

```text
 N1                                                  remote tier
 journal (NVMe) ──► block codec ──► syncer ──► store ──► bucket dfs-data
 alice's bytes     header + 32     retries,    one       blocks/<K1 in hex>
                   chunk bodies    pools,      attempt   control/claim
                   (RFC 5 per      health      per call  control/health
                   chunk)            ▲            │
                                     └────────────┘
                                 nil = stored, or one of six errors
 read: store ──► exact byte range ──► codec: decode, check hash ──► engine
```

### The words you need

- **[Block](rfc-0-data-lifecycle.md#Glossary)**: one object in the bucket, a
  header indexing its chunks followed by one body per chunk, written by one put
  ([§3.2](#3.2%20Layout)).
- **Block name**: the block's 32-byte identity, minted afresh for each put
  attempt and never reused, so one name is always one byte sequence
  ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)).
- **Block codec**: the code above the store that encodes blocks, decodes them
  and verifies every chunk it returns ([§3.4](#3.4%20Every%20read%20is%20verified%20by%20the%20codec)).
- **Remote block store**: one backend's adapter. It maps names to keys and makes
  one attempt per call ([§2](#2.%20The%20dividing%20line)).
- **Capability check**: the test against the real service, run every time a
  store opens ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)).
- **Control object**: a small fixed-role object beside the blocks, such as the
  namespace claim, never listed as a block ([§4.13](#4.13%20Control%20objects)).
- **Backend profile**: what one service must offer and how the contract maps
  onto it; S3 is [Appendix C](#Appendix%20C%20%E2%80%94%20the%20S3-compatible%20block%20store).

### What this RFC promises

- A put that succeeds means the whole block arrived intact and survives the loss
  of this machine; there is no partial put.
- A get returns exactly the bytes asked for, or an error. A short or clamped
  answer is an error, never data.
- No chunk leaves the codec unless its hash matches; damaged bytes are an error,
  never content.
- Where metadata says a chunk sits inside a block stays right for the block's
  life, because a name is never written with different bytes.
- A store does not open on a service missing a feature it relies on, or on a
  bucket whose versioning or expiry rules would keep or delete blocks behind its
  back; a setting that drifts later stops puts and deletes until it is fixed. The
  one store that opens on a versioned, object-locked bucket is a backup location
  configured `immutable`, which then requires those settings, never deletes,
  sets a capped retain-until on every put, and keeps every version of its
  exports and state objects listable.

### How the rest is organised

[§2](#2.%20The%20dividing%20line) draws the line between the store and the syncer
above it. [§3](#3.%20The%20block%20format) is the block format and the codec;
[§4](#4.%20The%20store%20contract) is the contract, one operation per
subsection. [§5](#5.%20Backend%20profiles) and
[Appendix C](#Appendix%20C%20%E2%80%94%20the%20S3-compatible%20block%20store)
map it onto S3. [§6](#6.%20Invariants) lists the invariants and
[§7](#7.%20Test%20plan%20and%20benchmarks) the tests. On a first read, skip
§4.10, §4.12, §7 and the appendices; [Appendix B](#Appendix%20B%20%E2%80%94%20measurements)
holds the measurements behind the decisions in [§8](#8.%20Decisions%20and%20open%20questions).

## In short

- A **block** is the unit the remote tier stores: a header that indexes its
  chunks, then the chunk bodies. The engine composes and encodes it; the store
  never parses it.
- A **remote block store** keeps encoded blocks by name. Its operations: put a
  whole block, get a whole block or one byte range, delete, list, check health,
  re-check the service's settings, close — plus a put and get of a few small
  **control objects** by fixed role, such as the namespace claim ([§4.13](#4.13%20Control%20objects)), and at a
  backup location the **versioned objects** of each backup — export, state and
  progress — listed by version ([§4.15](#4.15%20Versioned%20objects%20at%20a%20backup%20location)).
- The store makes one attempt per call and remembers nothing between calls; the
  syncer owns retries, concurrency and health state. The block codec verifies
  every chunk read.
- Each backend lists the service features it needs and, on opening, exercises
  every one against the real service, refusing to open if any is missing.

## 1. Purpose

Two things need specifying:

- **the block format**: what an encoded block looks like, so that everything
  reading or writing blocks agrees on it;
- **the store contract**: what any remote block store (S3-compatible, a
  filesystem, anything else) must do, so the syncer ([RFC 3](rfc-3-syncer.md)) and garbage
  collection ([RFC 9](rfc-9-gc.md)) can rely on it without knowing which store they hold.

### 1.1 Non-goals

This document **MUST NOT** be read as specifying:

- when to transfer, or what: that is offload and eviction policy ([RFC 0 §5.2](rfc-0-data-lifecycle.md#5.2%20Offload), [RFC 0 §8.1](rfc-0-data-lifecycle.md#8.1%20Evict));
- what to delete: that is sweep ([RFC 9](rfc-9-gc.md));
- concurrency, retry or health state: that is the syncer ([RFC 3 §2](rfc-3-syncer.md#2.%20What%20both%20halves%20obey));
- how a block's name is derived: that is [RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block);
- what compression and encryption do to a chunk: that is [RFC 5](rfc-5-transforms.md).

### 1.2 A contract, not a component

Each consumer declares, on its own side, the few methods it needs
([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)). The syncer needs put, get and health; GC needs delete, list
and the settings re-check, since its relocation reads and puts go through the syncer
([RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface)). This document fixes what those methods *mean*, once, so a narrow
interface never silently drops a guarantee. A backend implements the full
interface of [§4.1](#4.1%20Interface) and satisfies every narrow one structurally.

**Two components, one document.** The block codec ([§3](#3.%20The%20block%20format)) and the remote block
store ([§4](#4.%20The%20store%20contract)) are separate components with no call between them: the codec
never calls a store, and a store never parses a block. Each is implemented and
tested alone ([§7.1](#7.1%20Conformance%20suite), [§7.3](#7.3%20Codec%20tests)). Neither calls the syncer, GC or the engine, but
several rules here are split with them — health is probed here and derived in
[RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work), a body's bounds come from [RFC 5 §3.1](rfc-5-transforms.md#3.1%20Interfaces), the settings re-check is
called by [RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period) and by the holder of a backup location
([RFC 12 §3.4.1](rfc-12-snapshots.md#3.4.1%20Layout%20at%20the%20location)) — so this document builds on those RFCs, and its frontmatter
says so. The cycles that makes (this RFC with RFC 3, 5, 9 and 12) are reviewed
together, as [the RFC index](rfc-index.md) lists them, rather than hidden by
leaving the edges out.

> decision: the codec and the store share one document because the layout of
> [§3.2](#3.2%20Layout) and the store's guarantee that one name is one byte sequence
> ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)) are read together by every implementer of either. Split §3 into its own
> document when a second block format, or a second codec, needs specifying.

## 2. The dividing line

> **A store performs one operation on one block and remembers nothing between
> operations. Everything that spans operations belongs above it.**

The test for a new concern: if removing it would change the outcome of **one
call**, it belongs in the store; if it would only be noticed **across calls**, it
belongs above.

Both sides earn their place. The syncer exists so that concurrency, retry and
health logic are written once, independently of any particular store. The store
exists so that each service's quirks stay in one place, and so there is a seam
where tests can substitute a store that loses, corrupts, delays and
half-completes ([§7.2](#7.2%20Fault%20transport)).

![The boundary: below it, one operation on one block, with no memory between calls; above it, the state that spans transfers](img/rfc4r-boundary.svg)

| Concern | Store | Above the store |
| --- | --- | --- |
| Encoding, compression, encryption | none: receives and returns opaque bytes | block codec ([§3](#3.%20The%20block%20format), [RFC 5](rfc-5-transforms.md)), run by the engine and GC |
| Verifying chunk content | none | block codec, on every read ([§3.4](#3.4%20Every%20read%20is%20verified%20by%20the%20codec)) |
| Transport integrity | protects every put ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)) | none |
| Where a block lives in the service | maps name to location ([§4.2](#4.2%20Names%20in%2C%20locations%20kept%20inside)) | never sees a location |
| Retry | none: one attempt per call ([§4.9](#4.9%20No%20state%20across%20calls)) | syncer ([RFC 3 §2.4](rfc-3-syncer.md#2.4%20Every%20transfer%20terminates%2C%20and%20reports)) |
| An unknown outcome | reports it as a transient error | syncer resolves it as not stored ([RFC 3 §2.5](rfc-3-syncer.md#2.5%20An%20unknown%20outcome%20is%20not%20a%20success)) |
| Concurrency, fairness, memory bound | none | syncer's worker pools ([RFC 3 §2.1](rfc-3-syncer.md#2.1%20A%20worker%20pool%20is%20the%20only%20concurrency%20control)) |
| Connection pool size | derived from the pools at construction ([§4.10](#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers)) | pool sizes |
| Health | one probe call ([§4.7](#4.7%20Health%20is%20one%20probe%20call)) | syncer's derived state, and refusing work ([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)) |
| Listing | walks every block once ([§4.6](#4.6%20List%20is%20a%20complete%2C%20resumable%20walk)) | GC decides what a listed block means ([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)) |
| Deletion | deletes, idempotently ([§4.5](#4.5%20Delete%20is%20batched%20and%20idempotent)) | GC decides what to delete ([RFC 9](rfc-9-gc.md)) |

## 3. The block format

### 3.1 Who writes it

The **block codec** encodes and decodes blocks. The engine uses it when it
assembles a block ([RFC 2 §5](rfc-2-carver.md#5.%20The%20block%20assembler), [RFC 8 §6.3](rfc-8-engine.md#6.3%20The%20offload%20pipeline)), and GC uses it, through the syncer, when it relocates chunks
([RFC 9 §4.2](rfc-9-gc.md#4.2%20Read%20verified%2C%20mint%2C%20put%2C%20then%20move)). The store receives the encoded bytes and returns them unchanged.

### 3.2 Layout

A block is a **header** followed by the **chunk bodies**, in order.

| Element | Where | Why it is there |
| --- | --- | --- |
| Format marker and version | first bytes | a reader built for another version, or handed something that is not a block, fails loudly instead of misparsing |
| Header length | first bytes | a reader fetches the whole header in one ranged request, rarely two |
| Block name, nonce, chain ID | header | a block served under the wrong key is detected instead of used. The name is the block's identity in block metadata too ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)), so it links the stored bytes back to their record; the nonce and chain ID are the name's inputs besides the hashes and the key scope, so a whole-block read can recompute it. |
| Chunk index | header | at most `N` entries ([RFC 2 §5](rfc-2-carver.md#5.%20The%20block%20assembler), P4), one per chunk: its hash, and the offset and length of its body. The hash is the plaintext hash, or its sealed form — keyed under a namespace secret — when the namespace encrypts ([RFC 5](rfc-5-transforms.md)). When the namespace encrypts, the offsets and lengths are sealed too, as one AEAD value under a key derived from the header key ([RFC 5 Appendix B.5](rfc-5-transforms.md#B.5%20What%20a%20bucket%20reader%20still%20learns)), so the header shows a bucket reader no chunk length. One chunk can be read with one ranged request at the range block metadata records ([RFC 6 §2.2](rfc-6-block-metadata.md#2.2%20Chunk)), without reading the header or the others. |
| Seal | header | `none`, or the ID of the header key that sealed the index's hashes and ranges ([RFC 5 Appendix B.5](rfc-5-transforms.md#B.5%20What%20a%20bucket%20reader%20still%20learns)). A reader checks the index under the key the block names, never under the namespace's current one, so a block stays checkable after the header key rotates |
| Chunk bodies | after the header | each chunk's bytes, transformed if a chain is configured ([§3.3](#3.3%20Transforms)) |

Five requirements on the layout:

- Each body **MUST** be decodable on its own, so one corrupt body loses one chunk,
  not the block.
- The header length **MUST** be bounded by a constant of the format version: `N`
  times the size of one index entry, plus the fixed fields. The codec **MUST**
  refuse a header that declares a larger length, or more than `N` entries, before
  allocating for it.
- Each body's declared length **MUST** be at most `MaxDecodeLen(Max)`, the
  largest body any registered transform could have written ([RFC 5 §3.1](rfc-5-transforms.md#3.1%20Interfaces)), and
  the codec checks it before allocating for the body. The read side never uses
  the current chain's `MaxEncodedLen`: a block written under an older chain must
  stay readable.
- Each body begins with its envelope, whose first byte is the envelope's own
  version ([RFC 5 §2.4](rfc-5-transforms.md#2.4%20Every%20body%20records%20what%20was%20applied)), so a ranged get of one body, which never sees the block
  header, still learns how to decode it.
- The name **MUST** be known before encoding starts, because it is written into
  the header. [RFC 8 §6.6](rfc-8-engine.md#6.6%20A%20block's%20name%20is%20minted%2C%20and%20its%20intent%20recorded%2C%20before%20the%20put) mints it from the attempt's nonce and the chunk
  hashes, which exist before any byte is encoded.

A chunk's ID is a keyed hash of its plaintext under the namespace's chunk-ID key
([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)), so no block of any namespace carries a hash a bucket
reader can compute from a file it holds. When the namespace encrypts, the index
holds the chunk IDs sealed once more under the header key the seal field names,
and the name is derived from those ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)). Block metadata keys
chunks by chunk ID; only the stored header is sealed.

The header comes first, so every body's length is known before the first byte is
sent: from each transform's declared length, or from a measuring pass when the
chain cannot declare it ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)). Only the header and one chunk are held in memory.

The byte-level encoding is left to the codec: field widths, integer encoding and
field order are not part of this contract. Changing them is a migration ([§3.5](#3.5%20Format%20changes%20are%20migrations)).

![A block: a header carrying marker, version, name and a chunk index, then one body per chunk; a ranged read addresses one body by offset and length](img/rfc4r-block-layout.svg)

### 3.3 Transforms

A chunk body **MAY** pass through a chain of transforms, such as compression and
encryption ([RFC 5](rfc-5-transforms.md)). Three rules hold for any chain:

1. **Decode inverts encode exactly.** For every chunk,
   `decode(encode(plaintext)) == plaintext`, byte for byte.
2. **Identity is over plaintext.** A chunk's ID and a block's name are
   functions of plaintext ([RFC 2 §4.1](rfc-2-carver.md#4.1%20A%20chunk), [RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)) — keyed under the namespace's
   chunk-ID key, and sealed once more when encrypting — never of transformed bytes. If they depended on
   transformed bytes, a compression level or key change would re-key the corpus
   and stop deduplication.
3. **The chain travels with the block.** How a body was transformed **MUST** be
   recoverable from the body itself ([RFC 5 §2.4](rfc-5-transforms.md#2.4%20Every%20body%20records%20what%20was%20applied)), not from configuration. Configuration describes
   the next write only; otherwise changing it would make stored data unreadable.

Transforms change body lengths, so a chunk's offset inside an encoded block
differs from its offset in the journal. That is expected: block metadata records
a chunk's offset in the encoded block, not in the journal ([RFC 6 §2.2](rfc-6-block-metadata.md#2.2%20Chunk)), and nothing
compares the two. Above the codec, behaviour **MUST NOT** differ with transforms
on or off, apart from these offsets and timing.


### 3.4 Every read is verified by the codec

The codec **MUST** check every chunk it returns against the plaintext hash the
caller expects, after undoing every transform, and **MUST** return an error rather
than unverified bytes. A whole-block read also checks each index entry against
the chunk it decoded — the plaintext hash, sealed when the namespace encrypts —
then recomputes the block's name from those entries, the header's nonce and
chain ID, and the key scope ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)), and checks it against the name it was
fetched under. Comparing the header's name field alone would accept a header
whose index was altered. A sealed index is checked under the header key the
block's seal names, which **MUST** be in the block record's census like any
other material ([RFC 5 §2.5](rfc-5-transforms.md#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material)); a seal naming a key outside it is malformed.

The codec decodes each body with `Chain.Decode`, passing the block record's
census ([RFC 5 §3.1](rfc-5-transforms.md#3.1%20Interfaces), [RFC 6 §2.3](rfc-6-block-metadata.md#2.3%20Block)): a body naming material the census does not list
is malformed, and its material is never requested from the provider.

No consumer reads block bytes except through the codec. Verification then happens
in one place, and every consumer (the engine's fetch, relocation, snapshot
verification) inherits it.

**A recorded position never goes stale.** One name is only ever stored as one
byte sequence ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)), so the position block metadata records for a chunk
([RFC 6 §2.2](rfc-6-block-metadata.md#2.2%20Chunk)) stays right for the life of the block. A ranged read that fails
verification, or runs past the end, is corrupt: the codec does not re-read the
header to look for the chunk elsewhere. When the store rejects a recorded range
as past the end (`ErrInvalid`), the codec reports `ErrCorrupt` of kind
`malformed`: the range came from a record, not from a caller's mistake, so the
stored block is shorter than its record says. `ErrNotFound` passes through
unchanged, for the engine to re-resolve ([§4.8](#4.8%20Errors%20are%20a%20closed%20set)).

### 3.5 Format changes are migrations

A change to the layout **MUST** advance the version, and readers **MUST** accept
every version that may still be stored. Stored blocks are already kept and
have no rewrite point, so an incompatible reader fails at the next read, not at
the change.

**The version written is a namespace setting.** A writer **MUST** refuse to write
a block format version newer than every reader of the namespace supports. The
version to write is recorded with the namespace ([RFC 13](rfc-13-configuration.md)) and is advanced only
after every binary that may read the namespace supports it; a binary that finds
a write version it does not know refuses to write, and still reads what it can.
So a rolling upgrade never leaves an older node facing blocks it cannot parse.

## 4. The store contract

### 4.1 Interface

Signatures are indicative; the obligations in [§4.2](#4.2%20Names%20in%2C%20locations%20kept%20inside)–[§4.15](#4.15%20Versioned%20objects%20at%20a%20backup%20location) are normative.

```go
// Name is a block's name (RFC 2 §4.2): derived from its chunk hashes, key scope,
// the put attempt's nonce and chain ID.
type Name [32]byte

// Role names a control object (§4.13). The set is fixed in code.
type Role uint8

const (
	RoleClaim  Role = iota + 1 // the namespace claim (RFC 12)
	RoleHealth                 // the health probe's object (§4.7)
	RoleRepair                 // the proof that a suspended store holds no old versions (§4.11)
	RoleKeys                   // the namespace's key records, wrapped; a non-encrypting namespace's chunk-ID key in the clear (RFC 5 Appendix B.2)
)

// ObjectKind names a versioned object at a backup location (§4.15). The set is
// fixed in code.
type ObjectKind uint8

const (
	ObjExport   ObjectKind = iota + 1 // one backup's export (RFC 12 §5.1)
	ObjState                          // one backup's state object (RFC 12 §3.4.1)
	ObjProgress                       // one batch of a running copy (RFC 12 §3.4.3)
	ObjKeys                           // the namespace's key records, one object per namespace, Backup empty (RFC 5 Appendix B.2)
)

// ObjectKey names one versioned object: its kind, the backup it belongs to,
// and for progress its batch number.
type ObjectKey struct {
	Kind   ObjectKind
	Backup string
	Batch  uint32
}

// ObjectInfo describes one stored version of a versioned object.
type ObjectInfo struct {
	Key         ObjectKey
	Version     Version   // empty on a mutable location
	Size        int64
	ModTime     time.Time // when the service stored it; reported, never trusted to order versions
	RetainUntil time.Time // zero on a mutable location
}

// Mode is how a store treats versions and deletes (§4.14). Every namespace's
// store is Mutable; a backup location's block folder is either.
type Mode uint8

const (
	Mutable   Mode = iota + 1 // no versioning, no lock, deletes allowed
	Immutable                 // versioning and compliance lock required, no delete
)

// Version names one stored version of a name in an immutable store (§4.14).
type Version string

// Checksum names the checksum a put carries up front, as the capability check
// chose it (§4.11), or None when the store verifies the service's digest instead.
type Checksum uint8

// Direction is put or get; health is probed per direction (§4.7).
type Direction uint8

const (
	DirPut Direction = iota + 1
	DirGet
)

// Range is a byte range in an encoded block. The zero Range means the whole block.
type Range struct{ Off, Len int64 }

// Info describes one stored block, as a listing reports it.
type Info struct {
	Name    Name
	Size    int64     // GC reports the space an unrecorded block holds
	ModTime time.Time // when the service stored it; reported, never used to decide a delete
}

type Store interface {
	// Checksum says which checksum Put needs before the first byte, if any.
	Checksum() Checksum

	// Put streams one whole encoded block of exactly size bytes, with sum when
	// Checksum is not None. It returns nil only once the block is stored.
	Put(ctx context.Context, name Name, body io.Reader, size int64, sum []byte) error

	// PutVersion is Put on an immutable store, under a compliance-mode
	// retain-until carried by the put itself, returning the version the service
	// stored (§4.14). On a mutable store it is ErrInvalid.
	PutVersion(ctx context.Context, name Name, body io.Reader, size int64, sum []byte, retainUntil time.Time) (Version, error)

	// Get returns exactly the bytes of r, or of the whole block when r is the
	// zero Range. The reader yields exactly that many bytes or fails.
	Get(ctx context.Context, name Name, r Range) (io.ReadCloser, error)

	// GetVersion is Get of one stored version, on an immutable store (§4.14).
	GetVersion(ctx context.Context, name Name, v Version, r Range) (io.ReadCloser, error)

	// Retention reads one version's current retain-until (§4.14).
	Retention(ctx context.Context, name Name, v Version) (time.Time, error)

	// ExtendRetention raises one version's retain-until to until. It reads the
	// current value first and, when until is not later, succeeds without a
	// change: it never asks the service to lower one (§4.14).
	ExtendRetention(ctx context.Context, name Name, v Version, until time.Time) error

	// Delete removes blocks, as many as the caller passes. It returns one
	// error per name, in order; nil means that block is gone, as the reply
	// stated for that name (§4.5). Deleting an absent block succeeds.
	Delete(ctx context.Context, names []Name) []error

	// DeleteVersions removes every stored version of each name, and any
	// delete marker (§4.5). Used only after a versioning drift.
	DeleteVersions(ctx context.Context, names []Name) []error

	// List yields every stored block once, in ascending name order, starting
	// after the given name (the zero Name starts at the beginning).
	List(ctx context.Context, after Name) iter.Seq2[Info, error]

	// Health makes one bounded probe round trip of direction d against the
	// store's own namespace (§4.7): a probe put for Put, a probe get for Get.
	// On an immutable store, DirPut is ErrInvalid without a request.
	Health(ctx context.Context, d Direction) error

	// Recheck re-reads the service settings that can drift after open
	// (§4.11). A non-nil result names the drift; the caller stops puts and
	// deletes until a later Recheck passes.
	Recheck(ctx context.Context) error

	// PutControl and GetControl store and read one small control object by
	// role (§4.13). They are never listed as blocks.
	PutControl(ctx context.Context, role Role, body []byte) error
	GetControl(ctx context.Context, role Role) ([]byte, error)

	// PutObject, GetObject, ObjectRetention, ExtendObjectRetention and
	// ListObjects reach the versioned objects of a backup location's block
	// folder store, by fixed kind (§4.15). On a namespace's own store they are
	// ErrInvalid. GetObject with an empty v reads the current version, and only
	// on a mutable location.
	PutObject(ctx context.Context, k ObjectKey, body io.Reader, size int64, sum []byte, retainUntil time.Time) (Version, error)
	GetObject(ctx context.Context, k ObjectKey, v Version) (io.ReadCloser, error)
	ObjectRetention(ctx context.Context, k ObjectKey, v Version) (time.Time, error)
	ExtendObjectRetention(ctx context.Context, k ObjectKey, v Version, until time.Time) error
	// DeleteObjects removes versioned objects at a mutable location, one result
	// per key, as Delete does for blocks; at an immutable one, ErrInvalid.
	DeleteObjects(ctx context.Context, keys []ObjectKey) []error
	// ListObjects yields every stored version of every object of kind, for
	// this folder's namespace, in ascending key order and, within a key, in
	// the service's version order, starting after the given key.
	ListObjects(ctx context.Context, kind ObjectKind, after ObjectKey) iter.Seq2[ObjectInfo, error]

	// Close releases the client. Stores are shared across shares; only the
	// holder that opened one closes it.
	Close() error
}

// Errors: every failure wraps exactly one of these (§4.8). ErrDenied carries
// a Cause, so a caller can tell a quota from a revoked permission.
var (
	ErrNotFound  = errors.New("remote: block not found")
	ErrInvalid   = errors.New("remote: invalid request")
	ErrDenied    = errors.New("remote: denied")
	ErrTransient = errors.New("remote: transient")
	ErrCorrupt   = errors.New("remote: corrupt in transit")
	// ErrThrottled wraps ErrTransient: the service asked to be sent less.
	ErrThrottled = fmt.Errorf("remote: throttled: %w", ErrTransient)
)
```

A backend is opened by its own constructor, which takes its settings, its mode
([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)), the client it shares ([§4.10](#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers)) and a context, and runs the
capability check ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)) before it returns. On a mutable store, `Delete` and
`DeleteVersions` do what [§4.5](#4.5%20Delete%20is%20batched%20and%20idempotent) says; on an immutable one they return
`ErrInvalid` for every name without sending a request.

```go
// Cause says why a store refused (ErrDenied), so a caller can choose between
// waiting and failing a write as out of space (RFC 8 §10.2).
type Cause uint8

const (
	CauseAccess Cause = iota + 1 // credentials, permissions, a missing bucket, a wrong endpoint
	CauseQuota                   // a quota or storage limit of the service is reached
)

// DeniedError is the error behind every ErrDenied.
type DeniedError struct{ Cause Cause; Code string }
func (e *DeniedError) Unwrap() error { return ErrDenied }
```

### 4.2 Names in, locations kept inside

Callers name blocks; they never see where a block lives. The store maps a name to
its own location deterministically: a key, a path, whatever the service
addresses by; each backend profile states its mapping ([§5](#5.%20Backend%20profiles)). The location is
opaque above the store, and nothing records it, because the name and the store's
configuration always recompute it.

A store **MUST NOT** invent names, and **MUST NOT** store anything under its
configured namespace other than blocks, its control objects ([§4.13](#4.13%20Control%20objects)), a backup
location's versioned objects ([§4.15](#4.15%20Versioned%20objects%20at%20a%20backup%20location)) and its capability-check objects. Past the store's fixed namespace prefix, a location
**MUST NOT** put anything — a date, a share, a counter — ahead of the name: names are uniform, and a
prefix would concentrate keys on one partition of the service.
Two stores **MUST NOT** share a namespace, since each would list the other's
blocks as its own ([RFC 9 §5.3](rfc-9-gc.md#5.3%20It%20runs%20only%20where%20the%20namespace%20is%20proven)).

**A store serves exactly one namespace.** Its prefix is that namespace's folder
([RFC 12 §2.1](rfc-12-snapshots.md#2.1%20A%20namespace%20is%20the%20unit%20that%20moves)). A **store configuration** — endpoint, credential, bucket,
chain and material provider ([RFC 5 §3.2](rfc-5-transforms.md#3.2%20Configuration)) — is what several namespaces share: each
namespace created on it opens its own store under its own prefix, and every
store opened from one configuration shares that configuration's client
([§4.10](#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers)). "Store" in this set means the per-namespace store.

### 4.3 Put: a whole block, checksummed, stored on success

- **A put stores a whole block.** There is no partial or appending put, and a put
  that does not complete **MUST NOT** leave the block readable ([RFC 3 §3.4](rfc-3-syncer.md#3.4%20One%20put%20per%20block)).
- **The body is a stream of known length.** The caller hands over a reader and
  the block's exact size, header first, then each chunk encoded as it is read
  ([§3.2](#3.2%20Layout)). No one holds the whole block: not the caller, not the store, and
  not a file on disk. The store reads the stream once; a retry is the syncer's,
  which streams the block again from the journal ([RFC 3 §2.4](rfc-3-syncer.md#2.4%20Every%20transfer%20terminates%2C%20and%20reports)).
- **The size, and a checksum when needed, come before the first byte.** Each
  transform declares the exact length it will produce for an input, when it can
  know it without encoding ([RFC 5 §3.1](rfc-5-transforms.md#3.1%20Interfaces)). When every transform in the chain can,
  and the store needs no up-front checksum, the lengths come from those
  declarations and the block is encoded once, while it is sent. Otherwise the
  engine runs a **measuring pass** first: it encodes each chunk, keeps only its
  length and folds it into the checksum, and discards the bytes; then it encodes
  again while sending. The two passes produce the same bytes, because encoding is
  deterministic ([RFC 5 §2.1](rfc-5-transforms.md#2.1%20A%20transform%20acts%20on%20one%20chunk)). The checksum covers the header, which is built only
  once every length is known: a checksum that can be combined from parts
  (CRC32C) is taken per body and joined to the header's, and one that cannot
  (MD5) needs a second measuring pass when the lengths also had to be measured.
- **A put is verified in transit.** Either the service checks the body against a
  checksum the store sends and refuses a mismatch, or the service reports a digest
  of what it stored and the store compares it with the one it computed while
  streaming. Either way a mismatch returns `ErrCorrupt`, and the syncer's retry
  puts the same bytes again. Without this, bytes corrupted in transit are found
  only at a later read, possibly after the journal released its copy. Which
  mechanism a service supports is the backend profile's to find out ([§5](#5.%20Backend%20profiles)); a
  service offering neither is refused.
- **Success means stored.** `nil` from `Put` means the block survives the loss
  of this machine, on every store. A service that acknowledges before that point
  is not admitted, and there is no setting that marks a store as keeping blocks
  less safely.
- **One name is one byte sequence.** A name is minted for one put attempt and
  never again ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)); before its first put the writer durably records a put
  intent for it ([RFC 6](rfc-6-block-metadata.md), [RFC 9](rfc-9-gc.md)). Every put of the name is that attempt's, with
  the same plan and the same bytes, so a put under an existing name arises only
  from the attempt's own retry after an unknown outcome
  ([RFC 3 §2.5](rfc-3-syncer.md#2.5%20An%20unknown%20outcome%20is%20not%20a%20success)), and rewrites identical bytes. There is no whole-block "already
  recorded, skip the put": dedup happens per chunk, before a block is planned
  ([RFC 8](rfc-8-engine.md)). The rule holds per store namespace, since two stores never share
  one ([§4.2](#4.2%20Names%20in%2C%20locations%20kept%20inside)). A backup's block folder is a store namespace of its own, whose
  names are put only by copies of one block, byte for byte as its source
  namespace stored it ([RFC 12 §3.4.1](rfc-12-snapshots.md#3.4.1%20Layout%20at%20the%20location)): one name is still one byte sequence
  there, though no attempt minted it in that namespace. At an immutable
  location the guarantee readers rely on is narrower and holds against any
  writer: a recorded version is one byte sequence ([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)).

  > decision: a retry *intends* identical bytes, but bytes corrupted in transit
  > are caught before storage only by a service that checks the put's checksum.
  > The block is committed only after a put of it succeeded, so a corrupt retry
  > caught by the digest never reaches a committed block. What remains is a
  > **delayed duplicate** of the same attempt's put — a request the client gave
  > up on that the service applies after a later retry succeeded and the block
  > was committed — carrying a transit error: on a digest-only service nothing
  > compares its digest, and reads report the chunk corrupt. Withdraw this
  > exemption, by refusing digest-only services or making a service-checked put
  > mandatory, if a digest-only service is ever observed applying a stale put.

No rule of this contract depends on a conditional put (put-if-absent), and a
store **MUST NOT** issue one: services that claim the same protocol differ on it
([Appendix B](#Appendix%20B%20%E2%80%94%20measurements)), and a store that relied on it would be correct only on some of them.
Doing without it rests on names minted once per attempt: no two attempts put one
name, and GC deletes an object only once no block record not yet `deleted` and no
put intent names it — a final state, since the name is never minted again — so no put can
re-create a block underneath its delete ([RFC 9](rfc-9-gc.md)). The capability check still
provokes it and reports whether the service honours it
([§4.12](#4.12%20What%20a%20store%20makes%20observable), `conditional_put`), so a deployment knows which of its services could
carry a rule that needs one.

> decision: conditional put is measured and reported, never used. The one
> object that would gain from it is the namespace claim, which stays a check
> fenced by clocks ([RFC 12 §4.1](rfc-12-snapshots.md#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)). Add a conditional control put, used only
> where the check proved it, if a deployment runs two installations on one bucket
> whose clocks cannot be held within the drift bound.

![A put streams: body lengths come from each transform's declared length, or from a measuring pass that encodes and discards; the header goes first, then each chunk encoded as it is sent, in one request with the profile's integrity check](img/rfc4r-put-path.svg)

### 4.4 Get: a whole block or one range, exactly

A get returns exactly the bytes asked for, or an error. There is no third outcome:

- a range that extends past the end of the block **MUST** fail with `ErrInvalid`,
  not return fewer bytes. Some services clamp such a range silently ([Appendix B](#Appendix%20B%20%E2%80%94%20measurements)), so the store
  **MUST** check the range the service says it sent against the range it asked
  for; a smaller one is a clamp, and `ErrInvalid`;
- a body shorter than the range the service says it sent, where that range
  matches the request, ended early in transit and **MUST** fail with
  `ErrTransient`;
- an absent block **MUST** fail with `ErrNotFound`;
- a get issued after a put of that name returned `nil` **MUST** return the
  block, from any client of the service, never `ErrNotFound`. The journal
  releases its copy once the put is recorded, so the next read of that content
  is a get; on a service whose reads lag its writes, that read would report a
  stored chunk absent. Such a service is not admitted ([Appendix C.1](#C.1%20Required%20service%20features)).
  Listings may lag ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)); gets may not.

The store cannot tell a caller's mistake from a record that outlived its block;
the codec can, and reports a recorded range the store rejects as corrupt
([§3.4](#3.4%20Every%20read%20is%20verified%20by%20the%20codec)).

The caller merges ranges that are adjacent in the block into one request, which
is why a get takes one range and not a list. Where the service returns a checksum
for what it sent, the store **SHOULD** check it. The codec's hash check ([§3.4](#3.4%20Every%20read%20is%20verified%20by%20the%20codec))
remains the one that decides.

### 4.5 Delete is batched and idempotent

`Delete` takes many names in one call, because GC deletes blocks by the
thousand and a request per block would make the request count, not the service,
the limit: at the one second per request measured on one service ([Appendix B](#Appendix%20B%20%E2%80%94%20measurements)),
a million blocks is a million seconds of requests one at a time and a thousand
requests in batches of a thousand. A single delete is a batch of one.

- **A late delete reaches nothing committed.** GC deletes a name only once no
  block record and no put intent names it, and a name is never minted again
  ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)). A delete that lands long after it was sent — a request retried by the
  network, a batch replayed after a crash — can therefore only remove an object
  that no record can ever name. The hole is closed by construction, with no
  fence and no delay.
- **Each name succeeds or fails on its own.** A batch is not atomic, and the
  store returns one result per name. The caller clears whatever records it keeps
  for a name ([RFC 9 §3.2](rfc-9-gc.md#3.2%20A%20retirement%20not%20yet%20deleted%20is%20durably%20recorded)) only for names whose result is `nil`.
- **A result is read from the reply, never inferred from its silence.** A store
  whose service offers a quiet mode, which reports only failures, **MUST NOT**
  use it: every requested name **MUST** appear in the reply exactly once, as
  deleted or as an error, and a name missing from the reply, or reported twice,
  is `ErrTransient`. A truncated or quiet reply would otherwise read as success
  for every name, and GC would prune records whose objects remain.
- **Deleting an absent block succeeds.** GC retries a delete after an unknown
  outcome and re-runs a pass after a crash; a delete that failed because it had
  already happened would turn recovery into an error.
- **A batch whose outcome is unknown fails every name in it** with
  `ErrTransient`. The retry is safe, since each delete is idempotent.
- **The store splits a batch** into as many service requests as its service's
  limit requires ([Appendix C.1](#C.1%20Required%20service%20features)), and a service without a batch operation deletes
  one name per request. The caller never needs to know the limit.
- **`DeleteVersions` removes every stored version of each name**, and any delete
  marker, where the service keeps versions. GC calls it only after a `Recheck`
  finds versioning drifted on, for the names deleted since the last good one
  ([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)); on a service that never versioned it is `Delete`.

### 4.6 List is a complete, resumable walk

GC finds most blocks that no record names from their put intents: a put whose
commit never landed leaves an intent behind ([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)). A put landing after its
intent was removed leaves an object that neither names, and only the store can
see those, so the contract includes a listing as a rare backstop. GC decides what a listed block means, and when it may be deleted
([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)). No correctness depends on a listing: a block it misses is leaked
space, not lost data.

A listing:

- **MUST** yield exactly once every block that exists for the whole walk. A block
  put or deleted during the walk may or may not appear;
- **MUST** yield blocks in ascending name order and accept a starting point
  (`after`), so a walk interrupted by a crash or a cancelled context resumes where
  it stopped instead of restarting over millions of blocks;
- **MUST** report each block's size and the time the service stored it;
- **MUST** stop when the caller breaks out of the loop or cancels `ctx`;
- **MUST** skip an entry under the block namespace whose key is not a block name,
  and count it ([§4.12](#4.12%20What%20a%20store%20makes%20observable)). It does not guess what such an entry is.

Paging is internal. The store fetches pages as the caller iterates and holds at
most one page in memory.

### 4.7 Health is one probe call

`Health` makes one bounded round trip, in one direction, that proves the store
can still do that half of its job. The **put probe** proves the service is
reachable, the credentials are accepted, and the namespace is writable; the
**get probe** proves it is reachable and readable.

**The get probe reads an object the store guarantees.** The health object is
written by the capability check at every open ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)), before the store
returns, and by every put probe; it is never deleted, including by the check's
cleanup. A get probe that finds it absent fails with `ErrNotFound`, which the
syncer counts as a failed get probe: something removed an object the store
owns. Neither probe depends on a block existing, so a store with no blocks, or
one whose puts are covered by successful uploads instead of probes
([§8](#8.%20Decisions%20and%20open%20questions), item 6), still probes both directions. An immutable store
([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) sends no put probe: `Health(DirPut)` returns `ErrInvalid` without a
request, and the syncer registers such a store with no put probe, judging its
put direction by its transfers alone ([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)). A probe that only proves the
service answers can report healthy while every put fails. Each probe runs
against the store's **own namespace** — its health control object ([§4.13](#4.13%20Control%20objects))
under the store's own prefix — never against a location outside it, with one
exception: a backup location's block folders share **one location-level health
object**, at the location's root ([Appendix C.2](#C.2%20Layout%20in%20the%20bucket)). It is put when the location
record is created ([RFC 13 §2.5](rfc-13-configuration.md#2.5%20A%20backup%20location%20is%20its%20own%20record)), before any namespace copies there, and the
record stores what identifies it: its version at an immutable location, and at a
mutable one a random nonce written in its body, which every later put keeps.
That identity is how a changed endpoint or credential is proven to reach the
same location ([RFC 13 §5.2](rfc-13-configuration.md#5.2%20Reaching%20the%20same%20content%20another%20way%20is%20not%20a%20change)). At an immutable location every open of a
folder, and every `Recheck`, extends that version's retention ([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)), and the
get probe reads that version. A folder's prefix exists only once a namespace has
copied there, so a health object under it could not be read by the first open
that needs it. Which
stores' probes the syncer actually sends is the syncer's choice: stores it
registers under one `ProbeKey` share one probe per direction for detecting an
outage, and a permission that differs by prefix is caught by its failure window
instead ([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)). The backend profile names the calls
([§5](#5.%20Backend%20profiles)).

The store returns the probe's outcome and nothing else. It keeps no health
status. The syncer probes each direction on an interval, derives each
direction's health from recent outcomes, and refuses work only in an unhealthy
direction ([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)): a store that cannot be written can still be read.

### 4.8 Errors are a closed set

Every failure **MUST** wrap exactly one error of [§4.1](#4.1%20Interface) — `ErrThrottled` counting as
the one it is, though it also matches `ErrTransient` — and a caller **MUST
NOT** need to interpret a native error. A caller that has to recognise a
service's status codes contains a second store implementation, and gets wrong
whichever service it was not written against.

**A refusal says why.** Every `ErrDenied` carries a cause, `access` or `quota`,
which the backend profile derives from the service's reply. The two need
different answers above the store — a quota is the store being full, and the
engine answers writes as out of space; an access refusal is an outage the
operator fixes ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)) — and a quota reply mapped anywhere else, as an
invalid request say, would never count toward health at all. A profile **MUST**
list the replies its service uses for a reached quota or a full store, and map
each to `ErrDenied` of cause `quota`.

| Error | Meaning | What the caller does |
| --- | --- | --- |
| `ErrNotFound` | this block is absent; the store itself is fine | fails the read; the engine re-resolves the chunk ([RFC 8 §7.7](rfc-8-engine.md#7.7%20An%20absent%20object%20is%20re-resolved%20while%20its%20location%20moves)) |
| `ErrInvalid` | the request was wrong: a range past the end, or an operation the service does not support | fails the call; not retried |
| `ErrDenied` | the store refused, or does not exist: credentials, permissions, a missing bucket (cause `access`); or a quota or storage limit of the service is reached (cause `quota`) | fails the call; the syncer's health rules decide what follows, and it reports the cause per direction ([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)) |
| `ErrTransient` | retrying may help: network, timeout, a server error, a short body | the syncer retries within its bound ([RFC 3 §2.4](rfc-3-syncer.md#2.4%20Every%20transfer%20terminates%2C%20and%20reports)) |
| `ErrThrottled` (wraps `ErrTransient`) | the service asked to be sent less: a throttling or slow-down reply | the syncer holds the flow and retries after backoff; it is backpressure and **MUST NOT** count toward health ([RFC 3 §2.4](rfc-3-syncer.md#2.4%20Every%20transfer%20terminates%2C%20and%20reports)) |
| `ErrCorrupt` | a put's checksum or digest mismatched in transit, or the codec's verification of a chunk failed, including a recorded range the store rejected ([§3.4](#3.4%20Every%20read%20is%20verified%20by%20the%20codec)) | a put is retried; a chunk that failed verification is corrupt and is not retried |

A local failure (the context cancelled, the body's reader failing) is returned
as itself. It is not the service's fault and does not count against the store's
health. A mismatching checksum the service returns with a get is
`ErrTransient`: the codec's hash check is the one that decides ([§4.4](#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly)). The
syncer uses this set as it is, plus one error for material ([RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface)).

### 4.9 No state across calls

A store **MUST NOT** carry a retry loop, a circuit breaker, a health status, a
concurrency limit of its own or a queue. Each request makes one attempt; only a
batch delete makes more than one request per call ([§4.5](#4.5%20Delete%20is%20batched%20and%20idempotent)), and those run one
after another.

State below the syncer is invisible to it: a retry loop there multiplies with the
syncer's budget, a health flag can latch unobserved, a queue holds requests the
syncer cannot schedule or count. With one attempt per call, the syncer's bound ([RFC 3 §2.4](rfc-3-syncer.md#2.4%20Every%20transfer%20terminates%2C%20and%20reports)) is the only one.

### 4.10 The connection pool is derived from its callers

A store's client usually limits concurrent requests somewhere: connections per
host for HTTP/1.1, streams per connection for HTTP/2, an SDK's in-flight cap. A
request past that limit does not fail. It waits inside the client, where no
caller can see it. So:

- **one client per store configuration.** Every store opened from one store
  configuration ([§4.2](#4.2%20Names%20in%2C%20locations%20kept%20inside)) shares one client and its pool; a store is a prefix
  over it, and its `Close` releases its reference. A pool per store would
  multiply with namespaces — 10⁴ namespaces on one endpoint would open 10⁴ pools
  sized for the whole installation each — while the workers that use them are
  shared;
- the limit **MUST** be derived when the client is built from the concurrency
  of everything that calls stores on it: the syncer's
  `upload_workers + fetch_workers` ([RFC 3 §2.10](rfc-3-syncer.md#2.10%20Two%20settings%2C%20and%20everything%20else%20fixed)) — which also run a backup
  copier's versioned-object puts and gets, listings and retention extensions, as
  small-object transfers ([RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface)) — GC's delete and list bound
  ([RFC 9 §7.1](rfc-9-gc.md#7.1%20GC%20bounds%20its%20own%20work)), and one for the probe — once, not per store, since those pools are
  shared across the stores they serve. A store has no connection setting of its
  own. Set below its callers it is a hidden second limit; set above them it is
  never reached;
- any client-wide cap on connections **MUST** be at least the same value, or it
  silently lowers the per-host one;
- idle connections kept for reuse **SHOULD** be sized the same way, so a
  connection released by one request is reused by the next rather than reopened
  with a new handshake.

[Appendix B](#Appendix%20B%20%E2%80%94%20measurements) measured both on loopback. A cap below the callers cut throughput 4×. A
small idle pool reopened 37 times more connections than needed, which cost
nothing on loopback; its cost over a WAN is not yet measured, hence **SHOULD**.

### 4.11 A store checks its service before it opens

Services that claim the same protocol differ on exactly the features this
contract relies on ([Appendix B](#Appendix%20B%20%E2%80%94%20measurements)). This contract does not list those features or
how to test them: they are specific to each backend and live in its profile
([§5](#5.%20Backend%20profiles)). It requires only that:

- each backend **MUST** declare, in its profile, the service features it needs
  to meet [§4.2](#4.2%20Names%20in%2C%20locations%20kept%20inside)–[§4.10](#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers), and how each is verified;
- its constructor **MUST** verify them against the real service and refuse to
  open, naming every missing feature, if any is absent;
- a feature is verified by provoking what it exists to do and seeing it happen.
  A service accepting a request is not evidence that it honoured it;
- integrity and digest behaviour are verified with an object of the largest
  encoded block size the store will be asked to put, not only a small one:
  services change checksum, digest and server-side encryption behaviour with
  object size, and a digest is trusted as a content digest only where the check
  proved it at that size;
- the client's checksum mode is pinned explicitly in the profile, never left to
  a client library's default, which can change between releases;
- the namespace claim ([§4.13](#4.13%20Control%20objects)) is read, and handed to the opener, which applies
  [RFC 12 §4.1](rfc-12-snapshots.md#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)'s claim rule; a claim that exists but cannot be read fails the
  open, while an absent one is reported as absent;
- where a service's listings lag its writes, a check that lists what it just
  wrote **MAY** re-list a bounded number of times, over a bounded time the
  profile states, before refusing to open;
- every object the store puts is written in one storage class the profile pins
  explicitly, never the service's default, and the check reads back the class
  the service reports for what it wrote. A class or a tiering rule that can move
  an object out of immediate reach — an archive class, or an automatic tier that
  archives objects nobody reads for a while — **MUST** refuse the open: a get of
  an archived block fails until a restore the store never requests, and blocks
  that are rarely read are exactly the ones such a rule moves.

The check runs whenever a store is opened, since an endpoint can be upgraded or
repointed between runs. It cleans up what it wrote, except the health object
([§4.7](#4.7%20Health%20is%20one%20probe%20call)); a failed cleanup is logged, not fatal. An immutable store adds
no version at open and cleans up nothing ([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)).

**Settings that drift are re-read, on one cadence, with one effect.** Some
service settings can change after open without the store noticing: versioning,
object lock, lifecycle and tiering rules, the storage class. `Recheck` re-reads
exactly those, and the claim. It has one caller per store, on one period:

- for a namespace's own store, GC, on its recheck period, independent of any
  other GC work ([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)); a GC pass does not re-read settings of its own;
- for a backup location's block folder store, the holder of the namespace's
  folder record ([RFC 12 §3.4.1](rfc-12-snapshots.md#3.4.1%20Layout%20at%20the%20location)), on GC's recheck period and once more before each
  copy and each sweep starts. Nothing else touches that store, so without this
  caller a folder whose lock was lowered, or whose versioning was suspended,
  would be written and swept as if nothing had changed.

Nothing else calls it. On drift `Recheck` returns the drifted setting, and its
caller does two things before its next call to the store:

- it issues no delete, puts nothing at a backup location, and prunes nothing for the store;
- it raises the store's **drift condition**, which the syncer reads as part of
  the store's health ([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)): while the condition stands, the put
  direction is unhealthy and the syncer refuses puts to the store. Gets go on,
  since drift endangers what is written and deleted, not what is read.

A later `Recheck` that passes clears the condition. A folder store's holder that
finds drift fails the copy or sweep it was about to start, which then follows
[RFC 12 §3.4.3](rfc-12-snapshots.md#3.4.3%20Writing%20one%2C%20step%20by%20step)'s failure path. The store itself keeps
neither the result nor the condition ([§4.9](#4.9%20No%20state%20across%20calls)). Each profile names the settings
its `Recheck` covers.

**A suspended setting can be recovered, and the proof is kept.** Versioning,
once turned on, can on some services only be suspended, never removed, and a
suspended store still keeps the versions written while it was on. After GC's
version repair ([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)) a listing of stored versions under the prefix
finds only current ones; GC then writes that proof as the control object of
role `repair` ([§4.13](#4.13%20Control%20objects)): the time of the listing and the status it read.
`Recheck` **MUST** accept a suspended store whose repair object exists and
**MUST NOT** list versions again for it. It lists versions only when versioning
reads suspended with no repair object, or with an empty one. A status read as
enabled is drift; on it GC overwrites the repair object with an empty one, and
writes a proof again only after its next repair.
Without the proof, every `Recheck` of a store that drifted once would walk every
version under its prefix, forever.

> decision: a suspended store's proof is trusted until a `Recheck` sees
> versioning enabled again. Versioning turned on and suspended again entirely
> between two rechecks goes unseen, and deletes in that window leave old
> versions that leak space; no block is lost, since a delete is the only write
> that creates one. Re-list on a period if such a window is ever observed.

### 4.12 What a store makes observable

Every store exports the same metrics, labelled by store. A backend adds none of
its own, so a dashboard works for every backend. The codec's metrics are
exported by the codec, whichever store it reads from. Each event is counted in one place:
a verification is counted here, once, and neither the syncer nor the transforms
count it again ([RFC 3 §2.12](rfc-3-syncer.md#2.12%20What%20the%20syncer%20makes%20observable),
[RFC 5 §6](rfc-5-transforms.md#6.%20Observability)).

| Answers | Metric | Type |
| --- | --- | --- |
| calls, labelled `op` (`put`, `get`, `delete`, `list`, `health`) and `result` (`ok` or the error of [§4.8](#4.8%20Errors%20are%20a%20closed%20set)) | `remote_requests_total` | counter |
| time per call, by `op` | `remote_request_seconds` | histogram |
| bytes moved, by `op` | `remote_bytes_total` | counter |
| connections opened; against requests, how well the pool reuses them ([§4.10](#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers)) | `remote_connections_opened_total` | counter |
| entries under the block namespace that are not blocks ([§4.6](#4.6%20List%20is%20a%20complete%2C%20resumable%20walk)) | `remote_list_foreign_total` | counter |
| the last capability check, per feature: 1 if present ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)) | `remote_capability` | gauge |
| chunks the codec returned verified | `codec_chunks_verified_total` | counter |
| blocks encoded with a measuring pass ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)); against blocks encoded, how often a put encodes twice | `codec_measuring_passes_total` | counter |
| chunks that failed verification, labelled `kind` = `hash`, `name`, `version`, `malformed`. Any of these is an alert | `codec_verify_failures_total` | counter |
| encode and decode time per block, by direction | `codec_seconds` | histogram |

`op` also takes `recheck`, `put_control`, `get_control`, `put_version`,
`get_version`, `retention`, `extend_retention`, `put_object`, `get_object`,
`list_objects` and `delete_objects`, and an `ErrDenied` result is labelled with
its cause; a drifted `Recheck`
sets the drifted setting's `remote_capability` to 0. The gauge also
carries `conditional_put`, whether the service honoured one at the last check
([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)); it is reported, and no rule reads it.

Logs: a refused open, and a drifted `Recheck`, log every missing feature at
`Error`, once. `ErrCorrupt`,
a verification failure and a skipped foreign entry log at `Warn` with the block
name. Every other per-call outcome is a metric, not a log line, since the syncer
already logs what it does about it.

### 4.13 Control objects

A few small objects are not blocks but belong to the store's namespace: the
namespace claim ([RFC 12 §4.1](rfc-12-snapshots.md#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)), the health probe's object ([§4.7](#4.7%20Health%20is%20one%20probe%20call)), the
versioning repair proof ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)), and the **keys** object: every key record of
the namespace, wrapped as the metadata store holds it, each written there before
it becomes current, so a recovery that has lost the metadata store finds keys
rotated since the last backup; in a non-encrypting namespace it also holds the
chunk-ID key in the clear, so that its blocks stay verifiable from the bucket
alone ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)). They
go through `PutControl` and `GetControl`, by a fixed **role**:

- a role maps to one fixed location in the profile, outside the block
  namespace, so a listing never yields a control object as a block ([§4.6](#4.6%20List%20is%20a%20complete%2C%20resumable%20walk));
- a control put replaces the whole object and is verified in transit like a
  block put ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)); a control get returns the whole object;
- the error set is [§4.8](#4.8%20Errors%20are%20a%20closed%20set)'s: an absent control object is `ErrNotFound`;
- a control object is small, bounded by a constant of the profile, and read
  into memory, not streamed;
- no conditional put and no locking: the claim is a check, not a lock
  ([RFC 12 §4.1](rfc-12-snapshots.md#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)).

Roles are a closed set in code. A new role is a new constant and a new location,
not a caller-chosen key, so control objects cannot become a second, unlisted
block store.

### 4.14 A backup location opens in one of two modes

Every namespace's own store is **mutable**: no versioning, no object lock, no
expiring rule, and deletes allowed (the refusals of [§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens) and
[Appendix C.1](#C.1%20Required%20service%20features)). A backup location's block folder ([RFC 12 §3.4.1](rfc-12-snapshots.md#3.4.1%20Layout%20at%20the%20location)) is opened
in the **mode** its location's configuration record states — `mutable` or
`immutable` — together with the credential reference and the lifecycle age that
record holds ([RFC 12 §6.2](rfc-12-snapshots.md#6.2%20Configuration)). The mode is never inferred from what the bucket
happens to have: an immutable location whose bucket lacks a setting fails to
open, and a mutable one whose bucket has one fails too.

A mutable location's folder is a mutable store like any other. An **immutable**
one:

- **requires versioning and compliance-mode object lock, with no default
  retention.** Every put names its own retain-until, in compliance mode
  (`PutVersion`, `PutObject`). Versioning without the lock is refused: a
  credential that can put could then supersede every object, and nothing would
  stop a delete of the old versions. A governance-mode lock is refused for the
  same reason: a credential with the bypass right removes it. A default
  retention is refused too: set to the longest retention of any policy, it would
  lock every block of an hourly policy for as long as a yearly one keeps its
  backups;
- **requires a cap on the retention a put may set**, enforced by the service
  for the location's credential — on S3, a bucket policy denying any put whose
  remaining retention exceeds the location record's retention cap
  ([Appendix C.1](#C.1%20Required%20service%20features)) — read at every open and provoked once per location record. The
  cap is the longest retention any policy writing there may set, plus one
  extension generation and `backups.max_copy_time` ([RFC 12 §3.4.4](rfc-12-snapshots.md#3.4.4%20Expiry%20and%20the%20sweep)). Without it, a
  stolen put credential could lock objects in compliance mode for a century,
  which no one can undo and the bucket's owner pays for;
- **requires the bucket's configuration out of the credential's reach.** The
  location's credential **MUST** be denied changing the bucket's policy, object
  lock configuration, lifecycle rules and versioning — on S3, `PutBucketPolicy`,
  `DeleteBucketPolicy`, `PutBucketObjectLockConfiguration`,
  `PutLifecycleConfiguration` and `PutBucketVersioning`. A cap the credential can
  lift limits nothing: a thief removes it, then locks junk for a century. The
  check reads the denies at every open and, once per location record, provokes
  each put action with the configuration it just read, which changes nothing if
  it lands, and requires every one refused;
- **requires a noncurrent-version expiry rule and a current-version expiry
  rule**, and refuses a transition to an archive class. The current-version
  rule's age **MUST** be at least the location record's lifecycle age. Block
  names are put once and stay current, so without the current-version rule no
  block is ever freed; without the noncurrent rule, every state-object update
  and every re-put of a name keeps one more locked version for good. Lifecycle
  rules, versioning and tiering are checked over the **location's whole root** —
  the block folders, `exports/`, `progress/`, `keys/` and the health object
  ([Appendix C.2](#C.2%20Layout%20in%20the%20bucket)) — not only under one folder's prefix, since state, export,
  key and progress versions must expire too;
- **has no delete.** `Delete` and `DeleteVersions` are `ErrInvalid` without a
  request, and the credential **MUST NOT** be able to delete: the capability
  check provokes a delete of the health object naming no version — which on a
  versioned bucket only adds a delete marker, so only a missing delete right
  refuses it — and requires it refused. A credential that can delete fails the
  open, naming it. A delete of a recorded version is not sent: the lock refuses
  it whatever the credential holds, so it proves the lock, not the credential;
- **reads and writes by version.** `PutVersion` returns the version the service
  stored, which the caller records ([RFC 12 §3.4.2](rfc-12-snapshots.md#3.4.2%20What%20is%20copied)); `GetVersion` reads that
  version and no other. A put by anyone, under any name, then only adds a
  version: it cannot replace what a recorded version holds;
- **extends instead of re-putting, in generations.** `ExtendRetention` reads a
  version's retain-until (`Retention`) and raises it only when the new value is
  later; the service refuses to lower it in compliance mode, and the store never
  asks it to, so an extension that would shorten succeeds as a no-op. A copy
  that reuses a stored version extends it rather than putting the name again.
  Every retain-until the caller sets **MUST** be the end of an extension
  **generation** — a fixed period `G` of the location record — at or after the
  time it needs: so a version still in use is extended at most once per
  generation, and a location holding `N` live versions makes about `N ÷ G`
  extension calls a day, not one per version per backup. **Each name has its own
  phase**: its generations end at the multiples of `G` offset by
  `hash(name) mod G`, so the versions reused on one day fall due spread across
  the generation rather than in one burst at its end — at 1 PiB of 4 MiB blocks,
  a burst would be about 2.7 × 10⁸ extensions in one copy. Which versions need
  extending, and when, is the copier's ([RFC 12 §3.4.4](rfc-12-snapshots.md#3.4.4%20Expiry%20and%20the%20sweep));
- **sends no put probe.** Each probe would add a locked version. `Health(DirPut)`
  is `ErrInvalid` without a request, and the syncer registers the store with no
  put probe, judging the put direction by the copies' own puts
  ([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)). Its get probe reads the health object's recorded version
  ([§4.7](#4.7%20Health%20is%20one%20probe%20call));
- **keeps its health object alive, within the cap.** Every open and every
  `Recheck` extends the location health object's recorded version ([§4.7](#4.7%20Health%20is%20one%20probe%20call)) to
  the **last end of one of its generations at or before now plus the retention
  cap**. That is never more than the cap, so the bucket policy never refuses it,
  and at least the cap less one generation ahead, so a process that stays up
  keeps it alive through its `Recheck`s as one that restarts does through its
  opens. This is the one target; [RFC 12](rfc-12-snapshots.md#3.4.4%20Expiry%20and%20the%20sweep) defers to it. An extension adds
  no version, so neither adds one. Without it the health object's lock would
  end, the lifecycle rules would remove its version, and the location would stop
  opening;
- **runs its put-integrity check once.** Check objects it writes cannot be
  deleted, so the integrity step of the capability check runs when the location
  record is created and whenever its endpoint or credential changes, and its
  outcome is stored in that record. Every open still reads the bucket's
  settings and provokes the refused deletes, which write nothing.

`Recheck` covers the same settings, over the location's whole root: versioning
suspended, the lock's mode changed, a default retention added, the retention cap
or a configuration deny removed or the cap raised past the record's, either
expiry rule removed or the current rule's age lowered is drift
([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)).

**The cap is provoked once per location record**, when the record is created and
whenever its store or credential changes, with the integrity step: a put whose
retain-until is one second above the cap, which **MUST** be refused. On a bucket
whose cap is missing it lands and locks one small object for the cap's length
and a second, which the record's own retention already allows. Every open then
reads the policy, which proves the cap is still there.

> decision: a service whose cap or denies the credential cannot read — held in
> an identity policy outside the bucket, say — cannot be verified at every open,
> and `DeleteBucketPolicy`, which cannot be provoked without effect, not at all.
> Such a service is refused as an immutable location rather than trusted on the
> provoked refusals alone. Accept it once its profile can read where the denies
> are held.

> decision: an immutable location defends against a stolen installation
> credential and an installation that misbehaves, not against the bucket's owner,
> who can still delete the bucket once its locks expire. A location whose bucket
> owner is the threat needs a second account or provider holding the bucket; that
> is the operator's choice of location, and this contract cannot tell.

### 4.15 Versioned objects at a backup location

Besides blocks, a backup location holds, per backup, an **export**, a **state
object** and the **progress objects** of a running copy ([RFC 12 §3.4.1](rfc-12-snapshots.md#3.4.1%20Layout%20at%20the%20location)).
They are not blocks, and not control objects either: there are many, one per
backup, and at an immutable location every write of one is a version that must
stay readable by version. So the block folder store reaches them as **versioned
objects**, by fixed kind (`ObjExport`, `ObjState`, `ObjProgress`, and
`ObjKeys`, the namespace's key records ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)), one per namespace)
and backup ID, through `PutObject`, `GetObject`, `ObjectRetention`,
`ExtendObjectRetention` and `ListObjects` ([§4.1](#4.1%20Interface)):

- a kind maps to one fixed location in the profile, under the location's root
  and outside the block namespace, so a block listing never yields one
  ([§4.6](#4.6%20List%20is%20a%20complete%2C%20resumable%20walk)); kinds are a closed set in code, like roles;
- a put is whole, of known size, and verified in transit like a block put
  ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)); a get returns the whole object;
- at an immutable location each put adds a version and returns it, carries its
  own compliance-mode retain-until under the location's cap, and is read back by
  version only; retention is read and extended exactly as for blocks
  ([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)). At a mutable one a put replaces the object, versions are empty,
  and `DeleteObjects` removes one;
- **`ListObjects` lists versions, not current objects.** It yields every stored
  version of every object of a kind for the folder's namespace, with its version,
  size, store time and retain-until, resumably and in a fixed order. A lifecycle
  rule puts a delete marker over a current version that has aged, and a writer
  holding the put credential can add a newer version at will; a restore that
  read only current objects would find no last good backup in the first case
  and a forged one in the second. The store does not decide which version is
  good: bodies are opaque to it, and the caller authenticates each version
  under the namespace's export key ([RFC 5 Appendix B.1](rfc-5-transforms.md#B.1%20How%20a%20chunk%20is%20encrypted)) and takes the
  authentic one with the **highest authenticated sequence** — never the newest
  by store order or time, which anyone holding the put credential can change by
  re-putting an older authentic version ([RFC 12 §3.3](rfc-12-snapshots.md#3.3%20Restore)).

Every call here is one attempt, like every other store call ([§4.9](#4.9%20No%20state%20across%20calls)); the
backup copier reaches them through the syncer as small-object transfers, which
retry, back off and count against the client's limit like blocks
([RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface), [§4.10](#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers)).

## 5. Backend profiles

Each backend has a profile: the service features it requires, how it maps the
contract onto its service, and the steps of its capability check ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)). A
profile is an appendix, so this contract stays independent of any one service.
S3-compatible object storage, the primary backend, is profiled in
[Appendix C](#Appendix%20C%20%E2%80%94%20the%20S3-compatible%20block%20store).

## 6. Invariants

| # | Invariant |
| --- | --- |
| R1 | A block's structure is readable from its own bytes: marker, version, name, nonce, chain ID, seal and index are in its header, and its header and bodies are bounded before anything is allocated for them. Its name can be recomputed from its header and the namespace's key scope, with no material; in an encrypting namespace its bodies' ranges are read only with the header key its seal names, and its content can be verified only with the namespace's material. |
| R2 | Every chunk returned by the codec was verified against its plaintext hash. |
| R3 | Decode inverts encode exactly; identity is over plaintext; the chain is recorded in the block. |
| R4 | The store never parses a block, and nothing above the codec observes a transform. |
| R5 | A put is whole, streamed at a length known before its first byte, checksummed, and `nil` only once stored. |
| R6 | A get returns exactly the requested bytes or an error, and a get after a successful put of the name returns the block. |
| R7 | Delete is idempotent per name, and reports per name. |
| R8 | A listing yields every block once, in order, resumably. |
| R9 | Every failure wraps one error of the closed set. |
| R10 | A store holds no state across calls and makes one attempt per call. |
| R11 | A client's limits are derived from the worker pools. |
| R12 | A store does not open against a service missing a required feature, or with a storage class or tiering rule that can archive what it writes. A mutable store does not open on a versioned or object-locked bucket, or under an expiring lifecycle rule; an immutable store opens only with versioning, a compliance-mode lock with no default retention, a service-enforced cap on the retention a put may set, a noncurrent-version expiry rule and a credential that cannot delete ([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)). |
| R13 | Within one store namespace, a name is put only by the attempt that minted it, always with the same bytes, so a recorded position never goes stale. In a backup's block folder, a name is put only by copies of the one block its source namespace stored under it; at an immutable location, every recorded version is one byte sequence whatever else is put under its name. |
| R14 | An encrypted namespace's blocks carry no plaintext chunk hash and no unsealed body offset or length. |
| R15 | No block is written in a format version a reader of its namespace does not support. |
| R16 | Control objects are reached by fixed role only and are never listed as blocks. |
| R17 | A drifted service setting, found by the store's one `Recheck` caller — GC for a namespace's store, the folder record's holder for a backup location's — stops that caller's deletes and puts and makes the store's put direction unhealthy until a re-check passes. A suspended store with a repair proof is not listed again. |
| R18 | An immutable store issues no delete and no put probe, reads by recorded version, sets a retain-until on every put, at a generation's end and under the location's cap, only ever raises one, and extends its health object's version at every open. |
| R19 | Stores opened from one store configuration share one client, whose limits are derived once from the callers' total concurrency. |
| R20 | A backup location's exports, state and progress objects are reached by fixed kind, put whole and verified, read by version at an immutable location, and listed by version, never by current object only. |
| R21 | Every `ErrDenied` carries its cause, and a service's quota or storage-full reply is `ErrDenied` of cause `quota`. |

## 7. Test plan and benchmarks

The set-wide test rules and tiers are in [the RFC index](rfc-index.md#Test%20tiers).

### 7.1 Conformance suite

One suite, run against every store implementation. Rows marked **T** need the
fault transport of [§7.2](#7.2%20Fault%20transport), and are skipped for a store without one (the
in-memory store).

| Section | Check | T |
| --- | --- | --- |
| [§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success) | Put, then get whole: bytes equal. | |
| [§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success) | A put cut off part-way through its stream: a get afterwards returns `ErrNotFound`, never a partial block. A stream shorter or longer than its declared size fails the put. | T |
| [§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success) | A put of a new name through a transport that flips one byte fails with `ErrCorrupt`. Where the service checks the put, a get afterwards returns `ErrNotFound`; where the store compares the digest, the corrupt block exists until the retry, and a retry without the fault makes a get return the original bytes. | T |
| [§4.4](#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly) | Get of each range inside the block returns exactly its bytes. A range starting past the end fails with `ErrInvalid`. | |
| [§4.4](#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly) | A transport that clamps a range ending past the end: the get fails with `ErrInvalid`, never returns fewer bytes. | T |
| [§4.4](#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly) | A transport that truncates a body under a `Content-Range` matching the request: the get fails with `ErrTransient`. | T |
| [§4.5](#4.5%20Delete%20is%20batched%20and%20idempotent) | Delete a batch twice, including a never-written name: every result is `nil`. A batch of 2,500 (above the service's limit): every block gone. | |
| [§4.5](#4.5%20Delete%20is%20batched%20and%20idempotent) | A transport that fails one key inside a batch: exactly that name's result is an error, the others `nil`. A batch whose response is dropped: every result is `ErrTransient`. | T |
| [§4.6](#4.6%20List%20is%20a%20complete%2C%20resumable%20walk) | Write 2,500 blocks; list with pages of 1,000; stop after 1,200 and resume from the last name: every block seen exactly once, in order, with size and time. | |
| [§4.6](#4.6%20List%20is%20a%20complete%2C%20resumable%20walk) | Put a key that is not a block name under the block namespace: the listing skips it and counts it. Cancel mid-listing: iteration ends with the context's error. | |
| [§4.2](#4.2%20Names%20in%2C%20locations%20kept%20inside) | After the whole suite, list the store's raw namespace: nothing but blocks, control objects and check objects, and every block key is the prefix followed directly by the name. | |
| [§4.13](#4.13%20Control%20objects) | Put and get each role: bytes equal, a second put replaces the first, an absent role is `ErrNotFound`, and a block listing yields none of them. | |
| [§4.13](#4.13%20Control%20objects) | A control put through a transport that flips one byte fails with `ErrCorrupt`. | T |
| [§4.8](#4.8%20Errors%20are%20a%20closed%20set) | For every failure the fault transport can produce, the error wraps exactly one sentinel and no service error type is reachable with `errors.As`. A cancelled context comes back as `context.Canceled`. | T |
| [§4.9](#4.9%20No%20state%20across%20calls) | One failing call makes exactly one request. | T |
| [§4.10](#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers) | Construct with syncer pools of 8 and 8 and a GC bound of 4: every client limit is at least 21. | |
| [§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens) | Against each fault profile of [§7.2](#7.2%20Fault%20transport), open fails and names the missing feature. A cleanup that fails does not fail the open. | T |
| [§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens) | A transport that corrupts or drops the integrity check only above a size threshold below the maximum block size: open fails. The check puts an object of the maximum encoded block size. | T |
| [§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens) | A transport whose listing omits new keys for the first few pages: open succeeds within the profile's re-list bound, and fails past it. | T |
| [§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens) | Open, then turn on versioning or add a lifecycle rule whose filter could match the prefix: `Recheck` fails naming it; turn it off: `Recheck` passes. | T |
| [§4.7](#4.7%20Health%20is%20one%20probe%20call) | The put probe fails while the namespace is read-only, and succeeds on the next call once write access returns; the get probe succeeds throughout. Revoke access to the store's own prefix only: both probes of that direction fail although a neighbouring prefix stays accessible. | |
| [§4.8](#4.8%20Errors%20are%20a%20closed%20set) | Answer every request with the service's throttling reply: each call fails with `ErrThrottled`, and `errors.Is(err, ErrTransient)` holds. | |
| [§4.4](#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly) | A transport that answers `404` to the first get after a successful put of the name: the get fails, and the capability check of a service that does so refuses the open. | T |
| [§4.8](#4.8%20Errors%20are%20a%20closed%20set) | Answer each request with every status from 100 to 599, with known and unknown error codes, and with a body that does not parse: each call wraps exactly one sentinel, and a response checksum mismatch on a get is `ErrTransient`, never `ErrCorrupt`. | T |
| [§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens) | A transport that reports an archive storage class for what the check wrote, or a tiering rule with an archive tier whose filter could match the prefix: open fails naming it, and `Recheck` fails when either appears later. | T |
| [§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens) | Turn versioning on after open: `Recheck` fails, GC issues no delete, and the syncer refuses puts while gets still succeed; turn it off: the next `Recheck` clears both. Count `Recheck` calls over an hour: exactly one per GC recheck period, none from a GC pass. | T |
| [§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens) | A suspended store with an object version left behind: `Recheck` lists versions and fails; after the repair writes the proof, ten further `Recheck` calls list no versions and pass. Enable versioning again: the proof is emptied, and the next suspended `Recheck` lists again. | T |
| [§4.7](#4.7%20Health%20is%20one%20probe%20call) | Delete the health object behind the store: the get probe fails with `ErrNotFound`; reopen the store: the check writes it again and the get probe passes. A store whose puts all succeed and whose put probes were skipped still passes the get probe. | T |
| [§4.10](#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers) | Open 1,000 stores from one store configuration: one client exists, its limit equals the callers' total concurrency, and connections opened under a steady load do not grow with the number of stores. | |
| [§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes) | Open an immutable store against each of: no versioning, versioning without a lock, a governance-mode lock, any default retention, no retention cap or a cap above the record's, no noncurrent-version or no current-version expiry rule, either rule present under one folder's prefix but not over `exports/` or `progress/`, a current-version rule younger than the lifecycle age, a credential that may delete the health object without naming a version, and a credential allowed any of the five bucket-configuration actions: each fails naming the missing requirement. Creating the location record provokes the cap with a retain-until one second above it and each configuration put with the configuration just read, and requires every one refused. A bucket with all of them: open succeeds, sends no put request after the location record's integrity check, and extends the location health object's recorded version to the last end of its generations at or before now plus the retention cap, which the cap's policy accepts. | T |
| [§4.7](#4.7%20Health%20is%20one%20probe%20call) | Create an immutable location record: its health object exists at the location's root and its version is in the record before any namespace copies there. A first namespace's folder then opens and its get probe passes. A design whose health object sits under the folder's prefix cannot open that first folder. | T |
| [§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes) | Put 10⁵ names reused on one day at an immutable location: their retain-untils are spread across `G` by each name's phase, no one generation end holding more than twice its share. | T |
| [§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes) | On an immutable store: every `PutVersion` and `PutObject` request carries a compliance-mode retain-until at a generation's end; `Health(DirPut)` returns `ErrInvalid` and the transport sees no request; `ExtendRetention` to a time at or before the current retain-until returns `nil` after one retention read and sends no retention write. Advance a virtual clock past the lifecycle age with opens in between, and again with one process left up and only its `Recheck`s running: the location still opens and the get probe passes. A store that never extends the health object, or extends it only at open, stops opening here. | T |
| [§4.15](#4.15%20Versioned%20objects%20at%20a%20backup%20location) | At an immutable location put one backup's state object three times, then have the transport add a delete marker over it as an expiring lifecycle rule would: `ListObjects` yields all three versions, `GetObject` by each returns its bytes, and a get of the current object is refused. At a mutable location a second put replaces the first, and `DeleteObjects` removes it. A block listing yields no versioned object. Re-put an older authentic state version on top of a newer one: the caller still takes the newer by its sequence. | T |
| [§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens) | Lower a backup location's lock or suspend its versioning between two copies: the folder record's holder runs `Recheck` before the next copy, finds the drift, and the copy fails before any put; the location's put direction is unhealthy until a `Recheck` passes. A design whose only `Recheck` caller is GC copies into the drifted folder. | T |
| [§4.8](#4.8%20Errors%20are%20a%20closed%20set) | Answer puts with each quota or storage-full reply the profile lists, and with `507`: each is `ErrDenied` of cause `quota`. Answer with `403`: `ErrDenied` of cause `access`. | T |
| [§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes) | On an immutable store, `Delete` returns `ErrInvalid` for every name and the transport sees no request; `PutVersion` twice under one name returns two versions, and `GetVersion` of the first returns the first bytes; `ExtendRetention` to an earlier time is refused without a request. | T |
| [§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes) | Open a mutable store on a bucket configured for an immutable location: open fails. Open an immutable store over a bucket with no versioning: open fails. | T |
| [§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success) | The capability check provokes a conditional put and reports `conditional_put`; no other call of the suite sends a conditional header. | T |
| [Appendix C.3](#C.3%20How%20the%20contract%20maps%20to%20S3) | Every request of the suite carries the pinned payload-signing value; an endpoint that is not HTTPS refuses the open. Report a bucket default encryption under which the ETag is not the content MD5, on a store whose check chose the ETag: open fails naming it; change the default after open: `Recheck` reports drift. | T |

The in-memory store **MUST NOT** be the only store these run against. It cannot
lose an acknowledged write, half-complete or delay.

### 7.2 Fault transport

A fault-injecting transport sits under a real store's client, at the request
level, and can, per request: drop it, delay it, fail it with a
chosen status and error code, truncate the response body, flip a byte in either
direction, clamp a range, ignore a checksum header (at all sizes, or only above
a threshold), hold new keys out of listings for a while, answer the first get after a put
with `404`, report a storage class, or pretend a bucket setting (versioning,
object lock and its mode and retention, lifecycle and tiering rules) is on or
off — at open, or switched later — or that a delete is allowed or refused. Each failure the contract names can then be
produced without a misbehaving service.

It also emulates the service faults measured in [Appendix B](#Appendix%20B%20%E2%80%94%20measurements): a service that
ignores every put checksum, and one with versioning or an expiring lifecycle
rule, **MUST** make a mutable store's capability check refuse, and one lacking
any requirement of [§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes) **MUST** make an immutable store's refuse. No real service is available
with those faults, so these profiles are the negative tests.

### 7.3 Codec tests

| Section | Check |
| --- | --- |
| [§3.3](#3.3%20Transforms) | Round-trip random chunk sets (1 byte to the chunk maximum) under every transform configuration: decode equals the input. |
| [§3.3](#3.3%20Transforms) | Encode under one transform configuration, change the configuration, decode: equal. |
| [§3.4](#3.4%20Every%20read%20is%20verified%20by%20the%20codec) | Flip every byte of an encoded block in turn: every read either fails or returns only chunks whose hash matched. |
| [§3.2](#3.2%20Layout) | Corrupt one body: every other chunk of the block still reads. |
| [§3.2](#3.2%20Layout) | A block fetched under a different name fails the name check. An unknown version fails with a version error, not a parse error. |
| [§3.2](#3.2%20Layout) | Fuzz the decoder: no panic, and no allocation larger than the fixed header maximum before the header is validated. A header declaring more than `N` entries, or a body longer than `MaxDecodeLen(Max)`, is refused before allocating. Remove a transform from the current chain: a block written under it still decodes. |
| [§3.2](#3.2%20Layout) | Get one body by range alone: its envelope's version byte is read and the chunk decodes without the header. |
| [§3.4](#3.4%20Every%20read%20is%20verified%20by%20the%20codec) | Plant a material ID in a body that the block record's census does not list: the read fails as malformed and the provider is never asked for it. |
| [§3.2](#3.2%20Layout) | Encode a block of an encrypting namespace: no plaintext chunk hash of its chunks, and no body offset or length in the clear, appears anywhere in its bytes; a whole-block read still recomputes and verifies its name without material, unseals the index under the header key, and verifies every chunk; a ranged read at block metadata's range verifies without reading the header. |
| [§3.4](#3.4%20Every%20read%20is%20verified%20by%20the%20codec) | A recorded range the store rejects as past the end: the codec returns `ErrCorrupt` of kind `malformed`. |
| [§3.5](#3.5%20Format%20changes%20are%20migrations) | With the namespace's write version below the newest the codec knows, blocks are written at the namespace's version; a write version the binary does not know refuses writes and still reads. |
| [§3.3](#3.3%20Transforms) | Run the engine's read and write tests with and without transforms: no difference observable apart from offsets and timing. |
| [§3.4](#3.4%20Every%20read%20is%20verified%20by%20the%20codec) | A ranged read at a wrong offset or length fails with `ErrCorrupt`, and nothing re-reads the header. |
| [§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success) | Encode the same block twice, once from declared lengths and once after a measuring pass, under every transform configuration: the bytes, the header and the checksum are identical, and the declared size equals the bytes streamed. |
| [§3.5](#3.5%20Format%20changes%20are%20migrations) | A stored fixture of every released version decodes. |
| [§3.2](#3.2%20Layout) | Write a block under header key H1, make H2 current, read the block: its index is checked under H1, as its seal names, and a recovery import resolves it. Plant a seal naming a key outside the block record's census: the read fails as malformed and the provider is not asked. |

### 7.4 Services

| Service | Expected | When |
| --- | --- | --- |
| In-memory store | passes the rows of [§7.1](#7.1%20Conformance%20suite) not marked T | every change |
| A local emulator of the remote service, current version, under the fault transport | passes [§7.1](#7.1%20Conformance%20suite) and the capability check | every change |
| Each supported production service ([Appendix B](#Appendix%20B%20%E2%80%94%20measurements)) | passes the capability check and [§7.1](#7.1%20Conformance%20suite) | nightly, in a disposable namespace |

A new service is supported once it passes the nightly row. Its results go into
[Appendix B](#Appendix%20B%20%E2%80%94%20measurements).

### 7.5 Benchmarks and targets

Run against the local emulator after merge and against a real service daily
([the RFC index](rfc-index.md#Test%20tiers)). Record the service, region, block size and pool sizes
with every result.

| Benchmark | Measures | Target |
| --- | --- | --- |
| Codec encode and decode, no transform | MB/s per core | within 10% of plaintext hashing on the same box: verification is its only per-byte cost |
| Put of one full block, 1 to N workers | MB/s at each step | at the service's saturation point, within 10% of the same curve measured with raw client calls; the pool-sizing tool uses this curve ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)) |
| Ranged get of one chunk (4 KiB to 1 MiB) | requests, p50 and p99 latency | one request per chunk, and within 10% of a raw client call of the same size |
| Whole-block get | MB/s | within 10% of a raw client call |
| Put of one block with and without a measuring pass | MB/s | report; the gap is what the second encode costs |
| Steady load at a fixed pool size for 10 minutes | connections opened | no more than the pool size, plus one per connection the service closed (counted from the transport's close events) |
| Health probe | requests | exactly one |
| List of 100,000 blocks | requests, peak memory | one per page of 1,000, and at most one page held |

## 8. Decisions and open questions

Settled in this revision, with the evidence in [Appendix B](#Appendix%20B%20%E2%80%94%20measurements):

1. **Conditional put is not required** ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)): services that claim the same
   protocol disagree on whether they honour it, and names minted per attempt
   leave nothing for it to protect.
2. **Put integrity comes from the service's check or the store's** ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)).
   Services disagree on which put checksum they enforce, and some enforce none,
   but every service measured returns a digest of the bytes it stored, so the
   store can verify what was stored — trusted only once the capability check has
   proved it at the maximum block size ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)).
3. **Put health is a probe put**, not an existence check on the namespace
   ([§4.7](#4.7%20Health%20is%20one%20probe%20call)). It is the only one of the two that proves writability, at a cost
   comparable to the existence check. Get health is a probe get of the health
   object, which every open writes, so the get probe never depends on a put probe
   having run, and a store that turns read-only stays readable.
4. **Versioning, object lock and expiring lifecycle rules are refused for a
   mutable store, and required for an immutable one** ([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)). On a
   namespace's own store each stops sweep from freeing space or deletes stored
   blocks behind the store's back, so every namespace's store is mutable and
   refuses them ([Appendix C.1](#C.1%20Required%20service%20features)). A backup location states its mode in its
   configuration record ([RFC 12 §6.2](rfc-12-snapshots.md#6.2%20Configuration)). In `immutable` mode the location
   needs versioning and a compliance-mode lock with no default retention, a
   service-enforced cap on the retention a put may set, a noncurrent-version
   expiry rule, and a credential that cannot delete; every put carries its own
   retain-until, it reads, writes and lists by version, extends retention in
   generations instead of re-putting, keeps its health object's lock alive, and
   sends no put probe. Nothing in it can be removed by DittoFS
   or by a holder of the installation's credentials before its retention ends.
   Every setting either mode depends on is re-read by `Recheck`
   ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)).

Settled with them:

5. **Request latency does not change the design.** A slow service needs more
   workers, and the sizing tool measures that on the host that will run it
   ([RFC 3 §2.11](rfc-3-syncer.md#2.11%20Pool%20sizes%20are%20measured%20once%2C%20by%20a%20tool)). The figure in [Appendix B](#Appendix%20B%20%E2%80%94%20measurements) is one host's path, recorded, not a
   property of the service.
6. **Probe cost is removed where it would matter.** One put per store every 5 s
   is 17,280 puts a day; at 10⁴ stores that is 2,000 puts a second of pure
   probing. A successful transfer stands in for the probe of its direction,
   and stores the syncer registers under one `ProbeKey` — the engine gives one
   key to stores sharing an endpoint, credential and bucket — share one probe
   per direction ([RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work)). So a busy installation sends almost none,
   and an idle one sends one per bucket. A permission that differs by prefix is
   not seen by the shared probe; the syncer's failure window catches it from the
   store's own failing transfers. An immutable store sends no put probe at all
   ([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)).

7. **What "stored" means is cited per service**, since no check can provoke a
   loss after acknowledgement ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)). [Appendix C.1](#C.1%20Required%20service%20features) records, per measured service, when a
   put is acknowledged and across which failure domains. A storage class that
   keeps data in one failure domain is allowed: the capability check reports the
   class's failure domain at open and through health, and a deployment that
   configures such a class accepts what it promises.

Open:

8. **Immutability at the filesystem layer.** Item 4 places immutability in an
   immutable backup location's bucket, outside the filesystem. Whether regulated data
   needs it inside — files or shares that cannot be changed or deleted until a
   retention date, enforced by DittoFS itself — is being looked at. Nothing in
   this document depends on the answer.

---

## Appendix A — where the current code differs

Descriptive, for the refactor, one line per requirement. None of it is a rule to
build around.

| # | The contract says | The implementation today |
| --- | --- | --- |
| A1 | The codec encodes blocks, with a header indexing every chunk ([§3](#3.%20The%20block%20format)) | each body carries its own small header, with no index up front |
| A2 | Every read is verified by the codec ([§3.4](#3.4%20Every%20read%20is%20verified%20by%20the%20codec)) | each consumer re-hashes on its own; relocation checks only a whole-block hash; an unverified range read is exported |
| A3 | One attempt per call ([§4.9](#4.9%20No%20state%20across%20calls)) | the client retries up to ten times with backoff |
| A4 | The connection pool is derived from the worker pools ([§4.10](#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers)) | a fixed connection limit, shared by one syncer per share |
| A5 | A put is verified in transit ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)) | request checksums are disabled and no digest is compared |
| A6 | Health is a probe put ([§4.7](#4.7%20Health%20is%20one%20probe%20call)) | an existence check, which does not prove writability |
| A7 | A store checks its service before opening, and re-checks drifting settings per GC pass ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)) | no capability check and no re-check |
| A8 | Consumers declare narrow interfaces ([§1.2](#1.2%20A%20contract%2C%20not%20a%20component)) | GC and many other consumers take the whole store interface |
| A9 | The syncer's boundary is a component boundary ([§2](#2.%20The%20dividing%20line)) | the upload window and the fetch, health and prefetch logic live in different components |
| A10 | Block names derive from chunk hashes and a per-attempt nonce carried in the header, with a put intent recorded first ([RFC 2 §4.2](rfc-2-carver.md#4.2%20A%20block)) | names are random and cover no chunk hash, so a whole-block read cannot verify them, and no intent is recorded, so a retried put after an unknown outcome leaves an orphan only a listing finds |
| A11 | Stores are tested against the service versions they claim to support ([§7.4](#7.4%20Services)) | one test suite pins an emulator version that ignores checksums and conditional puts, while others use a current one |

## Appendix B — measurements

Run on 2026-09-28 with aws-sdk-go-v2 (service/s3 v1.113.1), retries off. Where
the SDK might add or drop headers, a raw SigV4-signed request confirmed the
result. Scaleway Object Storage (`fr-par`) and Cubbit DS3 (`s3.cubbit.eu`) were
each tested in a disposable bucket, deleted afterwards; LocalStack ran in
Docker. MinIO could not be pulled in this environment and was not measured.

**Service semantics**

| Behaviour | Scaleway | Cubbit DS3 | LocalStack 4.13.1 | LocalStack 3.0 |
| --- | --- | --- | --- | --- |
| Put with `If-None-Match: *` on an existing key | `412`, original kept | **`200`, overwritten** | `412`, original kept | **`200`, overwritten** |
| 16 concurrent conditional puts to one new key | 1 succeeds, 15 get `412` | **all 16 succeed** | 1 succeeds, 15 get `412` | **all 16 succeed** |
| Put with a wrong `x-amz-checksum-crc32c` | `400 InvalidRequest`, nothing stored | **`200`, stored** | `400 BadDigest`, nothing stored | **`200`, stored** |
| Put with a wrong `Content-MD5` | **`200`, stored** | **`200`, stored** | `400 BadDigest`, nothing stored | `400 InvalidDigest`, nothing stored |
| Put with a wrong signed payload hash | **`200`, stored** | **`200`, stored** | — | — |
| ETag of a single-part put | MD5 of the stored bytes | MD5 of the stored bytes, also when a wrong `Content-MD5` was sent | — | — |
| Range `bytes=2-5` of a 10-byte object | `206`, 4 bytes | `206`, 4 bytes | — | `206`, 4 bytes |
| Range starting past the end | `416 InvalidRange` | `416 InvalidRange` | — | `416 InvalidRange` |
| Range `bytes=8-15` of a 10-byte object | **`206`, 2 bytes (clamped)** | **`206`, 2 bytes (clamped)** | — | `206`, 2 bytes |
| Range on an absent key | `404 NoSuchKey` | `404 NoSuchKey` | — | `404 NoSuchKey` |
| Delete of an absent key | `204` | success | — | `204` |
| `DeleteObjects` of 3 keys plus one never written | all 4 reported deleted, the 3 keys gone | same | — | — |
| 2,500 keys, pages of 1,000, resumed with `start-after` | each key once, ascending, `LastModified` present | same | — | same |
| `HeadBucket` / 0-byte put, median of 20 | 50 ms / 122 ms | 1.2 s / 945 ms | — | 2.5 ms / 2.7 ms |
| Compliance-mode put with a retain-until above a bucket-policy cap on `s3:object-lock-remaining-retention-days` | not measured | not measured | not measured | not measured |
| Delete naming no version, by a credential denied `s3:DeleteObject`, on a versioned locked bucket | not measured | not measured | not measured | not measured |
| `PutBucketPolicy`, `DeleteBucketPolicy`, `PutBucketObjectLockConfiguration`, `PutLifecycleConfiguration`, `PutBucketVersioning` by a credential the bucket policy denies them | not measured | not measured | not measured | not measured |
| `PutObjectRetention` lowering a compliance-mode retain-until | not measured | not measured | not measured | not measured |

**Connection pool** (net/http client against a local TLS server adding 20 ms per
request, 64 workers for 5 s each, 64 KiB responses)

| Client setting | Requests/s | New connections | p50 | p99 |
| --- | --- | --- | --- | --- |
| net/http default (idle per host 2, no cap) | 2,956 | 2,382 | 21 ms | 30 ms |
| idle 64, no cap | 2,817 | 64 | 22 ms | 33 ms |
| idle 64, cap 64 | 2,826 | 64 | 22 ms | 31 ms |
| idle 16, cap 16 | 695 | 16 | 91 ms | 118 ms |

On loopback a new TLS handshake is nearly free, which is why the default's churn
does not cost throughput here. The AWS SDK's own client keeps 10 idle
connections per host, not 2. The churn's cost over a WAN is not measured.

**Service notes, not measured.** From each service's documentation, to be
confirmed by the capability check before the service is supported:

| Service | Note |
| --- | --- |
| Backblaze B2 | keeps versions of every object on its native API, so the versioning check is expected to refuse it; how its S3-compatible API reports versioning is unverified |
| Wasabi | bills a minimum storage duration per object, so a block deleted early is still paid for until it expires: sweep and relocation churn cost money there that they do not elsewhere |
| Google Cloud Storage (XML API) | has no batch delete, so the store deletes one key per request ([§4.5](#4.5%20Delete%20is%20batched%20and%20idempotent)) |
| MinIO | not measured: it could not be pulled in the measurement environment |
| AWS S3 | not measured; documents honouring `If-None-Match` and `If-Match` on puts, which changes nothing here, since no rule uses a conditional put ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)) |
| Ceph RGW | not measured |
| Cloudflare R2 | not measured |

No service in this table is supported until it passes the nightly row of
[§7.4](#7.4%20Services), whatever its documentation or compatibility claims say. The two most
deployed, AWS S3 and MinIO, are the first to measure; until then a deployment on
either runs on a service this set has not checked.

## Appendix C — the S3-compatible block store

S3-compatible object storage is the primary backend. This appendix is its
profile ([§5](#5.%20Backend%20profiles)): what it requires of the service, how it maps [§4](#4.%20The%20store%20contract) onto it, and
its capability check.

### C.1 Required service features

| Feature | Used for | Required behaviour | Verified |
| --- | --- | --- | --- |
| `PutObject`, single part | put, put probe | atomic replace; readers never see a partial object | from the service's documentation; it cannot be provoked |
| Put integrity: an enforced `x-amz-checksum-crc32c` or `Content-MD5`, or else an ETag equal to the MD5 of the stored bytes | put | a corrupted body is rejected, or detected from the ETag — at the maximum encoded block size, not only on small objects | check step 1 |
| `GetObject` after `PutObject` | get | strong read-after-write for a new key and an overwritten one: a get from any client after the put's `200` returns the new object ([§4.4](#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly)) | check step 2, by a get issued at once after the put; and from the service's documentation, since a lag that the check happens to miss cannot be provoked |
| `x-amz-storage-class` on every put; the class each get reports | put, open | every object written in the class the profile names, never the service default; no archive class (one whose get needs a restore) | check step 2: the class reported for the check object |
| `GetBucketIntelligentTieringConfiguration` (list) | open, `Recheck` | no configuration with an archive tier whose filter could match the prefix | check step 5 |
| `GetObject` with `Range` | get | `206` with the bytes asked for; `416` when the range starts past the end. A clamped overlong range is tolerated: the store checks lengths itself ([§4.4](#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly)) | check step 2 |
| `DeleteObjects` (batch, up to 1,000 keys) with `Content-MD5`, `Quiet` false | delete | every key, absent ones included, listed as deleted; per-key errors reported per key; a key missing from the reply is a failure ([§4.5](#4.5%20Delete%20is%20batched%20and%20idempotent)) | check step 3 |
| `ListObjectsV2` with `prefix`, `max-keys`, `start-after` | list | ascending key order, every key once across pages, `LastModified` present | check step 4 |
| `GetBucketVersioning` | open, `Recheck` | versioning never enabled: no status in the reply. A `Suspended` bucket still keeps the versions written while it was on, and object lock requires versioning, so this one check covers both; `Recheck` accepts `Suspended` where the repair proof exists, and otherwise once `ListObjectVersions` under the prefix finds only current versions ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)) | check step 5 |
| `DeleteObjects` with `VersionId`, `ListObjectVersions` | `DeleteVersions`, `Recheck` after drift | every version and delete marker of a name removed; listing reports every version under the prefix | only when versioning drifted; not checked at open |
| `GetBucketLifecycleConfiguration` | open, `Recheck` | no rule that expires or transitions objects under the store's prefix: expiry deletes stored blocks, and a transition to an archive class makes gets fail. A rule counts as matching if its filter could match any object under the prefix — a shorter or empty prefix filter, or a tag or size filter the store's objects could meet | check step 5 |
| `GetObject` / `PutObject` on the claim key | open | the claim is read ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)); absent is reported as absent | check step 6 |
| `PutObject` with `If-None-Match: *` | open | none: the outcome is reported as `conditional_put` and nothing uses it ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)) | check step 4 |
| `GetBucketEncryption` | open, `Recheck` | the bucket's default encryption is read and recorded with the integrity mechanism step 1 chose. Where that mechanism is the ETag, a default under which the ETag is not the MD5 of the stored bytes (encryption under a customer-managed or service-managed KMS key, or customer-provided keys) refuses the open; any change of the default after open is drift, since it can change what the ETag means and what step 1 measured | check step 5 |

**An immutable store** ([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) replaces the versioning and lifecycle rows above with
these, and drops `DeleteObjects`:

| Feature | Required behaviour | Verified |
| --- | --- | --- |
| `GetBucketVersioning` | `Enabled` | check step 5 |
| `GetObjectLockConfiguration` | object lock enabled, and **no** default retention rule | check step 5 |
| `GetBucketPolicy` | a statement denying the location's credential `s3:PutObject` and `s3:PutObjectRetention` whenever `s3:object-lock-remaining-retention-days` exceeds the location record's retention cap, with that cap or a lower one ([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)); and a statement denying it `s3:PutBucketPolicy`, `s3:DeleteBucketPolicy`, `s3:PutBucketObjectLockConfiguration`, `s3:PutLifecycleConfiguration` and `s3:PutBucketVersioning`; the credential must be allowed to read the policy, or the open refuses | check step 5 at every open; the cap and the four put actions also provoked, once per location record, in step 1 |
| `GetBucketLifecycleConfiguration` | a `NoncurrentVersionExpiration` rule and an `Expiration` rule no younger than the record's lifecycle age, both covering the location's whole root (block folders, `exports/`, `progress/`, `keys/`, `control/`); no transition | check step 5 |
| `PutObject` with `x-amz-object-lock-mode: COMPLIANCE` and `x-amz-object-lock-retain-until-date`, returning `x-amz-version-id`; `GetObject` with `versionId` | `PutVersion`, `PutObject`, `GetVersion`, `GetObject` | check step 1, once per location record |
| `GetObjectRetention` | `Retention`, `ObjectRetention`, and the read inside every extension | check step 1, once per location record |
| `PutObjectRetention` | `ExtendRetention` and `ExtendObjectRetention`, compliance mode only, sent only when the read showed an earlier retain-until | check step 1, once per location record |
| `ListObjectVersions` under the location's `exports/`, `progress/` and `keys/` prefixes for the namespace | `ListObjects`: every version, delete markers skipped, resumable by key and version marker | check step 4, once per location record |
| `DeleteObject` without `versionId`, on the location health object | refused (`403`); a delete with `versionId` is not sent, since the lock refuses it whatever the credential may do | check step 7 |

For the two bucket-setting calls, "no configuration" (`404` with the service's
not-found code) passes, `501 NotImplemented` passes (a service without the
feature cannot have it enabled), and `403` refuses: the credential must be
allowed to read both settings, or the check cannot tell. `Recheck` re-reads the
same two settings under the same rules.

**When a put is stored, per service.** Cited from each service's own
statement, not measured ([§8](#8.%20Decisions%20and%20open%20questions), item 7):

| Service | A `200` on put means | Failure domains |
| --- | --- | --- |
| Cubbit DS3 | every fragment of the object's erasure code (N+K per site, across the redundancy class's sites) is stored | the sites of the bucket's redundancy class |
| Scaleway, Standard Multi-AZ | stored under the service's multi-zone redundancy | three availability zones, in regions that offer it |
| Scaleway, One Zone | stored under the service's single-zone redundancy | one availability zone: allowed, reported at open |

A service is added to this table before it is supported.

Not required: conditional put, multipart upload ([RFC 3 §3.4](rfc-3-syncer.md#3.4%20One%20put%20per%20block) rules it out), trailing checksums,
`HeadObject`, `HeadBucket`, `CopyObject`, bucket creation.

### C.2 Layout in the bucket

| Object | Key |
| --- | --- |
| a block | `<prefix>blocks/<name in lowercase hex>` |
| the namespace claim (control role `claim`) | `<prefix>control/claim` |
| the health object (control role `health`) | `<prefix>control/health`; at a backup location, one per location, `<location>control/health`, put when the location record is created ([§4.7](#4.7%20Health%20is%20one%20probe%20call)) |
| the key records (control role `keys`): every key record, wrapped, and a non-encrypting namespace's chunk-ID key in the clear | `<prefix>control/keys` |
| capability-check objects | `<prefix>check/<random>/…` |
| at a backup location: a backup's export, state object and progress objects ([§4.15](#4.15%20Versioned%20objects%20at%20a%20backup%20location)) | `<location>exports/<namespace>/<backup>/export`, `<location>exports/<namespace>/<backup>/state`, `<location>progress/<namespace>/<backup>/<batch>` |
| at a backup location: a namespace's key records (`ObjKeys`) | `<location>keys/<namespace>/keys` |

Block names are uniformly distributed hashes, so keys spread across the service's
partitions without a partitioning scheme; nothing is placed between `blocks/` and
the name ([§4.2](#4.2%20Names%20in%2C%20locations%20kept%20inside)). A listing walks `<prefix>blocks/` only.
Several stores may share a bucket under different prefixes, never one prefix
([§4.2](#4.2%20Names%20in%2C%20locations%20kept%20inside)).

### C.3 How the contract maps to S3

| Contract | S3 |
| --- | --- |
| one attempt per call ([§4.9](#4.9%20No%20state%20across%20calls)) | SDK retries off (maximum attempts 1) |
| client configuration | the request and response checksum modes set explicitly to `WhenRequired`, never left at the SDK's default, which has changed between releases; the store sends exactly the integrity header its profile chose and nothing else. The payload-signing mode is pinned too: every request is signed with `x-amz-content-sha256: UNSIGNED-PAYLOAD`, over HTTPS only, and an endpoint that is not HTTPS refuses the open. A signed payload hash is not an integrity check here — two measured services store a body whose signed hash is wrong ([Appendix B](#Appendix%20B%20%E2%80%94%20measurements)) — and signing it would need a pass over the body before the first byte. The capability check runs with this same configuration |
| connection pool ([§4.10](#4.10%20The%20connection%20pool%20is%20derived%20from%20its%20callers)) | one HTTP client per store configuration: its per-host connection cap, its idle connections per host and its total idle connections all set to the derived limit |
| put integrity ([§4.3](#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20stored%20on%20success)) | send the checksum header chosen by the check, computed by the measuring pass. If the service enforces none, `Checksum` is `None`: compute the MD5 while streaming, compare the returned ETag with it, and return `ErrCorrupt` on a mismatch; the retry puts the same bytes again |
| exact get ([§4.4](#4.4%20Get%3A%20a%20whole%20block%20or%20one%20range%2C%20exactly)) | compare `Content-Range` with the request, then the body's length with `Content-Range` |
| `ErrNotFound` | `404 NoSuchKey`; `404 NoSuchVersion` on `GetVersion` |
| `ErrInvalid` | `416`; a `Content-Range` that differs from the request (a clamp); `400` codes not listed elsewhere; `501` |
| `ErrDenied`, cause `access` | `401`, `403`, `404 NoSuchBucket`, `400 ExpiredToken`, `400 InvalidToken`; any `3xx`, which on this service means the endpoint or region is wrong, never a block's state |
| `ErrDenied`, cause `quota` | `507` with any code; and any reply whose error code the profile's quota list names — each service's own bucket-quota-exceeded and storage-full codes, recorded per service in [Appendix B](#Appendix%20B%20%E2%80%94%20measurements) as they are measured; for MinIO, `XMinioAdminBucketQuotaExceeded` (a `400`), taken from its documented error table and not yet measured. Such a reply is matched before the rows below, so a quota answered with `400` or `403` is never `ErrInvalid` |
| `ErrThrottled` | `429`, `503 SlowDown` |
| `ErrTransient` | `400 RequestTimeout`, `408`, `409 OperationAborted`, `500`, `502`, `503` other than `SlowDown`, `504`, network errors, timeouts, a body shorter than a `Content-Range` that matches the request, a mismatching response checksum on a get ([§4.8](#4.8%20Errors%20are%20a%20closed%20set)), a response body that does not parse |
| `ErrCorrupt` | `400 BadDigest`, `400 InvalidDigest`, `400 XAmzContentSHA256Mismatch`; the exact status and code the check recorded for its chosen checksum, only on puts that carried that checksum; an ETag mismatch on a put |
| everything else | the rest of its class: any other `4xx` (including `404` with another code) is `ErrInvalid`, any other `5xx` is `ErrTransient`, and a status outside `100`–`599`, or any other reply, is `ErrTransient`. The table is total: a reply this row does not catch cannot exist |

`404 NoSuchBucket` is `ErrDenied`, not `ErrNotFound`: a missing bucket is the
store's failure, and reading it as a missing block would make every read
re-resolve its chunk ([RFC 8 §7.7](rfc-8-engine.md#7.7%20An%20absent%20object%20is%20re-resolved%20while%20its%20location%20moves)) and then report it absent, while the
store's health never counted the outage.

S3 throttles above a few thousand requests per second per key prefix and answers
`503 SlowDown`. That limit is the service's, not the client's: the store reports
it as `ErrThrottled` and the syncer holds the flow and backs off, without
counting it against the store's health.

### C.4 The capability check

The S3 profile's check ([§4.11](#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens)). In a fresh `<prefix>check/<random>/`
namespace, with the production client configuration of [Appendix C.3](#C.3%20How%20the%20contract%20maps%20to%20S3), in order:

1. **Put integrity.** For `x-amz-checksum-crc32c`, then `Content-MD5`: put a small
   object with the correct value and expect success, then put another with a
   wrong value and expect a rejection with nothing stored. The first that passes
   both is the one the store sends, and the status and error code of its rejection
   are recorded for [§4.8](#4.8%20Errors%20are%20a%20closed%20set)'s `ErrCorrupt`. Scaleway, for one, rejects a wrong
   CRC32C with `400 InvalidRequest`, not `BadDigest`. If neither is enforced, put
   a known body with a wrong `Content-MD5` and expect `200` with an ETag equal to
   the MD5 of the body actually sent; that proves the ETag is computed from the
   stored bytes, not echoed. If that fails too, refuse. Whichever mechanism was
   chosen is then proved again with an object of the maximum encoded block size
   — a correct put, and one with a wrong checksum or a known body whose ETag must
   match — since services change checksum, ETag and server-side encryption
   behaviour with size.
2. Put a 10-byte object in the profile's storage class; get it at once, and
   expect it back, with that class reported; get bytes 2–5 and expect exactly 4
   bytes; get from offset 20 and expect `416`.
3. Batch-delete two keys that exist and one that was never written, and expect
   all three reported deleted and the two keys gone from a listing. A service that
   rejects the batch operation is recorded, and the store deletes one key per
   `DeleteObject` request instead.
4. Put three keys and list with `max-keys=1` using `start-after`; expect each key
   once, in order, with `LastModified`. Where a listing lags, re-list up to 5
   times over at most 30 s before refusing. Put one of them again with
   `If-None-Match: *`, and record whether the service refused it as
   `conditional_put`.
5. Read the bucket's versioning status, lifecycle rules, tiering configuration
   and default encryption, as [Appendix C.1](#C.1%20Required%20service%20features) states.
6. Read the namespace claim at `<prefix>control/claim` and hand it to the opener.
7. Put the health object at `<prefix>control/health`, then delete everything
   else the check wrote. The health object stays.

An immutable store runs step 1, its version and retention rows, and the provoked
retention cap and configuration puts ([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)) only when its
location record is created or its endpoint or credential changes, and records
the outcome there; at every open it runs step 5 against its own rows (the lock
with no default, the retention cap in the bucket policy, versioning, lifecycle,
encryption), reads its recorded health object version, extends that version's
retain-until ([§4.14](#4.14%20A%20backup%20location%20opens%20in%20one%20of%20two%20modes)), and replaces steps 2–4, 6 and 7 with the one
refused delete of the location health object. It adds no version at open. The
location health object itself is put once, at `<location>control/health`, when
the location record is created, and its version is stored in that record.

Against the services of [Appendix B](#Appendix%20B%20%E2%80%94%20measurements), step 1 chooses CRC32C on Scaleway and LocalStack
4.13.1, `Content-MD5` on LocalStack 3.0, and the ETag on Cubbit DS3.
