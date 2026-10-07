---
rfc: 0
title: "RFC 0 — the data lifecycle"
component: data lifecycle
status: frozen
aliases:
  - RFC 0
tags:
  - rfc
---
# RFC 0 — the data lifecycle

**Status:** frozen. A change to a term, an invariant, the failure model or the
single-node profile changes every RFC that cites it, and is made only together
with them.
**Audience:** anyone implementing or reviewing a storage component.

This is the root of the RFC set. It defines the terms, the data model, the
residency function, the operations and the invariants that no single component
can enforce alone. Every other RFC in the set inherits these and does not
redefine them. Conventions, including the RFC 2119 key words, are in
[the RFC index](rfc-index.md#Conventions).

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

DittoFS is a file server that runs as an ordinary program, with no kernel
module. Clients mount it over NFS (Network File System) or SMB (Server Message
Block), as they would mount a NAS (network-attached storage) box. Behind the
protocols, file content lives in a remote object store — an S3 (Simple Storage
Service) compatible bucket — with a fast local journal in front of it.

This RFC is the root of the set. It defines the words every other RFC uses, the
path content takes from a client's write to the bucket and back, and the rules
that no single component can keep on its own.

### The problem

An object store is cheap, durable and close to bottomless, but it has the wrong
shape for files:

- an object is written whole and never changed in place, so changing 64 KiB of a
  30 GB file means writing a new object;
- every request takes tens of milliseconds and is billed;
- it has no rename, no links, no locks and no permissions.

A file server has to acknowledge small random overwrites at local-disk speed,
keep every acknowledged write across a crash, honour a client's flush (NFS
`COMMIT`, SMB `FLUSH`, `fsync`), and give POSIX and Windows semantics: rename,
hard links, byte-range locks, deny modes, access-control lists.

DittoFS splits the work. A **journal** on a local NVMe disk takes every write
and acknowledges it. Later, in the background, the written bytes are cut into
**chunks**, packed into large **blocks** and uploaded, one object per block. A
**metadata store** records which file holds which chunks at which offsets, and
which block holds each chunk. Once a block is safely uploaded, the local copy
can be dropped, and a later read fetches it back.

### One write, followed end to end

The installation runs on one node, N1. alice's Windows desktop, alice-pc, holds
her profile disk `profiles/alice/ODFC_alice.vhdx` open all day: a 30 GB virtual
disk file that receives 64 KiB random overwrites.

1. **09:00, the write.** alice-pc writes 64 KiB at offset 4 GiB. N1 checks that
   alice may write the file, appends one **record** to the journal, and replies.
   Nothing is hashed, chunked or uploaded on this path. Only the journal holds
   the new bytes: the extent is **Dirty**.
2. **09:00, the flush.** alice-pc sends an SMB flush. The journal makes the
   record durable on its disk, and the metadata store records that the write
   happened — the file's size, holes and modification time — in one commit
   shared with other files' writes. The bucket is not involved.
3. **09:04, the offload.** The journal offers its dirty extents. The bytes are
   cut into chunks of about 256 KiB, each named by a keyed hash of its content;
   chunks from many files are packed into a block of about 4 MiB; the block is
   put to the bucket `dfs-data`. Once the bucket reports it stored, the block is
   **remote-durable**: one metadata transaction records the chunks and the
   block, and only then is the journal told which extents are safe. Those
   extents are now **Resident**: held locally and remote-durable.
4. **18:00, eviction.** The journal is filling. It drops local copies of extents
   it was told are remote-durable, and only those; their disk space returns once
   a whole segment holds nothing still needed. The extent at 4 GiB is now
   **Remote**.
5. **Next morning, the read.** alice signs in and Windows reads the extent. The
   journal answers "not held"; the metadata store answers "chunk C, inside block
   K1". N1 gets those bytes from the bucket, checks their hash and serves them.

Now suppose the NVMe disk damaged the record at 09:02, after the flush and
before the offload. A design with one source of truth sees only "nothing held
locally" and cannot tell *never written* from *lost*: it returns zeros, or the
older bytes still in the bucket, and reports success. Here the two sources
answer different questions. The metadata store says the extent was written and
no current chunk covers it; the journal says it holds nothing. That combination
is **Lost**, and the read fails with an error. Zeros are returned only for an
extent that was never written.

```text
 alice-pc (SMB)                    build01 (NFS)
   │ write 64 KiB, flush             │ untar, compile, rm -rf
   ▼                                 ▼
 ┌─────────────────────────── node N1 ───────────────────────────┐
 │ protocol adapters ──► filesystem service ──► metadata store   │
 │                              │               2 flush records  │
 │                              ▼                 the write      │
 │   journal (local NVMe) ◄── engine            files, refs,     │
 │   1 append, acknowledge      │               chunks, blocks   │
 │   4 evict once durable       │ 3 offload           ▲ record   │
 │                              └─► cut chunks ─► pack block ─┐  │
 └────────────────────────────────────────────────────────────│──┘
                                                   put block  ▼
 5 a read of an evicted extent ◄─ get, verify ── bucket dfs-data
```

**The whole system on one machine.** Every layer a client operation passes
through, with the three paths numbered: write, offload, read.

![A single node: NFS and SMB adapters translate only; the filesystem service admits, authorises and orders every operation; below it the metadata store, open state and the engine with journal, carver, transforms, syncer and GC; backed by an embedded transactional database, a local journal device and an S3 bucket](img/rfc0-single-node.svg)

**The same system as a cluster.** The binary is the same; roles split across
machines. `protocol` nodes hold client connections and forward each call;
`storage` nodes each own some shards and replicate their journals to each other;
every node shares one replicated metadata store and one bucket.

