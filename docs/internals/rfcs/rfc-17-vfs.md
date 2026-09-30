---
rfc: 17
title: "RFC 17 — the filesystem service (VFS)"
component: vfs
status: draft
depends_on:
  - "[[rfc-0-data-lifecycle]]"
  - "[[rfc-7-namespace-metadata]]"
  - "[[rfc-8-engine]]"
  - "[[rfc-11-ownership]]"
  - "[[rfc-13-configuration]]"
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
- A file has one owner, which holds its namespace, open state and content. The
  conflict check and the I/O it gates run at that owner, in one step.
- It is the one enforcer of quotas, the one emitter of quota and access-audit
  events, and the one place operation latency is measured.
- It never blocks a worker on a client: a conflict that needs a recall returns
  `ErrDelay`, and the client retries.

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
	ReclaimComplete(ctx context.Context, c metadata.ClientID) error // RECLAIM_COMPLETE; may end grace early (RFC 14)

}

// Names: the namespace, by handle.
type Names interface {
	Root(ctx context.Context, id Identity, share metadata.ShareID) (Handle, error) // authorised against the share grant
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

// Data: opens, reads, writes. Every data call names an OpenRef: an open with
// the ClientID that holds it, or an anonymous one for NFSv3, which has no
// opens and carries the identity instead.
type Data interface {
	Open(ctx context.Context, id Identity, req OpenRequest) (OpenResult, error) // disposition, access, deny, grant wanted, durability; req names the ClientID
	Close(ctx context.Context, c metadata.ClientID, o metadata.OpenID) error
		Read(ctx context.Context, o OpenRef, off int64, dst []byte) (n int, eof bool, err error) // n verified bytes may come with err (§5.2)
	Write(ctx context.Context, o OpenRef, off int64, src []byte, stable Stability) (n int, v WriteVerifier, err error)
	Commit(ctx context.Context, o OpenRef, off, length int64) (WriteVerifier, error)
	Allocate(ctx context.Context, o OpenRef, off, length int64) error
	Deallocate(ctx context.Context, o OpenRef, off, length int64) error // punch a hole
	Seek(ctx context.Context, o OpenRef, off int64, what SeekWhat) (int64, error) // SEEK_DATA, SEEK_HOLE
		Copy(ctx context.Context, src OpenRef, srcOff int64, dst OpenRef, dstOff, length int64) (int64, error) // server-side copy and clone
	PreWarm(ctx context.Context, id Identity, dir Handle, recursive bool) (Progress, error) // the service enumerates the files it may read, the engine fetches them
}

// Locking: byte-range locks, caching grants, watches (RFC 14).
type Locking interface {
	Lock(ctx context.Context, o OpenRef, r metadata.ByteRange, exclusive, wait, reclaim bool) error
	TestLock(ctx context.Context, o OpenRef, r metadata.ByteRange, exclusive bool) (*metadata.Lock, error)
	Unlock(ctx context.Context, o OpenRef, r metadata.ByteRange) error
	ReturnGrant(ctx context.Context, c metadata.ClientID, g metadata.GrantID) error
	AckBreak(ctx context.Context, c metadata.ClientID, g metadata.GrantID, to GrantKind) error
	Watch(ctx context.Context, c metadata.ClientID, id Identity, dir Handle, recursive bool, filter metadata.ChangeMask) (metadata.WatchID, error)
		Unwatch(ctx context.Context, c metadata.ClientID, w metadata.WatchID) error
	LayoutGet(ctx context.Context, o OpenRef, r metadata.ByteRange, write, reclaim bool) (metadata.Layout, error) // pNFS
	LayoutCommit(ctx context.Context, c metadata.ClientID, l metadata.LayoutID, end int64, mtime time.Time) error // ErrBadLayout on a stale epoch
	LayoutReturn(ctx context.Context, c metadata.ClientID, l metadata.LayoutID) error
}

// Volumes: what statfs, FSSTAT and SMB volume queries ask.
type Volumes interface {
	StatFS(ctx context.Context, id Identity, h Handle) (metadata.FilesystemInfo, error)
	Quota(ctx context.Context, id Identity, h Handle, p metadata.Principal) (metadata.Usage, metadata.Quota, error)
}
```

Every open-state call names the `ClientID` it acts for, and the service
**MUST** refuse, with `ErrNotYours`, an `OpenID`, `GrantID`, `WatchID`,
`LayoutID` or lock owner that the named client does not hold. A leaked or forged ID then reaches
nothing, and a client whose lease expires releases its watches with the rest of
its state ([RFC 14 §4.3](rfc-14-open-state.md#4.3%20An%20expired%20lease%20releases%20everything%20it%20held%2C%20everywhere)).

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
([RFC 14 §5.3](rfc-14-open-state.md#5.3%20A%20recall%20MUST%20end%20within%20a%20bounded%20time)).

**No worker waits on a recall.** An operation that conflicts with a caching
grant starts the recall and returns `ErrDelay` at once; the adapter maps it to
NFS4ERR_DELAY or NFS3ERR_JUKEBOX, or to an SMB interim (pending) response, and
the client retries. A client that holds grants on many files and never
acknowledges therefore costs each conflicting operation one retry interval,
never a pinned worker. After one of a client's recalls is revoked, the service
**MUST** stop offering that client grants and revoke the others it holds
([RFC 14](rfc-14-open-state.md)). For SMB, whose writes name a FileId rather
than a lease, revoking a lease **MUST** invalidate the opens it covered, so a
stale buffered write through them is refused.

**Notifications honour traverse permission.** `Notify` delivers a change only
when the watch's identity may traverse every directory between the watched one
and the change, evaluated through the chokepoint ([§4.6](#4.6%20One%20chokepoint)) at delivery. A recursive
watch never reports names below a directory its holder cannot enter.

### 3.3 Event hooks

```go
// Events is implemented by an event sink supplied at composition. The
// service calls it and never waits on it.
type Events interface {
	Quota(ctx context.Context, e QuotaEvent)   // advisory threshold crossed, soft limit exceeded, grace expired, hard refusal
	Access(ctx context.Context, e AccessEvent) // one authorisation decision the audit policy selects
}

type AccessEvent struct {
	Time     time.Time
	Share    metadata.ShareID
	Client   metadata.ClientID // zero for NFSv3
	Identity Identity
	Protocol Protocol
	Op       Op
	File     metadata.FileID
	Name     []byte // the name acted on, for namespace operations
	Want     Access
	Allowed  bool
}
```

Both streams are emitted from the one path every operation takes: access events
where the service consults the chokepoint ([§4.6](#4.6%20One%20chokepoint)), quota events where it
enforces the quota ([§5.6](#5.6%20Quota)). No adapter, store or engine emits either,
so no protocol can reach a file without appearing in the audit stream.

- An event hook **MUST NOT** block or fail the operation. The sink queues, and
  an event it cannot queue is dropped and counted (§7).
- Which operations produce an access event — every decision, denials only,
  writes and namespace changes, per share — is the audit policy, a setting
  ([RFC 13](rfc-13-configuration.md)). With no policy, no access event is built.
- Delivery, retention and the export format of both streams belong to the
  planned observability RFC (RFC 25). This RFC fixes only where they are
  emitted and what an event carries.

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
`ErrShareViolation`, `ErrGrace`, `ErrDelay`, `ErrNotYours`, `ErrBadLayout`, and the content errors of
[RFC 8](rfc-8-engine.md) (`ErrLost`, `ErrUnavailable`, `ErrCorrupt`). Each adapter maps them
once. `ErrDelay` means "retry shortly": a recall is in progress (§3.2).
`ErrGrace` means the file's unit is in grace and the request needs, or
conflicts with, state that may still be reclaimed (§5.1). A routing refusal — wrong owner, stale epoch — **MUST NOT** reach an
adapter: the service re-routes and retries within the caller's deadline ([RFC 15 §6](rfc-15-topology.md#6.%20Learning%20owners)).

### 4.4 Handles are opaque

The service mints handles through the metadata store
([RFC 7 §6](rfc-7-namespace-metadata.md#6.%20Handles)). An adapter stores and returns them, and **MUST NOT** build,
parse or compare their contents.

### 4.5 No copies on the data path

`Read` fills the caller's buffer and `Write` takes the caller's. The service
passes them to the engine, which lends or streams below
([RFC 8](rfc-8-engine.md)); the service **MUST NOT** add a copy of its own. Buffer budgets
are RFC 24's.

### 4.6 One chokepoint

Authorisation stays in the metadata store's `Files.Authorize`
([RFC 7 §7.1](rfc-7-namespace-metadata.md#7.1%20One%20chokepoint)). The service calls it on every operation that needs it, and is
its only caller. An open-time grant ([RFC 7 §7.2](rfc-7-namespace-metadata.md#7.2%20Two%20timings%2C%20both%20allowed%3B%20one%20owner%2C%20always)) is computed there and stored
with the open; the service evaluates it on the operations it gates.

- **The share grant is checked on every call**, not only at mount or tree
  connect. `Authorize` evaluates the caller's `ShareGrant` each time, and any
  cache of authorisation results **MUST** include the grant in its key. A
  client removed from a share loses access on its next call, cached handles or
  not.
- `Root` takes the caller's identity and is refused when no grant admits it.
  Holding a handle grants nothing: a handle names a file, and every call on it
  is authorised afresh.
- **Deny modes.** A granted open's deny mode is evaluated once, at open
  ([RFC 14 §6](rfc-14-open-state.md#6.%20A%20deny%20mode%20is%20checked%20at%20open)). An NFSv3 anonymous open has no open to evaluate
  at, so each of its reads and writes is checked against the deny modes held
  by others: a read under deny-read and a write under deny-write are refused
  with `ErrShareViolation`.

### 4.7 Callable across the network

A call may reach a node that does not own the file ([RFC 15](rfc-15-topology.md)). Every
operation **MUST** therefore take and return values and resume an iteration
from a cursor the caller holds.

A call the service forwards carries RFC 15's **route envelope**: a request ID,
unique per originating node, and the owner epoch the sender expects. The owner
refuses a call whose epoch is not its own, and keeps a short dedup table keyed
by (request ID, epoch) that returns a mutation's original result to a retry.
So a write whose reply was lost, retried after another write landed on the same
extent, returns the first write's result instead of overwriting the second.
Repeating a mutation with the same arguments is **not** assumed to be safe.

### 4.8 One owner per file

Every call on a file runs at the owner of the file's ownership unit
([RFC 11](rfc-11-ownership.md)), which holds the file's namespace records, its open state and
its content; for a file striped into range units, the owner of the unit that
holds the extent. A node with the `protocol` role only forwards
([RFC 15](rfc-15-topology.md)).

The conflict check and the I/O it gates therefore run at one owner, with no
hop between them. The owner **MUST** order them so that no lock, deny mode or
grant is granted between the check and the I/O it admits: a conflicting grant
is ordered wholly before the check, which then refuses, or wholly after the
I/O. The service does no cross-owner orchestration for a single-file
operation; the only operations that touch two owners are those RFC 15 names
(a rename across units, a `Copy` between files of two units), and it orders them.

## 5. Orchestration

The service is the only code that orders a client operation across components.
Each order below is normative.

### 5.1 Write

1. resolve the handle; refuse a stale one; route to the owner (§4.7, §4.8);
2. authorise against the file ([§4.6](#4.6%20One%20chokepoint)), or evaluate the open's stored grant;
   for an anonymous open, check deny modes held by others;
3. if the unit is in grace, refuse with `ErrGrace` a write that overlaps a lock,
   deny mode or grant that may still be reclaimed ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20ownership%20unit)); otherwise
   check open state: the caller's open and byte-range locks held by others
   ([RFC 14 §7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols)). A conflicting caching grant starts a recall and the write
   returns `ErrDelay` (§3.2);
4. reserve the charge the write adds, if it extends the file (§5.6);
5. call the engine's `Write`, which stages the bytes in the journal and returns
   the write verifier ([RFC 8](rfc-8-engine.md));
6. reply. Existence, usage and `mtime` are recorded at the next stability point
   by the engine, not here; that commit releases the reservation.

Steps 3–5 run at the owner as one step (§4.8).

### 5.2 Read

Steps 1–3 as for a write, with read access; then the engine's `Read`, which
resolves residency and streams verified chunks
([RFC 8 §7.2](rfc-8-engine.md#7.2%20The%20reply%20streams%2C%20one%20verified%20chunk%20at%20a%20time)). It never returns an unverified byte, but it can
return `n` verified bytes and then an error (`ErrLost`, `ErrUnavailable`,
`ErrCorrupt`) for the chunk after them. Each adapter answers that once:

- **NFSv3 and NFSv4** answer a short read of the `n` bytes, with `eof` false,
  when `n > 0`; the client re-reads from `off + n` and then gets the error
  (`NFS3ERR_IO` / `NFS4ERR_IO`, or `NFS4ERR_DELAY` / `NFS3ERR_JUKEBOX` for
  `ErrUnavailable`). With `n = 0` the error is the reply.
- **SMB2/3** answers a short read likewise when `n > 0` and the request's
  `MinimumCount` is met; otherwise the error, `STATUS_UNEXPECTED_IO_ERROR`, or
  `STATUS_FILE_CORRUPT_ERROR` for `ErrCorrupt`.

A reply is never padded with zeros for the bytes that failed.

### 5.3 Open and close

`Open` resolves or creates the name, authorises, checks deny modes against the
opens already held, offers a caching grant if no other client conflicts, and
records the open — all at the file's owner, in one step as the protocol
sees it. `Close` drops the open after checking that the named client holds it.
The last close of an unlinked file deletes its durable open record; the
content is then released by the release transaction that consumes the file's
pending-release record ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)). The service never calls a release itself.

### 5.4 Remove and rename

The metadata store applies the namespace change. The final unlink always writes
the file's pending-release record in the same transaction, and holders are only
the durable open records; the release transaction deletes the record once no
open remains ([RFC 7 §4.3](rfc-7-namespace-metadata.md#4.3%20Release%20is%20what%20block%20metadata%20sees)). A crash between the unlink and the release
leaks nothing, because the record, not a call from the service, drives it. A
rename that replaces a target applies the same rule to the target.

### 5.5 Size changes

A `SetAttr` that changes size is authorised and checked like a write, then
applied by the engine's `Truncate` at the file's owner; the other attributes in
the same call are applied by the metadata store in the same service call.

### 5.6 Quota

The service is the one enforcer of quotas. Neither the metadata store nor the
engine refuses a write for quota.

- At step 4 of a write, the owner adds the charge the write would add to an
  in-memory **reservation** per principal and per project, and refuses with
  `ErrQuota` when committed usage plus unfolded deltas
  ([RFC 16 §4.4](rfc-16-metadata-store.md#4.4%20Counters%20that%20many%20writers%20change)) plus its reservations would exceed the hard limit.
  The existence commit that records the charge releases the reservation; so
  does a write that fails. A reservation is volatile: after a crash the journal
  re-applies the writes and their charge ([RFC 8](rfc-8-engine.md)).
- **Overshoot bound.** An owner sees its own reservations, not other owners'.
  Each owner lets a principal or project hold at most `S` bytes reserved and
  uncommitted (the reservation slack, a setting); a write past it waits for a
  commit or returns `ErrDelay`. A principal writing through `k` owners at once
  can therefore exceed its hard limit by at most `(k − 1) × S`, plus what
  transactions committing concurrently add. With one owner, only the latter.
- **Soft limits.** A quota's soft limit and grace time
  ([RFC 16](rfc-16-metadata-store.md)) are enforced here too. Crossing the
  advisory threshold or the soft limit emits a quota event (§3.3) and refuses
  nothing; the metadata store records when the soft limit was first exceeded,
  and once the grace time has run from then the soft limit is enforced as the
  hard one. Falling back under it clears the record.

### 5.7 GetAttr

The metadata store is a leaf: it never calls the engine. `GetAttr` joins
`Files.Get` with the engine's overlay — `Size`, `Times` and `Version` of writes
staged but not yet committed — at the file's owner, where both are local. The
overlay wins where it is newer. This is the only read path for a file's
attributes; `ReadDir` with attributes does the same join per entry, batched
per owner for entries whose files live in other units.

### 5.8 The write verifier

The NFS write verifier is returned by the engine's `Write` and `Commit` and
derived from the owner's epoch and the process instance. The service passes it
through unchanged. It therefore changes whenever the file's owner changes or
restarts, which is what makes a client resend writes it sent unstable
([RFC 14 §10](rfc-14-open-state.md#10.%20Ownership)).

## 6. Invariants

| # | Invariant |
| --- | --- |
| V1 | Adapters hold only `vfs.Service`. No adapter holds a metadata view, an engine, open state or a component. |
| V2 | Every client operation that needs authorisation reaches `Files.Authorize` exactly through the service. |
| V3 | Every cross-component order of a client operation is the one in §5, in every protocol. |
| V4 | A routing refusal never reaches an adapter. |
| V5 | The service persists nothing and imports no adapter. |
| V6 | A recall or break that is not acknowledged by its deadline revokes the grant; no worker waits on a recall — the conflicting operation returns `ErrDelay`. |
| V7 | The conflict check and the I/O it admits run at one owner, and no conflicting state is granted between them. |
| V8 | Every open-state call names its `ClientID`, and an ID the client does not hold is refused. |
| V9 | The share grant is evaluated on every call. |
| V10 | Quota is enforced only by the service, and overshoot stays within the bound of §5.6. |
| V11 | A forwarded mutation retried after a lost reply returns its original result and applies once. |

## 7. Observability

| Answers | Metric | Type |
| --- | --- | --- |
| How fast is each operation, per protocol? | `dittofs_vfs_op_seconds{op, protocol, outcome}` | histogram |
| How often are clients told to retry? | `dittofs_vfs_delayed_total{op, reason}` | counter |
| How often does quota refuse, and how much is reserved? | `dittofs_vfs_quota_refusals_total{kind}`, `dittofs_vfs_quota_reserved_bytes` | counter, gauge |
| Are callbacks failing? | `dittofs_vfs_callback_errors_total{kind}` | counter |
| Are events being lost? | `dittofs_vfs_events_dropped_total{stream}` | counter |

Operation latency and quota metrics are owned here and nowhere else.
Forwarding and epoch refusals are RFC 15's; recalls are RFC 14's.

Each service call opens a trace span that parents the metadata store's and the
engine's spans. No share label on per-operation metrics ([RFC 16 §8.1](rfc-16-metadata-store.md#8.1%20Metrics)).

## 8. Conformance

- **Service suite:** drives `vfs.Service` directly with a fake `Callbacks` that
  records what adapters would have been told. It covers every operation's
  order (§5), and the cross-protocol cases: an NFS lock against an SMB deny
  mode, an NFS write against an SMB lease, a recall that times out, a client
  whose lease expires holding state of both kinds. It also asserts:
  - a conflicting operation returns `ErrDelay` while its recall is pending,
    with no worker blocked;
  - an `OpenID`, `GrantID` or `WatchID` presented by another client is refused;
  - a client removed from a share grant is refused on its next call with a
    handle it already holds;
  - a recursive watch reports nothing below a directory its holder cannot
    traverse;
  - during a unit's grace, a write overlapping a reclaimable lock returns
    `ErrGrace`;
  - a lock granted concurrently with a write is ordered wholly before or after
    it (V7), driven by a model test that interleaves the two;
  - a write retried after a dropped reply, with another write between, applies
    once (V11);
  - writers through `k` owners overshoot a hard limit by no more than §5.6's
    bound, counted per run;
  - an event sink that blocks does not slow an operation, and its drops are
    counted.
- **Import test:** no adapter package imports the metadata store, open state or
  the engine; the service imports no adapter (V1, V5).
- **Split run:** stated once in [RFC 15](rfc-15-topology.md), gated on the first remote view
  shipping.
- **Benchmarks:** each operation through the service, collocated and split, so a
  protocol benchmark minus a service benchmark is the adapter's own cost.

## 9. Open questions

1. **Compound batching.** Adapters run compounds as sequences of calls. Whether
   a batch call (a `LOOKUP`-`GETATTR`-`OPEN` chain in one service call) pays for
   itself under a split deployment is unmeasured.
2. **Where the pNFS layout operations live.** `LAYOUTGET` and `LAYOUTCOMMIT` are
   protocol-specific but cross owners ([RFC 15](rfc-15-topology.md)); whether they are service
   operations or an adapter-side protocol over `Data` is open. Either way a
   layout is bound to the owner's epoch ([RFC 14](rfc-14-open-state.md)).

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
