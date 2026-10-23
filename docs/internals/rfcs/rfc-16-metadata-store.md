---
rfc: 16
title: "RFC 16 — metadata store"
component: metadata store
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-6-block-metadata]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-13-configuration]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
aliases:
  - RFC 16
tags:
  - rfc
---

# RFC 16 — metadata store

**Status:** draft.
**Audience:** anyone adding an entity, a record kind, a backend or a consumer
of the metadata layer, and anyone debugging what it holds.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). Signatures are indicative; the rules in §1.2 are normative.

A rule or record marked **(cluster)** binds only once a second node can serve a
share; what the first release, one node, follows instead is
[RFC 0's single-node profile](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile).

---

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

### In plain words

The metadata store is DittoFS's memory for everything except the bytes inside
files. It knows which files exist, what they are called, who may use them, and
where their bytes went. Think of a library's card catalogue: the books sit in
the stacks, and the cards say what exists and where to find it. Unlike a drawer
of cards, every change is a transaction: all of its records land, or none do.
Each caller gets a narrow view of the store: the filesystem service, the engine,
GC (the garbage collector, which deletes blocks nothing uses) and the management
API each see only what they need. One small interface hides the database
underneath: an embedded one on a single node, a replicated one in a cluster.

```text
 file bytes:      journal ──► blocks in the bucket
 everything else: one transactional store (this RFC)
```

DittoFS keeps every fact it must remember, other than file content, in one
transactional database: files and directories, which chunks hold each file's
bytes, the open state that must survive a restart, users, shares, nodes and
settings. This RFC specifies that database as a whole: the **entities** callers
read, the narrow **views** each caller is handed, and how entities are laid out
as keys and values in a transactional key-value store (a **KV**) that one small
interface hides.

In outline: clients write over NFS (Network File System) or SMB (Server Message
Block); writes land in a local journal and are later cut into chunks, packed
into blocks and uploaded to an S3 (Simple Storage Service) bucket
([RFC 0](rfc-0-data-lifecycle.md)). The metadata store records what exists and
where it went. What each record *means* is owned elsewhere — files and names by
[RFC 7](rfc-7-namespace-metadata.md), content by
[RFC 6](rfc-6-block-metadata.md), opens and locks by
[RFC 14](rfc-14-open-state.md); this RFC owns how they are stored together.

### The problem, in one example

On node N1, alice's folder `profiles/alice/` is directory `d1`, and her profile
disk `ODFC_alice.vhdx` is file `f7`. Follow two operations into the database.

1. **alice-pc overwrites 64 KiB at 4 GiB, then flushes.** The write itself
   touches no metadata: only the journal holds it. The flush runs one
   transaction that rewrites `f7`'s one File record (times, change counter;
   the size is unchanged), moves its version-floor entry to the new counter, and adds an overwrite record saying "this 64 KiB was
   replaced", because older content there was already committed. The flush
   waits for that transaction, since it overwrites committed content; an append
   would have been left to the next group commit. It also
   *guards* `f7`'s fence record and N1's node record: claims on keys it does
   not write, which fail the transaction if another node took the file over
   after the transaction's snapshot.
2. **Minutes later the extent is uploaded.** A put intent naming the new block
   is recorded before the put. After the put, the offload commit writes a ref
   ("these bytes are chunk C"), the chunk record, the block record and the
   index entries beside them. It never touches `f7`'s File
   record or its overwrite record, so uploads and alice's writes never contend
   for one key.
3. **alice-pc creates the folder `Downloads`.** One transaction writes the new
   directory's File record, one Entry under `d1` keyed by the keyed digest of
   the folded name, then the folded name `downloads`, and holding `Downloads`, a small *delta* record for `d1`'s
   times, and a usage delta for the new file. It guards `d1`'s record rather than rewriting it.

Two designs this avoids, both measured. Rewriting `d1`'s times in place makes
every create in the folder write one key, and parallel creates in one directory
fell from 111k/s to 28k/s; a delta per transaction, folded in later, kept the
full rate. Keeping a file's refs as a list inside its record makes each upload
re-read and re-write the whole list, which is how a long sequential write
slowed down as the file grew; one key per ref keeps each commit's cost to what
changed.

![The filesystem service holds Namespace, Files and Capacity; open state OpenRecords; the engine Existence and Content; GC Blocks; authentication Principals; the management API ControlPlane; internal tools Inspect. Adapters hold only vfs.Service. Every view sits on one metadata store of entities, codecs, key layout and transaction-level helpers, which calls only the small KV interface (Update, View, Limits; Get, Scan, Before, Versions, Set, Delete, Guard, GuardRange, Now), backed by an embedded store on one node or a replicated one in a cluster](img/rfc16-views.svg)

![The keys the example writes, grouped by scope. Under F‖profiles‖f7 the flush writes the File and an overwrite record and the offload writes a ref; under d1 and d9 mkdir writes the new directory's File, the Entry keyed by the folded name and a time delta. mkdir guards d1's File record; the flush guards f7's fence and N1's node record. Per share, the flush moves the version-floor entry and mkdir writes a usage delta. Per namespace, the offload writes a put intent, then chunk, block and reverse-ref records. Server-wide records hold users, shards and settings; the store format record is read first](img/rfc16-keys.svg)

