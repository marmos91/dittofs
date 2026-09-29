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
| `ChunkRef`, `Chunk`, `Block`, `ChunkHash`, `BlockName`, `JournalVersion` | the content map and the blocks it lives in | [RFC 6](rfc-6-block-metadata.md) |
| `FileID`, `ShareID`, `Principal` | identity | §2.2 |
| `User`, `Group`, `Membership` | principals with names | §2.3 |
| `Share`, `ShareGrant`, `Snapshot`, `SnapshotCut` | a share, who may mount it, its snapshots | §2.3 |
| `Node`, `OwnershipUnit` | cluster membership and ownership ([RFC 11](rfc-11-ownership.md)) | §2.3 |
| `Setting`, `Secret` | configuration values and sealed credentials ([RFC 13](rfc-13-configuration.md)) | §2.3 |

Holes, removals, fences, cuts, put intents, pending deletions, candidates and
the chunk change stamp are [RFC 6](rfc-6-block-metadata.md)'s bookkeeping for its guarantees. They are
records, not entities: they stay behind the interfaces of §3.

### 2.2 Identity types

```go
```go
type (
	FileID  [16]byte // RFC 0 §3: UUID, never reused, stable across rename and relink
	ShareID [16]byte
)

// Principal is a protocol-neutral identity: a user or a group. The adapter
// maps UIDs, GIDs and SIDs onto principals (RFC 18); this layer compares them
// and never interprets their spelling.
type Principal struct {
	Kind PrincipalKind // User, Group, Special (owner, group, everyone)
	ID   string        // canonical form, e.g. "u:1000", "S-1-5-21-…"
}
```
```

Every other identity type sits beside the only entities that use it: the
content identities (`ChunkHash`, `BlockName`, `JournalVersion`) with the
content entities in [RFC 6](rfc-6-block-metadata.md), and `SnapshotCut` with `Snapshot` (§2.3).

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

// ShareGrant is one principal's access to one share, checked at mount and
// tree connect before any file ACL is read.
type ShareGrant struct {
	Share     ShareID
	Principal Principal
	Access    ShareAccess // none, read, read-write, admin
}

// SnapshotCut numbers a share's snapshots in the order they were taken. A
// ref records the cut it was born after and the cut it died after
// (ChunkRef.Born, Died), which is how a snapshot, a backup or an export
// decides which content it sees (RFC 12, RFC 6 §6.5).
type SnapshotCut uint64

// Snapshot is a named, user-visible cut of a share.
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
	Roles    Roles // protocol, metadata, block
	LastSeen time.Time
}

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
type Secret struct {
	ID    SecretRef
	Kind  SecretKind // password (slow hash), nt-hash (only with NTLM enabled), key
	KeyID string     // which wrapping key sealed it
	Value []byte     // sealed
}
```

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
func (p Principal) String() string              // "user:u:1000", "group:S-1-5-32-544"

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
func (u Usage) Remaining(q Quota) Usage

func (s Snapshot) Expired(now time.Time) bool
```

Entity methods defined elsewhere:

| Entity | Methods | Where |
| --- | --- | --- |
| `File`, `FileType` | `IsDir`, `IsRegular`, `IsSymlink`, `IsStream`, `IsSpecial`, `IsRoot`, `HasContent`, `HasFlag`, `FSMode`, `Validate`, `Apply`, `Diff`, `Clone` | [RFC 7](rfc-7-namespace-metadata.md) |
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
| Namespace implementation | `Existence`'s read side: `Size`, `Times`, `Allocation` |
| Engine | `Existence`, `Content` |
| GC | `Blocks` |
| Authentication, tree connect, mount | `Principals` |
| Management API | `ControlPlane` |
| Ownership (RFC 11) | `Node` and `OwnershipUnit` records through its own view, fenced by epoch |
| Debug tooling | `Dump` (§4.5) |

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

