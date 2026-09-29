---
rfc: 17
title: "RFC 17 — the filesystem service (VFS)"
component: vfs
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-14-open-state]]"
  - "[[rfc-15-topology]]"
  - "[[rfc-16-metadata-store]]"
aliases:
  - RFC 17
  - VFS
tags:
  - rfc
---
# RFC 17 — the filesystem service (VFS)

**Status:** draft.
**Audience:** anyone writing or changing a protocol adapter, and anyone changing
the order in which a client operation touches metadata, open state and content.

Conventions, RFC 2119 keywords and test tiers are set once in the
[index](rfc-index.md). This document specifies behaviour, not the current code.

---

## In short

- Adapters speak wire. They call one protocol-neutral service, `vfs.Service`,
  and nothing else below them.
- The service is the only caller of the metadata store ([RFC 16](rfc-16-metadata-store.md)), open state
  ([RFC 14](rfc-14-open-state.md)) and the content engine ([RFC 8](rfc-8-engine.md)), and the only code that orders an
  operation across them.
- Its operation set is the union of what the protocols need, not their
  intersection. Translation, framing and replay stay in the adapters.
- It hides the topology: whether an owner is this process or another node
  ([RFC 15](rfc-15-topology.md)) never reaches an adapter.

---

## 1. Purpose

Every client operation touches several components: a write is authorised by the
namespace, checked against open state, charged to a quota, staged in the journal
and later recorded as existence. Something has to put those steps in order. This
document names that something and fixes where it stands:

```
   NFS adapter      SMB adapter      pNFS endpoints       wire only
        \                |                /
   ┌───────────────── vfs.Service ─────────────────┐
   │ open, close, read, write, commit, lookup,     │     the one API
   │ readdir, getattr, setattr, create, link,      │     adapters see
   │ remove, rename, lock, grants, watches, statfs │
   └───┬────────────┬─────────────┬────────────┬───┘
   metadata      content        open state    routing
   (RFC 16)      (RFC 8)        (RFC 14)      (RFC 15)
```

