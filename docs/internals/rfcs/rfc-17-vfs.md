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

## Start here

*Explanatory. The rules are in §1 onward; where this section and a rule differ, the rule wins.*

**What this is.** DittoFS speaks NFS and SMB. Each protocol has an **adapter**
that decodes requests off the wire and encodes the replies. Beneath them sits
one filesystem service, the single API every adapter calls. It puts the steps of
each client operation in order — admission to the share, the permission check,
the check against other clients' opens and locks, quota, then the data — and
runs the operation on the server that owns the file.

**The problem, with one example.** alice-pc keeps
`profiles/alice/ODFC_alice.vhdx` open over SMB all day, with a deny mode that
forbids anyone else to write while she holds it. A script on build01 writes to
the same file over NFSv3, a protocol with no notion of opening a file.

If each adapter put the steps together itself, the SMB adapter would check deny
modes, because SMB has them, and the NFSv3 adapter, written for a protocol
without opens, would not. The NFSv3 write would go straight into the journal, in
the middle of a virtual disk Windows has mounted. The disk is corrupt, and no
log anywhere shows a conflict.

With the service:

1. The SMB adapter decodes alice's 64 KiB WRITE, finds the open its SMB file ID
   names, and asks the service to write those bytes at that offset.
2. The service admits the call to the `profiles` share, sends it to the primary
   of the file's shard, checks the open's permission and the locks others hold,
   reserves quota if the file grows, and hands the bytes to the engine, which
   stages them in the journal. alice-pc gets OK.
3. build01's write arrives through the NFS adapter as a write on an
   **anonymous open**, which carries the caller's identity and no state. The
   same path checks it against the deny modes others hold, finds alice's, and
   refuses it as a sharing violation. The NFS adapter turns that into an NFS
   error.

One rule, in one place, for both protocols.

**Reaching a share works the same way.** Resolving a share's name or path,
admitting the call, the NFSv4 **pseudo-filesystem** and the list of shares a
client may see all belong to the service. An adapter only turns NFSv3 MOUNT,
NFSv4 PUTROOTFH, SMB TREE_CONNECT, SECINFO and share enumeration into the
service's share calls and lookups, and keeps no share list of its own.
Admission is not a door passed once at mount: it runs on every call, so an
NFSv4 client presenting a saved handle meets the same checks as one walking the
pseudo-filesystem into the share. Mounts, tree connects and sessions are never
stored.

```text
  alice-pc (SMB)                            build01 (NFSv3)
  WRITE 64 KiB, own open                    WRITE, same file, no open
        │                                         │
        ▼                                         ▼
  SMB adapter: decode,                      NFS adapter: decode,
  SMB file ID → alice's open                anonymous open
        │                                         │
        ▼                                         ▼
 ┌──────────────────────── filesystem service ─────────────────────────┐
 │ 1 admit to share `profiles`: state, flavour, client rules, squash   │
 │ 2 route to the primary of the file's shard                          │
 │ 3 authorise through the metadata store's one chokepoint             │
 │ 4 check open state: locks, deny modes, caching grants, grace        │
 │ 5 reserve quota if the file grows                                   │
 │ 6 engine: stage the bytes in the journal, return the verifier       │
 └─────────────────────────────────────────────────────────────────────┘
        │                                         │
   alice-pc: OK                     build01: refused at step 4,
                                    alice's deny mode
```

**The words you need.**