The composition root (RFC 8) calls `Open` and hands each consumer only the
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
	// whether or not the transaction writes it. A gating read uses it
	// (RFC 6 §5.4).
	Guard(key []byte) error
}
```

Two backends with identical semantics differ only here, and one conformance
suite over `KV` plus one over the entity layer covers both.

### 4.2 Keys: per-file, per-share, content-addressed

Every key starts with a kind byte. The layout encodes the boundary
[RFC 6 §1.2](rfc-6-block-metadata.md#1.2%20Why%20it%20is%20a%20separate%20RFC%20from%20the%20namespace) draws between what belongs to one file and what is shared:

| Scope | Key | Entity or record |
| --- | --- | --- |
| **Per file** — all under `F‖ShareID‖FileID`, written `F‖id` below | `F‖id` | File: every field, one value |
| | `F‖id‖acl` | ACL |
| | `F‖id‖x‖name` | Xattr |
| | `F‖id‖s‖streamID` | named stream link |
| | `F‖id‖e‖name` | Entry, under its **parent** directory |
| | `F‖id‖t‖unique` | directory time delta (§4.4) |
| | `F‖id‖h‖start`, `F‖id‖rm‖version` | Hole, Removal |
| | `F‖id‖r‖offset`, `F‖id‖hist‖died‖offset` | ChunkRef (live), ChunkRef (history) |
| | `F‖id‖fx`, `F‖id‖fo` | fences |
| | `F‖id‖rel` | pending release |
| | `F‖id‖o‖openID` | durable Open (only the cases RFC 14 makes durable: keeps an unlinked file alive, SMB persistent handle) |
| | `F‖id‖n‖parent‖name` | reverse name index — **not written today**; reserved ([RFC 7](rfc-7-namespace-metadata.md)) |
| **Per share** — under `S‖ShareID` | `S‖id‖info` | Share |
| | `S‖id‖g‖principal` | ShareGrant |
| | `S‖id‖snap‖cut` | Snapshot |
| | `S‖id‖cut`, `S‖id‖live‖k` | Cut, LiveCut |
| | `S‖id‖u`, `S‖id‖pu‖principal`, `S‖id‖pj‖project` | folded usage: share, principal, project (§4.4) |
| | `S‖id‖ud‖unique` | usage delta, not yet folded (§4.4) |
| | `S‖id‖q‖principal-or-project` | Quota |
| | `S‖id‖snap‖cut‖…` | snapshot capture of namespace, FileData and hole records (RFC 12 §2.4) |
| **Content-addressed** | `C‖hash`, `B‖name`, `I‖name`, `D‖name`, `K‖name` | Chunk, Block, put intent, pending deletion, candidate |
| **Server-wide** | `U‖principal`, `G‖principal` | User, Group |
| | `M‖group‖member`, `MR‖member‖group` | Membership, both directions |
| | `NX‖kind‖name` | name index: user, group and share names → ID, unique |
| | `PX‖scheme‖id` | protocol-ID index: UID, GID, SID → principal |
| | `N‖node`, `UT‖unit` | Node, OwnershipUnit |
| | `CFG‖scope‖key` | Setting |
| | `NS‖namespace‖gc‖…` | sweep, audit and relocation cursors, per namespace because GC runs once per namespace across its shares ([RFC 9](rfc-9-gc.md)), so a restarted pass resumes |
| | `SEC‖id` | Secret (§2.3): envelope-encrypted, never dumped or exported |
| | `CL‖clientID` | durable Client record: only what reclaim needs ([RFC 14](rfc-14-open-state.md)) |
| **Store** | `\x00format` | store format record (§4.6) |

The table is **exhaustive**: every key the store writes has a row. A new
record kind is a format bump (§4.6) and gets its row in the same change;
volatile open state (locks, caching grants, watches, non-durable opens) is
never written to the KV and so has none.

Four consequences:

- `Files.Get` is one read of one key. The File was two keys (attributes, and
  the write path's fields) so that `chmod` and a write never conflicted; the
  second key cost 25% on `GETATTR`, 60% on `READDIRPLUS` and 30% on serial
  creates (Appendix A), to avoid a conflict that is rare and cheap to retry.
  Split it again only if a profile shows those conflicts.
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
  server-wide is copied wholesale into another installation.

### 4.3 Codecs

Each persisted type has one codec: a version byte followed by a fixed field
order. Decoding refuses an unknown version rather than guessing. A codec never
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
a new key, unique to the transaction (`S‖id‖ud‖unique`), holding the changes to
the share's, the owner's, the group's and the project's usage. It reads
nothing, so it conflicts with nothing: measured, it ran at the rate of no
counter at all. The share's owner **folds** deltas into the totals in batches,
one transaction per batch, deleting the deltas it folded. A reader — `statfs`,
a quota check — reads the totals plus the deltas not yet folded; the fold
keeps that set small.

The deltas are in the same transaction as the change, so a crash loses
nothing and needs no rebuild. Quota enforcement reads the totals plus unfolded
deltas, so it is exact up to transactions committing concurrently with the
check; the overshoot is at most those, and is stated rather than hidden. Every
system surveyed enforces with some slack (Appendix B).

**Directory times are the same problem.** Every create, unlink and rename in a
directory updates its `Modify`, `Change` and `Version`; measured, that
read-and-rewrite cut parallel creates in one directory from 111k/s to 28k/s,
whatever the file layout. Directory time changes are therefore delta records
under the directory (`F‖id‖t‖unique`) folded the same way, and a directory's
`Get` applies any unfolded ones — usually none.

Charged bytes follow the existence commit, not the offload: a file is charged
for its size when its size is recorded, and never charged twice for a chunk
another file shares. Deduplication savings are the share's, not a principal's.

> ponytail: one folder per share, run by the share's owner. Fold throughput
> caps the sustained rate of charge-changing transactions per share; shard the
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

One record, `\x00format`, holds the store's format version and the version of
every codec it contains. Opening a store **MUST** read it first and refuse a
format newer than the binary supports, before reading any other key. A new
record kind or a codec version is a format bump; a newer binary opens an older
store and upgrades it by writing, an older binary refuses a newer one. This is
the same rule [RFC 1 §4.3](rfc-1-journal.md) states for the journal.

## 5. Decisions and evidence

Each question was put to prior art and, where the embedded store could answer
it, to a benchmark (Appendix A). Sources are linked once per system.

| # | Question | Answer | Evidence |
| --- | --- | --- | --- |
| 1 | Drop `generation`? | **Yes**, with share-scoped keys (§4.2). A restore into a new share gets new keys and cannot alias an old handle; an in-place rollback revives the same files, whose old handles rightly work again — ZFS behaves the same. | JuiceFS and 3FS use never-reused IDs with no generation; [ZFS handles after rollback](https://github.com/openzfs/zfs/issues/9587) |
| 2 | `Mode` beside the ACL? | **Keep both.** `Mode` lives on the File so `GETATTR` never reads the ACL; `chmod` merges into the owner, group and everyone entries and leaves inheritable entries alone; a file with no ACL gets one synthesised on read. Reject "last writer wins". | [RFC 8881 §6.4](https://www.rfc-editor.org/rfc/rfc8881.html#section-6.4) requires them to agree; OneFS "Balanced", [Qumulo XPP](https://docs.qumulo.com/administrator-guide/authorization-qumulo-core/managing-cross-protocol-permissions-xpp.html); ONTAP mixed style is the cautionary case |
| 3 | Shard usage counters? | **No — delta records and a fold** (§4.4). Shards spread collisions; deltas remove them. | Appendix A; [JuiceFS quota design](https://juicefs.com/en/blog/engineering/quota-design-in-distributed-architecture) batches deltas too |
| 4 | Open state durability? | **One rule per entity:** `Client` durable (only what RFC 8881 §8.4.3 requires for reclaim); `Open` volatile and reclaimed in grace, durable when it keeps an unlinked file alive or is an SMB persistent handle; `Lock` volatile unless its open is persistent; `CachingGrant` and `Watch` volatile, never reclaimed. Grace may run per ownership unit. | knfsd `nfsdcld` and Ganesha `rados_cluster` store client records only; CephFS rebuilds caps on reconnect; SMB persistent handles are the one durable case |
| 5 | Stream as File? | **Yes for content, NTFS for identity:** a stream has no owner or ACL of its own, reports its base file's ID to SMB, and is released with it. | NTFS streams are attributes of one file record; ZFS named attributes and Samba `streams_depot` are files |
| 6 | Share in per-file keys? | **Yes**: `F‖ShareID‖FileID` (§4.2). | JuiceFS volume prefix; TiKV range deletion drops a share in seconds, not hours |
| 7 | Nested groups? | **Walk with a depth cap and cache.** Kerberos and AD already deliver the flattened list in the ticket, so no walk for them. Local groups are walked at login, depth ≤ 8 with cycle detection, cached under a key naming every membership it read (RFC 7 §7.5). No stored closure table. | Kerberos PAC; SSSD nesting level; Zanzibar's flattened index shows the closure's write cost |
| 8 | Deleted principals? | Deleting a user removes the User, its memberships and grants; the principal stays in ownership, ACLs and usage. An admin **reassign** job moves files and their charge through `chown`. A deleted principal's ID is never reused. | ONTAP shows raw SIDs in quota reports; NTFS refuses to drop a quota entry while the SID owns files; MS-SMB2 returns zeroed entries for unknown SIDs |
| 9 | Where credentials live? | **In the KV, as `Secret` records** (§2.3), envelope-encrypted under a key held outside it (a file or a KMS), never dumped or exported. Password hashes use a slow hash; an NT hash exists only when NTLM is enabled. Keytabs stay files, referenced by path. | Samba `tdbsam`, TrueNAS and ONTAP all keep credentials in their replicated config store; Samba warns NT hashes are cleartext-equivalent |

Still open, for measurement on the replicated store:

- minting a new file's ID near its parent's, so a create lands on one region;
- the fold's sustained throughput per share (§4.4's ponytail);
- whether `EntriesPlus` batching closes enough of the `READDIRPLUS` gap.

## 6. Testing

The conformance sections of RFC 6, RFC 7 and RFC 14 run these suites; test
tiers are the [index](rfc-index.md)'s.

### 6.1 Three suites, layered like the code

| Suite | Runs against | Proves |
| --- | --- | --- |
| **KV conformance** | every `KV` implementation (embedded, replicated, and an in-memory fake used only by this suite's own tests) | `Update` is atomic and retried; `Guard` makes a read conflict ([RFC 6 §5.4](rfc-6-block-metadata.md#5.4%20Reads%20that%20gate%20a%20commit)); `Scan` sees a transaction's own writes and nothing uncommitted; `Limits` is honest (a transaction at the limit commits, one past it is refused) |
| **Entity conformance** | the store over each real `KV` | every rule in §1.2 and every invariant of RFC 6 §9 and RFC 7 §10, through the §3 interfaces only |
| **Routing conformance** | the store collocated, and split with views over the network ([RFC 15](rfc-15-topology.md)) | the same entity suite passes unchanged when every view is remote, and a call to a former owner is refused by epoch |

The entity suite never reads a key. A test that does is testing the layout,
not the behaviour, and breaks on every layout change for no reason; layout has
its own golden tests (§6.3).

### 6.2 Model-based and property tests

A reference model — plain Go maps holding files, entries, refs and counts, with
no KV — runs the same random operation sequences as the store: create, link,
unlink, rename, write, truncate, clone, snapshot, release, chown, set-ACL.
After every step the store and the model **MUST** agree on every entity reached
through `Inspect`, and after every sequence:

- every file's `Nlink` equals the entries naming it;
- every chunk's `Refcount` equals the live and history refs naming it, found by
  a full walk;
- usage per share and per principal equals the sum over the files charged;
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
- a process kill between the phases of a batched removal (RFC 6 §6.2) or a
  release handoff ([RFC 15](rfc-15-topology.md)), resumed on restart;
- an epoch change mid-operation (the old owner's writes are refused).

On the replicated backend, a partition-and-kill harness in the Jepsen style runs
nightly, checking the §6.2 invariants and linearizable namespace operations.

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

## 8. Observability

On fold, metric names follow RFC 25's conventions; until then they use
`dittofs_metadata_`.

### 8.1 Metrics

| Answers | Metric | Type |
| --- | --- | --- |
| How fast is each operation? | `dittofs_metadata_op_seconds{op, outcome}` | histogram |
| How much does each operation touch? | `dittofs_metadata_op_keys{op, kind=read\|written}` | histogram |
| Is contention costing us? | `dittofs_metadata_txn_retries_total{op}`, `dittofs_metadata_txn_conflicts_total{area}` | counters |
| Is the KV the bottleneck? | `dittofs_metadata_kv_seconds{call}` | histogram |
| Are transactions near the backend's limits? | `dittofs_metadata_txn_size{kind=entries\|bytes}` against `…_txn_limit` | histogram, gauge |
| Is the fold keeping up? | `dittofs_metadata_unfolded_deltas{kind=usage\|dirtime}`, `dittofs_metadata_fold_seconds` | gauge, histogram |
| Is routing healthy? | `dittofs_metadata_forwarded_total{view, role}`, `dittofs_metadata_epoch_refusals_total{owner}` | counters |
| Is open state piling up? | `dittofs_metadata_open_state{kind=client\|open\|lock\|grant\|watch}` | gauges |
| Are recalls finishing? | `dittofs_metadata_recall_seconds`, `dittofs_metadata_recalls_revoked_total` | histogram, counter |
| Are we in grace? | `dittofs_metadata_grace_active` | gauge |
| Quotas | `dittofs_metadata_quota_refusals_total`, `dittofs_metadata_quota_overshoot_bytes` | counter, gauge |
| Store | `dittofs_metadata_format_version`, `dittofs_metadata_codec_errors_total{kind}` | gauge, counter |

**No share label on per-operation metrics.** At 10⁴ shares, a share label on a
histogram multiplies its series by 10⁴. Per-share figures — usage, quota,
open-state counts — are gauges sampled from the counters that already exist,
exported for the top shares by a stated rule, or read through the management
API. Principal labels are never used.

### 8.2 Logs and events

Expected errors (`ErrNoEntity`, `ErrExist`, `ErrAccess`, stale handle) are
`Debug`; a codec refusal, an invariant violation found by audit, an epoch
refusal on a node that believed it was owner, and a revoked recall are `Error`
or `Warn` with the entity ID. Each such event carries the operation's trace ID.

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