Everything about one file sits under `F‖` with the share's ID and the file's
ID, so a client operation stays inside one or two such prefixes and a share's
files are one prefix to walk, though deleting them is not a range drop: the
counts their refs name live outside it (§2.3.2). Per-share records sit under `S‖`; content records
under the bucket namespace they are counted in (`C‖`, `B‖`, `CR‖`);
installation-wide records under their own kinds. [§4.2](#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)
lists every key.

### The words you need

- **Entity**: a plain value a read returns, such as a File, an Entry or a Share
  ([§2.1](#2.1%20The%20entity%20map)). One entity is not one record: what a
  write touches is decided by the operation, never by the struct.
- **Record**: one key and its value in the KV; every key has a row in the key
  table ([§4.2](#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed)).
- **KV**: the small interface a backend implements — run a transaction, read,
  scan, set, delete, guard ([§4.1](#4.1%20One%20small%20interface%20per%20backend)).
- **Guard**: a transaction's claim on a key it does not write, or on every key
  under a prefix (a range guard). It aborts its own transaction if the key was
  written after that transaction's snapshot; it never stops the writer, and
  never conflicts with another guard.
- **Delta and fold**: a counter changed by many writers is never read and
  rewritten; each transaction adds a delta record and the shard's primary folds
  them in later ([§4.4](#4.4%20Counters%20that%20many%20writers%20change)).
- **View**: the narrow interface one consumer is handed — the engine, GC and
  the filesystem service each hold different ones ([§3](#3.%20Interfaces%2C%20by%20consumer)).
- **Format record**: the one record read first, naming the store format and
  the case-folding rules; a binary that does not know them refuses to open the
  store ([§4.6](#4.6%20Store%20format)).

### What this RFC promises

- One database holds every kind of metadata, so there is one thing to run, back
  up, replicate and test.
- Reading a file's attributes costs what [§4.2](#4.2%20Keys%3A%20per-file%2C%20per-share%2C%20content-addressed) states, never more; no record holds a list
  that grows with a file or a directory.
- No counter is a hot key: usage and directory times are written as deltas, so
  parallel creates in one directory do not serialise.
- A serialisation conflict is retried within the caller's deadline, never
  returned as an error.
- A store opens only under a binary that understands its format and fold
  rules, and a value written in an unknown codec version is refused, never
  guessed.

### Scenarios

**alice-pc and build01 each create a file in `profiles/alice/` at the same moment.**

1. Each create is one transaction. It guards the directory's record, a claim on
   a key it does not write, instead of rewriting it
   ([§4.1](#4.1%20One%20small%20interface%20per%20backend)).
2. Two guards on one key never conflict, so both transactions commit
   ([§4.1](#4.1%20One%20small%20interface%20per%20backend)).
3. Each writes its own Entry, and a directory-time delta under a key unique to
   its transaction ([§4.4](#4.4%20Counters%20that%20many%20writers%20change)).
4. Later the primary of the directory's shard folds the deltas into the
   directory's record, at a moment when no create there is in flight
   ([§4.4](#4.4%20Counters%20that%20many%20writers%20change)).

Outcome: creates in one busy folder run in parallel; measured, the deltas kept
the full rate where rewriting the directory cut it from 111k/s to 28k/s.

**On a case-insensitive share, alice-pc creates `Report.docx` while build01 creates `REPORT.DOCX`.**

1. The share's fold rule turns both names into one key, `report.docx`
   ([§4.6](#4.6%20Store%20format)).
2. Both transactions write that one Entry key. The store sees a write-write
   conflict and aborts whichever commits second
   ([§4.1](#4.1%20One%20small%20interface%20per%20backend)).
3. `Update` retries the loser within its caller's deadline, against a fresh
   snapshot ([§4.1](#4.1%20One%20small%20interface%20per%20backend)).
4. The retry reads the entry the winner wrote, so the second create finds the
   name taken.

Outcome: one entry, spelled as the winner spelled it, and no client ever sees a
raw conflict error.

**N1 loses power just after a flush is reported done.**

1. The flush's transaction returned only once its writes were synced to N1's
   device ([§4.1](#4.1%20One%20small%20interface%20per%20backend)).
2. Other commits ready at that moment shared the same sync (group commit), and
   none was acknowledged before it ([§4.1](#4.1%20One%20small%20interface%20per%20backend)).
3. On restart, every transaction that was reported done is in the store. The
   journal may already have released bytes because of one; that is safe.
4. A commit whose `Update` returned an error may or may not have landed;
   callers treat it as unknown, never as refused
   ([§4.1](#4.1%20One%20small%20interface%20per%20backend)).

Outcome: nothing reported as committed is lost, so nothing built on a commit
has to be undone.

**An operator removes the share `profiles`.**

1. If a snapshot cut is running, `DeleteShare` answers "retry later". If
   snapshots exist, the request has to say whether to delete or detach them
   ([§2.3.2](#2.3.2%20Removing%20a%20share)).
2. Otherwise one transaction marks the share `being removed` and frees its name
   and path ([§2.3.2](#2.3.2%20Removing%20a%20share)).
3. From that commit, every call into the share is answered stale, and a new
   share may take the name ([§2.3.2](#2.3.2%20Removing%20a%20share)).
4. The rest runs in the background. Each step is idempotent and recorded, so a
   crash resumes where the record says it stopped
   ([§2.3.2](#2.3.2%20Removing%20a%20share)).

Outcome: clients lose the share at once, and the cleanup finishes on its own,
even across crashes.

**N1 is upgraded to a newer binary, then rolled back.**

1. The new binary reads the format record before any other key, and finds a
   format it can read ([§4.6](#4.6%20Store%20format)).
2. It keeps writing that older format, and records the range it supports in
   its node record ([§4.6](#4.6%20Store%20format)).
3. Rolling back is reinstalling the old binary, which still opens the store
   ([§4.6](#4.6%20Store%20format)).
4. Once an operator raises the format through the control plane's gate, the old
   binary refuses the store and says why ([§4.6](#4.6%20Store%20format)).

Outcome: opening a store never upgrades it; only an explicit raise does.

### How the rest is organised

[§1](#1.%20Purpose) states the three rules that keep the model honest.
[§2](#2.%20Entities) maps every entity to the RFC that defines it and defines
the identity, control-plane and cluster entities. [§3](#3.%20Interfaces%2C%20by%20consumer)
lists the views by consumer and how one store is assembled.
[§4](#4.%20Persistence%20pattern) is the persistence pattern: the KV contract,
the key layout, codecs, counters, debugging and the store format.
[§5](#5.%20Decisions%20and%20evidence) records the decisions and their evidence;
[§6](#6.%20Testing) to [§8](#8.%20Observability) cover testing, benchmarks with
the metadata footprint at 2 PB, and observability. On a first read, skip the
code in §2.3, §5, §7.4 and the appendices.

## 1. Purpose

The metadata layer used to be specified as records: what is stored under which
key. That is the right level for performance and the wrong one for everyone who
uses the layer. This RFC specifies the layer the other way round: the
**entities** a caller works with, the **interfaces** each caller holds, and the
**persistence pattern** that maps entities onto a transactional key-value store.
It is the one place where the whole metadata layer can be read at once.

It owns:

- the rules every entity and interface follows (§1.2);
- the map of every entity and the RFC that defines it (§2.1);
- the identity types, and the server-wide and control-plane entities (§2.2–2.3);
- the views no narrower RFC owns, and how the store is assembled (§3);
- the KV contract, the key layout, codecs, counters and the store format (§4);
- how the store is tested, benchmarked and observed (§6–§8).

### 1.1 Non-goals

This RFC **MUST NOT**:

- restate the semantics of an entity owned elsewhere — what a `File` means is
  [RFC 7](rfc-7-namespace-metadata.md)'s, what a `ChunkRef` means is [RFC 6](rfc-6-block-metadata.md)'s, what an `Open` means is [RFC 14](rfc-14-open-state.md)'s;
- name a storage product in normative text: the `KV` contract (§4.1) is what a
  backend must meet, and products appear only in labelled evidence;
- define authorisation, identity mapping or the management API's behaviour —
  RFC 19, RFC 18 and RFC 23 own those, in that order; this RFC stores
  their entities.

### 1.2 Three rules that keep the model honest

Entities make the layer easy to use and debug. Three rules keep them from
reintroducing the costs the record-level RFCs were written to remove.

**1. One entity is not one record.** An entity is what a read returns. What a
write touches is decided by the operation, not by the struct. `File` is read as
one value; how many records hold it is §4.2's choice, made by measurement, and
a caller never sees it. No interface offers `Put(File)`: a caller that could
write the whole struct would read-modify-write it, and every write would
conflict with every attribute change for fields it never meant to touch. Writes are operations — `SetAttrs`,
`Truncate`, `Link`, `Rename` — and each names the fields it changes.

**2. Collections are iterators, never fields.** No entity holds a list that grows
with its file or its directory: no `File.Refs`, no `Directory.Entries`, no
`Chunk.Files`. Those are reached through `iter.Seq2` over a range. A slice on an
entity is an O(*N*) read hidden in a struct literal, which is how a 160 GB
sequential write decayed when every commit loaded and re-encoded the file's
whole ref list ([RFC 6 §5.2](rfc-6-block-metadata.md#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed)).

**3. Interfaces follow consumers, not entities.** Each caller declares the
narrow view it needs ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)): the namespace, the engine, GC and the adapters
each hold a different subset. There is no per-entity CRUD interface, and no
interface exists to have one method per record.

## 2. Entities

All entities live in one package. Each is a plain struct with no
behaviour beyond pure methods (§2.4), and each renders as JSON for debugging
(§4.5).

### 2.1 The entity map

| Entity | What it is | Defined in |
| --- | --- | --- |
| `File`, `FileType` | anything a name resolves to: regular file, directory, symlink, device, FIFO, socket, named stream; its size and times included | [RFC 7](rfc-7-namespace-metadata.md) |
| `Entry` | one name in one directory | [RFC 7](rfc-7-namespace-metadata.md) |
| `ACL`, `ACE` | a file's one access-control list | [RFC 7](rfc-7-namespace-metadata.md) (semantics: RFC 19) |
| `Xattr` | one small named value on a file | [RFC 7](rfc-7-namespace-metadata.md) |
| named streams | a `File` of type `Stream` with `StreamOf` set | [RFC 7](rfc-7-namespace-metadata.md) |
| `FilesystemInfo`, `Capabilities`, `Usage`, `PrincipalUsage`, `Quota` | what a share reports about itself, and its limits | [RFC 7](rfc-7-namespace-metadata.md) |
| `Attrs` | the fields a `SetAttrs` changes | [RFC 7](rfc-7-namespace-metadata.md) |
| `ClientRecord`, `OpenRecord`, `LockRecord`, `DeleteRecord` | open state's durable records, plain structs here; the runtime `Client`, `Open`, `Lock`, `CachingGrant`, `Watch` that wrap them are open state's own | [RFC 14](rfc-14-open-state.md) |
| `ChunkRef`, `Chunk`, `Block`, `ChunkHash`, `BlockName`, `JournalVersion`, `SnapshotCut` | the content map, the blocks it lives in, and the cut numbering snapshots | [RFC 6](rfc-6-block-metadata.md) |
| `FileID`, `ShareID`, `Principal`, `PrincipalID` | identity | §2.2 |
| `User`, `Group`, `Membership` | principals with names | §2.3 |
| `Share`, `ShareGrant`, `Snapshot` | a share, who may reach it, its snapshots | §2.3 |
| `Node`, `Shard`, `SlotTable` | cluster membership and shard placement ([RFC 11](rfc-11-ownership.md)) | §2.3 |
| `Setting`, `Secret` | configuration values and sealed credentials ([RFC 13](rfc-13-configuration.md)) | §2.3 |
| `Operation`, `IdempotencyRecord`, `Session`, `APIToken`, `AuditRecord`, `AuditCursor`, `NodeStatus` | the management API's records (RFC 23) | §2.3 |

Holes, removals, fences, cuts, put intents, the GC index keys and
the chunk change stamp are [RFC 6](rfc-6-block-metadata.md)'s bookkeeping for its guarantees. They are
records, not entities: they stay behind the interfaces of §3.

### 2.2 Identity types

```go
type (
	FileID      [16]byte // RFC 0 §3: UUID, never reused, stable across rename and relink
	ShareID     [16]byte
	PrincipalID [16]byte // minted, never reissued, exactly as FileID is
)

// Principal is a protocol-neutral identity: a user, a group, a machine or a
// well-known principal (RFC 18 §2.1). Its ID is
// minted when the principal is created and never reissued, so a UID, GID or
// SID reused by a later account cannot inherit an old principal's files,
// ACE grants or charge. Protocol spellings are held only on the User or Group
// record (IDs) and in the PX‖ index that maps each back to its principal
// (§4.2); no file, ACL or usage record holds one, and this layer compares IDs
// and never interprets a spelling.
type Principal struct {
	Kind PrincipalKind // User, Group, Machine (RFC 18 §2.4), Special (a well-known principal: a fixed ID, below)
	ID   PrincipalID
}
```

**Well-known principals have fixed IDs.** A stored ACL entry names its
principal by ID, so every well-known principal an entry can name has one, fixed
for all installations: fifteen zero bytes, then the byte below. No minted ID
takes that form, since a minted ID is a random UUID with its version bits set.
Which principal each is, and which callers it matches, is
[RFC 18 §2.1](rfc-18-identity.md#2.1%20Kinds%20and%20well-known%20principals)'s;
this table is only the encoding.

| Byte | Principal |
| --- | --- |
| 0x01 | owner (`OWNER@`) |
| 0x02 | group (`GROUP@`) |
| 0x03 | everyone |
| 0x04 | owner rights |
| 0x05 | creator owner |
| 0x06 | creator group |
| 0x07 | authenticated users |
| 0x08 | administrators |
| 0x09 | system |
| 0x0A | network |
| 0x0B | built-in users |
| 0x0C | anonymous |
| 0x0D | root |

uid and gid 0 are root's and 65534 anonymous's, held by rule and never as `PX`
rows; a `PutUser`, `PutGroup` or `Minter` write naming either is refused
`ErrInvalid`.

Every other identity type sits beside the only entities that use it: the
content identities (`ChunkHash`, `BlockName`, `JournalVersion`) and
`SnapshotCut` with the content entities in [RFC 6](rfc-6-block-metadata.md#2.%20The%20records), which defines them once.

### 2.3 Server-wide and control-plane entities

The control plane persists in the same database as file metadata, through the
same `KV` and the same codecs (§4). One database means one thing to run, back
up, replicate and make highly available, and one conformance suite; a second
database kept only for users and shares would double every one of those.

These entities differ from the file, open-state and content entities ([RFC 7](rfc-7-namespace-metadata.md), [RFC 14](rfc-14-open-state.md), [RFC 6](rfc-6-block-metadata.md)) in **scope**, and the difference decides
what moves with a share:

| Scope | Entities | Moves with a share export ([RFC 26](rfc-26-catalog-backups.md)) |
| --- | --- | --- |
| Per share | Share, ShareGrant, ExportPolicy, Snapshot | yes |
| Server-wide | Installation, User, Group, Membership, Netgroup, Node, NodeLease, NodeStatus, Shard, Setting, ShareList, and the management API's Operation, IdempotencyRecord, Session, APIToken, AuditRecord and AuditCursor | no — an export carries the principals its files, grants and export policy reference — the policy's `Anonymous` among them (§5, decision 8) — and every netgroup its policy references, with its members, so an import can refuse one that collides ([RFC 26 §3.2](rfc-26-catalog-backups.md#3.2%20Import%20is%20staged%20and%20published%20atomically)) |

```go
// User and Group are principals with a name. A file refers to them only by
// Principal (§2.2), never by name, so renaming a user touches no file and
// deleting one leaves its principal in ACLs as an unknown principal, which is
// how both protocols already report it.
type User struct {
	Principal  Principal
	Name       string
	IDs        ProtocolIDs // UID, SID, Kerberos principal, directory entry ID, OpenID Connect (issuer, subject): RFC 18 §2.2's stored spellings; each also a PX‖ row
	Email      string      // optional, never defaulted, never a spelling: no PX‖ row, no lookup by it (RFC 18 §2.2)
	Primary    Principal   // the group a new file gets (RFC 7 §2.4, RFC 18); zero, or a group deleted or being deleted: none, and a new file's group is then its creator's own principal
	Disabled   bool
	Secret     SecretRef   // by reference, never the credential itself (RFC 13)
	// Role, NotBefore and MustChange are the management API's (RFC 23 §5):
	// they grant nothing on files. A user with no Role has no API role of its
	// own; an OpenID Connect sign-in may still give its session one from the
	// issuer's role claim (RFC 23 §5.5), so no Role does not mean no API access.
	Role       APIRole     // none, viewer, operator, admin
	NotBefore  time.Time   // store time: an access or API token issued before it is refused; raised by a disable, a role or password change, or a sign-out everywhere
	MustChange bool        // set with a generated password: until it changes, a sign-in permits only the password change and sign-out
	Generation uint64      // raised by every PutUser; the management API's entity tag (RFC 23 §4.4)
}

type Group struct {
	Principal  Principal
	Name       string
	IDs        ProtocolIDs // GID, SID, directory entry ID; each also a PX‖ row
	Generation uint64      // raised by every PutGroup, as User's is
}

// A machine principal (RFC 18 §2.4) is held as a User record whose Principal
// has kind Machine: no Name, so no NX‖ row; one spelling, its krb row; no
// Primary and no Secret. It is minted by the first Kerberos authentication of
// its name, in one transaction with that PX‖krb row.

// Membership is one member of one group, its own record in both directions:
// a group of 10^5 users is not a list on the group (§1.2 rule 2).
type Membership struct {
	Group  Principal
	Member Principal // a user or a nested group
}

// Share is one exported filesystem. Name is what an SMB client tree-connects
// to; Path is where it sits in the NFS namespace, the MOUNT argument and the
// NFSv4 pseudo-filesystem path. Both are indexed and unique, both may change
// (§2.3.1), and neither appears in a handle, which carries ID.
type Share struct {
	ID     ShareID
	Name   string
	Path   string // absolute, normalised: "/photos", "/home/alice"
	Root   FileID
	Config SettingsRef
	Generation uint64 // raised by every PutShare: S‖id‖info's, not the state's or the namespaces' (RFC 23 §4.4)
	// Tag is the share's journal tag (RFC 1 §3), minted from the TG counter
	// when the share arrives in the installation and never given to another
	// share; PutShare never writes it. A recovery import opens every adopted
	// journal (after Restamp) before it publishes any share, raises TG above
	// every tag their Stats name, and only then mints tags (RFC 8 §2.5), so no
	// recovered share reuses a tag a journal holds.
	Tag    uint32
	// State is its own record (S‖id‖st), not part of the one PutShare writes:
	// a cut and a deletion change it as well as the management API.
	State  ShareState // enabled, quiesced, being removed, retired (§2.3.1)
	// Namespaces lists the block namespaces the share's refs are counted in,
	// each a (generation, NamespaceID) pair: one outside a re-home, two during
	// one. Write is the generation new put attempts carve under, the write
	// namespace (RFC 27 §2.7). Both are their own record (S‖id‖nsg), written
	// only by a re-home and an import, never by PutShare.
	Namespaces []NamespaceGen
	Write      uint32 // as wide as a ref's nsgen, which RFC 6 §2.1 owns
}

// ShareGrant is one principal's access to one share; RFC 18 §6.4 reads a
// caller's grants on every call and RFC 19 §3.2 applies them as the share
// ceiling. A cache keys on the share's grant-list version, S‖id‖gv (§4.2),
// never on a grant's own Version. A share's default access is a grant to the
// well-known everyone or authenticated-users principal. An admin grant makes
// its principal privileged only as RFC 18 §6.5 decides.
type ShareGrant struct {
	Share     ShareID
	Principal Principal
	Access    ShareAccess // none, read, read-write, admin; none is stored, and is distinct from no grant
	Version   uint64      // the writing commit's; a cache keys on the share's grant-list version, which covers this grant's absence too
}

// ExportPolicy is the share's admission and identity-mapping rule, one per
// share. The filesystem service applies it on every call (RFC 17 §4.9); the
// file checks behind Files.Authorize never see it (RFC 7 §7.4).
type ExportPolicy struct {
	Share       ShareID
	Flavors     []AuthFlavor  // admitted, in preference order: SECINFO's answer
	MinKerberos KerberosLevel // krb5, krb5i, krb5p; zero when Kerberos is not required
	Clients     []ClientRule  // first match decides; no match refuses. Empty admits every client
	Squash      Squash        // none, root, all; root by default (RFC 18 §6.2)
	Anonymous   Principal     // what a squashed, AUTH_NONE or anonymous-session caller acts as; by default the well-known anonymous principal (§2.2, 0x0C). A deleted one fails closed: calls it would serve are refused (RFC 18 §6.2)
	ManageGIDs  bool          // false by default; true replaces an AUTH_SYS caller's gids with its principal's store memberships (RFC 18 §3.2)
	ReadOnly    bool          // false by default; true narrows every caller's share access to read-only at admission, whatever its grant or client rule (RFC 17 §4.9, RFC 7 §7.4)
	SMB         SMBShareFlags // encrypt, hidden from enumeration, continuously available, access-based enumeration (RFC 19 §3.7), guest access (RFC 22 §5.1); all off by default
	Version     uint64        // raised by every change; the authorisation cache key carries it
}

// ClientRule admits or refuses clients by address range or netgroup, and may
// narrow the share grant for them to read-only.
type ClientRule struct {
	Match  ClientMatch // CIDR or netgroup name
	Access ShareAccess // none, read, read-write: never wider than the grant
}

// Netgroup is a named set of hosts, server-wide, referenced by name from
// client rules. A rule naming a netgroup that does not exist fails closed: a
// rule granting access matches no host, and a rule with Access none matches
// every host, so deleting a netgroup a deny names never lets its hosts fall
// through to a later, broader rule.
type Netgroup struct {
	Name    string
	Members []string // host names, addresses or CIDRs
	Version uint64   // the writing commit's change sequence, so a re-created netgroup never repeats a Version an admission cache keyed on (RFC 17 §4.9)
}

// Installation is the installation's identity: one record. ID is minted at
// random by the explicit initialisation step (Init, §3.2), which writes it here and into
// every journal's format file (RFC 1 §3), and is never changed. A start never
// creates this record: a start that finds journals naming an installation and
// no record, or a record and no journal, reports the side that is missing
// ("metadata store lost", or a journal foreign or missing) and waits for an
// operator (RFC 13 §2.2). It is the NFSv4.1 server
// owner's major ID and the server scope on every node (RFC 14 §2.1), and the
// input the pseudo-filesystem's handles digest (RFC 7 §6.1). A store copied to
// make a second installation MUST be given a new one, or two installations
// answer clients as one server.
type Installation struct {
	ID [16]byte
	// Instance is the instance part of the identity in a namespace claim
	// (RFC 27 §2.1): minted afresh whenever a start finds the platform's
	// machine-generation identifier differs from Generation, so a cloned or
	// restored image is a new instance and holds no namespace until an operator
	// says which copy is the installation.
	Instance   [16]byte
	Generation []byte // the platform's machine-generation identifier last seen; empty where the platform offers none, and there only the namespace claim's nonce check (RFC 27 §2.1) catches a copied image
	// Active is the version written for each shared format other than the
	// store format — journal format, block format, settings schema, (cluster)
	// node messages — raised only by the control plane's gate once every
	// registered node's range holds the new version (RFC 13 §5.5), never by a
	// binary opening the store. The store format's one record is \x00format
	// (§4.6), which the same gate raises.
	Active map[Format]uint32
}

// ShareList is one record per installation, raised in the transaction of
// every share create, delete, rename, path change and state change. It is the
// NFSv4 pseudo-filesystem's change attribute (RFC 17 §4.9).
type ShareList struct {
	Version uint64
}

// Snapshot is a named, user-visible cut of a share. SnapshotCut, and the
// born/died rule every versioned record follows, are RFC 6 §6.5's.
type Snapshot struct {
	Share   ShareID
	Cut     SnapshotCut
	Name     string
	State    SnapshotState // cutting, holding, complete, failed, deleting (RFC 12 §2.2)
	Deadline time.Time     // UTC, store time: a cut not committed by then is aborted (RFC 12 §2.3)
	CutAt    time.Time     // UTC, stamped by the cut transaction; names its Previous Versions token (RFC 12 §2.5)
	Ordinal  uint16        // below 2^15, unique among the snapshots visible at any one path, unusable for 24 h after its snapshot is deleted; in its file ids and SMB volume serial (RFC 12 §2.5)
	Locked   time.Time     // UTC lock expiry: no deletion before it; raised, never lowered (RFC 12 §2.7)
	Expires  time.Time     // zero: kept until deleted
}

// Node is one server process in a cluster; Shard is one shard and
// its current primary (RFC 11). Every fenced commit guards its primary's Node
// record (RFC 11 §8), so the record changes only when the node acquires a lease
// or a takeover marks it lapsed; renewals write NodeLease and the node's journal
// generations instead.
type Node struct {
	ID      NodeID
	Address string
	Roles   Roles  // protocol, storage; both, the only accepted value (RFC 15 §2.1)
	Binary  string // the binary version it runs; the replication gate opens only when every node's knows the extension (RFC 10 §2.3)
	Domain  string // (cluster) its failure domain, for slot placement (RFC 11 §2.2)
	Epoch   uint64 // raised at every start on a single node; in a cluster each time the node acquires its lease anew
	Formats map[Format]VersionRange // the versions it reads and writes, per shared format, registered at every start (RFC 13 §5.5)
	Lapsed  bool   // (cluster) marked by a takeover or an address takeover: the lease at Epoch no longer fences anything (RFC 10 §9.2, RFC 15 §5.3)
}

// NodeLease is the node lease's expiry, one per node, not per shard
// (RFC 11 §3.1). Renewals write only this record and the node's journal
// generations (RFC 10 §2.2).
type NodeLease struct {
	Epoch    uint64    // the node epoch it extends
	Expires  time.Time // in store time
	LastSeen time.Time
}

// Shard is a shard record: one primary, one epoch and the primary's
// replicas (RFC 11, RFC 10). It changes only by compare-and-swap. The epoch is
// fenced per file, not here — each file's F_x and F_o records carry it (§4.2,
// RFC 6 §5.4) — so content and namespace commits do not read this record; a
// move between shards (RFC 11 §4), a cross-shard commit (RFC 11 §8.1) and a
// usage fold (§4.4) do. A move's cursor is
// in its own move record (§4.2), so advancing it changes no shard record.
type Shard struct {
	ID           ShardID
	Share        ShareID
	Primary      Replica // node, journal identity and join incarnation (RFC 10 §2.2); JoinPoint and Learner unused
	PrimaryEpoch uint64  // node epoch Primary was named under; a node whose lease lapsed is primary of nothing (RFC 11 §3.1)
	Epoch        uint64  // ownership epoch: fences (RFC 10 §2.1)
	Incarnation  uint64  // shard incarnation: raised each time a node or journal begins serving the shard as primary; feeds the write verifier (RFC 11 §3.1)
	GraceInstance uint64 // set to Incarnation in the transaction of a start or failover that lost the shard's open state, and by compare-and-swap before it serves by a new primary whose handover's table never arrived (RFC 11 §3.1); by nothing else: a handover that ships the table, a batch move and an address arrival keep it. Names the grace instance; a stateid carries its low 5 bits (RFC 14 §2.1, §4.4)
	Replicas     []Replica
	Count        int // configured size of the replica set, primary included
	Floor        int // fewest of primary and replicas, learners included, that acknowledge a write
	Relinquished NodeID // the node that relinquished the shard, barred by node ID whatever its epoch; zero when none (RFC 10 §7.2)
	LossAcked    bool   // the operator acknowledged the lost replicas (AckLost), which lets the relinquisher claim (RFC 10 §7.2); a takeover clears both
}

// Replica is one replica of a shard (RFC 10 §2.1). Node, Journal and
// JoinIncarnation together identify it, so a node that lost its journal, or a
// reused node ID, is not taken for the replica it replaced.
type Replica struct {
	Node            NodeID
	Journal         JournalID // from the journal's format file (RFC 28 §2.1)
	JoinIncarnation uint64    // raised each time the node joins this shard; not the shard incarnation
	JoinPoint       Version   // first version it acknowledges (RFC 10 §7.3)
	Learner         bool      // older content not yet covered: it acknowledges, but never takes over or serves reads
}

// Setting is one configuration record at one scope (RFC 13 §3).
type Setting struct {
	Scope      SettingScope // installation, node, namespace, remote store or share, with the scope's ID (RFC 13 §3)
	Key        string
	Value      []byte       // secrets appear only as references
	Generation uint64       // raised by every change (RFC 13 §2.3)
	Schema     uint32       // the record's schema version; a reader that does not know it refuses the record (RFC 13 §2.3)
}
```

```go
// Secret is one credential: a password hash, an NT hash, a key. The value
// is envelope-encrypted under a key held outside the KV, so a copy of the
// store alone reveals nothing. Referenced by SecretRef from User and Setting.
// Each secret kind is sealed under the wrapping key of the role that needs it
// (RFC 13 §7); every node holds both keys.
type Secret struct {
	ID    SecretRef
	Kind  SecretKind // password (slow hash), nt-hash (only with NTLM enabled), keytab, remote credential, key, node credential (RFC 15 §4.4)
	Version uint64   // raised by every rotation; the previous version stays resolvable until every node reports this one (RFC 13 §7)
	KeyID string     // which wrapping key sealed it: one per role or secret kind
	Value []byte     // sealed
}
```

Every node holds both wrapping keys; they stay separate so a later split of the
roles ([RFC 15 §2.1](rfc-15-topology.md#2.1%20One%20node%20type)) re-seals
nothing. The KV lets any node read the sealed bytes; confidentiality rests on
the keys a node's bootstrap names.

```go
// The management API's records (RFC 23, which owns their meaning, and the types
// named here and not defined). Each is written in the transaction of the
// request it records, alongside the change.
type Operation struct { // RFC 23 §4.6
	ID       OperationID // minted, 16 bytes
	Kind     string      // "share.delete", "user.delete", "share.rehome", ...
	Target   ResourceRef // kind and ID
	Account  Principal
	State    OpState     // pending, running, succeeded, failed, cancelling, cancelled
	Claim    OpClaim     // for work the runner drives: node, node epoch, expiry; zero otherwise
	Step     []byte      // the runner's last durable step; owner-driven work keeps its cursor in its own records
	Started, Updated time.Time
	Error    []byte      // the problem body, when failed
	Result   []byte      // when succeeded
	// Progress is not stored: RFC 23 §4.6 reads it from the owner's cursor; Error is the encoded Problem
}

type IdempotencyRecord struct { // RFC 23 §4.5
	Account Principal
	Key     string
	Digest  [32]byte  // of method, path and body
	Answer  []byte    // status and body as first answered
	Expires time.Time // 24 h after the request
}

type Session struct { // RFC 23 §5.3
	ID        [16]byte
	Account   Principal
	Refresh   [32]byte  // hash of the current refresh token; never the token. It and every hash it replaced are keys under SS‖session‖h (§4.2)
	Expires   time.Time // api.session_lifetime after sign-in
	Revoked   bool
}

type APIToken struct { // RFC 23 §5.3
	ID      [16]byte
	Account Principal
	Hash    [32]byte  // of the token; never the token
	Expires time.Time // no later than api.token_max_lifetime after creation
}

type AuditRecord struct { // RFC 23 §6: append-only; deleted only past audit.retention
	ID      [16]byte
	Time    time.Time // store time of the commit
	Actor   Principal // with the account name, session or token ID; the host for the break-glass reset
	Node    NodeID
	Source  string    // the client's address
	Request []byte    // method, path, request ID, idempotency key
	Target  ResourceRef
	Before, After uint64 // the target's generation
	Change  []byte    // spec fields changed, old and new; a secret field as "set", never a value
	Outcome []byte    // applied, refused with its problem code, or the operation's ID
}

// AuditCursor is the admin stream's export position for one node that takes
// requests (RFC 25 §4.3): how far that node's audit records, read in order
// through AUX‖node, have been appended to a spool. Exactly one holder
// advances it: the node itself while its lease holds; once it is lost, the
// live node that took it, which exports the lost node's records into
// its own spool. Every write of it — a take, a give-back, an advance after a
// synced batch — is a conditional write naming the holder it read, so a
// holder that lost it stops at its next batch. A record exported by both
// holders across a change is a duplicate the consumer drops by request ID.
type AuditCursor struct {
	Node        NodeID    // whose records it walks
	Time        time.Time // the last record exported, by its store time
	Audit       [16]byte  // and ID: the position in AUX‖0x03‖node; both zero before the first
	Holder      NodeID    // who exports them now
	HolderEpoch uint64    // the holder's node epoch at its take; a holder whose epoch has moved on holds nothing
}

// NodeStatus is what one node reports (RFC 23 §2.4), written by that node at
// most once a second and only when a value changed. Every node writes it: it
// is a node record (RFC 15 §2.5).
type NodeStatus struct {
	Node           NodeID
	Settings       uint64 // CFGGEN reconciled through
	ShareList      uint64 // SL reconciled through
	Principals     uint64 // PGEN reconciled through
	Refused        []RefusedRecord // each with the generation and the error
	RestartPending []RecordRef
	Secrets        []SecretVersion // each secret reference it resolves, and the version it holds (RFC 13 §7)
	Conditions     []byte // the node's health conditions (RFC 25)
}
```

Adapter settings and identity-provider configuration follow the same pattern
and are left out of this list; neither changes a file or content key.

#### 2.3.1 Share names, paths and state

- **Names** are compared case-insensitively and are unique under that fold,
  because SMB clients compare them so. A name is at most 80 characters, the
SMB share-name limit (MS-SRVS `NNLEN`), has no
  `\ / : * ? " < > |` or control character, and is not `IPC$`. A name ending in
  `$` is still reachable by name but left out of share enumeration.
- **Paths** are absolute, with no empty, `.` or `..` component, each component a
  valid file name. **No share's path is an ancestor of another's**: shares are
  disjoint trees ([RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem)),
  so `/home` and `/home/alice` cannot both be shares. The components above a
  share's path exist only in the pseudo-filesystem.
- **Rename and re-path** change the index rows and raise `ShareList`, in one
  transaction. Handles, open state and established tree connects name the
  `ShareID` and keep working; only a client that connects or mounts afresh
  needs the new name or path. A durable or persistent open reconnected after
  a rename is reconnected through a connect to the new name.
- **State** gates every call, evaluated with the grant (RFC 17 §4.9). `enabled`
  admits. A cut is not a state: its own gate holds writes briefly and never
  reads ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)), so the share state adds nothing to it. `quiesced`, an
  administrator's pause, refuses with `ErrDelay`, which the client retries.
  `being removed` refuses with `ErrStale`, so handles into a removed share go
  stale rather than turn into an access error. `retired` is a share deleted with
  its snapshots detached ([RFC 12 §2.8](rfc-12-snapshots.md#2.8%20Deleting)): it is in no export list and serves
  no client, refusing every call with `ErrStale`, while its records, FileIDs,
  `Cut` record and detached snapshots stay; they go when its last detached
  snapshot is deleted, and its identity is never reused.
- **An import is a create for these rules.** An import ([RFC 26 §3.2](rfc-26-catalog-backups.md#3.2%20Import%20is%20staged%20and%20published%20atomically))
  **MUST** refuse, before it publishes anything, a share whose name collides
  under the name fold with an existing share's, whose path equals an existing
  share's or is an ancestor or descendant of one, a principal whose name already
  names a different principal, and a netgroup whose name already names one with
  different members. Each refusal names the collision; the import request may
  carry a new share name, path or principal name, applied in the staged copy,
  and nothing is renamed silently. Without this an import can publish a second
  share answering one tree connect, or nest one share inside another's tree.

#### 2.3.2 Removing a share

A share is removed by `DeleteShare`, and removal is one-way: once a share is
`being removed` it never serves again. The state changes are these, each a
`SetShareState` naming the state it expects:

| From | To | By | Refused when |
| --- | --- | --- | --- |
| `enabled` | `quiesced`, and back | the management API | — |
| `enabled`, `quiesced` | `being removed` | `DeleteShare` | a cut is running (`ErrDelay`); a GC pause exists on any namespace the share's `Namespaces` names (`ErrMoving`), whether a move's ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)) or a recovery import's the operator has not yet released ([RFC 26 §2.3](rfc-26-catalog-backups.md#2.3%20Restore)); snapshots exist and the request neither deletes nor detaches them; with `delete`, any use record exists — backup, clone, restore, copy or move ([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot)) — or any snapshot is locked (`ErrLocked`, [RFC 12 §2.7](rfc-12-snapshots.md#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)), since the removal could not finish while either holds a snapshot. `detach` is accepted with locked snapshots, whose locks apply unchanged once detached. A running re-home does not refuse it: the removal ends the re-home (step 1) |
| `being removed`, `retired` | — | `DeleteShare` again | always, with `ErrStale`: the share is already gone to clients |
| `being removed` | `retired` | the removal, when it detached snapshots | — |
| `being removed`, `retired` | no record | the removal's last step; for `retired`, the deletion of its last detached snapshot ([RFC 12 §2.8](rfc-12-snapshots.md#2.8%20Deleting)) | — |

```go
// DeleteShare starts a share's removal and returns once the share is
// being removed: its name and path are free, and every call into it is
// ErrStale. The rest runs in the background and survives a crash.
DeleteShare(ctx context.Context, share ShareID, snapshots SnapshotDisposition, expect uint64) error

type SnapshotDisposition uint8 // refuse (the default), delete, detach
```

**The removal runs in this order.** Each step is idempotent. One
**coordinator** drives it: the primary of the shard that holds the share's root
([RFC 11 §2](rfc-11-ownership.md#2.%20Shards)). Steps 3 and 4 run on every shard, each driven by that shard's
primary, which records the step its shard reached in `SH‖shard‖rm`; every other
step is the coordinator's, recorded in the state record `S‖id‖st`. A crash or a
failover of any of these primaries ([RFC 11](rfc-11-ownership.md)) resumes its part where its
record says it stopped. Once `st` reaches step 3, each shard's primary runs
steps 3 and 4 for its shard; a shard with no `rm` row has not begun. The
coordinator moves `st` past step 4 only once every shard record of the share and
each one's `rm` row has been read with tracking and found done, in the batches
below, never in one transaction over every shard. The set is fixed from step 1 on, since no shard is created for
the share and no file moves between its shards after it (step 1's gates), so
no shard can be left with undropped files while the removal goes on. That set
is the share's shard list (`S‖id‖shs‖shard`, written
with each shard record and deleted with it, [RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards)), and the coordinator advances `st` over it in batches within K
([RFC 6 §5.2](rfc-6-block-metadata.md#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed)), each batch reading its shards and `rm` rows with tracking and
recording its cursor in `st`; `st` moves past step 4 only in the batch that
finds the last shard done. One transaction over 10⁵ per-child shards would read
2×10⁵ tracked keys, past a replicated store's transaction limits.

![DeleteShare is refused, changing nothing, while a cut runs, a GC pause holds a namespace the share names, snapshots exist without a disposition, a delete meets a use record or locked snapshot, or the share is already being removed. Otherwise step 1, one transaction, marks it being removed and frees its name and path, and DeleteShare returns: every call is ErrStale. Step 2 handles snapshots. Steps 3 and 4 run per shard, recorded in SH‖shard‖rm: join, drop open state, offload for detach, forget the tag once every shard is offloaded, then drop files in batches. Steps 5 to 7 delete server-wide rows, the share's records and shards, the state record last or retired with detach, and drain the namespace](img/rfc16-removal.svg)

1. **Mark.** One transaction. It reads with tracking (§4.1) `S‖id‖nsg`, the GC
   pause of every namespace that names, and `S‖id‖rh`, and it `GuardRange`s
   `S‖id‖snap‖` and `S‖id‖use‖`; it refuses as the table says. Then it moves
   the state to `being removed`, deletes `NX‖share‖name` and `NP‖path`, deletes
   `S‖id‖rh` if a re-home is running, which ends the re-home — with `detach` it
   keeps it, marked ended, so `ErrRehoming` still refuses a clone, catalog
   backup or move of the kept snapshots, whose history refs sit at both
   generations, which neither a clone ([RFC 12 §2.6](rfc-12-snapshots.md#2.6%20A%20writable%20clone%20is%20a%20new%20share%20in%20the%20same%20namespace)) nor an export can
   carry. A re-home it ends that has parked refs already has their parking record
   (`NS‖ns‖park‖ShareID`), written by its first parking batch, so their prune
   runs from it once the re-home has ended
   ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)). It then raises `SL`.
   From this commit on, admission answers `ErrStale` for every
   call into the share ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)), the pseudo-filesystem no longer shows it,
   and a new share may take the name. Every gate holds in both commit orders
   ([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries)), because each side reads a key the other writes:
   - RFC 27's pause transaction reads each share's state with tracking and
     waits for a share that is `being removed`;
   - a re-home's start and a cut's announce ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)) read the
     share's state with tracking and refuse a share that is not `enabled` or
     `quiesced`; either one committing first is caught by this step's
     tracked read or range guard;
   - a new use record falls under the `S‖id‖use‖` range guard, and its writer
     reads the state ([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot));
   - a re-home's switch transaction ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace), step 3) reads the share's
     state with tracking and stops for a share that is not `enabled` or
     `quiesced`. Puts it made into the new namespace are orphans that
     namespace's GC collects, and both generations stay in `Namespaces`, so
     step 4 drops each ref at the generation it names and step 7 drains both;
   - setting or extending a snapshot lock reads the share's state with
     tracking and, with `delete`, is refused with `ErrStale` once the share is
     `being removed` ([RFC 12 §2.7](rfc-12-snapshots.md#2.7%20Scheduled%20snapshots%2C%20retention%20and%20locks)), so no lock committing after this
     step stalls step 2 for up to `lock_max`; with `detach` it is allowed;
   - every control-plane write that names the share — `PutShare`,
     `SetShareState`, `SetGrant`, `DeleteGrant`, `PutExportPolicy`, a share-scoped
     `PutSetting` — reads `S‖id‖st` with tracking and refuses a share that is
     `being removed` or `retired` with `ErrStale`, so none leaves a row after
     step 5 has deleted the rest;
   - creating a shard of the share and a move's commit between two of its
     shards ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) read the share's state with tracking and
     refuse a share that is not `enabled` or `quiesced`, so no file lands in a
     shard whose `rm` row already says done.
2. **Snapshots.** With `delete`, each snapshot is deleted through RFC 12's
   serial deletion. With `detach`, nothing is deleted yet: step 4 keeps what the
   snapshots see.
3. **Live state and journal**, per shard. The engine moves the share's storage binding
   to `removing` ([RFC 8 §2.3](rfc-8-engine.md#2.3%20One%20engine%20per%20node%3B%20a%20share%20is%20a%20storage%20binding)), which stops admission and joins every call, clone,
   pass and batch already in flight, and waits for the filesystem service's
   count of calls admitted to the share to reach zero
   ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)), which joins the namespace calls admitted before the
   mark — a late `Create` among them — that never enter the engine. Every open, lock, lock waiter, caching
   grant, watch, layout and copy under the share then ends without a recall
   (`DropShare` with the removed reason, [RFC 14 §11](rfc-14-open-state.md#11.%20Interface)): the adapter is told of each through
   `Revoked`, and the holder's next call is `ErrStale` whatever it holds. No
   record of the dropped state references is kept, so a reference of the removed share is
   later answered `ErrBadState` with no revoked-state flag for the client to clear
   ([RFC 14 §11](rfc-14-open-state.md#11.%20Interface)): a flag nothing could clear would hold the client in a
   `TEST_STATEID` loop for good. With
   `detach`, the primary then offloads, through the still-open syncer flow
   ([RFC 3](rfc-3-syncer.md)), the extents a kept snapshot's hold marks cover, until no hold of a
   kept snapshot remains ([RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)); without it, `Forget` would drop
   the marks with the bytes and leave the snapshot reading content that is
   gone. Each shard's primary records its shard's `rm` row as **offloaded** once
   its join and `DropShare` are done and, with `detach`, once that offload has
   ended; with `delete` there is nothing to offload, and the row goes to
   offloaded at once. `Forget` drops the share's tag whole, in a journal that
   may serve several of the share's shards as primary or replica, so no shard
   forgets before every shard is offloaded: the coordinator moves `st` to a
   forget phase only in a batch that finds every shard's `rm` at offloaded or
   later, as it does for step 4. Then each primary sends its shard's replicated
   `Forget` to itself and every replica ([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)), which writes a
   durable **forget** record for (shard, tag). A node keeps the tag, its flow
   and its handle while any other shard of the share it serves, as primary or
   replica, has not recorded one; when its last has, it closes the flow and the
   handle, **then** runs RFC 1's whole-tag `Forget` ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)), which is
   refused while a handle of the tag is open. A journal without the replication
   extension has no forget record kind: its node tracks the shard forgets in the
   shards' `rm` rows and runs the whole-tag `Forget` before it moves the last
   shard's row to forgotten — the `Forget` is durable and idempotent, so a crash
   between the two runs it again, and a row that says forgotten never stands
   for a tag the journal still holds. **(cluster)** A per-(shard, tag) forget
   record stays live, and repack carries it forward, for as long as the node
   keeps the shard's install record, not only until the whole-tag record
   exists; `ErrForgotten` rests on it
   ([RFC 10 §2.3](rfc-10-journal-replication.md#2.3%20The%20journal%20extension)),
   and a restart between the two still finds every shard recorded. At every start
   a node runs the whole-tag `Forget` again for a tag its journal's `Stats`
   still names whose share's `rm` rows all say forgotten, so no crash leaves the
   tag's bytes dirty until step 6.
   Every other dirty byte is dropped
   without being offloaded. A primary that took the shard over after this step forgets the
   tag again in its own journal before step 4. **(cluster)** Once the share's
   state reaches its forget phase, nothing new reaches the tag: the primary stops
   the shard's tail copies and the repair scheduler joins no node to it; a
   takeover skips the reconcile and the re-issue
   ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover) steps 3 and 4)
   and sends its `Forget` to every member again; and a node holding the shard's forget record refuses
   `Apply` and `Copy` for the shard
   ([RFC 10 §9.2](rfc-10-journal-replication.md#9.2%20Takeover)). Any of the three
   would write bytes under a forgotten tag that no `Forget` reaches until the
   node's next start after step 6. Last, it deletes every `MV`
   record naming its shard as giving or receiving, so no crashed move resumes;
   the ship of a batch that never committed was dropped with the tag.
4. **Files**, per shard. Every file of the shard under `F‖ShareID` is dropped
   in bounded batches. Each ref goes through the batched removal of
   [RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation), so counts fall and GC retires blocks the ordinary way
   ([RFC 9](rfc-9-gc.md)). The batches are metadata transactions alone: the tag is
   forgotten, so no batch takes a journal version or writes a journal record,
   and each file's removal takes as its version one above the share's version
   floor ([RFC 6 §8.3](rfc-6-block-metadata.md#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor)), which covers every ref the file holds since
   no write follows. It is **not** a range drop: the counts and the `CR` reverse
   refs a ref names live outside the prefix. With `detach`, only what no kept
   snapshot sees is dropped ([RFC 12 §2.8](rfc-12-snapshots.md#2.8%20Deleting)).
5. **Server-wide rows that name the share.** Its grants and their `GR` reverse
   keys; share-scoped settings (`CFG`), raising `CFGGEN` in the same
   transaction, and any secret (`SEC`) only those settings referenced; every
   client's `CL‖client‖sh‖shard` for its shards; and any `MV` record. Parked
   refs (`NS‖ns‖pk‖ShareID‖…`) stay until the backups that need them expire
   ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)).
6. **The share's own records, then its shards.** Without kept snapshots, every
   `S‖id‖*` record but `st` is deleted, then the shards' `SH‖shard` records with
   their `rq`, `rqx`, `xh`, `hw`, `fw` and `rm` rows, since they name the primaries that drive
   and resume the removal ([RFC 11 §2](rfc-11-ownership.md#2.%20Shards)). The root's shard record goes last, in
   one transaction with `st`, which raises `SL` and runs step 7's check, so no
   crash leaves shard rows with no record saying a removal is due, or a removal
   finished with its drain check never run. `nsg` is deleted in that same last
   transaction, never in an earlier batch, since step 7's check reads it to know
   which namespaces to drain, and a coordinator resuming between batches needs
   it. With `detach`, the state becomes
   `retired`, and what RFC 12 §2.8 still reads stays: `st`, `info`, `nsg`, an
   ended `rh`,
   `snap`, `cut`, `use`, the history records (`live`, `hd`, `sc`, `hr`) and the
   shard records. Everything else under `S‖id` — export policy, quotas, usage,
   the rest — is deleted. The shard records and the rest go with the last
   detached snapshot, whose deletion runs step 7's check in the same way. The
   `ShareID` and the share's journal tag are never reused.
7. **The namespace.** The drain gate is stated once, in [RFC 9 §7.6](rfc-9-gc.md#7.6%20A%20namespace%20no%20share%20names%20is%20drained),
   and this step runs it: when nothing that gate reads still names the
   namespace, it is drained. The check reads each of them with tracking and
   writes the drain mark in step 6's last transaction. The drain then runs
   RFC 9 §7.6, steps 1–3, as written there.

> decision: removal frees the name at once and the space later. A client sees the share
> gone at the first commit. The bytes stay in the bucket until GC has deleted
> them. Step 7 deletes the key records, which is not crypto-shredding: the keys
> stay, wrapped under a master key, in every export and location `keys` object
> written for the namespace, so a holder of that master key can still read any
> ciphertext that survives ([RFC 9 §7.6](rfc-9-gc.md#7.6%20A%20namespace%20no%20share%20names%20is%20drained)). Destroying keys before GC
> reaches the blocks would also break a retained catalog backup that needs them
> ([RFC 5](rfc-5-transforms.md) T19). Add a `shred` disposition that refuses while any backup
> names the namespace and destroys the master key wrapping its keys, if a
> deployment must make deleted data unreadable.

### 2.4 Methods on entities

Entities carry **pure** methods: no I/O, no context, no store. They exist so
that a rule stated once in these RFCs is written once in code, and every caller
asks the entity instead of re-deriving the rule. A method that needs the store
belongs on an interface (§3), never here. **Every entity follows this rule**;
the methods below are the cross-cutting ones, and each entity's own methods are
listed beside its definition.

```go
// Identity types: printable, parseable, comparable, JSON-friendly.
func NewFileID() FileID
func ParseFileID(s string) (FileID, error)
func (id FileID) String() string                // canonical UUID form
func (id FileID) IsZero() bool
func (id FileID) MarshalText() ([]byte, error)  // and UnmarshalText; same for ShareID, ChunkHash, BlockName
func ParsePrincipal(s string) (Principal, error)
func (p Principal) String() string              // "user:7c1e…", "group:0b9a…": kind and ID, never a UID or SID

// Every enum: String, and text marshalling, so dumps and logs read as words.
func (t FileType) String() string               // "regular", "directory", …

// Attrs: builders set the mask, so a caller cannot change a field and forget
// to say so. Attrs itself is RFC 7's.
func (a Attrs) WithMode(m uint32) Attrs         // and WithOwner, WithGroup, WithTimes, WithSize, WithFlags
func (a Attrs) Changes(f AttrMask) bool

// Ranges and extents: the overlap arithmetic. A lock holds a ByteRange
// (RFC 14); a removal, a clone spec and a read name an Extent (RFC 6).
func (r ByteRange) End() int64
func (r ByteRange) Overlaps(o ByteRange) bool   // and Contains, Intersect
func (e Extent) End() int64
func (e Extent) Overlaps(o Extent) bool         // and Contains, Intersect

// Usage and quota.
func (u Usage) Add(d Usage) Usage
func (u Usage) Exceeds(l Limit) bool

func (s Snapshot) Expired(now time.Time) bool
```

Entity methods defined elsewhere:

| Entity | Methods | Where |
| --- | --- | --- |
| `File`, `FileType` | `IsDir`, `IsRegular`, `IsSymlink`, `IsStream`, `IsSpecial`, `IsRoot`, `HasContent`, `HasFlag`, `FSMode`, `Validate`, `Apply` | [RFC 7](rfc-7-namespace-metadata.md) |
| `Entry` | implements `io/fs.DirEntry`: `Name`, `IsDir`, `Type` | [RFC 7](rfc-7-namespace-metadata.md) |
| `ACL` | `ACLFromMode`, `Mode`, `WithMode` (chmod's merge), `Clone` | [RFC 7](rfc-7-namespace-metadata.md) |
| `ChunkRef`, `Block` | `End`, `IsLive`, `VisibleAt(SnapshotCut)`; `Retired` | [RFC 6](rfc-6-block-metadata.md) |
| `Open`, `Lock`, `CachingGrant` | `Conflicts`, `BrokenBy` | [RFC 14](rfc-14-open-state.md) |

Two things are deliberately **not** methods. Permission evaluation — "may this
identity do this to this file" — needs the principal's groups and the share's
grant, so it is `Files.Authorize` behind the one chokepoint ([RFC 7](rfc-7-namespace-metadata.md) §7.1), never
`acl.Allows`. And no entity has `Save`, `Reload` or a back-pointer to the store:
entities are values, freely copied, compared and printed.

## 3. Interfaces, by consumer

Adapters hold one thing: `vfs.Service` ([RFC 17](rfc-17-vfs.md)). Everything below is held by
the filesystem service or by internal consumers. Every namespace call takes a
resolved identity and a handle and is authorised inside the layer ([RFC 7](rfc-7-namespace-metadata.md) §7.1);
the content calls take a `FileID`, because only the namespace ever sees a name.

The views are defined where their semantics are:

| View | Holds | Defined in |
| --- | --- | --- |
| `Namespace`, `Files`, `Capacity` | names, attributes, ACLs, xattrs, streams, statfs and usage | [RFC 7](rfc-7-namespace-metadata.md) |
| `Existence`, `Content`, `Blocks` | the write path, the engine's commits and refs, GC | [RFC 6](rfc-6-block-metadata.md) |
| `OpenRecords` (`metadata.Clients`, `metadata.Opens`, not open state's `openstate.Clients` and `openstate.Opens` views over them) | client records, durable opens and their locks, delete pending, the restart-notice number, and `NextID(kind)`, which issues client, open and lock IDs from `IDS‖kind`; never caching grants or watches, which are never durable | [RFC 14 §11.1](rfc-14-open-state.md#11.1%20Package%2C%20dependencies%20and%20wiring) |
| `Principals`, `Records`, `Minter` | identity's lookups and a share's policy, its reads of one record, and the only writes it makes | this RFC, for [RFC 18 §2.6](rfc-18-identity.md#2.6%20Components%2C%20wiring%20and%20lifecycle) |
| `Lifecycle` | a device's journal identity, the version limits, the incarnations | this RFC, for [RFC 8 §2.1](rfc-8-engine.md#2.1%20Content%20composition) |
| `ControlPlane`, `Inspect` | the management API, trusted reads by ID | this RFC |

```go
// Principals is what authentication and the chokepoint read (RFC 18). Groups
// resolves transitive membership; its result is cached only under a key that
// includes everything it read (RFC 7 §7.5).
type Principals interface {
	// ByName takes a kind: a user and a group may share a name (a user's
	// private group), and NX‖user‖name and NX‖group‖name are separate rows.
	ByName(ctx context.Context, kind PrincipalKind, name string) (Principal, error)
	ByProtocolID(ctx context.Context, id ProtocolID) (Principal, error)
	Groups(ctx context.Context, p Principal) iter.Seq2[Principal, error]
	// A caller of Grant reads S‖id‖gv before the grants it keys a cache entry on.
	Grant(ctx context.Context, share ShareID, p Principal) (ShareAccess, bool, error) // false: no grant names p; a stored none is (none, true)
	// SharePolicy reads what resolution needs of one share in one snapshot, so a
	// squash, an anonymous principal and the versions a cache keys on never come
	// from two different commits.
	SharePolicy(ctx context.Context, share ShareID) (SharePolicy, error)
}

// SharePolicy is a share's identity policy as resolution reads it (RFC 18 §6).
type SharePolicy struct {
	Share         ShareID
	Squash        Squash
	Anonymous     Principal
	ManageGIDs    bool
	PolicyVersion uint64 // ExportPolicy.Version
	GrantVersion  uint64 // S‖id‖gv
}

// Records reads one principal's record by ID: the spellings, the Disabled flag
// and the deleted mark that Principals, which answers a Principal only, lacks.
type Records interface {
	User(ctx context.Context, p Principal) (User, error)
	Group(ctx context.Context, p Principal) (Group, error)
}

// Minter is identity's only write path: the records a first sign-in or lookup
// mints (RFC 18 §2.4, §5.1). It never overwrites. Each write is one transaction,
// draws the IDs a names from their counters inside it, and fails with ErrExist
// when another principal holds a row it would write. Every Minter write, like
// every other User or Group record change, raises the principals generation
// (PGEN, §4.2) in the same transaction (RFC 18 §7.2).
type Minter interface {
	CreateUser(ctx context.Context, u User, a Alloc) error
	CreateGroup(ctx context.Context, g Group, a Alloc) error
	Rename(ctx context.Context, p Principal, name string) error    // the record's Name and its name index only
	SetEmail(ctx context.Context, p Principal, email string) error // the record's Email only, from the directory (RFC 18 §5.1)
}

// Alloc names the IDs the store draws from its uid, gid and RID counters in the
// write's own transaction (RFC 18 §2.3). An ID not named here is written as the
// record holds it, and a zero one is refused ErrInvalid: zero never means
// "allocate".
type Alloc struct{ UID, GID, RID bool }

// Lifecycle is the engine's store access outside block metadata's views and
// Inspect.Refs (RFC 8 §2.1, §2.5, §2.7), over S‖id‖vl and the incarnation keys.
// Signatures are indicative; RFC 8 owns what each call carries.
type Lifecycle interface {
	JournalID(ctx context.Context, device DeviceID) (JournalID, error)                 // the identity recorded for a device, for Open
	VersionLimits(ctx context.Context, shares []ShareID) (map[ShareID]Version, error) // each share's limit, read for Open's floor
	RaiseVersionLimits(ctx context.Context, to map[ShareID]Version) error             // a tracked max-write of S‖id‖vl, within K keys a transaction
	RaiseIncarnations(ctx context.Context, shards []ShardID) error                    // in the start transaction, or at attach
}

// ControlPlane is the management API's view (RFC 23). Every write names what
// it changes; a user rename writes the user record and its name index, and
// nothing under any F‖ prefix.
type ControlPlane interface {
	Users(ctx context.Context, after string) iter.Seq2[User, error]
	// Every Put*, Set* and Delete* takes expect, the generation the caller
	// read (Version, for a grant, an export policy, a netgroup or a quota). It reads the record with
	// conflict tracking and refuses ErrGenerationMismatch when the stored one
	// differs, so the management API's If-Match check sits inside the write's
	// transaction (RFC 23 §4.4); the write raises the generation. Zero expects
	// the record absent: the write is create-only and refused
	// ErrGenerationMismatch when one exists, which is If-None-Match: *.
	//
	// Every record of a reconciled kind — settings (CFGGEN), shares (SL),
	// principals (PGEN) — also carries Position uint64: the value of its kind's
	// counter that its last change raised, written in that change's
	// transaction. Grants, export policies, quotas and netgroups carry none,
	// since they take effect through the versions in the cache keys that read
	// them (RFC 23 §2.3), not through reconciliation. A generation counts one
	// record's changes and is not comparable with the counters; a node reports
	// the counter value it has reconciled through (NodeStatus), and RFC 23 §2.4
	// says when that makes a record applied.
	PutUser(ctx context.Context, u User, expect uint64, a Alloc) error // a applies only to a create (expect zero), as Minter's does; a later put allocates nothing
	DeleteUser(ctx context.Context, p Principal, expect uint64) error // as DeleteGroup: the first transaction deletes the indexes (NX, PX) and marks the record deleted, bounded batches a restart resumes from that mark drop its grants (through GR), memberships (MR, M) and quota records, and the last transaction deletes the record with every version of the secret it names (SEC‖id‖*, through RFC 13 §7), so no password or NT hash outlives its user; a marked user is admitted nowhere
	Groups(ctx context.Context, after string) iter.Seq2[Group, error]
	PutGroup(ctx context.Context, g Group, expect uint64, a Alloc) error
	AddMember(ctx context.Context, m Membership) error
	RemoveMember(ctx context.Context, m Membership) error
	DeleteGroup(ctx context.Context, p Principal, expect uint64) error // all its rows: the group, its name and protocol-ID indexes (NX, PX), grants (through GR), memberships both ways (M, MR) and quota records; the first transaction deletes the indexes and marks the group record deleted, bounded batches a restart resumes from that mark drop the rest, and the record goes last; a membership of a marked group grants nothing, and a User.Primary naming one counts as zero
	Shares(ctx context.Context) iter.Seq2[Share, error]
	PutShare(ctx context.Context, s Share, expect uint64) error // Name, Path, Root, Config only
	SetShareState(ctx context.Context, share ShareID, from, to ShareState) error
	DeleteShare(ctx context.Context, share ShareID, snapshots SnapshotDisposition, expect uint64) error // §2.3.2
	SetGrant(ctx context.Context, g ShareGrant, expect uint64) error // Access none stores a grant that refuses the principal, distinct from no grant (RFC 18 §6.4); a grant naming a principal with no file spelling — a machine or an API-only account (RFC 18 §2.4) — is refused; Version, and the share's grant-list version S‖id‖gv written in the same transaction, are the writing commit's, so neither ever repeats a value a cache keyed on
	DeleteGrant(ctx context.Context, share ShareID, p Principal, expect uint64) error // deletes the grant and its GR key, so the principal falls through to its groups' grants, and sets S‖id‖gv to the writing commit's version, so a cache keyed before the deletion misses
	PutExportPolicy(ctx context.Context, p ExportPolicy, expect uint64) error // raises Version, as SetGrant does
	Quotas(ctx context.Context, share ShareID) iter.Seq2[Quota, error] // the share's quotas: share-wide, the user and group defaults, per principal and per project
	SetQuota(ctx context.Context, share ShareID, q Quota, expect uint64) error // a quota that limits nothing deletes it, and a share-wide or project one its qx; a changed soft limit restarts its grace: qx is rewritten at the change's time if usage is above the new soft limit and deleted otherwise, while a change to any other limit leaves a running grace; a principal's grace under a changed default, or one that gains or loses its own quota, restarts at its next check, which finds the soft limit qx recorded no longer in force (RFC 7 §2.8)
	Netgroups(ctx context.Context, after string) iter.Seq2[Netgroup, error]
	PutNetgroup(ctx context.Context, n Netgroup, expect uint64) error // sets Version to the writing commit's, as SetGrant does
	DeleteNetgroup(ctx context.Context, name string, expect uint64) error // rules naming it are not rewritten and fail closed (Netgroup)
	Snapshots(ctx context.Context, share ShareID) iter.Seq2[Snapshot, error]
	Settings(ctx context.Context, scope SettingScope) iter.Seq2[Setting, error]
	PutSetting(ctx context.Context, s Setting, expect uint64) error

	// The management API's own records (§2.3) — Operation, IdempotencyRecord,
	// Session, APIToken, AuditRecord, AuditCursor — are reached here too,
	// indicatively one read and one conditional write each, written in the
	// transaction of the request it records. An AuditRecord is append-only;
	// a cursor's write names the holder it read. NodeStatus is not here: every
	// node writes its own through the Node view.
	PutOperation(ctx context.Context, op Operation, expect uint64) error
	AdvanceCursor(ctx context.Context, c AuditCursor, holder NodeID, holderEpoch uint64) error
}
```

What each consumer holds:

| Consumer | Holds |
| --- | --- |
| NFS and SMB adapters | `vfs.Service` only ([RFC 17](rfc-17-vfs.md)) |
| Filesystem service ([RFC 17](rfc-17-vfs.md)) | `Namespace`, `Files`, `Capacity`, and the engine's content facade |
| Open state ([RFC 14](rfc-14-open-state.md)), `*openstate.Table` | `OpenRecords`: the filesystem service reaches open state only through the table, never the view |
| Engine | `Existence`, `Content`, `Lifecycle`; `Inspect.Refs` for the reseed only |
| GC | `Blocks` |
| Identity: authentication and resolution ([RFC 18](rfc-18-identity.md)) | `Principals`, `Records`, `Minter`; tree connect and mount go through the filesystem service's `Shares` calls ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)) |
| Management API | `ControlPlane`, its own records included |
| Shard placement (RFC 11), and every node's own `NodeStatus` | `Node` and `Shard` records through its own view, fenced by epoch |
| Debug tooling | `Dump` (§4.5) |

**The metadata store is a leaf.** It never calls the engine, and no view here
holds an engine interface. There is one read path for a file's attributes: the
filesystem service's `GetAttr` joins `Files.Get` with the engine's overlay of
`Size`, `Times` and `Version` for bytes not yet committed ([RFC 8](rfc-8-engine.md)); nothing
else answers size or times, and `Existence` offers no read of them.

`User` and `Share` here are entities, not wire types: `PutUser` receives a
`User` and overwrites it whole, and the only field it writes that another
entity reads is the name index. `PutUser`, `PutGroup`, `PutShare`,
`PutExportPolicy`, `PutNetgroup` and `PutSetting` are the only places in the
model where a whole-record put is allowed, each checked against `expect` inside
its transaction, and only the management API makes one. Identity never calls
`PutUser`: what a sign-in mints goes through `Minter`, which never overwrites.
Both create paths name the IDs they allocate (`Alloc`), so an allocation commits
or aborts with the record that holds it.
`PutShare` writes `S‖id‖info` alone — name, path,
root, configuration — and the name and path indexes. The two parts of a `Share`
that a background path changes are records of their own: `S‖id‖st`, the state,
which cuts, deletions and the management API all change through
`SetShareState`, and `S‖id‖nsg`, the namespaces and write generation, which
only a re-home ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) and an import write. Each is changed only by a
read-modify-write whose read is conflict-tracked (§4.1), and `SetShareState`
names the state it expects, so a management-API change racing a cut or a
re-home conflicts and retries against what the other wrote, never reverts it.

### 3.1 Lookups by ID, listings, and who may use them

The adapter views take handles and an identity because every one of their calls
is a client's, and is authorised. Internal consumers — snapshots and export
(RFC 12, RFC 26), the audit, repair, the management API and the debug tool — work by ID,
and have no client to authorise. They hold a separate view, which adapters never
receive:

```go
// Inspect reads by ID without authorisation. It is for trusted internal
// consumers only; handing it to an adapter would bypass RFC 7 §7.1.
type Inspect interface {
	File(ctx context.Context, id FileID) (File, error)
	Entry(ctx context.Context, dir FileID, name []byte) (Entry, error)
	Entries(ctx context.Context, dir FileID, after []byte) iter.Seq2[Entry, error]
	Walk(ctx context.Context, root FileID, after WalkCursor) iter.Seq2[WalkStep, error] // depth-first, resumable
	ACL(ctx context.Context, id FileID) (ACL, error)
	Refs(ctx context.Context, file FileID, from int64) iter.Seq2[ChunkRef, error]
	Chunk(ctx context.Context, h ChunkHash) (Chunk, error)
	Block(ctx context.Context, name BlockName) (Block, error)
}
```

Every listing in the model — entries, refs, users, shares, snapshots, xattrs —
is an iterator that resumes from a cursor the caller holds (§1.2 rule 2, [RFC 15](rfc-15-topology.md)).
Helpers that only combine these calls (resolve a path from the root, sum a
tree's size, compare two snapshots) are functions over the interfaces, in the
package of the tool that needs them, not methods on the store.

### 3.2 How it is assembled

There is one concrete store, built once from a `KV`:

```go
err := metadata.Init(ctx, kv, install)                     // once per installation, with the minted ID; see below
st, err := metadata.Open(ctx, kv, metadata.Options{...}) // reads the format record first (§4.6)

var (
	_ Namespace    = st.Namespace()
	_ Files        = st.Files()
	_ Capacity     = st.Capacity()
	_ Existence    = st.Existence()
	_ Content      = st.Content()
	_ Blocks       = st.Blocks()
	_ Principals   = st.Principals()
	_ Records      = st.Records()
	_ Minter       = st.Minter()
	_ Lifecycle    = st.Lifecycle()
	_ ControlPlane = st.ControlPlane()
	_ Inspect      = st.Inspect()
)
```

`Init` is the store's part of the explicit initialisation step
([RFC 13 §2.2](rfc-13-configuration.md#2.2%20A%20host%20holds%20only%20its%20bootstrap)). In one transaction it writes `\x00format`, `IN`
with the installation ID the step minted and `SL` at 0, and it refuses a store that already
holds a format record. `Open` never creates one: it reports a store with no
format record as not initialised.

Inside, the store is not one giant type. Each area — namespace, files, content,
blocks, principals, control plane, open state — is its own small struct over one
shared core that holds the `KV`, the codecs and the key layout. What each area
writes, it writes through **transaction-level helpers** (`unlinkTx(tx, …)`,
`chargeUsageTx(tx, …)`, `releaseTx(tx, …)`) that take the caller's `Txn`. That is
how an operation spanning areas stays one transaction: `Unlink` removes the
entry, decrements `Nlink`, moves usage and writes the pending release in one
`Update`, by calling four helpers, never four interface methods that would each
open a transaction of their own.

`node.New` ([RFC 23 §2.1](rfc-23-control-plane.md#2.1%20The%20node%20builds%20its%20components)) calls `Open` — the one thing it opens, since every view is bound to the open store — and hands each consumer only the
view it declared ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)). A view is always local to its consumer's
node: what crosses between nodes is a whole filesystem-service call
([RFC 15 §2.5](rfc-15-topology.md#2.5%20The%20cut)), never a view's.

## 4. Persistence pattern

### 4.1 One small interface per backend

The entity layer is written once. A backend implements only this:

```go
type KV interface {
	// Update runs fn in one transaction and retries it on a serialisation
	// conflict under ctx's deadline (RFC 0 §9.2). fn may run more than once.
	Update(ctx context.Context, fn func(Txn) error) error
	View(ctx context.Context, fn func(Reader) error) error
	Limits() TxnLimits // value, key, transaction size with conflict ranges, transaction age: derive RFC 6's K
}

type TxnLimits struct {
	Key, Value int           // largest key, largest value, in bytes
	TxnBytes   int           // one transaction: keys, values and conflict ranges
	TxnEntries int           // keys written or deleted in one transaction; 0 if unbounded
	Age        time.Duration // a transaction older than this, from its snapshot, aborts
}

type Reader interface {
	Get(key []byte) ([]byte, error) // ErrNotFound
	Scan(prefix, after []byte) iter.Seq2[KeyValue, error] // untracked
	// Before returns the last record under prefix whose key sorts below key,
	// untracked like Scan; ok is false when there is none.
	Before(prefix, key []byte) (kv KeyValue, ok bool, err error)
	// Versions returns each key's change sequence, in one batched read
	// (§4.1, "Every committed write carries a change sequence"). A key this
	// transaction wrote or deleted has none yet: ErrUncommittedVersion.
	Versions(keys [][]byte) ([]uint64, error)
}

type Txn interface {
	Reader
	Set(key, value []byte) error
	Delete(key []byte) error
	// Guard is a tracked read that returns no value: this transaction aborts
	// at commit if another committed a write to key after its snapshot. It
	// binds only this transaction, and two guards never conflict (§4.1).
	Guard(key []byte) error
	// GuardRange is Guard for every key under prefix, present or not.
	GuardRange(prefix []byte) error
	// Now is store time for this transaction (§4.1, "Now is store time").
	Now() time.Time
}
```

![One Update: fn runs against one snapshot, where Get, Guard and GuardRange are tracked, Scan and Before are not, and Set and Delete write. At commit, a conflict on any tracked or written key aborts the attempt, which is retried on a fresh snapshot within the caller's deadline; otherwise the commit shares a sync with other ready commits and returns only once durable. An error may still have committed. Panels: alice-pc and build01 both guard d1 and both commit; Report.docx and REPORT.DOCX fold to one key, so the second aborts and its retry finds the name taken; a guard committing before a writer lets both commit, a writer committing first aborts the guard](img/rfc16-txn.svg)

**A commit is reported only once durable.** `Update` **MUST** return success only
after the transaction's writes are durable: on the embedded store, synced to its
device, so a crash or power loss of the node a moment later keeps them; on the
replicated store, replicated to a quorum of its replicas and durable there. A
backend that answers before then — an embedded store with its sync turned off, a
replicated store answering from its leader's memory — is unsupported, because
every caller treats a returned `Update` as a fact: an existence commit lets the
journal release the bytes it recorded, and a removal commit lets GC act. An
`Update` that returns an error **MAY** still have committed, and callers handle
that as an unknown outcome, never as a refusal.

**The embedded store shares one sync among concurrent commits.** Synced one
commit at a time, a store reaches only as many commits per second as its device
completes syncs — a few hundred on consumer NVMe — for the whole installation.
The embedded backend **MUST** therefore group the `Update`s that are ready to
commit together into one sync (group commit), and **MUST NOT** acknowledge any
of them before that sync completes. A mode that acknowledges before the sync and
syncs in the background is not supported, under any setting: the rule above has
no loss window to configure, because what the journal releases and GC deletes on
the strength of an `Update` cannot be put back. A flush that waits on a metadata
commit — an overwrite's existence commit included — waits for that shared sync.
The benchmark of §7.1 holds the rate.

**Guards bind their own transaction, and are shared.** A guard — `Guard(key)`,
or `GuardRange(prefix)` for every key under a prefix — is a conflict-tracked
read that returns no value:

- A transaction that guards a key **MUST** abort at commit if another
  transaction committed a write to that key after its snapshot. `GuardRange`
  **MUST** do the same for every key under its prefix, keys absent at the
  snapshot included, so a key created under the prefix aborts it.
- A guard binds **only** its own transaction. If a transaction G that guards a
  key commits before a transaction W that writes it, both commit, and W is
  ordered after G; no rule may assume W fails. That is harmless where W decides
  nothing from what G wrote — a fence raised after a commit that read it is
  simply later than that commit. Where W does decide from G's writes, W must
  itself track a key G writes ([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries)): a directory's removal
  `GuardRange`s its entries for exactly this reason ([RFC 7 §3.6](rfc-7-namespace-metadata.md#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on)).
- Two guards **MUST NOT** conflict, point or range, **whatever their commit
  order**. Two transactions that guard one key, and write nothing the other
  tracks, both commit — including when one commits between the other's
  snapshot and its guard. A backend whose guard is a shared lock that holds
  only while both are held, so that a guard taken after another guarding
  transaction has committed fails, does not meet this rule; the KV conformance
  suite commits two guards of one key in both orders (§6.1). Creates in one
  directory each guard the parent (`Guard(F‖parent)`, [RFC 7](rfc-7-namespace-metadata.md)) and every
  namespace transaction guards its file's fence (RFC 6 §5.4); a guard that
  serialised them would bring back the one-directory create rate of Appendix A.
- A guard constrains only a transaction that writes. A backend **MAY** skip the
  conflict check of a transaction that writes nothing, and a caller **MUST
  NOT** rely on a guard in one: every check this set makes with a guard is made
  in the transaction that writes what the check permits.

**`Now` is store time.** Every time a record stores to be compared later — GC's
`not_before`, a delete's completion, a `Recheck`, a node lease's expiry — is taken
from `Now` and compared with `Now` in a later transaction ([RFC 9 §1.2](rfc-9-gc.md#1.2%20Words%20this%20document%20uses)).
A lease's expiry is compared with `Now` only where a lease is renewed or
acquired, never to fence a commit; a takeover's claim compares no store time
with a clock either, and claims a lease only once its record is unchanged since
a read L + δ earlier on the claimant's monotonic clock ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)). **Every fenced
commit guards its primary's node record (cluster)** and is refused unless the record holds
the node epoch the commit carries, unmarked ([RFC 11 §8](rfc-11-ownership.md#8.%20Metadata%20consistency)); a takeover marks it in
the transaction that claims the shard, so a paused primary's commit in flight
conflicts and aborts before its successor has fenced any file. The guard holds
whenever the store chooses a commit's timestamp, which a check of `Now` against
an expiry would not. `Now` **MUST** be
monotonic across transactions that commit in order (on a backend without an
oracle, those of one node: below), and within a stated bound of
real time, and reading it writes no key. A backend with a timestamp oracle
returns the timestamp the oracle issued for the transaction's snapshot; its
commit timestamp, chosen later, is at or above it. A backend without one, or
whose versions are not time — the embedded single-node store, or a replicated
store whose read version is a counter that stands still on an idle store and
races ahead under load — returns the node's clock clamped monotone: never
below the last value it returned, and never past a ceiling held in the node's
clock record (`N‖node‖clk`, §4.2), which a background write raises one second
ahead before `Now` reaches it; a start resumes from that ceiling, so a clock stepped back across a restart
cannot carry `Now` backward. That costs one write per second per node, never one
per transaction: a clock kept in a key that every transaction read and rewrote
would be the hot counter §4.4 forbids. Clamped this way, `Now` is monotonic
across the transactions of one node; across nodes of a cluster on such a store,
two transactions in commit order may read `Now` apart by up to the clock-offset
bound σ ([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)). A time one node stored compared with
another node's `Now` must allow σ; the cluster's lease waits avoid the comparison
altogether by measuring a duration on the waiter's monotonic clock
([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)).

**Every committed write carries a change sequence.** Its source is the version
the store assigns at commit — an oracle's commit timestamp, or the store's own
commit version — so it orders writes like their commits, and no transaction
writes a counter to get it. `Versions` returns it for a batch of keys in one
read, and a backend **MUST** answer it in one of two ways, or both: keep the
version with the key and read it back, or have the store write the commit's
version into the stored value at commit — filled by the store, never by the
transaction, and stripped by the backend before `Get` or `Scan` returns the
value. Either way a transaction's `Get`, `Scan` and `Before` of a key it wrote
return the value it wrote, as KV conformance requires. A store that fills the
version in at commit may make such a key unreadable until commit and fail a read
of it inside the transaction with an error it does not retry; a backend on such a store
**MUST** then serve the transaction's own writes from its own write buffer with
the store's read-your-writes off, or keep the versions in a separate keyspace
whose writes count in the transaction's key budget (§4.1, limits), so no read
of a transaction's own write ever reaches the store unfilled. `Versions` of a
key the transaction wrote or deleted returns `ErrUncommittedVersion`, a named
error, never a guess: a `Delete` is a write, and the key's version changes only
at the commit. A scan that returns a version per
key is not required. A move's delta is
the records whose change sequence is above its base cut's, under a share's
prefixes and its namespace's content-addressed ones, found by a scan and one
`Versions` call per page of it ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)).

Two backends with identical semantics differ only here, and one conformance
suite over `KV` plus one over the entity layer covers both.

**Isolation: snapshot isolation plus conflict-tracked reads.** A backend
**MUST** run each `Update` against one consistent snapshot, **MUST** detect a
write-write conflict on every key, a blind write included, and **MUST** track
point reads: every `Get` inside `Update`, every `Guard`, and every key under a
`GuardRange`, registers itself, and the commit aborts if another transaction
committed a write to it after the snapshot was taken. A backend whose own check
does not cover a blind write — one that checks only reads — **MUST** add a read
conflict on each key the transaction writes, at commit and with no extra
request, so a blind write is checked like a read. Each abort is retried under
I8 ([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries)). Two tracked reads of one key never conflict with each other.
So a lost update (both read *k* and write *k*) and write skew over point keys
(each reads the key the other writes) both abort whichever side commits second.
`Scan` and `Before` are not tracked: a range read detects no phantom, and every invariant over
a range — "the count is zero, so delete", "the directory is empty, so remove" —
**MUST** follow RFC 0 §9.2: each side tracks, by `Get`, `Guard` or `GuardRange`,
a key the other writes. A backend that offers less is unsupported; the KV
conformance suite (§6.1) is the test.

**Where the store's own transactions fall short, the backend supplies the rest.**
These are properties of `KV`, not of the store under it. An embedded store
whose native transactions track only point reads, cannot register a read of a
key the transaction has already written, have no range conflicts, or register
every key an iterator passes, meets them in its backend, in one process:

- the backend keeps each transaction's tracked keys and guarded prefixes itself,
  whatever the store registers, and adds every key the transaction writes;
- at commit, under the one lock that already orders the store's commits, it
  checks them against the keys committed since the transaction's snapshot — an
  in-memory log of recent commits' keys, sorted, so a guarded prefix is one
  range probe — and aborts on any hit, as a store-detected conflict would;
- `Scan` and `Before` read through an iterator that registers nothing in the
  store's own read set, or whose registrations the backend discards, so a scan
  never aborts a transaction the suite says it must not.

The log needs to reach back only as far as the oldest open transaction's
snapshot, which `Limits().Age` bounds. A replicated store with range conflicts
and blind-write conflicts natively needs no such log, but still needs the
read-your-writes mapping above if it fills versions in at commit.

**Transaction limits are the backend's, and RFC 6's key budget comes from
them.** `Limits` reports what one transaction may hold: the largest key and
value, the bytes of one transaction counted with its conflict ranges — every
key it reads, guards or writes, and every guarded prefix — the keys it may write
or delete, and the age from its snapshot past which the store aborts it. They
are properties of the backend, never settings. They bind three ways:

- **Values and keys.** No codec may produce a value or a key past them: a record
  whose value could grow past `Value` is split into keys of its own (§1.2 rule
  2), and §4.2.1's longest key is checked against `Key` when a store opens.
- **The key budget.** [RFC 6 §5.2](rfc-6-block-metadata.md#5.2%20Cost%20per%20commit%20is%20bounded%20by%20what%20changed) owns *K* and derives it from `Limits`: the
  largest *n* for which a batch's fixed records plus *n* items, each at its
  worst-case encoded key, value and conflict-range bytes, stay within
  `TxnBytes` and, where it is bounded, `TxnEntries`. Conflict ranges count
  because on some backends they are charged against the same budget as the
  writes; a derivation from written bytes alone overruns it.
- **Age.** No transaction holds a snapshot across remote I/O or a wait
  ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit)): everything a batch writes is computed before its transaction or
  inside it from the store alone, so a batch of *K* items commits well inside
  `Age`. A transaction that outlives `Age` aborts and is retried like a
  conflict.

### 4.2 Keys: per-file, per-share, content-addressed

Every key starts with a one-byte kind code, and its labels below stand for
the bytes §4.2.1 assigns. The layout encodes the boundary
[RFC 6 §1.2](rfc-6-block-metadata.md#1.2%20Why%20it%20is%20a%20separate%20RFC%20from%20the%20namespace) draws between what belongs to one file and what is shared:

| Scope | Key | Entity or record |
| --- | --- | --- |
| **Per file** — all under `F‖ShareID‖FileID`, written `F‖id` below | `F‖id` | File: every field, FileData's included, one value — among them a directory's `Subdirs`, a signed 64-bit count that a fold may leave below zero until its recount ([RFC 7 §4.1](rfc-7-namespace-metadata.md#4.1%20%60nlink%60%20is%20exactly%20its%20entries)), the directories it names, and `Incomplete`, set by a clone or restore of a file its snapshot lists incomplete ([RFC 7 §2.4](rfc-7-namespace-metadata.md#2.4%20Attributes%2C%20and%20who%20writes%20them), [RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)) |
| | `F‖id‖acl` | ACL |
| | `F‖id‖x‖name` | Xattr |
| | `F‖id‖s‖streamID` | named stream link |
| | `F‖id‖e‖digest‖key` | Entry, under its **parent** directory. `key` is the name folded by the share's fold rule (§4.6), the identity on a case-sensitive share; `digest` is the 63-bit keyed digest of `key` that orders a listing and is its resume cookie ([RFC 7 §3.5](rfc-7-namespace-metadata.md#3.5%20A%20cookie%20survives%20concurrent%20mutation)); the fold is [RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case)'s; the name's original bytes are in the value |
| | `F‖id‖t‖unique` | directory time delta, carrying the cut it was written after and a signed change to the directory's `Subdirs`, +1 or −1 when the entry it adds or removes names a directory (§4.4, [RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps)) |
| | `F‖id‖h‖start`, `F‖id‖rm‖version` | Hole, with its `born`, the cut it was written at — a clone batch's hole the clone's cut ([RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy)); Removal, with its extent, kind, the cut it was written after, its pruning cursor, `ovSpan` (the File's `MaxOverwrite` at phase 1), `holeCredit` (phase 1 found more than K holes, so phase 2 gives their bytes back), `charge` (the signed change to the File's `Charged` its phase-2 batches gathered, folded in by the transaction that sets it done while the File exists), `shard` (the File's shard at phase 1, naming its `rr` entry), for a release `fileVersion` (the deleted File's `Version`), and for a clone its **clone spec** — source ShareID and FileID, source offset, destination offset, length and the change version `asOf` the source was read at — from which a clone resumes after a crash, its source range registration rebuilt ([RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy)) ([RFC 6 §2.4](rfc-6-block-metadata.md#2.4%20FileData%20and%20holes), [§6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)); a removal not yet done has its index entry `S‖id‖rr‖…` (below) |
| | `F‖id‖ov‖start` | Overwrite record: an extent of content whose existence was committed and which a later committed write replaced, with the newest such write's version; an offset in it whose covering ref's `newest` is below that version reads as uncarved. Written only by an existence commit, never for an append or a first write into a hole ([RFC 6 §2.4](rfc-6-block-metadata.md#2.4%20FileData%20and%20holes), [§3.3](rfc-6-block-metadata.md#3.3%20Holes%2C%20not%20written%20extents)) |
| | `F‖id‖ref‖offset` | ChunkRef, live; its value carries `nsgen`, the generation of the share's namespace its chunk is counted in ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |
| | `F‖id‖H‖suffix‖died` | **history** of a versioned per-file record: the value it had, under its live key's suffix — `ref‖offset` (ChunkRef), 0x00 (File, §4.2.1), `acl`, `x‖name`, `s‖streamID`, `e‖digest‖key`, `h‖start`, `ov‖start` ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)) — then `died`, so one record's versions sit together and the history of one kind is in offset or name order. Records of one kind overlap across versions, so `Before` at an offset can return a later, shorter record and miss an earlier, longer one that also covers it: a snapshot read or a removal's dating finds the versions over offset *o* with one scan of the kind's keys from *o* − *L* to *o*, where *L* bounds every record of the kind — for refs the largest chunk maximum ([RFC 2 §3.2](rfc-2-carver.md#3.2%20One%20setting%2C%20and%20the%20bounds%20derived%20from%20it)) over every namespace the share's `Namespaces` lists, so history refs cut under the old generation's larger maximum are still found while a re-home runs ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)); for holes the File's `MaxSpan`, which every hole write raises, a removal's merged hole included, and for overwrite records its own bound, the File's `MaxOverwrite` ([RFC 7](rfc-7-namespace-metadata.md)), so one long hole never widens the overwrite scan; a removal's phase 2, which may run after a release deleted the File, takes it from its removal record's `ovSpan` ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)) — never a scan of the file's whole history ([RFC 12 §2.2](rfc-12-snapshots.md#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree)) |
| | `F‖id‖fx`, `F‖id‖fo` | fences: the (shard, epoch) the file's commits must carry ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit), [RFC 11 §8](rfc-11-ownership.md#8.%20Metadata%20consistency)); every offload commit writes `fo`. Per-file and range shards, and their records, are deferred ([RFC 11 Appendix C](rfc-11-ownership.md#Appendix%20C%20%E2%80%94%20later%3A%20per-file%20and%20range%20shards)) |
| | `F‖id‖rel` | pending release: written by the final unlink, deleted only by the release transaction, each with its index entry `S‖id‖pr‖…` (below); holds no holder list, the holders are the `op‖` records ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)) |
| | `F‖id‖op‖openID` | durable Open, written and deleted with its index entry `S‖id‖do‖…` (below) (only the cases RFC 14 makes durable: keeps an unlinked file alive, a persistent open, a clone's source open — its own kind, naming the destination and the clone's version, which grace end never closes and only the clone's last batch deletes, [RFC 14 §9.2](rfc-14-open-state.md#9.2%20A%20new%20primary%20releases%20nothing%20before%20grace%20ends)); a persistent open's value holds every field of RFC 14's `Open` — `Mode`, `Token`, `Seq`, `Delete`, `Channel.Seq`, `Reconnect` (`Timeout`, `Create`, `Instance`, `LockSeq`), `Clone` and its principal, but neither `LossSeen` nor `Channel.Outstanding`, which are volatile — and the grant covering it with its key, kind, epoch and `Adapter` bytes ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)) |
| | `F‖id‖op‖openID‖l‖start‖lockID` | durable Lock, only under a persistent open, so closing the open drops one prefix; `lockID` is minted per lock from `IDS` (below), so two locks of one open at one offset — two lock owners, or stacked shared locks — are two keys, and a scan still returns them in offset order ([RFC 14](rfc-14-open-state.md)) |
| | `F‖id‖dp` | delete pending, with the entry it will remove, the principal that marked it, as which the unlink runs, and the marking open's grant and privilege, which authorise it; written only while the file has a persistent open, deleted by that entry's unlink or by a clear ([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)) |
| **Per share** — under `S‖ShareID` | `S‖id‖info` | Share: name, path, root and configuration, the fields `PutShare` writes (§3), and the journal tag, written once when the share arrives |
| | `S‖id‖st` | the share's `State`; changed only by a conflict-tracked read-modify-write naming the state it expects (§3) |
| | `S‖id‖nsg` | the share's `Namespaces` and `Write` generation; written only by a re-home and an import, by a conflict-tracked read-modify-write (§3, [RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |
| | `S‖id‖g‖principal` | ShareGrant; its reverse key `GR‖principal‖ShareID` is written and deleted with it |
| | `S‖id‖gv` | grant-list version: the commit version of the share's last `SetGrant` or `DeleteGrant`, written in that transaction and read with the caller's grants. Authorisation and admission caches key on it ([RFC 7 §7.5](rfc-7-namespace-metadata.md#7.5%20A%20cached%20decision%20is%20keyed%20by%20everything%20it%20read), [RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)): a deleted grant has no record left to carry a version, and this one moves with it. `DeleteUser` and `DeleteGroup`, dropping a principal's grants through `GR`, set it on each share in the batch that drops the grant |
| | `S‖id‖xp` | ExportPolicy |
| | `S‖id‖snap‖cut` | Snapshot, with its ordinal; nothing else lives under this prefix, so listing snapshots reads only snapshots |
| | `S‖id‖cut`, `S‖id‖live‖k` | Cut: `k`, `klatest`, the cut time of the share's latest cut, and `deleting`, the cut a running deletion removes; LiveCut, one per live snapshot, with its kind, share or subtree, its snapshot's ordinal, and a subtree cut's covered set of shards ([RFC 12 §2.2](rfc-12-snapshots.md#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree), [RFC 12 §2.8](rfc-12-snapshots.md#2.8%20Deleting), [RFC 12 §2.10](rfc-12-snapshots.md#2.10%20Subtree%20snapshots)) |
| | `S‖id‖sc‖shard` | SubCut: `klatest`, the newest live subtree cut covering that shard, and the cut time of the latest subtree cut covering it, which a later cut's time must exceed by one second ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)); raised only by a cut, behind the shard's gate, lowered only by a deletion ([RFC 12 §2.10](rfc-12-snapshots.md#2.10%20Subtree%20snapshots)) |
| | `S‖id‖hd‖shard‖died‖FileID‖suffix` | died index: one empty key per history record of the share, ref or namespace, keyed first by the shard the version was superseded in and then by `died`, `suffix` being the history key's, written and deleted with it; a snapshot deletion walks it from its cut in each shard the cut covers, and in no other ([RFC 12 §2.2](rfc-12-snapshots.md#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree), [RFC 12 §2.8](rfc-12-snapshots.md#2.8%20Deleting)) |
| | `S‖id‖hr‖cut‖shard` | hold record: the shard's journals still hold content of that cut not yet offloaded; written by the cut, and for the receiving shard by a move's commit that ships held versions ([RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)) |
| | `S‖id‖use‖cut‖useID` | use record: its kind — `clone`, `restore`, `backup`, `copy` or `move` — reading that snapshot, the deadline its reader renews, which a `move` record has none of, nor a `backup` record once its backup is complete, by its writer or by abandonment, which keeps it until the backup expires, while a `copy` record is deleted at its backup's completion; and for a `backup` or `copy` the backup's ID, whose backup record (`NS‖ns‖backup`, below) holds its retention and the expiry time that installation-wide expiry reads; while one exists the snapshot cannot be deleted ([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot)) |
| | `S‖id‖rh` | re-home record: the new namespace, the cursor and the pass number of a running re-home, or ended, on a share retired with detached snapshots during one ([§2.3.2](#2.3.2%20Removing%20a%20share)), which the deletion of its last detached snapshot deletes; its existence refuses new clones, new catalog backups and moves of either namespace with `ErrRehoming`, while snapshots, copying backups and reads of existing snapshots go on ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |
| | `S‖id‖or‖shard` | `old_refs`, per shard: the share's refs still at the old generation during a re-home, folded like usage: a transaction that writes or drops such a ref writes the change in an `old_refs` delta (`S‖id‖od‖…`, below), never to this key (§4.4) |
| | `S‖id‖od‖shard‖unique` | `old_refs` delta, not yet folded, per shard: its own range, apart from the usage deltas, so a re-home's finish can guard each shard's range as empty without conflicting with every writer of the share ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace), §4.4) |
| | `S‖id‖ut‖shard`, `S‖id‖pu‖principal‖shard`, `S‖id‖pj‖project‖shard` | folded usage, **per shard**: share, with its `history_bytes`, principal, project; each written only by its shard's fold, and a reader sums a prefix scan over the shards (§4.4) |
| | `S‖id‖ud‖shard‖unique` | usage delta, not yet folded, per shard (§4.4) |
| | `S‖id‖qt‖target` | Quota: `Hard`, `Soft`, `Grace`, `Advisory`, `Version` ([RFC 7](rfc-7-namespace-metadata.md)); `target` is a principal, a project, the share as a whole, which `FilesystemInfo.Quota` reports, or a default for every user or every group without a quota of its own. Written by `SetQuota` (§3) |
| | `S‖id‖qx‖target` | when usage first exceeded the soft limit, and that soft limit; deleted when it falls back under. `target` is the share, a project or a principal — the principal even when its limit is the default, so each principal's grace is its own and none is keyed by a default. A `SetQuota` that changes a share-wide, project or principal soft limit rewrites it at the change's time if usage is above the new limit and deletes it otherwise; a check that finds the soft limit in force differs from the one recorded — a default changed, or the principal gained or lost its own quota — does the same at the check's time ([RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota)) |
| | `S‖id‖pjn` | project allocator: the next `ProjectID`, read and raised in the transaction of the `SetProject` it numbers, so no project number is issued twice in the share ([RFC 7 §2.8](rfc-7-namespace-metadata.md#2.8%20A%20share%20is%20one%20filesystem)) |
| | `S‖id‖vf‖bucket‖version‖FileID`, `S‖id‖vf‖bucket‖version‖FileID‖unique` | version-floor index: **one entry per File, at its stored `Version`**, written by every transaction that raises that `Version`, at the version the filesystem service drew from the engine under the file's accept lock ([RFC 17 §5.1](rfc-17-vfs.md#5.1%20Write)) for a namespace call — an existence commit, an attribute, flag, ACL or xattr change, a link change — which deletes the entry it supersedes; and one entry per unfolded directory-time delta at the delta's version, under its own unique suffix, written blind with the delta and replaced by the directory's own entry when the fold absorbs it. No ref has an entry: a File's `Version` is at least every version its refs and `Applied` carry. `bucket` is the file's identity hashed into one of *B* fixed buckets, so a share's commits spread over *B* key ranges ([RFC 6 §8.3](rfc-6-block-metadata.md#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor), [RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)) |
| | `S‖id‖fnc` | numeric file-id allocator: the next unreserved number, reserved in ranges by shard primaries, so the protocol's numeric id is injective ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)); the number itself is a File field |
| | `S‖id‖shs‖shard` | the share's shard list: one empty key per shard of the share, written in the transaction that creates the shard and deleted with the shard record ([RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards)); the removal records it at step 1 and advances its gate over it in batches ([§2.3.2](#2.3.2%20Removing%20a%20share)) |
| | `S‖id‖vl` | version limit: one `Version`, below which the share's journal may issue versions, written by the engine before the journal serves the share and raised, in its own transaction, before it is reached. Several journals serve one share's shards, each with its own counter, so every write of it — the first at start, the one at an attach, and every raise — is a tracked read that writes the larger of the stored value and its own, never a blind write: a journal whose counter lags never lowers the limit another journal has reported versions under; where a journal opens or attaches is RFC 0's attach rule, stated once in [RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile) ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing), [RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)) |
| | `S‖id‖pr‖shard‖file` | pending-release index: one empty key per pending release `F‖id‖rel`, under the shard the file is in, written and deleted in the transactions that write and delete the pending release; a release of a file with named streams indexes each stream's pending release under that stream's own shard. A primary's recovery reads its own shards' ranges of it, never every file of the share ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)). Like `rr` and `do` below, it is keyed by shard so each primary reads only its own: a batch move's commit re-keys a moved file's entries under the receiving shard ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) |
| | `S‖id‖rr‖shard‖file‖version` | unfinished-removal index: one empty key per removal record `F‖id‖rm‖version` not yet pruned, written with the record by the removal's first transaction and deleted by the transaction that prunes it, not the one that sets it done, under the `shard` the removal record carries, since a release's File is gone by then; a batch move's commit that re-keys the entry rewrites that field; a primary's recovery lists its own shards' ranges and resumes phase 2 for a removal not done and the prune for one done ([RFC 6 §6.4](rfc-6-block-metadata.md#6.4%20Delete), [§6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)) |
| | `S‖id‖do‖shard‖file‖openID` | durable-open index: one empty key per durable open record `F‖id‖op‖openID`, written and deleted with it; a new primary reads the shard's range of it before it answers anything there, to install its persistent and clone source opens, and again at grace end ([RFC 14 §9.2](rfc-14-open-state.md#9.2%20A%20new%20primary%20releases%20nothing%20before%20grace%20ends)) |
| **Per namespace** — content-addressed, one partition per remote key namespace ([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)) | `C‖ns‖hash`, `B‖ns‖name` | Chunk, Block (with its GC state: `live`, `retired`, `deleted`, the store time of its move to `deleted` (`marked_at`, [RFC 9 §3.1](rfc-9-gc.md#3.1%20Retire%20the%20records%2C%20then%20delete%20the%20object)), and its carried chunk list) |
| | `CR‖ns‖hash‖ShareID‖FileID‖offset‖died` | reverse ref index: one empty-valued key per live (`died` zero), history or parked ref, written in the ref's transaction and deleted by the transaction that drops it or by the parked-ref prune; authoritative for "which refs name this chunk", and the refcount is its cache ([RFC 6 §6.1](rfc-6-block-metadata.md#6.1%20A%20refcount%20is%20exactly%20its%20refs)) |
| | `I‖ns‖name` | put intent: domain, domain ID, epoch, and for a shard the writer's node ID and node epoch ([RFC 9 §7.2](rfc-9-gc.md#7.2%20Every%20record%20GC%20stores%20names%20its%20reclamation)) |
| | `BR‖ns‖b‖not_before‖name`, `BD‖ns‖name`, `BC‖ns‖b‖bucket‖name`, `b` the block name's first byte | GC index: retired blocks by `not_before`, deleted blocks awaiting prune (value: when the delete succeeded), compaction candidates by dead-ratio bucket. Derived from block records; repairable and rebuildable ([RFC 9 §7.4](rfc-9-gc.md#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt)) |
| | `NS‖ns‖gc‖lease‖partition`, `NS‖ns‖gc‖recheck`, `NS‖ns‖gc‖hold`, `NS‖ns‖gc‖suspect‖hash` | GC lease per GC partition with its epoch, last `Recheck` result, the deleter's hold, audit lowering state ([RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix)) |
| | `NS‖ns‖gc‖forward` | GC forward-walk marker: the numbers of the last forward pass started and the last completed ([RFC 9 §6.2](rfc-9-gc.md#6.2%20Corrections)) |
| | `NS‖ns‖claim` | claim nonces: the instance nonce this installation intends to write into the namespace's claim, recorded durably before the claim put, and the last one it wrote; a `Recheck` reads the claim and compares it with these before any rewrite. Every claim put on any path — start, `Recheck`, import, recovery, re-home, restore — records its intended nonce here first. **Installation-local**: never exported and never imported, so a moved namespace's importer starts with its own nonces and does not fence itself on finding the claim it just took ([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period), [RFC 27 §2.1](rfc-27-namespace-migration.md#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)) |
| | `NS‖ns‖gc‖pause` | GC pause record: while it exists the namespace gets no relocation, delete or collection; its value names its kind; a move's holds the step it reached, the list of its base cuts, which are the only snapshots the move deletes, and a deadline A renews; both ends delete each base cut — its use record, then the snapshot — before they delete this record, so a crash never leaves a base cut that no record names ([RFC 13](rfc-13-configuration.md)'s move pause deadline row); a recovery import's holds none of these — an interrupted import restarts from scratch, never resumes from its staging — and is never aborted by a deadline, only the operator's release ends it ([RFC 26 §2.3](rfc-26-catalog-backups.md#2.3%20Restore)); GC reads it before every pass and batch, and every relocation, delete or collection commit reads it with a tracked `Get` and refuses while it exists ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step), [RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)) |
| | `NS‖ns‖drain` | drain mark: the namespace is being emptied after no share names it. It holds the current **check token**: the drain's key check writes a fresh one when it starts, every copy batch that adds a census entry reads the mark with tracking and writes a new one, and the key-removal transaction reads it with tracking, proceeds only if it still holds its own check's token, and rewrites it — so a census entry and a removal conflict in both commit orders. It refuses a share creation, clone or restore into it and resumes the drain after a crash ([RFC 9 §7.6](rfc-9-gc.md#7.6%20A%20namespace%20no%20share%20names%20is%20drained)); deleted last |
| | `NS‖ns‖gc‖cursor‖walk‖partition` | walk cursor per kind of walk (audit, block walk, index rebuild) and GC partition ([RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix)), so a restarted walk resumes; derived |
| | `NS‖ns‖pk‖ShareID‖FileID‖offset‖died` | parked ref: a ref a re-home dropped from namespace `ns` while an unexpired catalog backup taken before the re-home finished still needs it, filed under `ns`'s prefix and owned by `ns`'s installation, so moving the re-homed share neither carries nor drops it; it keeps its chunk count and reverse key in `ns`, is seen by no read, snapshot or listing, only by `ns`'s GC; it carries no time: the share's parking record below holds it, and the prune drops the share's parked refs in batches once that time passes ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |
| | `NS‖ns‖park‖ShareID` | parking record: one per (namespace, share) whose refs are parked in `ns`, holding the latest expiry time of the catalog backups those refs serve and the IDs of any of them still `writing` when it was written, each of which holds the prune: that backup's completion or failure reads the record with tracking, raises the time to its expiry if it completed, and removes its ID ([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot)). The first switch batch that parks a ref writes it, `Extend` of such a backup raises it in its own transaction — refused once the backup's recorded expiry has passed in store time — and the prune, which may run from it before the re-home's finish as well as after while it names no writing backup, reads it with tracking in each batch and stops if it was raised; it is deleted with the last parked ref of the share. One record rather than a time on each ref, since a share parks one ref per ref it held and no transaction within K rewrites them all ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace), [RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot)) |
| | `NS‖ns‖key‖kind‖keyID` | namespace key record: kind (`chunk-id-key`, `chunking-key`, `export-key`, `data-key`), ID — an export key's ID is its generation, 8 bytes big-endian, a counter one above the previous export key's, followed by a minted ID's bytes, which the wrap's associated data binds under the master key, so "newer export key" means a higher generation and no holder of a retired key can mint one ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)) —, fingerprint, the ID of the master key that wraps it — the immutable, version-qualified ID the key service reports for that wrap ([RFC 5 Appendix B.1](rfc-5-transforms.md#B.1%20How%20a%20chunk%20is%20encrypted)), a copy kept for lookup of the one inside the wrapped bytes, which is the one that counts; a record whose two differ is refused as corrupt — the wrapped bytes, and state — `current`, `retiring` (an export key a rotation has replaced while any location is not yet re-sealed; its record then holds that **pending set** of locations, from which re-seals resume at every start and on the snapshot service's period, and several keys may be retiring at once, [RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)), `retired` or `destroyed`; a destroyed key keeps only ID, fingerprint and state. Every key is wrapped but one: a non-encrypting namespace's chunk-ID key, held in the clear, with no master key ID. Each record reaches the namespace's `keys` control object before it becomes current; a backup location's `keys` object must hold it before any state, export or progress version is written there, but does not gate the rotation. Not a secret: a wrapped key is useless without the master key, which is never here, and the one clear key hides nothing its plaintext blocks do not already show ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)) |
| | `NS‖ns‖keyseq‖target` | the namespace's key lock and, per `keys` object it writes — the namespace's own, or a backup location's — the last sequence put there under the current export-key generation; every sequence, a backup's state and progress versions' included, counts within one generation, starting at 1 in a new one ([RFC 5 Appendix B.1](rfc-5-transforms.md#B.1%20How%20a%20chunk%20is%20encrypted)). Every writer of a `keys` object (rotation, re-wrap, destroy, and every writer of a version at a location, which brings that location's object up to date first: [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)) reads and rewrites this row with tracking in the transaction that decides the put, so two writers conflict and never put one sequence twice. A recovery import raises each row above the highest sequence it found at that object under the current, highest generation; a sequence found under an older generation raises nothing ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)), so the dead installation's versions never outrank the new ones and a planted older-key version at 2⁶⁴ − 1 exhausts no counter. Sequences are compared only within one object. These rows name every location whose `keys` object holds the namespace's records, a location with no recorded backup included, so a rotation takes a retiring key's pending set from them, not from the backup records, and a location leaves the set only once its `keys` put has been read back with its own sequence; a location record is deleted only after that put, or after the location is dropped from every pending set as a lost location is ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys), [RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)). A `keys` object's versions are ordered by export-key generation, then sequence, not by RFC 4 §4.15's highest sequence alone ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys), [RFC 4 §4.13](rfc-4-remote-tier.md#4.13%20Control%20objects)) |
| | `NS‖ns‖bk‖location` | folder record: the copy or sweep holding the namespace's block folder at a backup location, its deadline, the **lease number** raised at every take, which every sweep commit reads, the folder's census of (material ID, fingerprint), and the count of blocks in its mark set, raised by each copy transaction that adds blocks and recomputed by a settle — both emptied, in batches, by an operator's statement that the location is lost —, which RFC 9 §7.6's drain gate reads ([RFC 26 §2.4.1](rfc-26-catalog-backups.md#2.4.1%20Layout%20at%20the%20location)) |
| | `NS‖ns‖backup‖backupID` | backup record: one per backup of the namespace's shares, under the namespace so it moves with it. The transaction that starts the backup writes it with kind, share, snapshot, location and the retention fixed then; its state — `writing`, `complete`, `damaged`, `failed`, `expired` or `lost` — what each state version carries is written here before that version is put at the location, and the state field reads `complete` only after the location's `complete` version ([RFC 26 §2.4.3](rfc-26-catalog-backups.md#2.4.3%20Writing%20one%2C%20step%20by%20step) step 5); before the backup is marked `complete` at its location one transaction records the export's digest, its manifest section's MAC, the head's version at an immutable location and its size, which `FetchRaw` reads it by, the material IDs the export names, which the drain reads until expiry ([RFC 9 §7.6](rfc-9-gc.md#7.6%20A%20namespace%20no%20share%20names%20is%20drained)), and the expiry time, which `Extend` raises; an operator's statement that a location is lost marks its backups `lost` here only. A backup's record stays under the namespace that sealed its export, so that namespace's holder expires, re-seals and sweeps it: a re-home leaves the share's earlier backups' records under the old namespace, and its finish releases the use records of the catalog backups taken before it, whose chunks the parking then holds ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)); a move of the namespace carries its records with it. A recovery import writes one for every backup of the namespace it finds at a configured location, from that backup's authenticated state, as `failed` when that is not `complete`, and rebuilds each folder record's census and block count from its mark set before it publishes ([RFC 26 §2.3](rfc-26-catalog-backups.md#2.3%20Restore)). Re-seals, the next export's digest list and the drain read these, never what a location returns, and a location version that differs is a health condition. Expiry deletes it last, after the export is removed ([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot), [RFC 5 Appendix B.3](rfc-5-transforms.md#B.3%20Rotation)) |
| **Server-wide** | `U‖principal`, `G‖principal` | User, Group, keyed by `PrincipalID`; a machine principal is a User record (§2.3) |
| | `M‖group‖member`, `MR‖member‖group` | Membership, both directions |
| | `GR‖principal‖ShareID` | grant reverse index: one empty key per `ShareGrant`, so deleting a principal finds its grants without a scan of every share; server-wide, so an export carries no row of it and an import writes one per grant it publishes |
| | `NX‖kind‖name` | name index: user, group and share names → ID, unique within the kind; `name` is the name's case fold — for a user or group simple Unicode case folding, as Windows and directories compare them (RFC 18 §4.6), for a share §2.3.1's fold — and the value holds the original name |
| | `NP‖path` | share path index: path → `ShareID`, unique; the pseudo-filesystem is built by listing it (RFC 17 §4.9) |
| | `NG‖name` | Netgroup |
| | `SL` | ShareList: one version per installation |
| | `TG` | share-tag counter: the next journal tag ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)), raised in the transaction that records a share's tag and never lowered, so no tag is given twice; a recovery import opens every adopted journal (after `Restamp`) before publishing any share, raises it above every tag their `Stats` name ([RFC 1 §3.7](rfc-1-journal.md#3.7%20State%20introspection)), and only then mints tags ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)) |
| | `IN` | Installation: the installation's identity, written once by the explicit initialisation step, which writes the same ID into every journal's `format` file ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)); its instance part and recorded machine-generation identifier; and the active version of every shared format but the store format, which `\x00format` holds, each raised only by the control plane's gate (§2.3, §4.6) |
| | `NSM` | the restart-notice number, one per installation; raised durably before the grace of any failover that lost locks of clients that recover by notice, and at a moved address's arrival above the larger of the move delta's number and its own ([RFC 14 §4.5](rfc-14-open-state.md#4.5%20NLM%20locks%20and%20restart%20notification), [RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step)) |
| | `PGEN` | principals generation: one installation-wide counter, raised in the transaction of every `User`, `Group` or `Membership` write and every `PX‖` row written or deleted; every node reads it at least once a second and stops serving identity it cached under an older one (RFC 18 §7.2) |
| | `PX‖scheme‖id` | protocol-ID index → `PrincipalID`, unique per scheme; `scheme` is one of `uid`, `gid`, `sid` (binary form) `krb`, a Kerberos principal as `name@REALM`, compared byte for byte as the KDC spells it, `dir`, a directory entry's immutable ID ([RFC 18 §5.1](rfc-18-identity.md#5.1%20What%20a%20directory%20provides)), and `oidc`, an OpenID Connect (issuer, subject) as the issuer spells them, held only by accounts linked to an issuer ([RFC 18 §8.2](rfc-18-identity.md#8.2%20OpenID%20Connect%20sign-in)); with the User or Group record, the only place a protocol spelling is stored ([RFC 18 §2.2](rfc-18-identity.md#2.2%20Spellings)) |
| | `N‖node`, `N‖node‖exp`, `N‖node‖clk` | Node: its roles, binary version, the version range it reads and writes for every shared format, failure domain, node epoch and whether a takeover marked it lapsed, guarded by every fenced commit; NodeLease (cluster): the lease's expiry, which a renewal writes with its journals' generations and nothing else, and a resume rewrites with the node record ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)). On a single node the node record's epoch rises at every start and no lease is written ([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)). Clock ceiling, only on a backend without a timestamp oracle: the bound `Now` stays under, raised once a second (§4.1); its own key, so raising it conflicts with no fenced commit |
| | `SH‖shard` | Shard, with its primary as (node, node epoch, journal identity, join incarnation), its epoch, its shard incarnation, its grace instance and its replicas ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) |
| | `SH‖shard‖rq‖requestID` | (cluster) request record: a namespace mutation's request ID and result, written in its transaction and expiring after the retry window ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)), so a retry after a takeover is answered, not applied again ([RFC 11 §5.1](rfc-11-ownership.md#5.1%20Gateways%20forward%20to%20the%20primary)); a batch move's commit re-keys the shipped ones under the receiving shard ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries) step 3) |
| | `SH‖shard‖rqx‖expiry‖requestID` | (cluster) request-record expiry index: one empty key per request record, written and deleted with it, so the shard's primary sweeps its own expired records on the retry window by one ordered scan of its own range ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)). Keyed per shard, so each primary inserts at the tail of its own range only, at its own shard's mutation rate; one installation-wide time-ordered range would put every namespace mutation of the installation on one region; a batch move's commit re-keys an entry with its request record ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries) step 3) |
| | `SH‖shard‖fw‖ref` | (cluster) forwarding entry: a tagged reference to a record a batch move took to another shard — a state reference's token, or an `OpenID` — → the receiving shard and its `GraceInstance` at the commit, which the routing error carries; written by the batch's commit at the giving shard, deleted lazily once the receiving shard reports no such record, by a periodic sweep, and by a move back of the record (RFC 14 §10) ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries), [RFC 14 §10](rfc-14-open-state.md#10.%20Shard%20placement)) |
| | `SH‖shard‖xh‖opID` | (cluster) cross-shard hold record: a participant's hold for one prepared operation, with its deadline, written by a fenced transaction that also rewrites, at the shard's current epoch, the fences of the files it holds that still name an older one, so a primary that stayed through a raise leaves the coordinator's commit no stale fence to fail on; every commit of the operation guards it, and a release deletes it before the participant grants what it refused ([RFC 11 §8.1](rfc-11-ownership.md#8.1%20Operations%20across%20shards)) |
| | `SH‖shard‖rm` | the share removal's progress on this shard: step 3, offloaded, forgotten, step 4 with its cursor, or done ([§2.3.2](#2.3.2%20Removing%20a%20share)); absent before the shard's step 3 starts |
| | `SH‖shard‖hw` | (cluster) each replica's acknowledged committed-point mark, persisted by the primary on a period; never lowered, raises no epoch ([RFC 10 §7.2](rfc-10-journal-replication.md#7.2%20Removal)) |
| | `J‖journal` | a journal, keyed by journal identity: the node and device it lives on, written when the journal is created and before anything in it is acknowledged, so a start that finds no journal or another one there refuses its shares ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)); and, from the replication extension on, its generation, raised at every open, lease acquisition and lease renewal, and by a primary that finds itself rolled back ([RFC 10 §2.2](rfc-10-journal-replication.md#2.2%20One%20journal%20carries%20many%20shards)) |
| | `RS‖partition` | (cluster) repair scheduler lease per partition of the shard-ID hash, with its epoch ([RFC 10 §7.4](rfc-10-journal-replication.md#7.4%20Repair)) |
| | `MV‖giving‖receiving` | move record: the cursor of a move of files from the giving shard to the receiving one, one per pair, so a crashed move resumes and several moves out of one shard run at once ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) |
| | `MKP‖masterKeyID‖ns‖moveID` | export pin, keyed by the version-qualified master key ID: a master key version the export of a move of namespace `ns` to another installation carried a wrapping under; while one exists the material provider refuses to destroy that key, and only an operator's `ReleasePin` deletes it ([RFC 27 §2.4](rfc-27-namespace-migration.md#2.4%20Key%20scope%20and%20material)) |
| | `SLOT` | (cluster) slot table: the fixed slot count and each slot's ordered nodes, primary first, in distinct failure domains, weighted by node capacity; one per installation, changed only by compare-and-swap ([RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards)) |
| | `CFG‖scope‖key` | Setting. A backup location's record ([RFC 13 §2.5](rfc-13-configuration.md#2.5%20A%20backup%20location%20is%20its%20own%20record)) is one, and carries two system fields the control plane writes when it creates the record: the put-integrity outcome, and the **location health object's identity** — the version of `<location>control/health` at an immutable location, the nonce written in it at a mutable one ([RFC 4 §4.7](rfc-4-remote-tier.md#4.7%20Health%20is%20one%20probe%20call)) |
| | `CFGGEN` | settings generation: one installation-wide counter, raised in the transaction of every `Setting` or `Secret` change; every node point-reads it each second and re-reads the records when it moved ([RFC 13 §5](rfc-13-configuration.md#5.%20Binding%20classes)) |
| | `SEC‖id‖version` | Secret (§2.3), one key per version, the previous one kept only until every node reports the current one: envelope-encrypted, never dumped or exported |
| | `CL‖clientID`, `CL‖clientID‖sh‖shard` | durable Client record. Its fields are **exactly** these, and the store **MUST** write no other: protocol, name, boot, owner principal (zero when the client names a machine or host, or its principal maps to the anonymous one), whether state protection is on (`Protect`), recovery kind (`Recovery`), the `Adapter` bytes — opaque, at most 4 KiB, version first, never read by the store, written in the transaction of the change they belong to ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)) — and (cluster) lease owner node, back-channel node, back-channel-down flag, session holders and the ended mark ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)). `Client.Expires` **MUST NOT** be written: it is volatile, held by the lease owner, so a renewal writes nothing; written, it would make 10⁵ clients cost 10⁵ writes per lease period. A pending confirmation is never written. The record is rewritten only when one of its fields changes. One key per shard the client holds state in (`Client.Shards`), checked on reclaim, whose value is `Done`: the grace instance the client last finished reclaiming in there, or the shard's instance when it joined, so a client owes a reclaim while it is below the shard's `GraceInstance` and a failover writes no client record ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)). An administrator's revoke deletes every such key in its transaction, so a reclaim after a restart finds none ([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)). (cluster) An expired record is kept, marked ended, until every shard it names has released the client's state ([RFC 14 §4.3](rfc-14-open-state.md#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)). A client that recovers by notice keeps where its notice goes in its `Adapter` bytes, so these records are the restart-notice monitor list ([RFC 14 §4.5](rfc-14-open-state.md#4.5%20NLM%20locks%20and%20restart%20notification)) |
| | `CLO‖protocol‖name` | client name index: the client's own stable name → `ClientID`, unique, so a client naming itself finds its record after any restart ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)) |
| | `IDS‖kind` | ID allocator, one per kind (client, open, lock): the next unreserved ID, reserved in ranges by the process that issues them, so an ID is never issued twice and issuing one writes nothing; a range left unused when the process stops is abandoned. A `ClientID` and an `OpenID` are unique across the installation ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)); an `OpenID` is issued only to an SMB, durable or clone source open, never to every NFS open, and below 2⁴⁰: `IDS‖open` refuses to issue past it ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)); a `lockID` need only differ from every other lock under its open and offset, which a server-wide counter more than meets. Three more kinds, uid, gid and RID, are the allocation counters of principals' spellings (RFC 18 §2.3): each only rises, and is read and raised in the transaction that creates the `User` or `Group` it numbers, as `S‖id‖pjn` is, never reserved in ranges; a uid or gid is drawn from `identity.id_range` and one any principal holds is skipped, which its `PX‖` row shows, so no allocated uid, gid or RID is given twice, even after its principal is deleted |
| | `N‖node‖st` | NodeStatus (§2.3): what the node has applied, refused and holds restart-pending, and its conditions; written only by that node (RFC 23 §2.4) |
| | `OP‖opID` | Operation (§2.3), deleted 7 days after it ends (RFC 23 §4.6) |
| | `OPR‖kind‖target` | running-operation index: one per target and operation kind → `opID`, written when the operation starts and deleted when it ends, so a second request for the same work finds it in one read and is refused (RFC 23 §4.6); `kind` the operation's kind as a string, `target` tagged |
| | `IK‖account‖key` | IdempotencyRecord (§2.3), written in the request's own transaction (RFC 23 §4.5) |
| | `XP‖expiry‖tagged` | expiry index for the management API's records that expire: one empty key per idempotency record (tag 0x01, account and key), finished operation (0x02, `opID`) and session (0x03, session ID), written and deleted with it, so the sweep is one ordered scan |
| | `SS‖session`, `SSH‖hash`, `SS‖session‖h‖hash` | Session (§2.3); refresh-token index: a refresh token's hash → session ID, the current one and every one it replaced, until the session expires, so presenting an invalidated token finds the session it must revoke (RFC 23 §5.3); and its reverse, one empty key per hash the session has retained, written with its `SSH` row, so the expiry sweep (`XP`, tag 0x03) deletes the session's `SSH` rows by one range of its own, never a scan of `SSH` |
| | `AT‖tokenID`, `ATH‖hash`, `ATA‖principal‖tokenID` | APIToken (§2.3); token index: the hash → token ID; per-account index, for listing and for deleting an account's tokens with it |
| | `AU‖time‖auditID` | AuditRecord (§2.3), in store-time order, so a time-range read and the retention sweep are one range; never rewritten (RFC 23 §6) |
| | `AUX‖tagged‖time‖auditID` | audit index: one empty key per record for each of its actor (tag 0x01, principal), target (0x02, resource kind and ID) and taking node (0x03, `NodeID`), so the reads RFC 23 §6 filters by and the per-node export RFC 25 §4.3 reads are ranges |
| | `AUC‖node` | AuditCursor (§2.3): one per node that takes requests, written first by that node's first export and changed only by conditional writes naming the holder it read; it outlives the node's lease, and is deleted with the node's record once its last audit record within `audit.retention` is exported (RFC 25 §4.3) |
| **Store** | `\x00format` | store format record (§4.6) |

The table is **exhaustive**: every key the store writes has a row. A new
record kind is a format bump (§4.6) and gets its row in the same change — the
management API's records, `N‖node‖st` and `OP` to `AUC`, arrive with one;
volatile open state (locks, caching grants, watches, layouts, asynchronous copies, non-durable opens, delete pending without a persistent open) and
the primary's in-memory tables (quota reservations, routed-request dedup) are
never written to the KV and so have none. A file's shard and its primary's epoch
need no row of their own: the shard is a `File` field and the epoch is in the
fences.

> ponytail: every process issuing client, open and lock IDs reserves from the
> same three `IDS‖kind` keys, one write per range, so in a cluster they are the
> only server-wide keys an open's path can contend on. Widen the range, or give
> each node a key of its own, when range reservations show up in a profile.

Consequences:

- `Files.Get` costs what [RFC 7 §2.5](rfc-7-namespace-metadata.md#2.5%20Where%20%60size%60%20lives) states, the one statement of it: one
  read of the File's one key, plus, for a directory, a scan of its unfolded
  time deltas. The File was two keys (attributes, and
  the write path's fields) so that `chmod` and a write never conflicted; the
  second key cost 25% on `GETATTR`, 60% on `READDIRPLUS` and 30% on serial
  creates (Appendix A), to avoid a conflict that is rare and cheap to retry.

  > ponytail: one key per File, so `chmod` and an existence commit on one file
  > conflict and one retries. That holds only for one-shot writers of the
  > `File`: a removal's phase-2 batches, which run in a stream beside a busy
  > writer, never write it, and only the transaction that sets the removal done
  > does ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)). Split the write path's fields into their own key
  > when a profile shows those retries.
- Every per-file key carries the `ShareID`. Exporting a share is a prefix scan,
  and deleting one walks the prefix in batches (§2.3.2), instead of a tree walk; a share's files stay together for
  placement; and a restore into a new share gets new keys, so it can never
  alias a handle to the original (§5, decision 1). Prior art prefixes
  by volume for the same reasons (Appendix B). The handle already names
  the share, so the prefix costs no lookup.
- A directory's entries sit under the directory's own prefix, so listing is one
  prefix scan and a create in it touches the parent's prefix and the child's.
- Every per-file operation stays inside one or two `F‖` prefixes. Only the
  offload commit, removal batches and GC reach the content-addressed keys, and
  none of them is on a client's path. If metadata is ever split across
  servers, this is the line to split on.
- A share's export is the prefix of its keys — `S‖ShareID` and
  `F‖ShareID` — plus the server-wide records its principals name. The `F‖`
  prefix holds every file of the share, not only those its tree reaches: a file
  kept only by a snapshot's history, and an unlinked file kept by an open, with
  its pending release, are under it too, so an import loses neither. Nothing
  server-wide is copied wholesale into another installation. An import maps
  principals by `PrincipalID` and **MUST** refuse one whose ID already names a
  different principal, whose protocol ID is already mapped to another, or whose
  name already names another; share names, paths and netgroups are checked as
  §2.3.1 states.
- Content-addressed keys carry the namespace, so two namespaces never share a
  chunk record, a count, a candidate or a GC pass, although one database holds
  them all ([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)).
- Two GC index kinds are **sequential within a partition, by design**:
  `BR‖ns‖b‖not_before‖name` orders one partition's retired blocks by time, and
  `BC‖ns‖b‖bucket‖name` puts one partition's candidates of a dead-ratio bucket
  in one range. `b` is the block name's first byte, the unit GC partitions by
  ([RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix)),
  so a partition's holder reads only its own range, and the layout does not
  depend on the partition count. Within a partition GC reads the due blocks and
  the best candidates with one ordered scan each; across partitions the inserts
  spread over up to 256 tail ranges per namespace, at the rate blocks retire or
  change bucket — a background rate, batched, never a client operation's.
  Every other content-addressed key leads with a hash or a nonce-unique name
  and spreads evenly.

#### 4.2.1 Key encoding

The labels in §4.2's table are names for bytes. A key is the concatenation
(`‖`) of its components' encodings, and every encoding preserves order and
delimits itself, so two keys compare byte for byte as their component tuples
compare, and the keys under a prefix are exactly those whose leading components
are the prefix's. No separator byte is written.

- **Kind**: one byte, from the first table below. Labels of different lengths —
  `S`, `SH`, `SEC`, `SLOT` — are different bytes, so no kind is a prefix of
  another.
- **Sub-kind**: one byte, unique among its siblings at its position, from the
  second table. The labels are names only, so `rq` and `rqx` are two codes like
  any other two.
- **Integer** — offset, `start`, cut, `born`, `died`, shard, generation, a client, open or lock ID,
  bucket, digest, epoch, sequence, a GC partition, a record's own `Version` (a Secret's, a
  grant's), a repair partition (`RS‖partition`), a POSIX uid or gid (`PX‖uid`,
  `PX‖gid`, zero-extended): unsigned, 8 bytes, big-endian, so refs scan in offset order and
  one record's history in `died` order. A time (`not_before`) is store time as unsigned
  nanoseconds since the Unix epoch, encoded the same way.
- **Journal version** — every content version the journal assigned, wherever it
  is stored: `F‖id‖rm‖version`, `S‖id‖vf‖bucket‖version‖FileID`, and in values a
  ref's `oldest` and `newest`, an overwrite record's and a Hole's `version`, a
  File's `Version` and `Applied`, a clone spec's `asOf` and a clone source open's `CloneVersion`, a share's version limit. A `JournalVersion`, 16 bytes big-endian,
  the epoch half then the counter half, so it sorts as the one 128-bit number
  [RFC 1 §4.3](rfc-1-journal.md#4.3%20Versions) compares. In this format the epoch half is zero: a key or
  value carrying a non-zero epoch fails to decode, as a journal record carrying
  one fails the open, and is reported as a codec error (§4.3), never read as a
  smaller number.
- **Identifier** — FileID, ShareID, PrincipalID, NamespaceID, NodeID, JournalID,
  a backup's ID, and the use, operation, move, session, API token and audit
  record IDs: raw bytes at its fixed length, 16 for every minted ID, a Secret's
  ID (`SEC‖id`) among them. A client ID, an open ID and a lock ID are integers,
  issued from `IDS‖kind`, at the widths [RFC 14 §2.11](rfc-14-open-state.md#2.11%20State%20references) gives them — an open ID below 2⁴⁰ — 8 bytes each, the
  widths the adapters carry them in on the wire ([RFC 21 §11.4](rfc-21-nfs.md#11.4%20Open%20state),
  [RFC 22 §16.4](rfc-22-smb.md#16.4%20Open%20state)). A request ID is a byte
  string, since it is composed of several fields
  ([RFC 15 §4.3](rfc-15-topology.md#4.3%20The%20route%20envelope)). `ChunkHash` and `BlockName` are raw
  at the fixed lengths [RFC 6](rfc-6-block-metadata.md#2.%20The%20records) gives them.
- **Name byte** — the `b` of `BR‖ns‖b‖…` and `BC‖ns‖b‖…`: one raw byte, the
  block name's first, repeated ahead of the time or bucket, so a contiguous
  range of `b` is one GC partition's keys whatever the partition count
  ([RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix)).
- **Byte string** — an entry's folded key, an xattr name, an idempotency key (`IK`), an operation or resource kind (`OPR`, `AUX`), a user, group, share
  or netgroup name, a path, a setting key, a SID, a Kerberos principal, a
  client owner, a backup location's name (`NS‖ns‖bk‖location`, and `NS‖ns‖keyseq‖target`, empty for the namespace's own object), a namespace key ID (up to 255 bytes, [RFC 5](rfc-5-transforms.md)), a master key ID
  (`MKP`'s `masterKeyID`, [RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)): each `0x00` byte written as `0x00 0xFF`, then the two-byte
  terminator `0x00 0x01`. Inside a string `0x00` is always followed by `0xFF`,
  so the terminator cannot occur there: no string's encoding is a prefix of
  another's, whatever component follows. A string sorts before every longer
  string it begins, since `0x00 0x01` sorts below `0x00 0xFF` and below any
  other byte that can follow.
- **Tagged** component — an expiry entry (`XP`), an audit index entry (`AUX`), a resource target (`OPR`), a quota's `target`, a setting's `scope`, a forwarding
  entry's `ref`: one tag byte from the last row of the second table, then the
  tagged value in its own encoding above — a PrincipalID, a project number as an
  integer, a default's principal kind as its enumerated byte, nothing for a share-wide quota or the installation scope, the scope's
  ID as an identifier, a state reference's token or an `OpenID` as an integer. The
  empty value keeps the share-wide quota's key off the prefix of every other
  quota key: its tag byte differs. `unique` in a delta key is the
  writer's NodeID, node epoch and sequence, 32 bytes.
- **Suffix** — the history key `F‖id‖H‖suffix‖died` and the died index's
  trailing `suffix` hold the live key's own encoding after `F‖id`. In the
  history key the File itself, whose live suffix is empty, takes the byte 0x00,
  which no sub-kind code uses, so its `died` cannot be read as another record's
  sub-kind; every other suffix is prefix-free already, its sub-kind code fixed
  and its components fixed-length or terminated.

| Scope | Kind codes |
| --- | --- |
| Store | `\x00` 0x00, followed by the bytes `format`; nothing else is under 0x00 |
| Per file, per share | `F` 0x01, `S` 0x02 |
| Per namespace | `C` 0x10, `CR` 0x11, `B` 0x12, `BR` 0x13, `BD` 0x14, `BC` 0x15, `I` 0x16, `NS` 0x17 |
| Server-wide | `U` 0x20, `G` 0x21, `M` 0x22, `MR` 0x23, `GR` 0x24, `NX` 0x25, `NP` 0x26, `NG` 0x27, `SL` 0x28, `IN` 0x29, `NSM` 0x2A, `PX` 0x2B, `N` 0x2C, `SH` 0x2D, `J` 0x2E, `RS` 0x2F, `MV` 0x30, `SLOT` 0x31, `CFG` 0x32, `CFGGEN` 0x33, `SEC` 0x34, `CL` 0x35, `CLO` 0x36, `TG` 0x37, `MKP` 0x38, `IDS` 0x3A, `PGEN` 0x3B, `OP` 0x3C, `OPR` 0x3D, `IK` 0x3E, `XP` 0x3F, `SS` 0x40, `SSH` 0x41, `AT` 0x42, `ATH` 0x43, `ATA` 0x44, `AU` 0x45, `AUX` 0x46, `AUC` 0x47; 0x39 retired |

| Under | Sub-kind codes |
| --- | --- |
| `F‖id` | none for the File itself; `acl` 0x01, `x` 0x02, `s` 0x03, `e` 0x04, `t` 0x05, `h` 0x06, `rm` 0x07, `ov` 0x08, `ref` 0x09, `H` 0x0A, `fx` 0x0B, `fo` 0x0C, `rel` 0x0D, `op` 0x0E, `dp` 0x0F; under `op‖openID`, `l` 0x01 |
| `S‖id` | `info` 0x01, `st` 0x02, `nsg` 0x03, `g` 0x04, `xp` 0x05, `snap` 0x06, `cut` 0x07, `live` 0x08, `sc` 0x09, `hd` 0x0A, `hr` 0x0B, `use` 0x0C, `rh` 0x0D, `or` 0x0E, `ut` 0x0F, `pu` 0x10, `pj` 0x11, `ud` 0x12, `qt` 0x13, `qx` 0x14, `vf` 0x15, `fnc` 0x16, `vl` 0x17, `shs` 0x18, `pr` 0x19, `rr` 0x1A, `do` 0x1B, `od` 0x1C, `pjn` 0x1D, `gv` 0x1E |
| `NS‖ns` | `gc` 0x01, and under it `lease` 0x01, `recheck` 0x02, `hold` 0x03, `suspect` 0x04, `forward` 0x05, `pause` 0x06, `cursor` 0x07; `claim` 0x02, `pk` 0x03, `key` 0x04, `bk` 0x05, `drain` 0x06, `keyseq` 0x07, `park` 0x08, `backup` 0x09 |
| `N‖node`, `SH‖shard`, `CL‖clientID`, `SS‖session` | `exp` 0x01, `clk` 0x02, `st` 0x03; `rq` 0x01, `xh` 0x02, `hw` 0x03, `rm` 0x04, `fw` 0x05, `rqx` 0x06; `sh` 0x01; `h` 0x01 |
| `NS‖ns‖gc‖cursor` | walk: audit 0x01, block walk 0x02, index rebuild 0x03 |
| `CLO` | `protocol.Kind`'s byte, one per protocol ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)): NFSv3 0x01, which holds no client record, NFSv4 0x02, its minor versions one kind, NLM 0x03, SMB 0x04 |
| `IDS` | kind: client 0x01, open 0x02, lock 0x03, uid 0x04, gid 0x05, RID 0x06 |
| enumerated components | `NX` kind: user 0x01, group 0x02, share 0x03; `PX` scheme: `uid` 0x01, `gid` 0x02, `sid` 0x03, `krb` 0x04, `oidc` 0x05; `NS‖ns‖key` kind: `chunk-id-key` 0x01, `chunking-key` 0x02, `export-key` 0x04, `data-key` 0x05 (0x03 unassigned); quota `target` tag: principal 0x01, project 0x02, share 0x03, default 0x04, its kind user 0x01 or group 0x02; forwarding `ref` tag: stateid 0x01, `OpenID` 0x02; `XP` tag: idempotency 0x01, operation 0x02, session 0x03; `AUX` tag: actor 0x01, target 0x02, node 0x03; resource `target` (`OPR`, `AUX`): the resource kind as a string, then its ID as a byte string; `CFG` scope tag: installation 0x01, node 0x02, namespace 0x03, remote store 0x04, share 0x05 |

This encoding, both tables and every fold rule are part of the store format
(§4.6): changing any of them is a format bump, and a code once assigned is never
given to another kind. Golden vectors pin them (§6.3).

### 4.3 Codecs

Each persisted type has one codec: a version byte followed by a fixed field
order. The per-value version byte is the **only** codec-version mechanism: a
lazy upgrade leaves old and new values side by side, and each decodes by its
own byte. A binary writes only the codec versions the store's *active* format
names (§4.6), never a newer one it merely knows, so a value written before the
format is raised decodes under the older binary too. Decoding refuses an unknown
version rather than guessing. A codec never
embeds a list that grows with a file (§1.2 rule 2); a history ref, a hole and an
xattr are each their own key.

### 4.4 Counters that many writers change

**A counter is never read and rewritten by the transaction that changes it.**
Measured on the embedded store (Appendix A), one shared counter updated inside
every create cut throughput 4–7× with 11–43 retries per create; 16 stripes
recovered most of it at 16 writers but still halved it at 64. The replicated
store serialises writes to one key outright, so a hot counter there caps a
share's create rate.

Instead, a transaction that changes charged bytes writes a **delta record** —
a new key, unique to the transaction (`S‖id‖ud‖shard‖unique`), holding the
changes to the share's, the file owner's, the group's and the project's usage. It
reads nothing, so it conflicts with nothing: measured, it ran at the rate of no
counter at all. The `unique` part **MUST** be unique across writers (the
primary's node ID and epoch, then a sequence): a delta written under another's
key replaces it, and the retry the write-write conflict forces writes the
replacing delta again, so one is lost.

**The shard's primary folds, into its shard's own totals.** Each shard's
primary ([RFC 11](rfc-11-ownership.md)) folds the deltas its shard wrote — usage deltas, and a
re-home's `old_refs` deltas (`S‖id‖od‖shard‖unique`) — in batches, one transaction
per batch, deleting the deltas it folded and adding them to totals kept **per
shard** (`S‖id‖ut‖shard`, `S‖id‖pu‖principal‖shard`, `S‖id‖pj‖project‖shard`,
`S‖id‖or‖shard`). A fold transaction guards `SH‖shard`, so a former primary's
fold conflicts with the takeover that rewrites it; a fold is background work, so
this one record per shard is paid per batch, never per client operation. No two
shards' folds write one key, so no client operation and no fold writes a
share-wide usage key ([RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards)). A reader — `statfs`, a quota check — sums
the per-shard totals by one prefix scan, plus the deltas not yet folded; the
fold keeps that set small, and `metadata_unfolded_deltas` is the alert when it
does not.

> ponytail: a usage read scans one total per shard of the share, O(shards),
> and a quota check is one, on the write path. Add a share-wide total folded
> from the per-shard ones on a slow period, and cache each principal's usage at
> the primaries on the fold period, when a share with many shards shows that
> scan in a `statfs` or quota-check profile.

**Charging is logical bytes per file.** A file is charged its logical size
excluding holes, in full, whether or not its content is shared with another
file by a clone or by deduplication; deduplication savings are the share's,
never a principal's. The charge is `File.Charged` ([RFC 7](rfc-7-namespace-metadata.md)): every existence
commit and removal that changes it writes the difference as a usage delta in
the same transaction, and `chown` moves the whole `Charged` from the old owner
and group to the new ones. So a charge is always read from one field, never
recomputed by scanning a file's holes. The one bounded read of holes is a
removal's first phase, which lowers `Charged` by its extent's bytes less the
holes among them and reads at most K holes to know them; past K it lowers
`Charged` by the whole extent below `size`, inside parts of the edge holes it
narrows, joins or replaces included — the removal's `holeCredit`. Its later
batches never write the `File`: they gather the masked holes' bytes, and a
clone's batches the source holes' bytes they lower the charge by, in the
removal record's `charge`, and the transaction that sets the removal done folds
`charge` into `Charged` and writes it as a usage delta, once. Whichever
transaction drops or replaces a masked hole gives its bytes back when the
oldest removal not done that masks it has `holeCredit`: a phase-2 batch into
its own removal's `charge`, an existence commit or a phase 1 into `Charged`
directly ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)). So usage reads low under a hole credit, and a clone's high,
only until the removal is done, and is exact then ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation), [RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy)).
The `File` record, which phase 1 writes and every existence commit of the file
reads, orders that read against those commits.

The deltas are in the same transaction as the change, so a crash loses
nothing and needs no rebuild.

**Quota is enforced by reservation.** Bytes are charged at the existence
commit, but a write is admitted long before, so a check against committed
totals alone lets every staged, uncommitted byte in the journal through. Each
primary therefore holds an in-memory **reservation** per principal, per
project and per share: a write reserves the bytes it may add before it is acknowledged, the
check is totals + unfolded deltas + this primary's reservations against the
limit — the principal's own quota or else the share's default for its kind, the project's, and the share-wide quota's
(`S‖id‖qt` with the share tag), whose usage is the sum of the per-shard
`S‖id‖ut‖shard` totals — and the reservation is released when the existence
commit charging those bytes lands. A crash drops the reservations with the uncommitted writes they
covered, and replay re-reserves what it replays.

> decision: quota fails open by a stated bound, not exactly. One primary sees
> only its own reservations, so where one principal, project or share is written
> through several shards concurrently the overshoot is at most the reservation slack each such
> primary may hold (`min(S, f × Hard)`, so a small quota gets a small one,
> [RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota)) beyond the committed total, summed over those primaries —
> what caps each primary's reservations is the bytes it has admitted whose
> existence has not yet committed; with one shard it is zero once every
> removal is done.
> A removal still running under a hole credit reads usage low by the bytes it
> has yet to give back, on any number of shards, until it is done
> ([RFC 6 §6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)). Tighten it by leasing each primary a slice of the remaining
> quota if a deployment shows overshoot past its slack.

**Directory times are the same problem.** Every create, unlink and rename in a
directory updates its `Modify`, `Change` and `Version`; measured, that
read-and-rewrite cut parallel creates in one directory from 111k/s to 28k/s,
whatever the file layout. Directory time changes are therefore delta records
under the directory (`F‖id‖t‖unique`), folded by the primary of the directory's shard the
same way, and a directory's `Get` applies any unfolded ones — usually none. A
delta does not replace the conflict the old rewrite gave for free: the
structural operations guard the parent instead, and a directory's removal
range-guards its entries ([RFC 7 §3.6](rfc-7-namespace-metadata.md#3.6%20A%20structural%20change%20guards%20the%20directory%20it%20depends%20on)). The
directory's primary starts a fold only while no create, link or rename-into of
that directory is in flight, and admits none until the fold commits, so a fold
never aborts the creates it folds ([RFC 7 §9.2](rfc-7-namespace-metadata.md#9.2%20Timestamps)).

**A directory-time delta carries the cut it was written after**, the share's
`k` its transaction read ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). Snapshot *k* sees a directory as the
version of its `File` visible at *k* plus the unfolded deltas with `born < k`.
For that to hold, the fold **MUST** fold deltas in `born` order, one `born`
value per transaction, and write the folded record with that `born`; the value
it supersedes moves to history with `died` = that `born` when a live cut sees
it. A transaction that rewrites the directory record itself (a `SETATTR`, a
rmdir) first folds, in the same transaction, the directory's deltas with
`born` below the `k` it read; those committed before the current cut, so the
scan that finds them races no writer. Usage deltas carry no cut: quota charges
live bytes only ([RFC 12 §2.9](rfc-12-snapshots.md#2.9%20Space%20is%20reported%2C%20not%20charged)). History is counted beside them: every
transaction that moves a ref to history or drops a history ref writes the change
to the share's `history_bytes` in its usage delta, folded like the rest and
reported, never charged. This includes the offload commit, which writes only
`history_bytes` and `old_refs` changes there, never charged bytes; a delta key
is unique to its transaction, so it shares no key with the write path.

> ponytail: one folder per shard, run by the shard's primary. Fold throughput
> caps the sustained rate of charge-changing transactions per shard; split the
> fold by key range when the unfolded-delta count stays high in a profile.

### 4.5 Debugging

Because every key belongs to one entity and every value has one codec, one tool
can walk a prefix and print entities as JSON: `dump file <id>` prints the File,
its ACL, xattrs, streams, entries, holes and refs; `dump chunk <hash>` the chunk,
its block and its refcount against a recount; `dump user <name>` a user, its
memberships and its grants. `Secret` records are never printed: `dump` shows
that one exists, and its key ID, not its contents. No other debug path should
read raw keys.

### 4.6 Store format

One record, `\x00format`, holds one integer, the store format, and the fold
rules the store's shares use. It is the **only** record of the store format:
the store format's active version is this integer, and the control plane's gate
raises it by rewriting this record, in the transaction that checks every
registered node's range. The explicit initialisation step writes it, with the
Installation record, when it creates the store (`Init`, §3.2). Opening a store **MUST** read it first and refuse a
format outside the range the binary reads and writes, before reading any other
key. A new record kind, a change to the key encoding (§4.2.1), or permission to write a new codec
version is a format bump: format *n* names, in code, the codec versions a binary
may write, so no value is written that an older reader of format *n* cannot
decode.

**A store is never upgraded by being opened.** The format a store is written in
is the store format's *active version*, the integer in `\x00format`, and it is
raised only through the control plane's gate, which refuses while any
registered node's range lacks the new version ([RFC 13 §5.5](rfc-13-configuration.md#5.5%20A%20node%20joins%20only%20where%20its%20versions%20overlap)). A newer binary
opening an older store keeps writing the older format, on a single node as in a
cluster, and records its own range in its node record. Until the raise, the
older binary still opens the store, so rolling back a single node is
reinstalling the old binary; after it, the older binary refuses the store and
says why. A format change that would let a newer binary read only a store it has
rewritten is not a format bump but a migration, run as its own operator step.
Each node's ranges sit in its node record, the store format's active version in
`\x00format` and the others in the Installation record (§2.3), so the gate reads
all three in one scan and raises a version in one transaction. With one record
for the store format, an old binary that reads `\x00format` first finds the
raised version there and refuses; two records could disagree, and the first
check would pass a binary the second refuses.

**Fold rules.** An entry's key is its name folded by the share's fold rule
(§4.2), so the rule is part of the key encoding: a binary that folded one name
differently would miss existing entries and admit duplicates. Each fold rule is
recorded in the format record by ID, and an ID names one 16-bit **upcase
table**: the mapping each UTF-16 code unit of a name goes through, unit for
unit, with no normalisation ([RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case)). A different table is a new ID; a share's case setting names
one ID and is bound ([RFC 13](rfc-13-configuration.md)); a binary that does not implement every
recorded rule **MUST** refuse the store. The identity rule serves
case-sensitive shares.

The first case-insensitive rule's table is **the mapping set of Windows'
`$UpCase` table**, as Windows 7 writes it to a new NTFS volume: 973 mappings,
each a BMP unit to one other BMP unit, every other unit to itself. All 973
agree with Unicode's `Simple_Uppercase_Mapping`, so the table is equally
Unicode simple uppercase over the Basic Multilingual Plane less 225 mappings
Windows does not make:

- 18 folds Windows omits on purpose: `µ`, `ı`, `ſ`, `ς`; the Greek symbol
  variants `ϐ ϑ ϕ ϖ ϰ ϱ ϵ`; the titlecase digraphs `ǅ ǈ ǋ ǲ`, which map to
  themselves; `ẛ`; and the iota forms `ͅ` and `ι`;
- 207 letters newer than the table: Cherokee small letters, Georgian Mtavruli,
  and Latin and Cyrillic Extended additions.

The table is checked in as data and pinned by a hash test; it names no Unicode
version and is never regenerated from one, so neither a toolchain upgrade nor a
newer Unicode release can change a share's keys. It **MUST** be frozen, with its
hash, before the first release that creates a store; any other table, a later
Windows `$UpCase` included, is a new ID. Under it `Ǆ` and `ǆ` fold together
while `ǅ` stays apart, `Straße` and `STRASSE` do not fold, the Kelvin sign
stays apart from `K` ([RFC 7 §3.8](rfc-7-namespace-metadata.md#3.8%20Names%20every%20protocol%20of%20a%20share%20can%20use)), and the long `ſ` and the dotless `ı` stay
apart from `s` and `i`, as on NTFS.

The table maps UTF-16 code units, so it is defined only for names that are
valid UTF-8; a case-insensitive share refuses any other name at the boundary,
whatever its name rule ([RFC 7 §3.8](rfc-7-namespace-metadata.md#3.8%20Names%20every%20protocol%20of%20a%20share%20can%20use)).

> decision: the first table is Windows' own mapping set rather than Unicode's, so a name
> pair that is one file on an NTFS volume is one entry here and a pair that is
> two there is two here; a copy between the two never merges or refuses a name
> the other kept. The 225 Unicode mappings it leaves out were each checked
> against the Windows table, not inferred. Overturned by a client population
> whose Windows version folds a pair this table keeps apart, which would be a
> new table ID rather than a change to this one.

## 5. Decisions and evidence

Each question was put to prior art and, where the embedded store could answer
it, to a benchmark (Appendix A). Sources are linked once per system.

| # | Question | Answer | Evidence |
| --- | --- | --- | --- |
| 1 | Drop `generation`? | **Yes**, with share-scoped keys (§4.2). A restore into a new share gets new keys and cannot alias an old handle; nothing rolls a share back in place ([RFC 26 §1.1](rfc-26-catalog-backups.md#1.1%20Non-goals)), so a released file's handle never resolves again. | JuiceFS and 3FS use never-reused IDs with no generation |
| 2 | `Mode` beside the ACL? | **Keep both.** `Mode` lives on the File so `GETATTR` never reads the ACL; `chmod` follows [RFC 8881 §6.4.1.1](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4.1.1) exactly ([RFC 7](rfc-7-namespace-metadata.md)); a file with no ACL gets one synthesised on read. Reject "last writer wins". | [RFC 8881 §6.4](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4) requires them to agree; OneFS "Balanced", [Qumulo XPP](https://docs.qumulo.com/administrator-guide/authorization-qumulo-core/managing-cross-protocol-permissions-xpp.html); ONTAP mixed style is the cautionary case |
| 3 | Stripe usage counters? | **No — delta records and a fold** (§4.4). Stripes spread collisions; deltas remove them. | Appendix A; [JuiceFS quota design](https://juicefs.com/en/blog/engineering/quota-design-in-distributed-architecture) batches deltas too |
| 4 | Open state durability? | **One rule per entity:** `Client` durable (what reclaim requires, and the client's identity: name, boot, owner, state protection, adapter bytes), except its lease expiry, which is volatile so a renewal writes nothing (§4.2); `Open` volatile and reclaimed in grace, durable when it keeps an unlinked file alive or is persistent; `Lock` volatile unless its open is persistent; `CachingGrant` and `Watch` volatile, never reclaimed. Grace may run per shard. | knfsd `nfsdcld` and Ganesha `rados_cluster` store client records only; CephFS rebuilds caps on reconnect; SMB persistent handles are the one durable case |
| 5 | Stream as File? | **Yes for content, NTFS for identity:** a stream has no owner or ACL of its own, reports its base file's ID to SMB, and is released with it. | NTFS streams are attributes of one file record; ZFS named attributes and Samba `streams_depot` are files |
| 6 | Share in per-file keys? | **Yes**: `F‖ShareID‖FileID` (§4.2). | JuiceFS volume prefix; TiKV range deletion drops a share in seconds, not hours |
| 7 | Nested groups? | **Walk with a depth cap and cache.** Kerberos and AD already deliver the flattened list in the ticket, so no walk for them. Local groups are walked at login, depth ≤ 8 with cycle detection, cached under a key naming every membership it read (RFC 7 §7.5). No stored closure table. | Kerberos PAC; SSSD nesting level; Zanzibar's flattened index shows the closure's write cost |
| 8 | Deleted principals? | Deleting a user removes the User, its memberships and grants, the grants found through their reverse keys (`GR‖principal`, §4.2) without a scan of every share; the principal stays in ownership, ACLs and usage. An admin **reassign** job moves files and their charge through `chown`. A deleted principal's ID is never reissued, because IDs are minted, not derived from a UID or SID (§2.2); a new account reusing the UID gets a new principal. | ONTAP shows raw SIDs in quota reports; NTFS refuses to drop a quota entry while the SID owns files; MS-SMB2 returns zeroed entries for unknown SIDs |
| 9 | Where credentials live? | **In the KV, as `Secret` records** (§2.3), envelope-encrypted under a key held outside it (a file or a KMS), never dumped or exported. Password hashes use a slow hash; an NT hash exists only when NTLM is enabled. A keytab is a `Secret` of kind keytab, sealed under the `protocol` wrapping key like every authentication secret ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)), so every node reads the one keytab from the store; a node whose Kerberos library needs a path writes it at start to a file only that node's process can read, and never back. | Samba `tdbsam`, TrueNAS and ONTAP all keep credentials in their replicated config store; Samba warns NT hashes are cleartext-equivalent |
| 10 | What must a guard promise, and must the embedded store sync every commit? | **A guard binds only its own transaction, is shared in every commit order, and has a range form; the embedded store group-commits and never acknowledges before its sync** (§4.1). No optimistic store checks a later writer against an earlier guard, so a rule that needs the writer to fail tracks a key on both sides; a removal uses `GuardRange`. Relaxed background sync is unsupported, because a released journal extent or a GC deletion cannot be undone. | Measured against §4.1: FoundationDB 7.3.77 and Badger both commit a writer after an earlier-committed guard of its key, and both leave an entry under a removed directory without a range guard; TiKV 8.5.8's shared locks fail a guard taken after another guarding transaction committed, queuing one-directory creates at 26–85/s; Badger with every commit synced and little sharing of syncs reached ~400 commits/s (6.2 ms per sync, consumer NVMe); FoundationDB refuses a value over 100,000 B, a key over 10,000 B and a transaction over ~10 MB counted with conflict ranges, and aborts one older than 5 s; its read version is not time, and it detects no blind write-write conflict without an added read conflict |

> ponytail: nested local groups are walked at login, depth ≤ 8, and cached
> (decision 7). The walk costs one read per membership edge; add a stored
> closure only if login latency for deeply nested local groups shows in a
> profile.

A FileID is never minted near its parent's to place a create on one region:
handles are not MACed, so an ID that can be guessed from its parent's is a
handle a client could forge ([RFC 7 §6](rfc-7-namespace-metadata.md#6.%20Handles)).

The isolation a backend must provide is settled in §4.1. The group a file gets when its creator has no primary group
is settled: the creator's own principal ([RFC 7 §2.4](rfc-7-namespace-metadata.md#2.4%20Attributes%2C%20and%20who%20writes%20them)). Still open, for
measurement on the replicated store:

- the fold's sustained throughput per share (§4.4's ponytail);
- whether `EntriesPlus` batching closes enough of the `READDIRPLUS` gap.

## 6. Testing

The conformance sections of RFC 6, RFC 7 and RFC 14 run these suites; test
tiers are the [index](rfc-index.md)'s.

### 6.1 Two suites, layered like the code

| Suite | Runs against | Proves |
| --- | --- | --- |
| **KV conformance** | every `KV` implementation (embedded, replicated, and an in-memory fake used only by this suite's own tests) | `Update` is atomic and retried; **writer first**: G guards *k* and writes *j*, W writes *k*, both from snapshots taken before either commits; W commits, then G — assert G aborts; **guard first**: the same, G committing first — assert both commit, and that W is ordered after G (a suite asserting W aborts rejects every conforming backend, and no rule may rely on it); **two guards, either order**: T1 and T2 each guard *k* and write a key of their own; commit T1 then T2, then T2 then T1, and once with T1 committing between T2's snapshot and T2's `Guard` — assert all commit; repeat with `GuardRange` over a prefix holding *k*; **range guard**: T guards prefix *P* with no key under it, another transaction creates *P*‖*x* and commits, T commits — assert T aborts; a key created outside *P* does not abort it; **removal against create**, at the KV level: R scans *P* and finds it empty, `GuardRange(P)` and writes *d*; C guards *d* and writes *P*‖*x*; run both commit orders from snapshots taken before either commits — assert exactly one commits and never both, so no key is left under *P* beside a written *d*; drop R's `GuardRange` and the guard-first order commits both; **read-only**: no caller's transaction that calls `Guard` or `GuardRange` writes nothing — the counting wrapper of §6.3 asserts it over the entity suite;  **lost update**: two transactions each `Get` one key and `Set` it to the value read plus one, from one snapshot — exactly one commits first and the other retries, so the key ends two higher; **write skew**: two transactions each `Get` the key the other `Set`s, from one snapshot — one aborts; a blind `Set` of one key by two transactions conflicts, on a backend that checks only reads too; a `Scan` registers nothing, and a range invariant gated on a point record holds (§4.1); `Now` never runs backward across commits in order on one node, nor across a restart with the clock stepped back, and on an idle store it advances with real time; `Versions` returns, for one key written by commits in order, increasing sequences, and for a key the calling transaction wrote or deleted `ErrUncommittedVersion`; `Get`, `Scan` and `Before` of a key the transaction wrote return what it wrote, on a backend over a store that fills versions in at commit too; `Scan` sees a transaction's own writes and nothing uncommitted; `Before` returns the greatest key below its argument under the prefix — a transaction's own writes and deletes included, nothing uncommitted, nothing outside the prefix, `ok` false on an empty prefix or at its first key — and another transaction's later write to the key it returned does not abort the reader; `Limits` is honest (a transaction at each limit commits, one past it is refused, a transaction whose writes fit but whose guards and reads take it past `TxnBytes` is refused, one held open past `Age` aborts); a returned `Update` survives a power loss of the store's node, and on the replicated store the loss of its leader (§4.1, §6.4) |
| **Entity conformance** | the store over each real `KV` | every rule in §1.2 and every invariant of RFC 6 §9 and RFC 7 §10, through the §3 interfaces only; **refusal by a rule**: a create or rename onto a name that exists answers `ErrExist`, and only for a key the request names — seed the store with a state that breaches a constraint no request names (two Files holding one protocol file number, say), then run a request that trips over it, such as a truncate: assert an I/O-class error and a health condition naming the records, never `ErrExist`, since `SETATTR` has no such answer and the breach is the server's own ([RFC 0 §9.2](rfc-0-data-lifecycle.md#9.2%20Conflicts%20and%20their%20retries), I8) |

Routing conformance and the partition-and-kill harness are stated once, in
[RFC 15](rfc-15-topology.md), and are gated on the first view that ships remote; this RFC adds
nothing to them.

The entity suite never reads a key. A test that does is testing the layout,
not the behaviour, and breaks on every layout change for no reason; layout has
its own golden tests (§6.3).

### 6.2 Model-based and property tests

A reference model — plain Go maps holding files, entries, refs and counts, with
no KV — runs the same random operation sequences as the store: create, link,
unlink, rename, write, truncate, clone, snapshot, snapshot deletion, release,
chown, set-ACL.
After every step the store and the model **MUST** agree on every entity reached
through `Inspect`, and after every sequence:

- every file's `Nlink` equals the entries naming it, and every directory's
  `Subdirs`, with its unfolded deltas, the entries in it that name a directory;
- every chunk's `Refcount` equals the live, history and parked refs naming it,
  found by a full walk;
- usage per share and per principal equals the sum of `File.Charged` over the
  files each is charged for;
- every snapshot reads, through `Inspect` at its cut, exactly the files,
  entries, ACLs, xattrs and content the model held at that cut;
- no entity is reachable that the model says is released.

Sequences shrink on failure to the shortest reproducer. The same generator runs
with concurrent clients, and the store's results **MUST** be explainable by some
serial order of the committed operations.

### 6.3 Codecs and layout

- **Golden vectors:** each codec version has fixed bytes for fixed values, in
  the repository, and so does one key of every row of §4.2's table under
  §4.2.1's encoding. A codec or key change that alters them without bumping the
  version or the format fails. A prefix scan of each sub-kind returns only
  that sub-kind's keys, and refs scan back in offset order.
- **Refusal:** decoding a newer version, a truncated value or a trailing byte
  fails with a named error, never a zero value; so does a journal version, in a
  key or a value, whose epoch half is not zero (§4.2.1).
- **Format record:** a store written by version *n* opens under *n+1* and is
  refused by *n−1* (§4.6).
- **No upgrade by opening:** open a format-*n* store under an *n+1* binary, run
  the entity suite, stop, and open it under the *n* binary again: assert it
  opens and every value decodes. Raise the active version through the gate with
  an *n*-only node registered: assert refused, naming the node. Raise it with
  none: assert the *n* binary then refuses the store. A binary that writes its
  newest codec on open fails the first assertion.
- **Version floor per File:** write and commit a file at version 100, `chmod` it,
  and read `VersionFloor`: assert it is the `chmod`'s `Version`. Assert no
  offload commit writes a floor entry, and a create in a directory writes one
  blind entry under a unique key that the fold replaces.
- **Client record writes:** renew 10⁴ clients for ten lease periods.
  Assert no `CL‖` key is written. Replay a setup call after a restart:
  assert it is answered from the record's `Adapter` bytes. Two principals
  connect one machine-named client: assert one record, with no owner.
- **Import collisions:** import a share whose name differs from an existing
  share's only in case, one whose path equals an existing share's, one whose path is under an existing share's and one whose path is above one, a principal
  whose name another principal holds, and a netgroup of the same name with other
  members. Assert each refused before anything is published, naming the
  collision, and each accepted with a new name or path in the request.
- **Installation instance:** start the store on a platform whose
  machine-generation identifier changed. Assert a new `Instance` is minted and
  no namespace is written until an operator names the copy.
- **Cost assertions:** each §3 method has a stated bound on keys read and
  written (for example `Files.Get`: [RFC 7 §2.5](rfc-7-namespace-metadata.md#2.5%20Where%20%60size%60%20lives)'s; `Create`: at
  most six writes). A counting `KV` wrapper records them, and the suite fails
  when a method exceeds its bound. This is what keeps rule 2 of §1.2 true after
  the next refactor.

### 6.4 Faults and crashes

A fault-injecting `KV` wrapper fails, delays or reorders at every call:

- concurrent `Update`s on the embedded store, with power cut after some return: assert every returned `Update` survives, and that the commits shared syncs (fewer syncs than commits);
- a conflict on the Nth `Update` attempt (retry must converge, never apply twice);
- a lost commit reply (the operation must be idempotent from the caller's side);
- a power loss immediately after `Update` returns, on the embedded store's real
  device (every write it returned must read back after restart; a backend whose
  sync is off fails this);
- a process kill between the phases of a batched removal (RFC 6 §6.2),
  resumed on restart;
- a kill with pending releases, unfinished removals and durable opens in a
  10⁶-file share of two shards: assert each restarted or taking-over primary
  finds them by reading only its own shards' ranges of `S‖id‖pr`, `S‖id‖rr`
  and `S‖id‖do`, never a file key outside them, releases and resumes all of
  them, and leaves no index entry behind a record that is gone; move a file
  with a pending release between the shards first: assert the receiving
  shard's primary finds it;
- an epoch change mid-operation (the old primary's writes are refused, namespace
  transactions included, through the fence they guard);
- a share removal (§2.3.2) killed after each of its seven steps and resumed,
  and once failed over to the shard's next primary: assert it finishes, every
  call into the share answers `ErrStale` from step 1 on, a new share can take
  the name at once, and at the end no key carries the `ShareID` but parked refs
  a retained backup needs, every chunk's
  `Refcount` equals the refs the other shares hold, and neither the primary's
  journal nor any replica's holds a record under the share's tag; with two
  shards, kill one shard's primary mid-step 4 while the other finishes: assert
  `st` stays at step 4 until the successor finishes, and no File record of the
  share survives; kill the coordinator between step 6's transactions: assert
  the namespace is still drained; create a file in a call admitted just before
  the mark: assert it is dropped; `detach` right after a cut: assert the newest
  kept snapshot reads its held bytes, not `failed`; kill the coordinator
  between step 6's batches: assert the resumed check still finds `nsg` and
  drains the right namespaces; with two shards in one journal, fail one
  shard's primary mid-offload: assert no shard forgot the tag and the detached
  snapshot reads its held bytes; after the removal, `FreeState` and
  `Renew` of a client that held its references: assert `ErrBadState` and no
  revoked-state flag; lock a snapshot just after step 1 with `delete`: assert
  `ErrStale`; detach during a re-home: assert a clone, catalog backup and move
  of a kept snapshot are refused with `ErrRehoming`;
- a removal racing a namespace pause (RFC 27 §2.2), a cut's announce, a
  re-home's start, a new use record and a second `DeleteShare`, each in both
  commit orders: exactly one wins, and a loser is refused, waits or retries as
  the §2.3.2 table says; remove one tracked read or range guard from step 1
  and the guard-first order lets both commit;
- `DeleteShare` with `detach`: the share ends `retired`, every kept snapshot
  reads as it did, and deleting the last one leaves no key carrying the
  `ShareID`; with the default disposition on a share with snapshots, and with
  `delete` while a backup holds one, it is refused and changes nothing;
- a namespace drained after its last share's removal (RFC 9 §7.6): every block
  is deleted, then its keys destroyed, then its records gone; a share creation
  into it while draining is refused.

## 7. Benchmarks and profiling

### 7.1 What is measured

| Level | Tool | Measures |
| --- | --- | --- |
| **KV** | Go benchmarks over `KV` | per-call latency and throughput, conflict rate under contention, on each backend |
| **KV, durability** | Go benchmark over the embedded `KV` on the reference box | 1, 16 and 64 writers each committing single-key `Update`s, every one synced: commits/s, syncs/s, p99 latency. Gate: at 64 writers commits/s at least 10× the device's measured syncs/s, and p99 within three sync times; a backend syncing per commit fails it (§4.1) |
| **Entity** | Go benchmarks over §3 | per-operation latency, keys read and written, allocations, for: create, lookup, getattr, readdir and readdirplus (10, 10⁴, 10⁶ entries), rename, unlink, set-ACL, commit of an offload, removal of a large file |
| **Protocol** | standard metadata workloads over NFS and SMB mounts | `mdtest` (create, stat, remove, per directory and shared directory), a small-file build tree (untar, compile, `rm -rf`), `ls -l` of a large directory, and a profile-container sign-in storm over SMB: many users at once each opening one large container file with a durable handle and lease and reading then releasing a small metadata file ([Reference workloads](rfc-index.md#Reference%20workloads)) |
| **Scale** | a synthetic namespace | 10⁸ files, 10⁴ shares, 10⁵ principals: operation latency must not grow with total size, only with what each operation touches |

Every benchmark states its backend, machine, and whether the KV was local or
over a network. A number from the embedded KV says nothing about the
replicated one, which adds a network round trip per read and a two-phase commit
per multi-region transaction; each is measured on its own.

> decision: the durability gate's 10× and three sync times are not measured;
> they are what grouping 64 ready commits into a few syncs must reach if it
> works at all, and a per-commit sync sits near 1×. They are proposed targets
> under the index's one rule for benchmark targets
> ([RFC index](rfc-index.md#Test%20tiers)), replaced by the first measured run on
> the reference box.

### 7.2 Targets, gates and regressions

Targets follow the index's one rule for benchmark targets
([RFC index](rfc-index.md#Test%20tiers)): each is stated against something
measured on the same box — here a create or stat on a local filesystem and on a
reference NAS — and replaced by the first measurement. The entity benchmarks run after each merge and the protocol and scale
benchmarks daily, on the reference box; a row more than 10% worse on a measured
path blocks until explained, as [Test tiers](rfc-index.md#Test%20tiers) sets out.

### 7.3 Profiling

- Every operation runs under a profiler label naming it (`op=create`,
  `op=getattr`, …) and its area, so a CPU or allocation profile of a mixed
  workload splits by operation.
- A trace span per operation carries one child span per `KV` call, with keys
  read, keys written and retries as attributes. A slow operation shows whether
  it waited on the KV, on a conflict retry, or on itself.
- The counting `KV` wrapper of §6.3 is also a profiling tool: run it over a
  workload and it reports, per operation, the key prefixes touched — the fastest
  way to find a hot key or a scan that should not be there.

### 7.4 Metadata footprint at 2 PB

What the store holds at the target scale, from the record sizes of §4.2 and
RFC 6 Appendix B, every journal version at its 16 bytes (§4.2.1). These are raw key and value bytes; the backend's own overhead
(per-key headers, compaction's space amplification, replication) comes on top
and is measured, not assumed.

**Assumptions.** 2 PiB of logical content; mean chunk 256 KiB, RFC 2's default
`Target`, so 2 PiB / 256 KiB = 2³³ ≈ 8.6×10⁹ chunks; no deduplication (worst
case: one chunk record per ref); 10⁸ to 10⁹ files, entry names of about 24
bytes; no stored ACL (synthesised from mode); the default 4 MiB block target
([RFC 2 §5.2](rfc-2-carver.md#5.2%20Packing%20rules)), so 2 PiB / 4 MiB = 2²⁹ ≈ 5.4×10⁸ blocks of 16 chunks each. Snapshots: 30 daily cuts retained, 1% of bytes and 1% of files
rewritten per day.

| Record | Key + value (B) | Per | Count | Bytes |
| --- | --- | --- | --- | --- |
| ChunkRef, live | 42 + 105 = 147 | chunk | 8.6×10⁹ | 1.26 TB |
| version-floor index entry | 67 + 0 ≈ 68 | file | 10⁸ – 10⁹ | 0.007 – 0.07 TB |
| Chunk | 49 + 61 = 110 | chunk | 8.6×10⁹ | 0.95 TB |
| reverse ref key (`CR`) | 105 + 0 ≈ 105 | ref | 8.6×10⁹ | 0.90 TB |
| Block, with its carried list (16 chunks of a 4 MiB block × 36 B) | 49 + ~580 ≈ 630 | block | 5.4×10⁸ | 0.34 TB |
| File (attributes and write-path fields) | 33 + 176 ≈ 210 | file | 10⁸ – 10⁹ | 0.02 – 0.21 TB |
| Entry | 58 + 42 = 100 | file | 10⁸ – 10⁹ | 0.01 – 0.1 TB |
| fences `F_x`, `F_o` | 2 × (35 + 9) ≈ 90 | file | 10⁸ – 10⁹ | 0.01 – 0.09 TB |
| history ref + its chunk + its reverse key + its died-index key (snapshots) | 147 + 110 + 105 + 55 = 417 | superseded chunk | 30 × 1% × 8.6×10⁹ = 2.6×10⁹ | 1.08 TB |
| File history + its died-index key (snapshots) | ≈ 270 | changed file | 30% of files | 0.008 – 0.08 TB |

| Totals | Keys | Bytes | Three replicas |
| --- | --- | --- | --- |
| without snapshots, 10⁸ files | 2.7×10¹⁰ | 3.5 TB | 10.5 TB |
| without snapshots, 10⁹ files | 3.1×10¹⁰ | 3.9 TB | 11.8 TB |
| with snapshots, 10⁸ files | 3.7×10¹⁰ | 4.6 TB | 13.8 TB |
| with snapshots, 10⁹ files | 4.2×10¹⁰ | 5.1 TB | 15.2 TB |

Content records (ref, chunk, reverse key: 362 bytes and three keys per chunk)
are about 79% of the total at 10⁹ files, and they scale as
1/`Target`. The reverse index costs about 0.9 TB, a quarter of the total, and is
what makes a recount O(refs of a hash) and lets the deleter check the refs before
every delete ([RFC 9 §3.5](rfc-9-gc.md#3.5%20The%20deleter%20verifies%20before%20it%20deletes)). The block records with their carried lists add about
0.34 TB at the 4 MiB block target: a carried list grows with the chunks per block
as fast as the block count falls, so the block target moves only their 49-byte
keys — about 0.3 TB at 64 MiB, 0.4 TB at 1 MiB — while the chunk `Target` scales
them as 1/`Target`, like the content records. `Target` is the lever, not the file
layout:

| `Target` | Chunks at 2 PiB | Content records | Cold 4 KiB read fetches |
| --- | --- | --- | --- |
| 256 KiB (default) | 8.6×10⁹ | 3.1 TB | ≈ 0.3 MiB |
| 1 MiB | 2.1×10⁹ | 0.76 TB | ≈ 1.2 MiB |
| 4 MiB | 5.4×10⁸ | 0.20 TB | ≈ 5 MiB |

**Recommendation (non-normative).** A share of large, mostly sequential files —
media, checkpoints, backups, a mean file of 1 GiB or more — is better served by a
namespace `Target` of 1 MiB: a quarter of the metadata, at the cost of a larger
cold fetch per small random read and coarser deduplication ([RFC 2](rfc-2-carver.md)).
`Target` is a namespace setting and bound ([RFC 13](rfc-13-configuration.md)), so it is chosen at
creation. Deduplication lowers the chunk rows, never the ref rows.

## 8. Observability

Metric names are shown without the deployment's prefix, which the exporter
adds. Metric names follow RFC 25's conventions, under the prefix `metadata_`.

### 8.1 Metrics

| Answers | Metric | Type |
| --- | --- | --- |
| How much does each transaction touch? | `metadata_txn_keys{op, kind=read\|written}` | histogram |
| Is contention costing us? | `metadata_txn_retries_total{op}`, `metadata_txn_conflicts_total{area}` | counters |
| Is the KV the bottleneck? | `metadata_kv_seconds{call}` | histogram |
| Are transactions near the backend's limits? | `metadata_txn_entries`, `metadata_txn_bytes` against `metadata_txn_entries_limit`, `metadata_txn_bytes_limit` | histograms, gauges |
| Is the fold keeping up? | `metadata_unfolded_deltas{kind=usage\|dirtime}`, `metadata_fold_seconds` | gauge, histogram |
| Store | `metadata_format_version`, `metadata_codec_errors_total{kind}` | gauge, counter |

Every metric here is internal to the store. Per-operation latency is the
filesystem service's `vfs_op_seconds` ([RFC 17](rfc-17-vfs.md)); forwarding and epoch
refusals are [RFC 15](rfc-15-topology.md)'s; open state, recalls and grace are [RFC 14](rfc-14-open-state.md)'s; quota
refusals and overshoot are [RFC 17](rfc-17-vfs.md)'s. Each metric has one owning RFC.

**No share label on per-operation metrics.** At 10⁴ shares, a share label on a
histogram multiplies its series by 10⁴. Per-share figures — usage, quota,
open-state counts — are gauges sampled from the counters that already exist,
exported for the top shares by a stated rule, or read through the management
API. Principal labels are never used.

### 8.2 Logs and events

Expected errors (`ErrNoEntity`, `ErrExist`, `ErrAccess`, stale handle) are
`Debug`; a codec refusal, a refused format or fold rule, and an invariant
violation found by audit are `Error` with the entity ID. Each such event carries the operation's trace ID.

### 8.3 Health and debugging

- Health is derived, not declared: the store is unhealthy when `KV` calls fail
  past a window or the format record cannot be read; degraded when the conflict
  or retry rate crosses a stated threshold.
- The `dump` tool (§4.5) and the `Inspect` view (§3.1) answer "what does the
  store hold for this file, chunk, user or share" without reading raw keys. An
  audit run (RFC 6 §7.5) reports its findings as entities, not keys.


## Appendix A — layout measurements

Embedded store, on-disk, Apple M1 Max (8 performance + 2 efficiency cores),
APFS SSD, Go 1.26.7, 120 B attributes, 40 B write-path fields, 40 B entries;
median of three 2 s runs. Parallel rows varied about ±30% between runs, so only
differences of 2× or more are claimed. The replicated store was not measured:
it adds a network round trip per read and a two-phase commit across regions,
and it conflicts on concurrent writes to one key even when neither read it,
which the embedded store natively does not: under §4.1 its backend adds a read
conflict on each key it writes, so the one-directory rows below, which write
only unique keys, are unchanged by it. The "sync on" row is not evidence of the
rate with every commit synced: a later measurement on consumer NVMe, syncing
every commit, reached about 400 commits/s (§5, decision 10), and the gap is
unexplained. The §7.1 durability benchmark, not this row, gates group commit. The benchmark is kept outside the
repository; its code and full results go with the fold.

| Create | 1 key (inlined) | 2 keys (File + Entry) | 3 keys (File a + d + Entry) |
| --- | --- | --- | --- |
| serial, no sync | 84k/s | 85k/s | 62k/s |
| 16 writers, one directory (retries per create) | 28k (8.5) | 28k (8.9) | 25k (10.9) |
| same, directory times written without reading | — | 111k (0) | — |
| 16 writers, separate directories | 122k | 83k | 95k |
| sync on: serial / one directory / separate | 15k / 9.9k / 16k | 14k / 10k / 15k | 15k / 11k / 17k |

| Read, 200k files | 1 reader | 16 readers |
| --- | --- | --- |
| `GETATTR`, one key | 268k/s | 421k/s |
| `GETATTR`, two adjacent keys by scan | 204k/s | 417k/s |
| `GETATTR`, two reads | 216k/s | 326k/s |
| `LOOKUP`, inlined | 406k/s | 500k/s |
| `LOOKUP`, entry then file | 181k/s | 360k/s |
| `LOOKUP`, entry then a then d | 128k/s | 248k/s |

| Listing 10,000 entries | Time |
| --- | --- |
| names only | 3.0 ms |
| names plus each child's File (one key) | 26.7 ms |
| names plus each child's File (two keys) | 43.7 ms |
| inlined, one scan | 4.9 ms |

| Create plus usage counter | 16 writers (retries) | 64 writers (retries) |
| --- | --- | --- |
| no counter | 109k | 107k |
| one counter, read and rewritten | 27k (11) | 15k (20) |
| 16 stripes, read and rewritten | 90k (0.66) | 44k (3.1) |
| delta record per transaction | 104k (0) | 113k (0) |

## Appendix B — prior art

Non-normative. The systems the decisions of §5 were checked against, and what
each contributed.

| Topic | Systems | What they show |
| --- | --- | --- |
| Inode and entry as separate records | JuiceFS, 3FS, Tectonic keep them apart; CephFS and HopsFS inline | inlining wins lookups and costs hard links (CephFS stray directories, HopsFS none at all) |
| Volume prefix on per-file keys | JuiceFS prefixes every key by volume; TiKV range deletion, Badger `DropPrefix` | deleting or moving a volume is a prefix operation |
| Handles without a generation | JuiceFS and 3FS never reuse IDs; ZFS handles survive rollback | never-reused IDs make the generation redundant |
| Mode beside an ACL | OneFS "Balanced", Qumulo XPP merge `chmod`; ONTAP mixed style is last-writer-wins | merging keeps both views consistent; last-writer-wins is hard to operate |
| Quota enforcement slack | ZFS (seconds), ONTAP FlexGroup (about 5%), CephFS and Lustre (by design) | exact enforcement across writers is not attempted anywhere |
| Counter batching | JuiceFS batches quota deltas per client | a hot counter key is avoided, not striped |
| Logical-bytes charging | VAST, OneFS, ONTAP, NTFS | a principal's usage never depends on deduplication |
| Credentials in the config store | Samba `tdbsam`, TrueNAS, ONTAP | kept beside users, sealed |