- **adapter** — the code for one wire protocol: framing, compounds, replay
  caches, error codes, and nothing else ([§4.2](#4.2%20Translation%20stays%20in%20adapters)).
- **primary** — the storage node that owns the file's shard and holds its
  namespace, open state and content; every call on the file runs there
  ([§4.8](#4.8%20One%20primary%20per%20file), [glossary](rfc-0-data-lifecycle.md#Glossary)).
- **open** / **anonymous open** — a client's open file, held as state / what an
  NFSv3 read or write runs against instead: the caller's identity, no state
  ([§4.1](#4.1%20The%20operation%20set%20is%20the%20union%2C%20not%20the%20intersection)).
- **admission** — the per-share checks every call passes before the permission
  check: the share's state, the authentication flavours it accepts, its client
  rules, and squashing, which maps the caller to the user it acts as
  ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)).
- **the chokepoint** — the one function in the metadata store that decides file
  permissions; the service is its only caller ([§4.6](#4.6%20One%20chokepoint)).
- **caching grant** — leave for a client to cache a file (an NFS delegation, an
  SMB lease or oplock). A conflicting operation starts a recall and tells its
  client to retry shortly ([§3.2](#3.2%20Callbacks)).
- **pseudo-filesystem** — the read-only tree of directories through which an
  NFSv4 client reaches each share's root, built the same on every node from the
  share list ([§4.9](#4.9%20Shares%2C%20mounts%20and%20trees)).

**What this RFC promises.**

- Every call is admitted to its share, whatever path its handle arrived by, and
  the share grant is checked on every call: a client removed from a share is
  refused on its next call, cached handles or not.
- Conflicts across protocols — an NFS lock against an SMB deny mode, an NFS
  write against an SMB lease — are decided in one place, and the check and the
  I/O it admits run in one step at the primary, so nothing is granted between
  them.
- No worker waits on a client: a conflict that needs a recall answers "retry
  shortly", and a recall not acknowledged by its deadline revokes the grant.
- Adapters never see the cluster: a call sent to the wrong node is re-routed
  inside the service, and a write retried after a lost reply is applied once.
- Quota is enforced here and nowhere else, and writers spread over several
  primaries overshoot a hard limit by a stated bound at most.

**How the rest is organised.** §1 gives the purpose and non-goals; §2 only
explains the name. §3 is the interface — the operation groups, the callbacks to
adapters, the event hooks — and can be skimmed. §4 holds the rules, with §4.9 on
shares, mounts and the pseudo-filesystem. §5, the order of each operation, is
the core. §6–§8 are invariants, metrics and the conformance suite; §9 lists open
questions.

## In short

- Adapters speak wire. They call one protocol-neutral service, `vfs.Service`,
  and nothing else below them.
- The service is the only caller of the metadata store ([RFC 16](rfc-16-metadata-store.md)), open state
  ([RFC 14](rfc-14-open-state.md)) and the content engine ([RFC 8](rfc-8-engine.md)), and the only code that orders an
  operation across them.
- Its operation set is the union of what the protocols need, not their
  intersection. Translation, framing and replay stay in the adapters.
- It hides the topology: whether a primary is this process or another node
  ([RFC 15](rfc-15-topology.md)) never reaches an adapter.
- A file has one primary, which holds its namespace, open state and content. The
  conflict check and the I/O it gates run at that primary, in one step.
- It is the one enforcer of quotas, the one emitter of quota and access-audit
  events, and the one place operation latency is measured.
- Every call is admitted to its share — state, flavour, client rules, squash —
  whatever path its handle arrived by. Mounts, tree connects and sessions are
  never stored; the NFSv4 pseudo-filesystem is a pure function of the share list.
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
   │ remove, rename, lock, grants, watches, statfs,│
   │ mount, tree connect, pseudo-fs, share list    │
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
3. **The topology is invisible to adapters.** A call whose primary is another node
   is forwarded by the service; an adapter never learns there was a hop.
4. **It is testable without a protocol.** The service has its own conformance
   suite (§8), and the protocol suites then test translation only.

### 1.1 Non-goals

The service **MUST NOT**:

- encode or decode a wire format, run a compound or keep a replay cache —
  those are the adapters' ([§4.2](#4.2%20Translation%20stays%20in%20adapters));
- evaluate a file permission itself — it asks the one chokepoint ([§4.6](#4.6%20One%20chokepoint));
- hold a copy of metadata, open state or content that outlives the call that
  read it;
- persist anything of its own. Every durable fact is recorded by the component
  that owns it;
- import an adapter. Calls upward go through interfaces adapters implement
  ([§3.2](#3.2%20Callbacks));
- answer DFS referrals or NFSv4 referrals to another installation. A share is
  served only by the installation that holds it.

## 2. Name

`Filesystem` is taken three ways over: the metadata store's `FilesystemInfo`,
Go's `io/fs.FS`, and the product's own description. The word for this layer
across the field is **VFS** — the protocol-neutral file API between front-ends
and storage. The package is `vfs`, the interface `vfs.Service`, and an adapter
holds one value of it. Call sites read `svc.Open(…)`, never `vfs.VFS`.

## 3. Interface

Signatures are indicative; the obligations are normative. `Service` embeds the
groups, so an adapter holds one value and a test can fake one group.

`Identity` below is what the adapter authenticated — the protocol, the
authentication flavour and Kerberos level, the principal or numeric IDs the
credential carried, and the client's address — not yet mapped through any
share's policy. The service resolves it per share (§4.9). An `OpenRef` carries
the `Identity` of the call that presents it, so calls that name only an open
are admitted like every other.

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
	Shares
}

// Clients: who is talking to us, and how to call them back (RFC 14).
type Clients interface {
	Connect(ctx context.Context, c ClientInfo, cb Callbacks) (metadata.ClientID, error)
	Renew(ctx context.Context, c metadata.ClientID) error
		Disconnect(ctx context.Context, c metadata.ClientID) error
	ReclaimComplete(ctx context.Context, c metadata.ClientID) error // RECLAIM_COMPLETE; may end grace early (RFC 14)

}

// Names: the namespace, by handle. Every method also takes a handle of the
// NFSv4 pseudo-filesystem (§4.9) and answers for it; an adapter never tells
// the two kinds apart.
type Names interface {
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

// Shares: how a client reaches a share, and which shares it may see (§4.9).
// An adapter passes the name or path the client sent and never holds a ShareID.
type Shares interface {
	Mount(ctx context.Context, id Identity, path string) (Handle, []AuthFlavor, error)     // NFSv3 MNT
	PseudoRoot(ctx context.Context, id Identity) (Handle, error)                             // NFSv4 PUTROOTFH, PUTPUBFH
	TreeConnect(ctx context.Context, id Identity, name string) (Handle, ShareInfo, error)    // SMB TREE_CONNECT
	SecInfo(ctx context.Context, id Identity, dir Handle, name []byte) ([]AuthFlavor, error) // SECINFO, SECINFO_NO_NAME (empty name)
	ListShares(ctx context.Context, id Identity) iter.Seq2[ShareInfo, error]                 // NetShareEnum, showmount -e
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
grant starts the recall and returns `ErrDelay` at once. The adapter maps
`ErrDelay` to the protocol's retry signal — NFS4ERR_DELAY or NFS3ERR_JUKEBOX —
and the client retries. Where the protocol has none, as for SMB, whose client
does not retry a request answered with an interim (pending) response, the
adapter sends the interim response and re-drives the call itself, within the
call's deadline, answering the client once the call completes or the deadline
passes. Either way no service worker waits: the retry is the client's or the
adapter's, and holds no state at the primary. A client that holds grants on many files and never
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
- turning `MNT`, `PUTROOTFH`, `TREE_CONNECT`, `SECINFO` and share enumeration
  into the `Shares` calls of §4.9, and nothing more: no share resolution,
  admission or pseudo-filesystem logic lives in an adapter;
- security descriptors ↔ `metadata.ACL`, applying RFC 19's mapping;
- mapping the service's errors to protocol status codes;
- a protocol's own numeric file IDs, read from the file's stored number
  ([RFC 7 §6.5](rfc-7-namespace-metadata.md#6.5%20A%20protocol%27s%20numeric%20file%20id%20is%20a%20stored%20number%2C%20never%20reused)).

### 4.3 Errors are neutral values

The service returns `ErrNotFound`, `ErrExist`, `ErrAccess`, `ErrStale`,
`ErrNotEmpty`, `ErrNotDir`, `ErrIsDir`, `ErrNoSpace`, `ErrQuota`, `ErrLocked`,
`ErrShareViolation`, `ErrGrace`, `ErrDelay`, `ErrNotYours`, `ErrBadLayout`, `ErrWrongSecurity`, and the content errors of
[RFC 8](rfc-8-engine.md) (`ErrLost`, `ErrUnavailable`, `ErrCorrupt`). Each adapter maps them
once. `ErrDelay` means "retry shortly": a recall is in progress (§3.2).
`ErrGrace` means the file's shard is in grace and the request needs, or
conflicts with, state that may still be reclaimed (§5.1). `ErrWrongSecurity`
means the share's policy does not admit the call's flavour (§4.9); NFSv4 maps it
to `NFS4ERR_WRONGSEC`, NFSv3 and SMB to an access error. A routing refusal — wrong primary, stale epoch — **MUST NOT** reach an
adapter: the service re-routes and retries within the caller's deadline ([RFC 15 §6](rfc-15-topology.md#6.%20Learning%20primaries)).

An operation that arrives with no deadline is given one of 30 s from its
arrival at the service, fixed rather than a setting ([RFC 13](rfc-13-configuration.md)), so every wait below
it ends ([RFC 0 §10.3](rfc-0-data-lifecycle.md#10.3%20Every%20wait%20on%20a%20request%20ends%20at%20a%20deadline)).

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
- `Mount`, `TreeConnect` and a `Lookup` into a share take the caller's identity and are refused when no grant admits it.
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
unique across the cluster, the shard and epoch the sender expects, and a hop
count. The primary refuses a call whose epoch is not its own, and keeps a short
dedup table keyed by request ID alone — handed over with the shard or its
files ([RFC 11 §4](rfc-11-ownership.md#4.%20Moving%20files%20and%20primaries)) — that returns a mutation's original result to a retry.
So a write whose reply was lost, retried after another write landed on the same
extent, returns the first write's result instead of overwriting the second.
Repeating a mutation with the same arguments is **not** assumed to be safe.

### 4.8 One primary per file

Every call on a file runs at the primary of the file's shard
([RFC 11](rfc-11-ownership.md)), which holds the file's namespace records, its open state and
its content. A node with the `protocol` role only forwards
([RFC 15](rfc-15-topology.md)).

The conflict check and the I/O it gates therefore run at one primary, with no
hop between them. The primary **MUST** order them so that no lock, deny mode or
grant is granted between the check and the I/O it admits: a conflicting grant
is ordered wholly before the check, which then refuses, or wholly after the
I/O. The service does no cross-primary orchestration for a single-file
operation. The operations that touch two primaries are those
[RFC 15 §4.2](rfc-15-topology.md#4.2%20Calls%20that%20touch%20two%20primaries) and [RFC 11 §8.1](rfc-11-ownership.md#8.1%20Operations%20across%20shards) name — a rename, link or unlink whose
files are in several shards, a create whose new file lands in a shard other
than its directory's, and a `Copy` between files of two shards — which it orders
through one coordinator, and `ReadDir` with attributes, which reads each entry's
attributes at its own primary ([§5.7](#5.7%20GetAttr)) and needs no order between them.

### 4.9 Shares, mounts and trees

**Admission runs on every call.** Before it authorises, the service admits the
call to the share of the handle or open it names, in this order:

1. the share's state ([RFC 16 §2.3.1](rfc-16-metadata-store.md#2.3.1%20Share%20names%2C%20paths%20and%20state)) — `ErrDelay` while quiesced,
   `ErrStale` once being removed;
2. the share's `ExportPolicy` flavour rule — `ErrWrongSecurity`;
3. its client rules, against the call's address and the netgroups it is in —
   `ErrAccess`, or a narrowing to read-only;
4. squashing, which turns the `Identity` into the principal the call acts as on
   this share.

Then `Authorize` evaluates the grant and the file (§4.6). **There is no call
that skips admission**: lock, unlock, test-lock, layout, watch, grant return and
the calls that name only an open are admitted like a read. Admission is a
property of the share a call reaches, not of how it reached it, so an NFSv4
compound that `PUTFH`s a handle and one that walks the pseudo-filesystem into the
share meet the same four checks. A compound crossing from one share into another
is re-admitted, and re-squashed, at the crossing. The admission result may be
cached per (share, identity) only with `ExportPolicy.Version`, the netgroups'
versions and `ShareList.Version` in the key.

**The service owns every step of reaching a share; the adapter only
translates.** What the three protocols share — resolving a name or path through
RFC 16's indexes, admission, the pseudo-filesystem, what `SECINFO` answers and
which shares a client may list — is written once, here, behind the `Shares`
group and the pseudo handles `Names` accepts. NFSv3 `MNT` is `Mount`; NFSv4
`PUTROOTFH` is `PseudoRoot` followed by `Lookup`s; SMB `TREE_CONNECT` is
`TreeConnect`. Each resolves, admits and authorises, and returns the share's
root handle. They are where a client fails *early*; they are not where it is
gated, since every later call is admitted again. An adapter holds no share list,
and a change to the share list reaches every protocol at once.

> decision: share resolution and the pseudo-filesystem live in the service, not
> the adapters, because the rules for reaching a share drifted when each
> adapter had its own copy: a check present on one entry path was missing on
> the other. Move a piece back to an adapter only if a protocol needs a rule the
> others must not share, and then state it in that protocol's RFC.

**Neither is stored.** No record of a mount, a tree connect or an SMB session
exists in the metadata store:

- the NFSv3 MOUNT table is not kept. `DUMP` returns an empty list and `UMNT`
  and `UMNTALL` succeed and change nothing, which every client tolerates; a
  table kept per node would be wrong after any failover anyway;
- SMB sessions, tree connects and their IDs live in the protocol node's memory.
  A client that reconnects — after a restart, or an address takeover
  ([RFC 15 §5.3](rfc-15-topology.md#5.3%20Client%20addressing)) — sets up its session and tree connects again, then
  reclaims durable or persistent opens ([RFC 14 §8](rfc-14-open-state.md#8.%20What%20is%20durable)). A reclaim is refused
  unless the reclaiming tree connect's `ShareID` is the open's;
- `ClientID` is the only client state that is stored, and it is RFC 14's.

**The NFSv4 pseudo-filesystem is a function of the share list.** The service
on every `protocol` node builds it from the share path index
([RFC 16](rfc-16-metadata-store.md)), and must build the same one, because a client moved between
nodes by an address takeover or `fs_locations` presents the handles it already
holds:

- a pseudo directory's handle is minted by the handle codec as its own kind
  ([RFC 7 §6.1](rfc-7-namespace-metadata.md#6.1%20A%20handle%20names%20a%20file%2C%20never%20a%20path)), from a digest of the installation's identity and the
  directory's path, and nothing else; so is its numeric file id. Every node and every restart mints
  the same. A pseudo handle whose path no longer leads to a share is `ErrStale`;
- its `fsid` is one reserved value per installation, never a share's; its
  change attribute is `ShareList.Version`, so adding, removing, renaming or
  re-pathing a share is seen by every client's cache;
- a share's root reports the share's own `fsid`, and `mounted_on_fileid` the
  numeric id of the pseudo directory it sits in, so a client sees a mount
  crossing there;
- a pseudo directory lists only the components that lead to a share the caller
  would be admitted to. `SECINFO` on a pseudo directory answers every flavour some share
  admits; on a share's path, the share's `Flavors`.

**Share enumeration** — SMB `NetShareEnum` over `IPC$`, NFS `showmount -e` —
is `ListShares`: the shares whose policy is not hidden and to which the caller
would be admitted. `IPC$` is the SMB adapter's RPC endpoint, not a share, and
reaches no file.

> decision: the MOUNT table is not kept, so `showmount -a` lists nothing. It is
> advisory in the protocol and no client depends on it; keep one only if an
> operator workflow is shown to need it, and then per node and labelled as such.

## 5. Orchestration

The service is the only code that orders a client operation across components.
Each order below is normative.

### 5.1 Write

1. resolve the handle; refuse a stale one; admit the call to its share (§4.9);
   route to the primary (§4.7, §4.8);
2. authorise against the file ([§4.6](#4.6%20One%20chokepoint)), or evaluate the open's stored grant;
   for an anonymous open, check deny modes held by others;
3. if the shard is in grace, refuse with `ErrGrace` a write that overlaps a lock,
   deny mode or grant that may still be reclaimed ([RFC 14 §4.4](rfc-14-open-state.md#4.4%20Grace%20is%20per%20shard)); otherwise
   check open state: the caller's open and byte-range locks held by others
   ([RFC 14 §7](rfc-14-open-state.md#7.%20Conflicts%20across%20protocols)). A conflicting caching grant starts a recall and the write
   returns `ErrDelay` (§3.2);
4. reserve the charge the write adds, if it extends the file (§5.6);
5. call the engine's `Write`, which stages the bytes in the journal and returns
   the write verifier ([RFC 8](rfc-8-engine.md)), passing the times the caller's open has suspended
   ([RFC 14 §2.2](rfc-14-open-state.md#2.2%20Open)), which neither the overlay nor the existence commit then
   advances for this write; `Version` advances regardless (§5.7);
6. reply. Existence, usage and `mtime` are recorded at the next stability point
   by the engine, not here; that commit releases the reservation.

Steps 3–5 run at the primary as one step (§4.8).

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
records the open — all at the file's primary, in one step as the protocol
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
applied by the engine's `Truncate` at the file's primary; the other attributes in
the same call are applied by the metadata store in the same service call.

### 5.6 Quota

The service is the one enforcer of quotas. Neither the metadata store nor the
engine refuses a write for quota.

- At step 4 of a write, the primary adds the charge the write would add to an
  in-memory **reservation** per principal and per project, and refuses with
  `ErrQuota` when committed usage plus unfolded deltas
  ([RFC 16 §4.4](rfc-16-metadata-store.md#4.4%20Counters%20that%20many%20writers%20change)) plus its reservations would exceed the hard limit.
  The existence commit that records the charge releases the reservation; so
  does a write that fails. A reservation is volatile: after a crash the journal
  re-applies the writes and their charge ([RFC 8](rfc-8-engine.md)).
- **Overshoot bound.** A primary sees its own reservations, not other primaries'.
  Each primary lets a principal or project hold at most `S` bytes reserved and
  uncommitted (the reservation slack, a setting); a write past it waits for a
  commit or returns `ErrDelay`. A principal writing through `k` primaries at once
  can therefore exceed its hard limit by at most `(k − 1) × S`, plus what
  transactions committing concurrently add. With one primary, only the latter.
- **Soft limits.** A quota's soft limit and grace time
  ([RFC 16](rfc-16-metadata-store.md)) are enforced here too. Crossing the
  advisory threshold or the soft limit emits a quota event (§3.3) and refuses
  nothing; the metadata store records when the soft limit was first exceeded,
  and once the grace time has run from then the soft limit is enforced as the
  hard one. Falling back under it clears the record.

### 5.7 GetAttr

The metadata store is a leaf: it never calls the engine. `GetAttr` joins
`Files.Get` with the engine's overlay — `Size`, `Times` and `Version` of writes
staged but not yet committed — at the file's primary, where both are local. The
overlay wins where it is newer.

**The overlay's `Version` advances on every write the primary accepts**, at
acceptance, not at the existence commit that later records it: a `GetAttr`
after a write is acknowledged returns a `Version` greater than any returned
before it. The existence commit **MUST** leave the committed `Version` no lower
than any the overlay reported for the writes it records, so the change attribute
never moves backward when the overlay drains. So an NFS client's close-to-open
check, and SMB change detection, see another client's staged write before it is
committed.

This is the only read path for a file's
attributes; `ReadDir` with attributes does the same join per entry, batched
per primary for entries whose files live in other shards.

### 5.8 The write verifier

The NFS write verifier is returned by the engine's `Write` and `Commit` and
derived from the primary's node, its node epoch, the process instance and the
loss generation of the journal holding the file — not from the shard epoch
([RFC 11 §7](rfc-11-ownership.md#7.%20Protocol%20state), [RFC 8 §4.1](rfc-8-engine.md#4.1%20A%20write%20is%20staged%20and%20acknowledged%2C%20and%20nothing%20more)). The service passes it through unchanged. It
therefore changes whenever the node serving the file's shard as primary changes
or restarts, and whenever the running primary's journal loses writes it had
acknowledged as unstable ([RFC 1 §3.8](rfc-1-journal.md#3.8%20Loss%20events)), and not when only the shard epoch
is raised. A change is what makes a client resend writes it sent unstable
([RFC 14 §10](rfc-14-open-state.md#10.%20Shard%20placement)); on the normal path none of the inputs moves, and the
verifier is constant.

## 6. Invariants

| # | Invariant |
| --- | --- |
| V1 | Adapters hold only `vfs.Service`. No adapter holds a metadata view, an engine, open state or a component. |
| V2 | Every client operation that needs authorisation reaches `Files.Authorize` exactly through the service. |
| V3 | Every cross-component order of a client operation is the one in §5, in every protocol. |
| V4 | A routing refusal never reaches an adapter. |
| V5 | The service persists nothing and imports no adapter. |
| V6 | A recall or break that is not acknowledged by its deadline revokes the grant; no worker waits on a recall — the conflicting operation returns `ErrDelay`. |
| V7 | The conflict check and the I/O it admits run at one primary, and no conflicting state is granted between them. |
| V8 | Every open-state call names its `ClientID`, and an ID the client does not hold is refused. |
| V9 | The share grant is evaluated on every call. |
| V10 | Quota is enforced only by the service, and overshoot stays within the bound of §5.6. |
| V11 | A forwarded mutation retried after a lost reply returns its original result and applies once. |
| V12 | Every call that names a handle or an open is admitted to its share — state, flavour, client rules, squash — whatever path the handle arrived by. |
| V13 | No mount, tree connect or session is stored; a reconnecting client is admitted afresh. |
| V14 | Every node builds the same pseudo-filesystem from the same share list: the same handles, numeric ids, `fsid` and change attribute. |
| V15 | No adapter resolves a share name or path, admits a call, or builds or interprets any part of the pseudo-filesystem; all of it is behind `Shares` and `Names`. |
| V16 | The `Version` `GetAttr` returns advances on every write the primary accepts, and never moves backward when the write is committed. |

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
  - during a shard's grace, a write overlapping a reclaimable lock returns
    `ErrGrace`;
  - a lock granted concurrently with a write is ordered wholly before or after
    it (V7), driven by a model test that interleaves the two;
  - client A reads `GetAttr`'s `Version`, client B writes and is acknowledged,
    and A's next `GetAttr` returns a greater `Version` before any existence
    commit; after the commit it is not lower (V16);
  - an SMB call that meets a pending recall gets an interim response and
    completes once the recall ends, without the client resending it, and a
    recall that outlasts the call's deadline completes it with the deadline's
    error (§3.2);
  - a write retried after a dropped reply, with another write between, applies
    once (V11);
  - the verifier `Write` and `Commit` return through the service changes after
    the primary's journal drops an unstable write as lost, in the same process
    and epoch, and is unchanged across a shard-epoch raise that moves nothing
    (§5.8); a service that caches the verifier per process passes the second
    half and fails the first;
  - writers through `k` primaries overshoot a hard limit by no more than §5.6's
    bound, counted per run;
  - an event sink that blocks does not slow an operation, and its drops are
    counted;
  - for each of lock, unlock, test-lock, layout-get and watch, reached both by
    a handle presented directly and by a walk from the pseudo-filesystem: a
    quiesced share returns `ErrDelay`, a share whose policy refuses the flavour
    returns `ErrWrongSecurity`, and a client outside its client rules returns
    `ErrAccess` (V12);
  - a compound crossing from a share squashing root into one that does not acts
    as root only in the second;
  - a renamed share keeps serving held handles and established tree connects,
    and a new tree connect needs the new name;
  - two services built from the same share list return byte-identical pseudo
    handles, numeric ids and change attributes, and a share added on one is
    seen on the other with a raised change attribute (V14).
- **Import test:** no adapter package imports the metadata store, open state or
  the engine; the service imports no adapter (V1, V5). The `Shares` calls and
  pseudo handles are driven in the service suite with no adapter at all, and
  every case above runs once per entry call (`Mount`, `PseudoRoot` + `Lookup`,
  `TreeConnect`) (V15).
- **Split run:** stated once in [RFC 15](rfc-15-topology.md), gated on the first remote view
  shipping.
- **Benchmarks:** each operation through the service, collocated and split, so a
  protocol benchmark minus a service benchmark is the adapter's own cost.

## 9. Open questions

1. **Compound batching.** Adapters run compounds as sequences of calls. Whether
   a batch call (a `LOOKUP`-`GETATTR`-`OPEN` chain in one service call) pays for
   itself under a split deployment is unmeasured.
2. **Where the pNFS layout operations live.** `LAYOUTGET` and `LAYOUTCOMMIT` are
   protocol-specific but cross primaries ([RFC 15](rfc-15-topology.md)); whether they are service
   operations or an adapter-side protocol over `Data` is open. Either way a
   layout is bound to the (shard, epoch) it was granted under ([RFC 14](rfc-14-open-state.md)).

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
