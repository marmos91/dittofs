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

> [!important] Pending review — folded from the metadata model
> This RFC is the part of the reviewed metadata model that has no narrower
> home. File and namespace entities moved to [RFC 7](rfc-7-namespace-metadata.md), content entities to [RFC 6](rfc-6-block-metadata.md),
> open state to [RFC 14](rfc-14-open-state.md), roles and routing to [RFC 15](rfc-15-topology.md), the filesystem service to [RFC 17](rfc-17-vfs.md).

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). Signatures are indicative; the rules in §1.2 are normative.

---

## 1. Purpose

The metadata layer used to be specified as records: what is stored under which
key. That is the right level for performance and the wrong one for everyone who
uses the layer. This RFC specifies the layer the other way round: the
**entities** a caller works with, the **interfaces** each caller holds, and the
**persistence pattern** that maps entities onto a transactional key-value store.
It is the one place where the whole `metadata` package can be read at once.

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

All entities live in one package, `metadata`. Each is a plain struct with no
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
| `Client`, `Open`, `Lock`, `CachingGrant`, `Watch` | open state | [RFC 14](rfc-14-open-state.md) |
| `ChunkRef`, `Chunk`, `Block`, `ChunkHash`, `BlockName`, `JournalVersion`, `SnapshotCut` | the content map, the blocks it lives in, and the cut numbering snapshots | [RFC 6](rfc-6-block-metadata.md) |
| `FileID`, `ShareID`, `Principal`, `PrincipalID` | identity | §2.2 |
| `User`, `Group`, `Membership` | principals with names | §2.3 |
| `Share`, `ShareGrant`, `Snapshot` | a share, who may reach it, its snapshots | §2.3 |
| `Node`, `OwnershipUnit` | cluster membership and ownership ([RFC 11](rfc-11-ownership.md)) | §2.3 |
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
// ACE grants or charge. Protocol spellings live only in the PX‖ index (§4.2);
// this layer compares IDs and never interprets a spelling.
type Principal struct {
	Kind PrincipalKind // User, Group, Special (owner, group, everyone: fixed well-known IDs)
	ID   PrincipalID
}
```

Every other identity type sits beside the only entities that use it: the
content identities (`ChunkHash`, `BlockName`, `JournalVersion`) and
`SnapshotCut` with the content entities in [RFC 6](rfc-6-block-metadata.md#2.%20The%20records), which defines them once.

> [!important] Pending review — opaque principal IDs
> `Principal.ID` is a minted, never-reissued ID; UID, GID and SID appear only in
> the `PX‖` index. An import maps principals by ID and refuses a collision (§4.2).

### 2.3 Server-wide and control-plane entities

The control plane persists in the same database as file metadata, through the
same `KV` and the same codecs (§4). One database means one thing to run, back
up, replicate and make highly available, and one conformance suite; a second
database kept only for users and shares would double every one of those.

These entities differ from the file, open-state and content entities ([RFC 7](rfc-7-namespace-metadata.md), [RFC 14](rfc-14-open-state.md), [RFC 6](rfc-6-block-metadata.md)) in **scope**, and the difference decides
what moves with a share:

| Scope | Entities | Moves with a share export ([RFC 12](rfc-12-snapshots.md)) |
| --- | --- | --- |
| Per share | Share, ShareGrant, Snapshot | yes |
| Server-wide | User, Group, Membership, Node, OwnershipUnit, Setting | no — an export carries the principals its files and grants reference (§5, decision 8) |

```go
// User and Group are principals with a name. A file refers to them only by
// Principal (§2.2), never by name, so renaming a user touches no file and
// deleting one leaves its principal in ACLs as an unknown principal, which is
// how both protocols already report it.
type User struct {
	Principal Principal
	Name      string
	IDs       ProtocolIDs   // UID, SID, Kerberos principal: the mapping RFC 18 owns
	Disabled  bool
	Secret    SecretRef     // by reference, never the credential itself (RFC 13)
}

type Group struct {
	Principal Principal
	Name      string
	IDs       ProtocolIDs   // GID, SID
}

// Membership is one member of one group, its own record in both directions:
// a group of 10^5 users is not a list on the group (§1.2 rule 2).
type Membership struct {
	Group  Principal
	Member Principal // a user or a nested group
}