![A cluster: protocol nodes P1 and P2 behind floating addresses forward each call to the primary of the file's shard; storage nodes S1, S2, S3 are each primary of one shard and replica of the others, shipping journal records between them; all share a replicated metadata store and one S3 bucket](img/rfc0-cluster.svg)

The layers, and which node runs which, are drawn in [§1.3](#1.3%20The%20layers).

### The words you need

The [Glossary](#Glossary) below defines every shared term in one line. These
seven carry this document:

- **Journal**: the local tier, one per disk, holding recent writes until they
  are offloaded ([§1](#1.%20Scope)).
- **Offload**: the background pass that uploads a journal's dirty extents and
  records them in metadata ([§5.2](#5.2%20Offload)).
- **Chunk**: a run of a file's bytes, about 256 KiB, named by a keyed hash of
  its content and shared by every ref that names it ([§2.1](#2.1%20Entities)).
- **Block**: one object in the bucket, a whole number of chunks written by one
  put; a chunk never spans two blocks ([§2.2](#2.2%20How%20a%20file%20relates%20to%20its%20chunks)).
- **Ref** and **refcount**: one file's use of one chunk at one offset; a chunk's
  refcount is how many refs name it ([§2.1](#2.1%20Entities)).
- **Residency**: where an extent's bytes are, computed on every read from the
  journal's answer and the metadata store's — Absent, Dirty, Resident, Remote
  or Lost — and never stored ([§4.2](#4.2%20The%20residency%20function)).
- **Evict**, **reclaim**, **sweep**: drop a local copy that is remote-durable;
  recover local space without losing anything; delete a block from the bucket
  that nothing references. Only sweep destroys a last copy ([§8](#8.%20Reclamation)).

### What this RFC promises

- A write is acknowledged only once the journal can recover it.
- An extent whose write reached its stability point never reads as zeros, nor
  as older content that write replaced. If its bytes are gone, the read fails.
  Before the stability point, a write lost to local corruption can read as
  before it, and the write verifier changes so the client resends it
  ([§4.2](#4.2%20The%20residency%20function)).
- A local copy is dropped only after the bucket holds it, and that is learnt
  from a report by whoever observed the upload, never assumed from a transfer
  finishing or time passing.
- A block is deleted from the bucket only when no file, snapshot or clone
  references a chunk whose record locates it there.
- Every failure — bucket down, journal full, metadata unwritable, crash, a lost
  journal device — has one specified behaviour. Each clears on its own once the
  cause is gone, except the loss of a journal device that held content not yet
  remote-durable, which waits for an operator to acknowledge that loss
  ([§10](#10.%20Failure%20model)).
- The first release is one node with both roles, an embedded metadata store and
  no replicas. [§1.4](#1.4%20The%20single-node%20profile) says which rules bind it; the rest are
  marked **(cluster)**.

### A guided tour of the set

[The index](rfc-index.md) lists every RFC; this is which one to open for what.

| To learn | Read |
| --- | --- |
| how writes are stored on local disk, survive a crash, and fill it | [RFC 1](rfc-1-journal.md), journal |
| how bytes become chunks, and chunks become blocks | [RFC 2](rfc-2-carver.md), carver |
| how blocks are uploaded and fetched, retried and paced | [RFC 3](rfc-3-syncer.md), syncer |
| what an object in the bucket looks like, and what a backend must offer | [RFC 4](rfc-4-remote-tier.md), remote tier |
| compression, encryption and the threat model | [RFC 5](rfc-5-transforms.md), transforms |
| which chunk holds each byte of a file, and how chunks are counted | [RFC 6](rfc-6-block-metadata.md), block metadata |
| how all metadata sits in one database: entities, keys, counters | [RFC 16](rfc-16-metadata-store.md), metadata store |
| names, directories, handles, permissions, when a file stops existing | [RFC 7](rfc-7-namespace-metadata.md), namespace |
| opens, locks, deny modes, leases and delegations | [RFC 14](rfc-14-open-state.md), open state |
| the one interface NFS and SMB call | [RFC 17](rfc-17-vfs.md), filesystem service |
| the content path that ties the journal, carver, syncer and metadata together | [RFC 8](rfc-8-engine.md), engine |
| deleting blocks nothing references, safely | [RFC 9](rfc-9-gc.md), GC (garbage collection) |
| snapshots, backups, moving a share | [RFC 12](rfc-12-snapshots.md), snapshots |
| what is a setting and what is fixed | [RFC 13](rfc-13-configuration.md), configuration |
| more than one node: replicated journals, shards, roles — deferred past the first release ([§1.4](#1.4%20The%20single-node%20profile)) | [RFC 10](rfc-10-journal-replication.md), [RFC 11](rfc-11-ownership.md), [RFC 15](rfc-15-topology.md) |

Identity, authorization, the protocol adapters and operations are RFC 18–25,
planned. A first pass can follow the write path: this RFC, then 1, 2, 3, 6 and
8; then 7 and 14 for what clients see; then 15 for how it spreads over nodes,
once the cluster is built.

### The shared cast

Every RFC tells its examples with the same installation, so a story started in
one continues in the next.

- **The installation** `dittofs` serves two shares:
  - `profiles`, over SMB, for Windows desktops: one profile container per user,
    such as `profiles/alice/ODFC_alice.vhdx`, a 30 GB virtual disk with about
    10.5 GB used, held open all day by one read/write handle and written by
    64 KiB random overwrites; bob has the same;
  - `builds`, over NFS, where a Linux build server untars and compiles many
    small files, then removes them all with `rm -rf`.
- **Users** alice and bob. **Clients** alice-pc (Windows, SMB) and build01
  (Linux, NFS).
- **Single node**: node N1, its journal on an NVMe disk, its remote tier the S3
  bucket `dfs-data`.
- **Cluster**, only where an RFC is about several nodes: protocol nodes P1 and
  P2; storage nodes S1, S2 and S3; shard A is `profiles/alice/` with primary S1,
  shard B is `profiles/bob/` with primary S2.
- **Units**, smallest to largest: a **write** (what a client sends); a journal
  **record** (one write as stored locally); a **segment** (a local file of
  records, about 256 MiB); a **chunk** (about 256 KiB of one file's content,
  named by its keyed hash); a **block** (an object in the bucket packing many
  chunks, 4 MiB by default); a **namespace** (a prefix in the bucket); a **shard**
  (a set of files owned by one node at a time).

### How the rest is organised

[§1](#1.%20Scope) sets the scope and draws the layers. [§2](#2.%20Terminology)
defines the entities and operations; [§3](#3.%20Identity) is file identity and
why deduplication is off. [§4](#4.%20Residency) is the heart: the two sources
and the residency function; [§1.4](#1.4%20The%20single-node%20profile) is what the first release must
honour. [§5](#5.%20The%20write%20path) to
[§8](#8.%20Reclamation) follow content through writes, reads, overwrites and
deletes, and space recovery. [§9](#9.%20Invariants) lists the ten invariants
and [§10](#10.%20Failure%20model) the failure model. On a first read, skip
§3.1, §9.1–§9.3 and §10.3.

## Glossary

The words the whole set shares, one or two sentences each. **The row is the
definition**; the linked section elaborates it, and every other RFC uses the
word in this sense and does not redefine it. A linked section that drifts from
its row is a defect in that section, and a row changes only together with the
sections that rely on it. Where a word is overloaded across RFCs, its row names
every sense. A row marked **(cluster)** names a thing the first release does not
have ([§1.4](#1.4%20The%20single-node%20profile)).

### Units at a glance

Two families of unit. The **bytes** units say where content is; the
**ownership** units say who serves it. Sizes are defaults or examples, not limits.

| Unit | What it is | Groups what | Typical size or count | Where it lives | Owned by |
| --- | --- | --- | --- | --- | --- |
| **Bytes, local** | | | | | |
| write | what a client sends: bytes at an offset of one file | nothing; it is the input | up to the maximum write size the protocol negotiates; 64 KiB in the examples | memory, until the journal holds it | [RFC 17](rfc-17-vfs.md), [RFC 8](rfc-8-engine.md) |
| journal record | one write, or one piece of a write split at a segment's end, framed with a header and checksum | the bytes of one write | the write's size | local disk | [RFC 1](rfc-1-journal.md) |
| extent | an `(offset, length)` region of one file's bytes; a value, never a stored thing | nothing; it names bytes | any | nowhere; components keep state *against* one | this RFC |
| segment | an append-only local file of records, sealed when full | records of many files, in arrival order | fixed, not a setting; its value is open ([RFC 1 §12](rfc-1-journal.md#12.%20Open%20questions)); 256 MiB in the examples | local disk | [RFC 1](rfc-1-journal.md) |
| **Bytes, remote** | | | | | |
| chunk | a run of one file's bytes cut at a content-defined boundary, named by the hash of its content | bytes | about 256 KiB; boundaries between 64 KiB and 1 MiB | bytes in a block in the bucket; its record in the metadata store | [RFC 2](rfc-2-carver.md) (identity), [RFC 6](rfc-6-block-metadata.md) (record) |
| block | one object in the bucket, written by one put | whole chunks of many files of one share | a target of 4 MiB by default, settable from 1 MiB to 64 MiB; at most 1,024 chunks; passes the target by at most one chunk, and falls short only at the chunk cap or a pass's end | bucket; its record in the metadata store | [RFC 2](rfc-2-carver.md), [RFC 4](rfc-4-remote-tier.md) |
| namespace | one prefix (a folder) in a bucket; chunks are counted within it | blocks of one or more shares | one per share by default | bucket | [RFC 12](rfc-12-snapshots.md) |
| **Ownership** | | | | | |
| file | a namespace object with an identity, attributes and an ordered list of refs | refs | any size; one 30 GB file in the examples | metadata store | [RFC 7](rfc-7-namespace-metadata.md) |
| share | one exported file tree with its own settings | files | two in the examples | metadata store | [RFC 16](rfc-16-metadata-store.md) |
| shard | a set of files with one primary at a time | files of one share | one per share; one per child of a marked directory **(cluster)** | metadata store (its shard record) | [RFC 11](rfc-11-ownership.md), [§1.4](#1.4%20The%20single-node%20profile) |
| slot | one of a fixed number of buckets a per-child shard's ID hashes into | per-child shards | 4,096, fixed when the installation is created; **(cluster)** | metadata store (the slot table) | [RFC 11](rfc-11-ownership.md) |
| node | one DittoFS server process | the shards it is primary or replica of | one in the first release; several in a cluster | a host | [RFC 15](rfc-15-topology.md), [§1.4](#1.4%20The%20single-node%20profile) |
| installation | every node that shares one metadata store and control plane | nodes, shares, namespaces | one | all of the above | [RFC 13](rfc-13-configuration.md) |

```text
 BYTES axis: where content is            OWNERSHIP axis: who serves it

 client write (64 KiB at 4 GiB)          file profiles/alice/ODFC_alice.vhdx
   │ appended as one                       │ belongs to exactly one
   ▼                                       ▼
 record ── in a segment (256 MiB)        shard A (profiles/alice/)
   │       of the journal, local NVMe      │ its shard record names
   │ offload: carve                        ▼
   ▼                                     primary S1, replicas S2 and S3
 chunks (~256 KiB, named by hash)
   │ pack, whole chunks only             per-child shard ──► one of 4096 slots
   ▼                                     slot table: slot ──► ordered nodes
 block (~4 MiB) ── put into a namespace
                   (a prefix) in bucket dfs-data

 The axes are independent:
 - moving shard A from S1 to S2 moves no bytes in the bucket;
 - one block packs chunks of many files; one segment, records of many files;
 - a file's bytes may sit in any number of segments and blocks.
```

**Do not confuse**

- **chunk** vs **block**: a chunk is content, named by its hash and counted; a
  block is the object that carries whole chunks to the bucket, named afresh for
  each put. Refs name chunks, never blocks.
- **segment** vs **block**: a segment is a local file of records; a block is a
  remote object of chunks. They group bytes differently and neither can be
  derived from the other ([§2.2](#2.2%20How%20a%20file%20relates%20to%20its%20chunks)).
- **shard** vs **share**: a share is what a client mounts; a shard is part or all
  of one share, the unit one primary serves. A share is one shard by default.
- **namespace** vs **share**: a namespace is a folder in the bucket where blocks
  live and chunks are counted; a share is a file tree. Each share gets its own
  namespace by default; a clone or restore joins its source's.
- **record** vs **ref**: a journal record holds the bytes of one write on local
  disk; a ref is a metadata record saying which chunk holds a file's bytes at an
  offset. In [RFC 16](rfc-16-metadata-store.md) "record" alone means one key and
  its value in the metadata store.
- **node epoch** vs **epoch**: a node epoch numbers one node's starts and
  leases; an epoch (shard epoch) numbers one shard record's changes. The write
  verifier follows the first, fencing the second.
- **durable** vs **remote-durable**: durable is held by a local journal after a
  sync, or by every replica's; remote-durable is held by the remote tier, as a
  report from whoever observed the put says. Only remote-durable content may be
  evicted.

### Data units

| Term | Means | Defined in |
| --- | --- | --- |
| **write** | what a client sends: bytes at an offset of one file. The journal stages it and the client is acknowledged; nothing is chunked, hashed or uploaded on that path | [§5.1](#5.1%20Write) |
| **record** (journal) | one framed run of bytes within a segment — one write, or one piece of a write that crossed a segment's end — with a header carrying its identity, length and checksum | [§2.1](#2.1%20Entities), [RFC 1 §4.3](rfc-1-journal.md#4.3%20Records) |
| **segment** | a local append-only file of records, capped at a fixed size and sealed when full; its space returns only when the whole segment is deleted | [§2.1](#2.1%20Entities), [RFC 1 §4.2](rfc-1-journal.md#4.2%20Segments) |
| **extent** | a contiguous `(offset, length)` region of one file's bytes, and the only term for it. A value, not a stored thing | [§2.1](#2.1%20Entities) |
| **held extent** | an extent the journal holds, with its content version and offloaded bit | [RFC 1 §2](rfc-1-journal.md#2.%20The%20model%20it%20presents) |
| **version** | the number ordering every write and removal of one file: 128 bits, an epoch half (zero in this format) and a counter half drawn from the journal serving the file. Where two cover the same byte, the higher wins, whatever order they arrived in. A journal raises its counter above a share's version floor before it serves a share it did not serve when it opened. Not a sequence number (per journal) nor a cut number (per share) | [§2.1](#2.1%20Entities), [RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions) |
| **chunk** | a run of one file's bytes, about 256 KiB, cut at a content-defined boundary and named by its **chunk ID**, a keyed hash of its content under the namespace's chunk-ID key; shared by every ref that names it. An all-zero chunk is never stored: the file records a hole instead | [§2.1](#2.1%20Entities) |
| **Target**, **Min**, **Max** | the one chunking setting, default 256 KiB, and the boundary bounds derived from it, Target ÷ 4 and 4 × Target | [RFC 2 §3.2](rfc-2-carver.md#3.2%20One%20setting%2C%20and%20the%20bounds%20derived%20from%20it) |
| **stretch** | one unbroken run of a file's bytes handed to the carver in one call; an end the offer imposed, not a hole or the file's end, leaves its tail uncut for the next offer | [RFC 2 §2.4](rfc-2-carver.md#2.4%20An%20artificial%20end%20leaves%20the%20tail%20uncut) |
| **block** | the object the remote tier stores: a whole number of chunks, written by one put. It targets a configured size — 4 MiB by default, settable from 1 MiB to 64 MiB — and passes it by at most one chunk, because a chunk never spans two blocks; it falls short only at the format's chunk cap or at the end of an offload pass | [§2.1](#2.1%20Entities) |
| **block plan** | the assembler's list of the chunk hashes for one block and where their bytes sit, from which the block is streamed | [RFC 2 §5](rfc-2-carver.md#5.%20The%20block%20assembler) |
| **block name** | a block's 32-byte identity, minted afresh for each put attempt and never reused, so one name is always one byte sequence | [RFC 4 §4.3](rfc-4-remote-tier.md#4.3%20Put%3A%20a%20whole%20block%2C%20checksummed%2C%20durable%20on%20success) |

### Places

| Term | Means | Defined in |
| --- | --- | --- |
| **journal** | the local tier: one per device on a storage node, holding recent writes until they are offloaded | [RFC 1](rfc-1-journal.md) |
| **remote tier** / **remote store** | the durable object storage behind the journals / one configured backend of it | [RFC 4](rfc-4-remote-tier.md) |
| **remote block store** | one backend's adapter: it maps block names to object keys and makes one attempt per call | [RFC 4 §2](rfc-4-remote-tier.md#2.%20The%20dividing%20line) |
| **bucket** | the object-store container a remote store points at; it holds one or more namespaces, each under its own prefix | [RFC 12 §2.1](rfc-12-snapshots.md#2.1%20A%20namespace%20is%20the%20unit%20that%20moves) |
| **namespace** | one prefix, like a folder, inside a bucket of the remote tier; chunks are named under its chunk-ID key and counted (and, once deduplication is added, deduplicated) within it, never across two. Whether it encrypts is fixed when it is created | [RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count) |
| **control object** | a small object of a fixed role beside the blocks, such as a namespace's claim; never listed as a block | [RFC 4 §4.13](rfc-4-remote-tier.md#4.13%20Control%20objects) |
| **metadata store** | the transactional store holding every fact about files, content, identity and configuration | [RFC 16](rfc-16-metadata-store.md) |
| **KV** | the small interface a metadata backend implements: run a transaction, read, scan, set, delete, guard | [RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend) |
| **control plane** | the records in the metadata store that hold every setting; the one source of configuration | [RFC 13 §2.1](rfc-13-configuration.md#2.1%20The%20control%20plane%20is%20the%20source) |

### Ownership and cluster

| Term | Means | Defined in |
| --- | --- | --- |
| **installation** | all the nodes that share one metadata store and control plane; the widest scope a setting has. The first release's installation is one node | [RFC 13 §3](rfc-13-configuration.md#3.%20Scopes) |
| **node** | one DittoFS server process, with one or both roles; only a `storage` node can be a primary or a replica | [RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20binary%2C%20roles%20chosen%20at%20deployment) |
| **role** | what a node runs: `protocol` (adapters and the filesystem service, no state of its own) or `storage` (the metadata store's view, open state, the content subsystem and journals) | [RFC 15 §2](rfc-15-topology.md#2.%20Roles) |
| **front-end** | **(cluster)** the node a client's call arrives at; when it is not the primary, it forwards the call there | [RFC 11 §5.1](rfc-11-ownership.md#5.1%20Front-ends%20forward%20to%20the%20primary) |
| **route envelope** | **(cluster)** what every forwarded call carries: request ID, shard and epoch, hop count | [RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope) |
| **floating address** / **SMB Witness** | **(cluster)** a client-facing address the cluster owns and a surviving protocol node takes over / the SMB protocol that tells a client an address moved or a node is draining | [RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing) |
| **shard** | a set of files with one primary at a time; a share is one shard, and only in a cluster can a subtree or each child of a marked directory be its own. A file is never split across shards | [RFC 11 §2](rfc-11-ownership.md#2.%20Shards) |
| **slot** / **slot table** | **(cluster)** one of a fixed number (4,096 by default) of buckets a per-child shard's ID hashes into / the record assigning each slot an ordered list of storage nodes, the first as primary, in proportion to capacity | [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards) |
| **primary** | the one storage node that orders and accepts a shard's writes, as its shard record names — as (node, node epoch), with the journal it holds the shard in. On a single node, that node is the primary of every share | [RFC 11 §3](rfc-11-ownership.md#3.%20The%20primary) |
| **replica** | **(cluster)** a storage node other than the primary whose journal holds a copy of every write to the shard | [RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms) |
| **learner** | **(cluster)** a replica that has not yet been given the shard's older content: it counts for new writes but cannot take over, and a takeover drops it | [RFC 10 §7.3](rfc-10-journal-replication.md#7.3%20Joining) |
| **replica set** | **(cluster)** a shard's primary and its replicas; an acknowledged write is in all of their journals until offloaded | [RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms) |
| **shard record** | the metadata-store record naming a shard's primary as (node, node epoch, journal identity, incarnation), its replicas, its epoch and its replica count and floor; it changes only by compare-and-swap. The **incarnation** is raised each time a journal begins serving the shard | [RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities) |
| **node lease** | **(cluster)** the one lease each storage node renews in the metadata store; a node whose lease lapsed serves nothing, a takeover marks its node record lapsed, and every commit on its shards guards that mark. Not a client lease, nor an SMB lease | [RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch) |
| **node epoch** | a number raised for one node at every start and, in a cluster, every new lease; a node whose lease lapsed takes a new one at a higher node epoch and is primary of nothing until it takes a shard over. The write verifier is derived from it, not from the shard epoch | [RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch) |
| **epoch** (shard epoch) | a number in the shard record, raised by every change to it; **(cluster)** a message carrying an older one is refused, and a commit is refused unless the file's fence records hold exactly its (shard, epoch) and the primary's node record is not marked lapsed | [RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch) |
| **fence records** | **(cluster)** two records per file, each holding the (shard, epoch) the file's commits must carry | [RFC 11 §8](rfc-11-ownership.md#8.%20Metadata%20consistency) |
| **committed point** | **(cluster)** per shard, the newest version at or below which the whole replica set holds every operation | [RFC 10 §2.1](rfc-10-journal-replication.md#2.1%20Terms) |
| **takeover** / **handover** | **(cluster)** a replica becoming primary after the primary's lease lapsed, followed by grace / a planned change of primary, with no lease wait and no grace | [RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover), [RFC 10 §9.4](rfc-10-journal-replication.md#9.4%20Handover) |
| **move** (files) | **(cluster)** changing the shard some files belong to, in batches; between batches every file is in exactly one shard. Not a namespace move | [RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries) |

### Metadata records

| Term | Means | Defined in |
| --- | --- | --- |
| **entity** | a plain value a metadata read returns, such as a File, an Entry or a Share; one entity is not one stored record | [RFC 16 §2.1](rfc-16-metadata-store.md#2.1%20The%20entity%20map) |
| **record** (metadata) | one key and its value in the KV. Not a journal record | [RFC 16 §4.2](rfc-16-metadata-store.md#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed) |
| **guard** | a transaction's claim on a key it does not write; it conflicts with a concurrent write of that key, never with another guard | [RFC 16 §4.1](rfc-16-metadata-store.md#4.1%20One%20small%20interface%20per%20backend) |
| **file** | a namespace entry with an identity, attributes and an ordered list of chunk refs. Its identity is a random ID, never reused, that survives rename | [§2.1](#2.1%20Entities), [RFC 7 §2.1](rfc-7-namespace-metadata.md#2.1%20File) |
| **FileAttr** | a file's size, owner and group, mode, timestamps and identifiers. At create, the owner is the caller's principal and the group is the credential's group — the NFS AUTH_SYS gid, or an SMB principal's primary group — or the parent directory's group when the parent has setgid | [§2.1](#2.1%20Entities) |
| **FileData** | the fields of a file that only the write path sets — size, the newest version whose existence is recorded, write-time timestamps — named as a group; not a record of its own | [RFC 6 §2.4](rfc-6-block-metadata.md#2.4%20FileData%20and%20holes) |
| **entry** | one name in one directory, pointing at a file; a file with two hard links has two entries | [RFC 7 §2.2](rfc-7-namespace-metadata.md#2.2%20Entry) |
| **nlink** | the number of entries naming a file, kept exact in the transaction of every entry change | [RFC 7 §4.1](rfc-7-namespace-metadata.md#4.1%20%60nlink%60%20is%20exactly%20its%20entries) |
| **handle** | what a client holds to name a file: the share and the file's ID, never a path; stable across restart | [RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path) |
| **ref** | one file's use of one chunk at one offset, with the versions it came from; it names the chunk by its chunk ID, never the block | [§2.1](#2.1%20Entities), [RFC 6 §2.1](rfc-6-block-metadata.md#2.1%20ChunkRef) |
| **refcount** | how many refs name a chunk, live and history alike; the reverse ref index is the authority it is checked against | [RFC 6 §6.1](rfc-6-block-metadata.md#6.1%20A%20refcount%20is%20exactly%20its%20refs) |
| **chunk record** | one per chunk ID per namespace: which block holds the chunk, where in it, and the chunk's refcount. Two files that carried identical bytes name one record, located in the first carrier's block ([§3.1](#3.1%20Deduplication)) | [RFC 6 §2.2](rfc-6-block-metadata.md#2.2%20Chunk) |
| **block record** | one per block: how many of its chunks are still referenced, and its state — `live`, `retired` or `deleted`. It exists only once the block is remote-durable | [RFC 6 §2.3](rfc-6-block-metadata.md#2.3%20Block) |
| **reverse ref index** | one key per ref, ordered by chunk hash; the deleter reads it before every delete, and the counts are a cache of it | [RFC 9 §2.1](rfc-9-gc.md#2.1%20References%20are%20the%20only%20authority) |
| **existence** | a file's size, holes, overwrite set and modification time: that a write happened, recorded apart from its content. Committed at a stability point, group-committed across files; until then the journal is its authority | [§5.1](#5.1%20Write), [RFC 6 §3](rfc-6-block-metadata.md#3.%20Existence) |
| **hole** | an extent of a file below its size that was never written, or was deallocated; it reads as zeros with no fetch. Recorded as its own record, never inferred from gaps between refs | [RFC 6 §3.3](rfc-6-block-metadata.md#3.3%20Holes%2C%20not%20written%20extents) |
| **hole** / **uncarved** / **carved** | metadata's three answers for an offset below the size: never written; written but with no current chunk; covered by a current chunk | [RFC 6 §3.2](rfc-6-block-metadata.md#3.2%20Every%20offset%20is%20in%20exactly%20one%20class) |
| **overwrite set** | per file, one overwrite record for each run of a committed write that overlapped content whose existence was committed before it, with the newest version covering the run; an offset under a record newer than its ref is uncarved, so a lost overwrite reads as Lost, never as the old chunk. A record leaves only where refs at or above its version cover its whole extent, or the extent became a hole | [RFC 6 §3.3](rfc-6-block-metadata.md#3.3%20Holes%2C%20not%20written%20extents) |
| **removal** | a truncate, deallocate, release or clone target: recorded in one small transaction, then applied to refs in bounded batches, masking the refs it has not yet dropped | [§7](#7.%20Mutation%20and%20removal), [RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation) |
| **pending release** | a record saying a file has lost its last name and its content is still to be released; it keeps nothing alive | [RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees) |
| **put intent** | a record written before every put, naming the block name and the shard or GC partition and epoch it runs under; the commit that records the block deletes it, so an upload that never commits is found without listing the bucket. A single node abandons every intent of its shards at start ([§1.4](#1.4%20The%20single-node%20profile)) | [§5.2](#5.2%20Offload), [RFC 6 §7.6](rfc-6-block-metadata.md#7.6%20Put%20intents) |

### Lifecycle

| Term | Means | Defined in |
| --- | --- | --- |
| **residency** | where an extent's bytes are, computed on every read from the journal's answer and metadata's, and never stored: one of the five classes below | [§4.2](#4.2%20The%20residency%20function) |
| **Absent** | metadata says hole (or past the end of the file) and the journal holds nothing: reads as zeros, with no fetch | [§4.2](#4.2%20The%20residency%20function) |
| **Dirty** | the journal holds the bytes and no current chunk covers them: served locally, and never evicted | [§4.2](#4.2%20The%20residency%20function) |
| **Resident** | a current chunk covers the bytes and the journal still holds them: served locally, evictable | [§4.2](#4.2%20The%20residency%20function) |
| **Remote** | a current chunk covers the bytes and the journal does not hold them: fetched, verified and served | [§4.2](#4.2%20The%20residency%20function) |
| **Lost** | the bytes were written, no current chunk covers them and the journal does not hold them, or their block can no longer be decoded: the read fails, never returns zeros | [§4.2](#4.2%20The%20residency%20function) |
| **durable** | held where a crash cannot lose it: by a journal's device after a sync, or by every replica's journal **(cluster)**. Not the remote-tier sense | [§5.1](#5.1%20Write), [RFC 1 §6](rfc-1-journal.md#6.%20Durability%20and%20ordering) |
| **remote-durable** | held by the remote tier, as a report from whoever observed the put says; never inferred from a transfer ending or time passing. Also "synced to the remote tier" | [§4.3](#4.3%20Reporting) |
| **stability point** | a client's flush (NFS `COMMIT`, SMB `FLUSH`, `fsync`, a stable write, or a close where the protocol needs one): the journal syncs and the file's existence is committed. It does not involve the bucket | [§5.1](#5.1%20Write), [RFC 8 §5](rfc-8-engine.md#5.%20Commit%3A%20the%20stability%20point) |
| **offload** | the pass that copies a journal's dirty extents to the remote tier and records them in metadata | [§5.2](#5.2%20Offload) |
| **offer** / **report** | the frozen view of dirty bytes the journal hands to an offload / the statement of which extents became remote-durable, the only way the journal learns it | [RFC 1 §3.3](rfc-1-journal.md#3.3%20Offload) |
| **offloaded bit** | the journal's per-extent mark that it was told the bytes are remote-durable; set by a report or a fill, and needed before release | [RFC 1 §2](rfc-1-journal.md#2.%20The%20model%20it%20presents) |
| **carve** | cut a stretch of bytes into chunks at content-defined boundaries and hash each one; the chunker says where a chunk ends, the carver runs it | [RFC 2 §1.2](rfc-2-carver.md#1.2%20Two%20layers%3A%20the%20chunker%20and%20the%20carver) |
| **put** / **get** / **put attempt** | make remote-durable / retrieve from the remote tier / one decision to store one block plan under a name minted for it, which every retry reuses | [§2.3](#2.3%20Operations), [RFC 3 §2.5](rfc-3-syncer.md#2.5%20An%20unknown%20outcome%20is%20not%20a%20success) |
| **fill** | place bytes fetched from the remote tier into the journal; never over bytes the journal holds | [§6.2](#6.2%20Fill) |
| **evict** | drop a local copy that is remote-durable, Resident to Remote. Writes nothing to metadata and never touches the remote tier; it frees no disk space by itself, only makes it reclaimable | [§8.1](#8.1%20Evict) |
| **release** | (journal) the mechanism of eviction: stop holding extents whose offloaded bit is set / (file) drop a file's refs, its journal content and its records once it has no name and no open | [RFC 1 §3.5](rfc-1-journal.md#3.5%20Release), [RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees) |
| **reclaim** | (journal) recover local space without changing what content exists. Not a client reclaiming open state in grace | [§8.2](#8.2%20Reclaim) |
| **repack** | copy live records out of a sparse segment and delete the segment; the step that frees journal space, drawing on the repack reserve |
| **unreclaimed bytes** | journal bytes still on disk whose extents are released or superseded; what capacity triggers measure, beside dirty bytes | [§10](#10.%20Failure%20model), [RFC 1 §7](rfc-1-journal.md#7.%20Capacity) |
| **repack reserve** | journal space set aside outside every share's limit for repack's copies, at least one segment's live payload, so space recovery never waits on a share at its limit | [§8.2](#8.2%20Reclaim), [RFC 1 §8.2](rfc-1-journal.md#8.2%20Repack) | [RFC 1 §8.2](rfc-1-journal.md#8.2%20Repack) |
| **sweep** | delete a remote block that nothing references; the only operation that destroys a last copy, carried out by retirement and the deleter. [RFC 12](rfc-12-snapshots.md) also uses it for deleting unlisted blocks at a backup's block folder | [§8.3](#8.3%20Sweep) |
| **retire** / **resurrect** | move a block record to `retired` in the transaction that leaves its count of referenced chunks at zero / bring it back to `live` when a clone, copy or restore references one of its chunks again | [RFC 9 §1.2](rfc-9-gc.md#1.2%20Words%20this%20document%20uses) |
| **trash** | the wait between a block's retirement and its deletion, 48 hours by default; it postpones a delete the refs already allow and never allows one | [RFC 9 §3.7](rfc-9-gc.md#3.7%20Trash) |
| **delete** | (file) remove its last entry; its refs go at release, and nothing leaves the remote tier / (block) `retired` to `deleted`, done only by the deleter after checking the reverse ref index, then the object is deleted | [§7](#7.%20Mutation%20and%20removal), [RFC 9 §3.5](rfc-9-gc.md#3.5%20The%20deleter%20verifies%20before%20it%20deletes) |
| **loss event** | an extent the journal stopped holding without being asked, dropped as corrupt or stale; a loss of content not yet offloaded raises the loss generation | [RFC 1 §3.8](rfc-1-journal.md#3.8%20Loss%20events) |

### Clients and protocols

| Term | Means | Defined in |
| --- | --- | --- |
| **share** | one exported file tree, with its own settings, snapshots and journal limits | [RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities) |
| **export policy** | a share's admission and identity-mapping rule: authentication flavours, client rules, squashing. Applied by the filesystem service on every call; file permission checks never see it | [RFC 16 §2.3](rfc-16-metadata-store.md#2.3%20Server-wide%20and%20control-plane%20entities) |
| **share grant** | one principal's access to one share (none, read, read-write, admin), evaluated on every call before the file's mode or ACL. Not export policy | [RFC 7 §7.4](rfc-7-namespace-metadata.md#7.4%20The%20identity%20arrives%20resolved) |
| **admission** | the per-share checks every call passes before the permission check: the share's state, its flavours, its client rules, then squashing | [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees) |
| **pseudo-filesystem** | the read-only tree of directories through which an NFSv4 client reaches each share's root, built the same on every node from the share list | [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees) |
| **adapter** | the code for one wire protocol: framing, compounds, replay caches, error codes, and nothing else | [RFC 17 §4.2](rfc-17-vfs.md#4.2%20Translation%20stays%20in%20adapters) |
| **chokepoint** | the one function in the metadata store that decides file permissions; the filesystem service is its only caller | [RFC 17 §4.6](rfc-17-vfs.md#4.6%20One%20chokepoint) |
| **client ID** | one client instance as its protocol names it; a client that reboots is a new one, which is how its stale state is recognised and released | [RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client) |
| **client lease** | how long the server keeps a silent client's state. Not a node lease, nor an SMB lease | [RFC 14 §4.1](rfc-14-open-state.md#4.1%20A%20client%20lease) |
| **open** / **anonymous open** | one open of one file, with the access it was granted and the access it denies / what an NFSv3 read or write runs against instead: the caller's identity, no state | [RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open), [RFC 17 §4.1](rfc-17-vfs.md#4.1%20The%20operation%20set%20is%20the%20union%2C%20not%20the%20intersection) |
| **open state** | who holds a file open, locked or cached; the only thing besides a name that keeps a file alive | [RFC 7 §4.2](rfc-7-namespace-metadata.md#4.2%20Open%20state%20is%20the%20second%20holder) |
| **deny mode** | the access an open denies to later opens, checked once, when an open is granted | [RFC 14 §6](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open) |
| **byte-range lock** | a lock on a range of a file, held by a lock owner; SMB locks are mandatory, NFS locks advisory | [RFC 14 §2.3](rfc-14-open-state.md#2.3%20Lock) |
| **caching grant** | the server's promise that nobody else is using a file — an NFS delegation, an SMB oplock or SMB lease — taken back by a recall within a bounded time | [RFC 14 §5](rfc-14-open-state.md#5.%20Caching%20grants) |
| **durable** / **persistent open** | an SMB open kept across a network disconnect for its timeout, never across a restart of its primary / one that also survives a restart or failover, because it is written down; every write on it is stable | [RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens) |
| **delete pending** | SMB's delete on close: the name stays until the last open closes, and new opens are refused meanwhile | [RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close) |
| **grace period** | after a primary loses open state, at least one lease period in which only former holders may take state, by reclaiming it. Reclaim completion is tracked per (client, shard, grace instance), and a restart or failover opens a new reclaim phase. GC's trash is not a grace period | [RFC 14 §4.2](rfc-14-open-state.md#4.2%20Grace%20makes%20volatile%20state%20safe) |
| **write verifier** | the NFS value returned by a write and a commit, derived from the node, its node epoch, the process instance, the shard's incarnation and the journal's loss generation; a change makes the client resend unflushed writes. Not derived from the shard epoch | [RFC 17 §5.8](rfc-17-vfs.md#5.8%20The%20write%20verifier) |
| **loss generation** | a per-journal counter, monotonic for the life of the process across reopens of the journal, raised only on a loss of extents not yet offloaded and on every failed sync window; folded into the write verifier | [RFC 1 §3.8](rfc-1-journal.md#3.8%20Loss%20events) |

### Snapshots and backups

| Term | Means | Defined in |
| --- | --- | --- |
| **cut** / **snapshot** | a number marking one instant of a share / the share as it was at one cut, read-only | [RFC 12 §2.2](rfc-12-snapshots.md#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree) |
| **cut number** | a per-share counter raised by one at each snapshot; snapshot *k* is the share as of the instant it became *k* | [RFC 12 §2.2](rfc-12-snapshots.md#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree) |
| **born**, **died** | the cut number a version was committed under, and the one its replacement was; snapshot *k* sees a version when born < *k* ≤ died | [RFC 12 §2.2](rfc-12-snapshots.md#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree) |
| **history** / **history ref** | versions a live snapshot can still see after the share replaced them / a ref among them, counted like a live one; the chunk's count does not change when a ref moves into history | [§7](#7.%20Mutation%20and%20removal), [RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref) |
| **snapshot hold** | the journal keeping a flushed but not yet offloaded version that a cut sees, until it is offloaded | [RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history) |
| **use record** | a durable mark on a snapshot while a clone, restore, backup or move reads it; the snapshot cannot be deleted meanwhile | [RFC 12 §3.2](rfc-12-snapshots.md#3.2%20A%20backup%20holds%20its%20snapshot) |
| **clone** | (share) a new writable share made from a complete snapshot, in the snapshot's namespace / (file) a copy of an extent of a file made by copying refs, not bytes; a server-side copy works the same way | [RFC 12 §2.6](rfc-12-snapshots.md#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace), [RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy) |
| **catalog backup** | an export of one snapshot's metadata to a backup location, encrypted and authenticated under the namespace's keys; the content stays in the namespace, held by a use record on the snapshot |
| **backup location** | where backups are written, described by its own configuration record: a credential reference, a lifecycle age and a **mode**, `mutable` or `immutable`. An immutable location requires versioning and compliance-mode object lock with retention at least the backup's, and is read by object version ID | [RFC 12 §3.4](rfc-12-snapshots.md#3.4%20Copying%20backups), [RFC 13 §2.5](rfc-13-configuration.md#2.5%20A%20backup%20location%20is%20its%20own%20record) | [RFC 12 §3.1](rfc-12-snapshots.md#3.1%20A%20backup%20is%20an%20export%20of%20one%20snapshot%27s%20metadata) |
| **copying backup** | a catalog backup that also copies, byte for byte, every block its snapshot names into a block folder at the location, so it survives losing the namespace's bucket | [RFC 12 §3.4](rfc-12-snapshots.md#3.4%20Copying%20backups) |
| **claim** | the control object in a namespace's folder naming the one installation that may write and collect there | [RFC 12 §4.1](rfc-12-snapshots.md#4.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim) |
| **move** (namespace) | handing a namespace, with its shares' metadata, from one installation to another on the same bucket; the blocks stay where they are. Not a move of files between shards | [RFC 12 §4.2](rfc-12-snapshots.md#4.2%20The%20move%2C%20step%20by%20step) |
| **re-home** | moving one share out of a shared namespace into a new one of its own by copying its content, while the share serves | [RFC 12 §4.7](rfc-12-snapshots.md#4.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace) |

### Components and mechanisms

| Term | Means | Defined in |
| --- | --- | --- |
| **filesystem service** | the one protocol-neutral interface every adapter calls; it orders each client operation and routes it to the primary that serves it | [§1.3](#1.3%20The%20layers), [RFC 17](rfc-17-vfs.md) |
| **engine** | the content path at a node: it drives the journal, carver, syncer and block metadata, and decides their policy; one per node, with a share context per share | [RFC 8 §2.3](rfc-8-engine.md#2.3%20One%20engine%20per%20node%3B%20a%20share%20is%20a%20context) |
| **policy component** | a small decision function in the engine — when to offload, whether to fill, what to evict; the mechanism it picks is safe on its own | [RFC 8 §3](rfc-8-engine.md#3.%20Policy) |
| **carver** / **chunker** | the component that cuts a stretch into chunks and hashes each / the boundary function it runs | [RFC 2 §1.2](rfc-2-carver.md#1.2%20Two%20layers%3A%20the%20chunker%20and%20the%20carver) |
| **block assembler** | the part of the carver that packs whole chunks into block plans | [RFC 2 §5](rfc-2-carver.md#5.%20The%20block%20assembler) |
| **syncer** / **uploader** / **fetcher** | the component that moves blocks to and from the remote tier / its half that puts whole blocks / its half that gets chunks or whole blocks | [RFC 3 §1.2](rfc-3-syncer.md#1.2%20Two%20halves%2C%20one%20component) |
| **flow** | a handle bound to one store and one queue in the syncer's scheduler; the engine opens one per share, GC one to relocate blocks | [RFC 3 §1.3](rfc-3-syncer.md#1.3%20Interface) |
| **worker pool** | per syncer half, the only limit on transfers in flight, and so the memory bound | [RFC 3 §2.1](rfc-3-syncer.md#2.1%20A%20worker%20pool%20is%20the%20only%20concurrency%20control) |
| **demand** / **background** / **speculation** | the syncer's three classes of transfer, served in that order: a get a reader is blocked on; uploads and relocation's reads; a prefetch no reader has asked for yet (read-ahead or pre-warm) | [RFC 3 §2.9](rfc-3-syncer.md#2.9%20Workers%20are%20shared%20fairly%20across%20flows), [§2.3](#2.3%20Operations) |
| **health** | kept per remote store and per direction, from probes and recent failures; an unhealthy direction refuses calls at once | [RFC 3 §2.8](rfc-3-syncer.md#2.8%20An%20unhealthy%20store%20refuses%20work) |
| **block codec** | the code above the store that encodes blocks, decodes them and verifies every chunk it returns | [RFC 4 §3.4](rfc-4-remote-tier.md#3.4%20Every%20read%20is%20verified%20by%20the%20codec) |
| **capability check** | the test against the real service, run every time a remote store opens | [RFC 4 §4.11](rfc-4-remote-tier.md#4.11%20A%20store%20checks%20its%20service%20before%20it%20opens) |
| **transform** / **chain** | an invertible step over one chunk's bytes, which may decline a chunk it cannot help / the configured transforms, at most one per stage, in the fixed order compress, encrypt, redundancy | [RFC 5 §2.1](rfc-5-transforms.md#2.1%20A%20transform%20acts%20on%20one%20chunk), [RFC 5 §2.3](rfc-5-transforms.md#2.3%20The%20chain%20order%20is%20fixed) |
| **envelope** | the few bytes at the head of each stored chunk body listing which transforms were applied | [RFC 5 §2.4](rfc-5-transforms.md#2.4%20Every%20body%20records%20what%20was%20applied) |
| **chunk-ID key** | the per-namespace key under which every chunk ID of the namespace is a keyed hash, created with the namespace and never changed, so a reader of the bucket cannot confirm a known file's content | [§2.1](#2.1%20Entities), [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets) |
| **material** / **chain ID** / **census** | what a transform needs from outside the body, such as a key / a hash of everything in the chain that decides a body's bytes, part of every block name / the per-block record of which transforms and material its bodies used | [RFC 5 §2.5](rfc-5-transforms.md#2.5%20Reading%20needs%20no%20configuration%2C%20only%20material), [RFC 5 §2.8](rfc-5-transforms.md#2.8%20The%20chain%20ID), [RFC 5 §5.3](rfc-5-transforms.md#5.3%20Retiring%20material%20or%20a%20transform%20needs%20a%20census) |
| **GC** (garbage collection) | the component that retires, holds in trash and deletes blocks nothing references, and compacts mostly-dead ones | [RFC 9](rfc-9-gc.md) |
| **deleter** / **compactor** | GC's part that deletes a due retired block after checking the reverse ref index / GC's part that rewrites the live chunks of mostly-dead or small blocks into new blocks, deleting nothing itself | [RFC 9 §3.5](rfc-9-gc.md#3.5%20The%20deleter%20verifies%20before%20it%20deletes), [RFC 9 §4](rfc-9-gc.md#4.%20Compactor) |

### Configuration

| Term | Means | Defined in |
| --- | --- | --- |
| **bootstrap** | the few facts a host must hold to reach the control plane, and nothing else | [RFC 13 §2.2](rfc-13-configuration.md#2.2%20A%20host%20holds%20only%20its%20bootstrap) |
| **scope** | what a setting must be the same across: the installation, a node, a namespace, a remote store or a share | [RFC 13 §3](rfc-13-configuration.md#3.%20Scopes) |
| **fixed** | a quantity that is not a setting, because no operator can name a workload its value is wrong for | [RFC 13 §4.1](rfc-13-configuration.md#4.1%20Fixed%20by%20default) |
| **binding class** | what a setting's change does: live, restart, next write, or bound (refused while content exists) | [RFC 13 §5](rfc-13-configuration.md#5.%20Binding%20classes) |
| **secret reference** | the name of a secret sealed under the wrapping key of the role that uses it; configuration never holds the value | [RFC 13 §7](rfc-13-configuration.md#7.%20Secrets) |
| **provisioning file** | an optional file of records, applied through the API, which then refuses to edit what it declares | [RFC 13 §2.4](rfc-13-configuration.md#2.4%20Records%20can%20be%20declared%20in%20a%20provisioning%20file) |

**Owner** in these RFCs means only a file's owner — a user — never a server. The
server that writes a shard is its primary.

---

## 1. Scope

DittoFS stores file content in two tiers. The **journal** is the local tier: it
holds bytes on this machine, one journal per device, each serving the files of
many shares. The **remote tier** holds them durably elsewhere.
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

A component **MUST NOT** import another component in this set. The composition
root is the one exception: it builds the components a node's roles need,
imports them, and is imported by none of them ([RFC 15](rfc-15-topology.md)).

Where a component requires a capability it does not own, it **MUST** declare an
interface for that capability in its own package, named for the need rather than
for the provider. The composition root supplies an implementation at composition
time.
Each component **MUST** build and pass its tests with every such interface
stubbed.

A capability **MUST NOT** be negotiated by type assertion on an interface the
provider does not declare it satisfies. A capability that is absent **MUST**
produce a build failure, not a silent fallback.

An assertion that fails yields a working program with silently degraded
behaviour — an unindexed lookup, a disabled guard — that no test observes; a
declared parameter cannot fail this way. The same holds for configuration: an
invalid setting **MUST** be refused, never replaced by a default.

Conformance is checked by a per-component import-graph test.

### 1.3 The layers

![Adapters call one filesystem service; below it sit the metadata store, open state and the content subsystem, over a transactional KV, journal devices and the remote tier; roles protocol and storage mark which node composes what](img/rfc0-architecture.svg)

Four layers, each calling only the one below:

| Layer | What it does | Specified in |
| --- | --- | --- |
| **Adapters** | speak one wire protocol each: framing, compounds, replay, error codes | RFC 20–22 (planned) |
| **Filesystem service** | the one protocol-neutral API adapters call; orders every client operation across the parts below and routes it to the primary that serves it | [RFC 17](rfc-17-vfs.md) |
| **Metadata store**, **open state**, **content subsystem** | the store holds every fact about files, content, identity and configuration ([RFC 6](rfc-6-block-metadata.md), [RFC 7](rfc-7-namespace-metadata.md), [RFC 16](rfc-16-metadata-store.md)); open state holds opens, locks and caching grants ([RFC 14](rfc-14-open-state.md)); the content subsystem is the content data path, made of the engine ([RFC 8](rfc-8-engine.md)), the journal, carver, syncer and GC ([RFC 1](rfc-1-journal.md)–[3](rfc-3-syncer.md), [RFC 9](rfc-9-gc.md)) | as named |
| **Persistence** | a transactional KV (embedded on one node, replicated in a cluster), journal devices, the remote tier | [RFC 16](rfc-16-metadata-store.md), [RFC 1](rfc-1-journal.md), [RFC 4](rfc-4-remote-tier.md) |

One binary serves every deployment. Which layers a node composes is set by its
**roles** ([RFC 15](rfc-15-topology.md)):

- `protocol` — adapters and the filesystem service. It holds no state of its
  own and forwards each call to the primary that serves it.
- `storage` — the metadata store's view, open state and the content subsystem
  with its journals. Only a node with this role can be a shard's primary or replica.

The default is both roles in one process, where every arrow in the picture is a
local call; a split deployment runs protocol nodes in front of storage nodes
over one replicated KV, and the same interfaces cross the network.

**Each shard has one primary** ([RFC 11](rfc-11-ownership.md)). On a single node that node is
the primary of every share; in a cluster, a primary holds one epoch, live while
its node's lease is **(cluster)**. That primary holds the shard's namespace records, its open state and its
content, so the conflict check for an I/O and the I/O itself run in one place,
and no operation on one file is split across two primaries. A file is never split
across shards; per-file and range shards, which would stripe one large file
across nodes, are deferred ([RFC 11 Appendix C](rfc-11-ownership.md#Appendix%20C%20%E2%80%94%20later%3A%20per-file%20and%20range%20shards)).

### 1.4 The single-node profile

The first release is one node with both roles, an embedded metadata store and
no replicas. This section is normative for it. A rule anywhere in the set marked
**(cluster)**, and every rule of [RFC 10](rfc-10-journal-replication.md),
[RFC 11](rfc-11-ownership.md) and [RFC 15](rfc-15-topology.md) the table below
does not name, binds only once a second storage node can serve a share, and is
outside the first release's conformance. Those three RFCs carry the status
`deferred`.

| Concern | Single node: binds the first release | Cluster only |
| --- | --- | --- |
| composition | one process with both roles, built by the composition root ([RFC 15 §2](rfc-15-topology.md#2.%20Roles)); every arrow in [§1.3](#1.3%20The%20layers) is a local call | protocol-only nodes, the route envelope, forwarding, floating addresses |
| shards | each share is one shard, and the node is its primary | subtree and per-child shards, the slot table, moves ([RFC 11 §2](rfc-11-ownership.md#2.%20Shards)–[§4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) |
| shard record | one per share, naming the node as primary with the journal that serves the share, and an incarnation raised each time a journal begins serving it | replicas, replica count and floor, epoch changes by handover and takeover |
| node epoch | raised by one at every start, in the transaction that records the start; no lease is renewed | the node lease, its renewal, takeover, grace after takeover |
| fence records | not consulted: one process orders every commit | [RFC 11 §8](rfc-11-ownership.md#8.%20Metadata%20consistency) |
| put intents | at start, before its first offload, the node abandons every put intent recorded under its shards ([RFC 6 §7.6](rfc-6-block-metadata.md#7.6%20Put%20intents)): it is the only writer, so every attempt an earlier process began has ended | an intent is abandoned once its domain's epoch is superseded |
| self-fence | once the embedded store has completed no transaction for 30 s — the default request deadline ([RFC 17 §4.3](rfc-17-vfs.md#4.3%20Errors%20are%20neutral%20values)) — the node stops acknowledging writes and stability points, answering retry-later ([§10.3](#10.3%20Every%20wait%20on%20a%20request%20ends%20at%20a%20deadline)), and keeps serving reads. A store stall is a local fault, reported as a health condition of the node, never a lease loss: it clears when the store answers, with no new node epoch and no grace | at half the node lease, as a lease loss |
| write verifier | derived from the node, its node epoch, the process instance, the shard's incarnation and the journal's loss generation ([§4.2](#4.2%20The%20residency%20function)) | the same inputs, at whichever node is primary |
| version order | a journal raises its counter above a share's version floor before serving a share it did not serve when it opened ([§2.1](#2.1%20Entities)) | the same rule at every takeover and move |
| lost journal device | the shares it served are refused until an operator acknowledges the loss ([§10](#10.%20Failure%20model)) | a replica holding the content takes over with no operator |

> decision: on one node a store stall fences only after 30 s, not at half a
> lease, because no successor exists that a slow node could race: the fence
> bounds how many writes are acknowledged whose existence cannot commit, not who
> may write the share. Overturned by a measured stall distribution on the
> embedded store whose tail makes 30 s of such writes exceed the journal's
> headroom, or by a second storage node, which brings the lease back.

| Rule | Check — fails on a design without the rule |
| --- | --- |
| intents abandoned at start | Crash the node between an intent and its put; restart. Assert every intent the earlier process recorded is abandoned before the first offload and its object is collected. A design that waits for an epoch to supersede them leaks them, since on one node no epoch rises. |
| self-fence threshold | Stall the embedded store for 10 s; assert writes are still acknowledged. Stall it for 31 s; assert writes and stability points answer retry-later, reads are served, the health condition names the store, and once the store answers, writes resume with an unchanged verifier. A design that fences at half a lease, or as a lease loss, fails the first half or the last. |
| node epoch at start | Restart the node. Assert the node epoch rose by one and the write verifier changed. |
| incarnation | Attach a share to a second journal whose counter is below the share's version floor. Assert the incarnation rose, the verifier changed, and a write after the attach reads back rather than the ref imported with the share. |

## 2. Terminology

### 2.1 Entities

**File** — a namespace entry with an identity, attributes, and an ordered list
of chunk references.

**FileAttr** — a file's size, owner and group, mode, timestamps and identifiers.

**Chunk** — a run of bytes identified by its **chunk ID**: a keyed BLAKE3-256
hash of its content under the namespace's **chunk-ID key**, the same function in
keyed mode and at the same speed. The key is created with the namespace, from the
first release, and never changes, so one namespace names one content one way and
a reader of the bucket who knows a file's content cannot compute its chunk IDs to
confirm that it is stored. Whether a namespace encrypts is likewise fixed when it
is created ([RFC 5](rfc-5-transforms.md)). Chunks are content-addressed,
reference-counted, and shared: one chunk **MAY** be referenced by many files.
Boundaries are content-defined (FastCDC), so inserting bytes early in a file
re-cuts only the chunks around the insertion.

Chunk boundaries are sought between configured bounds, `Min` and `Max`. The
bounds limit where a boundary may be *placed*; they do not bound chunk size
absolutely. The final chunk of a file is emitted whole at whatever size remains,
so a file smaller than `Min` is exactly one chunk. **Content MUST NOT be padded
to a chunk size.**

**A chunk whose bytes are all zero is a hole.** Where one is cut, the file
records a hole instead of a ref ([RFC 6](rfc-6-block-metadata.md)); no all-zero chunk is ever stored or
counted, and its extent resolves as **Absent**, which reads as zeros.

**ChunkRef** — one file's use of one chunk at one offset. A file's content is
fully described by its ordered list of chunk refs. Many refs **MAY** name one chunk.

**Version** — the content version of a file's bytes: a number that orders every
write and removal of one file, assigned when the operation is staged by the
journal that serves the file ([RFC 1 §5.3](rfc-1-journal.md#5.3%20Versions)). It is 128 bits, an epoch half and
then a counter half, compared as one number; in this format the epoch half is
zero, so the counter alone orders it. Where two operations cover the same byte,
the higher version wins, whatever order they arrived in. A chunk ref records the
versions of the content it was committed from ([RFC 6 §4.4](rfc-6-block-metadata.md#4.4%20Commits%20for%20one%20file%20apply%20in%20order)).

A file's versions are compared across journals — a ref committed under one
against a write staged by another — so the order **MUST** survive a change of
journal. **Before a journal serves a share it did not serve when it opened** —
attached at runtime by a move, a recovery, a clone or a restore — **it MUST raise
its counter above that share's version floor** ([RFC 6 §8.3](rfc-6-block-metadata.md#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor)), the highest
version metadata records for any file of the share. Without it the journal can
stage a new write below a ref imported with the share; the write is
acknowledged, stabilised and offloaded, and every read returns the older content.

Three orders are easily confused and never compared with one another: the
content version (per file, above), the journal's sequence number (per journal,
the order records were appended), and the cut number (per share, which snapshots
see a ref, [§7](#7.%20Mutation%20and%20removal)).

**Block** — the unit of remote storage: a whole number of chunks in one object,
addressed by one remote key and written by one put. A get retrieves chunks of a
block, or the whole block. A block ends once it reaches a configured target —
4 MiB by default, settable from 1 MiB to 64 MiB — or once it holds the format's
maximum number of chunks, whichever comes first ([RFC 2 §5](rfc-2-carver.md#5.%20The%20block%20assembler), P2). It
**MAY** pass the target by at most one chunk, because **a block boundary is
always a chunk boundary**; a block ended by the chunk count, and the last block
of an offload pass, **MAY** fall short. A chunk **MUST NOT** span two blocks ([§2.2](#2.2%20How%20a%20file%20relates%20to%20its%20chunks)).

**Segment** — a local append-only file of records, capped at a configured size,
sealed when full and immutable thereafter.

**Record** — one framed run of bytes within a segment, carrying a header with
its identity, length and checksum.

**Extent** — a contiguous `(offset, length)` region of one file's bytes. This is
the only term for that concept: "range", "region" and "span" are not separate
terms and **MUST NOT** be introduced as though they were. Protocol names that
contain one — a byte-range lock, a key range in the metadata store — keep their
own meaning. An extent is a value,
not a thing that is stored; where a component keeps state about one, it keeps it
*against* the extent.

A **block** and a **segment** are unrelated groupings ([§2.2](#2.2%20How%20a%20file%20relates%20to%20its%20chunks)).

### 2.2 How a file relates to its chunks

A file does not contain its content. It contains an ordered list of references to
chunks, and the chunks are shared.

![A file's ordered chunk refs pointing into a shared, reference-counted chunk pool; two versions of a file share the chunks either side of an edit](img/rfc0-files-and-chunks.svg)

The refs are ordered and belong to the file. The chunks are content-addressed and
belong to nobody: the same chunk is named by every ref whose content hashes to
it, and its refcount is how many refs those are. This is the whole of sharing —
a clone, a snapshot and, once it is added, deduplication ([§3.1](#3.1%20Deduplication)) all
work the same way: no copying step and no second representation, only a second
ref naming a chunk that already exists.

Because boundaries are content-defined, an edit disturbs only the chunks it
falls inside. Editing the middle of a file re-cuts the middle chunk; the chunks
either side keep their hashes and stay shared. Under fixed-size chunking the
edit would shift every boundary after it, so every subsequent chunk would hash
differently and nothing downstream of the edit would dedup.

#### A chunk is never split across blocks

Chunks are grouped into blocks for transfer. The grouping accumulates chunks
until the running total reaches the configured target, and then **ends at that
chunk's boundary** — so a block is usually a little larger than the target,
never a little different in composition. A block is smaller only when it reaches
the format's chunk count first, or ends its offload pass.

![Chunks accumulating to a target, drawn at 8 MiB for legibility (the default is 4 MiB): the crossing chunk is included whole, making the block 9.6 MiB, versus the forbidden alternative of cutting that chunk at exactly 8 MiB](img/rfc0-block-packing.svg)

*The figure draws an 8 MiB target for legibility; the default is 4 MiB.*

Overshoot is the price of a rule that holds everywhere else. If a chunk could be
cut to make a block an exact size, then:

- its hash would name content living in two blocks, so **one hash would no
  longer be one locator** — and a chunk's ID being its address is what makes
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
| **offload** | the journal's pass: offer dirty extents, accept remote-durability reports. Not a client's flush (NFS `COMMIT`, SMB `FLUSH`, `fsync`), which only syncs the journal ([RFC 8 §5.1](rfc-8-engine.md#5.1%20Commit%20is%20answered%20by%20the%20journal)) |
| **chunk** | place one content-defined boundary |
| **box** | group chunks into one block |
| **put** / **get** | make remote-durable / retrieve from the remote tier |
| **fill** | place retrieved remote bytes into the journal |
| **demand** fetch | a get a reader is waiting on |
| **speculate** | get a block no reader has asked for yet: **read-ahead** (following an observed access pattern) or **pre-warm** (an explicit request over a share or subtree) |
| **evict** | release local bytes that are remote-durable ([§8.1](#8.1%20Evict)) |
| **reclaim** | recover local space without losing content ([§8.2](#8.2%20Reclaim)) |
| **sweep** | delete a remote block that nothing references ([§8.3](#8.3%20Sweep)) |

These words are disjoint and **MUST NOT** be used interchangeably. In
particular, *evict*, *reclaim* and *sweep* differ in what they may destroy:
eviction destroys a local copy, reclamation destroys nothing, and sweep destroys
the last copy.

## 3. Identity

A file's identity is its `ID`, a UUID unique across the store and stable for
the life of the file: it survives rename, relink and every rewrite of content.
The journal keys content by it.

The journal's file identifier **MUST** be a distinct type constructible only
from a file's `ID`, so that a value of another kind cannot reach it by
conversion. It
does not encode the file's share: one journal serves many shares, and the share
is looked up, never derived from the identifier.

### 3.1 Deduplication

**The first release does not deduplicate across files.** Offload always carries
the chunks it cuts: it never asks whether a chunk is already stored and never
references one in place of uploading it. Chunks stay content-addressed and named
by their chunk ID, which is still verified on every read, and refcounts stay,
because clones and snapshots share chunks.

**Identical bytes from two files name one chunk record.** A chunk record is keyed
by its chunk ID alone ([RFC 6 §4.1](rfc-6-block-metadata.md#4.1%20What%20one%20commit%20records)), so when two files carry identical bytes,
both commits carried the bytes and both refs name one record. The record keeps
the block of the first commit that carried the chunk. A later commit carrying the
same chunk while that block is live adds its ref to the record and does not
repoint it, so its own copy is unreferenced in its block from the start and is
left for the compactor. The second file's bytes are then read from the first
carrier's block. That is safe: a block is not deleted while a referenced chunk's
record locates the chunk in it (I3), and once the block is retired a commit
carrying the chunk repoints the record to its own block instead of resurrecting
the retired one ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence)). A chunk record is therefore reached through
the refs of every file whose offload carried its bytes, their snapshot history,
and clones and server-side copies made from an existing ref
([RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy)); no commit depends on a lookup having been right.

> decision: one copy per chunk ID is trusted. The record keeps the first
> carrier's block, and a later carrier's identical copy is never a fallback.
> Pointing each commit at its own copy would need one record per (chunk, block)
> and a count per copy. It holds while the first carrier's block is protected by
> I3 and by the repoint on retirement; overturned if a live block can become
> unreadable without being retired, which would make a second copy worth keeping.

Offload never resurrects a block: a commit that
carries a chunk whose record names a retired block repoints the record to its
own block instead ([RFC 6 §7.2](rfc-6-block-metadata.md#7.2%20Adoption%20is%20conditional%20on%20existence)), so once a block's count has reached zero
no offload raises it again, and sweep never races a reference arriving through
offload. Clones,
copies and restores still raise counts from existing refs, and the
retire-and-resurrect protocol stays for them ([§8.3](#8.3%20Sweep)).

> decision: cross-file deduplication is off in the first release. Its oracle is
> the one component where a single wrong answer — a chunk reported stored that
> is stored nowhere — loses data rather than costing bytes
> ([RFC 8 §6.5](rfc-8-engine.md#6.5%20The%20dedup%20oracle)). Content-addressed chunks and refcounts keep it
> re-addable without a format or record change; the deferred design is
> [RFC 6 §8.2](rfc-6-block-metadata.md#8.2%20Deduplication%20lookup) and RFC 8 §6.5. Overturned by a deduplication ratio
> measured on a real customer corpus that pays for the risk, or by customer
> demand for it. Chunk IDs are keyed per namespace from the first release
> ([§2.1](#2.1%20Entities)), so adding deduplication later needs no migration of any
> stored name.

**Deduplication, when added, is per chunk.** A chunk whose ID is already known
is referenced rather than stored or transferred again, and its refcount
increases. It catches any overlap between files, whole or partial; there is no
second, whole-file mechanism. Nothing in the first release may close this
path: no record may bind a chunk to one file, and no rule may assume a chunk
record is reached only through its own file's refs.

## 4. Residency

### 4.1 The two oracles

Content location is described by two independent sources, each authoritative for
exactly one question.

> The **journal** answers: *do I hold these bytes on this disk, and at what
> offset?*
>
> The **metadata store** answers: *does this extent exist, which chunk covers it,
> which block holds it, and is that block remote-durable?*

The journal **MUST NOT** record whether content exists or whether it was ever
written, and **MUST NOT** be consulted about remote durability — it is not
authoritative for it. What it holds is still evidence: until a write's existence
is committed to metadata ([§5.1](#5.1%20Write)), the bytes the journal holds are the only
record that the write happened, and the engine treats them as such. It **MAY** record that it has been *told* an extent is
remote-durable, for the two internal purposes [RFC 1 §2](rfc-1-journal.md#2.%20The%20model%20it%20presents) permits: selecting offload
candidates, and refusing an unsafe release. That record is never an answer.

The metadata store **MUST NOT** record where bytes sit on local disk.

**Neither oracle consults the other.** The journal requires no interface from the
metadata store and **MUST NOT** acquire one. An extent it does not hold is reported
as absent, with no judgement about why; resolving that absence is the engine's,
in [§6.1](#6.1%20Resolution). A design in which the journal asks what exists reintroduces the single
oracle this section exists to remove.

### 4.2 The residency function

An extent's residency is not stored. It is computed from the two answers. For an
offset, metadata answers one of three classes ([RFC 6 §3.2](rfc-6-block-metadata.md#3.2%20Every%20offset%20is%20in%20exactly%20one%20class)): a **hole**
(or past the end of the file), **uncarved** (written, no current chunk yet), or
**carved** (a chunk covers it; a chunk is recorded only once its block is
remote-durable). Metadata's answer has one input beyond the refs: the file's
**overwrite set** ([RFC 6 §3.3](rfc-6-block-metadata.md#3.3%20Holes%2C%20not%20written%20extents)). Every existence commit writes an overwrite
record for each run of a write it covers that overlaps content whose existence
was committed before it, with the newest version covering that run; a first
write into a hole or past the size writes none. The test is existence, not refs:
committed content may be under an offer still in flight, whose commit can land
after the overwrite's with an older ref. An offset a ref covers is carved only if
no overwrite record newer than that ref's content covers it; otherwise the ref is
stale and the offset is uncarved. A record leaves the set only where refs at or
above its version cover its whole extent, or where the extent became a hole or
lies past the size — never merely because no ref covers it.

| metadata | journal | residency | read behaviour | check |
| --- | --- | --- | --- | --- |
| hole | absent | **Absent** | zeros | a never-written extent reads zeros with no fetch |
| hole | present | **Dirty** | serve locally: a write whose existence is not yet committed ([§5.1](#5.1%20Write)) | an unstable write past EOF reads back before its stability point |
| uncarved, no chunk | present | **Dirty** | serve locally | a stabilised, unoffloaded write reads back |
| uncarved, no chunk | absent | **Lost** | fail | the same write, its journal extent dropped, fails rather than reading zeros |
| uncarved, stale chunk | present | **Dirty** | serve locally | an overwrite of offloaded content reads the new bytes before its offload |
| uncarved, stale chunk | absent | **Lost** | fail; never the stale chunk | the same overwrite, stabilised and its journal extent dropped, fails rather than reading the older chunk; and again with the overwrite staged while an older offer of the extent was in flight, so the older ref commits after the overwrite's existence |
| carved | present | **Resident** | serve locally | an offloaded, unevicted extent reads with no fetch |
| carved | absent | **Remote** | get, fill, serve | an evicted extent is fetched and verified |

Each row's check is driven at the component that consumes the answer
([RFC 8 §7.1](rfc-8-engine.md#7.1%20Resolution%20asks%20the%20journal%20first%2C%20then%20metadata)), and each fails on a design that merges that row into
another: without the overwrite set, the stale-chunk rows are classed carved,
and the last of them serves old data as **Remote**.

Until a write's existence is committed, metadata still calls its extent a hole,
or, for an overwrite of offloaded content, carved. If the journal loses those
bytes to local corruption before the next stability point, the extent reads as
**Absent**, or as the older content. This is the one way an acknowledged write
can read as what it replaced, accepted as the price of group-committing
existence ([§5.1](#5.1%20Write)): the write was never stable, and I1 binds only from its
stability point on. A crash alone cannot cause it, because the journal's record
survives a crash. The journal reports the corruption as a loss event and, since
the extent was not yet offloaded, raises its loss generation, which changes the
NFS write verifier so the client resends ([RFC 1 §3.8](rfc-1-journal.md#3.8%20Loss%20events), [RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more)).
The loss generation never falls within one process, across reopens of the
journal included, so the verifier never returns to a value a client already
holds.

SMB has no write verifier, so it does not use this window: a write on a
persistent open, or on any open of a continuously available share, is stable —
synced before it is acknowledged ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)). Any other SMB open
does not survive a restart of its primary, so a client holding unflushed writes
on it sees its handle fail rather than a silent gap.

> decision: an unflushed write on a non-persistent SMB open has no
> resend signal; its loss to local corruption before a flush is accepted, as a
> local disk's write cache lost before a flush is. Overturned by a workload whose
> clients rely on unflushed writes on non-persistent opens and cannot be moved to
> a continuously available share.

![The two oracles and the five states their answers imply, with journal silence shown as the ambiguity a single source cannot resolve](img/rfc0-residency-join.svg)

A block counts as remote-durable only while it can be decoded. A block whose
material is lost for good ([RFC 5 §2.7](rfc-5-transforms.md#2.7%20Failures)) is not remote-durable, so an extent it covers that the
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
corresponding block is remote-durable. An implementation **MUST NOT** infer
remote durability from the completion of a transfer, the absence of an error, or
elapsed time. Remote durability is reported by the component that observed it,
to the component that records it.

The journal's offloaded bit is set in exactly two ways: by a durability report,
from an offload ([§5.2](#5.2%20Offload)) or from the engine's reseed, and by fill ([§6.2](#6.2%20Fill)),
whose bytes came from the remote tier and are therefore remote-durable by
construction ([RFC 1 §3.4](rfc-1-journal.md#3.4%20Fill)). The journal persists the bit, so it survives a
restart ([RFC 1 §9.2](rfc-1-journal.md#9.2%20Offload%20state%20after%20recovery)).

## 5. The write path

### 5.1 Write

1. The namespace layer authorises the write.
2. The journal stages the bytes and appends a record.
3. The client is acknowledged.

The client **MUST NOT** be acknowledged before the bytes are recoverable from
the journal under the configured durability policy. Content **MUST NOT** be
chunked, hashed or transferred on this path.

The resulting extent is **Dirty**.

The write's **existence** — the file's size, its holes and its mtime — is not
written to metadata per write. It becomes durable at the protocol's stability
point (a commit, a flush, a stable write, or a close where the protocol requires
one), group-committed across files. Until then the journal is the authority for
it: reads see the write through the journal, and after a crash the engine
re-applies existence from the journal before serving ([RFC 8](rfc-8-engine.md)). Truncate,
deallocate, release and clone are not deferred this way; each is a synchronous
metadata operation.

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
    ◄──── returns the extents now remote-durable
journal marks exactly those extents
```

![The write path: client acknowledged from the journal, then a later offload whose callback carves, puts and records durability, returning the remote-durable extents along the edge that is the only path to Resident](img/rfc0-lifecycle.svg)

The callback returns the extents that became remote-durable. The journal **MUST** mark
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

A block's name is not a function of its content alone: each put attempt mints a
fresh name, and records a **put intent** for it before the put
([RFC 6 §7.6](rfc-6-block-metadata.md#7.6%20Put%20intents)). A retry within one attempt reuses its name and writes
the same bytes; a new attempt never reuses an earlier one's name. An attempt that
fails leaves an intent and possibly an object, and GC collects both
([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)).

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

The journal is asked before metadata, so a write can be staged and its
existence committed between the two answers. **A metadata answer carrying an
overwrite record newer than the version the read was resolved at sends the read
back to the journal** for that extent before any outcome is chosen; only a
second miss resolves it as **Lost**. Without the re-ask, bytes safe in the
journal fail as **Lost**.

Questions about where data lies — allocated extents, the next data or hole —
use the same overwrite set: an offset under an overwrite record newer than what
metadata otherwise says lies there is data, even where the older answer is a
hole.

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

**Delete.** The namespace entry is removed. The file's chunk refs are removed,
and each named chunk's refcount decremented, when the file is released: at once,
or, if the file is still open, once the last open state is gone
([RFC 7](rfc-7-namespace-metadata.md)). Deletion **MUST NOT** remove content
from the remote tier; that is sweep ([§8.3](#8.3%20Sweep)), and it is asynchronous.

**Removals are batched.** A truncate, deallocate, release or clone can name more
refs than one metadata transaction may write. Each is recorded first as a durable
removal, in one small transaction that writes no refs; its refs are then dropped
in bounded batches that resume after a crash
([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)). From the first transaction on, the removal
**masks** the refs it has not yet dropped: no read resolves through them, and the
extent reads as the removal left it. A masked ref stays counted until the batch
that deletes it, so a partly applied removal can leak for a while but never
under-count (I10).

**Snapshots keep superseded records.** A snapshot is a versioned view, not a
copy: taking one writes one cut record, and nothing is drained or copied
([RFC 12](rfc-12-snapshots.md)). Refs, and the namespace records — files, entries, ACLs,
extended attributes, stream links — carry the cut they were born after and the
cut they died after. An overwrite, removal or release that replaces
a ref a snapshot can still see moves it into the file's history in the same
transaction, instead of dropping it; a namespace record is superseded the same
way. Content still dirty in the journal at the cut is under a snapshot hold for
that cut: the journal keeps the superseded version until it is offloaded under the
cut. A history ref is counted
like a live one; the chunk's count does not change when a ref moves. Which
snapshots see a ref is decided by the share's cut number, recorded on the ref as
the cut its content's existence commit read and as its successor's ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)), never by a
journal version: versions are per journal, and a share's files may live in
several. Deleting a snapshot drops the history that no neighbouring cut still
sees.

## 8. Reclamation

Three operations recover space. They differ in what they may destroy, and
**MUST** be kept distinct in both code and discussion.

![Evict, reclaim and sweep side by side, keyed by what each may destroy: one local copy, nothing, and the last copy](img/rfc0-reclamation-scopes.svg)

### 8.1 Evict

Evict releases local bytes whose content is remote-durable, transitioning
**Resident → Remote**.

Evicting an extent that is not remote-durable produces **Lost** and is data
loss. An implementation **MUST NOT** evict a **Dirty** extent. This is the
definition of the operation, not a check applied to it.

Eviction **MUST NOT** modify the remote tier.

Eviction writes nothing to metadata ([RFC 8 §10.1](rfc-8-engine.md#10.1%20Eviction%20is%20chosen%20here%2C%20and%20needs%20no%20new%20record)): residency is computed, so
once the journal stops holding durable content, it resolves as **Remote**. Its
one ordering rule is that local bytes **MUST** be released only after the offload
commit that made them remote-durable is itself committed. A release that precedes
it can leave, after a crash, content whose only copy is gone: **Lost**.

**Eviction frees no space by itself.** The journal returns disk space only by
deleting whole segments ([RFC 1 §8.1](rfc-1-journal.md#8.1%20Releasing%20storage)), so an evicted extent's bytes stay on
disk, as **unreclaimed bytes**, until every other record in its segment is also
released or superseded, or repack copies the segment's live records out.
Eviction makes space reclaimable; reclaim makes it free.

### 8.2 Reclaim

Reclaim recovers local space without changing what content exists. A reclaim pass
**MUST** be content-preserving — it changes where bytes are, never whether they
are.

Reclaim is the umbrella; the mechanisms under it are **repack** (copying live
records out of a sparse segment and unlinking it), retiring a segment that holds
nothing, and removing an unattachable file. [RFC 1 §8.2](rfc-1-journal.md#8.2%20Repack) specifies them.

**Repack is the step that frees space**, so nothing a share does may stop it.
Its copies draw on a journal-level **repack reserve**, outside every share's
limit and sized for at least one segment's live payload: a share at its limit
never blocks space recovery for the others. Repack **MUST** verify a source
record's whole payload checksum before copying any part of it, so rotted bytes
never become a valid record, and a copied release or removal record keeps its
original sequence number, so it never outranks a later record after a crash.

**"Compaction" is deliberately not used for any of these.** In an LSM that word
names an operation that merges runs and *drops* superseded entries; in a
log-structured queue it names one that keeps only the latest value per key. Both
discard content. Reclaim never does.

### 8.3 Sweep

Sweep deletes blocks from the remote tier. It is the only operation in this
system that deletes content anywhere, and the only one after which content
cannot be recovered.

Safety rests on reference counting, specified in [RFC 6](rfc-6-block-metadata.md):

- a chunk carries the number of refs naming it: a file's live refs plus the
  history refs its snapshots still see ([§7](#7.%20Mutation%20and%20removal))
- a block carries the number of chunks whose records locate them in it and whose
  refcount is nonzero
- a block is sweepable only when that number is zero

A block **MUST NOT** be deleted while a referenced chunk's record locates the
chunk in it. **Compaction is not a fourth reclaimer.** The compactor
([RFC 9 §4](rfc-9-gc.md#4.%20Compactor)) copies a mostly-dead block's live chunks into a new block
and repoints their records, deleting nothing itself; the source block is then
swept like any other, once I3 allows it. A copy of a chunk whose record locates it elsewhere — the source of
a compaction once its records are repointed, or a second carrier's copy of
identical bytes ([§3.1](#3.1%20Deduplication)) — keeps no block alive.

Reference counts are read at different instants from the state they describe. An
implementation **MUST** ensure that content created or referenced after a sweep
began cannot be deleted by that sweep, even when every individual observation
was correct when made. [RFC 9](rfc-9-gc.md) specifies the protocol: a block is retired in
the transaction that leaves its count at zero, a later reference to one of its
chunks — a clone, a copy or a restore; offload never does ([§3.1](#3.1%20Deduplication)) — brings
it back, and the irreversible step — deleting it — is taken in a
transaction that first checks an index of the references themselves, so a count
that drifted low cannot delete anything. It needs no fence against writers: a name is never put twice
([§5.2](#5.2%20Offload)), so a remote object may be deleted once no block record not yet `deleted` and no put
intent names it. That state is final for metadata: no later commit can record the name, so a delete
that lands late can reach no committed block. A put already in flight can still
land after it, so the transaction that abandons an intent leaves a `retired`
record whose delete waits past the longest put deadline
([RFC 6 §7.6](rfc-6-block-metadata.md#7.6%20Put%20intents)); an object that lands later still is never committed, and
is collected by listing ([RFC 9 §5](rfc-9-gc.md#5.%20Unrecorded%20objects)).

## 9. Invariants

These hold across components. No component can enforce any of them alone.

| # | Invariant |
| --- | --- |
| **I1** | An extent whose write has reached its stability point is never read as zeros, nor as older content that write replaced. Zeros are returned only for **Absent**. |
| **I2** | A **Dirty** extent is never evicted. |
| **I3** | A remote block is never deleted while a referenced chunk's record locates the chunk in it. |
| **I4** | Fill never overwrites content the journal holds. |
| **I5** | Remote durability is reported, never inferred. |
| **I6** | No component imports another component in this set, except the composition root, which composes them ([§1.2](#1.2%20Component%20autonomy)). Adapters import only the filesystem service ([RFC 17](rfc-17-vfs.md)). |
| **I7** | Every stored record has a named reclamation path that holds at the record's maximum size. |
| **I8** | A serialization conflict is retried within the caller's deadline, never surfaced as an I/O error. |
| **I9** | A block name is minted by one put attempt and put by no other; a remote object is deleted only when no block record not yet `deleted` and no put intent names it. |
| **I10** | A removal masks every ref it has not yet dropped from the moment it is recorded, and a ref stays counted until the transaction that deletes it. |

An implementation is conformant when all ten hold under concurrent operation,
across crash and restart, and in every condition in [§10](#10.%20Failure%20model).

Each invariant **MUST** be tested at the component that consumes the data
([test rules](rfc-index.md#Test%20tiers)).

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

**A read that gates a commit MUST conflict with every concurrent write that would
change its result.** Stores differ in what they detect: one tracks point reads
but not range scans, another validates no reads at all and detects only
write-write conflicts and explicit locks. A scan over a key range is therefore
never such a read, and a check that must hold under any supported store is made
on point records written by both sides ([RFC 11 §8](rfc-11-ownership.md#8.%20Metadata%20consistency)).

### 9.3 Where each invariant is tested and observed

An invariant spans components, so its test lives with the component that
consumes the data ([§9](#9.%20Invariants)), and its signal with the component that can see
it break.

| # | Test owned by | Signal owned by |
| --- | --- | --- |
| **I1** | [RFC 8](rfc-8-engine.md): reads of **Lost** and **Remote** extents, including an overwrite of offloaded content whose journal copy is lost, an overwrite staged while an older offer was in flight, a read racing an existence commit, and a write after a share is attached to a journal below its version floor | RFC 8: reads failed as **Lost**, reads failed because the remote tier is unavailable |
| **I2** | [RFC 1](rfc-1-journal.md): `Release` refuses unmarked extents; RFC 8: eviction choice | RFC 1: dirty bytes against held bytes |
| **I3** | [RFC 9](rfc-9-gc.md): sweep against concurrent reference, including history refs a snapshot still sees, a compaction source after its records are repointed, and two files with identical bytes after the first carrier is deleted | RFC 9: blocks swept, deletions refused; [RFC 6](rfc-6-block-metadata.md): count audit mismatches |
| **I4** | RFC 1: `Fill` against a concurrent write | RFC 1: fills refused as older than the file |
| **I5** | RFC 1: marking follows reports only; RFC 8: reports follow durable commits | RFC 1: bytes offered against bytes marked durable |
| **I6** | every RFC: its own import test | the build |
| **I7** | RFC 6, RFC 7, RFC 9: each stored record at its maximum size | the owning RFC: store size under rewrite |
| **I8** | RFC 6, RFC 7: concurrent writers on one key | the owning RFC: conflicts retried, and conflicts surfaced (which must stay zero) |
| **I9** | RFC 9: deletion racing a put and a delayed delete landing after a retry; [RFC 8](rfc-8-engine.md): a crashed attempt's intent is collected | RFC 9: intents abandoned and collected, objects collected by listing (which should stay near zero) |
| **I10** | RFC 6: the model test crashes a removal between batches and reads through it; RFC 8: reads during a partly applied truncate | RFC 6: removals not yet done, and their age |

## 10. Failure model

Every condition below has exactly one specified behaviour.

| Condition | Behaviour |
| --- | --- |
| **Remote tier unavailable** | Writes continue into the journal while capacity allows. No extent becomes **Resident**, so no extent becomes evictable. Reads of **Remote** extents fail; they **MUST NOT** return zeros. |
| **Journal at capacity, remote available** | Capacity triggers measure **unreclaimed bytes** — released or superseded but still on disk — beside dirty bytes. Offload what is **Dirty**, evict what is **Resident**, and repack the segments whose live share is smallest, so the space returns ([§8.2](#8.2%20Reclaim)); repack draws on the repack reserve and so runs even with every share at its limit. A write refused because the journal is full is transient: it answers retry-later (`ErrDelay`) before the caller's deadline runs out. A write refused by a share's limit or a quota answers no space (`ErrNoSpace`) or over quota (`ErrQuota`) at once ([§10.3](#10.3%20Every%20wait%20on%20a%20request%20ends%20at%20a%20deadline)). |
| **Remote tier slow** | The remote accepts transfers but drains slower than writes arrive, with no error to act on. Writes are paced to the measured drain rate ([RFC 8 §10.2.1](rfc-8-engine.md#10.2.1%20Writes%20are%20paced%20before%20the%20limit%2C%20not%20stopped%20at%20it)); each waits at most until its deadline ([§10.3](#10.3%20Every%20wait%20on%20a%20request%20ends%20at%20a%20deadline)) and is then refused. Progress is not a reason to keep waiting: a drain that frees a trickle never runs a writer out of time otherwise. |
| **Journal at capacity, remote unavailable** | Refuse the write with retry-later while unreclaimed bytes can still be repacked; once every local extent is **Dirty**, with no space repack can return, answer no space (`ErrNoSpace`). I2 forbids evicting a **Dirty** extent, so refusal is the only behaviour that does not lose data. |
| **Metadata unwritable** | Writes are still staged and acknowledged from the journal, until the node fences itself ([§1.4](#1.4%20The%20single-node%20profile)); the next stability point fails and is reported as failed ([§5.1](#5.1%20Write)). Truncate, deallocate and the other synchronous operations fail. Offload fails, so extents stay **Dirty** and the journal fills until writes are refused ([§10.1](#10.1%20Capacity%20is%20a%20bound%2C%20not%20a%20target)). Reads continue while metadata is readable. |
| **Crash** | On restart the journal rebuilds its placement index from its segments; a torn tail — the records of the newest segment after the last that verifies, unless a later record proves them synced — is treated as never written: never served, never reported as damage, and never appended after ([RFC 1 §9.3](rfc-1-journal.md#9.3%20Torn%20and%20corrupt%20records)). Metadata recovers by its backend's own durability, and the engine re-applies from the journal the existence of writes not yet committed, before serving ([§5.1](#5.1%20Write)). A removal recorded but not done masks until its batches resume, and an unfinished clone resumes before its destination is served ([§7](#7.%20Mutation%20and%20removal)). A put whose attempt died leaves an intent, abandoned at the next start on a single node ([§1.4](#1.4%20The%20single-node%20profile)) and, in a cluster, once its domain's epoch is superseded ([§5.2](#5.2%20Offload)). Otherwise the two recover independently and **MAY** disagree; [§10.2](#10.2%20No%20state%20requires%20intervention%20to%20leave) applies. |
| **Local content corrupt** | The journal drops only the extents backed by records that fail verification ([RFC 1 §9.3](rfc-1-journal.md#9.3%20Torn%20and%20corrupt%20records)). A dropped extent that was remote-durable resolves as **Remote** and is fetched again, and raises no loss generation. One that was **Dirty** resolves as **Lost** after its write's stability point; before it, the extent reads as it did before the write ([§4.2](#4.2%20The%20residency%20function)), and the loss generation rises so the NFS write verifier changes. The rest of the segment stays usable and reclaimable. |
| **Journal device lost** | The device or its filesystem is gone, or the journal found at open is not the one recorded. Content already remote-durable is unaffected and resolves as **Remote**. On a single node the engine refuses every share the journal served, naming each, and opens no new journal on the device until an operator acknowledges the loss ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)); afterwards content never offloaded resolves as **Lost**, never zeros, the shares attach to a new journal under §2.1's version-floor rule, and their incarnation rises, so the write verifier changes. **(cluster)** A shard whose content a replica's journal holds is taken over by that replica with no operator action; only a shard no other journal holds waits for the acknowledgement. |
| **Node lost** | On a single node, a node that restarts is **Crash**; a host that does not come back takes its journal device with it, which is **Journal device lost**. **(cluster)** A replica takes the node's shards over once its lease has lapsed plus the drift bound ([RFC 11 §9](rfc-11-ownership.md#9.%20Failure)); acknowledged writes survive in the replicas' journals. |
| **Partition** | On a single node, a partition from the remote tier is **Remote tier unavailable**; the embedded metadata store cannot be partitioned from its node. **(cluster)** A node cut off from the metadata store fences itself at half its lease and its shards fail over where the store is reachable; a primary cut off from a replica cannot acknowledge until that replica is removed ([RFC 11 §9](rfc-11-ownership.md#9.%20Failure)). |
| **Metadata store stalled** | The store answers nothing rather than an error. On a single node it is **Metadata unwritable** while it lasts, and after 30 s the node fences itself as a local fault, not a lease loss: writes and stability points answer retry-later, reads continue, and the node resumes with no new node epoch when the store answers ([§1.4](#1.4%20The%20single-node%20profile)). **(cluster)** A node that cannot renew fences itself at half its lease, and its shards fail over. |
| **Journal sync fails** | The device reports a failed sync for a window of appended records. The journal fails that window at once: it drops the window's records from its index as a loss event and raises its loss generation, and every `Sync` waiting on the window fails, so the client's stability point fails and the NFS write verifier changes. A later sync of the same file covers only what was appended after, so it is never wedged by the failed window ([RFC 1 §6.3](rfc-1-journal.md#6.3%20A%20failed%20sync)). |
| **Material unavailable** | The material provider cannot supply a key ([RFC 5 §2.7](rfc-5-transforms.md#2.7%20Failures)). Behaves as **Remote tier unavailable**: offload cannot encode and reads of **Remote** extents cannot decode. Material lost for good is not this row: its blocks are not remote-durable ([§4.2](#4.2%20The%20residency%20function)). |

### 10.1 Capacity is a bound, not a target

The journal's capacity limit is enforced by a reservation taken before a write
is accepted ([RFC 1 §7](rfc-1-journal.md#7.%20Capacity)); a limit tested without reserving bounds nothing.

Refusal at the limit is a specified outcome, not a failure of the design. The
alternative — accepting content that cannot be made durable and cannot be
released — has no exit.

### 10.2 No state requires intervention to leave

Every condition in this section **MUST** resolve on its own once the underlying
cause is removed, with one exception: a lost journal device on a share no other
journal holds waits for an operator to acknowledge the loss. An implementation
**MUST NOT** have any other state reachable by normal operation from which it
cannot return without operator action.

> decision: a lost journal device, on a share no replica holds, waits for an
> operator. Serving the share at once would turn every write not yet offloaded
> into a failed read that nobody was told to expect; refusing until the loss is
> acknowledged makes it a decision someone made. Overturned by shares whose
> owners would rather lose unoffloaded writes than wait — scratch space, caches —
> which would make the acknowledgement a per-share setting.

In particular: sustained inability to offload **MUST** be reported as a health
condition of the share, and **MUST NOT** be represented only as log output.

A count found about to go below zero is corruption ([RFC 6 §6.3](rfc-6-block-metadata.md#6.3%20Underflow%20is%20corruption%2C%20not%20a%20boundary)), and it
fails its transaction. It **MUST NOT** wedge the file or the share: the failure
schedules a targeted recount of the chunks it names, and the operation retries
once the recount has corrected them.

Recovery after a crash is required to be *truthful*, not to make the two oracles
agree. A chunk that metadata knows about and whose bytes did not survive is
**Lost**, and reads of it fail. Reconciling the two by assuming agreement
reintroduces the failure this model exists to prevent.

### 10.3 Every wait on a request ends at a deadline

Several rules in this set bound a wait by "the caller's deadline": a conflict
retry ([§9.2](#9.2%20Conflicts%20and%20their%20retries)), a write paced or refused at capacity ([RFC 8 §10.2](rfc-8-engine.md#10.2%20A%20capacity%20refusal%20comes%20back%20here)), a
cold read ([RFC 8 §7.5](rfc-8-engine.md#7.5%20An%20unreachable%20remote%20fails%20the%20read%2C%20distinguishably)). The bound is worth only what sets it, so:

- every operation that reaches the engine on behalf of a client **MUST** carry a
  deadline. The protocol front end sets it from the request; an operation that
  arrives with none **MUST** be given a stated default at the filesystem service
  ([RFC 17](rfc-17-vfs.md)), so "bounded by the caller's deadline" is never "unbounded";
- every wait on that operation's path **MUST** end at the deadline: a queue, a
  reservation, a retry, and a lock. A lock that cannot be abandoned **MUST** be
  held only for work bounded independently of any remote call, and the bound
  stated at the lock;
- a wait **MUST NOT** restart its budget because something made progress. A
  budget refreshed on progress is unbounded against a remote that drains a
  trickle;
- the refusal **MUST** say why. A write refused because the journal is full —
  transient, since offload or repack will free space — reaches the client as
  retry-later (`ErrDelay`, which the adapters map to `NFS4ERR_DELAY`,
  `NFS3ERR_JUKEBOX` and, for SMB, a request held pending), answered before the
  caller's deadline runs out; one refused by a share limit or a quota reaches
  it as "no space" or "over quota" at once; neither is an I/O error, so the
  client can tell a full store from a broken one.

Background work — offload, sweep, repair — is not a client request and **MAY**
wait longer, under its own stated bound.

## 11. Open questions

1. **What a retried transaction may close over** ([§9.2](#9.2%20Conflicts%20and%20their%20retries)) — a retried closure
   re-runs against state that changed since it was first called. Whether it may
   close over values read before the transaction opened, or must re-read every
   row it modifies, is not settled. One that closes over pre-read state can
   re-propose the decision the conflict was raised to prevent, which delays a
   lost update rather than preventing it.
2. **Fill policy** ([§6.2](#6.2%20Fill)) — filling is discretionary, and [RFC 8](rfc-8-engine.md) proposes a
   policy. Which one is right on real workloads is unmeasured.
3. **Eviction granularity** ([§8.1](#8.1%20Evict)) — this document constrains eviction by
   remote durability, not by unit; the unit is [RFC 1](rfc-1-journal.md)'s to choose. The right segment size
   is unmeasured, as is repack's write amplification now that storage is freed only in whole segments
   ([RFC 1 §12](rfc-1-journal.md#12.%20Open%20questions)).
