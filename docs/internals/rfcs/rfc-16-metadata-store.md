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
2. **Minutes later the range is uploaded.** A put intent naming the new block
   is recorded before the put. After the put, the offload commit writes a ref
   ("these bytes are chunk C"), the chunk record, the block record and the
   index entries beside them. It never touches `f7`'s File
   record or its overwrite record, so uploads and alice's writes never contend
   for one key.
3. **alice-pc creates the folder `Downloads`.** One transaction writes the new
   directory's File record, one Entry under `d1` keyed by the folded name
   `downloads` and holding `Downloads`, a small *delta* record for `d1`'s
   times, and a usage delta for the new file. It guards `d1`'s record rather than rewriting it.

Two designs this avoids, both measured. Rewriting `d1`'s times in place makes
every create in the folder write one key, and parallel creates in one directory
fell from 111k/s to 28k/s; a delta per transaction, folded in later, kept the
full rate. Keeping a file's refs as a list inside its record makes each upload
re-read and re-write the whole list, which is how a long sequential write
slowed down as the file grew; one key per ref keeps each commit's cost to what
changed.

```text
 filesystem service      engine          GC        management API
 Namespace, Files,       Existence,      Blocks    ControlPlane
 Capacity, OpenState     Content
       └──────────────────────┴────────────┴────────────┘
              one store: entities, codecs, key layout
                              │  Update, View, Guard, Now
                              ▼
          KV: embedded on one node, or replicated in a cluster

 key (first part = kind)       holds                     written by
 F‖profiles‖f7                 File: size, times, …      flush
 F‖profiles‖f7‖ov‖4GiB         overwrite record          flush
 F‖profiles‖f7‖ref‖…           ref to chunk C            offload
 C‖ns‖C, B‖ns‖K1, CR‖ns‖C‖…    chunk, block, ref index   offload
 F‖profiles‖d9                 File: new directory       mkdir
 F‖profiles‖d1‖e‖downloads     Entry, value "Downloads"  mkdir
 F‖profiles‖d1‖t‖…             time delta for d1         mkdir
 S‖profiles‖ud‖…               usage delta               mkdir
 U‖…, SH‖A, CFG‖…              users, shards, settings   control plane
 (kinds and sub-kinds shown by label, shares and files by name; a key
  holds one-byte codes and 16-byte IDs, §4.2.1)
```

Everything about one file sits under `F‖` with the share's ID and the file's
ID, so a client operation stays inside one or two such prefixes and deleting a
share is one range drop. Per-share records sit under `S‖`; content records
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
  RFC 19, RFC 18 and RFC 23 (planned) own those; this RFC stores their entities.

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
| `Client`, `Open`, `Lock`, `CachingGrant`, `Watch`, `Layout`, `Copy` | open state | [RFC 14](rfc-14-open-state.md) |
| `ChunkRef`, `Chunk`, `Block`, `ChunkHash`, `BlockName`, `JournalVersion`, `SnapshotCut` | the content map, the blocks it lives in, and the cut numbering snapshots | [RFC 6](rfc-6-block-metadata.md) |
| `FileID`, `ShareID`, `Principal`, `PrincipalID` | identity | §2.2 |
| `User`, `Group`, `Membership` | principals with names | §2.3 |
| `Share`, `ShareGrant`, `Snapshot` | a share, who may reach it, its snapshots | §2.3 |
| `Node`, `Shard`, `SlotTable` | cluster membership and shard placement ([RFC 11](rfc-11-ownership.md)) | §2.3 |
| `Setting`, `Secret` | configuration values and sealed credentials ([RFC 13](rfc-13-configuration.md)) | §2.3 |

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

// Principal is a protocol-neutral identity: a user or a group. Its ID is
// minted when the principal is created and never reissued, so a UID, GID or
// SID reused by a later account cannot inherit an old principal's files,
// ACE grants or charge. Protocol spellings are held only on the User or Group
// record (IDs) and in the PX‖ index that maps each back to its principal
// (§4.2); no file, ACL or usage record holds one, and this layer compares IDs
// and never interprets a spelling.
type Principal struct {
	Kind PrincipalKind // User, Group, Special (owner, group, everyone: fixed well-known IDs)
	ID   PrincipalID
}
```

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
| Server-wide | Installation, User, Group, Membership, Netgroup, Node, NodeLease, Shard, Setting, ShareList | no — an export carries the principals its files and grants reference (§5, decision 8), and every netgroup its policy references, with its members, so an import can refuse one that collides ([RFC 26 §3.2](rfc-26-catalog-backups.md#3.2%20Import%20is%20staged%20and%20published%20atomically)) |

```go
// User and Group are principals with a name. A file refers to them only by
// Principal (§2.2), never by name, so renaming a user touches no file and
// deleting one leaves its principal in ACLs as an unknown principal, which is
// how both protocols already report it.
type User struct {
	Principal Principal
	Name      string
	IDs       ProtocolIDs   // UID, SID, Kerberos principal: the mapping RFC 18 owns; each also a PX‖ row
	Primary   Principal     // the group a new file gets (RFC 7 §2.4, RFC 18); zero: none, and a new file's group is then its creator's own principal
	Disabled  bool
	Secret    SecretRef     // by reference, never the credential itself (RFC 13)
}

type Group struct {
	Principal Principal
	Name      string
	IDs       ProtocolIDs   // GID, SID; each also a PX‖ row
}

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
	// State is its own record (S‖id‖st), not part of the one PutShare writes:
	// a cut and a deletion change it as well as the management API.
	State  ShareState // enabled, quiesced, frozen for a cut, being removed, retired (§2.3.1)
	// Namespaces lists the block namespaces the share's refs are counted in,
	// each a (generation, NamespaceID) pair: one outside a re-home, two during
	// one. Write is the generation new put attempts carve under, the write
	// namespace (RFC 27 §2.7). Both are their own record (S‖id‖nsg), written
	// only by a re-home and an import, never by PutShare.
	Namespaces []NamespaceGen
	Write      uint64
}

// ShareGrant is one principal's access to one share. Files.Authorize
// evaluates it on every call, before any file ACL, and the grant is part of
// the authorisation cache key (RFC 7 §7), so a revoked grant stops a client
// that still holds handles; mount and tree connect only fail early on it.
type ShareGrant struct {
	Share     ShareID
	Principal Principal
	Access    ShareAccess // none, read, read-write, admin
	Version   uint64      // raised by every change; the authorisation cache key carries it
}

// ExportPolicy is the share's admission and identity-mapping rule, one per
// share. The filesystem service applies it on every call (RFC 17 §4.9); the
// file checks behind Files.Authorize never see it (RFC 7 §7.4).
type ExportPolicy struct {
	Share       ShareID
	Flavors     []AuthFlavor  // admitted, in preference order: SECINFO's answer
	MinKerberos KerberosLevel // krb5, krb5i, krb5p; zero when Kerberos is not required
	Clients     []ClientRule  // first match decides; no match refuses. Empty admits every client
	Squash      Squash        // none, root, all
	Anonymous   Principal     // what a squashed caller becomes
	SMB         SMBShareFlags // encrypt, hidden from enumeration, continuously available
	Version     uint64        // raised by every change; the authorisation cache key carries it
}

// ClientRule admits or refuses clients by address range or netgroup, and may
// narrow the share grant for them to read-only.
type ClientRule struct {
	Match  ClientMatch // CIDR or netgroup name
	Access ShareAccess // none, read, read-write: never wider than the grant
}