// Share is one exported filesystem.
type Share struct {
	ID     ShareID
	Name   string
	Root   FileID
	State  ShareState // enabled, quiesced, frozen for a cut, being removed
	Config SettingsRef
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

// Snapshot is a named, user-visible cut of a share. SnapshotCut, and the
// born/died rule every versioned record follows, are RFC 6 §6.5's.
type Snapshot struct {
	Share   ShareID
	Cut     SnapshotCut
	Name    string
	Created time.Time
	Expires time.Time // zero: kept until deleted
}

// Node is one server process in a cluster; OwnershipUnit is one ownership unit and
// its current owner (RFC 11).
type Node struct {
	ID       NodeID
	Address  string
	Roles    Roles // protocol, storage; both by default (RFC 15)
	LastSeen time.Time
}

// OwnershipUnit has one owner, one epoch and one lease (RFC 11). The epoch is
// fenced per file, not here: each file's F_x and F_o records carry it (§4.2,
// RFC 6 §5.4), so no commit reads this record.
type OwnershipUnit struct {
	ID      OwnershipUnitID
	Share   ShareID
	Owner   NodeID
	Epoch   uint64
	Expires time.Time // ownership lease
}

// Setting is one configuration value at one scope (RFC 13).
type Setting struct {
	Scope SettingScope // server, share, adapter
	Key   string
	Value []byte       // secrets appear only as references
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
	Kind  SecretKind // password (slow hash), nt-hash (only with NTLM enabled), keytab, remote credential, key
	KeyID string     // which wrapping key sealed it: one per role or secret kind
	Value []byte     // sealed
}
```

A protocol node needs authentication secrets and a storage node needs remote-tier
credentials and key material; neither needs the other's. With one wrapping key,
a compromised protocol node could unseal the bucket credential. The KV still
lets any node read the sealed bytes; confidentiality rests on the key a node's
bootstrap names, and restricting what a node may *write* is [RFC 15](rfc-15-topology.md)'s.

> [!important] Pending review — wrapping key per role
> `Secret.KeyID` names a wrapping key per role or secret kind, and a node's
> bootstrap names only its own roles' keys ([RFC 13 §7](rfc-13-configuration.md#7.%20Secrets)).

Adapter settings, netgroups and identity-provider configuration follow the
same pattern and are left out of this list; none of them changes a file or
content key.

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
	PutShare(ctx context.Context, s Share) error
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
| Authentication, tree connect, mount | `Principals` |
| Management API | `ControlPlane` |
| Ownership (RFC 11) | `Node` and `OwnershipUnit` records through its own view, fenced by epoch |
| Debug tooling | `Dump` (§4.5) |

**The metadata store is a leaf.** It never calls the engine, and no view here
holds an engine interface. There is one read path for a file's attributes: the
filesystem service's `GetAttr` joins `Files.Get` with the engine's overlay of
`Size`, `Times` and `Version` for bytes not yet committed ([RFC 8](rfc-8-engine.md)); nothing
else answers size or times, and `Existence` offers no read of them.

> [!important] Pending review — one read path, store is a leaf
> The namespace no longer holds `Existence`'s read side; `GetAttr` joins
> `Files.Get` with the engine's `Size/Times/Version` overlay in the filesystem service.

`User` and `Share` here are entities, not wire types: `PutUser` receives a
`User`, and the only field it writes that another entity reads is the name
index. That is the one place in the model where a whole-entity put is
allowed, because control-plane entities have one writer (the management API)
and no background path.

### 3.1 Lookups by ID, listings, and who may use them

The adapter views take handles and an identity because every one of their calls
is a client's, and is authorised. Internal consumers — snapshots and export
(RFC 12), the audit, repair, the management API and the debug tool — work by ID,
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

This is the shape the control plane's `Runtime` already has: sub-services over
shared state, one entry point, narrow views handed out.

## 4. Persistence pattern

### 4.1 One small interface per backend

The entity layer is written once. A backend implements only this:

```go
type KV interface {
	// Update runs fn in one transaction and retries it on a serialisation
	// conflict under ctx's deadline (RFC 0 §9.2). fn may run more than once.
	Update(ctx context.Context, fn func(Txn) error) error
	View(ctx context.Context, fn func(Reader) error) error
	Limits() TxnLimits // entries and bytes per transaction: derives RFC 6's K
}

type Reader interface {
	Get(key []byte) ([]byte, error) // ErrNotFound
	Scan(prefix, after []byte) iter.Seq2[KeyValue, error]
}

type Txn interface {
	Reader
	Set(key, value []byte) error
	Delete(key []byte) error
	// Guard makes the transaction conflict with any concurrent write to key,
	// whether or not the transaction writes it. Guards are SHARED: two
	// transactions guarding one key do not conflict with each other, only
	// with a transaction that writes it. A gating read uses it (RFC 6 §5.4).
	Guard(key []byte) error
}
```

**`Guard` is shared.** A transaction that guards a key **MUST** conflict with
every concurrent transaction that writes that key, and **MUST NOT** conflict
with one that only guards it. Creates in one directory each guard the parent
(`Guard(F‖parent)`, [RFC 7](rfc-7-namespace-metadata.md)) and every namespace transaction guards its file's
fence (RFC 6 §5.4); an exclusive guard would serialise both, which is the
one-directory create rate of Appendix A again. On a backend that tracks point
reads a guard is a tracked read; on one that validates no reads it is a
shared (read) lock on the key, and a write takes the exclusive lock.

> [!important] Pending review — shared Guard
> `KV.Guard` is specified shared: guards conflict with writes, never with each
> other. KV conformance gains the check (§6.1).

Two backends with identical semantics differ only here, and one conformance
suite over `KV` plus one over the entity layer covers both.

### 4.2 Keys: per-file, per-share, content-addressed

Every key starts with a kind byte. The layout encodes the boundary
[RFC 6 §1.2](rfc-6-block-metadata.md#1.2%20Why%20it%20is%20a%20separate%20RFC%20from%20the%20namespace) draws between what belongs to one file and what is shared:

| Scope | Key | Entity or record |
| --- | --- | --- |
| **Per file** — all under `F‖ShareID‖FileID`, written `F‖id` below | `F‖id` | File: every field, FileData's included, one value |
| | `F‖id‖acl` | ACL |
| | `F‖id‖x‖name` | Xattr |
| | `F‖id‖s‖streamID` | named stream link |
| | `F‖id‖e‖key` | Entry, under its **parent** directory. `key` is the name folded by the share's fold rule (§4.6), the identity on a case-sensitive share; the name's original bytes are in the value ([RFC 7 §3.3](rfc-7-namespace-metadata.md#3.3%20Case)) |
| | `F‖id‖t‖unique` | directory time delta, carrying the cut it was written after (§4.4) |
| | `F‖id‖h‖start`, `F‖id‖rm‖version` | Hole, Removal |
| | `F‖id‖r‖offset` | ChunkRef, live |
| | `F‖id‖H‖died‖suffix` | **history** of a versioned per-file record: the value it had, under its live key's suffix — `r‖offset` (ChunkRef), empty (File), `acl`, `x‖name`, `s‖streamID`, `e‖key`, `h‖start` ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)) |
| | `F‖id‖fx`, `F‖id‖fo` | fences: the epoch of the file's unit owner ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit)) |
| | `F‖id‖ru‖start` | range unit record: owner, epoch, lease, members and committed end of one byte range split off the file ([RFC 11 §2.3](rfc-11-ownership.md#2.3%20Range%20units)); size is the maximum committed end over these and the base unit |
| | `F‖id‖ru‖start‖fx`, `F‖id‖ru‖start‖fo` | per-range fences: the range owner's epoch, guarding existence and offload commits for bytes in that range ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit)) |
| | `F‖id‖rel` | pending release: written by the final unlink, deleted only by the release transaction; holds no holder list, the holders are the `o‖` records ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)) |
| | `F‖id‖o‖openID` | durable Open (only the cases RFC 14 makes durable: keeps an unlinked file alive, SMB persistent handle) |
| | `F‖id‖o‖openID‖l‖start` | durable Lock, only under a persistent open, so closing the open drops one prefix ([RFC 14](rfc-14-open-state.md)) |
| **Per share** — under `S‖ShareID` | `S‖id‖info` | Share |
| | `S‖id‖g‖principal` | ShareGrant |
| | `S‖id‖snap‖cut` | Snapshot; nothing else lives under this prefix, so listing snapshots reads only snapshots |
| | `S‖id‖cut`, `S‖id‖live‖k` | Cut, LiveCut |
| | `S‖id‖u`, `S‖id‖pu‖principal`, `S‖id‖pj‖project` | folded usage: share, principal, project (§4.4) |
| | `S‖id‖ud‖unit‖unique` | usage delta, not yet folded, per ownership unit (§4.4) |
| | `S‖id‖q‖principal-or-project` | Quota: `Hard`, `Soft`, `Grace`, `Advisory` ([RFC 7](rfc-7-namespace-metadata.md)) |
| | `S‖id‖qx‖principal-or-project` | when usage first exceeded the soft limit; deleted when it falls back under ([RFC 17 §5.6](rfc-17-vfs.md#5.6%20Quota)) |
| | `S‖id‖vf‖version‖FileID‖offset` | version-floor index ([RFC 6 §8.3](rfc-6-block-metadata.md#8.3%20A%20file%27s%20refs%20and%20the%20version%20floor)) |
| | `S‖id‖fnc` | numeric file-id allocator: the next unreserved number, reserved in ranges by unit owners, so the protocol's numeric id is injective ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20derived%2C%20and%20collisions%20are%20its%20problem)); the number itself is a File field |
| **Per namespace** — content-addressed, one partition per remote key namespace ([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)) | `C‖ns‖hash`, `B‖ns‖name` | Chunk, Block (with its GC state: `live`, `retired`, `deleted`) |
| | `I‖ns‖name` | put intent ([RFC 9 §7.2](rfc-9-gc.md#7.2%20Every%20record%20GC%20stores%20names%20its%20reclamation)) |
| | `BZ‖ns‖name`, `BR‖ns‖not_before‖name`, `BD‖ns‖name`, `BC‖ns‖name` | GC index: blocks at `live` = 0, retired (in the trash, ordered by `not_before`), deleted (awaiting the object's delete), past the compaction threshold. Derived from block records; droppable and rebuildable ([RFC 9 §7.4](rfc-9-gc.md#7.4%20The%20index%20can%20be%20dropped%20and%20rebuilt)) |
| | `NS‖ns‖gc‖lease`, `NS‖ns‖gc‖summary` | GC lease, last pass summary ([RFC 9 §7.3](rfc-9-gc.md#7.3%20GC%20is%20one%20service%20per%20namespace)) |
| | `NS‖ns‖gc‖cursor‖walk`, `NS‖ns‖gc‖scratch‖walk‖hash` | pass cursor per kind of walk (audit, index rebuild, compaction scan), audit scratch, so a restarted pass resumes; derived, like the summary |
| **Server-wide** | `U‖principal`, `G‖principal` | User, Group, keyed by `PrincipalID` |
| | `M‖group‖member`, `MR‖member‖group` | Membership, both directions |
| | `NX‖kind‖name` | name index: user, group and share names → ID, unique |
| | `PX‖scheme‖id` | protocol-ID index: UID, GID, SID → `PrincipalID`; the only place a protocol spelling is stored (§2.2) |
| | `N‖node`, `UT‖unit` | Node, OwnershipUnit; `UT‖unit` also holds the cursor of a move the unit is giving files away in, so a crashed move resumes ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20ownership)) |
| | `CFG‖scope‖key` | Setting |
| | `SEC‖id` | Secret (§2.3): envelope-encrypted, never dumped or exported |
| | `CL‖clientID`, `CL‖clientID‖u‖unit` | durable Client record: only what reclaim needs, and the units the client held state in (`Client.Units`), checked on reclaim ([RFC 14](rfc-14-open-state.md)) |
| **Store** | `\x00format` | store format record (§4.6) |

The table is **exhaustive**: every key the store writes has a row. A new
record kind is a format bump (§4.6) and gets its row in the same change;
volatile open state (locks, caching grants, watches, non-durable opens) and
the owner's in-memory tables (quota reservations, routed-request dedup) are
never written to the KV and so have none. A file's unit and its owner epoch
need no row of their own: the unit is a `File` field and the epoch is in the
fences.

> [!important] Pending review — key table
> Content keys gain the namespace; rows added for history (`H‖`), durable
> locks, the numeric-id allocator, the version-floor index, relocation
> candidates, GC lease, summary and scratch, and client units. The reserved
> reverse-name row and snapshot captures are gone.

> [!important] Pending review — GC index keys
> Pending-deletion (`D‖`), candidate (`K‖`) and relocation-candidate (`RC‖`)
> keys are replaced by the block record's own state and four derived index
> prefixes (`BZ‖`, `BR‖`, `BD‖`, `BC‖`) that an operator can drop and rebuild.

Consequences:

- `Files.Get` is one read of one key. The File was two keys (attributes, and
  the write path's fields) so that `chmod` and a write never conflicted; the
  second key cost 25% on `GETATTR`, 60% on `READDIRPLUS` and 30% on serial
  creates (Appendix A), to avoid a conflict that is rare and cheap to retry.

  > ponytail: one key per File, so `chmod` and an existence commit on one file
  > conflict and one retries. Split the write path's fields into their own key
  > when a profile shows those retries.
- Every per-file key carries the `ShareID`. Deleting or exporting a share is a
  prefix operation (a range drop) instead of a tree walk; a share's files stay together for
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
- A share's export is `S‖id` plus the `F‖` prefixes of the files its tree
  reaches, plus the server-wide records its principals name. Nothing
  server-wide is copied wholesale into another installation. An import maps
  principals by `PrincipalID` and **MUST** refuse one whose ID already names a
  different principal, or whose protocol ID is already mapped to another.
- Content-addressed keys carry the namespace, so two namespaces never share a
  chunk record, a count, a candidate or a GC pass, although one database holds
  them all ([RFC 6 §2.6](rfc-6-block-metadata.md#2.6%20The%20scope%20of%20a%20count)).

### 4.3 Codecs

Each persisted type has one codec: a version byte followed by a fixed field
order. The per-value version byte is the **only** codec-version mechanism: a
lazy upgrade leaves old and new values side by side, and each decodes by its
own byte. Decoding refuses an unknown version rather than guessing. A codec never
embeds a list that grows with a file (§1.2 rule 2); a history ref, a hole and an
xattr are each their own key.

### 4.4 Counters that many writers change

**A counter is never read and rewritten by the transaction that changes it.**
Measured on the embedded store (Appendix A), one shared counter updated inside
every create cut throughput 4–7× with 11–43 retries per create; 16 shards
recovered most of it at 16 writers but still halved it at 64. The replicated
store serialises writes to one key outright, so a hot counter there caps a
share's create rate.

Instead, a transaction that changes charged bytes writes a **delta record** —
a new key, unique to the transaction (`S‖id‖ud‖unit‖unique`), holding the
changes to the share's, the owner's, the group's and the project's usage. It
reads nothing, so it conflicts with nothing: measured, it ran at the rate of no
counter at all. The `unique` part **MUST** be unique across writers (the
owner's node ID and epoch, then a sequence): on a backend that detects no blind
write-write conflict, two deltas under one key would silently lose one.

**The unit's owner folds.** Each ownership unit's owner ([RFC 11](rfc-11-ownership.md)) folds the
deltas its unit wrote into the totals in batches, one transaction per batch,
deleting the deltas it folded. A fold transaction guards `UT‖unit`, so a
former owner's fold conflicts with the takeover that rewrites it; a fold is
background work, so this one record per unit is paid per batch, never per
client operation. Two units' folds write the same total keys and one retries,
which costs the fold cadence and nothing else. A reader — `statfs`, a quota
check — reads the totals plus the deltas not yet folded; the fold keeps that
set small, and `dittofs_metadata_unfolded_deltas` is the alert when it does not.

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
owner therefore holds an in-memory **reservation** per principal and per
project: a write reserves the bytes it may add before it is acknowledged, the
check is totals + unfolded deltas + this owner's reservations against the
limit, and the reservation is released when the existence commit charging those
bytes lands. A crash drops the reservations with the uncommitted writes they
covered, and replay re-reserves what it replays.

> decision: quota fails open by a stated bound, not exactly. One owner sees
> only its own reservations, so where one principal writes through several
> units concurrently the overshoot is at most the reservation slack each such
> owner may hold beyond the committed total, summed over those owners; with one
> unit it is zero. Tighten it by leasing each owner a slice of the remaining
> quota if a deployment shows overshoot past its slack.

> [!important] Pending review — quota reservation and charging
> Overshoot is bounded by per-owner reservation slack, not by concurrent commits;
> a file is charged its logical bytes in `File.Charged`, moved by `chown`.

**Directory times are the same problem.** Every create, unlink and rename in a
directory updates its `Modify`, `Change` and `Version`; measured, that
read-and-rewrite cut parallel creates in one directory from 111k/s to 28k/s,
whatever the file layout. Directory time changes are therefore delta records
under the directory (`F‖id‖t‖unique`), folded by the directory's unit owner the
same way, and a directory's `Get` applies any unfolded ones — usually none. A
delta does not replace the conflict the old rewrite gave for free: the
structural operations guard the parent instead ([RFC 7](rfc-7-namespace-metadata.md), §4.1).

**A directory-time delta carries the cut it was written after**, the share's
`k` its transaction read ([RFC 6 §6.5](rfc-6-block-metadata.md#6.5%20Who%20owns%20a%20ref)). Snapshot *k* sees a directory as the
version of its `File` visible at *k* plus the unfolded deltas with `born < k`.
For that to hold, the fold **MUST** fold deltas in `born` order, one `born`
value per transaction, and write the folded record with that `born`; the value
it supersedes moves to history with `died` = that `born` when a live cut sees
it. A transaction that rewrites the directory record itself (a `SETATTR`, a
rmdir) first folds, in the same transaction, the directory's deltas with
`born` below the `k` it read; those committed before the current cut, so the
scan that finds them races no writer. Usage deltas carry no cut: a snapshot
reports no usage of its own.

> [!important] Pending review — delta records carry their cut
> Directory-time deltas record `born`; the fold runs in `born` order so
> versioned directory records stay exact at every cut without a drain.

> ponytail: one folder per unit, run by the unit's owner. Fold throughput
> caps the sustained rate of charge-changing transactions per unit; shard the
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
rules the store's shares use. Opening a store **MUST** read it first and refuse a
format newer than the binary supports, before reading any other key. A new
record kind, a key encoding, or permission to write a new codec version is a
format bump: format *n* names, in code, the codec versions a binary may write,
so no value is written that an older reader of format *n* cannot decode. A
newer binary opens an older store and upgrades it by writing, an older binary
refuses a newer one. This is the same rule [RFC 1 §4.3](rfc-1-journal.md) states for the journal.

**Fold rules.** An entry's key is its name folded by the share's fold rule
(§4.2), so the rule is part of the key encoding: a binary that folded one name
differently would miss existing entries and admit duplicates. Each fold rule is
recorded in the format record by ID with its definition's version (the
Unicode version and case mapping it applies); a share's case setting names
one ID and is bound ([RFC 13](rfc-13-configuration.md)); a binary that does not implement every
recorded rule **MUST** refuse the store. The identity rule serves
case-sensitive shares.

> [!important] Pending review — one version mechanism; fold rule in the format record
> The format record holds one format integer (no per-codec map) plus the fold
> rules entry keys depend on.

## 5. Decisions and evidence

Each question was put to prior art and, where the embedded store could answer
it, to a benchmark (Appendix A). Sources are linked once per system.

| # | Question | Answer | Evidence |
| --- | --- | --- | --- |
| 1 | Drop `generation`? | **Yes**, with share-scoped keys (§4.2). A restore into a new share gets new keys and cannot alias an old handle; an in-place rollback revives the same files, whose old handles rightly work again — ZFS behaves the same. | JuiceFS and 3FS use never-reused IDs with no generation; [ZFS handles after rollback](https://github.com/openzfs/zfs/issues/9587) |
| 2 | `Mode` beside the ACL? | **Keep both.** `Mode` lives on the File so `GETATTR` never reads the ACL; `chmod` follows [RFC 8881 §6.4.1.1](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4.1.1) exactly ([RFC 7](rfc-7-namespace-metadata.md)); a file with no ACL gets one synthesised on read. Reject "last writer wins". | [RFC 8881 §6.4](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4) requires them to agree; OneFS "Balanced", [Qumulo XPP](https://docs.qumulo.com/administrator-guide/authorization-qumulo-core/managing-cross-protocol-permissions-xpp.html); ONTAP mixed style is the cautionary case |
| 3 | Shard usage counters? | **No — delta records and a fold** (§4.4). Shards spread collisions; deltas remove them. | Appendix A; [JuiceFS quota design](https://juicefs.com/en/blog/engineering/quota-design-in-distributed-architecture) batches deltas too |
| 4 | Open state durability? | **One rule per entity:** `Client` durable (only what RFC 8881 §8.4.3 requires for reclaim); `Open` volatile and reclaimed in grace, durable when it keeps an unlinked file alive or is an SMB persistent handle; `Lock` volatile unless its open is persistent; `CachingGrant` and `Watch` volatile, never reclaimed. Grace may run per ownership unit. | knfsd `nfsdcld` and Ganesha `rados_cluster` store client records only; CephFS rebuilds caps on reconnect; SMB persistent handles are the one durable case |
| 5 | Stream as File? | **Yes for content, NTFS for identity:** a stream has no owner or ACL of its own, reports its base file's ID to SMB, and is released with it. | NTFS streams are attributes of one file record; ZFS named attributes and Samba `streams_depot` are files |
| 6 | Share in per-file keys? | **Yes**: `F‖ShareID‖FileID` (§4.2). | JuiceFS volume prefix; TiKV range deletion drops a share in seconds, not hours |
| 7 | Nested groups? | **Walk with a depth cap and cache.** Kerberos and AD already deliver the flattened list in the ticket, so no walk for them. Local groups are walked at login, depth ≤ 8 with cycle detection, cached under a key naming every membership it read (RFC 7 §7.5). No stored closure table. | Kerberos PAC; SSSD nesting level; Zanzibar's flattened index shows the closure's write cost |
| 8 | Deleted principals? | Deleting a user removes the User, its memberships and grants; the principal stays in ownership, ACLs and usage. An admin **reassign** job moves files and their charge through `chown`. A deleted principal's ID is never reissued, because IDs are minted, not derived from a UID or SID (§2.2); a new account reusing the UID gets a new principal. | ONTAP shows raw SIDs in quota reports; NTFS refuses to drop a quota entry while the SID owns files; MS-SMB2 returns zeroed entries for unknown SIDs |
| 9 | Where credentials live? | **In the KV, as `Secret` records** (§2.3), envelope-encrypted under a key held outside it (a file or a KMS), never dumped or exported. Password hashes use a slow hash; an NT hash exists only when NTLM is enabled. Keytabs stay files, referenced by path. | Samba `tdbsam`, TrueNAS and ONTAP all keep credentials in their replicated config store; Samba warns NT hashes are cleartext-equivalent |

> ponytail: nested local groups are walked at login, depth ≤ 8, and cached
> (decision 7). The walk costs one read per membership edge; add a stored
> closure only if login latency for deeply nested local groups shows in a
> profile.

A FileID is never minted near its parent's to place a create on one region:
handles are not MACed, so an ID that can be guessed from its parent's is a
handle a client could forge ([RFC 7 §6](rfc-7-namespace-metadata.md#6.%20Handles)).

Still open, for measurement on the replicated store:

- the fold's sustained throughput per share (§4.4's ponytail);
- whether `EntriesPlus` batching closes enough of the `READDIRPLUS` gap.

## 6. Testing

The conformance sections of RFC 6, RFC 7 and RFC 14 run these suites; test
tiers are the [index](rfc-index.md)'s.

### 6.1 Two suites, layered like the code

| Suite | Runs against | Proves |
| --- | --- | --- |
| **KV conformance** | every `KV` implementation (embedded, replicated, and an in-memory fake used only by this suite's own tests) | `Update` is atomic and retried; `Guard` conflicts with a concurrent write of its key and never with a concurrent `Guard` of it (§4.1, [RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit)); `Scan` sees a transaction's own writes and nothing uncommitted; `Limits` is honest (a transaction at the limit commits, one past it is refused) |
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
  the repository. A codec change that alters them without bumping the version
  fails.
- **Refusal:** decoding a newer version, a truncated value or a trailing byte
  fails with a named error, never a zero value.
- **Format record:** a store written by version *n* opens under *n+1* and is
  refused by *n−1* (§4.6).
- **Cost assertions:** each §3 method has a stated bound on keys read and
  written (for example `Files.Get`: one scan, at most three keys; `Create`: at
  most six writes). A counting `KV` wrapper records them, and the suite fails
  when a method exceeds its bound. This is what keeps rule 2 of §1.2 true after
  the next refactor.

### 6.4 Faults and crashes

A fault-injecting `KV` wrapper fails, delays or reorders at every call:

- a conflict on the Nth `Update` attempt (retry must converge, never apply twice);
- a lost commit reply (the operation must be idempotent from the caller's side);
- a process kill between the phases of a batched removal (RFC 6 §6.2),
  resumed on restart;
- an epoch change mid-operation (the old owner's writes are refused, namespace
  transactions included, through the fence they guard).

## 7. Benchmarks and profiling

### 7.1 What is measured

| Level | Tool | Measures |
| --- | --- | --- |
| **KV** | Go benchmarks over `KV` | per-call latency and throughput, conflict rate under contention, on each backend |
| **Entity** | Go benchmarks over §3 | per-operation latency, keys read and written, allocations, for: create, lookup, getattr, readdir and readdirplus (10, 10⁴, 10⁶ entries), rename, unlink, set-ACL, commit of an offload, removal of a large file |
| **Protocol** | standard metadata workloads over NFS and SMB mounts | `mdtest` (create, stat, remove, per directory and shared directory), a small-file build tree (untar, compile, `rm -rf`), `ls -l` of a large directory |
| **Scale** | a synthetic namespace | 10⁸ files, 10⁴ shares, 10⁵ principals: operation latency must not grow with total size, only with what each operation touches |

Every benchmark states its backend, machine, and whether the KV was local or
over a network. A number from the embedded KV says nothing about the
replicated one, which adds a network round trip per read and a two-phase commit
per multi-region transaction; each is measured on its own.

### 7.2 Targets, gates and regressions

Targets are relative first: a create or stat through DittoFS against the same
operation on a local filesystem and on a reference NAS on the same machine.
Absolute targets follow from the first measured baseline, and are agreed per
backend. Per pull request, the entity benchmarks run and a regression past a
stated threshold is reported, not blocking; nightly, the protocol and scale
benchmarks run and are tracked over time.

### 7.3 Profiling

- Every operation runs under a `pprof` label naming it (`op=create`,
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
bytes; no stored ACL (synthesised from mode); a 64 MiB block target, so about
3.4×10⁷ blocks. Snapshots: 30 daily cuts retained, 1% of bytes and 1% of files
rewritten per day.

| Record | Key + value (B) | Per | Count | Bytes |
| --- | --- | --- | --- | --- |
| ChunkRef, live | 42 + 89 = 131 | chunk | 8.6×10⁹ | 1.13 TB |
| version-floor index entry | 59 + 0 ≈ 60 | ref | 8.6×10⁹ | 0.52 TB |
| Chunk | 49 + 61 = 110 | chunk | 8.6×10⁹ | 0.95 TB |
| Block | 49 + ~46 ≈ 95 | block | 3.4×10⁷ | < 0.01 TB |
| File (attributes and write-path fields) | 33 + 160 ≈ 200 | file | 10⁸ – 10⁹ | 0.02 – 0.2 TB |
| Entry | 58 + 42 = 100 | file | 10⁸ – 10⁹ | 0.01 – 0.1 TB |
| fences `F_x`, `F_o` | 2 × (35 + 9) ≈ 90 | file | 10⁸ – 10⁹ | 0.01 – 0.09 TB |
| history ref + its chunk (snapshots) | 131 + 110 = 241 | superseded chunk | 30 × 1% × 8.6×10⁹ = 2.6×10⁹ | 0.62 TB |
| File history (snapshots) | ≈ 200 | changed file | 30% of files | 0.006 – 0.06 TB |

| Totals | Keys | Bytes | Three replicas |
| --- | --- | --- | --- |
| without snapshots, 10⁸ files | 2.6×10¹⁰ | 2.6 TB | 7.9 TB |
| without snapshots, 10⁹ files | 3.0×10¹⁰ | 3.0 TB | 9.0 TB |
| with snapshots, 10⁸ files | 3.1×10¹⁰ | 3.3 TB | 9.8 TB |
| with snapshots, 10⁹ files | 3.5×10¹⁰ | 3.7 TB | 11 TB |

Content records (ref, index entry, chunk: 301 bytes and three keys per chunk)
are about 87% of the total at 10⁹ files, and they scale as 1/`Target`. So
`Target` is the lever, not the file layout:

| `Target` | Chunks at 2 PiB | Content records | Cold 4 KiB read fetches |
| --- | --- | --- | --- |
| 256 KiB (default) | 8.6×10⁹ | 2.6 TB | ≈ 0.3 MiB |
| 1 MiB | 2.1×10⁹ | 0.65 TB | ≈ 1.2 MiB |
| 4 MiB | 5.4×10⁸ | 0.16 TB | ≈ 5 MiB |

**Recommendation (non-normative).** A share of large, mostly sequential files —
media, checkpoints, backups, a mean file of 1 GiB or more — is better served by a
namespace `Target` of 1 MiB: a quarter of the metadata, at the cost of a larger
cold fetch per small random read and coarser deduplication ([RFC 2](rfc-2-carver.md)).
`Target` is a namespace setting and bound ([RFC 13](rfc-13-configuration.md)), so it is chosen at
creation. Deduplication lowers the chunk rows, never the ref rows.

> [!important] Pending review — metadata sizing
> About 3×10¹⁰ keys and 3–4 TB raw at 2 PB with the default `Target`; a 1 MiB
> `Target` is recommended for large-file shares. Not yet measured on a backend.

## 8. Observability

On fold, metric names follow RFC 25's conventions; until then they use
`dittofs_metadata_`.

### 8.1 Metrics

| Answers | Metric | Type |
| --- | --- | --- |
| How much does each transaction touch? | `dittofs_metadata_txn_keys{op, kind=read\|written}` | histogram |
| Is contention costing us? | `dittofs_metadata_txn_retries_total{op}`, `dittofs_metadata_txn_conflicts_total{area}` | counters |
| Is the KV the bottleneck? | `dittofs_metadata_kv_seconds{call}` | histogram |
| Are transactions near the backend's limits? | `dittofs_metadata_txn_size{kind=entries\|bytes}` against `…_txn_limit` | histogram, gauge |
| Is the fold keeping up? | `dittofs_metadata_unfolded_deltas{kind=usage\|dirtime}`, `dittofs_metadata_fold_seconds` | gauge, histogram |
| Store | `dittofs_metadata_format_version`, `dittofs_metadata_codec_errors_total{kind}` | gauge, counter |

Every metric here is internal to the store. Per-operation latency is the
filesystem service's `dittofs_vfs_op_seconds` ([RFC 17](rfc-17-vfs.md)); forwarding and epoch
refusals are [RFC 15](rfc-15-topology.md)'s; open state, recalls and grace are [RFC 14](rfc-14-open-state.md)'s; quota
refusals and overshoot are [RFC 17](rfc-17-vfs.md)'s. Each metric has one owning RFC.

> [!important] Pending review — metrics trimmed to the store
> Operation latency, routing, open-state, recall, grace and quota rows moved to
> the RFC that owns each event.

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
which the embedded store does not. The benchmark is kept outside the
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
| 16 shards, read and rewritten | 90k (0.66) | 44k (3.1) |
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
| Counter batching | JuiceFS batches quota deltas per client | a hot counter key is avoided, not sharded |
| Logical-bytes charging | VAST, OneFS, ONTAP, NTFS | a principal's usage never depends on deduplication |
| Credentials in the config store | Samba `tdbsam`, TrueNAS, ONTAP | kept beside users, sealed |