[RFC 0 §1.3](rfc-0-data-lifecycle.md#1.3%20The%20layers) draws the same picture with the roles marked.

One service, rather than each adapter composing the parts itself, buys four
things:

1. **Orchestration is written once.** Written in each adapter, the orders drift,
   and a check one adapter has and the other lacks is how a client reaches data
   nobody audited.
2. **Cross-protocol rules have one home.** An NFS lock against an SMB deny mode,
   or an NFS write against an SMB lease, is decided where both are visible.
3. **The topology is invisible to adapters.** A call whose owner is another node
   is forwarded by the service; an adapter never learns there was a hop.
4. **It is testable without a protocol.** The service has its own conformance
   suite (§8), and the protocol suites then test translation only.

### 1.1 Non-goals

The service **MUST NOT**:

- encode or decode a wire format, run a compound, keep a replay cache or build
  the NFSv4 pseudo-filesystem — those are the adapters' ([§4.2](#4.2%20Translation%20stays%20in%20adapters));
- evaluate a permission itself — it asks the one chokepoint ([§4.6](#4.6%20One%20chokepoint));
- hold a copy of metadata, open state or content that outlives the call that
  read it;
- persist anything of its own. Every durable fact is recorded by the component
  that owns it;
- import an adapter. Calls upward go through interfaces adapters implement
  ([§3.2](#3.2%20Callbacks)).

## 2. Name

`Filesystem` is taken three ways over: the metadata store's `FilesystemInfo`,
Go's `io/fs.FS`, and the product's own description. The word for this layer
across the field is **VFS** — the protocol-neutral file API between front-ends
and storage. The package is `vfs`, the interface `vfs.Service`, and an adapter
holds one value of it. Call sites read `svc.Open(…)`, never `vfs.VFS`.

## 3. Interface

Signatures are indicative; the obligations are normative. `Service` embeds the
groups, so an adapter holds one value and a test can fake one group.

### 3.1 Operations

```go
package vfs

type Service interface {
	Clients
	Names
	Attributes
	Data
	Locking
	Volumes
}

// Clients: who is talking to us, and how to call them back (RFC 14).
type Clients interface {
	Connect(ctx context.Context, c ClientInfo, cb Callbacks) (metadata.ClientID, error)
	Renew(ctx context.Context, c metadata.ClientID) error
	Disconnect(ctx context.Context, c metadata.ClientID) error
}

// Names: the namespace, by handle.
type Names interface {
	Root(ctx context.Context, share metadata.ShareID) (Handle, error)
	Lookup(ctx context.Context, id Identity, dir Handle, name []byte) (Handle, metadata.File, error)
	ReadDir(ctx context.Context, id Identity, dir Handle, after Cursor, plus bool) iter.Seq2[DirEntry, error]
	Create(ctx context.Context, id Identity, dir Handle, name []byte, k CreateKind, a metadata.Attrs) (Handle, metadata.File, error) // mkdir, symlink, mknod, NFSv3 CREATE
	Link(ctx context.Context, id Identity, dir Handle, name []byte, target Handle) error
	Remove(ctx context.Context, id Identity, dir Handle, name []byte, dirOnly bool) error
	Rename(ctx context.Context, id Identity, from Handle, fromName []byte, to Handle, toName []byte, noReplace bool) error
}

// Attributes, ACLs, xattrs, streams.
type Attributes interface {
	GetAttr(ctx context.Context, id Identity, h Handle) (metadata.File, error)
	SetAttr(ctx context.Context, id Identity, h Handle, a metadata.Attrs, via *OpenRef) (metadata.File, error)
	GetACL(ctx context.Context, id Identity, h Handle) (metadata.ACL, error)
	SetACL(ctx context.Context, id Identity, h Handle, acl metadata.ACL) (metadata.File, error)
	Xattrs(ctx context.Context, id Identity, h Handle) iter.Seq2[metadata.Xattr, error]
	SetXattr(ctx context.Context, id Identity, h Handle, x metadata.Xattr, mode XattrMode) error
	RemoveXattr(ctx context.Context, id Identity, h Handle, name []byte) error
	Streams(ctx context.Context, id Identity, h Handle) iter.Seq2[metadata.File, error]
	Access(ctx context.Context, id Identity, h Handle, want Access) (Access, error) // NFS ACCESS, SMB MaximalAccess
}

// Data: opens, reads, writes. Every data call names an OpenRef: an open, or
// an anonymous one for NFSv3, which has no opens.
type Data interface {
	Open(ctx context.Context, id Identity, req OpenRequest) (OpenResult, error) // disposition, access, deny, grant wanted, durability
	Close(ctx context.Context, o metadata.OpenID) error
	Read(ctx context.Context, o OpenRef, off int64, dst []byte) (n int, eof bool, err error)
	Write(ctx context.Context, o OpenRef, off int64, src []byte, stable Stability) (n int, v WriteVerifier, err error)
	Commit(ctx context.Context, o OpenRef, off, length int64) (WriteVerifier, error)
	Allocate(ctx context.Context, o OpenRef, off, length int64) error
	Deallocate(ctx context.Context, o OpenRef, off, length int64) error // punch a hole
	Seek(ctx context.Context, o OpenRef, off int64, what SeekWhat) (int64, error) // SEEK_DATA, SEEK_HOLE
	Copy(ctx context.Context, src OpenRef, srcOff int64, dst OpenRef, dstOff, length int64) (int64, error) // server-side copy and clone
}

// Locking: byte-range locks, caching grants, watches (RFC 14).
type Locking interface {
	Lock(ctx context.Context, o OpenRef, r metadata.ByteRange, exclusive, wait, reclaim bool) error
	TestLock(ctx context.Context, o OpenRef, r metadata.ByteRange, exclusive bool) (*metadata.Lock, error)
	Unlock(ctx context.Context, o OpenRef, r metadata.ByteRange) error
	ReturnGrant(ctx context.Context, g metadata.GrantID) error
	AckBreak(ctx context.Context, g metadata.GrantID, to GrantKind) error
	Watch(ctx context.Context, id Identity, dir Handle, recursive bool, filter metadata.ChangeMask) (metadata.WatchID, error)
	Unwatch(ctx context.Context, w metadata.WatchID) error
}

// Volumes: what statfs, FSSTAT and SMB volume queries ask.
type Volumes interface {
	StatFS(ctx context.Context, id Identity, h Handle) (metadata.FilesystemInfo, error)
	Quota(ctx context.Context, id Identity, h Handle, p metadata.Principal) (metadata.Usage, metadata.Quota, error)
}
```

### 3.2 Callbacks

```go
// Callbacks is implemented by each adapter and registered per client at
// Connect. The service calls up through it and never imports an adapter.
type Callbacks interface {
	RecallGrant(ctx context.Context, g metadata.CachingGrant, to GrantKind) error // NFS CB_RECALL, SMB lease or oplock break
	Notify(ctx context.Context, w metadata.WatchID, changes []Change) error      // SMB CHANGE_NOTIFY, NFS directory notifications
	Revoked(ctx context.Context, what Revocation)                                // lease expiry, administrative revoke
}
```

A callback **MUST NOT** block the service beyond its context's deadline, and a
callback that fails or times out **MUST** be treated as the client not answering:
a recall that is not acknowledged by its deadline revokes the grant
([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)). The conflicting operation that caused the recall waits on the
recall, not on the callback's return.

> [!important] Pending review — the service and its callbacks
> New RFC, folded from the metadata-model review draft. The interface groups,
> the callback direction and the name `vfs` are that draft's, agreed in review.

## 4. Rules

### 4.1 The operation set is the union, not the intersection

`Open` takes a create disposition, requested access, a deny mode, a wanted
caching grant and a durability, because SMB needs create-open-deny as one
atomic step and NFSv4's `OPEN` has the same shape. NFSv3 has no opens: its reads
and writes run against an **anonymous open**, which carries the identity and
no state. An operation one protocol needs and the other lacks — `Copy`, `Seek`,
`Watch`, streams — is in the set; an adapter whose protocol lacks it simply does
not call it.

A protocol's semantics **MUST NOT** be approximated by composing weaker service
calls in the adapter. If SMB's atomic create-with-deny were built from a lookup,
a create and an open, two clients could interleave between them. The service
offers the atomic form, or the operation is not supported.

### 4.2 Translation stays in adapters

Adapters own, and the service never sees:

- wire encoding and framing (XDR, SMB2), NFSv4 `COMPOUND` and SMB compounding,
  which run as sequences of service calls;
- stateid and FileId encodings, sequence and replay caches;
- the NFSv4 pseudo-filesystem, built from the share list;
- security descriptors ↔ `metadata.ACL`, applying RFC 19's mapping;
- mapping the service's errors to protocol status codes;
- a protocol's own numeric file IDs, derived from `FileID`
  ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20derived%2C%20and%20collisions%20are%20its%20problem)).

### 4.3 Errors are neutral values

The service returns `ErrNotFound`, `ErrExist`, `ErrAccess`, `ErrStale`,
`ErrNotEmpty`, `ErrNotDir`, `ErrIsDir`, `ErrNoSpace`, `ErrQuota`, `ErrLocked`,
`ErrShareViolation`, `ErrGrace`, `ErrDelay`, and the content errors of
[RFC 8 §9.1](rfc-8-engine.md#9.1%20One%20content%20facade%2C%20called%20by%20the%20filesystem%20service) (`ErrLost`, `ErrUnavailable`, `ErrCorrupt`). Each adapter maps them
once. A routing refusal — wrong owner, stale epoch — **MUST NOT** reach an
adapter: the service re-routes and retries within the caller's deadline ([RFC 15 §6](rfc-15-topology.md#6.%20Learning%20owners)).

### 4.4 Handles are opaque

The service mints handles through the metadata store
([RFC 7 §6](rfc-7-namespace-metadata.md#6.%20Handles)). An adapter stores and returns them, and **MUST NOT** build,
parse or compare their contents.

### 4.5 No copies on the data path

`Read` fills the caller's buffer and `Write` takes the caller's. The service
passes them to the engine, which lends or streams below
([RFC 8 §9.1](rfc-8-engine.md#9.1%20One%20content%20facade%2C%20called%20by%20the%20filesystem%20service)); the service **MUST NOT** add a copy of its own. Buffer budgets
are RFC 24's.

### 4.6 One chokepoint

Authorisation stays in the metadata store's `Files.Authorize`
([RFC 7 §7.1](rfc-7-namespace-metadata.md#7.1%20One%20chokepoint)). The service calls it on every operation that needs it, and is
its only caller. An open-time grant ([RFC 7 §7.2](rfc-7-namespace-metadata.md#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always)) is computed there and stored
with the open; the service evaluates it on the operations it gates.

### 4.7 Callable across the network

Under a split deployment ([RFC 15 §4.1](rfc-15-topology.md#4.1%20Every%20call%20is%20safe%20to%20route)) a call may reach a node that neither owns
the file's namespace nor its data. Every operation **MUST** therefore take and
return values, resume an iteration from a cursor the caller holds, and be safe
to retry: a write repeated with the same arguments leaves the same content.

## 5. Orchestration

The service is the only code that orders a client operation across components.
Each order below is normative.

### 5.1 Write

1. resolve the handle; refuse a stale one;
2. authorise against the file ([§4.6](#4.6%20One%20chokepoint)), or evaluate the open's stored grant;
3. check open state: the caller's open, deny modes and byte-range locks held by
   others ([RFC 14 §7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols)); break conflicting caching grants and wait for the break;
4. check the quota against the charge the write would add, if it extends the
   file;
5. call the data owner's engine `Write`, which stages the bytes in the journal
   ([RFC 8 §4.1](rfc-8-engine.md#4.1%20The%20facade%20orders%20a%20write%3B%20adapters%20do%20not));
6. reply. Existence, usage and `mtime` are recorded at the next stability point
   by the engine, not here.

### 5.2 Read

Steps 1–3 as for a write, with read access; then the engine's `Read`, which
resolves residency ([RFC 8 §6](rfc-8-engine.md#6.%20The%20read)).

### 5.3 Open and close

`Open` resolves or creates the name, authorises, checks deny modes against the
opens already held, offers a caching grant if no other client conflicts, and
records the open — all at the namespace owner, in one step as the protocol
sees it. `Close` drops the open; the last close of an unlinked file releases it
through the engine ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)).

### 5.4 Remove and rename

The metadata store applies the namespace change and, when the last entry and
last open are gone, the pending release; the service then asks the data owner's
engine to `Release`. A rename that replaces a target applies the same rule to
the target.

### 5.5 Size changes

A `SetAttr` that changes size is authorised and checked like a write, then
applied by the engine's `Truncate` on the data owner; the other attributes in
the same call are applied by the metadata store in the same service call.

> [!important] Pending review — orchestration orders
> RFC 8 §4.1's step 1 (authorise) moved here; the engine now stages and
> commits content only. The quota check (step 4) is new with RFC 16's usage
> counters.

## 6. Invariants

| # | Invariant |
| --- | --- |
| V1 | Adapters hold only `vfs.Service`. No adapter holds a metadata view, an engine, open state or a component. |
| V2 | Every client operation that needs authorisation reaches `Files.Authorize` exactly through the service. |
| V3 | Every cross-component order of a client operation is the one in §5, in every protocol. |
| V4 | A routing refusal never reaches an adapter. |
| V5 | The service persists nothing and imports no adapter. |
| V6 | A recall or break that is not acknowledged by its deadline revokes the grant; nothing waits on an unanswering client beyond it. |

## 7. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| How fast is each operation, per protocol? | `dittofs_vfs_op_seconds{op, protocol, outcome}` | histogram |
| How often do calls leave this node? | `dittofs_vfs_forwarded_total{op, role}` | counter |
| Are recalls finishing? | `dittofs_vfs_recall_seconds`, `dittofs_vfs_recalls_revoked_total` | histogram, counter |
| Are callbacks failing? | `dittofs_vfs_callback_errors_total{kind}` | counter |

Each service call opens a trace span that parents the metadata store's and the
engine's spans. No share label on per-operation metrics ([RFC 16 §8.1](rfc-16-metadata-store.md#8.1%20Metrics)).

## 8. Conformance

- **Service suite:** drives `vfs.Service` directly with a fake `Callbacks` that
  records what adapters would have been told. It covers every operation's
  order (§5), and the cross-protocol cases: an NFS lock against an SMB deny
  mode, an NFS write against an SMB lease, a recall that times out, a client
  whose lease expires holding state of both kinds.
- **Import test:** no adapter package imports the metadata store, open state or
  the engine; the service imports no adapter (V1, V5).
- **Split run:** the service suite passes unchanged with every view below it
  remote ([RFC 15](rfc-15-topology.md)).
- **Benchmarks:** each operation through the service, collocated and split, so a
  protocol benchmark minus a service benchmark is the adapter's own cost.

## 9. Open questions

1. **Compound batching.** Adapters run compounds as sequences of calls. Whether
   a batch call (a `LOOKUP`-`GETATTR`-`OPEN` chain in one service call) pays for
   itself under a split deployment is unmeasured.
2. **Where the pNFS layout operations live.** `LAYOUTGET` and `LAYOUTCOMMIT` are
   protocol-specific but cross owners ([RFC 15](rfc-15-topology.md)); whether they are service
   operations or an adapter-side protocol over `Data` is open.
3. **Anonymous opens and deny modes.** An NFSv3 write against a file an SMB
   client opened with deny-write is refused; whether an NFSv3 read under
   deny-read is refused too is RFC 14's.

## Appendix A — prior art

*Non-normative.* The layer exists under different names in every
multi-protocol file server that kept its protocols consistent:

- **Samba's VFS layer** — every SMB operation passes through a stack of VFS
  modules before the file system; protocol code never touches the backend.
- **NFS-Ganesha's FSAL** — the protocol layers call one file-system abstraction
  layer, with a backend per storage system.
- **The Linux VFS** — system calls, the in-kernel NFS server and the SMB server
  all reach file systems through the same inode and file operations.
- **libcephfs** — one client library that NFS-Ganesha and Samba both call, so
  both protocols see one set of capabilities and locks.

The shared lesson: the protocol layer translates, and one layer beneath it
orders and enforces.