// Netgroup is a named set of hosts, server-wide, referenced by name from
// client rules. A rule naming a netgroup that does not exist matches nothing.
type Netgroup struct {
	Name    string
	Members []string // host names, addresses or CIDRs
	Version uint64
}

// Installation is the installation's identity: one record. ID is minted at
// random by the explicit initialisation step, which writes it here and into
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
	Generation []byte // the platform's machine-generation identifier last seen; empty where the platform offers none
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
	Roles   Roles  // protocol, storage; both by default (RFC 15)
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
	Incarnation  uint64  // shard incarnation: raised each time a node or journal begins serving the shard as primary; feeds the write verifier and names the grace instance (RFC 11 §3.1)
	Replicas     []Replica
	Count        int // configured size of the replica set, primary included
	Floor        int // fewest of primary and replicas, learners included, that acknowledge a write
}

// Replica is one replica of a shard (RFC 10 §2.1). Node, Journal and
// JoinIncarnation together identify it, so a node that lost its journal, or a
// reused node ID, is not taken for the replica it replaced.
type Replica struct {
	Node            NodeID
	Journal         JournalID // from the journal's segment header (RFC 1)
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
// (RFC 13 §7), so a node holds only its own roles' keys.
type Secret struct {
	ID    SecretRef
	Kind  SecretKind // password (slow hash), nt-hash (only with NTLM enabled), keytab, remote credential, key, node credential (RFC 15 §4.4)
	Version uint64   // raised by every rotation; the previous version stays resolvable until every node reports this one (RFC 13 §7)
	KeyID string     // which wrapping key sealed it: one per role or secret kind
	Value []byte     // sealed
}
```

A protocol node needs authentication secrets and a storage node needs remote-tier
credentials and key material; neither needs the other's. With one wrapping key,
a compromised protocol node could unseal the bucket credential. The KV still
lets any node read the sealed bytes; confidentiality rests on the key a node's
bootstrap names, and restricting what a node may *write* is [RFC 15](rfc-15-topology.md)'s.

Adapter settings and identity-provider configuration follow the same pattern
and are left out of this list; neither changes a file or content key.

#### 2.3.1 Share names, paths and state

- **Names** are compared case-insensitively and are unique under that fold,
  because SMB clients compare them so. A name is at most 80 characters, has no
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
  needs the new name or path. A durable or persistent SMB open reclaimed after
  a rename is reclaimed through a tree connect to the new name.
- **State** gates every call, evaluated with the grant (RFC 17 §4.9). `enabled`
  and `frozen for a cut` admit: a cut's own gate holds writes briefly and never
  reads ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)), so the share state adds nothing. `quiesced`, an
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

// Ranges: the overlap arithmetic every lock, removal and read uses.
func (r ByteRange) End() int64
func (r ByteRange) Overlaps(o ByteRange) bool   // and Contains, Intersect

// Usage and quota.
func (u Usage) Add(d Usage) Usage
func (u Usage) Exceeds(q Quota) bool

func (s Snapshot) Expired(now time.Time) bool
```

Entity methods defined elsewhere:

| Entity | Methods | Where |
| --- | --- | --- |
| `File`, `FileType` | `IsDir`, `IsRegular`, `IsSymlink`, `IsStream`, `IsSpecial`, `IsRoot`, `HasContent`, `HasFlag`, `FSMode`, `Validate`, `Apply` | [RFC 7](rfc-7-namespace-metadata.md) |
| `Entry` | implements `io/fs.DirEntry`: `Name`, `IsDir`, `Type` | [RFC 7](rfc-7-namespace-metadata.md) |
| `ACL` | `ACLFromMode`, `Mode`, `WithMode` (chmod's merge), `Clone` | [RFC 7](rfc-7-namespace-metadata.md) |
| `ChunkRef`, `Block` | `End`, `IsLive`, `VisibleAt(SnapshotCut)`; `Retirable` | [RFC 6](rfc-6-block-metadata.md) |
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
| `OpenState` | opens, locks, caching grants, watches | [RFC 14](rfc-14-open-state.md) |
| `Principals`, `ControlPlane`, `Inspect` | identity lookups, the management API, trusted reads by ID | this RFC |

```go
// Principals is what authentication and the chokepoint read (RFC 18). Groups
// resolves transitive membership; its result is cached only under a key that
// includes everything it read (RFC 7 §7.5).
type Principals interface {
	ByName(ctx context.Context, name string) (Principal, error)
	ByProtocolID(ctx context.Context, id ProtocolID) (Principal, error)
	Groups(ctx context.Context, p Principal) iter.Seq2[Principal, error]
	Grant(ctx context.Context, share ShareID, p Principal) (ShareAccess, error)
}

// ControlPlane is the management API's view (RFC 23). Every write names what
// it changes; a user rename writes the user record and its name index, and
// nothing under any F‖ prefix.
type ControlPlane interface {
	Users(ctx context.Context, after string) iter.Seq2[User, error]
	PutUser(ctx context.Context, u User) error
	DeleteUser(ctx context.Context, p Principal) error
	Groups(ctx context.Context, after string) iter.Seq2[Group, error]
	PutGroup(ctx context.Context, g Group) error
	AddMember(ctx context.Context, m Membership) error
	RemoveMember(ctx context.Context, m Membership) error
	Shares(ctx context.Context) iter.Seq2[Share, error]
	PutShare(ctx context.Context, s Share) error // Name, Path, Root, Config only
	SetShareState(ctx context.Context, share ShareID, from, to ShareState) error
	SetGrant(ctx context.Context, g ShareGrant) error
	Snapshots(ctx context.Context, share ShareID) iter.Seq2[Snapshot, error]
	Settings(ctx context.Context, scope SettingScope) iter.Seq2[Setting, error]
	PutSetting(ctx context.Context, s Setting) error
}
```

What each consumer holds:

| Consumer | Holds |
| --- | --- |
| NFS and SMB adapters | `vfs.Service` only ([RFC 17](rfc-17-vfs.md)) |
| Filesystem service ([RFC 17](rfc-17-vfs.md)) | `Namespace`, `Files`, `Capacity`, `OpenState`, and the engine's content facade |
| Engine | `Existence`, `Content` |
| GC | `Blocks` |
| Authentication (session setup, Kerberos contexts) | `Principals`; tree connect and mount go through the filesystem service's `Shares` calls ([RFC 17 §4.9](rfc-17-vfs.md#4.9%20Shares%2C%20mounts%20and%20trees)) |
| Management API | `ControlPlane` |
| Shard placement (RFC 11) | `Node` and `Shard` records through its own view, fenced by epoch |
| Debug tooling | `Dump` (§4.5) |

**The metadata store is a leaf.** It never calls the engine, and no view here
holds an engine interface. There is one read path for a file's attributes: the
filesystem service's `GetAttr` joins `Files.Get` with the engine's overlay of
`Size`, `Times` and `Version` for bytes not yet committed ([RFC 8](rfc-8-engine.md)); nothing
else answers size or times, and `Existence` offers no read of them.

`User` and `Share` here are entities, not wire types: `PutUser` receives a
`User`, and the only field it writes that another entity reads is the name
index. That is the one place in the model where a whole-entity put is
allowed, and only over records whose one writer is the management API, with no
background path. `PutShare` therefore writes `S‖id‖info` alone — name, path,
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
st, err := metadata.Open(ctx, kv, metadata.Options{...}) // reads the format record first (§4.6)

var (
	_ Namespace    = st.Namespace()
	_ Files        = st.Files()
	_ Capacity     = st.Capacity()
	_ Existence    = st.Existence()
	_ Content      = st.Content()
	_ Blocks       = st.Blocks()
	_ Principals   = st.Principals()
	_ ControlPlane = st.ControlPlane()
	_ Inspect      = st.Inspect()
)
```

Inside, the store is not one giant type. Each area — namespace, files, content,
blocks, principals, control plane, open state — is its own small struct over one
shared core that holds the `KV`, the codecs and the key layout. What each area
writes, it writes through **transaction-level helpers** (`unlinkTx(tx, …)`,
`chargeUsageTx(tx, …)`, `releaseTx(tx, …)`) that take the caller's `Txn`. That is
how an operation spanning areas stays one transaction: `Unlink` removes the
entry, decrements `Nlink`, moves usage and writes the pending release in one
`Update`, by calling four helpers, never four interface methods that would each
open a transaction of their own.

The composition root ([RFC 15](rfc-15-topology.md)) calls `Open` and hands each consumer only the
view it declared ([RFC 0 §1.2](rfc-0-data-lifecycle.md#1.2%20Component%20autonomy)). In a split deployment ([RFC 15](rfc-15-topology.md)) a view may instead be
a network client satisfying the same interface; the consumer cannot tell, which
is why every method must be safe to call remotely ([RFC 15](rfc-15-topology.md)).

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
	// Versions returns each key's change sequence, in one batched read
	// (§4.1, "Every committed write carries a change sequence").
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
A lease's expiry is compared with `Now` only where a lease is decided — a renewal,
an acquisition, a takeover's claim — never to fence a commit. **Every fenced
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
bound σ ([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)), and a time one node stored is compared with
another node's `Now` allowing σ, as every cross-host wait already does.

**Every committed write carries a change sequence.** Its source is the version
the store assigns at commit — an oracle's commit timestamp, or the store's own
commit version — so it orders writes like their commits, and no transaction
writes a counter to get it. `Versions` returns it for a batch of keys in one
read, and a backend **MUST** answer it in one of two ways, or both: keep the
version with the key and read it back, or have the store write the commit's
version into the stored value at commit — filled by the store, never by the
transaction, and stripped by the backend before `Get` or `Scan` returns the
value. A scan that returns a version per key is not required. A move's delta is
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
`Scan` is not tracked: a range read detects no phantom, and every invariant over
a range — "the count is zero, so delete", "the directory is empty, so remove" —
**MUST** follow RFC 0 §9.2: each side tracks, by `Get`, `Guard` or `GuardRange`,
a key the other writes. A backend that offers less is unsupported; the KV
conformance suite (§6.1) is the test.

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
| **Per file** — all under `F‖ShareID‖FileID`, written `F‖id` below | `F‖id` | File: every field, FileData's included, one value |
| | `F‖id‖acl` | ACL |
| | `F‖id‖x‖name` | Xattr |
| | `F‖id‖s‖streamID` | named stream link |
| | `F‖id‖e‖digest‖key` | Entry, under its **parent** directory. `key` is the name folded by the share's fold rule (§4.6), the identity on a case-sensitive share; `digest` is the 63-bit keyed digest of `key` that orders a listing and is its resume cookie ([RFC 7 §3.5](rfc-7-namespace-metadata.md#3.5%20A%20cookie%20survives%20concurrent%20mutation)); the fold is [RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case)'s; the name's original bytes are in the value |
| | `F‖id‖t‖unique` | directory time delta, carrying the cut it was written after (§4.4) |
| | `F‖id‖h‖start`, `F‖id‖rm‖version` | Hole; Removal, with its range, kind, the cut it was written after, and its pruning cursor, and for a clone its **clone spec** — source FileID, source offset, destination offset, length and the journal position `asOf` the source was read at — from which a clone resumes after a crash, its source range registration rebuilt ([RFC 6 §6.6](rfc-6-block-metadata.md#6.6%20Clone%20and%20server-side%20copy)) ([RFC 6 §2.4](rfc-6-block-metadata.md#2.4%20FileData%20and%20holes), [§6.2](rfc-6-block-metadata.md#6.2%20Truncation%20and%20deallocation)) |
| | `F‖id‖ov‖start` | Overwrite record: an extent of content whose existence was committed and which a later committed write replaced, with the newest such write's version; an offset in it whose covering ref's `newest` is below that version reads as uncarved. Written only by an existence commit, never for an append or a first write into a hole ([RFC 6 §2.4](rfc-6-block-metadata.md#2.4%20FileData%20and%20holes), [§3.3](rfc-6-block-metadata.md#3.3%20Holes%2C%20not%20written%20extents)) |
| | `F‖id‖ref‖offset` | ChunkRef, live; its value carries `nsgen`, the generation of the share's namespace its chunk is counted in ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |
| | `F‖id‖H‖died‖suffix` | **history** of a versioned per-file record: the value it had, under its live key's suffix — `ref‖offset` (ChunkRef), empty (File), `acl`, `x‖name`, `s‖streamID`, `e‖digest‖key`, `h‖start`, `ov‖start` ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)) |
| | `F‖id‖fx`, `F‖id‖fo` | fences: the (shard, epoch) the file's commits must carry ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit), [RFC 11 §8](rfc-11-ownership.md#8.%20Metadata%20consistency)); every offload commit writes `fo`. Per-file and range shards, and their records, are deferred ([RFC 11 Appendix C](rfc-11-ownership.md#Appendix%20C%20%E2%80%94%20later%3A%20per-file%20and%20range%20shards)) |
| | `F‖id‖rel` | pending release: written by the final unlink, deleted only by the release transaction; holds no holder list, the holders are the `op‖` records ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)) |
| | `F‖id‖op‖openID` | durable Open (only the cases RFC 14 makes durable: keeps an unlinked file alive, SMB persistent handle); a persistent open's value holds every field, its principal, delete-on-close principal, create GUID, app instance, timeout and lock sequences included, and the lease covering it with its key, parent key, kind and epoch ([RFC 14 §8.1](rfc-14-open-state.md#8.1%20SMB%20durable%20and%20persistent%20opens)) |
| | `F‖id‖op‖openID‖l‖start‖lockID` | durable Lock, only under a persistent open, so closing the open drops one prefix; `lockID` is minted per lock, so two locks of one open at one offset — two lock owners, or stacked shared locks — are two keys, and a scan still returns them in offset order ([RFC 14](rfc-14-open-state.md)) |
| | `F‖id‖dp` | delete pending, with the entry it will remove and the principal that marked it, as which the unlink runs; written only while the file has a persistent open, deleted by that entry's unlink or by a clear ([RFC 14 §9.4](rfc-14-open-state.md#9.4%20Delete%20on%20close)) |
| **Per share** — under `S‖ShareID` | `S‖id‖info` | Share: name, path, root and configuration, the fields `PutShare` writes (§3) |
| | `S‖id‖st` | the share's `State`; changed only by a conflict-tracked read-modify-write naming the state it expects (§3) |
| | `S‖id‖nsg` | the share's `Namespaces` and `Write` generation; written only by a re-home and an import, by a conflict-tracked read-modify-write (§3, [RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |
| | `S‖id‖g‖principal` | ShareGrant; its reverse key `GR‖principal‖ShareID` is written and deleted with it |
| | `S‖id‖xp` | ExportPolicy |
| | `S‖id‖snap‖cut` | Snapshot, with its ordinal; nothing else lives under this prefix, so listing snapshots reads only snapshots |
| | `S‖id‖cut`, `S‖id‖live‖k` | Cut: `k`, `klatest`, the cut time of the share's latest cut, and `deleting`, the cut a running deletion removes; LiveCut, one per live snapshot, with its kind, share or subtree, its snapshot's ordinal, and a subtree cut's covered set of shards ([RFC 12 §2.2](rfc-12-snapshots.md#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree), [RFC 12 §2.8](rfc-12-snapshots.md#2.8%20Deleting), [RFC 12 §2.10](rfc-12-snapshots.md#2.10%20Subtree%20snapshots)) |
| | `S‖id‖sc‖shard` | SubCut: `klatest`, the newest live subtree cut covering that shard, and the cut time of the latest subtree cut covering it, which a later cut's time must exceed by one second ([RFC 12 §2.3](rfc-12-snapshots.md#2.3%20The%20cut%20is%20one%20transaction%20behind%20a%20brief%20gate)); raised only by a cut, behind the shard's gate, lowered only by a deletion ([RFC 12 §2.10](rfc-12-snapshots.md#2.10%20Subtree%20snapshots)) |
| | `S‖id‖hd‖shard‖died‖FileID‖suffix` | died index: one empty key per history record of the share, ref or namespace, keyed first by the shard the version was superseded in and then by `died`, `suffix` being the history key's, written and deleted with it; a snapshot deletion walks it from its cut in each shard the cut covers, and in no other ([RFC 12 §2.2](rfc-12-snapshots.md#2.2%20A%20snapshot%20is%20counted%20content%20and%20a%20frozen%20tree), [RFC 12 §2.8](rfc-12-snapshots.md#2.8%20Deleting)) |
| | `S‖id‖hr‖cut‖shard` | hold record: the shard's journals still hold content of that cut not yet offloaded; written by the cut, and for the receiving shard by a move's commit that ships held versions ([RFC 12 §2.4](rfc-12-snapshots.md#2.4%20A%20snapshot%20hold%20bridges%20dirty%20content%20to%20history)) |
| | `S‖id‖use‖cut‖useID` | use record: its kind — `clone`, `restore`, `backup`, `copy` or `move` — reading that snapshot, and the deadline its reader renews, which a `move` record has none of; while one exists the snapshot cannot be deleted ([RFC 26 §2.2](rfc-26-catalog-backups.md#2.2%20A%20backup%20holds%20its%20snapshot)) |
| | `S‖id‖rh` | re-home record: the new namespace, the cursor and the pass number of a running re-home; its existence refuses new clones, new catalog backups and moves of either namespace with `ErrRehoming`, while snapshots, copying backups and reads of existing snapshots go on ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |
| | `S‖id‖or‖shard` | `old_refs`, per shard: the share's refs still at the old generation during a re-home, folded like usage: a transaction that writes or drops such a ref writes the change in its usage delta (`S‖id‖ud‖…`), never to this key (§4.4) |
| | `S‖id‖ut‖shard`, `S‖id‖pu‖principal‖shard`, `S‖id‖pj‖project‖shard` | folded usage, **per shard**: share, with its `history_bytes`, principal, project; each written only by its shard's fold, and a reader sums a prefix scan over the shards (§4.4) |
| | `S‖id‖ud‖shard‖unique` | usage delta, not yet folded, per shard (§4.4) |
| | `S‖id‖qt‖principal-or-project` | Quota: `Hard`, `Soft`, `Grace`, `Advisory` ([RFC 7](rfc-7-namespace-metadata.md)) |
| | `S‖id‖qx‖principal-or-project` | when usage first exceeded the soft limit; deleted when it falls back under ([RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota)) |
| | `S‖id‖vf‖bucket‖version‖FileID`, `S‖id‖vf‖bucket‖version‖FileID‖unique` | version-floor index: **one entry per File, at its stored `Version`**, written by every transaction that raises that `Version` — an existence commit, an attribute, flag, ACL or xattr change, a link change — which deletes the entry it supersedes; and one entry per unfolded directory-time delta at the delta's version, under its own unique suffix, written blind with the delta and replaced by the directory's own entry when the fold absorbs it. No ref has an entry: a File's `Version` is at least every version its refs and `Applied` carry. `bucket` is the file's identity hashed into one of *B* fixed buckets, so a share's commits spread over *B* key ranges ([RFC 6 §8.3](rfc-6-block-metadata.md#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor), [RFC 7 §9.4](rfc-7-namespace-metadata.md#9.4%20The%20change%20attribute%20and%20ctime%20never%20move%20backward)) |
| | `S‖id‖fnc` | numeric file-id allocator: the next unreserved number, reserved in ranges by shard primaries, so the protocol's numeric id is injective ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)); the number itself is a File field |
| **Per namespace** — content-addressed, one partition per remote key namespace ([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)) | `C‖ns‖hash`, `B‖ns‖name` | Chunk, Block (with its GC state: `live`, `retired`, `deleted`, and its carried chunk list) |
| | `CR‖ns‖hash‖ShareID‖FileID‖offset‖died` | reverse ref index: one empty-valued key per live (`died` zero) or history ref, written in the ref's transaction; authoritative for "which refs name this chunk", and the refcount is its cache ([RFC 6 §6.1](rfc-6-block-metadata.md#6.1%20A%20refcount%20is%20exactly%20its%20refs)) |
| | `I‖ns‖name` | put intent: domain, domain ID, epoch, and the writer's node epoch for a shard ([RFC 9 §7.2](rfc-9-gc.md#7.2%20Every%20record%20GC%20stores%20names%20its%20reclamation)) |
| | `BR‖ns‖not_before‖name`, `BD‖ns‖name`, `BC‖ns‖bucket‖name` | GC index: retired blocks by `not_before`, deleted blocks awaiting prune (value: when the delete succeeded), compaction candidates by dead-ratio bucket. Derived from block records; repairable and rebuildable ([RFC 9 §7.4](rfc-9-gc.md#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt)) |
| | `NS‖ns‖gc‖lease‖shard`, `NS‖ns‖gc‖recheck`, `NS‖ns‖gc‖hold`, `NS‖ns‖gc‖suspect‖hash` | GC lease per prefix shard with its epoch, last `Recheck` result, the deleter's hold, audit lowering state ([RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace%2C%20partitioned%20by%20prefix)) |
| | `NS‖ns‖gc‖forward` | GC forward-walk marker: the numbers of the last forward pass started and the last completed ([RFC 9 §6.2](rfc-9-gc.md#6.2%20Corrections)) |
| | `NS‖ns‖claim` | claim nonces: the instance nonce this installation intends to write into the namespace's claim, recorded durably before the claim put, and the last one it wrote; a `Recheck` reads the claim and compares it with these before any rewrite. Every claim put on any path — start, `Recheck`, import, recovery, re-home, restore — records its intended nonce here first. **Installation-local**: never exported and never imported, so a moved namespace's importer starts with its own nonces and does not fence itself on finding the claim it just took ([RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period), [RFC 27 §2.1](rfc-27-namespace-migration.md#2.1%20One%20installation%20per%20namespace%2C%20proven%20by%20a%20claim)) |
| | `NS‖ns‖gc‖pause` | GC pause record: while it exists the namespace gets no relocation, delete or collection; GC reads it before every pass and batch, and every relocation commit guards it ([RFC 27 §2.2](rfc-27-namespace-migration.md#2.2%20The%20move%2C%20step%20by%20step), [RFC 9 §7.5](rfc-9-gc.md#7.5%20Service%20settings%20are%20rechecked%20on%20their%20own%20period)) |
| | `NS‖ns‖gc‖cursor‖walk‖shard` | walk cursor per kind of walk (audit, block walk, index rebuild) and shard, so a restarted walk resumes; derived |
| | `NS‖ns‖pk‖ShareID‖FileID‖offset‖died` | parked ref: a ref a re-home dropped from namespace `ns` while an unexpired catalog backup taken before the re-home finished still needs it, filed under `ns`'s prefix and owned by `ns`'s installation, so moving the re-homed share neither carries nor drops it; it keeps its chunk count and reverse key in `ns`, is seen by no read, snapshot or listing, only by `ns`'s GC, and is dropped when the last such backup expires ([RFC 27 §2.7](rfc-27-namespace-migration.md#2.7%20Moving%20one%20share%20out%20of%20a%20shared%20namespace)) |
| | `NS‖ns‖key‖kind‖keyID` | namespace key record: kind (`chunk-id-key`, `chunking-key`, `header-key`, `export-key`, `data-key`), ID, fingerprint, the ID of the master key that wraps it, the wrapped bytes, and state — `current`, `retired` or `destroyed`; a destroyed key keeps only ID, fingerprint and state. Every key is wrapped but one: a non-encrypting namespace's chunk-ID key, held in the clear, with no master key ID. Each record reaches the namespace's `keys` control object and every backup location its policies write to before it becomes current. Not a secret: a wrapped key is useless without the master key, which is never here, and the one clear key hides nothing its plaintext blocks do not already show ([RFC 5 Appendix B.2](rfc-5-transforms.md#B.2%20Keys)) |
| | `NS‖ns‖bk‖location` | folder record: the copy or sweep holding the namespace's block folder at a backup location, its deadline, the **lease number** raised at every take, which every sweep commit reads, and the folder's census of (material ID, fingerprint) ([RFC 26 §2.4.1](rfc-26-catalog-backups.md#2.4.1%20Layout%20at%20the%20location)) |
| **Server-wide** | `U‖principal`, `G‖principal` | User, Group, keyed by `PrincipalID` |
| | `M‖group‖member`, `MR‖member‖group` | Membership, both directions |
| | `GR‖principal‖ShareID` | grant reverse index: one empty key per `ShareGrant`, so deleting a principal finds its grants without a scan of every share; server-wide, so an export carries no row of it and an import writes one per grant it publishes |
| | `NX‖kind‖name` | name index: user, group and share names → ID, unique; a share name is keyed by its case fold (§2.3.1) |
| | `NP‖path` | share path index: path → `ShareID`, unique; the pseudo-filesystem is built by listing it (RFC 17 §4.9) |
| | `NG‖name` | Netgroup |
| | `SL` | ShareList: one version per installation |
| | `IN` | Installation: the installation's identity, written once by the explicit initialisation step, which writes the same ID into every journal's `format` file ([RFC 1 §3](rfc-1-journal.md#3.%20Interface)); its instance part and recorded machine-generation identifier; and the active version of every shared format but the store format, which `\x00format` holds, each raised only by the control plane's gate (§2.3, §4.6) |
| | `NSM` | the NSM state number, one per installation; raised durably before the grace of any failover that lost NLM locks ([RFC 14 §4.5](rfc-14-open-state.md#4.5%20NLM%20locks%20and%20restart%20notification)) |
| | `PX‖scheme‖id` | protocol-ID index → `PrincipalID`, unique per scheme; `scheme` is one of `uid`, `gid`, `sid` (binary form) and `krb`, a Kerberos principal as `name@REALM`, compared byte for byte as the KDC spells it; with the User or Group record, the only place a protocol spelling is stored (§2.2) |
| | `N‖node`, `N‖node‖exp`, `N‖node‖clk` | Node: its roles, binary version, the version range it reads and writes for every shared format, failure domain, node epoch and whether a takeover marked it lapsed, guarded by every fenced commit; NodeLease (cluster): the lease's expiry, which a renewal writes with its journals' generations and nothing else, and a resume rewrites with the node record ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)). On a single node the node record's epoch rises at every start and no lease is written ([RFC 0 §1.4](rfc-0-data-lifecycle.md#1.4%20The%20single-node%20profile)). Clock ceiling, only on a backend without a timestamp oracle: the bound `Now` stays under, raised once a second (§4.1); its own key, so raising it conflicts with no fenced commit |
| | `SH‖shard` | Shard, with its primary as (node, node epoch, journal identity, join incarnation), its epoch, its shard incarnation and its replicas ([RFC 11 §3.1](rfc-11-ownership.md#3.1%20The%20primary%20is%20fenced%20by%20an%20epoch)) |
| | `SH‖shard‖rq‖requestID` | (cluster) request record: a namespace mutation's request ID and result, written in its transaction and expiring after the retry window, so a retry after a takeover is answered, not applied again ([RFC 11 §5.1](rfc-11-ownership.md#5.1%20Front-ends%20forward%20to%20the%20primary)) |
| | `SH‖shard‖xh‖opID` | (cluster) cross-shard hold record: a participant's hold for one prepared operation, with its deadline; every commit of the operation guards it, and a release deletes it before the participant grants what it refused ([RFC 11 §8.1](rfc-11-ownership.md#8.1%20Operations%20across%20shards)) |
| | `SH‖shard‖hw` | (cluster) each replica's acknowledged committed-point mark, persisted by the primary on a period; never lowered, raises no epoch ([RFC 10 §7.2](rfc-10-journal-replication.md#7.2%20Removal)) |
| | `J‖journal` | a journal, keyed by journal identity: the node and device it lives on, written when the journal is created and before anything in it is acknowledged, so a start that finds no journal or another one there refuses its shares ([RFC 8 §2.5](rfc-8-engine.md#2.5%20Start%20in%20order%2C%20stop%20in%20reverse%2C%20and%20join%20before%20closing)); and, from the replication extension on, its generation, raised at every open, lease acquisition and lease renewal, and by a primary that finds itself rolled back ([RFC 10 §2.2](rfc-10-journal-replication.md#2.2%20One%20journal%20carries%20many%20shards)) |
| | `RS‖partition` | (cluster) repair scheduler lease per partition of the shard-ID hash, with its epoch ([RFC 10 §7.4](rfc-10-journal-replication.md#7.4%20Repair)) |
| | `MV‖giving‖receiving` | move record: the cursor of a move of files from the giving shard to the receiving one, one per pair, so a crashed move resumes and several moves out of one shard run at once ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) |
| | `SLOT` | (cluster) slot table: the fixed slot count and each slot's ordered nodes, primary first, in distinct failure domains, weighted by node capacity; one per installation, changed only by compare-and-swap ([RFC 11 §2.2](rfc-11-ownership.md#2.2%20Automatic%20per-child%20shards)) |
| | `CFG‖scope‖key` | Setting. A backup location's record ([RFC 13 §2.5](rfc-13-configuration.md#2.5%20A%20backup%20location%20is%20its%20own%20record)) is one, and carries two system fields the control plane writes when it creates the record: the put-integrity outcome, and the **location health object's identity** — the version of `<location>control/health` at an immutable location, the nonce written in it at a mutable one ([RFC 4 §4.7](rfc-4-remote-tier.md#4.7%20Health%20is%20one%20probe%20call)) |
| | `CFGGEN` | settings generation: one installation-wide counter, raised in the transaction of every `Setting` or `Secret` change; every node point-reads it each second and re-reads the records when it moved ([RFC 13 §5](rfc-13-configuration.md#5.%20Binding%20classes)) |
| | `SEC‖id‖version` | Secret (§2.3), one key per version, the previous one kept only until every node reports the current one: envelope-encrypted, never dumped or exported |
| | `CL‖clientID`, `CL‖clientID‖sh‖shard` | durable Client record. Its fields are **exactly** these, and the store **MUST** write no other: protocol, owner, boot verifier, principal (zero for an SMB record, which is keyed by `ClientGuid` alone and names a machine, not a user), state protection, the NFSv4.1 `CREATE_SESSION` sequence ID and its cached reply (`CSSeq`, `CSReply`: RFC 8881 §18.36.4 compares `csa_sequence` with them after any restart or failover), whether its state was revoked, and (cluster) lease owner, back-channel node, path-down flag and session holders ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client), [§8](rfc-14-open-state.md#8.%20What%20is%20durable)). `Expires` **MUST NOT** be written: it is volatile, held by the lease owner, so a renewal (`SEQUENCE`, `RENEW`) writes nothing; written, it would make 10⁵ clients cost 10⁵ writes per lease period. The record is rewritten only when one of its fields changes. One key per shard the client held state in (`Client.Shards`), checked on reclaim, whose value is the grace instance of the client's last `RECLAIM_COMPLETE` there and, for NFSv4.0, whether a loss of that shard's state is still to be signalled to the client ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)); for an NLM host also its notification address, so these records are the NSM monitor list ([RFC 14 §4.5](rfc-14-open-state.md#4.5%20NLM%20locks%20and%20restart%20notification)) |
| | `CLO‖protocol‖owner` | client owner index: the client's own name for itself → `ClientID`, unique, so `EXCHANGE_ID`, `SETCLIENTID` and an SMB negotiate find the record after any restart ([RFC 14 §2.1](rfc-14-open-state.md#2.1%20Client)) |
| **Store** | `\x00format` | store format record (§4.6) |

The table is **exhaustive**: every key the store writes has a row. A new
record kind is a format bump (§4.6) and gets its row in the same change;
volatile open state (locks, caching grants, watches, layouts, asynchronous copies, non-durable opens, delete pending without a persistent open) and
the primary's in-memory tables (quota reservations, routed-request dedup) are
never written to the KV and so have none. A file's shard and its primary's epoch
need no row of their own: the shard is a `File` field and the epoch is in the
fences.

Consequences:

- `Files.Get` costs what [RFC 7 §2.5](rfc-7-namespace-metadata.md#2.5%20Where%20%60size%60%20lives) states, the one statement of it: one
  read of the File's one key, plus, for a directory, a scan of its unfolded
  time deltas. The File was two keys (attributes, and
  the write path's fields) so that `chmod` and a write never conflicted; the
  second key cost 25% on `GETATTR`, 60% on `READDIRPLUS` and 30% on serial
  creates (Appendix A), to avoid a conflict that is rare and cheap to retry.

  > ponytail: one key per File, so `chmod` and an existence commit on one file
  > conflict and one retries. Split the write path's fields into their own key
  > when a profile shows those retries.
- Every per-file key carries the `ShareID`. Deleting or exporting a share is a
  prefix operation (a range drop or a range scan) instead of a tree walk; a share's files stay together for
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
- Two GC index kinds are **sequential, by design**: `BR‖ns‖not_before‖name`
  orders retired blocks by time, so every retirement inserts at the tail of the
  namespace's range, and `BC‖ns‖bucket‖name` puts every candidate of one
  dead-ratio bucket in one range. That is what lets GC read the due blocks and
  the best candidates with one ordered scan each. The cost is one hot range per
  namespace for each kind: on the replicated store its inserts land on one
  region, at the rate blocks retire or change bucket — a background rate,
  batched, never a client operation's. Every other content-addressed key leads
  with a hash or a nonce-unique name and spreads evenly.

  > ponytail: `BR‖` and `BC‖` insert at one point of their range. Lead each key
  > with the block name's first byte as a partition, and scan the partitions in
  > parallel, when a region split on those ranges shows in a GC profile.

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
  second table. The labels are also chosen so that none is a prefix of a
  sibling's: `ref`, `rel`, `rm`; `op`, `ov`; `ut`, `ud`, `use`; `qt`, `qx`.
- **Integer** — offset, `start`, version, cut, `born`, `died`, shard, generation,
  bucket, digest, epoch, sequence: unsigned, 8 bytes, big-endian, so refs scan
  in offset order and history in `died` order. A time (`not_before`) is store
  time as unsigned nanoseconds since the Unix epoch, encoded the same way.
- **Identifier** — FileID, ShareID, PrincipalID, NamespaceID, NodeID, JournalID,
  and the open, lock, use, operation, request, client and key IDs: raw bytes at
  its fixed length, 16 for every minted ID. `ChunkHash` and `BlockName` are raw
  at the fixed lengths [RFC 6](rfc-6-block-metadata.md#2.%20The%20records) gives them.
- **Byte string** — an entry's folded key, an xattr name, a user, group, share
  or netgroup name, a path, a setting key, a SID, a Kerberos principal, a
  client owner: each `0x00` byte written as `0x00 0xFF`, then the two-byte
  terminator `0x00 0x01`. Inside a string `0x00` is always followed by `0xFF`,
  so the terminator cannot occur there: no string's encoding is a prefix of
  another's, whatever component follows. A string sorts before every longer
  string it begins, since `0x00 0x01` sorts below `0x00 0xFF` and below any
  other byte that can follow.
- **Tagged** component — `principal-or-project`, a setting's `scope`: one tag
  byte, then the encoding the tag names. `unique` in a delta key is the
  writer's NodeID, node epoch and sequence, 32 bytes.
- **Suffix** — the history key `F‖id‖H‖died‖suffix` and the died index's
  trailing `suffix` hold the live key's own encoding after `F‖id`, empty for the
  File itself.

| Scope | Kind codes |
| --- | --- |
| Store | `\x00` 0x00, followed by the bytes `format`; nothing else is under 0x00 |
| Per file, per share | `F` 0x01, `S` 0x02 |
| Per namespace | `C` 0x10, `CR` 0x11, `B` 0x12, `BR` 0x13, `BD` 0x14, `BC` 0x15, `I` 0x16, `NS` 0x17 |
| Server-wide | `U` 0x20, `G` 0x21, `M` 0x22, `MR` 0x23, `GR` 0x24, `NX` 0x25, `NP` 0x26, `NG` 0x27, `SL` 0x28, `IN` 0x29, `NSM` 0x2A, `PX` 0x2B, `N` 0x2C, `SH` 0x2D, `J` 0x2E, `RS` 0x2F, `MV` 0x30, `SLOT` 0x31, `CFG` 0x32, `CFGGEN` 0x33, `SEC` 0x34, `CL` 0x35, `CLO` 0x36 |

| Under | Sub-kind codes |
| --- | --- |
| `F‖id` | none for the File itself; `acl` 0x01, `x` 0x02, `s` 0x03, `e` 0x04, `t` 0x05, `h` 0x06, `rm` 0x07, `ov` 0x08, `ref` 0x09, `H` 0x0A, `fx` 0x0B, `fo` 0x0C, `rel` 0x0D, `op` 0x0E, `dp` 0x0F; under `op‖openID`, `l` 0x01 |
| `S‖id` | `info` 0x01, `st` 0x02, `nsg` 0x03, `g` 0x04, `xp` 0x05, `snap` 0x06, `cut` 0x07, `live` 0x08, `sc` 0x09, `hd` 0x0A, `hr` 0x0B, `use` 0x0C, `rh` 0x0D, `or` 0x0E, `ut` 0x0F, `pu` 0x10, `pj` 0x11, `ud` 0x12, `qt` 0x13, `qx` 0x14, `vf` 0x15, `fnc` 0x16 |
| `NS‖ns` | `gc` 0x01, and under it `lease` 0x01, `recheck` 0x02, `hold` 0x03, `suspect` 0x04, `forward` 0x05, `pause` 0x06, `cursor` 0x07; `claim` 0x02, `pk` 0x03, `key` 0x04, `bk` 0x05 |
| `N‖node`, `SH‖shard`, `CL‖clientID` | `exp` 0x01, `clk` 0x02; `rq` 0x01, `xh` 0x02, `hw` 0x03; `sh` 0x01 |
| enumerated components | `NX` kind: user 0x01, group 0x02, share 0x03; `PX` scheme: `uid` 0x01, `gid` 0x02, `sid` 0x03, `krb` 0x04; `NS‖ns‖key` kind: `chunk-id-key` 0x01, `chunking-key` 0x02, `header-key` 0x03, `export-key` 0x04, `data-key` 0x05 |

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
primary ([RFC 11](rfc-11-ownership.md)) folds the deltas its shard wrote in batches, one transaction
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

> ponytail: a usage read scans one total per shard of the share, O(shards).
> Add a share-wide total folded from the per-shard ones on a slow period when a
> share with many shards shows that scan in a `statfs` profile.

**Charging is logical bytes per file.** A file is charged its logical size
excluding holes, in full, whether or not its content is shared with another
file by a clone or by deduplication; deduplication savings are the share's,
never a principal's. The charge is `File.Charged` ([RFC 7](rfc-7-namespace-metadata.md)): every existence
commit and removal that changes it writes the difference as a usage delta in
the same transaction, and `chown` moves the whole `Charged` from the old owner
and group to the new ones. So a charge is always read from one field, never
recomputed by scanning holes.

The deltas are in the same transaction as the change, so a crash loses
nothing and needs no rebuild.

**Quota is enforced by reservation.** Bytes are charged at the existence
commit, but a write is admitted long before, so a check against committed
totals alone lets every staged, uncommitted byte in the journal through. Each
primary therefore holds an in-memory **reservation** per principal and per
project: a write reserves the bytes it may add before it is acknowledged, the
check is totals + unfolded deltas + this primary's reservations against the
limit, and the reservation is released when the existence commit charging those
bytes lands. A crash drops the reservations with the uncommitted writes they
covered, and replay re-reserves what it replays.

> decision: quota fails open by a stated bound, not exactly. One primary sees
> only its own reservations, so where one principal writes through several
> shards concurrently the overshoot is at most the reservation slack each such
> primary may hold beyond the committed total, summed over those primaries; with one
> shard it is zero. Tighten it by leasing each primary a slice of the remaining
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
Installation record, when it creates the store. Opening a store **MUST** read it first and refuse a
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

## 5. Decisions and evidence

Each question was put to prior art and, where the embedded store could answer
it, to a benchmark (Appendix A). Sources are linked once per system.

| # | Question | Answer | Evidence |
| --- | --- | --- | --- |
| 1 | Drop `generation`? | **Yes**, with share-scoped keys (§4.2). A restore into a new share gets new keys and cannot alias an old handle; nothing rolls a share back in place ([RFC 12 §1.1](rfc-12-snapshots.md#1.1%20Non-goals)), so a released file's handle never resolves again. | JuiceFS and 3FS use never-reused IDs with no generation |
| 2 | `Mode` beside the ACL? | **Keep both.** `Mode` lives on the File so `GETATTR` never reads the ACL; `chmod` follows [RFC 8881 §6.4.1.1](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4.1.1) exactly ([RFC 7](rfc-7-namespace-metadata.md)); a file with no ACL gets one synthesised on read. Reject "last writer wins". | [RFC 8881 §6.4](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4) requires them to agree; OneFS "Balanced", [Qumulo XPP](https://docs.qumulo.com/administrator-guide/authorization-qumulo-core/managing-cross-protocol-permissions-xpp.html); ONTAP mixed style is the cautionary case |
| 3 | Stripe usage counters? | **No — delta records and a fold** (§4.4). Stripes spread collisions; deltas remove them. | Appendix A; [JuiceFS quota design](https://juicefs.com/en/blog/engineering/quota-design-in-distributed-architecture) batches deltas too |
| 4 | Open state durability? | **One rule per entity:** `Client` durable (what RFC 8881 §8.4.3 requires for reclaim, and the client's identity: owner, verifier, principal, state protection, the `CREATE_SESSION` sequence and reply), except its lease expiry, which is volatile so a renewal writes nothing (§4.2); `Open` volatile and reclaimed in grace, durable when it keeps an unlinked file alive or is an SMB persistent handle; `Lock` volatile unless its open is persistent; `CachingGrant` and `Watch` volatile, never reclaimed. Grace may run per shard. | knfsd `nfsdcld` and Ganesha `rados_cluster` store client records only; CephFS rebuilds caps on reconnect; SMB persistent handles are the one durable case |
| 5 | Stream as File? | **Yes for content, NTFS for identity:** a stream has no owner or ACL of its own, reports its base file's ID to SMB, and is released with it. | NTFS streams are attributes of one file record; ZFS named attributes and Samba `streams_depot` are files |
| 6 | Share in per-file keys? | **Yes**: `F‖ShareID‖FileID` (§4.2). | JuiceFS volume prefix; TiKV range deletion drops a share in seconds, not hours |
| 7 | Nested groups? | **Walk with a depth cap and cache.** Kerberos and AD already deliver the flattened list in the ticket, so no walk for them. Local groups are walked at login, depth ≤ 8 with cycle detection, cached under a key naming every membership it read (RFC 7 §7.5). No stored closure table. | Kerberos PAC; SSSD nesting level; Zanzibar's flattened index shows the closure's write cost |
| 8 | Deleted principals? | Deleting a user removes the User, its memberships and grants, the grants found through their reverse keys (`GR‖principal`, §4.2) without a scan of every share; the principal stays in ownership, ACLs and usage. An admin **reassign** job moves files and their charge through `chown`. A deleted principal's ID is never reissued, because IDs are minted, not derived from a UID or SID (§2.2); a new account reusing the UID gets a new principal. | ONTAP shows raw SIDs in quota reports; NTFS refuses to drop a quota entry while the SID owns files; MS-SMB2 returns zeroed entries for unknown SIDs |
| 9 | Where credentials live? | **In the KV, as `Secret` records** (§2.3), envelope-encrypted under a key held outside it (a file or a KMS), never dumped or exported. Password hashes use a slow hash; an NT hash exists only when NTLM is enabled. A keytab is a `Secret` of kind keytab, sealed under the `protocol` role's key like every authentication secret ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)), so every `protocol` node reads the one keytab from the store; a node whose Kerberos library needs a path writes it at start to a file only that node's process can read, and never back. | Samba `tdbsam`, TrueNAS and ONTAP all keep credentials in their replicated config store; Samba warns NT hashes are cleartext-equivalent |
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
| **KV conformance** | every `KV` implementation (embedded, replicated, and an in-memory fake used only by this suite's own tests) | `Update` is atomic and retried; **writer first**: G guards *k* and writes *j*, W writes *k*, both from snapshots taken before either commits; W commits, then G — assert G aborts; **guard first**: the same, G committing first — assert both commit, and that W is ordered after G (a suite asserting W aborts rejects every conforming backend, and no rule may rely on it); **two guards, either order**: T1 and T2 each guard *k* and write a key of their own; commit T1 then T2, then T2 then T1, and once with T1 committing between T2's snapshot and T2's `Guard` — assert all commit; repeat with `GuardRange` over a prefix holding *k*; **range guard**: T guards prefix *P* with no key under it, another transaction creates *P*‖*x* and commits, T commits — assert T aborts; a key created outside *P* does not abort it; **removal against create**, at the KV level: R scans *P* and finds it empty, `GuardRange(P)` and writes *d*; C guards *d* and writes *P*‖*x*; run both commit orders from snapshots taken before either commits — assert exactly one commits and never both, so no key is left under *P* beside a written *d*; drop R's `GuardRange` and the guard-first order commits both; **read-only**: no caller's transaction that calls `Guard` or `GuardRange` writes nothing — the counting wrapper of §6.3 asserts it over the entity suite;  **lost update**: two transactions each `Get` one key and `Set` it to the value read plus one, from one snapshot — exactly one commits first and the other retries, so the key ends two higher; **write skew**: two transactions each `Get` the key the other `Set`s, from one snapshot — one aborts; a blind `Set` of one key by two transactions conflicts, on a backend that checks only reads too; a `Scan` registers nothing, and a range invariant gated on a point record holds (§4.1); `Now` never runs backward across commits in order on one node, nor across a restart with the clock stepped back, and on an idle store it advances with real time; `Versions` returns, for one key written by commits in order, increasing sequences, and none a transaction wrote itself; `Scan` sees a transaction's own writes and nothing uncommitted; `Limits` is honest (a transaction at each limit commits, one past it is refused, a transaction whose writes fit but whose guards and reads take it past `TxnBytes` is refused, one held open past `Age` aborts); a returned `Update` survives a power loss of the store's node, and on the replicated store the loss of its leader (§4.1, §6.4) |
| **Entity conformance** | the store over each real `KV` | every rule in §1.2 and every invariant of RFC 6 §9 and RFC 7 §10, through the §3 interfaces only |

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

- every file's `Nlink` equals the entries naming it;
- every chunk's `Refcount` equals the live and history refs naming it, found by
  a full walk;
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
  fails with a named error, never a zero value.
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
- **Client record writes:** renew 10⁴ NFSv4 clients for ten lease periods.
  Assert no `CL‖` key is written. Replay a `CREATE_SESSION` after a restart:
  assert it is answered from `CSReply`. Negotiate one SMB `ClientGuid` under two
  principals: assert one record, with no principal.
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
- an epoch change mid-operation (the old primary's writes are refused, namespace
  transactions included, through the fence they guard).

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

### 7.2 Targets, gates and regressions

Targets are relative first: a create or stat through DittoFS against the same
operation on a local filesystem and on a reference NAS on the same machine.
Absolute targets follow from the first measured baseline, and are agreed per
backend. The entity benchmarks run after each merge and the protocol and scale
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
RFC 6 Appendix B. These are raw key and value bytes; the backend's own overhead
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
| ChunkRef, live | 42 + 89 = 131 | chunk | 8.6×10⁹ | 1.13 TB |
| version-floor index entry | 59 + 0 ≈ 60 | file | 10⁸ – 10⁹ | 0.006 – 0.06 TB |
| Chunk | 49 + 61 = 110 | chunk | 8.6×10⁹ | 0.95 TB |
| reverse ref key (`CR`) | 105 + 0 ≈ 105 | ref | 8.6×10⁹ | 0.90 TB |
| Block, with its carried list (16 chunks of a 4 MiB block × 36 B) | 49 + ~580 ≈ 630 | block | 5.4×10⁸ | 0.34 TB |
| File (attributes and write-path fields) | 33 + 160 ≈ 200 | file | 10⁸ – 10⁹ | 0.02 – 0.2 TB |
| Entry | 58 + 42 = 100 | file | 10⁸ – 10⁹ | 0.01 – 0.1 TB |
| fences `F_x`, `F_o` | 2 × (35 + 9) ≈ 90 | file | 10⁸ – 10⁹ | 0.01 – 0.09 TB |
| history ref + its chunk + its reverse key + its died-index key (snapshots) | 131 + 110 + 105 + 55 = 401 | superseded chunk | 30 × 1% × 8.6×10⁹ = 2.6×10⁹ | 1.04 TB |
| File history + its died-index key (snapshots) | ≈ 255 | changed file | 30% of files | 0.008 – 0.08 TB |

| Totals | Keys | Bytes | Three replicas |
| --- | --- | --- | --- |
| without snapshots, 10⁸ files | 2.7×10¹⁰ | 3.4 TB | 10.1 TB |
| without snapshots, 10⁹ files | 3.1×10¹⁰ | 3.8 TB | 11.3 TB |
| with snapshots, 10⁸ files | 3.7×10¹⁰ | 4.4 TB | 13.3 TB |
| with snapshots, 10⁹ files | 4.2×10¹⁰ | 4.9 TB | 14.7 TB |

Content records (ref, chunk, reverse key: 346 bytes and three keys per chunk)
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
| 256 KiB (default) | 8.6×10⁹ | 3.0 TB | ≈ 0.3 MiB |
| 1 MiB | 2.1×10⁹ | 0.73 TB | ≈ 1.2 MiB |
| 4 MiB | 5.4×10⁸ | 0.19 TB | ≈ 5 MiB |

**Recommendation (non-normative).** A share of large, mostly sequential files —
media, checkpoints, backups, a mean file of 1 GiB or more — is better served by a
namespace `Target` of 1 MiB: a quarter of the metadata, at the cost of a larger
cold fetch per small random read and coarser deduplication ([RFC 2](rfc-2-carver.md)).
`Target` is a namespace setting and bound ([RFC 13](rfc-13-configuration.md)), so it is chosen at
creation. Deduplication lowers the chunk rows, never the ref rows.

## 8. Observability

Metric names are shown without the deployment's prefix, which the exporter
adds. On fold, they follow RFC 25's conventions; until then they start
`metadata_`.

### 8.1 Metrics

| Answers | Metric | Type |
| --- | --- | --- |
| How much does each transaction touch? | `metadata_txn_keys{op, kind=read\|written}` | histogram |
| Is contention costing us? | `metadata_txn_retries_total{op}`, `metadata_txn_conflicts_total{area}` | counters |
| Is the KV the bottleneck? | `metadata_kv_seconds{call}` | histogram |
| Are transactions near the backend's limits? | `metadata_txn_size{kind=entries\|bytes}` against `…_txn_limit` | histogram, gauge |
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
